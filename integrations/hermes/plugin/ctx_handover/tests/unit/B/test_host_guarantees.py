"""The plugin's lock and import guarantees against Hermes' own primitives, on either session store (issue
GottZ/ctx#570).

Each test takes the foreign side through the path Hermes itself uses on the running host — the agent's turn-lease
admission (``persistence.authority.coordination.acquire_turn_lease_waiting`` over the store's coordination on the
storage interface, ``SessionDB.acquire_session_turn_lease`` on the old class) and the compressor's own lock — so a
store whose leases the plugin reached the wrong way, or not at all, shows here:

* no handover starts inside a running Hermes turn, and no Hermes turn starts while a handover holds the session
  (it waits behind the lease and runs only after the release);
* no handover starts inside a running compaction, and no compaction starts while the session is locked;
* an import never writes into a session that already exists on the target, and a second import of the same capsule
  lands as a lineage of its own (no double import into one row set);
* an imported session passes Hermes' own resume guard and replays exactly what the source replayed.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine import lock, session_export, session_import
from ctx_handover.engine.errors import SessionBusy
from ctx_handover.engine.session_hash import h_model
from ctx_handover.tests.fakes.hoststore import is_store_host
from ctx_handover.tests.unit.B.conftest import assert_lineage_equal, export, plan, rich_lineage
from ctx_handover.tests.unit.B.test_lock_support import JOB, build, describe

HERMES_TURN = "pid=1:turn=relay-1:platform=cli"
"""A holder in the agent's own shape (``agent/turn_facade_lease.py`` ``admit_durable_turn_lease``); pid 1 is never
this test's process, and with no pid-namespace stamp it is reclaimed only after its TTL."""


def hermes_turn_lease(db, session_id: str, *, wait_seconds: float) -> bool:
    """Take the session's turn lease the way a Hermes turn does on this host, waiting up to ``wait_seconds``."""
    if is_store_host():
        from persistence.authority.coordination import acquire_turn_lease_waiting

        return bool(
            acquire_turn_lease_waiting(
                db.store.coordination,
                session_id=session_id,
                holder=HERMES_TURN,
                ttl_seconds=120,
                wait_seconds=wait_seconds,
                poll_interval_seconds=0.1,
            )
        )
    return bool(
        db.acquire_session_turn_lease(
            session_id, HERMES_TURN, ttl_seconds=120, wait_seconds=wait_seconds, poll_interval_seconds=0.1
        )
    )


def hermes_release_turn(db, session_id: str) -> None:
    db.release_session_turn_lease(session_id, HERMES_TURN)


def test_a_hermes_turn_waits_behind_the_handover_lease(session_db, make_session, ledger):
    """While a handover holds the session, the agent's own admission of a turn waits and gives up (no turn runs
    in the middle of the handover); once the handover released its leases, the same admission succeeds.

    Mutant: ``lock.acquire`` skips the turn lease (only the compression lock) → the turn is admitted at once."""
    tip = build(make_session, drivers=False)
    info = describe(session_db, tip)
    holder = lock.acquire(session_db, ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    assert not hermes_turn_lease(session_db, tip, wait_seconds=0.5)
    assert not hermes_turn_lease(session_db, info.root_id, wait_seconds=0.2)  # keyed by the lineage root
    lock.release(session_db, ledger, info, holder, JOB)
    assert hermes_turn_lease(session_db, tip, wait_seconds=0.5)
    hermes_release_turn(session_db, tip)


def test_no_handover_inside_a_running_hermes_turn(session_db, make_session, ledger):
    """A turn the agent admitted refuses the lock (``SessionBusy``, reason ``turn``), takes no compression lock
    and leaves the ledger row released.

    Mutant: ``acquire`` treats a refused ``try_acquire_session_turn_lease`` as acquired."""
    tip = build(make_session, drivers=False)
    assert hermes_turn_lease(session_db, tip, wait_seconds=0.5)
    info = describe(session_db, tip)
    with pytest.raises(SessionBusy) as err:
        lock.acquire(session_db, ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    assert err.value.details["reason"] == "turn"
    assert session_db.get_compression_lock_holder(tip) is None
    assert ledger.source_state(JOB)["state"] == "released"
    hermes_release_turn(session_db, tip)


def test_no_handover_inside_a_compaction_and_no_compaction_while_locked(session_db, make_session, ledger):
    """A compaction holding the compression lock refuses the lock (reason ``compression``) and the turn lease taken
    first is given back; while a handover holds the session, the compressor's own lock attempt fails.

    Mutant: ``acquire`` ignores a refused ``try_acquire_compression_lock``."""
    tip = build(make_session, drivers=False)
    compressor = "pid=1:compress:agent=main"
    assert session_db.try_acquire_compression_lock(tip, compressor, ttl_seconds=120)
    info = describe(session_db, tip)
    with pytest.raises(SessionBusy) as err:
        lock.acquire(session_db, ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
    assert err.value.details["reason"] == "compression"
    assert hermes_turn_lease(session_db, tip, wait_seconds=0.5)  # the turn lease was given back
    hermes_release_turn(session_db, tip)
    session_db.release_compression_lock(tip, compressor)
    holder = lock.acquire(session_db, ledger, info, JOB + "2", ttl_s=120, wait_s=0, cancelled=lambda: False)
    assert not session_db.try_acquire_compression_lock(tip, compressor, ttl_seconds=120)
    lock.release(session_db, ledger, info, holder, JOB + "2")
    assert session_db.try_acquire_compression_lock(tip, compressor, ttl_seconds=120)


def test_an_import_never_writes_into_an_existing_session(
    make_session, session_db, target_db, store, source_paths, target_cwd
):
    """A planned id that a session on the target took meanwhile is re-minted: that session keeps its rows and
    title, and the import lands under fresh ids. Importing the same capsule again lands a second, separate lineage
    — never into the rows of the first.

    Mutant: ``_import_once`` without ``_require_free`` and with every payload id counted as imported."""
    tip = rich_lineage(make_session)
    info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    p = plan(target_db, capsule, target_cwd)
    taken = p.segments[0].new_id
    target_db.create_session(taken, "cli")
    target_db.set_session_title(taken, "Somebody else")
    make_session.twin(target_db).turns(taken, 1)
    foreign_rows = target_db.get_messages(taken, include_inactive=True)
    first = session_import.import_hidden(target_db, store, cap.capsule_id, p)
    assert taken not in (first.root_id, first.tip_id)
    assert target_db.get_messages(taken, include_inactive=True) == foreign_rows
    assert target_db.get_session(taken)["title"] == "Somebody else"
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [first.root_id, first.tip_id])
    again = session_import.import_hidden(target_db, store, cap.capsule_id, plan(target_db, capsule, target_cwd))
    assert {again.root_id, again.tip_id}.isdisjoint({first.root_id, first.tip_id, taken})
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [again.root_id, again.tip_id])
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [first.root_id, first.tip_id])


def test_an_imported_session_passes_hermes_resume_guard(
    make_session, session_db, target_db, store, source_paths, target_cwd
):
    """After the commit step (``unhide``) Hermes' own resume reads accept the imported tip: the resume guard
    passes, the resume target of the root is the tip, and the history the model sees on resume equals the
    source's (H_model)."""
    tip = rich_lineage(make_session, cwd=str(target_cwd))
    _info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    prepared = session_import.import_hidden(target_db, store, cap.capsule_id, plan(target_db, capsule, target_cwd))
    session_import.unhide(target_db, prepared)
    assert target_db.assert_resume_safe(prepared.tip_id) >= 1
    assert target_db.resolve_resume_session_id(prepared.root_id) == prepared.tip_id
    assert h_model(target_db.get_resume_conversations(prepared.tip_id)[0]) == h_model(
        session_db.get_resume_conversations(tip)[0]
    )
    assert target_db.get_session(prepared.tip_id)["hidden"] == 0
