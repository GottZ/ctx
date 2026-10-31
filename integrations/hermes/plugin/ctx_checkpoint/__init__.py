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
profile's ``.env``), which adds semantic ``/api/query`` recall without the LLM step.

Setup runs through Hermes' own surfaces: the Dashboard/Desktop memory form reads
``get_config_schema()`` and writes through ``save_config()``; ``hermes memory setup
ctx_checkpoint`` hands over to ``post_setup()``. Compaction thresholds stay Hermes' own.
"""

from __future__ import annotations

import getpass
import logging
import re
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

from .memory import checkpoint as _cp
from .memory.client import CtxApiError
from .memory.config import (
    overlay_from_umbrella,
    umbrella_memory_enabled,
    umbrella_transport,
)
from .memory.mcp_client import McpClient
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
SETUP_API_KEY_ENV = "CTX_API_KEY"
_SETUP_AUTH_HEADERS = {"Authorization": "Bearer ${" + SETUP_API_KEY_ENV + "}"}
_SETUP_STORED_KEYS = ("mcp_server", "category", "sensitivity", "chunk_chars", "memory")
# Gateway session hygiene compacts at this fixed share of the context window
# (``gateway/run_turn_hygiene.py``, not configurable) and holds a turn for at most
# ``compression.hygiene_max_turn_hold_seconds`` (default 10 s) before the turn runs on
# the uncompressed transcript. A compressor threshold at or above it lets hygiene
# compact first.
_HOST_HYGIENE_THRESHOLD = 0.85
_hygiene_warning_emitted = False

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


def _umbrella_settings(section: dict[str, Any] | None) -> dict[str, Any]:
    """The ``memory.ctx_checkpoint`` section with the plugin's own keys defaulted and clamped.

    Engine keys written there under their own names pass through untouched (they are
    validated by the engine's loader).
    """
    raw = dict(section or {})
    sensitivity = str(raw.get("sensitivity") or _DEFAULT_SENSITIVITY).strip().lower()
    out = dict(raw)
    out.update(
        {
            "mcp_server": str(raw.get("mcp_server") or _DEFAULT_MCP_SERVER),
            "category": str(raw.get("category") or _DEFAULT_CATEGORY).strip() or _DEFAULT_CATEGORY,
            "sensitivity": sensitivity if sensitivity in _SENSITIVITY_CHOICES else _DEFAULT_SENSITIVITY,
            "chunk_chars": _clamped_chunk_chars(raw.get("chunk_chars")),
            "budget_s": _clamped_budget_s(raw.get("budget_s")),
            "max_parts": _clamped_max_parts(raw.get("max_parts")),
            "memory": _as_bool(raw.get("memory"), False),
            "transport": umbrella_transport(raw),
        }
    )
    return out


def _load_plugin_config() -> dict[str, Any]:
    try:
        from hermes_cli.config import load_config

        config = load_config()
        memory = config.get("memory", {})
        if not isinstance(memory, dict):
            return {}
        provider = memory.get("ctx_checkpoint", {})
        return dict(provider) if isinstance(provider, dict) else {}
    except Exception:
        return {}


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


def _require_writable_config() -> None:
    from hermes_cli.config import is_managed

    if is_managed():
        raise RuntimeError("this Hermes install is package-managed; config.yaml is read-only")


def _write_setup_config(settings: dict[str, Any], *, activate: bool) -> None:
    """Write the provider section and the ctx MCP server entry into config.yaml.

    Only these two subtrees are written, merged over the file as it is on disk at
    write time (``merge_existing``), so concurrent edits to other sections survive.
    An existing server entry is kept: a disabled one is re-enabled, a ``command``
    (stdio) entry is otherwise left alone, the ``url`` of an HTTP entry follows the
    setup input, and the Bearer header referencing ``CTX_API_KEY`` is added only when
    the entry carries neither ``headers`` nor ``auth``. Compaction settings are never
    touched.
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

    memory: dict[str, Any] = {
        "ctx_checkpoint": {key: settings[key] for key in _SETUP_STORED_KEYS},
    }
    if activate:
        memory["provider"] = "ctx_checkpoint"
    save_config({"mcp_servers": {name: entry}, "memory": memory}, merge_existing=True)


def _env_value(name: str) -> str:
    """Scope-aware environment or .env value (the host's own lookup), ``""`` if unset."""
    try:
        from hermes_cli.config import get_env_value

        return get_env_value(name) or ""
    except Exception:
        return ""


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

    def _now(self) -> float:
        return _monotonic()

    def _chain_writer_seams(self) -> dict[str, Any]:
        return {"sleep": lambda seconds: _sleep(seconds), "monotonic": lambda: _monotonic()}

    def _connect(self) -> None:
        if self.transport == "rest":
            super()._connect()
            return
        name = self._server_name()
        self._client = McpClient(
            self._dispatch,
            name,
            timeout=float(self._config.get("api_timeout") or 10.0),
            synthesis_timeout=float(self._config.get("synthesis_timeout") or 90.0),
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

            server = _configured_server(load_config(), self._server_name())
            return bool(server) and server.get("enabled", True) is not False
        except Exception:
            return False

    def unavailable_reason(self) -> str:
        if self.transport == "rest":
            return (
                "transport rest: set CTX_KEY in .env and the server URL — rest_base_url in "
                "memory.ctx_checkpoint, CTX_BASE_URL in .env or ctx_base_url in ctx.json"
            )
        return (
            f"configure the ctx MCP server entry mcp_servers.{self._server_name()} — "
            "`hermes memory setup ctx_checkpoint` does it"
        )

    # -- lifecycle -----------------------------------------------------------------

    def initialize(self, session_id: str, **kwargs) -> None:
        _warn_if_hygiene_preempts()
        super().initialize(session_id, **kwargs)

    # -- memory-feature gates ------------------------------------------------------

    def get_tool_schemas(self) -> list[dict[str, Any]]:
        if not self.memory_enabled:
            return []
        return super().get_tool_schemas()

    def system_prompt_block(self) -> str:
        if not self.memory_enabled or not self._active:
            return ""
        block = super().system_prompt_block()
        return f"{block}\nSkill: {self.name}:{SKILL_NAME} explains how to use the store well." if block else ""

    # -- setup surfaces (Dashboard/Desktop form, `hermes memory setup`) ------------

    def get_config_schema(self) -> list[dict[str, Any]]:
        """Setup fields for the Dashboard/Desktop memory form and ``post_setup()``.

        The fields follow the existing ``mcp_servers`` entry named by ``mcp_server``
        (default ``ctx``; set in config.yaml, not in setup): its URL prefills
        ``ctx_url``, and the key is asked only when setup manages that entry's auth
        (see ``_server_setup_mode``). A ``command`` (stdio) entry gets neither field.
        """
        try:
            config = _read_user_config()
        except Exception:
            config = {}
        entry = _configured_server(config, self._server_name())
        mode = _server_setup_mode(entry)
        fields: list[dict[str, Any]] = []
        if mode != "stdio":
            url = entry.get("url")
            fields.append(
                {
                    "key": "ctx_url",
                    "label": "ctx server URL",
                    "description": "MCP endpoint of your ctx server",
                    "placeholder": "https://ctx.example.com/mcp",
                    "required": True,
                    "default": url if isinstance(url, str) else "",
                }
            )
        if mode in ("new", "managed"):
            fields.append(
                {
                    "key": "ctx_api_key",
                    "label": "ctx API key",
                    "description": f"API key for that ctx server, stored in Hermes' .env as {SETUP_API_KEY_ENV}",
                    "secret": True,
                    "required": True,
                    "env_var": SETUP_API_KEY_ENV,
                    "url": "https://github.com/GottZ/ctx#readme",
                }
            )
        return [
            *fields,
            {
                "key": "sensitivity",
                "label": "Sensitivity",
                "description": "ctx sensitivity stored on checkpoint blocks",
                "choices": list(_SENSITIVITY_CHOICES),
                "default": str(self._section.get("sensitivity") or _DEFAULT_SENSITIVITY),
            },
            {
                "key": "category",
                "label": "Category",
                "description": "ctx category for checkpoint blocks",
                "default": str(self._section.get("category") or _DEFAULT_CATEGORY),
            },
            {
                "key": "chunk_chars",
                "label": "Part size (characters)",
                "description": "Size of one stored transcript part",
                "type": "integer",
                "minimum": _MIN_CHUNK_CHARS,
                "maximum": _MAX_CHUNK_CHARS,
                "step": 1_000,
                "default": _clamped_chunk_chars(self._section.get("chunk_chars")),
            },
            {
                "key": "memory",
                "label": "Memory features",
                "description": (
                    "on: recall before each turn, ctx_* agent tools, MEMORY.md/USER.md mirror, fact "
                    "extraction and session digests over the same ctx server; off: checkpoints only"
                ),
                "choices": list(_MEMORY_CHOICES),
                "default": "on" if self.memory_enabled else "off",
            },
        ]

    def save_config(self, values: dict[str, Any], hermes_home: str) -> None:
        """Persist non-secret setup values (the host writes the key to .env itself)."""
        settings = _normalize_setup_values(values, self._section)
        _write_setup_config(settings, activate=False)
        self._section.update(_umbrella_settings({**self._section, **{k: settings[k] for k in _SETUP_STORED_KEYS}}))

    def post_setup(self, hermes_home: str, config: dict[str, Any]) -> None:
        """``hermes memory setup ctx_checkpoint``: prompt, write .env and config, activate.

        Everything is asked and validated before anything is written; the key goes
        to .env before config.yaml starts referencing it.
        """
        print("\n  Configuring ctx_checkpoint:\n")
        key_saved = False
        try:
            _require_writable_config()
            values, api_key = _prompt_setup(self.get_config_schema())
            settings = _normalize_setup_values(values, self._section)
            if api_key:
                from hermes_cli.config import save_env_value

                save_env_value(SETUP_API_KEY_ENV, api_key)
                key_saved = True
            _write_setup_config(settings, activate=True)
        except (KeyboardInterrupt, EOFError):
            print("\n  Setup cancelled; nothing was changed.\n")
            return
        except Exception as exc:
            logger.debug("ctx_checkpoint setup failed", exc_info=True)
            print(f"\n  Setup failed: {exc}")
            if key_saved:
                print(f"  The API key was saved to .env as {SETUP_API_KEY_ENV};")
                print("  config.yaml was not changed.\n")
            else:
                print("  Nothing was changed.\n")
            return
        self._section.update(_umbrella_settings({**self._section, **{k: settings[k] for k in _SETUP_STORED_KEYS}}))
        name = settings["mcp_server"]
        print("\n  Memory provider: ctx_checkpoint")
        print(f"  ctx MCP server entry: mcp_servers.{name}")
        print(f"  Memory features: {'on' if settings['memory'] else 'off (checkpoints only)'}")
        if api_key:
            print(f"  API key saved to .env as {SETUP_API_KEY_ENV}")
        print(f"  Check the connection with `hermes mcp test {name}`;")
        print("  new sessions use the provider.\n")


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
