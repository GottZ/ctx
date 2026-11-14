"""S12: plugin entries in Hermes' route table (pure functions) and the store half of routing."""

from __future__ import annotations

import json
import os

import pytest

BOT, GUILD, OTHER = "900000000000000300", "900000000000000100", "900000000000000110"
OP = "900000000000000400"
NAME = f"ctx_discord_policy:{BOT}:{GUILD}"


@pytest.fixture
def table(anchor):
    return anchor.mod("routing.table")


def test_entry_name_and_shape(table):
    assert table.entry_name(BOT, GUILD) == NAME
    assert table.make_entry(BOT, GUILD, "beta", "default") == {"name": NAME, "platform": "discord", "guild_id": GUILD,
                                                               "profile": "beta"}
    assert table.make_entry(BOT, GUILD, "beta", "helper")["bot_profile"] == "helper"
    assert table.is_plugin_entry({"name": NAME}) and not table.is_plugin_entry({"name": "lounge-guild"})


def test_with_profile_touches_only_the_own_entry(table):
    core = [{"name": "core-other", "platform": "discord", "guild_id": OTHER, "profile": "beta"},
            {"name": "core-chan", "platform": "discord", "guild_id": GUILD, "chat_id": "5", "profile": "ops"},
            {"name": f"ctx_discord_policy:{BOT}:{OTHER}", "platform": "discord", "guild_id": OTHER,
             "profile": "beta"}]
    added = table.with_profile(core, BOT, GUILD, "beta", "default")
    assert added[:3] == core and added[3]["name"] == NAME
    replaced = table.with_profile(added, BOT, GUILD, "gamma", "default")
    assert replaced[:3] == core and replaced[3]["profile"] == "gamma" and len(replaced) == 4
    removed = table.with_profile(replaced, BOT, GUILD, None, "default")
    assert removed == core                                  # by name: core routes to the same profile stay


def test_core_routes_of_the_server(table):
    entries = [{"name": "g", "platform": "discord", "guild_id": GUILD, "profile": "x"},
               {"name": "g-other-bot", "platform": "discord", "guild_id": GUILD, "profile": "x", "bot_profile": "h"},
               {"name": "c", "platform": "discord", "guild_id": GUILD, "chat_id": "5", "profile": "y"},
               {"name": "off", "platform": "discord", "guild_id": GUILD, "profile": "z", "enabled": False},
               {"name": NAME, "platform": "discord", "guild_id": GUILD, "profile": "w"}]
    assert [e["name"] for e in table.core_guild_entries(entries, GUILD, "default")] == ["g"]
    assert [e["name"] for e in table.core_guild_entries(entries, GUILD, "h")] == ["g-other-bot"]
    assert [e["name"] for e in table.core_exceptions(entries, GUILD, "default")] == ["c"]


def test_adoption_renames_only(table):
    entries = [{"name": "g", "platform": "discord", "guild_id": GUILD, "profile": "lounge"},
               {"name": "other", "platform": "discord", "guild_id": OTHER, "profile": "lounge"}]
    out, profile = table.adopted(entries, "g", BOT, GUILD, "default")
    assert profile == "lounge"
    assert out == [{"name": NAME, "platform": "discord", "guild_id": GUILD, "profile": "lounge"}, entries[1]]
    with pytest.raises(LookupError):
        table.adopted(entries, "other", BOT, GUILD, "default")


def _load_yaml(text):
    try:
        import yaml  # PyYAML (test dependency)

        return yaml.safe_load(text)
    except ImportError:  # Hermes' own environment ships ruamel.yaml
        from ruamel.yaml import YAML

        return YAML(typ="safe").load(text)


def test_snippet_is_yaml(table):
    snippet = table.snippet_yaml(table.make_entry(BOT, GUILD, "beta", "helper"))
    assert _load_yaml(snippet) == {"profile_routes": [{"name": NAME, "platform": "discord", "guild_id": GUILD,
                                                           "profile": "beta", "bot_profile": "helper"}]}


# ── store: routing fields ────────────────────────────────────────────────────────────────────────────


@pytest.fixture
def store(anchor):
    store = anchor.mod("cli").store_for(anchor.home)
    anchor.mod("store.ops").bind(store, BOT, GUILD, by="cli")
    return store


def test_set_routing_records_and_resets_models(anchor, store):
    ops = anchor.mod("store.ops")
    ops.set_models(store, BOT, GUILD, {"models.public": {"provider": "deepseek", "model": "deepseek-chat"}}, by=OP)
    calls = []
    snap = ops.set_routing(store, BOT, GUILD, "beta", by=OP, expected_rev=2,
                           effects=lambda guild, profile: calls.append((guild["rev"], profile)))
    guild = snap.guild(BOT, GUILD)
    assert calls == [(2, "beta")]
    assert guild["routing"]["profile"] == "beta" and guild["routing"]["set_by"] == OP and guild["routing"]["set_at"]
    assert guild["models"]["public"] is None and guild["rev"] == 3
    fields = [a["field"] for a in snap.data["audit"][-2:]]
    assert fields == ["routing.profile", "models"]
    assert "beta" not in json.dumps(snap.data["audit"])     # audit keeps hashes only


def test_adoption_keeps_models(anchor, store):
    ops = anchor.mod("store.ops")
    choice = {"provider": "deepseek", "model": "deepseek-chat"}
    ops.set_models(store, BOT, GUILD, {"models.public": choice}, by=OP)
    snap = ops.adopt_routing(store, BOT, GUILD, "lounge", by=OP)
    assert snap.guild(BOT, GUILD)["models"]["public"] == choice


def test_failed_effects_write_nothing_and_stale_panels_cause_no_effects(anchor, store):
    ops, file = anchor.mod("store.ops"), anchor.mod("store.file")
    before = store.path.read_text()

    def broken(guild, profile):
        raise OSError("config.yaml not writable")

    with pytest.raises(OSError):
        ops.set_routing(store, BOT, GUILD, "beta", by=OP, effects=broken)
    assert store.path.read_text() == before
    calls = []
    with pytest.raises(file.ConflictError):
        ops.set_routing(store, BOT, GUILD, "beta", by=OP, expected_rev=0, effects=lambda g, p: calls.append(p))
    assert calls == [] and store.path.read_text() == before


@pytest.mark.parametrize("value", ["Beta", "../evil", "a" * 65, "", 7])
def test_routing_profile_is_a_profile_id(anchor, store, value):
    ops, schema = anchor.mod("store.ops"), anchor.mod("store.schema")
    with pytest.raises(schema.SchemaError):
        ops.set_routing(store, BOT, GUILD, value, by=OP)


def test_update_fields_refuses_routing_and_model_paths(anchor, store):
    ops, schema = anchor.mod("store.ops"), anchor.mod("store.schema")
    for path, value in (("routing.profile", "beta"), ("models.public", None), ("models.manager_pick", None),
                        ("models.public_choices", []), ("models.on_unavailable", "close")):
        with pytest.raises(schema.SchemaError, match="not a setting"):
            ops.update_fields(store, BOT, GUILD, {path: value}, by=OP)


def test_hand_edited_routing_makes_the_store_corrupt(anchor, store):
    doc = json.loads(store.path.read_text())
    doc["bots"][BOT]["guilds"][GUILD]["routing"]["profile"] = "../../etc"
    store.path.write_text(json.dumps(doc))
    os.utime(store.path, ns=(1, 1))
    assert store.read().state == "corrupt" and store.read().guild_state(BOT, GUILD) == "paused"


def test_audit_origin_is_recorded(anchor, store):
    ops = anchor.mod("store.ops")
    ops.set_routing(store, BOT, GUILD, "beta", by="dashboard", via="dashboard")
    assert store.read().data["audit"][-1]["via"] == "dashboard"
    with ops.origin("discord"):
        ops.set_routing(store, BOT, GUILD, None, by=OP)
    assert store.read().data["audit"][-1]["via"] == "discord"


# ── locked profiles (Hermes configuration of the default root) ────────────────────────────────────────


def test_locked_profiles_reading(anchor):
    config = anchor.mod("config")
    values = {}
    config.set_reader(lambda home, key, default: values.get(key, default))
    try:
        assert config.locked_profiles(anchor.home) == frozenset()
        values["locked_profiles"] = ["Default", " community ", "../x", 7]
        assert config.locked_profiles(anchor.home) == frozenset({"default", "community"})
        values["locked_profiles"] = "default"
        assert config.locked_profiles(anchor.home) is None      # not a list: everything counts as locked

        def broken(home, key, default):
            raise RuntimeError("config.yaml could not be read")

        config.set_reader(broken)
        assert config.locked_profiles(anchor.home) is None
    finally:
        config.set_reader(None)
