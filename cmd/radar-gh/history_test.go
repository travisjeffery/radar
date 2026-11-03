package main

import (
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

func TestRoundBaseline(t *testing.T) {
	r1 := commentState{Head: "h1", Findings: []stateFinding{{ID: "a", Severity: "P1"}}}
	if round, base := roundBaseline(commentState{}, false, "h1"); round != 1 || base != nil {
		t.Fatalf("no comment: round %d, base %+v", round, base)
	}
	// A comment from before rounds were recorded is round 1.
	round, base := roundBaseline(r1, true, "h2")
	if round != 2 || base == nil || base.Head != "h1" || base.Round != 1 || len(base.Findings) != 1 {
		t.Fatalf("new head: round %d, base %+v", round, base)
	}
	// The verdict and clarify stages at h2 read the comment the findings
	// stage wrote there and keep its baseline.
	r2 := commentState{Head: "h2", Round: 2, Prior: base}
	if round, again := roundBaseline(r2, true, "h2"); round != 2 || again != base {
		t.Fatalf("same head: round %d, base %+v", round, again)
	}
	if round, next := roundBaseline(r2, true, "h3"); round != 3 || next.Head != "h2" || next.Round != 2 {
		t.Fatalf("third head: round %d, base %+v", round, next)
	}
}

func TestStateCarriesTheRounds(t *testing.T) {
	first := renderReviewComment("o/r", commentDecision(), "")
	next := commentDecision()
	next.HeadSHA = strings.Repeat("b", 40)
	second := renderReviewComment("o/r", next, first)
	s, ok := parseCommentState(second)
	if !ok || s.Round != 2 || s.Prior == nil || s.Prior.Head != head || s.Prior.Round != 1 || len(s.Prior.Findings) != 2 {
		t.Fatalf("state = %+v", s)
	}
	// Re-rendered at the same head, as clarify does, it keeps round 2's
	// baseline and labels.
	moved := next
	moved.Findings = append([]radar.MergedFinding(nil), next.Findings...)
	moved.Findings[0].ID = "f9"
	again := renderReviewComment("o/r", moved, second)
	if s, _ := parseCommentState(again); s.Round != 2 || s.Prior == nil || s.Prior.Head != head {
		t.Fatalf("re-rendered state = %+v", s)
	}
	if !strings.Contains(again, "`still open`") {
		t.Fatalf("a re-render at the same head lost its labels:\n%s", again)
	}
}

func TestHugeStateTrimsThePriorRoundFirst(t *testing.T) {
	var findings []radar.MergedFinding
	for i := range 200 {
		findings = append(findings, radar.MergedFinding{ReviewFinding: radar.ReviewFinding{
			ID: strings.Repeat("f", 10) + string(rune('a'+i%26)), Severity: "P2", Title: strings.Repeat("t", 150), File: "a.go", Line: i}})
	}
	prior := &priorState{Head: "h1", Round: 1}
	for _, f := range findings {
		prior.Findings = append(prior.Findings, stateFinding{ID: f.ID, Severity: f.Severity, Title: f.Title, File: f.File, Line: f.Line})
	}
	state := renderState("h2", 2, findings[:10], prior, nil, nil)
	if len(state) > maxStateChars+len(stateOpen)+len(stateClose) {
		t.Fatalf("state is %d characters", len(state))
	}
	s, ok := parseCommentState(state)
	if !ok || len(s.Findings) != 10 || s.Prior == nil || len(s.Prior.Findings) == 0 || len(s.Prior.Findings) == 200 {
		t.Fatalf("state kept %d findings and %+v prior", len(s.Findings), s.Prior != nil)
	}
	if len(prior.Findings) != 200 {
		t.Fatal("trimming changed the caller's baseline")
	}
}

func heldDecision() radar.PullRequestReview {
	d := commentDecision()
	d.Convergence = &radar.ConvergenceRecord{
		Round: 3, PriorHead: strings.Repeat("c", 40), Incremental: true,
		Held: []radar.HeldFinding{{FindingID: "f1", File: "dir name/a.go", Line: 14, Severity: "P1", Reason: radar.HeldUnchangedLines}},
	}
	return d
}

func TestRenderHeldFindings(t *testing.T) {
	got := renderReviewComment("o/r", heldDecision(), "")
	for _, want := range []string{
		"_Review round 3. A P0 or P1 blocks when it is on lines changed since `cccccccccccc`",
		"### P2 - Medium\n\n#### Zero limit deadlocks `P1, not blocking: first raised on lines unchanged since cccccccccccc`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "### P1 - High") {
		t.Fatalf("a held P1 shows under P1:\n%s", got)
	}

	d := heldDecision()
	d.Convergence.Held[0] = radar.HeldFinding{FindingID: "f1", File: "dir name/a.go", Line: 14, Severity: "P1", Reason: radar.HeldResolvedThread, ResolvedBy: "author", ByPRAuthor: true}
	if got := renderReviewComment("o/r", d, ""); !strings.Contains(got, "`P1, not blocking: its thread was resolved by author; a person other than the author must accept that`") {
		t.Fatalf("resolved label missing:\n%s", got)
	}

	d.Convergence = &radar.ConvergenceRecord{Round: 2, PriorHead: "c", Reason: "the changes since c could not all be read"}
	if got := renderReviewComment("o/r", d, ""); !strings.Contains(got, "_Review round 2. Every P0 and P1 blocks: the changes since c could not all be read._") {
		t.Fatalf("fail-closed note missing:\n%s", got)
	}
}

func TestHeldFindingsAreNotPostedInline(t *testing.T) {
	files := []radar.PullRequestFile{{Path: "dir name/a.go", Patch: "@@ -0,0 +1,20 @@\n" + strings.Repeat("+x\n", 20)}}
	if got := inlineComments(commentDecision(), files); len(got) != 1 {
		t.Fatalf("the blocking P1 is posted inline: %+v", got)
	}
	if got := inlineComments(heldDecision(), files); len(got) != 0 {
		t.Fatalf("a held P1 would open a thread, which blocks: %+v", got)
	}
}

func TestResolvedFindings(t *testing.T) {
	node := func(resolved, outdated bool, typename, by, body string) resolvedThreadNode {
		n := resolvedThreadNode{IsResolved: resolved, IsOutdated: outdated, Path: "a.go", Line: 7}
		if by != "" {
			n.ResolvedBy = &struct {
				Login string `json:"login"`
			}{by}
		}
		n.Comments.Nodes = append(n.Comments.Nodes, struct {
			Author struct {
				Login    string `json:"login"`
				Typename string `json:"__typename"`
			} `json:"author"`
			Body string `json:"body"`
		}{Body: body})
		n.Comments.Nodes[0].Author.Login, n.Comments.Nodes[0].Author.Typename = "github-actions", typename
		return n
	}
	radarBody := "**P1: Zero limit deadlocks** (Radar, found by `codex`)\n\nimpact\n\n" + inlineFindingMarker("abc123")
	got := resolvedFindings([]resolvedThreadNode{
		node(true, false, "Bot", "reviewer", radarBody),
		node(true, false, "Bot", "author", radarBody),
		node(true, false, "Bot", "", radarBody),
		node(false, false, "Bot", "", radarBody),                  // unresolved
		node(true, true, "Bot", "reviewer", radarBody),            // outdated: its lines changed
		node(true, false, "User", "reviewer", radarBody),          // a person quoting Radar
		node(true, false, "Bot", "reviewer", radarBody+" quoted"), // the marker is not last
	}, "author")
	ranged := node(true, false, "Bot", "reviewer", radarBody)
	ranged.StartLine, ranged.Line = 20, 31
	if r := resolvedFindings([]resolvedThreadNode{ranged}, "author"); len(r) != 1 || r[0].Line != 20 || r[0].EndLine != 31 {
		t.Fatalf("a range thread keeps both ends: %+v", r)
	}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[0].FindingID != "abc123" || got[0].Title != "Zero limit deadlocks" || got[0].ByPRAuthor || got[0].File != "a.go" || got[0].Line != 7 {
		t.Fatalf("got %+v", got[0])
	}
	if !got[1].ByPRAuthor || !got[2].ByPRAuthor {
		t.Fatalf("resolved by the author, or by no one known, needs a person: %+v", got[1:])
	}
}

func TestStateKeepsBlockersBeforeOtherFindings(t *testing.T) {
	finding := func(sev string, i int) radar.MergedFinding {
		return radar.MergedFinding{ReviewFinding: radar.ReviewFinding{ID: sev + string(rune('a'+i%26)), Severity: sev, Title: strings.Repeat("t", 150), File: "a.go", Line: i}}
	}
	prior := &priorState{Head: "h1", Round: 1}
	var current []radar.MergedFinding
	for i := range 15 {
		prior.Findings = append(prior.Findings, stateFinding{ID: "p" + string(rune('a'+i%26)), Severity: "P1", Title: strings.Repeat("t", 150), File: "a.go", Line: i})
		current = append(current, finding("P1", i))
	}
	for i := range 60 {
		prior.Findings = append(prior.Findings, stateFinding{ID: "q", Severity: "P3", Title: strings.Repeat("t", 150), File: "a.go", Line: i})
		current = append(current, finding("P2", i))
	}
	s, _ := parseCommentState(renderState("h2", 2, current, prior, nil, nil))
	blockers := func(fs []stateFinding) (n int) {
		for _, f := range fs {
			if blockingState(f) {
				n++
			}
		}
		return n
	}
	if blockers(s.Prior.Findings) != 15 || blockers(s.Findings) != 15 || s.Truncated || s.Prior.Truncated {
		t.Fatalf("kept %d prior and %d current blockers; truncated %t/%t", blockers(s.Prior.Findings), blockers(s.Findings), s.Truncated, s.Prior.Truncated)
	}

	// With too many blockers to keep, the state says so, and the next
	// round compares with nothing.
	for i := range 200 {
		current = append(current, finding("P0", i))
	}
	s, _ = parseCommentState(renderState("h2", 2, current, nil, nil, nil))
	if !s.Truncated {
		t.Fatal("cutting blockers must mark the state truncated")
	}
	if _, base := roundBaseline(s, true, "h3"); base == nil || !base.Truncated {
		t.Fatalf("the next round must know its baseline is incomplete: %+v", base)
	}
}

func TestStateCarriesThePullRequestCost(t *testing.T) {
	withCost := func(d radar.PullRequestReview, usd float64, unpriced bool) radar.PullRequestReview {
		d.Agent.Usage = &radar.ReviewUsage{InputTokens: 1, CostUSD: usd, Unpriced: unpriced}
		return d
	}
	first := withCost(commentDecision(), 1.25, false)
	if got := pullRequestTotals(first, "", false); got.Reviews != 1 || got.CostUSD != 1.25 || got.Partial {
		t.Fatalf("first review totals = %+v", got)
	}
	firstComment := renderReviewComment("o/r", first, "")

	next := withCost(commentDecision(), 0.75, false)
	next.HeadSHA = strings.Repeat("b", 40)
	got := pullRequestTotals(next, firstComment, false)
	if got.Reviews != 2 || got.CostUSD != 2 {
		t.Fatalf("second push totals = %+v", got)
	}
	next.PullRequestTotals = got
	second := renderReviewComment("o/r", next, firstComment)
	if s, _ := parseCommentState(second); s.Round != 2 || s.CostUSD != 2 {
		t.Fatalf("state = %+v", s)
	}

	// A re-render at the same head that reused the reviews, as clarify
	// does, adds nothing; one without totals keeps the running cost.
	reused := next
	reused.PullRequestTotals = nil
	reused.Agent.Usage = &radar.ReviewUsage{ReusedRequests: 2}
	if got := pullRequestTotals(reused, second, false); got.Reviews != 2 || got.CostUSD != 2 {
		t.Fatalf("re-render totals = %+v", got)
	}
	if s, _ := parseCommentState(renderReviewComment("o/r", reused, second)); s.CostUSD != 2 {
		t.Fatalf("re-rendered state = %+v", s)
	}

	// A previous comment that could not be fetched leaves the earlier
	// reviews' cost unknown: the total is partial and stays so.
	lost := withCost(commentDecision(), 0.5, false)
	lost.HeadSHA = strings.Repeat("d", 40)
	lostTotals := pullRequestTotals(lost, "", true)
	if lostTotals.CostUSD != 0.5 || !lostTotals.Partial {
		t.Fatalf("unread totals = %+v", lostTotals)
	}
	lost.PullRequestTotals = lostTotals
	after := withCost(commentDecision(), 0.25, false)
	after.HeadSHA = strings.Repeat("e", 40)
	if got := pullRequestTotals(after, renderReviewComment("o/r", lost, ""), false); got.CostUSD != 0.75 || !got.Partial {
		t.Fatalf("totals after an unread comment = %+v", got)
	}

	// An unpriced reviewer is remembered, so the total says it is partial.
	third := withCost(commentDecision(), 0, true)
	third.HeadSHA = strings.Repeat("c", 40)
	if got := pullRequestTotals(third, second, false); got.Reviews != 3 || got.CostUSD != 2 || !got.Partial {
		t.Fatalf("unpriced totals = %+v", got)
	}
}

func TestPriorFindingsOfLeavesOutLateAndDowngradesHeld(t *testing.T) {
	got := priorFindingsOf([]stateFinding{
		{ID: "open", Severity: "P1", File: "a.go", Line: 3},
		{ID: "held", Severity: "P1", File: "a.go", Line: 9, Held: true},
		{ID: "late", Severity: "P2", File: "a.go", Line: 20, Late: true},
		{ID: "late-held", Severity: "P1", File: "a.go", Line: 30, Held: true, Late: true},
	})
	if len(got) != 2 || got[0].ID != "open" || got[0].Severity != "P1" || got[1].ID != "held" || got[1].Severity != "P2" {
		t.Fatalf("baseline = %+v", got)
	}
}

func lateDecision() radar.PullRequestReview {
	d := heldDecision()
	d.Convergence.Late = []radar.HeldFinding{
		{FindingID: "f1", File: "dir name/a.go", Line: 14, Severity: "P1", Reason: radar.HeldUnchangedLines},
		{FindingID: "f2", File: "b.go", Line: 3, Severity: "P3", Reason: radar.HeldUnchangedLines},
	}
	return d
}

func TestRenderLateFindingsApart(t *testing.T) {
	got := renderReviewComment("o/r", lateDecision(), "")
	for _, want := range []string{
		"<details><summary>Raised late on unchanged lines (2)</summary>",
		"_First raised this round on lines unchanged since `cccccccccccc`",
		"- P1 Zero limit deadlocks - [`dir name/a.go:14-16`]",
		"- P3 naming - [`b.go:3`]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "### P2 - Medium") || strings.Contains(got, "### P3 - Nit") || strings.Contains(got, "#### naming") {
		t.Fatalf("a late finding shows among the findings:\n%s", got)
	}
	s, ok := parseCommentState(got)
	if !ok || len(s.Findings) != 2 || !s.Findings[0].Late || !s.Findings[0].Held || !s.Findings[1].Late || s.Findings[1].Held {
		t.Fatalf("state = %+v", s.Findings)
	}
	// The next round's baseline leaves them out, so raised again on lines
	// still unchanged they stay apart, and the held P1 cannot block.
	if _, base := roundBaseline(s, true, strings.Repeat("d", 40)); base == nil || len(priorFindingsOf(base.Findings)) != 0 {
		t.Fatalf("baseline = %+v", base)
	}
	// Gone the next round, a late finding is not news.
	next := commentDecision()
	next.HeadSHA = strings.Repeat("d", 40)
	next.Findings = nil
	if again := renderReviewComment("o/r", next, got); strings.Contains(again, "Resolved since the last review") {
		t.Fatalf("a late finding listed as resolved:\n%s", again)
	}
}
