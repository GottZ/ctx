"""S12: eligibility and the "set profile" flow (configuration + running table + store), R1 status."""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from fakes.ctx import FakeCtx
from unit.routing_fakes import FakeAdapter, FakeHermes, FakeRunner, arm

BOT, GUILD, OTHER = "900000000000000300", "900000000000000100", "900000000000000110"
OP = "900000000000000400"
NAME = f"ctx_discord_policy:{BOT}:{GUILD}"


class World(SimpleNamespace):
    def set(self, profile, **kw):
        return self.apply.set_profile(self.store, BOT, GUILD, profile, owner_home=self.root, by=OP,
                                      adapter=self.adapter, **kw)

    def rev(self) -> int:
        return self.store.read().guild_rev(BOT, GUILD)


@pytest.fixture
def world(anchor, monkeypatch):
    root = anchor.home
    anchor.entry.register(FakeCtx("default"))
    fake = FakeHermes(root).install(monkeypatch, anchor.mod("hermes"))
    for name in ("beta", "gamma", "helper"):
        arm(anchor, fake.add_profile(name, display_name=f"{name.title()} bot"), name)
    fake.add_profile("bare")                                           # plugin not enabled there
    fake.add_profile("parked")
    (root / "profiles" / "parked" / "gateway.parked").write_text("x")
    fake.served = {"beta", "gamma", "helper", "bare"}
    settings = {"operators": [OP], "locked_profiles": []}
    anchor.mod("config").set_reader(lambda home, key, default: settings.get(key, default))
    fake.config_entries = [{"name": "core-other", "platform": "discord", "guild_id": OTHER, "profile": "beta"}]
    runner = FakeRunner(root, fake.config_entries)
    store = anchor.mod("cli").store_for(root)
    anchor.mod("store.ops").bind(store, BOT, GUILD, by="cli")
    w = World(anchor=anchor, root=root, fake=fake, runner=runner, adapter=FakeAdapter(runner), store=store,
              settings=settings, apply=anchor.mod("routing.apply"), eligibility=anchor.mod("routing.eligibility"))
    yield w
    anchor.mod("config").set_reader(None)


def test_candidates_with_reasons_and_next_steps(world):
    world.settings["locked_profiles"] = ["gamma"]
    world.runner._profile_bot_platforms = {"helper": {"discord"}}
    found, why = world.eligibility.candidates(world.runner, world.root)
    assert why is None
    verdict = {c.name: c for c in found}
    assert set(verdict) == {"beta", "gamma", "helper", "bare", "parked"}   # the bot's own profile is no target
    assert verdict["beta"].eligible and verdict["beta"].label == "beta — Beta bot"
    assert "locked" in verdict["gamma"].reason
    assert "own Discord bot" in verdict["helper"].reason and "invite" in verdict["helper"].next_step
    assert "not enabled" in verdict["bare"].reason and "plugins enable" in verdict["bare"].next_step
    assert "parked" in verdict["parked"].reason and "gateway start" in verdict["parked"].next_step
    assert [c.name for c in found][0] == "beta"                         # eligible first


def test_selection_is_unavailable_without_multiplexing(world):
    world.runner.config.multiplex_profiles = False
    found, why = world.eligibility.candidates(world.runner, world.root)
    assert found == [] and "multiplex_profiles true" in why
    with pytest.raises(world.apply.Refused, match="single profile"):
        world.set("beta")


def test_set_profile_saves_and_applies_live(world):
    result = world.set("beta")
    assert (result.saved, result.live, result.strategy) == (True, True, "saved+live")
    assert world.fake.config_entries[0]["name"] == "core-other"
    assert world.fake.config_entries[1] == {"name": NAME, "platform": "discord", "guild_id": GUILD, "profile": "beta"}
    assert [r.name for r in world.runner.config.profile_routes] == ["core-other", NAME]
    assert world.store.read().guild(BOT, GUILD)["routing"]["profile"] == "beta"
    assert world.adapter.build_source(chat_id="1", user_id="2", guild_id=GUILD).profile == "beta"
    r1 = world.anchor.RUNTIME.status.get("R1")
    assert r1.outcome.value == "ok" and r1.reason.startswith("saved+live · 1 servers routed")


def test_not_armed_profile_is_refused_and_nothing_written(world):
    """M-P2: eligibility includes the armed check."""
    with pytest.raises(world.apply.Refused, match="not enabled"):
        world.set("bare")
    assert world.fake.writes == 0 and world.store.read().guild(BOT, GUILD)["routing"]["profile"] is None


def test_profile_disarmed_after_the_choice_is_refused_on_the_next_change(world):
    world.set("beta")
    world.anchor.RUNTIME.armed.disarm(world.anchor.mod("hermes").home_key(world.root / "profiles" / "beta"))
    with pytest.raises(world.apply.Refused):
        world.set("beta", expected_rev=world.rev())


def test_hand_written_guild_route_blocks_a_second_entry(world):
    """M-P5: same specificity as ours: offer the take-over instead."""
    world.fake.config_entries.append({"name": "lounge-guild", "platform": "discord", "guild_id": GUILD,
                                      "profile": "gamma"})
    with pytest.raises(world.apply.Refused) as refused:
        world.set("beta")
    assert refused.value.core_entry["name"] == "lounge-guild" and world.fake.writes == 0
    world.runner.config.profile_routes = __import__("unit.routing_fakes", fromlist=["parse"]).parse(
        world.fake.config_entries)
    result = world.apply.adopt(world.store, BOT, GUILD, "lounge-guild", owner_home=world.root, by=OP,
                               adapter=world.adapter)
    assert result.profile == "gamma" and result.saved and result.live
    assert [e["name"] for e in world.fake.config_entries] == ["core-other", NAME]
    assert world.fake.config_entries[1]["profile"] == "gamma"
    assert world.store.read().guild(BOT, GUILD)["routing"]["profile"] == "gamma"


def test_configuration_write_failure_changes_nothing(world):
    world.fake.write_error = OSError("disk full")
    with pytest.raises(OSError):
        world.set("beta")
    assert world.store.read().guild(BOT, GUILD)["routing"]["profile"] is None
    assert [r.name for r in world.runner.config.profile_routes] == ["core-other"]


def test_running_table_not_replaceable_is_saved_pending(world):
    """Abnahme 5: probe red => saved, not live; R1 reports pending (A4-P keeps members closed)."""
    world.runner.config.profile_routes = tuple(world.runner.config.profile_routes)   # not the expected list
    result = world.set("beta")
    assert (result.saved, result.live, result.strategy) == (True, False, "saved")
    assert "not recognised" in result.reason
    assert world.store.read().guild(BOT, GUILD)["routing"]["profile"] == "beta"
    assert world.anchor.RUNTIME.status.get("R1").outcome.value == "off"


def test_verification_failure_is_reported(world, monkeypatch):
    monkeypatch.setattr(world.runner, "_resolve_profile_home_for_source", lambda source: world.root)
    result = world.set("beta")
    assert result.saved and not result.live and "another profile" in result.reason
    status = world.anchor.RUNTIME.status.get("R1")
    assert status.outcome.value == "ok"   # the table holds the entry; the per-message check decides


def test_managed_configuration_is_live_only(world):
    """Abnahme 6: no write attempt, a YAML snippet, live replacement."""
    world.fake.managed = "nixos"
    result = world.set("beta")
    assert (result.saved, result.live, result.strategy) == (False, True, "live only")
    assert world.fake.writes == 0 and NAME in result.snippet and "nixos" in result.reason
    assert world.anchor.RUNTIME.status.get("R1").reason.startswith("live only")


def test_switching_resets_models_and_marks_the_server(world):
    ops = world.anchor.mod("store.ops")
    ops.set_models(world.store, BOT, GUILD, {"models.public": {"provider": "deepseek", "model": "x"}}, by=OP)
    world.set("beta")
    assert world.store.read().guild(BOT, GUILD)["models"]["public"] is None
    check = world.anchor.mod("routing.check")
    assert (BOT, GUILD) in check.TOUCHED
    switched = check.SWITCHED.get((BOT, GUILD))
    assert switched is not None                       # members' turns admitted before this never start
    world.apply.reapply(world.store, BOT, GUILD, owner_home=world.root, adapter=world.adapter)
    assert check.SWITCHED.get((BOT, GUILD)) == switched   # applying the same profile again is no switch


def test_stale_panel_changes_neither_configuration_nor_table(world):
    file = world.anchor.mod("store.file")
    with pytest.raises(file.ConflictError):
        world.set("beta", expected_rev=0)
    assert world.fake.writes == 0 and [r.name for r in world.runner.config.profile_routes] == ["core-other"]


def test_back_to_the_bot_profile_removes_the_entry(world):
    world.set("beta")
    result = world.set(None, expected_rev=world.rev())
    assert result.saved and result.live
    assert [e["name"] for e in world.fake.config_entries] == ["core-other"]
    assert world.adapter.build_source(chat_id="1", user_id="2", guild_id=GUILD).profile is None


def test_locked_bot_profile_is_allowed_with_a_warning(world):
    world.set("beta")
    world.settings["locked_profiles"] = ["default"]
    result = world.set(None, expected_rev=world.rev())
    assert result.live and "locked" in result.warning


def test_without_a_gateway_the_choice_is_saved_for_the_next_start(world):
    """The settings page outside the gateway process: configuration + store, not live."""
    result = world.apply.set_profile(world.store, BOT, GUILD, "beta", owner_home=world.root, by="dashboard",
                                     via="dashboard", adapter=None)
    assert (result.saved, result.live, result.strategy) == (True, False, "saved")
    assert world.fake.config_entries[-1]["name"] == NAME
    with pytest.raises(world.apply.Refused, match="no such profile"):
        world.apply.set_profile(world.store, BOT, GUILD, "ghost", owner_home=world.root, by="dashboard",
                                adapter=None)


def test_describe_for_the_panel(world):
    view = world.apply.describe(world.store, BOT, GUILD, owner_home=world.root, adapter=world.adapter)
    assert (view.state, view.desired, view.owner_profile, view.armed, view.locked) == ("own", None, "default", True,
                                                                                         False)
    world.set("beta")
    view = world.apply.describe(world.store, BOT, GUILD, owner_home=world.root, adapter=world.adapter)
    assert (view.state, view.desired, view.live_profile) == ("live", "beta", "beta")
    world.runner.config.profile_routes = []
    assert world.apply.describe(world.store, BOT, GUILD, owner_home=world.root, adapter=world.adapter).state == "pending"


def test_reapply_uses_only_the_stored_choice(world):
    world.set("beta")
    world.runner.config.profile_routes = []
    result = world.apply.reapply(world.store, BOT, GUILD, owner_home=world.root, adapter=world.adapter)
    assert result.profile == "beta" and result.live
    assert NAME in [r.name for r in world.runner.config.profile_routes]


def test_on_bind_replays_choices_for_a_managed_configuration(world):
    world.fake.managed = "nixos"
    world.set("beta")
    world.runner.config.profile_routes = []                            # a restart: Hermes' own table only
    binding = SimpleNamespace(bot_id=BOT, owner_home=world.root, store=world.store)
    world.apply.on_bind(world.adapter, binding)
    assert NAME in [r.name for r in world.runner.config.profile_routes]
    assert world.anchor.RUNTIME.status.get("R1").outcome.value == "ok"


def test_on_bind_reports_disagreement(world):
    world.set("beta")
    world.runner.config.profile_routes = []                            # config.yaml lost the entry by hand
    binding = SimpleNamespace(bot_id=BOT, owner_home=world.root, store=world.store)
    world.apply.on_bind(world.adapter, binding)
    r1 = world.anchor.RUNTIME.status.get("R1")
    assert r1.outcome.value == "fallback" and "pending restart (1)" in r1.reason
    assert world.runner.config.profile_routes == []                    # never applied silently


def test_on_bind_after_a_managed_take_over_renames_instead_of_duplicating(world):
    """Review finding 3: with a managed configuration the take-over only renamed the running entry; after a
    restart Hermes' table has the hand-written name again. A second guild route of the same specificity would
    make Hermes refuse every message of the server (admission_conflict), the operator's too."""
    world.fake.managed = "nixos"
    core = {"name": "core-guild", "platform": "discord", "guild_id": GUILD, "profile": "gamma"}
    world.fake.config_entries.append(core)
    routes = __import__("unit.routing_fakes", fromlist=["parse"]).parse
    world.runner.config.profile_routes = routes(world.fake.config_entries)
    world.apply.adopt(world.store, BOT, GUILD, "core-guild", owner_home=world.root, by=OP, adapter=world.adapter)
    world.runner.config.profile_routes = routes(world.fake.config_entries)     # restart: YAML unchanged
    binding = SimpleNamespace(bot_id=BOT, owner_home=world.root, store=world.store)
    world.apply.on_bind(world.adapter, binding)
    guild_routes = [(r.name, r.profile) for r in world.runner.config.profile_routes if r.guild_id == GUILD]
    assert guild_routes == [(NAME, "gamma")]
    view = world.apply.describe(world.store, BOT, GUILD, owner_home=world.root, adapter=world.adapter)
    assert view.state == "live"


def test_on_bind_never_adds_a_second_guild_route_next_to_another_profile(world):
    world.fake.managed = "nixos"
    world.set("beta")
    core = {"name": "core-guild", "platform": "discord", "guild_id": GUILD, "profile": "gamma"}
    routes = __import__("unit.routing_fakes", fromlist=["parse"]).parse
    world.runner.config.profile_routes = routes(world.fake.config_entries + [core])   # restart, hand-written route
    binding = SimpleNamespace(bot_id=BOT, owner_home=world.root, store=world.store)
    world.apply.on_bind(world.adapter, binding)
    guild_routes = [(r.name, r.profile) for r in world.runner.config.profile_routes if r.guild_id == GUILD]
    assert guild_routes == [("core-guild", "gamma")]
    view = world.apply.describe(world.store, BOT, GUILD, owner_home=world.root, adapter=world.adapter)
    assert view.state == "conflict"


def test_describe_reports_a_conflict_when_both_entries_are_live(world):
    world.set("beta")
    core = {"name": "core-guild", "platform": "discord", "guild_id": GUILD, "profile": "beta"}
    routes = __import__("unit.routing_fakes", fromlist=["parse"]).parse
    world.runner.config.profile_routes = routes(world.fake.config_entries + [core])   # hand edit added a twin
    view = world.apply.describe(world.store, BOT, GUILD, owner_home=world.root, adapter=world.adapter)
    assert view.state == "conflict" and view.core_entry["name"] == "core-guild"


def test_two_bots_replacing_the_running_table_at_once_keep_both_entries(world):
    """Review finding 6: bots have separate stores (separate locks) but share the gateway's one table."""
    import threading
    import time

    routes = __import__("unit.routing_fakes", fromlist=["parse"]).parse
    world.runner.config.profile_routes = routes([])
    table = world.anchor.mod("routing.table")

    def slow(bot):
        def transform(entries):
            time.sleep(0.2)
            return table.with_profile(entries, bot, GUILD, "beta", "default")
        return transform

    threads = [threading.Thread(target=world.apply.replace_live, args=(world.runner, slow(b)))
               for b in ("900000000000000301", "900000000000000302")]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert sorted(r.name for r in world.runner.config.profile_routes) == [
        f"ctx_discord_policy:900000000000000301:{GUILD}", f"ctx_discord_policy:900000000000000302:{GUILD}"]
