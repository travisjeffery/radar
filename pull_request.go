package radar

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ApprovalCurrentContext is the commit status the approver posts on every
// pull request head once Radar's own approval, if any, covers that head.
// A base branch that requires it from the approver keeps a Radar
// approval from carrying over to code pushed after it, without dismissing
// human approvals on push.
const ApprovalCurrentContext = "radar/approval-current"

// PullRequestMode controls whether a safe result is reported or may be acted on.
type PullRequestMode string

const (
	PullRequestModeShadow  PullRequestMode = "shadow"
	PullRequestModeApprove PullRequestMode = "approve"
)

// PullRequestAction is the provider-neutral action recommended by the reviewer.
type PullRequestAction string

const (
	PullRequestRouteToHuman    PullRequestAction = "route-to-human"
	PullRequestPolicyCandidate PullRequestAction = "policy-update-candidate"
	PullRequestWouldApprove    PullRequestAction = "would-approve"
	PullRequestApprove         PullRequestAction = "approve"
	// PullRequestFindingsOnly is the findings stage's result: the agents
	// reviewed the change before its checks finished, so there is no verdict.
	PullRequestFindingsOnly PullRequestAction = "findings-only"
)

// StageFindings marks a review run before the pull request's checks finish:
// it reports the agents' findings and never a verdict.
const StageFindings = "findings"

// PullRequestFile is one complete file-level change from a code-review provider.
type PullRequestFile struct {
	Path            string `json:"path"`
	PreviousPath    string `json:"previous_path,omitempty"`
	Status          string `json:"status"`
	Additions       int    `json:"additions"`
	Deletions       int    `json:"deletions"`
	Patch           string `json:"patch,omitempty"`
	ContentComplete bool   `json:"content_complete"`
}

// PullRequestInput is the provider-neutral state required for a review decision.
// Adapters must populate it from current, paginated provider state.
type PullRequestInput struct {
	ID                   string            `json:"id"`
	Repository           string            `json:"repository"`
	Number               int               `json:"number"`
	Title                string            `json:"title"`
	Body                 string            `json:"body,omitempty"`
	BaseRef              string            `json:"base_ref"`
	HeadSHA              string            `json:"head_sha"`
	Author               string            `json:"author"`
	AuthorType           string            `json:"author_type,omitempty"`
	Open                 bool              `json:"open"`
	Draft                bool              `json:"draft"`
	SameRepository       bool              `json:"same_repository"`
	ChecksObserved       bool              `json:"checks_observed"`
	ChecksStable         bool              `json:"checks_stable"`
	ChecksPassing        bool              `json:"checks_passing"`
	CheckFingerprint     string            `json:"check_fingerprint,omitempty"`
	UnresolvedThreads    int               `json:"unresolved_threads"`
	ChangesRequested     bool              `json:"changes_requested"`
	ApprovalsKeptCurrent bool              `json:"approvals_kept_current"`
	Files                []PullRequestFile `json:"files"`
	// Context is the pull request's commits, linked issue, unresolved
	// threads and check states, shown to the reviewers as untrusted data.
	Context *PullRequestContext `json:"context,omitempty"`
	// Stack lists the open pull requests this one is stacked on, nearest
	// first: Stack[0].HeadRef is BaseRef, and the last entry's BaseRef is the
	// branch the stack ultimately merges into. Empty for an unstacked PR.
	Stack []PullRequestStackEntry `json:"stack,omitempty"`
	// Labels are the pull request's label names. Only users with triage or
	// write access can set them, so unlike the title and body they are
	// trusted: the policy's human_review_label among them routes to a human.
	Labels []string `json:"labels,omitempty"`
	// Prior is Radar's previous review of this pull request at another
	// head, with the changes since; a later round blocks only on what the
	// new push changed. Nil for a first review.
	Prior *PriorReview `json:"prior,omitempty"`
	// ResolvedFindings are Radar findings whose threads a person resolved,
	// on lines unchanged since; re-raising one does not block.
	ResolvedFindings []ResolvedFinding `json:"resolved_findings,omitempty"`
}

// PullRequestStackEntry is one open pull request below another in a stack.
type PullRequestStackEntry struct {
	Number  int    `json:"number"`
	HeadRef string `json:"head_ref"`
	HeadSHA string `json:"head_sha"`
	BaseRef string `json:"base_ref"`
}

// PullRequestApprovalBasis selects what makes a pull request approvable.
type PullRequestApprovalBasis string

const (
	// ApprovalBasisAllowRules requires an allow rule, the global size limits
	// and the risk threshold, then a maximally confident safe review.
	ApprovalBasisAllowRules PullRequestApprovalBasis = "allow-rules"
	// ApprovalBasisReviewFindings rests the verdict on the review agents'
	// findings: no size limits, allow rules or risk gate, and no requirement
	// that the change be one of the safe-signal kinds.
	ApprovalBasisReviewFindings PullRequestApprovalBasis = "review-findings"
)

// PullRequestPathRule names one complete, low-risk class. Every changed file
// must match an include and no exclusion for the rule to apply.
type PullRequestPathRule struct {
	Name            string   `json:"name"`
	Include         []string `json:"include"`
	Exclude         []string `json:"exclude,omitempty"`
	Statuses        []string `json:"statuses"`
	MaxFiles        int      `json:"max_files,omitempty"`
	MaxChangedLines int      `json:"max_changed_lines,omitempty"`
}

// PullRequestPolicy is the versioned, auditable policy for generic PR review.
// Approval requires an explicit allow rule, no deny match, a calibrated risk
// pass, and a maximally confident ACR verdict without risk signals.
type PullRequestPolicy struct {
	Version             int                   `json:"version"`
	Mode                PullRequestMode       `json:"mode"`
	AllowedBaseBranches []string              `json:"allowed_base_branches"`
	AllowRules          []PullRequestPathRule `json:"allow_rules"`
	DenyPaths           []string              `json:"deny_paths,omitempty"`
	DenyPhrases         []string              `json:"deny_phrases,omitempty"`
	MaxFiles            int                   `json:"max_files"`
	MaxChangedLines     int                   `json:"max_changed_lines"`
	MaxRiskPercentile   float64               `json:"max_risk_percentile"`
	CalibrationSample   []float64             `json:"calibration_sample"`
	MinReviewConfidence int                   `json:"min_review_confidence"`
	IgnoredChecks       []string              `json:"ignored_checks,omitempty"`
	AllowSkippedChecks  bool                  `json:"allow_skipped_checks,omitempty"`
	// BlockingFindingSeverities lists the review-finding severities that stop
	// an approval. Empty means P0, P1 and P2, the original criterion. It must
	// always include P0 and P1.
	BlockingFindingSeverities []string `json:"blocking_finding_severities,omitempty"`
	// ApprovalBasis defaults to allow-rules.
	ApprovalBasis PullRequestApprovalBasis `json:"approval_basis,omitempty"`
	// RequiredReviewers is how many review agents must each pass; default 1.
	RequiredReviewers int `json:"required_reviewers,omitempty"`
	// AllowStacked admits a pull request based on another open pull request's
	// head branch when the stack bottoms out on an allowed base branch. A
	// stacked pull request is never approved, only marked would-approve: its
	// verdict covers its own delta, not the changes below it.
	AllowStacked bool `json:"allow_stacked,omitempty"`
	// GeneratedFiles withholds generated files from the review agents.
	GeneratedFiles *PullRequestGeneratedFiles `json:"generated_files,omitempty"`
	// Lockfiles reviews lockfiles instead of withholding them.
	Lockfiles *PullRequestLockfiles `json:"lockfiles,omitempty"`
	// ReviewChunkChars caps the patch text sent in one review request; larger
	// diffs are reviewed in several. Zero means defaultReviewChunkChars.
	ReviewChunkChars int `json:"review_chunk_chars,omitempty"`
	// AuthorTrust bounds the shadow-only adjustment a private per-author tier
	// may make. Nil disables tiers.
	AuthorTrust *PullRequestAuthorTrust `json:"author_trust,omitempty"`
	// ReviewerMinConfidence overrides min_review_confidence for a reviewer,
	// keyed by its provenance (e.g. "bedrock/us.anthropic.claude-sonnet-5").
	// Review-findings only, so a reviewer passing at its floor has still
	// raised no blocking finding and no defect signal. A floor may sit at
	// most one point below min_review_confidence.
	ReviewerMinConfidence map[string]int `json:"reviewer_min_confidence,omitempty"`
	// LowRiskPaths name files, such as documentation and tests, whose
	// changes alone may pass at LowRiskMinReviewConfidence. They
	// match case exactly, so a case difference never relaxes the floor.
	LowRiskPaths []string `json:"low_risk_paths,omitempty"`
	// LowRiskMinReviewConfidence is every reviewer's floor when each changed
	// path, rename sources included, matches LowRiskPaths. Review-findings
	// only, at most one point below min_review_confidence; zero turns it off.
	LowRiskMinReviewConfidence int `json:"low_risk_min_review_confidence,omitempty"`
	// ReviewCriteria selects review packs, guidance and per-area policy from
	// a rules file in the trusted checkout the policy is read from.
	ReviewCriteria *PullRequestReviewCriteria `json:"review_criteria,omitempty"`
	// NeverCarryPaths are globs of files whose change always gets a new
	// review, though they look like documentation or generated output:
	// Markdown a program reads at run time, say, or review packs.
	NeverCarryPaths []string `json:"never_carry_paths,omitempty"`
	// HumanReviewLabel is the label that opts a pull request out of
	// auto-approval. Empty means DefaultHumanReviewLabel; it cannot be
	// switched off.
	HumanReviewLabel string `json:"human_review_label,omitempty"`
}

// DefaultHumanReviewLabel is the label that routes a pull request to a human
// when the policy names no other.
const DefaultHumanReviewLabel = "radar:human-review"

// humanReviewLabel returns the label that opts a pull request out of
// auto-approval.
func (p PullRequestPolicy) humanReviewLabel() string {
	if p.HumanReviewLabel == "" {
		return DefaultHumanReviewLabel
	}
	return p.HumanReviewLabel
}

// HumanReviewRequested reports whether in carries the policy's
// human-review label. GitHub label names are case-insensitive.
func (p PullRequestPolicy) HumanReviewRequested(in PullRequestInput) bool {
	return containsFold(in.Labels, p.humanReviewLabel())
}

// PullRequestReviewCriteria names the review-criteria rules file.
type PullRequestReviewCriteria struct {
	// Rules is the rules file's path in the trusted checkout, such as
	// ".radar/rules.yaml".
	Rules string `json:"rules"`
}

// maxConfidenceRelief is how far a reviewer's floor may sit below the
// policy's min_review_confidence.
const maxConfidenceRelief = 1

// lowRisk reports whether every changed path, rename sources included,
// matches LowRiskPaths and the policy sets a low-risk floor.
func (p PullRequestPolicy) lowRisk(files []PullRequestFile) bool {
	if p.LowRiskMinReviewConfidence == 0 || len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !matchesAnyPath(f.Path, p.LowRiskPaths) || (f.PreviousPath != "" && !matchesAnyPath(f.PreviousPath, p.LowRiskPaths)) {
			return false
		}
	}
	return true
}

func (p PullRequestPolicy) minConfidenceFor(reviewer string) int {
	if floor, ok := p.ReviewerMinConfidence[reviewer]; ok {
		return floor
	}
	return p.MinReviewConfidence
}

// PullRequestReview is a strict, machine-readable decision bound to one head.
type PullRequestReview struct {
	SchemaVersion  int               `json:"schema_version"`
	RequestID      string            `json:"request_id"`
	HeadSHA        string            `json:"head_sha"`
	PolicyVersion  int               `json:"policy_version"`
	Mode           PullRequestMode   `json:"mode"`
	Action         PullRequestAction `json:"action"`
	Eligible       bool              `json:"eligible"`
	MatchedRule    string            `json:"matched_rule,omitempty"`
	RawRiskScore   float64           `json:"raw_risk_score"`
	RiskPercentile float64           `json:"risk_percentile"`
	// Reviewer names the ACR provider and model that produced Agent, e.g.
	// "openai/gpt-4o-mini" or "fireworks/accounts/fireworks/models/glm-5p3".
	// With several reviewers it joins their names with "+", and Agent is the
	// conservative merge of their verdicts; Reviews holds each one.
	Reviewer      string                       `json:"reviewer,omitempty"`
	Agent         ACRResult                    `json:"agent"`
	Reviews       []PullRequestReviewerVerdict `json:"reviews,omitempty"`
	ApprovalBasis PullRequestApprovalBasis     `json:"approval_basis"`
	Stack         []PullRequestStackEntry      `json:"stack,omitempty"`
	// Withheld are the generated files the agents saw only by name.
	Withheld []GeneratedFile `json:"withheld,omitempty"`
	// Lockfiles summarises each changed lockfile the agents reviewed.
	Lockfiles []LockfileSummary `json:"lockfiles,omitempty"`
	// GeneratedUnlisted are reviewed files whose generated-code header the
	// change itself adds, outside every configured generated path.
	GeneratedUnlisted []string `json:"generated_unlisted,omitempty"`
	// Criteria records the review packs, guidance and area policy applied.
	Criteria *ReviewCriteriaRecord `json:"criteria,omitempty"`
	// Findings are every reviewer's findings merged: one entry per issue,
	// naming each reviewer that raised it (see MergedFinding).
	Findings []MergedFinding `json:"findings,omitempty"`
	// Assumptions are every reviewer's assumptions merged, load-bearing
	// first. They are recorded and shown; the verdict does not read them.
	Assumptions []MergedAssumption `json:"assumptions,omitempty"`
	// Clarifications are the outcomes of people's clarifications that the
	// verdict applied to Findings, which stay as the reviewers reported them.
	Clarifications []AppliedClarification `json:"clarifications,omitempty"`
	// Convergence is this review's round and the blocking findings the
	// verdict held back as raised late or already resolved.
	// Findings keep them as reported.
	Convergence *ConvergenceRecord `json:"convergence,omitempty"`
	// VerdictReason is the first gate that stopped the pull request, or why
	// it passed.
	VerdictReason string `json:"verdict_reason,omitempty"`
	// Stage is StageFindings for a findings-only review, StageCommit for a
	// commit reviewed without a pull request, empty for a verdict.
	Stage string `json:"stage,omitempty"`
	// ReusedReviews counts agent requests answered from a ReviewRecord
	// instead of a new model call.
	ReusedReviews int `json:"reused_reviews,omitempty"`
	// ReusedFromHead is the earlier head whose recorded reviews answered
	// some of this head's requests, when any did.
	ReusedFromHead string `json:"reused_from_head,omitempty"`
	// ReusedMoved counts the reviews reused from ReusedFromHead whose
	// patches were the same but sat at other head-side lines, as after the
	// base branch changed lines above them; their findings were moved with
	// the patches. The rest were byte-identical requests, or the
	// same patches at the same head-side lines under other base-side hunk
	// numbers or other guidance.
	ReusedMoved int `json:"reused_moved,omitempty"`
	// ReusedGuidanceChanged counts the reviews reused from ReusedFromHead
	// although the repository's guidance changed since: their
	// findings were checked against the earlier guidance. A review-pack
	// change still reviews anew.
	ReusedGuidanceChanged int `json:"reused_guidance_changed,omitempty"`
	// ReusedCarried counts the reviews reused from ReusedFromHead because
	// the push since changed only documentation or generated files.
	// The deterministic gates still ran on this head.
	ReusedCarried int `json:"reused_carried,omitempty"`
	// PullRequestTotals is the pull request's review count and estimated
	// cost so far, when the adapter keeps them.
	PullRequestTotals *PullRequestTotals `json:"pull_request_totals,omitempty"`
	Stages            []StageResult      `json:"stages"`
}

// PullRequestTotals are what Radar has spent reviewing one pull request.
type PullRequestTotals struct {
	// Reviews is the review round: one per head reviewed.
	Reviews int `json:"reviews"`
	// CostUSD is the estimated model cost of those reviews, this one's
	// included. Partial is set when it leaves spend out: a model without a
	// price, or earlier reviews whose record could not be read.
	CostUSD float64 `json:"cost_usd"`
	Partial bool    `json:"partial,omitempty"`
}

// PullRequestReviewerVerdict is one review agent's verdict and whether it met
// the policy's bar.
type PullRequestReviewerVerdict struct {
	Reviewer string    `json:"reviewer"`
	Passed   bool      `json:"passed"`
	Reason   string    `json:"reason"`
	Agent    ACRResult `json:"agent"`
}

// PullRequestReviewer applies a policy with pluggable Radar scoring and review.
type PullRequestReviewer struct {
	policy     PullRequestPolicy
	scorer     RiskScorer
	calibrator *Calibrator
	agents     []ReviewAgent
	generated  generatedMatcher
	// neverCarry are the policy's never_carry_paths, compiled once.
	neverCarry []*regexp.Regexp
	criteria   *ReviewCriteria
	memo       *reviewMemo
	// clarifications are the pull request's clarification outcomes, applied
	// to the findings the verdict counts (see UseClarifications).
	clarifications *ClarificationLedger
}

// NewPullRequestReviewer validates policy and constructs a generic PR
// reviewer. With several agents, each must pass on its own.
func NewPullRequestReviewer(policy PullRequestPolicy, scorer RiskScorer, agents ...ReviewAgent) (*PullRequestReviewer, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if scorer == nil {
		return nil, fmt.Errorf("radar: pull request scorer is required")
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("radar: pull request review agent is required")
	}
	for _, agent := range agents {
		if agent == nil {
			return nil, fmt.Errorf("radar: pull request review agent is required")
		}
	}
	if len(agents) < policy.requiredReviewers() {
		return nil, fmt.Errorf("radar: policy requires %d reviewers, got %d", policy.requiredReviewers(), len(agents))
	}
	r := &PullRequestReviewer{
		policy:     policy,
		scorer:     scorer,
		calibrator: NewCalibrator(policy.CalibrationSample),
		agents:     agents,
		memo:       &reviewMemo{byKey: map[string]ACRResult{}},
	}
	if policy.GeneratedFiles != nil {
		r.generated = newGeneratedMatcher(*policy.GeneratedFiles, nil)
	}
	for _, glob := range policy.NeverCarryPaths {
		// Validated with the policy above.
		re, _ := compilePathGlob(glob)
		r.neverCarry = append(r.neverCarry, re)
	}
	r.memo.trivial = r.trivialPath
	return r, nil
}

// trivialPath reports a file whose changes alone carry the last review
// forward: documentation, or a path the trusted policy or
// .gitattributes marks generated. A generated file the pull request adds is
// shown in full, so a change to it still changes the request's SubstanceKey
// and is reviewed. Never trivial:
//   - a dependency lockfile: the deterministic backstops do not catch a
//     hash-only change, a downgrade or a typosquat, which only a reviewer
//     reading it does;
//   - agent and review guidance, which steers every later review once
//     merged;
//   - a path the policy's never_carry_paths names.
func (r *PullRequestReviewer) trivialPath(p string) bool {
	// MDX can import and run code, generated or not.
	if strings.EqualFold(path.Ext(p), ".mdx") || dependencyLockfile(p) || agentGuidanceFile(p) {
		return false
	}
	for _, re := range r.neverCarry {
		if re.MatchString(p) {
			return false
		}
	}
	if r.criteria != nil && r.criteria.Rules.IsGuidanceFile(p) {
		return false
	}
	return isDocPath(p) || r.generated.pathGenerated(p)
}

// UseGitattributes adds the linguist-generated paths of a trusted
// .gitattributes file to the policy's generated paths.
func (r *PullRequestReviewer) UseGitattributes(data []byte) error {
	if r.policy.GeneratedFiles == nil {
		return fmt.Errorf("radar: policy has no generated_files block")
	}
	paths, err := ParseGitattributesGenerated(data)
	if err != nil {
		return err
	}
	r.generated = newGeneratedMatcher(*r.policy.GeneratedFiles, paths)
	return nil
}

// UseReviewCriteria loads the review criteria the policy's review_criteria
// names. A policy that names criteria routes every pull request to a human
// until they are loaded.
func (r *PullRequestReviewer) UseReviewCriteria(c ReviewCriteria) error {
	if r.policy.ReviewCriteria == nil {
		return fmt.Errorf("radar: policy has no review_criteria block")
	}
	if c.RulesPath != r.policy.ReviewCriteria.Rules {
		return fmt.Errorf("radar: review criteria were loaded from %q, but the policy names %q", c.RulesPath, r.policy.ReviewCriteria.Rules)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	r.criteria = &c
	return nil
}

func (p PullRequestPolicy) basis() PullRequestApprovalBasis {
	if p.ApprovalBasis == "" {
		return ApprovalBasisAllowRules
	}
	return p.ApprovalBasis
}

func (p PullRequestPolicy) requiredReviewers() int {
	if p.RequiredReviewers <= 0 {
		return 1
	}
	return p.RequiredReviewers
}

func (p PullRequestPolicy) reviewChunkChars() int {
	if p.ReviewChunkChars <= 0 {
		return defaultReviewChunkChars
	}
	return p.ReviewChunkChars
}

// Validate rejects policies that could silently broaden automation.
func (p PullRequestPolicy) Validate() error {
	if p.Version <= 0 {
		return fmt.Errorf("radar: pull request policy version must be positive")
	}
	if p.Mode != PullRequestModeShadow && p.Mode != PullRequestModeApprove {
		return fmt.Errorf("radar: pull request policy mode must be shadow or approve")
	}
	if len(p.AllowedBaseBranches) == 0 {
		return fmt.Errorf("radar: at least one allowed base branch is required")
	}
	switch p.basis() {
	case ApprovalBasisAllowRules:
		if len(p.AllowRules) == 0 {
			return fmt.Errorf("radar: at least one allow rule is required")
		}
		if p.MaxFiles <= 0 || p.MaxChangedLines <= 0 {
			return fmt.Errorf("radar: positive global file and line limits are required")
		}
	case ApprovalBasisReviewFindings:
		// Limits that would be ignored must not look as if they apply.
		if len(p.AllowRules) > 0 || p.MaxFiles != 0 || p.MaxChangedLines != 0 {
			return fmt.Errorf("radar: approval basis review-findings takes no allow rules or global file and line limits")
		}
	default:
		return fmt.Errorf("radar: approval basis must be allow-rules or review-findings")
	}
	if len(p.ReviewerMinConfidence) > 0 && p.basis() != ApprovalBasisReviewFindings {
		return fmt.Errorf("radar: reviewer_min_confidence requires approval basis review-findings")
	}
	for reviewer, floor := range p.ReviewerMinConfidence {
		if strings.TrimSpace(reviewer) == "" || floor < p.MinReviewConfidence-maxConfidenceRelief || floor > ACRMaxConfidence {
			return fmt.Errorf("radar: reviewer_min_confidence for %q must be between %d and %d", reviewer, p.MinReviewConfidence-maxConfidenceRelief, ACRMaxConfidence)
		}
	}
	if p.ReviewCriteria != nil {
		if err := validateRepositoryPath(p.ReviewCriteria.Rules); err != nil {
			return fmt.Errorf("radar: review_criteria.rules: %w", err)
		}
	}
	if p.HumanReviewLabel != "" && strings.TrimSpace(p.HumanReviewLabel) != p.HumanReviewLabel {
		return fmt.Errorf("radar: human_review_label %q has surrounding whitespace", p.HumanReviewLabel)
	}
	if p.RequiredReviewers < 0 || p.ReviewChunkChars < 0 {
		return fmt.Errorf("radar: required reviewers and review chunk size cannot be negative")
	}
	if p.GeneratedFiles != nil {
		if err := p.GeneratedFiles.validate(); err != nil {
			return err
		}
	}
	if p.MaxRiskPercentile < 0 || p.MaxRiskPercentile > 100 {
		return fmt.Errorf("radar: max risk percentile must be between 0 and 100")
	}
	if len(p.CalibrationSample) == 0 {
		return fmt.Errorf("radar: an explicit calibration sample is required")
	}
	for _, score := range p.CalibrationSample {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return fmt.Errorf("radar: calibration scores must be finite")
		}
	}
	if p.MinReviewConfidence < ACRMinConfidence || p.MinReviewConfidence > ACRMaxConfidence {
		return fmt.Errorf("radar: review confidence must be between %d and %d", ACRMinConfidence, ACRMaxConfidence)
	}
	if len(p.BlockingFindingSeverities) > 0 {
		seen := map[string]bool{}
		for _, s := range p.BlockingFindingSeverities {
			if !containsFold([]string{"P0", "P1", "P2", "P3"}, s) {
				return fmt.Errorf("radar: unknown blocking finding severity %q", s)
			}
			seen[strings.ToUpper(s)] = true
		}
		if !seen["P0"] || !seen["P1"] {
			return fmt.Errorf("radar: blocking finding severities must include P0 and P1")
		}
	}
	seen := map[string]bool{}
	for _, r := range p.AllowRules {
		if strings.TrimSpace(r.Name) == "" || seen[r.Name] {
			return fmt.Errorf("radar: allow rule names must be non-empty and unique")
		}
		seen[r.Name] = true
		if len(r.Include) == 0 || len(r.Statuses) == 0 {
			return fmt.Errorf("radar: allow rule %q requires include patterns and statuses", r.Name)
		}
		if r.MaxFiles < 0 || r.MaxChangedLines < 0 {
			return fmt.Errorf("radar: allow rule %q limits cannot be negative", r.Name)
		}
		for _, status := range r.Statuses {
			if !containsFold([]string{"added", "modified", "removed", "renamed", "copied", "changed", "unchanged"}, status) {
				return fmt.Errorf("radar: allow rule %q has unknown status %q", r.Name, status)
			}
		}
		for _, pattern := range append(append([]string{}, r.Include...), r.Exclude...) {
			if _, err := compilePathGlob(pattern); err != nil {
				return fmt.Errorf("radar: allow rule %q: %w", r.Name, err)
			}
		}
	}
	for _, pattern := range append(append(append([]string{}, p.DenyPaths...), p.NeverCarryPaths...), p.LowRiskPaths...) {
		if _, err := compilePathGlob(pattern); err != nil {
			return err
		}
	}
	if c := p.LowRiskMinReviewConfidence; c != 0 {
		if p.basis() != ApprovalBasisReviewFindings || len(p.LowRiskPaths) == 0 {
			return fmt.Errorf("radar: low_risk_min_review_confidence requires approval basis review-findings and low_risk_paths")
		}
		if c < p.MinReviewConfidence-maxConfidenceRelief || c > p.MinReviewConfidence {
			return fmt.Errorf("radar: low_risk_min_review_confidence must be between %d and %d", p.MinReviewConfidence-maxConfidenceRelief, p.MinReviewConfidence)
		}
	}
	return p.validateAuthorTrust()
}

// Review evaluates current PR state. It never performs provider mutations.
func (r *PullRequestReviewer) Review(in PullRequestInput) PullRequestReview {
	out, _ := r.review(in)
	return out
}

// Findings runs the review agents without waiting for checks, so their
// findings can be posted as soon as a pull request opens or changes. It
// skips only the gates about checks and other reviews; the result never
// approves and its action is always findings-only. Record the agents'
// verdicts with ReviewRecord so the verdict stage reuses them.
func (r *PullRequestReviewer) Findings(in PullRequestInput) PullRequestReview {
	r.memo.usePush(in.Prior)
	out, _ := r.evaluate(in, StageFindings)
	return out
}

// reviewFacts are the policy-independent results a tiered evaluation reuses,
// so the review agents run once.
type reviewFacts struct {
	agentReviewed bool
	denied        bool
	riskPassed    bool
	diff          Diff
	results       []ACRResult
	areaPolicy    ReviewAreaPolicy
}

func (r *PullRequestReviewer) review(in PullRequestInput) (PullRequestReview, reviewFacts) {
	return r.evaluate(in, "")
}

// evaluate runs the gates for stage: "" is the verdict, StageFindings and
// StageCommit skip the gates that stage cannot satisfy.
func (r *PullRequestReviewer) evaluate(in PullRequestInput, stage string) (PullRequestReview, reviewFacts) {
	out, facts := r.evaluateStages(in, stage)
	r.memo.mu.Lock()
	out.ReusedReviews, r.memo.reused = r.memo.reused, 0
	out.ReusedFromHead, r.memo.reusedFrom = r.memo.reusedFrom, ""
	out.ReusedMoved, r.memo.reusedMoved = r.memo.reusedMoved, 0
	out.ReusedGuidanceChanged, r.memo.reusedGuidance = r.memo.reusedGuidance, 0
	out.ReusedCarried, r.memo.reusedCarried = r.memo.reusedCarried, 0
	r.memo.mu.Unlock()
	out.VerdictReason = verdictReason(out)
	out.Stage = stage
	if stage == StageFindings {
		out.Action = PullRequestFindingsOnly
		out.Eligible = false
	}
	return out, facts
}

func (r *PullRequestReviewer) evaluateStages(in PullRequestInput, stage string) (PullRequestReview, reviewFacts) {
	var facts reviewFacts
	// A commit review is report-only whatever the policy's mode: there is no
	// pull request to approve.
	mode := r.policy.Mode
	if stage == StageCommit {
		mode = PullRequestModeShadow
	}
	out := PullRequestReview{
		SchemaVersion:  1,
		RequestID:      in.ID,
		HeadSHA:        in.HeadSHA,
		PolicyVersion:  r.policy.Version,
		Mode:           mode,
		Action:         PullRequestRouteToHuman,
		RiskPercentile: -1,
		Reviewer:       r.describeReviewers(),
		ApprovalBasis:  r.policy.basis(),
		Stack:          in.Stack,
	}
	add := func(name string, passed bool, reason string) bool {
		out.Stages = append(out.Stages, StageResult{Name: name, Passed: passed, Reason: reason})
		return passed
	}

	stateGate := reviewStateGate
	switch stage {
	case StageFindings:
		stateGate = findingsStateGate
	case StageCommit:
		stateGate = commitStateGate
	}
	if ok, reason := stateGate(r.policy, in); !add("pr.state", ok, reason) {
		return out, facts
	}

	// The opt-out stops approval but not the review: the findings are still
	// posted, and shadow mode records that the label routed the change. A
	// commit has no labels to opt out with.
	optedOut := false
	if stage != StageCommit {
		optedOut = r.policy.HumanReviewRequested(in)
		optOutReason := "no " + r.policy.humanReviewLabel() + " label"
		if optedOut {
			optOutReason = "the " + r.policy.humanReviewLabel() + " label asks for a human reviewer"
		}
		add("pr.opt-out", !optedOut, optOutReason)
	}

	diff, complete, reason := pullRequestDiff(in)
	if !add("pr.complete-diff", complete, reason) {
		return out, facts
	}

	denied, denyReason := pullRequestDenied(r.policy, in, stage)
	add("pr.deny-policy", !denied, denyReason)
	denied = denied || optedOut

	lockfiles := r.lockfileSummaries(in)
	out.Lockfiles = lockfiles
	if r.policy.Lockfiles != nil && len(lockfiles) > 0 {
		backstop, reason := lockfileBackstop(*r.policy.Lockfiles, in.Files, lockfiles, in.Title+"\n"+in.Body)
		add("pr.lockfile-policy", !backstop, reason)
		denied = denied || backstop
	}

	criteria, areaPolicy, ok := r.selectCriteria(&out, in, add)
	if !ok {
		return out, facts
	}
	if areaPolicy.humanRequired() {
		denied = true
	}
	facts.areaPolicy = areaPolicy

	allowlisted := true
	if r.policy.basis() == ApprovalBasisAllowRules {
		rule, allowReason := matchPullRequestRule(r.policy, in)
		allowlisted = rule != nil
		add("pr.allow-rule", allowlisted, allowReason)
		if rule != nil {
			out.MatchedRule = rule.Name
		}
	}

	out.RawRiskScore = r.scorer.Score(diff)
	if math.IsNaN(out.RawRiskScore) || math.IsInf(out.RawRiskScore, 0) {
		add("pr.risk-threshold", false, "risk scorer returned a non-finite value")
		return out, facts
	}
	out.RiskPercentile = r.calibrator.Percentile(out.RawRiskScore)
	riskPassed := true
	if r.policy.basis() == ApprovalBasisAllowRules {
		riskPassed = out.RiskPercentile <= r.policy.MaxRiskPercentile
		add("pr.risk-threshold", riskPassed,
			fmt.Sprintf("risk percentile %.1f vs threshold P%.1f", out.RiskPercentile, r.policy.MaxRiskPercentile))
	} else {
		// The heuristic score grows with line count, so gating on it would
		// reintroduce the size limit this basis drops.
		add("pr.risk-score", true, fmt.Sprintf("risk percentile %.1f recorded, not gating under review-findings", out.RiskPercentile))
	}

	prContext := renderPRContext(in, stage)
	// The context's room is fixed, not its length: replies grow the
	// context between stages, and a budget that moved with them would
	// re-chunk the diff and miss every recorded review.
	chunk := r.policy.reviewChunkChars()
	budget := chunk - criteria.chars() - min(maxPRContextChars+len(prContextTag)*2+8, chunk/4)
	reviewDiff := r.reviewableDiff(diff, in, lockfiles, budget)
	reviewDiff.Criteria = criteria
	reviewDiff.PRContext = prContext
	out.Withheld = reviewDiff.Withheld
	for _, f := range in.Files {
		if _, unlisted := r.generated.classify(f); unlisted {
			out.GeneratedUnlisted = append(out.GeneratedUnlisted, f.Path)
		}
	}
	agentPassed := false
	if len(reviewDiff.Changes) == 0 {
		add("pr.review-agent", false, fmt.Sprintf("all %d changed files are generated; nothing for the review agents to assess", len(out.Withheld)))
	} else {
		agentPassed, facts.results = r.runReviewers(&out, reviewDiff, budget, areaPolicy, r.policy.lowRisk(in.Files), newConvergence(in), add)
		facts.agentReviewed = true
	}
	facts.diff = reviewDiff

	facts.denied, facts.riskPassed = denied, riskPassed
	out.Eligible = !denied && allowlisted && riskPassed
	if out.Eligible && agentPassed && len(in.Stack) > 0 {
		add("pr.stack", true, fmt.Sprintf("stacked on #%d; the verdict covers this pull request's own changes only", in.Stack[0].Number))
	}
	out.Action = decideAction(mode, len(in.Stack) > 0, denied, out.Eligible, agentPassed)
	return out, facts
}

// selectCriteria resolves the review criteria for in and records them. It
// fails closed when the policy names criteria that were not loaded.
func (r *PullRequestReviewer) selectCriteria(out *PullRequestReview, in PullRequestInput, add func(string, bool, string) bool) (*ReviewContext, ReviewAreaPolicy, bool) {
	if r.policy.ReviewCriteria == nil {
		return nil, ReviewAreaPolicy{}, true
	}
	if r.criteria == nil {
		add("pr.review-criteria", false, "policy names review criteria "+r.policy.ReviewCriteria.Rules+" but none were loaded")
		return nil, ReviewAreaPolicy{}, false
	}
	ctx, record, area := r.criteria.selectCriteria(in.Files, func(f PullRequestFile) bool { return !r.withheld(f) })
	out.Criteria = &record
	if limit := r.policy.reviewChunkChars() / 2; ctx.chars() > limit {
		add("pr.review-criteria", false, fmt.Sprintf("selected review criteria are %d characters, over half the %d-character review budget", ctx.chars(), r.policy.reviewChunkChars()))
		return nil, ReviewAreaPolicy{}, false
	}
	packs := make([]string, 0, len(record.Packs))
	for _, p := range record.Packs {
		packs = append(packs, fmt.Sprintf("%s@v%d", p.ID, p.Version))
	}
	add("pr.review-criteria", true, fmt.Sprintf("%d area(s), packs [%s], %d guidance file(s)", len(record.Areas), strings.Join(packs, " "), len(record.Guidance)))
	if area.humanRequired() {
		var names []string
		for _, a := range record.Areas {
			names = append(names, a.Name)
		}
		add("pr.area-policy", false, "an area this change touches requires human approval (auto_approve false); areas: "+strings.Join(names, ", "))
	}
	return ctx, area, true
}

// withheld reports whether reviewableDiff leaves f out of what the agents see.
func (r *PullRequestReviewer) withheld(f PullRequestFile) bool {
	if r.policy.Lockfiles != nil {
		if _, ok := lockfileKindOf(f.Path); ok {
			return false
		}
	}
	reason, _ := r.generated.classify(f)
	return reason != ""
}

func decideAction(mode PullRequestMode, stacked, denied, eligible, agentPassed bool) PullRequestAction {
	switch {
	case eligible && agentPassed && (stacked || mode != PullRequestModeApprove):
		return PullRequestWouldApprove
	case eligible && agentPassed:
		return PullRequestApprove
	case !denied && !eligible && agentPassed:
		return PullRequestPolicyCandidate
	}
	return PullRequestRouteToHuman
}

func (r *PullRequestReviewer) lockfileSummaries(in PullRequestInput) []LockfileSummary {
	if r.policy.Lockfiles == nil {
		return nil
	}
	var out []LockfileSummary
	for _, f := range in.Files {
		if kind, ok := lockfileKindOf(f.Path); ok {
			out = append(out, summarizeLockfile(f.Path, kind, f.Patch))
		}
	}
	return out
}

// reviewableDiff withholds generated files from what the agents see. With
// lockfile review on, lockfiles are kept: one whose +/- lines repeat an
// earlier lockfile's is shown as a pointer to it, and one too large for a
// single request is split by hunk so every line is still reviewed.
func (r *PullRequestReviewer) reviewableDiff(diff Diff, in PullRequestInput, lockfiles []LockfileSummary, chunkBudget int) Diff {
	out := diff
	out.Changes = nil
	out.Lockfiles = lockfiles
	summaries := map[string]LockfileSummary{}
	for _, s := range lockfiles {
		summaries[s.Path] = s
	}
	firstWith := map[string]string{}
	budget := chunkBudget - chunkOverheadChars - 512
	for i, f := range in.Files {
		change := diff.Changes[i]
		if summary, isLock := summaries[f.Path]; isLock {
			key := changeLinesKey(change, summary)
			if first, seen := firstWith[key]; seen {
				change.Content = "(identical +/- lines and summary to " + first + "; context omitted)"
				out.Changes = append(out.Changes, change)
				continue
			}
			firstWith[key] = f.Path
			pieces := splitLockfilePatch(change.Content, budget)
			for j, piece := range pieces {
				part := change
				part.Content = piece
				if len(pieces) > 1 {
					part.Content = fmt.Sprintf("(lockfile patch piece %d of %d)\n%s", j+1, len(pieces), piece)
				}
				out.Changes = append(out.Changes, part)
			}
			continue
		}
		if reason, _ := r.generated.classify(f); reason != "" {
			out.Withheld = append(out.Withheld, GeneratedFile{
				Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Reason: reason,
			})
			continue
		}
		out.Changes = append(out.Changes, change)
	}
	if len(out.Withheld) > 0 && r.policy.GeneratedFiles != nil && r.policy.GeneratedFiles.Note != "" {
		out.ReviewNotes = append(out.ReviewNotes, r.policy.GeneratedFiles.Note)
	}
	return out
}

// runReviewers runs every agent concurrently and records each verdict. It
// passes only when every agent passes.
func (r *PullRequestReviewer) runReviewers(out *PullRequestReview, diff Diff, budget int, area ReviewAreaPolicy, lowRisk bool, conv *convergence, add func(string, bool, string) bool) (bool, []ACRResult) {
	results := make([]ACRResult, len(r.agents))
	var wg sync.WaitGroup
	for i, agent := range r.agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Only a described agent is memoized: the key names the
			// reviewer, and two anonymous agents must not share verdicts.
			if name := describeReviewAgent(agent); name != "" {
				agent = memoAgent{agent: agent, name: name, memo: r.memo}
			}
			results[i] = reviewInChunks(agent, diff, budget)
		}()
	}
	wg.Wait()

	known := map[string]bool{}
	if diff.Criteria != nil {
		for _, id := range diff.Criteria.RuleIDs {
			known[id] = true
		}
	}
	for i := range results {
		// Only an agent that reads the repository has repository access,
		// whatever its model says.
		if ra, ok := r.agents[i].(RepoAccessAgent); (!ok || !ra.RepoAccess()) && results[i].Coverage != nil {
			// A copy: a reused or shared verdict may hold the same pointer.
			coverage := *results[i].Coverage
			coverage.RepoAccess = false
			results[i].Coverage = &coverage
		}
		unknown := clearUnknownRuleIDs(&results[i], known)
		if out.Criteria != nil {
			out.Criteria.UnknownRuleCitations = append(out.Criteria.UnknownRuleCitations, unknown...)
		}
	}

	// Clarification outcomes change what the verdict counts, never what
	// the reviewers reported; results stay as they were for the record.
	blocking := r.policy.blockingSeverities()
	counted := make([]ACRResult, len(results))
	var asserted []FindingOutcome
	var held []HeldFinding
	var stillBlocking []ReviewFinding
	for i, res := range results {
		var a []FindingOutcome
		var h []HeldFinding
		counted[i], a = applyClarifications(r.clarifications, res, blocking)
		// Then a later round holds back what it raised late or a person
		// already resolved.
		counted[i], h = conv.apply(counted[i], blocking)
		asserted = append(asserted, a...)
		held = append(held, h...)
		for _, f := range counted[i].Findings {
			if containsFold(blocking, f.Severity) {
				stillBlocking = append(stillBlocking, f)
			}
		}
	}
	passed := 0
	for i, res := range counted {
		ok, reason := r.reviewerPasses(describeReviewAgent(r.agents[i]), res, diff, area, lowRisk)
		if ok {
			passed++
		}
		out.Reviews = append(out.Reviews, PullRequestReviewerVerdict{
			Reviewer: describeReviewAgent(r.agents[i]), Passed: ok, Reason: reason, Agent: results[i],
		})
	}
	out.Findings = mergeReviewerFindings(results, func(i int) string { return describeReviewAgent(r.agents[i]) })
	out.Assumptions = mergeReviewerAssumptions(results, func(i int) string { return describeReviewAgent(r.agents[i]) })
	out.Clarifications = recordClarifications(r.clarifications, out.Findings)
	if conv != nil {
		conv.recordHeld(out.Findings, held, stillBlocking)
		out.Convergence = &conv.record
	}
	var reviewed bool
	if len(results) == 1 {
		out.Agent = results[0]
		reason := out.Reviews[0].Reason
		out.Reviews = nil
		reviewed = add("pr.review-agent", passed == 1, reason)
	} else {
		out.Agent = mergeVerdicts(results, func(i int) string { return describeReviewAgent(r.agents[i]) })
		for _, review := range out.Reviews {
			add("pr.review-agent", review.Passed, review.Reviewer+": "+review.Reason)
		}
		reviewed = add("pr.review-consensus", passed == len(results),
			fmt.Sprintf("%d of %d reviewers passed; every reviewer must", passed, len(results)))
	}
	if conv != nil {
		reviewed = convergenceGate(conv, add) && reviewed
	}
	if r.clarifications == nil {
		return reviewed, results
	}
	// A blocking finding resolved only on someone's word needs a person to
	// accept that word: an author cannot talk Radar into approving.
	if len(asserted) > 0 {
		return add("clarification.assertion", false, fmt.Sprintf("%d blocking finding(s) resolved only by %s's assertion; a person must accept it", len(asserted), asserted[0].Author)) && reviewed, results
	}
	add("clarification.assertion", true, "no blocking finding is resolved by assertion alone")
	return reviewed, results
}

// defectSignals are the risk signals that describe a likely problem rather
// than the change's size or shape; only they block under review-findings.
var defectSignals = map[ChangeSignal]bool{
	SignalBugOrLogicError: true,
	SignalPerformanceRisk: true,
	SignalSecretsExposure: true,
	SignalSQLInjection:    true,
	SignalAuthBypass:      true,
}

func (r *PullRequestReviewer) reviewerPasses(reviewer string, res ACRResult, diff Diff, area ReviewAreaPolicy, lowRisk bool) (bool, string) {
	minConfidence := r.policy.MinReviewConfidence
	if r.policy.basis() == ApprovalBasisReviewFindings {
		minConfidence = r.policy.minConfidenceFor(reviewer)
		if lowRisk {
			minConfidence = max(min(minConfidence, r.policy.LowRiskMinReviewConfidence), r.policy.MinReviewConfidence-maxConfidenceRelief)
		}
	}
	return reviewerVerdict(r.policy, area, minConfidence, false, res, diff)
}

// reviewerVerdict applies the policy's bar, tightened by the area policy, for
// one reviewer. waiveClaim drops the accept-claim requirement under
// allow-rules only, unless the area policy makes declines block; a decline
// always blocks under review-findings.
func reviewerVerdict(p PullRequestPolicy, area ReviewAreaPolicy, minConfidence int, waiveClaim bool, res ACRResult, diff Diff) (bool, string) {
	blocking := p.blockingSeverities()
	hasBlocking := hasBlockingFinding(res.Findings, blocking)
	overLimit := area.exceedsFindingLimits(res.Findings)
	waiveClaim = waiveClaim && !area.DeclineBlocks
	covered := reviewCoversDiff(res, diff) && !res.Truncated
	confident := res.Confidence >= minConfidence
	var passed bool
	var detail string
	switch p.basis() {
	case ApprovalBasisReviewFindings:
		var defects []string
		for _, s := range res.RiskSignals {
			if defectSignals[s] {
				defects = append(defects, string(s))
			}
		}
		// The verdict is computed from the review's fields; there is no
		// accept claim to decline with. A structural change needs
		// a human, as the declines that used to say so did.
		structural := false
		for _, s := range res.RiskSignals {
			structural = structural || s == SignalStructuralChange
		}
		needsHuman := res.NeedsHuman != nil && res.NeedsHuman.Required
		passed = confident && covered && !hasBlocking && len(defects) == 0 && !structural && !needsHuman && !res.Failed
		detail = fmt.Sprintf("confidence=%d/%d defect-signals=%v structural-change=%t needs-human=%t", res.Confidence, minConfidence, defects, structural, needsHuman)
		if needsHuman {
			detail += " (" + res.NeedsHuman.Reason + ")"
		}
	default:
		claimed := res.Accept || res.ModelAccept
		// A tier may waive the accept claim, never a reviewer's call for a human.
		needsHuman := res.NeedsHuman != nil && res.NeedsHuman.Required
		passed = (claimed || waiveClaim) && confident && len(res.RiskSignals) == 0 && len(res.SafeSignals) > 0 && covered && !hasBlocking && !needsHuman
		detail = fmt.Sprintf("accept=%t confidence=%d/%d risk-signals=%d", claimed, res.Confidence, minConfidence, len(res.RiskSignals))
	}
	if overLimit != "" {
		passed = false
		detail += " area-finding-limit=" + overLimit
	}
	return passed, fmt.Sprintf("%s reviewed-files=%d/%d blocking-findings(%s)=%t: %s",
		detail, len(res.ReviewedFiles), len(diff.Changes), strings.Join(blocking, ","), hasBlocking, res.Summary)
}

func (r *PullRequestReviewer) describeReviewers() string {
	names := make([]string, 0, len(r.agents))
	for _, agent := range r.agents {
		names = append(names, describeReviewAgent(agent))
	}
	return strings.Join(names, "+")
}

// reviewCoversDiff requires the agent to name exactly the files it was
// shown; a file split into several pieces counts once.
func reviewCoversDiff(result ACRResult, diff Diff) bool {
	want := map[string]bool{}
	for _, change := range diff.Changes {
		want[change.File] = true
	}
	got := map[string]bool{}
	for _, file := range result.ReviewedFiles {
		if !want[file] {
			return false
		}
		got[file] = true
	}
	return len(got) == len(want)
}

// blockingSeverities returns the finding severities that block approval,
// defaulting to P0-P2 for policies that predate the setting.
func (p PullRequestPolicy) blockingSeverities() []string {
	if len(p.BlockingFindingSeverities) == 0 {
		return []string{"P0", "P1", "P2"}
	}
	out := make([]string, 0, len(p.BlockingFindingSeverities))
	for _, s := range p.BlockingFindingSeverities {
		out = append(out, strings.ToUpper(s))
	}
	return out
}

func hasBlockingFinding(findings []ReviewFinding, blocking []string) bool {
	for _, finding := range findings {
		if containsFold(blocking, finding.Severity) {
			return true
		}
	}
	return false
}

func reviewStateGate(policy PullRequestPolicy, in PullRequestInput) (bool, string) {
	switch {
	case strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.HeadSHA) == "":
		return false, "request id and head SHA are required"
	case !in.Open:
		return false, "pull request is not open"
	case in.Draft:
		return false, "pull request is a draft"
	case !in.SameRepository:
		return false, "cross-repository pull requests are not eligible"
	case !matchesAnyPath(in.BaseRef, policy.AllowedBaseBranches) && len(in.Stack) == 0:
		return false, "base branch is not allowed"
	case len(in.Stack) > 0 && !policy.AllowStacked:
		return false, "stacked pull requests are not enabled by policy"
	case !in.ChecksObserved:
		return false, "no checks were observed"
	case strings.TrimSpace(in.CheckFingerprint) == "":
		return false, "check fingerprint is missing"
	case !in.ChecksStable:
		return false, "check surface is not stable"
	case !in.ChecksPassing:
		return false, "checks are not passing"
	case in.UnresolvedThreads > 0:
		return false, fmt.Sprintf("%d unresolved review thread(s)", in.UnresolvedThreads)
	case in.ChangesRequested:
		return false, "changes have been requested"
	case policy.Mode == PullRequestModeApprove && !in.ApprovalsKeptCurrent:
		return false, "approval mode requires that a push dismisses approvals, or that the base branch requires Radar's " + ApprovalCurrentContext + " status"
	}
	if len(in.Stack) > 0 {
		if err := validateStack(policy, in); err != nil {
			return false, err.Error()
		}
		root := in.Stack[len(in.Stack)-1].BaseRef
		return true, fmt.Sprintf("pull request state is eligible; stacked %d deep on %s", len(in.Stack), root)
	}
	return true, "pull request state is eligible"
}

// findingsStateGate is reviewStateGate without the gates a findings-only
// review must not wait for: checks, review threads and requested changes.
// Drafts are reviewed.
func findingsStateGate(policy PullRequestPolicy, in PullRequestInput) (bool, string) {
	switch {
	case strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.HeadSHA) == "":
		return false, "request id and head SHA are required"
	case !in.Open:
		return false, "pull request is not open"
	case !in.SameRepository:
		return false, "cross-repository pull requests are not eligible"
	case !matchesAnyPath(in.BaseRef, policy.AllowedBaseBranches) && len(in.Stack) == 0:
		return false, "base branch is not allowed"
	case len(in.Stack) > 0 && !policy.AllowStacked:
		return false, "stacked pull requests are not enabled by policy"
	}
	if len(in.Stack) > 0 {
		if err := validateStack(policy, in); err != nil {
			return false, err.Error()
		}
	}
	return true, "pull request is open for review; checks are not awaited at the findings stage"
}

// validateStack checks the stack is a chain of distinct open pull requests
// from this one's base down to an allowed base branch.
func validateStack(policy PullRequestPolicy, in PullRequestInput) error {
	want := in.BaseRef
	seen := map[int]bool{in.Number: true}
	for _, entry := range in.Stack {
		switch {
		case entry.Number <= 0 || seen[entry.Number]:
			return fmt.Errorf("stack repeats or omits a pull request number")
		case entry.HeadRef != want:
			return fmt.Errorf("stack is broken: expected #%d to have head %s, found %s", entry.Number, want, entry.HeadRef)
		case strings.TrimSpace(entry.HeadSHA) == "":
			return fmt.Errorf("stack entry #%d has no head SHA", entry.Number)
		}
		seen[entry.Number] = true
		want = entry.BaseRef
	}
	if !matchesAnyPath(want, policy.AllowedBaseBranches) {
		return fmt.Errorf("stack bottoms out on %s, which is not an allowed base branch", want)
	}
	return nil
}

func pullRequestDiff(in PullRequestInput) (Diff, bool, string) {
	if len(in.Files) == 0 {
		return Diff{}, false, "pull request has no changed files"
	}
	changes := make([]Change, 0, len(in.Files))
	seen := map[string]bool{}
	for _, f := range in.Files {
		if err := validateRepositoryPath(f.Path); err != nil {
			return Diff{}, false, err.Error()
		}
		if f.PreviousPath != "" {
			if err := validateRepositoryPath(f.PreviousPath); err != nil {
				return Diff{}, false, err.Error()
			}
		}
		if seen[f.Path] {
			return Diff{}, false, "provider returned duplicate changed path " + f.Path
		}
		seen[f.Path] = true
		if f.Additions < 0 || f.Deletions < 0 {
			return Diff{}, false, "provider returned negative line counts for " + f.Path
		}
		if !f.ContentComplete {
			return Diff{}, false, "provider did not return a complete patch for " + f.Path
		}
		changes = append(changes, Change{
			File: f.Path, PreviousFile: f.PreviousPath, Type: f.Status,
			Additions: f.Additions, Deletions: f.Deletions, Content: f.Patch,
		})
	}
	return Diff{
		ID: in.ID, Org: in.Repository, Source: SourceHuman,
		Author: Author{Name: in.Author}, CI: CIPassing, State: DiffPublished,
		Changes: changes,
	}, true, "complete provider diff"
}

func pullRequestDenied(policy PullRequestPolicy, in PullRequestInput, stage string) (bool, string) {
	for _, f := range in.Files {
		for _, candidate := range []string{f.Path, f.PreviousPath} {
			if candidate != "" && matchesAnyPathFold(candidate, policy.DenyPaths) {
				return true, "path is explicitly denied: " + candidate
			}
		}
	}
	text := strings.ToLower(in.Title + "\n" + in.Body)
	// A commit has no title or description. Its messages are the words its
	// author wrote, from which GitHub would fill a one-commit pull request's
	// title and description, so every one is matched in full.
	if stage == StageCommit && in.Context != nil {
		for _, m := range in.Context.Commits {
			text += "\n" + strings.ToLower(m)
		}
	}
	for _, f := range in.Files {
		text += "\n" + strings.ToLower(f.Patch)
	}
	for _, phrase := range policy.DenyPhrases {
		if phrase != "" && strings.Contains(text, strings.ToLower(phrase)) {
			return true, "content contains denied phrase: " + phrase
		}
	}
	return false, "no explicit deny rule matched"
}

func matchPullRequestRule(policy PullRequestPolicy, in PullRequestInput) (*PullRequestPathRule, string) {
	lines := 0
	for _, f := range in.Files {
		lines += f.Additions + f.Deletions
	}
	if len(in.Files) > policy.MaxFiles {
		return nil, fmt.Sprintf("%d files exceeds global limit %d", len(in.Files), policy.MaxFiles)
	}
	if lines > policy.MaxChangedLines {
		return nil, fmt.Sprintf("%d changed lines exceeds global limit %d", lines, policy.MaxChangedLines)
	}

	for i := range policy.AllowRules {
		rule := &policy.AllowRules[i]
		if rule.MaxFiles > 0 && len(in.Files) > rule.MaxFiles {
			continue
		}
		if rule.MaxChangedLines > 0 && lines > rule.MaxChangedLines {
			continue
		}
		ok := true
		for _, f := range in.Files {
			if !matchesAnyPath(f.Path, rule.Include) || matchesAnyPath(f.Path, rule.Exclude) ||
				!containsFold(rule.Statuses, f.Status) {
				ok = false
				break
			}
			if f.PreviousPath != "" && (!matchesAnyPath(f.PreviousPath, rule.Include) || matchesAnyPath(f.PreviousPath, rule.Exclude)) {
				ok = false
				break
			}
		}
		if ok {
			return rule, "matched allow rule: " + rule.Name
		}
	}
	return nil, "no allow rule covers the complete change"
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func validateRepositoryPath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("invalid repository path %q", value)
	}
	return nil
}

// MatchesPathGlob reports whether value matches any of Radar's path globs.
func MatchesPathGlob(value string, patterns []string) bool { return matchesAnyPath(value, patterns) }

func matchesAnyPath(value string, patterns []string) bool {
	for _, pattern := range patterns {
		re, err := compilePathGlob(pattern)
		if err == nil && re.MatchString(value) {
			return true
		}
	}
	return false
}

// matchesAnyPathFold reports whether value matches any pattern as written or
// with both lowercased, so "**/*iam*" also guards IAMPolicy.java.
// Only the guards use it, deny paths and area paths, where matching more
// files can only send more pull requests to a human; excludes, allow rules,
// generated files and base branches keep the exact match, where matching
// more would relax a gate.
func matchesAnyPathFold(value string, patterns []string) bool {
	if matchesAnyPath(value, patterns) {
		return true
	}
	lower := make([]string, len(patterns))
	for i, p := range patterns {
		lower[i] = strings.ToLower(p)
	}
	return matchesAnyPath(strings.ToLower(value), lower)
}

func compilePathGlob(glob string) (*regexp.Regexp, error) {
	if glob == "" || strings.HasPrefix(glob, "/") || strings.Contains(glob, "\\") || strings.Contains(glob, "..") {
		return nil, fmt.Errorf("invalid path pattern %q", glob)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				if i+2 < len(glob) && glob[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		case '[':
			class, n, err := globClass(glob[i:])
			if err != nil {
				return nil, fmt.Errorf("invalid path pattern %q: %w", glob, err)
			}
			b.WriteString(class)
			i += n
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// globClass translates a fnmatch character class at the start of s ("[a-z]",
// "[!0-9]", "[]x]") to a regular expression that, as in git, never matches
// "/". It returns the expression and how many bytes of s it consumed. An
// unclosed "[" is a literal, as in fnmatch.
func globClass(s string) (string, int, error) {
	j := 1
	negate := j < len(s) && (s[j] == '!' || s[j] == '^')
	if negate {
		j++
	}
	start := j
	if j < len(s) && s[j] == ']' {
		j++
	}
	end := strings.IndexByte(s[j:], ']')
	if end < 0 {
		return regexp.QuoteMeta("["), 1, nil
	}
	end += j
	var members strings.Builder
	for _, r := range s[start:end] {
		switch r {
		case '/':
			continue
		case '\\', ']', '[', '^':
			members.WriteRune('\\')
		}
		members.WriteRune(r)
	}
	if negate {
		return "[^/" + members.String() + "]", end + 1, nil
	}
	if members.Len() == 0 {
		return "", 0, fmt.Errorf("character class matches nothing")
	}
	return "[" + members.String() + "]", end + 1, nil
}

// SortedIgnoredChecks returns the configured check exclusions in stable order.
func (p PullRequestPolicy) SortedIgnoredChecks() []string {
	checks := append([]string(nil), p.IgnoredChecks...)
	sort.Strings(checks)
	return checks
}
