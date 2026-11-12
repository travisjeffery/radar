package radar

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func findingsPolicy(mode PullRequestMode) PullRequestPolicy {
	p := testPullRequestPolicy(mode)
	p.ApprovalBasis = ApprovalBasisReviewFindings
	p.AllowRules = nil
	p.MaxFiles, p.MaxChangedLines = 0, 0
	p.MaxRiskPercentile = 0
	p.MinReviewConfidence = 8
	p.BlockingFindingSeverities = []string{"P0", "P1"}
	return p
}

// recordingAgent returns result with ReviewedFiles set to the files it was
// shown, and remembers every diff it received.
type recordingAgent struct {
	mu     sync.Mutex
	result ACRResult
	seen   []Diff
}

func (a *recordingAgent) Review(d Diff) ACRResult {
	a.mu.Lock()
	a.seen = append(a.seen, d)
	a.mu.Unlock()
	res := a.result
	res.ReviewedFiles = nil
	for _, c := range d.Changes {
		res.ReviewedFiles = append(res.ReviewedFiles, c.File)
	}
	return res
}

func largeFeatureInput(files int) PullRequestInput {
	in := safePullRequestInput()
	in.Files = nil
	for i := range files {
		in.Files = append(in.Files, PullRequestFile{
			Path: fmt.Sprintf("service/src/main/java/Feature%d.java", i), Status: "added",
			Additions: 100, Patch: "@@ -0,0 +1,100 @@\n" + strings.Repeat("+line\n", 99) + "+line",
			ContentComplete: true,
		})
	}
	return in
}

// structuralVerdict is a large feature with no defects: high review effort,
// which does not block, and a P2 finding.
func structuralVerdict() ACRResult {
	return ACRResult{
		ModelAccept: true,
		Confidence:  9,
		RiskSignals: []ChangeSignal{SignalHighReviewEffort},
		Findings:    []ReviewFinding{{Severity: "P2", Title: "naming", Summary: "consider renaming"}},
		Summary:     "new feature, no defects found",
	}
}

func TestReviewFindingsApprovesLargeChangeWithoutDefects(t *testing.T) {
	agent := &recordingAgent{result: structuralVerdict()}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(1000), agent)
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(largeFeatureInput(40))
	if got.Action != PullRequestWouldApprove || !got.Eligible || got.ApprovalBasis != ApprovalBasisReviewFindings {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	for _, stage := range got.Stages {
		if stage.Name == "pr.allow-rule" || stage.Name == "pr.risk-threshold" {
			t.Fatalf("review-findings must not run %s", stage.Name)
		}
	}
}

func TestReviewFindingsBlocks(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ACRResult)
	}{
		{"P1 finding", func(r *ACRResult) {
			r.Findings = append(r.Findings, ReviewFinding{Severity: "P1", Title: "bug", Summary: "breaks"})
		}},
		{"defect signal", func(r *ACRResult) { r.RiskSignals = append(r.RiskSignals, SignalBugOrLogicError) }},
		{"low confidence", func(r *ACRResult) { r.Confidence = 7 }},
		{"agent error", func(r *ACRResult) { *r = ACRResult{Summary: "ACR OpenAI error, failing safe"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict := structuralVerdict()
			tt.mutate(&verdict)
			reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), &recordingAgent{result: verdict})
			if err != nil {
				t.Fatal(err)
			}
			if got := reviewer.Review(largeFeatureInput(3)); got.Action != PullRequestRouteToHuman {
				t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
			}
		})
	}
}

func TestReviewFindingsRequiresFullCoverage(t *testing.T) {
	verdict := structuralVerdict()
	verdict.ReviewedFiles = []string{"service/src/main/java/Feature0.java"}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), fixedVerdictAgent{verdict})
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewer.Review(largeFeatureInput(2)); got.Action != PullRequestRouteToHuman {
		t.Fatalf("partial coverage must route to human, got %s", got.Action)
	}
}

func TestReviewFindingsKeepsDenyPaths(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(2)
	in.Files[1].Path = "service/security/Filter.java"
	if got := reviewer.Review(in); got.Action != PullRequestRouteToHuman {
		t.Fatalf("deny path must route to human, got %s", got.Action)
	}
}

func TestReviewFindingsPolicyRejectsSizeLimits(t *testing.T) {
	p := findingsPolicy(PullRequestModeShadow)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.MaxFiles = 20
	if err := p.Validate(); err == nil {
		t.Fatal("global limits must be rejected under review-findings")
	}
	p = findingsPolicy(PullRequestModeShadow)
	p.AllowRules = testPullRequestPolicy(PullRequestModeShadow).AllowRules
	if err := p.Validate(); err == nil {
		t.Fatal("allow rules must be rejected under review-findings")
	}
	p = findingsPolicy(PullRequestModeShadow)
	p.ApprovalBasis = "vibes"
	if err := p.Validate(); err == nil {
		t.Fatal("unknown basis must be rejected")
	}
}

type namedAgent struct {
	*recordingAgent
	name string
}

func (a namedAgent) Describe() string { return a.name }

func TestReviewConsensusRequiresEveryReviewer(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.RequiredReviewers = 2
	pass := namedAgent{&recordingAgent{result: structuralVerdict()}, "openai/a"}
	blocked := structuralVerdict()
	blocked.Findings = []ReviewFinding{{Severity: "P0", Title: "data loss", Summary: "drops rows"}}
	fail := namedAgent{&recordingAgent{result: blocked}, "fireworks/b"}

	if _, err := NewPullRequestReviewer(policy, fixedScorer(0), pass); err == nil {
		t.Fatal("fewer agents than required_reviewers must be rejected")
	}

	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), pass, fail)
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(largeFeatureInput(2))
	if got.Action != PullRequestRouteToHuman {
		t.Fatalf("one blocking reviewer must route to human, got %s", got.Action)
	}
	if len(got.Reviews) != 2 || !got.Reviews[0].Passed || got.Reviews[1].Passed || got.Reviewer != "openai/a+fireworks/b" {
		t.Fatalf("reviews = %+v reviewer=%q", got.Reviews, got.Reviewer)
	}
	if len(got.Agent.Findings) != 2 {
		t.Fatalf("merged verdict must carry every finding, got %+v", got.Agent.Findings)
	}

	reviewer, err = NewPullRequestReviewer(policy, fixedScorer(0), pass, namedAgent{&recordingAgent{result: structuralVerdict()}, "fireworks/b"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewer.Review(largeFeatureInput(2)); got.Action != PullRequestWouldApprove {
		t.Fatalf("agreeing reviewers must pass, got %s; stages %+v", got.Action, got.Stages)
	}
}

func stackedInput() PullRequestInput {
	in := largeFeatureInput(2)
	in.Number = 44
	in.BaseRef = "dev/feature-1"
	in.Stack = []PullRequestStackEntry{
		{Number: 43, HeadRef: "dev/feature-1", HeadSHA: strings.Repeat("c", 40), BaseRef: "dev/feature-0"},
		{Number: 42, HeadRef: "dev/feature-0", HeadSHA: strings.Repeat("b", 40), BaseRef: "main"},
	}
	return in
}

func TestStackedPullRequests(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewer.Review(stackedInput()); got.Action != PullRequestRouteToHuman {
		t.Fatalf("stacks must be opt-in, got %s", got.Action)
	}

	policy.AllowStacked = true
	reviewer, err = NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(stackedInput())
	if got.Action != PullRequestWouldApprove || len(got.Stack) != 2 || got.Stack[0].Number != 43 {
		t.Fatalf("got %s stack=%+v; stages %+v", got.Action, got.Stack, got.Stages)
	}

	broken := stackedInput()
	broken.Stack[1].HeadRef = "someone/else"
	if got := reviewer.Review(broken); got.Action != PullRequestRouteToHuman {
		t.Fatalf("a broken chain must route to human, got %s", got.Action)
	}
	offMain := stackedInput()
	offMain.Stack[1].BaseRef = "release/1"
	if got := reviewer.Review(offMain); got.Action != PullRequestRouteToHuman {
		t.Fatalf("a stack off a disallowed base must route to human, got %s", got.Action)
	}
	cycle := stackedInput()
	cycle.Stack[1].Number = 44
	if got := reviewer.Review(cycle); got.Action != PullRequestRouteToHuman {
		t.Fatalf("a stack containing the PR itself must route to human, got %s", got.Action)
	}
}

func TestStackedPullRequestIsNeverApproved(t *testing.T) {
	policy := findingsPolicy(PullRequestModeApprove)
	policy.AllowStacked = true
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(stackedInput())
	if got.Action != PullRequestWouldApprove {
		t.Fatalf("approve mode must stop at would-approve for a stacked PR, got %s", got.Action)
	}
	unstacked := largeFeatureInput(2)
	if got := reviewer.Review(unstacked); got.Action != PullRequestApprove {
		t.Fatalf("the same change on main must approve, got %s; stages %+v", got.Action, got.Stages)
	}
}

const gitattributesFixture = `# comment
*.sh text eol=lf
gradle.lockfile linguist-generated=true
/python-models/src/models/** linguist-generated
**/*_gen.go linguist-generated
docs/generated.md linguist-generated
docs/generated.md -linguist-generated
`

func TestParseGitattributesGenerated(t *testing.T) {
	got, err := ParseGitattributesGenerated([]byte(gitattributesFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []GitattributesRule{
		{"**/gradle.lockfile", true}, {"python-models/src/models/**", true}, {"**/*_gen.go", true},
		{"docs/generated.md", true}, {"docs/generated.md", false},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := ParseGitattributesGenerated([]byte(`"quoted path" linguist-generated`)); err == nil {
		t.Fatal("quoted patterns must be rejected, not misread")
	}
}

func TestGitattributesLastMatchWins(t *testing.T) {
	m := newGeneratedMatcher(PullRequestGeneratedFiles{}, mustRules(t, "*.go linguist-generated\nhandwritten/*.go -linguist-generated\n"))
	modified := func(path string) PullRequestFile { return PullRequestFile{Path: path, Status: "modified"} }
	if reason, _ := m.classify(modified("api/client.go")); reason != "path" {
		t.Fatal("api/client.go should be generated")
	}
	if reason, _ := m.classify(modified("handwritten/auth.go")); reason != "" {
		t.Fatal("a later unset for a different pattern must win, as in git")
	}
}

func TestGeneratedFilesPolicyRequiresRootGitattributes(t *testing.T) {
	p := generatedPolicy()
	p.GeneratedFiles.Gitattributes = "api/.gitattributes"
	if err := p.Validate(); err == nil {
		t.Fatal("a nested .gitattributes must be rejected: its patterns are relative to its directory")
	}
	p.GeneratedFiles.Gitattributes = ".gitattributes"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func mustRules(t *testing.T, data string) []GitattributesRule {
	t.Helper()
	rules, err := ParseGitattributesGenerated([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestNewFileAtGeneratedPathIsReviewed(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(generatedPolicy(), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.UseGitattributes([]byte(gitattributesFixture)); err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	in.Files = append(in.Files, PullRequestFile{
		Path: "internal/auth/bypass_gen.go", Status: "added", Additions: 1,
		Patch: "@@ -0,0 +1 @@\n+package auth", ContentComplete: true,
	})
	if got := reviewer.Review(in); len(got.Withheld) != 0 {
		t.Fatalf("a new file must be reviewed even under a generated name: %+v", got.Withheld)
	}
}

// An explicit decline blocks approval under review-findings regardless of
// findings, signals or confidence: Codex declined 7 replayed
// PRs as needing a human while raising only structural or effort signals.
// The verdict comes from the review's fields, not an accept claim:
// a structural change or a failed review blocks; a review that
// raised nothing blocking passes whatever ModelAccept says.
func TestReviewFindingsVerdictComesFromTheFields(t *testing.T) {
	structural := structuralVerdict()
	structural.RiskSignals = append(structural.RiskSignals, SignalStructuralChange)
	failed := structuralVerdict()
	failed.Failed = true
	unclaimed := structuralVerdict()
	unclaimed.ModelAccept = false
	for _, tt := range []struct {
		name    string
		verdict ACRResult
		want    PullRequestAction
		reason  string
	}{
		{"structural change", structural, PullRequestRouteToHuman, "structural-change=true"},
		{"failed review", failed, PullRequestRouteToHuman, ""},
		{"no accept claim, nothing blocking", unclaimed, PullRequestWouldApprove, "structural-change=false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy := findingsPolicy(PullRequestModeShadow)
			policy.RequiredReviewers = 2
			reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0),
				namedAgent{&recordingAgent{result: structuralVerdict()}, "fireworks/glm"},
				namedAgent{&recordingAgent{result: tt.verdict}, "codex/openai.gpt-6.1-sol"})
			if err != nil {
				t.Fatal(err)
			}
			got := reviewer.Review(largeFeatureInput(1))
			if got.Action != tt.want || !strings.Contains(got.Reviews[1].Reason, tt.reason) {
				t.Fatalf("got %s; %s", got.Action, got.Reviews[1].Reason)
			}
			if tt.want == PullRequestRouteToHuman && !strings.Contains(got.VerdictReason, "pr.review-agent") {
				t.Fatalf("verdict reason = %q", got.VerdictReason)
			}
		})
	}
}

func TestReviewFindingsAcceptingReviewPasses(t *testing.T) {
	accepted := ACRResult{ModelAccept: true, Confidence: 9, Summary: "fine"}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), &recordingAgent{result: accepted})
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewer.Review(largeFeatureInput(1)); got.Action != PullRequestWouldApprove {
		t.Fatalf("an accepting clean review must pass, got %s", got.Action)
	}
}

func generatedPolicy() PullRequestPolicy {
	p := findingsPolicy(PullRequestModeShadow)
	p.GeneratedFiles = &PullRequestGeneratedFiles{
		Paths:         []string{"**/*.snap"},
		HeaderMarkers: []string{`^// Code generated .* DO NOT EDIT\.$`, `@generated`},
		Note:          "CI verifies lockfiles are in sync.",
	}
	return p
}

func TestGeneratedFilesWithheld(t *testing.T) {
	agent := &recordingAgent{result: structuralVerdict()}
	reviewer, err := NewPullRequestReviewer(generatedPolicy(), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.UseGitattributes([]byte(gitattributesFixture)); err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	in.Files = append(in.Files,
		PullRequestFile{Path: "service/gradle.lockfile", Status: "modified", Additions: 500, Deletions: 400, Patch: "@@ -1 +1 @@\n-a\n+b", ContentComplete: true},
		PullRequestFile{Path: "ui/__snapshots__/a.snap", Status: "modified", Additions: 3, Patch: "@@ -1 +1 @@\n+x", ContentComplete: true},
	)
	got := reviewer.Review(in)
	if got.Action != PullRequestWouldApprove {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	if len(got.Withheld) != 2 || got.Withheld[0].Path != "service/gradle.lockfile" || got.Withheld[0].Additions != 500 {
		t.Fatalf("withheld = %+v", got.Withheld)
	}
	seen := agent.seen[0]
	if len(seen.Changes) != 1 || len(seen.Withheld) != 2 || len(seen.ReviewNotes) != 1 {
		t.Fatalf("agent saw %+v", seen)
	}
	prompt := renderDiffForReview(seen)
	if !strings.Contains(prompt, "service/gradle.lockfile (modified, +500/-400)") || strings.Contains(prompt, "\n+b") {
		t.Fatalf("prompt must list withheld files without their patches:\n%s", prompt)
	}
}

func TestGeneratedFilesStillHitDenyPaths(t *testing.T) {
	policy := generatedPolicy()
	policy.DenyPaths = append(policy.DenyPaths, "**/gradle.lockfile")
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.UseGitattributes([]byte(gitattributesFixture)); err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	in.Files = append(in.Files, PullRequestFile{Path: "gradle.lockfile", Status: "modified", Additions: 1, Patch: "@@ -1 +1 @@\n+b", ContentComplete: true})
	if got := reviewer.Review(in); got.Action != PullRequestRouteToHuman {
		t.Fatalf("withholding must not bypass deny paths, got %s", got.Action)
	}
}

func TestGeneratedHeaderOrigin(t *testing.T) {
	tests := []struct {
		name         string
		file         PullRequestFile
		wantWithheld bool
		wantUnlisted bool
	}{
		{
			// A header someone added in an earlier change would look the same.
			name: "pre-existing header on a modified file",
			file: PullRequestFile{Path: "api/client.go", Status: "modified", Additions: 1, Deletions: 1,
				Patch: "@@ -1,3 +1,3 @@\n // Code generated by oapi-codegen. DO NOT EDIT.\n package api\n-var a = 1\n+var a = 2"},
			wantUnlisted: true,
		},
		{
			name: "header added by this change",
			file: PullRequestFile{Path: "service/handler.go", Status: "modified", Additions: 2, Deletions: 0,
				Patch: "@@ -1,2 +1,3 @@\n+// Code generated by hand. DO NOT EDIT.\n package service\n+var x = 1"},
			wantUnlisted: true,
		},
		{
			name: "new file with a header",
			file: PullRequestFile{Path: "service/new.ts", Status: "added", Additions: 2,
				Patch: "@@ -0,0 +1,2 @@\n+/* @generated */\n+export const x = 1"},
			wantUnlisted: true,
		},
		{
			name: "header out of view",
			file: PullRequestFile{Path: "api/client.go", Status: "modified", Additions: 1, Deletions: 1,
				Patch: "@@ -40,3 +40,3 @@\n context\n-old\n+new"},
		},
		{
			name: "renamed file keeps the header",
			file: PullRequestFile{Path: "api/v2/client.go", PreviousPath: "api/client.go", Status: "renamed", Additions: 1, Deletions: 1,
				Patch: "@@ -1,2 +1,2 @@\n // Code generated by oapi-codegen. DO NOT EDIT.\n-a\n+b"},
			wantUnlisted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &recordingAgent{result: structuralVerdict()}
			reviewer, err := NewPullRequestReviewer(generatedPolicy(), fixedScorer(0), agent)
			if err != nil {
				t.Fatal(err)
			}
			in := largeFeatureInput(1)
			tt.file.ContentComplete = true
			in.Files = append(in.Files, tt.file)
			got := reviewer.Review(in)
			withheld := len(got.Withheld) == 1
			unlisted := len(got.GeneratedUnlisted) == 1
			if withheld != tt.wantWithheld || unlisted != tt.wantUnlisted {
				t.Fatalf("withheld=%v unlisted=%v, want %t/%t", got.Withheld, got.GeneratedUnlisted, tt.wantWithheld, tt.wantUnlisted)
			}
			if !withheld && len(agent.seen[0].Changes) != 2 {
				t.Fatal("a file that is not withheld must be reviewed in full")
			}
		})
	}
}

func TestGeneratedPathRenamedFromHandWritten(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(generatedPolicy(), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	in.Files = append(in.Files, PullRequestFile{
		Path: "ui/a.snap", PreviousPath: "ui/handler.ts", Status: "renamed", Additions: 1,
		Patch: "@@ -1 +1 @@\n+x", ContentComplete: true,
	})
	if got := reviewer.Review(in); len(got.Withheld) != 0 {
		t.Fatalf("moving a hand-written file under a generated name must not hide it: %+v", got.Withheld)
	}
}

func TestAllGeneratedRoutesToHuman(t *testing.T) {
	agent := &recordingAgent{result: structuralVerdict()}
	reviewer, err := NewPullRequestReviewer(generatedPolicy(), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := safePullRequestInput()
	in.Files = []PullRequestFile{{Path: "ui/a.snap", Status: "modified", Additions: 1, Patch: "@@ -1 +1 @@\n+x", ContentComplete: true}}
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman || len(agent.seen) != 0 {
		t.Fatalf("got %s after %d agent calls", got.Action, len(agent.seen))
	}
}

func TestChunkedReview(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.ReviewChunkChars = 1000
	agent := &recordingAgent{result: structuralVerdict()}
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(5) // each patch is ~620 characters
	got := reviewer.Review(in)
	if got.Action != PullRequestWouldApprove {
		t.Fatalf("got %s; stages %+v", got.Action, got.Stages)
	}
	if len(agent.seen) != 5 {
		t.Fatalf("agent calls = %d, want one per part", len(agent.seen))
	}
	files := map[string]bool{}
	for _, part := range agent.seen {
		if part.Parts != 5 || !strings.Contains(renderDiffForReview(part), fmt.Sprintf("part %d of 5", part.Part)) {
			t.Fatalf("part %d/%d not announced", part.Part, part.Parts)
		}
		for _, c := range part.Changes {
			files[c.File] = true
		}
	}
	if len(files) != 5 {
		t.Fatalf("parts covered %d files, want 5", len(files))
	}
}

func TestChunkedReviewBlocksWhenAnyPartBlocks(t *testing.T) {
	results := []ACRResult{
		{Confidence: 10, Accept: true, ReviewedFiles: []string{"a"}, Summary: "ok"},
		{Confidence: 9, ReviewedFiles: []string{"b"}, Findings: []ReviewFinding{{Severity: "P1", Title: "t", Summary: "s"}}, Summary: "bug"},
	}
	merged := mergeVerdicts(results, func(i int) string { return fmt.Sprint(i) })
	if merged.Accept || merged.Confidence != 9 || len(merged.Findings) != 1 || len(merged.ReviewedFiles) != 2 {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestChunkedReviewFileOverBudgetFailsSafe(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.ReviewChunkChars = 100
	agent := &recordingAgent{result: structuralVerdict()}
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(largeFeatureInput(1))
	if got.Action != PullRequestRouteToHuman || len(agent.seen) != 0 || !strings.Contains(got.Agent.Summary, "-character review budget") {
		t.Fatalf("got %s summary=%q calls=%d", got.Action, got.Agent.Summary, len(agent.seen))
	}
}

// truncatingAgent reports truncation for any part with more than limit
// characters of patch, the way a model that runs out of output does.
type truncatingAgent struct {
	recordingAgent
	limit int
}

func (a *truncatingAgent) Review(d Diff) ACRResult {
	size := 0
	for _, c := range d.Changes {
		size += len(c.Content)
	}
	res := a.recordingAgent.Review(d)
	if size > a.limit {
		return ACRResult{Summary: "truncated", Truncated: true, Usage: &ReviewUsage{OutputTokens: 100, Requests: 1}}
	}
	res.Usage = &ReviewUsage{OutputTokens: 10, Requests: 1}
	return res
}

func TestTruncatedPartIsReviewedInHalves(t *testing.T) {
	agent := &truncatingAgent{recordingAgent: recordingAgent{result: structuralVerdict()}, limit: 13000}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(largeFeatureInput(40)) // ~24k characters: one split
	if got.Action != PullRequestWouldApprove || got.Agent.Truncated {
		t.Fatalf("got %s truncated=%t; stages %+v", got.Action, got.Agent.Truncated, got.Stages)
	}
	if got.Agent.Usage == nil || got.Agent.Usage.Requests != 3 {
		t.Fatalf("usage must count the truncated attempt and both halves: %+v", got.Agent.Usage)
	}
}

func TestTruncationThatNeverFitsFailsSafe(t *testing.T) {
	agent := &truncatingAgent{recordingAgent: recordingAgent{result: structuralVerdict()}, limit: 10}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(largeFeatureInput(40))
	if got.Action != PullRequestRouteToHuman || !got.Agent.Truncated {
		t.Fatalf("got %s truncated=%t", got.Action, got.Agent.Truncated)
	}
	if n := len(agent.seen); n > 1+2+4+8 {
		t.Fatalf("splitting must be bounded, made %d calls", n)
	}
}

func TestTruncatedVerdictNeverPasses(t *testing.T) {
	verdict := structuralVerdict()
	verdict.Truncated = true
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), &recordingAgent{result: verdict})
	if err != nil {
		t.Fatal(err)
	}
	// recordingAgent's verdict covers every file, so only Truncated can block.
	reviewer.policy.ReviewChunkChars = 1 << 30
	got := reviewer.Review(largeFeatureInput(1))
	if got.Action != PullRequestRouteToHuman {
		t.Fatalf("a truncated verdict must route to human, got %s", got.Action)
	}
}

func TestSmallTruncatedPartIsNotSplit(t *testing.T) {
	agent := &truncatingAgent{recordingAgent: recordingAgent{result: structuralVerdict()}, limit: 10}
	reviewer, err := NewPullRequestReviewer(findingsPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(largeFeatureInput(2)) // ~1200 characters
	if got.Action != PullRequestRouteToHuman || len(agent.seen) != 1 {
		t.Fatalf("a small truncated part must fail safe after one call, got %s after %d calls", got.Action, len(agent.seen))
	}
}

func TestReviewerMinConfidence(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.RequiredReviewers = 2
	policy.ReviewerMinConfidence = map[string]int{"bedrock/sonnet": 7}
	verdict := func(confidence int, mutate func(*ACRResult)) ACRResult {
		v := structuralVerdict()
		v.Confidence = confidence
		if mutate != nil {
			mutate(&v)
		}
		return v
	}
	tests := []struct {
		name        string
		glm, sonnet ACRResult
		want        PullRequestAction
	}{
		{"sonnet at its floor", verdict(8, nil), verdict(7, nil), PullRequestWouldApprove},
		{"sonnet below its floor", verdict(8, nil), verdict(6, nil), PullRequestRouteToHuman},
		{"floor does not apply to glm", verdict(7, nil), verdict(8, nil), PullRequestRouteToHuman},
		{"floor never excuses a P1", verdict(8, nil), verdict(7, func(v *ACRResult) {
			v.Findings = append(v.Findings, ReviewFinding{Severity: "P1", Title: "bug", Summary: "breaks"})
		}), PullRequestRouteToHuman},
		{"floor never excuses a defect signal", verdict(8, nil), verdict(7, func(v *ACRResult) {
			v.RiskSignals = append(v.RiskSignals, SignalPerformanceRisk)
		}), PullRequestRouteToHuman},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0),
				namedAgent{&recordingAgent{result: tt.glm}, "fireworks/glm"},
				namedAgent{&recordingAgent{result: tt.sonnet}, "bedrock/sonnet"})
			if err != nil {
				t.Fatal(err)
			}
			if got := reviewer.Review(largeFeatureInput(2)); got.Action != tt.want {
				t.Fatalf("got %s, want %s; stages %+v", got.Action, tt.want, got.Stages)
			}
		})
	}
}

func TestReviewerMinConfidenceValidation(t *testing.T) {
	p := findingsPolicy(PullRequestModeShadow)
	p.ReviewerMinConfidence = map[string]int{"bedrock/sonnet": 6}
	if err := p.Validate(); err == nil {
		t.Fatal("a floor more than one point below min_review_confidence must be rejected")
	}
	p.ReviewerMinConfidence = map[string]int{"": 7}
	if err := p.Validate(); err == nil {
		t.Fatal("an empty reviewer key must be rejected")
	}
	p.ReviewerMinConfidence = map[string]int{"bedrock/sonnet": 7}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	allow := testPullRequestPolicy(PullRequestModeShadow)
	allow.ReviewerMinConfidence = map[string]int{"bedrock/sonnet": 10}
	if err := allow.Validate(); err == nil {
		t.Fatal("reviewer floors are review-findings only")
	}
}

func TestCopiedGeneratedFileIsReviewed(t *testing.T) {
	reviewer, err := NewPullRequestReviewer(generatedPolicy(), fixedScorer(0), &recordingAgent{result: structuralVerdict()})
	if err != nil {
		t.Fatal(err)
	}
	in := largeFeatureInput(1)
	in.Files = append(in.Files, PullRequestFile{
		Path: "ui/copy.snap", PreviousPath: "ui/a.snap", Status: "copied", Additions: 1,
		Patch: "@@ -1 +1 @@\n+x", ContentComplete: true,
	}, PullRequestFile{
		Path: "ui/moved.snap", PreviousPath: "ui/b.snap", Status: "renamed", Additions: 1,
		Patch: "@@ -1 +1 @@\n+y", ContentComplete: true,
	})
	got := reviewer.Review(in)
	if len(got.Withheld) != 1 || got.Withheld[0].Path != "ui/moved.snap" {
		t.Fatalf("a copy creates a file and must be reviewed; a rename within generated paths is withheld: %+v", got.Withheld)
	}
}

func TestPathGlobCharacterClasses(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"generated/[0-9]*.go", "generated/1_types.go", true},
		{"generated/[0-9]*.go", "generated/types.go", false},
		{"generated/[0-9]*.go", "generated/[0-9]x.go", false},
		{"v[!0-9].txt", "va.txt", true},
		{"v[!0-9].txt", "v1.txt", false},
		{"v[!0-9].txt", "v/.txt", false},
		{"a[/b]c", "a/c", false},
		{"a[/b]c", "abc", true},
		{"x[]y]z", "x]z", true},
		{"lit[eral", "lit[eral", true},
	} {
		if got := matchesAnyPath(tc.path, []string{tc.pattern}); got != tc.want {
			t.Errorf("matchesAnyPath(%q, %q) = %t, want %t", tc.path, tc.pattern, got, tc.want)
		}
	}
	if _, err := compilePathGlob("a[/]b"); err == nil {
		t.Error("a class that can only match / matches nothing and must be rejected")
	}
	rules, err := ParseGitattributesGenerated([]byte("generated/[0-9]*.go linguist-generated\n"))
	if err != nil || len(rules) != 1 {
		t.Fatalf("rules=%v err=%v", rules, err)
	}
	m := newGeneratedMatcher(PullRequestGeneratedFiles{}, rules)
	if reason, _ := m.classify(PullRequestFile{Path: "generated/2_api.go", Status: "modified"}); reason != "path" {
		t.Fatal(".gitattributes character classes must match as git does")
	}
}

// overlapAgent records the most Review calls in flight at once.
type overlapAgent struct {
	recordingAgent
	safe              bool
	inFlight, maxSeen atomic.Int32
}

func (a *overlapAgent) Review(d Diff) ACRResult {
	n := a.inFlight.Add(1)
	defer a.inFlight.Add(-1)
	for {
		seen := a.maxSeen.Load()
		if n <= seen || a.maxSeen.CompareAndSwap(seen, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	return a.recordingAgent.Review(d)
}

type concurrentOverlapAgent struct{ *overlapAgent }

func (concurrentOverlapAgent) ConcurrentReviewSafe() bool { return true }

func TestChunksRunSeriallyUnlessTheAgentIsConcurrencySafe(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.ReviewChunkChars = 1000
	plain := &overlapAgent{recordingAgent: recordingAgent{result: structuralVerdict()}}
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), plain)
	if err != nil {
		t.Fatal(err)
	}
	reviewer.Review(largeFeatureInput(6))
	if got := plain.maxSeen.Load(); got != 1 {
		t.Fatalf("an agent that does not declare concurrency safety saw %d concurrent calls", got)
	}
	safe := concurrentOverlapAgent{&overlapAgent{recordingAgent: recordingAgent{result: structuralVerdict()}}}
	reviewer, err = NewPullRequestReviewer(policy, fixedScorer(0), safe)
	if err != nil {
		t.Fatal(err)
	}
	reviewer.Review(largeFeatureInput(6))
	if got := safe.maxSeen.Load(); got < 2 || got > maxConcurrentChunks {
		t.Fatalf("a concurrency-safe agent saw %d concurrent calls, want 2..%d", got, maxConcurrentChunks)
	}
}

func TestBuiltInAgentsDeclareConcurrencySafety(t *testing.T) {
	for _, agent := range []ReviewAgent{RuleBasedAgent{}, &OpenAIAgent{}, &LLMAgent{}, &FireworksAgent{}} {
		if !concurrentReviewSafe(agent) {
			t.Errorf("%T should declare ConcurrentReviewSafe", agent)
		}
	}
}

// slowTruncatingAgent truncates whole diffs, reviews halves slowly, and
// records how many halves it reviewed at once.
type slowTruncatingAgent struct {
	mu        sync.Mutex
	active    int
	maxActive int
	safe      bool
}

func (a *slowTruncatingAgent) Review(d Diff) ACRResult {
	if len(d.ReviewNotes) == 0 || !strings.Contains(strings.Join(d.ReviewNotes, " "), "split in two") {
		return ACRResult{Summary: "truncated", Truncated: true}
	}
	a.mu.Lock()
	a.active++
	a.maxActive = max(a.maxActive, a.active)
	a.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	a.mu.Lock()
	a.active--
	a.mu.Unlock()
	res := structuralVerdict()
	for _, c := range d.Changes {
		res.ReviewedFiles = append(res.ReviewedFiles, c.File)
	}
	return res
}

type safeSlowTruncatingAgent struct{ *slowTruncatingAgent }

func (safeSlowTruncatingAgent) ConcurrentReviewSafe() bool { return true }

func TestTruncatedHalvesRunTogetherWhenSafe(t *testing.T) {
	serial := &slowTruncatingAgent{}
	if res := reviewPart(serial, largeFeatureInputDiff(t), 0, make(chan struct{}, 1)); res.Truncated || serial.maxActive != 1 {
		t.Fatalf("an agent not marked safe reviews one half at a time: max %d, %+v", serial.maxActive, res.Summary)
	}
	parallel := &slowTruncatingAgent{}
	if res := reviewPart(safeSlowTruncatingAgent{parallel}, largeFeatureInputDiff(t), 0, make(chan struct{}, maxConcurrentChunks)); res.Truncated || parallel.maxActive != 2 {
		t.Fatalf("a concurrency-safe agent reviews both halves at once: max %d", parallel.maxActive)
	}
}

func largeFeatureInputDiff(t *testing.T) Diff {
	t.Helper()
	d, ok, reason := pullRequestDiff(largeFeatureInput(40))
	if !ok {
		t.Fatal(reason)
	}
	return d
}

// Assumptions are recorded in the decision, merged across reviewers, and do
// not change the verdict, even load-bearing ones (display only).
func TestAssumptionsAreRecordedNotDecidedOn(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.RequiredReviewers = 2
	withAssumption := func(text string) ACRResult {
		v := structuralVerdict()
		v.Assumptions = []ReviewAssumption{{Assumption: text, Basis: "not visible in the diff", LoadBearing: true}}
		return v
	}
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0),
		namedAgent{&recordingAgent{result: withAssumption("Callers never pass a nil map.")}, "openai/a"},
		namedAgent{&recordingAgent{result: withAssumption("callers never pass a nil map")}, "fireworks/b"})
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(largeFeatureInput(2))
	if got.Action != PullRequestWouldApprove {
		t.Fatalf("assumptions must not change the verdict, got %s; stages %+v", got.Action, got.Stages)
	}
	if len(got.Assumptions) != 1 || strings.Join(got.Assumptions[0].Reviewers, ",") != "openai/a,fireworks/b" || !got.Assumptions[0].LoadBearing {
		t.Fatalf("assumptions = %+v", got.Assumptions)
	}
	if len(got.Agent.Assumptions) != 2 {
		t.Fatalf("merged verdict must carry each reviewer's assumptions, got %+v", got.Agent.Assumptions)
	}
}

func TestLowRiskMinReviewConfidence(t *testing.T) {
	policy := findingsPolicy(PullRequestModeShadow)
	policy.LowRiskPaths = []string{"**/*.md", "**/tests/**"}
	policy.LowRiskMinReviewConfidence = 7
	docs := func(paths ...string) PullRequestInput {
		in := largeFeatureInput(len(paths))
		for i, p := range paths {
			in.Files[i].Path = p
		}
		return in
	}
	renamed := docs("docs/guide.md")
	renamed.Files[0].Status, renamed.Files[0].PreviousPath = "renamed", "service/src/main/java/Guide.java"
	tests := []struct {
		name       string
		in         PullRequestInput
		confidence int
		want       PullRequestAction
	}{
		{"docs and tests at the low-risk floor", docs("docs/guide.md", "svc/tests/test_x.py"), 7, PullRequestWouldApprove},
		{"below the low-risk floor", docs("docs/guide.md"), 6, PullRequestRouteToHuman},
		{"one code file keeps the policy floor", docs("docs/guide.md", "svc/main.py"), 7, PullRequestRouteToHuman},
		{"a rename out of code keeps the policy floor", renamed, 7, PullRequestRouteToHuman},
		{"case differences do not relax the floor", docs("docs/GUIDE.MD"), 7, PullRequestRouteToHuman},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := structuralVerdict()
			v.Confidence = tt.confidence
			reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), &recordingAgent{result: v})
			if err != nil {
				t.Fatal(err)
			}
			if got := reviewer.Review(tt.in); got.Action != tt.want {
				t.Fatalf("got %s, want %s; stages %+v", got.Action, tt.want, got.Stages)
			}
		})
	}
}

func TestLowRiskMinReviewConfidenceValidation(t *testing.T) {
	p := findingsPolicy(PullRequestModeShadow)
	p.LowRiskMinReviewConfidence = 7
	if err := p.Validate(); err == nil {
		t.Fatal("a low-risk floor without low_risk_paths must be rejected")
	}
	p.LowRiskPaths = []string{"**/*.md"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.LowRiskMinReviewConfidence = 6
	if err := p.Validate(); err == nil {
		t.Fatal("a low-risk floor more than one point below min_review_confidence must be rejected")
	}
	p.LowRiskMinReviewConfidence = 9
	if err := p.Validate(); err == nil {
		t.Fatal("a low-risk floor above min_review_confidence must be rejected")
	}
	allow := testPullRequestPolicy(PullRequestModeShadow)
	allow.LowRiskPaths, allow.LowRiskMinReviewConfidence = []string{"**/*.md"}, 9
	if err := allow.Validate(); err == nil {
		t.Fatal("the low-risk floor is review-findings only")
	}
}
