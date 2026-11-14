"""Plugin entries in Hermes' ``profile_routes`` (pure functions; no Hermes import).

A plugin entry is a guild route named ``ctx_discord_policy:<application_id>:<guild_id>``. Everything
else in the table is a *core entry*: the plugin never changes it, except when the operator explicitly
takes over a hand-written guild route of the same server (rename only, PROFILE-PER-GUILD §4.7).
"""

from __future__ import annotations

from typing import Any, Iterable, Optional

PREFIX = "ctx_discord_policy"
GUILD_SPECIFICITY = 2           # Hermes: user 16, thread 8, chat 4, guild 2


def entry_name(bot_id: str, guild_id: str) -> str:
    return f"{PREFIX}:{bot_id}:{guild_id}"


def _get(entry: Any, key: str) -> Any:
    return entry.get(key) if isinstance(entry, dict) else getattr(entry, key, None)


def _text(value: Any) -> Optional[str]:
    if value is None:
        return None
    text = str(value).strip()
    return text or None


def bot_profile_key(name: Optional[str]) -> Optional[str]:
    """Hermes' key for a route's bot: ``None`` for the default profile's bot (``_bot_profile_key``)."""
    name = (name or "").strip()
    return None if not name or name == "default" else name


def is_plugin_entry(entry: Any) -> bool:
    name = _get(entry, "name")
    return isinstance(name, str) and name.startswith(PREFIX + ":")


def is_own_entry(entry: Any, bot_id: str, guild_id: str) -> bool:
    return _get(entry, "name") == entry_name(bot_id, guild_id)


def make_entry(bot_id: str, guild_id: str, profile: str, owner_profile: Optional[str]) -> dict[str, Any]:
    entry = {"name": entry_name(bot_id, guild_id), "platform": "discord", "guild_id": str(guild_id),
             "profile": profile}
    owner = bot_profile_key(owner_profile)
    if owner is not None:
        entry["bot_profile"] = owner
    return entry


def _enabled(entry: Any) -> bool:
    value = _get(entry, "enabled")
    return value is None or value is True


def specificity(entry: Any) -> int:
    return (2 * bool(_text(_get(entry, "guild_id"))) + 4 * bool(_text(_get(entry, "chat_id")))
            + 8 * bool(_text(_get(entry, "thread_id"))) + 16 * bool(_text(_get(entry, "user_id"))))


def _same_bot(entry: Any, owner_profile: Optional[str]) -> bool:
    return bot_profile_key(_get(entry, "bot_profile")) == bot_profile_key(owner_profile)


def _in_guild(entry: Any, guild_id: str) -> bool:
    return (str(_get(entry, "platform") or "") == "discord" and _text(_get(entry, "guild_id")) == str(guild_id)
            and _enabled(entry))


def core_guild_entries(entries: Iterable[Any], guild_id: str, owner_profile: Optional[str]) -> list:
    """Hand-written routes of exactly this server and bot at guild level (same specificity as ours)."""
    return [e for e in entries if not is_plugin_entry(e) and _in_guild(e, guild_id) and _same_bot(e, owner_profile)
            and specificity(e) == GUILD_SPECIFICITY]


def core_exceptions(entries: Iterable[Any], guild_id: str, owner_profile: Optional[str]) -> list:
    """Hand-written routes inside this server that are more specific (channel, thread, user): they win."""
    return [e for e in entries if not is_plugin_entry(e) and _in_guild(e, guild_id) and _same_bot(e, owner_profile)
            and specificity(e) > GUILD_SPECIFICITY]


def own_entries(entries: Iterable[Any], bot_id: str, guild_id: str) -> list:
    return [e for e in entries if is_own_entry(e, bot_id, guild_id)]


def with_profile(entries: list[dict[str, Any]], bot_id: str, guild_id: str, profile: Optional[str],
                 owner_profile: Optional[str]) -> list[dict[str, Any]]:
    """``entries`` with this server's plugin entry set to ``profile`` (removed for ``None``).

    Only the entry named for (bot, guild) is touched; core entries and other servers' entries keep their
    position and content."""
    out, placed = [], False
    for entry in entries:
        if is_own_entry(entry, bot_id, guild_id):
            if profile is not None and not placed:
                out.append(make_entry(bot_id, guild_id, profile, owner_profile))
                placed = True
            continue
        out.append(entry)
    if profile is not None and not placed:
        out.append(make_entry(bot_id, guild_id, profile, owner_profile))
    return out


def adopted(entries: list[dict[str, Any]], core_name: str, bot_id: str, guild_id: str,
            owner_profile: Optional[str]) -> tuple[list[dict[str, Any]], str]:
    """``entries`` with the core guild route ``core_name`` renamed to this server's plugin entry.

    Profile and discriminators stay as written (the parsed table is equal up to the name). Returns the new
    entries and the adopted profile; raises LookupError when no such core guild route exists."""
    candidates = [e for e in core_guild_entries(entries, guild_id, owner_profile) if _get(e, "name") == core_name]
    if len(candidates) != 1:
        raise LookupError(f"no hand-written route {core_name!r} for server {guild_id}")
    target = candidates[0]
    out = []
    for entry in entries:
        if entry is target:
            renamed = dict(entry)
            renamed["name"] = entry_name(bot_id, guild_id)
            out.append(renamed)
        elif not is_own_entry(entry, bot_id, guild_id):
            out.append(entry)
    return out, str(_get(target, "profile"))


def snippet_yaml(entry: dict[str, Any]) -> str:
    """The plugin entry as a ``profile_routes`` YAML item (for a managed configuration)."""
    lines = ["profile_routes:"]
    first = True
    for key in ("name", "platform", "guild_id", "profile", "bot_profile"):
        if key not in entry:
            continue
        value = str(entry[key]).replace('"', "")
        lines.append(f"  {'- ' if first else '  '}{key}: \"{value}\"")
        first = False
    return "\n".join(lines) + "\n"
