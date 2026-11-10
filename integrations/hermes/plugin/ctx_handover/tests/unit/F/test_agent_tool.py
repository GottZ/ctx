"""Cluster F — agent tool ``handover`` (DESIGN §14.3, §8.4, SEC 5.6; TESTPLAN §2.7)."""

from __future__ import annotations

import json
import threading
import time

from ctx_handover.engine import agent_tool
from ctx_handover.engine.commands import Plugin
from ctx_handover.engine.model import Mode, SessionRef
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer

SID = "20261109_101500_aaaaaa"

DESIGN_14_3 = {
    "name": "handover",
    "description": "Hand this session and its working files to another Hermes gateway the user paired (see "
    "/handover). action=plan shows what would move; action=suggest records resources the continued work needs that "
    "a file scan cannot see (paths outside the project, other repos, tools, NAMES of environment variables — never "
    "secrets); action=request asks to hand over to a peer after this turn ends (only when the user asked for it); "
    "action=status and action=peers are read-only.",
    "parameters": {
        "type": "object",
        "additionalProperties": False,
        "required": ["action"],
        "properties": {
            "action": {"type": "string", "enum": ["plan", "suggest", "request", "status", "peers"]},
            "peer": {"type": "string", "maxLength": 64},
            "resources": {
                "type": "array",
                "maxItems": 64,
                "items": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["path"],
                    "properties": {
                        "path": {"type": "string", "maxLength": 4096},
                        "reason": {"type": "string", "maxLength": 300},
                    },
                },
            },
            "exclude": {"type": "array", "maxItems": 64, "items": {"type": "string", "maxLength": 4096}},
            "tools_needed": {"type": "array", "maxItems": 32, "items": {"type": "string", "maxLength": 64}},
            "env_vars_needed": {"type": "array", "maxItems": 32, "items": {"type": "string", "maxLength": 128}},
            "note": {"type": "string", "maxLength": 2000},
        },
    },
}


def _tool(backend: FakeBackend | None = None):
    backend = backend or FakeBackend(peers=[make_peer("server")])
    ctx = FakeCtx()
    plugin = Plugin(ctx, backend_factory=lambda c, p: backend)
    return agent_tool.handler(ctx, provider=lambda: plugin), backend, ctx


def _call(handle, args: dict, sid: str = SID) -> dict:
    return json.loads(handle(args, session_id=sid, task_id="t", user_task="x"))


def test_agent_tool_schema_valid():
    """The schema is DESIGN §14.3 verbatim, JSON-serialisable, and the plugin's own validator enforces it (no
    ``jsonschema`` in the managed runtime, LAB L7): unknown keys, wrong enums, oversize lists and anything that is
    not a NAME (``KEY=value``, lower-case, shell) are rejected."""
    assert agent_tool.TOOL_SCHEMA == DESIGN_14_3
    json.dumps(agent_tool.TOOL_SCHEMA)
    handle, _backend, _ = _tool()
    bad = [
        {},
        {"action": "explode"},
        {"action": "plan", "extra": 1},
        {"action": "suggest", "env_vars_needed": ["OPENAI_API_KEY=sk-123"]},
        {"action": "suggest", "env_vars_needed": ["lower"]},
        {"action": "suggest", "tools_needed": ["rm -rf /"]},
        {"action": "suggest", "resources": [{"path": "/x", "secret": "y"}]},
        {"action": "suggest", "resources": [{"path": "a\x00b"}]},
        {"action": "suggest", "resources": [{"path": "/x"}] * 65},
        {"action": "suggest", "note": "n" * 2001},
        {"action": "request", "peer": "../evil"},
    ]
    for args in bad:
        reply = _call(handle, args)
        assert reply["ok"] is False, args
    assert _call(handle, {"action": "peers"}, sid="")["ok"] is False


def test_agent_tool_suggest_records():
    """``suggest`` stores one suggestions row for the session (resources, exclusions, tools, NAMES, note)."""
    handle, backend, _ = _tool()
    reply = _call(
        handle,
        {
            "action": "suggest",
            "resources": [{"path": "~/notes/plan.md", "reason": "design notes"}],
            "exclude": ["build/"],
            "tools_needed": ["node", "pnpm"],
            "env_vars_needed": ["DATABASE_URL"],
            "note": "run pnpm test first",
        },
    )
    assert reply["ok"] is True and reply["recorded"] is True
    assert "only proposed" in reply["message"]
    assert backend.suggestions == [
        (
            SID,
            {
                "resources": [{"path": "~/notes/plan.md", "reason": "design notes"}],
                "exclude": ["build/"],
                "tools_needed": ["node", "pnpm"],
                "env_vars_needed": ["DATABASE_URL"],
                "note": "run pnpm test first",
            },
        )
    ]


def test_agent_tool_plan_is_dry_run_without_lock():
    handle, backend, _ = _tool()
    reply = _call(handle, {"action": "plan", "peer": "server"})
    start = backend.last("start_job")
    assert start["mode"] is Mode.PUSH and start["options"].dry_run is True and start["options"].refine == "off"
    assert start["session_ref"] == SessionRef(id=SID) and start["confirm"] is None
    assert reply["ok"] is True and reply["locked"] is False and reply["plan"]["items_count"] == 1
    assert "continue_async" not in backend.names()
    reply = _call(handle, {"action": "plan"})
    assert backend.last("start_job")["mode"] is Mode.EXPORT and reply["ok"] is True


def test_agent_tool_request_respects_allow_agent_start():
    """Default: nothing starts, the agent is told to ask the user. With ``allow_agent_start`` a worker starts
    ``to <peer> --wait`` without a person: (a) proposed items are dropped, any other confirmation cancels.
    Messaging-platform sessions cannot request at all (R24)."""
    handle, backend, _ = _tool()
    backend.sources[SID] = "local"
    reply = _call(handle, {"action": "request", "peer": "server"})
    assert reply == {
        "ok": True,
        "started": False,
        "message": "Recorded. The handover does not start by itself (allow_agent_start is off). Ask the user to run "
        "/handover to server.",
    }
    assert "start_job" not in backend.names()

    backend.sources["tg"] = "telegram"
    assert _call(handle, {"action": "request", "peer": "server"}, sid="tg")["ok"] is False

    backend.config_data = {"plugins": {"entries": {"ctx_handover": {"settings": {"allow_agent_start": True}}}}}
    backend.confirm_reasons = ["a"]
    reply = _call(handle, {"action": "request", "peer": "server"})
    assert reply["started"] is True
    deadline = time.monotonic() + 5
    while "continue_async" not in backend.names() and time.monotonic() < deadline:
        time.sleep(0.01)
    start = backend.last("start_job")
    assert start["options"].wait is True and start["options"].yes is False and start["mode"] is Mode.PUSH
    assert backend.confirm_answers == ["no_proposed"]
    assert "continue_async" in backend.names()

    backend.confirm_reasons = ["b"]
    before = backend.names().count("continue_async")
    _call(handle, {"action": "request", "peer": "server"})
    deadline = time.monotonic() + 5
    while len(backend.confirm_answers) < 2 and time.monotonic() < deadline:
        time.sleep(0.01)
    for t in threading.enumerate():
        if t.name == "ctx_handover-agent-request":
            t.join(timeout=5)
    assert backend.confirm_answers[-1] == "cancel"
    assert backend.names().count("continue_async") == before


def test_agent_tool_status_and_peers():
    backend = FakeBackend(peers=[make_peer("server"), make_peer("laptop", direction="inbound")])
    backend.jobs = [
        {"job_id": "J1", "mode": "push", "state": "packing", "peer": "server", "calling_session": SID},
        {"job_id": "J2", "mode": "pull", "state": "done", "peer": "server", "calling_session": "x"},
    ]
    handle, backend, _ = _tool(backend)
    peers = _call(handle, {"action": "peers"})["peers"]
    assert peers == [
        {"name": "server", "direction": "dial", "rights": ["pull", "push"]},
        {"name": "laptop", "direction": "inbound", "rights": ["pull", "push"]},
    ]
    status = _call(handle, {"action": "status"})
    assert [j["job_id"] for j in status["jobs"]] == ["J1"]


def test_agent_tool_never_raises():
    backend = FakeBackend(peers=[make_peer("server")])

    def boom(*a, **k):
        raise RuntimeError("ledger locked")

    backend.suggestion_put = boom  # type: ignore[method-assign]
    handle, _b, _ = _tool(backend)
    reply = _call(handle, {"action": "suggest", "resources": [{"path": "/x"}]})
    assert reply["ok"] is False
    assert json.loads(agent_tool.handler(FakeCtx())({"action": "peers"}, session_id=SID))["ok"] is False
