package radar

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// ReviewRecord holds the verdicts the review agents returned for one pull
// request head, so a later evaluation of the same head reuses them instead of
// calling the agents again. The findings stage writes it before checks
// finish; the verdict stage reads it once they have, so both rest on the same
// reviews.
type ReviewRecord struct {
	SchemaVersion int    `json:"schema_version"`
	HeadSHA       string `json:"head_sha"`
	// Reviews are keyed by RequestKey: the agent and the exact request it
	// was sent, so a verdict is reused only for a byte-identical request.
	Reviews []RecordedReview `json:"reviews"`
}

// RecordedReview is one agent's verdict on one request.
type RecordedReview struct {
	Reviewer string `json:"reviewer"`
	Request  string `json:"request"`
	// Patches is PatchKey for the same request, and Hunks where its
	// patches' hunks sat, by file: [first line, line count] at this head.
	// A later head whose request differs only in where the hunks sit
	// reuses the review with its lines moved.
	Patches string              `json:"patches,omitempty"`
	Hunks   map[string][][2]int `json:"hunks,omitempty"`
	// Content is ContentKey for the same request: PatchKey without the
	// repository's guidance. A later head whose request differs only in
	// that, and in where the hunks sit, reuses the review and says the
	// guidance changed.
	Content string `json:"content,omitempty"`
	// Substance is SubstanceKey for the same request. A later head reached
	// by a push that changed only documentation or withheld generated files
	// reuses the review.
	Substance string    `json:"substance,omitempty"`
	Result    ACRResult `json:"result"`
}

const reviewRecordSchemaVersion = 1

// RequestKey identifies what an agent was asked: its provenance, the system
// prompt and the rendered diff. Any change to the policy's criteria, the
// chunking, the withheld files or the patches changes the key.
func RequestKey(reviewer string, d Diff) string {
	// The pull request context (threads, check states) changes between the
	// findings and verdict stages; the review is of the patches, so the key
	// leaves it out and the verdict reuses the findings stage's review.
	d.PRContext = ""
	h := sha256.New()
	for _, part := range []string{reviewer, ReviewSystemPrompt(d), renderDiffForReview(d)} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// patchHunkHeader is a unified diff hunk header. Its line numbers change
// when the base branch adds or removes lines above a hunk; the hunk itself,
// and the enclosing function GitHub appends after it, may not.
var patchHunkHeader = regexp.MustCompile(`(?m)^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ContentKey is PatchKey without the guidance read from the default branch,
// but still with the review packs selected from it. Every guidance edit
// there changes each open pull request's PatchKey, though not its patches.
// A pack change, which may add a rule the earlier review never
// applied, still changes the key.
func ContentKey(reviewer string, d Diff) string {
	// The fixed instructions that introduce the criteria stay too: a
	// release that changes them reviews anew.
	packs, instructions := "", ""
	if d.Criteria != nil {
		packs, instructions = d.Criteria.Packs, reviewCriteriaInstructions
	}
	d.Criteria = nil
	h := sha256.New()
	for _, part := range []string{PatchKey(reviewer, d), packs, instructions} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PatchKey is RequestKey with every hunk header's line numbers dropped: two
// requests with the same key send the same prompt and the same patches,
// line for line, with the same context lines and enclosing functions, at
// possibly other places in their files.
func PatchKey(reviewer string, d Diff) string {
	d.PRContext = ""
	changes := make([]Change, len(d.Changes))
	for i, c := range d.Changes {
		// Only the patches: guidance or notes quoting a hunk header must
		// still change the key.
		c.Content = patchHunkHeader.ReplaceAllString(c.Content, "@@ @@")
		changes[i] = c
	}
	d.Changes = changes
	h := sha256.New()
	for _, part := range []string{reviewer, ReviewSystemPrompt(d), renderDiffForReview(d)} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SubstanceKey is RequestKey with the contents of every documentation file
// trivial reports blanked, and without the withheld generated files trivial
// reports. Files trivial does not report (lockfiles, guidance, paths the
// policy names) stay whole, shown or withheld.
// The file names stay, since a review must name exactly the files its
// request shows. Everything else stays as sent, hunk headers included, so a
// code patch that moved is not carried (the moved-lines reuse's survival proof is for
// that), nor a lockfile change, whose supply-chain signals only a reviewer
// reads. A generated file the request shows in full (one the pull request
// adds) stays whole too, unless it is documentation, which is blanked
// generated or not. It is "" when every change is blanked: such a
// request is the push itself, reviewed anew.
func SubstanceKey(reviewer string, d Diff, trivial func(string) bool) string {
	changes := make([]Change, len(d.Changes))
	kept := false
	for i, c := range d.Changes {
		// A rename is blanked only when its source is trivial too: a
		// patch renamed from code or guidance stays whole.
		if isDocPath(c.File) && trivial(c.File) && (c.PreviousFile == "" || trivial(c.PreviousFile)) {
			c.Content, c.Complexity = "", 0
		} else {
			kept = true
		}
		changes[i] = c
	}
	if !kept {
		return ""
	}
	var withheld []GeneratedFile
	for _, w := range d.Withheld {
		// A withheld rename does not say where from, so it stays.
		if !trivial(w.Path) || w.Status == "renamed" {
			withheld = append(withheld, w)
		}
	}
	d.Changes, d.Withheld = changes, withheld
	return RequestKey(reviewer, d)
}

// docExtensions are the documentation files a push may change without a
// new review. MDX is left out: it can import and run code.
var docExtensions = map[string]bool{".md": true, ".markdown": true, ".rst": true, ".adoc": true}

func isDocPath(p string) bool {
	return docExtensions[strings.ToLower(path.Ext(p))]
}

// dependencyLockfiles are lockfile names beyond those Radar summarizes
// (lockfileKindOf) and the *.lock, *.lock.* and *.lockfile patterns: a push
// to one is always reviewed, however the policy classifies it.
var dependencyLockfiles = map[string]bool{
	"pnpm-lock.yaml": true, "npm-shrinkwrap.json": true, "bun.lockb": true,
	"packages.lock.json": true, "Package.resolved": true, "go.work.sum": true,
}

func dependencyLockfile(p string) bool {
	if _, ok := lockfileKindOf(p); ok {
		return true
	}
	base := path.Base(p)
	return dependencyLockfiles[base] || strings.HasSuffix(base, ".lock") ||
		strings.Contains(base, ".lock.") || strings.HasSuffix(base, ".lockfile")
}

// agentGuidanceFile reports files that may instruct coding and review
// agents, whatever the review rules list. Tools keep inventing places for
// them, so rather than a list of tools it takes every dotfile and anything
// under a dot-directory (.github, .claude, .agents, ...), plus the
// well-known names and suffixes outside them.
func agentGuidanceFile(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	base := strings.ToLower(path.Base(p))
	switch strings.ToUpper(strings.TrimSuffix(base, path.Ext(base))) {
	case "AGENTS", "AGENT", "AGENTS.OVERRIDE", "CLAUDE", "CLAUDE.LOCAL", "GEMINI", "CONVENTIONS", "SKILL":
		return true
	}
	for _, suffix := range []string{".instructions.md", ".prompt.md", ".chatmode.md", ".agent.md"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// diffHunks is where each change's hunks sit on the head's side: the first
// line and how many head-side lines the request shows. That count comes
// from the lines present, not the header, since a hunk split across
// requests keeps its whole count in the first piece's header. It is nil,
// and the request's review can then not be moved, when a change shows
// patch lines before its first header (a later piece of a split hunk) or
// has no hunk at all: those lines have no coordinates to check.
func diffHunks(d Diff) map[string][][2]int {
	out := make(map[string][][2]int, len(d.Changes))
	for _, c := range d.Changes {
		var hunks [][2]int
		// A blank line inside a hunk is a context line whose leading space
		// was trimmed, as rightSideLines in radar-gh counts it.
		for _, line := range strings.Split(strings.TrimSuffix(c.Content, "\n"), "\n") {
			if m := patchHunkHeader.FindStringSubmatch(line); m != nil {
				start, _ := strconv.Atoi(m[1])
				hunks = append(hunks, [2]int{start, 0})
				continue
			}
			if len(hunks) == 0 {
				return nil
			}
			if line == "" || strings.HasPrefix(line, "+") || strings.HasPrefix(line, " ") {
				hunks[len(hunks)-1][1]++
			}
		}
		if len(hunks) == 0 {
			return nil
		}
		out[c.File] = append(out[c.File], hunks...)
	}
	return out
}

// moveRange maps lines first..last of file at the recorded head to this
// head. Both must sit in one of the patch's hunks, which moves them by its
// offset. Any other line is one the patches do not show, which the base
// branch may have changed, so the range does not map. last is 0 for a
// single line.
func moveRange(from, to map[string][][2]int, file string, first, last int) (int, int, bool) {
	a, b := from[file], to[file]
	if len(a) != len(b) {
		return 0, 0, false
	}
	end := last
	if end == 0 {
		end = first
	}
	for i, h := range a {
		if first >= h[0] && end < h[0]+h[1] && first <= end {
			off := b[i][0] - h[0]
			if last == 0 {
				return first + off, 0, true
			}
			return first + off, last + off, true
		}
	}
	return 0, 0, false
}

// moveResult moves a recorded review's findings and pointers with its
// patches' hunks. It fails when any of them names a line outside one hunk:
// that review is then done anew rather than shown at a guessed line. Line
// numbers quoted in free text (a summary, an assumption's basis) are the
// earlier head's.
func moveResult(res ACRResult, from, to map[string][][2]int) (ACRResult, bool) {
	findings := make([]ReviewFinding, len(res.Findings))
	for i, f := range res.Findings {
		if f.Line != 0 || f.EndLine != 0 {
			var ok bool
			if f.Line, f.EndLine, ok = moveRange(from, to, f.File, f.Line, f.EndLine); !ok {
				return ACRResult{}, false
			}
		}
		findings[i] = f
	}
	pointers := make([]ReviewPointer, len(res.WhereToLook))
	for i, p := range res.WhereToLook {
		if p.Line != 0 {
			var ok bool
			if p.Line, _, ok = moveRange(from, to, p.File, p.Line, 0); !ok {
				return ACRResult{}, false
			}
		}
		pointers[i] = p
	}
	if res.Findings != nil {
		res.Findings = findings
	}
	if res.WhereToLook != nil {
		res.WhereToLook = pointers
	}
	return res, true
}

// promptSHA256 hashes a request prefix for ReviewUsage.PrefixSHA256.
func promptSHA256(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reviewMemo records each agent's verdicts and answers repeated requests
// from them.
type reviewMemo struct {
	// recordedOnly answers a request no record holds with a failed review
	// instead of calling the agent (see RequireRecordedReviews).
	recordedOnly bool
	mu           sync.Mutex
	byKey        map[string]ACRResult
	order        []RecordedReview
	reused       int
	// prior holds an earlier head's recorded reviews (UsePriorReviewRecord).
	// One is used only when a request at this head is byte-identical to it,
	// and joins this head's record only then.
	prior     map[string]RecordedReview
	priorHead string
	// priorByPatches indexes prior by PatchKey, for a request that differs
	// from a recorded one only in where its hunks sit.
	priorByPatches map[string]RecordedReview
	// priorByContent indexes prior by ContentKey, for a request that also
	// differs in the guidance it carries.
	priorByContent map[string]RecordedReview
	// priorBySubstance indexes prior by SubstanceKey, for a request after a
	// push that changed only trivial files.
	priorBySubstance map[string]RecordedReview
	// trivial reports a file whose changes alone need no new review:
	// documentation or a generated path. carry is set when the
	// push from priorHead changed only such files.
	trivial func(string) bool
	carry   bool
	// reusedFrom is priorHead once a review has been reused from it,
	// reusedMoved counts those reused with their lines moved, and
	// reusedGuidance those reused although the guidance changed.
	reusedFrom     string
	reusedMoved    int
	reusedGuidance int
	reusedCarried  int
	// push is how each file changed from priorHead to this head, when the
	// comparison is complete (usePush); without it nothing is moved.
	push     map[string]fileDelta
	pushFrom string
}

// usePush keeps the comparison from the prior review's head to this head,
// which proves a moved hunk's lines survived the push and only shifted. It
// counts only when it is from the head whose record is offered.
func (m *reviewMemo) usePush(p *PriorReview) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.push, m.pushFrom, m.carry = nil, "", false
	if p == nil || !p.ChangesComplete || p.Head == "" || p.Head != m.priorHead {
		return
	}
	m.push, m.pushFrom = make(map[string]fileDelta, len(p.Changes)), p.Head
	m.carry = len(p.Changes) > 0 && m.trivial != nil
	for _, f := range p.Changes {
		m.push[f.Path] = newFileDelta(f)
		if m.trivial != nil && (!m.trivial(f.Path) || f.PreviousPath != "" && !m.trivial(f.PreviousPath)) {
			m.carry = false
		}
	}
}

// survived reports whether every line the request shows in each hunk is
// one the push did not change and that sat exactly the hunk's offset
// earlier at the prior head. An identical patch the author moved elsewhere
// (between two identical blocks, say) changes those lines, so it does not
// survive and is reviewed anew.
func (m *reviewMemo) survived(from, to map[string][][2]int) bool {
	if m.push == nil || m.pushFrom != m.priorHead || len(to) == 0 || len(from) != len(to) {
		return false
	}
	for file, hunks := range to {
		was := from[file]
		if len(was) != len(hunks) {
			return false
		}
		d, changed := m.push[file]
		for i, h := range hunks {
			off := h[0] - was[i][0]
			if h[1] == 0 && off != 0 {
				// A deletion-only hunk shows no head-side line whose
				// survival could be checked.
				return false
			}
			if !changed {
				// The push did not touch the file, so nothing in it moved.
				if off != 0 {
					return false
				}
				continue
			}
			if d.whole || d.previousPath != file {
				return false
			}
			for line := h[0]; line < h[0]+h[1]; line++ {
				if d.changed[line] || d.toPrevious(line) != line-off {
					return false
				}
			}
		}
	}
	return true
}

func (m *reviewMemo) lookup(key string) (ACRResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res, ok := m.byKey[key]
	if !ok {
		var review RecordedReview
		if review, ok = m.prior[key]; ok {
			res = review.Result
			m.byKey[key] = res
			m.order = append(m.order, review)
			m.reusedFrom = m.priorHead
		}
	}
	if ok {
		m.reused++
	}
	return res, ok
}

// store keeps only complete verdicts. A failure (no reviewed files, as every
// fail-safe result has) or a response cut off at the output limit is left
// out, so the next stage tries again instead of inheriting a timeout, a
// provider error or a truncation.
func (m *reviewMemo) store(review RecordedReview) {
	res := review.Result
	if res.Failed || res.Truncated || len(res.ReviewedFiles) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byKey[review.Request]; ok {
		return
	}
	m.byKey[review.Request] = res
	m.order = append(m.order, review)
}

// lookupMoved answers a request from the earlier head's review of the same
// patches at other lines, with its findings moved to this head's lines, or
// of the same patches under other guidance. It joins this head's
// record under this head's request, so the verdict stage reuses it as it
// would a new review.
func (m *reviewMemo) lookupMoved(review RecordedReview) (ACRResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if res, ok := m.byKey[review.Request]; ok {
		// A concurrent request reused it first.
		m.reused++
		return res, true
	}
	prior, ok := m.priorByPatches[review.Patches]
	guidance := false
	if !ok && review.Content != "" {
		prior, ok = m.priorByContent[review.Content]
		guidance = ok
	}
	if !ok {
		return ACRResult{}, false
	}
	// Hunks at the same head-side lines can still differ in their
	// base-side numbers, so every such reuse needs the push to prove its
	// lines survived; a hunk that did not move has offset 0.
	if !m.survived(prior.Hunks, review.Hunks) {
		return ACRResult{}, false
	}
	res, ok := moveResult(prior.Result, prior.Hunks, review.Hunks)
	if !ok {
		return ACRResult{}, false
	}
	moved := !sameHunks(prior.Hunks, review.Hunks)
	review.Result = res
	m.byKey[review.Request] = res
	m.order = append(m.order, review)
	m.reusedFrom = m.priorHead
	m.reused++
	if moved {
		m.reusedMoved++
	}
	if guidance {
		m.reusedGuidance++
	}
	return res, true
}

// sameHunks reports whether two requests' hunks sit at the same head-side
// lines, so a reuse moved nothing.
func sameHunks(a, b map[string][][2]int) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for file, hunks := range a {
		other, ok := b[file]
		if !ok || len(other) != len(hunks) {
			return false
		}
		for i := range hunks {
			if hunks[i] != other[i] {
				return false
			}
		}
	}
	return true
}

// lookupCarried answers a request from the earlier head's review when the
// push since changed only trivial files and the request differs from that
// review's only in them. A review with a finding or pointer in a file the
// push changed is done anew: its lines may no longer say what it saw.
func (m *reviewMemo) lookupCarried(review RecordedReview) (ACRResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// As survived does, the push must be from the head the prior record
	// is of.
	if !m.carry || review.Substance == "" || m.push == nil || m.pushFrom != m.priorHead {
		return ACRResult{}, false
	}
	if res, ok := m.byKey[review.Request]; ok {
		// A concurrent request reused it first.
		m.reused++
		return res, true
	}
	prior, ok := m.priorBySubstance[review.Substance]
	if !ok {
		return ACRResult{}, false
	}
	// A renamed file counts under both names. A finding names its file as
	// the reviewer wrote it, so "./a" is "a"; one naming no file stands. A
	// name that is not plainly a repository path (absolute, with a drive
	// or backslashes, or leaving the repository) cannot be checked, so it
	// blocks.
	pushed := map[string]bool{}
	for file, d := range m.push {
		pushed[file] = true
		if d.previousPath != "" {
			pushed[d.previousPath] = true
		}
	}
	names := func(file string) bool {
		if file == "" {
			return false
		}
		if strings.ContainsAny(file, `\:`) {
			return true
		}
		clean := path.Clean(file)
		// A trivial file the key blanks or drops (documentation, or a
		// withheld generated file) may have changed without the push
		// showing it (after a retargeted base), so a finding on one is
		// not carried either.
		return pushed[clean] || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") ||
			m.trivial != nil && m.trivial(clean)
	}
	for _, f := range prior.Result.Findings {
		if names(f.File) {
			return ACRResult{}, false
		}
	}
	for _, p := range prior.Result.WhereToLook {
		if names(p.File) {
			return ACRResult{}, false
		}
	}
	review.Result = prior.Result
	m.byKey[review.Request] = prior.Result
	m.order = append(m.order, review)
	m.reusedFrom = m.priorHead
	m.reused++
	m.reusedCarried++
	return prior.Result, true
}

// memoAgent answers from the memo when it can and records what the wrapped
// agent returns otherwise.
type memoAgent struct {
	agent ReviewAgent
	name  string
	memo  *reviewMemo
}

// ReviewVariantAgent is implemented by agents whose requests differ by mode
// beyond the prompt and diff, such as with or without repository tools; the
// variant is part of a recorded review's key.
type ReviewVariantAgent interface {
	ReviewVariant() string
}

func (a memoAgent) Review(d Diff) ACRResult {
	name := a.name
	if v, ok := a.agent.(ReviewVariantAgent); ok && v.ReviewVariant() != "" {
		name += "\x00" + v.ReviewVariant()
	}
	key := RequestKey(name, d)
	review := RecordedReview{Reviewer: a.name, Request: key, Patches: PatchKey(name, d), Hunks: diffHunks(d), Content: ContentKey(name, d)}
	if a.memo.trivial != nil {
		review.Substance = SubstanceKey(name, d, a.memo.trivial)
	}
	if res, ok := a.memo.lookup(key); ok {
		// Reused, it cost nothing now; the record keeps what it cost then.
		res.Usage = &ReviewUsage{ReusedRequests: 1}
		return res
	}
	if res, ok := a.memo.lookupMoved(review); ok {
		res.Usage = &ReviewUsage{ReusedRequests: 1}
		return res
	}
	if res, ok := a.memo.lookupCarried(review); ok {
		res.Usage = &ReviewUsage{ReusedRequests: 1}
		return res
	}
	if a.memo.recordedOnly {
		return ACRResult{Summary: "not reviewed at the findings stage; no new review was requested", Failed: true}
	}
	res := a.agent.Review(d)
	if res.Usage != nil {
		// A copy: the agent may hand the same usage to later results.
		u := *res.Usage
		u.CostUSD, u.Unpriced = 0, false
		if cost, ok := EstimateReviewCostUSD(a.name, &u); ok {
			u.CostUSD = cost
		} else {
			u.Unpriced = true
		}
		res.Usage = &u
	}
	review.Result = res
	a.memo.store(review)
	return res
}

func (a memoAgent) ConcurrentReviewSafe() bool { return concurrentReviewSafe(a.agent) }

func (a memoAgent) Describe() string { return a.name }

// UseReviewRecord primes the reviewer with recorded verdicts. A record for a
// different head is an error: the caller asked for the wrong record.
func (r *PullRequestReviewer) UseReviewRecord(rec ReviewRecord, head string) error {
	if err := checkReviewRecord(rec, head); err != nil {
		return err
	}
	r.memo.mu.Lock()
	defer r.memo.mu.Unlock()
	for _, review := range rec.Reviews {
		if _, ok := r.memo.byKey[review.Request]; ok {
			continue
		}
		r.memo.byKey[review.Request] = review.Result
		r.memo.order = append(r.memo.order, review)
	}
	return nil
}

// UsePriorReviewRecord offers the reviews recorded for an earlier head of the
// same pull request. A later push often leaves a reviewer's
// request byte-identical, as when the base branch is merged in and the
// pull request's own patches do not change; such a request is answered from
// the earlier review instead of a new model call. Any other request is
// reviewed anew. rec must be for priorHead, which must not be head: the
// caller binds the record to a trusted run of that earlier head, as it binds
// UseReviewRecord's to this one.
func (r *PullRequestReviewer) UsePriorReviewRecord(rec ReviewRecord, priorHead, head string) error {
	if priorHead == "" || priorHead == head {
		return fmt.Errorf("prior head %q is not an earlier head than %q", priorHead, head)
	}
	if err := checkReviewRecord(rec, priorHead); err != nil {
		return err
	}
	prior := make(map[string]RecordedReview, len(rec.Reviews))
	byPatches := make(map[string]RecordedReview, len(rec.Reviews))
	byContent := make(map[string]RecordedReview, len(rec.Reviews))
	bySubstance := make(map[string]RecordedReview, len(rec.Reviews))
	for _, review := range rec.Reviews {
		prior[review.Request] = review
		// Recorded before carrying, a review has no substance key.
		if review.Substance != "" {
			bySubstance[review.Substance] = review
		}
		// Recorded before moved-line reuse, a review has no patch key or hunks;
		// before guidance-change reuse, no content key.
		if review.Patches == "" || review.Hunks == nil {
			continue
		}
		byPatches[review.Patches] = review
		if review.Content != "" {
			byContent[review.Content] = review
		}
	}
	r.memo.mu.Lock()
	defer r.memo.mu.Unlock()
	r.memo.prior, r.memo.priorHead = prior, priorHead
	r.memo.priorByPatches, r.memo.priorByContent, r.memo.priorBySubstance = byPatches, byContent, bySubstance
	return nil
}

// checkReviewRecord validates every entry before any is used, so a record
// rejected here leaves nothing behind.
func checkReviewRecord(rec ReviewRecord, head string) error {
	if rec.SchemaVersion != reviewRecordSchemaVersion {
		return fmt.Errorf("review record schema version %d, want %d", rec.SchemaVersion, reviewRecordSchemaVersion)
	}
	if rec.HeadSHA == "" || rec.HeadSHA != head {
		return fmt.Errorf("review record is for head %q, not %q", rec.HeadSHA, head)
	}
	for _, review := range rec.Reviews {
		if review.Request == "" || review.Reviewer == "" {
			return fmt.Errorf("review record entry without reviewer or request key")
		}
		if review.Result.Failed || review.Result.Truncated || len(review.Result.ReviewedFiles) == 0 {
			return fmt.Errorf("review record holds an incomplete review from %s", review.Reviewer)
		}
	}
	return nil
}

// RequireRecordedReviews makes the reviewer answer only from recorded
// reviews: a request no record holds fails instead of calling an agent. A
// clarification re-checks findings already posted and must not pay for a
// new review.
func (r *PullRequestReviewer) RequireRecordedReviews() { r.memo.recordedOnly = true }

// ReviewRecord returns every verdict recorded or reused so far, bound to head.
func (r *PullRequestReviewer) ReviewRecord(head string) ReviewRecord {
	r.memo.mu.Lock()
	defer r.memo.mu.Unlock()
	return ReviewRecord{
		SchemaVersion: reviewRecordSchemaVersion,
		HeadSHA:       head,
		Reviews:       append([]RecordedReview{}, r.memo.order...),
	}
}
