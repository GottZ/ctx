"""``types`` on the MCP search and the fallback for a server that refuses them (GottZ/ctx#342).

A ctx server with #342 takes ``types=["checkpoint"]`` on its MCP ``search`` tool; an older one refuses a
type that retrieval never ranks (the fixture's ``search.types_refused``, v5.8.0 to v5.52.x) or, before
v5.8.0, the argument itself. The client then searches without ``types`` and narrows the rows to the asked
types, remembers the refusal, and asks again when such an answer holds none of them. Any other error is
raised. And a server that leaves never-ranked types out of a search without ``types`` (the later step of
#342) still answers every search the plugin makes, because they name the type.
"""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_plugin import tools
from ctx_plugin.checkpoint import ChainWriter, _ChainRun
from ctx_plugin.client import CtxApiError
from ctx_plugin.mcp_client import McpClient

FIXTURE = json.loads((Path(__file__).resolve().parent / "mcp_answers.json").read_text(encoding="utf-8"))
REFUSED = FIXTURE["search"]["types_refused"]
# The go-sdk's refusal of an argument a tool does not declare (a ctx server before v5.8.0), in the
# form the plugin already reads for the query tool (mcp_client.py, ``_UNKNOWN_ARGS_MARKER``).
UNKNOWN_ARG = 'validating "arguments": validating root: unexpected additional properties ["types"]'
HEAD_TITLE = "Compaction checkpoint head r1"
HEAD = {"id": "019f6000-0000-7000-8000-000000000001", "title": HEAD_TITLE, "category": "compaction-checkpoints"}
NEVER_RANKED = ("checkpoint", "system-meta")


def _row(row_id: str, title: str, block_type: str) -> dict:
    return {"id": row_id, "title": title, "category": "compaction-checkpoints", "type": block_type}


ROWS = [
    _row("019f6000-0000-7000-8000-0000000000a1", "notes on checkpoint heads", "knowledge"),
    {**HEAD, "type": "checkpoint"},
    _row("019f6000-0000-7000-8000-0000000000a2", "Compaction source part 1", "checkpoint"),
]


class Server:
    """The MCP ``search``/``get`` tools of a ctx server, as Hermes hands their answers to the plugin.

    ``refuses``: the answer to a search with ``types`` (``None`` = takes them). ``old``: a server from
    v5.8.0 to v5.52.x, which refuses a never-ranked type by name and takes the others. ``registered``:
    the type names the server knows (``None`` = all). ``drops_never_ranked``: a search without
    ``types`` leaves the never-ranked types out (the later step of #342)."""

    def __init__(
        self,
        *,
        refuses: str | None = None,
        old: bool = False,
        registered=None,
        drops_never_ranked: bool = False,
        rows=None,
    ):
        self.refuses = refuses
        self.old = old
        self.registered = registered
        self.drops_never_ranked = drops_never_ranked
        self.rows = list(ROWS if rows is None else rows)
        self.searches: list[dict] = []

    def dispatch(self, name: str, args: dict):
        tool = name.rsplit("__", 1)[-1]
        if tool == "get":
            meta = {"latest_manifest_id": "019f6000-0000-7000-8000-000000000201", "root_session_id": "r1"}
            return json.dumps({"result": json.dumps({**HEAD, "type": "checkpoint", "metadata": meta})})
        assert tool == "search", tool
        self.searches.append(dict(args))
        types = args.get("types")
        if types and self.refuses is not None:
            return json.dumps({"error": self.refuses})
        for name in types or ():
            if self.registered is not None and name not in self.registered:
                return json.dumps({"error": f'unknown block type "{name}"'})
            if self.old and name in NEVER_RANKED:
                return json.dumps({"error": REFUSED.replace('"checkpoint"', f'"{name}"')})
        rows = self.rows
        if types:
            rows = [r for r in rows if r.get("type") in types]
        elif self.drops_never_ranked:
            rows = [r for r in rows if r.get("type") not in NEVER_RANKED]
        return json.dumps({"result": json.dumps(rows)})


def _types_sent(server: Server) -> list:
    return [args.get("types") for args in server.searches]


def _ids(data: dict) -> list[str]:
    return [row["id"] for row in data["results"]]


def test_a_server_with_342_gets_the_types_and_answers_once():
    server = Server()
    client = McpClient(server.dispatch)
    data = client.search("checkpoint head", types=["checkpoint"])
    assert _ids(data) == [HEAD["id"], ROWS[2]["id"]]
    assert _types_sent(server) == [["checkpoint"]]


@pytest.mark.parametrize("refusal", [REFUSED, UNKNOWN_ARG], ids=["not-retrieval-visible", "unknown-argument"])
def test_an_older_server_is_searched_without_types_and_the_rows_are_narrowed(refusal):
    server = Server(refuses=refusal)
    client = McpClient(server.dispatch)
    data = client.search("checkpoint head", category="compaction-checkpoints", types=["checkpoint"])
    assert _ids(data) == [HEAD["id"], ROWS[2]["id"]], "the knowledge row is narrowed away"
    assert _types_sent(server) == [["checkpoint"], None]
    assert server.searches[1] == {"query": "checkpoint head", "category": "compaction-checkpoints"}
    # Remembered: the next search goes without the types first. Its narrowing dropped a row a typed
    # search would not have spent a place on, so it asks with the types once more (refused again).
    client.search("checkpoint head", types=["checkpoint"])
    assert _types_sent(server) == [["checkpoint"], None, None, ["checkpoint"]]


def test_a_row_without_a_type_survives_the_narrowing():
    """A server too old to name a row's type: the row is kept, as before this client sent types."""
    untyped = {k: v for k, v in HEAD.items()}
    server = Server(refuses=UNKNOWN_ARG, rows=[untyped])
    data = McpClient(server.dispatch).search("checkpoint head", types=["checkpoint"])
    assert _ids(data) == [HEAD["id"]]


@pytest.mark.parametrize(
    "error",
    [
        'unknown block type "checkpoint"',
        'block type "insight" is not retrieval-visible',
        "read rate limit exceeded (60/min)",
        "search failed: connection reset",
        'unexpected additional properties ["types_exclude"]',
    ],
)
def test_any_other_error_is_raised_and_not_searched_without_types(error):
    server = Server(refuses=error)
    client = McpClient(server.dispatch)
    with pytest.raises(CtxApiError):
        client.search("checkpoint head", types=["checkpoint"])
    assert _types_sent(server) == [["checkpoint"]], "no second search without the types"
    # Nothing remembered: the next search sends the types again.
    server.refuses = None
    assert _ids(client.search("checkpoint head", types=["checkpoint"]))[0] == HEAD["id"]
    assert _types_sent(server)[-1] == ["checkpoint"]


def test_a_refusal_raised_by_the_dispatch_falls_back_too():
    """Hermes can raise the transport's error instead of answering ``{"error"}``."""
    server = Server()

    def dispatch(name, args):
        if args.get("types"):
            raise RuntimeError(REFUSED)
        return server.dispatch(name, args)

    data = McpClient(dispatch).search("checkpoint head", types=["checkpoint"])
    assert _ids(data) == [HEAD["id"], ROWS[2]["id"]]


def test_a_search_without_types_is_unchanged():
    server = Server(refuses=REFUSED)
    data = McpClient(server.dispatch).search("checkpoint head", category="c", tags=["t"], limit=5)
    assert len(data["results"]) == 3
    assert server.searches == [{"query": "checkpoint head", "category": "c", "tags": ["t"], "limit": 5}]


def test_an_asked_type_without_a_row_asks_again_even_beside_rows_of_another():
    """Mixed types after the update: the answer without ``types`` holds knowledge rows but no checkpoint
    row, so the client asks with the types again instead of answering without the checkpoint rows."""
    server = Server(refuses=REFUSED)
    client = McpClient(server.dispatch)
    client.search("checkpoint head", types=["checkpoint", "knowledge"])
    server.refuses, server.drops_never_ranked = None, True
    data = client.search("checkpoint head", types=["checkpoint", "knowledge"])
    assert sorted(_ids(data)) == sorted(row["id"] for row in ROWS)
    assert _types_sent(server)[-2:] == [None, ["checkpoint", "knowledge"]]


def test_after_a_refusal_a_server_updated_meanwhile_is_asked_again():
    """The client met a server before #342; the server is updated to one that leaves never-ranked
    types out of a search without ``types``. The narrowed answer holds no checkpoint row, so the
    client asks with the types again — and finds the head."""
    server = Server(refuses=REFUSED)
    client = McpClient(server.dispatch)
    client.search("checkpoint head", types=["checkpoint"])
    server.refuses, server.drops_never_ranked = None, True
    data = client.search("checkpoint head", types=["checkpoint"])
    assert _ids(data) == [HEAD["id"], ROWS[2]["id"]]
    assert _types_sent(server) == [["checkpoint"], None, None, ["checkpoint"]]
    # From now on the types go first again.
    client.search("checkpoint head", types=["checkpoint"])
    assert _types_sent(server)[-1] == ["checkpoint"]


def test_a_failing_second_ask_leaves_the_answer_of_the_older_server():
    """The ask after an empty fallback is extra: a rate limit or timeout there must not turn the answer
    an older server gave (nothing found) into an error."""
    server = Server(refuses=REFUSED, rows=[])
    client = McpClient(server.dispatch)
    assert client.search("checkpoint head", types=["checkpoint"])["results"] == []
    server.refuses = "read rate limit exceeded (60/min)"
    assert client.search("checkpoint head", types=["checkpoint"])["results"] == []
    assert _types_sent(server) == [["checkpoint"], None, None, ["checkpoint"]]


class _Clock:
    """A monotonic clock the test moves: ``mcp_client.time`` is replaced by it."""

    def __init__(self) -> None:
        self.now = 1000.0

    def monotonic(self) -> float:
        return self.now


def _timed_client(monkeypatch, server: Server, cost: dict) -> tuple[McpClient, list, _Clock]:
    """A client whose dispatches take ``cost[<types or None>]`` seconds of the test clock; the timeout
    each dispatch got is recorded."""
    import ctx_plugin.mcp_client as mcp_client

    clock = _Clock()
    monkeypatch.setattr(mcp_client, "time", clock)
    client = McpClient(server.dispatch)
    timeouts: list = []
    dispatch_once = client._call

    def timed(tool, args, timeout=None):
        timeouts.append((args.get("types"), timeout))
        clock.now += cost.get("typed" if args.get("types") else "untyped", 0.0)
        return dispatch_once(tool, args, None)

    client._call = timed
    return client, timeouts, clock


def test_the_fallback_keeps_to_the_callers_timeout(monkeypatch):
    """The search without types gets what is left of the call's timeout, not a second full one."""
    client, timeouts, _clock = _timed_client(monkeypatch, Server(refuses=REFUSED), {"typed": 0.4})
    client.search("checkpoint head", types=["checkpoint"], timeout=1.0)
    assert timeouts[0] == (["checkpoint"], 1.0)
    assert timeouts[1][0] is None and timeouts[1][1] == pytest.approx(0.6)


def test_the_ask_again_leaves_the_caller_half_of_what_is_left(monkeypatch):
    """After a remembered refusal the ask with types is extra: it takes at most half of what is left of
    the call's timeout, so the caller keeps time for its next call (the chain writer reads the head it
    found). With too little left it is not sent."""
    server = Server(old=True)
    client, timeouts, _clock = _timed_client(monkeypatch, server, {"untyped": 2.0})
    client.search("checkpoint head", types=["checkpoint"], timeout=10.0)
    server.old, server.drops_never_ranked = False, True
    timeouts.clear()
    data = client.search("checkpoint head", types=["checkpoint"], timeout=10.0)
    assert _ids(data) == [HEAD["id"], ROWS[2]["id"]]
    assert [t for t, _ in timeouts] == [None, ["checkpoint"]]
    assert timeouts[1][1] == pytest.approx((10.0 - 2.0) / 2)
    # Memory cleared by the accepted ask: the old server comes back, the client is refused again,
    # then a search whose fallback leaves less than the minimum sends no extra ask.
    server.old, server.drops_never_ranked = True, False
    client.search("checkpoint head", types=["checkpoint"], timeout=10.0)
    server.old, server.drops_never_ranked = False, True
    timeouts.clear()
    assert client.search("checkpoint head", types=["checkpoint"], timeout=2.6)["results"] == []
    assert [t for t, _ in timeouts] == [None], "0.3 s would be half of what is left: no extra ask"


def test_a_refusal_of_one_type_does_not_stop_a_typed_search_for_another():
    """A server before #342 refuses ``checkpoint`` and takes ``knowledge``: after the refusal a search
    for knowledge is still sent with its type and answered by the server."""
    server = Server(old=True)
    client = McpClient(server.dispatch)
    client.search("checkpoint head", types=["checkpoint"])
    data = client.search("checkpoint head", types=["knowledge"])
    assert _ids(data) == [ROWS[0]["id"]]
    assert _types_sent(server) == [["checkpoint"], None, ["knowledge"]], "one typed search, no search without types"


def test_a_remembered_refusal_is_asked_again_when_the_narrowing_drops_rows():
    """The client met a server before #342; the server now takes the types but still answers a search
    without them with every type. The narrowing dropped a row, so the client asks with the types again,
    gets the typed answer and sends the types first from then on."""
    server = Server(old=True)
    client = McpClient(server.dispatch)
    client.search("checkpoint head", types=["checkpoint"])
    server.old = False
    data = client.search("checkpoint head", types=["checkpoint"])
    assert _ids(data) == [HEAD["id"], ROWS[2]["id"]]
    client.search("checkpoint head", types=["checkpoint"])
    assert _types_sent(server) == [["checkpoint"], None, None, ["checkpoint"], ["checkpoint"]]


@pytest.mark.parametrize(
    ("rows", "searches"),
    [
        ([{**HEAD, "type": "checkpoint"}], 1),
        ([], 2),
        (ROWS, 2),
    ],
    ids=["head-lookup-finds-the-head", "nothing-found", "narrowing-drops-a-row"],
)
def test_calls_against_an_older_server_after_the_refusal(rows, searches):
    """What a remembered refusal costs a search on a server before #342: one call when the answer
    without types holds rows of the asked type only (the head lookup by title, category and tag), two
    when it holds none of them or rows the narrowing drops (the extra ask, refused again)."""
    server = Server(old=True, rows=rows)
    client = McpClient(server.dispatch)
    client.search(HEAD_TITLE, types=["checkpoint"])
    before = len(server.searches)
    client.search(HEAD_TITLE, category="compaction-checkpoints", tags=["checkpoint-head"], types=["checkpoint"])
    assert len(server.searches) - before == searches


def test_rows_without_a_type_are_not_asked_again():
    """A server that names no row's type predates the type filter: its narrowed answer is not checked
    again with the types, even when no row carries the asked type."""
    server = Server(refuses=UNKNOWN_ARG, rows=[dict(HEAD)])
    client = McpClient(server.dispatch)
    client.search("checkpoint head", types=["checkpoint"])
    client.search("checkpoint head", types=["checkpoint"])
    assert _types_sent(server) == [["checkpoint"], None, None]


def test_an_unknown_type_in_the_ask_again_is_raised():
    """After a remembered refusal, a type the updated server does not know is refused by the ask again
    and raised — as a fresh client gets it — instead of answering an empty search."""
    server = Server(old=True)
    client = McpClient(server.dispatch)
    client.search("checkpoint head", types=["checkpoint"])
    server.old, server.registered = False, {"checkpoint", "knowledge"}
    with pytest.raises(CtxApiError) as remembered:
        client.search("checkpoint head", types=["checkpoint", "nope"])
    with pytest.raises(CtxApiError) as fresh:
        McpClient(server.dispatch).search("checkpoint head", types=["checkpoint", "nope"])
    assert "unknown block type" in str(remembered.value) and "unknown block type" in str(fresh.value)


def test_the_refusal_log_names_no_type_value(caplog):
    """A value a caller put into ``types`` by mistake (a key) is not written to the log."""
    server = Server(refuses=UNKNOWN_ARG)
    with caplog.at_level("DEBUG"):
        McpClient(server.dispatch).search("checkpoint head", types=["sk-live-0000-not-a-key"])
    assert caplog.records and not any("sk-live" in record.getMessage() for record in caplog.records)


@pytest.mark.parametrize("server", [Server(), Server(drops_never_ranked=True), Server(refuses=REFUSED)])
def test_the_chain_writer_finds_its_head(server):
    """With #342, after the later step that leaves never-ranked types out of a search without types,
    and on a server before #342: the head lookup finds the head on all three."""
    writer = ChainWriter(client=McpClient(server.dispatch))
    run = _ChainRun(deadline=writer.monotonic() + 30.0, budget_s=30.0, window_writes=writer._window_writes())
    manifest_id, _hashes, _cp = writer.load_previous_head(HEAD_TITLE, run)
    assert manifest_id == "019f6000-0000-7000-8000-000000000201"


def test_a_head_lookup_without_types_would_miss_the_head_after_the_later_step():
    """The premise of the test above: on that server a search that names no type finds no head."""
    server = Server(drops_never_ranked=True)
    data = McpClient(server.dispatch).search(HEAD_TITLE, tags=["checkpoint-head"])
    assert HEAD["id"] not in _ids(data)


def _tool_provider(server: Server):
    return SimpleNamespace(
        _active=True,
        _client=McpClient(server.dispatch),
        _config={"prefetch_types_exclude": ["checkpoint", "tool-evidence"]},
        _profile="default",
        _bound_secrets=lambda: (),
    )


def test_the_search_tool_names_types_and_drops_the_default_exclusion():
    """The plugin's ``ctx_search`` takes ``types`` (the read path names ``checkpoint``); named types
    replace the ``prefetch_types_exclude`` default, which would exclude the checkpoint type itself."""
    server = Server()
    provider = _tool_provider(server)
    out = json.loads(
        tools.handle_tool_call(provider, "ctx_search", {"query": "checkpoint head", "types": ["checkpoint"]})
    )
    assert [row["id"] for row in out["results"]] == [HEAD["id"], ROWS[2]["id"]]
    assert server.searches == [{"query": "checkpoint head", "types": ["checkpoint"]}]
    tools.handle_tool_call(provider, "ctx_search", {"query": "checkpoint head"})
    assert server.searches[-1] == {"query": "checkpoint head", "types_exclude": ["checkpoint", "tool-evidence"]}
