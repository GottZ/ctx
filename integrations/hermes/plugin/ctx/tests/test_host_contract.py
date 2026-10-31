"""Current MemoryProvider contract: scoped secrets, context-bound threads, recall indicator,
unavailable hint, never-raising status, plugin metadata consistency."""

from __future__ import annotations

import contextvars
import json
import re
import sys
from pathlib import Path

import pytest
from conftest import PLUGIN_DIR, join_writes

from ctx_plugin import checkpoint, config
from ctx_plugin.provider import CtxMemoryProvider

# -- Scoped secrets ---------------------------------------------------------------------


def test_env_secret_prefers_host_secret_scope(monkeypatch):
    import agent.secret_scope as scope

    monkeypatch.setenv("CTX_KEY", "process-env-key")
    monkeypatch.setattr(scope, "get_secret", lambda name, default=None: {"CTX_KEY": " profile-key "}.get(name, default))
    assert config.env_secret("CTX_KEY") == "profile-key"
    assert config.env_secret("CTX_BASE_URL") == ""


def test_env_secret_falls_back_to_environ_on_older_hosts(monkeypatch):
    monkeypatch.setitem(sys.modules, "agent.secret_scope", None)  # import → ImportError
    monkeypatch.setenv("CTX_KEY", " env-key ")
    monkeypatch.setenv("CTX_BASE_URL", "https://ctx.example.com/")
    assert config.env_secret("CTX_KEY") == "env-key"
    assert config.resolve_base_url("https://ignored.example.com") == "https://ctx.example.com"


def test_provider_reads_key_and_url_through_secret_scope(make_provider, monkeypatch, tmp_path):
    import agent.secret_scope as scope

    monkeypatch.setenv("HERMES_HOME", str(tmp_path))  # no ctx.json fallback for the base URL
    monkeypatch.setattr(scope, "get_secret", lambda name, default=None: {"CTX_KEY": "scoped"}.get(name, default))
    assert CtxMemoryProvider().is_available() is False  # process env has a URL, the profile scope has none
    secrets = {"CTX_KEY": "scoped", "CTX_BASE_URL": "http://scoped.invalid/"}
    monkeypatch.setattr(scope, "get_secret", lambda name, default=None: secrets.get(name, default))
    p, _ = make_provider()
    assert p._api_key == "scoped" and p._base_url == "http://scoped.invalid"
    assert p.is_available() is True


def test_is_available_and_status_never_raise_on_unscoped_secret(monkeypatch):
    import agent.secret_scope as scope

    def unscoped(name, default=None):
        raise RuntimeError("UnscopedSecretError: no profile secret scope active")

    monkeypatch.setattr(scope, "get_secret", unscoped)
    p = CtxMemoryProvider()
    assert p.is_available() is False
    status = p.get_status_config({})
    assert status["ok"] is False and "unreadable" in status["summary"]


def test_unavailable_reason_names_the_settings():
    reason = CtxMemoryProvider().unavailable_reason()
    assert "CTX_KEY" in reason and "CTX_BASE_URL" in reason


# -- Context-bound background work ---------------------------------------------------------

PROFILE_VAR: contextvars.ContextVar[str] = contextvars.ContextVar("profile_var", default="unset")


def test_write_threads_inherit_the_callers_context(make_provider, monkeypatch):
    p, _ = make_provider()
    seen = []
    monkeypatch.setattr(p, "_auto_store", lambda *a, **k: seen.append(PROFILE_VAR.get()) or True)
    token = PROFILE_VAR.set("profile-a")
    try:
        p.on_memory_write("add", "memory", "Der Mailserver ist HOST-MX auf Port 587 mit STARTTLS fuer alle Clients.")
    finally:
        PROFILE_VAR.reset(token)
    join_writes(p)
    assert seen == ["profile-a"]


def test_idle_timer_runs_in_the_callers_context(make_provider, monkeypatch):
    p, _ = make_provider(config={"idle_digest_minutes": 0.005, "digest_every_exchanges": 0})
    seen = []
    monkeypatch.setattr(p, "_digest", lambda messages, trigger, *, hook: seen.append(PROFILE_VAR.get()))
    token = PROFILE_VAR.set("profile-b")
    try:
        p.sync_turn("u", "a", messages=[{"role": "user", "content": "x"}])
        timer = p._idle_timer
    finally:
        PROFILE_VAR.reset(token)
    timer.join(timeout=5.0)
    assert seen == ["profile-b"]


# -- Recall indicator ----------------------------------------------------------------------


def test_recall_status_reflects_only_the_last_prefetch(make_provider):
    p, c = make_provider()
    assert p.recall_status() is None
    c.query_result = {
        "success": True,
        "sources": [{"title": "A", "category": "x", "content": "a"}, {"title": "B", "category": "y", "content": "b"}],
    }
    assert p.prefetch("proxy")
    status = p.recall_status()
    assert status.count == 2 and status.provider_label == "CTX"
    c.query_result = {"success": True, "sources": []}
    assert p.prefetch("proxy") == ""
    assert p.recall_status() is None


def test_recall_status_counts_a_prewarmed_result(make_provider):
    p, c = make_provider()
    c.query_result = {"success": True, "sources": [{"title": "A", "category": "x", "content": "a"}]}
    p.queue_prefetch("dns")
    p._queue_thread.join(timeout=5.0)
    c.query_result = {"success": True, "sources": []}  # a live query now would find nothing
    assert p.prefetch("dns")
    assert p.recall_status().count == 1


def test_recall_status_counts_only_injected_hits(make_provider):
    p, c = make_provider(config={"max_prefetch_results": 1})
    c.query_result = {
        "success": True,
        "sources": [
            {"title": "C", "category": "x", "type": "checkpoint", "content": "excluded"},
            {"title": "A", "category": "x", "content": "a"},
            {"title": "B", "category": "y", "content": "b"},
        ],
    }
    p.prefetch("q")
    assert p.recall_status().count == 1


# -- Plugin metadata --------------------------------------------------------------------------


def test_plugin_yaml_version_matches_chain_metadata_version():
    text = (PLUGIN_DIR / "plugin.yaml").read_text(encoding="utf-8")
    match = re.search(r"^version:\s*([\w.]+)\s*$", text, re.M)
    assert match and match.group(1) == checkpoint.PLUGIN_VERSION
    assert re.search(r"^name:\s*ctx\s*$", text, re.M), "the directory/provider name must stay 'ctx'"


def test_plugin_dir_ships_no_tests():
    assert not list(Path(PLUGIN_DIR).glob("test_*.py"))


def test_ctx_store_success_false_envelope_carries_retryable(make_provider):
    p, c = make_provider()
    c.store = lambda *a, **k: {"success": False, "code": "rate_limit", "error": "max 100 writes"}
    out = json.loads(p.handle_tool_call("ctx_store", {"category": "learnings", "title": "t", "content": "c"}))
    assert out["code"] == "rate_limit" and out["retryable"] is True
    c.store = lambda *a, **k: {"success": False, "code": "reserved_type", "error": "derived"}
    out = json.loads(p.handle_tool_call("ctx_store", {"category": "learnings", "title": "t", "content": "c"}))
    assert out["retryable"] is False


@pytest.mark.parametrize(
    "header",
    [
        "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig1234567890",
        "authorization=Basic dXNlcjpzdXBlci1zZWNyZXQtcGFzc3dvcmQ=",
        "Authorization: Digest username-and-response-0123456789",
    ],
)
def test_plugin_redaction_masks_auth_scheme_credentials(header):
    """Defense in depth without the host redactor: the credential after the scheme must go."""
    redacted = checkpoint._redact_checkpoint_secrets(f"curl -H '{header}' https://api.example.com")
    credential = header.split()[-1]
    assert credential not in redacted and "[REDACTED]" in redacted
    assert checkpoint._redact_checkpoint_secrets("send it with Bearer abcdefgh12345678") == (
        "send it with Bearer [REDACTED]"
    )
