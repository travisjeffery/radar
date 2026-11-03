package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/travisjeffery/radar"
)

// Clarifications: people with write access answer a Radar finding
// with /radar, in the finding's thread or on the pull request. The clarify
// stage finds every such comment Radar has not answered, re-checks the
// findings they are about with one reviewer that reads the repository, and
// writes the replies, the updated review comment and the pull request's
// clarification ledger. It never reviews anew and never approves.

const (
	// maxClarificationsPerFinding and maxClarificationsPerDay are the
	// limits: re-checks per finding, and per pull request per
	// UTC day.
	maxClarificationsPerFinding = 3
	maxClarificationsPerDay     = 10
)

// invocation is /radar at the start of a line, the documented form, or
// @radar anywhere. @radar is an unrelated GitHub account, so it is accepted
// but not documented.
var invocation = regexp.MustCompile(`(?im)(^\s*/radar\b|(^|[^\w@./-])@radar\b)`)

// replyMarker tags Radar's replies so a run knows what was answered.
const replyMarkerOpen, replyMarkerClose = "<!-- radar-reply ", " -->"

var replyMarkerRE = regexp.MustCompile(`<!-- radar-reply (\{.*?\}) -->`)

var findingMarkerRE = regexp.MustCompile(`<!-- radar-finding ([0-9a-f]+) -->`)

// findingRef is a finding's short id as a person types it.
var findingRef = regexp.MustCompile(`\b[0-9a-f]{6,12}\b`)

// replyRecord is what a reply's hidden marker says.
type replyRecord struct {
	// To is the comment answered.
	To int64 `json:"to"`
	// Results are each finding's outcome; "limit" and "notice" are replies
	// that re-checked nothing.
	Results []replyResult `json:"results,omitempty"`
	// Checked is set when the reply cost a re-check, which the daily
	// limit counts.
	Checked bool `json:"checked,omitempty"`
	// Limit is "finding" or "pull_request" on a limit notice.
	Limit string `json:"limit,omitempty"`
}

type replyResult struct {
	Finding string `json:"finding"`
	Outcome string `json:"outcome"`
}

// prComment is an issue comment or a review comment.
type prComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	HTMLURL   string `json:"html_url"`
	InReplyTo int64  `json:"in_reply_to_id"`
	// Path and Line place a review comment; Line is null once the line is
	// outdated, and OriginalLine is where it was posted.
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"original_line"`
	User         struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	// inline is set for a review comment.
	inline bool
}

func (c prComment) bot() bool { return strings.EqualFold(c.User.Type, "Bot") }

// radarLogin posts Radar's comments: the workflow's GITHUB_TOKEN.
const radarLogin = "github-actions[bot]"

// fromRadar reports a comment Radar posted; only its markers count, so
// another app cannot mark comments answered or spend the limits.
func (c prComment) fromRadar() bool { return c.bot() && c.User.Login == radarLogin }

// clarifyReply is one comment for the workflow to post: in a review thread
// (InReplyTo, the thread's first comment) or on the pull request.
type clarifyReply struct {
	Kind      string `json:"kind"`
	InReplyTo int64  `json:"in_reply_to,omitempty"`
	Body      string `json:"body"`
}

// pendingClarification is a comment to re-check findings for.
type pendingClarification struct {
	radar.Clarification
	root int64
	url  string
	// limited are findings the comment named that are past their limit.
	limited []string
}

// clarifyPlan is what a run does with the comments on the pull request.
type clarifyPlan struct {
	pending []pendingClarification
	notices []clarifyReply
	handled []int64
}

// planInput is everything planning reads, so it can be tested offline.
type planInput struct {
	issue, review []prComment
	// findings are the findings reported at this head; open are their ids
	// still blocking after earlier clarifications, which a comment naming
	// no finding is about.
	findings []radar.MergedFinding
	open     []string
	ledger   radar.ClarificationLedger
	prAuthor string
	head     string
	// commits are the pull request's commit SHAs and the heads Radar
	// reviewed: a token naming one is a commit, not a finding ID.
	commits []string
	// permitted reports whether a login may ask Radar (write access); an
	// error leaves the comment for the next run.
	permitted func(login string) (bool, error)
	now       time.Time
}

// planClarifications decides, for each comment that asks Radar and has no
// answer yet, whether to re-check findings, reply with a notice, or ignore
// it. Bots, edited comments and people without write access are ignored;
// limits are counted from Radar's own replies.
func planClarifications(in planInput) clarifyPlan {
	var plan clarifyPlan
	answered := map[int64]bool{}
	for _, id := range in.ledger.Handled {
		answered[id] = true
	}
	perFinding := map[string]int{}
	findingLimitNoticed := map[string]bool{}
	today := in.now.UTC().Format("2006-01-02")
	checkedToday, dayLimitNoticed := 0, false
	roots := map[int64]prComment{}
	var all []prComment
	all = append(all, in.issue...)
	all = append(all, in.review...)
	for _, c := range all {
		if !c.fromRadar() {
			continue
		}
		if m := findingMarkerRE.FindStringSubmatch(c.Body); m != nil && c.inline && c.InReplyTo == 0 {
			roots[c.ID] = c
		}
		m := replyMarkerRE.FindStringSubmatch(c.Body)
		if m == nil {
			continue
		}
		var r replyRecord
		if json.Unmarshal([]byte(m[1]), &r) != nil {
			continue
		}
		answered[r.To] = true
		for _, res := range r.Results {
			if r.Checked && res.Outcome != "limit" {
				perFinding[res.Finding]++
			}
			if r.Limit == "finding" {
				findingLimitNoticed[res.Finding] = true
			}
		}
		if strings.HasPrefix(c.CreatedAt, today) {
			if r.Checked {
				checkedToday++
			}
			dayLimitNoticed = dayLimitNoticed || r.Limit == "pull_request"
		}
	}

	byID := map[string]radar.MergedFinding{}
	for _, f := range in.findings {
		byID[f.ID] = f
	}
	type permission struct {
		ok  bool
		err error
	}
	permitted := map[string]permission{}
	allowed := func(login string) (bool, error) {
		p, seen := permitted[login]
		if !seen {
			p.ok, p.err = in.permitted(login)
			permitted[login] = p
		}
		return p.ok, p.err
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].CreatedAt < all[b].CreatedAt })
	for _, c := range all {
		if c.bot() || !strings.EqualFold(c.User.Type, "User") || answered[c.ID] || !invocation.MatchString(c.Body) {
			continue
		}
		// An edit after the fact cannot change what Radar was asked.
		if c.UpdatedAt != c.CreatedAt {
			plan.handled = append(plan.handled, c.ID)
			continue
		}
		if ok, err := allowed(c.User.Login); err != nil {
			continue
		} else if !ok {
			plan.handled = append(plan.handled, c.ID)
			continue
		}
		notice := func(text string, rec replyRecord) {
			rec.To = c.ID
			plan.notices = append(plan.notices, replyTo(c, text, rec))
		}
		var targets []string
		var thread []radar.ThreadComment
		root := int64(0)
		if c.inline {
			rootComment, ok := roots[c.InReplyTo]
			if !ok {
				notice("Radar re-checks its own findings: reply with `/radar` in a Radar finding's thread, or comment on the pull request with `/radar <finding id>` and what Radar missed.", replyRecord{})
				continue
			}
			root = c.InReplyTo
			id, current := currentFinding(rootComment, in.findings)
			if !current {
				notice(fmt.Sprintf("Radar no longer reports this finding at `%s`, so there is nothing to re-check.", shortSHA(in.head)), replyRecord{Results: []replyResult{{Finding: id, Outcome: "notice"}}})
				continue
			}
			targets = []string{id}
			for _, t := range in.review {
				if t.InReplyTo == root && t.ID != c.ID && t.CreatedAt < c.CreatedAt {
					author := t.User.Login
					if t.bot() {
						author = "Radar"
					}
					thread = append(thread, radar.ThreadComment{Author: author, Body: stripMarkers(t.Body)})
				}
			}
			if len(thread) > 4 {
				thread = thread[len(thread)-4:]
			}
		} else {
			targets = referencedFindings(c.Body, in.findings)
			if len(targets) == 0 {
				// A mistyped or stale ID must not widen the request to
				// every finding.
				if bad := unresolvedRefs(c.Body, in.findings, in.commits); len(bad) > 0 {
					notice(fmt.Sprintf("Radar reports no finding with the ID %s at `%s`. Radar reviews each push again, so a finding fixed since is no longer listed; the Radar review shows the IDs still open. Name one of those, or none to re-check every open P0 or P1.", "`"+strings.Join(bad, "`, `")+"`", shortSHA(in.head)), replyRecord{})
					continue
				}
				targets = append(targets, in.open...)
			}
			if len(targets) == 0 {
				notice(fmt.Sprintf("Radar has already reviewed `%s` (it reviews each push) and reports no open P0 or P1 findings to re-check. To re-check another finding, name it by the ID shown next to it in the Radar review.", shortSHA(in.head)), replyRecord{})
				continue
			}
		}
		var within, limited []string
		for _, id := range targets {
			if perFinding[id] < maxClarificationsPerFinding {
				within = append(within, id)
			} else {
				limited = append(limited, id)
			}
		}
		if len(within) == 0 {
			if !findingLimitNoticed[targets[0]] {
				findingLimitNoticed[targets[0]] = true
				var results []replyResult
				for _, id := range targets {
					results = append(results, replyResult{Finding: id, Outcome: "limit"})
				}
				notice(fmt.Sprintf("Radar has re-checked this finding %d times, its limit. A human reviewer can resolve the thread.", maxClarificationsPerFinding), replyRecord{Results: results, Limit: "finding"})
			} else {
				plan.handled = append(plan.handled, c.ID)
			}
			continue
		}
		if checkedToday >= maxClarificationsPerDay {
			if !dayLimitNoticed {
				dayLimitNoticed = true
				notice(fmt.Sprintf("Radar has answered %d clarifications on this pull request today (UTC), its daily limit. A human reviewer can resolve the thread.", maxClarificationsPerDay), replyRecord{Limit: "pull_request"})
			} else {
				plan.handled = append(plan.handled, c.ID)
			}
			continue
		}
		checkedToday++
		for _, id := range within {
			perFinding[id]++
		}
		kind := "issue"
		if c.inline {
			kind = "inline"
		}
		plan.pending = append(plan.pending, pendingClarification{
			Clarification: radar.Clarification{
				CommentID: c.ID, Kind: kind, Author: c.User.Login, ByPRAuthor: strings.EqualFold(c.User.Login, in.prAuthor),
				Body: stripMarkers(c.Body), FindingIDs: within, Thread: thread,
			},
			limited: limited,
			root:    root, url: c.HTMLURL,
		})
	}
	return plan
}

// currentFinding is the finding at this head a Radar finding comment is
// about: the one with its id, or after a push moved it, the one in the same
// file within three lines of where the comment was posted.
func currentFinding(root prComment, findings []radar.MergedFinding) (string, bool) {
	m := findingMarkerRE.FindStringSubmatch(root.Body)
	if m == nil {
		return "", false
	}
	for _, f := range findings {
		if f.ID == m[1] {
			return f.ID, true
		}
	}
	line := root.Line
	if line == 0 {
		line = root.OriginalLine
	}
	for _, f := range findings {
		if root.Path != "" && f.File == root.Path && line > 0 && abs(f.Line-line) <= 3 {
			return f.ID, true
		}
	}
	return m[1], false
}

// unresolvedRefs returns the tokens in a comment that look like finding IDs
// (six or twelve hex characters with a digit, as Radar shows them) but name
// no current finding, or more than one. Radar shows commits as twelve hex
// characters too, so a token naming one of commits is left out.
func unresolvedRefs(body string, findings []radar.MergedFinding, commits []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, ref := range findingRef.FindAllString(strings.ToLower(body), -1) {
		if (len(ref) != 6 && len(ref) != 12) || !strings.ContainsAny(ref, "0123456789") || seen[ref] {
			continue
		}
		seen[ref] = true
		if slices.ContainsFunc(commits, func(sha string) bool { return strings.HasPrefix(strings.ToLower(sha), ref) }) {
			continue
		}
		matches := map[string]bool{}
		for _, f := range findings {
			if strings.HasPrefix(f.ID, ref) {
				matches[f.ID] = true
			}
		}
		if len(matches) != 1 {
			out = append(out, ref)
		}
	}
	return out
}

// referencedFindings returns the findings a comment names by id or prefix.
func referencedFindings(body string, findings []radar.MergedFinding) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range findingRef.FindAllString(strings.ToLower(body), -1) {
		var match string
		for _, f := range findings {
			if strings.HasPrefix(f.ID, ref) {
				if match != "" && match != f.ID {
					match = ""
					break
				}
				match = f.ID
			}
		}
		if match != "" && !seen[match] {
			seen[match] = true
			out = append(out, match)
		}
	}
	return out
}

func stripMarkers(s string) string {
	s = replyMarkerRE.ReplaceAllString(s, "")
	return strings.TrimSpace(findingMarkerRE.ReplaceAllString(s, ""))
}

// replyTo addresses a reply to c: in its thread for a review comment, on
// the pull request otherwise.
func replyTo(c prComment, text string, rec replyRecord) clarifyReply {
	marker, _ := json.Marshal(rec)
	if c.inline {
		root := c.InReplyTo
		if root == 0 {
			root = c.ID
		}
		return clarifyReply{Kind: "inline", InReplyTo: root, Body: text + "\n\n" + replyMarkerOpen + string(marker) + replyMarkerClose}
	}
	quote := "Re: " + oneLine(firstLineOf(c.Body))
	if c.HTMLURL != "" {
		quote = fmt.Sprintf("Re: [%s's comment](%s)", oneLine(c.User.Login), c.HTMLURL)
	}
	return clarifyReply{Kind: "issue", Body: quote + "\n\n" + text + "\n\n" + replyMarkerOpen + string(marker) + replyMarkerClose}
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return clipRunes(line, 120)
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

var outcomeLabels = map[string]string{
	radar.OutcomeStillApplies:        "Still applies",
	radar.OutcomeWithdrawn:           "Withdrawn",
	radar.OutcomeDowngraded:          "Downgraded",
	radar.OutcomeResolvedByAssertion: "Resolved by assertion",
}

// outcomeText renders one finding's outcome for a reply.
func outcomeText(repo, head string, o radar.FindingOutcome) string {
	label := outcomeLabels[o.Outcome]
	if o.Outcome == radar.OutcomeDowngraded {
		label = fmt.Sprintf("Downgraded from %s to %s", o.FromSeverity, o.Severity)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**: %s", label, oneLine(o.Reply))
	if len(o.Evidence) > 0 {
		var refs []string
		for _, e := range o.Evidence {
			refs = append(refs, locationLink(repo, head, e.File, e.Line, e.EndLine))
		}
		b.WriteString(" Evidence: " + strings.Join(refs, ", ") + ".")
	}
	if o.Outcome == radar.OutcomeResolvedByAssertion {
		fmt.Fprintf(&b, " This rests on %s's word, not on code Radar could check, so a person must accept it before Radar would approve.", oneLine(o.Author))
	}
	if o.Steering {
		b.WriteString(" _The comment read as an instruction to Radar rather than context, so Radar did not follow it._")
	}
	return b.String()
}

// clarifyReplies renders a reply to each pending comment from its outcomes.
func clarifyReplies(repo, head string, pending []pendingClarification, outcomes []radar.FindingOutcome, findings map[string]radar.MergedFinding) []clarifyReply {
	var out []clarifyReply
	for _, p := range pending {
		rec := replyRecord{To: p.CommentID, Checked: true}
		var lines []string
		for _, o := range outcomes {
			if o.CommentID != p.CommentID {
				continue
			}
			rec.Results = append(rec.Results, replyResult{Finding: o.FindingID, Outcome: o.Outcome})
			if p.Kind == "inline" {
				lines = append(lines, outcomeText(repo, head, o))
				continue
			}
			f := findings[o.FindingID]
			lines = append(lines, fmt.Sprintf("- %s %s (`%s`): %s", f.Severity, oneLine(f.Title), shortID(f.ID), outcomeText(repo, head, o)))
		}
		for _, id := range p.limited {
			rec.Results = append(rec.Results, replyResult{Finding: id, Outcome: "limit"})
			f := findings[id]
			lines = append(lines, fmt.Sprintf("- %s %s (`%s`): **Not re-checked**: Radar has re-checked it %d times, its limit. A human reviewer can resolve it.", f.Severity, oneLine(f.Title), shortID(id), maxClarificationsPerFinding))
		}
		if len(lines) == 0 {
			continue
		}
		text := strings.Join(lines, "\n")
		if p.Kind != "inline" {
			text = "Radar re-checked the findings this comment is about:\n\n" + text
		}
		text += fmt.Sprintf("\n\n_Re-checked at `%s`. The Radar review comment is updated._", shortSHA(head))
		c := prComment{ID: p.CommentID, InReplyTo: p.root, HTMLURL: p.url, inline: p.Kind == "inline", Body: p.Body}
		c.User.Login = p.Author
		out = append(out, replyTo(c, text, rec))
	}
	return out
}

// failedReplies answers pending comments when the re-check could not run.
func failedReplies(pending []pendingClarification, why string) []clarifyReply {
	var out []clarifyReply
	for _, p := range pending {
		rec := replyRecord{To: p.CommentID, Checked: true}
		for _, id := range p.FindingIDs {
			rec.Results = append(rec.Results, replyResult{Finding: id, Outcome: "error"})
		}
		c := prComment{ID: p.CommentID, InReplyTo: p.root, HTMLURL: p.url, inline: p.Kind == "inline", Body: p.Body}
		c.User.Login = p.Author
		out = append(out, replyTo(c, "Radar could not re-check this: "+oneLine(why)+". The finding stands; a human reviewer can resolve the thread.", rec))
	}
	return out
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// evidenceChecker accepts a reference only to lines that exist in a regular
// file inside the checkout of the head.
func evidenceChecker(workdir string) func(radar.EvidenceRef) bool {
	root, err := filepath.EvalSymlinks(workdir)
	if workdir == "" || err != nil {
		return func(radar.EvidenceRef) bool { return false }
	}
	return func(e radar.EvidenceRef) bool {
		clean := filepath.Clean(e.File)
		if e.File == "" || filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, "..") || e.Line < 1 ||
			clean == ".git" || strings.HasPrefix(clean, ".git"+string(filepath.Separator)) {
			return false
		}
		path, err := filepath.EvalSymlinks(filepath.Join(root, clean))
		if err != nil || !strings.HasPrefix(path, root+string(filepath.Separator)) {
			return false
		}
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		defer f.Close()
		if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
			return false
		}
		end := max(e.EndLine, e.Line)
		lines := 0
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines++
			if lines >= end {
				return true
			}
		}
		return false
	}
}

func runClarify(args []string) int {
	flags := flag.NewFlagSet("radar-gh clarify", flag.ContinueOnError)
	repo := flags.String("repo", "", "GitHub owner/repository (required)")
	prNumber := flags.Int("pr", 0, "pull request number (required)")
	policyPath := flags.String("policy", "", "path to the versioned JSON policy (required)")
	agentName := flags.String("agent", "", "the review agents, exactly as the findings stage ran them (required)")
	expectedHead := flags.String("expected-head", "", "the pull request head to answer at (required)")
	packsDir := flags.String("review-packs", "", "directory of review packs, as the findings stage had")
	reviewWorkdir := flags.String("review-workdir", "", "read-only checkout of the pull request head (required: clarifications are verified in it)")
	reusePath := flags.String("reuse", "", "the findings stage's review record for this head (required)")
	ledgerPath := flags.String("clarifications", "", "the pull request's clarification ledger so far")
	ledgerOut := flags.String("clarifications-out", "", "write the updated clarification ledger here (required)")
	commentPath := flags.String("comment", "", "write the updated Radar review comment here")
	repliesPath := flags.String("replies", "", "write the replies to post (JSON) here (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *repo == "" || *prNumber <= 0 || *policyPath == "" || *agentName == "" || *expectedHead == "" || *reviewWorkdir == "" || *reusePath == "" || *ledgerOut == "" || *repliesPath == "" {
		fmt.Fprintln(os.Stderr, "radar-gh clarify: -repo, -pr, -policy, -agent, -expected-head, -review-workdir, -reuse, -clarifications-out and -replies are required")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "radar-gh clarify:", err)
		return 1
	}
	policy, err := loadPullRequestPolicy(*policyPath)
	if err != nil {
		return fail(err)
	}
	reviewer, agents, err := buildReviewer(policy, *policyPath, *agentName, *packsDir, *reviewWorkdir)
	if err != nil {
		return fail(err)
	}
	// A clarification re-checks findings already posted; it never pays for
	// a new review.
	reviewer.RequireRecordedReviews()
	snap, err := fetchFindingsSnapshot(*repo, *prNumber, policy)
	if err != nil {
		return fail(err)
	}
	in := snap.Input
	if in.HeadSHA != *expectedHead {
		return fail(fmt.Errorf("expected head %s, found %s", *expectedHead, in.HeadSHA))
	}
	if err := useReviewRecord(reviewer, *reusePath, in.HeadSHA); err != nil {
		return fail(fmt.Errorf("the findings stage's record: %w", err))
	}
	ledger := radar.ClarificationLedger{SchemaVersion: radar.ClarificationLedgerSchemaVersion, PullRequest: *prNumber}
	if *ledgerPath != "" {
		if l, err := loadClarificationLedger(*ledgerPath, *prNumber); err != nil {
			fmt.Fprintln(os.Stderr, "radar-gh clarify: starting a new ledger:", err)
		} else {
			ledger = l
		}
	}
	reviewer.UseClarifications(ledger)
	// The same rounds the findings stage compared with.
	previous, previousRead := readPreviousComment(*repo, *prNumber)
	loadHistory(*repo, *prNumber, &in, previous)
	before := reviewer.Findings(in)
	for _, v := range reviewerVerdicts(before) {
		if v.Agent.Failed {
			return fail(fmt.Errorf("the findings stage's record does not cover %s at %s; nothing to re-check against", v.Reviewer, shortSHA(in.HeadSHA)))
		}
	}

	issue, review, err := fetchPRComments(*repo, *prNumber)
	if err != nil {
		return fail(err)
	}
	commits := []string{in.HeadSHA}
	if in.Prior != nil {
		commits = append(commits, in.Prior.Head)
	}
	if shas, err := fetchCommitSHAs(*repo, *prNumber); err != nil {
		// Without them a commit named in a comment can read as an unknown
		// finding ID; the comment still gets an answer.
		fmt.Fprintln(os.Stderr, "radar-gh clarify: commits:", err)
	} else {
		commits = append(commits, shas...)
	}
	plan := planClarifications(planInput{
		issue: issue, review: review, findings: before.Findings, open: openBlocking(before), ledger: ledger,
		prAuthor: in.Author, head: in.HeadSHA, commits: commits, now: time.Now(),
		permitted: func(login string) (bool, error) { return hasWriteAccess(*repo, login) },
	})
	ledger.Handled = append(ledger.Handled, plan.handled...)
	replies := plan.notices

	if len(plan.pending) > 0 {
		findings := map[string]radar.MergedFinding{}
		for _, f := range before.Findings {
			// Most severe first: an id two findings share names the worse.
			if _, seen := findings[f.ID]; !seen {
				findings[f.ID] = f
			}
		}
		req := radar.ClarifyRequest{HeadSHA: in.HeadSHA}
		for _, f := range in.Files {
			if f.Status == "added" {
				req.AddedFiles = append(req.AddedFiles, f.Path)
			}
		}
		asked := map[string]bool{}
		for _, p := range plan.pending {
			req.Clarifications = append(req.Clarifications, p.Clarification)
			for _, id := range p.FindingIDs {
				if !asked[id] {
					asked[id] = true
					req.Findings = append(req.Findings, findings[id])
				}
			}
		}
		adjudicator := chooseAdjudicator(agents)
		if adjudicator == nil {
			replies = append(replies, failedReplies(plan.pending, "no configured reviewer can read the repository to check a clarification")...)
		} else {
			res := adjudicator.Clarify(req)
			writeClarifySummary(res)
			if res.Failed {
				replies = append(replies, failedReplies(plan.pending, "the re-check failed")...)
				fmt.Fprintln(os.Stderr, "radar-gh clarify: re-check failed:", res.Error)
			} else {
				adjs := radar.EnforceAdjudications(req, res.Adjudications, evidenceChecker(*reviewWorkdir))
				outcomes := ledgerEntries(plan.pending, adjs, findings, in.HeadSHA, res.Adjudicator, time.Now())
				ledger.Outcomes = append(ledger.Outcomes, outcomes...)
				replies = append(replies, clarifyReplies(*repo, in.HeadSHA, plan.pending, outcomes, findings)...)
			}
		}
	}
	if replies == nil {
		replies = []clarifyReply{}
	}
	if err := writeJSONFile(*ledgerOut, ledger); err != nil {
		return fail(err)
	}
	if err := writeJSONFile(*repliesPath, replies); err != nil {
		return fail(err)
	}
	reviewer.UseClarifications(ledger)
	decision := reviewer.Findings(in)
	decision.PullRequestTotals = pullRequestTotals(decision, previous, !previousRead)
	if *commentPath != "" && len(plan.pending) > 0 {
		if err := os.WriteFile(*commentPath, []byte(renderReviewComment(*repo, decision, previous)), 0o644); err != nil {
			return fail(err)
		}
	}
	if err := writeJSON(os.Stdout, decision); err != nil {
		return fail(err)
	}
	return 0
}

// openBlocking returns the P0 and P1 findings at this head that earlier
// clarifications left open: not withdrawn, resolved by assertion or
// downgraded below P1.
func openBlocking(d radar.PullRequestReview) []string {
	applied := map[string]radar.AppliedClarification{}
	for _, c := range d.Clarifications {
		applied[radar.ClarificationKey(c.FindingID, c.File, c.Line)] = c
	}
	var out []string
	for _, f := range d.Findings {
		severity := f.Severity
		if c, ok := applied[radar.ClarificationKey(f.ID, f.File, f.Line)]; ok {
			if c.Outcome == radar.OutcomeWithdrawn || c.Outcome == radar.OutcomeResolvedByAssertion {
				continue
			}
			severity = c.Severity
		}
		if severity == "P0" || severity == "P1" {
			out = append(out, f.ID)
		}
	}
	return out
}

// chooseAdjudicator is the first agent that can re-check a clarification
// in the repository: the Codex reviewer where configured.
func chooseAdjudicator(agents []radar.ReviewAgent) radar.ClarifyAgent {
	for _, a := range agents {
		ca, ok := a.(radar.ClarifyAgent)
		ra, reads := a.(radar.RepoAccessAgent)
		if ok && reads && ra.RepoAccess() {
			return ca
		}
	}
	return nil
}

// ledgerEntries records one outcome per comment and finding it asked about.
func ledgerEntries(pending []pendingClarification, adjs []radar.Adjudication, findings map[string]radar.MergedFinding, head, adjudicator string, now time.Time) []radar.FindingOutcome {
	byID := map[string]radar.Adjudication{}
	for _, a := range adjs {
		byID[a.FindingID] = a
	}
	var out []radar.FindingOutcome
	for _, p := range pending {
		for _, id := range p.FindingIDs {
			a, ok := byID[id]
			if !ok {
				continue
			}
			f := findings[id]
			out = append(out, radar.FindingOutcome{
				FindingID: id, File: f.File, Line: f.Line, RuleID: f.RuleID, Title: f.Title,
				FromSeverity: f.Severity, Severity: a.Severity, Outcome: a.Outcome, Basis: a.Basis,
				Evidence: a.Evidence, Steering: a.Steering, Reply: a.Reply,
				CommentID: p.CommentID, Author: p.Author, ByPRAuthor: p.ByPRAuthor,
				Head: head, Adjudicator: adjudicator, At: now.UTC().Format(time.RFC3339),
			})
		}
	}
	return out
}

func writeClarifySummary(res radar.ClarifyResult) {
	elapsed := int64(0)
	if res.ElapsedMS != nil {
		elapsed = *res.ElapsedMS
	}
	u := radar.ReviewUsage{}
	if res.Usage != nil {
		u = *res.Usage
	}
	fmt.Fprintf(os.Stderr, "radar-gh clarify: %s re-checked in %ds: %d input tokens (%d cached), %d output, %d requests; failed=%t\n",
		res.Adjudicator, elapsed/1000, u.InputTokens, u.CacheReadTokens, u.OutputTokens, u.Requests, res.Failed)
}

// fetchPRComments returns the pull request's issue and review comments.
func fetchPRComments(repo string, number int) (issue, review []prComment, err error) {
	var pages [][]prComment
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, number)); err != nil {
		return nil, nil, fmt.Errorf("reading comments: %w", err)
	}
	for _, page := range pages {
		issue = append(issue, page...)
	}
	pages = nil
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", repo, number)); err != nil {
		return nil, nil, fmt.Errorf("reading review comments: %w", err)
	}
	for _, page := range pages {
		for _, c := range page {
			c.inline = true
			review = append(review, c)
		}
	}
	return issue, review, nil
}

// fetchCommitSHAs returns the pull request's commit SHAs.
func fetchCommitSHAs(repo string, number int) ([]string, error) {
	var pages [][]struct {
		SHA string `json:"sha"`
	}
	if err := ghJSON(&pages, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/commits?per_page=100", repo, number)); err != nil {
		return nil, err
	}
	var out []string
	for _, page := range pages {
		for _, c := range page {
			out = append(out, c.SHA)
		}
	}
	return out, nil
}

// hasWriteAccess reports whether login can push to the repository.
func hasWriteAccess(repo, login string) (bool, error) {
	var perm struct {
		Permission string `json:"permission"`
	}
	if err := ghJSON(&perm, "api", fmt.Sprintf("repos/%s/collaborators/%s/permission", repo, login)); err != nil {
		fmt.Fprintf(os.Stderr, "radar-gh clarify: permission of %s: %v\n", login, err)
		return false, err
	}
	return perm.Permission == "admin" || perm.Permission == "write" || perm.Permission == "maintain", nil
}
