"""The in-process source participant (layer L6, stdlib only; INTERFACES §4.5, DESIGN §6–§9, §7).

``LocalSource`` implements ``SourceAPI`` on the machine where the session lives — called directly by a local
conductor (push, export) or through the registry by a remote one (pull, offer claim). It composes the session
(B), planning (D) and workspace (E) subsystems in the order of DESIGN §3.2/§7:

* ``facts`` — this host's facts, with the tool names the session's transcript uses (``tools_seen``, W084).
* ``start_plan`` — collector over the lineage rows of ``export_session_lineage(include_inactive=True)`` (W076) +
  refinement with the rows and ``redact`` (W077) + agent suggestions; ``build_plan`` with ``remote_driven`` (a
  remote conductor's hints never add, R18) and the estimate of the session's active context
  (``plan.active_context_tokens`` of the tip's live rows → ``ResourcePlan.context_tokens``, R32) — **no lock**
  [BX M9]; the busy probe is read-only.
* ``lock`` — busy probe, turn lease + compression lock (``lock.acquire``; a route call blocks at most
  ``LOCK_WAIT_S`` seconds, W060), then ``describe`` again under the held lease and the revision compared with the
  plan's (§7.2 step 6, W037).
* ``start_export`` / ``start_pack`` — capsule with revision bracket; pack of the confirmed final plan, every item
  built by cluster E's one entry ``materialize.pack_plan_item`` (contract X12, X17). A remote-driven source
  (``remote_driven``) restricts the remote final plan to what it releases itself (``plan.restrict_to_releasable``
  with the remote hints and ``redact``, R18) and logs "pulled by <peer>" (SEC 5.8). Every object of the capsule,
  pack and delta closures is registered with the job (``job_objects``): what a pull peer may read is exactly that
  (contract X13).
* ``start_prepare`` — revision and work-tree re-check (``delta`` / ``stale_session`` / ``stale_worktree``), then the
  primary lock while the leases are held (``lock.prepare``, p1–p7).
* ``start_retire`` — only after the target's ACK; ``lock.retire`` releases the leases only after the ledger, the
  veto rows and the workspace-map ``out`` rows are durable (R1, r1–r5).
* ``start_release`` — undo everything of lock/prepare (abort path, I2).

Its own working data (session info, holder, plan, capsule, pack states) is journaled on the job's ``source``
row, so another process can continue (janitor, DESIGN §3.6). The B/D/E functions are reached through
``ports`` (default: the real modules, imported lazily; unit tests may pass stand-ins with the same signatures).
"""

from __future__ import annotations

import dataclasses
import os
import time
from collections.abc import Callable
from datetime import datetime
from pathlib import Path
from types import SimpleNamespace

from . import CAPABILITIES_V1, PROTOCOL, WIRE_MAJOR, WIRE_MINOR
from . import settings as settings_mod
from .contracts import JobContext
from .errors import (
    Cancelled,
    HandoverError,
    InvalidState,
    LeaseLost,
    NotFound,
    SessionBusy,
    VerifyFailed,
    WaitTimeout,
)
from .jsonutil import canonical_bytes, from_wire, sha256_hex, to_wire
from .ledger import STALE_OWNER_S, Ledger, private_dir, process_owner
from .model import (
    CapsuleInfo,
    CommitAck,
    DestMode,
    EnvFacts,
    FinalPlan,
    GitState,
    Hello,
    HermesPaths,
    HostFacts,
    Include,
    ItemKind,
    Limits,
    MapRow,
    Mode,
    OpStatus,
    PackInfo,
    ParticipantStatus,
    PreparedInfo,
    PrepareOutcome,
    PrepareResult,
    Progress,
    Refinement,
    ResourcePlan,
    Role,
    SessionInfo,
    SessionRef,
)
from .ops import OpRunner, object_closure
from .policy import PolicyConfig

LOCK_WAIT_S = 20
"""Longest a ``lock`` call waits for a running turn's lease (route ``source/lock`` stays below the request timeout,
I7, W060); with ``--wait`` the conductor repeats it until its own deadline."""


def default_source_ports() -> SimpleNamespace:
    """The real B/D/E/F modules (layer 2 wiring); imported lazily so the module loads without them."""
    from . import (
        collector,
        ctxrecords,
        envfacts,
        gitops,
        gitpack,
        lock,
        materialize,
        plan,
        refine,
        session_export,
    )

    return SimpleNamespace(
        collector=collector,
        ctxrecords=ctxrecords,
        envfacts=envfacts,
        gitops=gitops,
        gitpack=gitpack,
        lock=lock,
        materialize=materialize,
        plan=plan,
        refine=refine,
        session_export=session_export,
    )


def _settings_of(config: dict) -> dict:
    """The plugin settings of ``config`` through K0's one normalizer (contract V22): every schema key, YAML words and
    ``None`` read by the same rules as everywhere else."""
    return settings_mod.from_config(config)


def _digest(*parts: object) -> str:
    return sha256_hex(canonical_bytes([p if not dataclasses.is_dataclass(p) else to_wire(p) for p in parts]))


class LocalSource:
    """``SourceAPI`` over this machine's session store and working directories (see module docstring)."""

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
        calling_session: str | None,
        session_key: str | None,
        side_question: Callable[[str], str | None] | None,
        llm: Callable | None,
        read_secret: Callable,
        redact: Callable[[str], str],
        state_db: Path,
        remote_driven: bool,
        policy: PolicyConfig,
        ports: SimpleNamespace | None = None,
        clock: Callable[[], float] | None = None,
        data_dir: Path | None = None,
        instance_name: str = "",
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
        self.calling_session = calling_session
        self.session_key = session_key
        self.side_question = side_question
        self.llm = llm
        self.read_secret = read_secret
        self.redact = redact
        self.state_db = Path(state_db)
        self.remote_driven = remote_driven
        self.policy = policy
        self._ports = ports
        self._clock = clock or time.time
        self.data_dir = Path(data_dir) if data_dir else Path(hermes.home) / "plugin-data" / "ctx_handover"
        self.instance_name = instance_name or host.profile
        self.peer_name = peer_name

    # -- plumbing ------------------------------------------------------------------------------------------------
    @property
    def ports(self) -> SimpleNamespace:
        if self._ports is None:
            self._ports = default_source_ports()
        return self._ports

    def _ensure_job(self, job: JobContext) -> None:
        if self.ledger.job(job.job_id, Role.SOURCE) is None:
            peer_id = job.peer.peer_id if job.peer is not None else None
            self.ledger.create_job(job.job_id, Role.SOURCE, job.mode, peer_id, {"role": "source", "local": True})

    def _epoch(self, job: JobContext) -> int:
        self._ensure_job(job)
        epoch = self.ledger.claim_owner(job.job_id, Role.SOURCE, process_owner(), STALE_OWNER_S)
        if epoch is None:
            raise InvalidState("another process is working on this job", reason="busy_elsewhere", retryable=True)
        return epoch

    def _ctx(self, job_id: str) -> dict:
        out: dict = {}
        for ev in self.ledger.events(job_id, Role.SOURCE):
            data = ev["data"] or {}
            if isinstance(data.get("ctx"), dict):
                out.update(data["ctx"])
        return out

    def _remember(self, job: JobContext, **values: object) -> None:
        enc = {k: (to_wire(v) if dataclasses.is_dataclass(v) else v) for k, v in values.items()}
        self.ledger.event(job.job_id, Role.SOURCE, "ctx", {"ctx": enc}, epoch=self._epoch(job))

    def _sync_state(self, job: JobContext) -> None:
        src = self.ledger.source_state(job.job_id)
        if src is not None:
            row = self.ledger.job(job.job_id, Role.SOURCE)
            if row is not None and row["state"] != src["state"]:
                self.ledger.transition(job.job_id, Role.SOURCE, "*", src["state"], {}, epoch=self._epoch(job))

    def _info(self, job: JobContext) -> SessionInfo:
        raw = self._ctx(job.job_id).get("info")
        if raw is None:
            raise InvalidState("the source of this job is not locked", reason="not_locked")
        return from_wire(SessionInfo, raw)

    def _start(
        self, job: JobContext, op: str, digest: str, fn, *, restart_failed: bool = False, exclusive: bool = False
    ) -> OpStatus:
        self._epoch(job)
        return self.ops.start(
            job.job_id, op, _digest(op, digest), fn, restart_failed=restart_failed, exclusive=exclusive
        )

    def _cfg(self, job: JobContext) -> PolicyConfig:
        return self.policy

    @staticmethod
    def _rows(db, info: SessionInfo) -> list[dict]:
        """Every stored row of the lineage in storage order with its flags — the form the collector, the refinement
        and the tool-name scan are built for (``export_session_lineage(tip, include_inactive=True)``, W076)."""
        doc = db.export_session_lineage(info.tip_id, include_inactive=True) or {}
        return [r for r in doc.get("messages") or () if isinstance(r, dict)]

    def _session_ref(self, job: JobContext) -> SessionRef | None:
        """The session a job is about: the participant row's ``session_ref`` (remote conductor, route #7) or, for a
        local conductor in this process, the conductor row's (same ledger, same job id)."""
        for role in (Role.SOURCE, Role.CONDUCTOR):
            row = self.ledger.job(job.job_id, role)
            raw = ((row or {}).get("params") or {}).get("session_ref")
            if isinstance(raw, dict) and (raw.get("id") or raw.get("latest") or raw.get("offer_id")):
                return from_wire(SessionRef, raw)
        return None

    def _describe(self, db, job: JobContext, ref: SessionRef) -> SessionInfo:
        """``session_export.describe`` of the job's session; a claimed offer (``take --offer``, L3-F12) resolves
        through ``_offer_session`` (B's offer branch, ``reports/L2/g3-b-api.md``)."""
        if not ref.offer_id:
            return self.ports.session_export.describe(db, ref, calling_session=self.calling_session)
        return self.ports.session_export.describe(
            db, ref, calling_session=self.calling_session, offer_session=self._offer_session(job)
        )

    def _offer_session(self, job: JobContext) -> Callable[[str], str | None]:
        """The session an offer names — for the peer it was made for only, claimed for this job (route #6 rules:
        idempotent for the same job; another peer's, an expired or an otherwise claimed offer is ``not_found``). The
        claim happens because that peer asked for the offer (``take --offer``), never on its own (O6)."""
        peer_id = job.peer.peer_id if job.peer is not None else None

        def lookup(offer_id: str) -> str | None:
            if peer_id is None:
                raise NotFound(f"offer {offer_id} not found")
            claimed = self.ledger.offer_claim(str(offer_id), peer_id, job.job_id)
            return str(claimed.get("session_id") or "") or None

        return lookup

    def _tools_seen(self, job: JobContext) -> set[str]:
        """Tool names the transcript runs (``envfacts.tools_seen_from_rows``, W084); empty when the session is
        unknown here (the facts then carry no transcript tools)."""
        ref = self._session_ref(job)
        if ref is None:
            return set()
        db = self.db_factory()
        try:
            info = self._describe(db, job, ref)
        except HandoverError:
            return set()
        return set(self.ports.envfacts.tools_seen_from_rows(self._rows(db, info)))

    # -- SourceAPI -----------------------------------------------------------------------------------------------
    def hello(self) -> Hello:
        caps = set(CAPABILITIES_V1) - {"operator.v1"}
        if self.llm is None:
            caps.discard("refine.llm")
        if self.side_question is None:
            caps.discard("refine.side_question")
        return Hello(
            protocol=PROTOCOL,
            majors=(WIRE_MAJOR,),
            minor=WIRE_MINOR,
            instance_id=self.ledger.instance_id(),
            listener_pubkey="",
            plugin_version=self.host.plugin_version,
            hermes_version=self.host.hermes_version,
            name=self.instance_name,
            capabilities=frozenset(caps),
            limits=Limits(),
        )

    def list_sessions(self, limit: int, query: str | None, include_archived: bool) -> list[dict]:
        """The pull picker (route #4): newest sessions of this profile with at least one message."""
        db = self.db_factory()
        rows = db.list_sessions_rich(
            limit=int(limit),
            order_by_last_active=True,
            include_archived=bool(include_archived),
            search_query=query or None,
            min_message_count=1,
        )
        out = []
        for r in rows:
            sid = r.get("id")
            handover = self.ledger.source_by_session(sid) if sid else None
            out.append(
                {
                    "id": sid,
                    "root_id": r.get("root_id") or sid,
                    "title": r.get("title"),
                    "started_at": r.get("started_at"),
                    "last_active_at": r.get("last_active") or r.get("last_active_at"),
                    "message_count": r.get("message_count"),
                    "cwd": r.get("cwd"),
                    "model": r.get("model"),
                    "busy": False,
                    "handover_state": handover["state"] if handover else None,
                }
            )
        return out

    def facts(self, job: JobContext) -> EnvFacts:
        self._ensure_job(job)
        return self.ports.envfacts.probe(
            instance_name=self.instance_name,
            options=job.options,
            host=self.host,
            config=self.config,
            tools_seen=sorted(self._tools_seen(job)),
            capabilities=self.hello().capabilities,
            cfg=self.policy,  # the facts' write-root check with this host's policy (gp-d-api §1c)
        )

    def _stored_refinement(self, ref: SessionRef) -> Refinement | None:
        """The refinement an offer stored when it was created in the session's own gateway (DESIGN §8.3, R9)."""
        if not ref.offer_id:
            return None
        offer = self.ledger.offer_get(ref.offer_id)
        if offer is None or not isinstance(offer.get("refinement"), dict):
            return None
        return from_wire(Refinement, offer["refinement"])

    def _suggest_dest(self, job: JobContext) -> Callable[[str], str | None]:
        """Where an item came from on the target (this host's ``in`` row for the job's peer, §9.7 rule 3)."""
        peer_id = job.peer.peer_id if job.peer is not None else None

        def suggest(source_path: str) -> str | None:
            if peer_id is None:
                return None
            for row in self.ledger.map_by_local(source_path):
                if row.peer_id == peer_id and row.direction == "in":
                    return row.remote_path
            return None

        return suggest

    def start_plan(self, job: JobContext, target_facts: EnvFacts, session_ref: SessionRef) -> OpStatus:
        def fn(report) -> dict:
            p = self.ports
            db = self.db_factory()
            info = self._describe(db, job, session_ref)
            rows = self._rows(db, info)
            session_row = db.get_session(info.tip_id) or {}
            cfg = self._cfg(job)
            probe = p.gitops.GitProbe(self.git)
            candidates = p.collector.collect(rows, session_row, self.hermes, cfg, probe, source_os=os.name)
            approx = sum(len(str(r.get("content") or "")) for r in rows) // 4
            refinement, refine_report = p.refine.refine(
                candidates,
                target_facts,
                approx_tokens=approx,
                options=job.options,
                side_question=self.side_question,
                llm=self.llm,
                stored=self._stored_refinement(session_ref),
                rows=rows,
                redact=self.redact,
            )
            busy = p.lock.probe_busy(db, self.state_db, info, Path(self.hermes.home), self.session_key)
            suggestions = self.ledger.suggestions(info.tip_id, self._clock())
            if info.root_id != info.tip_id:
                suggestions = suggestions + self.ledger.suggestions(info.root_id, self._clock())
            plan = p.plan.build_plan(
                job.job_id,
                tuple(info.revision),
                candidates,
                refinement,
                refine_report,
                job.options.hints,
                cfg,
                lambda root: probe.info(root, cfg),
                busy,
                suggestions,
                job.options,
                approx,
                redact=self.redact,
                remote_driven=self.remote_driven,
                suggest_dest=self._suggest_dest(job),
                context_tokens=p.plan.active_context_tokens(rows, info.tip_id),
            )
            self._remember(job, plan=plan, planned_info=info)
            return to_wire(plan)

        digest = _digest(job.job_id, session_ref, target_facts, job.options.refine, job.options.hints)
        return self._start(job, "plan", digest, fn)

    def lock(self, job: JobContext, session_ref: SessionRef, plan_revision: tuple) -> SessionInfo:
        """Busy probe, leases, revision check (DESIGN §7.2). Idempotent per job (re-acquires a lost lease)."""
        p = self.ports
        self._epoch(job)
        db = self.db_factory()
        info = self._describe(db, job, session_ref)
        busy = p.lock.probe_busy(db, self.state_db, info, Path(self.hermes.home), self.session_key)
        if busy.busy and not job.options.force:
            raise SessionBusy(
                "A turn is running in this session. Wait for it, or use --wait to hand over as soon as it finishes.",
                reasons=list(busy.reasons),
            )
        ttl, _refresh = p.lock.lease_timing(job.options)
        # without --wait a running turn answers at once (§7.2 step 3); with it this call waits at most LOCK_WAIT_S
        # and the conductor repeats until its deadline (W060)
        wait_s = min(LOCK_WAIT_S, max(0, int(job.options.wait_timeout_s))) if job.options.wait else 0
        try:
            holder = p.lock.acquire(
                db, self.ledger, info, job.job_id, ttl_s=ttl, wait_s=wait_s, cancelled=job.cancelled
            )
        except WaitTimeout as exc:
            raise SessionBusy(
                "A turn is still running in this session; waiting continues.", reason="turn", waited_s=wait_s
            ) from exc
        # DESIGN §7.2 step 6: the revision under the held lease decides (a turn may have ended in between)
        info = self._describe(db, job, session_ref)
        info = dataclasses.replace(info, revision_changed=tuple(info.revision) != tuple(plan_revision))
        self._remember(job, info=info, holder=holder, busy_warnings=list(busy.warnings))
        self._sync_state(job)
        return info

    def heartbeat(self, job: JobContext) -> None:
        ctx = self._ctx(job.job_id)
        src = self.ledger.source_state(job.job_id)
        if src is None or src["state"] not in ("locking", "locked", "preparing", "prepared", "retiring"):
            if src is not None and src["state"] in ("released", "reclaimed", "suspended"):
                # the state travels in the details: route #9 answers it as ``{state}`` (``source_reclaimed`` for a
                # reclaim, contract V17) instead of an anonymous lost lease
                raise LeaseLost(f"the source is {src['state']}", state=src["state"])
            return
        info = from_wire(SessionInfo, ctx["info"])
        ttl, _refresh = self.ports.lock.lease_timing(job.options)
        refreshed = self.ports.lock.refresh(self.db_factory(), info, ctx.get("holder"), ttl_s=ttl)
        if not refreshed and src["state"] in ("locking", "locked", "preparing"):
            raise LeaseLost("the session's turn lease was lost")

    def suspend(self, job: JobContext) -> None:
        """Idle remote conductor (DESIGN §7.3): give up the leases, keep the export/pack caches."""
        src = self.ledger.source_state(job.job_id)
        if src is None or src["state"] not in ("locking", "locked"):
            return
        info = self._info(job)
        self.ports.lock.suspend(self.db_factory(), self.ledger, info, src.get("holder"), job.job_id)
        self._sync_state(job)

    def start_export(self, job: JobContext) -> OpStatus:
        info = self._info(job)
        db = self.db_factory()
        revision = tuple(db.conversation_revision(info.tip_id, include_ancestors=True))

        def fn(report) -> dict:
            source_meta = {
                "instance_id": self.ledger.instance_id(),
                "profile": self.host.profile,
                "hermes_home": self.hermes.home,
                "document_cache": self.hermes.document_cache,
                "hermes_version": self.host.hermes_version,
            }
            capsule = self.ports.session_export.export_capsule(
                self.db_factory(),
                info,
                job_id=job.job_id,
                store=self.store,
                source_meta=source_meta,
                hermes=self.hermes,
                cancelled=job.cancelled,
            )
            self._register_closure(job, capsule.capsule_id)
            self._remember(job, capsule=capsule, exported=capsule)
            return to_wire(capsule)

        return self._start(job, "export", _digest(revision), fn)

    def _register_closure(self, job: JobContext, root_id: str) -> None:
        """Every object of a document's closure belongs to the job: what a pull peer may read is exactly that
        (server routes #19/#20, R18) and the block GC keeps it while the job lives (DESIGN §5.1)."""
        ids = object_closure(self.store, root_id, max_doc_bytes=64 * 1024 * 1024, cache={})
        self.ledger.job_objects_add(job.job_id, ids)

    def _worktrees_unchanged(self, job: JobContext, final: FinalPlan, states: dict) -> bool:
        """DESIGN §9.3 step 8: ``gitpack.recheck`` with the recorded exclusions for every packed git item. A packed
        git item without a recorded state counts as changed (re-pack), never as unchanged: only an explicit equal
        re-check lets the primary lock follow."""
        tmp = private_dir(self.data_dir / "staging" / job.job_id / "recheck")
        skipped = {d.item_id for d in final.decisions if d.mode is DestMode.SKIP}
        for item in final.plan.items:
            if item.include is not Include.YES or item.kind is not ItemKind.GIT_REPO or item.item_id in skipped:
                continue
            recorded = states.get(item.item_id)
            if not isinstance(recorded, dict):
                return False
            same, _current = self.ports.gitpack.recheck(self.git, item.source_path, from_wire(GitState, recorded), tmp)
            if not same:
                return False
        return True

    def _git_states(self, final: FinalPlan) -> dict:
        tmp = self.data_dir / "staging" / final.plan.job_id / "recheck"
        states = {}
        for item in final.plan.items:
            if item.include is Include.YES and item.kind is ItemKind.GIT_REPO:
                private_dir(tmp)
                st = self.ports.gitops.worktree_state(self.git, item.source_path, (), tmp)
                states[item.item_id] = [st.head, st.index_tree, st.worktree_tree]
        return states

    def start_pack(self, job: JobContext, final_plan: FinalPlan) -> OpStatus:
        p = self.ports
        own_raw = self._ctx(job.job_id).get("plan")
        if self.remote_driven:
            if own_raw is None:
                raise InvalidState("plan before pack", reason="no_plan")
            final_plan = p.plan.restrict_to_releasable(
                final_plan,
                from_wire(ResourcePlan, own_raw),
                self._cfg(job),
                remote_hints=job.options.hints,
                redact=self.redact,
            )
            peer_id = job.peer.peer_id if job.peer is not None else "unknown"
            items = [i.item_id for i in final_plan.plan.items if i.include is Include.YES]
            self.ledger.pull_log_put(job.job_id, peer_id, items)
        doc = to_wire(final_plan)
        doc.pop("plan_id", None)
        plan_id = p.plan.plan_id(doc)
        final_plan = dataclasses.replace(final_plan, plan_id=plan_id)
        states = self._git_states(final_plan)

        def fn(report) -> dict:
            return self._pack(job, final_plan, report)

        return self._start(job, "pack", _digest(plan_id, states), fn, exclusive=True)

    def _pack(self, job: JobContext, final: FinalPlan, report) -> dict:
        """Build the pack document of DESIGN §9.1. Every item comes from cluster E's one entry
        ``materialize.pack_plan_item`` (contract X12: git with LFS, trees and extra directories as ``file_tree``,
        single files with the policy re-checked, ``hermes_file`` with ``PlanItem.hermes_kind``, X17); its ``state``
        (``materialize.map_state_of`` form) is what the ``out`` row records for the return trip (X09, R4)."""
        p = self.ports
        cfg = self._cfg(job)
        decisions = {d.item_id: d for d in final.decisions}
        workdir = self.data_dir / "staging" / job.job_id / "git"
        todo = [
            item
            for item in final.plan.items
            if item.include is Include.YES
            and not (item.item_id in decisions and decisions[item.item_id].mode is DestMode.SKIP)
        ]
        items, states = [], {}
        for item in todo:
            if job.cancelled():
                raise Cancelled("cancelled")
            decision = decisions.get(item.item_id)
            packed = p.materialize.pack_plan_item(
                item,
                git=self.git,
                store=self.store,
                cfg=cfg,
                workdir=workdir / item.item_id,
                created_at=self._clock(),
                basis_candidates=tuple(decision.git_have) if decision else (),
                force=job.options.force,
                extra_ignore=tuple(job.options.extra_ignore),
                hermes_home=self.hermes.home,
                cancelled=job.cancelled,
            )
            items.append(dict(packed.doc))
            if packed.state is not None:
                states[item.item_id] = packed.state
            report(Progress(step="packing", items_done=len(items), items_total=len(todo)))
        doc = {"kind": "ctxh.pack", "v": 1, "job_id": job.job_id, "final_plan_id": final.plan_id, "items": items}
        m = self.store.put_bytes(canonical_bytes(doc), "application/json")
        self._register_closure(job, m.object_id)
        pack = PackInfo(pack_id=m.object_id, size=m.size)
        self._remember(
            job,
            pack=pack,
            pack_states=states,
            final_plan=final,
            pack_time=self._clock(),
            git_states=self._git_states(final),
        )
        return to_wire(pack)

    def start_prepare(self, job: JobContext, prepared: PreparedInfo) -> OpStatus:
        ctx = self._ctx(job.job_id)
        capsule_id = (ctx.get("exported") or {}).get("capsule_id")
        pack_id = (ctx.get("pack") or {}).get("pack_id")
        info = self._info(job)
        revision = tuple(self.db_factory().conversation_revision(info.tip_id, include_ancestors=True))

        def fn(report) -> dict:
            return to_wire(self._prepare(job, prepared))

        return self._start(job, "prepare", _digest(prepared, capsule_id, pack_id, revision), fn, exclusive=True)

    def _prepare(self, job: JobContext, prepared: PreparedInfo) -> PrepareResult:
        p = self.ports
        ctx = self._ctx(job.job_id)
        src = self.ledger.source_state(job.job_id)
        if src is not None and src["state"] == "prepared":
            return PrepareResult(outcome=PrepareOutcome.OK)
        if src is None or src["state"] not in ("locked", "preparing"):
            raise InvalidState(
                f"the source is {src['state'] if src else 'not locked'}",
                reason="state",
                state=src["state"] if src else None,
            )
        info = from_wire(SessionInfo, ctx["info"])
        exported = from_wire(CapsuleInfo, ctx["exported"])
        db = self.db_factory()
        outcome, new_rows = p.session_export.check_revision(db, info, exported)
        outcome = PrepareOutcome(outcome)
        if outcome is PrepareOutcome.DELTA:
            delta = p.session_export.export_delta(db, info, exported, new_rows, store=self.store)
            self._register_closure(job, delta.delta_id)
            baseline = dataclasses.replace(
                exported,
                h_all=delta.h_all,
                h_rows=delta.h_rows,
                h_model=delta.h_model,
                revision=delta.revision,
                rows=exported.rows + delta.rows,
            )
            self._remember(job, exported=baseline, deltas=int(ctx.get("deltas") or 0) + 1)
            return PrepareResult(outcome=PrepareOutcome.DELTA, delta=delta)
        if outcome is PrepareOutcome.STALE_SESSION:
            return PrepareResult(outcome=PrepareOutcome.STALE_SESSION, detail="session changed")
        final = from_wire(FinalPlan, ctx["final_plan"]) if ctx.get("final_plan") else None
        # no pack recorded here (another process lost it) is not "unchanged": the conductor packs again
        if final is None or not self._worktrees_unchanged(job, final, ctx.get("pack_states") or {}):
            return PrepareResult(outcome=PrepareOutcome.STALE_WORKTREE, detail="work tree changed since packing")
        if (prepared.h_all, prepared.h_rows) != (exported.h_all, exported.h_rows):
            raise VerifyFailed("the target's import does not match the source transcript", which="h_all")
        # an export has no receiving peer yet (``--for`` only picks the facts): "" makes the title say "exported to
        # bundle" instead of "handed over to <peer>" (N55)
        peer = "" if job.mode is Mode.EXPORT else self.peer_name or (job.peer.name if job.peer is not None else "")
        p.lock.prepare(db, self.ledger, info, job.job_id, peer, datetime.fromtimestamp(self._clock()))
        self._sync_state(job)
        return PrepareResult(outcome=PrepareOutcome.OK)

    def _out_rows(self, job: JobContext, ack: CommitAck) -> list[MapRow]:
        """r3: workspace-map ``out`` rows incl. the outgoing git state / tree fingerprint per item (R4, §9.8)."""
        ctx = self._ctx(job.job_id)
        final = from_wire(FinalPlan, ctx["final_plan"]) if ctx.get("final_plan") else None
        states = ctx.get("pack_states") or {}
        peer_id = job.peer.peer_id if job.peer is not None else (ack.root_id and "bundle") or "local"
        dests = {item_id: dest for item_id, dest, _mode in ack.dests}
        rows = []
        for item in final.plan.items if final else ():
            if item.item_id in dests and item.kind in (ItemKind.GIT_REPO, ItemKind.FILE_TREE, ItemKind.EXTRA_DIR):
                rows.append(
                    MapRow(
                        local_path=item.source_path,
                        peer_id=peer_id,
                        remote_path=dests[item.item_id],
                        direction="out",
                        repo_identity=(item.git.root_commits[0] if item.git and item.git.root_commits else None),
                        state=states.get(item.item_id),
                        last_job_id=job.job_id,
                    )
                )
        return rows

    def start_retire(self, job: JobContext, ack: CommitAck) -> OpStatus:
        def fn(report) -> dict:
            src = self.ledger.source_state(job.job_id)
            if src is None:
                raise NotFound("no source handover for this job")
            if src["state"] in ("released", "reclaimed"):
                raise InvalidState(f"the source is {src['state']}", reason="state", state=src["state"])
            info = self._info(job)
            holder = self._ctx(job.job_id).get("holder")
            self.ports.lock.retire(
                self.db_factory(), self.ledger, info, holder, job.job_id, ack, self._out_rows(job, ack)
            )
            self._sync_state(job)
            self._record("departure", job, ack)
            return {"state": self.ledger.source_state(job.job_id)["state"]}

        return self._start(job, "retire", _digest(ack), fn, restart_failed=True, exclusive=True)

    def start_release(self, job: JobContext, reason: str) -> OpStatus:
        def fn(report) -> dict:
            src = self.ledger.source_state(job.job_id)
            if src is None:
                self._epoch(job)
                row = self.ledger.job(job.job_id, Role.SOURCE)
                if row is not None and row["state"] not in ("released",):
                    self.ledger.transition(
                        job.job_id, Role.SOURCE, "*", "released", {"reason": reason}, epoch=self._epoch(job)
                    )
                return {"state": "released"}
            if src["state"] in ("retired", "exported"):
                raise InvalidState(f"the source is {src['state']}", reason="state", state=src["state"])
            if src["state"] not in ("released", "reclaimed"):
                info = self._info(job)
                self.ports.lock.release(self.db_factory(), self.ledger, info, src.get("holder"), job.job_id)
            self._sync_state(job)
            return {"state": self.ledger.source_state(job.job_id)["state"]}

        self._ensure_job(job)
        return self._start(job, "release", "release", fn, restart_failed=True, exclusive=True)

    def _record(self, kind: str, job: JobContext, ack: CommitAck) -> None:
        """The departure record for ctx (DESIGN §13) — only when ctx_checkpoint is the memory provider and
        ``ctx_persist`` is not off (``ctxrecords.server_for``); the outbox does the rest (X22)."""
        rec = self.ports.ctxrecords
        try:
            if rec.server_for(self.config, _settings_of(self.config)) is None:
                return
            info = self._info(job)
            final_raw = self._ctx(job.job_id).get("final_plan") or {}
            rec.enqueue(
                self.ledger,
                kind,
                {
                    "job_id": job.job_id,
                    "source_root": info.root_id,
                    "target_root": ack.root_id,
                    "source_session": info.tip_id,
                    "target_session": ack.session_id,
                    "peer": self.peer_name or (job.peer.name if job.peer else None),
                    "local_name": self.instance_name,
                    "title": info.title,
                    "at": ack.committed_at,
                    "path_map": [to_wire(e) for e in ack.path_map],
                    "model_change": (final_raw or {}).get("model_change"),
                    "plugin_version": self.host.plugin_version,
                },
                redact=self.redact,
            )
        except HandoverError:
            pass

    def op_status(self, job: JobContext, op_id: str) -> OpStatus:
        return self.ops.status(job.job_id, op_id)

    def status(self, job: JobContext) -> ParticipantStatus:
        row = self.ledger.job(job.job_id, Role.SOURCE)
        if row is None:
            raise NotFound(f"job {job.job_id} not found")
        src = self.ledger.source_state(job.job_id)
        ops = {}
        for r in self.ledger.ops_for_job(job.job_id):
            ops[r["op"]] = self.ops.status(job.job_id, r["op_id"])
        notes = ("source_reclaimed",) if src and src["state"] == "reclaimed" else ()
        return ParticipantStatus(
            state=src["state"] if src else row["state"],
            step="",
            epoch=int(row["max_peer_epoch"]),
            progress=None,
            ops=ops,
            last_error=row["last_error"],
            notes=notes,
        )

    def blocks(self):
        return self.store
