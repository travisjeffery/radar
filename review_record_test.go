package radar

import (
	"encoding/json"
	"strings"
	"testing"
)

// countingNamedAgent counts its calls and describes itself, so the reviewer
// memoizes it.
type countingNamedAgent struct {
	*recordingAgent
	name string
}

func (a countingNamedAgent) Describe() string { return a.name }

func (a countingNamedAgent) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.seen)
}

func pendingChecks(in PullRequestInput) PullRequestInput {
	in.ChecksObserved, in.ChecksStable, in.ChecksPassing = true, false, false
	in.CheckFingerprint = "pending"
	return in
}

func TestFindingsStageReviewsBeforeChecksFinish(t *testing.T) {
	agent := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeApprove), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := pendingChecks(largeFeatureInput(2))
	in.Draft = true
	in.UnresolvedThreads = 3
	in.ChangesRequested = true

	got := reviewer.Findings(in)
	if got.Action != PullRequestFindingsOnly || got.Stage != StageFindings || got.Eligible {
		t.Fatalf("findings stage must never decide: action=%s stage=%q eligible=%t", got.Action, got.Stage, got.Eligible)
	}
	if agent.calls() != 1 || len(got.Agent.Findings) != 1 {
		t.Fatalf("agent calls=%d findings=%+v; stages %+v", agent.calls(), got.Agent.Findings, got.Stages)
	}

	if verdict := reviewer.Review(in); verdict.Action != PullRequestRouteToHuman || agent.calls() != 1 {
		t.Fatalf("the verdict must still wait for checks: action=%s calls=%d", verdict.Action, agent.calls())
	}
}

func TestFindingsStageKeepsEligibilityGates(t *testing.T) {
	for name, mutate := range map[string]func(*PullRequestInput){
		"closed": func(in *PullRequestInput) { in.Open = false },
		"fork":   func(in *PullRequestInput) { in.SameRepository = false },
		"base":   func(in *PullRequestInput) { in.BaseRef = "release" },
	} {
		t.Run(name, func(t *testing.T) {
			agent := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
			reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
			if err != nil {
				t.Fatal(err)
			}
			in := largeFeatureInput(1)
			mutate(&in)
			if got := reviewer.Findings(in); agent.calls() != 0 || got.Action != PullRequestFindingsOnly {
				t.Fatalf("calls=%d action=%s", agent.calls(), got.Action)
			}
		})
	}
}

func TestReviewRecordIsReusedByTheVerdictStage(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.RequiredReviewers = 2
	in := largeFeatureInput(3)
	first := []countingNamedAgent{
		{&recordingAgent{result: structuralVerdict()}, "openai/a"},
		{&recordingAgent{result: structuralVerdict()}, "fireworks/b"},
	}
	findings, err := NewPullRequestReviewer(policy, fixedScorer(0), first[0], first[1])
	if err != nil {
		t.Fatal(err)
	}
	findings.Findings(pendingChecks(in))
	record := findings.ReviewRecord(in.HeadSHA)
	if len(record.Reviews) != 2 {
		t.Fatalf("record = %+v", record)
	}

	// The record crosses a process boundary as JSON.
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var loaded ReviewRecord
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	second := []countingNamedAgent{
		{&recordingAgent{result: structuralVerdict()}, "openai/a"},
		{&recordingAgent{result: structuralVerdict()}, "fireworks/b"},
	}
	verdict, err := NewPullRequestReviewer(policy, fixedScorer(0), second[0], second[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := verdict.UseReviewRecord(loaded, in.HeadSHA); err != nil {
		t.Fatal(err)
	}
	got := verdict.Review(in)
	if got.Action != PullRequestWouldApprove || got.ReusedReviews != 2 {
		t.Fatalf("action=%s reused=%d; stages %+v", got.Action, got.ReusedReviews, got.Stages)
	}
	if second[0].calls() != 0 || second[1].calls() != 0 {
		t.Fatalf("recorded requests must not reach the agents: %d, %d", second[0].calls(), second[1].calls())
	}
}

func TestReviewRecordMissesOnAnyRequestChange(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	agent := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	findings, err := NewPullRequestReviewer(policy, fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	findings.Findings(in)
	record := findings.ReviewRecord(in.HeadSHA)

	changed := in
	changed.Files = append([]PullRequestFile{}, in.Files...)
	changed.Files[0].Patch += "\n+one more line"
	changed.Files[0].Additions++
	again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	verdict, err := NewPullRequestReviewer(policy, fixedScorer(0), again)
	if err != nil {
		t.Fatal(err)
	}
	if err := verdict.UseReviewRecord(record, in.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if got := verdict.Review(changed); got.ReusedReviews != 0 || again.calls() != 1 {
		t.Fatalf("a changed patch must be reviewed again: reused=%d calls=%d", got.ReusedReviews, again.calls())
	}

	other := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/other-model"}
	verdict, err = NewPullRequestReviewer(policy, fixedScorer(0), other)
	if err != nil {
		t.Fatal(err)
	}
	if err := verdict.UseReviewRecord(record, in.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if verdict.Review(in); other.calls() != 1 {
		t.Fatal("another reviewer must not reuse a verdict")
	}
}

func TestReviewRecordSkipsFailures(t *testing.T) {
	failing := countingNamedAgent{&recordingAgent{result: ACRResult{Summary: "ACR OpenAI error, failing safe: timeout"}}, "openai/a"}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), failingAgent{failing})
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	reviewer.Findings(in)
	if got := reviewer.ReviewRecord(in.HeadSHA); len(got.Reviews) != 0 {
		t.Fatalf("a failed review must not be recorded: %+v", got.Reviews)
	}
}

// failingAgent returns its result without reviewed files, like every
// fail-safe result.
type failingAgent struct{ countingNamedAgent }

func (a failingAgent) Review(d Diff) ACRResult {
	res := a.countingNamedAgent.Review(d)
	res.ReviewedFiles = nil
	return res
}

func TestReviewRecordBoundToHead(t *testing.T) {
	agent := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	record := ReviewRecord{SchemaVersion: 1, HeadSHA: strings.Repeat("b", 40)}
	if err := reviewer.UseReviewRecord(record, strings.Repeat("a", 40)); err == nil {
		t.Fatal("a record for another head must be rejected")
	}
	record.HeadSHA = strings.Repeat("a", 40)
	record.SchemaVersion = 2
	if err := reviewer.UseReviewRecord(record, record.HeadSHA); err == nil {
		t.Fatal("an unknown schema version must be rejected")
	}
}

func TestReviewRecordSkipsTruncatedReviews(t *testing.T) {
	truncated := structuralVerdict()
	truncated.Truncated = true
	agent := countingNamedAgent{&recordingAgent{result: truncated}, "openai/a"}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	reviewer.Findings(in)
	if got := reviewer.ReviewRecord(in.HeadSHA); len(got.Reviews) != 0 {
		t.Fatalf("a truncated review must not be recorded: %+v", got.Reviews)
	}
}

func TestRejectedReviewRecordLeavesNothingBehind(t *testing.T) {
	agent := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	findings, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	findings.Findings(in)
	record := findings.ReviewRecord(in.HeadSHA)
	record.Reviews = append(record.Reviews, RecordedReview{Reviewer: "openai/a"})

	again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	verdict, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), again)
	if err != nil {
		t.Fatal(err)
	}
	if err := verdict.UseReviewRecord(record, in.HeadSHA); err == nil {
		t.Fatal("an entry without a request key must reject the record")
	}
	if got := verdict.Review(in); got.ReusedReviews != 0 || again.calls() != 1 {
		t.Fatalf("a rejected record must not be partly used: reused=%d calls=%d", got.ReusedReviews, again.calls())
	}
}

// laterHead is in at another head with the same patches, as when the base
// branch is merged into the pull request.
func laterHead(in PullRequestInput) PullRequestInput {
	in.HeadSHA = strings.Repeat("c", 40)
	return in
}

func TestPriorHeadReviewAnswersAByteIdenticalRequest(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	in := largeFeatureInput(2)
	first := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round1, err := NewPullRequestReviewer(policy, fixedScorer(0), first)
	if err != nil {
		t.Fatal(err)
	}
	round1.Findings(in)
	record := round1.ReviewRecord(in.HeadSHA)

	next := laterHead(in)
	again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round2, err := NewPullRequestReviewer(policy, fixedScorer(0), again)
	if err != nil {
		t.Fatal(err)
	}
	if err := round2.UsePriorReviewRecord(record, in.HeadSHA, next.HeadSHA); err != nil {
		t.Fatal(err)
	}
	got := round2.Findings(next)
	if again.calls() != 0 || got.ReusedReviews != 1 || got.ReusedFromHead != in.HeadSHA {
		t.Fatalf("calls=%d reused=%d from=%q", again.calls(), got.ReusedReviews, got.ReusedFromHead)
	}
	if len(got.Agent.Findings) != 1 {
		t.Fatalf("the reused review's findings must be reported: %+v", got.Agent.Findings)
	}
	if u := got.Agent.Usage; u == nil || u.InputTokens != 0 || u.ReusedRequests != 1 {
		t.Fatalf("a reused review costs nothing now: %+v", u)
	}
	// This head's record carries the review, so the verdict stage reuses it.
	if rec := round2.ReviewRecord(next.HeadSHA); len(rec.Reviews) != 1 || rec.Reviews[0].Request != record.Reviews[0].Request {
		t.Fatalf("record = %+v", rec)
	}
}

func TestPriorHeadReviewMissesOnAnyRequestChange(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	in := largeFeatureInput(1)
	first := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round1, err := NewPullRequestReviewer(policy, fixedScorer(0), first)
	if err != nil {
		t.Fatal(err)
	}
	round1.Findings(in)
	record := round1.ReviewRecord(in.HeadSHA)

	next := laterHead(in)
	next.Files = append([]PullRequestFile{}, in.Files...)
	next.Files[0].Patch += "\n+one more line"
	next.Files[0].Additions++
	again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round2, err := NewPullRequestReviewer(policy, fixedScorer(0), again)
	if err != nil {
		t.Fatal(err)
	}
	if err := round2.UsePriorReviewRecord(record, in.HeadSHA, next.HeadSHA); err != nil {
		t.Fatal(err)
	}
	got := round2.Findings(next)
	if again.calls() != 1 || got.ReusedReviews != 0 || got.ReusedFromHead != "" {
		t.Fatalf("a changed patch must be reviewed again: calls=%d reused=%d from=%q", again.calls(), got.ReusedReviews, got.ReusedFromHead)
	}
	// An unused earlier review does not carry into this head's record.
	if rec := round2.ReviewRecord(next.HeadSHA); len(rec.Reviews) != 1 || rec.Reviews[0].Request == record.Reviews[0].Request {
		t.Fatalf("record = %+v", rec)
	}
}

func TestPriorHeadRecordBoundToThatHead(t *testing.T) {
	agent := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	prior, head := strings.Repeat("a", 40), strings.Repeat("c", 40)
	record := ReviewRecord{SchemaVersion: 1, HeadSHA: strings.Repeat("b", 40)}
	if err := reviewer.UsePriorReviewRecord(record, prior, head); err == nil {
		t.Fatal("a record for another head than the named prior head must be rejected")
	}
	record.HeadSHA = head
	if err := reviewer.UsePriorReviewRecord(record, head, head); err == nil {
		t.Fatal("the prior head must be an earlier head, not this one")
	}
	record.HeadSHA = prior
	record.Reviews = []RecordedReview{{Reviewer: "openai/a", Request: "k", Result: ACRResult{Failed: true}}}
	if err := reviewer.UsePriorReviewRecord(record, prior, head); err == nil {
		t.Fatal("an incomplete review must reject the record")
	}
}
