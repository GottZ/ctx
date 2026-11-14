"""Placeholder rendering for prompt templates and server guidance (DESIGN §7.4, OPEN L13).

Substitution runs through one regular expression over a fixed whitelist of names. Nothing in the prompt
engine calls ``str.format``, ``format_map``, ``string.Template`` or ``%``: a template or a server's guidance
may contain ``{x.__class__}``, ``{0}`` or any other format field and it stays literal text (reported as an
unknown placeholder, so the guidance preview can warn about it). ``{{`` and ``}}`` yield ``{`` and ``}``.
Substitution is a single pass, so a value that itself looks like a placeholder is never expanded again.

Every substituted value is sanitised, whoever supplied it. Server, channel and bot names are chosen by
Discord users: they lose control, format and other invisible characters, every kind of line break and
leading Markdown block markup, and square brackets (including their full-width and CJK lookalikes) become
parentheses. A value can therefore never begin a line of its own or imitate a ``[section header]`` of the
assembled context. Values are capped at 100 characters, the bot name at 32 (Discord's nickname limit).

Text that a server's managers write into the guild configuration (bot name, reply language) is used only
where their server guidance is used too: always in public turns, in operator turns only when the operator
switched the guidance on for that guild (``use_guild_text``). Otherwise the bot's Discord display name and
the neutral default language apply.
"""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path
from typing import Any, Mapping, Optional

PLACEHOLDERS: tuple[str, ...] = (
    "bot_name", "server_name", "channel_name", "channel", "language", "worklog_channel", "operator_label",
)
VALUE_LIMIT = 100
# Tighter caps per placeholder; everything else uses VALUE_LIMIT.
VALUE_LIMITS: dict[str, int] = {"bot_name": 32}

# Rendered when the configuration or the turn does not provide a value.
FALLBACKS: dict[str, str] = {
    "bot_name": "the assistant",
    "server_name": "this server",
    "channel_name": "this channel",
    "channel": "this channel",
    "language": "the language of the message",
    "worklog_channel": "the work log channel",
    "operator_label": "the operator",
}

# Templates shipped in ``templates/<name>.txt``; each one may be replaced by the operator (DESIGN §7.1).
TEMPLATES: tuple[str, ...] = ("frame", "ambient", "direct", "worklog", "reaction_wakeup")
TEMPLATE_DIR = Path(__file__).resolve().with_name("templates")

# One token per match, left to right: an escape, a whitelisted placeholder, or any other brace field
# without whitespace (kept literally and reported). Everything else is plain text.
_TOKEN = re.compile(r"\{\{|\}\}|\{(" + "|".join(PLACEHOLDERS) + r")\}|\{([^{}\s]{1,64})\}")
_WHITESPACE = re.compile(r"\s+")
_LEADING_MARKUP = re.compile(r"^[\s#>`~|=-]+")
_SNOWFLAKE = re.compile(r"[0-9]{1,20}")
# Zero-width (non-)joiners are format characters too, but they hold emoji sequences and several scripts
# together; they cannot break a line or reorder text.
_KEPT_FORMAT_CHARS = frozenset("‌‍")
_DROPPED_CATEGORIES = frozenset({"Cc", "Cf", "Cs"})
# Invisible characters outside Cc/Cf: combining grapheme joiner, Hangul and Khmer fillers, braille blank,
# and the variation selectors supplement (256 selectors can smuggle a byte payload past a human reader).
_INVISIBLE = re.compile("[\u034f\u115f\u1160\u17b4\u17b5\u2800\u3164\uffa0\U000e0100-\U000e01ef]")
# A base character takes at most one variation selector (emoji/text presentation); runs carry payloads.
_SELECTOR_RUN = re.compile("([\ufe00-\ufe0f])[\ufe00-\ufe0f]+")
_BRACKETS = str.maketrans({
    "[": "(", "]": ")", "\uff3b": "(", "\uff3d": ")", "\u3010": "(", "\u3011": ")", "\u3014": "(", "\u3015": ")",
    "\u3016": "(", "\u3017": ")", "\u3018": "(", "\u3019": ")", "\u301a": "(", "\u301b": ")", "\u27e6": "(",
    "\u27e7": ")", "\u2045": "(", "\u2046": ")", "\ufe5d": "(", "\ufe5e": ")",
})


@dataclass(frozen=True)
class Rendered:
    text: str
    unknown: tuple[str, ...] = ()   # brace fields that were left literal, in order of first appearance


def _drop_invisible(text: str) -> str:
    text = "".join(ch for ch in text
                   if ch in _KEPT_FORMAT_CHARS or unicodedata.category(ch) not in _DROPPED_CATEGORIES)
    return _SELECTOR_RUN.sub(r"\1", _INVISIBLE.sub("", text))


def sanitize_value(value: Any, *, limit: int = VALUE_LIMIT) -> str:
    """One line of plain text that cannot start a section, a heading or a quote of its own."""
    text = "" if value is None else str(value)
    text = _WHITESPACE.sub(" ", text)          # every line break (\\r, \\u2028, \\x85, ...) and tab
    text = _drop_invisible(text)               # remaining control, bidi and zero-width characters
    text = text.translate(_BRACKETS)
    text = _WHITESPACE.sub(" ", text).strip()
    text = _LEADING_MARKUP.sub("", text)
    limit = max(1, int(limit))
    if len(text) > limit:
        text = text[:limit - 1].rstrip() + "…"
    return text


def sanitize_block(text: Any) -> str:
    """Multi-line text from a server administrator: lines kept, invisible and control characters dropped.

    Every Unicode line boundary (``str.splitlines``) becomes ``\\n`` so the caller can prefix each line.
    """
    lines = []
    for line in str(text or "").splitlines():
        lines.append(_drop_invisible(line.replace("\t", "    ")).rstrip())
    return "\n".join(lines).strip("\n")


def channel_mention(value: Any) -> str:
    """``<#id>`` for a Discord snowflake, otherwise empty."""
    if isinstance(value, bool) or value is None:
        return ""
    text = str(value)
    return f"<#{text}>" if _SNOWFLAKE.fullmatch(text) else ""


def render(template: str, values: Mapping[str, Any]) -> Rendered:
    """Substitute whitelisted placeholders with sanitised values; leave every other field literal."""
    unknown: list[str] = []

    def token(match: "re.Match[str]") -> str:
        whole = match.group(0)
        if whole == "{{":
            return "{"
        if whole == "}}":
            return "}"
        name = match.group(1)
        if name is not None:
            return sanitize_value(values.get(name, ""), limit=VALUE_LIMITS.get(name, VALUE_LIMIT))
        if whole not in unknown:
            unknown.append(whole)
        return whole

    text = _TOKEN.sub(token, str(template))
    return Rendered(text=text, unknown=tuple(unknown))


def _section(mapping: Any, key: str) -> Mapping[str, Any]:
    value = mapping.get(key) if isinstance(mapping, Mapping) else None
    return value if isinstance(value, Mapping) else {}


def placeholder_values(
    settings: Optional[Mapping[str, Any]],
    guild: Optional[Mapping[str, Any]],
    *,
    server_name: Any = "",
    channel_name: Any = "",
    channel_id: Any = "",
    bot_display_name: Any = "",
    use_guild_text: bool = True,
) -> dict[str, str]:
    """Resolved, sanitised placeholder values for one turn (global settings §5.1, guild state §5.3).

    ``use_guild_text=False`` ignores the bot name and reply language written by the server's managers
    (operator turns without server guidance): the Discord display name and the default language apply.
    """
    settings = settings if isinstance(settings, Mapping) else {}
    identity = _section(guild, "identity") if use_guild_text else {}
    conversation = _section(guild, "conversation") if use_guild_text else {}
    worklog = _section(guild, "worklog")
    bot_name = identity.get("bot_name")
    raw = {
        "bot_name": bot_name if isinstance(bot_name, str) and bot_name.strip() else bot_display_name,
        "server_name": server_name,
        "channel_name": channel_name,
        "channel": channel_mention(channel_id),
        "language": conversation.get("language"),
        "worklog_channel": channel_mention(worklog.get("channel")),
        "operator_label": settings.get("operator_label"),
    }
    out: dict[str, str] = {}
    for name in PLACEHOLDERS:
        value = raw[name]
        clean = sanitize_value(value, limit=VALUE_LIMITS.get(name, VALUE_LIMIT)) if isinstance(value, str) else ""
        out[name] = clean or FALLBACKS[name]
    return out


@lru_cache(maxsize=None)
def load_template(name: str) -> str:
    """The shipped default text of one template (``KeyError`` for any name outside ``TEMPLATES``)."""
    if name not in TEMPLATES:
        raise KeyError(name)
    return (TEMPLATE_DIR / f"{name}.txt").read_text(encoding="utf-8").strip()
