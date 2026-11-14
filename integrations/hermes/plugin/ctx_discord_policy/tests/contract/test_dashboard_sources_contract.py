"""Settings page P2 against real Hermes: plugin RPC names, profile/skill/model lists (SETTINGS-PAGE §4)."""

from __future__ import annotations

import importlib.util
import inspect
import time
from pathlib import Path

import pytest

PLUGIN = "ctx_discord_policy"


def test_plugin_rpc_is_probed_and_our_method_names_are_valid(hermes, anchor):
    """``ctx.register_rpc`` exists from 04846730f3 on; our dotted names pass Hermes' own name rule."""
    hm, rpc = anchor.mod("hermes"), anchor.mod("rpc")
    present = importlib.util.find_spec("hermes_cli.plugin_rpc") is not None
    assert hm.rpc_available() is present
    report = {"rpc": present, "methods": []}
    if present:
        from hermes_cli.plugin_events import plugin_event_name
        from hermes_cli.plugin_rpc import register_rpc

        assert list(inspect.signature(register_rpc).parameters)[:3] == ["self", "name", "handler"]
        for name in rpc.READ_METHODS:
            report["methods"].append(plugin_event_name(PLUGIN, name, kind="RPC method"))
        assert report["methods"] == [f"plugin.{PLUGIN}.{n}" for n in rpc.READ_METHODS]
        from hermes_cli.gateway_client import connect_gateway
        from hermes_cli.gateway_runtime import discover_gateway_endpoint

        assert "endpoint" in inspect.signature(connect_gateway).parameters
        assert "timeout" in inspect.signature(discover_gateway_endpoint).parameters
    # Hermes serves a method as handler(dict(params), session_id=...) (gateway/session_plugin_rpc.py); without a
    # bot bound in this process every handler answers None rather than raising.
    bot, guild = "900000000000000001", "900000000000000002"
    calls = {"directory.guilds": {"bot": bot}, "directory.guild": {"bot": bot, "guild": guild},
             "members.search": {"bot": bot, "guild": guild, "q": "al"}, "users.resolve": {"bot": bot, "ids": []}}
    assert set(calls) == set(rpc.READ_METHODS)
    for name, params in calls.items():
        assert rpc.HANDLERS[name](dict(params), session_id=None) is None, name
    hermes.write_report("dashboard-rpc", report)


def test_profile_skill_and_model_lists_run_against_this_hermes(hermes, anchor):
    hm = anchor.mod("hermes")
    profiles = hm.profiles()
    assert isinstance(profiles, list) and all({"name", "path"} <= set(p) for p in profiles)
    from hermes_constants import get_hermes_home

    home = Path(get_hermes_home())
    skills = hm.skills_of(home)
    assert isinstance(skills, list)
    started = time.monotonic()
    models = hm.model_options(None)
    took = time.monotonic() - started
    assert models is None or isinstance(models.get("providers"), list)
    hermes.write_report("dashboard-lists", {"profiles": len(profiles), "skills": len(skills),
                                            "models": None if models is None else len(models["providers"]),
                                            "model_options_seconds": round(took, 2)})


def test_the_dashboard_secret_scope_reader_exists(hermes, anchor):
    hm = anchor.mod("hermes")
    assert hm.discord_bot_token() == ""      # no token in a fresh home, and no exception
    from agent.secret_scope import get_secret

    assert "default" in inspect.signature(get_secret).parameters


def test_member_search_is_a_setting_hermes_itself_accepts(hermes, anchor):
    """``member_search`` (auto | off) is declared so that Hermes' own settings writer takes it (and the
    automatic form offers it); any other value is refused there, not only in the plugin."""
    from hermes_cli.plugins_settings import save_plugin_settings

    repo = Path(__file__).resolve().parents[2]
    with pytest.raises(ValueError):
        save_plugin_settings(PLUGIN, repo, {"member_search": "maybe"})
    save_plugin_settings(PLUGIN, repo, {"member_search": "off"})
    assert anchor.mod("hermes").plugin_settings().get("member_search") == "off"
    save_plugin_settings(PLUGIN, repo, {"member_search": "auto"})


def test_the_skill_list_leaves_out_disabled_skills(hermes, anchor):
    """Hermes' ``find_all_skills(skip_disabled=True)`` means "ignore the disabled state" (its config UI); the
    page offers, and the environment check accepts, only skills a turn could load."""
    home = anchor.home
    for name in ("faq-lookup", "server-index"):
        skill = home / "skills" / name
        skill.mkdir(parents=True, exist_ok=True)
        (skill / "SKILL.md").write_text(f"---\nname: {name}\ndescription: test skill\n---\nBody\n", encoding="utf-8")
    config = home / "config.yaml"
    before = config.read_text(encoding="utf-8") if config.exists() else ""
    try:
        config.write_text(before + "\nskills:\n  disabled:\n  - faq-lookup\n", encoding="utf-8")
        names = [s["name"] for s in anchor.mod("hermes").skills_of(home)]
    finally:
        config.write_text(before, encoding="utf-8")
    assert "server-index" in names and "faq-lookup" not in names
    config.write_text(before + "\nskills:\n  disabled:\n  - faq-lookup\n", encoding="utf-8")
    try:
        assert anchor.mod("hermes").installed_skills(home) >= {"server-index"}
        assert "faq-lookup" not in anchor.mod("hermes").installed_skills(home)
    finally:
        config.write_text(before, encoding="utf-8")


def test_gateway_rpc_without_a_running_gateway_fails_fast_and_starts_none(hermes, anchor):
    hm = anchor.mod("hermes")
    if not hm.rpc_available():
        pytest.skip("this Hermes has no plugin RPC")
    from hermes_cli.gateway_runtime import discover_gateway_endpoint

    started = time.monotonic()
    with pytest.raises(RuntimeError, match="not reachable"):
        hm.gateway_rpc(anchor.home, "plugin.ctx_discord_policy.directory.guilds", {"bot": "1" * 18}, 3.0)
    assert time.monotonic() - started < 5
    assert getattr(discover_gateway_endpoint(anchor.home, timeout=1.0), "endpoint", None) is None
