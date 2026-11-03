package radar

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// calibrationRules map each severity calibration the adjudicated findings
// rely on to the prompt text that carries it.
var calibrationRules = map[string][]string{
	"pre-existing": {
		"Severity measures what this change does, not everything it touches",
		"P0 and P1 require that this pull request introduces the risk, or materially worsens it",
		"neither causes nor newly exposes",
		"is pre-existing. Report it at most P2",
		`start its impact with "Pre-existing:"`,
		"set needs_human so an owner decides",
		"introduces a risk only when it grants something the existing path does not",
		"it reaches or exposes a pre-existing security, authorization or data-handling gap",
	},
	"concession": {
		"concedes the code is acceptable as shipped",
		"fails only if something outside this change behaves differently from how it does today",
		"is at most P3. Drop it if it leaves the author nothing to do",
	},
	"introduced-through-unchanged-code": {
		"a line it changes or adds creates the risk or newly exposes it",
		"The vulnerable code itself may be unchanged",
		"passes untrusted input to an existing sink",
		"name the changed line that causes it",
	},
	"concrete-failure": {
		"P0 and P1 need a concrete failure",
		"name the input, call sequence or state that triggers it through this change",
		"Hardening, defence in depth, or a missing check, limit or validation with no concrete way to fail through this change is at most P3",
	},
	"dormant-new-code": {
		"This cap is not for dormant code the change adds",
		"keeps the severity it deserves, even when nothing calls or enables it yet",
	},
}

type boundaryCase struct {
	ID            string `json:"id"`
	Example       string `json:"example"`
	KeepsSeverity string `json:"keeps_severity"`
	Calibration   string `json:"calibration"`
}

type adjudicatedFinding struct {
	ID               string `json:"id"`
	URL              string `json:"url"`
	ReportedSeverity string `json:"reported_severity"`
	Impact           string `json:"impact"`
	Recommendation   string `json:"recommendation"`
	MaxSeverity      string `json:"max_severity"`
	NeedsHuman       bool   `json:"needs_human"`
	Calibration      string `json:"calibration"`
	Note             string `json:"note"`
}

type calibrationFixtures struct {
	Cases         []adjudicatedFinding `json:"cases"`
	BoundaryCases []boundaryCase       `json:"boundary_cases"`
}

func loadCalibrationFixtures(t *testing.T) calibrationFixtures {
	t.Helper()
	data, err := os.ReadFile("testdata/calibration/adjudicated_findings.json")
	if err != nil {
		t.Fatal(err)
	}
	var file calibrationFixtures
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func loadAdjudicatedFindings(t *testing.T) []adjudicatedFinding {
	t.Helper()
	return loadCalibrationFixtures(t).Cases
}

func requirePromptRule(t *testing.T, rule string) {
	t.Helper()
	want, ok := calibrationRules[rule]
	if !ok {
		t.Fatalf("unknown calibration %q", rule)
	}
	for _, s := range want {
		if !strings.Contains(acrSystemPrompt, s) {
			t.Errorf("prompt lost %q (calibration %q)", s, rule)
		}
	}
}

// Every finding people adjudicated as over-rated must be covered by a prompt
// rule, or say what covers it instead, so a prompt edit cannot silently drop
// the calibration it was rated against.
func TestAdjudicatedFindingsStayCalibrated(t *testing.T) {
	fixtures := loadCalibrationFixtures(t)
	cases := fixtures.Cases
	if len(cases) == 0 {
		t.Fatal("no adjudicated findings")
	}
	used := map[string]bool{}
	// Boundary cases keep their severity: the calibration must not cap a
	// risk the change introduces through unchanged code, or a defect in new
	// code nothing calls yet.
	for _, b := range fixtures.BoundaryCases {
		t.Run(b.ID, func(t *testing.T) {
			if b.Example == "" {
				t.Fatal("a boundary case needs an example")
			}
			if rank, ok := severityRank[b.KeepsSeverity]; !ok || rank > severityRank["P1"] {
				t.Fatalf("a boundary case keeps a P0 or P1, got %q", b.KeepsSeverity)
			}
			used[b.Calibration] = true
			requirePromptRule(t, b.Calibration)
		})
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			if c.URL == "" || c.Impact == "" || c.Recommendation == "" {
				t.Fatal("a case needs the finding's url, impact and recommendation")
			}
			maxRank, okMax := severityRank[c.MaxSeverity]
			reported, okReported := severityRank[c.ReportedSeverity]
			if !okMax || !okReported || maxRank <= reported {
				t.Fatalf("max severity %s is not below the reported %s", c.MaxSeverity, c.ReportedSeverity)
			}
			if c.Calibration == "" || c.Calibration == "repository-guidance" {
				if c.Note == "" {
					t.Fatal("a case no prompt rule covers must say why in its note")
				}
				return
			}
			used[c.Calibration] = true
			requirePromptRule(t, c.Calibration)
			if c.Calibration == "pre-existing" && c.NeedsHuman && !strings.Contains(acrSystemPrompt, "pre-existing security, authorization or data-handling gap") {
				t.Error("a pre-existing security gap no longer reaches needs_human")
			}
		})
	}
	for rule := range calibrationRules {
		if !used[rule] {
			t.Errorf("calibration %q has no adjudicated case", rule)
		}
	}
}

// The concession rule keys on what the finding itself says; the adjudicated
// case must still read as a concession, or the fixture no longer tests it.
func TestConcessionCaseConcedes(t *testing.T) {
	for _, c := range loadAdjudicatedFindings(t) {
		if c.Calibration == "concession" && !strings.Contains(c.Recommendation, "acceptable as shipped") {
			t.Errorf("%s: recommendation does not concede the code is acceptable as shipped", c.ID)
		}
	}
}

// Pre-existing gaps no longer count as "a defect or gap" excluded from
// needs_human, but every other defect still does.
func TestNeedsHumanStillExcludesIntroducedDefects(t *testing.T) {
	if !strings.Contains(acrSystemPrompt, "anything else that is a defect or gap (that is a finding, at the right severity)") {
		t.Fatal("needs_human must still exclude defects that belong in findings")
	}
}
