package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/travisjeffery/radar"
)

type githubPullRequest struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	State        string `json:"state"`
	Draft        bool   `json:"draft"`
	ChangedFiles int    `json:"changed_files"`
	User         struct {
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
	agentName := flags.String("agent", "openai", "review agent: openai, anthropic, or rule-based")
	expectedHead := flags.String("expected-head", "", "event head SHA; required in approval mode")
	settle := flags.Duration("settle", 10*time.Second, "time between check observations")
	apply := flags.Bool("apply", false, "allow an APPROVE review when policy mode is approve")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *repo == "" || *prNumber <= 0 || *policyPath == "" || *settle < 0 {
		fmt.Fprintln(os.Stderr, "radar-gh review: -repo, -pr, and -policy are required; -settle cannot be negative")
		return 2
	}

	policy, err := loadPullRequestPolicy(*policyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	if policy.Mode == radar.PullRequestModeApprove {
		if !*apply {
			fmt.Fprintln(os.Stderr, "radar-gh review: approval mode requires the explicit -apply flag")
			return 2
		}
		if *expectedHead == "" {
			fmt.Fprintln(os.Stderr, "radar-gh review: approval mode requires -expected-head")
			return 2
		}
	}

	agent, err := newReviewAgent(*agentName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	reviewer, err := radar.NewPullRequestReviewer(policy, radar.HeuristicScorer{}, agent)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}

	first, err := fetchGitHubSnapshot(*repo, *prNumber, policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	if *expectedHead != "" && first.Input.HeadSHA != *expectedHead {
		fmt.Fprintf(os.Stderr, "radar-gh review: expected head %s, found %s\n", *expectedHead, first.Input.HeadSHA)
		return 1
	}
	if *settle > 0 {
		time.Sleep(*settle)
	}
	second, err := fetchGitHubSnapshot(*repo, *prNumber, policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	second.Input.ChecksStable = first.Input.HeadSHA == second.Input.HeadSHA &&
		first.Input.CheckFingerprint == second.Input.CheckFingerprint
	if *expectedHead != "" && second.Input.HeadSHA != *expectedHead {
		fmt.Fprintf(os.Stderr, "radar-gh review: head changed from expected %s to %s\n", *expectedHead, second.Input.HeadSHA)
		return 1
	}

	decision := reviewer.Review(second.Input)
	if decision.Action == radar.PullRequestApprove {
		if second.ExistingApproval {
			if err := writeJSON(os.Stdout, decision); err != nil {
				fmt.Fprintln(os.Stderr, "radar-gh review:", err)
				return 1
			}
			return 0
		}
		current, err := fetchGitHubSnapshot(*repo, *prNumber, policy)
		if err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh review: pre-approval revalidation:", err)
			return 1
		}
		if err := validateApprovalSnapshot(second.Input, current.Input, *expectedHead); err != nil {
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
	if err := writeJSON(os.Stdout, decision); err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh review:", err)
		return 1
	}
	return 0
}

func loadPullRequestPolicy(path string) (radar.PullRequestPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return radar.PullRequestPolicy{}, fmt.Errorf("opening policy: %w", err)
	}
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

func newReviewAgent(name string) (radar.ReviewAgent, error) {
	switch name {
	case "openai":
		return radar.NewOpenAIAgent()
	case "anthropic":
		return radar.NewLLMAgent()
	case "rule-based":
		return radar.RuleBasedAgent{}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q", name)
	}
}

func fetchGitHubSnapshot(repo string, number int, policy radar.PullRequestPolicy) (githubSnapshot, error) {
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
	checks, err := fetchChecks(repo, pr.Head.SHA, policy)
	if err != nil {
		return githubSnapshot{}, err
	}
	reviews, err := fetchReviews(repo, number)
	if err != nil {
		return githubSnapshot{}, err
	}
	unresolved, changesRequested, err := fetchReviewState(repo, number)
	if err != nil {
		return githubSnapshot{}, err
	}
	staleDismissed := false
	if policy.Mode == radar.PullRequestModeApprove {
		staleDismissed, err = staleApprovalsDismissed(repo, pr.Base.Ref)
		if err != nil {
			return githubSnapshot{}, fmt.Errorf("reading stale-review protection: %w", err)
		}
	}
	existingApproval := existingRadarApproval(reviews, pr.Head.SHA)

	inputFiles := make([]radar.PullRequestFile, 0, len(files))
	for _, file := range files {
		patch := ""
		if file.Patch != nil {
			patch = *file.Patch
		}
		inputFiles = append(inputFiles, radar.PullRequestFile{
			Path: file.Filename, PreviousPath: file.PreviousFilename, Status: file.Status,
			Additions: file.Additions, Deletions: file.Deletions, Patch: patch,
			ContentComplete: file.Patch != nil && patchLineCountsMatch(patch, file.Additions, file.Deletions),
		})
	}
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
			StaleApprovalsDismissed: staleDismissed, Files: inputFiles,
		},
		ExistingApproval: existingApproval,
	}, nil
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

func staleApprovalsDismissed(repo, branch string) (bool, error) {
	var protection struct {
		RequiredPullRequestReviews *struct {
			DismissStaleReviews bool `json:"dismiss_stale_reviews"`
		} `json:"required_pull_request_reviews"`
	}
	endpoint := fmt.Sprintf("repos/%s/branches/%s/protection", repo, url.PathEscape(branch))
	if err := ghJSON(&protection, "api", endpoint); err != nil {
		return false, err
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

func validateApprovalSnapshot(reviewed, current radar.PullRequestInput, expectedHead string) error {
	switch {
	case current.HeadSHA != reviewed.HeadSHA || current.HeadSHA != expectedHead:
		return errors.New("head SHA changed after review")
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
	case !current.StaleApprovalsDismissed:
		return errors.New("stale approvals are not dismissed")
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
