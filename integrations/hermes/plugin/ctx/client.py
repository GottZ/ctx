"""CTX REST client — stdlib urllib, no external dependencies.

Wire shapes verified live against the production instance (2026-08-03):

- ``POST /api/query``  body: ``{"query": ..., "synthesize": bool, "include_content": bool}``
  NOTE the field is ``query``, NOT ``text`` (the server 400s on ``text``).
- ``POST /api/store``  body: ``{"category", "title", "content", "sensitivity"?, "type"?, "tags"?}``
  (``tags`` list live-verified 2026-10-20; an upsert WITHOUT tags resets them to [])
- ``POST /api/search`` body: ``{"query", "category"?, "tags"?, "types"?, "types_exclude"?, "limit"?}``
  (compact, no LLM; ``tags`` = any-of overlap ``tags && $n``; ``types_exclude`` opt-in bind parameter,
  server v5.x)
- ``POST /api/query`` also takes ``types_exclude`` (GottZ/ctx ``handler/query.go`` ``queryRequest``)
- Read rows/sources carry ``type`` and ``"untrusted": true`` for foreign-text types
  (``checkpoint``, ``tool-evidence``, ``tool-overview``, ``insight`` — docs/api.md "Untrusted framing", V-11)
- Write rejections carry a machine-readable ``"code"`` next to ``"error"``
  (``handler/errcode.go``, Gap-C6-c) → ``CtxApiError.code``
- ``GET  /api/whoami`` — key/scope validation
- ``POST /api/manage`` body: ``{"action": "get", "data": {"id": ...}}`` — full block

Auth: ``X-Context-Key: <key>`` (alternative ``Authorization: Bearer``).
"""

from __future__ import annotations

import json
import logging
import time
import urllib.error
import urllib.request
from typing import Any

logger = logging.getLogger(__name__)


# Closed rejection vocabulary of the CTX write surfaces (GottZ/ctx
# go/internal/handler/errcode.go, rejectClass table; docs/api.md "/api/blob/*").
REJECT_CODES = (
    "unauthorized",
    "invalid_body",
    "missing_fields",
    "invalid_sensitivity",
    "invalid_encoding",
    "scope_denied",
    "size_cap",
    "unknown_type",
    "reserved_type",
    "parent_required",
    "unknown_parent",
    "reserved_category",
    "reserved_metadata",
    "provenance_protected",
    "unknown_action",
    "constraint",
    "rate_limit",
    "internal",
)
# Codes that are retried once (transient); everything else is permanent — repeating
# the identical write yields the identical verdict.
RETRYABLE_CODES = frozenset({"rate_limit"})
# Metadata keys the server owns (403 reserved_metadata) — rejected client-side.
RESERVED_METADATA_KEYS = ("provenance",)


class CtxApiError(Exception):
    """Raised for HTTP/connection errors. Callers degrade, never crash.

    ``status`` = HTTP status (None for connection errors), ``code`` = the server's
    machine-readable rejection code (``""`` when the answer carried none).
    """

    def __init__(self, message: str, *, status: int | None = None, code: str = ""):
        super().__init__(message)
        self.status = status
        self.code = code or ""

    @property
    def permanent(self) -> bool:
        """True for a coded rejection that must not be retried."""
        return bool(self.code) and self.code not in RETRYABLE_CODES


def response_code(data: Any) -> str:
    """Machine-readable code of a ``{"success": false, "code": ...}`` envelope, else ``""``."""
    if isinstance(data, dict):
        code = data.get("code")
        if isinstance(code, str):
            return code.strip()
    return ""


def _parse_error_body(body: str) -> tuple:
    """(code, error-text) from an error response body; tolerant of non-JSON."""
    try:
        parsed = json.loads(body)
    except ValueError:
        return "", ""
    if not isinstance(parsed, dict):
        return "", ""
    err = parsed.get("error")
    return response_code(parsed), (str(err) if err is not None else "")


def store_action(resp: dict[str, Any]) -> str:
    """Classify a /api/store response as created/updated/noop (live-verified 2026-10-20).

    - identical content → ``{"action": "noop", "existing_id": ..., "reason": ...}``
      (tags are NOT updated on noop)
    - create/update → ``{"block": {..., "created_at", "updated_at"}}`` (no action
      field) — equal timestamps mean the block was just created.
    """
    if not isinstance(resp, dict):
        return "unknown"
    action = resp.get("action")
    if isinstance(action, str) and action:
        return action
    block = resp.get("block")
    if isinstance(block, dict):
        created, updated = block.get("created_at"), block.get("updated_at")
        if created and updated:
            return "created" if created == updated else "updated"
        return "stored"
    return "stored" if resp.get("success") else "unknown"


def store_block_id(resp: dict[str, Any]) -> str:
    if not isinstance(resp, dict):
        return ""
    block = resp.get("block")
    if isinstance(block, dict) and block.get("id"):
        return str(block["id"])
    return str(resp.get("id") or resp.get("existing_id") or "")


class CtxClient:
    """Minimal, dependency-free client for the CTX HTTP API."""

    def __init__(
        self,
        base_url: str,
        api_key: str,
        timeout: float = 10.0,
        synthesis_timeout: float = 90.0,
    ):
        self._base = base_url.rstrip("/")
        self._key = api_key
        self._timeout = timeout
        self._synthesis_timeout = synthesis_timeout

    # -- low-level -----------------------------------------------------------

    def _request(
        self, method: str, path: str, payload: dict[str, Any] | None = None, timeout: float | None = None
    ) -> dict[str, Any]:
        url = f"{self._base}{path}"
        headers = {
            "X-Context-Key": self._key,
            "Content-Type": "application/json",
            "Accept": "application/json",
        }
        data = json.dumps(payload).encode("utf-8") if payload is not None else None
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        t = timeout if timeout is not None else self._timeout
        last_error: CtxApiError | None = None

        for attempt in (0, 1):  # one retry on 429 (per-key write rate limit)
            try:
                with urllib.request.urlopen(req, timeout=t) as resp:
                    raw = resp.read().decode("utf-8", "replace")
                try:
                    parsed = json.loads(raw)
                except ValueError:
                    return {"success": False, "error": "invalid JSON response from CTX"}
                if not isinstance(parsed, dict):
                    return {"success": False, "error": "unexpected response shape from CTX"}
                return parsed
            except urllib.error.HTTPError as exc:
                body = ""
                try:
                    body = exc.read(16384).decode("utf-8", "replace")
                except Exception:
                    pass
                code, err_text = _parse_error_body(body)
                detail = err_text or body[:300] or str(exc.reason)
                message = f"HTTP {exc.code}{f' [{code}]' if code else ''}: {detail[:300]}"
                error = CtxApiError(message, status=exc.code, code=code)
                # Retry once only on a transient 429 (per-key write rate limit);
                # permanent rejection codes are never repeated.
                if exc.code == 429 and not error.permanent and attempt == 0:
                    time.sleep(1.0)
                    last_error = error
                    continue
                raise error from exc
            except (urllib.error.URLError, TimeoutError, OSError) as exc:
                raise CtxApiError(f"connection error: {exc}") from exc

        raise last_error or CtxApiError("request failed")

    # -- API -------------------------------------------------------------

    def whoami(self) -> dict[str, Any]:
        return self._request("GET", "/api/whoami")

    def query(
        self,
        text: str,
        *,
        synthesize: bool = False,
        include_content: bool = True,
        types_exclude: list[str] | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {"query": text, "synthesize": synthesize}
        if include_content:
            payload["include_content"] = True
        if types_exclude:
            payload["types_exclude"] = list(types_exclude)
        t = self._synthesis_timeout if synthesize else None
        return self._request("POST", "/api/query", payload, timeout=t)

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
        payload: dict[str, Any] = {"query": text}
        if category:
            payload["category"] = category
        if tags:
            payload["tags"] = list(tags)
        if types:
            payload["types"] = list(types)
        if types_exclude:
            payload["types_exclude"] = list(types_exclude)
        if limit:
            payload["limit"] = int(limit)
        return self._request("POST", "/api/search", payload, timeout=timeout)

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
        if metadata:
            for key in RESERVED_METADATA_KEYS:
                if key in metadata:
                    raise CtxApiError(
                        f"metadata key '{key}' is server-owned (rejected client-side)",
                        status=None,
                        code="reserved_metadata",
                    )
        payload: dict[str, Any] = {"category": category, "title": title, "content": content}
        if sensitivity:
            payload["sensitivity"] = sensitivity
        if tags is not None:
            payload["tags"] = list(tags)
        if type:
            payload["type"] = type
        if parent_id:
            payload["parent_id"] = parent_id
        if metadata:
            payload["metadata"] = dict(metadata)
        return self._request("POST", "/api/store", payload, timeout=timeout)

    def get_block(self, block_id: str, timeout: float | None = None) -> dict[str, Any]:
        # Live-verified v4.x contract (2026-08-13): /api/manage expects "id" at
        # top level; the nested data.id form is rejected with
        # "Missing required field: id". Fall back for older server versions.
        payload: dict[str, Any] = {"action": "get", "id": block_id}
        try:
            return self._request("POST", "/api/manage", payload, timeout=timeout)
        except CtxApiError as exc:
            if "Missing required field: id" not in str(exc):
                raise
        return self._request("POST", "/api/manage", {"action": "get", "data": {"id": block_id}}, timeout=timeout)
