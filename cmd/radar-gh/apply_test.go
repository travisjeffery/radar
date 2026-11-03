package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	radar "github.com/travisjeffery/radar"
)

const approver = "radar-approver[bot]"

func review(id int64, login, state, head, body string) githubReview {
	r := githubReview{ID: id, State: state, Body: body, CommitID: head}
	r.User.Login, r.User.Type = login, "Bot"
	if !strings.HasSuffix(login, "[bot]") {
		r.User.Type = "User"
	}
	return r
}

func TestRadarApprovals(t *testing.T) {
	reviews := []githubReview{
		review(1, approver, "APPROVED", "aaa", radarApprovalPrefix+"6 approved"),
		review(2, approver, "DISMISSED", "bbb", radarApprovalPrefix+"6 approved"),
		review(3, "alice", "APPROVED", "aaa", "lgtm"),
		review(4, "oscapprovebot[bot]", "APPROVED", "ccc", radarApprovalPrefix+"6 approved"),
		review(5, approver, "APPROVED", "ddd", "not Radar's body"),
		review(6, approver, "APPROVED", "eee", radarApprovalPrefix+"6 approved"),
		review(7, approver, "DISMISSED", "eee", radarApprovalPrefix+"6 approved"),
	}
	got := radarApprovals(reviews, approver)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("radarApprovals = %+v, want only review 1", got)
	}
	if !approvalCovers(got, "aaa") || approvalCovers(got, "fff") {
		t.Fatal("approvalCovers")
	}
	if stale := staleApprovals(got, "fff"); len(stale) != 1 || stale[0].ID != 1 {
		t.Fatalf("staleApprovals = %+v", stale)
	}
}

// fakeGitHub answers the reviews endpoint and records every write.
func fakeGitHub(t *testing.T, reviews []githubReview) *[]string {
	t.Helper()
	var writes []string
	prev := runGH
	t.Cleanup(func() { runGH = prev })
	runGH = func(args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "/reviews?per_page=100"):
			body, _ := json.Marshal([][]githubReview{reviews})
			return body, nil
		case strings.Contains(joined, "-X PUT") || strings.Contains(joined, "-X POST"):
			writes = append(writes, joined)
			return []byte("{}"), nil
		}
		t.Fatalf("unexpected gh call %v", args)
		return nil, nil
	}
	return &writes
}

func stubSnapshot(t *testing.T, in radar.PullRequestInput) {
	t.Helper()
	prev := applySnapshot
	t.Cleanup(func() { applySnapshot = prev })
	applySnapshot = func(string, int, radar.PullRequestPolicy) (githubSnapshot, error) {
		return githubSnapshot{Input: in}, nil
	}
}

func currentInput(head string) radar.PullRequestInput {
	return radar.PullRequestInput{
		HeadSHA: head, BaseRef: "main", CheckFingerprint: "fp", Open: true, SameRepository: true,
		ChecksObserved: true, ChecksPassing: true, ApprovalsKeptCurrent: true,
	}
}

func approveRequest(head string, action radar.PullRequestAction) approvalRequest {
	return approvalRequest{Repo: "Acme/backend", Number: 7, Head: head, Base: "main", CheckFingerprint: "fp",
		Action: action, PolicyVersion: 6, VerdictReason: "pr.review-agent: P1 finding"}
}

func countWrites(writes []string, part string) int {
	n := 0
	for _, w := range writes {
		if strings.Contains(w, part) {
			n++
		}
	}
	return n
}

func TestApplyDecisionApproves(t *testing.T) {
	policy := radar.PullRequestPolicy{Mode: radar.PullRequestModeApprove}
	stubSnapshot(t, currentInput("new"))
	writes := fakeGitHub(t, []githubReview{review(1, approver, "APPROVED", "old", radarApprovalPrefix+"6 approved")})
	if err := applyDecision(policy, approveRequest("new", radar.PullRequestApprove), approver); err != nil {
		t.Fatal(err)
	}
	if countWrites(*writes, "event=APPROVE") != 1 || countWrites(*writes, "dismissals") != 0 ||
		countWrites(*writes, "statuses/new") != 1 || countWrites(*writes, "context="+radar.ApprovalCurrentContext) != 1 {
		t.Fatalf("writes = %q", *writes)
	}
}

func TestApplyDecisionDoesNotApproveTwice(t *testing.T) {
	policy := radar.PullRequestPolicy{Mode: radar.PullRequestModeApprove}
	stubSnapshot(t, currentInput("new"))
	writes := fakeGitHub(t, []githubReview{review(1, approver, "APPROVED", "new", radarApprovalPrefix+"6 approved")})
	if err := applyDecision(policy, approveRequest("new", radar.PullRequestApprove), approver); err != nil {
		t.Fatal(err)
	}
	if countWrites(*writes, "event=APPROVE") != 0 || countWrites(*writes, "statuses/new") != 1 {
		t.Fatalf("writes = %q", *writes)
	}
}

func TestApplyDecisionDismissesOnlyRadar(t *testing.T) {
	policy := radar.PullRequestPolicy{Mode: radar.PullRequestModeApprove}
	stubSnapshot(t, currentInput("new"))
	writes := fakeGitHub(t, []githubReview{
		review(1, approver, "APPROVED", "old", radarApprovalPrefix+"6 approved"),
		review(2, "alice", "APPROVED", "old", "lgtm"),
	})
	if err := applyDecision(policy, approveRequest("new", radar.PullRequestRouteToHuman), approver); err != nil {
		t.Fatal(err)
	}
	if countWrites(*writes, "reviews/1/dismissals") != 1 || countWrites(*writes, "reviews/2/") != 0 ||
		countWrites(*writes, "event=APPROVE") != 0 || countWrites(*writes, "statuses/new") != 1 {
		t.Fatalf("writes = %q", *writes)
	}
}

func TestApplyDecisionRevalidates(t *testing.T) {
	policy := radar.PullRequestPolicy{Mode: radar.PullRequestModeApprove}
	in := currentInput("new")
	in.Labels = []string{"radar:human-review"}
	stubSnapshot(t, in)
	writes := fakeGitHub(t, []githubReview{review(1, approver, "APPROVED", "old", radarApprovalPrefix+"6 approved")})
	if err := applyDecision(policy, approveRequest("new", radar.PullRequestApprove), approver); err != nil {
		t.Fatal(err)
	}
	if countWrites(*writes, "event=APPROVE") != 0 || countWrites(*writes, "reviews/1/dismissals") != 1 || countWrites(*writes, "statuses/new") != 1 {
		t.Fatalf("a label added after the verdict must stop the approval; writes = %q", *writes)
	}
}

func TestApplyDecisionLeavesAMovedHead(t *testing.T) {
	policy := radar.PullRequestPolicy{Mode: radar.PullRequestModeApprove}
	stubSnapshot(t, currentInput("newer"))
	writes := fakeGitHub(t, nil)
	if err := applyDecision(policy, approveRequest("new", radar.PullRequestApprove), approver); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 0 {
		t.Fatalf("writes = %q, want none", *writes)
	}
}

func TestApprovalStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reviews []githubReview
		posts   int
	}{
		{name: "never approved", reviews: []githubReview{review(2, "alice", "APPROVED", "old", "lgtm")}, posts: 1},
		{name: "approved this head", reviews: []githubReview{review(1, approver, "APPROVED", "head", radarApprovalPrefix+"6 approved")}, posts: 1},
		{name: "approval dismissed", reviews: []githubReview{review(1, approver, "DISMISSED", "old", radarApprovalPrefix+"6 approved")}, posts: 1},
		{name: "approved an earlier head", reviews: []githubReview{review(1, approver, "APPROVED", "old", radarApprovalPrefix+"6 approved")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := fakeGitHub(t, tc.reviews)
			if code := runApprovalStatus([]string{"-repo", "Acme/backend", "-pr", "7", "-head", "head", "-approver-login", approver}); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := countWrites(*writes, "statuses/head"); got != tc.posts {
				t.Fatalf("status posts = %d, want %d (%s)", got, tc.posts, fmt.Sprint(*writes))
			}
		})
	}
}
