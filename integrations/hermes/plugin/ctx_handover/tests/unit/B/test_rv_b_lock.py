"""First review round: findings of the first review Medium/Low in ``engine/lock.py`` that touch the promise
"an abort leaves the source usable and without a lock".

* L3 — an exception from ``try_acquire_compression_lock`` left the turn lease standing until its TTL (120 s).
* L6 — a session the idle sweep had archived came back deliberately archived after release/reclaim (Hermes then
  never un-archives it on resume).
* M9 — the canonical Bot Chat came back hidden and untitled after release/reclaim when Bot Mode had minted a new
  Bot Chat meanwhile: nothing but its id reached the conversation.

Lineages are built through the core's write paths; the ledger is the production ``Ledger``.
"""

from __future__ import annotations

import sqlite3

import pytest

from ctx_handover.engine import lock
from ctx_handover.engine.ledger import Ledger
from ctx_handover.tests.unit.B.test_lock_support import JOB, PEER, build, describe, now

OTHER = "pid=1:turn=other:platform=cli"


@pytest.fixture
def real_ledger(tmp_path):
    led = Ledger(tmp_path / "data" / "ledger.sqlite3")
    try:
        yield led
    finally:
        led.close()


def _flags(db, info) -> dict:
    return {
        seg: {k: db.get_session(seg)[k] for k in ("archived", "auto_archived", "hidden", "ended_at", "end_reason")}
        for seg in info.lineage_ids
    }


def _undo(db, ledger, info, holder, how: str) -> dict | None:
    if how == "release":
        lock.release(db, ledger, info, holder, JOB)
        return None
    return lock.reclaim(db, ledger, info.tip_id, force=True)


# ---------------------------------------------------------------------------------------------------------
# L3 — both leases are given back when acquire fails after the turn lease
# ---------------------------------------------------------------------------------------------------------


def test_rv_b_l3_compression_lock_error_gives_the_turn_lease_back(session_db, make_session, real_ledger):
    """★ L3: ``try_acquire_compression_lock`` raises (Hermes' write patience ran out): ``acquire`` re-raises, the
    ledger row is ``released`` and another turn takes the turn lease at once. Before: the turn lease of this
    holder stayed until the TTL, so the session's next turn waited up to ``lease_ttl_s``."""
    tip = build(make_session, drivers=False)
    info = describe(session_db, tip)
    original = session_db.try_acquire_compression_lock

    def locked_store(*args, **kwargs):
        raise sqlite3.OperationalError("database is locked")

    session_db.try_acquire_compression_lock = locked_store
    with pytest.raises(sqlite3.OperationalError):
        lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    session_db.try_acquire_compression_lock = original
    assert real_ledger.source_state(JOB)["state"] == "released"
    assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)
    session_db.release_session_turn_lease(tip, OTHER)
    assert session_db.try_acquire_compression_lock(tip, "pid=1:c", ttl_seconds=30)


def test_rv_b_l3_ledger_error_after_both_leases_gives_both_back(session_db, make_session, tmp_path):
    """★ L3 (second door): the ledger write ``locked`` fails after both leases were taken; both leases go back."""

    class _Refusing(Ledger):
        def source_set(self, job_id, state, **fields):
            if getattr(state, "value", state) == "locked":
                raise sqlite3.OperationalError("database is locked")
            return super().source_set(job_id, state, **fields)

    led = _Refusing(tmp_path / "data" / "ledger.sqlite3")
    try:
        tip = build(make_session, drivers=False)
        info = describe(session_db, tip)
        with pytest.raises(sqlite3.OperationalError):
            lock.acquire(session_db, led, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
        assert led.source_state(JOB)["state"] == "released"
        assert session_db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)
        session_db.release_session_turn_lease(tip, OTHER)
        assert session_db.try_acquire_compression_lock(tip, "pid=1:c", ttl_seconds=30)
    finally:
        led.close()


# ---------------------------------------------------------------------------------------------------------
# L6 — the idle sweep's archive keeps its provenance
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("how", ["release", "reclaim"])
def test_rv_b_l6_auto_archived_session_stays_auto_archived(session_db, make_session, real_ledger, how):
    """★ L6: an ended session the idle sweep archived (``archive_stale_sessions``) comes back exactly as it was —
    archived by the sweep, so Hermes un-archives it on the next resume. Before: ``auto_archived`` 1 → 0 (prepare
    re-archived it deliberately, release archived it deliberately again)."""
    tip = build(make_session, drivers=False)
    session_db.end_session(tip, "cli_close")
    assert session_db.archive_stale_sessions(0) >= 1
    info = describe(session_db, tip)
    before = _flags(session_db, info)
    assert {r["auto_archived"] for r in before.values()} == {1}
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    assert {session_db.get_session(s)["hidden"] for s in info.lineage_ids} == {1}
    _undo(session_db, real_ledger, info, holder, how)
    assert _flags(session_db, info) == before


def test_rv_b_l6_open_auto_archived_session_is_never_left_deliberately_archived(session_db, make_session, real_ledger):
    """★ L6 (open session): the sweep also archives sessions that never ended. Release reopens the tip, which is a
    resume in Hermes' terms and lifts the sweep's archive; it must not leave a deliberate archive behind. Before:
    ``archived 1, auto_archived 0`` on every row (and Hermes would never lift it)."""
    tip = build(make_session, drivers=False)
    assert session_db.archive_stale_sessions(0) >= 1
    info = describe(session_db, tip)
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    lock.release(session_db, real_ledger, info, holder, JOB)
    after = _flags(session_db, info)
    assert not any(r["archived"] and not r["auto_archived"] for r in after.values()), after
    assert after[tip]["ended_at"] is None and after[tip]["hidden"] == 0


def test_rv_b_l6_deliberate_archive_unchanged(session_db, make_session, real_ledger):
    """Control: a deliberately archived session stays deliberately archived (no write at all on the flag)."""
    tip = build(make_session, drivers=False)
    session_db.set_session_archived(tip, True, notify=False)
    info = describe(session_db, tip)
    before = _flags(session_db, info)
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    lock.release(session_db, real_ledger, info, holder, JOB)
    assert _flags(session_db, info) == before


# ---------------------------------------------------------------------------------------------------------
# M9 — the canonical Bot Chat stays reachable
# ---------------------------------------------------------------------------------------------------------


def _bot_chat(make_session, session_db) -> str:
    sid = make_session.create(title=session_db.CANONICAL_BOT_CHAT_TITLE)
    make_session.turns(sid, 2)
    session_db.set_session_hidden(sid, True)
    return sid


@pytest.mark.parametrize("how", ["release", "reclaim"])
def test_rv_b_m9_bot_chat_replaced_meanwhile_comes_back_listed(session_db, make_session, real_ledger, how):
    """★ M9: Bot Mode mints a new canonical Bot Chat while the old one is handed over (archiving retired its name).
    After release/reclaim the old conversation is visible under a free title and the output says so; the new Bot
    Chat keeps the name. Before: ``title=None hidden=1`` — reachable only by id."""
    canonical = session_db.CANONICAL_BOT_CHAT_TITLE
    sid = _bot_chat(make_session, session_db)
    info = describe(session_db, sid)
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    newer = _bot_chat(make_session, session_db)
    if how == "release":
        lock.release(session_db, real_ledger, info, holder, JOB)
        warnings: list[str] = []
    else:
        lock.retire(session_db, real_ledger, info, holder, JOB, _ack(), [])
        warnings = lock.reclaim(session_db, real_ledger, sid, force=False)["warnings"]
    row = session_db.get_session(sid)
    assert row["hidden"] == 0 and row["archived"] == 0 and row["ended_at"] is None
    assert row["title"] == f"{canonical} (before handover)"
    assert session_db.get_session_title(newer) == canonical
    if how == "reclaim":
        assert any("back in the session list" in w for w in warnings), warnings


def test_rv_b_m9_bot_chat_without_replacement_is_restored_exactly(session_db, make_session, real_ledger):
    """Control: no new Bot Chat meanwhile ⇒ the canonical chat gets its name and ``hidden`` back unchanged."""
    canonical = session_db.CANONICAL_BOT_CHAT_TITLE
    sid = _bot_chat(make_session, session_db)
    info = describe(session_db, sid)
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    lock.release(session_db, real_ledger, info, holder, JOB)
    row = session_db.get_session(sid)
    assert (row["title"], row["hidden"], row["archived"]) == (canonical, 1, 0)


def _ack():
    from ctx_handover.tests.unit.B.test_lock_support import ack

    return ack()


# ---------------------------------------------------------------------------------------------------------
# Success sweep — ``set_session_title`` answers ``False`` when its compare-and-swap lost
# ---------------------------------------------------------------------------------------------------------


def _lost_cas_once(session_db, *, on_title=None):
    """``set_session_title`` reports ``False`` without writing, once (Hermes' CAS lost against another write)."""
    original = session_db.set_session_title
    state = {"armed": True}

    def lost(sid, title):
        if state["armed"] and (on_title is None or title == on_title):
            state["armed"] = False
            return False
        return original(sid, title)

    session_db.set_session_title = lost
    return original


def test_rv_b_sweep_p3_lost_cas_is_not_taken_as_marked(session_db, make_session, real_ledger):
    """★ Sweep (``lock.py`` p3): a rename answered ``False`` did not mark the title; prepare tries the job-suffixed
    form instead of counting the segment as marked. Before: ``break`` after any non-raising call ⇒ title unmarked."""
    tip = build(make_session, drivers=False)
    info = describe(session_db, tip)
    lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    original = _lost_cas_once(session_db)
    lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    session_db.set_session_title = original
    title = session_db.get_session_title(tip)
    assert title.startswith("Work (handed over to laptop ") and title.endswith(f", job {lock.short(JOB)})")


def test_rv_b_sweep_restore_lost_cas_is_reported(session_db, make_session, real_ledger):
    """★ Sweep (``lock.py`` restore): a title restore answered ``False`` left the marked title; release says so in
    its warnings. Before: silently counted as restored."""
    tip = build(make_session, drivers=False)
    info = describe(session_db, tip)
    holder = lock.acquire(session_db, real_ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    original = _lost_cas_once(session_db, on_title="Work")
    entry = real_ledger.source_state(JOB)
    out = lock._undo(
        session_db,
        real_ledger,
        JOB,
        entry,
        holder,
        from_states=lock._RELEASABLE,
        middle=lock.SourceState.RELEASING,
        final=lock.SourceState.RELEASED,
        action="release",
        sleep=lambda s: None,
    )
    session_db.set_session_title = original
    assert any("could not be restored" in w and "'Work'" in w for w in out["warnings"]), out["warnings"]
