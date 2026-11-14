"""Where the settings page's suggestions for a server come from (SETTINGS-PAGE §4.2, §4.5).

Suggestions are comfort, never authority: what is saved is always an id, checked by the store. Each lookup
names its source, in this order:

1. ``live`` — the discord.py cache of the bot, when this process is the gateway that runs it;
2. ``rpc`` — the same data computed in the gateway, over Hermes' plugin RPC (Hermes ≥ 04846730f3);
3. ``snapshot`` — the directory the gateway writes to ``plugin-data/ctx_discord_policy/directory/`` (any
   process, any Hermes); older than :data:`STALE_AFTER` counts as stale (bot offline);
4. ``none`` — ids only.

The providers for 1 and 2 are registered by the gateway-side modules (``dsp.snapshot``, ``dsp.rpc``); this
module only reads. File format (written by ``dsp.snapshot``, schema 1)::

    directory/<bot>/index.json   {schema, bot, bot_name, written_at, reason, guilds: [{id, name, icon}]}
    directory/<bot>/<guild>.json {schema, guild, name, icon, written_at,
                                  channels: [{id, name, kind, parent_id, position, can_view, can_send}],
                                  roles: [{id, name, color, position, managed}],
                                  emojis: [{id, name, animated, available}]}
"""

from __future__ import annotations

import json
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Callable, Optional

from .. import hermes
from ..store.schema import is_snowflake

SCHEMA = 1
STALE_AFTER = 15 * 60          # seconds; the gateway rewrites the directory at least every 10 minutes


@dataclass
class GuildDirectory:
    source: str = "none"       # live | rpc | snapshot | none
    at: Optional[float] = None
    stale: bool = False
    name: Optional[str] = None
    icon: Optional[str] = None
    channels: list = field(default_factory=list)
    roles: list = field(default_factory=list)
    emojis: list = field(default_factory=list)

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)

    def meta(self) -> dict[str, Any]:
        return {"source": self.source, "at": self.at, "stale": self.stale}

    @property
    def known(self) -> bool:
        return self.source != "none"

    def channel_kinds(self) -> dict[str, str]:
        return {str(c.get("id")): str(c.get("kind") or "") for c in self.channels if isinstance(c, dict)}

    def role_ids(self) -> frozenset:
        return frozenset(str(r.get("id")) for r in self.roles if isinstance(r, dict))

    def emoji_ids(self) -> frozenset:
        return frozenset(str(e.get("id")) for e in self.emojis if isinstance(e, dict))

    def label(self) -> str:
        if self.source == "live":
            return "live"
        if self.source == "rpc":
            return "live (gateway)"
        if self.source == "snapshot" and self.at:
            return "directory of " + time.strftime("%Y-%m-%d %H:%M UTC", time.gmtime(self.at))
        return "IDs only"


@dataclass
class BotDirectory:
    source: str = "none"
    at: Optional[float] = None
    stale: bool = False
    bot_name: Optional[str] = None
    guilds: list = field(default_factory=list)      # [{id, name, icon}]

    def meta(self) -> dict[str, Any]:
        return {"source": self.source, "at": self.at, "stale": self.stale}

    def names(self) -> dict[str, str]:
        return {str(g.get("id")): str(g.get("name") or "") for g in self.guilds if isinstance(g, dict)}


# Providers ahead of the snapshot (live, rpc). Each: (home, bot, guild_or_None) -> GuildDirectory|BotDirectory|None.
PROVIDERS: list[tuple[str, Callable[..., Any]]] = []


def register_provider(name: str, provider: Callable[..., Any]) -> None:
    """Add (or replace) a source tried before the snapshot; called by the gateway-side modules."""
    global PROVIDERS
    PROVIDERS = [(n, p) for n, p in PROVIDERS if n != name] + [(name, provider)]


def directory_root(home: Path) -> Path:
    return hermes.plugin_data_dir(home) / "directory"


def _read(path: Path) -> Optional[dict]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return data if isinstance(data, dict) and data.get("schema") == SCHEMA else None


def _stale(at: Any) -> bool:
    return not isinstance(at, (int, float)) or time.time() - float(at) > STALE_AFTER


def _from_providers(home: Path, bot: str, guild: Optional[str], *, allow_remote: bool) -> Any:
    for name, provider in PROVIDERS:
        if name == "rpc" and not allow_remote:
            continue
        try:
            found = provider(home, bot, guild)
        except Exception:  # a failing source falls through to the next one
            found = None
        if found is not None:
            return found
    return None


def bot_directory(home: Path, bot: str, *, allow_remote: bool = True) -> BotDirectory:
    if not is_snowflake(str(bot)):
        return BotDirectory()
    found = _from_providers(home, str(bot), None, allow_remote=allow_remote)
    if isinstance(found, BotDirectory):
        return found
    data = _read(directory_root(home) / str(bot) / "index.json")
    if data is None:
        return BotDirectory()
    guilds = [g for g in data.get("guilds") or [] if isinstance(g, dict) and is_snowflake(str(g.get("id")))]
    at = data.get("written_at")
    return BotDirectory("snapshot", at if isinstance(at, (int, float)) else None, _stale(at),
                        data.get("bot_name") if isinstance(data.get("bot_name"), str) else None, guilds)


def guild_directory(home: Path, bot: str, guild: str, *, allow_remote: bool = True) -> GuildDirectory:
    if not (is_snowflake(str(bot)) and is_snowflake(str(guild))):
        return GuildDirectory()
    found = _from_providers(home, str(bot), str(guild), allow_remote=allow_remote)
    if isinstance(found, GuildDirectory):
        return found
    data = _read(directory_root(home) / str(bot) / f"{guild}.json")
    if data is None or str(data.get("guild")) != str(guild):
        return GuildDirectory()
    at = data.get("written_at")

    def rows(key: str) -> list:
        return [r for r in data.get(key) or [] if isinstance(r, dict) and is_snowflake(str(r.get("id")))]

    return GuildDirectory("snapshot", at if isinstance(at, (int, float)) else None, _stale(at),
                          data.get("name") if isinstance(data.get("name"), str) else None,
                          data.get("icon") if isinstance(data.get("icon"), str) else None,
                          rows("channels"), rows("roles"), rows("emojis"))


def snapshot_bots(home: Path) -> list[str]:
    """Bots that have a directory in this home (the gateway wrote one: the bot is this profile's)."""
    root = directory_root(home)
    try:
        return sorted(p.name for p in root.iterdir() if p.is_dir() and is_snowflake(p.name))
    except OSError:
        return []
