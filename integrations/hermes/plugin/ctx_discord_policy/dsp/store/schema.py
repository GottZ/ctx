"""Schema v1 of the per-guild policy state (DESIGN §5.2/§5.3) and its validation.

Deliberately absent: any field that maps to authority. Who is an operator comes only from the Hermes
configuration (``plugins.entries.ctx_discord_policy.settings.operators``); the stored state can switch
catalog lanes on or off and fill their parameter lists, nothing more. Unknown keys are refused on every
write path and dropped (with a reported problem) when a file is read.
"""

from __future__ import annotations

import copy
import re
from typing import Any, Callable

SCHEMA_VERSION = 1

STATES = ("unbound", "active", "paused")
GROUPINGS = ("default", "rotate", "separate")
ARCHIVE_MINUTES = (60, 1440, 4320, 10080)
MANAGER_MODES = ("discord_admins", "role", "operators_only")
DEFAULT_GREETINGS = ("hey", "hi", "yo", "ok", "okay", "please")
DEFAULT_LANGUAGE = "the language of the message"
HARD_PROMPT_LIMIT = 20000          # absolute ceiling; the operator setting guild_prompt_max_chars caps lower
PROMPT_HISTORY = 10
AUDIT_RING = 500

_SNOWFLAKE = re.compile(r"[1-9][0-9]{5,19}")
_SKILL = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
# Knowledge lookups are MCP read tools by name ("mcp__<server>__get…" etc.): a state field may fill the
# catalog entry's parameter list, never name an effectful tool such as terminal.
_TOOL = re.compile(r"mcp__[A-Za-z0-9_-]{1,64}__(?:get|read|fetch|lookup|view|show)[A-Za-z0-9_]{0,48}")
_CONTROL = re.compile(r"[\x00-\x08\x0b\x0c\x0e-\x1f\x7f\x85\u2028\u2029]")
_LINE_BREAK = re.compile(r"[\n\r\x0b\x0c\x1c-\x1e\x85\u2028\u2029]")


class SchemaError(ValueError):
    """A value or key that the schema does not allow (message names the path)."""


def is_snowflake(value: Any) -> bool:
    return isinstance(value, str) and bool(_SNOWFLAKE.fullmatch(value))


def guild_defaults() -> dict[str, Any]:
    """Defaults after activation (OPEN L8): direct address only, no ambient channel, public lane limited
    to reading the current channel's history, no worklog until the operator picks a channel."""
    return {
        "state": "unbound",
        "rev": 0,
        "bound_by": None,
        "bound_at": None,
        "identity": {"bot_name": "", "wake_names": [], "greetings": list(DEFAULT_GREETINGS),
                     "reaction_emoji": None},
        "conversation": {"ambient_channels": [], "excluded_channels": [], "language": DEFAULT_LANGUAGE},
        "prompts": {"guild": "", "guild_in_operator_lane": False, "history": [], "template_overrides": {}},
        "progress": {"grouping": "default"},
        "worklog": {"channel": None, "archive_minutes": 1440, "final_to_origin": True},
        "lanes": {"channel_history": {"enabled": True, "max_limit": 100}, "permalink_hydration": True,
                  "skills": [], "knowledge": []},
        "operator_lane": {"channel_hydration": True},
        "managers": {"mode": "discord_admins", "role_ids": []},
        "audit_channel": None,
        "updated_by": None,
        "updated_at": None,
    }


def empty_document() -> dict[str, Any]:
    return {"schema": SCHEMA_VERSION, "bots": {}, "audit": []}


# ── field validators ─────────────────────────────────────────────────────────────────────────────


def _text(limit: int, *, single_line: bool = True, allow_empty: bool = True) -> Callable[[str, Any], str]:
    def check(path: str, value: Any) -> str:
        if not isinstance(value, str):
            raise SchemaError(f"{path}: expected text")
        if single_line and _LINE_BREAK.search(value):
            raise SchemaError(f"{path}: must be one line")
        if _CONTROL.search(value.replace("\n", "").replace("\r", "") if not single_line else value):
            raise SchemaError(f"{path}: control characters are not allowed")
        if len(value) > limit:
            raise SchemaError(f"{path}: longer than {limit} characters")
        if not allow_empty and not value.strip():
            raise SchemaError(f"{path}: must not be empty")
        return value
    return check


def _bool(path: str, value: Any) -> bool:
    if not isinstance(value, bool):
        raise SchemaError(f"{path}: expected true/false")
    return value


def _choice(options: tuple) -> Callable[[str, Any], Any]:
    def check(path: str, value: Any) -> Any:
        if value not in options:
            raise SchemaError(f"{path}: must be one of {', '.join(map(str, options))}")
        return value
    return check


def _snowflake(path: str, value: Any) -> str:
    if isinstance(value, int) and not isinstance(value, bool):
        value = str(value)
    if not is_snowflake(value):
        raise SchemaError(f"{path}: expected a Discord id")
    return value


def _optional(check: Callable[[str, Any], Any]) -> Callable[[str, Any], Any]:
    def wrapped(path: str, value: Any) -> Any:
        return None if value is None else check(path, value)
    return wrapped


def _list(item: Callable[[str, Any], Any], limit: int, *, unique: bool = True) -> Callable[[str, Any], list]:
    def check(path: str, value: Any) -> list:
        if not isinstance(value, list):
            raise SchemaError(f"{path}: expected a list")
        if len(value) > limit:
            raise SchemaError(f"{path}: at most {limit} entries")
        out = [item(f"{path}[{i}]", v) for i, v in enumerate(value)]
        if unique:
            seen: list = []
            for entry in out:
                if entry not in seen:
                    seen.append(entry)
            out = seen
        return out
    return check


def _int_range(low: int, high: int) -> Callable[[str, Any], int]:
    def check(path: str, value: Any) -> int:
        if isinstance(value, bool) or not isinstance(value, int) or not low <= value <= high:
            raise SchemaError(f"{path}: expected an integer {low}..{high}")
        return value
    return check


def _skill(path: str, value: Any) -> str:
    if not isinstance(value, str) or not _SKILL.fullmatch(value):
        raise SchemaError(f"{path}: not a skill name")
    return value


def _knowledge(path: str, value: Any) -> dict[str, str]:
    if not isinstance(value, dict) or set(value) != {"tool", "id"}:
        raise SchemaError(f"{path}: expected {{tool, id}}")
    tool, ident = value["tool"], value["id"]
    if not isinstance(tool, str) or not _TOOL.fullmatch(tool):
        raise SchemaError(f"{path}.tool: not an MCP read tool (mcp__<server>__get…/read…/fetch…/lookup…/view…)")
    if not isinstance(ident, str) or not ident.strip() or len(ident) > 200 or _CONTROL.search(ident):
        raise SchemaError(f"{path}.id: expected a non-empty id of at most 200 characters")
    return {"tool": tool, "id": ident}


def _emoji(path: str, value: Any) -> Any:
    if isinstance(value, str):
        if not value or len(value) > 32 or _CONTROL.search(value) or value.isascii():
            raise SchemaError(f"{path}: expected a unicode emoji")
        return value
    if isinstance(value, dict) and set(value) == {"id", "name"}:
        return {"id": _snowflake(f"{path}.id", value["id"]), "name": _text(32, allow_empty=False)(f"{path}.name",
                                                                                            value["name"])}
    raise SchemaError(f"{path}: expected a unicode emoji or {{id, name}}")


def _history(path: str, value: Any) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != {"sha256", "by", "at", "text"}:
        raise SchemaError(f"{path}: expected {{sha256, by, at, text}}")
    return {"sha256": _text(64)(f"{path}.sha256", value["sha256"]), "by": _text(64)(f"{path}.by", value["by"]),
            "at": _text(40)(f"{path}.at", value["at"]),
            "text": _text(HARD_PROMPT_LIMIT, single_line=False)(f"{path}.text", value["text"])}


def _overrides(path: str, value: Any) -> dict[str, str]:
    if not isinstance(value, dict) or len(value) > 16:
        raise SchemaError(f"{path}: expected a mapping of at most 16 templates")
    out = {}
    for key, text in value.items():
        if not isinstance(key, str) or not re.fullmatch(r"[a-z][a-z0-9_]{0,40}", key):
            raise SchemaError(f"{path}: bad template name {key!r}")
        out[key] = _text(8000, single_line=False)(f"{path}.{key}", text)
    return out


_GUILD_FIELDS: dict[str, Any] = {
    "state": _choice(STATES),
    "rev": _int_range(0, 2**62),
    "bound_by": _optional(_text(64)),
    "bound_at": _optional(_text(40)),
    "identity": {
        "bot_name": _text(32),
        "wake_names": _list(_text(32, allow_empty=False), 5),
        "greetings": _list(_text(24, allow_empty=False), 12),
        "reaction_emoji": _optional(_emoji),
    },
    "conversation": {
        "ambient_channels": _list(_snowflake, 100),
        "excluded_channels": _list(_snowflake, 100),
        "language": _text(100, allow_empty=False),
    },
    "prompts": {
        "guild": _text(HARD_PROMPT_LIMIT, single_line=False),
        "guild_in_operator_lane": _bool,
        "history": _list(_history, PROMPT_HISTORY, unique=False),
        "template_overrides": _overrides,
    },
    "progress": {"grouping": _choice(GROUPINGS)},
    "worklog": {
        "channel": _optional(_snowflake),
        "archive_minutes": _choice(ARCHIVE_MINUTES),
        "final_to_origin": _bool,
    },
    "lanes": {
        "channel_history": {"enabled": _bool, "max_limit": _int_range(1, 100)},
        "permalink_hydration": _bool,
        "skills": _list(_skill, 25),
        "knowledge": _list(_knowledge, 25),
    },
    "operator_lane": {"channel_hydration": _bool},
    "managers": {"mode": _choice(MANAGER_MODES), "role_ids": _list(_snowflake, 25)},
    "audit_channel": _optional(_snowflake),
    "updated_by": _optional(_text(64)),
    "updated_at": _optional(_text(40)),
}


def field_paths(spec: dict[str, Any] = _GUILD_FIELDS, prefix: str = "") -> list[str]:
    """Every leaf path of the guild schema (used by tests that pin the absence of authority fields)."""
    out = []
    for key, sub in spec.items():
        path = f"{prefix}{key}"
        out.extend(field_paths(sub, path + ".") if isinstance(sub, dict) else [path])
    return out


def _validate(spec: dict[str, Any], value: Any, path: str, strict: bool, problems: list[str],
              defaults: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise SchemaError(f"{path or 'guild'}: expected a mapping")
    out = copy.deepcopy(defaults)
    for key, raw in value.items():
        sub = spec.get(key)
        sub_path = f"{path}.{key}" if path else key
        if sub is None:
            if strict:
                raise SchemaError(f"{sub_path}: unknown setting")
            problems.append(f"{sub_path}: unknown setting dropped")
            continue
        if isinstance(sub, dict):
            out[key] = _validate(sub, raw, sub_path, strict, problems, defaults[key])
        else:
            try:
                out[key] = sub(sub_path, raw)
            except SchemaError as exc:
                if strict:
                    raise
                problems.append(f"{exc} (default kept)")
    return out


def validate_guild(value: Any, *, strict: bool = True, prompt_cap: int = HARD_PROMPT_LIMIT) -> tuple[dict, list]:
    """Return ``(normalised guild, problems)``. ``strict`` raises on the first problem (write paths)."""
    problems: list[str] = []
    guild = _validate(_GUILD_FIELDS, value, "", strict, problems, guild_defaults())
    cap = max(0, min(int(prompt_cap), HARD_PROMPT_LIMIT))
    if len(guild["prompts"]["guild"]) > cap:
        message = f"prompts.guild: longer than the configured limit of {cap} characters"
        if strict:
            raise SchemaError(message)
        problems.append(message + " (truncated)")
        guild["prompts"]["guild"] = guild["prompts"]["guild"][:cap]
    return guild, problems


# ── S-INT: revision epochs of deleted guilds ─────────────────────────────────────────────────────────
# ``bots.<bot>.purged`` = {guild id: revision its purge left}. A guild created again continues from there,
# so a revision a panel showed before the purge never matches again (no ABA through purge + reactivate).
PURGED_KEY = "purged"


def validate_purged(path: str, value: Any) -> dict[str, int]:
    if not isinstance(value, dict):
        raise SchemaError(f"{path}: expected a mapping of guild ids to revisions")
    out = {}
    for guild_id, rev in value.items():
        if not is_snowflake(guild_id):
            raise SchemaError(f"{path}.{guild_id}: not a Discord id")
        out[guild_id] = _GUILD_FIELDS["rev"](f"{path}.{guild_id}", rev)
    return out


def validate_document(doc: Any, *, strict: bool = True) -> tuple[dict[str, Any], list[str]]:
    """Validate a whole v1 document.

    ``strict`` (default, every read of a v1 file): any invalid value or unknown key raises, so the store
    treats the file as corrupt (all guilds paused, never rewritten) instead of silently replacing a
    restrictive value with a wider default. ``strict=False`` only for reading a newer schema (downgrade),
    whose snapshot is read-only and pauses every guild anyway."""
    if not isinstance(doc, dict):
        raise SchemaError("document: expected a mapping")
    if doc.get("schema") != SCHEMA_VERSION:
        raise SchemaError(f"document: schema {doc.get('schema')!r} is not {SCHEMA_VERSION}")
    bots = doc.get("bots", {})
    if not isinstance(bots, dict):
        raise SchemaError("bots: expected a mapping")
    problems: list[str] = []
    out = empty_document()
    for bot_id, bot in bots.items():
        if not is_snowflake(bot_id) or not isinstance(bot, dict) or not isinstance(bot.get("guilds", {}), dict):
            raise SchemaError(f"bots.{bot_id}: malformed")
        guilds = {}
        for guild_id, guild in bot.get("guilds", {}).items():
            if not is_snowflake(guild_id):
                raise SchemaError(f"bots.{bot_id}.guilds.{guild_id}: not a Discord id")
            clean, issues = validate_guild(guild, strict=strict)
            problems.extend(f"bots.{bot_id}.guilds.{guild_id}.{p}" for p in issues)
            guilds[guild_id] = clean
        extra = set(bot) - {"guilds", PURGED_KEY}
        if extra and strict:
            raise SchemaError(f"bots.{bot_id}: unknown setting {sorted(extra)[0]}")
        problems.extend(f"bots.{bot_id}.{k}: unknown setting dropped" for k in sorted(extra))
        out["bots"][bot_id] = {"guilds": guilds}
        if PURGED_KEY in bot:
            try:
                purged = validate_purged(f"bots.{bot_id}.{PURGED_KEY}", bot[PURGED_KEY])
            except SchemaError as exc:
                if strict:
                    raise
                problems.append(f"{exc} (dropped)")
                purged = {}
            if purged:
                out["bots"][bot_id][PURGED_KEY] = purged
    audit = doc.get("audit", [])
    out["audit"] = [a for a in audit if isinstance(a, dict)][-AUDIT_RING:] if isinstance(audit, list) else []
    extra = set(doc) - {"schema", "bots", "audit"}
    if extra and strict:
        raise SchemaError(f"document: unknown setting {sorted(extra)[0]}")
    problems.extend(f"{k}: unknown setting dropped" for k in sorted(extra))
    return out, problems
