"""ctx-backed pre-compaction source checkpoint provider.

The normal Hermes compaction summary is intentionally lossy.  This provider
creates a deterministic, redacted source checkpoint before that summary can
replace older turns.  It stores direct user/assistant text in bounded ctx
blocks and returns stable block IDs for deterministic inclusion in the handoff.

Setup runs through Hermes' own surfaces: the Dashboard/Desktop memory form reads
``get_config_schema()`` and writes through ``save_config()``; ``hermes memory
setup ctx_checkpoint`` hands over to ``post_setup()``.  Compaction thresholds stay
Hermes' own defaults.
"""

from __future__ import annotations

import contextvars
import getpass
import hashlib
import importlib
import json
import logging
import re
import sys
import threading
import time
from datetime import datetime, timezone
from typing import Any, Callable, Dict, List, Optional

from agent.memory_provider import MemoryProvider
from agent.redact import redact_sensitive_text

logger = logging.getLogger(__name__)

_UUID_RE = re.compile(
    r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-"
    r"[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b"
)
# The block id inside ctx's answer text: "Stored: <title> (id: <uuid>, category: …)",
# "No change needed — identical content already exists (id: <uuid>)". Only this group
# names the block: the title in front of it carries the root session id, and a session
# id that is itself a UUID used to be taken for the block id (0.5.2).
_ANSWER_ID_RE = re.compile(r"\(id: (" + _UUID_RE.pattern + r")")
_PRIOR_CONTEXT_PREFIX = "[PRIOR CONTEXT — for reference only; not a new message]"
_PRIOR_CONTEXT_MERGE_BOUNDARY = (
    "\n\n[END OF PRIOR CONTEXT — COMPACTION SUMMARY BELOW]\n\n"
    "[CONTEXT COMPACTION — REFERENCE ONLY]"
)
_SYNTHETIC_ASSISTANT_PREFIXES = (
    "[CONTEXT COMPACTION — REFERENCE ONLY]",
    "[Your active task list was preserved across context compression]",
    "<memory-context>",
)
_DEFAULT_CHUNK_CHARS = 36_000
_MIN_CHUNK_CHARS = 1_000
_MAX_CHUNK_CHARS = 40_000
# ctx caps block content at 50 KiB of UTF-8 BYTES (go/internal/handler/context_store.go,
# blockSizeLimit: ``len(content) > 50*1024``). ``chunk_chars`` counts characters, so a
# part of Cyrillic (2 bytes/char) or CJK (3 bytes/char) text crossed the cap long before
# the character limit and every checkpoint of such a transcript failed (0.5.2). Parts
# are now cut at whichever limit comes first.
_CTX_CONTENT_MAX_BYTES = 50 * 1024
# Transcript bytes a part must at least have room for after its header; below that the
# session identifiers alone would fill the part.
_MIN_PART_TEXT_BYTES = 4 * 1024
# Root session id as a title token: at most this many characters of [A-Za-z0-9_.-].
_ROOT_TOKEN_MAX = 48
_DEFAULT_MCP_SERVER = "ctx"
_DEFAULT_CATEGORY = "compaction-checkpoints"
_DEFAULT_SENSITIVITY = "internal"
# ctx sensitivity levels, least to most sensitive (go/internal/backends/trust.go).
_SENSITIVITY_CHOICES = ("public", "internal", "personal", "credentials")
_MCP_SERVER_NAME_RE = re.compile(r"[A-Za-z0-9_-]+")
# Setup stores the key in Hermes' .env and references it from the MCP server entry;
# the key itself is never written into config.yaml.
SETUP_API_KEY_ENV = "CTX_API_KEY"
_SETUP_AUTH_HEADERS = {"Authorization": "Bearer ${" + SETUP_API_KEY_ENV + "}"}
_SETUP_STORED_KEYS = ("mcp_server", "category", "sensitivity", "chunk_chars")
# Gateway session hygiene compacts at this fixed share of the context window
# (``gateway/run_turn_hygiene.py``, not configurable) and holds a turn for at most
# ``compression.hygiene_max_turn_hold_seconds`` (default 10 s) before the turn runs on
# the uncompressed transcript. A compressor threshold at or above it lets hygiene
# compact first.
_HOST_HYGIENE_THRESHOLD = 0.85
_hygiene_warning_emitted = False
# The value may start with an auth scheme ("Authorization: Bearer <token>"). Without
# the optional scheme group only the scheme word was masked and the credential stayed
# in the transcript (hermes-ctx-plugin 1.7.0 finding, W4-NERF, ported 0.5.1).
_GENERIC_SECRET_RE = re.compile(
    r"(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|"
    r"password|passwd|authorization)\b(\s*[:=]\s*)((?:(?:bearer|basic|digest)\s+)?[^\s,;]+)"
)
_BEARER_RE = re.compile(r"(?i)\b(Bearer\s+)[A-Za-z0-9._~+/-]{8,}=*")
# Explicit block type on every checkpoint write (ctx >= v5.5.0 accepts ``type`` on the
# MCP store tool). The server classifies the titles as ``checkpoint`` anyway (M120); the
# explicit claim pins ``type_source=manual`` so a later classifier change cannot move
# archive blocks into retrieval.
_CHECKPOINT_TYPE = "checkpoint"
# Wall-clock budget for one checkpoint (head search, head read, every store). The host
# holds the compaction until this provider returns; without a budget a slow ctx or a
# long 429 streak could hold it for minutes. Exceeding the budget fails closed. Every
# call runs against the remaining budget (``_call_within_deadline``): before 0.5.2 the
# deadline was only checked between stores, so one hanging call could hold the
# compaction for Hermes' MCP tool timeout (300 s by default) on top of the budget.
_DEFAULT_BUDGET_S = 60.0
_MIN_BUDGET_S = 5.0
_MAX_BUDGET_S = 600.0
# ctx write rate limit: 100 writes per 60 s per API key (``query.rate_limit_write``,
# go/internal/handler/ratelimit.go). One checkpoint = parts + manifest + head; the cap
# keeps a single chain inside that window with headroom for other writers on the key.
_DEFAULT_MAX_PARTS = 80
_MAX_PARTS_CEILING = 95
# Backoff after a rate-limited store (seconds, each attempt bounded by the budget).
_RATE_LIMIT_BACKOFF_S = (2.0, 4.0, 8.0)
_RATE_LIMIT_MARKERS = ("rate limit", "rate_limit", "429")
# A key with ctx's ``confirm_writes`` capability gets every store STAGED instead of
# executed (go/internal/handler/mcp_confirm.go); Hermes delivers that answer as an
# error payload. Checkpoint writes run unattended, so the plugin refuses such a key
# with a named reason instead of confirming on the operator's behalf (0.5.2).
_STAGED_MARKER = "confirm_writes"
# Injection seams for tests.
_sleep = time.sleep
_monotonic = time.monotonic

# Highest checkpoint contract THIS plugin implements. 2 = the fail-closed contract as
# merged upstream (NousResearch/hermes-agent#94639, 2026-08-25; upstream renumbered:
# 1 is the historical best-effort hook, the fail-closed gate requires >= 2). Raised to
# 3 in W02-5, once the tool-index renderer lands and ``tool_evidence`` is actually
# rendered into the checkpoint — until then the plugin must never declare 3, even on
# a host that offers it.
PLUGIN_CHECKPOINT_API_MAX = 2
# Host-side marker of the v3 contract: the helper in ``agent.conversation_compression``
# that collects tool evidence for the pre-compress memory handoff. Its presence (as a
# callable) is what distinguishes a real v3 host from one that merely bumped the
# constant.
HOST_V3_FEATURE = "_tool_evidence_for_pre_compress_memory"
_HOST_CONTRACT_MODULE = "agent.memory_provider"
_HOST_FEATURE_MODULE = "agent.conversation_compression"
_HOST_CONTRACT_CONSTANT = "PRE_COMPRESS_CHECKPOINT_API_VERSION"
# Assumed host contract when the probe cannot decide. A host that loads this provider
# is at least v2 per the contract documentation (the fail-closed gate is what this
# plugin exists for); when not even the host module is reachable, 2 is the
# conservative, gate-compatible answer: it never claims a tool-evidence channel that
# was not proven, and a v1-host gate (``>= 1``) accepts it as well.
_PROBE_FALLBACK_API = 2


def _load_host_module(name: str):
    """Return an already-imported host module, importing it only as a fallback."""
    module = sys.modules.get(name)
    if module is not None:
        return module
    return importlib.import_module(name)


def _probe_host_checkpoint_api() -> int:
    """Return the checkpoint contract version the loading host actually carries.

    3 only when ``PRE_COMPRESS_CHECKPOINT_API_VERSION`` is >= 3 AND the host exposes
    ``HOST_V3_FEATURE`` as a callable; otherwise 2 when the constant is >= 2;
    otherwise 1. Pure: no state, and no exception leaves this function — any failure
    (module missing, constant missing or not an int, import error) yields
    ``_PROBE_FALLBACK_API``. Logging stays at debug level so a probe can never turn
    into operator noise.
    """
    try:
        contract = _load_host_module(_HOST_CONTRACT_MODULE)
        version = getattr(contract, _HOST_CONTRACT_CONSTANT, None)
        if isinstance(version, bool) or not isinstance(version, int):
            logger.debug(
                "ctx checkpoint host probe: %s.%s is %r, not an int; assuming v%d",
                _HOST_CONTRACT_MODULE,
                _HOST_CONTRACT_CONSTANT,
                version,
                _PROBE_FALLBACK_API,
            )
            return _PROBE_FALLBACK_API
        if version >= 3:
            features = _load_host_module(_HOST_FEATURE_MODULE)
            if callable(getattr(features, HOST_V3_FEATURE, None)):
                return 3
            logger.debug(
                "ctx checkpoint host probe: constant %d but %s.%s missing; reporting v2",
                version,
                _HOST_FEATURE_MODULE,
                HOST_V3_FEATURE,
            )
            return 2
        return 2 if version >= 2 else 1
    except Exception:
        logger.debug(
            "ctx checkpoint host probe failed; assuming v%d",
            _PROBE_FALLBACK_API,
            exc_info=True,
        )
        return _PROBE_FALLBACK_API


def _negotiate_checkpoint_api(host_api: int) -> int:
    """Contract this plugin declares: min(host, plugin), never below 2.

    The plugin implements v2 completely, so 2 is the floor even for a v1 host
    (whose gate checks ``>= 1`` and therefore accepts it). The ceiling is
    ``PLUGIN_CHECKPOINT_API_MAX`` — read at call time so raising it in W02-5 flips the
    negotiation without touching this function.
    """
    return max(2, min(host_api, PLUGIN_CHECKPOINT_API_MAX))


class CtxCheckpointError(RuntimeError):
    """Raised when a durable ctx checkpoint cannot be completed."""


def _load_plugin_config() -> Dict[str, Any]:
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


def _read_user_config() -> Dict[str, Any]:
    """config.yaml exactly as written: no defaults, no ``${VAR}`` expansion.

    Setup writes go through this read so env-var templates (and the secrets behind
    them) are never materialized into config.yaml.
    """
    from hermes_cli.config import read_user_config_raw

    return read_user_config_raw()


def _configured_server(config: Dict[str, Any], name: str) -> Dict[str, Any]:
    servers = config.get("mcp_servers")
    entry = servers.get(name) if isinstance(servers, dict) else None
    return entry if isinstance(entry, dict) else {}


def _server_setup_mode(entry: Dict[str, Any]) -> str:
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


def _normalize_setup_values(values: Dict[str, Any], current: Dict[str, Any]) -> Dict[str, Any]:
    """Validate setup input; blank fields fall back to the current config, then defaults."""

    def text(key: str, default: str) -> str:
        raw = values.get(key)
        if raw is None or str(raw).strip() == "":
            raw = current.get(key)
        value = str(raw).strip() if raw is not None else ""
        return value or default

    mcp_server = text("mcp_server", _DEFAULT_MCP_SERVER)
    if not _MCP_SERVER_NAME_RE.fullmatch(mcp_server):
        raise ValueError(
            f"MCP server name {mcp_server!r} may only contain letters, digits, '_' and '-'"
        )
    sensitivity = text("sensitivity", _DEFAULT_SENSITIVITY)
    if sensitivity not in _SENSITIVITY_CHOICES:
        raise ValueError(
            f"sensitivity must be one of {', '.join(_SENSITIVITY_CHOICES)}, not {sensitivity!r}"
        )
    raw_chunk = text("chunk_chars", str(_DEFAULT_CHUNK_CHARS))
    try:
        chunk_chars = int(raw_chunk)
    except ValueError:
        raise ValueError(f"chunk_chars must be a whole number, not {raw_chunk!r}") from None
    ctx_url = str(values.get("ctx_url") or "").strip()
    if ctx_url and not ctx_url.startswith(("https://", "http://", "${")):
        raise ValueError(f"ctx server URL must start with https:// or http://, not {ctx_url!r}")
    return {
        "mcp_server": mcp_server,
        "category": text("category", _DEFAULT_CATEGORY),
        "sensitivity": sensitivity,
        "chunk_chars": _clamped_chunk_chars(chunk_chars),
        "ctx_url": ctx_url,
    }


def _require_writable_config() -> None:
    from hermes_cli.config import is_managed

    if is_managed():
        raise RuntimeError("this Hermes install is package-managed; config.yaml is read-only")


def _write_setup_config(settings: Dict[str, Any], *, activate: bool) -> None:
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
        raise ValueError(
            f"ctx server URL is required: there is no MCP server entry named {name!r} yet"
        )

    memory: Dict[str, Any] = {
        "ctx_checkpoint": {key: settings[key] for key in _SETUP_STORED_KEYS},
    }
    if activate:
        memory["provider"] = "ctx_checkpoint"
    save_config({"mcp_servers": {name: entry}, "memory": memory}, merge_existing=True)


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


def _is_rate_limited(error: str) -> bool:
    lowered = error.lower()
    return any(marker in lowered for marker in _RATE_LIMIT_MARKERS)


def _is_staged(error: str) -> bool:
    lowered = error.lower()
    return lowered.lstrip().startswith("staged") or _STAGED_MARKER in lowered


def _utf8_prefix(text: str, max_bytes: int) -> str:
    """Longest prefix of ``text`` whose UTF-8 encoding fits into ``max_bytes``."""
    encoded = text.encode("utf-8")
    if len(encoded) <= max_bytes:
        return text
    # Cutting the bytes can split one multi-byte character at the end; "ignore" drops
    # exactly that incomplete tail and nothing else.
    return encoded[:max_bytes].decode("utf-8", "ignore")


def _root_title_token(root: str) -> str:
    """Root session id as a title token: ``[A-Za-z0-9_.-]``, at most 48 characters.

    An id that sanitizing leaves unchanged and that fits is used as it is, so heads of
    ordinary Hermes session ids keep their titles. An id that sanitizing or truncation
    changed gets a hash of the full id as suffix: ``team/alice:1`` and
    ``team:alice/1``, or two long ids sharing their first 48 characters, used to map to
    one head and mixed their chains (0.5.2).
    """
    safe = re.sub(r"[^A-Za-z0-9_.-]", "_", root)
    if safe == root and len(safe) <= _ROOT_TOKEN_MAX:
        return safe
    suffix = hashlib.sha256(root.encode("utf-8", "surrogatepass")).hexdigest()[:8]
    return f"{safe[:_ROOT_TOKEN_MAX - len(suffix) - 1]}-{suffix}"


def _run_bounded(fn: Callable[[], Any], timeout: float) -> tuple[bool, Any, Optional[BaseException]]:
    """Run ``fn`` on a daemon worker thread and wait at most ``timeout`` seconds.

    Returns ``(finished, value, error)``. A call still running at the timeout is
    abandoned, not cancelled: it ends in the background (at the latest at the host's
    own MCP tool timeout) and its result is dropped. The caller's context variables
    travel along (``contextvars.copy_context``), so host state bound to the agent's
    context — approval prompts, the session key — stays visible to the call.
    """
    box: Dict[str, Any] = {}
    context = contextvars.copy_context()

    def run() -> None:
        try:
            box["value"] = context.run(fn)
        except BaseException as exc:  # handed back to the calling thread below
            box["error"] = exc

    worker = threading.Thread(target=run, name="ctx-checkpoint-call", daemon=True)
    worker.start()
    worker.join(timeout)
    if worker.is_alive():
        return False, None, None
    return True, box.get("value"), box.get("error")


def _call_within_deadline(
    fn: Callable[[], Any], deadline: Optional[float], budget_s: float, what: str
) -> Any:
    """``fn()`` bounded by the checkpoint budget; an exhausted budget fails closed."""
    if deadline is None:
        return fn()
    remaining = deadline - _monotonic()
    if remaining <= 0:
        raise CtxCheckpointError(f"ctx checkpoint budget {budget_s:.0f}s exceeded before {what}")
    finished, value, error = _run_bounded(fn, remaining)
    if not finished:
        raise CtxCheckpointError(
            f"ctx checkpoint budget {budget_s:.0f}s exceeded while {what}; the call was abandoned"
        )
    if error is not None:
        raise error
    return value


def _env_value(name: str) -> str:
    """Scope-aware environment or .env value (the host's own lookup), ``""`` if unset."""
    try:
        from hermes_cli.config import get_env_value

        return get_env_value(name) or ""
    except Exception:
        return ""


def _prompt_setup(schema: List[Dict[str, Any]]) -> tuple[Dict[str, Any], str]:
    """Ask for each schema field on the terminal; returns ``(values, api_key)``."""
    values: Dict[str, Any] = {}
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


def _default_dispatch(name: str, args: dict, **kwargs) -> str:
    from tools.registry import registry

    return registry.dispatch(name, args, **kwargs)


def _flatten_content(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for part in content:
            if isinstance(part, str):
                parts.append(part)
            elif isinstance(part, dict):
                text = part.get("text")
                if isinstance(text, str):
                    parts.append(text)
        return "\n".join(parts)
    return str(content or "")


def _redact_checkpoint_secrets(text: str) -> str:
    """Apply conservative key/value redaction beyond provider-specific rules."""
    text = _GENERIC_SECRET_RE.sub(r"\1\2[REDACTED]", text)
    return _BEARER_RE.sub(r"\1[REDACTED]", text)


def _is_synthetic_handoff(message: Dict[str, Any], raw: str) -> bool:
    if message.get("_compressed_summary"):
        return True
    if str(message.get("role") or "") != "assistant":
        return False
    stripped = raw.lstrip()
    if stripped.startswith(_PRIOR_CONTEXT_PREFIX):
        return _PRIOR_CONTEXT_MERGE_BOUNDARY in stripped
    return any(
        stripped.startswith(prefix) for prefix in _SYNTHETIC_ASSISTANT_PREFIXES
    )


def _find_value(value: Any, key: str) -> Optional[str]:
    if isinstance(value, dict):
        candidate = value.get(key)
        if isinstance(candidate, str) and candidate:
            return candidate
        for nested in value.values():
            found = _find_value(nested, key)
            if found:
                return found
    elif isinstance(value, list):
        for nested in value:
            found = _find_value(nested, key)
            if found:
                return found
    elif isinstance(value, str):
        try:
            decoded = json.loads(value)
        except (TypeError, ValueError):
            return None
        return _find_value(decoded, key)
    return None


def _find_block_id_by_title(value: Any, title: str) -> Optional[str]:
    if isinstance(value, dict):
        candidate = value.get("id")
        if value.get("title") == title and isinstance(candidate, str):
            if _UUID_RE.fullmatch(candidate):
                return candidate
        for nested in value.values():
            found = _find_block_id_by_title(nested, title)
            if found:
                return found
    elif isinstance(value, list):
        for nested in value:
            found = _find_block_id_by_title(nested, title)
            if found:
                return found
    elif isinstance(value, str):
        try:
            decoded = json.loads(value)
        except (TypeError, ValueError):
            return None
        return _find_block_id_by_title(decoded, title)
    return None


def _parse_tool_result(raw: Any) -> tuple[Optional[str], Optional[str]]:
    """Return ``(block_id, error)`` from an MCP store result.

    The block id comes from a structured ``id`` field when the answer carries one, else
    from the ``(id: <uuid>)`` group of ctx's answer text — never from the first UUID
    anywhere in the answer (see ``_ANSWER_ID_RE``). An answer without either yields no
    block id, and the caller fails closed.
    """
    text = raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False, default=str)
    parsed: Any = None
    try:
        parsed = json.loads(text)
    except (TypeError, ValueError):
        pass

    match = _ANSWER_ID_RE.search(text)
    if parsed is not None:
        error = _find_value(parsed, "error")
        if error:
            return None, error
        block_id = _find_value(parsed, "id")
        if block_id and _UUID_RE.fullmatch(block_id):
            return block_id, None
    elif "error" in text.lower() and not match:
        return None, text[:500]
    return (match.group(1) if match else None), None


class CtxCheckpointMemoryProvider(MemoryProvider):
    """Archive redacted direct conversation evidence before compaction."""

    @property
    def pre_compress_checkpoint_api_version(self) -> int:
        """Negotiated checkpoint contract, re-evaluated on every read.

        ``min(host capability, PLUGIN_CHECKPOINT_API_MAX)``, never below 2. Today
        that is 2 on every host: the fail-closed contract as merged upstream
        (NousResearch/hermes-agent#94639, 2026-08-25; upstream renumbered — 1 is
        the historical best-effort hook, the fail-closed gate requires >= 2).
        Backward-compatible with the v1-contract images (their gate checks
        ``>= 1``); the input shape is unchanged (normalized direct evidence).

        The host reads this as ``int(getattr(provider, ...))`` on the instance
        (``agent/memory_manager.py``); a property satisfies that path and always
        returns an ``int``.
        """
        return _negotiate_checkpoint_api(_probe_host_checkpoint_api())

    def __init__(
        self,
        config: Optional[Dict[str, Any]] = None,
        dispatch: Optional[Callable[..., str]] = None,
    ) -> None:
        self._config = dict(config) if config is not None else _load_plugin_config()
        self._dispatch = dispatch or _default_dispatch
        self._session_id = ""
        self._root_session_id = ""
        self._platform = ""
        self._agent_context = ""
        self._tool_evidence_warned = False

    @property
    def name(self) -> str:
        return "ctx_checkpoint"

    def is_available(self) -> bool:
        try:
            from hermes_cli.config import load_config

            server = _configured_server(load_config(), self._server_name())
            return bool(server) and server.get("enabled", True) is not False
        except Exception:
            return False

    def initialize(self, session_id: str, **kwargs) -> None:
        self._session_id = str(session_id or "")
        self._root_session_id = self._session_id
        self._platform = str(kwargs.get("platform") or "")
        # Upstream vocabulary: "primary", "subagent", "cron", "flush"
        # (agent/memory_provider.py, initialize() docstring). Since
        # NousResearch/hermes-agent#94983 the five fork sites inherit the
        # configured provider under an armed checkpoint gate and initialize
        # their own instance with agent_context="subagent", so a fork's working
        # transcript reaches this archive as evidence in its own right. We
        # label it rather than drop it; an absent or empty kwarg stays empty
        # instead of claiming "primary" about a host that never said so.
        self._agent_context = str(kwargs.get("agent_context") or "")
        _warn_if_hygiene_preempts()

    def on_session_switch(
        self,
        new_session_id: str,
        *,
        parent_session_id: str = "",
        reset: bool = False,
        **kwargs,
    ) -> None:
        self._session_id = str(new_session_id or "")
        if reset or not self._root_session_id:
            self._root_session_id = self._session_id

    def get_tool_schemas(self) -> List[Dict[str, Any]]:
        return []

    def get_config_schema(self) -> List[Dict[str, Any]]:
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
        fields: List[Dict[str, Any]] = []
        if mode != "stdio":
            url = entry.get("url")
            fields.append({
                "key": "ctx_url",
                "label": "ctx server URL",
                "description": "MCP endpoint of your ctx server",
                "placeholder": "https://ctx.example.com/mcp",
                "required": True,
                "default": url if isinstance(url, str) else "",
            })
        if mode in ("new", "managed"):
            fields.append({
                "key": "ctx_api_key",
                "label": "ctx API key",
                "description": (
                    f"API key for that ctx server, stored in Hermes' .env as {SETUP_API_KEY_ENV}"
                ),
                "secret": True,
                "required": True,
                "env_var": SETUP_API_KEY_ENV,
                "url": "https://github.com/GottZ/ctx#readme",
            })
        return fields + [
            {
                "key": "sensitivity",
                "label": "Sensitivity",
                "description": "ctx sensitivity stored on checkpoint blocks",
                "choices": list(_SENSITIVITY_CHOICES),
                "default": str(self._config.get("sensitivity") or _DEFAULT_SENSITIVITY),
            },
            {
                "key": "category",
                "label": "Category",
                "description": "ctx category for checkpoint blocks",
                "default": str(self._config.get("category") or _DEFAULT_CATEGORY),
            },
            {
                "key": "chunk_chars",
                "label": "Part size (characters)",
                "description": "Size of one stored transcript part",
                "type": "integer",
                "minimum": _MIN_CHUNK_CHARS,
                "maximum": _MAX_CHUNK_CHARS,
                "step": 1_000,
                "default": _clamped_chunk_chars(self._config.get("chunk_chars")),
            },
        ]

    def save_config(self, values: Dict[str, Any], hermes_home: str) -> None:
        """Persist non-secret setup values (the host writes the key to .env itself)."""
        settings = _normalize_setup_values(values, self._config)
        _write_setup_config(settings, activate=False)
        self._config.update({key: settings[key] for key in _SETUP_STORED_KEYS})

    def post_setup(self, hermes_home: str, config: Dict[str, Any]) -> None:
        """``hermes memory setup ctx_checkpoint``: prompt, write .env and config, activate.

        Everything is asked and validated before anything is written; the key goes
        to .env before config.yaml starts referencing it.
        """
        print("\n  Configuring ctx_checkpoint:\n")
        key_saved = False
        try:
            _require_writable_config()
            values, api_key = _prompt_setup(self.get_config_schema())
            settings = _normalize_setup_values(values, self._config)
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
        self._config.update({key: settings[key] for key in _SETUP_STORED_KEYS})
        name = settings["mcp_server"]
        print("\n  Memory provider: ctx_checkpoint")
        print(f"  ctx MCP server entry: mcp_servers.{name}")
        if api_key:
            print(f"  API key saved to .env as {SETUP_API_KEY_ENV}")
        print(f"  Check the connection with `hermes mcp test {name}`;")
        print("  new sessions use the provider.\n")

    def on_pre_compress(
        self,
        messages: List[Dict[str, Any]],
        *,
        tool_evidence: Optional[List[Dict[str, Any]]] = None,
        require_checkpoint: Optional[bool] = None,
        **_kwargs: Any,
    ) -> str:
        # Keyword tolerance (design 02-tool-evidenz §4.2): a v3 host may send
        # ``tool_evidence`` and further keywords without breaking this plugin.
        # ``tool_evidence`` is NOT rendered in this version — it is only labelled
        # honestly in the metadata; the prose checkpoint stays complete either way.
        # ``require_checkpoint`` (sent by v2 hosts) only shapes the failure log below.
        try:
            return self._checkpoint(messages, tool_evidence)
        except Exception as exc:
            # The host logs a failing provider at DEBUG only (agent/memory_manager.py,
            # on_pre_compress), so in best-effort mode a refused or failed checkpoint was
            # invisible at the default log level (0.5.2).
            if require_checkpoint:
                outcome = "compaction stays blocked until a checkpoint succeeds"
            elif require_checkpoint is None:
                outcome = "the host decides whether compaction proceeds"
            else:
                outcome = "compaction proceeds without a durable checkpoint"
            logger.warning("ctx checkpoint failed (%s): %s", outcome, exc)
            raise

    def _checkpoint(
        self,
        messages: List[Dict[str, Any]],
        tool_evidence: Optional[List[Dict[str, Any]]],
    ) -> str:
        host_api = _probe_host_checkpoint_api()
        negotiated_api = _negotiate_checkpoint_api(host_api)
        tool_index_status = self._tool_index_status(tool_evidence)
        budget_s = _clamped_budget_s(self._config.get("budget_s"))
        deadline = _monotonic() + budget_s

        # Redaction failure raises inside the renderer (never store unredacted text).
        rendered = self._render_direct_evidence(messages)
        if not rendered:
            # Nothing to archive: the batch holds no direct user/assistant text (the
            # host already drops tool rows and pure tool-call wrappers). The lossy
            # summary cannot discard what was never there, so a required checkpoint
            # has nothing to make durable — blocking the compaction would halt the
            # session for nothing. The host counts a returning provider as a
            # successful checkpoint regardless of the receipt text.
            logger.info(
                "ctx checkpoint: no direct user/assistant evidence in %d messages; "
                "nothing to archive, compaction proceeds",
                len(messages) if isinstance(messages, list) else -1,
            )
            return ""

        digest = hashlib.sha256(rendered.encode("utf-8")).hexdigest()
        root = self._root_session_id or self._session_id or "unknown"
        # Reserve the largest header any part of this checkpoint can carry; the rest of
        # ctx's byte cap is the transcript budget of one part.
        header_bytes = len(self._part_header(root, digest, 99_999, 99_999).encode("utf-8"))
        byte_budget = _CTX_CONTENT_MAX_BYTES - header_bytes
        if byte_budget < _MIN_PART_TEXT_BYTES:
            raise CtxCheckpointError(
                f"ctx checkpoint part header takes {header_bytes} of ctx's "
                f"{_CTX_CONTENT_MAX_BYTES}-byte content cap; nothing written"
            )
        chunks = self._split_chunks(rendered, byte_budget)
        max_parts = _clamped_max_parts(self._config.get("max_parts"))
        if len(chunks) > max_parts:
            raise CtxCheckpointError(
                f"ctx checkpoint needs {len(chunks)} source parts > max_parts {max_parts} "
                "(ctx write rate limit 100/60 s per key); nothing written"
            )
        now = datetime.now(timezone.utc).isoformat()
        short_root = _root_title_token(root)
        head_title = f"Compaction checkpoint head {short_root}"
        previous_manifest_id = self._load_previous_manifest_id(
            head_title, deadline=deadline, budget_s=budget_s
        )
        checkpoint_id = hashlib.sha256(
            f"{root}\0{now}\0{digest}".encode("utf-8")
        ).hexdigest()[:16]
        title_base = (
            f"Compaction source {short_root} {digest[:16]} {checkpoint_id}"
        )
        common_metadata = {
            "evidence_date": now,
            "source_kind": "redacted-direct-transcript",
            "root_session_id": root,
            "active_session_id": self._session_id,
            "platform": self._platform,
            "agent_context": self._agent_context,
            "host_checkpoint_api": host_api,
            "checkpoint_api_version": negotiated_api,
            "tool_index_status": tool_index_status,
            "sha256": digest,
            "warnings": ["W1", "W3", "W5", "W9", "W18", "W19", "W21"],
            "invalidated_by": "A verified correction or transcript-recovery finding",
        }

        chunk_ids: list[str] = []
        for index, chunk in enumerate(chunks, start=1):
            body = self._part_header(root, digest, index, len(chunks)) + chunk
            metadata = dict(common_metadata)
            metadata.update({"part": index, "parts": len(chunks)})
            block_id = self._store(
                title=f"{title_base} part {index:03d} of {len(chunks):03d}",
                content=body,
                metadata=metadata,
                tags=["hermes", "compaction", "source-evidence", "transcript", "control"],
                deadline=deadline,
                budget_s=budget_s,
            )
            if not block_id:
                raise CtxCheckpointError(
                    f"ctx checkpoint failed while storing source part "
                    f"{index}/{len(chunks)}"
                )
            chunk_ids.append(block_id)

        manifest = (
            "# Compaction checkpoint manifest\n\n"
            "Stable source index created before lossy context compaction. The linked "
            "blocks preserve the exact redacted user/assistant text available to the "
            "provider; local `state.db` remains the authoritative full transcript, "
            "including tool evidence.\n\n"
            f"- Root session: `{root}`\n"
            f"- Active session: `{self._session_id}`\n"
            f"- Transcript SHA-256: `{digest}`\n"
            f"- Parent manifest: "
            f"{f'`{previous_manifest_id}`' if previous_manifest_id else 'none'}\n"
            f"- Source parts: {len(chunk_ids)}\n\n"
            "## Source block IDs\n\n"
            + "\n".join(f"- `{block_id}`" for block_id in chunk_ids)
        )
        manifest_id = self._store(
            title=f"{title_base} manifest",
            content=manifest,
            metadata={
                **common_metadata,
                "source_block_ids": chunk_ids,
                "parent_manifest_id": previous_manifest_id,
            },
            tags=["hermes", "compaction", "checkpoint-manifest", "ctx", "control"],
            deadline=deadline,
            budget_s=budget_s,
        )
        if not manifest_id:
            raise CtxCheckpointError("ctx checkpoint failed while storing manifest")

        head = (
            "# Compaction checkpoint head\n\n"
            "Stable read entrypoint for the latest confirmed pre-compaction "
            "checkpoint of this root session. Load the manifest next; load source "
            "blocks only when exact wording or provenance is needed.\n\n"
            f"- Checkpoint API version: {negotiated_api}\n"
            f"- Root session: `{root}`\n"
            f"- Active session: `{self._session_id}`\n"
            f"- Latest manifest: `{manifest_id}`\n"
            f"- Transcript SHA-256: `{digest}`\n"
            f"- Source parts: {len(chunk_ids)}\n"
        )
        head_id = self._store(
            title=head_title,
            content=head,
            metadata={
                **common_metadata,
                "latest_manifest_id": manifest_id,
                "source_block_count": len(chunk_ids),
            },
            tags=["hermes", "compaction", "checkpoint-head", "ctx", "control"],
            deadline=deadline,
            budget_s=budget_s,
        )
        if not head_id:
            raise CtxCheckpointError("ctx checkpoint failed while publishing stable head")

        logger.info(
            "ctx pre-compression checkpoint stored: head=%s manifest=%s parts=%d digest=%s",
            head_id,
            manifest_id,
            len(chunk_ids),
            digest[:16],
        )
        return (
            f"ctx checkpoint head: `{head_id}`\n"
            f"ctx checkpoint manifest: `{manifest_id}`\n"
            f"source transcript SHA-256: `{digest}`\n"
            "Load the stable head first, then its latest manifest; load source "
            "blocks only when exact wording or provenance is needed."
        )

    def _part_header(self, root: str, digest: str, index: int, total: int) -> str:
        return (
            "# Compaction source evidence\n\n"
            "This is deterministic, redacted source evidence captured before "
            "Hermes replaced older turns with a lossy compaction summary. "
            "Role labels and message ordinals are preserved. Tool results, "
            "system prompts, synthetic compaction handoffs, and credentials "
            "are intentionally excluded.\n\n"
            f"- Root session: `{root}`\n"
            f"- Active session: `{self._session_id}`\n"
            f"- Transcript SHA-256: `{digest}`\n"
            f"- Part: {index}/{total}\n\n"
            "## Direct transcript\n\n"
        )

    def _load_previous_manifest_id(
        self,
        head_title: str,
        *,
        deadline: Optional[float] = None,
        budget_s: float = _DEFAULT_BUDGET_S,
    ) -> Optional[str]:
        prefix = f"mcp__{self._server_name()}__"
        # Narrowed to heads by their tag (every head carries ``checkpoint-head`` since the
        # first release): an unfiltered full-text search ranked source parts that talk
        # about checkpoints or headings ahead of the head and could push it out of the
        # result window, which silently started a new chain without a parent (0.5.2).
        search_args = {
            "query": head_title,
            "category": str(self._config.get("category") or _DEFAULT_CATEGORY),
            "tags": ["checkpoint-head"],
            "limit": 5,
        }
        search_result = _call_within_deadline(
            lambda: self._dispatch(prefix + "search", search_args),
            deadline,
            budget_s,
            "searching the checkpoint head",
        )
        search_text = (
            search_result
            if isinstance(search_result, str)
            else json.dumps(search_result, ensure_ascii=False, default=str)
        )
        try:
            search_payload = json.loads(search_text)
        except (TypeError, ValueError) as exc:
            raise CtxCheckpointError("ctx checkpoint head search returned invalid JSON") from exc
        search_error = _find_value(search_payload, "error")
        if search_error:
            raise CtxCheckpointError("ctx checkpoint head search failed")
        head_id = _find_block_id_by_title(search_payload, head_title)
        if not head_id:
            return None

        head_result = _call_within_deadline(
            lambda: self._dispatch(prefix + "get", {"id": head_id}),
            deadline,
            budget_s,
            "reading the checkpoint head",
        )
        head_text = (
            head_result
            if isinstance(head_result, str)
            else json.dumps(head_result, ensure_ascii=False, default=str)
        )
        try:
            head_payload = json.loads(head_text)
        except (TypeError, ValueError) as exc:
            raise CtxCheckpointError("ctx checkpoint head read returned invalid JSON") from exc
        head_error = _find_value(head_payload, "error")
        if head_error:
            raise CtxCheckpointError("ctx checkpoint head read failed")
        manifest_id = _find_value(head_payload, "latest_manifest_id")
        if not manifest_id or not _UUID_RE.fullmatch(manifest_id):
            raise CtxCheckpointError(
                "ctx checkpoint head is missing a valid latest_manifest_id"
            )
        return manifest_id

    def _tool_index_status(self, tool_evidence: Any) -> str:
        """Label whether tool evidence reached this call — never whether it was used.

        ``"absent"`` when the host sent nothing; ``"received-unrendered"`` when a
        value arrived (a v3 host sending the kwarg despite our declared 2, or any
        host sending it unconditionally). Warned once per instance, never raised:
        the prose checkpoint is complete without it.
        """
        if tool_evidence is None:
            return "absent"
        if not self._tool_evidence_warned:
            self._tool_evidence_warned = True
            logger.warning(
                "ctx checkpoint received tool_evidence (%d entries) but this plugin "
                "(contract max v%d) does not render it yet; stored as "
                "tool_index_status=received-unrendered",
                len(tool_evidence) if isinstance(tool_evidence, (list, tuple)) else -1,
                PLUGIN_CHECKPOINT_API_MAX,
            )
        return "received-unrendered"

    def _server_name(self) -> str:
        return str(self._config.get("mcp_server") or _DEFAULT_MCP_SERVER)

    def _render_direct_evidence(self, messages: List[Dict[str, Any]]) -> str:
        entries: list[str] = []
        for ordinal, message in enumerate(messages, start=1):
            if not isinstance(message, dict):
                continue
            role = str(message.get("role") or "")
            if role not in {"user", "assistant"}:
                continue
            raw = _flatten_content(message.get("content"))
            # The explicit marker is authoritative. Content heuristics are only
            # a legacy fallback for old assistant envelopes whose DB row lacked
            # persistent provenance; user quotations must remain evidence.
            if _is_synthetic_handoff(message, raw):
                continue
            stripped = raw.lstrip()
            if not stripped:
                continue
            try:
                safe = _redact_checkpoint_secrets(
                    redact_sensitive_text(raw, force=True)
                ).strip()
            except Exception as exc:
                # Fail closed: a checkpoint with unredacted text must never be stored,
                # and "nothing rendered" must not be mistaken for "nothing to archive".
                logger.exception("ctx checkpoint redaction failed for message %d", ordinal)
                raise CtxCheckpointError("ctx checkpoint redaction failed") from exc
            if safe:
                entries.append(f"### Message {ordinal} — {role}\n\n{safe}\n")
        return "\n".join(entries)

    def _split_chunks(self, rendered: str, byte_budget: int = _CTX_CONTENT_MAX_BYTES) -> list[str]:
        """Parts of at most ``chunk_chars`` characters AND ``byte_budget`` UTF-8 bytes.

        A part ends at the last message boundary inside that window, or hard at the
        window's end when the boundary would leave the part shorter than a third of it.
        """
        limit = _clamped_chunk_chars(self._config.get("chunk_chars"))
        chunks: list[str] = []
        remaining = rendered
        while remaining:
            window = len(_utf8_prefix(remaining[:limit], byte_budget))
            if window >= len(remaining):
                chunks.append(remaining)
                break
            cut = remaining.rfind("\n### Message ", 0, window)
            if cut < window // 3:
                cut = window
            chunks.append(remaining[:cut].rstrip())
            remaining = remaining[cut:].lstrip()
        return chunks

    def _store(
        self,
        *,
        title: str,
        content: str,
        metadata: Dict[str, Any],
        tags: List[str],
        deadline: Optional[float] = None,
        budget_s: float = _DEFAULT_BUDGET_S,
    ) -> Optional[str]:
        """One checkpoint block write inside the budget.

        A rate-limited answer is retried after ``_RATE_LIMIT_BACKOFF_S`` while the
        deadline allows it; an exhausted budget raises ``CtxCheckpointError`` (fail
        closed, nothing further is written), and so does a staged answer from a
        ``confirm_writes`` key. Any other store error returns ``None`` and the caller
        raises with the block's role in the message.
        """
        prefix = f"mcp__{self._server_name()}__"
        payload = {
            "category": str(self._config.get("category") or _DEFAULT_CATEGORY),
            "title": title,
            "content": content,
            "tags": tags,
            "metadata": metadata,
            "sensitivity": str(self._config.get("sensitivity") or _DEFAULT_SENSITIVITY),
            "type": _CHECKPOINT_TYPE,
        }
        attempt = 0
        while True:
            result = _call_within_deadline(
                lambda: self._dispatch(prefix + "store", payload),
                deadline,
                budget_s,
                f"storing {title!r}",
            )
            block_id, error = _parse_tool_result(result)
            if not error:
                break
            if _is_staged(error):
                # Confirming here would turn the operator's confirm_writes decision into
                # a formality; the staged write expires on its own (writes.confirm_ttl).
                raise CtxCheckpointError(
                    "ctx staged the checkpoint write instead of storing it: the API key "
                    "behind mcp_servers." + self._server_name() + " has confirm_writes. "
                    "Checkpoint writes run unattended and are never confirmed on the "
                    "operator's behalf; use a ctx key without confirm_writes"
                )
            if _is_rate_limited(error) and attempt < len(_RATE_LIMIT_BACKOFF_S):
                delay = _RATE_LIMIT_BACKOFF_S[attempt]
                attempt += 1
                if deadline is None or _monotonic() + delay < deadline:
                    logger.info(
                        "ctx store rate-limited; backing off %.0fs (attempt %d) before %r",
                        delay,
                        attempt,
                        title,
                    )
                    _sleep(delay)
                    continue
                raise CtxCheckpointError(
                    f"ctx checkpoint rate-limited and budget {budget_s:.0f}s exhausted "
                    f"while storing {title!r}"
                )
            logger.warning("ctx store failed: %s", error)
            return None
        if not block_id:
            logger.warning("ctx store returned no block id")
        return block_id


def register(ctx) -> None:
    ctx.register_memory_provider(CtxCheckpointMemoryProvider())
