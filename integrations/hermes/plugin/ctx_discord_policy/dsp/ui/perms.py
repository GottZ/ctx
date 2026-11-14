"""Who may do what in ``/policy`` (DESIGN §4), evaluated afresh on every interaction.

Three roles:

* **operator** — the caller's user id is in ``settings.operators`` of the Hermes configuration of the
  bot owner's home (:func:`dsp.config.is_operator`). Nothing in Discord (roles, Administrator, server
  ownership) and nothing in the plugin's own state can make anyone an operator.
* **manager** — Discord says the caller has *Manage Server* (or *Administrator*) in this interaction, and
  the server's ``managers`` setting (operator-only) allows it: ``discord_admins`` (default), ``role``
  (Manage Server **and** one of the listed roles) or ``operators_only`` (nobody).
* **member** — everyone else: no access to the interface.

:data:`TABLE` is the one place that says which role may change which setting. Enforcement
(:func:`require`, :func:`check_write`) and display (:func:`may`, used by the panels to decide which
controls a caller sees) both read it; there is no second list.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable, Optional

from .. import config
from ..store.schema import MANAGER_MODES

OPERATOR = "operator"
MANAGER = "manager"
MEMBER = "member"
ROLES = (OPERATOR, MANAGER, MEMBER)

_O = frozenset({OPERATOR})
_OM = frozenset({OPERATOR, MANAGER})
_M = frozenset({MANAGER})
_NOBODY: frozenset = frozenset()


@dataclass(frozen=True)
class Setting:
    key: str
    label: str
    roles: frozenset
    fields: tuple = ()      # schema leaf paths this setting writes (``state`` is governed by TRANSITIONS)
    note: str = ""          # why it is restricted, shown in refusals


TABLE: tuple = (
    # Opening /policy at all (members see nothing; DESIGN §4.1).
    Setting("panel.open", "the policy panel", _OM),
    # Lifecycle of the server's policy.
    Setting("guild.activate", "activating the policy in this server", _O),
    Setting("guild.unbind", "unbinding this server or deleting its settings", _O),
    Setting("guild.request", "asking the operator to activate", _M),
    Setting("guild.pause", "pausing or resuming the policy in this server", _OM),
    # Conversation settings (server managers and operators).
    Setting("channels", "channels", _OM, ("conversation.ambient_channels", "conversation.excluded_channels")),
    Setting("identity", "name, wake words, greetings, reaction and language", _OM,
            ("identity.bot_name", "identity.wake_names", "identity.greetings", "identity.reaction_emoji",
             "conversation.language")),
    Setting("guidance", "server guidance", _OM, ("prompts.guild", "prompts.history")),
    Setting("progress", "progress display", _OM, ("progress.grouping",)),
    # Operator-only settings.
    Setting("worklog", "the worklog", _O, ("worklog.channel", "worklog.archive_minutes", "worklog.final_to_origin"),
            note="Worklogs carry the output of the operator's tools."),
    Setting("public_lanes", "what members may let the bot do", _O,
            ("lanes.channel_history.enabled", "lanes.channel_history.max_limit", "lanes.permalink_hydration",
             "lanes.skills", "lanes.knowledge"),
            note="These settings release the operator's data and tools to members."),
    Setting("operator_lane", "how the bot treats the operator's own messages", _O,
            ("operator_lane.channel_hydration", "prompts.guild_in_operator_lane"),
            note="Server guidance in operator turns could steer the operator's tools."),
    Setting("managers", "who may manage this server's policy, and the audit channel", _O,
            ("managers.mode", "managers.role_ids", "audit_channel")),
    Setting("templates", "the operator frame and prompt templates", _O, ("prompts.template_overrides",)),
    Setting("operators", "the operator list", _NOBODY,
            note="Operators are set only in the Hermes configuration on the host, never in Discord."),
    # Reading the adaptation layer's status.
    Setting("status.full", "the full protection status", _O),
    Setting("status.summary", "the protection summary", _OM),
)

SETTINGS: dict = {s.key: s for s in TABLE}

# Every change of ``state`` maps to exactly one setting; anything not listed is refused.
TRANSITIONS: dict = {
    ("unbound", "active"): "guild.activate",
    ("active", "paused"): "guild.pause",
    ("paused", "active"): "guild.pause",
    ("active", "unbound"): "guild.unbind",
    ("paused", "unbound"): "guild.unbind",
}
PURGE = "guild.unbind"

# Written by the store or by the lifecycle operations only, never through a settings form.
BOOKKEEPING = ("state", "rev", "bound_by", "bound_at", "updated_by", "updated_at")


class Refused(Exception):
    """An action the caller may not take; ``str(exc)`` is the user-facing reason with the next step."""


@dataclass(frozen=True)
class Actor:
    user_id: str
    role: str
    manage_guild: bool = False
    why: str = ""                     # for members: why they are not a manager here
    role_ids: tuple = field(default=(), compare=False)

    @property
    def is_operator(self) -> bool:
        return self.role == OPERATOR


def _manager_rule(guild_config: Optional[dict]) -> tuple[str, tuple]:
    managers = (guild_config or {}).get("managers") or {}
    mode = managers.get("mode", "discord_admins")
    role_ids = tuple(str(r) for r in managers.get("role_ids") or ())
    return (mode if mode in MANAGER_MODES else "operators_only"), role_ids


def resolve(*, user_id: Any, manage_guild: bool, role_ids: Iterable[Any], owner_home: Any,
            guild_config: Optional[dict]) -> Actor:
    """The caller's role for one interaction. ``manage_guild`` and ``role_ids`` come from the interaction
    (set by Discord); the operator decision comes only from the Hermes configuration."""
    uid = str(user_id or "").strip()
    roles = tuple(str(r) for r in role_ids or ())
    if uid and config.is_operator(owner_home, uid):
        return Actor(uid, OPERATOR, bool(manage_guild), role_ids=roles)
    if not manage_guild:
        return Actor(uid, MEMBER, False, "no_manage", roles)
    mode, allowed = _manager_rule(guild_config)
    if mode == "discord_admins":
        return Actor(uid, MANAGER, True, role_ids=roles)
    if mode == "role":
        if set(allowed) & set(roles):
            return Actor(uid, MANAGER, True, role_ids=roles)
        return Actor(uid, MEMBER, True, "role:" + ",".join(allowed), roles)
    return Actor(uid, MEMBER, True, "operators_only", roles)


def _interaction_role_ids(user: Any) -> list:
    roles = getattr(user, "roles", None) or []
    ids = [getattr(r, "id", None) for r in roles]
    ids = [i for i in ids if i is not None]
    if not ids:
        # Without the guild in the client cache ``Member.roles`` resolves to nothing; the raw ids Discord
        # sent with the interaction are on ``Member._roles`` (discord.py 2.x).
        ids = list(getattr(user, "_roles", None) or [])
    return ids


def from_interaction(interaction: Any, *, owner_home: Any, guild_config: Optional[dict]) -> Actor:
    perms = getattr(interaction, "permissions", None)
    manage = bool(getattr(perms, "manage_guild", False) or getattr(perms, "administrator", False))
    user = getattr(interaction, "user", None)
    return resolve(user_id=getattr(user, "id", None), manage_guild=manage, role_ids=_interaction_role_ids(user),
                   owner_home=owner_home, guild_config=guild_config)


def operator_list(owner_home: Any) -> tuple:
    """``(operators, readable)``. ``readable`` is False when the Hermes configuration could not be read
    (for example a broken config.yaml): then nobody is an operator, which the interface must say instead
    of presenting it as "no operator configured"."""
    try:
        config._raw(owner_home, "operators")  # the reader raises when Hermes serves a fallback copy
    except Exception:
        return frozenset(), False
    return config.operators(owner_home), True


def may(actor: Actor, key: str) -> bool:
    setting = SETTINGS.get(key)
    return setting is not None and actor.role in setting.roles


def _member_reason(actor: Actor) -> str:
    if actor.why == "operators_only":
        return ("In this server only the bot's operator can change its settings. "
                "Next step: ask the operator.")
    if actor.why.startswith("role:"):
        roles = [r for r in actor.why[5:].split(",") if r]
        listed = ", ".join(f"<@&{r}>" for r in roles) or "a role the operator chose"
        return ("In this server the bot's settings can be changed by members with **Manage Server** and one "
                f"of these roles: {listed}. Next step: ask a server admin for one of them.")
    return ("Changing the bot's settings in this server needs the **Manage Server** permission. "
            "Next step: ask a server admin.")


def refusal(actor: Actor, key: str) -> str:
    """User-facing reason why ``actor`` may not use ``key``, with the next step."""
    setting = SETTINGS.get(key)
    if setting is None:
        return "This control is not known to this version of the plugin. Next step: run /policy again."
    if not setting.roles:
        return setting.note or f"Nobody can change {setting.label} in Discord."
    if actor.role == MEMBER:
        return _member_reason(actor)
    if key == "guild.activate":
        return ("Only the bot's operator can activate the policy in a server. "
                "Next step: use **Ask the operator to activate**.")
    if key == "guild.request" and actor.role == OPERATOR:
        return "You are an operator of this bot: activate the policy directly with **Activate in this server**."
    if setting.roles == _O:
        note = f" {setting.note}" if setting.note else ""
        return (f"Only the bot's operator can change {setting.label}.{note} "
                "Next step: ask the bot's operator.")
    return f"You can't use {setting.label} here. Next step: ask the bot's operator."


def require(actor: Actor, key: str) -> None:
    if not may(actor, key):
        raise Refused(refusal(actor, key))


def check_write(actor: Actor, key: str, paths: Iterable[str]) -> None:
    """Refuse unless ``actor`` may change ``key`` and every written path belongs to that setting."""
    require(actor, key)
    allowed = set(SETTINGS[key].fields)
    stray = sorted(set(paths) - allowed)
    if stray:
        raise Refused(f"Internal error: {', '.join(stray)} is not part of {SETTINGS[key].label}; nothing was saved.")


def transition_setting(old: str, new: str) -> Optional[str]:
    return TRANSITIONS.get((old, new))


def visible(actor: Actor) -> list:
    """Settings whose controls ``actor`` sees (derived from :data:`TABLE`)."""
    return [s.key for s in TABLE if actor.role in s.roles]


def owners_of(path: str) -> list:
    """Settings that write ``path`` (exactly one for every non-bookkeeping schema field)."""
    return [s.key for s in TABLE if path in s.fields]
