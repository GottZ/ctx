"""Mandatory scenario 11 (layer 2, R26, DESIGN §8.3, §11.1): environment probes and refinement leave no traces.

(a) A handover's environment probes (``envfacts.probe`` with the operator's ``tool_probe`` list) run a tool that
writes into ``$HOME``, every XDG directory and its working directory when it starts. They run in both processes —
the laptop's conductor and the server's listener — and both (throw-away) homes are byte-for-byte unchanged afterwards,
while the tool demonstrably ran.

(b) A refinement side question that overruns ``refine_timeout_s`` is abandoned: the job's plan does not contain its
late answer, the late answer writes nothing (journal, files) once it arrives, its thread ends, and later jobs never
see it — neither a job whose side question answers fresh nor one whose side question answers nothing."""

from __future__ import annotations

import os
import threading
import time
from pathlib import Path

from ctx_handover.engine import jobs
from ctx_handover.engine.jsonutil import from_wire
from ctx_handover.engine.model import FinalPlan, Mode, Options, PlanHints, Role, SessionRef
from ctx_handover.tests.fakes.planning_fake import json_reply
from ctx_handover.tests.fakes.repo_builder import dir_fingerprint
from ctx_handover.tests.integration._int3_helpers import run_job, session_in, surface
from ctx_handover.tests.integration.harness import Listener, pair

FAKECLI = """#!/bin/sh
echo "ran HOME=$HOME PPID=$PPID" >> {witness}
mkdir -p "$HOME/.fakecli" && echo run >> "$HOME/.fakecli/history"
for d in "${{XDG_CONFIG_HOME:-$HOME/.config}}" "${{XDG_CACHE_HOME:-$HOME/.cache}}" \\
         "${{XDG_DATA_HOME:-$HOME/.local/share}}" "${{XDG_STATE_HOME:-$HOME/.local/state}}"; do
  mkdir -p "$d/fakecli" && echo 1 > "$d/fakecli/state"
done
echo 1 > .fakecli.lock
echo "fakecli 4.5.6"
"""
THREAD = "ctx_handover-refine"


def _diff(before: dict, after: dict) -> dict:
    return {
        "added": sorted(set(after) - set(before)),
        "removed": sorted(set(before) - set(after)),
        "changed": sorted(k for k in set(before) & set(after) if before[k] != after[k]),
    }


def test_int_scn_probes_leave_both_homes_untouched(homes, monkeypatch):
    """Scenario 11a: ``tool_probe: [fakecli]`` in both homes' settings; a planning-only push through the CLI surface
    (``hermes handover to server --session <id> --dry-run``) probes the laptop in this process and the server in the
    listener process."""
    server, laptop = homes
    bindir = server.root.parent / "bin"
    bindir.mkdir()
    witness = server.root.parent / "witness.log"
    cli = bindir / "fakecli"
    cli.write_text(FAKECLI.format(witness=witness), encoding="utf-8")
    cli.chmod(0o755)
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ.get('PATH', os.defpath)}")
    for home in (server, laptop):
        home.write_settings({"refine": "off", "tool_probe": ["fakecli"], "stall_timeout_min": 1})
        for rel in (".config", ".cache", ".local/share", ".local/state"):
            (home.home / rel).mkdir(parents=True, exist_ok=True)
    project = laptop.home / "project"
    project.mkdir()
    (project / "main.py").write_text("print('hi')\n", encoding="utf-8")
    server.identity()
    listener = Listener(server)
    try:
        pair(server, laptop, listener)
        sid = session_in(laptop, project, "Probe me")
        before = {h.name: dir_fingerprint(h.home) for h in (server, laptop)}
        call, _ = surface(laptop, f"to server --session {sid} --dry-run --json")
        after = {h.name: dir_fingerprint(h.home) for h in (server, laptop)}
        listener_pid = listener.proc.pid
    finally:
        listener.stop()
    ran = witness.read_text(encoding="utf-8").splitlines() if witness.exists() else []
    parents = {line.rsplit("PPID=", 1)[-1] for line in ran}
    assert {str(os.getpid()), str(listener_pid)} <= parents, ("the probe did not run in both processes", ran, call.out)
    assert all(str(laptop.home) not in line and str(server.home) not in line for line in ran), ran
    for name in before:
        assert _diff(before[name], after[name]) == {"added": [], "removed": [], "changed": []}, name


def _plan_of(home, job_id: str) -> FinalPlan:
    with home.active():
        row = jobs.runner_for(None).ledger.job(job_id, Role.CONDUCTOR) or {}
    raw = (row.get("result") or {}).get("plan")
    assert isinstance(raw, dict), ("the planning-only job recorded no plan", row.get("state"), row.get("result"))
    return from_wire(FinalPlan, raw)


def _late_free(plan: FinalPlan, late: Path) -> None:
    assert all(i.source_path != str(late) for i in plan.plan.items), "the late answer's item is in the plan"
    assert "late-tool" not in plan.plan.tools_needed, "the late answer's tool is in the plan"
    assert "late answer" not in plan.plan.notes_for_target, "the late answer's note is in the plan"


def _refine_threads(older: frozenset) -> list[threading.Thread]:
    return [t for t in threading.enumerate() if t.name == THREAD and t.is_alive() and t not in older]


def test_int_scn_abandoned_refinement_leaves_no_state(online):
    """Scenario 11b: three planning-only pushes with ``refine: side_question`` and ``refine_timeout_s`` = 1."""
    _server, laptop, _listener, peer = online
    project = laptop.home / "project"
    project.mkdir()
    (project / "main.py").write_text("print('hi')\n", encoding="utf-8")
    late = laptop.home / "late.md"
    late.write_text("# found late\n", encoding="utf-8")
    sid = session_in(laptop, project, "Refine me")
    older = frozenset(_refine_threads(frozenset()))
    opts = Options(refine="side_question", refine_timeout_s=1, dry_run=True, hints=PlanHints())
    gate = threading.Event()
    late_reply = {
        "add": [{"path": str(late), "reason": "late"}],
        "tools_needed": ["late-tool"],
        "notes_for_target": "late answer",
    }

    def late_side_question(prompt: str) -> str:
        gate.wait(60)
        return json_reply(late_reply)

    home_before = dir_fingerprint(laptop.home)
    started = time.monotonic()
    first = run_job(laptop, Mode.PUSH, peer, SessionRef(id=sid), options=opts, side_question=late_side_question)
    assert time.monotonic() - started < 50, "planning waited for the overrunning side question"
    plan1 = _plan_of(laptop, first.job_id)
    _late_free(plan1, late)
    assert plan1.plan.refine.used == "off" and "timeout" in str(plan1.plan.refine.error), plan1.plan.refine
    with laptop.active():
        events1 = len(jobs.runner_for(None).ledger.events(first.job_id, Role.CONDUCTOR))
    abandoned = _refine_threads(older)
    assert len(abandoned) == 1, "the overrunning side question should still run in its own thread"

    gate.set()
    for t in abandoned:
        t.join(10)
    assert _refine_threads(older) == [], "the abandoned refinement thread did not end"
    with laptop.active():
        assert len(jobs.runner_for(None).ledger.events(first.job_id, Role.CONDUCTOR)) == events1, "late write"
    assert _plan_of(laptop, first.job_id) == plan1

    fresh = run_job(
        laptop,
        Mode.PUSH,
        peer,
        SessionRef(id=sid),
        options=opts,
        side_question=lambda prompt: json_reply({"tools_needed": ["node"], "notes_for_target": "fresh answer"}),
    )
    plan2 = _plan_of(laptop, fresh.job_id)
    _late_free(plan2, late)
    assert plan2.plan.refine.used == "side_question" and "node" in plan2.plan.tools_needed, plan2.plan.refine

    silent = run_job(laptop, Mode.PUSH, peer, SessionRef(id=sid), options=opts, side_question=lambda prompt: None)
    plan3 = _plan_of(laptop, silent.job_id)
    _late_free(plan3, late)
    assert plan3.plan.refine.used == "off", "a late answer stood in for a missing one"
    assert _diff(home_before, dir_fingerprint(laptop.home)) == {"added": [], "removed": [], "changed": []}
