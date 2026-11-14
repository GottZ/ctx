"""A4-P: public turns run only in the profile the operator chose, and never in a locked profile.

Three places in the core ask (PROFILE-PER-GUILD §3.5), because Hermes runs a member's message on three paths:

* :func:`admission_refusal` — the adapter's admission (``ingress.admission``), for every member's message.
  A follow-up to a busy session is authorized later only through the route stored here and never passes
  the runner wrapper, so a closed server must not store one;
* :func:`refusal` — the runner wrapper (``policy.authority.refusal``), with the source Hermes runs the
  turn for (catches what changed between admission and the start of the turn);
* :func:`turn_refusal` — the turn start (``pre_llm_call``). Queued follow-ups run inside the running turn
  under its route, so a switch or a lock after their admission is caught only there (Hermes honours a
  ``pre_llm_call`` block from v0.23.0; older releases ignore it and keep the public tool gate only).

:func:`refusal` closes (a reason is returned, the event is dropped) when:

* the turn would run in a profile listed in ``settings.locked_profiles`` (always, whatever routed it
  there: a plugin entry, a hand-written route or the bot's own profile), or the list can't be read;
* the server has a chosen profile and the running route table does not route it there through this
  server's plugin entry — unless a more specific hand-written route (channel, thread, user) applies;
* the server has no chosen profile but the running table still holds its plugin entry;
* the message was routed before the server's profile changed (its stamped profile is not what the
  running table says now): a switch drops public messages that were waiting, turns already running
  finish where they started;
* the chosen profile's home is gone (Hermes would fall back to the launch home);
* the server's members' model is unavailable and the operator chose ``close`` for that case (S13).

Operator turns are not checked here: they keep stock behaviour (PROFILE-PER-GUILD §4.3).
"""

from __future__ import annotations

import logging
import threading
import time
from typing import Any, Optional

from .. import config, hermes
from ..adapt.status import SECURITY, Outcome, Result
from ..runtime import RUNTIME, BoundedMap
from . import eligibility, table

logger = logging.getLogger("ctx_discord_policy")

INTERVENTION = "A4-P"
NOTE_EVERY = 300.0

# (bot, guild) whose plugin route changed in this process: the stamped-profile check also applies after
# the server went back to the bot's own profile (no plugin entry left to recognise it by).
TOUCHED: BoundedMap = BoundedMap()
# (bot, guild) -> reason of the last closed public message; a server leaves it with its next passing one.
# The status row reflects every server in it, so a busy open server never hides a closed one.
CLOSED: BoundedMap = BoundedMap()
_last_note: BoundedMap = BoundedMap()
_lock = threading.Lock()


# (bot, guild) -> time the server's chosen profile last changed in this process: public turns of routes
# admitted before it never start (switching drops members' messages that were waiting).
SWITCHED: BoundedMap = BoundedMap()

TURN_SWITCHED = ("ctx_discord_policy: this server's settings changed after your message arrived, so it is not "
                 "answered. Please send it again.")
TURN_CLOSED = "ctx_discord_policy: members' messages are not answered in this server right now."


def mark_touched(bot_id: str, guild_id: str) -> None:
    TOUCHED.put((str(bot_id), str(guild_id)), time.time())


def mark_switched(bot_id: str, guild_id: str) -> None:
    SWITCHED.put((str(bot_id), str(guild_id)), time.time())


def _norm(profile: Any) -> Optional[str]:
    text = str(profile or "").strip()
    return None if not text or text == "default" else text


def _record(latest: Optional[tuple[str, str]] = None) -> None:
    count = len(CLOSED)
    if not count:
        RUNTIME.status.record(Result(INTERVENTION, SECURITY, Outcome.OK, "public turns run in their chosen profile",
                                     "per-message check"))
        return
    detail = f"; server {latest[0]}: {latest[1]}" if latest else ""
    RUNTIME.status.record(Result(INTERVENTION, SECURITY, Outcome.FALLBACK,
                                 f"{count} server{'s' if count != 1 else ''} closed for members{detail}",
                                 "per-message check"))


def _note(bot_id: str, guild_id: str, reason: str) -> str:
    key = (bot_id, guild_id)
    with _lock:
        changed = CLOSED.get(key) != reason
        CLOSED.put(key, reason)
        now = time.monotonic()
        last = _last_note.get((bot_id, guild_id, reason))
        due = changed or last is None or now - last >= NOTE_EVERY
        if due:
            _last_note.put((bot_id, guild_id, reason), now)
            _record((guild_id, reason))
    return reason


def _cleared(bot_id: str, guild_id: str) -> None:
    with _lock:
        if CLOSED.pop((bot_id, guild_id)) is not None:
            _record()


def refusal(runner: Any, event: Any, route: Any, target: Optional[str]) -> Optional[str]:
    """Why this public event must not run (``None``: it may). Any internal error closes."""
    if route is None or getattr(route, "is_operator", False):
        return None
    try:
        reason = _refusal(runner, event, route, target)
    except Exception:
        logger.exception("ctx_discord_policy: A4-P check failed; public message dropped")
        reason = "the per-server profile check failed internally"
    if reason is not None:
        return _note(str(route.bot_id), str(route.guild_id), reason)
    _cleared(str(route.bot_id), str(route.guild_id))
    return None


def admission_refusal(adapter: Any, *, bot_id: str, guild_id: str, channel_id: str, author_id: str,
                      message_id: str, target: Optional[str]) -> Optional[str]:
    """A4-P for a member's message at admission: the source as the adapter builds it for a public turn."""
    route = _Probe(str(bot_id), str(guild_id))
    try:
        source = adapter.build_source(chat_id=channel_id, chat_type="group", user_id=author_id, guild_id=guild_id,
                                      message_id=message_id or None)
    except Exception:
        logger.exception("ctx_discord_policy: A4-P could not build the message source; not admitted")
        return _note(route.bot_id, route.guild_id, "the message source could not be built")
    return refusal(getattr(adapter, "gateway_runner", None), _Event(source), route, target)


def turn_refusal(route: Any) -> Optional[str]:
    """A4-P at the start of a public turn: the member-facing text when it must not run (``None``: it may).

    Runs in the turn's profile scope. Closed when the server switched profiles after the route was admitted,
    or the profile this turn runs in is locked (or the lock list can't be read); any error closes."""
    if route is None or getattr(route, "is_operator", False):
        return None
    try:
        switched = SWITCHED.get((str(route.bot_id), str(route.guild_id)))
        if switched is not None and switched > float(getattr(route, "created_at", 0.0) or 0.0):
            _note(str(route.bot_id), str(route.guild_id), "a turn admitted before the profile switch was not started")
            return TURN_SWITCHED
        locked = config.locked_profiles(hermes.default_root())
        here = hermes.home_key(hermes.current_home())
        if locked is None or here in {k for k in map(eligibility.home_key_of, locked) if k}:
            _note(str(route.bot_id), str(route.guild_id), "a turn in a locked profile was not started")
            return TURN_CLOSED
    except Exception:
        logger.exception("ctx_discord_policy: A4-P turn check failed; public turn not started")
        return TURN_CLOSED
    return None


class _Probe:
    """The route fields :func:`refusal` reads, for a message that has no route yet (admission)."""

    is_operator = False

    def __init__(self, bot_id: str, guild_id: str) -> None:
        self.bot_id, self.guild_id = bot_id, guild_id


class _Event:
    def __init__(self, source: Any) -> None:
        self.source = source


def _refusal(runner: Any, event: Any, route: Any, target: Optional[str]) -> Optional[str]:
    bot_id, guild_id = str(route.bot_id), str(route.guild_id)
    binding = RUNTIME.bots.for_bot(bot_id)
    if binding is None:
        return "the bot is not bound"
    guild = binding.store.read().guild(bot_id, guild_id) or {}
    desired = (guild.get("routing") or {}).get("profile")
    locked = config.locked_profiles(hermes.default_root())
    if locked is None:
        return "settings.locked_profiles can't be read"
    if target is None or target in {k for k in map(eligibility.home_key_of, locked) if k}:
        return "it would run in a locked profile (settings.locked_profiles)"
    owner = eligibility.owner_profile_name(binding.owner_home)
    cfg = getattr(runner, "config", None)
    routes = getattr(cfg, "profile_routes", None)
    own_live = [r for r in routes or () if table.is_own_entry(r, bot_id, guild_id)]
    if desired is None and not own_live and (bot_id, guild_id) not in TOUCHED:
        return _model_refusal(runner, event, route, guild, target)
    if not getattr(cfg, "multiplex_profiles", False):
        return "the server has a chosen profile but Hermes does not multiplex profiles"
    if not isinstance(routes, (list, tuple)):
        return "the running route table is not recognised (Hermes changed)"
    source = getattr(event, "source", None)
    matched = hermes.match_route(list(routes), source, table.bot_profile_key(owner))
    exception = matched is not None and not table.is_plugin_entry(matched) \
        and table.specificity(matched) > table.GUILD_SPECIFICITY
    if desired is not None:
        ours = matched is not None and table.is_own_entry(matched, bot_id, guild_id) \
            and getattr(matched, "profile", None) == desired
        if not (ours or exception):
            return "the running Hermes does not route this server to its chosen profile (pending restart or changed)"
    elif own_live:
        return "the running Hermes still routes this server through a plugin entry the policy no longer has"
    expected = getattr(matched, "profile", None) if matched is not None else owner
    if _norm(expected) != _norm(getattr(source, "profile", None)):
        return "the message was routed before the server's profile changed"
    if matched is not None and table.is_own_entry(matched, bot_id, guild_id):
        if target != eligibility.home_key_of(str(desired)):
            return f"the home of profile {desired} is gone (Hermes would fall back to the launch home)"
    return _model_refusal(runner, event, route, guild, target)


def _model_refusal(runner: Any, event: Any, route: Any, guild: dict, target: Optional[str]) -> Optional[str]:
    from ..models import check as model_check  # late: models imports routing for the profile scope

    return model_check.public_refusal(runner, event, route, guild, target)
