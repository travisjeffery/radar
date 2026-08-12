package radar

import (
	"strings"
	"testing"
)

type fixedScorer float64

func (s fixedScorer) Score(Diff) float64 { return float64(s) }

type countingAgent struct {
	result ACRResult
	calls  int
}

func (a *countingAgent) Review(Diff) ACRResult {
	a.calls++
	return a.result
}

func testPullRequestPolicy(mode PullRequestMode) PullRequestPolicy {
	return PullRequestPolicy{
		Version:             1,
		Mode:                mode,
		AllowedBaseBranches: []string{"main"},
		AllowRules: []PullRequestPathRule{{
			Name:            "documentation",
			Include:         []string{"**/*.md"},
			Exclude:         []string{".github/**", "docs/runbooks/**"},
			Statuses:        []string{"added", "modified"},
			MaxFiles:        3,
			MaxChangedLines: 20,
		}},
		DenyPaths:           []string{".github/**", "**/security/**"},
		DenyPhrases:         []string{"disable verification"},
		MaxFiles:            5,
		MaxChangedLines:     40,
		MaxRiskPercentile:   20,
		CalibrationSample:   []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		MinReviewConfidence: 10,
	}
}

func safePullRequestInput() PullRequestInput {
	return PullRequestInput{
		ID: "owner/repo#42", Repository: "owner/repo", Number: 42,
		Title: "Clarify setup", BaseRef: "main", HeadSHA: strings.Repeat("a", 40),
		Author: "contributor", Open: true, SameRepository: true,
		ChecksObserved: true, ChecksStable: true, ChecksPassing: true,
		CheckFingerprint: "stable", StaleApprovalsDismissed: true,
		Files: []PullRequestFile{{
			Path: "docs/setup.md", Status: "modified", Additions: 2, Deletions: 1,
			Patch: "@@ -1 +1 @@\n-old\n+new", ContentComplete: true,
		}},
	}
}

func safeAgentResult() ACRResult {
	return ACRResult{
		Accept: true, Confidence: 10,
		SafeSignals: []ChangeSignal{SignalDocCommentUpdate}, ReviewedFiles: []string{"docs/setup.md"},
		Summary: "documentation-only change",
	}
}

func TestPullRequestReviewerShadowWouldApprove(t *testing.T) {
	agent := &countingAgent{result: safeAgentResult()}
	reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := safePullRequestInput()
	in.StaleApprovalsDismissed = false
	got := reviewer.Review(in)
	if got.Action != PullRequestWouldApprove || !got.Eligible || got.MatchedRule != "documentation" {
		t.Fatalf("got %+v", got)
	}
	if agent.calls != 1 {
		t.Fatalf("agent calls = %d, want 1", agent.calls)
	}
}

func TestPullRequestReviewerApprovalRequiresStaleDismissal(t *testing.T) {
	policy := testPullRequestPolicy(PullRequestModeApprove)
	agent := &countingAgent{result: safeAgentResult()}
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := safePullRequestInput()
	in.StaleApprovalsDismissed = false
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman || agent.calls != 0 {
		t.Fatalf("must fail before LLM: review=%+v calls=%d", got, agent.calls)
	}
	in.StaleApprovalsDismissed = true
	got = reviewer.Review(in)
	if got.Action != PullRequestApprove {
		t.Fatalf("got action %q, want approve", got.Action)
	}
}

func TestPullRequestReviewerConservativeRouting(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*PullRequestInput)
		score      float64
		agent      ACRResult
		want       PullRequestAction
		agentCalls int
	}{
		{name: "draft", mutate: func(in *PullRequestInput) { in.Draft = true }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "fork", mutate: func(in *PullRequestInput) { in.SameRepository = false }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "unstable checks", mutate: func(in *PullRequestInput) { in.ChecksStable = false }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "unresolved thread", mutate: func(in *PullRequestInput) { in.UnresolvedThreads = 1 }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "changes requested", mutate: func(in *PullRequestInput) { in.ChangesRequested = true }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "incomplete patch", mutate: func(in *PullRequestInput) { in.Files[0].ContentComplete = false }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "allow miss becomes candidate", mutate: func(in *PullRequestInput) { in.Files[0].Path = "src/main.go" }, agent: safeAgentResult(), want: PullRequestPolicyCandidate, agentCalls: 1},
		{name: "high score becomes candidate", mutate: func(in *PullRequestInput) {}, score: 9, agent: safeAgentResult(), want: PullRequestPolicyCandidate, agentCalls: 1},
		{name: "deny path never candidate", mutate: func(in *PullRequestInput) { in.Files[0].Path = ".github/workflows/ci.md" }, agent: safeAgentResult(), want: PullRequestRouteToHuman, agentCalls: 1},
		{name: "denied phrase never candidate", mutate: func(in *PullRequestInput) { in.Body = "disable verification" }, agent: safeAgentResult(), want: PullRequestRouteToHuman, agentCalls: 1},
		{name: "agent veto", mutate: func(in *PullRequestInput) {}, agent: ACRResult{Confidence: 10, RiskSignals: []ChangeSignal{SignalBugOrLogicError}, Summary: "risk"}, want: PullRequestRouteToHuman, agentCalls: 1},
		{name: "incomplete agent coverage", mutate: func(in *PullRequestInput) {}, agent: ACRResult{Accept: true, Confidence: 10, SafeSignals: []ChangeSignal{SignalDocCommentUpdate}, Summary: "safe"}, want: PullRequestRouteToHuman, agentCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &countingAgent{result: tt.agent}
			reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), fixedScorer(tt.score), agent)
			if err != nil {
				t.Fatal(err)
			}
			in := safePullRequestInput()
			tt.mutate(&in)
			if len(agent.result.ReviewedFiles) == 1 {
				agent.result.ReviewedFiles[0] = in.Files[0].Path
			}
			got := reviewer.Review(in)
			if got.Action != tt.want {
				t.Fatalf("action = %q, want %q; stages=%+v", got.Action, tt.want, got.Stages)
			}
			if agent.calls != tt.agentCalls {
				t.Fatalf("agent calls = %d, want %d", agent.calls, tt.agentCalls)
			}
		})
	}
}

func TestPullRequestRenameChecksOldAndNewPaths(t *testing.T) {
	agent := &countingAgent{result: safeAgentResult()}
	reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := safePullRequestInput()
	in.Files[0].Status = "renamed"
	in.Files[0].PreviousPath = "src/security/policy.md"
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman {
		t.Fatalf("old denied path must block candidate and approval: %+v", got)
	}
}

func TestPullRequestPolicyValidate(t *testing.T) {
	policy := testPullRequestPolicy(PullRequestModeShadow)
	policy.CalibrationSample = nil
	if err := policy.Validate(); err == nil {
		t.Fatal("expected missing calibration error")
	}
	policy = testPullRequestPolicy(PullRequestModeShadow)
	policy.AllowRules[0].Include = []string{"../secret"}
	if err := policy.Validate(); err == nil {
		t.Fatal("expected invalid glob error")
	}
}

func TestPathGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/setup.md", true},
		{"docs/*", "docs/setup.md", true},
		{"docs/*", "docs/guides/setup.md", false},
		{"release/?otes.md", "release/notes.md", true},
	} {
		got := matchesAnyPath(tc.path, []string{tc.pattern})
		if got != tc.want {
			t.Errorf("matchesAnyPath(%q, %q) = %t, want %t", tc.path, tc.pattern, got, tc.want)
		}
	}
}
