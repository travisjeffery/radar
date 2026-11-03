package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

func replayPolicy() radar.PullRequestPolicy {
	return radar.PullRequestPolicy{
		Version: 4, Mode: radar.PullRequestModeShadow, AllowedBaseBranches: []string{"main"},
		AllowRules: []radar.PullRequestPathRule{{
			Name: "small-change", Include: []string{"**"}, Statuses: []string{"modified"},
			MaxFiles: 10, MaxChangedLines: 150,
		}},
		MaxFiles: 20, MaxChangedLines: 800, MaxRiskPercentile: 60,
		CalibrationSample: []float64{0, 1, 2, 3}, MinReviewConfidence: 8,
		AuthorTrust: &radar.PullRequestAuthorTrust{Tiers: []radar.PullRequestTrustTier{
			{Tier: 2, MinReviewConfidence: 7},
		}},
	}
}

func replayRecord(t *testing.T, policy radar.PullRequestPolicy, author, body string, confidence int, outcome string) string {
	t.Helper()
	in := radar.PullRequestInput{
		ID: "o/r#1", Repository: "o/r", Number: 1, BaseRef: "main", HeadSHA: strings.Repeat("a", 40),
		Author: author, AuthorType: "User", Body: body, Open: true, SameRepository: true,
		ChecksObserved: true, ChecksStable: true, ChecksPassing: true, CheckFingerprint: "f",
		Files: []radar.PullRequestFile{{Path: "src/a.go", Status: "modified", Additions: 1, Patch: "+x", ContentComplete: true}},
	}
	agent := radar.ACRResult{Accept: confidence >= 8, ModelAccept: confidence >= 8, Confidence: confidence,
		SafeSignals: []radar.ChangeSignal{radar.SignalRefactorNoBehaviorChange}, ReviewedFiles: []string{"src/a.go"}}
	reviewer, err := radar.NewPullRequestReviewer(policy, replayedScorer(0), replayedAgent{"rule-based", agent})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(trustReplayRecord{Input: in, Review: reviewer.Review(in), Outcome: outcome})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTrustReplay(t *testing.T) {
	policy := replayPolicy()
	roster, err := radar.ParseAuthorTrustRoster([]byte(`{"trusted": {"tier": 2}}`))
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		replayRecord(t, policy, "trusted", "", 9, "good"),                                 // live approval
		replayRecord(t, policy, "trusted", "", 7, "bad"),                                  // tier-only approval
		replayRecord(t, policy, "trusted", "", 7, ""),                                     // tier-only, unobserved
		replayRecord(t, policy, "trusted", "https://claude.ai/code/session_x", 7, "good"), // agent-authored: tier 1
		replayRecord(t, policy, "someone", "", 7, "good"),                                 // unlisted
	}
	report, err := trustReplay(policy, roster, strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	want := trustReplayReport{
		Records: 5, Replayed: 5,
		Background:    outcomeCount{Observed: 4, Bad: 1},
		LiveApprovals: outcomeCount{Approvals: 1, Observed: 1},
		TierOnly:      outcomeCount{Approvals: 2, Observed: 1, Bad: 1},
		ByTier:        map[int]outcomeCount{2: {Approvals: 2, Observed: 1, Bad: 1}},
	}
	if fmt.Sprintf("%+v", report) != fmt.Sprintf("%+v", want) {
		t.Fatalf("got  %+v\nwant %+v", report, want)
	}
}

func TestTrustReplayTwoReviewersUnderReviewFindings(t *testing.T) {
	policy := radar.PullRequestPolicy{
		Version: 5, Mode: radar.PullRequestModeShadow, AllowedBaseBranches: []string{"main"},
		ApprovalBasis: radar.ApprovalBasisReviewFindings, RequiredReviewers: 2,
		CalibrationSample: []float64{0, 1, 2, 3}, MinReviewConfidence: 8,
		BlockingFindingSeverities: []string{"P0", "P1"},
		AuthorTrust:               &radar.PullRequestAuthorTrust{Tiers: []radar.PullRequestTrustTier{{Tier: 2, MinReviewConfidence: 7}}},
	}
	in := radar.PullRequestInput{
		ID: "o/r#1", Repository: "o/r", Number: 1, BaseRef: "main", HeadSHA: strings.Repeat("a", 40),
		Author: "trusted", AuthorType: "User", Open: true, SameRepository: true,
		ChecksObserved: true, ChecksStable: true, ChecksPassing: true, CheckFingerprint: "f",
		Files: []radar.PullRequestFile{{Path: "src/a.go", Status: "modified", Additions: 1, Patch: "+x", ContentComplete: true}},
	}
	verdict := func(confidence int) radar.ACRResult {
		return radar.ACRResult{ModelAccept: true, Accept: confidence >= 8, Confidence: confidence, ReviewedFiles: []string{"src/a.go"}}
	}
	reviewer, err := radar.NewPullRequestReviewer(policy, replayedScorer(0),
		replayedAgent{"fireworks/glm", verdict(7)}, replayedAgent{"bedrock/codex", verdict(9)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(trustReplayRecord{Input: in, Review: reviewer.Review(in), Outcome: "good"})
	if err != nil {
		t.Fatal(err)
	}
	roster, err := radar.ParseAuthorTrustRoster([]byte(`{"trusted": {"tier": 2}}`))
	if err != nil {
		t.Fatal(err)
	}
	report, err := trustReplay(policy, roster, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if report.Replayed != 1 || report.LiveMismatches != 0 || report.TierOnly.Approvals != 1 {
		t.Fatalf("report = %+v", report)
	}
}
