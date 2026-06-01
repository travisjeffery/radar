# radar

A Go implementation of **RADAR (Risk Aware Diff Auto Review)**, the code-review
automation funnel from *"Automating Low-Risk Code Review at Meta: RADAR, Risk
Calibration, and Review Efficiency"* ([arXiv:2605.30208](https://arxiv.org/pdf/2605.30208)).

RADAR classifies every code-review diff and decides whether it can be
auto-approved / auto-landed without human review (when it is genuinely low-risk)
or must be routed to a human reviewer. This package implements that funnel as a
small, dependency-free library plus a CLI.

## The funnel

`Engine.Classify(Diff)` routes a diff through the layered funnel and returns a
`DecisionTrace` recording the final decision and every gate evaluated (so the
routing is fully auditable, mirroring Figures 2–4 of the paper).

```
                          ┌─ human ──────────► RADAR Verification → RADAR Approval
New Diff ─ authorship ─┤                        (Fig. 4: P5 DRS, ACR, stricter waiver)
                          └─ bot ─ source? ─┬─ deterministic codemod → Blanket AutoAccept
                                            ├─ AI codemod ──────────► ACE pipeline
                                            └─ RACER runbook ─ per-runbook ─► ACE pipeline
                                                              eligibility
```

**ACE pipeline** (bot diffs, Fig. 3) — three layers, all must pass:

1. **Static heuristics** — scope (no open-source/SOX/extra-review), CI/state,
   content blocklists, path blocklists.
2. **DRS threshold** — the diff's Diff Risk Score percentile must be ≤ the
   source's threshold (P50 allowlisted, P20 default).
3. **ACR** — the Automated Code Review agent requires confidence ≥ 8/10 and
   **zero** risk signals.

Pass all → **auto-land** (after a configurable delay); any fail → **route to human**.

**RADAR Verification + Approval** (human diffs, Fig. 4) — two steps: Verification
gates on eligibility + content + ACR + DRS (P5 default) and, if it passes, the
author ships with *deferred* review (`verification-passed`); Approval applies a
stricter DRS threshold and maximal ACR confidence to waive human review entirely
(`radar-approved`).

### Decisions

`blanket-auto-accept`, `auto-land`, `verification-passed`, `radar-approved`,
`route-to-human`, `not-eligible`.

## Pluggable components

The two pieces that are proprietary/ML at Meta are interfaces here:

- **`RiskScorer`** — the Diff Risk Score (DRS). `HeuristicScorer` is a
  transparent stand-in (size, complexity, risky paths, risk signals); a
  `Calibrator` maps raw scores to percentiles via an empirical CDF, so a
  threshold `PX` means "only the safest X% qualify" (paper §2.3).
- **`ReviewAgent`** — the Automated Code Review (ACR). `RuleBasedAgent` (default,
  offline, deterministic) classifies the paper's safe/risk signal taxonomy from
  structured change tags. `LLMAgent` (Anthropic) and `OpenAIAgent` (OpenAI) are
  optional API-backed adapters behind the same interface; both fail safe (route
  to human) on any error.

Per-org risk appetite (`OrgPolicyConfig`, modeling `OrgRADARPolicyConfig`) and
per-runbook eligibility (60-day risk history, daily limits, denylist) are
configurable, matching Table 1.

## CLI

```
go run ./cmd/radar classify [-llm] [-json] testdata/human_approved.json
go run ./cmd/radar replay   [-llm]         testdata/diffs.json
```

`classify` prints the decision and the stage-by-stage trace for one diff.
`replay` streams a batch and reports the paper's RQ metrics (reviewed/landed
counts, approve rate, verification pass rate, coverage, per-source breakdown).
With `-llm` the ACR uses the Anthropic API (`$ANTHROPIC_API_KEY`,
optional `$RADAR_ACR_MODEL`); otherwise the deterministic rule-based ACR is used.

See `testdata/` for fixtures exercising every decision path.

### Running against real GitHub PRs

`cmd/radar-gh` pulls real pull requests (via the `gh` CLI) and runs them through
the funnel — a way to exercise the LLM ACR against real diffs:

```
radar-gh -repo OWNER/REPO -limit 15 -llm [-human-drs 5] [-state all]
```

PRs are treated as eligible human-authored diffs (GitHub PRs lack Meta's
author-eligibility attributes), so the decision turns on content risk: the LLM
ACR classifies each diff and the DRS threshold gates on calibrated risk.
`-human-drs` overrides the human DRS percentile threshold (paper default P5) for
exploration. With `-llm` the ACR uses the OpenAI API (`$OPENAI_API_KEY`,
optional `$RADAR_ACR_MODEL`, default `gpt-4o-mini`).

## Scope / non-goals

- This reproduces RADAR's **decision logic and metric definitions**, not Meta's
  empirical telemetry (the 535K+ diffs, revert/PI rates — those are
  observational and not reproducible).
- `HeuristicScorer` is a faithful *interface* stand-in for Meta's proprietary DRS
  model, not a retrained equivalent.
- Zero third-party dependencies; the LLM adapter uses only `net/http` and
  `encoding/json`.

## Tests

```
go test ./...
```
