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
  identical content already exists (id: <uuid>)``. The block id is the ``(id: …)`` group
  (or a structured ``id``), never another UUID in the text: the title in front of it can
  carry a UUID-shaped session id. Rejections come back as ``{"error": "..."}`` from the
  dispatcher; a rate-limit text maps to ``code="rate_limit"`` so the chain writer's backoff
  applies. A ``confirm_writes`` key gets the write STAGED instead (``mcp_confirm.go``),
  which also arrives as an error envelope; it maps to ``code="confirm_writes"`` and is
  refused — writes from this plugin run unattended and are never confirmed on the
  operator's behalf (0.6.1).
- ``get`` → the full block JSON → ``{"success", "block"}``.

Hermes wraps every MCP text result as ``{"result": "<text>"}`` (``tools/mcp_tool_handlers``)
and every failure as ``{"error": "..."}``; both shapes, plus bare JSON, are accepted here.

A ``timeout`` passed to ``search``/``store``/``get_block`` (the chain writer passes its
remaining budget) bounds that one dispatch: it runs on a worker thread with the caller's
context variables and is abandoned at the deadline (``code="timeout"``). The worker acts
for the calling thread in Hermes' interrupt registry, so a ``/stop`` aimed at the caller
ends a bounded call as it ends an unbounded one. Without a ``timeout``, the MCP
transport's own timeout applies (300 s by default in Hermes). Hermes serializes calls per
MCP server, so an abandoned call still completes before the next call is sent.
"""

from __future__ import annotations

import contextvars
import json
import logging
import re
import threading
from collections.abc import Callable
from typing import Any

from .client import CtxApiError

logger = logging.getLogger(__name__)

_UUID_RE = re.compile(r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b")
_ANSWER_ID_RE = re.compile(r"\(id:\s*(" + _UUID_RE.pattern + r")")
_PAYLOAD_HASH_RE = re.compile(r"payload_hash:\s*([0-9a-fA-F]{16,128})")
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
    if lowered.lstrip().startswith("staged") or "confirm_writes" in lowered:
        return "confirm_writes"
    if any(marker in lowered for marker in _RATE_LIMIT_MARKERS):
        return "rate_limit"
    return ""


def _interrupt_acting_for() -> Any:
    """Hermes' ``tools.interrupt.acting_for_tid`` ContextVar; ``None`` on a host without it."""
    try:
        from tools.interrupt import acting_for_tid
    except ImportError:
        return None
    return acting_for_tid


def _run_bounded(fn: Callable[[], Any], timeout: float) -> tuple[bool, Any, BaseException | None]:
    """Run ``fn`` on a daemon worker thread and wait at most ``timeout`` seconds.

    Returns ``(finished, value, error)``. A call still running at the timeout is
    abandoned, not cancelled: it ends in the background (at the latest at the MCP
    transport's own timeout) and its result is dropped. The caller's context variables
    travel along, so host state bound to the agent's context (approval prompts, the
    session key) stays visible to the dispatch.

    Hermes' interrupts are per thread: ``is_interrupted()`` checks the current thread and
    ``acting_for_tid``. The worker therefore acts for the calling thread (or for the
    thread the caller already acts for), so a ``/stop`` aimed at the caller reaches the
    MCP wait (``tools/mcp_tool_loop._run_on_mcp_loop`` polls ``is_interrupted()``) the same
    way it reaches an unbounded call; Hermes' terminal tool does the same around its
    deadline worker. The value is set inside the copied context: set in the caller after
    the copy, it would never reach the worker.
    """
    box: dict[str, Any] = {}
    context = contextvars.copy_context()
    caller_tid = threading.get_ident()
    acting_for = _interrupt_acting_for()

    def target() -> Any:
        if acting_for is not None and acting_for.get() is None:
            acting_for.set(caller_tid)
        return fn()

    def run() -> None:
        try:
            box["value"] = context.run(target)
        except BaseException as exc:  # handed back to the calling thread
            box["error"] = exc

    worker = threading.Thread(target=run, name="ctx-mcp-call", daemon=True)
    worker.start()
    worker.join(timeout)
    if worker.is_alive():
        return False, None, None
    return True, box.get("value"), box.get("error")


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
        # Informational defaults: only an explicit per-call ``timeout`` (the chain writer's
        # remaining budget) bounds a dispatch; otherwise the MCP transport owns its timeouts.
        self._timeout = timeout
        self._synthesis_timeout = synthesis_timeout

    @property
    def server_name(self) -> str:
        return self._server

    # -- transport ----------------------------------------------------------------

    def _call(self, tool: str, args: dict[str, Any], timeout: float | None = None) -> tuple[Any, str]:
        """Dispatch one tool; returns ``(payload, text)``. Raises ``CtxApiError`` on an error envelope.

        With ``timeout`` the dispatch is bounded (see the module docstring) and an
        abandoned call raises ``CtxApiError(code="timeout")``.
        """
        name = f"mcp__{self._server}__{tool}"
        try:
            if timeout is None:
                raw = self._dispatch(name, args)
            else:
                finished, raw, error = _run_bounded(lambda: self._dispatch(name, args), timeout)
                if not finished:
                    raise CtxApiError(
                        f"mcp {tool}: no answer within {timeout:.1f}s; the call was abandoned", code="timeout"
                    )
                if error is not None:
                    raise error
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
                if code == "confirm_writes":
                    # Guidance and hash first: the chain writer keeps only the first 200
                    # characters of this message, and the server name is the variable part.
                    hash_match = _PAYLOAD_HASH_RE.search(text)
                    raise CtxApiError(
                        f"mcp {tool}: staged, not stored — the key has confirm_writes and this plugin never "
                        "confirms on the operator's behalf; "
                        + (f"payload_hash {hash_match.group(1)}; " if hash_match else "")
                        + f"use a key without confirm_writes for mcp_servers.{self._server}",
                        code=code,
                    )
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
        payload, body = self._call("search", args, timeout)
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
        # A confirm_writes key gets the write staged; _call raises code="confirm_writes" for it.
        payload, body = self._call("store", args, timeout)
        block_id = _store_id(payload, body)
        if not block_id:
            return {"success": False, "error": f"store returned no block id: {body[:200]}"}
        if "No change needed" in body:
            return {"success": True, "action": "noop", "existing_id": block_id, "reason": "identical content"}
        return {"success": True, "block": {"id": block_id, "title": title, "category": category}}

    def get_block(self, block_id: str, timeout: float | None = None) -> dict[str, Any]:
        payload, body = self._call("get", {"id": block_id}, timeout)
        block = self._as_json(body)
        if not isinstance(block, dict) and isinstance(payload, dict) and "result" not in payload:
            block = payload
        if not isinstance(block, dict):
            return {"success": False, "error": f"get returned no block: {body[:200]}"}
        if isinstance(block.get("block"), dict):
            block = block["block"]
        return {"success": True, "block": block}


def _store_id(payload: Any, text: str) -> str | None:
    """Block id of a store answer: a valid structured ``id``, else the ``(id: <uuid>)`` group.

    Never any other UUID in the answer text: it starts with the block title, which can carry
    a UUID-shaped root session id, and the first-UUID fallback took that for the block id
    (0.6.1). No id → ``None`` and the caller reports the store as failed.
    """
    block_id = _find_value(payload, "id") if payload is not None else None
    if block_id and _UUID_RE.fullmatch(block_id):
        return block_id
    match = _ANSWER_ID_RE.search(text)
    return match.group(1) if match else None


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
