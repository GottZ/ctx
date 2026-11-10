"""Listener-side participants of remote-driven jobs (layer L7, stdlib only; INTERFACES §4.5, DESIGN §4.5, §7.3).

The ``ParticipantRegistry`` sits between the HTTP server (cluster C) and the local participant APIs
(``LocalSource``/``LocalTarget``). It owns the binding rules of DESIGN §4.5 [SEC 1.7]:

* ``POST /jobs`` with ``role = source`` needs the peer right ``pull``, ``role = target`` needs ``push``; every
  later call re-checks the job's role against the caller's rights. The mode is the role's one wire mode
  (``WIRE_MODE``: source ⇒ ``pull``, target ⇒ ``push``); any other pair is refused (R6-A).
* Jobs are keyed by ``(peer_id, job_id)``: another peer gets ``not_found`` and cannot reuse the id.
* Every job request carries the conductor's epoch; a lower epoch than the highest seen ⇒
  ``invalid_state(stale_epoch)`` (DESIGN §3.1).
* Offers are listed and claimed only by their own peer.
* A remote conductor's options can only narrow: security settings (roots, limits, secrets) stay local
  (``participant_options``; R18, R19).
* Job ids are ULIDs (``ids.ULID_RE``, contract K-2). The registry alone owns the job caps per peer and per
  gateway (constructor values; the composition root fills them from ``max_jobs_per_peer``/``max_jobs_total``) and
  the routes #8–#10 (``status``/``heartbeat``/``cancel``); the server only delegates (contract X18).

``sweep`` runs periodically in the listener process: it heartbeats the participant jobs this process owns,
gives up the leases of a source whose remote conductor went quiet for ``participant_idle_timeout`` (``suspended``,
DESIGN §7.3; never the source of this host's own push/export), expires target staging after ``staging_ttl`` (§3.5)
and continues aborts left by a revocation.
"""

from __future__ import annotations

import time
from collections.abc import Callable, Sequence
from dataclasses import fields, replace

from .contracts import JobContext, SourceAPI, TargetAPI
from .errors import (
    BadRequest,
    Forbidden,
    HandoverError,
    InvalidState,
    JobConflict,
    LeaseLost,
    NotFound,
    RateLimited,
)
from .ids import ULID_RE
from .jsonutil import from_wire
from .ledger import JOURNAL_KEEP_S, STALE_OWNER_S, TERMINAL, Ledger, process_owner
from .model import AuthContext, Mode, OpState, Options, ParticipantStatus, Peer, Progress, Right, Role, SessionRef

MAX_JOBS_PER_PEER = 2
MAX_JOBS = 4
MAX_SESSIONS_LIST = 200
CONTINUE_PROMPT_MAX = 2000

_REMOTE_FIELDS = ("dest", "maps", "model", "force", "wait", "continue_prompt", "yes", "dry_run")
"""Options a remote conductor may set for its job; everything else comes from the local settings."""

_REMOTE_HINTS = ("exclude", "dest_overrides", "no_workspace")
"""Hints a remote conductor may give: they narrow the plan or name destinations (R18, R19); never additions."""

WIRE_MODE = {Role.SOURCE: Mode.PULL, Role.TARGET: Mode.PUSH}
"""The one mode each role has when a paired device opens it here (DESIGN §2.2): the device pulls from a source here
or pushes to a target here. Export, import and offer never cross the wire; a target opened as ``pull`` would pass
for this host's own job (review R6-A)."""

_RELEASABLE_SOURCE = frozenset({"locking", "locked", "preparing", "suspended"})
_LOST_SOURCE = frozenset({"suspended", "releasing", "released", "reclaiming", "reclaimed"})
"""Source states without the session's leases: route #9 names them (``remote.LOST_STATES``)."""
_TARGET_TTL_EXEMPT = frozenset({"committing", "committed", "aborted", "expired"})


def participant_options(local: Options, remote: dict | None) -> Options:
    """The job options a participant uses: its own settings, narrowed by what the remote conductor asked for.

    A remote side never widens roots, limits or releases: ``export_roots``/``allow_roots``/sizes stay local; of
    the hints only the narrowing ones (``exclude``, ``no_workspace``) and the destination overrides (checked
    against ``allow_roots`` on the target, R19) are taken — ``include_secret`` and ``include`` never come from a
    peer (R18, contract X07: a remote addition is a FinalPlan item the source releases under ``export_roots``,
    ``plan.restrict_to_releasable``). ``wait_timeout_s`` is capped at the local value, ``refine`` can only be
    switched off, the continuation prompt is capped at 2000 characters (DESIGN §11.5)."""
    remote = dict(remote or {})
    names = {f.name for f in fields(Options)}
    picked: dict = {}
    for key in _REMOTE_FIELDS:
        if key in remote and key in names:
            picked[key] = remote[key]
    hints_in = remote.get("hints") if isinstance(remote.get("hints"), dict) else {}
    hints = {k: v for k, v in hints_in.items() if k in _REMOTE_HINTS}
    decoded = from_wire(Options, {**picked, "hints": hints})
    out = replace(
        local,
        **{k: getattr(decoded, k) for k in picked},
        hints=replace(decoded.hints, include=(), include_secret=()),
    )
    if isinstance(remote.get("wait_timeout_s"), int) and not isinstance(remote.get("wait_timeout_s"), bool):
        out = replace(out, wait_timeout_s=max(0, min(local.wait_timeout_s, int(remote["wait_timeout_s"]))))
    if remote.get("refine") == "off":
        out = replace(out, refine="off")
    if out.continue_prompt is not None:
        out = replace(out, continue_prompt=str(out.continue_prompt)[:CONTINUE_PROMPT_MAX])
    return out


class _NullReporter:
    def progress(self, p: Progress) -> None:
        pass

    def info(self, text: str) -> None:
        pass

    def warn(self, text: str) -> None:
        pass


_NULL = _NullReporter()


class ParticipantRegistry:
    """Binds remote requests to local participant APIs (see module docstring)."""

    def __init__(
        self,
        ledger: Ledger,
        factory: Callable[[Role, Peer | None], SourceAPI | TargetAPI],
        *,
        clock: Callable[[], float] | None = None,
        local_options: Options | Callable[[], Options] | None = None,
        max_jobs_per_peer: int = MAX_JOBS_PER_PEER,
        max_jobs: int = MAX_JOBS,
        on_error: Callable[[str, BaseException], None] | None = None,
    ) -> None:
        self.ledger = ledger
        self._factory = factory
        self._clock = clock or time.time
        self._local_options = local_options
        self._max_per_peer = max_jobs_per_peer
        self._max_jobs = max_jobs
        self._on_error = on_error or (lambda where, exc: None)
        self._last_compaction = 0.0

    # -- helpers -------------------------------------------------------------------------------------------------
    def _options(self) -> Options:
        lo = self._local_options
        if callable(lo):
            return lo()
        return lo or Options()

    @staticmethod
    def _peer(auth: AuthContext) -> Peer:
        if auth.peer is None:
            raise Forbidden("this route needs a paired peer")
        return auth.peer

    @staticmethod
    def _right_for(role: Role) -> Right:
        if role is Role.SOURCE:
            return Right.PULL
        if role is Role.TARGET:
            return Right.PUSH
        raise BadRequest("role must be source or target", field="role")

    def _require(self, auth: AuthContext, right: Right) -> None:
        if right not in auth.rights:
            raise Forbidden(f"this device has no {right} right here", right=str(right))

    def _context(self, row: dict, peer: Peer | None, epoch: int) -> JobContext:
        params = row["params"] or {}
        job_id = row["job_id"]
        return JobContext(
            job_id=job_id,
            mode=Mode(row["mode"]),
            peer=peer,
            options=participant_options(self._options(), params.get("options")),
            epoch=int(epoch),
            reporter=_NULL,
            cancelled=lambda: self._cancelled(job_id, Role(row["role"])),
            created_at=float(row["created_at"]),
        )

    def _cancelled(self, job_id: str, role: Role) -> bool:
        row = self.ledger.job(job_id, role)
        return row is None or row["state"] in ("aborting", "aborted", "expired")

    def _api(self, role: Role, peer: Peer | None):
        return self._factory(role, peer)

    def _row(self, auth: AuthContext, job_id: str, role: Role) -> dict:
        peer = self._peer(auth)
        row = self.ledger.job(job_id, role)
        if row is None or row["peer_id"] != peer.peer_id:
            raise NotFound(f"job {job_id} not found")
        return row

    @staticmethod
    def _check_mode(role: Role, mode: Mode) -> None:
        if mode is not WIRE_MODE[role]:
            raise BadRequest(f"a paired device can open a {role} job here only as a {WIRE_MODE[role]}", field="mode")

    def bind(self, auth: AuthContext, job_id: str, role: Role) -> tuple[SourceAPI | TargetAPI, JobContext]:
        """Job bound to the calling peer, role right re-checked, epoch fenced; returns ``(api, job context)``. Only rows
        route #7 opened are served: a row of this host's own job is ``not_found``, and a row route #7 wrote before it
        checked the mode (R6-A) that carries another one is refused."""
        row = self._row(auth, job_id, role)
        if (row.get("params") or {}).get("local") is True:
            # a row of this host's own job that carries the device's id — the planning row of an offer, whose id the
            # device learns from route #5 (Fremd-Review R6): never a job the device drives
            raise NotFound(f"job {job_id} not found")
        self._require(auth, self._right_for(role))
        self._check_mode(role, Mode(row["mode"]))
        epoch = row["max_peer_epoch"]
        if auth.epoch is not None:
            epoch = self.ledger.peer_epoch(job_id, role, int(auth.epoch))
        self.ledger.touch_job(job_id, role)
        return self._api(role, auth.peer), self._context(row, auth.peer, epoch)

    def _find(self, auth: AuthContext, job_id: str) -> Role:
        peer = self._peer(auth)
        for role in (Role.SOURCE, Role.TARGET):
            row = self.ledger.job(job_id, role)
            if row is not None and row["peer_id"] == peer.peer_id:
                return role
        raise NotFound(f"job {job_id} not found")

    # -- routes ------------------------------------------------------------------------------------------------------
    def open(self, auth: AuthContext, body: dict) -> dict:
        """``POST /jobs`` (route #7): create or re-open the participant job (idempotent per params)."""
        peer = self._peer(auth)
        job_id = body.get("job_id")
        if not isinstance(job_id, str) or not ULID_RE.match(job_id):  # job ids are ULIDs everywhere (contract K-2)
            raise BadRequest("invalid job id", field="job_id")
        try:
            role = Role(str(body.get("role")))
            mode = Mode(str(body.get("mode")))
            epoch = int(body.get("epoch"))
        except (TypeError, ValueError):
            raise BadRequest("role, mode and epoch are required", field="role") from None
        self._require(auth, self._right_for(role))
        self._check_mode(role, mode)
        existing = self.ledger.job(job_id, role)
        if existing is not None and existing["peer_id"] != peer.peer_id:
            raise JobConflict("job id not available")
        # one job id, one peer — across roles (review M17): the objects a job may serve (``job_objects``) are keyed by
        # the id alone, so a source job opened under the id of another peer's push (or of this host's own
        # conductor job) would release that job's objects to the caller
        for other in Role:
            row = self.ledger.job(job_id, other) if other is not role else None
            if row is not None and (other is Role.CONDUCTOR or row["peer_id"] != peer.peer_id):
                raise JobConflict("job id not available")
        if existing is None:
            self._check_caps(peer.peer_id)
        params = {
            "role": str(role),
            "mode": str(mode),
            "conductor": dict(body.get("conductor") or {}),
            "session_ref": body.get("session_ref"),
            "options": dict(body.get("options") or {}),
        }
        self.ledger.create_job(job_id, role, mode, peer.peer_id, params)
        epoch = self.ledger.peer_epoch(job_id, role, epoch)
        row = self.ledger.job(job_id, role)
        job = self._context(row, peer, epoch)
        if role is Role.TARGET:
            self._api(role, peer).open_job(job)
        self.ledger.touch_job(job_id, role)
        row = self.ledger.job(job_id, role)
        return {"job_id": job_id, "state": row["state"], "epoch": epoch}

    def _unlocked_idle(self, row: dict, now: float, after_s: float) -> bool:
        """A remote conductor's source job that never locked its session (opened at FACTS; ``source_handovers`` has
        no row for it) and had no request for ``after_s`` (review R2-M1): it holds no session and no lease."""
        return (
            row["role"] == str(Role.SOURCE)
            and (row.get("params") or {}).get("local") is not True
            and float(row["updated_at"]) < now - float(after_s)
            and self.ledger.source_state(row["job_id"]) is None
        )

    def _check_caps(self, peer_id: str) -> None:
        """The caps count the jobs remote conductors opened here (route #7). The participant rows of this host's own
        conductor jobs and the planning row of an offer (``params.local``) are no such job (L3-F07: an open offer
        and a pending export used up the slots of the peer). Neither is a source job that never locked anything and
        had no request for ``participant_idle_timeout`` (review R2-M1: its conductor aborted while this side was
        offline); it stays open for a conductor that comes back and ends in ``sweep`` after ``staging_ttl``."""
        now, idle_s = self.ledger.now(), self._options().participant_idle_timeout_s
        active = [
            j
            for j in self.ledger.jobs(active=True, limit=10_000)
            if j["role"] != str(Role.CONDUCTOR)
            and (j.get("params") or {}).get("local") is not True
            and not self._unlocked_idle(j, now, idle_s)
        ]
        if sum(1 for j in active if j["peer_id"] == peer_id) >= self._max_per_peer:
            raise RateLimited("too many concurrent handovers for this device", reason="concurrent_jobs")
        if len(active) >= self._max_jobs:
            raise RateLimited("too many concurrent handovers on this gateway", reason="concurrent_jobs")

    def get(self, auth: AuthContext, job_id: str, role: Role) -> SourceAPI | TargetAPI:
        """The participant API of a job (bound to the calling peer, role right re-checked, epoch fenced)."""
        return self.bind(auth, job_id, role)[0]

    def context(self, auth: AuthContext, job_id: str, role: Role) -> JobContext:
        """The ``JobContext`` to pass to the API of that job (same checks as ``get``)."""
        return self.bind(auth, job_id, role)[1]

    job_context = context  # name used by server.Server (cluster C)

    def list_sessions(self, auth: AuthContext, body: dict) -> dict:
        """Route #4: the session picker of a pull (needs ``pull``; titles of non-archived sessions, §8.7)."""
        self._require(auth, Right.PULL)
        try:
            limit = int(body.get("limit") or 20)
        except (TypeError, ValueError):
            raise BadRequest("limit must be an integer", field="limit") from None
        limit = max(1, min(MAX_SESSIONS_LIST, limit))
        query = body.get("query")
        query = str(query)[:200] if query else None
        api = self._api(Role.SOURCE, auth.peer)
        return {"sessions": api.list_sessions(limit, query, bool(body.get("include_archived")))}

    def offers(self, auth: AuthContext) -> dict:
        """Route #5: offers of the calling peer only."""
        peer = self._peer(auth)
        self._require(auth, Right.PULL)
        rows = self.ledger.offers_for(peer.peer_id, self._clock())
        return {
            "offers": [
                {
                    "offer_id": r["offer_id"],
                    "session": r["session_id"],
                    "created_at": r["created_at"],
                    "expires_at": r["expires_at"],
                    "note": r["note"],
                    "has_refinement": r["refinement"] is not None,
                }
                for r in rows
            ]
        }

    def claim(self, auth: AuthContext, offer_id: str, job_id: str) -> dict:
        """Route #6: claim an offer of the calling peer for ``job_id`` (never auto-claimed, O6)."""
        peer = self._peer(auth)
        self._require(auth, Right.PULL)
        if not isinstance(job_id, str) or not ULID_RE.match(job_id):
            raise BadRequest("invalid job id", field="job_id")
        row = self.ledger.offer_claim(str(offer_id), peer.peer_id, str(job_id))
        out: dict = {"session_ref": {"id": row["session_id"], "latest": False, "offer_id": row["offer_id"]}}
        if row["refinement"] is not None:
            out["refinement"] = row["refinement"]
        return out

    def touch(self, job_id: str) -> None:
        """Record a request for the job (idle timeout, DESIGN §7.3)."""
        for role in (Role.SOURCE, Role.TARGET):
            if self.ledger.job(job_id, role) is not None:
                self.ledger.touch_job(job_id, role)

    def heartbeat(self, auth: AuthContext, job_id: str, body: dict) -> dict:
        """Route #9: keep a source's leases alive while the conductor works (DESIGN §7.3). The answer is
        ``{state}`` of the participant (DESIGN §4.7): for a source its handover state, so a source that gave up its
        leases (``reclaimed``, ``released``, ``suspended`` …) names that state and the dialer's ``RemoteSource``
        raises ``LeaseLost(state=…)`` (contract V17) instead of seeing an anonymous error."""
        role = self._find(auth, job_id)
        auth = replace(auth, epoch=body.get("epoch", auth.epoch))
        api, job = self.bind(auth, job_id, role)
        if role is Role.SOURCE:
            lost = self._lost_source_state(job_id)
            if lost is not None:
                return {"state": lost}
            try:
                api.heartbeat(job)
            except LeaseLost:
                lost = self._lost_source_state(job_id)
                if lost is None:
                    raise
                return {"state": lost}
            src = self.ledger.source_state(job_id)
            if src is not None:
                return {"state": src["state"]}
        row = self.ledger.job(job_id, role)
        return {"state": row["state"]}

    def _lost_source_state(self, job_id: str) -> str | None:
        """The state of a source that no longer holds its leases (``_LOST_SOURCE``), else None."""
        src = self.ledger.source_state(job_id)
        state = src.get("state") if src is not None else None
        return str(state) if state in _LOST_SOURCE else None

    def cancel(self, auth: AuthContext, job_id: str, body: dict) -> dict:
        """Route #10: only the conductor's peer, current epoch; a prepared source or committing target refuse."""
        role = self._find(auth, job_id)
        auth = replace(auth, epoch=body.get("epoch", auth.epoch))
        api, job = self.bind(auth, job_id, role)
        reason = "cancelled: " + str(body.get("reason") or "conductor")[:200]
        if role is Role.SOURCE:
            src = self.ledger.source_state(job_id)
            state = src["state"] if src else "open"
            if state not in _RELEASABLE_SOURCE and state != "open":
                raise InvalidState(f"the source is {state}", reason="state", state=state)
            api.start_release(job, reason)
        else:
            api.abort(job, reason)
        return {"state": self.ledger.job(job_id, role)["state"]}

    def status(self, auth: AuthContext, job_id: str, body: dict) -> ParticipantStatus:
        """Route #8; ``note = "source_reclaimed"`` tells a target that the source took the session back (§7.7)."""
        role = self._find(auth, job_id)
        auth = replace(auth, epoch=body.get("epoch", auth.epoch))
        api, job = self.bind(auth, job_id, role)
        if role is Role.TARGET and body.get("note") == "source_reclaimed":
            self.ledger.arrival_source_reclaimed(job_id)
        return api.status(job)

    # -- maintenance -------------------------------------------------------------------------------------------------
    def _internal_job(self, row: dict) -> tuple[SourceAPI | TargetAPI, JobContext]:
        peer = self.ledger.peer(row["peer_id"]) if row["peer_id"] else None
        return self._api(Role(row["role"]), peer), self._context(row, peer, row["max_peer_epoch"])

    def sweep(self, now: float) -> None:
        """Heartbeat owned jobs, idle source leases, staging TTL (also of a source job that never locked anything,
        review R2-M1), pending aborts, journal compaction."""
        opts = self._options()
        me = process_owner()
        for row in self.ledger.jobs(active=True, limit=10_000):
            role = Role(row["role"])
            if role is Role.CONDUCTOR:
                continue
            try:
                if row["owner"] == me:
                    self.ledger.heartbeat(row["job_id"], role, me, row["owner_epoch"])
                if role is Role.SOURCE:
                    src = self.ledger.source_state(row["job_id"])
                    # idle means a *remote* conductor went quiet (DESIGN §7.3); the source row of this host's own
                    # push/export (``params.local``) moves only with transitions, and its conductor keeps it alive
                    # itself — the janitor handles that conductor's death (review H7)
                    if (
                        src is not None
                        and src["state"] in ("locking", "locked")
                        and (row.get("params") or {}).get("local") is not True
                        and row["updated_at"] < now - float(opts.participant_idle_timeout_s)
                    ):
                        api, job = self._internal_job(row)
                        api.suspend(job)
                    elif row["state"] == "aborting":
                        api, job = self._internal_job(row)
                        api.start_release(job, "aborting")
                    elif src is None and self._unlocked_idle(row, now, opts.staging_ttl_s):
                        # opened at FACTS, never locked, no request since: the conductor is gone (it aborted while
                        # this side was offline, or never came back) — the row ends like the staging of a target
                        # (DESIGN §3.5) instead of staying active for ever (review R2-M1)
                        api, job = self._internal_job(row)
                        api.start_release(job, "expired")
                else:
                    state = row["state"]
                    if state == "aborting":
                        api, job = self._internal_job(row)
                        api.abort(job, "aborting")
                    elif state not in _TARGET_TTL_EXEMPT and row["created_at"] < now - float(opts.staging_ttl_s):
                        api, job = self._internal_job(row)
                        api.abort(job, "expired")
            except HandoverError as exc:
                self._on_error(f"sweep {row['job_id']}", exc)
        if now - self._last_compaction > 3600:
            self._last_compaction = now
            try:
                self.ledger.compact_terminal(JOURNAL_KEEP_S)
            except HandoverError as exc:
                self._on_error("compaction", exc)

    def recover(self, row: dict) -> str:
        """Janitor action for a participant job whose owning process died (DESIGN §3.6); returns what was done.

        source ``locked``/``preparing``/``releasing`` ⇒ release (undo the journaled prepare steps) · ``prepared`` ⇒
        nothing (fail-closed until the conductor or the user resolves it) · ``retiring`` ⇒ continue the retire ·
        ``reclaiming`` ⇒ kept: a repeated ``/handover reclaim`` finishes it (``lock.reclaim`` accepts the state;
        review L4) · target ``committing`` ⇒ continue the commit op · other target states are handled by the
        conductor (interrupted ops are restarted) and by ``sweep`` (staging TTL).

        The source row of this host's own conductor job (``params.local``: push, export) is kept while that
        conductor's row is alive (``Ledger.owner_alive``): the conductor decides about its source — a stale source
        row alone (no step for ``STALE_OWNER_S`` between pack and prepare) is no dead job (review H6)."""
        role = Role(row["role"])
        if (
            role is Role.SOURCE
            and (row.get("params") or {}).get("local") is True
            and self.ledger.owner_alive(row["job_id"], Role.CONDUCTOR, STALE_OWNER_S)
        ):
            return "kept"
        api, job = self._internal_job(row)
        if role is Role.SOURCE:
            src = self.ledger.source_state(row["job_id"])
            state = src["state"] if src else row["state"]
            if state in ("locking", "locked", "preparing", "releasing"):
                st = api.start_release(job, "owner_died")
                # the release runs as an operation: "released" only when it finished, else it is under way
                return "released" if getattr(st, "state", None) is OpState.DONE else "release_started"
            if state == "retiring" and src and src.get("ack"):
                from .model import CommitAck

                api.start_retire(job, from_wire(CommitAck, src["ack"]))
                return "retire_continued"
            return "kept"
        if row["state"] == "committing":
            recover = getattr(api, "recover", None)
            if callable(recover):
                recover(job)
                return "commit_continued"
        return "kept"

    def abort_jobs(self, job_ids: Sequence[str], reason: str) -> None:
        """After ``revoke_peer`` (DESIGN §12.5): targets discard their staging, sources not yet prepared are
        released; a prepared source follows the in-doubt rule and stays locked."""
        for job_id in job_ids:
            for role in (Role.TARGET, Role.SOURCE):
                row = self.ledger.job(job_id, role)
                if row is None or row["state"] in TERMINAL[role]:
                    continue
                try:
                    api, job = self._internal_job(row)
                    if role is Role.TARGET:
                        api.abort(job, reason)
                    else:
                        src = self.ledger.source_state(job_id)
                        if src is None or src["state"] in _RELEASABLE_SOURCE:
                            api.start_release(job, reason)
                except HandoverError as exc:
                    self._on_error(f"abort {job_id}", exc)


def session_ref_of(row: dict) -> SessionRef:
    """The ``SessionRef`` a participant job was opened with (``None`` fields when absent)."""
    raw = (row.get("params") or {}).get("session_ref") or {}
    return from_wire(SessionRef, raw) if isinstance(raw, dict) else SessionRef()
