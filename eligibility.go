package radar

import (
	"regexp"
	"strings"
)

// blocklistedPhrases are content phrases that, if present in a change, exclude a
// diff from automation (paper §2.8, §2.9 content-level checks). These mark
// changes that should always receive human eyes. They are matched as standalone
// words/phrases, not substrings (see blocklistedPhraseREs).
var blocklistedPhrases = []string{
	"do not merge",
	"do not land",
	"wip",
	"hack",
	"fixme",
	"security review required",
}

// blocklistedPhraseREs match each phrase only when it is not embedded in a
// larger word, identifier, or path: the adjacent character may not be a letter,
// digit, or identifier/path punctuation. Marker usage still matches ("WIP:",
// "// hack to work around"), while "wiped", "hackathon", and paths like
// "deployer/hack/upgrade.sh" do not.
var blocklistedPhraseREs = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(blocklistedPhrases))
	for i, p := range blocklistedPhrases {
		res[i] = regexp.MustCompile(`(?:^|[^a-z0-9_/.\-])` + regexp.QuoteMeta(p) + `(?:[^a-z0-9_/.\-]|$)`)
	}
	return res
}()

// blocklistedPathFragments are path fragments that require human review when a
// diff touches them (paper §2.9 file suffix/prefix blocklists). Distinct from
// the scope exclusions (open-source / SOX), these are configurable per-org.
var blocklistedPathFragments = []string{
	".github/workflows",
	"infra/prod",
}

// contentChecks verifies the diff contains no blocklisted phrases. It returns
// ok=false and the offending phrase otherwise.
func contentChecks(d Diff) (ok bool, reason string) {
	for _, c := range d.Changes {
		lower := strings.ToLower(c.Content)
		for i, re := range blocklistedPhraseREs {
			if re.MatchString(lower) {
				return false, "content matches blocklisted phrase: " + blocklistedPhrases[i]
			}
		}
	}
	return true, "no blocklisted content"
}

// pathChecks verifies the diff touches no blocklisted paths.
func pathChecks(d Diff) (ok bool, reason string) {
	for _, c := range d.Changes {
		lower := strings.ToLower(c.File)
		for _, frag := range blocklistedPathFragments {
			if strings.Contains(lower, frag) {
				return false, "path matches blocklist: " + frag
			}
		}
	}
	return true, "no blocklisted paths"
}

// scopeChecks applies the scope exclusions shared by every pipeline: diffs that
// touch open-source code, SOX-scoped code, or require additional reviews are
// excluded from automated review (paper §2.7.3, §2.8).
func scopeChecks(d Diff) (ok bool, reason string) {
	switch {
	case d.OpenSource:
		return false, "diff touches open-source code"
	case d.SOXScoped:
		return false, "diff touches SOX-scoped code"
	case d.RequiresAdditionalReviews:
		return false, "diff requires additional reviews"
	}
	return true, "scope ok"
}

// stateChecks verifies the diff is in an automatable lifecycle state with
// passing CI (paper §2.9): the latest published version, not WIP/RFC/rejected,
// CI green.
func stateChecks(d Diff) (ok bool, reason string) {
	switch d.State {
	case DiffWorkInProgress:
		return false, "diff is work-in-progress"
	case DiffRequestForComment:
		return false, "diff is a request-for-comment"
	case DiffRejected:
		return false, "diff was previously rejected"
	}
	if d.CI != CIPassing {
		return false, "CI signal not in allowed (passing) state"
	}
	return true, "diff state and CI ok"
}

// authorEligibility applies the human author-eligibility criteria (paper §2.9):
// an eligible role, oncall ownership, sufficient intern tenure, and more than
// MinDiffsLastYear landed diffs in the past year.
func authorEligibility(a Author) (ok bool, reason string) {
	if !a.EligibleRole {
		return false, "author lacks eligible role"
	}
	if !a.HasOncallOwnership {
		return false, "author lacks oncall ownership"
	}
	if a.IsIntern && a.TenureDays < InternMinTenureDays {
		return false, "intern below minimum tenure"
	}
	if a.DiffsLastYear <= MinDiffsLastYear {
		return false, "author has too few diffs in the past year"
	}
	return true, "author eligible"
}
