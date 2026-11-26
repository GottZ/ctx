"""GottZ/ctx#289 point 4 over Hermes' MCP dispatch: digests and facts name the block type
``session-digest``; a ctx server without it refuses one write per process and entry.

The double is the one of ``test_paced_idempotent_chain.py`` (one ctx MCP server behind Hermes'
dispatch: the key's write window, Hermes' RPC lock and its count of error answers in a row, the
breaker's input). Its variant here answers a store naming a type it does not know with ctx's own
text (``mcp_answers.json``, section ``session_digest``) before the write limit, as ctx does
(``handler/stage_gates.go``: the claim gates run before the rate check), and books nothing for it.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

from test_paced_idempotent_chain import (
    _MODULE,
    HOST_BREAKER_THRESHOLD,
    CtxServerDouble,
    _provider,
    _sleep_outside_the_lock,
)
from test_paced_idempotent_chain import clock as clock  # the fixture, re-exported for pytest

_cp = _MODULE._cp
_mcp_client = sys.modules[f"{_MODULE.__name__}.memory.mcp_client"]
_SD = json.loads(
    (Path(__file__).resolve().parents[1] / "memory" / "tests" / "mcp_answers.json").read_text(encoding="utf-8")
)["session_digest"]
FACTS = [
    "The proxy on INFRA-WWW terminates TLS for every service with Traefik 3.2 since 2026-10-01.",
    "Backups of the gitea database run nightly at 03:15 to the NAS share /volume1/backup/gitea.",
    "The staging cluster uses PostgreSQL 18.6 with pgvector 0.8.7 on host db-stage-02.",
]


class TypedServerDouble(CtxServerDouble):
    """The double with a registry: a store naming a type outside ``known`` is refused (ctx's text),
    before the write limit and without a booking; the stored blocks keep the type they were sent with."""

    def __init__(self, clock, *, known: tuple[str, ...], **kwargs) -> None:
        super().__init__(clock, **kwargs)
        self.known = set(known)
        self.type_refusals: list[dict] = []

    def _store(self, args: dict) -> dict:
        block_type = args.get("type")
        if block_type and block_type not in self.known:
            self.type_refusals.append(args)
            return {"error": _SD["unknown_type"].format(type=block_type)}
        return super()._store(args)


def _messages() -> list[dict]:
    return [
        m for fact in FACTS for m in ({"role": "user", "content": "And then?"}, {"role": "assistant", "content": fact})
    ]


def _digest_turns(tag: str) -> list[dict]:
    out = []
    for i in range(4):
        out.append({"role": "user", "content": f"Question {tag}{i}: what is the state of service {tag}{i}?"})
        out.append({"role": "assistant", "content": f"Service {tag}{i} runs on port {8000 + i}, health check green."})
    return out


def _join(provider) -> None:
    thread = provider._write_thread
    if thread is not None:
        thread.join(10)


def _facts(server: CtxServerDouble) -> list[dict]:
    return [b for b in server.blocks.values() if b["category"] == "learnings"]


def _digests(server: CtxServerDouble) -> list[dict]:
    return [b for b in server.blocks.values() if b["title"].startswith("Session digest ")]


def test_against_a_server_with_the_type_digests_and_facts_carry_it(monkeypatch, clock):
    server = TypedServerDouble(clock, known=("checkpoint", _SD["type"]))
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, memory=True)
    receipt = provider.on_pre_compress(_messages(), require_checkpoint=True)
    provider.on_session_end(_digest_turns("a"))
    _join(provider)
    assert "ctx checkpoint head:" in receipt and f"stored {len(FACTS)} fact block(s)" in receipt, receipt
    assert [b["type"] for b in _facts(server)] == [_SD["type"]] * len(FACTS)
    assert [b["type"] for b in _digests(server)] == [_SD["type"]]
    assert server.type_refusals == []


def test_against_a_server_without_the_type_one_write_is_refused_and_the_checkpoint_completes(monkeypatch, clock):
    """The checkpoint that completes today completes: the refused fact goes again without the type in
    the same run; Hermes sees one error answer, ended by the next answer; ctx booked no write for the
    refusal and the entry holds no slot for it; no later write of the process names the type."""
    server = TypedServerDouble(clock, known=("checkpoint",))
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, memory=True)
    receipt = provider.on_pre_compress(_messages(), require_checkpoint=True)

    assert "ctx checkpoint head:" in receipt and f"stored {len(FACTS)} fact block(s)" in receipt, receipt
    assert "not stored" not in receipt
    assert len(server.type_refusals) == 1 and server.type_refusals[0]["category"] == "learnings"
    assert [b["type"] for b in _facts(server)] == [None] * len(FACTS)
    assert server.max_consecutive_errors == 1 and server.consecutive_errors == 0
    entry = _cp.entry_state(provider._entry_key())
    assert entry.errors_in_row == 0
    assert len(entry.booked) == len(server.booked), (entry.booked, server.booked)

    provider.on_session_end(_digest_turns("a"))
    _join(provider)
    provider.on_session_switch("session-paced-2", parent_session_id="session-paced")
    more = [
        m
        for fact in ["The release host signs every tag with its SSH key since v5.0.0, checked in CI."]
        for m in ({"role": "user", "content": "And?"}, {"role": "assistant", "content": fact})
    ]
    provider.on_pre_compress(more, require_checkpoint=True)
    assert len(server.type_refusals) == 1, server.type_refusals
    assert [b["type"] for b in _digests(server)] == [None]
    assert len(_facts(server)) == len(FACTS) + 1 and all(b["type"] is None for b in _facts(server))


def test_a_refusal_at_the_limit_of_error_answers_lets_a_read_end_the_run_before_the_untyped_write(clock):
    """The refusal is an error answer of the entry (Hermes counts it): with one error answer in a row
    before it, the write after it waits for a read alone (the probe) instead of drawing a third."""
    server = TypedServerDouble(clock, known=("checkpoint",))
    server.consecutive_errors = 1  # Hermes saw one error answer of this entry before
    client = _MODULE.McpClient(server.dispatch, "ctx")
    entry = _cp.EntryState()
    admitted = entry.admit(write=False, window_writes=10, now=clock.now).ticket
    entry.release(admitted, error=True)  # the plugin saw it too
    writer = _cp.ChainWriter(client=client, category="compaction-checkpoints", entry=entry)
    writer.monotonic = lambda: clock.now

    def sleep(seconds: float) -> None:
        clock.now += seconds

    writer.sleep = sleep
    run = writer.new_run(started=clock.now, deadline=clock.now + 60.0, budget_s=60.0, planned_writes=1)
    try:
        data = writer.store_paced(
            "required fact 1",
            run,
            category="learnings",
            title="fact: The proxy on INFRA-WWW terminates TLS",
            content=FACTS[0],
            type=_SD["type"],
        )
    finally:
        run.close()
    assert data.get("success") is True
    tools = [(tool, args.get("type")) for tool, args in server.calls]
    assert tools == [("store", _SD["type"]), ("search", None), ("store", None)], tools
    assert server.max_consecutive_errors == 2 < HOST_BREAKER_THRESHOLD
    assert entry.errors_in_row == 0


def test_the_store_refusal_is_read_from_the_whole_answer_of_a_store_that_named_a_type():
    code = _mcp_client._code_for_error
    text = _SD["unknown_type"].format(type=_SD["type"])
    assert code("store", text, {"type": _SD["type"]}) == "unknown_type"
    assert code("store", f"  {text}\n", {"type": _SD["type"]}) == "unknown_type"
    assert code("store", text, {}) == ""  # a store that named no type
    assert code("search", text, {"types": [_SD["type"]]}) == ""
    assert code("store", f"title too long: {text}", {"type": _SD["type"]}) == ""  # words inside another answer


def test_the_agent_store_tool_reports_an_unknown_type_over_mcp_as_over_rest(monkeypatch, clock):
    """Declared change of 0.19.0: an agent's ``ctx_store`` naming a type the server does not know is
    reported over MCP as on REST — rejected with the code ``unknown_type``, not retryable — where it was
    a code-less failure; the tool sends no second write (the fallback is the plugin's own writes')."""
    server = TypedServerDouble(clock, known=("checkpoint",))
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, memory=True)
    out = json.loads(
        provider.handle_tool_call(
            "ctx_store", {"category": "learnings", "title": "t", "content": "c", "type": "no-such-type"}
        )
    )
    assert out.get("code") == "unknown_type" and out.get("retryable") is False, out
    assert "rejected" in out["error"]
    assert [tool for tool, _ in server.calls] == ["store"]


def _two_failed_agent_reads(provider) -> None:
    """Two of the agent's ``ctx_get`` calls for an id ctx does not hold: two error answers Hermes counts for
    the entry, which the plugin's entry state does not see (the agent's tools are not admitted there)."""
    for _ in range(2):
        out = json.loads(provider.handle_tool_call("ctx_get", {"block_id": "019f7000-0000-7000-8000-00000000dead"}))
        assert "error" in out, out


def test_after_two_foreign_error_answers_the_ask_of_a_digest_never_draws_a_third(monkeypatch, clock):
    """A fresh reviewer's run against the v5.87.0 handler: two failed agent reads, then the first digest of
    the process asked with the type — the refusal was Hermes' third error answer in a row, the breaker
    opened, the digest was lost and the next checkpoint failed; 0.18.0 stored the digest. The ask is now
    preceded by a listing of its category, whose answer ends the run."""
    server = TypedServerDouble(clock, known=("checkpoint",))
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, memory=True)
    _two_failed_agent_reads(provider)
    assert server.consecutive_errors == 2

    provider.on_session_end(_digest_turns("a"))
    _join(provider)

    assert server.max_consecutive_errors == 2 < HOST_BREAKER_THRESHOLD, server.calls
    tools = [(tool, args.get("type")) for tool, args in server.calls]
    assert tools == [("get", None), ("get", None), ("search", None), ("store", _SD["type"]), ("store", None)], tools
    assert [b["type"] for b in _digests(server)] == [None]
    assert server.consecutive_errors == 0


def test_after_two_foreign_error_answers_a_digest_on_a_server_with_the_type_is_stored_with_it(monkeypatch, clock):
    server = TypedServerDouble(clock, known=("checkpoint", _SD["type"]))
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, memory=True)
    _two_failed_agent_reads(provider)
    provider.on_session_end(_digest_turns("a"))
    _join(provider)
    provider.on_session_end(_digest_turns("a") + _digest_turns("b"))
    _join(provider)
    tools = [(tool, args.get("type")) for tool, args in server.calls]
    # One listing before the ask, none once the type is known.
    assert tools == [("get", None), ("get", None), ("search", None), ("store", _SD["type"]), ("store", _SD["type"])]
    assert [b["type"] for b in _digests(server)] == [_SD["type"]] * 2
    assert server.max_consecutive_errors == 2
