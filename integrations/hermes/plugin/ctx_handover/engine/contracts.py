"""Cross-cluster protocols of ctx_handover (layer L1, stdlib only; INTERFACES §3.3).

The conductor (cluster A) drives one ``SourceAPI`` and one ``TargetAPI``; each is local (in-process engine),
remote (HTTP, cluster C) or a bundle (cluster C). Long steps are ``start_*`` operations that return an
``OpStatus`` immediately and are polled with ``op_status`` (DESIGN §3.7, I7). Every ``start_*`` is idempotent
per ``(job_id, op, digest)``: repeating a call with the same arguments returns the same operation.

The ``result`` of a finished operation is the wire form (``jsonutil.to_wire``) of the type named in the
method's docstring; the conductor decodes it with ``jsonutil.from_wire``.

All protocols are ``runtime_checkable`` so tests can assert that fakes and real classes satisfy them
(attribute presence only — signatures are checked by the owning cluster's tests).
"""

from __future__ import annotations

from collections.abc import Callable, Iterable, Iterator, Sequence
from dataclasses import dataclass
from typing import Protocol, runtime_checkable

from .model import (
    CapsuleInfo,
    CommitAck,
    ContinueResult,
    DeltaInfo,
    EnvFacts,
    FinalPlan,
    Hello,
    Mode,
    ObjectManifest,
    OpStatus,
    Options,
    ParticipantStatus,
    Peer,
    PreparedInfo,
    PrepareRequest,
    Progress,
    ResourcePlan,
    SessionInfo,
    SessionRef,
)


@runtime_checkable
class Reporter(Protocol):
    """Progress sink of a job (post_message, TTY or nothing). Never raises into the caller."""

    def progress(self, p: Progress) -> None:
        """Report progress of the current step (throttled by the implementation)."""
        ...

    def info(self, text: str) -> None:
        """Report an informational line."""
        ...

    def warn(self, text: str) -> None:
        """Report a warning line."""
        ...


@dataclass(frozen=True)
class JobContext:
    """Everything a participant call needs to know about the job it serves.

    ``epoch`` is the caller's owner epoch (fencing, DESIGN §3.1); ``cancelled`` is polled by long steps;
    ``options`` were resolved once at job creation (DESIGN §14.4).
    """

    job_id: str
    mode: Mode
    peer: Peer | None
    options: Options
    epoch: int
    reporter: Reporter
    cancelled: Callable[[], bool]
    created_at: float


@runtime_checkable
class GitProbe(Protocol):
    """Read-only git questions the collector asks (implemented by ``gitops.GitProbe``)."""

    def toplevel(self, path: str) -> str | None:
        """Work-tree root containing ``path`` (``git rev-parse --show-toplevel``), or None."""
        ...

    def common_dir(self, path: str) -> str | None:
        """The repository's common git dir (``--git-common-dir``), or None."""
        ...

    def is_shallow(self, path: str) -> bool:
        """True for a shallow repository."""
        ...

    def is_partial(self, path: str) -> bool:
        """True for a partial clone (a promisor remote) containing ``path``: like a shallow clone it lacks objects,
        so it travels as a file tree (W107, contract V30)."""
        ...

    def is_ignored(self, path: str) -> bool:
        """True unless git confirms that the directory ``path`` is not ignored in the work tree around it
        (``git check-ignore``; a directory holding a tracked file is not ignored). The collector takes an enclosing
        repository as the workspace only when the session's cwd is not ignored there (R36, L3-F19)."""
        ...

    def has_tracked_files(self, path: str) -> bool | None:
        """Whether the index of the work tree around the directory ``path`` holds a file below it (R36a): True, False,
        or None when git cannot answer. A session folder with tracked files is part of the user's own repository, so
        that repository is the workspace without a question."""
        ...


@runtime_checkable
class BusyReportLike(Protocol):
    """What planning reads from a busy probe (``lock.BusyReport`` satisfies it; INTERFACES §4.2).

    Defined here so ``plan`` does not import ``lock`` (same-layer import, INTERFACES §2).
    """

    processes: tuple[dict, ...]
    delegations: tuple[dict, ...]
    cron: tuple[dict, ...]
    warnings: tuple[str, ...]


@runtime_checkable
class BlockSource(Protocol):
    """Where blocks and object manifests are read from (local store, remote peer, bundle)."""

    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        """Manifests of the given objects, in request order; unknown ids raise ``NotFound``."""
        ...

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        """Yield ``(block_id, raw bytes)`` for the requested blocks (order may differ from the request)."""
        ...


@runtime_checkable
class BlockSink(Protocol):
    """Where blocks and object manifests are written to (local store, remote peer, bundle writer)."""

    def put_manifests(self, manifests: Sequence[ObjectManifest]) -> int:
        """Store manifests; returns how many were new."""
        ...

    def missing(self, ids: Sequence[str]) -> list[str]:
        """The subset of block ids the sink does not have yet (I9: verified blocks are never resent)."""
        ...

    def write_blocks(self, frames: Iterable[tuple[str, bytes]]) -> tuple[int, list[tuple[str, str]]]:
        """Write ``(block_id, raw)`` pairs; returns ``(accepted, [(block_id, error_code), …])``."""
        ...


@runtime_checkable
class SourceAPI(Protocol):
    """The source side of a job (DESIGN §3.3, §4.5): ``LocalSource``, ``RemoteSource`` or ``BundleSource``."""

    def hello(self) -> Hello:
        """Versions, capabilities and limits of the source host."""
        ...

    def list_sessions(self, limit: int, query: str | None, include_archived: bool) -> list[dict]:
        """Session list for the picker (no job needed)."""
        ...

    def facts(self, job: JobContext) -> EnvFacts:
        """Deterministic environment facts of the source (DESIGN §11.1)."""
        ...

    def start_plan(self, job: JobContext, target_facts: EnvFacts, session_ref: SessionRef) -> OpStatus:
        """Collect and refine the resource plan; result ``ResourcePlan``."""
        ...

    def lock(self, job: JobContext, session_ref: SessionRef, plan_revision: tuple) -> SessionInfo:
        """Acquire the turn lease and compression lock after the user confirmed the plan (DESIGN §7.2)."""
        ...

    def heartbeat(self, job: JobContext) -> None:
        """Refresh the leases held for this job; raises ``LeaseLost`` when they are gone."""
        ...

    def start_export(self, job: JobContext) -> OpStatus:
        """Export the capsule into the source block store; result ``CapsuleInfo``."""
        ...

    def start_pack(self, job: JobContext, final_plan: FinalPlan) -> OpStatus:
        """Pack the working material of the final plan; result ``PackInfo``."""
        ...

    def start_prepare(self, job: JobContext, prepared: PreparedInfo) -> OpStatus:
        """Re-check revision and work trees, apply the primary lock (DESIGN §7.4); result ``PrepareResult`` (#16)."""
        ...

    def start_retire(self, job: JobContext, ack: CommitAck) -> OpStatus:
        """Retire the source after the target's ACK (DESIGN §7.5); result ``{"state": …}`` (route #17)."""
        ...

    def start_release(self, job: JobContext, reason: str) -> OpStatus:
        """Undo lock/prepare and free the session (DESIGN §7.7); result ``{"state": …}`` (route #18)."""
        ...

    def op_status(self, job: JobContext, op_id: str) -> OpStatus:
        """Poll an operation started by this job."""
        ...

    def status(self, job: JobContext) -> ParticipantStatus:
        """The participant's state of this job."""
        ...

    def blocks(self) -> BlockSource:
        """The block source the conductor moves objects from."""
        ...


@runtime_checkable
class TargetAPI(Protocol):
    """The target side of a job (DESIGN §3.3, §4.5): ``LocalTarget``, ``RemoteTarget`` or ``BundleTarget``."""

    def hello(self) -> Hello:
        """Versions, capabilities and limits of the target host."""
        ...

    def open_job(self, job: JobContext) -> None:
        """Create the target participant for this job (idempotent)."""
        ...

    def facts(self, job: JobContext, source_facts: EnvFacts) -> EnvFacts:
        """Deterministic environment facts of the target (DESIGN §11.1)."""
        ...

    def negotiate(self, job: JobContext, plan: ResourcePlan) -> FinalPlan:
        """Decide destinations, modes and space for every item (DESIGN §8.6)."""
        ...

    def start_stage_context(self, job: JobContext, capsule: CapsuleInfo) -> OpStatus:
        """Verify the capsule's blocks and caps and stage it; result ``{segments, rows, bytes, import_path}`` (#26)."""
        ...

    def start_prepare(self, job: JobContext, req: PrepareRequest) -> OpStatus:
        """Import hidden, verify hashes, prepare work trees (DESIGN §3.4); result ``PreparedInfo``."""
        ...

    def start_apply_delta(self, job: JobContext, delta: DeltaInfo) -> OpStatus:
        """Append the source's late rows and re-verify (DESIGN §6.2); result ``PreparedInfo``."""
        ...

    def start_reset_session(self, job: JobContext) -> OpStatus:
        """Discard the hidden import for a restart (DESIGN §3.2 TARGET_RESETTING); result ``{}`` (#29)."""
        ...

    def start_commit(self, job: JobContext) -> OpStatus:
        """Journaled commit c1–c7 with inverses (DESIGN §3.4); result ``CommitAck``."""
        ...

    def continue_session(self, job: JobContext, prompt: str) -> ContinueResult:
        """Start the continuation when the target allows it (DESIGN §11.5)."""
        ...

    def abort(self, job: JobContext, reason: str) -> None:
        """Abort the job on the target and discard staged state (idempotent)."""
        ...

    def op_status(self, job: JobContext, op_id: str) -> OpStatus:
        """Poll an operation started by this job."""
        ...

    def status(self, job: JobContext) -> ParticipantStatus:
        """The participant's state of this job."""
        ...

    def blocks(self) -> BlockSink:
        """The block sink the conductor moves objects into."""
        ...
