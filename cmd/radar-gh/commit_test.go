package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

// commitRepo builds a repository whose main branch holds policy v1 and whose
// head commit, off main, rewrites the policy to v99 and edits a document. It
// returns the directory, main's tip and the head commit.
func commitRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	policy := func(version int) string {
		data, err := os.ReadFile("../../examples/github-policy.json")
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		p["version"] = version
		p["mode"] = "approve"
		out, _ := json.Marshal(p)
		return string(out)
	}
	git("init", "-q", "-b", "main")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	write(".github/radar-policy.json", policy(1))
	write("docs/setup.md", "old\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	base = git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "feature")
	write(".github/radar-policy.json", policy(99))
	write("docs/setup.md", "new\n")
	git("commit", "-q", "-am", "Rewrite setup")
	head = git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	return dir, base, head
}

// stubCommitGitHub answers the API calls review-commit makes for repo o/r.
func stubCommitGitHub(t *testing.T, base, head string, files []githubFile, status string) {
	t.Helper()
	prev := runGH
	t.Cleanup(func() { runGH = prev })
	runGH = func(args ...string) ([]byte, error) {
		endpoint := args[len(args)-1]
		var v any
		switch {
		case endpoint == "repos/o/r":
			v = map[string]string{"default_branch": "main"}
		case endpoint == "repos/o/r/branches/main":
			v = map[string]any{"commit": map[string]string{"sha": base}}
		case strings.HasPrefix(endpoint, "repos/o/r/commits/"):
			v = map[string]string{"sha": head}
		case strings.HasPrefix(endpoint, "repos/o/r/compare/"+base+"..."+head):
			page := map[string]any{
				"status": status, "total_commits": 1,
				"merge_base_commit": map[string]string{"sha": base},
				"commits": []any{map[string]any{
					"commit": map[string]string{"message": "Rewrite setup"},
					"author": map[string]string{"login": "dev"},
				}},
				"files": files,
			}
			v = []any{page}
		default:
			return nil, fmt.Errorf("unexpected gh call %v", args)
		}
		return json.Marshal(v)
	}
}

func setupFiles() []githubFile {
	patch := "@@ -1 +1 @@\n-old\n+new"
	policyPatch := "@@ -1 +1 @@\n-{\"version\":1}\n+{\"version\":99}"
	return []githubFile{
		{Filename: ".github/radar-policy.json", Status: "modified", Additions: 1, Deletions: 1, Patch: &policyPatch},
		{Filename: "docs/setup.md", Status: "modified", Additions: 1, Deletions: 1, Patch: &patch},
	}
}

// The policy is the base branch's, never the commit's: a commit that rewrites
// the policy is still reviewed under the one on main.
func TestReviewCommitReadsPolicyFromBase(t *testing.T) {
	dir, base, head := commitRepo(t)
	prevURL := gitRemoteURL
	t.Cleanup(func() { gitRemoteURL = prevURL })
	gitRemoteURL = func(string) string { return "file://" + dir }
	stubCommitGitHub(t, base, head, setupFiles(), "ahead")

	got, err := reviewCommit(commitReviewOptions{repo: "o/r", sha: head[:12], policy: ".github/radar-policy.json", agent: "rule-based"})
	if err != nil {
		t.Fatal(err)
	}
	r := got.Review
	if r.PolicyVersion != 1 {
		t.Fatalf("policy version %d, want the base's 1", r.PolicyVersion)
	}
	if got.BaseSHA != base || got.MergeBaseSHA != base || got.HeadSHA != head || got.BaseRef != "main" {
		t.Fatalf("bound to %+v", got)
	}
	// The base policy is in approve mode; a commit review still never approves.
	if r.Stage != radar.StageCommit || r.Mode != radar.PullRequestModeShadow || r.Action == radar.PullRequestApprove {
		t.Fatalf("stage %q mode %s action %s", r.Stage, r.Mode, r.Action)
	}
	if r.RequestID != "o/r@"+head {
		t.Fatalf("request id %q", r.RequestID)
	}
}

func TestFetchCommitInputRefusesWhatItCannotReviewCompletely(t *testing.T) {
	base, head := strings.Repeat("b", 40), strings.Repeat("a", 40)

	stubCommitGitHub(t, base, head, nil, "identical")
	_, err := fetchCommitInput("o/r", head, "")
	if err == nil || !strings.Contains(err.Error(), "no changes") || reviewCommitExitCode(err) != 3 {
		t.Fatalf("identical: err = %v, exit %d; want unreviewable (3)", err, reviewCommitExitCode(err))
	}

	var many []githubFile
	for i := range maxCompareFiles {
		many = append(many, githubFile{Filename: fmt.Sprintf("f%d.md", i), Status: "added"})
	}
	stubCommitGitHub(t, base, head, many, "ahead")
	_, err = fetchCommitInput("o/r", head, "")
	if err == nil || !strings.Contains(err.Error(), "at most") || reviewCommitExitCode(err) != 3 {
		t.Fatalf("300 files: err = %v, exit %d; want unreviewable (3)", err, reviewCommitExitCode(err))
	}

	stubCommitGitHub(t, base, head, setupFiles(), "ahead")
	in, err := fetchCommitInput("o/r", head, "")
	if err != nil {
		t.Fatal(err)
	}
	if in.Author != "dev" || len(in.Commits) != 1 || len(in.Files) != 2 || !in.Files[1].ContentComplete {
		t.Fatalf("input %+v", in)
	}
}

// A failure worth retrying (GitHub down, a policy that will not load) keeps
// exit 1, apart from an unreviewable commit (3) and bad flags (2).
func TestReviewCommitExitCodes(t *testing.T) {
	for err, want := range map[error]int{
		errUsage{"x"}: 2,
		fmt.Errorf("wrap: %w", errUnreviewable{"x"}): 3,
		errors.New("GitHub 502"):                     1,
	} {
		if got := reviewCommitExitCode(err); got != want {
			t.Errorf("%v: exit %d, want %d", err, got, want)
		}
	}
}

func TestCommitReviewOptionsValidate(t *testing.T) {
	ok := commitReviewOptions{repo: "Acme/radar", sha: "1a2b3c4", policy: ".github/radar-policy.json"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []commitReviewOptions{
		{repo: "Acme", sha: "1a2b3c4", policy: "p.json"},
		{repo: "Acme/radar", sha: "main", policy: "p.json"},
		{repo: "Acme/radar", sha: "1a2b3c4", policy: "../p.json"},
		{repo: "Acme/radar", sha: "1a2b3c4", policy: "/etc/p.json"},
		{repo: "Acme/radar", sha: "1a2b3c4", policy: "p.json", base: "--upload-pack=x"},
	} {
		if bad.validate() == nil {
			t.Fatalf("%+v should be rejected", bad)
		}
	}
}

func TestAgentsReadWorkdir(t *testing.T) {
	if agentsReadWorkdir("openai,rule-based") || !agentsReadWorkdir("openai,bedrock=us.anthropic.x") {
		t.Fatal("only fireworks, bedrock and codex read a checkout")
	}
}

// A repository with no policy file is reviewed under Radar's default when
// -default-policy asks for it, and fails without it; a policy file that is
// present but invalid never falls back.
func TestReviewCommitDefaultPolicy(t *testing.T) {
	dir, base, head := commitRepo(t)
	prevURL := gitRemoteURL
	t.Cleanup(func() { gitRemoteURL = prevURL })
	gitRemoteURL = func(string) string { return "file://" + dir }
	stubCommitGitHub(t, base, head, setupFiles(), "ahead")

	opts := commitReviewOptions{repo: "o/r", sha: head[:12], policy: ".github/missing.json", agent: "rule-based"}
	if _, err := reviewCommit(opts); err == nil {
		t.Fatal("a missing policy without -default-policy should fail")
	}

	opts.defaultPolicy = true
	got, err := reviewCommit(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.PolicySource != policySourceDefault {
		t.Fatalf("policy source %q, want %q", got.PolicySource, policySourceDefault)
	}
	if r := got.Review; r.Stage != radar.StageCommit || r.Action == radar.PullRequestApprove {
		t.Fatalf("stage %q action %s", r.Stage, r.Action)
	}

	opts.policy = "docs/setup.md"
	if _, err := reviewCommit(opts); err == nil {
		t.Fatal("an invalid policy file should fail, not fall back")
	}

	opts.policy, opts.defaultPolicy = ".github/radar-policy.json", true
	got, err = reviewCommit(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.PolicySource != policySourceRepository {
		t.Fatalf("policy source %q, want the repository's", got.PolicySource)
	}
}

// With -git-cache, the checkouts fetch into a bare repository per GitHub
// repository and borrow its objects, so a later review of the same commits
// fetches nothing: it works with the remote gone.
func TestReviewCommitReusesTheGitCache(t *testing.T) {
	dir, _, head := commitRepo(t)
	// main moves on after the branch, so the base reviewed has a parent the
	// cache never fetches, as on any real repository.
	if err := os.WriteFile(filepath.Join(dir, "docs", "other.md"), []byte("main moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var base string
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "main moves on"}, {"rev-parse", "HEAD"}} {
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		base = strings.TrimSpace(string(out))
	}
	prevURL := gitRemoteURL
	t.Cleanup(func() { gitRemoteURL = prevURL })
	gitRemoteURL = func(string) string { return "file://" + dir }
	stubCommitGitHub(t, base, head, setupFiles(), "ahead")
	cache := t.TempDir()
	opts := commitReviewOptions{repo: "o/r", sha: head[:12], policy: ".github/radar-policy.json", agent: "rule-based", gitCache: cache}

	got, err := reviewCommit(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Review.PolicyVersion != 1 {
		t.Fatalf("policy version %d, want the base's 1", got.Review.PolicyVersion)
	}
	headDir := filepath.Join(t.TempDir(), "head")
	if err := checkoutCommit("o/r", head, headDir, cache); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(headDir, "docs", "setup.md")); err != nil || string(data) != "new\n" {
		t.Fatalf("head checkout docs/setup.md = %q, %v", data, err)
	}
	// One commit deep, as a checkout fetched from the remote is.
	if out, err := exec.Command("git", "-C", headDir, "log", "--format=%H").Output(); err != nil || strings.TrimSpace(string(out)) != head {
		t.Fatalf("git log in the head checkout = %q, %v; want only %s", out, err, head)
	}
	bare := filepath.Join(cache, "o", "r.git")
	for _, sha := range []string{base, head} {
		if err := exec.Command("git", "-C", bare, "rev-parse", "-q", "--verify", "refs/radar/"+sha).Run(); err != nil {
			t.Fatalf("the cache has no ref for %s: %v", sha, err)
		}
	}

	// The remote is gone, so only the cache can serve the same review.
	gitRemoteURL = func(string) string { return "file://" + filepath.Join(t.TempDir(), "gone") }
	if _, err := reviewCommit(opts); err != nil {
		t.Fatalf("a second review of the same base should fetch nothing: %v", err)
	}
	if err := checkoutCommit("o/r", head, filepath.Join(t.TempDir(), "head"), cache); err != nil {
		t.Fatalf("a second checkout of the same head should fetch nothing: %v", err)
	}
	baseDir := filepath.Join(t.TempDir(), "base")
	if err := checkoutCommit("o/r", base, baseDir, cache); err != nil {
		t.Fatalf("a second checkout of the same base should fetch nothing: %v", err)
	}
	if out, err := exec.Command("git", "-C", baseDir, "log", "--format=%H").Output(); err != nil || strings.TrimSpace(string(out)) != base {
		t.Fatalf("git log in the base checkout = %q, %v; want only %s", out, err, base)
	}
	opts.gitCache = ""
	if _, err := reviewCommit(opts); err == nil {
		t.Fatal("without the cache the review fetches from the remote, which is gone")
	}
}

// A cache that cannot be used never fails the review: the checkout is
// fetched from the remote as without one, and nothing is written outside it.
func TestCheckoutCommitFallsBackWithoutAUsableCache(t *testing.T) {
	dir, base, _ := commitRepo(t)
	prevURL := gitRemoteURL
	t.Cleanup(func() { gitRemoteURL = prevURL })
	gitRemoteURL = func(string) string { return "file://" + dir }
	notADir := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	for _, tc := range []struct{ repo, cache string }{{"o/r", notADir}, {"../r", cache}} {
		out := filepath.Join(t.TempDir(), "base")
		if err := checkoutCommit(tc.repo, base, out, tc.cache); err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if _, err := os.Stat(filepath.Join(out, ".github", "radar-policy.json")); err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cache), "r.git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a repository named ../r must not get a cache outside the cache directory: %v", err)
	}
}

// The built-in default is a valid policy.
func TestDefaultPolicyIsValid(t *testing.T) {
	p, err := decodePullRequestPolicy(defaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != radar.PullRequestModeShadow {
		t.Fatalf("mode %s, want shadow", p.Mode)
	}
}
