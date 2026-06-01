package radar

import "fmt"

// runACE executes the ACE (AI Commit Eligibility) pipeline for a bot diff
// (paper §2.8, Figure 3). It evaluates three validation layers in order — all
// must pass — using the DRS threshold already selected for the diff's source:
//
//  1. static heuristics / safety checks (scope, CI/state, content, paths);
//  2. DRS threshold (diff's risk percentile must be <= threshold);
//  3. RADAR Review Agent (ACR): confidence >= 8 and zero risk signals.
//
// On success the diff is auto-landed after the configured landing delay; any
// failure routes it to a human. The trace records every layer evaluated.
func (e *Engine) runACE(d Diff, threshold float64, t *DecisionTrace) *DecisionTrace {
	// Layer 1: static heuristics (a sequence of hard safety checks).
	if ok, reason := scopeChecks(d); !t.add("ace.scope", ok, reason) {
		return t.finish(DecisionRouteToHuman, "ace")
	}
	if ok, reason := stateChecks(d); !t.add("ace.state-ci", ok, reason) {
		return t.finish(DecisionRouteToHuman, "ace")
	}
	if ok, reason := contentChecks(d); !t.add("ace.content-blocklist", ok, reason) {
		return t.finish(DecisionRouteToHuman, "ace")
	}
	if ok, reason := pathChecks(d); !t.add("ace.path-blocklist", ok, reason) {
		return t.finish(DecisionRouteToHuman, "ace")
	}

	// Layer 2: DRS threshold.
	pct := e.calibrator.Percentile(e.scorer.Score(d))
	t.DRSPercentile = pct
	pass := pct <= threshold
	reason := fmt.Sprintf("DRS percentile %.1f <= threshold P%.0f", pct, threshold)
	if !pass {
		reason = fmt.Sprintf("DRS percentile %.1f exceeds threshold P%.0f", pct, threshold)
	}
	if !t.add("ace.drs-threshold", pass, reason) {
		return t.finish(DecisionRouteToHuman, "ace")
	}

	// Layer 3: RADAR Review Agent (ACR).
	acr := e.acr.Review(d)
	if !t.add("ace.acr", acr.Accept, acrReason(acr)) {
		return t.finish(DecisionRouteToHuman, "ace")
	}

	return t.finish(DecisionAutoLand, "ace")
}

// acrReason renders a short explanation of an ACR verdict for a stage result.
func acrReason(r ACRResult) string {
	if r.Accept {
		return fmt.Sprintf("ACR accepted (confidence %d/10): %s", r.Confidence, r.Summary)
	}
	if len(r.RiskSignals) > 0 {
		return fmt.Sprintf("ACR rejected (confidence %d/10): risk signals %v", r.Confidence, r.RiskSignals)
	}
	return fmt.Sprintf("ACR rejected (confidence %d/10): %s", r.Confidence, r.Summary)
}
