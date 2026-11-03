package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/travisjeffery/radar"
)

var clarifyNow = time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)

const (
	findingA = "aaaaaa111111"
	findingB = "bbbbbb222222"
)

func comment(id int64, login, typ, body string, at time.Time) prComment {
	c := prComment{ID: id, Body: body, CreatedAt: at.Format(time.RFC3339), UpdatedAt: at.Format(time.RFC3339), HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/1#c%d", id)}
	c.User.Login, c.User.Type = login, typ
	return c
}

func inline(c prComment, replyTo int64) prComment {
	c.inline, c.InReplyTo = true, replyTo
	return c
}

// findingRoot is Radar's inline comment for a finding.
func findingRoot(id int64, finding string) prComment {
	c := inline(comment(id, "github-actions[bot]", "Bot", "**P1: thing**\n\n"+inlineFindingMarker(finding), clarifyNow.Add(-time.Hour)), 0)
	c.Path, c.Line = "a.go", 10
	return c
}

func movedAway(c prComment) prComment {
	c.Path = "gone.go"
	return c
}

func radarReply(id int64, rec replyRecord, at time.Time) prComment {
	marker, _ := json.Marshal(rec)
	return comment(id, "github-actions[bot]", "Bot", "answer\n\n"+replyMarkerOpen+string(marker)+replyMarkerClose, at)
}

func basePlan() planInput {
	return planInput{
		findings: []radar.MergedFinding{
			{ReviewFinding: radar.ReviewFinding{ID: findingA, Severity: "P1", Title: "A", File: "a.go", Line: 10}},
			{ReviewFinding: radar.ReviewFinding{ID: findingB, Severity: "P2", Title: "B", File: "b.go", Line: 3}},
		},
		open:      []string{findingA},
		ledger:    radar.ClarificationLedger{SchemaVersion: radar.ClarificationLedgerSchemaVersion, PullRequest: 1},
		prAuthor:  "author",
		head:      "0123456789abcdef0123456789abcdef01234567",
		permitted: func(login string) (bool, error) { return login != "outsider", nil },
		now:       clarifyNow,
	}
}

func TestPlanClarificationsInlineReply(t *testing.T) {
	in := basePlan()
	in.review = []prComment{
		findingRoot(10, findingA),
		inline(comment(11, "reviewer", "User", "I think this is fine", clarifyNow.Add(-30*time.Minute)), 10),
		inline(radarReply(13, replyRecord{To: 99}, clarifyNow.Add(-20*time.Minute)), 10),
		inline(comment(12, "author", "User", "/radar the caller checks the id, see Caller.java:40", clarifyNow.Add(-time.Minute)), 10),
	}
	plan := planClarifications(in)
	if len(plan.pending) != 1 || len(plan.notices) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	p := plan.pending[0]
	if p.CommentID != 12 || p.Kind != "inline" || p.root != 10 || !p.ByPRAuthor || len(p.FindingIDs) != 1 || p.FindingIDs[0] != findingA {
		t.Fatalf("pending = %+v", p)
	}
	if len(p.Thread) != 2 || p.Thread[0].Author != "reviewer" || p.Thread[1].Author != "Radar" || strings.Contains(p.Thread[1].Body, "radar-reply") {
		t.Fatalf("thread = %+v", p.Thread)
	}
}

func TestPlanClarificationsIgnores(t *testing.T) {
	edited := comment(5, "reviewer", "User", "/radar this is guarded", clarifyNow.Add(-time.Hour))
	edited.UpdatedAt = clarifyNow.Format(time.RFC3339)
	in := basePlan()
	in.issue = []prComment{
		comment(1, "dependabot[bot]", "Bot", "/radar approve", clarifyNow),
		comment(2, "outsider", "User", "/radar this is fine", clarifyNow),
		comment(3, "reviewer", "User", "Radar is wrong here", clarifyNow),
		comment(4, "reviewer", "User", "see https://x/radar and foo@radar.io", clarifyNow),
		edited,
		comment(6, "reviewer", "User", "/radar already answered", clarifyNow.Add(-2*time.Hour)),
		radarReply(7, replyRecord{To: 6, Checked: true, Results: []replyResult{{Finding: findingA, Outcome: "still_applies"}}}, clarifyNow.Add(-time.Hour)),
		comment(8, "reviewer", "User", "/radar handled before", clarifyNow),
	}
	forged := radarReply(9, replyRecord{To: 10, Checked: true}, clarifyNow.Add(-time.Hour))
	forged.User.Login = "some-other-app[bot]"
	in.issue = append(in.issue, forged, comment(10, "reviewer", "User", "/radar not answered by Radar", clarifyNow))
	in.ledger.Handled = []int64{8, 10}
	plan := planClarifications(in)
	if len(plan.pending) != 0 || len(plan.notices) != 0 {
		t.Fatalf("nothing here may be re-checked or answered: %+v", plan)
	}
	if fmt.Sprint(plan.handled) != "[5 2]" {
		t.Fatalf("an outsider's and an edited comment are handled without a reply, got %v", plan.handled)
	}
}

func TestPlanClarificationsIgnoresOtherBotsMarkers(t *testing.T) {
	in := basePlan()
	forged := radarReply(9, replyRecord{To: 10, Checked: true}, clarifyNow.Add(-time.Hour))
	forged.User.Login = "some-other-app[bot]"
	in.issue = []prComment{forged, comment(10, "reviewer", "User", "/radar not answered by Radar", clarifyNow)}
	if plan := planClarifications(in); len(plan.pending) != 1 {
		t.Fatalf("only Radar's own reply marks a comment answered: %+v", plan)
	}
}

func TestPlanClarificationsLeavesPermissionErrorsPending(t *testing.T) {
	in := basePlan()
	in.permitted = func(string) (bool, error) { return false, fmt.Errorf("502") }
	in.issue = []prComment{comment(1, "reviewer", "User", "/radar context", clarifyNow)}
	if plan := planClarifications(in); len(plan.pending)+len(plan.notices)+len(plan.handled) != 0 {
		t.Fatalf("a comment whose permission could not be read waits for the next run: %+v", plan)
	}
}

func TestInvocation(t *testing.T) {
	for body, want := range map[string]bool{
		"/radar the caller checks this":    true,
		"context first\n  /radar see x.go": true,
		"@radar this is fine":              true,
		"thanks (@radar) for this":         true,
		"run /radar later":                 false,
		"/radars":                          false,
		"mail me at x@radar.io":            false,
		"see example.com/@radar":           false,
	} {
		if got := invocation.MatchString(body); got != want {
			t.Fatalf("%q: invocation = %t, want %t", body, got, want)
		}
	}
}

func TestPlanClarificationsTopLevel(t *testing.T) {
	in := basePlan()
	in.issue = []prComment{
		comment(1, "reviewer", "User", "/radar bbbbbb is a known pattern here", clarifyNow),
		comment(2, "reviewer", "User", "/radar the retries are bounded upstream", clarifyNow.Add(time.Second)),
	}
	plan := planClarifications(in)
	if len(plan.pending) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	if got := plan.pending[0].FindingIDs; len(got) != 1 || got[0] != findingB {
		t.Fatalf("a named finding is re-checked, whatever its severity: %v", got)
	}
	if got := plan.pending[1].FindingIDs; len(got) != 1 || got[0] != findingA {
		t.Fatalf("naming no finding means every open P0/P1: %v", got)
	}

	in.open = nil
	in.issue = in.issue[1:]
	plan = planClarifications(in)
	if len(plan.pending) != 0 || len(plan.notices) != 1 || !strings.Contains(plan.notices[0].Body, "no open P0 or P1") {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanClarificationsNotices(t *testing.T) {
	in := basePlan()
	in.review = []prComment{
		inline(comment(20, "human", "User", "some review", clarifyNow.Add(-time.Hour)), 0),
		inline(comment(21, "reviewer", "User", "/radar what about this", clarifyNow), 20),
		movedAway(findingRoot(30, "cccccc333333")),
		inline(comment(31, "reviewer", "User", "/radar fixed now", clarifyNow), 30),
	}
	plan := planClarifications(in)
	if len(plan.pending) != 0 || len(plan.notices) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	if n := plan.notices[0]; n.Kind != "inline" || n.InReplyTo != 20 || !strings.Contains(n.Body, "its own findings") {
		t.Fatalf("notice = %+v", n)
	}
	if n := plan.notices[1]; n.InReplyTo != 30 || !strings.Contains(n.Body, "no longer reports") {
		t.Fatalf("notice = %+v", n)
	}
}

func TestPlanClarificationsFollowsAMovedFinding(t *testing.T) {
	in := basePlan()
	root := findingRoot(10, "dddddd444444")
	root.Line = 0
	root.OriginalLine = 12
	in.review = []prComment{root, inline(comment(11, "reviewer", "User", "/radar this is guarded", clarifyNow), 10)}
	plan := planClarifications(in)
	if len(plan.pending) != 1 || plan.pending[0].FindingIDs[0] != findingA {
		t.Fatalf("a finding moved by a push is still the thread's finding: %+v", plan)
	}
}

func TestPlanClarificationsLimits(t *testing.T) {
	in := basePlan()
	in.review = []prComment{findingRoot(10, findingA)}
	for i := range maxClarificationsPerFinding {
		in.review = append(in.review, radarReply(int64(100+i), replyRecord{To: int64(200 + i), Checked: true, Results: []replyResult{{Finding: findingA, Outcome: "still_applies"}}}, clarifyNow.Add(-48*time.Hour)))
	}
	in.review = append(in.review,
		inline(comment(300, "reviewer", "User", "/radar once more", clarifyNow), 10),
		inline(comment(301, "reviewer", "User", "/radar and again", clarifyNow.Add(time.Second)), 10))
	plan := planClarifications(in)
	if len(plan.pending) != 0 || len(plan.notices) != 1 || !strings.Contains(plan.notices[0].Body, "its limit") {
		t.Fatalf("plan = %+v", plan)
	}
	if fmt.Sprint(plan.handled) != "[301]" {
		t.Fatalf("the limit is announced once; later comments are handled quietly: %v", plan.handled)
	}

	day := basePlan()
	for i := range maxClarificationsPerDay {
		day.issue = append(day.issue, radarReply(int64(100+i), replyRecord{To: int64(200 + i), Checked: true, Results: []replyResult{{Finding: "other", Outcome: "still_applies"}}}, clarifyNow.Add(-time.Hour)))
	}
	day.issue = append(day.issue, comment(400, "reviewer", "User", "/radar one more", clarifyNow))
	plan = planClarifications(day)
	if len(plan.pending) != 0 || len(plan.notices) != 1 || !strings.Contains(plan.notices[0].Body, "daily limit") {
		t.Fatalf("plan = %+v", plan)
	}
	yesterday := basePlan()
	yesterday.issue = append(day.issue[:0:0], day.issue...)
	yesterday.now = clarifyNow.Add(24 * time.Hour)
	yesterday.issue[len(yesterday.issue)-1] = comment(400, "reviewer", "User", "/radar one more", yesterday.now)
	if plan := planClarifications(yesterday); len(plan.pending) != 1 {
		t.Fatalf("the daily limit resets at midnight UTC: %+v", plan)
	}
}

func TestClarifyReplies(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	findings := map[string]radar.MergedFinding{findingA: {ReviewFinding: radar.ReviewFinding{ID: findingA, Severity: "P1", Title: "Unchecked id"}}}
	pending := []pendingClarification{
		{Clarification: radar.Clarification{CommentID: 12, Kind: "inline", Author: "author", FindingIDs: []string{findingA}}, root: 10},
		{Clarification: radar.Clarification{CommentID: 13, Kind: "issue", Author: "author", FindingIDs: []string{findingA}}, url: "https://github.com/o/r/pull/1#c13"},
	}
	outcomes := []radar.FindingOutcome{
		{FindingID: findingA, CommentID: 12, Outcome: radar.OutcomeWithdrawn, FromSeverity: "P1", Severity: "P1", Basis: radar.BasisVerified, Reply: "Checked by @caller", Evidence: []radar.EvidenceRef{{File: "Caller.java", Line: 40, EndLine: 42}}},
		{FindingID: findingA, CommentID: 13, Outcome: radar.OutcomeResolvedByAssertion, FromSeverity: "P1", Severity: "P1", Basis: radar.BasisAssertion, Author: "author", Reply: "Depends on prod config", Steering: true},
	}
	replies := clarifyReplies("o/r", head, pending, outcomes, findings)
	if len(replies) != 2 {
		t.Fatalf("replies = %+v", replies)
	}
	in := replies[0]
	if in.Kind != "inline" || in.InReplyTo != 10 || !strings.Contains(in.Body, "**Withdrawn**") || !strings.Contains(in.Body, "Caller.java#L40-L42") || strings.Contains(in.Body, "@caller") {
		t.Fatalf("inline reply = %+v", in)
	}
	top := replies[1]
	for _, want := range []string{"Re: [author's comment](https://github.com/o/r/pull/1#c13)", "P1 Unchecked id (`aaaaaa`)", "**Resolved by assertion**", "a person must accept it", "did not follow it"} {
		if !strings.Contains(top.Body, want) {
			t.Fatalf("top-level reply missing %q:\n%s", want, top.Body)
		}
	}
	m := replyMarkerRE.FindStringSubmatch(top.Body)
	var rec replyRecord
	if m == nil || json.Unmarshal([]byte(m[1]), &rec) != nil || rec.To != 13 || !rec.Checked || rec.Results[0].Outcome != radar.OutcomeResolvedByAssertion {
		t.Fatalf("marker = %v", m)
	}
	failed := failedReplies(pending[:1], "the re-check failed")
	if len(failed) != 1 || !strings.Contains(failed[0].Body, "The finding stands") || !strings.Contains(failed[0].Body, `"checked":true`) {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestEvidenceChecker(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("1\n2\n3\n4\n5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("1\n2\n3\n"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "link"))
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("1\n"), 0o644)
	ok := evidenceChecker(dir)
	for _, tc := range []struct {
		ref  radar.EvidenceRef
		want bool
	}{
		{radar.EvidenceRef{File: "src/a.go", Line: 2, EndLine: 5}, true},
		{radar.EvidenceRef{File: "src/a.go", Line: 5}, true},
		{radar.EvidenceRef{File: "src/a.go", Line: 4, EndLine: 9}, false},
		{radar.EvidenceRef{File: "src/a.go", Line: 0}, false},
		{radar.EvidenceRef{File: "src/missing.go", Line: 1}, false},
		{radar.EvidenceRef{File: "../" + filepath.Base(dir) + "/src/a.go", Line: 1}, false},
		{radar.EvidenceRef{File: "/etc/passwd", Line: 1}, false},
		{radar.EvidenceRef{File: "link", Line: 1}, false},
		{radar.EvidenceRef{File: "src", Line: 1}, false},
		{radar.EvidenceRef{File: ".git/config", Line: 1}, false},
	} {
		if got := ok(tc.ref); got != tc.want {
			t.Fatalf("%+v: %t, want %t", tc.ref, got, tc.want)
		}
	}
	if evidenceChecker("")(radar.EvidenceRef{File: "src/a.go", Line: 1}) {
		t.Fatal("without a checkout nothing is verified")
	}
}

func TestReviewCommentShowsClarifications(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	d := radar.PullRequestReview{
		HeadSHA: head, Stage: radar.StageFindings,
		Reviews: []radar.PullRequestReviewerVerdict{{Reviewer: "codex/openai.gpt-6.1-sol", Agent: radar.ACRResult{Summary: "ok", Confidence: 9}}},
		Findings: []radar.MergedFinding{
			{ReviewFinding: radar.ReviewFinding{ID: findingA, Severity: "P1", Title: "Withdrawn one", File: "a.go", Line: 1, Summary: "x"}, Reviewers: []string{"codex/openai.gpt-6.1-sol"}},
			{ReviewFinding: radar.ReviewFinding{ID: findingB, Severity: "P1", Title: "Downgraded one", File: "b.go", Line: 1, Summary: "x"}, Reviewers: []string{"codex/openai.gpt-6.1-sol"}},
			{ReviewFinding: radar.ReviewFinding{ID: "cccccc333333", Severity: "P0", Title: "Asserted one", File: "c.go", Line: 1, Summary: "x"}, Reviewers: []string{"codex/openai.gpt-6.1-sol"}},
		},
		Clarifications: []radar.AppliedClarification{
			{FindingID: findingA, File: "a.go", Line: 1, Outcome: radar.OutcomeWithdrawn, FromSeverity: "P1", Severity: "P1", Author: "author", Head: head},
			{FindingID: findingB, File: "b.go", Line: 1, Outcome: radar.OutcomeDowngraded, FromSeverity: "P1", Severity: "P3", Author: "author", Head: head},
			{FindingID: "cccccc333333", File: "c.go", Line: 1, Outcome: radar.OutcomeResolvedByAssertion, FromSeverity: "P0", Severity: "P0", Author: "author", Head: head},
		},
	}
	got := renderReviewComment("o/r", d, "")
	for _, want := range []string{
		"starting a line with `/radar`",
		"### P0 - Blocker\n\n#### Asserted one `resolved by assertion`",
		"a person must accept it before Radar would approve",
		"### P3 - Nit\n\n#### Downgraded one `downgraded from P1 after clarification`",
		"<details><summary>Withdrawn after clarification (1)</summary>",
		"- P1 Withdrawn one (`aaaaaa`), after author's clarification",
		"ID `bbbbbb`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "### P1 - High") {
		t.Fatalf("no finding is left at P1:\n%s", got)
	}
	files := []radar.PullRequestFile{{Path: "a.go", Patch: "@@ -0,0 +1 @@\n+x"}, {Path: "b.go", Patch: "@@ -0,0 +1 @@\n+x"}, {Path: "c.go", Patch: "@@ -0,0 +1 @@\n+x"}}
	posted := inlineComments(d, files)
	if len(posted) != 1 || posted[0].Path != "c.go" || !strings.Contains(posted[0].Body, "`/radar`") {
		t.Fatalf("withdrawn and downgraded findings are not posted inline: %+v", posted)
	}
}

func TestPlanClarificationsUnknownReference(t *testing.T) {
	in := basePlan()
	in.issue = []prComment{
		comment(1, "reviewer", "User", "/radar a1b2c3 is guarded upstream", clarifyNow),
		comment(2, "reviewer", "User", "/radar this facade is fine, see commit ee4fd81", clarifyNow.Add(time.Second)),
	}
	plan := planClarifications(in)
	if len(plan.notices) != 1 || !strings.Contains(plan.notices[0].Body, "no finding with the ID `a1b2c3` at `0123456789ab`") {
		t.Fatalf("a mistyped ID gets a notice, not every finding: %+v", plan)
	}
	if len(plan.pending) != 1 || plan.pending[0].CommentID != 2 || plan.pending[0].FindingIDs[0] != findingA {
		t.Fatalf("words and commit SHAs are not finding IDs: %+v", plan.pending)
	}
}

// Radar shows commits as twelve hex characters, the length of a full finding
// ID, and a comment naming the reviewed commit got "no current finding".
func TestPlanClarificationsNamedCommits(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "named-commits-comment.md"))
	if err != nil {
		t.Fatal(err)
	}
	in := basePlan()
	in.head = "2dce474a2474e3eacf54bcdc950e8f6151681e03"
	in.commits = []string{in.head, "eb91a6c1085c0e029f6080575e47be6ae3bf98bd"}
	in.issue = []prComment{comment(1, "reviewer", "User", string(body), clarifyNow)}
	plan := planClarifications(in)
	if len(plan.notices) != 0 || len(plan.pending) != 1 || fmt.Sprint(plan.pending[0].FindingIDs) != "["+findingA+"]" {
		t.Fatalf("a named commit is not a finding ID; naming none re-checks every open P0/P1: %+v", plan)
	}

	in.open = nil
	plan = planClarifications(in)
	if len(plan.notices) != 1 || !strings.Contains(plan.notices[0].Body, "already reviewed `2dce474a2474`") {
		t.Fatalf("with nothing open, the notice names the head Radar reviewed: %+v", plan)
	}

	in.issue = []prComment{comment(2, "reviewer", "User", "/radar `87b679` and `87b679` are fixed in `eb91a6c1085c`", clarifyNow)}
	plan = planClarifications(in)
	if len(plan.notices) != 1 || !strings.Contains(plan.notices[0].Body, "no finding with the ID `87b679` at") {
		t.Fatalf("a stale ID is named once and the commit not at all: %+v", plan)
	}
}

func TestPlanClarificationsReportsLimitedFindings(t *testing.T) {
	in := basePlan()
	for i := range maxClarificationsPerFinding {
		in.issue = append(in.issue, radarReply(int64(100+i), replyRecord{To: int64(200 + i), Checked: true, Results: []replyResult{{Finding: findingB, Outcome: "still_applies"}}}, clarifyNow.Add(-48*time.Hour)))
	}
	in.issue = append(in.issue, comment(300, "reviewer", "User", "/radar aaaaaa and bbbbbb are both guarded", clarifyNow))
	plan := planClarifications(in)
	if len(plan.pending) != 1 || fmt.Sprint(plan.pending[0].FindingIDs) != "["+findingA+"]" || fmt.Sprint(plan.pending[0].limited) != "["+findingB+"]" {
		t.Fatalf("plan = %+v", plan)
	}
	findings := map[string]radar.MergedFinding{}
	for _, f := range in.findings {
		findings[f.ID] = f
	}
	outcomes := []radar.FindingOutcome{{FindingID: findingA, CommentID: 300, Outcome: radar.OutcomeStillApplies, Reply: "stands"}}
	replies := clarifyReplies("o/r", in.head, plan.pending, outcomes, findings)
	if len(replies) != 1 || !strings.Contains(replies[0].Body, "(`bbbbbb`): **Not re-checked**") || !strings.Contains(replies[0].Body, `{"finding":"`+findingB+`","outcome":"limit"}`) {
		t.Fatalf("a named finding past its limit is reported: %+v", replies)
	}
}

func TestThreadBodyKeepsNewestReplies(t *testing.T) {
	type node = struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Body string `json:"body"`
	}
	nodes := []node{{Body: strings.Repeat("finding ", 500)}}
	for i := range 8 {
		n := node{Body: fmt.Sprintf("reply %d %s", i, strings.Repeat("x", 300))}
		n.Author.Login = "dev"
		nodes = append(nodes, n)
	}
	got := threadBody(nodes)
	if len(got) > 1500 || !strings.Contains(got, "reply 7") || strings.Contains(got, "reply 0") || !strings.Contains(got, "earlier replies omitted") {
		t.Fatalf("len %d: %q", len(got), got)
	}
}
