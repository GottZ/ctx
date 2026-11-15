"""Plugin 0.8.0: the Desktop memory form (declared surface), the Settings → Plugins section,
the connection test, the checkpoint outcome file and the status on the MCP transport.

Like ``test_setup.py`` these run against the real Hermes host on ``PYTHONPATH`` with an
isolated ``HERMES_HOME`` per test; config.yaml and .env are written by the host's own
``hermes_cli.config``. The connection test is replaced by a fake (``memory.status.probe_server``):
no test opens a network connection.
"""

import importlib
import importlib.util
import json
import logging
import os
import sys
import time
import types
from pathlib import Path

import pytest
from hermes_cli.config import read_user_config_raw

PLUGIN_DIR = Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_surfaces", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
Provider = _MODULE.CtxCheckpointMemoryProvider
KEY_ENV = _MODULE.SETUP_API_KEY_ENV
CTX_TOOLS = ["search", "get", "store", "query", "update", "archive"]
SECRET = "ctx-key-0123456789abcdef"


def _submodule(name):
    return importlib.import_module(f"{_SPEC.name}.{name}")


@pytest.fixture
def hermes_home(tmp_path, monkeypatch):
    home = tmp_path / "hermes-home"
    home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(home))
    monkeypatch.delenv(KEY_ENV, raising=False)
    monkeypatch.setattr(_MODULE, "_hygiene_warning_emitted", False)
    return home


@pytest.fixture
def probe(monkeypatch):
    """Fake connection test: records (name, entry) and answers ``tools`` or raises ``error``."""
    state = types.SimpleNamespace(calls=[], timeouts=[], tools=list(CTX_TOOLS), error=None)

    def fake(name, entry, timeout):
        state.calls.append((name, json.loads(json.dumps(entry))))
        state.timeouts.append(timeout)
        if state.error is not None:
            raise state.error
        return list(state.tools)

    monkeypatch.setattr(_submodule("memory.status"), "probe_server", fake)
    return state


@pytest.fixture
def linked_plugin(hermes_home):
    """The plugin as a user plugin of the isolated home, where the host's loader finds it."""
    plugins = hermes_home / "plugins"
    plugins.mkdir()
    (plugins / "ctx_checkpoint").symlink_to(PLUGIN_DIR, target_is_directory=True)
    return plugins / "ctx_checkpoint"


def _write_config(home, config):
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")


def _read_config(home):
    path = home / "config.yaml"
    return read_user_config_raw(path) if path.exists() else {}


def _env_text(home):
    path = home / ".env"
    return path.read_text(encoding="utf-8") if path.exists() else ""


def _ok_dispatch():
    calls = []

    def dispatch(name, args, **kwargs):
        calls.append((name, args))
        if name.endswith("__search"):
            return json.dumps({"result": "[]"})
        return json.dumps({"id": f"019f6000-0000-7000-8000-{len(calls):012d}"})

    return dispatch, calls


def _unknown_tool_dispatch(name, args, **kwargs):
    return json.dumps({"error": f"Unknown tool: {name}"})


TRANSCRIPT = [
    {"role": "user", "content": "please keep this wording"},
    {"role": "assistant", "content": "kept, word for word"},
]


# -- A. Desktop memory form (declared surface) -------------------------------------------


def test_desktop_memory_form_is_declared_from_the_one_field_table(hermes_home, linked_plugin):
    from plugins.memory.config_schema import STORAGE_PROVIDER, get_provider_config_schema

    declared = get_provider_config_schema("ctx_checkpoint")

    assert declared is not None, "Desktop's Settings -> Memory form needs config_schema.py"
    assert declared.storage == STORAGE_PROVIDER
    legacy = Provider(config={}).get_config_schema()
    desktop_only = {spec["key"] for spec in _submodule("config_schema").FIELDS if spec.get("desktop_only")}
    assert desktop_only == {"checkpoint_required"}
    assert [f.key for f in declared.fields if f.key not in desktop_only] == [f["key"] for f in legacy]
    by_key = {f.key: f for f in declared.fields}
    for field in legacy:
        assert by_key[field["key"]].label == field["label"], field["key"]
        assert by_key[field["key"]].description == field["description"], field["key"]
    assert by_key["ctx_api_key"].kind == "secret" and by_key["ctx_api_key"].env_key == KEY_ENV
    assert {f.key for f in declared.fields if f.inline} == {"ctx_url", "ctx_api_key", "memory", "checkpoint_required"}


def test_declared_defaults_are_the_plugin_defaults(hermes_home, linked_plugin):
    from plugins.memory.config_schema import get_provider_config_schema

    declared = {f.key: f for f in get_provider_config_schema("ctx_checkpoint").fields}

    assert declared["sensitivity"].default == _MODULE._DEFAULT_SENSITIVITY
    assert [o.value for o in declared["sensitivity"].options] == list(_MODULE._SENSITIVITY_CHOICES)
    assert declared["category"].default == _MODULE._DEFAULT_CATEGORY
    assert declared["chunk_chars"].default == str(_MODULE._DEFAULT_CHUNK_CHARS)
    assert declared["memory"].default == "false"
    assert declared["checkpoint_required"].default == "false"
    table = {spec["key"]: spec for spec in _submodule("config_schema").FIELDS}
    assert (table["chunk_chars"]["minimum"], table["chunk_chars"]["maximum"]) == (
        _MODULE._MIN_CHUNK_CHARS,
        _MODULE._MAX_CHUNK_CHARS,
    )


def _desktop_save(monkeypatch, provider, values):
    """PUT /api/memory/providers/ctx_checkpoint/config?surface=declared, minus FastAPI."""
    routes = pytest.importorskip("hermes_cli.web_routers.memory_providers")
    from plugins.memory.config_schema import get_provider_config_schema

    monkeypatch.setattr(routes, "_load_memory_provider", lambda name, **kwargs: provider)
    declared = get_provider_config_schema("ctx_checkpoint")
    routes._update_memory_provider_config(declared, {k: routes._stringify_submitted(v) for k, v in values.items()})
    return routes, declared


def test_desktop_form_save_tests_the_connection_and_writes_through_the_provider(
    hermes_home, linked_plugin, probe, monkeypatch
):
    provider = Provider(config={})

    routes, declared = _desktop_save(
        monkeypatch,
        provider,
        {"ctx_url": "https://ctx.example.test/mcp", "ctx_api_key": SECRET, "memory": True},
    )

    assert [name for name, _entry in probe.calls] == ["ctx"]
    tested = probe.calls[0][1]
    assert tested["url"] == "https://ctx.example.test/mcp"
    assert tested["headers"] == {"Authorization": f"Bearer {SECRET}"}  # the submitted key, before .env has it
    config = _read_config(hermes_home)
    assert config["mcp_servers"]["ctx"] == {
        "url": "https://ctx.example.test/mcp",
        "headers": {"Authorization": "Bearer ${" + KEY_ENV + "}"},
    }
    assert config["memory"]["provider"] == "ctx_checkpoint"
    assert config["memory"]["ctx_checkpoint"]["memory"] is True
    assert "compression" not in config
    assert f"{KEY_ENV}={SECRET}" in _env_text(hermes_home)
    assert SECRET not in (hermes_home / "config.yaml").read_text(encoding="utf-8")

    payload = {f["key"]: f for f in routes._declared_provider_payload(declared)["fields"]}
    assert payload["ctx_url"]["value"] == "https://ctx.example.test/mcp"
    assert payload["memory"]["value"] == "true"
    assert payload["checkpoint_required"]["value"] == "false"
    assert payload["ctx_api_key"]["is_set"] is True and payload["ctx_api_key"]["value"] == ""
    assert payload["sensitivity"]["value"] == "internal"


def test_desktop_form_refuses_a_save_whose_connection_test_fails(hermes_home, linked_plugin, probe, monkeypatch):
    probe.error = RuntimeError(f"HTTP 401 Unauthorized (Authorization: Bearer {SECRET})")

    with pytest.raises(ValueError) as refused:
        _desktop_save(
            monkeypatch, Provider(config={}), {"ctx_url": "https://ctx.example.test/mcp", "ctx_api_key": SECRET}
        )

    message = str(refused.value)
    assert "connection test" in message.lower()
    assert "401" in message
    assert SECRET not in message
    assert "mcp_servers" not in _read_config(hermes_home)
    assert SECRET not in _env_text(hermes_home)


def test_desktop_form_refuses_a_server_without_the_ctx_tools(hermes_home, linked_plugin, probe, monkeypatch):
    probe.tools = ["read_file", "write_file"]

    with pytest.raises(ValueError, match="store"):
        _desktop_save(
            monkeypatch, Provider(config={}), {"ctx_url": "https://files.example.test/mcp", "ctx_api_key": SECRET}
        )


def test_desktop_inline_panel_saves_one_field_at_a_time(hermes_home, linked_plugin, probe, monkeypatch):
    provider = Provider(config={})

    # The compact panel autosaves each field on its own: URL first, the key not known yet.
    _desktop_save(monkeypatch, provider, {"ctx_url": "https://ctx.example.test/mcp"})
    assert probe.calls == []
    assert _read_config(hermes_home)["mcp_servers"]["ctx"]["url"] == "https://ctx.example.test/mcp"

    _desktop_save(monkeypatch, provider, {"ctx_api_key": SECRET})
    assert len(probe.calls) == 1
    assert probe.calls[0][1]["headers"] == {"Authorization": f"Bearer {SECRET}"}
    assert f"{KEY_ENV}={SECRET}" in _env_text(hermes_home)

    _desktop_save(monkeypatch, provider, {"category": "my-checkpoints"})
    assert len(probe.calls) == 1  # no connection field changed
    assert _read_config(hermes_home)["memory"]["ctx_checkpoint"]["category"] == "my-checkpoints"


def test_turning_required_on_needs_a_passing_test_and_writes_one_key(hermes_home, linked_plugin, probe, monkeypatch):
    (hermes_home / ".env").write_text(f"{KEY_ENV}={SECRET}\n", encoding="utf-8")
    _write_config(
        hermes_home,
        {
            "mcp_servers": {
                "ctx": {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${CTX_API_KEY}"}}
            },
            "compression": {"threshold": 0.6},
            "memory": {"provider": "ctx_checkpoint"},
        },
    )
    provider = Provider(config={})
    probe.error = RuntimeError("connection refused")

    with pytest.raises(ValueError, match="connection refused"):
        _desktop_save(monkeypatch, provider, {"checkpoint_required": True})
    assert _read_config(hermes_home)["compression"] == {"threshold": 0.6}

    probe.error = None
    _desktop_save(monkeypatch, provider, {"checkpoint_required": True})
    assert _read_config(hermes_home)["compression"] == {"threshold": 0.6, "checkpoint_required": True}
    assert len(probe.calls) == 2

    _desktop_save(monkeypatch, provider, {"checkpoint_required": False})
    assert _read_config(hermes_home)["compression"] == {"threshold": 0.6, "checkpoint_required": False}
    assert len(probe.calls) == 2  # turning it off needs no test


def test_desktop_form_writes_no_mode_it_did_not_change(hermes_home, linked_plugin, probe, monkeypatch):
    import hermes_cli.config as host_config

    # memory.provider is set already, so the route itself writes nothing; every write is the plugin's
    _write_config(
        hermes_home,
        {
            "mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp", "auth": "oauth"}},
            "memory": {"provider": "ctx_checkpoint"},
        },
    )
    writes = []
    real_save = host_config.save_config

    def spy(changes, *args, **kwargs):
        writes.append(json.loads(json.dumps(changes)))
        return real_save(changes, *args, **kwargs)

    monkeypatch.setattr(host_config, "save_config", spy)

    _desktop_save(monkeypatch, Provider(), {"checkpoint_required": False, "category": "mine"})

    # Not written at all, not merely dropped by Hermes as a default value.
    assert writes and all("compression" not in change for change in writes)
    assert probe.calls == []


def test_web_dashboard_save_never_changes_the_operating_mode(hermes_home, probe, monkeypatch):
    # The dashboard sends every field with the value it loaded. The mode was switched on
    # elsewhere after the page loaded; saving another field must not switch it off again.
    routes = pytest.importorskip("hermes_cli.web_routers.memory_providers")
    import hermes_cli.config as host_config

    _write_config(
        hermes_home,
        {
            "mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp", "auth": "oauth"}},
            "compression": {"checkpoint_required": True},
        },
    )
    writes = []
    real_save = host_config.save_config

    def spy(changes, *args, **kwargs):
        writes.append(json.loads(json.dumps(changes)))
        return real_save(changes, *args, **kwargs)

    monkeypatch.setattr(host_config, "save_config", spy)
    provider = Provider(config={})
    assert "checkpoint_required" not in {f["key"] for f in provider.get_config_schema()}

    routes._write_memory_provider_config_values("ctx_checkpoint", provider, {"category": "mine"})
    provider.save_config({"checkpoint_required": "off"}, str(hermes_home))  # a stale value sent anyway

    assert writes and all("compression" not in change for change in writes)
    assert _read_config(hermes_home)["compression"] == {"checkpoint_required": True}
    assert _read_config(hermes_home)["memory"]["ctx_checkpoint"]["category"] == "mine"
    assert probe.calls == []


def test_desktop_form_refuses_turning_required_on_before_a_key_exists(hermes_home, linked_plugin, probe, monkeypatch):
    _write_config(hermes_home, {"mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp"}}})

    with pytest.raises(ValueError, match="Save the ctx API key first"):
        _desktop_save(monkeypatch, Provider(config={}), {"checkpoint_required": True})

    assert "compression" not in _read_config(hermes_home)
    assert probe.calls == []


def test_desktop_form_refuses_a_url_for_a_stdio_entry(hermes_home, linked_plugin, probe, monkeypatch):
    _write_config(hermes_home, {"mcp_servers": {"ctx": {"command": "ctx", "args": ["mcp"]}}})

    with pytest.raises(ValueError, match="stdio"):
        _desktop_save(monkeypatch, Provider(config={}), {"ctx_url": "https://ctx.example.test/mcp"})

    assert _read_config(hermes_home)["mcp_servers"]["ctx"] == {"command": "ctx", "args": ["mcp"]}


def test_desktop_form_on_a_package_managed_install_is_refused_as_bad_request(hermes_home, monkeypatch):
    import hermes_cli.config as host_config

    monkeypatch.setattr(host_config, "is_managed", lambda: True)

    # The declared route turns a ValueError into 400 with its text; anything else is a 500.
    with pytest.raises(ValueError, match="package-managed") as refused:
        Provider(config={}).write_config_fields({"category": "mine"}, str(hermes_home))
    assert isinstance(refused.value, RuntimeError)  # what earlier callers caught


def test_desktop_form_cleared_field_goes_back_to_its_default(hermes_home, linked_plugin, probe, monkeypatch):
    _write_config(
        hermes_home,
        {
            "mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp", "auth": "oauth"}},
            "memory": {"ctx_checkpoint": {"chunk_chars": 20000, "category": "mine"}},
        },
    )
    provider = Provider()  # as Hermes loads it: the section from config.yaml

    provider.write_config_fields({"chunk_chars": None}, str(hermes_home))

    section = _read_config(hermes_home)["memory"]["ctx_checkpoint"]
    assert section["chunk_chars"] == _MODULE._DEFAULT_CHUNK_CHARS
    assert section["category"] == "mine"


def test_a_hermes_without_the_probe_does_not_block_setup(hermes_home, linked_plugin, probe, monkeypatch, capsys):
    probe.error = ImportError("no _probe_single_server")
    _desktop_save(monkeypatch, Provider(config={}), {"ctx_url": "https://ctx.example.test/mcp", "ctx_api_key": SECRET})
    assert _read_config(hermes_home)["mcp_servers"]["ctx"]["url"] == "https://ctx.example.test/mcp"

    status = Provider(config={}).get_status_config({})
    assert status["ok"] is None
    assert "no MCP connection test" in status["summary"]

    _terminal_answers(monkeypatch, "https://ctx.example.test/mcp", "on")
    Provider(config={}).post_setup(str(hermes_home), {})
    out = capsys.readouterr().out
    assert "once the connection test passes" in out  # the mode is not offered without a test
    assert "compression" not in _read_config(hermes_home)


def test_a_changed_probe_signature_counts_as_no_test(monkeypatch):
    import hermes_cli.mcp_config as host_mcp

    status = _submodule("memory.status")

    def probe_without_timeout(name, config):
        raise AssertionError("must not be called")

    monkeypatch.setattr(host_mcp, "_probe_single_server", probe_without_timeout)
    monkeypatch.setattr(status, "probe_server", status.host_probe)

    test = status.run_connection_test("ctx", {"url": "https://ctx.example.test/mcp"})

    assert test.ok is None


def test_an_entry_with_a_longer_connect_timeout_keeps_it(hermes_home, linked_plugin, probe, monkeypatch):
    _write_config(hermes_home, {"mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp", "connect_timeout": 60}}})

    _desktop_save(monkeypatch, Provider(config={}), {"ctx_api_key": SECRET})

    assert probe.timeouts == [60.0]
    _desktop_save(monkeypatch, Provider(config={}), {"ctx_url": "https://ctx2.example.test/mcp"})
    assert probe.timeouts[-1] == 60.0
    _write_config(hermes_home, {"mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp"}}})
    Provider(config={}).get_status_config({})
    assert probe.timeouts[-1] == _submodule("memory.status").CONNECTION_TEST_TIMEOUT_S


# -- B. Settings -> Plugins section ---------------------------------------------------------


def _manifest():
    import hermes_yaml

    return hermes_yaml.safe_load((PLUGIN_DIR / "plugin.yaml").read_text(encoding="utf-8"))


def test_plugins_section_fields_and_defaults_match_the_plugin(hermes_home):
    from hermes_cli.plugins_settings import plugin_settings_fields

    fields = {f["key"]: f for f in plugin_settings_fields("ctx_checkpoint", PLUGIN_DIR)}
    engine = _submodule("memory.config")

    assert tuple(fields) == engine.PLUGINS_FORM_KEYS
    assert fields["budget_s"]["type"] == "number" and fields["budget_s"]["default"] == _MODULE._DEFAULT_BUDGET_S
    assert fields["max_parts"]["type"] == "number" and fields["max_parts"]["default"] == _MODULE._DEFAULT_MAX_PARTS
    defaults = engine._default_config()
    for key in ("auto_recall", "auto_memory_mirror", "auto_compress_extract", "auto_session_digest"):
        assert fields[key]["type"] == "boolean", key
        assert fields[key]["default"] is defaults[key], key
    for field in fields.values():
        assert field["label"] and field["description"], field["key"]
    memory_form = {spec["key"] for spec in _submodule("config_schema").FIELDS}
    assert not memory_form & set(fields), "a key may live in one form only"


def test_plugins_section_values_reach_the_provider_with_the_documented_precedence(hermes_home, linked_plugin):
    from hermes_cli.plugins_settings import save_plugin_settings

    (hermes_home / "ctx.json").write_text(
        json.dumps({"checkpoint_budget_s": 20, "auto_recall": True}), encoding="utf-8"
    )
    _write_config(
        hermes_home,
        {"memory": {"ctx_checkpoint": {"budget_s": 30, "max_parts": 50, "memory": True, "category": "mine"}}},
    )
    save_plugin_settings("ctx_checkpoint", linked_plugin, {"budget_s": 90, "auto_recall": False})

    provider = Provider(dispatch=_ok_dispatch()[0])
    provider.initialize("session-precedence", platform="cli", hermes_home=str(hermes_home))

    assert provider._section["budget_s"] == 90.0  # Settings -> Plugins wins over memory.ctx_checkpoint
    assert provider._section["max_parts"] == 50  # not set in the Plugins form: memory.ctx_checkpoint
    assert provider._section["category"] == "mine"
    assert provider._config["checkpoint_budget_s"] == 90.0  # ... and over ctx.json
    assert provider._config["auto_recall"] is False


def test_plugins_section_values_are_clamped_like_handwritten_ones(hermes_home, linked_plugin):
    from hermes_cli.plugins_settings import save_plugin_settings

    save_plugin_settings("ctx_checkpoint", linked_plugin, {"budget_s": 9999, "max_parts": 0})

    section = Provider()._section
    assert section["budget_s"] == _MODULE._MAX_BUDGET_S
    assert section["max_parts"] == 1


def test_plugins_section_ignores_undeclared_keys(hermes_home):
    _write_config(
        hermes_home,
        {
            "memory": {"ctx_checkpoint": {"category": "from-memory-form"}},
            "plugins": {"entries": {"ctx_checkpoint": {"settings": {"category": "hand-edited", "transport": "rest"}}}},
        },
    )

    section = Provider()._section
    assert section["category"] == "from-memory-form"
    assert section["transport"] == "mcp"


# -- C. Status on the MCP transport --------------------------------------------------------


def _mcp_home(home, url="https://ctx.example.test/mcp", compression=None):
    config = {
        "mcp_servers": {"ctx": {"url": url, "headers": {"Authorization": "Bearer ${CTX_API_KEY}"}}},
        "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {}},
    }
    if compression is not None:
        config["compression"] = compression
    _write_config(home, config)
    (home / ".env").write_text(f"{KEY_ENV}={SECRET}\n", encoding="utf-8")


def _status_text(status):
    return "\n".join(f"{k}: {v}" for k, v in status.items())


def test_memory_status_on_mcp_reports_server_test_mode_and_last_checkpoint(hermes_home, probe):
    _mcp_home(hermes_home)
    provider = Provider(config={})

    status = provider.get_status_config({})

    text = _status_text(status)
    assert status["ok"] is True
    assert "CTX_BASE_URL" not in text and "not configured" not in text
    assert "mcp_servers.ctx" in status["summary"]
    assert "6 tools" in status["summary"]
    assert "best effort" in status["mode"]
    assert "compression.checkpoint_required" in status["mode"]
    assert "none" in status["last_checkpoint"]
    assert probe.calls[0][0] == "ctx"


def test_memory_status_names_the_last_checkpoint(hermes_home, probe):
    _mcp_home(hermes_home, compression={"checkpoint_required": True})
    dispatch, _calls = _ok_dispatch()
    provider = Provider(config={}, dispatch=dispatch)
    provider.initialize("session-status", platform="cli", hermes_home=str(hermes_home))
    provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)
    head = provider._last_chain_receipt.head_id

    status = Provider(config={}).get_status_config({})

    assert "required" in status["mode"]
    assert head in status["last_checkpoint"]
    assert "1 part" in status["last_checkpoint"]
    assert "saved" in status["last_checkpoint"]


@pytest.mark.parametrize(
    "error",
    [f"401 Unauthorized for Authorization: Bearer {SECRET}", f"401 Unauthorized: unknown key {SECRET}"],
)
def test_memory_status_reports_a_failed_test_without_the_key(hermes_home, probe, error):
    _mcp_home(hermes_home)
    probe.error = RuntimeError(error)

    status = Provider(config={}).get_status_config({})

    assert status["ok"] is False
    assert "401" in status["summary"]
    assert SECRET not in _status_text(status)


@pytest.mark.parametrize(
    "servers, expect",
    [
        ({}, "mcp_servers.ctx"),
        ({"ctx": {"url": "https://ctx.example.test/mcp", "enabled": False}}, "disabled"),
    ],
)
def test_memory_status_without_a_usable_entry_names_the_fix(hermes_home, probe, servers, expect):
    _write_config(hermes_home, {"mcp_servers": servers})

    status = Provider(config={}).get_status_config({})

    assert status["ok"] is False
    assert expect in status["summary"]
    assert "hermes memory setup ctx_checkpoint" in status["summary"]
    assert probe.calls == []


def test_hermes_ctx_checkpoint_status_shows_the_same_report(hermes_home, probe, capsys):
    _mcp_home(hermes_home)
    cli = _submodule("memory.cli")

    rc = cli.cmd_status(types.SimpleNamespace(), hermes_home, cli._load(hermes_home))

    out = capsys.readouterr().out
    assert rc == 0
    assert "mcp_servers.ctx" in out
    assert "best effort" in out
    assert "last_checkpoint" in out

    probe.error = RuntimeError("connection refused")
    assert cli.cmd_status(types.SimpleNamespace(), hermes_home, cli._load(hermes_home)) == 1


@pytest.mark.parametrize(
    "url, warns",
    [
        ("http://203.0.113.7/mcp", True),
        ("http://ctx.example.com/mcp", True),
        ("HTTP://ctx.example.com/mcp", True),
        ("http://127.0.0.1:8080/mcp", False),
        ("http://localhost:8080/mcp", False),
        ("http://10.0.0.5/mcp", False),
        ("http://192.168.1.20:8080/mcp", False),
        ("http://[::1]:8080/mcp", False),
        ("http://box.local/mcp", False),
        ("http://ctx:8080/mcp", False),
        ("https://ctx.example.com/mcp", False),
    ],
)
def test_plain_http_to_a_remote_host_warns_in_status(hermes_home, probe, url, warns):
    _mcp_home(hermes_home, url=url)

    text = _status_text(Provider(config={}).get_status_config({}))

    assert ("unencrypted" in text) is warns


def test_terminal_setup_warns_about_plain_http_to_a_remote_host(hermes_home, probe, monkeypatch, capsys):
    monkeypatch.setattr(
        "builtins.input", lambda prompt="": {"  ctx server URL: ": "http://ctx.example.com/mcp"}.get(prompt, "")
    )
    monkeypatch.setattr(_MODULE.getpass, "getpass", lambda prompt="": SECRET)

    Provider(config={}).post_setup(str(hermes_home), {})

    assert "unencrypted" in capsys.readouterr().out
    assert _read_config(hermes_home)["mcp_servers"]["ctx"]["url"] == "http://ctx.example.com/mcp"


# -- D. Outcome of every checkpoint, and the reason for the user ----------------------------


def _outcome(home):
    status = _submodule("memory.status")
    return json.loads((home / "ctx_state" / status.STATUS_FILE).read_text(encoding="utf-8"))["last_checkpoint"]


def test_a_saved_checkpoint_is_recorded(hermes_home):
    dispatch, _calls = _ok_dispatch()
    provider = Provider(config={}, dispatch=dispatch)
    provider.initialize("session-ok", platform="cli", hermes_home=str(hermes_home))

    provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)

    outcome = _outcome(hermes_home)
    assert outcome["ok"] is True
    assert outcome["mode"] == "required"
    assert outcome["head_id"] == provider._last_chain_receipt.head_id
    assert outcome["parts"] == 1
    assert outcome["error"] == ""
    assert outcome["at"].endswith("Z")
    assert outcome["context"] == "cli/?"


def test_the_latest_failure_stays_visible_after_another_session_succeeds(hermes_home, probe):
    _mcp_home(hermes_home)
    failing = Provider(config={}, dispatch=_unknown_tool_dispatch)
    failing.initialize("session-gateway", platform="discord", agent_context="primary", hermes_home=str(hermes_home))
    failing.on_pre_compress(TRANSCRIPT)
    working = Provider(config={}, dispatch=_ok_dispatch()[0])
    working.initialize("session-cli", platform="cli", agent_context="primary", hermes_home=str(hermes_home))
    working.on_pre_compress(TRANSCRIPT)

    status = Provider(config={}).get_status_config({})

    assert "saved" in status["last_checkpoint"] and "cli/primary" in status["last_checkpoint"]
    assert "FAILED" in status["last_failure"] and "discord/primary" in status["last_failure"]


@pytest.mark.parametrize(
    "cause, hint",
    [
        (
            "ctx checkpoint needs 91 parts > max_parts 80 (write rate limit 100/60s per key); nothing written",
            "max_parts",
        ),
        ("ctx checkpoint budget 60s exceeded before part 3 store", "budget_s"),
        ("ctx checkpoint rate-limited during part 2 store (budget 60s)", "100 per minute"),
        ("ctx checkpoint store rejected [confirm_writes]: staged", "confirm_writes"),
        ("ctx checkpoint head search failed: boom", "hermes ctx_checkpoint status"),
    ],
)
def test_the_failure_notice_names_a_next_step(cause, hint):
    status = _submodule("memory.status")

    text = status.failure_warning({"mode": "required", "error": cause}, "ctx")

    assert cause in text and hint in text


def test_a_failed_best_effort_checkpoint_is_recorded_and_reported(hermes_home):
    seen = []
    provider = Provider(config={}, dispatch=_unknown_tool_dispatch)
    provider.initialize("session-be", platform="cli", hermes_home=str(hermes_home), warning_callback=seen.append)

    assert provider.on_pre_compress(TRANSCRIPT) == ""

    outcome = _outcome(hermes_home)
    assert outcome["ok"] is False
    assert outcome["mode"] == "best_effort"
    assert "Unknown tool" in outcome["error"]
    assert len(seen) == 1
    assert "compacted the conversation without saving" in seen[0]
    assert "hermes mcp test ctx" in seen[0]


def test_a_failed_required_checkpoint_tells_the_user_why_once(hermes_home):
    seen = []
    provider = Provider(config={}, dispatch=_unknown_tool_dispatch)
    provider.initialize("session-req", platform="cli", hermes_home=str(hermes_home), warning_callback=seen.append)

    for _ in range(2):
        with pytest.raises(_MODULE.CtxCheckpointError):
            provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)

    assert len(seen) == 1
    assert "not compacted" in seen[0]
    assert "is not connected" in seen[0]
    assert _outcome(hermes_home)["mode"] == "required"

    provider._dispatch = _ok_dispatch()[0]
    provider._connect()
    provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)
    provider._dispatch = _unknown_tool_dispatch
    provider._connect()
    with pytest.raises(_MODULE.CtxCheckpointError):
        provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)
    assert len(seen) == 2  # a new failure after a success is reported again


def test_a_checkpoint_without_a_chain_is_not_reported_as_the_previous_one(hermes_home):
    dispatch, _calls = _ok_dispatch()
    provider = Provider(config={}, dispatch=dispatch)
    provider.initialize("session-stale", platform="cli", hermes_home=str(hermes_home))
    provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)
    assert _outcome(hermes_home)["ok"] is True

    provider._active = False  # e.g. a re-initialization that ended unbound
    assert provider.on_pre_compress(TRANSCRIPT) == ""

    outcome = _outcome(hermes_home)
    assert outcome["ok"] is False
    assert outcome["head_id"] == ""
    assert outcome["skipped"] == "the provider is not connected"


def test_a_failing_warning_callback_never_changes_the_checkpoint_path(hermes_home):
    def broken(message):
        raise RuntimeError("sink gone")

    provider = Provider(config={}, dispatch=_unknown_tool_dispatch)
    provider.initialize("session-broken-sink", platform="cli", hermes_home=str(hermes_home), warning_callback=broken)

    assert provider.on_pre_compress(TRANSCRIPT) == ""
    with pytest.raises(_MODULE.CtxCheckpointError):
        provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)


def test_the_outcome_file_survives_state_pruning(hermes_home):
    dispatch, _calls = _ok_dispatch()
    provider = Provider(config={}, dispatch=dispatch)
    provider.initialize("session-prune", platform="cli", hermes_home=str(hermes_home))
    provider.on_pre_compress(TRANSCRIPT, require_checkpoint=True)
    state = hermes_home / "ctx_state"
    state.mkdir(exist_ok=True)
    ledger = state / "old-root.json"
    ledger.write_text("{}", encoding="utf-8")
    old = time.time() - 90 * 86400
    for path in state.iterdir():
        os.utime(path, (old, old))

    removed = _submodule("memory.digest").prune_state_dir(str(hermes_home), 30)

    assert removed >= 1 and not ledger.exists()
    assert _outcome(hermes_home)["ok"] is True


def test_checkpoint_mode_off_warns_at_initialize_and_in_status(hermes_home, probe, caplog):
    _mcp_home(hermes_home, compression={"checkpoint_required": True})
    seen = []
    provider = Provider(config={"checkpoint_mode": "off"}, dispatch=_ok_dispatch()[0])

    with caplog.at_level(logging.WARNING):
        provider.initialize("session-off", platform="cli", hermes_home=str(hermes_home), warning_callback=seen.append)

    assert provider.pre_compress_checkpoint_api_version == 1
    messages = [r.getMessage() for r in caplog.records if "checkpoint_mode" in r.getMessage()]
    assert len(messages) == 1
    assert "memory.ctx_checkpoint" in messages[0]
    assert "every compaction" in messages[0]
    assert seen == messages

    # A gateway builds a provider per message: the log line once per process and home,
    # the session notice for every session that gets a warning channel.
    again = []
    with caplog.at_level(logging.WARNING):
        Provider(config={"checkpoint_mode": "off"}, dispatch=_ok_dispatch()[0]).initialize(
            "session-off-2", platform="cli", hermes_home=str(hermes_home), warning_callback=again.append
        )
    assert len([r for r in caplog.records if "checkpoint_mode" in r.getMessage()]) == 1
    assert again == messages

    _write_config(
        hermes_home,
        {
            **_read_config(hermes_home),
            "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {"checkpoint_mode": "off"}},
        },
    )
    assert "checkpoint_mode" in _status_text(Provider().get_status_config({}))


def test_checkpoint_mode_off_from_ctx_json_names_the_file(hermes_home, caplog):
    (hermes_home / "ctx.json").write_text(json.dumps({"checkpoint_mode": "off"}), encoding="utf-8")

    with caplog.at_level(logging.WARNING):
        Provider(config={}).initialize("session-json", platform="cli", hermes_home=str(hermes_home))

    messages = [r.getMessage() for r in caplog.records if "checkpoint_mode" in r.getMessage()]
    assert len(messages) == 1
    assert str(hermes_home / "ctx.json") in messages[0]


# -- E. Terminal setup: test first, then the mode ------------------------------------------


def _terminal_answers(monkeypatch, url, mode_answer):
    def answer(prompt=""):
        if prompt == "  ctx server URL: ":
            return url
        if prompt.startswith("  Wait for the checkpoint"):
            return mode_answer
        return ""

    monkeypatch.setattr("builtins.input", answer)
    monkeypatch.setattr(_MODULE.getpass, "getpass", lambda prompt="": SECRET)


def test_terminal_setup_tests_the_connection_before_writing_anything(hermes_home, probe, monkeypatch, capsys):
    probe.error = RuntimeError("Name or service not known")
    _terminal_answers(monkeypatch, "https://ctx.example.test/mcp", "")

    Provider(config={}).post_setup(str(hermes_home), {})

    out = capsys.readouterr().out
    assert "Connection test failed" in out
    assert "Name or service not known" in out
    assert "Nothing was changed" in out
    assert not (hermes_home / "config.yaml").exists()
    assert not (hermes_home / ".env").exists()


def test_terminal_setup_offers_the_required_mode_after_a_passing_test(hermes_home, probe, monkeypatch, capsys):
    _terminal_answers(monkeypatch, "https://ctx.example.test/mcp", "on")

    Provider(config={}).post_setup(str(hermes_home), {})

    out = capsys.readouterr().out
    assert "Connection test passed" in out
    assert "best effort" in out and "required" in out
    assert _read_config(hermes_home)["compression"] == {"checkpoint_required": True}
    assert probe.calls[0][1]["headers"] == {"Authorization": f"Bearer {SECRET}"}


def test_terminal_setup_keeps_the_mode_on_a_blank_answer(hermes_home, probe, monkeypatch):
    _terminal_answers(monkeypatch, "https://ctx.example.test/mcp", "")

    Provider(config={}).post_setup(str(hermes_home), {})

    config = _read_config(hermes_home)
    assert config["memory"]["provider"] == "ctx_checkpoint"
    assert "compression" not in config


# -- F. Manifest -------------------------------------------------------------------------------


def test_manifest_floor_is_the_first_hermes_whose_subagents_inherit_the_provider():
    # delegate_task children get the provider under compression.checkpoint_required from
    # v0.23.1 on (tools/delegate_tool.py: skip_memory=not child_checkpoint_required); before,
    # every sub-agent compaction failed closed.
    assert _manifest()["requires_hermes"] == ">=0.23.1"


# -- G. Skipped checkpoints, alias keys, templated URLs, concurrent outcomes, ctx.json -------


@pytest.mark.parametrize(
    "config, prepare, reason",
    [
        ({}, lambda provider: setattr(provider, "_active", False), "the provider is not connected"),
        ({"checkpoint_chain": False}, lambda provider: None, "checkpoint_chain: false"),
    ],
)
def test_a_skipped_best_effort_checkpoint_tells_the_user_why_once(hermes_home, config, prepare, reason):
    seen = []
    provider = Provider(config=config, dispatch=_ok_dispatch()[0])
    provider.initialize("session-skip", platform="cli", hermes_home=str(hermes_home), warning_callback=seen.append)
    prepare(provider)

    for _ in range(2):
        assert provider.on_pre_compress(TRANSCRIPT) == ""

    assert _outcome(hermes_home)["skipped"] == reason
    assert len(seen) == 1
    assert reason in seen[0]
    assert "without saving its wording to ctx" in seen[0]


def test_a_plugins_form_value_wins_over_an_engine_name_alias(hermes_home, linked_plugin):
    from hermes_cli.plugins_settings import save_plugin_settings

    _write_config(
        hermes_home,
        {
            "memory": {
                "ctx_checkpoint": {
                    "budget_s": 30,
                    "checkpoint_budget_s": 45,
                    "max_parts": 40,
                    "checkpoint_max_parts": 50,
                }
            }
        },
    )
    save_plugin_settings("ctx_checkpoint", linked_plugin, {"budget_s": 120, "max_parts": 70})

    provider = Provider(dispatch=_ok_dispatch()[0])
    provider.initialize("session-alias", platform="cli", hermes_home=str(hermes_home))

    assert provider._config["checkpoint_budget_s"] == 120.0
    assert provider._config["checkpoint_max_parts"] == 70


@pytest.mark.parametrize(
    "url",
    [
        "${CTX_ENDPOINT}/mcp?api_key=private-value",
        "${CTX_ENDPOINT}/mcp#api_key=private-value",
        "https://${CTX_HOST}/mcp?api_key=private-value",
    ],
)
def test_a_templated_url_is_shown_without_its_query(hermes_home, probe, url):
    shown = _submodule("memory.status").display_url(url)
    assert "private-value" not in shown and "?" not in shown and "#" not in shown
    assert shown.endswith("/mcp")

    _mcp_home(hermes_home, url=url)
    probe.error = RuntimeError("connection refused")
    failed = _status_text(Provider(config={}).get_status_config({}))
    probe.error = None
    passed = _status_text(Provider(config={}).get_status_config({}))

    assert "private-value" not in failed and "private-value" not in passed
    assert "/mcp" in passed
