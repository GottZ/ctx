"""Ledger concurrency with two real processes (cluster A; TESTPLAN §2.2, BUILD-ORDER §6 item 1).

No threads, no mocks: each worker is a ``multiprocessing`` spawn child with its own ``Ledger`` and SQLite
connection on the same file, synchronised with a cross-process barrier so the calls really overlap.
"""

from __future__ import annotations

import multiprocessing as mp

import pytest

from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Peer, Right, Role
from ctx_handover.tests.unit.A import procs_helpers

CTX = mp.get_context("spawn")


def _collect(out, n: int) -> dict:
    return dict(out.get(timeout=60) for _ in range(n))


def _join(procs) -> None:
    for p in procs:
        p.join(timeout=60)
        assert p.exitcode == 0, f"{p.name} exit {p.exitcode}"


def test_ledger_redeem_pair_code_atomic_two_processes(tmp_path):
    """★ Two processes redeem the same codes at the same moment: exactly one peer row per code (SEC 1.3).

    Mutant: ``redeem_pair_code`` ignores the DELETE rowcount (inserts the peer although the code was already
    gone)."""
    path = tmp_path / "ledger.sqlite3"
    lg = Ledger(path)
    now = 2_000_000_000.0
    codes = [f"C{i:05d}" for i in range(12)]
    for code in codes:
        lg.put_pair_code(code, "secret", frozenset({Right.PUSH}), "", now + 600)
    barrier = CTX.Barrier(2)
    out = CTX.Queue()
    procs = [
        CTX.Process(target=procs_helpers.redeem_worker, args=(str(path), tag, codes, now, barrier, out), name=tag)
        for tag in ("pa", "pb")
    ]
    for p in procs:
        p.start()
    results = _collect(out, 2)
    _join(procs)

    wins = [a + b for a, b in zip(results["pa"], results["pb"], strict=True)]
    assert wins == [1] * len(codes), f"per-code winners {wins}"
    with lg.tx() as conn:
        n_peers = conn.execute("SELECT COUNT(*) FROM peers WHERE direction='inbound'").fetchone()[0]
        n_codes = conn.execute("SELECT COUNT(*) FROM pair_codes").fetchone()[0]
    assert n_peers == len(codes)
    assert n_codes == 0
    lg.close()


def test_ledger_fail_pair_code_caps_at_five(tmp_path):
    """★ Concurrent failures from two processes never push ``failures`` above 5; the code is burned (SEC 1.3).

    Mutant: ``fail_pair_code`` without ``AND failures < 5``."""
    path = tmp_path / "ledger.sqlite3"
    lg = Ledger(path)
    now = 2_000_000_000.0
    lg.put_pair_code("BURNME", "secret", frozenset({Right.PULL}), "", now + 600)
    barrier = CTX.Barrier(2)
    out = CTX.Queue()
    procs = [
        CTX.Process(target=procs_helpers.fail_worker, args=(str(path), tag, "BURNME", 8, barrier, out), name=tag)
        for tag in ("pa", "pb")
    ]
    for p in procs:
        p.start()
    results = _collect(out, 2)
    _join(procs)

    seen = results["pa"] + results["pb"]
    assert max(seen) == 5, seen
    with lg.tx() as conn:
        failures = conn.execute("SELECT failures FROM pair_codes WHERE code_id='BURNME'").fetchone()[0]
    assert failures == 5
    assert lg.pair_code("BURNME", now) is None
    late = Peer(name="late", peer_id="late", direction="inbound", url=None, pubkey="pk", rights=frozenset({Right.PULL}))
    assert lg.redeem_pair_code("BURNME", now, late) is False
    lg.close()


def test_old_owner_fenced_after_adoption(tmp_path):
    """★ Process A owns a job and stalls; process B adopts it (epoch + 1); A wakes up: every write of A fails
    and the journal holds nothing from A after the adoption [SK M5].

    Mutant: ``transition`` compares the state only (no owner-epoch CAS)."""
    path = tmp_path / "ledger.sqlite3"
    lg = Ledger(path)
    lg.create_job("J", Role.CONDUCTOR, Mode.PUSH, None, {"p": 1})
    ready, go = CTX.Event(), CTX.Event()
    out = CTX.Queue()
    proc = CTX.Process(target=procs_helpers.stalled_owner, args=(str(path), "J", ready, go, out), name="A")
    proc.start()
    assert ready.wait(timeout=60)

    assert lg.claim_owner("J", Role.CONDUCTOR, "proc-B", 60) is None  # A is alive
    new_epoch = lg.claim_owner("J", Role.CONDUCTOR, "proc-B", 0.0)  # A counts as stalled
    assert new_epoch == 2
    seq_at_adoption = lg.events("J", Role.CONDUCTOR)[-1]["seq"]
    go.set()
    report = out.get(timeout=60)
    proc.join(timeout=60)
    assert proc.exitcode == 0

    assert report["epoch"] == 1
    assert report["heartbeat"] is False
    assert report["transition"] == "fenced"
    assert report["event"] == "fenced"
    assert report["intent_commit"] == "fenced"
    row = lg.job("J", Role.CONDUCTOR)
    assert row["state"] == "hello" and row["owner"] == "proc-B" and row["owner_epoch"] == 2
    after = [e for e in lg.events("J", Role.CONDUCTOR) if e["seq"] > seq_at_adoption]
    assert after == []
    lg.close()


def test_ledger_source_lock_unique_across_connections(tmp_path):
    """The one-active-handover index holds across connections (a second ledger ⇒ AlreadyHandingOver)."""
    from ctx_handover.engine.errors import AlreadyHandingOver
    from ctx_handover.engine.model import SessionInfo

    path = tmp_path / "ledger.sqlite3"
    a, b = Ledger(path), Ledger(path)
    info = SessionInfo("t", "r", ("r", "t"), None, None, None, None, None, None, 1, (1,))
    a.source_lock("J1", info, "h", None)
    with pytest.raises(AlreadyHandingOver):
        b.source_lock("J2", info, "h", None)
    a.close()
    b.close()
