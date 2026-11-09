"""Stand-ins for the cluster B/C/D/E/F functions ``LocalSource``/``LocalTarget`` call in the orchestration tests.

They follow the signatures the real modules have now (contract.md: ``materialize.pack_plan_item`` X12,
``envfacts.tools_seen_from_rows`` W084, ``lock.acquire(wait_s=…)`` W060, K-1 object ids) and run on real substrates
where it matters for the orchestration: the real ``SessionDB`` (hidden flag, revision, messages), the real file
system (partial directories renamed into place) and a real ``Ledger``. Production uses the real modules (the
``ports`` default); the tests against them are ``test_l2_source_*.py`` and ``tests/integration``.
"""

from __future__ import annotations

import io
import json
import os
import shutil
from dataclasses import dataclass, replace
from pathlib import Path
from types import SimpleNamespace
from typing import ClassVar

from ctx_handover.engine import note as real_note
from ctx_handover.engine import plan as planning
from ctx_handover.engine import session_import as real_session_import
from ctx_handover.engine.errors import NotFound, PolicyDenied
from ctx_handover.engine.jsonutil import canonical_bytes, doc_id, object_id, object_manifest, sha256_hex, to_wire
from ctx_handover.engine.model import (
    CapsuleInfo,
    ContinueResult,
    DeltaInfo,
    DestMode,
    Include,
    ItemKind,
    ObjectManifest,
    PreparedInfo,
    PrepareOutcome,
    RefineReport,
    ResourcePlan,
    SessionInfo,
)
from ctx_handover.tests.fakes.apis_fake import make_facts

# ---------------------------------------------------------------------------------------------------------
# C: block store
# ---------------------------------------------------------------------------------------------------------


class MemStore:
    """In-memory BlockStore (BlockSource + BlockSink + object helpers of ``blocks.BlockStore``); objects follow the
    one id rule (contract K-1, ``jsonutil.object_manifest``)."""

    def __init__(self) -> None:
        self.objects: dict[str, ObjectManifest] = {}
        self.data: dict[str, bytes] = {}
        self.released: list[tuple[str, bool]] = []

    def put_bytes(self, data: bytes, media: str) -> ObjectManifest:
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

    def put_stream(self, fileobj, *, media: str, cancelled=lambda: False) -> ObjectManifest:
        return self.put_bytes(fileobj.read(), media)

    def open_object(self, object_id: str):
        m = self.objects.get(object_id)
        if m is None:
            raise NotFound(object_id)
        return io.BytesIO(b"".join(self.data[b] for b in m.blocks))

    def doc(self, object_id: str) -> dict:
        with self.open_object(object_id) as fh:
            return json.loads(fh.read())

    def manifests(self, ids):
        return [self.objects[i] for i in ids]

    def read_blocks(self, ids):
        return iter([(i, self.data[i]) for i in ids if i in self.data])

    def put_manifests(self, ms):
        for m in ms:
            self.objects[m.object_id] = m
        return len(ms)

    def missing(self, ids):
        return [i for i in ids if i not in self.data]

    def write_blocks(self, frames):
        n = 0
        for bid, raw in frames:
            self.data[bid] = raw
            n += 1
        return n, []

    def release_job(self, job_id: str, *, keep: bool) -> None:
        self.released.append((job_id, keep))


# ---------------------------------------------------------------------------------------------------------
# B: session export / import / lock (on the real SessionDB)
# ---------------------------------------------------------------------------------------------------------


def _rows_hash(rows) -> str:
    return sha256_hex(canonical_bytes([[r.get("role"), r.get("content")] for r in rows]))


def describe(db, ref, *, calling_session=None) -> SessionInfo:
    sid = ref.id or calling_session
    tip = db.resolve_resume_session_id(sid)
    lineage = tuple(db.get_compression_lineage(tip))
    row = db.get_session(tip) or {}
    return SessionInfo(
        tip_id=tip,
        root_id=lineage[0],
        lineage_ids=lineage,
        title=row.get("title"),
        model=row.get("model"),
        provider=row.get("billing_provider"),
        cwd=row.get("cwd"),
        git_repo_root=row.get("git_repo_root"),
        git_branch=row.get("git_branch"),
        active_messages=int(row.get("message_count") or 0),
        revision=tuple(db.conversation_revision(tip, include_ancestors=True)),
    )


def _rows(db, tip):
    return db.get_messages(tip, include_ancestors=True)


def export_capsule(db, info, *, job_id, store, source_meta, hermes, cancelled=lambda: False, **kw) -> CapsuleInfo:
    rows = _rows(db, info.tip_id)
    page = store.put_bytes(canonical_bytes({"rows": [[r["role"], r["content"]] for r in rows]}), "application/json")
    doc = {
        "kind": "ctxh.capsule",
        "v": 1,
        "job_id": job_id,
        "session": to_wire(info),
        "segments": [{"row_object": page.object_id, "pages": [page.object_id], "rows": len(rows)}],
        "hashes": {"h_all": _rows_hash(rows)},
    }
    m = store.put_bytes(canonical_bytes(doc), "application/json")
    h = _rows_hash(rows)
    return CapsuleInfo(
        capsule_id=m.object_id,
        size=m.size,
        segments=1,
        rows=len(rows),
        max_segment_bytes=page.size,
        approx_tokens=len(rows) * 10,
        h_all=h,
        h_rows=h,
        h_model=h,
        revision=tuple(db.conversation_revision(info.tip_id, include_ancestors=True)),
    )


def check_revision(db, info, exported):
    now = tuple(db.conversation_revision(info.tip_id, include_ancestors=True))
    if now == tuple(exported.revision):
        return PrepareOutcome.OK, []
    rows = _rows(db, info.tip_id)
    if len(rows) > exported.rows:
        return PrepareOutcome.DELTA, rows[exported.rows :]
    return PrepareOutcome.STALE_SESSION, []


def export_delta(db, info, exported, new_rows, *, store) -> DeltaInfo:
    rows = _rows(db, info.tip_id)
    m = store.put_bytes(
        canonical_bytes({"kind": "ctxh.delta", "v": 1, "rows": [[r["role"], r["content"]] for r in new_rows]}),
        "application/json",
    )
    h = _rows_hash(rows)
    return DeltaInfo(
        delta_id=m.object_id,
        tip_id=info.tip_id,
        rows=len(new_rows),
        h_all=h,
        h_rows=h,
        h_model=h,
        revision=tuple(db.conversation_revision(info.tip_id, include_ancestors=True)),
    )


def read_capsule(store, capsule_id, *, max_json_bytes):
    return store.doc(capsule_id)


@dataclass(frozen=True)
class _Seg:
    """The fields of B's ``SegmentPlan`` the target reads (the tip's ``model``/``model_config`` feed the real
    ``offline_capable``)."""

    old_id: str
    new_id: str
    path: str = "A"
    model: str | None = None
    model_config: dict | None = None


def plan_import(
    db, capsule, *, cwd_for, model_policy, model_override, target_config, provider_usable, max_session_bytes
):
    old = capsule["session"]["tip_id"]
    new = "imp-" + capsule["job_id"][-8:]
    return SimpleNamespace(
        segments=(_Seg(old, new),),
        cwd=cwd_for(capsule["session"].get("cwd")),
        model_change=None,
        git_repo_root=None,
        capsule=capsule,
    )


def import_hidden(db, store, capsule_id, plan, *, cancelled=lambda: False, **_kw) -> PreparedInfo:
    doc = store.doc(capsule_id)
    page = store.doc(doc["segments"][0]["pages"][0])
    sid = plan.segments[0].new_id
    db.create_session(sid, "cli", model="test-model", cwd=plan.cwd)
    for role, content in page["rows"]:
        db.append_message(sid, role, content)
    db.set_session_hidden(sid, True)
    h = doc["hashes"]["h_all"]
    return PreparedInfo(
        tip_id=sid,
        root_id=sid,
        title="T",
        h_all=h,
        h_rows=h,
        h_model=h,
        import_path="A",
        dests=(),
        warnings=(),
        source_revision=tuple(doc["session"]["revision"]),
    )


def apply_delta(db, store, delta, prepared):
    for role, content in store.doc(delta.delta_id)["rows"]:
        db.append_message(prepared.tip_id, role, content)
    return replace(
        prepared, h_all=delta.h_all, h_rows=delta.h_rows, h_model=delta.h_model, source_revision=delta.revision
    )


def unhide(db, prepared):
    db.set_session_hidden(prepared.tip_id, False)


def hide(db, prepared):
    db.set_session_hidden(prepared.tip_id, True)


def discard(db, prepared):
    try:
        db.delete_session(prepared.tip_id)
    except Exception:  # noqa: S110 - stand-in: already gone
        pass


def write_side_state(db, capsule, new_tip, *, goal_max_turns=20):
    db.set_meta(f"goal:{new_tip}", json.dumps({"status": "paused"}))


def clear_side_state(db, new_tip):
    db.set_meta(f"goal:{new_tip}", json.dumps({"status": "cleared"}))


@dataclass(frozen=True)
class Busy:
    busy: bool = False
    reasons: tuple = ()
    warnings: tuple = ()
    processes: tuple = ()
    delegations: tuple = ()
    cron: tuple = ()


class LockStandIn:
    """``lock`` stand-in: ledger rows only (leases are B's); records the call order."""

    def __init__(self) -> None:
        self.calls: list[str] = []
        self.busy = Busy()
        self.refresh_ok = True

    def holder_string(self, job_id):
        return f"pid=1:job={job_id}"

    def lease_timing(self, options):
        return (120, 30)

    def probe_busy(self, db, state_db, info, hermes_home, session_key):
        self.calls.append("probe_busy")
        return self.busy

    def acquire(self, db, ledger, info, job_id, *, ttl_s, wait_s, cancelled):
        self.calls.append("acquire")
        ledger.source_lock(job_id, info, self.holder_string(job_id), None)
        ledger.source_set(job_id, "locked")
        return self.holder_string(job_id)

    def refresh(self, db, info, holder, *, ttl_s):
        self.calls.append("refresh")
        return self.refresh_ok

    def prepare(self, db, ledger, info, job_id, peer_name, now):
        self.calls.append("prepare")
        ledger.source_set(job_id, "preparing")
        ledger.veto_add(job_id, info.lineage_ids, info.root_id)
        ledger.source_set(job_id, "prepared", prepared_at=1.0)

    def retire(self, db, ledger, info, holder, job_id, ack, out_rows):
        self.calls.append("retire")
        ledger.source_set(job_id, "retiring", ack=to_wire(ack))
        for row in out_rows:
            ledger.map_put(row)
        self.calls.append("release_leases")
        ledger.source_set(job_id, "retired")

    def release(self, db, ledger, info, holder, job_id):
        self.calls.append("release")
        ledger.veto_remove(job_id)
        ledger.source_set(job_id, "released")

    def suspend(self, db, ledger, info, holder, job_id):
        self.calls.append("suspend")
        ledger.source_set(job_id, "suspended")


# ---------------------------------------------------------------------------------------------------------
# D: planning, environment, note
# ---------------------------------------------------------------------------------------------------------


class PlanStandIn:
    """D's planning port: plan building and the source restriction are stand-ins; the pure functions the target
    reads (``final_doc``, ``instruction_files``) are D's own (no second implementation)."""

    final_doc = staticmethod(planning.final_doc)
    instruction_files = staticmethod(planning.instruction_files)

    def __init__(self, items) -> None:
        self.items = tuple(items)
        self.build_calls: list[dict] = []

    def build_plan(
        self,
        job_id,
        revision,
        candidates,
        refinement,
        report,
        hints,
        cfg,
        git_info,
        busy,
        suggestions,
        options,
        approx_tokens,
        *,
        redact=None,
        remote_driven=False,
        suggest_dest=None,
    ):
        self.build_calls.append({"suggestions": list(suggestions), "hints": hints, "remote_driven": remote_driven})
        return ResourcePlan(
            job_id=job_id,
            revision=tuple(revision),
            items=self.items,
            not_transferred=(),
            processes=(),
            delegations=(),
            cron=(),
            refine=report,
            tools_needed=(),
            env_vars_needed=(),
            notes_for_target="",
            approx_tokens=approx_tokens,
        )

    def restrict_to_releasable(self, remote_final, own_plan, cfg, *, remote_hints=None, redact=None):
        own = {(i.item_id, i.source_path) for i in own_plan.items}
        extra = [
            i.item_id
            for i in remote_final.plan.items
            if i.include is Include.YES and (i.item_id, i.source_path) not in own
        ]
        if extra:
            raise PolicyDenied("items not released by this source", items=extra)
        return remote_final

    def plan_id(self, doc):
        return doc_id(doc)


def collect(rows, session_row, hermes, cfg, git, *, source_os):
    return []


@dataclass(frozen=True)
class PackedItem:
    """``materialize.PackedItem`` (contract X12)."""

    doc: dict
    state: dict | None
    excluded: tuple = ()
    skipped: tuple = ()
    warnings: tuple = ()


class PackStandIn:
    """``materialize.pack_plan_item`` for single files (the policy is re-checked at packing, like E's entry);
    records every call's keyword arguments."""

    def __init__(self) -> None:
        self.calls: list[dict] = []

    def pack_plan_item(
        self,
        item,
        *,
        git,
        store,
        cfg,
        workdir,
        created_at,
        basis_candidates=(),
        force=False,
        extra_ignore=(),
        hermes_home=None,
        cancelled=lambda: False,
    ):
        from ctx_handover.engine.policy import check_source_file

        self.calls.append({"item_id": item.item_id, "workdir": workdir, "hermes_home": hermes_home, "force": force})
        kind = "hermes_file" if item.kind is ItemKind.HERMES_FILE else "extra_file"
        verdict = check_source_file(Path(item.source_path), cfg, explicit=True, follow_link=False)
        if not verdict.allowed:
            return PackedItem({"item_id": item.item_id, "kind": kind, "object": None, "skipped": verdict.reason}, None)
        with open(item.source_path, "rb") as fh:
            m = store.put_bytes(fh.read(), "application/octet-stream")
        doc = {"item_id": item.item_id, "kind": kind, "object": m.object_id, "size": m.size, "mode": "100644"}
        if kind == "hermes_file":
            doc.update(hermes_kind=item.hermes_kind, rel=Path(item.source_path).name)
        return PackedItem(doc, None)


def refine(candidates, target, *, approx_tokens, options, side_question, llm, stored, rows=None, redact=None):
    return stored, RefineReport(used="stored" if stored else "off")


def probe(*, instance_name, options, host, config, tools_seen=(), capabilities=frozenset()):
    facts = make_facts(instance_name or "x")
    return replace(facts, tools_seen={name: None for name in tools_seen})


def tools_seen_from_rows(rows):
    return {str(r.get("content")).split()[1] for r in rows if str(r.get("content") or "").startswith("$ ")}


def delta(source, target):
    return {}


def compose(**kw):
    return ("<ctx-handover-note>facts</ctx-handover-note>", "# report\n")


# ---------------------------------------------------------------------------------------------------------
# E: workspace (real file system)
# ---------------------------------------------------------------------------------------------------------


class GitProbe:
    def __init__(self, git, *, profile: str = "source") -> None:
        self.git, self.profile = git, profile

    def info(self, root, cfg):
        return None


def worktree_state(git, root, exclude_pathspecs, tmp):
    raise AssertionError("no git items in these tests")


def recheck(git, root, recorded, tmp):
    """``gitpack.recheck`` (DESIGN §9.3 step 8) over the stand-in ``worktree_state``: ``(equal, current)``."""
    current = worktree_state(git, root, recorded.exclude_pathspecs, tmp)
    keys = ("head", "index_tree", "worktree_tree")
    return tuple(getattr(current, k) for k in keys) == tuple(getattr(recorded, k) for k in keys), current


class Assessment(SimpleNamespace):
    pass


def assess(
    item, dest, *, git, probe, out_row, tree_fingerprint, target_facts, fallback, tmp, cfg, recorded, local_choice
):
    return Assessment(mode=DestMode.FRESH, conflict=None, git_have=(), need_bytes=item.size_estimate, warnings=())


class WorkspaceTxn:
    """``materialize.WorkspaceTxn`` stand-in: files of a tree manifest into a 0700 partial dir, renamed at c1."""

    fail: ClassVar[dict[str, BaseException]] = {}  # step name -> exception raised by forward/inverse

    def __init__(self, job_id, staging, git, store, journal, *, cfg=None, rules=None, now=None, cancelled=None):
        self.job_id, self.staging, self.store, self.journal = job_id, Path(staging), store, journal
        self.items: dict[str, dict] = {}

    def snapshot(self) -> dict:
        return {"v": 1, "job_id": self.job_id, "items": {k: dict(v) for k, v in self.items.items()}}

    @classmethod
    def restore(cls, snapshot, staging, git, store, journal, **kw):
        txn = cls(snapshot["job_id"], staging, git, store, journal, **kw)
        txn.items = {k: dict(v) for k, v in snapshot["items"].items()}
        return txn

    def map_states(self) -> dict:
        return {k: {"tree": {"v": 1, "files": {}}} for k in self.items}

    def prepare_item(self, item, decision, pack_item, *, out_row) -> None:
        partial = self.staging / "partial" / item.item_id
        partial.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(partial, 0o700)
        tree = self.store.doc(pack_item["manifest"])
        for entry in tree["entries"]:
            with self.store.open_object(entry["object"]) as fh:
                (partial / entry["path"]).write_bytes(fh.read())
        state = {"partial": str(partial), "dest": os.path.expanduser(decision.dest), "mode": str(decision.mode)}
        self.items[item.item_id] = state
        self.journal("prepared", item.item_id, state)

    def commit_steps(self):
        steps = []
        for item_id, st in self.items.items():

            def fwd(st=st, item_id=item_id):
                exc = self.fail.get(f"c1:{item_id}")
                if exc:
                    raise exc
                dest = Path(st["dest"])
                if dest.exists() and not Path(st["partial"]).exists():
                    return  # already renamed (idempotent repeat)
                dest.parent.mkdir(parents=True, exist_ok=True)
                os.rename(st["partial"], dest)

            def inv(st=st, item_id=item_id):
                exc = self.fail.get(f"undo:c1:{item_id}")
                if exc:
                    raise exc
                if Path(st["dest"]).exists():
                    os.rename(st["dest"], st["partial"])

            steps.append((f"c1:{item_id}", fwd, inv))  # E's step names (X11, W021)
        return steps

    def place_hermes_files(self, files):
        placed: list[Path] = []

        def fwd():
            exc = self.fail.get("c3:hermes_files")
            if exc:
                raise exc
            for pack_item, dest in files:
                dest.parent.mkdir(parents=True, exist_ok=True)
                with self.store.open_object(pack_item["object"]) as fh:
                    dest.write_bytes(fh.read())
                placed.append(dest)

        def inv():
            for _pack_item, dest in files:
                if dest.exists():
                    dest.unlink()

        return fwd, inv

    def discard(self) -> None:
        shutil.rmtree(self.staging / "partial", ignore_errors=True)

    def dests(self):
        return [(i, st["dest"], DestMode(st["mode"])) for i, st in self.items.items()]


def choose_dest(item, *, source_facts, target_facts, lookup, options):
    root = Path(os.path.expanduser(options.workspace_root))
    return str(root / Path(item.source_path).name), "workspace-root"


def choose_all(items, *, source_facts, target_facts, lookup, options, local_choice=True, job_id="", cfg=None):
    return [
        choose_dest(i, source_facts=source_facts, target_facts=target_facts, lookup=lookup, options=options)
        for i in items
    ]


def fallback_dest(dest, *, now, workspace_root, allow_roots):
    return dest + "-handover-1"


def map_cwd(source_cwd, item, dest):
    rel = os.path.relpath(source_cwd, item.source_path) if source_cwd.startswith(item.source_path) else "."
    return os.path.normpath(os.path.join(dest, rel))


def target_path(kind, basename, content_sha256, target, new_session_id):
    return Path(target.home) / "cache" / kind / basename


# ---------------------------------------------------------------------------------------------------------
# F: records, continuation
# ---------------------------------------------------------------------------------------------------------


def enqueue(ledger, kind, data, *, redact):
    ledger.outbox_put(kind, data)


def server_for(config, settings):
    return "ctx"


class Spawner:
    def __init__(self) -> None:
        self.spawned: list[tuple] = []

    def spawn(self, session_id, prompt, job_id, *, requested_by, verified=True, cwd=None):
        self.spawned.append((session_id, prompt, job_id, requested_by))
        return ContinueResult(started=True, pid=4321, log="/log")


def source_ports(items, lock=None, plan=None, pack=None):
    plan = plan or PlanStandIn(items)
    return SimpleNamespace(
        collector=SimpleNamespace(collect=collect),
        ctxrecords=SimpleNamespace(enqueue=enqueue, server_for=server_for),
        envfacts=SimpleNamespace(probe=probe, delta=delta, tools_seen_from_rows=tools_seen_from_rows),
        gitops=SimpleNamespace(GitProbe=GitProbe, worktree_state=worktree_state),
        gitpack=SimpleNamespace(recheck=recheck),
        lock=lock or LockStandIn(),
        materialize=pack or PackStandIn(),
        plan=plan,
        refine=SimpleNamespace(refine=refine),
        session_export=SimpleNamespace(
            describe=describe, export_capsule=export_capsule, check_revision=check_revision, export_delta=export_delta
        ),
    )


def target_ports(txn_cls=WorkspaceTxn):
    return SimpleNamespace(
        ctxrecords=SimpleNamespace(enqueue=enqueue, server_for=server_for),
        envfacts=SimpleNamespace(probe=probe, delta=delta),
        gitops=SimpleNamespace(GitProbe=GitProbe),
        hermes_files=SimpleNamespace(target_path=target_path),
        materialize=SimpleNamespace(assess=assess, WorkspaceTxn=txn_cls),
        note=SimpleNamespace(compose=compose, case_collision_texts=real_note.case_collision_texts),
        pathmap=SimpleNamespace(
            choose_dest=choose_dest, choose_all=choose_all, fallback_dest=fallback_dest, map_cwd=map_cwd
        ),
        plan=PlanStandIn(()),
        session_export=SimpleNamespace(read_capsule=read_capsule),
        session_import=SimpleNamespace(
            map_model=real_session_import.map_model,
            offline_capable=real_session_import.offline_capable,
            plan_import=plan_import,
            import_hidden=import_hidden,
            apply_delta=apply_delta,
            unhide=unhide,
            hide=hide,
            discard=discard,
            write_side_state=write_side_state,
            clear_side_state=clear_side_state,
        ),
    )


def tree_pack(store, item_id, files: dict[str, bytes], final_plan_id: str) -> str:
    """A pack document with one file-tree item (built through the store, like ``LocalSource._pack``)."""
    entries = []
    for path, data in sorted(files.items()):
        m = store.put_bytes(data, "application/octet-stream")
        entries.append({"path": path, "type": "file", "mode": "0644", "size": len(data), "object": m.object_id})
    tree = store.put_bytes(canonical_bytes({"kind": "ctxh.tree", "v": 1, "entries": entries}), "application/json")
    doc = {
        "kind": "ctxh.pack",
        "v": 1,
        "job_id": "01JZJ000000000000000000000",
        "final_plan_id": final_plan_id,
        "items": [{"item_id": item_id, "kind": "file_tree", "manifest": tree.object_id}],
    }
    return store.put_bytes(canonical_bytes(doc), "application/json").object_id
