"""``LocalTarget`` against the real B/D/E/F modules (layer 2): the map lookup bound to the source peer (X10),
destinations of the counterpart inside ``allow_roots`` (X15), extras under ``_handover/<job6>`` (W102), the model
change from B (X04), session files placed by kind + basename only (X17), the note's instruction files read on this
host (W082), the ctx record (X22) and the tools the source saw (W084)."""

from __future__ import annotations

import dataclasses
import hashlib
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_handover.engine import ctxrecords, gitops, hostcompat, jobs, materialize, session_import
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.ids import short, ulid
from ctx_handover.engine.model import (
    DestMode,
    GitInfo,
    Include,
    ItemKind,
    ItemRole,
    MapRow,
    Mode,
    OpState,
    Options,
    Origin,
    PlanItem,
    RefineReport,
    ResourcePlan,
)
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes.apis_fake import RecordingReporter, make_facts
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.A.test_local_target import Env, WorkspaceTxn

LAPTOP = make_facts("laptop")  # source facts: home /laptop/home, provider p-laptop


@pytest.fixture
def server(tmp_path):
    require_upstream()
    jobs._reset_for_tests()  # per-process profile caches of the composition root
    home = Home(tmp_path, "server")
    yield home
    jobs._reset_for_tests()


def real_target(home: Home, *, peer: str | None = "PEER1"):
    """``LocalTarget`` exactly as the composition root builds it, with the job's source peer bound."""
    with home.active():
        _source, target = jobs.make_local_apis(None, calling_session=None, session_key=None)
    target.source_peer_id = peer
    target.peer_name = "laptop" if peer else None
    return target


def job_of(mode: Mode, options: Options | None = None) -> JobContext:
    return JobContext(
        job_id=ulid(),
        mode=mode,
        peer=None,
        options=options or Options(),
        epoch=1,
        reporter=RecordingReporter(),
        cancelled=lambda: False,
        created_at=0.0,
    )


def resource_plan(job: JobContext, *items: PlanItem) -> ResourcePlan:
    return ResourcePlan(
        job_id=job.job_id,
        revision=(1,),
        items=tuple(items),
        not_transferred=(),
        processes=(),
        delegations=(),
        cron=(),
        refine=RefineReport(used="off"),
        tools_needed=(),
        env_vars_needed=(),
        notes_for_target="",
        approx_tokens=100,
    )


def item(item_id: str, kind: ItemKind, path: str, **kw) -> PlanItem:
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=path,
        role=kw.pop("role", ItemRole.PRIMARY if kind in (ItemKind.GIT_REPO, ItemKind.FILE_TREE) else ItemRole.EXTRA),
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=100,
        **kw,
    )


def negotiate(home: Home, target, job: JobContext, *items: PlanItem):
    with home.active():
        target.open_job(job)
        target.facts(job, LAPTOP)
        return target.negotiate(job, resource_plan(job, *items))


# -- X10 ---------------------------------------------------------------------------------------------------------
def _handed_out_repo(home: Home, peer: str, remote: str) -> tuple[Path, str]:
    """A clean repository on this host that it handed to ``peer`` earlier: the ``out`` row with the outgoing git
    state as ``materialize.map_state_of`` writes it (contract X09)."""
    r = RepoBuilder.init(home.home / "work" / "app")
    r.write("main.py", "print('server')\n")
    r.add_all()
    r.commit("first")
    root = r.out("rev-list", "--max-parents=0", "HEAD")
    tmp = home.root / "wt-tmp"
    tmp.mkdir()
    st = gitops.worktree_state(gitops.Git(), str(r.root), (), tmp)
    state = materialize.map_state_of(st)  # E's one producer of the flat form (contract X09)
    home.ledger().map_put(
        MapRow(
            local_path=str(r.root),
            peer_id=peer,
            remote_path=remote,
            direction="out",
            repo_identity=root,
            state=state,
            last_job_id="01J0000000000000000000000X",
        )
    )
    return r.root, root


def _git_item(path: str, root_commit: str) -> PlanItem:
    info = GitInfo(
        head=None,
        branch="main",
        object_format="sha1",
        shallow=False,
        dirty=False,
        in_progress=None,
        lfs=False,
        submodules=(),
        remotes=(),
        root_commits=(root_commit,),
        untracked_bytes=0,
        untracked_count=0,
        denied_tracked=(),
    )
    return item("w0", ItemKind.GIT_REPO, path, git=info)


def test_return_trip_finds_the_out_row(server):
    """★ X10: the target looks up ``(source peer, source path, direction)``; the repository it handed to that peer
    comes back to its original place in return mode. Mutant (red proof): ``pathmap`` gets the unbound
    ``ledger.map_lookup`` of layer 1 (``peer_id`` ← path, ``remote_path`` ← direction) — the row is never found."""
    remote = "/laptop/home/hermes-handover/app"
    origin, root = _handed_out_repo(server, "PEER1", remote)
    target = real_target(server)
    final = negotiate(server, target, job_of(Mode.PUSH), _git_item(remote, root))
    (d,) = final.decisions
    assert (d.dest, d.reason, d.mode, d.conflict) == (str(origin), "return-mapping", DestMode.RETURN, None)


def test_return_lookup_is_bound_to_the_source_peer(server):
    """The out row of another peer never maps this job's paths (X10 binding)."""
    remote = "/laptop/home/hermes-handover/app"
    origin, root = _handed_out_repo(server, "PEER2", remote)
    target = real_target(server, peer="PEER1")
    final = negotiate(server, target, job_of(Mode.PUSH), _git_item(remote, root))
    (d,) = final.decisions
    assert d.reason != "return-mapping" and d.dest != str(origin)


# -- X15 / R19 ---------------------------------------------------------------------------------------------------
def test_counterpart_destination_stays_inside_allow_roots(server):
    """★ X15: a destination typed on the counterpart (push: ``participant_options`` carries ``dest``) goes into
    ``pathmap``/``assess`` with ``local_choice=False``: outside ``allow_roots`` it is not used; the same path typed on
    this host (pull) is. Mutant (red proof): ``local_choice`` always True — the push lands outside."""
    outside = str(server.root / "elsewhere" / "proj")
    tree = item("w0", ItemKind.FILE_TREE, "/laptop/home/proj")
    remote = negotiate(server, real_target(server), job_of(Mode.PUSH, Options(dest=outside)), tree)
    (d,) = remote.decisions
    assert d.dest != outside and d.reason != "user"
    assert Path(d.dest).is_relative_to(server.home / "hermes-handover")
    local = negotiate(server, real_target(server), job_of(Mode.PULL, Options(dest=outside)), tree)
    (d,) = local.decisions
    assert (d.dest, d.reason) == (outside, "user")


def test_counterpart_destination_never_in_the_denylist(server):
    """R19: a counterpart's destination in the target denylist (``~/.ssh``) is refused even inside the home."""
    denied = str(server.home / ".ssh" / "proj")
    tree = item("w0", ItemKind.FILE_TREE, "/laptop/home/proj")
    (d,) = negotiate(server, real_target(server), job_of(Mode.PUSH, Options(dest=denied)), tree).decisions
    assert not Path(d.dest).is_relative_to(server.home / ".ssh")
    if d.conflict is not None:
        assert not Path(d.conflict.fallback_dest).is_relative_to(server.home / ".ssh")


# -- W102 --------------------------------------------------------------------------------------------------------
def test_counterpart_extra_lands_under_the_job_directory(server):
    """W102: an extra of a remote-driven job lands at ``<workspace_root>/_handover/<job6>/<home-relative>`` — the
    conflict "extras land at their path only through a local map" falls back to exactly that place. Mutant
    (red proof): the generic ``fallback_dest`` (``…-handover-<time>``)."""
    job = job_of(Mode.PUSH)
    extra = item("x0", ItemKind.EXTRA_FILE, "/laptop/home/notes/todo.txt")
    target = real_target(server)
    (d,) = negotiate(server, target, job, extra).decisions
    root = target._facts(job)[1].workspace_root  # this host's workspace root as its facts spell it
    want = str(Path(root) / "_handover" / short(job.job_id) / "notes" / "todo.txt")
    assert d.dest == want
    assert d.conflict is None or d.conflict.fallback_dest == want


# -- X04 ---------------------------------------------------------------------------------------------------------
def test_final_plan_model_change_is_b_map_model(server):
    """X04: ``FinalPlan.model_change`` is B's ``map_model`` result unchanged (the source reports a provider this
    host does not serve ⇒ the target default). Mutant (red proof): the layer-1 dict built in ``LocalTarget``."""
    server.write_settings({"refine": "off"}, model={"default": "local-model", "provider": "ollama"})
    job = job_of(Mode.PUSH)
    final = negotiate(server, real_target(server), job, item("w0", ItemKind.FILE_TREE, "/laptop/home/proj"))
    with server.active():
        _fields, want = session_import.map_model(
            {"model": "m-laptop", "model_config": {"provider": "p-laptop"}},
            job.options.model_mapping,
            None,
            hostcompat.load_config(),
            hostcompat.provider_usable,
        )
    assert want is not None and final.model_change == want


# -- X17 ---------------------------------------------------------------------------------------------------------
def test_session_files_placed_by_kind_and_basename_only(server):
    """★ X17 (hardening #7): the kind comes from the plan item (the pack item must agree), the name is the basename;
    an item without a kind, with contradicting kinds or an unknown kind is not placed — never guessed. Mutant
    (red proof): the layer-1 guess from ``rel`` (default ``spillover``)."""
    target = real_target(server)
    store = target.store
    data = {k: store.put_bytes(k.encode() * 3, "application/octet-stream") for k in "abcde"}
    items = {
        "h1": item("h1", ItemKind.HERMES_FILE, "/x/h1", hermes_kind="hook_outputs"),
        "h2": item("h2", ItemKind.HERMES_FILE, "/x/h2"),
        "h3": item("h3", ItemKind.HERMES_FILE, "/x/h3", hermes_kind="spillover"),
        "h4": item("h4", ItemKind.HERMES_FILE, "/x/h4", hermes_kind="native_inputs"),
        "h5": item("h5", ItemKind.HERMES_FILE, "/x/h5"),
    }
    pack = [
        {"item_id": "h1", "object": data["a"].object_id, "hermes_kind": "hook_outputs", "rel": "../../etc/run.log"},
        {"item_id": "h2", "object": data["b"].object_id, "rel": "cache/spillover/sp.txt"},
        {"item_id": "h3", "object": data["c"].object_id, "hermes_kind": "native_inputs", "rel": "x.png"},
        {"item_id": "h4", "object": data["d"].object_id, "hermes_kind": "native_inputs", "rel": "in\\img.png"},
        {"item_id": "h5", "object": data["e"].object_id, "hermes_kind": "config", "rel": "config.yaml"},
    ]
    with server.active():
        placed, skipped = target._hermes_placement(pack, items, "20260101_000000_abcdef")  # the unit under test
    where = {e["pack_item"]["item_id"]: Path(e["target"]) for e in placed}
    hh = Path(target.hermes.home)
    assert where == {
        "h1": hh / "hook_outputs" / "20260101_000000_abcdef" / "run.log",
        "h4": Path(target.hermes.document_cache) / "native-inputs" / hashlib.sha256(b"ddd").hexdigest() / "img.png",
    }
    assert len(skipped) == 3 and all("not placed" in s for s in skipped)


# -- W082 / X22 / W084 / X04 (ack) -------------------------------------------------------------------------------
@pytest.fixture
def env(tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host):
    WorkspaceTxn.fail = {}
    e = Env(tmp_path, clock, session_db, make_session, user_home, hermes_home)
    yield e
    WorkspaceTxn.fail = {}
    e.ledger.close()


def _capture(ports, module: str, name: str) -> list:
    calls: list = []
    original = getattr(getattr(ports, module), name)

    def wrapper(*args, **kwargs):
        calls.append((args, kwargs))
        return original(*args, **kwargs)

    setattr(getattr(ports, module), name, wrapper)
    return calls


def test_note_lists_instruction_files_read_on_this_host(env):
    """W082: ``note.compose`` gets ``peer_name`` and the instruction files ``plan.instruction_files`` finds HERE
    (new or different at the destination); a list sent by the counterpart in ``note_parts`` is replaced."""
    sha = hashlib.sha256(b"be careful\n").hexdigest()
    plan = env.plan()
    plan = dataclasses.replace(
        plan, items=(dataclasses.replace(plan.items[0], instruction_files=(("AGENTS.md", sha),)),)
    )
    env.plan = lambda size=1000: plan
    calls = _capture(env.target.ports, "note", "compose")
    env.prepared()
    kw = calls[-1][1]
    assert kw["peer_name"] == "server"
    assert kw["note_parts"]["instruction_files"] == [
        {"item_id": "w0", "rel": "AGENTS.md", "status": "new", "sha256": sha}
    ]
    assert kw["origin"] == "verified paired device"


def test_note_ignores_instruction_files_of_the_counterpart(env, monkeypatch):
    """SEC 4.3: what the conductor lists as instruction files is never rendered as a fact of this host."""
    calls = _capture(env.target.ports, "note", "compose")
    original = env.target.start_prepare

    def start_prepare(job, req):
        evil = {**req.note_parts, "instruction_files": [{"item_id": "w0", "rel": "EVIL.md", "status": "new"}]}
        return original(job, dataclasses.replace(req, note_parts=evil))

    monkeypatch.setattr(env.target, "start_prepare", start_prepare)
    env.prepared()
    assert calls[-1][1]["note_parts"]["instruction_files"] == []


def _ctx_config() -> dict:
    return {"memory": {"provider": "ctx_checkpoint"}}


def test_ctx_record_carries_every_build_record_field(env):
    """★ X22: with ctx_checkpoint configured the arrival record carries all fields of F ``build_record`` — source
    root/tip are the SOURCE session ids from the capsule — and builds. Mutant (red proof): the layer-1 values
    (instance name as source root, capsule id as source session)."""
    env.target.config = _ctx_config()
    env.target.ports.ctxrecords = SimpleNamespace(server_for=ctxrecords.server_for, enqueue=lambda *a, **k: None)
    calls = _capture(env.target.ports, "ctxrecords", "enqueue")
    prepared = env.prepared()
    assert env.wait(env.target.start_commit(env.job)).state is OpState.DONE
    ((args, _kw),) = calls
    kind, data = args[1], args[2]
    assert kind == "arrival"
    assert set(data) == {
        "job_id", "source_root", "target_root", "source_session", "target_session", "peer", "local_name", "title",
        "at", "path_map", "model_change", "plugin_version",
    }  # fmt: skip
    root = env.db.get_session(env.src_sid)
    assert (data["source_root"], data["source_session"]) == (env.src_sid, env.src_sid) and root is not None
    assert (data["target_root"], data["target_session"]) == (prepared.root_id, prepared.tip_id)
    assert data["peer"] == "server"
    record = ctxrecords.build_record(kind, data, redact=lambda t: t)
    assert env.src_sid in record["content"] and record["metadata"]["source_root"] == env.src_sid


def test_ctx_record_only_with_a_ctx_server(env):
    """X22/W116: without ctx_checkpoint (``server_for`` None) nothing is queued."""
    env.target.config = {}
    env.target.ports.ctxrecords = SimpleNamespace(server_for=ctxrecords.server_for, enqueue=lambda *a, **k: None)
    calls = _capture(env.target.ports, "ctxrecords", "enqueue")
    env.prepared()
    assert env.wait(env.target.start_commit(env.job)).state is OpState.DONE
    assert calls == []


def test_target_facts_look_up_the_tools_the_source_saw(env):
    """W084: the target probes the tool names from ``source_facts.tools_seen`` (lookup only)."""
    calls = _capture(env.target.ports, "envfacts", "probe")
    sf = dataclasses.replace(make_facts("laptop"), tools_seen={"rg": "/usr/bin/rg", "jq": None})
    env.target.open_job(env.job)
    env.target.facts(env.job, sf)
    assert list(calls[-1][1]["tools_seen"]) == ["jq", "rg"]


def test_commit_ack_offline_from_b(env):
    """X04: ``CommitAck.offline_capable`` is B's ``offline_capable`` of the route the import wrote — here the
    target's main provider is local, whatever the source facts said."""
    env.target.config = {"model": {"default": "qwen", "provider": "ollama"}}
    env.prepared()
    st = env.wait(env.target.start_commit(env.job))
    assert st.state is OpState.DONE
    assert st.result["offline_capable"] is session_import.offline_capable({}, env.target.config) is True
