"""Cluster F — ``commands.EngineBackend``, the composition of the surfaces with the engine (layer-2 glue).

The engine modules of clusters A–C do not exist in layer 1. These tests put stand-ins into the backend's parts
(``runtime``/``ledger``/``store``/``runner``) or into ``sys.modules`` under the module names of INTERFACES §4, with
the signatures written there, and check that the glue calls them as the contract says.
"""

from __future__ import annotations

import inspect
import sys
import time
import types
from pathlib import Path

import pytest

from ctx_handover.engine import commands
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
        side_question=None,
    ):
        self.calls.append(("plan_sync", locals()))
        return JOB, sample_final_plan()

    def status(self, job_id):
        return {"job": {"job_id": job_id, "state": "done"}} if job_id else {"jobs": []}

    def create_offer(self, peer, session_ref, *, ctx, options=None, side_question=None):
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
    eb._parts.update({"runtime": types.SimpleNamespace(ledger=ledger, store=store, veto=veto), "runner": StubRunner()})
    eb.ledger_stub, eb.store_stub = ledger, store  # type: ignore[attr-defined]
    return eb


def test_gc_purges_job_artifacts(backend, fake_host):
    """BO §6 #21, SEC 6.3: ``gc --job`` deletes that job's journal (ledger), blocks, staging, quarantine, report and
    continuation log immediately; other jobs keep theirs; ``--all`` purges every job and empties the cache;
    a job id that is not a plain ULID is never used as a path."""
    data = fake_host.plugin_data
    for job in (JOB, OTHER):
        for rel in (
            f"staging/{job}/git/x",
            f"quarantine/{job}/objects/y",
            f"reports/{job}.md",
            f"logs/continue-{job}.log",
            f"logs/continue-{job}.json",
        ):
            path = data / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("x", encoding="utf-8")
    result = backend.gc(job_id=JOB, all_jobs=False)
    assert result == {"freed_bytes": 7 * 1024**2, "jobs": 1}
    assert backend.ledger_stub.purged == [JOB]
    assert backend.store_stub.released == [(JOB, False)]
    for rel in (f"staging/{JOB}", f"quarantine/{JOB}", f"reports/{JOB}.md", f"logs/continue-{JOB}.log"):
        assert not (data / rel).exists(), rel
    assert (data / f"staging/{OTHER}/git/x").exists() and (data / f"reports/{OTHER}.md").exists()
    assert backend.store_stub.gc_calls[-1] == {"keep_days": 3, "max_bytes": 5 * 1024**3}
    backend.gc(job_id=None, all_jobs=True)
    assert backend.ledger_stub.purged == [JOB, JOB, OTHER]
    assert not (data / f"staging/{OTHER}").exists()
    assert backend.store_stub.gc_calls[-1] == {"keep_days": 0, "max_bytes": 0}


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


def test_spawner_wiring_reads_live_settings(backend, fake_host):
    """R10/R22: the continuation spawner of this gateway refuses remote requests unless ``allow_remote_continue``
    and refuses everything while approvals are off — both read at spawn time."""
    spawner = backend._spawner()
    assert spawner._allow_remote() is False and spawner._approvals_off() is False
    fake_host.config = {
        "approvals": {"mode": False},
        "plugins": {"entries": {"ctx_handover": {"settings": {"allow_remote_continue": True}}}},
    }
    assert spawner._allow_remote() is True and spawner._approvals_off() is True
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
    backend.create_offer(peer, SessionRef(id="s"), Options(yes=True), side_question=None)
    offer = runner.calls[-1][1]
    assert offer["options"].yes is True and offer["ctx"] is backend._ctx


def test_logs_redacted_and_session_state(backend):
    events = backend.logs(JOB)
    assert len(events) == 1 and "sk-abcdefghijklmnop1234" not in events[0]["data"]
    assert backend.session_state("s") == {
        "source": {"state": "retired", "peer": "laptop", "job_id": JOB},
        "arrival": None,
    }


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

    def redeem_dialer(client_factory, identity, pair, *, name, gate, allow_insecure, ledger):
        redeemed.append(dict(name=name, gate=gate, allow_insecure=allow_insecure, client=client_factory))
        return make_peer(name)

    _stub_module(monkeypatch, "crypto", Identity=Identity)
    _stub_module(
        monkeypatch,
        "pairing",
        create_code=create_code,
        pair_string=pair_string,
        parse_pair_string=parse_pair_string,
        redeem_dialer=redeem_dialer,
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
    assert redeemed[0]["client"](make_peer("srv")) == ("client", "srv")


def test_contract_signatures_used_by_the_glue():
    """The keyword names the glue passes exist in the INTERFACES §4 signatures it targets (checked against the
    stand-ins above, which copy those signatures)."""
    params = inspect.signature(StubRunner.plan_sync).parameters
    for name in ("session_ref", "calling_session", "session_key", "ctx", "confirm"):
        assert name in params
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

    class Client:
        def __init__(self, peer, ident, *, read_secret):
            self.peer = peer

        def probe_gate(self):
            return {
                "ok": False,
                "detail": "dashboard answered 401 without a plugin signature",
                "remedy": "check peer_token",
            }

    _stub_module(monkeypatch, "wire", WireClient=Client)
    fake_host.plugin_dir()
    checks = {c["name"]: c for c in backend.doctor_checks()}
    assert checks["git"]["ok"] is None and "patched line" in checks["git"]["detail"]
    assert checks["data folder"]["ok"] is True
    assert checks["identity"]["ok"] is True and "0123456789abcdef" in checks["identity"]["detail"]
    assert "1 paired device(s) may dial in" in checks["inbound"]["detail"]
    assert checks["peer server"]["ok"] is False and checks["peer server"]["remedy"] == "check peer_token"
    assert "peer laptop" not in checks
