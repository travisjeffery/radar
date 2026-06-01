package radar

import "fmt"

// ApprovalDRSFactor tightens the DRS threshold for RADAR Approval relative to
// RADAR Verification: approval (waiving human review entirely) requires the diff
// to be safer than mere verification (deferred review). With the P5 default,
// approval requires roughly P2.5.
const ApprovalDRSFactor = 0.5

// runHumanReview executes the two-step pipeline for human-authored diffs (paper
// §2.9, Figure 4):
//
//	Step 1 — RADAR Verification: eligibility (state/CI, scope, author), content
//	and path checks, then ACR + DRS against the org's human threshold (P5
//	default). Passing means the author may ship immediately with human review
//	DEFERRED (DecisionVerificationPassed).
//
//	Step 2 — RADAR Approval: re-evaluates the verified diff against stricter
//	criteria (a tighter DRS threshold and maximal ACR confidence). Passing waives
//	human review entirely (DecisionRADARApproved). Failing leaves the diff at the
//	verification outcome (deferred review).
//
// Any verification-stage failure routes the diff to standard human review.
func (e *Engine) runHumanReview(d Diff, t *DecisionTrace) *DecisionTrace {
	// --- Step 1: RADAR Verification ---
	if ok, reason := stateChecks(d); !t.add("verify.state-ci", ok, reason) {
		return t.finish(DecisionRouteToHuman, "human-review")
	}
	if ok, reason := scopeChecks(d); !t.add("verify.scope", ok, reason) {
		return t.finish(DecisionRouteToHuman, "human-review")
	}
	if ok, reason := authorEligibility(d.Author); !t.add("verify.author", ok, reason) {
		return t.finish(DecisionRouteToHuman, "human-review")
	}
	if ok, reason := contentChecks(d); !t.add("verify.content-blocklist", ok, reason) {
		return t.finish(DecisionRouteToHuman, "human-review")
	}
	if ok, reason := pathChecks(d); !t.add("verify.path-blocklist", ok, reason) {
		return t.finish(DecisionRouteToHuman, "human-review")
	}

	// RADAR Review Agent and DRS form one validation group (Figure 4); the ACR
	// runs first so the substantive code-quality review is always performed for
	// an eligible diff, then the DRS threshold gates on calibrated risk.
	acr := e.acr.Review(d)
	if !t.add("verify.acr", acr.Accept, acrReason(acr)) {
		return t.finish(DecisionRouteToHuman, "human-review")
	}

	verifyThreshold := e.policy.HumanDRSThreshold
	pct := e.calibrator.Percentile(e.scorer.Score(d))
	t.DRSPercentile = pct
	if !t.add("verify.drs-threshold",
		pct <= verifyThreshold,
		fmt.Sprintf("DRS percentile %.1f vs verification threshold P%.1f", pct, verifyThreshold)) {
		return t.finish(DecisionRouteToHuman, "human-review")
	}
	// Verification passed: the author can ship with deferred review.

	// --- Step 2: RADAR Approval (waive human review entirely) ---
	if !e.policy.DeferredReviewEnabled {
		t.add("approval.enabled", false, "org policy disables deferred-review waiver; deferred human review required")
		return t.finish(DecisionVerificationPassed, "human-review")
	}

	approvalThreshold := verifyThreshold * ApprovalDRSFactor
	if !t.add("approval.drs-threshold",
		pct <= approvalThreshold,
		fmt.Sprintf("DRS percentile %.1f vs approval threshold P%.2f", pct, approvalThreshold)) {
		return t.finish(DecisionVerificationPassed, "human-review")
	}
	if !t.add("approval.acr-confidence",
		acr.Confidence >= ACRMaxConfidence,
		fmt.Sprintf("ACR confidence %d/10 vs required %d", acr.Confidence, ACRMaxConfidence)) {
		return t.finish(DecisionVerificationPassed, "human-review")
	}

	return t.finish(DecisionRADARApproved, "human-review")
}

// ACRMaxConfidence is the ACR confidence required to waive human review at the
// RADAR Approval step (the maximal score on the 0–10 scale).
const ACRMaxConfidence = 10
