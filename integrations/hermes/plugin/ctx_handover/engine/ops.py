"""Asynchronous participant operations (layer L3, stdlib only; INTERFACES §4.5, DESIGN §3.7, I7).

Every long participant step (`plan`, `export`, `pack`, `prepare`, `apply_delta`, `reset_session`, `commit`,
`retire`, `release`) is an operation: ``start`` returns at once with an ``OpStatus`` and the work runs in a
worker thread; the caller polls ``status``. Operations are keyed by ``(job_id, op, digest)`` in the ledger, so a
repeated start with the same input returns the same operation (running or finished) — also from another
process, because the result is durable.

Restart rules (I9, DESIGN §3.6):

* ``done`` — returned as is; the function never runs again for that digest.
* ``failed`` and not retryable — returned as is.
* ``failed`` and retryable — a new start runs the function again under the same ``op_id``.
* ``running`` in this process, or ``running`` with a fresh ``updated_at`` (another live process runs it) —
  returned as is.
* ``running`` but stale (no heartbeat for ``stale_s``; its process died) — ``status`` reports it as failed with
  ``details.reason = "interrupted"`` (retryable) and a new start runs it again. Operation functions are
  therefore written to resume from their own journal (target commit, source retire/release).

A worker runs in a copy of the caller's ``contextvars`` context (DESIGN §2.1 consequence 6). A
``BaseException`` that is not an ``Exception`` (interpreter shutdown, a simulated crash in tests) is not
recorded: the row stays ``running`` as if the process had died.
"""

from __future__ import annotations

import contextlib
import contextvars
import threading
from collections.abc import Callable

from .errors import HandoverError, NotFound
from .ids import ulid
from .jsonutil import from_wire, to_wire
from .ledger import Ledger
from .model import OpState, OpStatus, Progress

OpFn = Callable[[Callable[[Progress], None]], dict]


def error_dict(exc: BaseException) -> dict:
    """The error envelope body (``code, message, retryable, details``) recorded for a failed operation."""
    if isinstance(exc, HandoverError):
        return exc.to_wire()["error"]
    return {
        "code": "internal",
        "message": f"{type(exc).__name__}: {exc}"[:1024],
        "retryable": True,
        "details": {"exception": type(exc).__name__},
    }


class OpRunner:
    """Starts and tracks operations of this process (see module docstring)."""

    def __init__(
        self,
        ledger: Ledger,
        run_in_context: Callable | None = None,
        *,
        stale_s: float = 60.0,
        heartbeat_s: float = 10.0,
        progress_interval_s: float = 1.0,
        inline: bool = False,
    ) -> None:
        self._ledger = ledger
        self._wrap = run_in_context or (lambda fn, *a, **kw: fn(*a, **kw))
        self._stale_s = float(stale_s)
        self._heartbeat_s = float(heartbeat_s)
        self._progress_interval_s = float(progress_interval_s)
        self._inline = inline
        self._lock = threading.Lock()
        self._live: dict[str, threading.Thread] = {}
        self._beat: threading.Thread | None = None
        self._stop = threading.Event()

    # -- public ----------------------------------------------------------------------------------------------
    def start(self, job_id: str, op: str, digest: str, fn: OpFn, *, restart_failed: bool = False) -> OpStatus:
        """Start ``fn`` for ``(job_id, op, digest)`` or return the existing operation (see module docstring).

        ``restart_failed`` runs a failed operation again even when its error is not retryable — for operations
        made of idempotent, journaled sub-steps that must eventually finish (source retire/release)."""
        with self._lock:
            row = self._ledger.op_get(job_id, op, digest)
            if row is not None:
                st = self._status_of(row)
                restart = st.state is OpState.FAILED and (restart_failed or bool((st.error or {}).get("retryable")))
                if not restart:
                    return st
                op_id = row["op_id"]
            else:
                op_id = ulid()
            self._ledger.op_put(job_id, op, digest, op_id, OpState.RUNNING)
            if self._inline:
                pass
            else:
                ctx = contextvars.copy_context()
                thread = threading.Thread(
                    target=ctx.run,
                    args=(self._wrap, self._body, job_id, op, digest, op_id, fn),
                    name=f"ctxh-op-{op}-{op_id[-6:]}",
                    daemon=True,
                )
                self._live[op_id] = thread
                thread.start()
                self._ensure_heartbeat()
        if self._inline:
            self._body(job_id, op, digest, op_id, fn)
        return self.status(job_id, op_id)

    def status(self, job_id: str, op_id: str) -> OpStatus:
        row = self._ledger.op_by_id(op_id)
        if row is None or row["job_id"] != job_id:
            raise NotFound(f"operation {op_id} not found")
        return self._status_of(row)

    def wait(self, job_id: str, op_id: str, timeout: float = 30.0) -> OpStatus:
        """Block until the operation leaves ``running`` in this process (tests and the CLI foreground)."""
        thread = self._live.get(op_id)
        if thread is not None:
            thread.join(timeout)
        return self.status(job_id, op_id)

    def join_all(self, timeout: float = 30.0) -> None:
        for thread in list(self._live.values()):
            thread.join(timeout)

    def live(self) -> set[str]:
        with self._lock:
            return {k for k, t in self._live.items() if t.is_alive()}

    def stop(self) -> None:
        self._stop.set()

    # -- internals ---------------------------------------------------------------------------------------------
    def _status_of(self, row: dict) -> OpStatus:
        state = OpState(row["state"])
        error = row["error"]
        stale = row["updated_at"] < self._ledger.now() - self._stale_s
        if state is OpState.RUNNING and not self._is_live(row["op_id"]) and stale:
            state = OpState.FAILED
            error = {
                "code": "internal",
                "message": "operation interrupted (process restart); start it again",
                "retryable": True,
                "details": {"reason": "interrupted"},
            }
            self._ledger.op_put(
                row["job_id"],
                row["op"],
                row["digest"],
                row["op_id"],
                OpState.FAILED,
                error=error,
                progress=row["progress"],
            )
        progress = from_wire(Progress, row["progress"]) if isinstance(row["progress"], dict) else None
        return OpStatus(op_id=row["op_id"], state=state, result=row["result"], error=error, progress=progress)

    def _is_live(self, op_id: str) -> bool:
        thread = self._live.get(op_id)
        return thread is not None and thread.is_alive()

    def _body(self, job_id: str, op: str, digest: str, op_id: str, fn: OpFn) -> None:
        last = [0.0]

        def report(p: Progress) -> None:
            now = self._ledger.now()
            if now - last[0] < self._progress_interval_s and last[0]:
                return
            last[0] = now
            with contextlib.suppress(Exception):  # progress is best effort
                self._ledger.op_progress(op_id, to_wire(p))

        try:
            try:
                result = fn(report)
            except Exception as exc:
                self._ledger.op_put(job_id, op, digest, op_id, OpState.FAILED, error=error_dict(exc))
                return
            except BaseException:  # process death (or its simulation): leave the row "running"
                return
            self._ledger.op_put(job_id, op, digest, op_id, OpState.DONE, result=dict(result or {}))
        finally:
            with self._lock:
                self._live.pop(op_id, None)

    def _ensure_heartbeat(self) -> None:
        if self._beat is not None and self._beat.is_alive():
            return
        ctx = contextvars.copy_context()
        self._beat = threading.Thread(target=ctx.run, args=(self._heartbeat_loop,), name="ctxh-op-beat", daemon=True)
        self._beat.start()

    def _heartbeat_loop(self) -> None:
        while not self._stop.wait(self._heartbeat_s):
            ids = self.live()
            if not ids:
                return
            for op_id in ids:
                with contextlib.suppress(Exception):
                    self._ledger.op_progress(op_id, None)
