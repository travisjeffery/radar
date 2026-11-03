package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/travisjeffery/radar"
)

// reviewCommentMarker identifies Radar's findings comment, which each run
// updates in place.
const reviewCommentMarker = "<!-- radar-pr-review -->"

// maxCommentChars stays under GitHub's 65536-character comment limit.
const maxCommentChars = 60000

// maxWhereToLook bounds the reading list a human gets.
const maxWhereToLook = 5

// maxShownAssumptions caps the Assumptions section; the decision trace keeps
// the rest.
const maxShownAssumptions = 8

var severityHeadings = []struct{ severity, heading string }{
	{"P0", "P0 - Blocker"},
	{"P1", "P1 - High"},
	{"P2", "P2 - Medium"},
	{"P3", "P3 - Nit"},
}

// commentState is what the comment records, hidden, about the findings it
// showed, so the next push can say which are new, still open or resolved.
type commentState struct {
	Head string `json:"head"`
	// Round is the head's review round, 1 for the first.
	Round    int            `json:"round,omitempty"`
	Findings []stateFinding `json:"findings"`
	// Prior is the round before, which every stage at this head compares with.
	Prior *priorState `json:"prior,omitempty"`
	// Truncated is set when blocking findings were cut to fit: the next
	// round then has no complete baseline and every blocker counts.
	Truncated bool `json:"truncated,omitempty"`
	// CostUSD is the estimated model cost of every review of the pull
	// request so far; Partial is set when it leaves spend out.
	CostUSD float64 `json:"cost_usd,omitempty"`
	Partial bool    `json:"partial,omitempty"`
}

type stateFinding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	RuleID   string `json:"rule_id,omitempty"`
	// Held is set when the verdict did not count it as blocking, and Late
	// when it was raised late on unchanged lines and shown apart.
	Held bool `json:"held,omitempty"`
	Late bool `json:"late,omitempty"`
}

const stateOpen, stateClose = "<!-- radar-state ", " -->"

var stateRE = regexp.MustCompile(`<!-- radar-state (\{.*?\}) -->`)

// parseCommentState reads the state a previous comment recorded; a comment
// without one (or none at all) has no history.
func parseCommentState(body string) (commentState, bool) {
	m := stateRE.FindStringSubmatch(body)
	if m == nil {
		return commentState{}, false
	}
	var s commentState
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
		return commentState{}, false
	}
	return s, true
}

// pullRequestTotals adds this review's estimated cost to what the previous
// comment recorded. A re-render at the same head adds only what this run
// spent, which is nothing when it reused the reviews. previousUnread is set
// when the previous comment could not be fetched: the earlier reviews'
// cost is then unknown, so the total is marked partial, and stays so.
func pullRequestTotals(decision radar.PullRequestReview, previous string, previousUnread bool) *radar.PullRequestTotals {
	prev, ok := parseCommentState(previous)
	round, _ := roundBaseline(prev, ok, decision.HeadSHA)
	t := &radar.PullRequestTotals{Reviews: round, Partial: previousUnread}
	if ok {
		t.CostUSD, t.Partial = prev.CostUSD, t.Partial || prev.Partial
	}
	if u := decision.Agent.Usage; u != nil {
		t.CostUSD += u.CostUSD
		t.Partial = t.Partial || u.Unpriced
	}
	return t
}

// renderReviewComment renders the human review from the same structured
// reviews the verdict rests on: summary and risk, where to look,
// the merged findings with their status since the last push, questions,
// assumptions (load-bearing first), suggested tests, the verdict and its reason, and each reviewer's coverage.
// previous is the comment it replaces, or "".
func renderReviewComment(repo string, decision radar.PullRequestReview, previous string) string {
	var b strings.Builder
	b.WriteString(reviewCommentMarker + "\n# Radar review\n\n")
	if decision.Stage == radar.StageCommit {
		fmt.Fprintf(&b, "Reviewed commit `%s` as a pull request into its base branch would be. Report-only: nothing was posted or approved.\n\n", shortSHA(decision.HeadSHA))
	} else {
		fmt.Fprintf(&b, "Reviewed `%s`. This comment is updated on each push.\n\n", shortSHA(decision.HeadSHA))
	}

	verdicts := reviewerVerdicts(decision)
	if len(verdicts) == 0 {
		b.WriteString("The reviewers did not run: ")
		b.WriteString(firstFailedStage(decision))
		b.WriteString("\n")
		return b.String()
	}

	var completed, failed []radar.PullRequestReviewerVerdict
	for _, v := range verdicts {
		if reviewIncomplete(v.Agent) {
			failed = append(failed, v)
		} else {
			completed = append(completed, v)
		}
	}
	if len(failed) > 0 {
		// A reviewer that errored, timed out or was cut off reported
		// nothing, which must not read as a clean review.
		fmt.Fprintf(&b, "> [!WARNING]\n> %s did not complete its review; see Coverage. Any findings attributed to it cover only the part it finished.\n\n", joinLabels(failed))
	}

	if lead := leadReview(completed); lead != nil {
		b.WriteString("## Summary\n\n" + sanitize(lead.Agent.Summary) + "\n\n")
		if risk := highestRisk(completed); risk != nil {
			fmt.Fprintf(&b, "**Risk: %s** - %s\n\n", strings.ToUpper(risk.Level[:1])+risk.Level[1:], oneLine(risk.Reason))
		}
	}

	if pointers := whereToLook(completed); len(pointers) > 0 {
		b.WriteString("## Where to look\n\n")
		for _, p := range pointers {
			loc := locationLink(repo, decision.HeadSHA, p.File, p.Line, p.Line)
			if loc == "" {
				loc = "`" + oneLine(p.File) + "`"
			}
			fmt.Fprintf(&b, "- %s - %s\n", loc, oneLine(p.Why))
		}
		b.WriteString("\n")
	}

	findings := mergedFindings(decision)
	prev, hasPrev := parseCommentState(previous)
	// Status is against the previous round, which a comment for this head
	// keeps, so a re-render at the same head labels findings the same way.
	round, base := roundBaseline(prev, hasPrev, decision.HeadSHA)
	var baseState commentState
	if base != nil {
		baseState = commentState{Head: base.Head, Findings: base.Findings}
	}
	// A finding counts as resolved only when every reviewer rechecked it.
	status, resolved := findingStatus(findings, baseState, base != nil, len(failed) == 0)
	held, late := convergenceKeys(decision.Convergence)
	b.WriteString("## Findings\n\n")
	switch {
	case len(findings) == 0 && len(completed) == 0:
		b.WriteString("No reviewer completed its review.\n\n")
	case len(findings) == 0 && len(failed) > 0:
		fmt.Fprintf(&b, "No findings from %s.\n\n", joinLabels(completed))
	case len(findings) == 0:
		b.WriteString("No findings.\n\n")
	}
	clarified := map[string]radar.AppliedClarification{}
	for _, c := range decision.Clarifications {
		clarified[radar.ClarificationKey(c.FindingID, c.File, c.Line)] = c
	}
	var withdrawn, raisedLate []radar.MergedFinding
	if len(findings) > 0 {
		b.WriteString("_Radar missed something? Reply to a finding's inline comment, or comment on the pull request, starting a line with `/radar` and saying what it missed; name findings by their ID. Radar re-checks them in the code and replies._\n\n")
		if c := decision.Convergence; c != nil && c.Round > 1 {
			if c.Incremental {
				fmt.Fprintf(&b, "_Review round %d. A P0 or P1 blocks when it is on lines changed since `%s` or was already blocking there. Other findings first raised on lines unchanged since then do not block and are listed apart, under \"Raised late on unchanged lines\"._\n\n", c.Round, shortSHA(c.PriorHead))
			} else {
				fmt.Fprintf(&b, "_Review round %d. Every P0 and P1 blocks: %s._\n\n", c.Round, oneLine(c.Reason))
			}
		}
	}
	for _, group := range severityHeadings {
		var in []radar.MergedFinding
		for _, f := range findings {
			if _, ok := late[radar.HeldKey(f.ID, f.File, f.Line)]; ok {
				if group.severity == "P0" {
					raisedLate = append(raisedLate, f)
				}
				continue
			}
			severity := f.Severity
			if c, ok := clarified[radar.ClarificationKey(f.ID, f.File, f.Line)]; ok {
				if c.Outcome == radar.OutcomeWithdrawn {
					if group.severity == "P0" {
						withdrawn = append(withdrawn, f)
					}
					continue
				}
				severity = c.Severity
			}
			if _, ok := held[radar.HeldKey(f.ID, f.File, f.Line)]; ok {
				severity = "P2"
			}
			if severity == group.severity {
				in = append(in, f)
			}
		}
		if len(in) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", group.heading)
		for _, f := range in {
			title := oneLine(f.Title)
			if s := status[f.ID]; s != "" {
				title += " `" + s + "`"
			}
			if c, ok := clarified[radar.ClarificationKey(f.ID, f.File, f.Line)]; ok {
				switch c.Outcome {
				case radar.OutcomeDowngraded:
					title += " `downgraded from " + c.FromSeverity + " after clarification`"
				case radar.OutcomeResolvedByAssertion:
					title += " `resolved by assertion`"
				}
			}
			if h, ok := held[radar.HeldKey(f.ID, f.File, f.Line)]; ok {
				title += " `" + heldLabel(h, decision.Convergence) + "`"
			}
			fmt.Fprintf(&b, "#### %s\n\n", title)
			var meta []string
			if loc := locationLink(repo, decision.HeadSHA, f.File, f.Line, f.EndLine); loc != "" {
				meta = append(meta, "Location: "+loc)
			}
			found := "Found by " + joinReviewers(f.Reviewers)
			if f.Agreed() {
				found += " (agree)"
			}
			meta = append(meta, found)
			if f.RuleID != "" {
				meta = append(meta, "Rule: `"+f.RuleID+"`")
			}
			if f.Confidence > 0 {
				meta = append(meta, fmt.Sprintf("Confidence: %d/10", f.Confidence))
			}
			meta = append(meta, "ID `"+shortID(f.ID)+"`")
			b.WriteString(strings.Join(meta, " · ") + "\n\n")
			b.WriteString("**Impact:** " + sanitize(f.Summary) + "\n\n")
			if f.Recommendation != "" {
				b.WriteString("**Recommendation:** " + sanitize(f.Recommendation) + "\n\n")
			}
			if c, ok := clarified[radar.ClarificationKey(f.ID, f.File, f.Line)]; ok && c.Outcome == radar.OutcomeResolvedByAssertion {
				fmt.Fprintf(&b, "**Clarification:** %s said this is not a problem for a reason Radar cannot check in the code; it no longer counts as a blocker, but a person must accept it before Radar would approve.\n\n", oneLine(c.Author))
			}
		}
	}
	if len(raisedLate) > 0 {
		since := "the last review"
		if c := decision.Convergence; c != nil && c.PriorHead != "" {
			since = "`" + shortSHA(c.PriorHead) + "`"
		}
		fmt.Fprintf(&b, "<details><summary>Raised late on unchanged lines (%d)</summary>\n\n", len(raisedLate))
		fmt.Fprintf(&b, "_First raised this round on lines unchanged since %s, in code earlier rounds already reviewed. They do not block; each comes back to the findings above once a push changes its lines._\n\n", since)
		for _, f := range raisedLate {
			loc := locationLink(repo, decision.HeadSHA, f.File, f.Line, f.EndLine)
			if loc == "" && f.File != "" {
				loc = "`" + oneLine(f.File) + "`"
			}
			if loc != "" {
				loc = " - " + loc
			}
			fmt.Fprintf(&b, "- %s %s%s · ID `%s`: %s\n", f.Severity, oneLine(f.Title), loc, shortID(f.ID), oneLine(sanitize(f.Summary)))
		}
		b.WriteString("\n</details>\n\n")
	}
	if len(withdrawn) > 0 {
		fmt.Fprintf(&b, "<details><summary>Withdrawn after clarification (%d)</summary>\n\n", len(withdrawn))
		for _, f := range withdrawn {
			c := clarified[radar.ClarificationKey(f.ID, f.File, f.Line)]
			fmt.Fprintf(&b, "- %s %s (`%s`), after %s's clarification, verified in the code at `%s`\n", f.Severity, oneLine(f.Title), shortID(f.ID), oneLine(c.Author), shortSHA(c.Head))
		}
		b.WriteString("\n</details>\n\n")
	}
	if len(resolved) > 0 {
		fmt.Fprintf(&b, "<details><summary>Resolved since the last review (%d)</summary>\n\n", len(resolved))
		for _, f := range resolved {
			loc := ""
			if f.File != "" {
				loc = fmt.Sprintf(" (`%s`)", oneLine(f.File))
			}
			fmt.Fprintf(&b, "- %s %s%s\n", f.Severity, oneLine(f.Title), loc)
		}
		b.WriteString("\n</details>\n\n")
	}

	if qs := collect(completed, func(r radar.ACRResult) []string { return r.Questions }); len(qs) > 0 {
		b.WriteString("## Questions\n\n")
		for i, q := range qs {
			fmt.Fprintf(&b, "%d. %s\n", i+1, oneLine(q))
		}
		b.WriteString("\n")
	}
	if as := decision.Assumptions; len(as) > 0 {
		b.WriteString("## Assumptions\n\nWhat the reviewers took as true without verifying it. If one is wrong, the review may be too.\n\n")
		for i, a := range as {
			if i == maxShownAssumptions {
				fmt.Fprintf(&b, "\n_%d more in this run's decision trace._\n", len(as)-maxShownAssumptions)
				break
			}
			lead := ""
			if a.LoadBearing {
				lead = "**Load-bearing:** "
			}
			fmt.Fprintf(&b, "%d. %s%s\\\n", i+1, lead, oneLine(a.Assumption))
			var meta []string
			if a.Basis != "" {
				meta = append(meta, "Basis: "+oneLine(a.Basis))
			}
			if a.IfWrong != "" {
				meta = append(meta, "If wrong: "+oneLine(a.IfWrong))
			}
			meta = append(meta, "Assumed by "+joinReviewers(a.Reviewers))
			fmt.Fprintf(&b, "   %s\n", strings.Join(meta, " · "))
		}
		b.WriteString("\n")
	}
	if ts := collect(completed, func(r radar.ACRResult) []string { return r.TestsSuggested }); len(ts) > 0 {
		b.WriteString("## Suggested tests\n\n")
		for _, t := range ts {
			fmt.Fprintf(&b, "- %s\n", oneLine(t))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Verdict\n\n" + verdictLine(decision) + "\n\n")
	asked := false
	for _, v := range completed {
		if nh := v.Agent.NeedsHuman; nh != nil && nh.Required {
			fmt.Fprintf(&b, "- %s asks for a human reviewer: %s\n", reviewerLabel(v.Reviewer), oneLine(nh.Reason))
			asked = true
		}
	}
	if asked {
		b.WriteString("\n")
	}

	b.WriteString("## Coverage\n\n| Reviewer | Completed | Repo access | Files | Confidence | Notes |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, v := range verdicts {
		done, access, notes := "yes", "no", ""
		if reviewIncomplete(v.Agent) {
			done = "no"
			notes = v.Agent.Summary
		}
		if c := v.Agent.Coverage; c != nil {
			if c.RepoAccess {
				access = "yes"
				if c.ToolCalls > 0 {
					access = fmt.Sprintf("yes (%d tool calls)", c.ToolCalls)
				}
			}
			if notes == "" {
				notes = c.Limits
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d/10 | %s |\n", reviewerLabel(v.Reviewer), done, access,
			len(uniqueFiles(v.Agent.ReviewedFiles)), v.Agent.Confidence, tableCell(notes))
	}
	if len(decision.Withheld) > 0 {
		fmt.Fprintf(&b, "\n%d generated file(s) were withheld from the reviewers.\n", len(decision.Withheld))
	}
	if decision.Criteria != nil && len(decision.Criteria.Packs) > 0 {
		var packs []string
		for _, p := range decision.Criteria.Packs {
			packs = append(packs, fmt.Sprintf("`%s@v%d`", p.ID, p.Version))
		}
		fmt.Fprintf(&b, "\nReview packs: %s.\n", strings.Join(packs, ", "))
	}

	totals := decision.PullRequestTotals
	if totals == nil {
		totals = pullRequestTotals(decision, previous, false)
	}
	state := renderState(decision.HeadSHA, round, findings, base, decision.Convergence, totals)
	out := b.String()
	if budget := maxCommentChars - len(state) - 200; len(out) > budget {
		cut := strings.LastIndex(out[:budget], "\n#### ")
		if cut <= 0 {
			cut = budget
		}
		out = out[:cut] + "\n\n_The review is too long for one comment; the full findings are in this run's decision trace._\n"
	}
	return out + "\n" + state + "\n"
}

// maxStateChars bounds the hidden state by size; findings arrive most severe
// first, so a cut drops only the least severe.
const maxStateChars = 12000

func renderState(head string, round int, findings []radar.MergedFinding, prior *priorState, conv *radar.ConvergenceRecord, totals *radar.PullRequestTotals) string {
	held, late := convergenceKeys(conv)
	clip := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	s := commentState{Head: head, Round: round}
	if totals != nil {
		s.CostUSD, s.Partial = totals.CostUSD, totals.Partial
	}
	for _, f := range findings {
		s.Findings = append(s.Findings, stateFinding{ID: clip(f.ID, 64), Severity: clip(f.Severity, 4), Title: clip(f.Title, 200),
			File: clip(f.File, 300), Line: f.Line, RuleID: clip(f.RuleID, 64)})
		key := radar.HeldKey(f.ID, f.File, f.Line)
		_, s.Findings[len(s.Findings)-1].Held = held[key]
		_, s.Findings[len(s.Findings)-1].Late = late[key]
	}
	if prior != nil {
		p := *prior
		p.Findings = append([]stateFinding(nil), prior.Findings...)
		s.Prior = &p
	}
	data, _ := json.Marshal(s)
	// The P0s and P1s are the baseline a later round blocks against, so P2s
	// and P3s go first, the prior round's before this one's. Findings
	// arrive most severe first, so a cut keeps the blockers. If blockers
	// must go too, the state says so and the next round fails closed.
	for _, list := range []*[]stateFinding{priorFindings(&s), &s.Findings} {
		for len(data) > maxStateChars && len(*list) > 0 && !blockingState((*list)[len(*list)-1]) {
			*list = (*list)[:len(*list)-1]
			data, _ = json.Marshal(s)
		}
	}
	for len(data) > maxStateChars && (len(*priorFindings(&s)) > 0 || len(s.Findings) > 0) {
		if p := priorFindings(&s); len(*p) > 0 {
			*p = (*p)[:len(*p)*3/4]
			s.Prior.Truncated = true
		} else {
			s.Findings = s.Findings[:len(s.Findings)*3/4]
			s.Truncated = true
		}
		data, _ = json.Marshal(s)
	}
	// json.Marshal escapes <, > and & as < and so on, so a title
	// cannot close the HTML comment early.
	return stateOpen + string(data) + stateClose
}

// priorFindings returns the prior round's findings, or an empty list to
// trim when there is no prior round.
func priorFindings(s *commentState) *[]stateFinding {
	if s.Prior == nil {
		return &[]stateFinding{}
	}
	return &s.Prior.Findings
}

func blockingState(f stateFinding) bool { return f.Severity == "P0" || f.Severity == "P1" }

// convergenceKeys indexes the findings a later round held back from the
// verdict and those it raised late, by id and place.
func convergenceKeys(c *radar.ConvergenceRecord) (held, late map[string]radar.HeldFinding) {
	held, late = map[string]radar.HeldFinding{}, map[string]radar.HeldFinding{}
	if c == nil {
		return held, late
	}
	for _, h := range c.Held {
		held[radar.HeldKey(h.FindingID, h.File, h.Line)] = h
	}
	for _, h := range c.Late {
		late[radar.HeldKey(h.FindingID, h.File, h.Line)] = h
	}
	return held, late
}

// heldLabel says why a blocking finding shows as P2 and does not block.
func heldLabel(h radar.HeldFinding, c *radar.ConvergenceRecord) string {
	if h.Reason == radar.HeldResolvedThread {
		label := h.Severity + ", not blocking: its thread was resolved"
		if h.ResolvedBy != "" {
			label += " by " + oneLine(h.ResolvedBy)
		}
		if h.ByPRAuthor {
			label += "; a person other than the author must accept that"
		}
		return label
	}
	since := "the last review"
	if c != nil && c.PriorHead != "" {
		since = shortSHA(c.PriorHead)
	}
	return h.Severity + ", not blocking: first raised on lines unchanged since " + since
}

// findingStatus labels each current finding "new" or "still open" against
// the previous comment's findings, and lists those no longer reported. With
// no history, or the same head re-reviewed, there are no labels. When a
// reviewer did not complete, a missing finding may only be unchecked, so
// nothing is listed as resolved.
func findingStatus(current []radar.MergedFinding, prev commentState, compare, allCompleted bool) (map[string]string, []stateFinding) {
	status := map[string]string{}
	if !compare {
		return status, nil
	}
	matched := make([]bool, len(prev.Findings))
	for _, f := range current {
		status[f.ID] = "new"
		for i, p := range prev.Findings {
			if matched[i] {
				continue
			}
			if p.ID == f.ID || (p.File != "" && p.File == f.File && p.RuleID == f.RuleID && abs(p.Line-f.Line) <= 3) {
				matched[i] = true
				status[f.ID] = "still open"
				break
			}
		}
	}
	if !allCompleted {
		return status, nil
	}
	var resolved []stateFinding
	for i, p := range prev.Findings {
		// One raised late was shown apart; it is no news that it is gone.
		if !matched[i] && !p.Late {
			resolved = append(resolved, p)
		}
	}
	return status, resolved
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// mergedFindings returns the decision's merged findings, or for a decision
// from an older Radar, each reviewer's findings unmerged.
func mergedFindings(d radar.PullRequestReview) []radar.MergedFinding {
	if len(d.Findings) > 0 {
		return d.Findings
	}
	var out []radar.MergedFinding
	for _, v := range reviewerVerdicts(d) {
		for _, f := range v.Agent.Findings {
			if f.ID == "" {
				f.ID = radar.FindingID(f)
			}
			out = append(out, radar.MergedFinding{ReviewFinding: f, Reviewers: []string{v.Reviewer}})
		}
	}
	return out
}

// leadReview picks the review whose summary heads the comment: one with
// repository access if any, as it saw the most context.
func leadReview(completed []radar.PullRequestReviewerVerdict) *radar.PullRequestReviewerVerdict {
	for i := range completed {
		if c := completed[i].Agent.Coverage; c != nil && c.RepoAccess {
			return &completed[i]
		}
	}
	if len(completed) > 0 {
		return &completed[0]
	}
	return nil
}

var riskRank = map[string]int{"low": 1, "medium": 2, "high": 3}

func highestRisk(completed []radar.PullRequestReviewerVerdict) *radar.ReviewRisk {
	var best *radar.ReviewRisk
	for _, v := range completed {
		if r := v.Agent.Risk; r != nil && riskRank[r.Level] > 0 && (best == nil || riskRank[r.Level] > riskRank[best.Level]) {
			best = r
		}
	}
	return best
}

// whereToLook interleaves the reviewers' reading lists, one entry per place.
func whereToLook(completed []radar.PullRequestReviewerVerdict) []radar.ReviewPointer {
	seen := map[string]bool{}
	var out []radar.ReviewPointer
	for round := 0; len(out) < maxWhereToLook; round++ {
		more := false
		for _, v := range completed {
			if round >= len(v.Agent.WhereToLook) {
				continue
			}
			more = true
			p := v.Agent.WhereToLook[round]
			key := fmt.Sprintf("%s:%d", p.File, p.Line)
			if p.File == "" || seen[key] || len(out) >= maxWhereToLook {
				continue
			}
			seen[key] = true
			out = append(out, p)
		}
		if !more {
			break
		}
	}
	return out
}

// collect gathers a list field across reviewers, dropping exact repeats.
func collect(completed []radar.PullRequestReviewerVerdict, field func(radar.ACRResult) []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range completed {
		for _, s := range field(v.Agent) {
			key := strings.ToLower(strings.TrimSpace(s))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, s)
		}
	}
	return out
}

// verdictLine states what Radar decided, or at the findings stage what it
// would decide on these findings, with the reason. Approvals stay in shadow.
func verdictLine(d radar.PullRequestReview) string {
	reason := oneLine(d.VerdictReason)
	if d.Stage == radar.StageFindings {
		for _, s := range d.Stages {
			if !s.Passed {
				return "On these findings Radar would **route this pull request to a human**: " + reason + ". The verdict is decided again once checks finish."
			}
		}
		return "On these findings nothing blocks; the verdict is decided once checks finish."
	}
	switch d.Action {
	case radar.PullRequestApprove:
		return "**Approved** by Radar: " + reason + "."
	case radar.PullRequestWouldApprove:
		if d.Stage == radar.StageCommit {
			return "**Would approve** as a pull request (a commit review never approves): " + reason + "."
		}
		return "**Would approve** (shadow mode; Radar does not approve here): " + reason + "."
	case radar.PullRequestPolicyCandidate:
		return "**Needs a human**: the reviewers found nothing blocking, but the change is outside the approval policy (" + reason + ")."
	}
	return "**Needs a human**: " + reason + "."
}

// reviewIncomplete reports a verdict that is not a finished review: the
// agent failed safe on all or part of the diff, or was cut off.
func reviewIncomplete(r radar.ACRResult) bool {
	return r.Failed || r.Truncated
}

// reviewerVerdicts returns each reviewer's verdict; a single reviewer's
// verdict is the decision's Agent.
func reviewerVerdicts(d radar.PullRequestReview) []radar.PullRequestReviewerVerdict {
	if len(d.Reviews) > 0 {
		return d.Reviews
	}
	if d.Agent.Summary == "" {
		return nil
	}
	return []radar.PullRequestReviewerVerdict{{Reviewer: d.Reviewer, Agent: d.Agent}}
}

func firstFailedStage(d radar.PullRequestReview) string {
	for _, s := range d.Stages {
		if !s.Passed {
			return sanitize(s.Reason)
		}
	}
	return "no reviewable changes."
}

func joinLabels(vs []radar.PullRequestReviewerVerdict) string {
	labels := make([]string, 0, len(vs))
	for _, v := range vs {
		labels = append(labels, reviewerLabel(v.Reviewer))
	}
	return strings.Join(labels, ", ")
}

func joinReviewers(rs []string) string {
	labels := make([]string, 0, len(rs))
	for _, r := range rs {
		labels = append(labels, reviewerLabel(r))
	}
	sort.Strings(labels)
	return strings.Join(labels, ", ")
}

// reviewerLabel drops the provider-specific model path to keep the comment
// readable: fireworks/accounts/fireworks/models/glm-5p3 becomes glm-5p3.
func reviewerLabel(r string) string {
	if r == "" {
		return "reviewer"
	}
	provider, model, ok := strings.Cut(r, "/")
	if !ok {
		return "`" + r + "`"
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	return "`" + provider + "/" + model + "`"
}

// locationLink links file and lines at head; "" when there is no file.
func locationLink(repo, head, file string, line, end int) string {
	file = strings.TrimSpace(file)
	if file == "" || strings.ContainsAny(file, "`\n") {
		return ""
	}
	text, anchor := file, ""
	switch {
	case line > 0 && end > line:
		text = fmt.Sprintf("%s:%d-%d", file, line, end)
		anchor = fmt.Sprintf("#L%d-L%d", line, end)
	case line > 0:
		text = fmt.Sprintf("%s:%d", file, line)
		anchor = fmt.Sprintf("#L%d", line)
	}
	if repo == "" || head == "" {
		return "`" + text + "`"
	}
	escaped := (&url.URL{Path: file}).EscapedPath()
	return fmt.Sprintf("[`%s`](https://github.com/%s/blob/%s/%s%s)", text, repo, head, strings.TrimPrefix(escaped, "/"), anchor)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func uniqueFiles(files []string) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		out[f] = true
	}
	return out
}

// sanitize keeps model text from closing or forging the comment marker or
// state, and from mentioning users or teams.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "<!--", "&lt;!--")
	s = strings.ReplaceAll(s, "-->", "--&gt;")
	return strings.ReplaceAll(s, "@", "@​")
}

func oneLine(s string) string {
	return sanitize(strings.Join(strings.Fields(s), " "))
}

func tableCell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", "\\|")
}
