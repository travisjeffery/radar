package radar

import (
	"encoding/json"
	"fmt"
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

// movedHunks is in at a later head where the base branch added ten lines
// above every hunk: the same patches, ten lines further down.
func movedHunks(in PullRequestInput) PullRequestInput {
	prior := in.HeadSHA
	in = laterHead(in)
	in.Files = append([]PullRequestFile{}, in.Files...)
	in.Prior = &PriorReview{Head: prior, Round: 1, ChangesComplete: true}
	for i := range in.Files {
		in.Files[i].Status = "modified"
		in.Files[i].Patch = strings.Replace(in.Files[i].Patch, "@@ -0,0 +1,100 @@", "@@ -10,0 +11,100 @@", 1)
		in.Prior.Changes = append(in.Prior.Changes, PullRequestFile{
			Path: in.Files[i].Path, Status: "modified", Additions: 10, ContentComplete: true,
			Patch: "@@ -1,3 +1,13 @@\n" + strings.Repeat("+base\n", 10) + " line\n line\n line",
		})
	}
	return in
}

func findingAt(file string, line, end int) ACRResult {
	res := structuralVerdict()
	res.Findings = []ReviewFinding{{Severity: "P2", Title: "naming", Summary: "consider renaming", File: file, Line: line, EndLine: end}}
	res.WhereToLook = []ReviewPointer{{File: file, Line: line, Why: "start here"}}
	return res
}

func modifiedInput(files int) PullRequestInput {
	in := largeFeatureInput(files)
	for i := range in.Files {
		in.Files[i].Status = "modified"
	}
	return in
}

func TestPriorHeadReviewFollowsMovedHunks(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	in := modifiedInput(2)
	file := in.Files[0].Path
	first := countingNamedAgent{&recordingAgent{result: findingAt(file, 5, 7)}, "openai/a"}
	round1, err := NewPullRequestReviewer(policy, fixedScorer(0), first)
	if err != nil {
		t.Fatal(err)
	}
	round1.Findings(in)
	record := round1.ReviewRecord(in.HeadSHA)
	if len(record.Reviews) != 1 || record.Reviews[0].Patches == "" || len(record.Reviews[0].Hunks[file]) != 1 {
		t.Fatalf("the record must carry the patch key and hunks: %+v", record.Reviews)
	}

	next := movedHunks(in)
	again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round2, err := NewPullRequestReviewer(policy, fixedScorer(0), again)
	if err != nil {
		t.Fatal(err)
	}
	if err := round2.UsePriorReviewRecord(roundTrip(t, record), in.HeadSHA, next.HeadSHA); err != nil {
		t.Fatal(err)
	}
	got := round2.Findings(next)
	if again.calls() != 0 || got.ReusedReviews != 1 || got.ReusedMoved != 1 || got.ReusedFromHead != in.HeadSHA {
		t.Fatalf("calls=%d reused=%d moved=%d from=%q", again.calls(), got.ReusedReviews, got.ReusedMoved, got.ReusedFromHead)
	}
	if fs := got.Agent.Findings; len(fs) != 1 || fs[0].Line != 15 || fs[0].EndLine != 17 {
		t.Fatalf("the finding must move with its hunk: %+v", fs)
	}
	if ps := got.Agent.WhereToLook; len(ps) != 1 || ps[0].Line != 15 {
		t.Fatalf("the pointer must move with its hunk: %+v", ps)
	}
	// This head's record holds the moved review under this head's request,
	// so the verdict stage reuses it without a model call.
	rec := round2.ReviewRecord(next.HeadSHA)
	if len(rec.Reviews) != 1 || rec.Reviews[0].Request == record.Reviews[0].Request || rec.Reviews[0].Result.Findings[0].Line != 15 {
		t.Fatalf("record = %+v", rec)
	}
	verdict := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round2v, err := NewPullRequestReviewer(policy, fixedScorer(0), verdict)
	if err != nil {
		t.Fatal(err)
	}
	if err := round2v.UseReviewRecord(roundTrip(t, rec), next.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if round2v.Findings(next); verdict.calls() != 0 {
		t.Fatalf("the verdict stage must reuse the moved review: calls=%d", verdict.calls())
	}
}

func TestPriorHeadReviewNotMovedOffItsHunks(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	in := modifiedInput(1)
	file := in.Files[0].Path
	for name, res := range map[string]ACRResult{
		"finding below the hunk": findingAt(file, 150, 0),
		"range leaving the hunk": findingAt(file, 95, 120),
		"another file":           findingAt("service/src/main/java/Other.java", 3, 0),
	} {
		t.Run(name, func(t *testing.T) {
			first := countingNamedAgent{&recordingAgent{result: res}, "openai/a"}
			round1, err := NewPullRequestReviewer(policy, fixedScorer(0), first)
			if err != nil {
				t.Fatal(err)
			}
			round1.Findings(in)
			record := round1.ReviewRecord(in.HeadSHA)

			next := movedHunks(in)
			again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
			round2, err := NewPullRequestReviewer(policy, fixedScorer(0), again)
			if err != nil {
				t.Fatal(err)
			}
			if err := round2.UsePriorReviewRecord(record, in.HeadSHA, next.HeadSHA); err != nil {
				t.Fatal(err)
			}
			if got := round2.Findings(next); again.calls() != 1 || got.ReusedReviews != 0 || got.ReusedMoved != 0 {
				t.Fatalf("a line the patches do not show cannot be moved; review anew: calls=%d reused=%d", again.calls(), got.ReusedReviews)
			}
		})
	}
}

func TestPriorHeadReviewMissesWhenContextChanges(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	in := modifiedInput(1)
	in.Files[0].Patch = "@@ -1,3 +1,4 @@\n context\n+added\n context\n context"
	in.Files[0].Additions = 1
	first := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round1, err := NewPullRequestReviewer(policy, fixedScorer(0), first)
	if err != nil {
		t.Fatal(err)
	}
	round1.Findings(in)
	record := round1.ReviewRecord(in.HeadSHA)

	// The base branch changed a line next to the patch: same addition, new
	// surroundings, so it is reviewed again.
	next := laterHead(in)
	next.Files = append([]PullRequestFile{}, in.Files...)
	next.Files[0].Patch = "@@ -5,3 +5,4 @@\n context\n+added\n changed context\n context"
	again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round2, err := NewPullRequestReviewer(policy, fixedScorer(0), again)
	if err != nil {
		t.Fatal(err)
	}
	if err := round2.UsePriorReviewRecord(record, in.HeadSHA, next.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if got := round2.Findings(next); again.calls() != 1 || got.ReusedReviews != 0 {
		t.Fatalf("changed context must be reviewed again: calls=%d reused=%d", again.calls(), got.ReusedReviews)
	}
}

// roundTrip passes a record through JSON, as the workflow's artifact does.
func roundTrip(t *testing.T, rec ReviewRecord) ReviewRecord {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var out ReviewRecord
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDiffHunksAndMoveRange(t *testing.T) {
	d := Diff{Changes: []Change{
		{File: "a.go", Content: "@@ -1,3 +1,4 @@ func A\n x\n+y\n x\n x\n@@ -40,2 +41,3 @@ func B\n x\n+y\n x"},
		{File: "b.go", Content: "@@ -3 +4 @@\n-x\n+y"},
		{File: "c.go", Content: "@@ -5,2 +5,0 @@\n-x\n-x"},
	}}
	from := diffHunks(d)
	want := map[string][][2]int{"a.go": {{1, 4}, {41, 3}}, "b.go": {{4, 1}}, "c.go": {{5, 0}}}
	if fmt.Sprint(from) != fmt.Sprint(want) {
		t.Fatalf("hunks = %v, want %v", from, want)
	}
	// At the later head the base branch added 5 lines above a.go's first
	// hunk and 2 more before its second, and 1 above b.go's and c.go's.
	to := map[string][][2]int{"a.go": {{6, 4}, {48, 3}}, "b.go": {{5, 1}}, "c.go": {{6, 0}}}
	for _, tc := range []struct {
		name               string
		file               string
		first, last        int
		wantFirst, wantEnd int
		ok                 bool
	}{
		{"line in the first hunk", "a.go", 2, 0, 7, 0, true},
		{"range in the second hunk moves by its own offset", "a.go", 41, 43, 48, 50, true},
		{"line between hunks", "a.go", 20, 0, 0, 0, false},
		{"range spanning two hunks", "a.go", 2, 42, 0, 0, false},
		{"range leaving its hunk", "a.go", 42, 44, 0, 0, false},
		{"header without a count is one line", "b.go", 4, 0, 5, 0, true},
		{"line after a one-line hunk", "b.go", 5, 0, 0, 0, false},
		{"a deletion-only hunk shows no line", "c.go", 5, 0, 0, 0, false},
		{"file without hunks", "d.go", 1, 0, 0, 0, false},
		{"reversed range", "a.go", 3, 2, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, end, ok := moveRange(from, to, tc.file, tc.first, tc.last)
			if ok != tc.ok || ok && (first != tc.wantFirst || end != tc.wantEnd) {
				t.Fatalf("moveRange(%d, %d) = %d, %d, %t; want %d, %d, %t", tc.first, tc.last, first, end, ok, tc.wantFirst, tc.wantEnd, tc.ok)
			}
		})
	}
	// A file whose hunk count differs between the heads does not map.
	if _, _, ok := moveRange(from, map[string][][2]int{"a.go": {{6, 4}}}, "a.go", 2, 0); ok {
		t.Fatal("a different number of hunks must not map")
	}
}

func TestPatchKeyKeepsEnclosingFunction(t *testing.T) {
	d := Diff{ID: "x", Changes: []Change{{File: "a.go", Content: "@@ -1,2 +1,3 @@ func A()\n x\n+y\n x"}}}
	moved := Diff{ID: "x", Changes: []Change{{File: "a.go", Content: "@@ -9,2 +9,3 @@ func A()\n x\n+y\n x"}}}
	other := Diff{ID: "x", Changes: []Change{{File: "a.go", Content: "@@ -9,2 +9,3 @@ func B()\n x\n+y\n x"}}}
	if PatchKey("r", d) != PatchKey("r", moved) {
		t.Fatal("the same hunk at other line numbers must keep its key")
	}
	if PatchKey("r", d) == PatchKey("r", other) {
		t.Fatal("the same hunk in another function must be reviewed anew")
	}
	if RequestKey("r", d) == RequestKey("r", moved) {
		t.Fatal("the exact request key must still see the line numbers")
	}
}

func TestDiffHunksCountsOnlyTheLinesShown(t *testing.T) {
	// The first piece of a hunk split across requests keeps the whole
	// hunk's header but shows only its first lines.
	d := Diff{Changes: []Change{{File: "a.go", Content: "@@ -100,40 +100,40 @@\n+a\n b\n-c\n\\ No newline at end of file"}}}
	from := diffHunks(d)
	if got := from["a.go"]; len(got) != 1 || got[0] != [2]int{100, 2} {
		t.Fatalf("hunks = %v, want [[100 2]]", got)
	}
	to := map[string][][2]int{"a.go": {{110, 2}}}
	if l, _, ok := moveRange(from, to, "a.go", 101, 0); !ok || l != 111 {
		t.Fatalf("a shown line must move: %d %t", l, ok)
	}
	if _, _, ok := moveRange(from, to, "a.go", 130, 0); ok {
		t.Fatal("a line the piece does not show must not move")
	}
}

func TestLookupMovedCountsAConcurrentReuseOnce(t *testing.T) {
	file := "a.go"
	hunks := map[string][][2]int{file: {{1, 3}}}
	res := findingAt(file, 2, 0)
	res.ReviewedFiles = []string{file}
	m := &reviewMemo{byKey: map[string]ACRResult{}, priorHead: "prior",
		priorByPatches: map[string]RecordedReview{"p": {Reviewer: "r", Request: "old", Patches: "p", Hunks: hunks, Result: res}}}
	m.usePush(&PriorReview{Head: "prior", ChangesComplete: true, Changes: []PullRequestFile{{
		Path: file, Status: "modified", ContentComplete: true,
		Patch: "@@ -1,3 +1,13 @@\n" + strings.Repeat("+base\n", 10) + " line\n line\n line",
	}}})
	moved := map[string][][2]int{file: {{11, 3}}}
	for range 2 {
		got, ok := m.lookupMoved(RecordedReview{Reviewer: "r", Request: "new", Patches: "p", Hunks: moved})
		if !ok || got.Findings[0].Line != 12 {
			t.Fatalf("lookupMoved = %+v, %t", got.Findings, ok)
		}
	}
	if m.reused != 2 || m.reusedMoved != 1 || len(m.order) != 1 {
		t.Fatalf("reused=%d moved=%d order=%d; want 2, 1, 1", m.reused, m.reusedMoved, len(m.order))
	}
}

func TestLookupCarriedCountsAConcurrentReuseOnce(t *testing.T) {
	res := findingAt("a.go", 2, 0)
	res.ReviewedFiles = []string{"a.go"}
	m := &reviewMemo{byKey: map[string]ACRResult{}, priorHead: "prior",
		trivial:          isDocPath,
		priorBySubstance: map[string]RecordedReview{"s": {Reviewer: "r", Request: "old", Substance: "s", Result: res}}}
	m.usePush(&PriorReview{Head: "prior", ChangesComplete: true, Changes: []PullRequestFile{{
		Path: "docs/guide.md", Status: "modified", ContentComplete: true, Patch: "@@ -1 +1 @@\n-a\n+b",
	}}})
	for range 2 {
		got, ok := m.lookupCarried(RecordedReview{Reviewer: "r", Request: "new", Substance: "s"})
		if !ok || got.Findings[0].Line != 2 {
			t.Fatalf("lookupCarried = %+v, %t", got.Findings, ok)
		}
	}
	if m.reused != 2 || m.reusedCarried != 1 || len(m.order) != 1 {
		t.Fatalf("reused=%d carried=%d order=%d; want 2, 1, 1", m.reused, m.reusedCarried, len(m.order))
	}
}

func TestPriorHeadReviewNotMovedWithoutProofTheLinesSurvived(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	in := modifiedInput(1)
	file := in.Files[0].Path
	for name, mutate := range map[string]func(*PullRequestInput){
		"no comparison with the prior head": func(next *PullRequestInput) { next.Prior = nil },
		"comparison incomplete":             func(next *PullRequestInput) { next.Prior.ChangesComplete = false },
		"comparison from another head":      func(next *PullRequestInput) { next.Prior.Head = strings.Repeat("d", 40) },
		// The author moved the same addition ten lines down, between two
		// identical blocks: its lines are new at this head, not shifted.
		"patch retargeted by the push": func(next *PullRequestInput) {
			next.Prior.Changes[0].Patch = "@@ -1,100 +1,110 @@\n" + strings.Repeat("-line\n", 100) + strings.Repeat("+line\n", 110)
		},
	} {
		t.Run(name, func(t *testing.T) {
			first := countingNamedAgent{&recordingAgent{result: findingAt(file, 5, 0)}, "openai/a"}
			round1, err := NewPullRequestReviewer(policy, fixedScorer(0), first)
			if err != nil {
				t.Fatal(err)
			}
			round1.Findings(in)
			record := round1.ReviewRecord(in.HeadSHA)

			next := movedHunks(in)
			mutate(&next)
			again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
			round2, err := NewPullRequestReviewer(policy, fixedScorer(0), again)
			if err != nil {
				t.Fatal(err)
			}
			if err := round2.UsePriorReviewRecord(record, in.HeadSHA, next.HeadSHA); err != nil {
				t.Fatal(err)
			}
			if got := round2.Findings(next); again.calls() != 1 || got.ReusedMoved != 0 {
				t.Fatalf("without proof the lines only shifted, review anew: calls=%d moved=%d", again.calls(), got.ReusedMoved)
			}
		})
	}
}

func TestPatchKeyNormalizesOnlyThePatches(t *testing.T) {
	d := Diff{ID: "x", ReviewNotes: []string{"see\n@@ -1,2 +1,3 @@ example"}, Changes: []Change{{File: "a.go", Content: "@@ -1,2 +1,3 @@\n x\n+y\n x"}}}
	other := d
	other.ReviewNotes = []string{"see\n@@ -7,2 +7,3 @@ example"}
	if PatchKey("r", d) == PatchKey("r", other) {
		t.Fatal("a note quoting a hunk header must still change the key")
	}
}

func TestDiffHunksCountsBlankContextLines(t *testing.T) {
	d := Diff{Changes: []Change{{File: "a.go", Content: "@@ -1,3 +1,4 @@\n x\n\n+y\n x\n"}}}
	if got := diffHunks(d)["a.go"]; len(got) != 1 || got[0] != [2]int{1, 4} {
		t.Fatalf("hunks = %v, want [[1 4]]", got)
	}
}

func TestDiffHunksRefusesLinesWithoutCoordinates(t *testing.T) {
	for name, content := range map[string]string{
		"a later piece of a split hunk": " x\n+y\n@@ -40,2 +41,3 @@\n x\n+y\n x",
		"no hunk header":                "+y",
	} {
		t.Run(name, func(t *testing.T) {
			if got := diffHunks(Diff{Changes: []Change{{File: "a.go", Content: content}}}); got != nil {
				t.Fatalf("hunks = %v, want none: those lines cannot be checked", got)
			}
		})
	}
}

func TestSurvivedRefusesAMovedDeletionOnlyHunk(t *testing.T) {
	m := &reviewMemo{priorHead: "prior"}
	m.usePush(&PriorReview{Head: "prior", ChangesComplete: true, Changes: []PullRequestFile{{
		Path: "a.go", Status: "modified", ContentComplete: true, Patch: "@@ -1,3 +1,13 @@\n" + strings.Repeat("+base\n", 10) + " line\n line\n line",
	}}})
	from := map[string][][2]int{"a.go": {{4, 0}}}
	if m.survived(from, map[string][][2]int{"a.go": {{14, 0}}}) {
		t.Fatal("a moved deletion-only hunk has no line to prove it survived")
	}
	if m.survived(from, map[string][][2]int{}) {
		t.Fatal("a request without hunks cannot be moved")
	}
}

func TestSurvivedBranches(t *testing.T) {
	base := strings.Repeat("+base\n", 10)
	for name, tc := range map[string]struct {
		push     PullRequestFile
		from, to [2]int
		want     bool
	}{
		"shifted by the lines added above": {
			PullRequestFile{Path: "a.go", Status: "modified", Patch: "@@ -1,3 +1,13 @@\n" + base + " l\n l\n l"},
			[2]int{20, 3}, [2]int{30, 3}, true,
		},
		"file the push did not touch, yet moved": {
			PullRequestFile{Path: "other.go", Status: "modified", Patch: "@@ -1,3 +1,13 @@\n" + base + " l\n l\n l"},
			[2]int{20, 3}, [2]int{30, 3}, false,
		},
		"comparison without a patch": {
			PullRequestFile{Path: "a.go", Status: "modified"},
			[2]int{20, 3}, [2]int{30, 3}, false,
		},
		"renamed by the push": {
			PullRequestFile{Path: "a.go", PreviousPath: "old.go", Status: "renamed", Patch: "@@ -1,3 +1,13 @@\n" + base + " l\n l\n l"},
			[2]int{20, 3}, [2]int{30, 3}, false,
		},
		"push deleted the line just above the hunk": {
			PullRequestFile{Path: "a.go", Status: "modified", Patch: "@@ -1,4 +1,13 @@\n" + base + " a\n-b\n c\n d"},
			[2]int{3, 2}, [2]int{12, 2}, false,
		},
		"offset other than the push's shift": {
			PullRequestFile{Path: "a.go", Status: "modified", Patch: "@@ -1,3 +1,13 @@\n" + base + " l\n l\n l"},
			[2]int{20, 3}, [2]int{31, 3}, false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := &reviewMemo{priorHead: "prior"}
			tc.push.ContentComplete = true
			m.usePush(&PriorReview{Head: "prior", ChangesComplete: true, Changes: []PullRequestFile{tc.push}})
			got := m.survived(map[string][][2]int{"a.go": {tc.from}}, map[string][][2]int{"a.go": {tc.to}})
			if got != tc.want {
				t.Fatalf("survived = %t, want %t", got, tc.want)
			}
		})
	}
}

// guidanceRounds reviews in with the test criteria, then next with the
// criteria edit applies, offering the first head's record to the second.
func guidanceRounds(t *testing.T, in, next PullRequestInput, edit func(*ReviewCriteria), clear func(*ReviewRecord)) (PullRequestReview, int) {
	t.Helper()
	first := countingNamedAgent{&recordingAgent{result: findingAt(in.Files[0].Path, 5, 7)}, "openai/a"}
	round1 := criteriaReviewer(t, first)
	round1.Findings(in)
	record := roundTrip(t, round1.ReviewRecord(in.HeadSHA))
	if clear != nil {
		clear(&record)
	}

	again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round2, err := NewPullRequestReviewer(criteriaPolicy(), fixedScorer(0), again)
	if err != nil {
		t.Fatal(err)
	}
	c := testCriteria(t)
	edit(&c)
	if err := round2.UseReviewCriteria(c); err != nil {
		t.Fatal(err)
	}
	if err := round2.UsePriorReviewRecord(record, in.HeadSHA, next.HeadSHA); err != nil {
		t.Fatal(err)
	}
	return round2.Findings(next), again.calls()
}

func editGuidance(c *ReviewCriteria) { c.Guidance["CLAUDE.md"] = []byte("new repo conventions") }

// pushedNothing is in at a later head whose complete comparison with in's
// head changes none of its files, as a merge of an unrelated base change.
func pushedNothing(in PullRequestInput) PullRequestInput {
	prior := in.HeadSHA
	in = laterHead(in)
	in.Prior = &PriorReview{Head: prior, Round: 1, ChangesComplete: true}
	return in
}

func TestPriorHeadReviewSurvivesAGuidanceChange(t *testing.T) {
	in := criteriaInput("svc/a.go", "infra/cmd/main.go")
	for name, edit := range map[string]func(*ReviewCriteria){
		"repository guidance": editGuidance,
		"directory guidance":  func(c *ReviewCriteria) { c.Guidance["infra/AGENTS.md"] = []byte("new infra guide") },
	} {
		t.Run(name, func(t *testing.T) {
			got, calls := guidanceRounds(t, in, pushedNothing(in), edit, nil)
			if calls != 0 || got.ReusedReviews != 1 || got.ReusedGuidanceChanged != 1 || got.ReusedMoved != 0 || got.ReusedFromHead != in.HeadSHA {
				t.Fatalf("calls=%d reused=%d guidance=%d moved=%d from=%q", calls, got.ReusedReviews, got.ReusedGuidanceChanged, got.ReusedMoved, got.ReusedFromHead)
			}
			if fs := got.Agent.Findings; len(fs) != 1 || fs[0].Line != 5 {
				t.Fatalf("the finding must stay where it was: %+v", fs)
			}
		})
	}
}

func TestPriorHeadReviewUnderTheSameGuidanceIsNotCountedAsChanged(t *testing.T) {
	in := criteriaInput("svc/a.go")
	got, calls := guidanceRounds(t, in, laterHead(in), func(*ReviewCriteria) {}, nil)
	if calls != 0 || got.ReusedReviews != 1 || got.ReusedGuidanceChanged != 0 {
		t.Fatalf("calls=%d reused=%d guidance=%d", calls, got.ReusedReviews, got.ReusedGuidanceChanged)
	}
}

func TestPriorHeadReviewFollowsMovedHunksAcrossAGuidanceChange(t *testing.T) {
	in := criteriaInput("svc/a.go")
	got, calls := guidanceRounds(t, in, movedHunks(in), editGuidance, nil)
	if calls != 0 || got.ReusedReviews != 1 || got.ReusedGuidanceChanged != 1 || got.ReusedMoved != 1 {
		t.Fatalf("calls=%d reused=%d guidance=%d moved=%d", calls, got.ReusedReviews, got.ReusedGuidanceChanged, got.ReusedMoved)
	}
	if fs := got.Agent.Findings; len(fs) != 1 || fs[0].Line != 15 || fs[0].EndLine != 17 {
		t.Fatalf("the finding must move with its hunk: %+v", fs)
	}
}

// A guidance change does not excuse moved hunks from the survival proof.
func TestPriorHeadReviewNotMovedAcrossAGuidanceChangeWithoutProof(t *testing.T) {
	in := criteriaInput("svc/a.go")
	for name, mutate := range map[string]func(*PullRequestInput){
		"no comparison":         func(n *PullRequestInput) { n.Prior = nil },
		"incomplete comparison": func(n *PullRequestInput) { n.Prior.ChangesComplete = false },
		"lines changed in the push": func(n *PullRequestInput) {
			n.Prior.Changes[0].Patch = "@@ -1,3 +1,14 @@\n" + strings.Repeat("+base\n", 10) + " line\n+edited\n line\n line"
		},
	} {
		t.Run(name, func(t *testing.T) {
			next := movedHunks(in)
			mutate(&next)
			got, calls := guidanceRounds(t, in, next, editGuidance, nil)
			if calls != 1 || got.ReusedReviews != 0 || got.ReusedGuidanceChanged != 0 || got.ReusedMoved != 0 {
				t.Fatalf("calls=%d reused=%d guidance=%d moved=%d", calls, got.ReusedReviews, got.ReusedGuidanceChanged, got.ReusedMoved)
			}
		})
	}
}

func TestPriorHeadReviewMissesWhenGuidanceAndPatchesChange(t *testing.T) {
	in := criteriaInput("svc/a.go")
	next := laterHead(in)
	next.Files = append([]PullRequestFile{}, in.Files...)
	next.Files[0].Patch += "\n+one more line"
	got, calls := guidanceRounds(t, in, next, editGuidance, nil)
	if calls != 1 || got.ReusedReviews != 0 || got.ReusedGuidanceChanged != 0 {
		t.Fatalf("calls=%d reused=%d guidance=%d", calls, got.ReusedReviews, got.ReusedGuidanceChanged)
	}
}

// A pack change may add a rule the earlier review never applied, so it is
// reviewed anew, even with the guidance changed too.
func TestPriorHeadReviewMissesWhenAPackChanges(t *testing.T) {
	in := criteriaInput("svc/a.go")
	for name, edit := range map[string]func(*ReviewCriteria){
		"pack": func(c *ReviewCriteria) {
			c.Packs["go"] = mustPack(t, "go", "---\nid: go\nversion: 4\n---\n- **GO-ERR-01** wrap errors\n")
		},
		"pack and guidance": func(c *ReviewCriteria) {
			editGuidance(c)
			c.Packs["go"] = mustPack(t, "go", "---\nid: go\nversion: 4\n---\n- **GO-ERR-01** wrap errors\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, calls := guidanceRounds(t, in, pushedNothing(in), edit, nil)
			if calls != 1 || got.ReusedReviews != 0 || got.ReusedGuidanceChanged != 0 {
				t.Fatalf("calls=%d reused=%d guidance=%d", calls, got.ReusedReviews, got.ReusedGuidanceChanged)
			}
		})
	}
}

// A PatchKey hit at the same head-side lines can still differ in its
// base-side numbers, so lookupMoved reuses it only with the push's proof,
// and does not count it as moved.
func TestLookupMovedNeedsProofEvenForUnmovedHunks(t *testing.T) {
	in := modifiedInput(1)
	first := countingNamedAgent{&recordingAgent{result: findingAt(in.Files[0].Path, 5, 7)}, "openai/a"}
	round1, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), first)
	if err != nil {
		t.Fatal(err)
	}
	round1.Findings(in)
	prior := round1.ReviewRecord(in.HeadSHA).Reviews[0]
	again := prior
	again.Request = "the same patches with other base-side header numbers"

	m := &reviewMemo{byKey: map[string]ACRResult{}, priorHead: in.HeadSHA, priorByPatches: map[string]RecordedReview{prior.Patches: prior}}
	if _, ok := m.lookupMoved(again); ok || m.reused != 0 {
		t.Fatalf("reused without the push's proof: reused=%d", m.reused)
	}
	m.usePush(&PriorReview{Head: in.HeadSHA, ChangesComplete: true})
	if _, ok := m.lookupMoved(again); !ok || m.reused != 1 || m.reusedMoved != 0 {
		t.Fatalf("ok=%v reused=%d moved=%d", ok, m.reused, m.reusedMoved)
	}
}

func TestPriorHeadReviewNotReusedAcrossAGuidanceChangeWithoutAComparison(t *testing.T) {
	in := criteriaInput("svc/a.go")
	got, calls := guidanceRounds(t, in, laterHead(in), editGuidance, nil)
	if calls != 1 || got.ReusedReviews != 0 || got.ReusedGuidanceChanged != 0 {
		t.Fatalf("calls=%d reused=%d guidance=%d", calls, got.ReusedReviews, got.ReusedGuidanceChanged)
	}
}

func TestPriorHeadReviewWithoutAContentKeyMissesOnAGuidanceChange(t *testing.T) {
	in := criteriaInput("svc/a.go")
	// Recorded before guidance-change reuse.
	clear := func(rec *ReviewRecord) {
		for i := range rec.Reviews {
			rec.Reviews[i].Content = ""
		}
	}
	got, calls := guidanceRounds(t, in, pushedNothing(in), editGuidance, clear)
	if calls != 1 || got.ReusedReviews != 0 {
		t.Fatalf("calls=%d reused=%d", calls, got.ReusedReviews)
	}
}

func TestContentKeyIgnoresOnlyTheGuidance(t *testing.T) {
	d := Diff{ID: "x", Changes: []Change{{File: "a.go", Content: "@@ -1,1 +1,2 @@\n line\n+new"}}}
	d.Criteria = &ReviewContext{Prefix: "old conventions", Suffix: "old guide"}
	withGuidance := d
	withGuidance.Criteria = &ReviewContext{Prefix: "conventions", Suffix: "guide"}
	if ContentKey("a", d) != ContentKey("a", withGuidance) || PatchKey("a", d) == PatchKey("a", withGuidance) {
		t.Fatal("ContentKey must ignore the guidance that PatchKey keeps")
	}
	noCriteria := d
	noCriteria.Criteria = nil
	if ContentKey("a", d) == ContentKey("a", noCriteria) {
		t.Fatal("ContentKey must keep the instructions that introduce the criteria")
	}
	withPack := withGuidance
	withPack.Criteria = &ReviewContext{Prefix: "conventions", Suffix: "guide", Packs: "go@abc"}
	if ContentKey("a", withGuidance) == ContentKey("a", withPack) {
		t.Fatal("ContentKey must change with the packs")
	}
	other := d
	other.Changes = []Change{{File: "a.go", Content: "@@ -1,1 +1,2 @@\n line\n+other"}}
	if ContentKey("a", d) == ContentKey("a", other) || ContentKey("a", d) == ContentKey("b", d) {
		t.Fatal("ContentKey must still change with the patches and the reviewer")
	}
}

// carryInput is a pull request with one code file and the given other
// files, each modified with a ten-line hunk.
func carryInput(paths ...string) PullRequestInput {
	in := modifiedInput(1)
	for _, p := range paths {
		in.Files = append(in.Files, PullRequestFile{
			Path: p, Status: "modified", Additions: 10, ContentComplete: true,
			Patch: "@@ -1,0 +1,10 @@\n" + strings.Repeat("+text\n", 9) + "+text",
		})
	}
	return in
}

// pushed is in at a later head where the push changed only the named files,
// each by one more line, with the complete comparison from in's head.
func pushed(in PullRequestInput, paths ...string) PullRequestInput {
	prior := in.HeadSHA
	in = laterHead(in)
	in.Files = append([]PullRequestFile{}, in.Files...)
	in.Prior = &PriorReview{Head: prior, Round: 1, ChangesComplete: true}
	for _, p := range paths {
		for i := range in.Files {
			if in.Files[i].Path == p {
				in.Files[i].Patch = strings.Replace(in.Files[i].Patch, "@@ -1,0 +1,10 @@", "@@ -1,0 +1,11 @@", 1) + "\n+more"
				in.Files[i].Additions++
			}
		}
		in.Prior.Changes = append(in.Prior.Changes, PullRequestFile{
			Path: p, Status: "modified", Additions: 1, ContentComplete: true,
			Patch: "@@ -10,0 +11 @@\n+more",
		})
	}
	return in
}

func carryPolicy() PullRequestPolicy {
	p := findingsPolicy(PullRequestModeShadow)
	p.DenyPaths = nil
	p.GeneratedFiles = &PullRequestGeneratedFiles{Paths: []string{"gen/**"}}
	return p
}

// carryRounds reviews in, then next with in's record offered, and returns
// the second round and its model calls.
func carryRounds(t *testing.T, in, next PullRequestInput, first ACRResult) (PullRequestReview, int, ReviewRecord) {
	t.Helper()
	return carryRoundsWith(t, carryPolicy(), in, next, first)
}

func carryRoundsWith(t *testing.T, policy PullRequestPolicy, in, next PullRequestInput, first ACRResult) (PullRequestReview, int, ReviewRecord) {
	t.Helper()
	round1, err := NewPullRequestReviewer(policy, fixedScorer(0), countingNamedAgent{&recordingAgent{result: first}, "openai/a"})
	if err != nil {
		t.Fatal(err)
	}
	round1.Findings(in)
	record := roundTrip(t, round1.ReviewRecord(in.HeadSHA))
	again := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	round2, err := NewPullRequestReviewer(policy, fixedScorer(0), again)
	if err != nil {
		t.Fatal(err)
	}
	if err := round2.UsePriorReviewRecord(record, in.HeadSHA, next.HeadSHA); err != nil {
		t.Fatal(err)
	}
	got := round2.Findings(next)
	return got, again.calls(), round2.ReviewRecord(next.HeadSHA)
}

func TestPriorHeadReviewCarriedOverATrivialPush(t *testing.T) {
	for name, file := range map[string]string{
		"documentation":  "docs/guide.md",
		"generated file": "gen/api.pb.go",
	} {
		t.Run(name, func(t *testing.T) {
			in := carryInput(file)
			code := in.Files[0].Path
			got, calls, rec := carryRounds(t, in, pushed(in, file), findingAt(code, 5, 7))
			if calls != 0 || got.ReusedReviews != 1 || got.ReusedCarried != 1 || got.ReusedFromHead != in.HeadSHA {
				t.Fatalf("calls=%d reused=%d carried=%d from=%q", calls, got.ReusedReviews, got.ReusedCarried, got.ReusedFromHead)
			}
			if fs := got.Agent.Findings; len(fs) != 1 || fs[0].File != code || fs[0].Line != 5 {
				t.Fatalf("findings = %+v", fs)
			}
			// The carried review joins this head's record, so the verdict
			// stage reuses it without a model call.
			verdict := countingNamedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
			stage, err := NewPullRequestReviewer(carryPolicy(), fixedScorer(0), verdict)
			if err != nil {
				t.Fatal(err)
			}
			next := pushed(in, file)
			if err := stage.UseReviewRecord(roundTrip(t, rec), next.HeadSHA); err != nil {
				t.Fatal(err)
			}
			if stage.Findings(next); verdict.calls() != 0 {
				t.Fatalf("the verdict stage must reuse the carried review: calls=%d", verdict.calls())
			}
		})
	}
}

func TestTrivialPathFollowsGitattributesAndRulesGuidance(t *testing.T) {
	policy := carryPolicy()
	policy.ReviewCriteria = &PullRequestReviewCriteria{Rules: ".radar/rules.yaml"}
	r, err := NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	if r.trivialPath("proto/api.pb.go") {
		t.Fatal("proto/api.pb.go is trivial before .gitattributes marks it generated")
	}
	if err := r.UseGitattributes([]byte("proto/*.pb.go linguist-generated\n")); err != nil {
		t.Fatal(err)
	}
	c := testCriteria(t)
	c.Rules.RepoGuidance = append(c.Rules.RepoGuidance, "docs/REVIEWING.md")
	c.Guidance["docs/REVIEWING.md"] = []byte("Check the error paths.")
	if err := r.UseReviewCriteria(c); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"proto/api.pb.go":   true,  // linguist-generated in the trusted .gitattributes
		"docs/guide.md":     true,  // documentation
		"docs/REVIEWING.md": false, // documentation the rules read as guidance
		"gen/api.pb.go":     true,  // the policy's generated paths still apply
	} {
		if got := r.trivialPath(p); got != want {
			t.Errorf("trivialPath(%q) = %t, want %t", p, got, want)
		}
	}
}

func TestPriorHeadReviewNotCarriedOverASubstantivePush(t *testing.T) {
	in := carryInput("docs/guide.md")
	code := in.Files[0].Path
	for name, next := range map[string]PullRequestInput{
		"code changed too": pushed(in, "docs/guide.md", code),
		"incomplete comparison": func() PullRequestInput {
			n := pushed(in, "docs/guide.md")
			n.Prior.ChangesComplete = false
			return n
		}(),
		"comparison from another head": func() PullRequestInput {
			n := pushed(in, "docs/guide.md")
			n.Prior.Head = strings.Repeat("d", 40)
			return n
		}(),
		"documentation renamed from code": func() PullRequestInput {
			n := pushed(in, "docs/guide.md")
			n.Prior.Changes[0].PreviousPath = "service/src/main/java/Guide.java"
			return n
		}(),
		"documentation added": func() PullRequestInput {
			n := pushed(in, "docs/new.md")
			n.Files = append(n.Files, PullRequestFile{Path: "docs/new.md", Status: "added", Additions: 1, Patch: "@@ -0,0 +1 @@\n+new", ContentComplete: true})
			return n
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			got, calls, _ := carryRounds(t, in, next, findingAt(code, 5, 7))
			if calls != 1 || got.ReusedCarried != 0 {
				t.Fatalf("calls=%d reused=%d carried=%d", calls, got.ReusedReviews, got.ReusedCarried)
			}
		})
	}
}

func TestPriorHeadReviewNotCarriedWhenItPointsAtAPushedFile(t *testing.T) {
	in := carryInput("docs/guide.md")
	for name, res := range map[string]ACRResult{
		"finding": func() ACRResult {
			r := findingAt(in.Files[0].Path, 5, 0)
			r.Findings[0].File = "docs/guide.md"
			return r
		}(),
		"pointer": func() ACRResult {
			r := findingAt(in.Files[0].Path, 5, 0)
			r.WhereToLook[0].File = "docs/guide.md"
			return r
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			got, calls, _ := carryRounds(t, in, pushed(in, "docs/guide.md"), res)
			if calls != 1 || got.ReusedCarried != 0 {
				t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
			}
		})
	}
}

// A lockfile push can carry a hash-only change, a downgrade or a typosquat
// that only a reviewer catches, and MDX can run code, so neither is carried.
func TestPriorHeadReviewNotCarriedOverALockfileOrMDXPush(t *testing.T) {
	for _, file := range []string{"go.sum", "package-lock.json", "docs/page.mdx"} {
		t.Run(file, func(t *testing.T) {
			in := carryInput(file)
			got, calls, _ := carryRounds(t, in, pushed(in, file), findingAt(in.Files[0].Path, 5, 7))
			if calls != 1 || got.ReusedCarried != 0 {
				t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
			}
		})
	}
}

// Guidance steers every later review once merged, and a lockfile's
// supply-chain signals need a reviewer, whatever a policy calls them; a
// policy can also name Markdown that must always be reviewed.
func TestPriorHeadReviewNotCarriedOverGuidanceLockfilesOrNamedPaths(t *testing.T) {
	policy := carryPolicy()
	policy.NeverCarryPaths = []string{"prompts/**"}
	for _, file := range []string{
		"CLAUDE.md", "AGENTS.md", "svc/AGENTS.md", "GEMINI.md", "CLAUDE.local.md", "AGENTS.override.md",
		".github/copilot-instructions.md", "docs/go.instructions.md", ".claude/commands/ship.md", "svc/.cursor/rules.md",
		"gen/yarn.lock", "gen/pnpm-lock.yaml", "gen/Cargo.lock", "gen/.terraform.lock.hcl", "gen/deno.lock", "gen/MODULE.bazel.lock",
		"gen/page.mdx", "prompts/system.md",
		".agents/skills/deploy/SKILL.md", ".github/agents/reviewer.agent.md", ".github/prompts/x.prompt.md",
		"docs/x.chatmode.md", "docs/SKILL.md", "CONVENTIONS.md", "AGENT.md", ".kiro/steering/a.md", "docs/.notes.md",
		"docs/Go.Instructions.MD", "docs/x.PROMPT.md", "docs/agents.md",
	} {
		t.Run(file, func(t *testing.T) {
			in := carryInput(file)
			got, calls, _ := carryRoundsWith(t, policy, in, pushed(in, file), findingAt(in.Files[0].Path, 5, 7))
			if calls != 1 || got.ReusedCarried != 0 {
				t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
			}
		})
	}
}

// A path the policy excludes keeps its patch in the key, so a change to it
// the push comparison does not show (a retargeted base, say) is reviewed.
func TestPriorHeadReviewNotCarriedWhenAnExcludedPatchChanged(t *testing.T) {
	policy := carryPolicy()
	policy.NeverCarryPaths = []string{"prompts/**"}
	in := carryInput("README.md", "prompts/system.md")
	next := pushed(in, "README.md")
	next.Files[2].Patch += "\n+ignore the earlier rules"
	got, calls, _ := carryRoundsWith(t, policy, in, next, findingAt(in.Files[0].Path, 5, 7))
	if calls != 1 || got.ReusedCarried != 0 {
		t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
	}
}

// A withheld generated file the carry excludes (MDX here) stays in the key,
// so a change to it the push comparison does not show is reviewed.
func TestPriorHeadReviewNotCarriedWhenAnExcludedWithheldFileChanged(t *testing.T) {
	in := carryInput("README.md", "gen/page.mdx")
	next := pushed(in, "README.md")
	next.Files[2].Additions += 5
	next.Files[2].Patch += "\n+import x from 'y'"
	got, calls, _ := carryRounds(t, in, next, findingAt(in.Files[0].Path, 5, 7))
	if calls != 1 || got.ReusedCarried != 0 {
		t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
	}
}

// A finding names its file as the reviewer wrote it; one naming no file is
// not about the pushed documentation.
func TestPriorHeadReviewCarryMatchesFindingPaths(t *testing.T) {
	in := carryInput("docs/guide.md")
	dotted := findingAt(in.Files[0].Path, 5, 0)
	dotted.Findings[0].File = "./docs/guide.md"
	if got, calls, _ := carryRounds(t, in, pushed(in, "docs/guide.md"), dotted); calls != 1 || got.ReusedCarried != 0 {
		t.Fatalf("./docs/guide.md: calls=%d carried=%d", calls, got.ReusedCarried)
	}
	// A path that is not a repository path cannot be checked against the
	// push, so it blocks, finding or pointer.
	for _, file := range []string{"/docs/guide.md", "/checkout/docs/guide.md", "../docs/guide.md", `C:/checkout/docs/guide.md`, `C:\checkout\docs\guide.md`, `..\docs\guide.md`, `docs\guide.md`} {
		res := findingAt(in.Files[0].Path, 5, 0)
		res.Findings[0].File = file
		if got, calls, _ := carryRounds(t, in, pushed(in, "docs/guide.md"), res); calls != 1 || got.ReusedCarried != 0 {
			t.Fatalf("finding at %s: calls=%d carried=%d", file, calls, got.ReusedCarried)
		}
		res = findingAt(in.Files[0].Path, 5, 0)
		res.WhereToLook[0].File = file
		if got, calls, _ := carryRounds(t, in, pushed(in, "docs/guide.md"), res); calls != 1 || got.ReusedCarried != 0 {
			t.Fatalf("pointer at %s: calls=%d carried=%d", file, calls, got.ReusedCarried)
		}
	}
	// A finding on documentation the push did not show is not carried
	// either: the key blanks that file, so it may have changed unseen.
	unpushed := carryInput("docs/guide.md", "docs/other.md")
	onOther := findingAt(unpushed.Files[0].Path, 5, 0)
	onOther.Findings[0].File = "docs/other.md"
	if got, calls, _ := carryRounds(t, unpushed, pushed(unpushed, "docs/guide.md"), onOther); calls != 1 || got.ReusedCarried != 0 {
		t.Fatalf("finding on docs/other.md: calls=%d carried=%d", calls, got.ReusedCarried)
	}
	// Nor on a withheld generated file the push did not show: the key
	// drops it.
	unpushedGen := carryInput("docs/guide.md", "gen/other.pb.go")
	onGen := findingAt(unpushedGen.Files[0].Path, 5, 0)
	onGen.Findings[0].File = "gen/other.pb.go"
	if got, calls, _ := carryRounds(t, unpushedGen, pushed(unpushedGen, "docs/guide.md"), onGen); calls != 1 || got.ReusedCarried != 0 {
		t.Fatalf("finding on gen/other.pb.go: calls=%d carried=%d", calls, got.ReusedCarried)
	}
	general := findingAt(in.Files[0].Path, 5, 0)
	general.Findings[0].File, general.Findings[0].Line = "", 0
	if got, calls, _ := carryRounds(t, in, pushed(in, "docs/guide.md"), general); calls != 0 || got.ReusedCarried != 1 {
		t.Fatalf("a finding with no file: calls=%d carried=%d", calls, got.ReusedCarried)
	}
}

// A push that renames a guidance or policy-named file to documentation is
// not carried: the source counts as much as the destination.
func TestPriorHeadReviewNotCarriedOverARenameFromAnExcludedFile(t *testing.T) {
	policy := carryPolicy()
	policy.NeverCarryPaths = []string{"prompts/**"}
	for _, from := range []string{"prompts/system.md", "CLAUDE.md", ".github/agents/x.agent.md"} {
		t.Run(from, func(t *testing.T) {
			in := carryInput("docs/guide.md")
			next := pushed(in, "docs/guide.md")
			next.Prior.Changes[0].PreviousPath, next.Prior.Changes[0].Status = from, "renamed"
			got, calls, _ := carryRoundsWith(t, policy, in, next, findingAt(in.Files[0].Path, 5, 7))
			if calls != 1 || got.ReusedCarried != 0 {
				t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
			}
		})
	}
}

// A finding on a generated file the push renamed away is not carried.
func TestPriorHeadReviewNotCarriedWhenItPointsAtARenamedFile(t *testing.T) {
	in := carryInput("gen/b.pb.go")
	next := pushed(in, "gen/b.pb.go")
	next.Prior.Changes[0].PreviousPath, next.Prior.Changes[0].Status = "gen/a.pb.go", "renamed"
	res := findingAt(in.Files[0].Path, 5, 0)
	res.Findings[0].File = "gen/a.pb.go"
	got, calls, _ := carryRounds(t, in, next, res)
	if calls != 1 || got.ReusedCarried != 0 {
		t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
	}
}

// A code patch whose hunk moved is not carried, even when the push changed
// only documentation: moving it is the moved-lines reuse's job, with its survival proof.
func TestPriorHeadReviewNotCarriedWhenACodeHunkMoved(t *testing.T) {
	in := carryInput("docs/guide.md")
	next := pushed(in, "docs/guide.md")
	next.Files[0].Patch = strings.Replace(next.Files[0].Patch, "@@ -0,0 +1,100 @@", "@@ -10,0 +11,100 @@", 1)
	got, calls, _ := carryRounds(t, in, next, findingAt(in.Files[0].Path, 5, 7))
	if calls != 1 || got.ReusedCarried != 0 {
		t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
	}
}

// A generated path the pull request adds is shown in full, so a push to it
// is reviewed: naming a file like generated code must not hide it.
func TestPriorHeadReviewNotCarriedOverAnAddedGeneratedFile(t *testing.T) {
	in := carryInput("gen/api.pb.go")
	in.Files[1].Status = "added"
	got, calls, _ := carryRounds(t, in, pushed(in, "gen/api.pb.go"), findingAt(in.Files[0].Path, 5, 7))
	if calls != 1 || got.ReusedCarried != 0 {
		t.Fatalf("calls=%d carried=%d", calls, got.ReusedCarried)
	}
}

func docOrGen(p string) bool { return isDocPath(p) || strings.HasPrefix(p, "gen/") }

func TestSubstanceKey(t *testing.T) {
	code := Change{File: "a.go", Content: "@@ -1 +1 @@\n+x"}
	doc := Change{File: "README.md", Content: "@@ -1 +1 @@\n+old", Complexity: 1}
	gen := GeneratedFile{Path: "gen/a.go", Additions: 1}
	d := Diff{ID: "x", Changes: []Change{code, doc}, Withheld: []GeneratedFile{gen}}
	edited := d
	edited.Changes = []Change{code, {File: "README.md", Content: "@@ -1 +1,2 @@\n+new\n+more", Complexity: 2}}
	edited.Withheld = []GeneratedFile{{Path: "gen/a.go", Additions: 5}}
	if SubstanceKey("r", d, docOrGen) == "" || SubstanceKey("r", d, docOrGen) != SubstanceKey("r", edited, docOrGen) {
		t.Fatal("SubstanceKey must ignore documentation and withheld generated contents")
	}
	renamed := d
	renamed.Changes = []Change{code, {File: "OTHER.md", Content: doc.Content}}
	codeEdit := d
	codeEdit.Changes = []Change{{File: "a.go", Content: "@@ -1 +1 @@\n+y"}, doc}
	codeMoved := d
	codeMoved.Changes = []Change{{File: "a.go", Content: "@@ -5 +5 @@\n+x"}, doc}
	lock := d
	lock.Lockfiles = []LockfileSummary{{Path: "go.sum"}}
	mdx := d
	mdx.Changes = []Change{code, {File: "page.mdx", Content: "@@ -1 +1 @@\n+old"}}
	mdxEdit := d
	mdxEdit.Changes = []Change{code, {File: "page.mdx", Content: "@@ -1 +1 @@\n+import x"}}
	for name, pair := range map[string][2]Diff{
		"another file name":   {d, renamed},
		"the code":            {d, codeEdit},
		"a code hunk's place": {d, codeMoved},
		"a lockfile summary":  {d, lock},
		"MDX content":         {mdx, mdxEdit},
	} {
		if SubstanceKey("r", pair[0], docOrGen) == SubstanceKey("r", pair[1], docOrGen) {
			t.Errorf("SubstanceKey must change with %s", name)
		}
	}
	fromCode := d
	fromCode.Changes = []Change{code, {File: "README.md", PreviousFile: "src/guide.go", Content: doc.Content}}
	fromDoc := d
	fromDoc.Changes = []Change{code, {File: "README.md", PreviousFile: "OLD.md", Content: "@@ -1 +1 @@\n+other"}}
	if SubstanceKey("r", d, docOrGen) == SubstanceKey("r", fromCode, docOrGen) {
		t.Error("a documentation patch renamed from code must stay in the key")
	}
	if SubstanceKey("r", d, docOrGen) != SubstanceKey("r", fromDoc, docOrGen) {
		t.Error("a documentation patch renamed from documentation is blanked")
	}
	renamedGen := d
	renamedGen.Withheld = []GeneratedFile{{Path: "gen/a.go", Status: "renamed", Additions: 1}}
	if SubstanceKey("r", d, docOrGen) == SubstanceKey("r", renamedGen, docOrGen) {
		t.Error("a withheld rename does not say where from, so it stays in the key")
	}
	if SubstanceKey("r", Diff{Changes: []Change{doc}}, docOrGen) != "" {
		t.Fatal("a request of only documentation has no SubstanceKey")
	}
}

func TestNeverCarryPathsAreValidated(t *testing.T) {
	policy := carryPolicy()
	policy.NeverCarryPaths = []string{"../prompts/**"}
	if _, err := NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: structuralVerdict()}); err == nil {
		t.Fatal("a never_carry_paths glob leaving the repository must be refused")
	}
}
