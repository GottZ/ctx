"""Offline bundle: ``BundleTarget`` writes, ``BundleSource`` reads (layer L6, stdlib; DESIGN §5.4, T4).

A bundle is the block store of one job in a single uncompressed POSIX tar (default ``*.ctxh.tar``) or, with
``--dir``, the same tree as a directory::

    CTXH-BUNDLE            "ctxh-bundle 1\\n"
    manifest.json          bundle manifest (canonical JSON)
    manifest.sig           base64url Ed25519 signature of manifest.json by the exporter's identity key
    manifests/ab/<62>.json object manifests
    blocks/ab/<62>         raw blocks

The conductor drives a ``BundleTarget`` like any target (open, facts, negotiate, objects and blocks, stage,
prepare, commit). Everything is written into ``<out>.partial`` (0700, files 0600) and becomes visible only in the
commit step (tar written to a temp file, fsynced and renamed; or the directory renamed). The job's progress —
facts, FinalPlan, capsule, pack, prepare request, operations, state — is kept in that staging as well
(``STATE_FILE``, rewritten atomically after every step), so a new ``BundleTarget`` for the same path, in this
process or another one, continues the export: the CLI runs ``plan_sync`` and ``run_foreground`` with one instance
each (contract V24). A committed bundle is its own record: a resumed commit finds it at ``out`` (signed by this
identity for this job) and reports it committed instead of writing it again.

Reading is hardened (SEC 2.6): exact member names only, regular files only (no links, devices, directories in a
tar; no symlinks in a directory bundle), no duplicates, size caps checked from the header before reading
(``manifest.json`` ≤ 64 MiB, JSON depth ≤ 64, blocks ≤ one block), never extracted to disk, every object manifest
re-hashed against its name, every block hash-checked when read, totals and object sizes must match. A signature
counts only with a public key this ledger stores for a non-revoked peer (or one the caller passes, e.g. this
gateway's own key) — never with ``exporter.pubkey`` alone (SEC 1.6). A second import of the same bundle on the
same profile needs ``again`` (SK L5).
"""

from __future__ import annotations

import errno
import functools
import hashlib
import io
import os
import re
import shutil
import stat
import tarfile
import threading
import time
from collections.abc import Callable, Iterable, Iterator, Sequence
from dataclasses import replace
from pathlib import Path
from typing import TYPE_CHECKING

from . import PROTOCOL, WIRE_MAJOR, WIRE_MINOR
from . import plan as planning
from .blocks import (
    BLOCK_SIZE,
    MAX_MANIFEST_JSON,
    BlockCorrupt,
    manifest_doc,
    manifest_from_doc,
    mkdir_private,
    verify_manifest,
    write_private,
)
from .contracts import JobContext
from .crypto import Identity, verify
from .errors import (
    BadRequest,
    ConflictError,
    HandoverError,
    InsufficientStorage,
    InvalidState,
    NotFound,
    Unauthorized,
)
from .ids import ULID_RE
from .jsonutil import canonical_bytes, from_wire, loads_bounded, object_manifest, to_wire
from .model import (
    CapsuleInfo,
    CommitAck,
    ConductorState,
    ContinueResult,
    DeltaInfo,
    DestMode,
    EnvFacts,
    FinalPlan,
    Hello,
    Include,
    ObjectManifest,
    OpState,
    OpStatus,
    Options,
    PackInfo,
    ParticipantStatus,
    PreparedInfo,
    PrepareOutcome,
    PrepareRequest,
    PrepareResult,
    ResourcePlan,
    Role,
    SessionInfo,
    SessionRef,
    TargetDecision,
)

if TYPE_CHECKING:
    from .ledger import Ledger

__all__ = ["MAGIC", "BlockCorrupt", "BundleSource", "BundleTarget", "read_manifest"]

MAGIC = b"ctxh-bundle 1\n"
MAX_MANIFEST_BYTES = 64 * 1024**2
MAX_SIG_BYTES = 256
MEDIA_JSON = "application/json"
STATE_FILE = ".export-state.json"
"""The export's progress inside ``<out>.partial`` (never part of the bundle; removed before the commit renames)."""
BUNDLE_CAPS = frozenset({"session.v1", "blocks.v1", "git.bundle.v1", "files.v1", "hermesfiles.v1"})

_HEX2 = re.compile(r"[0-9a-f]{2}")
_BLOCK_RE = re.compile(r"blocks/([0-9a-f]{2})/([0-9a-f]{62})")
_MANIFEST_RE = re.compile(r"manifests/([0-9a-f]{2})/([0-9a-f]{62})\.json")
_FIXED = {"CTXH-BUNDLE": 64, "manifest.json": None, "manifest.sig": MAX_SIG_BYTES}


_NO_SPACE = frozenset(e for e in (errno.ENOSPC, getattr(errno, "EDQUOT", None)) if e is not None)


def _bad(message: str, **details) -> BadRequest:
    return BadRequest(f"bundle rejected: {message}", field="bundle", **details)


def write_error(exc: OSError, out: Path, need: int = 0) -> HandoverError:
    """An ``OSError`` of this host's file system while it writes the bundle ``out`` (its staging or the commit) as a
    ``HandoverError`` the conductor handles — it never leaves the conductor's thread (gp-a §8). Repeating the same
    write does not help, so neither is repeatable: a full disk or quota is ``insufficient_storage`` for the bundle's
    folder (free space measured now), anything else (no permission, I/O error) an ``internal`` error naming the
    bundle and the reason. The export then ends and frees its source."""
    folder = Path(out).parent
    if exc.errno in _NO_SPACE:
        try:
            free = shutil.disk_usage(folder).free
        except OSError:
            free = 0
        return InsufficientStorage(
            f"not enough space to write the bundle {out}",
            need_bytes=max(0, int(need)),
            free_bytes=int(free),
            path=str(folder),
            side="This machine",
            errno=exc.errno,
        )
    reason = exc.strerror or type(exc).__name__
    return HandoverError(
        f"the bundle {out} could not be written ({reason})",
        retryable=False,
        path=str(exc.filename or out),
        errno=exc.errno,
    )


def _block_name(block_id: str) -> str:
    return f"blocks/{block_id[:2]}/{block_id[2:]}"


def _manifest_name(object_id: str) -> str:
    return f"manifests/{object_id[:2]}/{object_id[2:]}.json"


# ---------------------------------------------------------------------------------------------------------
# Writer side: staging directory with the bundle layout
# ---------------------------------------------------------------------------------------------------------


class _DirSink:
    """``BlockSink``/``BlockSource`` over a staging directory in the bundle layout (no ledger)."""

    def __init__(self, root: Path, on_error: Callable[[OSError], HandoverError] | None = None) -> None:
        self.root = Path(root)
        self._tmp = self.root / ".tmp"
        self._on_error = on_error or (lambda exc: write_error(exc, self.root))

    def _bpath(self, block_id: str) -> Path:
        if not re.fullmatch(r"[0-9a-f]{64}", str(block_id)):
            raise BadRequest("malformed block id", field="block_id")
        return self.root / _block_name(block_id)

    def has(self, block_id: str) -> bool:
        return self._bpath(block_id).is_file()

    def missing(self, ids: Sequence[str]) -> list[str]:
        return [i for i in ids if not self.has(i)]

    def write_blocks(self, frames: Iterable[tuple[str, bytes]]) -> tuple[int, list[tuple[str, str]]]:
        accepted, rejected = 0, []
        for block_id, raw in frames:
            if len(raw) > BLOCK_SIZE:
                rejected.append((block_id, "payload_too_large"))
                continue
            if hashlib.sha256(raw).hexdigest() != block_id:
                rejected.append((block_id, "block_corrupt"))
                continue
            path = self._bpath(block_id)
            if not path.is_file():
                try:
                    write_private(path, bytes(raw), self._tmp)
                except OSError as exc:  # a full disk is the export's end, not a crash of the conductor's thread
                    raise self._on_error(exc) from exc
            accepted += 1
        return accepted, rejected

    def put_manifests(self, manifests: Sequence[ObjectManifest]) -> int:
        new = 0
        for m in manifests:
            m = verify_manifest(m)
            path = self.root / _manifest_name(m.object_id)
            if not path.is_file():
                try:
                    write_private(path, canonical_bytes(manifest_doc(m)), self._tmp)
                except OSError as exc:
                    raise self._on_error(exc) from exc
                new += 1
        return new

    def put_bytes(self, data: bytes, media: str) -> ObjectManifest:
        """An in-memory object, split by K-1's ``jsonutil.object_manifest`` (the same ids as the block store)."""
        doc, parts = object_manifest(data, media)
        self.write_blocks(parts)
        m = verify_manifest(
            ObjectManifest(
                sha256=doc["sha256"],
                size=doc["size"],
                block_size=doc["block_size"],
                blocks=tuple(doc["blocks"]),
                media=doc["media"],
            )
        )
        self.put_manifests([m])
        return m

    def all_manifests(self) -> dict[str, ObjectManifest]:
        out: dict[str, ObjectManifest] = {}
        base = self.root / "manifests"
        if not base.is_dir():
            return out
        for sub in sorted(base.iterdir()):
            for path in sorted(sub.iterdir()):
                oid = sub.name + path.name[: -len(".json")]
                doc = loads_bounded(path.read_bytes(), max_bytes=MAX_MANIFEST_JSON)
                out[oid] = manifest_from_doc(doc, oid)
        return out

    def complete(self, object_id: str) -> bool:
        path = self.root / _manifest_name(object_id)
        if not path.is_file():
            return False
        m = manifest_from_doc(loads_bounded(path.read_bytes(), max_bytes=MAX_MANIFEST_JSON), object_id)
        return not self.missing(list(m.blocks))

    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        known = self.all_manifests()
        try:
            return [known[i] for i in object_ids]
        except KeyError as exc:
            raise NotFound("object not in the bundle", object_id=str(exc)) from None

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        for block_id in ids:
            try:
                raw = self._bpath(block_id).read_bytes()
            except FileNotFoundError:
                raise NotFound("block not in the bundle", block_id=block_id) from None
            if hashlib.sha256(raw).hexdigest() != block_id:
                raise BlockCorrupt("bundle block does not match its id", block_id=block_id)
            yield block_id, raw


def _generic_facts(src: EnvFacts) -> EnvFacts:
    """Declared facts of an unknown importer: fresh directories, no git basis (DESIGN §5.4), no checked workspace
    root (R29: ``workspace_root_ok`` None — the exporter's own check says nothing about the importer's root; the
    importer checks its root when it imports) and no context window (R32: ``host.context_length`` None — the importer
    compares the session with its own model's window when it imports)."""
    defaults = Options()
    return replace(
        src,
        instance_name="bundle",
        hostname="",
        user="",
        home="",
        workspace_root=defaults.workspace_root,
        allow_roots=defaults.allow_roots,
        disk_free={},
        case_insensitive={},
        git_version=None,
        git_lfs=False,
        tools={},
        tools_seen={},
        offline_capable=False,
        capabilities=frozenset(),
        workspace_root_ok=None,
        workspace_root_problem="",
        host=replace(src.host, context_length=None),
    )


def _host_io(method):
    """``TargetAPI`` methods of ``BundleTarget`` touch this host's file system: an ``OSError`` there is raised as the
    ``HandoverError`` of ``write_error`` (the conductor handles only those; gp-a §8)."""

    @functools.wraps(method)
    def wrapper(self, *args, **kwargs):
        try:
            return method(self, *args, **kwargs)
        except OSError as exc:
            raise self._write_error(exc) from exc

    return wrapper


def _done(op_id: str, result: dict) -> OpStatus:
    return OpStatus(op_id=op_id, state=OpState.DONE, result=result)


def _failed(op_id: str, exc: HandoverError) -> OpStatus:
    return OpStatus(op_id=op_id, state=OpState.FAILED, error=exc.to_wire()["error"])


class BundleTarget:
    """``TargetAPI`` that writes an offline bundle (export, DESIGN §5.4).

    ``negotiate`` (``(plan, facts) -> FinalPlan``) lets the composition root use the planning cluster's decisions;
    without it every included item becomes a fresh-directory decision. Either way ``plan_id`` follows the one rule
    of DESIGN §8.6: ``plan.plan_id(plan.final_doc(final))`` (the FinalPlan document without ``plan_id``), so the
    pack's ``final_plan_id`` and the bundle's FinalPlan agree. ``retire`` False marks the bundle as a copy
    (``--keep``, O3): the manifest says ``"retire": false`` and ``BundleSource.is_copy`` is True on import.
    """

    def __init__(
        self,
        out: Path,
        identity: Identity,
        *,
        as_dir: bool,
        declared_facts: EnvFacts | None,
        ledger: Ledger,
        retire: bool = True,
        exporter_name: str = "",
        negotiate: Callable[[ResourcePlan, EnvFacts], FinalPlan] | None = None,
        plugin_version: str = "",
        hermes_version: str = "",
        clock: Callable[[], float] = time.time,
    ) -> None:
        self.out = Path(out)
        self.as_dir = bool(as_dir)
        self._identity = identity
        self._declared = declared_facts
        self._ledger = ledger
        self._retire = bool(retire)
        self._name = exporter_name
        self._negotiate = negotiate
        self._plugin_version = plugin_version
        self._hermes_version = hermes_version
        self._clock = clock
        self._partial = self.out.parent / f"{self.out.name}.partial"
        self._sink = _DirSink(self._partial, on_error=self._write_error)
        self._job_id: str | None = None
        self._created_at = 0.0
        self._facts: EnvFacts | None = None
        self._final: FinalPlan | None = None
        self._capsule: CapsuleInfo | None = None
        self._pack: PackInfo | None = None
        self._delta: DeltaInfo | None = None
        self._request: PrepareRequest | None = None
        self._ops: dict[str, OpStatus] = {}
        self._state = "open"
        self._ack: CommitAck | None = None
        self._lock = threading.RLock()
        self._seen: tuple[int, int, int] | None = None  # stat of the state file this instance wrote or read last

    # -- TargetAPI -------------------------------------------------------------------------------------------
    def hello(self) -> Hello:
        return Hello(
            protocol=PROTOCOL,
            majors=(WIRE_MAJOR,),
            minor=WIRE_MINOR,
            instance_id=self._identity.instance_id,
            listener_pubkey=self._identity.public_b64(),
            plugin_version=self._plugin_version,
            hermes_version=self._hermes_version,
            name=self._name,
            capabilities=BUNDLE_CAPS,
        )

    @_host_io
    def open_job(self, job: JobContext) -> None:
        """Open the staging. A repeated ``open_job`` of the same job keeps its progress; the staging of another
        export to the same path is refused while that job runs and cleared when it ended (or never recorded one)."""
        if self.out.exists() or self.out.is_symlink():
            raise ConflictError(f"{self.out} already exists; choose another path", path=str(self.out))
        with self._lock:
            doc = self._read_state()
            ours = doc is not None  # a staging of this plugin: private to this user, with its progress document
            if doc is not None and doc.get("job_id") == job.job_id:
                try:
                    self._apply(doc)
                    return
                except (BadRequest, KeyError, TypeError, ValueError):
                    doc = None  # unreadable progress of this job: start its staging again
            other = doc.get("job_id") if doc is not None else None
            if isinstance(other, str) and self._job_running(other):
                raise ConflictError(
                    f"another export (job {other}) writes to {self.out}; choose another path", path=str(self.out)
                )
            if self._partial.is_symlink():
                raise ConflictError(f"{self._partial} is a symbolic link; choose another path", path=str(self.out))
            if not ours and os.path.lexists(self._partial):
                # not a staging of this plugin (no progress document of ours): never delete it (L8) — only an
                # empty directory, which holds nothing, is taken over
                try:
                    os.rmdir(self._partial)
                except OSError:
                    raise ConflictError(
                        f"{self._partial} exists and is not an export staging of ctx_handover; remove it or choose "
                        "another path",
                        path=str(self.out),
                    ) from None
            shutil.rmtree(self._partial, ignore_errors=True)
            self._reset()
            self._job_id = job.job_id
            self._created_at = job.created_at or self._clock()
            mkdir_private(self._partial)
            if not self._own_private(os.lstat(self._partial), directory=True):
                raise ConflictError(
                    f"{self._partial} is not a private directory of this user; choose another path", path=str(self.out)
                )
            self._save()

    @_host_io
    def facts(self, job: JobContext, source_facts: EnvFacts) -> EnvFacts:
        with self._lock:
            self._sync(job)
            self._facts = self._declared or _generic_facts(source_facts)
            self._save()
            return self._facts

    @_host_io
    def negotiate(self, job: JobContext, plan: ResourcePlan) -> FinalPlan:
        with self._lock:
            self._sync(job)
            final = self._negotiated(plan)
            self._final = final
            self._save()
            return final

    def _negotiated(self, plan: ResourcePlan) -> FinalPlan:
        facts = self._facts or self._declared
        if self._negotiate is not None:
            final = self._negotiate(plan, facts)
        else:
            decisions = tuple(
                TargetDecision(
                    item_id=i.item_id,
                    dest=i.suggested_dest or "",
                    mode=DestMode.FRESH,
                    reason="bundle",
                    need_bytes=int(i.size_estimate),
                )
                for i in plan.items
                if i.include == Include.YES
            )
            need = sum(d.need_bytes for d in decisions)
            self.out.parent.mkdir(parents=True, exist_ok=True)
            free = shutil.disk_usage(self.out.parent).free
            final = FinalPlan(
                plan_id="",
                plan=plan,
                decisions=decisions,
                need_bytes=need,
                free_bytes={str(self.out.parent): free},
                model_change=None,
            )
            if need > free:
                raise InsufficientStorage(
                    "not enough space for the bundle",
                    need_bytes=need,
                    free_bytes=free,
                    path=str(self.out.parent),
                    side="This machine",
                )
        return replace(final, plan_id=planning.plan_id(planning.final_doc(final)))

    def _write_error(self, exc: OSError) -> HandoverError:
        """``write_error`` for this export: the bundle path, and the confirmed plan's size as the space it needs."""
        need = self._final.need_bytes if self._final is not None else 0
        return write_error(exc, self.out, need)

    def _record(self, op: OpStatus) -> OpStatus:
        """Keep ``op`` and write the progress into the staging. When that write fails (full disk, I/O error) a step
        that succeeded is reported FAILED with the write error — its progress is not on disk, so no other instance
        could continue from it (V24); a failed step stays failed, recorded in memory only."""
        self._ops[op.op_id] = op
        try:
            self._save()
        except OSError as exc:
            if op.state is OpState.DONE:
                op = _failed(op.op_id, self._write_error(exc))
                self._ops[op.op_id] = op
        return op

    @_host_io
    def start_stage_context(self, job: JobContext, capsule: CapsuleInfo) -> OpStatus:
        with self._lock:
            self._sync(job)
            op_id = f"bundle-stage-{job.job_id}"
            if not self._sink.complete(capsule.capsule_id):
                return self._record(_failed(op_id, NotFound("capsule object incomplete in the bundle")))
            self._capsule = capsule
            self._state = "context_staged"
            return self._record(
                _done(
                    op_id,
                    {
                        "segments": capsule.segments,
                        "rows": capsule.rows,
                        "bytes": capsule.size,
                        "import_path": "bundle",
                    },
                )
            )

    def _prepared(self) -> PreparedInfo:
        capsule = self._capsule
        hashes = self._delta or capsule
        title = (self._request.note_parts or {}).get("title") if self._request else None
        return PreparedInfo(
            tip_id="",
            root_id="",
            title=title if isinstance(title, str) else None,
            h_all=hashes.h_all,
            h_rows=hashes.h_rows,
            h_model=hashes.h_model,
            import_path="bundle",
            dests=(),
            warnings=(),
            source_revision=hashes.revision,
        )

    @_host_io
    def start_prepare(self, job: JobContext, req: PrepareRequest) -> OpStatus:
        with self._lock:
            self._sync(job)
            op_id = f"bundle-prepare-{job.job_id}"
            if self._capsule is None or not self._sink.complete(req.pack.pack_id):
                return self._record(_failed(op_id, NotFound("capsule or pack incomplete in the bundle")))
            self._pack = req.pack
            self._request = req
            self._state = "prepared"
            return self._record(_done(op_id, to_wire(self._prepared())))

    @_host_io
    def start_apply_delta(self, job: JobContext, delta: DeltaInfo) -> OpStatus:
        with self._lock:
            self._sync(job)
            op_id = f"bundle-delta-{job.job_id}"
            if self._request is None or not self._sink.complete(delta.delta_id):
                return self._record(_failed(op_id, NotFound("delta object incomplete in the bundle")))
            self._delta = delta
            return self._record(_done(op_id, to_wire(self._prepared())))

    @_host_io
    def start_reset_session(self, job: JobContext) -> OpStatus:
        with self._lock:
            self._sync(job)
            self._capsule = self._delta = None
            self._state = "open"
            return self._record(_done(f"bundle-reset-{job.job_id}", {}))

    @_host_io
    def start_commit(self, job: JobContext) -> OpStatus:
        with self._lock:
            self._sync(job)
            op_id = f"bundle-commit-{job.job_id}"
            if self._ack is None:
                try:
                    self._commit()
                except HandoverError as exc:
                    return self._record(_failed(op_id, exc))
                except OSError as exc:  # full disk, no permission: the commit fails for good, the job ends (gp-a §8)
                    return self._record(_failed(op_id, self._write_error(exc)))
            op = _done(op_id, to_wire(self._ack))
            self._ops[op_id] = op  # the staging (and its state file) is gone: the bundle at ``out`` is the record
            return op

    def continue_session(self, job: JobContext, prompt: str) -> ContinueResult:
        return ContinueResult(started=False, via="bundle")

    @_host_io
    def abort(self, job: JobContext, reason: str) -> None:
        with self._lock:
            self._sync(job)
            if self._ack is not None:
                return
            shutil.rmtree(self._partial, ignore_errors=True)
            tmp = self.out.parent / f"{self.out.name}.tmp"
            if tmp.exists():
                tmp.unlink()
            self._state = "aborted"
            self._seen = None

    @_host_io
    def op_status(self, job: JobContext, op_id: str) -> OpStatus:
        with self._lock:
            self._sync(job)
            try:
                return self._ops[op_id]
            except KeyError:
                raise NotFound("operation not known", op_id=op_id) from None

    @_host_io
    def status(self, job: JobContext) -> ParticipantStatus:
        with self._lock:
            self._sync(job)
            return ParticipantStatus(
                state=self._state,
                step="",
                epoch=job.epoch,
                progress=None,
                ops={k: v for k, v in self._ops.items()},
                last_error=None,
            )

    def blocks(self) -> _DirSink:
        return self._sink

    # -- progress in the staging (contract V24) ----------------------------------------------------------------
    @property
    def _state_path(self) -> Path:
        return self._partial / STATE_FILE

    def _reset(self) -> None:
        self._job_id, self._created_at, self._facts, self._final = None, 0.0, None, None
        self._capsule = self._pack = self._delta = self._request = None
        self._ops, self._state, self._ack, self._seen = {}, "open", None, None

    def _save(self) -> None:
        """Write the progress into the staging (atomic, 0600) and remember the file this instance wrote."""
        if self._job_id is None or not self._partial.is_dir():
            return
        doc = {
            "v": 1,
            "job_id": self._job_id,
            "created_at": float(self._created_at),
            "state": self._state,
            "facts": to_wire(self._facts) if self._facts is not None else None,
            "final": to_wire(self._final) if self._final is not None else None,
            "capsule": to_wire(self._capsule) if self._capsule is not None else None,
            "pack": to_wire(self._pack) if self._pack is not None else None,
            "delta": to_wire(self._delta) if self._delta is not None else None,
            "request": to_wire(self._request) if self._request is not None else None,
            "ops": {op_id: to_wire(op) for op_id, op in self._ops.items()},
        }
        write_private(self._state_path, canonical_bytes(doc), self._partial / ".tmp")
        self._seen = self._stat_state()

    def _stat_state(self) -> tuple[int, int, int] | None:
        try:
            st = os.lstat(self._state_path)
        except FileNotFoundError:
            return None
        return st.st_mtime_ns, st.st_size, st.st_ino

    @staticmethod
    def _own_private(st: os.stat_result, *, directory: bool) -> bool:
        """The staging and its state file must be this user's and private (a planted state would be signed)."""
        kind_ok = stat.S_ISDIR(st.st_mode) if directory else stat.S_ISREG(st.st_mode)
        if not kind_ok:
            return False
        return not hasattr(os, "geteuid") or (st.st_uid == os.geteuid() and not st.st_mode & 0o077)

    def _read_state(self) -> dict | None:
        """The staging's progress document, or None (none, unreadable, not private to this user)."""
        try:
            if not self._own_private(os.lstat(self._partial), directory=True):
                return None
            if not self._own_private(os.lstat(self._state_path), directory=False):
                return None
            doc = loads_bounded(self._state_path.read_bytes(), max_bytes=MAX_MANIFEST_BYTES)
        except (OSError, BadRequest):
            return None
        return doc if isinstance(doc, dict) and doc.get("v") == 1 else None

    def _apply(self, doc: dict) -> None:
        """Take over a progress document of this job (raises ``BadRequest`` for a malformed one)."""

        def opt(cls, key):
            value = doc.get(key)
            return None if value is None else from_wire(cls, value)

        ops = doc.get("ops") or {}
        if not isinstance(ops, dict):
            raise BadRequest("malformed export state", field="ops")
        self._job_id = str(doc["job_id"])
        self._created_at = float(doc.get("created_at") or 0.0)
        self._state = str(doc.get("state") or "open")
        self._facts = opt(EnvFacts, "facts")
        self._final = opt(FinalPlan, "final")
        self._capsule = opt(CapsuleInfo, "capsule")
        self._pack = opt(PackInfo, "pack")
        self._delta = opt(DeltaInfo, "delta")
        self._request = opt(PrepareRequest, "request")
        self._ops = {str(k): from_wire(OpStatus, v) for k, v in ops.items()}
        self._seen = self._stat_state()

    def _sync(self, job: JobContext) -> None:
        """Before every step: take over what another instance (or process) recorded for ``job`` since this one
        last wrote or read the state file; with no staging left, a bundle this identity committed for ``job``."""
        if self._ack is not None:
            return
        seen = self._stat_state()
        if seen is None:
            if self._job_id in (None, job.job_id) and (self.out.exists() or self.out.is_symlink()):
                ack = self._committed(job)
                if ack is not None:
                    self._job_id, self._state, self._ack = job.job_id, "committed", ack
            return
        if seen == self._seen:
            return
        doc = self._read_state()
        if doc is None or doc.get("job_id") != job.job_id:
            return
        try:
            self._apply(doc)
        except (BadRequest, KeyError, TypeError, ValueError):
            return

    def _committed(self, job: JobContext) -> CommitAck | None:
        """The ack of a bundle at ``out`` this identity signed for ``job`` (a commit that finished before this
        instance existed), else None."""
        try:
            manifest, verified = read_manifest(self.out, trusted_pubkeys={self._identity.public_b64()})
        except (HandoverError, OSError):
            return None
        if not verified or manifest.get("job_id") != job.job_id:
            return None
        self._created_at = float(manifest.get("created_at") or 0.0)
        title = manifest.get("title")
        return self._ack_for(title if isinstance(title, str) else None)

    def _ack_for(self, title: str | None) -> CommitAck:
        return CommitAck(
            session_id="",
            root_id="",
            title=title,
            dests=(),
            path_map=(),
            model=None,
            offline_capable=True,
            warnings=(f"bundle written to {self.out}",),
            committed_at=self._clock(),
        )

    def _job_running(self, job_id: str) -> bool:
        """Whether the conductor job of another export to this path still runs on this profile."""
        lookup = getattr(self._ledger, "job", None)
        row = lookup(job_id, Role.CONDUCTOR) if callable(lookup) else None
        return row is not None and row.get("state") not in (str(ConductorState.DONE), str(ConductorState.ABORTED))

    # -- commit ------------------------------------------------------------------------------------------------
    def _commit(self) -> None:
        if self._capsule is None or self._pack is None or self._final is None or self._request is None:
            # a commit without the prepared staging cannot succeed by repeating it (contract V24)
            raise InvalidState("bundle commit before prepare", state=self._state, reason="not_prepared")
        final_obj = self._sink.put_bytes(canonical_bytes(to_wire(self._final)), MEDIA_JSON)
        facts_obj = self._sink.put_bytes(canonical_bytes(to_wire(self._request.source_facts)), MEDIA_JSON)
        objects = self._sink.all_manifests()
        referenced: dict[str, int] = {}
        for oid, m in objects.items():
            if self._sink.missing(list(m.blocks)):
                raise NotFound("bundle object incomplete", object_id=oid)
            for block_id in m.blocks:
                referenced[block_id] = (self._partial / _block_name(block_id)).stat().st_size
        title = (self._request.note_parts or {}).get("title")
        manifest = {
            "kind": "ctxh.bundle",
            "v": 1,
            "created_at": float(self._created_at),
            "job_id": self._job_id,
            "exporter": {
                "instance_id": self._identity.instance_id,
                "name": self._name,
                "pubkey": self._identity.public_b64(),
                "plugin_version": self._plugin_version,
                "hermes_version": self._hermes_version,
            },
            "retire": self._retire,
            "capsule_id": self._capsule.capsule_id,
            "final_plan_id": final_obj.object_id,
            "pack_id": self._pack.pack_id,
            "facts_id": facts_obj.object_id,
            "plan_id": self._final.plan_id,
            "capsule": to_wire(self._capsule),
            "pack": to_wire(self._pack),
            "delta": to_wire(self._delta) if self._delta is not None else None,
            "title": title if isinstance(title, str) else None,
            "totals": {"blocks": len(referenced), "bytes": sum(referenced.values())},
            "objects": {oid: {"size": m.size, "blocks": len(m.blocks)} for oid, m in sorted(objects.items())},
        }
        manifest_bytes = canonical_bytes(manifest)
        if len(manifest_bytes) > MAX_MANIFEST_BYTES:
            raise HandoverError("bundle manifest too large", size=len(manifest_bytes))
        sig = self._identity.sign(manifest_bytes).encode("ascii")
        tmp_dir = self._partial / ".tmp"
        write_private(self._partial / "CTXH-BUNDLE", MAGIC, tmp_dir)
        write_private(self._partial / "manifest.json", manifest_bytes, tmp_dir)
        write_private(self._partial / "manifest.sig", sig, tmp_dir)
        shutil.rmtree(tmp_dir, ignore_errors=True)
        if self.as_dir:
            for path in (self._partial / "blocks").glob("*/*"):
                if path.parent.name + path.name not in referenced:
                    path.unlink()
            if self.out.exists():
                raise ConflictError(f"{self.out} appeared during the export", path=str(self.out))
            self._state_path.unlink(missing_ok=True)  # not a bundle member; from here on the bundle is the record
            os.rename(self._partial, self.out)
        else:
            self._write_tar(manifest_bytes, sig, objects, referenced)
            shutil.rmtree(self._partial, ignore_errors=True)
        self._state = "committed"
        self._seen = None
        self._ack = self._ack_for(manifest["title"])

    def _write_tar(self, manifest_bytes: bytes, sig: bytes, objects: dict, referenced: dict) -> None:
        tmp = self.out.parent / f"{self.out.name}.tmp"
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        mtime = int(self._created_at)
        try:
            with os.fdopen(fd, "wb") as fh:
                with tarfile.open(fileobj=fh, mode="w", format=tarfile.USTAR_FORMAT) as tf:

                    def add(name: str, path: Path | None = None, data: bytes | None = None) -> None:
                        info = tarfile.TarInfo(name)
                        info.mode = 0o600
                        info.mtime = mtime
                        info.uid = info.gid = 0
                        info.uname = info.gname = ""
                        if data is not None:
                            info.size = len(data)
                            tf.addfile(info, io.BytesIO(data))
                        else:
                            info.size = path.stat().st_size
                            with path.open("rb") as src:
                                tf.addfile(info, src)

                    add("CTXH-BUNDLE", data=MAGIC)
                    add("manifest.json", data=manifest_bytes)
                    add("manifest.sig", data=sig)
                    for oid in sorted(objects):
                        add(_manifest_name(oid), self._partial / _manifest_name(oid))
                    for block_id in sorted(referenced):
                        add(_block_name(block_id), self._partial / _block_name(block_id))
                fh.flush()
                os.fsync(fh.fileno())
            if self.out.exists():
                raise ConflictError(f"{self.out} appeared during the export", path=str(self.out))
            os.replace(tmp, self.out)
        except BaseException:
            try:
                tmp.unlink()
            except FileNotFoundError:
                pass
            raise


# ---------------------------------------------------------------------------------------------------------
# Reader side
# ---------------------------------------------------------------------------------------------------------


class _Reader:
    """Validated member index of a tar or directory bundle; reads members with header-checked size caps."""

    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self._lock = threading.Lock()
        self._tar: tarfile.TarFile | None = None
        self.sizes: dict[str, int] = {}
        self._members: dict[str, tarfile.TarInfo] = {}
        if self.path.is_symlink():
            raise _bad("the bundle path is a symlink")
        if self.path.is_dir():
            self._index_dir()
        elif self.path.is_file():
            self._index_tar()
        else:
            raise NotFound("bundle not found", path=str(self.path))

    @staticmethod
    def _check_name(name: str) -> None:
        if name in _FIXED or _BLOCK_RE.fullmatch(name) or _MANIFEST_RE.fullmatch(name):
            return
        raise _bad("unexpected member", member=name[:200])

    def _add(self, name: str, size: int) -> None:
        self._check_name(name)
        if name in self.sizes:
            raise _bad("duplicate member", member=name)
        if _BLOCK_RE.fullmatch(name) and size > BLOCK_SIZE:
            raise _bad("block member larger than one block", member=name)
        if _MANIFEST_RE.fullmatch(name) and size > MAX_MANIFEST_JSON:
            raise _bad("object manifest too large", member=name, reason="too_large")
        cap = _FIXED.get(name)
        if name == "manifest.json":
            cap = MAX_MANIFEST_BYTES
        if cap is not None and size > cap:
            raise _bad(f"{name} too large", member=name, reason="too_large")
        self.sizes[name] = size

    def _index_tar(self) -> None:
        try:
            tf = tarfile.open(self.path, mode="r:")  # noqa: SIM115 - kept open for streaming reads, closed in close()
        except (tarfile.TarError, OSError) as exc:
            raise _bad(f"not an uncompressed tar ({type(exc).__name__})") from None
        try:
            for info in tf:
                if info.type not in (tarfile.REGTYPE, tarfile.AREGTYPE):
                    raise _bad("only regular files are allowed", member=info.name[:200])
                self._add(info.name, info.size)
                self._members[info.name] = info
        except tarfile.TarError as exc:
            tf.close()
            raise _bad(f"corrupt tar ({type(exc).__name__})") from None
        except BaseException:
            tf.close()
            raise
        self._tar = tf

    def _index_dir(self) -> None:
        root = self.path
        for entry in os.scandir(root):
            rel = entry.name
            if entry.is_symlink():
                raise _bad("symlink in a directory bundle", member=rel)
            if entry.is_dir(follow_symlinks=False):
                if rel not in {"blocks", "manifests"}:
                    raise _bad("unexpected directory", member=rel)
                for sub in os.scandir(entry.path):
                    if sub.is_symlink() or not sub.is_dir(follow_symlinks=False) or not _HEX2.fullmatch(sub.name):
                        raise _bad("unexpected entry", member=f"{rel}/{sub.name}")
                    for leaf in os.scandir(sub.path):
                        name = f"{rel}/{sub.name}/{leaf.name}"
                        if leaf.is_symlink() or not leaf.is_file(follow_symlinks=False):
                            raise _bad("only regular files are allowed", member=name)
                        self._add(name, leaf.stat(follow_symlinks=False).st_size)
            elif entry.is_file(follow_symlinks=False):
                self._add(rel, entry.stat(follow_symlinks=False).st_size)
            else:
                raise _bad("only regular files are allowed", member=rel)

    def read(self, name: str, cap: int) -> bytes:
        size = self.sizes.get(name)
        if size is None:
            raise NotFound("member not in the bundle", member=name)
        if size > cap:
            raise _bad("member too large", member=name, reason="too_large")
        with self._lock:
            if self._tar is not None:
                fh = self._tar.extractfile(self._members[name])
                data = fh.read(cap + 1) if fh is not None else b""
            else:
                fd = os.open(self.path / name, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
                with os.fdopen(fd, "rb") as fh:
                    data = fh.read(cap + 1)
        if len(data) != size:
            raise _bad("member size changed while reading", member=name)
        return data

    def close(self) -> None:
        if self._tar is not None:
            self._tar.close()
            self._tar = None


def _str(value: object, field: str, *, max_len: int = 200, optional: bool = False) -> str | None:
    if value is None and optional:
        return None
    if not isinstance(value, str) or len(value) > max_len:
        raise _bad(f"invalid {field}")
    return value


def _validate(reader: _Reader) -> tuple[dict, bytes, bytes, dict[str, ObjectManifest]]:
    for name in _FIXED:
        if name not in reader.sizes:
            raise _bad(f"missing {name}")
    if reader.read("CTXH-BUNDLE", 64) != MAGIC:
        raise _bad("not a ctxh bundle (magic)")
    manifest_bytes = reader.read("manifest.json", MAX_MANIFEST_BYTES)
    sig = reader.read("manifest.sig", MAX_SIG_BYTES).strip()
    manifest = loads_bounded(manifest_bytes, max_bytes=MAX_MANIFEST_BYTES, max_depth=64)
    if not isinstance(manifest, dict) or manifest.get("kind") != "ctxh.bundle" or manifest.get("v") != 1:
        raise _bad("unsupported bundle manifest")
    job_id = manifest.get("job_id")
    if not isinstance(job_id, str) or not ULID_RE.match(job_id):
        raise _bad("invalid job id")
    exporter = manifest.get("exporter")
    if not isinstance(exporter, dict):
        raise _bad("invalid exporter")
    for key in ("instance_id", "name", "pubkey", "plugin_version", "hermes_version"):
        _str(exporter.get(key), f"exporter.{key}")
    if not isinstance(manifest.get("retire"), bool):
        raise _bad("invalid retire flag")
    totals = manifest.get("totals")
    objects_map = manifest.get("objects")
    if not isinstance(totals, dict) or not isinstance(objects_map, dict):
        raise _bad("invalid totals or objects")
    block_members = {n: s for n, s in reader.sizes.items() if _BLOCK_RE.fullmatch(n)}
    manifest_members = {n for n in reader.sizes if _MANIFEST_RE.fullmatch(n)}
    if totals.get("blocks") != len(block_members) or totals.get("bytes") != sum(block_members.values()):
        raise _bad("totals do not match the members")
    if {_manifest_name(oid) for oid in objects_map} != manifest_members:
        raise _bad("object map does not match the object manifests")
    objects: dict[str, ObjectManifest] = {}
    used: set[str] = set()
    for oid, entry in objects_map.items():
        if not isinstance(entry, dict):
            raise _bad("invalid object entry")
        doc = loads_bounded(reader.read(_manifest_name(oid), MAX_MANIFEST_JSON), max_bytes=MAX_MANIFEST_JSON)
        try:
            m = manifest_from_doc(doc, oid)
        except (BadRequest, BlockCorrupt) as exc:
            raise _bad(f"object manifest {oid[:12]} invalid: {exc.message}") from None
        if entry.get("size") != m.size or entry.get("blocks") != len(m.blocks):
            raise _bad("object entry does not match its manifest", object_id=oid)
        sizes = []
        for block_id in m.blocks:
            name = _block_name(block_id)
            if name not in block_members:
                raise _bad("object block missing", object_id=oid)
            sizes.append(block_members[name])
        if sum(sizes) != m.size:
            raise _bad("object blocks do not add up to its size", object_id=oid)
        used.update(_block_name(b) for b in m.blocks)
        objects[oid] = m
    if used != set(block_members):
        raise _bad("blocks without an object")
    for key in ("capsule_id", "final_plan_id", "pack_id", "facts_id"):
        value = manifest.get(key)
        if value is not None and value not in objects:
            raise _bad(f"{key} not among the objects")
    return manifest, manifest_bytes, sig, objects


def _trusted_keys(ledger: Ledger | None, trusted_pubkeys) -> dict[str, str]:
    """``{public key: name}`` of the keys a bundle signature may verify with. A key several non-revoked peers share
    (one device paired both ways, or a duplicate a failed pairing left, L3-F05) names one of them by a fixed rule —
    the ``dial`` peer first (the way back to the exporter, contract V13), then the oldest entry — never whichever
    name happens to sort last."""
    keys: dict[str, str] = {}
    if ledger is not None:
        live = [p for p in ledger.peers() if p.revoked_at is None and p.pubkey]
        for peer in sorted(live, key=lambda p: (p.direction != "dial", float(p.created_at or 0.0), p.name)):
            keys.setdefault(peer.pubkey, peer.name)
    for key in trusted_pubkeys or ():
        keys.setdefault(key, "trusted key")
    return keys


def _signer(manifest_bytes: bytes, sig: bytes, keys: dict[str, str]) -> str | None:
    try:
        sig_text = sig.decode("ascii")
    except UnicodeDecodeError:
        return None
    for pubkey, name in keys.items():
        if verify(pubkey, manifest_bytes, sig_text):
            return name
    return None


def read_manifest(path: Path, *, trusted_pubkeys=frozenset(), ledger: Ledger | None = None) -> tuple[dict, bool]:
    """Validate a bundle completely (members, manifest, objects, totals) and return ``(manifest, verified)``.

    ``verified`` is True only for a signature by a key in ``trusted_pubkeys`` or a non-revoked peer of ``ledger``.
    """
    reader = _Reader(Path(path))
    try:
        manifest, manifest_bytes, sig, _objects = _validate(reader)
    finally:
        reader.close()
    return manifest, _signer(manifest_bytes, sig, _trusted_keys(ledger, trusted_pubkeys)) is not None


class _BundleBlocks:
    """``BlockSource`` over a validated bundle; every block is hash-checked when read."""

    def __init__(self, reader: _Reader, objects: dict[str, ObjectManifest]) -> None:
        self._reader = reader
        self._objects = objects

    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        try:
            return [self._objects[i] for i in object_ids]
        except KeyError as exc:
            raise NotFound("object not in the bundle", object_id=str(exc)) from None

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        for block_id in ids:
            raw = self._reader.read(_block_name(block_id), BLOCK_SIZE)
            if hashlib.sha256(raw).hexdigest() != block_id:
                raise BlockCorrupt("bundle block does not match its id", block_id=block_id)
            yield block_id, raw

    def object_bytes(self, object_id: str) -> bytes:
        m = self.manifests([object_id])[0]
        if m.size > MAX_MANIFEST_JSON:
            raise _bad("document object too large", object_id=object_id, reason="too_large")
        data = b"".join(raw for _i, raw in self.read_blocks(list(m.blocks)))
        if hashlib.sha256(data).hexdigest() != m.sha256:
            raise BlockCorrupt("bundle object does not match its manifest", object_id=object_id)
        return data


class BundleSource:
    """``SourceAPI`` over a bundle file or directory (import, DESIGN §5.4).

    Raises ``Unauthorized`` for an unverified bundle unless ``trust_unsigned`` (the surface asked the user), and
    ``ConflictError`` for a bundle this profile imported before unless ``again``. Attributes for surfaces:
    ``manifest``, ``verified``, ``signer`` (peer name, ``"trusted key"`` or None), ``is_copy``, ``reimport_of``.
    """

    def __init__(
        self,
        path: Path,
        *,
        trusted_pubkeys,
        trust_unsigned: bool,
        ledger: Ledger,
        again: bool = False,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self.path = Path(path)
        self._ledger = ledger
        self._clock = clock
        self._reader = _Reader(self.path)
        try:
            self.manifest, manifest_bytes, sig, objects = _validate(self._reader)
            self.signer = _signer(manifest_bytes, sig, _trusted_keys(ledger, trusted_pubkeys))
            self.verified = self.signer is not None
            if not self.verified and not trust_unsigned:
                raise Unauthorized(
                    "the bundle is not signed by a paired, non-revoked peer; import it with --trust-unsigned only "
                    "if you know where it comes from",
                    exporter=str(self.manifest["exporter"].get("name", ""))[:64],
                )
            seen = ledger.bundle_import_seen(self.manifest["job_id"])
            if seen and not again:
                raise ConflictError(
                    f"this bundle was already imported here as session {seen}; use --again to import it once more",
                    session_id=seen,
                )
        except BaseException:
            self._reader.close()
            raise
        self.reimport_of = seen
        self.is_copy = not self.manifest["retire"]
        self._blocks = _BundleBlocks(self._reader, objects)
        self._ops: dict[str, OpStatus] = {}

    def close(self) -> None:
        self._reader.close()

    def _doc(self, key: str) -> dict:
        data = loads_bounded(self._blocks.object_bytes(self.manifest[key]), max_bytes=MAX_MANIFEST_JSON)
        if not isinstance(data, dict):
            raise _bad(f"{key} is not a JSON object")
        return data

    def _record(self, op: OpStatus) -> OpStatus:
        self._ops[op.op_id] = op
        return op

    # -- SourceAPI -------------------------------------------------------------------------------------------
    def hello(self) -> Hello:
        exp = self.manifest["exporter"]
        return Hello(
            protocol=PROTOCOL,
            majors=(WIRE_MAJOR,),
            minor=WIRE_MINOR,
            instance_id=exp["instance_id"],
            listener_pubkey=exp["pubkey"],
            plugin_version=exp["plugin_version"],
            hermes_version=exp["hermes_version"],
            name=exp["name"],
            capabilities=BUNDLE_CAPS,
        )

    def list_sessions(self, limit: int, query: str | None, include_archived: bool) -> list[dict]:
        return []

    def facts(self, job: JobContext) -> EnvFacts:
        return from_wire(EnvFacts, self._doc("facts_id"))

    def start_plan(self, job: JobContext, target_facts: EnvFacts, session_ref: SessionRef) -> OpStatus:
        final = from_wire(FinalPlan, self._doc("final_plan_id"))
        return self._record(_done(f"bundle-plan-{job.job_id}", to_wire(final.plan)))

    def lock(self, job: JobContext, session_ref: SessionRef, plan_revision: tuple) -> SessionInfo:
        capsule = from_wire(CapsuleInfo, self.manifest["capsule"])
        return SessionInfo(
            tip_id="",
            root_id="",
            lineage_ids=(),
            title=self.manifest.get("title"),
            model=None,
            provider=None,
            cwd=None,
            git_repo_root=None,
            git_branch=None,
            active_messages=capsule.rows,
            revision=capsule.revision,
        )

    def heartbeat(self, job: JobContext) -> None:
        return None

    def start_export(self, job: JobContext) -> OpStatus:
        return self._record(_done(f"bundle-export-{job.job_id}", dict(self.manifest["capsule"])))

    def start_pack(self, job: JobContext, final_plan: FinalPlan) -> OpStatus:
        return self._record(_done(f"bundle-pack-{job.job_id}", dict(self.manifest["pack"])))

    def start_prepare(self, job: JobContext, prepared: PreparedInfo) -> OpStatus:
        result = PrepareResult(outcome=PrepareOutcome.OK)
        return self._record(_done(f"bundle-prepare-{job.job_id}", to_wire(result)))

    def start_retire(self, job: JobContext, ack: CommitAck) -> OpStatus:
        self._ledger.bundle_import_put(self.manifest["job_id"], ack.session_id)
        return self._record(_done(f"bundle-retire-{job.job_id}", {"state": "imported"}))

    def start_release(self, job: JobContext, reason: str) -> OpStatus:
        return self._record(_done(f"bundle-release-{job.job_id}", {"state": "released"}))

    def op_status(self, job: JobContext, op_id: str) -> OpStatus:
        try:
            return self._ops[op_id]
        except KeyError:
            raise NotFound("operation not known", op_id=op_id) from None

    def status(self, job: JobContext) -> ParticipantStatus:
        return ParticipantStatus(
            state="bundle", step="", epoch=job.epoch, progress=None, ops=dict(self._ops), last_error=None
        )

    def blocks(self) -> _BundleBlocks:
        return self._blocks
