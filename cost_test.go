package radar

import (
	"encoding/json"
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEstimateReviewCostUSD(t *testing.T) {
	// A median backend Opus review (2026-10-11..14): 306,886 input tokens,
	// 241,554 read from cache and 62,391 written to it, 6,556 output.
	u := &ReviewUsage{InputTokens: 306886, CacheReadTokens: 241554, CacheWriteTokens: 62391, OutputTokens: 6556}
	got, ok := EstimateReviewCostUSD("bedrock/us.anthropic.claude-opus-5-5", u)
	want := (2941*4.40 + 241554*0.22 + 62391*5.50 + 6556*22.0) / 1e6
	if !ok || !near(got, want) {
		t.Fatalf("opus = %v, %t; want %v", got, ok, want)
	}
	got, ok = EstimateReviewCostUSD("codex/openai.gpt-6.1-sol", &ReviewUsage{InputTokens: 1e6, OutputTokens: 1e6})
	if !ok || !near(got, 2.20+11) {
		t.Fatalf("sol = %v, %t", got, ok)
	}
	if _, ok := EstimateReviewCostUSD("fireworks/accounts/fireworks/models/glm-5p3", &ReviewUsage{InputTokens: 10}); ok {
		t.Fatal("a model without a price must say so")
	}
	if got, ok := EstimateReviewCostUSD("fireworks/accounts/fireworks/models/glm-5p3", &ReviewUsage{ReusedRequests: 1}); !ok || got != 0 {
		t.Fatalf("a reused review costs nothing: %v, %t", got, ok)
	}
}

func usageAgent(name string, u ReviewUsage) countingNamedAgent {
	res := structuralVerdict()
	res.Usage = &u
	return countingNamedAgent{&recordingAgent{result: res}, name}
}

func TestReviewCostIsCountedOnceAcrossStages(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.RequiredReviewers = 2
	in := largeFeatureInput(3)
	opus := ReviewUsage{InputTokens: 1e6, OutputTokens: 1e5, Requests: 3}
	sol := ReviewUsage{InputTokens: 2e6, CacheReadTokens: 1e6, Requests: 5}
	findings, err := NewPullRequestReviewer(policy, fixedScorer(0),
		usageAgent("bedrock/us.anthropic.claude-opus-5-5", opus), usageAgent("codex/openai.gpt-6.1-sol", sol))
	if err != nil {
		t.Fatal(err)
	}
	first := findings.Findings(pendingChecks(in))
	wantOpus, wantSol := 4.40+2.2, 2.20+0.11
	if len(first.Reviews) != 2 || !near(first.Reviews[0].Agent.Usage.CostUSD, wantOpus) || !near(first.Reviews[1].Agent.Usage.CostUSD, wantSol) {
		t.Fatalf("per-reviewer cost: %+v", first.Reviews)
	}
	if u := first.Agent.Usage; !near(u.CostUSD, wantOpus+wantSol) || u.Unpriced || u.ReusedRequests != 0 || u.Requests != 8 {
		t.Fatalf("run usage = %+v", u)
	}

	data, err := json.Marshal(findings.ReviewRecord(in.HeadSHA))
	if err != nil {
		t.Fatal(err)
	}
	var record ReviewRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if !near(record.Reviews[0].Result.Usage.CostUSD+record.Reviews[1].Result.Usage.CostUSD, wantOpus+wantSol) {
		t.Fatalf("the record keeps what the reviews cost: %+v", record.Reviews)
	}

	verdict, err := NewPullRequestReviewer(policy, fixedScorer(0),
		usageAgent("bedrock/us.anthropic.claude-opus-5-5", opus), usageAgent("codex/openai.gpt-6.1-sol", sol))
	if err != nil {
		t.Fatal(err)
	}
	if err := verdict.UseReviewRecord(record, in.HeadSHA); err != nil {
		t.Fatal(err)
	}
	got := verdict.Review(in)
	if got.ReusedReviews != 2 {
		t.Fatalf("reused = %d", got.ReusedReviews)
	}
	if u := got.Agent.Usage; u == nil || u.CostUSD != 0 || u.InputTokens != 0 || u.ReusedRequests != 2 {
		t.Fatalf("a verdict that reuses every review spends nothing: %+v", u)
	}
	if again := verdict.ReviewRecord(in.HeadSHA); !near(again.Reviews[0].Result.Usage.CostUSD+again.Reviews[1].Result.Usage.CostUSD, wantOpus+wantSol) {
		t.Fatalf("reusing must not rewrite the record: %+v", again.Reviews)
	}
}

func TestUnpricedReviewerIsFlagged(t *testing.T) {
	agent := usageAgent("fireworks/accounts/fireworks/models/glm-5p3", ReviewUsage{InputTokens: 100, OutputTokens: 10, Requests: 1})
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	if u := reviewer.Findings(largeFeatureInput(1)).Agent.Usage; u == nil || !u.Unpriced || u.CostUSD != 0 {
		t.Fatalf("usage = %+v", u)
	}
}
