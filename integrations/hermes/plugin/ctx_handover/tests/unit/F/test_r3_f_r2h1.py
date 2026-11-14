"""Review round 3, R2-H1, F part (integrator, from a review probe): the first
use of a process by a slash/CLI verb builds the profile runtime under ``EngineBackend._part``'s lock, with the
janitor in the foreground. The hooks' build thread (``hook_runtime`` → ``_started(janitor_background=True)`` →
``_part``) waited on that lock; after ``HOOK_BUILD_WAIT_S`` the hook answered ``None`` and the first turn on a
handed-over session went through unrefused — two working copies (README lock guarantee (5)).

Now ``hook_runtime`` takes the profile runtime from ``jobs.ensure_started`` directly (idempotent, the same runtime
— X19 — and it returns once the veto index has read the ledger, without waiting for a janitor that runs elsewhere)."""

from __future__ import annotations

import threading
import time

import pytest

import ctx_handover
from ctx_handover.engine import commands, jobs
from ctx_handover.engine.model import SessionInfo, SourceState
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx

SID = "20261109_120000_aaaaaa"
JOB = "01JZR3APROBEPARTLOCK000000"


@pytest.fixture
def handed_over(fake_host):
    """A retired source session in the profile's ledger, then a fresh process (no runtime yet)."""
    jobs._reset_for_tests()
    rt = jobs.ensure_started(FakeCtx(host=fake_host))
    info = SessionInfo(
        tip_id=SID,
        root_id=SID,
        lineage_ids=(SID,),
        title="W",
        model=None,
        provider=None,
        cwd=None,
        git_repo_root=None,
        git_branch=None,
        active_messages=2,
        revision=((SID,), 2, 2, 0),
    )
    rt.ledger.source_lock(JOB, info, "pid=1:x", "laptop")
    rt.ledger.source_set(JOB, SourceState.PREPARED)
    rt.ledger.veto_add(JOB, [SID], SID)
    rt.ledger.source_set(JOB, SourceState.RETIRED, peer="laptop")
    jobs._reset_for_tests()
    yield fake_host
    jobs._reset_for_tests()


def test_r3_f_r2h1_first_hook_is_refused_while_a_verb_runs_the_janitor(handed_over, monkeypatch):
    """★ A verb's first use runs a slow janitor in the foreground; the first ``pre_llm_call`` on the handed-over
    session meanwhile is refused within a second.

    Red before the fix: ``first hook result None`` after ``HOOK_BUILD_WAIT_S`` (5.00 s) — the build thread waited for
    ``EngineBackend._part``'s lock that the verb held through its janitor."""
    release, started = threading.Event(), threading.Event()

    def slow_janitor(self, *, background=True):
        started.set()
        release.wait(30)
        return {}

    monkeypatch.setattr(jobs.Runtime, "janitor", slow_janitor)
    ctx = FakeCtx(host=handed_over)
    ctx_handover.register(ctx)
    backend = commands.plugin_for(ctx).backend
    verb = threading.Thread(target=backend._ledger, daemon=True)  # a slash verb's first use
    verb.start()
    try:
        assert started.wait(10)
        t0 = time.monotonic()
        first = ctx.hooks["pre_llm_call"][0](session_id=SID)
        waited = time.monotonic() - t0
    finally:
        release.set()
        verb.join(30)
    assert first is not None, f"first hook result None after {waited:.2f}s"
    assert waited < 1.0, waited
    assert first.get("action") == "block" and "handed over to laptop" in first.get("message", ""), first


def test_r3_f_r2h1_hook_and_verbs_share_one_runtime(handed_over):
    """X19: the runtime the hook builds directly is the one the verbs use (``ensure_started`` is idempotent)."""
    ctx = FakeCtx(host=handed_over)
    ctx_handover.register(ctx)
    backend = commands.plugin_for(ctx).backend
    hook_rt = backend.hook_runtime()
    assert hook_rt is not None
    assert backend._started() is jobs.ensure_started(ctx)
    assert backend._ledger() is jobs.ensure_started(ctx).ledger
