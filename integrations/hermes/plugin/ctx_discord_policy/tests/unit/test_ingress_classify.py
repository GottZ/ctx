"""S5: when is the bot addressed — mention, reply, reaction, wake name — and in which conversation mode.

Two servers with different wake names, greetings and languages. Every name and id is invented. The wake
name rules are generic (position of the name in its clause, pause marks of any script); the words around
the name that depend on sentence structure are data of each server (``identity.greetings``).
"""

from __future__ import annotations

import copy
from types import SimpleNamespace

import pytest
from unit.test_security_core import world  # noqa: F401 - the S3 world fixture (security core end to end)

BOT = "920000000000000001"
GUILD_A = "920000000000000100"
GUILD_B = "920000000000000200"
GENERAL_A = "920000000000000110"
AMBIENT_A = "920000000000000111"
EXCLUDED_A = "920000000000000112"
WORKLOG_A = "920000000000000113"
GENERAL_B = "920000000000000210"
MEMBER = "920000000000000008"
OTHER = "920000000000000009"


def guild_a(anchor, **identity):
    cfg = anchor.mod("store.schema").guild_defaults()
    cfg["state"], cfg["rev"] = "active", 3
    cfg["identity"].update({"bot_name": "Nova", "wake_names": ["Nova", "Jörg"],
                            "greetings": ["hey", "hallo", "servus", "bitte", "please", "grüß dich"]})
    cfg["identity"].update(identity)
    cfg["conversation"].update({"ambient_channels": [AMBIENT_A], "excluded_channels": [EXCLUDED_A]})
    cfg["worklog"]["channel"] = WORKLOG_A
    return cfg


def guild_b(anchor, **identity):
    cfg = anchor.mod("store.schema").guild_defaults()
    cfg["state"], cfg["rev"] = "active", 9
    cfg["identity"].update({"bot_name": "ノヴァ", "wake_names": ["ノヴァ", "Нова", "Pixel🦊", "نوفا"],
                            "greetings": ["ねえ", "привет", "hi", "مرحبا"]})
    cfg["identity"].update(identity)
    return cfg


class Adapter:
    def __init__(self, bot_name="bot"):
        self._client = SimpleNamespace(user=SimpleNamespace(id=int(BOT), bot=True, name=bot_name,
                                                            display_name=bot_name))

    def _self_is_explicitly_mentioned(self, message):
        return any(str(getattr(m, "id", "")) == BOT for m in getattr(message, "mentions", []) or [])


def msg(content="hello there", *, guild=GUILD_A, channel=GENERAL_A, author=MEMBER, mentions=(), reference=None,
        channel_type=0, parent_id=None, bot=False, webhook_id=None, mid=4242, me_name=None):
    chan = SimpleNamespace(id=int(channel), type=channel_type, parent_id=int(parent_id) if parent_id else None)
    g = SimpleNamespace(id=int(guild))
    if me_name is not None:
        g.me = SimpleNamespace(id=int(BOT), display_name=me_name)
    return SimpleNamespace(id=mid, content=content, guild=g, channel=chan, mentions=list(mentions),
                           author=SimpleNamespace(id=int(author), bot=bot, display_name="member"),
                           reference=reference, webhook_id=webhook_id)


@pytest.fixture
def c(anchor):
    return anchor.mod("ingress.classify")


def spoken(c, anchor, text, cfg=None):
    return c.classify(Adapter(), msg(text), cfg if cfg is not None else guild_a(anchor)).trigger == "wake_name"


# ── wake names: spoken to the bot ──────────────────────────────────────────────────────────────────


@pytest.mark.parametrize("text", [
    "Nova, what do you think?",
    "nova: summary please",
    "NOVA!",
    "Nova",
    "Nova?",
    "  Nova — quick question",
    "hey Nova can you weigh in?",
    "hey nova",
    "hey, Nova, look at this",
    "Hallo Nova, wie geht's?",
    "Servus Jörg, schau mal",
    "JÖRG, hilf mir bitte",
    "grüß dich Nova",
    "Nova bitte fass das zusammen",
    "Nova please summarize this",
    "Danke für den Link. Nova, was meinst du dazu?",
    "first line\nNova, second line asks",
    "Was meinst du, Nova?",
    "danke, Nova 🙏",
    "thanks for that, hey Nova!",
    "**Nova**, bold call",
    "👋 Nova, morning",
    "¿Nova, estás ahí?",
    "@nova can you check",
    "I think @Nova knows",
    "Nova <:wave:920000000000000777> hi",
])
def test_server_a_wake_names_spoken_to(c, anchor, text):
    assert spoken(c, anchor, text), text


@pytest.mark.parametrize("text", [
    "ask Nova later",
    "Nova is great",
    "Nova was a madman",
    "nope, nova was a madman",
    "I named that robin Nova",
    "Nova's answer was wrong",
    "Novak, come here",
    "Supernova, what a word",
    "Have you seen Nova today?",
    "Jorg, schau mal",
    "nova-chan is cute",
    "nova.exe crashed",
    "Tom, Nova und ich gehen",
    "> Nova, do the thing\nwhat did they say there",
    "`Nova, run` is the syntax",
    "```\nNova, run\n```",
    "mail me at me@nova.example",
    "hey novak",
    "this Nova is broken",
])
def test_server_a_third_person_and_lookalikes_are_not_spoken_to(c, anchor, text):
    assert not spoken(c, anchor, text), text


@pytest.mark.parametrize("text", [
    "ノヴァ、これ見て",
    "ねえノヴァ、手伝って",
    "ノヴァ？",
    "Нова, помоги мне",
    "Привет, Нова!",
    "привет нова как дела",
    "Pixel🦊, hi",
    "hi pixel🦊",
    "pixel🦊!",
    "نوفا، ساعدني",
    "مرحبا نوفا",
    "ＰＩＸＥＬ🦊、ちょっといい？",
    "ＮＯＶＡ is not my name. ノヴァ、 is",
])
def test_server_b_wake_names_in_other_scripts(c, anchor, text):
    assert spoken(c, anchor, text, guild_b(anchor)), text


@pytest.mark.parametrize("text", [
    "ノヴァは便利だ",
    "Я спросил Нову вчера",
    "Нова работает",
    "Pixel is a word",
    "pixel🦊 can you",
    "Nova, what do you think?",          # server A's name means nothing in server B
    "hey Nova",
    "سألت نوفا أمس",
])
def test_server_b_does_not_wake_on_third_person_or_server_a_names(c, anchor, text):
    assert not spoken(c, anchor, text, guild_b(anchor)), text


def test_address_suffixes_glued_to_the_name_are_data_too(c, anchor):
    plain = guild_b(anchor)
    suffixed = guild_b(anchor, greetings=["ねえ", "さん", "ちゃん"])
    assert not spoken(c, anchor, "ノヴァさん、手伝って", plain)
    assert spoken(c, anchor, "ノヴァさん、手伝って", suffixed)
    assert spoken(c, anchor, "ねえノヴァちゃん", suffixed)
    assert not spoken(c, anchor, "ノヴァさんは優しい", suffixed)          # "Nova-san is kind": third person
    korean = guild_b(anchor, wake_names=["노바"], greetings=["야"])
    assert spoken(c, anchor, "노바야, 도와줘", korean) and not spoken(c, anchor, "노바는 좋아", korean)


def test_long_runs_of_symbols_cost_linear_time(c, anchor):
    import time

    for names, text in ((["★★★"], "x " + "★" * 7998), (["🦊🦊"], "🦊" * 7998), (["Nova"], "Nova " * 1600)):
        cfg = guild_a(anchor, wake_names=names)
        started = time.perf_counter()
        spoken(c, anchor, text, cfg)
        assert time.perf_counter() - started < 0.5, names


@pytest.mark.parametrize("name,text,third", [
    ("नोवा", "नोवा, मदद करो", "मैंने नोवा से पूछा"),                 # Devanagari
    ("নোভা", "নোভা, সাহায্য করো", "আমি নোভা কে বললাম"),            # Bengali
    ("நோவா", "நோவா, உதவி செய்", "நான் நோவா பற்றி பேசினேன்"),       # Tamil
])
def test_names_in_indic_scripts_count_their_vowel_signs(c, anchor, name, text, third):
    """Two consonants and two vowel signs are a four-character name, not a two-letter one."""
    cfg = guild_b(anchor, wake_names=[name])
    assert c.compile_identity(cfg).names == (c.fold_entry(name),)
    assert spoken(c, anchor, text, cfg) and not spoken(c, anchor, third, cfg)


def test_turkish_dotted_capital_i_is_the_letter_i(c, anchor):
    cfg = guild_a(anchor, wake_names=["İpek"])
    for text in ("İpek, merhaba", "ipek, merhaba", "IPEK, merhaba"):
        assert spoken(c, anchor, text, cfg), text


@pytest.mark.parametrize("name,text", [("Story", "History is boring"), ("King", "Hiking was fun today"),
                                       ("Api", "Okapi are odd animals"), ("Ring", "Hiring is hard, ok?")])
def test_a_greeting_glued_to_a_name_is_one_word_in_scripts_with_spaces(c, anchor, name, text):
    cfg = guild_a(anchor, wake_names=[name], greetings=["hi", "ok", "yo"])
    assert not spoken(c, anchor, text, cfg)
    assert spoken(c, anchor, f"hi {name}, quick question", cfg)


def test_words_that_merely_contain_the_name_do_not_use_up_the_occurrence_budget(c, anchor):
    """Only occurrences that stand as a word of their own count against the per-message bound."""
    cfg = guild_a(anchor, wake_names=["Max"])
    filler = ("Climax maximal " * 80).strip()                 # 160 occurrences, none of them a word
    assert spoken(c, anchor, filler + ". Max, hilf mir", cfg)
    assert not spoken(c, anchor, filler, cfg)
    many = " ".join(["Max ist da"] * 80)                       # 80 word-bounded occurrences in third person
    assert not spoken(c, anchor, many + ". Max, hilf mir", cfg)  # beyond the bound: not judged


def test_greetings_are_data_of_the_server(c, anchor):
    """The words around a name that depend on sentence structure come from the server's configuration."""
    assert not spoken(c, anchor, "yo Nova what's up")
    assert spoken(c, anchor, "yo Nova what's up", guild_a(anchor, greetings=["yo"]))
    assert not spoken(c, anchor, "Nova kannst du helfen")
    assert spoken(c, anchor, "Nova kannst du helfen", guild_a(anchor, greetings=["kannst"]))
    assert not spoken(c, anchor, "hey Nova", guild_a(anchor, greetings=[]))
    assert spoken(c, anchor, "Nova, hi", guild_a(anchor, greetings=[]))


def test_default_greetings_apply_when_the_configuration_has_none(c, anchor):
    cfg = guild_a(anchor)
    del cfg["identity"]["greetings"]
    assert spoken(c, anchor, "hey Nova help", cfg)


def test_entries_typed_with_at_or_punctuation_mean_the_word(c, anchor):
    cfg = guild_a(anchor, wake_names=["@Nova", "Orbit!", "?!"], greetings=["hey,", "¡hola!"])
    assert spoken(c, anchor, "Nova! hi", cfg) and spoken(c, anchor, "@nova help", cfg)
    assert spoken(c, anchor, "Orbit, hi", cfg) and spoken(c, anchor, "hola Orbit can you", cfg)
    assert spoken(c, anchor, "hey Nova look", cfg)
    assert not spoken(c, anchor, "?! what", cfg) and not spoken(c, anchor, "ask Nova later", cfg)
    assert c.fold_entry("Ni.ko") == "ni.ko" and c.fold_entry("[Nova]") == "nova"


def test_names_and_greetings_are_literal_text_not_patterns(c, anchor):
    cfg = guild_a(anchor, wake_names=["Ni.ko", "N[ov]a", "x(y|z)+w"], greetings=["h.y"])
    assert spoken(c, anchor, "Ni.ko, hi", cfg)
    assert not spoken(c, anchor, "Nisko, hi", cfg)       # "." is a dot, not any character
    assert not spoken(c, anchor, "Noa, what?", cfg)      # "[ov]" is not a character class
    assert spoken(c, anchor, "N[ov]a, what?", cfg)
    assert not spoken(c, anchor, "xyyw, hi", cfg)        # "(y|z)+" is not a group
    assert spoken(c, anchor, "x(yz)+w, hi", cfg)         # "|" is markup ("||spoiler||"), dropped on both sides
    assert not spoken(c, anchor, "hay Ni.ko", cfg)
    assert spoken(c, anchor, "h.y Ni.ko", cfg)


@pytest.mark.parametrize("names", [[""], ["a"], ["ab"], ["  "], ["*_*"], ["明"], ["🦊"]])
def test_empty_or_very_short_wake_names_never_match_ordinary_messages(c, anchor, names):
    cfg = guild_a(anchor, wake_names=names, bot_name="")
    for text in ("a, b", "ab, what", "hello", "", "  ", "明, 来", "🦊, hi", "* hi"):
        assert not spoken(c, anchor, text, cfg), (names, text)


def test_two_character_ideographic_names_are_long_enough(c, anchor):
    cfg = guild_a(anchor, wake_names=["小明"])
    assert spoken(c, anchor, "小明，帮我看看", cfg)
    assert not spoken(c, anchor, "小明是谁", cfg)


def test_empty_greetings_and_short_greetings_are_ignored(c, anchor):
    cfg = guild_a(anchor, greetings=["", "a", " "])
    assert not spoken(c, anchor, "a Nova is a star", cfg)
    assert not spoken(c, anchor, " Nova is here", cfg)


def test_wake_names_fall_back_to_the_bot_name_then_the_display_name(c, anchor):
    cfg = guild_a(anchor, wake_names=[], bot_name="Orbit")
    assert spoken(c, anchor, "Orbit, hello", cfg)
    assert not spoken(c, anchor, "Nova, hello", cfg)
    cfg = guild_a(anchor, wake_names=[], bot_name="")
    adapter = Adapter(bot_name="Comet")
    assert c.classify(adapter, msg("Comet, hello"), cfg).trigger == "wake_name"
    nick = msg("Lumen, hello", me_name="Lumen")
    assert c.classify(adapter, nick, cfg).trigger == "wake_name"


def test_compiled_patterns_are_cached_per_revision_and_follow_changes(c, anchor):
    cfg = guild_a(anchor)
    first = c.compile_identity(cfg)
    assert c.compile_identity(copy.deepcopy(cfg)) is first
    changed = guild_a(anchor, wake_names=["Orbit"])
    changed["rev"] = cfg["rev"]               # same rev, other content: never the stale patterns
    assert c.compile_identity(changed) is not first
    assert spoken(c, anchor, "Orbit, hi", changed) and not spoken(c, anchor, "Nova, hi", changed)
    bumped = copy.deepcopy(cfg)
    bumped["rev"] += 1
    assert c.compile_identity(bumped) is not first


def test_malformed_identity_is_tolerated(c, anchor):
    for identity in (None, "Nova", {"wake_names": "Nova"}, {"wake_names": [None, 3, "Nova"]},
                     {"greetings": None, "wake_names": ["Nova"]}):
        cfg = guild_a(anchor)
        cfg["identity"] = identity
        c.classify(Adapter(), msg("Nova, hi"), cfg)  # never raises
    assert c.classify(Adapter(), msg("Nova, hi"), None).trigger == ""


# ── the interaction matrix (generalised from the 2.0.6 tests) ─────────────────────────────────────


def bot_user():
    return SimpleNamespace(id=int(BOT), bot=True)


def test_interaction_matrix(c, anchor):
    cfg = guild_a(anchor)
    adapter = Adapter()
    own = SimpleNamespace(message_id=43, resolved=SimpleNamespace(id=43, author=bot_user(), content="bot said"))
    other = SimpleNamespace(message_id=42, resolved=SimpleNamespace(
        id=42, author=SimpleNamespace(id=int(OTHER), bot=False), content="human said"))
    unresolved = SimpleNamespace(message_id=99, resolved=None)

    def sig(m):
        s = c.classify(adapter, m, cfg)
        return s.direct, s.trigger, s.mode

    assert sig(msg("just chatting")) == ("none", "", "none")
    assert sig(msg("just chatting", channel=AMBIENT_A)) == ("none", "", "ambient")
    assert sig(msg("hi", mentions=[bot_user()])) == ("direct", "mention", "direct")
    assert sig(msg("thanks", reference=own)) == ("direct", "reply", "direct")
    assert sig(msg("hm", reference=other)) == ("none", "", "none")
    assert sig(msg("hm", reference=other, channel=AMBIENT_A)) == ("none", "", "ambient")
    assert sig(msg("Nova, and you?", reference=other)) == ("direct", "wake_name", "direct")
    assert sig(msg("hm", reference=unresolved)) == ("maybe_reply", "reply", "maybe_reply")
    assert sig(msg("hm", reference=unresolved, channel=AMBIENT_A)) == ("maybe_reply", "reply", "ambient")
    assert sig(msg("Nova, hm", reference=unresolved)) == ("direct", "wake_name", "direct")
    assert sig(msg("Nova, in the ambient channel", channel=AMBIENT_A)) == ("direct", "wake_name", "direct")
    # what a route carries (prompts.assemble knows worklog, direct, ambient) and how admission treats it
    def route(m):
        s = c.classify(adapter, m, cfg)
        return s.route_mode, s.reply_unverified, s.addressed

    assert route(msg("just chatting")) == ("direct", False, False)              # stock decides
    assert route(msg("just chatting", channel=AMBIENT_A)) == ("ambient", False, True)
    assert route(msg("hi", mentions=[bot_user()])) == ("direct", False, True)
    assert route(msg("hm", reference=unresolved)) == ("direct", True, True)     # the handler verifies
    assert route(msg("hm", reference=unresolved, channel=AMBIENT_A)) == ("ambient", False, True)
    assert route(msg("Nova, in the ambient channel", channel=AMBIENT_A)) == ("direct", False, True)
    # the backwards-compatible seam of the security core
    assert c.direct_signal(adapter, msg("Nova, hi")) == "none"
    assert c.direct_signal(adapter, msg("Nova, hi"), cfg) == "direct"
    assert c.direct_signal(adapter, msg("hi", mentions=[bot_user()])) == "direct"


def test_every_signal_maps_to_a_mode_the_prompt_engine_knows(c, anchor):
    """An unknown route mode blocks public turns in prompts.assemble: route_mode never yields one."""
    import itertools

    modes = anchor.mod("prompts.assemble").MODES
    for direct, ambient, thread in itertools.product((c.DIRECT, c.MAYBE_REPLY, c.NONE), (False, True),
                                                     (False, True)):
        signal = c.Signal(direct=direct, ambient=ambient, worklog_area=thread, worklog_thread=thread)
        assert signal.route_mode in modes, signal


def test_threads_inherit_ambient_and_excluded_from_their_parent(c, anchor):
    cfg = guild_a(anchor)
    in_ambient = msg("chat", channel="920000000000000150", channel_type=11, parent_id=AMBIENT_A)
    in_excluded = msg("chat", channel="920000000000000151", channel_type=11, parent_id=EXCLUDED_A)
    s = c.classify(Adapter(), in_ambient, cfg)
    assert s.ambient and not s.excluded and s.mode == "ambient"
    s = c.classify(Adapter(), in_excluded, cfg)
    assert s.excluded and not s.ambient


def test_ambient_only_in_configured_channels_and_never_in_excluded_ones(c, anchor):
    cfg = guild_a(anchor)
    assert not c.classify(Adapter(), msg("chat", channel=GENERAL_A), cfg).ambient
    assert c.classify(Adapter(), msg("chat", channel=AMBIENT_A), cfg).ambient
    both = guild_a(anchor)
    both["conversation"]["excluded_channels"].append(AMBIENT_A)
    s = c.classify(Adapter(), msg("chat", channel=AMBIENT_A), both)
    assert s.excluded and not s.ambient
    assert not c.classify(Adapter(), msg("chat", channel=AMBIENT_A), guild_a(anchor) | {"conversation": {}}).ambient
    # the same channel id is not ambient in another server's configuration
    assert not c.classify(Adapter(), msg("chat", guild=GUILD_B, channel=AMBIENT_A), guild_b(anchor)).ambient


def test_worklog_threads_run_in_worklog_mode(c, anchor):
    cfg = guild_a(anchor)
    thread = msg("continue", channel="920000000000000160", channel_type=11, parent_id=WORKLOG_A)
    s = c.classify(Adapter(), thread, cfg)
    assert s.worklog_thread and s.worklog_area and s.mode == s.route_mode == "worklog"
    in_channel = c.classify(Adapter(), msg("note", channel=WORKLOG_A), cfg)
    assert in_channel.worklog_area and not in_channel.worklog_thread and in_channel.mode != "worklog"
    assert in_channel.route_mode != "worklog"
    other_parent = msg("x", channel="920000000000000161", channel_type=11, parent_id=GENERAL_A)
    assert not c.classify(Adapter(), other_parent, cfg).worklog_area
    no_worklog = guild_a(anchor)
    no_worklog["worklog"]["channel"] = None
    assert not c.classify(Adapter(), thread, no_worklog).worklog_area


def test_threads_this_process_opened_as_worklogs_count_as_worklog_area(c, anchor):
    worklog = anchor.mod("delivery.worklog")
    worklog.DELIVERY.threads.put("920000000000000170", (BOT, GUILD_A))
    cfg = guild_a(anchor)
    cfg["worklog"]["channel"] = None
    s = c.classify(Adapter(), msg("x", channel="920000000000000170", channel_type=11,
                                  parent_id="920000000000000171"), cfg)
    assert s.worklog_area
    other_guild = c.classify(Adapter(), msg("x", guild=GUILD_B, channel="920000000000000170", channel_type=11,
                                            parent_id="920000000000000171"), guild_b(anchor))
    assert not other_guild.worklog_area


@pytest.mark.parametrize("kind", ["bot", "webhook", "system"])
def test_bots_webhooks_and_system_messages_never_wake(c, anchor, kind):
    m = msg("Nova, run this", mentions=[bot_user()], bot=kind == "bot",
            webhook_id=920000000000000555 if kind == "webhook" else None)
    if kind == "system":
        m.author.system = True
    s = c.classify(Adapter(), m, guild_a(anchor))
    assert not s.human and s.direct == "none" and s.trigger == ""
    assert c.direct_signal(Adapter(), m, guild_a(anchor)) == "none"
    assert c.is_human(msg("x"))


def test_reaction_wakeup_marker_counts_only_for_its_own_reactor_and_message(c, anchor):
    wake = c.ReactionWakeup(bot_id=BOT, guild_id=GUILD_A, channel_id=GENERAL_A, message_id="4242",
                            reactor_id=MEMBER, emoji="custom:920000000000000900")
    good = msg("[wake]")
    good._dsp_reaction_wakeup = wake
    assert c.classify(Adapter(), good, guild_a(anchor)).trigger == "reaction"
    forged = msg("[wake]", author=OTHER)
    forged._dsp_reaction_wakeup = wake
    assert c.classify(Adapter(), forged, guild_a(anchor)).trigger == ""
    moved = msg("[wake]", mid=4343)
    moved._dsp_reaction_wakeup = wake
    assert c.classify(Adapter(), moved, guild_a(anchor)).trigger == ""
    not_a_marker = msg("[wake]")
    not_a_marker._dsp_reaction_wakeup = {"reactor_id": MEMBER, "message_id": "4242"}
    assert c.classify(Adapter(), not_a_marker, guild_a(anchor)).trigger == ""


def test_classification_never_rewrites_the_message(c, anchor):
    m = msg("Nova /restart")
    before = dict(vars(m))
    c.classify(Adapter(), m, guild_a(anchor))
    assert vars(m) == before and m.content == "Nova /restart"


# ── end to end through the security core: a wake name cannot smuggle in a gateway command ─────────


async def test_wake_name_followed_by_a_command_is_conversation_not_a_command(anchor, world):
    """The security-core world of S3: admission classifies with the guild's configuration
    (``classify.classify(adapter, message, guild_cfg)``), so the guild's wake names address the bot."""
    from fakes.gateway import MEMBER as WORLD_MEMBER, message, user

    world.configure(identity__wake_names=["Nova"])
    for mid, text in enumerate(("Nova, /restart", "hey Nova /restart", "@Nova /restart", "Nova: /yolo"), 7101):
        turn = await world.deliver(message(user(WORLD_MEMBER), content=text, mid=mid))
        assert isinstance(turn, dict), text               # addressed by name: a public turn ran ...
        assert turn["event"].get_command() is None, text  # ... as conversation, not as a gateway command
        assert turn["event"].text == text
        assert "BLOCKED" in turn["tools"][0]               # and it has no tool authority
        route = anchor.RUNTIME.message_routes.get((str(turn["event"].source.chat_id), str(mid), WORLD_MEMBER))
        assert route.lane == "public" and route.mode == "direct"
    # the name glued to a command is not addressed at all: stock, nothing runs
    assert await world.deliver(message(user(WORLD_MEMBER), content="Nova /restart", mid=7110)) is None
    # a public member's bare command is refused before Hermes would dispatch it (security core)
    assert await world.deliver(message(user(WORLD_MEMBER), content="/restart", mid=7102,
                                       mentions=[user("900000000000000300", bot=True)])) is None
    # unaddressed text in a direct-only channel stays stock (not admitted)
    assert await world.deliver(message(user(WORLD_MEMBER), content="ask Nova later", mid=7103)) is None

