"""Operator-level settings from the Hermes configuration (DESIGN §5.1).

The operator list is the single source of tool authority. It is read from the Hermes configuration of the
bot owner's home on every decision (Hermes caches the parsed file by its signature, so an edit through the
dashboard or ``hermes config set`` applies to the next turn without a restart). Nothing in Discord and
nothing in the plugin's own state can add an operator.
"""

from __future__ import annotations

import json
import logging
import os
from typing import Any, Callable, Optional

from .store.schema import HARD_PROMPT_LIMIT, is_snowflake

logger = logging.getLogger("ctx_discord_policy")

Reader = Callable[[os.PathLike, str, Any], Any]

DEFAULTS: dict[str, Any] = {
    "operators": [],
    "frame_prompt": "",
    "operator_label": "the operator",
    "guild_prompt_max_chars": 6000,
    "notify_operators": True,
    "bot_messages": "closed",
}

BOT_MESSAGES = ("closed", "member")

_reader: Optional[Reader] = None
_warned: set[tuple[str, str]] = set()


def set_reader(reader: Optional[Reader]) -> None:
    """Install how settings are read: ``reader(home, key, default)`` (bootstrap installs the Hermes one)."""
    global _reader
    _reader = reader


def _raw(home: os.PathLike, key: str) -> Any:
    if _reader is None:
        return DEFAULTS[key]
    return _reader(home, key, DEFAULTS[key])


def _warn_once(home: os.PathLike, message: str) -> None:
    key = (str(home), message)
    if key not in _warned:
        _warned.add(key)
        logger.warning("ctx_discord_policy: %s", message)


def operator_list(home: os.PathLike) -> tuple[frozenset[str], bool]:
    """``(operators, readable)`` for the bot owned by ``home``.

    ``readable`` is False when the Hermes configuration could not be read (the reader raises, for example
    while Hermes serves a fallback copy of a broken ``config.yaml``): then nobody is an operator, and an
    interface must say so instead of presenting it as "no operator configured". Fails closed either way.
    """
    try:
        raw = _raw(home, "operators")
    except Exception:
        logger.exception("ctx_discord_policy: reading settings.operators failed; no operator this turn")
        return frozenset(), False
    return frozenset(operator_ids(home, raw)), True


def operators(home: os.PathLike) -> frozenset[str]:
    """Discord user ids with tool authority for the bot owned by ``home``. Fails closed: any reading
    problem yields no operator (public lanes only)."""
    return operator_list(home)[0]


class UnreadableList(ValueError):
    """A list setting whose value has none of the forms :func:`list_entries` reads."""


def list_entries(raw: Any) -> tuple[list[Any], bool]:
    """``(entries, as_text)`` of a list setting (``operators``, ``locked_profiles``), for every reader of them.

    * a YAML list: its items (what ``hermes config set … '["<id>"]'`` and earlier versions write);
    * text, the form Hermes' settings form saves: the parts between commas, whitespace around each removed,
      empty parts dropped (``"a, b,"`` → ``a``, ``b``); text that starts with ``[`` is read as a JSON list
      (``'["a", "b"]'``, what the earlier JSON editor of that form took);
    * a whole number: one entry (``hermes config set`` reads a single ID as a number);
    * absent or null: no entry.

    ``as_text`` is True for the last three forms. Raises :class:`UnreadableList` for anything else (a mapping,
    a boolean, a float, text starting with ``[`` that is no JSON list); each setting reads that on its closed
    side. What counts as an entry is the setting's own rule.
    """
    if raw is None:
        return [], False
    if isinstance(raw, (list, tuple)):
        return list(raw), False
    if isinstance(raw, int) and not isinstance(raw, bool):
        return [raw], True
    if not isinstance(raw, str):
        raise UnreadableList(f"a {type(raw).__name__} is neither a list nor text")
    text = raw.strip()
    if text.startswith("["):
        try:
            parsed = json.loads(text)
        except ValueError:
            raise UnreadableList("text starting with [ is no JSON list") from None
        return parsed, True    # JSON text starting with [ that parses is always a list
    return [part.strip() for part in text.split(",") if part.strip()], True


def operator_ids(home: Any, raw: Any) -> list[str]:
    """The Discord user ids of a raw ``settings.operators`` value, in order, each once (fails closed: an
    entry that is no user id counts for nobody, an unreadable value means no operator; both are logged)."""
    try:
        entries, _ = list_entries(raw)
    except UnreadableList as exc:
        _warn_once(home, f"settings.operators: {exc}; no operator until it is fixed (Discord user ids, "
                         "separated by commas)")
        return []
    out: list[str] = []
    for entry in entries:
        value = str(entry) if isinstance(entry, int) and not isinstance(entry, bool) else entry
        if isinstance(value, str) and is_snowflake(value.strip()):
            if value.strip() not in out:
                out.append(value.strip())
        else:
            _warn_once(home, f"settings.operators: ignoring {entry!r} (not a Discord user id)")
    return out



def is_operator(home: os.PathLike, user_id: Any) -> bool:
    uid = str(user_id or "").strip()
    return bool(uid) and uid in operators(home)


def setting(home: os.PathLike, key: str) -> Any:
    """A non-authority setting, type-checked against its default (wrong type -> default + warning)."""
    default = DEFAULTS[key]
    try:
        value = _raw(home, key)
    except Exception:
        logger.exception("ctx_discord_policy: reading settings.%s failed", key)
        return default
    if isinstance(default, bool):
        ok = isinstance(value, bool)
    elif isinstance(default, int):
        ok = isinstance(value, int) and not isinstance(value, bool)
    else:
        ok = isinstance(value, type(default))
    if not ok:
        _warn_once(home, f"settings.{key} has the wrong type; using {default!r}")
        return default
    if key == "guild_prompt_max_chars":
        return max(0, min(value, HARD_PROMPT_LIMIT))
    if key == "bot_messages" and value not in BOT_MESSAGES:
        _warn_once(home, f"settings.bot_messages must be one of {', '.join(BOT_MESSAGES)}; using closed")
        return default
    return value


def bot_messages_as_members(home: os.PathLike) -> bool:
    """``settings.bot_messages: member``: messages of other bots and webhooks that Hermes' own ``allow_bots``
    lets in run as members' turns. Default and every reading problem: closed (they get no turn at all)."""
    return setting(home, "bot_messages") == "member"


# --- S12/S13 --- profiles the policy never opens to members (PROFILE-PER-GUILD §4.3) ------------------

import re  # noqa: E402  (kept inside this block)

DEFAULTS["locked_profiles"] = []

_PROFILE_ID = re.compile(r"[a-z0-9][a-z0-9_-]{0,63}")


def locked_profiles(root: os.PathLike) -> Optional[frozenset[str]]:
    """``settings.locked_profiles`` of the default root (the owner of ``profile_routes``).

    A locked profile is never offered and never opened to public messages, whatever routes there.
    ``None`` means the list could not be read: callers treat every profile as locked (closed).

    Read with :func:`list_entries`. In the text forms an entry that is no profile name makes the whole value
    unreadable: before the text form existed every text value locked every profile, and a mistyped list
    (``a; b``) must not unlock one. A YAML list keeps ignoring such an entry (it can name no profile: Hermes
    allows the same pattern)."""
    try:
        raw = _raw(root, "locked_profiles")
    except Exception:
        logger.exception("ctx_discord_policy: reading settings.locked_profiles failed; treating every "
                         "profile as locked")
        return None
    try:
        entries, as_text = list_entries(raw)
    except UnreadableList as exc:
        _warn_once(root, f"settings.locked_profiles: {exc}; treating every profile as locked until it is "
                         "fixed (profile names, separated by commas)")
        return None
    out = set()
    for entry in entries:
        name = entry.strip().lower() if isinstance(entry, str) else ""
        if _PROFILE_ID.fullmatch(name):
            out.add(name)
        elif as_text:
            _warn_once(root, f"settings.locked_profiles: {entry!r} is not a profile name; treating every "
                             "profile as locked until it is fixed")
            return None
        else:
            _warn_once(root, f"settings.locked_profiles: ignoring {entry!r} (not a profile name)")
    return frozenset(out)


# --- end S12/S13 -------------------------------------------------------------------------------------
