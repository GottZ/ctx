"""In-memory stand-in for cluster B's session surface (``session_export``, ``session_import``, ``lock``).

For cluster A's orchestration tests: ``FakeSession`` holds sessions as plain dicts and exposes three namespaces
with the exact function names and parameter names of the real modules (checked by
``tests/unit/B/test_session_fake.py``)::

    fake = FakeSession()
    tip = fake.add_session(rows=4, title="Work")
    monkeypatch.setattr(local_source, "session_export", fake.session_export)   # however A wires modules
    fake.fail("import_hidden", VerifyFailed("tampered"))                      # next call raises
    fake.script("check_revision", PrepareOutcome.DELTA)                        # next outcome
    fake.calls                                                                 # [(name, kwargs), …]

Objects (capsule, delta) are written to and read from whatever ``BlockSink``/``BlockSource`` the caller passes,
through ``session_hash.put_json``/``read_json``, so A's block mover sees real objects. Hashes are sha256 over the
fake rows; nothing here touches a ``SessionDB``. The ``db`` parameters are accepted and ignored.
"""

from __future__ import annotations

import hashlib
import json
import time
from collections import defaultdict, deque
from dataclasses import dataclass, field, replace
from types import SimpleNamespace
from typing import Any

from ctx_handover.engine.errors import (
    AlreadyHandingOver,
    InvalidState,
    NotFound,
    SessionBusy,
    VerifyFailed,
)
from ctx_handover.engine.ids import mint_session_id, short
from ctx_handover.engine.model import (
    CapsuleInfo,
    DeltaInfo,
    PreparedInfo,
    PrepareOutcome,
    SessionInfo,
    SourceState,
)
from ctx_handover.engine.session_hash import put_json, read_json


@dataclass
class FakeBusyReport:
    """Satisfies ``contracts.BusyReportLike`` and has the fields of ``lock.BusyReport``."""

    busy: bool = False
    reasons: tuple[str, ...] = ()
    warnings: tuple[str, ...] = ()
    processes: tuple[dict, ...] = ()
    delegations: tuple[dict, ...] = ()
    cron: tuple[dict, ...] = ()


@dataclass
class FakeSegmentPlan:
    old_id: str
    new_id: str
    parent_new_id: str | None
    is_tip: bool
    title: str | None
    path: str = "A"
    model: str | None = None
    model_config: dict | None = None
    rows: int = 0
    bytes: int = 0


@dataclass
class FakeImportPlan:
    segments: tuple
    model_change: dict | None
    cwd: str
    git_repo_root: str | None
    job_short: str = ""
    warnings: tuple[str, ...] = field(default=())


def _digest(obj: object) -> str:
    return hashlib.sha256(json.dumps(obj, sort_keys=True, default=str).encode()).hexdigest()


class FakeSession:
    """See module docstring. ``sessions[id] = {"row": {...}, "rows": [...], "hidden": bool, "archived": bool,
    "ended": str|None, "title": str|None}``; ``handovers[job_id]`` is the source state of a job."""

    def __init__(self) -> None:
        self.sessions: dict[str, dict] = {}
        self.handovers: dict[str, dict] = {}
        self.leases: dict[str, str] = {}
        self.calls: list[tuple[str, dict]] = []
        self._faults: dict[str, deque] = defaultdict(deque)
        self._scripts: dict[str, deque] = defaultdict(deque)
        self.session_export = SimpleNamespace(
            describe=self.describe,
            export_capsule=self.export_capsule,
            check_revision=self.check_revision,
            export_delta=self.export_delta,
            iter_capsule_rows=self.iter_capsule_rows,
            read_capsule=self.read_capsule,
        )
        self.session_import = SimpleNamespace(
            plan_import=self.plan_import,
            import_hidden=self.import_hidden,
            apply_delta=self.apply_delta,
            unhide=self.unhide,
            hide=self.hide,
            discard=self.discard,
            write_side_state=self.write_side_state,
            clear_side_state=self.clear_side_state,
            map_model=self.map_model,
            offline_capable=self.offline_capable,
        )
        self.lock = SimpleNamespace(
            holder_string=self.holder_string,
            lease_timing=self.lease_timing,
            probe_busy=self.probe_busy,
            acquire=self.acquire,
            refresh=self.refresh,
            prepare=self.prepare,
            retire=self.retire,
            release=self.release,
            reclaim=self.reclaim,
            mark_exported=self.mark_exported,
            veto_texts=self.veto_texts,
        )

    # -- scripting --------------------------------------------------------------------------------------
    def fail(self, name: str, exc: BaseException) -> None:
        """The next call of ``name`` raises ``exc``."""
        self._faults[name].append(exc)

    def script(self, name: str, value: Any) -> None:
        """The next call of ``name`` returns ``value`` (``check_revision``: a ``PrepareOutcome``; ``probe_busy``: a
        ``FakeBusyReport``; ``acquire``: ``"busy"`` raises ``SessionBusy``)."""
        self._scripts[name].append(value)

    def _enter(self, name: str, **kwargs) -> Any:
        self.calls.append((name, kwargs))
        if self._faults[name]:
            raise self._faults[name].popleft()
        return self._scripts[name].popleft() if self._scripts[name] else None

    # -- fixtures -----------------------------------------------------------------------------------------
    def add_session(
        self,
        *,
        rows: int = 4,
        title: str | None = "Work",
        cwd: str = "/srv/work",
        parent: str | None = None,
        model: str = "fake-model",
    ) -> str:
        sid = mint_session_id()
        while sid in self.sessions:
            sid = mint_session_id()
        msgs = [{"role": "user" if i % 2 == 0 else "assistant", "content": f"m{i}", "active": 1} for i in range(rows)]
        self.sessions[sid] = {
            "row": {"id": sid, "cwd": cwd, "model": model, "parent": parent},
            "rows": msgs,
            "hidden": False,
            "archived": False,
            "ended": None,
            "title": title,
        }
        return sid

    def append(self, sid: str, content: str) -> None:
        """A message typed on the source during the handover (drives ``check_revision`` → delta)."""
        self.sessions[sid]["rows"].append({"role": "user", "content": content, "active": 1})

    def _revision(self, sid: str) -> tuple:
        rows = self.sessions[sid]["rows"]
        return ((sid,), len(rows), len(rows), 0)

    # -- session_export -------------------------------------------------------------------------------------
    def describe(self, db, ref, *, calling_session):
        self._enter("describe", ref=ref, calling_session=calling_session)
        sid = ref.id or calling_session
        if ref.latest and not sid:
            sid = next(reversed(self.sessions), None)
        if not sid or sid not in self.sessions:
            raise NotFound("session not found", session_id=sid)
        s = self.sessions[sid]
        return SessionInfo(
            sid,
            sid,
            (sid,),
            s["title"],
            s["row"]["model"],
            None,
            s["row"]["cwd"],
            None,
            None,
            len(s["rows"]),
            self._revision(sid),
        )

    def export_capsule(
        self,
        db,
        info,
        *,
        job_id,
        store,
        source_meta,
        hermes,
        page_rows=2000,
        page_bytes=4 * 1024**2,
        cancelled=lambda: False,
    ):
        self._enter("export_capsule", job_id=job_id)
        rows = list(self.sessions[info.tip_id]["rows"])
        doc = {
            "kind": "ctxh.capsule",
            "v": 1,
            "job_id": job_id,
            "fake": True,
            "tip_id": info.tip_id,
            "rows": rows,
            "title": self.sessions[info.tip_id]["title"],
            "source": dict(source_meta),
        }
        m = put_json(store, doc)
        h = _digest(rows)
        return CapsuleInfo(
            m.object_id, m.size, 1, len(rows), m.size, len(rows) * 10, h, h, h, self._revision(info.tip_id)
        )

    def check_revision(self, db, info, exported):
        scripted = self._enter("check_revision", tip=info.tip_id)
        if scripted is not None:
            outcome = PrepareOutcome(scripted)
        elif self._revision(info.tip_id) == tuple(exported.revision):
            outcome = PrepareOutcome.OK
        else:
            outcome = PrepareOutcome.DELTA
        new = self.sessions[info.tip_id]["rows"][exported.rows :] if outcome is PrepareOutcome.DELTA else []
        return outcome, list(new)

    def export_delta(self, db, info, exported, new_rows, *, store):
        self._enter("export_delta", rows=len(new_rows))
        rows = list(self.sessions[info.tip_id]["rows"])
        m = put_json(store, {"kind": "ctxh.delta", "v": 1, "fake": True, "rows": list(new_rows)})
        h = _digest(rows)
        return DeltaInfo(m.object_id, info.tip_id, len(new_rows), h, h, h, self._revision(info.tip_id))

    def iter_capsule_rows(self, store, capsule_id, *, max_json_bytes=64 * 1024**2):
        self._enter("iter_capsule_rows", capsule_id=capsule_id)
        for row in self.read_capsule(store, capsule_id, max_json_bytes=max_json_bytes)["rows"]:
            yield 0, row

    def read_capsule(self, store, capsule_id, *, max_json_bytes, **caps):
        self._enter("read_capsule", capsule_id=capsule_id)
        return read_json(store, capsule_id, max_bytes=max_json_bytes)

    # -- session_import ---------------------------------------------------------------------------------------
    def plan_import(
        self, db, capsule, *, cwd_for, model_policy, model_override, target_config, provider_usable, max_session_bytes
    ):
        self._enter("plan_import", model_policy=model_policy)
        new_id = mint_session_id()
        seg = FakeSegmentPlan(capsule.get("tip_id", "?"), new_id, None, True, capsule.get("title"))
        return FakeImportPlan((seg,), None, cwd_for(None), None, short(str(capsule.get("job_id") or "")))

    def import_hidden(
        self,
        db,
        store,
        capsule_id,
        plan,
        *,
        cancelled=lambda: False,
        local_hermes_version=None,
        max_json_bytes=64 * 1024**2,
        max_session_bytes=512 * 1024**2,
    ):
        self._enter("import_hidden", capsule_id=capsule_id)
        capsule = read_json(store, capsule_id, max_bytes=max_json_bytes)
        seg = plan.segments[-1]
        rows = list(capsule.get("rows") or [])
        self.sessions[seg.new_id] = {
            "row": {"id": seg.new_id, "cwd": plan.cwd, "model": None, "parent": None},
            "rows": rows,
            "hidden": True,
            "archived": False,
            "ended": None,
            "title": seg.title,
        }
        h = _digest(rows)
        return PreparedInfo(seg.new_id, seg.new_id, seg.title, h, h, h, "A", (), (), ())

    def apply_delta(self, db, store, delta, prepared):
        self._enter("apply_delta", delta_id=delta.delta_id)
        doc = read_json(store, delta.delta_id, max_bytes=64 * 1024**2)
        self.sessions[prepared.tip_id]["rows"].extend(doc.get("rows") or [])
        h = _digest(self.sessions[prepared.tip_id]["rows"])
        if h != delta.h_all:
            raise VerifyFailed("fake delta mismatch")
        return replace(prepared, h_all=h, h_rows=h, h_model=h, source_revision=tuple(delta.revision))

    def unhide(self, db, prepared):
        self._enter("unhide", tip=prepared.tip_id)
        self.sessions[prepared.tip_id]["hidden"] = False

    def hide(self, db, prepared):
        self._enter("hide", tip=prepared.tip_id)
        self.sessions[prepared.tip_id]["hidden"] = True

    def discard(self, db, prepared):
        self._enter("discard", tip=prepared.tip_id)
        self.sessions.pop(prepared.tip_id, None)

    def write_side_state(self, db, capsule, new_tip, *, goal_max_turns=20):
        self._enter("write_side_state", new_tip=new_tip)

    def clear_side_state(self, db, new_tip):
        self._enter("clear_side_state", new_tip=new_tip)

    def map_model(self, row, policy, override, target_config, provider_usable):
        self._enter("map_model", policy=policy)
        return {
            "model": override or row.get("model"),
            "model_config": None,
            "billing_provider": None,
            "billing_base_url": None,
            "billing_mode": None,
        }, None

    def offline_capable(self, model_config, target_config):
        self._enter("offline_capable")
        return False

    # -- lock ---------------------------------------------------------------------------------------------------
    def holder_string(self, job_id):
        return f"pid=0:ctx_handover:job={job_id}"

    def lease_timing(self, options):
        return int(options.lease_ttl_s), 30

    def probe_busy(self, db, state_db, info, hermes_home, session_key):
        scripted = self._enter("probe_busy", tip=info.tip_id)
        return scripted if scripted is not None else FakeBusyReport()

    def acquire(self, db, ledger, info, job_id, *, ttl_s, wait_s, cancelled, **kw):
        scripted = self._enter("acquire", job_id=job_id, wait_s=wait_s)
        if scripted == "busy":
            raise SessionBusy("A turn is running in this session.")
        if any(
            h["root_id"] == info.root_id and h["state"] not in ("released", "reclaimed")
            for j, h in self.handovers.items()
            if j != job_id
        ):
            raise AlreadyHandingOver("already handing over", root_id=info.root_id)
        holder = self.holder_string(job_id)
        self.handovers[job_id] = {
            "root_id": info.root_id,
            "tip_id": info.tip_id,
            "state": "locked",
            "holder": holder,
            "at": time.time(),
        }
        self.leases[info.root_id] = holder
        return holder

    def refresh(self, db, info, holder, *, ttl_s):
        scripted = self._enter("refresh", holder=holder)
        return bool(scripted) if scripted is not None else self.leases.get(info.root_id) == holder

    def _set(self, job_id, expect, to):
        h = self.handovers.get(job_id)
        if h is None or (expect and h["state"] not in expect):
            raise InvalidState(f"source handover is {h and h['state']}", job_id=job_id)
        h["state"] = to

    def prepare(self, db, ledger, info, job_id, peer_name, now):
        self._enter("prepare", job_id=job_id, peer=peer_name)
        if self.handovers[job_id]["state"] == SourceState.PREPARED:
            return
        self._set(job_id, ("locked", "preparing"), SourceState.PREPARED.value)
        s = self.sessions[info.tip_id]
        s.update(hidden=True, archived=True, ended="handed_over", saved_title=s["title"])
        s["title"] = f"{s['title']} (handed over to {peer_name})" if s["title"] else None

    def retire(self, db, ledger, info, holder, job_id, ack, out_rows):
        self._enter("retire", job_id=job_id)
        if self.handovers[job_id]["state"] == SourceState.RETIRED:
            return
        self._set(job_id, ("prepared", "retiring"), SourceState.RETIRED.value)
        self.leases.pop(info.root_id, None)

    def release(self, db, ledger, info, holder, job_id):
        self._enter("release", job_id=job_id)
        h = self.handovers.get(job_id)
        if h is None or h["state"] == "released":
            return
        h["state"] = SourceState.RELEASED.value
        s = self.sessions.get(info.tip_id)
        if s is not None:
            s.update(hidden=False, archived=False, ended=None, title=s.pop("saved_title", s["title"]))
        self.leases.pop(info.root_id, None)

    def reclaim(self, db, ledger, session_id, *, force):
        self._enter("reclaim", session_id=session_id, force=force)
        for job_id, h in self.handovers.items():
            if h["tip_id"] == session_id and h["state"] in ("retired", "exported", "prepared", "retiring"):
                if h["state"] in ("prepared", "retiring") and not force:
                    return {"ok": False, "state": h["state"], "message": "A handover is being committed."}
                h["state"] = SourceState.RECLAIMED.value
                s = self.sessions[session_id]
                s.update(hidden=False, archived=False, ended=None, title=s.pop("saved_title", s["title"]))
                return {"ok": True, "state": "reclaimed", "job_id": job_id, "withheld": []}
        return {"ok": False, "state": None, "message": "Nothing to reclaim."}

    def mark_exported(self, db, ledger, info, holder, job_id, bundle_path):
        self._enter("mark_exported", job_id=job_id, bundle_path=bundle_path)
        self._set(job_id, ("prepared",), SourceState.EXPORTED.value)
        self.leases.pop(info.root_id, None)

    def veto_texts(self, entry):
        return f"This session was handed over (job {short(str(entry.get('job') or entry.get('job_id') or ''))})."
