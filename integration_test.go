package radar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestClassifyTestdata exercises the full funnel end-to-end on the JSON fixtures
// using the same wiring as the CLI (default calibration sample, rule-based ACR),
// confirming each fixture routes to its intended decision (Figures 2–4).
func TestClassifyTestdata(t *testing.T) {
	registry := RunbookRegistry{
		"dead-code-removal":  {Name: "dead-code-removal", Allowlisted: true, DailyLimit: 2000, History: RiskHistory{RevertRate: 0.01, RejectionRate: 0.02, LandedDiffs: 800}},
		"test-infra-codemod": {Name: "test-infra-codemod", Denylisted: true},
		"risky-runbook":      {Name: "risky-runbook", DailyLimit: 100, History: RiskHistory{ProductionIncidents: 1, LandedDiffs: 200}},
	}
	eng := NewEngine(
		WithRunbooks(registry),
		WithCalibrator(NewCalibrator(DefaultCalibrationSample())),
	)

	cases := map[string]Decision{
		"codemod_approved":    DecisionBlanketAutoAccept,
		"codemod_unapproved":  DecisionNotEligible,
		"ai_codemod_safe":     DecisionAutoLand,
		"ai_codemod_risky":    DecisionRouteToHuman,
		"runbook_allowlisted": DecisionAutoLand,
		"runbook_denylisted":  DecisionNotEligible,
		"runbook_incident":    DecisionRouteToHuman,
		"human_approved":      DecisionRADARApproved,
		"human_verification":  DecisionVerificationPassed,
		"human_ineligible":    DecisionRouteToHuman,
		"human_security_risk": DecisionRouteToHuman,
	}

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var d Diff
			if err := json.Unmarshal(data, &d); err != nil {
				t.Fatal(err)
			}
			got := eng.Classify(d)
			if got.Decision != want {
				t.Fatalf("%s: decision = %s, want %s (DRS pct %.1f)\nstages: %+v",
					name, got.Decision, want, got.DRSPercentile, got.Stages)
			}
		})
	}
}
