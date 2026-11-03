package radar

import (
	"strings"
	"testing"
)

const goPackFixture = `---
id: go
version: 3
title: Go
---
# Go

- **GO-ERR-01** Wrap errors with %w.
- **GO-CTX-01** Pass the caller's context.
`

const iamPackFixture = `---
id: iam-least-privilege
version: 1
---
- **IAM-01** No wildcard actions.
`

func mustPack(t *testing.T, id, data string) ReviewPack {
	t.Helper()
	p, err := ParseReviewPack(id, []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func falsePtr() *bool { f := false; return &f }

func testCriteria(t *testing.T) ReviewCriteria {
	t.Helper()
	return ReviewCriteria{
		RulesPath: ".radar/rules.yaml", RulesSHA256: "abc",
		Rules: ReviewRules{
			Version:      1,
			RepoGuidance: []string{"CLAUDE.md"},
			Areas: []ReviewArea{
				{Name: "go", Paths: []string{"**/*.go"}, Packs: []string{"go"}},
				{Name: "iam", Paths: []string{"infra/pulumi/iam/**"}, Packs: []string{"iam-least-privilege"}, Policy: ReviewAreaPolicy{AutoApprove: falsePtr()}},
				{Name: "migrations", Paths: []string{"**/migrations/**"}, Policy: ReviewAreaPolicy{DeclineBlocks: true, MaxFindings: map[string]int{"P2": 0}}},
			},
		},
		Packs: map[string]ReviewPack{
			"go":                  mustPack(t, "go", goPackFixture),
			"iam-least-privilege": mustPack(t, "iam-least-privilege", iamPackFixture),
		},
		Guidance: map[string][]byte{
			"CLAUDE.md":              []byte("repo conventions"),
			"infra/AGENTS.md":        []byte("infra guide"),
			"infra/pulumi/AGENTS.md": []byte("pulumi guide"),
		},
	}
}

func TestParseReviewPack(t *testing.T) {
	p := mustPack(t, "go", goPackFixture)
	if p.ID != "go" || p.Version != 3 || strings.Join(p.Rules, ",") != "GO-ERR-01,GO-CTX-01" || len(p.SHA256) != 64 {
		t.Fatalf("pack = %+v", p)
	}
	for name, data := range map[string]string{
		"no front matter":    "- **GO-ERR-01** x\n",
		"unterminated":       "---\nid: go\nversion: 1\n",
		"id mismatch":        "---\nid: python\nversion: 1\n---\nx\n",
		"no version":         "---\nid: go\n---\nx\n",
		"bad version":        "---\nid: go\nversion: one\n---\nx\n",
		"unknown key":        "---\nid: go\nversion: 1\nseverity: P0\n---\nx\n",
		"duplicate rule":     "---\nid: go\nversion: 1\n---\n- **GO-1** a\n- **GO-1** b\n",
		"empty":              "---\nid: go\nversion: 1\n---\n\n",
		"prose only":         "---\nid: go\nversion: 1\n---\nWrap errors.\n",
		"line not key:value": "---\nid: go\nversion 1\n---\nx\n",
	} {
		if _, err := ParseReviewPack("go", []byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReviewRulesValidate(t *testing.T) {
	if err := testCriteria(t).Validate(); err != nil {
		t.Fatal(err)
	}
	tru := true
	for name, mutate := range map[string]func(*ReviewCriteria){
		"auto_approve true":     func(c *ReviewCriteria) { c.Rules.Areas[1].Policy.AutoApprove = &tru },
		"P1 cap above zero":     func(c *ReviewCriteria) { c.Rules.Areas[2].Policy.MaxFindings = map[string]int{"P1": 1} },
		"negative P2 cap":       func(c *ReviewCriteria) { c.Rules.Areas[2].Policy.MaxFindings = map[string]int{"P2": -1} },
		"unknown severity":      func(c *ReviewCriteria) { c.Rules.Areas[2].Policy.MaxFindings = map[string]int{"p2": 0} },
		"duplicate area":        func(c *ReviewCriteria) { c.Rules.Areas[1].Name = "go" },
		"area without effect":   func(c *ReviewCriteria) { c.Rules.Areas[2].Policy = ReviewAreaPolicy{} },
		"area without paths":    func(c *ReviewCriteria) { c.Rules.Areas[0].Paths = nil },
		"bad glob":              func(c *ReviewCriteria) { c.Rules.Areas[0].Paths = []string{"../x"} },
		"pack not loaded":       func(c *ReviewCriteria) { delete(c.Packs, "go") },
		"bad pack id":           func(c *ReviewCriteria) { c.Rules.Packs = []string{"Go"} },
		"guidance path":         func(c *ReviewCriteria) { c.Rules.RepoGuidance = []string{"../CLAUDE.md"} },
		"directory guidance":    func(c *ReviewCriteria) { c.Rules.DirectoryGuidance = "docs/AGENTS.md" },
		"no version":            func(c *ReviewCriteria) { c.Rules.Version = 0 },
		"repo guidance missing": func(c *ReviewCriteria) { delete(c.Guidance, "CLAUDE.md") },
		"rule in two packs": func(c *ReviewCriteria) {
			c.Packs["iam-least-privilege"] = mustPack(t, "iam-least-privilege", "---\nid: iam-least-privilege\nversion: 1\n---\n- **GO-ERR-01** x\n")
		},
		"pack loaded under id": func(c *ReviewCriteria) { c.Packs["iam-least-privilege"] = c.Packs["go"] },
	} {
		c := testCriteria(t)
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func allReviewed(PullRequestFile) bool { return true }

func TestSelectCriteriaPacksPolicyAndGuidance(t *testing.T) {
	c := testCriteria(t)
	files := []PullRequestFile{
		{Path: "infra/pulumi/iam/role.go"},
		{Path: "infra/cmd/main.go"},
		{Path: "README.md"},
	}
	ctx, record, policy := c.selectCriteria(files, allReviewed)
	if len(record.Areas) != 2 || record.Areas[0].Name != "go" || len(record.Areas[0].Files) != 2 || record.Areas[1].Name != "iam" {
		t.Fatalf("areas = %+v", record.Areas)
	}
	if !policy.humanRequired() || record.Policy == nil {
		t.Fatalf("iam area must require a human: %+v", policy)
	}
	if len(record.Packs) != 2 || record.Packs[0].ID != "go" || record.Packs[0].Version != 3 || record.Packs[1].ID != "iam-least-privilege" {
		t.Fatalf("packs = %+v", record.Packs)
	}
	if strings.Join(ctx.RuleIDs, ",") != "GO-ERR-01,GO-CTX-01,IAM-01" {
		t.Fatalf("rule ids = %v", ctx.RuleIDs)
	}
	// The repository guidance is the stable prefix; the selected packs and
	// the nearest guidance of each directory are the per-PR suffix.
	if ctx.Prefix != "=== guidance CLAUDE.md ===\nrepo conventions" {
		t.Fatalf("prefix = %q", ctx.Prefix)
	}
	for _, want := range []string{"review pack go v3", "GO-ERR-01", "IAM-01", "guidance infra/pulumi/AGENTS.md", "guidance infra/AGENTS.md"} {
		if !strings.Contains(ctx.Suffix, want) {
			t.Errorf("suffix lacks %q:\n%s", want, ctx.Suffix)
		}
	}
	if strings.Contains(ctx.Suffix, "CLAUDE.md") {
		t.Errorf("prefix guidance repeated in the suffix")
	}
	if len(record.Guidance) != 3 || !record.Guidance[0].Prefix || record.Guidance[1].Path != "infra/pulumi/AGENTS.md" {
		t.Fatalf("guidance = %+v", record.Guidance)
	}
}

func TestSelectCriteriaWithheldFilesSelectNoPacksButKeepPolicy(t *testing.T) {
	c := testCriteria(t)
	files := []PullRequestFile{
		{Path: "infra/pulumi/iam/zz_generated.go"},
		{Path: "docs/x.md"},
	}
	reviewed := func(f PullRequestFile) bool { return f.Path == "docs/x.md" }
	ctx, record, policy := c.selectCriteria(files, reviewed)
	if !policy.humanRequired() {
		t.Fatal("a withheld file in a human-only area must still require a human")
	}
	if len(record.Packs) != 0 || ctx.Suffix != "" {
		t.Fatalf("withheld file selected packs %+v or guidance %q", record.Packs, ctx.Suffix)
	}
}

func TestSelectCriteriaMatchesRenameSource(t *testing.T) {
	c := testCriteria(t)
	_, record, policy := c.selectCriteria([]PullRequestFile{{Path: "infra/x.txt", PreviousPath: "infra/pulumi/iam/policy.json"}}, allReviewed)
	if !policy.humanRequired() || len(record.Areas) != 1 {
		t.Fatalf("renaming out of an area must keep its policy: %+v", record)
	}
}

func TestSelectCriteriaMergesMostRestrictive(t *testing.T) {
	c := testCriteria(t)
	c.Rules.Areas = append(c.Rules.Areas, ReviewArea{Name: "schema", Paths: []string{"**/migrations/**"}, Policy: ReviewAreaPolicy{MaxFindings: map[string]int{"P2": 2, "P3": 1}}})
	_, _, policy := c.selectCriteria([]PullRequestFile{{Path: "svc/migrations/001.sql"}}, allReviewed)
	if !policy.DeclineBlocks || policy.MaxFindings["P2"] != 0 || policy.MaxFindings["P3"] != 1 || policy.humanRequired() {
		t.Fatalf("merged policy = %+v", policy)
	}
}

func TestSelectCriteriaGuidanceBudgetPrefersNearest(t *testing.T) {
	c := testCriteria(t)
	c.Rules.MaxGuidanceChars = len("repo conventions") + len("pulumi guide") + 3
	ctx, record, _ := c.selectCriteria([]PullRequestFile{{Path: "infra/pulumi/a.go"}, {Path: "infra/b.go"}}, allReviewed)
	if !strings.Contains(ctx.Suffix, "pulumi guide") {
		t.Fatalf("nearest guidance must win the budget:\n%s", ctx.Suffix)
	}
	if strings.Contains(ctx.Suffix, "infra guide") {
		t.Fatalf("budget exceeded:\n%s", ctx.Suffix)
	}
	last := record.Guidance[len(record.Guidance)-1]
	if last.Path != "infra/AGENTS.md" || !last.Truncated {
		t.Fatalf("guidance = %+v", record.Guidance)
	}
}

func criteriaPolicy() PullRequestPolicy {
	p := findingsPolicy(PullRequestModeShadow)
	p.DenyPaths = nil
	p.ReviewCriteria = &PullRequestReviewCriteria{Rules: ".radar/rules.yaml"}
	return p
}

func criteriaInput(paths ...string) PullRequestInput {
	in := largeFeatureInput(len(paths))
	for i, p := range paths {
		in.Files[i].Path = p
	}
	return in
}

func TestReviewCriteriaMustBeLoaded(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(criteriaPolicy(), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(criteriaInput("svc/a.go"))
	if got.Action != PullRequestRouteToHuman || got.Stages[len(got.Stages)-1].Name != "pr.review-criteria" {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
}

func criteriaReviewer(t *testing.T, agents ...ReviewAgent) *PullRequestReviewer {
	t.Helper()
	reviewer, err := NewPullRequestReviewer(criteriaPolicy(), fixedScorer(0), agents...)
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.UseReviewCriteria(testCriteria(t)); err != nil {
		t.Fatal(err)
	}
	return reviewer
}

func TestReviewCriteriaReachThePrompt(t *testing.T) {
	agent := &recordingAgent{result: structuralVerdict()}
	got := criteriaReviewer(t, agent).Review(criteriaInput("infra/cmd/main.go"))
	if got.Action != PullRequestWouldApprove {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	if got.Criteria == nil || len(got.Criteria.Packs) != 1 || got.Criteria.Packs[0].SHA256 == "" || got.Criteria.RulesSHA256 != "abc" {
		t.Fatalf("criteria = %+v", got.Criteria)
	}
	seen := agent.seen[0]
	system, user := ReviewSystemPrompt(seen), renderDiffForReview(seen)
	if !strings.Contains(system, reviewCriteriaInstructions) || !strings.Contains(system, "repo conventions") || strings.Contains(system, "GO-ERR-01") {
		t.Fatalf("system prompt must hold only the stable prefix:\n%s", system)
	}
	if !strings.Contains(user, "GO-ERR-01") || !strings.Contains(user, "infra guide") || strings.Index(user, "GO-ERR-01") > strings.Index(user, "--- change 1") {
		t.Fatalf("selected criteria must precede the patches:\n%s", user)
	}
}

func TestReviewWithoutCriteriaKeepsThePrompt(t *testing.T) {
	if ReviewSystemPrompt(Diff{}) != acrSystemPrompt+"\n\n"+prContextInstructions {
		t.Fatal("a review without criteria must use the plain prompt")
	}
}

func TestAreaAutoApproveFalseRoutesToHuman(t *testing.T) {
	got := criteriaReviewer(t, &recordingAgent{result: structuralVerdict()}).Review(criteriaInput("infra/pulumi/iam/role.go"))
	if got.Action != PullRequestRouteToHuman || got.Eligible {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	found := false
	for _, s := range got.Stages {
		found = found || s.Name == "pr.area-policy" && !s.Passed
	}
	if !found {
		t.Fatalf("no failed pr.area-policy stage: %+v", got.Stages)
	}
}

func TestAreaFindingLimitBlocks(t *testing.T) {
	// structuralVerdict carries one P2 finding, which the policy's
	// blocking severities let through; the migrations area caps P2 at 0.
	got := criteriaReviewer(t, &recordingAgent{result: structuralVerdict()}).Review(criteriaInput("svc/migrations/001.sql"))
	if got.Action != PullRequestRouteToHuman || !strings.Contains(got.Stages[len(got.Stages)-1].Reason, "area-finding-limit=P2=1>0") {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	if got := criteriaReviewer(t, &recordingAgent{result: structuralVerdict()}).Review(criteriaInput("svc/a.go")); got.Action != PullRequestWouldApprove {
		t.Fatalf("outside the area the P2 must not block: %s", got.Action)
	}
}

func TestAreaDeclineBlocksRemovesTheClaimWaiver(t *testing.T) {
	p := testPullRequestPolicy(PullRequestModeShadow)
	res := safeAgentResult()
	res.Accept, res.ModelAccept = false, false
	d := Diff{Changes: []Change{{File: "docs/setup.md"}}}
	if ok, _ := reviewerVerdict(p, ReviewAreaPolicy{}, 10, true, res, d); !ok {
		t.Fatal("fixture: the waiver should pass a declined review")
	}
	if ok, _ := reviewerVerdict(p, ReviewAreaPolicy{DeclineBlocks: true}, 10, true, res, d); ok {
		t.Fatal("decline_blocks must override the waiver")
	}
}

func TestAuthorTrustCannotRelaxAreaPolicy(t *testing.T) {
	p := criteriaPolicy()
	p.AuthorTrust = &PullRequestAuthorTrust{Tiers: []PullRequestTrustTier{{Tier: 2, MinReviewConfidence: 7}}}
	reviewer, err := NewPullRequestReviewer(p, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.UseReviewCriteria(testCriteria(t)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"infra/pulumi/iam/role.go", "svc/migrations/001.sql"} {
		_, audit := reviewer.ReviewWithAuthorTrust(criteriaInput(path), AuthorTrust{Tier: 3})
		if audit.TierAction != PullRequestRouteToHuman {
			t.Fatalf("%s: tier action %s", path, audit.TierAction)
		}
	}
}

func TestUnknownRuleCitationsAreCleared(t *testing.T) {
	verdict := structuralVerdict()
	verdict.Findings = []ReviewFinding{
		{Severity: "P3", Title: "a", Summary: "a", RuleID: "GO-ERR-01"},
		{Severity: "P3", Title: "b", Summary: "b", RuleID: "GO-MADE-UP-9"},
	}
	got := criteriaReviewer(t, &recordingAgent{result: verdict}).Review(criteriaInput("svc/a.go"))
	if got.Agent.Findings[0].RuleID != "GO-ERR-01" || got.Agent.Findings[1].RuleID != "" {
		t.Fatalf("findings = %+v", got.Agent.Findings)
	}
	if strings.Join(got.Criteria.UnknownRuleCitations, ",") != "GO-MADE-UP-9" {
		t.Fatalf("unknown citations = %v", got.Criteria.UnknownRuleCitations)
	}
}

func TestOversizedCriteriaFailSafe(t *testing.T) {
	p := criteriaPolicy()
	p.ReviewChunkChars = 1000
	reviewer, err := NewPullRequestReviewer(p, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	c := testCriteria(t)
	c.Guidance["CLAUDE.md"] = []byte(strings.Repeat("x", 800))
	if err := reviewer.UseReviewCriteria(c); err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(criteriaInput("svc/a.go"))
	if got.Action != PullRequestRouteToHuman || got.Stages[len(got.Stages)-1].Name != "pr.review-criteria" {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
}

func TestParseVerdictNormalisesRuleID(t *testing.T) {
	res, err := parseACRVerdict(`{"summary":"ok","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[{"severity":"P3","title":"t","file":"a.go","line":1,"end_line":1,"impact":"s","recommendation":"","rule_id":" go-err-01 ","confidence":8}],"questions":[],"tests_suggested":[],"signals":{"safe":["test-addition"],"risk":[]},"coverage":{"files_reviewed":["a.go"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Findings[0].RuleID != "GO-ERR-01" {
		t.Fatalf("rule id = %q", res.Findings[0].RuleID)
	}
}

func TestRuleCitationsWithoutCriteriaAreCleared(t *testing.T) {
	verdict := structuralVerdict()
	verdict.Findings = []ReviewFinding{{Severity: "P3", Title: "a", Summary: "a", RuleID: "TEST-CONFIG-HARDCODED-VALUE"}}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), &recordingAgent{result: verdict})
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewer.Review(largeFeatureInput(1)); got.Agent.Findings[0].RuleID != "" {
		t.Fatalf("a review without packs cannot cite a rule: %+v", got.Agent.Findings)
	}
}

func TestAreaFindingLimitIgnoresSeverityCase(t *testing.T) {
	p := ReviewAreaPolicy{MaxFindings: map[string]int{"P2": 0}}
	if p.exceedsFindingLimits([]ReviewFinding{{Severity: " p2"}}) == "" {
		t.Fatal("a lowercase p2 must count against the P2 cap")
	}
}

func TestUseReviewCriteriaRequiresThePolicysRulesFile(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(criteriaPolicy(), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	c := testCriteria(t)
	c.RulesPath = "other/rules.yaml"
	if err := reviewer.UseReviewCriteria(c); err == nil {
		t.Fatal("criteria from another rules file were accepted")
	}
}
