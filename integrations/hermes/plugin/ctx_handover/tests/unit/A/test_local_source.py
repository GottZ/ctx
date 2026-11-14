"""LocalSource: release rules of a remote-driven source, suggestions, lock/prepare/retire/release order
(cluster A; TESTPLAN §2.2, DESIGN §7, §8.7, R18). Real SessionDB and ledger; B/D/E functions are stand-ins."""

from __future__ import annotations

import os
from dataclasses import replace
from pathlib import Path

import pytest

from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.errors import LeaseLost, PolicyDenied, SessionBusy
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.local_source import LocalSource
from ctx_handover.engine.model import (
    CommitAck,
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
    PathMapEntry,
    Peer,
    PlanItem,
    PreparedInfo,
    PrepareOutcome,
    ResourcePlan,
    Right,
    Role,
    SessionRef,
    TargetDecision,
)
from ctx_handover.engine.ops import OpRunner
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.fakes.apis_fake import RecordingReporter, make_facts
from ctx_handover.tests.unit.A import standins


def _item(item_id, path, kind=ItemKind.EXTRA_FILE, role=ItemRole.EXTRA):
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=str(path),
        role=role,
        origin=Origin.TRANSCRIPT,
        include=Include.YES,
        reasons=("read",),
        size_estimate=10,
    )


class SrcEnv:
    def __init__(self, tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host, *, remote_driven):
        self.db = session_db
        self.clock = clock
        self.data = hermes_home / "plugin-data" / "ctx_handover"
        self.ledger = Ledger(self.data / "ledger.sqlite3", clock=clock)
        self.store = standins.MemStore()
        self.ops = OpRunner(self.ledger, None, inline=True)
        work = user_home / "proj"
        work.mkdir()
        (work / "notes.txt").write_text("my notes\n")
        (work / "todo.md").write_text("- a\n")
        self.items = (_item("x1", work / "notes.txt"), _item("x2", work / "todo.md"))
        self.lock = standins.LockStandIn()
        self.plan_port = standins.PlanStandIn(self.items)
        self.ports = standins.source_ports(self.items, lock=self.lock, plan=self.plan_port)
        self.policy = PolicyConfig.from_options(
            Options(),
            home=user_home,
            hermes_home=hermes_home,
            hermes_root=hermes_home,
            plugin_dir=self.data,
            case_insensitive=False,
            target_os="linux",
        )
        self.sid = make_session.create(title="T", cwd=str(work))
        make_session.user(self.sid, "please read notes.txt")
        make_session.assistant(self.sid, "done")
        self.peer = Peer(
            name="laptop", peer_id="PL", direction="inbound", url=None, pubkey="pk", rights=frozenset({Right.PULL})
        )
        self.source = LocalSource(
            ledger=self.ledger,
            store=self.store,
            db_factory=lambda: self.db,
            hermes=HermesPaths(home=str(hermes_home), document_cache=str(hermes_home / "cache")),
            host=fake_host.host_facts(),
            config={},
            ops=self.ops,
            git=None,
            calling_session=None,
            session_key=None,
            side_question=None,
            llm=None,
            read_secret=lambda n: None,
            redact=lambda t: t,
            state_db=hermes_home / "state.db",
            remote_driven=remote_driven,
            policy=self.policy,
            ports=self.ports,
            clock=clock,
            data_dir=self.data,
        )
        self.job = JobContext(
            job_id="01HZX3S0RC0000000000000000",
            mode=Mode.PULL if remote_driven else Mode.PUSH,
            peer=self.peer if remote_driven else None,
            options=Options(),
            epoch=1,
            reporter=RecordingReporter(),
            cancelled=lambda: False,
            created_at=clock(),
        )
        self.ref = SessionRef(id=self.sid)
        if remote_driven:
            self.ledger.create_job(self.job.job_id, Role.SOURCE, Mode.PULL, "PL", {"role": "source"})

    def plan(self) -> ResourcePlan:
        st = self.source.start_plan(self.job, make_facts("laptop"), self.ref)
        assert st.state is OpState.DONE, st.error
        from ctx_handover.engine.jsonutil import from_wire

        return from_wire(ResourcePlan, st.result)

    def final(self, plan: ResourcePlan, items=None) -> FinalPlan:
        plan = replace(plan, items=tuple(items or plan.items))
        decisions = tuple(
            TargetDecision(
                item_id=i.item_id, dest="/laptop/" + Path(i.source_path).name, mode=DestMode.FRESH, reason="test"
            )
            for i in plan.items
        )
        return FinalPlan(
            plan_id="remote-claimed-id", plan=plan, decisions=decisions, need_bytes=0, free_bytes={}, model_change=None
        )


@pytest.fixture
def mk(tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host):
    made = []

    def make(remote_driven=True):
        e = SrcEnv(
            tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host, remote_driven=remote_driven
        )
        made.append(e)
        return e

    yield make
    for e in made:
        e.ledger.close()


def test_local_source_remote_driven_restricts_plan(mk):
    """★ A remote final plan with an item the source did not plan ⇒ ``policy_denied``; a subset is packed and
    "pulled by <peer>" is logged on the source (R18, SEC 5.8).

    Mutant: ``start_pack`` skips ``restrict_to_releasable`` for remote-driven jobs."""
    e = mk(remote_driven=True)
    plan = e.plan()
    evil = _item("x9", "/etc/passwd")
    with pytest.raises(PolicyDenied) as exc:  # rejected before any work starts (route #15 answers 422)
        e.source.start_pack(e.job, e.final(plan, items=[*plan.items, evil]))
    assert exc.value.details["items"] == ["x9"]
    assert e.ledger.pull_log(5) == [] and e.ledger.ops_for_job(e.job.job_id) == [
        r for r in e.ledger.ops_for_job(e.job.job_id) if r["op"] != "pack"
    ]

    subset = e.final(plan, items=[plan.items[0]])
    st = e.source.start_pack(e.job, subset)
    assert st.state is OpState.DONE, st.error
    log = e.ledger.pull_log(5)
    assert log[0]["peer_id"] == "PL" and log[0]["items"] == ["x1"]
    pack = e.store.doc(st.result["pack_id"])
    assert [i["item_id"] for i in pack["items"]] == ["x1"]
    assert pack["final_plan_id"] != "remote-claimed-id"  # the source recomputes the plan id (R9)


def test_local_source_merges_suggestions_in_pull(mk):
    """Agent suggestions stored for the session are merged by the source itself [BX H4]."""
    e = mk(remote_driven=True)
    e.ledger.suggestion_put(e.sid, {"path": "/home/u/extra.txt", "note": "needed"})
    e.plan()
    assert e.plan_port.build_calls[-1]["suggestions"][0]["path"] == "/home/u/extra.txt"


def test_local_source_lock_busy_force_and_revision(mk, make_session):
    e = mk(remote_driven=False)
    plan = e.plan()
    e.lock.busy = standins.Busy(busy=True, reasons=("queued prompt",))
    with pytest.raises(SessionBusy):
        e.source.lock(e.job, e.ref, plan.revision)
    assert e.ledger.source_state(e.job.job_id) is None
    e.lock.busy = standins.Busy()
    make_session.user(e.sid, "typed after planning")
    info = e.source.lock(e.job, e.ref, plan.revision)
    assert info.revision_changed
    assert e.ledger.source_state(e.job.job_id)["state"] == "locked"
    assert e.ledger.job(e.job.job_id, Role.SOURCE)["state"] == "locked"
    info2 = e.source.lock(e.job, e.ref, info.revision)  # idempotent re-lock
    assert not info2.revision_changed


def test_local_source_heartbeat_lease_lost(mk):
    e = mk(remote_driven=False)
    plan = e.plan()
    e.source.lock(e.job, e.ref, plan.revision)
    e.source.heartbeat(e.job)
    e.lock.refresh_ok = False
    with pytest.raises(LeaseLost):
        e.source.heartbeat(e.job)


def _prepared(capsule) -> PreparedInfo:
    return PreparedInfo(
        tip_id="n",
        root_id="n",
        title="T",
        h_all=capsule["h_all"],
        h_rows=capsule["h_rows"],
        h_model=capsule["h_model"],
        import_path="A",
        dests=(),
        warnings=(),
        source_revision=(),
    )


def test_local_source_prepare_outcomes_and_retire_order(mk, make_session):
    """Delta on appended rows; prepare applies the primary lock only with matching hashes; retire runs after
    the ACK and writes the ``out`` rows before the leases go (R1, R4)."""
    e = mk(remote_driven=False)
    plan = e.plan()
    e.source.lock(e.job, e.ref, plan.revision)
    capsule = e.source.start_export(e.job).result
    pack = e.source.start_pack(e.job, e.final(plan)).result
    assert pack["pack_id"]
    make_session.user(e.sid, "late message")
    st = e.source.start_prepare(e.job, _prepared(capsule))
    assert st.result["outcome"] == PrepareOutcome.DELTA and st.result["delta"]["rows"] == 1
    delta = st.result["delta"]
    bad = replace(_prepared(capsule), h_all="0" * 64)
    st = e.source.start_prepare(e.job, bad)
    assert st.state is OpState.FAILED and st.error["code"] == "verify_failed"
    assert e.ledger.source_state(e.job.job_id)["state"] == "locked"
    good = replace(_prepared(capsule), h_all=delta["h_all"], h_rows=delta["h_rows"], h_model=delta["h_model"])
    st = e.source.start_prepare(e.job, good)
    assert st.result["outcome"] == PrepareOutcome.OK
    assert e.ledger.source_state(e.job.job_id)["state"] == "prepared"
    ack = CommitAck(
        session_id="n",
        root_id="n",
        title="T",
        dests=(("x1", "/laptop/notes.txt", DestMode.FRESH),),
        path_map=(PathMapEntry("/a", "/b", "workspace"),),
        model="m",
        offline_capable=True,
        warnings=(),
        committed_at=1.0,
    )
    st = e.source.start_retire(e.job, ack)
    assert st.state is OpState.DONE and st.result["state"] == "retired"
    assert e.lock.calls[-2:] == ["retire", "release_leases"]
    assert e.ledger.outbox_due(e.clock() + 1)[0]["kind"] == "departure"
    st = e.source.start_release(e.job, "too late")
    assert st.state is OpState.FAILED and st.error["code"] == "invalid_state"
    assert e.ledger.source_state(e.job.job_id)["state"] == "retired"


def test_local_source_release_undoes_lock(mk):
    e = mk(remote_driven=False)
    plan = e.plan()
    e.source.lock(e.job, e.ref, plan.revision)
    st = e.source.start_release(e.job, "cancelled")
    assert st.result["state"] == "released"
    assert e.lock.calls[-1] == "release"
    assert e.ledger.source_state(e.job.job_id)["state"] == "released"
    st2 = e.source.start_release(e.job, "cancelled")
    assert st2.result["state"] == "released"  # idempotent


def test_local_source_list_sessions_real_db(mk):
    e = mk(remote_driven=True)
    out = e.source.list_sessions(20, None, False)
    assert [s["id"] for s in out] == [e.sid]
    assert out[0]["title"] == "T" and out[0]["handover_state"] is None


def test_local_source_files_are_packed_through_the_policy(mk):
    e = mk(remote_driven=False)
    plan = e.plan()
    secret = Path(os.environ["HOME"]) / "proj" / ".env"
    secret.write_text("TOKEN=abc\n")
    items = [*plan.items, _item("x3", secret)]
    st = e.source.start_pack(e.job, e.final(plan, items=items))
    pack = e.store.doc(st.result["pack_id"])
    by_id = {i["item_id"]: i for i in pack["items"]}
    assert by_id["x3"]["object"] is None and by_id["x3"]["skipped"] == "secret"
    with e.store.open_object(by_id["x1"]["object"]) as fh:
        assert fh.read() == b"my notes\n"
