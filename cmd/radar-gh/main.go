// Command radar-gh fetches real pull requests from a GitHub repository (via the
// `gh` CLI) and runs them through the RADAR funnel, reporting a per-PR decision
// and aggregate metrics. It is a way to exercise RADAR — including the LLM-backed
// Automated Code Review agent — against real diffs.
//
// Usage:
//
//	radar-gh -repo OWNER/REPO -limit 15 [-llm] [-state all] [-human-drs 5]
//
// With -llm the ACR uses the OpenAI API ($OPENAI_API_KEY); otherwise the
// deterministic rule-based ACR is used (which, lacking signal tags on real
// diffs, will conservatively route most PRs to humans — the LLM path is the
// interesting one here).
//
// NOTE: GitHub PRs do not carry Meta's author-eligibility attributes (SWE role,
// oncall ownership, etc.). To let PRs reach the ACR/DRS stages, authors are
// treated as eligible; the funnel then decides on content + risk, which is the
// part worth testing against real code. CI and lifecycle state are real: each
// PR's check rollup maps to the CI signal (no checks / pending → not green) and
// PRs closed without merging are treated as rejected.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	"github.com/travisjeffery/radar"
)

// perFileCap caps how many characters of each file's diff are sent to the ACR,
// to keep token usage bounded. maxFiles caps files per PR.
const (
	perFileCap = 4000
	maxFiles   = 30
)

type ghPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
	ChangedFile int    `json:"changedFiles"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "review" {
		os.Exit(runReview(os.Args[2:]))
	}
	os.Exit(runReplay(os.Args[1:]))
}

func runReplay(args []string) int {
	flags := flag.NewFlagSet("radar-gh", flag.ContinueOnError)
	repo := flags.String("repo", "", "owner/repo to pull PRs from (required)")
	limit := flags.Int("limit", 15, "number of PRs to classify")
	state := flags.String("state", "all", "PR state filter: open, closed, merged, all")
	useLLM := flags.Bool("llm", false, "use the OpenAI-backed ACR agent ($OPENAI_API_KEY)")
	humanDRS := flags.Float64("human-drs", radar.DRSHumanDefault, "human-diff DRS percentile threshold (paper default P5)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "radar-gh: -repo OWNER/REPO is required")
		return 2
	}

	agent, agentName, err := newAgent(*useLLM)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh:", err)
		return 1
	}

	prs, err := listPRs(*repo, *state, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh: listing PRs:", err)
		return 1
	}
	if len(prs) == 0 {
		fmt.Fprintln(os.Stderr, "radar-gh: no PRs found")
		return 1
	}

	// Build diffs first so DRS can be calibrated across the batch.
	var diffs []radar.Diff
	meta := map[string]ghPR{}
	for _, pr := range prs {
		raw, err := prDiff(*repo, pr.Number)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  skipping PR #%d (diff fetch failed: %v)\n", pr.Number, err)
			continue
		}
		d := toDiff(*repo, pr, raw, prCI(*repo, pr.Number))
		diffs = append(diffs, d)
		meta[d.ID] = pr
	}
	if len(diffs) == 0 {
		fmt.Fprintln(os.Stderr, "radar-gh: no diffs could be fetched")
		return 1
	}

	policy := radar.DefaultPolicy()
	policy.HumanDRSThreshold = *humanDRS
	eng := radar.NewEngine(
		radar.WithReviewAgent(agent),
		radar.WithCalibrator(radar.NewCalibratorFromDiffs(radar.HeuristicScorer{}, diffs)),
		radar.WithPolicy(policy),
	)

	fmt.Printf("RADAR over %d PRs from %s (ACR: %s, human DRS threshold P%.0f)\n", len(diffs), *repo, agentName, *humanDRS)
	fmt.Println(strings.Repeat("=", 78))

	var m metrics
	for _, d := range diffs {
		t := eng.Classify(d)
		m.record(t)
		pr := meta[d.ID]
		fmt.Printf("\nPR #%-5d %s\n", pr.Number, truncate(pr.Title, 64))
		fmt.Printf("  files=%d +%d/-%d  by @%s [%s]\n", pr.ChangedFile, pr.Additions, pr.Deletions, pr.Author.Login, pr.State)
		fmt.Printf("  decision: %-20s DRS=%s\n", t.Decision, drsStr(t.DRSPercentile))
		fmt.Printf("  %s\n", acrStageReason(t))
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 78))
	m.print()
	return 0
}

func newAgent(useLLM bool) (radar.ReviewAgent, string, error) {
	if !useLLM {
		return radar.RuleBasedAgent{}, "rule-based", nil
	}
	a, err := radar.NewOpenAIAgent()
	if err != nil {
		return nil, "", err
	}
	return a, "openai/" + a.Model, nil
}

// --- gh integration ---

func listPRs(repo, state string, limit int) ([]ghPR, error) {
	out, err := runGH("pr", "list", "--repo", repo, "--state", state, "--limit", fmt.Sprint(limit),
		"--json", "number,title,state,additions,deletions,changedFiles,author")
	if err != nil {
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parsing pr list: %w", err)
	}
	return prs, nil
}

func prDiff(repo string, number int) (string, error) {
	out, err := runGH("pr", "diff", fmt.Sprint(number), "--repo", repo)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// prCI fetches the PR's check rollup and maps it to a CISignal: any failed
// check is CIFailing, all-green is CIPassing, and no checks / pending checks /
// a failed fetch are CIUnknown (not green, so the state gate fails closed).
func prCI(repo string, number int) radar.CISignal {
	out, err := runGH("pr", "view", fmt.Sprint(number), "--repo", repo, "--json", "statusCheckRollup")
	if err != nil {
		return radar.CIUnknown
	}
	var v struct {
		StatusCheckRollup []struct {
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &v); err != nil || len(v.StatusCheckRollup) == 0 {
		return radar.CIUnknown
	}
	ci := radar.CIPassing
	for _, c := range v.StatusCheckRollup {
		s := c.Conclusion
		if s == "" {
			s = c.State
		}
		switch strings.ToUpper(s) {
		case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED":
			return radar.CIFailing
		case "SUCCESS", "NEUTRAL", "SKIPPED", "EXPECTED":
		default: // pending or unrecognized: not green
			ci = radar.CIUnknown
		}
	}
	return ci
}

func runGH(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// --- diff conversion ---

// toDiff converts a GitHub PR and its unified diff into a radar.Diff: human
// source with the PR's real CI signal and lifecycle state; only author
// eligibility is fabricated (see package note). One Change per file.
func toDiff(repo string, pr ghPR, raw string, ci radar.CISignal) radar.Diff {
	state := radar.DiffPublished
	if pr.State == "CLOSED" {
		// Closed without merging (gh reports merged PRs as MERGED): the diff
		// was effectively rejected.
		state = radar.DiffRejected
	}
	return radar.Diff{
		ID:     fmt.Sprintf("PR#%d", pr.Number),
		Org:    repo,
		Source: radar.SourceHuman,
		CI:     ci,
		State:  state,
		Author: radar.Author{
			Name:               pr.Author.Login,
			EligibleRole:       true,
			HasOncallOwnership: true,
			TenureDays:         900,
			DiffsLastYear:      100,
		},
		Changes: splitUnifiedDiff(raw),
	}
}

// splitUnifiedDiff breaks a unified diff into per-file Changes. Each file's hunk
// text is truncated to perFileCap; at most maxFiles files are kept.
func splitUnifiedDiff(raw string) []radar.Change {
	lines := strings.Split(raw, "\n")
	var changes []radar.Change
	var cur *radar.Change
	flush := func() {
		if cur != nil {
			if len(cur.Content) > perFileCap {
				cur.Content = cut(cur.Content, perFileCap) + "\n...[truncated]"
			}
			changes = append(changes, *cur)
			cur = nil
		}
	}
	for _, ln := range lines {
		if strings.HasPrefix(ln, "diff --git ") {
			flush()
			if len(changes) >= maxFiles {
				break
			}
			cur = &radar.Change{File: parseGitFile(ln)}
			continue
		}
		if cur != nil {
			cur.Content += ln + "\n"
		}
	}
	flush()
	if len(changes) == 0 {
		// No recognizable file headers; keep the whole diff as one change.
		c := raw
		if len(c) > perFileCap {
			c = cut(c, perFileCap) + "\n...[truncated]"
		}
		changes = []radar.Change{{File: "(whole-diff)", Content: c}}
	}
	return changes
}

// parseGitFile extracts the b/ path from a "diff --git a/x b/y" header line.
// Paths may contain spaces (and git quotes paths with special characters), so
// the header is split on the last " b/" marker rather than on whitespace —
// otherwise a path like ".github/workflows/ci cd.yml" would parse as "cd.yml"
// and evade the path blocklist.
func parseGitFile(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if i := strings.LastIndex(rest, ` "b/`); i >= 0 {
		return strings.TrimSuffix(rest[i+len(` "b/`):], `"`)
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+len(" b/"):]
	}
	return "(unknown)"
}

// cut returns s truncated to at most n bytes without splitting a UTF-8 rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return cut(s, n-1) + "…"
}

func drsStr(p float64) string {
	if p < 0 {
		return "n/a"
	}
	return fmt.Sprintf("P%.0f", p)
}

// acrStageReason returns the reason of whichever ACR stage ran (verify.acr or
// ace.acr), or the last failing stage if the ACR was never reached.
func acrStageReason(t *radar.DecisionTrace) string {
	for _, s := range t.Stages {
		if s.Name == "verify.acr" || s.Name == "ace.acr" {
			return s.Reason
		}
	}
	// Surface the first failing stage so the routing is explained.
	for _, s := range t.Stages {
		if !s.Passed {
			return fmt.Sprintf("[%s] %s", s.Name, s.Reason)
		}
	}
	return ""
}

// --- metrics ---

type metrics struct {
	total                                                            int
	blanket, autoLand, verifyPass, approved, routeHuman, notEligible int
}

func (m *metrics) record(t *radar.DecisionTrace) {
	m.total++
	switch t.Decision {
	case radar.DecisionBlanketAutoAccept:
		m.blanket++
	case radar.DecisionAutoLand:
		m.autoLand++
	case radar.DecisionVerificationPassed:
		m.verifyPass++
	case radar.DecisionRADARApproved:
		m.approved++
	case radar.DecisionRouteToHuman:
		m.routeHuman++
	case radar.DecisionNotEligible:
		m.notEligible++
	}
}

func (m *metrics) print() {
	pct := func(n int) float64 {
		if m.total == 0 {
			return 0
		}
		return float64(n) / float64(m.total) * 100
	}
	automated := m.blanket + m.autoLand + m.verifyPass + m.approved
	fmt.Println("Summary")
	fmt.Printf("  PRs classified     : %d\n", m.total)
	fmt.Printf("  Automated by RADAR : %d (%.1f%%)\n", automated, pct(automated))
	fmt.Printf("  Routed to human    : %d (%.1f%%)\n", m.routeHuman+m.notEligible, pct(m.routeHuman+m.notEligible))
	fmt.Println("  Breakdown:")
	fmt.Printf("    radar-approved      %d\n", m.approved)
	fmt.Printf("    verification-passed %d\n", m.verifyPass)
	fmt.Printf("    auto-land           %d\n", m.autoLand)
	fmt.Printf("    blanket-auto-accept %d\n", m.blanket)
	fmt.Printf("    route-to-human      %d\n", m.routeHuman)
	fmt.Printf("    not-eligible        %d\n", m.notEligible)
}
