# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
"""P3 (ctx-plugin-v2): transcript checkpoint chain head → manifest → parts over REST.

Part A ports the 32 upstream tests of GottZ/ctx ``integrations/hermes/plugin/
ctx_checkpoint/tests`` (test_provider_contract.py: 9, test_contract_negotiation.py: 23,
commit 38fdb74, MPL-2.0). Adaptation: the MCP dispatch fake (``mcp__ctx__search/get/
store``) becomes a REST ``ChainFake`` client (``search``/``get``/``store`` with REST
response shapes); the provider is ours (``CtxMemoryProvider``) with
``auto_compress_extract: false`` so the frame is the pure chain receipt. Deliberate
deviation: ``agent_context`` cron/subagent/flush stays write-gated (plugin rule since
1.0), so the upstream "label the fork" tests assert *no writes* for those contexts and
check the labelling with ``primary``/empty instead.

Part B: own tests (chain linking over two compactions, part splitting, golden body
format vs distiller ``parse.go``, redaction, budget, rate limit, part cap, gating,
receipt bound, wire payload).
"""

from __future__ import annotations

import importlib
import json
import logging
import re
import sys
import time

import agent.conversation_compression as host_compression
import agent.memory_provider as host_contract
import pytest
from agent.memory_manager import MemoryManager

import ctx_plugin.checkpoint as _MODULE
from ctx_plugin.client import CtxApiError

CtxCheckpointError = _MODULE.CtxCheckpointError
_HOST_FEATURE = "_tool_evidence_for_pre_compress_memory"
CHAIN_CAT = "compaction-checkpoints"


class ChainFake:
    """REST stand-in. ``calls`` = ordered (name, args) like the upstream dispatch log."""

    def __init__(self, base_url="", api_key="", timeout=10.0, synthesis_timeout=90.0, **_kw):
        self.calls: list = []
        self.ids = None  # iterator of ids for store; else counter
        self.counter = 50
        self.search_payload = {"success": True, "results": []}
        self.get_payload = None
        self.store_hook = None  # callable(args) -> None | raise
        self.stateful = False  # upsert by (category,title) + search/get over own blocks
        self.blocks: dict = {}  # title -> {"id", "metadata", ...}

    def whoami(self):
        return {"success": True, "home_scope": "shared", "label": "test-key"}

    def _next_id(self):
        if self.ids is not None:
            return next(self.ids)
        bid = f"019f6000-0000-7000-8000-{self.counter:012d}"
        self.counter += 1
        return bid

    def search(self, text, **k):
        self.calls.append(("search", {"query": text, **k}))
        if self.stateful:
            rows = [
                {"id": b["id"], "title": t, "type": "checkpoint", "untrusted": True}
                for t, b in self.blocks.items()
                if text in t
            ]
            return {"success": True, "results": rows}
        return self.search_payload

    def get_block(self, block_id, **k):
        self.calls.append(("get", {"id": block_id, **k}))
        if self.stateful:
            for t, b in self.blocks.items():
                if b["id"] == block_id:
                    return {"success": True, "block": {"id": block_id, "title": t, "metadata": b["metadata"]}}
            return {"success": False, "error": "not found"}
        return self.get_payload

    def store(self, category, title, content, sensitivity=None, tags=None, **k):
        args = {"category": category, "title": title, "content": content, "sensitivity": sensitivity, "tags": tags, **k}
        self.calls.append(("store", args))
        if self.store_hook is not None:
            self.store_hook(args)
        bid = self.blocks[title]["id"] if self.stateful and title in self.blocks else self._next_id()
        if self.stateful:
            self.blocks[title] = {"id": bid, "metadata": k.get("metadata") or {}}
        return {"success": True, "block": {"id": bid, "created_at": "t", "updated_at": "t"}}

    def query(self, text, **k):
        return {"success": True, "sources": []}


@pytest.fixture
def factory(monkeypatch, tmp_path):
    import ctx_plugin.provider as provider_mod

    monkeypatch.setattr(provider_mod, "CtxClient", ChainFake)
    monkeypatch.setenv("CTX_KEY", "test-key-not-real")
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx.invalid")
    made = []

    def _make(session_id="session-a", config=None, **init_kwargs):
        from ctx_plugin.provider import CtxMemoryProvider

        cfg = {"auto_compress_extract": False}
        cfg.update(config or {})
        home = tmp_path / "profiles" / f"p{len(made)}"
        home.mkdir(parents=True, exist_ok=True)
        (home / "ctx.json").write_text(json.dumps(cfg), encoding="utf-8")
        p = CtxMemoryProvider()
        p.initialize(session_id, hermes_home=str(home), **init_kwargs)
        assert p._active
        made.append(p)
        return p, p._client

    yield _make
    for p in made:
        p.shutdown()


def _names(calls):
    return [n for n, _ in calls]


def _stores(calls):
    return [a for n, a in calls if n == "store"]


# =============================================================================
# Part A — port of upstream test_provider_contract.py (9)
# =============================================================================


def test_provider_publishes_versioned_stable_head_with_bounded_frame(factory):
    source_id = "019f6000-0000-7000-8000-000000000041"
    manifest_id = "019f6000-0000-7000-8000-000000000042"
    head_id = "019f6000-0000-7000-8000-000000000043"
    provider, c = factory("root/session-a", platform="cli")
    c.ids = iter([source_id, manifest_id, head_id])

    frame = provider.on_pre_compress([{"role": "user", "content": "durable source decision"}])

    assert provider.pre_compress_checkpoint_api_version == 2
    assert _names(c.calls) == ["search", "store", "store", "store"]
    assert c.calls[3][1]["title"] == "Compaction checkpoint head root_session-a"
    assert manifest_id in c.calls[3][1]["content"]
    assert source_id in c.calls[2][1]["content"]
    assert head_id in frame and manifest_id in frame
    assert source_id not in frame
    assert len(frame) < 1_000


def test_provider_frame_stays_bounded_when_transcript_spans_many_source_blocks(factory):
    provider, c = factory("session-a", config={"checkpoint_chunk_chars": 1_000}, platform="cli")

    frame = provider.on_pre_compress([{"role": "user", "content": "durable " + "x" * 12_000}])

    assert len(c.calls) > 7
    source_parts = [call[1]["metadata"]["part"] for call in c.calls[1:-2]]
    assert source_parts == list(range(1, len(source_parts) + 1))
    assert len(frame) < 1_000
    assert "ctx source blocks:" not in frame


def test_provider_upserts_same_head_title_for_new_transcript_digest(factory):
    provider, c = factory("session-a", platform="cli")
    c.ids = iter(
        [
            "019f6000-0000-7000-8000-000000000061",
            "019f6000-0000-7000-8000-000000000062",
            "019f6000-0000-7000-8000-000000000063",
            "019f6000-0000-7000-8000-000000000064",
            "019f6000-0000-7000-8000-000000000065",
            "019f6000-0000-7000-8000-000000000063",
        ]
    )

    first = provider.on_pre_compress([{"role": "user", "content": "first durable decision"}])
    second = provider.on_pre_compress([{"role": "user", "content": "second corrected decision"}])

    assert c.calls[3][1]["title"] == c.calls[7][1]["title"]
    assert c.calls[2][1]["title"] != c.calls[6][1]["title"]
    assert "019f6000-0000-7000-8000-000000000062" in first
    assert "019f6000-0000-7000-8000-000000000065" in second
    assert "019f6000-0000-7000-8000-000000000065" in c.calls[7][1]["content"]


def test_memory_manager_accepts_provider_as_required_checkpoint_api_v2(factory):
    provider, c = factory("session-a", platform="cli")
    c.ids = iter(
        [
            "019f6000-0000-7000-8000-000000000071",
            "019f6000-0000-7000-8000-000000000072",
            "019f6000-0000-7000-8000-000000000073",
        ]
    )
    manager = MemoryManager()
    manager.add_provider(provider)

    assert manager.supports_pre_compress_checkpoint(1) is True
    frame = manager.on_pre_compress(
        [{"role": "user", "content": "manager integration evidence"}], require_checkpoint=True, checkpoint_api_version=2
    )

    assert "019f6000-0000-7000-8000-000000000073" in frame


def test_literal_prior_context_user_quote_is_preserved_as_source_evidence(factory):
    provider, c = factory("session-a", platform="cli")
    provider.on_pre_compress(
        [
            {
                "role": "user",
                "content": (
                    "[PRIOR CONTEXT — for reference only; not a new message] "
                    "is a literal marker I want to discuss, not a Hermes envelope"
                ),
            }
        ]
    )
    assert "literal marker I want to discuss" in c.calls[1][1]["content"]


def test_provider_links_new_manifest_to_previous_head_manifest_before_head_update(factory):
    previous_head_id = "019f6000-0000-7000-8000-000000000091"
    previous_manifest_id = "019f6000-0000-7000-8000-000000000092"
    provider, c = factory("session-a", platform="cli")
    c.ids = iter(["019f6000-0000-7000-8000-000000000093", "019f6000-0000-7000-8000-000000000094", previous_head_id])
    c.search_payload = {
        "success": True,
        "results": [{"id": previous_head_id, "title": "Compaction checkpoint head session-a", "type": "checkpoint"}],
    }
    c.get_payload = {
        "success": True,
        "block": {"id": previous_head_id, "metadata": {"latest_manifest_id": previous_manifest_id}},
    }

    provider.on_pre_compress([{"role": "user", "content": "new durable child checkpoint"}])

    assert _names(c.calls) == ["search", "get", "store", "store", "store"]
    manifest_args = c.calls[3][1]
    assert manifest_args["metadata"]["parent_manifest_id"] == previous_manifest_id
    assert previous_manifest_id in manifest_args["content"]
    assert "019f6000-0000-7000-8000-000000000094" in c.calls[4][1]["content"]


def _store_metadata_for(factory, **initialize_kwargs):
    provider, c = factory("session-fork", **initialize_kwargs)
    provider.on_pre_compress([{"role": "user", "content": "fork evidence worth archiving"}])
    return [a["metadata"] for a in _stores(c.calls)]


def test_subagent_fork_checkpoint_is_not_written_by_this_plugin(factory):
    # Upstream labels subagent forks; this plugin write-gates cron/flush/subagent (deviation).
    assert _store_metadata_for(factory, platform="cli", agent_context="subagent") == []
    metadata = _store_metadata_for(factory, platform="cli", agent_context="primary")
    assert len(metadata) == 3 and all(m["agent_context"] == "primary" for m in metadata)


def test_missing_agent_context_stays_empty_instead_of_claiming_primary(factory):
    metadata = _store_metadata_for(factory, platform="cli")
    assert len(metadata) == 3
    assert all(entry["agent_context"] == "" for entry in metadata)
    assert all(entry["platform"] == "cli" for entry in metadata)


def test_agent_context_and_platform_are_independent_metadata_keys(factory):
    metadata = _store_metadata_for(factory, platform="telegram", agent_context="primary")
    assert all(entry["platform"] == "telegram" for entry in metadata)
    assert all(entry["agent_context"] == "primary" for entry in metadata)
    assert _store_metadata_for(factory, platform="telegram", agent_context="cron") == []


# =============================================================================
# Part A — port of upstream test_contract_negotiation.py (23)
# =============================================================================


def _provider(factory):
    p, c = factory("session-negotiation", platform="cli")
    return p, c


def _simulate_v3_host(monkeypatch, *, with_feature=True):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", 3)
    if with_feature:
        monkeypatch.setattr(host_compression, _HOST_FEATURE, lambda messages: [], raising=False)
    elif hasattr(host_compression, _HOST_FEATURE):
        monkeypatch.delattr(host_compression, _HOST_FEATURE)


def _tool_evidence_warnings(caplog):
    return [r for r in caplog.records if r.levelno == logging.WARNING and "tool_evidence" in r.getMessage()]


def test_fork_host_is_a_pure_v2_host():
    assert host_contract.PRE_COMPRESS_CHECKPOINT_API_VERSION == 2
    assert not hasattr(host_compression, _HOST_FEATURE)


def test_host_read_path_int_getattr_on_instance_negotiates_2_against_fork_host(factory):
    provider, _ = _provider(factory)
    from ctx_plugin.provider import CtxMemoryProvider

    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2
    assert isinstance(provider.pre_compress_checkpoint_api_version, int)
    assert isinstance(CtxMemoryProvider.__dict__["pre_compress_checkpoint_api_version"], property)
    assert _MODULE._probe_host_checkpoint_api() == 2
    assert _MODULE.PLUGIN_CHECKPOINT_API_MAX == 2
    assert _MODULE.HOST_V3_FEATURE == _HOST_FEATURE


def test_probe_reports_3_on_simulated_v3_host_but_plugin_declares_its_own_max(factory, monkeypatch):
    _simulate_v3_host(monkeypatch)
    provider, _ = _provider(factory)
    assert _MODULE._probe_host_checkpoint_api() == 3
    assert _MODULE.PLUGIN_CHECKPOINT_API_MAX == 2
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2


def test_negotiation_flips_to_3_once_plugin_max_is_raised(factory, monkeypatch):
    _simulate_v3_host(monkeypatch)
    monkeypatch.setattr(_MODULE, "PLUGIN_CHECKPOINT_API_MAX", 3)
    provider, _ = _provider(factory)
    assert _MODULE._probe_host_checkpoint_api() == 3
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 3


def test_raised_plugin_max_alone_does_not_exceed_a_v2_host(factory, monkeypatch):
    monkeypatch.setattr(_MODULE, "PLUGIN_CHECKPOINT_API_MAX", 3)
    provider, _ = _provider(factory)
    assert _MODULE._probe_host_checkpoint_api() == 2
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2


def test_v3_constant_without_host_feature_probes_2(monkeypatch):
    _simulate_v3_host(monkeypatch, with_feature=False)
    assert _MODULE._probe_host_checkpoint_api() == 2


def test_v3_feature_present_but_not_callable_probes_2(monkeypatch):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", 3)
    monkeypatch.setattr(host_compression, _HOST_FEATURE, "not-callable", raising=False)
    assert _MODULE._probe_host_checkpoint_api() == 2


def test_v1_host_probes_1_but_plugin_still_declares_2(factory, monkeypatch):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", 1)
    provider, _ = _provider(factory)
    assert _MODULE._probe_host_checkpoint_api() == 1
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2


def test_probe_falls_back_to_2_when_host_module_entry_is_none(factory, monkeypatch):
    provider, _ = _provider(factory)
    monkeypatch.setitem(sys.modules, "agent.memory_provider", None)
    assert _MODULE._probe_host_checkpoint_api() == 2
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2


def test_probe_falls_back_to_2_when_host_module_name_does_not_exist(monkeypatch):
    monkeypatch.setattr(_MODULE, "_HOST_CONTRACT_MODULE", "agent.ctx_checkpoint_does_not_exist")
    assert _MODULE._probe_host_checkpoint_api() == 2


def test_probe_falls_back_to_2_when_feature_module_is_missing_on_v3_host(monkeypatch):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", 3)
    monkeypatch.setattr(_MODULE, "_HOST_FEATURE_MODULE", "agent.ctx_checkpoint_does_not_exist")
    assert _MODULE._probe_host_checkpoint_api() == 2


@pytest.mark.parametrize("bad_value", ["3", 3.0, None, True, [3]])
def test_probe_falls_back_to_2_on_non_int_constant(monkeypatch, bad_value):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", bad_value)
    assert _MODULE._probe_host_checkpoint_api() == 2


def test_probe_falls_back_to_2_when_constant_is_missing(monkeypatch):
    monkeypatch.delattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION")
    assert _MODULE._probe_host_checkpoint_api() == 2


def test_probe_swallows_exception_raised_during_import(factory, monkeypatch):
    provider, _ = _provider(factory)

    class _Exploding:
        @staticmethod
        def find_spec(name, path=None, target=None):
            if name == "agent.memory_provider":
                raise RuntimeError("simulated import failure")
            return None

    monkeypatch.delitem(sys.modules, "agent.memory_provider")
    monkeypatch.setattr(sys, "meta_path", [_Exploding(), *sys.meta_path])
    with pytest.raises(RuntimeError):
        importlib.import_module("agent.memory_provider")
    assert _MODULE._probe_host_checkpoint_api() == 2
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2


def test_tool_evidence_kwarg_is_accepted_and_labelled_received_unrendered(factory, caplog):
    provider, c = _provider(factory)
    evidence = [{"ordinal": 3, "tool": "read_file", "summary": "read README.md"}]
    with caplog.at_level(logging.WARNING):
        frame = provider.on_pre_compress(
            [{"role": "user", "content": "evidence with a tool index"}],
            tool_evidence=evidence,
            future_keyword="ignored",
        )
    stores = _stores(c.calls)
    assert len(stores) == 3
    assert all(a["metadata"]["tool_index_status"] == "received-unrendered" for a in stores)
    assert "read README.md" not in stores[0]["content"]
    assert len(frame) < 1_000


def test_tool_evidence_warning_fires_once_per_instance(factory, caplog):
    provider, c = _provider(factory)
    with caplog.at_level(logging.WARNING):
        provider.on_pre_compress([{"role": "user", "content": "first"}], tool_evidence=[{"ordinal": 1}])
        provider.on_pre_compress([{"role": "user", "content": "second"}], tool_evidence=[])
    assert len(_tool_evidence_warnings(caplog)) == 1
    assert all(a["metadata"]["tool_index_status"] == "received-unrendered" for a in _stores(c.calls))
    other, _ = _provider(factory)
    with caplog.at_level(logging.WARNING):
        other.on_pre_compress([{"role": "user", "content": "third"}], tool_evidence=[{"ordinal": 2}])
    assert len(_tool_evidence_warnings(caplog)) == 2


def test_without_tool_evidence_status_is_absent_and_silent(factory, caplog):
    provider, c = _provider(factory)
    with caplog.at_level(logging.WARNING):
        provider.on_pre_compress([{"role": "user", "content": "plain prose"}])
    stores = _stores(c.calls)
    assert len(stores) == 3
    assert all(a["metadata"]["tool_index_status"] == "absent" for a in stores)
    assert _tool_evidence_warnings(caplog) == []


def test_metadata_carries_negotiated_contract_on_part_manifest_and_head(factory):
    provider, c = _provider(factory)
    provider.on_pre_compress([{"role": "user", "content": "negotiated contract"}])
    part, manifest, head = _stores(c.calls)
    assert part["title"].endswith("part 001 of 001")
    assert manifest["title"].endswith("manifest")
    assert head["title"] == "Compaction checkpoint head session-negotiation"
    for args in (part, manifest, head):
        md = args["metadata"]
        assert md["host_checkpoint_api"] == 2 and md["checkpoint_api_version"] == 2
        assert md["tool_index_status"] == "absent" and md["checkpoint_api_version"] != 1
    assert "- Checkpoint API version: 2\n" in head["content"]
    assert "Checkpoint API version: 1" not in head["content"]


def test_head_reflects_negotiated_3_on_v3_host_with_raised_plugin_max(factory, monkeypatch):
    _simulate_v3_host(monkeypatch)
    monkeypatch.setattr(_MODULE, "PLUGIN_CHECKPOINT_API_MAX", 3)
    provider, c = _provider(factory)
    provider.on_pre_compress([{"role": "user", "content": "v3 negotiated"}])
    part, manifest, head = _stores(c.calls)
    for args in (part, manifest, head):
        assert args["metadata"]["host_checkpoint_api"] == 3
        assert args["metadata"]["checkpoint_api_version"] == 3
    assert "- Checkpoint API version: 3\n" in head["content"]


# =============================================================================
# Part B — own tests
# =============================================================================

UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")
# go/internal/distillsource/ctxcheckpoint/parse.go header (### Message <digits> — user|assistant)
PARSE_GO_HEADER = re.compile(r"^### Message (\d+) — (user|assistant)$", re.M)


def test_two_compactions_one_head_two_linked_manifests(factory):
    """Server-side lookup (stateful fake): 1 head upserted, manifest 2 → manifest 1."""
    p, c = factory("sess-chain", platform="cli", agent_context="primary")
    c.stateful = True
    p.on_pre_compress([{"role": "user", "content": "erste Kompaktierung"}])
    p._last_manifest_by_root.clear()  # force the server path for compaction 2
    p.on_pre_compress([{"role": "user", "content": "zweite Kompaktierung"}])
    stores = _stores(c.calls)
    heads = [a for a in stores if a["title"].startswith("Compaction checkpoint head")]
    manifests = [a for a in stores if a["title"].endswith(" manifest")]
    assert len({h["title"] for h in heads}) == 1 and len(heads) == 2
    assert len(c.blocks) == 1 + 2 + 2  # one head block, 2 manifests, 2 parts
    m1_id = c.blocks[manifests[0]["title"]]["id"]
    m2_id = c.blocks[manifests[1]["title"]]["id"]
    assert manifests[0]["metadata"]["parent_manifest_id"] is None
    assert manifests[1]["metadata"]["parent_manifest_id"] == m1_id
    assert f"- Parent manifest: `{m1_id}`" in manifests[1]["content"]
    head_md = c.blocks[heads[0]["title"]]["metadata"]
    assert head_md["latest_manifest_id"] == m2_id
    assert _names(c.calls).count("get") == 1
    for a in stores:
        assert a["type"] == "checkpoint" and a["category"] == CHAIN_CAT and a["sensitivity"] == "personal"
        assert "provenance" not in a["metadata"]
        assert a["metadata"]["ctx_plugin"]["writer"] == "hermes-ctx-plugin"
        assert a["metadata"]["root_session_id"] == "sess-chain"
        assert "source:hermes" in a["tags"] and "hook:checkpoint" in a["tags"]
        assert "parent_id" not in a, "chain lives in metadata (FK CASCADE on parent_id)"


def test_cache_links_chain_when_server_search_misses(factory, caplog):
    p, c = factory("sess-cache", agent_context="primary")
    p.on_pre_compress([{"role": "user", "content": "eins"}])
    p.on_pre_compress([{"role": "user", "content": "zwei"}])
    manifests = [a for a in _stores(c.calls) if a["title"].endswith(" manifest")]
    first_manifest_id = "019f6000-0000-7000-8000-000000000051"
    assert manifests[1]["metadata"]["parent_manifest_id"] == first_manifest_id


def test_session_switch_keeps_root_unless_reset(factory):
    p, c = factory("root-1", agent_context="primary")
    p.on_session_switch("child-2", parent_session_id="root-1")
    p.on_pre_compress([{"role": "user", "content": "nach kompaktierung"}])
    head = _stores(c.calls)[-1]
    assert head["title"] == "Compaction checkpoint head root-1"
    assert head["metadata"]["active_session_id"] == "child-2"
    p.on_session_switch("fresh-3", reset=True)
    p.on_pre_compress([{"role": "user", "content": "neue session"}])
    assert _stores(c.calls)[-1]["title"] == "Compaction checkpoint head fresh-3"


def test_part_splitting_at_message_boundaries_and_40k_cap(factory):
    p, c = factory("sess-split", config={"checkpoint_chunk_chars": 99_999}, agent_context="primary")
    msgs = [
        {"role": "user" if i % 2 == 0 else "assistant", "content": f"Nachricht {i}: " + "w" * 9_000} for i in range(12)
    ]
    p.on_pre_compress(msgs)
    parts = [a for a in _stores(c.calls) if " part " in a["title"]]
    assert len(parts) >= 3
    n = len(parts)
    rendered = _MODULE.render_direct_evidence(msgs)
    bodies = []
    for i, a in enumerate(parts, start=1):
        assert a["title"].endswith(f"part {i:03d} of {n:03d}")
        body = a["content"].split("## Direct transcript\n\n", 1)[1]
        assert len(body) <= _MODULE.MAX_CHUNK_CHARS
        assert body.startswith("### Message "), "split must land on a message boundary"
        assert a["metadata"]["part"] == i and a["metadata"]["parts"] == n
        bodies.append(body)
    # lossless: concatenated parts == rendered transcript (modulo the boundary whitespace)
    assert re.sub(r"\s+", "", "".join(bodies)) == re.sub(r"\s+", "", rendered)
    manifest = _stores(c.calls)[-2]
    assert manifest["metadata"]["source_block_ids"] == [f"019f6000-0000-7000-8000-{50 + i:012d}" for i in range(n)]


def test_hard_cut_when_single_message_exceeds_chunk():
    chunks = _MODULE.split_chunks("### Message 1 — user\n\n" + "z" * 5_000, 1_000)
    assert len(chunks) == 6 and all(len(ch) <= 1_000 for ch in chunks)
    assert _MODULE.clamp_chunk_chars(10) == 1_000 and _MODULE.clamp_chunk_chars(10**6) == 40_000


def test_golden_part_format_matches_distiller_parser(factory):
    p, c = factory("sess-golden", agent_context="primary")
    p.on_pre_compress(
        [
            {"role": "user", "content": "Frage A"},
            {"role": "tool", "content": "TOOLROW-MARKER must not appear"},
            {"role": "assistant", "content": "Antwort B"},
            {"role": "system", "content": "SYSPROMPT-MARKER"},
            {"role": "assistant", "content": [{"type": "text", "text": "Liste C"}]},
        ]
    )
    part = _stores(c.calls)[0]
    assert part["title"].startswith("Compaction source sess-golden ")
    headers = PARSE_GO_HEADER.findall(part["content"])
    assert headers == [("1", "user"), ("3", "assistant"), ("5", "assistant")]
    assert "TOOLROW-MARKER" not in part["content"] and "SYSPROMPT-MARKER" not in part["content"]
    assert part["content"].startswith("# Compaction source evidence\n\nThis is deterministic, redacted source evidence")
    assert "- Part: 1/1\n\n## Direct transcript\n\n### Message 1 — user\n\nFrage A\n" in part["content"]


def test_synthetic_handoffs_and_summaries_excluded(factory):
    p, c = factory("sess-synth", agent_context="primary")
    p.on_pre_compress(
        [
            {"role": "user", "content": "summary carrier", "_compressed_summary": True},
            {"role": "assistant", "content": "[CONTEXT COMPACTION — REFERENCE ONLY] old summary"},
            {"role": "assistant", "content": "<memory-context>recall</memory-context>"},
            {"role": "user", "content": "echte Nachricht"},
        ]
    )
    body = _stores(c.calls)[0]["content"]
    assert "summary carrier" not in body and "old summary" not in body and "recall" not in body
    assert "### Message 4 — user\n\nechte Nachricht" in body


SECRETS = [
    "api_key=AbCdEf0123456789xyzSECRET",
    "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig1234567890",
    "password: hunter2-very-secret",
    "sk-proj-Abcdefghijklmnopqrstuvwxyz0123456789ABCD",
]


def test_redaction_host_and_upstream_rules(factory):
    p, c = factory("sess-redact", agent_context="primary")
    p.on_pre_compress([{"role": "user", "content": "Bitte merken:\n" + "\n".join(SECRETS)}])
    body = _stores(c.calls)[0]["content"]
    for leaked in (
        "AbCdEf0123456789xyzSECRET",
        "eyJhbGciOiJIUzI1NiJ9.payload",
        "hunter2-very-secret",
        "sk-proj-Abcdefghijklmnopqrstuvwxyz0123456789ABCD",
    ):
        assert leaked not in body
    assert "Bitte merken" in body


def test_redaction_failure_fails_closed(factory, monkeypatch):
    def boom(text):
        raise RuntimeError("redactor down")

    monkeypatch.setattr(_MODULE, "_host_redact", boom)
    p, c = factory("sess-redfail", agent_context="primary")
    assert p.on_pre_compress([{"role": "user", "content": "secret-ish"}]) == ""  # best effort: no raise
    assert _stores(c.calls) == []
    with pytest.raises(CtxCheckpointError):
        p.on_pre_compress([{"role": "user", "content": "secret-ish"}], require_checkpoint=True)
    assert _stores(c.calls) == []


def test_budget_exceeded_stops_before_head(factory, caplog):
    p, c = factory("sess-budget", config={"checkpoint_chunk_chars": 1_000}, agent_context="primary")
    p._config["checkpoint_budget_s"] = 0.25
    c.store_hook = lambda args: time.sleep(0.1)
    with pytest.raises(CtxCheckpointError, match="budget"):
        p.on_pre_compress([{"role": "user", "content": "x" * 6_000}], require_checkpoint=True)
    titles = [a["title"] for a in _stores(c.calls)]
    assert titles and not any(t.startswith("Compaction checkpoint head") for t in titles)
    assert not any(t.endswith(" manifest") for t in titles)
    # best effort: same failure is a WARNING, compaction continues, no exception
    c.calls.clear()
    with caplog.at_level(logging.WARNING, logger="ctx_plugin.provider"):
        assert p.on_pre_compress([{"role": "user", "content": "y" * 6_000}]) == ""
    assert any("chain failed" in r.getMessage() and "budget" in r.getMessage() for r in caplog.records)


def test_budget_shared_with_required_facts(factory):
    p, c = factory("sess-budget2", config={"auto_compress_extract": True}, agent_context="primary")
    p._config["checkpoint_budget_s"] = 0.0
    with pytest.raises(CtxCheckpointError):
        p.on_pre_compress(
            [{"role": "user", "content": "Der Proxy auf INFRA-WWW terminiert TLS fuer alle Dienste mit Traefik."}],
            require_checkpoint=True,
        )
    assert _stores(c.calls) == []


def _rate_limited_store(fail_times):
    state = {"n": 0}

    def hook(args):
        if state["n"] < fail_times:
            state["n"] += 1
            raise CtxApiError("HTTP 429 [rate_limit]: max 100 writes", status=429, code="rate_limit")

    return hook


def test_rate_limit_backs_off_and_completes(factory):
    p, c = factory("sess-rl", agent_context="primary")
    slept = []
    p._chain().sleep = slept.append
    c.store_hook = _rate_limited_store(2)
    frame = p.on_pre_compress([{"role": "user", "content": "rate limited write"}], require_checkpoint=True)
    assert slept == [2.0, 4.0]
    assert "ctx checkpoint head:" in frame
    assert len([a for a in _stores(c.calls)]) == 3 + 2  # two refused attempts + 3 stored


def test_rate_limit_persistent_fails_without_head(factory, caplog):
    p, c = factory("sess-rl2", agent_context="primary")
    slept = []
    p._chain().sleep = slept.append
    c.store_hook = _rate_limited_store(99)
    with pytest.raises(CtxCheckpointError, match="rate-limited"):
        p.on_pre_compress([{"role": "user", "content": "rate limited write"}], require_checkpoint=True)
    assert slept == list(_MODULE.RATE_LIMIT_BACKOFF_S)
    assert not any(a["title"].startswith("Compaction checkpoint head") for a in _stores(c.calls))


def test_rate_limit_backoff_never_sleeps_past_budget(factory):
    p, c = factory("sess-rl3", agent_context="primary")
    p._config["checkpoint_budget_s"] = 3.0
    slept = []
    p._chain().sleep = slept.append
    c.store_hook = _rate_limited_store(99)
    with pytest.raises(CtxCheckpointError):
        p.on_pre_compress([{"role": "user", "content": "x"}], require_checkpoint=True)
    assert slept == [2.0], "4 s backoff would overrun the 3 s budget"


def test_part_cap_refuses_before_any_request(factory):
    p, c = factory(
        "sess-cap", config={"checkpoint_chunk_chars": 1_000, "checkpoint_max_parts": 2}, agent_context="primary"
    )
    with pytest.raises(CtxCheckpointError, match="checkpoint_max_parts"):
        p.on_pre_compress([{"role": "user", "content": "q" * 5_000}], require_checkpoint=True)
    assert c.calls == []


def test_permanent_rejection_is_not_retried(factory):
    p, c = factory("sess-rej", agent_context="primary")

    def hook(args):
        raise CtxApiError("HTTP 403 [reserved_metadata]: no", status=403, code="reserved_metadata")

    c.store_hook = hook
    with pytest.raises(CtxCheckpointError, match="reserved_metadata"):
        p.on_pre_compress([{"role": "user", "content": "x"}], require_checkpoint=True)
    assert len(_stores(c.calls)) == 1


@pytest.mark.parametrize("ctx_name", ["cron", "flush", "subagent"])
def test_no_chain_writes_in_gated_agent_contexts(factory, ctx_name):
    p, c = factory("sess-gate", agent_context=ctx_name)
    assert p.on_pre_compress([{"role": "user", "content": "x"}]) == ""
    assert c.calls == []


def test_checkpoint_chain_switch_independent_of_facts(factory):
    # dev-profile shape: facts off, chain on (default) → chain written
    p, c = factory("sess-dev", config={"auto_compress_extract": False}, agent_context="primary")
    assert "ctx checkpoint head:" in p.on_pre_compress([{"role": "user", "content": "dev chain"}])
    assert len(_stores(c.calls)) == 3
    # chain off, facts on → no chain traffic at all
    p2, c2 = factory(
        "sess-dev2", config={"checkpoint_chain": False, "auto_compress_extract": True}, agent_context="primary"
    )
    p2.on_pre_compress(
        [{"role": "user", "content": "Der Proxy auf INFRA-WWW terminiert TLS fuer alle Dienste mit Traefik."}]
    )
    p2.shutdown()
    assert all(a["category"] != CHAIN_CAT for a in _stores(c2.calls))
    assert not any(n == "search" for n, _ in c2.calls)
    # both off → required raises, best effort silent
    p3, _c3 = factory("sess-dev3", config={"checkpoint_chain": False}, agent_context="primary")
    assert p3.on_pre_compress([{"role": "user", "content": "x"}]) == ""
    with pytest.raises(CtxCheckpointError):
        p3.on_pre_compress([{"role": "user", "content": "x"}], require_checkpoint=True)


def test_checkpoint_mode_off_writes_no_chain(factory):
    p, c = factory("sess-off", config={"checkpoint_mode": "off"}, agent_context="primary")
    assert p.on_pre_compress([{"role": "user", "content": "x"}]) == ""
    assert c.calls == []


def test_best_effort_chain_failure_keeps_facts(factory, caplog):
    p, c = factory("sess-be", config={"auto_compress_extract": True, "facts_from_user": False}, agent_context="primary")

    def hook(args):
        if args["category"] == CHAIN_CAT:
            raise CtxApiError("connection error: simulated")

    c.store_hook = hook
    with caplog.at_level(logging.WARNING, logger="ctx_plugin.provider"):
        out = p.on_pre_compress(
            [{"role": "user", "content": "Der Proxy auf INFRA-WWW terminiert TLS fuer alle Dienste mit Traefik."}]
        )
    p.shutdown()
    assert "Facts extracted" in out and "ctx checkpoint head" not in out
    assert any(a["category"] == "learnings" for a in _stores(c.calls))
    assert any("chain failed" in r.getMessage() for r in caplog.records)


def test_empty_evidence_noop_receipt_no_writes(factory):
    # P6 deviation: upstream raises on empty direct evidence; we return a no-op
    # receipt so fail-closed (compression.checkpoint_required) never blocks a
    # compaction that has nothing to archive. Nothing is written either way.
    p, c = factory("sess-empty", agent_context="primary")
    out = p.on_pre_compress([{"role": "tool", "content": "only tool"}], require_checkpoint=True)
    assert "empty" in out and "nothing to archive" in out
    assert c.calls == []


def test_chaos_ctx_down_required_blocks_compaction(factory):
    # P6 chaos test: CTX unreachable during a required checkpoint must raise
    # (the host then blocks compaction with BLOCKED_MISSING_PREREQUISITE and
    # keeps the uncompressed transcript) and must not publish a head.
    p, c = factory("sess-chaos", agent_context="primary")

    def _boom(args):
        raise ConnectionError("CTX unreachable (chaos test)")

    c.store_hook = _boom
    msgs = [{"role": "user", "content": "Wichtige Anweisung"}, {"role": "assistant", "content": "Erledigt."}]
    with pytest.raises(CtxCheckpointError, match="failed: ConnectionError"):
        p.on_pre_compress(msgs, require_checkpoint=True)
    # the failing part store was attempted, but no manifest/head was published
    stores = [x for x in c.calls if x[0] == "store"]
    assert len(stores) == 1 and "source" in stores[0][1]["title"]


def test_required_receipt_contains_chain_and_facts_and_stays_under_host_cap(factory):
    p, _c = factory("sess-rcpt", config={"auto_compress_extract": True}, agent_context="primary")
    msgs = [
        {
            "role": "assistant",
            "content": f"Fakt {i}: Der Dienst {i} auf INFRA-HOST-{i} nutzt Port {8000 + i} fuer die interne API "
            "mit TLS.",
        }
        for i in range(40)
    ]
    receipt = p.on_pre_compress(msgs, require_checkpoint=True)
    assert receipt.startswith("ctx checkpoint head: `")
    assert "CTX pre-compress receipt" in receipt
    assert len(receipt) < 6000


def test_wire_payload_of_real_client(monkeypatch):
    from ctx_plugin import client as client_mod
    from ctx_plugin.checkpoint import ChainWriter

    sent = []

    class _Resp:
        def __init__(self, body):
            self._b = json.dumps(body).encode()

        def read(self):
            return self._b

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    n = {"i": 0}

    def fake_urlopen(req, timeout=None):
        body = json.loads(req.data) if req.data else None
        sent.append({"url": req.full_url, "body": body, "timeout": timeout})
        if req.full_url.endswith("/api/search"):
            return _Resp({"success": True, "results": []})
        n["i"] += 1
        return _Resp({"success": True, "block": {"id": f"019f6000-0000-7000-8000-{n['i']:012d}"}})

    monkeypatch.setattr(client_mod.urllib.request, "urlopen", fake_urlopen)
    w = ChainWriter(client=client_mod.CtxClient("http://ctx.invalid", "k"), sensitivity="personal", api_timeout=10.0)
    r = w.write([{"role": "user", "content": "wire"}], root_session_id="r1", session_id="r1", budget_s=20)
    assert [s["url"].rsplit("/", 1)[1] for s in sent] == ["search", "store", "store", "store"]
    assert sent[0]["body"] == {
        "query": "Compaction checkpoint head r1",
        "category": CHAIN_CAT,
        "types": ["checkpoint"],
        "limit": 10,
    }
    for s in sent[1:]:
        b = s["body"]
        assert b["type"] == "checkpoint" and b["sensitivity"] == "personal" and b["category"] == CHAIN_CAT
        assert "provenance" not in b["metadata"] and "parent_id" not in b
        assert 0 < s["timeout"] <= 10.0
    assert r.head_id.endswith("3") and UUID.match(r.manifest_id)


def test_config_defaults_and_coercion(tmp_path):
    from ctx_plugin.config import load_ctx_config

    cfg = load_ctx_config(str(tmp_path))
    assert cfg["checkpoint_chain"] is True and cfg["checkpoint_sensitivity"] == "personal"
    assert cfg["checkpoint_chunk_chars"] == 36_000 and cfg["checkpoint_max_parts"] == 80
    (tmp_path / "ctx.json").write_text(
        json.dumps(
            {
                "checkpoint_chain": "false",
                "checkpoint_sensitivity": "bogus",
                "checkpoint_chunk_chars": 999_999,
                "checkpoint_max_parts": 500,
            }
        )
    )
    cfg = load_ctx_config(str(tmp_path))
    assert cfg["checkpoint_chain"] is False and cfg["checkpoint_sensitivity"] == "personal"
    assert cfg["checkpoint_chunk_chars"] == 40_000 and cfg["checkpoint_max_parts"] == 95
