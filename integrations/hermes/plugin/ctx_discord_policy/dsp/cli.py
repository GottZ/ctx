"""``hermes ctx-discord-policy …``: host-side administration of the policy store (operator with host access).

Subcommands: ``status``, ``bind``, ``pause``, ``unbind``, ``export``, ``import``. The store edited is the
one of the active Hermes profile (``hermes -p <profile> ctx-discord-policy …``), i.e. the profile that owns
the Discord bot. Operators themselves are never set here; they live in the Hermes configuration
(``hermes config set plugins.entries.ctx_discord_policy.settings.operators '["<id>"]'``).
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Optional, TextIO

from . import config, hermes
from .store import ops
from .store.file import PolicyStore, StoreError
from .store.schema import SchemaError, is_snowflake


def store_for(home: Path) -> PolicyStore:
    return PolicyStore(hermes.plugin_data_dir(home) / "policy.json")


def _known_bots(home: Path, store: PolicyStore) -> list[str]:
    bots = set(store.read().data.get("bots", {}))
    status = _gateway_status(home)
    if isinstance(status, dict) and isinstance(status.get("bots"), dict):
        bots.update(str(b) for b in status["bots"])
    return sorted(b for b in bots if is_snowflake(b))


def _resolve_bot(home: Path, store: PolicyStore, explicit: Optional[str]) -> str:
    if explicit:
        if not is_snowflake(str(explicit)):
            raise SchemaError(f"--bot {explicit!r} is not a Discord application id")
        return str(explicit)
    bots = _known_bots(home, store)
    if len(bots) == 1:
        return bots[0]
    if not bots:
        raise SchemaError("no Discord bot known yet: start the gateway once, or pass --bot <application id>")
    raise SchemaError(f"several bots are known ({', '.join(bots)}); pass --bot <application id>")


def _gateway_status(home: Path) -> Any:
    try:
        status = json.loads((hermes.plugin_data_dir(home) / "status.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return status if isinstance(status, dict) else None


def setup(parser: argparse.ArgumentParser) -> None:
    sub = parser.add_subparsers(dest="dsp_command", metavar="COMMAND")
    status = sub.add_parser("status", help="Show protection status, bound bots and configured servers")
    status.add_argument("--json", action="store_true", help="Machine-readable output")
    for name, text in (("bind", "Activate the policy in a server (creates its configuration)"),
                       ("pause", "Pause the policy in a server (stock Hermes behaviour there)"),
                       ("unbind", "Deactivate a server; --purge also deletes its configuration")):
        cmd = sub.add_parser(name, help=text)
        cmd.add_argument("guild", help="Discord server (guild) id")
        cmd.add_argument("--bot", help="Discord application id of the bot (default: the only known bot)")
        if name == "unbind":
            cmd.add_argument("--purge", action="store_true", help="Delete the server's configuration")
    exp = sub.add_parser("export", help="Print configuration as JSON")
    exp.add_argument("--guild", help="Only this server")
    exp.add_argument("--bot", help="Only this bot")
    imp = sub.add_parser("import", help="Replace one server's configuration from an export file")
    imp.add_argument("file", help="JSON file as written by export (one bot, one server)")
    imp.add_argument("--guild", help="Server id inside the file (default: the only one)")
    imp.add_argument("--bot", help="Bot id inside the file (default: the only one)")


def _status(home: Path, store: PolicyStore, as_json: bool, out: TextIO) -> int:
    snapshot = store.read()
    status = _gateway_status(home)
    operator_count = len(config.operators(home))
    guilds = [
        {"bot": bot, "guild": gid, "state": snapshot.guild_state(bot, gid), "rev": g.get("rev", 0)}
        for bot, b in snapshot.data.get("bots", {}).items() for gid, g in b.get("guilds", {}).items()
    ]
    report = {"home": str(home), "store": str(store.path), "store_state": snapshot.state,
              "store_error": snapshot.error, "store_problems": list(snapshot.problems),
              "operators_configured": operator_count, "guilds": guilds, "gateway_status": status}
    if as_json:
        json.dump(report, out, indent=2, sort_keys=True, default=str)
        out.write("\n")
        return 0
    out.write(f"Store: {store.path} [{snapshot.state}]\n")
    if snapshot.error:
        out.write(f"  ! {snapshot.error}\n")
    for problem in snapshot.problems:
        out.write(f"  ~ {problem}\n")
    out.write(f"Operators configured: {operator_count}"
              + ("" if operator_count else "  (no operator: only public lanes; set "
                 "plugins.entries.ctx_discord_policy.settings.operators)") + "\n")
    for row in guilds:
        out.write(f"  bot {row['bot']} guild {row['guild']}: {row['state']} (rev {row['rev']})\n")
    if not guilds:
        out.write("  no server configured\n")
    if status is None:
        out.write("Gateway status: none yet (the gateway writes it when a Discord bot connects)\n")
    else:
        out.write(f"Gateway status (pid {status.get('pid')}, plugin {status.get('plugin_version')}):\n")
        interventions = status.get("interventions") if isinstance(status.get("interventions"), dict) else {}
        for name, result in sorted(interventions.items()):
            if not isinstance(result, dict):
                continue
            out.write(f"  {result.get('klass', '?')} {name:<16} {result.get('outcome', '?'):<9} "
                      f"{result.get('reason', '')}\n")
        armed = status.get("armed") if isinstance(status.get("armed"), dict) else {}
        names = sorted(str(v.get("profile", "?")) for v in armed.values() if isinstance(v, dict))
        out.write(f"  armed profiles: {', '.join(names) or 'none'}\n")
    return 0


def run(args: argparse.Namespace, *, home: Optional[Path] = None, out: TextIO = sys.stdout,
        err: TextIO = sys.stderr) -> int:
    home = Path(home) if home is not None else hermes.current_home()
    store = store_for(home)
    command = getattr(args, "dsp_command", None)
    try:
        if command in (None, "status"):
            return _status(home, store, bool(getattr(args, "json", False)), out)
        if command == "export":
            snapshot = store.read()
            if snapshot.state == "corrupt":
                raise StoreError(f"policy store is corrupt ({snapshot.error}); nothing to export")
            json.dump(ops.export(snapshot, args.bot, args.guild), out, indent=2, sort_keys=True,
                      ensure_ascii=False)
            out.write("\n")
            return 0
        if command == "import":
            return _import(home, store, args, out)
        bot = _resolve_bot(home, store, getattr(args, "bot", None))
        if command == "bind":
            snapshot = ops.bind(store, bot, args.guild, by="cli")
        elif command == "pause":
            snapshot = ops.set_state(store, bot, args.guild, "paused", by="cli")
        elif command == "unbind":
            snapshot = (ops.purge if args.purge else
                        lambda s, b, g, by: ops.set_state(s, b, g, "unbound", by=by))(store, bot, args.guild, by="cli")
        else:
            err.write(f"unknown command {command}\n")
            return 2
        out.write(f"bot {bot} guild {args.guild}: {snapshot.guild_state(bot, args.guild)}\n")
        return 0
    except (SchemaError, StoreError, OSError, ValueError) as exc:
        err.write(f"ctx-discord-policy: {exc}\n")
        return 1


def _import(home: Path, store: PolicyStore, args: argparse.Namespace, out: TextIO) -> int:
    payload: Any = json.loads(Path(args.file).read_text(encoding="utf-8"))
    bots = payload.get("bots") if isinstance(payload, dict) else None
    if not isinstance(bots, dict) or not bots:
        raise SchemaError("import: expected an export document with 'bots'")
    bot = str(args.bot) if args.bot else (next(iter(bots)) if len(bots) == 1 else None)
    if bot is None or bot not in bots:
        raise SchemaError("import: pass --bot (the file has several bots or not this one)")
    guilds = (bots[bot] or {}).get("guilds") or {}
    guild = str(args.guild) if args.guild else (next(iter(guilds)) if len(guilds) == 1 else None)
    if guild is None or guild not in guilds:
        raise SchemaError("import: pass --guild (the file has several servers or not this one)")
    snapshot = ops.import_guild(store, bot, guild, guilds[guild], by="cli",
                                prompt_cap=config.setting(home, "guild_prompt_max_chars"))
    out.write(f"imported bot {bot} guild {guild}: {snapshot.guild_state(bot, guild)} "
              f"(rev {snapshot.guild(bot, guild)['rev']})\n")
    return 0


def handler(args: argparse.Namespace) -> int:
    return run(args)


def register(ctx: Any) -> None:
    register_cli = getattr(ctx, "register_cli_command", None)
    if callable(register_cli):
        register_cli("ctx-discord-policy", "ctx_discord_policy: status and server activation",
                     setup, handler, description=__doc__.splitlines()[0])
