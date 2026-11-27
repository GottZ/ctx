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
        pytest.skip("the adapter exists on a storage-interface host only")
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
        pytest.skip("the adapter exists on a storage-interface host only")
    sid = make_session.create()
    make_session.turns(sid, 1)
    rows = session_db.get_messages(sid)
    before = len(session_db.get_messages(sid, include_inactive=True))
    copy = [{**rows[0], "_row_id": rows[0]["id"], "_restored_from_store": True}]
    copy[0].pop("id")
    assert session_db.engine_db.append_messages_batch(sid, copy, chunk_rows=500) == 1
    assert len(session_db.get_messages(sid, include_inactive=True)) == before + 1


@pytest.mark.parametrize(
    ("given", "stored"),
    [
        ({"note": "kept"}, {"note": "kept"}),
        ('{"note": "kept"}', {"note": "kept"}),
        ("[1, 2]", None),
        ("not json", None),
        ("", None),
    ],
)
@pytest.mark.parametrize("writer", ["append_messages_batch", "archive_and_compact"])
def test_batch_writers_take_display_metadata_as_object_or_json_text(session_db, make_session, writer, given, stored):
    """Both batch writers the import calls store ``display_metadata`` given as an object or as its JSON text as that
    object, and any other text as none — what the old ``SessionDB`` writers store, so the check runs on both hosts.
    The other JSON fields of a row (``tool_calls``, the uid maps, the reasoning sidecars) and structured ``content``
    in its column form are stored alike on both hosts already (probe of issue GottZ/ctx#570, finding 2).

    Mutant: ``hostcompat._new_messages`` passes the text through → the storage-interface host raises ``ValueError``
    for every non-empty text."""
    db = getattr(session_db, "engine_db", session_db)
    sid = make_session.create()
    rows = [{"role": "user", "content": "q"}, {"role": "assistant", "content": "a", "display_metadata": given}]
    if writer == "append_messages_batch":
        db.append_messages_batch(sid, rows, chunk_rows=500)
    else:
        db.archive_and_compact(sid, rows)
    last = db.get_messages(sid)[-1]
    assert last["role"] == "assistant"
    assert last.get("display_metadata") == stored


def test_delete_names_the_core_s_media_root_without_creating_it(session_db, hermes_home, tmp_path):
    """``delete_session`` names the media root Hermes' own delete names (``native_inputs_root`` of the store's
    profile) and creates nothing to name it: for the ``HERMES_HOME`` profile, for a profile bound as the context's
    home (``set_hermes_home_override``), for one whose legacy ``document_cache/`` holds files, and for a store opened
    at another profile than the active one (that store's profile's root, not the active one's).

    Mutant: the media root from ``get_document_cache_dir()`` → naming it creates the profile's cache folder, and a
    store of another profile names the active profile's root."""
    if not is_store_host():
        pytest.skip("the adapter exists on a storage-interface host only")
    from gateway.session_ingress_media import native_inputs_root
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    from ctx_handover.tests.fakes.hoststore import open_session_db

    def core_root(home=None) -> str:
        token = None if home is None else set_hermes_home_override(home)
        try:
            return str(native_inputs_root())
        finally:
            if token is not None:
                reset_hermes_home_override(token)

    def tree(home) -> list[str]:
        return sorted(str(p.relative_to(home)) for p in home.rglob("*"))

    before = tree(hermes_home)
    named = hostcompat._media_root(session_db.engine_db._home)
    assert tree(hermes_home) == before
    assert named == core_root()

    work = tmp_path / "root" / "profiles" / "work"
    work.mkdir(parents=True)
    legacy = tmp_path / "root" / "profiles" / "legacy"
    (legacy / "document_cache").mkdir(parents=True)
    (legacy / "document_cache" / "kept.pdf").write_bytes(b"%PDF-1.7")
    for home in (work, legacy):
        db = open_session_db(home / "state.db")
        try:
            before = tree(home)
            token = set_hermes_home_override(home)
            try:
                named = hostcompat._media_root(db.engine_db._home)
                assert tree(home) == before
                assert named == str(native_inputs_root())
            finally:
                reset_hermes_home_override(token)
            assert hostcompat._media_root(db.engine_db._home) == core_root(home) != core_root()
        finally:
            db.close()
    assert core_root(legacy).startswith(str((legacy / "document_cache").resolve()))


def test_a_busy_store_in_a_lease_call_is_not_acquired(session_db, make_session, ledger, monkeypatch):
    """The store's ``StoreContention`` in a lease call reaches ``lock`` as the ``sqlite3.OperationalError`` it
    treats as "not acquired": ``acquire`` refuses (``SessionBusy``) and ``refresh`` reports a lost lease — never an
    unhandled store error in the middle of a handover.

    Mutant: the adapter lets ``TransientStoreError`` through → ``acquire`` and ``refresh`` raise it."""
    if not is_store_host():
        pytest.skip("the adapter exists on a storage-interface host only")
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


def test_any_store_error_in_a_lease_refresh_is_a_lost_lease(session_db, make_session, ledger, monkeypatch):
    """A store error that is not contention (a corrupt or replaced store) in a lease refresh reaches ``lock`` as a
    ``sqlite3.Error`` too, as the old class's ``DatabaseError`` did: ``refresh`` reports the lease lost, which the
    conductor counts, instead of raising the interface's error into the lease beat.

    Mutant: the adapter maps only ``TransientStoreError`` → ``refresh`` raises ``CorruptStoreError``."""
    if not is_store_host():
        pytest.skip("the adapter exists on a storage-interface host only")
    from persistence import CorruptStoreError

    tip = build(make_session, drivers=False)
    info = describe(session_db, tip)
    holder = lock.acquire(session_db, ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    coordination = session_db.store.coordination

    def corrupt(self, **kw):
        raise CorruptStoreError("state.db is corrupt")

    monkeypatch.setattr(type(coordination), "refresh_compression_lock", corrupt)
    assert lock.refresh(session_db, info, holder, ttl_s=120) is False
    with pytest.raises(sqlite3.DatabaseError):
        session_db.refresh_compression_lock(tip, holder, ttl_seconds=120)
    monkeypatch.undo()
    lock.release(session_db, ledger, info, holder, JOB)


def test_an_import_counts_only_the_sessions_it_wrote(session_db, make_session):
    """``import_sessions`` names a payload id that already exists under ``skipped_ids``, never under
    ``imported_ids`` — the engine discards and re-mints only what is not its own (``session_import._import_a``).

    Mutant: the adapter reports every payload id as imported."""
    if not is_store_host():
        pytest.skip("the adapter exists on a storage-interface host only")
    taken = make_session.create(title="Already here")
    fresh = make_session.mint_id()
    payload = [
        {"id": taken, "source": "cli", "messages": []},
        {"id": fresh, "source": "cli", "messages": [{"role": "user", "content": "hi"}]},
    ]
    result = session_db.engine_db.import_sessions(payload)
    assert result["ok"] is True
    assert result["imported_ids"] == [fresh] and result["skipped_ids"] == [taken]
    assert session_db.get_session(taken)["title"] == "Already here"
