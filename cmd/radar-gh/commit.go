package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/travisjeffery/radar"
)

// maxCompareFiles is the most files GitHub's compare API lists. A change
// that reaches it may have more, so it cannot be reviewed completely.
const maxCompareFiles = 300

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern  = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
)

// defaultPolicy is the policy review-commit falls back to, with
// -default-policy, for a repository that has none at -policy: the hosted
// review of a repository with no Radar CI of its own. It is
// the shape of backend's, without its repository-specific paths, rules or
// .gitattributes; it reviews against any default branch and requires every
// reviewer -agent names to pass.
//
//go:embed default-policy.json
var defaultPolicy []byte

const (
	policySourceRepository = "repository"
	policySourceDefault    = "radar-default"
)

// commitReviewOptions selects the commit and how to review it. The policy is
// a path inside the repository, read at the base branch's tip.
type commitReviewOptions struct {
	repo, sha, base string
	policy          string
	agent           string
	packsDir        string
	// defaultPolicy falls back to Radar's built-in policy when the
	// repository has no file at policy.
	defaultPolicy bool
	// gitCache is a directory of bare repositories, one per GitHub
	// repository, that the checkouts fetch into and borrow objects from, so
	// a later review downloads only what earlier ones did not.
	gitCache string
}

// commitReviewResult is review-commit's JSON output: the review and the
// commits it is bound to, so a caller can tell which base the policy came from.
type commitReviewResult struct {
	Repository   string `json:"repository"`
	BaseRef      string `json:"base_ref"`
	BaseSHA      string `json:"base_sha"`
	MergeBaseSHA string `json:"merge_base_sha"`
	HeadSHA      string `json:"head_sha"`
	// PolicySource is where the policy came from: "repository" (policy at
	// the base branch's tip) or "radar-default" (-default-policy).
	PolicySource string                  `json:"policy_source"`
	Review       radar.PullRequestReview `json:"review"`
	// Markdown is the review as CI renders it, so a caller of -json (the
	// hosted Radar MCP) shows the same text without its own renderer.
	Markdown string `json:"markdown"`
}

// runReviewCommit reviews a pushed commit that need not have a pull request,
// as CI's verdict would review a pull request from it into the base branch.
// It is report-only: it never approves, comments or posts.
func runReviewCommit(args []string) int {
	flags := flag.NewFlagSet("radar-gh review-commit", flag.ContinueOnError)
	var opts commitReviewOptions
	flags.StringVar(&opts.repo, "repo", "", "GitHub owner/repository (required)")
	flags.StringVar(&opts.sha, "sha", "", "commit to review, pushed to the repository (required)")
	flags.StringVar(&opts.base, "base", "", "branch the commit would merge into; default the repository's default branch")
	flags.StringVar(&opts.policy, "policy", ".github/radar-policy.json", "policy path in the repository, read at the base branch's tip")
	flags.StringVar(&opts.agent, "agent", "openai", "review agents, as for radar-gh review")
	flags.BoolVar(&opts.defaultPolicy, "default-policy", false, "use Radar's built-in default policy when the repository has no file at -policy")
	flags.StringVar(&opts.packsDir, "review-packs", "", "directory of review packs (<id>.md) the policy's review_criteria rules name")
	flags.StringVar(&opts.gitCache, "git-cache", "", "directory that keeps a bare repository per GitHub repository for the checkouts, so a later review downloads only the objects earlier ones did not; default none, each checkout fetched in full")
	asJSON := flags.Bool("json", false, "print the structured result as JSON instead of the Markdown review")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if err := opts.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review-commit:", err)
		return 2
	}
	result, err := reviewCommit(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review-commit:", err)
		return reviewCommitExitCode(err)
	}
	result.Markdown = renderReviewComment(result.Repository, result.Review, "")
	if *asJSON {
		if err := writeJSON(os.Stdout, result); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review-commit:", err)
			return 1
		}
		return 0
	}
	fmt.Print(result.Markdown)
	return 0
}

// errUnreviewable is a commit no review of it can cover: no changes against
// its base, or more files than GitHub lists. Asking again gives the same
// answer, so it exits 3, apart from a failure (1) worth retrying.
type errUnreviewable struct{ msg string }

func (e errUnreviewable) Error() string { return e.msg }

// reviewCommitExitCode is review-commit's exit status for err: 2 for flags
// that cannot work, 3 for a commit that cannot be reviewed, 1 otherwise.
func reviewCommitExitCode(err error) int {
	switch {
	case errors.As(err, new(errUsage)):
		return 2
	case errors.As(err, new(errUnreviewable)):
		return 3
	}
	return 1
}

func (o commitReviewOptions) validate() error {
	switch {
	case !repoPattern.MatchString(o.repo):
		return errors.New("-repo must be owner/repository")
	case !shaPattern.MatchString(o.sha):
		return errors.New("-sha must be a commit SHA")
	case o.base != "" && (strings.HasPrefix(o.base, "-") || strings.Contains(o.base, "..")):
		return errors.New("-base must be a branch name")
	case !filepath.IsLocal(o.policy):
		return errors.New("-policy must be a relative path inside the repository")
	}
	return nil
}

// reviewCommit fetches the commit's changes and the base branch's policy
// itself, so the result is bound to what is pushed, and runs the reviewer CI
// builds: the same agents, generated-file list and review criteria.
func reviewCommit(opts commitReviewOptions) (commitReviewResult, error) {
	in, err := fetchCommitInput(opts.repo, opts.sha, opts.base)
	if err != nil {
		return commitReviewResult{}, err
	}
	tmp, err := os.MkdirTemp("", "radar-commit-")
	if err != nil {
		return commitReviewResult{}, err
	}
	defer os.RemoveAll(tmp)

	// The policy, rules, guidance and generated-file list come from the base
	// branch's tip, as CI reads them from the default branch, never from the
	// commit under review.
	baseDir := filepath.Join(tmp, "base")
	if err := checkoutCommit(opts.repo, in.BaseSHA, baseDir, opts.gitCache); err != nil {
		return commitReviewResult{}, fmt.Errorf("checking out base %s: %w", in.BaseSHA, err)
	}
	policyPath := filepath.Join(baseDir, filepath.FromSlash(opts.policy))
	policySource, packsDir := policySourceRepository, opts.packsDir
	policy, err := loadPullRequestPolicy(policyPath)
	if errors.Is(err, fs.ErrNotExist) && opts.defaultPolicy {
		// Only an absent file falls back; a policy that is present but
		// invalid still fails, so a broken one is never silently replaced.
		policySource, packsDir = policySourceDefault, ""
		policy, err = decodePullRequestPolicy(defaultPolicy)
		// Every reviewer the caller runs must pass, as backend's two must.
		policy.RequiredReviewers = len(strings.Split(opts.agent, ","))
	}
	if err != nil {
		return commitReviewResult{}, fmt.Errorf("policy at %s: %w", in.BaseSHA, err)
	}

	// Agents that read the repository read the commit, in its own checkout,
	// as CI's read the pull request head.
	workdir := ""
	if agentsReadWorkdir(opts.agent) {
		workdir = filepath.Join(tmp, "head")
		if err := checkoutCommit(opts.repo, in.HeadSHA, workdir, opts.gitCache); err != nil {
			return commitReviewResult{}, fmt.Errorf("checking out %s: %w", in.HeadSHA, err)
		}
	}
	// The default policy names no review criteria, so the packs the
	// repository's rules would select do not apply.
	reviewer, _, err := buildReviewer(policy, policyPath, opts.agent, packsDir, workdir)
	if err != nil {
		return commitReviewResult{}, err
	}
	return commitReviewResult{
		Repository: in.Repository, BaseRef: in.BaseRef, BaseSHA: in.BaseSHA,
		MergeBaseSHA: in.MergeBaseSHA, HeadSHA: in.HeadSHA, PolicySource: policySource,
		Review: reviewer.ReviewCommit(in),
	}, nil
}

// agentsReadWorkdir reports whether any agent in spec reads a checkout, the
// agents CI gives the pull request head to.
func agentsReadWorkdir(spec string) bool {
	for _, entry := range strings.Split(spec, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(entry), "=")
		switch name {
		case "fireworks", "bedrock", "codex":
			return true
		}
	}
	return false
}

type githubCompare struct {
	Status          string `json:"status"`
	TotalCommits    int    `json:"total_commits"`
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	Commits []struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	} `json:"commits"`
	Files []githubFile `json:"files"`
}

// fetchCommitInput resolves the commit and the base branch's tip, then reads
// the changes from their merge base to the commit: the files a pull request
// from the commit into base would list, in the same form.
func fetchCommitInput(repo, sha, base string) (radar.CommitInput, error) {
	if base == "" {
		var r struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := ghJSON(&r, "api", "repos/"+repo); err != nil {
			return radar.CommitInput{}, fmt.Errorf("reading the default branch: %w", err)
		}
		base = r.DefaultBranch
	}
	var branch struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := ghJSON(&branch, "api", fmt.Sprintf("repos/%s/branches/%s", repo, url.PathEscape(base))); err != nil {
		return radar.CommitInput{}, fmt.Errorf("reading branch %s: %w", base, err)
	}
	var head struct {
		SHA string `json:"sha"`
	}
	if err := ghJSON(&head, "api", fmt.Sprintf("repos/%s/commits/%s", repo, sha)); err != nil {
		return radar.CommitInput{}, fmt.Errorf("reading commit %s (is it pushed?): %w", sha, err)
	}
	baseSHA, headSHA := branch.Commit.SHA, head.SHA
	if baseSHA == "" || headSHA == "" {
		return radar.CommitInput{}, errors.New("GitHub returned no commit SHA")
	}

	// Files come on the first page only; later pages carry more commits.
	var pages []githubCompare
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/compare/%s...%s?per_page=100", repo, baseSHA, headSHA)); err != nil {
		return radar.CommitInput{}, fmt.Errorf("comparing %s with %s: %w", base, headSHA, err)
	}
	if len(pages) == 0 {
		return radar.CommitInput{}, errors.New("compare returned nothing")
	}
	in := radar.CommitInput{
		Repository: repo, BaseRef: base, BaseSHA: baseSHA,
		MergeBaseSHA: pages[0].MergeBaseCommit.SHA, HeadSHA: headSHA,
	}
	var files []githubFile
	for _, page := range pages {
		files = append(files, page.Files...)
		for _, c := range page.Commits {
			// Oldest first, so the last is the head commit.
			in.Commits = append(in.Commits, c.Commit.Message)
			in.Author = ""
			if c.Author != nil {
				in.Author = c.Author.Login
			}
		}
	}
	switch {
	case pages[0].Status == "identical" || pages[0].Status == "behind" || len(files) == 0:
		return radar.CommitInput{}, errUnreviewable{fmt.Sprintf("%s has no changes to review against %s", headSHA, base)}
	case len(files) >= maxCompareFiles:
		return radar.CommitInput{}, errUnreviewable{fmt.Sprintf("GitHub lists at most %d changed files, so a change this large cannot be reviewed completely", maxCompareFiles)}
	}
	if len(in.Commits) != pages[0].TotalCommits {
		// Commit messages are context only; the review goes on without the rest.
		fmt.Fprintf(os.Stderr, "radar-gh review-commit: read %d of %d commit messages\n", len(in.Commits), pages[0].TotalCommits)
	}
	in.Files = toPullRequestFiles(files)
	return in, nil
}

// toPullRequestFiles converts GitHub's file entries, marking a file complete
// only when GitHub returned its whole patch.
func toPullRequestFiles(files []githubFile) []radar.PullRequestFile {
	out := make([]radar.PullRequestFile, 0, len(files))
	for _, file := range files {
		patch := ""
		if file.Patch != nil {
			patch = *file.Patch
		}
		out = append(out, radar.PullRequestFile{
			Path: file.Filename, PreviousPath: file.PreviousFilename, Status: file.Status,
			Additions: file.Additions, Deletions: file.Deletions, Patch: patch,
			ContentComplete: file.Patch != nil && patchLineCountsMatch(patch, file.Additions, file.Deletions),
		})
	}
	return out
}

// gitRemoteURL is where checkoutCommit fetches from; tests point it at a
// local repository.
var gitRemoteURL = func(repo string) string { return "https://github.com/" + repo + ".git" }

// checkoutCommit checks out exactly sha into dir. The gh credential helper is
// passed per command, so no token is left in the checkout for an agent that
// reads it, as CI checks out without persisting credentials. With a cache
// directory, the commit is fetched into the repository's cache first and the
// checkout borrows the cache's objects, so only objects the cache lacks are
// downloaded. If the cache fails, the checkout is fetched from
// GitHub as without one.
func checkoutCommit(repo, sha, dir, cache string) error {
	if cache != "" {
		err := checkoutFromCache(repo, sha, dir, cache)
		if err == nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "radar-gh review-commit: git cache: %v; fetching %s from GitHub\n", err, sha)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return fetchCheckout(gitRemoteURL(repo), sha, dir)
}

// checkoutFromCache fetches sha into repo's bare repository under cache,
// unless an earlier review did, then checks it out at dir from the cache's
// objects. The cache's refs, one per fetched commit, are what git tells
// GitHub it already has.
func checkoutFromCache(repo, sha, dir, cache string) error {
	owner, name, _ := strings.Cut(repo, "/")
	if owner == "." || owner == ".." || name == "." || name == ".." {
		return fmt.Errorf("no cache for repository %q", repo)
	}
	bare, err := filepath.Abs(filepath.Join(cache, owner, name+".git"))
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		if err := os.MkdirAll(bare, 0o755); err != nil {
			return err
		}
		if err := runGit(bare, "init", "-q", "--bare"); err != nil {
			return fmt.Errorf("git init: %w", err)
		}
	}
	ref := "refs/radar/" + sha
	if exec.Command("git", "-C", bare, "rev-parse", "-q", "--verify", ref+"^{commit}").Run() != nil {
		if err := runGit(bare, "fetch", "-q", "--depth=1", "--no-tags", gitRemoteURL(repo), "+"+sha+":"+ref); err != nil {
			return fmt.Errorf("git fetch: %w", err)
		}
	}
	// The checkout borrows the cache's objects rather than fetching them.
	// The cache's shallow file, which names the commits whose parents it
	// lacks, does not come with them, so the checkout's names its commit: one
	// commit deep, as a fetched checkout is.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := runGit(dir, "init", "-q"); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	gitDir := filepath.Join(dir, ".git")
	if err := os.WriteFile(filepath.Join(gitDir, "objects", "info", "alternates"), []byte(filepath.ToSlash(filepath.Join(bare, "objects"))+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(gitDir, "shallow"), []byte(sha+"\n"), 0o644); err != nil {
		return err
	}
	if err := runGit(dir, "checkout", "-q", "--detach", sha); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	return checkedOut(dir, sha)
}

// runGit runs git in dir with the gh credential helper for this command only.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}, args...)...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// fetchCheckout fetches sha from source into a new repository at dir, one
// commit deep, and checks it out.
func fetchCheckout(source, sha, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth=1", "--no-tags", source, sha},
		{"checkout", "-q", "--detach", "FETCH_HEAD"},
	} {
		if err := runGit(dir, args...); err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
	}
	return checkedOut(dir, sha)
}

// checkedOut confirms that dir has exactly sha checked out.
func checkedOut(dir, sha string) error {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != sha {
		return fmt.Errorf("checked out %s, want %s", got, sha)
	}
	return nil
}
