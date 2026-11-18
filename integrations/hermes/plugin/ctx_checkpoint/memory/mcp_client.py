"""ctx client over Hermes' own MCP dispatch — the credential-free transport.

Same method surface as :class:`client.CtxClient` (``whoami``, ``query``, ``search``,
``store``, ``get_block``) and the same response dictionaries, so the provider, the chain
writer and the agent tools run unchanged on either transport. The calls go through
``tools.registry.registry.dispatch("mcp__<server>__<tool>", args)`` under the name Hermes
registers (:func:`mcp_tool_name`): Hermes talks to the ctx MCP server configured in
``mcp_servers.<server>`` and the plugin never sees a key.

What the ctx MCP tools answer (``go/internal/handler/mcp.go``) and how it is mapped:

- ``search`` → JSON list of block previews (``id``, ``title``, ``category``, ``type``,
  ``untrusted``, ``content``/``content_preview``, ``score`` …) → ``{"success", "results",
  "count"}``. ``types`` is sent (0.16.0, GottZ/ctx#342): a ctx server with #342 takes every
  registered type there, ``checkpoint`` included. An older one refuses a type that retrieval never
  ranks — ``block type "checkpoint" is not retrieval-visible`` (ctx v5.8.0 to v5.52.x) — or, before
  v5.8.0, the argument itself (``unexpected additional properties ["types"]``). On exactly that
  refusal the call is repeated without ``types`` and the rows are narrowed here to the asked types
  (a row without a ``type`` is kept: such a server names none); any other error is raised. The
  answer is remembered per client, so an older server is asked once; a narrowed answer without any
  row of the asked types asks again, so a server updated meanwhile — one that may leave those types
  out of a search without ``types`` — is not read through the fallback.
- ``query`` → prose answer followed by ``Sources:`` lines ``[N] title (category) id:uuid``,
  ``key:value`` tokens after the id kept (`` superseded_by:<uuid>`` is read), and an indented
  ``untrusted_origin:<type> (…)`` line under a source with that record (ctx v5.41.0, #280), read
  into the source as ``untrusted_origin``. A retrieval-only ``query(synthesize=False)`` calls the
  tool with ``synthesize: false`` (and ``include_content: true`` when content is asked for): the
  semantic ``/api/query`` retrieval without the synthesis step, as on ``transport: rest``. The
  tool then answers one JSON document ``{"confidence", "sources": [{id, title, category, score,
  superseded_by?, untrusted?, untrusted_origin?, content?}]}`` (ctx v5.36.0 or newer,
  GottZ/ctx#183), mapped to ``{"success", "answer": "", "confidence", "sources"}``. A server older
  than v5.36.0 refuses the two arguments at schema validation (``unexpected additional properties ["synthesize" …]``);
  this client then serves that call and every later one by ``search`` (lexical hybrid, no
  embedding). Only one call asks: retrieval-only calls made while it is in flight use
  ``search`` as well, so the server sees one refused call per client (a new client, bound
  when the provider connects again, asks once more). Any other error, and an answer that is
  not the sources document, is raised and leaves the question open for the next call.
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
import hashlib
import json
import logging
import re
import threading
import time
from collections.abc import Callable, Iterable
from typing import Any

from .client import CtxApiError
from .config import ORIGIN_NOTE_PREFIX, UNTRUSTED_ORIGIN_FIELD, untrusted_origin, with_untrusted_origin_checked

logger = logging.getLogger(__name__)

_UUID_RE = re.compile(r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b")
_ANSWER_ID_RE = re.compile(r"\(id:\s*(" + _UUID_RE.pattern + r")")
_PAYLOAD_HASH_RE = re.compile(r"payload_hash:\s*([0-9a-fA-F]{16,128})")
# A source line of the query text: `[N] title (category) id:<uuid>`, then `key:value` tokens the
# server adds (` superseded_by:<uuid>`, #197) — kept whatever they are, read where known (#295).
_SOURCE_LINE_RE = re.compile(
    r"^\[(\d+|n/a)\]\s+(.*?)\s+\(([^()]*)\)\s+id:([0-9a-fA-F-]{36})((?:\s+[a-z0-9_]+:\S+)*)\s*$"
)
# The keys of a source line's tail the plugin reads into the source.
_SOURCE_LINE_KEYS = ("superseded_by",)
_SERVER_NAME_RE = re.compile(r"[A-Za-z0-9_-]+")
# Hermes' MCP tool names (``tools/mcp_tool_schema.py``): the characters a name part keeps, the
# provider limit a generated name is shortened to and the hash digits that end a shortened name.
_NAME_PART_RE = re.compile(r"[^A-Za-z0-9_]")
MCP_TOOL_NAME_MAX_CHARS = 64
MCP_TOOL_NAME_HASH_CHARS = 8
_RATE_LIMIT_MARKERS = ("rate limit", "rate_limit")
_MAX_CONTENT_FETCH = 10  # query(include_content=True) over MCP: one get per source, capped
# The go-sdk's schema validation refusal of an argument the tool does not declare.
_UNKNOWN_ARGS_MARKER = "unexpected additional properties"
_UNKNOWN, _PROBING, _YES, _NO = "unknown", "probing", "yes", "no"  # McpClient._query_retrieval_state
_RETRIEVAL_ARGS_RE = re.compile(r"\b(?:synthesize|include_content)\b")
# A ctx server before GottZ/ctx#342 refusing a ``types`` filter that names a type retrieval never
# ranks (``handler/mcp.go`` ``resolveMCPVisibleTypes``, v5.8.0 to v5.52.x). The name is read between
# the words, not with its quotes, for the same reason as :func:`_refuses_retrieval_args`.
_TYPE_NOT_VISIBLE_RE = re.compile(r"\bblock type\W+([A-Za-z0-9_.:-]+)\W+is not retrieval-visible\b")
_TYPES_ARG_RE = re.compile(r"\btypes\b")


def _refuses_types(text: str, types: Iterable[str]) -> bool:
    """True when a search tool error is an older server's refusal of the ``types`` sent: a type the
    filter names is not retrieval-visible there, or the server does not know the argument."""
    match = _TYPE_NOT_VISIBLE_RE.search(text)
    if match is not None:
        return match.group(1) in set(types)
    return _UNKNOWN_ARGS_MARKER in text.lower() and _TYPES_ARG_RE.search(text) is not None


def _refuses_retrieval_args(text: str) -> bool:
    """True when a query tool error says the server does not know ``synthesize``/``include_content``.

    The names are matched as words, not with their quotes: a transport that escapes the
    go-sdk's ``["synthesize" "include_content"]`` once more must not defeat the fallback.
    """
    return _UNKNOWN_ARGS_MARKER in text.lower() and _RETRIEVAL_ARGS_RE.search(text) is not None


class _RetrievalArgsRefused(CtxApiError):
    """The server refused ``synthesize``/``include_content`` (a server from before GottZ/ctx#183).
    Told apart on the text as it arrived, before redaction: a failing redactor withholds the text
    and must not cost the fallback to ``search``."""


class _TypesRefused(CtxApiError):
    """The server refused the ``types`` of a search (a server from before GottZ/ctx#342). Told
    apart on the text as it arrived, before redaction, like :class:`_RetrievalArgsRefused`."""


def _refusal_kind(tool: str, args: dict[str, Any], text: str) -> type[CtxApiError]:
    """The error class for a tool error ``text``: a refusal this client falls back from, or a plain
    :class:`CtxApiError`. Only the refusal of exactly the ``types`` this search sent counts."""
    types = args.get("types")
    if tool == "search" and types and _refuses_types(text, types):
        return _TypesRefused
    if _refuses_retrieval_args(text):
        return _RetrievalArgsRefused
    return CtxApiError


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


def _tool_name_part(value: str) -> str:
    return _NAME_PART_RE.sub("_", str(value or ""))


def mcp_tool_name(server: str, tool: str) -> str:
    """The name Hermes registers ``tool`` of the MCP server entry ``server`` under, which the
    client dispatches and the plugin tells the model (system prompt block, receipt).

    Hermes' rule (``tools.mcp_tool_schema.mcp_prefixed_tool_name``, not a public plugin API, so
    it is followed here and pinned against Hermes' function by the tests): every character
    outside ``[A-Za-z0-9_]`` becomes ``_`` (``ctx-prod`` → ``mcp__ctx_prod__store``), and a name
    over :data:`MCP_TOOL_NAME_MAX_CHARS` keeps its start and ends in ``_`` plus the first
    :data:`MCP_TOOL_NAME_HASH_CHARS` hex digits of its SHA-256."""
    full = f"mcp__{_tool_name_part(server)}__{_tool_name_part(tool)}"
    if len(full) <= MCP_TOOL_NAME_MAX_CHARS:
        return full
    suffix = "_" + hashlib.sha256(full.encode("utf-8")).hexdigest()[:MCP_TOOL_NAME_HASH_CHARS]
    return full[: MCP_TOOL_NAME_MAX_CHARS - len(suffix)] + suffix


def colliding_servers(server: str, servers: Any) -> list[str]:
    """The other entries of ``mcp_servers`` (the mapping) that can register a tool under the name
    of one of ``server``'s checkpoint tools, entries with ``enabled: false`` left out. Hermes keeps
    the first registration of a name and skips the other, so a call by name could reach the other
    server: the plugin refuses such a binding.

    Hermes joins the parts as ``mcp__<server>__<tool>`` and merges no underscores, and config.yaml
    does not list another server's tools, so the rule takes every tool name as possible: an entry
    ``other`` collides when ``mcp__<own>__<tool>`` starts with ``mcp__<other>__``. The checkpoint
    tools (``search``, ``get``, ``store``, ``query``) hold no ``_``, so that is exactly when
    ``<own>__`` starts with ``<other>__``: the same part, or the other's part followed by ``_``
    (``c-`` next to ``c``: ``mcp__c___store`` is the name of a tool ``_store`` of ``c``) or by ``__``
    (``ctx--prod`` next to ``ctx``: a tool ``prod__store`` of ``ctx``). A name with one separator
    (``ctx-prod`` next to ``ctx``) never collides; the entry ``ctx`` next to ``ctx--prod`` neither."""
    entries = servers.items() if isinstance(servers, dict) else ()
    own = _tool_name_part(server) + "__"
    return sorted(
        str(name)
        for name, entry in entries
        if str(name) != server
        and own.startswith(_tool_name_part(str(name)) + "__")
        and not (isinstance(entry, dict) and entry.get("enabled") is False)
    )


def collision_text(server: str, others: list[str]) -> str:
    """Why the entry ``server`` is not used while ``others`` collide with it."""
    entries = ", ".join(f"mcp_servers.{name}" for name in others)
    return (
        f"mcp_servers.{server} and {entries} can register tools under the same names "
        f"({mcp_tool_name(server, 'store')} …): Hermes keeps one registration of a name, so a checkpoint "
        "could reach the other server; rename one of the entries so that, with every character outside "
        "A-Za-z0-9_ turned into _, neither name equals the other or starts with it followed by _"
    )


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
        secrets: Callable[[], Iterable[str]] | None = None,
    ):
        if not _SERVER_NAME_RE.fullmatch(server_name or ""):
            raise ValueError(f"MCP server name {server_name!r} may only contain letters, digits, '_' and '-'")
        self._dispatch = dispatch
        self._server = server_name
        # The ctx keys the provider binds (read per error): foreign error texts are redacted with
        # them where they enter the plugin (#304). The client itself holds no credential.
        self._secrets = secrets
        # Informational defaults: only an explicit per-call ``timeout`` (the chain writer's
        # remaining budget) bounds a dispatch; otherwise the MCP transport owns its timeouts.
        self._timeout = timeout
        self._synthesis_timeout = synthesis_timeout
        # Does the server's query tool take synthesize:false (GottZ/ctx #183)? "unknown" until
        # the first retrieval-only call answers, "probing" while that one call is in flight
        # (concurrent calls meanwhile use search, so a server from before #183 is refused
        # once, not once per overlapping call), then "yes" or "no" ("no" = search from then on).
        self._query_retrieval_state = _UNKNOWN
        self._query_retrieval_lock = threading.Lock()
        # Does the server's search tool take the ``types`` this client sends (GottZ/ctx#342)?
        # "unknown" until a search with ``types`` answers, then "yes" or "no" ("no" = search without
        # ``types`` and narrow here; asked again when such an answer holds none of the asked types).
        self._search_types_state = _UNKNOWN
        self._search_types_lock = threading.Lock()

    @property
    def server_name(self) -> str:
        return self._server

    # -- transport ----------------------------------------------------------------

    def _redact(self, text: Any) -> str:
        """A foreign error text (a server answer, a dispatch error) as it may leave this client:
        ``checkpoint.redact_error_text`` with the bound keys. Never raises."""
        from .checkpoint import redact_error_text

        try:
            secrets = tuple(self._secrets()) if self._secrets is not None else ()
        except Exception:
            secrets = ()
        return redact_error_text(text, secrets)

    def _call(self, tool: str, args: dict[str, Any], timeout: float | None = None) -> tuple[Any, str]:
        """Dispatch one tool; returns ``(payload, text)``. Raises ``CtxApiError`` on an error envelope.

        With ``timeout`` the dispatch is bounded (see the module docstring) and an
        abandoned call raises ``CtxApiError(code="timeout")``.
        """
        name = mcp_tool_name(self._server, tool)
        failure: CtxApiError | None = None
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
            kind = _refusal_kind(tool, args, str(exc))
            failure = kind(f"mcp dispatch failed: {type(exc).__name__}: {self._redact(exc)}")
        if failure is not None:
            # Raised after the handler and without the original: its text is unredacted, and raised
            # in the handler it would stay the implicit ``__context__`` (#304).
            raise failure
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
                kind = _refusal_kind(tool, args, text)
                raise kind(
                    f"mcp {tool}: {self._redact(text)[:300]}", status=429 if code == "rate_limit" else None, code=code
                )
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
        wanted = list(dict.fromkeys(str(t) for t in types or () if t))
        if not wanted:
            return self._search_rows(*self._call("search", args, timeout))
        deadline = None if timeout is None else time.monotonic() + timeout
        with self._search_types_lock:
            state = self._search_types_state
        if state != _NO:
            rows = self._search_with_types(args, wanted, timeout)
            if rows is not None:
                return rows
        data = self._search_rows(*self._call("search", args, self._remaining(deadline)), only_types=wanted)
        if state == _NO and not data["results"]:
            # Nothing of the asked types without the filter: a server updated since the refusal may
            # leave those types out of a search that names none, so it is asked again.
            rows = self._search_with_types(args, wanted, self._remaining(deadline))
            if rows is not None:
                return rows
        return data

    def _search_with_types(
        self, args: dict[str, Any], wanted: list[str], timeout: float | None
    ) -> dict[str, Any] | None:
        """The search with ``types``; ``None`` (and remembered) when the server refuses them."""
        try:
            payload, body = self._call("search", {**args, "types": wanted}, timeout)
        except _TypesRefused:
            with self._search_types_lock:
                if self._search_types_state != _NO:
                    logger.info(
                        "ctx MCP server %r refuses search types %s (before GottZ/ctx#342); searching without "
                        "types and narrowing the rows",
                        self._server,
                        wanted,
                    )
                self._search_types_state = _NO
            return None
        with self._search_types_lock:
            self._search_types_state = _YES
        return self._search_rows(payload, body)

    @staticmethod
    def _remaining(deadline: float | None) -> float | None:
        """What is left of a call's ``timeout`` for its second dispatch; raises when nothing is."""
        if deadline is None:
            return None
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise CtxApiError("mcp search: no time left for the search without types", code="timeout")
        return remaining

    def _search_rows(self, payload: Any, body: str, only_types: list[str] | None = None) -> dict[str, Any]:
        """The search answer as ``{"success", "results", "count"}``. With ``only_types``: rows of other
        types are dropped (a row without a ``type`` is kept)."""
        results = self._as_json(body)
        if results is None and isinstance(payload, list):
            results = payload
        if isinstance(results, dict) and isinstance(results.get("results"), list):
            results = results["results"]
        if not isinstance(results, list):
            results = []
        rows = [r for r in results if isinstance(r, dict)]
        if only_types:
            rows = [r for r in rows if not r.get("type") or r.get("type") in only_types]
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
            data = self._retrieval_via_query(text, include_content, types_exclude, limit)
            if data is not None:
                return data
            # A server from before #183 (or the first call on it still in flight): the lexical
            # hybrid search is the no-LLM path. Its compact rows carry type/untrusted and
            # content_preview, not content, so the recall on this path shows titles only.
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
                    block = with_untrusted_origin_checked(self.get_block(source["id"]).get("block") or {})
                except CtxApiError as exc:
                    logger.debug("mcp query: content fetch for %s failed: %s", source["id"], self._redact(exc))
                    continue
                for key in ("content", "type", "untrusted", UNTRUSTED_ORIGIN_FIELD, "score", "tags"):
                    if key in block and key not in source:
                        source[key] = block[key]
        return {"success": True, "answer": answer, "sources": sources}

    def _retrieval_via_query(
        self,
        text: str,
        include_content: bool,
        types_exclude: list[str] | None,
        limit: int | None,
    ) -> dict[str, Any] | None:
        """Retrieval-only ``query``, or ``None`` when this call is to use ``search``.

        ``None``: the server refused the arguments (now or earlier), or the first call is
        still in flight. Any other error is raised, as the REST transport raises it, and
        leaves the question open for the next call.
        """
        with self._query_retrieval_lock:
            state = self._query_retrieval_state
            if state in (_PROBING, _NO):
                return None
            if state == _UNKNOWN:
                self._query_retrieval_state = _PROBING
        try:
            data = self._query_without_synthesis(text, include_content, types_exclude, limit)
        except BaseException:
            with self._query_retrieval_lock:
                if self._query_retrieval_state == _PROBING:
                    self._query_retrieval_state = _UNKNOWN
            raise
        with self._query_retrieval_lock:
            self._query_retrieval_state = _YES if data is not None else _NO
        return data

    def _query_without_synthesis(
        self,
        text: str,
        include_content: bool,
        types_exclude: list[str] | None,
        limit: int | None,
    ) -> dict[str, Any] | None:
        """``query`` with ``synthesize: false``; ``None`` when the server refuses the arguments."""
        args: dict[str, Any] = {"question": text, "synthesize": False}
        if include_content:
            args["include_content"] = True
        if limit:
            args["limit"] = int(limit)
        if types_exclude:
            args["types_exclude"] = list(types_exclude)
        try:
            _payload, body = self._call("query", args)
        except _RetrievalArgsRefused:
            logger.info(
                "ctx MCP server %r predates query synthesize:false; retrieval-only recall uses search", self._server
            )
            return None
        # ``body`` is the tool's text for a {"result": ...} wrapping and the JSON text of a
        # bare answer (``_call``), so one parse covers every shape Hermes hands back.
        answer = self._as_json(body)
        rows = answer.get("sources") if isinstance(answer, dict) else None
        if not isinstance(answer, dict) or "sources" not in answer or not (rows is None or isinstance(rows, list)):
            # Not the retrieval-only document. Raised, not taken for a refusal: a server
            # from before #183 refuses the arguments (its go-sdk schema has
            # additionalProperties false), so one odd answer must not cost the semantic
            # recall for the rest of the binding.
            # The text is not echoed: it can carry block content.
            raise CtxApiError(f"mcp query: the retrieval-only answer is not a sources document ({len(body)} chars)")
        sources = [row for row in rows or [] if isinstance(row, dict)]
        return {"success": True, "answer": "", "confidence": answer.get("confidence") or "", "sources": sources}

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
            return {"success": False, "error": f"store returned no block id: {self._redact(body)[:200]}"}
        if "No change needed" in body:
            return {"success": True, "action": "noop", "existing_id": block_id, "reason": "identical content"}
        return {"success": True, "block": {"id": block_id, "title": title, "category": category}}

    def get_block(self, block_id: str, timeout: float | None = None) -> dict[str, Any]:
        payload, body = self._call("get", {"id": block_id}, timeout)
        block = self._as_json(body)
        if not isinstance(block, dict) and isinstance(payload, dict) and "result" not in payload:
            block = payload
        if not isinstance(block, dict):
            return {"success": False, "error": f"get returned no block: {self._redact(body)[:200]}"}
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
    """Split the MCP query text into ``(answer, sources)``. A source line keeps whatever ``key:value``
    tokens follow its id (``superseded_by`` is read); an indented ``untrusted_origin:<type> (…)`` line
    right under it (``store.UntrustedOriginNote``, #280) is the source's record."""
    answer, _, tail = body.partition("\n\nSources:\n")
    sources: list[dict[str, Any]] = []
    previous: dict[str, Any] | None = None
    # Split at "\n" only: the server ends a line with it, and splitlines() would also split a title
    # at \r, \x1c-\x1e, \x85 or U+2028/U+2029 and let its second half pass for a source line.
    for line in tail.split("\n"):
        stripped = line.strip()
        if previous is not None and line[:1].isspace() and stripped.startswith(ORIGIN_NOTE_PREFIX):
            value = stripped[len(ORIGIN_NOTE_PREFIX) :].split(" ", 1)[0]
            previous[UNTRUSTED_ORIGIN_FIELD] = untrusted_origin({UNTRUSTED_ORIGIN_FIELD: value})
            previous = None
            continue
        previous = None
        match = _SOURCE_LINE_RE.match(stripped)
        if not match:
            continue
        entry: dict[str, Any] = {"id": match.group(4), "title": match.group(2), "category": match.group(3)}
        if match.group(1) != "n/a":
            entry["citation_index"] = int(match.group(1))
        for token in match.group(5).split():
            key, _, value = token.partition(":")
            if key in _SOURCE_LINE_KEYS and value:
                entry[key] = value
        sources.append(entry)
        previous = entry
    return answer.strip(), sources
