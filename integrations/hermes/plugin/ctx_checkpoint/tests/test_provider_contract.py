import importlib.util
import json
from pathlib import Path

from agent.memory_manager import MemoryManager


_PROVIDER_PATH = Path(__file__).resolve().parents[1] / "__init__.py"
_SPEC = importlib.util.spec_from_file_location("ctx_checkpoint_provider_contract", _PROVIDER_PATH)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_MODULE)
CtxCheckpointMemoryProvider = _MODULE.CtxCheckpointMemoryProvider


def test_provider_publishes_versioned_stable_head_with_bounded_frame():
    calls = []
    source_id = "019f6000-0000-7000-8000-000000000041"
    manifest_id = "019f6000-0000-7000-8000-000000000042"
    head_id = "019f6000-0000-7000-8000-000000000043"
    ids = iter([source_id, manifest_id, head_id])

    def dispatch(name, args, **kwargs):
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return json.dumps({"result": "[]"})
        return json.dumps({"id": next(ids)})

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("root/session-a", platform="cli")

    frame = provider.on_pre_compress(
        [{"role": "user", "content": "durable source decision"}]
    )

    assert provider.pre_compress_checkpoint_api_version == 2
    assert [name for name, _ in calls] == [
        "mcp__ctx__search",
        "mcp__ctx__store",
        "mcp__ctx__store",
        "mcp__ctx__store",
    ]
    # "/" is sanitized away, so the title carries a hash of the full root id (0.5.2).
    assert calls[3][1]["title"] == (
        "Compaction checkpoint head " + _MODULE._root_title_token("root/session-a")
    )
    assert calls[3][1]["title"].startswith("Compaction checkpoint head root_session-a-")
    assert manifest_id in calls[3][1]["content"]
    assert source_id in calls[2][1]["content"]
    assert head_id in frame
    assert manifest_id in frame
    assert source_id not in frame
    assert len(frame) < 1_000


def test_provider_frame_stays_bounded_when_transcript_spans_many_source_blocks():
    calls = []
    counter = 50

    def dispatch(name, args, **kwargs):
        nonlocal counter
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return json.dumps({"result": "[]"})
        block_id = f"019f6000-0000-7000-8000-{counter:012d}"
        counter += 1
        return json.dumps({"id": block_id})

    provider = CtxCheckpointMemoryProvider(
        config={"chunk_chars": 1_000},
        dispatch=dispatch,
    )
    provider.initialize("session-a", platform="cli")

    frame = provider.on_pre_compress(
        [{"role": "user", "content": "durable " + "x" * 12_000}]
    )

    assert len(calls) > 7
    source_parts = [call[1]["metadata"]["part"] for call in calls[1:-2]]
    assert source_parts == list(range(1, len(source_parts) + 1))
    assert len(frame) < 1_000
    assert "ctx source blocks:" not in frame


def test_provider_upserts_same_head_title_for_new_transcript_digest():
    calls = []
    ids = iter(
        [
            "019f6000-0000-7000-8000-000000000061",
            "019f6000-0000-7000-8000-000000000062",
            "019f6000-0000-7000-8000-000000000063",
            "019f6000-0000-7000-8000-000000000064",
            "019f6000-0000-7000-8000-000000000065",
            "019f6000-0000-7000-8000-000000000063",
        ]
    )

    def dispatch(name, args, **kwargs):
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return json.dumps({"result": "[]"})
        return json.dumps({"id": next(ids)})

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("session-a", platform="cli")

    first = provider.on_pre_compress(
        [{"role": "user", "content": "first durable decision"}]
    )
    second = provider.on_pre_compress(
        [{"role": "user", "content": "second corrected decision"}]
    )

    assert calls[3][1]["title"] == calls[7][1]["title"]
    assert calls[2][1]["title"] != calls[6][1]["title"]
    assert "019f6000-0000-7000-8000-000000000062" in first
    assert "019f6000-0000-7000-8000-000000000065" in second
    assert "019f6000-0000-7000-8000-000000000065" in calls[7][1]["content"]


def test_memory_manager_accepts_provider_as_required_checkpoint_api_v2():
    ids = iter(
        [
            "019f6000-0000-7000-8000-000000000071",
            "019f6000-0000-7000-8000-000000000072",
            "019f6000-0000-7000-8000-000000000073",
        ]
    )

    def dispatch(name, args, **kwargs):
        if name == "mcp__ctx__search":
            return json.dumps({"result": "[]"})
        return json.dumps({"id": next(ids)})

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("session-a", platform="cli")
    manager = MemoryManager()
    manager.add_provider(provider)

    assert manager.supports_pre_compress_checkpoint(1) is True
    frame = manager.on_pre_compress(
        [{"role": "user", "content": "manager integration evidence"}],
        require_checkpoint=True,
        checkpoint_api_version=2,
    )

    assert "019f6000-0000-7000-8000-000000000073" in frame


def test_literal_prior_context_user_quote_is_preserved_as_source_evidence():
    calls = []
    ids = iter(
        [
            "019f6000-0000-7000-8000-000000000081",
            "019f6000-0000-7000-8000-000000000082",
            "019f6000-0000-7000-8000-000000000083",
        ]
    )

    def dispatch(name, args, **kwargs):
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return json.dumps({"result": "[]"})
        return json.dumps({"id": next(ids)})

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("session-a", platform="cli")

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

    assert "literal marker I want to discuss" in calls[1][1]["content"]


def test_provider_links_new_manifest_to_previous_head_manifest_before_head_update():
    calls = []
    previous_head_id = "019f6000-0000-7000-8000-000000000091"
    previous_manifest_id = "019f6000-0000-7000-8000-000000000092"
    ids = iter(
        [
            "019f6000-0000-7000-8000-000000000093",
            "019f6000-0000-7000-8000-000000000094",
            previous_head_id,
        ]
    )

    def dispatch(name, args, **kwargs):
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return json.dumps(
                {
                    "result": json.dumps(
                        [
                            {
                                "id": previous_head_id,
                                "title": "Compaction checkpoint head session-a",
                            }
                        ]
                    )
                }
            )
        if name == "mcp__ctx__get":
            return json.dumps(
                {
                    "id": previous_head_id,
                    "metadata": {"latest_manifest_id": previous_manifest_id},
                }
            )
        return json.dumps({"id": next(ids)})

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("session-a", platform="cli")
    provider.on_pre_compress(
        [{"role": "user", "content": "new durable child checkpoint"}]
    )

    assert [name for name, _ in calls] == [
        "mcp__ctx__search",
        "mcp__ctx__get",
        "mcp__ctx__store",
        "mcp__ctx__store",
        "mcp__ctx__store",
    ]
    manifest_args = calls[3][1]
    assert manifest_args["metadata"]["parent_manifest_id"] == previous_manifest_id
    assert previous_manifest_id in manifest_args["content"]
    assert "019f6000-0000-7000-8000-000000000094" in calls[4][1]["content"]


def _store_metadata_for(**initialize_kwargs):
    """Return the metadata of every block written for one checkpoint run."""
    calls = []
    counter = 100

    def dispatch(name, args, **kwargs):
        nonlocal counter
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return json.dumps({"result": "[]"})
        block_id = f"019f6000-0000-7000-8000-{counter:012d}"
        counter += 1
        return json.dumps({"id": block_id})

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("session-fork", **initialize_kwargs)
    provider.on_pre_compress(
        [{"role": "user", "content": "fork evidence worth archiving"}]
    )
    return [args["metadata"] for name, args in calls if name == "mcp__ctx__store"]


def test_subagent_fork_checkpoint_is_labelled_in_every_block_metadata():
    metadata = _store_metadata_for(platform="cli", agent_context="subagent")

    assert len(metadata) == 3
    assert all(entry["agent_context"] == "subagent" for entry in metadata)
    assert all(entry["platform"] == "cli" for entry in metadata)


def test_missing_agent_context_stays_empty_instead_of_claiming_primary():
    metadata = _store_metadata_for(platform="cli")

    assert len(metadata) == 3
    assert all(entry["agent_context"] == "" for entry in metadata)
    assert all(entry["platform"] == "cli" for entry in metadata)


def test_agent_context_and_platform_are_independent_metadata_keys():
    metadata = _store_metadata_for(platform="telegram", agent_context="cron")

    assert all(entry["platform"] == "telegram" for entry in metadata)
    assert all(entry["agent_context"] == "cron" for entry in metadata)


# ---------------------------------------------------------------------------
# 0.5.1 — port-backs from the hermes-ctx-plugin review (W4-NERF, 2026-10-31)
# ---------------------------------------------------------------------------


def _counting_dispatch(calls, *, store_results=None):
    """Dispatch fake: empty head search, then one block id per store (or a scripted result)."""
    counter = 200
    scripted = list(store_results or [])

    def dispatch(name, args, **kwargs):
        nonlocal counter
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return json.dumps({"result": "[]"})
        if scripted:
            return scripted.pop(0)
        block_id = f"019f6000-0000-7000-8000-{counter:012d}"
        counter += 1
        return json.dumps({"id": block_id})

    return dispatch


def test_authorization_header_credential_is_fully_redacted():
    redact = _MODULE._redact_checkpoint_secrets
    for scheme in ("Bearer", "Basic", "Digest"):
        out = redact(f"curl -H 'Authorization: {scheme} sk-live-ABCDEFGH123456'")
        assert "sk-live-ABCDEFGH123456" not in out, out
        assert "ABCDEFGH" not in out, out
    assert redact("token: xyz") == "token: [REDACTED]"
    # A sentence that merely mentions the word stays readable.
    assert redact("the authorization flow is documented") == "the authorization flow is documented"


def test_every_checkpoint_write_declares_the_checkpoint_type():
    calls = []
    provider = CtxCheckpointMemoryProvider(dispatch=_counting_dispatch(calls))
    provider.initialize("session-a", platform="cli")
    provider.on_pre_compress([{"role": "user", "content": "typed evidence"}])
    stores = [args for name, args in calls if name == "mcp__ctx__store"]
    assert len(stores) == 3
    assert all(args["type"] == "checkpoint" for args in stores)


def test_batch_without_direct_evidence_returns_empty_receipt_and_writes_nothing():
    calls = []
    provider = CtxCheckpointMemoryProvider(dispatch=_counting_dispatch(calls))
    provider.initialize("session-a", platform="cli")
    frame = provider.on_pre_compress(
        [
            {"role": "tool", "content": "exit 0"},
            {"role": "assistant", "content": "", "tool_calls": [{"id": "x"}]},
            {"role": "system", "content": "prompt"},
            {"role": "assistant", "content": "[CONTEXT COMPACTION — REFERENCE ONLY] summary"},
        ]
    )
    assert frame == ""
    assert calls == []


def test_memory_manager_accepts_empty_evidence_under_required_checkpoint():
    calls = []
    provider = CtxCheckpointMemoryProvider(dispatch=_counting_dispatch(calls))
    provider.initialize("session-a", platform="cli")
    manager = MemoryManager()
    manager.add_provider(provider)
    frame = manager.on_pre_compress(
        [{"role": "tool", "content": "only tool rows here"}],
        require_checkpoint=True,
        checkpoint_api_version=2,
    )
    assert frame == ""
    assert calls == []


def test_redaction_failure_raises_instead_of_being_mistaken_for_empty_evidence(monkeypatch):
    calls = []
    provider = CtxCheckpointMemoryProvider(dispatch=_counting_dispatch(calls))
    provider.initialize("session-a", platform="cli")

    def broken(text, force=False):
        raise RuntimeError("redactor down")

    monkeypatch.setattr(_MODULE, "redact_sensitive_text", broken)
    import pytest

    with pytest.raises(_MODULE.CtxCheckpointError, match="redaction failed"):
        provider.on_pre_compress([{"role": "user", "content": "secret-ish evidence"}])
    assert calls == []


def test_more_parts_than_max_parts_writes_nothing():
    import pytest

    calls = []
    provider = CtxCheckpointMemoryProvider(
        config={"chunk_chars": 1_000, "max_parts": 3}, dispatch=_counting_dispatch(calls)
    )
    provider.initialize("session-a", platform="cli")
    with pytest.raises(_MODULE.CtxCheckpointError, match="max_parts 3"):
        provider.on_pre_compress([{"role": "user", "content": "durable " + "x" * 12_000}])
    assert calls == []


def test_rate_limited_store_backs_off_and_retries_within_budget(monkeypatch):
    calls = []
    slept = []
    clock = {"now": 1000.0}
    monkeypatch.setattr(_MODULE, "_sleep", lambda s: slept.append(s))
    monkeypatch.setattr(_MODULE, "_monotonic", lambda: clock["now"])
    rate_limited = json.dumps({"error": "Rate limit exceeded: max 100 writes per 60 seconds"})
    provider = CtxCheckpointMemoryProvider(
        dispatch=_counting_dispatch(calls, store_results=[rate_limited, rate_limited])
    )
    provider.initialize("session-a", platform="cli")
    frame = provider.on_pre_compress([{"role": "user", "content": "rate limited evidence"}])
    stores = [name for name, _ in calls if name == "mcp__ctx__store"]
    assert len(stores) == 5  # part ×3 (two 429s), manifest, head
    assert slept == [2.0, 4.0]
    assert "ctx checkpoint head" in frame


def test_rate_limit_beyond_budget_fails_closed(monkeypatch):
    import pytest

    calls = []
    clock = {"now": 1000.0}
    monkeypatch.setattr(_MODULE, "_sleep", lambda s: None)
    monkeypatch.setattr(_MODULE, "_monotonic", lambda: clock["now"])
    rate_limited = json.dumps({"error": "Rate limit exceeded: max 100 writes per 60 seconds"})
    provider = CtxCheckpointMemoryProvider(
        config={"budget_s": 5},  # clamped floor: 5 s — the first 2 s backoff fits, the 4 s one does not
        dispatch=_counting_dispatch(calls, store_results=[rate_limited, rate_limited, rate_limited]),
    )
    provider.initialize("session-a", platform="cli")

    def advancing_sleep(s):
        clock["now"] += s

    monkeypatch.setattr(_MODULE, "_sleep", advancing_sleep)
    with pytest.raises(_MODULE.CtxCheckpointError, match="budget 5s"):
        provider.on_pre_compress([{"role": "user", "content": "rate limited evidence"}])
    assert "mcp__ctx__store" in [name for name, _ in calls]


def test_exhausted_budget_before_a_store_fails_closed(monkeypatch):
    import pytest

    calls = []
    clock = {"now": 1000.0}
    monkeypatch.setattr(_MODULE, "_monotonic", lambda: clock["now"])
    real_dispatch = _counting_dispatch(calls)

    def slow_dispatch(name, args, **kwargs):
        clock["now"] += 100.0  # every call eats 100 s of the 60 s default budget
        return real_dispatch(name, args, **kwargs)

    provider = CtxCheckpointMemoryProvider(dispatch=slow_dispatch)
    provider.initialize("session-a", platform="cli")
    with pytest.raises(_MODULE.CtxCheckpointError, match="budget 60s exceeded"):
        provider.on_pre_compress([{"role": "user", "content": "slow evidence"}])
    # Only the head search ran; no partial chain was written.
    assert [name for name, _ in calls] == ["mcp__ctx__search"]


def test_budget_and_max_parts_are_clamped():
    assert _MODULE._clamped_budget_s(None) == 60.0
    assert _MODULE._clamped_budget_s("1") == 5.0
    assert _MODULE._clamped_budget_s(10_000) == 600.0
    assert _MODULE._clamped_max_parts(None) == 80
    assert _MODULE._clamped_max_parts(0) == 1
    assert _MODULE._clamped_max_parts(500) == 95


# ---------------------------------------------------------------------------
# 0.5.2 — answers as ctx and Hermes really shape them, byte cap, budget, logging
# ---------------------------------------------------------------------------


def _hermes_ok(text):
    """A successful MCP call as Hermes hands it over (tools/mcp_tool_handlers.py)."""
    return json.dumps({"result": text}, ensure_ascii=False)


def _hermes_error(text):
    """An isError MCP result as Hermes hands it over: ``tool_error`` → {"error": …}."""
    return json.dumps({"error": text}, ensure_ascii=False)


def _ctx_text_dispatch(calls, *, store=None):
    """Dispatch fake speaking ctx's real answer texts (go/internal/handler/mcp.go)."""
    counter = 300

    def dispatch(name, args, **kwargs):
        nonlocal counter
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return _hermes_ok("[]")
        if store is not None:
            return store(name, args)
        block_id = f"019f6000-0000-7000-8000-{counter:012d}"
        counter += 1
        return _hermes_ok(f"Stored: {args['title']} (id: {block_id}, category: {args['category']})")

    return dispatch


def test_parse_takes_the_id_group_never_another_uuid_in_the_answer():
    parse = _MODULE._parse_tool_result
    block = "019f6000-0000-7000-8000-000000000301"
    session = "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c"
    stored = _hermes_ok(f"Stored: Compaction source {session} part 001 (id: {block}, category: c)")
    assert parse(stored) == (block, None)
    noop = _hermes_ok(f"No change needed — identical content already exists (id: {block})")
    assert parse(noop) == (block, None)
    assert parse(json.dumps({"id": block})) == (block, None)  # structured id wins
    # A UUID outside the id group is not a block id: no id, the caller fails closed.
    assert parse(_hermes_ok(f"Stored something about {session}")) == (None, None)
    assert parse(_hermes_error("store failed: boom")) == (None, "store failed: boom")


def test_uuid_shaped_session_id_never_becomes_a_block_id():
    import re

    calls = []
    session = "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c"  # API clients may pick UUID session ids
    provider = CtxCheckpointMemoryProvider(dispatch=_ctx_text_dispatch(calls))
    provider.initialize(session, platform="api_server")

    frame = provider.on_pre_compress([{"role": "user", "content": "evidence"}])

    part, manifest, head = [args for name, args in calls if name == "mcp__ctx__store"]
    assert session in head["title"]  # the session id IS in every answer text ...
    assert session not in frame      # ... but never taken for a block id
    # The fake answers part 300, manifest 301, head 302.
    assert "019f6000-0000-7000-8000-000000000301" in head["content"]  # the manifest
    assert "019f6000-0000-7000-8000-000000000302" in frame            # the head
    assert manifest["metadata"]["source_block_ids"] == ["019f6000-0000-7000-8000-000000000300"]
    assert re.findall(r"`([0-9a-f-]{36})`", manifest["content"])[-1] == (
        "019f6000-0000-7000-8000-000000000300"
    )


def test_store_answer_without_an_id_fails_closed():
    import pytest

    calls = []
    provider = CtxCheckpointMemoryProvider(
        dispatch=_ctx_text_dispatch(calls, store=lambda name, args: _hermes_ok("Stored."))
    )
    provider.initialize("session-a", platform="cli")
    with pytest.raises(_MODULE.CtxCheckpointError, match="source part 1/1"):
        provider.on_pre_compress([{"role": "user", "content": "evidence"}])


def test_multibyte_parts_stay_under_ctx_byte_cap():
    for char, per_message in (("ж", 30_000), ("漢", 20_000), ("🙂", 15_000)):
        calls = []
        provider = CtxCheckpointMemoryProvider(
            config={"chunk_chars": 40_000}, dispatch=_ctx_text_dispatch(calls)
        )
        provider.initialize("20261031_120000_abcdef", platform="cli")
        messages = [
            {"role": "user", "content": (char * 99 + " ") * (per_message // 100)},
            {"role": "assistant", "content": (char * 49 + "\n") * (per_message // 50)},
        ]

        provider.on_pre_compress(messages)

        parts = [args["content"] for name, args in calls
                 if name == "mcp__ctx__store" and "part" in args["metadata"]]
        assert len(parts) > 1, char
        sizes = [len(content.encode("utf-8")) for content in parts]
        assert max(sizes) <= _MODULE._CTX_CONTENT_MAX_BYTES, (char, sizes)
        # Every character of the transcript lands in exactly one part.
        assert sum(content.count(char) for content in parts) == sum(
            message["content"].count(char) for message in messages
        )


def test_ascii_parts_keep_the_character_limit():
    provider = CtxCheckpointMemoryProvider(config={"chunk_chars": 1_000})
    chunks = provider._split_chunks("x" * 5_000, byte_budget=_MODULE._CTX_CONTENT_MAX_BYTES)
    assert [len(chunk) for chunk in chunks] == [1_000] * 5


def test_staged_store_is_refused_by_name_and_never_confirmed(caplog):
    import logging

    import pytest

    staged = _hermes_error(
        "STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).\n"
        "payload_hash: " + "ab" * 32 + "\nexpires: never (writes.confirm_ttl = 0)"
    )
    calls = []
    provider = CtxCheckpointMemoryProvider(
        dispatch=_ctx_text_dispatch(calls, store=lambda name, args: staged)
    )
    provider.initialize("session-a", platform="cli")

    with caplog.at_level(logging.WARNING), pytest.raises(
        _MODULE.CtxCheckpointError, match="confirm_writes"
    ):
        provider.on_pre_compress([{"role": "user", "content": "evidence"}])

    assert [name for name, _ in calls] == ["mcp__ctx__search", "mcp__ctx__store"]
    assert not any(name.endswith("__confirm") for name, _ in calls)


def test_head_search_is_narrowed_to_checkpoint_heads():
    calls = []
    provider = CtxCheckpointMemoryProvider(
        config={"category": "hermes-checkpoints"}, dispatch=_ctx_text_dispatch(calls)
    )
    provider.initialize("20261031_120000_abcdef", platform="cli")

    provider.on_pre_compress([{"role": "user", "content": "evidence"}])

    name, args = calls[0]
    assert name == "mcp__ctx__search"
    assert args == {
        "query": "Compaction checkpoint head 20261031_120000_abcdef",
        "category": "hermes-checkpoints",
        "tags": ["checkpoint-head"],
        "limit": 5,
    }
    head = [a for n, a in calls if n == "mcp__ctx__store"][-1]
    assert "checkpoint-head" in head["tags"]  # the tag the search relies on


def test_a_hanging_store_is_abandoned_at_the_budget(monkeypatch):
    import threading

    import pytest

    clock = {"now": 1000.0}
    monkeypatch.setattr(_MODULE, "_monotonic", lambda: clock["now"])
    release = threading.Event()
    calls = []

    def dispatch(name, args, **kwargs):
        calls.append(name)
        if name == "mcp__ctx__search":
            clock["now"] += 4.95  # leaves 0.05 s of the 5 s budget for the store
            return _hermes_ok("[]")
        release.wait(10)  # a store that hangs far past the budget
        return _hermes_ok("Stored: x (id: 019f6000-0000-7000-8000-000000000399, category: c)")

    provider = CtxCheckpointMemoryProvider(config={"budget_s": 5}, dispatch=dispatch)
    provider.initialize("session-a", platform="cli")
    try:
        with pytest.raises(_MODULE.CtxCheckpointError, match="exceeded while storing"):
            provider.on_pre_compress([{"role": "user", "content": "evidence"}])
    finally:
        release.set()
    assert calls == ["mcp__ctx__search", "mcp__ctx__store"]


def test_the_head_search_runs_inside_the_budget(monkeypatch):
    import pytest

    calls = []
    monkeypatch.setattr(_MODULE, "_run_bounded", lambda fn, timeout: (False, None, None))
    provider = CtxCheckpointMemoryProvider(dispatch=_ctx_text_dispatch(calls))
    provider.initialize("session-a", platform="cli")

    with pytest.raises(_MODULE.CtxCheckpointError, match="while searching the checkpoint head"):
        provider.on_pre_compress([{"role": "user", "content": "evidence"}])
    assert calls == []  # abandoned before the fake ever answered


def test_dispatch_errors_and_context_cross_the_worker_thread():
    import contextvars

    import pytest

    marker = contextvars.ContextVar("ctx_checkpoint_test_marker", default="unset")
    marker.set("agent-context")
    seen = []

    def dispatch(name, args, **kwargs):
        seen.append(marker.get())
        if name == "mcp__ctx__store":
            raise RuntimeError("transport down")
        return _hermes_ok("[]")

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("session-a", platform="cli")
    with pytest.raises(RuntimeError, match="transport down"):
        provider.on_pre_compress([{"role": "user", "content": "evidence"}])
    assert seen == ["agent-context", "agent-context"]


def test_root_ids_that_sanitizing_changes_get_distinct_head_titles():
    import re

    token = _MODULE._root_title_token
    # Ordinary ids keep their titles, so existing heads are found after the upgrade.
    for root in ("20261031_120000_abcdef", "room_" + "a" * 32, "api_1761900000_0123abcd",
                 "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c"):
        assert token(root) == root
    assert token("team/alice:1") != token("team:alice/1")
    assert token("x" * 48 + "-one") != token("x" * 48 + "-two")
    for root in ("team/alice:1", "x" * 200, "ü" * 10):
        assert len(token(root)) <= 48
        assert re.fullmatch(r"[A-Za-z0-9_.-]+", token(root))
        assert token(root) == token(root)  # stable across calls and processes


def test_failure_is_logged_as_a_warning_naming_the_compaction_outcome(caplog):
    import logging

    import pytest

    failing = _ctx_text_dispatch([], store=lambda name, args: _hermes_error("store failed: db down"))
    for require, outcome in (
        (True, "compaction stays blocked"),
        (False, "compaction proceeds without a durable checkpoint"),
        (None, "the host decides"),
    ):
        caplog.clear()
        provider = CtxCheckpointMemoryProvider(dispatch=failing)
        provider.initialize("session-a", platform="cli")
        kwargs = {} if require is None else {"require_checkpoint": require}
        with caplog.at_level(logging.WARNING), pytest.raises(_MODULE.CtxCheckpointError):
            provider.on_pre_compress([{"role": "user", "content": "evidence"}], **kwargs)
        failures = [r.getMessage() for r in caplog.records
                    if r.levelno == logging.WARNING and r.getMessage().startswith("ctx checkpoint failed")]
        assert len(failures) == 1, failures
        assert outcome in failures[0]
        assert "source part 1/1" in failures[0]


# Same inputs and outputs as HEAD_TITLE_GOLDEN in the ctx provider's
# tests/test_p3_checkpoint_chain.py: both providers write identical chain titles.
def test_head_title_golden_vectors_shared_with_the_ctx_provider():
    golden = {
        "20261031_120000_abcdef": "20261031_120000_abcdef",
        "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c": "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c",
        "root/session-a": "root_session-a-a0af5788",
        "team/alice:1": "team_alice_1-8e207def",
        "team:alice/1": "team_alice_1-7054b256",
        "x" * 60: "x" * 39 + "-42f2d973",
        "über-sitzung": "_ber-sitzung-a42b9cb4",
    }
    for root, token in golden.items():
        assert _MODULE._root_title_token(root) == token, root
