"""Plugin 0.13.0 (GottZ/ctx#297): the ctx MCP tools are called and named the way Hermes registers
them.

Hermes registers the tools of an ``mcp_servers`` entry as ``mcp__<server>__<tool>`` with every
character outside ``[A-Za-z0-9_]`` replaced by ``_`` and a name over 64 characters shortened with a
hash suffix (``tools.mcp_tool_schema.mcp_prefixed_tool_name``, not part of Hermes' public plugin
API). The plugin follows the same rule in ``memory.mcp_client.mcp_tool_name``; these tests pin it
against Hermes' function, run the dispatch, the system prompt block and the connection test with
an entry name that Hermes rewrites, and refuse two entries whose tools Hermes would register under
the same names.
"""

import hashlib
import importlib
import importlib.util
import json
import sys
import types
from pathlib import Path

import pytest
from tools.mcp_tool_schema import mcp_prefixed_tool_name

PLUGIN_DIR = Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_tool_names", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
Provider = _MODULE.CtxCheckpointMemoryProvider
mcp_client = importlib.import_module(f"{_SPEC.name}.memory.mcp_client")
status = importlib.import_module(f"{_SPEC.name}.memory.status")

ROOT = "20261116_030000_abcdef"
TRANSCRIPT = [
    {"role": "user", "content": "keep this wording"},
    {"role": "assistant", "content": "kept"},
]
CTX_TOOLS = ["search", "get", "store", "query", "update", "archive"]
LONG = "ctx-" + "x" * 70


@pytest.mark.parametrize(
    "server",
    ["ctx", "kb", "ctx-prod", "ctx.prod", "ctx prod", "ctx_prod", "Ctx-Prod-2", "ünïcode", LONG, "a" * 50, "a" * 57],
)
@pytest.mark.parametrize("tool", ["search", "get", "store", "query"])
def test_the_tool_name_is_the_one_hermes_registers(server, tool):
    assert mcp_client.mcp_tool_name(server, tool) == mcp_prefixed_tool_name(server, tool)


def _registry_dispatch(servers, calls):
    """Hermes' registry for these entries: the tools under the names Hermes registers them with;
    any other name is unknown, as ``registry.dispatch`` answers it."""
    ids: dict[tuple[str, str], str] = {}
    known = {mcp_prefixed_tool_name(server, tool): tool for server in servers for tool in CTX_TOOLS}

    def dispatch(name, args, **kwargs):
        calls.append(name)
        tool = known.get(name)
        if tool is None:
            return json.dumps({"error": f"Unknown tool: {name}"})
        if tool == "search":
            return json.dumps({"result": "[]"})
        key = (args["category"], args["title"])
        ids.setdefault(key, f"019f6000-0000-7000-8000-{len(ids) + 1:012d}")
        return json.dumps({"id": ids[key]})

    dispatch.ids = ids
    return dispatch


def test_an_entry_name_with_a_hyphen_checkpoints_and_names_the_registered_tools():
    calls = []
    dispatch = _registry_dispatch(["ctx-prod"], calls)
    provider = Provider(config={"mcp_server": "ctx-prod"}, dispatch=dispatch)
    provider.initialize(ROOT, platform="cli")

    receipt = provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)

    assert "Unknown tool" not in receipt
    assert calls and set(calls) <= {"mcp__ctx_prod__search", "mcp__ctx_prod__store"}
    head = dispatch.ids[("compaction-checkpoints", f"Compaction checkpoint head {ROOT}")]
    block = provider.system_prompt_block()
    assert f'`mcp__ctx_prod__get` id="{head}"' in block and "`mcp__ctx_prod__search`" in block
    assert "mcp__ctx-prod__" not in block and "mcp__ctx-prod__" not in receipt
    assert "`mcp__ctx_prod__get`" in receipt


def test_a_long_entry_name_is_dispatched_under_hermes_shortened_name():
    calls = []
    provider = Provider(config={"mcp_server": LONG}, dispatch=_registry_dispatch([LONG], calls))
    provider.initialize(ROOT, platform="cli")

    provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)

    assert set(calls) <= {mcp_prefixed_tool_name(LONG, "search"), mcp_prefixed_tool_name(LONG, "store")}
    assert all(len(name) == 64 for name in calls)


def test_the_default_entry_keeps_its_names_and_its_pointer_binding_byte_for_byte():
    """Profiles with the entry ``ctx`` (or any name Hermes keeps) are unchanged: the dispatched
    names, the read path in the system prompt block and the binding digest the outcome file stores
    each head under (a changed digest would hide the pointer of every running session)."""
    calls = []
    dispatch = _registry_dispatch(["ctx"], calls)
    provider = Provider(config={}, dispatch=dispatch)
    provider.initialize(ROOT, platform="cli")

    provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)

    assert set(calls) == {"mcp__ctx__search", "mcp__ctx__store"} and calls.count("mcp__ctx__store") == 3
    raw = "mcp\0ctx\0compaction-checkpoints"
    assert provider._pointer_binding() == hashlib.sha256(raw.encode("utf-8")).hexdigest()[:16]
    head = dispatch.ids[("compaction-checkpoints", f"Compaction checkpoint head {ROOT}")]
    assert f'`mcp__ctx__get` id="{head}"' in provider.system_prompt_block()


def test_the_pointer_binding_stays_on_the_entry_name():
    """Two entries that Hermes names alike are two servers: a head recorded through one is not
    named for the other (the binding uses the entry name, not the tool name)."""
    hyphen = Provider(config={"mcp_server": "ctx-prod"}, dispatch=_registry_dispatch(["ctx-prod"], []))
    underscore = Provider(config={"mcp_server": "ctx_prod"}, dispatch=_registry_dispatch(["ctx_prod"], []))

    assert hyphen._pointer_binding() != underscore._pointer_binding()


def _write_config(home, config):
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")


ENTRY = {"url": "https://ctx.example.test/mcp", "auth": "oauth"}


@pytest.mark.parametrize(
    ("servers", "colliding"),
    [
        ({"ctx-prod": ENTRY, "ctx_prod": ENTRY}, ["ctx_prod"]),
        ({"ctx-prod": ENTRY, "ctx.prod": ENTRY, "ctx_prod": ENTRY}, ["ctx.prod", "ctx_prod"]),
        ({"ctx-prod": ENTRY, "ctx_prod": {**ENTRY, "enabled": False}}, []),
        ({"ctx-prod": ENTRY, "ctx-dev": ENTRY}, []),
    ],
    ids=["two-entries", "three-entries", "other-disabled", "distinct"],
)
def test_entries_whose_tools_hermes_names_alike_are_refused(_isolated_hermes_home, servers, colliding):
    """Hermes keeps the tools of the entry that connects first and skips the other's, so a dispatch
    by name could reach the other server: the provider is unavailable and says which entries."""
    _write_config(_isolated_hermes_home, {"mcp_servers": servers})
    provider = Provider(config={"mcp_server": "ctx-prod"})

    assert provider.is_available() is (not colliding)
    if colliding:
        reason = provider.unavailable_reason()
        assert "mcp__ctx_prod__" in reason
        for other in colliding:
            assert f"mcp_servers.{other}" in reason


def test_an_entry_named_like_another_plus_two_separators_is_refused(_isolated_hermes_home):
    """PR #306 review: Hermes joins server and tool part with ``__`` and merges nothing, so the
    ``store`` of ``ctx--prod`` and a tool ``prod__store`` of ``ctx`` get one name; Hermes keeps one
    registration of it. The other server's tools are not in config.yaml, so the entry is refused."""
    assert mcp_prefixed_tool_name("ctx--prod", "store") == mcp_prefixed_tool_name("ctx", "prod__store")
    _write_config(_isolated_hermes_home, {"mcp_servers": {"ctx": ENTRY, "ctx--prod": ENTRY}})

    refused = Provider(config={"mcp_server": "ctx--prod"})
    assert refused.is_available() is False
    assert "mcp_servers.ctx " in refused.unavailable_reason() + " "

    # The other way round nothing can collide: the checkpoint tools have no "__" in their names.
    assert Provider(config={"mcp_server": "ctx"}).is_available() is True


@pytest.mark.parametrize("other", ["ctx-prod", "ctx_prod", "ctx.prod", "ctxprod", "ctx-prod-2"])
def test_an_entry_named_like_another_plus_one_separator_is_kept(_isolated_hermes_home, other):
    _write_config(_isolated_hermes_home, {"mcp_servers": {"ctx": ENTRY, other: ENTRY}})

    assert Provider(config={"mcp_server": other}).is_available() is True
    assert Provider(config={"mcp_server": "ctx"}).is_available() is True


_NAMES = sorted(
    {
        f"{base}{sep}{rest}"
        for base in ("ctx", "ctx_", "c", "kb")
        for sep in ("", "-", "_", ".", "--", "__", "-_", "_-", "-.", "___")
        for rest in ("", "prod", "_prod", "x" * 70)
    }
)


def test_the_collision_rule_is_exactly_a_possible_shared_name_in_hermes():
    """For every pair of entry names: refused exactly when some tool of the other entry gets the
    name Hermes gives a checkpoint tool of this one — the witness tool is built and checked with
    Hermes' own function (shortened names included)."""
    mismatches = []
    for server in _NAMES:
        for other in _NAMES:
            if other == server:
                continue
            possible = False
            for tool in ("search", "get", "store", "query"):
                full = f"mcp__{mcp_client._tool_name_part(server)}__{tool}"
                prefix = f"mcp__{mcp_client._tool_name_part(other)}__"
                if full.startswith(prefix) and len(full) > len(prefix):
                    witness = full[len(prefix) :]
                    possible |= mcp_prefixed_tool_name(other, witness) == mcp_prefixed_tool_name(server, tool)
            refused = bool(mcp_client.colliding_servers(server, {server: ENTRY, other: ENTRY}))
            if refused != possible:
                mismatches.append((server, other, refused, possible))
    assert not mismatches, mismatches[:10]


def test_without_its_own_entry_the_reason_names_the_missing_entry(_isolated_hermes_home):
    _write_config(_isolated_hermes_home, {"mcp_servers": {"ctx_prod": ENTRY}})
    provider = Provider(config={"mcp_server": "ctx-prod"})

    assert provider.is_available() is False
    assert provider.unavailable_reason().startswith("configure the ctx MCP server entry mcp_servers.ctx-prod")


@pytest.fixture
def probe(monkeypatch):
    state = types.SimpleNamespace(calls=[])

    def fake(name, entry, timeout):
        state.calls.append(name)
        return list(CTX_TOOLS)

    monkeypatch.setattr(status, "probe_server", fake)
    return state


def test_the_connection_test_names_the_registered_tools_and_refuses_a_collision(_isolated_hermes_home, probe):
    _write_config(_isolated_hermes_home, {"mcp_servers": {"ctx-prod": ENTRY}})
    passed = status.run_connection_test("ctx-prod", ENTRY, servers={"ctx-prod": ENTRY})
    assert passed.ok is True
    assert "mcp__ctx_prod__search" in passed.detail and "mcp__ctx_prod__store" in passed.detail

    plain = status.run_connection_test("ctx", ENTRY, servers={"ctx": ENTRY})
    assert plain.ok is True and "mcp__" not in plain.detail  # the name Hermes keeps: nothing to point out

    refused = status.run_connection_test("ctx-prod", ENTRY, servers={"ctx-prod": ENTRY, "ctx_prod": ENTRY})
    assert refused.ok is False
    assert "mcp_servers.ctx_prod" in refused.detail and "mcp__ctx_prod__" in refused.detail
    assert probe.calls == ["ctx-prod", "ctx"]  # a collision is refused before any connection


def test_status_and_the_desktop_save_refuse_a_collision(_isolated_hermes_home, probe, monkeypatch):
    _write_config(
        _isolated_hermes_home,
        {
            "mcp_servers": {"ctx-prod": ENTRY, "ctx_prod": ENTRY},
            "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {"mcp_server": "ctx-prod"}},
        },
    )
    provider = Provider()

    report = provider.get_status_config({})
    assert report["ok"] is False and "mcp_servers.ctx_prod" in report["summary"]

    with pytest.raises(ValueError, match=r"mcp_servers\.ctx_prod"):
        provider.write_config_fields({"ctx_url": "https://ctx2.example.test/mcp"}, str(_isolated_hermes_home))
    assert probe.calls == []


def test_the_failure_hint_reads_the_registered_name():
    hint = status.failure_warning(
        {"error": "ctx checkpoint head lookup failed: Unknown tool: mcp__ctx_prod__search"}, "ctx-prod"
    )
    assert "is not connected in this Hermes" in hint and "`hermes mcp test ctx-prod`" in hint
