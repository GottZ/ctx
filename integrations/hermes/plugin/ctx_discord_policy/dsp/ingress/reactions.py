"""Reaction wakeup (intervention K5; DESIGN §5.2 ``identity.reaction_emoji``, §6.2 K5).

A person who reacts with the server's wake emoji to a message wakes the bot for exactly that message.
The woken message is the reactor's own: written by the reactor, replying to the reacted-to message, with
the wakeup text of the prompt engine (``prompts.reaction_wakeup_text``). It then runs through the same
admission gate as every other message (``ingress.admission.decide`` and the wrapped
``DiscordAdapter._handle_message``); this module decides nothing about lanes or tools. A member who wakes
the bot on an operator's message therefore gets a public turn of their own, never an operator turn.

Who and where:

* only people — bots and webhooks never wake the bot (``classify.is_human_author``);
* only in a server where the policy is active and a wake emoji is configured (custom emoji by id, or a
  Unicode emoji; ``null`` switches the wakeup off);
* only where the reactor could have written the message themself (``send_messages``, in threads
  ``send_messages_in_threads``): a reaction in a channel that is read-only for the reactor wakes nothing;
* once per (message, reactor, emoji): removing the reaction allows a new wakeup; a wakeup that did not
  reach dispatch is forgotten, so the reaction can be added again.

Strategies (DESIGN §6.1): (1) the listener goes onto the native client the platform-handler factory hands
over (``native.add_listener``, discord.ext.commands.Bot); (2) a client without ``add_listener`` but with
discord.py's event dispatch gets its ``on_raw_reaction_add/remove`` handlers wrapped (any existing handler
keeps running first); otherwise ``off``. The result goes into the status register (comfort class).
"""

from __future__ import annotations

import logging
import sys
import time
from typing import Any, Awaitable, Callable, Mapping, Optional

from .. import RUNTIME_API, config, prompts
from ..adapt.status import COMFORT, Outcome, Result, StatusRegister
from ..adapt.strategies import WRAPPER_MARK, is_own_wrapper, run_intervention
from ..policy.routes import snowflake
from ..runtime import ROUTE_CAP, RUNTIME, BoundedMap
from . import classify

logger = logging.getLogger("ctx_discord_policy")

INTERVENTION = "K5"
LISTENER_MARK = "_dsp_reaction_listeners"
SEEN: BoundedMap[tuple[str, str, str, str], float] = BoundedMap(ROUTE_CAP)

GuildConfigGetter = Callable[[str, str], Optional[Mapping[str, Any]]]
Dispatch = Callable[[Any, Any], Awaitable[bool]]

# Variation selectors and skin tone modifiers: Discord applies a member's chosen skin tone to reactions,
# so "\ud83d\udc4b\ud83c\udfff" and "\ud83d\udc4b" are the same wake emoji.
_SELECTORS = str.maketrans("", "", "\ufe0e\ufe0f\U0001f3fb\U0001f3fc\U0001f3fd\U0001f3fe\U0001f3ff")
WAKEUP_MARKER_TEXT = "[REACTION WAKEUP]"


# ── emoji ────────────────────────────────────────────────────────────────────────────────────────


def emoji_key(emoji: Any) -> Optional[str]:
    """``custom:<id>`` for a custom emoji, ``unicode:<text>`` (without variation selectors and skin tone
    modifiers) otherwise."""
    if emoji is None:
        return None
    if isinstance(emoji, str):
        ident, name = None, emoji
    elif isinstance(emoji, Mapping):
        ident, name = emoji.get("id"), emoji.get("name")
    else:
        ident, name = getattr(emoji, "id", None), getattr(emoji, "name", None)
    custom = snowflake(ident) if ident is not None and not isinstance(ident, bool) else None
    if custom:
        return f"custom:{custom}"
    if isinstance(name, str) and name.translate(_SELECTORS):
        return "unicode:" + name.translate(_SELECTORS)
    return None


def configured_emoji(guild_config: Any) -> Optional[str]:
    """The guild's wake emoji as an :func:`emoji_key`; ``None`` = the reaction wakeup is off."""
    if not isinstance(guild_config, Mapping):
        return None
    identity = guild_config.get("identity")
    if not isinstance(identity, Mapping):
        return None
    return emoji_key(identity.get("reaction_emoji"))


# ── the woken message ────────────────────────────────────────────────────────────────────────────


class Reference:
    """``message.reference`` of the woken message: the reacted-to message, resolved."""

    def __init__(self, original: Any, channel_id: Any, guild_id: Any) -> None:
        self.message_id = getattr(original, "id", None)
        self.channel_id = channel_id
        self.guild_id = guild_id
        self.resolved = original
        self.cached_message = original
        self.fail_if_not_exists = False


class WakeupMessage:
    """The message a reaction wakeup hands to the gate: the reactor's reply to the reacted-to message.

    Only the attributes listed here exist (no fallback to the original), so nothing of the reacted-to
    message — its author, webhook id, attachments, mentions — passes as the reactor's own input; the
    original reaches the turn only as the resolved reference. Processing reactions of Hermes land on the
    reacted-to message, which is where the reactor looks for them.
    """

    def __init__(self, original: Any, *, author: Any, channel: Any, guild: Any, content: str, bot_user: Any,
                 message_type: Any, wakeup: classify.ReactionWakeup) -> None:
        from datetime import datetime, timezone

        self._original = original
        self.id = getattr(original, "id", None)
        self.author = author
        self.channel = channel
        self.guild = guild
        self.content = content
        self.clean_content = content
        self.mentions = [bot_user] if bot_user is not None else []
        self.raw_mentions = [bot_user.id] if bot_user is not None else []
        self.role_mentions: list = []
        self.raw_role_mentions: list = []
        self.channel_mentions: list = []
        self.mention_everyone = False
        self.type = message_type
        self.reference = Reference(original, getattr(channel, "id", None), getattr(guild, "id", None))
        self.attachments: list = []
        self.embeds: list = []
        self.stickers: list = []
        self.message_snapshots: list = []
        self.components: list = []
        self.webhook_id = None
        self.application_id = None
        self.interaction_metadata = None
        self.poll = None
        self.flags = None
        self.pinned = False
        self.tts = False
        self.created_at = datetime.now(timezone.utc)
        self.edited_at = None
        self.jump_url = str(getattr(original, "jump_url", "") or "")
        setattr(self, classify.WAKEUP_MARKER, wakeup)

    async def add_reaction(self, *args: Any, **kwargs: Any) -> Any:
        return await self._original.add_reaction(*args, **kwargs)

    async def remove_reaction(self, *args: Any, **kwargs: Any) -> Any:
        return await self._original.remove_reaction(*args, **kwargs)

    def __repr__(self) -> str:
        return f"<WakeupMessage id={self.id} reactor={getattr(self.author, 'id', None)}>"


def _reply_type(adapter: Any) -> Any:
    for klass in type(adapter).__mro__:
        module = sys.modules.get(getattr(klass, "__module__", "") or "")
        discord = getattr(module, "discord", None)
        message_type = getattr(discord, "MessageType", None)
        if message_type is not None and hasattr(message_type, "reply"):
            return message_type.reply
    try:
        import discord  # noqa: PLC0415 - in-process Hermes ships discord.py
    except ImportError:
        return "reply"
    return discord.MessageType.reply


def _channel_guild(channel: Any) -> str:
    guild = getattr(channel, "guild", None)
    return snowflake(getattr(guild, "id", None) if guild is not None else getattr(channel, "guild_id", None)) or ""


def _may_write(channel: Any, member: Any) -> bool:
    """The reactor could have written a message here (the reaction stands for that message)."""
    permissions_for = getattr(channel, "permissions_for", None)
    if not callable(permissions_for):
        return False
    try:
        perms = permissions_for(member)
    except Exception:
        return False
    if not bool(getattr(perms, "view_channel", False)):
        return False
    flag = "send_messages_in_threads" if classify.is_thread(channel) else "send_messages"
    return bool(getattr(perms, flag, False))


async def _member(client: Any, guild: Any, payload: Any, reactor_id: str) -> Any:
    member = getattr(payload, "member", None)
    if member is not None:
        return member
    get_member = getattr(guild, "get_member", None)
    if callable(get_member):
        try:
            member = get_member(int(reactor_id))
        except Exception:
            member = None
    if member is None:
        fetch_member = getattr(guild, "fetch_member", None)
        if callable(fetch_member):
            try:
                member = await fetch_member(int(reactor_id))
            except Exception:
                member = None
    return member


async def _channel(client: Any, channel_id: str) -> Any:
    getter = getattr(client, "get_channel", None)
    channel = None
    if callable(getter):
        try:
            channel = getter(int(channel_id))
        except Exception:
            channel = None
    if channel is None:
        fetch = getattr(client, "fetch_channel", None)
        if callable(fetch):
            channel = await fetch(int(channel_id))
    return channel


# ── the gate ─────────────────────────────────────────────────────────────────────────────────────


def store_guild_config(adapter: Any) -> GuildConfigGetter:
    """``get_guild_config(bot_id, guild_id)`` from the store of the adapter's bot: the guild's settings
    while the guild is ``active`` (a paused, unbound or corrupt state yields ``None``)."""

    def get(bot_id: str, guild_id: str) -> Optional[Mapping[str, Any]]:
        binding = RUNTIME.bots.for_adapter(adapter)
        if binding is None or binding.bot_id != str(bot_id):
            return None
        snapshot = binding.store.read()
        if snapshot.guild_state(binding.bot_id, str(guild_id)) != "active":
            return None
        return snapshot.guild(binding.bot_id, str(guild_id))

    return get


async def dispatch_through_gate(adapter: Any, message: Any) -> bool:
    """Hand the woken message to the policy's gate: only a message the gate admits as addressed is handed
    to the (wrapped) handler, so a wakeup the policy does not take never reaches stock Hermes, which has
    no reaction wakeup of its own. The handler wrapper decides again and builds the route."""
    from . import admission  # late: admission imports the policy package

    decision = admission.decide(adapter, message)
    if decision is None or decision.excluded or not decision.addressed:
        return False
    handle = getattr(adapter, "_handle_message", None)
    if not callable(handle):
        return False
    return bool(await handle(message, role_authorized=False))


def _active_config(get_guild_config: GuildConfigGetter, bot_id: str, guild_id: str) -> Optional[Mapping[str, Any]]:
    try:
        cfg = get_guild_config(bot_id, guild_id)
    except Exception:
        logger.warning("ctx_discord_policy: guild configuration unreadable for a reaction", exc_info=True)
        return None
    if not isinstance(cfg, Mapping) or cfg.get("state", "active") != "active":
        return None
    return cfg


def _command_like(text: str, bot_id: str) -> bool:
    """Would Hermes read the woken message as a gateway command? It drops the bot's own mention, then
    leading attachment references (``@image:`` …), and treats a leading ``/`` as a command."""
    seen = text.replace(f"<@{bot_id}>", "").replace(f"<@!{bot_id}>", "").lstrip()
    return seen[:1] in ("/", "@")


def _wakeup_text(binding: Any, cfg: Mapping[str, Any], guild: Any, channel: Any, client: Any,
                 reactor_id: str) -> str:
    """The prompt engine's wakeup text for this reactor.

    Text the server's managers wrote (bot name, reply language) reaches an operator's turn only where the
    operator switched the server guidance on for operator turns (the rule of ``prompts.render``). The
    text never reads as a gateway command, whatever a template or a Discord name contains: if it would,
    the shipped template is used instead."""
    try:
        label = config.setting(binding.owner_home, "operator_label")
    except Exception:
        label = None
    settings = {"operator_label": label} if isinstance(label, str) else {}
    try:
        operator = config.is_operator(binding.owner_home, reactor_id)
    except Exception:
        operator = True                                  # unknown: no text of the server's managers
    view = dict(cfg)
    section = cfg.get("prompts")
    if operator and not (isinstance(section, Mapping) and section.get("guild_in_operator_lane") is True):
        view.pop("identity", None)
        view.pop("conversation", None)
    me = getattr(guild, "me", None)
    display = getattr(me, "display_name", None) or getattr(getattr(client, "user", None), "name", None) or ""
    bot_id = snowflake(getattr(getattr(client, "user", None), "id", None)) or ""

    def render(guild_view: Mapping[str, Any]) -> str:
        return prompts.reaction_wakeup_text(settings, guild_view, server_name=getattr(guild, "name", "") or "",
                                            channel_name=getattr(channel, "name", "") or "",
                                            channel_id=getattr(channel, "id", "") or "", bot_display_name=display)

    text = render(view)
    if _command_like(text, bot_id):
        text = render({key: value for key, value in view.items() if key != "prompts"})   # shipped template
    if _command_like(text, bot_id):
        text = f"{WAKEUP_MARKER_TEXT} {text}"
    return text


async def on_reaction_add(adapter: Any, payload: Any, get_guild_config: GuildConfigGetter,
                          dispatch: Dispatch) -> str:
    """Handle one ``raw_reaction_add`` event; returns what happened (for logs and tests)."""
    guild_id = snowflake(getattr(payload, "guild_id", None))
    if not guild_id:
        return "ignored: not in a server"
    binding = RUNTIME.bots.for_adapter(adapter)
    if binding is None:
        return "ignored: bot not bound"
    cfg = _active_config(get_guild_config, binding.bot_id, guild_id)
    if cfg is None:
        return "ignored: policy not active in this server"
    wanted = configured_emoji(cfg)
    if wanted is None:
        return "ignored: no wake reaction configured"
    if emoji_key(getattr(payload, "emoji", None)) != wanted:
        return "ignored: another emoji"
    client = getattr(adapter, "_client", None)
    self_id = snowflake(getattr(getattr(client, "user", None), "id", None))
    reactor_id = snowflake(getattr(payload, "user_id", None))
    channel_id = snowflake(getattr(payload, "channel_id", None))
    message_id = snowflake(getattr(payload, "message_id", None))
    if not (self_id and reactor_id and channel_id and message_id):
        return "ignored: incomplete event"
    if reactor_id == self_id:
        return "ignored: the bot's own reaction"
    key = (binding.bot_id, message_id, reactor_id, wanted)
    if key in SEEN:
        return "ignored: duplicate"
    SEEN.put(key, time.time())       # before the first await: a duplicate event can never slip in between
    try:
        outcome = await _wake(adapter, payload, cfg, binding, client, guild_id, channel_id, message_id,
                              reactor_id, wanted, dispatch)
    except Exception:
        SEEN.pop(key)
        logger.exception("ctx_discord_policy: reaction wakeup for message %s failed", message_id)
        return "failed"
    if outcome != "dispatched":
        SEEN.pop(key)
    return outcome


async def _wake(adapter: Any, payload: Any, cfg: Mapping[str, Any], binding: Any, client: Any, guild_id: str,
                channel_id: str, message_id: str, reactor_id: str, wanted: str, dispatch: Dispatch) -> str:
    get_guild = getattr(client, "get_guild", None)
    guild = get_guild(int(guild_id)) if callable(get_guild) else None
    if guild is None:
        return "ignored: server not available"
    member = await _member(client, guild, payload, reactor_id)
    if member is None or snowflake(getattr(member, "id", None)) != reactor_id:
        return "ignored: reactor unknown"
    if not classify.is_human_author(member):
        return "ignored: bots and webhooks never wake the bot"
    channel = await _channel(client, channel_id)
    if channel is None or _channel_guild(channel) != guild_id:
        return "ignored: channel not in this server"
    if not _may_write(channel, member):
        return "ignored: the reactor cannot write in this channel"
    original = await channel.fetch_message(int(message_id))
    text = _wakeup_text(binding, cfg, guild, channel, client, reactor_id)
    wakeup = classify.ReactionWakeup(bot_id=binding.bot_id, guild_id=guild_id, channel_id=channel_id,
                                     message_id=snowflake(getattr(original, "id", None)) or message_id,
                                     reactor_id=reactor_id, emoji=wanted)
    bot_user = getattr(client, "user", None)
    woken = WakeupMessage(original, author=member, channel=channel, guild=guild,
                          content=f"<@{snowflake(getattr(bot_user, 'id', None))}> {text}", bot_user=bot_user,
                          message_type=_reply_type(adapter), wakeup=wakeup)
    if not await dispatch(adapter, woken):
        logger.info("ctx_discord_policy: reaction wakeup on message %s by %s was not admitted", message_id,
                    reactor_id)
        return "not dispatched"
    return "dispatched"


def on_reaction_remove(adapter: Any, payload: Any) -> bool:
    """A removed reaction may wake the bot again when it is added again."""
    binding = RUNTIME.bots.for_adapter(adapter)
    key_emoji = emoji_key(getattr(payload, "emoji", None))
    message_id = snowflake(getattr(payload, "message_id", None))
    reactor_id = snowflake(getattr(payload, "user_id", None))
    if binding is None or not (key_emoji and message_id and reactor_id):
        return False
    return SEEN.pop((binding.bot_id, message_id, reactor_id, key_emoji)) is not None


# ── installation ─────────────────────────────────────────────────────────────────────────────────


def _listeners(adapter: Any, get_guild_config: GuildConfigGetter, dispatch: Dispatch) -> tuple[Any, Any]:
    async def on_raw_reaction_add(payload: Any) -> None:
        try:
            await on_reaction_add(adapter, payload, get_guild_config, dispatch)
        except Exception:  # a listener must never take the client's event loop down
            logger.exception("ctx_discord_policy: reaction listener failed")

    async def on_raw_reaction_remove(payload: Any) -> None:
        try:
            on_reaction_remove(adapter, payload)
        except Exception:
            logger.exception("ctx_discord_policy: reaction removal listener failed")

    return on_raw_reaction_add, on_raw_reaction_remove


def _chain(previous: Any, ours: Any) -> Any:
    async def chained(*args: Any, **kwargs: Any) -> Any:
        result = None
        if previous is not None:
            result = await previous(*args, **kwargs)
        await ours(*args, **kwargs)
        return result

    chained.__name__ = ours.__name__
    if previous is not None:
        chained.__wrapped__ = previous  # type: ignore[attr-defined]
    setattr(chained, WRAPPER_MARK, (INTERVENTION, RUNTIME_API))
    return chained


def install_reaction_wakeup(native: Any, adapter: Any, get_guild_config: Optional[GuildConfigGetter] = None,
                            dispatch: Optional[Dispatch] = None,
                            register: Optional[StatusRegister] = None) -> Result:
    """Install the reaction wakeup on the native Discord client (call it from the platform-handler factory,
    after the bot is bound). ``get_guild_config(bot_id, guild_id)`` defaults to the bot's store (active
    guilds only), ``dispatch(adapter, message)`` to :func:`dispatch_through_gate`."""
    register = register or RUNTIME.status
    getter = get_guild_config or store_guild_config(adapter)
    deliver = dispatch or dispatch_through_gate

    def body() -> Result:
        if native is None:
            return Result(INTERVENTION, COMFORT, Outcome.OFF,
                          "the platform handler factory handed over no native client; reaction wakeups are off",
                          "platform handler factory")
        add_listener = getattr(native, "add_listener", None)
        if callable(add_listener):
            if getattr(native, LISTENER_MARK, None) is not None:
                return Result(INTERVENTION, COMFORT, Outcome.INJECTED,
                              "already installed: reaction listeners on this client", "native.add_listener")
            on_add, on_remove = _listeners(adapter, getter, deliver)
            add_listener(on_add, "on_raw_reaction_add")
            add_listener(on_remove, "on_raw_reaction_remove")
            setattr(native, LISTENER_MARK, (on_add, on_remove))
            return Result(INTERVENTION, COMFORT, Outcome.INJECTED,
                          "reaction listeners added through the platform handler factory (native.add_listener)",
                          f"{type(native).__name__}.add_listener")
        if callable(getattr(native, "dispatch", None)) and callable(getattr(native, "event", None)):
            on_add, on_remove = _listeners(adapter, getter, deliver)
            states = []
            for name, ours in (("on_raw_reaction_add", on_add), ("on_raw_reaction_remove", on_remove)):
                current = getattr(native, name, None)
                if is_own_wrapper(current, INTERVENTION):
                    states.append("already")
                    continue
                setattr(native, name, _chain(current, ours))
                states.append("wrapped")
            note = "already wrapped" if all(s == "already" for s in states) else "wrapped"
            return Result(INTERVENTION, COMFORT, Outcome.WRAPPED,
                          f"{note}: the client's on_raw_reaction_add/remove handlers (no add_listener)",
                          f"{type(native).__name__}.dispatch")
        return Result(INTERVENTION, COMFORT, Outcome.OFF,
                      "the native client has neither add_listener nor discord.py event dispatch; reaction "
                      "wakeups are off", type(native).__name__)

    return run_intervention(register, INTERVENTION, COMFORT, body)


__all__ = ["INTERVENTION", "SEEN", "WakeupMessage", "configured_emoji", "dispatch_through_gate", "emoji_key",
           "install_reaction_wakeup", "on_reaction_add", "on_reaction_remove", "store_guild_config"]
