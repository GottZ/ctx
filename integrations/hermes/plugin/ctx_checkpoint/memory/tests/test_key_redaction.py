"""Key echo redaction + client state on re-init (ctx_checkpoint 0.6.2).

A CTX server (or a proxy in front of it) that repeats the request's API key in an error text
must never get that key into ``CtxApiError`` texts/codes, returned failure envelopes, logs,
``exc_info`` tracebacks, ``get_status_config`` summaries or tool results. Redaction lives in
``CtxClient`` so every caller is covered; it is applied before truncation.
Re-initializing a provider instance must not keep the previous client, key, whoami identity
or checkpoint writer — not even when the new run ends inactive.

Network-free: ``urllib.request.urlopen`` is mocked; every key is synthetic.
"""

from __future__ import annotations

import importlib.util
import io
import json
import logging
import sys
import urllib.error
import urllib.request
from pathlib import Path
from types import SimpleNamespace

import pytest

KEY_A = "ctxkey-REDACT-ALPHA-5e8c13a7d2"
KEY_B = "ctxkey-REDACT-BETA-91f0b4c6e3"
URL_A = "http://alpha.ctx.invalid"
URL_B = "http://beta.ctx.invalid"
REDACTED = "[REDACTED]"


def _assert_no_key(text, key=KEY_A):
    assert key not in text
    # no partial prefix either (truncation must happen after redaction)
    assert key[:12] not in text


def _all_log_text(caplog):
    parts = [caplog.text]
    for rec in caplog.records:
        parts.append(rec.getMessage())
        parts.extend(str(a) for a in (rec.args or ()))
    return "\n".join(parts)


def _raise(exc):
    def _urlopen(*_a, **_k):
        raise exc

    return _urlopen


def _http_error(status, body, reason="Unauthorized"):
    raw = body if isinstance(body, bytes) else json.dumps(body).encode("utf-8")
    return urllib.error.HTTPError(URL_A + "/api/x", status, reason, {}, io.BytesIO(raw))


class _Resp:
    def __init__(self, obj):
        self._raw = json.dumps(obj).encode("utf-8")

    def read(self, *_a):
        return self._raw

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _respond(obj):
    return lambda *_a, **_k: _Resp(obj)


@pytest.fixture
def ctx_env(monkeypatch):
    monkeypatch.setenv("CTX_KEY", KEY_A)
    monkeypatch.setenv("CTX_BASE_URL", URL_A)
    return monkeypatch


def _init(tmp_path, session="sess-redact", profile="alpha"):
    from ctx_plugin.provider import CtxMemoryProvider

    home = tmp_path / "profiles" / profile
    home.mkdir(parents=True, exist_ok=True)
    p = CtxMemoryProvider()
    p.initialize(session, hermes_home=str(home), agent_context="primary")
    return p


# -- helpers ---------------------------------------------------------------------------------


def test_redact_secret_and_redact_deep():
    from ctx_plugin.client import REDACTED as CLIENT_REDACTED
    from ctx_plugin.client import redact_deep, redact_secret

    assert CLIENT_REDACTED == REDACTED
    assert redact_secret(f"a {KEY_A} b {KEY_A}", KEY_A) == f"a {REDACTED} b {REDACTED}"
    assert redact_secret(None, KEY_A) == ""
    assert redact_secret("no key here", "") == "no key here"
    assert redact_secret(ValueError(f"x {KEY_A}"), KEY_A) == f"x {REDACTED}"
    value = {KEY_A: [f"k={KEY_A}", 3, None, {"n": f"{KEY_A}!"}], "t": True}
    assert redact_deep(value, KEY_A) == {KEY_A: [f"k={REDACTED}", 3, None, {"n": f"{REDACTED}!"}], "t": True}
    assert redact_deep(value, "") is value


# -- HTTPError: every text that leaves the client --------------------------------------------


@pytest.mark.parametrize(
    "body",
    [
        pytest.param({"error": f"invalid provided key {KEY_A}"}, id="json-error"),
        pytest.param({"message": f"invalid provided key {KEY_A}"}, id="json-message"),
        pytest.param({"error": {"reason": f"invalid key {KEY_A}", "hints": [f"rotate {KEY_A}"]}}, id="json-nested"),
        pytest.param(f"upstream rejected {KEY_A}".encode(), id="plain-text-body"),
        pytest.param(("x" * 288 + KEY_A).encode(), id="plain-text-key-at-300-cut"),
        pytest.param({"error": "x" * 288 + KEY_A}, id="json-error-key-at-300-cut"),
    ],
)
def test_http_error_body_echo_is_redacted(monkeypatch, body):
    from ctx_plugin.client import CtxApiError, CtxClient

    monkeypatch.setattr(urllib.request, "urlopen", _raise(_http_error(401, body)))
    with pytest.raises(CtxApiError) as info:
        CtxClient(URL_A, KEY_A).whoami()
    _assert_no_key(str(info.value))
    assert info.value.status == 401
    assert REDACTED in str(info.value)  # 288 + len("[REDACTED]") still fits the 300-char cut


def test_http_error_reason_echo_is_redacted(monkeypatch):
    from ctx_plugin.client import CtxApiError, CtxClient

    monkeypatch.setattr(urllib.request, "urlopen", _raise(_http_error(401, b"", reason=f"Unauthorized {KEY_A}")))
    with pytest.raises(CtxApiError) as info:
        CtxClient(URL_A, KEY_A).search("x")
    assert str(info.value) == f"HTTP 401: Unauthorized {REDACTED}"


def test_unreadable_error_body_debug_log_does_not_chain_key(monkeypatch, caplog):
    from ctx_plugin.client import CtxApiError, CtxClient

    class _BrokenBody(io.BytesIO):
        def read(self, *_a):
            raise OSError("connection reset while reading body")

    exc = urllib.error.HTTPError(URL_A + "/api/x", 401, f"Unauthorized {KEY_A}", {}, _BrokenBody())
    monkeypatch.setattr(urllib.request, "urlopen", _raise(exc))
    caplog.set_level(logging.DEBUG)
    with pytest.raises(CtxApiError) as info:
        CtxClient(URL_A, KEY_A).search("x")
    assert "CTX error body unreadable (OSError)" in caplog.text
    _assert_no_key(str(info.value))
    _assert_no_key(_all_log_text(caplog))


def test_http_error_unknown_code_echo_is_redacted(monkeypatch):
    from ctx_plugin.client import CtxApiError, CtxClient

    monkeypatch.setattr(urllib.request, "urlopen", _raise(_http_error(422, {"error": "no", "code": f"bad-{KEY_A}"})))
    with pytest.raises(CtxApiError) as info:
        CtxClient(URL_A, KEY_A).store("c", "t", "x")
    assert info.value.code == f"bad-{REDACTED}"
    _assert_no_key(str(info.value))


@pytest.mark.parametrize("code", ["unauthorized", "scope_denied", "unknown_type", "rate_limit"])
def test_http_error_reject_codes_stay_verbatim(monkeypatch, code):
    from ctx_plugin.client import REJECT_CODES, CtxApiError, CtxClient

    assert code in REJECT_CODES
    status = 429 if code == "rate_limit" else 403
    body = {"error": f"k {KEY_A}", "code": code}

    def _urlopen(*_a, **_k):  # fresh error per call: a 429 is retried once and reads the body again
        raise _http_error(status, body)

    monkeypatch.setattr(urllib.request, "urlopen", _urlopen)
    monkeypatch.setattr("ctx_plugin.client.time.sleep", lambda _s: None)
    with pytest.raises(CtxApiError) as info:
        CtxClient(URL_A, KEY_A).store("c", "t", "x")
    assert info.value.code == code
    assert str(info.value) == f"HTTP {status} [{code}]: k {REDACTED}"


def test_texts_without_key_are_untouched(monkeypatch):
    from ctx_plugin.client import CtxApiError, CtxClient

    monkeypatch.setattr(urllib.request, "urlopen", _raise(_http_error(400, {"error": "Missing required field: id"})))
    with pytest.raises(CtxApiError) as info:
        CtxClient(URL_A, KEY_A).whoami()
    assert str(info.value) == "HTTP 400: Missing required field: id"


# -- connection errors -----------------------------------------------------------------------


@pytest.mark.parametrize(
    "exc",
    [
        pytest.param(urllib.error.URLError(f"proxy refused key {KEY_A}"), id="URLError"),
        pytest.param(OSError(f"socket said {KEY_A}"), id="OSError"),
        pytest.param(TimeoutError(f"timed out for {KEY_A}"), id="TimeoutError"),
    ],
)
def test_connection_error_echo_is_redacted(monkeypatch, exc):
    from ctx_plugin.client import CtxApiError, CtxClient

    monkeypatch.setattr(urllib.request, "urlopen", _raise(exc))
    with pytest.raises(CtxApiError) as info:
        CtxClient(URL_A, KEY_A).query("x")
    assert str(info.value).startswith("connection error:")
    assert REDACTED in str(info.value)
    _assert_no_key(str(info.value))


@pytest.mark.parametrize(
    "exc",
    [
        pytest.param(urllib.error.URLError(f"proxy refused key {KEY_A}"), id="URLError"),
        pytest.param(_http_error(401, {"error": f"key {KEY_A}"}, reason=f"Unauthorized {KEY_A}"), id="HTTPError"),
    ],
)
def test_original_exception_is_not_chained(monkeypatch, exc):
    """``raise … from None``: the unredacted original must not ride along as cause/context."""
    from ctx_plugin.client import CtxApiError, CtxClient

    monkeypatch.setattr(urllib.request, "urlopen", _raise(exc))
    with pytest.raises(CtxApiError) as info:
        CtxClient(URL_A, KEY_A).whoami()
    assert info.value.__cause__ is None
    assert info.value.__suppress_context__ is True


# -- HTTP 200 failure envelopes --------------------------------------------------------------


@pytest.mark.parametrize(
    ("payload", "expect"),
    [
        pytest.param(
            {"success": False, "error": f"key {KEY_A} revoked"},
            {"success": False, "error": f"key {REDACTED} revoked"},
            id="error",
        ),
        pytest.param(
            {"success": False, "error": "no", "code": f"invalid-{KEY_A}"},
            {"success": False, "error": "no", "code": f"invalid-{REDACTED}"},
            id="code",
        ),
        pytest.param(
            {"success": False, "error": {"reason": f"key {KEY_A}", "hints": [f"rotate {KEY_A}", 3]}},
            {"success": False, "error": {"reason": f"key {REDACTED}", "hints": [f"rotate {REDACTED}", 3]}},
            id="nested",
        ),
        pytest.param(
            {"success": False, "message": f"key {KEY_A}"},
            {"success": False, "message": f"key {REDACTED}"},
            id="message",
        ),
        pytest.param(
            {"error": f"key {KEY_A}", "code": "scope_denied"},
            {"error": f"key {REDACTED}", "code": "scope_denied"},
            id="error-without-success-reject-code-kept",
        ),
    ],
)
def test_200_failure_envelope_is_redacted(monkeypatch, payload, expect):
    from ctx_plugin.client import CtxClient

    monkeypatch.setattr(urllib.request, "urlopen", _respond(payload))
    data = CtxClient(URL_A, KEY_A).store("c", "t", "x")
    assert data == expect
    _assert_no_key(json.dumps(data))


@pytest.mark.parametrize(
    "payload",
    [
        pytest.param({"success": True, "results": [{"id": "e1", "content": "eee", "score": 0.5}]}, id="search"),
        pytest.param({"action": "noop", "existing_id": "e1", "reason": "identical content"}, id="store-noop"),
        pytest.param({"block": {"id": "e2", "created_at": "t", "updated_at": "t"}}, id="store-created"),
    ],
)
def test_200_success_payload_is_not_rewritten(monkeypatch, payload):
    """Successful answers are never rewritten — not even with a 1-character key."""
    from ctx_plugin.client import CtxClient

    monkeypatch.setattr(urllib.request, "urlopen", _respond(payload))
    assert CtxClient(URL_A, "e").search("q") == payload


# -- callers: provider, status, tools, logs --------------------------------------------------


def test_whoami_http_error_echo_not_logged(tmp_path, ctx_env, caplog):
    """Real CtxClient, whoami 401 echoing the key → provider inactive, nothing in the log."""
    caplog.set_level(logging.DEBUG)
    ctx_env.setattr(urllib.request, "urlopen", _raise(_http_error(401, {"error": f"invalid provided key {KEY_A}"})))
    p = _init(tmp_path)
    assert not p._active and p._client is None
    log = _all_log_text(caplog)
    _assert_no_key(log)
    assert "CTX whoami validation failed" in log and REDACTED in log


def test_whoami_echo_of_injected_client_not_logged(tmp_path, ctx_env, fake_client_cls, caplog):
    """A foreign (non-CtxClient) client raising with the key in its text is redacted too."""
    from ctx_plugin.client import CtxApiError

    def echo_whoami(self):
        raise CtxApiError(f"HTTP 401: invalid provided key {KEY_A}", status=401)

    ctx_env.setattr(fake_client_cls, "whoami", echo_whoami)
    caplog.set_level(logging.DEBUG)
    p = _init(tmp_path)
    assert not p._active and p._client is None
    log = _all_log_text(caplog)
    _assert_no_key(log)
    assert REDACTED in log


@pytest.mark.parametrize("mode", ["http_error", "error_field"])
def test_status_summary_does_not_echo_key(ctx_env, mode):
    from ctx_plugin.provider import CtxMemoryProvider

    if mode == "http_error":
        ctx_env.setattr(urllib.request, "urlopen", _raise(_http_error(401, {"error": f"invalid key {KEY_A}"})))
    else:
        ctx_env.setattr(urllib.request, "urlopen", _respond({"success": False, "error": f"key {KEY_A} revoked"}))
    status = CtxMemoryProvider().get_status_config({})
    assert status["ok"] is False
    _assert_no_key(json.dumps(status))
    assert REDACTED in status["summary"]


def test_tool_errors_and_logs_do_not_echo_key(monkeypatch, caplog):
    from ctx_plugin.client import CtxClient
    from ctx_plugin.tools import handle_tool_call

    caplog.set_level(logging.DEBUG)
    provider = SimpleNamespace(_active=True, _client=CtxClient(URL_A, KEY_A), _config={}, _profile="alpha")
    store_args = {"category": "c", "title": "t", "content": "x"}
    outputs = []
    for status, body in ((403, {"error": f"denied for {KEY_A}", "code": "scope_denied"}), (500, {"error": KEY_A})):
        monkeypatch.setattr(urllib.request, "urlopen", _raise(_http_error(status, body)))
        outputs.append(handle_tool_call(provider, "ctx_search", {"query": "x"}))
    for tool, args, payload in (
        ("ctx_store", store_args, {"success": False, "error": f"key {KEY_A}", "code": f"invalid-{KEY_A}"}),
        ("ctx_search", {"query": "q"}, {"success": False, "error": {"reason": f"key {KEY_A}"}}),
        ("ctx_query", {"text": "q"}, {"success": False, "error": f"key {KEY_A} revoked"}),
    ):
        monkeypatch.setattr(urllib.request, "urlopen", _respond(payload))
        outputs.append(handle_tool_call(provider, tool, args))
    for out in outputs:
        assert "error" in json.loads(out)
        _assert_no_key(out)
    _assert_no_key(_all_log_text(caplog))


def test_exc_info_traceback_does_not_carry_key(monkeypatch, caplog):
    """tools.py logs failures with exc_info=True — the chained original exception (URLError /
    HTTPError reason) must not smuggle the unredacted key into the rendered traceback."""
    from ctx_plugin.client import CtxClient
    from ctx_plugin.tools import handle_tool_call

    caplog.set_level(logging.DEBUG)
    provider = SimpleNamespace(_active=True, _client=CtxClient(URL_A, KEY_A), _config={}, _profile="alpha")
    monkeypatch.setattr(urllib.request, "urlopen", _raise(urllib.error.URLError(f"proxy refused key {KEY_A}")))
    out = handle_tool_call(provider, "ctx_search", {"query": "x"})
    monkeypatch.setattr(urllib.request, "urlopen", _raise(_http_error(401, b"", reason=f"Unauthorized {KEY_A}")))
    out += handle_tool_call(provider, "ctx_search", {"query": "x"})
    _assert_no_key(out)
    assert "Traceback" in caplog.text  # exc_info was rendered
    _assert_no_key(_all_log_text(caplog))


# -- re-initialization of the same instance (REST) -------------------------------------------


def test_reinitialize_without_key_drops_client_identity_and_chain_writer(tmp_path, ctx_env, fake_client_cls):
    p = _init(tmp_path)
    writer = p._chain()
    assert p._active and writer.client is p._client and p._home_scope and p._whoami_label
    ctx_env.delenv("CTX_KEY")
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "alpha"), agent_context="primary")
    assert not p._active
    assert p._client is None and p._chain_writer is None
    assert p._api_key == ""
    assert p._home_scope == "" and p._whoami_label == ""


def test_reinitialize_without_url_drops_base_url(tmp_path, ctx_env, fake_client_cls):
    p = _init(tmp_path)
    ctx_env.delenv("CTX_BASE_URL")
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "beta"), agent_context="primary")
    assert not p._active and p._client is None
    assert p._base_url == ""


def test_reinitialize_after_whoami_failure_clears_identity(tmp_path, ctx_env, fake_client_cls):
    from ctx_plugin.client import CtxApiError

    p = _init(tmp_path)
    p._chain()
    assert p._home_scope == "shared" and p._whoami_label == "test-key"

    def fail(self):
        raise CtxApiError("connection error: simulated")

    ctx_env.setattr(fake_client_cls, "whoami", fail)
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "alpha"), agent_context="primary")
    assert not p._active and p._client is None and p._chain_writer is None
    assert p._home_scope == "" and p._whoami_label == ""


def test_reinitialize_with_other_key_replaces_client(tmp_path, ctx_env, fake_client_cls):
    p = _init(tmp_path)
    old = p._client
    ctx_env.setenv("CTX_KEY", KEY_B)
    ctx_env.setenv("CTX_BASE_URL", URL_B)
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "beta"), agent_context="primary")
    assert p._active and p._client is not old
    assert (p._api_key, p._base_url) == (KEY_B, URL_B)
    assert p._chain().client is p._client


# -- re-initialization: caches bound to the previous binding ---------------------------------

MANIFEST_ID = "019a0000-0000-7000-8000-000000000001"


def test_reinitialize_drops_prefetch_cache(tmp_path, ctx_env, fake_client_cls):
    p = _init(tmp_path)
    p._prefetch_cache = ("same query", "<recall of alpha>", 1, p._now())
    ctx_env.setenv("CTX_KEY", KEY_B)
    ctx_env.setenv("CTX_BASE_URL", URL_B)
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "beta"), agent_context="primary")
    assert p._prefetch_cache is None


def test_prefetch_warm_up_publishes_without_reinitialize(tmp_path, ctx_env, fake_client_cls):
    p = _init(tmp_path)
    ctx_env.setattr(p, "_recall", lambda *_a: ("<recall of alpha>", 1))
    p.queue_prefetch("same query")
    p._queue_thread.join(5.0)
    assert p._prefetch_cache[:3] == ("same query", "<recall of alpha>", 1)


def test_inflight_prefetch_does_not_publish_after_reinitialize(tmp_path, ctx_env, fake_client_cls):
    import threading

    p = _init(tmp_path)
    started, release = threading.Event(), threading.Event()

    def slow_recall(*_a):
        started.set()
        assert release.wait(5.0)
        return "<recall of alpha>", 1

    ctx_env.setattr(p, "_recall", slow_recall)
    p.queue_prefetch("same query")
    assert started.wait(5.0)
    warm = p._queue_thread
    ctx_env.setenv("CTX_KEY", KEY_B)
    ctx_env.setenv("CTX_BASE_URL", URL_B)
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "beta"), agent_context="primary")
    release.set()
    warm.join(5.0)
    assert not warm.is_alive()
    assert p._prefetch_cache is None


def test_prefetch_queued_during_reinitialize_does_not_publish(tmp_path, ctx_env, fake_client_cls):
    """A warm-up queued between the start of initialize() and _connect() still sees the
    previous client; its result must not survive the re-init."""
    p = _init(tmp_path)
    ctx_env.setattr(p, "_recall", lambda *_a: ("<recall of alpha>", 1))
    warm = []
    prune = p._prune_state_once

    def queue_mid_init():
        p.queue_prefetch("same query")
        warm.append(p._queue_thread)
        warm[-1].join(5.0)
        prune()

    ctx_env.setattr(p, "_prune_state_once", queue_mid_init)
    ctx_env.setenv("CTX_KEY", KEY_B)
    ctx_env.setenv("CTX_BASE_URL", URL_B)
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "beta"), agent_context="primary")
    assert warm and not warm[0].is_alive()
    assert p._prefetch_cache is None


def test_warm_up_queries_the_client_it_was_queued_with(tmp_path, ctx_env, fake_client_cls):
    """A warm-up that only starts running after a re-init must still go to the binding it
    was queued under, never to the next binding's server."""
    import ctx_plugin.provider as provider_mod

    p = _init(tmp_path)
    old = p._client
    deferred = []
    real_spawn = provider_mod.spawn_context_thread

    def spawn_later(target, **kwargs):
        deferred.append(real_spawn(target, **kwargs))
        return SimpleNamespace(start=lambda: None, is_alive=lambda: False, join=lambda *_a, **_k: None)

    ctx_env.setattr(provider_mod, "spawn_context_thread", spawn_later)
    p.queue_prefetch("alpha question")
    ctx_env.setenv("CTX_KEY", KEY_B)
    ctx_env.setenv("CTX_BASE_URL", URL_B)
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "beta"), agent_context="primary")
    new = p._client
    assert new is not old
    deferred[0].start()
    deferred[0].join(5.0)
    assert [r[1] for r in old.reads] == ["alpha question"]
    assert all(r[1] != "alpha question" for r in new.reads)
    assert p._prefetch_cache is None


def test_prefetch_queries_the_client_it_passed_the_gate_with(tmp_path, ctx_env, fake_client_cls):
    """A re-init between prefetch()'s gate and its recall must not send this turn's query
    to the next binding's server."""
    p = _init(tmp_path)
    old = p._client
    real_lock = p._prefetch_lock

    class RebindOnEnter:
        def __enter__(self):
            p._prefetch_lock = real_lock
            ctx_env.setenv("CTX_KEY", KEY_B)
            ctx_env.setenv("CTX_BASE_URL", URL_B)
            p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "beta"), agent_context="primary")
            return real_lock.__enter__()

        def __exit__(self, *exc):
            return real_lock.__exit__(*exc)

    p._prefetch_lock = RebindOnEnter()
    p.prefetch("alpha turn")
    new = p._client
    assert new is not old
    assert [r[1] for r in old.reads] == ["alpha turn"]
    assert all(r[1] != "alpha turn" for r in new.reads)


def test_manifest_cache_kept_on_same_binding(tmp_path, ctx_env, fake_client_cls):
    p = _init(tmp_path)
    p._last_manifest_by_root = {"root-1": MANIFEST_ID}
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / "alpha"), agent_context="primary")
    assert p._last_manifest_by_root == {"root-1": MANIFEST_ID}


def test_manifest_cache_survives_failed_reinitialize_on_same_binding(tmp_path, ctx_env, fake_client_cls):
    """A failed whoami leaves the provider inactive (no writes); the cache belongs to the
    last verified binding and is still there when a retry on that binding succeeds."""
    from ctx_plugin.client import CtxApiError

    p = _init(tmp_path)
    p._last_manifest_by_root = {"root-1": MANIFEST_ID}
    home = str(tmp_path / "profiles" / "alpha")
    whoami = fake_client_cls.whoami

    def fail(self):
        raise CtxApiError("connection error: simulated")

    ctx_env.setattr(fake_client_cls, "whoami", fail)
    p.initialize("sess-next", hermes_home=home, agent_context="primary")
    assert not p._active
    ctx_env.setattr(fake_client_cls, "whoami", whoami)
    p.initialize("sess-next", hermes_home=home, agent_context="primary")
    assert p._active
    assert p._last_manifest_by_root == {"root-1": MANIFEST_ID}


@pytest.mark.parametrize(
    "change",
    [
        pytest.param({"CTX_KEY": KEY_B}, id="other-key"),
        pytest.param({"CTX_BASE_URL": URL_B}, id="other-backend"),
        pytest.param({}, id="other-profile"),
    ],
)
def test_manifest_cache_dropped_on_other_binding(tmp_path, ctx_env, fake_client_cls, change):
    p = _init(tmp_path)
    p._last_manifest_by_root = {"root-1": MANIFEST_ID}
    for name, value in change.items():
        ctx_env.setenv(name, value)
    profile = "alpha" if change else "beta"
    p.initialize("sess-next", hermes_home=str(tmp_path / "profiles" / profile), agent_context="primary")
    assert p._active
    assert p._last_manifest_by_root == {}


# -- re-initialization of the umbrella over MCP (finding: no stale state) --------------------


def _umbrella_cls():
    name = "ctx_checkpoint_key_redaction_umbrella"
    module = sys.modules.get(name)
    if module is None:
        path = Path(__file__).resolve().parents[2] / "__init__.py"
        spec = importlib.util.spec_from_file_location(name, path, submodule_search_locations=[str(path.parent)])
        module = importlib.util.module_from_spec(spec)
        sys.modules[name] = module
        spec.loader.exec_module(module)
    return module.CtxCheckpointMemoryProvider


def test_mcp_reinitialize_rebinds_client_identity_and_chain_writer():
    """The MCP ``_connect`` binds a fresh McpClient and every identity field unconditionally,
    holds no key, never ends inactive, and ``_chain()`` rebuilds a writer whose client is stale."""
    provider = _umbrella_cls()(dispatch=lambda *_a, **_k: json.dumps({"result": "[]"}))
    provider.initialize("session-a", platform="cli")
    old_client, old_writer = provider._client, provider._chain()
    assert provider._active and old_writer.client is old_client
    provider.initialize("session-b", platform="cli")
    assert provider._active and provider._client is not old_client
    assert (provider._api_key, provider._base_url, provider._home_scope) == ("", "", "")
    assert provider._whoami_label == "mcp:ctx"
    writer = provider._chain()
    assert writer is not old_writer and writer.client is provider._client
