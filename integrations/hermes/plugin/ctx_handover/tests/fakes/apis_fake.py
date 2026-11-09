"""Fault-injection fakes of ``SourceAPI``/``TargetAPI`` (cluster A; INTERFACES §6, TESTPLAN §1 ``fault_apis``).

``World`` holds one ``FakeSource`` and one ``FakeTarget`` whose state machines follow DESIGN §3.3, plus their
block stores. Every API call can be intercepted at every point of every participant:

``world.inject(side, method, kind, …)``
    side    ``"source"`` | ``"target"`` | ``"source_blocks"`` | ``"target_blocks"``
    method  an API method name (``"lock"``, ``"start_commit"``, ``"op_status"`` …) or ``"complete:<op>"`` for
            the completion of an asynchronous operation (``complete:export``, ``complete:commit`` …)
    kind    ``error`` / ``error_after``           raise ``error`` before / after the call took effect
            ``disconnect`` / ``disconnect_after`` ``PeerUnreachable`` before / after (lost response)
            ``crash`` / ``crash_after``           ``SimulatedCrash`` (the conductor's process dies)
            ``action`` / ``action_after``         run ``action(world)`` (append rows, cancel, …)
            ``late``                              the operation stays ``running`` for ``polls`` more polls
            ``interrupt``                         the participant process dies: running ops are lost
                                                  (``failed``, retryable ``interrupted``) — state stays
    times / skip  how often the fault fires and how many matching calls pass first

Invariant monitor: after every call ``world.check()`` records a violation when the target copy is active
(committed) while the source session is still usable (not under the primary lock) — "never two active copies
unnoticed". A user reclaim (``world.source.reclaim(force=True)``) is the noticed exception.

The session is a list of row strings; ``h_all`` is the sha256 of the rows, so a target that imported anything
else than what the source held at its prepare point fails verification (``VerifyFailed``).

Objects follow the one id rule (contract K-1, ``jsonutil.object_manifest``: blocks of 524 288 bytes) unless a test
builds a ``FakeBlocks`` with a smaller block size for the block mover alone. Plan ids follow the one rule of DESIGN
§8.6 (``plan.plan_id`` over the FinalPlan document without ``plan_id``); ``FakePlanning`` delegates every planning
function to the real ``plan`` module except ``confirmation_reasons`` (see there).
"""

from __future__ import annotations

import hashlib
import itertools
import json
import threading
import time
from collections.abc import Callable, Iterable, Iterator, Sequence
from dataclasses import dataclass, field, replace

from ctx_handover.engine import CAPABILITIES_V1, PROTOCOL
from ctx_handover.engine import plan as planning
from ctx_handover.engine.errors import (
    HandoverError,
    InsufficientStorage,
    InvalidState,
    LeaseLost,
    NotFound,
    PayloadTooLarge,
    PeerUnreachable,
    SessionBusy,
    VerifyFailed,
)
from ctx_handover.engine.jsonutil import (
    OBJECT_BLOCK_SIZE,
    canonical_bytes,
    from_wire,
    object_id,
    object_manifest,
    sha256_hex,
    to_wire,
)
from ctx_handover.engine.model import (
    CapsuleInfo,
    CommitAck,
    ContinueResult,
    DeltaInfo,
    DestMode,
    EnvFacts,
    FinalPlan,
    Hello,
    HostFacts,
    Include,
    ItemKind,
    ItemRole,
    Limits,
    ObjectManifest,
    OpState,
    OpStatus,
    Origin,
    PackInfo,
    ParticipantStatus,
    PathMapEntry,
    PlanItem,
    PreparedInfo,
    PrepareOutcome,
    PrepareRequest,
    PrepareResult,
    RefineReport,
    ResourcePlan,
    SessionInfo,
    SessionRef,
    TargetDecision,
)

RESTARTABLE_OPS = frozenset({"retire", "release"})
"""Operations whose sub-steps are idempotent: a failed one runs again on the next start (DESIGN §3.7, §7.5)."""


class SimulatedCrash(BaseException):
    """The conductor's process dies here (not an ``Exception``: nothing in the engine may swallow it)."""


# ---------------------------------------------------------------------------------------------------------
# small builders
# ---------------------------------------------------------------------------------------------------------


def make_facts(name: str, *, free: int = 10 * 1024**3, offline: bool = True) -> EnvFacts:
    host = HostFacts(
        is_wsl=False,
        is_container=False,
        profile="default",
        multiplex=False,
        hermes_version="0.23.0",
        plugin_version="0.1.0",
        model="m-" + name,
        provider="p-" + name,
        context_length=131072,
        terminal_backend="local",
        hermes_home=f"/{name}/.hermes",
        document_cache=f"/{name}/.hermes/cache/documents",
    )
    return EnvFacts(  # noqa: S604 - EnvFacts.shell is a fact, no subprocess
        instance_name=name,
        os_family="linux",
        os_name="Linux",
        os_version="6.8",
        arch="x86_64",
        hostname=name,
        user="u",
        home=f"/{name}/home",
        shell="bash",
        host=host,
        tools={},
        tools_seen={},
        workspace_root=f"/{name}/home/hermes-handover",
        allow_roots=(f"/{name}/home/hermes-handover",),
        disk_free={"/": free},
        case_insensitive={"/": False},
        offline_capable=offline,
        locale="C.UTF-8",
        timezone="UTC",
        git_version="2.47.0",
        git_lfs=False,
    )


def make_item(item_id: str, path: str, *, include: Include = Include.YES, size: int = 100) -> PlanItem:
    return PlanItem(
        item_id=item_id,
        kind=ItemKind.FILE_TREE,
        source_path=path,
        role=ItemRole.PRIMARY if item_id == "w0" else ItemRole.EXTRA,
        origin=Origin.CWD if item_id == "w0" else Origin.TRANSCRIPT,
        include=include,
        reasons=("test",),
        size_estimate=size,
    )


def _sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def rows_hash(rows: Sequence[str]) -> str:
    return _sha(json.dumps(list(rows)).encode())


# ---------------------------------------------------------------------------------------------------------
# faults
# ---------------------------------------------------------------------------------------------------------


@dataclass
class Fault:
    side: str
    method: str
    kind: str
    error: HandoverError | None = None
    times: int = 1
    skip: int = 0
    polls: int = 0
    action: Callable[[World], None] | None = None
    fired: int = 0

    @property
    def after(self) -> bool:
        return self.kind.endswith("_after")

    def fire(self, world: World) -> None:
        base = self.kind.removesuffix("_after")
        if base == "error":
            raise self.error or HandoverError("injected")
        if base == "disconnect":
            raise PeerUnreachable(f"injected disconnect at {self.side}.{self.method}")
        if base == "crash":
            raise SimulatedCrash(f"{self.side}.{self.method}")
        if base == "action":
            assert self.action is not None
            self.action(world)
            return
        if base == "interrupt":
            world.interrupt(self.side)
            return
        raise AssertionError(f"unknown fault kind {self.kind}")


@dataclass
class FakeOp:
    op_id: str
    name: str
    digest: str
    complete: Callable[[], dict]
    polls_left: int
    state: OpState = OpState.RUNNING
    result: dict | None = None
    error: dict | None = None

    def status(self) -> OpStatus:
        return OpStatus(op_id=self.op_id, state=self.state, result=self.result, error=self.error)


# ---------------------------------------------------------------------------------------------------------
# block store
# ---------------------------------------------------------------------------------------------------------


class FakeBlocks:
    """In-memory ``BlockSource`` + ``BlockSink`` with manifests (DESIGN §4.6, §5.2)."""

    def __init__(self, world: World, side: str, block_size: int) -> None:
        self.world, self.side, self.block_size = world, side, block_size
        self.objects: dict[str, ObjectManifest] = {}
        self.data: dict[str, bytes] = {}
        self.max_body: int | None = None
        self.write_delay_s = 0.0
        self.inflight = 0
        self.max_inflight = 0
        self.write_calls: list[int] = []
        self.read_calls: list[int] = []
        self._lock = threading.Lock()

    def put_object(self, data: bytes, media: str = "application/json") -> ObjectManifest:
        if self.block_size == OBJECT_BLOCK_SIZE:  # the one id rule (contract K-1)
            doc, blocks = object_manifest(data, media)
            for bid, raw in blocks:
                self.data[bid] = raw
            m = ObjectManifest(
                sha256=doc["sha256"],
                size=doc["size"],
                block_size=doc["block_size"],
                blocks=tuple(doc["blocks"]),
                media=media,
                object_id=object_id(doc),
            )
            self.objects[m.object_id] = m
            return m
        ids = []  # a smaller block size exercises the block mover only (``move_object`` does not check ids)
        for off in range(0, max(len(data), 1), self.block_size):
            raw = data[off : off + self.block_size]
            bid = _sha(raw)
            self.data[bid] = raw
            ids.append(bid)
        m = ObjectManifest(
            sha256=_sha(data), size=len(data), block_size=self.block_size, blocks=tuple(ids), media=media
        )
        doc = {"kind": "ctxh.object", "v": 1, **{k: v for k, v in to_wire(m).items() if k != "object_id"}}
        m = replace(m, object_id=sha256_hex(canonical_bytes(doc)))
        self.objects[m.object_id] = m
        return m

    def put_doc(self, doc: dict) -> ObjectManifest:
        return self.put_object(canonical_bytes(doc))

    def read_object(self, object_id: str) -> bytes:
        m = self.objects[object_id]
        return b"".join(self.data[b] for b in m.blocks)

    def has_object(self, object_id: str) -> bool:
        m = self.objects.get(object_id)
        return m is not None and all(b in self.data for b in m.blocks)

    # BlockSource
    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        def run() -> list[ObjectManifest]:
            try:
                return [self.objects[i] for i in object_ids]
            except KeyError as exc:
                raise NotFound(f"object {exc.args[0]} unknown") from None

        return self.world.around(self.side, "manifests", run)

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        def run() -> list[tuple[str, bytes]]:
            self.read_calls.append(len(ids))
            return [(i, self.data[i]) for i in ids if i in self.data]

        return iter(self.world.around(self.side, "read_blocks", run))

    # BlockSink
    def put_manifests(self, manifests: Sequence[ObjectManifest]) -> int:
        def run() -> int:
            new = 0
            for m in manifests:
                if m.object_id not in self.objects:
                    new += 1
                self.objects[m.object_id] = m
            return new

        return self.world.around(self.side, "put_manifests", run)

    def missing(self, ids: Sequence[str]) -> list[str]:
        return self.world.around(self.side, "missing", lambda: [i for i in ids if i not in self.data])

    def write_blocks(self, frames: Iterable[tuple[str, bytes]]) -> tuple[int, list[tuple[str, str]]]:
        frames = list(frames)

        def run() -> tuple[int, list[tuple[str, str]]]:
            body = sum(len(raw) for _, raw in frames)
            if self.max_body is not None and body > self.max_body:
                raise PayloadTooLarge("body over limit", limit=self.max_body)
            if self.write_delay_s:
                time.sleep(self.write_delay_s)
            self.write_calls.append(body)
            ok, rejected = 0, []
            for bid, raw in frames:
                if _sha(raw) != bid:
                    rejected.append((bid, "block_corrupt"))
                    continue
                self.data[bid] = raw
                ok += 1
            return ok, rejected

        with self._lock:
            self.inflight += 1
            self.max_inflight = max(self.max_inflight, self.inflight)
        try:
            return self.world.around(self.side, "write_blocks", run)
        finally:
            with self._lock:
                self.inflight -= 1


# ---------------------------------------------------------------------------------------------------------
# participants
# ---------------------------------------------------------------------------------------------------------


class _Participant:
    side = ""

    def __init__(self, world: World) -> None:
        self.world = world
        self.max_epoch = 0
        self.ops: dict[str, FakeOp] = {}
        self.by_digest: dict[tuple[str, str], str] = {}
        self.last_error: dict | None = None
        self.notes: list[str] = []
        self.hosted = False

    def _epoch(self, job) -> None:
        if job.epoch < self.max_epoch:
            raise InvalidState("stale epoch", reason="stale_epoch", epoch=job.epoch, current=self.max_epoch)
        self.max_epoch = job.epoch

    def _call(self, name: str, job, fn: Callable[[], object]) -> object:
        self.world.log.append((self.side, name))

        def body() -> object:
            if job is not None:
                self._epoch(job)
            return fn()

        return self.world.around(self.side, name, body)

    def _start(self, name: str, digest: str, complete: Callable[[], dict], polls: int | None = None) -> OpStatus:
        key = (name, digest)
        op_id = self.by_digest.get(key)
        if op_id is not None:
            op = self.ops[op_id]
            retry = op.state is OpState.FAILED and (bool((op.error or {}).get("retryable")) or name in RESTARTABLE_OPS)
            if not retry:
                return op.status()
            op.state, op.error, op.result = OpState.RUNNING, None, None
            op.polls_left = self.world.op_polls.get(name, 1)
            op.complete = complete
            return op.status()
        op_id = f"{self.side}-{name}-{next(self.world.counter)}"
        op = FakeOp(op_id, name, digest, complete, self.world.op_polls.get(name, 1) if polls is None else polls)
        self.ops[op_id] = op
        self.by_digest[key] = op_id
        self.world.started.append((self.side, name))
        return op.status()

    def _advance(self, op: FakeOp) -> None:
        if op.state is not OpState.RUNNING:
            return
        late = self.world._take_late(self.side, op.name)
        if late:
            op.polls_left += late
        op.polls_left -= 1
        if op.polls_left > 0:
            return
        fault = self.world._match(self.side, "complete:" + op.name)
        try:
            if fault is not None and not fault.after:
                fault.fire(self.world)
            op.result = dict(op.complete())
            op.state = OpState.DONE
            if fault is not None and fault.after:
                fault.fire(self.world)
        except HandoverError as exc:
            op.state = OpState.FAILED
            op.error = exc.to_wire()["error"]
            self.last_error = op.error
            self.on_op_failed(op)

    def on_op_failed(self, op: FakeOp) -> None:
        """Participant reaction to a failed operation (the target rolls a failed commit back)."""

    def tick(self) -> None:
        """Time passes on the participant: every running operation advances by one poll."""
        for op in list(self.ops.values()):
            self._advance(op)

    def op_status(self, job, op_id: str) -> OpStatus:
        def run() -> OpStatus:
            op = self.ops.get(op_id)
            if op is None:
                raise NotFound(f"op {op_id}")
            self._advance(op)
            return op.status()

        return self._call("op_status", job, run)  # type: ignore[return-value]

    def interrupt(self) -> None:
        for op in self.ops.values():
            if op.state is OpState.RUNNING:
                op.state = OpState.FAILED
                op.error = {
                    "code": "internal",
                    "message": "operation interrupted (process restart)",
                    "retryable": True,
                    "details": {"reason": "interrupted"},
                }

    def hello(self) -> Hello:
        return self._call("hello", None, self._hello)  # type: ignore[return-value]

    def _hello(self) -> Hello:
        return Hello(
            protocol=PROTOCOL,
            majors=(1,),
            minor=0,
            instance_id=self.side.upper(),
            listener_pubkey="",
            capabilities=self.world.capabilities.get(self.side, CAPABILITIES_V1),
            limits=Limits(hosted=self.hosted, max_request_bytes=self.world.max_request_bytes),
        )


class FakeSource(_Participant):
    """Source participant: session rows, leases, primary lock (DESIGN §3.3, §7)."""

    side = "source"

    def __init__(self, world: World) -> None:
        super().__init__(world)
        self.rows: list[str] = [f"row-{i}" for i in range(5)]
        self.rev = 1  # bumps on every change
        self.rewrites = 0  # bumps on non-append changes
        self.worktree = 1
        self.state: str | None = None
        self.job_id: str | None = None
        self.leases = False
        self.primary_lock = False
        self.veto = False
        self.busy = False
        self.busy_until_polls = 0
        self.lease_lost = False
        self.exported_rows: list[str] | None = None
        self.export_rev = None
        self.rows_at_prepare: list[str] | None = None
        self.pack_worktree = None
        self.events: list[str] = []
        self.retire_ack: CommitAck | None = None
        self.items = [make_item("w0", "/srv/app"), make_item("x1", "/srv/notes.txt")]
        self.blocks_store = FakeBlocks(world, "source_blocks", world.block_size)
        self.list_calls = 0

    # world actions -------------------------------------------------------------------------------------------
    def append_rows(self, *rows: str) -> None:
        self.rows.extend(rows)
        self.rev += 1

    def rewrite(self) -> None:
        self.rows[0] = self.rows[0] + "-edited"
        self.rev += 1
        self.rewrites += 1

    def touch_worktree(self) -> None:
        self.worktree += 1

    @property
    def usable(self) -> bool:
        return not self.primary_lock

    def revision(self) -> tuple:
        return (self.rev, self.rewrites, len(self.rows))

    def reclaim(self, *, force: bool = False) -> None:
        self.world.reclaimed = True
        self.primary_lock = self.veto = self.leases = False
        self.state = "reclaimed"

    # SourceAPI -----------------------------------------------------------------------------------------------
    def list_sessions(self, limit: int, query: str | None, include_archived: bool) -> list[dict]:
        self.list_calls += 1
        return [{"id": "tip", "title": "T"}]

    def facts(self, job) -> EnvFacts:
        return self._call("facts", job, lambda: make_facts("server"))  # type: ignore[return-value]

    def start_plan(self, job, target_facts: EnvFacts, session_ref: SessionRef) -> OpStatus:
        def complete() -> dict:
            plan = ResourcePlan(
                job_id=job.job_id,
                revision=self.revision(),
                items=tuple(self.items),
                not_transferred=(),
                processes=(),
                delegations=(),
                cron=(),
                refine=RefineReport(used=job.options.refine),
                tools_needed=(),
                env_vars_needed=(),
                notes_for_target="",
                approx_tokens=1000,
            )
            return to_wire(plan)

        digest = f"{self.revision()}:{job.options.refine}:{len(self.items)}"
        return self._call("start_plan", job, lambda: self._start("plan", digest, complete))  # type: ignore[return-value]

    def _info(self, plan_revision: tuple) -> SessionInfo:
        return SessionInfo(
            tip_id="tip",
            root_id="root",
            lineage_ids=("root", "tip"),
            title="T",
            model="m",
            provider="p",
            cwd="/srv/app",
            git_repo_root=None,
            git_branch=None,
            active_messages=len(self.rows),
            revision=self.revision(),
            revision_changed=tuple(plan_revision) != self.revision(),
        )

    def lock(self, job, session_ref: SessionRef, plan_revision: tuple) -> SessionInfo:
        def run() -> SessionInfo:
            if self.state in ("locked", "locking") and self.job_id == job.job_id:
                self.leases = True
                return self._info(plan_revision)
            if self.state not in (None, "open", "suspended", "released"):
                raise InvalidState(f"source is {self.state}", reason="state", state=self.state)
            if self.busy:
                raise SessionBusy("A turn is running in this session.")
            self.state, self.job_id, self.leases, self.lease_lost = "locked", job.job_id, True, False
            self.events.append("lock")
            return self._info(plan_revision)

        return self._call("lock", job, run)  # type: ignore[return-value]

    def heartbeat(self, job) -> None:
        def run() -> None:
            self.world.heartbeats += 1
            if self.lease_lost or (self.state in ("locked", "preparing") and not self.leases):
                raise LeaseLost("lease lost")

        self._call("heartbeat", job, run)

    def start_export(self, job) -> OpStatus:
        def complete() -> dict:
            rows = list(self.rows)
            page = self.blocks_store.put_doc({"rows": rows})
            seg = self.blocks_store.put_doc({"id": "tip", "kind": "row"})
            capsule = self.blocks_store.put_doc(
                {
                    "kind": "ctxh.capsule",
                    "v": 1,
                    "job_id": job.job_id,
                    "segments": [{"row_object": seg.object_id, "pages": [page.object_id], "rows": len(rows)}],
                    "hashes": {"h_all": rows_hash(rows)},
                }
            )
            self.exported_rows, self.export_rev = rows, self.revision()
            info = CapsuleInfo(
                capsule_id=capsule.object_id,
                size=capsule.size,
                segments=1,
                rows=len(rows),
                max_segment_bytes=page.size,
                approx_tokens=len(rows) * 10,
                h_all=rows_hash(rows),
                h_rows=rows_hash(rows),
                h_model=rows_hash(rows),
                revision=self.revision(),
            )
            return to_wire(info)

        return self._call("start_export", job, lambda: self._start("export", str(self.revision()), complete))  # type: ignore[return-value]

    def start_pack(self, job, final_plan: FinalPlan) -> OpStatus:
        def complete() -> dict:
            items = []
            for it in final_plan.plan.items:
                if it.include is not Include.YES:
                    continue
                obj = self.blocks_store.put_object(
                    f"{it.source_path}@{self.worktree}".encode() * 20, "application/octet-stream"
                )
                tree = self.blocks_store.put_doc(
                    {"kind": "ctxh.tree", "v": 1, "entries": [{"path": "f", "type": "file", "object": obj.object_id}]}
                )
                items.append({"item_id": it.item_id, "kind": "file_tree", "manifest": tree.object_id})
            pack = self.blocks_store.put_doc(
                {"kind": "ctxh.pack", "v": 1, "job_id": job.job_id, "final_plan_id": final_plan.plan_id, "items": items}
            )
            self.packed_plan = final_plan
            self.pack_worktree = self.worktree
            return to_wire(PackInfo(pack_id=pack.object_id, size=pack.size))

        digest = f"{final_plan.plan_id}:{self.worktree}"
        return self._call("start_pack", job, lambda: self._start("pack", digest, complete))  # type: ignore[return-value]

    def start_prepare(self, job, prepared: PreparedInfo) -> OpStatus:
        def complete() -> dict:
            if self.state == "prepared":
                return to_wire(PrepareResult(outcome=PrepareOutcome.OK))
            if self.state != "locked":
                raise InvalidState(f"source is {self.state}", reason="state", state=self.state)
            if self.lease_lost or not self.leases:
                raise LeaseLost("lease lost before prepare")
            exported = self.exported_rows or []
            if self.rows != exported:
                if self.rewrites == (self.export_rev or (0, 0, 0))[1] and self.rows[: len(exported)] == exported:
                    new = self.rows[len(exported) :]
                    obj = self.blocks_store.put_doc({"kind": "ctxh.delta", "v": 1, "rows": new})
                    delta = DeltaInfo(
                        delta_id=obj.object_id,
                        tip_id="tip",
                        rows=len(new),
                        h_all=rows_hash(self.rows),
                        h_rows=rows_hash(self.rows),
                        h_model=rows_hash(self.rows),
                        revision=self.revision(),
                    )
                    self.exported_rows, self.export_rev = list(self.rows), self.revision()
                    return to_wire(PrepareResult(outcome=PrepareOutcome.DELTA, delta=delta))
                return to_wire(PrepareResult(outcome=PrepareOutcome.STALE_SESSION))
            if self.pack_worktree != self.worktree:
                return to_wire(PrepareResult(outcome=PrepareOutcome.STALE_WORKTREE))
            if prepared.h_all != rows_hash(self.rows):
                raise VerifyFailed("target holds other rows than the source", which="h_all")
            self.state = "prepared"
            self.events += ["veto", "primary_lock"]
            self.veto = self.primary_lock = True
            self.rows_at_prepare = list(self.rows)
            return to_wire(PrepareResult(outcome=PrepareOutcome.OK))

        digest = f"{prepared.h_all}:{self.revision()}:{self.worktree}"
        return self._call("start_prepare", job, lambda: self._start("prepare", digest, complete))  # type: ignore[return-value]

    def start_retire(self, job, ack: CommitAck) -> OpStatus:
        def complete() -> dict:
            if self.state in ("released", "reclaimed"):
                raise InvalidState(f"source is {self.state}", reason="state", state=self.state)
            if self.state not in ("prepared", "retiring", "retired"):
                raise InvalidState(f"source is {self.state}", reason="state", state=self.state)
            assert self.veto and self.primary_lock, "R1: lock before releasing the lease"
            self.state = "retiring"
            self.retire_ack = ack
            self.events.append("release_lease")
            self.leases = False
            self.state = "retired"
            return {"state": "retired"}

        return self._call("start_retire", job, lambda: self._start("retire", "retire", complete))  # type: ignore[return-value]

    def start_release(self, job, reason: str) -> OpStatus:
        def complete() -> dict:
            if self.state in ("retired", "exported"):
                raise InvalidState(f"source is {self.state}", reason="state", state=self.state)
            if self.state in ("released", "reclaimed", None, "open"):
                self.state = self.state if self.state in ("released", "reclaimed") else "released"
                return {"state": self.state}
            self.primary_lock = self.veto = self.leases = False
            self.events.append("release")
            self.state = "released"
            return {"state": "released"}

        return self._call("start_release", job, lambda: self._start("release", "release", complete))  # type: ignore[return-value]

    def status(self, job) -> ParticipantStatus:
        def run() -> ParticipantStatus:
            self.tick()
            ops = {op.name: op.status() for op in self.ops.values()}
            return ParticipantStatus(
                state=self.state or "open",
                step="",
                epoch=self.max_epoch,
                progress=None,
                ops=ops,
                last_error=self.last_error,
                notes=tuple(self.notes),
            )

        return self._call("status", job, run)  # type: ignore[return-value]

    def blocks(self) -> FakeBlocks:
        return self.blocks_store


class FakeTarget(_Participant):
    """Target participant: staging, hidden import, async commit with rollback (DESIGN §3.3, §3.4)."""

    side = "target"

    def __init__(self, world: World) -> None:
        super().__init__(world)
        self.state: str | None = None
        self.free = 10 * 1024**3
        self.hidden_rows: list[str] | None = None
        self.capsule_id: str | None = None
        self.pack_id: str | None = None
        self.final: FinalPlan | None = None
        self.active = False
        self.materialised: list[str] = []
        self.commit_fail: HandoverError | None = None
        self.continued: list[str] = []
        self.blocks_store = FakeBlocks(world, "target_blocks", world.block_size)
        self.prepared: PreparedInfo | None = None
        self.ack: CommitAck | None = None
        self.pack_items: list[str] = []

    def open_job(self, job) -> None:
        def run() -> None:
            if self.state is None:
                self.state = "open"

        self._call("open_job", job, run)

    def facts(self, job, source_facts: EnvFacts) -> EnvFacts:
        return self._call("facts", job, lambda: make_facts("laptop", free=self.free))  # type: ignore[return-value]

    def negotiate(self, job, plan: ResourcePlan) -> FinalPlan:
        def run() -> FinalPlan:
            decisions = tuple(
                TargetDecision(
                    item_id=it.item_id,
                    dest="/laptop" + it.source_path,
                    mode=DestMode.FRESH,
                    reason="test",
                    need_bytes=it.size_estimate,
                )
                for it in plan.items
                if it.include is not Include.NO
            )
            need = sum(d.need_bytes for d in decisions)
            if self.free < need * 1.1:
                raise InsufficientStorage("not enough space", need_bytes=need, free_bytes=self.free, path="/")
            final = planning.with_plan_id(
                FinalPlan(
                    plan_id="",
                    plan=plan,
                    decisions=decisions,
                    need_bytes=need,
                    free_bytes={"/": self.free},
                    model_change=None,
                )
            )
            self.final = final
            if self.state in (None, "open"):
                self.state = "negotiated"
            return final

        return self._call("negotiate", job, run)  # type: ignore[return-value]

    def _require_closure(self, object_id: str) -> dict:
        store = self.blocks_store
        if not store.has_object(object_id):
            raise NotFound(f"object {object_id[:12]} not staged")
        doc = json.loads(store.read_object(object_id))
        refs = []
        for seg in doc.get("segments", []):
            refs += [seg["row_object"], *seg["pages"]]
        for item in doc.get("items", []):
            refs.append(item["manifest"])
            tree = json.loads(store.read_object(item["manifest"])) if store.has_object(item["manifest"]) else None
            if tree is None:
                raise NotFound("tree manifest not staged")
            refs += [e["object"] for e in tree["entries"]]
        for ref in refs:
            if not store.has_object(ref):
                raise NotFound(f"object {ref[:12]} not staged")
        return doc

    def start_stage_context(self, job, capsule: CapsuleInfo) -> OpStatus:
        def complete() -> dict:
            self._require_closure(capsule.capsule_id)
            self.capsule_id = capsule.capsule_id
            if self.state in ("open", "negotiated", "resetting", "context_staged", "data_staged"):
                self.state = "context_staged"
            return {"segments": capsule.segments, "rows": capsule.rows, "bytes": capsule.size, "import_path": "A"}

        return self._call(
            "start_stage_context", job, lambda: self._start("stage_context", capsule.capsule_id, complete)
        )  # type: ignore[return-value]

    def _rows_of(self, capsule_id: str) -> list[str]:
        doc = json.loads(self.blocks_store.read_object(capsule_id))
        rows: list[str] = []
        for seg in doc["segments"]:
            for page in seg["pages"]:
                rows += json.loads(self.blocks_store.read_object(page))["rows"]
        if rows_hash(rows) != doc["hashes"]["h_all"]:
            raise VerifyFailed("capsule rows do not match h_all")
        return rows

    def start_prepare(self, job, req: PrepareRequest) -> OpStatus:
        def complete() -> dict:
            if self.state in ("committing", "committed", "aborted", "aborting", "expired"):
                raise InvalidState(f"target is {self.state}", reason="state", state=self.state)
            self._require_closure(req.capsule.capsule_id)
            pack_doc = self._require_closure(req.pack.pack_id)
            if pack_doc.get("final_plan_id") != req.final_plan_id:
                raise VerifyFailed("pack does not belong to the confirmed final plan", which="final_plan_id")
            self.pack_items = [i["item_id"] for i in pack_doc["items"]]
            rows = self._rows_of(req.capsule.capsule_id)
            if rows_hash(rows) != req.capsule.h_all:
                raise VerifyFailed("h_all mismatch after import", which="h_all")
            self.hidden_rows = rows
            self.pack_id = req.pack.pack_id
            self.state = "prepared"
            self.prepared = PreparedInfo(
                tip_id="new-tip",
                root_id="new-root",
                title="T",
                h_all=rows_hash(rows),
                h_rows=rows_hash(rows),
                h_model=rows_hash(rows),
                import_path="A",
                dests=tuple((d.item_id, d.dest, d.mode) for d in (self.final.decisions if self.final else ())),
                warnings=(),
                source_revision=req.capsule.revision,
            )
            return to_wire(self.prepared)

        digest = f"{req.capsule.capsule_id}:{req.pack.pack_id}"
        return self._call("start_prepare", job, lambda: self._start("prepare", digest, complete))  # type: ignore[return-value]

    def start_apply_delta(self, job, delta: DeltaInfo) -> OpStatus:
        def complete() -> dict:
            if self.state != "prepared" or self.hidden_rows is None or self.prepared is None:
                raise InvalidState(f"target is {self.state}", reason="state", state=self.state)
            if not self.blocks_store.has_object(delta.delta_id):
                raise NotFound("delta not staged")
            new = json.loads(self.blocks_store.read_object(delta.delta_id))["rows"]
            if self.prepared.source_revision != delta.revision:
                self.hidden_rows = self.hidden_rows + new
            if rows_hash(self.hidden_rows) != delta.h_all:
                raise VerifyFailed("h_all mismatch after delta", which="h_all")
            self.prepared = replace(
                self.prepared,
                h_all=delta.h_all,
                h_rows=delta.h_rows,
                h_model=delta.h_model,
                source_revision=delta.revision,
            )
            return to_wire(self.prepared)

        return self._call("start_apply_delta", job, lambda: self._start("apply_delta", delta.delta_id, complete))  # type: ignore[return-value]

    def start_reset_session(self, job) -> OpStatus:
        def complete() -> dict:
            if self.state in ("committing", "committed"):
                raise InvalidState(f"target is {self.state}", reason="state", state=self.state)
            self.hidden_rows = None
            self.prepared = None
            self.state = "context_staged"
            return {}

        return self._call(
            "start_reset_session", job, lambda: self._start("reset_session", str(self.capsule_id), complete)
        )  # type: ignore[return-value]

    def start_commit(self, job) -> OpStatus:
        def complete() -> dict:
            if self.state == "committed" and self.ack is not None:
                return to_wire(self.ack)
            if self.commit_fail is not None:
                self.state = "aborted"
                self.hidden_rows = None
                self.materialised = []
                raise self.commit_fail
            self.materialised = list(self.pack_items)
            self.active = True
            self.state = "committed"
            self.ack = CommitAck(
                session_id="new-tip",
                root_id="new-root",
                title="T",
                dests=self.prepared.dests if self.prepared else (),
                path_map=(PathMapEntry("/srv/app", "/laptop/srv/app", "workspace"),),
                model="m",
                offline_capable=True,
                warnings=(),
                committed_at=self.world.clock(),
            )
            return to_wire(self.ack)

        def run() -> OpStatus:
            if self.state not in ("prepared", "committing", "committed"):
                raise InvalidState(f"target is {self.state}", reason="state", state=self.state)
            if self.state == "prepared":
                self.state = "committing"
            return self._start("commit", "commit", complete)

        return self._call("start_commit", job, run)  # type: ignore[return-value]

    def continue_session(self, job, prompt: str) -> ContinueResult:
        def run() -> ContinueResult:
            self.continued.append(prompt)
            return ContinueResult(started=True, pid=4242, log="/x.log")

        return self._call("continue_session", job, run)  # type: ignore[return-value]

    def abort(self, job, reason: str) -> None:
        def run() -> None:
            if self.state in ("committing", "committed"):
                raise InvalidState(f"target is {self.state}", reason="state", state=self.state)
            self.hidden_rows = None
            self.materialised = []
            if self.state not in ("expired",):
                self.state = "aborted"

        self._call("abort", job, run)

    def status(self, job) -> ParticipantStatus:
        def run() -> ParticipantStatus:
            self.tick()
            ops = {op.name: op.status() for op in self.ops.values()}
            return ParticipantStatus(
                state=self.state or "open",
                step="",
                epoch=self.max_epoch,
                progress=None,
                ops=ops,
                last_error=self.last_error,
                notes=tuple(self.notes),
            )

        return self._call("status", job, run)  # type: ignore[return-value]

    def on_op_failed(self, op: FakeOp) -> None:
        if op.name == "commit" and self.state == "committing":
            self.state = "aborted"  # the commit op rolled back completely (DESIGN §3.4)
            self.hidden_rows = None
            self.materialised = []
            self.active = False

    def tick(self) -> None:
        commit = next((o for o in self.ops.values() if o.name == "commit"), None)
        if (
            commit is not None
            and self.state == "committing"
            and commit.state is OpState.FAILED
            and (commit.error or {}).get("details", {}).get("reason") == "interrupted"
        ):
            # the target's janitor continues the commit op in its journaled direction (DESIGN §3.6)
            commit.state, commit.error = OpState.RUNNING, None
            commit.polls_left = self.world.op_polls.get("commit", 1)
        super().tick()

    def expire(self) -> None:
        self.hidden_rows = None
        self.state = "expired"

    def blocks(self) -> FakeBlocks:
        return self.blocks_store


# ---------------------------------------------------------------------------------------------------------
# the world
# ---------------------------------------------------------------------------------------------------------


class World:
    """Both participants, their faults and the invariant monitor (see module docstring)."""

    def __init__(self, clock, *, block_size: int = OBJECT_BLOCK_SIZE, max_request_bytes: int = 8 * 1024**2) -> None:
        self.clock = clock
        self.block_size = block_size
        self.max_request_bytes = max_request_bytes
        self.counter = itertools.count(1)
        self.log: list[tuple[str, str]] = []
        self.started: list[tuple[str, str]] = []
        self.faults: list[Fault] = []
        self.violations: list[str] = []
        self.reclaimed = False
        self.op_polls: dict[str, int] = {}
        self.capabilities: dict[str, frozenset[str]] = {}
        self.heartbeats = 0
        self.slept = 0.0
        self._lock = threading.RLock()
        self.source = FakeSource(self)
        self.target = FakeTarget(self)

    # faults ------------------------------------------------------------------------------------------------
    def inject(self, side: str, method: str, kind: str, **kw) -> Fault:
        fault = Fault(side=side, method=method, kind=kind, **kw)
        self.faults.append(fault)
        return fault

    def _match(self, side: str, method: str) -> Fault | None:
        with self._lock:
            for f in self.faults:
                if f.side == side and f.method == method and f.kind != "late" and f.fired < f.times + f.skip:
                    f.fired += 1
                    if f.fired <= f.skip:
                        return None
                    return f
        return None

    def _take_late(self, side: str, op_name: str) -> int:
        with self._lock:
            for f in self.faults:
                if f.side == side and f.method == "complete:" + op_name and f.kind == "late" and f.fired < f.times:
                    f.fired += 1
                    return f.polls
        return 0

    def around(self, side: str, method: str, fn: Callable[[], object]):
        """Run ``fn`` with the faults injected for ``side.method`` (before / after) and check the invariant."""
        fault = self._match(side, method)
        if fault is not None and not fault.after:
            fault.fire(self)
        result = fn()
        self.check()
        if fault is not None and fault.after:
            fault.fire(self)
            self.check()
        return result

    def interrupt(self, side: str) -> None:
        (self.source if side == "source" else self.target).interrupt()

    # time ----------------------------------------------------------------------------------------------------
    def sleep(self, seconds: float) -> None:
        self.slept += float(seconds)
        self.clock.advance(float(seconds))

    # invariants ------------------------------------------------------------------------------------------------
    def check(self) -> None:
        if self.target.active and self.source.usable and not self.reclaimed:
            self.violations.append(
                f"two active copies: target {self.target.state}, source {self.source.state} (after {self.log[-1]})"
            )

    def assert_outcome(self, final_state: str) -> None:
        """The two promises: success ⇒ complete at the target; failure ⇒ source unchanged and usable."""
        assert self.violations == [], self.violations
        if final_state == "done":
            assert self.target.active and self.target.state == "committed"
            assert self.source.state == "retired" and self.source.primary_lock and not self.source.leases
            assert self.target.hidden_rows == self.source.rows_at_prepare, "target incomplete"
            confirmed = [i.item_id for i in self.source.packed_plan.plan.items if i.include is Include.YES]
            assert self.target.materialised == confirmed
            assert self.source.events.index("primary_lock") < self.source.events.index("release_lease")
        elif final_state == "aborted":
            assert not self.target.active, "target copy active after abort"
            assert self.source.usable and not self.source.leases and not self.source.veto
            assert self.source.state in (None, "open", "released", "reclaimed")
            assert self.target.hidden_rows is None or self.target.state not in ("committed",)
        else:  # pragma: no cover - test helper misuse
            raise AssertionError(final_state)


def decode(cls, data: dict):
    return from_wire(cls, data)


# ---------------------------------------------------------------------------------------------------------
# planning port and reporter used with the conductor
# ---------------------------------------------------------------------------------------------------------


class FakePlanning:
    """The ``plan`` functions the conductor calls (cluster D, INTERFACES §4.2), recorded in ``calls``.

    Every function is the real one of ``engine.plan`` except ``confirmation_reasons``: the World's items are
    ``extra`` items outside the workspace, so D's rule (f) would ask in every test; the fault matrix exercises the
    state machine, so only (a) ``proposed``, (b) ``conflict`` and (g) ``instruction_files`` are raised here."""

    def __init__(self) -> None:
        self.calls: list[str] = []

    def confirmation_reasons(
        self, final: FinalPlan, options, source_offline: bool, *, instructions: Sequence[dict] = (), cfg=None
    ) -> list[str]:
        self.calls.append("confirmation_reasons")
        reasons = []
        if any(i.include is Include.PROPOSED for i in final.plan.items):
            reasons.append(planning.CONFIRM_PROPOSED)
        if any(d.conflict is not None for d in final.decisions):
            reasons.append(planning.CONFIRM_CONFLICT)
        if instructions:
            reasons.append(planning.CONFIRM_INSTRUCTIONS)
        return reasons

    def apply_choice(self, final: FinalPlan, choice: str) -> FinalPlan:
        self.calls.append("apply_choice:" + choice)
        return planning.apply_choice(final, choice)

    def plan_id(self, final_plan_doc: dict) -> str:
        self.calls.append("plan_id")
        return planning.plan_id(final_plan_doc)

    def presend_summary(self, final: FinalPlan, **kw) -> dict:
        self.calls.append("presend_summary")
        return planning.presend_summary(final, **kw)

    def instruction_files(self, items, dest_lookup) -> list[dict]:
        self.calls.append("instruction_files")
        return planning.instruction_files(items, dest_lookup)

    def note_parts_from(self, final, **kw) -> dict:
        self.calls.append("note_parts_from")
        return planning.note_parts_from(final, **kw)


@dataclass
class RecordingReporter:
    progress_events: list = field(default_factory=list)
    infos: list[str] = field(default_factory=list)
    warns: list[str] = field(default_factory=list)

    def progress(self, p) -> None:
        self.progress_events.append(p)

    def info(self, text: str) -> None:
        self.infos.append(text)

    def warn(self, text: str) -> None:
        self.warns.append(text)
