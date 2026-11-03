package radar

import (
	"strings"
	"testing"
)

func stage(r PullRequestReview, name string) (StageResult, bool) {
	for _, s := range r.Stages {
		if s.Name == name {
			return s, true
		}
	}
	return StageResult{}, false
}

func TestHumanReviewLabelRoutesToHuman(t *testing.T) {
	agent := &recordingAgent{result: structuralVerdict()}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeApprove), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(2)
	in.Labels = []string{"area:docs"}
	if got := reviewer.Review(in); got.Action != PullRequestApprove {
		t.Fatalf("without the label the change must approve, got %s; stages %+v", got.Action, got.Stages)
	}

	in.Labels = append(in.Labels, DefaultHumanReviewLabel)
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman || got.Eligible {
		t.Fatalf("the label must route to a human, got %s eligible=%t", got.Action, got.Eligible)
	}
	if !strings.HasPrefix(got.VerdictReason, "pr.opt-out: ") || !strings.Contains(got.VerdictReason, DefaultHumanReviewLabel) {
		t.Fatalf("verdict reason = %q", got.VerdictReason)
	}
	// The opt-out stops approval, not the review: the findings still post.
	if len(got.Findings) == 0 {
		t.Fatal("the reviewers must still run under the opt-out")
	}
}

func TestHumanReviewLabelShadowRecordsRoute(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(2)
	in.Labels = []string{DefaultHumanReviewLabel}
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman {
		t.Fatalf("shadow must record the route, got %s", got.Action)
	}
	if s, ok := stage(got, "pr.opt-out"); !ok || s.Passed {
		t.Fatalf("pr.opt-out stage = %+v, present=%t", s, ok)
	}
}

func TestHumanReviewLabelIsNeverAPolicyCandidate(t *testing.T) {
	// Under allow-rules a clean review outside the rules is a candidate;
	// with the label it must stay an ordinary route to a human.
	reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), fixedScorer(0), &countingAgent{result: safeAgentResult()})
	if err != nil {
		t.Fatal(err)
	}
	in := safePullRequestInput()
	in.Files[0].Path = "src/main.md.go"
	reviewer.agents[0].(*countingAgent).result.ReviewedFiles = []string{in.Files[0].Path}
	if got := reviewer.Review(in); got.Action != PullRequestPolicyCandidate {
		t.Fatalf("precondition: got %s", got.Action)
	}
	in.Labels = []string{DefaultHumanReviewLabel}
	if got := reviewer.Review(in); got.Action != PullRequestRouteToHuman {
		t.Fatalf("got %s", got.Action)
	}
}

func TestHumanReviewLabelAddedAfterFindings(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeApprove), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(2)
	findings := reviewer.Findings(in)
	if s, ok := stage(findings, "pr.opt-out"); !ok || !s.Passed {
		t.Fatalf("findings without the label: pr.opt-out = %+v", s)
	}
	// The verdict reads the labels again, so one added since counts.
	in.Labels = []string{DefaultHumanReviewLabel}
	if got := reviewer.Review(in); got.Action != PullRequestRouteToHuman {
		t.Fatalf("a label added after the findings run must route, got %s", got.Action)
	}
	// And one removed since no longer does.
	in.Labels = nil
	if got := reviewer.Review(in); got.Action != PullRequestApprove {
		t.Fatalf("a removed label must not route, got %s", got.Action)
	}
}

func TestHumanReviewLabelOnFindingsStage(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(2)
	in.Labels = []string{DefaultHumanReviewLabel}
	got := reviewer.Findings(in)
	if got.Action != PullRequestFindingsOnly || len(got.Findings) == 0 {
		t.Fatalf("findings must still run, got %s with %d findings", got.Action, len(got.Findings))
	}
	if !strings.HasPrefix(got.VerdictReason, "pr.opt-out: ") {
		t.Fatalf("verdict reason = %q", got.VerdictReason)
	}
}

func TestCustomHumanReviewLabel(t *testing.T) {
	policy := findingsPolicy(PullRequestModeApprove)
	policy.HumanReviewLabel = "needs-human"
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(2)
	in.Labels = []string{DefaultHumanReviewLabel}
	if got := reviewer.Review(in); got.Action != PullRequestApprove {
		t.Fatalf("a custom label replaces the default, got %s", got.Action)
	}
	in.Labels = []string{"Needs-Human"}
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman || !strings.Contains(got.VerdictReason, "needs-human") {
		t.Fatalf("label names match case-insensitively, as on GitHub: got %s %q", got.Action, got.VerdictReason)
	}
}

func TestHumanReviewLabelOnStackedPullRequest(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.AllowStacked = true
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	in := stackedInput()
	in.Labels = []string{DefaultHumanReviewLabel}
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	if _, ok := stage(got, "pr.stack"); ok {
		t.Fatal("an opted-out stacked PR must not report the stack as passing")
	}
}

func TestHumanReviewLabelValidation(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.HumanReviewLabel = " radar:human-review"
	if err := policy.Validate(); err == nil {
		t.Fatal("a label with surrounding whitespace never matches and must be rejected")
	}
}
