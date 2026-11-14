"""Per-profile registration against the shared runtime anchor.

``register()`` never blocks and never imports ``gateway.run``: it runs on Hermes' plugin-load worker while
discovery holds its lock. Anything that needs the gateway happens later (adapter binding, connect).
"""

from __future__ import annotations

import logging
import time
from pathlib import Path
from typing import Any

from . import cli, config, hermes, rpc, snapshot
from .adapt.status import SECURITY, Outcome, Result, compare_with_previous, write_json_atomic
from .adapt import interventions
# Loaded with the anchor, never lazily: after an in-place update a lazy import would mix two versions in one
# process (the entry point also imports every module of the package before the anchor counts as ready).
from . import prompts, wiring  # noqa: F401
from .delivery import delete, progress, split  # noqa: F401
from .ui import commands as ui_commands  # noqa: F401
from .adapt.strategies import run_intervention
from .policy import hooks
from .runtime import RUNTIME, Instance

logger = logging.getLogger("ctx_discord_policy")


# ── attachment points of the layers ──────────────────────────────────────────────────────────────────
#
# REGISTER_STEPS run in every profile instance at the end of ``register()``, once that profile is armed:
# ``step(ctx, instance)``. Plugin-load time: never block, never import ``gateway.run``.
#
# BIND_STEPS run on every call of the Discord platform-handler factory (a connected bot, also after a
# reconnect with a new client), in this order, after the bot is bound (``RUNTIME.bots.bind``) and before the
# status file is written: ``step(bind: wiring.Bind)``. Every step records its own outcome in the status
# register; an exception is logged and never breaks the bind or the steps after it. Order matters where it
# is noted. A layer adds itself with one line.
REGISTER_STEPS: tuple = (
    ("P3", rpc.register_step),               # read-only plugin RPC for a settings page in another process
)

BIND_STEPS: tuple = (
    ("K1", wiring.install_delete),           # first: sees the adapter class as Hermes left it
    ("K1b", wiring.install_delete_chat),     # after K1: wraps what K1 found or injected
    ("K3/K4", wiring.install_delivery),
    ("K2/K2b", wiring.install_progress),
    ("R1", wiring.apply_routing),            # chosen profiles back into the running table (managed config)
    ("K8", wiring.install_models),           # server models; gateway.run is loaded once a bot binds
    ("K5", wiring.install_reaction_wakeup),  # after R1: woken messages go through admission and A4-P
    ("K7", wiring.probe_hydration),
    ("P2", snapshot.bind_step),              # the bot's server directory for the settings page
    ("K6", wiring.install_ui),               # the interface last: everything it shows is in place
)


def bind_bot(native: Any, adapter: Any, binding: Any) -> None:
    """Run :data:`BIND_STEPS` for one bound bot, then write the status file in the bot owner's home."""
    bind = wiring.Bind(native=native, adapter=adapter, binding=binding)
    for name, step in BIND_STEPS:
        try:
            step(bind)
        except Exception:  # steps record their own outcomes; one failing layer never stops the others
            logger.exception("ctx_discord_policy: bind step %s failed", name)
    safe_write_status(binding.owner_home)


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
        handle = ctx.register_hook(name, callback)
        if handle is None:  # Hermes ignores registrations after a timed-out (abandoned) load
            raise RuntimeError(f"register_hook({name!r}) returned no handle; plugin load was abandoned")
        handles.append(handle)
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
    payload = {"status_schema": 1, "written_at": time.time(), "hermes_version": hermes.hermes_version(),
               **RUNTIME.describe()}
    now = payload.get("interventions") or {}
    for change in compare_with_previous(path, payload):
        name = change.split(": ", 1)[0]
        if name not in now:  # runtime notices (per-turn entries) start empty in every process
            logger.info("ctx_discord_policy: since the last start: %s", change)
            continue
        degraded = now[name].get("outcome") in (Outcome.FALLBACK.value, Outcome.OFF.value)
        logger.warning("ctx_discord_policy: %s since the last start: %s",
                       "degraded" if degraded else "adapted to this Hermes", change)
    write_json_atomic(path, payload)
    return path


def register(ctx: Any, *, plugin_dir: Path) -> bool:
    """Register this profile's instance. Returns True when the profile is armed."""
    home = hermes.current_home()
    home_key = hermes.home_key(home)
    profile = str(getattr(ctx, "profile_name", "") or hermes.profile_name_for_home(home) or "default")
    plugin_id = str(getattr(ctx, "plugin_id", "") or "ctx_discord_policy")

    # The CLI first: ``hermes ctx-discord-policy status`` must work exactly when protection is not active.
    try:
        cli.register(ctx)
    except Exception:
        logger.warning("ctx_discord_policy: CLI registration failed", exc_info=True)
    _install_config_reader(ctx)
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
    instance = Instance(home_key=home_key, profile=profile, plugin_id=plugin_id, plugin_dir=str(plugin_dir),
                        home=str(home))
    with RUNTIME.lock:
        RUNTIME.instances[home_key] = instance
        RUNTIME.armed.arm(instance)
    RUNTIME.status.record(Result(f"A8[{profile}]", SECURITY, Outcome.ARMED, result.reason,
                                 result.probe))

    def _disarm() -> None:
        RUNTIME.armed.disarm(home_key)
        RUNTIME.status.record(Result(f"A8[{profile}]", SECURITY, Outcome.OFF, "plugin unloaded in this profile",
                                     "on_unload"))
        logger.info("ctx_discord_policy: profile %s disarmed (plugin unloaded)", profile)

    on_unload = getattr(ctx, "on_unload", None)
    if callable(on_unload):
        on_unload(_disarm)
    # A3: the official way to the Discord adapter; binds the bot when it connects in this profile.
    register_handler = getattr(ctx, "register_platform_handler", None)
    if callable(register_handler):
        register_handler("discord", interventions.discord_platform_handler)
    else:
        RUNTIME.status.record(Result("A3", SECURITY, Outcome.OFF, "ctx.register_platform_handler missing",
                                     "PluginContext"))
    interventions.install_runner()  # only if gateway.run is already imported; never imports it
    for name, step in REGISTER_STEPS:
        try:
            step(ctx, instance)
        except Exception:  # a layer's registration never disarms the profile
            logger.exception("ctx_discord_policy: register step %s failed in profile %s", name, profile)
    # The status file is written where a Discord bot binds (gateway process), never from register():
    # every ``hermes`` CLI invocation also loads plugins and must not overwrite the gateway's status.
    logger.info("ctx_discord_policy %s armed in profile %s", RUNTIME.version, profile)
    return True


def safe_write_status(home: Path) -> None:
    try:
        write_status(home)
    except Exception:  # the status file is informational; it must never break a bind or connect
        logger.warning("ctx_discord_policy: could not write status file", exc_info=True)
