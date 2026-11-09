"""Review round 3, package r3-a — R2-H1 (REVIEW-2 §3): the first turn of a freshly started process must meet a loaded
veto index.

Since the M6 fix the janitor of a process's first use runs in the background, and ``ensure_started`` returned as soon
as the ``VetoIndex`` refresher thread was started — before it had read the ledger once. The hook of the first turn
read an empty index and let a turn through on a session that was handed over (reviewer harness: 15 and 18 of 20).
DECISIONS R1 (the veto is the fallback layer of the source lock) and README guarantee (5).

Red before r3-a: ``first turn passed the lock in N of 20 fresh processes`` (N > 0) and ``lookup … is None``."""

from __future__ import annotations

import multiprocessing as mp
import os
import threading
import time
from pathlib import Path

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import SessionInfo, SourceState

CTX = mp.get_context("spawn")
SID = "20261109_120000_aaaaaa"
JOB = "01JZR3AF1RST7VRN0000000000"
RUNS = 20
PARALLEL = 4


@pytest.fixture
def runtime_env(fake_host):
    jobs._reset_for_tests()
    yield fake_host
    jobs._reset_for_tests()


def _seed(ledger: Ledger) -> None:
    """A source session handed over and retired here: the ledger vetoes it (DESIGN §7.6)."""
    info = SessionInfo(
        tip_id=SID,
        root_id=SID,
        lineage_ids=(SID,),
        title="Work",
        model=None,
        provider=None,
        cwd=None,
        git_repo_root=None,
        git_branch=None,
        active_messages=2,
        revision=((SID,), 2, 2, 0),
    )
    ledger.source_lock(JOB, info, "pid=1:x", "laptop")
    ledger.source_set(JOB, SourceState.PREPARED)
    ledger.veto_add(JOB, [SID], SID)
    ledger.source_set(JOB, SourceState.RETIRED, peer="laptop")


def first_turn_worker(sid: str, out) -> None:
    """A fresh process (spawn child): register the plugin like Hermes does, then the first turn at once.

    The engine modules are imported before the first turn (warm, as in a process where another profile, a dashboard
    route or ``hermes plugins validate`` loaded them; module import time otherwise hides the race behind luck);
    nothing is started — ledger, veto index, identity, store and the background janitor are built by the first hook.
    """
    import ctx_handover
    from ctx_handover.engine import (  # noqa: F401 - imported, not started
        blocks,
        commands,
        crypto,
        ctxrecords,
        jobs,
        ledger,
        local_source,
        local_target,
        lock,
        ops,
        participant,
    )
    from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx, FakeHost

    patcher = pytest.MonkeyPatch()
    host = FakeHost(user_home=Path(os.environ["HOME"]), hermes_home=Path(os.environ["HERMES_HOME"]))
    host.install(patcher)
    ctx = FakeCtx(host=host)
    ctx_handover.register(ctx)
    t0 = time.monotonic()
    first = ctx.hooks["pre_llm_call"][0](session_id=sid, parent_session_id="", turn_id="t1")
    waited = time.monotonic() - t0
    tool = ctx.hooks["pre_tool_call"][0](session_id=sid, tool_name="terminal")
    out.put({"pid": os.getpid(), "first": first, "tool": tool, "waited": round(waited, 3)})


def test_r3_a_h1_first_turn_of_fresh_processes_is_vetoed(runtime_env, fake_ctx):
    """★ 20 fresh processes over a ledger whose source session is retired: the first ``pre_llm_call`` and the first
    ``pre_tool_call`` of each are blocked — none passes the lock (README guarantee (5), R1).

    Mutant (r3-a, ``lock.VetoIndex.start``): drop the synchronous first ``refresh_now(force=True)``."""
    rt = jobs.ensure_started(fake_ctx)
    _seed(rt.ledger)
    jobs._reset_for_tests()  # the parent's runtime is closed; every child builds its own from the file

    results: list[dict] = []
    for batch in range(0, RUNS, PARALLEL):
        out = CTX.Queue()
        procs = [
            CTX.Process(target=first_turn_worker, args=(SID, out), name=f"first-turn-{batch + i}")
            for i in range(min(PARALLEL, RUNS - batch))
        ]
        for p in procs:
            p.start()
        results += [out.get(timeout=120) for _ in procs]
        for p in procs:
            p.join(timeout=60)
            assert p.exitcode == 0, f"{p.name} exit {p.exitcode}"

    assert len(results) == RUNS
    passed = [r for r in results if not (isinstance(r["first"], dict) and r["first"].get("action") == "block")]
    assert not passed, f"first turn passed the lock in {len(passed)} of {RUNS} fresh processes: {passed}"
    tool_passed = [r for r in results if not (isinstance(r["tool"], dict) and r["tool"].get("action") == "block")]
    assert not tool_passed, f"first tool call passed the lock in {len(tool_passed)} of {RUNS}: {tool_passed}"
    assert all("handed over to laptop" in r["first"]["message"] for r in results)


def test_r3_a_h1_ensure_started_returns_with_the_index_loaded(runtime_env, fake_ctx, monkeypatch):
    """The hook path (``janitor_background=True``) returns only after the index read the ledger once — even when
    that read is slow. Deterministic form of the race above.

    Mutant: as above (red: ``lookup`` is None right after ``ensure_started``)."""
    rt = jobs.ensure_started(fake_ctx)
    _seed(rt.ledger)
    jobs._reset_for_tests()

    real = Ledger.veto_rows

    def slow_veto_rows(self):
        time.sleep(0.3)
        return real(self)

    monkeypatch.setattr(Ledger, "veto_rows", slow_veto_rows)
    rt = jobs.ensure_started(fake_ctx, janitor_background=True)
    entry = rt.veto.lookup(SID)
    assert entry is not None and entry["state"] == "retired"
    assert rt.veto.refreshed_at is not None


def test_r3_a_h1_the_hook_still_does_not_wait_for_the_janitor(runtime_env, fake_ctx, monkeypatch):
    """M6 stays: the index is loaded on the build, the janitor still runs in ``ctxh-janitor``."""
    rt0 = jobs.ensure_started(fake_ctx)
    _seed(rt0.ledger)
    jobs._reset_for_tests()
    started, release = threading.Event(), threading.Event()

    def slow_janitor(self, *, background: bool = True) -> dict:
        started.set()
        release.wait(10)
        return {}

    monkeypatch.setattr(jobs.Runtime, "janitor", slow_janitor)
    t0 = time.monotonic()
    rt = jobs.ensure_started(fake_ctx, janitor_background=True)
    try:
        assert rt.veto.lookup(SID) is not None
        assert started.wait(5)
        assert time.monotonic() - t0 < 5
        assert any(t.name == "ctxh-janitor" and t.daemon for t in threading.enumerate())
    finally:
        release.set()


def test_r3_a_h1_concurrent_first_uses_both_see_the_loaded_index(runtime_env, fake_ctx, monkeypatch):
    """A slash command and a hook start the runtime at the same moment: whichever comes second gets the runtime only
    after the first load, never an index that is still empty."""
    rt0 = jobs.ensure_started(fake_ctx)
    _seed(rt0.ledger)
    jobs._reset_for_tests()
    real = Ledger.veto_rows

    def slow_veto_rows(self):
        time.sleep(0.3)
        return real(self)

    monkeypatch.setattr(Ledger, "veto_rows", slow_veto_rows)
    seen: list[object] = []
    barrier = threading.Barrier(2)

    def use(background: bool) -> None:
        barrier.wait(5)
        rt = jobs.ensure_started(fake_ctx, janitor_background=background)
        seen.append(rt.veto.lookup(SID))

    threads = [threading.Thread(target=use, args=(flag,)) for flag in (True, False)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(20)
    assert len(seen) == 2 and all(e is not None for e in seen), seen
