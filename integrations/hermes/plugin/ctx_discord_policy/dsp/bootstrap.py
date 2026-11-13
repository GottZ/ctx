"""Per-profile registration against the shared runtime anchor.

``register()`` never blocks and never imports ``gateway.run``: it runs on Hermes' plugin-load worker while
discovery holds its lock. Anything that needs the gateway happens later (adapter binding, connect).
"""

from __future__ import annotations

import logging
import time
from pathlib import Path
from typing import Any

from . import cli, config, hermes
from .adapt.status import SECURITY, Outcome, Result, compare_with_previous, write_json_atomic
from .adapt.strategies import run_intervention
from .policy import hooks
from .runtime import RUNTIME, Instance

logger = logging.getLogger("ctx_discord_policy")


def _a1_isolation(profile: str) -> Result:
    mode, evidence = hermes.isolation_mode()
    if mode == "in_process":
        return Result(f"A1[{profile}]", SECURITY, Outcome.OK, "in_process", evidence)
    return Result(f"A1[{profile}]", SECURITY, Outcome.OFF,
                  f"plugins.isolation={mode}: adapter and runner patches need the gateway process", evidence)


def _a8_register_hooks(ctx: Any) -> Result:
    """Register every policy hook; any failure propagates so Hermes disposes the partial registration."""
    handles = []
    for name, callback in hooks.HOOKS:
        handles.append(ctx.register_hook(name, callback))
    return Result("A8", SECURITY, Outcome.ARMED, f"{len(handles)} hooks registered",
                  "ctx.register_hook")


def _install_config_reader(ctx: Any) -> None:
    """Settings are read through the official ``ctx.get_config`` of the first armed instance, always under
    the home that owns the bot (never the scope a hook happens to run in)."""
    with RUNTIME.lock:
        if getattr(RUNTIME, "config_ctx", None) is None:
            RUNTIME.config_ctx = ctx
            config.set_reader(lambda home, key, default: hermes.read_plugin_setting(RUNTIME.config_ctx, home,
                                                                                    key, default))


def write_status(home: Path) -> Path:
    path = hermes.plugin_data_dir(home) / "status.json"
    payload = {"written_at": time.time(), "hermes_version": hermes.hermes_version(), **RUNTIME.describe()}
    for change in compare_with_previous(path, payload):
        logger.warning("ctx_discord_policy: changed since last start: %s", change)
    write_json_atomic(path, payload)
    return path


def register(ctx: Any, *, plugin_dir: Path) -> bool:
    """Register this profile's instance. Returns True when the profile is armed."""
    home = hermes.current_home()
    home_key = hermes.home_key(home)
    profile = str(getattr(ctx, "profile_name", "") or hermes.profile_name_for_home(home) or "default")
    plugin_id = str(getattr(ctx, "plugin_id", "") or "ctx_discord_policy")

    isolation = run_intervention(RUNTIME.status, f"A1[{profile}]", SECURITY, lambda: _a1_isolation(profile))
    run_intervention(RUNTIME.status, "A2", SECURITY, lambda: Result(
        "A2", SECURITY, Outcome.OK, f"anchor {RUNTIME.version} api {RUNTIME.runtime_api} pid {RUNTIME.pid}",
        "sys.modules['ctx_discord_policy']"))
    if isolation.outcome is not Outcome.OK:
        logger.error("ctx_discord_policy: not active in profile %s: %s", profile, isolation.reason)
        return False

    # A8: let exceptions propagate (Hermes marks the plugin failed and disposes partial registrations;
    # the profile is then simply not armed, which keeps public admission into it closed).
    try:
        result = _a8_register_hooks(ctx)
    except Exception as exc:
        RUNTIME.status.record(Result(f"A8[{profile}]", SECURITY, Outcome.OFF,
                                     f"hook registration failed in profile {profile}: {exc}", "ctx.register_hook"))
        raise
    instance = Instance(home_key=home_key, profile=profile, plugin_id=plugin_id, plugin_dir=str(plugin_dir))
    with RUNTIME.lock:
        RUNTIME.instances[home_key] = instance
        RUNTIME.armed.arm(instance)
    RUNTIME.status.record(Result(f"A8[{profile}]", SECURITY, Outcome.ARMED, result.reason,
                                 result.probe))

    def _disarm() -> None:
        RUNTIME.armed.disarm(home_key)
        logger.info("ctx_discord_policy: profile %s disarmed (plugin unloaded)", profile)

    on_unload = getattr(ctx, "on_unload", None)
    if callable(on_unload):
        on_unload(_disarm)
    _install_config_reader(ctx)
    try:
        cli.register(ctx)
    except Exception:  # the CLI is a convenience; it must not unarm the policy
        logger.warning("ctx_discord_policy: CLI registration failed", exc_info=True)
    # The status file is written where a Discord bot binds (gateway process), never from register():
    # every ``hermes`` CLI invocation also loads plugins and must not overwrite the gateway's status.
    logger.info("ctx_discord_policy %s armed in profile %s", RUNTIME.version, profile)
    return True


def safe_write_status(home: Path) -> None:
    try:
        write_status(home)
    except OSError:
        logger.warning("ctx_discord_policy: could not write status file", exc_info=True)
