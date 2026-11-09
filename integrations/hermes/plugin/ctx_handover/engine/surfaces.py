"""What the person sees: argument parsing, settings, confirmation, progress, final and error texts (layer L10;
DESIGN §11.6, §14.1, §14.4, §8.7).

Everything here is pure (no I/O, no Hermes): ``commands`` (slash and CLI) and ``agent_tool`` compose it with the
engine. One grammar serves ``/handover …`` and ``hermes handover …``; the parser never exits and reports every
problem as a sentence that says what to type instead. Every error text names the remedy (BX M8).
"""

from __future__ import annotations

import base64
import hashlib
import math
import re
import shlex
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from . import plan as planning
from .errors import HandoverError
from .model import (
    CommitAck,
    ContinueResult,
    GateAuth,
    GateMode,
    Mode,
    Options,
    PathMapEntry,
    Peer,
    PlanHints,
    Right,
)

MiB = 1024**2
GiB = 1024**3

DEFAULT_CONTINUE = "Continue where we left off."
LOCAL_PLATFORMS: frozenset[str] = frozenset({"local", "cli", "tui", "desktop"})
"""Platforms that are a person at this machine: ``local`` (gateway-hosted CLI/TUI/Desktop, LAB L2), ``cli``
(in-process classic CLI, ``cli.py:1287``), ``tui``/``desktop`` (``tui_gateway/agent_factory.py:29-33``). Everything
else (messaging adapters, ``api_server``, webhooks, unknown) is remote for R24."""
PAIR_PREFIX = "ctxh-pair:"
PAIR_TTL_MAX_S = 3600

# ---------------------------------------------------------------------------------------------------------
# Settings (DESIGN §14.4; plugin.yaml config_schema mirrors this table, a unit test keeps both identical)
# ---------------------------------------------------------------------------------------------------------


def _s(typ: str, default: Any = None, choices: Sequence[str] = (), env: str | None = None) -> dict:
    spec: dict[str, Any] = {"type": typ}
    if typ == "secret":
        spec["env"] = env
    else:
        spec["default"] = default
    if choices:
        spec["choices"] = tuple(choices)
    return spec


SETTINGS_SCHEMA: dict[str, dict] = {
    "inbound": _s("str", "auto", ("auto", "on", "off")),
    "listen_url": _s("str", ""),
    "peer_gate": _s("str", "session_token", ("session_token", "bearer", "basic_login")),
    "peer_token": _s("secret", env="CTX_HANDOVER_PEER_TOKEN"),
    "peer_username": _s("str", ""),
    "peer_password": _s("secret", env="CTX_HANDOVER_PEER_PASSWORD"),
    "peer_extra_headers": _s("dict", {}),
    "workspace_root": _s("str", "~/hermes-handover"),
    "allow_roots": _s("list", ["~/hermes-handover"]),
    "export_roots": _s("list", []),
    "max_session_mb": _s("int", 512),
    "max_file_mb": _s("int", 100),
    "max_extra_dir_mb": _s("int", 256),
    "max_hermes_file_mb": _s("int", 64),
    "max_files": _s("int", 200000),
    "max_untracked_files": _s("int", 200000),
    "max_total_gb": _s("float", 10),
    "confirm_above_mb": _s("int", 500),
    "max_request_mb": _s("int", 8),
    "block_cache_gb": _s("float", 5),
    "block_cache_days": _s("int", 3),
    "staging_ttl_hours": _s("int", 24),
    "lease_ttl_s": _s("int", 120),
    "participant_idle_timeout_min": _s("int", 15),
    "stall_timeout_min": _s("int", 30),
    "wait_timeout_min": _s("int", 30),
    "refine": _s("str", "auto", ("auto", "side_question", "llm", "off")),
    "refine_timeout_s": _s("int", 45),
    "refine_history_chars": _s("int", 60000),
    "refine_side_question_max_tokens": _s("int", 200000),
    "refine_auto_include": _s("bool", False),
    "extra_paths": _s("str", "ask", ("ask", "auto", "off")),
    "include_hermes_files": _s("bool", True),
    "model_mapping": _s("str", "auto", ("auto", "target_default")),
    "allow_remote_continue": _s("bool", False),
    "allow_agent_start": _s("bool", False),
    "accept_offers": _s("str", "ask", ("ask", "off")),
    "extra_ignore": _s("list", []),
    "tool_probe": _s("list", []),
    "ctx_persist": _s("str", "auto", ("auto", "off")),
    "ctx_category": _s("str", "handover"),
}

SECRET_ENVS = {k: v["env"] for k, v in SETTINGS_SCHEMA.items() if v["type"] == "secret"}
_HEADER_RE = re.compile(r"^[A-Za-z0-9-]{1,64}$")
_ENV_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]{0,127}$")


def _coerce(key: str, spec: dict, value: Any) -> Any:
    typ = spec["type"]
    default = spec.get("default")
    if typ == "str":
        if isinstance(value, bool) and spec.get("choices"):  # YAML 1.1 turns `on`/`off` into booleans
            value = "on" if value else "off"
        if not isinstance(value, str):
            return default
        if spec.get("choices") and value not in spec["choices"]:
            return default
        return value
    if typ == "int":
        return value if isinstance(value, int) and not isinstance(value, bool) and value >= 0 else default
    if typ == "float":
        ok = isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and value >= 0
        return float(value) if ok else default
    if typ == "bool":
        return value if isinstance(value, bool) else default
    if typ == "list":
        if not isinstance(value, list):
            return list(default)
        return [v for v in value if isinstance(v, str) and v.strip()]
    if typ == "dict":
        if not isinstance(value, Mapping):
            return dict(default)
        return {
            k: v
            for k, v in value.items()
            if isinstance(k, str) and isinstance(v, str) and _HEADER_RE.fullmatch(k) and _ENV_RE.fullmatch(v)
        }
    return default


def settings_from_config(config: Mapping | None) -> dict:
    """``plugins.entries.ctx_handover.settings`` merged over the defaults; a value of the wrong type or outside
    its choices falls back to the default (Hermes only warns at load, so the plugin must not trust it). Secret
    keys are not part of the result: their values live in ``.env`` under ``SECRET_ENVS``."""
    raw: Any = {}
    if isinstance(config, Mapping):
        entry = (
            ((config.get("plugins") or {}).get("entries") or {}).get("ctx_handover")
            if isinstance(config.get("plugins"), Mapping)
            and isinstance((config.get("plugins") or {}).get("entries"), Mapping)
            else None
        )
        if isinstance(entry, Mapping) and isinstance(entry.get("settings"), Mapping):
            raw = entry["settings"]
    out: dict[str, Any] = {}
    for key, spec in SETTINGS_SCHEMA.items():
        if spec["type"] == "secret":
            continue
        default = spec.get("default")
        if key in raw:
            out[key] = _coerce(key, spec, raw[key])
        else:
            out[key] = (
                list(default) if isinstance(default, list) else dict(default) if isinstance(default, dict) else default
            )
    return out


# ---------------------------------------------------------------------------------------------------------
# Grammar (DESIGN §14.1)
# ---------------------------------------------------------------------------------------------------------

VERBS: tuple[str, ...] = (
    "to",
    "take",
    "export",
    "import",
    "status",
    "peers",
    "pair",
    "reclaim",
    "cancel",
    "resume",
    "setup",
    "doctor",
    "identity",
    "gc",
    "logs",
    "help",
)
PEERS_SUBVERBS = ("list", "revoke", "remove", "rename", "show")
IDENTITY_SUBVERBS = ("show", "rotate")

# option -> (dest, kind); kinds: flag, value, multi, map, optval, float, rights, ttl, gate
_OPTIONS: dict[str, tuple[str, str]] = {
    "--dest": ("dest", "value"),
    "--map": ("maps", "map"),
    "--include": ("include", "multi"),
    "--include-secret": ("include_secret", "multi"),
    "--exclude": ("exclude", "multi"),
    "--no-workspace": ("no_workspace", "flag"),
    "--model": ("model", "value"),
    "--wait": ("wait", "flag"),
    "--continue": ("continue", "optval"),
    "--force": ("force", "flag"),
    "--dry-run": ("dry_run", "flag"),
    "--yes": ("yes", "flag"),
    "--max-total-gb": ("max_total_gb", "float"),
    "--latest": ("latest", "flag"),
    "--offer": ("offer", "value"),
    "--keep": ("keep", "flag"),
    "--for": ("for_peer", "value"),
    "--dir": ("as_dir", "flag"),
    "--trust-unsigned": ("trust_unsigned", "flag"),
    "--again": ("again", "flag"),
    "--name": ("name", "value"),
    "--rights": ("rights", "rights"),
    "--ttl": ("ttl_s", "ttl"),
    "--gate": ("gate", "gate"),
    "--allow-insecure": ("allow_insecure", "flag"),
    "--job": ("job", "value"),
    "--all": ("all", "flag"),
    "--json": ("json", "flag"),
    "--session": ("session", "value"),
}
_PLAN = {
    "--include",
    "--include-secret",
    "--exclude",
    "--no-workspace",
    "--wait",
    "--force",
    "--dry-run",
    "--yes",
    "--max-total-gb",
}
_TARGET = {"--dest", "--map", "--model", "--continue"}
_VERB_OPTIONS: dict[str, set[str]] = {
    "to": _PLAN | _TARGET | {"--session"},
    "take": _PLAN | _TARGET | {"--latest", "--offer"},
    "export": _PLAN | {"--keep", "--for", "--dir", "--session"},
    "import": {"--dest", "--map", "--model", "--continue", "--trust-unsigned", "--again", "--yes", "--wait"},
    "status": set(),
    "peers": set(),
    "pair": {"--name", "--rights", "--ttl", "--gate", "--allow-insecure"},
    "reclaim": {"--force"},
    "cancel": set(),
    "resume": set(),
    "setup": set(),
    "doctor": set(),
    "identity": set(),
    "gc": {"--job", "--all"},
    "logs": set(),
    "help": set(),
}
_POSITIONALS: dict[str, tuple[int, int]] = {
    "to": (1, 1),
    "take": (0, 2),
    "export": (0, 1),
    "import": (1, 1),
    "status": (0, 1),
    "peers": (0, 3),
    "pair": (0, 1),
    "reclaim": (0, 1),
    "cancel": (0, 1),
    "resume": (0, 1),
    "setup": (0, 0),
    "doctor": (0, 0),
    "identity": (0, 1),
    "gc": (0, 0),
    "logs": (1, 1),
    "help": (0, 1),
}
USAGE: dict[str, str] = {
    "to": "/handover to <peer> [--dest P] [--map SRC=DST] [--include P] [--include-secret P] [--exclude GLOB] "
    '[--no-workspace] [--model M] [--wait] [--continue ["prompt"]] [--force] [--dry-run] [--yes] '
    "[--session ID (CLI)]",
    "take": "/handover take [<peer>] [<session-id> | --latest | --offer <id>] [same options as to]",
    "export": "/handover export [<path>] [--keep] [--for <peer>] [--dir] [--include P] [--exclude GLOB] … "
    "[--session ID (CLI)]",
    "import": '/handover import <bundle> [--dest P] [--trust-unsigned] [--again] [--continue ["prompt"]]',
    "status": "/handover status [<job>]",
    "peers": "/handover peers [revoke <name> | remove <name> | rename <old> <new> | show <name>]",
    "pair": "/handover pair [--name N] [--rights push,pull] [--ttl 10m]   (on the gateway that receives)\n"
    "/handover pair <ctxh-pair:…> [--name N] [--gate session_token|bearer|basic_login] [--allow-insecure]"
    "   (on the device that dials)",
    "reclaim": "/handover reclaim [<session-id>] [--force]",
    "cancel": "/handover cancel [<job>]",
    "resume": "/handover resume [<job>]",
    "setup": "/handover setup",
    "doctor": "/handover doctor",
    "identity": "/handover identity [show | rotate]",
    "gc": "/handover gc [--job <id> | --all]",
    "logs": "/handover logs <job>",
    "help": "/handover help [<command>]",
}


@dataclass(frozen=True)
class Parsed:
    """One parsed command line: ``verb``, normalised ``args`` and the problems found (empty = valid)."""

    verb: str
    args: dict = field(default_factory=dict)
    errors: tuple[str, ...] = ()


def _parse_ttl(text: str) -> int | None:
    m = re.fullmatch(r"(\d{1,5})\s*([smh]?)", text.strip().lower())
    if not m:
        return None
    n = int(m.group(1))
    return {"s": n, "m": n * 60, "h": n * 3600, "": n * 60}[m.group(2)]


def _parse_rights(text: str) -> frozenset[Right] | None:
    parts = [p.strip().lower() for p in text.split(",") if p.strip()]
    if not parts or any(p not in {"push", "pull"} for p in parts):
        return None
    return frozenset(Right(p) for p in parts)


def _parse_gate(text: str) -> GateMode | None:
    norm = text.strip().lower().replace("-", "_")
    try:
        return GateMode(norm)
    except ValueError:
        return None


def parse(raw_args: str) -> Parsed:
    """Parse the text after ``/handover`` (or the CLI argv joined with ``shlex.join``). Never raises, never exits."""
    errors: list[str] = []
    try:
        tokens = shlex.split(raw_args or "", posix=True)
    except ValueError as exc:
        return Parsed("help", {}, (f"Could not read the command line ({exc}). Check the quotes.",))
    if not tokens:
        return Parsed("help", {"topic": None})
    verb = tokens[0].lower()
    if verb not in VERBS:
        return Parsed("help", {"topic": None}, (f"Unknown command {tokens[0]!r}. Commands: {', '.join(VERBS[:-1])}.",))
    allowed = _VERB_OPTIONS[verb] | {"--json"}
    args: dict[str, Any] = {"positionals": []}
    i = 1
    while i < len(tokens):
        tok = tokens[i]
        i += 1
        if not tok.startswith("--") or tok == "--":
            if tok == "--":
                args["positionals"].extend(tokens[i:])
                break
            args["positionals"].append(tok)
            continue
        name, eq, inline = tok.partition("=")
        spec = _OPTIONS.get(name)
        if spec is None or name not in allowed:
            errors.append(f"{name} is not an option of '{verb}'. Usage: {USAGE[verb]}")
            continue
        dest, kind = spec
        if kind == "flag":
            if eq:
                errors.append(f"{name} takes no value.")
            args[dest] = True
            continue
        if kind == "optval":
            if eq:
                value = inline
            elif i < len(tokens) and not tokens[i].startswith("--"):
                value = tokens[i]
                i += 1
            else:
                value = DEFAULT_CONTINUE
            args[dest] = value if value.strip() else DEFAULT_CONTINUE
            continue
        if eq:
            value = inline
        elif i < len(tokens):
            value = tokens[i]
            i += 1
        else:
            errors.append(f"{name} needs a value. Usage: {USAGE[verb]}")
            continue
        if kind == "value":
            if not value.strip():
                errors.append(f"{name} needs a value.")
            else:
                args[dest] = value
        elif kind == "multi":
            args.setdefault(dest, []).append(value)
        elif kind == "map":
            src, sep, dst = value.partition("=")
            if not sep or not src.strip() or not dst.strip():
                errors.append(f"--map needs SOURCE=DESTINATION, got {value!r}.")
            else:
                args.setdefault(dest, []).append((src, dst))
        elif kind == "float":
            try:
                number = float(value)
                if not math.isfinite(number) or number <= 0:
                    raise ValueError
                args[dest] = number
            except ValueError:
                errors.append(f"{name} needs a positive number, got {value!r}.")
        elif kind == "rights":
            rights = _parse_rights(value)
            if rights is None:
                errors.append(f"--rights takes push, pull or push,pull, got {value!r}.")
            else:
                args[dest] = rights
        elif kind == "ttl":
            ttl = _parse_ttl(value)
            if ttl is None or ttl <= 0:
                errors.append(f"--ttl takes a duration such as 10m or 600s, got {value!r}.")
            elif ttl > PAIR_TTL_MAX_S:
                errors.append("--ttl is at most 60 minutes (a pairing code is a short-lived secret).")
            else:
                args[dest] = ttl
        elif kind == "gate":
            gate = _parse_gate(value)
            if gate is None:
                errors.append(f"--gate takes session_token, bearer or basic_login, got {value!r}.")
            else:
                args[dest] = gate
    pos: list[str] = args["positionals"]
    low, high = _POSITIONALS[verb]
    if verb == "peers" and pos and pos[0].lower() == "add":
        errors.insert(
            0,
            "There is no 'peers add': a device becomes a peer only by pairing. Run /handover pair on the gateway "
            "that should accept handovers and paste the printed ctxh-pair:… string into /handover pair on this device.",
        )
    elif len(pos) < low or len(pos) > high:
        errors.append(f"Wrong number of arguments for '{verb}'. Usage: {USAGE[verb]}")
    _verb_args(verb, args, errors)
    return Parsed(verb, args, tuple(errors))


def _verb_args(verb: str, args: dict, errors: list[str]) -> None:
    pos: list[str] = args["positionals"]
    first = pos[0] if pos else None
    for key in (
        "wait",
        "force",
        "dry_run",
        "yes",
        "no_workspace",
        "latest",
        "keep",
        "as_dir",
        "trust_unsigned",
        "again",
        "allow_insecure",
        "all",
        "json",
    ):
        args.setdefault(key, False)
    for key in ("include", "include_secret", "exclude", "maps"):
        args.setdefault(key, [])
    if verb == "to":
        args["peer"] = first
    elif verb == "take":
        if args.get("latest") and args.get("offer"):
            errors.append("Use either --latest or --offer <id>, not both.")
    elif verb == "export":
        args["path"] = first
    elif verb == "import":
        args["bundle"] = first
    elif verb in {"status", "cancel", "resume", "logs"}:
        args["job"] = first
    elif verb == "reclaim":
        args["session"] = first
    elif verb == "gc":
        args.setdefault("job", None)
        if args.get("job") and args.get("all"):
            errors.append("Use either --job <id> or --all, not both.")
    elif verb == "pair":
        args["pair_string"] = first
        if first is not None and not first.startswith(PAIR_PREFIX):
            errors.append(
                f"{first!r} is not a pairing string. Paste the whole line that starts with {PAIR_PREFIX} "
                "(printed by /handover pair on the other gateway)."
            )
    elif verb == "peers":
        sub = (first or "list").lower()
        names = pos[1:]
        if sub not in PEERS_SUBVERBS:
            if sub != "add":
                errors.append(f"Unknown peers command {first!r}. Usage: {USAGE['peers']}")
        elif sub in {"revoke", "remove", "show"} and len(names) != 1:
            errors.append(f"'peers {sub}' needs one peer name. Usage: {USAGE['peers']}")
        elif sub == "rename" and len(names) != 2:
            errors.append(f"'peers rename' needs the old and the new name. Usage: {USAGE['peers']}")
        elif sub == "list" and names:
            errors.append(f"Usage: {USAGE['peers']}")
        args["sub"] = sub
        args["names"] = names
    elif verb == "identity":
        sub = (first or "show").lower()
        if sub not in IDENTITY_SUBVERBS:
            errors.append(f"Usage: {USAGE['identity']}")
        args["sub"] = sub
    elif verb == "help":
        args["topic"] = first


def help_text(topic: str | None = None) -> str:
    """Usage of one verb or the overview."""
    if topic and topic.lower() in USAGE:
        return USAGE[topic.lower()]
    return "\n".join(
        [
            "ctx_handover moves this session and its working files to another Hermes gateway you paired.",
            "  /handover take [<peer>]        bring a session from the other gateway here",
            "  /handover to <peer>            send this session there (or leave an offer the other side takes)",
            "  /handover export / import      the same as a single file, without a connection",
            "  /handover status | reclaim | cancel | resume | peers | pair | setup | doctor | identity | gc | logs",
            "First time: /handover setup, then /handover pair on the gateway that receives.",
        ]
    )


def options_from(parsed: Parsed, settings: dict) -> Options:
    """Settings (§14.4) plus the flags of one command, resolved once at job creation."""
    a = parsed.args
    s = {**settings_from_config({}), **(settings or {})}
    maps = tuple((str(x), str(y)) for x, y in a.get("maps") or ())
    hints = PlanHints(
        include=tuple(a.get("include") or ()),
        include_secret=tuple(a.get("include_secret") or ()),
        exclude=tuple(a.get("exclude") or ()),
        dest_overrides=maps,
        no_workspace=bool(a.get("no_workspace")),
    )
    total_gb = a.get("max_total_gb") or s["max_total_gb"]
    return Options(
        dest=a.get("dest"),
        maps=maps,
        hints=hints,
        model=a.get("model"),
        force=bool(a.get("force")),
        dry_run=bool(a.get("dry_run")),
        yes=bool(a.get("yes")),
        wait=bool(a.get("wait")),
        wait_timeout_s=int(s["wait_timeout_min"]) * 60,
        continue_prompt=a.get("continue"),
        keep_source=bool(a.get("keep")),
        trust_unsigned=bool(a.get("trust_unsigned")),
        again=bool(a.get("again")),
        as_dir=bool(a.get("as_dir")),
        max_total_bytes=int(float(total_gb) * GiB),
        max_file_bytes=int(s["max_file_mb"]) * MiB,
        max_extra_dir_bytes=int(s["max_extra_dir_mb"]) * MiB,
        max_hermes_file_bytes=int(s["max_hermes_file_mb"]) * MiB,
        max_files=int(s["max_files"]),
        max_untracked_files=int(s["max_untracked_files"]),
        workspace_root=str(s["workspace_root"]),
        allow_roots=tuple(s["allow_roots"]),
        export_roots=tuple(s["export_roots"]),
        max_session_bytes=int(s["max_session_mb"]) * MiB,
        refine=str(s["refine"]),
        refine_timeout_s=int(s["refine_timeout_s"]),
        refine_history_chars=int(s["refine_history_chars"]),
        refine_side_question_max_tokens=int(s["refine_side_question_max_tokens"]),
        refine_auto_include=bool(s["refine_auto_include"]),
        extra_paths=str(s["extra_paths"]),
        include_hermes_files=bool(s["include_hermes_files"]),
        model_mapping=str(s["model_mapping"]),
        lease_ttl_s=int(s["lease_ttl_s"]),
        participant_idle_timeout_s=int(s["participant_idle_timeout_min"]) * 60,
        stall_timeout_s=int(s["stall_timeout_min"]) * 60,
        staging_ttl_s=int(s["staging_ttl_hours"]) * 3600,
        confirm_above_bytes=int(s["confirm_above_mb"]) * MiB,
        extra_ignore=tuple(s["extra_ignore"]),
        tool_probe=tuple(s["tool_probe"]),
    )


def gate_from_settings(settings: dict, overrides: dict) -> GateAuth:
    """The dashboard-gate credentials for a new dial peer: mode from ``--gate`` or ``peer_gate``, secrets only as
    ``.env`` variable NAMES (``peer_token``/``peer_password`` of the settings form), extra proxy headers as
    ``(header, ENV_NAME)`` pairs. A pairing string never carries any of it (R17)."""
    s = {**settings_from_config({}), **(settings or {})}
    mode = overrides.get("gate") or _parse_gate(str(s.get("peer_gate") or "")) or GateMode.SESSION_TOKEN
    headers = tuple(sorted((str(k), str(v)) for k, v in (s.get("peer_extra_headers") or {}).items()))
    return GateAuth(
        mode=GateMode(mode),
        token_env=SECRET_ENVS["peer_token"],
        username=str(s.get("peer_username") or ""),
        password_env=SECRET_ENVS["peer_password"],
        extra_headers=headers,
    )


# ---------------------------------------------------------------------------------------------------------
# Small formatters
# ---------------------------------------------------------------------------------------------------------


def human_bytes(n: object) -> str:
    try:
        value = float(n)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return "?"
    if value >= GiB:
        return f"{value / GiB:.1f} GiB"
    if value >= MiB:
        return f"{value / MiB:.1f} MiB"
    if value >= 1024:
        return f"{value / 1024:.1f} KiB"
    return f"{int(value)} B"


def short_job(job_id: object) -> str:
    text = str(job_id or "")
    return text[-6:] if text else "?"


def when(ts: object) -> str:
    try:
        return datetime.fromtimestamp(float(ts), tz=timezone.utc).strftime("%Y-%m-%d %H:%M UTC")  # type: ignore[arg-type]
    except (TypeError, ValueError, OverflowError, OSError):
        return "never"


def _yn(flag: object) -> str:
    return "yes" if flag is True else "no" if flag is False else "unknown"


def _get(obj: Any, key: str, default: Any = None) -> Any:
    if isinstance(obj, Mapping):
        return obj.get(key, default)
    return getattr(obj, key, default)


def fingerprint_of(pubkey_b64: str) -> str:
    """16 hex characters of sha256 over the raw public key (the paste string's ``fp``): cluster C's
    ``crypto.fingerprint_of`` (W120); a key that is not canonical base64url (a display of a damaged row) is hashed
    as given, so listing peers never fails."""
    try:
        from .crypto import fingerprint_of as canonical

        return canonical(pubkey_b64)
    except (ImportError, ValueError, TypeError):
        pass
    try:
        raw = base64.urlsafe_b64decode(pubkey_b64 + "=" * (-len(pubkey_b64) % 4))
    except (ValueError, TypeError):
        raw = str(pubkey_b64).encode("utf-8")
    return hashlib.sha256(raw).hexdigest()[:16]


# ---------------------------------------------------------------------------------------------------------
# Plan summary, confirmation (DESIGN §8.7, §14.1)
# ---------------------------------------------------------------------------------------------------------

CONFIRM_CHOICES: list[dict] = [
    {"value": "yes", "label": "Yes, hand it over"},
    {"value": "no_proposed", "label": "Yes, without the proposed items"},
    {"value": "show_plan", "label": "Show the plan and stop"},
    {"value": "cancel", "label": "Cancel"},
]

REASONS: dict[str, str] = {letter: code for code, letter in planning.CONFIRM_LETTERS.items()}
"""Confirmation reasons (a)–(g) of DESIGN §14.1: letter → cluster D's reason code (``plan.CONFIRM_*``, X03)."""

REASON_TEXT = {
    planning.CONFIRM_PROPOSED: "some items are only proposed (found by the model, the agent or outside the project)",
    planning.CONFIRM_CONFLICT: "a destination has local changes, so a fresh folder is used",
    planning.CONFIRM_LARGE: "the handover is larger than confirm_above_mb",
    planning.CONFIRM_SECRET: "--include-secret sends a file from the secrets list",
    planning.CONFIRM_MODEL: "the model there does not work offline, or the session is larger than its context window",
    planning.CONFIRM_OUTSIDE: "items outside the project folder (or tracked secret files) are included",
    planning.CONFIRM_INSTRUCTIONS: "instruction files (AGENTS.md, .hermes.md …) arrive from an unverified source",
}

SUSPECTED_SECRET: str = planning.SUSPECTED_SECRET
"""Item marker (an exact element of ``PlanItem.reasons``) of a proposal the policy flagged as a suspected secret
(R25, X05). It is not a reason code: such an item is a proposal, i.e. reason (a)."""

SUSPECTED_TEXT = (
    "some proposed files look like secrets; they stay here unless you name each one with --include-secret <path>"
)

_PROPOSAL_REASONS = frozenset({planning.CONFIRM_PROPOSED})
"""Reason (a) alone — dropping all proposals answers it (BX L5; the conductor's own timeout rule)."""


def reason_names(reasons: Sequence[str]) -> list[str]:
    """Reason codes for letters or codes; unknown entries are kept (they still require a person)."""
    out: list[str] = []
    for r in reasons or ():
        key = str(r).strip().lower()
        name = REASONS.get(key, key)
        if name not in out:
            out.append(name)
    return out


def suspected_secrets(info: Mapping | None) -> list[str]:
    """Proposals the policy flagged as suspected secrets (R25): plan items with ``include == proposed`` whose
    ``reasons`` contain the exact element ``SUSPECTED_SECRET`` (X05, W126), plus the conductor's
    ``suspected_secrets`` list."""
    info = info or {}
    found: list[str] = []
    final = info.get("final_plan")
    for item in _get(_get(final, "plan"), "items", ()) or ():
        reasons = [str(r) for r in (_get(item, "reasons", ()) or ())]
        if _enum_text(_get(item, "include", "")) == "proposed" and SUSPECTED_SECRET in reasons:
            found.append(str(_get(item, "source_path", "")))
    for path in info.get("suspected_secrets") or ():
        if str(path) not in found:
            found.append(str(path))
    return found


def choice_on_timeout(reasons: Sequence[str]) -> str:
    """What an unanswered confirmation means (``ask_choice`` → None after 90 s, or no person present): only
    (a) proposed items ⇒ continue without them; anything else ⇒ cancel with nothing locked (BX L5)."""
    names = reason_names(reasons)
    return "no_proposed" if names and all(n in _PROPOSAL_REASONS for n in names) else "cancel"


def _enum_text(value: Any) -> str:
    return str(getattr(value, "value", value) or "")


def model_text(model_change: Any) -> str | None:
    """The target model of a ``FinalPlan.model_change`` (``{"to": {"model", "provider"}, …}``) as text."""
    if not isinstance(model_change, Mapping):
        return None
    to = model_change.get("to")
    if isinstance(to, Mapping):
        name = to.get("model") or to.get("name")
        provider = to.get("provider")
        if name and provider:
            return f"{name} ({provider})"
        return str(name or provider) if (name or provider) else None
    return str(to) if to else None


def _offline(model_change: Any) -> bool | None:
    if not isinstance(model_change, Mapping):
        return None
    for key in ("offline_capable", "offline"):
        if isinstance(model_change.get(key), bool):
            return model_change[key]
    return None


def _instruction_lines(entries: Sequence[Any]) -> list[str]:
    out = []
    for e in entries or ():
        if isinstance(e, Mapping):
            out.append(f"{e.get('rel') or e.get('path') or '?'} ({e.get('status') or 'new'})")
        else:
            out.append(str(e))
    return out


def plan_summary(final: Any, presend: Mapping | None = None, *, instruction_files: Sequence[Any] = ()) -> dict:
    """What ``confirm_question``, ``render_plan`` and ``render_presend`` show (W125): the totals and lists of
    cluster D's ``plan.presend_summary`` (``presend`` when the conductor passed it, else computed from ``final``)
    and the per-item rows, proposals, conflicts and need of D's ``plan.summarize``; F adds nothing of its own
    except the model text and the suspected-secret list (R25)."""
    pre: dict = dict(presend) if isinstance(presend, Mapping) else {}
    full: dict = {}
    if final is not None:
        if not pre:
            pre = planning.presend_summary(final)
        full = planning.summarize(final)
    item_list = list(pre.get("item_list") or ())
    count = pre.get("items")
    change = full.get("model_change") if full else None
    rows = [
        {
            "item_id": r.get("item_id"),
            "kind": r.get("kind"),
            "source_path": r.get("source_path"),
            "include": r.get("include"),
            "size": r.get("size") or 0,
            "dest": r.get("dest"),
            "mode": r.get("mode"),
            "conflict": r.get("conflict"),
        }
        for r in full.get("items") or ()
    ]
    return {
        "items_count": int(count) if isinstance(count, int) else len(item_list),
        "total_bytes": int(pre.get("total_bytes") or 0),
        "excluded_secrets": int(pre.get("excluded_secrets") or 0),
        "denied_tracked": [str(p) for p in pre.get("denied_tracked") or ()],
        "withheld_stash": [str(p) for p in pre.get("withheld_stash") or ()],
        "outside_workspace": [str(p) for p in pre.get("extras_outside_workspace") or ()],
        "item_list": item_list,
        "proposed": int(full.get("proposed") or 0),
        "conflicts": int(full.get("conflicts") or 0),
        "need_bytes": int(full.get("need_bytes") or 0),
        "model": model_text(change),
        "offline": _offline(change),
        "items": rows,
        "not_transferred": [f"{n.get('path')} ({n.get('why')})" for n in full.get("not_transferred") or ()],
        "suspected_secrets": suspected_secrets({"final_plan": final}),
        "instruction_files": _instruction_lines(instruction_files),
    }


def confirm_question(summary: Mapping, peer: str | None) -> str:
    """``Hand over to <peer>: <n> items, <size>, <k> proposed, <c> conflicts, model <m> (offline: yes/no).
    Continue?``"""
    return (
        f"Hand over to {peer or 'the bundle'}: {summary.get('items_count', 0)} items, "
        f"{human_bytes(summary.get('total_bytes', 0))}, {summary.get('proposed', 0)} proposed, "
        f"{summary.get('conflicts', 0)} conflicts, model {summary.get('model') or 'unchanged'} "
        f"(offline: {_yn(summary.get('offline'))}). Continue?"
    )


def _bullets(title: str, values: Sequence[Any], limit: int = 12) -> list[str]:
    values = [str(v) for v in values or () if str(v)]
    if not values:
        return []
    lines = [title]
    lines.extend(f"  - {v}" for v in values[:limit])
    if len(values) > limit:
        lines.append(f"  - … {len(values) - limit} more")
    return lines


def render_plan(summary: dict) -> str:
    """The plan as text: totals, items with destination and conflict, then everything left behind (§8.7)."""
    s = summary or {}
    lines = [
        f"Plan: {s.get('items_count', 0)} items, {human_bytes(s.get('total_bytes', 0))}, "
        f"{s.get('proposed', 0)} proposed, {s.get('conflicts', 0)} conflicts; model "
        f"{s.get('model') or 'unchanged'} (offline: {_yn(s.get('offline'))})."
    ]
    if s.get("need_bytes"):
        lines.append(f"Space needed on the receiving side: {human_bytes(s['need_bytes'])}.")
    for row in s.get("items") or ():
        dest = f" → {row.get('dest')}" if row.get("dest") else ""
        conflict = f" (conflict: {row['conflict']})" if row.get("conflict") else ""
        lines.append(
            f"  [{row.get('include', '?')}] {row.get('source_path')}{dest} · "
            f"{human_bytes(row.get('size', 0))}{conflict}"
        )
    if s.get("excluded_secrets"):
        lines.append(f"Secrets left out: {s['excluded_secrets']} (listed under 'Not transferred').")
    lines += _bullets(
        "Tracked files on the secrets list (they travel with the git history):", s.get("denied_tracked") or ()
    )
    lines += _bullets("Stash entries withheld:", s.get("withheld_stash") or ())
    lines += _bullets("Outside the project folder:", s.get("outside_workspace") or ())
    lines += _bullets(
        "Proposed files that look like secrets (they stay here unless named with --include-secret <path>):",
        s.get("suspected_secrets") or (),
    )
    lines += _bullets("Instruction files that will shape the agent there:", s.get("instruction_files") or ())
    lines += _bullets("Not transferred:", s.get("not_transferred") or ())
    return "\n".join(lines)


def render_presend(summary: dict, peer: str | None, mode: Mode | str) -> str:
    """First progress message (SEC 5.8): what leaves, how much, what stays behind — cluster D's
    ``presend_summary`` numbers through ``plan_summary`` (W125)."""
    s = summary or {}
    arrow = "←" if str(mode) in {"pull", "import"} else "→"
    secrets = s.get("excluded_secrets") or 0
    head = (
        f"Handover {arrow} {peer or 'bundle'}: {s.get('items_count', 0)} items, "
        f"{human_bytes(s.get('total_bytes', 0))}; "
        f"{secrets if isinstance(secrets, int) else len(secrets)} secrets left out"
    )
    extras = []
    if s.get("denied_tracked"):
        extras.append(f"{len(s['denied_tracked'])} tracked secret files in git history")
    if s.get("withheld_stash"):
        extras.append(f"{len(s['withheld_stash'])} stash entries withheld")
    if s.get("outside_workspace"):
        extras.append(f"{len(s['outside_workspace'])} items outside the project")
    return head + ("; " + "; ".join(extras) if extras else "") + "."


# ---------------------------------------------------------------------------------------------------------
# Status, peers, offers
# ---------------------------------------------------------------------------------------------------------


def render_peers(peers: Sequence[Peer], *, fingerprint: Callable[[str], str] = fingerprint_of) -> str:
    """Name, direction, rights, URL, gate mode, last seen, key fingerprint per paired device."""
    if not peers:
        return "No paired devices. Run /handover pair on the gateway that should receive handovers."
    lines = []
    for p in peers:
        rights = ",".join(sorted(str(r) for r in p.rights)) or "none"
        gate = p.gate.mode.value if p.gate is not None else "-"
        seen = when(p.last_seen_at) if p.last_seen_at else "never"
        ip = f" from {p.last_ip} (as reported by the proxy)" if p.last_ip else ""
        state = f" · revoked {when(p.revoked_at)}" if p.revoked_at else ""
        where = p.url or "(dials in)"
        profile = f" · profile {p.profile}" if p.profile else ""
        lines.append(
            f"{p.name} · {p.direction} · rights {rights} · {where} · gate {gate}{profile} · last seen {seen}{ip} · "
            f"key {fingerprint(p.pubkey)}{state}"
        )
    return "\n".join(lines)


def _job_line(job: Mapping) -> list[str]:
    prog = job.get("progress") or {}
    step = prog.get("step") if isinstance(prog, Mapping) else None
    head = (
        f"{short_job(job.get('job_id'))} · {job.get('mode', '?')} · {job.get('peer') or 'bundle'} · "
        f"{job.get('state', '?')}"
    )
    if step and step != job.get("state"):
        head += f" · {step}"
    if isinstance(prog, Mapping) and prog.get("bytes_total"):
        head += f" · {human_bytes(prog.get('bytes_done', 0))}/{human_bytes(prog['bytes_total'])}"
    lines = [head]
    for w in job.get("warnings") or ():
        lines.append(f"    warning: {w}")
    err = job.get("last_error")
    if isinstance(err, Mapping) and err:
        lines.append(f"    last error: {err.get('code', '?')}: {err.get('message', '')}")
    if job.get("next_action"):
        lines.append(f"    next: {job['next_action']}")
    cont = job.get("continuation")
    if isinstance(cont, Mapping) and cont.get("started"):
        if cont.get("running"):
            state = "running"
        elif cont.get("exit_code") is not None:
            state = f"exit code {cont.get('exit_code')}"
        else:
            state = "started"
        lines.append(f"    continuation: pid {cont.get('pid')} ({state}), log {cont.get('log')}")
    elif isinstance(cont, Mapping) and isinstance(cont.get("error"), Mapping):
        lines.append(f"    continuation not started: {cont['error'].get('code', '?')}: {cont['error'].get('message', '')}")
    return lines


def render_status(status: dict) -> str:
    """``/handover status``: active and recent jobs, this session's handover state, disk use, ctx outbox."""
    s = status or {}
    lines: list[str] = []
    session = s.get("session") or {}
    source = session.get("source") if isinstance(session, Mapping) else None
    if isinstance(source, Mapping) and source:
        lines.append(
            f"This session: {source.get('state')} (handover to {source.get('peer') or 'a bundle'}, "
            f"job {short_job(source.get('job_id'))})."
        )
    arrival = session.get("arrival") if isinstance(session, Mapping) else None
    if isinstance(arrival, Mapping) and arrival:
        lines.append(
            f"This session arrived from {arrival.get('peer') or 'a bundle'} (job {short_job(arrival.get('job_id'))}); "
            f"note {arrival.get('note_state', 'pending')}."
        )
        if arrival.get("source_reclaimed_at"):
            lines.append("The source session was reclaimed on the other side; the two copies are independent now.")
    jobs = list(s.get("jobs") or ())
    if not jobs and not lines:
        lines.append("No handovers yet. /handover help shows how to start.")
    for job in jobs:
        lines += _job_line(job)
    for entry in s.get("pulled_by") or ():
        items = ", ".join(str(i) for i in (entry.get("items") or ())[:5])
        lines.append(f"pulled by {entry.get('peer')} at {when(entry.get('at'))}: {items}")
    disk = s.get("disk") or {}
    if disk:
        lines.append(
            f"Transfer cache {human_bytes(disk.get('blocks_bytes', 0))}, "
            f"staging {human_bytes(disk.get('staging_bytes', 0))}"
            " (/handover gc frees it)."
        )
    ctx = s.get("ctx") or {}
    if ctx.get("pending"):
        lines.append(f"{ctx['pending']} ctx records waiting for {ctx.get('server') or 'ctx'}.")
    if ctx.get("error"):
        lines.append(f"ctx: {ctx['error']}")
    return "\n".join(lines)


def render_offer(offer: Mapping, peer: str, local_name: str, *, now: float) -> str:
    """``Offer created for laptop (valid 2 h). On laptop run: /handover take server``."""
    hours = max(0.0, (float(offer.get("expires_at") or now) - now) / 3600.0)
    validity = f"{hours:.0f} h" if hours >= 1 else f"{max(1, round(hours * 60))} min"
    return (
        f"Offer created for {peer} (valid {validity}). On {peer} run: /handover take {local_name}\n"
        "This session stays usable here until the other side takes it."
    )


# ---------------------------------------------------------------------------------------------------------
# Errors (DESIGN §14.1 table)
# ---------------------------------------------------------------------------------------------------------


def _d(err: HandoverError, key: str, default: Any = None) -> Any:
    return (err.details or {}).get(key, default)


def render_error(err: HandoverError, peer: str | None) -> str:
    """The remedy text for an error code; unknown codes fall back to the error's own message."""
    p = peer or str(_d(err, "peer", "") or "") or "the other gateway"
    code = getattr(err, "code", "internal")
    if code == "gate_rejected":
        if str(_d(err, "mode", "")) == "basic_login":
            return (
                f"The dashboard of {p} rejected the Basic login from the ctx_handover settings. Use the user name "
                f"and password you sign in to {p}'s dashboard with (peer_username / peer_password)."
            )
        return (
            f"The dashboard of {p} rejected the access credential from the ctx_handover settings. Use the token or "
            f"user/password you use for that gateway in Desktop (HERMES_DASHBOARD_SESSION_TOKEN on {p}, or its "
            "Basic login)."
        )
    if code == "listener_missing":
        return (
            f"{p} has no ctx_handover routes. If you just installed or enabled it there, restart `hermes serve` "
            f"and the gateway on {p}."
        )
    if code == "https_required":
        return (
            f"{p} uses plain HTTP. If the address is only reachable through your VPN (e.g. Tailscale), pair again "
            "with --allow-insecure; otherwise use HTTPS."
        )
    if code == "pair_invalid":
        return f"The pairing code is wrong or expired (10 min, single use). Run /handover pair again on {p}."
    if code == "unauthorized":
        skew = _d(err, "skew_s")
        clock = f" (skew {skew} s)" if skew is not None else ""
        return f"{p} did not accept this device's key. Pair again; if it keeps failing, check the clocks{clock}."
    if code == "forbidden":
        right = _d(err, "right") or "this"
        return f"{p} does not allow {right} for this device. Pair again with the needed rights."
    if code == "session_busy":
        return "A turn is running in this session. Wait for it, or use --wait to hand over as soon as it finishes."
    if code == "wait_timeout":
        return f"The running turn did not finish within {_d(err, 'minutes', 30)} min. Nothing was changed."
    if code == "already_handing_over":
        return f"This session is already being handed over (job {short_job(_d(err, 'job'))}). See /handover status."
    if code == "capability_missing":
        cap = _d(err, "capability") or "do this"
        versions = []
        if _d(err, "plugin_version"):
            versions.append(f"plugin {_d(err, 'plugin_version')}")
        if _d(err, "hermes_version"):
            versions.append(f"Hermes {_d(err, 'hermes_version')}")
        suffix = f" ({', '.join(versions)})" if versions else ""
        if _d(err, "local"):
            return f"This gateway cannot {cap}{suffix}: {err.message} Run /handover doctor."
        return f"{p} cannot {cap}{suffix}. Update the plugin there."
    if code == "payload_too_large":
        return f"A proxy in front of {p} limits request bodies below one block (≈ 513 KiB). Raise its body limit."
    if code == "insufficient_storage":
        side = _d(err, "side") or p
        return (
            f"{side} needs {human_bytes(_d(err, 'need_bytes', 0))} free in {_d(err, 'path', '?')}, has "
            f"{human_bytes(_d(err, 'free_bytes', 0))}. Nothing was transferred."
        )
    if code == "policy_denied":
        items = [str(i) for i in (_d(err, "items") or ())][:3]
        count = _d(err, "count") or len(items) or 1
        return (
            f"{count} items are not allowed on {p}: {', '.join(items) or err.message}. Adjust allow_roots there or "
            "use --dest."
        )
    if code == "restart_limit":
        return "The session kept changing during the handover (3 restarts). Pause work on it and try again."
    if code == "verify_failed":
        return f"The imported transcript on {p} did not match. Nothing was activated; the source is unchanged."
    if code == "in_doubt":
        return (
            "The target may already have the session; the source stays read-only until resolved. /handover status "
            "shows progress; /handover reclaim --force takes it back here."
        )
    if code == "peer_unreachable":
        reason = _d(err, "reason") or err.message or "no answer"
        release = _d(err, "release_at")
        free = f"by {release} (lease expiry)" if release else "when the lease expires"
        return (
            f"{p} is not reachable ({reason}). The job is suspended; the session here is usable again {free}; "
            f"/handover resume {short_job(_d(err, 'job'))} continues."
        )
    if code == "continue_refused":
        if _d(err, "reason") == "approvals_off":
            return (
                f"Handed over, but approvals are switched off on {p}, so no turn starts there without a person. "
                "Open the session there and send a message."
            )
        if _d(err, "reason") == "unverified":
            return (
                "Handed over, but the bundle is not verified, so no turn starts automatically. Open the session "
                "and send a message."
            )
        return (
            f"Handed over, but {p} does not allow remote continuation (allow_remote_continue). Open the session "
            "there and send a message."
        )
    if code == "cancelled":
        return "Cancelled. Nothing was changed."
    if code == "identity_lost":
        return (
            "This device's handover key is missing from the profile .env. Run /handover identity rotate, then pair "
            "again with every device."
        )
    if code == "env_not_writable":
        return (
            "The .env of this profile is managed and not writable, so ctx_handover cannot store its key. Ask the "
            "administrator to allow CTX_HANDOVER_IDENTITY_KEY, then run the command again."
        )
    if code == "inbound_disabled":
        return f"{p} does not accept handovers (inbound is off or nothing is paired). Pair again or check its setup."
    if code == "bad_response_signature":
        return (
            f"The answer from {p} was not signed by the key paired with it. Pair again; check for a proxy that "
            "rewrites responses."
        )
    if code == "lease_lost":
        return "The session could not stay paused (lease lost). Nothing was handed over; try again."
    if code == "not_found":
        return err.message or "Not found."
    if code == "bad_request":
        return err.message or "The command could not be used as given."
    text = err.message or code
    return text if code in {"internal", *text.split()} else f"{text} ({code})"


# ---------------------------------------------------------------------------------------------------------
# Final messages (DESIGN §11.6)
# ---------------------------------------------------------------------------------------------------------


def _ack_cwd(ack: CommitAck | None) -> str | None:
    if ack is None:
        return None
    for entry in ack.path_map or ():
        if isinstance(entry, PathMapEntry) and entry.kind == "workspace":
            return entry.target
        if isinstance(entry, Mapping) and entry.get("kind") == "workspace":
            return str(entry.get("target"))
    for dest in ack.dests or ():
        if len(dest) >= 2:
            return str(dest[1])
    return None


def _note_block(note: str | None, origin: str | None, lists: dict[str, Sequence[Any]]) -> list[str]:
    lines: list[str] = []
    if origin:
        lines.append(f"Origin: {origin}.")
    titles = {
        "excluded_secrets": "Secrets left out:",
        "withheld_stash": "Stash entries withheld:",
        "conflicts": "Conflicts (a fresh folder was used):",
        "instruction_files": "Instruction files new or changed here:",
        "warnings": "Warnings:",
    }
    for key, title in titles.items():
        lines += _bullets(title, lists.get(key) or ())
    if note:
        lines.append("The agent gets this note with your first message (it is not shown in the transcript view):")
        lines.append(note)
    return lines


def render_job_details(job: Mapping) -> str:
    """For ``/handover status <job>`` on the receiving side (SEC 4.6): origin, excluded secrets, withheld stash
    entries, conflicts, instruction files, warnings and the full note text."""
    lists = {
        key: job.get(key) or ()
        for key in ("excluded_secrets", "withheld_stash", "conflicts", "instruction_files", "warnings")
    }
    return "\n".join(_note_block(job.get("note"), job.get("origin"), lists))


def render_final(
    mode: Mode,
    ack: CommitAck | None,
    state: str,
    peer: str | None,
    continue_result: ContinueResult | None,
    *,
    title: str | None = None,
    note: str | None = None,
    origin: str | None = None,
    excluded_secrets: Sequence[str] = (),
    withheld_stash: Sequence[str] = (),
    conflicts: Sequence[str] = (),
    instruction_files: Sequence[str] = (),
    warnings: Sequence[str] = (),
    continue_error: HandoverError | None = None,
    bundle_path: str | None = None,
    bundle_bytes: int | None = None,
    keep: bool = False,
    remote_gateway: bool = False,
) -> str:
    """Success text naming target session, folder, model, offline capability and the next step (R14, BX P6).

    ``state`` is the conductor's final state; for a pull, anything before ``retired``/``done`` means the remote
    source is released on the next contact (SK L8).
    """
    mode = Mode(mode)
    peer_name = peer or "the other gateway"
    t = (ack.title if ack is not None and ack.title else None) or title or "this session"
    lists = {
        "excluded_secrets": excluded_secrets,
        "withheld_stash": withheld_stash,
        "conflicts": conflicts,
        "instruction_files": instruction_files,
        "warnings": list(warnings) + list(ack.warnings if ack is not None else ()),
    }
    if mode is Mode.EXPORT:
        size = f" ({human_bytes(bundle_bytes)})" if bundle_bytes is not None else ""
        lines = [f'Exported "{t}" to {bundle_path or "the bundle"}{size}.']
        if keep:
            lines.append("This session stays active here; the bundle is a copy (the import note says so).")
        else:
            lines.append("This session is now read-only here; /handover reclaim takes it back.")
        if remote_gateway:
            lines.append("Download it in Desktop through the file view of this gateway, then /handover import it.")
        else:
            lines.append("Copy the file to the other machine and run /handover import <file> there.")
        lines += _note_block(None, None, {"excluded_secrets": excluded_secrets, "warnings": warnings})
        return "\n".join(lines)
    sid = ack.session_id if ack is not None else "?"
    cwd = _ack_cwd(ack)
    model = ack.model if ack is not None else None
    where = ", ".join(x for x in (f"session {sid}", cwd, f"model {model or 'default'}") if x)
    offline = f"works offline: {_yn(ack.offline_capable if ack is not None else None)}"
    if mode in (Mode.PUSH, Mode.OFFER):
        lines = [
            f'Handed over to {peer_name}: "{t}" continues there ({where}; {offline}). This session is now '
            "read-only here; /handover reclaim takes it back."
        ]
        if continue_result is not None and continue_result.started:
            lines.append(f"The agent is continuing on {peer_name}.")
        elif continue_error is not None:
            lines.append(render_error(continue_error, peer))
        lines += _note_block(None, None, {"excluded_secrets": excluded_secrets, "warnings": lists["warnings"]})
        return "\n".join(lines)
    # pull, import: the session is here now
    retired = str(state).lower() in {"retired", "continuing", "done"}
    if mode is Mode.PULL and not retired:
        lines = [f'"{t}" is active here ({where}; {offline}); {peer_name} will be released on the next contact.']
    else:
        lines = [f'"{t}" is here now ({where}; {offline}).']
    if continue_result is not None and continue_result.started:
        lines.append("The agent is continuing in that session now.")
    else:
        if continue_error is not None:
            lines.append(render_error(continue_error, None))
        lines.append("Open it from the session list and send a message; the first turn gets a note about what changed.")
    lines += _note_block(note, origin, lists)
    return "\n".join(lines)
