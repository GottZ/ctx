"""Small helpers shared by the settings panels."""

from __future__ import annotations

import re
from typing import Any, Iterable, Optional

from ...policy.routes import snowflake
from ..panels import Ctx

_SPLIT = re.compile(r"[,\n]")


def onoff(value: Any) -> str:
    return "on" if value else "off"


def parse_list(text: Any) -> list:
    """Comma- or line-separated entries, trimmed, empty ones dropped, order kept, duplicates removed."""
    out: list = []
    for part in _SPLIT.split(str(text or "")):
        item = part.strip()
        if item and item not in out:
            out.append(item)
    return out


def mentions(ids: Iterable[Any], kind: str = "#") -> str:
    prefix = {"#": "<#", "@&": "<@&"}[kind]
    return ", ".join(f"{prefix}{i}>" for i in ids or ()) or "none"


def selected_ids(item: Any) -> list:
    """Ids chosen in a channel or role select (discord.py resolves them to objects with ``id``)."""
    out = []
    for value in list(getattr(item, "values", None) or []):
        ident = snowflake(getattr(value, "id", value))
        if ident and ident not in out:
            out.append(ident)
    return out


def guild_channel_ids(ctx: Ctx) -> Optional[set]:
    """Channel ids of this server from the client cache, or None when the server is not cached."""
    guild = getattr(ctx.interaction, "guild", None)
    channels = getattr(guild, "channels", None)
    if guild is None or channels is None:
        return None
    ids = {str(c.id) for c in channels}
    ids.update(str(t.id) for t in getattr(guild, "threads", None) or [])
    return ids


def guild_role_ids(ctx: Ctx) -> Optional[set]:
    guild = getattr(ctx.interaction, "guild", None)
    roles = getattr(guild, "roles", None)
    if guild is None or roles is None:
        return None
    return {str(r.id) for r in roles}


def foreign(ids: Iterable[str], known: Optional[set]) -> list:
    """Ids that are certainly not part of this server (only decidable when the server is cached)."""
    if known is None:
        return []
    return [i for i in ids if i not in known]
