"""Setup surfaces (plugin 0.5.0): Dashboard/Desktop schema form, ``save_config`` and
``hermes memory setup`` via ``post_setup``.

These tests run against the real Hermes host on ``PYTHONPATH`` with an isolated
``HERMES_HOME`` per test, so config.yaml and .env are written by the host's own
``hermes_cli.config``.
"""

import importlib.util
import json
import logging
import sys
from pathlib import Path

import pytest
from hermes_cli.config import read_user_config_raw

_PROVIDER_PATH = Path(__file__).resolve().parents[1] / "__init__.py"
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_setup", _PROVIDER_PATH, submodule_search_locations=[str(_PROVIDER_PATH.parent)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE  # package context for the plugin's relative imports
_SPEC.loader.exec_module(_MODULE)
CtxCheckpointMemoryProvider = _MODULE.CtxCheckpointMemoryProvider
KEY_ENV = _MODULE.SETUP_API_KEY_ENV


@pytest.fixture
def hermes_home(tmp_path, monkeypatch):
    home = tmp_path / "hermes-home"
    home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(home))
    monkeypatch.delenv(KEY_ENV, raising=False)
    monkeypatch.setattr(_MODULE, "_hygiene_warning_emitted", False)
    return home


def _write_config(home, config):
    # JSON is valid YAML; the host parses it like any hand-written config.yaml.
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")


def _read_config(home):
    return read_user_config_raw(home / "config.yaml")


def _answers(monkeypatch, lines, secret=""):
    answers = iter(lines)
    monkeypatch.setattr("builtins.input", lambda prompt="": next(answers))
    monkeypatch.setattr(_MODULE.getpass, "getpass", lambda prompt="": secret)


def test_schema_offers_url_key_and_checkpoint_settings_on_a_fresh_home(hermes_home):
    schema = CtxCheckpointMemoryProvider(config={}).get_config_schema()

    fields = {field["key"]: field for field in schema}
    assert [field["key"] for field in schema] == [
        "ctx_url",
        "ctx_api_key",
        "sensitivity",
        "category",
        "chunk_chars",
        "memory",
    ]
    assert fields["ctx_url"]["required"] is True
    assert fields["ctx_url"]["default"] == ""
    assert fields["ctx_api_key"]["secret"] is True
    assert fields["ctx_api_key"]["required"] is True
    assert fields["ctx_api_key"]["env_var"] == KEY_ENV
    assert fields["sensitivity"]["choices"] == ["public", "internal", "personal", "credentials"]
    assert fields["sensitivity"]["default"] == "internal"
    assert fields["category"]["default"] == "compaction-checkpoints"
    assert fields["chunk_chars"]["default"] == 36_000
    assert (fields["chunk_chars"]["minimum"], fields["chunk_chars"]["maximum"]) == (1_000, 40_000)


@pytest.mark.parametrize(
    "entry, keys",
    [
        # Own auth (another env var, or OAuth): kept as it is, no key asked.
        (
            {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${MCP_CTX_API_KEY}"}},
            {"ctx_url"},
        ),
        ({"url": "https://ctx.example.test/mcp", "auth": "oauth"}, {"ctx_url"}),
        # Auth that setup wrote earlier: the key can be rotated.
        (
            {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${CTX_API_KEY}"}},
            {"ctx_url", "ctx_api_key"},
        ),
        # No auth yet: setup adds the header, so it needs the key.
        ({"url": "https://ctx.example.test/mcp"}, {"ctx_url", "ctx_api_key"}),
        # stdio transport: neither URL nor key.
        ({"command": "ctx", "args": ["mcp"]}, set()),
    ],
)
def test_schema_follows_the_existing_server_entry(hermes_home, entry, keys):
    _write_config(hermes_home, {"mcp_servers": {"ctx": entry}})

    fields = {f["key"]: f for f in CtxCheckpointMemoryProvider(config={}).get_config_schema()}

    assert set(fields) & {"ctx_url", "ctx_api_key"} == keys
    if "ctx_url" in fields:
        assert fields["ctx_url"]["default"] == "https://ctx.example.test/mcp"


def test_schema_follows_the_configured_server_name(hermes_home):
    _write_config(
        hermes_home,
        {
            "mcp_servers": {
                "ctx": {"url": "https://wrong.example.test/mcp"},
                "ctx-prod": {"url": "https://prod.example.test/mcp", "auth": "oauth"},
            }
        },
    )
    provider = CtxCheckpointMemoryProvider(config={"mcp_server": "ctx-prod"})

    fields = {f["key"]: f for f in provider.get_config_schema()}
    assert fields["ctx_url"]["default"] == "https://prod.example.test/mcp"
    assert "mcp_server" not in fields

    provider.save_config({"ctx_url": "https://prod.example.test/mcp"}, str(hermes_home))
    servers = _read_config(hermes_home)["mcp_servers"]
    assert servers["ctx"] == {"url": "https://wrong.example.test/mcp"}
    assert servers["ctx-prod"] == {"url": "https://prod.example.test/mcp", "auth": "oauth"}


def test_save_config_writes_provider_section_and_server_entry_only(hermes_home):
    calls = []

    def dispatch(name, args, **kwargs):
        calls.append((name, args))
        if name.endswith("__search"):
            return json.dumps({"result": "[]"})
        return json.dumps({"id": f"019f6000-0000-7000-8000-{len(calls):012d}"})

    provider = CtxCheckpointMemoryProvider(config={}, dispatch=dispatch)
    provider.save_config(
        {
            "ctx_url": "https://ctx.example.test/mcp",
            "sensitivity": "personal",
            "category": "hermes-checkpoints",
            "chunk_chars": 20_000,
            "mcp_server": "ctx",
        },
        str(hermes_home),
    )

    config = _read_config(hermes_home)
    assert config["memory"]["ctx_checkpoint"] == {
        "mcp_server": "ctx",
        "category": "hermes-checkpoints",
        "sensitivity": "personal",
        "chunk_chars": 20_000,
        "memory": False,
    }
    assert "provider" not in config["memory"]
    assert config["mcp_servers"]["ctx"] == {
        "url": "https://ctx.example.test/mcp",
        "headers": {"Authorization": "Bearer ${" + KEY_ENV + "}"},
    }
    assert "compression" not in config
    assert not (hermes_home / ".env").exists()

    provider.initialize("session-setup", platform="cli")
    provider.on_pre_compress([{"role": "user", "content": "saved settings apply"}])
    stores = [args for name, args in calls if name == "mcp__ctx__store"]
    assert {args["category"] for args in stores} == {"hermes-checkpoints"}
    assert {args["sensitivity"] for args in stores} == {"personal"}


def test_save_config_never_materializes_env_templates(hermes_home, monkeypatch):
    secret = "ctx-secret-value-0123456789"
    monkeypatch.setenv("MCP_CTX_API_KEY", secret)
    monkeypatch.setenv("OTHER_TOKEN", "other-secret-value-0123456789")
    _write_config(
        hermes_home,
        {
            "mcp_servers": {
                "ctx": {
                    "url": "https://ctx.example.test/mcp",
                    "headers": {"Authorization": "Bearer ${MCP_CTX_API_KEY}"},
                },
                "other": {"url": "https://other.example.test/mcp", "headers": {"X-Token": "${OTHER_TOKEN}"}},
            },
        },
    )

    CtxCheckpointMemoryProvider(config={}).save_config({"ctx_url": "https://ctx.example.test/mcp"}, str(hermes_home))

    text = (hermes_home / "config.yaml").read_text(encoding="utf-8")
    assert "${MCP_CTX_API_KEY}" in text
    assert "${OTHER_TOKEN}" in text
    assert secret not in text
    assert "other-secret-value" not in text


def test_save_config_keeps_an_existing_entry_and_follows_the_url(hermes_home):
    _write_config(
        hermes_home,
        {
            "mcp_servers": {
                "ctx": {
                    "url": "https://old.example.test/mcp",
                    "auth": "oauth",
                    "timeout": 45,
                    "enabled": False,
                }
            }
        },
    )

    CtxCheckpointMemoryProvider(config={}).save_config({"ctx_url": "https://new.example.test/mcp"}, str(hermes_home))

    entry = _read_config(hermes_home)["mcp_servers"]["ctx"]
    assert entry["url"] == "https://new.example.test/mcp"
    assert entry["auth"] == "oauth"
    assert entry["timeout"] == 45
    assert entry["enabled"] is True
    assert "headers" not in entry


def test_save_config_leaves_a_stdio_entry_on_its_transport(hermes_home):
    _write_config(
        hermes_home,
        {
            "mcp_servers": {
                "ctx": {
                    "command": "ctx",
                    "args": ["mcp"],
                    "enabled": False,
                }
            }
        },
    )

    CtxCheckpointMemoryProvider(config={}).save_config({}, str(hermes_home))

    assert _read_config(hermes_home)["mcp_servers"]["ctx"] == {
        "command": "ctx",
        "args": ["mcp"],
        "enabled": True,
    }


def test_save_config_merges_over_the_file_as_it_is_at_write_time(hermes_home, monkeypatch):
    _write_config(hermes_home, {"model": {"default": "before"}})
    stale = _read_config(hermes_home)
    # Another writer (the Dashboard autosave) changes config.yaml after setup read it.
    _write_config(hermes_home, {"model": {"default": "after"}, "display": {"skin": "mono"}})
    monkeypatch.setattr(_MODULE, "_read_user_config", lambda: stale)

    CtxCheckpointMemoryProvider(config={}).save_config({"ctx_url": "https://ctx.example.test/mcp"}, str(hermes_home))

    config = _read_config(hermes_home)
    assert config["model"] == {"default": "after"}
    assert config["display"] == {"skin": "mono"}
    assert config["mcp_servers"]["ctx"]["url"] == "https://ctx.example.test/mcp"


def test_save_config_refuses_a_package_managed_install(hermes_home, monkeypatch):
    import hermes_cli.config as host_config

    monkeypatch.setattr(host_config, "is_managed", lambda: True)

    with pytest.raises(RuntimeError, match="package-managed"):
        CtxCheckpointMemoryProvider(config={}).save_config(
            {"ctx_url": "https://ctx.example.test/mcp"}, str(hermes_home)
        )
    assert not (hermes_home / "config.yaml").exists()


def test_save_config_requires_a_url_when_no_entry_exists(hermes_home):
    with pytest.raises(ValueError, match="URL is required"):
        CtxCheckpointMemoryProvider(config={}).save_config({}, str(hermes_home))

    assert not (hermes_home / "config.yaml").exists()


@pytest.mark.parametrize(
    "values, message",
    [
        ({"ctx_url": "ctx.example.test/mcp"}, "must start with"),
        ({"ctx_url": "https://ctx.example.test/mcp", "sensitivity": "secret"}, "sensitivity"),
        ({"ctx_url": "https://ctx.example.test/mcp", "mcp_server": "my ctx"}, "MCP server name"),
        ({"ctx_url": "https://ctx.example.test/mcp", "chunk_chars": "lots"}, "whole number"),
    ],
)
def test_save_config_rejects_invalid_input(hermes_home, values, message):
    with pytest.raises(ValueError, match=message):
        CtxCheckpointMemoryProvider(config={}).save_config(values, str(hermes_home))


@pytest.mark.parametrize("raw, stored", [("500", 1_000), ("99999", 40_000), ("", 36_000)])
def test_save_config_clamps_part_size(hermes_home, raw, stored):
    CtxCheckpointMemoryProvider(config={}).save_config(
        {"ctx_url": "https://ctx.example.test/mcp", "chunk_chars": raw}, str(hermes_home)
    )

    assert _read_config(hermes_home)["memory"]["ctx_checkpoint"]["chunk_chars"] == stored


def test_post_setup_prompts_writes_env_and_activates(hermes_home, monkeypatch, capsys):
    # ctx_url, sensitivity (invalid, then valid), category, chunk_chars, memory features
    _answers(monkeypatch, ["https://ctx.example.test/mcp", "secret", "personal", "", "", ""], secret="ctx-key-abcdef")

    CtxCheckpointMemoryProvider(config={}).post_setup(str(hermes_home), {})

    config = _read_config(hermes_home)
    assert config["memory"]["provider"] == "ctx_checkpoint"
    assert config["memory"]["ctx_checkpoint"]["sensitivity"] == "personal"
    assert config["mcp_servers"]["ctx"]["url"] == "https://ctx.example.test/mcp"
    assert "ctx-key-abcdef" not in (hermes_home / "config.yaml").read_text(encoding="utf-8")
    assert f"{KEY_ENV}=ctx-key-abcdef" in (hermes_home / ".env").read_text(encoding="utf-8")
    out = capsys.readouterr().out
    assert "Choose one of" in out
    assert "hermes mcp test ctx" in out


def test_post_setup_requires_a_key_for_a_new_server(hermes_home, monkeypatch, capsys):
    secrets = iter(["", "ctx-key-second-try"])
    monkeypatch.setattr(
        "builtins.input",
        lambda prompt="": {
            "  ctx server URL: ": "https://ctx.example.test/mcp",
        }.get(prompt, ""),
    )
    monkeypatch.setattr(_MODULE.getpass, "getpass", lambda prompt="": next(secrets))

    CtxCheckpointMemoryProvider(config={}).post_setup(str(hermes_home), {})

    assert "A key is required." in capsys.readouterr().out
    assert f"{KEY_ENV}=ctx-key-second-try" in (hermes_home / ".env").read_text(encoding="utf-8")


def test_post_setup_keeps_a_current_key_on_blank_input(hermes_home, monkeypatch, capsys):
    (hermes_home / ".env").write_text(f"{KEY_ENV}=existing-key-1234\n", encoding="utf-8")
    prompts = []
    monkeypatch.setattr(
        "builtins.input",
        lambda prompt="": {
            "  ctx server URL: ": "https://ctx.example.test/mcp",
        }.get(prompt, ""),
    )
    monkeypatch.setattr(_MODULE.getpass, "getpass", lambda prompt="": prompts.append(prompt) or "")

    CtxCheckpointMemoryProvider(config={}).post_setup(str(hermes_home), {})

    assert prompts == ["  ctx API key (current: ...1234, blank keeps it): "]
    assert (hermes_home / ".env").read_text(encoding="utf-8") == f"{KEY_ENV}=existing-key-1234\n"
    assert _read_config(hermes_home)["memory"]["provider"] == "ctx_checkpoint"


def test_post_setup_reports_a_failed_key_write_without_touching_config(hermes_home, monkeypatch, capsys):
    import hermes_cli.config as host_config

    _answers(monkeypatch, ["https://ctx.example.test/mcp", "", "", "", ""], secret="ctx-key-abcdef")

    def refuse(key, value):
        raise PermissionError(".env is read-only")

    monkeypatch.setattr(host_config, "save_env_value", refuse)

    CtxCheckpointMemoryProvider(config={}).post_setup(str(hermes_home), {})

    out = capsys.readouterr().out
    assert "Setup failed: .env is read-only" in out
    assert "Nothing was changed." in out
    assert not (hermes_home / "config.yaml").exists()


def test_post_setup_refuses_a_package_managed_install(hermes_home, monkeypatch, capsys):
    import hermes_cli.config as host_config

    monkeypatch.setattr(host_config, "is_managed", lambda: True)
    monkeypatch.setattr("builtins.input", lambda prompt="": pytest.fail("prompted anyway"))

    CtxCheckpointMemoryProvider(config={}).post_setup(str(hermes_home), {})

    assert "package-managed" in capsys.readouterr().out
    assert not (hermes_home / "config.yaml").exists()


def test_post_setup_cancel_changes_nothing(hermes_home, monkeypatch, capsys):
    def cancel(prompt=""):
        raise KeyboardInterrupt

    monkeypatch.setattr("builtins.input", cancel)

    CtxCheckpointMemoryProvider(config={}).post_setup(str(hermes_home), {})

    assert "nothing was changed" in capsys.readouterr().out
    assert not (hermes_home / "config.yaml").exists()
    assert not (hermes_home / ".env").exists()


def test_dashboard_form_round_trip_configures_and_enables_the_provider(hermes_home):
    routes = pytest.importorskip("hermes_cli.web_routers.memory_providers")
    status = pytest.importorskip("hermes_cli.web_server_memory")
    provider = CtxCheckpointMemoryProvider(config={})
    assert status._memory_provider_is_configured("ctx_checkpoint", provider) is False
    assert provider.is_available() is False

    routes._write_memory_provider_config_values(
        "ctx_checkpoint",
        provider,
        {
            "ctx_url": "https://ctx.example.test/mcp",
            "ctx_api_key": "ctx-key-from-form",
        },
    )

    config = _read_config(hermes_home)
    assert config["mcp_servers"]["ctx"]["headers"] == {
        "Authorization": "Bearer ${" + KEY_ENV + "}",
    }
    assert config["memory"]["ctx_checkpoint"]["sensitivity"] == "internal"
    assert f"{KEY_ENV}=ctx-key-from-form" in (hermes_home / ".env").read_text(encoding="utf-8")
    assert status._memory_provider_is_configured("ctx_checkpoint", provider) is True
    assert provider.is_available() is True


@pytest.mark.parametrize(
    "servers, available",
    [
        ({}, False),
        ({"other": {"url": "https://other.example.test/mcp"}}, False),
        ({"ctx": {"url": "https://ctx.example.test/mcp", "enabled": False}}, False),
        ({"ctx": {"url": "https://ctx.example.test/mcp"}}, True),
    ],
)
def test_available_only_with_an_enabled_server_entry(hermes_home, servers, available):
    _write_config(hermes_home, {"mcp_servers": servers})

    assert CtxCheckpointMemoryProvider(config={}).is_available() is available


def test_warns_once_when_threshold_lets_hygiene_compact_first(hermes_home, caplog):
    _write_config(hermes_home, {"compression": {"threshold": 0.919}})
    provider = CtxCheckpointMemoryProvider(config={})

    with caplog.at_level(logging.WARNING):
        provider.initialize("session-a", platform="cli")
        provider.initialize("session-b", platform="cli")

    warnings = [r.getMessage() for r in caplog.records if "hygiene" in r.getMessage()]
    assert len(warnings) == 1
    assert "0.919" in warnings[0]


def test_no_warning_when_compression_is_disabled(hermes_home, caplog):
    _write_config(hermes_home, {"compression": {"enabled": False, "threshold": 0.9}})

    with caplog.at_level(logging.WARNING):
        CtxCheckpointMemoryProvider(config={}).initialize("session-a", platform="cli")

    assert not [r for r in caplog.records if "hygiene" in r.getMessage()]


def test_no_warning_on_hermes_default_threshold(hermes_home, caplog):
    with caplog.at_level(logging.WARNING):
        CtxCheckpointMemoryProvider(config={}).initialize("session-a", platform="cli")

    assert not [r for r in caplog.records if "hygiene" in r.getMessage()]
