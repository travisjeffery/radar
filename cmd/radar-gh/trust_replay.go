package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/travisjeffery/radar"
)

// trustReplayRecord is one historical review: the provider input, the stored
// decision whose agent verdict and raw risk score are replayed, and the
// observed outcome ("bad", "good", or empty when unknown).
type trustReplayRecord struct {
	Input          radar.PullRequestInput  `json:"input"`
	Review         radar.PullRequestReview `json:"review"`
	CommitMessages []string                `json:"commit_messages,omitempty"`
	Outcome        string                  `json:"outcome,omitempty"`
}

type outcomeCount struct {
	Approvals int `json:"approvals"`
	Observed  int `json:"observed"`
	Bad       int `json:"bad"`
}

func (c *outcomeCount) add(outcome string) {
	c.Approvals++
	switch outcome {
	case "bad":
		c.Observed++
		c.Bad++
	case "good":
		c.Observed++
	}
}

type trustReplayReport struct {
	Records        int                  `json:"records"`
	Replayed       int                  `json:"replayed"`
	LiveMismatches int                  `json:"live_mismatches"`
	Background     outcomeCount         `json:"background"`
	LiveApprovals  outcomeCount         `json:"live_approvals"`
	TierOnly       outcomeCount         `json:"tier_only_approvals"`
	ByTier         map[int]outcomeCount `json:"tier_only_by_effective_tier"`
}

type replayedAgent struct {
	name   string
	result radar.ACRResult
}

func (a replayedAgent) Review(radar.Diff) radar.ACRResult { return a.result }

func (a replayedAgent) Describe() string { return a.name }

// replayedAgents rebuilds one agent per stored reviewer verdict, keeping
// each reviewer's name so per-reviewer confidence floors still apply.
func replayedAgents(review radar.PullRequestReview) []radar.ReviewAgent {
	if len(review.Reviews) == 0 {
		return []radar.ReviewAgent{replayedAgent{review.Reviewer, review.Agent}}
	}
	agents := make([]radar.ReviewAgent, 0, len(review.Reviews))
	for _, v := range review.Reviews {
		agents = append(agents, replayedAgent{v.Reviewer, v.Agent})
	}
	return agents
}

type replayedScorer float64

func (s replayedScorer) Score(radar.Diff) float64 { return float64(s) }

func runTrustReplay(args []string) int {
	flags := flag.NewFlagSet("radar-gh trust-replay", flag.ContinueOnError)
	policyPath := flags.String("policy", "", "policy with an author_trust block (required)")
	trustPath := flags.String("author-trust", "", "private login-to-tier roster (required)")
	recordsPath := flags.String("records", "", "JSON Lines of {input, review, commit_messages, outcome} (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *policyPath == "" || *trustPath == "" || *recordsPath == "" {
		fmt.Fprintln(os.Stderr, "radar-gh trust-replay: -policy, -author-trust, and -records are required")
		return 2
	}
	policy, err := loadPullRequestPolicy(*policyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh trust-replay:", err)
		return 1
	}
	data, err := os.ReadFile(*trustPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh trust-replay: reading roster:", err)
		return 1
	}
	roster, err := radar.ParseAuthorTrustRoster(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh trust-replay:", err)
		return 1
	}
	f, err := os.Open(*recordsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh trust-replay:", err)
		return 1
	}
	defer f.Close()
	report, err := trustReplay(policy, roster, f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh trust-replay:", err)
		return 1
	}
	if err := writeJSON(os.Stdout, report); err != nil {
		fmt.Fprintln(os.Stderr, "radar-gh trust-replay:", err)
		return 1
	}
	return 0
}

// trustReplay re-decides each record with and without the author's tier from
// its stored verdicts, so a replay costs no model calls. A review that was
// split into chunks replays from its merged verdict, so live_mismatches counts
// records whose replayed live decision differs from the stored one. Records the live
// reviewer never reached the agent for are counted but not replayed.
func trustReplay(policy radar.PullRequestPolicy, roster radar.AuthorTrustRoster, r io.Reader) (trustReplayReport, error) {
	report := trustReplayReport{ByTier: map[int]outcomeCount{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var rec trustReplayRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return report, fmt.Errorf("record %d: %w", line, err)
		}
		report.Records++
		if !hasStage(rec.Review, "pr.review-agent") {
			continue
		}
		reviewer, err := radar.NewPullRequestReviewer(policy, replayedScorer(rec.Review.RawRiskScore), replayedAgents(rec.Review)...)
		if err != nil {
			return report, err
		}
		trust := radar.AuthorTrust{AgentAuthored: radar.AgentAuthored(rec.Input.Body, rec.CommitMessages)}
		if strings.EqualFold(rec.Input.AuthorType, "User") {
			trust.Tier = roster.Tier(rec.Input.Author)
		}
		live, audit := reviewer.ReviewWithAuthorTrust(rec.Input, trust)
		report.Replayed++
		if live.Action != rec.Review.Action {
			report.LiveMismatches++
		}
		if rec.Outcome == "bad" || rec.Outcome == "good" {
			report.Background.Observed++
			if rec.Outcome == "bad" {
				report.Background.Bad++
			}
		}
		switch {
		case audit.LiveAction == radar.PullRequestWouldApprove:
			report.LiveApprovals.add(rec.Outcome)
		case audit.TierAction == radar.PullRequestWouldApprove:
			report.TierOnly.add(rec.Outcome)
			c := report.ByTier[audit.EffectiveTier]
			c.add(rec.Outcome)
			report.ByTier[audit.EffectiveTier] = c
		}
	}
	return report, scanner.Err()
}

func hasStage(review radar.PullRequestReview, name string) bool {
	for _, s := range review.Stages {
		if s.Name == name {
			return true
		}
	}
	return false
}
