package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/travisjeffery/radar"
	"github.com/travisjeffery/radar/bedrock"
)

type githubPullRequest struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	State        string `json:"state"`
	Draft        bool   `json:"draft"`
	ChangedFiles int    `json:"changed_files"`
	Labels       []struct {
		Name string `json:"name"`
	} `json:"labels"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

type githubFile struct {
	Filename         string  `json:"filename"`
	PreviousFilename string  `json:"previous_filename"`
	Status           string  `json:"status"`
	Additions        int     `json:"additions"`
	Deletions        int     `json:"deletions"`
	Patch            *string `json:"patch"`
}

type githubReview struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Body        string `json:"body"`
	SubmittedAt string `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	CommitID string `json:"commit_id"`
}

type checkObservation struct {
	Observed    bool
	Passing     bool
	Fingerprint string
}

type githubSnapshot struct {
	Input            radar.PullRequestInput
	ExistingApproval bool
}

const radarApprovalPrefix = "Radar policy v"

func runReview(args []string) int {
	flags := flag.NewFlagSet("radar-gh review", flag.ContinueOnError)
	repo := flags.String("repo", "", "GitHub owner/repository (required)")
	prNumber := flags.Int("pr", 0, "pull request number (required)")
	policyPath := flags.String("policy", "", "path to a versioned JSON policy (required)")
	agentName := flags.String("agent", "openai", "review agents, comma-separated: openai, anthropic, fireworks, bedrock, bedrock-openai, codex, or rule-based, each optionally name=model")
	expectedHead := flags.String("expected-head", "", "event head SHA; required in approval mode")
	settle := flags.Duration("settle", 10*time.Second, "time between check observations")
	apply := flags.Bool("apply", false, "allow an APPROVE review when policy mode is approve")
	deferApply := flags.Bool("defer-apply", false, "approval mode: decide, and write the decision to -approval-request for `radar-gh apply` instead of approving here")
	requestPath := flags.String("approval-request", "", "with -defer-apply: write the approval request (JSON) to this file")
	trustPath := flags.String("author-trust", "", "private login-to-tier roster (JSON); enables the shadow tier evaluation")
	trustAuditPath := flags.String("author-trust-audit", "", "file for the private tier audit record; required with -author-trust")
	packsDir := flags.String("review-packs", "", "directory of review packs (<id>.md) the policy's review_criteria rules name")
	stage := flags.String("stage", "verdict", "verdict (wait for checks and decide) or findings (review now; never decides)")
	commentPath := flags.String("comment", "", "write the findings comment (Markdown) to this file")
	recordPath := flags.String("record", "", "write the agents' verdicts for this head to this file, for -reuse")
	inlinePath := flags.String("inline", "", "write the P0/P1 findings on diff lines as review comments (JSON) to this file")
	reusePath := flags.String("reuse", "", "reuse the verdicts a -record file holds for this head instead of calling the agents again")
	priorPath := flags.String("reuse-prior", "", "findings stage: a -record file of an earlier head of this pull request, whose verdicts answer byte-identical requests (needs -reuse-prior-head)")
	priorHead := flags.String("reuse-prior-head", "", "the earlier head the -reuse-prior record must be for")
	reviewWorkdir := flags.String("review-workdir", "", "read-only checkout of the pull request head: the codex agent explores it, and the fireworks agent reads it through tools")
	codexWorkdir := flags.String("codex-workdir", "", "older name for -review-workdir")
	clarificationsPath := flags.String("clarifications", "", "the pull request's clarification ledger (JSON) from its clarify runs; its outcomes apply to the findings the verdict counts")
	flags.Int64Var(&approverAppID, "approver-app-id", 0, "approval mode: the approver App's ID; a base branch that requires its "+radar.ApprovalCurrentContext+" status keeps approvals current")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	findingsStage := *stage == radar.StageFindings
	if !findingsStage && *stage != "verdict" {
		fmt.Fprintln(os.Stderr, "radar-gh review: -stage must be verdict or findings")
		return 2
	}
	if findingsStage && (*apply || *deferApply || *trustPath != "") {
		fmt.Fprintln(os.Stderr, "radar-gh review: the findings stage never decides; -apply, -defer-apply and -author-trust need -stage verdict")
		return 2
	}
	if *repo == "" || *prNumber <= 0 || *policyPath == "" || *settle < 0 {
		fmt.Fprintln(os.Stderr, "radar-gh review: -repo, -pr, and -policy are required; -settle cannot be negative")
		return 2
	}
	if (*trustPath == "") != (*trustAuditPath == "") {
		fmt.Fprintln(os.Stderr, "radar-gh review: -author-trust and -author-trust-audit go together")
		return 2
	}

	policy, err := loadPullRequestPolicy(*policyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	// The findings stage never approves, so approval mode's flags apply only
	// to the verdict.
	if policy.Mode == radar.PullRequestModeApprove && !findingsStage {
		if *apply == *deferApply {
			fmt.Fprintln(os.Stderr, "radar-gh review: approval mode requires exactly one of -apply and -defer-apply")
			return 2
		}
		if *deferApply && *requestPath == "" {
			fmt.Fprintln(os.Stderr, "radar-gh review: -defer-apply needs -approval-request")
			return 2
		}
		if *expectedHead == "" {
			fmt.Fprintln(os.Stderr, "radar-gh review: approval mode requires -expected-head")
			return 2
		}
	}

	workdir := *reviewWorkdir
	if workdir == "" {
		workdir = *codexWorkdir
	}
	reviewer, _, err := buildReviewer(policy, *policyPath, *agentName, *packsDir, workdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		if errors.As(err, new(errUsage)) {
			return 2
		}
		return 1
	}
	if *clarificationsPath != "" {
		// Outcomes that cannot be read are left out: the findings then
		// count as reported, which is never less strict.
		if ledger, err := loadClarificationLedger(*clarificationsPath, *prNumber); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: not applying clarifications:", err)
		} else {
			reviewer.UseClarifications(ledger)
		}
	}

	fetch := fetchGitHubSnapshot
	if findingsStage {
		fetch = fetchFindingsSnapshot
	}
	first, err := fetch(*repo, *prNumber, policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	if *expectedHead != "" && first.Input.HeadSHA != *expectedHead {
		fmt.Fprintf(os.Stderr, "radar-gh review: expected head %s, found %s\n", *expectedHead, first.Input.HeadSHA)
		return 1
	}
	if *reusePath != "" {
		// A record that cannot be used costs only new model calls, so it
		// never fails the review.
		if err := useReviewRecord(reviewer, *reusePath, first.Input.HeadSHA); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: not reusing recorded reviews:", err)
		}
	}
	if *priorPath != "" {
		// As -reuse: a record that cannot be used costs only model calls.
		if !findingsStage || *priorHead == "" {
			fmt.Fprintln(os.Stderr, "radar-gh review: -reuse-prior needs -stage findings and -reuse-prior-head; ignoring it")
		} else if err := usePriorReviewRecord(reviewer, *priorPath, *priorHead, first.Input.HeadSHA); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: not reusing the earlier head's reviews:", err)
		}
	}
	out := outputs{comment: *commentPath, record: *recordPath, inline: *inlinePath, repo: *repo, number: *prNumber}
	if *commentPath != "" && !findingsStage {
		// A comment written without the one it replaces would drop the
		// rounds it records, whether or not the reviewers run.
		out.readPrevious()
	}
	if findingsStage {
		first.Input.Context = fetchPRContext(*repo, *prNumber, first.Input.Title, first.Input.HeadSHA)
		out.readPrevious()
		loadHistory(*repo, *prNumber, &first.Input, out.previous)
		decision := reviewer.Findings(first.Input)
		return finishReview(*repo, *prNumber, decision, first.Input, reviewer, out)
	}
	if *settle > 0 {
		time.Sleep(*settle)
	}
	second, err := fetchGitHubSnapshot(*repo, *prNumber, policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	second.Input.ChecksStable = snapshotsStable(first.Input, second.Input)
	// Only when the reviewers can run: the workflow re-runs this every 15s
	// while checks are pending, and each fetch spends the repository's
	// shared GITHUB_TOKEN budget.
	if second.Input.ChecksObserved && second.Input.ChecksStable && second.Input.ChecksPassing {
		second.Input.Context = fetchPRContext(*repo, *prNumber, second.Input.Title, second.Input.HeadSHA)
		if out.previous == "" {
			out.readPrevious()
		}
		loadHistory(*repo, *prNumber, &second.Input, out.previous)
	}
	if *expectedHead != "" && second.Input.HeadSHA != *expectedHead {
		fmt.Fprintf(os.Stderr, "radar-gh review: head changed from expected %s to %s\n", *expectedHead, second.Input.HeadSHA)
		return 1
	}

	var decision radar.PullRequestReview
	if *trustPath == "" {
		decision = reviewer.Review(second.Input)
	} else {
		var audit radar.AuthorTrustAudit
		decision, audit = reviewWithAuthorTrust(reviewer, *repo, *prNumber, second.Input, *trustPath)
		// Stderr and stdout are public; only the audit file names the tier.
		if err := writeAuditFile(*trustAuditPath, audit); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: writing author trust audit failed")
			return 1
		}
	}
	if *deferApply {
		// The apply job re-checks the pull request and acts on every
		// verdict: it approves, or dismisses Radar's own approvals.
		if err := writeJSONFile(*requestPath, newApprovalRequest(*repo, *prNumber, second.Input, decision)); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: writing the approval request:", err)
			return 1
		}
		return finishReview(*repo, *prNumber, decision, second.Input, reviewer, out)
	}
	if decision.Action == radar.PullRequestApprove {
		if second.ExistingApproval {
			return finishReview(*repo, *prNumber, decision, second.Input, reviewer, out)
		}
		current, err := fetchGitHubSnapshot(*repo, *prNumber, policy)
		if err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: pre-approval revalidation:", err)
			return 1
		}
		if err := validateApprovalSnapshot(policy, second.Input, current.Input, *expectedHead); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: pre-approval revalidation:", err)
			return 1
		}
		if !current.ExistingApproval {
			if err := approvePullRequest(*repo, *prNumber, current.Input.HeadSHA, decision); err != nil {
				fmt.Fprintln(os.Stderr, "radar-gh review: approving:", err)
				return 1
			}
		}
	}
	return finishReview(*repo, *prNumber, decision, second.Input, reviewer, out)
}

// buildReviewer constructs the reviewer a stage runs: the policy's agents,
// generated-file list and review criteria, all read from the trusted checkout
// the policy came from.
func buildReviewer(policy radar.PullRequestPolicy, policyPath, agentName, packsDir, workdir string) (*radar.PullRequestReviewer, []radar.ReviewAgent, error) {
	agents, err := newReviewAgents(agentName, agentOptions{workdir: workdir})
	if err != nil {
		return nil, nil, err
	}
	reviewer, err := radar.NewPullRequestReviewer(policy, radar.HeuristicScorer{}, agents...)
	if err != nil {
		return nil, nil, err
	}
	if policy.GeneratedFiles != nil && policy.GeneratedFiles.Gitattributes != "" {
		// Read from the trusted checkout the policy came from, never the PR head.
		path, err := checkoutFile(policyPath, policy.GeneratedFiles.Gitattributes)
		if err != nil {
			return nil, nil, fmt.Errorf("locating generated_files.gitattributes: %w", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("reading generated_files.gitattributes: %w", err)
		}
		if err := reviewer.UseGitattributes(data); err != nil {
			return nil, nil, err
		}
	}

	if policy.ReviewCriteria != nil {
		criteria, err := loadReviewCriteria(policyPath, policy, packsDir)
		if err != nil {
			return nil, nil, fmt.Errorf("review criteria: %w", err)
		}
		if err := reviewer.UseReviewCriteria(criteria); err != nil {
			return nil, nil, err
		}
	} else if packsDir != "" {
		return nil, nil, errUsage{"-review-packs needs a policy with review_criteria"}
	}

	return reviewer, agents, nil
}

// errUsage is a flag combination that cannot work, exit status 2.
type errUsage struct{ msg string }

func (e errUsage) Error() string { return e.msg }

// loadClarificationLedger reads the pull request's clarification outcomes.
func loadClarificationLedger(path string, pr int) (radar.ClarificationLedger, error) {
	var l radar.ClarificationLedger
	data, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return radar.ClarificationLedger{}, fmt.Errorf("decoding %s: %w", path, err)
	}
	if err := l.Validate(pr); err != nil {
		return radar.ClarificationLedger{}, err
	}
	return l, nil
}

// outputs are the files a review writes besides the decision on stdout,
// and the Radar comment the new one replaces.
type outputs struct {
	comment, record, inline string
	previous                string
	// previousUnread is set when the previous comment could not be fetched.
	previousUnread bool
	repo           string
	number         int
}

// readPrevious fetches the Radar comment the new one replaces.
func (o *outputs) readPrevious() {
	var ok bool
	o.previous, ok = readPreviousComment(o.repo, o.number)
	o.previousUnread = !ok
}

// finishReview writes the record, comment and inline comments, then the
// decision to stdout.
func finishReview(repo string, number int, decision radar.PullRequestReview, in radar.PullRequestInput, reviewer *radar.PullRequestReviewer, out outputs) int {
	if out.record != "" {
		if err := writeJSONFile(out.record, reviewer.ReviewRecord(decision.HeadSHA)); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: writing review record:", err)
			return 1
		}
	}
	if out.comment != "" {
		decision.PullRequestTotals = pullRequestTotals(decision, out.previous, out.previousUnread)
		if err := os.WriteFile(out.comment, []byte(renderReviewComment(repo, decision, out.previous)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: writing comment:", err)
			return 1
		}
	}
	if out.inline != "" {
		comments := inlineComments(decision, in.Files)
		if comments == nil {
			comments = []inlineComment{}
		}
		if err := writeJSONFile(out.inline, comments); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: writing inline comments:", err)
			return 1
		}
	}
	if err := writeJSON(os.Stdout, decision); err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	return 0
}

// readPreviousComment returns Radar's comment on the pull request, which
// records the review rounds so far: the previous round's findings for the
// status labels, and the baseline a later round blocks against.
// Without it every finding shows unlabelled and every blocking finding
// counts, so a failure to read it is logged, not fatal.
// It reports false when the lookup failed, as against there being no comment.
func readPreviousComment(repo string, number int) (string, bool) {
	previous, err := fetchPreviousComment(repo, number)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh: reading the previous Radar comment:", err)
		return "", false
	}
	return previous, true
}

// fetchPreviousComment returns the body of the Radar comment on the pull
// request, or "" when there is none.
func fetchPreviousComment(repo string, number int) (string, error) {
	var pages [][]struct {
		Body string `json:"body"`
		User struct {
			Type string `json:"type"`
		} `json:"user"`
	}
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, number)); err != nil {
		return "", err
	}
	for _, page := range pages {
		for _, c := range page {
			if strings.EqualFold(c.User.Type, "Bot") && strings.HasPrefix(c.Body, reviewCommentMarker) {
				return c.Body, nil
			}
		}
	}
	return "", nil
}

func useReviewRecord(reviewer *radar.PullRequestReviewer, path, head string) error {
	record, err := readReviewRecord(path)
	if err != nil {
		return err
	}
	return reviewer.UseReviewRecord(record, head)
}

func usePriorReviewRecord(reviewer *radar.PullRequestReviewer, path, priorHead, head string) error {
	record, err := readReviewRecord(path)
	if err != nil {
		return err
	}
	return reviewer.UsePriorReviewRecord(record, priorHead, head)
}

func readReviewRecord(path string) (radar.ReviewRecord, error) {
	var record radar.ReviewRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil {
		return record, fmt.Errorf("decoding %s: %w", path, err)
	}
	return record, nil
}

func writeJSONFile(path string, value any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := writeJSON(f, value); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// checkoutFile resolves rel against the root of the git checkout holding
// the policy, so a file the policy names is read from that trusted checkout
// whatever the working directory.
func checkoutFile(policyPath, rel string) (string, error) {
	dir := filepath.Dir(policyPath)
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("policy %s is not inside a git checkout: %w", policyPath, err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), rel), nil
}

// snapshotsStable reports whether two observations show the same pull request
// state: its head and checks, and for a stacked pull request the same chain
// of parents at the same heads, since a parent force-push changes the diff
// without moving this head.
func snapshotsStable(first, second radar.PullRequestInput) bool {
	if first.HeadSHA != second.HeadSHA || first.CheckFingerprint != second.CheckFingerprint || first.BaseRef != second.BaseRef {
		return false
	}
	if len(first.Stack) != len(second.Stack) {
		return false
	}
	for i := range first.Stack {
		if first.Stack[i] != second.Stack[i] {
			return false
		}
	}
	return true
}

// reviewWithAuthorTrust never lets a trust failure change or block the live
// decision: it falls back to the plain review and records why in the audit.
func reviewWithAuthorTrust(reviewer *radar.PullRequestReviewer, repo string, number int, in radar.PullRequestInput, rosterPath string) (radar.PullRequestReview, radar.AuthorTrustAudit) {
	fail := func(reason string) (radar.PullRequestReview, radar.AuthorTrustAudit) {
		live, audit := reviewer.ReviewWithAuthorTrust(in, radar.AuthorTrust{})
		audit.Error = reason
		return live, audit
	}
	data, err := os.ReadFile(rosterPath)
	if err != nil {
		return fail("roster unreadable")
	}
	roster, err := radar.ParseAuthorTrustRoster(data)
	if err != nil {
		return fail(err.Error())
	}
	messages, err := fetchCommitMessages(repo, number)
	if err != nil {
		return fail("commit messages unavailable")
	}
	trust := radar.AuthorTrust{AgentAuthored: radar.AgentAuthored(in.Body, messages)}
	if strings.EqualFold(in.AuthorType, "User") {
		trust.Tier = roster.Tier(in.Author)
	}
	return reviewer.ReviewWithAuthorTrust(in, trust)
}

// fetchCommitMessages returns every commit message; GitHub lists at most 250
// commits per pull request, so a longer one is an error.
func fetchCommitMessages(repo string, number int) ([]string, error) {
	var pages [][]struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/commits?per_page=100", repo, number)); err != nil {
		return nil, err
	}
	var messages []string
	for _, page := range pages {
		for _, c := range page {
			messages = append(messages, c.Commit.Message)
		}
	}
	if len(messages) >= 250 {
		return nil, errors.New("commit list may be truncated")
	}
	return messages, nil
}

// writeAuditFile refuses a symlink and replaces any existing file, because
// OpenFile's mode applies only on creation and the audit names tiers.
func writeAuditFile(path string, audit radar.AuthorTrustAudit) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("audit path is not a regular file")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := writeJSON(f, audit); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func loadPullRequestPolicy(path string) (radar.PullRequestPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return radar.PullRequestPolicy{}, fmt.Errorf("opening policy: %w", err)
	}
	return decodePullRequestPolicy(data)
}

func decodePullRequestPolicy(data []byte) (radar.PullRequestPolicy, error) {
	var policy radar.PullRequestPolicy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&policy); err != nil {
		return policy, fmt.Errorf("decoding policy: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return policy, errors.New("policy must contain exactly one JSON object")
	}
	if err := policy.Validate(); err != nil {
		return policy, err
	}
	return policy, nil
}

// newReviewAgents parses -agent: one agent keeps reading $RADAR_ACR_MODEL;
// with several, each names its own model and $RADAR_ACR_MODEL must be unset
// so one model id cannot leak into another provider.
// agentOptions carries settings only some agents use.
type agentOptions struct {
	// workdir is the read-only pull request checkout reviewers may read.
	workdir string
}

func newReviewAgents(spec string, opts agentOptions) ([]radar.ReviewAgent, error) {
	entries := strings.Split(spec, ",")
	if len(entries) > 1 && os.Getenv("RADAR_ACR_MODEL") != "" {
		return nil, errors.New("with several agents, give each model as name=model and leave RADAR_ACR_MODEL unset")
	}
	seen := map[string]bool{}
	var agents []radar.ReviewAgent
	for _, entry := range entries {
		name, model, explicit := strings.Cut(strings.TrimSpace(entry), "=")
		if !explicit && len(entries) == 1 {
			model = os.Getenv("RADAR_ACR_MODEL")
		}
		if seen[name] {
			return nil, fmt.Errorf("agent %q is listed twice", name)
		}
		seen[name] = true
		agent, err := newReviewAgent(name, model, opts)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, nil
}

func newReviewAgent(name, model string, opts agentOptions) (radar.ReviewAgent, error) {
	switch name {
	case "openai":
		agent, err := radar.NewOpenAIAgent()
		if err == nil && model != "" {
			agent.Model = model
		}
		return agent, err
	case "anthropic":
		agent, err := radar.NewLLMAgent()
		if err == nil && model != "" {
			agent.Model = model
		}
		return agent, err
	case "fireworks":
		agent, err := radar.NewFireworksAgentForModel(model)
		if err != nil || opts.workdir == "" {
			return agent, err
		}
		// With a checkout, GLM reads the repository through tools.
		if agent.Tools, err = radar.NewRepoTools(opts.workdir); err != nil {
			return nil, err
		}
		if agent.ToolCallBudget, err = envInt("RADAR_TOOL_CALLS"); err != nil {
			return nil, err
		}
		calls, err := envInt("RADAR_TOOL_TOKENS")
		agent.ToolTokenBudget = int64(calls)
		return agent, err
	case "bedrock":
		agent, err := bedrock.NewAgent(context.Background(), model)
		if err != nil || opts.workdir == "" {
			return agent, err
		}
		// With a checkout, Claude reads the repository through tools.
		if agent.Tools, err = radar.NewRepoTools(opts.workdir); err != nil {
			return nil, err
		}
		if agent.ToolCallBudget, err = envInt("RADAR_TOOL_CALLS"); err != nil {
			return nil, err
		}
		tokens, err := envInt("RADAR_TOOL_TOKENS")
		agent.ToolTokenBudget = int64(tokens)
		return agent, err
	case "bedrock-openai":
		return bedrock.NewCodexAgent(context.Background(), model)
	case "codex":
		agent, err := bedrock.NewCodexExecAgent(context.Background(), model, opts.workdir)
		if err != nil || runtime.GOOS != "windows" {
			return agent, err
		}
		// On Windows Codex reads the checkout through Radar's tools,
		// served by this binary, instead of a shell.
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		agent.ToolsCommand = []string{exe, "repo-tools-mcp", "-root", agent.Workdir}
		return agent, nil
	case "rule-based":
		return radar.RuleBasedAgent{}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q", name)
	}
}

// envInt reads an optional positive integer setting; unset is 0.
func envInt(name string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return n, nil
}

func fetchGitHubSnapshot(repo string, number int, policy radar.PullRequestPolicy) (githubSnapshot, error) {
	return fetchSnapshot(repo, number, policy, true)
}

// fetchFindingsSnapshot reads only what the findings stage's gates use: the
// pull request, its complete files and its stack. Checks, reviews, threads
// and branch protection are the verdict's, so an error reading them must not
// stop the findings.
func fetchFindingsSnapshot(repo string, number int, policy radar.PullRequestPolicy) (githubSnapshot, error) {
	return fetchSnapshot(repo, number, policy, false)
}

func fetchSnapshot(repo string, number int, policy radar.PullRequestPolicy, verdictState bool) (githubSnapshot, error) {
	var pr githubPullRequest
	if err := ghJSON(&pr, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, number)); err != nil {
		return githubSnapshot{}, fmt.Errorf("fetching pull request: %w", err)
	}
	files, err := fetchPullRequestFiles(repo, number)
	if err != nil {
		return githubSnapshot{}, err
	}
	if len(files) != pr.ChangedFiles {
		return githubSnapshot{}, fmt.Errorf("incomplete file pagination: fetched %d of %d files", len(files), pr.ChangedFiles)
	}
	var checks checkObservation
	var reviews []githubReview
	var unresolved int
	var changesRequested bool
	if verdictState {
		if checks, err = fetchChecks(repo, pr.Head.SHA, policy); err != nil {
			return githubSnapshot{}, err
		}
		if reviews, err = fetchReviews(repo, number); err != nil {
			return githubSnapshot{}, err
		}
		if unresolved, changesRequested, err = fetchReviewState(repo, number); err != nil {
			return githubSnapshot{}, err
		}
	}
	var stack []radar.PullRequestStackEntry
	if policy.AllowStacked && !matchesBranch(pr.Base.Ref, policy.AllowedBaseBranches) {
		stack, err = fetchStack(repo, pr.Base.Ref, policy.AllowedBaseBranches)
		if err != nil {
			return githubSnapshot{}, err
		}
	}
	protectedBase := pr.Base.Ref
	if len(stack) > 0 {
		protectedBase = stack[len(stack)-1].BaseRef
	}
	keptCurrent := false
	if verdictState && policy.Mode == radar.PullRequestModeApprove {
		keptCurrent, err = approvalsKeptCurrent(repo, protectedBase, approverAppID)
		if err != nil {
			return githubSnapshot{}, fmt.Errorf("reading how the base branch keeps approvals current: %w", err)
		}
	}
	existingApproval := existingRadarApproval(reviews, pr.Head.SHA)

	labels := make([]string, 0, len(pr.Labels))
	for _, label := range pr.Labels {
		labels = append(labels, label.Name)
	}
	inputFiles := toPullRequestFiles(files)
	return githubSnapshot{
		Input: radar.PullRequestInput{
			ID: fmt.Sprintf("%s#%d", repo, number), Repository: repo, Number: number,
			Title: pr.Title, Body: pr.Body, BaseRef: pr.Base.Ref, HeadSHA: pr.Head.SHA,
			Author: pr.User.Login, AuthorType: pr.User.Type,
			Open: strings.EqualFold(pr.State, "open"), Draft: pr.Draft,
			SameRepository: strings.EqualFold(pr.Base.Repo.FullName, pr.Head.Repo.FullName),
			ChecksObserved: checks.Observed, ChecksPassing: checks.Passing,
			CheckFingerprint:  checks.Fingerprint,
			UnresolvedThreads: unresolved, ChangesRequested: changesRequested,
			ApprovalsKeptCurrent: keptCurrent, Files: inputFiles,
			Stack: stack, Labels: labels,
		},
		ExistingApproval: existingApproval,
	}, nil
}

// maxStackDepth bounds the walk down a stack of pull requests.
const maxStackDepth = 8

// fetchStack walks from base down through the open same-repository pull
// requests whose head branch each base is, until it reaches an allowed base.
// It returns nil when base is not another open pull request's head, which
// the state gate then rejects as a disallowed base.
func fetchStack(repo, base string, allowed []string) ([]radar.PullRequestStackEntry, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var stack []radar.PullRequestStackEntry
	ref := base
	for len(stack) < maxStackDepth {
		var pulls []githubPullRequestRef
		endpoint := fmt.Sprintf("repos/%s/pulls?state=open&head=%s&per_page=100", repo, url.QueryEscape(owner+":"+ref))
		if err := ghJSON(&pulls, "api", endpoint); err != nil {
			return nil, fmt.Errorf("fetching stack parent of %s: %w", ref, err)
		}
		var parents []githubPullRequestRef
		for _, p := range pulls {
			if p.Head.Ref == ref && strings.EqualFold(p.Head.Repo.FullName, repo) && strings.EqualFold(p.Base.Repo.FullName, repo) {
				parents = append(parents, p)
			}
		}
		if len(parents) != 1 {
			// None: not a stack. Several: the branch is the head of more than
			// one open pull request, so which change sits below is ambiguous.
			return nil, nil
		}
		p := parents[0]
		stack = append(stack, radar.PullRequestStackEntry{Number: p.Number, HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref})
		if matchesBranch(p.Base.Ref, allowed) {
			return stack, nil
		}
		ref = p.Base.Ref
	}
	return nil, nil
}

type githubPullRequestRef struct {
	Number int `json:"number"`
	Base   struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func matchesBranch(ref string, allowed []string) bool {
	return radar.MatchesPathGlob(ref, allowed)
}

func fetchPullRequestFiles(repo string, number int) ([]githubFile, error) {
	var pages [][]githubFile
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", repo, number)); err != nil {
		return nil, fmt.Errorf("fetching changed files: %w", err)
	}
	var files []githubFile
	for _, page := range pages {
		files = append(files, page...)
	}
	return files, nil
}

func fetchReviews(repo string, number int) ([]githubReview, error) {
	var pages [][]githubReview
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/reviews?per_page=100", repo, number)); err != nil {
		return nil, fmt.Errorf("fetching reviews: %w", err)
	}
	var reviews []githubReview
	for _, page := range pages {
		reviews = append(reviews, page...)
	}
	return reviews, nil
}

func existingRadarApproval(reviews []githubReview, head string) bool {
	latest := map[string]githubReview{}
	for _, review := range reviews {
		login := strings.ToLower(review.User.Login)
		if prior, ok := latest[login]; !ok || review.ID > prior.ID {
			latest[login] = review
		}
	}
	existingApproval := false
	for _, review := range latest {
		switch strings.ToUpper(review.State) {
		case "APPROVED":
			if strings.EqualFold(review.User.Type, "Bot") && review.CommitID == head && strings.HasPrefix(review.Body, radarApprovalPrefix) {
				existingApproval = true
			}
		}
	}
	return existingApproval
}

func fetchReviewState(repo string, number int) (int, bool, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, false, fmt.Errorf("invalid repository %q", repo)
	}
	const query = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewDecision reviewThreads(first:100,after:$cursor){nodes{isResolved}pageInfo{hasNextPage endCursor}}}}}`
	cursor := ""
	unresolved := 0
	changesRequested := false
	for {
		args := []string{"api", "graphql", "-f", "query=" + query, "-f", "owner=" + parts[0], "-f", "name=" + parts[1], "-F", "number=" + strconv.Itoa(number)}
		if cursor != "" {
			args = append(args, "-f", "cursor="+cursor)
		}
		var response struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewDecision string `json:"reviewDecision"`
						ReviewThreads  struct {
							Nodes []struct {
								IsResolved bool `json:"isResolved"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := ghJSON(&response, args...); err != nil {
			return 0, false, fmt.Errorf("fetching review state: %w", err)
		}
		if strings.EqualFold(response.Data.Repository.PullRequest.ReviewDecision, "CHANGES_REQUESTED") {
			changesRequested = true
		}
		threads := response.Data.Repository.PullRequest.ReviewThreads
		for _, thread := range threads.Nodes {
			if !thread.IsResolved {
				unresolved++
			}
		}
		if !threads.PageInfo.HasNextPage {
			return unresolved, changesRequested, nil
		}
		if threads.PageInfo.EndCursor == "" || threads.PageInfo.EndCursor == cursor {
			return 0, false, errors.New("review thread pagination did not advance")
		}
		cursor = threads.PageInfo.EndCursor
	}
}

// approverAppID is the approver App's ID (-approver-app-id), whose
// radar/approval-current status a base branch may require.
var approverAppID int64

// approvalsKeptCurrent reports whether an approval of one head can't carry
// over to code pushed after it, into branch: a ruleset or classic branch
// protection dismisses approvals on push, or a ruleset requires the
// approver's radar/approval-current status, which Radar posts on each head
// only once its own approval covers it. The status counts only
// when the rule pins it to the approver App: anyone with statuses: write
// could post an unpinned one. Most repositories protect their default
// branch only with rulesets, which the classic endpoint doesn't show.
func approvalsKeptCurrent(repo, branch string, approverAppID int64) (bool, error) {
	var pages [][]struct {
		Type       string `json:"type"`
		Parameters struct {
			DismissStaleReviewsOnPush bool `json:"dismiss_stale_reviews_on_push"`
			RequiredStatusChecks      []struct {
				Context       string `json:"context"`
				IntegrationID int64  `json:"integration_id"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	rules := fmt.Sprintf("repos/%s/rules/branches/%s?per_page=100", repo, url.PathEscape(branch))
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", rules); err != nil {
		return false, fmt.Errorf("reading branch rules: %w", err)
	}
	for _, page := range pages {
		for _, rule := range page {
			switch rule.Type {
			case "pull_request":
				if rule.Parameters.DismissStaleReviewsOnPush {
					return true, nil
				}
			case "required_status_checks":
				for _, check := range rule.Parameters.RequiredStatusChecks {
					if approverAppID > 0 && check.Context == radar.ApprovalCurrentContext && check.IntegrationID == approverAppID {
						return true, nil
					}
				}
			}
		}
	}
	var protection struct {
		RequiredPullRequestReviews *struct {
			DismissStaleReviews bool `json:"dismiss_stale_reviews"`
		} `json:"required_pull_request_reviews"`
	}
	endpoint := fmt.Sprintf("repos/%s/branches/%s/protection", repo, url.PathEscape(branch))
	if err := ghJSON(&protection, "api", endpoint); err != nil {
		// A branch without classic protection answers 404, which gh reports
		// only as an error. Either way nothing is known to keep approvals
		// current, so the state gate holds the approval.
		return false, nil
	}
	return protection.RequiredPullRequestReviews != nil && protection.RequiredPullRequestReviews.DismissStaleReviews, nil
}

type githubCheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

type githubStatus struct {
	ID      int64  `json:"id"`
	Context string `json:"context"`
	State   string `json:"state"`
}

func fetchChecks(repo, sha string, policy radar.PullRequestPolicy) (checkObservation, error) {
	var checkPages []struct {
		CheckRuns []githubCheckRun `json:"check_runs"`
	}
	if err := ghJSON(&checkPages, "api", "--paginate", "--slurp", "-H", "Accept: application/vnd.github+json", fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, sha)); err != nil {
		return checkObservation{}, fmt.Errorf("fetching check runs: %w", err)
	}
	var statusPages [][]githubStatus
	if err := ghJSON(&statusPages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/commits/%s/statuses?per_page=100", repo, sha)); err != nil {
		return checkObservation{}, fmt.Errorf("fetching commit statuses: %w", err)
	}
	var runs []githubCheckRun
	for _, page := range checkPages {
		runs = append(runs, page.CheckRuns...)
	}
	var statuses []githubStatus
	for _, page := range statusPages {
		statuses = append(statuses, page...)
	}
	return evaluateChecks(runs, statuses, policy), nil
}

func evaluateChecks(runs []githubCheckRun, statuses []githubStatus, policy radar.PullRequestPolicy) checkObservation {
	ignored := map[string]bool{}
	for _, name := range policy.IgnoredChecks {
		ignored[name] = true
	}
	latestRuns := map[string]githubCheckRun{}
	for _, run := range runs {
		key := run.App.Slug + "/" + run.Name
		if ignored[run.Name] || ignored[key] {
			continue
		}
		if prior, ok := latestRuns[key]; !ok || run.ID > prior.ID {
			latestRuns[key] = run
		}
	}
	latestStatuses := map[string]githubStatus{}
	for _, status := range statuses {
		if ignored[status.Context] {
			continue
		}
		if prior, ok := latestStatuses[status.Context]; !ok || status.ID > prior.ID {
			latestStatuses[status.Context] = status
		}
	}
	states := make([]string, 0, len(latestRuns)+len(latestStatuses))
	passing := true
	for key, run := range latestRuns {
		states = append(states, "check:"+key+":"+strings.ToLower(run.Status)+":"+strings.ToLower(run.Conclusion))
		if !strings.EqualFold(run.Status, "completed") || !allowedConclusion(run.Conclusion, policy.AllowSkippedChecks) {
			passing = false
		}
	}
	for context, status := range latestStatuses {
		states = append(states, "status:"+context+":"+strings.ToLower(status.State))
		if !strings.EqualFold(status.State, "success") {
			passing = false
		}
	}
	sort.Strings(states)
	sum := sha256.Sum256([]byte(strings.Join(states, "\n")))
	return checkObservation{
		Observed:    len(states) > 0,
		Passing:     len(states) > 0 && passing,
		Fingerprint: hex.EncodeToString(sum[:]),
	}
}

func allowedConclusion(conclusion string, allowSkipped bool) bool {
	if strings.EqualFold(conclusion, "success") {
		return true
	}
	return allowSkipped && (strings.EqualFold(conclusion, "neutral") || strings.EqualFold(conclusion, "skipped"))
}

func patchLineCountsMatch(patch string, additions, deletions int) bool {
	gotAdditions := 0
	gotDeletions := 0
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			gotAdditions++
		}
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			gotDeletions++
		}
	}
	return gotAdditions == additions && gotDeletions == deletions
}

func validateApprovalSnapshot(policy radar.PullRequestPolicy, reviewed, current radar.PullRequestInput, expectedHead string) error {
	switch {
	case policy.HumanReviewRequested(current):
		return errors.New("the human-review label was added")
	case current.HeadSHA != reviewed.HeadSHA || current.HeadSHA != expectedHead:
		return errors.New("head SHA changed after review")
	case current.BaseRef != reviewed.BaseRef || len(current.Stack) > 0:
		return errors.New("base branch changed after review, or the pull request is stacked")
	case current.CheckFingerprint != reviewed.CheckFingerprint:
		return errors.New("check state changed after review")
	case !current.ChecksObserved || !current.ChecksPassing:
		return errors.New("checks are no longer passing")
	case !current.Open || current.Draft || !current.SameRepository:
		return errors.New("pull request is no longer eligible")
	case current.UnresolvedThreads > 0:
		return errors.New("an unresolved review thread appeared")
	case current.ChangesRequested:
		return errors.New("changes are requested")
	case !current.ApprovalsKeptCurrent:
		return errors.New("the base branch no longer keeps approvals current")
	}
	return nil
}

func approvePullRequest(repo string, number int, head string, decision radar.PullRequestReview) error {
	body := fmt.Sprintf("%s%d approved this exact head after stable checks, deterministic eligibility, calibrated risk, and automated review.", radarApprovalPrefix, decision.PolicyVersion)
	_, err := runGH("api", "-X", "POST", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number),
		"-f", "event=APPROVE", "-f", "commit_id="+head, "-f", "body="+body)
	return err
}

func ghJSON(target any, args ...string) error {
	out, err := runGH(args...)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := dec.Decode(target); err != nil {
		return err
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
