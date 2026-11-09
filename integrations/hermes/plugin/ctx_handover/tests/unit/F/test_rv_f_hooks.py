"""Review round rv-f, M6 (``review-code/REVIEW.md`` §2, g2 M1): the first hook call of a process built the whole
runtime inside ``pre_llm_call`` (janitor, GC, outbox flush) and a failure there switched the turn veto off for the
life of the process (``HookSet._rt`` latched ``_loaded = True`` in its ``except``).

Now the runtime is built off the hook's thread; a hook call waits at most ``HOOK_BUILD_WAIT_S`` for it, and a failed
build is retried after a growing pause (``HOOK_RETRY_MIN_S`` … ``HOOK_RETRY_MAX_S``), so the veto comes back without
a restart. The module constants are patched with ``raising=False`` so the same tests run against the code before the
fix (red proof)."""

from __future__ import annotations

import itertools
import threading
import time

from ctx_handover.engine import commands
from ctx_handover.engine.commands import HookSet, SideQuestionTokens
from ctx_handover.tests.fakes.surfaces_fake import FakeHookRuntime

SID = "20261109_120000_abcdef"


def _vetoing_runtime() -> FakeHookRuntime:
    rt = FakeHookRuntime()
    rt.vetoed[SID] = {"peer": "laptop"}
    return rt


def _until(predicate, timeout: float = 5.0) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.01)
    return bool(predicate())


def test_rv_f_m6_a_failed_build_is_retried_and_the_veto_comes_back(monkeypatch):
    """★ M6: the factory fails on its first call (e.g. the ledger is locked) and works on the second ⇒ after the retry
    pause the veto is active again; within the pause no second build runs (no I/O per turn).

    Red before the fix: ``assert None == {'action': 'block', …}`` — the failure latched ``_loaded`` and the factory
    was never called again."""
    monkeypatch.setattr(commands, "HOOK_RETRY_MIN_S", 0.2, raising=False)
    monkeypatch.setattr(commands, "HOOK_RETRY_MAX_S", 0.2, raising=False)
    rt = _vetoing_runtime()
    calls: list[float] = []

    def factory():
        calls.append(time.monotonic())
        if len(calls) == 1:
            raise OSError("database is locked")
        return rt

    hooks = HookSet(factory, SideQuestionTokens())
    assert hooks.pre_llm_call(session_id=SID) is None  # this turn passes: no runtime (logged)
    assert hooks.pre_tool_call(session_id=SID) is None
    assert len(calls) == 1, "a second build within the retry pause"
    time.sleep(0.3)
    blocked = hooks.pre_llm_call(session_id=SID)
    assert blocked == {"action": "block", "message": rt.veto_text({"peer": "laptop"})}, blocked
    assert len(calls) == 2
    assert hooks.pre_tool_call(session_id=SID) == blocked
    assert len(calls) == 2, "the runtime is built once it worked"


def test_rv_f_m6_the_retry_pause_grows_and_is_capped(monkeypatch):
    """M6: consecutive failures double the pause up to ``HOOK_RETRY_MAX_S`` (no retry storm on a broken ledger)."""
    monkeypatch.setattr(commands, "HOOK_RETRY_MIN_S", 0.1, raising=False)
    monkeypatch.setattr(commands, "HOOK_RETRY_MAX_S", 0.25, raising=False)
    calls: list[float] = []

    def factory():
        calls.append(time.monotonic())
        raise OSError("disk I/O error")

    hooks = HookSet(factory, SideQuestionTokens())
    deadline = time.monotonic() + 1.6
    while time.monotonic() < deadline:
        assert hooks.pre_llm_call(session_id=SID) is None
        time.sleep(0.01)
    gaps = [b - a for a, b in itertools.pairwise(calls)]
    # doubling 0.1 → 0.2, then capped at 0.25: ≥ 7 attempts in 1.6 s; uncapped (0.4, 0.8 …) gives 5
    assert len(calls) >= 6, gaps
    assert 0.09 <= gaps[0] < 0.3, gaps
    assert 0.19 <= gaps[1] < 0.45, gaps
    assert all(g < 0.45 for g in gaps[2:]), gaps  # capped at HOOK_RETRY_MAX_S (+ scheduling slack)


def test_rv_f_m6_the_hook_does_not_wait_for_a_slow_build(monkeypatch):
    """★ M6: a build that blocks (the janitor's GC or outbox flush on the first call of a process) holds a turn at
    most ``HOOK_BUILD_WAIT_S``; once the build has finished, the next call vetoes. Concurrent hook calls start one
    build, not one each.

    Red before the fix: the hook thread is still blocked after 2 s (``assert not worker.is_alive()``)."""
    monkeypatch.setattr(commands, "HOOK_BUILD_WAIT_S", 0.2, raising=False)
    rt = _vetoing_runtime()
    release = threading.Event()
    built: list[int] = []

    def factory():
        built.append(1)
        release.wait(10)
        return rt

    hooks = HookSet(factory, SideQuestionTokens())
    answers: list[object] = []
    workers = [
        threading.Thread(target=lambda: answers.append(hooks.pre_llm_call(session_id=SID)), daemon=True)
        for _ in range(3)
    ]
    try:
        started = time.monotonic()
        for worker in workers:
            worker.start()
        for worker in workers:
            worker.join(2.0)
        assert not any(w.is_alive() for w in workers), "the hook waited for the whole build"
        assert time.monotonic() - started < 1.5
        assert answers == [None, None, None], answers
        assert built == [1], "one build for concurrent hook calls"
    finally:
        release.set()
    assert _until(lambda: hooks.pre_llm_call(session_id=SID) is not None)
    assert hooks.pre_llm_call(session_id=SID) == {"action": "block", "message": rt.veto_text({"peer": "laptop"})}
    assert built == [1]


def test_rv_f_m6_the_build_runs_off_the_hook_thread():
    """M6: the hook never builds the runtime on its own thread (the janitor, GC and flush must not run on the turn's
    thread); a fast build is still in place for the very first call (no turn passes without need)."""
    rt = _vetoing_runtime()
    threads: list[str] = []

    def factory():
        threads.append(threading.current_thread().name)
        return rt

    hooks = HookSet(factory, SideQuestionTokens())
    assert hooks.pre_llm_call(session_id=SID) == {"action": "block", "message": rt.veto_text({"peer": "laptop"})}
    assert threads and threads[0] != threading.current_thread().name, threads


def test_rv_f_m6_the_hook_path_runs_the_first_janitor_in_the_background(monkeypatch, fake_host):
    """★ M6 (A part wired, rv-a ``ensure_started(..., janitor_background=True)``): when a hook builds the profile
    runtime, the process's first janitor run (recovery, retention, GC, outbox flush) goes to a thread of its own; a
    command keeps the synchronous janitor.

    Red before the fix: ``ensure_started`` was called without ``janitor_background``."""
    import types

    from ctx_handover.engine import jobs
    from ctx_handover.engine.commands import EngineBackend, Plugin
    from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx

    calls: list[dict] = []
    runtime = types.SimpleNamespace(veto=object(), ledger=object())
    monkeypatch.setattr(jobs, "runner_for", lambda ctx, factory=None: None)

    def ensure_started(ctx=None, reporter_factory=None, **kw):
        calls.append(kw)
        return runtime

    monkeypatch.setattr(jobs, "ensure_started", ensure_started)
    ctx = FakeCtx(host=fake_host)
    hooked = EngineBackend(ctx, Plugin(ctx))
    hooked.hook_runtime()
    assert calls == [{"janitor_background": True}], calls
    calls.clear()
    commanded = EngineBackend(ctx, Plugin(ctx))
    commanded._started()
    assert calls in ([{}], [{"janitor_background": False}]), calls
