package radar

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
)

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
)

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
	ID                      string            `json:"id"`
	Repository              string            `json:"repository"`
	Number                  int               `json:"number"`
	Title                   string            `json:"title"`
	Body                    string            `json:"body,omitempty"`
	BaseRef                 string            `json:"base_ref"`
	HeadSHA                 string            `json:"head_sha"`
	Author                  string            `json:"author"`
	AuthorType              string            `json:"author_type,omitempty"`
	Open                    bool              `json:"open"`
	Draft                   bool              `json:"draft"`
	SameRepository          bool              `json:"same_repository"`
	ChecksObserved          bool              `json:"checks_observed"`
	ChecksStable            bool              `json:"checks_stable"`
	ChecksPassing           bool              `json:"checks_passing"`
	CheckFingerprint        string            `json:"check_fingerprint,omitempty"`
	UnresolvedThreads       int               `json:"unresolved_threads"`
	ChangesRequested        bool              `json:"changes_requested"`
	StaleApprovalsDismissed bool              `json:"stale_approvals_dismissed"`
	Files                   []PullRequestFile `json:"files"`
}

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
	Agent          ACRResult         `json:"agent"`
	Stages         []StageResult     `json:"stages"`
}

// PullRequestReviewer applies a policy with pluggable Radar scoring and review.
type PullRequestReviewer struct {
	policy     PullRequestPolicy
	scorer     RiskScorer
	calibrator *Calibrator
	agent      ReviewAgent
}

// NewPullRequestReviewer validates policy and constructs a generic PR reviewer.
func NewPullRequestReviewer(policy PullRequestPolicy, scorer RiskScorer, agent ReviewAgent) (*PullRequestReviewer, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if scorer == nil {
		return nil, fmt.Errorf("radar: pull request scorer is required")
	}
	if agent == nil {
		return nil, fmt.Errorf("radar: pull request review agent is required")
	}
	return &PullRequestReviewer{
		policy:     policy,
		scorer:     scorer,
		calibrator: NewCalibrator(policy.CalibrationSample),
		agent:      agent,
	}, nil
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
	if len(p.AllowRules) == 0 {
		return fmt.Errorf("radar: at least one allow rule is required")
	}
	if p.MaxFiles <= 0 || p.MaxChangedLines <= 0 {
		return fmt.Errorf("radar: positive global file and line limits are required")
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
	for _, pattern := range p.DenyPaths {
		if _, err := compilePathGlob(pattern); err != nil {
			return err
		}
	}
	return nil
}

// Review evaluates current PR state. It never performs provider mutations.
func (r *PullRequestReviewer) Review(in PullRequestInput) PullRequestReview {
	out := PullRequestReview{
		SchemaVersion:  1,
		RequestID:      in.ID,
		HeadSHA:        in.HeadSHA,
		PolicyVersion:  r.policy.Version,
		Mode:           r.policy.Mode,
		Action:         PullRequestRouteToHuman,
		RiskPercentile: -1,
	}
	add := func(name string, passed bool, reason string) bool {
		out.Stages = append(out.Stages, StageResult{Name: name, Passed: passed, Reason: reason})
		return passed
	}

	if ok, reason := reviewStateGate(r.policy, in); !add("pr.state", ok, reason) {
		return out
	}

	diff, complete, reason := pullRequestDiff(in)
	if !add("pr.complete-diff", complete, reason) {
		return out
	}

	denied, denyReason := pullRequestDenied(r.policy, in)
	add("pr.deny-policy", !denied, denyReason)

	rule, allowReason := matchPullRequestRule(r.policy, in)
	allowlisted := rule != nil
	add("pr.allow-rule", allowlisted, allowReason)
	if rule != nil {
		out.MatchedRule = rule.Name
	}

	out.RawRiskScore = r.scorer.Score(diff)
	if math.IsNaN(out.RawRiskScore) || math.IsInf(out.RawRiskScore, 0) {
		add("pr.risk-threshold", false, "risk scorer returned a non-finite value")
		return out
	}
	out.RiskPercentile = r.calibrator.Percentile(out.RawRiskScore)
	riskPassed := out.RiskPercentile <= r.policy.MaxRiskPercentile
	add("pr.risk-threshold", riskPassed,
		fmt.Sprintf("risk percentile %.1f vs threshold P%.1f", out.RiskPercentile, r.policy.MaxRiskPercentile))

	out.Agent = r.agent.Review(diff)
	agentPassed := out.Agent.Accept && out.Agent.Confidence >= r.policy.MinReviewConfidence &&
		len(out.Agent.RiskSignals) == 0 && len(out.Agent.SafeSignals) > 0 &&
		reviewCoversDiff(out.Agent, diff) && !hasBlockingFinding(out.Agent.Findings)
	add("pr.review-agent", agentPassed,
		fmt.Sprintf("accept=%t confidence=%d/%d risk-signals=%d reviewed-files=%d/%d blocking-findings=%t: %s",
			out.Agent.Accept, out.Agent.Confidence, r.policy.MinReviewConfidence, len(out.Agent.RiskSignals),
			len(out.Agent.ReviewedFiles), len(diff.Changes), hasBlockingFinding(out.Agent.Findings), out.Agent.Summary))

	out.Eligible = !denied && allowlisted && riskPassed
	if out.Eligible && agentPassed {
		if r.policy.Mode == PullRequestModeApprove {
			out.Action = PullRequestApprove
		} else {
			out.Action = PullRequestWouldApprove
		}
		return out
	}
	if !denied && !out.Eligible && agentPassed {
		out.Action = PullRequestPolicyCandidate
	}
	return out
}

func reviewCoversDiff(result ACRResult, diff Diff) bool {
	want := map[string]int{}
	for _, change := range diff.Changes {
		want[change.File]++
	}
	got := map[string]int{}
	for _, file := range result.ReviewedFiles {
		got[file]++
	}
	if len(want) != len(got) {
		return false
	}
	for file, count := range want {
		if got[file] != count {
			return false
		}
	}
	return true
}

func hasBlockingFinding(findings []ReviewFinding) bool {
	for _, finding := range findings {
		if strings.EqualFold(finding.Severity, "P0") || strings.EqualFold(finding.Severity, "P1") || strings.EqualFold(finding.Severity, "P2") {
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
	case !matchesAnyPath(in.BaseRef, policy.AllowedBaseBranches):
		return false, "base branch is not allowed"
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
	case policy.Mode == PullRequestModeApprove && !in.StaleApprovalsDismissed:
		return false, "approval mode requires stale approvals to be dismissed"
	}
	return true, "pull request state is eligible"
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

func pullRequestDenied(policy PullRequestPolicy, in PullRequestInput) (bool, string) {
	for _, f := range in.Files {
		for _, candidate := range []string{f.Path, f.PreviousPath} {
			if candidate != "" && matchesAnyPath(candidate, policy.DenyPaths) {
				return true, "path is explicitly denied: " + candidate
			}
		}
	}
	text := strings.ToLower(in.Title + "\n" + in.Body)
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

func matchesAnyPath(value string, patterns []string) bool {
	for _, pattern := range patterns {
		re, err := compilePathGlob(pattern)
		if err == nil && re.MatchString(value) {
			return true
		}
	}
	return false
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
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// SortedIgnoredChecks returns the configured check exclusions in stable order.
func (p PullRequestPolicy) SortedIgnoredChecks() []string {
	checks := append([]string(nil), p.IgnoredChecks...)
	sort.Strings(checks)
	return checks
}
