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
    assert ctx.searches == [{"query": TITLE, "tags": ["checkpoint-head"], "limit": 5, "types": ["checkpoint"]}]


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


def test_a_miss_after_a_remembered_refusal_lets_the_next_record_ask_with_the_type():
    """A search without the type that finds no head drops the memory: the next record asks with the type again. On a
    server before #342 that costs one refused call after such a record. After an update to a server that leaves
    checkpoints out of a search without a type, the record that missed is the only one without the link when the
    update came after a lookup had finished and left the memory set, as arranged here, and one flusher runs; an
    update between a lookup's refused search and its search without the type costs the next record the link too."""
    old = Ctx(refuses=REFUSED)
    assert ctxrecords._checkpoint_head(old.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID
    assert ctxrecords._checkpoint_head(old.call_mcp, "ctx", "20261118_120000_headless", 5.0) is None
    assert ctxrecords._checkpoint_head(old.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID
    assert _types_sent(old) == [["checkpoint"], None, None, ["checkpoint"], None]
    assert ctxrecords._types_refused == {"ctx": True}

    old.refuses, old.drops_never_ranked = None, True
    assert ctxrecords._checkpoint_head(old.call_mcp, "ctx", ROOT, 5.0) is None, "the one record without the link"
    assert ctxrecords._checkpoint_head(old.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID
    assert _types_sent(old)[-2:] == [None, ["checkpoint"]]
    assert ctxrecords._types_refused == {}


def test_a_failure_of_the_search_without_the_type_keeps_the_memory():
    """Only a search that answers without the head drops the memory, not one that fails."""
    ctx = Ctx(refuses=REFUSED)
    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID

    def failing(server, tool, arguments=None, timeout=30):
        ctx.searches.append(dict(arguments or {}))
        return {"ok": False, "error": "read rate limit exceeded"}

    assert ctxrecords._checkpoint_head(failing, "ctx", ROOT, 5.0) is None
    assert ctxrecords._types_refused == {"ctx": True}


class _Clock:
    def __init__(self) -> None:
        self.now = 500.0

    def monotonic(self) -> float:
        return self.now


def test_the_search_without_the_type_gets_what_is_left_of_the_timeout(monkeypatch):
    """The refused search and the one without the type share one timeout: the second gets what the first left, and
    none is sent with less than a call's minimum."""
    clock = _Clock()
    monkeypatch.setattr(ctxrecords, "time", clock)
    ctx = Ctx(refuses=REFUSED)
    timeouts: list = []

    def timed(server, tool, arguments=None, timeout=30):
        timeouts.append(((arguments or {}).get("types"), timeout))
        clock.now += 0.4 if (arguments or {}).get("types") else 0.0
        return ctx.call_mcp(server, tool, arguments, timeout)

    assert ctxrecords._checkpoint_head(timed, "ctx", ROOT, 2.0) == HEAD_ID
    assert timeouts[0] == (["checkpoint"], 2.0)
    assert timeouts[1][0] is None and timeouts[1][1] == pytest.approx(1.6)
    ctxrecords._types_refused.clear()
    timeouts.clear()
    clock.now += 0.0
    assert ctxrecords._checkpoint_head(timed, "ctx", ROOT, 1.2) is None
    assert [t for t, _ in timeouts] == [["checkpoint"]], "0.8 s left: below a call's minimum, no second search"


def test_through_flush_with_the_default_budget_the_upgrade_costs_one_link(tmp_path):
    """The case through ``flush`` with its default budget: the process remembers a refusal, the server is then updated
    to one that leaves checkpoints out of a search without a type. The first record goes without the link (its search
    without the type missed and drops the memory), every later one carries it; each record is stored once."""
    from ctx_handover.tests.fakes.surfaces_fake import FakeOutboxLedger
    from ctx_handover.tests.unit.F.test_ctxrecords import _data

    ctx = Ctx(refuses=REFUSED)
    stores: list[dict] = []

    def call_mcp(server, tool, arguments=None, timeout=30):
        if tool == "store":
            stores.append(dict(arguments or {}))
            return {"ok": True, "result": f"Stored block (id: 0199{len(stores):04d}-0000-7000-8000-000000000000)"}
        return ctx.call_mcp(server, tool, arguments, timeout)

    assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", ROOT, 5.0) == HEAD_ID  # the refusal is remembered
    ctx.refuses, ctx.drops_never_ranked = None, True
    ledger = FakeOutboxLedger(tmp_path / "outbox.sqlite3")
    for n in range(3):
        record = _data(job_id=f"job-i342-{n}", source_root=ROOT, source_session=ROOT)
        ctxrecords.enqueue(ledger, "departure", record, redact=lambda s: s)
    for now in (5.0, 6.0, 7.0):
        ctxrecords.flush(ledger, call_mcp, "ctx", "handover", now=now)
    assert [s["metadata"].get("checkpoint_head") for s in stores] == [None, HEAD_ID, HEAD_ID]
    assert ledger.outbox_due(float("inf"), 1000) == [], "every row is delivered, none is left claimed or due"


def test_records_without_a_head_on_an_older_server_cost_two_and_one_searches_in_turn():
    """The cost the disclosure names: on a server before #342, a record whose root has no head costs the refused search
    and the one without the type (the refusal is remembered), the next costs the search without the type alone (it
    finds no head and drops the memory), and so on. A lookup does not drop the memory it set itself."""
    ctx = Ctx(refuses=REFUSED)
    searches = []
    for n in range(4):
        before = len(ctx.searches)
        assert ctxrecords._checkpoint_head(ctx.call_mcp, "ctx", f"20261118_120000_headless{n}", 5.0) is None
        searches.append(len(ctx.searches) - before)
    assert searches == [2, 1, 2, 1]
    assert _types_sent(ctx) == [["checkpoint"], None, None, ["checkpoint"], None, None]


def test_a_flush_searches_before_it_stores(tmp_path):
    """The record of a root with a checkpoint head: its search runs before its store (the disclosure in
    docs/security.md), and the store carries the head."""
    from ctx_handover.tests.fakes.surfaces_fake import FakeOutboxLedger
    from ctx_handover.tests.unit.F.test_ctxrecords import _data

    ctx = Ctx()
    calls: list[str] = []

    def call_mcp(server, tool, arguments=None, timeout=30):
        calls.append(tool)
        if tool == "store":
            assert (arguments or {}).get("metadata", {}).get("checkpoint_head") == HEAD_ID
            return {"ok": True, "result": "Stored block (id: 01990000-0000-7000-8000-000000000001)"}
        return ctx.call_mcp(server, tool, arguments, timeout)

    ledger = FakeOutboxLedger(tmp_path / "outbox.sqlite3")
    ctxrecords.enqueue(ledger, "departure", _data(source_root=ROOT, source_session=ROOT), redact=lambda s: s)
    assert ctxrecords.flush(ledger, call_mcp, "ctx", "handover", now=5.0) == 1
    assert calls == ["search", "store"]
