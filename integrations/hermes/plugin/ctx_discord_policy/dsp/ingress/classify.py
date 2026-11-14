"""Minimal message classification for the security core: is the bot addressed, is this a thread.

Wake names, greetings and the reaction wakeup (conversation layer) extend ``direct_signal`` later; the
security core only needs the signals Discord itself provides (mention, reply to the bot).
"""

from __future__ import annotations

import re
from typing import Any

DIRECT = "direct"
MAYBE_REPLY = "maybe_reply"   # a reply whose referenced message is not resolved yet
NONE = "none"

_THREAD_TYPES = {10, 11, 12}
_UNSAFE = re.compile(r"[\x00-\x1f\x7f\[\]]")


def is_thread(channel: Any) -> bool:
    kind = getattr(channel, "type", None)
    value = getattr(kind, "value", kind)
    try:
        return int(value) in _THREAD_TYPES
    except (TypeError, ValueError):
        return False


def _self_id(adapter: Any) -> str:
    return str(getattr(getattr(getattr(adapter, "_client", None), "user", None), "id", "") or "")


def direct_signal(adapter: Any, message: Any) -> str:
    mentioned = getattr(adapter, "_self_is_explicitly_mentioned", None)
    if callable(mentioned) and bool(mentioned(message)):
        return DIRECT
    reference = getattr(message, "reference", None)
    if reference is None:
        return NONE
    self_id = _self_id(adapter)
    resolved = getattr(reference, "resolved", None)
    author_id = str(getattr(getattr(resolved, "author", None), "id", "") or "")
    if resolved is not None and author_id:
        return DIRECT if self_id and author_id == self_id else NONE
    return MAYBE_REPLY if getattr(reference, "message_id", None) is not None else NONE


async def verify_reply_to_self(adapter: Any, message: Any) -> bool:
    """Resolve an unresolved reply reference; True only if it points at a message by this bot."""
    reference = getattr(message, "reference", None)
    channel = getattr(message, "channel", None)
    message_id = getattr(reference, "message_id", None)
    fetch = getattr(channel, "fetch_message", None)
    if message_id is None or not callable(fetch):
        return False
    try:
        resolved = await fetch(int(message_id))
    except Exception:
        return False
    author_id = str(getattr(getattr(resolved, "author", None), "id", "") or "")
    return bool(author_id) and author_id == _self_id(adapter)


def clean_label(value: str, limit: int = 100) -> str:
    """A Discord display name made safe for a bracketed label: no control chars, brackets or newlines."""
    return _UNSAFE.sub(" ", value).strip()[:limit] or "member"
