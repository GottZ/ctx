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

import functools
import http.client
import io
import json
import logging
import socket
import time
import types
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


# The most bytes of one answer's body the client reads (#566); a server or a proxy that sends more
# ends the call as an error instead of growing the process. Measured against ctx's handlers: a
# retrieval query at its 20-source clamp at most 0.26 MB, type-list 8 KB; a block from one write at
# the 1 MiB request cap, HTML-escaped on the way out (six bytes per character), 6.3 MB in a get or a
# store answer; a search at the plugin's largest limit (10) with every row carrying such a block's
# tags 59.8 MB. Only a block whose metadata was merged from more than about ten such writes (each
# adds 6 MB to its answers) reads larger.
MAX_ANSWER_BYTES = 64 * 1024 * 1024
# The size of each read of a body.
READ_CHUNK_BYTES = 64 * 1024
# The least share of the time left that one address of a host gets for its connect when the host has
# several (the last one always gets all that is left): a connect over a slow route still fits in it.
CONNECT_SHARE_MIN_S = 0.1


class _AnswerTooLarge(http.client.HTTPException):
    """The body passed :data:`MAX_ANSWER_BYTES`; nothing more of it is read."""


def _time_left(deadline: float) -> float:
    """Seconds until ``deadline`` (``time.monotonic``); ``TimeoutError`` once it has passed — the
    error a socket raises at its timeout, so every caller reads both alike."""
    left = deadline - time.monotonic()
    if left <= 0:
        raise TimeoutError("timed out")
    return left


class _DeadlineReader(io.RawIOBase):
    """The raw stream under an answer's buffer: each read waits at most until the deadline. A socket
    timeout bounds one read; set to what is left before every read, it bounds them all together."""

    def __init__(self, sock: socket.socket, deadline: float) -> None:
        super().__init__()
        self._sock = sock
        self._raw = sock.makefile("rb", buffering=0)
        self._deadline = deadline

    def readable(self) -> bool:
        return True

    def readinto(self, buffer: Any) -> int | None:
        self._sock.settimeout(_time_left(self._deadline))
        return self._raw.readinto(buffer)

    def fileno(self) -> int:
        return self._raw.fileno()

    def close(self) -> None:
        try:
            self._raw.close()
        finally:
            super().close()


class _DeadlineResponse(http.client.HTTPResponse):
    """An answer read against the request's deadline — status line, headers and body — whose whole
    body (``read()``) is read in pieces of :data:`READ_CHUNK_BYTES`, at most :data:`MAX_ANSWER_BYTES`."""

    def __init__(self, sock: socket.socket, *args: Any, deadline: float, **kwargs: Any) -> None:
        # HTTPResponse takes nothing from the socket but its reading file.
        reads = types.SimpleNamespace(makefile=lambda *_: io.BufferedReader(_DeadlineReader(sock, deadline)))
        super().__init__(reads, *args, **kwargs)

    def read(self, amt: int | None = None) -> bytes:
        if amt is not None and amt >= 0:
            return super().read(amt)
        if self.length is not None and self.length > MAX_ANSWER_BYTES:
            raise _AnswerTooLarge(f"answer larger than {MAX_ANSWER_BYTES} bytes, the most the client reads")
        parts: list[bytes] = []
        size = 0
        while chunk := super().read(READ_CHUNK_BYTES):
            size += len(chunk)
            if size > MAX_ANSWER_BYTES:
                raise _AnswerTooLarge(f"answer larger than {MAX_ANSWER_BYTES} bytes, the most the client reads")
            parts.append(chunk)
        if self.length:
            # The connection ended before the announced length: a piece read stays silent about it
            # (http.client), the whole read raised — so does this one.
            raise http.client.IncompleteRead(b"".join(parts), self.length)
        return b"".join(parts)


class _DeadlineConnection:
    """An ``http.client`` connection whose whole exchange ends within the ``timeout`` it was made with
    (#566): every address of the connect and every read of the answer wait at most for what is left of
    it, and the request goes out within what was left when the connection stood (one send call is
    bounded by the socket's timeout as a whole). ``http.client`` hands the timeout to each socket
    operation on its own, so an answer sent a byte at a time held a call for as long as it kept coming."""

    def __init__(self, *args: Any, **kwargs: Any) -> None:
        super().__init__(*args, **kwargs)
        if self.timeout < 0:
            raise ValueError("Timeout value must be non-negative")  # what socket.settimeout says
        self._deadline = time.monotonic() + self.timeout
        self._create_connection = self._connect_by_deadline
        self.response_class = functools.partial(_DeadlineResponse, deadline=self._deadline)

    def _connect_by_deadline(self, address: tuple, _timeout: Any = None, source_address: Any = None) -> socket.socket:
        """``socket.create_connection`` within the deadline (the stdlib gives every address the whole
        timeout): each address gets an equal share of what is left among the ones not tried yet, at
        least :data:`CONNECT_SHARE_MIN_S` and never more than is left, so an address that never answers
        (an IPv6 address without a route) leaves time for the ones after it, and the last one gets all
        that is left. The host name lookup is not bounded."""
        host, port = address
        errors: list[OSError] = []
        infos = socket.getaddrinfo(host, port, 0, socket.SOCK_STREAM)
        for tried, (family, kind, proto, _name, peer) in enumerate(infos):
            left = _time_left(self._deadline)
            sock = socket.socket(family, kind, proto)
            try:
                sock.settimeout(min(left, max(CONNECT_SHARE_MIN_S, left / (len(infos) - tried))))
                if source_address:
                    sock.bind(source_address)
                sock.connect(peer)
                # What follows before the answer — a TLS handshake, the request — is bounded by the
                # socket's timeout as a whole.
                sock.settimeout(_time_left(self._deadline))
                return sock
            except OSError as exc:
                sock.close()
                errors.append(exc)
            except BaseException:
                sock.close()
                raise
        if errors:
            raise errors[0]
        raise OSError("getaddrinfo returns an empty list")


class _DeadlineHTTPConnection(_DeadlineConnection, http.client.HTTPConnection):
    pass


class _DeadlineHTTPHandler(urllib.request.HTTPHandler):
    def do_open(self, http_class: Any, req: urllib.request.Request, **http_conn_args: Any) -> Any:
        return super().do_open(_DeadlineHTTPConnection, req, **http_conn_args)


_HANDLERS: list[Any] = [_RefuseRedirect, _DeadlineHTTPHandler]
if hasattr(http.client, "HTTPSConnection"):  # Python built with ssl

    class _DeadlineHTTPSConnection(_DeadlineConnection, http.client.HTTPSConnection):
        pass

    class _DeadlineHTTPSHandler(urllib.request.HTTPSHandler):
        def do_open(self, http_class: Any, req: urllib.request.Request, **http_conn_args: Any) -> Any:
            return super().do_open(_DeadlineHTTPSConnection, req, **http_conn_args)

    _HANDLERS.append(_DeadlineHTTPSHandler)

# The default opener's handlers (proxies from the environment, error processing) with the redirect
# handler replaced and the HTTP(S) handlers bounded by the request's timeout as a whole.
_OPENER = urllib.request.build_opener(*_HANDLERS)


# The wait before the one retry of a 429 (per-key write rate limit) or a 409 write_conflict.
# Clock and sleep are seams.
RATE_RETRY_WAIT_S = 1.0
_monotonic = time.monotonic
_sleep = time.sleep


def _urlopen(req: urllib.request.Request, *, timeout: float) -> Any:
    """``urllib.request.urlopen`` without following redirects (see ``_RefuseRedirect``), the whole
    exchange within ``timeout`` seconds: the connect, the request, the answer's status line and
    headers, and its body through ``read()`` — at most :data:`MAX_ANSWER_BYTES` of it (#566)."""
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
        # The server registry's type names (``query_types``), read once per client.
        self._type_names: frozenset[str] | None = None

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
        # The timeout bounds the whole request — connect, request, the answer's head and body
        # (``_urlopen``, #566), the one 429 retry and the wait before it included — so a caller's
        # deadline (a checkpoint passes the time it leaves) holds for all of it.
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
        timeout: float | None = None,
    ) -> dict[str, Any]:
        """``timeout``: the call's bound (the recall passes what Hermes' wait for the prefetch leaves);
        default ``synthesis_timeout`` for a synthesis, else the client's timeout."""
        payload: dict[str, Any] = {"query": text, "synthesize": synthesize}
        if include_content:
            payload["include_content"] = True
        if types_exclude:
            payload["types_exclude"] = list(types_exclude)
        if timeout is None and synthesize:
            timeout = self._synthesis_timeout
        return self._request("POST", "/api/query", payload, timeout=timeout)

    def query_types(self, text: str, types: list[str], *, limit: int, timeout: float | None = None) -> dict[str, Any]:
        """Retrieval-only ``/api/query`` that sees only blocks of ``types`` (GottZ/ctx#289): the REST query
        takes no positive type filter, so the other type names of the server's registry (``type-list``,
        read once per client) go into ``types_exclude`` — what the MCP query tool does with its ``types``.
        A type the registry does not list answers no sources, without a query."""
        end = _monotonic() + timeout if timeout is not None else None
        if self._type_names is None:
            listing = self._request("POST", "/api/manage", {"action": "type-list"}, timeout=timeout)
            rows = listing.get("types") if isinstance(listing, dict) else None
            if not isinstance(rows, list):
                raise CtxApiError("type-list returned no types")
            self._type_names = frozenset(str(r.get("name")) for r in rows if isinstance(r, dict) and r.get("name"))
        if not set(types) <= self._type_names:
            return {"success": True, "answer": "", "sources": []}
        payload: dict[str, Any] = {
            "query": text,
            "synthesize": False,
            "include_content": True,
            "limit": int(limit),
            "types_exclude": sorted(self._type_names - set(types)),
        }
        left = None if end is None else end - _monotonic()
        if left is not None and left <= 0:
            raise CtxApiError("query: no time left after the type list", code="timeout")
        return self._request("POST", "/api/query", payload, timeout=left)

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
        # "Missing required field: id". Fall back for older server versions — within the one bound
        # of the call (#566): the second form gets what the first left.
        end = _monotonic() + (timeout if timeout is not None else self._timeout)
        payload: dict[str, Any] = {"action": "get", "id": block_id}
        try:
            return self._request("POST", "/api/manage", payload, timeout=timeout)
        except CtxApiError as exc:
            if "Missing required field: id" not in str(exc):
                raise
        left = end - _monotonic()
        if left <= 0:
            raise CtxApiError("connection error: timed out")  # what the bound ends a call with
        return self._request("POST", "/api/manage", {"action": "get", "data": {"id": block_id}}, timeout=left)
