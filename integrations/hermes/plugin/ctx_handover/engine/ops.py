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

import collections
import contextlib
import contextvars
import os
import re
import threading
from collections.abc import Callable, Iterable
from pathlib import Path
from typing import TYPE_CHECKING

from .errors import BadRequest, BlockCorrupt, HandoverError, NotFound
from .ids import ulid
from .jsonutil import from_wire, loads_bounded, sha256_hex, to_wire
from .ledger import Ledger, private_dir
from .model import OpState, OpStatus, Progress

if TYPE_CHECKING:
    from .contracts import BlockSource

try:  # POSIX
    import fcntl
except ImportError:  # pragma: no cover - Windows
    fcntl = None  # type: ignore[assignment]
try:  # Windows
    import msvcrt
except ImportError:
    msvcrt = None  # type: ignore[assignment]

OpFn = Callable[[Callable[[Progress], None]], dict]


class JobLock:
    """Exclusive OS file lock of one job (``flock``/``msvcrt.locking``), held while an operation that changes the
    file system runs. The kernel drops it only when the holder closes it or its process dies — a process that
    hangs (stopped, swapping) keeps it, so a second process can never run the same file-system steps in parallel
    even when the first one missed its heartbeats (fencing beyond the ledger's owner epoch)."""

    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self._fd: int | None = None

    def acquire(self) -> bool:
        """Take the lock without waiting; False when another holder has it."""
        private_dir(self.path.parent)
        fd = os.open(self.path, os.O_RDWR | os.O_CREAT, 0o600)
        try:
            if fcntl is not None:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            elif msvcrt is not None:  # pragma: no cover - Windows
                msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
        except OSError:
            os.close(fd)
            return False
        self._fd = fd
        return True

    def release(self) -> None:
        fd, self._fd = self._fd, None
        if fd is None:
            return
        with contextlib.suppress(OSError):
            if fcntl is not None:
                fcntl.flock(fd, fcntl.LOCK_UN)
            elif msvcrt is not None:  # pragma: no cover - Windows
                msvcrt.locking(fd, msvcrt.LK_UNLCK, 1)
        os.close(fd)

    def held_elsewhere(self) -> bool:
        """True when another open handle (another process or thread) holds the lock right now."""
        if not self.path.exists():
            return False
        probe = JobLock(self.path)
        if probe.acquire():
            probe.release()
            return False
        return True


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
        lock_dir: Path | None = None,
    ) -> None:
        self._ledger = ledger
        self._wrap = run_in_context or (lambda fn, *a, **kw: fn(*a, **kw))
        self._stale_s = float(stale_s)
        self._heartbeat_s = float(heartbeat_s)
        self._progress_interval_s = float(progress_interval_s)
        self._inline = inline
        self._lock_dir = Path(lock_dir) if lock_dir is not None else None
        self._lock = threading.Lock()
        self._live: dict[str, threading.Thread] = {}
        self._beat: threading.Thread | None = None
        self._stop = threading.Event()

    # -- public ----------------------------------------------------------------------------------------------
    def start(
        self, job_id: str, op: str, digest: str, fn: OpFn, *, restart_failed: bool = False, exclusive: bool = False
    ) -> OpStatus:
        """Start ``fn`` for ``(job_id, op, digest)`` or return the existing operation (see module docstring).

        ``restart_failed`` runs a failed operation again even when its error is not retryable — for operations
        made of idempotent, journaled sub-steps that must eventually finish (source retire/release).
        ``exclusive`` runs ``fn`` under the job's ``JobLock`` (operations that change the file system)."""
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
                    args=(self._wrap, self._body, job_id, op, digest, op_id, fn, exclusive),
                    name=f"ctxh-op-{op}-{op_id[-6:]}",
                    daemon=True,
                )
                self._live[op_id] = thread
                thread.start()
                self._ensure_heartbeat()
        if self._inline:
            self._body(job_id, op, digest, op_id, fn, exclusive)
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
        if stale and state is OpState.RUNNING and not self._is_live(row["op_id"]) and self._held(row["job_id"]):
            stale = False  # the heartbeat is late but the holder's process is alive (hung): not orphaned
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

    def job_lock(self, job_id: str) -> JobLock | None:
        return None if self._lock_dir is None else JobLock(self._lock_dir / f"{job_id}.lock")

    def _held(self, job_id: str) -> bool:
        lock = self.job_lock(job_id)
        return lock is not None and lock.held_elsewhere()

    def _is_live(self, op_id: str) -> bool:
        thread = self._live.get(op_id)
        return thread is not None and thread.is_alive()

    def _body(self, job_id: str, op: str, digest: str, op_id: str, fn: OpFn, exclusive: bool = False) -> None:
        lock = self.job_lock(job_id) if exclusive else None
        if lock is not None and not lock.acquire():
            error = {
                "code": "internal",
                "message": "another process still works on this job; try again",
                "retryable": True,
                "details": {"reason": "busy_elsewhere"},
            }
            self._ledger.op_put(job_id, op, digest, op_id, OpState.FAILED, error=error)
            with self._lock:
                self._live.pop(op_id, None)
            return
        try:
            self._run_body(job_id, op, digest, op_id, fn)
        finally:
            if lock is not None:
                lock.release()

    def _run_body(self, job_id: str, op: str, digest: str, op_id: str, fn: OpFn) -> None:
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


# ---------------------------------------------------------------------------------------------------------
# object closure of top-level documents (DESIGN §5.3, §6.2, §9.1) — shared by the conductor (what to move) and
# the source (what a pull peer may read, R18)
# ---------------------------------------------------------------------------------------------------------

_HEX64 = re.compile(r"^[0-9a-f]{64}$")


def _check_ids(ids: Iterable[object], where: str) -> list[str]:
    out = []
    for i in ids:
        if not isinstance(i, str) or not _HEX64.match(i):
            raise BadRequest(f"invalid object id in {where}", field=where)
        out.append(i)
    return out


def doc_refs(doc: dict) -> tuple[list[str], list[str]]:
    """Objects a top-level document references: ``(leaf objects, nested documents)`` (DESIGN §5.3, §6.2, §9.1)."""
    leaves: list[object] = []
    docs: list[object] = []
    kind = doc.get("kind") if isinstance(doc, dict) else None
    if kind == "ctxh.capsule":
        for seg in doc.get("segments") or []:
            if isinstance(seg.get("row_object"), str):  # cluster B keeps the segment row inline (a dict)
                leaves.append(seg["row_object"])
            leaves.extend(seg.get("pages") or [])
    elif kind == "ctxh.delta":
        leaves.extend(doc.get("pages") or [])
        if isinstance(doc.get("row_object"), str):
            leaves.append(doc["row_object"])
    elif kind == "ctxh.pack":
        for item in doc.get("items") or []:
            for key in ("pack", "object"):
                if item.get(key):
                    leaves.append(item[key])
            for key in ("manifest", "lfs_objects"):
                if item.get(key):
                    docs.append(item[key])
    elif kind == "ctxh.tree":
        for entry in doc.get("entries") or []:
            if entry.get("object"):
                leaves.append(entry["object"])
    return _check_ids(leaves, f"{kind}.refs"), _check_ids(docs, f"{kind}.docs")


def read_object(src: BlockSource, object_id: str, *, max_bytes: int, cache: dict[str, bytes] | None = None) -> bytes:
    """Assemble one object from ``src`` (size-capped, sha256-verified); raw blocks land in ``cache``."""
    (m,) = src.manifests([object_id])
    if m.size > max_bytes:
        raise BadRequest("document object too large", field="object", size=m.size, max_bytes=max_bytes)
    got: dict[str, bytes] = {}
    want = list(dict.fromkeys(m.blocks))
    for start in range(0, len(want), 4096):
        for bid, raw in src.read_blocks(want[start : start + 4096]):
            if sha256_hex(raw) == bid:
                got[bid] = raw
    if any(b not in got for b in want):
        raise BlockCorrupt("source is missing blocks of a document", object_id=object_id)
    data = b"".join(got[b] for b in m.blocks)
    if sha256_hex(data) != m.sha256 or len(data) != m.size:
        raise BlockCorrupt("document object hash mismatch", object_id=object_id)
    if cache is not None:
        cache.update(got)
    return data


def object_closure(src: BlockSource, root_id: str, *, max_doc_bytes: int, cache: dict[str, bytes]) -> list[str]:
    """``root_id`` plus every object it references, transitively through nested documents (tree manifests)."""
    out, seen = [root_id], {root_id}
    queue = collections.deque([root_id])
    while queue:
        oid = queue.popleft()
        doc = loads_bounded(read_object(src, oid, max_bytes=max_doc_bytes, cache=cache), max_bytes=max_doc_bytes)
        if not isinstance(doc, dict):
            raise BadRequest("document is not an object", field="object")
        leaves, docs = doc_refs(doc)
        for x in leaves:
            if x not in seen:
                seen.add(x)
                out.append(x)
        for x in docs:
            if x not in seen:
                seen.add(x)
                out.append(x)
                queue.append(x)
    return out
