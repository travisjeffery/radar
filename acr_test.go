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
		{
			// Every change must be vouched: a safe signal on one change must
			// not auto-accept sibling changes that carry no signals at all.
			name: "unvouched sibling change rejects",
			changes: []Change{
				{Signals: []ChangeSignal{SignalDocCommentUpdate}, Complexity: 1},
				{Complexity: 3},
			},
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
	if got := NewCalibrator(nil).Percentile(5); got != 100 {
		t.Errorf("empty calibrator should fail closed with 100, got %v", got)
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
		r, err := parseACRVerdict(`{"summary":"ok","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":["pure-formatting"],"risk":[]},"coverage":{"files_reviewed":["a.go"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":9}`)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Accept || r.Confidence != 9 {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("enforces criterion despite model accept", func(t *testing.T) {
		// Model says accept but reports a risk signal: must be overridden to reject.
		r, err := parseACRVerdict(`{"summary":"x","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":[],"risk":["auth-bypass"]},"coverage":{"files_reviewed":[],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":10}`)
		if err != nil {
			t.Fatal(err)
		}
		if r.Accept {
			t.Fatal("accept must be overridden to false when a risk signal is present")
		}
	})
	t.Run("low confidence not accepted", func(t *testing.T) {
		r, err := parseACRVerdict(`{"summary":"not confident enough","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":["logging-addition"],"risk":[]},"coverage":{"files_reviewed":[],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":6}`)
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
	t.Run("rejects prose around json", func(t *testing.T) {
		if _, err := parseACRVerdict(`answer: {"summary":"ok","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":["pure-formatting"],"risk":[]},"coverage":{"files_reviewed":[],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":10}`); err == nil {
			t.Fatal("expected strict JSON error")
		}
	})
	t.Run("rejects unknown signals", func(t *testing.T) {
		if _, err := parseACRVerdict(`{"summary":"ok","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[],"questions":[],"tests_suggested":[],"signals":{"safe":["looks-fine"],"risk":[]},"coverage":{"files_reviewed":[],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":10}`); err == nil {
			t.Fatal("expected unknown signal error")
		}
	})
	t.Run("blocking finding overrides accept", func(t *testing.T) {
		r, err := parseACRVerdict(`{"summary":"finding present","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[{"severity":"P2","title":"Flaky assertion","file":"test.go","line":12,"end_line":12,"impact":"The test is timing-dependent.","recommendation":"","rule_id":"","confidence":8}],"questions":[],"tests_suggested":[],"signals":{"safe":["test-addition"],"risk":[]},"coverage":{"files_reviewed":[],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":10}`)
		if err != nil {
			t.Fatal(err)
		}
		if r.Accept {
			t.Fatal("P0-P2 findings must override accept")
		}
	})
	t.Run("allows non-blocking finding", func(t *testing.T) {
		r, err := parseACRVerdict(`{"summary":"safe","risk":{"level":"low","reason":"test"},"where_to_look":[],"findings":[{"severity":"P3","title":"Wording","file":"README.md","line":3,"end_line":3,"impact":"Optional wording improvement.","recommendation":"","rule_id":"","confidence":8}],"questions":[],"tests_suggested":[],"signals":{"safe":["doc-comment-update"],"risk":[]},"coverage":{"files_reviewed":["README.md"],"repo_access":false,"limits":""},"needs_human":{"required":false,"reason":""},"confidence":10}`)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Accept {
			t.Fatal("P3 finding should not independently block acceptance")
		}
	})
}
