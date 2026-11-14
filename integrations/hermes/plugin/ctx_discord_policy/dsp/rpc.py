"""Read-only plugin RPC methods for the settings page (SETTINGS-PAGE §4.2 strategy 2, S7, mutant M10).

When the settings page backend runs in another process than the gateway (a standalone ``hermes dashboard``),
it asks the gateway for live data over Hermes' plugin RPC (``ctx.register_rpc``, Hermes ≥ 04846730f3; the
methods are served as ``plugin.ctx_discord_policy.<name>`` on the gateway's session socket, run off the
loop in the registering profile's scope). Every method here only reads: a write over RPC would be a second
write path without the settings page's guard. Each answers only for bots of the profile it runs in.

Seam for the integrator: one entry in ``bootstrap.REGISTER_STEPS`` (per armed profile instance)::

    ("P3", rpc.register_step),

Outcome: intervention ``P3`` (comfort class): ``ok`` when registered, ``off`` on older Hermes.
"""

from __future__ import annotations

import logging
from typing import Any, Callable

from . import hermes, snapshot
from .adapt.status import COMFORT, Outcome, Result
from .runtime import RUNTIME
from .store.schema import is_snowflake
from .web.settings_io import member_search_of

logger = logging.getLogger("ctx_discord_policy")

# The only names registered. Profile routing may add exactly one more, ``routing.reapply``, which applies the
# stored choice and takes no value (SETTINGS-PAGE S7).
READ_METHODS = ("directory.guilds", "directory.guild", "members.search", "users.resolve")


def _bot(params: Any) -> str:
    bot = str((params or {}).get("bot") or "")
    if not is_snowflake(bot):
        raise ValueError("bot: expected a Discord application id")
    return bot


def _guild(params: Any) -> str:
    guild = str((params or {}).get("guild") or "")
    if not is_snowflake(guild):
        raise ValueError("guild: expected a Discord server id")
    return guild


def directory_guilds(params: Any, *, session_id: Any = None, **_: Any) -> Any:
    found = snapshot.live_index(hermes.current_home(), _bot(params))
    return None if found is None else {"bot_name": found.bot_name, "guilds": found.guilds, "at": found.at}


def directory_guild(params: Any, *, session_id: Any = None, **_: Any) -> Any:
    found = snapshot.live_guild(hermes.current_home(), _bot(params), _guild(params))
    return None if found is None else {"name": found.name, "icon": found.icon, "at": found.at,
                                       "channels": found.channels, "roles": found.roles, "emojis": found.emojis}


def _member_search_on() -> bool:
    """The operator's ``member_search`` in this profile (the method runs in its scope); unreadable = off."""
    try:
        return member_search_of(hermes.plugin_settings()) == "auto"
    except Exception:
        return False


def members_search(params: Any, *, session_id: Any = None, **_: Any) -> Any:
    query = str((params or {}).get("q") or "")
    if not 2 <= len(query) <= 32:
        raise ValueError("q: 2 to 32 characters")
    if not _member_search_on():
        return {"off": True, "results": []}
    try:
        results = snapshot.live_members(hermes.current_home(), _bot(params), _guild(params), query)
    except snapshot.BusyError:
        return {"busy": True, "results": []}
    return None if results is None else {"results": results}


def users_resolve(params: Any, *, session_id: Any = None, **_: Any) -> Any:
    ids = (params or {}).get("ids")
    if not isinstance(ids, list):
        raise ValueError("ids: expected a list")
    if not _member_search_on():
        return {"users": {}}
    found = snapshot.live_users(hermes.current_home(), _bot(params), [str(i) for i in ids][:snapshot.LIMIT])
    return None if found is None else {"users": found}


HANDLERS: dict[str, Callable[..., Any]] = {
    "directory.guilds": directory_guilds,
    "directory.guild": directory_guild,
    "members.search": members_search,
    "users.resolve": users_resolve,
}


def register(ctx: Any) -> Result:
    """Register the read-only methods on this profile's plugin context, if this Hermes serves plugin RPC."""
    register_rpc = getattr(ctx, "register_rpc", None)
    if not callable(register_rpc):
        return RUNTIME.status.record(Result("P3", COMFORT, Outcome.OFF,
                                            "ctx.register_rpc missing (Hermes before 04846730f3): a separate "
                                            "dashboard process uses the directory files", "PluginContext"))
    try:
        for name in READ_METHODS:
            register_rpc(name, HANDLERS[name])
    except Exception as exc:
        logger.warning("ctx_discord_policy: registering plugin RPC methods failed", exc_info=True)
        return RUNTIME.status.record(Result("P3", COMFORT, Outcome.FALLBACK, f"register_rpc failed: {exc}",
                                            "ctx.register_rpc"))
    return RUNTIME.status.record(Result("P3", COMFORT, Outcome.OK,
                                        f"read-only methods {', '.join(READ_METHODS)}", "ctx.register_rpc"))


def register_step(ctx: Any, instance: Any = None) -> None:
    """``bootstrap.REGISTER_STEPS`` entry (``step(ctx, instance)``)."""
    register(ctx)
