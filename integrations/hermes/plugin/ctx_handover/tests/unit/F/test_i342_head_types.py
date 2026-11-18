"""GottZ/ctx#342: the head search of a handover record names the checkpoint type.

ctx may leave the types that retrieval never ranks (``checkpoint`` among them) out of a search that names no type, so
the search for ctx_checkpoint's head sends ``types=["checkpoint"]``. A ctx server before #342 refuses that over MCP —
``block type "checkpoint" is not retrieval-visible`` (v5.8.0 to v5.52.x; the text is pinned in ctx_checkpoint's
``memory/tests/mcp_answers.json``, ``search.types_refused``, which the ctx side checks against its own code) or, before
v5.8.0, ``unexpected additional properties ["types"]``. The record then searches without the type, the process
remembers the refusal, and a search without the type that finds no head asks with the type again. Any other failure
is not a refusal."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from ctx_handover.engine import ctxrecords

FIXTURE = Path(__file__).resolve().parents[4] / "ctx_checkpoint" / "memory" / "tests" / "mcp_answers.json"
REFUSED = json.loads(FIXTURE.read_text(encoding="utf-8"))["search"]["types_refused"]
UNKNOWN_ARG = 'validating "arguments": validating root: unexpected additional properties ["types"]'
ROOT = "20261118_120000_i342"
TITLE = f"Compaction checkpoint head {ROOT}"
HEAD_ID = "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0342"
ROWS = [
    {"id": "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0001", "title": TITLE, "type": "knowledge"},
    {"id": HEAD_ID, "title": TITLE, "type": "checkpoint"},
]


class Ctx:
    """``call_mcp`` of a ctx server: ``refuses`` answers a search with ``types`` (``None`` = takes them);
    ``drops_never_ranked`` leaves checkpoint rows out of a search without ``types`` (the later step of #342)."""

    def __init__(self, *, refuses: str | None = None, drops_never_ranked: bool = False) -> None:
        self.refuses = refuses
        self.drops_never_ranked = drops_never_ranked
        self.searches: list[dict] = []

    def call_mcp(self, server: str, tool: str, arguments: dict | None = None, timeout: float = 30) -> dict:
        assert tool == "search", tool
        args = dict(arguments or {})
        self.searches.append(args)
        types = args.get("types")
        if types and self.refuses is not None:
            return {"ok": False, "error": self.refuses}
        rows = [r for r in ROWS if r["title"] == args.get("query")]
        if types:
            rows = [r for r in rows if r["type"] in types]
        elif self.drops_never_ranked:
            rows = [r for r in rows if r["type"] != "checkpoint"]
        return {"ok": True, "result": json.dumps({"results": rows})}


@pytest.fixture(autouse=True)
def _fresh_process_memory():
    remembered = getattr(ctxrecords, "_types_refused", {})
    remembered.clear()
    yield
    remembered.clear()


def _types_sent(ctx: Ctx) -> list:
    return [args.get("types") for args in ctx.searches]


def test_the_head_search_names_the_checkpoint_type():
    ctx = Ctx()
    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID
    assert ctx.searches == [{"query": TITLE, "limit": 5, "types": ["checkpoint"]}]


@pytest.mark.parametrize("refusal", [REFUSED, UNKNOWN_ARG], ids=["not-retrieval-visible", "unknown-argument"])
def test_an_older_server_is_searched_without_the_type_once_per_process(refusal):
    ctx = Ctx(refuses=refusal)
    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID, "the knowledge row is not the head"
    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID
    assert _types_sent(ctx) == [["checkpoint"], None, None]
    # Another server of the same process is asked with the type.
    assert ctxrecords._checkpoint_head(Ctx().call_mcp, "other", ROOT, 5.0) == HEAD_ID
    assert ctxrecords._types_refused == {"ctx": True}


@pytest.mark.parametrize(
    "error",
    [
        'unknown block type "checkpoint"',
        'block type "insight" is not retrieval-visible',
        "read rate limit exceeded",
        'unexpected additional properties ["types_exclude"]',
    ],
)
def test_any_other_failure_finds_no_head_and_is_not_remembered(error):
    ctx = Ctx(refuses=error)
    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) is None
    assert _types_sent(ctx) == [["checkpoint"]], "no search without the type"
    assert ctxrecords._types_refused == {}


@pytest.mark.parametrize(
    "ctx",
    [Ctx(), Ctx(drops_never_ranked=True), Ctx(refuses=REFUSED)],
    ids=["with-342", "after-the-later-step", "before-342"],
)
def test_the_record_finds_the_head_on_every_server(ctx):
    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID


def test_a_head_search_without_the_type_misses_the_head_after_the_later_step():
    """The premise of the test above: a server of the later step answers a search that names no type without it."""
    envelope = Ctx(drops_never_ranked=True).call_mcp("ctx", "search", {"query": TITLE, "limit": 5})
    assert ctxrecords._head_in(envelope, TITLE) is None


def test_after_a_refusal_a_server_updated_meanwhile_is_asked_again():
    ctx = Ctx(refuses=REFUSED)
    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID
    ctx.refuses, ctx.drops_never_ranked = None, True
    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID
    assert _types_sent(ctx) == [["checkpoint"], None, None, ["checkpoint"]]
    assert ctxrecords._types_refused == {}
