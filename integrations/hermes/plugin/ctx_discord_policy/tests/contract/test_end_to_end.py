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

    # Every comfort intervention reports a working outcome; /policy runs on the bot binding's own store.
    status = e2e.runtime.status.snapshot()["interventions"]
    for name in ("K1", "K1b", "K2", "K2b", "K3", "K4", "K6"):
        assert status.get(name, {}).get("outcome") in ("native", "injected", "wrapped", "ok"), (name, status.get(name))
    ui = sys.modules[f"{ANCHOR}.ui.items"].binding_for(e2e.native)
    assert ui is not None and ui.store is e2e.runtime.bots.for_bot(BOT).store
