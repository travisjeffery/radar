package radar

import "testing"

func TestRuleBasedAgent(t *testing.T) {
	agent := RuleBasedAgent{}
	tests := []struct {
		name       string
		changes    []Change
		wantAccept bool
		wantConf   int
	}{
		{
			name:       "only safe signals accepts",
			changes:    []Change{{Signals: []ChangeSignal{SignalPureFormatting, SignalDocCommentUpdate}, Complexity: 1}},
			wantAccept: true, wantConf: 10,
		},
		{
			name:       "risk signal rejects",
			changes:    []Change{{Signals: []ChangeSignal{SignalSQLInjection}, Complexity: 1}},
			wantAccept: false, wantConf: 3,
		},
		{
			name:       "high complexity is high review effort",
			changes:    []Change{{Signals: []ChangeSignal{SignalRefactorNoBehaviorChange}, Complexity: HighReviewEffortComplexity}},
			wantAccept: false, wantConf: 3,
		},
		{
			name:       "no recognizable signal rejects",
			changes:    []Change{{Complexity: 1}},
			wantAccept: false, wantConf: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := agent.Review(Diff{Changes: tt.changes})
			if r.Accept != tt.wantAccept || r.Confidence != tt.wantConf {
				t.Fatalf("got accept=%v conf=%d, want accept=%v conf=%d (%s)",
					r.Accept, r.Confidence, tt.wantAccept, tt.wantConf, r.Summary)
			}
		})
	}
}

func TestCalibratorPercentile(t *testing.T) {
	c := NewCalibrator([]float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}) // 10 values
	cases := []struct {
		raw  float64
		want float64
	}{
		{raw: -1, want: 0},    // below all
		{raw: 0, want: 0},     // 0 values strictly below
		{raw: 5, want: 50},    // 5 of 10 below
		{raw: 9.5, want: 100}, // all below
		{raw: 100, want: 100},
	}
	for _, tt := range cases {
		if got := c.Percentile(tt.raw); got != tt.want {
			t.Errorf("Percentile(%v) = %v, want %v", tt.raw, got, tt.want)
		}
	}
	if got := NewCalibrator(nil).Percentile(5); got != 0 {
		t.Errorf("empty calibrator should return 0, got %v", got)
	}
}

func TestHeuristicScorerMonotonic(t *testing.T) {
	s := HeuristicScorer{}
	small := Diff{Changes: []Change{{File: "a.go", Content: "x", Complexity: 1}}}
	complex := Diff{Changes: []Change{{File: "a.go", Content: "x", Complexity: 8}}}
	risky := Diff{Changes: []Change{{File: "src/auth/login.go", Content: "x", Complexity: 1}}}
	riskSig := Diff{Changes: []Change{{File: "a.go", Content: "x", Complexity: 1, Signals: []ChangeSignal{SignalAuthBypass}}}}

	if s.Score(complex) <= s.Score(small) {
		t.Error("higher complexity should score higher")
	}
	if s.Score(risky) <= s.Score(small) {
		t.Error("risky path should score higher")
	}
	if s.Score(riskSig) <= s.Score(small) {
		t.Error("risk signal should score higher")
	}
}

func TestParseACRVerdict(t *testing.T) {
	t.Run("accepts clean verdict", func(t *testing.T) {
		r, err := parseACRVerdict(`here is my answer: {"accept": true, "confidence": 9, "safe_signals": ["pure-formatting"], "summary": "ok"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Accept || r.Confidence != 9 {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("enforces criterion despite model accept", func(t *testing.T) {
		// Model says accept but reports a risk signal: must be overridden to reject.
		r, err := parseACRVerdict(`{"accept": true, "confidence": 10, "risk_signals": ["auth-bypass"], "summary": "x"}`)
		if err != nil {
			t.Fatal(err)
		}
		if r.Accept {
			t.Fatal("accept must be overridden to false when a risk signal is present")
		}
	})
	t.Run("low confidence not accepted", func(t *testing.T) {
		r, err := parseACRVerdict(`{"accept": true, "confidence": 6, "safe_signals": ["logging-addition"]}`)
		if err != nil {
			t.Fatal(err)
		}
		if r.Accept {
			t.Fatal("confidence below threshold must not be accepted")
		}
	})
	t.Run("errors without json", func(t *testing.T) {
		if _, err := parseACRVerdict("no json here"); err == nil {
			t.Fatal("expected error")
		}
	})
}
