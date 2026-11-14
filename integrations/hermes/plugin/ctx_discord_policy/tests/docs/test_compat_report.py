"""The daily upstream compatibility run: ref selection, finding categories, issue updates, neutral text."""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


@pytest.fixture(scope="module")
def report():
    spec = importlib.util.spec_from_file_location("dsp_compat_report", ROOT / "ci" / "compat_report.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


WORKFLOW = "hermes-ctx-discord-policy-upstream.yml"


def _workflow() -> Path:
    """The daily run's workflow: in the ctx repository's .github/workflows, above the plugin directory."""
    for base in (ROOT, *ROOT.parents):
        candidate = base / ".github" / "workflows" / WORKFLOW
        if candidate.is_file():
            return candidate
    raise AssertionError(f".github/workflows/{WORKFLOW} not found above {ROOT}")


def _release(tag, published, *, draft=False, prerelease=False):
    return {"tag_name": tag, "published_at": published, "draft": draft, "prerelease": prerelease}


def test_refs_are_the_last_published_releases_not_calendar_tags(report, tmp_path):
    releases = [
        _release("v0.23.1+canary.20261113T064447Z", "2026-11-13T08:06:48Z", prerelease=True),
        _release("v0.23.1", "2026-11-10T01:42:15Z"),
        _release("v0.23.0", "2026-11-05T21:32:33Z"),
        _release("v0.24.0", "2026-11-12T00:00:00Z", draft=True),
        _release("v0.22.7", "2026-11-03T21:53:16Z"),
        _release("v0.22.6", "2026-11-02T17:18:36Z"),
        _release("v2026.9.24", "2026-09-24T10:09:38Z"),   # sorts above every v0.x by version
        _release("v2026.9.21", "2026-09-21T18:10:55Z"),
        _release("nightly", "2026-11-14T00:00:00Z"),
    ]
    assert report.pick_refs(releases) == ["main", "v0.23.1", "v0.23.0", "v0.22.7"]
    path = tmp_path / "releases.jsonl"
    path.write_text("\n".join(json.dumps(r) for r in releases) + "\n", encoding="utf-8")
    out = tmp_path / "github_output"
    assert report.main(["refs", str(path), "--github-output", str(out)]) == 0
    assert out.read_text(encoding="utf-8").splitlines() == [
        'matrix=["main", "v0.23.1", "v0.23.0", "v0.22.7"]', "expect=main,v0.23.1,v0.23.0,v0.22.7"]
    path.write_text(json.dumps(releases[0]) + "\n" + json.dumps(releases[3]) + "\n", encoding="utf-8")
    assert report.main(["refs", str(path)]) == 1  # no published release: the refs job fails (and is reported)


def _junit(directory: Path, cases):
    directory.mkdir(parents=True, exist_ok=True)
    rows = []
    for classname, name, state in cases:
        inner = {"failed": '<failure message="assert False @someone">trace</failure>',
                 "error": '<error message="boom"/>', "skipped": '<skipped message="skip"/>'}.get(state, "")
        rows.append(f'<testcase classname="{classname}" name="{name}">{inner}</testcase>')
    (directory / "junit.xml").write_text(f'<testsuites><testsuite name="pytest">{"".join(rows)}</testsuite></testsuites>',
                                         encoding="utf-8")


def _matrix(directory: Path, part: str, ref: str, outcomes: dict):
    data = {"hermes_ref": ref, "hermes_sha": "a" * 40,
            "interventions": {name: {"outcome": o, "klass": "K" if name.startswith("K") else "S", "reason": f"why {name}"}
                              for name, o in outcomes.items()}}
    (directory / f"status-matrix-e2e-{part}-{ref}.json").write_text(json.dumps(data), encoding="utf-8")


def _findings(rep, category):
    return sorted((f["ref"], f["key"]) for f in rep["findings"] if f["category"] == category)


def test_findings_are_sorted_into_security_comfort_and_infrastructure(report, tmp_path):
    good = tmp_path / "compat-main"
    _junit(good, [("tests.contract.test_security_contract", "test_unarmed_closes", "passed"),
                  ("tests.contract.test_end_to_end", "test_end_to_end_security", "passed")])
    _matrix(good, "security", "main", {"A5": "wrapped", "A4": "fallback", "A9": "ok", "A9[b/g/public]": "fallback"})
    _matrix(good, "comfort", "main", {"K1": "injected", "K2": "wrapped"})

    bad = tmp_path / "compat-v0.23.1"
    _junit(bad, [("tests.contract.test_security_contract", "test_tool_block_in_routed_profile", "failed"),
                 ("tests.contract.test_two_homes", "test_unarmed", "skipped"),
                 ("tests.contract.test_end_to_end", "test_end_to_end_comfort", "failed"),
                 ("tests.contract.test_delivery_contract", "test_split[x]", "error"),
                 ("tests.contract.test_new_thing", "test_something", "failed")])
    _matrix(bad, "security", "v0.23.1", {"A5": "off", "A4": "off", "A9": "ok"})
    _matrix(bad, "comfort", "v0.23.1", {"A5": "wrapped", "K1": "fallback", "K6": "off"})  # degraded wins on merge
    (tmp_path / "compat-v0.23.0").mkdir()                                            # job died before pytest

    rep = report.summarize(tmp_path, ["main", "v0.23.1", "v0.23.0"])
    assert _findings(rep, "security") == sorted([
        ("v0.23.1", "intervention:A4:off"), ("v0.23.1", "intervention:A5:off"),
        ("v0.23.1", "test:tests.contract.test_new_thing::test_something:failed"),
        ("v0.23.1", "test:tests.contract.test_security_contract::test_tool_block_in_routed_profile:failed"),
        ("v0.23.1", "test:tests.contract.test_two_homes::test_unarmed:skipped")])
    assert _findings(rep, "comfort") == sorted([
        ("v0.23.1", "intervention:K1:fallback"), ("v0.23.1", "intervention:K6:off"),
        ("v0.23.1", "test:tests.contract.test_delivery_contract::test_split[x]:error"),
        ("v0.23.1", "test:tests.contract.test_end_to_end::test_end_to_end_comfort:failed")])
    assert _findings(rep, "infrastructure") == [("v0.23.0", "no-results")]
    assert not any(f["ref"] == "main" for f in rep["findings"])  # A4 fallback (own case), A9[...] notice
    summary = report.render_summary(rep)
    assert "`A9[b/g/public]`" in summary and "2/2 contract tests passed" in summary

    missing = report.summarize(tmp_path, [], "failure")
    assert _findings(missing, "infrastructure") == [("-", "refs")]  # a failed refs job is never "no drift"


def test_a_degraded_outcome_wins_whatever_the_file_order(report, tmp_path):
    for first, second in (("wrapped", "off"), ("off", "wrapped")):
        directory = tmp_path / f"compat-{first}"
        directory.mkdir()
        _matrix(directory, "a", "main", {"A5": first})
        _matrix(directory, "b", "main", {"A5": second})
        assert report.read_matrices(directory)["A5"]["outcome"] == "off"


def test_issue_is_written_only_when_the_findings_change(report):
    one = [{"category": "security", "ref": "main", "key": "intervention:A5:off", "text": "A5 off: pid 123"}]
    same = [{"category": "security", "ref": "main", "key": "intervention:A5:off", "text": "A5 off: pid 456"}]
    more = one + [{"category": "security", "ref": "v0.23.1", "key": "intervention:A6:off", "text": "A6"}]
    assert report.plan("security", one, None) == "create"
    body = report.render_issue("security", one, {"refs": {}}, "https://example.invalid/run/1")
    issue = {"number": 7, "texts": [body]}
    assert report.plan("security", same, issue) is None               # unchanged (volatile text ignored)
    assert report.plan("security", more, issue) == "comment"
    issue["texts"].append(report.render_issue("security", more, {"refs": {}}, ""))
    assert report.plan("security", more, issue) is None
    assert report.plan("security", one, issue) == "comment"            # back to the earlier set is a change
    assert report.plan("security", [], issue) == "close"
    assert report.plan("security", [], None) is None
    assert report.plan("comfort", one, {"number": 8, "texts": [body]}) == "comment"  # markers are per category


def test_an_incomplete_run_never_closes_a_drift_issue(report, tmp_path):
    """Nothing tested is not "resolved": a failed ref selection or a ref without results keeps the security
    and comfort issues open, and a comment names what was not tested."""
    finding = {"category": "security", "ref": "v0.23.0", "key": "intervention:A5:off", "text": "A5 off"}
    issue = {"number": 1, "texts": [report.render_issue("security", [finding], {"refs": {}}, "")]}

    no_refs = report.summarize(tmp_path, [], "failure")
    assert report.untested(no_refs) == ["-"]
    for category in ("security", "comfort"):
        assert report.plan(category, [], issue, complete=False) is None
    assert report.plan("infrastructure", [f for f in no_refs["findings"]], None, complete=False) == "create"

    good = tmp_path / "compat-main"
    _junit(good, [("tests.contract.test_security_contract", "test_unarmed_closes", "passed")])
    _matrix(good, "security", "main", {"A5": "wrapped"})
    (tmp_path / "compat-v0.23.0").mkdir()                                  # that job died before pytest
    partial = report.summarize(tmp_path, ["main", "v0.23.0"])
    assert report.untested(partial) == ["v0.23.0"] and _findings(partial, "security") == []
    assert report.plan("security", [], issue, complete=False) is None
    assert report.plan("security", [], issue, complete=True) == "close"
    other = [{"category": "security", "ref": "main", "key": "intervention:A6:off", "text": "A6 off"}]
    assert report.plan("security", other, issue, complete=False) == "comment"  # new findings still arrive
    body = report.render_issue("security", other, partial, "", untested=report.untested(partial))
    assert "Not tested in this run: `v0.23.0`" in body
    body = report.render_issue("security", other, no_refs, "", untested=report.untested(no_refs))
    assert "Not tested in this run: the ref selection" in body


def test_text_from_upstream_runs_is_neutralised(report):
    hostile = "ping @everyone and @maintainer ```\n# Heading\n<img src=x> [link](https://x) #12"
    text = report.neutral(hostile)
    assert text.startswith("`") and text.endswith("`") and text.count("`") == 2
    assert "@everyone" not in text and "@maintainer" not in text and "\n" not in text
    assert len(report.neutral("x" * 5000)) <= report.TEXT_LIMIT + 2
    body = report.render_issue("security", [{"category": "security", "ref": "main", "key": "k", "text": hostile}],
                               {"refs": {"main": {"hermes_sha": "@sha" + "0" * 40}}}, "")
    assert "@everyone" not in body and "@sha" not in body


def test_verdict_turns_the_run_red_for_security_and_infrastructure_only(report, tmp_path, capsys):
    path = tmp_path / "report.json"
    for findings, code in (([], 0), ([{"category": "comfort", "ref": "main", "key": "k", "text": "K2 fallback"}], 0),
                           ([{"category": "security", "ref": "main", "key": "k", "text": "A5 off"}], 1),
                           ([{"category": "infrastructure", "ref": "-", "key": "refs", "text": "refs"}], 1)):
        path.write_text(json.dumps({"refs": {}, "expect": [], "findings": findings}), encoding="utf-8")
        assert report.main(["verdict", str(path)]) == code
    assert "::error::infrastructure" in capsys.readouterr().out


def test_issues_carry_label_and_title_prefix_in_a_shared_repository(report, tmp_path, monkeypatch):
    path = tmp_path / "report.json"
    path.write_text(json.dumps({"refs": {}, "expect": ["main"], "findings": [
        {"category": "security", "ref": "main", "key": "k", "text": "A5 off"}]}), encoding="utf-8")
    calls = []

    def gh(*args):
        calls.append(args)
        return "[]"

    monkeypatch.setattr(report, "_gh", gh)
    assert report.main(["issues", str(path), "--repo", "o/r", "--label", "plugin-x",
                        "--title-prefix", "plugin-x: "]) == 0
    listed = [c for c in calls if c[:2] == ("issue", "list")]
    created = [c for c in calls if c[:2] == ("issue", "create")]
    assert len(listed) == 3 and all(c[c.index("--label") + 1] == "plugin-x" for c in listed)
    assert all('"plugin-x: ' in c[c.index("--search") + 1] for c in listed)
    assert len(created) == 1 and created[0][created[0].index("--title") + 1] == "plugin-x: Upstream drift: security"
    assert created[0][created[0].index("--label") + 1] == "plugin-x"
    calls.clear()
    assert report.main(["issues", str(path), "--repo", "o/r"]) == 0
    assert not any("--label" in c for c in calls)
    assert [c[c.index("--title") + 1] for c in calls if c[:2] == ("issue", "create")] == ["Upstream drift: security"]


def test_workflow_wires_the_report(report):
    text = _workflow().read_text(encoding="utf-8")
    assert "repos/NousResearch/hermes-agent/releases" in text and "git ls-remote" not in text
    assert 'CONTRACT_OUT: ${{ github.workspace }}/out/compat-${{ matrix.ref }}' in text
    assert '--junitxml="$CONTRACT_OUT/junit.xml"' in text and "continue-on-error" not in text
    # One artifact per ref, each holding its own compat-<ref>/ directory, merged on download: with a single
    # artifact (the other jobs died before pytest) download-artifact would otherwise unpack it flat.
    assert "path: out/" in text and "merge-multiple: true" in text
    assert "concurrency:" in text and "REFS_RESULT: ${{ needs.refs.result }}" in text
    assert text.count("issues: write") == 1
    assert '--label ctx_discord_policy --title-prefix "ctx_discord_policy: "' in text
    for step in ("summarize", "issues", "verdict"):
        assert f"ci/compat_report.py {step}" in text
