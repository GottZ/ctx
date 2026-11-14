"""Field catalogue of the settings page: labels, help, control kind and limits per schema field.

Sections are the settings of ``ui.perms.TABLE`` that write schema fields, in table order (each section saves
on its own, with the guild's ``rev`` — the same granularity as the Discord panels, S10). Limits restate the
schema for the browser's provisional checks; the server's ``validate_guild`` stays authoritative (a unit test
pins that the two agree). A field without an entry here (one a later layer adds) still appears, generically.
"""

from __future__ import annotations

from typing import Any, Optional

from ..store import schema
from ..ui import perms

# Written only by their own operations, never as a form field.
HIDDEN = frozenset({"prompts.history"})

_ARCHIVE_LABELS = {60: "1 hour", 1440: "24 hours", 4320: "3 days", 10080: "7 days"}
_GROUPING_LABELS = {"default": "Hermes default", "rotate": "Newest line only", "separate": "One message per tool"}
_MANAGER_LABELS = {"discord_admins": "Server admins (Manage Server)", "role": "Admins with one of these roles",
                   "operators_only": "Operators only"}

FIELDS: dict[str, dict[str, Any]] = {
    "identity.bot_name": {"label": "Name the bot uses here", "kind": "text", "max_length": 32,
                          "help": "Shown to the model as the bot's name in this server. Empty: the bot's "
                                  "Discord display name."},
    "identity.wake_names": {"label": "Names that wake it", "kind": "text_list", "max_items": 5, "max_length": 32,
                            "help": "Messages that start with one of these names count as addressing the bot."},
    "identity.greetings": {"label": "Greeting words", "kind": "text_list", "max_items": 12, "max_length": 24,
                           "help": "Words that may come before a wake name (\"hey Name\")."},
    "identity.reaction_emoji": {"label": "Reaction emoji", "kind": "emoji",
                                "help": "Reacting with this emoji to a message asks the bot to answer it. A unicode "
                                        "emoji or one of the server's emojis."},
    "conversation.language": {"label": "Reply language", "kind": "text", "max_length": 100,
                              "help": "For example \"English\" or \"the language of the message\"."},
    "conversation.ambient_channels": {"label": "Ambient listening", "kind": "channel_list", "max_items": 100,
                                      "help": "In these channels the bot reads every message and may join the "
                                              "conversation by itself."},
    "conversation.excluded_channels": {"label": "Never respond in", "kind": "channel_list", "max_items": 100,
                                       "help": "The bot never answers in these channels, not even when addressed."},
    "prompts.guild": {"label": "Server guidance", "kind": "guidance", "max_length": schema.HARD_PROMPT_LIMIT,
                      "help": "Tone, topics and community norms of this server, in your own words. It cannot "
                              "grant tools or change who the operator is."},
    "prompts.guild_in_operator_lane": {"label": "Also apply in operator turns", "kind": "bool",
                                       "help": "Off: the server's guidance is left out when the operator talks to "
                                               "the bot, so it can never steer the operator's tools."},
    "progress.grouping": {"label": "Progress messages", "kind": "choice",
                          "choices": [{"value": v, "label": _GROUPING_LABELS[v]} for v in schema.GROUPINGS],
                          "help": "How the bot shows what it is doing while it works."},
    "worklog.channel": {"label": "Worklog channel", "kind": "channel",
                        "help": "Tool output of operator turns goes into a thread in this channel. Empty: no worklog."},
    "worklog.archive_minutes": {"label": "Archive after", "kind": "choice",
                                "choices": [{"value": v, "label": _ARCHIVE_LABELS[v]} for v in schema.ARCHIVE_MINUTES],
                                "help": "Worklog threads are archived after this time without activity."},
    "worklog.final_to_origin": {"label": "Final copy to the original channel", "kind": "bool",
                                "help": "Post the final answer also where the operator asked."},
    "lanes.channel_history.enabled": {"label": "Read the current channel's history", "kind": "bool",
                                      "help": "Members' turns may read recent messages of the channel they are in."},
    "lanes.channel_history.max_limit": {"label": "Messages at most", "kind": "int", "min": 1, "max": 100,
                                        "help": "Upper bound for one history read (1–100)."},
    "lanes.permalink_hydration": {"label": "Follow message links", "kind": "bool",
                                  "help": "Links to messages of this server are shown to the model."},
    "lanes.skills": {"label": "Skills members may use", "kind": "skill_list", "max_items": 25,
                     "help": "Members' turns may read these skills. They must be installed in the profile this "
                             "server's turns run in."},
    "lanes.knowledge": {"label": "Knowledge references", "kind": "knowledge_list", "max_items": 25,
                        "help": "Approved lookups members' turns may make: an MCP read tool and one id each."},
    "operator_lane.channel_hydration": {"label": "Read the channel around the operator's message", "kind": "bool",
                                        "help": "Operator turns see the recent conversation of the channel."},
    "managers.mode": {"label": "Who may change settings in Discord", "kind": "choice",
                      "choices": [{"value": v, "label": _MANAGER_LABELS[v]} for v in schema.MANAGER_MODES],
                      "help": "Operators always may. This decides who else may use /policy in this server."},
    "managers.role_ids": {"label": "Roles", "kind": "role_list", "max_items": 25,
                          "help": "Used with \"Admins with one of these roles\"."},
    "audit_channel": {"label": "Audit channel", "kind": "channel",
                      "help": "Changes of this server's settings are announced here. Empty: none."},
    "prompts.template_overrides": {"label": "Template overrides", "kind": "templates", "max_items": 16,
                                   "max_length": 8000,
                                   "help": "Replace one of the built-in prompt templates for this server."},
}

SECTION_LABELS = {
    "identity": "Identity", "channels": "Channels", "guidance": "Guidance", "progress": "Progress",
    "worklog": "Worklog", "public_lanes": "Public lanes", "operator_lane": "Operator turns",
    "managers": "Management", "templates": "Advanced: template overrides", "profile": "Routing",
}


def _default(path: str) -> Any:
    node: Any = schema.guild_defaults()
    for part in path.split("."):
        if not isinstance(node, dict) or part not in node:
            return None
        node = node[part]
    return node


def field(path: str) -> dict[str, Any]:
    entry = dict(FIELDS.get(path) or {"label": path, "kind": "readonly", "help": ""})
    entry["path"] = path
    entry["default"] = _default(path)
    return entry


def sections() -> list[dict[str, Any]]:
    out = []
    for setting in perms.TABLE:
        paths = [p for p in setting.fields if p not in HIDDEN]
        if not paths:
            continue
        out.append({"key": setting.key, "label": SECTION_LABELS.get(setting.key, setting.label[:1].upper()
                                                                     + setting.label[1:]),
                    "note": setting.note, "fields": [field(p) for p in paths]})
    return out


def section_of(path: str) -> Optional[str]:
    owners = perms.owners_of(path)
    return owners[0] if owners else None
