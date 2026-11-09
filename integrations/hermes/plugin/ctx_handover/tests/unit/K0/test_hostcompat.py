"""K0: ``engine/hostcompat.py`` against the real Hermes checkout (production path) and its test double."""

from __future__ import annotations

import contextvars
import inspect
import os
import stat
import subprocess
import sys
from pathlib import Path

import pytest

from ctx_handover.engine import hostcompat
from ctx_handover.engine.errors import CapabilityMissing, EnvNotWritable, HandoverError
from ctx_handover.engine.hostcompat import HostFeatures
from ctx_handover.tests.conftest import PLUGIN_ROOT, require_upstream
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx, FakeHost, public_functions


@pytest.fixture(autouse=True)
def _upstream():
    require_upstream()


def test_hostcompat_import_is_lazy(tmp_path):
    """Importing the engine (hostcompat included) loads no Hermes module; ``register()`` stays cheap (R12)."""
    code = (
        "import sys\n"
        "import ctx_handover\n"
        "import ctx_handover.engine.hostcompat, ctx_handover.engine.policy\n"
        "ctx_handover.register(object())\n"
        "names = {'hermes_state','hermes_constants','hermes_cli','agent','gateway','plugins','providers','tools'}\n"
        "print(sorted(m for m in sys.modules if m.split('.')[0] in names))\n"
    )
    env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    env["PYTHONPATH"] = os.pathsep.join([str(PLUGIN_ROOT.parent), env.get("PYTHONPATH", "")])
    out = subprocess.run(  # noqa: S603
        [sys.executable, "-c", code], cwd=tmp_path, env=env, capture_output=True, text=True, check=True
    )
    assert out.stdout.strip() == "[]"


def test_features_against_real_hermes(monkeypatch):
    feats = hostcompat.features()
    assert feats.session_db_ok and feats.plugin_llm and feats.missing == ()
    assert not (feats.side_question or feats.ask_choice or feats.post_message or feats.host_isolated)
    ctx = FakeCtx()
    feats = hostcompat.features(ctx)
    assert feats.side_question and feats.ask_choice and feats.post_message
    monkeypatch.setenv(hostcompat.HOST_PROCESS_ENV, "1")
    feats = hostcompat.features(ctx)
    assert feats.host_isolated and feats.ask_choice and not feats.post_message


def test_required_sessiondb_methods_exist_on_the_floor_tree():
    from hermes_state import SessionDB

    missing = [m for m in hostcompat.REQUIRED_SESSIONDB_METHODS if not callable(getattr(SessionDB, m, None))]
    assert missing == []


def test_session_db_paths_and_plugin_dir(hermes_home):
    db = hostcompat.session_db()
    try:
        assert hostcompat.state_db_path() == hermes_home / "state.db"
        assert (hermes_home / "state.db").is_file()
    finally:
        db.close()
    paths = hostcompat.hermes_paths()
    assert Path(paths.home) == hermes_home
    assert Path(paths.document_cache) == hermes_home / "cache" / "documents"
    data = hostcompat.plugin_dir()
    assert data == hermes_home / "plugin-data" / "ctx_handover"
    assert stat.S_IMODE(os.stat(data).st_mode) == 0o700
    assert hostcompat.hermes_root() == Path(__import__("hermes_constants").get_default_hermes_root())


def test_host_facts_from_the_profile_config(hermes_home):
    (hermes_home / "config.yaml").write_text(
        "model:\n  default: m-1\n  provider: openrouter\n  context_length: 64000\nterminal:\n  backend: local\n",
        encoding="utf-8",
    )
    facts = hostcompat.host_facts()
    assert (facts.model, facts.provider, facts.context_length) == ("m-1", "openrouter", 64000)
    assert facts.terminal_backend == "local" and facts.profile == "default" and facts.multiplex is False
    assert facts.hermes_home == str(hermes_home) and facts.plugin_version == "0.1.0"
    assert isinstance(facts.hermes_version, str)


def test_read_secret_goes_through_get_secret(monkeypatch):
    """BUILD-ORDER §6 #22 (K0 part): gate secrets are read with ``agent.secret_scope.get_secret`` (rule 11)."""
    import agent.secret_scope as scope

    seen = []

    def spy(name, default=None):
        seen.append(name)
        return "from-scope"

    monkeypatch.setattr(scope, "get_secret", spy)
    assert hostcompat.read_secret("CTX_HANDOVER_PEER_TOKEN") == "from-scope"
    assert seen == ["CTX_HANDOVER_PEER_TOKEN"]
    monkeypatch.undo()
    monkeypatch.setenv("CTX_HANDOVER_PEER_TOKEN", "tok-1")
    assert hostcompat.read_secret("CTX_HANDOVER_PEER_TOKEN") == "tok-1"
    monkeypatch.delenv("CTX_HANDOVER_PEER_TOKEN")
    assert hostcompat.read_secret("CTX_HANDOVER_PEER_TOKEN") is None


def test_read_secret_wraps_scope_errors(monkeypatch):
    import agent.secret_scope as scope

    def boom(name, default=None):
        raise RuntimeError("no scope")

    monkeypatch.setattr(scope, "get_secret", boom)
    with pytest.raises(HandoverError) as info:
        hostcompat.read_secret("X_TOKEN")
    assert info.value.details["secret"] == "X_TOKEN"


def test_identity_env_write_and_fresh_read_back(hermes_home, monkeypatch):
    """R16 path through the core: ``env_writable`` → ``save_env`` → ``read_env_fresh`` (profile ``.env`` wins over
    a stale process environment)."""
    name = "CTX_HANDOVER_TEST_IDENTITY"
    monkeypatch.setenv(name, "stale-from-shell")
    hostcompat.env_writable(name)
    hostcompat.save_env(name, "fresh-value")
    assert f"{name}=fresh-value" in (hermes_home / ".env").read_text(encoding="utf-8")
    assert hostcompat.read_env_fresh(name) == "fresh-value"
    assert hostcompat.read_env_fresh("CTX_HANDOVER_NEVER_SET") is None


def test_env_writable_maps_refusal(monkeypatch):
    import hermes_cli.config as config

    def refuse(key, action):
        raise ValueError(f"{key} is managed")

    monkeypatch.setattr(config, "require_env_writable", refuse)
    with pytest.raises(EnvNotWritable) as info:
        hostcompat.env_writable("CTX_HANDOVER_X")
    assert info.value.code == "env_not_writable" and info.value.details["variable"] == "CTX_HANDOVER_X"


def test_redact_uses_the_core_redactor():
    token = "sk-" + "A1b2C3d4" * 6
    assert hostcompat.redact("hello world") == "hello world"
    assert token not in hostcompat.redact(f"OPENAI_API_KEY={token}")


@pytest.mark.parametrize("mode", [False, True, "off", "OFF ", "manual", "smart", "auto", None, 0])
def test_approvals_off_mode_matches_the_core(mode):
    from tools.approval_context import _normalize_approval_mode

    assert hostcompat.approvals_off({"approvals": {"mode": mode}}) is (_normalize_approval_mode(mode) == "off")


@pytest.mark.parametrize(
    ("value", "off"),
    [("approve", True), ("yes", True), ("allow", True), ("off", True), ("deny", False), (False, False)],
)
def test_approvals_off_single_query_mode(value, off):
    assert hostcompat.approvals_off({"approvals": {"single_query_mode": value}}) is off
    assert hostcompat.approvals_off({}) is False


def test_provider_usable_uses_target_configuration_only(monkeypatch):
    config = {"model": {"default": "m", "provider": "nous"}, "providers": {"my-local": {"base_url": "http://x"}}}
    assert hostcompat.provider_usable("my-local", config)
    assert not hostcompat.provider_usable("", config)
    assert not hostcompat.provider_usable("no-such-provider-xyz", config)
    monkeypatch.delenv("OPENROUTER_API_KEY", raising=False)
    assert not hostcompat.provider_usable("openrouter", config)
    monkeypatch.setenv("OPENROUTER_API_KEY", "or-key")
    assert hostcompat.provider_usable("openrouter", config)
    assert hostcompat.provider_usable("nous", config)
    assert not hostcompat.provider_usable("nous", {"model": {"provider": "openrouter"}})
    monkeypatch.setenv("GMI_BASE_URL", "http://gmi")
    monkeypatch.delenv("GMI_API_KEY", raising=False)
    assert not hostcompat.provider_usable("gmi", config)


def test_plugin_llm_adapter(monkeypatch):
    import agent.plugin_llm as plugin_llm

    calls = []

    class Result:
        def __init__(self, parsed):
            self.parsed, self.text = parsed, "t"

    answers = [Result({"add": []}), Result(None), Result(["not", "a", "dict"])]

    def fake_complete(self, **kw):
        calls.append((self._plugin_id, kw))
        return answers.pop(0)

    monkeypatch.setattr(plugin_llm.PluginLlm, "complete_structured", fake_complete)
    llm = hostcompat.plugin_llm()
    assert llm is not None
    assert llm("do it", [{"type": "text", "text": "x"}], {"type": "object"}, 45) == {"add": []}
    assert llm("i", [], {}, 1) is None and llm("i", [], {}, 1) is None
    plugin_id, kw = calls[0]
    assert plugin_id == "ctx_handover"
    assert kw["instructions"] == "do it" and kw["json_schema"] == {"type": "object"}
    assert kw["timeout"] == 45.0 and kw["purpose"] == "ctx_handover.refine"


def test_missing_hermes_degrades_to_capability_missing(monkeypatch):
    monkeypatch.setitem(sys.modules, "hermes_state", None)
    monkeypatch.setitem(sys.modules, "agent.plugin_llm", None)
    with pytest.raises(CapabilityMissing) as info:
        hostcompat.session_db()
    assert info.value.code == "capability_missing" and info.value.details["capability"] == "hermes_state.SessionDB"
    assert hostcompat.plugin_llm() is None
    hostcompat._static_features.cache_clear()
    try:
        feats = hostcompat.features()
        assert not feats.session_db_ok and not feats.plugin_llm
        assert set(feats.missing) == {"hermes_state.SessionDB", "agent.plugin_llm.PluginLlm"}
    finally:
        hostcompat._static_features.cache_clear()


def test_run_in_context_copies_the_context():
    var: contextvars.ContextVar[str] = contextvars.ContextVar("k0_var", default="outer")

    def inner():
        var.set("inner")
        return var.get()

    assert hostcompat.run_in_context(inner) == "inner"
    assert var.get() == "outer"


# --------------------------------------------------------------------------------------------------------
# The test double
# --------------------------------------------------------------------------------------------------------


def test_hostcompat_fake_parity():
    """``FakeHost`` mirrors every public hostcompat function with the same parameter names."""
    names = public_functions()
    assert set(names) >= {
        "features",
        "session_db",
        "state_db_path",
        "hermes_paths",
        "host_facts",
        "plugin_dir",
        "load_config",
        "read_secret",
        "env_writable",
        "save_env",
        "provider_usable",
        "redact",
        "plugin_llm",
        "run_in_context",
        "hermes_root",
        "read_env_fresh",
        "approvals_off",
    }
    for name in names:
        real = list(inspect.signature(getattr(hostcompat, name)).parameters)
        fake = list(inspect.signature(getattr(FakeHost, name)).parameters)[1:]
        assert fake == real, name


def test_fake_host_installs_over_the_module(fake_host, hermes_home):
    assert isinstance(fake_host, FakeHost)
    fake_host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "t"
    assert hostcompat.read_secret("CTX_HANDOVER_PEER_TOKEN") == "t"
    fake_host.silently_refused.add("CTX_HANDOVER_IDENTITY_KEY")
    hostcompat.save_env("CTX_HANDOVER_IDENTITY_KEY", "k")
    assert hostcompat.read_env_fresh("CTX_HANDOVER_IDENTITY_KEY") is None
    fake_host.env_locked.add("LOCKED")
    with pytest.raises(EnvNotWritable):
        hostcompat.env_writable("LOCKED")
    assert isinstance(hostcompat.features(), HostFeatures)
    assert hostcompat.hermes_paths().home == str(hermes_home)
    assert hostcompat.approvals_off({"approvals": {"mode": "off"}})
    assert ("read_secret", ("CTX_HANDOVER_PEER_TOKEN",)) in fake_host.calls
    db = hostcompat.session_db()
    db.close()


def test_fake_ctx_records(fake_ctx):
    handle = fake_ctx.post_message("one")
    handle.edit("two")
    fake_ctx.choice_answers.append("yes")
    assert fake_ctx.ask_choice("go?", ["yes", "no"]) == "yes"
    assert fake_ctx.ask_choice("again?", ["yes"]) is None
    assert fake_ctx.messages == ["two"] and fake_ctx.edits == [(0, "two")]
    assert fake_ctx.session_surface() == "local"
