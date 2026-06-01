// Command radar is the CLI front-end to the RADAR funnel (arXiv:2605.30208).
//
// Usage:
//
//	radar classify [-llm] <diff.json>     classify one diff, print decision + trace
//	radar replay   [-llm] <diffs.json>    classify a batch, print RADAR RQ metrics
//
// With -llm the Automated Code Review stage uses the Anthropic API (requires
// $ANTHROPIC_API_KEY); otherwise the deterministic rule-based ACR is used.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/travisjeffery/radar"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	switch cmd {
	case "classify":
		os.Exit(runClassify(os.Args[2:]))
	case "replay":
		os.Exit(runReplay(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "radar: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `radar — Risk Aware Diff Auto Review (arXiv:2605.30208)

Usage:
  radar classify [-llm] [-json] <diff.json>    classify one diff
  radar replay   [-llm] <diffs.json>           classify a batch, report metrics

Flags:
  -llm    use the Anthropic-backed ACR agent (needs $ANTHROPIC_API_KEY)
  -json   (classify) emit the decision trace as JSON
`)
}

// newReviewAgent selects the ACR implementation.
func newReviewAgent(useLLM bool) (radar.ReviewAgent, error) {
	if !useLLM {
		return radar.RuleBasedAgent{}, nil
	}
	return radar.NewLLMAgent()
}

func runClassify(args []string) int {
	fs := flag.NewFlagSet("classify", flag.ExitOnError)
	useLLM := fs.Bool("llm", false, "use the Anthropic-backed ACR agent")
	asJSON := fs.Bool("json", false, "emit the decision trace as JSON")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "classify: expected exactly one <diff.json> argument")
		return 2
	}

	d, err := loadDiff(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "classify:", err)
		return 1
	}
	agent, err := newReviewAgent(*useLLM)
	if err != nil {
		fmt.Fprintln(os.Stderr, "classify:", err)
		return 1
	}

	eng := radar.NewEngine(
		radar.WithReviewAgent(agent),
		radar.WithRunbooks(defaultRunbooks),
		radar.WithCalibrator(radar.NewCalibrator(radar.DefaultCalibrationSample())),
	)
	trace := eng.Classify(d)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(trace)
		return 0
	}
	printTrace(trace)
	return 0
}

func runReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	useLLM := fs.Bool("llm", false, "use the Anthropic-backed ACR agent")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "replay: expected exactly one <diffs.json> argument")
		return 2
	}

	diffs, err := loadDiffs(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		return 1
	}
	agent, err := newReviewAgent(*useLLM)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		return 1
	}

	// Calibrate DRS against the population of diffs being processed.
	eng := radar.NewEngine(
		radar.WithReviewAgent(agent),
		radar.WithRunbooks(defaultRunbooks),
		radar.WithCalibrator(radar.NewCalibratorFromDiffs(radar.HeuristicScorer{}, diffs)),
	)

	var m metrics
	m.bySource = map[string]*sourceMetrics{}
	orgs := map[string]bool{}
	for _, d := range diffs {
		t := eng.Classify(d)
		m.record(d, t)
		orgs[d.Org] = true
	}
	m.orgs = len(orgs)
	m.print()
	return 0
}

// --- metrics (RQ1/RQ2 definitions, Tables 3–5) ---

type sourceMetrics struct {
	reviewed int
	landed   int
}

type metrics struct {
	reviewed         int
	landed           int
	blanket          int
	autoLand         int
	verificationPass int
	radarApproved    int
	routeToHuman     int
	notEligible      int
	orgs             int
	bySource         map[string]*sourceMetrics
}

func (m *metrics) record(d radar.Diff, t *radar.DecisionTrace) {
	m.reviewed++
	sm := m.bySource[d.Source.String()]
	if sm == nil {
		sm = &sourceMetrics{}
		m.bySource[d.Source.String()] = sm
	}
	sm.reviewed++
	if t.Decision.Landed() {
		m.landed++
		sm.landed++
	}
	switch t.Decision {
	case radar.DecisionBlanketAutoAccept:
		m.blanket++
	case radar.DecisionAutoLand:
		m.autoLand++
	case radar.DecisionVerificationPassed:
		m.verificationPass++
	case radar.DecisionRADARApproved:
		m.radarApproved++
	case radar.DecisionRouteToHuman:
		m.routeToHuman++
	case radar.DecisionNotEligible:
		m.notEligible++
	}
}

func (m *metrics) print() {
	pct := func(n int) float64 {
		if m.reviewed == 0 {
			return 0
		}
		return float64(n) / float64(m.reviewed) * 100
	}
	approved := m.blanket + m.autoLand + m.radarApproved + m.verificationPass

	fmt.Println("RADAR replay — operational metrics")
	fmt.Println("===================================")
	fmt.Printf("RADAR reviewed diffs : %d\n", m.reviewed)
	fmt.Printf("RADAR landed diffs   : %d (%.1f%%)\n", m.landed, pct(m.landed))
	fmt.Printf("Coverage (orgs)      : %d\n", m.orgs)
	fmt.Println()
	fmt.Printf("Approve rate         : %.1f%% (%d eligible-approved)\n", pct(approved), approved)
	fmt.Printf("Verification pass    : %.1f%% (%d)\n", pct(m.verificationPass), m.verificationPass)
	fmt.Println()
	fmt.Println("Decision breakdown")
	fmt.Printf("  blanket-auto-accept: %d\n", m.blanket)
	fmt.Printf("  auto-land          : %d\n", m.autoLand)
	fmt.Printf("  verification-passed: %d\n", m.verificationPass)
	fmt.Printf("  radar-approved     : %d\n", m.radarApproved)
	fmt.Printf("  route-to-human     : %d\n", m.routeToHuman)
	fmt.Printf("  not-eligible       : %d\n", m.notEligible)
	fmt.Println()
	fmt.Println("By source type (reviewed / landed)")
	srcs := make([]string, 0, len(m.bySource))
	for s := range m.bySource {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, s := range srcs {
		sm := m.bySource[s]
		fmt.Printf("  %-22s %d / %d\n", s, sm.reviewed, sm.landed)
	}
}

// --- input loading ---

func loadDiff(path string) (radar.Diff, error) {
	var d radar.Diff
	data, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("parsing %s: %w", path, err)
	}
	return d, nil
}

func loadDiffs(path string) ([]radar.Diff, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var diffs []radar.Diff
	if err := json.Unmarshal(data, &diffs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return diffs, nil
}

func printTrace(t *radar.DecisionTrace) {
	fmt.Printf("diff %s\n", t.DiffID)
	fmt.Printf("decision: %s  (path: %s)\n", t.Decision, t.Path)
	if t.DRSPercentile >= 0 {
		fmt.Printf("DRS percentile: %.1f\n", t.DRSPercentile)
	}
	fmt.Println("stages:")
	for _, s := range t.Stages {
		mark := "✓"
		if !s.Passed {
			mark = "✗"
		}
		fmt.Printf("  %s %-26s %s\n", mark, s.Name, s.Reason)
	}
}
