"""The policy store: one JSON document per bot-owner home, written atomically under a file lock.

Path: ``<owner-home>/plugin-data/ctx_discord_policy/policy.json`` (0600, directory 0700).

Readers (hooks, admission) only ever see cached snapshots; writers (CLI now, the Discord UI later) go
through :meth:`PolicyStore.update`, which locks, re-reads, applies one mutation, bumps the guild ``rev``
and appends audit entries before an fsync'd ``os.replace``.

Failure behaviour (DESIGN §5.3):
- unreadable or structurally broken file: never overwritten; a copy ``policy.json.corrupt-<time>`` is
  kept; the snapshot is ``corrupt`` and every guild counts as paused (closed) until an operator fixes it;
- schema newer than this plugin: ``downgrade`` — read what v1 understands, refuse all writes;
- older schema: migrated in steps (``store.migrate``) after a ``policy.json.bak-<schema>-<time>`` copy.
"""

from __future__ import annotations

import contextlib
import copy
import hashlib
import json
import logging
import os
import shutil
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Iterator, Optional

from . import migrate
from .schema import AUDIT_RING, SCHEMA_VERSION, SchemaError, empty_document, validate_document

logger = logging.getLogger("ctx_discord_policy")

OK = "ok"
MISSING = "missing"
CORRUPT = "corrupt"
DOWNGRADE = "downgrade"


class StoreError(RuntimeError):
    """A write that the store refuses (corrupt/downgraded file, lock failure)."""


class ConflictError(StoreError):
    """The guild changed since the caller read it (optimistic ``rev`` check)."""


def utc_now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def value_digest(value: Any) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False).encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class Snapshot:
    state: str
    data: dict[str, Any]
    problems: tuple[str, ...] = ()
    error: str = ""
    signature: Any = None

    @property
    def writable(self) -> bool:
        return self.state in (OK, MISSING)

    def guild(self, bot_id: str, guild_id: str) -> Optional[dict[str, Any]]:
        if self.state == CORRUPT:
            return None
        return ((self.data.get("bots") or {}).get(str(bot_id)) or {}).get("guilds", {}).get(str(guild_id))

    def guild_state(self, bot_id: str, guild_id: str) -> str:
        """Effective state: ``paused`` for every guild while the file is corrupt (fail closed)."""
        if self.state == CORRUPT:
            return "paused"
        guild = self.guild(bot_id, guild_id)
        return guild["state"] if guild else "unbound"


@contextlib.contextmanager
def _file_lock(path: Path, timeout: float = 10.0) -> Iterator[None]:
    """Cross-process exclusive lock on ``path`` (fcntl on POSIX, msvcrt on Windows)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    deadline = time.monotonic() + timeout
    try:
        if os.name == "nt":  # pragma: no cover - exercised on Windows only
            import msvcrt

            while True:
                try:
                    msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
                    break
                except OSError:
                    if time.monotonic() > deadline:
                        raise StoreError(f"could not lock {path}")
                    time.sleep(0.05)
            try:
                yield
            finally:
                os.lseek(fd, 0, os.SEEK_SET)
                msvcrt.locking(fd, msvcrt.LK_UNLCK, 1)
        else:
            import fcntl

            while True:
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() > deadline:
                        raise StoreError(f"could not lock {path}")
                    time.sleep(0.01)
            try:
                yield
            finally:
                fcntl.flock(fd, fcntl.LOCK_UN)
    finally:
        os.close(fd)


def _write_atomic(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with contextlib.suppress(OSError):
        os.chmod(path.parent, 0o700)
    tmp = path.with_name(f".{path.name}.{os.getpid()}.{threading.get_ident()}.tmp")
    data = (json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False) + "\n").encode("utf-8")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(fd, data)
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(tmp, path)
    if os.name != "nt":
        dir_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)


class PolicyStore:
    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self.lock_path = self.path.with_name(self.path.name + ".lock")
        self._cache: Optional[Snapshot] = None
        self._cache_lock = threading.Lock()

    # ── reading ───────────────────────────────────────────────────────────────────────────────────

    def _signature(self) -> Any:
        try:
            st = self.path.stat()
        except FileNotFoundError:
            return None
        return (st.st_mtime_ns, st.st_size, st.st_ino)

    def read(self) -> Snapshot:
        """Cached snapshot; re-parsed only when the file's (mtime, size, inode) changed."""
        signature = self._signature()
        with self._cache_lock:
            if self._cache is not None and self._cache.signature == signature:
                return self._cache
        snapshot = self._load(signature, locked=False)
        with self._cache_lock:
            self._cache = snapshot
        return snapshot

    def _load(self, signature: Any, *, locked: bool) -> Snapshot:
        if signature is None:
            return Snapshot(MISSING, empty_document(), signature=None)
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, ValueError) as exc:
            return self._corrupt(signature, f"unreadable: {type(exc).__name__}: {exc}")
        if not isinstance(raw, dict) or not isinstance(raw.get("schema"), int) or isinstance(raw.get("schema"), bool):
            return self._corrupt(signature, "not a policy document (missing integer 'schema')")
        version = raw["schema"]
        if version > SCHEMA_VERSION:
            try:
                data, problems = validate_document(dict(raw, schema=SCHEMA_VERSION))
            except SchemaError as exc:
                return self._corrupt(signature, f"newer schema {version} and unreadable as v1: {exc}")
            return Snapshot(DOWNGRADE, data, tuple(problems), f"schema {version} is newer than {SCHEMA_VERSION}; "
                            "read-only until the newer plugin is restored", signature)
        if version < SCHEMA_VERSION:
            try:
                raw = migrate.upgrade(raw, version, backup=self._backup)
            except Exception as exc:
                return self._corrupt(signature, f"migration from schema {version} failed: {exc}")
        try:
            data, problems = validate_document(raw)
        except SchemaError as exc:
            return self._corrupt(signature, str(exc))
        if version < SCHEMA_VERSION:
            # Persist the migrated document (under the lock; a concurrent writer re-reads after us).
            with (contextlib.nullcontext() if locked else _file_lock(self.lock_path)):
                if self._signature() == signature:
                    _write_atomic(self.path, data)
            signature = self._signature()
        return Snapshot(OK, data, tuple(problems), signature=signature)

    def _backup(self, version: int) -> Path:
        target = self.path.with_name(f"{self.path.name}.bak-{version}-{int(time.time())}")
        shutil.copy2(self.path, target)
        return target

    def _corrupt(self, signature: Any, reason: str) -> Snapshot:
        copy_path = self.path.with_name(f"{self.path.name}.corrupt-{int(time.time())}")
        try:
            if not copy_path.exists():
                shutil.copy2(self.path, copy_path)
        except OSError:
            logger.warning("ctx_discord_policy: could not keep a copy of %s", self.path, exc_info=True)
        logger.error("ctx_discord_policy: %s is corrupt (%s); every guild stays paused until it is "
                     "repaired; copy kept at %s", self.path, reason, copy_path)
        return Snapshot(CORRUPT, empty_document(), error=reason, signature=signature)

    # ── writing ───────────────────────────────────────────────────────────────────────────────────

    def update(self, mutate: Callable[[dict[str, Any]], list[dict[str, Any]]], *, by: str) -> Snapshot:
        """Apply ``mutate(document)`` under the lock.

        ``mutate`` edits the document in place and returns change records
        ``{"bot", "guild", "field", "old", "new", "expected_rev"?}``; each touched guild gets ``rev`` + 1,
        ``updated_by``/``updated_at`` and one audit entry per field (hashes only, never values).
        """
        with _file_lock(self.lock_path):
            current = self._load(self._signature(), locked=True)
            if not current.writable:
                raise StoreError(f"policy store is {current.state}: {current.error}")
            document = copy.deepcopy(current.data)
            changes = mutate(document) or []
            now = utc_now()
            touched: dict[tuple[str, str], int] = {}
            for change in changes:
                key = (str(change["bot"]), str(change["guild"]))
                expected = change.get("expected_rev")
                base_rev = (current.guild(*key) or {}).get("rev", 0)
                if expected is not None and expected != base_rev:
                    raise ConflictError(f"guild {key[1]} changed (rev {base_rev}, you had {expected}); reload")
                touched.setdefault(key, base_rev)
                document["audit"].append({
                    "at": now, "bot": key[0], "guild": key[1], "by": by, "field": change["field"],
                    "old_sha256": value_digest(change.get("old")), "new_sha256": value_digest(change.get("new")),
                })
            for (bot_id, guild_id), base_rev in touched.items():
                guild = document["bots"].get(bot_id, {}).get("guilds", {}).get(guild_id)
                if guild is None:
                    continue  # purged
                guild["rev"] = base_rev + 1
                guild["updated_by"] = by
                guild["updated_at"] = now
            document["audit"] = document["audit"][-AUDIT_RING:]
            data, problems = validate_document(document)
            if problems:
                raise SchemaError("; ".join(problems))
            _write_atomic(self.path, data)
        return self.read()
