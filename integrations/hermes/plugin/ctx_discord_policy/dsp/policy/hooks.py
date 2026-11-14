"""The policy hooks every armed profile instance registers (intervention A8).

They only read the shared runtime anchor, so a hook fired in a routed profile sees the route that the
Discord ingress set in the launch profile's code. Any failure inside a hook blocks (fail closed).
"""

from __future__ import annotations

import hashlib
import logging
import time
from typing import Any, Optional

from . import lanes
from .routes import Route
from ..adapt.status import SECURITY, Outcome, Result
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
    """Bind the turn to its exact route and inject the assembled turn context (``wiring.turn_context``)."""
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
        from .. import wiring  # late: wiring imports the prompt engine and the store

        return {"context": wiring.turn_context(route)}
    except Exception:
        logger.exception("ctx_discord_policy: pre_llm_call failed")
        # Ends the turn before inference where Hermes honours a pre_llm_call block (v0.23.0+); older
        # Hermes ignores it, and the tool gate in pre_tool_call still applies.
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


def on_turn_end(*, session_id: str = "", turn_id: str = "", **_: Any) -> None:
    """``on_session_end`` fires after every turn (with its ``turn_id``): forget only that turn's binding.
    The session keeps its public marker; the next turn without a route must still get no tools."""
    try:
        if session_id and turn_id:
            RUNTIME.turn_routes.pop((str(session_id), str(turn_id)))
    except Exception:
        logger.exception("ctx_discord_policy: turn cleanup failed")


def on_session_gone(*, session_id: str = "", **_: Any) -> None:
    """``on_session_finalize``/``on_session_reset``: forget the session's turn bindings. The public marker
    stays (bounded LRU): a reset session id that carried public turns keeps refusing route-less tools."""
    try:
        sid = str(session_id or "")
        if sid:
            RUNTIME.turn_routes.drop_where(lambda key: key[0] == sid)
    except Exception:
        logger.exception("ctx_discord_policy: session cleanup failed")


def homeless_key(instance: Any) -> str:
    """Status key of an armed entry without a home path: one per home, never another profile's ``A8[…]``."""
    digest = hashlib.sha256(str(instance.home_key).encode("utf-8")).hexdigest()[:8]
    return f"A8[{instance.profile}#{digest}]"


def _note_homeless(instance: Any) -> None:
    key = homeless_key(instance)
    current = RUNTIME.status.get(key)
    if current is not None and current.outcome is Outcome.OFF:
        return  # reported once (the status register logs the warning)
    RUNTIME.status.record(Result(key, SECURITY, Outcome.OFF,
                                 f"armed entry of profile {instance.profile} has no home path, so its live hook "
                                 "registry cannot be checked: public admission into it stays closed",
                                 "armed register"))


def armed_live(home_key: str) -> bool:
    """Armed register AND the live hook registry of that profile agree (A8 behaviour check).

    A profile whose plugin load timed out may have passed ``register()`` without any hook landing in
    Hermes' registry; only a registry that really holds our ``pre_tool_call`` counts."""
    instance = RUNTIME.armed.instance(home_key)
    if instance is None:
        return False
    if not instance.home:
        # Every 3.0 registration records its home; an entry without one is an unknown state: closed.
        _note_homeless(instance)
        return False
    try:
        from .. import hermes

        live = hermes.hook_registered(instance.home, "pre_tool_call", on_pre_tool_call)
    except Exception:
        logger.exception("ctx_discord_policy: live hook check failed; treating profile as not armed")
        return False
    return live is not False


HOOKS = (
    ("pre_llm_call", on_pre_llm_call),
    ("pre_tool_call", on_pre_tool_call),
    ("on_session_end", on_turn_end),
    ("on_session_reset", on_session_gone),
    ("on_session_finalize", on_session_gone),
)
