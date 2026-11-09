"""Pausing and locking the source session (layer L4, stdlib only; DESIGN §7, §11.4, INTERFACES §4.1).

Two layers of protection (R1):

* **Primary lock** — Hermes' own session state on the whole compression lineage (ended ``handed_over``,
  archived, hidden, title marked), written by ``prepare`` with public ``SessionDB`` methods. It holds when this
  plugin is disabled, not loaded or broken (DESIGN §1.4).
* **Veto** — ``VetoIndex`` answers the ``pre_llm_call``/``pre_tool_call`` hooks from memory: keyed by the
  lineage root, following compaction rotations, no I/O in the callback, never raising.

The turn lease and the compression lock (both public ``SessionDB`` leases) pause the session between
``acquire`` and ``retire``/``release``: a queued turn waits behind the lease and, once released, runs into the
veto (lab L6: the lease pauses, it does not refuse).

Ledger access goes only through the ``ledger.Ledger`` methods of INTERFACES §4.5. Every sub-step of
``prepare``/``retire``/``release``/``reclaim`` runs inside ``ledger.tx()`` (``BEGIN IMMEDIATE``) and re-checks
the source state there, so a retire and a reclaim of the same job serialise across processes [SK M4]. This needs
``tx()`` to be re-entrant per thread (ledger methods called by the thread that holds ``tx()`` join that
transaction) — a contract point for cluster A. Sub-steps are journaled as ``step:<p1…p7|r1…r5>`` backups;
``step:undone`` records that a release/reclaim restored a prepared session (that job never prepares again).
"""

from __future__ import annotations

import contextlib
import itertools
import json
import os
import queue
import re
import sqlite3
import sys
import threading
import time
from collections.abc import Callable, Iterator, Mapping, Sequence
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path

from .errors import Cancelled, InvalidState, NotFound, SessionBusy, WaitTimeout
from .ids import short
from .jsonutil import to_wire
from .model import CommitAck, MapRow, Options, SessionInfo, SourceState

PLUGIN_TAG = "ctx_handover"

VETO_STATES: frozenset[str] = frozenset(
    {SourceState.PREPARED.value, SourceState.RETIRING.value, SourceState.RETIRED.value, SourceState.EXPORTED.value}
)
"""Source states whose sessions the veto refuses (DESIGN §7.6); ``locked``/``preparing`` queue behind the lease."""

DRIVER_KEYS: tuple[str, ...] = ("goal", "heartbeat", "loop")
"""``state_meta`` drivers paused at prepare and cleared at retire (``<key>:<tip>``, DESIGN §6.3)."""

WITHHELD_PROMPT_KEY = "withheld_prompt"
"""``display_metadata`` key holding the text of a vetoed prompt (``agent/message_metadata.py:93``)."""

HANDED_OVER = "handed_over"
PAUSED_REASON = "handed over"

BUSY_MESSAGE = "A turn is running in this session. Wait for it, or use --wait to hand over as soon as it finishes."
COMPACTION_BUSY_MESSAGE = "A compaction of this session is running. Try again when it has finished."

PREPARE_STEPS = ("p1", "p2", "p3", "p4", "p5", "p6", "p7")
RETIRE_STEPS = ("r1", "r2", "r3", "r4", "r5")

_PIDNS_RE = re.compile(r"\[(\d+)\]")
_REWIND_ATTEMPTS = 20
_REWIND_PAUSE_S = 0.25


# ---------------------------------------------------------------------------------------------------------
# Holder and timing
# ---------------------------------------------------------------------------------------------------------


def _pidns_inode() -> str | None:
    """This process' PID-namespace inode (``/proc/self/ns/pid`` → ``pid:[<inode>]``), ``None`` off Linux."""
    if not sys.platform.startswith("linux"):
        return None
    try:
        link = os.readlink("/proc/self/ns/pid")
    except OSError:
        return None
    match = _PIDNS_RE.search(link)
    return match.group(1) if match else None


def holder_string(job_id: str) -> str:
    """Lease holder of this process for ``job_id``.

    On Linux the core's own stamp shape ``pid=<pid>:pidns=<inode>`` (``hermes_state_pidns.py:81-87``, rebuilt
    here because it is private): a dead holder is then reclaimed at once instead of after the TTL [LAB L6].
    """
    ns = _pidns_inode()
    stamp = f":pidns={ns}" if ns else ""
    return f"pid={os.getpid()}{stamp}:{PLUGIN_TAG}:job={job_id}"


def lease_timing(options: Options) -> tuple[int, int]:
    """``(ttl, refresh)`` seconds: stamped holders ``(lease_ttl_s, 30)``, unstamped ``(60, 15)`` (DESIGN §7.2)."""
    if _pidns_inode():
        ttl = max(30, int(options.lease_ttl_s))
        return ttl, min(30, max(5, ttl // 4))
    return 60, 15


# ---------------------------------------------------------------------------------------------------------
# Busy signals (read-only, best effort; schema drift ⇒ warning, never an abort [O8])
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class BusyReport:
    """What else is running for the session (DESIGN §7.2 step 5). ``busy`` ⇒ refuse unless ``--force``."""

    busy: bool
    reasons: tuple[str, ...]
    warnings: tuple[str, ...]
    processes: tuple[dict, ...]
    delegations: tuple[dict, ...]
    cron: tuple[dict, ...]


def _ro_connect(state_db: Path) -> sqlite3.Connection:
    uri = Path(state_db).resolve().as_uri() + "?mode=ro"
    conn = sqlite3.connect(uri, uri=True, timeout=2.0)
    conn.row_factory = sqlite3.Row
    return conn


def _in_list(ids: Sequence[str]) -> str:
    return ",".join("?" for _ in ids)


def probe_busy(db, state_db: Path, info: SessionInfo, hermes_home: Path, session_key: str | None) -> BusyReport:
    """Queued prompts and live delegations make the session busy; background processes and attached cron jobs
    are reported. Reads ``state.db`` over its own read-only connection; ``db`` is accepted for symmetry."""
    del db
    lineage = [str(s) for s in info.lineage_ids] or [info.tip_id]
    reasons: list[str] = []
    warnings: list[str] = []
    delegations: list[dict] = []
    try:
        conn = _ro_connect(state_db)
    except sqlite3.Error as exc:
        warnings.append(f"busy probe: state.db not readable ({exc})")
        conn = None
    if conn is not None:
        try:
            try:
                row = conn.execute(
                    f"SELECT COUNT(*) FROM session_admissions WHERE status='queued'"  # noqa: S608
                    f" AND target_session_id IN ({_in_list(lineage)})",
                    lineage,
                ).fetchone()
                if row and int(row[0]) > 0:
                    reasons.append(f"{int(row[0])} queued prompt(s) waiting for this session")
            except sqlite3.Error as exc:
                warnings.append(f"busy probe: session_admissions not readable ({exc})")
            try:
                rows = conn.execute(
                    "SELECT * FROM async_delegations WHERE state IN ('running','finalizing')"
                ).fetchall()
                for r in rows:
                    d = dict(r)
                    linked = {str(d.get(k) or "") for k in ("origin_session_id", "parent_session_id")}
                    if linked & set(lineage) or (session_key and d.get("origin_session") == session_key):
                        delegations.append(
                            {k: d.get(k) for k in ("delegation_id", "state", "dispatched_at", "parent_session_id")}
                        )
                if delegations:
                    reasons.append(f"{len(delegations)} delegation(s) still running")
            except sqlite3.Error as exc:
                warnings.append(f"busy probe: async_delegations not readable ({exc})")
        finally:
            conn.close()
    processes = _processes(Path(hermes_home), lineage, session_key, warnings)
    cron = _cron(Path(hermes_home), lineage, warnings)
    return BusyReport(
        busy=bool(reasons),
        reasons=tuple(reasons),
        warnings=tuple(warnings),
        processes=tuple(processes),
        delegations=tuple(delegations),
        cron=tuple(cron),
    )


def _read_json_file(path: Path, warnings: list[str], label: str) -> object:
    try:
        with open(path, encoding="utf-8-sig") as fh:
            return json.load(fh)
    except FileNotFoundError:
        return None
    except (OSError, ValueError) as exc:
        warnings.append(f"busy probe: {label} not readable ({exc.__class__.__name__})")
        return None


def _processes(home: Path, lineage: Sequence[str], session_key: str | None, warnings: list[str]) -> list[dict]:
    data = _read_json_file(home / "processes.json", warnings, "processes.json")
    entries = data.get("processes", data.get("sessions")) if isinstance(data, dict) else data
    if entries is None:
        return []
    if not isinstance(entries, list):
        warnings.append("busy probe: processes.json has an unknown shape")
        return []
    out = []
    for entry in entries:
        if not isinstance(entry, dict):
            continue
        key = str(entry.get("session_key") or "")
        if (session_key and key == session_key) or any(key.endswith(f":{sid}") for sid in lineage):
            out.append({k: entry.get(k) for k in ("pid", "command", "cwd", "started_at", "session_key")})
    return out


def _cron(home: Path, lineage: Sequence[str], warnings: list[str]) -> list[dict]:
    data = _read_json_file(home / "cron" / "jobs.json", warnings, "cron/jobs.json")
    jobs = data.get("jobs") if isinstance(data, dict) else data
    if jobs is None:
        return []
    if not isinstance(jobs, list):
        warnings.append("busy probe: cron/jobs.json has an unknown shape")
        return []
    out = []
    for job in jobs:
        if not isinstance(job, dict) or not job.get("attach_to_session"):
            continue
        origin = job.get("origin") if isinstance(job.get("origin"), dict) else {}
        chat = str(origin.get("chat_id") or job.get("session_id") or "")
        if chat in lineage:
            out.append(
                {
                    "id": job.get("id"),
                    "name": job.get("name"),
                    "schedule": job.get("schedule"),
                    "disable": f"hermes cron pause {job.get('id')}",
                }
            )
    return out


# ---------------------------------------------------------------------------------------------------------
# Ledger helpers
# ---------------------------------------------------------------------------------------------------------


def _tx(ledger):
    """The serialising ledger transaction (re-entrant per thread, contract point for cluster A)."""
    return ledger.tx()


def _entry(ledger, job_id: str) -> dict:
    entry = ledger.source_state(job_id)
    if entry is None:
        raise NotFound("no source handover for this job", job_id=job_id)
    return entry


def _state(ledger, job_id: str) -> str:
    return str(_entry(ledger, job_id).get("state"))


def _backups(ledger, job_id: str) -> dict[str, str | None]:
    return dict(ledger.backups(job_id))


def _done(ledger, job_id: str) -> set[str]:
    return {k[5:] for k, v in ledger.backups(job_id) if k.startswith("step:") and v == "done"}


def _mark(ledger, job_id: str, step: str) -> None:
    ledger.backup_put(job_id, f"step:{step}", "done")


def _backup_once(ledger, job_id: str, key: str, value: str | None, existing: Mapping[str, str | None]) -> None:
    if key not in existing:
        ledger.backup_put(job_id, key, value)


def _require(state: str, allowed: Sequence[str], action: str, job_id: str) -> None:
    if state not in allowed:
        raise InvalidState(f"cannot {action} in state {state}", state=state, job_id=job_id, reason=action)


def _now_ts(now: datetime | float | None) -> float:
    if isinstance(now, datetime):
        return now.timestamp()
    if isinstance(now, (int, float)):
        return float(now)
    return time.time()


def _release_leases(db, tip: str, holder: str | None) -> None:
    if not holder:
        return
    db.release_compression_lock(tip, holder)
    db.release_session_turn_lease(tip, holder)


# ---------------------------------------------------------------------------------------------------------
# Lock, refresh
# ---------------------------------------------------------------------------------------------------------


def acquire(
    db,
    ledger,
    info: SessionInfo,
    job_id: str,
    *,
    ttl_s: int,
    wait_s: int,
    cancelled: Callable[[], bool],
    sleep: Callable[[float], None] = time.sleep,
    clock: Callable[[], float] = time.monotonic,
    poll_s: float = 2.0,
) -> str:
    """Ledger row ``locking`` → turn lease → compression lock → ``locked``; returns the holder (DESIGN §7.2).

    A running turn ⇒ ``SessionBusy`` (``wait_s == 0``) or a retry every ``poll_s`` up to ``wait_s`` ⇒
    ``WaitTimeout``. A running compaction ⇒ ``SessionBusy``. Every failure leaves the ledger row ``released``.
    """
    holder = holder_string(job_id)
    ledger.source_lock(job_id, info, holder, None)
    try:
        deadline = clock() + max(0, int(wait_s))
        while True:
            if cancelled():
                raise Cancelled("handover cancelled while waiting for the session")
            try:
                got = db.try_acquire_session_turn_lease(info.tip_id, holder, ttl_seconds=ttl_s, patience_s=2.0)
            except sqlite3.OperationalError:
                got = False
            if got:
                break
            if wait_s <= 0:
                raise SessionBusy(BUSY_MESSAGE, reason="turn", session_id=info.tip_id)
            remaining = deadline - clock()
            if remaining <= 0:
                raise WaitTimeout(
                    "The session stayed busy; gave up waiting.", waited_s=int(wait_s), session_id=info.tip_id
                )
            sleep(min(poll_s, remaining))
        if not db.try_acquire_compression_lock(info.tip_id, holder, ttl_seconds=ttl_s):
            db.release_session_turn_lease(info.tip_id, holder)
            raise SessionBusy(COMPACTION_BUSY_MESSAGE, reason="compression", session_id=info.tip_id)
    except BaseException:
        with contextlib.suppress(Exception):
            ledger.source_set(job_id, SourceState.RELEASED)
        raise
    ledger.source_set(job_id, SourceState.LOCKED, holder=holder)
    return holder


def refresh(db, info: SessionInfo, holder: str, *, ttl_s: int) -> bool:
    """Extend both leases; ``False`` when either is lost or the store refused (caller counts ``lease_lost``)."""
    try:
        turn = bool(db.refresh_session_turn_lease(info.tip_id, holder, ttl_seconds=ttl_s, patience_s=5.0))
        comp = bool(db.refresh_compression_lock(info.tip_id, holder, ttl_seconds=ttl_s))
    except sqlite3.Error:
        return False
    return turn and comp


def suspend(db, ledger, info: SessionInfo | None, holder: str | None, job_id: str) -> None:
    """Idle participant (DESIGN §7.3): a source whose remote conductor went quiet gives up both leases and
    waits as ``suspended``; the caller keeps its export/pack caches. Only before prepare: in ``locking``/
    ``locked`` nothing but the leases was taken (no veto rows, no marks), so nothing else is undone and the
    session is usable at once. ``acquire`` of the same job locks again (the ledger re-locks ``suspended``).

    A no-op on ``suspended``; from ``preparing`` on ⇒ ``InvalidState`` (only release/reclaim undo the primary
    lock). Leases and state change under the ledger lock, so a prepare racing it stops on ``suspended``.
    """
    entry = _entry(ledger, job_id)
    holder = holder or entry.get("holder")
    tip = str(entry.get("tip_id") or (info.tip_id if info is not None else ""))
    with _tx(ledger):
        state = _state(ledger, job_id)
        if state == SourceState.SUSPENDED.value:
            return
        _require(state, (SourceState.LOCKING.value, SourceState.LOCKED.value), "suspend", job_id)
        _release_leases(db, tip, holder)
        ledger.source_set(job_id, SourceState.SUSPENDED)


# ---------------------------------------------------------------------------------------------------------
# Prepare (primary lock) and its inverse
# ---------------------------------------------------------------------------------------------------------


def _retitle(title: str, peer: str, now: datetime | float | None, max_len: int, suffix: str = "") -> str:
    when = (now if isinstance(now, datetime) else datetime.fromtimestamp(_now_ts(now))).strftime("%Y-%m-%d %H:%M")
    tail = f" (handed over to {peer} {when}{suffix})"
    keep = max(1, max_len - len(tail))
    base = title if len(title) <= keep else title[: keep - 1].rstrip() + "…"
    return (base + tail)[:max_len]


def _pause_driver(raw: str) -> str | None:
    try:
        data = json.loads(raw)
    except ValueError:
        return None
    if not isinstance(data, dict) or data.get("status") in ("paused", "done", "cleared"):
        return None
    data["status"] = "paused"
    data["paused_reason"] = PAUSED_REASON
    return json.dumps(data, ensure_ascii=False)


def _clear_driver(raw: str) -> str | None:
    try:
        data = json.loads(raw)
    except ValueError:
        return None
    if not isinstance(data, dict) or data.get("status") == "cleared":
        return None
    data["status"] = "cleared"
    return json.dumps(data, ensure_ascii=False)


def prepare(db, ledger, info: SessionInfo, job_id: str, peer_name: str, now: datetime) -> None:
    """Primary lock p1–p7 (DESIGN §7.4), journaled and idempotent per sub-step; leases are still held.

    p1 ``preparing`` + veto rows for every lineage id · p2 goal/heartbeat/loop paused (backed up) · p3 titles
    marked (backed up) · p4 ``end_session(…, "handed_over")`` on every segment · p5 archived lineage-wide
    (``notify=False``) · p6 hidden lineage-wide · p7 ``prepared``.
    """
    tip = info.tip_id
    lineage = tuple(info.lineage_ids) or (tip,)
    allowed = (SourceState.LOCKED.value, SourceState.PREPARING.value)
    if _state(ledger, job_id) == SourceState.PREPARED.value and set(PREPARE_STEPS) <= _done(ledger, job_id):
        return
    done = _done(ledger, job_id)
    if "undone" in done:
        # the journaled p-steps and backups belong to the restored attempt; replaying them would skip every mark
        raise InvalidState(
            "this handover was released or reclaimed; start a new one",
            state=_state(ledger, job_id),
            job_id=job_id,
            reason="undone",
        )
    max_len = int(getattr(db, "MAX_TITLE_LENGTH", 100) or 100)

    if "p1" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), allowed, "prepare", job_id)
            ledger.source_set(job_id, SourceState.PREPARING, peer=peer_name, prepared_at=_now_ts(now))
            ledger.veto_add(job_id, list(lineage), info.root_id)
            _mark(ledger, job_id, "p1")
    if "p2" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), allowed, "prepare", job_id)
            existing = _backups(ledger, job_id)
            for key in DRIVER_KEYS:
                meta_key = f"{key}:{tip}"
                raw = db.get_meta(meta_key)
                _backup_once(ledger, job_id, f"meta:{key}", raw, existing)
                if raw:
                    paused = _pause_driver(raw)
                    if paused is not None:
                        db.set_meta(meta_key, paused)
            _mark(ledger, job_id, "p2")
    if "p3" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), allowed, "prepare", job_id)
            existing = _backups(ledger, job_id)
            for seg in lineage:
                key = f"title:{seg}"
                if existing.get(key):
                    original = json.loads(existing[key]).get("title")
                else:
                    original = db.get_session_title(seg)
                    if not original:
                        continue
                    ledger.backup_put(
                        job_id, key, json.dumps({"title": original, "source": db.get_session_title_source(seg)})
                    )
                if not original:
                    continue
                for suffix in ("", f", job {short(job_id)}"):
                    marked = _retitle(original, peer_name, now, max_len, suffix)
                    if db.get_session_title(seg) == marked:
                        break
                    try:
                        db.set_session_title(seg, marked)
                        break
                    except ValueError:
                        continue
            _mark(ledger, job_id, "p3")
    if "p4" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), allowed, "prepare", job_id)
            existing = _backups(ledger, job_id)
            row = db.get_session(tip) or {}
            _backup_once(
                ledger,
                job_id,
                f"end:{tip}",
                json.dumps({"ended_at": row.get("ended_at"), "end_reason": row.get("end_reason")}),
                existing,
            )
            for seg in lineage:
                db.end_session(seg, HANDED_OVER)
            _mark(ledger, job_id, "p4")
    if "p5" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), allowed, "prepare", job_id)
            existing = _backups(ledger, job_id)
            row = db.get_session(tip) or {}
            _backup_once(
                ledger,
                job_id,
                f"flags:{tip}",
                json.dumps({"archived": int(row.get("archived") or 0), "hidden": int(row.get("hidden") or 0)}),
                existing,
            )
            db.set_session_archived(tip, True, notify=False)
            _mark(ledger, job_id, "p5")
    if "p6" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), allowed, "prepare", job_id)
            db.set_session_hidden(tip, True)
            _mark(ledger, job_id, "p6")
    with _tx(ledger):
        _require(_state(ledger, job_id), (*allowed, SourceState.PREPARED.value), "prepare", job_id)
        ledger.source_set(job_id, SourceState.PREPARED)
        _mark(ledger, job_id, "p7")


def _restore_marks(db, ledger, job_id: str, tip: str, lineage: Sequence[str], warnings: list[str]) -> None:
    """Inverse of p6…p2 from the backups (each restore sets the backed-up value; idempotent)."""
    backups = _backups(ledger, job_id)
    flags = json.loads(backups[f"flags:{tip}"]) if backups.get(f"flags:{tip}") else None
    if flags is not None:
        db.set_session_hidden(tip, bool(flags.get("hidden")))
        db.set_session_archived(tip, bool(flags.get("archived")), notify=False)
    end = json.loads(backups[f"end:{tip}"]) if backups.get(f"end:{tip}") else None
    if end is not None and end.get("ended_at") is None:
        row = db.get_session(tip) or {}
        if row.get("end_reason") == HANDED_OVER:
            db.reopen_session(tip)
    for seg in lineage:
        raw = backups.get(f"title:{seg}")
        if not raw:
            continue
        saved = json.loads(raw)
        title, source = saved.get("title"), saved.get("source")
        if not title:
            continue
        if db.get_session_title(seg) != title:
            try:
                db.set_session_title(seg, title)
            except ValueError as exc:
                warnings.append(f"title {title!r} could not be restored: {exc}")
                continue
        if source:
            with contextlib.suppress(ValueError):
                db.set_session_title_source(seg, source)
    for key in DRIVER_KEYS:
        backup_key = f"meta:{key}"
        if backups.get(backup_key):
            db.set_meta(f"{key}:{tip}", backups[backup_key])


# ---------------------------------------------------------------------------------------------------------
# Retire
# ---------------------------------------------------------------------------------------------------------


def retire(
    db, ledger, info: SessionInfo, holder: str | None, job_id: str, ack: CommitAck, out_rows: list[MapRow]
) -> None:
    """r1–r5 after the target's ACK (DESIGN §7.5): r1 ``retiring`` + ACK · r2 drivers ``cleared`` · r3 workspace
    map ``out`` rows · r4 compression lock, then turn lease released — only after r1–r3 committed, so a queued
    turn runs into the veto [LAB L6] · r5 ``retired``. Idempotent per sub-step; ``retired`` with open sub-steps
    continues them [SK M4]. Each sub-step re-checks the state under the ledger lock: a reclaim that got there
    first stops the retire with ``InvalidState``."""
    tip = info.tip_id
    entry = _entry(ledger, job_id)
    holder = holder or entry.get("holder")
    if entry.get("state") == SourceState.RETIRED.value and set(RETIRE_STEPS) <= _done(ledger, job_id):
        return
    going = (SourceState.RETIRING.value, SourceState.RETIRED.value)
    done = _done(ledger, job_id)
    if "r1" not in done:
        with _tx(ledger):
            state = _state(ledger, job_id)
            _require(state, (SourceState.PREPARED.value, *going), "retire", job_id)
            if state == SourceState.PREPARED.value:
                ledger.source_set(job_id, SourceState.RETIRING, ack=to_wire(ack))
            _mark(ledger, job_id, "r1")
    if "r2" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), going, "retire", job_id)
            for key in DRIVER_KEYS:
                meta_key = f"{key}:{tip}"
                raw = db.get_meta(meta_key)
                if raw:
                    cleared = _clear_driver(raw)
                    if cleared is not None:
                        db.set_meta(meta_key, cleared)
            _mark(ledger, job_id, "r2")
    if "r3" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), going, "retire", job_id)
            for row in out_rows:
                ledger.map_put(row)
            _mark(ledger, job_id, "r3")
    if "r4" not in done:
        with _tx(ledger):
            _require(_state(ledger, job_id), going, "retire", job_id)
            _release_leases(db, tip, holder)
            _mark(ledger, job_id, "r4")
    with _tx(ledger):
        state = _state(ledger, job_id)
        _require(state, going, "retire", job_id)
        if state == SourceState.RETIRING.value:
            ledger.source_set(job_id, SourceState.RETIRED)
        _mark(ledger, job_id, "r5")


def mark_exported(db, ledger, info: SessionInfo, holder: str, job_id: str, bundle_path: str) -> None:
    """After ``prepare``: state ``exported`` with the bundle path, then the leases are released (the veto
    answers from then on, DESIGN §7.6). Idempotent."""
    tip = info.tip_id
    entry = _entry(ledger, job_id)
    holder = holder or entry.get("holder")
    with _tx(ledger):
        state = _state(ledger, job_id)
        _require(state, (SourceState.PREPARED.value, SourceState.EXPORTED.value), "export", job_id)
        if state == SourceState.PREPARED.value:
            ledger.source_set(job_id, SourceState.EXPORTED, bundle_path=str(bundle_path))
        _mark(ledger, job_id, "x1")
    with _tx(ledger):
        _require(_state(ledger, job_id), (SourceState.EXPORTED.value,), "export", job_id)
        _release_leases(db, tip, holder)
        _mark(ledger, job_id, "x2")


# ---------------------------------------------------------------------------------------------------------
# Veto texts and placeholder clean-up
# ---------------------------------------------------------------------------------------------------------


def _fmt_time(ts: object) -> str:
    try:
        return time.strftime("%Y-%m-%d %H:%M UTC", time.gmtime(float(ts)))  # type: ignore[arg-type]
    except (TypeError, ValueError, OverflowError, OSError):
        return "an unknown time"


def _ack_dict(value: object) -> dict:
    if isinstance(value, Mapping):
        return dict(value)
    if isinstance(value, str) and value:
        try:
            parsed = json.loads(value)
        except ValueError:
            return {}
        return parsed if isinstance(parsed, dict) else {}
    return {}


def veto_texts(entry: Mapping) -> str:
    """The block message for a vetoed turn (DESIGN §7.6), deterministic from the ledger entry.

    retired/retiring → handed over to ``peer`` at ``ack.committed_at`` as ``ack.title``; exported → bundle at
    ``prepared_at``; otherwise (prepared) → a handover is being completed. Times are UTC.
    """
    state = str(entry.get("state") or "")
    peer = entry.get("peer") or "another machine"
    job = str(entry.get("job_id") or entry.get("job") or "")
    job_short = short(job)
    if state in (SourceState.RETIRING.value, SourceState.RETIRED.value):
        ack = _ack_dict(entry.get("ack"))
        when = _fmt_time(ack.get("committed_at") or entry.get("prepared_at"))
        title = ack.get("title")
        where = f' and continues there as "{title}"' if title else " and continues there"
        return (
            f"This session was handed over to {peer} on {when} (job {job_short}){where}. "
            "Your message was not sent. To continue here instead, run /handover reclaim."
        )
    if state == SourceState.EXPORTED.value:
        return (
            f"This session was exported to a handover bundle on {_fmt_time(entry.get('prepared_at'))}. "
            "Run /handover reclaim to continue here."
        )
    return (
        f"A handover of this session to {peer} is being completed (job {job_short}). Your message was not sent. "
        "/handover status shows progress; /handover reclaim --force takes the session back if it is stuck."
    )


def veto_entry(row: Mapping) -> dict:
    """A ledger row (``veto_rows`` or ``source_state``) as the veto entry the hooks answer with: ``job_id`` set,
    ``ack`` parsed, ``message`` = ``veto_texts`` of it. The one place a veto text is derived from ledger data:
    ``VetoIndex`` builds its entries here and reclaim looks for exactly these texts (X08)."""
    entry = {k: v for k, v in row.items() if k != "message"}
    entry["job_id"] = entry.get("job_id") or entry.get("job")
    entry["ack"] = _ack_dict(entry.get("ack")) or None
    entry["message"] = veto_texts(entry)
    return entry


def _all_texts(entry: Mapping) -> set[str]:
    base = {k: v for k, v in veto_entry(entry).items() if k != "message"}
    return {
        veto_texts({**base, "state": s})
        for s in (SourceState.PREPARED.value, SourceState.RETIRED.value, SourceState.EXPORTED.value)
    }


def _job_texts(ledger, job_id: str, entry: Mapping) -> set[str]:
    """Every veto text a hook may have shown for ``job_id``: built by ``veto_entry`` from the job's
    ``veto_rows`` — the rows ``VetoIndex`` answers from — in each vetoing state, plus the same from its source
    row. The texts name the job, so no other turn qualifies."""
    texts = _all_texts(entry)
    for row in ledger.veto_rows():
        if str(row.get("job_id") or row.get("job") or "") == job_id:
            texts |= _all_texts(row)
    return texts


def _withheld(row: Mapping) -> str | None:
    meta = row.get("display_metadata")
    if isinstance(meta, str):
        try:
            meta = json.loads(meta)
        except ValueError:
            meta = None
    if isinstance(meta, Mapping):
        value = meta.get(WITHHELD_PROMPT_KEY)
        return value if isinstance(value, str) else None
    return None


def _is_placeholder(row: Mapping) -> bool:
    """The core's withheld placeholder of a vetoed prompt: a ``user`` row with ``withheld_prompt`` in its display
    metadata (``agent/turn_context.py:862-893``)."""
    return row.get("role") == "user" and _withheld(row) is not None


def _is_veto_reply(row: Mapping, texts: set[str]) -> bool:
    return row.get("role") == "assistant" and row.get("content") in texts


def _veto_pairs(rows: Sequence[Mapping], texts: set[str]) -> list[Mapping]:
    """Placeholders of this job in ``rows`` (storage order, any state): a placeholder directly followed by a reply
    with one of this job's veto texts. The texts name the job, so no other turn qualifies."""
    seq = [r for r in rows if r.get("role") != "session_meta"]
    return [a for a, b in itertools.pairwise(seq) if _is_placeholder(a) and _is_veto_reply(b, texts)]


def _placeholder_run(rows: Sequence[Mapping], texts: set[str]) -> tuple[int | None, list[Mapping]]:
    """The first row id of the trailing run of veto pairs among the live ``rows`` of the tip, and the run's
    placeholders. A pair is a withheld placeholder that is not a compaction summary carrier, followed by a reply
    with a veto text of this job; anything else — a real turn, the compaction summary, a veto reply whose
    placeholder the compaction left behind — ends the run, so a rewind never takes more than vetoed attempts."""
    live = [r for r in rows if r.get("role") != "session_meta"]
    start = len(live)
    i = len(live) - 2
    while i >= 0:
        user, reply = live[i], live[i + 1]
        if _is_placeholder(user) and not user.get("_compressed_summary") and _is_veto_reply(reply, texts):
            start = i
            i -= 2
            continue
        break
    if start >= len(live):
        return None, []
    run = live[start:]
    return int(run[0]["id"]), list(run[::2])


def _copy_key(row: Mapping) -> object:
    """Copies of one message (carried tails, rotation clones) share ``message_uid``."""
    return row.get("message_uid") or ("id", row.get("id"))


def _rewind_placeholders(
    db, entry: Mapping, texts: set[str], warnings: list[str], sleep: Callable[[float], None]
) -> tuple[int, list[str], int]:
    """Clean up the vetoed attempts of this job (DESIGN §7.7). Returns (rows rewound, typed texts, kept).

    Scope: the session's segments from the handed-over tip to the current tip (a compaction after the retire
    rotates or compacts in place). Only the trailing run of veto pairs on the live tip is rewound
    (``rewind_to_message``, non-destructive). Vetoed attempts a compaction moved out of reach stay: the core
    refuses every write on a segment closed by compression (``hermes_state_messages.py:268-269``), a rewind never
    touches archived rows, and a veto reply carried right behind the compaction summary can only go together with
    the summary. Their typed texts are still returned (in transcript order, one per message across copies) and
    ``kept`` counts them; nothing is ever written to an ancestor segment. ``texts`` = ``_job_texts``.
    """
    origin = str(entry.get("tip_id"))
    tip = db.resolve_resume_session_id(str(entry.get("root_id"))) or origin
    lineage = list(db.get_compression_lineage(tip)) or [tip]
    segments = lineage[lineage.index(origin) :] if origin in lineage else [tip]
    shown: list[Mapping] = []
    tip_live: list[dict] = []
    for seg in segments:
        rows = list(_rows_after(db, seg, 0))
        shown.extend(r for r in _veto_pairs(rows, texts) if r.get("active", 1) or r.get("compacted"))
        if seg == tip:
            tip_live = [r for r in rows if r.get("active", 1)]
    target, run = _placeholder_run(tip_live, texts)
    rewound = 0
    if target is not None:
        for attempt in range(_REWIND_ATTEMPTS):
            try:
                rewound = int(db.rewind_to_message(tip, target).get("rewound_count") or 0)
                break
            except ValueError as exc:
                warnings.append(f"placeholder rows not rewound: {exc}")
                break
            except Exception as exc:
                if attempt + 1 == _REWIND_ATTEMPTS:
                    warnings.append(f"placeholder rows not rewound: {exc.__class__.__name__}: {exc}")
                    break
                sleep(_REWIND_PAUSE_S)
    gone = {_copy_key(r) for r in run} if rewound else set()
    withheld: list[str] = []
    kept = 0
    seen: set[object] = set()
    for row in shown:
        key = _copy_key(row)
        if key in seen:
            continue
        seen.add(key)
        withheld.append(str(_withheld(row)))
        if key not in gone:
            kept += 1
    if kept:
        warnings.append(
            f"{kept} vetoed message(s) stay in this session's history: a compaction after the handover moved them "
            "where Hermes allows no changes; their typed text is listed as withheld"
        )
    return rewound, withheld, kept


def _rows_after(db, session_id: str, after_id: int) -> Iterator[dict]:
    last = after_id
    while True:
        rows = db.get_messages(session_id, include_inactive=True, after_id=last, limit=500)
        if not rows:
            return
        yield from rows
        last = int(rows[-1]["id"])
        if len(rows) < 500:
            return


# ---------------------------------------------------------------------------------------------------------
# Release and reclaim
# ---------------------------------------------------------------------------------------------------------


_RELEASABLE = (
    SourceState.LOCKING.value,
    SourceState.LOCKED.value,
    SourceState.PREPARING.value,
    SourceState.PREPARED.value,
    SourceState.RELEASING.value,
    SourceState.SUSPENDED.value,
)


def _undo(
    db,
    ledger,
    job_id: str,
    entry: Mapping,
    holder: str | None,
    *,
    from_states: Sequence[str],
    middle: SourceState,
    final: SourceState,
    action: str,
    sleep: Callable[[float], None],
) -> dict:
    """Shared body of release and reclaim: leases → placeholder rewind (veto still active) → ``middle`` →
    restored marks → veto rows removed (inverse of p1, ``ledger.veto_remove``) with ``final``. Each ledger step
    is a CAS under the ledger lock."""
    warnings: list[str] = []
    tip = str(entry.get("tip_id"))
    lineage = list(entry.get("lineage_ids") or [tip])
    holder = holder or entry.get("holder")
    with _tx(ledger):
        _require(_state(ledger, job_id), (*from_states, middle.value), action, job_id)
        _release_leases(db, tip, holder)
    rewound, withheld, kept = 0, [], 0
    vetoed = "p1" in _done(ledger, job_id)
    if vetoed:
        current = _entry(ledger, job_id)
        texts = _job_texts(ledger, job_id, current)
        rewound, withheld, kept = _rewind_placeholders(db, current, texts, warnings, sleep)
    with _tx(ledger):
        state = _state(ledger, job_id)
        _require(state, (*from_states, middle.value), action, job_id)
        if state != middle.value:
            ledger.source_set(job_id, middle)
    with _tx(ledger):
        _require(_state(ledger, job_id), (middle.value,), action, job_id)
        _restore_marks(db, ledger, job_id, tip, lineage, warnings)
    with _tx(ledger):
        _require(_state(ledger, job_id), (middle.value, final.value), action, job_id)
        ledger.veto_remove(job_id)
        ledger.source_set(job_id, final)
        if vetoed:
            _mark(ledger, job_id, "undone")
    return {
        "state": final.value,
        "job_id": job_id,
        "warnings": warnings,
        "withheld": withheld,
        "rewound": rewound,
        "kept": kept,
    }


def release(
    db, ledger, info: SessionInfo, holder: str | None, job_id: str, *, sleep: Callable[[float], None] = time.sleep
) -> None:
    """Abort path before the commit: undo the primary lock (inverse of p6…p2, placeholder rewind) and free the
    leases; ``released`` afterwards (also from ``suspended``: a cancelled idle job). A no-op on
    ``released``/``reclaimed``."""
    del info
    entry = _entry(ledger, job_id)
    if entry.get("state") in (SourceState.RELEASED.value, SourceState.RECLAIMED.value):
        return
    _undo(
        db,
        ledger,
        job_id,
        entry,
        holder,
        from_states=_RELEASABLE,
        middle=SourceState.RELEASING,
        final=SourceState.RELEASED,
        action="release",
        sleep=sleep,
    )


def reclaim(db, ledger, session_id: str, *, force: bool, sleep: Callable[[float], None] = time.sleep) -> dict:
    """``/handover reclaim`` (DESIGN §7.7 table). Returns ``{"state", "job_id", "message", "warnings",
    "withheld", "rewound"}``; ``state`` is ``None`` when nothing was done (refused or nothing to reclaim)."""
    entry = ledger.source_by_session(session_id)
    if entry is None:
        return {"state": None, "job_id": None, "message": "Nothing to reclaim.", "warnings": [], "withheld": []}
    job_id = str(entry.get("job_id"))
    state = str(entry.get("state"))
    peer = entry.get("peer") or "the other machine"
    warnings: list[str] = []

    def refused(message: str) -> dict:
        return {"state": None, "job_id": job_id, "message": message, "warnings": [], "withheld": []}

    if state in (SourceState.RETIRED.value, SourceState.EXPORTED.value, SourceState.RECLAIMING.value):
        if state != SourceState.RECLAIMING.value:
            warnings.append(f"the copy on {peer} stays active; the two copies are now independent")
        from_states: tuple[str, ...] = (SourceState.RETIRED.value, SourceState.EXPORTED.value)
    elif state in (SourceState.PREPARED.value, SourceState.RETIRING.value):
        if not force:
            return refused("A handover is being committed. Wait, or use --force.")
        warnings.append(f"the target on {peer} may already be active; the two copies are now independent")
        from_states = (SourceState.PREPARED.value, SourceState.RETIRING.value, SourceState.RETIRED.value)
    elif state in (SourceState.LOCKING.value, SourceState.LOCKED.value, SourceState.PREPARING.value):
        if not force:
            return refused(f"Handover in progress; use /handover cancel {job_id}.")
        with contextlib.suppress(NotFound):  # pull: the conductor is the peer; it sees ``released`` on contact
            ledger.set_cancel(job_id)
        release(db, ledger, None, None, job_id, sleep=sleep)  # type: ignore[arg-type]
        return {
            "state": SourceState.RELEASED.value,
            "job_id": job_id,
            "message": "Handover cancelled; the session is yours again.",
            "warnings": warnings,
            "withheld": [],
        }
    elif state == SourceState.RELEASING.value:
        release(db, ledger, None, None, job_id, sleep=sleep)  # type: ignore[arg-type]
        return {
            "state": SourceState.RELEASED.value,
            "job_id": job_id,
            "message": "Released.",
            "warnings": [],
            "withheld": [],
        }
    else:
        return refused("Nothing to reclaim.")
    result = _undo(
        db,
        ledger,
        job_id,
        entry,
        None,
        from_states=from_states,
        middle=SourceState.RECLAIMING,
        final=SourceState.RECLAIMED,
        action="reclaim",
        sleep=sleep,
    )
    result["warnings"] = warnings + result["warnings"]
    result["message"] = "The session is active here again."
    return result


# ---------------------------------------------------------------------------------------------------------
# VetoIndex (no I/O in the callback, never raises)
# ---------------------------------------------------------------------------------------------------------


def _default_ledger_factory(path: Path):
    from .ledger import Ledger  # cluster A, layer L2 (lazy: the module lands in layer 2)

    return Ledger(path)


class VetoIndex:
    """In-memory answer to "is this session vetoed?" and "is a handover note waiting here?" (DESIGN §7.6, §11.4).

    A daemon thread rebuilds the maps from the ledger whenever ``PRAGMA data_version`` moves (poll ``poll_s``)
    and flushes queued ledger writes from ``on_compaction`` first. On a read error the last good maps stay.
    ``lookup``/``arrival`` are plain dict reads — no file, database or network access — and never raise.
    """

    def __init__(
        self,
        ledger_path: Path,
        poll_s: float = 2.0,
        *,
        ledger_factory: Callable[[Path], object] | None = None,
        clock: Callable[[], float] = time.time,
    ):
        self.ledger_path = Path(ledger_path)
        self.poll_s = float(poll_s)
        self._factory = ledger_factory or _default_ledger_factory
        self._clock = clock
        self._ledger = None
        self._by_id: dict[str, dict] = {}
        self._arrivals: dict[str, dict] = {}
        self._pending: queue.Queue = queue.Queue()
        self._overlay: dict[str, dict] = {}
        self._swap = threading.Lock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._last_version: int | None = None
        self.last_error: str | None = None
        self.refreshed_at: float | None = None

    # -- lifecycle ---------------------------------------------------------------------------------------
    def start(self) -> None:
        """Start the refresher thread (idempotent)."""
        if self._thread is not None and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(target=self._run, name="ctx_handover-veto", daemon=True)
        self._thread.start()

    def stop(self, timeout: float = 5.0) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout)
        self._thread = None

    def _run(self) -> None:
        self.refresh_now()
        while not self._stop.wait(self.poll_s):
            self.refresh_now()

    # -- refresher -------------------------------------------------------------------------------------------
    def _get_ledger(self):
        if self._ledger is None:
            self._ledger = self._factory(self.ledger_path)
        return self._ledger

    def refresh_now(self, *, force: bool = False) -> bool:
        """One refresher tick (synchronous): flush queued writes, rebuild the maps if the ledger changed.
        Returns whether the maps were rebuilt. Never raises."""
        try:
            ledger = self._get_ledger()
        except Exception as exc:
            self.last_error = f"{exc.__class__.__name__}: {exc}"
            return False
        flushed = self._flush(ledger)
        try:
            version = int(ledger.data_version())
        except Exception as exc:
            self.last_error = f"{exc.__class__.__name__}: {exc}"
            return False
        if not force and not flushed and version == self._last_version:
            return False
        try:
            rows = list(ledger.veto_rows())
            arrival_rows = getattr(ledger, "arrival_rows", None)
            arrivals = list(arrival_rows()) if callable(arrival_rows) else []
        except Exception as exc:
            self.last_error = f"{exc.__class__.__name__}: {exc}"
            return False
        by_id: dict[str, dict] = {}
        for row in rows:
            entry = dict(row)
            if str(entry.get("state")) not in VETO_STATES:
                continue
            entry = veto_entry(entry)
            by_id[str(entry.get("session_id"))] = entry
        arrival_map = {
            str(a.get("session_id")): dict(a)
            for a in arrivals
            if a.get("note_state", "pending") in ("pending", "offered")
        }
        with self._swap:
            for sid, entry in self._overlay.items():
                by_id.setdefault(sid, entry)
            self._by_id = by_id
            self._arrivals = arrival_map
        self._last_version = version
        self.last_error = None
        self.refreshed_at = self._clock()
        return True

    def _flush(self, ledger) -> bool:
        wrote = False
        retry = []
        while True:
            try:
                job_id, session_id, root_id = self._pending.get_nowait()
            except queue.Empty:
                break
            try:
                ledger.veto_add(job_id, [session_id], root_id)
                wrote = True
                with self._swap:
                    self._overlay.pop(session_id, None)
            except Exception as exc:
                self.last_error = f"{exc.__class__.__name__}: {exc}"
                retry.append((job_id, session_id, root_id))
        for item in retry:
            self._pending.put(item)
        return wrote

    # -- hook-side (no I/O, never raises) ----------------------------------------------------------------------
    def lookup(self, session_id: str, parent_session_id: str | None = None) -> dict | None:
        """The veto entry (with ``message``) for ``session_id`` or, failing that, its ``parent_session_id``."""
        by_id = self._by_id
        try:
            entry = by_id.get(session_id)
            if entry is None and parent_session_id:
                entry = by_id.get(parent_session_id)
            return entry
        except Exception:
            try:
                return by_id.get(session_id) if isinstance(session_id, str) else None
            except Exception:
                return None

    def on_compaction(self, session_id: str, old_session_id: str) -> None:
        """A rotation made ``session_id`` the child of ``old_session_id``: veto it at once (memory) and queue the
        ledger row for the refresher. Never raises."""
        try:
            entry = self._by_id.get(old_session_id)
            if entry is None or not session_id:
                return
            child = {**entry, "session_id": session_id}
            with self._swap:
                self._by_id = {**self._by_id, session_id: child}
                self._overlay[session_id] = child
            self._pending.put((str(entry.get("job_id")), str(session_id), str(entry.get("root_id"))))
        except Exception:
            return

    def arrival(self, session_id: str) -> dict | None:
        """The pending/offered arrival row of ``session_id`` (handover note delivery, DESIGN §11.4)."""
        try:
            return self._arrivals.get(session_id)
        except Exception:
            return None
