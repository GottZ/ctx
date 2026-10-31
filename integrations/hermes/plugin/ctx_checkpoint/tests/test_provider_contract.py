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
    assert calls[3][1]["title"] == "Compaction checkpoint head root_session-a"
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
