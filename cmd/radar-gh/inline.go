package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/travisjeffery/radar"
)

// inlineComment is one review comment for the workflow to post on a line of
// the pull request, in the shape GitHub's create-review API takes.
type inlineComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	// StartSide is required by GitHub whenever StartLine is set.
	StartSide string `json:"start_side,omitempty"`
	Side      string `json:"side"`
	Body      string `json:"body"`
	// FindingID lets the poster skip a finding it already commented on.
	FindingID string `json:"finding_id"`
}

// inlineFindingMarker tags an inline comment with its finding's id.
func inlineFindingMarker(id string) string { return "<!-- radar-finding " + id + " -->" }

// inlineComments returns a comment for each blocking P0 and P1 finding whose
// lines GitHub can comment on: lines on the new side of the diff, added or
// context. A finding elsewhere stays in the summary comment only.
func inlineComments(decision radar.PullRequestReview, files []radar.PullRequestFile) []inlineComment {
	commentable := map[string]map[int]bool{}
	for _, f := range files {
		commentable[f.Path] = rightSideLines(f.Patch)
	}
	clarified := map[string]radar.AppliedClarification{}
	for _, c := range decision.Clarifications {
		clarified[radar.ClarificationKey(c.FindingID, c.File, c.Line)] = c
	}
	// A finding a later round holds back does not block, and an inline
	// thread would: an unresolved thread stops the verdict.
	held := map[string]bool{}
	if c := decision.Convergence; c != nil {
		for _, h := range c.Held {
			held[radar.HeldKey(h.FindingID, h.File, h.Line)] = true
		}
	}
	var out []inlineComment
	for _, f := range mergedFindings(decision) {
		if c, ok := clarified[radar.ClarificationKey(f.ID, f.File, f.Line)]; ok && (c.Outcome == radar.OutcomeWithdrawn || c.Outcome == radar.OutcomeDowngraded) {
			continue
		}
		if held[radar.HeldKey(f.ID, f.File, f.Line)] {
			continue
		}
		if f.Severity != "P0" && f.Severity != "P1" {
			continue
		}
		lines := commentable[f.File]
		if f.Line <= 0 || !lines[f.Line] {
			continue
		}
		c := inlineComment{Path: f.File, Line: f.Line, Side: "RIGHT", FindingID: f.ID}
		// A range must lie wholly in one hunk; otherwise comment on its first line.
		if end := f.EndLine; end > f.Line && contiguous(lines, f.Line, end) {
			c.StartLine, c.StartSide, c.Line = f.Line, "RIGHT", end
		}
		var b strings.Builder
		fmt.Fprintf(&b, "**%s: %s** (Radar, found by %s)\n\n", f.Severity, oneLine(f.Title), joinReviewers(f.Reviewers))
		b.WriteString("**Impact:** " + sanitize(f.Summary) + "\n\n")
		if f.Recommendation != "" {
			b.WriteString("**Recommendation:** " + sanitize(f.Recommendation) + "\n\n")
		}
		b.WriteString("_React 👍 if this is a real problem, 👎 if not; it measures Radar's precision. If Radar missed something, reply starting with `/radar` and say what; it re-checks the finding in the code._\n\n")
		b.WriteString(inlineFindingMarker(f.ID))
		c.Body = b.String()
		out = append(out, c)
	}
	return out
}

func contiguous(lines map[int]bool, from, to int) bool {
	for l := from; l <= to; l++ {
		if !lines[l] {
			return false
		}
	}
	return true
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// rightSideLines returns the new-file line numbers a patch shows: added and
// context lines, which GitHub accepts review comments on.
func rightSideLines(patch string) map[int]bool {
	lines := map[int]bool{}
	next := 0
	for _, l := range strings.Split(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			next, _ = strconv.Atoi(m[1])
			continue
		}
		if next == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, " "):
			lines[next] = true
			next++
		case strings.HasPrefix(l, "-"), strings.HasPrefix(l, `\`):
		default:
			// A blank line inside a hunk is a context line whose leading
			// space was trimmed.
			if l == "" {
				lines[next] = true
				next++
			}
		}
	}
	return lines
}
