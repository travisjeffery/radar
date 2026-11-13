#!/usr/bin/env python3
"""Measure how Radar's blocking findings evolve over a pull request's review rounds.

Every successful `Radar PR Findings` run reviews one head; its decision artifact
holds the merged findings. Runs of one pull request, oldest first, are its
rounds. For each P0/P1 in round 2 or later this records whether it was already
raised earlier, whether it sits on lines the push since the previous round
changed (the incremental-blocking rule), its inline reactions (👍 real, 👎 not),
whether a clarification withdrew or downgraded it, and whether a later round
no longer reports it (fixed or dropped).

Usage: rounds.py --since 2026-01-01T00:00:00Z --repo OWNER/REPO > rounds.jsonl
       rounds.py --summarize rounds.jsonl
"""
import argparse, io, json, re, subprocess, sys, zipfile
from collections import defaultdict


def gh(*args, raw=False):
    out = subprocess.run(["gh", *args], check=True, capture_output=True)
    return out.stdout if raw else json.loads(out.stdout or b"null")


def runs(repo, workflow, since):
    data = gh("api", "--paginate", "--slurp",
              f"repos/{repo}/actions/workflows/{workflow}/runs?created=>{since}&per_page=100")
    return [r for page in data for r in page["workflow_runs"]]


def decision(repo, run_id):
    for a in gh("api", f"repos/{repo}/actions/runs/{run_id}/artifacts")["artifacts"]:
        if a["name"].startswith("radar-decision-") and not a["expired"]:
            blob = gh("api", f"repos/{repo}/actions/artifacts/{a['id']}/zip", raw=True)
            with zipfile.ZipFile(io.BytesIO(blob)) as z:
                return json.loads(z.read("radar-decision.json"))
    return None


HUNK = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")


def changed_lines(repo, base, head):
    """New-side lines added or modified between two heads, by file; None when the
    comparison is unavailable (a force-push dropped base)."""
    try:
        cmp = gh("api", f"repos/{repo}/compare/{base}...{head}")
    except subprocess.CalledProcessError:
        return None
    if cmp.get("status") not in ("ahead", "identical"):
        return None
    out = defaultdict(set)
    for f in cmp.get("files") or []:
        line = 0
        for text in (f.get("patch") or "").splitlines():
            m = HUNK.match(text)
            if m:
                line = int(m.group(1))
                continue
            if text.startswith("+"):
                out[f["filename"]].add(line)
                line += 1
            elif text.startswith("-"):
                # A deletion changes the lines around it.
                out[f["filename"]].add(line)
            else:
                line += 1
    return out


def same(a, b, window=3):
    return a["id"] == b["id"] or (a.get("file") and a.get("file") == b.get("file")
                                  and a.get("rule_id", "") == b.get("rule_id", "")
                                  and abs((a.get("line") or 0) - (b.get("line") or 0)) <= window)


def repeats(f, previous):
    """Whether the previous round raised f at P0 or P1, as Radar's carry-over
    rule checks (ten lines; Radar also maps lines across the push)."""
    return any(p.get("severity") in ("P0", "P1") and same(f, p, 10) for p in previous)


def collect(repo, since):
    by_pr = defaultdict(list)
    for r in runs(repo, "radar-pr-findings.yml", since):
        if r["conclusion"] != "success":
            continue
        prs = r.get("pull_requests") or gh("api", f"repos/{repo}/commits/{r['head_sha']}/pulls")
        if not prs:
            continue
        by_pr[prs[0]["number"]].append(r)
    for pr, rs in sorted(by_pr.items()):
        rs.sort(key=lambda r: r["created_at"])
        heads, seen = [], set()
        for r in rs:
            if r["head_sha"] in seen:
                continue
            d = decision(repo, r["id"])
            if not d:
                continue
            seen.add(r["head_sha"])
            heads.append((r["head_sha"], d))
        if not heads:
            continue
        reactions = {}
        for page in gh("api", "--paginate", "--slurp", f"repos/{repo}/pulls/{pr}/comments?per_page=100"):
            for cm in page:
                m = re.search(r"<!-- radar-finding ([0-9a-f]+) -->", cm.get("body") or "")
                if m:
                    rx = cm.get("reactions") or {}
                    reactions[m.group(1)] = {"up": rx.get("+1", 0), "down": rx.get("-1", 0)}
        state = gh("api", f"repos/{repo}/pulls/{pr}")
        rounds = []
        for i, (head, d) in enumerate(heads):
            changed = changed_lines(repo, heads[i - 1][0], head) if i else None
            conceded = {c["finding_id"] for c in d.get("clarifications") or []
                        if c.get("outcome") in ("withdrawn", "downgraded", "resolved_by_assertion")}
            blockers = []
            for f in d.get("findings") or []:
                if f.get("severity") not in ("P0", "P1"):
                    continue
                # Only the previous round, as Radar compares.
                earlier = bool(i) and repeats(f, heads[i - 1][1].get("findings") or [])
                later = [same(f, n) for _, nd in heads[i + 1:] for n in nd.get("findings") or []]
                on_changed = None
                if changed is not None:
                    lo, hi = f.get("line") or 0, f.get("end_line") or f.get("line") or 0
                    on_changed = any(l in changed.get(f.get("file"), ()) for l in range(lo, hi + 1)) if lo else False
                blockers.append({"id": f["id"], "severity": f["severity"], "title": f["title"], "file": f.get("file"),
                                 "line": f.get("line"), "repeat": earlier, "on_changed_lines": on_changed,
                                 "gone_later": bool(i + 1 < len(heads)) and not any(later),
                                 "conceded": f["id"] in conceded, **reactions.get(f["id"], {"up": 0, "down": 0})})
            rounds.append({"round": i + 1, "head": head, "compare_ok": changed is not None or i == 0,
                           "verdict_reason": d.get("verdict_reason"), "blockers": blockers})
        print(json.dumps({"pr": pr, "merged": bool(state.get("merged_at")), "rounds": rounds}), flush=True)


def summarize(path):
    prs = [json.loads(l) for l in open(path)]
    per_round = defaultdict(lambda: [0, 0])
    late = defaultdict(int)
    for p in prs:
        previous = []
        for r in p["rounds"]:
            # Recomputed from the previous round's blockers, so data collected
            # before the rule matched the shipped one still measures it.
            for b in r["blockers"]:
                b["repeat"] = r["round"] > 1 and repeats(b, previous)
            previous = r["blockers"]
            n = min(r["round"], 5)
            per_round[n][0] += 1
            per_round[n][1] += len(r["blockers"])
            if r["round"] < 2:
                continue
            for b in r["blockers"]:
                late["total"] += 1
                late["repeat"] += b["repeat"]
                late["new"] += not b["repeat"]
                late["on_changed"] += b["on_changed_lines"] is True
                late["off_changed"] += b["on_changed_lines"] is False
                late["no_compare"] += b["on_changed_lines"] is None
                late["thumbs_down"] += b["down"] > 0
                late["thumbs_up"] += b["up"] > 0
                late["conceded"] += b["conceded"]
                late["gone_later"] += b["gone_later"]
                # What the incremental rule does with it: a repeat or a
                # finding on changed lines still blocks; the rest is held.
                # A P0 is never held for being raised late.
                held = b["on_changed_lines"] is False and not b["repeat"] and b["severity"] == "P1"
                late["would_hold"] += held
                late["would_hold_thumbs_up"] += held and b["up"] > 0
                late["would_hold_thumbs_down"] += held and b["down"] > 0
                late["would_hold_gone_later"] += held and b["gone_later"]
                late["still_block_changed"] += b["on_changed_lines"] is True
                late["still_block_repeat"] += b["on_changed_lines"] is not True and b["repeat"]
                if r["round"] >= 3:
                    late["r3_total"] += 1
                    late["r3_off_changed"] += b["on_changed_lines"] is False
                    late["r3_down_or_conceded"] += b["down"] > 0 or b["conceded"]
                    late["r3_up"] += b["up"] > 0
    print(f"pull requests: {len(prs)}, with 2+ rounds: {sum(len(p['rounds']) > 1 for p in prs)}, 3+: {sum(len(p['rounds']) > 2 for p in prs)}")
    print("round | reviews | P0/P1 | per review")
    for n in sorted(per_round):
        reviews, blockers = per_round[n]
        print(f"{n if n < 5 else '5+'} | {reviews} | {blockers} | {blockers / reviews:.2f}")
    print(json.dumps(dict(late), indent=1))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since")
    ap.add_argument("--repo", required=True)
    ap.add_argument("--summarize")
    a = ap.parse_args()
    if a.summarize:
        summarize(a.summarize)
    elif a.since:
        collect(a.repo, a.since)
    else:
        ap.error("--since or --summarize is required")


if __name__ == "__main__":
    sys.exit(main())
