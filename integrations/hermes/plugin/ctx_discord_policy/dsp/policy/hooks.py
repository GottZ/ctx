"""The policy hooks every armed profile instance registers (intervention A8).

They only read the shared runtime anchor, so a hook fired in a routed profile sees the route that the
Discord ingress set in the launch profile's code. Any failure inside a hook blocks (fail closed).
"""

from __future__ import annotations

import logging
import time
from typing import Any, Optional

from . import lanes
from .routes import Route
from ..runtime import RUNTIME

logger = logging.getLogger("ctx_discord_policy")

BLOCK_PUBLIC = (
    "BLOCKED by ctx_discord_policy: this Discord turn was written by a server member who is not the "
    "operator; only the server's enabled public lanes are available."
)
BLOCK_NO_ROUTE = (
    "BLOCKED by ctx_discord_policy: this session carries public Discord turns and this turn has no exact "
    "authority route; session identity alone cannot authorize tools."
)
BLOCK_INTERNAL = (
    "BLOCKED by ctx_discord_policy: the policy check failed internally, so the tool call is refused."
)


def _block(message: str) -> dict[str, str]:
    return {"action": "block", "message": message}


def _turn_key(session_id: Any, turn_id: Any) -> Optional[tuple[str, str]]:
    sid = str(session_id or "")
    return (sid, str(turn_id or "")) if sid else None


def current_route(session_id: Any = "", turn_id: Any = "") -> Optional[Route]:
    route = RUNTIME.route.get()
    if isinstance(route, Route):
        return route
    key = _turn_key(session_id, turn_id)
    if key is not None and turn_id:
        bound = RUNTIME.turn_routes.get(key)
        if isinstance(bound, Route):
            return bound
    return None


def on_pre_llm_call(*, session_id: str = "", turn_id: str = "", **_: Any) -> Optional[dict[str, str]]:
    """Bind the turn to its exact route and inject the capability lane."""
    try:
        route = RUNTIME.route.get()
        key = _turn_key(session_id, turn_id)
        if not isinstance(route, Route):
            if key is not None and turn_id:
                RUNTIME.turn_routes.pop(key)
            return None
        if key is not None:
            if turn_id:
                RUNTIME.turn_routes.put(key, route)
            if not route.is_operator:
                RUNTIME.public_sessions.put(key[0], time.time())
        return {"context": lanes.capability_lane_text(route)}
    except Exception:
        logger.exception("ctx_discord_policy: pre_llm_call failed")
        # Ending the turn before inference is the closed answer when the lane cannot be established.
        return _block(BLOCK_INTERNAL)


def on_pre_tool_call(*, tool_name: str = "", session_id: str = "", turn_id: str = "", args: Any = None,
                     **_: Any) -> Optional[dict[str, str]]:
    """Operator routes keep their tools; public routes only get catalog tools; no route in a public
    session gets nothing."""
    try:
        route = current_route(session_id, turn_id)
        if route is None:
            if session_id and str(session_id) in RUNTIME.public_sessions:
                return _block(BLOCK_NO_ROUTE)
            return None
        if route.is_operator:
            route.tool_started = True
            return None
        if lanes.public_tool_allowed(route, str(tool_name or ""), args):
            return None
        return _block(BLOCK_PUBLIC)
    except Exception:
        logger.exception("ctx_discord_policy: pre_tool_call failed")
        return _block(BLOCK_INTERNAL)


def on_session_gone(*, session_id: str = "", **_: Any) -> None:
    """Forget per-session bookkeeping when Hermes ends, resets or finalizes a session."""
    try:
        sid = str(session_id or "")
        if not sid:
            return
        RUNTIME.turn_routes.drop_where(lambda key: key[0] == sid)
        RUNTIME.public_sessions.pop(sid)
    except Exception:
        logger.exception("ctx_discord_policy: session cleanup failed")


HOOKS = (
    ("pre_llm_call", on_pre_llm_call),
    ("pre_tool_call", on_pre_tool_call),
    ("on_session_end", on_session_gone),
    ("on_session_reset", on_session_gone),
    ("on_session_finalize", on_session_gone),
)
