"""Optional ctx records of every handover side through the ledger outbox (layer L3, stdlib only; DESIGN §13,
BRIEF D12, BX M1).

ctx is never in the critical path. A process that finishes a job side (source retire, target commit — also
``hermes serve`` and an offline laptop) calls ``enqueue``: the redacted record goes into the ledger table
``ctx_outbox``. A process that holds a plugin ``ctx`` (gateway, CLI) calls ``flush`` on first use, before each
job and on ``/handover status`` — on a thread of its own, never in the caller's way — and keeps to a total budget
(``FLUSH_BUDGET_S``) wherever it runs; rows that fail stay in the outbox with the ledger's backoff, rows the budget
did not reach stay due. Each row is claimed in the ledger before its first MCP call (``ledger.outbox_claim``), so
flushes running at once — the janitor's and ``ctxh-outbox``, or two processes of the profile — never send one row
twice. Neither function raises: a failure is logged (once per row at warning level) and shown by
``/handover status``.

ctx is reached only through Hermes' MCP dispatch (``ctx.call_mcp``), which needs the operator's
``plugins.entries.ctx_handover.mcp_allowlist`` entry; ``/handover setup`` asks for it.
"""

from __future__ import annotations

import hashlib
import json
import logging
import re
import threading
import time
from collections.abc import Callable, Mapping
from datetime import datetime, timezone
from typing import Any

from . import settings as settings_rules

logger = logging.getLogger(__name__)

PROVIDER = "ctx_checkpoint"
DEFAULT_SERVER = "ctx"
KINDS = {"departure": "out", "arrival": "in"}
RECORD_MAX_BYTES = 8 * 1024
CALL_TIMEOUT_S = 10.0
SEARCH_TIMEOUT_S = 10.0
FLUSH_BUDGET_S = 2.0
"""Total time one ``flush`` may spend on MCP calls (M29; DESIGN §13 "ctx is never in the critical path"): no call
starts once less than ``MIN_CALL_S`` is left, and each call's timeout is what is left of the budget. The janitor's
flush before each job keeps to it too."""
MIN_CALL_S = 1.0
"""Hermes clamps an MCP timeout to at least one second (``PluginContext.call_mcp``)."""
CLAIM_S = 120.0
"""How long a flush holds a row it is sending (``ledger.outbox_claim``; far beyond one row's calls, which the budget
bounds): no other flusher sends it meanwhile. The claimer ends the claim itself (delivered, backed off, or handed
back untried); one that dies leaves the row due again after this."""
HEAD_TITLE = "Compaction checkpoint head {root}"
"""ctx_checkpoint's stable head title per root session (its README "Recall of a checkpoint"); ``{root}`` is the
root's title token (``head_token``)."""
ROOT_TOKEN_MAX = 48
CHECKPOINT_TYPE = "checkpoint"
HEAD_TAG = "checkpoint-head"
"""The tag every ctx_checkpoint head carries (its ``HEAD_TAGS``); the head search names it (GottZ/ctx#342)."""
"""The type ctx_checkpoint stores its heads under. The head search names it (GottZ/ctx#342): ctx may leave types that
retrieval never ranks out of a search that names no type."""
_TYPE_NOT_VISIBLE_RE = re.compile(r"\bblock type\W+([A-Za-z0-9_.:-]+)\W+is not retrieval-visible\b")
_UNKNOWN_ARGS_MARKER = "unexpected additional properties"
_TYPES_ARG_RE = re.compile(r"\btypes\b")
_types_refused: dict[str, bool] = {}
"""MCP servers (by ``mcp_servers`` name) that refused the head search's ``types`` in this process: a ctx server before
GottZ/ctx#342. They are searched without it until such a search finds no head (see :func:`_checkpoint_head`)."""
_types_refused_lock = threading.Lock()


def head_token(root: str) -> str:
    """The token ctx_checkpoint (0.6.1 and later, ``checkpoint._short_root``) puts into a root's head title: the id
    itself when it is made of ``[A-Za-z0-9_.-]`` and at most 48 characters long, else the sanitized id cut to fit and
    ``-`` plus the first 8 hex digits of the full id's SHA-256 — Hermes' 70-character ``local-<sha256>`` ids too
    (review R6-B: the raw id found no head for those)."""
    safe = re.sub(r"[^A-Za-z0-9_.-]", "_", root)
    if safe == root and len(safe) <= ROOT_TOKEN_MAX:
        return safe
    suffix = hashlib.sha256(root.encode("utf-8", "surrogatepass")).hexdigest()[:8]
    return f"{safe[: ROOT_TOKEN_MAX - len(suffix) - 1]}-{suffix}"


_SERVER_RE = re.compile(r"^[A-Za-z0-9_-]{1,64}$")
_TAG_RE = re.compile(r"[^A-Za-z0-9_.@:-]")
_PATH_LINES = 20


def detect(config: dict) -> str | None:
    """The MCP server name of ctx when ``memory.provider`` is ``ctx_checkpoint`` (``memory.ctx_checkpoint
    .mcp_server``, default ``ctx``), else None. ``ctx.has_plugin`` is no signal: it is False for exclusive memory
    providers (``hermes_cli/plugins.py:545-563``)."""
    if not isinstance(config, Mapping):
        return None
    memory = config.get("memory")
    if not isinstance(memory, Mapping) or memory.get("provider") != PROVIDER:
        return None
    section = memory.get(PROVIDER)
    server = section.get("mcp_server") if isinstance(section, Mapping) else None
    server = DEFAULT_SERVER if server in (None, "") else server
    return server if isinstance(server, str) and _SERVER_RE.fullmatch(server) else None


def allowed(config: dict, server: str) -> bool:
    """True when the operator granted this plugin the MCP server (``plugins.entries.ctx_handover.mcp_allowlist``,
    written by ``/handover setup``). Without it ``ctx.call_mcp`` raises ``PermissionError``, so nothing is sent."""
    plugins = config.get("plugins") if isinstance(config, Mapping) else None
    entries = plugins.get("entries") if isinstance(plugins, Mapping) else None
    entry = entries.get("ctx_handover") if isinstance(entries, Mapping) else None
    allow = entry.get("mcp_allowlist") if isinstance(entry, Mapping) else None
    return isinstance(allow, list) and server in allow


def persist_off(settings: Mapping[str, Any] | None) -> bool:
    """``ctx_persist`` says off, read by K0's one rule (``settings.coerce``, V22) — also as the raw YAML value: an
    unquoted ``off`` in ``config.yaml`` arrives as ``False`` (YAML 1.1) and means off; ``None`` or an unknown word is
    the default ``auto``."""
    value = settings.get("ctx_persist") if isinstance(settings, Mapping) else None
    return settings_rules.coerce("ctx_persist", value) == "off"


def server_for(config: dict, settings: Mapping[str, Any]) -> str | None:
    """The ctx server records go to, or None: ctx_checkpoint detected and ``ctx_persist`` not off (``persist_off``;
    callers may pass the raw settings mapping). Callers that finish a job side enqueue only when this is not None
    (the outbox then waits for consent and connectivity)."""
    if persist_off(settings):
        return None
    return detect(config)


def _get(obj: Any, key: str, default: Any = None) -> Any:
    if isinstance(obj, Mapping):
        return obj.get(key, default)
    return getattr(obj, key, default)


def _tag_value(value: object) -> str:
    return _TAG_RE.sub("-", str(value or "unknown"))[:64] or "unknown"


def _iso(ts: object) -> str:
    try:
        return datetime.fromtimestamp(float(ts), tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")  # type: ignore[arg-type]
    except (TypeError, ValueError, OverflowError, OSError):
        return ""


def _model_end(end: object) -> str:
    """``model (provider)`` of one side of a contract-form ``model_change`` (X04); anything else is unknown."""
    if not isinstance(end, Mapping):
        return "unknown"
    name, provider = end.get("model"), end.get("provider")
    if name and provider:
        return f"{name} ({provider})"
    return str(name or provider or "unknown")


def _cap_utf8(text: str, limit: int) -> str:
    raw = text.encode("utf-8")
    if len(raw) <= limit:
        return text
    marker = "\n… (truncated)"
    return raw[: limit - len(marker.encode("utf-8"))].decode("utf-8", "ignore") + marker


def build_record(kind: str, data: Mapping[str, Any], *, redact: Callable[[str], str]) -> dict:
    """The ctx block for one handover side (DESIGN §13), redacted and capped at ``RECORD_MAX_BYTES``.

    ``data`` keys (dict or attributes): ``job_id``, ``source_root``, ``target_root``, ``source_session``,
    ``target_session``, ``peer`` (the other side's name), ``local_name`` (this side's name), ``title``, ``at``
    (epoch seconds), ``path_map`` (``PathMapEntry`` or dicts), ``model_change`` (``None`` or contract X04's
    ``{"from": {"model", "provider"}, "to": {"model", "provider", "context_length"}, "reason",
    "offline_capable"}``), ``plugin_version``.
    Raises ``ValueError`` for an unknown kind; the redactor's exceptions propagate (``enqueue`` drops the record).
    """
    direction = KINDS.get(kind)
    if direction is None:
        raise ValueError(f"unknown record kind {kind!r}")
    job_id = str(_get(data, "job_id", "") or "")
    source_root = str(_get(data, "source_root", "") or "")
    target_root = str(_get(data, "target_root", "") or "")
    peer = str(_get(data, "peer", "") or "unknown")
    local = str(_get(data, "local_name", "") or "this gateway")
    short = job_id[-6:]
    at = _iso(_get(data, "at"))
    if direction == "out":
        title = f"Handover {source_root} → {peer} {short}"
        peers = {"from": local, "to": peer}
    else:
        title = f"Handover arrival {target_root} ← {peer} {short}"
        peers = {"from": peer, "to": local}
    lines = [
        f"# {title}",
        "",
        f"- Direction: {'departure' if direction == 'out' else 'arrival'} ({peers['from']} → {peers['to']})",
        f"- Job: {job_id}",
        f"- At: {at}",
        f"- Session title: {_get(data, 'title', '') or '(untitled)'}",
        f"- Source session: {_get(data, 'source_session', '') or source_root} (root {source_root})",
        f"- Target session: {_get(data, 'target_session', '') or target_root} (root {target_root})",
    ]
    path_map = list(_get(data, "path_map", ()) or ())
    if path_map:
        lines.append("- Paths:")
        for entry in path_map[:_PATH_LINES]:
            lines.append(f"  - {_get(entry, 'source', '')} → {_get(entry, 'target', '')} ({_get(entry, 'kind', '')})")
        if len(path_map) > _PATH_LINES:
            lines.append(f"  - … {len(path_map) - _PATH_LINES} more")
    change = _get(data, "model_change")
    if isinstance(change, Mapping) and change:
        line = f"- Model: {_model_end(change.get('from'))} → {_model_end(change.get('to'))}"
        if change.get("reason"):
            line += f" ({change['reason']})"
        if isinstance(change.get("offline_capable"), bool):
            line += f"; works offline: {'yes' if change['offline_capable'] else 'no'}"
        lines.append(line)
    content = _cap_utf8(redact("\n".join(lines)), RECORD_MAX_BYTES - 256)
    metadata = {
        "job_id": job_id,
        "source_root": source_root,
        "target_root": target_root,
        "peers": peers,
        "at": at,
        "plugin_version": str(_get(data, "plugin_version", "") or ""),
        "source": "ctx_handover",
    }
    return {
        "title": redact(title)[:300],
        "content": content,
        "tags": ["handover", "hermes", f"peer:{_tag_value(peer)}", f"direction:{direction}"],
        "metadata": metadata,
        "source_root": source_root,
    }


def enqueue(ledger, kind: str, data: dict, *, redact: Callable[[str], str]) -> None:
    """Put the redacted record of one finished side into the outbox. Never raises (DESIGN §13)."""
    try:
        record = build_record(kind, data, redact=redact)
    except Exception:
        logger.warning("ctx_handover: ctx record for job %s not built (dropped)", _get(data, "job_id", "?"))
        return
    try:
        ledger.outbox_put(kind, record)
    except Exception:
        logger.warning("ctx_handover: ctx record for job %s not queued", _get(data, "job_id", "?"), exc_info=True)


def _rows_of(result: Any) -> list[dict]:
    if isinstance(result, str):
        try:
            result = json.loads(result)
        except ValueError:
            return []
    if isinstance(result, Mapping):
        result = result.get("results")
    return [r for r in result if isinstance(r, Mapping)] if isinstance(result, list) else []


def _refuses_types(envelope: Any) -> bool:
    """True when a search envelope is an older ctx server's refusal of ``types=["checkpoint"]``: the type is not
    retrieval-visible there (v5.8.0 to v5.52.x), or the server does not know the argument (before v5.8.0). Any other
    failure is not."""
    if not isinstance(envelope, Mapping) or envelope.get("ok") is True:
        return False
    text = str(envelope.get("error") or "")
    match = _TYPE_NOT_VISIBLE_RE.search(text)
    if match is not None:
        return match.group(1) == CHECKPOINT_TYPE
    return _UNKNOWN_ARGS_MARKER in text.lower() and _TYPES_ARG_RE.search(text) is not None


def _head_search(call_mcp: Callable[..., dict], server: str, args: dict, timeout: float) -> Any:
    try:
        return call_mcp(server, "search", args, timeout)
    except Exception:
        return None


def _head_in(envelope: Any, title: str) -> str | None:
    """The id of the row titled ``title`` (of type checkpoint, when the row names its type) in a search envelope."""
    if not isinstance(envelope, Mapping) or envelope.get("ok") is not True:
        return None
    for row in _rows_of(envelope.get("result")):
        if row.get("title") == title and isinstance(row.get("id"), str) and row.get("type") in (None, CHECKPOINT_TYPE):
            return row["id"]
    return None


def _checkpoint_head(call_mcp: Callable[..., dict], server: str, root: str, timeout: float) -> str | None:
    """The checkpoint head of ``root``, searched by title and head tag with ``types=["checkpoint"]``.

    A server that refuses the type (before GottZ/ctx#342) is searched without it, within the same ``timeout``, and the
    refusal is remembered for that MCP server name in this process. A later search without the type that finds no head
    drops the memory, so the next record's search asks with the type again: a server updated meanwhile may leave
    checkpoints out of a search that names no type. A lookup keeps a memory it has just set, so on a server before #342
    records without a head in a row cost two calls and one call in turn, not two each. After such an update, every
    record whose search still ran without the type goes without the link: the first one; one more per flush that ran
    such a search at the same time (the memory is read under the lock and the search runs outside it, so no flush
    waits for another's network call); and the next one as well when the update fell between a lookup's refused search
    and its search without the type. The search is not repeated within the same record: the flush budget leaves no
    room for a third call before the store."""
    if not root:
        return None
    title = HEAD_TITLE.format(root=head_token(root))
    # Every head carries the tag (ctx_checkpoint HEAD_TAGS): the tag index finds the heads, where the title words alone
    # read the text vectors of every checkpoint part.
    args = {"query": title, "tags": [HEAD_TAG], "limit": 5}
    deadline = time.monotonic() + timeout
    with _types_refused_lock:
        remembered = _types_refused.get(server, False)
    if not remembered:
        envelope = _head_search(call_mcp, server, {**args, "types": [CHECKPOINT_TYPE]}, timeout)
        if not _refuses_types(envelope):
            return _head_in(envelope, title)
        with _types_refused_lock:
            _types_refused[server] = True
        logger.info("ctx_handover: ctx server %s refuses the checkpoint type in search; searching without it", server)
    remaining = deadline - time.monotonic()
    if remaining < MIN_CALL_S:
        return None
    envelope = _head_search(call_mcp, server, args, remaining)
    head = _head_in(envelope, title)
    answered = isinstance(envelope, Mapping) and envelope.get("ok") is True
    if head is None and remembered and answered:
        with _types_refused_lock:
            _types_refused.pop(server, None)
    return head


def flush(
    ledger,
    call_mcp: Callable[[str, str, dict, float], dict],
    server: str,
    category: str,
    now: float,
    *,
    budget_s: float = FLUSH_BUDGET_S,
    clock: Callable[[], float] = time.monotonic,
) -> int:
    """Store due outbox rows in ctx within ``budget_s`` seconds; returns how many were stored. Never raises.

    A failed row is reported back with ``ok=False`` (the ledger backs it off); the first failure of a row is
    logged at warning level, repeats at debug level. A row the budget does not reach (or whose ``store`` it does not
    reach) stays due without a backoff (M29). Each row is claimed before its first call (``ledger.outbox_claim``);
    a row another flusher holds, or one without an id, is not sent.
    """
    deadline = clock() + max(0.0, float(budget_s))
    until = float(now) + CLAIM_S

    def left(cap: float) -> float | None:
        remaining = deadline - clock()
        return min(cap, remaining) if remaining >= MIN_CALL_S else None

    try:
        rows = ledger.outbox_due(now, 20)
    except Exception:
        logger.debug("ctx_handover: outbox not readable", exc_info=True)
        return 0
    stored = 0
    for row in rows:
        row_id = row.get("id") if isinstance(row, Mapping) else None
        if left(SEARCH_TIMEOUT_S) is None:
            break  # the budget is spent: this row and the rest stay due, untried
        if row_id is None or not _claim(ledger, row_id, now, until):
            continue  # another flusher (thread or process) sends it, or it could never be marked done
        try:
            payload = dict(row["payload"])
            metadata = dict(payload.get("metadata") or {})
            content = str(payload.get("content") or "")
            root = str(payload.get("source_root") or "")
            head = _checkpoint_head(call_mcp, server, root, left(SEARCH_TIMEOUT_S) or MIN_CALL_S) if root else None
            store_timeout = left(CALL_TIMEOUT_S)
            if store_timeout is None:
                _release(ledger, row_id, until, row.get("next_try_at", now))
                break  # no time left for the store: the row is handed back due, untried
            if head:
                metadata["checkpoint_head"] = head
                content = _cap_utf8(content + f"\n- Checkpoint head: {head}", RECORD_MAX_BYTES)
            envelope = call_mcp(
                server,
                "store",
                {
                    "category": category,
                    "title": str(payload.get("title") or ""),
                    "content": content,
                    "tags": list(payload.get("tags") or []),
                    "metadata": metadata,
                    "sensitivity": "internal",
                },
                store_timeout,
            )
            ok = isinstance(envelope, Mapping) and envelope.get("ok") is True and not envelope.get("error")
            if not ok:
                raise RuntimeError(str(envelope.get("error") if isinstance(envelope, Mapping) else envelope)[:200])
        except Exception as exc:
            attempts = row.get("attempts", 0) if isinstance(row, Mapping) else 0
            log = logger.warning if not attempts else logger.debug
            log("ctx_handover: ctx record %s not stored (%s); it stays queued", row_id, type(exc).__name__)
            ok = False
        try:
            ledger.outbox_done(row_id, ok, now)
        except Exception:
            logger.debug("ctx_handover: outbox row %s not updated", row_id, exc_info=True)
            continue
        stored += 1 if ok else 0
    return stored


def _claim(ledger, row_id, now: float, until: float) -> bool:
    """``ledger.outbox_claim`` — True only when this flush holds the row now; any failure is no claim."""
    try:
        return ledger.outbox_claim(row_id, now, until) is True
    except Exception:
        logger.debug("ctx_handover: outbox row %s not claimed", row_id, exc_info=True)
        return False


def _release(ledger, row_id, until: float, due_at) -> None:
    """Hand an untried claimed row back (``ledger.outbox_release``); if that fails the claim lapses at ``until``."""
    try:
        ledger.outbox_release(row_id, until, float(due_at))
    except Exception:
        logger.debug("ctx_handover: outbox row %s not handed back", row_id, exc_info=True)


def pending(ledger) -> int:
    """Rows still waiting for ctx (for ``/handover status``); 0 when the outbox is unreadable."""
    try:
        return len(ledger.outbox_due(float("inf"), 1000))
    except Exception:
        return 0
