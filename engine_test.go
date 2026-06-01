package radar

import "testing"

// fakeScorer returns a fixed raw DRS value.
type fakeScorer float64

func (f fakeScorer) Score(Diff) float64 { return float64(f) }

// fakeAgent returns a fixed ACR verdict.
type fakeAgent ACRResult

func (f fakeAgent) Review(Diff) ACRResult { return ACRResult(f) }

// linearCalibrator maps an integer raw score v in [0,100] to percentile v, so
// tests can target DRS bands precisely.
func linearCalibrator() *Calibrator {
	sample := make([]float64, 100)
	for i := range sample {
		sample[i] = float64(i)
	}
	return NewCalibrator(sample)
}

// testEngine builds an Engine with deterministic fake scorer/agent and the
// linear calibrator, plus any extra options.
func testEngine(score float64, acr ACRResult, opts ...Option) *Engine {
	base := []Option{
		WithScorer(fakeScorer(score)),
		WithReviewAgent(fakeAgent(acr)),
		WithCalibrator(linearCalibrator()),
	}
	return NewEngine(append(base, opts...)...)
}

// accept / reject helpers for fakeAgent verdicts.
func acceptVerdict(conf int) ACRResult { return ACRResult{Accept: true, Confidence: conf} }
func rejectVerdict() ACRResult {
	return ACRResult{Accept: false, Confidence: 3, RiskSignals: []ChangeSignal{SignalBugOrLogicError}}
}

func eligibleHuman() Author {
	return Author{EligibleRole: true, HasOncallOwnership: true, TenureDays: 900, DiffsLastYear: 100}
}

func passingDiff(src SourceType) Diff {
	return Diff{ID: "T", Org: "orgA", Source: src, CI: CIPassing, State: DiffPublished}
}

func TestClassifyRouting(t *testing.T) {
	registry := RunbookRegistry{
		"clean-allow":  {Name: "clean-allow", Allowlisted: true, DailyLimit: 100, History: RiskHistory{LandedDiffs: 100}},
		"clean-strict": {Name: "clean-strict", Allowlisted: false, DailyLimit: 100, History: RiskHistory{LandedDiffs: 100}},
		"deny":         {Name: "deny", Denylisted: true},
	}

	tests := []struct {
		name  string
		diff  Diff
		score float64
		acr   ACRResult
		opts  []Option
		want  Decision
	}{
		{
			name: "deterministic codemod approved",
			diff: func() Diff { d := passingDiff(SourceDeterministicCodemod); d.CodemodApproved = true; return d }(),
			want: DecisionBlanketAutoAccept,
		},
		{
			name: "deterministic codemod unapproved",
			diff: passingDiff(SourceDeterministicCodemod),
			want: DecisionNotEligible,
		},
		{
			name:  "ai codemod safe lands",
			diff:  passingDiff(SourceAICodemod),
			score: 10, acr: acceptVerdict(10),
			want: DecisionAutoLand, // P20 threshold, 10 <= 20
		},
		{
			name:  "ai codemod drs too high",
			diff:  passingDiff(SourceAICodemod),
			score: 30, acr: acceptVerdict(10),
			want: DecisionRouteToHuman, // 30 > 20
		},
		{
			name:  "ai codemod acr rejects",
			diff:  passingDiff(SourceAICodemod),
			score: 1, acr: rejectVerdict(),
			want: DecisionRouteToHuman,
		},
		{
			name:  "runbook unknown not onboarded",
			diff:  func() Diff { d := passingDiff(SourceRACERRunbook); d.Runbook = "missing"; return d }(),
			score: 1, acr: acceptVerdict(10), opts: []Option{WithRunbooks(registry)},
			want: DecisionNotEligible,
		},
		{
			name:  "runbook denylisted",
			diff:  func() Diff { d := passingDiff(SourceRACERRunbook); d.Runbook = "deny"; return d }(),
			score: 1, acr: acceptVerdict(10), opts: []Option{WithRunbooks(registry)},
			want: DecisionRouteToHuman,
		},
		{
			name:  "runbook allowlisted relaxed threshold lands",
			diff:  func() Diff { d := passingDiff(SourceRACERRunbook); d.Runbook = "clean-allow"; return d }(),
			score: 40, acr: acceptVerdict(10), opts: []Option{WithRunbooks(registry)},
			want: DecisionAutoLand, // P50, 40 <= 50
		},
		{
			name:  "runbook non-allowlisted strict threshold routes",
			diff:  func() Diff { d := passingDiff(SourceRACERRunbook); d.Runbook = "clean-strict"; return d }(),
			score: 40, acr: acceptVerdict(10), opts: []Option{WithRunbooks(registry)},
			want: DecisionRouteToHuman, // P20, 40 > 20
		},
		{
			name:  "human approved (waives review)",
			diff:  func() Diff { d := passingDiff(SourceHuman); d.Author = eligibleHuman(); return d }(),
			score: 2, acr: acceptVerdict(10),
			want: DecisionRADARApproved, // 2 <= 2.5 approval threshold
		},
		{
			name:  "human verification only",
			diff:  func() Diff { d := passingDiff(SourceHuman); d.Author = eligibleHuman(); return d }(),
			score: 4, acr: acceptVerdict(10),
			want: DecisionVerificationPassed, // 4 <= 5 verify, > 2.5 approval
		},
		{
			name:  "human verification when deferred disabled",
			diff:  func() Diff { d := passingDiff(SourceHuman); d.Author = eligibleHuman(); return d }(),
			score: 2, acr: acceptVerdict(10),
			opts: []Option{WithPolicy(policyNoDeferral())},
			want: DecisionVerificationPassed,
		},
		{
			name:  "human drs too high",
			diff:  func() Diff { d := passingDiff(SourceHuman); d.Author = eligibleHuman(); return d }(),
			score: 10, acr: acceptVerdict(10),
			want: DecisionRouteToHuman,
		},
		{
			name:  "human acr rejects",
			diff:  func() Diff { d := passingDiff(SourceHuman); d.Author = eligibleHuman(); return d }(),
			score: 1, acr: rejectVerdict(),
			want: DecisionRouteToHuman,
		},
		{
			name:  "human ineligible author",
			diff:  func() Diff { d := passingDiff(SourceHuman); d.Author = Author{EligibleRole: false}; return d }(),
			score: 1, acr: acceptVerdict(10),
			want: DecisionRouteToHuman,
		},
		{
			name:  "human failing CI",
			diff:  func() Diff { d := passingDiff(SourceHuman); d.Author = eligibleHuman(); d.CI = CIFailing; return d }(),
			score: 1, acr: acceptVerdict(10),
			want: DecisionRouteToHuman,
		},
		{
			name:  "bot open-source scope excluded",
			diff:  func() Diff { d := passingDiff(SourceAICodemod); d.OpenSource = true; return d }(),
			score: 1, acr: acceptVerdict(10),
			want: DecisionRouteToHuman,
		},
		{
			name:  "source not permitted by org",
			diff:  passingDiff(SourceAICodemod),
			score: 1, acr: acceptVerdict(10),
			opts: []Option{WithPolicy(policyOnlyHuman())},
			want: DecisionNotEligible,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := testEngine(tt.score, tt.acr, tt.opts...)
			got := eng.Classify(tt.diff)
			if got.Decision != tt.want {
				t.Fatalf("decision = %s, want %s\ntrace: %+v", got.Decision, tt.want, got.Stages)
			}
		})
	}
}

func policyNoDeferral() OrgPolicyConfig {
	p := DefaultPolicy()
	p.DeferredReviewEnabled = false
	return p
}

func policyOnlyHuman() OrgPolicyConfig {
	p := DefaultPolicy()
	p.PermittedSources = map[SourceType]bool{SourceHuman: true}
	return p
}

func TestRunbookEligibility(t *testing.T) {
	clean := RiskHistory{ProductionIncidents: 0, RevertRate: 0.01, RejectionRate: 0.01, LandedDiffs: 100}
	tests := []struct {
		name    string
		rb      Runbook
		wantOK  bool
		wantThr float64
	}{
		{"allowlisted clean", Runbook{Name: "a", Allowlisted: true, DailyLimit: 100, History: clean}, true, DRSBotAllowlisted},
		{"non-allowlisted clean", Runbook{Name: "b", Allowlisted: false, DailyLimit: 100, History: clean}, true, DRSBotNonAllowlisted},
		{"denylisted", Runbook{Name: "c", Denylisted: true}, false, 0},
		{"blocked keyword test", Runbook{Name: "test-codemod", DailyLimit: 100, History: clean}, false, 0},
		{"daily limit hit", Runbook{Name: "d", DailyLimit: 10, LandedToday: 10, History: clean}, false, 0},
		{"has incident", Runbook{Name: "e", DailyLimit: 100, History: RiskHistory{ProductionIncidents: 1, LandedDiffs: 100}}, false, 0},
		{"revert too high", Runbook{Name: "f", DailyLimit: 100, History: RiskHistory{RevertRate: 0.5, LandedDiffs: 100}}, false, 0},
		{"rejection too high", Runbook{Name: "g", DailyLimit: 100, History: RiskHistory{RejectionRate: 0.5, LandedDiffs: 100}}, false, 0},
		{"too few landed", Runbook{Name: "h", DailyLimit: 100, History: RiskHistory{LandedDiffs: 1}}, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, thr, reason := tt.rb.eligibleForACE()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v (%s), want %v", ok, reason, tt.wantOK)
			}
			if ok && thr != tt.wantThr {
				t.Fatalf("threshold = %v, want %v", thr, tt.wantThr)
			}
		})
	}
}

func TestEligibilityHelpers(t *testing.T) {
	t.Run("content blocklist", func(t *testing.T) {
		d := Diff{Changes: []Change{{Content: "TODO: do not merge this"}}}
		if ok, _ := contentChecks(d); ok {
			t.Fatal("expected content check to fail on blocklisted phrase")
		}
	})
	t.Run("path blocklist", func(t *testing.T) {
		d := Diff{Changes: []Change{{File: ".github/workflows/ci.yml"}}}
		if ok, _ := pathChecks(d); ok {
			t.Fatal("expected path check to fail")
		}
	})
	t.Run("scope exclusions", func(t *testing.T) {
		for _, d := range []Diff{{OpenSource: true}, {SOXScoped: true}, {RequiresAdditionalReviews: true}} {
			if ok, _ := scopeChecks(d); ok {
				t.Fatalf("expected scope check to fail for %+v", d)
			}
		}
	})
	t.Run("state and ci", func(t *testing.T) {
		if ok, _ := stateChecks(Diff{State: DiffWorkInProgress, CI: CIPassing}); ok {
			t.Fatal("WIP should fail")
		}
		if ok, _ := stateChecks(Diff{State: DiffPublished, CI: CIFailing}); ok {
			t.Fatal("failing CI should fail")
		}
		if ok, _ := stateChecks(Diff{State: DiffPublished, CI: CIPassing}); !ok {
			t.Fatal("published + passing CI should pass")
		}
	})
	t.Run("author eligibility", func(t *testing.T) {
		if ok, _ := authorEligibility(eligibleHuman()); !ok {
			t.Fatal("eligible author should pass")
		}
		intern := eligibleHuman()
		intern.IsIntern = true
		intern.TenureDays = 10
		if ok, _ := authorEligibility(intern); ok {
			t.Fatal("intern below tenure should fail")
		}
		few := eligibleHuman()
		few.DiffsLastYear = 5
		if ok, _ := authorEligibility(few); ok {
			t.Fatal("too few diffs should fail")
		}
	})
}
