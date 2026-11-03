package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// runRequest is one review, sent by the server container to the runner in
// the reviewer container over a Unix socket on a volume only the two share.
// It carries the only credentials the review gets.
type runRequest struct {
	Repo   string `json:"repo"`
	SHA    string `json:"sha"`
	Agent  string `json:"agent"`
	Policy string `json:"policy"`
	// DefaultPolicy reviews under Radar's built-in policy when the
	// repository has none at Policy (radar-gh -default-policy).
	DefaultPolicy bool              `json:"default_policy,omitempty"`
	Env           map[string]string `json:"env"`
	// GitHubToken reads this one repository's contents and expires within
	// the hour.
	GitHubToken string `json:"github_token"`
	// CredentialsURI and CredentialsToken reach the server container's
	// broker, which serves Bedrock-only credentials for this review.
	CredentialsURI   string `json:"credentials_uri"`
	CredentialsToken string `json:"credentials_token"`
}

type runResponse struct {
	// Output is radar-gh review-commit's JSON.
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
	// Rejected means the commit cannot be reviewed (radar-gh exit 3: no
	// changes against its base, or too many files), which no retry changes,
	// or the runner refused the request itself; as opposed to a failure.
	Rejected bool `json:"rejected,omitempty"`
}

const (
	maxResultBytes = 16 << 20
	maxStderrBytes = 4 << 10
	// runnerReviewCap is one review at a time: see limitsConfig.ConcurrentReviews.
	runnerReviewCap = 1
	// radarGHUnreviewable is radar-gh review-commit's exit status for a
	// commit no review can cover.
	radarGHUnreviewable = 3
)

// runner executes reviews in the reviewer container.
type runner struct {
	radarGH  string
	packsDir string
	workDir  string
	path     string
	logger   *slog.Logger
	slots    chan struct{}
}

func (r *runner) validate(req runRequest) error {
	switch {
	case !repoPattern.MatchString(req.Repo):
		return errors.New("repo must be owner/repository")
	case !fullSHA.MatchString(req.SHA):
		return errors.New("sha must be a full commit SHA")
	case req.Agent == "" || !reviewEnvValue.MatchString(req.Agent):
		return errors.New("agent is invalid")
	case !filepath.IsLocal(req.Policy):
		return errors.New("policy must be a relative path inside the repository")
	case req.GitHubToken == "" || req.CredentialsToken == "":
		return errors.New("a review needs its GitHub token and credentials token")
	}
	if err := validateReviewEnv(req.Env); err != nil {
		return err
	}
	u, err := url.Parse(req.CredentialsURI)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		// The broker is in the same pod; credentials never leave loopback.
		return errors.New("credentials_uri must be http://127.0.0.1")
	}
	return nil
}

// reviewEnv is radar-gh's whole environment: the request's credentials and
// Radar settings, and nothing inherited from the runner. Codex gets less
// still: radar-gh passes it only PATH, HOME, its own config and a placeholder
// AWS key (bedrock/codex_exec.go).
func reviewEnv(req runRequest, dir, path string) []string {
	env := []string{
		"PATH=" + path,
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"LANG=C.UTF-8",
		"GH_CONFIG_DIR=" + filepath.Join(dir, ".gh"),
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GH_TOKEN=" + req.GitHubToken,
		"AWS_CONTAINER_CREDENTIALS_FULL_URI=" + req.CredentialsURI,
		"AWS_CONTAINER_AUTHORIZATION_TOKEN=" + req.CredentialsToken,
		"AWS_EC2_METADATA_DISABLED=true",
	}
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func (r *runner) args(req runRequest) []string {
	args := []string{"review-commit", "-repo", req.Repo, "-sha", req.SHA, "-policy", req.Policy, "-agent", req.Agent, "-json"}
	if req.DefaultPolicy {
		args = append(args, "-default-policy")
	}
	if r.packsDir != "" {
		args = append(args, "-review-packs", r.packsDir)
	}
	return args
}

func (r *runner) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/readyz" && req.Method == http.MethodGet:
		if problems := newIsolationCheck().problems(req.Context()); len(problems) > 0 {
			http.Error(w, strings.Join(problems, "; "), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	case req.URL.Path == "/v1/review" && req.Method == http.MethodPost:
		var in runRequest
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&in); err != nil {
			writeRunResponse(w, runResponse{Error: "bad request: " + err.Error(), Rejected: true})
			return
		}
		writeRunResponse(w, r.review(req.Context(), in))
	default:
		http.NotFound(w, req)
	}
}

func writeRunResponse(w http.ResponseWriter, resp runResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// review runs one review. Cancelling ctx (the server giving up, or shutting
// down) kills radar-gh and everything it started.
func (r *runner) review(ctx context.Context, req runRequest) runResponse {
	if err := r.validate(req); err != nil {
		return runResponse{Error: err.Error(), Rejected: true}
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return runResponse{Error: "the reviewer is at capacity"}
	}
	dir, err := os.MkdirTemp(r.workDir, "review-")
	if err != nil {
		return runResponse{Error: err.Error()}
	}
	defer os.RemoveAll(dir)

	started := time.Now()
	cmd := exec.CommandContext(ctx, r.radarGH, r.args(req)...)
	cmd.Dir = dir
	cmd.Env = reviewEnv(req, dir, r.path)
	var stdout limitedBuffer
	stdout.limit = maxResultBytes
	var stderr tailBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	killProcessGroup(cmd)
	err = cmd.Run()
	r.logger.Info("radar-gh review-commit", "repo", req.Repo, "sha", req.SHA, "duration", time.Since(started), "error", errString(err))
	if err != nil {
		// Exit 2 (flags that cannot work) is not the caller's doing: the
		// server built the flags, so it is a failure an operator fixes.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == radarGHUnreviewable {
			return runResponse{Error: stderr.String(), Rejected: true}
		}
		if ctx.Err() != nil {
			return runResponse{Error: "review cancelled: " + ctx.Err().Error()}
		}
		return runResponse{Error: fmt.Sprintf("radar-gh: %v: %s", err, stderr.String())}
	}
	if stdout.overflow {
		return runResponse{Error: fmt.Sprintf("radar-gh output exceeds %d bytes", maxResultBytes)}
	}
	out := bytes.TrimSpace(stdout.buf.Bytes())
	if !json.Valid(out) {
		return runResponse{Error: "radar-gh output is not JSON"}
	}
	return runResponse{Output: out}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.overflow = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// tailBuffer keeps the last few kilobytes of radar-gh's stderr for errors.
type tailBuffer struct{ buf bytes.Buffer }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - maxStderrBytes; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(t.buf.String()) }

// runRunner is the reviewer container's process.
func runRunner(logger *slog.Logger) int {
	if problems := newIsolationCheck().problems(context.Background()); len(problems) > 0 {
		for _, p := range problems {
			logger.Error("reviewer container is not isolated; refusing to run reviews", "problem", p)
		}
		return 1
	}
	// The work volume is the reviewer's only writable place: the chart runs it
	// with a read-only root and no /tmp, so nothing may default to TMPDIR.
	workDir := envOr("RADAR_MCP_WORK_DIR", os.TempDir())
	if err := checkCodexSandbox(context.Background(), workDir); err != nil {
		logger.Error("the Codex sandbox does not work in this container; Codex would review without reading the code", "error", err)
		return 1
	}
	if err := hardenProcess(); err != nil {
		logger.Error("protecting the runner's memory from the reviewers", "error", err)
		return 1
	}
	socket := envOr("RADAR_MCP_RUNNER_SOCKET", defaultRunnerSocket)
	radarGH, err := exec.LookPath(envOr("RADAR_MCP_RADAR_GH", "radar-gh"))
	if err != nil {
		logger.Error("finding radar-gh", "error", err)
		return 1
	}
	r := &runner{
		radarGH:  radarGH,
		packsDir: envOr("RADAR_MCP_REVIEW_PACKS", defaultPacksDir),
		workDir:  workDir,
		path:     os.Getenv("PATH"),
		logger:   logger,
		slots:    make(chan struct{}, runnerReviewCap),
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		logger.Error("listening on the runner socket", "socket", socket, "error", err)
		return 1
	}
	// The server container runs as a different user, so it needs the socket
	// open to others. Nothing is lost: only the two containers mount its
	// volume, Codex already runs as this user, and the runner holds no
	// credential to hand out; each request brings its own.
	if err := os.Chmod(socket, 0o666); err != nil {
		logger.Error("restricting the runner socket", "error", err)
		return 1
	}
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()
	logger.Info("radar-mcp runner ready", "socket", socket, "version", version)
	select {
	case <-ctx.Done():
	case err := <-errs:
		logger.Error("runner", "error", err)
		return 1
	}
	// In-flight reviews finish or are cancelled by the server's own drain;
	// the grace period here only has to outlast that.
	shutdown, cancel := context.WithTimeout(context.Background(), drainTimeout()+time.Minute)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		logger.Error("runner shutdown", "error", err)
		return 1
	}
	return 0
}

// checkCodexSandbox runs a command in Codex's Linux sandbox, as Codex runs
// every command it uses to read the code. The sandbox (bubblewrap) needs a
// user namespace, which the container runtime's default seccomp profile
// forbids; there every command fails and Codex reviews the diff alone, unlike
// CI. The chart gives the reviewer container, which holds no credential, an
// unconfined seccomp profile for this (docs/mcp.md). It works in workDir, as
// reviews do. Skipped when the image has no Codex.
func checkCodexSandbox(ctx context.Context, workDir string) error {
	codex, err := exec.LookPath(envOr("RADAR_CODEX_BIN", "codex"))
	if err != nil {
		return nil
	}
	dir, err := os.MkdirTemp(workDir, "codex-sandbox-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codex, "sandbox", "--", "true")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + dir, "TMPDIR=" + dir}
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, out.String())
	}
	return nil
}

// runnerClient is the server container's side of the socket.
type runnerClient struct {
	client *http.Client
}

func newRunnerClient(socket string) *runnerClient {
	return &runnerClient{client: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

// errRejected is a review radar-gh refused, such as a commit with no changes
// against its base or too many files; the caller's error, not the server's.
type errRejected struct{ msg string }

func (e errRejected) Error() string { return e.msg }

func (c *runnerClient) run(ctx context.Context, req runRequest) (json.RawMessage, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://runner/v1/review", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("reaching the reviewer container: %w", err)
	}
	defer resp.Body.Close()
	var out runResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResultBytes+1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("reading the reviewer's answer: %w", err)
	}
	switch {
	case out.Rejected:
		return nil, errRejected{out.Error}
	case out.Error != "":
		return nil, errors.New(out.Error)
	}
	return out.Output, nil
}

func (c *runnerClient) ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runner/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("reviewer container not ready: %s", strings.TrimSpace(string(b)))
	}
	return nil
}
