"""Is the bot addressed, and in which conversation mode does a message run (DESIGN §2, §5.2, §10 S5).

Signals, strongest first:

1. **reaction** — the message a reaction wakeup builds for exactly one reacted-to message
   (``ingress.reactions``); it carries a :class:`ReactionWakeup` marker for its own reactor;
2. **mention** — Discord says the bot is mentioned (``adapter._self_is_explicitly_mentioned``);
3. **reply** — the referenced message was written by the bot (an unresolved reference is
   ``MAYBE_REPLY``: the handler verifies it before routing);
4. **wake name** — one of the server's wake names, spoken *to* the bot.

Bots, webhooks and system messages never wake the bot (:func:`is_human`, the single place where a lab
mode may later treat test bots as people, DESIGN §5.1/§9).

**Wake names.** A name counts only when it is addressed, not when someone talks *about* the bot (the
Alexa/Siri problem: "ask Nova later", "Nova is great", "I named it Nova"). The rules use the position of
the name in its clause and the pause marks of any script; nothing in them is specific to one language:

* the name opens the message or a sentence and is followed by a pause mark, the end of the text, or one
  of the server's greeting words ("Nova, …", "Nova?", "Nova", "Nova please …");
* a greeting word that opens the message or a sentence stands right before the name ("hey Nova …",
  "Hallo Nova", "ねえノヴァ、…");
* the name closes a sentence after a comma-like pause mark, optionally after a greeting word
  ("Was meinst du, Nova?", "thanks, hey Nova!");
* the name is typed with an ``@`` ("@nova …"), anywhere.

A greeting word touches the name without a space only in scripts written without spaces (CJK, kana,
Hangul, Thai, …: "ねえノヴァ", "ノヴァさん", "노바야"); in other scripts that is one word ("hiking" for a bot
named "King"). The rules judge where a name stands, not what the sentence means: a sentence that opens
with the name and a pause mark reads as an address even when it is about the bot ("Nova, Siri and Alexa
are assistants").

What depends on sentence structure — the words that mark a name as spoken to — is data of each server
(``identity.greetings``), never code. Names and greetings are matched as literal text (``re.escape``),
after Unicode normalisation (NFKC, case folding; full-width forms, ``ß``/``ss`` and variation selectors
compare equal), with word boundaries computed from Unicode categories, so names in any script, with
umlauts, emoji or CJK characters work. Quoted lines, code and Discord markup (mentions, custom emoji) never
count. A wake name too short to be distinctive (fewer than three letters, digits, symbols or vowel signs;
ideographs count double) is ignored, so a stray empty or one-letter entry cannot make every message
address the bot.

Without configured wake names the bot answers to its server name (``identity.bot_name``), else to its
Discord display name in the server (DESIGN §5.2 default ``[bot_name]``).
"""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass
from functools import lru_cache
from typing import Any, Mapping, Optional

from ..policy.routes import snowflake

DIRECT = "direct"
MAYBE_REPLY = "maybe_reply"   # a reply whose referenced message is not resolved yet
NONE = "none"

AMBIENT = "ambient"
WORKLOG = "worklog"           # = delivery.worklog.WORKLOG_MODE

TRIGGER_REACTION = "reaction"
TRIGGER_MENTION = "mention"
TRIGGER_REPLY = "reply"
TRIGGER_NAME = "wake_name"

WAKEUP_MARKER = "_dsp_reaction_wakeup"
DEFAULT_GREETINGS = ("hey", "hi", "yo", "ok", "okay", "please")  # = store.schema.DEFAULT_GREETINGS

_THREAD_TYPES = {10, 11, 12}
_UNSAFE = re.compile(r"[\x00-\x1f\x7f\[\]]")

MIN_NAME_WEIGHT = 3
MIN_GREETING_WEIGHT = 2
MAX_NAMES = 16
MAX_GREETINGS = 32
MAX_ENTRY_CHARS = 64
MAX_TEXT_CHARS = 8000
MAX_OCCURRENCES = 64     # word-bounded name occurrences examined per message
SCAN = 32                # characters a rule looks across (leading symbols, trailing marks)


# ── small Discord facts ──────────────────────────────────────────────────────────────────────────


def is_thread(channel: Any) -> bool:
    kind = getattr(channel, "type", None)
    value = getattr(kind, "value", kind)
    try:
        return int(value) in _THREAD_TYPES
    except (TypeError, ValueError):
        return False


def _self_id(adapter: Any) -> str:
    return str(getattr(getattr(getattr(adapter, "_client", None), "user", None), "id", "") or "")


def is_human_author(author: Any) -> bool:
    """A person: not a bot account, not Discord's system user."""
    return author is not None and not bool(getattr(author, "bot", False)) \
        and not bool(getattr(author, "system", False))


def is_human(message: Any) -> bool:
    """Written by a person: not a bot, not a webhook, not a system message (bots and webhooks never wake
    the bot). The one place a lab mode (DESIGN §5.1/§9) would widen."""
    if getattr(message, "webhook_id", None) is not None:
        return False
    return is_human_author(getattr(message, "author", None))


@dataclass(frozen=True)
class ReactionWakeup:
    """Marker on the message a reaction wakeup builds (``ingress.reactions``)."""

    bot_id: str
    guild_id: str
    channel_id: str
    message_id: str
    reactor_id: str
    emoji: str


def _reaction_marker(message: Any) -> Optional[ReactionWakeup]:
    marker = getattr(message, WAKEUP_MARKER, None)
    if not isinstance(marker, ReactionWakeup):
        return None
    author_id = str(getattr(getattr(message, "author", None), "id", "") or "")
    message_id = str(getattr(message, "id", "") or "")
    if not author_id or marker.reactor_id != author_id or marker.message_id != message_id:
        return None
    return marker


# ── text normalisation ───────────────────────────────────────────────────────────────────────────

_CODE_BLOCK = re.compile(r"```.*?(?:```|\Z)", re.S)
_CODE_SPAN = re.compile(r"`[^`\n]*`")
_MARKUP = re.compile(r"<(?:@[!&]?\d+|#\d+|a?:[^\s:<>]{1,64}:\d+|t:-?\d+(?::[A-Za-z])?|/[^\s<>]{1,64}:\d+"
                     r"|id:[a-z_]+)>")
_SELECTORS = re.compile("[\ufe00-\ufe0f\U000e0100-\U000e01ef]")
_EMPHASIS = str.maketrans("", "", "*_~|`")
_LINE_BREAKS = re.compile("\r\n|[\r\x0b\x0c\x1c-\x1e\x85\u2028\u2029]")
_SPACES = re.compile(r"[^\S\n]+")

# Pause marks that separate a name from what follows even without a space (any script, after NFKC).
_STRONG = frozenset(",!?:;、،؛؟。")
# Comma-like marks before a name that closes a sentence.
_COMMA = frozenset(",、،;:-–—‒―")
# Sentence ends, after which a new clause starts.
_TERMINATORS = frozenset(".!?。؟।॥۔።᙮")
# Punctuation after a space that does not set a name off ("Nova #2", "Nova @x", "Nova 'quote").
_NOT_PAUSE = frozenset("#@&*%/\\'\"")
_CLAUSE_LEAD = frozenset("¿¡#-•·")
_ZWJ = "\u200d"


def _cat(ch: str) -> str:
    return unicodedata.category(ch)


def _is_word(ch: str) -> bool:
    cat = _cat(ch)
    return cat[0] in "LMN" or cat == "Pc"


def _opens(ch: str) -> bool:
    """Opening brackets and initial quote marks (Unicode categories P-s and P-i)."""
    cat = _cat(ch)
    return cat[0] == "P" and cat[1] in "si"


def _is_continuation(ch: str) -> bool:
    """Characters that glue onto the previous character (combining, joiners, emoji modifiers)."""
    return _is_word(ch) or ch == _ZWJ or "\U0001f3fb" <= ch <= "\U0001f3ff"


def _fold(text: str) -> str:
    text = unicodedata.normalize("NFKC", text)
    text = unicodedata.normalize("NFKC", text.casefold())
    text = text.replace("i\u0307", "i")             # Turkish "İ" folds to "i" + combining dot: same as "i"/"I"
    return _SELECTORS.sub("", text)


# Scripts written without spaces between words, where an address word may touch the name ("ねえノヴァ",
# "ノヴァさん", "노바야"): wide characters (CJK, kana, Hangul) and these blocks: Thai, Lao, Tibetan, Myanmar,
# Khmer.
_UNSPACED_BLOCKS = ((0x0E00, 0x0FFF), (0x1000, 0x109F), (0x1780, 0x17FF))


def _unspaced(ch: str) -> bool:
    if unicodedata.east_asian_width(ch) in ("W", "F"):
        return True
    code = ord(ch)
    return any(low <= code <= high for low, high in _UNSPACED_BLOCKS)


def _glued(t: str, i: int) -> bool:
    """A greeting may touch the name at ``i`` without a space only between two characters of such scripts
    ("hi" + "king" in "hiking" is a word, not an address)."""
    return 0 < i < len(t) and _unspaced(t[i - 1]) and _unspaced(t[i])


def fold_entry(value: Any) -> str:
    """A wake name or greeting in the form messages are compared in (empty if unusable). Punctuation and a
    typed ``@`` around the entry are dropped ("@Nova", "Nova!", "hey," mean the word itself)."""
    if not isinstance(value, str):
        return ""
    text = _fold(value[:MAX_ENTRY_CHARS * 2]).translate(_EMPHASIS)
    text = _SPACES.sub(" ", _LINE_BREAKS.sub(" ", text)).strip()
    start, end = 0, len(text)
    while start < end and (text[start] == " " or _cat(text[start])[0] == "P"):
        start += 1
    while end > start and (text[end - 1] == " " or _cat(text[end - 1])[0] == "P"):
        end -= 1
    return text[start:end][:MAX_ENTRY_CHARS]


def fold_message(text: Any) -> str:
    """Message text for matching: no code, no quoted lines, Discord markup as a neutral symbol."""
    raw = str(text or "")[:MAX_TEXT_CHARS]
    raw = _LINE_BREAKS.sub("\n", raw)
    raw = _CODE_BLOCK.sub(" ", raw)
    raw = _CODE_SPAN.sub(" ", raw)
    kept = []
    for line in raw.split("\n"):
        lead = line.lstrip()
        if lead.startswith(">>>"):
            break                                  # a block quote runs to the end of the message
        if lead == ">" or lead.startswith("> "):
            continue                               # a quoted line is someone else's text
        kept.append(line)
    raw = _MARKUP.sub(" ￼ ", "\n".join(kept))
    folded = _fold(raw).translate(_EMPHASIS)
    folded = _LINE_BREAKS.sub("\n", folded)
    return _SPACES.sub(" ", folded)


def weight(entry: str) -> int:
    """How distinctive a folded name is: letters, digits, symbols and combining marks (the vowel signs of
    Indic scripts: "नोवा" is four) count one, wide characters two."""
    total = 0
    for ch in entry:
        cat = _cat(ch)
        if cat[0] in "LNM" or cat == "So":
            total += 2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1
    return total


# ── the vocative rules ───────────────────────────────────────────────────────────────────────────


def _continues_right(t: str, e: int) -> bool:
    """The text at ``e`` continues the word that ends there ("novak", "nova's", "nova-chan", "nova.exe")."""
    if e >= len(t):
        return False
    ch = t[e]
    if _is_continuation(ch):
        return True
    if ch in _STRONG or ch in " \n":
        return False
    return _cat(ch)[0] == "P" and e + 1 < len(t) and _is_word(t[e + 1])


def _clause_start(t: str, s: int) -> bool:
    p, floor = s, max(0, s - SCAN)
    while p > floor:
        ch = t[p - 1]
        if ch == " " or ch in _CLAUSE_LEAD or _cat(ch)[0] == "S" or _opens(ch):
            p -= 1
            continue
        break
    else:
        if p > 0:
            return False                           # a long run of symbols: not judged, not a clause start
    return p == 0 or t[p - 1] == "\n" or t[p - 1] in _TERMINATORS


def _left_free(t: str, s: int) -> bool:
    return s == 0 or not _is_continuation(t[s - 1])


def _pause_or_end_after(t: str, e: int) -> bool:
    f = e
    while f < len(t) and t[f] == " ":
        f += 1
    if f >= len(t) or t[f] == "\n":
        return True
    ch = t[f]
    if ch in _STRONG:
        return True
    if _is_word(ch) or _opens(ch) or ch in _NOT_PAUSE:
        return False
    return _cat(ch)[0] in "PS"


def _greeting_after(t: str, e: int, starts: dict[int, list[int]]) -> bool:
    f = e
    while f < len(t) and t[f] == " ":
        f += 1
    if f == e:
        return False
    return any(not _continues_right(t, end) for end in starts.get(f, ()))


def _greeting_before(t: str, s: int, ends: dict[int, list[int]]) -> bool:
    """A greeting that opens a clause stands right before the name (spaces/comma between, or glued)."""
    candidates = list(ends.get(s, ())) if _glued(t, s) else []    # glued: scripts without spaces ("ねえノヴァ")
    q, floor = s, max(0, s - SCAN)
    while q > floor and (t[q - 1] == " " or t[q - 1] in _STRONG):
        q -= 1
    if q != s:
        candidates.extend(ends.get(q, ()))
    return any(_left_free(t, start) and _clause_start(t, start) for start in candidates)


def _pause_before(t: str, s: int, ends: dict[int, list[int]]) -> bool:
    q = s
    while q > 0 and t[q - 1] == " ":
        q -= 1
    if q != s:
        for start in ends.get(q, ()):            # "…, hey Nova!"
            if not _left_free(t, start):
                continue
            r = start
            while r > 0 and t[r - 1] == " ":
                r -= 1
            if r > 0 and t[r - 1] in _COMMA:
                return True
    return q > 0 and t[q - 1] in _COMMA


def _clause_ends_after(t: str, e: int) -> bool:
    f, ceiling = e, e + SCAN
    while f < len(t):
        if f >= ceiling:
            return False                           # a long run of symbols: not judged, not a clause end
        ch = t[f]
        if ch == "\n" or ch in _TERMINATORS:
            return True
        cat = _cat(ch)
        if ch == " " or (cat[0] in "PS" and not _opens(ch)):
            f += 1
            continue
        return False
    return True


def _word_bounded(t: str, s: int, e: int, greet_ends: dict[int, list[int]],
                  greet_starts: dict[int, list[int]]) -> bool:
    """Constant-time precondition of every rule: the name is not glued into a longer word on either side
    (a greeting glued on — "ねえノヴァ", "ノヴァさん" — does not count as a longer word)."""
    if not (_left_free(t, s) or (s in greet_ends and _glued(t, s))):
        return False
    return any(not _continues_right(t, end) for end in _name_ends(t, e, greet_starts))


def _name_ends(t: str, e: int, greet_starts: dict[int, list[int]]) -> tuple[int, ...]:
    """Where the name ends: right after it, or after an address word of the server glued to it in a script
    without spaces ("ノヴァさん", "노바야")."""
    return (e, *greet_starts.get(e, ())) if _glued(t, e) else (e,)


def _spoken_to(t: str, s: int, e: int, greet_ends: dict[int, list[int]],
               greet_starts: dict[int, list[int]]) -> bool:
    # An address word of the server glued to the name counts as part of it ("ノヴァさん", "노바야").
    return any(_spoken_at(t, s, end, greet_ends, greet_starts) for end in _name_ends(t, e, greet_starts))


def _spoken_at(t: str, s: int, e: int, greet_ends: dict[int, list[int]],
               greet_starts: dict[int, list[int]]) -> bool:
    if _continues_right(t, e):
        return False
    if s > 0 and t[s - 1] == "@" and (s == 1 or not _is_word(t[s - 2])):
        return True
    free = _left_free(t, s)
    if free and _clause_start(t, s) and (_pause_or_end_after(t, e) or _greeting_after(t, e, greet_starts)):
        return True
    if _greeting_before(t, s, greet_ends):
        return True
    return free and _pause_before(t, s, greet_ends) and _clause_ends_after(t, e)


@dataclass(frozen=True)
class Identity:
    """Compiled wake names and greetings of one guild configuration."""

    names: tuple[str, ...]
    greetings: tuple[str, ...]
    name_re: Optional["re.Pattern[str]"]
    greeting_re: Optional["re.Pattern[str]"]

    def spoken_to(self, text: Any) -> bool:
        if self.name_re is None:
            return False
        t = fold_message(text)
        greet_ends: dict[int, list[int]] = {}
        greet_starts: dict[int, list[int]] = {}
        if self.greeting_re is not None:
            for match in self.greeting_re.finditer(t):
                start, end = match.start(), match.start() + len(match.group(1))
                greet_ends.setdefault(end, []).append(start)
                greet_starts.setdefault(start, []).append(end)
        examined = 0
        for match in self.name_re.finditer(t):
            start = match.start()
            end = start + len(match.group(1))
            if not _word_bounded(t, start, end, greet_ends, greet_starts):
                continue                           # inside a longer word: rejected cheaply, not counted
            examined += 1
            if examined > MAX_OCCURRENCES:
                break
            if _spoken_to(t, start, end, greet_ends, greet_starts):
                return True
        return False


def _alternation(entries: tuple[str, ...]) -> Optional["re.Pattern[str]"]:
    if not entries:
        return None
    ordered = sorted(entries, key=len, reverse=True)
    # A lookahead finds every position (also overlapping ones); each entry is literal text.
    return re.compile("(?=(" + "|".join(re.escape(entry) for entry in ordered) + "))")


@lru_cache(maxsize=256)
def _compile(rev: Any, names: tuple[str, ...], greetings: tuple[str, ...]) -> Identity:
    """Cached per configuration revision; the folded entries are part of the key, so a configuration whose
    ``rev`` did not move (an import, a second bot with the same numbering) never gets stale patterns."""
    return Identity(names=names, greetings=greetings, name_re=_alternation(names),
                    greeting_re=_alternation(greetings))


def _entries(raw: Any, limit: int, minimum: int) -> tuple[str, ...]:
    if not isinstance(raw, (list, tuple)):
        return ()
    out: list[str] = []
    for value in list(raw)[:limit]:
        entry = fold_entry(value)
        if entry and weight(entry) >= minimum and entry not in out:
            out.append(entry)
    return tuple(out)


def compile_identity(guild_config: Any, fallback_name: str = "") -> Identity:
    """Wake names (else ``bot_name``, else ``fallback_name``) and greetings of a guild configuration."""
    cfg = guild_config if isinstance(guild_config, Mapping) else {}
    identity = cfg.get("identity")
    identity = identity if isinstance(identity, Mapping) else {}
    raw_names = identity.get("wake_names")
    names = _entries(raw_names, MAX_NAMES, MIN_NAME_WEIGHT)
    if not (isinstance(raw_names, (list, tuple)) and any(isinstance(v, str) and v.strip() for v in raw_names)):
        bot_name = identity.get("bot_name")
        fallback = bot_name if isinstance(bot_name, str) and bot_name.strip() else fallback_name
        names = _entries([fallback], 1, MIN_NAME_WEIGHT)
    greetings = _entries(identity.get("greetings", DEFAULT_GREETINGS), MAX_GREETINGS, MIN_GREETING_WEIGHT)
    rev = cfg.get("rev")
    return _compile(rev if isinstance(rev, int) and not isinstance(rev, bool) else None, names, greetings)


def _display_name(adapter: Any, message: Any) -> str:
    me = getattr(getattr(message, "guild", None), "me", None)
    for holder in (me, getattr(getattr(adapter, "_client", None), "user", None)):
        for attr in ("display_name", "name"):
            value = getattr(holder, attr, None) if holder is not None else None
            if isinstance(value, str) and value.strip():
                return value
    return ""


def wake_name_spoken(adapter: Any, message: Any, guild_config: Any) -> bool:
    """One of the guild's wake names is spoken to the bot in this message."""
    if not isinstance(guild_config, Mapping):
        return False
    identity = compile_identity(guild_config, _display_name(adapter, message))
    return identity.spoken_to(getattr(message, "content", "") or "")


# ── signals ──────────────────────────────────────────────────────────────────────────────────────


def _direct(adapter: Any, message: Any, guild_config: Any) -> tuple[str, str]:
    if not is_human(message):
        return NONE, ""
    if _reaction_marker(message) is not None:
        return DIRECT, TRIGGER_REACTION
    mentioned = getattr(adapter, "_self_is_explicitly_mentioned", None)
    if callable(mentioned) and bool(mentioned(message)):
        return DIRECT, TRIGGER_MENTION
    maybe = False
    reference = getattr(message, "reference", None)
    if reference is not None:
        self_id = _self_id(adapter)
        resolved = getattr(reference, "resolved", None)
        author_id = str(getattr(getattr(resolved, "author", None), "id", "") or "")
        if resolved is not None and author_id:
            if self_id and author_id == self_id:
                return DIRECT, TRIGGER_REPLY
        elif getattr(reference, "message_id", None) is not None:
            maybe = True
    if guild_config is not None and wake_name_spoken(adapter, message, guild_config):
        return DIRECT, TRIGGER_NAME
    if maybe:
        return MAYBE_REPLY, TRIGGER_REPLY
    return NONE, ""


def direct_signal(adapter: Any, message: Any, guild_config: Any = None) -> str:
    """``DIRECT`` / ``MAYBE_REPLY`` / ``NONE``. With ``guild_config`` the guild's wake names count too."""
    return _direct(adapter, message, guild_config)[0]


@dataclass(frozen=True)
class Signal:
    direct: str = NONE              # DIRECT | MAYBE_REPLY | NONE
    trigger: str = ""               # reaction | mention | reply | wake_name | ""
    human: bool = True              # written by a person (bots and webhooks never wake the bot)
    ambient: bool = False           # an ambient channel or a thread beneath one, and not excluded
    excluded: bool = False          # a "never respond in" channel or a thread beneath one
    worklog_area: bool = False      # the worklog channel or a thread beneath it
    worklog_thread: bool = False    # a thread beneath the worklog channel: an operator turn continues there

    @property
    def mode(self) -> str:
        """Conversation mode: ``worklog`` | ``direct`` | ``ambient`` | ``maybe_reply`` | ``none``.

        ``worklog`` is for operator messages (public messages in the worklog area are not admitted).
        ``maybe_reply``: a reply whose reference is not resolved yet, outside ambient channels — the handler
        verifies it (``verify_reply_to_self``) and runs it as ``direct`` or as unaddressed. ``none``: not
        addressed and not ambient."""
        if self.worklog_thread:
            return WORKLOG
        if self.direct == DIRECT:
            return DIRECT
        if self.ambient:
            return AMBIENT
        if self.direct == MAYBE_REPLY:
            return MAYBE_REPLY
        return NONE

    @property
    def route_mode(self) -> str:
        """The mode a route of this message carries: one of ``worklog``, ``direct``, ``ambient`` (the modes
        ``prompts.assemble`` knows; any other mode blocks public turns). An unresolved reply or an
        unaddressed message outside ambient channels runs as ``direct``: the handler verifies the reply
        (:attr:`reply_unverified`), and an unaddressed message is left to stock Hermes (:attr:`addressed`)."""
        if self.worklog_thread:
            return WORKLOG
        if self.direct == DIRECT:
            return DIRECT
        return AMBIENT if self.ambient else DIRECT

    @property
    def reply_unverified(self) -> bool:
        """An unresolved reply outside ambient channels: addressed only if it turns out to answer the bot."""
        return self.direct == MAYBE_REPLY and not self.ambient

    @property
    def addressed(self) -> bool:
        """Mention, reply (possibly unverified), reaction wakeup, wake name, or an ambient channel."""
        return self.direct != NONE or self.ambient


def _ids(raw: Any) -> set[str]:
    if not isinstance(raw, (list, tuple, set, frozenset)):
        return set()
    return {s for s in (snowflake(v) for v in raw) if s}


def _worklog_facts(adapter: Any, message: Any, cfg: Mapping[str, Any], scope: set[str], channel_id: str,
                   guild_id: str) -> tuple[bool, bool]:
    from ..delivery import worklog  # late: delivery imports the runtime anchor

    in_thread = is_thread(getattr(message, "channel", None))
    settings = worklog.parse_settings(dict(cfg))
    area = bool(settings.channel_id and settings.channel_id in scope)
    thread = area and in_thread and settings.channel_id != channel_id
    opened = worklog.DELIVERY.threads.get(channel_id)
    if isinstance(opened, tuple) and len(opened) == 2 and str(opened[1]) == guild_id:
        area = True
        thread = thread or in_thread
    return area, thread


def classify(adapter: Any, message: Any, guild_config: Any) -> Signal:
    """Every conversation signal of one guild message under one guild configuration (DESIGN §5.3 dict)."""
    cfg = guild_config if isinstance(guild_config, Mapping) else None
    channel = getattr(message, "channel", None)
    channel_id = snowflake(getattr(channel, "id", None)) or ""
    parent_id = snowflake(getattr(channel, "parent_id", None)) if is_thread(channel) else None
    guild_id = snowflake(getattr(getattr(message, "guild", None), "id", None)) or ""
    scope = {i for i in (channel_id, parent_id) if i}
    conversation = (cfg or {}).get("conversation")
    conversation = conversation if isinstance(conversation, Mapping) else {}
    excluded = bool(scope & _ids(conversation.get("excluded_channels")))
    ambient = not excluded and bool(scope & _ids(conversation.get("ambient_channels")))
    area, thread = _worklog_facts(adapter, message, cfg or {}, scope, channel_id, guild_id)
    direct, trigger = _direct(adapter, message, cfg)
    return Signal(direct=direct, trigger=trigger, human=is_human(message), ambient=ambient, excluded=excluded,
                  worklog_area=area, worklog_thread=thread)


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


__all__ = [
    "AMBIENT", "DIRECT", "Identity", "MAYBE_REPLY", "NONE", "ReactionWakeup", "Signal", "TRIGGER_MENTION",
    "TRIGGER_NAME", "TRIGGER_REACTION", "TRIGGER_REPLY", "WAKEUP_MARKER", "WORKLOG", "classify", "clean_label",
    "compile_identity", "direct_signal", "fold_entry", "fold_message", "is_human", "is_human_author",
    "is_thread", "verify_reply_to_self", "wake_name_spoken", "weight",
]
