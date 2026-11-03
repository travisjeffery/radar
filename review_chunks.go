package radar

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// defaultReviewChunkChars keeps one request well inside a 128k-token context
// (roughly four characters per token) with room for the prompt and verdict.
const defaultReviewChunkChars = 240_000

// maxConcurrentChunks bounds parallel requests per reviewer so a large diff
// fits the job deadline without bursting the provider's rate limit.
const maxConcurrentChunks = 3

// chunkOverheadChars approximates the per-change header renderDiffForReview adds.
const chunkOverheadChars = 64

// chunkDiff splits d into parts whose patch text fits budget, keeping each
// file whole. A single file over budget cannot be reviewed and is an error,
// so it fails safe instead of being truncated.
func chunkDiff(d Diff, budget int) ([]Diff, error) {
	var parts [][]Change
	size := 0
	for _, c := range d.Changes {
		n := len(c.Content) + len(c.File) + chunkOverheadChars
		if n > budget {
			return nil, fmt.Errorf("patch for %s is %d characters, over the %d-character review budget", c.File, n, budget)
		}
		if len(parts) == 0 || size+n > budget {
			parts = append(parts, nil)
			size = 0
		}
		parts[len(parts)-1] = append(parts[len(parts)-1], c)
		size += n
	}
	if len(parts) <= 1 {
		return []Diff{d}, nil
	}
	out := make([]Diff, len(parts))
	for i, changes := range parts {
		part := d
		part.Changes = changes
		part.Part, part.Parts = i+1, len(parts)
		out[i] = part
	}
	return out, nil
}

// maxTruncationSplits bounds how many times a part whose review ran out of
// output is halved and retried.
const maxTruncationSplits = 3

// minSplitChars is the smallest part worth halving after a truncation. A
// small part that still runs out of output is reasoning that will not end,
// and halving it only multiplies the cost (seen on a 22-line lockfile bump:
// seven truncated requests).
const minSplitChars = 16_000

// reviewInChunks runs agent over d, splitting it when it exceeds budget, and
// merges the parts into one verdict.
func reviewInChunks(agent ReviewAgent, d Diff, budget int) ACRResult {
	parts, err := chunkDiff(d, budget)
	if err != nil {
		return ACRResult{Summary: "diff not reviewed, failing safe: " + err.Error(), Failed: true}
	}
	// One semaphore bounds every request this review makes, parts and the
	// halves of truncated parts alike, so nested splits cannot burst past it.
	limit := 1
	if concurrentReviewSafe(agent) {
		limit = maxConcurrentChunks
	}
	sem := make(chan struct{}, limit)
	if len(parts) == 1 {
		return reviewPart(agent, parts[0], 0, sem)
	}
	start := time.Now()
	results := make([]ACRResult, len(parts))
	var wg sync.WaitGroup
	for i, part := range parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = reviewPart(agent, part, 0, sem)
		}()
	}
	wg.Wait()
	merged := mergeVerdicts(results, func(i int) string { return fmt.Sprintf("part %d/%d", i+1, len(results)) })
	elapsed := time.Since(start).Milliseconds()
	merged.ElapsedMS = &elapsed
	return merged
}

// reviewPart reviews one part and, when the agent ran out of output before
// finishing, reviews it again as two halves instead of failing. A request
// holds a slot of sem only while it runs, never while waiting for halves.
func reviewPart(agent ReviewAgent, part Diff, depth int, sem chan struct{}) ACRResult {
	sem <- struct{}{}
	res := agent.Review(part)
	<-sem
	if !res.Truncated || depth >= maxTruncationSplits || patchChars(part) < minSplitChars {
		return res
	}
	halves := halveDiff(part)
	if halves == nil {
		return res
	}
	// Halves run together as far as sem allows: one after the other, a
	// split review of a slow model took three deadlines end to end.
	results := make([]ACRResult, 2)
	var wg sync.WaitGroup
	for i := range halves {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = reviewPart(agent, halves[i], depth+1, sem)
		}()
	}
	wg.Wait()
	merged := mergeVerdicts(results, func(i int) string { return fmt.Sprintf("half %d/2", i+1) })
	merged.Usage = res.Usage.add(merged.Usage)
	return merged
}

func patchChars(d Diff) int {
	n := 0
	for _, c := range d.Changes {
		n += len(c.Content)
	}
	return n
}

// halveDiff splits a part's changes into two, or a single change's patch at
// a hunk or line boundary. It returns nil when there is nothing to split.
func halveDiff(d Diff) []Diff {
	// Without the last sentence a reviewer asked for a human just because
	// it saw half the change.
	note := "This part was too large to review in one response and is split in two; judge only the patches shown. Radar reviews the other half separately and combines both, so the split itself is not a finding and not a reason to ask for a human reviewer."
	mk := func(changes []Change) Diff {
		h := d
		h.Changes = changes
		h.ReviewNotes = append(append([]string{}, d.ReviewNotes...), note)
		return h
	}
	if len(d.Changes) > 1 {
		mid := len(d.Changes) / 2
		return []Diff{mk(d.Changes[:mid]), mk(d.Changes[mid:])}
	}
	if len(d.Changes) == 1 {
		pieces := splitLockfilePatch(d.Changes[0].Content, len(d.Changes[0].Content)/2+1)
		if len(pieces) < 2 {
			return nil
		}
		first, rest := d.Changes[0], d.Changes[0]
		first.Content = pieces[0]
		rest.Content = strings.Join(pieces[1:], "")
		return []Diff{mk([]Change{first}), mk([]Change{rest})}
	}
	return nil
}

// mergeVerdicts combines verdicts conservatively: every part must accept, the
// lowest confidence wins, and signals, findings and reviewed files accumulate.
func mergeVerdicts(results []ACRResult, label func(int) string) ACRResult {
	merged := ACRResult{Accept: len(results) > 0, ModelAccept: len(results) > 0, Confidence: ACRMaxConfidence}
	seenRisk := map[ChangeSignal]bool{}
	seenSafe := map[ChangeSignal]bool{}
	summaries := make([]string, 0, len(results))
	for i, res := range results {
		merged.Accept = merged.Accept && res.Accept
		merged.ModelAccept = merged.ModelAccept && (res.ModelAccept || res.Accept)
		merged.Confidence = min(merged.Confidence, res.Confidence)
		for _, s := range res.RiskSignals {
			if !seenRisk[s] {
				seenRisk[s] = true
				merged.RiskSignals = append(merged.RiskSignals, s)
			}
		}
		for _, s := range res.SafeSignals {
			if !seenSafe[s] {
				seenSafe[s] = true
				merged.SafeSignals = append(merged.SafeSignals, s)
			}
		}
		merged.ReviewedFiles = append(merged.ReviewedFiles, res.ReviewedFiles...)
		merged.Findings = append(merged.Findings, res.Findings...)
		merged.WhereToLook = append(merged.WhereToLook, res.WhereToLook...)
		merged.Questions = append(merged.Questions, res.Questions...)
		merged.Assumptions = append(merged.Assumptions, res.Assumptions...)
		merged.TestsSuggested = append(merged.TestsSuggested, res.TestsSuggested...)
		merged.Risk = higherRisk(merged.Risk, res.Risk)
		merged.Coverage = mergeCoverage(merged.Coverage, res.Coverage, label(i))
		merged.Truncated = merged.Truncated || res.Truncated
		merged.Failed = merged.Failed || res.Failed
		if res.NeedsHuman != nil && res.NeedsHuman.Required && merged.NeedsHuman == nil {
			nh := *res.NeedsHuman
			nh.Reason = label(i) + ": " + nh.Reason
			merged.NeedsHuman = &nh
		}
		merged.Usage = merged.Usage.add(res.Usage)
		summaries = append(summaries, label(i)+": "+res.Summary)
	}
	if len(results) == 0 {
		merged.Confidence = 0
	}
	merged.Summary = strings.Join(summaries, " | ")
	return merged
}

var riskRank = map[string]int{"low": 1, "medium": 2, "high": 3}

// higherRisk keeps the higher of two risk assessments.
func higherRisk(a, b *ReviewRisk) *ReviewRisk {
	if a == nil || (b != nil && riskRank[b.Level] > riskRank[a.Level]) {
		return b
	}
	return a
}

// mergeCoverage has repository access only when every part had it, and keeps
// each part's limits, labelled.
func mergeCoverage(a, b *ReviewCoverage, label string) *ReviewCoverage {
	if b == nil {
		return a
	}
	limits := b.Limits
	if limits != "" {
		limits = label + ": " + limits
	}
	if a == nil {
		return &ReviewCoverage{RepoAccess: b.RepoAccess, Limits: limits, ToolCalls: b.ToolCalls, Tools: addToolUses(nil, b.Tools)}
	}
	out := &ReviewCoverage{RepoAccess: a.RepoAccess && b.RepoAccess, Limits: a.Limits, ToolCalls: a.ToolCalls + b.ToolCalls,
		Tools: addToolUses(addToolUses(nil, a.Tools), b.Tools)}
	if limits != "" {
		if out.Limits != "" {
			out.Limits += "; "
		}
		out.Limits += limits
	}
	return out
}

// addToolUses sums b's per-tool counts into a copy of a.
func addToolUses(a, b map[string]ToolUse) map[string]ToolUse {
	if len(b) == 0 {
		return a
	}
	if a == nil {
		a = make(map[string]ToolUse, len(b))
	}
	for tool, u := range b {
		t := a[tool]
		t.Calls += u.Calls
		t.Bytes += u.Bytes
		a[tool] = t
	}
	return a
}
