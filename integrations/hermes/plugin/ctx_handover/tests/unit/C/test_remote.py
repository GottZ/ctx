"""``RemoteSource``/``RemoteTarget``/``RemoteBlocks`` against a real listener process (DESIGN §4.5, §4.6, §17.5)."""

from __future__ import annotations

import hashlib
from dataclasses import replace

import pytest

from ctx_handover.engine import contracts, hostcompat, pairing
from ctx_handover.engine.blocks import BLOCK_SIZE, BlockStore, object_id_of
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.errors import BadRequest, BlockCorrupt, LeaseLost, NotFound
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import (
    CapsuleInfo,
    CommitAck,
    DeltaInfo,
    FinalPlan,
    GateAuth,
    GateMode,
    Mode,
    OpState,
    Options,
    PackInfo,
    PlanHints,
    PreparedInfo,
    PrepareRequest,
    RefineReport,
    ResourcePlan,
    SessionRef,
)
from ctx_handover.engine.remote import RemoteBlocks, RemoteSource, RemoteTarget, remote_options
from ctx_handover.engine.wire import WireClient
from ctx_handover.tests.fakes.blocks_fake import MemoryBlockStore
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.helpers import _NullReporter, sample_facts
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

TOKEN = "gate-tok-remote"


@pytest.fixture
def setup(tmp_path, spawn_listener, host):
    host.secrets["CTX_HANDOVER_PEER_TOKEN"] = TOKEN
    lst = spawn_listener(gate_token=TOKEN)
    ledger = make_ledger(tmp_path / "dialer" / "ledger.sqlite3")
    identity = new_identity(ledger)
    code = pairing.create_code(lst.ledger, rights=frozenset({"push", "pull"}), name_hint="laptop", ttl_s=600)
    text = pairing.pair_string(
        code, url=lst.base_url, fingerprint=lst.identity.fingerprint(), name="server", profile=None
    )

    def factory(peer):
        return WireClient(peer, identity, read_secret=hostcompat.read_secret, sleep=lambda s: None)

    peer = pairing.redeem_dialer(
        factory,
        identity,
        pairing.parse_pair_string(text),
        name="server",
        gate=GateAuth(mode=GateMode.SESSION_TOKEN),
        allow_insecure=False,
        ledger=ledger,
    )
    client = factory(peer)
    client.hello(signed=True)
    return lst, client


def job_ctx(mode=Mode.PUSH, epoch=1, options=None) -> JobContext:
    return JobContext(
        job_id=ulid(),
        mode=mode,
        peer=None,
        options=options or Options(),
        epoch=epoch,
        reporter=_NullReporter(),
        cancelled=lambda: False,
        created_at=0.0,
    )


def _plan(job_id: str) -> ResourcePlan:
    return ResourcePlan(
        job_id=job_id,
        revision=(1, 2),
        items=(),
        not_transferred=(),
        processes=(),
        delegations=(),
        cron=(),
        refine=RefineReport(used="off"),
        tools_needed=(),
        env_vars_needed=(),
        notes_for_target="",
        approx_tokens=0,
    )


def test_remote_target_cycle(setup):
    _lst, client = setup
    target = RemoteTarget(client)
    assert isinstance(target, contracts.TargetAPI)
    job = job_ctx()
    target.open_job(job)
    target.open_job(job)  # idempotent
    facts = sample_facts(__import__("ctx_handover.engine.model", fromlist=["m"]), "laptop")
    assert target.facts(job, facts).instance_name == "target-host"
    final = target.negotiate(job, _plan(job.job_id))
    assert isinstance(final, FinalPlan) and final.plan.job_id == job.job_id
    capsule = CapsuleInfo(
        capsule_id="c" * 64,
        size=1,
        segments=1,
        rows=1,
        max_segment_bytes=1,
        approx_tokens=1,
        h_all="a",
        h_rows="b",
        h_model="c",
        revision=(1, 2),
    )
    op = target.start_stage_context(job, capsule)
    assert op.state == OpState.DONE and op.op_id == "op-stage_context"
    req = PrepareRequest(
        capsule=capsule,
        pack=PackInfo(pack_id="p" * 64, size=1),
        final_plan_id="f" * 64,
        source_facts=facts,
        note_parts={"x": 1},
    )
    assert target.start_prepare(job, req).op_id == "op-prepare"
    delta = DeltaInfo(delta_id="d" * 64, tip_id="t", rows=1, h_all="a", h_rows="b", h_model="c", revision=(3,))
    assert target.start_apply_delta(job, delta).op_id == "op-apply_delta"
    assert target.start_reset_session(job).op_id == "op-reset_session"
    assert target.start_commit(job).op_id == "op-commit"
    assert target.op_status(job, "op-plan").op_id == "op-plan"
    with pytest.raises(NotFound):
        target.op_status(job, "op-unknown")
    started = target.continue_session(job, "carry on")
    assert started.started and started.pid == 4242
    assert target.status(job).state == "open"
    target.abort(job, "user")
    # blocks through the target's sink
    sink = target.blocks()
    store = MemoryBlockStore()
    m = store.put_bytes(hashlib.sha256(b"x").digest() * (BLOCK_SIZE // 16), "application/octet-stream")
    assert sink.put_manifests([m]) == 1
    assert sink.missing(list(m.blocks)) == list(m.blocks)
    accepted, rejected = sink.write_blocks((b, store.get(b)) for b in m.blocks)
    assert (accepted, rejected) == (len(m.blocks), [])
    assert sink.missing(list(m.blocks)) == []


def test_remote_source_cycle(setup):
    lst, client = setup
    source = RemoteSource(client)
    assert isinstance(source, contracts.SourceAPI)
    assert source.hello().instance_id == lst.identity.instance_id
    assert source.list_sessions(5, None, False)[0]["id"] == "s1"
    job = job_ctx(mode=Mode.PULL)
    assert source.facts(job).instance_name == "source-host"
    facts = source.facts(job)
    assert source.start_plan(job, facts, SessionRef(latest=True)).op_id == "op-plan"
    info = source.lock(job, SessionRef(id="s"), (1, 2))
    assert info.tip_id == "t" and info.revision == (1, 2)
    source.heartbeat(job)
    assert source.start_export(job).op_id == "op-export"
    final = FinalPlan(
        plan_id="p" * 64, plan=_plan(job.job_id), decisions=(), need_bytes=0, free_bytes={}, model_change=None
    )
    assert source.start_pack(job, final).op_id == "op-pack"
    prepared = PreparedInfo(
        tip_id="t",
        root_id="r",
        title=None,
        h_all="a",
        h_rows="b",
        h_model="c",
        import_path="A",
        dests=(),
        warnings=(),
        source_revision=(1, 2),
    )
    assert source.start_prepare(job, prepared).op_id == "op-prepare"
    ack = CommitAck(
        session_id="s",
        root_id="r",
        title=None,
        dests=(),
        path_map=(),
        model=None,
        offline_capable=True,
        warnings=(),
        committed_at=1.0,
    )
    assert source.start_retire(job, ack).result == {"state": "retired"}
    assert source.start_release(job, "abort").result == {"state": "released"}
    # blocks of the job's objects, read through the remote source
    store = BlockStore(lst.plugin_dir, lst.ledger)
    data = b"".join(hashlib.sha256(str(i).encode()).digest() for i in range(BLOCK_SIZE // 16))
    m = store.put_bytes(data, "application/octet-stream")
    lst.ledger.job_object(job.job_id, m.object_id)
    blocks = source.blocks()
    assert blocks.manifests([m.object_id]) == [m]
    assert b"".join(raw for _i, raw in blocks.read_blocks(list(m.blocks))) == data


def test_remote_heartbeat_lease_lost():
    class Wire:
        def call(self, method, route, body=None, *, epoch=None):
            if route == "jobs":
                return {"job_id": body["job_id"], "state": "open", "epoch": 1}
            return {"state": "suspended"}

        def conductor_info(self):
            return {"instance_id": "X"}

    with pytest.raises(LeaseLost):
        RemoteSource(Wire()).heartbeat(job_ctx(mode=Mode.PULL))


def test_remote_options_never_send_secret_hints():
    opts = Options(hints=PlanHints(include=("a",), include_secret=(".env",)), export_roots=("/srv",))
    sent = remote_options(opts)
    assert sent["hints"]["include_secret"] == []
    assert sent["hints"]["include"] == ["a"]
    assert "export_roots" not in sent  # the listener's own settings decide (R18)


def test_remote_blocks_verifies_manifests():
    m = MemoryBlockStore().put_bytes(b"abc", "application/octet-stream")

    class Wire:
        def __init__(self, manifest):
            self.manifest = manifest
            self.peer_caps = frozenset()

        def call(self, method, route, body=None, *, epoch=None):
            from ctx_handover.engine.jsonutil import to_wire

            return {"manifests": [to_wire(self.manifest)]}

    good = RemoteBlocks(Wire(m), ulid(), 1)
    assert good.manifests([m.object_id]) == [m]
    forged = replace(m, sha256="0" * 64)  # keeps the old object id: a hash mismatch (K-1)
    with pytest.raises(BlockCorrupt):
        RemoteBlocks(Wire(forged), ulid(), 1).manifests([m.object_id])
    other = replace(forged, object_id=object_id_of(forged))  # consistent, but not what was asked for
    with pytest.raises(BadRequest):
        RemoteBlocks(Wire(other), ulid(), 1).manifests([m.object_id])
