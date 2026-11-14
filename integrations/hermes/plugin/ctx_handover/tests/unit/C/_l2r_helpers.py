"""Helpers of the layer-R tests of cluster C: an in-process listener reached through the real
``wire.WireClient`` (an ``httpx`` transport hands each request to ``Server.handle``), a real ``LocalSource`` for
the listener's source role, a recording target, and the export flow of a ``BundleTarget``.

``python -m ctx_handover.tests.unit.C._l2r_helpers bundle-child <config.json>`` runs the first half of an export
(open, facts, negotiate, stage, prepare) in its own process, for the test that a new process continues it.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import sys
import time
from pathlib import Path

import httpx

from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import (
    CapsuleInfo,
    GateAuth,
    GateMode,
    HermesPaths,
    Include,
    ItemKind,
    ItemRole,
    Mode,
    Options,
    Origin,
    PackInfo,
    ParticipantStatus,
    Peer,
    PlanItem,
    PrepareRequest,
    RefineReport,
    ResourcePlan,
    Right,
    Role,
)
from ctx_handover.engine.server import PeerSignatureAuth, Request, Server
from ctx_handover.engine.wire import API_PREFIX, WireClient
from ctx_handover.tests.unit.C.helpers import _NullReporter, sample_facts

M = __import__("ctx_handover.engine.model", fromlist=["m"])
ALL_RIGHTS = frozenset({Right.PUSH, Right.PULL})
ROUTE_PREFIX = API_PREFIX + "/v1/"
MEDIA_JSON = "application/json"


def noise(n: int, seed: int) -> bytes:
    out = bytearray()
    i = 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}:{i}".encode()).digest()
        i += 1
    return bytes(out[:n])


def job_context(job_id: str | None = None, *, mode: Mode = Mode.PULL, created_at: float | None = None) -> JobContext:
    return JobContext(
        job_id=job_id or ulid(),
        mode=mode,
        peer=None,
        options=Options(),
        epoch=1,
        reporter=_NullReporter(),
        cancelled=lambda: False,
        created_at=time.time() if created_at is None else float(created_at),
    )


# ---------------------------------------------------------------------------------------------------------
# In-process listener behind the real WireClient
# ---------------------------------------------------------------------------------------------------------


class InProcessTransport(httpx.BaseTransport):
    """``httpx`` transport that hands every request to ``Server.handle`` (route-relative path, lower-case headers,
    like ``dashboard/plugin_api.py``). ``seen`` records ``(method, route, body bytes)``."""

    def __init__(self, server: Server) -> None:
        self.server = server
        self.seen: list[tuple[str, str, int]] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        path = request.url.path
        assert path.startswith(ROUTE_PREFIX), path
        route = path[len(ROUTE_PREFIX) :]
        body = request.read()
        self.seen.append((request.method, route, len(body)))
        headers = {k.lower(): v for k, v in request.headers.items()}
        resp = asyncio.run(self.server.handle(Request(request.method, route, headers, body, "127.0.0.1")))
        return httpx.Response(resp.status, headers=resp.headers, content=resp.body)


class RecordingTarget:
    """The target participant of a push on the listener: records calls, answers in the contract types."""

    def __init__(self) -> None:
        self.calls: list[str] = []

    def open_job(self, job) -> None:
        self.calls.append("open_job")

    def facts(self, job, source_facts):
        self.calls.append("facts")
        return sample_facts(M, "target-host")

    def status(self, job) -> ParticipantStatus:
        self.calls.append("status")
        return ParticipantStatus(state="open", step="", epoch=job.epoch, progress=None, ops={}, last_error=None)

    def abort(self, job, reason: str) -> None:
        self.calls.append("abort")


def local_source(ledger, tmp_path: Path, fake_host):
    """A real ``LocalSource`` of the listener (the source of a pull), wired like ``jobs.make_local_apis`` needs
    for status and heartbeat (no session database is touched by those)."""
    from ctx_handover.engine.local_source import LocalSource
    from ctx_handover.engine.ops import OpRunner

    return LocalSource(
        ledger=ledger,
        store=None,
        db_factory=lambda: None,
        hermes=HermesPaths(home=str(tmp_path), document_cache=str(tmp_path)),
        host=fake_host.host_facts(),
        config={},
        ops=OpRunner(ledger, None, inline=True),
        git=None,
        calling_session=None,
        session_key=None,
        side_question=None,
        llm=None,
        read_secret=lambda name: None,
        redact=lambda text: text,
        state_db=tmp_path / "state.db",
        remote_driven=True,
        policy=None,
    )


class Pair:
    """A listener (real A ``Ledger`` and ``ParticipantRegistry``, real C ``Server``) and one paired dialer that
    reaches it through ``WireClient`` over ``InProcessTransport``."""

    def __init__(self, tmp_path: Path, *, source=None, target=None) -> None:
        from ctx_handover.engine.ledger import Ledger
        from ctx_handover.engine.participant import ParticipantRegistry
        from ctx_handover.tests.unit.C.conftest import new_identity

        self.ledger = Ledger(tmp_path / "listener" / "ledger.sqlite3")
        self.env: dict[str, str] = {}
        self.identity = new_identity(self.ledger, self.env)
        self.source = source(self.ledger) if callable(source) else source
        self.target = target if target is not None else RecordingTarget()
        self.registry = ParticipantRegistry(
            self.ledger, lambda role, peer: self.source if role is Role.SOURCE else self.target
        )
        self.server = Server(
            self.ledger,
            self.identity,
            self.registry,
            {"name": "server"},
            [PeerSignatureAuth(self.ledger, self.identity)],
        )
        dialer_ledger = Ledger(tmp_path / "dialer" / "ledger.sqlite3")
        self.dialer_identity = new_identity(dialer_ledger)
        self.peer_id = ulid()
        self.ledger.upsert_peer(
            Peer(
                name="laptop",
                peer_id=self.peer_id,
                direction="inbound",
                url=None,
                pubkey=self.dialer_identity.public_b64(),
                rights=ALL_RIGHTS,
                created_at=time.time(),
            )
        )
        self.transport = InProcessTransport(self.server)

    def client(self) -> WireClient:
        peer = Peer(
            name="server",
            peer_id=self.peer_id,
            direction="dial",
            url="http://127.0.0.1:9",
            pubkey=self.identity.public_b64(),
            rights=ALL_RIGHTS,
            gate=GateAuth(mode=GateMode.SESSION_TOKEN),
        )
        return WireClient(
            peer,
            self.dialer_identity,
            read_secret=lambda name: "gate-token",  # the in-process listener has no dashboard gate in front
            listener_id=self.identity.instance_id,
            transport=self.transport,
            max_attempts=1,
            sleep=lambda s: None,
        )


# ---------------------------------------------------------------------------------------------------------
# Bundle export flow
# ---------------------------------------------------------------------------------------------------------


def bundle_plan(job_id: str) -> ResourcePlan:
    item = PlanItem(
        item_id="i1",
        kind=ItemKind.FILE_TREE,
        source_path="/w/proj",
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=10,
    )
    return ResourcePlan(
        job_id=job_id,
        revision=(1, 2),
        items=(item,),
        not_transferred=(),
        processes=(),
        delegations=(),
        cron=(),
        refine=RefineReport(used="off"),
        tools_needed=(),
        env_vars_needed=(),
        notes_for_target="",
        approx_tokens=5,
    )


def move_objects(sink, payloads: list[bytes]) -> list:
    """Store ``payloads`` in a memory store and move manifests and blocks into ``sink`` the way the block mover
    does (manifests, ``missing``, ``write_blocks``); returns the manifests."""
    from ctx_handover.tests.fakes.blocks_fake import MemoryBlockStore

    src = MemoryBlockStore()
    ms = [src.put_bytes(p, MEDIA_JSON) for p in payloads]
    sink.put_manifests(ms)
    ids = [b for m in ms for b in m.blocks]
    _accepted, rejected = sink.write_blocks((b, src.get(b)) for b in sink.missing(ids))
    assert rejected == []
    assert sink.missing(ids) == []
    return ms


CAPSULE_BYTES = 524_288 + 77
PACK_BYTES = 3000


def stage_and_prepare(target, job: JobContext, final, *, sink=None):
    """Objects into the bundle, ``start_stage_context`` and ``start_prepare`` (what the conductor does after
    NEGOTIATED); returns ``(stage op, prepare op)``."""
    capsule_m, pack_m = move_objects(sink or target.blocks(), [noise(CAPSULE_BYTES, 1), noise(PACK_BYTES, 2)])
    capsule = CapsuleInfo(
        capsule_id=capsule_m.object_id,
        size=capsule_m.size,
        segments=1,
        rows=3,
        max_segment_bytes=capsule_m.size,
        approx_tokens=5,
        h_all="ha",
        h_rows="hr",
        h_model="hm",
        revision=(1, 2),
    )
    stage = target.start_stage_context(job, capsule)
    req = PrepareRequest(
        capsule=capsule,
        pack=PackInfo(pack_id=pack_m.object_id, size=pack_m.size),
        final_plan_id=final.plan_id,
        source_facts=sample_facts(M, "server"),
        note_parts={"title": "My session"},
    )
    return stage, target.start_prepare(job, req)


def _bundle_child(cfg_path: str) -> int:
    from ctx_handover.engine.bundle import BundleTarget
    from ctx_handover.engine.crypto import Identity
    from ctx_handover.engine.ledger import Ledger

    cfg = json.loads(Path(cfg_path).read_text(encoding="utf-8"))
    ledger = Ledger(Path(cfg["ledger"]))
    env = dict(cfg["env"])
    identity = Identity.load(ledger, read_secret=env.get, read_env_fresh=env.get)
    target = BundleTarget(
        Path(cfg["out"]),
        identity,
        as_dir=bool(cfg["as_dir"]),
        declared_facts=None,
        ledger=ledger,
        exporter_name="server",
    )
    job = job_context(cfg["job_id"], mode=Mode.EXPORT, created_at=cfg["created_at"])
    target.open_job(job)
    target.facts(job, sample_facts(M, "server"))
    final = target.negotiate(job, bundle_plan(job.job_id))
    stage, prepare = stage_and_prepare(target, job, final)
    print(
        json.dumps(
            {"plan_id": final.plan_id, "stage": str(stage.state), "prepare": str(prepare.state), "op": prepare.op_id}
        ),
        flush=True,
    )
    return 0


if __name__ == "__main__":
    if sys.argv[1:2] == ["bundle-child"]:
        sys.exit(_bundle_child(sys.argv[2]))
    sys.exit(f"unknown mode {sys.argv[1:2]}")
