"""``SourceAPI``/``TargetAPI``/block access over HTTP (layer L6; DESIGN §4.5, §4.6, §17.5).

Thin adapters from the contracts of ``engine/contracts.py`` to the v1 routes, through one ``wire.WireClient``.
Every job call carries the conductor's epoch in the body and in ``X-Ctxh-Epoch``; results are decoded with
``jsonutil.from_wire`` into the contract types. The remote source's participant job is opened lazily
(``POST /jobs`` with ``role = source`` is idempotent) because ``SourceAPI`` has no ``open_job``; the session
reference travels with ``source/plan`` (additive field) and ``source/lock``.

What the conductor sends as job options is narrowed for the wire (``remote_options``): local-only settings such
as ``export_roots`` and every ``include_secret`` hint stay on this host — the listener decides those from its
own configuration and never accepts them from a peer (R18, I6).
"""

from __future__ import annotations

import json
from collections.abc import Iterable, Iterator, Sequence

from .blocks import encode_frame, verify_manifest
from .contracts import JobContext
from .errors import BadRequest, HandoverError, LeaseLost, NotFound
from .jsonutil import from_wire, to_wire
from .model import (
    CapsuleInfo,
    CommitAck,
    ContinueResult,
    DeltaInfo,
    EnvFacts,
    FinalPlan,
    Hello,
    ObjectManifest,
    OpStatus,
    Options,
    ParticipantStatus,
    PreparedInfo,
    PrepareRequest,
    ResourcePlan,
    Role,
    SessionInfo,
    SessionRef,
)

MAX_OBJECT_IDS = 64
MAX_IDS = 4096
LOST_STATES = frozenset({"suspended", "released", "releasing", "reclaimed", "reclaiming", "aborted"})

_LOCAL_ONLY = ("export_roots", "allow_roots", "workspace_root", "trust_unsigned", "dest", "maps")


def remote_options(options: Options) -> dict:
    """The job options as sent to a peer: ``include_secret`` emptied and local-only settings removed."""
    wire = to_wire(options)
    hints = dict(wire.get("hints") or {})
    hints["include_secret"] = []
    wire["hints"] = hints
    for key in _LOCAL_ONLY:
        wire.pop(key, None)
    return wire


def _op(data: dict) -> OpStatus:
    return from_wire(OpStatus, data)


def _lost_state(exc: HandoverError) -> str | None:
    """The source state a refused keepalive names when the source gave up its leases (route #9): a
    ``source_reclaimed`` code, or a non-retryable refusal whose ``details.state`` is a lost state (not a stale
    epoch of this conductor); None for every other error."""
    if exc.code == "source_reclaimed" or exc.details.get("peer_code") == "source_reclaimed":
        return "reclaimed"
    state = exc.details.get("state")
    if not exc.retryable and exc.details.get("reason") != "stale_epoch" and state in LOST_STATES:
        return str(state)
    return None


class _Remote:
    def __init__(self, client) -> None:
        self.client = client
        self._opened: set[str] = set()
        self._job: JobContext | None = None

    def _call(self, job: JobContext, route: str, body: dict | None = None) -> dict:
        self._job = job
        return self.client.call("POST", f"jobs/{job.job_id}/{route}", body or {}, epoch=job.epoch)

    def _open(self, job: JobContext, role: Role) -> None:
        self._job = job
        if job.job_id in self._opened:
            return
        self.client.call(
            "POST",
            "jobs",
            {
                "job_id": job.job_id,
                "epoch": int(job.epoch),
                "role": str(role),
                "mode": str(job.mode),
                "conductor": self.client.conductor_info(),
                "options": remote_options(job.options),
            },
        )
        self._opened.add(job.job_id)

    def hello(self) -> Hello:
        return self.client.hello(signed=True)

    def status(self, job: JobContext) -> ParticipantStatus:
        return from_wire(ParticipantStatus, self._call(job, "status"))

    def op_status(self, job: JobContext, op_id: str) -> OpStatus:
        status = self.status(job)
        for op in status.ops.values():
            if op.op_id == op_id:
                return op
        raise NotFound("operation not known to the peer", op_id=op_id)

    def blocks_for(self, job: JobContext) -> RemoteBlocks:
        """The job's blocks on the peer (a fresh conductor instance has not called a job method yet)."""
        self._job = job
        return RemoteBlocks(self.client, job.job_id, job.epoch)

    def _blocks(self) -> RemoteBlocks:
        if self._job is None:
            raise NotFound("no job yet: call a job method before blocks()")
        return RemoteBlocks(self.client, self._job.job_id, self._job.epoch)


class RemoteSource(_Remote):
    """The source side of a pull, served by the listener (routes #4, #8–#20)."""

    def list_sessions(self, limit: int, query: str | None, include_archived: bool) -> list[dict]:
        body: dict = {"limit": int(limit), "include_archived": bool(include_archived)}
        if query:
            body["query"] = query
        return list(self.client.call("POST", "sessions/list", body).get("sessions", []))

    def facts(self, job: JobContext) -> EnvFacts:
        self._open(job, Role.SOURCE)
        return from_wire(EnvFacts, self._call(job, "source/facts"))

    def start_plan(self, job: JobContext, target_facts: EnvFacts, session_ref: SessionRef) -> OpStatus:
        self._open(job, Role.SOURCE)
        hints = remote_options(job.options)["hints"]
        body = {"target_facts": to_wire(target_facts), "hints": hints, "session_ref": to_wire(session_ref)}
        return _op(self._call(job, "source/plan", body))

    def lock(self, job: JobContext, session_ref: SessionRef, plan_revision: tuple) -> SessionInfo:
        self._open(job, Role.SOURCE)
        wait_s = int(job.options.wait_timeout_s) if job.options.wait else 0
        body = {"session_ref": to_wire(session_ref), "wait_s": wait_s, "plan_revision": list(plan_revision)}
        return from_wire(SessionInfo, self._call(job, "source/lock", body))

    def heartbeat(self, job: JobContext) -> None:
        """Route #9; a source that gave up its leases ⇒ ``LeaseLost`` naming its state (``reclaimed`` …), whether the
        listener answers ``{state}`` (DESIGN §4.7) or refuses with that state (contract V17)."""
        self._open(job, Role.SOURCE)
        try:
            state = self._call(job, "heartbeat", {"conductor_state": ""}).get("state")
        except HandoverError as exc:
            state = _lost_state(exc)
            if state is None:
                raise
        if state in LOST_STATES:
            raise LeaseLost(f"the source gave up its leases (state {state})", state=state)

    def start_export(self, job: JobContext) -> OpStatus:
        return _op(self._call(job, "source/export"))

    def start_pack(self, job: JobContext, final_plan: FinalPlan) -> OpStatus:
        return _op(self._call(job, "source/pack", {"final_plan": to_wire(final_plan)}))

    def start_prepare(self, job: JobContext, prepared: PreparedInfo) -> OpStatus:
        return _op(self._call(job, "source/prepare", {"prepared": to_wire(prepared)}))

    def start_retire(self, job: JobContext, ack: CommitAck) -> OpStatus:
        return _op(self._call(job, "source/retire", {"ack": to_wire(ack)}))

    def start_release(self, job: JobContext, reason: str) -> OpStatus:
        return _op(self._call(job, "source/release", {"reason": str(reason)[:200]}))

    def blocks(self) -> RemoteBlocks:
        return self._blocks()


class RemoteTarget(_Remote):
    """The target side of a push, served by the listener (routes #7–#10, #21–#32)."""

    def open_job(self, job: JobContext) -> None:
        self._open(job, Role.TARGET)

    def facts(self, job: JobContext, source_facts: EnvFacts) -> EnvFacts:
        return from_wire(EnvFacts, self._call(job, "target/facts", {"source_facts": to_wire(source_facts)}))

    def negotiate(self, job: JobContext, plan: ResourcePlan) -> FinalPlan:
        return from_wire(FinalPlan, self._call(job, "target/negotiate", {"plan": to_wire(plan)}))

    def start_stage_context(self, job: JobContext, capsule: CapsuleInfo) -> OpStatus:
        return _op(self._call(job, "target/stage_context", {"capsule": to_wire(capsule)}))

    def start_prepare(self, job: JobContext, req: PrepareRequest) -> OpStatus:
        return _op(self._call(job, "target/prepare", {"request": to_wire(req)}))

    def start_apply_delta(self, job: JobContext, delta: DeltaInfo) -> OpStatus:
        return _op(self._call(job, "target/apply_delta", {"delta": to_wire(delta)}))

    def start_reset_session(self, job: JobContext) -> OpStatus:
        return _op(self._call(job, "target/reset_session"))

    def start_commit(self, job: JobContext) -> OpStatus:
        return _op(self._call(job, "target/commit"))

    def continue_session(self, job: JobContext, prompt: str) -> ContinueResult:
        data = self._call(job, "target/continue", {"prompt": str(prompt)[:2000]})
        return ContinueResult(started=bool(data.get("started")), pid=data.get("pid"), log=data.get("log"))

    def abort(self, job: JobContext, reason: str) -> None:
        self._call(job, "target/abort", {"reason": str(reason)[:200]})

    def blocks(self) -> RemoteBlocks:
        return self._blocks()


class RemoteBlocks:
    """``BlockSource`` + ``BlockSink`` of one job on a peer (routes #19, #20, #23–#25)."""

    def __init__(self, client, job_id: str, epoch: int) -> None:
        self.client = client
        self.job_id = job_id
        self.epoch = int(epoch)

    def _call(self, route: str, body: dict) -> dict:
        return self.client.call("POST", f"jobs/{self.job_id}/{route}", body, epoch=self.epoch)

    # BlockSource
    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        """Manifests in request order; each one's id is recomputed and must be the one requested (§4.6 chain)."""
        out: dict[str, ObjectManifest] = {}
        ids = list(object_ids)
        for start in range(0, len(ids), MAX_OBJECT_IDS):
            chunk = ids[start : start + MAX_OBJECT_IDS]
            cursor: str | None = None
            while True:
                body: dict = {"object_ids": chunk}
                if cursor:
                    body["cursor"] = cursor
                data = self._call("objects/list", body)
                for raw in data.get("manifests", []):
                    m = verify_manifest(from_wire(ObjectManifest, raw))
                    if m.object_id not in chunk:
                        raise BadRequest("peer returned a manifest that was not requested", field="manifests")
                    out[m.object_id] = m
                cursor = data.get("cursor")
                if not cursor:
                    break
        missing = [i for i in ids if i not in out]
        if missing:
            raise NotFound("peer did not return every requested manifest", object_id=missing[0])
        return [out[i] for i in ids]

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        return self.client.get_blocks(self.job_id, self.epoch, list(ids))

    # BlockSink
    def put_manifests(self, manifests: Sequence[ObjectManifest]) -> int:
        stored = 0
        batch: list[dict] = []
        size = 0
        for m in manifests:
            wire = to_wire(m)
            n = len(json.dumps(wire))
            if batch and (len(batch) >= MAX_OBJECT_IDS or size + n > 2 * 1024**2):
                stored += int(self._call("objects/put", {"manifests": batch}).get("stored", 0))
                batch, size = [], 0
            batch.append(wire)
            size += n
        if batch:
            stored += int(self._call("objects/put", {"manifests": batch}).get("stored", 0))
        return stored

    def missing(self, ids: Sequence[str]) -> list[str]:
        out: list[str] = []
        ids = list(ids)
        for start in range(0, len(ids), MAX_IDS):
            out.extend(self._call("blocks/need", {"ids": ids[start : start + MAX_IDS]}).get("missing", []))
        return out

    def write_blocks(self, frames: Iterable[tuple[str, bytes]]) -> tuple[int, list[tuple[str, str]]]:
        caps = self.client.peer_caps
        encoded = (encode_frame(block_id, raw, peer_caps=caps) for block_id, raw in frames)
        return self.client.put_blocks(self.job_id, self.epoch, encoded)
