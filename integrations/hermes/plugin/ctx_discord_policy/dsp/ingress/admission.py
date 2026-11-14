"""Discord ingress: which messages the policy admits, under which authority (interventions A5/A6).

``decide()`` is the single gate. It returns ``None`` — stock Hermes behaviour, nothing granted — unless
every condition holds: the adapter's bot is bound, the guild is ``active`` in a readable store, the channel
is not excluded, the message addresses the bot (or the channel is ambient), the security interventions are
in place, and the profile the message routes to is armed. The lane is decided here, once, from the
operator list in the Hermes configuration.

Admission never opens ``DISCORD_ALLOWED_USERS``: the author is added to the adapter's allowlist only for
the duration of the synchronous stock admission call, and the turn is later authorized only through the
exact per-message route (``policy.authority``).
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Optional

from .. import config, hermes
from ..adapt.status import SECURITY, Outcome, Result
from ..policy import hooks
from ..policy.lanes import PublicLanes
from ..policy.routes import AMBIENT, DIRECT, OPERATOR, PUBLIC, Route, snowflake
from ..runtime import RUNTIME
from . import classify

logger = logging.getLogger("ctx_discord_policy")

REQUIRED_SECURITY = ("A5", "A6", "A7")
_last_unarmed_note: dict[str, float] = {}
_last_closed_note: dict[str, float] = {}


@dataclass
class Decision:
    bot_id: str
    owner_home: Any
    guild_id: str
    channel_id: str
    author_id: str
    lane: str
    mode: str
    lanes: PublicLanes
    target_home_key: str
    reply_unverified: bool = False
    addressed: bool = True     # mention/reply/ambient: admission may widen the allowlist for the author
    excluded: bool = False     # "never respond in" channel: nobody is admitted or routed
    in_thread: bool = False
    server_name: str = ""
    channel_name: str = ""
    bot_display_name: str = ""

    def route(self, message_id: str) -> Route:
        return Route(bot_id=self.bot_id, guild_id=self.guild_id, channel_id=self.channel_id,
                     message_id=message_id, author_id=self.author_id, lane=self.lane, mode=self.mode,
                     target_home_key=self.target_home_key, lanes=self.lanes, server_name=self.server_name,
                     channel_name=self.channel_name, bot_display_name=self.bot_display_name)


def _note_unarmed(target_key: str) -> None:
    now = time.monotonic()
    last = _last_unarmed_note.get(target_key)
    if last is not None and now - last < 300:  # monotonic() may be below 300 shortly after boot
        return
    _last_unarmed_note[target_key] = now
    RUNTIME.status.record(Result(
        "A4", SECURITY, Outcome.FALLBACK,
        f"messages route to profile home {target_key}, where ctx_discord_policy is not enabled: public "
        "admission there stays closed. Enable the plugin in that profile and restart.",
        "armed register"))


def _note_closed(key: str, reason: str) -> None:
    now = time.monotonic()
    last = _last_closed_note.get(key)
    if last is not None and now - last < 300:
        return
    _last_closed_note[key] = now
    RUNTIME.status.record(Result("A4", SECURITY, Outcome.FALLBACK, reason, "session/route probe"))


def sessions_shared(adapter: Any, *, guild_id: str, channel_id: str) -> bool:
    """Behaviour probe: would Hermes put two different members of this channel into ONE session?

    Then text batching, busy-input steering and approval replies of Hermes mix authors before any wrapper
    sees them, so public admission must stay closed there. Unknown means shared (closed)."""
    runner = getattr(adapter, "gateway_runner", None)
    key_of = getattr(runner, "_session_key_for_source", None)
    build = getattr(adapter, "build_source", None)
    if not callable(key_of) or not callable(build):
        return True
    try:
        keys = {key_of(build(chat_id=channel_id, chat_type="group", user_id=probe, guild_id=guild_id))
                for probe in ("dsp-session-probe-a", "dsp-session-probe-b")}
    except Exception:
        return True
    return len(keys) != 2


def target_home_key(adapter: Any, message: Any, *, guild_id: str, channel_id: str, author_id: str,
                    thread_id: Optional[str], parent_id: Optional[str]) -> Optional[str]:
    """The profile home this message will run in, as the gateway resolves it (A4). ``None`` = unknown."""
    runner = getattr(adapter, "gateway_runner", None)
    resolver = getattr(runner, "_resolve_profile_home_for_source", None)
    build = getattr(adapter, "build_source", None)
    if runner is None or not callable(resolver) or not callable(build):
        return None
    source = build(chat_id=channel_id, chat_type="thread" if thread_id else "group", user_id=author_id,
                   thread_id=thread_id, guild_id=guild_id, parent_chat_id=parent_id,
                   message_id=str(getattr(message, "id", "") or "") or None)
    if getattr(source, "profile_route_rejected", False):
        return None
    return hermes.home_key(resolver(source))


def decide(adapter: Any, message: Any) -> Optional[Decision]:
    try:
        return _decide(adapter, message)
    except Exception:
        logger.exception("ctx_discord_policy: admission decision failed; stock behaviour for this message")
        return None


def _decide(adapter: Any, message: Any) -> Optional[Decision]:
    guild = getattr(message, "guild", None)
    author = getattr(message, "author", None)
    if guild is None or author is None or bool(getattr(author, "bot", False)):
        return None
    binding = RUNTIME.bots.for_adapter(adapter)
    if binding is None or not RUNTIME.status.security_ok(*REQUIRED_SECURITY):
        return None
    guild_id = snowflake(getattr(guild, "id", None))
    author_id = snowflake(getattr(author, "id", None))
    channel = getattr(message, "channel", None)
    channel_id = snowflake(getattr(channel, "id", None))
    if not (guild_id and author_id and channel_id):
        return None
    self_user = getattr(getattr(adapter, "_client", None), "user", None)
    if self_user is not None and str(getattr(self_user, "id", "")) == author_id:
        return None
    snapshot = binding.store.read()
    if snapshot.guild_state(binding.bot_id, guild_id) != "active":
        return None  # unbound, paused, or a corrupt store (every guild counts as paused)
    guild_cfg = snapshot.guild(binding.bot_id, guild_id) or {}
    conversation = guild_cfg.get("conversation") or {}
    is_thread = classify.is_thread(channel)
    parent_id = snowflake(getattr(channel, "parent_id", None)) if is_thread else None
    scope = {channel_id} | ({parent_id} if parent_id else set())
    excluded = bool(scope & set(conversation.get("excluded_channels") or ()))
    signal = classify.direct_signal(adapter, message)
    ambient_channel = bool(scope & set(conversation.get("ambient_channels") or ()))
    addressed = True
    if signal == classify.DIRECT:
        mode, unverified = DIRECT, False
    elif signal == classify.MAYBE_REPLY:
        mode, unverified = (AMBIENT, False) if ambient_channel else (DIRECT, True)
    elif ambient_channel:
        mode, unverified = AMBIENT, False
    else:
        # Not addressed and not ambient: stock decides whether the bot reacts at all (bot threads, free
        # channels, allowlisted users); if it does, the turn still runs under this message's lane.
        mode, unverified, addressed = DIRECT, False, False
    lane = OPERATOR if config.is_operator(binding.owner_home, author_id) else PUBLIC
    worklog_channel = (guild_cfg.get("worklog") or {}).get("channel")
    if lane == PUBLIC and worklog_channel and worklog_channel in scope:
        excluded = True  # the worklog channel and its threads carry operator tool output: no public turns
    if excluded and lane == OPERATOR:
        return None  # "never respond in" binds public lanes; the operator keeps stock behaviour there
    if lane == PUBLIC and sessions_shared(adapter, guild_id=guild_id, channel_id=channel_id):
        _note_closed(f"shared:{binding.bot_id}", "Hermes shares one session between members of a channel "
                     "(group_sessions_per_user: false): public admission stays closed, because batching and "
                     "busy-input handling would mix public text into operator turns")
        return None
    # Public messages in threads are handed to Hermes as plain channel messages (own per-user session,
    # never the thread session an operator may share): resolve the target as the runner will see it, and
    # refuse when that differs from where the thread itself routes (a route by parent channel or thread).
    thread_view = is_thread and lane == OPERATOR
    target = target_home_key(adapter, message, guild_id=guild_id, channel_id=channel_id, author_id=author_id,
                             thread_id=channel_id if thread_view else None,
                             parent_id=parent_id if thread_view else None)
    if target is None:
        return None
    if is_thread and lane == PUBLIC:
        threaded = target_home_key(adapter, message, guild_id=guild_id, channel_id=channel_id,
                                   author_id=author_id, thread_id=channel_id, parent_id=parent_id)
        if threaded != target:
            _note_closed(f"thread-route:{channel_id}", f"thread {channel_id} routes to another profile than a "
                         "plain message there would: public admission in this thread stays closed")
            return None
    if not hooks.armed_live(target):
        _note_unarmed(target)
        return None
    me = getattr(guild, "me", None)
    bot_name = (getattr(me, "display_name", None) or getattr(self_user, "display_name", None)
                or getattr(self_user, "name", None) or "")
    return Decision(bot_id=binding.bot_id, owner_home=binding.owner_home, guild_id=guild_id,
                    channel_id=channel_id, author_id=author_id, lane=lane, mode=mode,
                    lanes=PublicLanes.from_guild(guild_cfg), target_home_key=target, reply_unverified=unverified,
                    addressed=addressed, excluded=excluded, in_thread=is_thread,
                    server_name=str(getattr(guild, "name", "") or ""),
                    channel_name=str(getattr(channel, "name", "") or ""), bot_display_name=str(bot_name))


# ── message views ────────────────────────────────────────────────────────────────────────────────


class _Delegate:
    """Read-through proxy: explicit overrides first, every other attribute from the original object."""

    def __init__(self, original: Any, **overrides: Any) -> None:
        object.__setattr__(self, "_original", original)
        for name, value in overrides.items():
            object.__setattr__(self, name, value)

    def __getattr__(self, name: str) -> Any:
        return getattr(object.__getattribute__(self, "_original"), name)


class AdmissionView(_Delegate):
    """Stock admission with only the unrelated-mention gate removed (message type, author, dedup stay)."""


def admission_view(message: Any) -> Any:
    if not list(getattr(message, "mentions", []) or []):
        return message
    return AdmissionView(message, mentions=[])


class AmbientAuthor(_Delegate):
    """Shared identity of one channel's ambient conversation (public members only)."""


class PlainChannel(_Delegate):
    """A thread presented as an ordinary channel (not a ``discord.Thread`` instance): Hermes then keys the
    session per user instead of per thread, so a public member never joins a thread session that the
    operator (or anyone with tools) uses. Every attribute and method still delegates to the thread."""


def routed_message(message: Any, route: Route, *, self_user: Any, message_type: Any, addressed: bool = True,
                   isolate_thread: bool = False) -> Any:
    """The message the stock handler sees, carrying its route.

    Addressed messages are marked as a reply that mentions the bot (no auto-thread, passes the mention
    gate); unaddressed ones keep their mentions and type, so stock alone decides whether the bot reacts.
    Ambient public messages speak as the channel's shared author; public messages in threads see the
    thread as a plain channel (see :class:`PlainChannel`)."""
    mentions = list(getattr(message, "mentions", []) or [])
    if addressed and self_user is not None and all(str(getattr(m, "id", "")) != str(getattr(self_user, "id", ""))
                                                   for m in mentions):
        mentions.append(self_user)
    author = getattr(message, "author", None)
    content = getattr(message, "content", "") or ""
    if route.session_user_id != route.author_id:
        name = str(getattr(author, "display_name", None) or getattr(author, "name", None) or "member")
        name = classify.clean_label(name)
        content = f"[Discord participant: {name} ({route.author_id})]\n{content}"
        author = AmbientAuthor(author, id=route.session_user_id, bot=False, display_name="Discord conversation")
    created = getattr(message, "created_at", None) or datetime.now(timezone.utc)
    overrides = dict(author=author, content=content, mentions=mentions, created_at=created, _dsp_route=route)
    if addressed:
        overrides["type"] = message_type
    if isolate_thread:
        overrides["channel"] = PlainChannel(getattr(message, "channel", None))
    return _Delegate(message, **overrides)
