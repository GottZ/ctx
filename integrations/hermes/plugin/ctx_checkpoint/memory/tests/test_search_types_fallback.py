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
import time
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

    ``refuses``: the answer to a search with ``types`` (``None`` = takes them). ``drops_never_ranked``:
    a search without ``types`` leaves the never-ranked types out (the later step of #342)."""

    def __init__(self, *, refuses: str | None = None, drops_never_ranked: bool = False, rows=None):
        self.refuses = refuses
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
    # Remembered: the next search does not send the types again.
    client.search("checkpoint head", types=["checkpoint"])
    assert _types_sent(server) == [["checkpoint"], None, None]


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


def test_the_fallback_keeps_to_the_callers_timeout():
    """The search without types gets what is left of the call's timeout, not a second full one."""
    server = Server(refuses=REFUSED)

    def slow_refusal(name, args):
        if args.get("types"):
            time.sleep(0.4)
        return server.dispatch(name, args)

    client = McpClient(slow_refusal)
    timeouts: list = []
    dispatch_once = client._call

    def spy(tool, args, timeout=None):
        timeouts.append(timeout)
        return dispatch_once(tool, args, timeout)

    client._call = spy
    client.search("checkpoint head", types=["checkpoint"], timeout=1.0)
    assert timeouts[0] == 1.0
    assert timeouts[1] is not None and timeouts[1] <= 0.61


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
