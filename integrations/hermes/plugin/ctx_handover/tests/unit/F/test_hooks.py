"""Cluster F — hook callbacks (DESIGN §7.6 veto, §11.4 note delivery, §8.3 side-question tokens)."""

from __future__ import annotations

from ctx_handover.engine.commands import HookSet, Plugin, SideQuestionTokens
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, FakeHookRuntime

SID = "20261109_141003_bbbbbb"
NOTE = "<ctx-handover-note job='01JC'>Moved from: server</ctx-handover-note>"


class Clock:
    def __init__(self) -> None:
        self.t = 1000.0

    def __call__(self) -> float:
        return self.t


def _hooks(rt: FakeHookRuntime | None = None) -> tuple[HookSet, FakeHookRuntime]:
    rt = rt or FakeHookRuntime()
    return HookSet(lambda: rt, SideQuestionTokens()), rt


def test_pre_llm_call_note_once():
    """★ DESIGN §11.4: the arrival note is offered as ``{"context": note}`` on the first turn, marked delivered by
    that turn's ``post_llm_call`` and never offered again; a turn that failed before finalising (no
    ``post_llm_call``) gets it again, at most 3 offers in total.

    Red proof: mutant = ``if not arrival or session_id in self._delivered`` without the ``_delivered`` check (offer
    again after delivery) → ``AssertionError: assert {'context': '<ctx-handover-note …'} is None``.
    """
    hooks, rt = _hooks()
    rt.arrivals[SID] = {"note": NOTE, "note_state": "pending", "offers": 0}
    assert hooks.pre_llm_call(session_id=SID, turn_id="t1") == {"context": NOTE}
    assert rt.offered == [(SID, "t1")]
    hooks.post_llm_call(session_id=SID, turn_id="t1")
    assert rt.delivered == [SID]
    assert hooks.pre_llm_call(session_id=SID, turn_id="t2") is None

    hooks, rt = _hooks()
    rt.arrivals[SID] = {"note": NOTE, "note_state": "pending", "offers": 0}
    offers = [hooks.pre_llm_call(session_id=SID, turn_id=f"t{i}") for i in range(5)]  # every turn failed
    assert offers[:3] == [{"context": NOTE}] * 3
    assert offers[3:] == [None, None]
    assert rt.delivered == []

    hooks, rt = _hooks()  # the ledger already counted two offers in an earlier process
    rt.arrivals[SID] = {"note": NOTE, "note_state": "offered", "offers": 2}
    assert hooks.pre_llm_call(session_id=SID, turn_id="t9") == {"context": NOTE}
    assert hooks.pre_llm_call(session_id=SID, turn_id="t10") is None

    hooks, rt = _hooks()
    rt.arrivals[SID] = {"note": NOTE, "note_state": "delivered", "offers": 1}
    assert hooks.pre_llm_call(session_id=SID, turn_id="t1") is None
    assert hooks.pre_llm_call(session_id="other", turn_id="t1") is None


def test_veto_blocks_lineage_and_tools():
    """Vetoed sessions (also through the parent of a rotated child) get ``{"action": "block"}`` for turns and
    tools; a compaction rotation is forwarded to the index (§7.6)."""
    hooks, rt = _hooks()
    rt.vetoed["root"] = {"peer": "laptop", "state": "retired"}
    block = hooks.pre_llm_call(session_id="root", turn_id="t")
    assert block == {"action": "block", "message": "This session was handed over to laptop. Run /handover reclaim."}
    assert hooks.pre_llm_call(session_id="child", parent_session_id="root")["action"] == "block"
    assert hooks.pre_tool_call(session_id="root", tool_name="terminal")["action"] == "block"
    assert hooks.pre_tool_call(session_id="free", tool_name="terminal") is None
    hooks.post_compaction(session_id="rotated", old_session_id="root", kind="rotation")
    assert rt.compactions == [("rotated", "root")]
    assert hooks.pre_llm_call(session_id="rotated")["action"] == "block"
    rt.arrivals["root"] = {"note": NOTE, "note_state": "pending"}
    assert hooks.pre_llm_call(session_id="root")["action"] == "block", "the veto wins over a note"


def test_hooks_never_raise():
    """§7.6: a failing index or a failing runtime start never raises into Hermes (``pre_tool_call`` is fail-closed
    in the core, so an exception there would block every tool)."""
    hooks, rt = _hooks()
    rt.raise_on_lookup = RuntimeError("index broken")
    assert hooks.pre_llm_call(session_id=SID) is None
    assert hooks.pre_tool_call(session_id=SID) is None

    def broken():
        raise OSError("ledger unreadable")

    hooks = HookSet(broken, SideQuestionTokens())
    assert hooks.pre_llm_call(session_id=SID) is None
    assert hooks.pre_tool_call(session_id=SID) is None
    assert hooks.post_llm_call(session_id=SID, side_question_token="x") is None
    assert hooks.post_compaction(session_id="a", old_session_id="b") is None


def test_hook_runtime_loaded_once():
    """Only the first hook call of a process builds the runtime (one-time ledger open); later calls are lookups."""
    built = []
    rt = FakeHookRuntime()

    def factory():
        built.append(1)
        return rt

    hooks = HookSet(factory, SideQuestionTokens())
    for _ in range(5):
        hooks.pre_llm_call(session_id=SID)
        hooks.pre_tool_call(session_id=SID)
    assert built == [1]


def test_side_question_token_capture_ttl():
    """§8.3: ``post_llm_call`` captures the token per session; it is usable once and only ≤ 60 s after the turn."""
    clock = Clock()
    tokens = SideQuestionTokens(clock=clock)
    hooks = HookSet(lambda: None, tokens)
    hooks.post_llm_call(session_id=SID, turn_id="t1", side_question_token="tok-1")
    clock.t += 59
    assert tokens.take(SID) == "tok-1"
    assert tokens.take(SID) is None, "one redemption per token"
    hooks.post_llm_call(session_id=SID, turn_id="t2", side_question_token="tok-2")
    clock.t += 61
    assert tokens.take(SID) is None, "tokens older than 60 s are unused"
    hooks.post_llm_call(session_id=SID, turn_id="t3", side_question_token=None)
    assert tokens.take(SID) is None


def test_side_question_needs_grant_and_fresh_token():
    """The plugin's side-question callable exists only with ``ctx.side_question`` and the granted capability, and
    answers only with a fresh token."""
    ctx = FakeCtx()
    plugin = Plugin(ctx, backend_factory=lambda c, p: FakeBackend())
    assert plugin.side_question_for(SID) is None  # capability not granted
    ctx.capabilities.add("llm.side_question")
    ask = plugin.side_question_for(SID)
    assert ask is not None and ask("q") is None  # no token yet
    plugin.hooks.post_llm_call(session_id=SID, side_question_token="tok")
    ctx.side_answers = ["answer"]
    assert ask("which resources?") == "answer"
    assert ctx.side_calls == [("tok", "which resources?", 45.0)]
    assert plugin.side_question_for(None) is None
