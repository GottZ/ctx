"""S-INT acceptance: the whole plugin against real Hermes code, end to end, per ref.

Real plugin managers in three homes (launch home that runs the bot, an armed routed profile, an unarmed
one), real ``DiscordAdapter`` (constructor, admission, ``_handle_message``, ``send``, ``delete_message``), the
A7 wrapper exactly as the plugin installs it on ``GatewayRunner``, Hermes' own profile resolution choosing
the scope in which Hermes' hook dispatch runs, real reply metadata, the real ``TurnRunner`` progress methods,
and ``/policy`` installed on a real discord.py ``commands.Bot``. Not exercised: the model call itself (the
runner body beneath A7 is a stand-in that resolves the profile and dispatches the hooks the way Hermes does).
"""

from __future__ import annotations

import sys
from types import SimpleNamespace

import pytest
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, load_in
from unit.delivery_fakes import World

BOT = "910000000000000001"
GUILD_A = "910000000000000100"      # routes to the armed profile "routed"
GUILD_C = "910000000000000300"      # routes to the unarmed profile "bare"
OPERATOR = "910000000000000007"
MEMBER = "910000000000000008"
SPILL_MAX_CHARS = 2600              # the routed profile's hook-output spill limit (budget 2100)
GUIDANCE = "This is {server_name}; keep replies short. " + "Members here like calm, friendly answers. " * 60


def _config(home, operators=()):
    ops = ", ".join(f'"{o}"' for o in operators)
    (home / "config.yaml").write_text(
        f"plugins:\n  enabled: [{PLUGIN}]\n  entries:\n    {PLUGIN}:\n      settings:\n"
        f"        operators: [{ops}]\n", encoding="utf-8")


@pytest.fixture
def e2e(hermes, tmp_path, monkeypatch, request):
    _evict(ANCHOR)
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "root"))
    homes = Homes(tmp_path)
    _config(homes.root, [OPERATOR])
    routed = homes.profile("routed")
    with (routed / "config.yaml").open("a", encoding="utf-8") as fh:
        fh.write(f"hooks:\n  output_spill:\n    max_chars: {SPILL_MAX_CHARS}\n")
    bare = homes.profile("bare", enabled=False)
    for home in (homes.root, routed, bare):
        load_in(home)
    import discord
    from discord.ext import commands
    from gateway.config import PlatformConfig
    from gateway.profile_routing import ProfileRoute
    from gateway.run import GatewayRunner
    from hermes_cli import plugins
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override
    from plugins.platforms.discord.adapter import DiscordAdapter

    class PolicyAdapter(DiscordAdapter):
        """A subclass keeps the class shared by this test process untouched."""

    pending = {}

    async def _handle_message(self, event):
        """Stand-in beneath the installed A7 wrapper: Hermes resolves the profile, then runs the turn there."""
        home = self._resolve_profile_home_for_source(event.source)
        seen = pending["seen"]
        seen["home"] = home
        scope = set_hermes_home_override(str(home))
        try:
            ids = dict(session_id=f"s-{event.message_id}", turn_id=f"t-{event.message_id}")
            seen["context"] = plugins.invoke_hook("pre_llm_call", platform="discord", **ids)
            seen["tools"] = [plugins._dispatch_pre_tool_call_hooks(name, args, **ids)[0]
                             for name, args in pending["tools"]]
            seen["route"] = runtime().route.get()
        finally:
            reset_hermes_home_override(scope)
        return "ran"

    # The plugin wraps whatever GatewayRunner._handle_message is when the bot binds (A7 at the factory).
    monkeypatch.setattr(GatewayRunner, "_handle_message", _handle_message)

    world = World()
    world.user.display_name = "Helper"
    channels = {}
    for name, guild in (("general", GUILD_A), ("work", GUILD_A), ("lobby", GUILD_C)):
        channel = world.channel(world.new_id(), guild)
        channel.type = 0
        channel.name = name
        channel.guild.name = f"guild-{guild[-3:]}"
        channels[name] = channel
    runner = GatewayRunner.__new__(GatewayRunner)
    runner.config = SimpleNamespace(
        multiplex_profiles=True, group_sessions_per_user=True, thread_sessions_per_user=False,
        profile_routes=[ProfileRoute(name="a", platform="discord", profile="routed", guild_id=GUILD_A),
                        ProfileRoute(name="c", platform="discord", profile="bare", guild_id=GUILD_C)])
    adapter = PolicyAdapter(PlatformConfig(enabled=True, token="contract-test"))
    adapter._client = world
    adapter.gateway_runner = runner
    adapter._discord_history_backfill = lambda: False  # the client double keeps no channel history
    adapter._text_batch_delay_seconds = 0  # deliver each message at once (no batching window)
    captured = []

    async def capture(event):
        captured.append(event)

    adapter.handle_message = capture
    native = commands.Bot(command_prefix="!", intents=discord.Intents.none(), application_id=int(BOT))
    token = set_hermes_home_override(str(homes.root))
    try:
        sys.modules[f"{ANCHOR}.adapt.interventions"].discord_platform_handler(native, adapter)
    finally:
        reset_hermes_home_override(token)

    def runtime():
        return sys.modules[f"{ANCHOR}.runtime"].RUNTIME

    ops = sys.modules[f"{ANCHOR}.store.ops"]
    store = runtime().bots.for_bot(BOT).store
    for guild in (GUILD_A, GUILD_C):
        ops.bind(store, BOT, guild, by="cli")
    guild = ops.export(store.read(), BOT, GUILD_A)["bots"][BOT]["guilds"][GUILD_A]
    guild["worklog"]["channel"] = str(channels["work"].id)
    guild["progress"]["grouping"] = "rotate"
    guild["prompts"]["guild"] = GUIDANCE
    ops.import_guild(store, BOT, GUILD_A, guild, by="cli", prompt_cap=6000)

    def message(author_id, channel, text):
        msg = world.post(channel, f"<@{BOT}> {text}", author_id, "member" if author_id == MEMBER else "op")
        msg.author.bot = False
        msg.guild = SimpleNamespace(id=int(channel.guild.id), name=channel.guild.name, me=None)
        msg.type, msg.mentions, msg.reference = discord.MessageType.default, [world.user], None
        msg.attachments, msg.message_snapshots, msg.created_at = [], [], None
        return msg

    async def turn(msg, tools=(("terminal", {"command": "id"}),)):
        """Adapter admission and handler, then the runner through the installed A7 wrapper."""
        admitted, role = adapter._discord_message_admission(msg, claim=True)
        if not admitted:
            return None
        before = len(captured)
        handled = await adapter._handle_message(msg, role_authorized=role)
        assert len(captured) == before + 1, (handled, sys.modules[f"{ANCHOR}.ingress.admission"].decide(adapter, msg))
        event = captured[-1]
        pending.update(seen={}, tools=tools)
        result = await GatewayRunner._handle_message(runner, event)
        seen = pending["seen"]
        seen.update(result=result, event=event)
        return seen

    try:
        yield SimpleNamespace(homes=homes, routed=routed, bare=bare, world=world, channels=channels, runner=runner,
                              adapter=adapter, native=native, runtime=runtime(), message=message, turn=turn,
                              hermes=hermes, runner_class=GatewayRunner)
    finally:
        # Written whatever the test's result: a failing assertion must not erase the ref's status matrix.
        part = request.node.name.removeprefix("test_end_to_end_")
        hermes.write_report(f"status-matrix-e2e-{part}", {"interventions": runtime().status.snapshot()["interventions"]})
        _evict(ANCHOR)


def _contexts(seen):
    return "\n".join(r["context"] for r in seen["context"] if isinstance(r, dict) and "context" in r)


async def test_end_to_end_security(e2e):
    """Admission, lanes, assembled context and the closed fallback through the real turn path."""
    general, lobby = e2e.channels["general"], e2e.channels["lobby"]
    handle = e2e.runner_class._handle_message
    assert getattr(handle, "__dsp_wrapper__", None) == ("A7", 1)  # the runner wrapper the plugin installed

    # Public turn: admitted, run in the profile Hermes resolves for guild A, assembled context (guidance
    # shortened to the routed profile's spill budget), host tools blocked there, the channel lane allowed.
    public = await e2e.turn(e2e.message(MEMBER, general, "what is up?"),
                            tools=(("terminal", {"command": "id"}),
                                   ("discord", {"action": "fetch_messages", "channel_id": str(general.id),
                                                "limit": 10})))
    assert public["result"] == "ran" and public["home"].resolve() == e2e.routed.resolve()
    context = _contexts(public)
    assert context.index("[Operator frame]") < context.index("[Capability lane: public]") \
        < context.index("[Conversation lane: direct]") < context.index("> This is guild-100; keep replies short.")
    assert "[Server guidance shortened here" in context and len(context) <= SPILL_MAX_CHARS - 500
    assert "BLOCKED by ctx_discord_policy" in public["tools"][0] and public["tools"][1] is None

    # Operator turn: operator lane, no server guidance, tools allowed.
    operator = await e2e.turn(e2e.message(OPERATOR, general, "check the queue"))
    assert operator["home"].resolve() == e2e.routed.resolve()
    context = _contexts(operator)
    assert "[Capability lane: operator]" in context and "keep replies short" not in context
    assert operator["tools"] == [None] and operator["route"].tool_started is True

    # Closed fallback: guild C routes to the unarmed profile; the member is not admitted.
    assert await e2e.turn(e2e.message(MEMBER, lobby, "hello")) is None
    status = e2e.runtime.status.snapshot()["interventions"]
    assert status["A4"]["outcome"] == "fallback" and "not enabled" in status["A4"]["reason"]

    # Every security intervention reports an outcome; the public context's budget problem is visible per bot,
    # guild, lane and conversation mode.
    budget = f"A9[{BOT}/{GUILD_A}/public/direct]"
    expected = {"A1[default]", "A1[routed]", "A2", "A3", "A4", "A5", "A6", "A7", "A8[default]", "A8[routed]", "A9",
                budget}
    assert expected <= set(status), sorted(expected - set(status))
    for name in ("A1[default]", "A1[routed]", "A2", "A3", "A5", "A6", "A7", "A8[default]", "A8[routed]", "A9"):
        assert status[name]["outcome"] in ("ok", "wrapped", "armed"), (name, status[name])
    assert status[budget]["outcome"] == "fallback" and "shortened" in status[budget]["reason"]
    assert not [name for name in status if name.startswith(f"A9[{BOT}/{GUILD_A}/operator/")]


async def test_end_to_end_comfort(e2e):
    """Worklog redirection, deleting a redirected message, rotate per guild, the interface."""
    from gateway.config import Platform
    from gateway.run_turn_runner import TurnRunner
    from gateway.session import SessionSource
    from gateway.turn_context import TurnContext

    general, work = e2e.channels["general"], e2e.channels["work"]

    # Operator tool output is redirected into a worklog thread.
    operator = await e2e.turn(e2e.message(OPERATOR, general, "check the queue"))
    route = operator["route"]
    metadata = e2e.runner._thread_metadata_for_source(operator["event"].source, route.message_id)
    token = e2e.runtime.route.set(route)
    try:
        sent = await e2e.adapter.send(str(general.id), "queue: 3 jobs", metadata=metadata)
    finally:
        e2e.runtime.route.reset(token)
    assert sent.success, sent.error
    threads = [c for c in e2e.world.channels.values() if c.parent_id == work.id]
    assert threads and any(m.content == "queue: 3 jobs" for m in threads[0].messages.values())

    # Deleting that message by the chat the core knows (the origin) reaches the thread it lives in (K1b over K1).
    assert await e2e.adapter.delete_message(str(general.id), sent.message_id) is True
    assert threads[0].messages[int(sent.message_id)].deleted

    # Rotate per guild on the real TurnRunner: guild A rotates, the other active guild keeps the profile's.
    def lines(guild):
        runner = object.__new__(TurnRunner)
        runner._ctx = TurnContext(source=SessionSource(platform=Platform.DISCORD, chat_id=str(general.id),
                                                       chat_type="group", guild_id=guild),
                                  progress_grouping="accumulate")
        st = runner._progress_edit_state(e2e.adapter)
        for item in ("a", "b"):
            runner._progress_absorb(st, item)
        return st.progress_lines

    assert lines(GUILD_A) == ["b"] and lines(GUILD_C) == ["a", "b"]

    # Every comfort intervention reports a working outcome (K5 reaction wakeup, K7 hydration, K8 server models,
    # R1 profile per server and P2 the settings page's server directory among them); /policy runs on the bot
    # binding's own store.
    status = e2e.runtime.status.snapshot()["interventions"]
    for name in ("K1", "K1b", "K2", "K2b", "K3", "K4", "K5", "K6", "K7", "K8", "R1", "P2"):
        assert status.get(name, {}).get("outcome") in ("native", "injected", "wrapped", "ok"), (name, status.get(name))
    ui = sys.modules[f"{ANCHOR}.ui.items"].binding_for(e2e.native)
    assert ui is not None and ui.store is e2e.runtime.bots.for_bot(BOT).store


# ── S-INT 2: a wake name, admission, hydration and the turn context on real discord.py objects ────────────


@pytest.fixture
async def e2e_real(hermes, tmp_path, monkeypatch, request):
    """One home that runs the bot, Hermes' real Discord admission and handler on a real ``commands.Bot`` whose
    cache holds real Guild/Role/Member/TextChannel objects (discord.py computes every permission), the A7 wrapper
    on ``GatewayRunner`` with a stand-in turn beneath it that dispatches the hooks the way Hermes does."""
    import asyncio

    from fakes.discord_world import BOT as RBOT, MEMBER as RMEMBER, OPERATOR as ROPERATOR, DiscordWorld

    _evict(ANCHOR)
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "root"))
    homes = Homes(tmp_path)
    _config(homes.root, [ROPERATOR])
    manager = load_in(homes.root)
    import discord
    from discord.ext import commands
    from gateway.config import Platform
    from gateway.platforms.helpers import MessageDeduplicator
    from gateway.run import GatewayRunner
    from hermes_cli import plugins
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override
    from plugins.platforms.discord.adapter import DiscordAdapter

    class PolicyAdapter(DiscordAdapter):
        """A subclass keeps the class shared by this test process untouched."""

    pending = {}

    async def _handle_message(self, event):
        home = self._resolve_profile_home_for_source(event.source)
        seen = pending["seen"]
        scope = set_hermes_home_override(str(home))
        try:
            ids = dict(session_id=f"s-{event.message_id}", turn_id=f"t-{event.message_id}")
            seen["context"] = plugins.invoke_hook("pre_llm_call", platform="discord", **ids)
            seen["route"] = runtime().route.get()
        finally:
            reset_hermes_home_override(scope)
        return "ran"

    monkeypatch.setattr(GatewayRunner, "_handle_message", _handle_message)
    bot = commands.Bot(command_prefix="!", intents=discord.Intents.default(), application_id=int(RBOT))
    await bot.__aenter__()                      # binds the running loop; no login, no gateway
    model = DiscordWorld(bot)
    runner = GatewayRunner.__new__(GatewayRunner)
    runner.config = SimpleNamespace(multiplex_profiles=False, profile_routes=[], group_sessions_per_user=True,
                                    thread_sessions_per_user=False)
    runner._resolve_profile_home_for_source = lambda source: homes.root
    runner._profile_name_for_source = lambda source, adapter_profile=None: None
    adapter = PolicyAdapter.__new__(PolicyAdapter)
    adapter.platform, adapter.config, adapter._owner_profile = Platform.DISCORD, None, None
    adapter.gateway_runner, adapter._dedup, adapter._client = runner, MessageDeduplicator(), bot
    adapter._allowed_user_ids, adapter._allowed_role_ids = {"950000000000000999"}, set()
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
    manager.get_platform_handler_factories = lambda platform: (
        [(sys.modules[f"{ANCHOR}.adapt.interventions"].discord_platform_handler, PLUGIN)] if platform == "discord"
        else [])
    token = set_hermes_home_override(str(homes.root))
    try:
        adapter._wire_plugin_handlers(bot)                 # Hermes' own wiring calls the policy's factory
    finally:
        reset_hermes_home_override(token)

    def runtime():
        return sys.modules[f"{ANCHOR}.runtime"].RUNTIME

    ops = sys.modules[f"{ANCHOR}.store.ops"]
    store = runtime().bots.for_bot(RBOT).store
    ops.bind(store, RBOT, model.guild.id, by="cli")
    guild = ops.export(store.read(), RBOT, str(model.guild.id))["bots"][RBOT]["guilds"][str(model.guild.id)]
    guild["identity"]["wake_names"] = ["Nova"]
    ops.import_guild(store, RBOT, str(model.guild.id), guild, by="cli", prompt_cap=6000)

    async def turn(author, channel, content, mid):
        msg = SimpleNamespace(id=mid, author=author, guild=model.guild, channel=channel, type=discord.MessageType.default,
                              mentions=[], content=content, reference=None, attachments=[], message_snapshots=[],
                              created_at=None, webhook_id=None)
        admitted, role = adapter._discord_message_admission(msg, claim=True)
        if not admitted:
            return None
        before = len(captured)
        await adapter._handle_message(msg, role_authorized=role)
        assert len(captured) == before + 1
        event = captured[-1]
        pending["seen"] = {}
        result = await GatewayRunner._handle_message(runner, event)
        return dict(pending["seen"], result=result, event=event)

    try:
        yield SimpleNamespace(model=model, bot=bot, turn=turn, runtime=runtime(), member=RMEMBER, hermes=hermes)
    finally:
        part = request.node.name.removeprefix("test_end_to_end_")
        hermes.write_report(f"status-matrix-e2e-{part}", {"interventions": runtime().status.snapshot()["interventions"]})
        sys.modules[f"{ANCHOR}.snapshot"].detach(RBOT)        # the P2 directory heartbeat of the bound bot
        await bot.__aexit__(None, None, None)
        _evict(ANCHOR)


async def test_end_to_end_wake_name_admission_hydration_turn_context(e2e_real):
    """A member addresses the bot by its wake name (no mention) and links a message: admitted on the public
    lane, the linked message of a channel the whole audience can read is quoted into the turn, and the turn
    runs under its route with the assembled context. The same message linking a moderators-only channel is
    admitted as well, but nothing of that channel reaches the turn."""
    from fakes.discord_world import _channel_payload

    m = e2e_real.model
    news = m.text_class(state=e2e_real.bot._connection, guild=m.guild,
                        data=_channel_payload("950000000000000208", "announcements"))   # everyone reads it
    m.guild._add_channel(news)
    note = m.post(news, m.mod, "the meetup moved to friday", 950000000000009401)
    secret = m.post(m.mods, m.mod, "mods only: ban list", 950000000000009402)
    link = f"https://discord.com/channels/{m.guild.id}/{news.id}/{note.id}"
    hidden = f"https://discord.com/channels/{m.guild.id}/{m.mods.id}/{secret.id}"

    readable = await e2e_real.turn(m.member, m.general, f"Nova, what does this say? {link}", 950000000000009411)
    assert readable is not None and readable["result"] == "ran"
    route = readable["route"]
    assert route.lane == "public" and route.mode == "direct" and route.author_id == e2e_real.member
    text = readable["event"].text
    assert text.startswith("Nova, what does this say?") and "Linked Discord message" in text
    assert "> the meetup moved to friday" in text
    context = _contexts(readable)
    assert "[Capability lane: public]" in context and "[Conversation lane: direct]" in context

    closed = await e2e_real.turn(m.member, m.general, f"Nova, and this? {hidden}", 950000000000009412)
    assert closed is not None and closed["route"].lane == "public"
    assert "ban list" not in closed["event"].text and "Linked Discord message" not in closed["event"].text
    assert m.mods.fetched == []                                      # not even read

    assert await e2e_real.turn(m.member, m.general, f"ask Nova about {link}", 950000000000009413) is None
    status = e2e_real.runtime.status.snapshot()["interventions"]
    for name in ("K5", "K7", "K8", "R1"):
        assert status.get(name, {}).get("outcome") in ("native", "injected", "wrapped", "ok"), (name, status.get(name))
