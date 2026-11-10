"""First review round, point 1 (M29 follow-up): two ctx outbox flushes running at once stored the same
rows twice. ``Runtime.janitor`` still flushed by itself on the first run of a process (``flush_outbox=_flush(ctx)``)
while the profile's ``ctxh-outbox`` thread flushed the same due rows; two processes of one profile (gateway and CLI)
did the same with one ledger file. Each flush read the due rows and sent all of them; the row was marked done only
after ``call_mcp`` returned.

Now a flush claims each row in the ledger before its first MCP call (``Ledger.outbox_claim``, one write
transaction): the row stops being due for every other flusher — in this process or another — until the claimer marks
it done, backs it off after a failure, or hands it back untried (``Ledger.outbox_release``)."""

from __future__ import annotations

import collections
import threading
import time

import pytest

from ctx_handover.engine import ctxrecords, jobs
from ctx_handover.engine.ledger import Ledger

CONSENT_SERVER = "ctx"


def _put(ledger, n: int) -> None:
    for i in range(n):
        ledger.outbox_put("departure", {"title": f"t{i}", "content": "c", "tags": [], "metadata": {}})


def _stores(calls) -> collections.Counter:
    return collections.Counter(args["title"] for _server, tool, args, _t in calls if tool == "store")


class BarrierCtx:
    """A ctx MCP server whose first ``store`` of each thread waits until two threads have reached one — so both
    flushers have read their due rows before either marks one done (the race of the finding, made certain)."""

    def __init__(self, parties: int = 2) -> None:
        self.calls: list[tuple[str, str, dict, float]] = []
        self.lock = threading.Lock()
        self.barrier = threading.Barrier(parties, timeout=10)
        self.waited: set[int] = set()

    def call_mcp(self, server: str, tool: str, arguments: dict | None = None, timeout: float = 30) -> dict:
        with self.lock:
            self.calls.append((server, tool, dict(arguments or {}), float(timeout)))
            first = threading.get_ident() not in self.waited
            self.waited.add(threading.get_ident())
        if first:
            try:
                self.barrier.wait()
            except threading.BrokenBarrierError:
                pass
        return {"ok": True}


def test_rv_final_two_flushes_at_once_send_each_row_once(tmp_path):
    """★ Two flushes at once over one ledger file (two processes of the profile: two ``Ledger`` objects) ⇒ each due
    row reaches ``call_mcp`` exactly once, and every row ends delivered.

    Red before the fix: ``Counter({'t0': 2, 't1': 2, 't2': 2, 't3': 2})`` — both flushers sent every row."""
    path = tmp_path / "ledger.sqlite3"
    gateway, cli = Ledger(path), Ledger(path)
    _put(gateway, 4)
    ctx = BarrierCtx()
    now = time.time()
    results: list[int] = []

    def run(ledger) -> None:
        results.append(ctxrecords.flush(ledger, ctx.call_mcp, CONSENT_SERVER, "handover", now, budget_s=30.0))

    threads = [threading.Thread(target=run, args=(led,)) for led in (gateway, cli)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(30)
    try:
        assert _stores(ctx.calls) == {"t0": 1, "t1": 1, "t2": 1, "t3": 1}, _stores(ctx.calls)
        assert sorted(results) and sum(results) == 4, results
        assert gateway.outbox_due(now + 10**9, 100) == []
    finally:
        gateway.close()
        cli.close()


@pytest.fixture
def runtime_env(fake_host):
    jobs._reset_for_tests()
    fake_host.config["memory"] = {"provider": "ctx_checkpoint"}
    fake_host.config.setdefault("plugins", {}).setdefault("entries", {}).setdefault("ctx_handover", {})[
        "mcp_allowlist"
    ] = [CONSENT_SERVER]
    yield fake_host
    jobs._reset_for_tests()


def test_rv_final_janitor_flush_beside_the_outbox_thread_sends_each_row_once(runtime_env, fake_ctx):
    """★ The scenario: the profile's ``ctxh-outbox`` thread is inside ``call_mcp`` for the first row when
    ``Runtime.janitor`` (first run of a process, its own ``flush_outbox``) flushes ⇒ still each row once.

    Red before the fix: the janitor sent the row the outbox thread was sending, and the outbox thread then sent the
    rows the janitor had already stored (``Counter({'t0': 2, 't1': 2, 't2': 2})``)."""
    entered, release = threading.Event(), threading.Event()
    calls: list[tuple[str, str, dict, float]] = []
    lock = threading.Lock()

    def call_mcp(server, tool, arguments=None, timeout=30):
        with lock:
            calls.append((server, tool, dict(arguments or {}), float(timeout)))
            first = len(calls) == 1
        if first:  # the outbox thread's first row hangs until the janitor's flush is over
            entered.set()
            release.wait(5)
        return {"ok": True}

    fake_ctx.call_mcp = call_mcp
    rt = jobs.ensure_started(fake_ctx)  # the first janitor of the process ran here (empty outbox)
    _put(rt.ledger, 3)
    flush = jobs._flush(fake_ctx)
    assert flush is not None
    rt.flush_in_background(flush)
    try:
        assert entered.wait(5), "the ctxh-outbox flush never called ctx"
        rt.janitor(background=False)
    finally:
        release.set()
        if rt.outbox_thread is not None:
            rt.outbox_thread.join(15)
    assert _stores(calls) == {"t0": 1, "t1": 1, "t2": 1}, _stores(calls)
    assert rt.ledger.outbox_due(rt.ledger.now() + 10**9, 100) == []


def test_rv_final_a_row_the_budget_did_not_reach_is_handed_back(tmp_path):
    """A row claimed for its checkpoint-head search but left untried because the budget ran out before its ``store``
    stays due at once (no wait for the claim to lapse, no backoff) — M29's "rows the budget did not reach stay due"
    holds with the claim."""
    ledger = Ledger(tmp_path / "ledger.sqlite3")
    ledger.outbox_put("departure", {"title": "t0", "content": "c", "tags": [], "metadata": {}, "source_root": "root0"})
    ticks = iter([0.0, 0.0, 0.0, 5.0, 5.0, 5.0])  # budget 2 s: the search spends it, the store is never started

    def clock() -> float:
        return next(ticks, 5.0)

    seen: list[str] = []

    def call_mcp(server, tool, arguments=None, timeout=30):
        seen.append(tool)
        return {"ok": True, "result": []}

    now = time.time()
    try:
        assert ctxrecords.flush(ledger, call_mcp, CONSENT_SERVER, "handover", now, clock=clock) == 0
        assert seen == ["search"], seen
        due = ledger.outbox_due(now, 10)
        assert [r["payload"]["title"] for r in due] == ["t0"]
        assert due[0]["attempts"] == 0
    finally:
        ledger.close()


def test_rv_final_claim_is_one_row_one_sender(tmp_path):
    """``Ledger.outbox_claim`` is True for exactly one of two claimers; a claimed row is not due; a failed store backs
    it off as before; ``outbox_release`` hands back only the claim it names."""
    path = tmp_path / "ledger.sqlite3"
    a, b = Ledger(path), Ledger(path)
    try:
        _put(a, 1)
        now = time.time()
        (row,) = a.outbox_due(now, 10)
        until = now + 30.0  # shorter than the first backoff (60 s), so the backoff is visible below
        assert a.outbox_claim(row["id"], now, until) is True
        assert b.outbox_claim(row["id"], now, until) is False
        assert b.outbox_due(now, 10) == []
        assert b.outbox_release(row["id"], until + 1.0, row["next_try_at"]) is False  # not this claim
        assert a.outbox_release(row["id"], until, row["next_try_at"]) is True
        assert [r["id"] for r in b.outbox_due(now, 10)] == [row["id"]]
        assert b.outbox_claim(row["id"], now, until) is True
        b.outbox_done(row["id"], False, now)
        (backed_off,) = a.outbox_due(now + 10**6, 10)
        assert backed_off["attempts"] == 1 and backed_off["next_try_at"] > until
        assert a.outbox_claim(row["id"], now, until) is False  # backed off: not due now
    finally:
        a.close()
        b.close()
