"""Shared fixtures and helpers of the lock/veto suites (no tests here).

Lineages are built through the core's own write paths (``SessionBuilder``: ``append_messages_batch``,
``publish_compression_child``); goal/heartbeat/loop state is the core's own serialisation
(``GoalState.to_json`` etc., stored with ``set_meta`` exactly as ``save_goal`` does — the save helpers
themselves go through an asynchronously bootstrapped cached ``SessionDB`` that a unit test cannot pin).
Vetoed turns run through the real agent loop (``run_agent.AIAgent.run_conversation`` with the
``pre_llm_call`` hook dispatch patched to the plugin's veto callback), the recipe of
``upstream/tests/agent/test_pre_llm_block_hook.py``.
"""

from __future__ import annotations

import json
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import ClassVar
from unittest.mock import patch

import pytest

from ctx_handover.engine import lock, session_export
from ctx_handover.engine.model import CommitAck, SessionRef
from ctx_handover.tests.unit.B.support import LedgerStub

JOB = "01JTESTJOB0000000000ABCDEF"
PEER = "laptop"


@pytest.fixture
def ledger(tmp_path):
    led = LedgerStub(tmp_path / "ledger.sqlite3")
    try:
        yield led
    finally:
        led.close()


def describe(db, sid: str):
    return session_export.describe(db, SessionRef(id=sid), calling_session=None)


def seed_drivers(db, tip: str) -> None:
    """goal/heartbeat/loop on ``tip`` in the core's own JSON (``GoalState``/``HeartbeatState``/``LoopState``)."""
    from hermes_cli.goals import GoalState
    from hermes_cli.heartbeat import HeartbeatState
    from hermes_cli.loops import LoopState

    db.set_meta(f"goal:{tip}", GoalState(goal="ship the release", created_at=1.0).to_json())
    db.set_meta(f"heartbeat:{tip}", HeartbeatState(prompt="check CI", interval_seconds=600, created_at=1.0).to_json())
    db.set_meta(
        f"loop:{tip}",
        LoopState(prompt="poll", interval_seconds=300.0, created_at=1.0, route={"platform": "cli"}).to_json(),
    )


def build(make_session, *, title: str = "Work", drivers: bool = True, cwd: str | None = None) -> str:
    """Two-segment lineage (rotation through the compressor's publish path); returns the tip id."""
    b = make_session
    root = b.create(title=title, cwd=cwd)
    b.turns(root, 3, with_tools=True)
    tip = b.rotate(root, keep_last=2)
    b.turns(tip, 2)
    if drivers:
        seed_drivers(b.db, tip)
    return tip


def driver(db, key: str, tip: str) -> dict | None:
    raw = db.get_meta(f"{key}:{tip}")
    return json.loads(raw) if raw else None


def now() -> datetime:
    return datetime.now(timezone.utc)


def ack(title: str = "Work", committed_at: float = 1_762_700_000.0) -> CommitAck:
    return CommitAck(
        session_id="20261109_120000_abcdef",
        root_id="20261109_120000_000000",
        title=title,
        dests=(),
        path_map=(),
        model="test-model",
        offline_capable=False,
        warnings=(),
        committed_at=committed_at,
    )


def locked(db, ledger, sid: str, job: str = JOB):
    info = describe(db, sid)
    holder = lock.acquire(db, ledger, info, job, ttl_s=120, wait_s=0, cancelled=lambda: False)
    return info, holder


def prepared(db, ledger, sid: str, job: str = JOB):
    info, holder = locked(db, ledger, sid, job)
    lock.prepare(db, ledger, info, job, PEER, now())
    return info, holder


class _Provider(BaseHTTPRequestHandler):
    chat_requests: ClassVar[list] = []

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0) or 0))
        try:
            req = json.loads(body.decode() or "{}")
        except ValueError:
            req = {}
        if isinstance(req, dict) and "messages" in req:
            type(self).chat_requests.append(req)
        self.send_response(500)
        self.end_headers()

    def log_message(self, *a, **kw):
        pass


@pytest.fixture
def provider_url():
    _Provider.chat_requests = []
    srv = HTTPServer(("127.0.0.1", 0), _Provider)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        yield f"http://127.0.0.1:{srv.server_address[1]}/v1"
    finally:
        srv.shutdown()


def chat_requests() -> list:
    return list(_Provider.chat_requests)


def veto_callback(index: lock.VetoIndex):
    """The plugin's ``pre_llm_call`` answer (what cluster F registers): block when the index knows the lineage."""

    def hook(name, **kw):
        if name != "pre_llm_call":
            return []
        entry = index.lookup(kw.get("session_id"), kw.get("parent_session_id") or None)
        return [{"action": "block", "message": lock.veto_texts(entry)}] if entry else []

    return hook


def vetoed_turn(db, sid: str, provider_url: str, index: lock.VetoIndex, typed: str) -> dict:
    """One real agent turn on ``sid`` with the veto callback as the ``pre_llm_call`` dispatcher."""
    from run_agent import AIAgent

    agent = AIAgent(
        api_key="test-key",
        base_url=provider_url,
        provider="openai-compat",
        model="test-model",
        max_iterations=2,
        enabled_toolsets=[],
        quiet_mode=True,
        skip_context_files=True,
        skip_memory=True,
        save_trajectories=False,
        platform="cli",
        session_db=db,
        session_id=sid,
    )
    history = db.get_messages_as_conversation(sid)
    with patch("hermes_cli.plugins.invoke_hook", side_effect=veto_callback(index)):
        return agent.run_conversation(typed, conversation_history=history, task_id="t")


def index_for(tmp_path, ledger_path) -> lock.VetoIndex:
    index = lock.VetoIndex(ledger_path, poll_s=0.05, ledger_factory=LedgerStub)
    index.refresh_now(force=True)
    return index
