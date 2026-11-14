"""Server directory of a connected bot: live in the gateway, as files everywhere else (SETTINGS-PAGE §4.2, P2).

The settings page backend often runs in another process than the Discord client (a standalone
``hermes dashboard``, ``hermes serve``, a plugin-host child). So the gateway, where the bot runs, keeps a
directory of its servers — names, channels, roles, emojis, never members — in
``<owner home>/plugin-data/ctx_discord_policy/directory/`` (format: ``dsp.web.directory``), written when the
bot is bound (after ``ready``), on server join/leave and channel/role/emoji/server changes (debounced), and
every :data:`HEARTBEAT` seconds. In the gateway process itself the backend reads the discord.py cache live.

Seam for the integrator: one entry in ``bootstrap.BIND_STEPS`` (runs after the bot is bound)::

    ("P2", snapshot.bind_step),

which calls ``attach(native, adapter, bot_id=binding.bot_id, owner_home=binding.owner_home,
store=binding.store)``.

``attach`` never raises; its outcome is intervention ``P2`` (comfort class) in the status register.
Every read of the client cache and every Discord request runs on the client's own event loop.
"""

from __future__ import annotations

import asyncio
import collections
import logging
import threading
import time
import weakref
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Optional

from . import hermes
from .adapt.status import COMFORT, Outcome, Result, write_json_atomic
from .runtime import RUNTIME
from .store.schema import is_snowflake
from .web import directory

logger = logging.getLogger("ctx_discord_policy")

HEARTBEAT = 600.0              # seconds between full rewrites
DEBOUNCE = 5.0                 # seconds a change waits for more changes of the same server
DETAIL_EXTRA = 200             # detail files for configured servers plus this many others
MAX_CHANNELS = 1000
CALL_TIMEOUT = 3.0             # seconds for a cache read on the client loop
SEARCH_TIMEOUT = 6.0           # seconds for a member search on the client loop
USERS_TIMEOUT = 6.0            # seconds for one name lookup request (all ids together)
FETCH_LIMIT = 10               # users fetched from Discord per lookup; the rest only from the cache
FETCH_TIMEOUT = 2.0
LIMIT = 25
EVENTS = ("on_ready", "on_guild_join", "on_guild_remove", "on_guild_update", "on_guild_channel_create",
          "on_guild_channel_delete", "on_guild_channel_update", "on_guild_role_create", "on_guild_role_delete",
          "on_guild_role_update", "on_guild_emojis_update")


# ── reading discord.py objects (also used by the RPC handlers) ─────────────────────────────────────


def _kind(channel: Any) -> str:
    kind = getattr(channel, "type", None)
    return str(getattr(kind, "name", kind) or "")


def _icon(guild: Any) -> Optional[str]:
    icon = getattr(guild, "icon", None)
    key = getattr(icon, "key", None)
    return str(key) if key else None


def channel_rows(guild: Any) -> list[dict[str, Any]]:
    me = getattr(guild, "me", None)
    rows: list[dict[str, Any]] = []
    sources = [(c, False) for c in list(getattr(guild, "channels", None) or [])]
    sources += [(t, True) for t in list(getattr(guild, "threads", None) or [])]
    for channel, is_thread in sources[:MAX_CHANNELS]:
        ident = getattr(channel, "id", None)
        if ident is None:
            continue
        perms = None
        check = getattr(channel, "permissions_for", None)
        if me is not None and callable(check):
            try:
                perms = check(me)
            except Exception:
                perms = None
        parent = getattr(channel, "parent_id", None) if is_thread else getattr(channel, "category_id", None)
        rows.append({"id": str(ident), "name": str(getattr(channel, "name", "") or ""), "kind": _kind(channel),
                     "parent_id": str(parent) if parent else None,
                     "position": int(getattr(channel, "position", 0) or 0),
                     "can_view": bool(getattr(perms, "view_channel", False)) if perms is not None else None,
                     "can_send": bool(getattr(perms, "send_messages", False)) if perms is not None else None})
    return rows


def role_rows(guild: Any) -> list[dict[str, Any]]:
    rows = []
    for role in list(getattr(guild, "roles", None) or []):
        default = getattr(role, "is_default", None)
        if callable(default) and default():
            continue   # @everyone cannot restrict anything
        color = getattr(role, "color", None)
        rows.append({"id": str(role.id), "name": str(getattr(role, "name", "") or ""),
                     "color": int(getattr(color, "value", 0) or 0), "position": int(getattr(role, "position", 0) or 0),
                     "managed": bool(getattr(role, "managed", False))})
    rows.sort(key=lambda r: -r["position"])
    return rows


def emoji_rows(guild: Any) -> list[dict[str, Any]]:
    return [{"id": str(e.id), "name": str(getattr(e, "name", "") or ""), "animated": bool(getattr(e, "animated", False)),
             "available": bool(getattr(e, "available", True))} for e in list(getattr(guild, "emojis", None) or [])]


def guild_payload(guild: Any, now: Optional[float] = None) -> dict[str, Any]:
    return {"schema": directory.SCHEMA, "guild": str(guild.id), "name": str(getattr(guild, "name", "") or ""),
            "icon": _icon(guild), "written_at": time.time() if now is None else now,
            "channels": channel_rows(guild), "roles": role_rows(guild), "emojis": emoji_rows(guild)}


def index_payload(client: Any, bot_id: str, reason: str, now: Optional[float] = None) -> dict[str, Any]:
    user = getattr(client, "user", None)
    return {"schema": directory.SCHEMA, "bot": bot_id,
            "bot_name": str(getattr(user, "display_name", None) or getattr(user, "name", "") or "") or None,
            "written_at": time.time() if now is None else now, "reason": reason,
            "guilds": [{"id": str(g.id), "name": str(getattr(g, "name", "") or ""), "icon": _icon(g)}
                       for g in list(getattr(client, "guilds", None) or [])]}


# ── live bots of this process ─────────────────────────────────────────────────────────────────────


@dataclass
class LiveBot:
    bot_id: str
    owner_home: Path
    owner_key: str
    client_ref: Any
    store: Any = None
    pending: dict = field(default_factory=dict)          # guild id (or "") -> TimerHandle
    heartbeat: Any = None
    listeners_ok: bool = True

    @property
    def client(self) -> Any:
        return self.client_ref()


class Throttle:
    """At most one Discord member search per second per server and 20 per minute per bot (Discord announces a
    rate limit for these requests; SETTINGS-PAGE §4.3)."""

    def __init__(self, per_guild: float = 1.0, per_bot_minute: int = 20) -> None:
        self._lock = threading.Lock()
        self._guild: dict[tuple, float] = {}
        self._bot: dict[str, collections.deque] = {}
        self.per_guild, self.per_bot_minute = per_guild, per_bot_minute

    def allow(self, bot: str, guild: str, now: Optional[float] = None) -> bool:
        now = time.monotonic() if now is None else now
        with self._lock:
            if now - self._guild.get((bot, guild), -1e9) < self.per_guild:
                return False
            window = self._bot.setdefault(bot, collections.deque())
            while window and now - window[0] > 60.0:
                window.popleft()
            if len(window) >= self.per_bot_minute:
                return False
            self._guild[(bot, guild)] = now
            window.append(now)
            return True


LIVE: dict[str, LiveBot] = {}
_LIVE_LOCK = threading.Lock()
THROTTLE = Throttle()


def live_bot(home: Any, bot_id: str) -> Optional[LiveBot]:
    with _LIVE_LOCK:
        entry = LIVE.get(str(bot_id))
    if entry is None or entry.client is None or entry.owner_key != hermes.home_key(home) or _closed(entry.client):
        return None
    return entry


def _loop_of(client: Any) -> Any:
    try:
        loop = getattr(client, "loop", None)
    except Exception:
        return None
    return loop if isinstance(loop, asyncio.AbstractEventLoop) and loop.is_running() else None


def _on_loop(client: Any, make: Any, timeout: float) -> Any:
    """Run ``make()`` (a coroutine factory) on the client's loop and wait for it, from any thread."""
    loop = _loop_of(client)
    if loop is None:
        raise RuntimeError("the bot's event loop is not running")
    try:
        running = asyncio.get_running_loop()
    except RuntimeError:
        running = None
    if running is loop:
        raise RuntimeError("called on the bot's own event loop; use the async variant")
    future = asyncio.run_coroutine_threadsafe(make(), loop)
    try:
        return future.result(timeout)
    except BaseException:
        future.cancel()            # a late answer is not wanted: stop the work on the bot's loop as well
        raise


def _ready(client: Any) -> bool:
    check = getattr(client, "is_ready", None)
    try:
        return bool(check()) if callable(check) else True
    except Exception:
        return False


def _closed(client: Any) -> bool:
    """discord.py keeps its cache after ``close()``; a closed client's view of its servers is not current."""
    check = getattr(client, "is_closed", None)
    try:
        return bool(check()) if callable(check) else False
    except Exception:
        return True


def live_guild(home: Any, bot_id: str, guild_id: str) -> Optional[directory.GuildDirectory]:
    entry = live_bot(home, bot_id)
    if entry is None or not _ready(entry.client):
        return None
    client = entry.client

    async def read() -> Optional[dict]:
        guild = client.get_guild(int(guild_id))
        return guild_payload(guild) if guild is not None else None

    data = _on_loop(client, read, CALL_TIMEOUT)
    if data is None:
        return None
    return directory.GuildDirectory("live", data["written_at"], False, data["name"], data["icon"], data["channels"],
                                    data["roles"], data["emojis"])


def live_index(home: Any, bot_id: str) -> Optional[directory.BotDirectory]:
    entry = live_bot(home, bot_id)
    if entry is None or not _ready(entry.client):
        return None
    client = entry.client

    async def read() -> dict:
        return index_payload(client, entry.bot_id, "live")

    data = _on_loop(client, read, CALL_TIMEOUT)
    return directory.BotDirectory("live", data["written_at"], False, data["bot_name"], data["guilds"])


def _provider(home: Any, bot_id: str, guild_id: Optional[str]) -> Any:
    return live_index(home, bot_id) if guild_id is None else live_guild(home, bot_id, guild_id)


directory.register_provider("live", _provider, priority=10)


def _member_row(member: Any) -> dict[str, Any]:
    avatar = getattr(getattr(member, "display_avatar", None), "url", None)
    return {"id": str(member.id), "name": str(getattr(member, "name", "") or ""),
            "display_name": str(getattr(member, "display_name", "") or getattr(member, "name", "") or ""),
            "avatar": str(avatar) if avatar else None}


def live_members(home: Any, bot_id: str, guild_id: str, query: str) -> Optional[list]:
    """Prefix search for members (Gateway request-guild-members with a query: no Members intent needed for a
    prefix; at most :data:`LIMIT`). ``None`` when this process does not run the bot."""
    entry = live_bot(home, bot_id)
    if entry is None or not _ready(entry.client):
        return None
    client = entry.client
    if not THROTTLE.allow(str(bot_id), str(guild_id)):
        raise BusyError("member search is rate limited; try again in a moment")

    async def search() -> list:
        guild = client.get_guild(int(guild_id))
        if guild is None:
            return []
        members = await guild.query_members(query=query, limit=LIMIT, cache=False)
        return [_member_row(m) for m in members]

    return _on_loop(client, search, SEARCH_TIMEOUT)


def live_users(home: Any, bot_id: str, ids: Iterable[str]) -> Optional[dict]:
    entry = live_bot(home, bot_id)
    if entry is None or not _ready(entry.client):
        return None
    client = entry.client
    wanted = [i for i in ids if is_snowflake(str(i))][:LIMIT]

    async def resolve() -> dict:
        out = {}
        fetched = 0
        for ident in wanted:
            user = client.get_user(int(ident))
            if user is None and fetched < FETCH_LIMIT:
                fetched += 1
                try:
                    user = await asyncio.wait_for(client.fetch_user(int(ident)), FETCH_TIMEOUT)
                except Exception:
                    user = None
            if user is not None:
                out[str(ident)] = {"name": str(getattr(user, "name", "") or ""),
                                   "display_name": str(getattr(user, "display_name", "") or "")}
        return out

    return _on_loop(client, resolve, USERS_TIMEOUT)


class BusyError(RuntimeError):
    """A Discord request refused by our own throttle."""


# ── writing the directory files ────────────────────────────────────────────────────────────────────


def _configured(store: Any, bot_id: str) -> set:
    try:
        return set(((store.read().data.get("bots") or {}).get(bot_id) or {}).get("guilds", {}))
    except Exception:
        return set()


def write_files(root: Path, index: dict, details: list, *, keep: Iterable[str]) -> None:
    """Write ``index.json`` and the detail files; remove detail files of servers no longer kept."""
    root.mkdir(parents=True, exist_ok=True)
    for payload in details:
        write_json_atomic(root / f"{payload['guild']}.json", payload)
    write_json_atomic(root / "index.json", index)
    keep = set(keep)
    try:
        for path in root.iterdir():
            if path.suffix == ".json" and path.stem != "index" and path.stem not in keep:
                path.unlink(missing_ok=True)
    except OSError:
        logger.debug("ctx_discord_policy: directory cleanup failed", exc_info=True)


async def refresh(entry: LiveBot, reason: str, guild_id: Optional[str] = None) -> None:
    """Rewrite the index and the detail files (one server when ``guild_id`` is given). Runs on the client loop;
    the file writes go to a worker thread."""
    client = entry.client
    if client is None or _closed(client) or not _ready(client):
        return          # disconnected: the files keep their age, so the page shows the bot as offline
    try:
        root = directory.directory_root(entry.owner_home) / entry.bot_id
        now = time.time()
        index = index_payload(client, entry.bot_id, reason, now)
        guilds = list(getattr(client, "guilds", None) or [])
        configured = _configured(entry.store, entry.bot_id) if entry.store is not None else set()
        keep = [str(g.id) for g in guilds if str(g.id) in configured]
        keep += [str(g.id) for g in guilds if str(g.id) not in configured][:DETAIL_EXTRA]
        chosen = [g for g in guilds if str(g.id) in keep and (guild_id is None or str(g.id) == str(guild_id))]
        details = [guild_payload(g, now) for g in chosen]
        await asyncio.get_running_loop().run_in_executor(None, lambda: write_files(root, index, details, keep=keep))
    except Exception as exc:
        RUNTIME.status.record(Result("P2", COMFORT, Outcome.FALLBACK,
                                     f"directory not written: {type(exc).__name__}", "snapshot.refresh"))
        raise
    previous = RUNTIME.status.get("P2")
    if entry.listeners_ok and previous is not None and previous.outcome is Outcome.FALLBACK:
        RUNTIME.status.record(Result("P2", COMFORT, Outcome.OK, f"directory of bot {entry.bot_id} written again",
                                     "snapshot.refresh"))


def _schedule(entry: LiveBot, reason: str, guild_id: Optional[str] = None) -> None:
    loop = _loop_of(entry.client)
    if loop is None:
        return
    key = str(guild_id or "")
    previous = entry.pending.pop(key, None)
    if previous is not None:
        previous.cancel()

    def fire() -> None:
        entry.pending.pop(key, None)
        task = loop.create_task(refresh(entry, reason, guild_id))
        task.add_done_callback(_log_failure)

    entry.pending[key] = loop.call_later(DEBOUNCE, fire)


def _log_failure(task: Any) -> None:
    """Done callback of a scheduled write (``refresh`` records the outcome itself)."""
    if not task.cancelled() and task.exception() is not None:
        logger.warning("ctx_discord_policy: writing the server directory failed: %s", task.exception())


def _listener(entry: LiveBot, event: str) -> Any:
    async def on_event(*args: Any) -> None:
        guild = None
        for arg in args:
            guild = getattr(arg, "guild", None) or (arg if hasattr(arg, "channels") else None)
            if guild is not None:
                break
        if event in ("on_ready", "on_guild_join", "on_guild_remove"):
            _schedule(entry, event[3:])          # the server list changed: everything
        else:
            _schedule(entry, event[3:], str(getattr(guild, "id", "")) or None)

    on_event.__name__ = f"dsp_directory_{event}"
    return on_event


async def _heartbeat(entry: LiveBot) -> None:
    while entry.client is not None:
        await asyncio.sleep(HEARTBEAT)
        try:
            await refresh(entry, "heartbeat")
        except Exception as exc:
            logger.warning("ctx_discord_policy: directory heartbeat failed: %s", exc)


def attach(native: Any, adapter: Any = None, *, bot_id: Any, owner_home: Any, store: Any = None,
           schedule: bool = True) -> Optional[LiveBot]:
    """Keep a directory of this bot's servers (live in this process, files for every other one)."""
    bot = str(bot_id or "")
    if not is_snowflake(bot) or native is None:
        RUNTIME.status.record(Result("P2", COMFORT, Outcome.OFF, "no bot id or client", "snapshot.attach"))
        return None
    add_listener = getattr(native, "add_listener", None)
    if not callable(add_listener) or not hasattr(native, "guilds") or not callable(getattr(native, "get_guild", None)):
        RUNTIME.status.record(Result("P2", COMFORT, Outcome.OFF,
                                     "client has no add_listener/guilds/get_guild: the settings page offers ID entry only",
                                     "discord.Client probe"))
        return None
    try:
        client_ref = weakref.ref(native)
    except TypeError:
        client_ref = (lambda obj: (lambda: obj))(native)
    entry = LiveBot(bot, Path(owner_home), hermes.home_key(owner_home), client_ref, store)
    with _LIVE_LOCK:
        previous = LIVE.get(bot)
        LIVE[bot] = entry
    if previous is not None:
        _stop(previous)
    marks = getattr(native, "__dsp_directory_listeners__", None)
    try:
        if not isinstance(marks, dict):
            marks = {}
            setattr(native, "__dsp_directory_listeners__", marks)
        for event in EVENTS:
            if event in marks:
                native.remove_listener(marks[event], event)
            marks[event] = _listener(entry, event)
            add_listener(marks[event], event)
    except Exception as exc:
        entry.listeners_ok = False
        RUNTIME.status.record(Result("P2", COMFORT, Outcome.FALLBACK, f"listeners not registered: {exc}",
                                     "client.add_listener"))
    if schedule:
        loop = _loop_of(native)
        if loop is not None:
            def start() -> None:
                task = loop.create_task(refresh(entry, "bound"))
                task.add_done_callback(_log_failure)
                entry.heartbeat = loop.create_task(_heartbeat(entry))
            loop.call_soon_threadsafe(start)
    if entry.listeners_ok:
        RUNTIME.status.record(Result("P2", COMFORT, Outcome.OK,
                                     f"directory of bot {bot} kept (live here, files in plugin-data)", "snapshot.attach"))
    return entry


def _stop(entry: LiveBot) -> None:
    """Cancel the entry's timers and heartbeat on their own loop (callable from any thread, never raises)."""
    handles = list(entry.pending.values()) + ([entry.heartbeat] if entry.heartbeat is not None else [])
    entry.pending.clear()
    entry.heartbeat = None
    for handle in handles:
        loop = getattr(handle, "_loop", None) or (handle.get_loop() if hasattr(handle, "get_loop") else None)
        try:
            if loop is not None and not loop.is_closed():
                loop.call_soon_threadsafe(handle.cancel)
        except RuntimeError:  # the loop closed meanwhile: nothing left to cancel
            pass


def detach(bot_id: str) -> None:
    with _LIVE_LOCK:
        entry = LIVE.pop(str(bot_id), None)
    if entry is not None:
        _stop(entry)


def bind_step(bind: Any) -> None:
    """``bootstrap.BIND_STEPS`` entry: ``bind`` carries ``native``, ``adapter`` and ``binding`` (the bound bot)."""
    binding = bind.binding
    attach(bind.native, bind.adapter, bot_id=binding.bot_id, owner_home=binding.owner_home,
           store=getattr(binding, "store", None))
