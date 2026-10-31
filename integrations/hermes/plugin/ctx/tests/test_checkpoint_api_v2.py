"""P1 contract tests: checkpoint API v2 against the REAL host MemoryManager (no network)."""

from __future__ import annotations

import json
import threading

import pytest
from agent.memory_manager import MemoryManager, _accepts_require_checkpoint
from agent.memory_provider import PRE_COMPRESS_CHECKPOINT_API_VERSION
from conftest import FakeClient, join_writes

from ctx_plugin.checkpoint import CtxCheckpointError, _negotiate_checkpoint_api
from ctx_plugin.config import load_ctx_config
from ctx_plugin.provider import CtxMemoryProvider

EVIDENCE_FACT = "Der Reverse-Proxy auf HOST-WWW terminiert TLS fuer alle oeffentlichen Dienste mit Traefik v3."
RAW_ONLY_FACT = "Tool-Ausgabe: der Container nginx-legacy lief mit Image-Version 1.2.3 und Port 8443 offen."

RAW = [
    {
        "role": "user",
        "content": "[CONTEXT COMPACTION] Zusammenfassung der bisherigen Arbeit mit vielen Details zum Stand.",
        "_compressed_summary": True,
    },
    {
        "role": "assistant",
        "content": "",
        "tool_calls": [{"id": "c1", "function": {"name": "terminal", "arguments": "{}"}}],
    },
    {"role": "tool", "tool_call_id": "c1", "content": RAW_ONLY_FACT},
    {"role": "assistant", "content": RAW_ONLY_FACT},
    {"role": "user", "content": EVIDENCE_FACT},
]
EVIDENCE = [{"role": "user", "content": EVIDENCE_FACT}]


@pytest.fixture
def provider_factory(fake_client_cls, monkeypatch, tmp_path):
    monkeypatch.setenv("CTX_KEY", "test-key-not-real")
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx.invalid")
    made = []

    def _make(ctx_json: dict | None = None, agent_context: str = "primary"):
        home = tmp_path / "profiles" / f"p{len(made)}"
        home.mkdir(parents=True, exist_ok=True)
        if ctx_json is not None:
            (home / "ctx.json").write_text(json.dumps(ctx_json), encoding="utf-8")
        p = CtxMemoryProvider()
        p.initialize("sess-v2", hermes_home=str(home), agent_context=agent_context)
        made.append(p)
        return p, p._client

    yield _make
    for p in made:
        p.shutdown()


def _manager(provider):
    mm = MemoryManager()
    mm.add_provider(provider)
    return mm


def _spy_inputs(monkeypatch):
    import ctx_plugin.provider as provider_mod

    seen = []
    real = provider_mod.extract_salient_facts

    def spy(messages, **kw):  # P4: provider passes facts_from_user/skip_keys
        seen.append(list(messages))
        return real(messages, **kw)

    monkeypatch.setattr(provider_mod, "extract_salient_facts", spy)
    return seen


# -- Version negotiation --------------------------------------------------------------


def test_provider_declares_v2_and_host_detects_it(provider_factory):
    p, _ = provider_factory()
    assert PRE_COMPRESS_CHECKPOINT_API_VERSION == 2
    assert p.pre_compress_checkpoint_api_version == 2
    mm = _manager(p)
    assert mm.supports_pre_compress_checkpoint(2) is True
    assert _accepts_require_checkpoint(p.on_pre_compress) is True


def test_checkpoint_mode_off_is_legacy_v1(provider_factory):
    p, _ = provider_factory({"checkpoint_mode": "off"})
    assert p.pre_compress_checkpoint_api_version == 1
    assert _manager(p).supports_pre_compress_checkpoint(2) is False


def test_negotiation_floor_and_ceiling():
    assert _negotiate_checkpoint_api(1) == 2
    assert _negotiate_checkpoint_api(2) == 2
    assert _negotiate_checkpoint_api(3) == 2


def test_config_coercion_checkpoint_keys(tmp_path):
    (tmp_path / "ctx.json").write_text(json.dumps({"checkpoint_mode": "Best-Effort", "checkpoint_budget_s": 999}))
    cfg = load_ctx_config(str(tmp_path))
    assert cfg["checkpoint_mode"] == "best_effort" and cfg["checkpoint_budget_s"] == 60.0
    (tmp_path / "ctx.json").write_text(json.dumps({"checkpoint_mode": "bogus", "checkpoint_budget_s": "x"}))
    cfg = load_ctx_config(str(tmp_path))
    assert cfg["checkpoint_mode"] == "best_effort" and cfg["checkpoint_budget_s"] == 20.0


# -- evidence_messages routing ------------------------------------------------------------


def test_manager_hands_evidence_not_raw_to_v2_provider(provider_factory, monkeypatch):
    p, client = provider_factory({"facts_from_user": False})  # evidence = user turn
    seen = _spy_inputs(monkeypatch)
    out = _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE)
    join_writes(p)
    assert seen == [EVIDENCE]
    assert "Facts extracted" in out
    stored = [c["content"] for c in client.calls]
    assert stored == [EVIDENCE_FACT]
    assert all("COMPACTION" not in s and RAW_ONLY_FACT not in s for s in stored)


def test_checkpoint_mode_off_provider_sees_raw(provider_factory, monkeypatch):
    p, client = provider_factory({"checkpoint_mode": "off"})
    seen = _spy_inputs(monkeypatch)
    _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE)
    join_writes(p)
    assert len(seen) == 1 and len(seen[0]) == len(RAW)
    # contrast to the v2 test: the raw transcript yields the tool-row fact
    assert RAW_ONLY_FACT in [c["content"] for c in client.calls]


def test_evidence_kwarg_wins_and_unknown_kwargs_tolerated(provider_factory, monkeypatch):
    p, _ = provider_factory()
    seen = _spy_inputs(monkeypatch)
    p.on_pre_compress(RAW, evidence_messages=EVIDENCE, checkpoint_api_version=2, future_flag=True)
    join_writes(p)
    assert seen == [EVIDENCE]


# -- require_checkpoint=True (fail-closed, synchronous) -------------------------------------


def test_memory_manager_accepts_provider_as_required_checkpoint_api_v2(provider_factory):
    p, client = provider_factory({"facts_from_user": False})  # evidence = user turn
    receipt = _manager(p).on_pre_compress(
        RAW, evidence_messages=EVIDENCE, require_checkpoint=True, checkpoint_api_version=2
    )
    # synchronous: store already happened, no background writer spawned
    assert p._write_thread is None
    assert len(client.calls) == 1
    assert receipt.strip() and "blk-0" in receipt and "CTX pre-compress receipt" in receipt


def test_required_checkpoint_without_facts_returns_nonempty_receipt(provider_factory):
    p, client = provider_factory()
    receipt = _manager(p).on_pre_compress(
        RAW, evidence_messages=[{"role": "user", "content": "ok"}], require_checkpoint=True
    )
    assert receipt.strip() and "0 salient facts" in receipt
    assert client.calls == []


@pytest.mark.parametrize("mode", ["raise", "reject"])
def test_required_checkpoint_failure_propagates(provider_factory, mode):
    p, client = provider_factory()
    if mode == "raise":
        client.fail_on = {0}
    else:
        client.reject_on = {0}
    with pytest.raises(CtxCheckpointError):
        _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE, require_checkpoint=True)


def test_required_checkpoint_raises_when_writes_disabled(provider_factory):
    p, _ = provider_factory(agent_context="subagent")
    with pytest.raises(CtxCheckpointError):
        _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE, require_checkpoint=True)


# -- require_checkpoint=False stays async best effort ---------------------------------------


def test_best_effort_is_async_and_does_not_block(provider_factory):
    p, client = provider_factory({"facts_from_user": False})  # evidence = user turn
    gate = threading.Event()
    real_store = client.store

    def slow_store(*a, **k):
        gate.wait(5.0)
        return real_store(*a, **k)

    client.store = slow_store
    out = _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE, require_checkpoint=False)
    assert "Facts extracted" in out
    assert client.calls == [], "store must not have run synchronously"
    assert p._write_thread is not None and p._write_thread.is_alive()
    gate.set()
    join_writes(p)
    assert [c["content"] for c in client.calls] == [EVIDENCE_FACT]


def test_best_effort_failure_is_swallowed(provider_factory):
    p, client = provider_factory({"facts_from_user": False})  # evidence = user turn
    client.fail_on = {0}
    out = _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE)
    join_writes(p)
    assert "Facts extracted" in out
    assert len(client.calls) == 1
    assert isinstance(FakeClient.instances[-1], FakeClient)
