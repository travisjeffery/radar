package main

import "github.com/travisjeffery/radar"

// defaultRunbooks is the demo RACER runbook registry used by the CLI. It models
// the per-runbook eligibility differences from the paper (§2.7.2): an
// allowlisted runbook with a clean record (relaxed P50 threshold), a
// non-allowlisted runbook (stricter P20), one throttled by its daily limit, one
// with an incident in its history, and one that is denylisted.
var defaultRunbooks = radar.RunbookRegistry{
	"dead-code-removal": {
		Name:        "dead-code-removal",
		Allowlisted: true,
		DailyLimit:  2000,
		LandedToday: 12,
		History:     radar.RiskHistory{ProductionIncidents: 0, RevertRate: 0.01, RejectionRate: 0.02, LandedDiffs: 800},
	},
	"lint-fixes": {
		Name:        "lint-fixes",
		Allowlisted: false,
		DailyLimit:  200,
		LandedToday: 5,
		History:     radar.RiskHistory{ProductionIncidents: 0, RevertRate: 0.03, RejectionRate: 0.04, LandedDiffs: 120},
	},
	"framework-migration": {
		Name:        "framework-migration",
		Allowlisted: true,
		DailyLimit:  50,
		LandedToday: 50, // throttled: at daily limit
		History:     radar.RiskHistory{ProductionIncidents: 0, RevertRate: 0.01, RejectionRate: 0.01, LandedDiffs: 300},
	},
	"risky-runbook": {
		Name:        "risky-runbook",
		Allowlisted: false,
		DailyLimit:  100,
		LandedToday: 1,
		History:     radar.RiskHistory{ProductionIncidents: 1, RevertRate: 0.02, RejectionRate: 0.03, LandedDiffs: 200},
	},
	"test-infra-codemod": {
		Name:       "test-infra-codemod",
		Denylisted: true,
	},
}
