"""ctx_checkpoint — the ctx memory provider for Hermes.

One plugin, one provider name, one settings section (``memory.ctx_checkpoint`` in Hermes'
``config.yaml``), two layers:

* **Checkpoint** (always on): before every compaction the direct user/assistant transcript
  is archived into ctx as a head → manifest → parts chain — deterministically, with no LLM
  in the critical path — and the block IDs are handed to Hermes for the compaction
  summary. With ``compression.checkpoint_required`` the compaction is blocked until the
  checkpoint is durable (checkpoint API v2, negotiated against the loading host).
* **Memory features** (``memory: true``): recall before each turn, the agent tools
  ``ctx_query`` / ``ctx_store`` / ``ctx_search`` / ``ctx_get``, a mirror of Hermes'
  ``MEMORY.md`` / ``USER.md`` writes, fact extraction from the compacted turns with
  cross-compaction dedupe, per-segment session digests and the ``hermes ctx_checkpoint``
  CLI. The engine (``memory/``) was contributed by derLonius (W4-NERF) as
  hermes-ctx-plugin and is maintained here.

Transport: by default every call goes through Hermes' own MCP dispatch to the ctx server
configured in ``mcp_servers.<mcp_server>`` — the plugin holds no credentials. ``transport:
rest`` switches to the engine's REST client (``CTX_BASE_URL`` / ``CTX_KEY`` in the
profile's ``.env``), which enables the CLI's read/write commands. Recall is the semantic
``/api/query`` retrieval without the synthesis step on both transports (over MCP with ctx
v5.36.0 or newer; an older server gets the lexical ``search``).

Setup runs through Hermes' own surfaces, all built from the field table in
``config_schema.py``: Hermes Desktop's memory form (Settings → Memory) reads that file's
``CONFIG_SCHEMA`` and goes through ``read_config_fields()`` / ``write_config_fields()``; the web
dashboard's memory form reads ``get_config_schema()`` and writes through ``save_config()``;
``hermes memory setup ctx_checkpoint`` hands over to ``post_setup()``. The Settings → Plugins
section (``config_schema`` in ``plugin.yaml``) holds the keys the memory form does not carry.
Saving tests the connection (``memory/status.py``), every checkpoint leaves its outcome in
``ctx_state/``, and ``hermes memory status`` reports both. Compaction thresholds stay Hermes'
own; ``compression.checkpoint_required`` is written only on an explicit choice.
"""

from __future__ import annotations

import getpass
import hashlib
import logging
import re
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

from .config_schema import FIELDS as _FORM_FIELDS
from .memory import checkpoint as _cp
from .memory import status as _st
from .memory.client import CtxApiError
from .memory.config import (
    UMBRELLA_KEY_MAP,
    overlay_from_umbrella,
    umbrella_memory_enabled,
    umbrella_section,
    umbrella_transport,
)
from .memory.mcp_client import McpClient, colliding_servers, collision_text, mcp_tool_name
from .memory.provider import CtxMemoryProvider

logger = logging.getLogger(__name__)

# -- Re-exports (test seams and the public names earlier versions had) -------------------
CtxCheckpointError = _cp.CtxCheckpointError
_redact_checkpoint_secrets = _cp._redact_checkpoint_secrets
# Host-side marker of the v3 contract: the helper in ``agent.conversation_compression``
# that collects tool evidence for the pre-compress memory handoff. Its presence (as a
# callable) is what distinguishes a real v3 host from one that merely bumped the constant.
HOST_V3_FEATURE = _cp.HOST_V3_FEATURE
_HOST_CONTRACT_MODULE = _cp._HOST_CONTRACT_MODULE
_HOST_FEATURE_MODULE = _cp._HOST_FEATURE_MODULE
_HOST_CONTRACT_CONSTANT = _cp._HOST_CONTRACT_CONSTANT
# Assumed host contract when the probe cannot decide: 2 is the conservative,
# gate-compatible answer (never claims the tool-evidence channel, accepted by a v1 gate).
_PROBE_FALLBACK_API = _cp._PROBE_FALLBACK_API


def _probe_host_checkpoint_api() -> int:
    """Checkpoint contract version of the loading host (engine probe with this module's names)."""
    return _cp._probe_host_checkpoint_api(
        _HOST_CONTRACT_MODULE, _HOST_FEATURE_MODULE, _HOST_CONTRACT_CONSTANT, HOST_V3_FEATURE, _PROBE_FALLBACK_API
    )


# Highest checkpoint contract THIS plugin implements. 2 = the fail-closed contract as
# merged upstream (NousResearch/hermes-agent#94639, 2026-08-25). Raised to 3 once the
# tool-index renderer lands and ``tool_evidence`` is actually rendered into the checkpoint
# — until then the plugin must never declare 3, even on a host that offers it.
PLUGIN_CHECKPOINT_API_MAX = 2
# Injection seams for tests (the chain writer's clock and sleep).
_sleep = time.sleep
_monotonic = time.monotonic


def _negotiate_checkpoint_api(host_api: int) -> int:
    """Contract this plugin declares: ``max(2, min(host, PLUGIN_CHECKPOINT_API_MAX))``.

    Read at call time so raising the ceiling flips the negotiation without touching this
    function; a v1 host's gate (``>= 1``) accepts the floor of 2.
    """
    return max(2, min(int(host_api), PLUGIN_CHECKPOINT_API_MAX))


# -- Settings ------------------------------------------------------------------------------

_DEFAULT_MCP_SERVER = "ctx"
_DEFAULT_CATEGORY = "compaction-checkpoints"
_DEFAULT_SENSITIVITY = "internal"
_DEFAULT_CHUNK_CHARS = _cp.DEFAULT_CHUNK_CHARS
_MIN_CHUNK_CHARS = _cp.MIN_CHUNK_CHARS
_MAX_CHUNK_CHARS = _cp.MAX_CHUNK_CHARS
_DEFAULT_BUDGET_S = 60.0
_MIN_BUDGET_S = 5.0
_MAX_BUDGET_S = 600.0
_DEFAULT_MAX_PARTS = _cp.DEFAULT_MAX_PARTS
_MAX_PARTS_CEILING = _cp.MAX_PARTS_CEILING
# ctx sensitivity levels, least to most sensitive (go/internal/backends/trust.go).
_SENSITIVITY_CHOICES = ("public", "internal", "personal", "credentials")
_MEMORY_CHOICES = ("off", "on")
_MCP_SERVER_NAME_RE = re.compile(r"[A-Za-z0-9_-]+")
# Setup stores the key in Hermes' .env and references it from the MCP server entry;
# the key itself is never written into config.yaml.
SETUP_API_KEY_ENV = _st.SETUP_API_KEY_ENV
_SETUP_AUTH_HEADERS = {"Authorization": "Bearer ${" + SETUP_API_KEY_ENV + "}"}
_SETUP_STORED_KEYS = ("mcp_server", "category", "sensitivity", "chunk_chars", "memory")
# Gateway session hygiene compacts at this fixed share of the context window
# (``gateway/run_turn_hygiene.py``, not configurable) and holds a turn for at most
# ``compression.hygiene_max_turn_hold_seconds`` (default 10 s) before the turn runs on
# the uncompressed transcript. A compressor threshold at or above it lets hygiene
# compact first.
_HOST_HYGIENE_THRESHOLD = 0.85
_hygiene_warning_emitted = False
# The checkpoint_mode warning is logged once per (profile home, text) and process: a gateway
# builds a provider per message. The session notice (warning callback) is sent each time.
_mode_warnings_logged: set[tuple[str, str]] = set()

SKILL_NAME = "ctx-memory"
_SKILL_PATH = Path(__file__).resolve().parent / "skills" / SKILL_NAME / "SKILL.md"
_SKILL_DESCRIPTION = (
    "How to use the ctx knowledge store as long-term memory and evidence layer: query as a "
    "question, search to browse, get by id, verify before acting, persist one durable fact per block."
)


def _clamped_chunk_chars(value: Any) -> int:
    try:
        number = int(value)
    except (TypeError, ValueError):
        number = _DEFAULT_CHUNK_CHARS
    return max(_MIN_CHUNK_CHARS, min(number, _MAX_CHUNK_CHARS))


def _clamped_budget_s(value: Any) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError):
        number = _DEFAULT_BUDGET_S
    return max(_MIN_BUDGET_S, min(number, _MAX_BUDGET_S))


def _clamped_max_parts(value: Any) -> int:
    try:
        number = int(value)
    except (TypeError, ValueError):
        number = _DEFAULT_MAX_PARTS
    return max(1, min(number, _MAX_PARTS_CEILING))


def _as_bool(value: Any, default: bool) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        lowered = value.strip().lower()
        if lowered in {"true", "1", "yes", "y", "on"}:
            return True
        if lowered in {"false", "0", "no", "n", "off"}:
            return False
    return default


def _clean_category(value: Any) -> str:
    return str(value or _DEFAULT_CATEGORY).strip() or _DEFAULT_CATEGORY


def _clean_sensitivity(value: Any) -> str:
    sensitivity = str(value or _DEFAULT_SENSITIVITY).strip().lower()
    return sensitivity if sensitivity in _SENSITIVITY_CHOICES else _DEFAULT_SENSITIVITY


# The checkpoint settings of the section (``UMBRELLA_KEY_MAP``): their normalizer, and the
# plugin's default, which sits UNDER ctx.json (``_config_base``) — it is never put into the
# section, so a value only ctx.json sets is not overridden by a default.
_CHECKPOINT_SETTINGS: dict[str, tuple[Callable[[Any], Any], Any]] = {
    "category": (_clean_category, _DEFAULT_CATEGORY),
    "sensitivity": (_clean_sensitivity, _DEFAULT_SENSITIVITY),
    "chunk_chars": (_clamped_chunk_chars, _DEFAULT_CHUNK_CHARS),
    "budget_s": (_clamped_budget_s, _DEFAULT_BUDGET_S),
    "max_parts": (_clamped_max_parts, _DEFAULT_MAX_PARTS),
}


def _umbrella_settings(section: dict[str, Any] | None) -> dict[str, Any]:
    """The ``memory.ctx_checkpoint`` section with the plugin's own keys defaulted and the
    checkpoint settings it sets clamped; a checkpoint setting it does not set stays absent.

    Engine keys written there under their own names pass through untouched (they are
    validated by the engine's loader).
    """
    raw = dict(section or {})
    out = dict(raw)
    out.update(
        {
            "mcp_server": str(raw.get("mcp_server") or _DEFAULT_MCP_SERVER),
            "memory": _as_bool(raw.get("memory"), False),
            "transport": umbrella_transport(raw),
        }
    )
    for key, (normalize, _default) in _CHECKPOINT_SETTINGS.items():
        if raw.get(key) is None:
            out.pop(key, None)
        else:
            out[key] = normalize(raw[key])
    return out


def _load_plugin_config() -> dict[str, Any]:
    """``memory.ctx_checkpoint`` with the Settings → Plugins values over it (``umbrella_section``)."""
    try:
        from hermes_cli.config import load_config

        return umbrella_section(load_config())
    except Exception:
        return {}


def _effective_config() -> dict[str, Any]:
    """Hermes' loaded config (defaults applied); ``{}`` when unreadable."""
    try:
        from hermes_cli.config import load_config

        config = load_config()
    except Exception:
        return {}
    return config if isinstance(config, dict) else {}


def _warn_if_hygiene_preempts() -> None:
    """Log once per process when ``compression.threshold`` lets gateway hygiene compact first."""
    global _hygiene_warning_emitted
    if _hygiene_warning_emitted:
        return
    _hygiene_warning_emitted = True
    try:
        from hermes_cli.config import load_config

        compression = load_config().get("compression", {})
        # Same truthiness as the gateway's hygiene gate: disabled compression has no hygiene.
        enabled = str(compression.get("enabled", True)).lower() in {"true", "1", "yes"}
        threshold = float(compression.get("threshold"))
    except Exception:
        return
    if enabled and threshold >= _HOST_HYGIENE_THRESHOLD:
        logger.warning(
            "compression.threshold %.3f is at or above the gateway hygiene trigger "
            "(%.2f of the context window): hygiene compacts before the compressor and "
            "holds a turn only for compression.hygiene_max_turn_hold_seconds, so turns "
            "can run on the uncompressed transcript. Keep compression.threshold below "
            "%.2f (Hermes default 0.50, floored at 0.75 below 512K windows).",
            threshold,
            _HOST_HYGIENE_THRESHOLD,
            _HOST_HYGIENE_THRESHOLD,
        )


def _read_user_config() -> dict[str, Any]:
    """config.yaml exactly as written: no defaults, no ``${VAR}`` expansion.

    Setup writes go through this read so env-var templates (and the secrets behind
    them) are never materialized into config.yaml.
    """
    from hermes_cli.config import read_user_config_raw

    return read_user_config_raw()


def _configured_server(config: dict[str, Any], name: str) -> dict[str, Any]:
    servers = config.get("mcp_servers")
    entry = servers.get(name) if isinstance(servers, dict) else None
    return entry if isinstance(entry, dict) else {}


def _server_setup_mode(entry: dict[str, Any]) -> str:
    """How far setup may manage a ctx MCP server entry.

    ``"stdio"``: a ``command`` entry — transport and auth stay untouched. ``"foreign"``:
    the entry authenticates on its own (``auth``, or headers that do not reference
    ``CTX_API_KEY``) — setup keeps that auth and asks for no key. ``"managed"``: its
    headers reference ``CTX_API_KEY``. ``"new"``: no entry, or one without ``headers`` or
    ``auth`` — setup writes the Bearer header and needs the key.
    """
    if not entry:
        return "new"
    if entry.get("command"):
        return "stdio"
    if entry.get("auth"):
        return "foreign"
    headers = entry.get("headers")
    if not headers:
        return "new"
    reference = "${" + SETUP_API_KEY_ENV + "}"
    values = headers.values() if isinstance(headers, dict) else [headers]
    return "managed" if any(reference in str(value) for value in values) else "foreign"


def _normalize_setup_values(values: dict[str, Any], current: dict[str, Any]) -> dict[str, Any]:
    """Validate setup input; blank fields fall back to the current config, then defaults."""

    def text(key: str, default: str) -> str:
        raw = values.get(key)
        if raw is None or str(raw).strip() == "":
            raw = current.get(key)
        value = str(raw).strip() if raw is not None else ""
        return value or default

    mcp_server = text("mcp_server", _DEFAULT_MCP_SERVER)
    if not _MCP_SERVER_NAME_RE.fullmatch(mcp_server):
        raise ValueError(f"MCP server name {mcp_server!r} may only contain letters, digits, '_' and '-'")
    sensitivity = text("sensitivity", _DEFAULT_SENSITIVITY)
    if sensitivity not in _SENSITIVITY_CHOICES:
        raise ValueError(f"sensitivity must be one of {', '.join(_SENSITIVITY_CHOICES)}, not {sensitivity!r}")
    raw_chunk = text("chunk_chars", str(_DEFAULT_CHUNK_CHARS))
    try:
        chunk_chars = int(raw_chunk)
    except ValueError:
        raise ValueError(f"chunk_chars must be a whole number, not {raw_chunk!r}") from None
    memory_raw = text("memory", "on" if _as_bool(current.get("memory"), False) else "off").lower()
    if memory_raw not in _MEMORY_CHOICES and memory_raw not in {"true", "false", "1", "0", "yes", "no"}:
        raise ValueError(f"memory must be one of {', '.join(_MEMORY_CHOICES)}, not {memory_raw!r}")
    ctx_url = str(values.get("ctx_url") or "").strip()
    if ctx_url and not ctx_url.startswith(("https://", "http://", "${")):
        raise ValueError(f"ctx server URL must start with https:// or http://, not {ctx_url!r}")
    return {
        "mcp_server": mcp_server,
        "category": text("category", _DEFAULT_CATEGORY),
        "sensitivity": sensitivity,
        "chunk_chars": _clamped_chunk_chars(chunk_chars),
        "memory": _as_bool(memory_raw, False),
        "ctx_url": ctx_url,
    }


def _switch_value(values: dict[str, Any], key: str) -> bool | None:
    """An on/off form value; ``None`` when not submitted (or blank)."""
    if key not in values:
        return None
    raw = values[key]
    if isinstance(raw, bool):
        return raw
    text = str(raw if raw is not None else "").strip().lower()
    if not text:
        return None
    if text in {"on", "true", "1", "yes"}:
        return True
    if text in {"off", "false", "0", "no"}:
        return False
    raise ValueError(f"{key} must be on or off, not {raw!r}")


class ConfigReadOnlyError(RuntimeError, ValueError):
    """config.yaml cannot be written. A ``ValueError`` as well, so Hermes' settings routes refuse
    the save with its text (400) instead of an internal error."""


def _require_writable_config() -> None:
    from hermes_cli.config import is_managed

    if is_managed():
        raise ConfigReadOnlyError("this Hermes install is package-managed; config.yaml is read-only")


def _write_setup_config(settings: dict[str, Any], *, activate: bool, checkpoint_required: bool | None = None) -> None:
    """Write the provider section and the ctx MCP server entry into config.yaml.

    Only these subtrees are written, merged over the file as it is on disk at
    write time (``merge_existing``), so concurrent edits to other sections survive.
    An existing server entry is kept: a disabled one is re-enabled, a ``command``
    (stdio) entry is otherwise left alone, the ``url`` of an HTTP entry follows the
    setup input, and the Bearer header referencing ``CTX_API_KEY`` is added only when
    the entry carries neither ``headers`` nor ``auth``. Of the compaction settings only
    ``compression.checkpoint_required`` is written, and only when the caller passes the
    user's explicit choice (``checkpoint_required`` not ``None``); thresholds never.
    """
    from hermes_cli.config import save_config

    _require_writable_config()
    config = _read_user_config()
    name = settings["mcp_server"]
    ctx_url = settings["ctx_url"]
    entry = dict(_configured_server(config, name))
    mode = _server_setup_mode(entry)
    if entry:
        if entry.get("enabled") is False:
            entry["enabled"] = True
        if mode != "stdio":
            if ctx_url:
                entry["url"] = ctx_url
            if mode == "new":
                entry["headers"] = dict(_SETUP_AUTH_HEADERS)
    elif ctx_url:
        entry = {"url": ctx_url, "headers": dict(_SETUP_AUTH_HEADERS)}
    else:
        raise ValueError(f"ctx server URL is required: there is no MCP server entry named {name!r} yet")

    memory_section = config.get("memory")
    section = memory_section.get("ctx_checkpoint") if isinstance(memory_section, dict) else None
    memory: dict[str, Any] = {"ctx_checkpoint": _stored_section(settings, section)}
    if activate:
        memory["provider"] = "ctx_checkpoint"
    changes: dict[str, Any] = {"mcp_servers": {name: entry}, "memory": memory}
    if checkpoint_required is not None:
        changes["compression"] = {"checkpoint_required": bool(checkpoint_required)}
    save_config(changes, merge_existing=True)


def _stored_section(settings: dict[str, Any], section: Any) -> dict[str, Any]:
    """The keys setup writes into ``memory.ctx_checkpoint``. An engine name written next to one of
    them wins over it (``overlay_from_umbrella``), so it gets the form's value too and the save
    takes effect (#289)."""
    stored = {key: settings[key] for key in _SETUP_STORED_KEYS}
    if isinstance(section, dict):
        for key in _SETUP_STORED_KEYS:
            alias = UMBRELLA_KEY_MAP.get(key)
            if alias and section.get(alias) is not None:
                stored[alias] = settings[key]
    return stored


def _candidate_entry(entry: dict[str, Any], ctx_url: str, mode: str, api_key: str) -> dict[str, Any]:
    """The server entry as setup would write it, for the connection test: the new URL, and a key
    typed in this setup in place of the ``${CTX_API_KEY}`` reference (it is not in .env yet).
    Never written anywhere."""
    candidate = dict(entry)
    candidate.pop("enabled", None)  # setup re-enables the entry; test it as enabled
    if mode != "stdio" and ctx_url:
        candidate["url"] = ctx_url
    reference = "${" + SETUP_API_KEY_ENV + "}"
    if mode == "new":
        candidate["headers"] = {"Authorization": f"Bearer {api_key}"} if api_key else dict(_SETUP_AUTH_HEADERS)
    elif mode == "managed" and api_key:
        headers = candidate.get("headers")
        if isinstance(headers, dict):
            candidate["headers"] = {k: str(v).replace(reference, api_key) for k, v in headers.items()}
    return candidate


def _legacy_field(spec: dict[str, Any], current: Any) -> dict[str, Any]:
    """One ``FIELDS`` entry in the shape of ``get_config_schema()`` (web dashboard, terminal)."""
    field: dict[str, Any] = {"key": spec["key"], "label": spec["label"], "description": spec["description"]}
    kind = spec["kind"]
    if kind == "secret":
        field.update({"secret": True, "required": True, "env_var": spec["env_key"], "url": spec["url"]})
    elif kind == "select":
        field.update({"choices": [value for value, _label in spec["options"]], "default": current})
    elif kind == "bool":
        field.update({"choices": list(_MEMORY_CHOICES), "default": "on" if current else "off"})
    elif kind == "number":
        field.update(
            {
                "type": "integer",
                "minimum": spec["minimum"],
                "maximum": spec["maximum"],
                "step": spec["step"],
                "default": current,
            }
        )
    else:
        if spec.get("placeholder"):
            field["placeholder"] = spec["placeholder"]
        if spec.get("required"):
            field["required"] = True
        field["default"] = current
    return field


_FIELD_DEFAULTS = {spec["key"]: spec.get("default") for spec in _FORM_FIELDS}
_MODE_FIELD = next(spec for spec in _FORM_FIELDS if spec["key"] == "checkpoint_required")


# Scope-aware environment or .env value (the host's own lookup), ``""`` if unset.
_env_value = _st.env_value


def _prompt_setup(schema: list[dict[str, Any]]) -> tuple[dict[str, Any], str]:
    """Ask for each schema field on the terminal; returns ``(values, api_key)``."""
    values: dict[str, Any] = {}
    api_key = ""
    for field in schema:
        key = field["key"]
        label = field.get("label") or key
        required = bool(field.get("required"))
        if field.get("secret"):
            existing = _env_value(str(field.get("env_var") or ""))
            if existing:
                masked = f"...{existing[-4:]}" if len(existing) > 4 else "set"
                hint = f" (current: {masked}, blank keeps it)"
            else:
                hint = ""
                if field.get("url"):
                    print(f"  Setup guide: {field['url']}")
            while True:
                api_key = getpass.getpass(f"  {label}{hint}: ").strip()
                if api_key or existing or not required:
                    break
                print("  A key is required.")
            continue
        default = field.get("default")
        default_text = "" if default is None else str(default)
        choices = [str(choice) for choice in field.get("choices") or []]
        prompt = f"  {label}"
        if choices:
            prompt += f" ({'/'.join(choices)})"
        if default_text:
            prompt += f" [{default_text}]"
        prompt += ": "
        while True:
            answer = input(prompt).strip() or default_text
            if not answer and required:
                print("  This value is required.")
                continue
            if choices and answer not in choices:
                print(f"  Choose one of: {', '.join(choices)}")
                continue
            if field.get("type") == "integer":
                try:
                    int(answer)
                except ValueError:
                    print("  Enter a whole number.")
                    continue
            break
        values[key] = answer
    return values, api_key


def _default_dispatch(name: str, args: dict, **kwargs) -> Any:
    from tools.registry import registry

    return registry.dispatch(name, args, **kwargs)


# -- Provider ------------------------------------------------------------------------------


class CtxCheckpointMemoryProvider(CtxMemoryProvider):
    """ctx memory provider: durable pre-compaction checkpoints, optionally the full memory layer."""

    def __init__(
        self,
        config: dict[str, Any] | None = None,
        dispatch: Callable[..., Any] | None = None,
    ) -> None:
        super().__init__()
        self._section = _umbrella_settings(config if config is not None else _load_plugin_config())
        self._dispatch = dispatch or _default_dispatch
        # Hermes' user-visible warning sink (passed to initialize() on CLI-platform sessions)
        # and the last notice sent through it, so a repeated failure is reported once.
        self._warning_callback: Callable[[str], Any] | None = None
        self._last_warning = ""
        # The ctx keys of this profile (``_profile_secrets``), resolved on every connect.
        self._profile_keys: tuple[str, ...] = ()

    # -- identity ------------------------------------------------------------------

    @property
    def name(self) -> str:
        return "ctx_checkpoint"

    @property
    def pre_compress_checkpoint_api_version(self) -> int:
        """Negotiated contract, re-evaluated on every read: ``max(2, min(host, plugin))``.

        The host reads this as ``int(getattr(provider, ...))`` on the instance; a property
        satisfies that path and always returns an ``int``.
        """
        return super().pre_compress_checkpoint_api_version

    @property
    def memory_enabled(self) -> bool:
        return umbrella_memory_enabled(self._section)

    @property
    def transport(self) -> str:
        return umbrella_transport(self._section)

    def _server_name(self) -> str:
        return str(self._section.get("mcp_server") or _DEFAULT_MCP_SERVER)

    # -- engine seams --------------------------------------------------------------

    def _probe_host_api(self) -> int:
        return _probe_host_checkpoint_api()

    def _negotiate(self, host_api: int) -> int:
        return _negotiate_checkpoint_api(host_api)

    def _config_overlay(self) -> dict[str, Any]:
        return overlay_from_umbrella(self._section)

    def _config_base(self) -> dict[str, Any]:
        return {UMBRELLA_KEY_MAP[key]: default for key, (_normalize, default) in _CHECKPOINT_SETTINGS.items()}

    def _effective_settings(self) -> dict[str, Any]:
        """The section with every checkpoint setting at the value the engine uses: set here,
        else ctx.json, else the plugin default (clamped by the engine's loader). The forms show
        it and fall back to it, so a save keeps a value that only ctx.json sets."""
        out = dict(self._section)
        try:
            home = self._hermes_home
            if not home:
                from hermes_constants import get_hermes_home

                home = str(get_hermes_home())
            engine = self._load_config(home)
        except Exception:
            engine = {}
        for key, (normalize, default) in _CHECKPOINT_SETTINGS.items():
            value = engine.get(UMBRELLA_KEY_MAP[key])
            out[key] = normalize(value) if value is not None else default
        return out

    def _now(self) -> float:
        return _monotonic()

    def _chain_writer_seams(self) -> dict[str, Any]:
        return {"sleep": lambda seconds: _sleep(seconds), "monotonic": lambda: _monotonic()}

    def _current_secrets(self) -> tuple[str, ...]:
        return (self._api_key, *self._profile_keys)

    def _profile_secrets(self) -> tuple[str, ...]:
        """The ctx keys of this profile for the configured server entry (``status.profile_ctx_keys``):
        what the provider binds on connect and what status and connection test redact with."""
        return _st.profile_ctx_keys(self._server_name())

    def _connect(self) -> None:
        self._remember_secrets()
        self._profile_keys = self._profile_secrets()
        if self.transport == "rest":
            super()._connect()
            return
        name = self._server_name()
        self._client = McpClient(
            self._dispatch,
            name,
            timeout=float(self._config.get("api_timeout") or 10.0),
            synthesis_timeout=float(self._config.get("synthesis_timeout") or 90.0),
            secrets=self._bound_secrets,
        )
        self._api_key = ""
        self._base_url = ""
        self._home_scope = ""
        self._whoami_label = f"mcp:{name}"
        self._active = True
        logger.info(
            "ctx_checkpoint bound to MCP server %r (profile=%s, memory features %s)",
            name,
            self._profile,
            "on" if self.memory_enabled else "off",
        )

    # -- availability --------------------------------------------------------------

    def is_available(self) -> bool:
        if self.transport == "rest":
            return super().is_available()
        try:
            from hermes_cli.config import load_config

            config = load_config()
            server = _configured_server(config, self._server_name())
            if not server or server.get("enabled", True) is False:
                return False
            return not self._colliding_servers(config)
        except Exception:
            return False

    def _colliding_servers(self, config: dict[str, Any]) -> list[str]:
        """Other ``mcp_servers`` entries whose tools Hermes registers under this entry's tool names;
        none while this entry itself is not configured (then that is the reason to name)."""
        if not _configured_server(config, self._server_name()):
            return []
        return colliding_servers(self._server_name(), config.get("mcp_servers"))

    def unavailable_reason(self) -> str:
        if self.transport == "rest":
            return (
                "transport rest: set CTX_KEY in .env and the server URL — rest_base_url in "
                "memory.ctx_checkpoint, CTX_BASE_URL in .env or ctx_base_url in ctx.json"
            )
        others = self._colliding_servers(_effective_config())
        if others:
            return collision_text(self._server_name(), others)
        return (
            f"configure the ctx MCP server entry mcp_servers.{self._server_name()} — "
            "`hermes memory setup ctx_checkpoint` does it"
        )

    # -- lifecycle -----------------------------------------------------------------

    def initialize(self, session_id: str, **kwargs) -> None:
        callback = kwargs.get("warning_callback")
        self._warning_callback = callback if callable(callback) else None
        self._last_warning = ""
        _warn_if_hygiene_preempts()
        super().initialize(session_id, **kwargs)
        warnings = _st.setting_warnings(
            self._config, self._section, self._hermes_home, _st.checkpoint_required(_effective_config())
        )
        for warning in warnings:
            if (self._hermes_home, warning) not in _mode_warnings_logged:
                _mode_warnings_logged.add((self._hermes_home, warning))
                logger.warning("%s", warning)
            self._notify_user(warning)

    def _checkpoint_notice(self, message: str) -> None:
        """A paced checkpoint takes minutes and the compaction waits for it: say so before."""
        self._notify_user(message)

    def _notify_user(self, message: str) -> None:
        """Pass a notice to Hermes' warning callback, once per distinct message; never raises."""
        if self._warning_callback is None or message == self._last_warning:
            return
        self._last_warning = message
        try:
            self._warning_callback(message)
        except Exception:
            logger.debug("ctx_checkpoint: warning callback failed", exc_info=True)

    # -- checkpoint outcome --------------------------------------------------------

    def on_pre_compress(
        self,
        messages: list[dict[str, Any]],
        *,
        require_checkpoint: bool = False,
        tool_evidence: list[dict[str, Any]] | None = None,
        **kwargs,
    ) -> str:
        """The engine's checkpoint, plus its outcome in ``ctx_state/`` and, on a failure or a
        compaction without a checkpoint, the reason for the user (Hermes passes only a generic
        text up)."""
        try:
            receipt_text = super().on_pre_compress(
                messages, require_checkpoint=require_checkpoint, tool_evidence=tool_evidence, **kwargs
            )
        except Exception as exc:
            self._record_outcome(bool(require_checkpoint), exc)
            raise
        self._record_outcome(bool(require_checkpoint), self._last_chain_error)
        return receipt_text

    def _record_outcome(self, required: bool, error: Exception | None) -> None:
        """Outcome file + user notice. Never raises: it must not turn a checkpoint into a failure,
        nor replace the exception a failed one raises."""
        try:
            receipt = self._last_chain_receipt
            skipped = "" if (error is not None or receipt is not None) else self._skip_reason()
            secrets = self._bound_secrets() if error is not None else ()
            outcome = _st.checkpoint_outcome(
                required=required,
                receipt=receipt,
                error=error,
                skipped=skipped,
                secrets=secrets,
                context=f"{self._platform or '?'}/{self._agent_context or '?'}",
            )
            # The chain's head is kept even when a later step of the same call failed (required
            # facts): it is durable in ctx and the read entry of this root session.
            _st.record_checkpoint(
                self._hermes_home,
                outcome,
                root=self._checkpoint_root(),
                head_id=str(receipt.head_id or "") if receipt is not None else "",
                binding=self._pointer_binding(),
            )
            if outcome["error"]:
                self._notify_user(_st.failure_warning(outcome, self._server_name()))
            elif outcome["skipped"]:
                self._notify_user(_st.skip_warning(outcome))
            else:
                self._last_warning = ""
        except Exception:
            logger.debug("ctx_checkpoint: checkpoint outcome not recorded", exc_info=True)

    def _skip_reason(self) -> str:
        if not (self._active and self._client):
            return "the provider is not connected"
        if self.pre_compress_checkpoint_api_version < 2:
            return "checkpoint_mode: off"
        if not self._config.get("checkpoint_chain", True):
            return "checkpoint_chain: false"
        return "no transcript chain was written"

    # -- status (hermes memory status) ---------------------------------------------

    def get_status_config(self, provider_config: dict) -> dict:
        """``hermes memory status``: on MCP the server entry, a connection test, the operating
        mode and the latest checkpoint (the same report as ``hermes ctx_checkpoint status``).
        Never raises."""
        if self.transport == "rest":
            status = super().get_status_config(provider_config)
            base = status.get("base_url")
            if base and _st.insecure_http(base):
                status["warning"] = _st.http_warning(base)
            return status
        try:
            home = self._hermes_home
            if not home:
                from hermes_constants import get_hermes_home

                home = str(get_hermes_home())
            return _st.mcp_report(home)
        except Exception as exc:
            return {"summary": f"✗ status unavailable: {_st.redact_error(exc, self._profile_secrets())}", "ok": False}

    # -- memory-feature gates ------------------------------------------------------

    def get_tool_schemas(self) -> list[dict[str, Any]]:
        if not self.memory_enabled:
            return []
        return super().get_tool_schemas()

    def system_prompt_block(self) -> str:
        """The memory text (memory features on), then the checkpoint pointer once a head is known
        for the session root — in both modes. Without either it is empty, as before 0.9.0."""
        if not self._active:
            return ""
        blocks = []
        if self.memory_enabled:
            block = super().system_prompt_block()
            if block:
                blocks.append(f"{block}\nSkill: {self.name}:{SKILL_NAME} explains how to use the store well.")
        pointer = self._checkpoint_pointer()
        if pointer:
            blocks.append(pointer)
        return "\n\n".join(blocks)

    # -- checkpoint read path (system prompt block, receipt) -----------------------

    def _pointer_binding(self) -> str:
        """What a recorded head belongs to: transport, ctx server (MCP entry name or REST base
        URL) and chain category, as a digest. A head written through another binding is not
        named — its id would not resolve there, or the search would run in another category."""
        server = self._base_url if self.transport == "rest" else self._server_name()
        raw = f"{self.transport}\0{server}\0{self._checkpoint_category()}"
        return hashlib.sha256(raw.encode("utf-8")).hexdigest()[:16]

    def _read_tools(self) -> _cp.ReadTools:
        """The read tools of this session, by the names the model sees: the plugin's own with
        memory features on (the block is withheld with them when Hermes gates the memory
        toolset off), the ctx MCP server's tools without, the plugin CLI on ``transport: rest``
        without memory features (no agent tool reads ctx there)."""
        if self.memory_enabled:
            return _cp.PLUGIN_READ_TOOLS
        if self.transport == "rest":
            return _cp.CLI_READ_TOOLS
        server = self._server_name()
        return _cp.mcp_read_tools(mcp_tool_name(server, "get"), mcp_tool_name(server, "search"))

    def _receipt_read_path(self, head_id: str, root: str) -> str:
        return _cp.read_path(self._read_tools(), head_id=head_id, category=self._checkpoint_category())

    def _checkpoint_pointer(self) -> str:
        """The pointer block for the session root, ``""`` while no head is known. The head comes
        from the outcome file every checkpoint updates (``memory/status.py``), so every process
        of the profile names it; no network call."""
        root = self._checkpoint_root()
        head = _st.known_head(self._hermes_home, root, binding=self._pointer_binding())
        if not head:
            return ""
        return _cp.pointer_block(
            self._read_tools(), head_id=head, root_session_id=root, category=self._checkpoint_category()
        )

    # -- setup surfaces (Desktop and dashboard memory forms, `hermes memory setup`) ----

    def _form_values(self) -> dict[str, Any]:
        """Current value of every memory-form field except the key (never read back)."""
        try:
            config = _read_user_config()
        except Exception:
            config = {}
        entry = _configured_server(config, self._server_name())
        url = entry.get("url") if _server_setup_mode(entry) != "stdio" else None
        current = self._effective_settings()
        return {
            "ctx_url": url if isinstance(url, str) else "",
            "sensitivity": current["sensitivity"],
            "category": current["category"],
            "chunk_chars": current["chunk_chars"],
            "memory": self.memory_enabled,
            "checkpoint_required": _st.checkpoint_required(_effective_config()),
        }

    def get_config_schema(self) -> list[dict[str, Any]]:
        """Setup fields for the web dashboard's memory form and ``post_setup()``.

        Built from ``config_schema.FIELDS`` (the table Hermes Desktop's form is declared
        from). The fields follow the existing ``mcp_servers`` entry named by ``mcp_server``
        (default ``ctx``; set in config.yaml, not in setup): its URL prefills ``ctx_url``,
        and the key is asked only when setup manages that entry's auth (see
        ``_server_setup_mode``). A ``command`` (stdio) entry gets neither field.
        """
        try:
            config = _read_user_config()
        except Exception:
            config = {}
        mode = _server_setup_mode(_configured_server(config, self._server_name()))
        current = self._form_values()
        fields = []
        for spec in _FORM_FIELDS:
            if spec.get("desktop_only"):
                continue
            if spec["key"] == "ctx_url" and mode == "stdio":
                continue
            if spec["key"] == "ctx_api_key" and mode not in ("new", "managed"):
                continue
            fields.append(_legacy_field(spec, current.get(spec["key"])))
        return fields

    def read_config_fields(self, hermes_home: str) -> dict[str, Any]:
        """Hermes Desktop's memory form (``config_schema.py``, ``storage=provider``): the stored values."""
        self._section = _umbrella_settings(_load_plugin_config())
        values = self._form_values()
        try:
            config = _read_user_config()
        except Exception:
            config = {}
        mode = _server_setup_mode(_configured_server(config, self._server_name()))
        placeholders: dict[str, str] = {}
        if mode == "stdio":
            values.pop("ctx_url")
            placeholders["ctx_api_key"] = "not used by a stdio server entry"
        elif not values["ctx_url"]:
            values.pop("ctx_url")
        if mode == "foreign":
            placeholders["ctx_api_key"] = "not used: the server entry signs in on its own"
        return {"values": values, "placeholders": placeholders}

    def write_config_fields(self, values: dict[str, Any], hermes_home: str) -> None:
        """Hermes Desktop's memory form: persist the submitted fields (one at a time from the
        compact panel). Hermes saves a submitted key to ``.env`` after this returns; a
        ``ValueError`` (invalid value, failed connection test) refuses the save."""
        submitted = dict(values or {})
        api_key = str(submitted.pop("ctx_api_key", None) or "").strip()
        form: dict[str, Any] = {}
        for key, value in submitted.items():
            if value is None:  # a cleared field: back to its default (a cleared URL keeps the entry's)
                if key != "ctx_url":
                    form[key] = _FIELD_DEFAULTS.get(key)
                continue
            form[key] = value
        self._apply_form(form, api_key=api_key, key_visible=True)

    def save_config(self, values: dict[str, Any], hermes_home: str) -> None:
        """The web dashboard's memory form: persist non-secret setup values. No connection test:
        the dashboard writes the key to .env only after this returns, so a test here could only
        see the previous key. The operating mode is not part of this form (``desktop_only``): the
        dashboard sends every field with the value it loaded, so it is ignored here."""
        values = {key: value for key, value in (values or {}).items() if key != "checkpoint_required"}
        self._apply_form(values, api_key="", key_visible=False)

    def _apply_form(self, values: dict[str, Any], *, api_key: str, key_visible: bool) -> None:
        """Validate, test the connection where it changed and the test can see every
        credential, then write. Nothing is written when a check fails, nor when the required
        mode is to be turned on and this Hermes cannot run the test."""
        _require_writable_config()
        config = _read_user_config()
        settings = _normalize_setup_values(values, self._effective_settings())
        wanted = _switch_value(values, "checkpoint_required")
        current_required = _st.checkpoint_required(_effective_config())
        switch = None if wanted is None or wanted == current_required else wanted
        name = settings["mcp_server"]
        entry = _configured_server(config, name)
        mode = _server_setup_mode(entry)
        ctx_url = settings["ctx_url"]
        if not entry and not ctx_url:
            if key_visible and api_key and not values:
                return  # only the key so far: Hermes keeps it in .env, the URL follows
            raise ValueError(f"ctx server URL is required: there is no MCP server entry named {name!r} yet")
        if mode == "stdio" and ctx_url:
            raise ValueError(
                f"mcp_servers.{name} runs a command (stdio) and has no URL; change that entry in config.yaml"
            )
        uses_key = mode in ("new", "managed")
        changed = (
            not entry
            or entry.get("enabled") is False
            or bool(ctx_url and ctx_url != entry.get("url"))
            or bool(api_key and uses_key)
        )
        needs_test = switch is True or (key_visible and changed)
        stored_key = _env_value(SETUP_API_KEY_ENV)
        if needs_test and uses_key and not (api_key or stored_key):
            if switch is True:
                raise ValueError(
                    "Save the ctx API key first: waiting for the checkpoint needs a working connection to ctx."
                )
            needs_test = False  # the key is not known yet; the save that brings it runs the test
        if needs_test:
            test = _st.run_connection_test(
                name,
                _candidate_entry(entry, ctx_url, mode, api_key),
                secrets=(api_key, *_st.profile_ctx_keys(name)),
                servers=config.get("mcp_servers"),
            )
            if test.ok is False:
                raise ValueError(f"{test.detail[:1].upper()}{test.detail[1:]}. Nothing was saved.")
            if test.ok is None and switch is True:
                # As the terminal setup: the mode is turned on only after a passing test (#294).
                raise ValueError(
                    f"{test.detail[:1].upper()}{test.detail[1:]}. The required mode needs a passing connection "
                    f"test, so it stays off; set compression.checkpoint_required in config.yaml once "
                    f"`hermes mcp test {name}` passes. Nothing was saved."
                )
        target = ctx_url or entry.get("url")
        if _st.insecure_http(target):
            logger.warning("ctx_checkpoint: %s", _st.http_warning(target))
        _write_setup_config(settings, activate=False, checkpoint_required=switch)
        self._section.update(_umbrella_settings({**self._section, **_stored_section(settings, self._section)}))

    def post_setup(self, hermes_home: str, config: dict[str, Any]) -> None:
        """``hermes memory setup ctx_checkpoint``: prompt, test the connection, offer the
        operating mode, then write .env and config and activate.

        Everything is asked, validated and tested before anything is written; the key goes
        to .env before config.yaml starts referencing it.
        """
        print("\n  Configuring ctx_checkpoint:\n")
        key_saved = False
        name, api_key = self._server_name(), ""
        try:
            _require_writable_config()
            values, api_key = _prompt_setup(self.get_config_schema())
            settings = _normalize_setup_values(values, self._effective_settings())
            name = settings["mcp_server"]
            config = _read_user_config()
            entry = _configured_server(config, name)
            mode = _server_setup_mode(entry)
            if not entry and not settings["ctx_url"]:
                raise ValueError(f"ctx server URL is required: there is no MCP server entry named {name!r} yet")
            target = settings["ctx_url"] or entry.get("url")
            if _st.insecure_http(target):
                print(f"\n  Warning: {_st.http_warning(target)}")
            test = _st.run_connection_test(
                name,
                _candidate_entry(entry, settings["ctx_url"], mode, api_key),
                secrets=(api_key, *_st.profile_ctx_keys(name)),
                servers=config.get("mcp_servers"),
            )
            if test.ok is False:
                print(f"\n  Connection test failed: {test.detail}")
                print("  Nothing was changed; check the URL and the API key, then run the setup again.\n")
                return
            print(f"\n  Connection test passed: {test.detail}" if test.ok else f"\n  {test.detail}")
            switch = self._prompt_mode(offer=bool(test.ok))
            if api_key:
                from hermes_cli.config import save_env_value

                save_env_value(SETUP_API_KEY_ENV, api_key)
                key_saved = True
            _write_setup_config(settings, activate=True, checkpoint_required=switch)
        except (KeyboardInterrupt, EOFError):
            print("\n  Setup cancelled; nothing was changed.\n")
            return
        except Exception as exc:
            secrets = (api_key, *_st.profile_ctx_keys(name))
            logger.debug("ctx_checkpoint setup failed: %s", _cp.redacted_traceback(exc, secrets))
            print(f"\n  Setup failed: {_st.redact_error(exc, secrets)}")
            if key_saved:
                print(f"  The API key was saved to .env as {SETUP_API_KEY_ENV};")
                print("  config.yaml was not changed.\n")
            else:
                print("  Nothing was changed.\n")
            return
        self._section.update(_umbrella_settings({**self._section, **_stored_section(settings, self._section)}))
        required = _st.checkpoint_required(_effective_config())
        print("\n  Memory provider: ctx_checkpoint")
        print(f"  ctx MCP server entry: mcp_servers.{name}")
        print(f"  Memory features: {'on' if settings['memory'] else 'off (checkpoints only)'}")
        print(f"  Operating mode: {'required' if required else 'best effort'}")
        if api_key:
            print(f"  API key saved to .env as {SETUP_API_KEY_ENV}")
        print(f"  `hermes ctx_checkpoint status` (or `hermes mcp test {name}`) checks the connection;")
        print("  new sessions use the provider.\n")

    def _prompt_mode(self, *, offer: bool) -> bool | None:
        """Explain both operating modes; after a passing connection test ask for one.
        Returns the new value of ``compression.checkpoint_required``, ``None`` to keep it."""
        current = _st.checkpoint_required(_effective_config())
        print("\n  Operating mode (Hermes' compression.checkpoint_required):")
        print(f"    off: {_st.mode_text(False)}")
        print(f"    on:  {_st.mode_text(True)}")
        if not offer:
            print(f"  Kept at {'on' if current else 'off'}; set compression.checkpoint_required in config.yaml")
            print("  once the connection test passes.")
            return None
        answers, _key = _prompt_setup([_legacy_field(_MODE_FIELD, current)])
        wanted = _switch_value(answers, "checkpoint_required")
        return None if wanted is None or wanted == current else wanted


def register(ctx) -> None:
    """Memory plugin entry point: the provider plus the bundled ``ctx-memory`` skill."""
    ctx.register_memory_provider(CtxCheckpointMemoryProvider())
    register_skill = getattr(ctx, "register_skill", None)
    if callable(register_skill) and _SKILL_PATH.is_file():
        try:
            register_skill(
                SKILL_NAME,
                _SKILL_PATH,
                _SKILL_DESCRIPTION,
                {"tags": ["ctx", "memory", "evidence", "knowledge-store"]},
            )
        except Exception as exc:  # a host without skill support must never lose the provider
            logger.debug("ctx_checkpoint: skill registration skipped: %s", exc)


__all__ = ["CtxApiError", "CtxCheckpointError", "CtxCheckpointMemoryProvider", "register"]
