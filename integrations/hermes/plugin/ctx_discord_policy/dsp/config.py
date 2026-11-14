"""Operator-level settings from the Hermes configuration (DESIGN §5.1).

The operator list is the single source of tool authority. It is read from the Hermes configuration of the
bot owner's home on every decision (Hermes caches the parsed file by its signature, so an edit through the
dashboard or ``hermes config set`` applies to the next turn without a restart). Nothing in Discord and
nothing in the plugin's own state can add an operator.
"""

from __future__ import annotations

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
}

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
    return _parse_operators(home, raw), True


def operators(home: os.PathLike) -> frozenset[str]:
    """Discord user ids with tool authority for the bot owned by ``home``. Fails closed: any reading
    problem yields no operator (public lanes only)."""
    return operator_list(home)[0]


def _parse_operators(home: os.PathLike, raw: Any) -> frozenset[str]:
    if raw is None:
        return frozenset()
    if not isinstance(raw, (list, tuple)):
        _warn_once(home, "settings.operators must be a list of Discord user ids; ignoring it")
        return frozenset()
    out = set()
    for entry in raw:
        value = str(entry) if isinstance(entry, int) and not isinstance(entry, bool) else entry
        if isinstance(value, str) and is_snowflake(value.strip()):
            out.add(value.strip())
        else:
            _warn_once(home, f"settings.operators: ignoring {entry!r} (not a Discord user id)")
    return frozenset(out)


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
    return value


# --- S12/S13 --- profiles the policy never opens to members (PROFILE-PER-GUILD §4.3) ------------------

import re  # noqa: E402  (kept inside this block)

DEFAULTS["locked_profiles"] = []

_PROFILE_ID = re.compile(r"[a-z0-9][a-z0-9_-]{0,63}")


def locked_profiles(root: os.PathLike) -> Optional[frozenset[str]]:
    """``settings.locked_profiles`` of the default root (the owner of ``profile_routes``).

    A locked profile is never offered and never opened to public messages, whatever routes there.
    ``None`` means the list could not be read: callers treat every profile as locked (closed)."""
    try:
        raw = _raw(root, "locked_profiles")
    except Exception:
        logger.exception("ctx_discord_policy: reading settings.locked_profiles failed; treating every "
                         "profile as locked")
        return None
    if raw is None:
        return frozenset()
    if not isinstance(raw, (list, tuple)):
        _warn_once(root, "settings.locked_profiles must be a list of profile names; treating every profile "
                         "as locked until it is fixed")
        return None
    out = set()
    for entry in raw:
        name = entry.strip().lower() if isinstance(entry, str) else ""
        if _PROFILE_ID.fullmatch(name):
            out.add(name)
        else:
            _warn_once(root, f"settings.locked_profiles: ignoring {entry!r} (not a profile name)")
    return frozenset(out)


# --- end S12/S13 -------------------------------------------------------------------------------------
