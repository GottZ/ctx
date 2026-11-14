"""Worklog threads: guild settings, lazy creation, and the delivery bookkeeping of intervention K3.

An operator turn that starts a tool gets a worklog thread in the guild's configured worklog channel the
first time the turn delivers something after that tool call (DESIGN §5.2 group "Worklog", §6.2 K3). The
turn's own output then goes into that thread; the origin channel gets a short pointer and, if
``worklog.final_to_origin`` is on, a copy of the final answer as a reply to the triggering message.

Guarantees this module enforces itself (independent of how the caller resolved the route):
* only an operator route whose tool call was admitted (``Route.tool_started``) can create a worklog;
* the worklog channel comes from the configuration of the route's own ``(bot_id, guild_id)`` and is
  refused unless Discord reports it as a channel of that guild;
* a route whose origin already is a worklog thread continues in place (no second thread).

Delivery state is process-wide: this package is loaded once per process as the runtime anchor
(DESIGN §3.1), so the registries below are shared by every profile instance, like ``RUNTIME``.
"""

from __future__ import annotations

import asyncio
import hashlib
import inspect
import logging
import re
import sys
from dataclasses import dataclass, field
from types import SimpleNamespace
from typing import Any, Callable, Iterable, Optional

from ..policy.routes import Route, snowflake
from ..runtime import ROUTE_CAP, RUNTIME, BoundedMap

logger = logging.getLogger("ctx_discord_policy")

# Route.mode a classifier may use for a message written inside an existing worklog thread.
WORKLOG_MODE = "worklog"
# Optional metadata stamp ("<channel_id>:<message_id>") a runner-side metadata wrapper may add to mark a
# delivery as the turn's own output for exactly that route.
ROUTE_METADATA_KEY = "ctx_discord_policy_route"
# Marks the origin copy of a final answer, so a replay of that copy is never routed again.
ORIGIN_COPY_KEY = "ctx_discord_policy_origin_copy"

# Metadata keys of an explicit send (``send_message`` tool: ``thread_id``; ntfy: ``publish_topic``). Hermes'
# own turn deliveries always carry more (guild scope, ``non_conversational``, ``_interim_send``, ``notify``,
# ``reply_to_message_id``) or the reply anchor.
EXPLICIT_SEND_KEYS = frozenset({"thread_id", "publish_topic"})

ARCHIVE_MINUTES = (60, 1440, 4320, 10080)
DEFAULT_ARCHIVE_MINUTES = 1440
REQUEST_PREVIEW_CHARS = 1200
THREAD_NAME_CHARS = 90

ACK_TEXT = "Working on this in <#{thread_id}>."
KICKOFF_HEADER = "\N{HAMMER AND WRENCH}\N{VARIATION SELECTOR-16} **{title}**"


class WorklogRefused(RuntimeError):
    """The worklog cannot be created for this route; delivery stays in the origin channel."""


@dataclass(frozen=True)
class WorklogSettings:
    """The worklog group of one guild's configuration (DESIGN §5.3), validated."""

    channel_id: Optional[str] = None
    archive_minutes: int = DEFAULT_ARCHIVE_MINUTES
    final_to_origin: bool = True
    bot_name: str = ""

    @property
    def enabled(self) -> bool:
        return self.channel_id is not None


def parse_settings(config: Any) -> WorklogSettings:
    """Worklog settings from a guild configuration dict; anything unusable means "no worklog"."""
    if not isinstance(config, dict):
        return WorklogSettings()
    state = config.get("state", "active")
    if state != "active":
        return WorklogSettings()
    group = config.get("worklog")
    if not isinstance(group, dict):
        return WorklogSettings()
    raw_channel = group.get("channel")
    channel = snowflake(raw_channel) if raw_channel is not None and not isinstance(raw_channel, bool) else None
    minutes = group.get("archive_minutes", DEFAULT_ARCHIVE_MINUTES)
    if isinstance(minutes, bool) or minutes not in ARCHIVE_MINUTES:
        minutes = DEFAULT_ARCHIVE_MINUTES
    final = group.get("final_to_origin", True)
    identity = config.get("identity") if isinstance(config.get("identity"), dict) else {}
    return WorklogSettings(channel_id=channel, archive_minutes=int(minutes),
                           final_to_origin=final if isinstance(final, bool) else True,
                           bot_name=_clean_line(str(identity.get("bot_name") or ""), 32))


@dataclass(eq=False)
class WorklogState:
    """Delivery state of one route (one admitted message) on one adapter (one bot)."""

    route: Route
    settings: WorklogSettings
    key: tuple = ()
    thread_id: str = ""
    in_place: bool = False      # the origin already is a worklog thread: deliver there, create nothing
    failed: str = ""            # creation or delivery failed: deliver in the origin for the rest of the turn
    final_text: str = ""        # latest final-marked content delivered into the worklog (copied at turn end)
    lock: asyncio.Lock = field(default_factory=asyncio.Lock, repr=False)
    final_lock: asyncio.Lock = field(default_factory=asyncio.Lock, repr=False)
    final_hashes: set = field(default_factory=set, repr=False)


class Delivery:
    """Process-wide delivery registries (bounded like the route registries of the anchor)."""

    def __init__(self) -> None:
        self.get_guild_config: Optional[Callable[[str, str], Any]] = None
        # state_key(adapter, route) -> WorklogState
        self.states: BoundedMap[tuple[int, str, str], WorklogState] = BoundedMap(ROUTE_CAP)
        # (chat id the caller used, message id) -> (chat the message really lives in, state key)
        self.locations: BoundedMap[tuple[str, str], tuple[str, tuple[int, str, str]]] = BoundedMap(ROUTE_CAP * 8)
        # worklog thread id -> (bot_id, guild_id) for threads this process created
        self.threads: BoundedMap[str, tuple[str, str]] = BoundedMap(ROUTE_CAP)
        self._warned: BoundedMap[tuple[str, ...], bool] = BoundedMap(256)

    def configure(self, get_guild_config: Callable[[str, str], Any]) -> None:
        if not callable(get_guild_config):
            raise TypeError("get_guild_config must be callable as get_guild_config(bot_id, guild_id)")
        self.get_guild_config = get_guild_config

    def warn_once(self, key: tuple[str, ...], message: str, *args: Any) -> None:
        if key in self._warned:
            return
        self._warned.put(key, True)
        logger.warning(message, *args)


DELIVERY = Delivery()


def state_key(adapter: Any, channel_id: Any, message_id: Any) -> tuple[int, str, str]:
    """Delivery state is per adapter instance (= per bot): two bots admitting one message never share it."""
    return (id(adapter), str(channel_id or ""), str(message_id or ""))


def state_for_event(adapter: Any, keys: Iterable[tuple[str, str]]) -> Optional[WorklogState]:
    for channel_id, message_id in keys:
        state = DELIVERY.states.get(state_key(adapter, channel_id, message_id))
        if state is not None:
            return state
    return None


def settings_for(route: Route) -> WorklogSettings:
    getter = DELIVERY.get_guild_config
    if getter is None:
        return WorklogSettings()
    return parse_settings(getter(route.bot_id, route.guild_id))


# ── call binding (wrappers pass *args/**kwargs through and only read named parameters) ──────────────


class CallArgs:
    """One call bound to the original's signature: read and replace named arguments, keep the rest."""

    def __init__(self, signature: inspect.Signature, bound: inspect.BoundArguments) -> None:
        self._sig = signature
        self._bound = bound
        self._var_kw = next((p.name for p in signature.parameters.values()
                             if p.kind is inspect.Parameter.VAR_KEYWORD), None)

    @classmethod
    def bind(cls, signature: Optional[inspect.Signature], args: Iterable[Any],
             kwargs: dict[str, Any]) -> Optional["CallArgs"]:
        if signature is None:
            return None
        try:
            return cls(signature, signature.bind(*args, **kwargs))
        except TypeError:
            return None

    def named(self, name: str) -> bool:
        """``name`` is an explicit parameter of the original (not only reachable through ``**kwargs``)."""
        param = self._sig.parameters.get(name)
        return param is not None and param.kind not in (inspect.Parameter.VAR_POSITIONAL,
                                                        inspect.Parameter.VAR_KEYWORD)

    def get(self, name: str, default: Any = None) -> Any:
        arguments = self._bound.arguments
        if name in arguments and self.named(name):
            return arguments[name]
        if self._var_kw is not None and name in (arguments.get(self._var_kw) or {}):
            return arguments[self._var_kw][name]
        return default

    def set(self, name: str, value: Any) -> None:
        if self.named(name):
            self._bound.arguments[name] = value
        elif self._var_kw is not None:
            self._bound.arguments.setdefault(self._var_kw, {})[name] = value
        else:
            raise KeyError(name)

    def invoke(self, fn: Callable[..., Any], first: Any = None) -> Any:
        args = list(self._bound.args)
        if first is not None and args:
            args[0] = first
        return fn(*args, **self._bound.kwargs)


def beneath(fn: Any, intervention_id: str) -> Any:
    """What our wrapper for ``intervention_id`` wraps (searched along the ``__wrapped__`` chain), else ``fn``."""
    current, seen = fn, 0
    while current is not None and seen < 32:
        mark = getattr(current, "__dsp_wrapper__", None)
        if isinstance(mark, tuple) and len(mark) == 2 and mark[0] == intervention_id:
            return getattr(current, "__wrapped__", fn)
        current = getattr(current, "__wrapped__", None)
        seen += 1
    return fn


def signature_of(fn: Any) -> Optional[inspect.Signature]:
    try:
        return inspect.signature(fn)
    except (TypeError, ValueError):
        return None


# ── route resolution for one delivery call ─────────────────────────────────────────────────────────


def delivery_target(chat_id: Any, metadata: Any) -> str:
    """Where Discord really delivers: ``metadata["thread_id"]`` wins over ``chat_id`` (adapter contract)."""
    if isinstance(metadata, dict) and metadata.get("thread_id"):
        return str(metadata["thread_id"])
    return str(chat_id if chat_id is not None else "")


def _registered(channel_id: str, message_id: str) -> Optional[Route]:
    route = RUNTIME.message_routes.get((channel_id, message_id))
    return route if isinstance(route, Route) else None


def candidate_routes(target: str, reply_to: Any, metadata: Any) -> list[Route]:
    """Every route this delivery could belong to: explicit stamp, the turn's context, the reply anchor."""
    found: list[Route] = []
    marker = metadata.get(ROUTE_METADATA_KEY) if isinstance(metadata, dict) else None
    if isinstance(marker, str) and ":" in marker:
        channel_id, _, message_id = marker.partition(":")
        stamped = _registered(channel_id, message_id)
        if stamped is not None:
            found.append(stamped)
    current = RUNTIME.route.get()
    if isinstance(current, Route) and current not in found:
        found.append(current)
    if reply_to not in (None, "") and target:
        anchored = _registered(target, str(reply_to))
        if anchored is not None and anchored not in found:
            found.append(anchored)
    return found


def route_for_target(routes: Iterable[Route], target: str) -> Optional[Route]:
    """The route whose origin is exactly this delivery target; deliveries elsewhere are never rerouted."""
    return next((r for r in routes if target and r.channel_id == target), None)


def is_turn_output(route: Route, reply_to: Any, metadata: Any) -> bool:
    """The delivery is the turn's own output, not an explicit send (e.g. a ``send_message`` tool call).

    Turn output: the reply anchor of the triggering message (base-adapter final), the route stamp, or
    metadata Hermes builds for the turn (reply scope ``scope_id``, progress ``non_conversational``, interim
    ``_interim_send``, stream ``reply_to_message_id``/``notify``). An explicit send carries no reply anchor
    and at most ``thread_id`` (``tools/send_message_tool.py``).
    """
    if isinstance(metadata, dict) and metadata.get(ORIGIN_COPY_KEY):
        return False
    if reply_to not in (None, "") and str(reply_to) == route.message_id:
        return True
    if not isinstance(metadata, dict):
        return False
    marker = metadata.get(ROUTE_METADATA_KEY)
    if marker is not None:
        return marker == f"{route.channel_id}:{route.message_id}"
    scope = metadata.get("scope_id")
    if scope not in (None, "") and str(scope) != route.guild_id:
        return False
    anchor = metadata.get("reply_to_message_id")
    if anchor not in (None, ""):
        return str(anchor) == route.message_id
    return bool(set(metadata) - EXPLICIT_SEND_KEYS)


async def public_route_hits_worklog(adapter: Any, routes: Iterable[Route], target: str) -> bool:
    """A non-operator route is in play and this delivery would land in the worklog area: a worklog thread
    or the configured worklog channel itself.

    Fail closed: a target that cannot be resolved while the guild has a worklog counts as a hit, and
    exceptions propagate so the caller refuses the delivery.
    """
    for route in routes:
        if route.is_operator:
            continue
        if not target or DELIVERY.threads.get(target) is not None:
            return True
        if route.mode == WORKLOG_MODE and target == route.channel_id:
            return True
        settings = settings_for(route)
        if not settings.enabled:
            continue
        if target == settings.channel_id:
            return True
        channel = await _channel(adapter, target)
        if channel is None:
            return True
        parent = getattr(channel, "parent_id", None)
        if parent is not None and str(parent) == settings.channel_id:
            return True
    return False


def stamp_metadata(metadata: Optional[dict[str, Any]], route: Route) -> dict[str, Any]:
    """Metadata with this route's stamp, for a runner-side metadata wrapper (optional integration)."""
    stamped = dict(metadata or {})
    stamped[ROUTE_METADATA_KEY] = f"{route.channel_id}:{route.message_id}"
    return stamped


# ── Discord helpers ────────────────────────────────────────────────────────────────────────────────


def _client(adapter: Any) -> Any:
    return getattr(adapter, "_client", None)


def _cached_channel(adapter: Any, channel_id: str) -> Any:
    client = _client(adapter)
    getter = getattr(client, "get_channel", None)
    if not callable(getter) or not channel_id:
        return None
    try:
        return getter(int(channel_id))
    except Exception:
        return None


async def _channel(adapter: Any, channel_id: str) -> Any:
    channel = _cached_channel(adapter, channel_id)
    if channel is not None:
        return channel
    fetch = getattr(_client(adapter), "fetch_channel", None)
    if not callable(fetch) or not channel_id:
        return None
    try:
        return await fetch(int(channel_id))
    except Exception:
        return None


def channel_guild_id(channel: Any) -> str:
    guild = getattr(channel, "guild", None)
    value = getattr(guild, "id", None) if guild is not None else getattr(channel, "guild_id", None)
    return str(value) if value is not None else ""


def _discord_module(adapter: Any) -> Any:
    for klass in type(adapter).__mro__:
        module = sys.modules.get(getattr(klass, "__module__", "") or "")
        candidate = getattr(module, "discord", None)
        if getattr(candidate, "AllowedMentions", None) is not None:
            return candidate
    return None


def _no_mentions(adapter: Any) -> Any:
    discord = _discord_module(adapter)
    try:
        return discord.AllowedMentions.none() if discord is not None else None
    except Exception:
        return None


_MASS_MENTION_RE = re.compile(r"@(everyone|here)", re.IGNORECASE)
_ENTITY_MENTION_RE = re.compile(r"<(@[!&]?|#)(\d+)>")


def _neutralize(text: str, *, entities: bool) -> str:
    text = _MASS_MENTION_RE.sub("@\u200b\\1", text)
    if entities:
        text = _ENTITY_MENTION_RE.sub(lambda m: f"<{m.group(1)}\u200b{m.group(2)}>", text)
    return text


def _clean_line(text: str, limit: int) -> str:
    text = re.sub(r"[\x00-\x1f\x7f*_~`|>]", " ", text)
    return re.sub(r"\s+", " ", text).strip()[:limit]


def thread_name(origin_message: Any, route: Route) -> str:
    if origin_message is None:
        return f"work log · {route.message_id}"[:THREAD_NAME_CHARS]
    author = getattr(origin_message, "author", None)
    name = _clean_line(str(getattr(author, "display_name", None) or getattr(author, "name", None) or "operator"), 32)
    content = _ENTITY_MENTION_RE.sub("", str(getattr(origin_message, "content", "") or ""))
    hint = _clean_line(content, 58).strip(" .") or "request"
    return f"{name or 'operator'} · {hint}"[:THREAD_NAME_CHARS]


def kickoff_text(route: Route, origin_message: Any, settings: WorklogSettings, *, entities: bool) -> str:
    request = str(getattr(origin_message, "content", "") or "").strip() if origin_message is not None else ""
    if len(request) > REQUEST_PREVIEW_CHARS:
        request = request[: REQUEST_PREVIEW_CHARS - 1] + "…"
    title = f"{settings.bot_name} work log" if settings.bot_name else "Work log"
    return "\n".join((
        KICKOFF_HEADER.format(title=title),
        f"Source: https://discord.com/channels/{route.guild_id}/{route.channel_id}/{route.message_id}",
        f"Trigger: `{route.mode}` by <@{route.author_id}>",
        "Request: " + (_neutralize(request, entities=entities) or "(attachment or referenced message)"),
    ))


async def _send_quiet(adapter: Any, channel: Any, text: str) -> Any:
    allowed = _no_mentions(adapter)
    if allowed is not None:
        return await channel.send(text, allowed_mentions=allowed)
    return await channel.send(text)


async def _origin_message(origin: Any, route: Route) -> Any:
    fetch = getattr(origin, "fetch_message", None)
    if not callable(fetch):
        return None
    try:
        return await fetch(int(route.message_id))
    except Exception:
        return None


# ── worklog creation ───────────────────────────────────────────────────────────────────────────────


async def is_worklog_thread(adapter: Any, target: str, route: Optional[Route] = None,
                            settings: Optional[WorklogSettings] = None, *, fetch: bool = True) -> bool:
    """``target`` is a worklog thread: one this process created, the route's own worklog-mode origin, or a
    thread whose parent is the configured worklog channel of the route's guild."""
    if not target:
        return False
    if DELIVERY.threads.get(target) is not None:
        return True
    if route is None:
        return False
    if route.mode == WORKLOG_MODE and target == route.channel_id:
        return True
    settings = settings if settings is not None else settings_for(route)
    if not settings.enabled:
        return False
    channel = await _channel(adapter, target) if fetch else _cached_channel(adapter, target)
    parent = getattr(channel, "parent_id", None) if channel is not None else None
    return parent is not None and str(parent) == settings.channel_id


async def create_worklog(adapter: Any, route: Route, settings: WorklogSettings) -> str:
    """Post the kickoff into the guild's worklog channel and open the thread on it; returns the thread id."""
    if not settings.enabled:
        raise WorklogRefused("no worklog channel configured")
    parent = await _channel(adapter, settings.channel_id or "")
    if parent is None:
        raise WorklogRefused(f"worklog channel {settings.channel_id} is not reachable")
    if channel_guild_id(parent) != route.guild_id:
        raise WorklogRefused(f"worklog channel {settings.channel_id} is not a channel of guild {route.guild_id}")
    if not callable(getattr(parent, "send", None)):
        raise WorklogRefused(f"worklog channel {settings.channel_id} does not accept messages")
    origin = await _channel(adapter, route.channel_id)
    origin_message = await _origin_message(origin, route) if origin is not None else None
    entities = _no_mentions(adapter) is None
    kickoff = await _send_quiet(adapter, parent, kickoff_text(route, origin_message, settings, entities=entities))
    name = thread_name(origin_message, route)
    try:
        try:
            thread = await kickoff.create_thread(name=name, auto_archive_duration=settings.archive_minutes)
        except TypeError:
            thread = await kickoff.create_thread(name=name)
        thread_id = snowflake(getattr(thread, "id", None))
        if thread_id is None:
            raise WorklogRefused("Discord returned no thread id")
        guild = channel_guild_id(thread)
        if guild and guild != route.guild_id:
            raise WorklogRefused(f"thread {thread_id} is not in guild {route.guild_id}")
    except Exception:
        delete = getattr(kickoff, "delete", None)
        if callable(delete):
            try:
                await delete()
            except Exception:
                logger.warning("ctx_discord_policy: could not remove the orphaned worklog kickoff", exc_info=True)
        raise
    DELIVERY.threads.put(thread_id, (route.bot_id, route.guild_id))
    logger.info("ctx_discord_policy: worklog thread %s opened in guild %s for message %s", thread_id,
                route.guild_id, route.message_id)
    if origin is not None:
        try:
            await _send_quiet(adapter, origin, ACK_TEXT.format(thread_id=thread_id))
        except Exception:
            logger.warning("ctx_discord_policy: worklog %s exists but the origin pointer failed", thread_id,
                           exc_info=True)
    return thread_id


async def worklog_for(adapter: Any, route: Any) -> Optional[WorklogState]:
    """The worklog this route's turn output goes into, created on first use; ``None`` = origin channel.

    Only an operator route with an admitted tool call ever reaches creation.
    """
    if not isinstance(route, Route) or not route.is_operator or not route.tool_started:
        return None
    key = state_key(adapter, route.channel_id, route.message_id)
    state = DELIVERY.states.get(key)
    if state is None:
        settings = settings_for(route)
        if not settings.enabled:
            return None
        state = WorklogState(route=route, settings=settings, key=key)
        DELIVERY.states.put(key, state)
    if state.failed or state.in_place:
        return None
    if state.thread_id:
        return state
    async with state.lock:
        if state.failed or state.in_place:
            return None
        if state.thread_id:
            return state
        if await is_worklog_thread(adapter, route.channel_id, route, state.settings):
            state.in_place = True
            return None
        try:
            state.thread_id = await create_worklog(adapter, route, state.settings)
        except WorklogRefused as exc:
            state.failed = str(exc)
            DELIVERY.warn_once(("refused", route.bot_id, route.guild_id, state.failed),
                               "ctx_discord_policy: no worklog for guild %s (%s); operator output stays in "
                               "the origin channel", route.guild_id, state.failed)
            return None
        except Exception as exc:
            state.failed = f"{type(exc).__name__}: {exc}"
            logger.warning("ctx_discord_policy: worklog creation failed for message %s in guild %s; output "
                           "stays in the origin channel", route.message_id, route.guild_id, exc_info=True)
            return None
        return state


def remember_delivery(chat_ids: Iterable[str], result: Any, state: WorklogState) -> None:
    """Remember where redirected messages live, so later edits by the caller's chat id reach them."""
    ids: list[str] = []
    message_id = getattr(result, "message_id", None)
    if message_id:
        ids.append(str(message_id))
    raw = getattr(result, "raw_response", None)
    if isinstance(raw, dict):
        ids.extend(str(i) for i in raw.get("message_ids") or [] if i)
    ids.extend(str(i) for i in getattr(result, "continuation_message_ids", ()) or () if i)
    for chat_id in {c for c in chat_ids if c}:
        for mid in ids:
            DELIVERY.locations.put((chat_id, mid), (state.thread_id, state.key))


def located(chat_id: Any, message_id: Any) -> Optional[tuple[str, Optional[WorklogState]]]:
    """``(chat the message really lives in, its route state)`` for a message this layer redirected."""
    entry = DELIVERY.locations.get((str(chat_id or ""), str(message_id or "")))
    if entry is None:
        return None
    return entry[0], DELIVERY.states.get(entry[1])


def message_chat(chat_id: Any, message_id: Any) -> str:
    """The chat a message really lives in (for edits/deletes by other interventions); else ``chat_id``."""
    found = located(chat_id, message_id)
    return found[0] if found is not None else str(chat_id or "")


def note_final(state: WorklogState, content: Any) -> None:
    """Remember the latest final-marked content of this turn (copied to the origin when the turn ends)."""
    text = str(content or "")
    if text.strip():
        state.final_text = text


def succeeded(result: Any) -> bool:
    return bool(getattr(result, "success", False))


def send_result(original: Any, *, success: bool, error: Optional[str] = None) -> Any:
    """A result of the adapter's own ``SendResult`` type (looked up in the original's module)."""
    target = inspect.unwrap(original)
    result_type = getattr(target, "__globals__", {}).get("SendResult")
    if result_type is not None:
        try:
            return result_type(success=success, error=error)
        except Exception:
            pass
    return SimpleNamespace(success=success, message_id=None, error=error, raw_response=None, retryable=False,
                           retry_after=None, continuation_message_ids=(), error_kind=None)


async def copy_final_to_origin(original_send: Any, adapter: Any, state: WorklogState, content: Any) -> Any:
    """The final answer once more as a reply to the triggering message (deduplicated per content)."""
    text = str(content or "")
    if not text.strip():
        return None
    digest = hashlib.sha256(text.encode("utf-8")).hexdigest()
    async with state.final_lock:
        if digest in state.final_hashes:
            return send_result(original_send, success=True)
        result = await original_send(adapter, state.route.channel_id, text, reply_to=state.route.message_id,
                                     metadata={"notify": True, ORIGIN_COPY_KEY: True})
        if succeeded(result):
            state.final_hashes.add(digest)
        return result


__all__ = [
    "ACK_TEXT", "ARCHIVE_MINUTES", "DELIVERY", "ORIGIN_COPY_KEY", "ROUTE_METADATA_KEY", "WORKLOG_MODE",
    "EXPLICIT_SEND_KEYS", "CallArgs", "Delivery", "beneath", "WorklogRefused", "WorklogSettings", "WorklogState", "candidate_routes",
    "channel_guild_id", "copy_final_to_origin", "create_worklog", "delivery_target", "is_turn_output",
    "is_worklog_thread", "kickoff_text", "located", "message_chat", "note_final", "parse_settings",
    "public_route_hits_worklog", "remember_delivery", "state_for_event", "state_key",
    "route_for_target", "send_result", "settings_for", "signature_of", "stamp_metadata", "succeeded",
    "thread_name", "worklog_for",
]
