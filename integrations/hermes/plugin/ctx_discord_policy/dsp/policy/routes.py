"""Authority routes: the exact, per-message record that lets one turn run under one lane."""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any, Optional

OPERATOR = "operator"
PUBLIC = "public"

DIRECT = "direct"
AMBIENT = "ambient"


def ambient_user_id(channel_id: str) -> str:
    """Session identity of the shared ambient conversation in one channel."""
    return f"ambient:{channel_id}"


@dataclass(eq=False)
class Route:
    """One admitted Discord message and the authority it carries.

    ``lane`` is decided once, from the operator list in the Hermes configuration, when the route is built;
    nothing stored in Discord or in the plugin's state can change it.
    """

    bot_id: str
    guild_id: str
    channel_id: str
    message_id: str
    author_id: str
    lane: str
    mode: str = DIRECT
    target_home_key: str = ""
    lanes: Any = None          # frozen public-lane parameters of the guild at admission time
    tool_started: bool = False
    created_at: float = field(default_factory=time.time)

    @property
    def session_user_id(self) -> str:
        """The user id Hermes sees for this message.

        Ambient messages of public members share one channel identity (one conversation per channel).
        Operator messages never join that shared session: an operator turn must not carry public members'
        text as its own input (shared session history, busy-input steering), and a public turn must never
        run inside an operator session.
        """
        return ambient_user_id(self.channel_id) if self.mode == AMBIENT and self.lane == PUBLIC else self.author_id

    @property
    def is_operator(self) -> bool:
        return self.lane == OPERATOR

    @property
    def key(self) -> tuple[str, str]:
        return (self.channel_id, self.message_id)

    def matches_source(self, source: Any, *, platform_is_discord: bool) -> bool:
        """Exactly this route's message source: same platform, same chat, same (session) user."""
        if not platform_is_discord:
            return False
        user_id = str(getattr(source, "user_id", "") or "")
        chat_id = str(getattr(source, "chat_id", "") or "")
        return bool(self.author_id) and user_id == self.session_user_id and chat_id == self.channel_id


def platform_is_discord(value: Any) -> bool:
    raw = getattr(value, "value", value)
    return str(raw or "").strip().lower() == "discord"


def snowflake(value: Any) -> Optional[str]:
    """Canonical Discord id string, or None for anything that is not a plain positive integer id."""
    text = str(value if value is not None else "").strip()
    if text.isdigit() and 0 < len(text) <= 20 and not text.startswith("0"):
        return text
    if isinstance(value, int) and not isinstance(value, bool) and value > 0:
        return str(value)
    return None
