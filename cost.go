package radar

import "strings"

// modelPrice is a model's list price in US dollars per million tokens.
type modelPrice struct {
	input, cacheRead, cacheWrite, output float64
}

// reviewModelPrices are the reviewer models Radar runs, matched by a
// substring of the reviewer's provenance. The rates are Bedrock's US
// regional on-demand prices; cache writes are the five-minute rate.
var reviewModelPrices = []struct {
	model string
	price modelPrice
}{
	{"anthropic.claude-opus-5-5", modelPrice{input: 4.40, cacheRead: 0.22, cacheWrite: 5.50, output: 22}},
	{"openai.gpt-6.1-sol", modelPrice{input: 2.20, cacheRead: 0.11, cacheWrite: 2.75, output: 11}},
}

// EstimateReviewCostUSD estimates what u cost for reviewer, a provenance
// such as bedrock/us.anthropic.claude-opus-5-5. InputTokens includes the
// cached input, as every agent reports it. It returns false for a model
// without a price. A reused review's usage has no tokens and costs nothing.
func EstimateReviewCostUSD(reviewer string, u *ReviewUsage) (float64, bool) {
	if u == nil {
		return 0, true
	}
	for _, m := range reviewModelPrices {
		if !strings.Contains(reviewer, m.model) {
			continue
		}
		p := m.price
		uncached := max(u.InputTokens-u.CacheReadTokens-u.CacheWriteTokens, 0)
		return (float64(uncached)*p.input + float64(u.CacheReadTokens)*p.cacheRead +
			float64(u.CacheWriteTokens)*p.cacheWrite + float64(u.OutputTokens)*p.output) / 1e6, true
	}
	return 0, u.InputTokens == 0 && u.OutputTokens == 0
}
