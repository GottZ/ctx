"""D, round G′: the target's workspace root (V40, R29), the excluded working directory (V41, R30), the planning
fake's ``is_partial``, README claims #11/#53, the note wishes and the layer move of ``envfacts``.

Lab findings covered without Hermes: L3-F01 (``workspace_root`` inside the Hermes home when HOME = HERMES_HOME, as
in the official Docker image) and L3-F02 (the session's working directory left out silently, ``--yes`` going on).
"""

from __future__ import annotations

import json
import os
import subprocess
from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path

import pytest

from ctx_handover.engine import collector, envfacts, note, pathmap, plan, session_import
from ctx_handover.engine.model import (
    Candidate,
    DestMode,
    HermesPaths,
    Include,
    ItemKind,
    Options,
    Origin,
    PlanHints,
    RefineReport,
    Weight,
)
from ctx_handover.engine.policy import PolicyConfig, check_target_dest
from ctx_handover.tests.fakes.planning_fake import (
    Busy,
    CeilingGitProbe,
    StaticGitProbe,
    decision,
    fake_redact,
    final_plan,
    git_init,
    host_facts,
    static_discover,
)
from ctx_handover.tests.unit.D.conftest import write
from ctx_handover.tests.unit.D.test_note import _compose

NOW = datetime(2026, 11, 9, 12, 0, 0, tzinfo=UTC)


# ---------------------------------------------------------------------------------------------------------
# Layouts
# ---------------------------------------------------------------------------------------------------------


def _docker(tmp_path: Path) -> tuple[Path, Path]:
    """``(home, hermes_home)`` of the official image: ``useradd -d /opt/data`` and ``HERMES_HOME=/opt/data``."""
    data = tmp_path / "opt-data"
    data.mkdir()
    return data, data


def _plain(tmp_path: Path) -> tuple[Path, Path]:
    home = tmp_path / "home-u"
    hermes = home / ".hermes"
    hermes.mkdir(parents=True)
    return home, hermes


def _cfg(home: Path, hermes: Path, options: Options | None = None) -> PolicyConfig:
    return PolicyConfig.from_options(
        options or Options(),
        home=home,
        hermes_home=hermes,
        hermes_root=home.parent / "hermes-agent",
        plugin_dir=hermes / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
    )


def _no_tools(tmp_path: Path, monkeypatch) -> None:
    empty = tmp_path / "no-tools"
    empty.mkdir(exist_ok=True)
    monkeypatch.setenv("PATH", str(empty))


def _probe(tmp_path, home: Path, hermes: Path, options: Options | None = None, *, system=None, cfg=None, **kw):
    s = system or replace(envfacts.default_system(), home=lambda: str(home))
    host = host_facts(hermes_home=str(hermes), document_cache=str(hermes / "cache" / "documents"))
    return envfacts.probe(
        instance_name="laptop",
        options=options or Options(),
        host=host,
        config={},
        system=s,
        scratch_dir=tmp_path,
        cfg=cfg,
        **kw,
    )


# ---------------------------------------------------------------------------------------------------------
# V40 / R29 — the default root and the target's own check (L3-F01)
# ---------------------------------------------------------------------------------------------------------


def test_gp_default_root_inside_hermes_home_is_the_reserved_directory(tmp_path, monkeypatch, user_home):
    """★ L3-F01: with HOME = HERMES_HOME the default ``~/hermes-handover`` would lie in the Hermes home (target
    denylist ⇒ every commit aborts). The target's facts name ``<HERMES_HOME>/handover-workspaces`` as its root and
    allow root instead, and report it usable.

    Red before G′: ``workspace_root`` was ``<home>/hermes-handover`` (``expand_root`` only)."""
    _no_tools(tmp_path, monkeypatch)
    home, hermes = _docker(tmp_path)
    facts = _probe(tmp_path, home, hermes)
    reserved = str(hermes / "handover-workspaces")
    assert facts.workspace_root == reserved
    assert facts.allow_roots == (reserved,)
    assert facts.workspace_root_ok is True and facts.workspace_root_problem == ""
    assert envfacts.workspace_root_message(facts) is None


def test_gp_default_root_outside_hermes_home_stays(tmp_path, monkeypatch, user_home):
    """The usual layout keeps ``~/hermes-handover`` (R29 changes only the Docker-like case)."""
    _no_tools(tmp_path, monkeypatch)
    home, hermes = _plain(tmp_path)
    facts = _probe(tmp_path, home, hermes)
    assert facts.workspace_root == str(home / "hermes-handover")
    assert facts.allow_roots == (str(home / "hermes-handover"),)
    assert facts.workspace_root_ok is True


def test_gp_configured_root_inside_hermes_home_is_reported_unusable(tmp_path, monkeypatch, user_home):
    """★ R29: an explicitly configured root inside the Hermes home stays denied — the target says so in its facts
    (checked at the first contact, not at the commit), and the message names the setting ``workspace_root``.

    Red before G′: the facts carried no verdict at all (L3-F01: the denial surfaced only at ``c2:cwd``)."""
    _no_tools(tmp_path, monkeypatch)
    home, hermes = _docker(tmp_path)
    root = str(hermes / "work")
    facts = _probe(tmp_path, home, hermes, Options(workspace_root=root, allow_roots=(root,)))
    assert facts.workspace_root == root
    assert facts.workspace_root_ok is False
    assert facts.workspace_root_problem == "denied"
    message = envfacts.workspace_root_message(facts)
    assert message is not None and "workspace_root" in message and root in message


@pytest.mark.parametrize(
    ("case", "problem"),
    [("outside-allow-roots", "not_allowed_root"), ("a-file", "not_directory"), ("under-a-file", "not_directory")],
)
def test_gp_root_problems(case, problem, tmp_path, monkeypatch, user_home):
    """Each way a root is unusable has its code (``envfacts.ROOT_PROBLEMS``); a missing root under a usable parent
    is fine (the commit creates it, R11)."""
    _no_tools(tmp_path, monkeypatch)
    home, hermes = _plain(tmp_path)
    allowed = home / "recv"
    if case == "outside-allow-roots":
        root = home / "elsewhere"
    elif case == "a-file":
        root = write(allowed / "ws")
    else:
        write(allowed / "file")
        root = allowed / "file" / "ws"
    options = Options(workspace_root=str(root), allow_roots=(str(allowed),))
    facts = _probe(tmp_path, home, hermes, options)
    assert (facts.workspace_root_ok, facts.workspace_root_problem) == (False, problem)
    assert problem in envfacts.ROOT_PROBLEMS
    fresh = Options(workspace_root=str(allowed / "new" / "ws"), allow_roots=(str(allowed),))
    fine = _probe(tmp_path, home, hermes, fresh)
    assert (fine.workspace_root_ok, fine.workspace_root_problem) == (True, "")


def test_gp_root_not_writable_and_check_failure_are_unusable(tmp_path, monkeypatch, user_home):
    """★ Fail-closed: a root the process cannot write (``os.access``, a seam of ``SystemOps``) is ``not_writable``;
    a check that raises reports ``check_failed`` — never ``ok``, and the probe itself does not fail.

    Mutant (red proof): ``check_workspace_root`` returning ``""`` from its ``except`` branch."""
    _no_tools(tmp_path, monkeypatch)
    home, hermes = _plain(tmp_path)
    base = replace(envfacts.default_system(), home=lambda: str(home))
    facts = _probe(tmp_path, home, hermes, system=replace(base, access=lambda _p, _m: False))
    assert (facts.workspace_root_ok, facts.workspace_root_problem) == (False, "not_writable")

    def boom(_p, _m):
        raise RuntimeError("probe broke")

    facts = _probe(tmp_path, home, hermes, system=replace(base, access=boom))
    assert (facts.workspace_root_ok, facts.workspace_root_problem) == (False, "check_failed")


def test_gp_root_check_uses_the_given_policy(tmp_path, monkeypatch, user_home):
    """With ``cfg`` (the target's own policy, what A passes) the check uses that policy, not one built from the
    options: a root the given policy does not allow is ``not_allowed_root`` even when the options would."""
    _no_tools(tmp_path, monkeypatch)
    home, hermes = _plain(tmp_path)
    narrow = _cfg(home, hermes, Options(allow_roots=(str(home / "only-here"),)))
    facts = _probe(tmp_path, home, hermes, cfg=narrow)
    assert (facts.workspace_root_ok, facts.workspace_root_problem) == (False, "not_allowed_root")


def test_gp_pathmap_resolves_the_default_root(tmp_path):
    """``pathmap.resolve_root``: the one resolver of configured roots in D (envfacts, ``fallback_dest``)."""
    home, hermes = _docker(tmp_path)
    assert pathmap.resolve_root("~/hermes-handover", home=str(home), hermes_home=str(hermes)) == str(
        hermes / "handover-workspaces"
    )
    assert pathmap.resolve_root(" ~/hermes-handover ", home=str(home), hermes_home=str(hermes)) == str(
        hermes / "handover-workspaces"
    )
    spelled = str(home / "hermes-handover")  # any spelling of the default, as the policy reads it (K0)
    assert pathmap.resolve_root(spelled, home=str(home), hermes_home=str(hermes)) == str(hermes / "handover-workspaces")
    assert pathmap.resolve_root("~/other", home=str(home), hermes_home=str(hermes)) == str(home / "other")
    assert pathmap.resolve_root("~user/x", home=str(home), hermes_home=str(hermes)) is None
    assert pathmap.resolve_root("", home=str(home), hermes_home=str(hermes)) is None
    plain_home, plain_hermes = _plain(tmp_path)
    assert pathmap.resolve_root("~/hermes-handover", home=str(plain_home), hermes_home=str(plain_hermes)) == str(
        plain_home / "hermes-handover"
    )


def test_gp_fallback_dest_in_docker_layout_lands_in_the_reserved_directory(tmp_path):
    """★ V35 + R29: the conflict fallback for the default settings in the Docker layout lies in
    ``<HERMES_HOME>/handover-workspaces`` and passes the target check.

    Red before G′: ``<home>/hermes-handover/proj-handover-<stamp>`` — inside the Hermes home, ``target_denied``."""
    home, hermes = _docker(tmp_path)
    cfg = _cfg(home, hermes)
    first = str(hermes / "handover-workspaces" / "team" / "proj")
    out = pathmap.fallback_dest(
        first, now=NOW, workspace_root="~/hermes-handover", allow_roots=("~/hermes-handover",), cfg=cfg
    )
    assert out == f"{first}-handover-20261109-120000"  # the sibling: its parent lies under the resolved allow root
    assert check_target_dest(Path(out), cfg, recorded=False, local_choice=False).allowed
    elsewhere = pathmap.fallback_dest(
        str(home / "outside" / "proj"), now=NOW, workspace_root="~/hermes-handover", allow_roots=(), cfg=cfg
    )
    assert elsewhere == str(hermes / "handover-workspaces" / "proj-handover-20261109-120000")


# ---------------------------------------------------------------------------------------------------------
# V41 / R30 — the excluded working directory (L3-F02)
# ---------------------------------------------------------------------------------------------------------


def _build(cfg: PolicyConfig, cwd: Path, hints: PlanHints | None = None):
    cand = Candidate(ItemKind.FILE_TREE, os.path.realpath(cwd), Weight.MENTION, ("cwd",), Origin.CWD)
    rp = plan.build_plan(
        "01JCJOB",
        (1, 2),
        [cand],
        None,
        RefineReport(used="off"),
        hints or PlanHints(),
        cfg,
        lambda _root: None,
        Busy(),
        (),
        Options(),
        1000,
        redact=fake_redact,
    )
    decisions = [
        decision(i, f"/home/u/hermes-handover/{Path(i.source_path).name}", mode=DestMode.SKIP)
        if i.include is Include.NO
        else decision(i, f"/home/u/hermes-handover/{Path(i.source_path).name}")
        for i in rp.items
    ]
    return final_plan(rp, decisions)


def test_gp_cwd_in_hermes_home_is_named_and_needs_an_answer(lay):
    """★ L3-F02 / R30: a session whose working directory lies in the Hermes home plans ``w0`` as ``no`` (policy
    ``hermes``). The pre-send summary says so (``workspace_excluded``), the confirmation reason
    ``workspace_excluded`` (letter h) is raised, and a blanket yes does not cover it (``plan.yes_exempt``).

    Red before G′: summary without the key, no reason ⇒ ``--yes`` went on with "0 items, 0 B"."""
    cwd = lay.hermes_home / "work" / "proj"
    write(cwd / "main.py")
    final = _build(lay.cfg(), cwd)
    w0 = final.plan.items[0]
    assert w0.include is Include.NO
    summary = plan.presend_summary(final)
    assert summary["workspace_excluded"] == {"path": os.path.realpath(cwd), "reason": "hermes", "why": "hermes"}
    assert "workspace_excluded" in plan.PRESEND_KEYS
    reasons = plan.confirmation_reasons(final, Options(), False)
    assert reasons == [plan.CONFIRM_WORKSPACE_EXCLUDED] == ["workspace_excluded"]
    assert plan.CONFIRM_LETTERS["workspace_excluded"] == "h"
    assert plan.REASON_CODES[7] == "workspace_excluded"  # G‴: (i) context_over follows (R32), (j) R36
    assert plan.yes_exempt(reasons) == ["workspace_excluded"]
    assert plan.yes_exempt(["proposed", "large", "model", "include_secret"]) == []
    confirmed = plan.apply_choice(final, "yes")  # the excluded workspace never becomes "yes"
    assert confirmed.plan.items[0].include is Include.NO


def test_gp_cwd_home_and_no_workspace(lay):
    """A home-directory cwd is named too (reason ``home``); the user's own ``--no-workspace`` is the explicit
    answer: the summary still names the directory, the reason is not raised."""
    final = _build(lay.cfg(), lay.home)
    excluded = plan.presend_summary(final)["workspace_excluded"]
    assert excluded is not None and excluded["reason"] == "home" and excluded["path"] == os.path.realpath(lay.home)
    assert "workspace_excluded" in plan.confirmation_reasons(final, Options(), False)
    told = Options(hints=PlanHints(no_workspace=True))
    assert "workspace_excluded" not in plan.confirmation_reasons(final, told, False)


def test_gp_included_or_absent_workspace_is_not_excluded(lay):
    """No key value and no reason when the workspace comes along, when the user dropped it (``--no-workspace``
    is not a policy exclusion) or when the session has no working directory."""
    final = _build(lay.cfg(), lay.project)
    assert plan.presend_summary(final)["workspace_excluded"] is None
    assert "workspace_excluded" not in plan.confirmation_reasons(final, Options(), False)
    dropped = _build(lay.cfg(), lay.project, PlanHints(no_workspace=True))
    assert dropped.plan.items[0].include is Include.NO
    assert plan.presend_summary(dropped)["workspace_excluded"] is None
    rp = plan.build_plan(
        "01JCJOB", (1, 2), [], None, RefineReport(used="off"), PlanHints(), lay.cfg(), lambda _r: None, Busy(), (),
        Options(), 0,
    )  # fmt: skip
    assert plan.workspace_excluded(rp) is None


def test_gp_reserved_directory_is_a_source(lay):
    """★ V46 (R29 on the source side, return trip R4 from a Docker install): a working directory and a transcript
    path under ``<HERMES_HOME>/handover-workspaces`` are collected and planned like any workspace; every other
    Hermes path stays out (and a cwd there falls under R30).

    Red before G′: the collector skipped every path inside the Hermes home (``hermes_owned``) — the extra
    ``notes.md`` beside the workspace never became a candidate."""
    reserved = lay.hermes_home / "handover-workspaces"
    proj = reserved / "proj"
    write(proj / "src" / "main.py")
    notes = write(reserved / "notes.md")
    other = write(lay.hermes_home / "work" / "x.md")
    (reserved / "link").symlink_to(lay.hermes_home / "work")  # a link out of the reserved directory stays out
    cache = write(reserved / "doc-cache" / "a.txt")  # the exception lifts the Hermes home only, never a cache
    calls = [
        {"id": f"c{n}", "function": {"name": "write_file", "arguments": json.dumps({"path": str(p)})}}
        for n, p in enumerate((notes, other, reserved / "link" / "x.md", cache))
    ]
    cands = collector.collect(
        [{"role": "assistant", "session_id": "s1", "tool_calls": calls}],
        {"id": "s1", "cwd": str(proj)},
        HermesPaths(home=str(lay.hermes_home), document_cache=str(reserved / "doc-cache")),
        lay.cfg(),
        StaticGitProbe({}),
        source_os="linux",
        discover=static_discover([]),
    )
    paths = {c.path: c for c in cands}
    assert paths[os.path.realpath(proj)].origin is Origin.CWD
    assert os.path.realpath(notes) in paths
    for left in (other, cache):  # M20: named as left behind (``rejected: hermes``), never a source
        found = paths.get(os.path.realpath(left))
        assert found is None or collector.rejected_reason(found.reasons) == "hermes", found
    final = _build(lay.cfg(), proj)
    assert final.plan.items[0].include is Include.YES
    assert plan.presend_summary(final)["workspace_excluded"] is None


# ---------------------------------------------------------------------------------------------------------
# Planning fake and README claims (wishes)
# ---------------------------------------------------------------------------------------------------------


def test_gp_ceiling_probe_reports_partial_clones(lay, tmp_path):
    """★ V30: ``GitProbe.is_partial`` is part of the contract; the real-git fake answers it and the collector
    asks it directly — a partial clone (promisor remote) travels as a file tree with ``PARTIAL_REASON``.

    Red before G′: ``CeilingGitProbe`` had no ``is_partial`` ⇒ the collector's ``getattr`` fallback said
    "complete" ⇒ ``git_repo``."""
    repo = git_init(lay.home / "partial")
    with open(repo / ".git" / "config", "a", encoding="utf-8") as fh:
        fh.write('[remote "origin"]\n\turl = https://example.invalid/r.git\n\tpromisor = true\n')
    probe = CeilingGitProbe(lay.home.parent)
    assert probe.is_partial(str(repo)) is True
    assert CeilingGitProbe(lay.home.parent).is_partial(str(lay.project)) is False
    cands = collector.collect(
        [],
        {"id": "s1", "cwd": str(repo)},
        HermesPaths(home=str(lay.hermes_home), document_cache=str(lay.hermes_home / "cache" / "documents")),
        lay.cfg(),
        probe,
        source_os="linux",
        discover=static_discover([]),
    )
    primary = next(c for c in cands if c.origin is Origin.CWD)
    assert primary.kind is ItemKind.FILE_TREE and collector.PARTIAL_REASON in primary.reasons


def test_gp_about_thirty_tools_are_probed_once_each(tmp_path, monkeypatch, user_home):
    """README #53 "``<tool> --version`` for a fixed list of about 30 developer tools": the allowlist has 25–35
    distinct names and a probe runs each one present exactly once (no other process).

    Mutant (red proof): a duplicate entry in ``TOOL_ALLOWLIST`` (two runs of one tool)."""
    names = envfacts.TOOL_ALLOWLIST
    assert 25 <= len(names) <= 35 and len(set(names)) == len(names)
    bindir = tmp_path / "bin"
    bindir.mkdir()
    log = tmp_path / "runs.log"
    for name in names:
        script = bindir / name
        script.write_text(f'#!/bin/sh\necho {name} >> "{log}"\necho "{name} 1.0"\n', encoding="utf-8")
        script.chmod(0o755)
    monkeypatch.setenv("PATH", f"{bindir}:/usr/bin:/bin")
    facts = envfacts.probe(
        instance_name="x", options=Options(), host=host_facts(), config={}, scratch_dir=tmp_path, cfg=None
    )
    runs = log.read_text(encoding="utf-8").split()
    assert sorted(runs) == sorted(names)
    assert all(facts.tools[n].version == f"{n} 1.0" for n in names if n not in envfacts.VERSION_ARGS)


def test_gp_remote_backend_workspace_is_not_a_source(lay, tmp_path, monkeypatch):
    """README #11 "Workspaces of remote terminal backends (docker, ssh, modal …) are not sources; the plugin moves
    files of the machine the gateway runs on": a session that worked in a container (``cwd`` and every path exist
    only there) yields no candidate, and planning starts no process (no ``docker``/``ssh`` to reach the backend).

    Mutant (red proof): the collector's ``lstat`` existence check dropped (``cls.lstat(path) is None``)."""
    inside = tmp_path / "container-only" / "workspace" / "app"
    read = json.dumps({"command": f"cat {inside}/x.py"})
    wrote = json.dumps({"path": f"{inside}/y.py"})
    rows = [
        {
            "role": "assistant",
            "session_id": "s1",
            "tool_calls": [
                {"id": "c1", "function": {"name": "terminal", "arguments": read}},
                {"id": "c2", "function": {"name": "write_file", "arguments": wrote}},
            ],
        },
        {"role": "tool", "tool_call_id": "c1", "content": json.dumps({"output": "", "cwd": str(inside)})},
    ]

    def no_process(*_a, **_k):
        raise AssertionError("planning must not start a process to reach a terminal backend")

    monkeypatch.setattr(subprocess, "Popen", no_process)
    cands = collector.collect(
        rows,
        {"id": "s1", "cwd": str(inside)},
        HermesPaths(home=str(lay.hermes_home), document_cache=str(lay.hermes_home / "cache" / "documents")),
        lay.cfg(),
        StaticGitProbe({}),
        source_os="linux",
        discover=static_discover([]),
    )
    assert cands == []


# ---------------------------------------------------------------------------------------------------------
# Note wishes
# ---------------------------------------------------------------------------------------------------------


def test_gp_note_names_withheld_by_source_policy():
    """★ V29: a pull path the source did not release keeps its reason in the note.

    Red before G′: the text was not in ``KNOWN_WHY`` ⇒ "(not transferred)"."""
    text, _report = _compose(
        note_parts={"not_transferred": [{"path": "/srv/app/secret-dir", "why": "withheld by source policy"}]}
    )
    assert "/srv/app/secret-dir (withheld by source policy)" in text


def test_gp_note_caps_each_entry_not_the_line():
    """★ Five long absolute paths in one list line — the last name stays visible (each entry is capped,
    not the joined line).

    Red before G′: ``one_line(line, 600)`` cut "secrets excluded: …/docs/sett…"."""
    deep = "/".join(["", "home", "a-rather-long-user-name", *["nested-directory-level"] * 6])
    secrets = [f"{deep}/proj-{i}/.env" for i in range(4)] + [f"{deep}/docs/settings.txt"]
    text, _report = _compose(note_parts={"secrets_excluded": secrets})
    assert "settings.txt" in text
    line = next(ln for ln in text.splitlines() if ln.startswith("- secrets excluded:"))
    assert len(line) > 600


def test_gp_note_bundle_import_sentence():
    """★ DESIGN §5.4: "every bundle import note says 'this bundle may have been imported elsewhere too'" — its own
    note part ``bundle_import`` (so the origin line stays a name).

    Red before G′: ``bundle_import`` was an unknown part (``strict`` ⇒ ValueError)."""
    assert "bundle_import" in note.NOTE_PART_KEYS
    text, _report = _compose(note_parts={"bundle_import": True})
    assert "this bundle may have been imported elsewhere too" in text
    plain, _report = _compose(note_parts={"bundle_import": False})
    assert "imported elsewhere" not in plain


# ---------------------------------------------------------------------------------------------------------
# Layer move: envfacts above session_import (V08)
# ---------------------------------------------------------------------------------------------------------


def test_gp_offline_capability_is_the_call_of_session_import(tmp_path, monkeypatch, user_home):
    """★ V08: ``EnvFacts.offline_capable`` is ``session_import.offline_capable({}, config)`` itself — one rule, no
    mirror (``envfacts`` sits above ``session_import`` since G′).

    Red before G′: the mirrored rule ignored the patched function (public host ⇒ False, no call)."""
    _no_tools(tmp_path, monkeypatch)
    calls = []

    def rule(model_config, target_config):
        calls.append((model_config, target_config))
        return True

    monkeypatch.setattr(session_import, "offline_capable", rule)
    config = {"model": {"provider": "openrouter", "base_url": "https://openrouter.ai/api/v1"}}
    facts = envfacts.probe(instance_name="x", options=Options(), host=host_facts(), config=config, scratch_dir=tmp_path)
    assert facts.offline_capable is True
    assert calls == [({}, config)]
    assert not hasattr(envfacts, "LOCAL_PROVIDERS") and not hasattr(envfacts, "_local_url")
