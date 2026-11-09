"""The in-process target participant (layer L6, stdlib only; INTERFACES §4.5, DESIGN §3.3–§3.4, §6, §8.6, §9, §11).

``LocalTarget`` implements ``TargetAPI`` on the machine where the session shall continue — called by a local
conductor (pull, import) or through the registry by a remote one (push). Its core is the two-phase commit:

* ``negotiate`` decides destinations and **checks the disk before anything is transferred** (``free < need ×
  1.1`` ⇒ ``insufficient_storage``) [SK M3, BX M3].
* ``start_prepare`` does the heavy, failure-prone work invisibly: workspace items into partial directories /
  namespaced refs (``materialize.WorkspaceTxn``), the session imported **hidden**, hashes verified, the note
  composed. The ``prepared`` vote is the target's prepare point.
* ``start_commit`` is an asynchronous operation of journaled sub-steps, each with an inverse (DESIGN §3.4):
  c1 workspace items → c2 session cwd → c3 HERMES_HOME files → c4 side state → c5 arrival row → c6 **unhide** →
  c7 ``committed`` (point of no return). A failing forward step switches the journal to ``rollback`` and undoes
  the done steps in reverse; the operation ends ``aborted``. A crash continues in the journaled direction
  (``recover``); a rollback step that fails is recorded as ``needs_attention`` and the session stays hidden.
  ``committing`` cannot be aborted from outside [SK H5].

Working data is journaled on the job's ``target`` row. The B/D/E/F functions are reached through ``ports``
(default: the real modules, imported lazily; unit tests may pass stand-ins).

Workspace transaction (contract X11): ``materialize.WorkspaceTxn(…, cfg=, rules=)``; its ``snapshot()`` is journaled
after PREPARE and as the result of every c1/c3 step (forward and inverse). Any process rebuilds the transaction
with ``WorkspaceTxn.restore(snapshot, …)`` and continues the commit in the journaled direction — steps already
journaled ``done`` never run again. Step names come from E (``c1:<item_id>``) unchanged (W021).

Import (contract X02): the target negotiates destinations for the bundle's plan (its decisions, its own id); the
conductor recomputes the bundle FinalPlan's id and sends it as ``PrepareRequest.final_plan_id``; ``prepare``
accepts a pack only when it belongs to that id.
"""

from __future__ import annotations

import dataclasses
import os
import shutil
import time
from collections.abc import Callable
from datetime import datetime
from pathlib import Path
from types import SimpleNamespace

from . import CAPABILITIES_V1, PROTOCOL, WIRE_MAJOR, WIRE_MINOR
from .contracts import JobContext
from .errors import (
    HandoverError,
    ImportFailed,
    InsufficientStorage,
    InvalidState,
    NotFound,
    PayloadTooLarge,
    VerifyFailed,
)
from .jsonutil import canonical_bytes, from_wire, loads_bounded, sha256_hex, to_wire
from .ledger import STALE_OWNER_S, Fenced, Ledger, private_dir, process_owner
from .model import (
    CapsuleInfo,
    CommitAck,
    ContinueResult,
    DeltaInfo,
    DestMode,
    EnvFacts,
    FinalPlan,
    Hello,
    HermesPaths,
    HostFacts,
    Include,
    ItemKind,
    ItemRole,
    Limits,
    MapRow,
    Mode,
    OpStatus,
    ParticipantStatus,
    PathMapEntry,
    PlanItem,
    PreparedInfo,
    PrepareRequest,
    Progress,
    ResourcePlan,
    Role,
    TargetDecision,
)
from .ops import OpRunner, error_dict
from .policy import HERMES_FILE_KINDS, PolicyConfig, check_target_dest

MAX_DOC_BYTES = 64 * 1024 * 1024
DISK_MARGIN = 1.1
QUOTA_MARGIN = 1.1
"""Staging quota per job on the receiving side: ``need_bytes`` + 10 % (DESIGN §4.7, R28)."""
STATE_DB_FACTOR = 3.5
"""A capsule needs its blocks plus ≈ 3.5 × its size in ``state.db`` (DESIGN §8.6, [RB §B8])."""

CAPSULE_BYTES_PER_TOKEN = 16
"""Capsule bytes per token of the plan's estimate: ≈ 4 characters per token, × 4 for row metadata, tool calls,
``api_content`` sidecars and JSON framing (DESIGN §6.2 example: 980 k tokens ⇒ 12.8 MB capsule)."""
CAPSULE_FIXED_BYTES = 1024 * 1024


def capsule_estimate(approx_tokens: int) -> int:
    """Upper estimate of the capsule's block bytes before it exists (disk check and staging quota, §8.6, R28)."""
    return max(0, int(approx_tokens)) * CAPSULE_BYTES_PER_TOKEN + CAPSULE_FIXED_BYTES


def default_target_ports() -> SimpleNamespace:
    """The real B/D/E/F modules (layer 2 wiring); imported lazily so the module loads without them."""
    from . import (
        ctxrecords,
        envfacts,
        gitops,
        hermes_files,
        materialize,
        note,
        pathmap,
        plan,
        session_export,
        session_import,
    )

    return SimpleNamespace(
        ctxrecords=ctxrecords,
        envfacts=envfacts,
        gitops=gitops,
        hermes_files=hermes_files,
        materialize=materialize,
        note=note,
        pathmap=pathmap,
        plan=plan,
        session_export=session_export,
        session_import=session_import,
    )


def _digest(*parts: object) -> str:
    return sha256_hex(canonical_bytes([p if not dataclasses.is_dataclass(p) else to_wire(p) for p in parts]))


class QuotaSink:
    """The block sink of one target job: every newly stored byte is charged against the job's staging quota
    (``need_bytes`` of the confirmed plan + 10 %, R28); a peer can never store more than the plan allows."""

    def __init__(self, store, ledger: Ledger, job_id: str) -> None:
        self._store, self._ledger, self._job_id = store, ledger, job_id

    def _charge(self, nbytes: int) -> None:
        if nbytes <= 0:
            return
        ok, used, quota = self._ledger.quota_charge(self._job_id, Role.TARGET, nbytes)
        if not ok:
            if quota < 0:
                raise InvalidState("no confirmed plan for this job on the target", reason="no_quota")
            raise InsufficientStorage(
                "The peer sent more data than the confirmed plan allows.",
                reason="job_quota",
                quota_bytes=quota,
                used_bytes=used,
                request_bytes=nbytes,
            )

    def put_manifests(self, manifests) -> int:
        new = [m for m in manifests if not self._has_manifest(m.object_id)]
        self._charge(sum(256 + 70 * len(m.blocks) for m in new))
        stored = self._store.put_manifests(manifests)
        self._ledger.job_objects_add(self._job_id, [m.object_id for m in manifests])  # GC keeps them (§5.1)
        return stored

    def _has_manifest(self, object_id: str) -> bool:
        try:
            self._store.manifests([object_id])
        except HandoverError:
            return False
        return True

    def missing(self, ids):
        return self._store.missing(ids)

    def write_blocks(self, frames):
        frames = list(frames)
        new_ids = set(self._store.missing([bid for bid, _raw in frames]))
        self._charge(sum(len(raw) for bid, raw in frames if bid in new_ids))
        return self._store.write_blocks(frames)

    def manifests(self, ids):
        return self._store.manifests(ids)

    def read_blocks(self, ids):
        return self._store.read_blocks(ids)


def _no_row(_remote_path: str, _direction: str) -> None:
    return None


def _existing_ancestor(path: Path) -> Path:
    p = path
    while not p.exists():
        if p.parent == p:
            break
        p = p.parent
    return p


class LocalTarget:
    """``TargetAPI`` over this machine's session store and write roots (see module docstring)."""

    def __init__(
        self,
        *,
        ledger: Ledger,
        store,
        db_factory: Callable,
        hermes: HermesPaths,
        host: HostFacts,
        config: dict,
        ops: OpRunner,
        git,
        provider_usable: Callable[[str, dict], bool],
        continuation,
        redact: Callable[[str], str],
        source_peer_id: str | None,
        peer_verified: bool,
        policy: PolicyConfig,
        ports: SimpleNamespace | None = None,
        clock: Callable[[], float] | None = None,
        data_dir: Path | None = None,
        disk_usage: Callable = shutil.disk_usage,
        instance_name: str = "",
        settings: dict | None = None,
        peer_name: str | None = None,
    ) -> None:
        self.ledger = ledger
        self.store = store
        self.db_factory = db_factory
        self.hermes = hermes
        self.host = host
        self.config = config
        self.ops = ops
        self.git = git
        self.provider_usable = provider_usable
        self.continuation = continuation
        self.redact = redact
        self.source_peer_id = source_peer_id
        self.peer_verified = peer_verified
        self.policy = policy
        self._ports = ports
        self._clock = clock or time.time
        self.data_dir = Path(data_dir) if data_dir else Path(hermes.home) / "plugin-data" / "ctx_handover"
        self.disk_usage = disk_usage
        self.instance_name = instance_name or host.profile
        self.settings = dict(settings or {})
        self.peer_name = peer_name

    # -- plumbing ------------------------------------------------------------------------------------------------
    @property
    def ports(self) -> SimpleNamespace:
        if self._ports is None:
            self._ports = default_target_ports()
        return self._ports

    def _ensure_job(self, job: JobContext) -> None:
        if self.ledger.job(job.job_id, Role.TARGET) is None:
            peer_id = job.peer.peer_id if job.peer is not None else None
            self.ledger.create_job(job.job_id, Role.TARGET, job.mode, peer_id, {"role": "target", "local": True})

    def _epoch(self, job: JobContext) -> int:
        self._ensure_job(job)
        epoch = self.ledger.claim_owner(job.job_id, Role.TARGET, process_owner(), STALE_OWNER_S)
        if epoch is None:
            raise InvalidState("another process is working on this job", reason="busy_elsewhere", retryable=True)
        return epoch

    def _row(self, job: JobContext) -> dict:
        row = self.ledger.job(job.job_id, Role.TARGET)
        if row is None:
            raise NotFound(f"job {job.job_id} not found")
        return row

    def _ctx(self, job_id: str) -> dict:
        out: dict = {}
        for ev in self.ledger.events(job_id, Role.TARGET):
            data = ev["data"] or {}
            if isinstance(data.get("ctx"), dict):
                out.update(data["ctx"])
        return out

    def _remember(self, job: JobContext, **values: object) -> None:
        enc = {k: (to_wire(v) if dataclasses.is_dataclass(v) else v) for k, v in values.items()}
        self.ledger.event(job.job_id, Role.TARGET, "ctx", {"ctx": enc}, epoch=self._epoch(job))

    def _go(self, job: JobContext, expect, to: str, **ctx: object) -> None:
        enc = {k: (to_wire(v) if dataclasses.is_dataclass(v) else v) for k, v in ctx.items()}
        self.ledger.transition(job.job_id, Role.TARGET, expect, to, {"ctx": enc} if enc else {}, epoch=self._epoch(job))

    def _start(
        self, job: JobContext, op: str, digest: str, fn, *, restart_failed: bool = False, exclusive: bool = False
    ) -> OpStatus:
        self._epoch(job)
        return self.ops.start(
            job.job_id, op, _digest(op, digest), fn, restart_failed=restart_failed, exclusive=exclusive
        )

    def _staging(self, job_id: str) -> Path:
        return private_dir(self.data_dir / "staging" / job_id)

    def _read_doc(self, object_id: str) -> dict:
        with self.store.open_object(object_id) as fh:
            data = fh.read(MAX_DOC_BYTES + 1)
        doc = loads_bounded(data, max_bytes=MAX_DOC_BYTES)
        if not isinstance(doc, dict):
            raise VerifyFailed("document is not an object", object_id=object_id)
        return doc

    def _remote_driven(self, job: JobContext) -> bool:
        return job.mode is Mode.PUSH and self.source_peer_id is not None

    # -- TargetAPI -----------------------------------------------------------------------------------------------
    def hello(self) -> Hello:
        return Hello(
            protocol=PROTOCOL,
            majors=(WIRE_MAJOR,),
            minor=WIRE_MINOR,
            instance_id=self.ledger.instance_id(),
            listener_pubkey="",
            plugin_version=self.host.plugin_version,
            hermes_version=self.host.hermes_version,
            name=self.instance_name,
            capabilities=frozenset(CAPABILITIES_V1 - {"operator.v1"}),
            limits=Limits(),
        )

    def open_job(self, job: JobContext) -> None:
        self._ensure_job(job)

    def facts(self, job: JobContext, source_facts: EnvFacts) -> EnvFacts:
        """This host's facts; the tool names the source saw in its transcript are looked up here too (W084)."""
        self._ensure_job(job)
        seen = sorted((source_facts.tools_seen or {}) if source_facts is not None else ())
        tf = self.ports.envfacts.probe(
            instance_name=self.instance_name,
            options=job.options,
            host=self.host,
            config=self.config,
            tools_seen=seen,
            capabilities=self.hello().capabilities,
        )
        self._remember(job, source_facts=source_facts, target_facts=tf)
        return tf

    def _facts(self, job: JobContext) -> tuple[EnvFacts | None, EnvFacts | None]:
        ctx = self._ctx(job.job_id)
        sf = from_wire(EnvFacts, ctx["source_facts"]) if ctx.get("source_facts") else None
        tf = from_wire(EnvFacts, ctx["target_facts"]) if ctx.get("target_facts") else None
        return sf, tf

    def _peer_key(self) -> str:
        """The workspace-map peer of this job's source: the paired peer, else ``local`` (bundle, own host)."""
        return self.source_peer_id or "local"

    def _lookup(self) -> Callable[[str, str], MapRow | None]:
        """``pathmap``'s map lookup bound to the job's source peer (contract X10): ``out`` = a path this host handed
        to that peer earlier (return trip), ``in`` = a path received from it before."""
        peer_id = self._peer_key()
        return lambda remote_path, direction: self.ledger.map_find(peer_id, remote_path, direction)

    def _fallback_for(self, job: JobContext, item: PlanItem, sf: EnvFacts | None, tf: EnvFacts | None, now):
        """``assess``'s fallback for one item: extras go to ``<workspace_root>/_handover/<job6>/<home-relative>``
        (W102, the extras rule of ``pathmap``), everything else to ``pathmap.fallback_dest``."""
        p = self.ports

        def generic(d: str) -> str:
            return p.pathmap.fallback_dest(
                d, now=now, workspace_root=job.options.workspace_root, allow_roots=job.options.allow_roots
            )

        if item.kind not in (ItemKind.EXTRA_FILE, ItemKind.EXTRA_DIR) or sf is None or tf is None:
            return generic
        extra, _reason = p.pathmap.choose_dest(
            item,
            source_facts=sf,
            target_facts=tf,
            lookup=_no_row,
            options=job.options,
            local_choice=False,
            job_id=job.job_id,
            cfg=self.policy,
        )
        return lambda d: generic(extra) if os.path.lexists(os.path.expanduser(extra)) else extra

    def negotiate(self, job: JobContext, plan: ResourcePlan) -> FinalPlan:
        """Destinations, modes, conflicts and the disk check — before any byte moves (DESIGN §8.6)."""
        p = self.ports
        self._ensure_job(job)
        sf, tf = self._facts(job)
        if tf is None:
            tf = self.facts(job, sf) if sf is not None else None
        peer_id = self._peer_key()
        tmp = private_dir(self._staging(job.job_id) / "assess")
        now = datetime.fromtimestamp(self._clock())
        decisions: list[TargetDecision] = []
        items = [i for i in plan.items if i.include is not Include.NO]
        local_choice = not self._remote_driven(job)  # destinations a counterpart typed stay inside allow_roots (R19)
        chosen = p.pathmap.choose_all(
            items,
            source_facts=sf,
            target_facts=tf,
            lookup=self._lookup(),
            options=job.options,
            local_choice=local_choice,
            job_id=job.job_id,
            cfg=self.policy,
        )
        probe = p.gitops.GitProbe(self.git, profile="received")
        for item, (dest, reason) in zip(items, chosen, strict=True):
            out_row = self.ledger.map_out(dest, peer_id)
            in_row = self.ledger.map_in(dest, peer_id)
            a = p.materialize.assess(
                item,
                dest,
                git=self.git,
                probe=probe,
                out_row=out_row,
                tree_fingerprint=in_row.state if in_row is not None and isinstance(in_row.state, dict) else None,
                target_facts=tf,
                fallback=self._fallback_for(job, item, sf, tf, now),
                tmp=tmp,
                cfg=self.policy,
                recorded=out_row is not None or in_row is not None or reason == "return-mapping",
                local_choice=reason == "user" and local_choice,
            )
            final_dest = a.conflict.fallback_dest if a.conflict is not None else dest
            decisions.append(
                TargetDecision(
                    item_id=item.item_id,
                    dest=final_dest,
                    mode=a.mode,
                    reason=reason,
                    conflict=a.conflict,
                    git_have=tuple(a.git_have),
                    need_bytes=int(a.need_bytes),
                    warnings=tuple(a.warnings),
                )
            )
        need, free, short = self._disk_check(decisions, plan)
        if short is not None:
            path, need_b, free_b = short
            raise InsufficientStorage(
                f"{self.instance_name} needs {need_b / 1024**3:.1f} GiB free in {path}, has "
                f"{free_b / 1024**3:.1f} GiB. Nothing was transferred.",
                need_bytes=need_b,
                free_bytes=free_b,
                path=path,
            )
        final = FinalPlan(
            plan_id="",
            plan=plan,
            decisions=tuple(decisions),
            need_bytes=need,
            free_bytes=free,
            model_change=self._model_change(job, sf),
        )
        final = dataclasses.replace(final, plan_id=p.plan.plan_id(p.plan.final_doc(final)))
        self._remember(job, final_plan=final)
        self.ledger.quota_set(job.job_id, Role.TARGET, int(need * QUOTA_MARGIN))
        row = self._row(job)
        if row["state"] in ("open", "negotiated"):
            self._go(job, ("open", "negotiated"), "negotiated")
        return final

    def _disk_check(self, decisions, plan: ResourcePlan) -> tuple[int, dict[str, int], tuple | None]:
        """Need per filesystem: items at their destination, the capsule (blocks + state.db) in the data dir."""
        by_dev: dict[int, dict] = {}

        def add(path: Path, need: int) -> None:
            anchor = _existing_ancestor(path)
            dev = os.stat(anchor).st_dev
            slot = by_dev.setdefault(dev, {"path": str(anchor), "need": 0})
            slot["need"] += max(0, int(need))

        for d in decisions:
            if d.mode is not DestMode.SKIP:
                add(Path(os.path.expanduser(d.dest)), d.need_bytes)
        capsule_bytes = capsule_estimate(plan.approx_tokens)
        add(self.data_dir, int(capsule_bytes * (1 + STATE_DB_FACTOR)))
        free: dict[str, int] = {}
        short = None
        total = 0
        for slot in by_dev.values():
            avail = int(self.disk_usage(slot["path"]).free)
            free[slot["path"]] = avail
            total += slot["need"]
            if short is None and avail < slot["need"] * DISK_MARGIN:
                short = (slot["path"], int(slot["need"] * DISK_MARGIN), avail)
        return total, free, short

    def _model_change(self, job: JobContext, sf: EnvFacts | None) -> dict | None:
        """``model_change`` of the FinalPlan exactly as B's ``map_model`` reports it (contract X04), for the model
        and provider the source reported; the import re-maps the tip row itself (``plan_import``)."""
        if sf is None:
            return None
        row = {"model": sf.host.model, "model_config": {"provider": sf.host.provider} if sf.host.provider else None}
        _fields, change = self.ports.session_import.map_model(
            row, job.options.model_mapping, job.options.model, self.config, self.provider_usable
        )
        return change

    def _final(self, job: JobContext) -> FinalPlan:
        raw = self._ctx(job.job_id).get("final_plan")
        if raw is None:
            raise InvalidState("negotiate first", reason="no_plan")
        return from_wire(FinalPlan, raw)

    def _cwd_for(self, final: FinalPlan, workspace_root: str) -> Callable[[str | None], str]:
        """Session cwd on this side: the source cwd mapped into the primary item's destination, else the
        handover root (R11: always derived from the session row, never from ``ctx.session_cwd()``)."""
        primary: PlanItem | None = next((i for i in final.plan.items if i.role is ItemRole.PRIMARY), None)
        dests = {d.item_id: d.dest for d in final.decisions}

        def cwd_for(source_cwd: str | None) -> str:
            if primary is not None and primary.item_id in dests and source_cwd:
                return self.ports.pathmap.map_cwd(source_cwd, primary, dests[primary.item_id])
            return os.path.expanduser(workspace_root)

        return cwd_for

    def _import_plan(self, job: JobContext, capsule_doc: dict, final: FinalPlan):
        return self.ports.session_import.plan_import(
            self.db_factory(),
            capsule_doc,
            cwd_for=self._cwd_for(final, job.options.workspace_root),
            model_policy=job.options.model_mapping,
            model_override=job.options.model,
            target_config=self.config,
            provider_usable=self.provider_usable,
            max_session_bytes=job.options.max_session_bytes,
        )

    def start_stage_context(self, job: JobContext, capsule: CapsuleInfo) -> OpStatus:
        def fn(report) -> dict:
            if capsule.size > job.options.max_session_bytes:
                raise PayloadTooLarge(
                    "the session is larger than max_session_mb here",
                    size=capsule.size,
                    limit=job.options.max_session_bytes,
                )
            doc = self.ports.session_export.read_capsule(self.store, capsule.capsule_id, max_json_bytes=MAX_DOC_BYTES)
            plan = self._import_plan(job, doc, self._final(job))
            path = "B'" if any(getattr(s, "path", "A") != "A" for s in plan.segments) else "A"
            self.ledger.job_object(job.job_id, capsule.capsule_id)
            row = self._row(job)
            if row["state"] in ("open", "negotiated", "context_staged", "resetting", "data_staged"):
                self._go(job, row["state"], "context_staged", capsule=capsule)
            else:
                self._remember(job, capsule=capsule)
            return {"segments": capsule.segments, "rows": capsule.rows, "bytes": capsule.size, "import_path": path}

        return self._start(job, "stage_context", capsule.capsule_id, fn)

    def _journal_fn(self, job: JobContext) -> Callable[[str, str, dict], None]:
        def journal(kind: str, step: str, data: dict) -> None:
            self.ledger.event(
                job.job_id, Role.TARGET, "ws", {"ws_kind": kind, "step": step, "data": data}, epoch=self._epoch(job)
            )

        return journal

    def _latest_snapshot(self, job_id: str) -> dict | None:
        """The newest ``WorkspaceTxn.snapshot()`` in the journal: after PREPARE (ctx ``ws_snapshot``) and after each
        c1/c3 step of the commit, forward or rollback (step ``result.ws``)."""
        snap = None
        for ev in self.ledger.events(job_id, Role.TARGET):
            data = ev["data"] or {}
            ctx = data.get("ctx")
            if isinstance(ctx, dict) and "ws_snapshot" in ctx:
                snap = ctx["ws_snapshot"]
            result = data.get("result")
            if ev["kind"] == "done" and isinstance(result, dict) and "ws" in result:
                snap = result["ws"]
        return snap

    def _txn(self, job: JobContext, *, restore: bool):
        """The job's workspace transaction: new, or rebuilt in this process from the journaled snapshot (E's
        ``WorkspaceTxn.snapshot``/``restore``) — a crashed commit continues in another process."""
        ws = self.ports.materialize.WorkspaceTxn
        staging = self._staging(job.job_id)
        journal = self._journal_fn(job)
        kw = {"cfg": self.policy, "rules": self.policy.platform_rules(), "now": self._clock, "cancelled": job.cancelled}
        snap = self._latest_snapshot(job.job_id) if restore else None
        if snap is not None:
            return ws.restore(snap, staging, self.git, self.store, journal, **kw)
        return ws(job.job_id, staging, self.git, self.store, journal, **kw)

    def start_prepare(self, job: JobContext, req: PrepareRequest) -> OpStatus:
        def fn(report) -> dict:
            return to_wire(self._prepare(job, req, report))

        return self._start(
            job, "prepare", _digest(req.capsule.capsule_id, req.pack.pack_id, req.final_plan_id), fn, exclusive=True
        )

    def _prepare(self, job: JobContext, req: PrepareRequest, report) -> PreparedInfo:
        p = self.ports
        row = self._row(job)
        if row["state"] in ("committing", "committed", "aborting", "aborted", "expired"):
            raise InvalidState(f"the target is {row['state']}", reason="state", state=row["state"])
        ctx = self._ctx(job.job_id)
        self._go(job, row["state"], "preparing")
        final = self._final(job)
        db = self.db_factory()
        capsule_doc = p.session_export.read_capsule(self.store, req.capsule.capsule_id, max_json_bytes=MAX_DOC_BYTES)
        pack_doc = self._read_doc(req.pack.pack_id)
        if pack_doc.get("kind") != "ctxh.pack" or pack_doc.get("final_plan_id") != req.final_plan_id:
            raise VerifyFailed("the pack does not belong to the confirmed final plan", which="final_plan_id")
        self._remember(job, case_pairs=self._case_pairs(pack_doc))
        items = {i.item_id: i for i in final.plan.items}
        decisions = {d.item_id: d for d in final.decisions}
        peer_id = self._peer_key()
        # workspace: prepare items not yet prepared (a re-pack prepares again from scratch)
        prior_pack = ctx.get("prepared_pack")
        if prior_pack is not None and prior_pack != req.pack.pack_id:
            self._txn(job, restore=True).discard()  # a re-pack prepares the work trees again from scratch
            self._remember(job, ws_snapshot=None, prepared_pack=None)
        hermes_files = []
        if prior_pack != req.pack.pack_id:
            txn = self._txn(job, restore=False)
            for pack_item in pack_doc.get("items") or []:
                item = items.get(pack_item.get("item_id"))
                decision = decisions.get(pack_item.get("item_id"))
                if item is None or decision is None:
                    raise VerifyFailed("pack item not in the final plan", item=pack_item.get("item_id"))
                if decision.mode is DestMode.SKIP or (
                    not pack_item.get("object") and item.kind in (ItemKind.EXTRA_FILE, ItemKind.HERMES_FILE)
                ):
                    continue
                if item.kind is ItemKind.HERMES_FILE:
                    hermes_files.append(pack_item)
                    continue
                row = self.ledger.map_out(decision.dest, peer_id) or self.ledger.map_in(decision.dest, peer_id)
                txn.prepare_item(item, decision, pack_item, out_row=row)
            dests = [list(d) for d in txn.dests()]
            self._remember(job, ws_snapshot=txn.snapshot())
        else:
            dests = ctx.get("dests") or []
            hermes_files = ctx.get("hermes_files") or []
        # session: reuse a hidden import of the same capsule, otherwise import hidden now
        prepared_raw = ctx.get("prepared")
        if prepared_raw is not None and ctx.get("prepared_capsule") == req.capsule.capsule_id:
            prepared = from_wire(PreparedInfo, prepared_raw)
        else:
            import_plan = self._import_plan(job, capsule_doc, final)
            prepared = p.session_import.import_hidden(
                db,
                self.store,
                req.capsule.capsule_id,
                import_plan,
                cancelled=job.cancelled,
                local_hermes_version=self.host.hermes_version,
                max_session_bytes=job.options.max_session_bytes,
            )
            if (prepared.h_all, prepared.h_rows, prepared.h_model) != (
                req.capsule.h_all,
                req.capsule.h_rows,
                req.capsule.h_model,
            ):
                p.session_import.discard(db, prepared)
                raise VerifyFailed("The imported transcript did not match. Nothing was activated.", which="hashes")
            tip = import_plan.segments[-1] if import_plan.segments else None
            session = capsule_doc.get("session") if isinstance(capsule_doc.get("session"), dict) else {}
            self._remember(
                job,
                new_ids=[s.new_id for s in import_plan.segments],
                cwd=import_plan.cwd,
                model_change=import_plan.model_change,
                tip_model=tip.model if tip is not None else None,
                tip_model_config=(tip.model_config if tip is not None else None) or {},
                source_session={
                    "root_id": str(session.get("root_id") or ""),
                    "tip_id": str(session.get("tip_id") or ""),
                },
            )
        placed, skipped = self._hermes_placement(hermes_files, items, prepared.tip_id)
        prepared = dataclasses.replace(
            prepared,
            dests=tuple((a, b, DestMode(c)) for a, b, c in dests),
            warnings=tuple(dict.fromkeys((*prepared.warnings, *skipped))),
        )
        path_map = [
            PathMapEntry(
                source=items[i].source_path,
                target=dest,
                kind="workspace" if items[i].role in (ItemRole.PRIMARY, ItemRole.SECONDARY) else "extra",
            )
            for i, dest, _m in dests
            if i in items
        ]
        path_map.append(
            PathMapEntry(source=req.source_facts.host.hermes_home, target=self.hermes.home, kind="hermes_home")
        )
        note_text, report_path = self._compose_note(job, req, final, prepared, path_map, delta_message=False)
        self._remember(
            job,
            prepared_pack=req.pack.pack_id,
            hermes_files=hermes_files,
            hermes_place=placed,
            dests=dests,
            path_map=[to_wire(e) for e in path_map],
            note=note_text,
            report_path=report_path,
            note_request=to_wire(req),
            prepared_capsule=req.capsule.capsule_id,
        )
        self._go(job, "preparing", "prepared", prepared=prepared)
        return prepared

    def _case_pairs(self, pack_doc: dict) -> list[list[str]]:
        """Contract V09: on a case-insensitive file system here (``PolicyConfig.case_insensitive``) the pack items'
        name pairs that differ only in case (E's ``[kept, colliding]`` pairs) collide; the note names both."""
        if not self.policy.case_insensitive:
            return []
        pairs: list[list[str]] = []
        for item in pack_doc.get("items") or ():
            for pair in (item.get("case_collisions") if isinstance(item, dict) else None) or ():
                if isinstance(pair, (list, tuple)) and len(pair) == 2 and list(pair) not in pairs:
                    pairs.append([str(pair[0]), str(pair[1])])
        return pairs

    def _hermes_placement(self, pack_items: list, items: dict, tip_id: str) -> tuple[list[dict], list[str]]:
        """Target paths of the received session files from kind + basename only (contract X17, hardening #7): the
        kind is the plan item's ``hermes_kind`` (the pack item's must agree), the name the basename of ``rel``.
        A file of unknown or contradicting kind, or with a name the policy refuses, is not placed — never guessed
        — and named in the warnings."""
        placed: list[dict] = []
        skipped: list[str] = []
        for pack_item in pack_items:
            item = items.get(pack_item.get("item_id"))
            plan_kind = item.hermes_kind if item is not None else None
            pack_kind = pack_item.get("hermes_kind")
            kind = plan_kind or pack_kind
            name = str(pack_item.get("rel") or "").replace("\\", "/").rsplit("/", 1)[-1]
            if kind not in HERMES_FILE_KINDS or (plan_kind and pack_kind and plan_kind != pack_kind):
                skipped.append(f"session file {name or '?'} not placed: unknown kind")
                continue
            try:
                (m,) = self.store.manifests([pack_item["object"]])
                dest = self.ports.hermes_files.target_path(kind, name, m.sha256, self.hermes, tip_id)
            except (HandoverError, KeyError, ValueError):
                skipped.append(f"session file {name or '?'} not placed: refused name or missing object")
                continue
            placed.append({"pack_item": {**pack_item, "hermes_kind": kind}, "target": str(dest)})
        return placed, skipped

    def _compose_note(
        self, job, req: PrepareRequest, final: FinalPlan, prepared: PreparedInfo, path_map, *, delta_message: bool
    ) -> tuple[str, str]:
        """The handover note and report (W082): ``note_parts`` of the conductor, the instruction files listed by THIS
        host (``plan.instruction_files``: new or different at the destination, read here — a counterpart's list is
        never taken over, SEC 4.3), the model change the import actually made (B ``plan_import``)."""
        p = self.ports
        ctx = self._ctx(job.job_id)
        sf, tf = req.source_facts, self._facts(job)[1]
        items = [(i, d) for i in final.plan.items for d in final.decisions if d.item_id == i.item_id]
        reports = private_dir(self.data_dir / "reports")
        report_path = reports / f"{job.job_id}.md"
        kind = "bundle" if job.mode is Mode.IMPORT else "peer"
        origin = f"{'verified' if self.peer_verified else 'unverified'} {kind}"
        if job.mode is Mode.IMPORT and not self.source_peer_id:
            # V13: no paired peer is bound to this import, so nothing that arrived can find its way back
            origin += "; no return path to the origin"
        if job.mode is Mode.IMPORT:  # DESIGN §5.4 (re-import guard): every bundle import note says so
            origin += "; this bundle may have been imported elsewhere too"
        note_parts = dict(req.note_parts or {})
        dests = {d.item_id: d.dest for d in final.decisions}
        note_parts["instruction_files"] = list(p.plan.instruction_files(final.plan.items, dests.get))
        pairs = ctx.get("case_pairs") or []
        if pairs:
            texts = list(note_parts.get("case_collisions") or [])
            texts += [t for t in p.note.case_collision_texts(pairs) if t not in texts]
            note_parts["case_collisions"] = texts
        model_change = ctx.get("model_change", final.model_change)
        note_text, report_md = p.note.compose(
            job_id=job.job_id,
            source=sf,
            target=tf,
            delta=p.envfacts.delta(sf, tf),
            path_map=path_map,
            items=items,
            note_parts=note_parts,
            model_change=model_change,
            pack_time=note_parts.get("pack_time") or 0,
            delta_message=delta_message,
            report_path=str(report_path),
            redact=self.redact,
            untrusted_text=final.plan.notes_for_target,
            origin=origin,
            peer_name=self.peer_name,
        )
        fd = os.open(report_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(report_md)
        return note_text, str(report_path)

    def start_apply_delta(self, job: JobContext, delta: DeltaInfo) -> OpStatus:
        def fn(report) -> dict:
            row = self._row(job)
            if row["state"] != "prepared":
                raise InvalidState(f"the target is {row['state']}", reason="state", state=row["state"])
            ctx = self._ctx(job.job_id)
            prepared = from_wire(PreparedInfo, ctx["prepared"])
            self._go(job, "prepared", "delta_applying")
            try:
                new = self.ports.session_import.apply_delta(self.db_factory(), self.store, delta, prepared)
            except HandoverError:
                self._go(job, "delta_applying", "prepared")
                raise
            new = dataclasses.replace(new, dests=prepared.dests)
            req = from_wire(PrepareRequest, ctx["note_request"])
            path_map = [from_wire(PathMapEntry, e) for e in ctx.get("path_map") or []]
            note_text, report_path = self._compose_note(job, req, self._final(job), new, path_map, delta_message=True)
            self.ledger.job_object(job.job_id, delta.delta_id)
            self._go(job, "delta_applying", "prepared", prepared=new, note=note_text, report_path=report_path)
            return to_wire(new)

        return self._start(job, "apply_delta", delta.delta_id, fn, exclusive=True)

    def start_reset_session(self, job: JobContext) -> OpStatus:
        def fn(report) -> dict:
            row = self._row(job)
            if row["state"] in ("committing", "committed", "aborted", "expired"):
                raise InvalidState(f"the target is {row['state']}", reason="state", state=row["state"])
            ctx = self._ctx(job.job_id)
            self._go(job, row["state"], "resetting")
            if ctx.get("prepared") is not None:
                self.ports.session_import.discard(self.db_factory(), from_wire(PreparedInfo, ctx["prepared"]))
            self._go(job, "resetting", "context_staged", prepared=None, prepared_capsule=None)
            return {}

        ctx = self._ctx(job.job_id)
        return self._start(job, "reset_session", str(ctx.get("prepared_capsule")), fn, exclusive=True)

    # -- commit (DESIGN §3.4) --------------------------------------------------------------------------------------
    def start_commit(self, job: JobContext) -> OpStatus:
        row = self._row(job)
        if row["state"] == "prepared":
            self._go(job, "prepared", "committing")
        elif row["state"] not in ("committing", "committed"):
            raise InvalidState(f"the target is {row['state']}", reason="state", state=row["state"])
        return self._start(job, "commit", "commit", lambda report: self._commit_body(job, report), exclusive=True)

    def recover(self, job: JobContext) -> OpStatus | None:
        """Janitor: a ``committing`` target whose process died continues its commit op in the journaled direction
        (DESIGN §3.6); ``preparing`` with a dead op is reported interrupted so the conductor restarts it."""
        row = self._row(job)
        if row["state"] != "committing":
            return None
        op = self.ledger.op_get(job.job_id, "commit", _digest("commit", "commit"))
        if op is not None:
            self.ops.status(job.job_id, op["op_id"])  # marks a dead running op interrupted
        return self._start(
            job, "commit", "commit", lambda report: self._commit_body(job, report), restart_failed=True, exclusive=True
        )

    def _cwd_step(self, cwd: str | None):
        def forward() -> dict:
            created: list[str] = []
            if not cwd:
                return {"created": created}
            target = Path(cwd)
            if target.exists():
                return {"created": created}
            verdict = check_target_dest(target, self.policy, recorded=False, local_choice=False)
            if not verdict.allowed:
                return {"created": created, "denied": verdict.reason}
            missing = []
            p = target
            while not p.exists():
                missing.append(p)
                p = p.parent
            for d in reversed(missing):
                d.mkdir(mode=0o700)
                created.append(str(d))
            return {"created": created}

        def inverse(data: dict | None) -> None:
            for d in reversed((data or {}).get("created") or []):
                try:
                    Path(d).rmdir()
                except OSError:
                    pass

        return forward, inverse

    def _steps(self, job: JobContext) -> list[tuple[str, Callable[[], dict | None], Callable[[dict | None], None]]]:
        p = self.ports
        ctx = self._ctx(job.job_id)
        prepared = from_wire(PreparedInfo, ctx["prepared"])
        db = self.db_factory()
        txn = self._txn(job, restore=True)
        steps: list = []

        def snap_after(fn):  # every workspace step journals the transaction's new snapshot (restore point)
            def run(*_a):
                fn()
                return {"ws": txn.snapshot()}

            return run

        for name, fwd, inv in txn.commit_steps():  # E names them ``c1:<item_id>`` (W021: used as they are)
            steps.append((name, snap_after(fwd), snap_after(inv)))
        fwd2, inv2 = self._cwd_step(ctx.get("cwd"))
        steps.append(("c2:cwd", fwd2, inv2))
        files = [(e["pack_item"], Path(e["target"])) for e in ctx.get("hermes_place") or []]  # placed in prepare
        f3, i3 = txn.place_hermes_files(files)
        steps.append(("c3:hermes_files", snap_after(f3), snap_after(i3)))
        capsule = from_wire(CapsuleInfo, ctx["capsule"]) if ctx.get("capsule") else None

        def side_fwd() -> None:
            if capsule is not None:
                doc = p.session_export.read_capsule(self.store, capsule.capsule_id, max_json_bytes=MAX_DOC_BYTES)
                p.session_import.write_side_state(db, doc, prepared.tip_id)

        steps.append(("c4:side_state", side_fwd, lambda d: p.session_import.clear_side_state(db, prepared.tip_id)))
        continue_prompt = None
        steps.append(
            (
                "c5:arrival",
                lambda: self.ledger.arrival_put(
                    session_ids=list(ctx.get("new_ids") or [prepared.tip_id]),
                    root_id=prepared.root_id,
                    job_id=job.job_id,
                    peer=self.peer_name or (job.peer.name if job.peer else None),
                    note=str(ctx.get("note") or ""),
                    report_path=str(ctx.get("report_path") or ""),
                    path_map=[from_wire(PathMapEntry, e) for e in ctx.get("path_map") or []],
                    continue_prompt=continue_prompt,
                ),
                lambda d: self.ledger.arrival_delete(job.job_id),
            )
        )
        steps.append(
            ("c6:unhide", lambda: p.session_import.unhide(db, prepared), lambda d: p.session_import.hide(db, prepared))
        )
        return steps

    def _journal_state(self, job_id: str) -> tuple[str, list[str], set[str], dict]:
        direction, done_fwd, done_inv, results = "forward", [], set(), {}
        for ev in self.ledger.events(job_id, Role.TARGET):
            data = ev["data"] or {}
            if ev["kind"] == "direction":
                direction = data.get("direction", direction)
            elif ev["kind"] == "done" and str(data.get("step", "")).startswith("c"):
                if data.get("direction") == "rollback":
                    done_inv.add(data["step"])
                elif data["step"] not in done_fwd:
                    done_fwd.append(data["step"])
                    results[data["step"]] = data.get("result")
        return direction, done_fwd, done_inv, results

    def _step_event(self, job: JobContext, kind: str, step: str, direction: str, result: dict | None = None) -> None:
        data = {"step": step, "direction": direction}
        if result is not None:
            data["result"] = result
        self.ledger.event(job.job_id, Role.TARGET, kind, data, epoch=self._epoch(job))

    def _commit_body(self, job: JobContext, report) -> dict:
        row = self._row(job)
        ctx = self._ctx(job.job_id)
        if row["state"] == "committed" and ctx.get("ack"):
            return dict(ctx["ack"])
        if row["state"] != "committing":
            raise InvalidState(f"the target is {row['state']}", reason="state", state=row["state"])
        steps = self._steps(job)
        direction, done_fwd, done_inv, results = self._journal_state(job.job_id)
        failure: dict | None = None
        if direction == "forward":
            try:
                for n, (name, fwd, _inv) in enumerate(steps):
                    if name in done_fwd:
                        continue
                    self._step_event(job, "intent", name, "forward")
                    result = fwd()
                    self._step_event(job, "done", name, "forward", result)
                    done_fwd.append(name)
                    results[name] = result
                    report(Progress(step="commit", items_done=n + 1, items_total=len(steps) + 1, message=name))
            except Fenced:
                raise
            except Exception as exc:
                failure = error_dict(exc)
                self.ledger.event(
                    job.job_id,
                    Role.TARGET,
                    "direction",
                    {"direction": "rollback", "cause": failure},
                    epoch=self._epoch(job),
                )
            else:
                return self._finalize(job)
        else:
            failure = next(
                (
                    e["data"].get("cause")
                    for e in reversed(self.ledger.events(job.job_id, Role.TARGET))
                    if e["kind"] == "direction"
                ),
                None,
            )
        return self._rollback(job, steps, done_fwd, done_inv, results, failure)

    def _rollback(self, job, steps, done_fwd, done_inv, results, failure) -> dict:
        inverse = {name: inv for name, _f, inv in steps}
        attention = []
        for name in reversed(done_fwd):
            if name in done_inv or name not in inverse:
                continue
            self._step_event(job, "intent", name, "rollback")
            undo_result = None
            try:
                undo_result = inverse[name](results.get(name))
            except Fenced:
                raise
            except Exception as exc:
                attention.append({"step": name, "error": error_dict(exc)})
                self.ledger.event(
                    job.job_id,
                    Role.TARGET,
                    "needs_attention",
                    {"step": name, "error": error_dict(exc)},
                    epoch=self._epoch(job),
                )
            self._step_event(job, "done", name, "rollback", undo_result if isinstance(undo_result, dict) else None)
        if not attention:
            self._cleanup(job, discard_session=True)
        last_error = dict(failure or {"code": "import_failed", "message": "commit failed"})
        if attention:
            last_error["needs_attention"] = attention
        self.ledger.job_update(job.job_id, Role.TARGET, last_error=last_error)
        self._go(job, "committing", "aborted", rolled_back=True)
        err = ImportFailed(
            str(last_error.get("message") or "commit failed; rolled back"),
            cause=last_error.get("code"),
            needs_attention=attention,
        )
        raise err

    def _finalize(self, job: JobContext) -> dict:
        """c7: workspace-map ``in`` rows, the ``committed`` row with the ``CommitAck`` (point of no return), the ctx
        record. The map rows are written first (idempotent upserts), so a crash right after ``committed`` loses
        none; ``model``/``offline_capable`` describe the route the import wrote (B ``plan_import``,
        ``offline_capable``, contract X04)."""
        ctx = self._ctx(job.job_id)
        prepared = from_wire(PreparedInfo, ctx["prepared"])
        final = self._final(job)
        items = {i.item_id: i for i in final.plan.items}
        sf, _tf = self._facts(job)
        peer_id = self._peer_key()
        try:
            txn = self._txn(job, restore=True)
            states = txn.map_states()
            dests = [(i, d, m) for i, d, m in txn.dests()] or list(prepared.dests)
        except HandoverError:
            states, dests = {}, list(prepared.dests)
        for item_id, dest, _mode in dests:
            item = items.get(item_id)
            if item is None:
                continue
            self.ledger.map_put(
                MapRow(
                    local_path=dest,
                    peer_id=peer_id,
                    remote_path=item.source_path,
                    direction="in",
                    repo_identity=item.git.root_commits[0] if item.git and item.git.root_commits else None,
                    state=states.get(item_id),
                    last_job_id=job.job_id,
                )
            )
        model_change = ctx.get("model_change", final.model_change)
        ack = CommitAck(
            session_id=prepared.tip_id,
            root_id=prepared.root_id,
            title=prepared.title,
            dests=tuple((i, d, DestMode(m)) for i, d, m in dests),
            path_map=tuple(from_wire(PathMapEntry, e) for e in ctx.get("path_map") or []),
            model=ctx.get("tip_model"),
            offline_capable=bool(
                self.ports.session_import.offline_capable(dict(ctx.get("tip_model_config") or {}), self.config)
            ),
            warnings=tuple(prepared.warnings),
            committed_at=self._clock(),
        )
        self._go(job, "committing", "committed", ack=ack)
        rec = self.ports.ctxrecords
        if rec.server_for(self.config, self.settings) is not None:  # ctx records only with ctx_checkpoint (X22)
            source = ctx.get("source_session") or {}
            rec.enqueue(
                self.ledger,
                "arrival",
                {
                    "job_id": job.job_id,
                    "source_root": source.get("root_id", ""),
                    "target_root": ack.root_id,
                    "source_session": source.get("tip_id", ""),
                    "target_session": ack.session_id,
                    "peer": self.peer_name or (job.peer.name if job.peer else None) or (sf and sf.instance_name),
                    "local_name": self.instance_name,
                    "title": ack.title,
                    "at": ack.committed_at,
                    "path_map": [to_wire(e) for e in ack.path_map],
                    "model_change": model_change,
                    "plugin_version": self.host.plugin_version,
                },
                redact=self.redact,
            )
        self._cleanup(job, discard_session=False)
        return to_wire(ack)

    def _cleanup(self, job: JobContext, *, discard_session: bool) -> None:
        ctx = self._ctx(job.job_id)
        if discard_session and ctx.get("prepared") is not None:
            try:
                self.ports.session_import.discard(self.db_factory(), from_wire(PreparedInfo, ctx["prepared"]))
            except HandoverError:
                pass
        try:
            if discard_session:
                self._txn(job, restore=True).discard()
        except HandoverError:
            pass
        staging = self.data_dir / "staging" / job.job_id
        shutil.rmtree(staging, ignore_errors=True)
        release = getattr(self.store, "release_job", None)
        if callable(release):
            try:
                release(job.job_id, keep=not discard_session)
            except HandoverError:
                pass

    # -- continuation, abort, status -------------------------------------------------------------------------------
    def continue_session(self, job: JobContext, prompt: str) -> ContinueResult:
        """Start the continuation after the handover (DESIGN §11.5, contract X21). The ``Spawner`` alone decides
        whether a turn may start (``verified`` = signature-checked peer or bundle; a remote request needs
        ``allow_remote_continue``; approvals off ⇒ refused, R22); the child runs in the cwd of the arrived session
        row (R11)."""
        row = self._row(job)
        ctx = self._ctx(job.job_id)
        if row["state"] != "committed" or not ctx.get("ack"):
            raise InvalidState("continue after the commit only", reason="state", state=row["state"])
        if ctx.get("continue") is not None:
            return from_wire(ContinueResult, ctx["continue"])
        ack = from_wire(CommitAck, ctx["ack"])
        requested_by = (self.peer_name or (job.peer.name if job.peer else None)) if self._remote_driven(job) else None
        res = self.continuation.spawn(
            ack.session_id,
            str(prompt)[:2000],
            job.job_id,
            requested_by=requested_by,
            verified=self.peer_verified,
            cwd=self._session_cwd(ack.session_id, ctx.get("cwd")),
        )
        self._remember(job, **{"continue": res})
        return res

    def _session_cwd(self, session_id: str, fallback: str | None) -> str | None:
        """The ``cwd`` of the arrived session's row (R11: from the row, never from ``ctx.session_cwd()``)."""
        try:
            row = self.db_factory().get_session(session_id) or {}
        except Exception:  # a row that cannot be read leaves the cwd the import wrote into it
            row = {}
        cwd = row.get("cwd") if isinstance(row, dict) else None
        return cwd if isinstance(cwd, str) and cwd else fallback

    def abort(self, job: JobContext, reason: str) -> None:
        """Abort and discard staged state (idempotent); ``committing``/``committed`` refuse [SK H5]."""
        row = self.ledger.job(job.job_id, Role.TARGET)
        if row is None:
            return
        state = row["state"]
        if state in ("committing", "committed"):
            raise InvalidState(f"the target is {state}", reason="state", state=state)
        if state in ("aborted", "expired"):
            return
        final_state = "expired" if reason == "expired" else "aborted"
        lock = self.ops.job_lock(job.job_id)
        if lock is not None and not lock.acquire():
            raise InvalidState("another process still works on this job", reason="busy_elsewhere", retryable=True)
        try:
            self._abort_locked(job, state, reason, final_state)
        finally:
            if lock is not None:
                lock.release()

    def _abort_locked(self, job: JobContext, state: str, reason: str, final_state: str) -> None:
        if state != "aborting":
            self._go(job, state, "aborting", abort_reason=reason)
        self._cleanup(job, discard_session=True)
        self._go(job, "aborting", final_state)

    def op_status(self, job: JobContext, op_id: str) -> OpStatus:
        return self.ops.status(job.job_id, op_id)

    def status(self, job: JobContext) -> ParticipantStatus:
        row = self._row(job)
        ops = {r["op"]: self.ops.status(job.job_id, r["op_id"]) for r in self.ledger.ops_for_job(job.job_id)}
        commit = ops.get("commit")
        if row["state"] == "committing" and commit is not None and commit.state.value == "failed":
            self.recover(job)  # a commit stuck half-way continues in its journaled direction (DESIGN §3.6)
            row = self._row(job)
            ops = {r["op"]: self.ops.status(job.job_id, r["op_id"]) for r in self.ledger.ops_for_job(job.job_id)}
        notes = ()
        ctx = self._ctx(job.job_id)
        if ctx.get("continue"):
            notes = ("continued",)
        return ParticipantStatus(
            state=row["state"],
            step="",
            epoch=int(row["max_peer_epoch"]),
            progress=None,
            ops=ops,
            last_error=row["last_error"],
            notes=notes,
        )

    def blocks(self):
        return self.store

    def blocks_for(self, job: JobContext) -> QuotaSink:
        """The quota-enforcing sink of one job (server route #25 and a local conductor's block mover)."""
        return QuotaSink(self.store, self.ledger, job.job_id)
