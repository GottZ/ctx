"""The conductor: drives one handover job against a ``SourceAPI`` and a ``TargetAPI`` (layer L7, stdlib only;
DESIGN §3, INTERFACES §4.5).

The conductor runs in the dialer's process. It is a state machine over ``ConductorState`` (DESIGN §3.2) whose
every transition is a compare-and-set on state and owner epoch in the ledger, and whose every participant side
effect is journaled as ``intent`` before and ``done`` after the call. Its working data (facts, plans, capsule,
pack, prepared info, ACK) lives in the journal (``ctx`` entries of the events), so another process can adopt a
stalled job and continue from the journal alone (``recover``/``resume``).

The two promises of the plugin are carried here:

* **Complete at the target or nothing at the target.** The target activates only in its commit operation
  (two-phase commit, §3.4); the conductor journals ``intent: commit`` (serialised with cancel) before it starts
  that operation, and before that point every failure aborts: the target discards its staging, the source is
  released and is unchanged.
* **The source is never damaged and never left locked by a failure, and two copies are never active
  unnoticed.** The source is locked only after the user confirmed the plan, prepared (primary lock) only after
  the target voted prepared, and retired only after the target's ACK. A commit whose outcome is unknown is
  ``IN_DOUBT``: the conductor polls the target until it reports a terminal state and never releases the source
  while the target says ``committing``. After the commit point it only drives forward (retire with retries); a
  pull whose remote source cannot be reached stays ``RETIRING`` and the janitor retries — the remote source keeps
  its primary lock meanwhile (fail-closed).

Transcript rows appended during the handover come back from the source's prepare re-check as an append-only
``delta`` (applied to the hidden import, re-verified); any other change restarts the export
(``TARGET_RESETTING``) and a changed work tree re-packs, both bounded by the restart counter (≤ 3) [R3, SK H2].

Planning helpers of cluster D (confirmation reasons, choices, pre-send summary, plan id, instruction files, note
parts) are reached through a ``planning`` port (default: the real ``plan`` module, imported lazily); a fake must
offer the same functions. The confirmed FinalPlan is what the whole job uses: ``plan.apply_choice`` runs on every
``yes``/``no_proposed`` (R25), and when that changed the plan the target negotiates it again, so the target holds
exactly the plan the source packs (contract X02, X06). An import takes the bundle's FinalPlan over instead of
negotiating a new one (X02).

Every object the conductor reads from a source goes through ``VerifiedBlocks``: each manifest is checked against
the id it was asked for (contract K-1, ``jsonutil.check_object_manifest``) before a document is parsed or a block
is moved.
"""

from __future__ import annotations

import collections
import contextlib
import dataclasses
import threading
import time
from collections.abc import Callable, Sequence
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, replace

from . import WIRE_MAJOR
from .contracts import BlockSink, BlockSource, JobContext, Reporter, SourceAPI, TargetAPI
from .errors import (
    BadRequest,
    BlockCorrupt,
    Cancelled,
    CapabilityMissing,
    HandoverError,
    InDoubt,
    InvalidState,
    LeaseLost,
    NotFound,
    PayloadTooLarge,
    PeerUnreachable,
    RateLimited,
    RestartLimit,
    SessionBusy,
    StaleSession,
    VerifyFailed,
    VersionUnsupported,
    WaitTimeout,
    error_class,
)
from .jsonutil import (
    check_object_manifest,
    from_wire,
    loads_bounded,
    object_manifest_doc,
    sha256_hex,
    to_wire,
)
from .ledger import Fenced, Ledger, process_owner
from .local_source import LocalSource
from .model import (
    CapsuleInfo,
    CommitAck,
    ConductorState,
    ContinueResult,
    DeltaInfo,
    EnvFacts,
    FinalPlan,
    Hello,
    Include,
    Mode,
    ObjectManifest,
    OpState,
    OpStatus,
    PackInfo,
    PreparedInfo,
    PrepareOutcome,
    PrepareRequest,
    PrepareResult,
    Progress,
    ResourcePlan,
    Role,
    SessionInfo,
    SessionRef,
)
from .ops import object_closure, read_object
from .plan import CONFIRM_PROPOSED, SUSPECTED_SECRET

S = ConductorState
MiB = 1024 * 1024

STOP_STATES = frozenset({S.DONE, S.ABORTED, S.SUSPENDED})
PLANNING_STATES = frozenset({S.CREATED, S.HELLO, S.FACTS, S.PLANNING, S.NEGOTIATED})
SUSPENDABLE = frozenset(
    {
        S.LOCKING,
        S.LOCKED,
        S.EXPORTING,
        S.CONTEXT_STAGED,
        S.PACKING,
        S.DATA_STAGED,
        S.TARGET_PREPARING,
        S.TARGET_PREPARED,
    }
)
PREPARE_PHASE = frozenset({S.SOURCE_PREPARING, S.DELTA_APPLYING, S.TARGET_RESETTING, S.SOURCE_PREPARED})
CANCELLABLE = PLANNING_STATES | SUSPENDABLE | PREPARE_PHASE
LEASE_STATES = (SUSPENDABLE - {S.LOCKING}) | PREPARE_PHASE | {S.COMMITTING, S.IN_DOUBT, S.COMMITTED, S.RETIRING}
FORWARD_STATES = frozenset({S.COMMITTING, S.IN_DOUBT, S.COMMITTED, S.RETIRING, S.RETIRED, S.CONTINUING})

REQUIRED_CAPS = ("session.v1", "blocks.v1")
ANSWERS = frozenset({"yes", "no_proposed", "show_plan", "cancel"})
"""The answers ``confirm(info)`` may give (DESIGN §14.1, W112); anything else ends the job like ``cancel``."""


@dataclass(frozen=True)
class PollPolicy:
    """Timing of polls, retries and limits (DESIGN §3.2, §4.5, §4.6). Tests shrink nothing but the clock."""

    op_first_s: float = 1.0
    op_max_s: float = 5.0
    in_doubt_first_s: float = 1.0
    in_doubt_max_s: float = 300.0
    retry_attempts: int = 5
    retry_first_s: float = 0.5
    retry_max_s: float = 60.0
    retire_attempts: int = 8
    owner_heartbeat_s: float = 10.0
    wait_retry_s: float = 2.0
    max_restarts: int = 3
    max_deltas: int = 10
    max_doc_bytes: int = 64 * MiB
    request_bytes: int = 4 * MiB
    keepalive_s: float = 5.0


class _Park(Exception):
    """Stop the runner and leave the job in its state for the janitor (retire/abort retries exhausted)."""


def is_transient(err: BaseException) -> bool:
    """Transport-level failures worth repeating the same idempotent step for."""
    if isinstance(err, (PeerUnreachable, RateLimited, BlockCorrupt)):
        return True
    return type(err) is HandoverError and bool(err.retryable)


def error_from(err: dict | None) -> HandoverError:
    """Rebuild a ``HandoverError`` from an operation's error dict (our own participant or a decoded peer one)."""
    err = err or {}
    code = str(err.get("code") or "internal")
    klass = error_class(code) or HandoverError
    exc = klass(str(err.get("message") or code), details=dict(err.get("details") or {}))
    if isinstance(err.get("retryable"), bool):
        exc.retryable = err["retryable"]
    return exc


def _encode(value: object) -> object:
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return to_wire(value)
    return value


# ---------------------------------------------------------------------------------------------------------
# object closure and block mover (DESIGN §4.6)
# ---------------------------------------------------------------------------------------------------------


class VerifiedBlocks:
    """A ``BlockSource`` view that checks every manifest against the id it was asked for (contract K-1).

    Exactly one manifest per requested id, in request order; each must be a v1 ``ctxh.object`` manifest with block
    size 524 288 whose recomputed ``object_id`` equals the requested id (``jsonutil.check_object_manifest``:
    ``BadRequest`` for a malformed manifest, ``BlockCorrupt`` for an id mismatch). Blocks are passed through; the
    readers verify each block's sha256 and the content hash."""

    def __init__(self, src: BlockSource) -> None:
        self._src = src

    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        ids = list(object_ids)
        got = list(self._src.manifests(ids))
        if len(got) != len(ids):
            raise BlockCorrupt("the source returned another number of manifests", asked=len(ids), got=len(got))
        out: list[ObjectManifest] = []
        for oid, m in zip(ids, got, strict=True):
            check_object_manifest(object_manifest_doc(m.sha256, m.size, m.block_size, m.blocks, m.media), oid)
            out.append(m if m.object_id == oid else replace(m, object_id=oid))
        return out

    def read_blocks(self, ids: Sequence[str]):
        return self._src.read_blocks(ids)


def _block_sizes(m: ObjectManifest) -> list[tuple[str, int]]:
    n = len(m.blocks)
    bs = m.block_size or 524_288
    return [(b, bs if i < n - 1 else max(0, m.size - bs * (n - 1))) for i, b in enumerate(m.blocks)]


def move_object(
    src: BlockSource,
    dst: BlockSink,
    object_ids: Sequence[str],
    *,
    reporter: Reporter,
    cancelled: Callable[[], bool],
    hosted: bool,
    request_bytes: int = 4 * MiB,
    max_request_bytes: int | None = None,
    max_ids: int = 4096,
    on_batch: Callable[[], None] | None = None,
    prefetched: dict[str, bytes] | None = None,
    clock: Callable[[], float] = time.monotonic,
) -> None:
    """Move objects (manifests + missing blocks) from ``src`` to ``dst`` (DESIGN §4.6).

    Push and pull use the same loop: manifests first, then the need-set (blocks the sink does not have — verified
    blocks are never resent, I9), then batches of blocks bounded by the adaptive request size (413 halves down to
    one block, > 20 s halves, three batches < 5 s double up to the peer's maximum). With ``hosted`` one request is
    in flight; otherwise reading the next batch overlaps writing the previous one (two in flight)."""
    ids = list(dict.fromkeys(object_ids))
    manifests: list[ObjectManifest] = []
    for start in range(0, len(ids), 64):
        manifests.extend(src.manifests(ids[start : start + 64]))
    batch: list[ObjectManifest] = []
    budget = 0
    for m in manifests:
        cost = 256 + 70 * len(m.blocks)
        if batch and budget + cost > 2 * MiB:
            dst.put_manifests(batch)
            batch, budget = [], 0
        batch.append(m)
        budget += cost
    if batch:
        dst.put_manifests(batch)

    sizes: dict[str, int] = {}
    floor = 1
    for m in manifests:
        for bid, size in _block_sizes(m):
            sizes.setdefault(bid, size)
        floor = max(floor, m.block_size or 1)
    order = list(sizes)
    total = sum(sizes.values())
    missing: list[str] = []
    for start in range(0, len(order), max_ids):
        missing.extend(dst.missing(order[start : start + max_ids]))
    state = {
        "limit": max(floor, int(request_bytes)),
        "max": max(floor, int(max_request_bytes or request_bytes)),
        "fast": 0,
        "done": total - sum(sizes[b] for b in missing),
    }
    cache = prefetched or {}

    def report() -> None:
        reporter.progress(Progress(step="transfer", bytes_done=state["done"], bytes_total=total))

    report()
    inflight = 1 if hosted else 2
    for _round in range(4):
        if not missing:
            break
        retry: list[str] = []
        pending = collections.deque(missing)
        outstanding: collections.deque[tuple[Future, list[str], int, float]] = collections.deque()

        def settle(item: tuple[Future, list[str], int, float], retry: list[str] = retry) -> None:
            fut, ids_, nbytes, t0 = item
            try:
                _ok, rejected = fut.result()
            except PayloadTooLarge:
                if len(ids_) <= 1:  # one block does not fit: a proxy limits bodies below a block
                    raise
                state["limit"] = max(floor, min(state["limit"], nbytes) // 2)
                state["fast"] = 0
                retry.extend(ids_)
                return
            elapsed = clock() - t0
            if elapsed > 20:
                state["limit"] = max(floor, state["limit"] // 2)
                state["fast"] = 0
            elif elapsed < 5:
                state["fast"] += 1
                if state["fast"] >= 3:
                    state["limit"] = min(state["max"], state["limit"] * 2)
                    state["fast"] = 0
            bad = [bid for bid, _ in rejected]
            retry.extend(bad)
            state["done"] += nbytes - sum(sizes.get(b, 0) for b in bad)
            report()

        with ThreadPoolExecutor(max_workers=inflight, thread_name_prefix="ctxh-move") as pool:
            try:
                while pending:
                    if cancelled():
                        raise Cancelled("cancelled by the user")
                    if on_batch is not None:
                        on_batch()
                    take: list[str] = []
                    est = 0
                    while pending and len(take) < max_ids and (not take or est + sizes[pending[0]] <= state["limit"]):
                        bid = pending.popleft()
                        take.append(bid)
                        est += sizes[bid]
                    frames = [(b, cache[b]) for b in take if b in cache]
                    rest = [b for b in take if b not in cache]
                    if rest:
                        frames.extend((bid, raw) for bid, raw in src.read_blocks(rest) if sha256_hex(raw) == bid)
                    got = {bid for bid, _ in frames}
                    retry.extend(b for b in take if b not in got)
                    if not frames:
                        continue
                    while len(outstanding) >= inflight:
                        settle(outstanding.popleft())
                    nbytes = sum(len(raw) for _, raw in frames)
                    outstanding.append((pool.submit(dst.write_blocks, frames), [b for b, _ in frames], nbytes, clock()))
                while outstanding:
                    settle(outstanding.popleft())
            except BaseException:
                for fut, *_ in outstanding:
                    fut.cancel()
                raise
        missing = list(dict.fromkeys(retry))
    still: list[str] = []
    for start in range(0, len(order), max_ids):
        still.extend(dst.missing(order[start : start + max_ids]))
    if still:
        raise BlockCorrupt(f"{len(still)} blocks could not be transferred", missing=len(still))
    state["done"] = total
    report()


# ---------------------------------------------------------------------------------------------------------
# the conductor
# ---------------------------------------------------------------------------------------------------------


def suspected_secrets(final: FinalPlan) -> list[str]:
    """Source paths of proposals the policy flagged as suspected secrets (``plan.SUSPECTED_SECRET`` in reasons)."""
    return [i.source_path for i in final.plan.items if i.include is Include.PROPOSED and SUSPECTED_SECRET in i.reasons]


def _default_planning():
    from . import plan as planning

    return planning


class Conductor:
    """One job's state machine (see module docstring). Not thread-safe: one runner thread per job."""

    def __init__(
        self,
        ledger: Ledger,
        job: JobContext,
        source: SourceAPI,
        target: TargetAPI,
        *,
        confirm: Callable[[dict], str | None] | None,
        continue_prompt: str | None,
        planning=None,
        owner: str | None = None,
        clock: Callable[[], float] | None = None,
        sleep: Callable[[float], None] | None = None,
        poll: PollPolicy | None = None,
        keepalive: bool = True,
    ) -> None:
        self.ledger = ledger
        self.job = job
        self.source = source
        self.target = target
        self.confirm = confirm
        self.continue_prompt = continue_prompt
        self._planning = planning
        self.owner = owner or process_owner()
        self._clock = clock or time.time
        self._sleep_fn = sleep or time.sleep
        self.poll = poll or PollPolicy()
        self._keepalive = keepalive
        self._ctx: dict = {}
        self._state: S = S.CREATED
        self._attempts = 0
        self._fenced = False
        self._lease_lost = False
        self._beat_lock = threading.Lock()
        now = self._clock()
        self._last_owner_hb = now
        self._last_lease_hb = now
        self._refresh_s = max(5.0, float(job.options.lease_ttl_s) / 4.0)
        row = ledger.job(job.job_id, Role.CONDUCTOR)
        if row is None:
            raise NotFound(f"conductor job {job.job_id} not found")
        params = row["params"] or {}
        self._session_ref = from_wire(SessionRef, params.get("session_ref") or {})
        if self.continue_prompt is None:
            self.continue_prompt = params.get("continue_prompt")
        self._handlers: dict[S, Callable[[], None]] = {
            S.CREATED: self._created,
            S.HELLO: self._hello,
            S.FACTS: self._facts,
            S.PLANNING: self._planning_step,
            S.NEGOTIATED: self._negotiated,
            S.LOCKING: self._locking,
            S.LOCKED: self._locked,
            S.EXPORTING: self._exporting,
            S.CONTEXT_STAGED: self._context_staged,
            S.PACKING: self._packing,
            S.DATA_STAGED: self._data_staged,
            S.TARGET_PREPARING: self._target_preparing,
            S.TARGET_PREPARED: self._target_prepared,
            S.SOURCE_PREPARING: self._source_preparing,
            S.DELTA_APPLYING: self._delta_applying,
            S.TARGET_RESETTING: self._target_resetting,
            S.SOURCE_PREPARED: self._source_prepared,
            S.COMMITTING: self._committing,
            S.IN_DOUBT: self._in_doubt,
            S.COMMITTED: self._committed,
            S.RETIRING: self._retiring,
            S.RETIRED: self._retired,
            S.CONTINUING: self._continuing,
            S.ABORTING: self._aborting,
        }

    # ======================================================================================================
    # public entry points
    # ======================================================================================================
    def run_until_negotiated(self) -> FinalPlan:
        """Synchronous part (slash handler): CREATED … NEGOTIATED including the confirmation.

        Returns the confirmed ``FinalPlan``. When the user chooses ``show_plan``/``cancel`` (or a timeout with a
        reason other than proposed items) the job ends ABORTED with nothing locked and ``Cancelled`` is raised;
        any other failure aborts the job and is raised as well."""
        self._load()
        while self._state in PLANNING_STATES - {S.NEGOTIATED}:
            if not self._step():
                break
        if self._state is S.NEGOTIATED:
            try:
                return self._confirm(interactive=True)
            except Cancelled:
                raise
            except Fenced:
                raise
            except HandoverError as err:
                self._fail(err)
        if self._state is S.ABORTING:
            self._drain_abort()
        raise self._last_error_exc()

    def run(self) -> CommitAck | None:
        """Run from the journaled state until DONE, ABORTED or SUSPENDED (or a parked RETIRING/ABORTING)."""
        self._load()
        with self._keepalive_thread():
            while self._state not in STOP_STATES:
                if not self._step():
                    break
        if self._state not in STOP_STATES:
            # parked for the janitor (in_doubt, retiring, aborting): the last report names that state (W111)
            self._report(Progress(step=str(self._state)))
        return self._ack()

    def resume(self) -> CommitAck | None:
        """``/handover resume``: a SUSPENDED job locks again (re-planning if the session moved), others continue."""
        self._load()
        if self._state is S.SUSPENDED:
            self._go(
                S.LOCKING,
                resumed=True,
                replanned=False,
                capsule=None,
                staged=None,
                prepared=None,
                info=None,
            )
        return self.run()

    def recover(self) -> CommitAck | None:
        """Janitor action for a job whose owner died (DESIGN §3.6), then continue where that leads."""
        self._load()
        st = self._state
        if st in STOP_STATES:
            return self._ack()
        if st is S.IN_DOUBT:
            self._note(commit_retries=0)  # a new resolution round may restart a never-started commit again
        elif self._commit_intended() and st in PREPARE_PHASE | {S.COMMITTING}:
            self._go(S.IN_DOUBT, recovered=True, commit_retries=0)
        elif st in PLANNING_STATES:
            self._go(S.ABORTING, abort_reason="owner_died")
            self.ledger.job_update(
                self.job.job_id,
                Role.CONDUCTOR,
                epoch=self.job.epoch,
                last_error={"code": "internal", "message": "the process running this handover stopped"},
            )
        elif st in SUSPENDABLE:
            self._go(S.SUSPENDED, recovered=True)
            self._finish_result()
            return None
        elif st in PREPARE_PHASE:
            self._go(S.ABORTING, abort_reason="owner_died")
        return self.run()

    def resolve_in_doubt(self) -> str:
        """Poll the target until the commit outcome is terminal; returns ``"committed"`` or ``"aborted"``."""
        self._load()
        if self._state not in (S.IN_DOUBT, S.COMMITTING):
            return str(self._state)
        if self._state is S.COMMITTING:
            self._go(S.IN_DOUBT)
        self._in_doubt()
        return "committed" if self._state is S.COMMITTED else "aborted"

    @property
    def state(self) -> S:
        return self._state

    # ======================================================================================================
    # journal helpers
    # ======================================================================================================
    def _load(self) -> None:
        row = self.ledger.job(self.job.job_id, Role.CONDUCTOR)
        if row is None:
            raise NotFound(f"conductor job {self.job.job_id} not found")
        ctx: dict = {}
        for ev in self.ledger.events(self.job.job_id, Role.CONDUCTOR):
            data = ev["data"] or {}
            if isinstance(data.get("ctx"), dict):
                ctx.update(data["ctx"])
        self._ctx = ctx
        self._state = S(row["state"])
        self._attempts = 0

    def _get(self, key: str, cls=None):
        value = self._ctx.get(key)
        if value is None or cls is None:
            return value
        return from_wire(cls, value)

    def _go(self, to: S, **ctx: object) -> None:
        enc = {k: _encode(v) for k, v in ctx.items()}
        self.ledger.transition(
            self.job.job_id,
            Role.CONDUCTOR,
            str(self._state),
            str(to),
            {"ctx": enc} if enc else {},
            epoch=self.job.epoch,
        )
        self._ctx.update(enc)
        self._state = to
        self._attempts = 0
        self._report(Progress(step=str(to)))

    def _note(self, **ctx: object) -> None:
        enc = {k: _encode(v) for k, v in ctx.items()}
        self.ledger.event(self.job.job_id, Role.CONDUCTOR, "ctx", {"ctx": enc}, epoch=self.job.epoch)
        self._ctx.update(enc)

    def _info(self, event: str, **data: object) -> None:
        self.ledger.event(
            self.job.job_id,
            Role.CONDUCTOR,
            "info",
            {"event": event, **{k: _encode(v) for k, v in data.items()}},
            epoch=self.job.epoch,
        )

    def _intent(self, step: str) -> None:
        self.ledger.event(self.job.job_id, Role.CONDUCTOR, "intent", {"step": step}, epoch=self.job.epoch)

    def _done(self, step: str, **data: object) -> None:
        self.ledger.event(
            self.job.job_id,
            Role.CONDUCTOR,
            "done",
            {"step": step, **{k: _encode(v) for k, v in data.items()}},
            epoch=self.job.epoch,
        )

    def _side_effect(self, step: str, fn: Callable[[], object]) -> object:
        """``intent`` → call → ``done`` (DESIGN §3.1: every external side effect is bracketed in the journal)."""
        self._intent(step)
        result = fn()
        self._done(step)
        return result

    def _commit_intended(self) -> bool:
        return any(
            e["kind"] == "intent" and (e["data"] or {}).get("step") == "commit"
            for e in self.ledger.events(self.job.job_id, Role.CONDUCTOR)
        )

    def _report(self, p: Progress) -> None:
        with contextlib.suppress(Exception):  # progress is best effort (DESIGN §2.1 consequence 5)
            self.job.reporter.progress(p)

    def _ack(self) -> CommitAck | None:
        return self._get("ack", CommitAck)

    def _last_error_exc(self) -> HandoverError:
        row = self.ledger.job(self.job.job_id, Role.CONDUCTOR)
        return error_from((row or {}).get("last_error"))

    def _finish_result(self, **extra: object) -> None:
        st = self._state
        if st is S.DONE:
            outcome = "done"
        elif st is S.ABORTED:
            outcome = "aborted"
        elif st is S.SUSPENDED:
            outcome = "suspended"
        else:
            outcome = str(extra.pop("outcome", st))
        result = {"outcome": outcome, "mode": str(self.job.mode)}
        for key in ("ack", "final_plan", "continue", "warnings", "ended_by"):
            if self._ctx.get(key) is not None:
                result["plan" if key == "final_plan" else key] = self._ctx[key]
        result.update({k: _encode(v) for k, v in extra.items()})
        self.ledger.job_update(self.job.job_id, Role.CONDUCTOR, epoch=self.job.epoch, result=result)

    # ======================================================================================================
    # driving loop, errors, liveness
    # ======================================================================================================
    def _step(self) -> bool:
        """Run the handler of the current state once; returns False when the runner must stop."""
        self._check_fenced()
        handler = self._handlers[self._state]
        try:
            handler()
        except Fenced:
            raise
        except _Park:
            return False
        except HandoverError as err:
            return self._on_error(err)
        return True

    def _on_error(self, err: HandoverError) -> bool:
        st = self._state
        if is_transient(err) and self._attempts < self.poll.retry_attempts:
            self._attempts += 1
            self._backoff(self._attempts)
            return True
        self._attempts = 0
        if st is S.ABORTING:
            self._record_error(err)
            return False  # parked: the janitor retries the abort
        if st in (S.COMMITTING,):
            self._record_error(err)
            self._go(S.IN_DOUBT)
            return True
        if st in (S.RETIRED, S.CONTINUING):
            self._warn(f"continuation: {err}")
            self._go(S.DONE)
            self._finish_result()
            return True
        if st in FORWARD_STATES:
            self._record_error(err)
            return False
        if st in SUSPENDABLE and is_transient(err):
            self._record_error(err)
            self._go(S.SUSPENDED)
            self._finish_result()
            return True
        self._fail(err)
        return True

    def _fail(self, err: HandoverError) -> None:
        """Before the commit point: record the error and go ABORTING (DESIGN §3.2, I2)."""
        if self._state in (S.ABORTING, S.ABORTED):
            return
        self._record_error(err)
        self._go(S.ABORTING, abort_reason=err.code)

    def _record_error(self, err: HandoverError) -> None:
        self.ledger.job_update(self.job.job_id, Role.CONDUCTOR, epoch=self.job.epoch, last_error=err.to_wire()["error"])
        self.ledger.event(
            self.job.job_id,
            Role.CONDUCTOR,
            "error",
            {"state": str(self._state), "error": err.to_wire()["error"]},
            epoch=self.job.epoch,
        )

    def _warn(self, text: str) -> None:
        warnings = [*list(self._ctx.get("warnings") or []), text]
        self._note(warnings=warnings)
        with contextlib.suppress(Exception):
            self.job.reporter.warn(text)

    def _drain_abort(self) -> None:
        while self._state is S.ABORTING:
            if not self._step():
                break

    def _backoff(self, attempt: int) -> None:
        delay = min(self.poll.retry_max_s, self.poll.retry_first_s * (2 ** (attempt - 1)))
        self._sleep(delay)

    def _sleep(self, seconds: float) -> None:
        self._sleep_fn(seconds)
        self._pulse()

    def _check_fenced(self) -> None:
        if self._fenced:
            raise Fenced("the job was adopted by another process", reason="stale_epoch", epoch=self.job.epoch)

    def _check_cancel(self) -> None:
        if self._state in CANCELLABLE and (self.ledger.cancel_requested(self.job.job_id) or self.job.cancelled()):
            raise Cancelled("cancelled by the user")

    def _cancelled(self) -> bool:
        return self._state in CANCELLABLE and (self.ledger.cancel_requested(self.job.job_id) or self.job.cancelled())

    def _pulse(self) -> None:
        """Owner heartbeat and lease refresh by the job clock; raises when fenced or the lease is gone early."""
        now = self._clock()
        with self._beat_lock:
            if now - self._last_owner_hb >= self.poll.owner_heartbeat_s:
                self._owner_beat(now)
            if self._state in LEASE_STATES and now - self._last_lease_hb >= self._refresh_s:
                self._lease_beat(now)
        self._check_fenced()
        if self._lease_lost and self._state in SUSPENDABLE | PREPARE_PHASE - {S.SOURCE_PREPARED}:
            raise LeaseLost("the source session's turn lease was lost before the handover was prepared")

    def _owner_beat(self, now: float) -> None:
        self._last_owner_hb = now
        if not self.ledger.heartbeat(self.job.job_id, Role.CONDUCTOR, self.owner, self.job.epoch):
            self._fenced = True

    def _lease_beat(self, now: float) -> None:
        self._last_lease_hb = now
        try:
            self.source.heartbeat(self.job)
        except LeaseLost:
            self._lease_lost = True
        except Fenced:
            raise
        except HandoverError:
            pass

    def _keepalive_thread(self):
        conductor = self

        class _Keep:
            def __enter__(self_inner):
                self_inner.stop = threading.Event()
                self_inner.thread = None
                if conductor._keepalive:
                    import contextvars

                    ctx = contextvars.copy_context()
                    self_inner.thread = threading.Thread(
                        target=ctx.run, args=(self_inner.loop,), name="ctxh-keepalive", daemon=True
                    )
                    self_inner.thread.start()
                return self_inner

            def loop(self_inner) -> None:
                while not self_inner.stop.wait(conductor.poll.keepalive_s):
                    now = conductor._clock()
                    try:
                        with conductor._beat_lock:
                            if now - conductor._last_owner_hb >= conductor.poll.owner_heartbeat_s:
                                conductor._owner_beat(now)
                            if (
                                conductor._state in LEASE_STATES
                                and now - conductor._last_lease_hb >= conductor._refresh_s
                            ):
                                conductor._lease_beat(now)
                    except Exception:  # noqa: S110 - keepalive is best effort; the main loop decides
                        pass

            def __exit__(self_inner, *exc):
                self_inner.stop.set()
                if self_inner.thread is not None:
                    self_inner.thread.join(timeout=10)
                return False

        return _Keep()

    def _await(self, api, st: OpStatus, *, cancellable: bool = True) -> dict:
        """Poll an operation until it leaves ``running`` (1 s backing off to 5 s, DESIGN §4.5)."""
        delay = self.poll.op_first_s
        while st.state is OpState.RUNNING:
            self._sleep(delay)
            delay = min(self.poll.op_max_s, delay * 1.5)
            if cancellable:
                self._check_cancel()
            st = api.op_status(self.job, st.op_id)
        if st.state is OpState.FAILED:
            raise error_from(st.error)
        return dict(st.result or {})

    def _hello_of(self, side: str) -> Hello | None:
        return self._get(f"hello_{side}", Hello)

    def _remote_limits(self, side: str) -> tuple[bool, int, int]:
        h = self._hello_of(side)
        if h is None:
            return False, self.poll.request_bytes, self.poll.request_bytes
        peer_max = max(h.limits.block_size, int(h.limits.max_request_bytes))
        return bool(h.limits.hosted), min(self.poll.request_bytes, peer_max), peer_max

    def _move(self, root_id: str, *, src_side: str = "source") -> None:
        src = VerifiedBlocks(self._side_blocks(self.source))
        dst = self._side_blocks(self.target)
        cache: dict[str, bytes] = {}
        ids = object_closure(src, root_id, max_doc_bytes=self.poll.max_doc_bytes, cache=cache)
        hosted_s, req_s, max_s = self._remote_limits("source")
        hosted_t, req_t, max_t = self._remote_limits("target")
        move_object(
            src,
            dst,
            ids,
            reporter=self.job.reporter,
            cancelled=self._cancelled,
            hosted=hosted_s or hosted_t,
            request_bytes=min(req_s, req_t),
            max_request_bytes=min(max_s, max_t),
            on_batch=self._pulse,
            prefetched=cache,
        )

    def _side_blocks(self, api):
        """``api.blocks_for(job)`` when the participant offers it (job-bound remote blocks, the target's quota sink),
        else ``api.blocks()``."""
        blocks_for = getattr(api, "blocks_for", None)
        return blocks_for(self.job) if callable(blocks_for) else api.blocks()

    def _planner(self):
        if self._planning is None:
            self._planning = _default_planning()
        return self._planning

    # ======================================================================================================
    # state handlers
    # ======================================================================================================
    def _created(self) -> None:
        self._go(S.HELLO)

    def _hello(self) -> None:
        self._check_cancel()
        hs = self.source.hello()
        ht = self.target.hello()
        for side, h in (("source", hs), ("target", ht)):
            if WIRE_MAJOR not in tuple(h.majors):
                raise VersionUnsupported(f"{side} speaks wire {list(h.majors)}, this side {WIRE_MAJOR}")
            for cap in REQUIRED_CAPS:
                if cap not in h.capabilities:
                    raise CapabilityMissing(f"{side} cannot {cap}", capability=cap, side=side)
        if self.continue_prompt and "continue.v1" not in ht.capabilities:
            raise CapabilityMissing("target cannot continue sessions", capability="continue.v1", side="target")
        self._side_effect("target.open_job", lambda: self.target.open_job(self.job))
        self._go(S.FACTS, hello_source=hs, hello_target=ht)

    def _facts(self) -> None:
        self._check_cancel()
        sf = self.source.facts(self.job)
        tf = self.target.facts(self.job, sf)
        self._go(S.PLANNING, source_facts=sf, target_facts=tf)

    def _planning_step(self) -> None:
        self._check_cancel()
        tf = self._get("target_facts", EnvFacts)
        st = self.source.start_plan(self.job, tf, self._session_ref)
        plan = from_wire(ResourcePlan, self._await(self.source, st))
        if self.job.mode is Mode.IMPORT:
            # contract X02: no new plan — the bundle's FinalPlan (id checked against the bundle manifest) is the
            # plan of record; the target only decides where things go here (its decisions never change that id)
            bundle = self._bundle_final()
            final = self.target.negotiate(self.job, bundle.plan)
            if to_wire(final.plan) != to_wire(bundle.plan):
                raise VerifyFailed("the target changed the bundle's plan", which="plan")
            self._go(S.NEGOTIATED, plan=bundle.plan, final_plan=final, bundle_plan_id=bundle.plan_id, confirmed=False)
            return
        final = self.target.negotiate(self.job, plan)
        self._go(S.NEGOTIATED, plan=plan, final_plan=final, confirmed=False)

    def _bundle_final(self) -> FinalPlan:
        """The FinalPlan an import bundle carries, read through ``VerifiedBlocks`` (size-capped, id-checked) from the
        bundle source (``BundleSource.manifest``/``blocks()``), its ``plan_id`` recomputed by the one rule (DESIGN
        §8.6) and compared with the bundle manifest's ``plan_id``."""
        manifest = self.source.manifest
        blocks = VerifiedBlocks(self.source.blocks())
        raw = read_object(blocks, str(manifest["final_plan_id"]), max_bytes=self.poll.max_doc_bytes)
        final = from_wire(FinalPlan, loads_bounded(raw, max_bytes=self.poll.max_doc_bytes))
        doc = to_wire(final)
        doc.pop("plan_id", None)
        computed = self._planner().plan_id(doc)
        if computed != final.plan_id or computed != manifest.get("plan_id"):
            raise VerifyFailed("the bundle's final plan does not match its manifest", which="final_plan_id")
        return final

    def _confirm(self, *, interactive: bool) -> FinalPlan:
        """Confirmation after NEGOTIATED (DESIGN §14.1): ``yes`` / ``no_proposed`` / ``show_plan`` / ``cancel``;
        ``None`` (timeout or no interactive surface) continues without proposed items only when proposals are
        the sole reason, otherwise the job ends with nothing locked [BX L5]."""
        final = self._get("final_plan", FinalPlan)
        if self._ctx.get("confirmed"):
            return final
        sf = self._get("source_facts", EnvFacts)
        planner = self._planner()
        instructions = self._instruction_files(final)
        offline = bool(sf and sf.offline_capable)
        # reason (d) only from the local source's own policy (X07): a local --include-secret is in its
        # PolicyConfig; a remote or bundle source releases no secret on this side's word
        cfg = self.source.policy if isinstance(self.source, LocalSource) else None
        reasons = list(
            planner.confirmation_reasons(final, self.job.options, offline, instructions=instructions, cfg=cfg)
        )
        importing = self.job.mode is Mode.IMPORT
        if importing:  # the pack holds what the export confirmed; nothing is left to choose here (X02)
            reasons = [r for r in reasons if r != CONFIRM_PROPOSED]
        choice: str | None = "yes"
        if reasons and not self.job.options.yes:
            choice = None
            if interactive and self.confirm is not None:
                summary = planner.presend_summary(final, instructions=instructions)
                choice = self.confirm(
                    {
                        "reasons": reasons,
                        "summary": summary,
                        "final_plan": final,
                        "suspected_secrets": suspected_secrets(final),
                        "instruction_files": instructions,
                    }
                )
            if choice is None:
                choice = "no_proposed" if set(reasons) <= {CONFIRM_PROPOSED} else "cancel"
            elif choice not in ANSWERS:
                choice = "cancel"  # an answer outside yes/no_proposed/show_plan/cancel never goes ahead (W112)
        if self.job.options.dry_run and choice not in ("show_plan", "cancel"):
            choice = "dry_run"  # the plan is shown, nothing is locked (W113)
        if choice in ("show_plan", "cancel", "dry_run"):
            self._info("planning_ended", ended_by=choice, reasons=reasons)
            self._note(ended_by=choice)
            err = Cancelled("ended after planning; nothing was locked", ended_by=choice)
            self._fail(err)
            self._drain_abort()
            raise err
        if not importing:
            # always (X06): "yes" takes proposals along except suspected secrets (R25), "no_proposed" drops them
            final = self._settle(final, choice)
        self._note(final_plan=final, confirmed=True, choice=choice, reasons=reasons, instructions=instructions)
        return final

    def _settle(self, negotiated: FinalPlan, choice: str, drop: frozenset[str] = frozenset()) -> FinalPlan:
        """The confirmed FinalPlan: ``drop`` items excluded, ``plan.apply_choice(choice)`` applied — and when that
        changed the plan, the target negotiates the confirmed plan again so it holds exactly the plan the source
        packs (a bundle stores it, X02; the staging quota covers only what was confirmed, R28)."""
        final = self._drop_items(negotiated, set(drop)) if drop else negotiated
        final = self._planner().apply_choice(final, choice)
        if final.plan_id == negotiated.plan_id:
            return final
        again = self.target.negotiate(self.job, final.plan)
        if to_wire(again.plan) != to_wire(final.plan):
            raise VerifyFailed("the target changed the confirmed plan", which="plan")
        return again

    def _instruction_files(self, final: FinalPlan) -> list[dict]:
        """Instruction files new or different here that arrive from an unverified source — a pull from a remote
        source or an unverified bundle (DESIGN §8.7, SEC 4.3, reason g). Read on the target, i.e. here."""
        mode = self.job.mode
        if mode is Mode.PULL or (mode is Mode.IMPORT and not self.source.verified):
            dests = {d.item_id: d.dest for d in final.decisions}
            try:
                return list(self._planner().instruction_files(final.plan.items, lambda item_id: dests.get(item_id)))
            except HandoverError:
                return []
        return []

    def _negotiated(self) -> None:
        """After the confirmation: the pre-send summary (DESIGN §8.7, SEC 5.8) is journaled as the structured
        ``plan.presend_summary`` document (ctx ``presend``); the surfaces render it — no text line from here."""
        self._check_cancel()
        final = self._confirm(interactive=False)
        presend = self._planner().presend_summary(final, instructions=self._ctx.get("instructions") or ())
        self._go(S.LOCKING, presend=dict(presend))

    def _locking(self) -> None:
        self._check_cancel()
        final = self._get("final_plan", FinalPlan)
        deadline: float | None = None
        while True:
            try:
                info = self._side_effect(
                    "source.lock", lambda: self.source.lock(self.job, self._session_ref, tuple(final.plan.revision))
                )
                break
            except SessionBusy:
                if not self.job.options.wait:
                    raise
                if deadline is None:
                    deadline = self._clock() + float(self.job.options.wait_timeout_s)
                if self._clock() >= deadline:
                    raise WaitTimeout(
                        f"The running turn did not finish within {int(self.job.options.wait_timeout_s // 60)} min. "
                        "Nothing was changed.",
                        wait_s=self.job.options.wait_timeout_s,
                    ) from None
                self._sleep(self.poll.wait_retry_s)
                self._check_cancel()
        self._last_lease_hb = self._clock()
        self._lease_lost = False
        self._go(S.LOCKED, info=info)

    def _locked(self) -> None:
        self._check_cancel()
        info = self._get("info", SessionInfo)
        if info is not None and info.revision_changed and not self._ctx.get("replanned"):
            self._replan()
        self._go(S.EXPORTING)

    def _replan(self) -> None:
        """Revision moved between plan and lock: deterministic re-plan (refine off) and re-negotiation; new
        proposals are excluded in this non-interactive part (DESIGN §3.2 LOCKED)."""
        old = self._get("plan", ResourcePlan)  # the plan the user confirmed proposals of (before the choice)
        old_ids = {i.item_id for i in old.items}
        old_proposed = {i.item_id for i in old.items if i.include is Include.PROPOSED}
        job_off = replace(self.job, options=replace(self.job.options, refine="off"))
        tf = self._get("target_facts", EnvFacts)
        plan = from_wire(ResourcePlan, self._await(self.source, self.source.start_plan(job_off, tf, self._session_ref)))
        negotiated = self.target.negotiate(self.job, plan)
        excluded = sorted(
            i.item_id
            for i in negotiated.plan.items
            if i.include is Include.PROPOSED and (i.item_id not in old_ids or i.item_id not in old_proposed)
        )
        choice = self._ctx.get("choice") if self._ctx.get("choice") in ("yes", "no_proposed") else "no_proposed"
        final = self._settle(negotiated, choice, frozenset(excluded))
        self._info("replanned", excluded_proposed=excluded, revision=list(plan.revision))
        self._note(plan=plan, final_plan=final, replanned=True)

    def _drop_items(self, final: FinalPlan, ids: set[str]) -> FinalPlan:
        items = tuple(replace(i, include=Include.NO) if i.item_id in ids else i for i in final.plan.items)
        plan = replace(final.plan, items=items)
        decisions = tuple(d for d in final.decisions if d.item_id not in ids)
        out = replace(final, plan=plan, decisions=decisions, plan_id="")
        doc = to_wire(out)
        doc.pop("plan_id", None)
        return replace(out, plan_id=self._planner().plan_id(doc))

    def _exporting(self) -> None:
        """Export ∥ pack: both operations start here; this state ends when the capsule is staged on the target."""
        self._check_cancel()
        final = self._get("final_plan", FinalPlan)
        self._intent("source.export")
        export = self.source.start_export(self.job)
        pack = None
        if self._ctx.get("pack") is None:
            self._intent("source.pack")
            pack = self.source.start_pack(self.job, final)
        delay = self.poll.op_first_s
        while export.state is OpState.RUNNING:
            self._sleep(delay)
            delay = min(self.poll.op_max_s, delay * 1.5)
            self._check_cancel()
            export = self.source.op_status(self.job, export.op_id)
            if pack is not None and pack.state is OpState.RUNNING:
                pack = self.source.op_status(self.job, pack.op_id)
        if export.state is OpState.FAILED:
            err = error_from(export.error)
            if isinstance(err, StaleSession):
                self._restart("export_revision_moved")
                return
            raise err
        capsule = from_wire(CapsuleInfo, export.result or {})
        self._done("source.export", capsule_id=capsule.capsule_id)
        if pack is not None and pack.state is OpState.DONE:
            self._done("source.pack")
            self._note(pack=from_wire(PackInfo, pack.result or {}), pack_moved=False)
        self._side_effect("move.capsule", lambda: self._move(capsule.capsule_id))
        staged = self._side_effect(
            "target.stage_context",
            lambda: self._await(self.target, self.target.start_stage_context(self.job, capsule)),
        )
        self._go(S.CONTEXT_STAGED, capsule=capsule, staged=staged)

    def _context_staged(self) -> None:
        self._go(S.DATA_STAGED if self._ctx.get("pack_moved") else S.PACKING)

    def _packing(self) -> None:
        self._check_cancel()
        final = self._get("final_plan", FinalPlan)
        pack = self._get("pack", PackInfo)
        if pack is None:
            self._intent("source.pack")
            pack = from_wire(PackInfo, self._await(self.source, self.source.start_pack(self.job, final)))
            self._done("source.pack")
            self._note(pack=pack, pack_moved=False, pack_time=self._clock())
        self._side_effect("move.pack", lambda: self._move(pack.pack_id))
        self._go(S.DATA_STAGED, pack_moved=True, pack_time=self._ctx.get("pack_time") or self._clock())

    def _data_staged(self) -> None:
        self._go(S.TARGET_PREPARING)

    def _note_parts(self) -> dict:
        """``PrepareRequest.note_parts`` (contract X16): exactly ``plan.note_parts_from`` — lists and flags only, every
        key one ``note.compose`` renders: ``source_kept`` when an imported bundle is a copy (O3), the instruction
        files of the confirmation, ``pack_time`` ("Workspace snapshot taken …", DESIGN §11.3) and the session title
        (also the bundle's title). Conductor facts the note does not render (mode, deltas, restarts, free-text
        warnings) are not sent; the plan's free text travels in the FinalPlan the target holds."""
        final = self._get("final_plan", FinalPlan)
        info = self._get("info", SessionInfo)
        kept = self.job.mode is Mode.IMPORT and bool(self.source.is_copy)
        return dict(
            self._planner().note_parts_from(
                final,
                source_kept=kept,
                instruction=self._ctx.get("instructions") or (),
                pack_time=self._ctx.get("pack_time"),
                title=info.title if info is not None else None,
            )
        )

    def _target_preparing(self) -> None:
        self._check_cancel()
        final = self._get("final_plan", FinalPlan)
        req = PrepareRequest(
            capsule=self._get("capsule", CapsuleInfo),
            pack=self._get("pack", PackInfo),
            final_plan_id=self._ctx.get("bundle_plan_id") or final.plan_id,
            source_facts=self._get("source_facts", EnvFacts),
            note_parts=self._note_parts(),
            continue_requested=bool(self.continue_prompt),
        )
        prepared = self._side_effect(
            "target.prepare", lambda: self._await(self.target, self.target.start_prepare(self.job, req))
        )
        self._go(S.TARGET_PREPARED, prepared=from_wire(PreparedInfo, prepared))

    def _target_prepared(self) -> None:
        self._go(S.SOURCE_PREPARING)

    def _restart(self, why: str) -> None:
        row = self.ledger.job(self.job.job_id, Role.CONDUCTOR) or {}
        restarts = int(row.get("restarts") or 0) + 1
        if restarts > self.poll.max_restarts:
            raise RestartLimit(
                "The session kept changing during the handover (3 restarts). Pause work on it and try again.",
                restarts=restarts - 1,
            )
        self.ledger.job_update(self.job.job_id, Role.CONDUCTOR, epoch=self.job.epoch, restarts=restarts)
        self._info("restart", why=why, restarts=restarts)

    def _source_preparing(self) -> None:
        self._check_cancel()
        prepared = self._get("prepared", PreparedInfo)
        raw = self._side_effect(
            "source.prepare", lambda: self._await(self.source, self.source.start_prepare(self.job, prepared))
        )
        res = from_wire(PrepareResult, raw)
        if res.outcome is PrepareOutcome.OK:
            self._go(S.SOURCE_PREPARED)
        elif res.outcome is PrepareOutcome.DELTA:
            if res.delta is None:
                raise BadRequest("delta outcome without delta", field="delta")
            deltas = int(self._ctx.get("deltas") or 0) + 1
            if deltas > self.poll.max_deltas:
                raise RestartLimit("The session kept growing during the handover.", deltas=deltas - 1)
            self._go(S.DELTA_APPLYING, delta=res.delta, deltas=deltas)
        elif res.outcome is PrepareOutcome.STALE_SESSION:
            self._restart("stale_session")
            self._go(S.TARGET_RESETTING)
        else:
            self._restart("stale_worktree")
            self._go(S.PACKING, pack=None, pack_moved=False)

    def _delta_applying(self) -> None:
        self._check_cancel()
        delta = self._get("delta", DeltaInfo)
        self._side_effect("move.delta", lambda: self._move(delta.delta_id))
        prepared = self._side_effect(
            "target.apply_delta", lambda: self._await(self.target, self.target.start_apply_delta(self.job, delta))
        )
        self._go(S.SOURCE_PREPARING, prepared=from_wire(PreparedInfo, prepared))

    def _target_resetting(self) -> None:
        self._check_cancel()
        self._side_effect(
            "target.reset_session", lambda: self._await(self.target, self.target.start_reset_session(self.job))
        )
        self._go(S.EXPORTING, capsule=None, staged=None, prepared=None)

    def _source_prepared(self) -> None:
        if not self.ledger.intent_commit(self.job.job_id, self.job.epoch):
            raise Cancelled("cancelled by the user")
        self._go(S.COMMITTING)

    def _committing(self) -> None:
        self._intent("target.commit")
        result = self._await(self.target, self.target.start_commit(self.job), cancellable=False)
        ack = from_wire(CommitAck, result)
        self._done("target.commit", session_id=ack.session_id)
        self._go(S.COMMITTED, ack=ack)

    def _in_doubt(self) -> None:
        """Only terminal target states end IN_DOUBT; ``committing`` keeps the source locked [SK H5]."""
        delay = self.poll.in_doubt_first_s
        started = self._clock()
        while True:
            if self._clock() - started > float(self.job.options.stall_timeout_s):
                self._record_error(
                    InDoubt(
                        "The target may already have the session; the source stays read-only until resolved.",
                        waited_s=int(self._clock() - started),
                    )
                )
                self._finish_result(outcome="in_doubt")
                raise _Park()  # the janitor resumes the resolution on the next start (DESIGN §3.6)
            try:
                st = self.target.status(self.job)
                if st.state == "committed":
                    op = (st.ops or {}).get("commit")
                    if op is not None and op.state is OpState.DONE and op.result:
                        result = dict(op.result)
                    else:
                        result = self._await(self.target, self.target.start_commit(self.job), cancellable=False)
                    ack = from_wire(CommitAck, result)
                    self._done("target.commit", session_id=ack.session_id, resolved="in_doubt")
                    self._go(S.COMMITTED, ack=ack)
                    return
                if st.state in ("aborted", "expired"):
                    self._info("in_doubt_resolved", target_state=st.state)
                    self._go(S.ABORTING, abort_reason=f"target_{st.state}")
                    return
                if st.state == "prepared":
                    # the commit never started there (lost request): start it again (idempotent), bounded
                    tries = int(self._ctx.get("commit_retries") or 0) + 1
                    if tries <= self.poll.retry_attempts:
                        self._go(S.COMMITTING, commit_retries=tries)
                        return
            except Fenced:
                raise
            except HandoverError:
                pass
            self._sleep(delay)
            delay = min(self.poll.in_doubt_max_s, delay * 2)

    def _committed(self) -> None:
        self._go(S.RETIRING)

    def _retiring(self) -> None:
        ack = self._get("ack", CommitAck)
        attempt = 0
        while True:
            try:
                raw = self._side_effect(
                    "source.retire",
                    lambda: self._await(self.source, self.source.start_retire(self.job, ack), cancellable=False),
                )
                break
            except Fenced:
                raise
            except InvalidState as err:
                self._warn(
                    f"the source was not retired ({err.details.get('state', 'unknown')}); the copies are "
                    "independent now"
                )
                raw = {"state": err.details.get("state")}
                break
            except HandoverError as err:
                attempt += 1
                if attempt >= self.poll.retire_attempts or not is_transient(err):
                    self._record_error(err)
                    self._finish_result(outcome="active_source_pending")
                    raise _Park() from err
                self._backoff(attempt)
        self._go(S.RETIRED, retire=raw)

    def _retired(self) -> None:
        if self.continue_prompt:
            self._go(S.CONTINUING)
        else:
            self._go(S.DONE)
            self._finish_result()

    def _continuing(self) -> None:
        if self._ctx.get("continue") is None:
            try:
                res = self.target.continue_session(self.job, self.continue_prompt or "")
                self._note(**{"continue": to_wire(res)})
            except Fenced:
                raise
            except HandoverError as err:
                self._warn(f"continuation not started: {err.code}")
                error = {"code": err.code, "message": err.message, "details": dict(err.details)}
                self._note(**{"continue": to_wire(ContinueResult(started=False)) | {"error": error}})
        self._go(S.DONE)
        self._finish_result()

    def _aborting(self) -> None:
        reason = str(self._ctx.get("abort_reason") or "aborted")
        try:
            self._side_effect("target.abort", lambda: self.target.abort(self.job, reason))
        except InvalidState as err:
            if err.details.get("state") in ("committing", "committed"):
                self._info("abort_refused_by_target", target_state=err.details.get("state"))
                self._go(S.IN_DOUBT)
                return
            if err.details.get("reason") == "stale_epoch":
                raise
        except NotFound:
            pass
        self._side_effect(
            "source.release",
            lambda: self._await(self.source, self.source.start_release(self.job, reason), cancellable=False),
        )
        self._go(S.ABORTED)
        self._finish_result()
