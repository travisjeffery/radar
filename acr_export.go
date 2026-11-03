package radar

// ACRSystemPrompt is the review instruction every API-backed agent sends, so
// agents outside this package review against the same criteria.
const ACRSystemPrompt = acrSystemPrompt

// RenderDiffForReview formats a diff the way every API-backed agent sends it.
func RenderDiffForReview(d Diff) string { return renderDiffForReview(d) }

// ACRVerdictSchema is the JSON Schema of the verdict agents must return.
func ACRVerdictSchema() map[string]any { return acrVerdictSchema() }

// ParseACRVerdict validates a verdict and applies Radar's acceptance rule.
func ParseACRVerdict(text string) (ACRResult, error) { return parseACRVerdict(text) }
