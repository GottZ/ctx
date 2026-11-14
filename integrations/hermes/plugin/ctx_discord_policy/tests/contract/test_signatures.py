"""Print (and record) the Hermes attachment points the plugin's adaptation layer depends on.

S0 scope: report only. Later layers turn the security-class entries into hard assertions through the
probes in ``dsp.adapt``.
"""

from __future__ import annotations

import importlib
import inspect

TARGETS = [
    # (intervention id, class, module, attribute path)
    ("A1", "S", "hermes_cli.plugin_isolation", "isolation_mode"),
    ("A2", "S", "hermes_constants", "hermes_home_key"),
    ("A2", "S", "hermes_constants", "set_hermes_home_override"),
    ("A3", "S", "gateway.platform_registry", "platform_registry.register"),
    ("A3", "S", "hermes_cli.plugins", "PluginContext.register_platform_handler"),
    ("A4", "S", "gateway.run", "GatewayRunner._resolve_profile_home_for_source"),
    ("A4", "S", "gateway.platforms.base", "BasePlatformAdapter.build_source"),
    ("A5", "S", "plugins.platforms.discord.adapter", "DiscordAdapter._discord_message_admission"),
    ("A6", "S", "plugins.platforms.discord.adapter", "DiscordAdapter._handle_message"),
    ("A6", "S", "plugins.platforms.discord.adapter", "DiscordAdapter._self_is_explicitly_mentioned"),
    ("A7", "S", "gateway.run", "GatewayRunner._handle_message"),
    ("A7", "S", "gateway.run", "GatewayRunner._is_user_authorized"),
    ("A7", "S", "gateway.run", "GatewayRunner._thread_metadata_for_source"),
    ("A7", "S", "gateway.session_envelope", "restore_native"),
    ("A8", "S", "hermes_cli.plugins", "PluginContext.register_hook"),
    ("A8", "S", "hermes_cli.plugins", "PluginContext.on_unload"),
    ("A8", "S", "hermes_cli.plugins", "PluginContext.get_config"),
    ("A8", "S", "hermes_cli.plugins", "PluginContext.register_cli_command"),
    ("A9", "S", "tools.hook_output_spill", "get_spill_config"),
    ("K1", "K", "gateway.platforms.base", "BasePlatformAdapter.delete_message"),
    ("K2", "K", "gateway.run_turn_progress", "GatewayTurnProgressMixin._progress_absorb"),
    ("K2b", "K", "gateway.run_turn_progress", "GatewayTurnProgressMixin._progress_edit_state"),
    ("K3", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.send"),
    ("K3", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.edit_message"),
    ("K3", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.send_multiple_images"),
    ("K3", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.send_voice"),
    ("K3", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.send_video"),
    ("K3", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.send_document"),
    ("K4", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.on_processing_start"),
    ("K4", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.on_processing_complete"),
    ("K5", "K", "plugins.platforms.discord.adapter", "DiscordAdapter.connect"),
]


def _resolve(module_name: str, path: str):
    obj = importlib.import_module(module_name)
    for part in path.split("."):
        obj = getattr(obj, part)
    return obj


def _describe(obj) -> dict:
    target = inspect.unwrap(obj)
    info = {"kind": "async" if inspect.iscoroutinefunction(target) else "sync"}
    try:
        info["signature"] = str(inspect.signature(target))
    except (TypeError, ValueError):
        info["signature"] = "<no signature>"
    try:
        lines, start = inspect.getsourcelines(target)
        info["where"] = f"{inspect.getsourcefile(target)}:{start}"
    except (OSError, TypeError):
        info["where"] = "<no source>"
    return info


def test_print_signatures(hermes, capsys):
    rows = []
    for intervention, klass, module_name, path in TARGETS:
        row = {"id": intervention, "class": klass, "target": f"{module_name}:{path}"}
        try:
            row.update(_describe(_resolve(module_name, path)))
            row["present"] = True
        except Exception as exc:  # report, never hide
            row.update(present=False, error=f"{type(exc).__name__}: {exc}")
        rows.append(row)
    adapter = _resolve("plugins.platforms.discord.adapter", "DiscordAdapter")
    base = _resolve("gateway.platforms.base", "BasePlatformAdapter")
    owner = next((cls for cls in adapter.__mro__ if "delete_message" in cls.__dict__), None)
    rows.append({
        # Native delete support may come from the class itself or a mixin (upstream main: media mixin).
        "id": "K1", "class": "K", "target": "DiscordAdapter.delete_message is not the base no-op",
        "present": adapter.delete_message is not base.delete_message,
        "kind": f"defined in {getattr(owner, '__module__', '?')}.{getattr(owner, '__qualname__', '?')}",
    })
    report = hermes.write_report("signatures", {"ref": hermes.ref, "targets": rows})
    with capsys.disabled():
        print(f"\n== Hermes {hermes.ref}: {sum(r['present'] for r in rows)}/{len(rows)} present ({report})")
        for row in rows:
            state = "ok " if row["present"] else "MISSING"
            print(f"{row['id']:4} {row['class']} {state} {row['target']} {row.get('kind', '')} "
                  f"{row.get('signature', row.get('error', ''))}")
    missing_security = [r for r in rows if r["class"] == "S" and not r["present"]]
    assert not missing_security, missing_security
