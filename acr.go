package radar

// Safe-signal taxonomy (paper §2.4). These are changes the ACR agent recognizes
// as safe non-functional updates, or functional changes simple enough not to
// require human judgment. A diff is auto-acceptable only when EVERY change
// carries a safe signal and NO change carries a risk signal.
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
	// SignalConfigTuning is a deliberate adjustment to an operational value
	// (resource limit, replica count, timeout, retry budget, threshold, pool
	// size) with no new code path.
	SignalConfigTuning ChangeSignal = "config-tuning"
	// SignalObservabilityChange changes alerts, dashboards, metrics, or log
	// levels without changing what the service does.
	SignalObservabilityChange ChangeSignal = "observability-change"
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
	SignalConfigTuning:             true,
	SignalObservabilityChange:      true,
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

// ACRResult is one reviewer's structured review: what the change does and how
// risky it is, its findings, questions and suggested tests, the safe and risk
// signals, and what the reviewer could cover. API-backed agents return it from
// the shared recall-oriented prompt; Radar computes the accept decision from
// these fields rather than asking the model for one.
type ACRResult struct {
	// Accept is Radar's strict acceptance: confidence of at least
	// ACRMinConfidence, complete coverage, recognized safe signals, no risk
	// signals, and no P0-P2 finding.
	Accept bool `json:"accept"`
	// ModelAccept is the acceptance before the finding-severity bar: what
	// the old gatekeeper prompt asked the model to claim (confidence of at
	// least ACRMinConfidence, no risk signals and no P0/P1 finding), computed
	// from the review's fields. Policies that relax which severities block
	// (PullRequestPolicy.BlockingFindingSeverities) decide from it.
	ModelAccept bool `json:"model_accept,omitempty"`
	// Confidence is the agent's confidence on a 0–10 scale.
	Confidence int `json:"confidence"`
	// RiskSignals are the risk signals the agent found.
	RiskSignals []ChangeSignal `json:"risk_signals,omitempty"`
	// SafeSignals are the safe signals the agent found.
	SafeSignals []ChangeSignal `json:"safe_signals,omitempty"`
	// ReviewedFiles binds an agent verdict to every current changed path.
	ReviewedFiles []string `json:"reviewed_files,omitempty"`
	// Findings are concrete review findings. P0-P2 findings prevent automatic
	// approval even when the model also reports an accept verdict.
	Findings []ReviewFinding `json:"findings,omitempty"`
	// Summary is a short human-readable explanation.
	Summary string `json:"summary"`
	// Risk is the reviewer's overall risk assessment.
	Risk *ReviewRisk `json:"risk,omitempty"`
	// WhereToLook points a human reviewer at the places to read first.
	WhereToLook []ReviewPointer `json:"where_to_look,omitempty"`
	// Questions are what the reviewer would ask the author.
	Questions []string `json:"questions,omitempty"`
	// Assumptions are what the reviewer took as true to reach its review,
	// load-bearing first. They are shown, not decided on.
	Assumptions []ReviewAssumption `json:"assumptions,omitempty"`
	// TestsSuggested are tests the reviewer would add for changed behaviour.
	TestsSuggested []string `json:"tests_suggested,omitempty"`
	// Coverage says what the reviewer could check.
	Coverage *ReviewCoverage `json:"coverage,omitempty"`
	// NeedsHuman is set when the reviewer says a person must review the
	// change whatever its findings; the verdict honours it.
	NeedsHuman *NeedsHuman `json:"needs_human,omitempty"`
	// ElapsedMS is how long the agent took to produce the verdict. It is set by
	// agents that measure latency (always, even when it rounds to 0) and nil for
	// agents that do not, so the two cases stay distinguishable in decisions.
	ElapsedMS *int64 `json:"elapsed_ms,omitempty"`
	// Truncated is set when the response hit the agent's output limit. Radar
	// then reviews the part again in halves; a truncated verdict never passes.
	Truncated bool `json:"truncated,omitempty"`
	// Failed is set when the agent produced no verdict and failed safe (a
	// provider error, a timeout, an unparseable response, or a diff it could
	// not be sent). A merged review is failed when any part failed.
	Failed bool `json:"failed,omitempty"`
	// Usage is what the review cost in tokens, when the provider reports it.
	Usage *ReviewUsage `json:"usage,omitempty"`
}

// ReviewUsage counts the tokens one review used, summed over its requests.
type ReviewUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	Requests         int   `json:"requests"`
	// ReusedRequests counts reviews answered from a ReviewRecord: they cost
	// nothing in this run, so their tokens are not counted here.
	ReusedRequests int `json:"reused_requests,omitempty"`
	// CostUSD estimates what the requests cost at list price (see
	// EstimateReviewCostUSD); Unpriced is set when a model has no price, so
	// CostUSD leaves part of the spend out.
	CostUSD  float64 `json:"cost_usd,omitempty"`
	Unpriced bool    `json:"unpriced,omitempty"`
	// FirstRequest is the review's first model request's input, split by
	// cache: what it read back of the shared prefix. Measurement
	// only.
	FirstRequest *RequestUsage `json:"first_request,omitempty"`
	// PrefixSHA256 hashes what a reviewer's requests share before the diff
	// (system prompt and tool definitions), so reviews that should read one
	// cached prefix can be grouped. Measurement only.
	PrefixSHA256 string `json:"prefix_sha256,omitempty"`
}

// RequestUsage is one model request's input tokens, split by cache.
type RequestUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// Add sums o into u, allocating u when nil, and keeps the first request's
// split: o's own, or o itself when it is a single request and u has none.
func (u *ReviewUsage) Add(o *ReviewUsage) *ReviewUsage {
	return u.add(o)
}

func (u *ReviewUsage) add(o *ReviewUsage) *ReviewUsage {
	if o == nil {
		return u
	}
	if u == nil {
		u = &ReviewUsage{}
	}
	if u.FirstRequest == nil {
		switch {
		case o.FirstRequest != nil:
			first := *o.FirstRequest
			u.FirstRequest = &first
		case u.Requests == 0 && o.Requests == 1:
			u.FirstRequest = &RequestUsage{InputTokens: o.InputTokens, CacheReadTokens: o.CacheReadTokens, CacheWriteTokens: o.CacheWriteTokens}
		}
	}
	if u.PrefixSHA256 == "" {
		u.PrefixSHA256 = o.PrefixSHA256
	}
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.Requests += o.Requests
	u.ReusedRequests += o.ReusedRequests
	u.CostUSD += o.CostUSD
	u.Unpriced = u.Unpriced || o.Unpriced
	return u
}

// ReviewFinding is one structured issue found by a ReviewAgent.
type ReviewFinding struct {
	// ID identifies the finding across reviewers and pushes: a hash of its
	// file, rule and normalized title (see FindingID).
	ID       string `json:"id,omitempty"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	EndLine  int    `json:"end_line,omitempty"`
	// Summary is the finding's impact, kept under its original name.
	Summary string `json:"summary"`
	// Recommendation is the reviewer's suggested fix.
	Recommendation string `json:"recommendation,omitempty"`
	// RuleID is the review pack rule the finding cites, if any.
	RuleID string `json:"rule_id,omitempty"`
	// Confidence is how sure the reviewer is that the finding is real, 0-10.
	Confidence int `json:"confidence,omitempty"`
}

// ReviewAssumption is something a reviewer took as true without verifying it:
// where the belief comes from, which finding or verdict would change if it
// is wrong, and whether the review depends on it.
type ReviewAssumption struct {
	Assumption string `json:"assumption"`
	// Basis is a file:line the reviewer read, the pull request description,
	// or "not visible in the diff".
	Basis string `json:"basis"`
	// IfWrong names the finding, risk or verdict that would change.
	IfWrong string `json:"if_wrong"`
	// LoadBearing is set when the review's findings or verdict depend on it.
	LoadBearing bool `json:"load_bearing"`
}

// NeedsHuman is a reviewer's explicit call for a human reviewer, and why.
type NeedsHuman struct {
	Required bool   `json:"required"`
	Reason   string `json:"reason"`
}

// ReviewRisk is a reviewer's overall risk level (low, medium or high) and why.
type ReviewRisk struct {
	Level  string `json:"level"`
	Reason string `json:"reason"`
}

// ReviewPointer is a place a human reviewer should read first.
type ReviewPointer struct {
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
	Why  string `json:"why"`
}

// ReviewCoverage is what a reviewer could check. RepoAccess is whether it
// could read repository files beyond the patches; Radar sets it false for
// agents that are only shown the diff, whatever the model claims.
type ReviewCoverage struct {
	RepoAccess bool   `json:"repo_access"`
	Limits     string `json:"limits,omitempty"`
	// ToolCalls counts the repository tool calls Radar ran for the reviewer.
	ToolCalls int `json:"tool_calls,omitempty"`
	// Tools counts calls and the bytes returned to the model, per tool: the
	// repository tools Radar runs, or the Codex agent's shell commands by
	// program (shell:rg, shell:sed). Measurement only.
	Tools map[string]ToolUse `json:"tools,omitempty"`
}

// ToolUse is how often a reviewer used one tool and how much it read.
type ToolUse struct {
	Calls int `json:"calls"`
	Bytes int `json:"bytes"`
}

// AddToolUse counts one call of tool that returned n bytes.
func (c *ReviewCoverage) AddToolUse(tool string, n int) {
	if c.Tools == nil {
		c.Tools = map[string]ToolUse{}
	}
	t := c.Tools[tool]
	t.Calls++
	t.Bytes += n
	c.Tools[tool] = t
}

// RepoAccessAgent is implemented by agents that can read the repository
// beyond the patches they are shown, such as the Codex CLI agent.
type RepoAccessAgent interface {
	RepoAccess() bool
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
// conservative acceptance criterion: every change must carry at least one safe
// signal, and no change may carry a risk signal.
func (RuleBasedAgent) Review(d Diff) ACRResult {
	var risk, safe []ChangeSignal
	seenRisk := map[ChangeSignal]bool{}
	seenSafe := map[ChangeSignal]bool{}
	unvouched := 0

	for _, c := range d.Changes {
		if c.Complexity >= HighReviewEffortComplexity && !seenRisk[SignalHighReviewEffort] {
			seenRisk[SignalHighReviewEffort] = true
			risk = append(risk, SignalHighReviewEffort)
		}
		hasSafe := false
		for _, sig := range c.Signals {
			switch {
			case isRiskSignal(sig):
				if !seenRisk[sig] {
					seenRisk[sig] = true
					risk = append(risk, sig)
				}
			case isSafeSignal(sig):
				hasSafe = true
				if !seenSafe[sig] {
					seenSafe[sig] = true
					safe = append(safe, sig)
				}
			}
		}
		if !hasSafe {
			unvouched++
		}
	}

	res := ACRResult{RiskSignals: risk, SafeSignals: safe}
	switch {
	case len(risk) > 0:
		// Any risk signal: low confidence, reject.
		res.Confidence = 3
		res.Accept = false
		res.Summary = "risk signal(s) detected; routing to human"
	case len(safe) == 0 || unvouched > 0:
		// A change without a safe signal is one the agent cannot vouch for.
		res.Confidence = 5
		res.Accept = false
		res.Summary = "change(s) without a recognizable safe signal; insufficient confidence"
	default:
		// Every change carries a safe signal: high confidence, accept.
		res.Confidence = 10
		res.Accept = true
		res.Summary = "only safe signals detected"
	}
	return res
}

// ConcurrentReviewAgent is implemented by agents whose Review is safe to call
// from several goroutines at once. Radar reviews the parts of a large diff in
// parallel only for these; any other agent is called one part at a time.
type ConcurrentReviewAgent interface {
	ReviewAgent
	ConcurrentReviewSafe() bool
}

func concurrentReviewSafe(agent ReviewAgent) bool {
	c, ok := agent.(ConcurrentReviewAgent)
	return ok && c.ConcurrentReviewSafe()
}

// ConcurrentReviewSafe reports that RuleBasedAgent keeps no state.
func (RuleBasedAgent) ConcurrentReviewSafe() bool { return true }

// ReviewAgentDescriber is implemented by agents that can name the provider and
// model behind a verdict, so decision records carry review provenance.
type ReviewAgentDescriber interface {
	Describe() string
}

// Describe names the agent for decision provenance.
func (RuleBasedAgent) Describe() string { return "rule-based" }

// describeReviewAgent returns the agent's provenance string, or "" for agents
// that do not describe themselves.
func describeReviewAgent(agent ReviewAgent) string {
	if d, ok := agent.(ReviewAgentDescriber); ok {
		return d.Describe()
	}
	return ""
}
