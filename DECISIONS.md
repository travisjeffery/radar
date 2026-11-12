# Decisions

Policy decisions behind Radar's review-findings basis, newest first, with the
reasoning behind each.

## Docs and tests pass at confidence 7, and advisory lock bumps without a manifest (2026-11-11)

A confidence floor of 8 sends low-risk changes to a human for no gain: documentation and test-only changes often score 7, and a lockfile changed without its manifest, as in a security bump of a transitive package, always routes.

Both reliefs are per repository and off by default:

- **`low_risk_paths` and `low_risk_min_review_confidence`:** the floor drops by at most one point, only when every changed path matches a low-risk glob exactly. The globs match case exactly, and a rename source must match too, so code renamed into `docs/` keeps the full floor. The rest of the bar still applies: no blocking finding, no defect signal, no structural change, no call for a human. Deny paths and area policy still route first.
- **`lockfiles.allow_advisory_upgrade`:** a lockfile without a manifest change passes the backstop only when:
  - every change is an upgrade of a package already locked;
  - Radar ties every changed line to a package;
  - the title or description names each upgraded package and cites a GHSA or CVE identifier.

  Source changes still route to a human, and the reviewers still check the lines. The review prompt's P1 for a lockfile without a manifest makes the same exception.

The citation is text the author writes, so it is not proof of an advisory. What bounds the risk is that only upgrades of packages already locked pass, and each one must be named.

## Deny paths and area paths ignore case (2026-11-11)

Path globs matched case exactly, so `**/*iam*` and `**/*secret*` missed `IAMPolicy.java` and `SECRETS.yaml`, and repositories had to list each casing (`**/*Secret*`, `**/*KMS*`). Deny paths and area `paths` now also match with the path and the glob both lowercased. That only adds matches, so more pull requests go to a human and none fewer.

Every other glob keeps the exact match: area `exclude`, allow rules, generated files and base branches. Folding them would let a case difference take a file out of a guard or into a withheld or allowed set.

## On Windows, Codex reads the checkout through Radar's tools, not a shell (2026-10-31)

On Windows, `codex exec --sandbox read-only` with `approval_policy = "never"` runs no command: with no Windows sandbox backend configured, Codex 0.161 rejects every shell command as "blocked by policy". Local `review-commit` runs on Windows therefore reviewed the patches only (`repo_access: false`).

Codex's own switch, `[windows] sandbox = "unelevated"` (a restricted token), runs the commands but would break "The hosted review isolates Codex from every credential" on a laptop. Measured: a sandboxed command listed `%USERPROFILE%\.aws\sso\cache`, and `curl --noproxy '*' http://example.com` returned 200, because that backend blocks the network only through the proxy variables it sets. Codex refuses read confinement and deny-read rules without its elevated backend. That backend needs a one-time administrator setup whose secrets live in a persistent CODEX_HOME, and Radar gives Codex a new one on every run.

So on Windows radar-gh turns Codex's shell tools off and serves it the bedrock and fireworks agents' repository tools (`read_file`, `grep`, `list_dir`, `git_log`): confined to the checkout, `.git` off limits, nothing written, no network. They come from `radar-gh repo-tools-mcp`, an MCP server. Codex 0.161 offers MCP tools only through tool search, so the prompt tells it to load them. Linux and macOS are unchanged: there Codex's sandbox confines writes and the network, and CI keeps the shell.

## Radar keeps its own approval current; human approvals stay (2026-10-29)

A Radar approval must not carry over to code pushed after it. Dismissing approvals on push would also dismiss human approvals, so Radar keeps only its own approval current instead:

- The approver App posts the `radar/approval-current` commit status on every head. It succeeds once Radar has approved that head, or when Radar holds no approval of an earlier head.
- A new head has no status until Radar posts one, so merge waits.
- When the new head's verdict doesn't approve, Radar dismisses only its own approvals.
- A ruleset requires the status, pinned to the App, in the repositories Radar approves in.

The approver's key is an environment secret that only the default branch can deploy. It is used by a separate job that runs `radar-gh apply` and has no OIDC token, so the reviewers' cloud trust is unchanged. An organization secret would reach every same-repository pull request's workflows, which could use it to approve, or to post the status. The job that runs the reviewers only decides and writes the approval request.

A small push is re-reviewed through the existing reuse: unchanged review requests reuse the approved head's reviews, and changed ones are re-reviewed in full. No delta-only review is used: a delta-only review recalled far fewer findings on push-changed lines than a full review.

## The hosted review can cover a whole organization (2026-10-22)

Amends "The hosted review isolates Codex from every credential". That rule
still holds: the allowlist may contain only repositories every engineer can
already read. An `owner/*` entry admits every repository in the
organization, and it rests on one fact: the organization's base permission
(`default_repository_permission`) is `read`, so every member can read every
repository.

The server trusts the config on this point. It does not check the base
permission live, and no scheduled job checks it either. Reading it would need
the GitHub App to hold Organization administration: read, and the App keeps
Contents and Metadata read only. If the base permission is ever lowered, the
wildcard entry must come out of the config the same day, and an organization
owner decides whether to add a live check before restoring it.

Wildcard repositories must be in the same organization by GitHub's own name,
readable by the App and not archived. Most have no Radar CI and no policy, so
they are reviewed under Radar's built-in default policy, with the wildcard
entry's reviewers. Results record `policy_source`. A repository that adds
`.github/radar-policy.json` is reviewed under its own policy.

Each replica still runs one review (credential isolation), so capacity comes
from replicas, autoscaled on `radar_mcp_reviews_in_flight`. Scale-down uses
the same drain as a rollout, so a running review gets
`drain_timeout_seconds` to finish. Two things bound spend:

- **Replica cap.** At most as many reviews run at once as there are replicas.
- **Per-person limits.** These are counted per replica (`limits.go`), so a
  person can run up to one review on each replica. They stop a runaway
  client but do not cap a person across the fleet.

## A push of only documentation or generated files keeps the review (2026-11-03)

This was first scoped to documentation, lockfiles and generated files, since
later rounds often change only such files since the last reviewed head.
Pushes that
change only comments or formatting in code were left out: telling them apart
takes a parser per language, and a comment can change what a reviewer
should conclude.

Lockfiles are left out too. The lockfile backstops catch only a source change and a
lockfile changed without its manifest; a hash-only change, a downgrade or a
typosquat is caught only by a reviewer reading the lockfile summary, so a
lockfile push is always reviewed, for every dependency lockfile Radar knows
(those it summarizes, a fixed list, and any `*.lock`, `*.lock.*` or
`*.lockfile` name), whatever the policy calls it. MDX is left out, generated
or not, because it can import and run code. Agent and review guidance is
left out because, once merged, it steers every later review. Tools keep
adding places for it, so the rule is structural rather than a list of
tools: every dotfile and every path under a dot-directory (`.github/`,
`.claude/`, `.agents/`, ...), the well-known names outside them (`AGENTS`,
`AGENT`, `CLAUDE`, `GEMINI`, `CONVENTIONS`, `SKILL`, with override and local
variants), the suffixes `.instructions.md`, `.prompt.md`, `.chatmode.md` and
`.agent.md`, and the files the review rules name as guidance. A policy's
`never_carry_paths` globs name any other file that must always be reviewed:
Markdown a program reads at run time, or the review packs in the repository
that hosts them (Radar does not know where a repository keeps its packs).
None of these files is blanked or dropped from the key either, whether shown
or withheld, so a change to one that the push comparison does not show, as
after a retargeted base, is reviewed.

Each recorded review now also keeps `SubstanceKey`: `RequestKey` with the
contents of documentation files (`.md`, `.markdown`, `.rst`, `.adoc`)
blanked and without the withheld generated files other than lockfiles.
Everything else stays as sent, hunk headers included, so a code patch that
moved is not carried; the moved-lines rule moves those, with its survival proof. File
names stay in the key, because a review must name exactly the files its
request shows, so a push that adds or removes a documentation file is
reviewed anew. A request whose only changes are documentation has no key.

A request reuses an earlier review on `SubstanceKey` only when the complete
comparison from that review's head shows the push changed nothing but
documentation and generated paths, from and to for a rename. A review that
names a file the push changed, under its new or old name, in a finding or a
pointer, is done anew; a finding that names no file still stands, and one
naming a path that cannot be a repository path (absolute, as a checkout
path is, or leaving the repository) blocks, since it cannot be checked. So
does one on any documentation or generated file, pushed or not: the key
leaves that file's contents out, so after a retargeted base it may have
changed unseen. A generated path the pull request adds is reviewed in full,
as Radar already does, so its contents stay in the key and a push to it is
reviewed; generated documentation is blanked like any other documentation. The decision counts these reuses in `reused_carried`.

What this gives up: no reviewer reads the new documentation lines. The
deterministic gates still run on every head. Markdown that a program reads at
run time (a prompt, an embedded template) is treated as documentation unless
the policy's `never_carry_paths` names it.

Records written with this carry another new field, `substance`, so a
radar-gh rolled back past it rejects them (strict decoding) and reviews
anew, as with the moved-lines and guidance rules. The policy is decoded strictly too, so
a repository adds `never_carry_paths` only once every caller of its policy
runs this release, and a rollback past it means removing the field first.

## The same patches reuse an earlier head's review across guidance changes (2026-11-03)

Guidance files (`AGENTS.md`, `CLAUDE.md`,
directory guidance) are read from the default branch into every request, so
each guidance edit there changed every open pull request's request and forced
a fresh review of patches that had not changed.

Each recorded review now also keeps `ContentKey`: `PatchKey` without the
guidance text, but with every selected review pack named by ID and content
hash. A request that matches no earlier review exactly or on `PatchKey` but
matches one on `ContentKey` reuses it, under the moved-lines rule's survival proof and
line moves. That proof is required even when the hunks sit at the same
head-side lines, since their base-side numbers may still differ; a hunk
that did not move has offset 0. So every reuse other than a byte-identical
request needs the complete comparison from the earlier head, and after a
force-push the request is reviewed anew. The decision counts these in
`reused_guidance_changed`.

Review packs stay in the key. A pack change can add a rule, a security rule
among them, that the earlier review never applied, and packs change rarely,
so the saving from reusing across them would be small. Guidance is
conventions and context: a reused review was checked against the earlier
guidance, and the next push that changes the patches is reviewed against
the new one. A citation of a rule since removed is dropped, as any unknown
rule ID is, and listed in `criteria.unknown_rule_citations`. Policy that
acts on the findings (blocking severities, area policy) governs a reused
review's verdict as it is now; policy that changes the request (the chunk
size, the withheld files, a note) changes the key and is reviewed anew.

The chunk budget is the chunk size less the criteria's length, so a guidance
edit that changes that length can re-split a diff large enough to take
several requests; those requests then miss and are reviewed anew, which is
safe but saves less than the estimate on such pull requests.
`reused_guidance_changed` measures what it does save.

Records written with this carry a third new field, `content`, so as with
the moved-lines rule a radar-gh rolled back past it rejects them (strict decoding) and
reviews anew. Records without `content` reuse only by the earlier rules.

## The same patches at moved lines reuse an earlier head's review (2026-11-02)

Later rounds often leave the pull request's own patches unchanged: the same
added, removed and context lines in every file, as after a merge of the base
branch. The byte-identical rule misses many of them. Some change hunk line
numbers only: the base branch had added or removed lines above a hunk, so
GitHub's `@@` headers differ. Others come from a changed `AGENTS.md` on the
default branch, which is part of the system prompt.

Each recorded review now also keeps `PatchKey`, the request with every hunk
header's line numbers dropped (the enclosing function GitHub names after them
stays), and where each hunk sat. When a request misses the byte-identical
rule but matches an earlier head's review on `PatchKey`, that review is
reused, with its findings and pointers moved by their hunk's offset. A
finding's whole range must sit in one hunk, among the lines that request
showed (a hunk split across requests counts only its shown lines). Equal
patches are not proof enough: an author can move the same addition between
two identical blocks. So a move also needs the complete comparison from that
earlier head to this one (the one convergence already reads) to show that no
line of any hunk changed and that each sat exactly its hunk's offset earlier.
A rebase, whose comparison GitHub cannot follow, is reviewed anew, as is a
request with a moved deletion-only hunk (no head-side line to check) or with
patch lines before its first hunk header (a later piece of a split hunk or a
lockfile piece). Context lines are part of the key, so a base change next to
a patch is still reviewed anew. A finding or pointer on a line outside the
patches' hunks cannot be moved with certainty, because the base branch may
have changed it, so that review is also done anew. The decision records the
count as `reused_moved`. Reviews are stored under this head's own request, so
the verdict stage reuses them as before. Any other change to the request
still misses: a review pack, or a policy change that alters what the request
shows (the chunk size, the withheld files, a note). Only a guidance change is
reused across, by the guidance rule. Records written before this carry no `PatchKey`
and reuse only by the byte-identical rule. Records written with it carry two
new fields, so a radar-gh rolled back past this change rejects them (strict
decoding): reuse then falls back to reviewing anew, and a clarification of
such a head fails. Every stage of a caller runs the same pinned release, so
this only matters on a rollback.

As with the byte-identical rule, a reviewer that reads the repository saw the earlier
checkout. Line numbers quoted in free text (a summary, a recommendation, an
assumption's basis) are that head's and are not moved.

## A byte-identical request reuses an earlier head's review (2026-10-21)

Many later rounds send each reviewer exactly the request the previous round
had: the same prompt, criteria, withheld files and patches, at a new head.
The findings stage now answers such a request
from the earlier head's recorded review instead of a new model call.

The reviewers that read the repository saw the earlier head's checkout, so
code outside the patches (the base branch merged in) may differ from what
they read. The review is of the patches, which are byte-identical, and the
verdict stage has always reused a review on the same rule; anything else in
the request, a changed patch above all, is reviewed anew. The record comes
from a successful run of the caller's findings workflow at that earlier
head, and not when that workflow differs there, as the verdict stage's does.

## Findings raised late on unchanged lines are set apart (2026-10-19)

Holding late P1s back from the verdict still listed every late finding, and
P2s and P3s were untouched, so a long pull request still never converged: each round re-reviewed the
whole change and added findings labelled `new`, most of them on lines the
previous push had not changed, some restating an earlier finding, and few on
the lines the author had just changed to answer Radar.

From round 2 a finding below P0 first raised on lines unchanged since the
last reviewed head (not near a changed line, not the previous round's
finding by id or by file, rule and place) is recorded under
`convergence.late` and shown in a collapsed "Raised late on unchanged lines"
list, not among the findings. Radar's comment state marks it, and the next
round's baseline leaves it out, so it stays apart until a push changes its
lines. A P0 is never set apart.

The same state marks a held finding, and the next baseline passes it at P2:
v0.13.0 counted a held P1 as "already blocking" in the following round, so
round 25 blocked on a P1 round 24 had held, on lines no push had touched.
Fixture: `testdata/convergence/pr-c-round25.json`.

## Every commit is reviewed; drafts by opt-in; each run reports its cost (2026-10-14)

Pushes are not debounced or coalesced: every commit gets a review. The
callers' existing per-PR `cancel-in-progress`, which stops an older head's
review when a newer push arrives, is unchanged.

Drafts: every findings caller already skipped them and reviewed on
`ready_for_review`. A `radar:review-draft` label now reviews a draft's head
when it is added and every push while it stays; `ready_for_review` then does
not review the same head again. A label rather than a `/radar review`
comment: a comment runs `issue_comment` on the default branch, which the
reviewers' cloud role does not trust for reviews, and its run would not be
the `pull_request` findings run the verdict reuses. Other label events land
in their own concurrency group, so adding an unrelated label never cancels a
review in progress.

Measurement: each review's `usage` now counts only what the run spent (a
reused review shows no tokens and counts in `reused_requests`, where it used
to repeat the findings stage's tokens) and carries `cost_usd` at Bedrock's
US list price for the reviewer's model, the rates that reconciled with the
cloud bill. Radar's comment carries the review round and the running cost, so
the job summary shows the run's cost and the pull request's to date. The
running cost counts the findings stage's reviews (and a re-render's, which
reuse them); a verdict that has to review anew shows its cost in its own
summary only, and a cancelled run's spend is not counted. When the previous
comment cannot be read, the total restarts from this run and is marked
partial for the rest of the pull request, rather than silently undercounted.

Cheaper per-commit reviews were looked at and not built. Cache writes are
most of the spend, but what is written is the new content: the diff, the pull
request context and the repository files the reviewer reads. Only the
system prompt and tools (plus any review packs) are the same
from one push to the next, and they are already cached for every review
inside the five-minute window. The diff cannot be read from an earlier
push's cache: the pull request context (threads, check states) comes
before the patches and changes between pushes, a cache hit needs a
breakpoint the earlier request wrote at the same point (four per request),
and the first changed file ends the shared prefix anyway. A one-hour TTL on
the system prompt would save little. Reviewing only the delta since the last
reviewed head would cut the diff and the exploration, but the reviewers would
no longer see how the push fits the rest of the change. Later rounds already
block only on changed lines, so the saving would come from shrinking what the
reviewers see, which needs a replay comparison first. That is a follow-up.

## Later review rounds block only on what the push changed (2026-10-13)

Every push was reviewed afresh and any new P0 or P1 blocked, so pull requests
did not converge: the P0/P1 count per review stayed flat from round to
round, and most late blockers sat on lines the push before them had not
changed, or were findings the author had already answered and resolved.
Few had a thumbs-down or a clarification conceding them, so a round budget was not built: what still blocks late is a finding left
open or a regression on changed lines.

From round 2 a P1 counts only when it is within three lines of a line
changed since the last reviewed head, or was a P0 or P1 there (same id, or
same file and rule within ten lines once mapped across the push). One first
raised on unchanged lines is held: shown as P2 and not blocking, and not
posted inline, since an open thread would block. A P0 always counts: the
review has no structured way to tell a late security hole from a late broken
test, so security P0s keep blocking. A P2 a policy blocks on is never held,
and a defect signal other than bug-or-logic-error still blocks. A Radar
thread a person resolved stays resolved while GitHub does not mark it
outdated and the finding is near it; resolved by the pull request's author,
the `review.prior-decisions` gate routes to a human, as an assertion does for
a clarification, and the reviewers are not told to drop it, so the author
cannot clear a blocker unseen. The round and its baseline live in Radar's
comment, so every stage at a head compares with the same round; when the
changes cannot be read (a force-push, a comparison of 300 files, a patch
GitHub cut short) or the previous round's blockers did not fit the comment,
every blocker counts. The verdict stays computed from structured fields: the
input's `prior` and `resolved_findings`, recorded under `convergence`, with
findings as reported.

The prompt also asks P0 and P1 for a concrete failure through this change;
hardening, defence in depth and a missing check with no way to fail are at
most P3 (for example a P1 asking for a test against a later refactor).
Fixtures: `testdata/convergence/` (anonymised rounds of real pull requests)
and the calibration case. `scripts/rounds.py` measures the effect per round.

## The hosted review sits behind a CDN, without an access gateway (2026-10-10)

Only the CDN reaches the origin (authenticated origin pulls with our own
client certificate, verified by the ingress), and WAF and edge rate-limit
rules are applied before DNS flips to it. No access gateway and no source-IP
allowlist, because both break MCP OAuth and the claude.ai connector, which
calls from Anthropic's servers; no browser challenges, because every client
is programmatic. Identity stays the OAuth proxy's. Every MCP call is one
JSON request (stateless transport, no event stream). `review_wait` has no
ceiling for now; review and tool-call durations are recorded so one can be
chosen from data, and the ingress timeouts follow the configured wait.
Design: `docs/mcp.md`, Edge and timeouts.

## The hosted review isolates Codex from every credential (2026-10-10)

The Radar review MCP (`docs/mcp.md`) runs the same reviewers as CI, Claude
on Bedrock and Codex, on any pushed commit of an allowlisted repository; the
allowlist holds only repositories every engineer can already read, because
any signed-in user may review any of them. Results live in S3, keyed by
repository, commit, Radar version and reviewer configuration. In a pod the
IRSA web identity token is a file, which Codex, reading as its container's
user under instructions from untrusted repository text, could use to assume
the pod's role; so Codex runs in a separate reviewer container that holds no
credential: no IRSA token (`skip-containers`), no GitHub App key, no AWS
identity of its own. Each review lends it a one-repository read-only GitHub
token and Bedrock-only session credentials through a loopback broker. The
reviewer refuses to start, and fails readiness, when it can read a token or
key or reaches instance metadata, and the image test proves both directions.
Codex's own sandbox needs a user namespace and mounts, so the reviewer, and
only the reviewer, runs seccomp- and AppArmor-unconfined; without that, Codex
would review without reading the code, unlike CI. A reviewer container runs
one review at a time, since the `gh` and `git` processes of a review carry
its tokens in a readable environment, and the MCP listener, on loopback the
reviewer shares, requires a secret only the auth proxy sends.

## P0 and P1 need a risk this change introduces (2026-10-08)

Adjudicating the findings people marked thumbs-down: all were P0 or P1, none
held at that severity, and few were plain false positives. Severity was the defect, not invention. The
prompt now says severity measures what the change does. P0 and P1 require
that a line the pull request changes or adds creates the risk or newly
exposes it; the vulnerable code itself may be unchanged, as when a new caller
passes untrusted input to an existing sink. A gap the diff reaches but
neither causes nor newly exposes is pre-existing: at most P2, its impact
starts "Pre-existing:", it goes to the author as a question, and a
pre-existing security, authorization or data-handling gap sets `needs_human`
so an owner decides (for example a P0 on an authorization rule the pull
request never touched). A new entry point to existing behaviour counts as
introduced only when it grants a privilege, data or caller the existing path
does not. A finding whose own text concedes the code is acceptable as
shipped, or that fails only if something outside the change behaves
differently from today (another runner wrapping the code, a dependency
changing its contract), is at most P3 or dropped. Dormant code the change
adds, such as an uncalled public API or an optional configuration path, keeps
its severity. The cases are fixtures in `testdata/calibration/`. Repository
facts the reviewers got wrong, such as a migration runner's autocommit mode
and the concurrent index pattern, go in that area's guidance file, not the
prompt. The comparison report records precision by Radar version, so the
effect shows from this release on.

## Clarifications re-check findings; only verified ones unblock (2026-10-07)

The design is `docs/design/clarifications.md`. People with write access
answer a Radar finding in its thread, or on the pull request, starting a line
with `/radar` (`@radar` is accepted but not documented: it is an unrelated
GitHub user). The Codex reviewer re-checks only the findings named, given the
comments as untrusted, delimited, capped data, and Radar replies. A finding
is withdrawn or downgraded only when the re-check cites lines that exist at
the head; one resting on the commenter's word is "resolved by assertion", no
longer a blocking finding but routed to a human by the
`clarification.assertion` gate, so an author cannot talk Radar into
approving their own change. Who comments does not change the weight. Clarify
runs never review anew or approve, are limited to 3 re-checks per finding and
10 per pull request per UTC day, and keep their outcomes in a per-PR ledger
every stage reads. Decisions keep the findings as first reported, so
comparisons with other reviewers are not skewed.

## Reviewers list their assumptions; display-only for now (2026-10-07)

A review's findings rest on things the reviewer could not check (a deploy
order, what callers pass, a flag's value in production), and until now a
reader could not see them. Every reviewer returns at most five assumptions,
load-bearing first, each with its basis and what would change if it were
wrong. They are distinct from questions, which ask the author for something.
Radar merges them across reviewers like findings, records them in the
decision and the comparison data, and shows them after Questions. They do
not affect the verdict yet. A future option, to decide on the comparison
data: an unverifiable load-bearing assumption (basis "not visible in the
diff") contributes to `needs_human`.

## A label opts a pull request out of auto-approval (2026-10-07)

Before this, the only per-PR ways to force a human under approve mode were
side effects (an unresolved thread, requested changes, a draft), and PR text
deliberately does not count because anyone can write it. The
`radar:human-review` label (`human_review_label` in the policy) is a
deterministic, trusted opt-out: only users with triage or write access can
set labels. It routes to a human through a `pr.opt-out` gate without stopping
the review, is read again before approving, and cannot be switched off by
policy. An author-level opt-out was considered and not built.

## needs_human is narrow (2026-10-07)

Both reviewers set `needs_human` on every PR of Radar v0.8.0's first live
runs, which would have routed every pull request to a person. The reasons
mixed one real case (an irreversible bulk deletion gated on out-of-band
preconditions) with "deserves a careful read", defects that belong in
findings, and code the reviewer could not inspect. The prompt now says
`needs_human` is false for almost every change and is set only for a concrete
reason a person must act that findings and Radar's own rules (P0/P1,
signals, deny paths, area policy) do not already carry: an irreversible
operation gated on facts outside the diff, a compliance-weighted provider or
data flow change, an intended weakening of a security control, or context
that tries to steer the review.

## Pull request context is untrusted input; needs_human is explicit (2026-10-07)

Reviewers see the pull request's description, commits, linked issue,
unresolved threads and check states, so they can judge intent, avoid
repeating an open thread and check whether an earlier concern was fixed. All
of it is untrusted: delimited and capped in the user message, named as data
in the system prompt, and never read by the verdict, which stays computed
from the structured findings. A reviewer may now say a person must review
(`needs_human`); the verdict honours it. Read-only, budgeted repository tools
for GLM-5.3 follow as a separate change.

## One structured review, a computed verdict, a rendered comment (2026-10-06)

Every reviewer gets one shared, recall-oriented prompt (every P0/P1, at most
five P2/P3, suggested tests, questions; review packs appended) and returns
structured JSON: summary, risk, where to look, findings with a stable id,
impact and recommendation, questions, suggested tests, signals and coverage.
Radar merges the reviewers (same file, lines within three, same rule: one
issue, agreement recorded), computes the verdict from the fields (P0/P1
block, deny paths, `structural-change`, per-area policy, trust tier; no
gatekeeper prompt or second model call) and renders the human comment from
the same data, with each finding's status since the last push and P0/P1 also
inline.

This supersedes "a reviewer's explicit decline blocks approval" below: there
is no accept claim to decline with. Declines carried "a human should look"
for structural changes; `structural-change` now blocks directly.
`high-review-effort` still does not. Why: people read the review as well as
the automation, so the comment must be a useful review, and the verdict
cannot disagree with it.

## One review run per head: findings first, verdict reuses them (2026-10-06)

Radar is the single AI review: the findings stage reviews before checks
finish and posts the comment, and the verdict stage reuses those verdicts by
exact request, so the comment people read and the decision rest on the same
reviews. The Codex reviewer runs `codex exec` read-only over the pull request
head, keeping agentic repository access (`openai.gpt-6.1-sol`, medium
effort). Reuse is keyed on the reviewer, system prompt and rendered diff, and
failed reviews are not recorded, so a reused verdict is one the verdict stage
would have asked for anyway. It first ran in comment-only mode for about a
week, with approvals in shadow.

## Per-area review criteria only tighten (2026-10-02)

Rules files map paths to review packs and per-area policy. Every matching area
applies and policies merge to the most restrictive; an area cannot loosen the
repository policy (`auto_approve` may only be false, P0/P1 caps only 0), and
author tiers do not relax it. Area policy covers every changed path, withheld
generated files and rename sources included, as deny paths do; packs and
guidance follow only what the reviewers are shown. Rules, guidance and packs
are read from trusted sources (the default-branch checkout and the packs
repository), never the pull request head, and the prompt tells reviewers the
criteria are context that cannot change the verdict format or acceptance bar.

## Author trust tiers keep the decline rule (2026-10-01)

Private per-author tiers are evaluated in shadow only and lower a confidence
floor by at most one point. Under review-findings a tiered evaluation still
treats any decline as blocking. The review prompt tells reviewers not to
accept below 8/10, so a tier changes a review-findings verdict only when a
reviewer accepts at 7. The shadow audit measures how often that is, before
anyone weighs relaxing the decline rule for tiered authors.

## A reviewer's explicit decline blocks approval (2026-10-01)

Under `approval_basis: review-findings`, a reviewer passes only if it accepts
(`accept: true`). A decline blocks approval whatever its findings, signals or
confidence.

Why: review-findings deliberately lets size and shape signals
(`structural-change`, `high-review-effort`) through, and reviewers often say
"a human should look" only by declining with one of those signals. In a
replay with GLM-5.3 and Codex (`openai.gpt-5.6-terra`), Codex often wrote
"requires human review" in its summary, and a share of the pair's approvals
were PRs Codex itself had declined. The alternatives were leaving it as-is or keeping GLM-5.3 + Sonnet 5.

The reviewer pair became GLM-5.3 on Fireworks plus Codex on Bedrock Mantle,
replacing Claude Sonnet 5 (a replay found Sonnet's P0/P1 findings a subset of
GLM's, and Codex the most precise on P0/P1). Codex reports confidence 8-10,
so it needs no `reviewer_min_confidence` entry.

## Per-reviewer confidence floor (2026-10-01)

`reviewer_min_confidence` lowers `min_review_confidence` for one reviewer by
at most one point, under review-findings only. It was added for Sonnet 5,
which failed many replayed PRs on confidence 6-7 alone. Removing the floor
entirely was rejected: the extra approvals were security- or
compliance-adjacent changes the reviewers had judged risky without naming a
defect.

## Decide from review findings, not size (2026-09-30)

`approval_basis: review-findings` drops allow rules, global file and line
limits, and the risk threshold (the heuristic score grows with line count, so
gating on it is a size cap in disguise). The state gate, deny paths and
phrases, lockfile backstops and generated-file withholding still apply.
Approval should rest on what the reviewers find, not on size.
