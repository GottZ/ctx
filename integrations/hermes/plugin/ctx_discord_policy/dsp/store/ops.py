"""Guild-level write operations shared by the CLI (now) and the Discord UI (later).

Every operation is a mutation for :meth:`PolicyStore.update`; inputs are validated strictly against the
schema before anything is written.
"""

from __future__ import annotations

import copy
from typing import Any, Optional

from .file import PolicyStore, Snapshot, utc_now
from .schema import HARD_PROMPT_LIMIT, SchemaError, field_paths, guild_defaults, is_snowflake, validate_guild


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

    ``rev`` and bookkeeping fields (``updated_*``, ``bound_*``) in the payload are ignored; the store owns
    them. ``bound_by`` is informational only and never maps to authority.
    """
    bot_id, guild_id = _require_ids(bot_id, guild_id)
    if not isinstance(payload, dict):
        raise SchemaError("import: expected a guild mapping")
    incoming = {k: v for k, v in payload.items()
                if k not in ("rev", "updated_by", "updated_at", "bound_by", "bound_at")}
    clean, _ = validate_guild(incoming, strict=True, prompt_cap=prompt_cap)

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guilds = _guilds(document, bot_id)
        old = guilds.get(guild_id)
        new = copy.deepcopy(clean)
        new["rev"] = (old or {}).get("rev", 0)
        new["bound_by"] = (old or {}).get("bound_by")
        new["bound_at"] = (old or {}).get("bound_at")
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


_NOT_SETTINGS = ("state", "rev", "bound_by", "bound_at", "updated_by", "updated_at")


def activate(store: PolicyStore, bot_id: str, guild_id: str, *, by: str,
             expected_rev: Optional[int] = None) -> Snapshot:
    """:func:`bind` with an optimistic ``rev`` check (the Discord interface passes the revision it showed)."""
    bot_id, guild_id = _require_ids(bot_id, guild_id)

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guilds = _guilds(document, bot_id)
        guild = guilds.get(guild_id)
        changes = []
        if guild is None:
            guild = guilds[guild_id] = guild_defaults()
            changes.append({"bot": bot_id, "guild": guild_id, "field": "created", "old": None, "new": True,
                            "expected_rev": expected_rev})
        if guild["state"] != "active":
            changes.append({"bot": bot_id, "guild": guild_id, "field": "state", "old": guild["state"],
                            "new": "active", "expected_rev": expected_rev})
            guild["state"] = "active"
            guild["bound_by"] = by
            guild["bound_at"] = utc_now()
        return changes

    return store.update(mutate, by=by)


def purge_checked(store: PolicyStore, bot_id: str, guild_id: str, *, by: str,
                  expected_rev: Optional[int] = None) -> Snapshot:
    """:func:`purge` with an optimistic ``rev`` check."""
    bot_id, guild_id = _require_ids(bot_id, guild_id)

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guilds = _guilds(document, bot_id)
        old = guilds.pop(guild_id, None)
        if old is None:
            return []
        if not guilds:
            document["bots"].pop(bot_id, None)
        return [{"bot": bot_id, "guild": guild_id, "field": "purged", "old": old, "new": None,
                 "expected_rev": expected_rev}]

    return store.update(mutate, by=by)


def update_fields(store: PolicyStore, bot_id: str, guild_id: str, changes: dict[str, Any], *, by: str,
                  expected_rev: Optional[int] = None, prompt_cap: int = HARD_PROMPT_LIMIT) -> Snapshot:
    """Set settings of a configured guild by dotted schema path (``{"progress.grouping": "rotate"}``).

    Every path must be a settings leaf of schema v1 (never ``state``, ``rev`` or bookkeeping); the guild with
    the changes applied is validated strictly before anything is written. One audit entry per changed path.
    """
    bot_id, guild_id = _require_ids(bot_id, guild_id)
    if not isinstance(changes, dict) or not changes:
        raise SchemaError("update: no settings given")
    known = set(field_paths())
    for path in changes:
        if path not in known or path in _NOT_SETTINGS:
            raise SchemaError(f"{path}: not a setting that can be changed here")

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guilds = _guilds(document, bot_id)
        guild = guilds.get(guild_id)
        if guild is None:
            raise SchemaError(f"guild {guild_id} is not configured for bot {bot_id}")
        candidate = copy.deepcopy(guild)
        records = []
        for path, value in changes.items():
            node = candidate
            *parents, leaf = path.split(".")
            for part in parents:
                node = node[part]
            old = node.get(leaf)
            node[leaf] = copy.deepcopy(value)
            records.append((path, old))
        clean, _ = validate_guild(candidate, strict=True, prompt_cap=prompt_cap)
        out = []
        for path, old in records:
            node = clean
            for part in path.split("."):
                node = node[part]
            if node != old:
                out.append({"bot": bot_id, "guild": guild_id, "field": path, "old": old, "new": node,
                            "expected_rev": expected_rev})
        guilds[guild_id] = clean
        return out

    return store.update(mutate, by=by)


def export(snapshot: Snapshot, bot_id: Optional[str] = None, guild_id: Optional[str] = None) -> dict[str, Any]:
    bots = snapshot.data.get("bots", {})
    selected = {b: v for b, v in bots.items() if bot_id is None or b == str(bot_id)}
    if guild_id is not None:
        selected = {b: {"guilds": {g: c for g, c in v["guilds"].items() if g == str(guild_id)}}
                    for b, v in selected.items()}
    return {"schema": snapshot.data.get("schema"), "bots": copy.deepcopy(selected)}


# --- SP --- settings page (shared write path): audit origin and a revision-checked import ----------------

import contextlib  # noqa: E402 - imports of this block stay in it (other blocks are appended independently)
from typing import Iterator  # noqa: E402

from .file import AUDIT_VIA, check_via  # noqa: E402


@contextlib.contextmanager
def origin(via: str) -> Iterator[None]:
    """Record ``via`` (``cli`` | ``discord`` | ``dashboard``) on the audit entries of every write in this
    block (context-local, so concurrent requests and interactions never see each other's origin)."""
    token = AUDIT_VIA.set(check_via(via))
    try:
        yield
    finally:
        AUDIT_VIA.reset(token)


def import_guild_checked(store: PolicyStore, bot_id: str, guild_id: str, payload: Any, *, by: str,
                         prompt_cap: int, expected_rev: Optional[int] = None, via: Optional[str] = None) -> Snapshot:
    """:func:`import_guild` with an optimistic ``rev`` check: an import never silently replaces settings
    that changed after the importer looked at them. Like :func:`import_guild` it may change ``state``."""
    bot_id, guild_id = _require_ids(bot_id, guild_id)
    if not isinstance(payload, dict):
        raise SchemaError("import: expected a guild mapping")
    incoming = {k: v for k, v in payload.items()
                if k not in ("rev", "updated_by", "updated_at", "bound_by", "bound_at")}
    clean, _ = validate_guild(incoming, strict=True, prompt_cap=prompt_cap)

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guilds = _guilds(document, bot_id)
        old = guilds.get(guild_id)
        new = copy.deepcopy(clean)
        new["rev"] = (old or {}).get("rev", 0)
        new["bound_by"] = (old or {}).get("bound_by")
        new["bound_at"] = (old or {}).get("bound_at")
        changes = []
        for section in new:
            if section in ("rev", "updated_by", "updated_at"):
                continue
            before = (old or guild_defaults()).get(section)
            if before != new[section]:
                changes.append({"bot": bot_id, "guild": guild_id, "field": section, "old": before,
                                "new": new[section], "expected_rev": expected_rev})
        guilds[guild_id] = new
        return changes or [{"bot": bot_id, "guild": guild_id, "field": "import", "old": None, "new": None,
                            "expected_rev": expected_rev}]

    return store.update(mutate, by=by, via=via)


# --- S9 --- server guidance with history, restore, reset (one write path for the Discord interface and the
# Hermes settings page). Every function takes ``expected_rev`` and the audit origin ``via``
# ("discord" | "dashboard" | "cli"; ``None``: the context's origin from :func:`origin`).

GUIDANCE_PATH = "prompts.guild"
_SHA_PREFIX_MIN = 12


def guidance_digest(text: str) -> str:
    import hashlib

    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def _leaf(node: dict[str, Any], path: str) -> tuple[dict[str, Any], str]:
    *parents, leaf = path.split(".")
    for part in parents:
        node = node[part]
    return node, leaf


def _set_guidance_in(guild: dict[str, Any], text: str, *, by: str) -> Optional[tuple[str, str]]:
    """Set the guidance text of ``guild`` in place and record the version; ``None`` when unchanged."""
    from .schema import PROMPT_HISTORY

    prompts = guild["prompts"]
    old = prompts.get("guild") or ""
    if text == old:
        return None
    history = list(prompts.get("history") or [])
    if not history and old:
        # The text from before the first recorded change (import, migration) stays restorable.
        # Its author is unknown (``updated_by`` names whoever changed any setting last).
        history.append({"sha256": guidance_digest(old), "by": "unknown", "at": str(guild.get("updated_at") or
                                                                                  utc_now())[:40], "text": old})
    history.append({"sha256": guidance_digest(text), "by": str(by)[:64], "at": utc_now(), "text": text})
    prompts["guild"] = text
    prompts["history"] = history[-PROMPT_HISTORY:]
    return old, text


def _guild_mutation(bot_id: str, guild_id: str, *, by: str, expected_rev: Optional[int], prompt_cap: int,
                    fields: Optional[dict[str, Any]] = None, guidance: Any = None) -> Any:
    """One mutation: settings by path (``fields``) and/or the guidance (``guidance(guild) -> text``)."""

    def mutate(document: dict[str, Any]) -> list[dict[str, Any]]:
        guild = _guilds(document, bot_id).get(guild_id)
        if guild is None:
            raise SchemaError(f"guild {guild_id} is not configured for bot {bot_id}")
        candidate = copy.deepcopy(guild)
        records = []
        for path, value in (fields or {}).items():
            node, leaf = _leaf(candidate, path)
            records.append((path, node.get(leaf)))
            node[leaf] = copy.deepcopy(value)
        changed_text = None
        if guidance is not None:
            changed_text = _set_guidance_in(candidate, guidance(candidate), by=by)
        clean, _ = validate_guild(candidate, strict=True, prompt_cap=prompt_cap)
        out = []
        for path, old in records:
            node, leaf = _leaf(clean, path)
            if node[leaf] != old:
                out.append({"bot": bot_id, "guild": guild_id, "field": path, "old": old, "new": node[leaf],
                            "expected_rev": expected_rev})
        if changed_text is not None:
            out.append({"bot": bot_id, "guild": guild_id, "field": GUIDANCE_PATH, "old": changed_text[0],
                        "new": changed_text[1], "expected_rev": expected_rev})
        _guilds(document, bot_id)[guild_id] = clean
        return out

    return mutate


def set_guidance(store: PolicyStore, bot_id: str, guild_id: str, text: str, *, by: str, via: Optional[str] = None,
                 expected_rev: Optional[int] = None, prompt_cap: int = HARD_PROMPT_LIMIT) -> Snapshot:
    """Replace the server guidance; the new version joins ``prompts.history`` (ring of ``PROMPT_HISTORY``).

    The same text again changes nothing (no new version, no new revision). ``prompt_cap`` is the operator's
    ``guild_prompt_max_chars``; a longer text is refused, never shortened.
    """
    bot_id, guild_id = _require_ids(bot_id, guild_id)
    if not isinstance(text, str):
        raise SchemaError("prompts.guild: expected text")
    mutate = _guild_mutation(bot_id, guild_id, by=by, expected_rev=expected_rev, prompt_cap=prompt_cap,
                             guidance=lambda guild: text)
    return store.update(mutate, by=by, via=via)


def find_guidance_version(guild: dict[str, Any], sha256: str) -> dict[str, Any]:
    """The history entry whose digest starts with ``sha256`` (at least 12 hex characters, unambiguous)."""
    prefix = str(sha256 or "").lower()
    if len(prefix) < _SHA_PREFIX_MIN or any(c not in "0123456789abcdef" for c in prefix):
        raise SchemaError("prompts.history: name a version by at least 12 characters of its sha256")
    matches = [e for e in (guild.get("prompts") or {}).get("history") or [] if str(e.get("sha256", "")).startswith(prefix)]
    if not matches:
        raise SchemaError("prompts.history: this version is no longer in the history")
    if len({m["sha256"] for m in matches}) > 1:
        raise SchemaError("prompts.history: the version id is ambiguous")
    return matches[-1]


def restore_guidance(store: PolicyStore, bot_id: str, guild_id: str, sha256: str, *, by: str,
                     via: Optional[str] = None, expected_rev: Optional[int] = None,
                     prompt_cap: int = HARD_PROMPT_LIMIT) -> Snapshot:
    """Make an earlier version from ``prompts.history`` the current guidance (as a new version)."""
    bot_id, guild_id = _require_ids(bot_id, guild_id)
    mutate = _guild_mutation(bot_id, guild_id, by=by, expected_rev=expected_rev, prompt_cap=prompt_cap,
                             guidance=lambda guild: find_guidance_version(guild, sha256)["text"])
    return store.update(mutate, by=by, via=via)


def reset_fields(store: PolicyStore, bot_id: str, guild_id: str, paths: Any, *, by: str, via: Optional[str] = None,
                 expected_rev: Optional[int] = None, prompt_cap: int = HARD_PROMPT_LIMIT) -> Snapshot:
    """Set settings back to their defaults (``schema.guild_defaults()``); resetting ``prompts.guild``
    records an empty version, so the text stays restorable from the history."""
    bot_id, guild_id = _require_ids(bot_id, guild_id)
    paths = list(paths or [])
    if not paths:
        raise SchemaError("reset: no settings given")
    known = set(field_paths())
    defaults = guild_defaults()
    fields: dict[str, Any] = {}
    guidance = None
    for path in paths:
        if path not in known or path in _NOT_SETTINGS or path == "prompts.history":
            raise SchemaError(f"{path}: not a setting that can be reset")
        if path == GUIDANCE_PATH:
            guidance = lambda guild: ""  # noqa: E731
            continue
        node, leaf = _leaf(defaults, path)
        fields[path] = node[leaf]
    mutate = _guild_mutation(bot_id, guild_id, by=by, expected_rev=expected_rev, prompt_cap=prompt_cap,
                             fields=fields, guidance=guidance)
    return store.update(mutate, by=by, via=via)
