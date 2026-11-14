"""Whether the status a reader shows is current: does the gateway process that wrote ``status.json`` still run,
and does this profile's ``plugins.isolation`` let the plugin run at all.

The status file outlives the gateway that wrote it. The writing process therefore holds a shared lock on
``status.lock`` next to it for as long as it lives; the kernel drops the lock when the process ends, however
it ends. A reader that can take the lock exclusively knows that no such process is left, and the status file
then shows a past run. No process id is compared, so neither a reused pid nor another PID namespace (the
gateway in a container, the CLI outside it) can mislead the reader. A lock is only seen by processes of the
same kernel: the writer records the kernel's boot id, and a reader under another kernel (another machine, the
Linux VM of Docker Desktop, or after a reboot) reports "unknown", as it does where either side has no boot id
to compare (outside Linux) and without ``flock`` (Windows).
"""

from __future__ import annotations

import logging
import os
import threading
import time
from pathlib import Path
from typing import Any

from . import hermes

logger = logging.getLogger("ctx_discord_policy")

LOCK_NAME = "status.lock"
BOOT_ID = Path("/proc/sys/kernel/random/boot_id")  # Linux: one random id per kernel boot, shared by containers
RUNNING, STOPPED, UNKNOWN = "running", "stopped", "unknown"
_ATTEMPTS = 5            # a reader holds the exclusive lock only for an instant
_PAUSE = 0.02

_held: dict[str, Any] = {}
_held_guard = threading.Lock()


def _flock():
    if os.name != "posix":
        return None
    import fcntl

    return fcntl


def hold(directory: Path) -> bool:
    """Take (once per process and directory) the shared lock the writer keeps until it exits. True when held."""
    fcntl = _flock()
    if fcntl is None:
        return False
    key = os.path.realpath(directory)
    with _held_guard:
        if key in _held:
            return True
        try:
            Path(directory).mkdir(parents=True, exist_ok=True, mode=0o700)
            # Created if missing (0600, like the status file), never truncated.
            fd = os.open(Path(directory) / LOCK_NAME, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        except OSError:
            logger.warning("ctx_discord_policy: cannot create %s", Path(directory) / LOCK_NAME, exc_info=True)
            return False
        handle = os.fdopen(fd, "ab")
        for _attempt in range(_ATTEMPTS):
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_SH | fcntl.LOCK_NB)
            except BlockingIOError:
                time.sleep(_PAUSE)
                continue
            except OSError:
                break
            _held[key] = handle  # kept open (and locked) for the life of this process
            return True
        handle.close()
        logger.warning("ctx_discord_policy: could not lock %s; readers cannot tell whether this gateway runs",
                       Path(directory) / LOCK_NAME)
        return False


def isolation_report() -> dict[str, Any]:
    """``plugins.isolation`` of the active profile as the status readers (CLI, settings page) report it. Anything
    but ``in_process`` means the plugin's own check (A1) refuses: nothing of it runs for Discord."""
    try:
        mode, evidence = hermes.isolation_mode()
    except Exception as exc:  # a configuration Hermes cannot read; the status itself still shows
        mode, evidence = "unknown", f"plugins.isolation could not be read: {exc}"
    return {"mode": mode, "evidence": evidence, "allows_plugin": mode == "in_process"}


def kernel_id() -> Any:
    """The running kernel's boot id, or None where there is none to read (not Linux)."""
    try:
        return BOOT_ID.read_text(encoding="ascii").strip() or None
    except (OSError, ValueError):
        return None


def writer_state(directory: Path, status: dict) -> tuple[str, str]:
    """``(RUNNING | STOPPED | UNKNOWN, reason)`` for the process that wrote *status* in *directory*."""
    if status.get("writer_lock") is not True:
        return UNKNOWN, "it was written without a process lock (an older version, or no file locking there)"
    written, here = status.get("writer_kernel"), kernel_id()
    if not written or not here:
        return UNKNOWN, "there is no kernel boot id to compare (outside Linux), so its lock may be out of sight"
    if written != here:
        return UNKNOWN, ("it was written under another kernel (another machine or VM, or before this machine "
                         "restarted), where its lock cannot be seen")
    fcntl = _flock()
    if fcntl is None:
        return UNKNOWN, "files cannot be locked on this system"
    try:
        handle = open(Path(directory) / LOCK_NAME, "rb")
    except OSError as exc:
        return UNKNOWN, f"its lock file cannot be opened ({exc.strerror or exc})"
    with handle:
        for _attempt in range(_ATTEMPTS):
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                time.sleep(_PAUSE)  # the gateway holds it, or another reader for an instant
                continue
            except OSError as exc:
                return UNKNOWN, f"its lock file cannot be locked ({exc.strerror or exc})"
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
            return STOPPED, "no process holds its lock any more"
    return RUNNING, "its process holds the lock"
