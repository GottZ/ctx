"""Guild-level write operations shared by the CLI (now) and the Discord UI (later).

Every operation is a mutation for :meth:`PolicyStore.update`; inputs are validated strictly against the
schema before anything is written.
"""

from __future__ import annotations

import copy
from typing import Any, Optional

from .file import PolicyStore, Snapshot, utc_now
from .schema import SchemaError, guild_defaults, is_snowflake, validate_guild


def _require_ids(bot_id: str, guild_id: str) -> tuple[str, str]:
    bot_id, guild_id = str(bot_id), str(guild_id)
    if not is_snowflake(bot_id):
        raise SchemaError(f"bot: {bot_id!r} is not a Discord application id")
    if not is_snowflake(guild_id):
        raise SchemaError(f"guild: {guild_id!r} is not a Discord guild id")
    return bot_id, guild_id


def _guilds(document: dict[str, Any], bot_id: str) -> dict[str, Any]:
    return document["bots"].setdefault(bot_id, {"guilds": {}})["guilds"]


def bind(store: PolicyStore, bot_id: str, guild_id: str, *, by: str) -> Snapshot:
    """Activate a guild (creating it with defaults); a paused or unbound guild becomes active again."""
    bot_id, guild_id = _require_ids(bot_id, guild_id)

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guilds = _guilds(document, bot_id)
        guild = guilds.get(guild_id)
        changes = []
        if guild is None:
            guild = guilds[guild_id] = guild_defaults()
            changes.append({"bot": bot_id, "guild": guild_id, "field": "created", "old": None, "new": True})
        if guild["state"] != "active":
            changes.append({"bot": bot_id, "guild": guild_id, "field": "state", "old": guild["state"],
                            "new": "active"})
            guild["state"] = "active"
            guild["bound_by"] = by
            guild["bound_at"] = utc_now()
        return changes

    return store.update(mutate, by=by)


def set_state(store: PolicyStore, bot_id: str, guild_id: str, state: str, *, by: str,
              expected_rev: Optional[int] = None) -> Snapshot:
    if state not in ("active", "paused", "unbound"):
        raise SchemaError(f"state: {state!r} is not active, paused or unbound")
    bot_id, guild_id = _require_ids(bot_id, guild_id)

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guild = _guilds(document, bot_id).get(guild_id)
        if guild is None:
            raise SchemaError(f"guild {guild_id} is not configured for bot {bot_id}")
        if guild["state"] == state:
            return []
        old, guild["state"] = guild["state"], state
        return [{"bot": bot_id, "guild": guild_id, "field": "state", "old": old, "new": state,
                 "expected_rev": expected_rev}]

    return store.update(mutate, by=by)


def purge(store: PolicyStore, bot_id: str, guild_id: str, *, by: str) -> Snapshot:
    """Delete a guild's configuration entirely (the audit keeps hashes of what was removed)."""
    bot_id, guild_id = _require_ids(bot_id, guild_id)

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guilds = _guilds(document, bot_id)
        old = guilds.pop(guild_id, None)
        if old is None:
            return []
        if not guilds:
            document["bots"].pop(bot_id, None)
        return [{"bot": bot_id, "guild": guild_id, "field": "purged", "old": old, "new": None}]

    return store.update(mutate, by=by)


def import_guild(store: PolicyStore, bot_id: str, guild_id: str, payload: Any, *, by: str,
                 prompt_cap: int) -> Snapshot:
    """Replace one guild's settings with ``payload`` (export format), strictly validated.

    ``rev`` and bookkeeping fields in the payload are ignored; the store owns them.
    """
    bot_id, guild_id = _require_ids(bot_id, guild_id)
    if not isinstance(payload, dict):
        raise SchemaError("import: expected a guild mapping")
    incoming = {k: v for k, v in payload.items() if k not in ("rev", "updated_by", "updated_at")}
    clean, _ = validate_guild(incoming, strict=True, prompt_cap=prompt_cap)

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guilds = _guilds(document, bot_id)
        old = guilds.get(guild_id)
        new = copy.deepcopy(clean)
        new["rev"] = (old or {}).get("rev", 0)
        changes = []
        for section in new:
            if section in ("rev", "updated_by", "updated_at"):
                continue
            before = (old or guild_defaults()).get(section)
            if before != new[section]:
                changes.append({"bot": bot_id, "guild": guild_id, "field": section, "old": before,
                                "new": new[section]})
        guilds[guild_id] = new
        return changes or [{"bot": bot_id, "guild": guild_id, "field": "import", "old": None, "new": None}]

    return store.update(mutate, by=by)


def export(snapshot: Snapshot, bot_id: Optional[str] = None, guild_id: Optional[str] = None) -> dict[str, Any]:
    bots = snapshot.data.get("bots", {})
    selected = {b: v for b, v in bots.items() if bot_id is None or b == str(bot_id)}
    if guild_id is not None:
        selected = {b: {"guilds": {g: c for g, c in v["guilds"].items() if g == str(guild_id)}}
                    for b, v in selected.items()}
    return {"schema": snapshot.data.get("schema"), "bots": copy.deepcopy(selected)}
