"""What the person sees: argument parsing, settings, confirmation, progress, final and error texts (layer L10;
DESIGN §11.6, §14.1, §14.4, §8.7).

Everything here is pure (no I/O, no Hermes): ``commands`` (slash and CLI) and ``agent_tool`` compose it with the
engine. One grammar serves ``/handover …`` and ``hermes handover …``; the parser never exits and reports every
problem as a sentence that says what to type instead. Every error text names the remedy (BX M8).
"""

from __future__ import annotations

import math
import re
import shlex
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from . import plan as planning
from . import settings as settings_rules
from .errors import BadRequest, HandoverError
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
# Settings (DESIGN §14.4) — one normaliser for every reader: K0's ``engine/settings.py`` (contract V22)
# ---------------------------------------------------------------------------------------------------------

SETTINGS_SCHEMA: Mapping[str, Mapping[str, Any]] = settings_rules.SCHEMA
"""The settings table (type, default or ``env``, choices) — K0's mirror of ``plugin.yaml`` ``config_schema``."""
SECRET_ENVS: Mapping[str, str] = settings_rules.SECRET_ENVS
"""Secret settings → the ``.env`` variable that holds their value (never ``config.yaml``)."""


def settings_from_config(config: Mapping | None) -> dict:
    """``plugins.entries.ctx_handover.settings`` normalised by K0's ``settings.from_config`` (V22): every key, a
    value of the wrong type, outside its choices or ``None`` read as the schema default; secret keys are not part of
    the result (their values live in ``.env`` under ``SECRET_ENVS``). F keeps no reading rule of its own."""
    return settings_rules.from_config(config)


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
    "--measure": ("measure", "flag"),
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
    "reclaim": {"--force", "--session"},
    "cancel": set(),
    "resume": set(),
    "setup": set(),
    "doctor": {"--measure"},
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
    "reclaim": "/handover reclaim [<session-id> | --session ID] [--force]",
    "cancel": "/handover cancel [<job>]",
    "resume": "/handover resume [<job>]",
    "setup": "/handover setup",
    "doctor": "/handover doctor [--measure]   (--measure sends test bodies of 1, 2, 4 and 8 MiB to each paired "
    "gateway)",
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
        "measure",
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
        # the session as a positional or as --session (the form every other CLI verb takes, L3-F06); one id only
        named = args.get("session")
        if first is not None and named is not None and first != named:
            errors.append(f"Name the session once: {first!r} and --session {named!r} differ. Usage: {USAGE['reclaim']}")
        args["session"] = first if first is not None else named
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


def include_secret_from(parsed: Parsed) -> tuple[str, ...]:
    """The ``--include-secret`` paths typed in this command (X07, R18, R24). They travel only as the explicit
    ``include_secret`` parameter of ``start_job``/``create_offer`` — never inside ``Options`` (whose hints go to the
    other side) — and only the local surfaces call this."""
    return tuple(str(p) for p in parsed.args.get("include_secret") or ())


def options_from(parsed: Parsed, settings: dict) -> Options:
    """Settings (§14.4) plus the flags of one command, resolved once at job creation. ``--include-secret`` is not
    part of the result (``hints.include_secret`` stays empty, X07): ``include_secret_from`` reads it."""
    a = parsed.args
    s = {**settings_from_config({}), **(settings or {})}
    maps = tuple((str(x), str(y)) for x, y in a.get("maps") or ())
    hints = PlanHints(
        include=tuple(a.get("include") or ()),
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


MIN_JOB_TOKEN = 4
"""Fewest characters of a job id that ``match_job`` resolves (the printed short form has 6)."""
FULL_JOB_LEN = 26
_JOB_TOKEN = re.compile(r"\A[0-9A-Za-z]+\Z")


def match_job(token: object, job_ids: Sequence[object], *, listed: bool = True) -> str:
    """The job a typed id names (L3-F15): the printed short form (``short_job``, the id's last six characters) or
    any other unique end or start of a job id resolves to the full id of ``job_ids`` (this gateway's jobs, every
    role), case-insensitively; an exact id wins. Several matches ⇒ ``BadRequest`` naming them (``listed=False``
    — a caller on a messaging platform — names only their count). Anything else is returned unchanged — a full
    id, a token shorter than ``MIN_JOB_TOKEN``, or one no job ends or starts with — so the engine answers "not
    found" for it exactly as before."""
    text = str(token or "").strip()
    known = [str(j) for j in job_ids or () if j]
    upper = text.upper()
    exact = [j for j in known if j.upper() == upper]
    if exact:
        return exact[0]
    if len(text) < MIN_JOB_TOKEN or len(text) >= FULL_JOB_LEN or not _JOB_TOKEN.match(text):
        return text
    found = list(dict.fromkeys(j for j in known if j.upper().endswith(upper) or j.upper().startswith(upper)))
    if len(found) == 1:
        return found[0]
    if not found:
        return text
    if not listed:
        raise BadRequest(
            f"{text} matches {len(found)} handovers; give more characters of the job id (/handover status lists "
            "yours).",
            field="job",
        )
    names = ", ".join(found[:5]) + (f" and {len(found) - 5} more" if len(found) > 5 else "")
    raise BadRequest(f"{text} matches several handovers: {names}. Give more characters or the full id.", field="job")


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
    """A peer key's fingerprint as people see it — only cluster C's ``crypto.fingerprint_of`` (W120, X24; the paste
    string's ``fp``). A key that is not canonical base64url (a damaged row) reads ``invalid key`` and a runtime
    without ``cryptography`` ``unavailable``, so listing peers never fails."""
    try:
        from .crypto import fingerprint_of as canonical
    except ImportError:
        return "unavailable"
    try:
        return canonical(pubkey_b64)
    except (ValueError, TypeError):
        return "invalid key"


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

CONFIRM_WORKSPACE_EXCLUDED: str = planning.CONFIRM_WORKSPACE_EXCLUDED
"""Reason (h) of cluster D (``reports/L2/gp-d-api.md`` §2a, R30): the policy left the session's working folder out
(for example it lies inside the Hermes home), so its project does not come along."""

CONFIRM_CONTEXT: str = planning.CONFIRM_CONTEXT
"""Reason (i) of cluster D (``reports/L2/g3-df-api.md`` §1, R32): the session is larger than the context window of
the model on the target, so the target compacts it on its first turn."""

CONFIRM_WORKSPACE_DIFFERS: str = planning.CONFIRM_WORKSPACE_DIFFERS
"""Reason (j) of cluster D (R36, L3-F19): the workspace holds more than the session's working folder — the repository
around it — and travels as a whole."""

YES_EXEMPT: frozenset[str] = planning.YES_EXEMPT
"""Reasons a blanket yes (``--yes``) never confirms: D's ``plan.YES_EXEMPT`` (R30, R32, R36; suspected secrets, R25,
are an item marker and handled apart)."""

REASON_TEXT = {
    planning.CONFIRM_PROPOSED: "some items are only proposed (found by the model, the agent or outside the project)",
    planning.CONFIRM_CONFLICT: "a destination has local changes, so a fresh folder is used",
    planning.CONFIRM_LARGE: "the handover is larger than confirm_above_mb",
    planning.CONFIRM_SECRET: "--include-secret sends a file from the secrets list",
    planning.CONFIRM_MODEL: "the model there does not work offline (this session's model did)",
    planning.CONFIRM_OUTSIDE: "items outside the project folder (or tracked secret files) are included",
    planning.CONFIRM_INSTRUCTIONS: "instruction files (AGENTS.md, .hermes.md …) arrive from an unverified source",
    CONFIRM_WORKSPACE_EXCLUDED: "the session's working folder stays here because the policy leaves it out, so its "
    "project does not come along (--yes does not confirm this; --no-workspace does)",
    CONFIRM_CONTEXT: "the session is larger than the context window of the model there, so it is compacted on its "
    "first turn there (/compress here first keeps that in your hands; --yes does not confirm this)",
    CONFIRM_WORKSPACE_DIFFERS: "the workspace is the whole repository around the session's working folder, not only "
    "that folder (--yes does not confirm this)",
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


def yes_exempt(reasons: Sequence[str]) -> list[str]:
    """The reasons (letters or codes) a blanket ``--yes`` does not confirm: D's ``plan.yes_exempt`` over the codes
    (R30), in order."""
    return planning.yes_exempt(reason_names(reasons))


UNNAMED_EXEMPT = "unnamed_exempt"
"""Stands in for an ``info["yes_exempt"]`` value of an unknown form: the conductor marked something ``--yes`` does
not confirm, and the surface cannot tell what — so it asks (fail-closed)."""


def yes_exempt_of(info: Mapping | None, reasons: Sequence[str]) -> list[str]:
    """What ``--yes`` does not confirm in one confirmation: the conductor's ``info["yes_exempt"]`` (codes it calls the
    callback for despite ``--yes``, ``conductor._confirm``) together with D's table over ``reasons`` — the
    conductor's list wins where the two differ. Only an absent key, ``None`` or an empty list/tuple/set name nothing;
    a bare string or a letter names one reason; any other value counts as ``UNNAMED_EXEMPT`` (ask, never "yes")."""
    raw = info.get("yes_exempt") if isinstance(info, Mapping) else None
    if raw is None:
        named: list[str] = []
    elif isinstance(raw, str):
        named = [raw]
    elif isinstance(raw, (list, tuple, set, frozenset)):
        named = [str(r) for r in raw]
    else:
        named = [UNNAMED_EXEMPT]
    out = yes_exempt(reasons)
    for name in reason_names(named):
        if name and name not in out:
            out.append(name)
    return out


_WORKSPACE_WHY = {
    "hermes": "it lies inside the Hermes home, which is never sent",
    "home": "it is the home folder itself",
    "secret": "it is on the secrets list",
    "too_large": "it is larger than the size limits allow",
    "missing": "it does not exist",
    "unreadable": "it cannot be read",
}
"""Sentences for the ``reason`` codes of ``plan.workspace_excluded`` (gp-d-api §2b); any other code (``other``)
shows D's ``why`` text."""


def _workspace_excluded(value: Any) -> dict | None:
    """D's ``presend_summary["workspace_excluded"]`` when it has its form — a mapping with a non-empty string
    ``path`` (plus ``reason`` and ``why``) — else None: the surface invents no folder."""
    if not isinstance(value, Mapping):
        return None
    path = value.get("path")
    if not isinstance(path, str) or not path:
        return None
    return {"path": path, "reason": str(value.get("reason") or ""), "why": str(value.get("why") or "")}


def _workspace_why(entry: Mapping) -> str:
    return _WORKSPACE_WHY.get(str(entry.get("reason") or "")) or str(entry.get("why") or "") or "left out by the policy"


def workspace_excluded_text(entry: Mapping) -> str:
    """``the working folder <path> stays here (<why>)`` for an excluded working folder (R30, L3-F02)."""
    return f"the working folder {entry.get('path')} stays here ({_workspace_why(entry)})"


def workspace_entry(value: Any) -> dict | None:
    """D's ``presend_summary["workspace"]`` (R36) when it has its form — a mapping with a non-empty string ``path`` —
    else None: the surface invents no folder. ``cwd`` defaults to ``path``; ``bytes`` and ``cwd_bytes`` keep only
    non-negative integers (a missing ``cwd_bytes`` of a differing workspace stays None: size unknown); ``differs`` is
    True when D says so or the two paths differ (rather say too much than hide a larger workspace)."""
    if not isinstance(value, Mapping):
        return None
    path = value.get("path")
    if not isinstance(path, str) or not path:
        return None
    cwd = value.get("cwd") if isinstance(value.get("cwd"), str) and value.get("cwd") else path
    differs = value.get("differs") is True or cwd != path
    size = _count(value.get("bytes"))
    raw = value.get("cwd_bytes")
    cwd_bytes = raw if isinstance(raw, int) and not isinstance(raw, bool) and raw >= 0 else None
    if not differs and cwd_bytes is None:
        cwd_bytes = size
    return {"path": path, "bytes": size, "cwd": cwd, "cwd_bytes": cwd_bytes, "differs": differs}


def workspace_text(entry: Mapping) -> str:
    """``workspace <path> (<size>)``, and for a workspace that holds more than the session's folder
    ``…, more than the session's folder <cwd> (<size>)`` (R36)."""
    text = f"workspace {entry.get('path')} ({human_bytes(entry.get('bytes', 0))})"
    if entry.get("differs"):
        size = entry.get("cwd_bytes")
        shown = human_bytes(size) if isinstance(size, int) else "size unknown"
        text += f", more than the session's folder {entry.get('cwd')} ({shown})"
    return text


def context_over_entry(value: Any) -> dict | None:
    """D's ``presend_summary["context_over"]`` (R32) when it has its form — positive integer ``tokens`` and
    ``context_length`` with ``tokens > context_length`` (``model`` optional) — else None: the surface invents no
    window."""
    if not isinstance(value, Mapping):
        return None
    tokens, window = value.get("tokens"), value.get("context_length")
    if not (_count(tokens) and _count(window) and tokens > window):
        return None
    model = value.get("model")
    return {"tokens": tokens, "context_length": window, "model": model if isinstance(model, str) and model else None}


def context_over_text(entry: Mapping) -> str:
    """``the session (about <n> tokens) is larger than the context window of <model> there (<w> tokens); it is
    compacted on its first turn there`` (R32, L3-F17)."""
    model = entry.get("model") or "the model"
    return (
        f"the session (about {entry.get('tokens'):,} tokens) is larger than the context window of {model} there "
        f"({entry.get('context_length'):,} tokens); it is compacted on its first turn there"
    )


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


def _instruction_lines(entries: Sequence[Any]) -> list[str]:
    out = []
    for e in entries or ():
        if isinstance(e, Mapping):
            # "new"/"changed" is D's statement (plan.instruction_files); a missing status is not "new"
            out.append(f"{e.get('rel') or e.get('path') or '?'} ({e.get('status') or 'status unknown'})")
        else:
            out.append(str(e))
    return out


def _count(value: Any) -> int:
    return value if isinstance(value, int) and not isinstance(value, bool) and value >= 0 else 0


def _texts(values: Any) -> list[str]:
    return [str(v) for v in values] if isinstance(values, (list, tuple)) else []


def plan_summary(final: Any, presend: Mapping | None = None, *, instruction_files: Sequence[Any] = ()) -> dict:
    """What ``confirm_question``, ``render_plan`` and ``render_presend`` show (W125, V10): the W125 keys of cluster
    D's ``plan.presend_summary`` (``plan.PRESEND_KEYS``) exactly as the conductor handed them over (``presend``;
    computed from ``final`` only when there is none) — ``items_count``, ``total_bytes``, ``excluded_secrets``,
    ``denied_tracked``, ``withheld_stash``, ``outside_workspace``, ``instruction_files``, ``model``, ``offline`` —
    plus the per-item rows, proposals, conflicts and need of D's ``plan.summarize`` when ``final`` is known, and
    ``workspace_excluded`` (the working folder the policy left out, R30; None unless it has D's form),
    ``context_over`` (the session is larger than the target model's window, R32; same rule) and ``workspace`` (the
    workspace against the session's working folder, R36; ``workspace_entry``). F adds
    nothing of its own except the suspected-secret list (R25). ``instruction_files`` (the target's result, with
    ``status``) replaces the summary's list when given."""
    pre: dict = dict(presend) if isinstance(presend, Mapping) else {}
    full: dict = {}
    if final is not None:
        if not pre:
            pre = planning.presend_summary(final)
        full = planning.summarize(final)
    item_list = list(pre.get("item_list") or ())
    offline = pre.get("offline")
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
        "items_count": _count(pre.get("items_count")),
        "total_bytes": _count(pre.get("total_bytes")),
        "excluded_secrets": _count(pre.get("excluded_secrets")),
        "denied_tracked": _texts(pre.get("denied_tracked")),
        "withheld_stash": _texts(pre.get("withheld_stash")),
        "outside_workspace": _texts(pre.get("outside_workspace")),
        "workspace_excluded": _workspace_excluded(pre.get("workspace_excluded")),
        "context_over": context_over_entry(pre.get("context_over")),
        "workspace": workspace_entry(pre.get("workspace")),
        "item_list": item_list,
        "proposed": int(full.get("proposed") or 0),
        "conflicts": int(full.get("conflicts") or 0),
        "need_bytes": int(full.get("need_bytes") or 0),
        "model": str(pre["model"]) if pre.get("model") else None,
        "offline": offline if isinstance(offline, bool) else None,
        "items": rows,
        "not_transferred": [f"{n.get('path')} ({n.get('why')})" for n in full.get("not_transferred") or ()],
        "suspected_secrets": suspected_secrets({"final_plan": final}),
        "instruction_files": _instruction_lines(instruction_files or pre.get("instruction_files") or ()),
    }


def confirm_question(summary: Mapping, peer: str | None) -> str:
    """``Hand over to <peer>: <n> items, <size>, <k> proposed, <c> conflicts, model <m> (offline: yes/no).
    [The workspace is <path> (<size>)[, more than the session's folder <cwd> (<size>)].] [The working folder <path>
    stays here (<why>).] [The session (about <n> tokens) is larger than …] Continue?``"""
    ws = workspace_entry(summary.get("workspace"))
    folder = f"The workspace is {workspace_text(ws).removeprefix('workspace ')}. " if ws else ""
    excluded = _workspace_excluded(summary.get("workspace_excluded"))
    if excluded:
        folder += f"The {workspace_excluded_text(excluded).removeprefix('the ')}. "
    over = context_over_entry(summary.get("context_over"))
    if over:
        folder += f"The {context_over_text(over).removeprefix('the ')}. "
    return (
        f"Hand over to {peer or 'the bundle'}: {summary.get('items_count', 0)} items, "
        f"{human_bytes(summary.get('total_bytes', 0))}, {summary.get('proposed', 0)} proposed, "
        f"{summary.get('conflicts', 0)} conflicts, model {summary.get('model') or 'unchanged'} "
        f"(offline: {_yn(summary.get('offline'))}). {folder}Continue?"
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
    ws = workspace_entry(s.get("workspace"))
    if ws:
        lines.append(f"Workspace: {workspace_text(ws).removeprefix('workspace ')}.")
    excluded = _workspace_excluded(s.get("workspace_excluded"))
    if excluded:
        lines.append(
            f"Working folder left out: {excluded['path']} ({_workspace_why(excluded)}); the session continues there "
            "without its project."
        )
    over = context_over_entry(s.get("context_over"))
    if over:
        lines.append(f"Context: {context_over_text(over)} (/compress here first to keep that in your hands).")
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
    ``presend_summary`` numbers through ``plan_summary`` (W125), the workspace against the session's folder (R36) and
    the working folder the policy left out (R30)."""
    s = summary or {}
    arrow = "←" if str(mode) in {"pull", "import"} else "→"
    secrets = s.get("excluded_secrets") or 0
    ws = workspace_entry(s.get("workspace"))
    head = (
        f"Handover {arrow} {peer or 'bundle'}: {s.get('items_count', 0)} items, "
        f"{human_bytes(s.get('total_bytes', 0))}; "
        + (f"{workspace_text(ws)}; " if ws else "")
        + f"{secrets if isinstance(secrets, int) else len(secrets)} secrets left out"
    )
    extras = []
    if s.get("denied_tracked"):
        extras.append(f"{len(s['denied_tracked'])} tracked secret files in git history")
    if s.get("withheld_stash"):
        extras.append(f"{len(s['withheld_stash'])} stash entries withheld")
    if s.get("outside_workspace"):
        extras.append(f"{len(s['outside_workspace'])} items outside the project")
    excluded = _workspace_excluded(s.get("workspace_excluded"))
    if excluded:
        extras.append(workspace_excluded_text(excluded))
    over = context_over_entry(s.get("context_over"))
    if over:
        extras.append(context_over_text(over))
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


_PARTICIPANT_SIDE = {"target": "from", "source": "to"}
"""Role of this gateway in a job another gateway conducts (``JobRunner.status``, contract V43) → the word before the
other side: a push that arrives here (``target``), a pull that takes a session from here (``source``)."""


def _job_line(job: Mapping) -> list[str]:
    prog = job.get("progress") or {}
    step = prog.get("step") if isinstance(prog, Mapping) else None
    role = job.get("role")
    side = _PARTICIPANT_SIDE.get(role) if isinstance(role, str) else None
    if side is not None:  # another gateway conducts this job; this gateway is its source or its target
        where = f"{role}, {side} {job.get('peer') or 'another gateway'}"
    else:
        where = str(job.get("peer") or "bundle")
    head = f"{short_job(job.get('job_id'))} · {job.get('mode', '?')} · {where} · {job.get('state', '?')}"
    if step and step != job.get("state"):
        head += f" · {step}"
    if isinstance(prog, Mapping) and prog.get("bytes_total"):
        head += f" · {human_bytes(prog.get('bytes_done', 0))}/{human_bytes(prog['bytes_total'])}"
    lines = [head]
    session = job.get("calling_session")
    if side is not None and isinstance(session, str) and session:
        title = job.get("title")
        named = f" ({title})" if isinstance(title, str) and title else ""
        lines.append(f"    session here: {session}{named}")
    arrived = _target_session(job.get("target_session"), None) if side is None else None
    if arrived is not None and str(job.get("mode")) in {"pull", "import", "push", "offer"}:
        # L17: the session this job created on the target — here after a pull/import, on the peer after a push
        where_session = "here" if str(job.get("mode")) in {"pull", "import"} else f"on {job.get('peer') or 'the peer'}"
        named = f" ({arrived['title']})" if arrived.get("title") else ""
        lines.append(
            f"    session {where_session}: {arrived['id']}{named} (open it with `hermes --resume {arrived['id']}`)"
        )
    for w in job.get("warnings") or ():
        lines.append(f"    warning: {w}")
    err = job.get("last_error")
    if isinstance(err, Mapping) and err:
        lines.append(f"    last error: {err.get('code', '?')}: {err.get('message', '')}")
    if job.get("source_kept") is True:  # export --keep (O3): the session was not locked or retired
        lines.append("    copy (--keep): this session stays active here; the bundle is a copy")
    if job.get("next_action"):
        lines.append(f"    next: {job['next_action']}")
    cont = job.get("continuation")
    if isinstance(cont, Mapping) and cont.get("started") is True:
        if cont.get("running") is True:
            state = "running"
        elif cont.get("exit_code") is not None:
            state = f"exit code {cont.get('exit_code')}"
        else:
            state = "started"
        lines.append(f"    continuation: pid {cont.get('pid')} ({state}), log {cont.get('log')}")
    elif isinstance(cont, Mapping) and isinstance(cont.get("error"), Mapping):
        cerr = cont["error"]
        lines.append(f"    continuation not started: {cerr.get('code', '?')}: {cerr.get('message', '')}")
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


def _workspace_root_text(err: HandoverError, peer: str) -> str:
    """R29 (contract V40): the receiving gateway's write root cannot be used, found while the facts were exchanged —
    before anything was locked or sent. D's sentence (``envfacts.workspace_root_message``) when the error carries
    one; the setting and the folder are named in any case."""
    host = str(_d(err, "host") or "") or peer
    path = str(_d(err, "path") or "")
    setting = str(_d(err, "setting") or "workspace_root")
    message = " ".join(str(err.message or "").split()).rstrip(".")
    if message and (not path or path in message) and setting in message:
        text = message
    else:
        why = message or str(_d(err, "problem") or "") or "check_failed"
        text = (
            f"the workspace root {path or '?'} on {host} cannot be used ({why}); set {setting} (and allow_roots) in "
            f"the ctx_handover settings on {host}"
        )
    return f"{text[:1].upper()}{text[1:]}. Nothing was locked or sent."


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
            f"and the gateway on {p}. Under plugins.isolation: host a gateway cannot receive or provide handovers "
            f"over HTTP (R33): set plugins.isolation: in_process on {p}, or use a bundle (/handover export)."
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
        if not _d(err, "need_bytes"):  # a write failed on a full disk; how much more it needs is not known
            return (
                f"{side} ran out of space in {_d(err, 'path', '?')} ({human_bytes(_d(err, 'free_bytes', 0))} free). "
                "Nothing was transferred."
            )
        return (
            f"{side} needs {human_bytes(_d(err, 'need_bytes', 0))} free in {_d(err, 'path', '?')}, has "
            f"{human_bytes(_d(err, 'free_bytes', 0))}. Nothing was transferred."
        )
    if code == "policy_denied" and _d(err, "reason") == "workspace_root":
        return _workspace_root_text(err, p)
    if code == "policy_denied":
        items = [str(i) for i in (_d(err, "items") or ())][:3]
        count = _d(err, "count") or len(items) or 1
        return (
            f"{count} items are not allowed on {p}: {', '.join(items) or err.message}. Adjust allow_roots there or "
            "use --dest."
        )
    if code == "conflict" and _d(err, "kind") == "partial_exists":
        where = str(_d(err, "path") or "") or "a folder"
        return (
            f"{where} is in the way on the receiving gateway and was not left there by this handover; nothing there "
            "was changed. Move or remove it there, then hand over again."
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
        "withheld": "Withheld by source policy (the other side does not release them):",
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
    """For ``/handover status <job>`` on the receiving side (SEC 4.6): origin, excluded secrets, paths the source
    withheld by its policy (``withheld``, V29), withheld stash entries, conflicts, instruction files, warnings and
    the full note text."""
    lists = {
        key: job.get(key) or ()
        for key in ("excluded_secrets", "withheld", "withheld_stash", "conflicts", "instruction_files", "warnings")
    }
    return "\n".join(_note_block(job.get("note"), job.get("origin"), lists))


def _target_session(entry: Mapping | None, ack: CommitAck | None) -> dict | None:
    """``{"id", "title"}`` of the session the handover created on the target: the job's ``target_session`` (A, L17)
    when its id is a non-empty string, else the ack's ``session_id``/``title``; None when neither names one."""
    if isinstance(entry, Mapping) and isinstance(entry.get("id"), str) and entry["id"]:
        title = entry.get("title")
        return {"id": entry["id"], "title": title if isinstance(title, str) and title else None}
    if ack is not None and isinstance(ack.session_id, str) and ack.session_id:
        return {"id": ack.session_id, "title": ack.title or None}
    return None


def open_text(session_id: str, title: str | None = None) -> str:
    """How to open a received session (L17; upstream read by g3-b): it is stored, not open — the Desktop sidebar and
    ``hermes sessions list`` show it, ``hermes --resume <id>`` opens it; a gateway's list of open sessions (TUI
    switcher, ``session.list`` without ``scope: stored``) shows it only after it was opened once."""
    named = f'"{title}" ({session_id})' if title else session_id
    return (
        f"The session {named} is stored but not open yet: it is listed in the Desktop sidebar and in "
        f"`hermes sessions list`. Open it with `hermes --resume {session_id}` (or pick it in the Desktop sidebar) "
        "and send a message; the first turn gets a note about what changed. A gateway's list of open sessions "
        "shows it only after it was opened once."
    )


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
    withheld: Sequence[str] = (),
    continue_error: HandoverError | None = None,
    bundle_path: str | None = None,
    bundle_bytes: int | None = None,
    keep: bool = False,
    remote_gateway: bool = False,
    job: str | None = None,
    target_session: Mapping | None = None,
) -> str:
    """Success text naming target session, folder, model, offline capability and the next step (R14, BX P6).

    ``state`` is the conductor's final state; for a pull, anything before ``retired``/``done`` means the remote
    source is released on the next contact (SK L8). ``withheld`` are the paths a pull asked for (``--include``) that
    the source does not release (V29, R18: local hints only narrow); the message lists them. Without ``ack`` (the
    target's acknowledgement is not recorded yet) the text names no session, model or offline capability — it points
    to ``/handover status <job>`` instead (L3-F03: never "session ?, model default").

    ``target_session`` (additive, L17) is the job's ``{"id", "title"}`` of the session the handover created on the
    target (cluster A, ``JobRunner.status``); with it (or the ack) the text names title and id and says exactly how to
    open that session (``open_text``) — a received session is stored, not open, until someone opens it.
    """
    mode = Mode(mode)
    peer_name = peer or "the other gateway"
    arrived = _target_session(target_session, ack)
    t = (
        (ack.title if ack is not None and ack.title else None)
        or (arrived or {}).get("title")
        or title
        or "this session"
    )
    lists = {
        "excluded_secrets": excluded_secrets,
        "withheld": withheld,
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
    if ack is not None:
        where = ", ".join(
            x for x in (f"session {ack.session_id}", _ack_cwd(ack), f"model {ack.model or 'default'}") if x
        )
        details = f"{where}; works offline: {_yn(ack.offline_capable)}"
    else:
        details = "the session details are not recorded yet" + (
            f"; /handover status {short_job(job)} shows them" if job else ""
        )
    if mode in (Mode.PUSH, Mode.OFFER):
        lines = [
            f'Handed over to {peer_name}: "{t}" continues there ({details}). This session is now '
            "read-only here; /handover reclaim takes it back."
        ]
        if continue_result is not None and continue_result.started:
            lines.append(f"The agent is continuing on {peer_name}.")
        else:
            if continue_error is not None:
                lines.append(render_error(continue_error, peer))
            if arrived is not None:
                lines.append(f"On {peer_name}: {open_text(arrived['id'], t)}")
        lines += _note_block(None, None, {"excluded_secrets": excluded_secrets, "warnings": lists["warnings"]})
        return "\n".join(lines)
    # pull, import: the session is here now
    retired = str(state).lower() in {"retired", "continuing", "done"}
    if mode is Mode.PULL and not retired:
        lines = [f'"{t}" is active here ({details}); {peer_name} will be released on the next contact.']
    else:
        lines = [f'"{t}" is here now ({details}).']
    if continue_result is not None and continue_result.started:
        lines.append("The agent is continuing in that session now.")
    else:
        if continue_error is not None:
            lines.append(render_error(continue_error, None))
        if arrived is not None:
            lines.append(open_text(arrived["id"], t))
        else:
            lines.append(
                "Open it from the Desktop sidebar or `hermes sessions list` and send a message; the first turn gets "
                "a note about what changed."
            )
    lines += _note_block(note, origin, lists)
    return "\n".join(lines)
