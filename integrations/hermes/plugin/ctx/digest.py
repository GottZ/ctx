"""P5 (ctx-plugin-v2): session digests per segment, stable root session, state pruning.

- ``DigestState``: per root session, profile-local in ``$HERMES_HOME/ctx_state/<root>.digest.json``
  (segment counter, time of the last digest, fingerprints of already digested messages).
  The fingerprints make the threshold "≥ N exchanges SINCE THE LAST DIGEST" robust against
  compaction (the kept head/tail reappear in the next segment) and process restarts.
- ``resolve_lineage_root``: compression-lineage root of a session id, read-only from the
  profile's ``state.db`` — same walk as the host's ``SessionDB.get_compression_lineage``
  (hermes_state_compression.py:790-828, fork markers hermes_state_messages.py:1908).
- ``prune_state_dir``: drop ``ctx_state`` files untouched for N days.

Never raises into the agent: every helper degrades to "no state" / "no root" / 0.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import sqlite3
import tempfile
import threading
import time
from collections.abc import Iterable
from pathlib import Path
from typing import Any

from .config import FACT_STATE_DIR, _safe_root

logger = logging.getLogger(__name__)

DIGEST_FP_MAX = 4000  # fingerprints kept per root (FIFO)
_CARRIER_PREFIXES = ("[CONTEXT COMPACTION", "[PRIOR CONTEXT")
_LINEAGE_MAX_HOPS = 500


# -- Messages ------------------------------------------------------------------------


def is_carrier(message: dict[str, Any]) -> bool:
    """Compaction summary carrier (never digest material, never the intent)."""
    if message.get("_compressed_summary"):
        return True
    content = message.get("content")
    return isinstance(content, str) and content.lstrip().startswith(_CARRIER_PREFIXES)


def text_exchanges(messages: list[dict[str, Any]] | None) -> list[dict[str, Any]]:
    """user/assistant messages with text, without compaction carriers (pre-P5 unit + carrier filter)."""
    out: list[dict[str, Any]] = []
    for m in messages or []:
        if not isinstance(m, dict) or m.get("role") not in ("user", "assistant"):
            continue
        content = m.get("content")
        if not isinstance(content, str) or not content.strip() or is_carrier(m):
            continue
        out.append(m)
    return out


def message_fp(message: dict[str, Any]) -> str:
    raw = f"{message.get('role')}\x00{str(message.get('content') or '').strip()}"
    return hashlib.sha256(raw.encode("utf-8", "replace")).hexdigest()[:20]


# -- Digest state ----------------------------------------------------------------------


class DigestState:
    """Segment counter + digested-message fingerprints of one root session. Atomic writes."""

    def __init__(self, hermes_home: str, root: str, max_fps: int = DIGEST_FP_MAX):
        self.root = str(root or "unknown")
        self.path = Path(hermes_home) / FACT_STATE_DIR / f"{_safe_root(self.root)}.digest.json"
        self.max_fps = max_fps
        self.lock = threading.RLock()
        self.segment = 0
        self.last_digest_ts = 0.0
        self.since_ts = time.time()  # reference for the time-based rolling digest
        self._fps: list[str] = []
        self._fp_set: set = set()
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
            if isinstance(raw, dict):
                self.segment = max(0, int(raw.get("segment") or 0))
                self.last_digest_ts = float(raw.get("last_digest_ts") or 0.0)
                self.since_ts = float(raw.get("since_ts") or self.last_digest_ts or self.since_ts)
                fps = raw.get("fps")
                if isinstance(fps, list):
                    self._fps = [f for f in fps if isinstance(f, str)][-max_fps:]
                    self._fp_set = set(self._fps)
        except FileNotFoundError:
            pass
        except Exception:
            logger.debug("CTX digest state unreadable: %s", self.path, exc_info=True)

    def new_exchanges(self, messages: list[dict[str, Any]] | None) -> list[dict[str, Any]]:
        with self.lock:
            return [m for m in text_exchanges(messages) if message_fp(m) not in self._fp_set]

    def claim(self, messages: Iterable[dict[str, Any]]) -> int:
        """Reserve the next segment number and mark ``messages`` as digested. Returns the segment."""
        with self.lock:
            self.segment += 1
            now = time.time()
            self.last_digest_ts = now
            self.since_ts = now
            for m in messages:
                fp = message_fp(m)
                if fp not in self._fp_set:
                    self._fps.append(fp)
                    self._fp_set.add(fp)
            if len(self._fps) > self.max_fps:
                self._fps = self._fps[-self.max_fps :]
                self._fp_set = set(self._fps)
            self._save()
            return self.segment

    def release(self, messages: Iterable[dict[str, Any]]) -> None:
        """Store failed → the messages count again for the next trigger (segment number stays used)."""
        with self.lock:
            drop = {message_fp(m) for m in messages}
            self._fps = [f for f in self._fps if f not in drop]
            self._fp_set = set(self._fps)
            self._save()

    def _save(self) -> None:
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            payload = {
                "root": self.root,
                "segment": self.segment,
                "last_digest_ts": self.last_digest_ts,
                "since_ts": self.since_ts,
                "fps": self._fps,
                "updated": time.time(),
            }
            fd, tmp = tempfile.mkstemp(prefix=".digest-", dir=str(self.path.parent))
            try:
                with os.fdopen(fd, "w", encoding="utf-8") as fh:
                    json.dump(payload, fh)
                os.chmod(tmp, 0o600)
                os.replace(tmp, self.path)
            except Exception:
                try:
                    os.unlink(tmp)
                except OSError:
                    pass
                raise
        except Exception:
            logger.debug("CTX digest state write failed: %s", self.path, exc_info=True)


# -- Stable root session (host lineage, read-only) --------------------------------------


def _fork_child(row: dict[str, Any], *, include_reset: bool = False) -> bool:
    """Mirror of ``SessionDB._is_explicit_fork_child_row`` (branch/delegate/tool child)."""
    if row.get("source") == "tool":
        return True
    cfg = row.get("model_config")
    if isinstance(cfg, str):
        try:
            cfg = json.loads(cfg)
        except (TypeError, ValueError):
            return False
    if not isinstance(cfg, dict):
        return False
    markers = (cfg.get("_branched_from"), cfg.get("_delegate_from"))
    if include_reset:
        markers += (cfg.get("_reset_from"),)
    parent_id = row.get("parent_session_id")
    return parent_id in markers if parent_id else any(m is not None for m in markers)


def resolve_lineage_root(hermes_home: str, session_id: str) -> str:
    """Compression-lineage root of ``session_id`` from ``<hermes_home>/state.db`` (read-only).

    Walks ``parent_session_id`` upwards while the row is a compression continuation (parent
    ``end_reason == 'compression'``, row not a branch/delegate/tool/reset fork) — the host's
    ``get_compression_lineage(session_id)[0]``. Returns "" when the DB or the row is missing
    (caller falls back to its own rule). Never raises.
    """
    sid = str(session_id or "").strip()
    if not sid or not hermes_home:
        return ""
    path = Path(hermes_home) / "state.db"
    if not path.is_file():
        return ""
    con = None
    try:
        con = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=2.0)
        con.row_factory = sqlite3.Row

        def _get(row_id: str) -> dict[str, Any] | None:
            row = con.execute(
                "SELECT id, source, parent_session_id, end_reason, model_config FROM sessions WHERE id = ?", (row_id,)
            ).fetchone()
            return dict(row) if row else None

        row = _get(sid)
        if not row:
            return ""
        if _fork_child(row):
            return sid
        seen = {sid}
        for _ in range(_LINEAGE_MAX_HOPS):
            parent_id = row.get("parent_session_id")
            if not parent_id or _fork_child(row, include_reset=True):
                break
            parent = _get(parent_id)
            if not parent or parent.get("end_reason") != "compression" or parent["id"] in seen:
                break
            row = parent
            seen.add(row["id"])
        return str(row["id"])
    except Exception:
        logger.debug("CTX lineage root lookup failed (%s)", path, exc_info=True)
        return ""
    finally:
        if con is not None:
            try:
                con.close()
            except Exception:
                pass


# -- Pruning -------------------------------------------------------------------------------


def prune_state_dir(hermes_home: str, max_age_days: float, *, now: float | None = None) -> int:
    """Delete ``ctx_state`` files (ledgers, digest states, stale temp files) not modified for
    ``max_age_days``. 0/negative = off. Returns the number of removed files. Never raises."""
    if not hermes_home or not max_age_days or max_age_days <= 0:
        return 0
    base = Path(hermes_home) / FACT_STATE_DIR
    if not base.is_dir():
        return 0
    cutoff = (now if now is not None else time.time()) - float(max_age_days) * 86400.0
    removed = 0
    try:
        entries = list(base.iterdir())
    except Exception:
        return 0
    for entry in entries:
        name = entry.name
        if not (name.endswith(".json") or name.startswith((".ledger-", ".digest-"))):
            continue
        try:
            if entry.is_file() and entry.stat().st_mtime < cutoff:
                entry.unlink()
                removed += 1
        except Exception:
            logger.debug("CTX state prune failed for %s", entry, exc_info=True)
    return removed
