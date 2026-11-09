"""Continuation on the target: one detached ``hermes chat --resume <id> --query=<prompt>`` (layer L3, stdlib only;
DESIGN §11.5, R10, R22, SEC 4.4).

The continuation is a single-query turn, so Hermes applies ``approvals.single_query_mode`` (default ``deny``: a
command that needs approval is refused instantly). This module never weakens that:

* the child environment drops every auto-approve and safety-bypass variable (``SCRUBBED_ENV``), the plugin-host
  marker and the parent's bound session variables; ``--yolo``/``--accept-hooks`` are never passed;
* nothing starts when this gateway runs with approvals switched off (``approvals_off()``) — there is no override;
* nothing starts for an unverified origin (unsigned bundle, unknown peer), and a request from a paired device only
  with this gateway's ``allow_remote_continue`` (default off; the ``allow_remote`` callable);
* a peer's prompt is capped at ``PROMPT_MAX`` characters and prefixed with ``[continuation requested by <peer>]``.

The prompt travels as ``--query=<prompt>`` (one argv element), so a prompt such as ``-p x`` can never be read as
an option by Hermes' pre-argparse scanners. The child is detached (``start_new_session`` on POSIX,
``DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`` on Windows), reads nothing (stdin = ``/dev/null``) and writes to
``<log_dir>/continue-<job>.log`` (0600, directory 0700). A waiter thread in the spawning process reaps the child
and appends the exit code as a trailer line; ``result()`` reads it back from any process.
"""

from __future__ import annotations

import json
import logging
import os
import re
import shutil
import subprocess
import sys
import threading
import time
from collections.abc import Callable, Mapping
from pathlib import Path
from typing import Any

from .errors import CapabilityMissing, ContinueRefused, HandoverError
from .ids import is_ulid
from .model import ContinueResult

logger = logging.getLogger(__name__)

SCRUBBED_ENV: tuple[str, ...] = (
    "HERMES_YOLO_MODE",
    "HERMES_ACCEPT_HOOKS",
    "HERMES_IGNORE_RULES",
    "HERMES_IGNORE_USER_CONFIG",
    "HERMES_SAFE_MODE",
)
"""Auto-approve and safety-bypass variables never inherited by the child (``hermes_cli/gateway_chat.py:41-43``)."""

ALSO_DROPPED: tuple[str, ...] = ("HERMES_PLUGIN_HOST_PROCESS",)
"""Process-role markers of the spawning process (plugin host) that must not describe the child."""

DROPPED_PREFIXES: tuple[str, ...] = ("HERMES_SESSION_",)
"""Session bindings of the spawning handler (``HERMES_SESSION_ID``/``_KEY``/``_PLATFORM`` …); the child binds
its own."""

PROMPT_MAX = 2000
DEFAULT_PROMPT = "Continue where we left off."
PEER_NAME_MAX = 64

_SESSION_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")
_CONTROL_RE = re.compile(r"[\x00-\x08\x0b-\x1f\x7f]")
_PEER_RE = re.compile(r"[^A-Za-z0-9_.@ -]")
_TRAILER_RE = re.compile(r"\[ctx_handover\] continuation exited with code (-?\d+)")
_HERMES_NAMES = frozenset({"hermes", "hermes.exe"})

_DETACHED_PROCESS = 0x00000008
_NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)
_CREATE_NEW_PROCESS_GROUP = 0x00000200


def job_files(log_dir: Path, job_id: str) -> tuple[Path, Path]:
    """The two files a job's continuation leaves in ``log_dir``: the child's log and the sidecar with pid, start
    and exit code (``continue-<job>.log``, ``continue-<job>.json``). The one place that names them — ``gc`` removes
    exactly these (SEC 6.3)."""
    return Path(log_dir) / f"continue-{job_id}.log", Path(log_dir) / f"continue-{job_id}.json"


def _clean_prompt(prompt: str | None) -> str:
    text = DEFAULT_PROMPT if prompt is None or not str(prompt).strip() else str(prompt)
    text = _CONTROL_RE.sub("", text.replace("\r\n", "\n"))
    return text[:PROMPT_MAX]


def _clean_peer(name: str) -> str:
    cleaned = _PEER_RE.sub("", _CONTROL_RE.sub("", str(name))).strip()
    return (cleaned or "a paired device")[:PEER_NAME_MAX]


class Spawner:
    """Starts and reports the detached continuation child of one job (see module docstring).

    ``approvals_off`` is evaluated at spawn time (the gateway's live configuration). The keyword-only
    parameters after it exist for tests and platform shims; production callers pass the first three.
    """

    def __init__(
        self,
        *,
        log_dir: Path,
        profile: str | None,
        approvals_off: Callable[[], bool],
        allow_remote: Callable[[], bool] = lambda: False,
        which: Callable[[str], str | None] = shutil.which,
        argv0: str | None = None,
        popen: Callable[..., Any] = subprocess.Popen,
        environ: Mapping[str, str] | None = None,
        windows: bool | None = None,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self._log_dir = Path(log_dir)
        self._profile = profile
        self._approvals_off = approvals_off
        self._allow_remote = allow_remote
        self._which = which
        self._argv0 = argv0
        self._popen = popen
        self._environ = environ
        self._windows = (os.name == "nt") if windows is None else bool(windows)
        self._clock = clock
        self._lock = threading.Lock()

    # -- argv and environment -------------------------------------------------------------------------
    def hermes_argv(self) -> list[str]:
        """``[hermes]`` or ``[hermes, "-p", profile]`` for a non-default profile (DESIGN §11.5, §15.4)."""
        exe = self._which("hermes")
        if not exe:
            argv0 = self._argv0 if self._argv0 is not None else (sys.argv[0] if sys.argv else "")
            if argv0 and Path(argv0.replace("\\", "/")).name.lower() in _HERMES_NAMES:
                exe = os.path.abspath(argv0) if not self._windows else argv0
        if not exe:
            raise CapabilityMissing(
                "The hermes command was not found on PATH, so the continuation cannot start.", capability="hermes_cli"
            )
        argv = [str(exe)]
        if self._profile and self._profile != "default":
            argv += ["-p", str(self._profile)]
        return argv

    def child_env(self) -> dict[str, str]:
        """The parent's environment without auto-approve, plugin-host and session-binding variables."""
        source = os.environ if self._environ is None else self._environ
        return {
            key: value
            for key, value in source.items()
            if key not in SCRUBBED_ENV and key not in ALSO_DROPPED and not key.startswith(DROPPED_PREFIXES)
        }

    # -- paths ----------------------------------------------------------------------------------------
    def _paths(self, job_id: str) -> tuple[Path, Path]:
        return job_files(self._log_dir, job_id)

    def _ensure_log_dir(self) -> None:
        self._log_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        try:
            os.chmod(self._log_dir, 0o700)
        except OSError:
            pass

    @staticmethod
    def _write_json(path: Path, data: dict) -> None:
        tmp = path.with_name(path.name + ".tmp")
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | _NOFOLLOW, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(data, fh, sort_keys=True)
        os.replace(tmp, path)

    @staticmethod
    def _read_json(path: Path) -> dict | None:
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return None
        return data if isinstance(data, dict) else None

    # -- spawn ----------------------------------------------------------------------------------------
    def spawn(
        self,
        session_id: str,
        prompt: str | None,
        job_id: str,
        *,
        requested_by: str | None,
        verified: bool = True,
        cwd: str | None = None,
    ) -> ContinueResult:
        """Start the detached continuation child once per job (a repeat returns the recorded child).

        ``requested_by`` names the paired device that asked (push ``--continue``); such a request runs only when
        this gateway's ``allow_remote_continue`` (the ``allow_remote`` callable) is on. Raises ``ContinueRefused``
        (``details.reason`` = ``unverified`` | ``not_allowed`` | ``approvals_off``) when the turn must not run
        unattended here, ``CapabilityMissing`` without a ``hermes`` executable, ``HandoverError`` for invalid ids
        or a failed start.
        """
        if not isinstance(session_id, str) or not _SESSION_ID_RE.fullmatch(session_id):
            raise HandoverError("invalid session id for the continuation", field="session_id")
        if not is_ulid(job_id):  # job ids are ULIDs everywhere (contract K-2, ``ids.ULID_RE``)
            raise HandoverError("invalid job id for the continuation", field="job_id")
        if not verified:
            raise ContinueRefused(
                "The handover came from an unverified source, so no turn is started automatically.",
                reason="unverified",
            )
        if requested_by and self._allow_remote() is not True:  # only a real boolean True opens it (no "false" text)
            raise ContinueRefused(
                "This gateway does not allow a paired device to start a turn (allow_remote_continue is off).",
                reason="not_allowed",
            )
        if self._approvals_off():
            raise ContinueRefused(
                "Approvals are switched off on this gateway, so no turn is started without a person present.",
                reason="approvals_off",
            )
        text = _clean_prompt(prompt)
        if requested_by:
            text = f"[continuation requested by {_clean_peer(requested_by)}] {text}"
        log_path, meta_path = self._paths(job_id)
        with self._lock:
            recorded = self._read_json(meta_path)
            if recorded and recorded.get("pid"):
                return ContinueResult(started=True, pid=int(recorded["pid"]), log=str(log_path), via="subprocess")
            argv = [*self.hermes_argv(), "chat", "--resume", session_id, "--query=" + text]
            self._ensure_log_dir()
            kwargs: dict[str, Any] = {
                "stdin": subprocess.DEVNULL,
                "stderr": subprocess.STDOUT,
                "env": self.child_env(),
                "close_fds": True,
            }
            if cwd and os.path.isdir(cwd):
                kwargs["cwd"] = cwd
            if self._windows:
                flags = getattr(subprocess, "DETACHED_PROCESS", _DETACHED_PROCESS)
                flags |= getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", _CREATE_NEW_PROCESS_GROUP)
                kwargs["creationflags"] = flags
            else:
                kwargs["start_new_session"] = True
            fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND | _NOFOLLOW, 0o600)
            try:
                os.write(fd, f"[ctx_handover] continuation of job {job_id} (session {session_id})\n".encode())
                kwargs["stdout"] = fd
                try:
                    proc = self._popen(argv, **kwargs)
                except OSError as exc:
                    raise HandoverError(f"could not start the continuation: {type(exc).__name__}") from exc
            finally:
                os.close(fd)
            pid = int(getattr(proc, "pid", 0) or 0)
            self._write_json(
                meta_path,
                {
                    "job_id": job_id,
                    "session_id": session_id,
                    "pid": pid,
                    "started_at": self._clock(),
                    "log": str(log_path),
                    "prompt_chars": len(text),
                    "requested_by": _clean_peer(requested_by) if requested_by else None,
                },
            )
        self._start_waiter(proc, job_id)
        return ContinueResult(started=True, pid=pid, log=str(log_path), via="subprocess")

    def _start_waiter(self, proc: Any, job_id: str) -> None:
        wait = getattr(proc, "wait", None)
        if not callable(wait):
            return

        def reap() -> None:
            try:
                code = int(wait())
            except Exception:  # the child is gone or not ours to wait for
                return
            log_path, meta_path = self._paths(job_id)
            try:
                fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND | _NOFOLLOW, 0o600)
                try:
                    os.write(fd, f"\n[ctx_handover] continuation exited with code {code}\n".encode())
                finally:
                    os.close(fd)
                with self._lock:
                    meta = self._read_json(meta_path) or {}
                    meta["exit_code"] = code
                    meta["ended_at"] = self._clock()
                    self._write_json(meta_path, meta)
            except OSError:
                logger.debug("continuation trailer for %s not written", job_id, exc_info=True)

        threading.Thread(target=reap, name=f"ctx_handover-continue-{job_id[-6:]}", daemon=True).start()

    # -- status ---------------------------------------------------------------------------------------
    def result(self, job_id: str) -> dict:
        """``{started, pid, log, running, exit_code, started_at}`` of the job's child, readable from any process.

        ``running`` is None when it cannot be told (Windows, or a pid this process may not signal).
        """
        if not is_ulid(job_id):
            return {"started": False}
        log_path, meta_path = self._paths(job_id)
        meta = self._read_json(meta_path)
        if not meta or not meta.get("pid"):
            return {"started": False}
        exit_code = meta.get("exit_code")
        if exit_code is None:
            exit_code = self._trailer_code(log_path)
        pid = int(meta["pid"])
        running: bool | None
        if exit_code is not None:
            running = False
        elif self._windows:
            running = None
        else:
            try:
                os.kill(pid, 0)
                running = True
            except ProcessLookupError:
                running = False
            except PermissionError:
                running = None
        return {
            "started": True,
            "pid": pid,
            "log": str(log_path),
            "running": running,
            "exit_code": exit_code,
            "started_at": meta.get("started_at"),
        }

    @staticmethod
    def _trailer_code(log_path: Path) -> int | None:
        try:
            with open(log_path, "rb") as fh:
                fh.seek(0, os.SEEK_END)
                size = fh.tell()
                fh.seek(max(0, size - 4096))
                tail = fh.read().decode("utf-8", "replace")
        except OSError:
            return None
        found = _TRAILER_RE.findall(tail)
        return int(found[-1]) if found else None


def inject_after_adoption(ctx, session_key: str, prompt: str) -> bool:
    """Optimisation in the gateway process for a session that was already opened (adopted): queue the prompt with
    ``ctx.inject_message``. False whenever that is not possible (fresh import: "not routed", serve: no gateway,
    operator gate closed) — the caller then uses the subprocess (DESIGN §11.5, LAB L5). Never raises."""
    inject = getattr(ctx, "inject_message", None)
    if not callable(inject):
        return False
    try:
        return bool(inject(prompt, "user", session_key=session_key))
    except Exception:
        logger.debug("inject_message for %s failed", session_key, exc_info=True)
        return False
