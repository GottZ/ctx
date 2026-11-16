"""P1 contract tests: checkpoint API v2 against the REAL host MemoryManager (no network)."""

from __future__ import annotations

import json
import logging
import threading

import pytest
from agent.memory_manager import MemoryManager, _accepts_require_checkpoint
from agent.memory_provider import PRE_COMPRESS_CHECKPOINT_API_VERSION
from conftest import FakeClient, join_writes

from ctx_plugin.checkpoint import (
    RATE_WINDOW_MARGIN_S,
    RATE_WINDOW_S,
    CtxCheckpointError,
    _negotiate_checkpoint_api,
)
from ctx_plugin.client import CtxApiError
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
    assert cfg["checkpoint_mode"] == "best_effort" and cfg["checkpoint_budget_s"] == 600.0  # clamp ceiling (0.6.0)
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


def test_required_checkpoint_in_subagent_writes_the_chain_and_gates_only_facts(provider_factory):
    # 1.8.0: a fork under compression.checkpoint_required must be able to compact. The chain
    # is archived and labelled; facts/mirror/digests stay gated for cron/flush/subagent.
    p, client = provider_factory(agent_context="subagent")
    out = _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE, require_checkpoint=True)
    assert "ctx checkpoint head:" in out
    assert client.calls == [], "no fact/mirror writes in a subagent"
    assert len(client.chain_calls) == 3
    assert all(c["metadata"]["agent_context"] == "subagent" for c in client.chain_calls)


def test_required_checkpoint_raises_when_chain_disabled_and_writes_gated(provider_factory):
    p, _ = provider_factory({"checkpoint_chain": False}, agent_context="subagent")
    with pytest.raises(CtxCheckpointError, match="checkpoint_chain is false"):
        _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE, require_checkpoint=True)


@pytest.mark.parametrize(
    "evidence", [EVIDENCE, [{"role": "user", "content": "ok"}]], ids=["with-a-fact", "without-facts"]
)
def test_required_checkpoint_refuses_a_disabled_chain_although_facts_are_on(provider_factory, evidence):
    # Facts are not a transcript checkpoint: without the chain no word of the conversation is in
    # ctx, so the required checkpoint must not pass on facts (or on nothing) alone (#289).
    p, client = provider_factory({"checkpoint_chain": False, "facts_from_user": False})
    with pytest.raises(CtxCheckpointError, match="checkpoint_chain is false"):
        _manager(p).on_pre_compress(RAW, evidence_messages=evidence, require_checkpoint=True)
    assert client.calls == [] and client.chain_calls == []


# -- require_checkpoint=False stays async best effort ---------------------------------------


def test_best_effort_writes_the_chain_in_the_call_and_the_facts_on_a_thread(provider_factory):
    """Best effort writes the transcript chain (parts, manifest, head) synchronously, as the
    required mode does; only the facts go to the write thread and are stored after the call."""
    p, client = provider_factory({"facts_from_user": False})  # evidence = user turn
    gate = threading.Event()
    real_store = client.store

    def gated_fact_store(category, *a, **k):
        if category != "compaction-checkpoints":
            assert gate.wait(5.0), "the fact store was never released"
        return real_store(category, *a, **k)

    client.store = gated_fact_store
    out = _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE, require_checkpoint=False)
    assert "Facts extracted" in out
    assert any(c["title"].startswith("Compaction checkpoint head") for c in client.chain_calls), (
        "the chain head must be stored before the call returns"
    )
    assert client.calls == [], "the facts must not be stored synchronously"
    assert p._write_thread is not None and p._write_thread.is_alive()
    gate.set()
    join_writes(p)
    assert [c["content"] for c in client.calls] == [EVIDENCE_FACT]


@pytest.mark.parametrize("server", ["slow", "rate_limited"])
def test_best_effort_chain_gives_up_at_its_deadline(provider_factory, monkeypatch, caplog, server):
    """Best effort never waits without bound: every chain call gets the time left as its timeout,
    a refused write waits at most the one rate window the chain is granted, and the chain gives
    up at ``checkpoint_budget_s`` (+ that window). The compaction then goes on with the facts."""
    # A budget that is no multiple of the call timeout: the last call gets only the time left.
    p, client = provider_factory({"facts_from_user": False, "checkpoint_chunk_chars": 1_000, "checkpoint_budget_s": 15})
    budget = float(p._config["checkpoint_budget_s"])
    assert budget % float(p._config["api_timeout"]) != 0
    clock = {"now": 1_000.0}
    monkeypatch.setattr(p, "_now", lambda: clock["now"])

    def sleep(seconds):
        clock["now"] += seconds

    monkeypatch.setattr(p, "_chain_writer_seams", lambda: {"monotonic": lambda: clock["now"], "sleep": sleep})
    p._chain_writer = None
    timeouts: list[float] = []
    real_store = client.store

    def chain_store(category, *a, timeout=None, **k):
        if category != "compaction-checkpoints":
            return real_store(category, *a, **k)
        timeouts.append(timeout)
        if server == "rate_limited":
            raise CtxApiError("mcp store: Rate limit exceeded: max 100 writes per 60 seconds", status=429)
        clock["now"] += timeout  # a call that hangs ends at its timeout (McpClient abandons it)
        return real_store(category, *a, **k)

    client.store = chain_store
    long_turn = [*EVIDENCE, {"role": "assistant", "content": "Antwort mit Details zum Stand. " * 400}]
    started = clock["now"]
    with caplog.at_level(logging.WARNING, logger="ctx_plugin.provider"):
        out = _manager(p).on_pre_compress(RAW, evidence_messages=long_turn, require_checkpoint=False)
    elapsed = clock["now"] - started
    join_writes(p)
    assert "Facts extracted" in out
    assert any("chain failed" in r.getMessage() and "best_effort" in r.getMessage() for r in caplog.records)
    assert timeouts and all(t is not None and t > 0 for t in timeouts)
    if server == "slow":
        assert elapsed <= budget + 0.5, f"the chain ran {elapsed:.1f}s past a {budget:.0f}s budget"
        assert timeouts[-1] == pytest.approx(budget % float(p._config["api_timeout"])), timeouts
        assert not any(c["title"].startswith("Compaction checkpoint head") for c in client.chain_calls)
    else:
        bound = budget + RATE_WINDOW_S + RATE_WINDOW_MARGIN_S
        assert 0 < elapsed <= bound, f"a refused chain waited {elapsed:.1f}s, bound {bound:.0f}s"
        assert len(timeouts) == 2, "a write refused twice in a row ends the chain"
    assert EVIDENCE_FACT in [c["content"] for c in client.calls], "the facts still go after a failed chain"


def test_best_effort_failure_is_swallowed(provider_factory):
    p, client = provider_factory({"facts_from_user": False})  # evidence = user turn
    client.fail_on = {0}
    out = _manager(p).on_pre_compress(RAW, evidence_messages=EVIDENCE)
    join_writes(p)
    assert "Facts extracted" in out
    assert len(client.calls) == 1
    assert isinstance(FakeClient.instances[-1], FakeClient)
