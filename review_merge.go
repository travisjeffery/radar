package radar

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// MergedFinding is one issue as the reviewers together reported it. Two
// reviewers' findings are the same issue when they name the same file, lines
// within mergeLineWindow of each other, and the same rule (or both none).
// Agreement raises confidence: Reviewers lists everyone who found it.
type MergedFinding struct {
	ReviewFinding
	// Reviewers raised this finding; more than one means they agree.
	Reviewers []string `json:"reviewers"`
}

// Agreed reports whether more than one reviewer raised the finding.
func (f MergedFinding) Agreed() bool { return len(f.Reviewers) > 1 }

// mergeLineWindow is how far apart two reviewers' lines may be for the same
// issue: they often cite the start of a function and the faulty line.
const mergeLineWindow = 3

var severityRank = map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}

// mergeReviewerFindings collapses the reviewers' findings into one list,
// most severe first. A merged finding takes the higher severity, the highest
// confidence, and its first reporter's wording.
func mergeReviewerFindings(results []ACRResult, name func(int) string) []MergedFinding {
	var out []MergedFinding
	for i, res := range results {
		for _, f := range res.Findings {
			reviewer := name(i)
			if j := matchFinding(out, f); j >= 0 {
				m := &out[j]
				if !containsFold(m.Reviewers, reviewer) {
					m.Reviewers = append(m.Reviewers, reviewer)
				}
				if severityRank[f.Severity] < severityRank[m.Severity] {
					m.Severity = f.Severity
				}
				m.Confidence = max(m.Confidence, f.Confidence)
				if m.Recommendation == "" {
					m.Recommendation = f.Recommendation
				}
				continue
			}
			out = append(out, MergedFinding{ReviewFinding: f, Reviewers: []string{reviewer}})
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if severityRank[out[a].Severity] != severityRank[out[b].Severity] {
			return severityRank[out[a].Severity] < severityRank[out[b].Severity]
		}
		if out[a].File != out[b].File {
			return out[a].File < out[b].File
		}
		return out[a].Line < out[b].Line
	})
	return out
}

func matchFinding(merged []MergedFinding, f ReviewFinding) int {
	for j, m := range merged {
		near := abs(m.Line-f.Line) <= mergeLineWindow
		// The id hashes file, rule and title, so two distant issues with a
		// common title ("Missing error handling") share one; they stay
		// separate unless their lines are close too.
		if m.ID != "" && m.ID == f.ID && near {
			return j
		}
		if f.File != "" && m.File == f.File && m.RuleID == f.RuleID && near {
			return j
		}
	}
	return -1
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// MergedAssumption is one assumption as the reviewers together stated it.
type MergedAssumption struct {
	ReviewAssumption
	// Reviewers made this assumption; more than one means they share it.
	Reviewers []string `json:"reviewers"`
}

// assumptionOverlap is how much of two assumptions' wording must be shared
// (Jaccard similarity of their significant words) for them to be one.
const assumptionOverlap = 0.6

// mergeReviewerAssumptions collapses the reviewers' assumptions into one
// list: load-bearing first, then the ones more reviewers share. A merged
// assumption keeps its first reporter's wording. It is load-bearing when any
// reviewer said so, and then explains itself with that reviewer's basis and
// consequence.
func mergeReviewerAssumptions(results []ACRResult, name func(int) string) []MergedAssumption {
	var out []MergedAssumption
	var keys []assumptionKey
	for i, res := range results {
		reviewer := name(i)
		for _, a := range res.Assumptions {
			k := keyAssumption(a.Assumption)
			if j := matchAssumption(keys, k); j >= 0 {
				m := &out[j]
				if !containsFold(m.Reviewers, reviewer) {
					m.Reviewers = append(m.Reviewers, reviewer)
				}
				if a.LoadBearing && !m.LoadBearing {
					// The consequence shown must be the one that made it
					// load-bearing, not the first reporter's lesser one.
					m.LoadBearing = true
					if a.Basis != "" {
						m.Basis = a.Basis
					}
					if a.IfWrong != "" {
						m.IfWrong = a.IfWrong
					}
				}
				if m.Basis == "" {
					m.Basis = a.Basis
				}
				if m.IfWrong == "" {
					m.IfWrong = a.IfWrong
				}
				continue
			}
			out = append(out, MergedAssumption{ReviewAssumption: a, Reviewers: []string{reviewer}})
			keys = append(keys, k)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].LoadBearing != out[b].LoadBearing {
			return out[a].LoadBearing
		}
		return len(out[a].Reviewers) > len(out[b].Reviewers)
	})
	return out
}

// assumptionKey is an assumption's significant words in order, and whether
// it is negated.
type assumptionKey struct {
	words   []string
	negated bool
}

// matchAssumption finds an assumption that says the same thing: the same
// negation, mostly the same words, and the shared words in the same order.
// Order and negation matter because "the migration runs before the deploy"
// and "the deploy runs before the migration" (or "callers do not pass nil")
// share their words but disagree, and merging them would show disagreement as
// consensus. Two wordings of one assumption that this misses are shown twice,
// which is the safer error.
func matchAssumption(merged []assumptionKey, k assumptionKey) int {
	for j, m := range merged {
		if m.negated == k.negated && jaccard(m.words, k.words) >= assumptionOverlap && sameOrder(m.words, k.words) {
			return j
		}
	}
	return -1
}

// keyAssumption lower-cases an assumption and keeps its words of three or
// more letters or digits, less the most common English ones. Negations count
// toward negated rather than the words, so an even number cancel out.
func keyAssumption(s string) assumptionKey {
	var k assumptionKey
	s = strings.ReplaceAll(strings.ToLower(s), "n't", " not")
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		switch {
		case negations[w]:
			k.negated = !k.negated
		case len(w) >= 3 && !stopWords[w] && !seen[w]:
			seen[w] = true
			k.words = append(k.words, w)
		}
	}
	return k
}

var negations = map[string]bool{"not": true, "no": true, "never": true, "without": true, "cannot": true, "none": true}

var stopWords = map[string]bool{
	"the": true, "and": true, "that": true, "this": true, "are": true, "for": true, "with": true,
	"any": true, "all": true, "its": true, "from": true, "will": true, "has": true,
	"have": true, "was": true, "were": true, "been": true, "into": true, "only": true,
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	in := map[string]bool{}
	for _, w := range a {
		in[w] = true
	}
	shared := 0
	for _, w := range b {
		if in[w] {
			shared++
		}
	}
	return float64(shared) / float64(len(a)+len(b)-shared)
}

// sameOrder reports whether the words a and b share appear in the same order
// in both.
func sameOrder(a, b []string) bool {
	in := map[string]bool{}
	for _, w := range b {
		in[w] = true
	}
	var shared []string
	for _, w := range a {
		if in[w] {
			shared = append(shared, w)
		}
	}
	i := 0
	for _, w := range b {
		if i < len(shared) && w == shared[i] {
			i++
		}
	}
	return i == len(shared)
}

// verdictReason names the first gate that failed, or says every gate passed.
func verdictReason(out PullRequestReview) string {
	for _, s := range out.Stages {
		if !s.Passed {
			return fmt.Sprintf("%s: %s", s.Name, firstLine(s.Reason))
		}
	}
	if len(out.Stages) == 0 {
		return "not evaluated"
	}
	return "every gate passed"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
