"""S12: ``hermes ctx-discord-policy routing list|set|clear|adopt`` (PROFILE-PER-GUILD §6.4) — same checks as the
panel, run on the host without a gateway (the choice is saved and applies when the gateway re-reads it)."""

from __future__ import annotations

import argparse
import io
import json

import pytest
from fakes.ctx import FakeCtx
from unit.routing_fakes import FakeHermes, arm

BOT, GUILD, OTHER = "900000000000000300", "900000000000000100", "900000000000000110"
NAME = f"ctx_discord_policy:{BOT}:{GUILD}"


@pytest.fixture
def world(anchor, monkeypatch):
    root = anchor.home
    anchor.entry.register(FakeCtx("default"))
    fake = FakeHermes(root).install(monkeypatch, anchor.mod("hermes"))
    for name in ("beta", "gamma"):
        arm(anchor, fake.add_profile(name), name)
    settings = {"operators": ["900000000000000400"], "locked_profiles": ["gamma"]}
    anchor.mod("config").set_reader(lambda home, key, default: settings.get(key, default))
    store = anchor.mod("cli").store_for(root)
    anchor.mod("store.ops").bind(store, BOT, GUILD, by="cli")
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="dsp_command")
    cli = anchor.mod("routing.cli")
    cli.setup(sub)

    def run(*argv):
        out, err = io.StringIO(), io.StringIO()
        code = cli.run(parser.parse_args(["routing", *argv]), home=root, store=store, bot=BOT, out=out, err=err)
        return code, out.getvalue(), err.getvalue()

    yield type("W", (), {"run": staticmethod(run), "fake": fake, "store": store, "settings": settings})
    anchor.mod("config").set_reader(None)


def chosen(world):
    return world.store.read().guild(BOT, GUILD)["routing"]["profile"]


def test_set_saves_the_route_and_says_when_it_applies(world):
    code, out, err = world.run("set", GUILD, "beta")
    assert (code, err) == (0, "")
    assert world.fake.config_entries == [{"name": NAME, "platform": "discord", "guild_id": GUILD, "profile": "beta"}]
    assert chosen(world) == "beta"
    assert "saved in Hermes' configuration" in out and "restart" in out and "stay closed" in out


def test_set_refuses_what_the_panel_refuses(world):
    code, _, err = world.run("set", GUILD, "gamma")
    assert code == 1 and "locked" in err and "Next step" in err
    code, _, err = world.run("set", GUILD, "nosuch")
    assert code == 1 and "no such profile" in err
    code, _, err = world.run("set", GUILD, "Bad Name")
    assert code == 1
    assert world.fake.config_entries == [] and chosen(world) is None


def test_hand_written_route_is_taken_over_not_duplicated(world):
    world.fake.config_entries = [{"name": "core-guild", "platform": "discord", "guild_id": GUILD, "profile": "beta"}]
    code, _, err = world.run("set", GUILD, "beta")
    assert code == 1 and "core-guild" in err and f"routing adopt {GUILD}" in err
    assert [e["name"] for e in world.fake.config_entries] == ["core-guild"]
    code, out, err = world.run("adopt", GUILD)
    assert (code, err) == (0, ""), err
    assert world.fake.config_entries == [{"name": NAME, "platform": "discord", "guild_id": GUILD, "profile": "beta"}]
    assert chosen(world) == "beta" and "took over" in out


def test_adopt_without_a_hand_written_route(world):
    code, _, err = world.run("adopt", GUILD)
    assert code == 1 and "no hand-written route" in err


def test_adopt_names_the_entry_when_there_are_several(world):
    world.fake.config_entries = [
        {"name": "a", "platform": "discord", "guild_id": GUILD, "profile": "beta"},
        {"name": "b", "platform": "discord", "guild_id": GUILD, "profile": "beta"}]
    code, _, err = world.run("adopt", GUILD)
    assert code == 1 and "--entry" in err
    code, _, err = world.run("adopt", GUILD, "--entry", "b")
    assert code == 0, err
    assert [e["name"] for e in world.fake.config_entries] == ["a", NAME]


def test_clear_removes_only_the_plugin_entry(world):
    world.fake.config_entries = [{"name": "core-other", "platform": "discord", "guild_id": OTHER, "profile": "beta"}]
    assert world.run("set", GUILD, "beta")[0] == 0
    code, out, _ = world.run("clear", GUILD)
    assert code == 0 and "the bot's own profile" in out
    assert [e["name"] for e in world.fake.config_entries] == ["core-other"] and chosen(world) is None


def test_managed_configuration_prints_the_entry_to_add(world):
    world.fake.managed = "NixOS"
    code, out, _ = world.run("set", GUILD, "beta")
    assert code == 0 and world.fake.writes == 0
    assert "managed (NixOS)" in out and f'name: "{NAME}"' in out and chosen(world) == "beta"


def test_list_shows_choice_configuration_and_locks(world):
    world.fake.config_entries = [
        {"name": "ops", "platform": "discord", "guild_id": GUILD, "chat_id": "900000000000000200", "profile": "gamma"}]
    world.run("set", GUILD, "beta")
    code, out, _ = world.run("list")
    assert code == 0
    assert f"guild {GUILD}: beta (chosen here) · Hermes configuration: beta" in out
    assert "exception ops -> gamma (locked)" in out
    code, out, _ = world.run("list", "--json")
    rows = json.loads(out)["servers"]
    assert rows[0]["guild"] == GUILD and rows[0]["chosen"] == "beta" and rows[0]["configured"] == "beta"
    assert rows[0]["exceptions"] == [{"name": "ops", "profile": "gamma", "locked": True}]
