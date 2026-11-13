"""Two-home contract test against real Hermes plugin managers (DESIGN §3, S1 acceptance).

A launch home and routed profile homes, each with its own plugin manager, load this plugin the way a
multiplexed gateway does. Checked: one runtime anchor for all instances, every enabled profile armed, the
policy hook fires inside a routed profile's scope, a profile without the plugin is not armed, and a second
plugin version in the same process registers nothing.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

import pytest
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, dispatch_tool, install_plugin, load_in

@pytest.fixture
def homes(hermes, tmp_path):
    _evict(ANCHOR)
    os.environ["HERMES_HOME"] = str(tmp_path / "root")
    built = Homes(tmp_path)
    yield built
    _evict(ANCHOR)


def _route(anchor, lane: str, target: Path):
    from hermes_constants import hermes_home_key

    routes = sys.modules[f"{ANCHOR}.policy.routes"]
    return routes.Route(bot_id="900000000000000001", guild_id="900000000000000002",
                        channel_id="900000000000000003", message_id="900000000000000004",
                        author_id="900000000000000005", lane=lane, target_home_key=hermes_home_key(target))


def test_one_anchor_two_armed_profiles_hook_fires_in_routed_scope(homes):
    from hermes_constants import hermes_home_key

    routed = homes.profile("routed")
    manager_root = load_in(homes.root)
    manager_routed = load_in(routed)
    loaded_root = manager_root._plugins[PLUGIN]
    loaded_routed = manager_routed._plugins[PLUGIN]
    assert loaded_root.enabled and loaded_root.error is None, loaded_root.error
    assert loaded_routed.enabled and loaded_routed.error is None, loaded_routed.error
    # Hermes' per-profile module split is real ...
    assert loaded_root.module is not loaded_routed.module
    assert loaded_root.module.__name__ != loaded_routed.module.__name__
    # ... and both instances share the one runtime anchor.
    anchor = sys.modules[ANCHOR]
    runtime = sys.modules[f"{ANCHOR}.runtime"].RUNTIME
    assert loaded_root.module.load_anchor(homes.plugin_dir) is anchor
    assert runtime.armed.is_armed(hermes_home_key(homes.root))
    assert runtime.armed.is_armed(hermes_home_key(routed))

    # A public route set by launch-side code is enforced by the hook running in the routed profile.
    token = runtime.route.set(_route(anchor, "public", routed))
    try:
        blocked = dispatch_tool(routed, session_id="s", turn_id="t")
    finally:
        runtime.route.reset(token)
    assert blocked and "BLOCKED by ctx_discord_policy" in blocked

    token = runtime.route.set(_route(anchor, "operator", routed))
    try:
        assert dispatch_tool(routed, session_id="s", turn_id="t2") is None
    finally:
        runtime.route.reset(token)


def test_profile_without_plugin_is_not_armed_and_runs_no_policy_hook(homes):
    from hermes_constants import hermes_home_key

    bare = homes.profile("bare", enabled=False)
    load_in(homes.root)
    load_in(bare)
    runtime = sys.modules[f"{ANCHOR}.runtime"].RUNTIME
    assert runtime.armed.is_armed(hermes_home_key(homes.root))
    assert not runtime.armed.is_armed(hermes_home_key(bare))
    token = runtime.route.set(_route(sys.modules[ANCHOR], "public", bare))
    try:
        # This is why public admission into an unarmed profile must stay closed (A4): nothing blocks here.
        assert dispatch_tool(bare, session_id="s", turn_id="t") is None
    finally:
        runtime.route.reset(token)


def test_second_plugin_version_in_process_registers_nothing(homes, tmp_path, caplog):
    from hermes_constants import hermes_home_key

    load_in(homes.root)
    stale_dir = install_plugin(tmp_path / "stale-copy" / PLUGIN, version="3.9.9")
    stale = homes.profile("stale", plugin_dir=stale_dir)
    with caplog.at_level("ERROR"):
        manager = load_in(stale)
    loaded = manager._plugins[PLUGIN]
    runtime = sys.modules[f"{ANCHOR}.runtime"].RUNTIME
    assert loaded.error is None  # no exception in the load path ...
    assert not runtime.armed.is_armed(hermes_home_key(stale))  # ... but nothing registered
    assert "restart" in caplog.text.lower()
    assert runtime.status.snapshot()["refused"][-1]["version"] == "3.9.9"
    assert sys.modules[ANCHOR].__version__ == "3.0.0"
