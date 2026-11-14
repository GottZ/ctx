"""Persistent controls: the ``custom_id`` schema and the one ``DynamicItem`` class behind every control.

``custom_id`` = ``dsp:<version>:<guild>:<action>:<arg>:<rev>`` (at most 100 characters):

* ``version`` — layout of this id (:data:`VERSION`); other versions get "run /policy again";
* ``guild`` — the server the panel was opened in. It is only *compared* with the interaction's server;
  every action acts on the server Discord reports for the interaction, never on this field;
* ``action``/``arg`` — what the control does (looked up in the panel action table);
* ``rev`` — the guild's settings revision the panel showed, for conflict detection.

The id never carries rights: every click is checked again through :mod:`.perms`. Any ``dsp:`` id reaches
:func:`dispatch` (also malformed or outdated ones), so every click gets an answer instead of Discord's
"This interaction failed".
"""

from __future__ import annotations

import re
import weakref
from dataclasses import dataclass
from typing import Any, Optional

PREFIX = "dsp"
VERSION = 1
MAX_LEN = 100
TEMPLATE = r"dsp:[!-~]{1,96}"

_ACTION = re.compile(r"[a-z][a-z0-9_]{0,15}")
_ARG = re.compile(r"[A-Za-z0-9_.-]{0,32}")
_GUILD = re.compile(r"[1-9][0-9]{0,19}")
_REV = re.compile(r"0|[1-9][0-9]{0,11}")


class BadId(ValueError):
    """A ``dsp:`` custom_id this version cannot act on (message is user-facing)."""


@dataclass(frozen=True)
class Ref:
    guild: str
    action: str
    arg: str = ""
    rev: int = 0
    version: int = VERSION


def encode(guild_id: Any, action: str, arg: str = "", rev: int = 0) -> str:
    guild = str(guild_id)
    if not _GUILD.fullmatch(guild):
        raise ValueError(f"guild id {guild!r}")
    if not _ACTION.fullmatch(action):
        raise ValueError(f"action {action!r}")
    if not _ARG.fullmatch(arg):
        raise ValueError(f"arg {arg!r}")
    if not isinstance(rev, int) or isinstance(rev, bool) or not _REV.fullmatch(str(rev)):
        raise ValueError(f"rev {rev!r}")
    custom_id = f"{PREFIX}:{VERSION}:{guild}:{action}:{arg}:{rev}"
    if len(custom_id) > MAX_LEN:  # pragma: no cover - bounded by the patterns above (max 87)
        raise ValueError("custom_id longer than 100 characters")
    return custom_id


def parse(custom_id: Any) -> Ref:
    outdated = BadId("This control belongs to an outdated panel. Next step: run /policy again.")
    if not isinstance(custom_id, str) or len(custom_id) > MAX_LEN:
        raise outdated
    parts = custom_id.split(":")
    if len(parts) < 2 or parts[0] != PREFIX:
        raise outdated
    if parts[1] != str(VERSION) or len(parts) != 6:
        raise outdated
    _, _, guild, action, arg, rev = parts
    if not (_GUILD.fullmatch(guild) and _ACTION.fullmatch(action) and _ARG.fullmatch(arg) and _REV.fullmatch(rev)):
        raise outdated
    return Ref(guild=guild, action=action, arg=arg, rev=int(rev))


# ── bindings: which UI binding serves a discord.py client ─────────────────────────────────────────────

_BINDINGS: "weakref.WeakKeyDictionary[Any, Any]" = weakref.WeakKeyDictionary()


def bind_client(client: Any, binding: Any) -> None:
    _BINDINGS[client] = binding


def unbind_client(client: Any) -> None:
    try:
        _BINDINGS.pop(client, None)
    except TypeError:
        pass


def binding_for(client: Any) -> Optional[Any]:
    try:
        return _BINDINGS.get(client)
    except TypeError:
        return None


async def dispatch(interaction: Any, item: Any) -> None:
    """Entry point of every click on a ``dsp:`` control (persistent or not)."""
    binding = binding_for(getattr(interaction, "client", None))
    if binding is None:
        await interaction.response.send_message(
            "The policy interface is not ready on this bot yet. Next step: try again in a minute.", ephemeral=True)
        return
    await binding.on_component(interaction, item)


# ── the DynamicItem class (one per discord module) ────────────────────────────────────────────────────

_CLASSES: dict = {}


def component_class(discord: Any) -> Any:
    """``DynamicItem`` subclass that wraps any dispatchable item (button or select) with a ``dsp:`` id.

    One class for every component type: discord.py keys dynamic items by template and calls each matching
    class with the clicked base item, so separate classes would need disjoint templates.
    """
    cls = _CLASSES.get(id(discord))
    if cls is not None:
        return cls

    class PolicyComponent(discord.ui.DynamicItem[discord.ui.Item], template=TEMPLATE):
        def __init__(self, item: Any) -> None:
            super().__init__(item)

        @classmethod
        async def from_custom_id(cls, interaction: Any, item: Any, match: Any, /) -> "PolicyComponent":
            return cls(item)

        async def interaction_check(self, interaction: Any, /) -> bool:
            # Rights are checked in dispatch() on every click, so that a refusal gets an answer.
            return True

        async def callback(self, interaction: Any) -> None:
            await dispatch(interaction, self.item)

    _CLASSES[id(discord)] = PolicyComponent
    return PolicyComponent
