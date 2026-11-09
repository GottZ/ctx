"""D: an abandoned refinement step is never reused and leaves no state (R26, DESIGN §8.3).

A side question or plugin-LLM call that overruns ``refine_timeout_s`` keeps running in its daemon thread (Python
cannot cancel it). The chain must continue without it, its late answer must never be used — not by the call that
gave up on it, not by a later one — and once it ends nothing of it may remain: no thread, no module state, no
file, no reference to its outcome. Real threads, real timeouts.
"""

from __future__ import annotations

import gc
import os
import threading
import time
import weakref

from ctx_handover.engine import refine
from ctx_handover.engine.model import Candidate, ItemKind, Options, Origin, Weight
from ctx_handover.tests.fakes.planning_fake import env_facts, fake_redact, json_reply
from ctx_handover.tests.unit.D.conftest import tree_diff, tree_state

THREAD_NAME = "ctx_handover-refine"
FAST = {"tools_needed": ["make"], "notes_for_target": "llm answer"}
LATE = {"add": [{"path": "/home/u/late.md", "reason": "late"}], "notes_for_target": "late answer"}
FRESH = {"tools_needed": ["node"], "notes_for_target": "fresh answer"}


class LateError(RuntimeError):
    """Raised by a side question after its caller gave up on it."""


def _cands() -> list[Candidate]:
    return [Candidate(ItemKind.GIT_REPO, "/home/u/app", Weight.WRITE, ("cwd",), Origin.CWD)]


def _rows() -> list[dict]:
    return [{"role": "user", "content": "fix the login bug"}, {"role": "assistant", "content": "done"}]


def _refine(side_question, llm):
    return refine.refine(
        _cands(),
        env_facts("laptop"),
        approx_tokens=1000,
        options=Options(refine_timeout_s=1),
        side_question=side_question,
        llm=llm,
        stored=None,
        rows=_rows(),
        redact=fake_redact,
    )


def _module_state() -> dict[str, tuple[int, str]]:
    return {k: (id(v), repr(v)) for k, v in vars(refine).items() if not k.startswith("__")}


def _abandoned(older: frozenset = frozenset()) -> list[threading.Thread]:
    """Live refinement threads not in ``older`` (an earlier test's abandoned call may still be running)."""
    return [t for t in threading.enumerate() if t.name == THREAD_NAME and t.is_alive() and t not in older]


def _llm(reply):
    def call(instructions, blocks, schema, timeout_s):
        return reply

    return call


def test_refine_abandoned_call_is_dropped(tmp_path, monkeypatch, user_home):
    """★ R26: the late answer of an abandoned side question is not used by this or any later refinement, and after
    the abandoned thread ends no thread, module state, file or reference to its outcome remains.

    Mutant: ``_bounded`` keeps abandoned calls in a module-level list and ``refine`` serves their late answer."""
    cwd = tmp_path / "cwd"
    cwd.mkdir()
    monkeypatch.chdir(cwd)
    older = frozenset(_abandoned())
    state_before = _module_state()
    trees_before = {p: tree_state(p) for p in (user_home, cwd)}

    # 1. the side question overruns the 1 s timeout; the chain continues with the llm step
    gate = threading.Event()

    def late_side_question(prompt):
        gate.wait(5)
        return json_reply(LATE)

    start = time.monotonic()
    ref, report = _refine(late_side_question, _llm(FAST))
    assert time.monotonic() - start < 3
    assert report.used == "llm" and "side_question: timeout" in report.error
    assert ref is not None and ref.notes == "llm answer"
    late = _abandoned(older)
    assert len(late) == 1, "the overrunning side question keeps running in its own thread"
    gate.set()
    late[0].join(5)
    assert _abandoned(older) == [], "the abandoned thread ends with its call"

    # 2. after the late answer arrived, later refinements never see it
    ref, report = _refine(lambda prompt: json_reply(FRESH), _llm(FAST))
    assert report.used == "side_question" and ref.notes == "fresh answer"
    ref, report = _refine(lambda prompt: None, _llm(None))
    assert (ref, report.used) == (None, "off"), "a late answer must not stand in for a missing one"

    # 3. a late failure: once its thread ended, nothing holds the exception (or its frames) any more
    gate = threading.Event()
    refs: list[weakref.ref] = []

    def late_failure(prompt):
        gate.wait(5)
        exc = LateError("provider answered after the deadline")
        refs.append(weakref.ref(exc))
        raise exc

    ref, report = _refine(late_failure, _llm(FAST))
    assert report.used == "llm" and ref.notes == "llm answer"
    late = _abandoned(older)
    gate.set()
    for thread in late:
        thread.join(5)
    gc.collect()
    assert len(refs) == 1 and refs[0]() is None, "the abandoned call's outcome must not be kept"

    assert _abandoned(older) == []
    assert _module_state() == state_before, "refine must not keep module state across calls"
    for path, before in trees_before.items():
        assert tree_diff(before, tree_state(path)) == {"added": [], "removed": [], "changed": []}, path
    assert os.listdir(cwd) == []
