"""Progress reporters for handover jobs (layer L10; DESIGN §14.1 "Progress", §2.1 consequence 5).

* ``PostMessageReporter`` — one ``ctx.post_message`` handle created by the slash handler (Desktop shows it as a
  sticky notice, LAB L2), edited from the job thread at most every ``throttle_s`` seconds. Best effort: without
  ``post_message`` (plugin host, ``hermes serve``) or when an edit fails nothing is shown and nothing raises;
  ``/handover status`` stays authoritative.
* ``TtyReporter`` — the CLI's progress line (``\\r`` on a TTY, plain lines otherwise, JSON lines with ``--json``).
* ``NullReporter`` — no output.
* ``ReporterBroker`` — hands the reporter a handler bound to the job runner's ``reporter_factory(job_id)``.

A reporter never raises into the caller (``contracts.Reporter``).
"""

from __future__ import annotations

import contextlib
import contextvars
import json
import logging
import sys
import threading
import time
from collections import OrderedDict
from collections.abc import Callable, Iterator
from typing import Any, TextIO

from .model import Progress

logger = logging.getLogger(__name__)

MiB = 1024**2


def format_progress(p: Progress, peer: str | None, direction: str, *, eta_s: float | None = None) -> str:
    """``Handover → laptop · packing workspace · 312/1024 MiB (30 %) · ~2 min left`` (DESIGN §14.1)."""
    arrow = "→" if direction == "out" else "←"
    parts = [f"Handover {arrow} {peer or 'bundle'}", p.step or "working"]
    if p.bytes_total:
        pct = int(100 * p.bytes_done / p.bytes_total) if p.bytes_total else 0
        parts.append(f"{p.bytes_done // MiB}/{p.bytes_total // MiB} MiB ({pct} %)")
    elif p.items_total:
        parts.append(f"{p.items_done}/{p.items_total}")
    if eta_s is not None and eta_s > 0:
        minutes = max(1, round(eta_s / 60))
        parts.append(f"~{minutes} min left")
    if p.message:
        parts.append(p.message)
    return " · ".join(parts)


CLOSING_STEPS = frozenset({"done", "aborted", "suspended"})
"""Conductor stop states (``conductor.STOP_STATES``): the runner reports one of them last and nothing follows — the
final text replaces the notice for good. A cancel by the user ends ``aborted`` with ``last_error.code ==
"cancelled"``."""

PARKING_STEPS = frozenset({"in_doubt", "retiring"})
"""States the runner may park in (the janitor resumes them) or pass through on the way to ``done``: the final text
is shown, but later progress may still replace it."""

TERMINAL_STEPS = CLOSING_STEPS | PARKING_STEPS
"""``Progress.step`` values that may be the last a runner reports (W111); they render the final text, unthrottled."""

_MAX_WARNINGS = 5


class NullReporter:
    """Reports nothing."""

    def progress(self, p: Progress) -> None:
        return None

    def info(self, text: str) -> None:
        return None

    def warn(self, text: str) -> None:
        return None

    def finish(self, text: str) -> None:
        return None


class _Rate:
    """Byte rate over the last minutes for a rough ETA."""

    def __init__(self) -> None:
        self._first: tuple[float, int] | None = None

    def eta(self, now: float, p: Progress) -> float | None:
        if not p.bytes_total or p.bytes_done <= 0:
            return None
        if self._first is None or p.bytes_done < self._first[1]:
            self._first = (now, p.bytes_done)
            return None
        elapsed = now - self._first[0]
        moved = p.bytes_done - self._first[1]
        if elapsed <= 0 or moved <= 0:
            return None
        return (p.bytes_total - p.bytes_done) / (moved / elapsed)


class PostMessageReporter:
    """A sticky ``post_message`` notice edited in place (see module docstring).

    ``on_done`` renders the final text when the runner reports a terminal step; without it the last progress
    line stays. Nothing is posted before ``start`` (the planning part runs inside the handler, which answers
    itself). After a closing step (``CLOSING_STEPS``) the notice is final; after a parking step it stays open,
    because the runner may continue to ``done`` from there. The handle is created by ``start`` in the handler
    thread (the conversation is bound there).
    """

    def __init__(
        self,
        ctx: Any,
        throttle_s: float = 2.0,
        *,
        clock: Callable[[], float] = time.monotonic,
        peer: str | None = None,
        direction: str = "out",
        on_done: Callable[[], str] | None = None,
        enabled: bool = True,
    ) -> None:
        self._ctx = ctx
        self._enabled = bool(enabled)
        self._throttle = float(throttle_s)
        self._clock = clock
        self._peer = peer
        self._direction = direction
        self._on_done = on_done
        self._handle: Any = None
        self._last_edit = float("-inf")
        self._line = ""
        self._warnings: list[str] = []
        self._rate = _Rate()
        self._lock = threading.Lock()
        self._closed = False
        self._started = False

    def _post(self, text: str) -> None:
        # Feature detection, not a hard call: in the plugin host ``post_message`` is a stub that raises
        # (``hermes_cli/plugin_isolation.py`` HOST_UNSUPPORTED_CTX_METHODS); the plugin still runs there.
        post = getattr(self._ctx, "post_message", None) if self._enabled else None
        if not callable(post):
            return
        try:
            handle = post(text)
        except Exception:
            logger.debug("post_message failed", exc_info=True)
            return
        if handle is not None and callable(getattr(handle, "edit", None)):
            self._handle = handle

    def _edit(self, text: str, *, force: bool) -> None:
        now = self._clock()
        if not force and now - self._last_edit < self._throttle:
            return
        self._last_edit = now
        if self._handle is None:
            self._post(text)
            return
        try:
            result = self._handle.edit(text)
            if isinstance(result, dict) and result.get("ok") is False:
                logger.debug("progress edit not delivered: %s", result.get("error"))
        except Exception:
            logger.debug("progress edit failed", exc_info=True)

    def _compose(self) -> str:
        if not self._warnings:
            return self._line
        return "\n".join([self._line, *(f"warning: {w}" for w in self._warnings[-_MAX_WARNINGS:])])

    def start(self, text: str) -> None:
        """Post the first message (the pre-send summary, SEC 5.8) and keep its handle."""
        with self._lock:
            self._started = True
            self._line = str(text)
            self._last_edit = self._clock()
            self._post(self._line)

    def progress(self, p: Progress) -> None:
        try:
            with self._lock:
                if self._closed or not self._started:
                    return  # planning runs inside the handler, which answers itself; the notice starts with start()
                if p.step in TERMINAL_STEPS:
                    text = None
                    if self._on_done is not None:
                        try:
                            text = self._on_done()
                        except Exception:
                            logger.debug("final text failed", exc_info=True)
                    self._line = text or format_progress(p, self._peer, self._direction)
                    self._closed = text is not None and p.step in CLOSING_STEPS
                    self._edit(self._compose() if text is None else text, force=True)
                    return
                eta = self._rate.eta(self._clock(), p)
                self._line = format_progress(p, self._peer, self._direction, eta_s=eta)
                self._edit(self._compose(), force=False)
        except Exception:
            logger.debug("progress report failed", exc_info=True)

    def info(self, text: str) -> None:
        try:
            with self._lock:
                if self._started and not self._closed:
                    self._line = str(text)
                    self._edit(self._compose(), force=False)
        except Exception:
            logger.debug("info report failed", exc_info=True)

    def warn(self, text: str) -> None:
        try:
            with self._lock:
                if str(text) not in self._warnings:
                    self._warnings.append(str(text))
        except Exception:
            logger.debug("warn report failed", exc_info=True)

    def finish(self, text: str) -> None:
        """Replace the notice with the final text (unthrottled)."""
        try:
            with self._lock:
                self._closed = True
                self._line = str(text)
                self._edit(self._line, force=True)
        except Exception:
            logger.debug("finish report failed", exc_info=True)


class TtyReporter:
    """Progress for ``hermes handover …`` in a terminal (see module docstring)."""

    def __init__(
        self,
        stream: TextIO | None = None,
        *,
        json_mode: bool = False,
        throttle_s: float = 0.5,
        clock: Callable[[], float] = time.monotonic,
        peer: str | None = None,
        direction: str = "out",
    ) -> None:
        self._stream = stream if stream is not None else sys.stderr
        self._json = json_mode
        self._throttle = throttle_s
        self._clock = clock
        self._peer = peer
        self._direction = direction
        self._last = float("-inf")
        self._rate = _Rate()
        self._lock = threading.Lock()
        self._dirty = False
        try:
            self._tty = bool(self._stream.isatty())
        except (AttributeError, ValueError):
            self._tty = False

    def _write(self, text: str) -> None:
        try:
            self._stream.write(text)
            self._stream.flush()
        except (OSError, ValueError):
            pass

    def _event(self, event: dict) -> None:
        self._write(json.dumps(event, ensure_ascii=False, sort_keys=True) + "\n")

    def progress(self, p: Progress) -> None:
        with self._lock:
            now = self._clock()
            if p.step not in TERMINAL_STEPS and now - self._last < self._throttle:
                return
            self._last = now
            if self._json:
                self._event(
                    {
                        "event": "progress",
                        "step": p.step,
                        "bytes_done": p.bytes_done,
                        "bytes_total": p.bytes_total,
                        "items_done": p.items_done,
                        "items_total": p.items_total,
                        "message": p.message,
                    }
                )
                return
            line = format_progress(p, self._peer, self._direction, eta_s=self._rate.eta(now, p))
            if self._tty:
                self._write("\r\x1b[K" + line)
                self._dirty = True
            else:
                self._write(line + "\n")

    def _line(self, prefix: str, text: str, event: str) -> None:
        with self._lock:
            if self._json:
                self._event({"event": event, "text": str(text)})
                return
            if self._dirty:
                self._write("\n")
                self._dirty = False
            self._write(f"{prefix}{text}\n")

    def info(self, text: str) -> None:
        self._line("", text, "info")

    def warn(self, text: str) -> None:
        self._line("warning: ", text, "warning")

    def finish(self, text: str) -> None:
        self._line("", text, "final")


class ReporterBroker:
    """Maps job ids to the reporter a handler bound (see module docstring); remembers up to 64 jobs."""

    def __init__(self) -> None:
        self._current: contextvars.ContextVar[Any] = contextvars.ContextVar("ctx_handover_reporter", default=None)
        self._by_job: OrderedDict[str, Any] = OrderedDict()
        self._lock = threading.Lock()

    @contextlib.contextmanager
    def bind(self, reporter: Any) -> Iterator[Any]:
        token = self._current.set(reporter)
        try:
            yield reporter
        finally:
            self._current.reset(token)

    def factory(self, job_id: str) -> Any:
        with self._lock:
            reporter = self._by_job.get(job_id)
            if reporter is None:
                reporter = self._current.get() or NullReporter()
                self._by_job[job_id] = reporter
                while len(self._by_job) > 64:
                    self._by_job.popitem(last=False)
            return reporter

    def release(self, job_id: str) -> None:
        with self._lock:
            self._by_job.pop(job_id, None)
