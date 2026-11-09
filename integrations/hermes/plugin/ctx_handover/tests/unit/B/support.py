"""Test support of cluster B: a ledger stand-in and an in-memory block store.

``LedgerStub`` implements exactly the ``ledger.Ledger`` methods cluster B calls (INTERFACES §4.5) on the
schema of INTERFACES §7 (the tables B touches), in a real SQLite file — WAL, ``BEGIN IMMEDIATE``, a
thread-local connection per instance. It also implements the two contract points B needs from cluster A
(listed in report B under "Wünsche an andere Cluster"):

* ``tx()`` is re-entrant per thread: ledger methods called while the same thread holds ``tx()`` run inside
  that transaction (``lock`` serialises retire/reclaim sub-steps this way, SK M4);
* ``arrival_rows()`` lists arrival rows for the in-memory note index (DESIGN §11.4).

``MemoryBlockStore`` implements ``BlockSource`` + ``BlockSink`` (no ``put_bytes``), so B's objects go through the
bare protocol path. Both live in tests only; the real ledger and block store belong to clusters A and C.
"""

from __future__ import annotations

import contextlib
import json
import sqlite3
import threading
import time
from collections.abc import Iterable, Iterator, Sequence
from pathlib import Path

from ctx_handover.engine.errors import AlreadyHandingOver, NotFound
from ctx_handover.engine.model import MapRow, ObjectManifest, SessionInfo, SourceState

ACTIVE_STATES = ("locking", "locked", "preparing", "prepared", "retiring", "retired", "exported")

_SCHEMA = """
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS jobs(job_id TEXT PRIMARY KEY, cancel_requested INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS source_handovers(job_id TEXT PRIMARY KEY, root_id TEXT NOT NULL, tip_id TEXT NOT NULL,
  lineage_ids TEXT NOT NULL, state TEXT NOT NULL, holder TEXT, peer TEXT, title TEXT, prepared_at REAL, ack TEXT,
  bundle_path TEXT, at REAL NOT NULL, updated_at REAL NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_handover ON source_handovers(root_id)
  WHERE state IN ('locking','locked','preparing','prepared','retiring','retired','exported');
CREATE TABLE IF NOT EXISTS veto_ids(session_id TEXT NOT NULL, root_id TEXT NOT NULL,
  job_id TEXT NOT NULL REFERENCES source_handovers(job_id), PRIMARY KEY(session_id, job_id));
CREATE TABLE IF NOT EXISTS backups(job_id TEXT NOT NULL, key TEXT NOT NULL, value TEXT, PRIMARY KEY(job_id, key));
CREATE TABLE IF NOT EXISTS arrivals(session_id TEXT PRIMARY KEY, root_id TEXT NOT NULL, job_id TEXT NOT NULL, peer TEXT,
  note TEXT NOT NULL, report_path TEXT, path_map TEXT NOT NULL, continue_prompt TEXT,
  note_state TEXT NOT NULL DEFAULT 'pending', offered_turn TEXT, offers INTEGER NOT NULL DEFAULT 0, at REAL NOT NULL,
  source_reclaimed_at REAL);
CREATE TABLE IF NOT EXISTS workspace_map(local_path TEXT NOT NULL, peer_id TEXT NOT NULL, remote_path TEXT NOT NULL,
  direction TEXT NOT NULL, repo_identity TEXT, state TEXT, last_job_id TEXT NOT NULL, at REAL NOT NULL,
  PRIMARY KEY(local_path, peer_id, remote_path, direction));
"""

_SOURCE_FIELDS = frozenset({"holder", "peer", "title", "prepared_at", "ack", "bundle_path", "tip_id", "lineage_ids"})


class LedgerStub:
    """Ledger stand-in (see module docstring). ``calls`` records method names in call order."""

    def __init__(self, path: Path):
        self.path = Path(path)
        self._local = threading.local()
        self.calls: list[str] = []
        self.fail_reads = False
        with self._raw() as conn:
            conn.executescript(_SCHEMA)

    # -- connection and transactions ---------------------------------------------------------------------
    def _conn(self) -> sqlite3.Connection:
        conn = getattr(self._local, "conn", None)
        if conn is None:
            conn = sqlite3.connect(self.path, timeout=5.0, isolation_level=None, check_same_thread=True)
            conn.row_factory = sqlite3.Row
            conn.execute("PRAGMA journal_mode=WAL")
            conn.execute("PRAGMA busy_timeout=5000")
            conn.execute("PRAGMA foreign_keys=ON")
            self._local.conn = conn
            self._local.depth = 0
        return conn

    @contextlib.contextmanager
    def _raw(self) -> Iterator[sqlite3.Connection]:
        conn = sqlite3.connect(self.path, timeout=5.0, isolation_level=None)
        try:
            conn.execute("PRAGMA journal_mode=WAL")
            yield conn
        finally:
            conn.close()

    @contextlib.contextmanager
    def tx(self) -> Iterator[sqlite3.Connection]:
        """``BEGIN IMMEDIATE`` on this thread's connection; nested calls join the open transaction."""
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
            conn.execute("ROLLBACK")
            raise
        self._local.depth = 0
        conn.execute("COMMIT")

    def _read(self, sql: str, params: Sequence = ()) -> list[sqlite3.Row]:
        if self.fail_reads:
            raise sqlite3.OperationalError("disk I/O error (injected)")
        return self._conn().execute(sql, params).fetchall()

    def close(self) -> None:
        conn = getattr(self._local, "conn", None)
        if conn is not None:
            conn.close()
            self._local.conn = None

    def data_version(self) -> int:
        if self.fail_reads:
            raise sqlite3.OperationalError("disk I/O error (injected)")
        return int(self._conn().execute("PRAGMA data_version").fetchone()[0])

    # -- meta / jobs -----------------------------------------------------------------------------------------
    def meta_get(self, key: str) -> str | None:
        rows = self._read("SELECT value FROM meta WHERE key=?", (key,))
        return rows[0][0] if rows else None

    def meta_set(self, key: str, value: str, conn=None) -> None:
        with self.tx() as c:
            c.execute(
                "INSERT INTO meta(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                (key, value),
            )

    def set_cancel(self, job_id: str) -> None:
        self.calls.append("set_cancel")
        with self.tx() as c:
            c.execute(
                "INSERT INTO jobs(job_id, cancel_requested) VALUES(?,1) "
                "ON CONFLICT(job_id) DO UPDATE SET cancel_requested=1",
                (job_id,),
            )

    def cancel_requested(self, job_id: str) -> bool:
        rows = self._read("SELECT cancel_requested FROM jobs WHERE job_id=?", (job_id,))
        return bool(rows and rows[0][0])

    # -- source handovers --------------------------------------------------------------------------------------
    def source_lock(self, job_id: str, info: SessionInfo, holder: str, peer: str | None) -> None:
        self.calls.append("source_lock")
        now = time.time()
        try:
            with self.tx() as c:
                c.execute(
                    "INSERT INTO source_handovers(job_id, root_id, tip_id, lineage_ids, state, holder, peer, title, at,"
                    " updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
                    (
                        job_id,
                        info.root_id,
                        info.tip_id,
                        json.dumps(list(info.lineage_ids)),
                        "locking",
                        holder,
                        peer,
                        info.title,
                        now,
                        now,
                    ),
                )
        except sqlite3.IntegrityError as exc:
            raise AlreadyHandingOver("this session is already being handed over", root_id=info.root_id) from exc

    def source_state(self, job_id: str) -> dict | None:
        rows = self._read("SELECT * FROM source_handovers WHERE job_id=?", (job_id,))
        return _source_row(rows[0]) if rows else None

    def source_set(self, job_id: str, state: SourceState, **fields) -> None:
        self.calls.append(f"source_set:{SourceState(state).value}")
        unknown = set(fields) - _SOURCE_FIELDS
        if unknown:
            raise TypeError(f"unknown source fields {sorted(unknown)}")
        sets = ["state=?", "updated_at=?"]
        params: list = [SourceState(state).value, time.time()]
        for key, value in fields.items():
            if key in ("ack", "lineage_ids") and value is not None and not isinstance(value, str):
                value = json.dumps(value, sort_keys=True)
            sets.append(f"{key}=?")
            params.append(value)
        with self.tx() as c:
            cur = c.execute(f"UPDATE source_handovers SET {', '.join(sets)} WHERE job_id=?", (*params, job_id))  # noqa: S608
            if cur.rowcount != 1:
                raise NotFound("no source handover", job_id=job_id)

    def source_by_session(self, session_id: str) -> dict | None:
        rows = self._read(
            "SELECT s.* FROM source_handovers s WHERE s.tip_id=? OR s.root_id=? OR EXISTS("
            " SELECT 1 FROM veto_ids v WHERE v.job_id=s.job_id AND v.session_id=?) OR EXISTS("
            " SELECT 1 FROM json_each(s.lineage_ids) j WHERE j.value=?) ORDER BY s.at DESC LIMIT 1",
            (session_id, session_id, session_id, session_id),
        )
        return _source_row(rows[0]) if rows else None

    # -- veto ----------------------------------------------------------------------------------------------
    def veto_add(self, job_id: str, session_ids: Sequence[str], root_id: str) -> None:
        self.calls.append("veto_add")
        with self.tx() as c:
            c.executemany(
                "INSERT OR IGNORE INTO veto_ids(session_id, root_id, job_id) VALUES(?,?,?)",
                [(sid, root_id, job_id) for sid in session_ids],
            )

    def veto_rows(self) -> list[dict]:
        rows = self._read(
            "SELECT v.session_id, v.root_id, v.job_id AS job, s.state, s.peer, s.title, s.at, s.prepared_at, s.ack,"
            " s.bundle_path, s.updated_at FROM veto_ids v JOIN source_handovers s ON s.job_id=v.job_id"
        )
        out = []
        for r in rows:
            d = dict(r)
            d["job_id"] = d["job"]
            if d.get("ack"):
                d["ack"] = json.loads(d["ack"])
            out.append(d)
        return out

    # -- backups -------------------------------------------------------------------------------------------
    def backup_put(self, job_id: str, key: str, value: str | None) -> None:
        self.calls.append(f"backup_put:{key}")
        with self.tx() as c:
            c.execute(
                "INSERT INTO backups(job_id, key, value) VALUES(?,?,?) "
                "ON CONFLICT(job_id, key) DO UPDATE SET value=excluded.value",
                (job_id, key, value),
            )

    def backups(self, job_id: str) -> list[tuple[str, str | None]]:
        return [
            (r[0], r[1]) for r in self._read("SELECT key, value FROM backups WHERE job_id=? ORDER BY key", (job_id,))
        ]

    # -- arrivals --------------------------------------------------------------------------------------------
    def arrival_put(
        self,
        *,
        session_ids: Sequence[str],
        root_id: str,
        job_id: str,
        peer: str | None,
        note: str,
        report_path: str,
        path_map: Sequence,
        continue_prompt: str | None,
    ) -> None:
        now = time.time()
        with self.tx() as c:
            c.executemany(
                "INSERT OR REPLACE INTO arrivals(session_id, root_id, job_id, peer, note, report_path, path_map,"
                " continue_prompt, at) VALUES(?,?,?,?,?,?,?,?,?)",
                [
                    (
                        sid,
                        root_id,
                        job_id,
                        peer,
                        note,
                        report_path,
                        json.dumps([list(map(str, p)) for p in path_map]),
                        continue_prompt,
                        now,
                    )
                    for sid in session_ids
                ],
            )

    def arrival_for(self, session_id: str) -> dict | None:
        rows = self._read("SELECT * FROM arrivals WHERE session_id=?", (session_id,))
        return dict(rows[0]) if rows else None

    def arrival_rows(self) -> list[dict]:
        return [dict(r) for r in self._read("SELECT * FROM arrivals WHERE note_state IN ('pending','offered')")]

    # -- workspace map -------------------------------------------------------------------------------------------
    def map_put(self, row: MapRow) -> None:
        self.calls.append("map_put")
        with self.tx() as c:
            c.execute(
                "INSERT OR REPLACE INTO workspace_map(local_path, peer_id, remote_path, direction, repo_identity,"
                " state, last_job_id, at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    row.local_path,
                    row.peer_id,
                    row.remote_path,
                    row.direction,
                    row.repo_identity,
                    json.dumps(row.state) if row.state is not None else None,
                    row.last_job_id,
                    time.time(),
                ),
            )

    def map_by_local(self, local_path: str) -> list[MapRow]:
        rows = self._read("SELECT * FROM workspace_map WHERE local_path=?", (local_path,))
        return [
            MapRow(
                r["local_path"],
                r["peer_id"],
                r["remote_path"],
                r["direction"],
                r["repo_identity"],
                json.loads(r["state"]) if r["state"] else None,
                r["last_job_id"],
            )
            for r in rows
        ]


def _source_row(row: sqlite3.Row) -> dict:
    d = dict(row)
    d["lineage_ids"] = json.loads(d["lineage_ids"])
    if d.get("ack"):
        d["ack"] = json.loads(d["ack"])
    return d


class MemoryBlockStore:
    """``BlockSource`` + ``BlockSink`` in memory (bare protocol: no ``put_bytes``). ``reads`` counts block reads."""

    def __init__(self) -> None:
        self.blocks: dict[str, bytes] = {}
        self.objects: dict[str, ObjectManifest] = {}
        self.reads: list[str] = []
        self.manifest_reads: list[str] = []

    def put_manifests(self, manifests: Sequence[ObjectManifest]) -> int:
        new = 0
        for m in manifests:
            if m.object_id not in self.objects:
                new += 1
            self.objects[m.object_id] = m
        return new

    def missing(self, ids: Sequence[str]) -> list[str]:
        return [i for i in ids if i not in self.blocks]

    def write_blocks(self, frames: Iterable[tuple[str, bytes]]) -> tuple[int, list[tuple[str, str]]]:
        n = 0
        for block_id, raw in frames:
            self.blocks[block_id] = bytes(raw)
            n += 1
        return n, []

    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        out = []
        for oid in object_ids:
            self.manifest_reads.append(oid)
            if oid not in self.objects:
                raise NotFound("unknown object", object_id=oid)
            out.append(self.objects[oid])
        return out

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        for block_id in ids:
            self.reads.append(block_id)
            if block_id in self.blocks:
                yield block_id, self.blocks[block_id]

    def tamper(self, object_id: str, mutate) -> None:
        """Replace the object's content with ``mutate(bytes)`` and re-address it under the SAME object id.

        Simulates a peer that ships a manipulated object whose manifest still claims the old id; the
        block and content hashes are recomputed so only the session-level hashes can notice.
        """
        from ctx_handover.engine.jsonutil import sha256_hex

        m = self.objects[object_id]
        data = b"".join(self.blocks[b] for b in m.blocks)
        new = mutate(data)
        bid = sha256_hex(new)
        self.blocks[bid] = new
        self.objects[object_id] = ObjectManifest(sha256_hex(new), len(new), m.block_size, (bid,), m.media, object_id)
