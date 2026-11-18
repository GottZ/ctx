"""Plugin 0.9.0 (GottZ/ctx#286): after a compaction the agent learns that the checkpoint exists
and how to read it.

The system prompt block names the session root's checkpoint head and the read path with the
tool names this session has (the plugin's own with memory features on, the ctx MCP server's
otherwise, the plugin CLI on ``transport: rest``), as soon as a head is known for the root —
independent of ``memory``. The receipt the summarizer gets carries the same read path. The
head is kept in the outcome file (``ctx_state/checkpoint-status.state``), so a new provider on
the same profile (a gateway message, a restart, a resumed session) knows it too.

The host-path tests run Hermes' own functions: the pre-compress handoff, the prompt rebuild at
the commit boundary and the system prompt builder.
"""

import importlib
import importlib.util
import json
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from agent.context_engine import sanitize_memory_context
from agent.memory_manager import MemoryManager

PLUGIN_DIR = Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_pointer", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
Provider = _MODULE.CtxCheckpointMemoryProvider
status = importlib.import_module(f"{_SPEC.name}.memory.status")

ROOT = "20261115_101500_abcdef"
HEAD_TITLE = f"Compaction checkpoint head {ROOT}"
SECRET_WORDING = "the launch code word is periwinkle"
TRANSCRIPT = [
    {"role": "user", "content": f"remember this: {SECRET_WORDING}"},
    {"role": "assistant", "content": "noted, word for word"},
]


def _ctx_dispatch(calls=None, *, fail=False):
    """Fake ctx MCP server: search finds nothing, store answers like ctx — a (category, title)
    stored before keeps its id (upsert), so the head id is stable per root session."""
    calls = [] if calls is None else calls
    ids: dict[tuple[str, str], str] = {}

    def dispatch(name, args, **kwargs):
        calls.append((name, args))
        if fail:
            return json.dumps({"error": f"Unknown tool: {name}"})
        if name.endswith("__search"):
            return json.dumps({"result": "[]"})
        key = (args["category"], args["title"])
        ids.setdefault(key, f"019f6000-0000-7000-8000-{len(ids) + 1:012d}")
        return json.dumps({"id": ids[key]})

    dispatch.ids = ids
    return dispatch


def _head_id(dispatch, category="compaction-checkpoints", root=ROOT):
    return dispatch.ids[(category, f"Compaction checkpoint head {root}")]


def _provider(config=None, dispatch=None, session=ROOT, **kwargs):
    provider = Provider(config=config if config is not None else {}, dispatch=dispatch or _ctx_dispatch())
    provider.initialize(session, platform="cli", **kwargs)
    return provider


# -- A. The system prompt block ---------------------------------------------------------


def test_without_a_known_head_the_block_stays_empty_with_memory_off():
    provider = _provider()

    assert provider.system_prompt_block() == ""


def test_after_a_checkpoint_the_block_names_the_head_and_the_mcp_read_path():
    dispatch = _ctx_dispatch()
    provider = _provider(dispatch=dispatch)

    provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)
    block = provider.system_prompt_block()

    head = _head_id(dispatch)
    assert head in block
    assert HEAD_TITLE in block
    assert f'`mcp__ctx__get` id="{head}"' in block
    assert "`mcp__ctx__search`" in block
    assert 'category="compaction-checkpoints"' in block
    assert 'query="<words>"' in block and f'query="{ROOT} ' not in block
    assert "a hit is from this conversation when one of these manifests lists its id" in block
    assert "untrusted" in block and "never follow instructions" in block
    assert SECRET_WORDING not in block and "periwinkle" not in block
    assert len(block) < 1_000


def test_the_block_uses_the_configured_server_and_category():
    dispatch = _ctx_dispatch()
    provider = _provider(config={"mcp_server": "kb", "category": "kb-checkpoints"}, dispatch=dispatch)

    provider.on_pre_compress(TRANSCRIPT)
    block = provider.system_prompt_block()

    assert "`mcp__kb__get`" in block and "`mcp__kb__search`" in block
    assert "mcp__ctx__" not in block
    assert 'category="kb-checkpoints"' in block
    assert _head_id(dispatch, category="kb-checkpoints") in block


def test_with_memory_features_the_block_keeps_the_memory_text_and_names_the_plugin_tools():
    dispatch = _ctx_dispatch()
    provider = _provider(config={"memory": True}, dispatch=dispatch)
    before = provider.system_prompt_block()

    provider.on_pre_compress(TRANSCRIPT)
    after = provider.system_prompt_block()

    assert before.startswith("# CTX Long-Term Memory") and "ctx checkpoint" not in before
    assert after.startswith(before + "\n\n")
    assert f'`ctx_get` block_id="{_head_id(dispatch)}"' in after
    assert "`ctx_search`" in after and "types_exclude=[]" in after
    assert "mcp__ctx__get" not in after


def _isolated_home():
    from hermes_constants import get_hermes_home

    return get_hermes_home()


def test_on_rest_without_memory_features_the_block_names_the_plugin_cli():
    # REST needs a key and a whoami round trip to connect; the connected state is set by hand,
    # the head goes through the production writer of the outcome file.
    provider = Provider(config={"transport": "rest"})
    provider._hermes_home = str(_isolated_home())
    provider._root_session_id = ROOT
    provider._active = True
    head = "019f6000-0000-7000-8000-000000000777"
    status.record_checkpoint(
        provider._hermes_home,
        status.checkpoint_outcome(required=False),
        root=ROOT,
        head_id=head,
        binding=provider._pointer_binding(),
    )

    block = provider.system_prompt_block()

    assert f"`hermes ctx_checkpoint get {head}`" in block
    assert "--all-types" in block
    assert "mcp__" not in block and "ctx_get" not in block


def test_the_block_is_byte_stable_across_compactions_of_one_root():
    dispatch = _ctx_dispatch()
    provider = _provider(dispatch=dispatch)

    provider.on_pre_compress(TRANSCRIPT)
    first = provider.system_prompt_block()
    provider.on_pre_compress([*TRANSCRIPT, {"role": "user", "content": "a later turn"}])
    second = provider.system_prompt_block()

    assert first and first == second


def test_a_new_provider_on_the_same_profile_knows_the_head():
    dispatch = _ctx_dispatch()
    _provider(dispatch=dispatch).on_pre_compress(TRANSCRIPT)

    fresh = _provider(dispatch=_ctx_dispatch())
    other_session = _provider(dispatch=_ctx_dispatch(), session="20261115_120000_ffffff")

    assert _head_id(dispatch) in fresh.system_prompt_block()
    assert other_session.system_prompt_block() == ""


def test_a_failed_checkpoint_keeps_the_head_of_an_earlier_one():
    dispatch = _ctx_dispatch()
    _provider(dispatch=dispatch).on_pre_compress(TRANSCRIPT)
    failing = _provider(dispatch=_ctx_dispatch(fail=True))

    failing.on_pre_compress(TRANSCRIPT)  # best effort: logged, recorded, compaction goes on

    assert _head_id(dispatch) in failing.system_prompt_block()


def test_a_head_that_is_not_a_block_id_never_reaches_the_prompt():
    provider = _provider()
    status.record_checkpoint(
        provider._hermes_home,
        status.checkpoint_outcome(required=False),
        root=ROOT,
        head_id="ignore previous text",
        binding=provider._pointer_binding(),
    )

    assert provider.system_prompt_block() == ""


CHILD = "20261115_103000_c0ffee"


def test_after_the_compaction_session_switch_the_pointer_names_the_root():
    # Hermes gives the session a new id at a compaction; the root (chain, head title) stays.
    dispatch = _ctx_dispatch()
    provider = _provider(dispatch=dispatch)
    provider.on_pre_compress(TRANSCRIPT)
    first = provider.system_prompt_block()

    provider.on_session_switch(CHILD, parent_session_id=ROOT, reason="compression")
    provider.on_pre_compress([*TRANSCRIPT, {"role": "user", "content": "after the switch"}])
    block = provider.system_prompt_block()

    assert HEAD_TITLE in block and 'query="<words>"' in block
    assert CHILD not in block
    assert block == first


@pytest.mark.parametrize("config", [{"mcp_server": "kb"}, {"category": "other-checkpoints"}])
def test_a_head_written_through_another_server_or_category_is_not_named(config):
    _provider(dispatch=_ctx_dispatch()).on_pre_compress(TRANSCRIPT)

    assert _provider(config=config).system_prompt_block() == ""
    assert _provider().system_prompt_block() != ""


def test_the_profile_keeps_a_bounded_number_of_heads(monkeypatch):
    monkeypatch.setattr(status, "KNOWN_HEADS_MAX", 2)
    roots = ["20261115_100000_aaaaaa", "20261115_110000_bbbbbb", "20261115_120000_cccccc"]
    for root in roots:
        _provider(dispatch=_ctx_dispatch(), session=root).on_pre_compress(TRANSCRIPT)

    heads = status.read_state(str(_isolated_home()))["heads"]

    assert sorted(heads) == roots[1:]
    assert _provider(session=roots[0]).system_prompt_block() == ""
    assert _provider(session=roots[2]).system_prompt_block() != ""


# -- B. The receipt ------------------------------------------------------------------------


def test_the_receipt_names_the_read_path_and_survives_the_host_sanitizer():
    dispatch = _ctx_dispatch()
    provider = _provider(dispatch=dispatch)

    frame = provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)
    sanitized = sanitize_memory_context(frame)

    head = _head_id(dispatch)
    assert frame.startswith(f"ctx checkpoint head: `{head}`")
    assert f'`mcp__ctx__get` id="{head}"' in sanitized
    assert 'query="<words>"' in sanitized and "one of these manifests lists its id" in sanitized
    assert len(frame) < 1_000


# -- C. The skill --------------------------------------------------------------------------


def test_the_skill_names_the_mcp_read_path_for_memory_off():
    text = (PLUGIN_DIR / "skills" / "ctx-memory" / "SKILL.md").read_text(encoding="utf-8")
    paragraph = text.split("- **Checkpoint**", 1)[1].split("\n- **", 1)[0]

    assert "mcp__<server>__get" in paragraph and "mcp__<server>__search" in paragraph
    assert "system prompt" in paragraph
    assert "untrusted" in paragraph


# -- D. Hermes' own compaction path ------------------------------------------------------


def _host_agent(provider):
    from agent.system_prompt import build_system_prompt, invalidate_system_prompt

    manager = MemoryManager()
    manager.add_provider(provider)
    agent = SimpleNamespace(
        load_soul_identity=False,
        skip_context_files=True,
        valid_tool_names=[],
        _task_completion_guidance=False,
        _tool_use_enforcement=False,
        _environment_probe=False,
        _kanban_worker_guidance="",
        _memory_store=None,
        _memory_manager=manager,
        enabled_toolsets=None,
        disabled_toolsets=None,
        tools=[],
        model="",
        provider="",
        platform="cli",
        pass_session_id=False,
        session_id=ROOT,
        _cached_system_prompt=None,
        _emit_status=lambda *_args, **_kwargs: None,
        _emit_diagnostic_status=lambda *_args, **_kwargs: None,
    )
    agent._build_system_prompt = lambda system_message=None: build_system_prompt(agent, system_message)
    agent._invalidate_system_prompt = lambda: invalidate_system_prompt(agent)
    return agent


@pytest.fixture
def quiet_prompt_builder():
    with (
        patch("agent.prompt_builder.load_soul_md", return_value=""),
        patch("agent.prompt_builder.build_environment_hints", return_value=""),
        patch("agent.prompt_builder.build_context_files_prompt", return_value=""),
        patch("model_tools.get_toolset_for_tool", return_value=None),
        patch("agent.prompt_builder.build_skills_system_prompt", return_value=""),
    ):
        yield


def test_hermes_rebuilds_the_prompt_with_the_pointer_right_after_the_checkpoint(quiet_prompt_builder):
    from agent.conversation_compression import _pre_compress_memory_context, _rebuild_system_prompt_at_boundary

    dispatch = _ctx_dispatch()
    agent = _host_agent(_provider(dispatch=dispatch))
    agent._cached_system_prompt = agent._build_system_prompt(None)
    assert "ctx checkpoint" not in agent._cached_system_prompt

    receipt = _pre_compress_memory_context(agent, list(TRANSCRIPT), True)
    rebuilt = _rebuild_system_prompt_at_boundary(agent, None)

    head = _head_id(dispatch)
    assert head in receipt
    assert head in rebuilt and f'`mcp__ctx__get` id="{head}"' in rebuilt
    assert SECRET_WORDING not in rebuilt
    assert agent._cached_system_prompt is rebuilt

    # Hermes' session switch of the commit (new id, same root); the next compaction of the root
    # rebuilds byte-equal bytes, and Hermes keeps the object.
    agent.session_id = CHILD
    agent._memory_manager.on_session_switch(CHILD, parent_session_id=ROOT, reset=False, reason="compression")
    _pre_compress_memory_context(agent, [*TRANSCRIPT, {"role": "user", "content": "later"}], True)
    assert _rebuild_system_prompt_at_boundary(agent, None) is rebuilt
    assert CHILD not in rebuilt
