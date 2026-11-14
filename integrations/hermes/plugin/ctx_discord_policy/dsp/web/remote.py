"""Live data from the gateway for a backend in another process, over plugin RPC (SETTINGS-PAGE §4.2 row 2).

Only where this Hermes serves plugin RPC (≥ 04846730f3) and only towards a gateway that already runs
(``hermes.gateway_rpc`` never starts one). Calls are short (:data:`TIMEOUT`); a failure is remembered for
:data:`BACKOFF` seconds per home so an unreachable gateway does not slow every page load, and answers are
cached for :data:`CACHE_TTL` seconds. Whether a standalone dashboard may mint the session ticket this needs
depends on the deployment (same OS user as the gateway); when it cannot, the snapshot files take over.
"""

from __future__ import annotations

import threading
import time
from typing import Any, Optional

from .. import hermes
from . import directory

PREFIX = f"plugin.{hermes.PLUGIN_ID}."
TIMEOUT = 3.0
# A search or a name lookup may take the gateway up to snapshot.SEARCH_TIMEOUT / USERS_TIMEOUT; waiting less
# would throw a slow answer away and stop asking for the whole back-off.
TIMEOUTS = {"members.search": 7.5, "users.resolve": 7.5}
BACKOFF = 60.0
CACHE_TTL = 30.0

_lock = threading.Lock()
_failed_until: dict[str, float] = {}
_cache: dict[tuple, tuple[float, Any]] = {}


class Unreachable(RuntimeError):
    pass


def timeout_for(method: str) -> float:
    return TIMEOUTS.get(method, TIMEOUT)


def call(home: Any, method: str, params: dict, *, cache: bool = True) -> Any:
    """``plugin.ctx_discord_policy.<method>`` on the gateway owning ``home``; raises :class:`Unreachable`."""
    key = str(home)
    now = time.monotonic()
    cache_key = (key, method, tuple(sorted((k, str(v)) for k, v in params.items())))
    with _lock:
        if _failed_until.get(key, 0.0) > now:
            raise Unreachable("gateway not reachable (recent failure)")
        hit = _cache.get(cache_key)
        if cache and hit is not None and now - hit[0] < CACHE_TTL:
            return hit[1]
    if not hermes.rpc_available():
        raise Unreachable("this Hermes has no plugin RPC")
    try:
        result = hermes.gateway_rpc(home, PREFIX + method, params, timeout_for(method))
    except Exception as exc:
        with _lock:
            _failed_until[key] = time.monotonic() + BACKOFF
        raise Unreachable(f"{type(exc).__name__}: {exc}") from exc
    with _lock:
        if cache:
            _cache[cache_key] = (time.monotonic(), result)
            if len(_cache) > 512:
                _cache.pop(next(iter(_cache)))
    return result


def _provider(home: Any, bot: str, guild: Optional[str]) -> Any:
    try:
        if guild is None:
            data = call(home, "directory.guilds", {"bot": bot})
            if not isinstance(data, dict):
                return None
            return directory.BotDirectory("rpc", data.get("at"), False, data.get("bot_name"),
                                          [g for g in data.get("guilds") or [] if isinstance(g, dict)])
        data = call(home, "directory.guild", {"bot": bot, "guild": guild})
    except Unreachable:
        return None
    if not isinstance(data, dict):
        return None
    return directory.GuildDirectory("rpc", data.get("at"), False, data.get("name"), data.get("icon"),
                                    list(data.get("channels") or []), list(data.get("roles") or []),
                                    list(data.get("emojis") or []))


directory.register_provider("rpc", _provider, priority=20)


def reset() -> None:
    with _lock:
        _failed_until.clear()
        _cache.clear()
