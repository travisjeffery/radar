package radar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// convergenceFixture is a later review round of a real pull request: the
// previous round's findings, the changes since, the threads a person had
// resolved, this round's blocking findings, and which of them the round
// holds back, and, when the fixture lists them, which findings it
// sets apart as raised late on unchanged lines.
type convergenceFixture struct {
	Source           string            `json:"source"`
	Round            int               `json:"round"`
	Head             string            `json:"head"`
	Prior            PriorReview       `json:"prior"`
	ResolvedFindings []ResolvedFinding `json:"resolved_findings"`
	Findings         []ReviewFinding   `json:"findings"`
	WantHeld         map[string]string `json:"want_held"`
	WantAction       PullRequestAction `json:"want_action"`
	WantLate         []string          `json:"want_late"`
	Note             string            `json:"note"`
}

func TestConvergenceOnRealPullRequests(t *testing.T) {
	paths, err := filepath.Glob("testdata/convergence/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fx convergenceFixture
			if err := json.Unmarshal(data, &fx); err != nil {
				t.Fatal(err)
			}
			if fx.Source == "" || fx.Note == "" {
				t.Fatal("a fixture needs its source and a note on what it shows")
			}
			in := safePullRequestInput()
			in.HeadSHA = fx.Head
			in.Files = nil
			files := map[string]bool{}
			for _, f := range fx.Findings {
				if f.File != "" && !files[f.File] {
					files[f.File] = true
					in.Files = append(in.Files, PullRequestFile{Path: f.File, Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b", ContentComplete: true})
				}
			}
			prior := fx.Prior
			in.Prior = &prior
			in.ResolvedFindings = fx.ResolvedFindings
			got := convergenceVerdict(t, in, fx.Findings...)
			held := heldReasons(got)
			if got.Convergence.Round != fx.Round || !got.Convergence.Incremental {
				t.Errorf("convergence = %+v", got.Convergence)
			}
			if len(held) != len(fx.WantHeld) {
				t.Errorf("held %v, want %v", held, fx.WantHeld)
			}
			for id, reason := range fx.WantHeld {
				if held[id] != reason {
					t.Errorf("%s: held %q, want %q", id, held[id], reason)
				}
			}
			if fx.WantLate != nil {
				var late []string
				for _, l := range got.Convergence.Late {
					late = append(late, l.FindingID)
				}
				if !slices.Equal(late, fx.WantLate) {
					t.Errorf("late %v, want %v", late, fx.WantLate)
				}
			}
			if got.Action != fx.WantAction {
				t.Errorf("action %s, want %s; stages %+v", got.Action, fx.WantAction, got.Stages)
			}
		})
	}
}
