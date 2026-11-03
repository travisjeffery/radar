package radar

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PriorReview is the last head Radar reviewed on a pull request, what it
// reported there, and what changed since, so that a later round blocks only
// on what the new push changed. Adapters read it from Radar's own
// comment and the provider's comparison of the two heads.
type PriorReview struct {
	Head string `json:"head"`
	// Round is that review's round, 1 for the pull request's first, so
	// this review is Round+1.
	Round int `json:"round"`
	// Findings are the merged findings Radar reported at Head, without those
	// it set apart there as raised late. One that Head's verdict held back is
	// listed at P2: it did not block there, so it is no baseline to block on.
	Findings []ReviewFinding `json:"findings,omitempty"`
	// Changes are the files that differ between Head and this head, each
	// with its patch against Head. A file without a patch counts as wholly
	// changed.
	Changes []PullRequestFile `json:"changes,omitempty"`
	// ChangesComplete is false when the changes or Findings could not all
	// be read, as after a force-push the comparison cannot follow; every
	// blocking finding then counts, as in a first review.
	ChangesComplete bool `json:"changes_complete"`
}

// ResolvedFinding is a Radar finding's inline thread that a person resolved
// and whose lines have not changed since (GitHub marks a thread outdated
// when they do). Re-raising it does not block.
type ResolvedFinding struct {
	FindingID string `json:"finding_id"`
	Title     string `json:"title,omitempty"`
	File      string `json:"file"`
	// Line and EndLine are the thread's lines at this head; EndLine is
	// set for a thread on a range.
	Line       int    `json:"line"`
	EndLine    int    `json:"end_line,omitempty"`
	ResolvedBy string `json:"resolved_by"`
	// ByPRAuthor is set when the pull request's author resolved it: like a
	// clarification resolved by assertion, a person other than the author
	// must then accept that.
	ByPRAuthor bool `json:"by_pr_author"`
}

// Why a blocking finding is held back from the verdict.
const (
	// HeldUnchangedLines: first raised in a later round, on lines the push
	// since the last review did not change.
	HeldUnchangedLines = "unchanged-lines"
	// HeldResolvedThread: a person resolved its thread and its lines have
	// not changed since.
	HeldResolvedThread = "resolved-thread"
)

// ConvergenceRecord says which round a review is and which blocking
// findings the verdict held back as already decided or raised late.
type ConvergenceRecord struct {
	Round     int    `json:"round"`
	PriorHead string `json:"prior_head,omitempty"`
	// Incremental is set when later-round blocking was limited to the lines
	// changed since PriorHead; Reason says why it was not, otherwise.
	Incremental bool          `json:"incremental"`
	Reason      string        `json:"reason"`
	Held        []HeldFinding `json:"held,omitempty"`
	// Late are the findings below P0 first raised this round on lines
	// unchanged since PriorHead. Each round re-reviews the whole pull request
	// and finds a few more such issues in code earlier rounds already read,
	// so adapters show them apart from the findings the push answers for,
	// and keep them apart in later rounds until their lines change.
	Late []HeldFinding `json:"late,omitempty"`
}

// HeldFinding is a blocking finding the verdict did not count. The findings
// keep it as reported.
type HeldFinding struct {
	FindingID  string `json:"finding_id"`
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	RuleID     string `json:"rule_id,omitempty"`
	Severity   string `json:"severity"`
	Reason     string `json:"reason"`
	ResolvedBy string `json:"resolved_by,omitempty"`
	ByPRAuthor bool   `json:"by_pr_author,omitempty"`
}

// HeldKey matches a held finding to a merged finding by id and location.
func HeldKey(id, file string, line int) string { return ClarificationKey(id, file, line) }

// convergence decides which blocking findings a later round counts.
type convergence struct {
	record   ConvergenceRecord
	changes  map[string]fileDelta
	prior    []ReviewFinding
	resolved []ResolvedFinding
}

// newConvergence returns nil when in carries no history, so a first review
// with no resolved threads is evaluated exactly as before.
func newConvergence(in PullRequestInput) *convergence {
	if in.Prior == nil && len(in.ResolvedFindings) == 0 {
		return nil
	}
	c := &convergence{record: ConvergenceRecord{Round: 1, Reason: "first review: every blocking finding counts"}, resolved: in.ResolvedFindings}
	p := in.Prior
	if p == nil {
		return c
	}
	c.record.Round, c.record.PriorHead = max(p.Round, 1)+1, p.Head
	switch {
	case p.Head == "" || p.Head == in.HeadSHA:
		c.record.Round, c.record.PriorHead = max(p.Round, 1), ""
		c.record.Reason = "no earlier head to compare with: every blocking finding counts"
	case !p.ChangesComplete:
		c.record.Reason = "the changes or findings since " + shortHead(p.Head) + " could not all be read (a force-push, a large comparison, or findings cut to fit the comment): every blocking finding counts"
	default:
		c.record.Incremental = true
		c.record.Reason = "later round: a blocking finding counts when it is on lines changed since " + shortHead(p.Head) + " or was already blocking there"
		c.prior = p.Findings
		c.changes = map[string]fileDelta{}
		for _, f := range p.Changes {
			c.changes[f.Path] = newFileDelta(f)
		}
	}
	return c
}

func shortHead(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// hold returns the reason a blocking finding does not count, or "".
func (c *convergence) hold(f ReviewFinding) (string, *ResolvedFinding) {
	if c.onChangedLines(f) {
		return "", nil
	}
	for i, r := range c.resolved {
		// By place, not id: the id hashes file, rule and title but not the
		// line, so two distant issues may share one, and a re-worded
		// finding gets another.
		if r.File != "" && r.File == f.File && f.Line > 0 && r.Line > 0 && rangesNear(r.Line, max(r.EndLine, r.Line), f.Line, max(f.EndLine, f.Line)) {
			return HeldResolvedThread, &c.resolved[i]
		}
	}
	// A P0 raised late still blocks: a security hole, data loss or crash
	// found on the third look is no less one, and the review has no
	// structured way to tell those P0s from the rest.
	if !c.record.Incremental || f.Severity == "P0" || c.wasBlocking(f) {
		return "", nil
	}
	return HeldUnchangedLines, nil
}

// rangesNear reports whether two line ranges overlap or lie within
// mergeLineWindow of each other.
func rangesNear(aStart, aEnd, bStart, bEnd int) bool {
	return aStart <= bEnd+mergeLineWindow && bStart <= aEnd+mergeLineWindow
}

// onChangedLines reports whether the push since the prior head changed the
// finding's lines, or lines within mergeLineWindow of them. Without an
// incremental comparison nothing is known to be unchanged.
func (c *convergence) onChangedLines(f ReviewFinding) bool {
	if !c.record.Incremental {
		// Resolved threads still hold: GitHub reports their lines unchanged.
		return false
	}
	d, ok := c.changes[f.File]
	if !ok {
		return f.File == ""
	}
	if d.whole || f.Line <= 0 {
		return true
	}
	end := max(f.EndLine, f.Line)
	for l := f.Line - mergeLineWindow; l <= end+mergeLineWindow; l++ {
		if d.changed[l] {
			return true
		}
	}
	return false
}

// carryOverWindow is how far apart a finding and the previous round's may
// be and still be one issue. Wider than mergeLineWindow: across rounds the
// reviewers re-word a finding and cite a nearby line (a real review, round 4
// cited 540 for round 3's 544), and a wider window only counts more.
const carryOverWindow = 10

// wasBlocking reports whether the prior review raised f at P0 or P1: the
// same id, or the same file and rule near its line before the push. Only
// the previous round is compared; a finding dropped and raised again later
// is new.
func (c *convergence) wasBlocking(f ReviewFinding) bool { return c.raisedBefore(f, true) }

// raisedBefore reports whether the prior review raised f, at P0 or P1 when
// blocking is set: the same id, or the same file and rule near its line
// before the push.
func (c *convergence) raisedBefore(f ReviewFinding, blocking bool) bool {
	file, line := f.File, f.Line
	if d, ok := c.changes[f.File]; ok {
		file, line = d.previousPath, d.toPrevious(f.Line)
	}
	for _, p := range c.prior {
		if blocking && p.Severity != "P0" && p.Severity != "P1" {
			continue
		}
		if p.ID != "" && p.ID == f.ID || p.File != "" && p.File == file && p.RuleID == f.RuleID && abs(p.Line-line) <= carryOverWindow {
			return true
		}
	}
	return false
}

// late reports whether f is first raised in this later round on lines the
// push since the prior head left unchanged. A P0 never is: it always shows
// and blocks.
func (c *convergence) late(f ReviewFinding) bool {
	return c.record.Incremental && f.Severity != "P0" && !c.onChangedLines(f) && !c.raisedBefore(f, false)
}

// apply drops the findings the round holds back from what the verdict
// counts, and clears the bug signal when no blocking finding is left, as a
// clarification does. It returns what it held.
func (c *convergence) apply(res ACRResult, blocking []string) (ACRResult, []HeldFinding) {
	if c == nil {
		return res, nil
	}
	var kept []ReviewFinding
	var held []HeldFinding
	for _, f := range res.Findings {
		// Only P0 and P1: a policy that also blocks on P2 keeps it blocking.
		if f.Severity != "P0" && f.Severity != "P1" || !containsFold(blocking, f.Severity) {
			kept = append(kept, f)
			continue
		}
		reason, r := c.hold(f)
		if reason == "" {
			kept = append(kept, f)
			continue
		}
		h := HeldFinding{FindingID: f.ID, File: f.File, Line: f.Line, RuleID: f.RuleID, Severity: f.Severity, Reason: reason}
		if r != nil {
			h.ResolvedBy, h.ByPRAuthor = r.ResolvedBy, r.ByPRAuthor
		}
		held = append(held, h)
	}
	if len(held) == 0 {
		return res, nil
	}
	had := hasBlockingFinding(res.Findings, blocking)
	res.Findings = kept
	if had && !hasBlockingFinding(kept, blocking) {
		res.RiskSignals = withoutSignal(res.RiskSignals, SignalBugOrLogicError)
	}
	return res, held
}

func withoutSignal(signals []ChangeSignal, drop ChangeSignal) []ChangeSignal {
	var out []ChangeSignal
	for _, s := range signals {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// recordHeld lists each merged finding the verdict held back, once, under
// the merged finding's id and place, and each one raised late. Reviewers'
// findings are matched to the merged ones as merging matched them; a merged
// finding is held, or late, only when no reviewer's version of it still
// counts as blocking, since otherwise the comment would show as not blocking
// what blocks.
func (c *convergence) recordHeld(findings []MergedFinding, held []HeldFinding, counted []ReviewFinding) {
	isHeld := make([]*HeldFinding, len(findings))
	blocks := make([]bool, len(findings))
	for i := range held {
		h := held[i]
		if j := matchFinding(findings, ReviewFinding{ID: h.FindingID, File: h.File, Line: h.Line, RuleID: h.RuleID}); j >= 0 && isHeld[j] == nil {
			isHeld[j] = &h
		}
	}
	for _, f := range counted {
		if j := matchFinding(findings, f); j >= 0 {
			blocks[j] = true
		}
	}
	for j, f := range findings {
		if h := isHeld[j]; h != nil && !blocks[j] {
			r := *h
			r.FindingID, r.File, r.Line, r.RuleID, r.Severity = f.ID, f.File, f.Line, f.RuleID, f.Severity
			c.record.Held = append(c.record.Held, r)
		}
		if !blocks[j] && c.late(f.ReviewFinding) {
			c.record.Late = append(c.record.Late, HeldFinding{FindingID: f.ID, File: f.File, Line: f.Line, RuleID: f.RuleID, Severity: f.Severity, Reason: HeldUnchangedLines})
		}
	}
}

// fileDelta is one file's change between two heads, by line of the newer.
type fileDelta struct {
	previousPath string
	whole        bool
	changed      map[int]bool
	// context maps each context line inside a hunk to the older head's
	// numbering; hunks shift the lines after them.
	context map[int]int
	hunks   []hunkSpan
}

type hunkSpan struct{ oldStart, oldLen, newStart, newLen int }

var hunkRange = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// newFileDelta parses f's patch. An added file, or one without a patch
// (binary, or too large for the provider to show) other than a rename
// that changes no line, is wholly changed. Adapters clear a patch the
// provider truncated.
func newFileDelta(f PullRequestFile) fileDelta {
	d := fileDelta{previousPath: f.Path, changed: map[int]bool{}, context: map[int]int{}}
	if f.PreviousPath != "" {
		d.previousPath = f.PreviousPath
	}
	pureRename := f.Status == "renamed" && f.Additions == 0 && f.Deletions == 0
	if f.Status == "added" || strings.TrimSpace(f.Patch) == "" && !pureRename {
		d.whole = true
		return d
	}
	oldLine, newLine := 0, 0
	for _, text := range strings.Split(f.Patch, "\n") {
		if m := hunkRange.FindStringSubmatch(text); m != nil {
			h := hunkSpan{oldStart: atoi(m[1]), oldLen: atoiOr(m[2], 1), newStart: atoi(m[3]), newLen: atoiOr(m[4], 1)}
			d.hunks = append(d.hunks, h)
			oldLine, newLine = h.oldStart, h.newStart
			continue
		}
		switch {
		case strings.HasPrefix(text, "+"):
			d.changed[newLine] = true
			newLine++
		case strings.HasPrefix(text, "-"):
			// A deletion changes the lines on either side of it.
			d.changed[newLine-1], d.changed[newLine] = true, true
			oldLine++
		case strings.HasPrefix(text, `\`):
		default:
			d.context[newLine] = oldLine
			oldLine++
			newLine++
		}
	}
	return d
}

// describe lists the changed lines as ranges, "whole file" when it is.
func (d fileDelta) describe() string {
	if d.whole {
		return "whole file"
	}
	lines := make([]int, 0, len(d.changed))
	for l := range d.changed {
		if l > 0 {
			lines = append(lines, l)
		}
	}
	sort.Ints(lines)
	var parts []string
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(lines[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", lines[i], lines[j]))
		}
		i = j + 1
	}
	if len(parts) == 0 {
		return "renamed, content unchanged"
	}
	if len(parts) > 20 {
		return "lines " + strings.Join(parts[:20], ", ") + fmt.Sprintf(" and %d more ranges", len(parts)-20)
	}
	return "lines " + strings.Join(parts, ", ")
}

// toPrevious maps an unchanged line to its number at the older head.
func (d fileDelta) toPrevious(line int) int {
	if old, ok := d.context[line]; ok {
		return old
	}
	shift := 0
	for _, h := range d.hunks {
		if line < h.newStart+h.newLen {
			break
		}
		shift = (h.oldStart + h.oldLen) - (h.newStart + h.newLen)
	}
	return line + shift
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	return atoi(s)
}

// convergenceGate routes to a human when a held finding rests only on the
// author resolving its thread: an author cannot resolve Radar out of a
// blocking finding on their own word, as with clarifications.
func convergenceGate(c *convergence, add func(string, bool, string) bool) bool {
	byAuthor := 0
	late := 0
	for _, h := range c.record.Held {
		switch {
		case h.Reason == HeldResolvedThread && h.ByPRAuthor:
			byAuthor++
		case h.Reason == HeldUnchangedLines:
			late++
		}
	}
	if byAuthor > 0 {
		return add("review.prior-decisions", false, fmt.Sprintf("round %d: %d blocking finding(s) are held only because the pull request's author resolved their threads; a person must accept that", c.record.Round, byAuthor))
	}
	return add("review.prior-decisions", true, fmt.Sprintf("round %d: %d blocking finding(s) held back (%d first raised on lines unchanged since the last review); %s", c.record.Round, len(c.record.Held), late, c.record.Reason))
}
