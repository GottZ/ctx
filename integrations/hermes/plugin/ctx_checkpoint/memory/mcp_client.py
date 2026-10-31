"""ctx client over Hermes' own MCP dispatch — the credential-free transport.

Same method surface as :class:`client.CtxClient` (``whoami``, ``query``, ``search``,
``store``, ``get_block``) and the same response dictionaries, so the provider, the chain
writer and the agent tools run unchanged on either transport. The calls go through
``tools.registry.registry.dispatch("mcp__<server>__<tool>", args)``: Hermes talks to the
ctx MCP server configured in ``mcp_servers.<server>`` and the plugin never sees a key.

What the ctx MCP tools answer (``go/internal/handler/mcp.go``) and how it is mapped:

- ``search`` → JSON list of block previews (``id``, ``title``, ``category``, ``type``,
  ``untrusted``, ``content``/``content_preview``, ``score`` …) → ``{"success", "results",
  "count"}``. The MCP tool rejects a ``types`` filter that names a retrieval-excluded type
  (``checkpoint``), while results of every type are returned when no filter is given — so
  ``types`` is dropped here on purpose and the chain writer's head lookup works by title.
- ``query`` → prose answer followed by ``Sources:`` lines ``[N] title (category) id:uuid``.
  The MCP query ALWAYS runs the LLM synthesis; a retrieval-only ``query(synthesize=False)``
  is therefore served by ``search`` (lexical hybrid, no embedding). Operators who want the
  semantic ``/api/query`` retrieval for recall choose ``transport: rest``.
- ``store`` → ``Stored: <title> (id: <uuid>, category: <c>)`` or ``No change needed —
  identical content already exists (id: <uuid>)``; a ``confirm_writes`` key answers with a
  ``payload_hash`` that is confirmed through the ``confirm`` tool. Rejections come back as
  ``{"error": "..."}`` from the dispatcher; a rate-limit text maps to ``code="rate_limit"``
  so the chain writer's backoff applies.
- ``get`` → the full block JSON → ``{"success", "block"}``.

Hermes wraps every MCP text result as ``{"result": "<text>"}`` (``tools/mcp_tool_handlers``)
and every failure as ``{"error": "..."}``; both shapes, plus bare JSON, are accepted here.
"""

from __future__ import annotations

import json
import logging
import re
from collections.abc import Callable
from typing import Any

from .client import CtxApiError

logger = logging.getLogger(__name__)

_UUID_RE = re.compile(r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b")
_SOURCE_LINE_RE = re.compile(r"^\[(\d+|n/a)\]\s+(.*?)\s+\(([^()]*)\)\s+id:([0-9a-fA-F-]{36})\s*$")
_SERVER_NAME_RE = re.compile(r"[A-Za-z0-9_-]+")
_RATE_LIMIT_MARKERS = ("rate limit", "rate_limit")
_MAX_CONTENT_FETCH = 10  # query(include_content=True) over MCP: one get per source, capped


def _find_value(value: Any, key: str) -> str | None:
    """First non-empty string under ``key`` anywhere in a (possibly JSON-string) structure."""
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
    elif isinstance(value, str) and value[:1] in "{[":
        try:
            return _find_value(json.loads(value), key)
        except (TypeError, ValueError):
            return None
    return None


def _code_for_error(text: str) -> str:
    lowered = text.lower()
    if any(marker in lowered for marker in _RATE_LIMIT_MARKERS):
        return "rate_limit"
    return ""


class McpClient:
    """ctx over ``mcp__<server>__*`` tool dispatch. Holds no credentials."""

    transport = "mcp"

    def __init__(
        self,
        dispatch: Callable[..., Any],
        server_name: str = "ctx",
        *,
        timeout: float = 10.0,
        synthesis_timeout: float = 90.0,
    ):
        if not _SERVER_NAME_RE.fullmatch(server_name or ""):
            raise ValueError(f"MCP server name {server_name!r} may only contain letters, digits, '_' and '-'")
        self._dispatch = dispatch
        self._server = server_name
        self._timeout = timeout  # informational: the MCP transport owns its timeouts
        self._synthesis_timeout = synthesis_timeout

    @property
    def server_name(self) -> str:
        return self._server

    # -- transport ----------------------------------------------------------------

    def _call(self, tool: str, args: dict[str, Any]) -> tuple[Any, str]:
        """Dispatch one tool; returns ``(payload, text)``. Raises ``CtxApiError`` on an error envelope."""
        try:
            raw = self._dispatch(f"mcp__{self._server}__{tool}", args)
        except CtxApiError:
            raise
        except Exception as exc:
            raise CtxApiError(f"mcp dispatch failed: {type(exc).__name__}: {exc}") from exc
        payload: Any = raw
        if isinstance(raw, str):
            try:
                payload = json.loads(raw)
            except (TypeError, ValueError):
                payload = {"result": raw}
        if isinstance(payload, dict):
            error = payload.get("error")
            if error:
                text = str(error)
                code = _code_for_error(text)
                raise CtxApiError(f"mcp {tool}: {text[:300]}", status=429 if code == "rate_limit" else None, code=code)
            if "result" in payload and isinstance(payload["result"], str):
                return payload, payload["result"]
        return payload, raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False, default=str)

    @staticmethod
    def _as_json(text: str) -> Any:
        try:
            return json.loads(text)
        except (TypeError, ValueError):
            return None

    # -- API -----------------------------------------------------------------------

    def whoami(self) -> dict[str, Any]:
        """No network: the MCP server entry is Hermes' to validate (`hermes mcp test <name>`)."""
        return {"success": True, "home_scope": "", "label": f"mcp:{self._server}", "role": "", "transport": "mcp"}

    def search(
        self,
        text: str,
        *,
        category: str | None = None,
        types_exclude: list[str] | None = None,
        tags: list[str] | None = None,
        types: list[str] | None = None,
        limit: int | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        args: dict[str, Any] = {"query": text}
        if category:
            args["category"] = category
        if tags:
            args["tags"] = list(tags)
        if types_exclude:
            args["types_exclude"] = list(types_exclude)
        if limit:
            args["limit"] = int(limit)
        payload, body = self._call("search", args)
        results = self._as_json(body)
        if results is None and isinstance(payload, list):
            results = payload
        if isinstance(results, dict) and isinstance(results.get("results"), list):
            results = results["results"]
        if not isinstance(results, list):
            results = []
        rows = [r for r in results if isinstance(r, dict)]
        return {"success": True, "results": rows, "count": len(rows)}

    def query(
        self,
        text: str,
        *,
        synthesize: bool = False,
        include_content: bool = True,
        types_exclude: list[str] | None = None,
        limit: int | None = None,
    ) -> dict[str, Any]:
        if not synthesize:
            # Retrieval-only recall: the MCP query tool always synthesizes, so the lexical
            # hybrid search is the no-LLM path. Rows already carry content/type/untrusted.
            data = self.search(text, types_exclude=types_exclude, limit=limit)
            return {"success": True, "sources": data.get("results") or [], "answer": ""}
        args: dict[str, Any] = {"question": text}
        if limit:
            args["limit"] = int(limit)
        if types_exclude:
            args["types_exclude"] = list(types_exclude)
        _payload, body = self._call("query", args)
        answer, sources = _parse_query_text(body)
        if include_content:
            for source in sources[: limit or _MAX_CONTENT_FETCH]:
                try:
                    block = self.get_block(source["id"]).get("block") or {}
                except CtxApiError as exc:
                    logger.debug("mcp query: content fetch for %s failed: %s", source["id"], exc)
                    continue
                for key in ("content", "type", "untrusted", "score", "tags"):
                    if key in block and key not in source:
                        source[key] = block[key]
        return {"success": True, "answer": answer, "sources": sources}

    def store(
        self,
        category: str,
        title: str,
        content: str,
        sensitivity: str | None = None,
        tags: list[str] | None = None,
        *,
        type: str | None = None,
        parent_id: str | None = None,
        metadata: dict[str, Any] | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        if parent_id:
            raise CtxApiError("parent_id is not supported over the MCP transport", code="invalid_body")
        if metadata and "provenance" in metadata:
            raise CtxApiError(
                "metadata key 'provenance' is server-owned (rejected client-side)", code="reserved_metadata"
            )
        args: dict[str, Any] = {"category": category, "title": title, "content": content}
        if sensitivity:
            args["sensitivity"] = sensitivity
        if tags is not None:
            args["tags"] = list(tags)
        if metadata:
            args["metadata"] = dict(metadata)
        if type:
            args["type"] = type
        payload, body = self._call("store", args)
        block_id, payload_hash = _store_ids(payload, body)
        if payload_hash and not block_id:
            # confirm_writes key: the store was staged; confirm it to get the block id.
            confirmed, confirmed_text = self._call("confirm", {"payload_hash": payload_hash})
            block_id, _ = _store_ids(confirmed, confirmed_text)
        if not block_id:
            return {"success": False, "error": f"store returned no block id: {body[:200]}"}
        if "No change needed" in body:
            return {"success": True, "action": "noop", "existing_id": block_id, "reason": "identical content"}
        return {"success": True, "block": {"id": block_id, "title": title, "category": category}}

    def get_block(self, block_id: str, timeout: float | None = None) -> dict[str, Any]:
        payload, body = self._call("get", {"id": block_id})
        block = self._as_json(body)
        if not isinstance(block, dict) and isinstance(payload, dict) and "result" not in payload:
            block = payload
        if not isinstance(block, dict):
            return {"success": False, "error": f"get returned no block: {body[:200]}"}
        if isinstance(block.get("block"), dict):
            block = block["block"]
        return {"success": True, "block": block}


def _store_ids(payload: Any, text: str) -> tuple[str | None, str | None]:
    """``(block_id, payload_hash)`` from a store/confirm answer in any of the known shapes."""
    block_id = _find_value(payload, "id") if payload is not None else None
    if not (block_id and _UUID_RE.fullmatch(block_id)):
        match = re.search(r"\(id:\s*([0-9a-fA-F-]{36})", text) or _UUID_RE.search(text)
        block_id = match.group(1) if match and match.lastindex else (match.group(0) if match else None)
    payload_hash = _find_value(payload, "payload_hash") if payload is not None else None
    return block_id, payload_hash


def _parse_query_text(body: str) -> tuple[str, list[dict[str, Any]]]:
    """Split the MCP query text into ``(answer, sources)``."""
    answer, _, tail = body.partition("\n\nSources:\n")
    sources: list[dict[str, Any]] = []
    for line in tail.splitlines():
        match = _SOURCE_LINE_RE.match(line.strip())
        if not match:
            continue
        entry: dict[str, Any] = {"id": match.group(4), "title": match.group(2), "category": match.group(3)}
        if match.group(1) != "n/a":
            entry["citation_index"] = int(match.group(1))
        sources.append(entry)
    return answer.strip(), sources
