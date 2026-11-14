#!/usr/bin/env python3
"""The daily Hermes compatibility run: which refs to test, what their contract runs say, and the issues.

Subcommands (used by ``.github/workflows/upstream-compat.yml``):

``refs RELEASES.jsonl [--count N] [--github-output FILE]``
    Upstream ``main`` plus the last N published releases. Input: one release per line as the GitHub
    releases API returns it (``tag_name``, ``draft``, ``prerelease``, ``published_at``). Only
    ``vMAJOR.MINOR.PATCH`` tags of non-draft, non-prerelease releases count, ordered by publication
    (Hermes also publishes calendar-named tags such as ``v2026.9.24``; ordering by version would pick those).

``summarize ARTIFACTS --expect main,v1.2.3 --refs-result R --out report.json [--summary FILE]``
    Reads, per ref, ``compat-<ref>/junit.xml`` (every contract test) and ``status-matrix-e2e-*.json`` (the
    status register after the end-to-end flows) and sorts every finding into one category:
    ``security`` (a failed or skipped security contract test, a degraded security intervention),
    ``comfort`` (a failed comfort contract test, a comfort intervention that fell back), ``infrastructure``
    (no refs, a ref whose job produced no test results).

``issues report.json --repo OWNER/NAME [--run-url URL] [--dry-run]``
    One issue per category with a fixed title. A new finding set opens or comments the issue; the same set
    as last time adds nothing; no findings close an open issue, but only after a complete run: while a ref
    went untested (infrastructure finding), the security and comfort issues stay open and a comment names the
    untested refs. Text that came out of a run against upstream code is neutralised before it reaches an issue
    (code spans, no mentions, bounded length).

``verdict report.json``
    Exit 1 when there are security or infrastructure findings (the run turns red), else 0.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import sys
import xml.etree.ElementTree as ET
from pathlib import Path
from typing import Any, Iterable, Optional

RELEASE_TAG = re.compile(r"v\d+\.\d+\.\d+")
REF_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
GOOD = {"ok", "armed", "wrapped", "native", "injected"}
# A4 falls back by design in the end-to-end flow's own unarmed-profile case.
EXPECTED = {("A4", "fallback")}
# Per-context notices of the turn context (A9[bot/guild/lane/mode]): configuration findings, not adaptation drift.
NOTICE = re.compile(r"A9\[.+\]")
# Contract test files (and end-to-end flows) whose failure is a comfort regression; everything else counts as
# security (a test nobody classified fails closed).
COMFORT_TESTS = {"test_comfort_injections", "test_delivery_contract", "test_ui_contract"}
COMFORT_SUFFIX = "_comfort"
CATEGORIES = ("security", "comfort", "infrastructure")
TITLES = {
    "security": "Upstream drift: security",
    "comfort": "Upstream drift: comfort",
    "infrastructure": "Upstream compatibility run failed",
}
MARKER = re.compile(r"<!-- dsp-compat:(security|comfort|infrastructure):([0-9a-f]{16}|none) -->")
TEXT_LIMIT = 300


# ── refs ─────────────────────────────────────────────────────────────────────────────────────────────


def pick_refs(releases: Iterable[dict], count: int = 3) -> list[str]:
    usable = [r for r in releases if isinstance(r, dict) and not r.get("draft") and not r.get("prerelease")
              and isinstance(r.get("tag_name"), str) and RELEASE_TAG.fullmatch(r["tag_name"])
              and isinstance(r.get("published_at"), str)]
    usable.sort(key=lambda r: r["published_at"], reverse=True)
    tags: list[str] = []
    for release in usable:
        if release["tag_name"] not in tags:
            tags.append(release["tag_name"])
    return ["main"] + tags[:count]


def _read_jsonl(path: Path) -> list[dict]:
    out = []
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line:
            out.append(json.loads(line))
    return out


def cmd_refs(args: argparse.Namespace) -> int:
    refs = pick_refs(_read_jsonl(args.releases), args.count)
    if len(refs) < 2:
        print("no published release found", file=sys.stderr)
        return 1
    print("refs:", ", ".join(refs))
    if args.github_output:
        with open(args.github_output, "a", encoding="utf-8") as fh:
            fh.write(f"matrix={json.dumps(refs)}\nexpect={','.join(refs)}\n")
    return 0


# ── summarize ────────────────────────────────────────────────────────────────────────────────────────


def test_category(classname: str, name: str) -> str:
    module = classname.rsplit(".", 1)[-1]
    base = name.split("[", 1)[0]
    return "comfort" if module in COMFORT_TESTS or base.endswith(COMFORT_SUFFIX) else "security"


def read_junit(path: Path) -> list[dict]:
    """Every test case with its state: passed | failed | error | skipped."""
    cases = []
    for case in ET.parse(path).getroot().iter("testcase"):
        state, message = "passed", ""
        for tag in ("failure", "error", "skipped"):
            node = case.find(tag)
            if node is not None:
                state = {"failure": "failed", "error": "error", "skipped": "skipped"}[tag]
                message = node.get("message") or (node.text or "")
                break
        cases.append({"classname": case.get("classname", ""), "name": case.get("name", ""), "state": state,
                      "message": message})
    return cases


def read_matrices(directory: Path) -> dict[str, dict]:
    """The end-to-end flows' status registers of one ref, merged (a degraded outcome wins)."""
    merged: dict[str, dict] = {}
    for path in sorted(directory.rglob("status-matrix-e2e-*.json")):
        data = json.loads(path.read_text(encoding="utf-8"))
        for name, result in (data.get("interventions") or {}).items():
            if not isinstance(result, dict):
                continue
            current = merged.get(name)
            if current is None or (current.get("outcome") in GOOD and result.get("outcome") not in GOOD):
                merged[name] = result
    return merged


def _finding(category: str, ref: str, key: str, text: str) -> dict:
    return {"category": category, "ref": ref, "key": key, "text": text}


def summarize(artifacts: Path, expect: list[str], refs_result: str = "success") -> dict:
    findings: list[dict] = []
    refs: dict[str, dict] = {}
    if refs_result != "success" or not expect:
        findings.append(_finding("infrastructure", "-", "refs",
                                 f"the Hermes refs could not be determined (refs job: {refs_result or 'unknown'})"))
    for ref in expect:
        if not REF_NAME.fullmatch(ref):
            findings.append(_finding("infrastructure", "-", f"ref:{ref}", "a ref name outside the expected form"))
            continue
        directory = artifacts / f"compat-{ref}"
        junit = directory / "junit.xml"
        entry = {"tests": [], "interventions": read_matrices(directory) if directory.is_dir() else {},
                 "hermes_sha": ""}
        refs[ref] = entry
        for path in sorted(directory.rglob("status-matrix-e2e-*.json")) if directory.is_dir() else []:
            entry["hermes_sha"] = json.loads(path.read_text(encoding="utf-8")).get("hermes_sha", "") or ""
        if not junit.is_file():
            findings.append(_finding("infrastructure", ref, "no-results",
                                     "the contract job produced no test results (checkout, environment or runner)"))
            continue
        try:
            entry["tests"] = read_junit(junit)
        except ET.ParseError as exc:
            findings.append(_finding("infrastructure", ref, "junit", f"unreadable test results: {exc}"))
            continue
        if not entry["tests"]:
            findings.append(_finding("infrastructure", ref, "no-tests", "the contract run collected no tests"))
        for case in entry["tests"]:
            if case["state"] == "passed":
                continue
            nodeid = f"{case['classname']}::{case['name']}"
            findings.append(_finding(test_category(case["classname"], case["name"]), ref,
                                     f"test:{nodeid}:{case['state']}",
                                     f"{nodeid} {case['state']}: {case['message'].splitlines()[0] if case['message'] else ''}"))
        for name, result in sorted(entry["interventions"].items()):
            outcome = str(result.get("outcome", "?"))
            if outcome in GOOD or (name, outcome) in EXPECTED or NOTICE.fullmatch(name):
                continue
            category = "comfort" if result.get("klass") == "K" or name.startswith("K") else "security"
            findings.append(_finding(category, ref, f"intervention:{name}:{outcome}",
                                     f"{name} {outcome}: {result.get('reason', '')}"))
    return {"refs": refs, "expect": expect, "findings": findings}


def neutral(text: Any, limit: int = TEXT_LIMIT) -> str:
    """Text from a run against upstream code, safe in an issue or summary: one line, bounded, as a code span,
    no mention or reference can trigger."""
    value = re.sub(r"[\x00-\x1f\x7f  ]+", " ", str(text))
    value = value.replace("`", "'").replace("@", "@\u200b").strip()
    if len(value) > limit:
        value = value[: limit - 1] + "…"
    return f"`{value}`" if value else "``"


def render_summary(report: dict) -> str:
    refs = list(report["refs"])
    names = sorted({n for entry in report["refs"].values() for n in entry["interventions"]})
    lines = ["## Hermes compatibility", ""]
    if refs:
        lines += ["| intervention | " + " | ".join(neutral(r) for r in refs) + " |", "|---|" + "---|" * len(refs)]
        for name in names:
            cells = [neutral(report["refs"][r]["interventions"].get(name, {}).get("outcome", "—")) for r in refs]
            lines.append(f"| {neutral(name)} | " + " | ".join(cells) + " |")
        counts = []
        for ref in refs:
            tests = report["refs"][ref]["tests"]
            failed = sum(1 for t in tests if t["state"] != "passed")
            counts.append(f"{neutral(ref)}: {len(tests) - failed}/{len(tests)} contract tests passed")
        lines += ["", *[f"- {c}" for c in counts]]
    for category in CATEGORIES:
        items = [f for f in report["findings"] if f["category"] == category]
        lines += ["", f"### {category}: " + (f"{len(items)} finding(s)" if items else "none")]
        lines += [f"- {neutral(f['ref'])} {neutral(f['text'])}" for f in items]
    return "\n".join(lines) + "\n"


def cmd_summarize(args: argparse.Namespace) -> int:
    expect = [r for r in (args.expect or "").split(",") if r]
    report = summarize(args.artifacts, expect, args.refs_result)
    args.out.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    text = render_summary(report)
    if args.summary:
        args.summary.write_text(text, encoding="utf-8")
    print(text)
    return 0


# ── issues ───────────────────────────────────────────────────────────────────────────────────────────


def fingerprint(findings: list[dict]) -> str:
    """Identity of a finding set: refs and what failed, never shas, pids, paths or reason texts."""
    keys = sorted({f"{f['ref']}|{f['key']}" for f in findings})
    return hashlib.sha256("\n".join(keys).encode("utf-8")).hexdigest()[:16] if keys else "none"


def last_marker(texts: Iterable[str], category: str) -> Optional[str]:
    found = None
    for text in texts:
        for match in MARKER.finditer(text or ""):
            if match.group(1) == category:
                found = match.group(2)
    return found


def untested(report: dict) -> list[str]:
    """Refs this run could not test (``-``: the ref selection itself failed)."""
    return sorted({f["ref"] for f in report["findings"] if f["category"] == "infrastructure"})


def plan(category: str, findings: list[dict], issue: Optional[dict], *, complete: bool = True) -> Optional[str]:
    """``create`` | ``comment`` | ``close`` | None for one category, given its open issue (``{"number",
    "texts"}``: body and comments in order) or None. ``complete`` is False when a ref went untested: nothing
    tested is not "resolved", so only the infrastructure issue may then close."""
    fp = fingerprint(findings)
    if not findings:
        if issue is None or not (complete or category == "infrastructure"):
            return None
        return "close"
    if issue is None:
        return "create"
    return None if last_marker(issue["texts"], category) == fp else "comment"


def render_issue(category: str, findings: list[dict], report: dict, run_url: str,
                 untested: Iterable[str] = ()) -> str:
    fp = fingerprint(findings)
    lines = [f"<!-- dsp-compat:{category}:{fp} -->"]
    if findings:
        lines.append(f"The daily compatibility run against Hermes found {len(findings)} {category} finding(s).")
    else:
        lines.append(f"The {category} findings are gone in the latest run.")
    gaps = ["the ref selection" if ref == "-" else neutral(ref) for ref in untested]
    if gaps:
        lines.append(f"Not tested in this run: {', '.join(gaps)} (see \"{TITLES['infrastructure']}\").")
    if run_url:
        lines.append(f"Run: {run_url}")
    lines.append("")
    for f in findings:
        sha = (report["refs"].get(f["ref"]) or {}).get("hermes_sha", "")
        lines.append(f"- {neutral(f['ref'])}{' @ ' + neutral(sha[:10]) if sha else ''}: {neutral(f['text'])}")
    return "\n".join(lines) + "\n"


def _gh(*args: str) -> str:
    return subprocess.run(["gh", *args], check=True, capture_output=True, text=True).stdout


def _open_issue(repo: str, title: str) -> Optional[dict]:
    found = json.loads(_gh("issue", "list", "--repo", repo, "--state", "open", "--search", f'"{title}" in:title',
                           "--json", "number,title,body", "--limit", "20") or "[]")
    for issue in found:
        if issue.get("title") == title:
            view = json.loads(_gh("issue", "view", str(issue["number"]), "--repo", repo, "--json", "comments"))
            texts = [issue.get("body") or ""] + [c.get("body") or "" for c in view.get("comments") or []]
            return {"number": issue["number"], "texts": texts}
    return None


def cmd_issues(args: argparse.Namespace) -> int:
    report = json.loads(args.report.read_text(encoding="utf-8"))
    gaps = untested(report)
    for category in CATEGORIES:
        findings = [f for f in report["findings"] if f["category"] == category]
        title = TITLES[category]
        issue = None if args.dry_run else _open_issue(args.repo, title)
        action = plan(category, findings, issue, complete=not gaps)
        body = render_issue(category, findings, report, args.run_url,
                            untested=() if category == "infrastructure" else gaps)
        print(f"{category}: {action or 'nothing to do'}")
        if args.dry_run or action is None:
            continue
        if action == "create":
            _gh("issue", "create", "--repo", args.repo, "--title", title, "--body", body)
        elif action == "comment":
            _gh("issue", "comment", str(issue["number"]), "--repo", args.repo, "--body", body)
        elif action == "close":
            _gh("issue", "close", str(issue["number"]), "--repo", args.repo, "--comment", body)
    return 0


def cmd_verdict(args: argparse.Namespace) -> int:
    report = json.loads(args.report.read_text(encoding="utf-8"))
    red = [f for f in report["findings"] if f["category"] in ("security", "infrastructure")]
    for f in report["findings"]:
        level = "error" if f in red else "warning"
        print(f"::{level}::{f['category']} {f['ref']}: {neutral(f['text'], 200)}")
    return 1 if red else 0


def main(argv: Optional[list[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)
    refs = sub.add_parser("refs")
    refs.add_argument("releases", type=Path)
    refs.add_argument("--count", type=int, default=3)
    refs.add_argument("--github-output")
    summ = sub.add_parser("summarize")
    summ.add_argument("artifacts", type=Path)
    summ.add_argument("--expect", default="")
    summ.add_argument("--refs-result", default="success")
    summ.add_argument("--out", type=Path, required=True)
    summ.add_argument("--summary", type=Path)
    iss = sub.add_parser("issues")
    iss.add_argument("report", type=Path)
    iss.add_argument("--repo", required=True)
    iss.add_argument("--run-url", default="")
    iss.add_argument("--dry-run", action="store_true")
    ver = sub.add_parser("verdict")
    ver.add_argument("report", type=Path)
    args = parser.parse_args(argv)
    return {"refs": cmd_refs, "summarize": cmd_summarize, "issues": cmd_issues, "verdict": cmd_verdict}[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
