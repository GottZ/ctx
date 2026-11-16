"""Plugin 0.13.0 (GottZ/ctx#304): every foreign error text the plugin raises, logs or hands to the
model is redacted with the bound ctx keys — the rest of #301.

The ctx stand-ins below repeat the key of the request in their error texts (an artificial key;
whether ctx or Hermes ever does that is not claimed here). Each test checks the raised object
with every exception chained to it — ``__context__`` too, whether or not it is suppressed, since
Hermes has helpers that walk ``__cause__ or __context__`` — the log records with the exceptions
they carry, and the tool result the model reads.
"""

import importlib
import importlib.util
import json
import logging
import sys
import types
from pathlib import Path

import pytest

PLUGIN_DIR = Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_error_texts", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
Provider = _MODULE.CtxCheckpointMemoryProvider
CtxApiError = _MODULE.CtxApiError

CTX_KEY = "e1a2" + "0123456789abcdef" * 3 + "fedcba987654"  # artificial
BLOCK_ID = "019f6000-0000-7000-8000-000000000304"
ROOT = "20261116_031500_abcdef"
TRANSCRIPT = [
    {"role": "user", "content": "keep this wording"},
    {"role": "assistant", "content": "kept"},
]


def _chain(exc):
    """``exc`` and every exception reachable through ``__cause__`` and ``__context__``."""
    seen, todo = [], [exc]
    while todo:
        current = todo.pop()
        if current is None or any(current is known for known in seen):
            continue
        seen.append(current)
        todo += [current.__cause__, current.__context__]
    return seen


def _texts(exc):
    return " | ".join(f"{type(e).__name__}: {e} {e.args!r}" for e in _chain(exc))


def _records(caplog):
    """The log records as a handler formats them (traceback included) and the exceptions they carry."""
    formatter = logging.Formatter("%(levelname)s %(name)s %(message)s")
    parts = []
    for record in caplog.records:
        parts.append(formatter.format(record))
        if record.exc_info and record.exc_info[1] is not None:
            parts.append(_texts(record.exc_info[1]))
    return "\n".join(parts)


def _assert_clean(text):
    assert CTX_KEY not in text
    assert CTX_KEY[:12] not in text


@pytest.fixture
def home(_isolated_hermes_home):
    return _isolated_hermes_home


def _configure(home, **section):
    entry = {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${K}"}}
    config = {"mcp_servers": {"ctx": entry}, "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": section}}
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")
    (home / ".env").write_text(f"K={CTX_KEY}\n", encoding="utf-8")


def _echo_dispatch(fail, *, text=f"refused for key {CTX_KEY}", raw=None):
    """ctx stand-in: the tools in ``fail`` answer an error envelope that repeats the key (or raise
    ``raw``); the others answer like ctx."""

    def dispatch(name, args, **kwargs):
        tool = name.rsplit("__", 1)[-1]
        if tool in fail:
            if raw is not None:
                raise raw
            return json.dumps({"error": text})
        if tool == "search":
            return json.dumps({"result": "[]"})
        if tool == "get":
            return json.dumps({"result": json.dumps({"id": args.get("id"), "content": "c"})})
        if tool == "query":
            return json.dumps({"result": json.dumps({"confidence": "high", "sources": []})})
        return json.dumps({"id": BLOCK_ID})

    return dispatch


def _provider(home, dispatch, **section):
    _configure(home, **section)
    provider = Provider(dispatch=dispatch)
    provider.initialize(ROOT, platform="cli", hermes_home=str(home))
    return provider


# -- 1. The raised error: no raw text in __cause__ or __context__ --------------------------


@pytest.mark.parametrize(
    "text",
    [f"refused for key {CTX_KEY}", f"rate limit exceeded for key {CTX_KEY}"],
    ids=["rejected", "rate-limited"],
)
def test_a_failed_chain_raises_without_the_raw_error_in_its_context(home, caplog, monkeypatch, text):
    monkeypatch.setattr(_MODULE, "_sleep", lambda seconds: None)
    provider = _provider(home, _echo_dispatch({"store"}, text=text))

    with caplog.at_level(logging.DEBUG), pytest.raises(_MODULE.CtxCheckpointError) as raised:
        provider.on_pre_compress(list(TRANSCRIPT), require_checkpoint=True)

    _assert_clean(_texts(raised.value))
    assert raised.value.__context__ is None and raised.value.__cause__ is None
    _assert_clean(_records(caplog))


def test_an_unexpected_client_error_in_the_chain_is_not_chained(home, caplog, monkeypatch):
    provider = _provider(home, _echo_dispatch(set()))

    def boom(*args, **kwargs):
        raise RuntimeError(f"socket answered {CTX_KEY}")

    monkeypatch.setattr(provider._client, "store", boom)
    with caplog.at_level(logging.DEBUG), pytest.raises(_MODULE.CtxCheckpointError) as raised:
        provider.on_pre_compress(list(TRANSCRIPT), require_checkpoint=True)

    assert "RuntimeError" in str(raised.value)
    _assert_clean(_texts(raised.value))
    _assert_clean(_records(caplog))


def test_a_crashed_chain_raises_without_the_raw_error_in_its_context(home, caplog, monkeypatch):
    provider = _provider(home, _echo_dispatch(set()))

    def crash(*args, **kwargs):
        raise RuntimeError(f"unexpected {CTX_KEY}")

    monkeypatch.setattr(provider, "_write_chain", crash)
    with caplog.at_level(logging.DEBUG), pytest.raises(_MODULE.CtxCheckpointError) as raised:
        provider.on_pre_compress(list(TRANSCRIPT), require_checkpoint=True)

    _assert_clean(_texts(raised.value))
    _assert_clean(_records(caplog))


def test_a_failed_redaction_raises_without_the_text_it_failed_on(home, caplog, monkeypatch):
    """The redactor's own error can hold the message it was given (a UnicodeError keeps it whole):
    it must not ride along as cause or context of the raised error."""
    checkpoint = importlib.import_module(f"{_SPEC.name}.memory.checkpoint")
    provider = _provider(home, _echo_dispatch(set()))

    def fail(text):
        raise UnicodeEncodeError("ascii", text, 0, 1, "cannot redact")

    monkeypatch.setattr(checkpoint, "_host_redact", fail)
    # A secret no bound key covers: only the redactor that failed would have masked it.
    unbound = "passphrase periwinkle-7781-artificial"
    with caplog.at_level(logging.DEBUG), pytest.raises(_MODULE.CtxCheckpointError) as raised:
        provider.on_pre_compress([{"role": "user", "content": f"remember the {unbound}"}], require_checkpoint=True)

    assert "redaction failed" in str(raised.value)
    assert unbound not in _texts(raised.value)
    assert unbound not in _records(caplog)


def test_a_dispatch_exception_is_redacted_where_it_enters_the_plugin(home):
    provider = _provider(home, _echo_dispatch({"store"}, raw=RuntimeError(f"transport closed for {CTX_KEY}")))

    with pytest.raises(CtxApiError) as raised:
        provider._client.store("c", "t", "x")

    assert "mcp dispatch failed: RuntimeError" in str(raised.value)
    _assert_clean(_texts(raised.value))


def _mcp_client(dispatch):
    mcp_client = importlib.import_module(f"{_SPEC.name}.memory.mcp_client")
    return mcp_client.McpClient(dispatch, "ctx", secrets=lambda: (CTX_KEY,))


@pytest.mark.parametrize("tool", ["store", "search", "get"])
def test_the_mcp_client_redacts_an_error_envelope_where_it_enters(tool):
    client = _mcp_client(_echo_dispatch({tool}))
    calls = {
        "store": lambda: client.store("c", "t", "x"),
        "search": lambda: client.search("q"),
        "get": lambda: client.get_block(BLOCK_ID),
    }

    with pytest.raises(CtxApiError) as raised:
        calls[tool]()

    assert f"mcp {tool}: refused for key" in str(raised.value)
    _assert_clean(_texts(raised.value))


@pytest.mark.parametrize(
    ("tool", "answer"),
    [("store", f"Stored nothing: key {CTX_KEY} is read-only"), ("get", f"no block for key {CTX_KEY}")],
)
def test_the_mcp_client_redacts_an_answer_without_a_block(tool, answer):
    client = _mcp_client(lambda name, args, **kwargs: json.dumps({"result": answer}))

    data = client.store("c", "t", "x") if tool == "store" else client.get_block(BLOCK_ID)

    assert data["success"] is False
    _assert_clean(data["error"])


def test_the_content_fetch_line_redacts_what_get_block_raises(caplog, monkeypatch):
    sources = f"The proxy.\n\nSources:\n[1] proxy (infrastructure) id:{BLOCK_ID}"
    client = _mcp_client(lambda name, args, **kwargs: json.dumps({"result": sources}))

    def raw(*args, **kwargs):
        raise CtxApiError(f"get refused for key {CTX_KEY}")

    monkeypatch.setattr(client, "get_block", raw)
    with caplog.at_level(logging.DEBUG):
        data = client.query("which proxy?", synthesize=True)

    assert data["sources"][0]["id"] == BLOCK_ID
    logged = _records(caplog)
    assert "content fetch" in logged
    _assert_clean(logged)


# -- 2. The agent tools (memory features on) -------------------------------------------------

TOOL_CALLS = {
    "ctx_store": ("store", {"category": "learnings", "title": "t", "content": "a fact"}),
    "ctx_search": ("search", {"query": "proxy"}),
    "ctx_get": ("get", {"block_id": BLOCK_ID}),
    "ctx_query": ("query", {"text": "which proxy?"}),
}


@pytest.mark.parametrize("tool", sorted(TOOL_CALLS))
@pytest.mark.parametrize(
    ("text", "raw"),
    [
        (f"refused for key {CTX_KEY}", None),
        (f"rate limit exceeded for key {CTX_KEY}", None),
        ("", RuntimeError(f"transport closed for {CTX_KEY}")),
    ],
    ids=["error", "error-with-code", "dispatch-raises"],
)
def test_a_failed_tool_call_hands_the_model_no_key(home, caplog, tool, text, raw):
    mcp_tool, args = TOOL_CALLS[tool]
    provider = _provider(home, _echo_dispatch({mcp_tool}, text=text, raw=raw), memory=True)

    with caplog.at_level(logging.DEBUG):
        result = provider.handle_tool_call(tool, dict(args))

    assert "error" in json.loads(result)
    _assert_clean(result)
    _assert_clean(_records(caplog))


@pytest.mark.parametrize(
    ("tool", "answer"),
    [
        ("ctx_store", {"result": f"Stored nothing: key {CTX_KEY} is read-only"}),
        ("ctx_get", {"result": f"no block for key {CTX_KEY}"}),
    ],
)
def test_an_answer_without_a_block_hands_the_model_no_key(home, caplog, tool, answer):
    mcp_tool, args = TOOL_CALLS[tool]

    def dispatch(name, call_args, **kwargs):
        return json.dumps(answer) if name.endswith(f"__{mcp_tool}") else json.dumps({"result": "[]"})

    provider = _provider(home, dispatch, memory=True)
    with caplog.at_level(logging.DEBUG):
        result = provider.handle_tool_call(tool, dict(args))

    assert "error" in json.loads(result)
    _assert_clean(result)
    _assert_clean(_records(caplog))


RAW_ERRORS = {
    "rejected-with-code": lambda: CtxApiError(f"refused for key {CTX_KEY}", code=f"bad-{CTX_KEY}"),
    "failed-without-code": lambda: CtxApiError(f"refused for key {CTX_KEY}"),
    "unexpected": lambda: RuntimeError(f"transport closed for {CTX_KEY}"),
}


class _RawClient:
    """A client that hands out the server's error texts as they came (no redaction of its own):
    the tools and the provider must not rely on the client having redacted them."""

    transport = "mcp"

    def __init__(self, error="rejected-with-code"):
        self._error = RAW_ERRORS[error]

    def _fail(self, *args, **kwargs):
        raise self._error()

    store = search = get_block = query = _fail


class _RawEnvelopeClient:
    """A client that answers ``success: false`` with the server's text as it came."""

    transport = "mcp"

    def _envelope(self, *args, **kwargs):
        return {"success": False, "error": f"refused for key {CTX_KEY}", "code": f"bad-{CTX_KEY}"}

    store = search = get_block = query = _envelope


@pytest.mark.parametrize("tool", sorted(TOOL_CALLS))
@pytest.mark.parametrize("error", [*sorted(RAW_ERRORS), "envelope"])
def test_the_tools_redact_what_a_client_hands_them(home, caplog, tool, error):
    provider = _provider(home, _echo_dispatch(set()), memory=True)
    provider._client = _RawEnvelopeClient() if error == "envelope" else _RawClient(error)

    with caplog.at_level(logging.DEBUG):
        result = provider.handle_tool_call(tool, dict(TOOL_CALLS[tool][1]))

    assert "error" in json.loads(result)
    _assert_clean(result)
    _assert_clean(_records(caplog))


def test_the_prefetch_lines_redact_what_a_client_hands_them(home, caplog):
    provider = _provider(home, _echo_dispatch(set()), memory=True, prefetch_mode="search")
    provider._client = _RawClient()

    with caplog.at_level(logging.DEBUG):
        assert provider.prefetch("which proxy?") == ""
        provider.queue_prefetch("which proxy now?")
        provider._queue_thread.join(timeout=10)

    logged = _records(caplog)
    assert "prefetch failed" in logged and "warm-up failed" in logged
    _assert_clean(logged)


# -- 3. Prefetch, recall and digest debug lines -------------------------------------------


def test_prefetch_debug_lines_carry_no_key(home, caplog):
    provider = _provider(home, _echo_dispatch({"search", "query"}), memory=True, prefetch_mode="search")

    with caplog.at_level(logging.DEBUG):
        assert provider.prefetch("which proxy?") == ""
        provider.queue_prefetch("which proxy now?")
        provider._queue_thread.join(timeout=10)

    logged = _records(caplog)
    assert "prefetch failed" in logged and "warm-up failed" in logged
    _assert_clean(logged)


def test_the_content_fetch_debug_line_of_a_synthesized_query_carries_no_key(home, caplog):
    sources = f"The proxy.\n\nSources:\n[1] proxy (infrastructure) id:{BLOCK_ID}"

    def dispatch(name, args, **kwargs):
        if name.endswith("__query"):
            return json.dumps({"result": sources})
        return json.dumps({"error": f"get refused for key {CTX_KEY}"})

    provider = _provider(home, dispatch, memory=True)
    with caplog.at_level(logging.DEBUG):
        result = provider.handle_tool_call("ctx_query", {"text": "which proxy?", "synthesize": True})

    assert json.loads(result)["sources"][0]["id"] == BLOCK_ID
    logged = _records(caplog)
    assert "content fetch" in logged
    _assert_clean(logged)


def test_digest_debug_lines_carry_no_key(home, caplog, monkeypatch):
    provider = _provider(home, _echo_dispatch(set()), memory=True)

    def boom(*args, **kwargs):
        raise CtxApiError(f"digest refused for key {CTX_KEY}")

    monkeypatch.setattr(provider, "_digest_enabled", lambda: True)
    monkeypatch.setattr(provider, "_digest_state", boom)
    monkeypatch.setattr(provider, "_digest", boom)
    with caplog.at_level(logging.DEBUG):
        provider.sync_turn("u", "a", messages=list(TRANSCRIPT))
        provider._on_idle(list(TRANSCRIPT), provider._root_session_id or provider._session_id or "unknown")

    logged = _records(caplog)
    assert "digest check failed" in logged and "idle digest failed" in logged
    _assert_clean(logged)


# -- 4. Decisions read the raw code, never the redacted one (PR #306 review) ----------------


@pytest.fixture
def failing_redactor(monkeypatch):
    """Hermes' redactor raises: every redacted text is withheld (``ERROR_WITHHELD``)."""
    checkpoint = importlib.import_module(f"{_SPEC.name}.memory.checkpoint")

    def fail(text):
        raise RuntimeError("redactor unavailable")

    monkeypatch.setattr(checkpoint, "_host_redact", fail)
    return checkpoint


class _EnvelopeClient:
    transport = "mcp"

    def __init__(self, code):
        self._code = code

    def store(self, *args, **kwargs):
        return {"success": False, "error": "write window full", **({"code": self._code} if self._code else {})}


@pytest.mark.parametrize(("code", "retryable"), [("rate_limit", True), ("scope_denied", False), ("", None)])
def test_a_failing_redactor_changes_neither_the_code_nor_the_retry_decision(home, failing_redactor, code, retryable):
    provider = _provider(home, _echo_dispatch(set()), memory=True)
    provider._client = _EnvelopeClient(code)

    result = json.loads(provider.handle_tool_call("ctx_store", dict(TOOL_CALLS["ctx_store"][1])))

    assert result["error"].startswith("CTX store failed: ")
    if code:
        assert result["code"] == code and result["retryable"] is retryable
    else:
        assert "code" not in result and "retryable" not in result


def test_a_failing_redactor_keeps_the_rejection_of_a_ctx_api_error_retryable(home, failing_redactor):
    provider = _provider(home, _echo_dispatch(set()), memory=True)
    provider._client = _RawClient("rejected-with-code")
    provider._client._error = lambda: CtxApiError("rate limit exceeded", code="rate_limit", status=429)

    result = json.loads(provider.handle_tool_call("ctx_store", dict(TOOL_CALLS["ctx_store"][1])))

    assert result["code"] == "rate_limit" and result["retryable"] is True


def test_an_old_server_is_recognized_on_the_raw_refusal_even_when_redaction_fails(failing_redactor):
    """The fallback to ``search`` for a server from before ``synthesize:false`` reads the refusal
    where it arrives, not the redacted text (withheld when the redactor fails)."""
    calls = []
    refusal = 'validating "arguments": unexpected additional properties ["synthesize" "include_content"]'

    def dispatch(name, args, **kwargs):
        calls.append(name)
        if name.endswith("__query"):
            return json.dumps({"error": refusal})
        return json.dumps({"result": "[]"})

    data = _mcp_client(dispatch).query("which proxy?")

    assert data["success"] is True
    assert calls == ["mcp__ctx__query", "mcp__ctx__search"]


def test_a_failing_redactor_keeps_an_empty_code_out_of_the_chain_error(home, failing_redactor):
    """The chain writer names a rejection code only when the answer carried one."""
    checkpoint = failing_redactor
    writer = checkpoint.ChainWriter.__new__(checkpoint.ChainWriter)
    writer.secret = (CTX_KEY,)
    writer.api_timeout = 5.0
    writer.monotonic = lambda: 0.0
    run = types.SimpleNamespace(deadline=60.0, budget_s=60.0)

    with pytest.raises(checkpoint.CtxCheckpointError) as raised:
        writer._call("head store", run, lambda **kwargs: {"success": False, "error": "refused"})

    assert "[" not in str(raised.value)
