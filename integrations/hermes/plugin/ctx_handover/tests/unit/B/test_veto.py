# ruff: noqa: F811  (pytest fixtures imported from test_lock_support are redefined as parameters)
"""lock.VetoIndex and veto texts: lineage-bound, no I/O in the callback, never raising (DESIGN §7.6, §11.4)."""

from __future__ import annotations

import builtins
import os
import socket
import sqlite3
import time

import pytest

from ctx_handover.engine import lock
from ctx_handover.tests.unit.B.support import LedgerStub
from ctx_handover.tests.unit.B.test_lock_support import (  # noqa: F401 (fixtures)
    JOB,
    PEER,
    ack,
    build,
    index_for,
    ledger,
    prepared,
)


def _retired(session_db, make_session, ledger, title="Work"):
    tip = build(make_session, title=title)
    info, holder = prepared(session_db, ledger, tip)
    lock.retire(session_db, ledger, info, holder, JOB, ack(title), [])
    return info


def test_veto_follows_lineage_after_rotation(session_db, make_session, ledger, tmp_path):
    """After retire a resumed copy (the core's resume reopens the tip) is compacted by the real rotation path;
    the new child is vetoed through the hook's ``parent_session_id`` and through ``post_compaction``, and the
    learned id reaches the ledger so a restarted gateway still vetoes it [SK B1].

    Mutant: ``VetoIndex.lookup`` ignores ``parent_session_id`` and ``on_compaction`` learns nothing (id-only).
    """
    info = _retired(session_db, make_session, ledger)
    index = index_for(tmp_path, ledger.path)
    assert all(index.lookup(s) for s in info.lineage_ids)
    session_db.reopen_session(info.tip_id)  # what a resume without the plugin does (cli_agent_setup_mixin.py:580)
    child = make_session.rotate(info.tip_id, keep_last=2)
    assert session_db.get_compression_lineage(child)[-1] == child
    assert index.lookup(child) is None  # unknown until the hooks tell us
    via_parent = index.lookup(child, parent_session_id=info.tip_id)
    assert via_parent is not None and via_parent["job_id"] == JOB
    index.on_compaction(child, info.tip_id)
    entry = index.lookup(child)
    assert entry is not None and entry["root_id"] == info.root_id
    assert entry["message"] == lock.veto_texts(entry)
    index.refresh_now()
    assert child in {r["session_id"] for r in ledger.veto_rows()}
    restarted = index_for(tmp_path, ledger.path)
    assert restarted.lookup(child) is not None


def test_veto_never_raises_on_ledger_error(session_db, make_session, ledger, tmp_path):
    """An unreadable ledger keeps the last good map: known ids stay vetoed, the callback never raises [SK B1].

    Mutant: ``refresh_now`` replaces the maps with empty ones when the ledger read fails.
    """
    info = _retired(session_db, make_session, ledger)
    index = index_for(tmp_path, ledger.path)
    index._get_ledger().fail_reads = True
    ledger.source_set(JOB, "retired", title="moved")  # a change the refresher can no longer read
    assert index.refresh_now(force=True) is False
    assert index.last_error and "injected" in index.last_error
    assert index.lookup(info.tip_id) is not None
    for weird in (None, 123, object(), "", "x" * 10_000):
        assert index.lookup(weird) is None  # type: ignore[arg-type]

    class Exploding(dict):
        def get(self, *a, **kw):
            raise RuntimeError("boom")

    index._by_id = Exploding()
    assert index.lookup(info.tip_id) is None
    index.on_compaction("child", info.tip_id)  # never raises either

    def broken_factory(path):
        raise sqlite3.OperationalError("unable to open database file")

    blind = lock.VetoIndex(tmp_path / "missing" / "ledger.sqlite3", ledger_factory=broken_factory)
    assert blind.refresh_now() is False
    assert blind.lookup(info.tip_id) is None


def test_veto_callback_no_io(session_db, make_session, ledger, tmp_path, monkeypatch):
    """``lookup``/``arrival``/``on_compaction`` touch no file, database or socket [SK B1]."""
    info = _retired(session_db, make_session, ledger)
    index = index_for(tmp_path, ledger.path)
    touched: list[str] = []

    def forbid(name):
        def _f(*a, **kw):
            touched.append(name)
            raise AssertionError(f"I/O in the veto callback: {name}")

        return _f

    with monkeypatch.context() as m:
        for mod, name in (
            (builtins, "open"),
            (os, "open"),
            (os, "stat"),
            (os, "lstat"),
            (os, "scandir"),
            (sqlite3, "connect"),
            (socket, "socket"),
            (time, "sleep"),
        ):
            m.setattr(mod, name, forbid(f"{mod.__name__}.{name}"))
        hit = index.lookup(info.tip_id)
        miss = index.lookup("20261109_000000_000000")
        index.on_compaction("20261109_999999_abcdef", info.tip_id)
        child = index.lookup("20261109_999999_abcdef")
        note = index.arrival(info.tip_id)
    assert touched == []
    assert hit is not None and miss is None and child is not None and note is None


def test_veto_message_texts():
    base = {"job_id": JOB, "peer": PEER, "prepared_at": 1_762_646_400.0, "root_id": "r", "session_id": "s"}
    prepared_text = lock.veto_texts({**base, "state": "prepared"})
    assert prepared_text == (
        f"A handover of this session to laptop is being completed (job {JOB[-6:]}). Your message was not sent. "
        "/handover status shows progress; /handover reclaim --force takes the session back if it is stuck."
    )
    retired = lock.veto_texts({**base, "state": "retired", "ack": {"title": "Work", "committed_at": 1_762_650_000.0}})
    assert retired == (
        f"This session was handed over to laptop on 2025-11-09 01:00 UTC (job {JOB[-6:]}) and continues there as "
        '"Work". Your message was not sent. To continue here instead, run /handover reclaim.'
    )
    assert (
        lock.veto_texts({**base, "state": "retiring", "ack": '{"title": "Work", "committed_at": 1762650000}'})
        == retired
    )
    exported = lock.veto_texts({**base, "state": "exported"})
    assert exported == (
        "This session was exported to a handover file on 2025-11-09 00:00 UTC. Run /handover reclaim to continue here."
    )
    assert "another machine" in lock.veto_texts({**base, "peer": None, "state": "prepared"})
    assert "an unknown time" in lock.veto_texts({**base, "prepared_at": None, "state": "exported"})


def test_veto_index_filters_states_and_tracks_arrivals(session_db, make_session, ledger, tmp_path):
    """Only prepared/retiring/retired/exported veto; ``locked``/``preparing`` queue behind the lease. Arrival rows
    in ``pending``/``offered`` are served from memory (DESIGN §11.4)."""
    from ctx_handover.tests.unit.B.test_lock_support import locked

    tip = build(make_session, title="Locked")
    locked(session_db, ledger, tip, "01JLOCKEDONLY")
    ledger.veto_add("01JLOCKEDONLY", [tip], tip)
    ledger.arrival_put(
        session_ids=["20261109_120000_abcdef"],
        root_id="20261109_120000_abcdef",
        job_id=JOB,
        peer=PEER,
        note="note",
        report_path="",
        path_map=[],
        continue_prompt=None,
    )
    index = index_for(tmp_path, ledger.path)
    assert index.lookup(tip) is None
    assert index.arrival("20261109_120000_abcdef")["note"] == "note"
    assert index.arrival(tip) is None


def test_veto_refresher_thread_picks_up_changes(session_db, make_session, ledger, tmp_path):
    tip = build(make_session)
    index = lock.VetoIndex(ledger.path, poll_s=0.02, ledger_factory=LedgerStub)
    index.start()
    try:
        assert index.lookup(tip) is None
        prepared(session_db, ledger, tip)
        deadline = time.monotonic() + 10
        while index.lookup(tip) is None and time.monotonic() < deadline:
            time.sleep(0.02)
        assert index.lookup(tip)["state"] == "prepared"
        index.start()  # idempotent
    finally:
        index.stop()
    assert index._thread is None


@pytest.mark.parametrize("state", ["released", "reclaimed", "locking", "suspended"])
def test_veto_texts_default_to_in_progress(state):
    text = lock.veto_texts({"state": state, "job_id": JOB, "peer": PEER})
    assert text.startswith("A handover of this session to laptop is being completed")
