"""S2: operators from the Hermes configuration only; CLI on the real store write path."""

from __future__ import annotations

import argparse
import io
import json

import pytest

BOT = "900000000000000001"
GUILD = "900000000000000002"
OP = "900000000000000011"


@pytest.fixture
def cfg(anchor):
    config = anchor.mod("config")
    files = {}

    def reader(home, key, default):
        # Re-read on every call, like Hermes' signature-cached loader does for an edited config.yaml.
        path = files.get(str(home))
        if path is None or not path.exists():
            return default
        return json.loads(path.read_text()).get(key, default)

    config.set_reader(reader)
    yield config, files
    config.set_reader(None)


def test_operators_come_from_settings_and_changes_apply_without_restart(cfg, anchor):
    config, files = cfg
    home = anchor.tmp / "owner"
    home.mkdir()
    files[str(home)] = home / "settings.json"
    (home / "settings.json").write_text(json.dumps({"operators": [OP]}))
    assert config.operators(home) == {OP} and config.is_operator(home, OP)
    (home / "settings.json").write_text(json.dumps({"operators": [int("900000000000000012")]}))
    assert config.operators(home) == {"900000000000000012"}  # next decision, no restart
    assert not config.is_operator(home, OP)


@pytest.mark.parametrize("raw", ["900000000000000011", {"id": OP}, None, 42, [None, "abc", True, "0123"]])
def test_malformed_operator_settings_fail_closed(cfg, anchor, raw):
    config, files = cfg
    home = anchor.tmp / "owner"
    home.mkdir()
    files[str(home)] = home / "settings.json"
    (home / "settings.json").write_text(json.dumps({"operators": raw}))
    assert config.operators(home) == frozenset()


def test_reader_failure_means_no_operator(cfg, anchor):
    config, _ = cfg

    def broken(home, key, default):
        raise OSError("config unreadable")

    config.set_reader(broken)
    assert config.operators(anchor.tmp) == frozenset()
    assert config.setting(anchor.tmp, "guild_prompt_max_chars") == 6000


def test_operators_are_per_owner_home(cfg, anchor):
    config, files = cfg
    owner, other = anchor.tmp / "owner", anchor.tmp / "other"
    for home, ops in ((owner, [OP]), (other, ["900000000000000099"])):
        home.mkdir()
        files[str(home)] = home / "settings.json"
        (home / "settings.json").write_text(json.dumps({"operators": ops}))
    assert config.operators(owner) == {OP}
    assert config.operators(other) == {"900000000000000099"}


def _cli(anchor, home, *argv):
    cli = anchor.mod("cli")
    parser = argparse.ArgumentParser()
    cli.setup(parser)
    out, err = io.StringIO(), io.StringIO()
    code = cli.run(parser.parse_args(list(argv)), home=home, out=out, err=err)
    return code, out.getvalue(), err.getvalue()


def test_cli_bind_pause_unbind_export_import(anchor, tmp_path):
    home = anchor.tmp / "owner"
    code, _, err = _cli(anchor, home, "bind", GUILD)
    assert code == 1 and "--bot" in err  # no bot known yet
    assert _cli(anchor, home, "bind", GUILD, "--bot", BOT)[0] == 0
    assert _cli(anchor, home, "pause", GUILD)[0] == 0  # the only known bot is now implicit
    code, out, _ = _cli(anchor, home, "status", "--json")
    report = json.loads(out)
    assert code == 0 and report["guilds"] == [{"bot": BOT, "guild": GUILD, "state": "paused", "rev": 2}]
    code, out, _ = _cli(anchor, home, "export", "--guild", GUILD)
    exported = json.loads(out)
    exported["bots"][BOT]["guilds"][GUILD]["conversation"]["ambient_channels"] = ["900000000000000033"]
    exported["bots"][BOT]["guilds"][GUILD]["state"] = "active"
    path = tmp_path / "export.json"
    path.write_text(json.dumps(exported))
    assert _cli(anchor, home, "import", str(path))[0] == 0
    store = anchor.mod("cli").store_for(home).read()
    assert store.guild(BOT, GUILD)["conversation"]["ambient_channels"] == ["900000000000000033"]
    assert store.guild_state(BOT, GUILD) == "active"
    exported["bots"][BOT]["guilds"][GUILD]["operators"] = [OP]
    path.write_text(json.dumps(exported))
    code, _, err = _cli(anchor, home, "import", str(path))
    assert code == 1 and "unknown setting" in err
    assert _cli(anchor, home, "unbind", GUILD)[0] == 0
    assert anchor.mod("cli").store_for(home).read().guild_state(BOT, GUILD) == "unbound"
    assert _cli(anchor, home, "unbind", GUILD, "--purge")[0] == 0
    assert anchor.mod("cli").store_for(home).read().guild(BOT, GUILD) is None


def test_cli_rejects_non_snowflake_ids(anchor):
    home = anchor.tmp / "owner"
    code, _, err = _cli(anchor, home, "bind", "not-a-guild", "--bot", BOT)
    assert code == 1 and "guild" in err
    code, _, err = _cli(anchor, home, "bind", GUILD, "--bot", "../../etc")
    assert code == 1 and "--bot" in err


def test_cli_status_text_mentions_missing_operators(anchor):
    code, out, _ = _cli(anchor, anchor.tmp / "owner", "status")
    assert code == 0 and "Operators configured: 0" in out and "no server configured" in out


def test_bootstrap_registers_the_cli(anchor, monkeypatch):
    from fakes.ctx import FakeCtx

    home = anchor.tmp / "root"
    home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(home))
    ctx = FakeCtx("default")
    anchor.entry.register(ctx)
    assert [c["name"] for c in ctx.cli] == ["ctx-discord-policy"]


def test_reading_a_missing_home_creates_nothing(anchor, tmp_path):
    hermes = anchor.mod("hermes")
    ghost = tmp_path / "no-such-home"

    class Ctx:
        def get_config(self, key, default=None):
            raise AssertionError("must not be consulted for a missing home")

    assert hermes.read_plugin_setting(Ctx(), ghost, "operators", []) == []
    assert not ghost.exists()


def test_cli_errors_are_messages_not_tracebacks(anchor, tmp_path):
    home = anchor.tmp / "owner"
    code, _, err = _cli(anchor, home, "import", str(tmp_path / "missing.json"), "--bot", BOT)
    assert code == 1 and "ctx-discord-policy:" in err
    (tmp_path / "bad.json").write_text("{nope")
    code, _, err = _cli(anchor, home, "import", str(tmp_path / "bad.json"))
    assert code == 1
    data_dir = home / "plugin-data" / "ctx_discord_policy"
    data_dir.mkdir(parents=True)
    (data_dir / "status.json").write_text('["not", "a", "dict"]')
    assert _cli(anchor, home, "status")[0] == 0
    (data_dir / "policy.json").write_text("{ broken")
    code, out, err = _cli(anchor, home, "export")
    assert code == 1 and "corrupt" in err and out == ""
