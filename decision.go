package radar

import (
	"encoding/json"
	"fmt"
)

// Decision is the terminal outcome RADAR assigns to a diff after routing it
// through the funnel. The six outcomes correspond to the leaves of the
// eligibility tree (Figure 2) and the bot / human pipelines (Figures 3 and 4).
type Decision int

const (
	// DecisionRouteToHuman sends the diff to standard or deferred human review.
	// It is the safe default whenever any gate fails for an otherwise-eligible
	// diff.
	DecisionRouteToHuman Decision = iota
	// DecisionNotEligible means the diff is excluded from automation up front
	// (e.g. an unapproved deterministic codemod, or a denylisted runbook). Like
	// RouteToHuman it requires a human, but it never entered an automated
	// pipeline.
	DecisionNotEligible
	// DecisionBlanketAutoAccept auto-accepts a vetted deterministic-codemod diff
	// without any per-diff review (paper §2.7.2).
	DecisionBlanketAutoAccept
	// DecisionAutoLand auto-lands a bot diff after it passes all three ACE layers;
	// it lands following a configurable delay during which a human may still
	// reject it (paper §2.8).
	DecisionAutoLand
	// DecisionVerificationPassed marks a human diff that passed RADAR Verification:
	// the author may ship immediately with human review deferred (paper §2.9).
	DecisionVerificationPassed
	// DecisionRADARApproved marks a human diff that additionally passed RADAR
	// Approval: no human review is required at all (paper §2.9).
	DecisionRADARApproved
)

// String returns the human-readable name of the decision.
func (d Decision) String() string {
	switch d {
	case DecisionRouteToHuman:
		return "route-to-human"
	case DecisionNotEligible:
		return "not-eligible"
	case DecisionBlanketAutoAccept:
		return "blanket-auto-accept"
	case DecisionAutoLand:
		return "auto-land"
	case DecisionVerificationPassed:
		return "verification-passed"
	case DecisionRADARApproved:
		return "radar-approved"
	default:
		return "unknown"
	}
}

// MarshalJSON encodes the decision as its string name, so serialized traces
// are unambiguous (the int zero value is indistinguishable from route-to-human)
// and stable if the constants are ever reordered.
func (d Decision) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON accepts the string name produced by MarshalJSON.
func (d *Decision) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	for _, dec := range []Decision{
		DecisionRouteToHuman, DecisionNotEligible, DecisionBlanketAutoAccept,
		DecisionAutoLand, DecisionVerificationPassed, DecisionRADARApproved,
	} {
		if dec.String() == s {
			*d = dec
			return nil
		}
	}
	return fmt.Errorf("radar: unknown decision %q", s)
}

// Landed reports whether the decision results in the diff landing without
// further human action (used by the replay harness to count landed diffs).
// VerificationPassed is excluded because it still carries a deferred human
// review.
func (d Decision) Landed() bool {
	switch d {
	case DecisionBlanketAutoAccept, DecisionAutoLand, DecisionRADARApproved:
		return true
	default:
		return false
	}
}

// Automated reports whether RADAR handled the diff without routing it to a
// human up front (i.e. it reached an automated approval, including a deferred
// verification pass).
func (d Decision) Automated() bool {
	switch d {
	case DecisionRouteToHuman, DecisionNotEligible:
		return false
	default:
		return true
	}
}

// StageResult records the outcome of one gate in the funnel, so a DecisionTrace
// can explain exactly which checks ran and which one decided the outcome.
type StageResult struct {
	// Name identifies the stage (e.g. "ace.static-heuristics", "drs-threshold").
	Name string `json:"name"`
	// Passed is true when the stage allowed the diff to proceed.
	Passed bool `json:"passed"`
	// Reason is a short human-readable explanation of the result.
	Reason string `json:"reason"`
}

// DecisionTrace is the result of Engine.Classify: the final Decision, the
// pipeline path the diff took, and the ordered list of stages evaluated. It is
// designed to be printed or serialized so the routing is fully auditable
// (mirroring Figures 2–4).
type DecisionTrace struct {
	// DiffID echoes the classified diff's ID.
	DiffID string `json:"diff_id"`
	// Decision is the terminal outcome.
	Decision Decision `json:"decision"`
	// Path names the pipeline that handled the diff (e.g. "ace", "human-review",
	// "blanket-auto-accept").
	Path string `json:"path"`
	// Stages is the ordered list of gate results.
	Stages []StageResult `json:"stages"`
	// DRSPercentile is the diff's Diff Risk Score percentile, when DRS was
	// evaluated (-1 otherwise).
	DRSPercentile float64 `json:"drs_percentile"`
}

// add appends a stage result to the trace and returns whether it passed, so
// callers can write `if !t.add(...) { ... }` to short-circuit on failure.
func (t *DecisionTrace) add(name string, passed bool, reason string) bool {
	t.Stages = append(t.Stages, StageResult{Name: name, Passed: passed, Reason: reason})
	return passed
}

// finish sets the terminal decision and path and returns the trace.
func (t *DecisionTrace) finish(d Decision, path string) *DecisionTrace {
	t.Decision = d
	t.Path = path
	return t
}
