"""Outcome vocabulary of the adaptation layer, the status register and the status file."""

from __future__ import annotations

import enum
import json
import logging
import os
import threading
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Optional

logger = logging.getLogger("ctx_discord_policy")


class Outcome(str, enum.Enum):
    """Result of one intervention (DESIGN §6.1)."""

    OK = "ok"                # precondition holds (A1/A2/A4-style checks)
    ARMED = "armed"          # hooks registered in this profile
    NATIVE = "native"        # the target already does it; nothing injected
    WRAPPED = "wrapped"      # our wrapper is active around the current original
    INJECTED = "injected"    # a missing method was added
    FALLBACK = "fallback"    # upstream behaviour, with a reason
    OFF = "off"              # feature unavailable, with a reason

    @property
    def degraded(self) -> bool:
        return self in (Outcome.FALLBACK, Outcome.OFF)


SECURITY = "S"
COMFORT = "K"


@dataclass
class Result:
    intervention: str
    klass: str
    outcome: Outcome
    reason: str = ""
    probe: str = ""
    at: float = field(default_factory=time.time)

    def as_dict(self) -> dict[str, Any]:
        data = asdict(self)
        data["outcome"] = self.outcome.value
        return data


class StatusRegister:
    """Process-wide record of every intervention outcome (lives in the runtime anchor)."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._results: dict[str, Result] = {}
        self._refused: list[dict[str, Any]] = []

    def record(self, result: Result) -> Result:
        with self._lock:
            previous = self._results.get(result.intervention)
            self._results[result.intervention] = result
        level = logging.WARNING if result.outcome.degraded and result.klass == SECURITY else logging.INFO
        if previous is not None and previous.outcome != result.outcome:
            level = logging.WARNING
            logger.log(level, "ctx_discord_policy: %s: %s -> %s (%s)", result.intervention,
                       previous.outcome.value, result.outcome.value, result.reason)
        else:
            logger.log(level, "ctx_discord_policy: %s %s: %s (%s)", result.klass, result.intervention,
                       result.outcome.value, result.reason)
        return result

    def get(self, intervention: str) -> Optional[Result]:
        with self._lock:
            return self._results.get(intervention)

    def note_refused(self, entry: dict[str, Any]) -> None:
        with self._lock:
            self._refused.append(dict(entry, at=time.time()))
            del self._refused[:-20]

    def security_ok(self, *interventions: str) -> bool:
        """True when every named security intervention has a non-degraded outcome."""
        with self._lock:
            for name in interventions:
                result = self._results.get(name)
                if result is None or result.outcome.degraded:
                    return False
        return True

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            return {
                "interventions": {k: v.as_dict() for k, v in sorted(self._results.items())},
                "refused": list(self._refused),
            }


def write_json_atomic(path: Path, payload: Any) -> None:
    """Write JSON with mode 0600 in a 0700 directory: temp file, fsync, rename."""
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        os.chmod(path.parent, 0o700)
    except OSError:
        pass
    tmp = path.with_name(f".{path.name}.{os.getpid()}.{threading.get_ident()}.tmp")
    data = (json.dumps(payload, indent=2, sort_keys=True, default=str) + "\n").encode("utf-8")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(fd, data)
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(tmp, path)


def compare_with_previous(path: Path, current: dict[str, Any]) -> list[str]:
    """Changes of intervention outcomes against the last status file (log lines, newest run wins)."""
    try:
        previous = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return []
    changes = []
    before = previous.get("interventions") if isinstance(previous, dict) else None
    before = before if isinstance(before, dict) else {}
    now = current.get("interventions") or {}
    for name, result in now.items():
        entry = before.get(name)
        old = entry.get("outcome") if isinstance(entry, dict) else None
        if old and old != result.get("outcome"):
            changes.append(f"{name}: {old} -> {result.get('outcome')} ({result.get('reason', '')})")
    for name in before:
        if name not in now:
            changes.append(f"{name}: no longer reported")
    return changes
