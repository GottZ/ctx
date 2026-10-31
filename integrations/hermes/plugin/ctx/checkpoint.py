# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
"""Transcript checkpoint chain head → manifest → parts (P3, REST transport).

Port of GottZ/ctx ``integrations/hermes/plugin/ctx_checkpoint/__init__.py``
(plugin 0.4.3, commit 38fdb74). Original work © Jan-Stefan Janetzky (GottZ),
licensed under the Mozilla Public License 2.0 — https://mozilla.org/MPL/2.0/.
This file is a modified version (MPL-2.0 §3.1): the MCP tool dispatch is replaced
by the plugin's REST ``CtxClient``; added are an explicit ``type=checkpoint``, a
time budget, rate-limit backoff, a part cap, a profile-configurable sensitivity
and an own provenance metadata key (``ctx_plugin``; ``metadata.provenance`` is
server-owned). Source of the original: https://github.com/GottZ/ctx

Wire format is kept BIT-IDENTICAL to upstream on purpose — the server classifies
the titles as type ``checkpoint`` (migrations M107 "Compaction source …" / M120
"Compaction checkpoint head …") and the distiller parses manifests via
``metadata.root_session_id`` + ``metadata.source_block_ids`` and parts via the
``### Message <n> — user|assistant`` header (go/internal/distillsource/ctxcheckpoint).
"""

from __future__ import annotations

import hashlib
import importlib
import json
import logging
import re
import sys
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

from .client import CtxApiError, response_code, store_block_id

logger = logging.getLogger(__name__)

PLUGIN_VERSION = "1.7.1"  # keep in sync with plugin.yaml (tests/test_host_contract.py)
UPSTREAM_REF = "GottZ/ctx integrations/hermes/plugin/ctx_checkpoint 0.4.3 @ 38fdb74 (MPL-2.0)"

CATEGORY_CHECKPOINT = "compaction-checkpoints"
CHECKPOINT_TYPE = "checkpoint"

_UUID_RE = re.compile(
    r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-"
    r"[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b"
)
_PRIOR_CONTEXT_PREFIX = "[PRIOR CONTEXT — for reference only; not a new message]"
_PRIOR_CONTEXT_MERGE_BOUNDARY = (
    "\n\n[END OF PRIOR CONTEXT — COMPACTION SUMMARY BELOW]\n\n[CONTEXT COMPACTION — REFERENCE ONLY]"
)
_SYNTHETIC_ASSISTANT_PREFIXES = (
    "[CONTEXT COMPACTION — REFERENCE ONLY]",
    "[Your active task list was preserved across context compression]",
    "<memory-context>",
)
DEFAULT_CHUNK_CHARS = 36_000
MAX_CHUNK_CHARS = 40_000
MIN_CHUNK_CHARS = 1_000
# Server write rate limit: 100 writes / 60 s per key (go/internal/config/config.go:860).
# One checkpoint = parts + manifest + head; the cap keeps a single chain below the
# window with headroom for facts/mirror writes of the same key.
DEFAULT_MAX_PARTS = 80
MAX_PARTS_CEILING = 95
# Backoff after a 429 that survived the client's own 1 s retry (seconds, bounded by the budget).
RATE_LIMIT_BACKOFF_S = (2.0, 4.0, 8.0)

# The value may start with an auth scheme ("Authorization: Bearer <token>"): without the
# optional scheme group only the scheme word would be masked and the credential would stay.
_GENERIC_SECRET_RE = re.compile(
    r"(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|"
    r"password|passwd|authorization)\b(\s*[:=]\s*)((?:(?:bearer|basic|digest)\s+)?[^\s,;]+)"
)
_BEARER_RE = re.compile(r"(?i)\b(Bearer\s+)[A-Za-z0-9._~+/-]{8,}=*")

PART_TAGS = ["hermes", "compaction", "source-evidence", "transcript", "control"]
MANIFEST_TAGS = ["hermes", "compaction", "checkpoint-manifest", "ctx", "control"]
HEAD_TAGS = ["hermes", "compaction", "checkpoint-head", "ctx", "control"]

# -- Checkpoint contract negotiation (upstream :47-129, unchanged semantics) ---------
# Highest checkpoint contract THIS plugin implements. 2 = the fail-closed contract
# (evidence_messages). 3 (tool evidence rendered into the checkpoint) is NOT implemented.
PLUGIN_CHECKPOINT_API_MAX = 2
HOST_V3_FEATURE = "_tool_evidence_for_pre_compress_memory"
_HOST_CONTRACT_MODULE = "agent.memory_provider"
_HOST_FEATURE_MODULE = "agent.conversation_compression"
_HOST_CONTRACT_CONSTANT = "PRE_COMPRESS_CHECKPOINT_API_VERSION"
_PROBE_FALLBACK_API = 2


class CtxCheckpointError(RuntimeError):
    """Raised when a durable ctx checkpoint cannot be completed."""


def _load_host_module(name: str):
    module = sys.modules.get(name)
    if module is not None:
        return module
    return importlib.import_module(name)


def _probe_host_checkpoint_api() -> int:
    """Checkpoint contract version of the loading host; never raises.

    3 only when the constant is >= 3 AND the host exposes ``HOST_V3_FEATURE`` as a
    callable; 2 when the constant is >= 2; else 1. Any failure → ``_PROBE_FALLBACK_API``.
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
        logger.debug("ctx checkpoint host probe failed; assuming v%d", _PROBE_FALLBACK_API, exc_info=True)
        return _PROBE_FALLBACK_API


def _negotiate_checkpoint_api(host_api: int) -> int:
    """Declared contract: min(host, plugin), never below 2 (a v1 host gate ``>= 1`` accepts 2)."""
    return max(2, min(int(host_api), PLUGIN_CHECKPOINT_API_MAX))


# -- Rendering (upstream :156-235, :561-606) ----------------------------------------


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
    """Conservative key/value redaction beyond the host's provider-specific rules."""
    text = _GENERIC_SECRET_RE.sub(r"\1\2[REDACTED]", text)
    return _BEARER_RE.sub(r"\1[REDACTED]", text)


def _host_redact(text: str) -> str:
    """Host ``agent.redact.redact_sensitive_text(force=True)``; import failure fails closed."""
    from agent.redact import redact_sensitive_text

    return redact_sensitive_text(text, force=True)


def redact_text(text: str) -> str:
    """Double redaction (host force + upstream generic rules) — defense in depth."""
    return _redact_checkpoint_secrets(_host_redact(text))


def _is_synthetic_handoff(message: dict[str, Any], raw: str) -> bool:
    if message.get("_compressed_summary"):
        return True
    if str(message.get("role") or "") != "assistant":
        return False
    stripped = raw.lstrip()
    if stripped.startswith(_PRIOR_CONTEXT_PREFIX):
        return _PRIOR_CONTEXT_MERGE_BOUNDARY in stripped
    return any(stripped.startswith(prefix) for prefix in _SYNTHETIC_ASSISTANT_PREFIXES)


def render_direct_evidence(messages: list[dict[str, Any]]) -> str:
    """Redacted ``### Message N — role`` transcript of direct user/assistant text only.

    Raises ``CtxCheckpointError`` when redaction fails (never stores unredacted text).
    """
    entries: list[str] = []
    for ordinal, message in enumerate(messages or [], start=1):
        if not isinstance(message, dict):
            continue
        role = str(message.get("role") or "")
        if role not in {"user", "assistant"}:
            continue
        raw = _flatten_content(message.get("content"))
        # The explicit marker is authoritative; content heuristics are a legacy fallback
        # for old assistant envelopes; user quotations must remain evidence.
        if _is_synthetic_handoff(message, raw):
            continue
        if not raw.lstrip():
            continue
        try:
            safe = redact_text(raw).strip()
        except Exception as exc:
            logger.warning("ctx checkpoint redaction failed for message %d (%s)", ordinal, type(exc).__name__)
            raise CtxCheckpointError("ctx checkpoint redaction failed") from exc
        if safe:
            entries.append(f"### Message {ordinal} — {role}\n\n{safe}\n")
    return "\n".join(entries)


def clamp_chunk_chars(value: Any) -> int:
    try:
        configured = int(value)
    except (TypeError, ValueError):
        configured = DEFAULT_CHUNK_CHARS
    return max(MIN_CHUNK_CHARS, min(configured, MAX_CHUNK_CHARS))


def split_chunks(rendered: str, chunk_chars: Any = DEFAULT_CHUNK_CHARS) -> list[str]:
    """Split at message boundaries (``\\n### Message ``) when possible; hard cut otherwise."""
    limit = clamp_chunk_chars(chunk_chars)
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


def _find_value(value: Any, key: str) -> str | None:
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


FACT_HASHES_KEY = "fact_hashes"
FACT_HASHES_META_MAX = 200


def _find_str_list(value: Any, key: str) -> list[str]:
    """First ``key`` holding a list of strings anywhere in a (possibly JSON-string) response."""
    if isinstance(value, dict):
        candidate = value.get(key)
        if isinstance(candidate, list):
            return [v for v in candidate if isinstance(v, str) and v]
        for nested in value.values():
            found = _find_str_list(nested, key)
            if found:
                return found
    elif isinstance(value, list):
        for nested in value:
            found = _find_str_list(nested, key)
            if found:
                return found
    elif isinstance(value, str) and value[:1] in "{[":
        try:
            return _find_str_list(json.loads(value), key)
        except (TypeError, ValueError):
            return []
    return []


def _merge_hashes(previous: list[str], new: list[str], cap: int = FACT_HASHES_META_MAX) -> list[str]:
    """Order-preserving union (previous first), newest ``cap`` kept."""
    merged: list[str] = []
    seen = set()
    for item in list(previous or []) + list(new or []):
        if isinstance(item, str) and item and item not in seen:
            seen.add(item)
            merged.append(item)
    return merged[-cap:]


def _find_block_id_by_title(value: Any, title: str) -> str | None:
    if isinstance(value, dict):
        candidate = value.get("id")
        if value.get("title") == title and isinstance(candidate, str) and _UUID_RE.fullmatch(candidate):
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
    return None


def _short_root(root_session_id: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]", "_", root_session_id)[:48]


def head_title_for(root_session_id: str) -> str:
    """Stable head title per root session (server classifies it as ``checkpoint``, M120)."""
    return f"Compaction checkpoint head {_short_root(root_session_id)}"


# -- Chain writer ----------------------------------------------------------------------


@dataclass
class ChainReceipt:
    head_id: str
    manifest_id: str
    part_ids: list[str]
    sha256: str
    parent_manifest_id: str | None
    checkpoint_id: str
    writes: int = 0
    duration_s: float = 0.0
    rate_limited: int = 0
    lookup: str = ""  # server | cache | none
    # P4: cumulative fact dedupe keys read from the previous head (server side)
    previous_fact_hashes: list[str] = field(default_factory=list)

    def frame(self) -> str:
        """Upstream receipt text (bounded, < 1000 chars; part IDs deliberately omitted)."""
        if not self.head_id:
            return "ctx checkpoint: empty (no direct user/assistant evidence — nothing to archive)"
        return (
            f"ctx checkpoint head: `{self.head_id}`\n"
            f"ctx checkpoint manifest: `{self.manifest_id}`\n"
            f"source transcript SHA-256: `{self.sha256}`\n"
            "Load the stable head first, then its latest manifest; load source "
            "blocks only when exact wording or provenance is needed."
        )


@dataclass
class ChainWriter:
    """Writes one checkpoint chain synchronously within a deadline. Fail-closed:
    every failure raises ``CtxCheckpointError``; the head is published LAST, so a
    partial failure never moves the stable head."""

    client: Any
    sensitivity: str = "personal"
    chunk_chars: int = DEFAULT_CHUNK_CHARS
    max_parts: int = DEFAULT_MAX_PARTS
    api_timeout: float = 10.0
    tags_for: Callable[[list[str]], list[str]] = lambda tags: list(tags)
    profile: str = "default"
    sleep: Callable[[float], None] = time.sleep
    monotonic: Callable[[], float] = time.monotonic
    _tool_evidence_warned: bool = field(default=False, init=False)

    # -- transport with budget + rate-limit backoff --

    def _call(
        self,
        what: str,
        deadline: float,
        budget_s: float,
        fn: Callable[..., dict[str, Any]],
        stats: dict[str, int],
        **kwargs,
    ) -> dict[str, Any]:
        attempt = 0
        while True:
            remaining = deadline - self.monotonic()
            if remaining <= 0:
                raise CtxCheckpointError(f"ctx checkpoint budget {budget_s:.0f}s exceeded before {what}")
            try:
                data = fn(timeout=max(0.5, min(self.api_timeout, remaining)), **kwargs)
            except CtxApiError as exc:
                if exc.code == "rate_limit" or exc.status == 429:
                    stats["rate_limited"] = stats.get("rate_limited", 0) + 1
                    if attempt < len(RATE_LIMIT_BACKOFF_S):
                        delay = RATE_LIMIT_BACKOFF_S[attempt]
                        if self.monotonic() + delay < deadline:
                            logger.info(
                                "ctx checkpoint %s rate-limited; backing off %.0fs (attempt %d)",
                                what,
                                delay,
                                attempt + 1,
                            )
                            self.sleep(delay)
                            attempt += 1
                            continue
                    raise CtxCheckpointError(
                        f"ctx checkpoint rate-limited during {what} (budget {budget_s:.0f}s)"
                    ) from exc
                code = f" [{exc.code}]" if exc.code else ""
                raise CtxCheckpointError(f"ctx checkpoint {what} failed{code}: {str(exc)[:200]}") from exc
            except CtxCheckpointError:
                raise
            except Exception as exc:
                raise CtxCheckpointError(f"ctx checkpoint {what} failed: {type(exc).__name__}") from exc
            if not isinstance(data, dict):
                raise CtxCheckpointError(f"ctx checkpoint {what} returned a non-object response")
            if data.get("success") is False:
                code = response_code(data)
                detail = str(data.get("error") or "unknown")[:200]
                raise CtxCheckpointError(f"ctx checkpoint {what} rejected{f' [{code}]' if code else ''}: {detail}")
            return data

    def _store(
        self,
        what: str,
        deadline: float,
        budget_s: float,
        stats: dict[str, int],
        *,
        title: str,
        content: str,
        metadata: dict[str, Any],
        tags: list[str],
    ) -> str:
        data = self._call(
            what,
            deadline,
            budget_s,
            self.client.store,
            stats,
            category=CATEGORY_CHECKPOINT,
            title=title,
            content=content,
            sensitivity=self.sensitivity,
            tags=self.tags_for(tags),
            type=CHECKPOINT_TYPE,
            metadata=metadata,
        )
        stats["writes"] = stats.get("writes", 0) + 1
        block_id = store_block_id(data)
        if not block_id:
            raise CtxCheckpointError(f"ctx checkpoint {what}: store returned no block id")
        return block_id

    def load_previous_head(
        self, head_title: str, deadline: float, budget_s: float, stats: dict[str, int]
    ) -> tuple[str | None, list[str]]:
        """``(latest_manifest_id, fact_hashes)`` of the existing head (upstream
        ``_load_previous_manifest_id`` plus the P4 dedupe keys from the same read).

        Searches the head by title and reads it. ``(None, [])`` = no head yet; a head
        without a valid ``latest_manifest_id`` raises ``CtxCheckpointError``.
        """
        result = self._call(
            "head search",
            deadline,
            budget_s,
            self.client.search,
            stats,
            text=head_title,
            category=CATEGORY_CHECKPOINT,
            types=[CHECKPOINT_TYPE],
            limit=10,
        )
        head_id = _find_block_id_by_title(result, head_title)
        if not head_id:
            return None, []
        head = self._call("head read", deadline, budget_s, self.client.get_block, stats, block_id=head_id)
        manifest_id = _find_value(head, "latest_manifest_id")
        if not manifest_id or not _UUID_RE.fullmatch(manifest_id):
            raise CtxCheckpointError("ctx checkpoint head is missing a valid latest_manifest_id")
        return manifest_id, _find_str_list(head, FACT_HASHES_KEY)

    def tool_index_status(self, tool_evidence: Any) -> str:
        if tool_evidence is None:
            return "absent"
        if not self._tool_evidence_warned:
            self._tool_evidence_warned = True
            logger.warning(
                "ctx checkpoint received tool_evidence (%d entries) but this plugin (contract max v%d) "
                "does not render it yet; stored as tool_index_status=received-unrendered",
                len(tool_evidence) if isinstance(tool_evidence, (list, tuple)) else -1,
                PLUGIN_CHECKPOINT_API_MAX,
            )
        return "received-unrendered"

    def write(
        self,
        messages: list[dict[str, Any]],
        *,
        root_session_id: str,
        session_id: str,
        platform: str = "",
        agent_context: str = "",
        budget_s: float = 20.0,
        deadline: float | None = None,
        tool_evidence: Any = None,
        cached_manifest_id: str | None = None,
        host_api: int | None = None,
        fact_hashes: list[str] | None = None,
    ) -> ChainReceipt:
        started = self.monotonic()
        if deadline is None:
            deadline = started + float(budget_s)
        host_api = _probe_host_checkpoint_api() if host_api is None else host_api
        negotiated_api = _negotiate_checkpoint_api(host_api)
        tool_status = self.tool_index_status(tool_evidence)

        rendered = render_direct_evidence(messages)
        if not rendered:
            # P6: nothing to archive — no direct user/assistant text means the
            # lossy summary has nothing to discard. Upstream raises
            # CtxCheckpointError here; under compression.checkpoint_required
            # that would block compaction for nothing. Return a no-op receipt
            # instead: success, nothing written (deviation, documented in README).
            return ChainReceipt(
                head_id="",
                manifest_id="",
                part_ids=[],
                sha256="",
                parent_manifest_id=None,
                checkpoint_id="",
                writes=0,
                duration_s=self.monotonic() - started,
                lookup="none",
            )
        digest = hashlib.sha256(rendered.encode("utf-8")).hexdigest()
        chunks = split_chunks(rendered, self.chunk_chars)
        max_parts = max(1, min(int(self.max_parts), MAX_PARTS_CEILING))
        if len(chunks) > max_parts:
            raise CtxCheckpointError(
                f"ctx checkpoint needs {len(chunks)} parts > checkpoint_max_parts {max_parts} "
                "(write rate limit 100/60s per key); nothing written"
            )

        now = datetime.now(timezone.utc).isoformat()
        root = root_session_id or session_id or "unknown"
        short_root = _short_root(root)
        head_title = head_title_for(root)
        stats: dict[str, int] = {"writes": 0, "rate_limited": 0}
        previous_manifest_id, previous_fact_hashes = self.load_previous_head(head_title, deadline, budget_s, stats)
        # P4: cumulative fact dedupe keys (previous head ∪ this run), newest kept, capped
        chain_fact_hashes = _merge_hashes(previous_fact_hashes, fact_hashes or [])
        lookup = "server" if previous_manifest_id else "none"
        if not previous_manifest_id and cached_manifest_id and _UUID_RE.fullmatch(cached_manifest_id):
            # search miss (ranking/backfill) but this process wrote the chain → keep it linked
            previous_manifest_id = cached_manifest_id
            lookup = "cache"
        checkpoint_id = hashlib.sha256(f"{root}\0{now}\0{digest}".encode()).hexdigest()[:16]
        title_base = f"Compaction source {short_root} {digest[:16]} {checkpoint_id}"
        common_metadata: dict[str, Any] = {
            "evidence_date": now,
            "source_kind": "redacted-direct-transcript",
            "root_session_id": root,
            "active_session_id": session_id,
            "platform": platform,
            "agent_context": agent_context,
            "host_checkpoint_api": host_api,
            "checkpoint_api_version": negotiated_api,
            "tool_index_status": tool_status,
            "sha256": digest,
            "warnings": ["W1", "W3", "W5", "W9", "W18", "W19", "W21"],
            "invalidated_by": "A verified correction or transcript-recovery finding",
            # own provenance key — metadata.provenance is server-owned (403 reserved_metadata)
            "ctx_plugin": {
                "writer": "hermes-ctx-plugin",
                "version": PLUGIN_VERSION,
                "profile": self.profile,
                "port_of": UPSTREAM_REF,
                "redaction": ["host:redact_sensitive_text(force=True)", "generic-key-value", "bearer"],
                "checkpoint_id": checkpoint_id,
            },
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
                f"- Active session: `{session_id}`\n"
                f"- Transcript SHA-256: `{digest}`\n"
                f"- Part: {index}/{len(chunks)}\n\n"
                "## Direct transcript\n\n"
                f"{chunk}"
            )
            metadata = dict(common_metadata)
            metadata.update(
                {"part": index, "parts": len(chunks), "part_sha256": hashlib.sha256(chunk.encode("utf-8")).hexdigest()}
            )
            chunk_ids.append(
                self._store(
                    f"source part {index}/{len(chunks)}",
                    deadline,
                    budget_s,
                    stats,
                    title=f"{title_base} part {index:03d} of {len(chunks):03d}",
                    content=body,
                    metadata=metadata,
                    tags=PART_TAGS,
                )
            )

        manifest = (
            "# Compaction checkpoint manifest\n\n"
            "Stable source index created before lossy context compaction. The linked "
            "blocks preserve the exact redacted user/assistant text available to the "
            "provider; local `state.db` remains the authoritative full transcript, "
            "including tool evidence.\n\n"
            f"- Root session: `{root}`\n"
            f"- Active session: `{session_id}`\n"
            f"- Transcript SHA-256: `{digest}`\n"
            f"- Parent manifest: "
            f"{f'`{previous_manifest_id}`' if previous_manifest_id else 'none'}\n"
            f"- Source parts: {len(chunk_ids)}\n\n"
            "## Source block IDs\n\n" + "\n".join(f"- `{block_id}`" for block_id in chunk_ids)
        )
        manifest_id = self._store(
            "manifest",
            deadline,
            budget_s,
            stats,
            title=f"{title_base} manifest",
            content=manifest,
            metadata={
                **common_metadata,
                "source_block_ids": chunk_ids,
                "parent_manifest_id": previous_manifest_id,
                FACT_HASHES_KEY: chain_fact_hashes,
            },
            tags=MANIFEST_TAGS,
        )

        head = (
            "# Compaction checkpoint head\n\n"
            "Stable read entrypoint for the latest confirmed pre-compaction "
            "checkpoint of this root session. Load the manifest next; load source "
            "blocks only when exact wording or provenance is needed.\n\n"
            f"- Checkpoint API version: {negotiated_api}\n"
            f"- Root session: `{root}`\n"
            f"- Active session: `{session_id}`\n"
            f"- Latest manifest: `{manifest_id}`\n"
            f"- Transcript SHA-256: `{digest}`\n"
            f"- Source parts: {len(chunk_ids)}\n"
        )
        head_id = self._store(
            "head publish",
            deadline,
            budget_s,
            stats,
            title=head_title,
            content=head,
            metadata={
                **common_metadata,
                "latest_manifest_id": manifest_id,
                "source_block_count": len(chunk_ids),
                FACT_HASHES_KEY: chain_fact_hashes,
            },
            tags=HEAD_TAGS,
        )
        return ChainReceipt(
            head_id=head_id,
            manifest_id=manifest_id,
            part_ids=chunk_ids,
            sha256=digest,
            parent_manifest_id=previous_manifest_id,
            checkpoint_id=checkpoint_id,
            writes=stats.get("writes", 0),
            duration_s=self.monotonic() - started,
            rate_limited=stats.get("rate_limited", 0),
            lookup=lookup,
            previous_fact_hashes=list(previous_fact_hashes),
        )
