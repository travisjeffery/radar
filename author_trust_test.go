package radar

import (
	"fmt"
	"strings"
	"testing"
)

func trustPolicy() PullRequestPolicy {
	p := testPullRequestPolicy(PullRequestModeShadow)
	p.MinReviewConfidence = 8
	p.MaxFiles = 20
	p.MaxChangedLines = 800
	p.AllowRules = append(p.AllowRules, PullRequestPathRule{
		Name: "small-change", Include: []string{"**"}, Statuses: []string{"added", "modified"},
		MaxFiles: 10, MaxChangedLines: 150,
	})
	p.AuthorTrust = &PullRequestAuthorTrust{Tiers: []PullRequestTrustTier{
		{Tier: 2, MinReviewConfidence: 7},
		{Tier: 3, MinReviewConfidence: 7, RuleLimits: []PullRequestRuleLimit{{Rule: "small-change", MaxFiles: 15, MaxChangedLines: 300}}},
	}}
	return p
}

func codeInput(files, linesPerFile int) PullRequestInput {
	in := safePullRequestInput()
	in.Files = nil
	for i := 0; i < files; i++ {
		in.Files = append(in.Files, PullRequestFile{
			Path: fmt.Sprintf("src/f%d.go", i), Status: "modified", Additions: linesPerFile,
			Patch: strings.Repeat("+x\n", linesPerFile), ContentComplete: true,
		})
	}
	return in
}

func agentFor(in PullRequestInput, confidence int, accept bool) ACRResult {
	r := ACRResult{Accept: accept, ModelAccept: accept, Confidence: confidence,
		SafeSignals: []ChangeSignal{SignalRefactorNoBehaviorChange}}
	for _, f := range in.Files {
		r.ReviewedFiles = append(r.ReviewedFiles, f.Path)
	}
	return r
}

func reviewTrust(t *testing.T, p PullRequestPolicy, in PullRequestInput, agent ACRResult, trust AuthorTrust) (PullRequestReview, AuthorTrustAudit, int) {
	t.Helper()
	a := &countingAgent{result: agent}
	r, err := NewPullRequestReviewer(p, fixedScorer(0), a)
	if err != nil {
		t.Fatal(err)
	}
	live, audit := r.ReviewWithAuthorTrust(in, trust)
	return live, audit, a.calls
}

func TestAuthorTrustLowersConfidenceInShadowOnly(t *testing.T) {
	in := codeInput(2, 10)
	for _, tc := range []struct {
		name  string
		trust AuthorTrust
		want  PullRequestAction
	}{
		{"unlisted", AuthorTrust{}, PullRequestRouteToHuman},
		{"tier 1", AuthorTrust{Tier: 1}, PullRequestRouteToHuman},
		{"tier 2", AuthorTrust{Tier: 2}, PullRequestWouldApprove},
		{"tier 2 agent-authored", AuthorTrust{Tier: 2, AgentAuthored: true}, PullRequestRouteToHuman},
		{"tier 3 agent-authored", AuthorTrust{Tier: 3, AgentAuthored: true}, PullRequestWouldApprove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Below the live minimum the prompt forbids an accept claim.
			live, audit, calls := reviewTrust(t, trustPolicy(), in, agentFor(in, 7, false), tc.trust)
			if live.Action != PullRequestRouteToHuman {
				t.Fatalf("live action %q must not change", live.Action)
			}
			if calls != 1 {
				t.Fatalf("agent calls = %d, want 1", calls)
			}
			if audit.TierAction != tc.want || audit.Adjusted != (tc.want != live.Action) {
				t.Fatalf("audit = %+v, want tier action %q", audit, tc.want)
			}
		})
	}
}

func TestAuthorTrustRequiresClaimAtLiveMinimum(t *testing.T) {
	in := codeInput(2, 10)
	_, audit, _ := reviewTrust(t, trustPolicy(), in, agentFor(in, 8, false), AuthorTrust{Tier: 3})
	if audit.TierAction == PullRequestWouldApprove {
		t.Fatalf("a confident decline must still route to a human: %+v", audit)
	}
}

func TestAuthorTrustTier3RaisesSmallChangeCap(t *testing.T) {
	in := codeInput(12, 20) // 12 files / 240 lines: over 10/150, within 15/300
	live, audit, _ := reviewTrust(t, trustPolicy(), in, agentFor(in, 9, true), AuthorTrust{Tier: 3})
	if live.Action != PullRequestPolicyCandidate || live.MatchedRule != "" {
		t.Fatalf("live = %q rule %q", live.Action, live.MatchedRule)
	}
	if audit.TierAction != PullRequestWouldApprove || audit.MatchedRule != "small-change" {
		t.Fatalf("audit = %+v", audit)
	}
	_, audit, _ = reviewTrust(t, trustPolicy(), in, agentFor(in, 9, true), AuthorTrust{Tier: 2})
	if audit.TierAction == PullRequestWouldApprove {
		t.Fatalf("tier 2 must not raise the cap: %+v", audit)
	}
}

func TestAuthorTrustNeverOverridesBlocks(t *testing.T) {
	top := AuthorTrust{Tier: 3}
	for _, tc := range []struct {
		name   string
		mutate func(*PullRequestInput, *ACRResult)
	}{
		{"deny path", func(in *PullRequestInput, a *ACRResult) {
			in.Files[0].Path = ".github/workflows/ci.yml"
			a.ReviewedFiles[0] = in.Files[0].Path
		}},
		{"deny phrase", func(in *PullRequestInput, _ *ACRResult) { in.Body = "disable verification" }},
		{"P0 finding", func(_ *PullRequestInput, a *ACRResult) {
			a.Findings = []ReviewFinding{{Severity: "P0", Title: "t", Summary: "s"}}
		}},
		{"P1 finding", func(_ *PullRequestInput, a *ACRResult) {
			a.Findings = []ReviewFinding{{Severity: "P1", Title: "t", Summary: "s"}}
		}},
		{"structural change", func(_ *PullRequestInput, a *ACRResult) {
			a.RiskSignals = []ChangeSignal{SignalStructuralChange}
		}},
		{"partial coverage", func(_ *PullRequestInput, a *ACRResult) { a.ReviewedFiles = a.ReviewedFiles[:1] }},
		{"confidence 6", func(_ *PullRequestInput, a *ACRResult) { a.Confidence = 6 }},
		{"checks failing", func(in *PullRequestInput, _ *ACRResult) { in.ChecksPassing = false }},
		{"over the tier cap", func(in *PullRequestInput, a *ACRResult) {
			*in = codeInput(16, 10)
			*a = agentFor(*in, 9, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := codeInput(2, 10)
			agent := agentFor(in, 7, false)
			tc.mutate(&in, &agent)
			_, audit, _ := reviewTrust(t, trustPolicy(), in, agent, top)
			if audit.TierAction == PullRequestWouldApprove {
				t.Fatalf("tier overrode a block: %+v", audit)
			}
		})
	}
}

func TestAuthorTrustRiskThresholdStillApplies(t *testing.T) {
	in := codeInput(2, 10)
	a := &countingAgent{result: agentFor(in, 9, true)}
	r, err := NewPullRequestReviewer(trustPolicy(), fixedScorer(9), a)
	if err != nil {
		t.Fatal(err)
	}
	_, audit := r.ReviewWithAuthorTrust(in, AuthorTrust{Tier: 3})
	if audit.TierAction == PullRequestWouldApprove {
		t.Fatalf("tier bypassed the risk threshold: %+v", audit)
	}
}

func TestAuthorTrustApproveModeStaysShadow(t *testing.T) {
	p := trustPolicy()
	p.Mode = PullRequestModeApprove
	in := codeInput(2, 10)
	live, audit, _ := reviewTrust(t, p, in, agentFor(in, 7, false), AuthorTrust{Tier: 2})
	if live.Action != PullRequestRouteToHuman || audit.TierAction != PullRequestWouldApprove {
		t.Fatalf("live %q audit %q", live.Action, audit.TierAction)
	}
}

func TestPublicReviewCarriesNoTrust(t *testing.T) {
	in := codeInput(2, 10)
	plain := &countingAgent{result: agentFor(in, 7, false)}
	r, err := NewPullRequestReviewer(trustPolicy(), fixedScorer(0), plain)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%+v", r.Review(in))
	for tier := 0; tier <= MaxAuthorTrustTier; tier++ {
		live, _ := r.ReviewWithAuthorTrust(in, AuthorTrust{Tier: tier})
		if got := fmt.Sprintf("%+v", live); got != want {
			t.Fatalf("tier %d changed the public review:\n got %s\nwant %s", tier, got, want)
		}
	}
}

func TestAuthorTrustPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tiers []PullRequestTrustTier
	}{
		{"tier 0", []PullRequestTrustTier{{Tier: 0}}},
		{"tier 4", []PullRequestTrustTier{{Tier: 4}}},
		{"duplicate tier", []PullRequestTrustTier{{Tier: 2}, {Tier: 2}}},
		{"two points lower", []PullRequestTrustTier{{Tier: 2, MinReviewConfidence: 6}}},
		{"stricter confidence", []PullRequestTrustTier{{Tier: 2, MinReviewConfidence: 9}}},
		{"unknown rule", []PullRequestTrustTier{{Tier: 3, RuleLimits: []PullRequestRuleLimit{{Rule: "nope", MaxFiles: 15, MaxChangedLines: 300}}}}},
		{"unbounded rule", []PullRequestTrustTier{{Tier: 3, RuleLimits: []PullRequestRuleLimit{{Rule: "documentation", MaxFiles: 4, MaxChangedLines: 30}}}}},
		{"tighten", []PullRequestTrustTier{{Tier: 3, RuleLimits: []PullRequestRuleLimit{{Rule: "small-change", MaxFiles: 5, MaxChangedLines: 300}}}}},
		{"more than double", []PullRequestTrustTier{{Tier: 3, RuleLimits: []PullRequestRuleLimit{{Rule: "small-change", MaxFiles: 15, MaxChangedLines: 301}}}}},
		{"over global", []PullRequestTrustTier{{Tier: 3, RuleLimits: []PullRequestRuleLimit{{Rule: "small-change", MaxFiles: 20, MaxChangedLines: 300}}}}},
		{"higher tier stricter", []PullRequestTrustTier{{Tier: 2, MinReviewConfidence: 7}, {Tier: 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := trustPolicy()
			if tc.name == "unbounded rule" {
				p.AllowRules[0].MaxFiles = 0
			}
			if tc.name == "over global" {
				p.MaxFiles = 18
			}
			p.AuthorTrust.Tiers = tc.tiers
			if err := p.Validate(); err == nil {
				t.Fatal("policy validated")
			}
		})
	}
	if err := trustPolicy().Validate(); err != nil {
		t.Fatalf("design policy rejected: %v", err)
	}
}

func TestParseAuthorTrustRoster(t *testing.T) {
	roster, err := ParseAuthorTrustRoster([]byte(`{
  "OctoCat": {"tier": 2, "note": "platform lead", "since": "2026-09-30"},
  "hubot-2": {"tier": 0},
  "mona": {"tier": 3}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if roster.Tier("octocat") != 2 || roster.Tier("MONA") != 3 || roster.Tier("hubot-2") != 0 || roster.Tier("nobody") != 0 {
		t.Fatalf("roster = %v", roster)
	}
	if r, err := ParseAuthorTrustRoster([]byte("{}")); err != nil || len(r) != 0 {
		t.Fatalf("empty roster: %v %v", r, err)
	}
	for _, bad := range []string{
		`octocat: 2`,
		`[{"octocat": {"tier": 1}}]`,
		`{"octocat": 2}`,
		`{"octocat": {"note": "no tier"}}`,
		`{"octocat": {"tier": 4}}`,
		`{"octocat": {"tier": -1}}`,
		`{"octocat": {"tier": "2"}}`,
		`{"octocat": {"tier": 1.5}}`,
		`{"octo_cat": {"tier": 1}}`,
		`{"octocat": {"tier": 1}, "OctoCat": {"tier": 2}}`,
		`{"octocat": {"tier": 1}} {}`,
		`{"octocat": {"tier": 1}`,
	} {
		_, err := ParseAuthorTrustRoster([]byte(bad))
		if err == nil {
			t.Fatalf("accepted %q", bad)
		}
		if strings.Contains(strings.ToLower(err.Error()), "octo") {
			t.Fatalf("error leaks a login: %v", err)
		}
	}
}

func TestAgentAuthored(t *testing.T) {
	for _, msg := range []string{
		"Fix\n\nCo-Authored-By: Claude Opus 5 <noreply@anthropic.com>",
		"Fix\n\nco-authored-by: Codex <codex@openai.com>",
		"Fix\n\nClaude-Session: https://claude.ai/code/session_abc",
		"Fix\n\nCo-authored-by: Cursor Agent <cursoragent@cursor.com>",
		"Fix\n\nhttps://chatgpt.com/codex/tasks/task_e_1",
		"Fix\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)",
	} {
		if !AgentAuthored("", []string{"plain", msg}) {
			t.Fatalf("missed %q", msg)
		}
	}
	if !AgentAuthored("https://claude.ai/code/session_x", nil) {
		t.Fatal("missed a session link in the PR body")
	}
	if AgentAuthored("Mentions claude in prose", []string{"Fix\n\nCo-authored-by: Ricardo <r@example.com>"}) {
		t.Fatal("human co-author flagged")
	}
}

func findingsTrustPolicy() PullRequestPolicy {
	p := findingsPolicy(PullRequestModeShadow)
	p.RequiredReviewers = 2
	p.AuthorTrust = &PullRequestAuthorTrust{Tiers: []PullRequestTrustTier{{Tier: 2, MinReviewConfidence: 7}}}
	return p
}

func findingsTrustReview(t *testing.T, p PullRequestPolicy, a, b ACRResult, trust AuthorTrust) (PullRequestReview, AuthorTrustAudit) {
	t.Helper()
	r, err := NewPullRequestReviewer(p, fixedScorer(0),
		namedAgent{&recordingAgent{result: a}, "fireworks/glm"}, namedAgent{&recordingAgent{result: b}, "bedrock/codex"})
	if err != nil {
		t.Fatal(err)
	}
	return r.ReviewWithAuthorTrust(largeFeatureInput(40), trust)
}

func TestAuthorTrustReviewFindingsLowersEveryReviewerFloor(t *testing.T) {
	seven := structuralVerdict()
	seven.Confidence = 7
	live, audit := findingsTrustReview(t, findingsTrustPolicy(), seven, structuralVerdict(), AuthorTrust{Tier: 2})
	if live.Action != PullRequestRouteToHuman {
		t.Fatalf("live action %q must not change", live.Action)
	}
	if audit.TierAction != PullRequestWouldApprove || !audit.Adjusted {
		t.Fatalf("tier 2 should pass an accepting reviewer at 7: %+v", audit)
	}
	_, audit = findingsTrustReview(t, findingsTrustPolicy(), seven, structuralVerdict(), AuthorTrust{Tier: 2, AgentAuthored: true})
	if audit.TierAction == PullRequestWouldApprove {
		t.Fatalf("agent-authored tier 2 is tier 1: %+v", audit)
	}
}

func TestAuthorTrustReviewFindingsKeepsBlocks(t *testing.T) {
	for name, mutate := range map[string]func(*ACRResult){
		"structural":   func(a *ACRResult) { a.RiskSignals = append(a.RiskSignals, SignalStructuralChange) },
		"P1 finding":   func(a *ACRResult) { a.Findings = []ReviewFinding{{Severity: "P1", Title: "t", Summary: "s"}} },
		"defect":       func(a *ACRResult) { a.RiskSignals = append(a.RiskSignals, SignalBugOrLogicError) },
		"confidence 6": func(a *ACRResult) { a.Confidence = 6 },
		"truncated":    func(a *ACRResult) { a.Truncated = true },
	} {
		t.Run(name, func(t *testing.T) {
			v := structuralVerdict()
			v.Confidence = 7
			mutate(&v)
			_, audit := findingsTrustReview(t, findingsTrustPolicy(), v, structuralVerdict(), AuthorTrust{Tier: 3})
			if audit.TierAction == PullRequestWouldApprove {
				t.Fatalf("tier overrode a block: %+v", audit)
			}
		})
	}
}

func TestAuthorTrustReviewFindingsReliefDoesNotStack(t *testing.T) {
	p := findingsTrustPolicy()
	p.ReviewerMinConfidence = map[string]int{"fireworks/glm": 7}
	six := structuralVerdict()
	six.Confidence = 6
	_, audit := findingsTrustReview(t, p, six, structuralVerdict(), AuthorTrust{Tier: 3})
	if audit.TierAction == PullRequestWouldApprove {
		t.Fatalf("a reviewer floor and a tier must not stack below policy-1: %+v", audit)
	}
}

func TestAuthorTrustRuleLimitsNeedAllowRules(t *testing.T) {
	p := findingsTrustPolicy()
	p.AuthorTrust.Tiers = []PullRequestTrustTier{{Tier: 3, MinReviewConfidence: 7,
		RuleLimits: []PullRequestRuleLimit{{Rule: "small-change", MaxFiles: 15, MaxChangedLines: 300}}}}
	if err := p.Validate(); err == nil {
		t.Fatal("rule_limits under review-findings must be rejected")
	}
	if err := findingsTrustPolicy().Validate(); err != nil {
		t.Fatalf("review-findings tier policy rejected: %v", err)
	}
}

func TestAuthorTrustKeepsGenuineDeclinesAboveThePromptThreshold(t *testing.T) {
	p := trustPolicy()
	p.MinReviewConfidence = 10
	p.AuthorTrust = &PullRequestAuthorTrust{Tiers: []PullRequestTrustTier{{Tier: 2, MinReviewConfidence: 9}}}
	in := codeInput(2, 10)
	_, audit, _ := reviewTrust(t, p, in, agentFor(in, 9, false), AuthorTrust{Tier: 2})
	if audit.TierAction == PullRequestWouldApprove {
		t.Fatalf("a decline at 9 is genuine (the prompt only forbids accepting below 8): %+v", audit)
	}
}
