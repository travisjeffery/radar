package radar

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const structuredReview = `{"summary":"Adds retry to the uploader.","risk":{"level":"Medium","reason":"changes failure handling"},
"where_to_look":[{"file":"up.go","line":40,"why":"retry loop"}],
"findings":[{"severity":"p1","title":"Retry loop never backs off","file":"up.go","line":42,"end_line":0,"impact":"Hammers the API on outage.","recommendation":"Add exponential backoff.","rule_id":"go-err-01","confidence":9}],
"questions":["Is the upload idempotent?"," "],"tests_suggested":["A test that a 503 is retried with backoff."],
"signals":{"safe":[],"risk":["bug-or-logic-error"]},
"coverage":{"files_reviewed":["up.go"],"repo_access":true,"limits":"could not run the tests"},"needs_human":{"required":false,"reason":""},"confidence":8}`

func TestParseStructuredReview(t *testing.T) {
	res, err := parseACRVerdict(structuredReview)
	if err != nil {
		t.Fatal(err)
	}
	f := res.Findings[0]
	if f.Severity != "P1" || f.RuleID != "GO-ERR-01" || f.EndLine != 42 || f.Summary != "Hammers the API on outage." ||
		f.Recommendation != "Add exponential backoff." || f.Confidence != 9 || f.ID == "" {
		t.Fatalf("finding = %+v", f)
	}
	if res.Risk == nil || res.Risk.Level != "medium" || len(res.WhereToLook) != 1 || len(res.Questions) != 1 ||
		len(res.TestsSuggested) != 1 || res.Coverage == nil || !res.Coverage.RepoAccess || res.Coverage.Limits == "" {
		t.Fatalf("review = %+v", res)
	}
	if res.Accept || res.ModelAccept {
		t.Fatal("a P1 finding and a defect signal must not be accepted")
	}

	for name, bad := range map[string]string{
		"risk level":         strings.Replace(structuredReview, `"Medium"`, `"spicy"`, 1),
		"finding confidence": strings.Replace(structuredReview, `"confidence":9`, `"confidence":11`, 1),
		"old accept field":   strings.Replace(structuredReview, `"confidence":8}`, `"confidence":8,"accept":true}`, 1),
	} {
		if _, err := parseACRVerdict(bad); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}
}

func TestFindingIDIsStableAcrossWording(t *testing.T) {
	a := ReviewFinding{File: "up.go", RuleID: "GO-ERR-01", Title: "Retry loop never backs off", Line: 42}
	b := ReviewFinding{File: "up.go", RuleID: "GO-ERR-01", Title: "retry loop: never backs-off!", Line: 57}
	if FindingID(a) != FindingID(b) {
		t.Fatal("case, punctuation and line must not change a finding's id")
	}
	if c := (ReviewFinding{File: "down.go", RuleID: "GO-ERR-01", Title: a.Title}); FindingID(c) == FindingID(a) {
		t.Fatal("another file is another finding")
	}
}

func TestMergeReviewerFindings(t *testing.T) {
	glm := ACRResult{Findings: []ReviewFinding{
		{ID: "a", Severity: "P2", Title: "No backoff", File: "up.go", Line: 40, Confidence: 6, Summary: "s"},
		{ID: "b", Severity: "P3", Title: "Naming", File: "up.go", Line: 90, Summary: "s"},
	}}
	codex := ACRResult{Findings: []ReviewFinding{
		{ID: "c", Severity: "P1", Title: "Retry loop never backs off", File: "up.go", Line: 42, Confidence: 9, Summary: "s", Recommendation: "backoff"},
		{ID: "d", Severity: "P1", Title: "Token logged", File: "auth.go", Line: 10, Summary: "s"},
	}}
	got := mergeReviewerFindings([]ACRResult{glm, codex}, func(i int) string { return []string{"fireworks/glm", "codex/sol"}[i] })
	if len(got) != 3 {
		t.Fatalf("merged = %+v", got)
	}
	agreed := got[0]
	if agreed.File != "auth.go" {
		agreed = got[1]
	}
	if got[0].Severity != "P1" || got[1].Severity != "P1" || got[2].Severity != "P3" {
		t.Fatalf("must sort most severe first: %+v", got)
	}
	for _, m := range got {
		if m.File == "up.go" && m.Line <= 42 {
			if !m.Agreed() || m.Severity != "P1" || m.Confidence != 9 || m.Recommendation != "backoff" || len(m.Reviewers) != 2 {
				t.Fatalf("findings within %d lines in one file are one issue: %+v", mergeLineWindow, m)
			}
		}
	}
}

func TestOnlyRepoAccessAgentsKeepRepoAccess(t *testing.T) {
	claims := structuralVerdict()
	claims.Coverage = &ReviewCoverage{RepoAccess: true}
	policy := findingsPolicy(PullRequestModeShadow)
	policy.RequiredReviewers = 2
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0),
		namedAgent{&recordingAgent{result: claims}, "fireworks/glm"},
		repoAgent{namedAgent{&recordingAgent{result: claims}, "codex/sol"}})
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(largeFeatureInput(1))
	if got.Reviews[0].Agent.Coverage.RepoAccess || !got.Reviews[1].Agent.Coverage.RepoAccess {
		t.Fatalf("a diff-only reviewer cannot have read the repository: %+v / %+v", got.Reviews[0].Agent.Coverage, got.Reviews[1].Agent.Coverage)
	}
	if got.VerdictReason != "every gate passed" {
		t.Fatalf("verdict reason = %q", got.VerdictReason)
	}
}

type repoAgent struct{ namedAgent }

func (repoAgent) RepoAccess() bool { return true }

func TestParseRejectsIncompleteReviews(t *testing.T) {
	var full map[string]any
	if err := json.Unmarshal([]byte(structuredReview), &full); err != nil {
		t.Fatal(err)
	}
	drop := func(path ...string) string {
		var c map[string]any
		data, _ := json.Marshal(full)
		_ = json.Unmarshal(data, &c)
		m := c
		for _, p := range path[:len(path)-1] {
			if p == "0" {
				m = m["findings"].([]any)[0].(map[string]any)
				continue
			}
			m = m[p].(map[string]any)
		}
		delete(m, path[len(path)-1])
		out, _ := json.Marshal(c)
		return string(out)
	}
	for _, path := range [][]string{{"risk"}, {"findings"}, {"signals"}, {"signals", "risk"}, {"coverage", "files_reviewed"}, {"0", "impact"}, {"0", "severity"}} {
		if _, err := parseACRVerdict(drop(path...)); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("a review missing %v must be rejected, got %v", path, err)
		}
	}
	if _, err := parseACRVerdict(strings.Replace(structuredReview, `"risk":{"level":"Medium","reason":"changes failure handling"}`, `"risk":null`, 1)); err == nil {
		t.Fatal("a null risk must be rejected")
	}
}

func TestDistantFindingsWithTheSameIDStaySeparate(t *testing.T) {
	a := ReviewFinding{ID: "same", Severity: "P2", Title: "Missing error handling", File: "x.go", Line: 10, Summary: "s"}
	b := ReviewFinding{ID: "same", Severity: "P2", Title: "Missing error handling", File: "x.go", Line: 400, Summary: "t"}
	got := mergeReviewerFindings([]ACRResult{{Findings: []ReviewFinding{a}}, {Findings: []ReviewFinding{b}}}, func(i int) string { return []string{"r1", "r2"}[i] })
	if len(got) != 2 {
		t.Fatalf("findings 390 lines apart are two issues: %+v", got)
	}
}

func TestParseAssumptions(t *testing.T) {
	assumption := func(text string, loadBearing bool) string {
		return fmt.Sprintf(`{"assumption":%q,"basis":" PR description ","if_wrong":"the P1 goes away","load_bearing":%t}`, text, loadBearing)
	}
	list := []string{
		assumption("minor one", false), assumption(" ", true), assumption("second minor", false),
		assumption("The migration runs first", true), assumption("third minor", false),
		assumption("fourth minor", false), assumption("Callers pass a non-nil map", true),
	}
	review := strings.Replace(structuredReview, `"tests_suggested"`, `"assumptions":[`+strings.Join(list, ",")+`],"tests_suggested"`, 1)
	res, err := parseACRVerdict(review)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range res.Assumptions {
		got = append(got, a.Assumption)
	}
	// Load-bearing first, empty ones dropped, capped at five.
	want := []string{"The migration runs first", "Callers pass a non-nil map", "minor one", "second minor", "third minor"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("assumptions = %q, want %q", got, want)
	}
	if a := res.Assumptions[0]; !a.LoadBearing || a.Basis != "PR description" || a.IfWrong != "the P1 goes away" {
		t.Fatalf("assumption = %+v", a)
	}
	if res.ModelAccept != false || res.Confidence != 8 {
		t.Fatal("assumptions must not change the review's decision fields")
	}

	// The list is display-only: leaving it out still completes the review,
	// but an entry must be whole.
	if res, err := parseACRVerdict(structuredReview); err != nil || res.Assumptions != nil {
		t.Fatalf("review without assumptions: %+v, %v", res.Assumptions, err)
	}
	partial := strings.Replace(structuredReview, `"tests_suggested"`, `"assumptions":[{"assumption":"x","basis":"","if_wrong":""}],"tests_suggested"`, 1)
	if _, err := parseACRVerdict(partial); err == nil || !strings.Contains(err.Error(), "assumptions[0].load_bearing") {
		t.Fatalf("partial assumption: err = %v", err)
	}
}

func TestMergeReviewerAssumptions(t *testing.T) {
	glm := ACRResult{Assumptions: []ReviewAssumption{
		{Assumption: "The feature flag is off in production.", Basis: "not visible in the diff"},
		{Assumption: "Retries are idempotent on the server side.", Basis: "PR description", IfWrong: "a log line is duplicated"},
	}}
	codex := ACRResult{Assumptions: []ReviewAssumption{
		{Assumption: "The schema migration has already run in every environment.", Basis: "PR description", LoadBearing: true},
		{Assumption: "retries are idempotent on the server-side", Basis: "not visible in the diff", IfWrong: "duplicate uploads become a P1", LoadBearing: true},
	}}
	names := []string{"fireworks/glm", "codex/gpt"}
	got := mergeReviewerAssumptions([]ACRResult{glm, codex}, func(i int) string { return names[i] })
	if len(got) != 3 {
		t.Fatalf("merged = %+v", got)
	}
	// The reviewer that made it load-bearing explains why.
	shared := got[0]
	if shared.Assumption != "Retries are idempotent on the server side." || !shared.LoadBearing || shared.Basis != "not visible in the diff" ||
		shared.IfWrong != "duplicate uploads become a P1" || strings.Join(shared.Reviewers, ",") != "fireworks/glm,codex/gpt" {
		t.Fatalf("shared assumption = %+v", shared)
	}
	if !got[1].LoadBearing || got[1].Reviewers[0] != "codex/gpt" || got[2].LoadBearing {
		t.Fatalf("order = %+v", got)
	}
}

// Assumptions that share their words but disagree stay separate, so the
// comment never shows disagreement as consensus.
func TestOpposingAssumptionsStaySeparate(t *testing.T) {
	for _, pair := range [][2]string{
		{"The migration runs before the deploy.", "The deploy runs before the migration."},
		{"Callers pass a nil map.", "Callers never pass a nil map."},
		{"The cache is warmed on startup.", "The cache isn't warmed on startup."},
	} {
		results := []ACRResult{
			{Assumptions: []ReviewAssumption{{Assumption: pair[0]}}},
			{Assumptions: []ReviewAssumption{{Assumption: pair[1]}}},
		}
		if got := mergeReviewerAssumptions(results, func(i int) string { return fmt.Sprint(i) }); len(got) != 2 {
			t.Errorf("%q and %q merged: %+v", pair[0], pair[1], got)
		}
	}
	results := []ACRResult{
		{Assumptions: []ReviewAssumption{{Assumption: "Callers don't pass a nil map."}}},
		{Assumptions: []ReviewAssumption{{Assumption: "callers never pass nil maps"}}},
	}
	if got := mergeReviewerAssumptions(results, func(i int) string { return fmt.Sprint(i) }); len(got) != 1 {
		t.Errorf("negations that agree must merge: %+v", got)
	}
}
