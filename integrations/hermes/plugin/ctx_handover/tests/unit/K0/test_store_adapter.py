"""K0: ``hostcompat.StoreSessionDB`` — the old ``SessionDB`` surface over Hermes' storage interface (issue
GottZ/ctx#570).

On a storage-interface host the adapter's answers are checked against Hermes' own ``hermes_state.SessionDB``
stand-in where that carries the method (the core's statement of the old shapes), and the conversions the engine
relies on are pinned: rows written by the engine are always fresh inserts, a busy store in a lease call is the
``sqlite3.OperationalError`` ``lock`` treats as "not acquired", and nothing opens the database file itself. On a
``SessionDB`` host the engine gets the class itself and these tests only pin that.
"""

from __future__ import annotations

import sqlite3
import warnings

import pytest

from ctx_handover.engine import hostcompat, lock
from ctx_handover.engine.errors import SessionBusy
from ctx_handover.tests.fakes.hoststore import is_store_host
from ctx_handover.tests.unit.B.support import LedgerStub
from ctx_handover.tests.unit.B.test_lock_support import JOB, build, describe


@pytest.fixture
def ledger(tmp_path):
    led = LedgerStub(tmp_path / "ledger.sqlite3")
    try:
        yield led
    finally:
        led.close()


def _stand_in(hermes_home):
    """Hermes' own ``hermes_state.SessionDB`` stand-in over the same store (its one deprecation warning muted)."""
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)
        from hermes_state import SessionDB

        return SessionDB(hermes_home / "state.db")


def test_engine_store_is_the_class_on_a_sessiondb_host(session_db):
    if is_store_host():
        assert isinstance(session_db.engine_db, hostcompat.StoreSessionDB)
    else:
        from hermes_state import SessionDB

        assert type(session_db) is SessionDB


def test_adapter_shapes_equal_hermes_own_stand_in(session_db, make_session, hermes_home):
    """``get_session``, ``get_messages`` (every view the engine reads) and ``list_sessions_rich`` answer exactly what
    Hermes' stand-in answers for the same store: the adapter reproduces the old shapes, not an approximation."""
    if not is_store_host():
        return
    tip = build(make_session)
    root = session_db.get_compression_lineage(tip)[0]
    core = _stand_in(hermes_home)
    try:
        for sid in (root, tip):
            assert session_db.get_session(sid) == core.get_session(sid)
            for kw in ({}, {"include_inactive": True}, {"include_inactive": True, "after_id": 0, "limit": 500}):
                assert session_db.get_messages(sid, **kw) == core.get_messages(sid, **kw)
        for kw in (
            {"limit": 50},
            {"limit": 1, "min_message_count": 1, "order_by_last_active": True, "include_archived": False},
            {"limit": 50, "include_hidden": True, "include_children": True},
        ):
            assert session_db.list_sessions_rich(**kw) == core.list_sessions_rich(**kw)
    finally:
        core.close()


def test_rows_the_engine_writes_are_fresh_inserts(session_db, make_session):
    """A message dict that names a stored row (``_row_id``, ``_db_row_snapshot``) would make the store repair or
    adopt that row instead of inserting; the adapter drops those keys, so a batch always appends.

    Mutant: ``_new_messages`` keeps ``_row_id`` → the stored row is repaired and nothing is appended."""
    if not is_store_host():
        return
    sid = make_session.create()
    make_session.turns(sid, 1)
    rows = session_db.get_messages(sid)
    before = len(session_db.get_messages(sid, include_inactive=True))
    copy = [{**rows[0], "_row_id": rows[0]["id"], "_restored_from_store": True}]
    copy[0].pop("id")
    assert session_db.engine_db.append_messages_batch(sid, copy, chunk_rows=500) == 1
    assert len(session_db.get_messages(sid, include_inactive=True)) == before + 1


def test_a_busy_store_in_a_lease_call_is_not_acquired(session_db, make_session, ledger, monkeypatch):
    """The store's ``StoreContention`` in a lease call reaches ``lock`` as the ``sqlite3.OperationalError`` it
    treats as "not acquired": ``acquire`` refuses (``SessionBusy``) and ``refresh`` reports a lost lease — never an
    unhandled store error in the middle of a handover.

    Mutant: the adapter lets ``TransientStoreError`` through → ``acquire`` and ``refresh`` raise it."""
    if not is_store_host():
        return
    from persistence import StoreContention

    tip = build(make_session, drivers=False)
    info = describe(session_db, tip)
    holder = lock.acquire(session_db, ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    coordination = session_db.store.coordination

    def contended(**kw):
        raise StoreContention("state.db is busy")

    monkeypatch.setattr(type(coordination), "refresh_turn_lease", lambda self, **kw: contended(**kw))
    assert lock.refresh(session_db, info, holder, ttl_s=120) is False
    lock.release(session_db, ledger, info, holder, JOB)
    monkeypatch.setattr(type(coordination), "try_acquire_turn_lease", lambda self, **kw: contended(**kw))
    with pytest.raises(SessionBusy):
        lock.acquire(session_db, ledger, info, JOB + "2", ttl_s=120, wait_s=0, cancelled=lambda: False)
    with pytest.raises(sqlite3.OperationalError):
        session_db.try_acquire_session_turn_lease(tip, "pid=1:x", ttl_seconds=10, patience_s=0.1)
