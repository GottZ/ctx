"""LocalTarget: disk check at negotiation, asynchronous journaled commit with rollback and recovery
(cluster A; TESTPLAN §2.2, DESIGN §3.4, §8.6). Real SessionDB, real file system, real ledger."""

from __future__ import annotations

import stat
import threading
import time
from collections import namedtuple
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_handover.engine import continuation, plan, session_import
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.errors import ContinueRefused, InsufficientStorage, InvalidState
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.local_target import LocalTarget
from ctx_handover.engine.model import (
    HermesPaths,
    Include,
    ItemKind,
    ItemRole,
    Mode,
    OpState,
    Options,
    Origin,
    PackInfo,
    PlanItem,
    PreparedInfo,
    PrepareRequest,
    RefineReport,
    ResourcePlan,
    Role,
    SessionRef,
)
from ctx_handover.engine.ops import OpRunner
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.fakes.apis_fake import RecordingReporter, SimulatedCrash, make_facts
from ctx_handover.tests.unit.A import standins
from ctx_handover.tests.unit.A.standins import MemStore, Spawner, target_ports, tree_pack
from ctx_handover.tests.unit.A.standins import WorkspaceTxn as StandInTxn

Usage = namedtuple("Usage", "total used free")


WorkspaceTxn = StandInTxn
"""The stand-in transaction names its steps like E (``c1:<item_id>``; contract X11, W021: the target uses E's names
as they are)."""


def layer2_ports():
    """The stand-in ports with the B/D functions the target calls by contract (X02, X04) taken from the real
    modules; the stand-in ``plan_import`` gets the ``model``/``model_config`` fields of a real ``SegmentPlan``."""
    ports = target_ports(txn_cls=WorkspaceTxn)
    stand_in_import = ports.session_import.plan_import

    def plan_import(*args, **kwargs):
        res = stand_in_import(*args, **kwargs)
        res.segments = tuple(
            session_import.SegmentPlan(s.old_id, s.new_id, None, i == len(res.segments) - 1, "T", s.path)
            for i, s in enumerate(res.segments)
        )
        return res

    ports.session_import.plan_import = plan_import
    ports.session_import.map_model = session_import.map_model
    ports.session_import.offline_capable = session_import.offline_capable
    ports.plan = SimpleNamespace(
        plan_id=plan.plan_id, final_doc=plan.final_doc, instruction_files=plan.instruction_files
    )
    return ports


class FakeProc:
    pid = 4321

    def wait(self, timeout=None):
        return 0


class Launches:
    """``popen`` of the real ``continuation.Spawner``: records argv and cwd, starts nothing."""

    def __init__(self) -> None:
        self.calls: list[tuple[list, str | None]] = []

    def __call__(self, argv, **kwargs):
        self.calls.append((list(argv), kwargs.get("cwd")))
        return FakeProc()


class Env:
    def __init__(
        self,
        tmp_path,
        clock,
        session_db,
        make_session,
        user_home,
        hermes_home,
        *,
        mode=Mode.PULL,
        free=50 * 1024**3,
        settings=None,
        verified=True,
    ):
        self.clock = clock
        self.db = session_db
        self.data = hermes_home / "plugin-data" / "ctx_handover"
        self.ledger = Ledger(self.data / "ledger.sqlite3", clock=clock)
        self.store = MemStore()
        self.free = free
        self.ops = OpRunner(self.ledger, None)
        self.spawner = Spawner()
        self.options = Options()
        self.policy = PolicyConfig.from_options(
            self.options,
            home=user_home,
            hermes_home=hermes_home,
            hermes_root=hermes_home,
            plugin_dir=self.data,
            case_insensitive=False,
            target_os="linux",
        )
        self.hermes = HermesPaths(home=str(hermes_home), document_cache=str(hermes_home / "cache" / "documents"))
        self.mode = mode
        self.settings = settings or {}
        self.verified = verified
        self.target = self.make_target()
        self.job = JobContext(
            job_id="01HZX3T4RG0000000000000000",
            mode=mode,
            peer=None,
            options=self.options,
            epoch=1,
            reporter=RecordingReporter(),
            cancelled=lambda: False,
            created_at=clock(),
        )
        self.src_sid = make_session.create(title="T", cwd="/srv/app/sub")
        make_session.user(self.src_sid, "hello")
        make_session.assistant(self.src_sid, "hi")
        self.home = user_home

    def make_target(self, ops=None):
        from ctx_handover.tests.fakes.hostcompat_fake import FakeHost

        return LocalTarget(
            ledger=self.ledger,
            store=self.store,
            db_factory=lambda: self.db,
            hermes=self.hermes,
            host=FakeHost(user_home=self.policy.home, hermes_home=Path(self.hermes.home)).host_facts(),
            config={},
            ops=ops or self.ops,
            git=None,
            provider_usable=lambda name, cfg: True,
            continuation=self.spawner,
            redact=lambda t: t,
            source_peer_id="PEER1",
            peer_verified=self.verified,
            policy=self.policy,
            ports=layer2_ports(),
            clock=self.clock,
            data_dir=self.data,
            disk_usage=lambda path: Usage(10**13, 0, self.free),
            settings=self.settings,
            peer_name="server",
        )

    def wait(self, st, ops=None):
        return (ops or self.ops).wait(self.job.job_id, st.op_id, timeout=30)

    def plan(self, size=1000) -> ResourcePlan:
        item = PlanItem(
            item_id="w0",
            kind=ItemKind.FILE_TREE,
            source_path="/srv/app",
            role=ItemRole.PRIMARY,
            origin=Origin.CWD,
            include=Include.YES,
            reasons=("cwd",),
            size_estimate=size,
        )
        return ResourcePlan(
            job_id=self.job.job_id,
            revision=(1,),
            items=(item,),
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

    def prepared(self) -> PreparedInfo:
        t, job = self.target, self.job
        t.open_job(job)
        t.facts(job, make_facts("server"))
        final = t.negotiate(job, self.plan())
        info = standins.describe(self.db, SessionRef(id=self.src_sid))
        capsule = standins.export_capsule(
            self.db, info, job_id=job.job_id, store=self.store, source_meta={}, hermes=self.hermes
        )
        assert self.wait(t.start_stage_context(job, capsule)).state is OpState.DONE
        pack_id = tree_pack(self.store, "w0", {"a.txt": b"hello"}, final.plan_id)
        req = PrepareRequest(
            capsule=capsule,
            pack=PackInfo(pack_id=pack_id, size=1),
            final_plan_id=final.plan_id,
            source_facts=make_facts("server"),
            note_parts={"mode": "pull"},
        )
        st = self.wait(t.start_prepare(job, req))
        assert st.state is OpState.DONE, st.error
        self.dest = Path(self.home) / "hermes-handover" / "app"
        return PreparedInfo(**{**st.result, "dests": tuple(tuple(d) for d in st.result["dests"])})

    def hidden(self, sid) -> bool:
        row = self.db.get_session(sid)
        return row is None or bool(row.get("hidden"))

    def step_events(self):
        return [
            (e["kind"], e["data"]["step"], e["data"]["direction"])
            for e in self.ledger.events(self.job.job_id, Role.TARGET)
            if e["kind"] in ("intent", "done") and str(e["data"].get("step", "")).startswith("c")
        ]


@pytest.fixture
def env(tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host):
    WorkspaceTxn.fail = {}
    e = Env(tmp_path, clock, session_db, make_session, user_home, hermes_home)
    yield e
    WorkspaceTxn.fail = {}
    e.ledger.close()


def test_local_target_negotiate_need_bytes_before_transfer(
    tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host
):
    """★ ``insufficient_storage`` at NEGOTIATED with the numbers; nothing staged, nothing written [SK M3, BX M3].

    Mutant: the disk check in ``negotiate`` is skipped (the shortage would only show up in PREPARE)."""
    e = Env(tmp_path, clock, session_db, make_session, user_home, hermes_home, free=10_000)
    e.target.open_job(e.job)
    e.target.facts(e.job, make_facts("server"))
    with pytest.raises(InsufficientStorage) as exc:
        e.target.negotiate(e.job, e.plan(size=5_000_000))
    d = exc.value.details
    assert d["need_bytes"] >= 5_000_000 and d["free_bytes"] == 10_000 and d["path"]
    assert e.store.data == {}
    assert e.ledger.job(e.job.job_id, Role.TARGET)["state"] == "open"
    e.free = 50 * 1024**3
    final = e.target.negotiate(e.job, e.plan(size=5_000_000))
    assert final.need_bytes >= 5_000_000 and final.plan_id
    assert e.ledger.job(e.job.job_id, Role.TARGET)["state"] == "negotiated"
    e.ledger.close()


def test_target_prepare_imports_hidden(env):
    prepared = env.prepared()
    assert env.hidden(prepared.tip_id)
    assert not env.dest.exists()
    partial = env.data / "staging" / env.job.job_id / "partial" / "w0"
    assert (partial / "a.txt").read_bytes() == b"hello"
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "prepared"


def test_sec_data_dir_permissions(env):
    """SEC 6.1: data dir and staging 0700 before the first file, partials 0700 until commit, reports and the
    ledger 0600."""
    env.prepared()
    staging = env.data / "staging" / env.job.job_id
    for path in (env.data, env.data / "staging", staging, staging / "partial" / "w0", env.data / "reports"):
        assert stat.S_IMODE(path.stat().st_mode) == 0o700, path
    for path in [env.data / "ledger.sqlite3", *(env.data / "reports").iterdir()]:
        assert stat.S_IMODE(path.stat().st_mode) == 0o600, path


def test_target_commit_async_over_60s(env):
    """★ A commit that takes 90 s completes through polling: ``start_commit`` returns at once and no poll blocks
    [SK H5, I7].

    Mutant: ``start_commit`` runs the commit body synchronously before returning."""
    prepared = env.prepared()
    gate = threading.Event()
    entered = threading.Event()

    class SlowTxn(WorkspaceTxn):
        def commit_steps(self):
            steps = super().commit_steps()

            def slow(fwd=steps[0][1]):
                entered.set()
                assert gate.wait(30)
                fwd()

            return [(steps[0][0], slow, steps[0][2])]

    env.target.ports.materialize.WorkspaceTxn = SlowTxn
    result = {}

    def call():
        t0 = time.monotonic()
        result["st"] = env.target.start_commit(env.job)
        result["dt"] = time.monotonic() - t0

    caller = threading.Thread(target=call)
    caller.start()
    caller.join(5)
    assert not caller.is_alive(), "start_commit blocked until the commit finished"
    assert result["st"].state is OpState.RUNNING and result["dt"] < 2
    assert entered.wait(5)
    for _ in range(10):  # polls while the commit runs; simulated 90 s
        t0 = time.monotonic()
        st = env.target.op_status(env.job, result["st"].op_id)
        assert st.state is OpState.RUNNING and time.monotonic() - t0 < 1
        env.clock.advance(9)
    assert env.target.status(env.job).state == "committing"
    gate.set()
    done = env.wait(result["st"])
    assert done.state is OpState.DONE and done.result["session_id"] == prepared.tip_id
    assert not env.hidden(prepared.tip_id) and (env.dest / "a.txt").read_bytes() == b"hello"
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "committed"


def test_commit_rollback_fresh_rename_and_unhide(env):
    """★ Failing c3 ⇒ c2 and c1 undone in reverse, session hidden, job ``aborted`` [SK H5].

    Mutant: the rollback skips the inverse of the c1 steps."""
    prepared = env.prepared()
    WorkspaceTxn.fail["c3:hermes_files"] = OSError("disk full")
    st = env.wait(env.target.start_commit(env.job))
    assert st.state is OpState.FAILED and st.error["code"] == "import_failed"
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "aborted"
    assert not env.dest.exists(), "fresh rename was not undone"
    assert not (env.dest / "sub").exists()
    assert env.hidden(prepared.tip_id)
    ev = env.step_events()
    fwd_done = [s for k, s, d in ev if k == "done" and d == "forward"]
    undo = [s for k, s, d in ev if k == "done" and d == "rollback"]
    assert fwd_done == ["c1:w0", "c2:cwd"]
    assert undo == ["c2:cwd", "c1:w0"]
    assert env.ledger.arrival_for(prepared.tip_id) is None
    with pytest.raises(InvalidState):
        env.target.start_commit(env.job)  # a rolled-back commit never runs again


def test_commit_unhide_is_last_step(env):
    """★ Journal order c1 … c6, then ``committed``; a crash before c6 leaves the session hidden, recovery
    continues forward [SK H5, I3].

    Mutant: unhide runs as the first commit step instead of the last."""
    prepared = env.prepared()
    real = env.ledger.arrival_put
    crashed = {"n": 0}

    def crash_once(**kw):
        if not crashed["n"]:
            crashed["n"] += 1
            raise SimulatedCrash("process died in c5")
        return real(**kw)

    env.ledger.arrival_put = crash_once
    st = env.target.start_commit(env.job)
    env.ops.join_all(10)
    assert env.ledger.op_by_id(st.op_id)["state"] == "running"  # the process "died"
    assert env.hidden(prepared.tip_id), "session visible before the commit point"
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "committing"

    env.clock.advance(61)
    ops2 = OpRunner(env.ledger, None)
    t2 = env.make_target(ops=ops2)
    st2 = t2.recover(env.job)
    done = ops2.wait(env.job.job_id, st2.op_id)
    assert done.state is OpState.DONE, done.error
    order = [s for k, s, d in env.step_events() if k == "done" and d == "forward"]
    assert order == ["c1:w0", "c2:cwd", "c3:hermes_files", "c4:side_state", "c5:arrival", "c6:unhide"]
    states = [e["state"] for e in env.ledger.events(env.job.job_id, Role.TARGET) if e["kind"] == "transition"]
    assert states[-1] == "committed"
    assert not env.hidden(prepared.tip_id)
    assert env.ledger.arrival_for(prepared.tip_id) is not None
    assert env.ledger.map_lookup("PEER1", "/srv/app").local_path == str(env.dest)


def test_commit_recovery_continues_journaled_direction(env):
    """A crash in the middle of a rollback resumes the rollback (never the forward direction) [SK H5]."""
    prepared = env.prepared()
    WorkspaceTxn.fail["c3:hermes_files"] = OSError("boom")
    WorkspaceTxn.fail["undo:c1:w0"] = SimulatedCrash("died while undoing c1")
    st = env.target.start_commit(env.job)
    env.ops.join_all(10)
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "committing"
    assert env.dest.exists()  # c1 not yet undone
    WorkspaceTxn.fail.clear()
    env.clock.advance(61)
    ops2 = OpRunner(env.ledger, None)
    st2 = env.make_target(ops=ops2).recover(env.job)
    done = ops2.wait(env.job.job_id, st2.op_id)
    assert done.state is OpState.FAILED and done.error["code"] == "import_failed"
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "aborted"
    assert not env.dest.exists()
    assert "c3:hermes_files" not in [s for k, s, d in env.step_events() if k == "done" and d == "forward"]
    assert env.hidden(prepared.tip_id)
    assert st.op_id == st2.op_id


def test_target_abort_semantics(env):
    prepared = env.prepared()
    env.target.abort(env.job, "cancelled")
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "aborted"
    assert env.db.get_session(prepared.tip_id) is None or env.hidden(prepared.tip_id)
    assert not (env.data / "staging" / env.job.job_id).exists()
    env.target.abort(env.job, "again")  # idempotent


def test_target_abort_refused_while_committing(env):
    env.prepared()
    gate = threading.Event()

    class Hold(WorkspaceTxn):
        def commit_steps(self):
            steps = super().commit_steps()
            return [(steps[0][0], lambda f=steps[0][1]: (gate.wait(10), f()), steps[0][2])]

    env.target.ports.materialize.WorkspaceTxn = Hold
    st = env.target.start_commit(env.job)
    with pytest.raises(InvalidState) as exc:
        env.target.abort(env.job, "user")
    assert exc.value.details["state"] == "committing"
    gate.set()
    assert env.wait(st).state is OpState.DONE


def test_continue_rules(tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host):
    """Remote continuation (contract X21): the real ``Spawner`` decides — off without ``allow_remote_continue``,
    refused for an unverified peer — and the child runs in the cwd of the arrived session row (R10, R11, R22)."""
    e = Env(tmp_path, clock, session_db, make_session, user_home, hermes_home, mode=Mode.PUSH)
    e.target.remote_driven = True  # the wire instance the registry's factory builds (R6-A: not the mode)
    allow = {"on": False}
    launches = Launches()
    e.target.continuation = continuation.Spawner(
        log_dir=tmp_path / "logs",
        profile=None,
        approvals_off=lambda: False,
        allow_remote=lambda: allow["on"],
        which=lambda name: "/opt/hermes/bin/hermes",
        popen=launches,
        environ={},
    )
    prepared = e.prepared()
    assert e.wait(e.target.start_commit(e.job)).state is OpState.DONE
    with pytest.raises(ContinueRefused) as exc:
        e.target.continue_session(e.job, "go on")
    assert exc.value.details["reason"] == "not_allowed"
    allow["on"] = True
    e.target.peer_verified = False
    with pytest.raises(ContinueRefused) as exc:
        e.target.continue_session(e.job, "go on")
    assert exc.value.details["reason"] == "unverified"
    e.target.peer_verified = True
    res = e.target.continue_session(e.job, "go on")
    row_cwd = e.db.get_session(prepared.tip_id)["cwd"]
    assert res.started and len(launches.calls) == 1
    argv, cwd = launches.calls[0]
    assert argv[-3:-1] == ["--resume", prepared.tip_id] and "requested by server" in argv[-1]
    assert cwd == row_cwd
    assert e.target.continue_session(e.job, "again").pid == res.pid  # recorded once
    assert len(launches.calls) == 1
    e.ledger.close()


def test_target_reset_and_delta(env, make_session):
    prepared = env.prepared()
    make_session.user(env.src_sid, "late")
    info = standins.describe(env.db, SessionRef(id=env.src_sid))
    delta = standins.export_delta(env.db, info, None, [{"role": "user", "content": "late"}], store=env.store)
    st = env.wait(env.target.start_apply_delta(env.job, delta))
    assert st.state is OpState.DONE and st.result["h_all"] == delta.h_all
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "prepared"
    st = env.wait(env.target.start_reset_session(env.job))
    assert st.state is OpState.DONE
    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "context_staged"
    assert env.db.get_session(prepared.tip_id) is None
