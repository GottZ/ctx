"""GottZ/ctx#289 (review of #564): the recall's second query for blocks of type ``session-digest``.

The type is damped (0.8), so a digest or a fact that alone answers a question can rank below the five
blocks the recall renders. After the recall's query, a second, small query that sees only that type adds
up to ``prefetch_session_digest_results`` of its blocks behind the five, which stay as they are. It is
sent only to a server entry that took a write naming the type in this process (a server without the type
refuses the query), never when its blocks would be dropped anyway, and within its own share of Hermes'
wait for the prefetch; a failure keeps the five.
"""

from __future__ import annotations

import json
import threading

import pytest
from conftest import FakeClient

import ctx_plugin.provider as provider_mod
from ctx_plugin import checkpoint as _cp
from ctx_plugin.client import CtxApiError, CtxClient
from ctx_plugin.config import SESSION_DIGEST_TYPE, UNTRUSTED_LABEL
from ctx_plugin.mcp_client import McpClient

MAIN = [{"id": f"k{i}", "title": f"Knowledge {i}", "category": "reference", "content": f"k {i}"} for i in range(4)] + [
    {"id": "d0", "title": "Session digest one", "category": "agent-briefing", "content": "d 0", "untrusted": True}
]
TYPED = [
    {"id": "d0", "title": "Session digest one", "category": "agent-briefing", "content": "d 0", "untrusted": True},
    {
        "id": "f1",
        "title": "fact: the queue drains at 03:00",
        "category": "learnings",
        "content": "f 1",
        "untrusted": True,
    },
    {"id": "f2", "title": "fact: the proxy is Traefik", "category": "learnings", "content": "f 2", "untrusted": True},
    {"id": "f3", "title": "fact: backups go to the NAS", "category": "learnings", "content": "f 3", "untrusted": True},
    {"id": "f4", "title": "fact: one more", "category": "learnings", "content": "f 4", "untrusted": True},
]


class TypedQueryServer(FakeClient):
    """Answers the recall's query with MAIN and the typed query with TYPED (or ``typed_raises``)."""

    typed_raises: Exception | None = None

    def __init__(self, *a, **k):
        super().__init__(*a, **k)
        self.query_result = {"success": True, "sources": [dict(s) for s in MAIN]}
        self.typed_calls: list = []

    def query_types(self, text, types, *, limit, timeout=None):
        self.typed_calls.append({"text": text, "types": list(types), "limit": limit, "timeout": timeout})
        if TypedQueryServer.typed_raises is not None:
            raise TypedQueryServer.typed_raises
        return {"success": True, "sources": [dict(s) for s in TYPED][:limit]}


@pytest.fixture
def typed_server(monkeypatch, fake_client_cls):
    TypedQueryServer.typed_raises = None
    monkeypatch.setattr(provider_mod, "CtxClient", TypedQueryServer)
    return TypedQueryServer


def _known(p) -> _cp.EntryState:
    entry = _cp.entry_state(p._entry_key())
    assert entry.type_to_send(SESSION_DIGEST_TYPE) == _cp.TYPE_ASK
    entry.type_answered(SESSION_DIGEST_TYPE, True)  # a write of this process named the type and was taken
    return entry


def _titles(context: str) -> list[str]:
    return [line.rsplit("] ", 1)[-1] for line in context.splitlines() if line.startswith("- ")]


def test_the_recall_adds_session_digests_the_query_did_not_answer(make_provider, typed_server):
    p, client = make_provider()
    _known(p)
    context = p.prefetch("when does the queue drain?")
    assert _titles(context) == [s["title"] for s in MAIN] + [s["title"] for s in TYPED[1:4]]
    assert p._last_recall_count == 8
    added = [line for line in context.splitlines() if line.startswith("- ") and "fact: " in line]
    assert len(added) == 3 and all(UNTRUSTED_LABEL in line for line in added)
    (call,) = client.typed_calls
    # Three wanted, and one place for the block of the type the five hold (marked untrusted).
    assert call["types"] == [SESSION_DIGEST_TYPE] and call["limit"] == 4
    assert 0 < call["timeout"] <= provider_mod.SESSION_DIGEST_RECALL_TIMEOUT_S
    p.shutdown()


@pytest.mark.parametrize(
    "config",
    [
        {"prefetch_session_digest_results": 0},
        {"prefetch_untrusted": "drop"},
        {"prefetch_types_exclude": ["checkpoint", SESSION_DIGEST_TYPE]},
    ],
    ids=["off", "untrusted_dropped", "type_excluded"],
)
def test_no_second_query_when_its_blocks_would_not_be_shown(make_provider, typed_server, config):
    p, client = make_provider(config=config)
    _known(p)
    p.prefetch("when does the queue drain?")
    assert client.typed_calls == []
    p.shutdown()


@pytest.mark.parametrize("verdict", [None, False], ids=["never_asked", "refused"])
def test_no_second_query_to_an_entry_that_has_not_taken_the_type(make_provider, typed_server, verdict):
    """A server without the type refuses the query (over MCP an error answer Hermes counts toward its
    breaker), so only an entry whose server took a write naming the type is asked."""
    p, client = make_provider()
    if verdict is False:
        entry = _cp.entry_state(p._entry_key())
        assert entry.type_to_send(SESSION_DIGEST_TYPE) == _cp.TYPE_ASK
        entry.type_answered(SESSION_DIGEST_TYPE, False)
    context = p.prefetch("when does the queue drain?")
    assert client.typed_calls == []
    assert _titles(context) == [s["title"] for s in MAIN]
    p.shutdown()


@pytest.mark.parametrize(
    "error",
    [CtxApiError("mcp query: no answer within 2.0s; the call was abandoned", code="timeout"), RuntimeError("boom")],
    ids=["timeout", "error"],
)
def test_a_failed_second_query_keeps_the_recalled_blocks(make_provider, typed_server, error):
    p, client = make_provider()
    entry = _known(p)
    typed_server.typed_raises = error
    context = p.prefetch("when does the queue drain?")
    assert len(client.typed_calls) == 1
    assert _titles(context) == [s["title"] for s in MAIN]
    assert entry.errors_in_row == 0  # the recall's reads count toward nothing of the entry's own state
    assert entry.type_known(SESSION_DIGEST_TYPE)  # and take no verdict
    assert not entry.recall_extra_paused()  # a REST call given up on holds nothing
    p.shutdown()


def test_no_second_query_when_hermes_wait_is_nearly_spent(make_provider, typed_server, monkeypatch):
    p, client = make_provider()
    _known(p)
    clock = iter([100.0, 100.0 + provider_mod.HOST_PREFETCH_WAIT_S - provider_mod.SESSION_DIGEST_RECALL_MARGIN_S])
    monkeypatch.setattr(p, "_now", lambda: next(clock))
    context = p.prefetch("when does the queue drain?")
    assert client.typed_calls == []
    assert _titles(context) == [s["title"] for s in MAIN]
    p.shutdown()


def test_the_first_query_failing_sends_no_second_one(make_provider, typed_server):
    p, client = make_provider()
    _known(p)
    client.query_raises = CtxApiError("connection error: simulated")
    p.prefetch("when does the queue drain?")
    assert client.typed_calls == []
    p.shutdown()


def test_the_mcp_client_asks_the_query_tool_with_types():
    calls: list = []
    rows = [dict(s) for s in TYPED[:2]]

    def dispatch(name, args):
        calls.append((name, dict(args)))
        return {"result": json.dumps({"confidence": "", "sources": rows})}

    client = McpClient(dispatch, "ctx")
    data = client.query_types("when?", [SESSION_DIGEST_TYPE], limit=4, timeout=1.5)
    assert [s["id"] for s in data["sources"]] == ["d0", "f1"]
    ((name, args),) = calls
    assert name == "mcp__ctx__query"
    assert args == {
        "question": "when?",
        "synthesize": False,
        "include_content": True,
        "limit": 4,
        "types": [SESSION_DIGEST_TYPE],
    }


def test_the_rest_client_excludes_the_other_types_it_lists_once(monkeypatch):
    client = CtxClient("http://ctx.invalid", "test-key-not-real")
    sent: list = []

    def request(method, path, payload=None, timeout=None):
        sent.append((path, payload))
        if payload.get("action") == "type-list":
            return {"success": True, "types": [{"name": n} for n in ("knowledge", SESSION_DIGEST_TYPE, "insight")]}
        return {"success": True, "sources": []}

    monkeypatch.setattr(client, "_request", request)
    client.query_types("when?", [SESSION_DIGEST_TYPE], limit=4, timeout=1.5)
    client.query_types("again?", [SESSION_DIGEST_TYPE], limit=3, timeout=1.5)
    assert [path for path, _ in sent] == ["/api/manage", "/api/query", "/api/query"]
    assert sent[1][1] == {
        "query": "when?",
        "synthesize": False,
        "include_content": True,
        "limit": 4,
        "types_exclude": ["insight", "knowledge"],
    }
    assert client.query_types("x", ["not-listed"], limit=2) == {"success": True, "answer": "", "sources": []}
    assert len(sent) == 3  # a type the server does not list is not queried


def test_a_query_without_sources_still_gets_the_session_digests(make_provider, typed_server):
    """A question only digests and facts answer can leave the first query empty: the second one still
    adds its blocks."""
    p, client = make_provider()
    _known(p)
    client.query_result = {"success": True, "sources": []}
    context = p.prefetch("when does the queue drain?")
    assert _titles(context) == [s["title"] for s in TYPED[:3]]
    assert client.typed_calls[0]["limit"] == 3
    p.shutdown()


class McpTypedQueryServer(TypedQueryServer):
    """The same double, reached over MCP with the grant: a bounded call through Hermes' call_mcp takes at
    least ``HOST_CALL_MIN_TIMEOUT_S`` and is waited for ``HOST_CALL_GRACE_S`` past its timeout."""

    transport = "mcp"

    def bounded_through_host(self, tool):
        return True


class McpNoGrantServer(McpTypedQueryServer):
    """Over MCP without the grant: the call goes through Hermes' tool dispatch, where an abandoned call
    holds the entry until the entry's tool timeout."""

    def bounded_through_host(self, tool):
        return False


@pytest.mark.parametrize(("elapsed", "sent"), [(5.0, True), (6.1, False)])
def test_over_mcp_the_second_query_fits_hermes_wait_with_the_grace_of_a_host_call(
    make_provider, monkeypatch, fake_client_cls, elapsed, sent
):
    """Over MCP the worst case of the second query is max(floor, timeout) + grace: it is sent only when
    that fits in what Hermes' wait leaves after the first query."""
    from ctx_plugin.mcp_client import HOST_CALL_GRACE_S, HOST_CALL_MIN_TIMEOUT_S

    TypedQueryServer.typed_raises = None
    monkeypatch.setattr(provider_mod, "CtxClient", McpTypedQueryServer)
    p, client = make_provider()
    _known(p)
    clock = iter([100.0, 100.0 + elapsed])
    monkeypatch.setattr(p, "_now", lambda: next(clock))
    p.prefetch("when does the queue drain?")
    assert bool(client.typed_calls) is sent
    if sent:
        timeout = client.typed_calls[0]["timeout"]
        assert timeout >= HOST_CALL_MIN_TIMEOUT_S
        worst = elapsed + max(HOST_CALL_MIN_TIMEOUT_S, timeout) + HOST_CALL_GRACE_S
        assert worst < provider_mod.HOST_PREFETCH_WAIT_S
    p.shutdown()


def test_a_block_the_first_answer_holds_but_does_not_show_may_come_from_the_second_query(make_provider, typed_server):
    """With ``max_prefetch_results`` below the server's five, a digest the first answer holds at a place
    the recall does not render is not taken as shown."""
    p, client = make_provider(config={"max_prefetch_results": 4})
    _known(p)
    context = p.prefetch("when does the queue drain?")  # MAIN's fifth block, d0, is not rendered
    assert _titles(context) == [s["title"] for s in MAIN[:4]] + [s["title"] for s in TYPED[:3]]
    assert client.typed_calls[0]["limit"] == 3  # no untrusted block among the four shown
    p.shutdown()


def test_the_rest_type_list_is_read_within_the_second_query_s_bound(monkeypatch):
    """The one type-list of a REST client counts against the second query's bound: the query gets what
    the listing left, and nothing is sent when it left nothing."""
    import ctx_plugin.client as client_mod

    now = {"t": 100.0}
    monkeypatch.setattr(client_mod, "_monotonic", lambda: now["t"])
    client = CtxClient("http://ctx.invalid", "test-key-not-real")
    sent: list = []

    def request(method, path, payload=None, timeout=None):
        sent.append((payload.get("action") or "query", timeout))
        if payload.get("action") == "type-list":
            now["t"] += listing_takes
            return {"success": True, "types": [{"name": n} for n in ("knowledge", SESSION_DIGEST_TYPE)]}
        return {"success": True, "sources": []}

    monkeypatch.setattr(client, "_request", request)
    listing_takes = 0.6
    client.query_types("when?", [SESSION_DIGEST_TYPE], limit=3, timeout=1.5)
    assert sent[0] == ("type-list", 1.5)
    assert sent[1][0] == "query" and sent[1][1] == pytest.approx(0.9)

    other = CtxClient("http://ctx.invalid", "test-key-not-real")
    sent.clear()
    monkeypatch.setattr(other, "_request", request)
    listing_takes = 2.0
    with pytest.raises(CtxApiError):
        other.query_types("when?", [SESSION_DIGEST_TYPE], limit=3, timeout=1.5)
    assert [what for what, _ in sent] == ["type-list"]


def _mcp_provider(make_provider, monkeypatch, server_cls, elapsed: float | None = None):
    TypedQueryServer.typed_raises = None
    monkeypatch.setattr(provider_mod, "CtxClient", server_cls)
    p, client = make_provider()
    entry = _known(p)
    if elapsed is not None:
        clock = iter([100.0, 100.0 + elapsed])
        monkeypatch.setattr(p, "_now", lambda: next(clock))
    return p, client, entry


@pytest.mark.parametrize(
    ("server_cls", "elapsed", "sent"),
    [
        (McpNoGrantServer, 0.5, True),
        (McpNoGrantServer, 1.5, False),
        (McpTypedQueryServer, 3.0, True),
    ],
    ids=["no_grant_fast_first", "no_grant_slow_first", "grant_slow_first"],
)
def test_without_the_grant_the_second_query_follows_only_a_fast_first_one(
    make_provider, monkeypatch, fake_client_cls, server_cls, elapsed, sent
):
    """Through Hermes' dispatch an abandoned second query holds the entry and the next recall waits
    behind it, so there it follows only a first query of at most SESSION_DIGEST_RECALL_FAST_MAIN_S; with
    the grant (Hermes frees the entry at the call's timeout) the time Hermes' wait leaves decides."""
    p, client, _entry = _mcp_provider(make_provider, monkeypatch, server_cls, elapsed)
    p.prefetch("when does the queue drain?")
    assert bool(client.typed_calls) is sent
    p.shutdown()


def test_without_the_grant_an_abandoned_second_query_keeps_it_off_for_the_entry(
    make_provider, monkeypatch, fake_client_cls, caplog
):
    monkeypatch.setattr(provider_mod, "SESSION_DIGEST_RECALL_PAUSE_S", 0.3)
    p, client, entry = _mcp_provider(make_provider, monkeypatch, McpNoGrantServer)
    TypedQueryServer.typed_raises = CtxApiError(
        "mcp query: no answer within 2.0s; the call was abandoned", code="timeout"
    )
    with caplog.at_level("WARNING"):
        first = p.prefetch("when does the queue drain?")
        second = p.prefetch("and the proxy?")
    assert _titles(first) == _titles(second) == [s["title"] for s in MAIN]
    assert len(client.typed_calls) == 1  # the second recall sent none
    assert entry.recall_extra_paused()
    warned = [r for r in caplog.records if r.levelname == "WARNING" and "second query" in r.getMessage()]
    assert len(warned) == 1 and "abandoned" in warned[0].getMessage()
    TypedQueryServer.typed_raises = None
    threading.Event().wait(0.4)
    p.prefetch("when does the queue drain?")
    assert len(client.typed_calls) == 2  # the pause ran out
    p.shutdown()


@pytest.mark.parametrize(
    ("server_cls", "error"),
    [
        (McpTypedQueryServer, CtxApiError("mcp query: no answer within 2.0s; the call was abandoned", code="timeout")),
        (McpNoGrantServer, CtxApiError("mcp query: refused", code="")),
    ],
    ids=["grant_timeout", "no_grant_error_answer"],
)
def test_a_second_query_that_holds_nothing_does_not_pause_it(
    make_provider, monkeypatch, fake_client_cls, server_cls, error
):
    """With the grant an abandoned call frees the entry, and an error answer holds nothing either: the
    next recall asks again."""
    p, client, entry = _mcp_provider(make_provider, monkeypatch, server_cls)
    TypedQueryServer.typed_raises = error
    p.prefetch("when does the queue drain?")
    p.prefetch("and the proxy?")
    assert len(client.typed_calls) == 2
    assert not entry.recall_extra_paused()
    TypedQueryServer.typed_raises = None
    p.shutdown()


def test_a_grant_refused_during_the_second_query_and_a_given_up_dispatch_pause_it(make_provider, caplog):
    """The second query starts through Hermes' call_mcp (the grant), Hermes refuses the grant during the
    call (PermissionError), the client sends it again through the dispatch, and that call is given up:
    it now holds the entry, so the second query pauses for the entry as on the dispatch path."""
    release = threading.Event()

    def dispatch(name, args):
        if name == "mcp__ctx__query" and args.get("types"):
            release.wait(30)  # never answers within the bound
        return {"result": json.dumps({"confidence": "", "sources": [dict(s) for s in MAIN]})}

    def call_mcp(server, tool, arguments=None, timeout=30):
        raise PermissionError("grant revoked")

    p, _client = make_provider()
    entry = _known(p)
    client = McpClient(dispatch, "ctx", host_call=lambda tool: call_mcp)
    assert client.bounded_through_host("query")
    p._client = client
    try:
        with caplog.at_level("WARNING"):
            context = p.prefetch("when does the queue drain?")
    finally:
        release.set()
    assert _titles(context) == [s["title"] for s in MAIN]
    assert not client.bounded_through_host("query")  # the client now goes through the dispatch
    assert entry.recall_extra_paused()
    assert [r for r in caplog.records if r.levelname == "WARNING" and "second query" in r.getMessage()]
    p.shutdown()
