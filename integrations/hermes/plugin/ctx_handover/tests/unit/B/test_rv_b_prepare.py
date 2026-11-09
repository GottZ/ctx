"""Review round, package rv-b: H3 — a partial failure inside ``prepare`` must never lose the backup.

Before, every sub-step p2…p5 wrote its backups in the same ledger transaction as the Hermes writes (each of which
commits on its own in ``state.db``). An exception in the middle of the loop — or the process dying between the
Hermes commit and the ledger commit — rolled the backups back while the Hermes writes stayed: after ``release`` the
title stayed marked, a goal stayed paused, the session stayed ended/archived; a retried ``prepare`` read the marked
title as the original and marked it twice (R14). Now each sub-step commits its backups first (write-once), then
writes to Hermes, then sets its step marker.

The lineage is built through the core's write paths; the ledger is the production ``Ledger``.
"""

from __future__ import annotations

import sqlite3
from datetime import datetime, timezone

import pytest

from ctx_handover.engine import lock
from ctx_handover.engine.ledger import Ledger
from ctx_handover.tests.unit.B.test_lock_support import JOB, PEER, build, describe, driver

WHEN = datetime(2026, 11, 9, 15, 8, tzinfo=timezone.utc)
OTHER = "pid=1:turn=other:platform=cli"


class _DyingLedger(Ledger):
    """The production ledger, except that the step marker of ``die_at`` raises once: the process "dies" after the
    sub-step's Hermes writes committed and before its ledger transaction did (the rollback is what a dead process
    leaves behind: nothing of that transaction)."""

    die_at: str | None = None

    def backup_put(self, job_id: str, key: str, value: str | None) -> None:
        if self.die_at is not None and key == f"step:{self.die_at}":
            self.die_at = None
            raise RuntimeError("process died")
        super().backup_put(job_id, key, value)


@pytest.fixture
def real_ledger(tmp_path):
    led = _DyingLedger(tmp_path / "data" / "ledger.sqlite3")
    try:
        yield led
    finally:
        led.close()


def _state(db, info) -> dict:
    """Everything prepare changes on the source and release must give back exactly."""
    rows = {}
    for seg in info.lineage_ids:
        row = db.get_session(seg)
        rows[seg] = {k: row[k] for k in ("archived", "auto_archived", "hidden", "ended_at", "end_reason", "title")} | {
            "title_source": db.get_session_title_source(seg)
        }
    return {"rows": rows, "meta": {k: db.get_meta(f"{k}:{info.tip_id}") for k in lock.DRIVER_KEYS}}


def _two_titles(session_db, make_session):
    """Lineage with two titled segments (older lineages keep a title on the root): ``Work part 1`` → ``Work``."""
    tip = build(make_session)
    info = describe(session_db, tip)
    session_db.set_session_title(info.root_id, "Work part 1")
    return tip, describe(session_db, tip)


def _flaky(original, fail_when):
    def call(*args, **kwargs):
        if fail_when(*args):
            raise sqlite3.OperationalError("database is locked")
        return original(*args, **kwargs)

    return call


def _fail_second_title(session_db):
    original = session_db.set_session_title
    calls = {"n": 0}

    def flaky(sid, title):
        calls["n"] += 1
        if calls["n"] == 2:
            raise sqlite3.OperationalError("database is locked")
        return original(sid, title)

    session_db.set_session_title = flaky
    return original


def _free(session_db, tip) -> bool:
    got = session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)
    if got:
        session_db.release_session_turn_lease(tip, OTHER)
    return bool(got)


def test_rv_b_h3_p3_partial_failure_release_restores_every_title(session_db, make_session, real_ledger):
    """★ H3 (p3): the rename of the second segment hits ``database is locked`` after the first was renamed; release
    gives both titles back. Before: ``title:*`` backups rolled back with the failed transaction, the root kept
    ``Work part 1 (handed over to laptop …)``."""
    tip, info = _two_titles(session_db, make_session)
    before = _state(session_db, info)
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    original = _fail_second_title(session_db)
    with pytest.raises(sqlite3.OperationalError):
        lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    session_db.set_session_title = original
    assert session_db.get_session_title(info.root_id).startswith("Work part 1 (handed over to laptop ")
    lock.release(session_db, real_ledger, info, holder, JOB)
    assert _state(session_db, info) == before
    assert real_ledger.source_state(JOB)["state"] == "released"
    assert _free(session_db, tip)


def test_rv_b_h3_p3_retry_marks_once_and_reclaim_restores(session_db, make_session, real_ledger):
    """★ H3 / R14: the same failure, then the job retries ``prepare``: every title is marked exactly once from its
    original, and ``reclaim`` gives the originals back. Before: the retry read the marked root title as the
    original ⇒ ``Work part 1 (handed over …) (handed over …)``, kept for good after the reclaim."""
    tip, info = _two_titles(session_db, make_session)
    before = _state(session_db, info)
    lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    original = _fail_second_title(session_db)
    with pytest.raises(sqlite3.OperationalError):
        lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    session_db.set_session_title = original
    lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    max_len = int(getattr(session_db, "MAX_TITLE_LENGTH", 100) or 100)
    assert session_db.get_session_title(info.root_id) == lock._retitle("Work part 1", PEER, WHEN, max_len)
    assert session_db.get_session_title(tip) == lock._retitle("Work", PEER, WHEN, max_len)
    assert real_ledger.source_state(JOB)["state"] == "prepared"
    out = lock.reclaim(session_db, real_ledger, tip, force=True)
    assert out["state"] == "reclaimed"
    assert _state(session_db, info) == before
    assert _free(session_db, tip)


def test_rv_b_h3_p2_partial_failure_release_restores_drivers(session_db, make_session, real_ledger):
    """★ H3 (p2): pausing the third driver (``loop``) fails after goal and heartbeat were paused; release gives all
    three back. Before: ``meta:*`` backups rolled back, the goal stayed ``paused`` / ``handed over``."""
    tip = build(make_session)
    info = describe(session_db, tip)
    before = _state(session_db, info)
    assert driver(session_db, "goal", tip)["status"] == "active"
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    original = session_db.set_meta
    session_db.set_meta = _flaky(original, lambda key, *_: key.startswith("loop:"))
    with pytest.raises(sqlite3.OperationalError):
        lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    session_db.set_meta = original
    assert driver(session_db, "goal", tip)["status"] == "paused"
    lock.release(session_db, real_ledger, info, holder, JOB)
    assert _state(session_db, info) == before
    assert driver(session_db, "goal", tip)["status"] == "active"


def test_rv_b_h3_p2_retry_keeps_the_original_drivers(session_db, make_session, real_ledger):
    """★ H3 (p2, retry): after the partial failure a retried ``prepare`` pauses the rest; release restores the
    drivers as they were before the first attempt (not the half-paused state the retry found)."""
    tip = build(make_session)
    info = describe(session_db, tip)
    before = _state(session_db, info)
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    original = session_db.set_meta
    session_db.set_meta = _flaky(original, lambda key, *_: key.startswith("loop:"))
    with pytest.raises(sqlite3.OperationalError):
        lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    session_db.set_meta = original
    lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    assert {driver(session_db, k, tip)["status"] for k in lock.DRIVER_KEYS} == {"paused"}
    lock.release(session_db, real_ledger, info, holder, JOB)
    assert _state(session_db, info) == before


@pytest.mark.parametrize("step", ["p2", "p3", "p4", "p5", "p6"])
def test_rv_b_h3_death_between_hermes_and_ledger_commit(session_db, make_session, real_ledger, step):
    """★ H3: the process dies after a sub-step's Hermes writes committed and before its ledger transaction did
    (simulated by the step marker raising); the release the janitor runs afterwards restores the exact prior state
    and frees the session. Before: the backups of p2…p5 went down with the uncommitted transaction (title marked,
    drivers paused, tip ended ``handed_over``, lineage archived after release)."""
    tip, info = _two_titles(session_db, make_session)
    before = _state(session_db, info)
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    real_ledger.die_at = step
    with pytest.raises(RuntimeError, match="process died"):
        lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    assert real_ledger.die_at is None
    assert f"step:{step}" not in dict(real_ledger.backups(JOB))
    lock.release(session_db, real_ledger, info, holder, JOB)
    assert _state(session_db, info) == before
    assert real_ledger.source_state(JOB)["state"] == "released"
    assert _free(session_db, tip)


@pytest.mark.parametrize("step", ["p2", "p3", "p4", "p5", "p6"])
def test_rv_b_h3_death_then_retry_is_idempotent(session_db, make_session, real_ledger, step):
    """★ H3: the same death, then the job continues ``prepare``: it ends ``prepared`` with every title marked once,
    and a reclaim afterwards restores the exact prior state."""
    tip, info = _two_titles(session_db, make_session)
    before = _state(session_db, info)
    lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    real_ledger.die_at = step
    with pytest.raises(RuntimeError, match="process died"):
        lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    lock.prepare(session_db, real_ledger, info, JOB, PEER, WHEN)
    max_len = int(getattr(session_db, "MAX_TITLE_LENGTH", 100) or 100)
    assert session_db.get_session_title(info.root_id) == lock._retitle("Work part 1", PEER, WHEN, max_len)
    assert session_db.get_session_title(tip) == lock._retitle("Work", PEER, WHEN, max_len)
    assert real_ledger.source_state(JOB)["state"] == "prepared"
    assert lock.reclaim(session_db, real_ledger, tip, force=True)["state"] == "reclaimed"
    assert _state(session_db, info) == before
