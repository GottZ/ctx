import importlib.util
import json
import sys
from pathlib import Path

from agent.memory_manager import MemoryManager

_PROVIDER_PATH = Path(__file__).resolve().parents[1] / "__init__.py"
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_provider_contract", _PROVIDER_PATH, submodule_search_locations=[str(_PROVIDER_PATH.parent)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE  # package context for the plugin's relative imports
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

    frame = provider.on_pre_compress([{"role": "user", "content": "durable source decision"}])

    assert provider.pre_compress_checkpoint_api_version == 2
    assert [name for name, _ in calls] == [
        "mcp__ctx__search",
        "mcp__ctx__search",
        "mcp__ctx__store",
        "mcp__ctx__store",
        "mcp__ctx__store",
    ]
    # "/" is sanitized away, so the title carries a hash of the full root id (0.6.1); with
    # no head under it yet, the 0.6.0 title is searched once for a chain to continue.
    assert calls[0][1]["query"] == "Compaction checkpoint head root_session-a-a0af5788"
    assert calls[1][1]["query"] == "Compaction checkpoint head root_session-a"
    assert calls[4][1]["title"] == "Compaction checkpoint head root_session-a-a0af5788"
    assert manifest_id in calls[4][1]["content"]
    assert source_id in calls[3][1]["content"]
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

    frame = provider.on_pre_compress([{"role": "user", "content": "durable " + "x" * 12_000}])

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

    first = provider.on_pre_compress([{"role": "user", "content": "first durable decision"}])
    second = provider.on_pre_compress([{"role": "user", "content": "second corrected decision"}])

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
    provider.on_pre_compress([{"role": "user", "content": "new durable child checkpoint"}])

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
    provider.on_pre_compress([{"role": "user", "content": "fork evidence worth archiving"}])
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

    import agent.redact as host_redact

    monkeypatch.setattr(host_redact, "redact_sensitive_text", broken)
    import pytest

    with pytest.raises(_MODULE.CtxCheckpointError, match="redaction failed"):
        provider.on_pre_compress([{"role": "user", "content": "secret-ish evidence"}], require_checkpoint=True)
    # best effort: logged, never raised, nothing written
    assert provider.on_pre_compress([{"role": "user", "content": "secret-ish evidence"}]) == ""
    assert calls == []


def test_more_parts_than_max_parts_writes_nothing():
    import pytest

    calls = []
    provider = CtxCheckpointMemoryProvider(
        config={"chunk_chars": 1_000, "max_parts": 3}, dispatch=_counting_dispatch(calls)
    )
    provider.initialize("session-a", platform="cli")
    with pytest.raises(_MODULE.CtxCheckpointError, match="max_parts 3"):
        provider.on_pre_compress([{"role": "user", "content": "durable " + "x" * 12_000}], require_checkpoint=True)
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
        provider.on_pre_compress([{"role": "user", "content": "rate limited evidence"}], require_checkpoint=True)
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
        provider.on_pre_compress([{"role": "user", "content": "slow evidence"}], require_checkpoint=True)
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
# 0.6.1 — the MCP transport against the answers ctx and Hermes really produce
# ---------------------------------------------------------------------------


def _hermes_ok(text):
    """A successful MCP call as Hermes hands it over (tools/mcp_tool_handlers.py)."""
    return json.dumps({"result": text}, ensure_ascii=False)


def _hermes_error(text):
    """An isError MCP result as Hermes hands it over: ``tool_error`` → {"error": …}."""
    return json.dumps({"error": text}, ensure_ascii=False)


def _ctx_text_dispatch(calls, *, store=None):
    """Dispatch fake speaking ctx's real store answer text (go/internal/handler/mcp.go)."""
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


def test_uuid_shaped_session_id_never_becomes_a_block_id():
    calls = []
    session = "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c"  # the API server lets clients pick session ids
    provider = CtxCheckpointMemoryProvider(dispatch=_ctx_text_dispatch(calls))
    provider.initialize(session, platform="api_server")

    frame = provider.on_pre_compress([{"role": "user", "content": "evidence"}], require_checkpoint=True)

    _part, manifest, head = [args for name, args in calls if name == "mcp__ctx__store"]
    assert session in head["title"]  # the session id IS in every store answer text ...
    assert session not in frame  # ... but never taken for a block id
    # The fake answers part 300, manifest 301, head 302.
    assert manifest["metadata"]["source_block_ids"] == ["019f6000-0000-7000-8000-000000000300"]
    assert "019f6000-0000-7000-8000-000000000301" in head["content"]
    assert "019f6000-0000-7000-8000-000000000302" in frame


def test_store_answer_without_an_id_group_fails_closed():
    import pytest

    calls = []
    session = "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c"
    answer = lambda name, args: _hermes_ok(f"Stored: {args['title']}")  # noqa: E731 - UUID only in the title
    provider = CtxCheckpointMemoryProvider(dispatch=_ctx_text_dispatch(calls, store=answer))
    provider.initialize(session, platform="api_server")

    with pytest.raises(_MODULE.CtxCheckpointError):
        provider.on_pre_compress([{"role": "user", "content": "evidence"}], require_checkpoint=True)
    assert [name for name, _ in calls] == ["mcp__ctx__search", "mcp__ctx__store"]


def test_staged_store_is_refused_by_name_and_never_confirmed():
    import pytest

    staged = _hermes_error(
        "STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).\n"
        "payload_hash: " + "ab" * 32 + "\nexpires: never (writes.confirm_ttl = 0)"
    )
    calls = []
    provider = CtxCheckpointMemoryProvider(dispatch=_ctx_text_dispatch(calls, store=lambda name, args: staged))
    provider.initialize("session-a", platform="cli")

    # Not just the raw server text: the refusal is coded and says what to do.
    with pytest.raises(_MODULE.CtxCheckpointError, match=r"\[confirm_writes\].*staged, not stored") as raised:
        provider.on_pre_compress([{"role": "user", "content": "evidence"}], require_checkpoint=True)
    # The staged hash survives the chain writer's 200-character cut of the cause.
    message = str(raised.value)
    assert "payload_hash " + "ab" * 32 in message
    assert [name for name, _ in calls] == ["mcp__ctx__search", "mcp__ctx__store"]
    assert not any(name.endswith("__confirm") for name, _ in calls)


def test_head_search_over_mcp_is_narrowed_to_checkpoint_heads():
    calls = []
    provider = CtxCheckpointMemoryProvider(
        config={"category": "hermes-checkpoints"}, dispatch=_ctx_text_dispatch(calls)
    )
    provider.initialize("20261031_120000_abcdef", platform="cli")

    provider.on_pre_compress([{"role": "user", "content": "evidence"}], require_checkpoint=True)

    name, args = calls[0]
    assert name == "mcp__ctx__search"
    assert args["query"] == "Compaction checkpoint head 20261031_120000_abcdef"
    assert args["category"] == "hermes-checkpoints"
    # The MCP search drops ``types``; the tag is what keeps source parts out of the window.
    assert args["tags"] == ["checkpoint-head"] and "types" not in args
    head = [a for n, a in calls if n == "mcp__ctx__store"][-1]
    assert "checkpoint-head" in head["tags"]


def test_multibyte_parts_over_mcp_stay_under_ctx_byte_cap():
    for char, per_message in (("ж", 30_000), ("漢", 20_000), ("🙂", 15_000)):
        calls = []
        provider = CtxCheckpointMemoryProvider(config={"chunk_chars": 40_000}, dispatch=_ctx_text_dispatch(calls))
        provider.initialize("20261031_120000_abcdef", platform="cli")
        messages = [
            {"role": "user", "content": (char * 99 + " ") * (per_message // 100)},
            {"role": "assistant", "content": (char * 49 + "\n") * (per_message // 50)},
        ]

        provider.on_pre_compress(messages, require_checkpoint=True)

        parts = [a["content"] for n, a in calls if n == "mcp__ctx__store" and " part " in a["title"]]
        assert len(parts) > 1, char
        sizes = [len(body.encode("utf-8")) for body in parts]
        assert max(sizes) <= 50 * 1024, (char, sizes)
        assert sum(body.count(char) for body in parts) == sum(m["content"].count(char) for m in messages)


def test_a_hanging_store_is_abandoned_at_the_call_timeout():
    import threading
    import time

    import pytest

    release = threading.Event()
    calls = []

    def dispatch(name, args, **kwargs):
        calls.append(name)
        if name == "mcp__ctx__search":
            return _hermes_ok("[]")
        release.wait(30)  # far past the 1 s per-call timeout and Hermes' own 300 s default
        return _hermes_ok("Stored: x (id: 019f6000-0000-7000-8000-000000000399, category: c)")

    provider = CtxCheckpointMemoryProvider(config={"api_timeout": 1}, dispatch=dispatch)
    provider.initialize("session-a", platform="cli")
    started = time.monotonic()
    try:
        with pytest.raises(_MODULE.CtxCheckpointError, match="timeout"):
            provider.on_pre_compress([{"role": "user", "content": "evidence"}], require_checkpoint=True)
    finally:
        release.set()
    assert time.monotonic() - started < 10
    assert calls == ["mcp__ctx__search", "mcp__ctx__store"]


def test_dispatch_sees_the_callers_context_variables():
    import contextvars

    marker = contextvars.ContextVar("ctx_checkpoint_test_marker", default="unset")
    marker.set("agent-context")
    seen = []
    inner = _ctx_text_dispatch([])

    def dispatch(name, args, **kwargs):
        seen.append(marker.get())
        return inner(name, args, **kwargs)

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("session-a", platform="cli")
    provider.on_pre_compress([{"role": "user", "content": "evidence"}], require_checkpoint=True)

    assert seen and set(seen) == {"agent-context"}  # bounded calls run on a worker thread


def test_bounded_dispatch_acts_for_the_calling_thread():
    import threading

    from tools.interrupt import acting_for_tid

    seen = []
    inner = _ctx_text_dispatch([])

    def dispatch(name, args, **kwargs):
        seen.append((threading.get_ident(), acting_for_tid.get()))
        return inner(name, args, **kwargs)

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch)
    provider.initialize("session-a", platform="cli")
    provider.on_pre_compress([{"role": "user", "content": "evidence"}], require_checkpoint=True)

    caller = threading.get_ident()
    assert seen and all(worker != caller and acting == caller for worker, acting in seen)
    assert acting_for_tid.get() is None  # set in the worker's copy of the context only


def test_stop_aimed_at_the_calling_thread_reaches_a_bounded_call():
    import threading
    import time

    import pytest
    from tools.interrupt import is_interrupted, set_interrupt

    caller = threading.get_ident()

    def dispatch(name, args, **kwargs):
        # polls like Hermes' _run_on_mcp_loop and answers like its InterruptedError branch
        until = time.monotonic() + 10
        while time.monotonic() < until:
            if is_interrupted():
                return _hermes_error("MCP call interrupted: user sent a new message")
            time.sleep(0.02)
        return _hermes_ok("[]")

    provider = CtxCheckpointMemoryProvider(config={"api_timeout": 5}, dispatch=dispatch)
    provider.initialize("session-a", platform="cli")
    stop = threading.Timer(0.2, set_interrupt, args=(True, caller))
    started = time.monotonic()
    stop.start()
    try:
        with pytest.raises(_MODULE.CtxCheckpointError, match="interrupted") as raised:
            provider.on_pre_compress([{"role": "user", "content": "evidence"}], require_checkpoint=True)
    finally:
        stop.cancel()
        set_interrupt(False, caller)
    assert time.monotonic() - started < 3  # not held until the 5 s call timeout
    assert "[timeout]" not in str(raised.value)
