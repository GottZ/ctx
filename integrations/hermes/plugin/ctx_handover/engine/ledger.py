"""The plugin's durable state: one SQLite ledger per profile (layer L2, stdlib only; INTERFACES §4.5, §7).

Gateway, ``hermes serve``, CLI and plugin-host processes share nothing but this file (DESIGN I10). Every
state change is one ``BEGIN IMMEDIATE`` transaction; job transitions compare the expected state **and** the
owner epoch (compare-and-set), so a runner that lost its job to another process can no longer write
(DESIGN §3.1, [SK M5]). All SQL of the plugin lives here (INTERFACES §6).

Files: the data directory is created ``0700`` before the first file, the database ``0600`` (SQLite creates
``-wal``/``-shm`` with the database's mode); ``secure_delete`` is on for every connection, so deleted pairing
codes do not linger in free pages (SEC 6.1, 6.2).

A ledger written by a newer plugin (higher ``schema_version``) opens read-only: reads work, every write raises
``InvalidState(reason="newer_schema")`` and ``doctor`` reports a downgrade (INTERFACES §7).

Connections are per thread (``threading.local``); ``tx()`` is re-entrant within a thread.
"""

from __future__ import annotations

import contextlib
import json
import os
import secrets
import sqlite3
import threading
import time
from collections.abc import Callable, Iterator, Sequence
from pathlib import Path

from .errors import AlreadyHandingOver, ConflictError, InvalidState, JobConflict, NotFound
from .ids import ulid
from .jsonutil import canonical_bytes, from_wire, sha256_hex, to_wire
from .model import (
    GateAuth,
    MapRow,
    Mode,
    ObjectManifest,
    OpState,
    PathMapEntry,
    Peer,
    Right,
    Role,
    SessionInfo,
    SourceState,
)

SCHEMA_VERSION = 2

STALE_OWNER_S = 60.0
"""An owner without heartbeat for this long is stale and may be adopted (DESIGN §3.1)."""

HEARTBEAT_S = 10.0
"""Owners heartbeat at least this often (DESIGN §3.1)."""

JOURNAL_KEEP_S = 30 * 86400
OPS_KEEP_S = 7 * 86400
SUGGESTION_TTL_S = 86400

CONDUCTOR_TERMINAL = frozenset({"done", "aborted"})
SOURCE_JOB_TERMINAL = frozenset({"released", "reclaimed", "retired", "exported"})
TARGET_TERMINAL = frozenset({"committed", "aborted", "expired"})
TERMINAL = {Role.CONDUCTOR: CONDUCTOR_TERMINAL, Role.SOURCE: SOURCE_JOB_TERMINAL, Role.TARGET: TARGET_TERMINAL}

ACTIVE_SOURCE_STATES = ("locking", "locked", "preparing", "prepared", "retiring", "retired", "exported")
"""Source states that hold the lineage (partial unique index ``one_active_handover``)."""

_REVOKE_SOURCE_STATES = frozenset({"locking", "locked", "preparing", "suspended"})
_REVOKE_TARGET_KEEP = frozenset({"committing", "committed", "aborted", "expired", "aborting"})

_SCHEMA = (
    "CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)",
    "CREATE TABLE IF NOT EXISTS peers(peer_id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,"
    " direction TEXT NOT NULL CHECK(direction IN ('dial','inbound')), url TEXT, pubkey TEXT NOT NULL,"
    " rights TEXT NOT NULL, gate TEXT, profile TEXT, allow_insecure INTEGER NOT NULL DEFAULT 0,"
    " created_at REAL NOT NULL, last_seen_at REAL, last_ip TEXT, revoked_at REAL)",
    "CREATE TABLE IF NOT EXISTS pair_codes(code_id TEXT PRIMARY KEY, secret TEXT NOT NULL, rights TEXT NOT NULL,"
    " name_hint TEXT, expires_at REAL NOT NULL, failures INTEGER NOT NULL DEFAULT 0)",
    "CREATE TABLE IF NOT EXISTS nonces(peer_id TEXT NOT NULL, nonce TEXT NOT NULL, ts INTEGER NOT NULL,"
    " PRIMARY KEY(peer_id, nonce))",
    "CREATE TABLE IF NOT EXISTS jobs(job_id TEXT NOT NULL, role TEXT NOT NULL, mode TEXT NOT NULL, peer_id TEXT,"
    " state TEXT NOT NULL, params TEXT NOT NULL, params_digest TEXT NOT NULL, owner TEXT,"
    " owner_epoch INTEGER NOT NULL DEFAULT 0, owner_heartbeat_at REAL,"
    " max_peer_epoch INTEGER NOT NULL DEFAULT 0, cancel_requested INTEGER NOT NULL DEFAULT 0,"
    " restarts INTEGER NOT NULL DEFAULT 0, created_at REAL NOT NULL, updated_at REAL NOT NULL,"
    " last_error TEXT, result TEXT, PRIMARY KEY(job_id, role))",
    "CREATE TABLE IF NOT EXISTS job_events(job_id TEXT NOT NULL, role TEXT NOT NULL, seq INTEGER NOT NULL,"
    " ts REAL NOT NULL, epoch INTEGER NOT NULL, kind TEXT NOT NULL, step TEXT, direction TEXT, state TEXT,"
    " data TEXT NOT NULL, PRIMARY KEY(job_id, role, seq))",
    "CREATE TABLE IF NOT EXISTS ops(job_id TEXT NOT NULL, op TEXT NOT NULL, digest TEXT NOT NULL,"
    " op_id TEXT NOT NULL UNIQUE, state TEXT NOT NULL, result TEXT, error TEXT, progress TEXT,"
    " updated_at REAL NOT NULL, PRIMARY KEY(job_id, op, digest))",
    "CREATE TABLE IF NOT EXISTS source_handovers(job_id TEXT PRIMARY KEY, root_id TEXT NOT NULL,"
    " tip_id TEXT NOT NULL, lineage_ids TEXT NOT NULL, state TEXT NOT NULL, holder TEXT, peer TEXT, title TEXT,"
    " prepared_at REAL, ack TEXT, bundle_path TEXT, at REAL NOT NULL, updated_at REAL NOT NULL)",
    "CREATE UNIQUE INDEX IF NOT EXISTS one_active_handover ON source_handovers(root_id)"
    " WHERE state IN ('locking','locked','preparing','prepared','retiring','retired','exported')",
    "CREATE TABLE IF NOT EXISTS veto_ids(session_id TEXT NOT NULL, root_id TEXT NOT NULL,"
    " job_id TEXT NOT NULL REFERENCES source_handovers(job_id), PRIMARY KEY(session_id, job_id))",
    "CREATE TABLE IF NOT EXISTS backups(job_id TEXT NOT NULL, key TEXT NOT NULL, value TEXT, PRIMARY KEY(job_id, key))",
    "CREATE TABLE IF NOT EXISTS arrivals(session_id TEXT PRIMARY KEY, root_id TEXT NOT NULL, job_id TEXT NOT NULL,"
    " peer TEXT, note TEXT NOT NULL, report_path TEXT, path_map TEXT NOT NULL, continue_prompt TEXT,"
    " note_state TEXT NOT NULL DEFAULT 'pending', offered_turn TEXT, offers INTEGER NOT NULL DEFAULT 0,"
    " at REAL NOT NULL, source_reclaimed_at REAL)",
    "CREATE TABLE IF NOT EXISTS bundle_imports(bundle_job_id TEXT PRIMARY KEY, session_id TEXT NOT NULL,"
    " at REAL NOT NULL)",
    "CREATE TABLE IF NOT EXISTS workspace_map(local_path TEXT NOT NULL, peer_id TEXT NOT NULL,"
    " remote_path TEXT NOT NULL, direction TEXT NOT NULL, repo_identity TEXT, state TEXT,"
    " last_job_id TEXT NOT NULL, at REAL NOT NULL, PRIMARY KEY(local_path, peer_id, remote_path, direction))",
    "CREATE TABLE IF NOT EXISTS blocks(block_id TEXT PRIMARY KEY, size INTEGER NOT NULL, last_used_at REAL NOT NULL)",
    "CREATE TABLE IF NOT EXISTS objects(object_id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, size INTEGER NOT NULL,"
    " media TEXT, at REAL NOT NULL)",
    "CREATE TABLE IF NOT EXISTS job_objects(job_id TEXT NOT NULL, object_id TEXT NOT NULL,"
    " PRIMARY KEY(job_id, object_id))",
    "CREATE TABLE IF NOT EXISTS suggestions(session_id TEXT NOT NULL, at REAL NOT NULL, data TEXT NOT NULL)",
    "CREATE TABLE IF NOT EXISTS offers(offer_id TEXT PRIMARY KEY, peer_id TEXT NOT NULL, session_id TEXT NOT NULL,"
    " note TEXT, refinement TEXT, created_at REAL NOT NULL, expires_at REAL NOT NULL, claimed_job TEXT)",
    "CREATE TABLE IF NOT EXISTS pull_log(job_id TEXT NOT NULL, peer_id TEXT NOT NULL, at REAL NOT NULL,"
    " items TEXT NOT NULL)",
    "CREATE TABLE IF NOT EXISTS job_quota(job_id TEXT NOT NULL, role TEXT NOT NULL, quota INTEGER NOT NULL,"
    " used INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(job_id, role))",
    "CREATE TABLE IF NOT EXISTS ctx_outbox(id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL,"
    " payload TEXT NOT NULL, created_at REAL NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,"
    " next_try_at REAL NOT NULL, done_at REAL)",
    "CREATE TABLE IF NOT EXISTS peer_facts(peer_id TEXT PRIMARY KEY, facts TEXT NOT NULL, at REAL NOT NULL)",
)

_PROCESS: dict[str, object] = {}


def process_owner() -> str:
    """``<pid>:<process start nonce>`` — the owner string of this process (regenerated after a fork)."""
    pid = os.getpid()
    if _PROCESS.get("pid") != pid:
        _PROCESS["pid"] = pid
        _PROCESS["owner"] = f"{pid}:{secrets.token_hex(4)}"
    return str(_PROCESS["owner"])


def private_dir(path: Path) -> Path:
    """Create ``path`` and every missing parent with mode 0700 (chmod after mkdir: umask-proof, SEC 6.1)."""
    path = Path(path)
    missing = []
    p = path
    while not p.exists():
        missing.append(p)
        if p.parent == p:
            break
        p = p.parent
    for d in reversed(missing):
        try:
            d.mkdir(mode=0o700)
        except FileExistsError:
            continue
        os.chmod(d, 0o700)
    return path


class Fenced(InvalidState):
    """The caller's owner epoch is no longer current: another process adopted the job (DESIGN §3.1).

    Wire code stays ``invalid_state`` (``details.reason = "stale_epoch"``); a runner that sees it stops at once
    without further side effects."""


def _dumps(value: object) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False, default=str)


def _loads(text: str | None) -> object:
    return None if text is None else json.loads(text)


def _rights_text(rights: frozenset[Right] | Sequence[Right]) -> str:
    return _dumps(sorted(str(r) for r in rights))


def _rights(text: str) -> frozenset[Right]:
    return frozenset(Right(r) for r in json.loads(text))


def _expect_set(expect: str | Sequence[str]) -> frozenset[str] | None:
    if isinstance(expect, str):
        return None if expect == "*" else frozenset({expect})
    values = frozenset(str(e) for e in expect)
    return None if "*" in values else values


class Ledger:
    """Process-shared durable state (see module docstring)."""

    def __init__(self, path: Path, *, clock: Callable[[], float] = time.time) -> None:
        self.path = Path(path)
        self._clock = clock
        self._local = threading.local()
        self._all: list[sqlite3.Connection] = []
        self._all_lock = threading.Lock()
        self.readonly = False
        self.schema_version = SCHEMA_VERSION
        self._prepare_files()
        self._init_schema()

    # -- files and connections -----------------------------------------------------------------------------
    def _prepare_files(self) -> None:
        private_dir(self.path.parent)
        if not self.path.exists():
            try:
                fd = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
                os.close(fd)
            except FileExistsError:
                pass
        self._fix_modes()

    def _fix_modes(self) -> None:
        for suffix in ("", "-wal", "-shm"):
            p = self.path.with_name(self.path.name + suffix)
            try:
                if p.exists() and (p.stat().st_mode & 0o777) != 0o600:
                    os.chmod(p, 0o600)
            except OSError:
                pass

    def _conn(self) -> sqlite3.Connection:
        conn = getattr(self._local, "conn", None)
        if conn is None:
            conn = sqlite3.connect(str(self.path), timeout=5.0, isolation_level=None)
            conn.row_factory = sqlite3.Row
            conn.execute("PRAGMA busy_timeout=5000")
            conn.execute("PRAGMA foreign_keys=ON")
            conn.execute("PRAGMA secure_delete=ON")
            if self.readonly:
                conn.execute("PRAGMA query_only=ON")
            self._local.conn = conn
            self._local.depth = 0
            with self._all_lock:
                self._all.append(conn)
        return conn

    def close(self) -> None:
        """Close every connection this object opened (all threads)."""
        with self._all_lock:
            conns, self._all = self._all, []
        for conn in conns:
            with contextlib.suppress(sqlite3.Error):
                conn.close()
        self._local = threading.local()
        self._fix_modes()

    @contextlib.contextmanager
    def tx(self) -> Iterator[sqlite3.Connection]:
        """``BEGIN IMMEDIATE`` transaction (re-entrant within a thread; the outermost commits)."""
        conn = self._conn()
        if self._local.depth:
            self._local.depth += 1
            try:
                yield conn
            finally:
                self._local.depth -= 1
            return
        conn.execute("BEGIN IMMEDIATE")
        self._local.depth = 1
        try:
            yield conn
        except BaseException:
            self._local.depth = 0
            with contextlib.suppress(sqlite3.Error):
                conn.execute("ROLLBACK")
            raise
        self._local.depth = 0
        conn.execute("COMMIT")

    @contextlib.contextmanager
    def _w(self) -> Iterator[sqlite3.Connection]:
        if self.readonly:
            raise InvalidState(
                f"ledger schema {self.schema_version} is newer than this plugin ({SCHEMA_VERSION}); downgrade "
                "detected — the ledger is read-only",
                reason="newer_schema",
                schema_version=self.schema_version,
            )
        with self.tx() as conn:
            yield conn

    def _q(self, sql: str, params: Sequence = ()) -> list[sqlite3.Row]:
        return self._conn().execute(sql, params).fetchall()

    def _q1(self, sql: str, params: Sequence = ()) -> sqlite3.Row | None:
        return self._conn().execute(sql, params).fetchone()

    def _init_schema(self) -> None:
        conn = self._conn()
        conn.execute("PRAGMA journal_mode=WAL")
        with self.tx() as c:
            c.execute("CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)")
            row = c.execute("SELECT value FROM meta WHERE key='schema_version'").fetchone()
            version = int(row[0]) if row else None
            if version is not None and version > SCHEMA_VERSION:
                self.readonly = True
                self.schema_version = version
            else:
                for stmt in _SCHEMA:
                    c.execute(stmt)
                c.execute(
                    "INSERT INTO meta(key, value) VALUES('schema_version', ?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                    (str(SCHEMA_VERSION),),
                )
        if self.readonly:
            conn.execute("PRAGMA query_only=ON")
        self._fix_modes()

    def data_version(self) -> int:
        """``PRAGMA data_version`` of this thread's connection (changes when another connection commits)."""
        return int(self._conn().execute("PRAGMA data_version").fetchone()[0])

    def now(self) -> float:
        return float(self._clock())

    # -- meta / identity -------------------------------------------------------------------------------------
    def meta_get(self, key: str) -> str | None:
        row = self._q1("SELECT value FROM meta WHERE key=?", (key,))
        return None if row is None else str(row[0])

    def meta_set(self, key: str, value: str, conn: sqlite3.Connection | None = None) -> None:
        sql = "INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value"
        if conn is not None:
            conn.execute(sql, (key, value))
            return
        with self._w() as c:
            c.execute(sql, (key, value))

    def claim_interval(self, key: str, now: float, interval_s: float) -> bool:
        """Claim a periodic task for all processes of this profile: True (and ``meta[key] = now``) when the last
        claim is older than ``interval_s``, absent, unreadable or in the future (clock moved back); False when
        another process (or an earlier call) claimed it within the interval. One ``BEGIN IMMEDIATE`` transaction,
        so two processes never both get True for the same interval."""
        with self._w() as c:
            row = c.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
            try:
                last = float(row[0]) if row is not None else None
            except ValueError:
                last = None
            if last is not None and last <= float(now) < last + float(interval_s):
                return False
            self.meta_set(key, repr(float(now)), conn=c)
            return True

    def instance_id(self) -> str:
        """This profile's ``instance_id`` (ULID), created on first use."""
        value = self.meta_get("instance_id")
        if value:
            return value
        with self._w() as c:
            row = c.execute("SELECT value FROM meta WHERE key='instance_id'").fetchone()
            if row:
                return str(row[0])
            value = ulid()
            self.meta_set("instance_id", value, conn=c)
            return value

    # -- peers / pairing / nonces ----------------------------------------------------------------------------
    @staticmethod
    def _peer(row: sqlite3.Row) -> Peer:
        gate = _loads(row["gate"])
        return Peer(
            name=row["name"],
            peer_id=row["peer_id"],
            direction=row["direction"],
            url=row["url"],
            pubkey=row["pubkey"],
            rights=_rights(row["rights"]),
            gate=from_wire(GateAuth, gate) if isinstance(gate, dict) else None,
            profile=row["profile"],
            allow_insecure=bool(row["allow_insecure"]),
            created_at=float(row["created_at"]),
            last_seen_at=float(row["last_seen_at"] or 0.0),
            last_ip=row["last_ip"],
            revoked_at=row["revoked_at"],
        )

    def peers(self, *, include_revoked: bool = False) -> list[Peer]:
        where = "" if include_revoked else " WHERE revoked_at IS NULL"
        sql = "SELECT * FROM peers" + where + " ORDER BY name"  # noqa: S608 - fixed literals only
        return [self._peer(r) for r in self._q(sql)]

    def peer(self, name_or_id: str) -> Peer | None:
        row = self._q1("SELECT * FROM peers WHERE peer_id=? OR name=? ORDER BY peer_id=? DESC", (name_or_id,) * 3)
        return None if row is None else self._peer(row)

    def inbound_peer(self, peer_id: str) -> Peer | None:
        """The peer that may authenticate requests to this listener: ``direction='inbound'``, not revoked."""
        row = self._q1("SELECT * FROM peers WHERE peer_id=? AND direction='inbound' AND revoked_at IS NULL", (peer_id,))
        return None if row is None else self._peer(row)

    def _insert_peer(self, c: sqlite3.Connection, peer: Peer, *, upsert: bool) -> None:
        gate = _dumps(to_wire(peer.gate)) if peer.gate is not None else None
        values = (
            peer.peer_id,
            peer.name,
            peer.direction,
            peer.url,
            peer.pubkey,
            _rights_text(peer.rights),
            gate,
            peer.profile,
            int(peer.allow_insecure),
            peer.created_at or self.now(),
            peer.last_seen_at or None,
            peer.last_ip,
            peer.revoked_at,
        )
        sql = (
            "INSERT INTO peers(peer_id, name, direction, url, pubkey, rights, gate, profile, allow_insecure,"
            " created_at, last_seen_at, last_ip, revoked_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)"
        )
        if upsert:
            sql += (
                " ON CONFLICT(peer_id) DO UPDATE SET name=excluded.name, direction=excluded.direction,"
                " url=excluded.url, pubkey=excluded.pubkey, rights=excluded.rights, gate=excluded.gate,"
                " profile=excluded.profile, allow_insecure=excluded.allow_insecure, revoked_at=excluded.revoked_at"
            )
        c.execute(sql, values)

    def upsert_peer(self, peer: Peer) -> None:
        with self._w() as c:
            clash = c.execute("SELECT peer_id FROM peers WHERE name=? AND peer_id<>?", (peer.name, peer.peer_id))
            if clash.fetchone():
                raise ConflictError(f"a peer named {peer.name!r} already exists", field="name")
            self._insert_peer(c, peer, upsert=True)

    def revoke_peer(self, name_or_id: str) -> list[str]:
        """Listener side, one transaction (DESIGN §12.5): mark revoked, delete its offers, set its abortable
        participant jobs to ``aborting`` and return their ids. Sources in ``prepared``/``retiring`` and targets in
        ``committing``/``committed`` keep their state (in-doubt rule, §3.4)."""
        with self._w() as c:
            row = c.execute("SELECT peer_id FROM peers WHERE peer_id=? OR name=?", (name_or_id, name_or_id)).fetchone()
            if row is None:
                raise NotFound(f"no peer {name_or_id!r}")
            peer_id = row[0]
            now = self.now()
            c.execute("UPDATE peers SET revoked_at=? WHERE peer_id=? AND revoked_at IS NULL", (now, peer_id))
            c.execute("DELETE FROM offers WHERE peer_id=?", (peer_id,))
            hits: list[tuple[str, str, int]] = []
            jobs = c.execute(
                "SELECT job_id, role, state, owner_epoch FROM jobs WHERE peer_id=? AND role<>'conductor'", (peer_id,)
            ).fetchall()
            for job in jobs:
                role, state = job["role"], job["state"]
                if role == Role.TARGET:
                    if state in _REVOKE_TARGET_KEEP:
                        continue
                else:
                    src = c.execute("SELECT state FROM source_handovers WHERE job_id=?", (job["job_id"],)).fetchone()
                    if src is None and state in SOURCE_JOB_TERMINAL | {"aborting"}:
                        continue
                    if src is not None and src[0] not in _REVOKE_SOURCE_STATES:
                        continue
                hits.append((job["job_id"], role, int(job["owner_epoch"])))
            for job_id, role, epoch in hits:
                c.execute(
                    "UPDATE jobs SET state='aborting', updated_at=? WHERE job_id=? AND role=?", (now, job_id, role)
                )
                self._append(c, job_id, Role(role), epoch, "transition", {"reason": "peer_revoked"}, state="aborting")
            return [h[0] for h in hits]

    def remove_peer(self, name_or_id: str) -> bool:
        """Dialer side: delete a dial peer's entry, pinned key and last known facts (DESIGN §12.5)."""
        with self._w() as c:
            row = c.execute(
                "SELECT peer_id FROM peers WHERE (peer_id=? OR name=?) AND direction='dial' ORDER BY peer_id=? DESC",
                (name_or_id,) * 3,
            ).fetchone()
            if row is None:
                return False
            c.execute("DELETE FROM peers WHERE peer_id=?", (row[0],))
            c.execute("DELETE FROM peer_facts WHERE peer_id=?", (row[0],))
            return True

    def peer_facts_set(self, peer_id: str, facts_wire: dict) -> None:
        """Keep the target facts last received from ``peer_id`` (its ``hello``/``facts`` answer, wire form of
        ``EnvFacts``; contract V28): ``export --for <peer>`` negotiates with them (DESIGN §5.4). The newest answer
        replaces the older one."""
        if not isinstance(peer_id, str) or not peer_id:
            raise TypeError("peer_facts_set needs a peer id")
        if not isinstance(facts_wire, dict):
            raise TypeError("peer_facts_set needs the wire form of the facts (a dict)")
        with self._w() as c:
            c.execute(
                "INSERT INTO peer_facts(peer_id, facts, at) VALUES(?,?,?) ON CONFLICT(peer_id) DO UPDATE SET"
                " facts=excluded.facts, at=excluded.at",
                (peer_id, _dumps(facts_wire), self.now()),
            )

    def peer_facts_get(self, peer_id: str) -> dict | None:
        """The facts ``peer_facts_set`` kept for ``peer_id`` (a fresh dict), or None when none were received or the
        stored text is not a JSON object."""
        row = self._q1("SELECT facts FROM peer_facts WHERE peer_id=?", (peer_id,))
        if row is None:
            return None
        try:
            value = _loads(row[0])
        except ValueError:
            return None
        return value if isinstance(value, dict) else None

    def rename_peer(self, old: str, new: str) -> bool:
        with self._w() as c:
            if c.execute("SELECT 1 FROM peers WHERE name=?", (new,)).fetchone():
                raise ConflictError(f"a peer named {new!r} already exists", field="name")
            cur = c.execute("UPDATE peers SET name=? WHERE name=? OR peer_id=?", (new, old, old))
            return cur.rowcount > 0

    def touch_peer(self, peer_id: str, ip: str | None) -> None:
        """Display-only last contact (``last_ip`` is never used for decisions, SEC 1.9)."""
        with self._w() as c:
            c.execute("UPDATE peers SET last_seen_at=?, last_ip=? WHERE peer_id=?", (self.now(), ip, peer_id))

    def put_pair_code(
        self, code_id: str, secret: str, rights: frozenset[Right], name_hint: str, expires_at: float
    ) -> None:
        with self._w() as c:
            c.execute(
                "INSERT INTO pair_codes(code_id, secret, rights, name_hint, expires_at, failures) VALUES(?,?,?,?,?,0)",
                (code_id, secret, _rights_text(rights), name_hint, float(expires_at)),
            )

    def pair_code(self, code_id: str, now: float) -> tuple[str, frozenset[Right], str] | None:
        row = self._q1(
            "SELECT secret, rights, name_hint FROM pair_codes WHERE code_id=? AND expires_at>? AND failures<5",
            (code_id, float(now)),
        )
        return None if row is None else (row[0], _rights(row[1]), row[2] or "")

    def redeem_pair_code(self, code_id: str, now: float, peer: Peer) -> bool:
        """Burn the code and insert the inbound peer atomically (DESIGN §12.3 step 5, SEC 1.3).

        The DELETE must hit exactly one row; a second process redeeming the same code gets ``False``. A name
        that is already taken gets a numeric suffix."""
        with self._w() as c:
            cur = c.execute(
                "DELETE FROM pair_codes WHERE code_id=? AND failures<5 AND expires_at>?", (code_id, float(now))
            )
            if cur.rowcount != 1:
                return False
            redeemed = True
            name, n = peer.name, 1
            while c.execute("SELECT 1 FROM peers WHERE name=?", (name,)).fetchone():
                n += 1
                name = f"{peer.name}-{n}"
            stored = Peer(
                name=name,
                peer_id=peer.peer_id,
                direction="inbound",
                url=peer.url,
                pubkey=peer.pubkey,
                rights=peer.rights,
                gate=None,
                profile=peer.profile,
                allow_insecure=peer.allow_insecure,
                created_at=float(now),
                last_seen_at=float(now),
                last_ip=peer.last_ip,
            )
            self._insert_peer(c, stored, upsert=False)
        if redeemed:  # the burned code must not linger in WAL frames either (T2, SEC 6.2)
            with contextlib.suppress(sqlite3.Error):
                self._conn().execute("PRAGMA wal_checkpoint(TRUNCATE)")
        return True

    def fail_pair_code(self, code_id: str) -> int:
        """Count a failed proof; never above 5 (the code is burned at 5). Returns the failure count."""
        with self._w() as c:
            c.execute("UPDATE pair_codes SET failures=failures+1 WHERE code_id=? AND failures<5", (code_id,))
            row = c.execute("SELECT failures FROM pair_codes WHERE code_id=?", (code_id,)).fetchone()
            return int(row[0]) if row else 0

    def purge_expired_codes(self, now: float) -> int:
        """Janitor: delete expired and burned codes (secure_delete) and checkpoint the WAL (SEC 6.2)."""
        with self._w() as c:
            n = c.execute("DELETE FROM pair_codes WHERE expires_at<=? OR failures>=5", (float(now),)).rowcount
        with contextlib.suppress(sqlite3.Error):
            self._conn().execute("PRAGMA wal_checkpoint(TRUNCATE)")
        return n

    def pairing_pending(self, now: float) -> bool:
        return self._q1("SELECT 1 FROM pair_codes WHERE expires_at>? AND failures<5", (float(now),)) is not None

    def nonce_seen(self, peer_id: str, nonce: str, ts: int, ttl_s: int) -> bool:
        """Insert the nonce and prune expired ones in the same transaction; True if it was already present."""
        with self._w() as c:
            c.execute("DELETE FROM nonces WHERE ts<?", (int(self.now()) - int(ttl_s),))
            cur = c.execute("INSERT OR IGNORE INTO nonces(peer_id, nonce, ts) VALUES(?,?,?)", (peer_id, nonce, int(ts)))
            return cur.rowcount == 0

    # -- jobs, journal, ownership ------------------------------------------------------------------------------
    @staticmethod
    def _job(row: sqlite3.Row) -> dict:
        d = dict(row)
        d["params"] = _loads(d["params"])
        d["last_error"] = _loads(d["last_error"])
        d["result"] = _loads(d["result"])
        d["cancel_requested"] = bool(d["cancel_requested"])
        return d

    def create_job(self, job_id: str, role: Role, mode: Mode, peer_id: str | None, params: dict) -> None:
        """Create a job row (idempotent for the same peer, mode and params; otherwise ``JobConflict``)."""
        encoded = canonical_bytes(params)
        digest = sha256_hex(encoded)
        with self._w() as c:
            row = c.execute(
                "SELECT peer_id, mode, params_digest FROM jobs WHERE job_id=? AND role=?", (job_id, str(role))
            ).fetchone()
            if row is not None:
                if row["peer_id"] == peer_id and row["mode"] == str(mode) and row["params_digest"] == digest:
                    return
                raise JobConflict("job id reused with other parameters", job_id=job_id)
            now = self.now()
            state = "created" if role == Role.CONDUCTOR else "open"
            c.execute(
                "INSERT INTO jobs(job_id, role, mode, peer_id, state, params, params_digest, created_at, updated_at)"
                " VALUES(?,?,?,?,?,?,?,?,?)",
                (job_id, str(role), str(mode), peer_id, state, encoded.decode("utf-8"), digest, now, now),
            )
            self._append(c, job_id, role, 0, "created", {"mode": str(mode), "peer_id": peer_id}, state=state)

    def job(self, job_id: str, role: Role) -> dict | None:
        row = self._q1("SELECT * FROM jobs WHERE job_id=? AND role=?", (job_id, str(role)))
        return None if row is None else self._job(row)

    def jobs(self, *, role: Role | None = None, active: bool | None = None, limit: int = 20) -> list[dict]:
        rows = self._q(
            "SELECT * FROM jobs" + (" WHERE role=?" if role else "") + " ORDER BY created_at DESC, job_id DESC",  # noqa: S608 - fixed literals only
            (str(role),) if role else (),
        )
        out = []
        for r in rows:
            d = self._job(r)
            terminal = d["state"] in TERMINAL[Role(d["role"])]
            if (active is True and terminal) or (active is False and not terminal):
                continue
            out.append(d)
            if len(out) >= limit:
                break
        return out

    def claim_owner(self, job_id: str, role: Role, owner: str, stale_after_s: float) -> int | None:
        """Take ownership: free or stale (heartbeat older than ``stale_after_s``) ⇒ new epoch; the same owner ⇒
        its current epoch; a live other owner ⇒ None (DESIGN §3.1)."""
        with self._w() as c:
            row = c.execute(
                "SELECT owner, owner_epoch, owner_heartbeat_at FROM jobs WHERE job_id=? AND role=?",
                (job_id, str(role)),
            ).fetchone()
            if row is None:
                raise NotFound(f"job {job_id} ({role}) not found")
            now = self.now()
            if row["owner"] == owner:
                c.execute("UPDATE jobs SET owner_heartbeat_at=? WHERE job_id=? AND role=?", (now, job_id, str(role)))
                return int(row["owner_epoch"])
            hb = row["owner_heartbeat_at"]
            if row["owner"] is not None and hb is not None and hb >= now - float(stale_after_s):
                return None
            epoch = int(row["owner_epoch"]) + 1
            c.execute(
                "UPDATE jobs SET owner=?, owner_epoch=?, owner_heartbeat_at=? WHERE job_id=? AND role=?",
                (owner, epoch, now, job_id, str(role)),
            )
            self._append(c, job_id, role, epoch, "owner", {"owner": owner, "previous": row["owner"]})
            return epoch

    def release_owner(self, job_id: str, role: Role, owner: str, epoch: int) -> None:
        """Give ownership up (the epoch stays; the next claim increments it)."""
        with self._w() as c:
            c.execute(
                "UPDATE jobs SET owner=NULL, owner_heartbeat_at=NULL WHERE job_id=? AND role=? AND owner=? "
                "AND owner_epoch=?",
                (job_id, str(role), owner, int(epoch)),
            )

    def heartbeat(self, job_id: str, role: Role, owner: str, epoch: int) -> bool:
        with self._w() as c:
            cur = c.execute(
                "UPDATE jobs SET owner_heartbeat_at=? WHERE job_id=? AND role=? AND owner=? AND owner_epoch=?",
                (self.now(), job_id, str(role), owner, int(epoch)),
            )
            return cur.rowcount == 1

    def _check_epoch(self, c: sqlite3.Connection, job_id: str, role: Role, epoch: int) -> sqlite3.Row:
        row = c.execute("SELECT * FROM jobs WHERE job_id=? AND role=?", (job_id, str(role))).fetchone()
        if row is None:
            raise NotFound(f"job {job_id} ({role}) not found")
        if int(row["owner_epoch"]) != int(epoch):
            raise Fenced(
                "the job was adopted by another process",
                reason="stale_epoch",
                epoch=int(epoch),
                current=int(row["owner_epoch"]),
            )
        return row

    def _append(
        self,
        c: sqlite3.Connection,
        job_id: str,
        role: Role,
        epoch: int,
        kind: str,
        data: dict,
        *,
        state: str | None = None,
    ) -> None:
        r = str(role)
        seq = c.execute(
            "SELECT COALESCE(MAX(seq), 0) + 1 FROM job_events WHERE job_id=? AND role=?", (job_id, r)
        ).fetchone()[0]
        c.execute(
            "INSERT INTO job_events(job_id, role, seq, ts, epoch, kind, step, direction, state, data)"
            " VALUES(?,?,?,?,?,?,?,?,?,?)",
            (
                job_id,
                r,
                seq,
                self.now(),
                int(epoch),
                kind,
                data.get("step"),
                data.get("direction"),
                state,
                _dumps(data),
            ),
        )

    def transition(
        self, job_id: str, role: Role, expect: str | Sequence[str], to: str, event: dict, *, epoch: int
    ) -> None:
        """CAS on state **and** owner epoch, then append the transition event (one transaction).

        ``expect`` is a state, a sequence of states, or ``"*"``. Stale epoch ⇒ ``Fenced``; wrong state ⇒
        ``InvalidState(reason="state", state=<actual>)``; in both cases nothing is written."""
        allowed = _expect_set(expect)
        with self._w() as c:
            row = self._check_epoch(c, job_id, role, epoch)
            if allowed is not None and row["state"] not in allowed:
                raise InvalidState(
                    f"job {job_id} is {row['state']}, expected {sorted(allowed)}",
                    reason="state",
                    state=row["state"],
                    expected=sorted(allowed),
                )
            c.execute(
                "UPDATE jobs SET state=?, updated_at=? WHERE job_id=? AND role=?",
                (str(to), self.now(), job_id, str(role)),
            )
            self._append(c, job_id, role, epoch, "transition", dict(event), state=str(to))

    def event(self, job_id: str, role: Role, kind: str, data: dict, *, epoch: int) -> None:
        """Append a journal event (``intent``/``done``/``info``/``step`` …) with an owner-epoch CAS."""
        with self._w() as c:
            self._check_epoch(c, job_id, role, epoch)
            self._append(c, job_id, role, epoch, kind, dict(data))

    def events(self, job_id: str, role: Role) -> list[dict]:
        out = []
        for r in self._q("SELECT * FROM job_events WHERE job_id=? AND role=? ORDER BY seq", (job_id, str(role))):
            d = dict(r)
            d["data"] = _loads(d["data"])
            out.append(d)
        return out

    def job_update(self, job_id: str, role: Role, *, epoch: int | None = None, **fields: object) -> None:
        """Update ``restarts``, ``last_error``, ``result`` (CAS on epoch when given). A job row that does not exist
        is ``NotFound`` — with or without an epoch: an error or result is never reported as recorded when nothing
        was written."""
        allowed = {"restarts", "last_error", "result"}
        unknown = set(fields) - allowed
        if unknown:
            raise ValueError(f"unknown job fields {sorted(unknown)}")
        with self._w() as c:
            if epoch is not None:
                self._check_epoch(c, job_id, role, epoch)
            sets, values = ["updated_at=?"], [self.now()]
            for key, value in fields.items():
                sets.append(f"{key}=?")
                values.append(value if key == "restarts" else (None if value is None else _dumps(value)))
            cur = c.execute(
                f"UPDATE jobs SET {', '.join(sets)} WHERE job_id=? AND role=?",  # noqa: S608 - fixed column names
                (*values, job_id, str(role)),
            )
            if cur.rowcount != 1:
                raise NotFound(f"job {job_id} ({role}) not found")

    def touch_job(self, job_id: str, role: Role) -> None:
        """Record activity (participant request) without a transition: ``updated_at = now``."""
        with self._w() as c:
            c.execute("UPDATE jobs SET updated_at=? WHERE job_id=? AND role=?", (self.now(), job_id, str(role)))

    def stale_jobs(self, stale_after_s: float) -> list[dict]:
        """Non-terminal jobs whose owner missed its heartbeat (or that have no owner and no recent activity)."""
        cutoff = self.now() - float(stale_after_s)
        out = []
        for r in self._q(
            "SELECT * FROM jobs WHERE (owner IS NOT NULL AND owner_heartbeat_at<?) OR (owner IS NULL AND updated_at<?)"
            " ORDER BY created_at",
            (cutoff, cutoff),
        ):
            d = self._job(r)
            if d["state"] not in TERMINAL[Role(d["role"])]:
                out.append(d)
        return out

    def peer_epoch(self, job_id: str, role: Role, epoch: int) -> int:
        """Participant side: highest conductor epoch seen; a lower one ⇒ ``InvalidState(stale_epoch)``."""
        with self._w() as c:
            row = c.execute("SELECT max_peer_epoch FROM jobs WHERE job_id=? AND role=?", (job_id, str(role))).fetchone()
            if row is None:
                raise NotFound(f"job {job_id} not found")
            current = int(row[0])
            if int(epoch) < current:
                raise InvalidState(
                    "request from an older conductor epoch", reason="stale_epoch", epoch=int(epoch), current=current
                )
            if int(epoch) > current:
                c.execute("UPDATE jobs SET max_peer_epoch=? WHERE job_id=? AND role=?", (int(epoch), job_id, str(role)))
            return max(int(epoch), current)

    def _commit_intended(self, c: sqlite3.Connection, job_id: str) -> bool:
        return (
            c.execute(
                "SELECT 1 FROM job_events WHERE job_id=? AND role='conductor' AND kind='intent' AND step='commit'",
                (job_id,),
            ).fetchone()
            is not None
        )

    def set_cancel(self, job_id: str) -> None:
        """Request cancellation of a conductor job; refused once ``intent: commit`` is journaled (DESIGN §3.8)."""
        with self._w() as c:
            row = c.execute("SELECT state FROM jobs WHERE job_id=? AND role='conductor'", (job_id,)).fetchone()
            if row is None:
                raise NotFound(f"job {job_id} not found")
            if self._commit_intended(c, job_id):
                raise InvalidState(
                    "Too late to cancel: the target is committing. Use /handover reclaim on the source to take the "
                    "session back.",
                    reason="committing",
                    state=row[0],
                )
            if row[0] in CONDUCTOR_TERMINAL:
                return
            c.execute(
                "UPDATE jobs SET cancel_requested=1, updated_at=? WHERE job_id=? AND role='conductor'",
                (self.now(), job_id),
            )

    def cancel_requested(self, job_id: str) -> bool:
        row = self._q1("SELECT cancel_requested FROM jobs WHERE job_id=? AND role='conductor'", (job_id,))
        return bool(row and row[0])

    def intent_commit(self, job_id: str, epoch: int) -> bool:
        """Journal ``intent: commit`` unless a cancel was requested (serialised with ``set_cancel``)."""
        with self._w() as c:
            row = self._check_epoch(c, job_id, Role.CONDUCTOR, epoch)
            if row["cancel_requested"]:
                return False
            if not self._commit_intended(c, job_id):
                self._append(c, job_id, Role.CONDUCTOR, epoch, "intent", {"step": "commit"})
            return True

    def compact_terminal(self, older_than_s: float, *, ops_older_than_s: float = OPS_KEEP_S) -> int:
        """Janitor: journals of terminal jobs older than ``older_than_s`` shrink to their last event; ``ops`` rows of
        terminal jobs older than ``ops_older_than_s`` go (DESIGN §3.1, SK §5). Returns deleted events."""
        now = self.now()
        deleted = 0
        with self._w() as c:
            for r in c.execute("SELECT job_id, role, state, updated_at FROM jobs").fetchall():
                if r["state"] not in TERMINAL[Role(r["role"])]:
                    continue
                if r["updated_at"] < now - float(older_than_s):
                    last = c.execute(
                        "SELECT MAX(seq) FROM job_events WHERE job_id=? AND role=?", (r["job_id"], r["role"])
                    ).fetchone()[0]
                    if last is not None:
                        deleted += c.execute(
                            "DELETE FROM job_events WHERE job_id=? AND role=? AND seq<?", (r["job_id"], r["role"], last)
                        ).rowcount
                if r["updated_at"] < now - float(ops_older_than_s):
                    c.execute("DELETE FROM ops WHERE job_id=?", (r["job_id"],))
        return deleted

    # -- ops ---------------------------------------------------------------------------------------------------
    @staticmethod
    def _op(row: sqlite3.Row) -> dict:
        d = dict(row)
        for key in ("result", "error", "progress"):
            d[key] = _loads(d[key])
        return d

    def op_get(self, job_id: str, op: str, digest: str) -> dict | None:
        row = self._q1("SELECT * FROM ops WHERE job_id=? AND op=? AND digest=?", (job_id, op, digest))
        return None if row is None else self._op(row)

    def op_put(
        self,
        job_id: str,
        op: str,
        digest: str,
        op_id: str,
        state: OpState,
        *,
        result: dict | None = None,
        error: dict | None = None,
        progress: dict | None = None,
    ) -> None:
        with self._w() as c:
            c.execute(
                "INSERT INTO ops(job_id, op, digest, op_id, state, result, error, progress, updated_at)"
                " VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(job_id, op, digest) DO UPDATE SET op_id=excluded.op_id,"
                " state=excluded.state, result=excluded.result, error=excluded.error, progress=excluded.progress,"
                " updated_at=excluded.updated_at",
                (
                    job_id,
                    op,
                    digest,
                    op_id,
                    str(state),
                    None if result is None else _dumps(result),
                    None if error is None else _dumps(error),
                    None if progress is None else _dumps(progress),
                    self.now(),
                ),
            )

    def op_insert(self, job_id: str, op: str, digest: str, op_id: str) -> bool:
        """Create the ``running`` row of a new operation; False (nothing written) when another process created the
        row for ``(job_id, op, digest)`` first — the caller then works with that row (never two op ids)."""
        with self._w() as c:
            cur = c.execute(
                "INSERT INTO ops(job_id, op, digest, op_id, state, result, error, progress, updated_at)"
                " VALUES(?,?,?,?,?,NULL,NULL,NULL,?) ON CONFLICT(job_id, op, digest) DO NOTHING",
                (job_id, op, digest, op_id, str(OpState.RUNNING), self.now()),
            )
            return cur.rowcount == 1

    def op_cas(
        self,
        op_id: str,
        expect: OpState | str,
        state: OpState,
        *,
        error: dict | None = None,
        keep_progress: bool = False,
    ) -> bool:
        """Compare-and-set the state of an operation: only when it is still ``expect``. Used for a restart
        (``failed`` → ``running``: exactly one restarter wins, a result written meanwhile is never overwritten) and
        for marking an orphan interrupted (``running`` → ``failed``). Clears ``result``; ``progress`` is cleared
        unless ``keep_progress``."""
        with self._w() as c:
            progress = "progress" if keep_progress else "NULL"
            cur = c.execute(
                f"UPDATE ops SET state=?, result=NULL, error=?, progress={progress}, updated_at=?"  # noqa: S608
                " WHERE op_id=? AND state=?",
                (str(state), None if error is None else _dumps(error), self.now(), op_id, str(expect)),
            )
            return cur.rowcount == 1

    def op_claim(self, op_id: str) -> bool:
        """The worker's last check before it runs an operation's function: True (and ``updated_at`` refreshed)
        only while the row is still ``running``. False when the operation was finished, failed or marked
        interrupted meanwhile (a late worker of a hung process must not run the steps again, DESIGN §3.1)."""
        with self._w() as c:
            cur = c.execute("UPDATE ops SET updated_at=? WHERE op_id=? AND state='running'", (self.now(), op_id))
            return cur.rowcount == 1

    def op_expire(self, op_id: str) -> None:
        """Make a ``running`` row stale at once (``updated_at = 0``) without changing its state: a worker that could
        not take the job lock hands the row to the next status check, which marks it interrupted only once nobody
        holds the lock (``ops`` rule 3); a holder's heartbeat or result overrides it."""
        with self._w() as c:
            c.execute("UPDATE ops SET updated_at=0 WHERE op_id=? AND state='running'", (op_id,))

    def op_progress(self, op_id: str, progress: dict | None) -> None:
        """Record progress of a running op and refresh its ``updated_at`` (liveness)."""
        with self._w() as c:
            if progress is None:
                c.execute("UPDATE ops SET updated_at=? WHERE op_id=? AND state='running'", (self.now(), op_id))
            else:
                c.execute(
                    "UPDATE ops SET progress=?, updated_at=? WHERE op_id=? AND state='running'",
                    (_dumps(progress), self.now(), op_id),
                )

    def op_by_id(self, op_id: str) -> dict | None:
        row = self._q1("SELECT * FROM ops WHERE op_id=?", (op_id,))
        return None if row is None else self._op(row)

    def ops_for_job(self, job_id: str) -> list[dict]:
        return [self._op(r) for r in self._q("SELECT * FROM ops WHERE job_id=? ORDER BY updated_at", (job_id,))]

    # -- source handovers, veto, backups ---------------------------------------------------------------------
    @staticmethod
    def _source(row: sqlite3.Row) -> dict:
        d = dict(row)
        d["lineage_ids"] = _loads(d["lineage_ids"])
        d["ack"] = _loads(d["ack"])
        return d

    def _conflicting(self, c: sqlite3.Connection, root_id: str, job_id: str) -> str | None:
        placeholders = ",".join("?" * len(ACTIVE_SOURCE_STATES))
        row = c.execute(
            f"SELECT job_id FROM source_handovers WHERE root_id=? AND job_id<>? AND state IN ({placeholders})",  # noqa: S608
            (root_id, job_id, *ACTIVE_SOURCE_STATES),
        ).fetchone()
        return None if row is None else str(row[0])

    def source_lock(self, job_id: str, info: SessionInfo, holder: str, peer: str | None) -> None:
        """Record the lock of a lineage for ``job_id`` (state ``locking``); one active handover per root.

        Re-locking the same job (``locking``/``locked``/``suspended``/``released`` — a resumed job whose leases the
        janitor released) is idempotent and records the new holder."""
        now = self.now()
        with self._w() as c:
            other = self._conflicting(c, info.root_id, job_id)
            if other is not None:
                raise AlreadyHandingOver("This session is already being handed over.", job=other, root_id=info.root_id)
            row = c.execute("SELECT state FROM source_handovers WHERE job_id=?", (job_id,)).fetchone()
            if row is not None:
                if row[0] not in ("locking", "locked", "suspended", "released"):
                    raise InvalidState(f"source of job {job_id} is {row[0]}", reason="state", state=row[0])
                c.execute(
                    "UPDATE source_handovers SET holder=?, tip_id=?, lineage_ids=?, peer=COALESCE(?, peer),"
                    " state=CASE WHEN state IN ('suspended','released') THEN 'locking' ELSE state END, updated_at=?"
                    " WHERE job_id=?",
                    (holder, info.tip_id, _dumps(list(info.lineage_ids)), peer, now, job_id),
                )
                return
            try:
                c.execute(
                    "INSERT INTO source_handovers(job_id, root_id, tip_id, lineage_ids, state, holder, peer, title, at,"
                    " updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
                    (
                        job_id,
                        info.root_id,
                        info.tip_id,
                        _dumps(list(info.lineage_ids)),
                        "locking",
                        holder,
                        peer,
                        info.title,
                        now,
                        now,
                    ),
                )
            except sqlite3.IntegrityError:
                raise AlreadyHandingOver(
                    "This session is already being handed over.", job=self._conflicting(c, info.root_id, job_id)
                ) from None

    def source_state(self, job_id: str) -> dict | None:
        row = self._q1("SELECT * FROM source_handovers WHERE job_id=?", (job_id,))
        return None if row is None else self._source(row)

    def source_set(self, job_id: str, state: SourceState | str, **fields: object) -> None:
        """Set the source state and any of ``holder, peer, title, prepared_at, ack, bundle_path, tip_id,
        lineage_ids``; entering an active state while another job holds the lineage ⇒ ``AlreadyHandingOver``."""
        allowed = {"holder", "peer", "title", "prepared_at", "ack", "bundle_path", "tip_id", "lineage_ids"}
        unknown = set(fields) - allowed
        if unknown:
            raise ValueError(f"unknown source fields {sorted(unknown)}")
        with self._w() as c:
            row = c.execute("SELECT root_id FROM source_handovers WHERE job_id=?", (job_id,)).fetchone()
            if row is None:
                raise NotFound(f"no source handover for job {job_id}")
            if str(state) in ACTIVE_SOURCE_STATES:
                other = self._conflicting(c, row[0], job_id)
                if other is not None:
                    raise AlreadyHandingOver("This session is already being handed over.", job=other)
            sets, values = ["state=?", "updated_at=?"], [str(state), self.now()]
            for key, value in fields.items():
                sets.append(f"{key}=?")
                if key == "ack":
                    value = None if value is None else _dumps(value)
                elif key == "lineage_ids":
                    value = _dumps(list(value))  # type: ignore[arg-type]
                values.append(value)
            c.execute(f"UPDATE source_handovers SET {', '.join(sets)} WHERE job_id=?", (*values, job_id))  # noqa: S608

    def source_by_session(self, session_id: str) -> dict | None:
        """The most recent handover whose lineage (or veto index) contains ``session_id``."""
        for row in self._q("SELECT * FROM source_handovers ORDER BY at DESC, rowid DESC"):
            d = self._source(row)
            if session_id == d["root_id"] or session_id == d["tip_id"] or session_id in (d["lineage_ids"] or []):
                return d
            if self._q1("SELECT 1 FROM veto_ids WHERE session_id=? AND job_id=?", (session_id, d["job_id"])):
                return d
        return None

    def source_rows(self, *, states: Sequence[str] | None = None) -> list[dict]:
        rows = [self._source(r) for r in self._q("SELECT * FROM source_handovers ORDER BY at DESC")]
        return [r for r in rows if states is None or r["state"] in states]

    def veto_add(self, job_id: str, session_ids: Sequence[str], root_id: str) -> None:
        with self._w() as c:
            for sid in session_ids:
                c.execute(
                    "INSERT OR IGNORE INTO veto_ids(session_id, root_id, job_id) VALUES(?,?,?)", (sid, root_id, job_id)
                )

    def veto_remove(self, job_id: str) -> None:
        """Inverse of p1 (release/reclaim): delete the job's veto rows."""
        with self._w() as c:
            c.execute("DELETE FROM veto_ids WHERE job_id=?", (job_id,))

    def veto_rows(self) -> list[dict]:
        """Every veto row joined with its handover (VetoIndex filters by state; X08, W034): ``session_id,
        root_id, job_id`` (and the older name ``job``), ``state, peer, title, at, prepared_at, ack`` (decoded like
        ``source_state``), ``bundle_path, updated_at`` — the same values ``source_by_session`` returns, so the
        veto text a hook shows equals the text ``reclaim`` looks for."""
        out = []
        for r in self._q(
            "SELECT v.session_id, v.root_id, s.job_id, s.job_id AS job, s.state, s.peer, s.title, s.at,"
            " s.prepared_at, s.ack, s.bundle_path, s.updated_at"
            " FROM veto_ids v JOIN source_handovers s ON s.job_id = v.job_id ORDER BY v.session_id"
        ):
            d = dict(r)
            d["ack"] = _loads(d["ack"])
            out.append(d)
        return out

    def backup_put(self, job_id: str, key: str, value: str | None) -> None:
        with self._w() as c:
            c.execute(
                "INSERT INTO backups(job_id, key, value) VALUES(?,?,?) ON CONFLICT(job_id, key) DO NOTHING",
                (job_id, key, value),
            )

    def backups(self, job_id: str) -> list[tuple[str, str | None]]:
        return [(r[0], r[1]) for r in self._q("SELECT key, value FROM backups WHERE job_id=? ORDER BY key", (job_id,))]

    # -- arrivals / notes / bundles ----------------------------------------------------------------------------
    @staticmethod
    def _arrival(row: sqlite3.Row) -> dict:
        d = dict(row)
        d["path_map"] = _loads(d["path_map"])
        return d

    def arrival_put(
        self,
        *,
        session_ids: Sequence[str],
        root_id: str,
        job_id: str,
        peer: str | None,
        note: str,
        report_path: str,
        path_map: Sequence[PathMapEntry],
        continue_prompt: str | None,
    ) -> None:
        pm = _dumps([to_wire(e) if not isinstance(e, dict) else e for e in path_map])
        with self._w() as c:
            for sid in session_ids:
                c.execute(
                    "INSERT INTO arrivals(session_id, root_id, job_id, peer, note, report_path, path_map,"
                    " continue_prompt, at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET"
                    " root_id=excluded.root_id, job_id=excluded.job_id, peer=excluded.peer, note=excluded.note,"
                    " report_path=excluded.report_path, path_map=excluded.path_map,"
                    " continue_prompt=excluded.continue_prompt",
                    (sid, root_id, job_id, peer, note, report_path, pm, continue_prompt, self.now()),
                )

    def arrival_for(self, session_id: str) -> dict | None:
        row = self._q1("SELECT * FROM arrivals WHERE session_id=?", (session_id,))
        return None if row is None else self._arrival(row)

    def arrival_rows(self) -> list[dict]:
        return [self._arrival(r) for r in self._q("SELECT * FROM arrivals ORDER BY at")]

    def arrival_delete(self, job_id: str) -> None:
        """Inverse of commit step c5."""
        with self._w() as c:
            c.execute("DELETE FROM arrivals WHERE job_id=?", (job_id,))

    def arrival_source_reclaimed(self, job_id: str) -> None:
        with self._w() as c:
            c.execute(
                "UPDATE arrivals SET source_reclaimed_at=? WHERE job_id=? AND source_reclaimed_at IS NULL",
                (self.now(), job_id),
            )

    def note_offer(self, session_id: str, turn_id: str) -> None:
        with self._w() as c:
            c.execute(
                "UPDATE arrivals SET note_state='offered', offered_turn=?, offers=offers+1 WHERE session_id=?"
                " AND note_state IN ('pending','offered')",
                (turn_id, session_id),
            )

    def note_delivered(self, session_id: str) -> None:
        with self._w() as c:
            c.execute("UPDATE arrivals SET note_state='delivered' WHERE session_id=?", (session_id,))

    def bundle_import_seen(self, bundle_job_id: str) -> str | None:
        row = self._q1("SELECT session_id FROM bundle_imports WHERE bundle_job_id=?", (bundle_job_id,))
        return None if row is None else str(row[0])

    def bundle_import_put(self, bundle_job_id: str, session_id: str) -> None:
        with self._w() as c:
            c.execute(
                "INSERT INTO bundle_imports(bundle_job_id, session_id, at) VALUES(?,?,?) ON CONFLICT(bundle_job_id)"
                " DO UPDATE SET session_id=excluded.session_id, at=excluded.at",
                (bundle_job_id, session_id, self.now()),
            )

    # -- workspace map -----------------------------------------------------------------------------------------
    @staticmethod
    def _map(row: sqlite3.Row) -> MapRow:
        return MapRow(
            local_path=row["local_path"],
            peer_id=row["peer_id"],
            remote_path=row["remote_path"],
            direction=row["direction"],
            repo_identity=row["repo_identity"],
            state=_loads(row["state"]),  # type: ignore[arg-type]
            last_job_id=row["last_job_id"],
        )

    def map_put(self, row: MapRow) -> None:
        with self._w() as c:
            c.execute(
                "INSERT INTO workspace_map(local_path, peer_id, remote_path, direction, repo_identity, state,"
                " last_job_id, at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(local_path, peer_id, remote_path, direction)"
                " DO UPDATE SET repo_identity=excluded.repo_identity, state=excluded.state,"
                " last_job_id=excluded.last_job_id, at=excluded.at",
                (
                    row.local_path,
                    row.peer_id,
                    row.remote_path,
                    row.direction,
                    row.repo_identity,
                    None if row.state is None else _dumps(row.state),
                    row.last_job_id,
                    self.now(),
                ),
            )

    def map_lookup(self, peer_id: str, remote_path: str, direction: str | None = None) -> MapRow | None:
        """Most recent row that maps ``remote_path`` on ``peer_id`` to a local path (``out`` before ``in``); with
        ``direction`` only rows of that direction (X10: the target binds the source peer,
        ``lookup = lambda path, direction: ledger.map_lookup(peer_id, path, direction)``)."""
        if direction is not None:
            return self.map_find(peer_id, remote_path, direction)
        row = self._q1(
            "SELECT * FROM workspace_map WHERE peer_id=? AND remote_path=? ORDER BY direction='out' DESC, at DESC",
            (peer_id, remote_path),
        )
        return None if row is None else self._map(row)

    def map_out(self, local_path: str, peer_id: str) -> MapRow | None:
        row = self._q1(
            "SELECT * FROM workspace_map WHERE local_path=? AND peer_id=? AND direction='out' ORDER BY at DESC",
            (local_path, peer_id),
        )
        return None if row is None else self._map(row)

    def map_in(self, local_path: str, peer_id: str) -> MapRow | None:
        """This host's ``in`` row for a destination it received into from ``peer_id`` (tree fingerprint, §9.6)."""
        row = self._q1(
            "SELECT * FROM workspace_map WHERE local_path=? AND peer_id=? AND direction='in' ORDER BY at DESC",
            (local_path, peer_id),
        )
        return None if row is None else self._map(row)

    def map_find(self, peer_id: str, remote_path: str, direction: str) -> MapRow | None:
        """The row of ``direction`` that maps ``remote_path`` on ``peer_id`` to a local path (``pathmap`` lookup,
        DESIGN §9.7 rule 3: ``out`` = handed away from here, ``in`` = received here)."""
        row = self._q1(
            "SELECT * FROM workspace_map WHERE peer_id=? AND remote_path=? AND direction=? ORDER BY at DESC",
            (peer_id, remote_path, direction),
        )
        return None if row is None else self._map(row)

    def map_by_local(self, local_path: str) -> list[MapRow]:
        return [
            self._map(r) for r in self._q("SELECT * FROM workspace_map WHERE local_path=? ORDER BY at", (local_path,))
        ]

    # -- objects / blocks ----------------------------------------------------------------------------------------
    def object_put(self, m: ObjectManifest) -> None:
        with self._w() as c:
            c.execute(
                "INSERT OR IGNORE INTO objects(object_id, sha256, size, media, at) VALUES(?,?,?,?,?)",
                (m.object_id, m.sha256, int(m.size), m.media, self.now()),
            )

    def object_get(self, object_id: str) -> ObjectManifest | None:
        """The index row of an object (``blocks`` empty: the full manifest lives in the BlockStore)."""
        row = self._q1("SELECT * FROM objects WHERE object_id=?", (object_id,))
        if row is None:
            return None
        return ObjectManifest(
            sha256=row["sha256"],
            size=int(row["size"]),
            block_size=0,
            blocks=(),
            media=row["media"] or "",
            object_id=row["object_id"],
        )

    def object_delete(self, ids: Sequence[str]) -> None:
        """Forget the ``objects`` index rows of collected objects (R28 manifest GC, ``BlockStore.collect_manifests``).
        Runs inside an open ``tx()`` of the calling thread when there is one (the GC removes the manifest files and
        the rows in one write transaction); ``job_objects`` references are left to ``purge_job``."""
        with self._w() as c:
            c.executemany("DELETE FROM objects WHERE object_id=?", [(i,) for i in ids])

    def block_put(self, block_id: str, size: int) -> None:
        with self._w() as c:
            c.execute(
                "INSERT INTO blocks(block_id, size, last_used_at) VALUES(?,?,?) ON CONFLICT(block_id) DO UPDATE SET"
                " last_used_at=excluded.last_used_at",
                (block_id, int(size), self.now()),
            )

    def block_touch(self, ids: Sequence[str]) -> None:
        now = self.now()
        with self._w() as c:
            c.executemany("UPDATE blocks SET last_used_at=? WHERE block_id=?", [(now, i) for i in ids])

    def block_delete(self, ids: Sequence[str]) -> None:
        with self._w() as c:
            c.executemany("DELETE FROM blocks WHERE block_id=?", [(i,) for i in ids])

    blocks_delete = block_delete  # name used by blocks.BlockStore (cluster C)

    def job_object(self, job_id: str, object_id: str) -> None:
        with self._w() as c:
            c.execute("INSERT OR IGNORE INTO job_objects(job_id, object_id) VALUES(?,?)", (job_id, object_id))

    def job_objects_add(self, job_id: str, object_ids: Sequence[str]) -> None:
        """Reference many objects from a job in one transaction (a pack can hold 200 000 file objects)."""
        with self._w() as c:
            c.executemany(
                "INSERT OR IGNORE INTO job_objects(job_id, object_id) VALUES(?,?)", [(job_id, o) for o in object_ids]
            )

    def job_objects(self, job_id: str) -> set[str]:
        """Object ids referenced by a job (X13: what a pull peer may read of it, R18; what ``release_job`` frees)."""
        return {r[0] for r in self._q("SELECT object_id FROM job_objects WHERE job_id=?", (job_id,))}

    def live_objects(self) -> set[str]:
        """Objects referenced by a job that is not terminal in some role on this host."""
        live: set[str] = set()
        rows = self._q("SELECT jo.object_id, j.role, j.state FROM job_objects jo JOIN jobs j ON j.job_id = jo.job_id")
        for r in rows:
            if r["state"] not in TERMINAL[Role(r["role"])]:
                live.add(r["object_id"])
        return live

    def gc_candidates(self, older_than: float, max_bytes: int) -> list[str]:
        """Blocks in LRU order that are older than ``older_than`` or exceed the cache cap (liveness is the
        caller's: ``live_objects`` + manifests)."""
        rows = self._q("SELECT block_id, size, last_used_at FROM blocks ORDER BY last_used_at, block_id")
        total = sum(int(r["size"]) for r in rows)
        out = []
        for r in rows:
            if r["last_used_at"] < older_than or total > max_bytes:
                out.append(r["block_id"])
                total -= int(r["size"])
        return out

    # -- suggestions, offers, ctx outbox, pull log ---------------------------------------------------------------
    def suggestion_put(self, session_id: str, data: dict) -> None:
        with self._w() as c:
            c.execute(
                "INSERT INTO suggestions(session_id, at, data) VALUES(?,?,?)", (session_id, self.now(), _dumps(data))
            )

    def suggestions(self, session_id: str, now: float) -> list[dict]:
        out = []
        for r in self._q(
            "SELECT at, data FROM suggestions WHERE session_id=? AND at>=? ORDER BY at",
            (session_id, float(now) - SUGGESTION_TTL_S),
        ):
            d = dict(_loads(r["data"]) or {})  # type: ignore[arg-type]
            d.setdefault("at", r["at"])
            out.append(d)
        return out

    def offer_put(
        self,
        offer_id: str,
        peer_id: str,
        session_id: str,
        note: str | None,
        refinement: dict | None,
        expires_at: float,
    ) -> None:
        with self._w() as c:
            c.execute(
                "INSERT INTO offers(offer_id, peer_id, session_id, note, refinement, created_at, expires_at)"
                " VALUES(?,?,?,?,?,?,?)",
                (
                    offer_id,
                    peer_id,
                    session_id,
                    note,
                    None if refinement is None else _dumps(refinement),
                    self.now(),
                    float(expires_at),
                ),
            )

    @staticmethod
    def _offer(row: sqlite3.Row) -> dict:
        d = dict(row)
        d["refinement"] = _loads(d["refinement"])
        return d

    def offers_for(self, peer_id: str, now: float) -> list[dict]:
        return [
            self._offer(r)
            for r in self._q(
                "SELECT * FROM offers WHERE peer_id=? AND expires_at>? AND claimed_job IS NULL ORDER BY created_at",
                (peer_id, float(now)),
            )
        ]

    def offer_get(self, offer_id: str) -> dict | None:
        row = self._q1("SELECT * FROM offers WHERE offer_id=?", (offer_id,))
        return None if row is None else self._offer(row)

    def offer_claim(self, offer_id: str, peer_id: str, job_id: str) -> dict:
        """Claim an offer for ``job_id`` (only by its peer, unexpired; idempotent for the same job)."""
        with self._w() as c:
            row = c.execute("SELECT * FROM offers WHERE offer_id=? AND peer_id=?", (offer_id, peer_id)).fetchone()
            if row is None or (row["claimed_job"] not in (None, job_id)):
                raise NotFound(f"offer {offer_id} not found")
            if row["claimed_job"] is None:
                if row["expires_at"] <= self.now():
                    raise NotFound(f"offer {offer_id} expired")
                c.execute("UPDATE offers SET claimed_job=? WHERE offer_id=?", (job_id, offer_id))
            row = c.execute("SELECT * FROM offers WHERE offer_id=?", (offer_id,)).fetchone()
            return self._offer(row)

    def outbox_put(self, kind: str, payload: dict) -> None:
        now = self.now()
        with self._w() as c:
            c.execute(
                "INSERT INTO ctx_outbox(kind, payload, created_at, next_try_at) VALUES(?,?,?,?)",
                (kind, _dumps(payload), now, now),
            )

    def outbox_due(self, now: float, limit: int = 20) -> list[dict]:
        out = []
        for r in self._q(
            "SELECT * FROM ctx_outbox WHERE done_at IS NULL AND next_try_at<=? ORDER BY id LIMIT ?",
            (float(now), int(limit)),
        ):
            d = dict(r)
            d["payload"] = _loads(d["payload"])
            out.append(d)
        return out

    def outbox_done(self, row_id: int, ok: bool, now: float) -> None:
        with self._w() as c:
            if ok:
                c.execute("UPDATE ctx_outbox SET done_at=? WHERE id=?", (float(now), int(row_id)))
                return
            row = c.execute("SELECT attempts FROM ctx_outbox WHERE id=?", (int(row_id),)).fetchone()
            attempts = (int(row[0]) if row else 0) + 1
            delay = min(30.0 * (2 ** min(attempts, 10)), 6 * 3600.0)
            c.execute(
                "UPDATE ctx_outbox SET attempts=?, next_try_at=? WHERE id=?",
                (attempts, float(now) + delay, int(row_id)),
            )

    # -- staging quota per job (R28, DESIGN §4.7: need_bytes from negotiation + 10 %) -------------------------
    def quota_set(self, job_id: str, role: Role, quota_bytes: int) -> None:
        """Set (or raise) the receiver-side staging quota of a job; already charged bytes are kept."""
        with self._w() as c:
            c.execute(
                "INSERT INTO job_quota(job_id, role, quota, used) VALUES(?,?,?,0) ON CONFLICT(job_id, role)"
                " DO UPDATE SET quota=MAX(job_quota.quota, excluded.quota)",
                (job_id, str(role), max(0, int(quota_bytes))),
            )

    def quota_get(self, job_id: str, role: Role) -> tuple[int, int] | None:
        """``(quota, used)`` of a job, or None when no plan was confirmed for it on this side."""
        row = self._q1("SELECT quota, used FROM job_quota WHERE job_id=? AND role=?", (job_id, str(role)))
        return None if row is None else (int(row[0]), int(row[1]))

    def quota_charge(self, job_id: str, role: Role, nbytes: int) -> tuple[bool, int, int]:
        """Atomically charge ``nbytes`` newly stored bytes: ``(ok, used, quota)``. Without a quota row or above
        the quota nothing is charged and ``ok`` is False (a peer can never store more than the confirmed plan)."""
        with self._w() as c:
            row = c.execute("SELECT quota, used FROM job_quota WHERE job_id=? AND role=?", (job_id, str(role)))
            row = row.fetchone()
            if row is None:
                return False, 0, -1
            quota, used = int(row[0]), int(row[1])
            if used + max(0, int(nbytes)) > quota:
                return False, used, quota
            c.execute(
                "UPDATE job_quota SET used=used+? WHERE job_id=? AND role=?", (max(0, int(nbytes)), job_id, str(role))
            )
            return True, used + max(0, int(nbytes)), quota

    def pull_log_put(self, job_id: str, peer_id: str, items: Sequence[str]) -> None:
        """ "pulled by <peer>: <items>" for the source's ``/handover status`` (DESIGN §8.7, SEC 5.8)."""
        with self._w() as c:
            c.execute(
                "INSERT INTO pull_log(job_id, peer_id, at, items) VALUES(?,?,?,?)",
                (job_id, peer_id, self.now(), _dumps(list(items))),
            )

    def pull_log(self, limit: int = 20) -> list[dict]:
        out = []
        for r in self._q("SELECT * FROM pull_log ORDER BY at DESC LIMIT ?", (int(limit),)):
            d = dict(r)
            d["items"] = _loads(d["items"])
            out.append(d)
        return out

    def purge_job(self, job_id: str) -> None:
        """``gc --job``: journal, ops, object references and the job rows of a terminal job (SEC 6.3)."""
        with self._w() as c:
            for r in c.execute("SELECT role, state FROM jobs WHERE job_id=?", (job_id,)).fetchall():
                if r["state"] not in TERMINAL[Role(r["role"])]:
                    raise InvalidState(f"job {job_id} is still {r['state']}", reason="active", state=r["state"])
            for table in ("job_events", "ops", "job_objects", "pull_log", "job_quota", "jobs"):
                c.execute(f"DELETE FROM {table} WHERE job_id=?", (job_id,))  # noqa: S608
