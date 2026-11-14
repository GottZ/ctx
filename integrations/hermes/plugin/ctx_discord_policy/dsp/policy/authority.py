"""Turn authorization through exact routes (intervention A7).

A non-allowlisted Discord member is authorized for one turn only when a route exists for exactly that
message: same platform, same chat, same (session) user, same message id, guild still active, and the
target profile still armed. Session identity alone never authorizes anything.

Routes are found in two ways, because Hermes may execute an admitted message later from its durable
FIFO, restoring the event from a serialized envelope without ``raw_message`` and inside a drain task
whose context was copied from whichever message started the drain:
1. ``event.raw_message._dsp_route`` (the routed message the adapter handed to the stock handler);
2. the per-message registry, keyed by ``(bot_id, chat_id, message_id, user_id)`` (``Route.key``): a
   reaction wakeup reuses the reacted-to message's id, so one message can carry routes of several users,
   and two bots of one gateway can admit the same message. The bot is the adapter that answers the source
   (``runner._delivery_adapter_for``); where it cannot be told, a lookup that finds routes of two bots finds
   none (closed).
The runner wrapper sets the route context variable explicitly for every event — to the event's own
route or to ``None`` — so a route can never be inherited by another message's turn.
"""

from __future__ import annotations

from typing import Any, Optional

from .. import config, hermes
from ..routing import check as routing_check  # S12: A4-P
from ..runtime import RUNTIME
from . import hooks
from .routes import PUBLIC, Route, platform_is_discord


def _source_message_id(source: Any, event: Any = None) -> str:
    for holder in (event, source):
        value = getattr(holder, "message_id", None) if holder is not None else None
        if value not in (None, ""):
            return str(value)
    return ""


def bot_for_source(runner: Any, source: Any) -> Optional[str]:
    """The bound bot that answers ``source`` (the adapter Hermes delivers through), ``None`` if unknown."""
    if runner is None or source is None:
        return None
    for name in ("_delivery_adapter_for", "_adapter_for_source"):
        get = getattr(runner, name, None)
        if not callable(get):
            continue
        try:
            adapter = get(source)
        except Exception:
            adapter = None
        binding = RUNTIME.bots.for_adapter(adapter) if adapter is not None else None
        if binding is not None:
            return binding.bot_id
    return None


# Stands for "routes of several bots, and the bot cannot be told": a policy event whose route fails every check,
# so A7 drops it instead of running it unrouted.
AMBIGUOUS = Route(bot_id="", guild_id="", channel_id="", message_id="", author_id="", lane=PUBLIC)


def registered_route(source: Any, message_id: str, bot_id: Optional[str]) -> Any:
    """The registry's route for this source's message (``Route.key``), of ``bot_id`` when known;
    :data:`AMBIGUOUS` when several bots hold one and the bot is not known."""
    found = RUNTIME.routes_for(getattr(source, "chat_id", ""), message_id, getattr(source, "user_id", ""), bot_id)
    if len(found) > 1:
        return AMBIGUOUS
    return found[0] if found else None


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


def candidate_for_event(event: Any, runner: Any = None, *, bot_id: Optional[str] = None) -> Optional[Route]:
    """The route the policy attached to this event's message, valid or not (``None``: not a policy event).
    ``bot_id`` (or the bot ``runner`` delivers the source through) picks that bot's route from the registry."""
    source = getattr(event, "source", None)
    message_id = _source_message_id(source, event)
    raw = getattr(event, "raw_message", None)
    candidate = getattr(raw, "_dsp_route", None) if raw is not None else None
    if not isinstance(candidate, Route) and message_id:
        candidate = registered_route(source, message_id, bot_id or bot_for_source(runner, source))
    return candidate if isinstance(candidate, Route) else None


def _other_bot(route: Route, runner: Any, source: Any) -> bool:
    """The source is answered by a known bot that is not the route's (another bot's route never applies)."""
    bot = bot_for_source(runner, source)
    return bool(bot) and bot != route.bot_id


def route_for_event(event: Any, runner: Any = None) -> Optional[Route]:
    candidate = candidate_for_event(event, runner)
    source = getattr(event, "source", None)
    if candidate is None or _other_bot(candidate, runner, source):
        return None
    if exact(candidate, source, _source_message_id(source, event) or candidate.message_id):
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
    if not route.is_operator and getattr(event, "_fifo_only", False):
        # A turn Hermes made from a command (a skill, /queue, /plan, ...) and queued as text: it passed no
        # command check here, so it never runs for a member.
        return "turns made from gateway commands are reserved for the operator"
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


def event_refusal(adapter: Any, event: Any) -> Optional[str]:
    """Why an event Hermes built from a Discord message must not enter its pipeline at all (intervention A10,
    before ``handle_message`` routes it to a running session, its busy handler or a new turn). ``None``: hand
    it on; the runner wrapper (A7) still checks it. A member's gateway command never reaches Hermes: while
    the member's session runs a turn, Hermes' busy handler would run it (a skill's text inlined) without
    passing A7's command check."""
    source = getattr(event, "source", None)
    if source is None or not platform_is_discord(getattr(source, "platform", None)):
        return None
    binding = RUNTIME.bots.for_adapter(adapter)
    if binding is None:
        return None
    candidate = candidate_for_event(event, bot_id=binding.bot_id)
    if candidate is None:
        return unrouted_refusal([binding], event)
    if not candidate.is_operator and is_command(event):
        return "gateway commands are reserved for the operator"
    return None


def bindings_for(runner: Any, source: Any) -> list:
    """The bound bot that answers ``source``; every bound bot when that cannot be told (closed: a server any
    of them knows counts)."""
    bot = bot_for_source(runner, source)
    binding = RUNTIME.bots.for_bot(bot) if bot else None
    return [binding] if binding is not None else RUNTIME.bots.bindings()


def unrouted_refusal(bindings: list, event: Any) -> Optional[str]:
    """Why an event without a policy route must not run (``None``: Hermes' own rules decide). In a server
    one of these bots knows (active or paused; every server while its policy file cannot be used), only an
    operator's event runs without a route. Internal events of the gateway and other platforms pass."""
    source = getattr(event, "source", None)
    if source is None or getattr(event, "internal", False):
        return None
    if not platform_is_discord(getattr(source, "platform", None)):
        return None
    guild_id = str(getattr(source, "guild_id", "") or "").strip()
    if not guild_id:
        return None                                  # direct messages: Hermes decides
    knowing = [b for b in bindings if b is not None
               and b.store.read().guild_state(b.bot_id, guild_id) in ("active", "paused")]
    if not knowing:
        return None
    user = str(getattr(source, "user_id", "") or "")
    if any(config.is_operator(b.owner_home, user) for b in knowing):
        return None
    return "in a server the policy knows, only an operator's message runs without the policy's route"


def authorizes(source: Any, runner: Any = None) -> bool:
    """True only for the exact route of the message this source belongs to (never without a message id)."""
    message_id = _source_message_id(source)
    if not message_id:
        return False
    bot = bot_for_source(runner, source)
    current = RUNTIME.route.get()
    if isinstance(current, Route) and (not bot or current.bot_id == bot) and exact(current, source, message_id):
        return True
    return exact(registered_route(source, message_id, bot), source, message_id)
