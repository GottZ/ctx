"""S5 contract: the conversation layer against the real Hermes Discord adapter of each ref.

K5: Hermes' own ``_wire_plugin_handlers`` calls the platform-handler factories with a real
``discord.ext.commands.Bot``; the reaction listener goes onto that client, a real
``RawReactionActionEvent`` dispatched by discord.py wakes the bot, and the woken message passes the
security core's gate and the real ``DiscordAdapter._handle_message`` (Hermes builds the MessageEvent:
reply anchor and referenced text from the original message, the reactor as the author).

K7: the permission semantics hydration relies on, computed by discord.py itself on real Guild, Role,
Member, TextChannel and Thread objects (private channel, role overwrites, private threads).

Wake names: through the real admission and handler, ``<wake name> /restart`` from a member is text,
never a gateway command.
"""

from __future__ import annotations

import asyncio
import sys
from types import SimpleNamespace

import pytest
from fakes.discord_world import (BOT, CHANNEL, EMOJI, GUILD, LOUNGE_CHANNEL, LOUNGE_MODS_CHANNEL, MEMBER, MODS_CHANNEL,
                                 MODS_ROLE, OPERATOR, ORIGINAL, OTHER_CHANNEL, OTHER_GUILD, PRIVATE_THREAD, DiscordWorld,
                                 _channel_payload, _member_payload)
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, load_in


@pytest.fixture
async def setup(hermes, tmp_path, monkeypatch):
    _evict(ANCHOR)
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "root"))
    homes = Homes(tmp_path)
    (homes.root / "config.yaml").write_text(
        f"plugins:\n  enabled: [{PLUGIN}]\n  entries:\n    {PLUGIN}:\n      settings:\n"
        f"        operators: [\"{OPERATOR}\"]\n", encoding="utf-8")
    import discord
    from discord.ext import commands
    from gateway.config import Platform
    from gateway.platforms.helpers import MessageDeduplicator
    from gateway.run import GatewayRunner
    from plugins.platforms.discord.adapter import DiscordAdapter

    saved = {(cls, name): cls.__dict__.get(name) for cls in (GatewayRunner, DiscordAdapter)
             for name in ("_handle_message", "_is_user_authorized", "_discord_message_admission")}
    manager = load_in(homes.root)
    bot = commands.Bot(command_prefix="!", intents=discord.Intents.default(), application_id=int(BOT))
    await bot.__aenter__()                      # binds the running loop; no login, no gateway
    model = DiscordWorld(bot)
    runner = GatewayRunner.__new__(GatewayRunner)
    runner.config = SimpleNamespace(multiplex_profiles=False, profile_routes=[], group_sessions_per_user=True,
                                    thread_sessions_per_user=False)
    runner._resolve_profile_home_for_source = lambda source: homes.root
    runner._profile_name_for_source = lambda source, adapter_profile=None: None
    adapter = DiscordAdapter.__new__(DiscordAdapter)
    adapter.platform = Platform.DISCORD
    adapter.config = None
    adapter._owner_profile = None
    adapter.gateway_runner = runner
    adapter._dedup = MessageDeduplicator()
    adapter._client = bot
    adapter._allowed_user_ids = {"950000000000000999"}
    adapter._allowed_role_ids = set()
    stubs = dict(
        _is_pairing_approved_user=lambda _uid: False, _get_parent_channel_id=lambda _c: None,
        _warn_if_fail_closed_default=lambda: None, _get_allowed_channels=lambda: set(),
        _get_ignored_channels=lambda: set(), _discord_free_response_channels=lambda: set(),
        _discord_require_mention=lambda: True, _voice_text_channels={}, _in_bot_thread=lambda m: False,
        _get_no_thread_channels=lambda: set(), _extra_or_env_flag=lambda *a, **k: True,
        _discord_free_response_auto_thread=lambda: False, _get_effective_topic=lambda ch, is_thread=False: None,
        _discord_history_backfill=lambda: False, _resolve_channel_skills=lambda *a: None,
        _resolve_channel_prompt=lambda *a: None, _text_batch_delay_seconds=0,
        _threads=SimpleNamespace(mark_async=lambda _t: asyncio.sleep(0)))
    for name, value in stubs.items():
        setattr(adapter, name, value)

    async def media(_attachments):
        return [], [], [], None

    adapter._collect_attachment_media = media
    captured = []

    async def capture(event):
        captured.append(event)

    adapter.handle_message = capture
    yield SimpleNamespace(adapter=adapter, runner=runner, bot=bot, model=model, captured=captured, homes=homes,
                          manager=manager, hermes=hermes, discord=discord)
    if f"{ANCHOR}.snapshot" in sys.modules:
        sys.modules[f"{ANCHOR}.snapshot"].detach(BOT)      # the P2 directory heartbeat of the bound bot
    await bot.__aexit__(None, None, None)
    for (cls, name), value in saved.items():
        if value is None:
            if name in cls.__dict__:
                delattr(cls, name)
        else:
            setattr(cls, name, value)
    _evict(ANCHOR)


def _mod(name):
    return sys.modules[f"{ANCHOR}.{name}"]


def _wire(s):
    """Hermes' own wiring: ``_wire_plugin_handlers(native)`` calls every factory with ``(native, adapter)``.
    The policy's factory binds the bot and runs its bind steps (K5 reaction listeners, K7 probe among them)."""
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    factories = [(_mod("adapt.interventions").discord_platform_handler, PLUGIN)]
    s.manager.get_platform_handler_factories = lambda platform: list(factories) if platform == "discord" else []
    token = set_hermes_home_override(str(s.homes.root))
    try:
        s.adapter._wire_plugin_handlers(s.bot)
    finally:
        reset_hermes_home_override(token)


def _configure(**identity):
    runtime = _mod("runtime").RUNTIME
    ops = _mod("store.ops")
    store = runtime.bots.for_bot(BOT).store
    ops.bind(store, BOT, GUILD, by="cli")
    guild = ops.export(store.read(), BOT, GUILD)["bots"][BOT]["guilds"][GUILD]
    guild["identity"].update(identity)
    ops.import_guild(store, BOT, GUILD, guild, by="cli", prompt_cap=6000)
    return store


async def _settle(timeout=2.0):
    current = asyncio.current_task()
    pending = [t for t in asyncio.all_tasks() if t is not current and not t.done()]
    if pending:
        await asyncio.wait(pending, timeout=timeout)


# ── K5: reaction wakeup through the real factory, discord.py dispatch and Hermes handler ─────────


async def test_k5_reaction_wakeup_through_real_hermes(setup):
    s = setup
    _wire(s)
    _configure(reaction_emoji={"id": EMOJI, "name": "nova"})
    results = [_mod("runtime").RUNTIME.status.get("K5")]            # recorded by the policy's own bind step
    assert results[0] is not None and results[0].outcome.value == "injected", results
    assert any(getattr(f, "__name__", "") == "on_raw_reaction_add" for f in s.bot.extra_events["on_raw_reaction_add"])
    original = s.model.post(s.model.general, s.model.operator, "operator wrote this", ORIGINAL)
    rows = []
    for reactor, lane in ((s.model.member, "public"), (s.model.operator, "operator")):
        event = s.discord.RawReactionActionEvent(
            {"message_id": str(ORIGINAL), "channel_id": CHANNEL, "user_id": str(reactor.id), "guild_id": GUILD,
             "type": 0, "burst": False},
            s.discord.PartialEmoji(name="nova", id=int(EMOJI)), "REACTION_ADD")
        event.member = reactor
        before = len(s.captured)
        s.bot.dispatch("raw_reaction_add", event)
        await _settle()
        assert len(s.captured) == before + 1, f"no MessageEvent for {lane}"
        ev = s.captured[-1]
        route = ev.raw_message._dsp_route
        rows.append({"lane": route.lane, "author": route.author_id, "text": ev.text,
                     "reply_to": ev.reply_to_message_id, "command": ev.get_command()})
        assert route.lane == lane and route.author_id == str(reactor.id)
        assert ev.source.user_id == str(reactor.id)
        assert ev.reply_to_message_id == str(ORIGINAL) and ev.reply_to_text == original.content
        assert ev.text.startswith("[REACTION WAKEUP]") and f"<@{BOT}>" not in ev.text
        assert ev.get_command() is None
    # a second reaction of the same member on the same message is not a second wakeup
    again = s.discord.RawReactionActionEvent(
        {"message_id": str(ORIGINAL), "channel_id": CHANNEL, "user_id": MEMBER, "guild_id": GUILD, "type": 0},
        s.discord.PartialEmoji(name="nova", id=int(EMOJI)), "REACTION_ADD")
    again.member = s.model.member
    s.bot.dispatch("raw_reaction_add", again)
    await _settle()
    assert len(s.captured) == 2
    report = s.hermes.write_report("ingress-k5", {"K5": results[0].as_dict(), "turns": rows})
    print(f"\n== ingress K5 {s.hermes.ref}: {results[0].outcome.value} ({results[0].reason}) -> {report}")


async def test_k5_bot_reactions_never_reach_hermes(setup):
    s = setup
    _wire(s)
    _configure(reaction_emoji={"id": EMOJI, "name": "nova"})
    s.model.post(s.model.general, s.model.operator, "x", ORIGINAL)
    import discord

    other_bot = discord.Member(data={**_member_payload("950000000000000502"),
                                     "user": {**_member_payload("950000000000000502")["user"], "bot": True}},
                               guild=s.model.guild, state=s.bot._connection)
    event = discord.RawReactionActionEvent(
        {"message_id": str(ORIGINAL), "channel_id": CHANNEL, "user_id": str(other_bot.id), "guild_id": GUILD,
         "type": 0}, discord.PartialEmoji(name="nova", id=int(EMOJI)), "REACTION_ADD")
    event.member = other_bot
    s.bot.dispatch("raw_reaction_add", event)
    await _settle()
    assert s.captured == []


async def test_k5_operator_wakeup_never_becomes_a_gateway_command(setup):
    """A server's managers choose the bot name; an operator's override of the wakeup text may name it. The
    operator's wakeup through real Hermes stays text, without the managers' name (guidance is off for
    operator turns), even when the name or the bot's nickname would start a command."""
    s = setup
    _wire(s)
    store = _configure(reaction_emoji={"id": EMOJI, "name": "nova"}, bot_name="/yolo")
    ops = _mod("store.ops")
    guild = ops.export(store.read(), BOT, GUILD)["bots"][BOT]["guilds"][GUILD]
    guild["prompts"]["template_overrides"] = {"reaction_wakeup": "{bot_name} please answer the referenced message."}
    ops.import_guild(store, BOT, GUILD, guild, by="cli", prompt_cap=6000)
    s.model.post(s.model.general, s.model.member, "member wrote this", ORIGINAL)
    rows = []
    for nickname in (None, "/yolo"):
        if nickname:                                     # the bot's member in the server, with a nickname
            me = s.discord.Member(data={**_member_payload(BOT), "nick": nickname}, guild=s.model.guild,
                                  state=s.bot._connection)
            s.model.guild._add_member(me)
            assert s.model.guild.me.display_name == nickname
        event = s.discord.RawReactionActionEvent(
            {"message_id": str(ORIGINAL), "channel_id": CHANNEL, "user_id": OPERATOR, "guild_id": GUILD, "type": 0,
             "burst": False}, s.discord.PartialEmoji(name="nova", id=int(EMOJI)), "REACTION_ADD")
        event.member = s.model.operator
        _mod("ingress.reactions").SEEN.clear()
        s.bot.dispatch("raw_reaction_add", event)
        await _settle()
        ev = s.captured[-1]
        rows.append((ev.raw_message._dsp_route.lane, ev.text, ev.get_command()))
    assert all(lane == "operator" and command is None for lane, _, command in rows), rows
    assert not rows[0][1].startswith("/") and "please answer the referenced message" in rows[0][1]
    assert rows[1][1].startswith("[REACTION WAKEUP]"), rows[1]


# ── wake names through the real admission and handler ────────────────────────────────────────────


async def test_wake_name_command_is_text_through_real_hermes(setup):
    s = setup
    _wire(s)
    _configure(wake_names=["Nova"])                  # admission classifies with the guild's wake names
    msg = SimpleNamespace(
        id=950000000000009101, author=s.model.member, guild=s.model.guild, channel=s.model.general,
        type=s.discord.MessageType.default, mentions=[], content="Nova, /restart", reference=None,
        attachments=[], message_snapshots=[], created_at=None, webhook_id=None)
    admitted, role = s.adapter._discord_message_admission(msg, claim=True)
    assert admitted
    await s.adapter._handle_message(msg, role_authorized=role)
    event = s.captured[-1]
    assert event.text == "Nova, /restart" and event.get_command() is None
    assert event.raw_message._dsp_route.lane == "public"
    unaddressed = SimpleNamespace(**{**vars(msg), "id": 950000000000009102, "content": "ask Nova later"})
    admitted, _ = s.adapter._discord_message_admission(unaddressed, claim=True)
    assert not admitted                          # neither addressed nor ambient: stock, member not allowed


# ── K7: discord.py's permission semantics on real model objects ──────────────────────────────────


async def test_k7_hydration_with_real_discord_permissions(setup):
    s = setup
    _wire(s)
    m = s.model
    hydrate = _mod("ingress.hydrate")
    routes = _mod("policy.routes")
    cfg = _mod("store.schema").guild_defaults()
    cfg["state"] = "active"
    secret = m.post(m.mods, m.mod, "mods only", 950000000000009201)
    thread_note = m.post(m.private_thread, m.mod, "private thread", 950000000000009202)
    link = f"https://discord.com/channels/{GUILD}/{MODS_CHANNEL}/{secret.id}"
    thread_link = f"https://discord.com/channels/{GUILD}/{PRIVATE_THREAD}/{thread_note.id}"

    # the facts discord.py itself computes
    assert not m.mods.permissions_for(m.member).view_channel
    assert m.mods.permissions_for(m.mod).view_channel
    assert m.mods.permissions_for(m.guild.get_role(int(MODS_ROLE))).view_channel
    assert not m.mods.permissions_for(m.guild.default_role).view_channel
    assert m.private_thread.permissions_for(m.member).view_channel   # membership is not part of it
    assert m.elsewhere.permissions_for(m.member).view_channel        # a member object of ANOTHER server
    foreign = m.post(m.elsewhere, m.member, "other server", 950000000000009203)

    async def run(author, channel, content, lane):
        message = m.post(channel, author, content, 950000000000009300 + len(channel.messages))
        route = routes.Route(bot_id=BOT, guild_id=GUILD, channel_id=str(channel.id), message_id=str(message.id),
                             author_id=str(author.id), lane=lane)
        return await hydrate.hydrate(s.adapter, message, route, cfg)

    rows = {}
    rows["member_private_channel"] = (await run(m.member, m.general, link, "public")).text
    rows["mod_into_wider_audience"] = (await run(m.mod, m.general, link, "public")).text
    rows["mod_same_channel"] = (await run(m.mod, m.mods, link, "public")).text
    rows["operator_lane"] = (await run(m.operator, m.general, link, "operator")).text
    rows["private_thread"] = (await run(m.member, m.general, thread_link, "public")).text
    general_note = m.post(m.general, m.member, "general talk", 950000000000009204)
    general_link = f"https://discord.com/channels/{GUILD}/{CHANNEL}/{general_note.id}"
    assert any(isinstance(k, s.discord.Object) for k in m.club_open.overwrites)
    rows["club_without_uncached_reader"] = (await run(m.mod, m.club, general_link, "public")).text
    rows["club_with_uncached_reader"] = (await run(m.mod, m.club_open, general_link, "public")).text
    # role combinations: discord.py sums up role overwrites (all denies, then all allows)
    mods_role = m.guild.get_role(int(MODS_ROLE))
    assert m.mods.permissions_for(m.both).view_channel
    assert m.lounge.permissions_for(mods_role).view_channel            # each role on its own reads #lounge
    assert not m.lounge.permissions_for(m.both).view_channel           # Mods + Restricted does not
    assert m.lounge_mods.permissions_for(m.both).view_channel          # an explicit allow outweighs the deny
    lounge_note = m.post(m.lounge, m.member, "lounge talk", 950000000000009205)
    lounge_mods_note = m.post(m.lounge_mods, m.mod, "lounge talk for mods", 950000000000009206)
    rows["role_combination_denied"] = (await run(m.mod, m.mods, f"https://discord.com/channels/{GUILD}/"
                                                 f"{LOUNGE_CHANNEL}/{lounge_note.id}", "public")).text
    rows["role_combination_allowed"] = (await run(m.mod, m.mods, f"https://discord.com/channels/{GUILD}/"
                                                  f"{LOUNGE_MODS_CHANNEL}/{lounge_mods_note.id}", "public")).text
    for claimed in (GUILD, OTHER_GUILD):
        url = f"https://discord.com/channels/{claimed}/{OTHER_CHANNEL}/{foreign.id}"
        rows[f"other_server_{claimed[-3:]}"] = (await run(m.operator, m.general, url, "operator")).text
    assert rows["member_private_channel"] == "" and rows["mod_into_wider_audience"] == ""
    assert "mods only" in rows["mod_same_channel"] and "mods only" in rows["operator_lane"]
    assert rows["private_thread"] == "" and m.private_thread.fetched == []
    assert rows["other_server_100"] == rows["other_server_110"] == "" and m.elsewhere.fetched == []
    assert "general talk" in rows["club_without_uncached_reader"]
    assert rows["club_with_uncached_reader"] == ""
    assert rows["role_combination_denied"] == "" and m.lounge.fetched == []
    assert "lounge talk for mods" in rows["role_combination_allowed"]
    # #quiet: @everyone views but may not read the history, Mods may; the answer is seen by every viewer
    history = s.discord.Permissions(read_message_history=True).value
    quiet = m.text_class(state=s.bot._connection, guild=m.guild, data=_channel_payload(
        "950000000000000207", "quiet", [(GUILD, 0, history), (MODS_ROLE, history, 0)]))
    m.guild._add_channel(quiet)
    assert quiet.permissions_for(m.member).view_channel and not quiet.permissions_for(m.member).read_message_history
    assert quiet.permissions_for(m.mod).read_message_history
    rows["view_only_audience"] = (await run(m.mod, quiet, link, "public")).text
    assert rows["view_only_audience"] == ""
    # an author discord.py cannot judge (a User without server roles) reads nothing
    user = s.discord.User(state=s.bot._connection, data=_member_payload("950000000000000504")["user"])
    with pytest.raises(AttributeError):
        m.general.permissions_for(user)
    rows["author_without_member"] = (await run(user, m.general, general_link, "public")).text
    assert rows["author_without_member"] == ""
    # a channel Discord refuses (discord.NotFound carries the HTTP status) is asked for once per bot
    calls = []

    async def refused(cid):
        calls.append(int(cid))
        raise s.discord.NotFound(SimpleNamespace(status=404, reason="Not Found"),
                                 {"message": "Unknown Channel", "code": 10003})

    s.bot.fetch_channel = refused
    gone = f"https://discord.com/channels/{GUILD}/950000000000000299/950000000000009207"
    rows["refused_channel"] = (await run(m.member, m.general, gone, "public")).text
    rows["refused_channel_again"] = (await run(m.member, m.general, gone, "public")).text
    assert rows["refused_channel"] == rows["refused_channel_again"] == "" and calls == [950000000000000299]
    probe = hydrate.probe(s.adapter, _mod("runtime").RUNTIME.status)
    assert probe.outcome.value == "ok", probe.reason
    report = s.hermes.write_report("ingress-k7", {"K7": probe.as_dict(),
                                                  "cases": {k: bool(v) for k, v in rows.items()}})
    print(f"\n== ingress K7 {s.hermes.ref}: {probe.outcome.value} ({probe.reason}) -> {report}")
