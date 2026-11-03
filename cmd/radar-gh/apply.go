package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	radar "github.com/travisjeffery/radar"
)

// Radar keeps its own approval current. The verdict stage decides
// in the job that runs the reviewers, and writes an approvalRequest. A
// separate job, the only one that can read the approver App's key (an
// environment only the default branch can deploy), runs `radar-gh apply` on
// it: re-check the pull request, approve the reviewed head or dismiss
// Radar's own earlier approvals, and post radar/approval-current on the head.
// `radar-gh approval-status` posts the status early on a head where Radar
// holds no approval of an earlier head, so a pull request Radar never
// approved isn't held for its review. Human approvals are never dismissed.

// approvalRequest is what the verdict decided about one head, passed to the
// apply job.
type approvalRequest struct {
	Repo             string                  `json:"repo"`
	Number           int                     `json:"number"`
	Head             string                  `json:"head"`
	Base             string                  `json:"base"`
	CheckFingerprint string                  `json:"check_fingerprint"`
	Action           radar.PullRequestAction `json:"action"`
	PolicyVersion    int                     `json:"policy_version"`
	VerdictReason    string                  `json:"verdict_reason"`
}

func newApprovalRequest(repo string, number int, reviewed radar.PullRequestInput, decision radar.PullRequestReview) approvalRequest {
	return approvalRequest{
		Repo: repo, Number: number, Head: reviewed.HeadSHA, Base: reviewed.BaseRef,
		CheckFingerprint: reviewed.CheckFingerprint, Action: decision.Action,
		PolicyVersion: decision.PolicyVersion, VerdictReason: decision.VerdictReason,
	}
}

// applySnapshot reads the pull request as the verdict does; a variable so
// tests can stand in for GitHub.
var applySnapshot = fetchGitHubSnapshot

func runApply(args []string) int {
	flags := flag.NewFlagSet("radar-gh apply", flag.ContinueOnError)
	repo := flags.String("repo", "", "GitHub owner/repository (required)")
	prNumber := flags.Int("pr", 0, "pull request number (required)")
	policyPath := flags.String("policy", "", "the policy the verdict used (required; its mode must be approve)")
	requestPath := flags.String("request", "", "the verdict's approval request (JSON, required)")
	flags.Int64Var(&approverAppID, "approver-app-id", 0, "the approver App's ID (required)")
	approverLogin := flags.String("approver-login", "", "the approver App's bot login, <slug>[bot] (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *repo == "" || *prNumber <= 0 || *policyPath == "" || *requestPath == "" || approverAppID <= 0 || *approverLogin == "" {
		fmt.Fprintln(os.Stderr, "radar-gh apply: -repo, -pr, -policy, -request, -approver-app-id and -approver-login are required")
		return 2
	}
	policy, err := loadPullRequestPolicy(*policyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh apply:", err)
		return 1
	}
	if policy.Mode != radar.PullRequestModeApprove {
		fmt.Fprintln(os.Stderr, "radar-gh apply: the policy's mode is not approve")
		return 2
	}
	var req approvalRequest
	data, err := os.ReadFile(*requestPath)
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh apply: reading the approval request:", err)
		return 1
	}
	if req.Repo != *repo || req.Number != *prNumber || req.Head == "" {
		fmt.Fprintln(os.Stderr, "radar-gh apply: the approval request is for another pull request")
		return 1
	}
	if err := applyDecision(policy, req, *approverLogin); err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh apply:", err)
		return 1
	}
	return 0
}

// applyDecision approves req.Head when the verdict approved it and the pull
// request is still as reviewed; otherwise it dismisses Radar's approvals.
// Either way no Radar approval is left that doesn't cover the head, so it
// posts radar/approval-current there. A head that moved is left to the
// verdict of the new head.
func applyDecision(policy radar.PullRequestPolicy, req approvalRequest, approverLogin string) error {
	current, err := applySnapshot(req.Repo, req.Number, policy)
	if err != nil {
		return err
	}
	if current.Input.HeadSHA != req.Head {
		fmt.Fprintf(os.Stderr, "radar-gh apply: head moved from %s to %s; its own verdict decides\n", req.Head, current.Input.HeadSHA)
		return nil
	}
	reviews, err := fetchReviews(req.Repo, req.Number)
	if err != nil {
		return err
	}
	approve := req.Action == radar.PullRequestApprove
	if approve {
		reviewed := radar.PullRequestInput{HeadSHA: req.Head, BaseRef: req.Base, CheckFingerprint: req.CheckFingerprint}
		if err := validateApprovalSnapshot(policy, reviewed, current.Input, req.Head); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh apply: not approving:", err)
			approve = false
		}
	}
	approvals := radarApprovals(reviews, approverLogin)
	if approve {
		if !approvalCovers(approvals, req.Head) {
			decision := radar.PullRequestReview{PolicyVersion: req.PolicyVersion}
			if err := approvePullRequest(req.Repo, req.Number, req.Head, decision); err != nil {
				return fmt.Errorf("approving: %w", err)
			}
		}
		return postApprovalCurrent(req.Repo, req.Head, "Radar approved this head.")
	}
	for _, r := range approvals {
		msg := "Radar no longer approves this pull request at " + shortSHA(req.Head) + ": " + oneLine(req.VerdictReason)
		if err := dismissReview(req.Repo, req.Number, r.ID, msg); err != nil {
			return fmt.Errorf("dismissing Radar's review %d: %w", r.ID, err)
		}
	}
	return postApprovalCurrent(req.Repo, req.Head, "Radar holds no approval of an earlier head.")
}

func runApprovalStatus(args []string) int {
	flags := flag.NewFlagSet("radar-gh approval-status", flag.ContinueOnError)
	repo := flags.String("repo", "", "GitHub owner/repository (required)")
	prNumber := flags.Int("pr", 0, "pull request number (required)")
	head := flags.String("head", "", "the head SHA to post on (required)")
	approverLogin := flags.String("approver-login", "", "the approver App's bot login, <slug>[bot] (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *repo == "" || *prNumber <= 0 || *head == "" || *approverLogin == "" {
		fmt.Fprintln(os.Stderr, "radar-gh approval-status: -repo, -pr, -head and -approver-login are required")
		return 2
	}
	reviews, err := fetchReviews(*repo, *prNumber)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh approval-status:", err)
		return 1
	}
	if stale := staleApprovals(radarApprovals(reviews, *approverLogin), *head); len(stale) > 0 {
		fmt.Fprintf(os.Stderr, "radar-gh approval-status: Radar approved an earlier head (review %d); the verdict on %s decides\n", stale[0].ID, shortSHA(*head))
		return 0
	}
	if err := postApprovalCurrent(*repo, *head, "Radar holds no approval of an earlier head."); err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh approval-status:", err)
		return 1
	}
	return 0
}

// radarApprovals returns the approver's standing approvals: its latest review
// on each head that is APPROVED with Radar's body. A dismissed review's state
// is DISMISSED, so it never counts.
func radarApprovals(reviews []githubReview, approverLogin string) []githubReview {
	latest := map[string]githubReview{}
	for _, r := range reviews {
		if !strings.EqualFold(r.User.Login, approverLogin) {
			continue
		}
		if prior, ok := latest[r.CommitID]; !ok || r.ID > prior.ID {
			latest[r.CommitID] = r
		}
	}
	var out []githubReview
	for _, r := range latest {
		if strings.EqualFold(r.State, "APPROVED") && strings.HasPrefix(r.Body, radarApprovalPrefix) {
			out = append(out, r)
		}
	}
	return out
}

func approvalCovers(approvals []githubReview, head string) bool {
	for _, r := range approvals {
		if r.CommitID == head {
			return true
		}
	}
	return false
}

// staleApprovals are the standing approvals of heads other than head: GitHub
// still counts them toward a required approval.
func staleApprovals(approvals []githubReview, head string) []githubReview {
	var out []githubReview
	for _, r := range approvals {
		if r.CommitID != head {
			out = append(out, r)
		}
	}
	return out
}

func dismissReview(repo string, number int, id int64, message string) error {
	_, err := runGH("api", "-X", "PUT", fmt.Sprintf("repos/%s/pulls/%d/reviews/%d/dismissals", repo, number, id),
		"-f", "message="+message, "-f", "event=DISMISS")
	return err
}

func postApprovalCurrent(repo, head, description string) error {
	if head == "" {
		return errors.New("no head to post " + radar.ApprovalCurrentContext + " on")
	}
	_, err := runGH("api", "-X", "POST", fmt.Sprintf("repos/%s/statuses/%s", repo, head),
		"-f", "state=success", "-f", "context="+radar.ApprovalCurrentContext, "-f", "description="+description)
	return err
}
