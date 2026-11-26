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

import http.client
import json
import logging
import time
import urllib.error
import urllib.parse
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
    "write_conflict",
    "internal",
)
# Codes that are retried once (transient); everything else is permanent — repeating
# the identical write yields the identical verdict. write_conflict (409, GottZ/ctx#547): the
# write met concurrent changes of the rows it needed and wrote nothing — the same write sent
# again normally goes through.
RETRYABLE_CODES = frozenset({"rate_limit", "write_conflict"})
# Metadata keys the server owns (403 reserved_metadata) — rejected client-side.
RESERVED_METADATA_KEYS = ("provenance",)
# Replacement for an API key echoed back in any error text (server body, proxy, socket).
REDACTED = "[REDACTED]"
# Redirect statuses urllib's HTTPRedirectHandler would follow; CtxClient refuses all of them.
REDIRECT_CODES = (301, 302, 303, 307, 308)
# Why CtxClient sends nothing for a base URL with user info, a query or a fragment (#311).
BASE_URL_REFUSED = (
    "CTX base URL refused before any connection: it carries user info, a query or a fragment, which the "
    "ctx API does not take; set it to the server address alone (https://host[:port]) and the key in CTX_KEY"
)


class _RefuseRedirect(urllib.request.HTTPRedirectHandler):
    """Never follow a redirect: the 30x surfaces as ``HTTPError``.

    urllib's default handler copies every request header — ``X-Context-Key`` included — into
    the request to the ``Location``, without comparing hosts (POST 301/302/303 become a GET).
    The ctx API never redirects ``/api/*``; a redirect means the base URL is not the ctx API.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


# The default opener's handlers (proxies from the environment, https, error processing) with
# the redirect handler replaced.
_OPENER = urllib.request.build_opener(_RefuseRedirect)


# The wait before the one retry of a 429 (per-key write rate limit) or a 409 write_conflict.
# Clock and sleep are seams.
RATE_RETRY_WAIT_S = 1.0
_monotonic = time.monotonic
_sleep = time.sleep


def _urlopen(req: urllib.request.Request, *, timeout: float) -> Any:
    """``urllib.request.urlopen`` without following redirects (see ``_RefuseRedirect``)."""
    return _OPENER.open(req, timeout=timeout)


def redact_secret(text: Any, secret: str | None) -> str:
    """``text`` with every exact occurrence of ``secret`` replaced by ``[REDACTED]``.

    Apply BEFORE any truncation, otherwise a cut could leave a key prefix behind.
    """
    out = "" if text is None else str(text)
    if secret and out:
        out = out.replace(secret, REDACTED)
    return out


def redact_deep(value: Any, secret: str | None) -> Any:
    """Recursively redact ``secret`` in every string VALUE of a JSON-like structure.

    Dict keys and non-string scalars are returned unchanged; containers are rebuilt.
    """
    if not secret:
        return value
    if isinstance(value, str):
        return value.replace(secret, REDACTED)
    if isinstance(value, dict):
        return {k: redact_deep(v, secret) for k, v in value.items()}
    if isinstance(value, list):
        return [redact_deep(v, secret) for v in value]
    return value


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


def is_failure_envelope(data: Any) -> bool:
    """Is a returned answer a failure envelope? ``success: false``, a non-empty ``error`` without
    ``success: true`` (a proxy in front of ctx may answer so; ctx does not), or no object at all.
    The client redacts such an answer as a failure (``CtxClient._request``) and the chain writer
    counts it as an error answer of the server entry (GottZ/ctx#289: one reading for both)."""
    if not isinstance(data, dict):
        return True
    success = data.get("success")
    return success is False or (success is not True and bool(data.get("error")))


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
        parsed = urllib.parse.urlsplit(str(base_url or "").strip())
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            # urllib would happily open file:// or other schemes; the store is an http(s) API.
            raise ValueError("CTX base URL must be an http(s) URL with a host")
        # urllib takes user info for part of the host (a lookup of ``user:pass@host``, or the password
        # read as the port, which its error text then repeats) and puts every request path after a
        # query or fragment: such a base URL never reaches the API. Requests are refused before any
        # connection, with a text that names no part of the URL (#311). Any ``@`` after the scheme
        # counts: a ``/`` in the user info ends the host before it (``https://u:1234/pass@host``).
        self._url_refused = "@" in str(base_url).split("://", 1)[1] or bool(parsed.query or parsed.fragment)
        self._base = base_url.strip().rstrip("/")
        self._key = api_key
        self._timeout = timeout
        self._synthesis_timeout = synthesis_timeout

    # -- low-level -----------------------------------------------------------

    def _redact(self, text: Any) -> str:
        """Central key redaction for every text leaving this client as an error."""
        return redact_secret(text, self._key)

    def _http_failure(self, exc: urllib.error.HTTPError) -> CtxApiError:
        """The error an HTTP error answer is raised as: status, code and text redacted."""
        if exc.code in REDIRECT_CODES:
            # Not followed (``_RefuseRedirect``); the Location is not part of the message.
            return CtxApiError(
                f"HTTP {exc.code}: redirect not followed — the ctx API never redirects; check the base URL",
                status=exc.code,
            )
        body = ""
        try:
            body = exc.read(16384).decode("utf-8", "replace")
        except Exception as read_exc:
            # No exc_info: the traceback would chain the unredacted HTTPError.
            logger.debug("CTX error body unreadable (%s)", type(read_exc).__name__)
        code, err_text = _parse_error_body(body)
        # Redact the FULL texts first, truncate afterwards (no key prefix survives a cut).
        # Codes from the closed vocabulary stay verbatim (machine-readable contract).
        if code not in REJECT_CODES:
            code = self._redact(code)
        detail = self._redact(err_text) or self._redact(body)[:300] or self._redact(exc.reason)
        message = f"HTTP {exc.code}{f' [{code}]' if code else ''}: {detail[:300]}"
        return CtxApiError(message, status=exc.code, code=code)

    def _request(
        self, method: str, path: str, payload: dict[str, Any] | None = None, timeout: float | None = None
    ) -> dict[str, Any]:
        if self._url_refused:
            raise CtxApiError(BASE_URL_REFUSED)
        url = f"{self._base}{path}"
        headers = {
            "X-Context-Key": self._key,
            "Content-Type": "application/json",
            "Accept": "application/json",
        }
        data = json.dumps(payload).encode("utf-8") if payload is not None else None
        req = urllib.request.Request(url, data=data, headers=headers, method=method)  # noqa: S310 — http(s) only, see __init__
        t = timeout if timeout is not None else self._timeout
        # The timeout bounds the whole request: the one 429 retry and the wait before it included,
        # so a caller's deadline (a checkpoint passes the time it leaves) also holds for the retry.
        end = _monotonic() + t
        last_error: CtxApiError | None = None

        for attempt in (0, 1):  # one retry on 429 (per-key write rate limit) or 409 write_conflict
            failure: CtxApiError | None = None
            try:
                with _urlopen(req, timeout=t) as resp:
                    raw = resp.read().decode("utf-8", "replace")
                try:
                    parsed = json.loads(raw)
                except ValueError:
                    return {"success": False, "error": "invalid JSON response from CTX"}
                if not isinstance(parsed, dict):
                    return {"success": False, "error": "unexpected response shape from CTX"}
                # Failure envelopes (HTTP 200 + success:false, or a non-empty ``error``
                # without success:true) may echo the key in error texts/objects or codes:
                # redact every string VALUE there. Successful data (incl. store/noop answers
                # without a success field) is never rewritten — a short key would corrupt
                # payloads; an ``error`` value next to it is redacted on its own.
                # Closed-vocabulary codes never contain a key and stay verbatim.
                if is_failure_envelope(parsed):
                    code = parsed.get("code")
                    parsed = redact_deep(parsed, self._key)
                    if isinstance(code, str) and code in REJECT_CODES:
                        parsed["code"] = code
                elif "error" in parsed:
                    parsed["error"] = redact_deep(parsed["error"], self._key)
                return parsed
            except urllib.error.HTTPError as exc:
                failure = self._http_failure(exc)
                # Retry once only on a transient 429 (per-key write rate limit) or a
                # write_conflict (the write met concurrent writes and wrote nothing);
                # permanent rejection codes are never repeated.
                if (
                    (exc.code == 429 or failure.code == "write_conflict")
                    and not failure.permanent
                    and attempt == 0
                    and _monotonic() + RATE_RETRY_WAIT_S < end
                ):
                    _sleep(RATE_RETRY_WAIT_S)
                    t = end - _monotonic()
                    last_error = failure
                    continue
            except (urllib.error.URLError, TimeoutError, OSError, http.client.HTTPException) as exc:
                # ``http.client.HTTPException`` (e.g. ``BadStatusLine`` from a proxy's non-HTTP
                # status line, which can reflect a header value) is no OSError.
                failure = CtxApiError(f"connection error: {self._redact(exc)}")
            except ValueError:
                # http.client rejects a header value with control characters (inner CR/LF) via
                # ValueError carrying the bytes repr of the key, and non-latin-1 header or
                # non-ASCII URL text via UnicodeEncodeError (a ValueError); socket rejects a
                # negative timeout the same way. The repr escapes defeat exact-match
                # redaction, so the message is fixed and names no value.
                failure = CtxApiError(
                    "invalid request: key, URL or timeout rejected before any connection "
                    "(control or non-latin-1 characters in the key or URL, or a negative timeout)"
                )
            # Raised after the handlers, not in them: there the error would keep the original
            # HTTPError/URLError — its text and body unredacted — as its implicit ``__context__``,
            # which ``from None`` only hides from printing (#304); its content is in the message.
            if failure is not None:
                raise failure from None

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
