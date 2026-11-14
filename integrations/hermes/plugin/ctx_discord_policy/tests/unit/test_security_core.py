"""S3: the security core end to end — admission, routing, runner authorization, lanes, catalog.

Every fixture is created through the real write paths: the plugin instance registers through its entry
point, the bot binds through the platform-handler factory, guilds are activated and configured through
the store operations the CLI uses, and operators come from the (per-home) settings reader.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from types import SimpleNamespace

import pytest
from fakes.ctx import FakeCtx
from fakes.gateway import (APP_ID, BOT_USER, CHANNEL, GUILD, MEMBER, OPERATOR, OTHER_CHANNEL, HookBus,
                           install_gateway_module, make_classes, message, remove_gateway_module, user)

TERMINAL = ("terminal", {"command": "id"})
FETCH_HERE = ("discord", {"action": "fetch_messages", "channel_id": CHANNEL, "limit": 20})
FETCH_OTHER = ("discord", {"action": "fetch_messages", "channel_id": OTHER_CHANNEL, "limit": 20})


class World(SimpleNamespace):
    def settings(self, home: Path, **values) -> None:
        path = home / "settings.json"
        current = json.loads(path.read_text()) if path.exists() else {}
        current.update(values)
        path.write_text(json.dumps(current))

    def configure(self, **sections) -> None:
        """Change the guild through export -> edit -> import, the CLI's write path."""
        ops = self.anchor.mod("store.ops")
        guild = ops.export(self.store.read(), APP_ID, GUILD)["bots"][APP_ID]["guilds"][GUILD]
        for path, value in sections.items():
            head, _, leaf = path.partition("__")
            target = guild[head]
            for part in leaf.split("__")[:-1] if leaf else []:
                target = target[part]
            if leaf:
                target[leaf.split("__")[-1]] = value
            else:
                guild[head] = value
        ops.import_guild(self.store, APP_ID, GUILD, guild, by="cli", prompt_cap=6000)

    def rebuild(self, **options) -> None:
        """New adapter/runner classes with other Hermes settings, bound like a reconnect."""
        remove_gateway_module(self.classes.module)
        self.classes = make_classes(home_resolver=lambda source: self.target, bus=self.bus, **options)
        install_gateway_module(self.classes.module)
        self.runner = self.classes.GatewayRunner()
        self.adapter = self.classes.DiscordAdapter(self.runner)
        self.runner.plan = [TERMINAL, FETCH_HERE, FETCH_OTHER]
        self.anchor.RUNTIME.status.record(self.anchor.mod("adapt.status").Result(
            "A7", "S", self.anchor.mod("adapt.status").Outcome.OFF, "rebuild", "test"))
        self.anchor.mod("adapt.interventions").discord_platform_handler(
            SimpleNamespace(application_id=int(APP_ID)), self.adapter)

    async def deliver(self, msg, *, adapter=None):
        adapter = adapter or self.adapter
        adapter.last_result = None
        admitted, role_authorized = adapter._discord_message_admission(msg, claim=True)
        if not admitted:
            return None
        await adapter._handle_message(msg, role_authorized)
        return adapter.last_result


@pytest.fixture
def world(anchor, monkeypatch):
    owner = anchor.tmp / "owner"
    owner.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(owner))
    ctx = FakeCtx("default")
    anchor.entry.register(ctx)
    config = anchor.mod("config")

    def reader(home, key, default):
        path = Path(home) / "settings.json"
        return json.loads(path.read_text()).get(key, default) if path.exists() else default

    config.set_reader(reader)
    w = World(anchor=anchor, owner=owner, ctx=ctx, target=owner)
    w.settings(owner, operators=[OPERATOR])
    bus = HookBus(ctx.hooks)
    classes = make_classes(home_resolver=lambda source: w.target, bus=bus)
    install_gateway_module(classes.module)
    w.bus, w.classes = bus, classes
    w.runner = classes.GatewayRunner()
    w.adapter = classes.DiscordAdapter(w.runner)
    w.runner.plan = [TERMINAL, FETCH_HERE, FETCH_OTHER]
    interventions = anchor.mod("adapt.interventions")
    interventions.discord_platform_handler(SimpleNamespace(application_id=int(APP_ID)), w.adapter)
    w.binding = anchor.RUNTIME.bots.for_bot(APP_ID)
    w.store = w.binding.store
    anchor.mod("store.ops").bind(w.store, APP_ID, GUILD, by="cli")
    try:
        yield w
    finally:
        remove_gateway_module(w.classes.module)
        config.set_reader(None)


def mention():
    return [user(BOT_USER, bot=True)]


async def test_security_interventions_are_in_place(world):
    status = world.anchor.RUNTIME.status
    for name in ("A3", "A4", "A5", "A6", "A7"):
        assert status.get(name) is not None and not status.get(name).outcome.degraded, name
    data = json.loads((world.owner / "plugin-data" / "ctx_discord_policy" / "status.json").read_text())
    assert APP_ID in data["bots"] and data["interventions"]["A7"]["outcome"] == "wrapped"


async def test_public_member_gets_an_exact_route_and_only_catalog_tools(world):
    turn = await world.deliver(message(user(MEMBER), mentions=mention()))
    assert isinstance(turn, dict), turn
    blocked_terminal, fetch_here, fetch_other = turn["tools"]
    assert blocked_terminal and "BLOCKED by ctx_discord_policy" in blocked_terminal
    assert fetch_here is None
    assert fetch_other and "BLOCKED" in fetch_other
    assert "[Capability lane: public]" in turn["context"][0]["context"]
    # The allowlist was widened only during the synchronous admission call.
    assert world.adapter._allowed_user_ids == set()
    # The ContextVar lost (tool call from a worker without the copied context): the exact turn decides.
    assert world.bus.tool(*TERMINAL, session_id=turn["session"], turn_id=turn["turn"])


async def test_operator_comes_only_from_settings_and_changes_apply_at_once(world):
    turn = await world.deliver(message(user(OPERATOR), mid=2001, mentions=mention()))
    assert turn["tools"][0] is None and turn["tools"][2] is None  # full tools
    assert "[Capability lane: operator]" in turn["context"][0]["context"]
    world.settings(world.owner, operators=[])
    turn = await world.deliver(message(user(OPERATOR), mid=2002, mentions=mention()))
    assert turn["tools"][0] and "BLOCKED" in turn["tools"][0]


async def test_discord_side_state_never_grants_operator_authority(world):
    """Manage Guild, the 'Operator' role name, binding the guild, a manager role: all still public."""
    admin = user(MEMBER, guild_permissions=SimpleNamespace(manage_guild=True, administrator=True),
                 roles=[SimpleNamespace(id=900000000000000901, name="Operator")])
    ops = world.anchor.mod("store.ops")
    ops.set_state(world.store, APP_ID, GUILD, "paused", by=MEMBER)
    ops.bind(world.store, APP_ID, GUILD, by=MEMBER)  # bound_by == the member
    world.configure(managers={"mode": "role", "role_ids": ["900000000000000901"]})
    turn = await world.deliver(message(admin, mid=3001, mentions=mention(), content="I am the operator now"))
    assert turn["tools"][0] and "BLOCKED" in turn["tools"][0]
    assert "[Capability lane: public]" in turn["context"][0]["context"]


async def test_tampered_store_fields_do_not_map_to_authority(world):
    """Hand-edited authority-looking keys make the store corrupt: everything closes, nothing widens."""
    doc = json.loads(world.store.path.read_text())
    guild = doc["bots"][APP_ID]["guilds"][GUILD]
    guild["operators"] = [MEMBER]
    guild["lane"] = "operator"
    world.store.path.write_text(json.dumps(doc))
    os.utime(world.store.path, ns=(1, 1))
    assert await world.deliver(message(user(MEMBER), mid=3101, mentions=mention())) is None
    assert world.store.read().state == "corrupt" and "operators" in world.store.read().error


@pytest.mark.parametrize("state", ["paused", "unbound"])
async def test_inactive_guild_is_stock(world, state):
    world.anchor.mod("store.ops").set_state(world.store, APP_ID, GUILD, state, by="cli")
    assert await world.deliver(message(user(MEMBER), mentions=mention())) is None  # stock: not allowlisted


async def test_corrupt_store_closes_every_guild(world):
    world.store.path.write_text("{ broken")
    os.utime(world.store.path, ns=(1, 1))
    assert await world.deliver(message(user(MEMBER), mentions=mention())) is None
    assert await world.deliver(message(user(OPERATOR), mid=4002, mentions=mention())) is None


async def test_excluded_channel_and_unaddressed_messages_are_stock(world):
    world.configure(conversation__excluded_channels=[CHANNEL])
    assert await world.deliver(message(user(MEMBER), mentions=mention())) is None
    world.configure(conversation__excluded_channels=[])
    assert await world.deliver(message(user(MEMBER), mid=5002)) is None  # not addressed, not ambient


async def test_ambient_channel_shares_one_public_session_but_never_the_operators(world):
    world.configure(conversation__ambient_channels=[CHANNEL])
    first = await world.deliver(message(user(MEMBER), mid=6001, content="lunch?"))
    second = await world.deliver(message(user("900000000000000501"), mid=6002, content="pizza"))
    assert first["event"].source.user_id == second["event"].source.user_id == f"ambient:{CHANNEL}"
    assert first["session"] == second["session"]
    assert first["event"].raw_message.content.startswith(f"[Discord participant: member ({MEMBER})]")
    operator = await world.deliver(message(user(OPERATOR), mid=6003, content="deploy it"))
    assert operator["event"].source.user_id == OPERATOR and operator["session"] != first["session"]
    assert operator["tools"][0] is None and first["tools"][0]


async def test_unarmed_target_profile_closes_public_admission(world, anchor):
    world.target = anchor.tmp / "routed-profile-without-plugin"
    assert await world.deliver(message(user(MEMBER), mentions=mention())) is None
    a4 = anchor.RUNTIME.status.get("A4")
    assert a4.outcome.value == "fallback" and "not enabled" in a4.reason


async def test_unarmed_note_is_recorded_right_after_boot(world, anchor, monkeypatch):
    """The rate limit must not swallow the first note while the monotonic clock is still small."""
    admission = anchor.mod("ingress.admission")
    monkeypatch.setattr(admission.time, "monotonic", lambda: 5.0)
    world.target = anchor.tmp / "fresh-boot-unarmed"
    assert await world.deliver(message(user(MEMBER), mentions=mention())) is None
    assert anchor.RUNTIME.status.get("A4").outcome.value == "fallback"


async def test_runner_side_target_check_refuses_a_route_that_moved(world, anchor, monkeypatch):
    """Admission saw an armed target; the runner resolves the real source to an unarmed one (A4, 2nd check)."""
    authority = anchor.mod("policy.authority")
    unarmed = anchor.mod("hermes").home_key(anchor.tmp / "unarmed")
    monkeypatch.setattr(authority, "event_target_key", lambda runner, event: unarmed)
    assert await world.deliver(message(user(MEMBER), mid=7001, mentions=mention())) is None  # dropped
    assert world.runner.turns == []  # no turn ran, neither with nor without a lane
    # The route is revoked: a later check for the same message (e.g. a queued replay) finds nothing.
    assert not authority.authorizes(SimpleNamespace(platform="discord", user_id=MEMBER, chat_id=CHANNEL,
                                                    message_id="7001"))


async def test_restored_fifo_event_finds_its_own_route_and_never_inherits_one(world, anchor):
    runtime, routes = anchor.RUNTIME, anchor.routes
    turn = await world.deliver(message(user(MEMBER), mid=8001, mentions=mention()))
    assert turn["tools"][0]  # public
    op_route = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="8002",
                            author_id=OPERATOR, lane="operator", target_home_key=anchor.mod("hermes").home_key(world.owner))
    token = runtime.route.set(op_route)  # a drain task created inside the operator's context
    try:
        restored = SimpleNamespace(source=turn["event"].source, raw_message=None, message_id="8001")
        replay = await world.runner._handle_message(restored)
        assert replay["tools"][0] and "BLOCKED" in replay["tools"][0]  # its own public route, not the inherited one
        stranger = SimpleNamespace(source=SimpleNamespace(platform="discord", chat_id=CHANNEL, user_id="900000000000000999",
                                                          message_id="8999"), raw_message=None, message_id="8999")
        assert await world.runner._handle_message(stranger) == "unauthorized"
    finally:
        runtime.route.reset(token)


async def test_internal_event_in_a_public_session_never_inherits_an_operator_route(world, anchor):
    """A drain task created inside an operator's context executes an internal event (no user, no message
    id, no authorization) for a public member's session: it must run without the operator's lane."""
    runtime, routes = anchor.RUNTIME, anchor.routes
    turn = await world.deliver(message(user(MEMBER), mid=8101, mentions=mention()))
    assert turn["tools"][0]  # the session is a public one now
    op_route = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="8102",
                            author_id=OPERATOR, lane="operator",
                            target_home_key=anchor.mod("hermes").home_key(world.owner))
    token = runtime.route.set(op_route)
    try:
        source = SimpleNamespace(**{**vars(turn["event"].source), "message_id": None})  # no originating message
        internal = SimpleNamespace(source=source, raw_message=None, message_id=None, internal=True)
        replay = await world.runner._handle_message(internal)
    finally:
        runtime.route.reset(token)
    assert replay["session"] == turn["session"]
    assert replay["tools"][0] and "BLOCKED" in replay["tools"][0]


def test_authorization_needs_the_exact_route(world, anchor):
    authority, routes, runtime = anchor.mod("policy.authority"), anchor.routes, anchor.RUNTIME
    home = anchor.mod("hermes").home_key(world.owner)
    route = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="9001", author_id=MEMBER,
                         lane="public", target_home_key=home)
    exact = dict(platform="discord", user_id=MEMBER, chat_id=CHANNEL, message_id="9001")
    token = runtime.route.set(route)
    try:
        assert authority.authorizes(SimpleNamespace(**exact))
        for change in ({"user_id": "900000000000000777"}, {"chat_id": OTHER_CHANNEL}, {"message_id": "9002"},
                       {"platform": "telegram"}):
            assert not authority.authorizes(SimpleNamespace(**{**exact, **change})), change
        anchor.mod("store.ops").set_state(world.store, APP_ID, GUILD, "paused", by="cli")
        assert not authority.authorizes(SimpleNamespace(**exact))
    finally:
        runtime.route.reset(token)
    assert not authority.authorizes(SimpleNamespace(**exact))  # no route in context, none stored


def test_unarmed_route_never_authorizes(world, anchor):
    authority, routes, runtime = anchor.mod("policy.authority"), anchor.routes, anchor.RUNTIME
    route = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="9101", author_id=MEMBER,
                         lane="public", target_home_key=anchor.mod("hermes").home_key(anchor.tmp / "elsewhere"))
    token = runtime.route.set(route)
    try:
        assert not authority.authorizes(SimpleNamespace(platform="discord", user_id=MEMBER, chat_id=CHANNEL,
                                                        message_id="9101"))
    finally:
        runtime.route.reset(token)


async def test_admission_return_type_change_closes(world, anchor):
    cls = type(world.adapter)
    original = cls._discord_message_admission.__wrapped__

    def changed(self, msg, *, claim):
        return {"admitted": True}

    cls._discord_message_admission.__wrapped__ = changed  # cosmetic; the wrapper holds the original
    wrapper = anchor.mod("adapt.interventions")._make_admission(changed)
    assert wrapper(world.adapter, message(user(MEMBER), mentions=mention()), claim=True) == (False, False)
    assert anchor.RUNTIME.status.get("A5").outcome.value == "off"
    assert await world.deliver(message(user(MEMBER), mid=9201, mentions=mention())) is None
    cls._discord_message_admission.__wrapped__ = original


async def test_unverified_reply_is_checked_before_routing(world):
    async def fetch_own(mid):
        return SimpleNamespace(author=SimpleNamespace(id=int(BOT_USER)))

    async def fetch_other(mid):
        return SimpleNamespace(author=SimpleNamespace(id=int(OPERATOR)))

    reply = message(user(MEMBER), mid=9301, reference=SimpleNamespace(message_id=42, resolved=None))
    reply.channel.fetch_message = fetch_own
    turn = await world.deliver(reply)
    assert isinstance(turn, dict) and turn["tools"][1] is None
    reply2 = message(user(MEMBER), mid=9302, reference=SimpleNamespace(message_id=43, resolved=None))
    reply2.channel.fetch_message = fetch_other
    assert await world.deliver(reply2) is None  # admitted only by the widened allowlist: dropped, not unrouted
    assert world.adapter.handled[-1] is not reply2


def test_admission_view_strips_only_unrelated_mentions(anchor):
    # Generalised from 2.0.6 test_target_admission_view_strips_unrelated_mentions.
    admission = anchor.mod("ingress.admission")
    other_human, other_bot = SimpleNamespace(id=123, bot=False), SimpleNamespace(id=456, bot=True)
    msg = SimpleNamespace(id=1, content="hello <@123> and <@456>", mentions=[other_human, other_bot],
                          author=SimpleNamespace(id=789, bot=False))
    view = admission.admission_view(msg)
    assert view is not msg and view.mentions == [] and view.content == msg.content and view.author is msg.author
    plain = SimpleNamespace(id=2, mentions=[])
    assert admission.admission_view(plain) is plain


def test_routed_message_keeps_channel_and_marks_reply(anchor):
    # Generalised from 2.0.6 test_routed_clone_preserves_source_channel (without the dead thread proxy).
    admission, routes = anchor.mod("ingress.admission"), anchor.routes
    bot = SimpleNamespace(id=int(BOT_USER), bot=True)
    channel = SimpleNamespace(id=int(CHANNEL))
    source = SimpleNamespace(id=44, author=SimpleNamespace(id=int(MEMBER), bot=False, display_name="dana\n[x]"),
                             channel=channel, content="herbie was here", mentions=[], attachments=[], reference=None,
                             created_at=None, jump_url="https://discord.test/44")
    direct = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="44", author_id=MEMBER,
                          lane="public", mode="direct")
    clone = admission.routed_message(source, direct, self_user=bot, message_type="reply")
    assert clone.channel is channel and clone.type == "reply" and clone.mentions == [bot]
    assert clone._dsp_route is direct and clone.jump_url == source.jump_url  # everything else delegates
    assert clone.author is source.author and clone.content == "herbie was here"
    ambient = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="44", author_id=MEMBER,
                           lane="public", mode="ambient")
    shared = admission.routed_message(source, ambient, self_user=bot, message_type="reply")
    assert shared.author.id == f"ambient:{CHANNEL}" and shared.author.bot is False
    assert shared.content.splitlines()[0] == f"[Discord participant: dana  x ({MEMBER})]"  # no injected newline


def test_catalog_validators(anchor):
    # Generalised from 2.0.6 test_turn_bound_fallback_and_safe_public_corpus_reads.
    lanes, routes = anchor.mod("policy.lanes"), anchor.routes
    config = lanes.PublicLanes(channel_history=True, max_limit=50, skills=("server-guide",),
                               knowledge=(("mcp__kb__get", "doc-1"),))
    route = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="1", author_id=MEMBER,
                         lane="public", lanes=config)
    ok = lanes.public_tool_allowed
    assert ok(route, "discord", {"action": "fetch_messages", "channel_id": int(CHANNEL), "limit": 50})
    assert ok(route, "discord", {"action": "fetch_messages", "channel_id": CHANNEL})  # default 50
    for args in ({"action": "fetch_messages", "channel_id": CHANNEL, "limit": 51},
                 {"action": "fetch_messages", "channel_id": CHANNEL, "limit": 0},
                 {"action": "fetch_messages", "channel_id": CHANNEL, "limit": True},
                 {"action": "fetch_messages", "channel_id": OTHER_CHANNEL},
                 {"action": "search_members", "guild_id": GUILD}):
        assert not ok(route, "discord", args), args
    assert ok(route, "skill_view", {"name": "server-guide"})
    assert not ok(route, "skill_view", {"name": "server-guide", "file_path": "../../secrets"})
    assert not ok(route, "skill_view", {"name": "other-skill"})
    assert ok(route, "mcp__kb__get", {"id": "doc-1"})
    for tool, args in (("mcp__kb__get", {"id": "doc-2"}), ("mcp__kb__get", {"id": "doc-1", "scope": "all"}),
                       ("mcp__kb__get", {}), ("mcp__other__get", {"id": "doc-1"}), ("terminal", {"command": "id"}),
                       ("clarify", {}), ("write_file", {"path": "x"})):
        assert not ok(route, tool, args), (tool, args)
    off = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="1", author_id=MEMBER,
                       lane="public", lanes=lanes.PublicLanes(channel_history=False))
    assert not ok(off, "discord", {"action": "fetch_messages", "channel_id": CHANNEL})
    text = lanes.capability_lane_text(route)
    assert "server-guide" in text and "mcp__kb__get doc-1" in text and "up to 50" in text


async def test_configured_lanes_are_frozen_into_the_route(world):
    world.configure(lanes__skills=["server-guide"], lanes__channel_history__max_limit=10)
    world.runner.plan = [("skill_view", {"name": "server-guide"}),
                         ("discord", {"action": "fetch_messages", "channel_id": CHANNEL, "limit": 20})]
    turn = await world.deliver(message(user(MEMBER), mid=9401, mentions=mention()))
    assert turn["tools"] == [None, turn["tools"][1]] and "BLOCKED" in turn["tools"][1]


def test_contextless_turn_in_a_public_session_gets_no_tools(world, anchor):
    # Generalised from 2.0.6 test_lifecycle_guard / test_per_turn_capability_lanes.
    hooks, routes, runtime = anchor.hooks, anchor.routes, anchor.RUNTIME
    public = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="1", author_id=MEMBER,
                          lane="public")
    token = runtime.route.set(public)
    try:
        hooks.on_pre_llm_call(session_id="shared", turn_id="public-turn")
    finally:
        runtime.route.reset(token)
    for tool in ("terminal", "write_file", "clarify", "mcp__ctx__query"):
        assert hooks.on_pre_tool_call(tool_name=tool, session_id="shared", turn_id="contextless")["action"] == "block"
    # An exact operator route in the same (marked) session keeps operator tools (2.0.6
    # test_exact_operator_route_clears_stale_public_marker).
    operator = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="2", author_id=OPERATOR,
                            lane="operator")
    token = runtime.route.set(operator)
    try:
        hooks.on_pre_llm_call(session_id="shared", turn_id="operator-turn")
        assert hooks.on_pre_tool_call(tool_name="terminal", session_id="shared", turn_id="operator-turn") is None
    finally:
        runtime.route.reset(token)
    assert hooks.on_pre_tool_call(tool_name="terminal", session_id="shared", turn_id="operator-turn") is None


async def test_failed_registration_leaves_routes_powerless(anchor, monkeypatch):
    # Generalised from 2.0.6 test_register_fail_closed: without armed hooks no route authorizes.
    home = anchor.tmp / "failing"
    home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(home))
    with pytest.raises(RuntimeError):
        anchor.entry.register(FakeCtx("default", fail_hook_at=2))
    authority, routes = anchor.mod("policy.authority"), anchor.routes
    route = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="1", author_id=MEMBER,
                         lane="public", target_home_key=anchor.mod("hermes").home_key(home))
    token = anchor.RUNTIME.route.set(route)
    try:
        assert not authority.authorizes(SimpleNamespace(platform="discord", user_id=MEMBER, chat_id=CHANNEL,
                                                        message_id="1"))
    finally:
        anchor.RUNTIME.route.reset(token)


async def test_public_members_cannot_run_gateway_commands(world):
    """`@bot /restart` from a member is typed as a gateway command by Hermes and would bypass every tool
    hook; public routes carrying a command are dropped, operator commands pass."""
    for text in ("/restart", "/approve always", "/yolo", "/stop"):
        msg = message(user(MEMBER), mid=10000 + len(text), mentions=mention(), content=f"<@{BOT_USER}> {text}")
        assert await world.deliver(msg) is None, text
    assert world.runner.turns == []
    turn = await world.deliver(message(user(OPERATOR), mid=10101, mentions=mention(), content=f"<@{BOT_USER}> /stop"))
    assert isinstance(turn, dict)


async def test_threads_never_put_operator_and_public_members_in_one_session(world):
    thread = dict(channel_type=11, parent_id="900000000000000299")
    operator = await world.deliver(message(user(OPERATOR), mid=10201, mentions=mention(), **thread))
    member = await world.deliver(message(user(MEMBER), mid=10202, mentions=mention(), **thread))
    assert operator["session"] == f"thread:{CHANNEL}"
    assert member["session"] == f"group:{CHANNEL}:{MEMBER}"  # the thread seen as a plain channel
    assert member["tools"][0] and "BLOCKED" in member["tools"][0] and operator["tools"][0] is None


@pytest.mark.parametrize("first", ["operator", "public"])
async def test_shared_session_configuration_closes_public_admission(world, anchor, first):
    """group_sessions_per_user: false puts every member of a channel into one Hermes session; Hermes then
    batches and steers text across authors before any wrapper sees it. Public admission stays closed."""
    world.rebuild(group_sessions_per_user=False)
    order = [(OPERATOR, 10301), (MEMBER, 10302)] if first == "operator" else [(MEMBER, 10302), (OPERATOR, 10301)]
    results = {uid: await world.deliver(message(user(uid), mid=mid, mentions=mention())) for uid, mid in order}
    assert isinstance(results[OPERATOR], dict) and results[MEMBER] is None
    assert "group_sessions_per_user" in anchor.RUNTIME.status.get("A4").reason


def test_session_guard_is_a_backstop_for_shared_keys(world, anchor):
    authority, routes = anchor.mod("policy.authority"), anchor.routes
    home = anchor.mod("hermes").home_key(world.owner)
    runner = SimpleNamespace(_session_key_for_source=lambda source: "group:shared")
    event = SimpleNamespace(get_command=lambda: None, source=None)
    operator = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="1", author_id=OPERATOR,
                            lane="operator", target_home_key=home)
    public = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="2", author_id=MEMBER,
                          lane="public", target_home_key=home)
    assert authority.refusal(runner, event, operator, home) is None
    assert "never share a session" in authority.refusal(runner, event, public, home)


async def test_unverified_reply_of_an_allowlisted_member_still_runs_under_the_public_lane(world):
    world.rebuild(stock_allowed={MEMBER}, free_channels={CHANNEL})

    async def fetch_other(mid):
        return SimpleNamespace(author=SimpleNamespace(id=int(OPERATOR)))

    reply = message(user(MEMBER), mid=10451, reference=SimpleNamespace(message_id=66, resolved=None))
    reply.channel.fetch_message = fetch_other
    turn = await world.deliver(reply)
    assert isinstance(turn, dict) and turn["tools"][0] and "BLOCKED" in turn["tools"][0]


async def test_public_thread_message_refused_when_the_thread_routes_elsewhere(world, anchor):
    support = anchor.tmp / "support-profile"
    world.runner._resolve_profile_home_for_source = lambda source: support if source.thread_id else world.owner
    msg = message(user(MEMBER), mid=10461, mentions=mention(), channel_type=11, parent_id="900000000000000299")
    assert await world.deliver(msg) is None
    assert "routes to another profile" in anchor.RUNTIME.status.get("A4").reason


async def test_path_like_text_is_conversation_not_a_command(world):
    msg = message(user(MEMBER), mid=10471, mentions=mention(), content=f"<@{BOT_USER}> /home/me/app.log is broken")
    turn = await world.deliver(msg)
    assert isinstance(turn, dict)


async def test_excluded_channel_leaves_the_operator_with_stock_behaviour(world):
    world.rebuild(stock_allowed={OPERATOR})
    world.configure(conversation__excluded_channels=[CHANNEL])
    turn = await world.deliver(message(user(OPERATOR), mid=10481, mentions=mention()))
    assert isinstance(turn, dict) and turn["context"] == []  # stock turn: no policy lane at all


async def test_role_authorization_never_rides_on_a_widened_admission(world):
    world.rebuild(allowed_roles={"900000000000000901"})
    msg = message(user(MEMBER), mid=10401, mentions=mention())
    assert world.adapter._discord_message_admission(msg, claim=True) == (True, False)

    async def fetch_other(mid):
        return SimpleNamespace(author=SimpleNamespace(id=int(OPERATOR)))

    # A reply that turns out not to address the bot: dropped, never handed on with role authorization.
    reply = message(user(MEMBER), mid=10402, reference=SimpleNamespace(message_id=55, resolved=None),
                    channel_type=11)
    reply.channel.fetch_message = fetch_other
    assert await world.deliver(reply) is None
    assert world.runner.turns == []


async def test_allowlisted_non_operator_gets_the_public_lane_where_stock_answers_unaddressed(world):
    """Free-response channels and bot threads: stock answers allowlisted users without a mention; in an
    active guild that turn still runs under the author's lane (operator only from the configuration)."""
    world.rebuild(stock_allowed={MEMBER}, free_channels={CHANNEL})
    turn = await world.deliver(message(user(MEMBER), mid=10501, content="no mention here"))
    assert isinstance(turn, dict) and turn["tools"][0] and "BLOCKED" in turn["tools"][0]
    assert await world.deliver(message(user(MEMBER), mid=10502, channel=OTHER_CHANNEL)) is None  # stock: ignored


async def test_excluded_channel_admits_no_public_member(world):
    world.rebuild(stock_allowed={MEMBER})
    world.configure(conversation__excluded_channels=[CHANNEL])
    assert await world.deliver(message(user(MEMBER), mid=10601, mentions=mention())) is None


async def test_queued_operator_route_loses_authority_when_the_operator_is_removed(world, anchor):
    turn = await world.deliver(message(user(OPERATOR), mid=10701, mentions=mention()))
    assert turn["tools"][0] is None
    world.settings(world.owner, operators=[])
    restored = SimpleNamespace(source=turn["event"].source, raw_message=None, message_id="10701")
    assert await world.runner._handle_message(restored) is None  # dropped, not run as operator


def test_sources_without_message_id_are_never_authorized(world, anchor):
    authority, routes, runtime = anchor.mod("policy.authority"), anchor.routes, anchor.RUNTIME
    route = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="10801", author_id=MEMBER,
                         lane="public", target_home_key=anchor.mod("hermes").home_key(world.owner))
    token = runtime.route.set(route)
    try:
        assert not authority.authorizes(SimpleNamespace(platform="discord", user_id=MEMBER, chat_id=CHANNEL))
    finally:
        runtime.route.reset(token)


async def test_one_route_object_from_admission_to_the_tool_hook(world, anchor):
    """Delivery reads route state (tool_started) from the registry: it must be the very object the tool
    hook updated through the ContextVar and the turn binding."""
    runtime = anchor.RUNTIME
    turn = await world.deliver(message(user(OPERATOR), mid=10901, mentions=mention()))
    stored = runtime.message_routes.get((CHANNEL, "10901"))
    bound = runtime.turn_routes.get((turn["session"], turn["turn"]))
    assert stored is bound and stored is turn["event"].raw_message._dsp_route
    assert stored.tool_started is True  # set by pre_tool_call on the operator's allowed tool


async def test_public_messages_in_the_worklog_channel_and_its_threads_are_not_admitted(world):
    world.configure(worklog__channel=CHANNEL)
    assert await world.deliver(message(user(MEMBER), mid=10911, mentions=mention())) is None
    thread = dict(channel="900000000000000777", channel_type=11, parent_id=CHANNEL)
    assert await world.deliver(message(user(MEMBER), mid=10912, mentions=mention(), **thread)) is None
    assert isinstance(await world.deliver(message(user(OPERATOR), mid=10913, mentions=mention())), dict)


def test_routed_message_keeps_the_real_message_methods(anchor):
    admission, routes = anchor.mod("ingress.admission"), anchor.routes
    calls = []

    class Original:
        id, content, mentions, channel, author = 44, "hi", [], SimpleNamespace(id=int(CHANNEL)), user(MEMBER)

        async def add_reaction(self, emoji):
            calls.append(emoji)

    original = Original()
    route = routes.Route(bot_id=APP_ID, guild_id=GUILD, channel_id=CHANNEL, message_id="44", author_id=MEMBER,
                         lane="public")
    clone = admission.routed_message(original, route, self_user=None, message_type="reply")
    import asyncio

    asyncio.run(clone.add_reaction("👀"))
    assert calls == ["👀"]
