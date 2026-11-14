"""Routes of the settings page backend (SETTINGS-PAGE §3.4), framework-free.

``handle(WebRequest) -> WebResponse``; the HTTP layer (``dashboard/plugin_api.py``) has already run the guard
(``web.guard.precheck``) and parsed a size-limited JSON body. Every write:

* acts as the host actor (``guard.host_actor``) and passes ``perms.check_write`` / ``perms.require`` for
  exactly the paths it writes;
* calls one ``store.ops`` operation inside ``ops.origin("dashboard")`` with ``by="dashboard"``, so the
  store's lock, ``rev`` check, audit and strict validation apply (no handler opens ``policy.json``);
* uses the store of the request's own Hermes home (``WebRequest.home``, from the route scope), never a home or
  profile named in the body (unknown body keys are refused).

Environment checks (``store.checks``) run before the write: errors refuse it, warnings come back with it.
"""

from __future__ import annotations

import copy
import difflib
import inspect
import json
import logging
import os
import re
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Callable, Mapping, Optional

from .. import RUNTIME_API, __version__, hermes
from ..adapt.status import write_json_atomic
from ..policy import lanes as policy_lanes
from .. import prompts
from ..prompts.assemble import GUIDANCE_HEADER
from ..store import checks, ops, schema
from ..store.file import ConflictError, PolicyStore, Snapshot, StoreError
from ..store.schema import SchemaError, is_snowflake
from ..ui import notify, perms
from . import catalog, directory, guard, settings_io, sources
from .responses import WebResponse, error, ok

logger = logging.getLogger("ctx_discord_policy")

API_VERSION = 1
PREVIEW_DRAFT_LIMIT = 64 * 1024
HEARTBEAT_EVERY = 60.0
_SHA256 = re.compile(r"[0-9a-f]{64}")
_FIELD = re.compile(r"^([a-z_]+(?:\.[a-z_]+)*)")


@dataclass
class WebRequest:
    method: str
    path: str
    query: Mapping[str, str] = field(default_factory=dict)
    body: Any = None
    home: Path = Path(".")
    plugin_dir: Path = Path(".")
    process: str = "dashboard"     # gateway | dashboard | plugin_host
    surface: str = "dashboard"
    backend: Any = None


class HermesBackend:
    """What the routes ask Hermes for (tests substitute a fake)."""

    hermes = hermes

    def read_settings(self) -> dict:
        return hermes.plugin_settings()

    def write_settings(self, plugin_dir: Path, values: dict) -> list:
        return hermes.save_plugin_settings(plugin_dir, values)

    def settings_changed(self, written: list) -> None:
        hermes.settings_changed(written)

    def spill_config(self) -> Optional[dict]:
        return hermes.spill_config()

    def skill_rows(self, home: Path) -> Optional[list]:
        return hermes.skills_of(home)

    def skills(self, home: Path) -> Optional[frozenset]:
        try:
            return hermes.installed_skills(home)
        except Exception:
            return None

    def profile_of(self, home: Path) -> str:
        return hermes.profile_name_for_home(home) or "default"

    def profile_home(self, name: str) -> Optional[Path]:
        """Home of an existing Hermes profile ``name`` (``hermes.profile_dir`` of the routing layer), or None."""
        if not hermes.valid_profile_name(name):
            return None
        try:
            path = Path(hermes.profile_dir(name))
        except Exception:
            return None
        return path if path.is_dir() else None

    _profiles_cache: tuple = (0.0, None)

    def profiles(self) -> Optional[list]:
        """Hermes' profile list, cached 30 s (listing many profiles takes a few hundred milliseconds)."""
        at, rows = HermesBackend._profiles_cache
        if rows is None or time.monotonic() - at > 30.0:
            rows = hermes.profiles()
            HermesBackend._profiles_cache = (time.monotonic(), rows)
        return rows

    def models(self, profile: Optional[str]) -> Optional[dict]:
        return hermes.model_options(profile)


DEFAULT_BACKEND = HermesBackend()


class Refusal(Exception):
    def __init__(self, response: WebResponse) -> None:
        super().__init__(response.status)
        self.response = response


def refuse(status: int, code: str, message: str, **extra: Any) -> Refusal:
    return Refusal(error(status, code, message, **extra))


# ── small helpers ────────────────────────────────────────────────────────────────────────────────

_STORES: dict[str, PolicyStore] = {}
_STORES_LOCK = threading.Lock()
_HEARTBEATS: dict[str, float] = {}


def store_for(home: Path) -> PolicyStore:
    path = hermes.plugin_data_dir(home) / "policy.json"
    with _STORES_LOCK:
        return _STORES.setdefault(str(path), PolicyStore(path))


def _read_json(path: Path) -> Optional[dict]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return data if isinstance(data, dict) else None


def gateway_status(home: Path) -> Optional[dict]:
    return _read_json(hermes.plugin_data_dir(home) / "status.json")


def who(by: Any, via: Any = None) -> dict[str, Any]:
    text = str(by) if by is not None else None
    if text == "dashboard":
        label = "the Hermes settings page"
    elif text == "cli":
        label = "the host command line"
    elif text and is_snowflake(text):
        label = f"Discord user {text}"
    else:
        label = "someone"
    return {"by": text, "label": label, "via": via if isinstance(via, str) else None}


def _live_bots(home: Path) -> dict[str, dict]:
    try:
        from ..runtime import RUNTIME
    except ImportError:  # pragma: no cover - the runtime is part of the anchor
        return {}
    key = hermes.home_key(home)
    out = {}
    for bot, info in RUNTIME.bots.snapshot().items():
        owner = info.get("owner_home")
        if owner and hermes.home_key(owner) == key:
            out[str(bot)] = info
    return out


def status_bots(home: Path) -> dict[str, str]:
    """``{bot: owner home}`` from a profile's ``status.json``. The gateway writes its process-wide bot list into
    every profile's status file, so each entry names the home that owns it."""
    status = gateway_status(home)
    bots = status.get("bots") if isinstance(status, dict) else None
    out = {}
    for bot, info in (bots.items() if isinstance(bots, dict) else ()):
        owner = info.get("owner_home") if isinstance(info, dict) else None
        if is_snowflake(str(bot)) and isinstance(owner, str) and owner:
            out[str(bot)] = owner
    return out


def known_bots(req: WebRequest, snap: Snapshot) -> list[str]:
    """Bots this profile owns: in its store, its gateway's status (owner = this home), its directory, live."""
    key = hermes.home_key(req.home)
    owners = status_bots(req.home)
    bots = set(snap.data.get("bots", {}))
    bots.update(b for b, owner in owners.items() if hermes.home_key(owner) == key)
    # A directory left behind by a bot that has moved to another profile does not make it this profile's.
    bots.update(b for b in directory.snapshot_bots(req.home) if b not in owners or hermes.home_key(owners[b]) == key)
    bots.update(_live_bots(req.home))
    return sorted(b for b in bots if is_snowflake(b))


def _bot_or_404(req: WebRequest, bot: str, snap: Snapshot) -> None:
    if not is_snowflake(bot) or bot not in known_bots(req, snap):
        raise refuse(404, "not_found", "This bot is not known in the active Hermes profile. Next step: switch to "
                                       "the profile that runs it.")


def _ids_or_404(bot: str, guild: str) -> None:
    if not (is_snowflake(bot) and is_snowflake(guild)):
        raise refuse(404, "not_found", "Not a Discord id.")


def guild_dir(req: WebRequest, bot: str, guild: str) -> directory.GuildDirectory:
    return directory.guild_directory(req.home, bot, guild, allow_remote=req.process != "gateway")


def bot_dir(req: WebRequest, bot: str) -> directory.BotDirectory:
    return directory.bot_directory(req.home, bot, allow_remote=req.process != "gateway")


def _mode(req: WebRequest, snap: Snapshot) -> str:
    if not snap.writable:
        return "read-only"
    if req.process == "gateway" and _live_bots(req.home):
        return "live"
    return "snapshot"


def _body(req: WebRequest, allowed: set, required: set = frozenset()) -> dict:
    body = req.body
    if not isinstance(body, dict):
        raise refuse(400, "bad_request", "Expected a JSON object.")
    extra = sorted(set(body) - set(allowed))
    if extra:
        raise refuse(422, "invalid", f"{extra[0]} is not accepted here.", field_name=extra[0])
    missing = sorted(set(required) - set(body))
    if missing:
        raise refuse(422, "invalid", f"{missing[0]} is required.", field_name=missing[0])
    return body


def _rev(body: dict) -> int:
    rev = body.get("expected_rev")
    if isinstance(rev, bool) or not isinstance(rev, int) or rev < 0:
        raise refuse(422, "invalid", "expected_rev must be the revision this page loaded (a whole number).",
                     field_name="expected_rev")
    return rev


def _schema_field(message: str) -> Optional[str]:
    """The dotted schema path a ``SchemaError`` names (``"worklog.channel: expected …"``), if any."""
    match = _FIELD.match(message)
    if match is None:
        return None
    path = match.group(1)
    return path if "." in path or path in ("audit_channel", "state") else None


REQUIRED_KEYWORDS = ("by", "expected_rev")


def _call(fn: Callable, *args: Any, **kwargs: Any) -> Any:
    """Call an operation of another layer. ``by`` and ``expected_rev`` must be accepted (a dropped revision
    check would turn a conflict into a silent overwrite); optional extras (``via``, ``prompt_cap``) are left out
    when the operation does not declare them (``via`` is also set by ``ops.origin``)."""
    try:
        params = inspect.signature(fn).parameters
    except (TypeError, ValueError):
        return fn(*args, **kwargs)
    if any(p.kind is inspect.Parameter.VAR_KEYWORD for p in params.values()):
        return fn(*args, **kwargs)
    missing = [k for k in REQUIRED_KEYWORDS if k in kwargs and k not in params]
    if missing:
        raise refuse(501, "not_available", "This version of the plugin cannot do that safely yet "
                                           f"(the operation does not take {', '.join(missing)}).")
    return fn(*args, **{k: v for k, v in kwargs.items() if k in params})


def _conflict(req: WebRequest, bot: str, guild: str) -> WebResponse:
    snap = store_for(req.home).read()
    current = snap.guild(bot, guild) or {}
    via = next((a.get("via") for a in reversed(snap.data.get("audit", []))
                if a.get("bot") == bot and a.get("guild") == guild), None)
    label = who(current.get("updated_by"), via)
    return error(409, "conflict", f"Not saved: {label['label']} changed this server's settings after you opened "
                                  "this page. Reload to see the current settings.",
                 current_rev=snap.guild_rev(bot, guild), updated_by=current.get("updated_by"),
                 updated_by_label=label["label"], updated_at=current.get("updated_at"), updated_via=via)


def _write(req: WebRequest, bot: str, guild: str, run: Callable[[], Any]) -> Any:
    """Run one store operation as the settings page and map the store's refusals to HTTP."""
    try:
        with ops.origin("dashboard"):
            return run()
    except ConflictError:
        raise Refusal(_conflict(req, bot, guild))
    except SchemaError as exc:
        message = str(exc)
        raise refuse(422, "invalid", f"Not saved: {message}.", field_name=_schema_field(message))
    except StoreError as exc:
        if not store_for(req.home).read().writable:
            raise refuse(423, "read_only", "Not saved: the policy file on the host is damaged or was written by a "
                                           "newer plugin version, so it is read-only. Every server stays paused "
                                           "until it is repaired.")
        logger.warning("ctx_discord_policy: settings page write refused: %s", exc)
        raise refuse(503, "unavailable", "Not saved: the policy file is busy. Try again in a moment.")


def _target_home(req: WebRequest, guild: Optional[dict]) -> tuple[Optional[Path], str]:
    """Home and name of the profile this guild's turns run in (lanes apply there, DESIGN §3.2)."""
    backend = req.backend
    routed = ((guild or {}).get("routing") or {}).get("profile") if isinstance(guild, dict) else None
    if isinstance(routed, str) and routed:
        return backend.profile_home(routed), routed
    return req.home, backend.profile_of(req.home)


def environment_for(req: WebRequest, guild: Optional[dict], found: directory.GuildDirectory) -> checks.Environment:
    target, profile = _target_home(req, guild)
    skills = req.backend.skills(target) if target is not None else None
    known = found.known
    return checks.Environment(skills=skills, skills_profile=profile,
                              channels=found.channel_kinds() if known else None,
                              roles=found.role_ids() if known else None,
                              emojis=found.emoji_ids() if known else None,
                              directory_source=found.label() if known else "")


def _settings_paths(guild: dict) -> dict[str, Any]:
    flat = checks.flatten({k: v for k, v in guild.items() if k not in perms.BOOKKEEPING})
    return {k: v for k, v in flat.items() if k not in catalog.HIDDEN}


def _summary(guild: dict, cap: int) -> dict[str, Any]:
    return {"ambient": len(guild["conversation"]["ambient_channels"]),
            "excluded": len(guild["conversation"]["excluded_channels"]),
            "guidance_chars": len(guild["prompts"]["guild"]), "guidance_max": cap,
            "worklog": guild["worklog"]["channel"] is not None, "managers": guild["managers"]["mode"]}


def _last_via(snap: Snapshot, bot: str, guild: str) -> Optional[str]:
    return next((a.get("via") for a in reversed(snap.data.get("audit", []))
                 if a.get("bot") == bot and a.get("guild") == guild), None)


def guild_body(req: WebRequest, snap: Snapshot, bot: str, guild_id: str) -> dict[str, Any]:
    found = guild_dir(req, bot, guild_id)
    stored = snap.guild(bot, guild_id)
    configured = stored is not None
    settings = copy.deepcopy(stored) if configured else schema.guild_defaults()
    cap = settings_io.prompt_cap(req.backend)
    findings = checks.environment(_settings_paths(settings), environment_for(req, settings, found)) \
        if configured else []
    history = [{"sha256": h["sha256"], **who(h["by"]), "at": h["at"], "chars": len(h["text"])}
               for h in reversed(settings["prompts"]["history"])]
    return {"bot": bot, "guild": guild_id, "name": found.name, "configured": configured,
            "state": snap.guild_state(bot, guild_id), "rev": snap.guild_rev(bot, guild_id), "settings": settings,
            "updated": {**who(settings.get("updated_by"), _last_via(snap, bot, guild_id)),
                        "at": settings.get("updated_at")},
            "bound": {**who(settings.get("bound_by")), "at": settings.get("bound_at")},
            "findings": [f.as_dict() for f in findings],
            "guidance": {"chars": len(settings["prompts"]["guild"]), "max_chars": cap, "history": history},
            "directory": found.meta()}


def _writable_or_423(snap: Snapshot) -> None:
    if not snap.writable:
        raise refuse(423, "read_only", "Not saved: the policy file on the host is damaged or was written by a newer "
                                       "plugin version, so it is read-only. Every server stays paused until it is "
                                       "repaired.")


def _guild_or_404(req: WebRequest, snap: Snapshot, bot: str, guild: str, *, configured: bool) -> Optional[dict]:
    _writable_or_423(snap)
    stored = snap.guild(bot, guild)
    if configured and stored is None:
        raise refuse(404, "not_configured", "This server has no settings yet. Next step: activate the policy for it.")
    return stored


def _after_write(req: WebRequest, bot: str, guild: str) -> dict[str, Any]:
    snap = store_for(req.home).read()
    return guild_body(req, snap, bot, guild)


# ── read routes ──────────────────────────────────────────────────────────────────────────────────


def get_meta(req: WebRequest) -> WebResponse:
    snap = store_for(req.home).read()
    return ok({"plugin_version": __version__, "runtime_api": RUNTIME_API, "api": API_VERSION,
               "process": req.process, "surface": req.surface, "profile": req.backend.profile_of(req.home),
               "hermes_version": hermes.hermes_version(), "mode": _mode(req, snap), "conflict": None,
               "features": {"guidance_ops": callable(getattr(ops, "set_guidance", None)),
                            "routing_ops": ROUTING_FROM_PAGE,
                            "member_search": settings_io.member_search_mode(req.backend) != "off",
                            "rpc": req.process != "gateway" and hermes.rpc_available()}})


def _protection(status: Optional[dict]) -> dict[str, Any]:
    if not isinstance(status, dict):
        return {"status_at": None, "pid": None, "hermes_version": None, "armed_profiles": [],
                "security_degraded": [], "comfort_fallbacks": 0}
    interventions = status.get("interventions") if isinstance(status.get("interventions"), dict) else {}
    degraded, comfort = [], 0
    for name, result in sorted(interventions.items()):
        if not isinstance(result, dict) or result.get("outcome") not in ("fallback", "off"):
            continue
        if result.get("klass") == "S":
            degraded.append({"id": name, "outcome": result.get("outcome"), "reason": result.get("reason", "")})
        else:
            comfort += 1
    armed = status.get("armed") if isinstance(status.get("armed"), dict) else {}
    return {"status_at": status.get("written_at"), "pid": status.get("pid"),
            "hermes_version": status.get("hermes_version"),
            "armed_profiles": sorted(str(v.get("profile")) for v in armed.values() if isinstance(v, dict)),
            "security_degraded": degraded, "comfort_fallbacks": comfort}


_STATE_ORDER = {"active": 0, "paused": 1, "unbound": 2}


def get_overview(req: WebRequest) -> WebResponse:
    snap = store_for(req.home).read()
    cap = settings_io.prompt_cap(req.backend)
    count, readable = settings_io.operator_count(req.backend)
    live = _live_bots(req.home)
    bots = []
    for bot in known_bots(req, snap):
        found = bot_dir(req, bot)
        names = found.names()
        configured = (snap.data.get("bots", {}).get(bot) or {}).get("guilds", {})
        rows = []
        for guild_id in sorted(set(configured) | set(names)):
            stored = snap.guild(bot, guild_id)
            settings = stored or schema.guild_defaults()
            rows.append({"id": guild_id, "name": names.get(guild_id) or None,
                         "state": snap.guild_state(bot, guild_id), "configured": stored is not None,
                         "rev": snap.guild_rev(bot, guild_id),
                         "updated": {**who(settings.get("updated_by"), _last_via(snap, bot, guild_id)),
                                     "at": settings.get("updated_at")},
                         "summary": _summary(settings, cap)})
        rows.sort(key=lambda r: (_STATE_ORDER.get(r["state"], 3), (r["name"] or "").lower(), r["id"]))
        online = bot in live or (found.source in ("live", "rpc")) or (found.source == "snapshot" and not found.stale)
        bots.append({"id": bot, "name": found.bot_name, "online": bool(online),
                     "owner_profile": req.backend.profile_of(req.home), "directory": found.meta(), "guilds": rows})
    next_step = None
    if not bots:
        next_step = ("No Discord bot has connected in this profile yet. Enable the Discord platform for this "
                     "profile and start the Hermes gateway: the bot and its servers appear here once it is online. "
                     "Operators can be set already, under Operators.")
    elif not any(b["guilds"] for b in bots):
        next_step = ("The bot is not in any server yet. Invite it to a Discord server; the server then appears here "
                     "and the policy can be activated for it.")
    return ok({"profile": req.backend.profile_of(req.home), "mode": _mode(req, snap), "process": req.process,
               "store": {"state": snap.state, "error": redact_paths(snap.error),
                         "problems": redact_paths(list(snap.problems))},
               "operators": {"count": count, "readable": readable}, "bots": bots,
               "protection": _protection(gateway_status(req.home)),
               "other_profiles": _other_profiles(req, {b["id"] for b in bots}), "next_step": next_step})


def _other_profiles(req: WebRequest, mine: set) -> list[dict[str, Any]]:
    """Bots another Hermes profile owns (read only: that profile configures them). Ownership comes from the
    gateway's status entries, which name each bot's owner home; owners that are no profile are left out."""
    rows = [r for r in (req.backend.profiles() or []) if r.get("path") and r.get("name")]
    names = {hermes.home_key(r["path"]): str(r["name"]) for r in rows}
    current = hermes.home_key(req.home)
    owned: dict[str, set] = {}
    for home in [req.home] + [Path(r["path"]) for r in rows if hermes.home_key(r["path"]) != current]:
        for bot, owner in status_bots(home).items():
            key = hermes.home_key(owner)
            if key != current and key in names and bot not in mine:
                owned.setdefault(names[key], set()).add(bot)
    return [{"profile": name, "bots": sorted(bots)} for name, bots in sorted(owned.items())]


def get_schema(req: WebRequest) -> WebResponse:
    return ok({"sections": catalog.sections(), "states": list(schema.STATES),
               "archive_minutes": list(schema.ARCHIVE_MINUTES), "groupings": list(schema.GROUPINGS),
               "manager_modes": list(schema.MANAGER_MODES), "placeholders": list(prompts.PLACEHOLDERS),
               "lanes": list(prompts.LANES), "modes": list(prompts.MODES),
               "limits": {"guidance_hard": schema.HARD_PROMPT_LIMIT,
                          "guidance_max": settings_io.prompt_cap(req.backend)}})


def get_guild(req: WebRequest, bot: str, guild: str) -> WebResponse:
    _ids_or_404(bot, guild)
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    if snap.guild(bot, guild) is None and guild not in bot_dir(req, bot).names():
        raise refuse(404, "not_found", "This server is not known for this bot.")
    return ok(guild_body(req, snap, bot, guild))


def get_export(req: WebRequest, bot: str, guild: str) -> WebResponse:
    _ids_or_404(bot, guild)
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    if snap.state == "corrupt":
        raise refuse(423, "read_only", "The policy file is damaged; there is nothing reliable to export.")
    _guild_or_404(req, snap, bot, guild, configured=True)
    return ok(ops.export(snap, bot, guild))


def get_settings(req: WebRequest) -> WebResponse:
    state = settings_io.read(req.backend)
    return ok({**state, "keys": settings_io.keys()})


_PATH_KEYS = frozenset({"owner_home", "store", "plugin_dir", "home", "path"})
_ABS_PATH = re.compile(r"(?:[A-Za-z]:)?(?:[\\/][^\s\\/:'\"]+){2,}")


def redact_paths(value: Any) -> Any:
    """Host paths left out of answers (the page needs none): known path keys and absolute paths in text."""
    if isinstance(value, dict):
        return {k: ("(host path)" if k in _PATH_KEYS and isinstance(v, str) else redact_paths(v))
                for k, v in value.items()}
    if isinstance(value, list):
        return [redact_paths(v) for v in value]
    if isinstance(value, str):
        return _ABS_PATH.sub(lambda m: "…/" + re.split(r"[\\/]", m.group(0))[-1], value)
    return value


def _status_for(home: Path, status: Any) -> Any:
    """The gateway's status as this profile sees it: its tables keyed by profile name instead of home path,
    and only the bots this profile owns (the gateway writes its process-wide lists into every profile)."""
    if not isinstance(status, dict):
        return status
    out = dict(status)
    for name in ("armed", "instances"):
        table = status.get(name)
        if isinstance(table, dict):
            rekeyed: dict[str, Any] = {}
            for index, value in enumerate(table.values()):
                label = str(value.get("profile") or "") if isinstance(value, dict) else ""
                rekeyed[label if label and label not in rekeyed else f"profile {index + 1}"] = value
            out[name] = rekeyed
    bots = status.get("bots")
    if isinstance(bots, dict):
        key = hermes.home_key(home)
        out["bots"] = {b: v for b, v in bots.items() if isinstance(v, dict) and isinstance(v.get("owner_home"), str)
                       and v["owner_home"] and hermes.home_key(v["owner_home"]) == key}
    return out


def get_status(req: WebRequest) -> WebResponse:
    status = redact_paths(_status_for(req.home, gateway_status(req.home)))
    page = redact_paths(_read_json(hermes.plugin_data_dir(req.home) / "page.json"))
    interventions = []
    if isinstance(status, dict) and isinstance(status.get("interventions"), dict):
        for name, result in sorted(status["interventions"].items()):
            if isinstance(result, dict):
                interventions.append({"id": name, "klass": result.get("klass"), "outcome": result.get("outcome"),
                                      "reason": result.get("reason", ""), "probe": result.get("probe", ""),
                                      "at": result.get("at")})
    protection = _protection(status)
    return ok({"status": status, "page": page, "interventions": interventions,
               "armed_profiles": protection["armed_profiles"],
               "at": status.get("written_at") if isinstance(status, dict) else None,
               "pid": status.get("pid") if isinstance(status, dict) else None,
               "plugin_version": status.get("plugin_version") if isinstance(status, dict) else None,
               "hermes_version": status.get("hermes_version") if isinstance(status, dict) else None})


def get_status_history(req: WebRequest) -> WebResponse:
    data = _read_json(hermes.plugin_data_dir(req.home) / "status-history.json")
    entries = data.get("entries") if isinstance(data, dict) else None
    return ok({"entries": [e for e in entries if isinstance(e, dict)][-100:] if isinstance(entries, list) else []})


def get_audit(req: WebRequest) -> WebResponse:
    snap = store_for(req.home).read()
    bot, guild = req.query.get("bot"), req.query.get("guild")
    try:
        limit = max(1, min(int(req.query.get("limit", "200")), schema.AUDIT_RING))
    except ValueError:
        limit = 200
    entries = []
    for entry in reversed(snap.data.get("audit", [])):
        if bot and entry.get("bot") != bot or guild and entry.get("guild") != guild:
            continue
        label = who(entry.get("by"), entry.get("via"))
        entries.append({"at": entry.get("at"), "bot": entry.get("bot"), "guild": entry.get("guild"),
                        "field": entry.get("field"), "by": label["by"], "label": label["label"], "via": label["via"]})
        if len(entries) >= limit:
            break
    body: dict[str, Any] = {"entries": entries}
    if bot and guild and snap.guild(bot, guild):
        current = snap.guild(bot, guild)["prompts"]["guild"]
        body["guidance"] = [{"sha256": h["sha256"], **who(h["by"]), "at": h["at"], "chars": len(h["text"]),
                             "diff": "".join(list(difflib.unified_diff(
                                 h["text"].splitlines(keepends=True), current.splitlines(keepends=True),
                                 fromfile="this version", tofile="current", n=1))[:400])}
                            for h in reversed(snap.guild(bot, guild)["prompts"]["history"])]
    return ok(body)


# ── write routes ─────────────────────────────────────────────────────────────────────────────────


def patch_guild(req: WebRequest, bot: str, guild: str) -> WebResponse:
    _ids_or_404(bot, guild)
    body = _body(req, {"expected_rev", "section", "changes"}, {"expected_rev", "section", "changes"})
    rev = _rev(body)
    section, changes = body["section"], body["changes"]
    setting = perms.SETTINGS.get(section) if isinstance(section, str) else None
    if setting is None or not setting.fields:
        raise refuse(422, "invalid", f"{section!r} is not a section of settings.", field_name="section")
    if not isinstance(changes, dict) or not changes:
        raise refuse(422, "invalid", "No settings given.", field_name="changes")
    try:
        perms.check_write(guard.host_actor(), section, list(changes))
    except perms.Refused:
        stray = sorted(set(changes) - set(setting.fields))
        raise refuse(422, "invalid", f"{', '.join(stray) or section} is not part of {setting.label}; nothing was saved.",
                     field_name=stray[0] if stray else "section")
    if set(changes) & catalog.HIDDEN:
        raise refuse(422, "invalid", "The guidance history is written by the guidance operations only.",
                     field_name="prompts.history")
    if "prompts.guild" in changes:
        raise refuse(422, "invalid", "Save the server guidance with the guidance editor (it keeps the history).",
                     field_name="prompts.guild")
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    stored = _guild_or_404(req, snap, bot, guild, configured=True)
    found = guild_dir(req, bot, guild)
    findings = checks.environment(changes, environment_for(req, stored, found))
    if checks.has_errors(findings):
        errors = [f.as_dict() for f in findings if f.level == checks.ERROR]
        raise refuse(422, "invalid", errors[0]["message"], field_name=errors[0]["field"],
                     findings=[f.as_dict() for f in findings])
    cap = settings_io.prompt_cap(req.backend)
    _write(req, bot, guild, lambda: ops.update_fields(store_for(req.home), bot, guild, changes, by="dashboard",
                                                      expected_rev=rev, prompt_cap=cap))
    return ok({"guild": _after_write(req, bot, guild), "warnings": [f.as_dict() for f in findings]})


def post_state(req: WebRequest, bot: str, guild: str) -> WebResponse:
    _ids_or_404(bot, guild)
    body = _body(req, {"expected_rev", "state", "purge"}, {"expected_rev", "state"})
    rev = _rev(body)
    state, purge = body["state"], body.get("purge", False)
    if state not in schema.STATES or not isinstance(purge, bool):
        raise refuse(422, "invalid", "state must be active, paused or unbound.", field_name="state")
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    _writable_or_423(snap)
    stored = snap.guild(bot, guild)
    store = store_for(req.home)
    host = guard.host_actor()
    if purge:
        perms.require(host, perms.PURGE)
        if stored is None:
            raise refuse(404, "not_configured", "This server has no settings to delete.")
        _write(req, bot, guild, lambda: ops.purge_checked(store, bot, guild, by="dashboard", expected_rev=rev))
        notify.forget_guidance(req.home, bot, guild)      # no held copy of a deleted server's guidance
        return ok({"guild": None, "purged": True})
    old = stored["state"] if stored else "unbound"
    if stored is None and state != "active":
        raise refuse(404, "not_configured", "This server has no settings yet. Next step: activate the policy for it.")
    if stored is not None and old == state:
        return ok({"guild": guild_body(req, snap, bot, guild)})
    key = perms.transition_setting(old, state)
    if key is None:
        raise refuse(422, "invalid", f"A server cannot go from {old} to {state}.", field_name="state")
    perms.require(host, key)
    if state == "active" and old == "unbound":
        _write(req, bot, guild, lambda: ops.activate(store, bot, guild, by="dashboard", expected_rev=rev))
    else:
        _write(req, bot, guild, lambda: ops.set_state(store, bot, guild, state, by="dashboard", expected_rev=rev))
    return ok({"guild": _after_write(req, bot, guild)})


def post_guidance(req: WebRequest, bot: str, guild: str) -> WebResponse:
    _ids_or_404(bot, guild)
    body = _body(req, {"expected_rev", "text"}, {"expected_rev", "text"})
    rev = _rev(body)
    text = body["text"]
    if not isinstance(text, str):
        raise refuse(422, "invalid", "The guidance must be text.", field_name="prompts.guild")
    return _write_guidance(req, bot, guild, rev, text)


def _write_guidance(req: WebRequest, bot: str, guild: str, rev: int, text: str) -> WebResponse:
    """The one way the guidance text is written (save and reset): the shared ``set_guidance`` keeps the
    history; without it the text alone is saved."""
    perms.check_write(guard.host_actor(), "guidance", ["prompts.guild"])
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    _guild_or_404(req, snap, bot, guild, configured=True)
    cap = settings_io.prompt_cap(req.backend)
    store = store_for(req.home)
    shared = getattr(ops, "set_guidance", None)
    if callable(shared):
        _write(req, bot, guild, lambda: _call(shared, store, bot, guild, text, by="dashboard", via="dashboard",
                                              expected_rev=rev, prompt_cap=cap))
    else:
        _write(req, bot, guild, lambda: ops.update_fields(store, bot, guild, {"prompts.guild": text},
                                                          by="dashboard", expected_rev=rev, prompt_cap=cap))
    return ok({"guild": _after_write(req, bot, guild), "history": callable(shared)})


def _shared_op(name: str) -> Callable:
    fn = getattr(ops, name, None)
    if not callable(fn):
        raise refuse(501, "not_available", "This version of the plugin cannot do that yet.")
    return fn


def post_guidance_restore(req: WebRequest, bot: str, guild: str) -> WebResponse:
    _ids_or_404(bot, guild)
    body = _body(req, {"expected_rev", "sha256"}, {"expected_rev", "sha256"})
    rev = _rev(body)
    digest = body["sha256"]
    if not isinstance(digest, str) or not _SHA256.fullmatch(digest):
        raise refuse(422, "invalid", "Not a version of the guidance.", field_name="sha256")
    fn = _shared_op("restore_guidance")
    perms.check_write(guard.host_actor(), "guidance", ["prompts.guild", "prompts.history"])
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    stored = _guild_or_404(req, snap, bot, guild, configured=True)
    finder = getattr(ops, "find_guidance_version", None)
    if callable(finder):
        # The shared operation refuses an unknown version as a validation error; for the page it is a version
        # that has left the history (ring of ten), which is a 404. Checked on the state the page read; should
        # it change before the write, the revision check answers 409 first.
        try:
            finder(stored, digest)
        except SchemaError:
            raise refuse(404, "not_found", "That version is no longer in the history.") from None
    cap = settings_io.prompt_cap(req.backend)
    store = store_for(req.home)
    _write(req, bot, guild, lambda: _call(fn, store, bot, guild, digest, by="dashboard", via="dashboard",
                                          expected_rev=rev, prompt_cap=cap))
    return ok({"guild": _after_write(req, bot, guild)})


def post_reset(req: WebRequest, bot: str, guild: str) -> WebResponse:
    _ids_or_404(bot, guild)
    body = _body(req, {"expected_rev", "section", "paths"}, {"expected_rev"})
    rev = _rev(body)
    section = body.get("section")
    setting = perms.SETTINGS.get(section) if isinstance(section, str) else None
    if setting is None or not setting.fields:
        raise refuse(422, "invalid", "Name the section to reset.", field_name="section")
    paths = body.get("paths")
    if paths is None:
        paths = [p for p in setting.fields if p not in catalog.HIDDEN]
    if not isinstance(paths, list) or not paths or not all(isinstance(p, str) for p in paths):
        raise refuse(422, "invalid", "paths must be a list of settings.", field_name="paths")
    if set(paths) & catalog.HIDDEN:
        raise refuse(422, "invalid", "The guidance history cannot be reset here.", field_name="paths")
    if "prompts.guild" in paths:
        # Resetting the guidance is saving an empty guidance (SETTINGS-PAGE §5.6): the old text stays in the history.
        if paths != ["prompts.guild"]:
            raise refuse(422, "invalid", "Reset the guidance on its own.", field_name="paths")
        return _write_guidance(req, bot, guild, rev, "")
    try:
        perms.check_write(guard.host_actor(), section, paths)
    except perms.Refused as exc:
        raise refuse(422, "invalid", str(exc), field_name="paths")
    fn = _shared_op("reset_fields")
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    _guild_or_404(req, snap, bot, guild, configured=True)
    cap = settings_io.prompt_cap(req.backend)
    store = store_for(req.home)
    _write(req, bot, guild, lambda: _call(fn, store, bot, guild, list(paths), by="dashboard", via="dashboard",
                                          expected_rev=rev, prompt_cap=cap))
    return ok({"guild": _after_write(req, bot, guild)})


# Choosing a server's Hermes profile is more than a store write: the routing flow (dsp.routing.apply:
# eligibility, Hermes' profile_routes, the live table, verification) does it. Until the page runs that flow,
# it shows the choice and leaves setting it to Discord (/policy). Never call ops.set_routing alone.
ROUTING_FROM_PAGE = False


def post_routing(req: WebRequest, bot: str, guild: str, adopt: bool = False) -> WebResponse:
    _ids_or_404(bot, guild)
    body = _body(req, {"expected_rev"} if adopt else {"expected_rev", "profile"}, {"expected_rev"})
    if not ROUTING_FROM_PAGE:
        raise refuse(501, "not_available", "The Hermes profile and model of a server are chosen in Discord for now "
                                           "(/policy, Profile and Model). This page shows them.")
    rev = _rev(body)
    fn = _shared_op("adopt_routing" if adopt else "set_routing")
    if "profile" in perms.SETTINGS:
        perms.require(guard.host_actor(), "profile")
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    _guild_or_404(req, snap, bot, guild, configured=True)
    store = store_for(req.home)
    if adopt:
        result = _write(req, bot, guild, lambda: _call(fn, store, bot, guild, by="dashboard", via="dashboard",
                                                       expected_rev=rev))
    else:
        profile = body.get("profile")
        if profile is not None and not isinstance(profile, str):
            raise refuse(422, "invalid", "profile must be a profile name or null.", field_name="profile")
        result = _write(req, bot, guild, lambda: _call(fn, store, bot, guild, profile, by="dashboard",
                                                       via="dashboard", expected_rev=rev))
    extra = result if isinstance(result, dict) else {}
    return ok({"guild": _after_write(req, bot, guild), "result": json.loads(json.dumps(extra, default=str))})


def _settings_only(document: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in document.items() if k not in perms.BOOKKEEPING}


def post_import(req: WebRequest, bot: str, guild: str) -> WebResponse:
    _ids_or_404(bot, guild)
    body = _body(req, {"expected_rev", "document"}, {"expected_rev", "document"})
    rev = _rev(body)
    document = body["document"]
    payload = document
    if isinstance(document, dict) and "bots" in document:
        guilds = [g for b in (document.get("bots") or {}).values() if isinstance(b, dict)
                  for g in ((b.get("guilds") or {}).values() if isinstance(b.get("guilds"), dict) else [])]
        if len(guilds) != 1:
            raise refuse(422, "invalid", "The file must hold exactly one server's settings.", field_name="document")
        payload = guilds[0]
    if not isinstance(payload, dict):
        raise refuse(422, "invalid", "Not a settings file of this plugin.", field_name="document")
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    _writable_or_423(snap)
    current = snap.guild(bot, guild)
    # An import replaces settings, never the lifecycle (activation stays an explicit step, TRANSITIONS) or the
    # guidance history (written by the guidance operations only).
    payload = copy.deepcopy(payload)
    payload["state"] = current["state"] if current else "unbound"
    if payload.get("prompts") is None:
        payload["prompts"] = {}             # a file without the section still must not empty the history
    if isinstance(payload["prompts"], dict):
        payload["prompts"]["history"] = copy.deepcopy(current["prompts"]["history"]) if current else []
    host = guard.host_actor()
    for path in checks.flatten({k: v for k, v in payload.items() if k not in perms.BOOKKEEPING}):
        for key in perms.owners_of(path):
            perms.require(host, key)
    found = guild_dir(req, bot, guild)
    incoming = {k: v for k, v in payload.items() if k not in perms.BOOKKEEPING}
    try:
        clean, _ = schema.validate_guild(incoming, strict=True, prompt_cap=settings_io.prompt_cap(req.backend))
    except SchemaError as exc:
        raise refuse(422, "invalid", f"Not imported: {exc}.", field_name=_schema_field(str(exc)))
    findings = checks.environment(_settings_paths(clean), environment_for(req, clean, found))
    if checks.has_errors(findings):
        errors = [f.as_dict() for f in findings if f.level == checks.ERROR]
        raise refuse(422, "invalid", errors[0]["message"], field_name=errors[0]["field"],
                     findings=[f.as_dict() for f in findings])
    warnings = [f.as_dict() for f in findings]
    if current is not None and snap.guild_rev(bot, guild) == rev and _settings_only(clean) == _settings_only(current):
        # The file holds exactly what is in effect: no new revision, no audit entry, and the page says so.
        return ok({"guild": guild_body(req, snap, bot, guild), "warnings": warnings, "changed": False})
    cap = settings_io.prompt_cap(req.backend)
    store = store_for(req.home)
    _write(req, bot, guild, lambda: ops.import_guild_checked(store, bot, guild, payload, by="dashboard",
                                                             prompt_cap=cap, expected_rev=rev, via="dashboard"))
    return ok({"guild": _after_write(req, bot, guild), "warnings": warnings, "changed": True})


def _sections(text: str, capability: str, mode: str) -> list[dict[str, str]]:
    marks = [("frame", "Operator frame", 0)]
    at_cap = text.find(capability)
    if at_cap >= 0:
        marks.append(("capability", "Capability lane", at_cap))
        at_conv = text.find(f"[Conversation lane: {mode}]", at_cap + len(capability))
        if at_conv >= 0:
            marks.append(("conversation", "Conversation lane", at_conv))
            at_guide = text.find(GUIDANCE_HEADER, at_conv)
            if at_guide >= 0:
                marks.append(("guidance", "Server guidance", at_guide))
    out = []
    for index, (kind, title, start) in enumerate(marks):
        end = marks[index + 1][2] if index + 1 < len(marks) else len(text)
        out.append({"kind": kind, "title": title, "text": text[start:end].strip("\n")})
    return out


def post_preview(req: WebRequest) -> WebResponse:
    body = _body(req, {"bot", "guild", "lane", "mode", "draft", "channel"}, {"bot", "guild", "lane", "mode"})
    bot, guild_id, lane, mode = str(body["bot"]), str(body["guild"]), body["lane"], body["mode"]
    _ids_or_404(bot, guild_id)
    if lane not in prompts.LANES:
        raise refuse(422, "invalid", "lane must be public or operator.", field_name="lane")
    if mode not in prompts.MODES:
        raise refuse(422, "invalid", "mode must be ambient, direct or worklog.", field_name="mode")
    draft = body.get("draft")
    if draft is not None and (not isinstance(draft, str) or len(draft) > PREVIEW_DRAFT_LIMIT):
        raise refuse(422, "invalid", "The draft must be text of at most 64 KB.", field_name="draft")
    snap = store_for(req.home).read()
    _bot_or_404(req, bot, snap)
    guild = copy.deepcopy(snap.guild(bot, guild_id) or schema.guild_defaults())
    if draft is not None:
        guild["prompts"]["guild"] = draft
    settings = settings_io.assemble_settings(req.backend)
    found = guild_dir(req, bot, guild_id)
    bot_found = bot_dir(req, bot)
    channel = str(body.get("channel") or "")
    channel_name = next((c.get("name") for c in found.channels if str(c.get("id")) == channel), "") if channel else ""
    capabilities = {name: policy_lanes.capability_lane_text(SimpleNamespace(
        is_operator=name == "operator", lanes=policy_lanes.PublicLanes.from_guild(guild))) for name in prompts.LANES}
    budget = prompts.budget_from_spill(req.backend.spill_config())
    assembled = prompts.assemble(settings, guild, lane=lane, mode=mode, capability=capabilities[lane], budget=budget,
                                 server_name=found.name or "", channel_name=channel_name or "", channel_id=channel,
                                 bot_display_name=bot_found.bot_name or "")
    check = prompts.check_guidance(guild["prompts"]["guild"], settings, guild, budget=budget, capabilities=capabilities)
    return ok({"text": assembled.text, "sections": _sections(assembled.text, capabilities[lane].strip("\n"), mode),
               "length": assembled.length, "budget": assembled.budget, "over_budget": assembled.over_budget,
               "guidance": assembled.guidance, "guidance_chars": assembled.guidance_chars,
               "guidance_chars_used": assembled.guidance_chars_used, "truncated_by": list(assembled.truncated_by),
               "placeholders": dict(assembled.placeholders),
               "unknown_placeholders": list(assembled.unknown_placeholders), "warnings": list(assembled.warnings),
               "check": {"chars": check.chars, "max_chars": check.max_chars, "needed": check.needed,
                         "room": check.room, "fits": check.fits, "problems": list(check.problems),
                         "unknown_placeholders": list(check.unknown_placeholders)}})


def put_settings(req: WebRequest) -> WebResponse:
    body = _body(req, {"expected_digest", "values"}, {"expected_digest", "values"})
    if isinstance(body["values"], dict) and "operators" in body["values"]:
        perms.require(guard.host_actor(), "operators")
    try:
        result = settings_io.write(req.backend, req.plugin_dir, body["expected_digest"], body["values"])
    except settings_io.SettingsError as exc:
        raise refuse(422, "invalid", str(exc), field_name=exc.field)
    except settings_io.PartlySaved as exc:
        raise refuse(423, "partly_saved", f"Only part was saved: {exc}. Reload to see what is in effect.",
                     written=exc.written)
    except settings_io.ConflictingSettings as exc:
        raise refuse(409, "conflict", "Not saved: the settings were changed elsewhere after this page loaded them. "
                                      "Reload to see them.", digest=exc.digest)
    except PermissionError as exc:
        raise refuse(423, "read_only", f"Not saved: {exc}" if str(exc) else
                     "Not saved: this Hermes configuration is managed and cannot be changed here.")
    except ValueError as exc:  # Hermes' own schema check refused a value
        message = str(exc)
        match = re.match(r"'([A-Za-z0-9_]+)'", message)
        raise refuse(422, "invalid", f"Not saved: {message}", field_name=match.group(1) if match else None)
    return ok(result)


# ── dispatch ─────────────────────────────────────────────────────────────────────────────────────

_ID = r"([^/]+)"
ROUTES: list[tuple[str, re.Pattern, Callable[..., WebResponse]]] = [
    ("GET", re.compile(r"/meta"), get_meta),
    ("GET", re.compile(r"/overview"), get_overview),
    ("GET", re.compile(r"/schema"), get_schema),
    ("GET", re.compile(rf"/guilds/{_ID}/{_ID}"), get_guild),
    ("PATCH", re.compile(rf"/guilds/{_ID}/{_ID}"), patch_guild),
    ("POST", re.compile(rf"/guilds/{_ID}/{_ID}/state"), post_state),
    ("POST", re.compile(rf"/guilds/{_ID}/{_ID}/guidance"), post_guidance),
    ("POST", re.compile(rf"/guilds/{_ID}/{_ID}/guidance/restore"), post_guidance_restore),
    ("POST", re.compile(rf"/guilds/{_ID}/{_ID}/reset"), post_reset),
    ("POST", re.compile(rf"/guilds/{_ID}/{_ID}/routing"), post_routing),
    ("POST", re.compile(rf"/guilds/{_ID}/{_ID}/routing/adopt"), lambda req, b, g: post_routing(req, b, g, True)),
    ("GET", re.compile(rf"/guilds/{_ID}/{_ID}/export"), get_export),
    ("POST", re.compile(rf"/guilds/{_ID}/{_ID}/import"), post_import),
    ("POST", re.compile(r"/preview"), post_preview),
    ("GET", re.compile(r"/settings"), get_settings),
    ("PUT", re.compile(r"/settings"), put_settings),
    ("GET", re.compile(r"/status"), get_status),
    ("GET", re.compile(r"/status/history"), get_status_history),
    ("GET", re.compile(r"/audit"), get_audit),
]
EXTRA_ROUTES: list[tuple[str, re.Pattern, Callable[..., WebResponse]]] = []


def add_routes(routes: list) -> None:
    for method, pattern, handler in routes:
        EXTRA_ROUTES.append((method, re.compile(pattern) if isinstance(pattern, str) else pattern, handler))


add_routes(sources.ROUTES)


def _heartbeat(req: WebRequest, mode: str) -> None:
    """``page.json``: where the settings page backend last answered (SETTINGS-PAGE §1.6, intervention P1)."""
    key = str(req.home)
    now = time.time()
    if now - _HEARTBEATS.get(key, 0.0) < HEARTBEAT_EVERY:
        return
    _HEARTBEATS[key] = now
    try:
        write_json_atomic(hermes.plugin_data_dir(req.home) / "page.json",
                          {"at": now, "process": req.process, "surface": req.surface, "pid": os.getpid(),
                           "plugin_version": __version__, "hermes_version": hermes.hermes_version(), "mode": mode})
    except OSError:
        logger.debug("ctx_discord_policy: could not write page.json", exc_info=True)


def handle(req: WebRequest) -> WebResponse:
    if req.backend is None:
        req.backend = DEFAULT_BACKEND
    method = req.method.upper()
    path = "/" + req.path.strip("/")
    allowed: list[str] = []
    for route_method, pattern, handler in ROUTES + EXTRA_ROUTES:
        match = pattern.fullmatch(path)
        if not match:
            continue
        if route_method != method and not (method == "HEAD" and route_method == "GET"):
            allowed.append(route_method)
            continue
        try:
            response = handler(req, *match.groups())
        except Refusal as refusal:
            response = refusal.response
        except perms.Refused as exc:
            response = error(422, "invalid", str(exc))
        except Exception:
            logger.exception("ctx_discord_policy: settings page route %s %s failed", method, path)
            response = error(500, "internal", "Something went wrong on the host. Details are in the Hermes log.")
        try:
            _heartbeat(req, _mode(req, store_for(req.home).read()))
        except Exception:
            pass
        return response
    if allowed:
        return error(405, "method", f"Use {', '.join(sorted(set(allowed)))} here.")
    return error(404, "not_found", "No such page.")
