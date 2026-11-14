"""S12 contract: the Hermes profile per server against real Hermes (PROFILE-PER-GUILD §7.2).

Real ``hermes_cli.config`` (read_raw_config / save_config), real ``gateway.profile_routing`` parser and
matcher, real ``GatewayRunner._profile_name_for_source`` / ``_resolve_profile_home_for_source``, real
``DiscordAdapter.build_source``, the ingress re-check ``_runtime_profile`` where the ref has it (v0.23.0+),
real plugin managers in every profile home (armed register, live hook registry). Writes
``routing-<ref>.json``.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, load_in

GUILD = "900000000000000100"
OTHER_GUILD = "900000000000000110"
CHANNEL = "900000000000000200"
BOT = "900000000000000300"
OPERATOR = "900000000000000400"
MEMBER = "900000000000000500"

ROOT_CONFIG = """# operator comment that must survive a plugin write
multiplex_profiles: true
model:
  default: m-root  # inline comment
profile_routes:
  - name: core-other   # core route comment
    platform: discord
    guild_id: '{other}'
    profile: beta
{extra}plugins:
  enabled: [{plugin}]
  entries:
    {plugin}:
      settings:
        operators: ['{operator}']
        locked_profiles: [{locked}]
"""


def mod(name):
    return sys.modules[f"{ANCHOR}.{name}"]


def write_root(homes, *, extra="", locked=""):
    text = ROOT_CONFIG.format(other=OTHER_GUILD, extra=extra, plugin=PLUGIN, operator=OPERATOR, locked=locked)
    (homes.root / "config.yaml").write_text(text, encoding="utf-8")


@pytest.fixture
def world(hermes, tmp_path):
    _evict(ANCHOR)
    os.environ["HERMES_HOME"] = str(tmp_path / "root")
    homes = Homes(tmp_path)
    write_root(homes)
    for name in ("beta", "gamma", "locked"):
        homes.profile(name)
    homes.profile("bare", enabled=False)
    from gateway.config import Platform, load_gateway_config
    from gateway.platforms.helpers import MessageDeduplicator
    from gateway.run import GatewayRunner
    from plugins.platforms.discord.adapter import DiscordAdapter

    saved = {(cls, name): cls.__dict__.get(name) for cls in (GatewayRunner, DiscordAdapter)
             for name in ("_handle_message", "_is_user_authorized", "_discord_message_admission")}
    for home in (homes.root, *(homes.root / "profiles" / n for n in ("beta", "gamma", "locked", "bare"))):
        load_in(home)
    runner = GatewayRunner.__new__(GatewayRunner)
    runner.config = load_gateway_config()
    adapter = DiscordAdapter.__new__(DiscordAdapter)
    adapter.platform = Platform.DISCORD
    adapter.config = None
    adapter._owner_profile = None
    adapter.gateway_runner = runner
    adapter._dedup = MessageDeduplicator()
    adapter._client = SimpleNamespace(user=SimpleNamespace(id=int(BOT), bot=True), application_id=int(BOT))
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    token = set_hermes_home_override(str(homes.root))
    try:
        mod("adapt.interventions").discord_platform_handler(SimpleNamespace(application_id=int(BOT)), adapter)
    finally:
        reset_hermes_home_override(token)
    runtime = mod("runtime").RUNTIME
    binding = runtime.bots.for_bot(BOT)
    mod("store.ops").bind(binding.store, BOT, GUILD, by="cli")
    yield SimpleNamespace(homes=homes, runner=runner, adapter=adapter, binding=binding, runtime=runtime,
                          GatewayRunner=GatewayRunner)
    for (cls, name), value in saved.items():
        if value is None:
            if name in cls.__dict__:
                delattr(cls, name)
        else:
            setattr(cls, name, value)
    _evict(ANCHOR)


def source(guild=GUILD, chat=CHANNEL, user=MEMBER, profile=None):
    from gateway.config import Platform
    from gateway.session import SessionSource

    return SessionSource(platform=Platform.DISCORD, chat_id=chat, chat_type="group", user_id=user, guild_id=guild,
                         scope_id=guild, profile=profile)


def has_ingress_recheck() -> bool:
    try:
        from gateway.session_ingress_context import _runtime_profile  # noqa: F401
    except ImportError:
        return False
    return True


def runtime_profile(world, src):
    """Hermes' ingress re-check (``_runtime_profile``, v0.23.0+); None where the ref has it inline."""
    if not has_ingress_recheck():
        return None
    from gateway.session_ingress_context import _runtime_profile

    homes = {"default": world.homes.root, **{n: world.homes.root / "profiles" / n for n in ("beta", "gamma")}}
    return _runtime_profile(world.runner, src, None, "default", homes, False)


def set_profile(world, profile, **kw):
    return mod("routing.apply").set_profile(world.binding.store, BOT, GUILD, profile, owner_home=world.homes.root,
                                            by=OPERATOR, adapter=world.adapter, **kw)


def config_text(world):
    return (world.homes.root / "config.yaml").read_text(encoding="utf-8")


def test_saved_and_live_agree_with_every_hermes_reader(world, hermes):
    """Abnahme 1: entry written + running table replaced => resolver, home and ingress agree; YAML kept."""
    from gateway.config import load_gateway_config

    result = set_profile(world, "beta")
    assert (result.saved, result.live, result.strategy) == (True, True, "saved+live"), result
    text = config_text(world)
    assert "# operator comment that must survive" in text and "# inline comment" in text
    assert "# core route comment" in text and OPERATOR in text
    assert f"ctx_discord_policy:{BOT}:{GUILD}" in text
    nxt = [(r.name, r.guild_id, r.profile) for r in load_gateway_config().profile_routes]
    assert (f"ctx_discord_policy:{BOT}:{GUILD}", GUILD, "beta") in nxt and ("core-other", OTHER_GUILD, "beta") in nxt
    src = source()
    assert world.runner._profile_name_for_source(src) == "beta"
    assert Path(world.runner._resolve_profile_home_for_source(source(profile="beta"))) == world.homes.root / "profiles" / "beta"
    built = world.adapter.build_source(chat_id=CHANNEL, chat_type="group", user_id=MEMBER, guild_id=GUILD)
    assert built.profile == "beta"
    ingress = runtime_profile(world, built)
    assert ingress == ("beta" if has_ingress_recheck() else None)
    guild = world.binding.store.read().guild(BOT, GUILD)
    assert guild["routing"]["profile"] == "beta" and guild["routing"]["set_by"] == OPERATOR
    status = world.runtime.status.get("R1")
    hermes.write_report("routing", {"r1": status.as_dict(), "ingress_recheck": ingress,
                                    "live_routes": [r.name for r in world.runner.config.profile_routes]})
    assert status.outcome.value == "ok" and status.reason.startswith("saved+live")


def test_write_binds_the_default_root_under_a_profile_override(world):
    """M-P6: under a profile's home override ``save_config`` would write that profile's file."""
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    token = set_hermes_home_override(str(world.homes.root / "profiles" / "gamma"))
    try:
        result = set_profile(world, "beta")
    finally:
        reset_hermes_home_override(token)
    assert result.saved
    assert f"ctx_discord_policy:{BOT}:{GUILD}" in config_text(world)
    gamma = world.homes.root / "profiles" / "gamma" / "config.yaml"
    assert "profile_routes" not in gamma.read_text(encoding="utf-8")


def test_running_table_holds_hermes_route_objects(world):
    """M-P7: the running table is Hermes' parse, not raw entries (Hermes calls .matches/.specificity)."""
    from gateway.profile_routing import ProfileRoute

    set_profile(world, "beta")
    routes = world.runner.config.profile_routes
    assert routes and all(isinstance(r, ProfileRoute) for r in routes)
    assert [r.specificity for r in routes] == sorted((r.specificity for r in routes), reverse=True)


def test_switch_back_removes_only_the_plugin_entry(world):
    """M-P8: removal by the plugin's entry name (core routes to the same profile stay)."""
    set_profile(world, "beta")
    result = set_profile(world, None, expected_rev=world.binding.store.read().guild_rev(BOT, GUILD))
    assert result.saved and result.live
    text = config_text(world)
    assert f"ctx_discord_policy:{BOT}:{GUILD}" not in text and "core-other" in text
    assert world.runner._profile_name_for_source(source()) is None
    assert world.runner._profile_name_for_source(source(guild=OTHER_GUILD)) == "beta"


def test_take_over_a_hand_written_route_changes_only_its_name(world):
    """Abnahme 2 / M-P5: no second guild route of the same specificity; adoption = rename."""
    extra = f"  - name: core-guild\n    platform: discord\n    guild_id: '{GUILD}'\n    profile: gamma\n"
    write_root(world.homes, extra=extra)
    from gateway.config import load_gateway_config

    world.runner.config = load_gateway_config()
    apply = mod("routing.apply")
    before = [(r.platform, r.guild_id, r.chat_id, r.profile, r.bot_profile, r.specificity)
              for r in world.runner.config.profile_routes]
    with pytest.raises(apply.Refused) as refused:
        set_profile(world, "beta")
    assert refused.value.core_entry["name"] == "core-guild"
    assert f"ctx_discord_policy:{BOT}:" not in config_text(world)
    result = apply.adopt(world.binding.store, BOT, GUILD, "core-guild", owner_home=world.homes.root, by=OPERATOR,
                         adapter=world.adapter)
    assert result.saved and result.live and result.profile == "gamma"
    after = [(r.platform, r.guild_id, r.chat_id, r.profile, r.bot_profile, r.specificity)
             for r in world.runner.config.profile_routes]
    assert sorted(before, key=str) == sorted(after, key=str)
    names = [r.name for r in world.runner.config.profile_routes]
    assert "core-guild" not in names and f"ctx_discord_policy:{BOT}:{GUILD}" in names
    assert sorted(load_gateway_config().profile_routes, key=lambda r: r.name) and \
        [r.name for r in load_gateway_config().profile_routes].count(f"ctx_discord_policy:{BOT}:{GUILD}") == 1
    built = world.adapter.build_source(chat_id=CHANNEL, chat_type="group", user_id=MEMBER, guild_id=GUILD)
    assert built.profile == "gamma"
    assert runtime_profile(world, built) == ("gamma" if has_ingress_recheck() else None)  # no admission_conflict
    assert world.binding.store.read().guild(BOT, GUILD)["routing"]["profile"] == "gamma"


def test_duplicate_guild_route_is_what_hermes_rejects(world):
    """Why M-P5 matters: two guild routes of the same specificity raise admission_conflict (v0.23.0+)."""
    from gateway.profile_routing import parse_profile_routes

    try:
        from gateway.session_ingress_context import _runtime_profile  # noqa: F401
    except ImportError:
        pytest.skip("this ref re-checks inline")
    world.runner.config.profile_routes = parse_profile_routes([
        {"name": "core-guild", "platform": "discord", "guild_id": GUILD, "profile": "gamma"},
        {"name": f"ctx_discord_policy:{BOT}:{GUILD}", "platform": "discord", "guild_id": GUILD, "profile": "beta"}])
    with pytest.raises(Exception, match="admission_conflict"):
        runtime_profile(world, source(profile="gamma"))


def test_unarmed_locked_and_own_bot_profiles_are_not_offered(world):
    """Abnahme 3/4: eligibility with real profiles (list_profiles, served set, armed register, lock list)."""
    write_root(world.homes, locked="locked")
    world.runner._profile_bot_platforms = {}
    eligibility = mod("routing.eligibility")
    found, why = eligibility.candidates(world.runner, world.homes.root)
    assert why is None
    verdict = {c.name: (c.eligible, c.reason) for c in found}
    assert verdict["beta"][0] and verdict["gamma"][0]
    assert not verdict["bare"][0] and "not enabled" in verdict["bare"][1]
    assert not verdict["locked"][0] and "locked" in verdict["locked"][1]
    assert "default" not in verdict                       # the bot's own profile is "no route", not a target
    from gateway.config import Platform

    world.runner._profile_bot_platforms = {"gamma": {Platform.DISCORD}}
    assert not eligibility.check("gamma", world.runner, world.homes.root).eligible
    with pytest.raises(mod("routing.apply").Refused):
        set_profile(world, "bare")
    with pytest.raises(mod("routing.apply").Refused):
        set_profile(world, "locked")


def _event(src, mid="7001"):
    return SimpleNamespace(source=src, raw_message=None, message_id=mid)


def _public_route(world, target_home):
    routes = mod("policy.routes")
    return routes.Route(bot_id=BOT, guild_id=GUILD, channel_id=CHANNEL, message_id="7001", author_id=MEMBER,
                        lane="public", target_home_key=mod("hermes").home_key(target_home))


def _a4p(world, src):
    target = mod("policy.authority").event_target_key(world.runner, _event(src))
    route = _public_route(world, world.runner._resolve_profile_home_for_source(src))
    return mod("routing.check").refusal(world.runner, _event(src), route, target)


def test_a4p_public_turn_runs_only_where_the_operator_chose(world):
    """Abnahme 4/5/7 with real sources: chosen+live passes; pending, locked, switched and stale close."""
    build = lambda: world.adapter.build_source(chat_id=CHANNEL, chat_type="group", user_id=MEMBER,  # noqa: E731
                                               guild_id=GUILD)
    assert _a4p(world, build()) is None                    # nothing chosen: bot's own profile, unlocked
    set_profile(world, "beta")
    before_switch = build()
    assert before_switch.profile == "beta" and _a4p(world, before_switch) is None
    # Saved but not live (e.g. the running table could not be replaced): closed for members.
    from gateway.profile_routing import parse_profile_routes

    live = world.runner.config.profile_routes
    world.runner.config.profile_routes = parse_profile_routes([{"name": "core-other", "platform": "discord",
                                                                "guild_id": OTHER_GUILD, "profile": "beta"}])
    assert "does not route" in _a4p(world, build())
    world.runner.config.profile_routes = live
    # Switch to gamma: a message routed before the switch (stamped beta) is dropped.
    set_profile(world, "gamma", expected_rev=world.binding.store.read().guild_rev(BOT, GUILD))
    assert "before the server's profile changed" in _a4p(world, before_switch)
    assert _a4p(world, build()) is None
    # Locked afterwards: closed, also when the bot's own profile is the target.
    write_root(world.homes, locked="gamma")
    assert "locked" in _a4p(world, build())
    set_profile(world, None, expected_rev=world.binding.store.read().guild_rev(BOT, GUILD))
    write_root(world.homes, locked="default")
    assert "locked" in _a4p(world, build())


def test_managed_configuration_is_live_only_and_never_written(world, monkeypatch):
    """Abnahme 6: ``is_managed`` => strategy 2, YAML snippet, no write attempt."""
    from hermes_cli import config as hc

    monkeypatch.setattr(hc, "is_managed", lambda: True)
    before = config_text(world)
    result = set_profile(world, "beta")
    assert (result.saved, result.live, result.strategy) == (False, True, "live only")
    assert f"ctx_discord_policy:{BOT}:{GUILD}" in result.snippet and "profile: \"beta\"" in result.snippet
    assert config_text(world) == before
    assert world.runner._profile_name_for_source(source()) == "beta"


def test_resolver_wrap_is_not_a_route(world):
    """M-P11 / way b1: only a route in the running table keeps Hermes' ingress re-check consistent."""
    try:
        from gateway.session_ingress_context import _runtime_profile  # noqa: F401
    except ImportError:
        pytest.skip("this ref re-checks inline")
    set_profile(world, "beta")
    assert runtime_profile(world, source(profile="beta")) == "beta"


def test_a4p_at_admission_with_the_real_adapter_source(world):
    """Review finding 1: the admission check builds the member's source with Hermes' own build_source."""
    from gateway.profile_routing import parse_profile_routes

    check = mod("routing.check")
    hermes_mod = mod("hermes")

    def admit():
        src = world.adapter.build_source(chat_id=CHANNEL, chat_type="group", user_id=MEMBER, guild_id=GUILD)
        target = hermes_mod.home_key(world.runner._resolve_profile_home_for_source(src))
        return check.admission_refusal(world.adapter, bot_id=BOT, guild_id=GUILD, channel_id=CHANNEL,
                                       author_id=MEMBER, message_id="7101", target=target)

    set_profile(world, "beta")
    assert admit() is None
    live = world.runner.config.profile_routes
    world.runner.config.profile_routes = parse_profile_routes([])         # saved, not live
    assert "does not route" in admit()
    world.runner.config.profile_routes = live
    write_root(world.homes, locked="beta")
    assert "locked" in admit()
