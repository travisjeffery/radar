package radar

// Safe-signal taxonomy (paper §2.4). These are changes the ACR agent recognizes
// as safe non-functional updates, or functional changes simple enough not to
// require human judgment. A diff is auto-acceptable only when its changes carry
// safe signals and NO risk signals.
const (
	SignalRefactorNoBehaviorChange ChangeSignal = "refactor-no-behavior-change"
	SignalDeadCodeRemoval          ChangeSignal = "dead-code-removal"
	SignalDefensiveProgramming     ChangeSignal = "defensive-programming"
	SignalLoggingAddition          ChangeSignal = "logging-addition"
	SignalPureFormatting           ChangeSignal = "pure-formatting"
	SignalDocCommentUpdate         ChangeSignal = "doc-comment-update"
	SignalImportHygiene            ChangeSignal = "import-hygiene"
	SignalTestAddition             ChangeSignal = "test-addition"
	SignalStaticResourceUpdate     ChangeSignal = "static-resource-update"
)

// Risk-signal taxonomy (paper §2.4). The presence of ANY risk signal
// disqualifies a diff from automated acceptance and routes it to a human. High
// review effort (complexity >= 4) is detected from Change.Complexity rather than
// a tag; the rest are explicit signals.
const (
	SignalStructuralChange ChangeSignal = "structural-change"
	SignalBugOrLogicError  ChangeSignal = "bug-or-logic-error"
	SignalPerformanceRisk  ChangeSignal = "performance-risk"
	SignalSecretsExposure  ChangeSignal = "secrets-exposure"
	SignalSQLInjection     ChangeSignal = "sql-injection"
	SignalAuthBypass       ChangeSignal = "auth-bypass"
	// SignalHighReviewEffort is reported by the ACR when a change's complexity
	// reaches HighReviewEffortComplexity; it is not normally set on input.
	SignalHighReviewEffort ChangeSignal = "high-review-effort"
)

// HighReviewEffortComplexity is the per-change complexity at or above which a
// change counts as high review effort, itself a risk signal (paper §2.4).
const HighReviewEffortComplexity = 4

// ACRMinConfidence is the minimum confidence (out of 10) the ACR must have to
// auto-accept a diff. Together with the zero-risk-signal requirement this is the
// conservative acceptance criterion of paper §2.4.
const ACRMinConfidence = 8

var safeSignalSet = map[ChangeSignal]bool{
	SignalRefactorNoBehaviorChange: true,
	SignalDeadCodeRemoval:          true,
	SignalDefensiveProgramming:     true,
	SignalLoggingAddition:          true,
	SignalPureFormatting:           true,
	SignalDocCommentUpdate:         true,
	SignalImportHygiene:            true,
	SignalTestAddition:             true,
	SignalStaticResourceUpdate:     true,
}

var riskSignalSet = map[ChangeSignal]bool{
	SignalStructuralChange: true,
	SignalBugOrLogicError:  true,
	SignalPerformanceRisk:  true,
	SignalSecretsExposure:  true,
	SignalSQLInjection:     true,
	SignalAuthBypass:       true,
	SignalHighReviewEffort: true,
}

func isSafeSignal(s ChangeSignal) bool { return safeSignalSet[s] }
func isRiskSignal(s ChangeSignal) bool { return riskSignalSet[s] }

// ACRResult is the verdict of an Automated Code Review pass: the detected safe
// and risk signals, the agent's confidence (0–10), and the derived accept
// decision. Accept is true only when confidence >= ACRMinConfidence and there
// are no risk signals.
type ACRResult struct {
	// Accept is the agent's auto-accept decision.
	Accept bool `json:"accept"`
	// Confidence is the agent's confidence on a 0–10 scale.
	Confidence int `json:"confidence"`
	// RiskSignals are the risk signals the agent found.
	RiskSignals []ChangeSignal `json:"risk_signals,omitempty"`
	// SafeSignals are the safe signals the agent found.
	SafeSignals []ChangeSignal `json:"safe_signals,omitempty"`
	// Summary is a short human-readable explanation.
	Summary string `json:"summary"`
}

// ReviewAgent is the RADAR Review Agent / Automated Code Review (ACR) component
// (paper §2.4). It reads a diff and classifies its changes against the safe and
// risk signal taxonomies, returning a verdict. Implementations include the
// deterministic RuleBasedAgent (default) and the LLM-backed LLMAgent.
type ReviewAgent interface {
	// Review classifies the diff and returns the ACR verdict.
	Review(Diff) ACRResult
}

// RuleBasedAgent is a deterministic ACR that derives signals directly from the
// structured Change.Signals and Change.Complexity on the input. It is offline,
// reproducible, and the default agent used by NewEngine. It mirrors the LLM
// agent's contract so the two are interchangeable behind ReviewAgent.
type RuleBasedAgent struct{}

// Review collects safe and risk signals across the diff's changes (promoting
// high-complexity changes to a high-review-effort risk signal) and applies the
// conservative acceptance criterion.
func (RuleBasedAgent) Review(d Diff) ACRResult {
	var risk, safe []ChangeSignal
	seenRisk := map[ChangeSignal]bool{}
	seenSafe := map[ChangeSignal]bool{}

	for _, c := range d.Changes {
		if c.Complexity >= HighReviewEffortComplexity && !seenRisk[SignalHighReviewEffort] {
			seenRisk[SignalHighReviewEffort] = true
			risk = append(risk, SignalHighReviewEffort)
		}
		for _, sig := range c.Signals {
			switch {
			case isRiskSignal(sig) && !seenRisk[sig]:
				seenRisk[sig] = true
				risk = append(risk, sig)
			case isSafeSignal(sig) && !seenSafe[sig]:
				seenSafe[sig] = true
				safe = append(safe, sig)
			}
		}
	}

	res := ACRResult{RiskSignals: risk, SafeSignals: safe}
	switch {
	case len(risk) > 0:
		// Any risk signal: low confidence, reject.
		res.Confidence = 3
		res.Accept = false
		res.Summary = "risk signal(s) detected; routing to human"
	case len(safe) == 0:
		// No signal at all: the agent cannot vouch for the change.
		res.Confidence = 5
		res.Accept = false
		res.Summary = "no recognizable safe signal; insufficient confidence"
	default:
		// Only safe signals: high confidence, accept.
		res.Confidence = 10
		res.Accept = true
		res.Summary = "only safe signals detected"
	}
	return res
}
