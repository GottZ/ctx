"""Global settings of the plugin (Hermes configuration, DESIGN §5.1) for the settings page (§2.4).

They do not live in the policy store: reading uses the active profile's Hermes configuration (fail-closed
like ``config.operators``), writing goes only through Hermes' own settings writer
(``plugins_settings.save_plugin_settings``: schema-checked, file-locked, refuses managed installs). A digest
of the values read guards against overwriting a change made elsewhere since the page loaded them.
"""

from __future__ import annotations

import hashlib
import json
import re
from typing import Any, Mapping, Optional

from .. import config
from ..store.schema import HARD_PROMPT_LIMIT, is_snowflake

KEYS: dict[str, dict[str, Any]] = {
    "operators": {"label": "Operators", "kind": "user_list",
                  "help": "Discord users with tool authority. Only settable here or on the host, never from "
                          "Discord."},
    "operator_label": {"label": "Operator label", "kind": "text", "max_length": 100,
                       "help": "How public texts refer to the operator ({operator_label})."},
    "guild_prompt_max_chars": {"label": "Guidance limit", "kind": "int", "min": 0, "max": HARD_PROMPT_LIMIT,
                               "help": f"Upper bound for each server's guidance (hard ceiling {HARD_PROMPT_LIMIT})."},
    "notify_operators": {"label": "Direct-message operators", "kind": "bool",
                         "help": "About activation requests, guidance changes and degraded protection."},
    "frame_prompt": {"label": "Operator frame", "kind": "frame", "max_length": HARD_PROMPT_LIMIT,
                     "help": "Placed before every server's guidance. Empty: the built-in neutral frame."},
    "member_search": {"label": "Member search", "kind": "choice",
                      "choices": [{"value": "auto", "label": "On (prefix search)"}, {"value": "off", "label": "Off"}],
                      "help": "Suggest server members by name when adding operators. Off: user IDs only."},
}
MEMBER_SEARCH = ("auto", "off")

_CONTROL = re.compile(r"[\x00-\x08\x0b\x0c\x0e-\x1f\x7f\x85  ]")
_LINE_BREAK = re.compile(r"[\r\n\x0b\x0c\x85  ]")


class SettingsError(ValueError):
    def __init__(self, field_name: str, message: str) -> None:
        super().__init__(message)
        self.field = field_name


def normalise(raw: Mapping[str, Any]) -> dict[str, Any]:
    """The values the plugin acts on, coerced like ``dsp.config`` does (a wrong type reads as the default)."""
    out: dict[str, Any] = {}
    for key, default in config.DEFAULTS.items():
        if key not in KEYS:
            continue
        value = raw.get(key, default)
        if key == "operators":
            items = value if isinstance(value, (list, tuple)) else []
            out[key] = [str(v).strip() for v in items
                        if (isinstance(v, str) or (isinstance(v, int) and not isinstance(v, bool)))
                        and is_snowflake(str(v).strip())]
        elif isinstance(default, bool):
            out[key] = value if isinstance(value, bool) else default
        elif isinstance(default, int):
            ok = isinstance(value, int) and not isinstance(value, bool)
            out[key] = max(0, min(value, HARD_PROMPT_LIMIT)) if ok else default
        else:
            out[key] = value if isinstance(value, str) else default
    out["member_search"] = member_search_of(raw)
    return out


def member_search_of(raw: Any) -> str:
    """``auto`` only when the setting says exactly that (or is absent); anything else is ``off``. YAML reads
    an unquoted ``off`` as False, and a value nobody can interpret must not switch Discord lookups on."""
    search = raw.get("member_search", "auto") if isinstance(raw, Mapping) else None
    return "auto" if search == "auto" else "off"


def digest(values: Mapping[str, Any]) -> str:
    return hashlib.sha256(json.dumps(values, sort_keys=True, ensure_ascii=False).encode("utf-8")).hexdigest()


def read(backend: Any) -> dict[str, Any]:
    """``{values, digest, readable, error}``. Unreadable configuration ⇒ defaults, ``readable`` False."""
    try:
        raw = backend.read_settings()
    except Exception as exc:  # ConfigUnreadable or Hermes missing: nothing is trusted, nothing written
        values = normalise({})
        values["operators"] = []
        return {"values": values, "digest": digest({"unreadable": True}), "readable": False,
                "error": f"The Hermes configuration could not be read ({type(exc).__name__}). Nothing can be "
                         "saved here until config.yaml is fixed."}
    values = normalise(raw)
    return {"values": values, "digest": digest(values), "readable": True, "error": None}


def validate(values: Any) -> dict[str, Any]:
    """Check a set of changes before Hermes' writer sees them (SETTINGS-PAGE §2.4)."""
    if not isinstance(values, Mapping) or not values:
        raise SettingsError("values", "No settings given.")
    clean: dict[str, Any] = {}
    for key, value in values.items():
        if key not in KEYS:
            raise SettingsError(str(key), f"{key} is not a setting of this plugin.")
        if key == "operators":
            if not isinstance(value, list) or len(value) > 100:
                raise SettingsError(key, "Operators must be a list of at most 100 Discord user IDs.")
            ids: list[str] = []
            for entry in value:
                text = str(entry).strip() if isinstance(entry, (str, int)) and not isinstance(entry, bool) else ""
                if not is_snowflake(text):
                    raise SettingsError(key, f"{entry!r} is not a Discord user ID (17–20 digits).")
                if text not in ids:
                    ids.append(text)
            clean[key] = ids
        elif key == "guild_prompt_max_chars":
            if isinstance(value, bool) or not isinstance(value, int) or not 0 <= value <= HARD_PROMPT_LIMIT:
                raise SettingsError(key, f"The guidance limit must be a whole number from 0 to {HARD_PROMPT_LIMIT}.")
            clean[key] = value
        elif key == "notify_operators":
            if not isinstance(value, bool):
                raise SettingsError(key, "Expected on or off.")
            clean[key] = value
        elif key == "operator_label":
            if not isinstance(value, str) or _LINE_BREAK.search(value) or _CONTROL.search(value) \
                    or len(value) > 100 or not value.strip():
                raise SettingsError(key, "The operator label must be one line of 1–100 characters.")
            clean[key] = value
        elif key == "member_search":
            if value not in MEMBER_SEARCH:
                raise SettingsError(key, "Member search is auto or off.")
            clean[key] = value
        elif key == "frame_prompt":
            if not isinstance(value, str) or _CONTROL.search(value) or len(value) > HARD_PROMPT_LIMIT:
                raise SettingsError(key, f"The operator frame must be text of at most {HARD_PROMPT_LIMIT} characters "
                                         "without control characters.")
            clean[key] = value
    return clean


def prompt_cap(backend: Any) -> int:
    return int(read(backend)["values"]["guild_prompt_max_chars"])


def assemble_settings(backend: Any) -> dict[str, Any]:
    """The settings mapping ``prompts.assemble`` expects."""
    values = read(backend)["values"]
    return {k: values[k] for k in ("frame_prompt", "operator_label", "guild_prompt_max_chars")}


def operator_count(backend: Any) -> tuple[int, bool]:
    state = read(backend)
    return len(state["values"]["operators"]), bool(state["readable"])


def member_search_mode(backend: Any) -> str:
    """The operator's member_search setting; ``off`` while the configuration cannot be read."""
    state = read(backend)
    return str(state["values"]["member_search"]) if state["readable"] else "off"


def keys() -> list[dict[str, Any]]:
    return [{"key": k, **v} for k, v in KEYS.items()]


def write(backend: Any, plugin_dir: Any, expected_digest: Optional[str], values: Any) -> dict[str, Any]:
    """Validate, compare the digest with a fresh read, write through Hermes, notify. Raises on refusal."""
    clean = validate(values)
    current = read(backend)
    if not current["readable"]:
        raise PermissionError(current["error"])
    if expected_digest != current["digest"]:
        raise ConflictingSettings(current["digest"])
    try:
        written = backend.write_settings(plugin_dir, clean)
    except PermissionError as exc:
        # Hermes writes key by key: a managed key late in the set can refuse after earlier keys were saved.
        after = read(backend)
        if after["readable"] and after["digest"] != current["digest"]:
            changed = sorted(k for k in clean if after["values"].get(k) != current["values"].get(k))
            backend.settings_changed(changed)
            raise PartlySaved(str(exc) or "a setting is managed by the administrator", changed) from exc
        raise
    backend.settings_changed(written)
    after = read(backend)
    return {"values": after["values"], "digest": after["digest"], "written": sorted(written)}


class ConflictingSettings(RuntimeError):
    def __init__(self, current_digest: str) -> None:
        super().__init__("The settings changed since this page loaded them.")
        self.digest = current_digest


class PartlySaved(PermissionError):
    def __init__(self, message: str, written: list) -> None:
        super().__init__(message)
        self.written = written
