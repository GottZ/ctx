"""Review round rv-f, M29 (``review-code/REVIEW.md`` §2, g6 M3): ctx was in the critical path — ``EngineBackend``
flushed the ctx outbox synchronously before every job start and on ``/handover status``, each due row with a
``search`` and a ``store`` of up to 10 s (D12, DESIGN §13: "ctx never in the critical path").

Now a job start leaves the flush to the runner's ``before_job`` (cluster A: the profile's one ``ctxh-outbox`` thread),
``status`` starts that same background flush (``Runtime.flush_in_background``, one at a time — two flushers would store
the same rows twice), and ``ctxrecords.flush`` keeps to a total budget (``FLUSH_BUDGET_S``) wherever it runs: no MCP
call starts once it is spent, and each call's timeout is what is left of it."""

from __future__ import annotations

import threading
import time
import types

from ctx_handover.engine import ctxrecords, jobs
from ctx_handover.engine.commands import Plugin
from ctx_handover.engine.model import Mode, Options, SessionRef
from ctx_handover.tests.fakes.surfaces_fake import FakeOutboxLedger

from .test_engine_backend import backend  # noqa: F401 - pytest fixture

HANG_CAP_S = 3.0
CONSENT = {
    "memory": {"provider": "ctx_checkpoint"},
    "plugins": {"entries": {"ctx_handover": {"mcp_allowlist": ["ctx"]}}},
}


class HungCtx:
    """A ctx MCP server that answers nothing: each call lasts its whole timeout (capped for the test)."""

    def __init__(self) -> None:
        self.calls: list[tuple[str, float]] = []
        self.threads: list[str] = []
        self.lock = threading.Lock()

    def call_mcp(self, server: str, tool: str, arguments: dict | None = None, timeout: float = 30) -> dict:
        with self.lock:
            self.calls.append((tool, float(timeout)))
            self.threads.append(threading.current_thread().name)
        time.sleep(min(float(timeout), HANG_CAP_S))
        raise TimeoutError(f"MCP call {tool} timed out")


def _record(ledger, n: int) -> None:
    for i in range(n):
        ledger.outbox_put(
            "departure",
            {"title": f"t{i}", "content": "c", "tags": [], "metadata": {}, "source_root": f"root{i}"},
        )


def _join_flushes(timeout: float = 15.0) -> None:
    for thread in threading.enumerate():
        if thread.name == "ctxh-outbox":
            thread.join(timeout)


def _profile_flusher(eb) -> None:
    """The fixture's runtime gets cluster A's real ``Runtime.flush_in_background`` (one ``ctxh-outbox`` thread)."""
    rt = eb._parts["runtime"]
    rt.outbox_thread = None
    rt.flush_in_background = types.MethodType(jobs.Runtime.flush_in_background, rt)


def test_rv_f_m29_flush_keeps_to_its_budget(tmp_path):
    """★ M29: a hung ctx server ⇒ ``ctxrecords.flush`` returns within its total budget; rows it did not reach stay
    due without a backoff (they were never tried), and no call gets a timeout beyond what is left of the budget.

    Red before the fix: two rows took ``2 × (search + store)`` full timeouts (12 s here, 40 s against a real 10 s
    timeout) — ``assert 12.0… < 3.5``."""
    ledger = FakeOutboxLedger(tmp_path / "outbox.sqlite3")
    _record(ledger, 2)
    ctx = HungCtx()
    started = time.monotonic()
    stored = ctxrecords.flush(ledger, ctx.call_mcp, "ctx", "handover", 1.0)
    elapsed = time.monotonic() - started
    budget = float(getattr(ctxrecords, "FLUSH_BUDGET_S", 2.0))
    assert stored == 0
    assert elapsed < budget + 1.5, (elapsed, ctx.calls)
    assert all(t <= budget for _tool, t in ctx.calls), ctx.calls
    assert len(ledger.outbox_due(1.0, 20)) >= 1, "a row the budget did not reach must stay due"


def test_rv_f_m29_job_start_does_not_wait_for_ctx(backend, fake_host):  # noqa: F811 - fixture
    """★ M29: with ctx consent, a due record and a hung ctx server, ``start_job`` reaches ``plan_sync`` at once and no
    MCP call runs on the caller's thread (the flush is the runner's ``before_job``, cluster A).

    Red before the fix: ``start_job`` returned after the synchronous flush (≥ 6 s here)."""
    _profile_flusher(backend)
    fake_host.config = dict(CONSENT)
    _record(backend.ledger_stub, 1)
    ctx = HungCtx()
    backend._ctx.call_mcp = ctx.call_mcp
    try:
        started = time.monotonic()
        job_id, _final = backend.start_job(
            Mode.PUSH,
            None,
            Options(),
            session_ref=SessionRef(id="s"),
            calling_session="s",
            session_key=None,
            bundle_path=None,
            confirm=None,
            side_question=None,
            reporter=None,
        )
        elapsed = time.monotonic() - started
        assert job_id and backend._parts["runner"].calls[-1][0] == "plan_sync"
        assert elapsed < 1.0, elapsed
    finally:
        _join_flushes()
    assert threading.current_thread().name not in ctx.threads, ctx.threads


def test_rv_f_m29_status_does_not_wait_for_ctx(backend, fake_host):  # noqa: F811 - fixture
    """★ M29: ``/handover status`` answers at once with the outbox count while a hung ctx server is being tried in the
    background (the profile's one ``ctxh-outbox`` flusher); a second status does not start a second flush.

    Red before the fix: the status waited for the synchronous flush (≥ 6 s here)."""
    _profile_flusher(backend)
    fake_host.config = dict(CONSENT)
    _record(backend.ledger_stub, 1)
    ctx = HungCtx()
    backend._ctx.call_mcp = ctx.call_mcp
    plugin = Plugin(backend._ctx, backend_factory=lambda c, p: backend)
    try:
        started = time.monotonic()
        out = plugin.slash("status", session_id=None, session_key=None, platform="local", source=object())
        again = plugin.slash("status", session_id=None, session_key=None, platform="local", source=object())
        elapsed = time.monotonic() - started
        assert elapsed < 1.0, (elapsed, out)
        assert "1 ctx records waiting for ctx." in out, out
        assert "1 ctx records waiting for ctx." in again, again
        flushes = [t for t in threading.enumerate() if t.name == "ctxh-outbox"]
        assert len(flushes) <= 1, flushes
    finally:
        _join_flushes()
    assert ctx.threads and set(ctx.threads) == {"ctxh-outbox"}, ctx.threads


def test_rv_f_m29_status_still_names_a_missing_consent(backend, fake_host):  # noqa: F811 - fixture
    """M29: the status line about the missing ``mcp_allowlist`` entry stays (computed without any MCP call)."""
    _profile_flusher(backend)
    fake_host.config = {"memory": {"provider": "ctx_checkpoint"}}
    _record(backend.ledger_stub, 1)
    ctx = HungCtx()
    backend._ctx.call_mcp = ctx.call_mcp
    plugin = Plugin(backend._ctx, backend_factory=lambda c, p: backend)
    out = plugin.slash("status", session_id=None, session_key=None, platform="local", source=object())
    _join_flushes()
    assert "/handover setup" in out and "1 ctx records waiting for ctx." in out, out
    assert ctx.calls == []
