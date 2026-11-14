"""Turn authorization through exact routes (intervention A7).

A non-allowlisted Discord member is authorized for one turn only when a route exists for exactly that
message: same platform, same chat, same (session) user, same message id, guild still active, and the
target profile still armed. Session identity alone never authorizes anything.

Routes are found in two ways, because Hermes may execute an admitted message later from its durable
FIFO, restoring the event from a serialized envelope without ``raw_message`` and inside a drain task
whose context was copied from whichever message started the drain:
1. ``event.raw_message._dsp_route`` (the routed message the adapter handed to the stock handler);
2. the per-message registry, keyed by ``(chat_id, message_id, user_id)`` of the source (``Route.key``): a
   reaction wakeup reuses the reacted-to message's id, so one message can carry routes of several users.
The runner wrapper sets the route context variable explicitly for every event — to the event's own
route or to ``None`` — so a route can never be inherited by another message's turn.
"""

from __future__ import annotations

from typing import Any, Optional

from .. import config, hermes
from ..routing import check as routing_check  # S12: A4-P
from ..runtime import RUNTIME
from . import hooks
from .routes import Route, platform_is_discord


def _source_message_id(source: Any, event: Any = None) -> str:
    for holder in (event, source):
        value = getattr(holder, "message_id", None) if holder is not None else None
        if value not in (None, ""):
            return str(value)
    return ""


def registry_key(source: Any, message_id: str) -> tuple[str, str, str]:
    """The registry key of the route this source's message would carry (``Route.key``)."""
    return (str(getattr(source, "chat_id", "") or ""), str(message_id or ""), str(getattr(source, "user_id", "") or ""))


def exact(route: Any, source: Any, message_id: str = "") -> bool:
    if not isinstance(route, Route) or source is None:
        return False
    if not platform_is_discord(getattr(source, "platform", None)):
        return False
    if str(getattr(source, "user_id", "") or "") != route.session_user_id:
        return False
    if str(getattr(source, "chat_id", "") or "") != route.channel_id:
        return False
    if message_id and message_id != route.message_id:
        return False
    if not hooks.armed_live(route.target_home_key):
        return False
    binding = RUNTIME.bots.for_bot(route.bot_id)
    if binding is None or binding.store.read().guild_state(route.bot_id, route.guild_id) != "active":
        return False
    if route.is_operator and not config.is_operator(binding.owner_home, route.author_id):
        return False  # operator removed after admission: a queued operator route loses its authority
    return True


def candidate_for_event(event: Any) -> Optional[Route]:
    """The route the policy attached to this event's message, valid or not (``None``: not a policy event)."""
    source = getattr(event, "source", None)
    message_id = _source_message_id(source, event)
    raw = getattr(event, "raw_message", None)
    candidate = getattr(raw, "_dsp_route", None) if raw is not None else None
    if not isinstance(candidate, Route) and message_id:
        candidate = RUNTIME.message_routes.get(registry_key(source, message_id))
    return candidate if isinstance(candidate, Route) else None


def route_for_event(event: Any) -> Optional[Route]:
    candidate = candidate_for_event(event)
    source = getattr(event, "source", None)
    if candidate is not None and exact(candidate, source, _source_message_id(source, event) or candidate.message_id):
        return candidate
    return None


def revoke(route: Route) -> None:
    """Forget a route so no later authorization check can find it again."""
    stored = RUNTIME.message_routes.get(route.key)
    if stored is route:
        RUNTIME.message_routes.pop(route.key)


def event_target_key(runner: Any, event: Any) -> Optional[str]:
    """Second armed check (A4) with the real source, as the runner resolves it."""
    resolver = getattr(runner, "_resolve_profile_home_for_source", None)
    source = getattr(event, "source", None)
    if not callable(resolver) or source is None:
        return None
    try:
        return hermes.home_key(resolver(source))
    except Exception:
        return None


def is_command(event: Any) -> bool:
    """What Hermes dispatches as a gateway command: ``get_command()`` (text such as "/home/x.log" is typed
    COMMAND but not dispatched, so the type alone does not count)."""
    get_command = getattr(event, "get_command", None)
    if callable(get_command):
        try:
            return bool(get_command())
        except Exception:
            return True  # cannot tell: treat as a command (closed)
    kind = getattr(event, "message_type", None)
    return str(getattr(kind, "value", kind) or "").lower() == "command"


def refusal(runner: Any, event: Any, route: Optional[Route], target: Optional[str]) -> Optional[str]:
    """Why a policy-admitted event must not run (``None``: it may run under ``route``)."""
    if route is None:
        return "its route is no longer valid (guild paused, operator removed, or source changed)"
    if target is None or target != route.target_home_key or not hooks.armed_live(target):
        return "the profile it routes to is not armed with ctx_discord_policy"
    closed = routing_check.refusal(runner, event, route, target)  # A4-P (S12/S13): public routes only
    if closed is not None:
        return closed
    if not route.is_operator and is_command(event):
        return "gateway commands are reserved for the operator"
    key_of = getattr(runner, "_session_key_for_source", None)
    if not callable(key_of):
        return "the session key cannot be determined (Hermes changed)"
    try:
        session_key = str(key_of(getattr(event, "source", None)) or "")
    except Exception:
        return "the session key cannot be determined"
    if not session_key:
        return "the session key cannot be determined"
    claimed = RUNTIME.session_lanes.get(session_key)
    if claimed is not None:
        lane, author = claimed
        if lane != route.lane and author != route.session_user_id:
            return (f"Hermes session {session_key} already carries {lane} turns of another author; operator "
                    "and public members never share a session (check group_sessions_per_user / "
                    "thread_sessions_per_user)")
    RUNTIME.session_lanes.put(session_key, (route.lane, route.session_user_id))
    return None


def authorizes(source: Any) -> bool:
    """True only for the exact route of the message this source belongs to (never without a message id)."""
    message_id = _source_message_id(source)
    if not message_id:
        return False
    current = RUNTIME.route.get()
    if exact(current, source, message_id):
        return True
    if message_id:
        stored = RUNTIME.message_routes.get(registry_key(source, message_id))
        if exact(stored, source, message_id):
            return True
    return False
