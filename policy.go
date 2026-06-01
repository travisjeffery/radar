package radar

import "strings"

// DRS percentile thresholds used as defaults across the funnel (paper §2.3,
// §2.8, Table 1). A threshold PX means only the lowest-risk X% of diffs pass:
// a lower threshold is more conservative.
const (
	// DRSHumanDefault is the default human-diff threshold (P5): only the safest
	// 5% of diffs qualify for RADAR Verification.
	DRSHumanDefault = 5.0
	// DRSBotNonAllowlisted is the stricter threshold (P20) for non-allowlisted
	// automation sources entering ACE.
	DRSBotNonAllowlisted = 20.0
	// DRSBotAllowlisted is the relaxed threshold (P50) for allowlisted runbooks /
	// sources with an established safety record.
	DRSBotAllowlisted = 50.0
)

// InternMinTenureDays is the minimum employment tenure (days) required for SWE
// interns to be eligible for automated review (paper §2.9).
const InternMinTenureDays = 60

// MinDiffsLastYear is the threshold below which (inclusive) a human author is
// excluded from automated review: authors with no more than this many diffs in
// the past year are routed to humans (paper §2.9).
const MinDiffsLastYear = 10

// RiskHistory is a runbook's safety record over the 60-day lookback window used
// for RACER runbook eligibility (paper §2.7.2, criterion 1).
type RiskHistory struct {
	// ProductionIncidents is the count of PIs attributed to the runbook in the
	// window. Any PI makes the runbook ineligible.
	ProductionIncidents int `json:"production_incidents"`
	// RevertRate is the fraction of the runbook's landed diffs that were reverted.
	RevertRate float64 `json:"revert_rate"`
	// RejectionRate is the fraction of the runbook's diffs rejected in review.
	RejectionRate float64 `json:"rejection_rate"`
	// LandedDiffs is how many diffs the runbook has landed in the window (used to
	// establish statistical confidence via a minimum count).
	LandedDiffs int `json:"landed_diffs"`
}

// Runbook is the per-runbook eligibility configuration and state for a RACER
// runbook. Per-runbook granularity is a distinctive feature of RADAR: two
// runbooks with identical transformations are gated independently by their own
// histories and limits (paper §2.7.2).
type Runbook struct {
	// Name is the runbook identifier.
	Name string `json:"name"`
	// Allowlisted runbooks with strong safety records use the relaxed P50 DRS
	// threshold; non-allowlisted runbooks use the stricter P20 default.
	Allowlisted bool `json:"allowlisted"`
	// Denylisted runbooks (e.g. ones that caused incidents, or that target
	// sensitive areas) are permanently blocked from RADAR-landing.
	Denylisted bool `json:"denylisted"`
	// DailyLimit caps how many diffs the runbook may RADAR-land per day. Defaults
	// are conservative; high-volume runbooks may be elevated up to 2000.
	DailyLimit int `json:"daily_limit"`
	// LandedToday is how many diffs the runbook has already landed today; the
	// runbook is throttled once it reaches DailyLimit.
	LandedToday int `json:"landed_today"`
	// History is the runbook's 60-day safety record.
	History RiskHistory `json:"history"`
}

// Eligibility caps for a runbook's 60-day risk history. Diffs from a runbook
// exceeding any cap (or with any PI, or with too few landed diffs) are routed
// to humans.
const (
	// MaxRunbookRevertRate is the maximum allowed revert rate.
	MaxRunbookRevertRate = 0.05
	// MaxRunbookRejectionRate is the maximum allowed review-rejection rate.
	MaxRunbookRejectionRate = 0.10
	// MinRunbookLandedDiffs is the minimum landed-diff count for statistical
	// confidence.
	MinRunbookLandedDiffs = 50
)

// blockedRunbookKeywords are substrings whose presence in a runbook name
// excludes it from automation (e.g. to avoid automating changes to test
// infrastructure without human oversight) (paper §2.7.2, criterion 4).
var blockedRunbookKeywords = []string{"test"}

// eligibleForACE reports whether the runbook passes its per-runbook eligibility
// criteria (risk history, daily limit, denylist, name keywords) and, if so,
// which DRS threshold applies. The reason explains a failure.
func (r Runbook) eligibleForACE() (ok bool, threshold float64, reason string) {
	if r.Denylisted {
		return false, 0, "runbook is denylisted"
	}
	for _, kw := range blockedRunbookKeywords {
		if strings.Contains(strings.ToLower(r.Name), kw) {
			return false, 0, "runbook name contains blocked keyword " + kw
		}
	}
	if r.DailyLimit > 0 && r.LandedToday >= r.DailyLimit {
		return false, 0, "runbook hit daily landing limit"
	}
	if r.History.ProductionIncidents > 0 {
		return false, 0, "runbook has production incidents in lookback window"
	}
	if r.History.RevertRate > MaxRunbookRevertRate {
		return false, 0, "runbook revert rate above cap"
	}
	if r.History.RejectionRate > MaxRunbookRejectionRate {
		return false, 0, "runbook rejection rate above cap"
	}
	if r.History.LandedDiffs < MinRunbookLandedDiffs {
		return false, 0, "runbook has insufficient landed diffs for confidence"
	}
	threshold = DRSBotNonAllowlisted
	if r.Allowlisted {
		threshold = DRSBotAllowlisted
	}
	return true, threshold, "runbook eligible"
}

// RunbookRegistry resolves runbook configurations by name.
type RunbookRegistry map[string]Runbook

// Lookup returns the runbook config for name and whether it is registered.
// Unknown runbooks are treated as non-onboarded (ineligible).
func (rr RunbookRegistry) Lookup(name string) (Runbook, bool) {
	rb, ok := rr[name]
	return rb, ok
}

// OrgPolicyConfig models Meta's OrgRADARPolicyConfig (paper §2.7.4): per-org
// risk appetite controlling DRS thresholds, whether deferred review (and thus
// RADAR Approval) is enabled, and which automation sources are permitted.
// Configurability lets RADAR operate across diverse risk environments within
// one company.
type OrgPolicyConfig struct {
	// HumanDRSThreshold gates human diffs in RADAR Verification (default P5).
	HumanDRSThreshold float64 `json:"human_drs_threshold"`
	// BotAllowlistedDRSThreshold is applied to allowlisted bot sources (default P50).
	BotAllowlistedDRSThreshold float64 `json:"bot_allowlisted_drs_threshold"`
	// BotDefaultDRSThreshold is applied to non-allowlisted bot sources (default P20).
	BotDefaultDRSThreshold float64 `json:"bot_default_drs_threshold"`
	// DeferredReviewEnabled allows RADAR Approval to waive human review entirely
	// for verified human diffs. When false, the strongest human outcome is
	// VerificationPassed (deferred review).
	DeferredReviewEnabled bool `json:"deferred_review_enabled"`
	// PermittedSources restricts which automation sources may be automated. A nil
	// or empty set permits all sources.
	PermittedSources map[SourceType]bool `json:"-"`
	// LandingDelay is the configurable delay (e.g. 24h) before an auto-landed bot
	// diff actually lands, during which a human may still reject it.
	LandingDelay string `json:"landing_delay"`
}

// sourcePermitted reports whether the org permits automating the given source.
func (p OrgPolicyConfig) sourcePermitted(s SourceType) bool {
	if len(p.PermittedSources) == 0 {
		return true
	}
	return p.PermittedSources[s]
}

// DefaultPolicy returns an OrgPolicyConfig with the paper's default thresholds
// (Table 1): human P5, bot allowlisted P50, bot default P20, deferred review
// enabled, all sources permitted, 24h landing delay.
func DefaultPolicy() OrgPolicyConfig {
	return OrgPolicyConfig{
		HumanDRSThreshold:          DRSHumanDefault,
		BotAllowlistedDRSThreshold: DRSBotAllowlisted,
		BotDefaultDRSThreshold:     DRSBotNonAllowlisted,
		DeferredReviewEnabled:      true,
		LandingDelay:               "24h",
	}
}
