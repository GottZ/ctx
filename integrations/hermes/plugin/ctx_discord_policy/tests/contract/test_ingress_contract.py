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
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, load_in

GUILD, CHANNEL, MODS_CHANNEL, PRIVATE_THREAD = ("950000000000000100", "950000000000000200",
                                                "950000000000000201", "950000000000000202")
OTHER_GUILD, OTHER_CHANNEL = "950000000000000110", "950000000000000210"
CLUB_CHANNEL, CLUB_OPEN_CHANNEL, UNCACHED = "950000000000000203", "950000000000000204", "950000000000000777"
LOUNGE_CHANNEL, LOUNGE_MODS_CHANNEL, RESTRICTED_ROLE, BOTH = ("950000000000000205", "950000000000000206",
                                                             "950000000000000601", "950000000000000503")
BOT, OPERATOR, MEMBER, MOD, MODS_ROLE, EMOJI = ("950000000000000300", "950000000000000400", "950000000000000500",
                                               "950000000000000501", "950000000000000600", "950000000000000700")
ORIGINAL = 950000000000009000

READ = dict(view_channel=True, read_message_history=True, send_messages=True, send_messages_in_threads=True)


def _perm(**flags):
    import discord
    return discord.Permissions(**flags).value


def _channel_payload(cid, name, overwrites=()):
    return {"id": cid, "type": 0, "guild_id": GUILD, "name": name, "position": 0, "parent_id": None,
            "topic": None, "nsfw": False, "rate_limit_per_user": 0, "last_message_id": None, "flags": 0,
            "permission_overwrites": [{"id": entry[0], "type": entry[3] if len(entry) > 3 else 0,
                                       "allow": str(entry[1]), "deny": str(entry[2])} for entry in overwrites]}


def _member_payload(uid, roles=()):
    return {"user": {"id": uid, "username": f"user{uid[-3:]}", "global_name": None, "discriminator": "0",
                     "avatar": None, "public_flags": 0},
            "roles": list(roles), "joined_at": "2026-01-01T00:00:00+00:00", "premium_since": None, "nick": None,
            "avatar": None, "banner": None, "pending": False, "flags": 0, "deaf": False, "mute": False,
            "communication_disabled_until": None}


class DiscordWorld:
    """Real discord.py model objects in a real client's cache (no network)."""

    def __init__(self, client):
        import discord

        self.discord = discord
        state = client._connection
        state.user = discord.ClientUser(state=state, data={"id": BOT, "username": "contract-bot", "bot": True,
                                                           "global_name": None, "discriminator": "0",
                                                           "avatar": None, "public_flags": 0})
        state._users[state.user.id] = state.user
        role = lambda rid, name, perms, pos: {  # noqa: E731
            "id": rid, "name": name, "color": 0, "hoist": False, "icon": None, "unicode_emoji": None,
            "position": pos, "permissions": str(perms), "managed": False, "mentionable": False, "flags": 0,
            "colors": {"primary_color": 0, "secondary_color": None, "tertiary_color": None}}
        self.guild = discord.Guild(data={
            "id": GUILD, "name": "contract-guild", "owner_id": BOT, "emojis": [], "stickers": [], "features": [],
            "channels": [], "member_count": 4, "preferred_locale": "en-US",
            "roles": [role(GUILD, "@everyone", _perm(**READ), 0), role(MODS_ROLE, "Mods", _perm(), 1),
                      role(RESTRICTED_ROLE, "Restricted", _perm(), 2)]},
            state=state)
        state._add_guild(self.guild)
        world = self

        class Text(discord.TextChannel):
            """A real TextChannel whose message reads come from memory instead of the REST API."""

            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                self.messages = {}
                self.fetched = []

            async def fetch_message(self, mid):
                self.fetched.append(int(mid))
                return self.messages[int(mid)]

            def history(self, **kwargs):
                items = list(self.messages.values())

                async def gen():
                    for item in reversed(items):
                        yield item

                return gen()

        class Thread(discord.Thread):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                self.messages, self.fetched = {}, []

            async def fetch_message(self, mid):
                self.fetched.append(int(mid))
                return self.messages[int(mid)]

        hidden = discord.Permissions(view_channel=True).value
        self.general = Text(state=state, guild=self.guild, data=_channel_payload(CHANNEL, "general"))
        self.mods = Text(state=state, guild=self.guild, data=_channel_payload(
            MODS_CHANNEL, "mods", [(GUILD, 0, hidden), (MODS_ROLE, hidden, 0)]))
        # a mods channel that one uncached member may also read (discord.py lists them as a discord.Object)
        self.club = Text(state=state, guild=self.guild, data=_channel_payload(
            CLUB_CHANNEL, "club", [(GUILD, 0, hidden), (MODS_ROLE, hidden, 0)]))
        self.club_open = Text(state=state, guild=self.guild, data=_channel_payload(
            CLUB_OPEN_CHANNEL, "club-open", [(GUILD, 0, hidden), (MODS_ROLE, hidden, 0), (UNCACHED, hidden, 0, 1)]))
        # a lounge the Restricted role is denied; the second one also allows the Mods role explicitly
        self.lounge = Text(state=state, guild=self.guild, data=_channel_payload(
            LOUNGE_CHANNEL, "lounge", [(RESTRICTED_ROLE, 0, hidden)]))
        self.lounge_mods = Text(state=state, guild=self.guild, data=_channel_payload(
            LOUNGE_MODS_CHANNEL, "lounge-mods", [(RESTRICTED_ROLE, 0, hidden), (MODS_ROLE, hidden, 0)]))
        for channel in (self.general, self.mods, self.club, self.club_open, self.lounge, self.lounge_mods):
            self.guild._add_channel(channel)
        self.private_thread = Thread(guild=self.guild, state=state, data={
            "id": PRIVATE_THREAD, "type": 12, "guild_id": GUILD, "parent_id": CHANNEL, "owner_id": MOD,
            "name": "private", "last_message_id": None, "rate_limit_per_user": 0, "message_count": 0,
            "member_count": 1, "flags": 0, "thread_metadata": {
                "archived": False, "auto_archive_duration": 1440, "locked": False, "invitable": False,
                "archive_timestamp": "2026-01-01T00:00:00+00:00"}})
        self.guild._add_thread(self.private_thread)
        self.member = discord.Member(data=_member_payload(MEMBER), guild=self.guild, state=state)
        self.mod = discord.Member(data=_member_payload(MOD, [MODS_ROLE]), guild=self.guild, state=state)
        self.operator = discord.Member(data=_member_payload(OPERATOR, [MODS_ROLE]), guild=self.guild, state=state)
        self.both = discord.Member(data=_member_payload(BOTH, [MODS_ROLE, RESTRICTED_ROLE]), guild=self.guild,
                                   state=state)
        for m in (self.member, self.mod, self.operator, self.both):
            self.guild._add_member(m)
        world.text_class = Text
        self.other_guild = discord.Guild(data={
            "id": OTHER_GUILD, "name": "elsewhere", "owner_id": BOT, "emojis": [], "stickers": [], "features": [],
            "channels": [], "member_count": 2, "preferred_locale": "en-US",
            "roles": [role(OTHER_GUILD, "@everyone", _perm(**READ), 0)]}, state=state)
        state._add_guild(self.other_guild)
        self.elsewhere = Text(state=state, guild=self.other_guild, data={
            **_channel_payload(OTHER_CHANNEL, "elsewhere"), "guild_id": OTHER_GUILD})
        self.other_guild._add_channel(self.elsewhere)

    def post(self, channel, author, content, mid):
        message = SimpleNamespace(id=mid, channel=channel, author=author, content=content, clean_content=content,
                                  attachments=[], created_at=None, guild=self.guild, jump_url="")
        channel.messages[mid] = message
        return message


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


def _wire(s, *, extra_factories=()):
    """Hermes' own wiring: ``_wire_plugin_handlers(native)`` calls every factory with ``(native, adapter)``.
    The policy's factory binds the bot; the K5 installer runs after it, as the integration will."""
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    factories = [(_mod("adapt.interventions").discord_platform_handler, PLUGIN), *extra_factories]
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
    results = []

    def k5_factory(native, adapter):
        results.append(_mod("ingress.reactions").install_reaction_wakeup(native, adapter))

    _wire(s, extra_factories=[(k5_factory, PLUGIN + "-k5")])
    _configure(reaction_emoji={"id": EMOJI, "name": "nova"})
    assert results and results[0].outcome.value == "injected", results
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
    _wire(s, extra_factories=[(lambda n, a: _mod("ingress.reactions").install_reaction_wakeup(n, a), PLUGIN + "-k5")])
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
    _wire(s, extra_factories=[(lambda n, a: _mod("ingress.reactions").install_reaction_wakeup(n, a), PLUGIN + "-k5")])
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


async def test_wake_name_command_is_text_through_real_hermes(setup, monkeypatch):
    s = setup
    _wire(s)
    store = _configure(wake_names=["Nova"])
    classify = _mod("ingress.classify")
    admission = _mod("ingress.admission")
    original = classify.direct_signal
    monkeypatch.setattr(admission.classify, "direct_signal",
                        lambda adapter, message, guild_config=None: original(adapter, message,
                                                                             store.read().guild(BOT, GUILD)))
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
