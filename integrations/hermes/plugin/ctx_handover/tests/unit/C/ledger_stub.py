# ruff: noqa: S608 - SQL is assembled only from constant column lists and "?" placeholders, never from input
"""SQLite stand-in for the part of ``engine/ledger.py`` (cluster A) that cluster C calls (layer-1 tests only).

The real ledger belongs to cluster A (INTERFACES §4.5, §6: "ledger.py (all SQL) A"). Until it lands, the C suites
run against this stub, which implements exactly the methods C calls with the DDL of INTERFACES §7 for the tables
involved and the transaction rules of DESIGN §12.3 (``BEGIN IMMEDIATE``, ``DELETE … failures < 5 AND
expires_at > ?`` with ``rowcount == 1``). ``make_ledger`` prefers the real ``ledger.Ledger`` as soon as it is
importable, so layer 2 re-runs every C test against A's implementation without edits.

Methods beyond INTERFACES §4.5, requested from cluster A in the C reports: ``job_objects``, ``blocks_delete``,
``object_delete`` (manifest GC, R28) and the staging quota ``quota_set``/``quota_get``/``quota_charge`` (R28;
same semantics as ``engine/ledger.py``).

Every method opens its own connection (process- and thread-safe, like several Hermes processes sharing one
profile directory). ``tx()`` is a dedicated connection holding ``BEGIN IMMEDIATE`` and, like the real ledger, is
re-entrant within a thread: a write method called inside it joins the open transaction. Reads called inside it
see the committed state through another connection, which equals the current state as long as the transaction
has not written yet (it holds the database's only write lock).
"""

from __future__ import annotations

import contextlib
import json
import os
import sqlite3
import threading
import time
from collections.abc import Iterator, Sequence
from pathlib import Path

_DDL = """
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS peers(peer_id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  direction TEXT NOT NULL CHECK(direction IN ('dial','inbound')), url TEXT, pubkey TEXT NOT NULL, rights TEXT NOT NULL,
  gate TEXT, profile TEXT, allow_insecure INTEGER NOT NULL DEFAULT 0, created_at REAL NOT NULL, last_seen_at REAL,
  last_ip TEXT, revoked_at REAL);
CREATE TABLE IF NOT EXISTS pair_codes(code_id TEXT PRIMARY KEY, secret TEXT NOT NULL, rights TEXT NOT NULL,
  name_hint TEXT, expires_at REAL NOT NULL, failures INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS nonces(peer_id TEXT NOT NULL, nonce TEXT NOT NULL, ts INTEGER NOT NULL,
  PRIMARY KEY(peer_id, nonce));
CREATE TABLE IF NOT EXISTS jobs(job_id TEXT NOT NULL, role TEXT NOT NULL, mode TEXT NOT NULL, peer_id TEXT,
  state TEXT NOT NULL, params TEXT NOT NULL, created_at REAL NOT NULL, cancel_requested INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(job_id, role));
CREATE TABLE IF NOT EXISTS blocks(block_id TEXT PRIMARY KEY, size INTEGER NOT NULL, last_used_at REAL NOT NULL);
CREATE TABLE IF NOT EXISTS objects(object_id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, size INTEGER NOT NULL,
  media TEXT, at REAL NOT NULL);
CREATE TABLE IF NOT EXISTS job_objects(job_id TEXT NOT NULL, object_id TEXT NOT NULL, PRIMARY KEY(job_id, object_id));
CREATE TABLE IF NOT EXISTS offers(offer_id TEXT PRIMARY KEY, peer_id TEXT NOT NULL, session_id TEXT NOT NULL,
  note TEXT, refinement TEXT, created_at REAL NOT NULL, expires_at REAL NOT NULL, claimed_job TEXT);
CREATE TABLE IF NOT EXISTS bundle_imports(bundle_job_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, at REAL NOT NULL);
CREATE TABLE IF NOT EXISTS job_quota(job_id TEXT NOT NULL, role TEXT NOT NULL, quota INTEGER NOT NULL,
  used INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(job_id, role));
"""

_TERMINAL = ("aborted", "expired", "committed", "retired", "released", "reclaimed", "done", "exported")


class LedgerStub:
    """See module docstring. ``model`` lets a path-loaded engine copy pass its own ``model`` module."""

    def __init__(self, path: Path, *, model=None, clock=time.time) -> None:
        if model is None:
            from ctx_handover.engine import model as model_mod

            model = model_mod
        self.path = Path(path)
        self._m = model
        self._clock = clock
        self._local = threading.local()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        if not self.path.exists():
            fd = os.open(self.path, os.O_WRONLY | os.O_CREAT, 0o600)
            os.close(fd)
        with self._conn() as conn:
            conn.executescript(_DDL)
            conn.execute("PRAGMA secure_delete=ON")

    # -- plumbing ----------------------------------------------------------------------------------------
    @contextlib.contextmanager
    def _conn(self) -> Iterator[sqlite3.Connection]:
        conn = sqlite3.connect(self.path, timeout=30, isolation_level=None)
        try:
            conn.execute("PRAGMA busy_timeout=30000")
            conn.execute("PRAGMA secure_delete=ON")
            yield conn
        finally:
            conn.close()

    @contextlib.contextmanager
    def tx(self) -> Iterator[sqlite3.Connection]:
        """``BEGIN IMMEDIATE`` transaction on a dedicated connection; commit on success, roll back on error.
        Re-entrant within a thread (the outermost commits), like ``engine.ledger.Ledger.tx``."""
        outer = getattr(self._local, "conn", None)
        if outer is not None:
            yield outer
            return
        with self._conn() as conn:
            conn.execute("BEGIN IMMEDIATE")
            self._local.conn = conn
            try:
                yield conn
            except BaseException:
                self._local.conn = None
                conn.execute("ROLLBACK")
                raise
            self._local.conn = None
            conn.execute("COMMIT")

    # -- meta ----------------------------------------------------------------------------------------------
    def meta_get(self, key: str) -> str | None:
        with self._conn() as conn:
            row = conn.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return row[0] if row else None

    def meta_set(self, key: str, value: str, conn: sqlite3.Connection | None = None) -> None:
        sql = "INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value"
        if conn is not None:
            conn.execute(sql, (key, value))
            return
        with self.tx() as own:
            own.execute(sql, (key, value))

    def instance_id(self) -> str:
        from ctx_handover.engine.ids import ulid

        with self.tx() as conn:
            row = conn.execute("SELECT value FROM meta WHERE key='instance_id'").fetchone()
            if row:
                return row[0]
            value = ulid()
            conn.execute("INSERT INTO meta(key, value) VALUES('instance_id', ?)", (value,))
            return value

    # -- peers -----------------------------------------------------------------------------------------------
    def _peer(self, row) -> object:
        m = self._m
        (peer_id, name, direction, url, pubkey, rights, gate, profile, insecure, created, seen, ip, revoked) = row
        gate_obj = None
        if gate:
            data = json.loads(gate)
            gate_obj = m.GateAuth(
                mode=m.GateMode(data["mode"]),
                token_env=data.get("token_env", "CTX_HANDOVER_PEER_TOKEN"),
                username=data.get("username", ""),
                password_env=data.get("password_env", "CTX_HANDOVER_PEER_PASSWORD"),
                extra_headers=tuple(tuple(p) for p in data.get("extra_headers", [])),
            )
        return m.Peer(
            name=name,
            peer_id=peer_id,
            direction=direction,
            url=url,
            pubkey=pubkey,
            rights=frozenset(m.Right(r) for r in rights.split(",") if r),
            gate=gate_obj,
            profile=profile,
            allow_insecure=bool(insecure),
            created_at=created,
            last_seen_at=seen or 0.0,
            last_ip=ip,
            revoked_at=revoked,
        )

    _PEER_COLS = (
        "peer_id, name, direction, url, pubkey, rights, gate, profile, allow_insecure, created_at, last_seen_at, "
        "last_ip, revoked_at"
    )

    def peers(self, *, include_revoked: bool = False) -> list:
        sql = f"SELECT {self._PEER_COLS} FROM peers"
        if not include_revoked:
            sql += " WHERE revoked_at IS NULL"
        with self._conn() as conn:
            return [self._peer(r) for r in conn.execute(sql + " ORDER BY name")]

    def peer(self, name_or_id: str):
        with self._conn() as conn:
            row = conn.execute(
                f"SELECT {self._PEER_COLS} FROM peers WHERE peer_id=? OR name=?", (name_or_id, name_or_id)
            ).fetchone()
        return self._peer(row) if row else None

    def inbound_peer(self, peer_id: str):
        with self._conn() as conn:
            row = conn.execute(
                f"SELECT {self._PEER_COLS} FROM peers WHERE peer_id=? AND direction='inbound' AND revoked_at IS NULL",
                (peer_id,),
            ).fetchone()
        return self._peer(row) if row else None

    def _peer_params(self, peer) -> tuple:
        gate = None
        if peer.gate is not None:
            gate = json.dumps(
                {
                    "mode": str(peer.gate.mode),
                    "token_env": peer.gate.token_env,
                    "username": peer.gate.username,
                    "password_env": peer.gate.password_env,
                    "extra_headers": [list(p) for p in peer.gate.extra_headers],
                },
                sort_keys=True,
            )
        return (
            peer.peer_id,
            peer.name,
            peer.direction,
            peer.url,
            peer.pubkey,
            ",".join(sorted(str(r) for r in peer.rights)),
            gate,
            peer.profile,
            int(bool(peer.allow_insecure)),
            peer.created_at or self._clock(),
            peer.last_seen_at or None,
            peer.last_ip,
            peer.revoked_at,
        )

    _UPSERT = (
        "INSERT INTO peers(peer_id, name, direction, url, pubkey, rights, gate, profile, allow_insecure, created_at, "
        "last_seen_at, last_ip, revoked_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(peer_id) DO UPDATE SET "
        "name=excluded.name, direction=excluded.direction, url=excluded.url, pubkey=excluded.pubkey, "
        "rights=excluded.rights, gate=excluded.gate, profile=excluded.profile, allow_insecure=excluded.allow_insecure, "
        "revoked_at=excluded.revoked_at"
    )

    def upsert_peer(self, peer) -> None:
        with self.tx() as conn:
            conn.execute("DELETE FROM peers WHERE name=? AND peer_id<>?", (peer.name, peer.peer_id))
            conn.execute(self._UPSERT, self._peer_params(peer))

    def revoke_peer(self, name_or_id: str) -> list[str]:
        with self.tx() as conn:
            row = conn.execute(
                "SELECT peer_id FROM peers WHERE (peer_id=? OR name=?) AND revoked_at IS NULL", (name_or_id, name_or_id)
            ).fetchone()
            if not row:
                return []
            peer_id = row[0]
            conn.execute("UPDATE peers SET revoked_at=? WHERE peer_id=?", (self._clock(), peer_id))
            conn.execute("DELETE FROM offers WHERE peer_id=?", (peer_id,))
            marks = ",".join("?" * len(_TERMINAL))
            ids = [
                r[0]
                for r in conn.execute(
                    "SELECT job_id FROM jobs WHERE peer_id=? AND role IN ('source','target') "
                    f"AND state NOT IN ({marks})",
                    (peer_id, *_TERMINAL),
                )
            ]
            conn.execute(
                f"UPDATE jobs SET state='aborting' WHERE peer_id=? AND role IN ('source','target') "
                f"AND state NOT IN ({marks})",
                (peer_id, *_TERMINAL),
            )
            return ids

    def remove_peer(self, name_or_id: str) -> bool:
        with self.tx() as conn:
            cur = conn.execute(
                "DELETE FROM peers WHERE (peer_id=? OR name=?) AND direction='dial'", (name_or_id, name_or_id)
            )
            return cur.rowcount == 1

    def touch_peer(self, peer_id: str, ip: str | None) -> None:
        with self.tx() as conn:
            conn.execute("UPDATE peers SET last_seen_at=?, last_ip=? WHERE peer_id=?", (self._clock(), ip, peer_id))

    # -- pairing codes -------------------------------------------------------------------------------------
    def put_pair_code(self, code_id: str, secret: str, rights, name_hint: str, expires_at: float) -> None:
        with self.tx() as conn:
            conn.execute(
                "INSERT INTO pair_codes(code_id, secret, rights, name_hint, expires_at) VALUES(?,?,?,?,?)",
                (code_id, secret, ",".join(sorted(str(r) for r in rights)), name_hint, expires_at),
            )

    def pair_code(self, code_id: str, now: float):
        with self._conn() as conn:
            row = conn.execute(
                "SELECT secret, rights, name_hint FROM pair_codes WHERE code_id=? AND failures<5 AND expires_at>?",
                (code_id, now),
            ).fetchone()
        if not row:
            return None
        rights = frozenset(self._m.Right(r) for r in row[1].split(",") if r)
        return row[0], rights, row[2] or ""

    def redeem_pair_code(self, code_id: str, now: float, peer) -> bool:
        with self.tx() as conn:
            cur = conn.execute("DELETE FROM pair_codes WHERE code_id=? AND failures<5 AND expires_at>?", (code_id, now))
            if cur.rowcount != 1:
                return False
            conn.execute(self._UPSERT, self._peer_params(peer))
        with self._conn() as conn:
            conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        return True

    def fail_pair_code(self, code_id: str) -> int:
        with self.tx() as conn:
            conn.execute("UPDATE pair_codes SET failures=failures+1 WHERE code_id=? AND failures<5", (code_id,))
            row = conn.execute("SELECT failures FROM pair_codes WHERE code_id=?", (code_id,)).fetchone()
            return int(row[0]) if row else 0

    def purge_expired_codes(self, now: float) -> int:
        with self.tx() as conn:
            n = conn.execute("DELETE FROM pair_codes WHERE expires_at<=? OR failures>=5", (now,)).rowcount
        with self._conn() as conn:
            conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        return n

    def pairing_pending(self, now: float) -> bool:
        with self._conn() as conn:
            row = conn.execute("SELECT 1 FROM pair_codes WHERE failures<5 AND expires_at>? LIMIT 1", (now,)).fetchone()
        return row is not None

    def nonce_seen(self, peer_id: str, nonce: str, ts: int, ttl_s: int) -> bool:
        with self.tx() as conn:
            conn.execute("DELETE FROM nonces WHERE ts<?", (int(self._clock()) - ttl_s,))
            cur = conn.execute("INSERT OR IGNORE INTO nonces(peer_id, nonce, ts) VALUES(?,?,?)", (peer_id, nonce, ts))
            return cur.rowcount == 0

    # -- jobs (just enough for the server's caps and revoke) ------------------------------------------------
    def create_job(self, job_id: str, role, mode, peer_id: str | None, params: dict) -> None:
        with self.tx() as conn:
            conn.execute(
                "INSERT INTO jobs(job_id, role, mode, peer_id, state, params, created_at) VALUES(?,?,?,?,?,?,?)",
                (job_id, str(role), str(mode), peer_id, "open", json.dumps(params), self._clock()),
            )

    def set_state(self, job_id: str, role, state: str) -> None:
        with self.tx() as conn:
            conn.execute("UPDATE jobs SET state=? WHERE job_id=? AND role=?", (state, job_id, str(role)))

    def job(self, job_id: str, role) -> dict | None:
        with self._conn() as conn:
            row = conn.execute(
                "SELECT job_id, role, mode, peer_id, state, params, created_at, cancel_requested FROM jobs "
                "WHERE job_id=? AND role=?",
                (job_id, str(role)),
            ).fetchone()
        if not row:
            return None
        keys = ("job_id", "role", "mode", "peer_id", "state", "params", "created_at", "cancel_requested")
        out = dict(zip(keys, row, strict=True))
        out["params"] = json.loads(out["params"])
        return out

    def jobs(self, *, role=None, active: bool | None = None, limit: int = 20) -> list[dict]:
        sql = "SELECT job_id, role, mode, peer_id, state, created_at FROM jobs"
        cond, args = [], []
        if role is not None:
            cond.append("role=?")
            args.append(str(role))
        if active is not None:
            marks = ",".join("?" * len(_TERMINAL))
            cond.append(f"state {'NOT ' if active else ''}IN ({marks})")
            args.extend(_TERMINAL)
        if cond:
            sql += " WHERE " + " AND ".join(cond)
        with self._conn() as conn:
            rows = conn.execute(sql + " ORDER BY created_at DESC LIMIT ?", (*args, limit)).fetchall()
        return [dict(zip(("job_id", "role", "mode", "peer_id", "state", "created_at"), r, strict=True)) for r in rows]

    def set_cancel(self, job_id: str) -> None:
        with self.tx() as conn:
            conn.execute("UPDATE jobs SET cancel_requested=1 WHERE job_id=?", (job_id,))

    def cancel_requested(self, job_id: str) -> bool:
        with self._conn() as conn:
            row = conn.execute("SELECT MAX(cancel_requested) FROM jobs WHERE job_id=?", (job_id,)).fetchone()
        return bool(row and row[0])

    # -- objects / blocks ----------------------------------------------------------------------------------
    def object_put(self, m) -> None:
        with self.tx() as conn:
            conn.execute(
                "INSERT OR IGNORE INTO objects(object_id, sha256, size, media, at) VALUES(?,?,?,?,?)",
                (m.object_id, m.sha256, m.size, m.media, self._clock()),
            )

    def object_get(self, object_id: str):
        with self._conn() as conn:
            row = conn.execute("SELECT sha256, size, media FROM objects WHERE object_id=?", (object_id,)).fetchone()
        if not row:
            return None
        return self._m.ObjectManifest(
            sha256=row[0], size=row[1], block_size=524_288, blocks=(), media=row[2] or "", object_id=object_id
        )

    def block_put(self, block_id: str, size: int) -> None:
        with self.tx() as conn:
            conn.execute(
                "INSERT INTO blocks(block_id, size, last_used_at) VALUES(?,?,?) ON CONFLICT(block_id) DO UPDATE "
                "SET last_used_at=excluded.last_used_at",
                (block_id, size, self._clock()),
            )

    def block_touch(self, ids: Sequence[str]) -> None:
        if not ids:
            return
        now = self._clock()
        with self.tx() as conn:
            conn.executemany("UPDATE blocks SET last_used_at=? WHERE block_id=?", [(now, i) for i in ids])

    def job_object(self, job_id: str, object_id: str) -> None:
        with self.tx() as conn:
            conn.execute("INSERT OR IGNORE INTO job_objects(job_id, object_id) VALUES(?,?)", (job_id, object_id))

    def job_objects_add(self, job_id: str, object_ids: Sequence[str]) -> None:
        """Mirrors ``engine/ledger.py``: many references in one transaction."""
        with self.tx() as conn:
            conn.executemany(
                "INSERT OR IGNORE INTO job_objects(job_id, object_id) VALUES(?,?)", [(job_id, o) for o in object_ids]
            )

    def job_objects(self, job_id: str) -> set[str]:
        """Requested from cluster A (C report): object ids registered for ``job_id``."""
        with self._conn() as conn:
            return {r[0] for r in conn.execute("SELECT object_id FROM job_objects WHERE job_id=?", (job_id,))}

    def live_objects(self) -> set[str]:
        marks = ",".join("?" * len(_TERMINAL))
        with self._conn() as conn:
            rows = conn.execute(
                f"SELECT DISTINCT jo.object_id FROM job_objects jo JOIN jobs j ON j.job_id = jo.job_id "
                f"WHERE j.state NOT IN ({marks})",
                _TERMINAL,
            ).fetchall()
        return {r[0] for r in rows}

    def gc_candidates(self, older_than: float, max_bytes: int) -> list[str]:
        """Blocks last used before ``older_than``, then the least recently used beyond ``max_bytes`` (LRU)."""
        with self._conn() as conn:
            rows = conn.execute("SELECT block_id, size, last_used_at FROM blocks ORDER BY last_used_at ASC").fetchall()
        total = sum(r[1] for r in rows)
        out = []
        for block_id, size, used in rows:
            if used < older_than or total > max_bytes:
                out.append(block_id)
                total -= size
        return out

    def blocks_delete(self, ids: Sequence[str]) -> None:
        """Requested from cluster A (C report): forget freed blocks."""
        with self.tx() as conn:
            conn.executemany("DELETE FROM blocks WHERE block_id=?", [(i,) for i in ids])

    def object_delete(self, ids: Sequence[str]) -> None:
        """Requested from cluster A (C layer-2 report): forget the index rows of collected objects (R28)."""
        with self.tx() as conn:
            conn.executemany("DELETE FROM objects WHERE object_id=?", [(i,) for i in ids])

    # -- staging quota (R28; mirrors engine/ledger.py) --------------------------------------------------------
    def quota_set(self, job_id: str, role, quota_bytes: int) -> None:
        with self.tx() as conn:
            conn.execute(
                "INSERT INTO job_quota(job_id, role, quota, used) VALUES(?,?,?,0) ON CONFLICT(job_id, role)"
                " DO UPDATE SET quota=MAX(job_quota.used, excluded.quota)",
                (job_id, str(role), max(0, int(quota_bytes))),
            )

    def quota_get(self, job_id: str, role) -> tuple[int, int] | None:
        with self._conn() as conn:
            row = conn.execute(
                "SELECT quota, used FROM job_quota WHERE job_id=? AND role=?", (job_id, str(role))
            ).fetchone()
        return None if row is None else (int(row[0]), int(row[1]))

    def quota_charge(self, job_id: str, role, nbytes: int) -> tuple[bool, int, int]:
        n = max(0, int(nbytes))
        with self.tx() as conn:
            row = conn.execute(
                "SELECT quota, used FROM job_quota WHERE job_id=? AND role=?", (job_id, str(role))
            ).fetchone()
            if row is None:
                return False, 0, -1
            quota, used = int(row[0]), int(row[1])
            if used + n > quota:
                return False, used, quota
            conn.execute("UPDATE job_quota SET used=used+? WHERE job_id=? AND role=?", (n, job_id, str(role)))
            return True, used + n, quota

    def block_rows(self) -> dict[str, tuple[int, float]]:
        """Test helper: ``{block_id: (size, last_used_at)}``."""
        with self._conn() as conn:
            return {r[0]: (r[1], r[2]) for r in conn.execute("SELECT block_id, size, last_used_at FROM blocks")}

    # -- bundles -------------------------------------------------------------------------------------------
    def bundle_import_seen(self, bundle_job_id: str) -> str | None:
        with self._conn() as conn:
            row = conn.execute(
                "SELECT session_id FROM bundle_imports WHERE bundle_job_id=?", (bundle_job_id,)
            ).fetchone()
        return row[0] if row else None

    def bundle_import_put(self, bundle_job_id: str, session_id: str) -> None:
        with self.tx() as conn:
            conn.execute(
                "INSERT INTO bundle_imports(bundle_job_id, session_id, at) VALUES(?,?,?) "
                "ON CONFLICT(bundle_job_id) DO UPDATE SET session_id=excluded.session_id, at=excluded.at",
                (bundle_job_id, session_id, self._clock()),
            )


def make_ledger(path: Path, *, model=None, clock=time.time):
    """The real ``ledger.Ledger`` when cluster A's module is importable, else ``LedgerStub``.

    ``CTXH_C_LEDGER=stub`` forces the stub (layer 1 red proofs), ``real`` requires A's ledger.
    """
    choice = os.environ.get("CTXH_C_LEDGER", "auto")
    if choice != "stub":
        try:
            from ctx_handover.engine.ledger import Ledger
        except ImportError:
            if choice == "real":
                raise
        else:
            return Ledger(Path(path))
    return LedgerStub(Path(path), model=model, clock=clock)
