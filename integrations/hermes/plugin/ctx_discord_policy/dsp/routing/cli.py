"""``hermes ctx-discord-policy routing …``: the Hermes profile per server from the host (PROFILE-PER-GUILD §6.4).

``list`` · ``set <guild> <profile>`` · ``clear <guild>`` · ``adopt <guild> [--entry NAME]``. The same flow as
the panel (:mod:`.apply`), run without a gateway: the choice is checked (valid name, not locked, the
profile exists, not the bot's own profile, no hand-written guild route of the same server), saved in the
default root's ``profile_routes`` and recorded in the policy store. The running gateway is a different
process, so it routes the server there after a restart (or **Apply again** in ``/policy``); until then its
per-message check keeps the server closed for members. Whether the profile is served and armed only the
gateway can tell; it checks that for every message (A4). That the profile runs no Discord bot of its own is
checked only by the panel's choice: a profile chosen here that does is routed, and Hermes then delivers
that server's tool messages through the profile's own bot.

Wiring (``dsp/cli.py``): ``setup(sub)`` next to the other subcommands; ``run(args, home=…, store=…, bot=…)``
when ``args.dsp_command == "routing"``.
"""

from __future__ import annotations

import argparse
import json
import sys
from typing import Any, Optional, TextIO

from .. import config, hermes
from ..store.file import PolicyStore, StoreError
from ..store.schema import SchemaError
from . import apply, eligibility, table

COMMAND = "routing"


def setup(sub: Any) -> None:
    """Add ``routing`` to the ``ctx-discord-policy`` subcommands (``sub`` from ``add_subparsers``)."""
    parser = sub.add_parser(COMMAND, help="Hermes profile per server (entries the policy manages in profile_routes)")
    actions = parser.add_subparsers(dest="dsp_routing", metavar="ACTION")
    listing = actions.add_parser("list", help="Show each server's profile: chosen, configured, exceptions")
    listing.add_argument("--json", action="store_true", help="Machine-readable output")
    chosen = actions.add_parser("set", help="Run a server in a Hermes profile")
    chosen.add_argument("guild", help="Discord server (guild) id")
    chosen.add_argument("profile", help="Hermes profile name")
    clear = actions.add_parser("clear", help="Run a server in the bot's own profile again (removes the entry)")
    clear.add_argument("guild", help="Discord server (guild) id")
    adopt = actions.add_parser("adopt", help="Take over a hand-written guild route of a server (rename only)")
    adopt.add_argument("guild", help="Discord server (guild) id")
    adopt.add_argument("--entry", help="Name of the hand-written route (default: the only one for the server)")
    for cmd in (listing, chosen, clear, adopt):
        cmd.add_argument("--bot", help="Discord application id of the bot (default: the only known bot)")


def _get(entry: Any, key: str) -> Any:
    return entry.get(key) if isinstance(entry, dict) else getattr(entry, key, None)


def _rows(store: PolicyStore, bot: str, owner: str) -> list[dict[str, Any]]:
    snapshot = store.read()
    guilds = ((snapshot.data.get("bots") or {}).get(bot) or {}).get("guilds") or {}
    entries = hermes.read_route_entries(hermes.default_root())
    locked = config.locked_profiles(hermes.default_root())
    rows = []
    for gid in sorted(guilds):
        own = table.own_entries(entries, bot, gid)
        core = table.core_guild_entries(entries, gid, owner)
        rows.append({
            "guild": gid, "chosen": (guilds[gid].get("routing") or {}).get("profile"),
            "configured": _get(own[0], "profile") if own else None,
            "core_entry": {"name": _get(core[0], "name"), "profile": _get(core[0], "profile")} if core else None,
            "exceptions": [{"name": _get(e, "name"), "profile": _get(e, "profile"),
                            "locked": locked is None or _get(e, "profile") in locked}
                           for e in table.core_exceptions(entries, gid, owner)],
        })
    return rows


def _list(store: PolicyStore, bot: str, owner: str, as_json: bool, out: TextIO) -> int:
    rows = _rows(store, bot, owner)
    if as_json:
        json.dump({"bot": bot, "owner_profile": owner, "servers": rows}, out, indent=2, sort_keys=True)
        out.write("\n")
        return 0
    if not rows:
        out.write(f"bot {bot}: no server configured\n")
    for row in rows:
        if row["chosen"]:
            head = f"{row['chosen']} (chosen here)"
        elif row["core_entry"]:
            head = f"{row['core_entry']['profile']} (hand-written entry {row['core_entry']['name']})"
        else:
            head = f"the bot's own profile ({owner})"
        configured = row["configured"] or (row["core_entry"] or {}).get("profile") or "no entry"
        line = f"guild {row['guild']}: {head} · Hermes configuration: {configured}"
        if row["chosen"] and row["configured"] != row["chosen"]:
            line += "  ! the configuration does not route it there (members' messages stay closed)"
        out.write(line + "\n")
        for exc in row["exceptions"]:
            out.write(f"    exception {exc['name']} -> {exc['profile']}" + (" (locked)" if exc["locked"] else "") + "\n")
    return 0


def _report(outcome: apply.Applied, bot: str, guild: str, out: TextIO) -> None:
    target = outcome.profile or "the bot's own profile"
    if outcome.saved:
        out.write(f"bot {bot} guild {guild}: {target}, saved in Hermes' configuration (profile_routes).\n")
    elif outcome.managed:
        out.write(f"bot {bot} guild {guild}: {target}, recorded in the policy. Hermes' configuration is managed "
                  f"({outcome.managed}), so nothing was written there; the gateway applies the choice while the "
                  "plugin runs. ")
        if outcome.snippet:
            out.write("Add this entry to your managed configuration so Hermes and cron know it:\n" + outcome.snippet)
        else:
            out.write(f"Remove the entry {table.entry_name(bot, guild)} from your managed configuration if it is "
                      "there.\n")
    else:
        out.write(f"bot {bot} guild {guild}: {target}, recorded in the policy ({outcome.reason}).\n")
    out.write("The running gateway applies it after a restart, or now with Apply again in /policy → Profile; "
              "members' messages in this server stay closed until it does.\n")
    if outcome.warning:
        out.write(f"! {outcome.warning}\n")


def run(args: argparse.Namespace, *, home: Any, store: PolicyStore, bot: str, out: TextIO = sys.stdout,
        err: TextIO = sys.stderr) -> int:
    action: Optional[str] = getattr(args, "dsp_routing", None) or "list"
    owner = eligibility.owner_profile_name(home)
    try:
        if action == "list":
            return _list(store, bot, owner, bool(getattr(args, "json", False)), out)
        guild = str(args.guild)
        if action == "set":
            outcome = apply.set_profile(store, bot, guild, args.profile, owner_home=home, by="cli", via="cli")
        elif action == "clear":
            outcome = apply.set_profile(store, bot, guild, None, owner_home=home, by="cli", via="cli")
        elif action == "adopt":
            name = args.entry or _only_core_entry(guild, owner)
            outcome = apply.adopt(store, bot, guild, name, owner_home=home, by="cli", via="cli")
            out.write(f"took over the hand-written route {name}.\n")
        else:
            err.write(f"unknown routing action {action}\n")
            return 2
    except apply.Refused as exc:
        text = str(exc)
        if exc.core_entry is not None:
            text += f" (`hermes ctx-discord-policy routing adopt {args.guild}`)"
        err.write(f"ctx-discord-policy: {text}\n")
        return 1
    except (LookupError, SchemaError, StoreError, OSError, ValueError, hermes.HermesUnavailable) as exc:
        err.write(f"ctx-discord-policy: {exc}\n")
        return 1
    _report(outcome, bot, guild, out)
    return 0


def _only_core_entry(guild: str, owner: str) -> str:
    found = table.core_guild_entries(hermes.read_route_entries(hermes.default_root()), guild, owner)
    if not found:
        raise LookupError(f"no hand-written route for server {guild} in Hermes' configuration")
    if len(found) > 1:
        names = ", ".join(str(_get(e, "name")) for e in found)
        raise LookupError(f"several hand-written routes for server {guild} ({names}); pass --entry NAME")
    return str(_get(found[0], "name"))
