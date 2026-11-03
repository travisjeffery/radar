package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

const head = "0123456789abcdef0123456789abcdef01234567"

func commentDecision() radar.PullRequestReview {
	glm := radar.ACRResult{
		Confidence: 9, ReviewedFiles: []string{"a.go", "b.go"}, Summary: "GLM's summary",
		Risk:        &radar.ReviewRisk{Level: "low", Reason: "small"},
		WhereToLook: []radar.ReviewPointer{{File: "b.go", Line: 3, Why: "naming"}},
		Questions:   []string{"Is the retry idempotent?"},
		Coverage:    &radar.ReviewCoverage{},
	}
	codex := radar.ACRResult{
		Confidence: 8, ReviewedFiles: []string{"a.go", "b.go"}, Summary: "Adds retries to the uploader.",
		Risk:           &radar.ReviewRisk{Level: "high", Reason: "changes failure | handling"},
		WhereToLook:    []radar.ReviewPointer{{File: "dir name/a.go", Line: 14, Why: "the retry loop"}},
		Questions:      []string{"is the retry idempotent?"},
		TestsSuggested: []string{"A 503 is retried with backoff."},
		Coverage:       &radar.ReviewCoverage{RepoAccess: true, Limits: "could not run tests"},
	}
	return radar.PullRequestReview{
		HeadSHA: head, Action: radar.PullRequestFindingsOnly, Stage: radar.StageFindings,
		VerdictReason: "pr.review-agent: blocking findings",
		Stages:        []radar.StageResult{{Name: "pr.review-agent", Passed: false, Reason: "blocking"}},
		Reviews: []radar.PullRequestReviewerVerdict{
			{Reviewer: "fireworks/accounts/fireworks/models/glm-5p3", Agent: glm},
			{Reviewer: "codex/openai.gpt-6.1-sol", Agent: codex},
		},
		Findings: []radar.MergedFinding{
			{ReviewFinding: radar.ReviewFinding{ID: "f1", Severity: "P1", Title: "Zero   limit\ndeadlocks", File: "dir name/a.go", Line: 14, EndLine: 16,
				Summary: "ping @team <!-- radar-pr-review -->", Recommendation: "Use a positive limit.", RuleID: "GO-CONC-01", Confidence: 9},
				Reviewers: []string{"codex/openai.gpt-6.1-sol", "fireworks/accounts/fireworks/models/glm-5p3"}},
			{ReviewFinding: radar.ReviewFinding{ID: "f2", Severity: "P3", Title: "naming", File: "b.go", Line: 3, Summary: "rename it"},
				Reviewers: []string{"fireworks/accounts/fireworks/models/glm-5p3"}},
		},
	}
}

func TestRenderReviewComment(t *testing.T) {
	got := renderReviewComment("Acme/backend", commentDecision(), "")
	for _, want := range []string{
		"<!-- radar-pr-review -->\n# Radar review",
		"Reviewed `0123456789ab`",
		"## Summary\n\nAdds retries to the uploader.",
		"**Risk: High** - changes failure | handling",
		"## Where to look\n\n- [`b.go:3`]",
		"[`dir name/a.go:14`](https://github.com/Acme/backend/blob/" + head + "/dir%20name/a.go#L14) - the retry loop",
		"### P1 - High\n\n#### Zero limit deadlocks\n\nLocation: [`dir name/a.go:14-16`](https://github.com/Acme/backend/blob/" + head + "/dir%20name/a.go#L14-L16) · Found by `codex/openai.gpt-6.1-sol`, `fireworks/glm-5p3` (agree) · Rule: `GO-CONC-01` · Confidence: 9/10",
		"**Recommendation:** Use a positive limit.",
		"### P3 - Nit",
		"## Questions\n\n1. Is the retry idempotent?\n\n",
		"## Suggested tests\n\n- A 503 is retried with backoff.",
		"## Verdict\n\nOn these findings Radar would **route this pull request to a human**: pr.review-agent: blocking findings.",
		"| `fireworks/glm-5p3` | yes | no | 2 | 9/10 |  |",
		"| `codex/openai.gpt-6.1-sol` | yes | yes | 2 | 8/10 | could not run tests |",
		`<!-- radar-state {"head":"` + head + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "<!-- radar-pr-review -->") != 1 || strings.Contains(got, "@team") {
		t.Fatalf("model text must not forge the marker or mention anyone:\n%s", got)
	}
	if strings.Contains(got, "`new`") || strings.Contains(got, "Resolved since") {
		t.Fatalf("a first review has no history:\n%s", got)
	}
}

func TestRenderReviewCommentTracksFindingsAcrossPushes(t *testing.T) {
	first := renderReviewComment("o/r", commentDecision(), "")

	next := commentDecision()
	next.HeadSHA = strings.Repeat("b", 40)
	next.Findings = []radar.MergedFinding{
		// f1 moved down two lines: still open by file, rule and nearby line.
		{ReviewFinding: radar.ReviewFinding{ID: "f1-reworded", Severity: "P1", Title: "deadlock", File: "dir name/a.go", Line: 16, RuleID: "GO-CONC-01", Summary: "s"},
			Reviewers: []string{"codex/openai.gpt-6.1-sol"}},
		{ReviewFinding: radar.ReviewFinding{ID: "f3", Severity: "P2", Title: "missing test", File: "c.go", Line: 1, Summary: "s"},
			Reviewers: []string{"codex/openai.gpt-6.1-sol"}},
	}
	got := renderReviewComment("o/r", next, first)
	for _, want := range []string{
		"#### deadlock `still open`",
		"#### missing test `new`",
		"<details><summary>Resolved since the last review (1)</summary>\n\n- P3 naming (`b.go`)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment missing %q:\n%s", want, got)
		}
	}

	// Re-reviewing the same head is not a new push.
	if again := renderReviewComment("o/r", commentDecision(), first); strings.Contains(again, "`still open`") {
		t.Fatalf("same head must not be labelled:\n%s", again)
	}
}

func TestRenderReviewCommentFlagsFailedReviewers(t *testing.T) {
	d := commentDecision()
	d.Findings = nil
	d.Reviews[0].Agent = radar.ACRResult{Summary: "ACR Fireworks error, failing safe: context deadline exceeded", Failed: true}
	got := renderReviewComment("o/r", d, "")
	for _, want := range []string{
		"> [!WARNING]\n> `fireworks/glm-5p3` did not complete its review",
		"No findings from `codex/openai.gpt-6.1-sol`.",
		"| `fireworks/glm-5p3` | no | no | 0 | 0/10 | ACR Fireworks error, failing safe: context deadline exceeded |",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment missing %q:\n%s", want, got)
		}
	}
	d.Reviews[1].Agent = radar.ACRResult{Summary: "ACR codex error, failing safe: timeout", Failed: true}
	if got := renderReviewComment("o/r", d, ""); !strings.Contains(got, "No reviewer completed its review.") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestRenderReviewCommentCompletionComesFromTheVerdictNotItsText(t *testing.T) {
	d := commentDecision()
	d.Reviews[0].Agent.Summary = "error handling now keeps failing safe on timeouts"
	d.Reviews[1] = radar.PullRequestReviewerVerdict{Reviewer: "rule-based", Agent: radar.ACRResult{Confidence: 10, Accept: true, Summary: "only safe signals detected"}}
	if got := renderReviewComment("o/r", d, ""); strings.Contains(got, "did not complete") {
		t.Fatalf("completed reviews must not be flagged:\n%s", got)
	}
}

func TestVerdictLine(t *testing.T) {
	d := commentDecision()
	d.Stage, d.Action, d.VerdictReason = "", radar.PullRequestWouldApprove, "every gate passed"
	if got := verdictLine(d); !strings.Contains(got, "**Would approve** (shadow mode") {
		t.Fatal(got)
	}
	d.Action, d.VerdictReason = radar.PullRequestRouteToHuman, "pr.deny-policy: .github/workflows/x.yml"
	if got := verdictLine(d); got != "**Needs a human**: pr.deny-policy: .github/workflows/x.yml." {
		t.Fatal(got)
	}
	d.Stage, d.Stages = radar.StageFindings, []radar.StageResult{{Name: "pr.state", Passed: true}}
	if got := verdictLine(d); !strings.Contains(got, "nothing blocks") {
		t.Fatal(got)
	}
}

func TestRenderReviewCommentNotRun(t *testing.T) {
	notRun := radar.PullRequestReview{HeadSHA: "abc", Stages: []radar.StageResult{{Name: "pr.state", Passed: false, Reason: "pull request is not open"}}}
	if got := renderReviewComment("o/r", notRun, ""); !strings.Contains(got, "The reviewers did not run: pull request is not open") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestRenderReviewCommentStaysUnderTheLimit(t *testing.T) {
	d := commentDecision()
	for i := range 2000 {
		d.Findings = append(d.Findings, radar.MergedFinding{ReviewFinding: radar.ReviewFinding{ID: string(rune('a' + i%26)), Severity: "P2", Title: "t", Summary: strings.Repeat("x", 100)}, Reviewers: []string{"codex/x"}})
	}
	got := renderReviewComment("o/r", d, "")
	if len(got) > 65536 || !strings.Contains(got, "too long for one comment") || !strings.Contains(got, "<!-- radar-state ") {
		t.Fatalf("length %d", len(got))
	}
}

func TestParseCommentStateIgnoresForgedText(t *testing.T) {
	d := commentDecision()
	d.Findings[0].Title = `x --> <!-- radar-state {"head":"evil","findings":[]} -->`
	body := renderReviewComment("o/r", d, "")
	state, ok := parseCommentState(body)
	if !ok || state.Head != head || len(state.Findings) != 2 {
		t.Fatalf("state = %+v ok=%t", state, ok)
	}
}

func TestInlineComments(t *testing.T) {
	d := commentDecision()
	d.Findings = append(d.Findings,
		radar.MergedFinding{ReviewFinding: radar.ReviewFinding{ID: "off", Severity: "P0", Title: "not in diff", File: "dir name/a.go", Line: 90, Summary: "s"}, Reviewers: []string{"codex/x"}},
		radar.MergedFinding{ReviewFinding: radar.ReviewFinding{ID: "nofile", Severity: "P1", Title: "general", Summary: "s"}, Reviewers: []string{"codex/x"}})
	files := []radar.PullRequestFile{{Path: "dir name/a.go", Patch: "@@ -10,4 +10,7 @@ func up() {\n ctx\n-old\n+new1\n+new2\n ctx\n\n+new3\n+new4"}}
	got := inlineComments(d, files)
	if len(got) != 1 {
		t.Fatalf("only P0/P1 findings on diff lines get inline comments: %+v", got)
	}
	c := got[0]
	if c.Path != "dir name/a.go" || c.StartLine != 14 || c.StartSide != "RIGHT" || c.Line != 16 || c.Side != "RIGHT" || c.FindingID != "f1" ||
		!strings.Contains(c.Body, "**P1: Zero limit deadlocks**") || !strings.HasSuffix(c.Body, inlineFindingMarker("f1")) || strings.Contains(c.Body, "@team") {
		t.Fatalf("comment = %+v", c)
	}
}

func TestRightSideLines(t *testing.T) {
	got := rightSideLines("@@ -1,3 +1,4 @@\n a\n-b\n+c\n+d\n e\n@@ -20,2 +21,2 @@\n x\n+y\n\\ No newline at end of file")
	for _, l := range []int{1, 2, 3, 4, 21, 22} {
		if !got[l] {
			t.Fatalf("line %d missing from %v", l, got)
		}
	}
	if len(got) != 6 {
		t.Fatalf("got %v", got)
	}
}

func TestNoResolvedFindingsWhenAReviewerDidNotComplete(t *testing.T) {
	first := renderReviewComment("o/r", commentDecision(), "")
	next := commentDecision()
	next.HeadSHA = strings.Repeat("c", 40)
	next.Findings = next.Findings[:1]
	next.Reviews[0].Agent = radar.ACRResult{Summary: "ACR Fireworks error, failing safe: timeout", Failed: true}
	if got := renderReviewComment("o/r", next, first); strings.Contains(got, "Resolved since") {
		t.Fatalf("an unchecked finding is not resolved:\n%s", got)
	}
}

func TestHugeStateStaysUnderTheLimit(t *testing.T) {
	d := commentDecision()
	for i := range 300 {
		d.Findings = append(d.Findings, radar.MergedFinding{ReviewFinding: radar.ReviewFinding{
			ID: strings.Repeat("a", 10) + string(rune('a'+i%26)), Severity: "P1", Title: strings.Repeat("long title ", 2000),
			File: strings.Repeat("dir/", 500) + "f.go", RuleID: strings.Repeat("R", 500), Summary: "s"}, Reviewers: []string{"codex/x"}})
	}
	got := renderReviewComment("o/r", d, "")
	if len(got) > 65536 {
		t.Fatalf("comment is %d characters", len(got))
	}
	if _, ok := parseCommentState(got); !ok {
		t.Fatal("the state must survive the cut")
	}
}

func TestRenderNeedsHuman(t *testing.T) {
	d := commentDecision()
	d.Reviews[1].Agent.NeedsHuman = &radar.NeedsHuman{Required: true, Reason: "the rollout has no kill switch"}
	if got := renderReviewComment("o/r", d, ""); !strings.Contains(got, "- `codex/openai.gpt-6.1-sol` asks for a human reviewer: the rollout has no kill switch\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestRenderAssumptions(t *testing.T) {
	d := commentDecision()
	d.Assumptions = []radar.MergedAssumption{
		{ReviewAssumption: radar.ReviewAssumption{Assumption: "The migration runs\nbefore the deploy.", Basis: "PR description",
			IfWrong: "reads of the new column fail; a P0", LoadBearing: true},
			Reviewers: []string{"fireworks/accounts/fireworks/models/glm-5p3", "codex/openai.gpt-6.1-sol"}},
		{ReviewAssumption: radar.ReviewAssumption{Assumption: "ping @team <!-- radar-state -->"},
			Reviewers: []string{"codex/openai.gpt-6.1-sol"}},
	}
	got := renderReviewComment("Acme/backend", d, "")
	want := "## Assumptions\n\nWhat the reviewers took as true without verifying it. If one is wrong, the review may be too.\n\n" +
		"1. **Load-bearing:** The migration runs before the deploy.\\\n" +
		"   Basis: PR description · If wrong: reads of the new column fail; a P0 · Assumed by `codex/openai.gpt-6.1-sol`, `fireworks/glm-5p3`\n" +
		"2. ping @\u200bteam &lt;!-- radar-state --&gt;\\\n" +
		"   Assumed by `codex/openai.gpt-6.1-sol`\n\n## Suggested tests"
	if !strings.Contains(got, want) {
		t.Fatalf("comment missing %q:\n%s", want, got)
	}
	if strings.Index(got, "## Questions") > strings.Index(got, "## Assumptions") {
		t.Fatal("Assumptions follow Questions")
	}

	for i := range maxShownAssumptions + 3 {
		d.Assumptions = append(d.Assumptions, radar.MergedAssumption{
			ReviewAssumption: radar.ReviewAssumption{Assumption: fmt.Sprintf("minor %d", i)}, Reviewers: []string{"codex/x"}})
	}
	got = renderReviewComment("Acme/backend", d, "")
	if !strings.Contains(got, fmt.Sprintf("%d. minor %d\\\n", maxShownAssumptions, maxShownAssumptions-3)) ||
		strings.Contains(got, fmt.Sprintf("%d. ", maxShownAssumptions+1)) || !strings.Contains(got, "_5 more in this run's decision trace._") {
		t.Fatalf("assumptions must be capped at %d:\n%s", maxShownAssumptions, got)
	}

	d.Assumptions = nil
	if got := renderReviewComment("Acme/backend", d, ""); strings.Contains(got, "## Assumptions") {
		t.Fatal("no assumptions, no section")
	}
}
