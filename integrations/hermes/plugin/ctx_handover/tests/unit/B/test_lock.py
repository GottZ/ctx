# ruff: noqa: F811  (pytest fixtures imported from test_lock_support are redefined as parameters)
"""lock.py: leases, busy probe, primary lock (prepare), retire, release, reclaim (TESTPLAN §2.3; DESIGN §7).

Every session comes from a real ``SessionDB`` built through the core's write paths (``make_session``); the
ledger is the B test stand-in (``support.LedgerStub``) on the schema of INTERFACES §7.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
import time

import pytest

from ctx_handover.engine import lock
from ctx_handover.engine.errors import AlreadyHandingOver, Cancelled, InvalidState, SessionBusy, WaitTimeout
from ctx_handover.engine.model import MapRow, Options
from ctx_handover.tests.unit.B.support import LedgerStub
from ctx_handover.tests.unit.B.test_lock_support import (  # noqa: F401 (fixtures)
    JOB,
    PEER,
    ack,
    build,
    chat_requests,
    describe,
    driver,
    index_for,
    ledger,
    locked,
    now,
    prepared,
    provider_url,
    vetoed_turn,
)

OTHER = "pid=1:turn=other:platform=cli"


def _row(db, sid):
    return db.get_session(sid)


# ---------------------------------------------------------------------------------------------------------
# Holder, leases, busy
# ---------------------------------------------------------------------------------------------------------


_DEAD_HOLDER = """
import os, sys
from pathlib import Path
from hermes_state import SessionDB
from ctx_handover.engine.lock import holder_string
db = SessionDB(Path(sys.argv[1]))
holder = holder_string("01JDEADHOLDER") if sys.argv[3] == "stamped" else f"pid={os.getpid()}:ctx_handover:job=x"
assert db.try_acquire_session_turn_lease(sys.argv[2], holder, ttl_seconds=300, patience_s=2.0)
print(holder, flush=True)
os._exit(0)
"""


@pytest.mark.skipif(not sys.platform.startswith("linux"), reason="pid namespaces are a Linux feature")
def test_lock_holder_pidns_stamp(session_db, make_session, hermes_home):
    """A stamped holder of a dead process is freed at once; an unstamped one only after its TTL [LAB L6]."""
    holder = lock.holder_string(JOB)
    inode = os.readlink("/proc/self/ns/pid").split("[")[1].rstrip("]")
    assert holder == f"pid={os.getpid()}:pidns={inode}:ctx_handover:job={JOB}"
    assert lock.lease_timing(Options()) == (120, 30)

    tip = build(make_session, drivers=False)
    db_path = str(hermes_home / "state.db")
    for mode, freed in (("stamped", True), ("unstamped", False)):
        out = subprocess.run(  # noqa: S603 (fixed argv, own interpreter)
            [sys.executable, "-c", _DEAD_HOLDER, db_path, tip, mode],
            env=os.environ.copy(),
            capture_output=True,
            text=True,
            timeout=120,
            check=False,
        )
        assert out.returncode == 0, out.stderr[-2000:]
        dead = out.stdout.strip()
        assert ("pidns=" in dead) is (mode == "stamped")
        started = time.monotonic()
        got = session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=60, patience_s=1.0)
        assert got is freed, (mode, dead)
        assert time.monotonic() - started < 5
        if got:
            session_db.release_session_turn_lease(tip, OTHER)


def test_lock_acquire_and_refresh_both_leases(session_db, make_session, ledger):
    tip = build(make_session, drivers=False)
    info, holder = locked(session_db, ledger, tip)
    assert ledger.source_state(JOB)["state"] == "locked"
    assert ledger.source_state(JOB)["holder"] == holder
    first = session_db.session_turn_lease_expires_at(tip, holder)
    assert first is not None
    assert session_db.get_compression_lock_holder(tip) == holder
    time.sleep(0.05)
    assert lock.refresh(session_db, info, holder, ttl_s=600) is True
    assert session_db.session_turn_lease_expires_at(tip, holder) > first
    # the turn lease covers the whole lineage (keyed by the root): a turn on the root is blocked too
    assert not session_db.try_acquire_session_turn_lease(info.root_id, OTHER, ttl_seconds=60, patience_s=0.2)
    session_db.release_compression_lock(tip, holder)
    assert lock.refresh(session_db, info, holder, ttl_s=600) is False


def test_lock_busy_when_turn_holds_lease(session_db, make_session, ledger):
    """A running turn refuses the lock with the user-facing text; the ledger row is freed for a later try.

    Mutant: ``acquire`` treats a refused ``try_acquire_session_turn_lease`` as acquired (``got = True``).
    """
    tip = build(make_session, drivers=False)
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=300, patience_s=1.0)
    info = describe(session_db, tip)
    with pytest.raises(SessionBusy) as err:
        lock.acquire(session_db, ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    assert err.value.message == lock.BUSY_MESSAGE
    assert ledger.source_state(JOB)["state"] == "released"
    assert session_db.get_compression_lock_holder(tip) is None
    session_db.release_session_turn_lease(tip, OTHER)
    holder = lock.acquire(session_db, ledger, info, "01JSECONDJOB", ttl_s=120, wait_s=0, cancelled=lambda: False)
    assert session_db.session_turn_lease_expires_at(tip, holder) is not None
    with pytest.raises(AlreadyHandingOver):
        lock.acquire(session_db, ledger, info, "01JTHIRDJOB", ttl_s=120, wait_s=0, cancelled=lambda: False)


class _Clock:
    def __init__(self):
        self.t = 0.0
        self.sleeps: list[float] = []

    def __call__(self) -> float:
        return self.t


def test_lock_wait_until_free(session_db, make_session, ledger):
    tip = build(make_session, drivers=False)
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=300, patience_s=1.0)
    clock = _Clock()

    def sleep(seconds: float) -> None:
        clock.sleeps.append(seconds)
        clock.t += seconds
        if len(clock.sleeps) == 2:
            session_db.release_session_turn_lease(tip, OTHER)

    info = describe(session_db, tip)
    holder = lock.acquire(
        session_db, ledger, info, JOB, ttl_s=120, wait_s=1800, cancelled=lambda: False, sleep=sleep, clock=clock
    )
    assert clock.sleeps == [2.0, 2.0]
    assert ledger.source_state(JOB)["state"] == "locked"
    assert session_db.get_compression_lock_holder(tip) == holder


def test_lock_wait_timeout(session_db, make_session, ledger):
    tip = build(make_session, drivers=False)
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=300, patience_s=1.0)
    clock = _Clock()

    def sleep(seconds: float) -> None:
        clock.sleeps.append(seconds)
        clock.t += seconds

    info = describe(session_db, tip)
    with pytest.raises(WaitTimeout):
        lock.acquire(
            session_db, ledger, info, JOB, ttl_s=120, wait_s=7, cancelled=lambda: False, sleep=sleep, clock=clock
        )
    assert clock.sleeps == [2.0, 2.0, 2.0, 1.0]
    assert ledger.source_state(JOB)["state"] == "released"
    flag = {"n": 0}

    def cancelled() -> bool:
        flag["n"] += 1
        return flag["n"] > 1

    with pytest.raises(Cancelled):
        lock.acquire(
            session_db,
            ledger,
            info,
            "01JCANCELJOB",
            ttl_s=120,
            wait_s=60,
            cancelled=cancelled,
            sleep=sleep,
            clock=clock,
        )
    assert ledger.source_state("01JCANCELJOB")["state"] == "released"


def test_lock_compression_lock_blocks_hygiene(session_db, make_session, ledger):
    """While locked, the core's compaction (in place and rotation) refuses: our compression lock fences it."""
    tip = build(make_session, drivers=False)
    info, holder = locked(session_db, ledger, tip)
    before = session_db.conversation_revision(tip, include_ancestors=True)
    with pytest.raises(RuntimeError, match="compression lock"):
        make_session.compact_in_place(tip, keep_last=1)
    with pytest.raises(RuntimeError, match="compression lock"):
        make_session.rotate(tip)
    assert session_db.conversation_revision(tip, include_ancestors=True) == before
    lock.release(session_db, ledger, info, holder, JOB)
    assert make_session.compact_in_place(tip, keep_last=1)["active"] >= 1


def test_lock_compaction_running_refuses(session_db, make_session, ledger):
    tip = build(make_session, drivers=False)
    assert session_db.try_acquire_compression_lock(tip, "pid=1:compress", ttl_seconds=300)
    with pytest.raises(SessionBusy) as err:
        locked(session_db, ledger, tip)
    assert err.value.details["reason"] == "compression"
    # the turn lease taken first was given back
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=60, patience_s=0.5)


# ---------------------------------------------------------------------------------------------------------
# Busy probe
# ---------------------------------------------------------------------------------------------------------


def test_probe_busy_idle(session_db, make_session, hermes_home):
    tip = build(make_session, drivers=False)
    report = lock.probe_busy(session_db, hermes_home / "state.db", describe(session_db, tip), hermes_home, None)
    assert report.busy is False
    assert report.warnings == ()


def test_probe_busy_queued_admission(session_db, make_session, hermes_home):
    """A queued prompt admitted through the unified runtime's own path (``admit_session_input``) makes the
    session busy."""
    from hermes_state_runtime import admit_session_input, begin_runtime_epoch

    tip = build(make_session, drivers=False)
    epoch = begin_runtime_epoch(session_db, instance_id="test")
    admit_session_input(
        session_db, epoch=epoch, principal_id="local", session_id=tip, request_id="r1", payload={"text": "next"}
    )
    report = lock.probe_busy(session_db, hermes_home / "state.db", describe(session_db, tip), hermes_home, None)
    assert report.busy is True
    assert any("queued prompt" in r for r in report.reasons)


def test_probe_busy_running_delegation(session_db, make_session, hermes_home):
    """A delegation recorded through the delegation tool's own writer (``_persist_dispatch``) makes it busy."""
    from tools import async_delegation

    tip = build(make_session, drivers=False)
    async_delegation._persist_dispatch(
        {
            "delegation_id": "d1",
            "session_key": "agent:main:local:dm:" + tip,
            "parent_session_id": tip,
            "dispatched_at": time.time(),
            "goal": "do it",
        }
    )
    report = lock.probe_busy(session_db, hermes_home / "state.db", describe(session_db, tip), hermes_home, None)
    assert report.busy is True
    assert [d["delegation_id"] for d in report.delegations] == ["d1"]


def test_probe_busy_processes_and_cron(session_db, make_session, hermes_home):
    """Background processes (``processes.json``, ``tools/process_registry.py:_CHECKPOINT_FIELDS``) and cron jobs
    attached to the session (``cron/jobs.json``) are reported, not busy. The files are written in the shape
    their writers persist; starting real processes or the cron ticker is out of scope for a unit test."""
    tip = build(make_session, drivers=False)
    key = "agent:main:local:dm:" + tip
    (hermes_home / "processes.json").write_text(
        json.dumps(
            [
                {"pid": 4242, "command": "npm run dev", "cwd": "/w", "session_key": key, "started_at": 1.0},
                {"pid": 7, "command": "other", "session_key": "agent:main:local:dm:else"},
            ]
        )
    )
    (hermes_home / "cron").mkdir(exist_ok=True)
    (hermes_home / "cron" / "jobs.json").write_text(
        json.dumps(
            {
                "jobs": [
                    {
                        "id": "j1",
                        "name": "nightly",
                        "attach_to_session": True,
                        "schedule": "0 3 * * *",
                        "origin": {"platform": "local", "chat_id": tip},
                    },
                    {"id": "j2", "attach_to_session": False, "origin": {"chat_id": tip}},
                ]
            }
        )
    )
    report = lock.probe_busy(session_db, hermes_home / "state.db", describe(session_db, tip), hermes_home, key)
    assert report.busy is False
    assert [p["pid"] for p in report.processes] == [4242]
    assert [c["id"] for c in report.cron] == ["j1"]
    assert "hermes cron pause j1" in report.cron[0]["disable"]


def test_probe_busy_schema_drift_is_a_warning(session_db, make_session, hermes_home, tmp_path):
    import sqlite3

    tip = build(make_session, drivers=False)
    bare = tmp_path / "bare.db"
    sqlite3.connect(bare).close()
    (hermes_home / "processes.json").write_text("{not json")
    report = lock.probe_busy(session_db, bare, describe(session_db, tip), hermes_home, None)
    assert report.busy is False
    assert any("session_admissions" in w for w in report.warnings)
    assert any("async_delegations" in w for w in report.warnings)
    assert any("processes.json" in w for w in report.warnings)
    missing = lock.probe_busy(session_db, tmp_path / "nope" / "state.db", describe(session_db, tip), hermes_home, None)
    assert missing.busy is False and missing.warnings


# ---------------------------------------------------------------------------------------------------------
# Prepare: the primary lock
# ---------------------------------------------------------------------------------------------------------


def test_prepare_applies_primary_lock(session_db, make_session, ledger):
    """p1–p7: ended, archived (``notify=False``), hidden, renamed, drivers paused, veto rows (DESIGN §7.4).

    Mutant: ``prepare`` skips p6 (``set_session_hidden``).
    """
    tip = build(make_session)
    events = []
    original = session_db.set_session_archived

    def spy(sid, archived, *, notify=True):
        events.append(notify)
        return original(sid, archived, notify=notify)

    session_db.set_session_archived = spy
    info, _ = prepared(session_db, ledger, tip)
    assert events == [False]
    for seg in info.lineage_ids:
        row = _row(session_db, seg)
        assert row["archived"] == 1 and row["hidden"] == 1, seg
        assert row["ended_at"] is not None, seg
    assert _row(session_db, tip)["end_reason"] == "handed_over"
    assert _row(session_db, info.root_id)["end_reason"] == "compression"  # first reason wins (SK L1)
    title = session_db.get_session_title(tip)
    assert title.startswith("Work (handed over to laptop ") and title.endswith(")")
    assert driver(session_db, "goal", tip)["status"] == "paused"
    assert driver(session_db, "goal", tip)["paused_reason"] == "handed over"
    assert driver(session_db, "heartbeat", tip)["status"] == "paused"
    assert driver(session_db, "loop", tip)["status"] == "paused"
    entry = ledger.source_state(JOB)
    assert entry["state"] == "prepared" and entry["peer"] == PEER and entry["prepared_at"]
    assert {r["session_id"] for r in ledger.veto_rows()} == set(info.lineage_ids)
    assert {f"step:{s}" for s in lock.PREPARE_STEPS} <= dict(ledger.backups(JOB)).keys()
    # idempotent: a second prepare changes nothing
    lock.prepare(session_db, ledger, info, JOB, PEER, now())
    assert session_db.get_session_title(tip) == title


def test_primary_lock_without_plugin(session_db, make_session, ledger, hermes_home):
    """After prepare the four marks are Hermes' own state: a fresh ``SessionDB`` without any plugin code sees the
    whole lineage archived, hidden, ended and renamed, and the default listing no longer shows it [SK B1].

    Mutant: ``prepare`` skips p5 (``set_session_archived``).
    """
    from hermes_state import SessionDB

    tip = build(make_session)
    info, _ = prepared(session_db, ledger, tip)
    fresh = SessionDB(hermes_home / "state.db")
    try:
        for seg in info.lineage_ids:
            row = fresh.get_session(seg)
            assert row["archived"] == 1, seg
            assert row["hidden"] == 1, seg
            assert row["ended_at"] is not None, seg
        assert fresh.get_session(tip)["end_reason"] == "handed_over"
        assert "handed over to laptop" in fresh.get_session_title(tip)
        assert fresh.get_session_by_title("Work") is None
        listed = {r["id"] for r in fresh.list_sessions_rich(limit=50)}
        assert not listed & set(info.lineage_ids)
    finally:
        fresh.close()


# ---------------------------------------------------------------------------------------------------------
# Retire
# ---------------------------------------------------------------------------------------------------------


def test_retire_order_veto_before_lease_release(session_db, make_session, ledger, tmp_path):
    """r1–r3 are committed before r4 frees the leases, so a queued turn runs into the veto [LAB L6].

    Mutant: ``retire`` frees the leases first (r4 before r1).
    """
    tip = build(make_session)
    info, holder = prepared(session_db, ledger, tip)
    log: list[str] = []
    observer = LedgerStub(ledger.path)
    seen: dict = {}
    for name in ("release_compression_lock", "release_session_turn_lease"):
        original = getattr(session_db, name)

        def wrapped(*a, _original=original, _name=name, **kw):
            log.append(_name)
            entry = observer.source_state(JOB)
            seen[_name] = (
                entry["state"],
                dict(observer.backups(JOB)).get("step:r3"),
                driver(session_db, "goal", tip)["status"],
                len(observer.map_by_local("/w")),
            )
            return _original(*a, **kw)

        setattr(session_db, name, wrapped)
    out = [MapRow("/w", "peer-1", "/remote/w", "out", state={"head": "abc"}, last_job_id=JOB)]
    lock.retire(session_db, ledger, info, holder, JOB, ack(), out)
    assert log == ["release_compression_lock", "release_session_turn_lease"]
    for name in log:
        assert seen[name] == ("retiring", "done", "cleared", 1), name
    assert ledger.source_state(JOB)["state"] == "retired"
    assert ledger.source_state(JOB)["ack"]["title"] == "Work"
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)
    observer.close()


def test_retire_resumes_open_substeps(session_db, make_session, ledger):
    """A crash after r1 leaves ``retiring``; the retry completes r2–r5 instead of answering "ok" [SK M4].

    Mutant: ``retire`` returns at once when the state is already ``retiring``/``retired`` (state-level
    idempotency only).
    """
    tip = build(make_session)
    info, holder = prepared(session_db, ledger, tip)
    original = session_db.set_meta

    def crash(key, value, **kw):
        raise OSError("process killed (simulated)")

    session_db.set_meta = crash
    with pytest.raises(OSError):
        lock.retire(session_db, ledger, info, holder, JOB, ack(), [])
    session_db.set_meta = original
    assert ledger.source_state(JOB)["state"] == "retiring"
    assert driver(session_db, "goal", tip)["status"] == "paused"
    lock.retire(session_db, ledger, info, None, JOB, ack(), [])  # a new process: holder from the ledger
    assert ledger.source_state(JOB)["state"] == "retired"
    assert driver(session_db, "goal", tip)["status"] == "cleared"
    assert {f"step:{s}" for s in lock.RETIRE_STEPS} <= dict(ledger.backups(JOB)).keys()
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)


def test_reclaim_serialized_with_retire(session_db, make_session, ledger):
    """A forced reclaim while retire is inside a sub-step waits for the ledger lock; afterwards the session is
    reclaimed with its goal restored, never cleared behind the reclaim's back [SK M4].

    Mutant: ``lock._tx`` returns a no-op context (no ledger lock around the sub-steps).
    """
    tip = build(make_session)
    info, holder = prepared(session_db, ledger, tip)
    original_goal = json.loads(dict(ledger.backups(JOB))["meta:goal"])
    entered, go = threading.Event(), threading.Event()
    original = session_db.set_meta

    def blocking(key, value, **kw):
        if key.startswith("goal:") and '"cleared"' in value:
            entered.set()
            go.wait(10)
        return original(key, value, **kw)

    session_db.set_meta = blocking
    errors: dict = {}

    def run_retire():
        try:
            lock.retire(session_db, LedgerStub(ledger.path), info, holder, JOB, ack(), [])
        except Exception as exc:
            errors["retire"] = exc

    results: dict = {}

    def run_reclaim():
        results["reclaim"] = lock.reclaim(session_db, LedgerStub(ledger.path), tip, force=True)

    t1 = threading.Thread(target=run_retire)
    t1.start()
    assert entered.wait(10)
    t2 = threading.Thread(target=run_reclaim)
    t2.start()
    time.sleep(0.6)
    waiting = t2.is_alive()
    go.set()
    t1.join(15)
    t2.join(15)
    assert waiting, "reclaim did not wait for the ledger lock held by retire"
    assert results["reclaim"]["state"] == "reclaimed"
    assert ledger.source_state(JOB)["state"] == "reclaimed"
    goal = driver(session_db, "goal", tip)
    assert goal["status"] == original_goal["status"] == "active"
    if "retire" in errors:
        assert isinstance(errors["retire"], InvalidState)


def test_mark_exported(session_db, make_session, ledger):
    tip = build(make_session)
    info, holder = prepared(session_db, ledger, tip)
    lock.mark_exported(session_db, ledger, info, holder, JOB, "/b/handover.ctxh")
    entry = ledger.source_state(JOB)
    assert entry["state"] == "exported" and entry["bundle_path"] == "/b/handover.ctxh"
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)
    assert lock.veto_texts({**entry, "job_id": JOB}).startswith("This session was exported to a handover bundle on")


# ---------------------------------------------------------------------------------------------------------
# Release and reclaim
# ---------------------------------------------------------------------------------------------------------


def _snapshot(db, tip, lineage):
    rows = {s: db.get_session(s) for s in lineage}
    return {
        "rows": {
            s: {k: r[k] for k in ("archived", "hidden", "ended_at", "end_reason", "title")} for s, r in rows.items()
        },
        "title_source": db.get_session_title_source(tip),
        "meta": {k: db.get_meta(f"{k}:{tip}") for k in lock.DRIVER_KEYS},
    }


@pytest.mark.parametrize("pre_archived", [False, True])
def test_release_undoes_prepare(session_db, make_session, ledger, pre_archived):
    """Release restores the exact prior state (not blanket defaults): a session archived before stays archived."""
    tip = build(make_session)
    if pre_archived:
        session_db.set_session_archived(tip, True, notify=False)
    info = describe(session_db, tip)
    before = _snapshot(session_db, tip, info.lineage_ids)
    info, holder = prepared(session_db, ledger, tip)
    assert _snapshot(session_db, tip, info.lineage_ids) != before
    lock.release(session_db, ledger, info, holder, JOB)
    assert _snapshot(session_db, tip, info.lineage_ids) == before
    assert ledger.source_state(JOB)["state"] == "released"
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)
    session_db.release_session_turn_lease(tip, OTHER)
    assert session_db.try_acquire_compression_lock(tip, "pid=1:c", ttl_seconds=30)
    lock.release(session_db, ledger, info, holder, JOB)  # idempotent


def test_release_after_lock_only(session_db, make_session, ledger):
    tip = build(make_session)
    info, holder = locked(session_db, ledger, tip)
    lock.release(session_db, ledger, info, holder, JOB)
    assert ledger.source_state(JOB)["state"] == "released"
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)


def test_reclaim_rewinds_placeholder_rows(session_db, make_session, ledger, provider_url, tmp_path):
    """Two real vetoed turns after retire leave two placeholder pairs; reclaim rewinds them (non-destructive),
    returns the withheld prompts, restores every mark and stops the veto [R1, BX L7].

    Mutant: ``_rewind_placeholders`` returns ``(0, [])`` without rewinding.
    """
    tip = build(make_session)
    info, holder = prepared(session_db, ledger, tip)
    lock.retire(session_db, ledger, info, holder, JOB, ack(), [])
    index = index_for(tmp_path, ledger.path)
    before_active = len(session_db.get_messages(tip))
    for i in range(2):
        result = vetoed_turn(session_db, tip, provider_url, index, f"typed {i}")
        assert result["final_response"].startswith("This session was handed over to laptop on ")
    assert chat_requests() == []
    live = session_db.get_messages(tip)
    assert len(live) == before_active + 4
    out = lock.reclaim(session_db, ledger, tip, force=False)
    assert out["state"] == "reclaimed"
    assert out["withheld"] == ["typed 0", "typed 1"]
    assert out["rewound"] == 4
    assert any("stays active" in w for w in out["warnings"])
    live_after = session_db.get_messages(tip)
    assert len(live_after) == before_active
    assert not any("handed over" in str(m.get("content")) for m in live_after)
    stored = session_db.get_messages(tip, include_inactive=True)
    assert sum(1 for m in stored if not m["active"]) >= 4  # rewound, not deleted
    row = session_db.get_session(tip)
    assert (row["archived"], row["hidden"], row["end_reason"]) == (0, 0, None)
    assert session_db.get_session_title(tip) == "Work"
    assert driver(session_db, "goal", tip)["status"] == "active"
    index.refresh_now()
    assert index.lookup(tip) is None


def test_reclaim_table(session_db, make_session, ledger):
    """DESIGN §7.7: refusals without --force, cancel+release for in-progress jobs, warnings for live copies."""
    assert lock.reclaim(session_db, ledger, "20261109_000000_000000", force=False)["message"] == "Nothing to reclaim."

    tip = build(make_session, title="Locked")
    locked(session_db, ledger, tip, "01JLOCKEDJOB")
    out = lock.reclaim(session_db, ledger, tip, force=False)
    assert out["state"] is None and out["message"] == "Handover in progress; use /handover cancel 01JLOCKEDJOB."
    out = lock.reclaim(session_db, ledger, tip, force=True)
    assert out["state"] == "released" and ledger.cancel_requested("01JLOCKEDJOB")
    assert lock.reclaim(session_db, ledger, tip, force=True)["message"] == "Nothing to reclaim."

    tip2 = build(make_session, title="Prepared")
    prepared(session_db, ledger, tip2, "01JPREPAREDJOB")
    out = lock.reclaim(session_db, ledger, tip2, force=False)
    assert out["state"] is None and out["message"] == "A handover is being committed. Wait, or use --force."
    out = lock.reclaim(session_db, ledger, tip2, force=True)
    assert out["state"] == "reclaimed" and any("may already be active" in w for w in out["warnings"])
    assert session_db.get_session_title(tip2) == "Prepared"

    tip3 = build(make_session, title="Exported")
    info3, holder3 = prepared(session_db, ledger, tip3, "01JEXPORTEDJOB")
    lock.mark_exported(session_db, ledger, info3, holder3, "01JEXPORTEDJOB", "/b/x.ctxh")
    out = lock.reclaim(session_db, ledger, tip3, force=False)
    assert out["state"] == "reclaimed" and any("stays active" in w for w in out["warnings"])

    tip4 = build(make_session, title="Retired")
    info4, holder4 = prepared(session_db, ledger, tip4, "01JRETIREDJOB")
    lock.retire(session_db, ledger, info4, holder4, "01JRETIREDJOB", ack("Retired"), [])
    out = lock.reclaim(session_db, ledger, info4.root_id, force=False)  # any lineage id finds the handover
    assert out["state"] == "reclaimed"
    assert lock.reclaim(session_db, ledger, tip4, force=False)["message"] == "Nothing to reclaim."
    with pytest.raises(InvalidState):
        lock.retire(session_db, ledger, info4, holder4, "01JRETIREDJOB", ack("Retired"), [])
