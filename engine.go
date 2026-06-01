package radar

// Engine is the RADAR funnel. It holds the pluggable components (risk scorer,
// DRS calibrator, ACR review agent), the runbook registry, and the org policy,
// and exposes Classify as the single entry point that routes a diff to a
// decision.
type Engine struct {
	scorer     RiskScorer
	calibrator *Calibrator
	acr        ReviewAgent
	runbooks   RunbookRegistry
	policy     OrgPolicyConfig
}

// Option configures an Engine.
type Option func(*Engine)

// WithScorer sets the DRS risk scorer (default HeuristicScorer).
func WithScorer(s RiskScorer) Option { return func(e *Engine) { e.scorer = s } }

// WithCalibrator sets the DRS calibrator (default: empty, mapping all scores to
// percentile 0). Provide a calibrator built from a representative sample so DRS
// thresholds are meaningful.
func WithCalibrator(c *Calibrator) Option { return func(e *Engine) { e.calibrator = c } }

// WithReviewAgent sets the ACR review agent (default RuleBasedAgent).
func WithReviewAgent(a ReviewAgent) Option { return func(e *Engine) { e.acr = a } }

// WithRunbooks sets the runbook registry used for RACER runbook eligibility.
func WithRunbooks(r RunbookRegistry) Option { return func(e *Engine) { e.runbooks = r } }

// WithPolicy sets the org policy config (default DefaultPolicy).
func WithPolicy(p OrgPolicyConfig) Option { return func(e *Engine) { e.policy = p } }

// NewEngine builds an Engine with the paper's defaults — HeuristicScorer,
// RuleBasedAgent, DefaultPolicy, an empty runbook registry, and an empty
// calibrator — overridable via Options.
func NewEngine(opts ...Option) *Engine {
	e := &Engine{
		scorer:     HeuristicScorer{},
		calibrator: NewCalibrator(nil),
		acr:        RuleBasedAgent{},
		runbooks:   RunbookRegistry{},
		policy:     DefaultPolicy(),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Classify routes a diff through the RADAR funnel and returns a DecisionTrace.
// It first classifies authorship (paper §2.7.1), then dispatches to the bot or
// human pipeline; bot diffs are further routed by automation source (Figure 2).
func (e *Engine) Classify(d Diff) *DecisionTrace {
	t := &DecisionTrace{DiffID: d.ID, DRSPercentile: -1}

	// Authorship classification (Figure 2: "Human or Bot?").
	if !d.Source.IsBot() {
		t.add("authorship", true, "human-authored")
		return e.runHumanReview(d, t)
	}
	t.add("authorship", true, "bot-authored: "+d.Source.String())

	// Org policy may forbid automating this source entirely (paper §2.7.4).
	if !e.policy.sourcePermitted(d.Source) {
		t.add("source-permitted", false, "org policy does not permit source "+d.Source.String())
		return t.finish(DecisionNotEligible, "eligibility")
	}

	// Route by automation source (Figure 2: "Source type?").
	switch d.Source {
	case SourceDeterministicCodemod:
		// Deterministic codemods bypass per-diff review when blanket-approved.
		if !t.add("codemod-approval", d.CodemodApproved, codemodReason(d.CodemodApproved)) {
			return t.finish(DecisionNotEligible, "eligibility")
		}
		return t.finish(DecisionBlanketAutoAccept, "blanket-auto-accept")

	case SourceAICodemod:
		// AI codemods enter ACE directly at the non-allowlisted threshold.
		return e.runACE(d, e.policy.BotDefaultDRSThreshold, t)

	case SourceRACERRunbook:
		// RACER runbooks must pass per-runbook eligibility before entering ACE.
		rb, ok := e.runbooks.Lookup(d.Runbook)
		if !ok {
			t.add("runbook-onboarded", false, "runbook not onboarded: "+d.Runbook)
			return t.finish(DecisionNotEligible, "eligibility")
		}
		eligible, threshold, reason := rb.eligibleForACE()
		if !t.add("runbook-eligibility", eligible, reason) {
			return t.finish(DecisionRouteToHuman, "eligibility")
		}
		return e.runACE(d, threshold, t)

	default:
		t.add("source", false, "unknown source")
		return t.finish(DecisionNotEligible, "eligibility")
	}
}

func codemodReason(approved bool) string {
	if approved {
		return "codemod approved for blanket auto-accept"
	}
	return "codemod not approved at codemod level"
}
