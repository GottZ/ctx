"""Plugin 0.14.0 (GottZ/ctx#308): status, connection test, terminal setup and the CLI redact an
error text with the same ctx keys the provider binds — one resolution, ``status.profile_ctx_keys``.

Before, ``hermes memory status`` / ``hermes ctx_checkpoint status`` and the connection test redacted
with ``CTX_API_KEY`` (or the key just typed) only, the CLI on ``transport: rest`` with ``CTX_KEY``
only. The stand-ins below repeat a key bare in an error text (artificial keys; whether ctx or Hermes
ever does that is not claimed here), next to a named value the checkpoint rules mask.

Like ``test_settings_and_status.py`` these run against the real Hermes host with an isolated
``HERMES_HOME``; the connection test is the fake ``memory.status.probe_server``, the REST client a
stand-in: no test opens a network connection.
"""

import importlib
import importlib.util
import json
import sys
import types
from pathlib import Path

import pytest

PLUGIN_DIR = Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_status_redaction", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
Provider = _MODULE.CtxCheckpointMemoryProvider
CtxApiError = _MODULE.CtxApiError

ENTRY_KEY = "e308" + "13579bdf02468ace" * 3 + "13579bdf0246"  # artificial: the server entry's own variable
SETUP_KEY = "a308" + "0123456789abcdef" * 3 + "fedcba987654"  # artificial: CTX_API_KEY
REST_KEY = "b308" + "fedcba9876543210" * 3 + "0123456789ab"  # artificial: CTX_KEY
PASSWORD = "Tr0ub4dor&3"
ENV_NAMES = ("CTX_API_KEY", "CTX_KEY", "CTX_BASE_URL", "OWN_CTX_KEY")


def _submodule(name):
    return importlib.import_module(f"{_SPEC.name}.{name}")


@pytest.fixture
def home(_isolated_hermes_home, monkeypatch):
    for name in ENV_NAMES:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setattr(_MODULE, "_hygiene_warning_emitted", False)
    return _isolated_hermes_home


@pytest.fixture
def probe(monkeypatch):
    state = types.SimpleNamespace(error=None, tools=["search", "get", "store", "query"])

    def fake(name, entry, timeout):
        if state.error is not None:
            raise state.error
        return list(state.tools)

    monkeypatch.setattr(_submodule("memory.status"), "probe_server", fake)
    return state


def _write(home, *, headers, section=None, env=""):
    config = {
        "mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp", "headers": headers}},
        "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": section or {}},
    }
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")
    (home / ".env").write_text(env, encoding="utf-8")


OWN_VARIABLE = {"Authorization": "Bearer ${OWN_CTX_KEY}"}
ENV = f"OWN_CTX_KEY={ENTRY_KEY}\nCTX_API_KEY={SETUP_KEY}\n"


def _foreign(*keys):
    """An error text that repeats the keys bare and carries a named password."""
    return "401 Unauthorized: unknown key " + " / ".join(keys) + f"; password: {PASSWORD}"


def _assert_clean(text, *keys):
    for key in (*keys, PASSWORD):
        assert key not in text, text
        assert key[:12] not in text, text


# -- one resolution ------------------------------------------------------------------------


def test_status_resolves_the_keys_the_provider_binds(home):
    _write(home, headers=OWN_VARIABLE, env=ENV)
    provider = Provider(config={}, dispatch=lambda *a, **k: json.dumps({"result": "[]"}))
    provider.initialize("root-308", platform="cli", hermes_home=str(home))

    keys = _submodule("memory.status").profile_ctx_keys("ctx")

    assert set(keys) == {ENTRY_KEY, SETUP_KEY}
    assert set(provider._bound_secrets()) == set(keys)


# -- status and connection test (MCP) ------------------------------------------------------


@pytest.mark.parametrize(
    "headers",
    [OWN_VARIABLE, {"X-Context-Key": "${env:OWN_CTX_KEY}"}, {"Authorization": f"Bearer {ENTRY_KEY}"}],
    ids=["own-variable", "env-reference", "literal"],
)
def test_memory_status_redacts_a_probe_error_with_the_entry_key(home, probe, headers):
    _write(home, headers=headers, env=ENV)
    probe.error = RuntimeError(_foreign(ENTRY_KEY, SETUP_KEY))

    status = Provider(config={}).get_status_config({})

    text = "\n".join(f"{k}: {v}" for k, v in status.items())
    assert status["ok"] is False and "401" in status["summary"]
    _assert_clean(text, ENTRY_KEY, SETUP_KEY)


def test_hermes_ctx_checkpoint_status_redacts_a_probe_error_with_the_entry_key(home, probe, capsys):
    _write(home, headers=OWN_VARIABLE, env=ENV)
    probe.error = RuntimeError(_foreign(ENTRY_KEY))
    cli = _submodule("memory.cli")

    rc = cli.cmd_status(types.SimpleNamespace(), home, cli._load(home))

    out = capsys.readouterr().out
    assert rc == 1 and "401" in out
    _assert_clean(out, ENTRY_KEY)


def test_a_status_that_cannot_be_built_names_no_key(home, monkeypatch, capsys):
    _write(home, headers=OWN_VARIABLE, env=ENV)
    cli = _submodule("memory.cli")

    def broken(hermes_home):
        raise RuntimeError(_foreign(ENTRY_KEY))

    monkeypatch.setattr(_MODULE._st, "mcp_report", broken)
    monkeypatch.setattr(cli, "mcp_report", broken)

    summary = Provider(config={}).get_status_config({})["summary"]
    assert cli.cmd_status(types.SimpleNamespace(), home, cli._load(home)) == 1

    out = capsys.readouterr().out
    assert "status unavailable" in summary and "status unavailable" in out
    _assert_clean(summary + out, ENTRY_KEY)


@pytest.mark.parametrize(
    "headers",
    [{"Authorization": f"Bearer {ENTRY_KEY}"}, OWN_VARIABLE],
    ids=["literal", "own-variable"],
)
def test_the_connection_test_redacts_the_key_of_the_entry_it_probes(home, probe, headers):
    """Whatever keys a caller passes (none here: the config changed between its two reads, or a
    caller forgot), the key of the very entry handed to the probe is masked in the result."""
    (home / ".env").write_text(ENV, encoding="utf-8")
    probe.error = RuntimeError(_foreign(ENTRY_KEY))
    entry = {"url": "https://ctx.example.test/mcp", "headers": headers}

    test = _submodule("memory.status").run_connection_test("ctx", entry, secrets=())

    assert test.ok is False and "401" in test.detail
    _assert_clean(test.detail, ENTRY_KEY)


def test_desktop_save_redacts_a_failed_connection_test_with_the_entry_key(home, probe):
    """A save that changes the URL of an entry with its own key variable runs the test; its refusal
    must not repeat that key."""
    _write(home, headers=OWN_VARIABLE, env=ENV)
    probe.error = RuntimeError(_foreign(ENTRY_KEY))

    with pytest.raises(ValueError) as refused:
        Provider(config={}).write_config_fields({"ctx_url": "https://ctx2.example.test/mcp"}, str(home))

    assert "Nothing was saved" in str(refused.value)
    _assert_clean(str(refused.value), ENTRY_KEY)


def test_terminal_setup_redacts_a_failed_connection_test_with_the_entry_key(home, probe, monkeypatch, capsys):
    _write(home, headers=OWN_VARIABLE, env=ENV)
    probe.error = RuntimeError(_foreign(ENTRY_KEY))
    monkeypatch.setattr("builtins.input", lambda prompt="": "")
    monkeypatch.setattr(_MODULE.getpass, "getpass", lambda prompt="": "")

    Provider(config={}).post_setup(str(home), {})

    out = capsys.readouterr().out
    assert "Connection test failed" in out
    _assert_clean(out, ENTRY_KEY)


def test_terminal_setup_failure_names_no_key(home, probe, monkeypatch, capsys, caplog):
    """``Setup failed: …`` prints the error of a write that failed after the test; it can repeat the
    key just typed (here the .env write) — printed and logged redacted."""
    typed = SETUP_KEY
    (home / "config.yaml").write_text(json.dumps({"mcp_servers": {}}), encoding="utf-8")
    monkeypatch.setattr("builtins.input", lambda prompt="": "https://ctx.example.test/mcp" if "URL" in prompt else "")
    monkeypatch.setattr(_MODULE.getpass, "getpass", lambda prompt="": typed)
    host_config = importlib.import_module("hermes_cli.config")

    def refuse(name, value):
        raise OSError(f".env is read-only: cannot write {name}={value} ({_foreign(ENTRY_KEY)})")

    monkeypatch.setattr(host_config, "save_env_value", refuse)
    caplog.set_level("DEBUG")

    Provider(config={}).post_setup(str(home), {})

    out = capsys.readouterr().out
    assert "Setup failed" in out and "read-only" in out
    _assert_clean(out, typed)
    _assert_clean(caplog.text, typed)


def test_terminal_setup_failure_names_no_entry_key(home, probe, monkeypatch, capsys, caplog):
    """The same for a key the server entry resolves through its own variable (no key typed: setup
    keeps that entry's auth); here the config write fails with it."""
    _write(home, headers=OWN_VARIABLE, env=ENV)
    monkeypatch.setattr("builtins.input", lambda prompt="": "")
    monkeypatch.setattr(_MODULE.getpass, "getpass", lambda prompt="": "")
    host_config = importlib.import_module("hermes_cli.config")

    def refuse(*args, **kwargs):
        raise OSError(f"config.yaml is read-only ({_foreign(ENTRY_KEY)})")

    monkeypatch.setattr(host_config, "save_config", refuse)
    caplog.set_level("DEBUG")

    Provider(config={}).post_setup(str(home), {})

    out = capsys.readouterr().out
    assert "Setup failed" in out and "read-only" in out
    _assert_clean(out, ENTRY_KEY)
    _assert_clean(caplog.text, ENTRY_KEY)


# -- the CLI and the status on transport: rest ----------------------------------------------


class _Client:
    """CtxClient stand-in: every call answers ``error`` (or raises it as CtxApiError), with its own
    key masked the way ``CtxClient`` masks it — the other keys and the password pass."""

    raise_error = False
    mask_own_key = True
    error = ""

    def __init__(self, base, key, **kwargs):
        self.key = key

    def _answer(self, *args, **kwargs):
        error = _Client.error.replace(self.key, "[REDACTED]") if _Client.mask_own_key else _Client.error
        if _Client.raise_error:
            raise CtxApiError(error)
        return {"success": False, "error": error}

    whoami = query = search = store = get_block = _answer


def _rest_home(home, monkeypatch):
    _write(home, headers=OWN_VARIABLE, section={"transport": "rest", "memory": True}, env=ENV)
    monkeypatch.setenv("CTX_KEY", REST_KEY)
    monkeypatch.setenv("CTX_BASE_URL", "https://ctx.example.test")
    cli = _submodule("memory.cli")
    monkeypatch.setattr(cli, "CtxClient", _Client)
    monkeypatch.setattr(_submodule("memory.provider"), "CtxClient", _Client)
    _Client.raise_error, _Client.mask_own_key = False, True
    _Client.error = _foreign(REST_KEY, ENTRY_KEY, SETUP_KEY)
    return cli


def _args(**values):
    base = {"text": "q", "synthesize": False, "all_types": False, "category": None, "tag": None}
    base.update(values)
    return types.SimpleNamespace(**base)


@pytest.mark.parametrize(
    ("command", "args"),
    [
        ("search", _args()),
        ("get", _args(block_id="019f6000-0000-7000-8000-000000000308")),
        ("query", _args()),
        ("store", _args(category="reference", title="t", content="c", sensitivity=None)),
        ("status", _args()),
    ],
)
def test_rest_cli_error_branches_redact_with_every_profile_key(home, monkeypatch, capsys, command, args):
    cli = _rest_home(home, monkeypatch)

    rc = cli._COMMANDS[command](args, home, cli._load(home))

    out = capsys.readouterr().out
    assert rc == 1 and "401" in out
    _assert_clean(out, REST_KEY, ENTRY_KEY, SETUP_KEY)


def test_rest_cli_sync_redacts_a_refused_entry(home, monkeypatch, capsys):
    cli = _rest_home(home, monkeypatch)
    (home / "memories").mkdir()
    (home / "memories" / "MEMORY.md").write_text("one entry worth mirroring\n", encoding="utf-8")

    rc = cli.cmd_sync(_args(), home, cli._load(home))

    out = capsys.readouterr().out
    assert rc == 1 and "401" in out
    _assert_clean(out, REST_KEY, ENTRY_KEY, SETUP_KEY)


def test_rest_cli_raised_error_is_redacted(home, monkeypatch, capsys):
    cli = _rest_home(home, monkeypatch)
    _Client.raise_error = True
    monkeypatch.setattr(cli, "_hermes_home", lambda: home)

    with pytest.raises(SystemExit):
        cli.ctx_command(types.SimpleNamespace(ctx_command="search", **vars(_args())))

    out = capsys.readouterr().out
    assert "401" in out
    _assert_clean(out, REST_KEY, ENTRY_KEY, SETUP_KEY)


@pytest.mark.parametrize("raise_error", [False, True], ids=["answer", "raised"])
def test_rest_memory_status_redacts_with_every_profile_key(home, monkeypatch, raise_error):
    _rest_home(home, monkeypatch)
    _Client.raise_error = raise_error

    status = Provider(config=None).get_status_config({})

    assert status["ok"] is False and "401" in status["summary"]
    _assert_clean(json.dumps(status, ensure_ascii=False), REST_KEY, ENTRY_KEY, SETUP_KEY)


def test_rest_cli_and_status_mask_the_rest_key_a_client_left_in(home, monkeypatch, capsys):
    """``CTX_KEY`` is bound as on the provider: a client that leaves its own key in an error text
    (an injected or foreign one; ``CtxClient`` masks it itself) does not get it printed."""
    cli = _rest_home(home, monkeypatch)
    _Client.mask_own_key = False

    rc = cli.cmd_search(_args(), home, cli._load(home))
    summary = Provider(config=None).get_status_config({})["summary"]

    out = capsys.readouterr().out
    assert rc == 1 and "401" in out and "401" in summary
    _assert_clean(out + summary, REST_KEY, ENTRY_KEY, SETUP_KEY)
