"""The one reader of the plugin settings (layer L1, stdlib only; DESIGN §14.4, contract V22).

``SCHEMA`` mirrors ``plugin.yaml`` ``config_schema`` (the engine never reads the manifest; a unit test keeps both
identical). ``normalize`` turns ``plugins.entries.ctx_handover.settings`` as loaded from ``config.yaml`` into a
fresh dict with exactly the keys of ``DEFAULTS``. Hermes only warns about a bad value at load, so nothing from
``config.yaml`` is trusted: a value of the wrong type, outside its choices, negative, not finite or empty is the
schema default, and every rejected value is a warning (once per process on the ``ctx_handover.settings`` logger;
``problems`` returns the same sentences without logging, for doctor and setup).

YAML 1.1 turns a bare ``on``/``off``/``no``/``yes`` into a boolean and ``hermes config set`` turns ``1`` into an int.
A boolean setting reads the words of ``BOOL_WORDS`` (quoted words are no truthy strings). A word setting whose
choices hold ``off`` (integrator decision G′-K0 §1) reads every false word of ``BOOL_WORDS`` — any case, quoted or
bare, ``False``, the integer 0 — as ``off``, and every true word as ``on`` when ``on`` is one of its choices, else
as its default (no problem: "yes" only says "not off"); a word setting without ``off`` takes only its own words.
Any other value is the default and a problem — except ``inbound`` (``FAIL_CLOSED_KEYS``), where it is ``off``: the
default ``auto`` would answer paired peers. ``None`` — an empty value in ``config.yaml`` or ``hermes config set …
null`` — is the default, never ``off``/``False``.

Secret keys (``SECRET_ENVS``) are not settings: their values live in ``.env`` and never appear in the result.
"""

from __future__ import annotations

import logging
import math
import re
import threading
from collections.abc import Mapping
from types import MappingProxyType
from typing import Any

PLUGIN_ID = "ctx_handover"

log = logging.getLogger("ctx_handover.settings")


def _spec(typ: str, default: Any = None, choices: tuple[str, ...] = (), env: str | None = None) -> Mapping:
    spec: dict[str, Any] = {"type": typ}
    if typ == "secret":
        spec["env"] = env
    else:
        spec["default"] = default
    if choices:
        spec["choices"] = choices
    return MappingProxyType(spec)


SCHEMA: Mapping[str, Mapping[str, Any]] = MappingProxyType(
    {
        "inbound": _spec("str", "auto", ("auto", "on", "off")),
        "listen_url": _spec("str", ""),
        "peer_gate": _spec("str", "session_token", ("session_token", "bearer", "basic_login")),
        "peer_token": _spec("secret", env="CTX_HANDOVER_PEER_TOKEN"),
        "peer_username": _spec("str", ""),
        "peer_password": _spec("secret", env="CTX_HANDOVER_PEER_PASSWORD"),
        "peer_extra_headers": _spec("dict", {}),
        "workspace_root": _spec("str", "~/hermes-handover"),
        "allow_roots": _spec("list", ["~/hermes-handover"]),
        "export_roots": _spec("list", []),
        "max_session_mb": _spec("int", 512),
        "max_file_mb": _spec("int", 100),
        "max_extra_dir_mb": _spec("int", 256),
        "max_hermes_file_mb": _spec("int", 64),
        "max_files": _spec("int", 200000),
        "max_untracked_files": _spec("int", 200000),
        "max_total_gb": _spec("float", 10.0),
        "confirm_above_mb": _spec("int", 500),
        "max_request_mb": _spec("int", 8),
        "block_cache_gb": _spec("float", 5.0),
        "block_cache_days": _spec("int", 3),
        "staging_ttl_hours": _spec("int", 24),
        "lease_ttl_s": _spec("int", 120),
        "participant_idle_timeout_min": _spec("int", 15),
        "stall_timeout_min": _spec("int", 30),
        "wait_timeout_min": _spec("int", 30),
        "refine": _spec("str", "auto", ("auto", "side_question", "llm", "off")),
        "refine_timeout_s": _spec("int", 45),
        "refine_history_chars": _spec("int", 60000),
        "refine_side_question_max_tokens": _spec("int", 200000),
        "refine_auto_include": _spec("bool", False),
        "extra_paths": _spec("str", "ask", ("ask", "auto", "off")),
        "include_hermes_files": _spec("bool", True),
        "model_mapping": _spec("str", "auto", ("auto", "target_default")),
        "allow_remote_continue": _spec("bool", False),
        "allow_agent_start": _spec("bool", False),
        "accept_offers": _spec("str", "ask", ("ask", "off")),
        "extra_ignore": _spec("list", []),
        "tool_probe": _spec("list", []),
        "ctx_persist": _spec("str", "auto", ("auto", "off")),
        "ctx_category": _spec("str", "handover"),
    }
)
"""DESIGN §14.4 = ``plugin.yaml`` ``config_schema``: ``type``, ``default`` (``env`` for secrets), ``choices``."""

DEFAULTS: Mapping[str, Any] = MappingProxyType(
    {key: spec["default"] for key, spec in SCHEMA.items() if spec["type"] != "secret"}
)
"""Every setting with its schema default. Read-only; ``normalize`` hands out copies."""

SECRET_ENVS: Mapping[str, str] = MappingProxyType(
    {key: spec["env"] for key, spec in SCHEMA.items() if spec["type"] == "secret"}
)
"""Secret keys and the ``.env`` variable holding each value (never part of the settings)."""

BOOL_WORDS: Mapping[str, bool] = MappingProxyType(
    {
        **dict.fromkeys(("true", "yes", "on", "1"), True),
        **dict.fromkeys(("false", "no", "off", "0"), False),
    }
)
"""Words a boolean setting may hold as text (``hermes_cli/config.py`` ``_SCALAR_WORDS`` plus ``1``/``0``)."""

POSITIVE_KEYS: frozenset[str] = frozenset(
    {"lease_ttl_s", "participant_idle_timeout_min", "stall_timeout_min", "staging_ttl_hours", "max_request_mb"}
)
"""Integer settings where 0 cannot work (a lease, an idle or stall timer, the staging time, the request cap): 0 is
the default there. Every other number may be 0 (``confirm_above_mb: 0`` = always ask)."""

FAIL_CLOSED_KEYS: frozenset[str] = frozenset({"inbound"})
"""Word settings where a value the reader does not recognise is ``off``, not the schema default (fail-closed: the
default ``auto`` of ``inbound`` answers paired peers). ``None`` is still the default."""

_UNSET_WORDS = frozenset({"", "null", "none", "~"})
_HEADER_RE = re.compile(r"^[A-Za-z0-9-]{1,64}$")
_ENV_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]{0,127}$")
_QUOTE_MAX = 40
_WARNED_MAX = 512
_warned: set[str] = set()
_warned_lock = threading.Lock()


class _Rejected(Exception):
    """A value (or part of it) the schema does not accept; ``reason`` completes "… ignored: <reason>"."""

    def __init__(self, reason: str) -> None:
        super().__init__(reason)
        self.reason = reason


def _copy(value: Any) -> Any:
    if isinstance(value, list):
        return list(value)
    if isinstance(value, Mapping):
        return dict(value)
    return value


def _unset(value: object) -> bool:
    return value is None or (isinstance(value, str) and value.strip().lower() in _UNSET_WORDS)


def _bool(value: object) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        word = BOOL_WORDS.get(value.strip().lower())
        if word is not None:
            return word
    elif isinstance(value, int) and value in (0, 1):
        return bool(value)
    raise _Rejected("not a boolean (use true or false)")


def _truth(value: object) -> bool | None:
    """The boolean a YAML boolean, the integer 0/1 or a word of ``BOOL_WORDS`` stands for; None for anything else."""
    if isinstance(value, bool):
        return value
    if isinstance(value, int):
        return bool(value) if value in (0, 1) else None
    if isinstance(value, str):
        return BOOL_WORDS.get(value.strip().lower())
    return None


def _word(spec: Mapping, value: object) -> str | None:
    """The canonical choice ``value`` names; None when it is a true word of a setting without the choice ``on``
    (the default, silently). See the module docstring."""
    choices: tuple[str, ...] = spec.get("choices") or ()
    if isinstance(value, str) and value.strip().lower() in choices:
        return value.strip().lower()
    truth = _truth(value)
    if truth is not None and "off" in choices:
        if not truth:
            return "off"
        return "on" if "on" in choices else None
    if isinstance(value, bool):
        raise _Rejected(f"a bare {'on' if value else 'off'} is not one of {', '.join(choices)}")
    raise _Rejected(f"not one of {', '.join(choices)}")


def _text(value: object) -> str:
    if isinstance(value, str):
        return value
    raise _Rejected("not a text (quote it)")


def _number(key: str, value: object, *, integer: bool) -> int | float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise _Rejected("not a number (write it without quotes)")
    if isinstance(value, float):
        if not math.isfinite(value):
            raise _Rejected("not a finite number")
        if integer and not value.is_integer():
            raise _Rejected("not a whole number")
    if value < 0:
        raise _Rejected("negative")
    if key in POSITIVE_KEYS and value < 1:
        raise _Rejected("must be at least 1")
    if integer:
        return int(value)
    try:
        return float(value)
    except OverflowError:
        raise _Rejected("too large") from None


def _list(value: object, dropped: list[str]) -> list[str]:
    if not isinstance(value, (list, tuple)):
        raise _Rejected("not a list (write [a, b])")
    out: list[str] = []
    for item in value:
        if isinstance(item, str) and item.strip():
            out.append(item)
        else:
            dropped.append(_quote(item))
    return out


def _headers(value: object, dropped: list[str]) -> dict[str, str]:
    if not isinstance(value, Mapping):
        raise _Rejected("not a list of header name: .env variable name pairs")
    out: dict[str, str] = {}
    for name, env in value.items():
        if isinstance(name, str) and isinstance(env, str) and _HEADER_RE.fullmatch(name) and _ENV_RE.fullmatch(env):
            out[name] = env
        else:
            dropped.append(_quote(name))  # the header name only: a misplaced value may be a secret
    return out


def _quote(value: object) -> str:
    text = repr(value)
    return text if len(text) <= _QUOTE_MAX else text[: _QUOTE_MAX - 1] + "…"


def _check(key: str, value: object) -> tuple[Any, list[str]]:
    """``(normalised value, problems)`` of one schema key; the default when the value is rejected."""
    spec = SCHEMA[key]
    typ = spec["type"]
    default = spec.get("default")
    if _unset(value):
        return _copy(default), []
    dropped: list[str] = []
    try:
        if typ == "bool":
            out: Any = _bool(value)
        elif typ == "str" and spec.get("choices"):
            word = _word(spec, value)
            out = _copy(default) if word is None else word
        elif typ == "str":
            out = _text(value)
        elif typ in ("int", "float"):
            out = _number(key, value, integer=typ == "int")
        elif typ == "list":
            out = _list(value, dropped)
        elif typ == "dict":
            out = _headers(value, dropped)
        else:  # pragma: no cover - SCHEMA holds no other type
            raise _Rejected(f"unknown type {typ}")
    except _Rejected as exc:
        shown = "" if typ == "dict" else f" {_quote(value)}"
        if key in FAIL_CLOSED_KEYS:
            text = (
                f"{key}{shown} ignored: {exc.reason}; using off, the safe choice: no handovers are accepted "
                f"({_fix(key)})"
            )
            return "off", [text]
        text = f"{key}{shown} ignored: {exc.reason}; using the default {_quote(default)} ({_fix(key)})"
        return _copy(default), [text]
    found = []
    if dropped:
        if typ == "dict":
            what, rule = "header names", "each entry must be Header-Name: ENV_VARIABLE_NAME"
        else:
            what, rule = "entries", "each entry must be a non-empty text"
        found.append(f"{key}: {what} {', '.join(dropped)} ignored ({rule}; {_fix(key)})")
    return out, found


def _fix(key: str) -> str:
    return f"fix with: hermes config set plugins.entries.{PLUGIN_ID}.settings.{key} <value>"


def _scan(raw: Mapping | None) -> tuple[dict[str, Any], list[str]]:
    out = {key: _copy(default) for key, default in DEFAULTS.items()}
    found: list[str] = []
    if raw is None:
        return out, found
    if not isinstance(raw, Mapping):
        return out, [
            f"plugins.entries.{PLUGIN_ID}.settings is not a list of key: value pairs; every setting uses its default"
        ]
    for key, spec in SCHEMA.items():
        if key not in raw:
            continue
        if spec["type"] == "secret":
            if not _unset(raw[key]):
                found.append(
                    f"{key} is a secret: put it into .env as {spec['env']} (the settings form does that), not into "
                    "config.yaml; the value there is ignored"
                )
            continue
        out[key], rejected = _check(key, raw[key])
        found.extend(rejected)
    return out, found


def raw_settings(config: Mapping | None) -> dict:
    """``plugins.entries.ctx_handover.settings`` of a loaded config as given (a shallow copy); ``{}`` when it is
    absent or not a mapping."""
    node: object = config
    for part in ("plugins", "entries", PLUGIN_ID, "settings"):
        if not isinstance(node, Mapping):
            return {}
        node = node.get(part)
    return dict(node) if isinstance(node, Mapping) else {}


def normalize(raw: Mapping | None) -> dict:
    """Every setting of ``DEFAULTS`` from ``raw`` (fresh lists and dicts); a rejected value is the schema default
    and a warning, unknown and secret keys are dropped. See the module docstring for the rules."""
    out, found = _scan(raw)
    for text in found:
        _warn_once(text)
    return out


def from_config(config: Mapping | None) -> dict:
    """``normalize`` of ``plugins.entries.ctx_handover.settings`` of a loaded config."""
    return normalize(raw_settings(config))


def problems(raw: Mapping | None) -> list[str]:
    """One sentence per value ``normalize`` rejects (schema order; no logging). Values of secret keys and of
    ``peer_extra_headers`` are never quoted."""
    return _scan(raw)[1]


def coerce(key: str, value: object) -> Any:
    """One setting by the same rules as ``normalize`` (no warning); ``KeyError`` for an unknown or secret key."""
    if key not in DEFAULTS:
        raise KeyError(key)
    return _check(key, value)[0]


def flag(value: object, default: bool = False) -> bool:
    """A boolean outside the schema (``hosted``, ``operator_api``): a bool, a word of ``BOOL_WORDS`` or 0/1;
    anything else, ``None`` included, is ``default``."""
    if _unset(value):
        return default
    try:
        return _bool(value)
    except _Rejected:
        return default


def positive_int(value: object, default: int) -> int:
    """A whole number ≥ 1 outside the schema (job caps such as ``max_jobs_per_peer``); anything else is
    ``default``."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return default
    if isinstance(value, float) and not (math.isfinite(value) and value.is_integer()):
        return default
    return int(value) if value >= 1 else default


def _warn_once(text: str) -> None:
    with _warned_lock:
        if text in _warned:
            return
        if len(_warned) < _WARNED_MAX:
            _warned.add(text)
    log.warning("ctx_handover setting: %s", text)


__all__ = [
    "BOOL_WORDS",
    "DEFAULTS",
    "FAIL_CLOSED_KEYS",
    "PLUGIN_ID",
    "POSITIVE_KEYS",
    "SCHEMA",
    "SECRET_ENVS",
    "coerce",
    "flag",
    "from_config",
    "normalize",
    "positive_int",
    "problems",
    "raw_settings",
]
