"""Review round rv-a, composition-root findings in ``engine/jobs.py``: M28 (the continuation child of the default
profile followed the sticky ``active_profile``), L13 (an LLM answer with its JSON inside prose was dropped), L21 (the
janitor's outbox flush ran without the ``mcp_allowlist`` grant) and the target OS of this host's own policy under
MSYS/Cygwin (rv-k0: ``os.name`` is ``posix`` there, the Windows naming rules did not apply)."""

from __future__ import annotations

import platform
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Options
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration.harness import Home

# -- M28 ----------------------------------------------------------------------------------------------------------


@pytest.fixture
def server(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    home = Home(tmp_path, "server")
    yield home
    jobs._reset_for_tests()


def test_rv_a_m28_the_default_profile_is_passed_by_name(server):
    """``hermes`` without ``-p`` follows ``hermes profile use`` (upstream ``hermes_cli/main.py`` reads
    ``active_profile``): the composition root hands the spawner the profile it checked approvals for — also when that
    is ``default`` — never ``None``."""
    with server.active():
        _source, target = jobs.make_local_apis(None, calling_session=None, session_key=None)
        host = target.host
    assert host.profile == "default"
    assert target.continuation._profile == "default"


# -- L13 ----------------------------------------------------------------------------------------------------------


def test_rv_a_l13_json_inside_prose_is_the_answer():
    """A model that wraps its JSON in a sentence (``parsed`` empty, ``text`` holds it) still answers; the first JSON
    object of the text is taken. No object ⇒ no answer; a list is no object."""
    result = SimpleNamespace(parsed=None, text='Here is the result: {"add": [["/x", "why"]], "drop": []} — done.')
    assert jobs.structured_answer(result) == {"add": [["/x", "why"]], "drop": []}
    fenced = SimpleNamespace(parsed=None, text='```json\n{"add": [], "notes": "a } brace"}\n```')
    assert jobs.structured_answer(fenced) == {"add": [], "notes": "a } brace"}
    assert jobs.structured_answer(SimpleNamespace(parsed=None, text="no json here")) is None
    assert jobs.structured_answer(SimpleNamespace(parsed=None, text="[1, 2]")) is None
    assert jobs.structured_answer(SimpleNamespace(parsed={"add": []}, text="ignored")) == {"add": []}


# -- L21 ----------------------------------------------------------------------------------------------------------


def test_rv_a_l21_janitor_flush_waits_for_the_mcp_grant(fake_host, fake_ctx, tmp_path, clock):
    """Before ``/handover setup`` granted the ctx server (``mcp_allowlist``) the janitor's flush sends nothing — and
    counts no failed attempt (no backoff of up to 6 h on rows the user has not consented to yet)."""
    fake_host.config["memory"] = {"provider": "ctx_checkpoint"}
    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    ledger.outbox_put("departure", {"title": "t", "job_id": "01JZJ000000000000000000000"})
    flush = jobs._flush(fake_ctx)
    assert flush is not None
    flush(ledger)
    assert fake_ctx.mcp_calls == []
    assert [r["attempts"] for r in ledger.outbox_due(clock() + 1, 10)] == [0]
    fake_host.config.setdefault("plugins", {}).setdefault("entries", {}).setdefault("ctx_handover", {})[
        "mcp_allowlist"
    ] = ["ctx"]
    jobs._flush(fake_ctx)(ledger)
    assert fake_ctx.mcp_calls != []
    ledger.close()


# -- rv-k0: the target OS of this host's policy ---------------------------------------------------------------------


@pytest.mark.parametrize(
    ("plat", "system", "want"),
    [
        ("msys", "MSYS_NT-10.0-19045", "windows"),
        ("cygwin", "CYGWIN_NT-10.0-19045", "windows"),
        ("win32", "Windows", "windows"),
        ("darwin", "Darwin", "macos"),
        ("linux", "Linux", "linux"),
    ],
)
def test_rv_a_policy_target_os_follows_the_facts_rule(monkeypatch, tmp_path, plat, system, want):
    """``os.name`` is ``posix`` under MSYS and Cygwin, so the Windows denylist and naming rules did not apply to
    writes on this host; the policy now takes the OS family the facts report (``envfacts``) and folds case like it."""
    monkeypatch.setattr(sys, "platform", plat)
    monkeypatch.setattr(platform, "system", lambda: system)
    monkeypatch.setattr(jobs.hostcompat, "effective_home", lambda: tmp_path / "home")
    monkeypatch.setattr(jobs.hostcompat, "hermes_root", lambda: tmp_path / "hh")
    hermes = SimpleNamespace(home=str(tmp_path / "hh"), document_cache=str(tmp_path / "hh" / "cache"))
    policy = jobs._policy(Options(), SimpleNamespace(data_dir=Path(tmp_path / "hh" / "pd")), hermes)
    assert policy.target_os == want
    assert policy.case_insensitive is (want in ("windows", "macos"))


# -- M6 / M29 (A parts of rv-f's findings): no janitor or ctx flush on the caller's critical path -------------------


@pytest.fixture
def runtime_env(fake_host):
    jobs._reset_for_tests()
    yield fake_host
    jobs._reset_for_tests()


def test_rv_a_m6_first_hook_does_not_wait_for_the_janitor(runtime_env, fake_ctx, monkeypatch):
    """``ensure_started(..., janitor_background=True)`` (the hook path) returns with ``veto``, identity and store in
    place while the one-time janitor (recovery, retention, GC, outbox flush) still runs — in a daemon thread
    ``ctxh-janitor``."""
    import threading

    started, release = threading.Event(), threading.Event()

    def slow_janitor(self, *, background: bool = True) -> dict:
        started.set()
        release.wait(10)
        return {}

    monkeypatch.setattr(jobs.Runtime, "janitor", slow_janitor)
    rt = jobs.ensure_started(fake_ctx, janitor_background=True)
    try:
        assert rt.veto is not None and rt.store is not None
        assert started.wait(5)
        assert any(t.name == "ctxh-janitor" and t.daemon for t in threading.enumerate())
    finally:
        release.set()


def test_rv_a_m6_the_janitor_runs_once_also_in_the_background(runtime_env, fake_ctx, monkeypatch):
    import threading

    calls: list[int] = []
    done = threading.Event()

    def counting(self, *, background: bool = True) -> dict:
        calls.append(1)
        done.set()
        return {}

    monkeypatch.setattr(jobs.Runtime, "janitor", counting)
    jobs.ensure_started(fake_ctx, janitor_background=True)
    assert done.wait(5)
    jobs.ensure_started(fake_ctx, janitor_background=True)
    jobs.ensure_started(fake_ctx)
    assert calls == [1]


def test_rv_a_m29_job_start_does_not_wait_for_the_ctx_outbox(runtime_env, fake_ctx, monkeypatch):
    """Before every job the janitor runs (``before_job``); its ctx outbox flush (``call_mcp`` per row, up to 10 s
    each) must not hold the job: ctx is never on the critical path (D12, DESIGN §13)."""
    import threading
    import time as _time

    runtime_env.config["memory"] = {"provider": "ctx_checkpoint"}
    runtime_env.config.setdefault("plugins", {}).setdefault("entries", {}).setdefault("ctx_handover", {})[
        "mcp_allowlist"
    ] = ["ctx"]
    hang, called = threading.Event(), threading.Event()

    def hanging_call(server, tool, arguments=None, timeout=30):
        called.set()
        hang.wait(10)
        return {"ok": True}

    fake_ctx.call_mcp = hanging_call
    rt = jobs.ensure_started(fake_ctx)
    rt.ledger.outbox_put("departure", {"title": "t", "job_id": "01JZJ000000000000000000000"})
    t0 = _time.monotonic()
    try:
        rt.runner._before_job()
        took = _time.monotonic() - t0
        assert took < 2.0, f"job start waited {took:.1f} s for the ctx outbox"
        assert called.wait(5), "the outbox is still flushed — in the background"
    finally:
        hang.set()
