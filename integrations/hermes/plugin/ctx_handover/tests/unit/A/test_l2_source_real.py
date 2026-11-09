"""LocalSource against the real B/D/E modules (layer 2; contract X12, X13, X14, X17, W060, W076, W084).

Real ``SessionDB`` (Upstream venv), real git repositories (``repo_builder``), real ``BlockStore`` and ``Ledger``; the
``ports`` default (the real ``collector``, ``refine``, ``plan``, ``lock``, ``session_export``, ``envfacts``,
``materialize``). The pull scope is checked the way the listener applies it (``server._job_allowed``): a peer reads
an object only when it is registered with the job, and the blocks of such objects only.
"""

from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path

import pytest

from ctx_handover.engine import local_source as local_source_module
from ctx_handover.engine.blocks import BlockStore
from ctx_handover.engine.conductor import VerifiedBlocks, move_object
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.errors import NotFound, PolicyDenied, SessionBusy
from ctx_handover.engine.gitops import Git
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.jsonutil import from_wire, loads_bounded
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.local_source import LocalSource
from ctx_handover.engine.model import (
    DestMode,
    FinalPlan,
    HermesPaths,
    Include,
    ItemKind,
    ItemRole,
    Mode,
    OpState,
    Options,
    Origin,
    Peer,
    PlanHints,
    PlanItem,
    PreparedInfo,
    PrepareOutcome,
    ResourcePlan,
    Right,
    Role,
    SessionRef,
    TargetDecision,
)
from ctx_handover.engine.ops import OpRunner, doc_refs
from ctx_handover.engine.plan import WHY_PEER_SECRET
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.fakes.apis_fake import RecordingReporter, make_facts
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.fakes.sessiondb import SessionBuilder

MAX_DOC = 64 * 1024 * 1024


class RealSource:
    """One source host with the real modules behind ``LocalSource`` (see module docstring)."""

    def __init__(self, clock, session_db, make_session, user_home, hermes_home, fake_host, *, remote_driven, name):
        self.clock = clock
        self.db = session_db
        self.data = hermes_home / "plugin-data" / f"ctx_handover-{name}"
        self.ledger = Ledger(self.data / "ledger.sqlite3", clock=clock)
        self.store = BlockStore(self.data, self.ledger, clock=clock)
        self.repo = RepoBuilder.init(user_home / f"proj-{name}")
        self.repo.write("README.md", "# app\n")
        self.repo.write("src/main.py", "print('v1')\n")
        self.repo.add_all()
        self.repo.commit("first")
        self.repo.write("src/main.py", "print('v2')\n")
        notes = user_home / f"notes-{name}"
        notes.mkdir()
        self.extra = notes / "plan.txt"
        self.extra.write_text("the plan\n")
        self.unreferenced = notes / "other.txt"
        self.unreferenced.write_text("not in the transcript\n")
        self.policy = PolicyConfig.from_options(
            Options(),
            home=user_home,
            hermes_home=hermes_home,
            hermes_root=hermes_home,
            plugin_dir=self.data,
            case_insensitive=False,
            target_os="linux",
        )
        self.sid = make_session.create(title=f"Real {name}", cwd=str(self.repo.root))
        make_session.user(self.sid, "read the plan and check the repo")
        calls = [
            SessionBuilder.tool_call("read_file", {"path": str(self.extra)}, call_id="c1"),
            SessionBuilder.tool_call("terminal", {"command": "git status && rg TODO src"}, call_id="c2"),
        ]
        make_session.assistant(self.sid, "", tool_calls=calls)
        make_session.tool(self.sid, "c1", "the plan", name="read_file")
        make_session.tool(self.sid, "c2", "clean", name="terminal")
        make_session.assistant(self.sid, "done")
        self.peer = Peer(
            name="laptop", peer_id=ulid(), direction="inbound", url=None, pubkey="pk", rights=frozenset({Right.PULL})
        )
        self.remote_driven = remote_driven
        self.source = LocalSource(
            ledger=self.ledger,
            store=self.store,
            db_factory=lambda: self.db,
            hermes=HermesPaths(home=str(hermes_home), document_cache=str(hermes_home / "cache" / "documents")),
            host=fake_host.host_facts(),
            config={},
            ops=OpRunner(self.ledger, None, inline=True),
            git=Git(),
            calling_session=None,
            session_key=None,
            side_question=None,
            llm=None,
            read_secret=lambda name: None,
            redact=fake_host.redact,
            state_db=hermes_home / "state.db",
            remote_driven=remote_driven,
            policy=self.policy,
            clock=clock,
            data_dir=self.data,
            peer_name="laptop" if remote_driven else None,
        )
        self.ref = SessionRef(id=self.sid)

    def job(self, options: Options | None = None) -> JobContext:
        job_id = ulid()
        options = options or Options(refine="off")
        if self.remote_driven:  # what ``ParticipantRegistry.open`` records for a pull (route #7)
            params = {"role": "source", "mode": "pull", "session_ref": {"id": self.sid}, "options": {}}
            self.ledger.create_job(job_id, Role.SOURCE, Mode.PULL, self.peer.peer_id, params)
        else:  # a local conductor in this process (push): its own row carries the session
            params = {"mode": "push", "session_ref": {"id": self.sid, "latest": False, "offer_id": None}}
            self.ledger.create_job(job_id, Role.CONDUCTOR, Mode.PUSH, None, params)
        return JobContext(
            job_id=job_id,
            mode=Mode.PULL if self.remote_driven else Mode.PUSH,
            peer=self.peer if self.remote_driven else None,
            options=options,
            epoch=1,
            reporter=RecordingReporter(),
            cancelled=lambda: False,
            created_at=self.clock(),
        )

    def plan(self, job: JobContext) -> ResourcePlan:
        st = self.source.start_plan(job, make_facts("laptop"), self.ref)
        assert st.state is OpState.DONE, st.error
        return from_wire(ResourcePlan, st.result)

    @staticmethod
    def final(plan: ResourcePlan, items=None) -> FinalPlan:
        """What a remote conductor sends after negotiation and confirmation: proposals confirmed (``yes``)."""
        items = tuple(
            replace(i, include=Include.YES) if i.include is Include.PROPOSED else i for i in (items or plan.items)
        )
        plan = replace(plan, items=items)
        decisions = tuple(
            TargetDecision(item_id=i.item_id, dest="/laptop" + i.source_path, mode=DestMode.FRESH, reason="test")
            for i in items
            if i.include is Include.YES
        )
        return FinalPlan(
            plan_id="remote", plan=plan, decisions=decisions, need_bytes=0, free_bytes={}, model_change=None
        )

    def doc(self, object_id: str) -> dict:
        return loads_bounded(self.store.read_object_bytes(object_id, max_bytes=MAX_DOC), max_bytes=MAX_DOC)


@pytest.fixture
def mk(clock, session_db, make_session, user_home, hermes_home, fake_host):
    made = []

    def make(*, remote_driven: bool) -> RealSource:
        env = RealSource(
            clock,
            session_db,
            make_session,
            user_home,
            hermes_home,
            fake_host,
            remote_driven=remote_driven,
            name=str(len(made)),
        )
        made.append(env)
        return env

    yield make
    for env in made:
        env.ledger.close()


class JobScope:
    """The pull peer's view of the source's store: only objects registered with the job and their blocks, exactly
    as the listener's routes #19/#20 release them (``server._job_allowed``, R18)."""

    def __init__(self, store: BlockStore, ledger: Ledger, job_id: str) -> None:
        self.store, self.ledger, self.job_id = store, ledger, job_id

    def _allowed(self) -> tuple[set[str], set[str]]:
        objects = set(self.ledger.job_objects(self.job_id))
        blocks = {b for m in self.store.manifests(sorted(objects)) for b in m.blocks}
        return objects, blocks

    def manifests(self, ids):
        objects, _ = self._allowed()
        if any(i not in objects for i in ids):
            raise NotFound("object not part of this job")
        return self.store.manifests(list(ids))

    def read_blocks(self, ids):
        _, blocks = self._allowed()
        return self.store.read_blocks([b for b in ids if b in blocks])


def _closure(env: RealSource, root: str) -> set[str]:
    """Every object reachable from ``root`` in the source's own store (walked here, independent of the source)."""
    seen, todo = {root}, [root]
    while todo:
        doc = env.doc(todo.pop())
        leaves, docs = doc_refs(doc)
        for oid in [*leaves, *docs]:
            if oid not in seen:
                seen.add(oid)
                if oid in docs:
                    todo.append(oid)
    return seen


def test_l2_source_pull_scope_holds_every_object_of_the_job(mk, make_session, tmp_path, clock):
    """★ X13: a pull peer can fetch the whole capsule, pack and delta closure (pages, row objects, git pack, tree
    manifests, file objects) through the job scope — and nothing else: an object stored by another job of the same
    store is refused.

    Mutant: ``LocalSource._register_closure`` registers only the root document (``[root_id]``)."""
    env = mk(remote_driven=True)
    job = env.job()
    plan = env.plan(job)
    info = env.source.lock(job, env.ref, plan.revision)
    assert not info.revision_changed
    capsule = env.source.start_export(job).result
    pack = env.source.start_pack(job, env.final(plan)).result
    make_session.user(env.sid, "typed while the transfer ran")
    prepared = PreparedInfo(
        tip_id="n",
        root_id="n",
        title="T",
        h_all="0" * 64,
        h_rows="0" * 64,
        h_model="0" * 64,
        import_path="A",
        dests=(),
        warnings=(),
        source_revision=(),
    )
    st = env.source.start_prepare(job, prepared)
    assert st.state is OpState.DONE and st.result["outcome"] == PrepareOutcome.DELTA, st.error
    delta = st.result["delta"]["delta_id"]

    expected = _closure(env, capsule["capsule_id"]) | _closure(env, pack["pack_id"]) | _closure(env, delta)
    pack_doc = env.doc(pack["pack_id"])
    assert {i["kind"] for i in pack_doc["items"]} >= {"git_repo", "extra_file"}
    leaves = {i.get("pack") or i.get("object") for i in pack_doc["items"]} - {None}
    assert leaves and leaves <= expected  # the git pack and the extra file object, beside the three roots
    assert len(expected - {capsule["capsule_id"], pack["pack_id"], delta}) >= len(leaves) + 1  # + capsule pages
    assert set(env.ledger.job_objects(job.job_id)) >= expected

    foreign = env.store.put_bytes(b"another job's data", "application/octet-stream")
    scope = VerifiedBlocks(JobScope(env.store, env.ledger, job.job_id))
    dst_ledger = Ledger(tmp_path / "laptop-ledger.sqlite3", clock=clock)
    dst = BlockStore(tmp_path / "laptop-data", dst_ledger, clock=clock)
    try:
        for root in (capsule["capsule_id"], pack["pack_id"], delta):
            move_object(
                scope,
                dst,
                sorted(_closure(env, root)),
                reporter=RecordingReporter(),
                cancelled=lambda: False,
                hosted=True,
            )
        for oid in expected:
            (m,) = dst.manifests([oid])
            assert dst.missing(list(m.blocks)) == []
        with pytest.raises(NotFound):
            scope.manifests([foreign.object_id])
    finally:
        dst_ledger.close()


def test_l2_source_remote_hints_never_add(mk):
    """★ X14 (R18): hints of a remote conductor never add to the source's plan — an ``include`` of a file the
    transcript does not name stays out of a pull plan (the same hint given locally adds it); a remote FinalPlan that
    adds it is refused (``export_roots`` empty); a remote ``include_secret`` is recorded as ignored (W079).

    Mutant: ``LocalSource.start_plan`` calls ``build_plan(..., remote_driven=False)``."""
    hints = PlanHints(include=(), include_secret=())
    env = mk(remote_driven=True)
    other = str(env.unreferenced)
    hints = replace(hints, include=(other,), include_secret=(str(env.extra),))
    job = env.job(Options(refine="off", hints=hints))
    plan = env.plan(job)
    assert other not in {i.source_path for i in plan.items}
    assert str(env.extra) in {i.source_path for i in plan.items}  # named by the transcript: the source's own item

    added = PlanItem(
        item_id="u9",
        kind=ItemKind.EXTRA_FILE,
        source_path=other,
        role=ItemRole.EXTRA,
        origin=Origin.USER,
        include=Include.YES,
        reasons=("user",),
        size_estimate=10,
    )
    env.source.lock(job, env.ref, plan.revision)
    with pytest.raises(PolicyDenied) as exc:
        env.source.start_pack(job, env.final(plan, items=[*plan.items, added]))
    assert exc.value.details["items"] == ["u9"]
    st = env.source.start_pack(job, env.final(plan))
    assert st.state is OpState.DONE, st.error
    restricted = from_wire(FinalPlan, env.source._ctx(job.job_id)["final_plan"])
    assert {"path": str(env.extra), "why": WHY_PEER_SECRET} in [
        {"path": n.path, "why": n.why} for n in restricted.plan.not_transferred
    ]

    local = mk(remote_driven=False)
    local_plan = local.plan(local.job(Options(refine="off", hints=PlanHints(include=(other,)))))
    assert other in {i.source_path for i in local_plan.items}


def test_l2_source_pack_items_come_from_e(mk, hermes_home):
    """X12/X17: every pack item is cluster E's (``materialize.pack_plan_item``): the git item carries
    ``lfs_objects``, ``case_collisions``, ``ntfs_invalid``; an extra directory is a ``file_tree``; a session file
    is a ``hermes_file`` with the plan item's ``hermes_kind`` (no guessing); the ``out`` state of a git item is the
    flat ``map_state_of`` form (X09)."""
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    extra_dir = env.extra.parent
    spill = hermes_home / "cache" / "spillover"
    spill.mkdir(parents=True)
    (spill / "call_1.txt").write_text("big tool output\n")
    more = (
        PlanItem(
            item_id="d1",
            kind=ItemKind.EXTRA_DIR,
            source_path=str(extra_dir),
            role=ItemRole.EXTRA,
            origin=Origin.USER,
            include=Include.YES,
            reasons=("user",),
            size_estimate=30,
        ),
        PlanItem(
            item_id="h1",
            kind=ItemKind.HERMES_FILE,
            source_path=str(spill / "call_1.txt"),
            role=ItemRole.HERMES,
            origin=Origin.TRANSCRIPT,
            include=Include.YES,
            reasons=("spillover: cache/spillover/call_1.txt",),
            size_estimate=16,
            hermes_kind="spillover",
        ),
    )
    env.source.lock(job, env.ref, plan.revision)
    st = env.source.start_pack(job, env.final(plan, items=[*plan.items, *more]))
    assert st.state is OpState.DONE, st.error
    items = {i["item_id"]: i for i in env.doc(st.result["pack_id"])["items"]}
    git = next(i for i in items.values() if i["kind"] == "git_repo")
    assert {"lfs_objects", "case_collisions", "ntfs_invalid", "index_tree", "worktree_tree"} <= set(git)
    assert items["d1"]["kind"] == "file_tree" and items["d1"]["manifest"]
    assert items["h1"]["kind"] == "hermes_file" and items["h1"]["hermes_kind"] == "spillover"
    assert items["h1"]["object"]
    states = env.source._ctx(job.job_id)["pack_states"]
    assert set(states[git["item_id"]]) == {"head", "index_tree", "worktree_tree", "branch", "exclude_pathspecs"}
    assert set(states["d1"]) == {"tree"}


def test_l2_source_facts_carry_transcript_tools_and_rows_come_from_the_lineage_export(mk, monkeypatch):
    """W084: the source's facts look up the tool names the transcript runs (``tools_seen_from_rows``), for a local
    conductor (its row) and a remote one (route #7 row). W076: the collector and the refinement get the rows of
    ``export_session_lineage(tip, include_inactive=True)`` (W077: with ``redact``)."""
    for remote in (False, True):
        env = mk(remote_driven=remote)
        facts = env.source.facts(env.job())
        assert {"git", "rg"} <= set(facts.tools_seen), facts.tools_seen

    env = mk(remote_driven=False)
    seen: dict = {}
    real_collect = env.source.ports.collector.collect
    real_refine = env.source.ports.refine.refine

    def collect(rows, *a, **kw):
        seen["collect"] = rows
        return real_collect(rows, *a, **kw)

    def refine(*a, **kw):
        seen["refine"] = kw
        return real_refine(*a, **kw)

    monkeypatch.setattr(env.source.ports, "collector", type("C", (), {"collect": staticmethod(collect)}))
    monkeypatch.setattr(env.source.ports, "refine", type("R", (), {"refine": staticmethod(refine)}))
    env.plan(env.job())
    want = env.db.export_session_lineage(env.sid, include_inactive=True)["messages"]
    assert [r["id"] for r in seen["collect"]] == [r["id"] for r in want]
    assert seen["refine"]["rows"] == seen["collect"] and seen["refine"]["redact"] is env.source.redact


class _Recorder:
    """The real ``lock`` module with ``acquire`` recording its ``wait_s``."""

    def __init__(self, real) -> None:
        self._real = real
        self.waits: list[int] = []

    def __getattr__(self, name):
        return getattr(self._real, name)

    def acquire(self, *a, wait_s, **kw):
        self.waits.append(wait_s)
        return self._real.acquire(*a, wait_s=wait_s, **kw)


def test_l2_source_lock_waits_at_most_twenty_seconds(mk, monkeypatch, session_db):
    """W060: a ``lock`` call (route ``source/lock``) waits for a running turn at most ``LOCK_WAIT_S`` (20 s), only
    with ``--wait`` (never longer than ``wait_timeout``); when the turn still runs it answers ``session_busy`` — the
    conductor repeats until its own deadline — and the source row is released, nothing stays locked."""
    env = mk(remote_driven=True)
    rec = _Recorder(env.source.ports.lock)
    monkeypatch.setattr(env.source.ports, "lock", rec)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    env.source.start_release(job, "test")
    waits = env.job(Options(refine="off", wait=True))
    env.source.lock(waits, env.ref, plan.revision)
    env.source.start_release(waits, "test")
    assert rec.waits[:2] == [0, 20]
    short = env.job(Options(refine="off", wait=True, wait_timeout_s=5))
    env.source.lock(short, env.ref, plan.revision)
    env.source.start_release(short, "test")
    assert rec.waits[-1] == 5

    turn = "pid=1:other-turn"
    assert session_db.try_acquire_session_turn_lease(env.sid, turn, ttl_seconds=120, patience_s=0)
    monkeypatch.setattr(local_source_module, "LOCK_WAIT_S", 1)
    waiting = env.job(Options(refine="off", wait=True))
    with pytest.raises(SessionBusy):
        env.source.lock(waiting, env.ref, plan.revision)
    assert rec.waits[-1] == 1
    assert env.ledger.source_state(waiting.job_id)["state"] == "released"
    session_db.release_session_turn_lease(env.sid, turn)


def test_l2_source_pack_item_documents_are_json(mk):
    """The pack document stays a ``ctxh.pack`` v1 JSON object whose ``final_plan_id`` is the plan id the source
    recomputed from the FinalPlan it packed (DESIGN §8.6, X02)."""
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    st = env.source.start_pack(job, env.final(plan))
    doc = env.doc(st.result["pack_id"])
    assert doc["kind"] == "ctxh.pack" and doc["v"] == 1 and doc["final_plan_id"] != "remote"
    packed = from_wire(FinalPlan, env.source._ctx(job.job_id)["final_plan"])
    assert packed.plan_id == doc["final_plan_id"]
    assert json.loads(json.dumps(doc)) == doc
    assert Path(env.extra).exists()


def test_l2_source_idle_suspend_gives_up_the_leases(mk, session_db):
    """DESIGN §7.3 through B's ``lock.suspend`` (no fallback any more): an idle pull source in ``locked`` gives up
    both leases and waits as ``suspended``; a turn can take the session at once; a second suspend is a no-op."""
    env = mk(remote_driven=True)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    env.source.suspend(job)
    assert env.ledger.source_state(job.job_id)["state"] == "suspended"
    assert env.ledger.job(job.job_id, Role.SOURCE)["state"] == "suspended"
    assert session_db.try_acquire_session_turn_lease(env.sid, "pid=2:turn", ttl_seconds=60, patience_s=0)
    session_db.release_session_turn_lease(env.sid, "pid=2:turn")
    env.source.suspend(job)
    assert env.ledger.source_state(job.job_id)["state"] == "suspended"
