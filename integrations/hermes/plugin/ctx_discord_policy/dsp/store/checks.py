"""Checks that depend on the environment, not on the schema (SETTINGS-PAGE §3.3, DESIGN §5.2).

``schema.validate_guild`` decides whether a value is well-formed. What it cannot know is whether the value
fits the installation: a skill must be installed in the profile the guild's turns run in (refused
otherwise, DESIGN §5.2 "validated against installed skills"); a channel, role or emoji id should exist in
the server (only a warning: the bot may be offline, the directory old, the channel brand new).

Both interfaces call :func:`environment` with the same changes they hand to the store (the Hermes settings
page before ``ops.update_fields``, the Discord modals likewise). Pure: the caller collects the facts into an
:class:`Environment`; ``None`` for a fact means "not known", which never produces a claim about ids.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable, Mapping, Optional

ERROR = "error"
WARNING = "warning"

CHANNEL_FIELDS = ("conversation.ambient_channels", "conversation.excluded_channels", "worklog.channel",
                  "audit_channel")
# Channel kinds a setting can use (discord.py ChannelType names). Admission compares a message's channel and,
# in a thread, its parent channel: a category id never matches, so it is warned for both channel lists.
_POSTABLE = frozenset({"text", "news"})
_CONVERSATION = frozenset({"text", "news", "forum", "media", "voice", "stage_voice", "public_thread",
                           "private_thread", "news_thread"})
_KINDS: dict[str, Optional[frozenset]] = {
    "conversation.ambient_channels": _CONVERSATION,
    "conversation.excluded_channels": _CONVERSATION,
    "worklog.channel": _POSTABLE,
    "audit_channel": _POSTABLE,
}


@dataclass(frozen=True)
class Environment:
    skills: Optional[frozenset] = None            # skills installed in the guild's target profile
    skills_profile: str = ""                      # that profile's name (for messages)
    channels: Optional[Mapping[str, str]] = None  # channel id -> kind, from the directory
    roles: Optional[frozenset] = None
    emojis: Optional[frozenset] = None            # custom emoji ids of the server
    profiles: Optional[frozenset] = None          # Hermes profiles that exist
    directory_source: str = ""                    # where channels/roles/emojis came from ("live", "directory of …")

    def __post_init__(self) -> None:
        # Ids compare as text whatever the source handed in (discord.py ids are integers).
        for name in ("skills", "roles", "emojis", "profiles"):
            value = getattr(self, name)
            if value is not None:
                object.__setattr__(self, name, frozenset(str(v) for v in value))
        if self.channels is not None:
            object.__setattr__(self, "channels", {str(k): str(v) for k, v in self.channels.items()})


@dataclass(frozen=True)
class Finding:
    level: str      # error | warning
    field: str      # dotted schema path
    code: str
    message: str    # English, shown next to the field

    def as_dict(self) -> dict[str, str]:
        return {"level": self.level, "field": self.field, "code": self.code, "message": self.message}


def has_errors(findings: Iterable[Finding]) -> bool:
    return any(f.level == ERROR for f in findings)


def _ids(value: Any) -> list[str]:
    if value is None:
        return []
    if isinstance(value, (list, tuple)):
        return [str(v) for v in value if v is not None]
    return [str(value)]


def _source(env: Environment) -> str:
    return f" ({env.directory_source})" if env.directory_source else ""


def _check_skills(value: Any, env: Environment) -> list[Finding]:
    if not isinstance(value, (list, tuple)):
        return []   # not a list: the schema refuses it with a precise message
    names = [str(v) for v in value]
    if not names:
        return []
    where = f"profile {env.skills_profile}" if env.skills_profile else "the profile this server's turns run in"
    if env.skills is None:
        return [Finding(WARNING, "lanes.skills", "skills_unknown",
                        f"The installed skills of {where} could not be read, so these names were not checked.")]
    missing = [n for n in names if n not in env.skills]
    if not missing:
        return []
    return [Finding(ERROR, "lanes.skills", "skill_missing",
                    f"Not installed in {where}: {', '.join(missing)}. Install the skill there first, or remove "
                    "it from the list.")]


def _check_channels(path: str, value: Any, env: Environment) -> list[Finding]:
    ids = _ids(value)
    if not ids or env.channels is None:
        return []
    out: list[Finding] = []
    unknown = [i for i in ids if i not in env.channels]
    if unknown:
        out.append(Finding(WARNING, path, "channel_unknown",
                           f"Not in the server's channel list{_source(env)}: {', '.join(unknown)}. "
                           "It may be new or deleted; it is saved as entered."))
    allowed = _KINDS.get(path)
    wrong = [i for i in ids if allowed is not None and i in env.channels and env.channels[i] not in allowed]
    if wrong:
        listed = ", ".join(f"{i} ({env.channels[i]})" for i in wrong)
        out.append(Finding(WARNING, path, "channel_kind", f"This setting cannot use channels of this kind: {listed}."))
    return out


def _check_roles(value: Any, env: Environment) -> list[Finding]:
    unknown = [i for i in _ids(value) if env.roles is not None and i not in env.roles]
    if not unknown:
        return []
    return [Finding(WARNING, "managers.role_ids", "role_unknown",
                    f"Not in the server's role list{_source(env)}: {', '.join(unknown)}. It is saved as entered.")]


def _check_emoji(value: Any, env: Environment) -> list[Finding]:
    if not isinstance(value, Mapping) or env.emojis is None:
        return []   # unicode emoji or no directory: nothing to check
    ident = str(value.get("id"))
    if ident in env.emojis:
        return []
    return [Finding(WARNING, "identity.reaction_emoji", "emoji_unknown",
                    f"The emoji :{value.get('name')}: is not in the server's emoji list{_source(env)}. "
                    "The bot can only react with emojis of servers it is in.")]


def _check_profile(value: Any, env: Environment) -> list[Finding]:
    if value is None or env.profiles is None or str(value) in env.profiles:
        return []
    return [Finding(ERROR, "routing.profile", "profile_missing", f"There is no Hermes profile named {value}.")]


def flatten(guild: Mapping[str, Any], prefix: str = "") -> dict[str, Any]:
    """``{dotted path: value}`` of every leaf of a guild mapping (an import checked like a set of changes)."""
    out: dict[str, Any] = {}
    for key, value in guild.items():
        path = f"{prefix}{key}"
        if isinstance(value, Mapping) and path not in ("identity.reaction_emoji", "prompts.template_overrides"):
            out.update(flatten(value, path + "."))
        else:
            out[path] = value
    return out


def environment(changes: Mapping[str, Any], env: Environment) -> list[Finding]:
    """Findings for ``changes`` (``{dotted path: value}``) in ``env``; errors refuse the write."""
    findings: list[Finding] = []
    for path, value in changes.items():
        if path == "lanes.skills":
            findings += _check_skills(value, env)
        elif path in CHANNEL_FIELDS:
            findings += _check_channels(path, value, env)
        elif path == "managers.role_ids":
            findings += _check_roles(value, env)
        elif path == "identity.reaction_emoji":
            findings += _check_emoji(value, env)
        elif path == "routing.profile":
            findings += _check_profile(value, env)
    return findings
