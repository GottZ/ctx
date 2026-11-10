"""Cluster F — ``commands.EngineBackend``, the composition of the surfaces with the engine (layer-2 glue).

The engine modules of clusters A–C do not exist in layer 1. These tests put stand-ins into the backend's parts
(``runtime``/``ledger``/``store``/``runner``) or into ``sys.modules`` under the module names of INTERFACES §4, with
the signatures written there, and check that the glue calls them as the contract says.
"""

from __future__ import annotations

import inspect
import os
import sys
import threading
import time
import types
from pathlib import Path

import httpx
import pytest

from ctx_handover.engine import commands
from ctx_handover.engine import wire as real_wire
from ctx_handover.engine.commands import EngineBackend, HookSet, Plugin, SideQuestionTokens
from ctx_handover.engine.model import GateAuth, GateMode, Mode, Options, Right, SessionRef
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeCtxServer, FakeOutboxLedger, make_peer, sample_final_plan

JOB = "01JC0000000000000000000ABC"
OTHER = "01JC0000000000000000000XYZ"


class StubLedger(FakeOutboxLedger):
    """The ledger methods the glue calls (INTERFACES §4.5) plus the real outbox of the fake."""

    def __init__(self, path: Path) -> None:
        super().__init__(path)
        self.purged: list[str] = []
        self.notes: list[tuple] = []
        self.peer_rows = [make_peer("server"), make_peer("laptop", direction="inbound")]

    def purge_job(self, job_id: str) -> None:
        self.purged.append(job_id)

    def jobs(self, *, role=None, active=None, limit: int = 20) -> list[dict]:
        return [{"job_id": JOB}, {"job_id": OTHER}, {"job_id": "../evil"}]

    def events(self, job_id: str, role) -> list[dict]:
        if str(role) != "conductor":
            return []
        return [{"ts": 2.0, "seq": 1, "kind": "info", "data": {"token": "OPENAI_API_KEY=sk-abcdefghijklmnop1234"}}]

    def source_by_session(self, session_id: str) -> dict:
        return {"state": "retired", "peer": "laptop", "job_id": JOB}

    def arrival_for(self, session_id: str) -> None:
        return None

    def note_offer(self, session_id: str, turn_id: str) -> None:
        self.notes.append(("offer", session_id, turn_id))

    def note_delivered(self, session_id: str) -> None:
        self.notes.append(("delivered", session_id))

    def peers(self, *, include_revoked: bool = False):
        return list(self.peer_rows)

    def peer(self, name: str):
        return next((p for p in self.peer_rows if p.name == name), None)

    def pairing_pending(self, now: float) -> bool:
        return False

    def meta_get(self, key: str):
        return getattr(self, "meta", {}).get(key)

    def meta_set(self, key: str, value: str, conn=None) -> None:
        self.__dict__.setdefault("meta", {})[key] = value


class StubStore:
    def __init__(self) -> None:
        self.released: list[tuple[str, bool]] = []
        self.gc_calls: list[dict] = []

    def release_job(self, job_id: str, *, keep: bool) -> None:
        self.released.append((job_id, keep))

    def gc(self, *, keep_days: int, max_bytes: int) -> int:
        self.gc_calls.append({"keep_days": keep_days, "max_bytes": max_bytes})
        return 7 * 1024**2


class StubRunner:
    """``jobs.JobRunner`` with the INTERFACES §4.5 signatures (+ the additive kwargs F asks A for)."""

    def __init__(self) -> None:
        self.calls: list[tuple[str, dict]] = []

    def plan_sync(
        self,
        mode,
        peer,
        options,
        *,
        session_ref,
        calling_session,
        session_key,
        ctx=None,
        confirm=None,
        bundle_path=None,
        include_secret=(),
        side_question=None,
    ):
        self.calls.append(("plan_sync", locals()))
        return JOB, sample_final_plan()

    def status(self, job_id):
        return {"job": {"job_id": job_id, "state": "done"}} if job_id else {"jobs": []}

    def create_offer(self, peer, session_ref, *, ctx, options=None, side_question=None, include_secret=()):
        self.calls.append(("create_offer", locals()))
        return {"offer_id": "01OFFER", "expires_at": 1.0}


@pytest.fixture
def backend(fake_host, tmp_path):
    ctx = FakeCtx(host=fake_host)
    plugin = Plugin(ctx)
    eb = EngineBackend(ctx, plugin)
    ledger = StubLedger(tmp_path / "ledger.sqlite3")
    store = StubStore()
    veto = types.SimpleNamespace(
        lookup=lambda sid, parent=None: {"peer": "laptop"} if sid == "vetoed" else None,
        arrival=lambda sid: None,
        on_compaction=lambda a, b: None,
    )
    runtime = types.SimpleNamespace(
        ledger=ledger, store=store, veto=veto, identity=None, identity_error=None, lock=threading.RLock()
    )
    eb._parts.update({"runtime": runtime, "runner": StubRunner()})
    eb.ledger_stub, eb.store_stub = ledger, store  # type: ignore[attr-defined]
    return eb


def test_gc_purges_job_artifacts(fake_host):
    """★ BO §6 #21, SEC 6.3, R28: ``gc --job`` removes everything of a finished job — its blocks and object
    manifests, staging with quarantine, report, continuation log, journal and job rows — through cluster
    A's one implementation ``jobs.gc_job`` against the real profile runtime (``Ledger``, ``BlockStore``); another
    job keeps all of its own. An unfinished job is refused with ``--job`` and kept by ``--all``; an id that is not a
    ULID never reaches a path.

    Red proof: mutant = ``EngineBackend.gc`` with layer 1's own steps (``purge_job`` before ``release_job``,
    ``rmtree`` of staging) instead of ``jobs.gc_job`` → the job's object references are gone before the block store
    looks them up: ``AssertionError: blocks of the finished job are still cached``.
    """
    from ctx_handover.engine import jobs
    from ctx_handover.engine.errors import BadRequest, InvalidState
    from ctx_handover.engine.ids import ulid
    from ctx_handover.engine.model import Role

    jobs._reset_for_tests()
    prof = None
    try:
        plugin = Plugin(FakeCtx(host=fake_host))
        backend = plugin.backend
        rt = backend._started()
        prof = rt
        ledger, store = rt.ledger, rt.store
        data = fake_host.plugin_data
        # stored long before any job of this host began: older than the manifest grace (R28) and than the
        # in-flight protection of cluster C (blocks touched since the oldest unfinished job began stay)
        old = time.time() - 7200
        made: dict[str, dict] = {}
        for name, final in (("a", "done"), ("b", "done"), ("open", None)):
            jid = ulid()
            ledger.create_job(jid, Role.CONDUCTOR, Mode.PUSH, None, {"name": name})
            epoch = ledger.claim_owner(jid, Role.CONDUCTOR, "test-owner", 60.0)
            ledger.event(jid, Role.CONDUCTOR, "info", {"step": "pack"}, epoch=epoch)
            if final:
                ledger.transition(jid, Role.CONDUCTOR, "*", final, {"why": "test"}, epoch=epoch)
                ledger.release_owner(jid, Role.CONDUCTOR, "test-owner", epoch)
            m = store.put_bytes(os.urandom(1_200_000), "application/octet-stream")  # three distinct blocks
            assert len(set(m.blocks)) == 3
            ledger.job_object(jid, m.object_id)
            for path in (store.manifest_path(m.object_id), *(store.block_path(b) for b in m.blocks)):
                os.utime(path, (old, old))
            paths = [
                data / "staging" / jid / "git" / "pack",
                data / "staging" / jid / "quarantine" / "w0" / "objects" / "x",
                data / "reports" / f"{jid}.md",
                data / "logs" / f"continue-{jid}.log",
            ]
            for path in paths:
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("x", encoding="utf-8")
            made[name] = {"job": jid, "object": m.object_id, "blocks": list(m.blocks), "paths": paths}

        def gone(name: str) -> None:
            entry = made[name]
            assert not any(store.has(b) for b in entry["blocks"]), "blocks of the finished job are still cached"
            assert not store.manifest_path(entry["object"]).exists(), "manifest of the finished job is still there"
            assert (
                ledger.job(entry["job"], Role.CONDUCTOR) is None and ledger.events(entry["job"], Role.CONDUCTOR) == []
            )
            assert not (data / "staging" / entry["job"]).exists()
            for path in entry["paths"]:
                assert not path.exists(), path

        def kept(name: str) -> None:
            entry = made[name]
            assert all(store.has(b) for b in entry["blocks"])  # cached within block_cache_days/_gb
            if name == "open":  # a finished job's manifests may go with any cache pass (R28); a live job's never
                assert store.manifest_path(entry["object"]).exists()
            assert ledger.job(entry["job"], Role.CONDUCTOR) is not None
            assert ledger.events(entry["job"], Role.CONDUCTOR)
            assert all(path.exists() for path in entry["paths"]), name

        result = backend.gc(job_id=made["a"]["job"], all_jobs=False)
        gone("a")
        kept("b")
        kept("open")
        assert result["jobs"] == 1 and result["freed_bytes"] >= 1_200_000
        with pytest.raises(InvalidState):
            backend.gc(job_id=made["open"]["job"], all_jobs=False)
        kept("open")
        with pytest.raises(BadRequest):
            backend.gc(job_id="../" + made["b"]["job"], all_jobs=False)
        kept("b")
        result = backend.gc(job_id=None, all_jobs=True)
        gone("b")
        kept("open")
        assert result["jobs"] == 1 and result["skipped"] == 1
    finally:
        if prof is not None and getattr(prof, "veto", None) is not None:
            prof.veto.stop()
        jobs._reset_for_tests()


def test_gc_removes_the_continuation_files(fake_host):
    """SEC 6.3: ``gc --job`` removes both files of the job's continuation that ``continuation.job_files`` names
    (log and sidecar), through ``jobs.gc_job`` — the sidecar keeps pid and start time, which ``status`` would
    otherwise report for a purged job."""
    from ctx_handover.engine import jobs
    from ctx_handover.engine.continuation import job_files
    from ctx_handover.engine.ids import ulid
    from ctx_handover.engine.model import Role

    jobs._reset_for_tests()
    rt = None
    try:
        backend = Plugin(FakeCtx(host=fake_host)).backend
        rt = backend._started()
        jid = ulid()
        rt.ledger.create_job(jid, Role.TARGET, Mode.PUSH, None, {})
        epoch = rt.ledger.claim_owner(jid, Role.TARGET, "test-owner", 60.0)
        rt.ledger.transition(jid, Role.TARGET, "*", "committed", {}, epoch=epoch)
        files = job_files(fake_host.plugin_data / "logs", jid)
        for path in files:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("x", encoding="utf-8")
        backend.gc(job_id=jid, all_jobs=False)
        assert [p.name for p in files if p.exists()] == []
    finally:
        if rt is not None and getattr(rt, "veto", None) is not None:
            rt.veto.stop()
        jobs._reset_for_tests()


def test_flush_outbox_needs_operator_consent(backend, fake_host):
    """D12: records go to ctx only with ctx_checkpoint as provider, ``ctx_persist`` not off and the operator's
    ``mcp_allowlist`` entry; otherwise nothing is sent and status says what to do."""
    server = FakeCtxServer()
    assert backend.flush_outbox(server.call_mcp) == {}
    fake_host.config = {"memory": {"provider": "ctx_checkpoint"}}
    backend.ledger_stub.outbox_put("departure", {"title": "t", "content": "c", "tags": [], "metadata": {}})
    res = backend.flush_outbox(server.call_mcp)
    assert res["pending"] == 1 and res["stored"] == 0 and "/handover setup" in res["error"]
    assert server.stores == []
    fake_host.config["plugins"] = {"entries": {"ctx_handover": {"mcp_allowlist": ["ctx"]}}}
    res = backend.flush_outbox(server.call_mcp)
    assert res == {"server": "ctx", "stored": 1, "pending": 0}
    fake_host.config["plugins"]["entries"]["ctx_handover"]["settings"] = {"ctx_persist": "off"}
    assert backend.flush_outbox(server.call_mcp) == {}


def test_backend_spawner_only_reads_results(backend, fake_host):
    """X21/R10/R22: the surfaces' ``Spawner`` only reads continuation results for ``status``; it never starts a
    turn, whatever the settings say (the starting spawner is the target's, ``jobs.make_local_apis``)."""
    import json as _json

    from ctx_handover.engine.continuation import job_files
    from ctx_handover.engine.errors import ContinueRefused

    fake_host.config = {"plugins": {"entries": {"ctx_handover": {"settings": {"allow_remote_continue": True}}}}}
    spawner = backend._spawner()
    with pytest.raises(ContinueRefused):
        spawner.spawn("20261109_141003_a1b2c3", "go", JOB, requested_by=None)
    with pytest.raises(ContinueRefused):
        spawner.spawn("20261109_141003_a1b2c3", "go", JOB, requested_by="laptop")
    log, meta = job_files(fake_host.plugin_data / "logs", JOB)
    meta.parent.mkdir(parents=True, exist_ok=True)
    meta.write_text(_json.dumps({"pid": 4242, "exit_code": 0, "started_at": 1.0}), encoding="utf-8")
    assert spawner.result(JOB) == {
        "started": True,
        "pid": 4242,
        "log": str(log),
        "running": False,
        "exit_code": 0,
        "started_at": 1.0,
    }
    assert spawner.hermes_argv()[1:] == []  # default profile ⇒ no -p


def test_start_job_passes_contract_kwargs(backend):
    """``plan_sync`` gets ``ctx``, ``confirm`` and — only when present — ``bundle_path``/``side_question``."""
    runner = backend._parts["runner"]
    peer = make_peer("server")
    backend.start_job(
        Mode.PUSH,
        peer,
        Options(),
        session_ref=SessionRef(id="s"),
        calling_session="s",
        session_key="k",
        bundle_path=None,
        confirm=None,
        side_question=None,
        reporter=None,
    )
    call = runner.calls[-1][1]
    assert call["bundle_path"] is None and call["side_question"] is None and call["ctx"] is backend._ctx
    backend.start_job(
        Mode.IMPORT,
        None,
        Options(),
        session_ref=SessionRef(),
        calling_session=None,
        session_key=None,
        bundle_path="/b/x.ctxh.tar",
        confirm=None,
        side_question=len,
        reporter=None,
    )
    call = runner.calls[-1][1]
    assert call["bundle_path"] == Path("/b/x.ctxh.tar") and call["side_question"] is len
    backend.create_offer(peer, SessionRef(id="s"), Options(yes=True), side_question=None, include_secret=("/a/.env",))
    offer = runner.calls[-1][1]
    assert offer["options"].yes is True and offer["ctx"] is backend._ctx
    assert offer["include_secret"] == ("/a/.env",)  # X07: the explicit parameter, as for plan_sync


def test_logs_redacted_and_session_state(backend):
    events = backend.logs(JOB)
    assert len(events) == 1 and "sk-abcdefghijklmnop1234" not in events[0]["data"]
    assert backend.session_state("s") == {
        "source": {"state": "retired", "peer": "laptop", "job_id": JOB},
        "arrival": None,
    }


def c_report(peer, report: dict) -> dict:
    """``report`` with cluster C's summary (``ok``, ``detail``, ``remedy``) added exactly as the real
    ``WireClient.probe_gate`` adds it (``wire.WireClient._summary``) — what the stub clients of these tests return."""
    client = real_wire.WireClient(
        peer,
        None,
        read_secret=lambda name: "",
        transport=httpx.MockTransport(lambda request: httpx.Response(500)),
        max_attempts=1,
        sleep=lambda s: None,
    )
    try:
        return client._summary(dict(report))
    finally:
        client.close()


def _stub_module(monkeypatch, name: str, **attrs) -> types.ModuleType:
    module = types.ModuleType(f"ctx_handover.engine.{name}")
    for key, value in attrs.items():
        setattr(module, key, value)
    monkeypatch.setitem(sys.modules, f"ctx_handover.engine.{name}", module)
    import ctx_handover.engine as engine_pkg

    # ``from . import x`` prefers the package attribute once the real module was imported by another suite
    monkeypatch.setattr(engine_pkg, name, module, raising=False)
    return module


def test_hook_runtime_adapter_queues_note_writes(backend, monkeypatch):
    """§7.6/§11.4: the adapter answers lookups from the in-memory index and writes note state on its own thread,
    so ``pre_llm_call`` never waits for SQLite."""
    texts = []
    _stub_module(
        monkeypatch,
        "lock",
        veto_texts=lambda entry: texts.append(entry) or f"handed over to {entry['peer']}",
        VetoIndex=None,
    )
    adapter = backend.hook_runtime()
    hooks = HookSet(lambda: adapter, SideQuestionTokens())
    assert hooks.pre_llm_call(session_id="vetoed") == {"action": "block", "message": "handed over to laptop"}
    adapter.note_offered("s1", "t1")
    adapter.note_delivered("s1")
    deadline = time.monotonic() + 5
    while len(backend.ledger_stub.notes) < 2 and time.monotonic() < deadline:
        time.sleep(0.01)
    assert backend.ledger_stub.notes == [("offer", "s1", "t1"), ("delivered", "s1")]


def test_pairing_wiring_creates_identity_only_in_operator_verbs(backend, monkeypatch, fake_host):
    """R16/R17: listener ``pair`` creates the identity through the profile .env writer with read-back helpers and
    prints a pairing string built without any gate secret; the dialer redeems with the gate from the settings."""
    created, codes, strings, redeemed = [], [], [], []

    class Identity:
        instance_id = "01INST"

        @classmethod
        def load(cls, ledger, *, read_secret, read_env_fresh):
            return None

        @classmethod
        def create(cls, ledger, *, env_writable, save_env, read_env_fresh):
            created.append((env_writable, save_env, read_env_fresh))
            return cls()

        def fingerprint(self) -> str:
            return "0123456789abcdef"

    def create_code(ledger, *, rights, name_hint, ttl_s):
        codes.append((rights, name_hint, ttl_s))
        return types.SimpleNamespace(
            code_id="ABCDEF", secret="s" * 16, expires_at=600.0, rights=rights, name_hint=name_hint
        )

    def pair_string(code, *, url, fingerprint, name, profile):
        strings.append(dict(url=url, fingerprint=fingerprint, name=name, profile=profile))
        return "ctxh-pair:ZZZ"

    def parse_pair_string(text):
        return {
            "url": "https://s.example",
            "code": "CTXH-ABCDEF-x",
            "fp": "0123456789abcdef",
            "name": "srv",
            "profile": None,
        }

    def redeem_dialer(client_factory, identity, pair, *, name, gate, allow_insecure, ledger, secret_store=None):
        redeemed.append(
            dict(name=name, gate=gate, allow_insecure=allow_insecure, client=client_factory, store=secret_store)
        )
        return make_peer(name or pair["name"])  # C names the listener when F passes no name (V44)

    _stub_module(monkeypatch, "crypto", Identity=Identity)
    _stub_module(
        monkeypatch,
        "pairing",
        create_code=create_code,
        pair_string=pair_string,
        parse_pair_string=parse_pair_string,
        redeem_dialer=redeem_dialer,
        SecretStore=types.SimpleNamespace,  # H1: the profile .env handed to the pairing
    )
    _stub_module(monkeypatch, "wire", WireClient=lambda peer, ident, *, read_secret: ("client", peer.name))
    fake_host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "dashboard-secret"
    out = backend.create_pair_code(
        rights=frozenset({Right.PUSH}), name_hint="laptop", ttl_s=600, url="https://s.example"
    )
    assert out["pair_string"] == "ctxh-pair:ZZZ" and "dashboard-secret" not in str(out) + str(strings)
    from ctx_handover.engine import hostcompat

    assert created == [(hostcompat.env_writable, hostcompat.save_env, hostcompat.read_env_fresh)]
    assert codes == [(frozenset({Right.PUSH}), "laptop", 600)] and strings[0]["profile"] is None
    gate = GateAuth(mode=GateMode.SESSION_TOKEN)
    peer = backend.redeem_pair("ctxh-pair:ZZZ", name=None, gate=gate, allow_insecure=True)
    assert peer.name == "srv" and redeemed[0]["gate"] is gate and redeemed[0]["allow_insecure"] is True
    assert redeemed[0]["name"] == ""  # no --name: the choice (paste-string name or the next free one) is C's
    assert redeemed[0]["client"](make_peer("srv")) == ("client", "srv")
    store = redeemed[0]["store"]  # H1: Hermes' own .env readers and writer, never a file of the plugin's
    assert (store.read, store.save, store.read_fresh, store.writable) == (
        hostcompat.read_secret,
        hostcompat.save_env,
        hostcompat.read_env_fresh,
        hostcompat.env_writable,
    )


def test_contract_signatures_used_by_the_glue():
    """The keyword names the glue passes exist in cluster A's real ``JobRunner`` signatures, and the stand-in above
    copies them (so the other tests of this file exercise the real contract)."""
    from ctx_handover.engine import jobs

    for method in ("plan_sync", "create_offer"):
        real = inspect.signature(getattr(jobs.JobRunner, method)).parameters
        stub = inspect.signature(getattr(StubRunner, method)).parameters
        assert set(stub) <= set(real), (method, set(stub) - set(real))
    real = inspect.signature(jobs.JobRunner.plan_sync).parameters
    for name in ("session_ref", "calling_session", "session_key", "ctx", "confirm", "bundle_path", "include_secret"):
        assert name in real and name in inspect.signature(StubRunner.plan_sync).parameters
    real_offer = inspect.signature(jobs.JobRunner.create_offer).parameters
    for name in ("ctx", "options", "side_question", "include_secret"):  # contract X07 / W110
        assert name in real_offer, name
    assert commands.LOCAL_ONLY_VERBS


def test_doctor_checks_wiring(backend, monkeypatch, fake_host):
    """``doctor`` (DESIGN §14.1, BX P9): git version from cluster E, data-folder mode, identity, inbound state and
    one gate probe per dialled peer from cluster C, each with a remedy."""
    _stub_module(
        monkeypatch,
        "gitops",
        Git=lambda: object(),
        check_version=lambda git: (True, "git 2.39.0 is below the patched line 2.39.5"),
    )

    class Identity:
        @classmethod
        def load(cls, ledger, *, read_secret, read_env_fresh):
            return types.SimpleNamespace(fingerprint=lambda: "0123456789abcdef", instance_id="01I")

    _stub_module(monkeypatch, "crypto", Identity=Identity)

    probes = []

    class Client:
        """``wire.WireClient`` with cluster C's ``probe_gate`` report form (wire.py ``probe_gate``)."""

        clock_skew = 0.0

        def __init__(self, peer, ident, *, read_secret):
            self.peer = peer

        def probe_gate(self, *, measure_limit: bool = False):
            probes.append(measure_limit)
            return c_report(self.peer, {"gate": "rejected", "plugin": False, "signed": False, "error": "gate_rejected"})

    _stub_module(monkeypatch, "wire", WireClient=Client, PROBE_SIZES_MIB=real_wire.PROBE_SIZES_MIB)
    fake_host.plugin_dir()
    checks = {c["name"]: c for c in backend.doctor_checks()}
    assert checks["git"]["ok"] is None and "patched line" in checks["git"]["detail"]
    assert checks["data folder"]["ok"] is True
    assert checks["identity"]["ok"] is True and "0123456789abcdef" in checks["identity"]["detail"]
    assert "1 paired device(s) may connect to this machine" in checks["inbound"]["detail"]
    assert probes == [False]  # the body-limit measurement runs only with doctor --measure
    assert checks["peer server"]["ok"] is False and "refused the stored sign-in" in checks["peer server"]["detail"]
    assert "store the current dashboard sign-in of server" in checks["peer server"]["remedy"]  # C's remedy (V27)
    assert "peer laptop" not in checks
