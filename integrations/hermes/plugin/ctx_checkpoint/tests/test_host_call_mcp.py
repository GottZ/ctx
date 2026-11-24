"""GottZ/ctx#299, point 3: a ctx call the checkpoint gives up on ends in Hermes as well.

The plugin bounds every checkpoint call by its remaining budget. Through the tool dispatch it can
only stop waiting: Hermes' MCP handler keeps running the call under the server entry's RPC lock
until the entry's own tool timeout (300 s by default), so the next compaction's first call — the
head search — waits behind it and fails as well, although the server would answer at once. With
the operator's grant (``plugins.entries.ctx_checkpoint.mcp_allowlist``) the bounded calls go
through Hermes' ``PluginContext.call_mcp(server, tool, args, timeout)``, which builds the handler
with the caller's timeout and cancels the call on Hermes' MCP loop when it runs out.

The first group runs that against Hermes' own code: the memory plugin loader's provider collector
hands ``register()`` its context, and every call runs through ``tools.mcp_tool_handlers``'
``_make_tool_handler`` on Hermes' MCP event loop under the entry's ``asyncio.Lock`` — the handler
the tool registry dispatches to, and the one ``call_mcp`` builds. Only the server session is a
stand-in: a tiny ctx whose ``call_tool`` can be told to never answer.
"""

from __future__ import annotations

import asyncio
import importlib.util
import json
import sys
import threading
import time
import types
from pathlib import Path

import pytest

_PROVIDER_PATH = Path(__file__).resolve().parents[1] / "__init__.py"
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_host_call", _PROVIDER_PATH, submodule_search_locations=[str(_PROVIDER_PATH.parent)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
CtxCheckpointMemoryProvider = _MODULE.CtxCheckpointMemoryProvider
CtxCheckpointError = _MODULE.CtxCheckpointError
mcp_tool_name = sys.modules[f"{_SPEC.name}.memory.mcp_client"].mcp_tool_name

_STORE_ANSWERS = json.loads(
    (Path(__file__).resolve().parents[1] / "memory" / "tests" / "mcp_answers.json").read_text(encoding="utf-8")
)["store"]
ENTRY_TOOL_TIMEOUT_S = 30.0  # the entry's own tool timeout (Hermes' default is 300 s)
TOOLS = ("search", "get", "store", "query")


class TinyCtx:
    """ctx's search/get/store over a dict, answering like its MCP tools (the store texts are the ones
    go/internal/handler/mcp_checkpoint_answers_integration_test.go pins)."""

    def __init__(self) -> None:
        self.blocks: dict[tuple[str, str], dict] = {}
        self.counter = 0
        self.calls: list[tuple[str, dict]] = []

    def answer(self, tool: str, args: dict) -> tuple[bool, str]:
        self.calls.append((tool, dict(args)))
        if tool == "store":
            key = (args["category"], args["title"])
            stored = self.blocks.get(key)
            incoming = {k: args.get(k) for k in ("content", "tags", "sensitivity", "type", "metadata")}
            if stored is not None and all(stored.get(k) == v for k, v in incoming.items()):
                return False, _STORE_ANSWERS["no_change"].format(title=args["title"], id=stored["id"], category=key[0])
            if stored is None:
                self.counter += 1
                stored = self.blocks[key] = {"id": f"019f7000-0000-7000-8000-{self.counter:012d}"}
            stored.update(incoming, title=args["title"], category=args["category"])
            return False, _STORE_ANSWERS["stored"].format(title=args["title"], id=stored["id"], category=key[0])
        if tool == "search":
            tags = set(args.get("tags") or [])
            rows = [
                {"id": b["id"], "title": b["title"], "category": b["category"], "type": "checkpoint"}
                for b in self.blocks.values()
                if args["query"] in b["title"] and tags <= set(b.get("tags") or [])
            ]
            return False, json.dumps(rows[: int(args.get("limit") or 10)])
        if tool == "get":
            for b in self.blocks.values():
                if b["id"] == args["id"]:
                    return False, json.dumps({k: b.get(k) for k in ("id", "title", "category", "content", "metadata")})
            return True, "block not found"
        return True, f"unknown tool {tool}"

    def heads(self) -> list[dict]:
        return [b for b in self.blocks.values() if b["title"].startswith("Compaction checkpoint head")]


class HangingSession:
    """The MCP client session of the entry: ``call_tool`` answers through TinyCtx, except that the
    next ``hang`` calls never answer (a server that stopped responding; cancellable)."""

    def __init__(self, ctx: TinyCtx) -> None:
        self.ctx = ctx
        self.hang = 0
        self.hung = 0

    async def call_tool(self, name, arguments=None, **_kwargs):
        if self.hang:
            self.hang -= 1
            self.hung += 1
            await asyncio.Event().wait()
        is_error, text = self.ctx.answer(name, dict(arguments or {}))
        return types.SimpleNamespace(
            content=[types.SimpleNamespace(type="text", text=text)], isError=is_error, structuredContent=None, meta=None
        )


@pytest.fixture
def hermes_mcp(monkeypatch):
    """Hermes' MCP loop with one connected server entry ``ctx`` (the stand-in session), fresh breaker
    state, and the entry's tools in Hermes' tool registry with the handlers Hermes registers."""
    from tools import mcp_tool, mcp_tool_handlers, mcp_tool_loop

    for name in ("_server_error_counts", "_server_breaker_opened_at", "_server_errors_all_application"):
        monkeypatch.setattr(mcp_tool, name, {})
    loop_was_running = mcp_tool._mcp_loop is not None and mcp_tool._mcp_loop.is_running()
    mcp_tool_loop._ensure_mcp_loop()
    ctx = TinyCtx()
    session = HangingSession(ctx)
    server = types.SimpleNamespace(session=session, _rpc_lock=asyncio.Lock())
    monkeypatch.setitem(mcp_tool._servers, "ctx", server)
    from tools.registry import registry

    names = []
    for tool in TOOLS:  # as Hermes registers an entry's tools (tools/mcp_tool_registration.py)
        name = mcp_tool_name("ctx", tool)
        handler = mcp_tool_handlers._make_tool_handler("ctx", tool, ENTRY_TOOL_TIMEOUT_S, native_images=True)
        schema = {"name": name, "description": f"ctx {tool}", "parameters": {"type": "object", "properties": {}}}
        registry.register(name=name, toolset="mcp-ctx", schema=schema, handler=handler)
        names.append(name)
    yield types.SimpleNamespace(server=server, session=session, ctx=ctx)
    for name in names:
        registry.deregister(name)
    if not loop_was_running:
        mcp_tool_loop._stop_mcp_loop()  # cancels a call still hanging


def _write_config(home: Path, *, granted: bool) -> None:
    config = {
        "mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp"}},
        "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {"mcp_server": "ctx", "api_timeout": 1}},
    }
    if granted:
        config["plugins"] = {"entries": {"ctx_checkpoint": {"mcp_allowlist": ["ctx"]}}}
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")


def _loaded_provider(hermes_mcp, home: Path):
    """The provider as Hermes' memory plugin loader collects it (plugins/memory: register(ctx) with
    the provider collector), dispatching through Hermes' tool registry as in production."""
    from plugins.memory import _ProviderCollector

    collector = _ProviderCollector("ctx_checkpoint", register_skills=False)
    collector.collect(_MODULE.register)
    provider = collector.provider
    assert isinstance(provider, CtxCheckpointMemoryProvider)
    provider.initialize("session-lock", platform="cli", hermes_home=str(home))
    return provider


def _turns(tag: str) -> list[dict]:
    return [{"role": "user", "content": f"{tag} " + "x" * 300}, {"role": "assistant", "content": f"{tag} done"}]


def _lock_freed(server, within_s: float = 3.0) -> bool:
    deadline = time.monotonic() + within_s
    while time.monotonic() < deadline:
        if not server._rpc_lock.locked():
            return True
        time.sleep(0.02)
    return False


def test_with_the_grant_an_abandoned_call_frees_the_entry_and_the_next_compaction_passes(
    hermes_mcp, _isolated_hermes_home
):
    _write_config(_isolated_hermes_home, granted=True)
    provider = _loaded_provider(hermes_mcp, _isolated_hermes_home)
    hermes_mcp.session.hang = 1  # the first call of the first compaction never gets an answer

    with pytest.raises(CtxCheckpointError, match="head search failed"):
        provider.on_pre_compress(_turns("first"), require_checkpoint=True)
    assert hermes_mcp.session.hung == 1
    # Hermes cancelled the call at the checkpoint's own timeout: the entry's lock is free again.
    assert _lock_freed(hermes_mcp.server)

    receipt = provider.on_pre_compress(_turns("second"), require_checkpoint=True)

    assert "ctx checkpoint head:" in receipt
    assert len(hermes_mcp.ctx.heads()) == 1


def test_without_the_grant_the_next_compaction_still_meets_the_held_lock(hermes_mcp, _isolated_hermes_home):
    """The behaviour the grant ends, unchanged without it: through the dispatch the abandoned call
    holds the entry until its own tool timeout, and the next compaction's head search waits behind it."""
    _write_config(_isolated_hermes_home, granted=False)
    provider = _loaded_provider(hermes_mcp, _isolated_hermes_home)
    hermes_mcp.session.hang = 1

    with pytest.raises(CtxCheckpointError, match=r"head search failed \[timeout\]"):
        provider.on_pre_compress(_turns("first"), require_checkpoint=True)
    assert hermes_mcp.server._rpc_lock.locked()
    with pytest.raises(CtxCheckpointError, match=r"head search failed \[timeout\]"):
        provider.on_pre_compress(_turns("second"), require_checkpoint=True)
    assert not hermes_mcp.ctx.heads()


def test_register_takes_call_mcp_from_the_plugin_context_behind_the_provider_collector():
    """Hermes hands a memory provider's register() its provider collector, which forwards only
    register_* calls; call_mcp comes from the PluginContext the collector holds for the plugin."""
    from hermes_cli.plugins import PluginContext
    from plugins.memory import _ProviderCollector

    collector = _ProviderCollector("ctx_checkpoint", register_skills=False)
    assert getattr(collector, "call_mcp", None) is None
    call, plugin_id = _MODULE._host_call_mcp(collector)
    assert isinstance(getattr(call, "__self__", None), PluginContext)
    assert call.__func__ is PluginContext.call_mcp
    assert plugin_id == "ctx_checkpoint"


def test_register_takes_call_mcp_only_from_a_real_plugin_context():
    """A PluginContext handed over directly counts; anything else does not: under
    plugins.isolation: host the plugin's context forwards call_mcp to a collector that has none."""
    from unittest.mock import MagicMock

    from hermes_cli.plugins import PluginContext, PluginManifest

    context = PluginContext(PluginManifest(name="ctx_checkpoint_dev", key="ctx_checkpoint_dev"), MagicMock())
    call, plugin_id = _MODULE._host_call_mcp(context)
    assert call.__func__ is PluginContext.call_mcp and plugin_id == "ctx_checkpoint_dev"

    def call_mcp(server, tool, arguments=None, timeout=30):
        return {"ok": True, "result": ""}

    assert _MODULE._host_call_mcp(types.SimpleNamespace(call_mcp=call_mcp)) == (None, "")
    assert _MODULE._host_call_mcp(types.SimpleNamespace()) == (None, "")

    def broken():
        raise RuntimeError("no plugin manager")

    assert _MODULE._host_call_mcp(types.SimpleNamespace(_plugin_context=broken)) == (None, "")


# -- without the grant, on a Hermes without call_mcp, and on a refused grant: as before ---------


class DispatchDouble:
    """ctx behind the tool dispatch, recording every call; ``block`` makes the next head search
    wait (real time) until the test releases it."""

    def __init__(self) -> None:
        self.ctx = TinyCtx()
        self.block_next_search = False
        self.release = threading.Event()
        self.returned = threading.Event()

    def dispatch(self, name: str, args: dict, **_kwargs) -> str:
        tool = name.rsplit("__", 1)[-1]
        if tool == "search" and self.block_next_search:
            self.block_next_search = False
            self.ctx.calls.append(("search(blocked)", dict(args)))
            self.release.wait(10)
            self.returned.set()
            return json.dumps({"error": "late"})
        is_error, text = self.ctx.answer(tool, args)
        return json.dumps({"error": text} if is_error else {"result": text})


@pytest.fixture
def registered(monkeypatch):
    """The tools Hermes registered for the entry (what its tools filter left): all of them unless
    a test narrows the set."""
    names = {mcp_tool_name("ctx", tool) for tool in TOOLS}
    monkeypatch.setattr(_MODULE, "_registered_tool", lambda name: name in names)
    return names


def _normalized(calls: list[tuple[str, dict]]) -> list[tuple[str, dict]]:
    out = []
    for tool, args in calls:
        args = json.loads(json.dumps(args))
        metadata = args.get("metadata")
        if isinstance(metadata, dict):
            metadata.pop("evidence_date", None)  # the wall clock of the write
        out.append((tool, args))
    return out


def _run_scenario(home: Path, *, granted: bool, call_mcp=None) -> dict:
    _write_config(home, granted=granted)
    double = DispatchDouble()
    provider = CtxCheckpointMemoryProvider(dispatch=double.dispatch, call_mcp=call_mcp)
    provider.initialize("session-same", platform="cli", hermes_home=str(home))
    receipt = provider.on_pre_compress(_turns("first"), require_checkpoint=True)
    double.block_next_search = True
    with pytest.raises(CtxCheckpointError) as failure:
        provider.on_pre_compress(_turns("second"), require_checkpoint=True)
    double.release.set()
    assert double.returned.wait(10)
    return {"receipt": receipt, "error": str(failure.value), "calls": _normalized(double.ctx.calls)}


def test_without_the_grant_or_without_call_mcp_or_on_a_refused_grant_the_calls_are_the_same(
    _isolated_hermes_home, registered
):
    asked: list[tuple] = []

    def recorder(server, tool, arguments=None, timeout=30):
        asked.append((server, tool))
        raise AssertionError("call_mcp without the grant")

    def refusing(server, tool, arguments=None, timeout=30):
        asked.append(("refused", server, tool))
        raise PermissionError("Plugin 'ctx_checkpoint' is not allowed to call MCP server 'ctx'")

    before = _run_scenario(_isolated_hermes_home, granted=False)  # a Hermes without call_mcp
    no_grant = _run_scenario(_isolated_hermes_home, granted=False, call_mcp=recorder)
    assert asked == []
    refused = _run_scenario(_isolated_hermes_home, granted=True, call_mcp=refusing)
    # Refused once (before any transport work); every later call goes through the dispatch.
    assert asked == [("refused", "ctx", "search")]

    assert "abandoned" in before["error"] and "[timeout]" in before["error"]
    for other in (no_grant, refused):
        assert other == before


# -- with the grant: the bounded calls through call_mcp ----------------------------------------


def _envelope(raw: str) -> dict:
    from hermes_cli.plugins import PluginContext

    return PluginContext._mcp_envelope(raw)


def test_with_the_grant_the_checkpoint_calls_go_through_call_mcp_with_their_own_timeout(
    _isolated_hermes_home, registered
):
    _write_config(_isolated_hermes_home, granted=True)
    double = DispatchDouble()
    through_host: list[tuple[str, str, float]] = []

    def call_mcp(server, tool, arguments=None, timeout=30):
        through_host.append((server, tool, timeout))
        return _envelope(double.dispatch(mcp_tool_name(server, tool), dict(arguments or {})))

    dispatched: list[str] = []

    def dispatch(name, args, **kwargs):
        dispatched.append(name)
        return double.dispatch(name, args, **kwargs)

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch, call_mcp=call_mcp)
    provider.initialize("session-granted", platform="cli", hermes_home=str(_isolated_hermes_home))
    receipt = provider.on_pre_compress(_turns("first"), require_checkpoint=True)

    assert "ctx checkpoint head:" in receipt
    assert dispatched == []
    assert [tool for _server, tool, _t in through_host] == [tool for tool, _args in double.ctx.calls]
    assert {server for server, _tool, _t in through_host} == {"ctx"}
    # Each call carries the checkpoint's own bound (api_timeout 1 s here), clamped as Hermes clamps it.
    assert all(t == 1.0 for _server, _tool, t in through_host), through_host


def test_with_the_grant_an_answer_hermes_cut_fails_the_call(_isolated_hermes_home, registered):
    _write_config(_isolated_hermes_home, granted=True)

    def call_mcp(server, tool, arguments=None, timeout=30):
        return _envelope(json.dumps({"result": "x" * 70_000}))

    provider = CtxCheckpointMemoryProvider(dispatch=DispatchDouble().dispatch, call_mcp=call_mcp)
    provider.initialize("session-cut", platform="cli", hermes_home=str(_isolated_hermes_home))
    with pytest.raises(CtxCheckpointError, match="64 KB"):
        provider.on_pre_compress(_turns("first"), require_checkpoint=True)


def test_with_the_grant_a_tool_the_entry_filters_out_still_goes_through_the_dispatch(_isolated_hermes_home, registered):
    """call_mcp reaches every tool of a granted server; the entry's tools filter decides what Hermes
    registers. A tool it left out goes through the dispatch, as without the grant ("Unknown tool")."""
    registered.discard(mcp_tool_name("ctx", "store"))
    _write_config(_isolated_hermes_home, granted=True)
    double = DispatchDouble()
    through_host: list[str] = []

    def call_mcp(server, tool, arguments=None, timeout=30):
        through_host.append(tool)
        return _envelope(double.dispatch(mcp_tool_name(server, tool), dict(arguments or {})))

    dispatched: list[str] = []

    def dispatch(name, args, **kwargs):
        dispatched.append(name.rsplit("__", 1)[-1])
        return double.dispatch(name, args, **kwargs)

    provider = CtxCheckpointMemoryProvider(dispatch=dispatch, call_mcp=call_mcp)
    provider.initialize("session-filtered", platform="cli", hermes_home=str(_isolated_hermes_home))
    provider.on_pre_compress(_turns("first"), require_checkpoint=True)

    assert "store" not in through_host and set(through_host) == {"search"}
    assert dispatched and set(dispatched) == {"store"}


def test_the_largest_head_and_manifest_reads_stay_under_hermes_plugin_cap():
    """A chain reads its head and the head's manifest; through call_mcp an answer over Hermes'
    64 KB cap would fail every checkpoint of the root. The largest the writer makes: the part
    ceiling and the full fact-hash list, as ctx's get tool renders a block (indented JSON)."""
    from hermes_cli.plugins import PluginContext

    cp = _MODULE._cp
    ids = [f"019f7000-0000-7000-8000-{i:012d}" for i in range(cp.MAX_PARTS_CEILING)]
    hashes = ["f" * 64] * cp.FACT_HASHES_META_MAX
    metadata = {
        "source_block_ids": ids,
        "parent_manifest_id": ids[0],
        cp.FACT_HASHES_KEY: hashes,
        "root_session_id": "r" * 70,
        "active_session_id": "a" * 70,
        "sha256": "0" * 64,
        "warnings": ["W1", "W3", "W5", "W9", "W18", "W19", "W21"],
        "ctx_plugin": {"writer": "ctx_checkpoint", "port_of": cp.UPSTREAM_REF, "redaction": list(cp.REDACTION_PASSES)},
    }
    content = "# Compaction checkpoint manifest\n\n" + "x" * 600 + "\n".join(f"- `{i}`" for i in ids)
    block = {"id": ids[0], "title": "Compaction source " + "t" * 120, "content": content, "metadata": metadata}
    raw = json.dumps({"result": json.dumps(block, indent=2)})
    assert len(raw) < PluginContext._MCP_RESULT_CHAR_CAP, len(raw)
