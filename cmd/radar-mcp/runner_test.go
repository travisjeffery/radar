package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func validRunRequest() runRequest {
	return runRequest{
		Repo: "Acme/backend", SHA: testSHA,
		Agent:       "bedrock=us.anthropic.claude-opus-5-5,codex=openai.gpt-6.1-sol",
		Policy:      ".github/radar-policy.json",
		Env:         map[string]string{"RADAR_CODEX_EFFORT": "high", "AWS_REGION": "us-east-2"},
		GitHubToken: "ghs_review", CredentialsURI: "http://127.0.0.1:8081/credentials", CredentialsToken: "per-review",
	}
}

// radar-gh's environment is built from the request alone. Secrets in the
// runner's own environment, as a misconfigured container would carry, do not
// reach it, and the only credentials it gets are the review's own.
func TestReviewEnvIsOnlyTheRequest(t *testing.T) {
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/radar-mcp-irsa-role")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "/var/run/secrets/eks.amazonaws.com/serviceaccount/token")
	t.Setenv("RADAR_MCP_GITHUB_APP_KEY_FILE", "/etc/radar-mcp/github-app/private-key.pem")
	t.Setenv("RADAR_MCP_RESULTS_BUCKET", "radar-mcp-results")

	env := reviewEnv(validRunRequest(), "/work/review-1", "/usr/bin")
	var names []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	sort.Strings(names)
	want := []string{
		"AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_EC2_METADATA_DISABLED", "AWS_REGION",
		"GH_CONFIG_DIR", "GH_NO_UPDATE_NOTIFIER", "GH_PROMPT_DISABLED", "GH_TOKEN", "GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT",
		"HOME", "LANG", "PATH", "RADAR_CODEX_EFFORT", "TMPDIR",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("env names = %v\nwant %v", names, want)
	}
	joined := strings.Join(env, "\n")
	for _, secret := range []string{"irsa-role", "serviceaccount/token", "private-key.pem", "radar-mcp-results"} {
		if strings.Contains(joined, secret) {
			t.Errorf("radar-gh's environment carries %q", secret)
		}
	}
}

func TestRunnerRejectsWhatItShouldNotRun(t *testing.T) {
	r := &runner{}
	for name, mutate := range map[string]func(*runRequest){
		"short sha":             func(q *runRequest) { q.SHA = "0123456" },
		"repo flag":             func(q *runRequest) { q.Repo = "-x/y" },
		"policy outside":        func(q *runRequest) { q.Policy = "../../etc/passwd" },
		"credentials off pod":   func(q *runRequest) { q.CredentialsURI = "http://169.254.170.2/creds" },
		"credentials over tls?": func(q *runRequest) { q.CredentialsURI = "https://127.0.0.1/creds" },
		"credential env":        func(q *runRequest) { q.Env = map[string]string{"AWS_ACCESS_KEY_ID": "AKIA"} },
		"codex binary env":      func(q *runRequest) { q.Env = map[string]string{"RADAR_CODEX_BIN": "/tmp/x"} },
		"no github token":       func(q *runRequest) { q.GitHubToken = "" },
		"agent shell":           func(q *runRequest) { q.Agent = "codex=$(id)" },
	} {
		t.Run(name, func(t *testing.T) {
			req := validRunRequest()
			mutate(&req)
			if err := r.validate(req); err == nil {
				t.Fatal("validate accepted it")
			}
		})
	}
	if err := r.validate(validRunRequest()); err != nil {
		t.Fatalf("valid request: %v", err)
	}
}

// fakeRadarGH stands in for radar-gh: it prints its environment and the
// result CI's review-commit -json would, or fails as asked.
const fakeRadarGH = `#!/bin/sh
case "$*" in
  *fail-unreviewable*) echo "radar-gh review-commit: abc has no changes to review against main" >&2; exit 3;;
  *fail-usage*) echo "radar-gh review-commit: -review-packs needs a policy with review_criteria" >&2; exit 2;;
  *fail-github*) echo "radar-gh review-commit: reading the default branch: GitHub 502" >&2; exit 1;;
esac
env > "$HOME/../env-seen"
printf '{"repository":"Acme/backend","head_sha":"%s","markdown":"## Radar","review":{}}' "$5"
`

func startRunner(t *testing.T) (*runnerClient, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "radar-gh")
	if err := os.WriteFile(bin, []byte(fakeRadarGH), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &runner{radarGH: bin, packsDir: "/opt/radar/review-packs", workDir: work, path: os.Getenv("PATH"),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), slots: make(chan struct{}, 1)}
	socket := filepath.Join(dir, "runner.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: r}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return newRunnerClient(socket), work
}

func TestRunnerRunsRadarGHWithTheReviewsCredentialsOnly(t *testing.T) {
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/radar-mcp-irsa-role")
	t.Setenv("RADAR_MCP_GITHUB_APP_KEY_FILE", "/etc/radar-mcp/github-app/private-key.pem")
	client, work := startRunner(t)

	out, err := client.run(context.Background(), validRunRequest())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.HeadSHA != testSHA {
		t.Fatalf("output %s, err %v", out, err)
	}
	seen, err := os.ReadFile(filepath.Join(work, "env-seen"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(seen)
	for _, want := range []string{"GH_TOKEN=ghs_review", "AWS_CONTAINER_AUTHORIZATION_TOKEN=per-review", "RADAR_CODEX_EFFORT=high"} {
		if !strings.Contains(env, want) {
			t.Errorf("radar-gh's environment lacks %s", want)
		}
	}
	for _, leaked := range []string{"AWS_ROLE_ARN", "RADAR_MCP_GITHUB_APP_KEY_FILE"} {
		if strings.Contains(env, leaked) {
			t.Errorf("radar-gh's environment carries %s", leaked)
		}
	}
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "review-") {
			t.Errorf("review directory %s was not removed", e.Name())
		}
	}
}

// Only a commit radar-gh says no review can cover (exit 3) is the caller's
// answer; bad flags (2) and other failures (1) are failures to fix or retry.
func TestRunnerClassifiesRadarGHOutcomes(t *testing.T) {
	client, _ := startRunner(t)
	for policy, wantRejected := range map[string]bool{
		"fail-unreviewable.json": true,
		"fail-usage.json":        false,
		"fail-github.json":       false,
	} {
		req := validRunRequest()
		req.Policy = policy
		_, err := client.run(context.Background(), req)
		if err == nil {
			t.Fatalf("%s: no error", policy)
		}
		if got := errors.As(err, new(errRejected)); got != wantRejected {
			t.Errorf("%s: rejected = %v, want %v (err %v)", policy, got, wantRejected, err)
		}
		if !strings.Contains(err.Error(), "radar-gh review-commit:") {
			t.Errorf("%s: err %v lacks radar-gh's message", policy, err)
		}
	}
}

// The sandbox check works in the work volume, not TMPDIR: the reviewer runs
// with a read-only root and no writable /tmp.
func TestCodexSandboxCheckUsesTheWorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	bin := filepath.Join(t.TempDir(), "codex")
	// The fake Codex succeeds only when run as "sandbox -- true" from a
	// directory it can write, as the real sandbox needs.
	script := "#!/bin/sh\n[ \"$1 $2 $3\" = \"sandbox -- true\" ] && touch ./probe\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RADAR_CODEX_BIN", bin)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "read-only-root-has-no-tmp"))
	work := t.TempDir()
	if err := checkCodexSandbox(context.Background(), work); err != nil {
		t.Fatalf("checkCodexSandbox with an unusable TMPDIR: %v", err)
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Fatalf("left %d entries in the work dir", len(entries))
	}
}

// Only a review that asks for the default policy passes -default-policy.
func TestRunnerArgsDefaultPolicy(t *testing.T) {
	r := &runner{packsDir: "/opt/radar/review-packs"}
	req := validRunRequest()
	if args := strings.Join(r.args(req), " "); strings.Contains(args, "-default-policy") {
		t.Fatalf("args %q carry -default-policy", args)
	}
	req.DefaultPolicy = true
	if args := strings.Join(r.args(req), " "); !strings.Contains(args, " -default-policy") {
		t.Fatalf("args %q lack -default-policy", args)
	}
}
