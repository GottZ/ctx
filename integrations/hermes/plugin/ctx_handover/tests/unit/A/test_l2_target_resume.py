"""★ Commit resumption in another process (contract X11, W020/W098; DESIGN §3.4, §3.6).

A real import runs in child process 1 and is killed (``SIGKILL``) between c1 and c3 — right after the journal
records ``c1:<item>`` or ``c2:cwd`` as done. Child process 2 (two minutes later on the ledger clock, so the dead
owner is stale) rebuilds ``LocalTarget`` from the profile, calls ``recover`` and finishes the commit from the
journaled ``WorkspaceTxn`` snapshot. A third home imports the same bundle without interruption. In the ``appear``
variant a foreign directory appears at the destination just before c1 in both runs, so c1 commits into its
fallback directory — a fact only the snapshot journaled after c1 carries into the second process.

Acceptance: the resumed end state equals the undisturbed one byte for byte (work tree, git state, session file,
transcript hashes, workspace-map state), and no step ran twice (one ``intent``/``done`` per forward step; c1's
own journal line exactly once).

Run as ``python -m ctx_handover.tests.unit.A.test_l2_target_resume <base> <home> import <bundle> <crash-after>
[appear]`` or ``… resume <job id>`` (the child side).
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.jsonutil import from_wire
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, OpState, Options, Role, SessionRef
from ctx_handover.engine.ops import OpRunner
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes.apis_fake import RecordingReporter
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.integration.harness import PLUGIN_PARENT, UPSTREAM, Home, lineage_hashes, session_marks
from ctx_handover.tests.unit.A.test_l2_target_import import IMPORT_OPTIONS, make_bundle

CHILD = "ctx_handover.tests.unit.A.test_l2_target_resume"
HOOK_BYTES = b"post-turn hook output\n"
LATER_S = 120.0
"""How much later the resuming process looks at the ledger: beyond ``STALE_OWNER_S`` and the op staleness."""


# -- child side ------------------------------------------------------------------------------------------------
def _hooks(home: Home, crash: str, appear: bool) -> None:
    """Journal hooks of child process 1: ``appear`` puts foreign content at the destination right before c1;
    ``crash`` SIGKILLs this process right after a forward step starting with that prefix is journaled done."""
    original = Ledger.event

    def event(self, job_id, role, kind, data, *, epoch):
        step = str((data or {}).get("step", ""))
        forward = role is Role.TARGET and (data or {}).get("direction") == "forward"
        if appear and forward and kind == "intent" and step.startswith("c1:"):
            dest = home.home / "hermes-handover" / "proj"
            dest.mkdir(parents=True, exist_ok=True)
            (dest / "foreign.txt").write_text("not from the handover\n")
        original(self, job_id, role, kind, data, epoch=epoch)
        if crash != "-" and forward and kind == "done" and step.startswith(crash):
            os.kill(os.getpid(), signal.SIGKILL)

    Ledger.event = event


def _child_import(home: Home, bundle: str, crash: str, appear: bool) -> int:
    _hooks(home, crash, appear)
    with home.active():
        runner = jobs.runner_for(None)
        job_id, final = runner.plan_sync(
            Mode.IMPORT,
            None,
            IMPORT_OPTIONS,
            session_ref=SessionRef(id=""),
            calling_session=None,
            session_key=None,
            bundle_path=Path(bundle),
        )
        print(f"JOB {job_id}", flush=True)
        assert final is not None
        return runner.run_foreground(job_id)


def _child_resume(home: Home, job_id: str) -> int:
    with home.active():
        _source, target = jobs.make_local_apis(None, calling_session=None, session_key=None)
        ledger = Ledger(target.ledger.path, clock=lambda: time.time() + LATER_S)
        target.ledger, target.ops = ledger, OpRunner(ledger, None)
        row = ledger.job(job_id, Role.CONDUCTOR)
        job = JobContext(
            job_id=job_id,
            mode=Mode.IMPORT,
            peer=None,
            options=from_wire(Options, row["params"]["options"]),
            epoch=int(row["owner_epoch"]),
            reporter=RecordingReporter(),
            cancelled=lambda: False,
            created_at=float(row["created_at"]),
        )
        st = target.recover(job)
        done = target.ops.wait(job_id, st.op_id, timeout=120)
        print("RESULT " + json.dumps({"state": str(done.state), "error": done.error}), flush=True)
        target.ops.stop()
        return 0 if done.state is OpState.DONE else 1


def main(argv: list[str]) -> int:
    base, name, verb, *rest = argv
    home = Home(Path(base), name)
    if verb == "import":
        return _child_import(home, rest[0], rest[1], "appear" in rest[2:])
    if verb == "resume":
        return _child_resume(home, rest[0])
    raise SystemExit(f"unknown verb {verb!r}")


# -- test side -------------------------------------------------------------------------------------------------
def _run_child(home: Home, *args: str) -> subprocess.CompletedProcess:
    env = {k: v for k, v in os.environ.items() if not k.startswith(("HERMES_", "CTX_HANDOVER_", "GIT_"))}
    env.update(home.env())
    env["PYTHONPATH"] = os.pathsep.join(p for p in (UPSTREAM, str(PLUGIN_PARENT)) if p)
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return subprocess.run(  # noqa: S603 - fixed argv: this interpreter and a module of the test tree
        [sys.executable, "-m", CHILD, str(home.root.parent), home.name, *args],
        env=env,
        cwd=str(home.root),
        capture_output=True,
        timeout=300,
        check=False,
    )


def _job_of(proc: subprocess.CompletedProcess) -> str:
    for line in proc.stdout.decode().splitlines():
        if line.startswith("JOB "):
            return line.split()[1]
    raise AssertionError(f"no job id; stderr: {proc.stderr.decode()[-2000:]}")


def _forward_steps(home: Home, job_id: str) -> dict[str, dict[str, int]]:
    """Per commit step: how often its forward ``intent`` and ``done`` were journaled; ``ws:c1`` counts E's own
    journal line of the c1 rename (``WorkspaceTxn`` forward step)."""
    out: dict[str, dict[str, int]] = {}
    for e in home.ledger().events(job_id, Role.TARGET):
        data = e["data"] or {}
        step = str(data.get("step", ""))
        if e["kind"] in ("intent", "done") and data.get("direction") == "forward" and step.startswith("c"):
            out.setdefault(step, {"intent": 0, "done": 0})[e["kind"]] += 1
        if e["kind"] == "ws" and data.get("ws_kind") == "done" and step.startswith("c1:"):
            out.setdefault("ws:" + step, {"intent": 0, "done": 0})["done"] += 1
    return out


def _owners(home: Home, job_id: str) -> list[str]:
    return [e["data"]["owner"] for e in home.ledger().events(job_id, Role.TARGET) if e["kind"] == "owner"]


def _ack(home: Home, job_id: str) -> dict:
    out: dict = {}
    for e in home.ledger().events(job_id, Role.TARGET):
        ctx = (e["data"] or {}).get("ctx")
        if isinstance(ctx, dict) and "ack" in ctx:
            out = ctx["ack"]
    return out


def _end_state(home: Home, job_id: str) -> dict:
    """Everything the commit wrote on this host; the destination is the one the ack names (with ``appear`` the
    fallback directory, whose name carries the commit time — compared by its prefix)."""
    ledger = home.ledger()
    arrival = [r for r in ledger.arrival_rows() if r["job_id"] == job_id]
    assert arrival, "no arrival row"
    tip = arrival[0]["session_id"]
    ((_item, dest_s, mode),) = _ack(home, job_id)["dests"]
    dest = Path(dest_s)
    repo = RepoBuilder(dest)
    hook_dir = home.hermes_home / "hook_outputs" / tip
    hook_files = {p.name: p.read_bytes() for p in sorted(hook_dir.iterdir())} if hook_dir.is_dir() else {}
    rows = [(r.direction, r.remote_path, r.state, r.repo_identity) for r in ledger.map_by_local(str(dest))]
    marks = session_marks(home.db(), tip)
    return {
        "target_state": ledger.job(job_id, Role.TARGET)["state"],
        "dest": dest.name if dest.name == "proj" else dest.name.split("-handover-")[0] + "-handover-…",
        "mode": mode,
        "tree": tree_digest(dest),
        "head": repo.head(),
        "status": repo.status(),
        "hook_files": hook_files,
        "hashes": lineage_hashes(home.db(), tip),
        "marks": {k: marks[k] for k in ("hidden", "archived", "ended", "title")},
        "map_rows": rows,
        "steps": sorted(_forward_steps(home, job_id)),
    }


@pytest.fixture
def homes3(tmp_path):
    require_upstream()
    jobs._reset_for_tests()  # per-process profile caches of the composition root
    laptop, server, control = Home(tmp_path, "laptop"), Home(tmp_path, "server"), Home(tmp_path, "control")
    yield laptop, server, control
    jobs._reset_for_tests()


@pytest.mark.parametrize(("crash_after", "appear"), [("c1:", False), ("c2:cwd", False), ("c1:", True)])
def test_commit_resumes_in_another_process(homes3, crash_after, appear):
    """★ X11. Mutants (red proofs): (a) the journal's done steps are forgotten — the second process runs c1 again;
    (b) only the PREPARE snapshot is restored (step results ignored) — with ``appear`` the second process records
    the occupied destination instead of the fallback c1 committed into."""
    laptop, server, control = homes3
    sid, _root, bundle, digest = make_bundle(laptop, hook_output=HOOK_BYTES)
    extra = ["appear"] if appear else []
    clean = _run_child(control, "import", str(bundle), "-", *extra)
    assert clean.returncode == 0, clean.stderr.decode()[-3000:]
    job_c = _job_of(clean)

    crashed = _run_child(server, "import", str(bundle), crash_after, *extra)
    assert crashed.returncode == -signal.SIGKILL, crashed.stderr.decode()[-3000:]
    job_s = _job_of(crashed)
    assert server.ledger().job(job_s, Role.TARGET)["state"] == "committing"
    before = _forward_steps(server, job_s)
    assert any(s.startswith(crash_after) for s in before)
    assert not any(s.startswith("c3:") for s in before)  # killed between c1 and c3

    resumed = _run_child(server, "resume", job_s)
    assert resumed.returncode == 0, (resumed.stdout.decode(), resumed.stderr.decode()[-3000:])

    owners = _owners(server, job_s)
    assert len({o.split(":")[0] for o in owners}) >= 2  # the commit finished under another process
    steps = _forward_steps(server, job_s)
    assert all(v["done"] == 1 for v in steps.values()), steps  # no step ran twice
    assert all(v["intent"] == 1 for k, v in steps.items() if not k.startswith("ws:")), steps
    assert {k for k in steps if k.startswith("c3:")}  # c3 ran — in the second process
    assert not any(k.startswith(("c1:c1:", "ws:c1:c1:")) for k in steps)  # E's step names as they are (W021)

    want = _end_state(control, job_c)
    got = _end_state(server, job_s)
    assert got == want
    assert want["tree"] == digest and want["hook_files"] == {"post-turn.txt": HOOK_BYTES}
    assert want["hashes"] == lineage_hashes(laptop.db(), sid)
    assert want["marks"]["hidden"] is False
    if appear:  # c1 went to the fallback; the occupied destination keeps only the foreign file
        assert want["dest"] == "proj-handover-…" and want["map_rows"]
        assert sorted(p.name for p in (server.home / "hermes-handover" / "proj").iterdir()) == ["foreign.txt"]


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
