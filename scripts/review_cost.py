#!/usr/bin/env python3
"""Measure what each Radar review costs and how long it takes.

Every `Radar PR Findings` run reviews one pull request head; its decision
artifact records, per reviewer, the token usage (uncached input, cache reads
and writes, output), the model requests, the tool calls and the elapsed time.
`Radar PR Review` runs the verdict stage after checks and reuses those reviews
when nothing changed, so its fresh reviews are counted apart. Cancelled runs
(a newer push superseded them) are counted with their runner time.

A finding is "acted on" when the next reviewed push changes lines within three
of it, or a person reacts 👍 to its inline comment; "dismissed" when it gets
a 👎 or a clarification withdraws or downgrades it. A finding of the last
reviewed head has no next push and is left out of the rate.

Usage: review_cost.py --since 2026-10-06T00:00:00Z --repo OWNER/REPO > runs.jsonl
       review_cost.py --outcomes runs.jsonl > runs2.jsonl   # findings again, from the run rows
       review_cost.py --summarize runs.jsonl [runs2.jsonl ...]
"""
import argparse, hashlib, io, json, os, re, statistics, subprocess, sys, threading, time, zipfile
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone

VERDICT_SAMPLE = 4
JOBS_SAMPLE = 3

# Bedrock US on-demand list prices per million tokens, as cost.go:
# input, cache read, cache write (five-minute), output.
PRICES = {
    "anthropic.claude-opus-5-5": (4.40, 0.22, 5.50, 22.0),
    "openai.gpt-6.1-sol": (2.20, 0.11, 2.75, 11.0),
}


# The API quota is shared with deploys and other agents: space calls at least
# PACE seconds apart and, every BUDGET_EVERY calls, pause until the reset when
# fewer than FLOOR requests remain.
PACE, FLOOR, BUDGET_EVERY = 0.5, 1500, 50
_lock = threading.Lock()
_last = [0.0]
_calls = [0]


def quota():
    """Core requests remaining and the reset time, from the headers of counted
    calls. GitHub keeps two counters for one token: the artifact and
    per-workflow run endpoints answer from one, jobs, compares and comments
    from the other, and /rate_limit shows only the second. The tighter wins."""
    worst = None
    for probe in ("repos/travisjeffery/radar/actions/artifacts?per_page=1", "repos/travisjeffery/radar/actions/runs?per_page=1"):
        out = subprocess.run(["gh", "api", "-i", probe], capture_output=True).stdout.decode(errors="replace")
        h = {k.strip().lower(): v.strip() for k, _, v in (l.partition(":") for l in out.splitlines()[:40]) if v}
        try:
            q = (int(h["x-ratelimit-remaining"]), int(h["x-ratelimit-reset"]))
        except (KeyError, ValueError):
            q = (0, int(time.time()) + 60)
        if worst is None or q[0] < worst[0]:
            worst = q
    return worst


def throttle():
    with _lock:
        wait = _last[0] + PACE - time.monotonic()
        if wait > 0:
            time.sleep(wait)
        _last[0] = time.monotonic()
        _calls[0] += 1
        if _calls[0] % BUDGET_EVERY == 0:
            remaining, reset = quota()
            if remaining < FLOOR:
                print(f"{remaining} API requests left; pausing until "
                      f"{datetime.fromtimestamp(reset, timezone.utc):%H:%M:%S}Z", file=sys.stderr, flush=True)
                time.sleep(max(reset - time.time(), 0) + 5)


CACHE = os.path.expanduser(os.environ.get("RADAR_COST_CACHE", "~/.cache/radar-review-cost"))


def gh(*args, raw=False, tries=3, cache=False):
    """Call gh; cache=True keeps the response on disk, for what cannot change
    (artifacts, a finished run's jobs, a comparison of two commits). The API
    allows 5000 requests an hour, shared with every other tool: on a rate
    limit it waits for the reset."""
    path = os.path.join(CACHE, hashlib.sha256(" ".join(args).encode()).hexdigest()) if cache else None
    if path and os.path.exists(path):
        with open(path, "rb") as f:
            body = f.read()
        return body if raw else json.loads(body or b"null")
    for i in range(tries):
        throttle()
        out = subprocess.run(["gh", *args], capture_output=True)
        limited = b"rate limit" in out.stderr.lower() or b"API rate limit exceeded" in out.stdout[:400]
        if out.returncode == 0 and not limited:
            if path:
                os.makedirs(CACHE, exist_ok=True)
                with open(path + ".tmp", "wb") as f:
                    f.write(out.stdout)
                os.replace(path + ".tmp", path)
            return out.stdout if raw else json.loads(out.stdout or b"null")
        if limited:
            # The window may have reset since the call failed: wait only while
            # the quota is still spent.
            remaining, reset = quota()
            if remaining < FLOOR:
                print(f"rate limited; waiting until {datetime.fromtimestamp(reset, timezone.utc):%H:%M:%S}Z", file=sys.stderr, flush=True)
                time.sleep(max(reset - time.time(), 0) + 5)
            continue
        if i + 1 == tries:
            raise subprocess.CalledProcessError(out.returncode, ["gh", *args], out.stdout, out.stderr)
        time.sleep(2 ** i)


def runs(repo, workflow, since):
    """Runs created since, a day at a time: GitHub lists at most 1000 runs a query."""
    out, day = [], ts(since)
    while day < datetime.now(timezone.utc):
        end = day + timedelta(days=1)
        rng = f"{day:%Y-%m-%dT%H:%M:%SZ}..{end:%Y-%m-%dT%H:%M:%SZ}"
        data = gh("api", "--paginate", "--slurp", f"repos/{repo}/actions/workflows/{workflow}/runs?created={rng}&per_page=100")
        out += [r for page in data for r in page["workflow_runs"]]
        day = end
    return list({r["id"]: r for r in out}.values())


def artifacts(repo, run_id):
    """The run's decision and, at the findings stage, its review record."""
    out = {}
    for a in gh("api", f"repos/{repo}/actions/runs/{run_id}/artifacts", cache=True)["artifacts"]:
        for prefix, name in (("radar-decision-", "radar-decision.json"), ("radar-findings-", "radar-record.json")):
            if a["name"].startswith(prefix) and not a["expired"]:
                blob = gh("api", f"repos/{repo}/actions/artifacts/{a['id']}/zip", raw=True, cache=True)
                with zipfile.ZipFile(io.BytesIO(blob)) as z:
                    if name in z.namelist():
                        out[name] = json.loads(z.read(name))
    return out


def ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")) if s else None


def secs(a, b):
    return (ts(b) - ts(a)).total_seconds() if a and b else None


def jobs(repo, run_id):
    out = []
    for j in gh("api", f"repos/{repo}/actions/runs/{run_id}/jobs", cache=True)["jobs"]:
        steps = [{"name": s["name"], "s": secs(s.get("started_at"), s.get("completed_at"))}
                 for s in j.get("steps") or []]
        out.append({"name": j["name"], "conclusion": j.get("conclusion"),
                    "queued": secs(j.get("created_at"), j.get("started_at")),
                    "ran": secs(j.get("started_at"), j.get("completed_at")),
                    "steps": [s for s in steps if s["s"] and s["s"] >= 5]})
    return out


def cost(reviewer, u):
    u = {k: u.get(k) or 0 for k in ("input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "requests")}
    for model, (pin, pread, pwrite, pout) in PRICES.items():
        if model in reviewer:
            uncached = max(u["input_tokens"] - u["cache_read_tokens"] - u["cache_write_tokens"], 0)
            return {"uncached": uncached * pin / 1e6, "cache_read": u["cache_read_tokens"] * pread / 1e6,
                    "cache_write": u["cache_write_tokens"] * pwrite / 1e6, "output": u["output_tokens"] * pout / 1e6}
    return None


def run_record(repo, workflow, r):
    rec = {"repo": repo, "workflow": workflow, "run": r["id"], "head": r["head_sha"],
           "conclusion": r["conclusion"], "created": r["created_at"],
           "wall": secs(r.get("run_started_at"), r.get("updated_at"))}
    rec["jobs"] = []
    verdict = workflow == "radar-pr-review.yml"
    # The verdict stage mostly reuses the findings stage's reviews; a sample of
    # its runs (one in VERDICT_SAMPLE) measures how often it reviews afresh.
    if r["conclusion"] in ("skipped", None) or verdict and (r["conclusion"] != "success" or r["id"] % VERDICT_SAMPLE):
        return rec
    # Job timings of one findings run in JOBS_SAMPLE, and of every cancelled one.
    if not verdict and (r["conclusion"] == "cancelled" or r["id"] % JOBS_SAMPLE == 0):
        try:
            rec["jobs"] = jobs(repo, r["id"])
        except subprocess.CalledProcessError:
            pass
    if r["conclusion"] != "success":
        return rec
    try:
        arts = artifacts(repo, r["id"])
    except subprocess.CalledProcessError as e:
        print(f"run {r['id']}: {e}", file=sys.stderr)
        rec["error"] = "artifact"
        return rec
    d = arts.get("radar-decision.json")
    if not d:
        return rec
    # The request hash covers the reviewer, prompt and rendered diff without
    # the pull request context: an identical one could reuse the review.
    rec["requests"] = sorted(f"{x['reviewer']} {x['request']}" for x in (arts.get("radar-record.json") or {}).get("reviews") or [])
    rec["pr"] = int(d["request_id"].rsplit("#", 1)[1])
    rec["stage"] = d.get("stage")
    rec["round"] = (d.get("convergence") or {}).get("round")
    rec["reused_reviews"] = d.get("reused_reviews", 0)
    rec["reused_from_head"] = d.get("reused_from_head")  # v0.14.0
    rec["reviewed_files"] = len((d.get("agent") or {}).get("reviewed_files") or [])
    rec["guidance"] = [g["sha256"][:12] for g in (d.get("criteria") or {}).get("guidance") or [] if g.get("prefix")]
    rec["reviews"] = []
    for rv in d.get("reviews") or []:
        a = rv.get("agent") or {}
        u = a.get("usage")
        raw = u or {}
        if u:  # older records leave zero counts out
            u = {k: u.get(k) or 0 for k in ("input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "requests")}
        rec["reviews"].append({"reviewer": rv["reviewer"], "passed": rv.get("passed"),
                               "elapsed_ms": a.get("elapsed_ms"), "usage": u,
                               "cost": cost(rv["reviewer"], u) if u else None,
                               "tool_calls": (a.get("coverage") or {}).get("tool_calls"),
                               # v0.14.0, measurement only
                               "reused": raw.get("reused_requests") or 0,
                               "first_request": raw.get("first_request"),
                               "prefix": (raw.get("prefix_sha256") or "")[:12] or None,
                               "tools": (a.get("coverage") or {}).get("tools"),
                               "findings": [{k: f.get(k) for k in ("severity", "file", "line", "end_line")}
                                            for f in a.get("findings") or []]})
    # Radar v0.14.0 or later: its usage carries cost_usd and first_request.
    rec["v014"] = any(rv["first_request"] is not None or rv["reused"] for rv in rec["reviews"]) or any(
        ((rv.get("agent") or {}).get("usage") or {}).get("cost_usd") is not None for rv in d.get("reviews") or [])
    rec["findings"] = [{k: f.get(k) for k in ("id", "severity", "file", "line", "end_line")}
                       for f in d.get("findings") or []]
    rec["conceded"] = [c["finding_id"] for c in d.get("clarifications") or []
                       if c.get("outcome") in ("withdrawn", "downgraded", "resolved_by_assertion")]
    return rec


def changed_lines(repo, base, head):
    """Lines the push from base to head changed, by file, on base's side, so
    a finding raised at base can be matched; None when the comparison is
    unavailable (a force-push dropped base)."""
    try:
        cmp = gh("api", f"repos/{repo}/compare/{base}...{head}", cache=True)
    except subprocess.CalledProcessError:
        return None
    if cmp.get("status") not in ("ahead", "identical"):
        return None
    out = defaultdict(set)
    for f in cmp.get("files") or []:
        line = 0
        for text in (f.get("patch") or "").splitlines():
            m = re.match(r"^@@ -(\d+)", text)
            if m:
                line = int(m.group(1))
                continue
            if text.startswith("-"):
                out[f["filename"]].add(line)
                line += 1
            elif text.startswith("+"):
                out[f["filename"]].add(line)
            else:
                line += 1
    return out


def reactions(repo, pr):
    out = {}
    for page in gh("api", "--paginate", "--slurp", f"repos/{repo}/pulls/{pr}/comments?per_page=100"):
        for cm in page:
            m = re.search(r"<!-- radar-finding ([0-9a-f]+) -->", cm.get("body") or "")
            if m:
                rx = cm.get("reactions") or {}
                out[m.group(1)] = {"up": rx.get("+1", 0), "down": rx.get("-1", 0)}
    return out


def near(f, lines):
    """Whether the push changed lines within three of f, and the coincidence
    control: lines 30 to 40 away, as Cloudflare measured."""
    lines = lines.get(f["file"], ())
    lo, hi = f["line"] - 3, (f.get("end_line") or f["line"]) + 3
    touched = any(l in lines for l in range(lo, hi + 1))
    control = any(l in lines for l in (*range(f["line"] - 40, f["line"] - 29), *range(f["line"] + 30, f["line"] + 41)))
    return touched, control


def outcomes(repo, pr, recs):
    """Label each finding of a pull request's findings-stage rounds: the merged
    findings by severity, and each reviewer's own findings by reviewer."""
    heads, seen = [], set()
    for r in sorted(recs, key=lambda r: r["created"]):
        if r.get("findings") is not None and r["head"] not in seen:
            seen.add(r["head"])
            heads.append(r)
    rx = reactions(repo, pr)
    out = []
    for i, r in enumerate(heads):
        nxt = changed_lines(repo, r["head"], heads[i + 1]["head"]) if i + 1 < len(heads) else None
        for f in r["findings"]:
            touched = control = None
            if nxt is not None and f.get("file") and f.get("line"):
                touched, control = near(f, nxt)
            x = rx.get(f["id"], {"up": 0, "down": 0})
            out.append({"pr": pr, "round": r.get("round"), "severity": f["severity"], "touched_next": touched, "control": control,
                        "last": i + 1 == len(heads), "conceded": f["id"] in r["conceded"], **x})
        for rv in r["reviews"]:
            for f in rv["findings"] if isinstance(rv["findings"], list) else []:
                touched = control = None
                if nxt is not None and f.get("file") and f.get("line"):
                    touched, control = near(f, nxt)
                out.append({"pr": pr, "reviewer": rv["reviewer"], "severity": f["severity"], "touched_next": touched,
                            "control": control, "last": i + 1 == len(heads)})
    return out


def safe_record(repo, workflow, r):
    try:
        return run_record(repo, workflow, r)
    except Exception as e:  # one malformed run must not lose the collection
        print(f"run {r['id']}: {e!r}", file=sys.stderr)
        return {"repo": repo, "workflow": workflow, "run": r["id"], "head": r["head_sha"],
                "conclusion": r["conclusion"], "created": r["created_at"], "wall": None, "jobs": [], "error": repr(e)}


def safe_outcomes(repo, pr, recs):
    try:
        return outcomes(repo, pr, recs)
    except subprocess.CalledProcessError as e:
        print(f"pr {pr}: {e}", file=sys.stderr)
        return []


def collect(repo, since):
    todo = [(wf, r) for wf in ("radar-pr-findings.yml", "radar-pr-review.yml") for r in runs(repo, wf, since)]
    by_pr = defaultdict(list)
    with ThreadPoolExecutor(8) as ex:
        recs = ex.map(lambda t: safe_record(repo, *t), todo)
        for r in recs:
            print(json.dumps({"kind": "run", **r}), flush=True)
            if r["workflow"] == "radar-pr-findings.yml" and r.get("pr"):
                by_pr[r["pr"]].append(r)
    with ThreadPoolExecutor(8) as ex:
        for fs in ex.map(lambda kv: safe_outcomes(repo, *kv), by_pr.items()):
            for f in fs:
                print(json.dumps({"kind": "rfinding" if "reviewer" in f else "finding", "repo": repo, **f}), flush=True)


def recompute(path):
    """Re-derive the finding rows from a collection's run rows."""
    rows = [json.loads(l) for l in open(path)]
    by_pr = defaultdict(list)
    for r in rows:
        if r["kind"] == "run":
            print(json.dumps(r), flush=True)
            if r["workflow"] == "radar-pr-findings.yml" and r.get("pr"):
                by_pr[(r["repo"], r["pr"])].append(r)
    with ThreadPoolExecutor(8) as ex:
        for fs in ex.map(lambda kv: [{"repo": kv[0][0], **f} for f in safe_outcomes(kv[0][0], kv[0][1], kv[1])], by_pr.items()):
            for f in fs:
                print(json.dumps({"kind": "rfinding" if "reviewer" in f else "finding", **f}), flush=True)


def pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    return xs[min(len(xs) - 1, int(p / 100 * len(xs)))] if xs else None


def tier(n):
    return "1-2" if n <= 2 else "3-10" if n <= 10 else "11-30" if n <= 30 else "31+"


def summarize(paths):
    rows = [json.loads(l) for p in paths for l in open(p)]
    runs_ = [r for r in rows if r["kind"] == "run"]
    finds = [r for r in rows if r["kind"] == "finding"]
    ok = [r for r in runs_ if r.get("reviews") is not None]

    print("## Runs")
    for wf in ("radar-pr-findings.yml", "radar-pr-review.yml"):
        rs = [r for r in runs_ if r["workflow"] == wf]
        by = defaultdict(int)
        for r in rs:
            by[r["conclusion"]] += 1
        cancelled_min = sum(max(j["ran"] or 0, 0) for r in rs if r["conclusion"] == "cancelled" for j in r["jobs"]) / 60
        print(f"{wf}: {len(rs)} runs {dict(by)}; runner minutes in cancelled runs {cancelled_min:.0f}")

    # A review is fresh when it made model requests; a reused one reports the
    # recorded usage again (before cost tracking), so the verdict stage counts only
    # its reviews whose request hash missed the record. Without the hash here,
    # count the verdict stage's reviews only when the run reused none.
    fresh, other = [], defaultdict(int)
    for r in ok:
        if r["workflow"] == "radar-pr-review.yml" and r.get("reused_reviews"):
            continue
        for rv in r["reviews"]:
            if not rv["usage"] or not rv["usage"]["requests"]:
                continue  # reused (those records show zero tokens)
            if rv["cost"]:
                fresh.append((r, rv))
            else:
                other[rv["reviewer"]] += 1
    if other:
        print(f"reviews on unpriced models, left out: {dict(other)}")
    print(f"\n## Per review (fresh model reviews: {len(fresh)})")
    print("reviewer | n | $/review | uncached | cache write | cache read | output | in tok p50 | out tok p50 | requests p50 | tool calls p50 | elapsed p50 s | p90 | p99")
    for name in sorted({rv["reviewer"] for _, rv in fresh}):
        xs = [rv for _, rv in fresh if rv["reviewer"] == name]
        n = len(xs)
        c = {k: sum(rv["cost"][k] for rv in xs) / n for k in ("uncached", "cache_write", "cache_read", "output")}
        tot = sum(c.values())
        el = [rv["elapsed_ms"] / 1000 for rv in xs if rv["elapsed_ms"]]
        print(f"{name} | {n} | {tot:.3f} | " + " | ".join(f"{c[k]:.3f} ({c[k] / tot:.0%})" for k in c) +
              f" | {pct([rv['usage']['input_tokens'] for rv in xs], 50)} | {pct([rv['usage']['output_tokens'] for rv in xs], 50)}"
              f" | {pct([rv['usage']['requests'] for rv in xs], 50)} | {pct([rv['tool_calls'] for rv in xs], 50)}"
              f" | {pct(el, 50):.0f} | {pct(el, 90):.0f} | {pct(el, 99):.0f}")

    # Cache reuse: of all input, how much was read back, and for single-request
    # reviews (where the only request is the first) whether anything was read.
    print("\nreviewer | input read from cache | written | 1-request reviews | of which read any cache")
    for name in sorted({rv["reviewer"] for _, rv in fresh}):
        xs = [rv["usage"] for _, rv in fresh if rv["reviewer"] == name]
        inp = sum(u["input_tokens"] for u in xs)
        one = [u for u in xs if u["requests"] == 1]
        print(f"{name} | {sum(u['cache_read_tokens'] for u in xs) / inp:.0%} | {sum(u['cache_write_tokens'] for u in xs) / inp:.0%}"
              f" | {len(one)} | {sum(u['cache_read_tokens'] > 0 for u in one)}")

    print("\n## Per findings run, by size tier (reviewed files)")
    print("tier | runs | $/run p50 | mean | p90 | wall p50 s | p90 | p99 | slowest reviewer p50 s")
    fr = [r for r in ok if r["workflow"] == "radar-pr-findings.yml"]
    for t in ("1-2", "3-10", "11-30", "31+", "all"):
        xs = [r for r in fr if t == "all" or tier(r["reviewed_files"]) == t]
        if not xs:
            continue
        costs = [sum(sum(rv["cost"].values()) for rv in r["reviews"] if rv["cost"]) for r in xs]
        slow = [max((rv["elapsed_ms"] or 0) for rv in r["reviews"]) / 1000 for r in xs if r["reviews"]]
        print(f"{t} | {len(xs)} | {pct(costs, 50):.2f} | {statistics.mean(costs):.2f} | {pct(costs, 90):.2f}"
              f" | {pct([r['wall'] for r in xs], 50):.0f} | {pct([r['wall'] for r in xs], 90):.0f} | {pct([r['wall'] for r in xs], 99):.0f}"
              f" | {pct(slow, 50):.0f}")
    rounds = defaultdict(int)
    for r in fr:
        rounds[min(r.get("round") or 1, 5)] += 1
    calls = [sum(rv["usage"]["requests"] for rv in r["reviews"] if rv["usage"]) for r in fr]
    tools = [sum(rv["tool_calls"] or 0 for rv in r["reviews"]) for r in fr]
    print(f"\nper findings run: model requests p50 {pct(calls, 50)} mean {statistics.mean(calls):.1f}; "
          f"Radar-run tool calls (bedrock, fireworks) p50 {pct(tools, 50)} mean {statistics.mean(tools):.1f}")
    print(f"\nfindings runs by round (5 = 5+): {dict(sorted(rounds.items()))}; re-reviews (round 2+): "
          f"{sum(v for k, v in rounds.items() if k > 1) / max(len(fr), 1):.0%}")
    days = {r["created"][:10] for r in fr}
    spend = sum(sum(rv["cost"].values()) for r, rv in fresh)
    print(f"spend on fresh reviews: ${spend:.0f} over {len(days)} days with runs")

    vr = [r for r in ok if r["workflow"] == "radar-pr-review.yml"]
    print(f"verdict runs with a decision: {len(vr)}; reused every review: "
          f"{sum(r.get('reused_reviews', 0) >= len(r['reviews']) for r in vr)}; reviewed afresh: "
          f"{sum(not r.get('reused_reviews') and bool(r['reviews']) for r in vr)}")

    # A round whose request hashes all equal the previous round's sent the same
    # prompt and patches again: skipping it could not change a finding.
    by_pr = defaultdict(list)
    for r in fr:
        by_pr[(r["repo"], r["pr"])].append(r)
    same, later, later_cost, same_cost = 0, 0, 0.0, 0.0
    for rs in by_pr.values():
        rs.sort(key=lambda r: r["created"])
        for prev, cur in zip(rs, rs[1:]):
            c = sum(sum(rv["cost"].values()) for rv in cur["reviews"] if rv["cost"])
            later += 1
            later_cost += c
            if cur.get("requests") and cur["requests"] == prev.get("requests"):
                same += 1
                same_cost += c
    print(f"re-reviews whose requests repeat the previous round's: {same}/{later} (${same_cost:.0f} of ${later_cost:.0f})")

    print("\n## Reuse across pushes")
    rr = [r for r in fr if r.get("reused_from_head")]
    reused = [(r, rv) for r in fr for rv in r["reviews"] if rv.get("reused")]
    print(f"findings runs answered partly from an earlier head: {len(rr)} of {len(fr)}; reviews reused: {len(reused)}")
    for name in sorted({rv["reviewer"] for _, rv in reused}):
        n = sum(rv["reviewer"] == name for _, rv in reused)
        xs = [sum(rv["cost"].values()) for _, rv in fresh if rv["reviewer"] == name]
        if xs:
            print(f"  {name}: {n} reused, ~${n * statistics.mean(xs):.2f} not spent at its mean fresh cost")

    print("\n## First request and prefix")
    print("reviewer | reviews with first_request | first request input read from cache | read any | prefixes")
    for name in sorted({rv["reviewer"] for _, rv in fresh}):
        xs = [rv for _, rv in fresh if rv["reviewer"] == name and rv.get("first_request")]
        if not xs:
            continue
        inp = sum(rv["first_request"]["input_tokens"] for rv in xs)
        read = sum(rv["first_request"].get("cache_read_tokens") or 0 for rv in xs)
        anyread = sum((rv["first_request"].get("cache_read_tokens") or 0) > 0 for rv in xs)
        print(f"{name} | {len(xs)} | {read / max(inp, 1):.0%} | {anyread}/{len(xs)} | {len({rv['prefix'] for rv in xs})}")
    print("\nreviewer | tool | calls per review | KB returned per review")
    for name in sorted({rv["reviewer"] for _, rv in fresh}):
        xs = [rv for _, rv in fresh if rv["reviewer"] == name and rv.get("tools") is not None]
        if not xs:
            continue
        tot = defaultdict(lambda: [0, 0])
        for rv in xs:
            for tool, use in rv["tools"].items():
                tot[tool][0] += use["calls"]
                tot[tool][1] += use["bytes"]
        for tool, (c, b) in sorted(tot.items(), key=lambda kv: -kv[1][0])[:10]:
            print(f"{name} | {tool} | {c / len(xs):.1f} | {b / len(xs) / 1024:.1f}")

    print("\n## Job and step time (successful findings runs)")
    st = defaultdict(list)
    for r in fr:
        for j in r["jobs"]:  # sampled
            st[("job", j["name"], "queued")].append(j["queued"])
            st[("job", j["name"], "ran")].append(j["ran"])
            for s in j["steps"]:
                st[("step", j["name"], s["name"])].append(s["s"])
    for k, v in sorted(st.items(), key=lambda kv: -statistics.mean(x for x in kv[1] if x is not None)):
        v = [x for x in v if x is not None]
        if len(v) >= len(fr) / 4 and pct(v, 50) >= 3:
            print(f"{k[0]} {k[1]} / {k[2]}: n={len(v)} p50={pct(v, 50):.0f}s p90={pct(v, 90):.0f}s p99={pct(v, 99):.0f}s")

    print("\n## Findings outcome (not last round)")
    print("severity | findings | touched by next push | control (30-40 lines away) | 👍 | 👎 | conceded | acted on")
    for sev in ("P0", "P1", "P2", "P3", "all"):
        xs = [f for f in finds if not f["last"] and (sev == "all" or f["severity"] == sev)]
        if not xs:
            continue
        acted = sum(bool(f["touched_next"]) or f["up"] > 0 for f in xs)
        print(f"{sev} | {len(xs)} | {sum(bool(f['touched_next']) for f in xs)} | {sum(bool(f.get('control')) for f in xs)} | {sum(f['up'] > 0 for f in xs)}"
              f" | {sum(f['down'] > 0 for f in xs)} | {sum(f['conceded'] for f in xs)} | {acted / len(xs):.0%}")

    print("\n## Acted on by reviewer (each reviewer's own findings, not last round, with a line)")
    print("reviewer | severity | findings | touched by next push | control | net of control")
    rf = [f for f in rows if f["kind"] == "rfinding" and not f["last"] and f["touched_next"] is not None]
    for name in sorted({f["reviewer"] for f in rf}):
        for sev in ("P0+P1", "P2+P3", "all"):
            xs = [f for f in rf if f["reviewer"] == name and (sev == "all" or f["severity"] in sev.split("+"))]
            if not xs:
                continue
            t, c = sum(f["touched_next"] for f in xs) / len(xs), sum(bool(f["control"]) for f in xs) / len(xs)
            print(f"{name} | {sev} | {len(xs)} | {t:.0%} | {c:.0%} | {t - c:+.0%}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since")
    ap.add_argument("--repo", required=True)
    ap.add_argument("--summarize", nargs="+")
    ap.add_argument("--outcomes", help="recompute the finding rows of a collection")
    a = ap.parse_args()
    if a.summarize:
        summarize(a.summarize)
    elif a.outcomes:
        recompute(a.outcomes)
    elif a.since:
        collect(a.repo, a.since)
    else:
        ap.error("--since or --summarize is required")


if __name__ == "__main__":
    sys.exit(main())
