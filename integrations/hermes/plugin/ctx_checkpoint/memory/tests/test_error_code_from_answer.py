"""The code of an MCP tool error comes from the form of ctx's answer about the call (GottZ/ctx#354, 0.16.1).

Hermes hands a tool error over as its text alone (``tools/mcp_tool_handlers.py``:
``tool_error(_sanitize_error(text))``); the code ctx puts next to it (``StructuredContent``) does not arrive. Before
0.16.1 the client coded every error whose text held ``rate limit`` / ``rate_limit`` as ``rate_limit`` — also a refusal
that repeats what the caller sent (a type named ``rate_limit``) and a transport error that only mentions a rate limit
— and every store error naming ``confirm_writes`` as a staged write. Now ``rate_limit`` is ctx's whole answer
``Rate limit exceeded: max <n> … per 60 seconds`` (``handler/ratelimit.go``, ``stage_gates.go``; pinned server-side in
``errcode_envelope_integration_test.go`` and ``recent_parity_integration_test.go``), and ``confirm_writes`` an answer
that starts ``STAGED — NOT``.
"""

from __future__ import annotations

import json

import pytest

from ctx_plugin import tools
from ctx_plugin.checkpoint import ChainWriter, CtxCheckpointError
from ctx_plugin.client import CtxApiError
from ctx_plugin.mcp_client import McpClient, _TypesUnknown

# ctx's answers when a key's budget is spent, as Hermes passes them on.
RATE_LIMIT_ANSWERS = (
    "Rate limit exceeded: max 100 writes per 60 seconds",
    "Rate limit exceeded: max 1 reads per 60 seconds",
    "Rate limit exceeded: max 20 blob writes per 60 seconds",
)
STAGED = (
    "STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).\n"
    "payload_hash: 0123456789abcdef0123456789abcdef\nexpires: never (writes.confirm_ttl = 0)"
)
# Errors that hold the words but are no rate-limit answer of ctx: a refusal repeating the caller's type names, a
# Hermes transport error that mentions a rate limit, and the sentence itself quoted inside another answer.
NOT_RATE_LIMITS = (
    'unknown block type "rate_limit"',
    'unknown block type "Rate limit exceeded: max 1 writes per 60 seconds"',
    "MCP call failed: McpError: upstream proxy answered: rate limit of the gateway reached",
    "search failed: rate_limit column missing",
)


def _error(text):
    return lambda name, args: json.dumps({"error": text}, ensure_ascii=False)


@pytest.mark.parametrize("answer", RATE_LIMIT_ANSWERS)
@pytest.mark.parametrize("tool", ["store", "search", "query"])
def test_ctx_rate_limit_answer_is_coded_rate_limit(tool, answer):
    client = McpClient(_error(answer))
    calls = {
        "store": lambda: client.store("c", "t", "x"),
        "search": lambda: client.search("q"),
        "query": lambda: client.query("q", synthesize=True),
    }
    with pytest.raises(CtxApiError) as raised:
        calls[tool]()
    assert raised.value.code == "rate_limit" and raised.value.status == 429
    assert str(raised.value) == f"mcp {tool}: {answer}"


@pytest.mark.parametrize("text", NOT_RATE_LIMITS)
def test_an_error_that_only_holds_rate_limit_words_keeps_its_text_and_no_code(text):
    client = McpClient(_error(text))
    with pytest.raises(CtxApiError) as raised:
        client.store("c", "t", "x")
    assert raised.value.code == "" and raised.value.status is None
    assert str(raised.value) == f"mcp store: {text}"


def test_a_transport_failure_naming_a_rate_limit_is_not_coded():
    """Hermes raising instead of answering (a dispatch error) is not ctx's answer either."""

    def dispatch(name, args):
        raise RuntimeError("MCP call failed: rate limit of the gateway reached")

    with pytest.raises(CtxApiError) as raised:
        McpClient(dispatch).store("c", "t", "x")
    assert raised.value.code == "" and raised.value.status is None


class _TypesServer:
    """A ctx server before #342 that refuses never-ranked types, and one that does not know a type name."""

    def __init__(self):
        self.unknown: set[str] = set()

    def dispatch(self, name, args):
        types = args.get("types") or []
        for type_name in types:
            if type_name in self.unknown:
                return json.dumps({"error": f'unknown block type "{type_name}"'})
        if "checkpoint" in types:
            return json.dumps({"error": 'block type "checkpoint" is not retrieval-visible'})
        return json.dumps({"result": "[]"})


@pytest.mark.parametrize("remembered", [False, True], ids=["fresh client", "after a remembered refusal"])
@pytest.mark.parametrize("name", ["rate_limit", "rate limit", "confirm_writes"])
def test_a_search_naming_a_type_with_code_words_is_refused_as_an_unknown_type(name, remembered):
    """The issue's case: ``types=["rate_limit"]`` on a server that knows no such type. On a fresh client the typed
    search answers it; on a client that remembered an older server's refusal the search goes without types first and
    the extra ask with types answers it. Both ways raise the unknown type, uncoded."""
    server = _TypesServer()
    client = McpClient(server.dispatch)
    if remembered:
        assert client.search("head", types=["checkpoint"])["results"] == []
        assert client._refused_types == {"checkpoint"}
    server.unknown.add(name)
    with pytest.raises(_TypesUnknown) as raised:
        client.search("head", types=["checkpoint", name] if remembered else [name])
    assert raised.value.code == "" and raised.value.status is None
    assert str(raised.value) == f'mcp search: unknown block type "{name}"'


def test_the_agent_tool_reports_such_a_refusal_as_a_failure_not_as_a_retryable_rate_limit():
    """What the model reads: before 0.16.1 ``code: rate_limit, retryable: true`` for a type name it chose."""
    from types import SimpleNamespace

    server = _TypesServer()
    server.unknown.add("rate_limit")
    provider = SimpleNamespace(
        _active=True,
        _client=McpClient(server.dispatch),
        _config={},
        _profile="default",
        _bound_secrets=lambda: (),
    )
    result = json.loads(tools.handle_tool_call(provider, "ctx_search", {"query": "x", "types": ["rate_limit"]}))
    assert "code" not in result and "retryable" not in result
    assert result["error"] == 'CTX tool ctx_search failed: mcp search: unknown block type "rate_limit"'


def test_staged_answer_is_coded_confirm_writes_and_an_echo_of_the_word_is_not():
    with pytest.raises(CtxApiError) as raised:
        McpClient(_error(STAGED)).store("c", "t", "x")
    assert raised.value.code == "confirm_writes"
    assert "payload_hash 0123456789abcdef0123456789abcdef" in str(raised.value)
    for echo in ('unknown block type "confirm_writes"', "refused: title names confirm_writes and STAGED"):
        with pytest.raises(CtxApiError) as raised:
            McpClient(_error(echo)).store("c", "t", "x", type="confirm_writes")
        assert raised.value.code == "" and str(raised.value) == f"mcp store: {echo}"


class _Clock:
    def __init__(self):
        self.now = 1000.0
        self.slept: list[float] = []

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.slept.append(seconds)
        self.now += seconds


@pytest.mark.parametrize(
    ("text", "slept", "message"),
    [
        (
            "Rate limit exceeded: max 100 writes per 60 seconds",
            [61.0],
            r"rate-limited during .*refused twice in a row",
        ),
        ('unknown block type "rate_limit"', [], r'failed: mcp store: unknown block type "rate_limit"'),
    ],
    ids=["ctx rate limit", "refusal naming rate_limit"],
)
def test_chain_writer_waits_a_window_only_for_ctx_rate_limit_answer(text, slept, message):
    """The chain writer's back-off: a rate limit is waited out once (a whole window, 61 s, when the chain has no write
    of its own in it) and a second one ends the checkpoint; any other error ends it at once with the server's text.
    Before 0.16.1 the refusal naming ``rate_limit`` took the first way: 61 s waited, then failed as a rate limit."""
    clock = _Clock()
    calls = []

    def dispatch(name, args):
        calls.append(name)
        if name.endswith("__search"):
            return json.dumps({"result": "[]"})
        return json.dumps({"error": text})

    writer = ChainWriter(client=McpClient(dispatch), monotonic=clock.monotonic, sleep=clock.sleep)
    with pytest.raises(CtxCheckpointError, match=message):
        writer.write(
            [{"role": "user", "content": "keep this"}, {"role": "assistant", "content": "kept"}],
            root_session_id="r354",
            session_id="r354",
            budget_s=60.0,
        )
    assert clock.slept == slept
