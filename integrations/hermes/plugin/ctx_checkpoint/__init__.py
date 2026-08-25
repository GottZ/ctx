"""ctx-backed pre-compaction source checkpoint provider.

The normal Hermes compaction summary is intentionally lossy.  This provider
creates a deterministic, redacted source checkpoint before that summary can
replace older turns.  It stores direct user/assistant text in bounded ctx
blocks and returns stable block IDs for deterministic inclusion in the handoff.
"""

from __future__ import annotations

import hashlib
import json
import logging
import re
from datetime import datetime, timezone
from typing import Any, Callable, Dict, List, Optional

from agent.memory_provider import MemoryProvider
from agent.redact import redact_sensitive_text

logger = logging.getLogger(__name__)

_UUID_RE = re.compile(
    r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-"
    r"[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b"
)
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
_MAX_CHUNK_CHARS = 40_000
_GENERIC_SECRET_RE = re.compile(
    r"(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|"
    r"password|passwd|authorization)\b(\s*[:=]\s*)([^\s,;]+)"
)
_BEARER_RE = re.compile(r"(?i)\b(Bearer\s+)[A-Za-z0-9._~+/-]{8,}=*")


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


def _parse_tool_result(raw: Any) -> tuple[Optional[str], Optional[str], Optional[str]]:
    """Return ``(block_id, payload_hash, error)`` from an MCP tool result."""
    text = raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False, default=str)
    parsed: Any = None
    try:
        parsed = json.loads(text)
    except (TypeError, ValueError):
        pass

    if parsed is not None:
        error = _find_value(parsed, "error")
        if error:
            return None, None, error
        block_id = _find_value(parsed, "id")
        payload_hash = _find_value(parsed, "payload_hash")
        if block_id and _UUID_RE.fullmatch(block_id):
            return block_id, payload_hash, None
        match = _UUID_RE.search(text)
        return (match.group(0) if match else None), payload_hash, None

    match = _UUID_RE.search(text)
    lowered = text.lower()
    if "error" in lowered and not match:
        return None, None, text[:500]
    return (match.group(0) if match else None), None, None


class CtxCheckpointMemoryProvider(MemoryProvider):
    """Archive redacted direct conversation evidence before compaction."""

    # 2 = fail-closed checkpoint contract as merged upstream (NousResearch/hermes-agent#94639,
    # 2026-08-25). Upstream renumbered: 1 now means the historical best-effort hook, the
    # fail-closed gate requires >= 2. Backward-compatible with the v1-contract images
    # (their gate checks >= 1). Input shape is unchanged (normalized direct evidence).
    pre_compress_checkpoint_api_version = 2

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

    @property
    def name(self) -> str:
        return "ctx_checkpoint"

    def is_available(self) -> bool:
        try:
            from hermes_cli.config import load_config

            servers = load_config().get("mcp_servers", {})
            server = servers.get(self._server_name(), {}) if isinstance(servers, dict) else {}
            return isinstance(server, dict) and server.get("enabled", True) is not False
        except Exception:
            return False

    def initialize(self, session_id: str, **kwargs) -> None:
        self._session_id = str(session_id or "")
        self._root_session_id = self._session_id
        self._platform = str(kwargs.get("platform") or "")

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

    def on_pre_compress(self, messages: List[Dict[str, Any]]) -> str:
        rendered = self._render_direct_evidence(messages)
        if not rendered:
            raise CtxCheckpointError(
                "ctx checkpoint has no direct user/assistant source evidence"
            )

        digest = hashlib.sha256(rendered.encode("utf-8")).hexdigest()
        chunks = self._split_chunks(rendered)
        now = datetime.now(timezone.utc).isoformat()
        root = self._root_session_id or self._session_id or "unknown"
        short_root = re.sub(r"[^A-Za-z0-9_.-]", "_", root)[:48]
        head_title = f"Compaction checkpoint head {short_root}"
        previous_manifest_id = self._load_previous_manifest_id(head_title)
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
            "sha256": digest,
            "warnings": ["W1", "W3", "W5", "W9", "W18", "W19", "W21"],
            "invalidated_by": "A verified correction or transcript-recovery finding",
        }

        chunk_ids: list[str] = []
        for index, chunk in enumerate(chunks, start=1):
            body = (
                "# Compaction source evidence\n\n"
                "This is deterministic, redacted source evidence captured before "
                "Hermes replaced older turns with a lossy compaction summary. "
                "Role labels and message ordinals are preserved. Tool results, "
                "system prompts, synthetic compaction handoffs, and credentials "
                "are intentionally excluded.\n\n"
                f"- Root session: `{root}`\n"
                f"- Active session: `{self._session_id}`\n"
                f"- Transcript SHA-256: `{digest}`\n"
                f"- Part: {index}/{len(chunks)}\n\n"
                "## Direct transcript\n\n"
                f"{chunk}"
            )
            metadata = dict(common_metadata)
            metadata.update({"part": index, "parts": len(chunks)})
            block_id = self._store(
                title=f"{title_base} part {index:03d} of {len(chunks):03d}",
                content=body,
                metadata=metadata,
                tags=["hermes", "compaction", "source-evidence", "transcript", "control"],
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
        )
        if not manifest_id:
            raise CtxCheckpointError("ctx checkpoint failed while storing manifest")

        head = (
            "# Compaction checkpoint head\n\n"
            "Stable read entrypoint for the latest confirmed pre-compaction "
            "checkpoint of this root session. Load the manifest next; load source "
            "blocks only when exact wording or provenance is needed.\n\n"
            f"- Checkpoint API version: 1\n"
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
                "checkpoint_api_version": 1,
                "latest_manifest_id": manifest_id,
                "source_block_count": len(chunk_ids),
            },
            tags=["hermes", "compaction", "checkpoint-head", "ctx", "control"],
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

    def _load_previous_manifest_id(self, head_title: str) -> Optional[str]:
        prefix = f"mcp__{self._server_name()}__"
        search_result = self._dispatch(
            prefix + "search",
            {
                "query": head_title,
                "category": str(
                    self._config.get("category") or "compaction-checkpoints"
                ),
                "limit": 5,
            },
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

        head_result = self._dispatch(prefix + "get", {"id": head_id})
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

    def _server_name(self) -> str:
        return str(self._config.get("mcp_server") or "ctx")

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
            except Exception:
                logger.exception("ctx checkpoint redaction failed for message %d", ordinal)
                return ""
            if safe:
                entries.append(f"### Message {ordinal} — {role}\n\n{safe}\n")
        return "\n".join(entries)

    def _split_chunks(self, rendered: str) -> list[str]:
        try:
            configured = int(self._config.get("chunk_chars", _DEFAULT_CHUNK_CHARS))
        except (TypeError, ValueError):
            configured = _DEFAULT_CHUNK_CHARS
        limit = max(1_000, min(configured, _MAX_CHUNK_CHARS))
        chunks: list[str] = []
        remaining = rendered
        while remaining:
            if len(remaining) <= limit:
                chunks.append(remaining)
                break
            cut = remaining.rfind("\n### Message ", 0, limit)
            if cut < limit // 3:
                cut = limit
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
    ) -> Optional[str]:
        prefix = f"mcp__{self._server_name()}__"
        result = self._dispatch(
            prefix + "store",
            {
                "category": str(self._config.get("category") or "compaction-checkpoints"),
                "title": title,
                "content": content,
                "tags": tags,
                "metadata": metadata,
                "sensitivity": str(self._config.get("sensitivity") or "internal"),
            },
        )
        block_id, payload_hash, error = _parse_tool_result(result)
        if error:
            logger.warning("ctx store failed: %s", error)
            return None
        if payload_hash and not block_id:
            confirmed = self._dispatch(prefix + "confirm", {"payload_hash": payload_hash})
            block_id, _unused_hash, error = _parse_tool_result(confirmed)
            if error:
                logger.warning("ctx staged store confirmation failed: %s", error)
                return None
        if not block_id:
            logger.warning("ctx store returned no block id")
        return block_id


def register(ctx) -> None:
    ctx.register_memory_provider(CtxCheckpointMemoryProvider())
