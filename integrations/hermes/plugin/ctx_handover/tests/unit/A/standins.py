"""Stand-ins for the cluster B/C/D/E/F functions ``LocalSource``/``LocalTarget`` call (layer 1 only).

They follow the signatures of INTERFACES §4.1–§4.6 and run on real substrates where it matters for the
orchestration: the real ``SessionDB`` (hidden flag, revision, messages), the real file system (partial
directories renamed into place) and a real ``Ledger``. In layer 2 the real modules replace them through the
``ports`` parameter's default.
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

from ctx_handover.engine.errors import NotFound, PolicyDenied
from ctx_handover.engine.jsonutil import canonical_bytes, doc_id, sha256_hex, to_wire
from ctx_handover.engine.model import (
    CapsuleInfo,
    ContinueResult,
    DeltaInfo,
    DestMode,
    Include,
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
    """In-memory BlockStore (BlockSource + BlockSink + object helpers of ``blocks.BlockStore``)."""

    def __init__(self, block_size: int = 4096) -> None:
        self.block_size = block_size
        self.objects: dict[str, ObjectManifest] = {}
        self.data: dict[str, bytes] = {}
        self.released: list[tuple[str, bool]] = []

    def put_bytes(self, data: bytes, media: str) -> ObjectManifest:
        ids = []
        for off in range(0, max(len(data), 1), self.block_size):
            raw = data[off : off + self.block_size]
            bid = sha256_hex(raw)
            self.data[bid] = raw
            ids.append(bid)
        m = ObjectManifest(
            sha256=sha256_hex(data), size=len(data), block_size=self.block_size, blocks=tuple(ids), media=media
        )
        m = replace(m, object_id=doc_id({k: v for k, v in to_wire(m).items() if k != "object_id"}))
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
    old_id: str
    new_id: str
    path: str = "A"


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


def import_hidden(db, store, capsule_id, plan, *, cancelled=lambda: False) -> PreparedInfo:
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
    ):
        self.build_calls.append({"suggestions": list(suggestions), "hints": hints})
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

    def restrict_to_releasable(self, remote_final, own_plan, cfg):
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


def refine(candidates, target, *, approx_tokens, options, side_question, llm, stored):
    return stored, RefineReport(used="stored" if stored else "off")


def probe(*, instance_name, options, host, config, tools_seen=(), capabilities=frozenset()):
    return make_facts(instance_name or "x")


def delta(source, target):
    return {}


def compose(**kw):
    return ("<ctx-handover-note>facts</ctx-handover-note>", "# report\n")


# ---------------------------------------------------------------------------------------------------------
# E: workspace (real file system)
# ---------------------------------------------------------------------------------------------------------


class GitProbe:
    def __init__(self, git) -> None:
        self.git = git

    def info(self, root, cfg):
        return None


def worktree_state(git, root, exclude_pathspecs, tmp):
    raise AssertionError("no git items in these tests")


class Assessment(SimpleNamespace):
    pass


def assess(
    item, dest, *, git, probe, out_row, tree_fingerprint, target_facts, fallback, tmp, cfg, recorded, local_choice
):
    return Assessment(mode=DestMode.FRESH, conflict=None, git_have=(), need_bytes=item.size_estimate, warnings=())


class WorkspaceTxn:
    """``materialize.WorkspaceTxn`` stand-in: files of a tree manifest into a 0700 partial dir, renamed at c1."""

    fail: ClassVar[dict[str, BaseException]] = {}  # step name -> exception raised by forward/inverse

    def __init__(self, job_id, staging, git, store, journal) -> None:
        self.job_id, self.staging, self.store, self.journal = job_id, Path(staging), store, journal
        self.items: dict[str, dict] = {}

    @classmethod
    def restore(cls, job_id, staging, git, store, journal, entries):
        txn = cls(job_id, staging, git, store, journal)
        for e in entries:
            if e.get("ws_kind") == "prepared":
                txn.items[e["step"]] = e["data"]
        return txn

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

            steps.append((item_id, fwd, inv))
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


class Spawner:
    def __init__(self) -> None:
        self.spawned: list[tuple] = []

    def spawn(self, session_id, prompt, job_id, *, requested_by):
        self.spawned.append((session_id, prompt, job_id, requested_by))
        return ContinueResult(started=True, pid=4321, log="/log")


def source_ports(items, lock=None, plan=None):
    plan = plan or PlanStandIn(items)
    return SimpleNamespace(
        collector=SimpleNamespace(collect=collect),
        ctxrecords=SimpleNamespace(enqueue=enqueue),
        envfacts=SimpleNamespace(probe=probe, delta=delta),
        filetree=None,
        gitops=SimpleNamespace(GitProbe=GitProbe, worktree_state=worktree_state),
        gitpack=None,
        hermes_files=None,
        lock=lock or LockStandIn(),
        plan=plan,
        refine=SimpleNamespace(refine=refine),
        session_export=SimpleNamespace(
            describe=describe, export_capsule=export_capsule, check_revision=check_revision, export_delta=export_delta
        ),
    )


def target_ports(txn_cls=WorkspaceTxn):
    return SimpleNamespace(
        ctxrecords=SimpleNamespace(enqueue=enqueue),
        envfacts=SimpleNamespace(probe=probe, delta=delta),
        gitops=SimpleNamespace(GitProbe=GitProbe),
        hermes_files=SimpleNamespace(target_path=target_path),
        materialize=SimpleNamespace(assess=assess, WorkspaceTxn=txn_cls),
        note=SimpleNamespace(compose=compose),
        pathmap=SimpleNamespace(choose_dest=choose_dest, fallback_dest=fallback_dest, map_cwd=map_cwd),
        plan=PlanStandIn(()),
        session_export=SimpleNamespace(read_capsule=read_capsule),
        session_import=SimpleNamespace(
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
        "job_id": "J",
        "final_plan_id": final_plan_id,
        "items": [{"item_id": item_id, "kind": "file_tree", "manifest": tree.object_id}],
    }
    return store.put_bytes(canonical_bytes(doc), "application/json").object_id
