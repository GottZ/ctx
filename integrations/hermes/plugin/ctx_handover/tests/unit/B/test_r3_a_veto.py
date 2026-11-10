# ruff: noqa: F811  (pytest fixtures imported from test_lock_support are redefined as parameters)
"""Review round 3 — the B side of R2-H1 and L1 (REVIEW-2 §3, §4).

R2-H1: ``VetoIndex.start`` returns only after the index read the ledger once (a concurrent caller waits for that
read); a failed first read does not stop the refresher. Red before the fix: ``lookup`` None right after ``start``.

L1: a session exported to a bundle (peer ``""`` at prepare, N55) is vetoed with a text about the bundle — not
"handed over to another machine … continues there as …". Red before the fix: the message names "another machine"."""

from __future__ import annotations

import threading
import time

from ctx_handover.engine import lock
from ctx_handover.engine.model import CommitAck
from ctx_handover.tests.unit.B.support import LedgerStub
from ctx_handover.tests.unit.B.test_lock_support import (  # noqa: F401 (fixtures)
    JOB,
    ack,
    build,
    describe,
    ledger,
    locked,
    now,
    prepared,
)


class _SlowStub(LedgerStub):
    """The ledger read of the index takes a while (a busy disk, a large ledger)."""

    delay = 0.3

    def veto_rows(self):
        time.sleep(self.delay)
        return super().veto_rows()


class _FailOnceStub(LedgerStub):
    failures = 1

    def data_version(self):
        if type(self).failures > 0:
            type(self).failures -= 1
            raise OSError("disk I/O error")
        return super().data_version()


def _bundle_ack(title: str = "Work") -> CommitAck:
    """The ack of a written bundle (``bundle._ack_for``): no target session yet."""
    return CommitAck(
        session_id="",
        root_id="",
        title=title,
        dests=(),
        path_map=(),
        model=None,
        offline_capable=True,
        warnings=("bundle written to /x/proj.hermes-handover",),
        committed_at=1_762_700_000.0,
    )


# -- R2-H1 ------------------------------------------------------------------------------------------------------


def test_r3_a_h1_start_returns_with_the_maps_loaded(session_db, make_session, ledger):
    """★ ``start`` reads the ledger on the calling thread before it returns.

    Mutant (``VetoIndex.start``): no synchronous first ``refresh_now(force=True)`` ⇒ ``lookup`` None."""
    tip = build(make_session)
    prepared(session_db, ledger, tip)
    index = lock.VetoIndex(ledger.path, poll_s=60, ledger_factory=_SlowStub)
    index.start()
    try:
        entry = index.lookup(tip)
        assert entry is not None and entry["state"] == "prepared"
        assert index.refreshed_at is not None and index.last_error is None
    finally:
        index.stop()


def test_r3_a_h1_a_concurrent_start_waits_for_the_first_read(session_db, make_session, ledger):
    tip = build(make_session)
    prepared(session_db, ledger, tip)
    index = lock.VetoIndex(ledger.path, poll_s=60, ledger_factory=_SlowStub)
    seen: list[object] = []
    barrier = threading.Barrier(3)

    def go() -> None:
        barrier.wait(5)
        index.start()
        seen.append(index.lookup(tip))

    threads = [threading.Thread(target=go) for _ in range(3)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(10)
    try:
        assert len(seen) == 3 and all(e is not None for e in seen), seen
        assert sum(1 for t in threading.enumerate() if t.name == "ctx_handover-veto") >= 1
    finally:
        index.stop()


def test_r3_a_h1_a_failed_first_read_is_retried_by_the_thread(session_db, make_session, ledger):
    """``start`` never raises: the error stays in ``last_error`` and the refresher loads the maps on its next tick."""
    tip = build(make_session)
    prepared(session_db, ledger, tip)
    _FailOnceStub.failures = 1
    index = lock.VetoIndex(ledger.path, poll_s=0.05, ledger_factory=_FailOnceStub)
    index.start()
    try:
        deadline = time.monotonic() + 10
        while index.lookup(tip) is None and time.monotonic() < deadline:
            time.sleep(0.02)
        assert index.lookup(tip) is not None
        assert index.last_error is None
    finally:
        index.stop()


def test_r3_a_h1_start_after_stop_does_not_read_again_on_the_caller(session_db, make_session, ledger):
    """The synchronous read is the first one only; a restarted index answers from its maps at once."""
    tip = build(make_session)
    prepared(session_db, ledger, tip)
    index = lock.VetoIndex(ledger.path, poll_s=60, ledger_factory=_SlowStub)
    index.start()
    index.stop()
    t0 = time.monotonic()
    index.start()
    try:
        assert time.monotonic() - t0 < _SlowStub.delay
        assert index.lookup(tip) is not None
    finally:
        index.stop()


# -- L1 -----------------------------------------------------------------------------------------------------------


def _exported(session_db, ledger, tip: str):
    """The export path of ``local_source``: prepare with peer "" (N55), then retire with the bundle ack."""
    info, holder = locked(session_db, ledger, tip)
    lock.prepare(session_db, ledger, info, JOB, "", now())
    return info, holder


def test_r3_a_l1_an_exported_session_is_vetoed_with_the_bundle_text(session_db, make_session, ledger):
    """★ After an export retired the source, the veto says the session went into a bundle — not that it continues
    on "another machine" under the bundle's title — and ``reclaim`` still recognises that text (X08).

    Mutant (``lock.veto_texts``): drop the bundle branch ⇒ "handed over to another machine … continues there as"."""
    tip = build(make_session)
    info, holder = _exported(session_db, ledger, tip)
    assert "(exported to bundle " in describe(session_db, tip).title
    lock.retire(session_db, ledger, info, holder, JOB, _bundle_ack(), [])
    index = lock.VetoIndex(ledger.path, ledger_factory=LedgerStub)
    index.refresh_now(force=True)
    entry = index.lookup(tip)
    assert entry is not None and entry["state"] == "retired"
    message = entry["message"]
    assert message == (
        "This session was exported to a handover bundle on 2025-11-09 14:53 UTC (job "
        f"{JOB[-6:]}); it continues where the bundle is imported. Your message was not sent. To continue here "
        "instead, run /handover reclaim."
    ), message
    assert "another machine" not in message and "continues there" not in message
    assert message in lock._job_texts(ledger, JOB, ledger.source_state(JOB))


def test_r3_a_l1_an_export_in_progress_names_the_bundle(session_db, make_session, ledger):
    tip = build(make_session)
    _exported(session_db, ledger, tip)
    index = lock.VetoIndex(ledger.path, ledger_factory=LedgerStub)
    index.refresh_now(force=True)
    message = index.lookup(tip)["message"]
    assert message.startswith(f"An export of this session to a handover bundle is being completed (job {JOB[-6:]})")
    assert "another machine" not in message
    assert message in lock._job_texts(ledger, JOB, ledger.source_state(JOB))


def test_r3_a_l1_a_handover_to_a_peer_keeps_its_text(session_db, make_session, ledger):
    """The bundle branch is the export's alone: a push names the peer as before."""
    tip = build(make_session)
    info, holder = prepared(session_db, ledger, tip)
    lock.retire(session_db, ledger, info, holder, JOB, ack(), [])
    index = lock.VetoIndex(ledger.path, ledger_factory=LedgerStub)
    index.refresh_now(force=True)
    message = index.lookup(tip)["message"]
    assert message.startswith("This session was handed over to laptop on ") and 'continues there as "Work"' in message
    assert lock.veto_texts({"state": "retired", "peer": None, "job_id": JOB}).startswith(
        "This session was handed over to another machine"
    )
