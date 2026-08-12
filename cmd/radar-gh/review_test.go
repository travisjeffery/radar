package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

func TestExamplePolicyLoadsInShadowMode(t *testing.T) {
	policy, err := loadPullRequestPolicy(filepath.Join("..", "..", "examples", "github-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != radar.PullRequestModeShadow {
		t.Fatalf("example mode = %q, want shadow", policy.Mode)
	}
}

func TestEvaluateChecks(t *testing.T) {
	policy := radar.PullRequestPolicy{IgnoredChecks: []string{"github-actions/review"}}
	runs := []githubCheckRun{
		{ID: 1, Name: "test", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "test", Status: "completed", Conclusion: "success"},
		{ID: 3, Name: "review", Status: "in_progress"},
	}
	runs[0].App.Slug = "github-actions"
	runs[1].App.Slug = "github-actions"
	runs[2].App.Slug = "github-actions"
	got := evaluateChecks(runs, nil, policy)
	if !got.Observed || !got.Passing || got.Fingerprint == "" {
		t.Fatalf("got %+v", got)
	}

	policy.IgnoredChecks = nil
	got = evaluateChecks(runs, nil, policy)
	if got.Passing {
		t.Fatal("in-progress self check must fail unless explicitly ignored")
	}

	got = evaluateChecks(nil, nil, policy)
	if got.Observed || got.Passing {
		t.Fatal("no checks must fail closed")
	}
}

func TestEvaluateChecksSkippedIsExplicit(t *testing.T) {
	run := githubCheckRun{ID: 1, Name: "optional", Status: "completed", Conclusion: "skipped"}
	run.App.Slug = "github-actions"
	policy := radar.PullRequestPolicy{}
	if evaluateChecks([]githubCheckRun{run}, nil, policy).Passing {
		t.Fatal("skipped check must fail by default")
	}
	policy.AllowSkippedChecks = true
	if !evaluateChecks([]githubCheckRun{run}, nil, policy).Passing {
		t.Fatal("skipped check should pass only when explicitly allowed")
	}
}

func TestPatchLineCountsMatch(t *testing.T) {
	patch := "@@ -1,2 +1,2 @@\n-old\n context\n+new"
	if !patchLineCountsMatch(patch, 1, 1) {
		t.Fatal("complete patch should match")
	}
	if patchLineCountsMatch(patch, 2, 1) {
		t.Fatal("truncated patch must not match provider totals")
	}
}

func TestExistingRadarApproval(t *testing.T) {
	changes := githubReview{ID: 1, State: "CHANGES_REQUESTED"}
	changes.User.Login = "reviewer"
	approved := githubReview{ID: 2, State: "APPROVED"}
	approved.User.Login = "reviewer"
	approved.User.Type = "Bot"
	approved.CommitID = strings.Repeat("a", 40)
	approved.Body = radarApprovalPrefix + "1 approved this exact head"
	existing := existingRadarApproval([]githubReview{changes, approved}, approved.CommitID)
	if !existing {
		t.Fatal("current Radar approval was not found")
	}
	if existingRadarApproval([]githubReview{approved}, strings.Repeat("b", 40)) {
		t.Fatal("approval for an old head must not be treated as current")
	}
}

func TestValidateApprovalSnapshot(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := radar.PullRequestInput{
		HeadSHA: head, CheckFingerprint: "stable", ChecksObserved: true, ChecksPassing: true,
		Open: true, SameRepository: true, StaleApprovalsDismissed: true,
	}
	if err := validateApprovalSnapshot(base, base, head); err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.CheckFingerprint = "changed"
	if err := validateApprovalSnapshot(base, changed, head); err == nil {
		t.Fatal("changed checks must block approval")
	}
	changed = base
	changed.StaleApprovalsDismissed = false
	if err := validateApprovalSnapshot(base, changed, head); err == nil {
		t.Fatal("stale approval protection must block approval")
	}
}
