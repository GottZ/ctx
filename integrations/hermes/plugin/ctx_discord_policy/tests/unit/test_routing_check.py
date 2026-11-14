"""S12: A4-P in the security core — public turns run only in the profile the operator chose.

End to end through the real core path (admission wrapper, stock-like handler, runner wrapper with
``authority.refusal``) like ``test_security_core``; the Hermes routing side is ``routing_fakes``.
"""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace

import pytest
from fakes.ctx import FakeCtx
from fakes.gateway import (APP_ID, BOT_USER, CHANNEL, GUILD, MEMBER, OPERATOR, HookBus, install_gateway_module,
                           make_classes, message, remove_gateway_module, user)
from unit.routing_fakes import FakeHermes, arm, match, parse

TERMINAL = ("terminal", {"command": "id"})
PLUGIN_ENTRY = {"name": f"ctx_discord_policy:{APP_ID}:{GUILD}", "platform": "discord", "guild_id": GUILD}


class World(SimpleNamespace):
    def settings(self, **values) -> None:
        path = self.owner / "settings.json"
        current = json.loads(path.read_text()) if path.exists() else {}
        current.update(values)
        path.write_text(json.dumps(current))

    def live(self, *entries) -> None:
        self.runner.config.profile_routes = parse(list(entries))

    def choose(self, profile) -> None:
        """What the panel does after a successful switch: store + live table (routing.apply, tested apart)."""
        before = (self.store.read().guild(APP_ID, GUILD).get("routing") or {}).get("profile")
        self.anchor.mod("store.ops").set_routing(self.store, APP_ID, GUILD, profile, by=OPERATOR)
        self.anchor.mod("routing.check").mark_touched(APP_ID, GUILD)
        if before != profile:
            self.anchor.mod("routing.check").mark_switched(APP_ID, GUILD)
        self.live(*([dict(PLUGIN_ENTRY, profile=profile)] if profile else []))

    async def deliver(self, msg):
        self.adapter.last_result = None
        admitted, role_authorized = self.adapter._discord_message_admission(msg, claim=True)
        if not admitted:
            return None
        await self.adapter._handle_message(msg, role_authorized)
        return self.adapter.last_result


@pytest.fixture
def world(anchor, monkeypatch):
    owner = anchor.tmp / "owner"
    owner.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(owner))
    ctx = FakeCtx("default")
    anchor.entry.register(ctx)
    fake = FakeHermes(owner).install(monkeypatch, anchor.mod("hermes"))
    for name in ("beta", "gamma"):
        arm(anchor, fake.add_profile(name), name)
    config = anchor.mod("config")

    def reader(home, key, default):
        path = Path(home) / "settings.json"
        return json.loads(path.read_text()).get(key, default) if path.exists() else default

    config.set_reader(reader)
    w = World(anchor=anchor, owner=owner, fake=fake)
    w.settings(operators=[OPERATOR])

    def resolve(source):
        name = getattr(source, "profile", None)
        return owner if not name or name == "default" else owner / "profiles" / name

    bus = HookBus(ctx.hooks)
    classes = make_classes(home_resolver=resolve, bus=bus)
    install_gateway_module(classes.module)
    w.classes, w.bus = classes, bus
    w.runner = classes.GatewayRunner()
    w.runner.config = SimpleNamespace(multiplex_profiles=True, profile_routes=[])
    w.adapter = classes.DiscordAdapter(w.runner)
    w.runner.plan = [TERMINAL]
    stock_build = w.adapter.build_source

    def build_source(*args, **kwargs):  # Hermes stamps the routed profile (multiplex only)
        src = stock_build(*args, **kwargs)
        cfg = w.runner.config
        hit = match(cfg.profile_routes or [], src, None) if cfg.multiplex_profiles else None
        src.profile = hit.profile if hit else None
        return src

    w.adapter.build_source = build_source
    anchor.mod("adapt.interventions").discord_platform_handler(SimpleNamespace(application_id=int(APP_ID)), w.adapter)
    w.binding = anchor.RUNTIME.bots.for_bot(APP_ID)
    w.store = w.binding.store
    anchor.mod("store.ops").bind(w.store, APP_ID, GUILD, by="cli")
    try:
        yield w
    finally:
        remove_gateway_module(classes.module)
        config.set_reader(None)


def mention():
    return [user(BOT_USER, bot=True)]


def ran(turn) -> bool:
    return isinstance(turn, dict)


async def test_member_turn_runs_in_the_chosen_live_profile(world):
    world.choose("beta")
    turn = await world.deliver(message(user(MEMBER), mid=11001, mentions=mention()))
    assert ran(turn) and turn["event"].source.profile == "beta"
    assert turn["tools"][0] and "BLOCKED" in turn["tools"][0]          # still the public lane


async def test_chosen_but_not_live_closes_for_members_only(world):
    """M-P3: a choice the running Hermes does not route (pending restart) admits no member."""
    world.choose("beta")
    world.live()                                                      # running table lost the entry
    assert await world.deliver(message(user(MEMBER), mid=11101, mentions=mention())) is None
    assert world.runner.turns == []
    a4p = world.anchor.RUNTIME.status.get("A4-P")
    assert a4p.outcome.value == "fallback" and "does not route" in a4p.reason
    operator = await world.deliver(message(user(OPERATOR), mid=11102, mentions=mention()))
    assert ran(operator) and operator["tools"][0] is None              # operator turns are not checked


async def test_locked_profile_is_never_public(world):
    """M-P4: the lock applies in the admission path, also without any plugin entry."""
    world.settings(locked_profiles=["default"])                        # the bot's own profile
    assert await world.deliver(message(user(MEMBER), mid=11201, mentions=mention())) is None
    world.settings(locked_profiles=["beta"])
    world.live(dict(PLUGIN_ENTRY, name="hand-written", profile="beta"))  # a hand-written route leads there
    assert await world.deliver(message(user(MEMBER), mid=11202, mentions=mention())) is None
    world.settings(locked_profiles=[])
    assert ran(await world.deliver(message(user(MEMBER), mid=11203, mentions=mention())))


async def test_unreadable_lock_list_closes(world):
    world.settings(locked_profiles="beta")                             # not a list
    assert await world.deliver(message(user(MEMBER), mid=11301, mentions=mention())) is None


async def test_messages_routed_before_a_switch_are_dropped(world):
    """§4.4: a queued public message keeps its stamped profile; after the switch it must not run there."""
    world.choose("beta")
    msg = message(user(MEMBER), mid=11401, mentions=mention())
    admitted, role = world.adapter._discord_message_admission(msg, claim=True)
    assert admitted
    queued = world.adapter.build_source(chat_id=CHANNEL, chat_type="group", user_id=MEMBER, guild_id=GUILD,
                                        message_id="11401")
    route = world.anchor.mod("ingress.admission").decide(world.adapter, msg).route("11401")
    world.anchor.RUNTIME.message_routes.put(route.key, route)
    world.choose("gamma")
    event = SimpleNamespace(source=queued, raw_message=None, message_id="11401")
    assert await world.runner._handle_message(event) is None            # dropped by the runner wrapper
    assert world.runner.turns == []
    assert ran(await world.deliver(message(user(MEMBER), mid=11402, mentions=mention())))   # new ones run


async def test_back_to_the_bot_profile_drops_what_was_routed_to_the_old_choice(world):
    world.choose("beta")
    queued = world.adapter.build_source(chat_id=CHANNEL, chat_type="group", user_id=MEMBER, guild_id=GUILD,
                                        message_id="11501")
    route = world.anchor.mod("ingress.admission").decide(
        world.adapter, message(user(MEMBER), mid=11501, mentions=mention())).route("11501")
    world.anchor.RUNTIME.message_routes.put(route.key, route)
    world.choose(None)
    assert await world.runner._handle_message(SimpleNamespace(source=queued, raw_message=None,
                                                              message_id="11501")) is None


async def test_stale_plugin_entry_without_a_choice_closes(world):
    world.live(dict(PLUGIN_ENTRY, profile="beta"))                       # e.g. config edited, store says none
    assert await world.deliver(message(user(MEMBER), mid=11601, mentions=mention())) is None


async def test_more_specific_hand_written_routes_still_apply(world):
    world.choose("beta")
    world.live(dict(PLUGIN_ENTRY, profile="beta"),
               {"name": "ops-channel", "platform": "discord", "guild_id": GUILD, "chat_id": CHANNEL, "profile": "gamma"})
    turn = await world.deliver(message(user(MEMBER), mid=11701, mentions=mention()))
    assert ran(turn) and turn["event"].source.profile == "gamma"


async def test_a_check_that_fails_internally_closes(world, monkeypatch):
    world.choose("beta")
    monkeypatch.setattr(world.anchor.mod("hermes"), "match_route",
                        lambda *a, **k: (_ for _ in ()).throw(RuntimeError("Hermes changed")))
    assert await world.deliver(message(user(MEMBER), mid=11801, mentions=mention())) is None
    assert "internally" in world.anchor.RUNTIME.status.get("A4-P").reason


async def test_no_choice_and_nothing_touched_is_stock_policy(world):
    """A server that never used profile selection is not affected (no false closures after updates)."""
    world.runner.config = SimpleNamespace(multiplex_profiles=False, profile_routes=None)
    assert ran(await world.deliver(message(user(MEMBER), mid=11901, mentions=mention())))


async def test_members_model_unavailable_with_close(world):
    """M-M7: ``on_unavailable: close`` closes members' turns while the model can't be applied (here: K8
    is not installed on this Hermes)."""
    ops = world.anchor.mod("store.ops")
    ops.set_models(world.store, APP_ID, GUILD, {"models.public": {"provider": "deepseek", "model": "deepseek-chat"}},
                   by=OPERATOR)
    assert ran(await world.deliver(message(user(MEMBER), mid=12001, mentions=mention())))   # rule: profile
    ops.set_models(world.store, APP_ID, GUILD, {"models.on_unavailable": "close"}, by=OPERATOR)
    assert await world.deliver(message(user(MEMBER), mid=12002, mentions=mention())) is None
    assert ran(await world.deliver(message(user(OPERATOR), mid=12003, mentions=mention())))


async def test_status_stays_closed_while_any_server_is_closed(world):
    """The A4-P status is per server: a member's turn passing in another server does not clear it."""
    other = "900000000000000110"
    world.anchor.mod("store.ops").bind(world.store, APP_ID, other, by="cli")
    world.choose("beta")
    world.live()                                                      # GUILD: chosen but not live
    assert await world.deliver(message(user(MEMBER), mid=12201, mentions=mention())) is None
    assert ran(await world.deliver(message(user(MEMBER), mid=12202, guild=other, mentions=mention())))
    a4p = world.anchor.RUNTIME.status.get("A4-P")
    assert a4p.outcome.value == "fallback" and GUILD in a4p.reason and a4p.reason.startswith("1 server closed")
    world.live(dict(PLUGIN_ENTRY, profile="beta"))
    assert ran(await world.deliver(message(user(MEMBER), mid=12203, mentions=mention())))
    assert world.anchor.RUNTIME.status.get("A4-P").outcome.value == "ok"


async def test_a_closed_server_admits_no_member_at_the_adapter(world):
    """Review finding 1: Hermes authorizes a follow-up to a busy session through the stored route only (the
    runner wrapper does not run for it), so A4-P also decides at admission: no route is stored to use."""
    world.choose("beta")
    world.live()                                                      # chosen but not live
    msg = message(user(MEMBER), mid=12301, mentions=mention())
    assert world.adapter._discord_message_admission(msg, claim=True)[0] is False
    world.live(dict(PLUGIN_ENTRY, profile="beta"))
    world.settings(locked_profiles=["beta"])                           # live, but locked
    msg = message(user(MEMBER), mid=12302, mentions=mention())
    assert world.adapter._discord_message_admission(msg, claim=True)[0] is False
    world.settings(locked_profiles=[])
    msg = message(user(MEMBER), mid=12303, mentions=mention())
    assert world.adapter._discord_message_admission(msg, claim=True)[0] is True
    operator = message(user(OPERATOR), mid=12304, mentions=mention())
    world.live()
    assert world.adapter._discord_message_admission(operator, claim=True)[0] is True   # operators unaffected


async def test_follow_ups_admitted_before_a_switch_never_start_in_the_old_profile(world):
    """Review finding 2: a queued follow-up runs inside the running turn (no runner wrapper, no new route),
    so the turn start (pre_llm_call) refuses public turns admitted before the server switched profiles."""
    hooks = world.anchor.mod("policy.hooks")
    world.choose("beta")
    route = world.anchor.mod("ingress.admission").decide(
        world.adapter, message(user(MEMBER), mid=12401, mentions=mention())).route("12401")
    token = world.anchor.RUNTIME.route.set(route)
    try:
        assert "context" in hooks.on_pre_llm_call(session_id="s-12401", turn_id="t1")
        world.choose("gamma")
        blocked = hooks.on_pre_llm_call(session_id="s-12401", turn_id="t2")
        assert blocked["action"] == "block" and "send it again" in blocked["message"]
        assert "beta" not in blocked["message"] and "gamma" not in blocked["message"]   # no profile names
    finally:
        world.anchor.RUNTIME.route.reset(token)
    fresh = world.anchor.mod("ingress.admission").decide(
        world.adapter, message(user(MEMBER), mid=12402, mentions=mention())).route("12402")
    operator = world.anchor.mod("ingress.admission").decide(
        world.adapter, message(user(OPERATOR), mid=12403, mentions=mention())).route("12403")
    operator.created_at = route.created_at                             # an operator turn is never refused
    for allowed in (fresh, operator):
        token = world.anchor.RUNTIME.route.set(allowed)
        try:
            assert "context" in hooks.on_pre_llm_call(session_id="s-x", turn_id="t3")
        finally:
            world.anchor.RUNTIME.route.reset(token)


async def test_a_turn_in_a_profile_locked_meanwhile_does_not_start(world, monkeypatch):
    hooks = world.anchor.mod("policy.hooks")
    world.choose("beta")
    route = world.anchor.mod("ingress.admission").decide(
        world.adapter, message(user(MEMBER), mid=12501, mentions=mention())).route("12501")
    monkeypatch.setattr(world.anchor.mod("hermes"), "current_home", lambda: world.owner / "profiles" / "beta")
    world.settings(locked_profiles=["beta"])
    token = world.anchor.RUNTIME.route.set(route)
    try:
        blocked = hooks.on_pre_llm_call(session_id="s-12501", turn_id="t1")
    finally:
        world.anchor.RUNTIME.route.reset(token)
    assert blocked["action"] == "block" and "beta" not in blocked["message"]


async def test_chosen_profile_whose_home_is_gone_closes(world, monkeypatch):
    """Review finding 5: Hermes falls back to the launch home for a route whose profile vanished."""
    world.choose("beta")
    stock = world.runner._resolve_profile_home_for_source
    monkeypatch.setattr(world.runner, "_resolve_profile_home_for_source",
                        lambda source: world.owner if getattr(source, "profile", None) == "beta" else stock(source))
    assert await world.deliver(message(user(MEMBER), mid=12601, mentions=mention())) is None
    assert "is gone" in world.anchor.RUNTIME.status.get("A4-P").reason


async def test_operator_lane_comes_from_the_bot_owner_home(world):
    """M-P10 / §4.6: the target profile's own operator list never decides the lane."""
    world.choose("beta")
    (world.owner / "profiles" / "beta" / "settings.json").write_text(json.dumps({"operators": [MEMBER]}))
    turn = await world.deliver(message(user(MEMBER), mid=12101, mentions=mention()))
    assert ran(turn) and "[Capability lane: public]" in turn["context"][0]["context"]
