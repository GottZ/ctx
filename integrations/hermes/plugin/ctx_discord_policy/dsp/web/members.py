"""Member search and user names for the operator list (SETTINGS-PAGE §4.3, S6).

Prefix search only — a complete member list needs Discord's privileged Members intent, which Hermes enables
only for name/role allowlists. Strategies, in order: ``live`` (the bot's client in this process, gateway
request-guild-members with a query), ``rpc`` (the same in the gateway), ``rest`` (Discord's member search with
the profile's bot token, server side only; whether Discord answers it without the Members intent is not
verified, so a refusal simply ends the chain), then nothing: the page offers ID entry.

Throttle (§4.3): queries of 2–32 characters, at most 25 results, answers cached 60 s per (server, prefix);
Discord requests at most once per second per server and 20 per minute per bot. Results are never stored; only
the id the operator picks ends up in the configuration. ``settings.member_search: off`` switches all of it off.
"""

from __future__ import annotations

import json
import re
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Optional

from .. import snapshot
from ..store.schema import is_snowflake
from . import remote

API = "https://discord.com/api/v10"
MIN_QUERY, MAX_QUERY = 2, 32
LIMIT = 25
CACHE_TTL = 60.0
REST_TIMEOUT = 5.0
_CONTROL = re.compile(r"[\x00-\x1f\x7f]")

_lock = threading.Lock()
_cache: dict[tuple, tuple[float, Any]] = {}
_token_app: dict[str, tuple[float, Optional[str]]] = {}     # token digest -> (valid until, application id)
TOKEN_CHECK_TTL = 600.0
TOKEN_RETRY_AFTER = 60.0                                       # after a network failure
USERS_REST_BUDGET = 10.0                                       # seconds for all REST name lookups of one request
REST_THROTTLE = snapshot.Throttle()


class Unavailable(RuntimeError):
    """No strategy could answer; ``str()`` is the reason shown next to the ID field."""


def check_query(query: Any) -> str:
    text = str(query or "").strip()
    if not MIN_QUERY <= len(text) <= MAX_QUERY or _CONTROL.search(text):
        raise ValueError(f"Type {MIN_QUERY} to {MAX_QUERY} characters of a name.")
    return text


def _cached(key: tuple) -> Any:
    with _lock:
        hit = _cache.get(key)
        if hit is not None and time.monotonic() - hit[0] < CACHE_TTL:
            return hit[1]
    return None


def _store(key: tuple, value: Any) -> None:
    with _lock:
        _cache[key] = (time.monotonic(), value)
        if len(_cache) > 1024:
            _cache.pop(next(iter(_cache)))


def _rest(path: str, token: str) -> Any:
    request = urllib.request.Request(API + path, headers={"Authorization": f"Bot {token}",
                                                          "User-Agent": "DiscordBot (hermes-plugin, 3.0.0)"})
    with urllib.request.urlopen(request, timeout=REST_TIMEOUT) as response:  # noqa: S310 - fixed https host
        return json.loads(response.read().decode("utf-8"))


def _token_for(bot: str, hermes: Any) -> Optional[str]:
    """The profile's bot token, only if it belongs to ``bot`` (checked once per 10 minutes)."""
    token = hermes.discord_bot_token()
    if not token:
        return None
    digest = str(hash(token))
    now = time.monotonic()
    with _lock:
        known = _token_app.get(digest)
    if known is None or now >= known[0]:
        try:
            app = str((_rest("/oauth2/applications/@me", token) or {}).get("id") or "")
            known = (now + TOKEN_CHECK_TTL, app)
        except (urllib.error.URLError, OSError, ValueError):
            known = (now + TOKEN_RETRY_AFTER, None)            # unknown, not "another bot": ask again soon
        with _lock:
            _token_app[digest] = known
    return token if known[1] == bot else None


def _row(member: Any) -> Optional[dict]:
    user = (member or {}).get("user") if isinstance(member, dict) else None
    if not isinstance(user, dict) or not is_snowflake(str(user.get("id"))):
        return None
    name = str(user.get("username") or "")
    return {"id": str(user["id"]), "name": name,
            "display_name": str(member.get("nick") or user.get("global_name") or name), "avatar": None}


def search(home: Any, bot: str, guild: str, query: str, *, process: str, mode: str, hermes: Any) -> dict:
    """``{source, results}``; raises :class:`Unavailable` (ID entry) or ``ValueError`` (bad query)."""
    if mode == "off":
        raise Unavailable("Member search is switched off (settings.member_search). Paste a user ID instead.")
    query = check_query(query)
    key = (str(home), bot, guild, query.lower())
    hit = _cached(key)
    if hit is not None:
        return hit
    try:
        found = snapshot.live_members(home, bot, guild, query) if process == "gateway" else None
    except snapshot.BusyError:
        raise Unavailable("Searching too fast; wait a moment.")
    except Exception:
        found = None
    if found is not None:
        result = {"source": "live", "results": found[:LIMIT]}
        _store(key, result)
        return result
    if process != "gateway":
        try:
            data = remote.call(home, "members.search", {"bot": bot, "guild": guild, "q": query}, cache=False)
        except remote.Unreachable:
            data = None
        if isinstance(data, dict) and data.get("off"):
            raise Unavailable("Member search is switched off (settings.member_search). Paste a user ID instead.")
        if isinstance(data, dict) and data.get("busy"):
            raise Unavailable("Searching too fast; wait a moment.")
        if isinstance(data, dict) and isinstance(data.get("results"), list):
            result = {"source": "rpc", "results": data["results"][:LIMIT]}
            _store(key, result)
            return result
    token = _token_for(bot, hermes)
    if token:
        if not REST_THROTTLE.allow(bot, guild):
            raise Unavailable("Searching too fast; wait a moment.")
        try:
            rows = _rest(f"/guilds/{guild}/members/search?" + urllib.parse.urlencode({"query": query, "limit": LIMIT}),
                         token)
        except (urllib.error.URLError, OSError, ValueError):
            rows = None
        if isinstance(rows, list):
            result = {"source": "rest", "results": [r for r in (_row(m) for m in rows) if r][:LIMIT]}
            _store(key, result)
            return result
    raise Unavailable("Member search is not available right now (the bot is offline or Discord refused). "
                      "Paste a user ID instead.")


def users(home: Any, bot: str, ids: list, *, process: str, hermes: Any, mode: str = "auto") -> dict:
    """``{id: {name, display_name}}`` for the ids any strategy could name (unknown ids are left out).
    ``member_search: off`` switches name lookups off as well: the page then shows the ids."""
    if mode == "off":
        return {}
    wanted = [i for i in dict.fromkeys(str(i) for i in ids) if is_snowflake(i)][:LIMIT]
    if not wanted:
        return {}
    key = (str(home), bot, "users", ",".join(sorted(wanted)))
    hit = _cached(key)
    if hit is not None:
        return hit
    found = None
    if process == "gateway":
        try:
            found = snapshot.live_users(home, bot, wanted)
        except Exception:
            found = None
    else:
        try:
            data = remote.call(home, "users.resolve", {"bot": bot, "ids": wanted})
            found = data.get("users") if isinstance(data, dict) else None
        except remote.Unreachable:
            found = None
    if found is None:
        token = _token_for(bot, hermes)
        if token:
            found = {}
            deadline = time.monotonic() + USERS_REST_BUDGET
            for ident in wanted[:10]:
                if time.monotonic() > deadline:
                    break
                try:
                    user = _rest(f"/users/{ident}", token)
                except urllib.error.HTTPError as exc:
                    if exc.code == 429:
                        break                   # rate limited: the rest would only run into the limit
                    continue                    # Discord answered (unknown id): try the next one
                except ValueError:
                    continue
                except (urllib.error.URLError, OSError):
                    break                       # Discord unreachable: one timeout, not ten
                if isinstance(user, dict) and str(user.get("id")) == ident:
                    found[ident] = {"name": str(user.get("username") or ""),
                                    "display_name": str(user.get("global_name") or user.get("username") or "")}
    result = found or {}
    _store(key, result)
    return result


def reset() -> None:
    with _lock:
        _cache.clear()
        _token_app.clear()
