"""Optional ctx records of every handover side through the ledger outbox (layer L3, stdlib only; DESIGN §13,
BRIEF D12, BX M1).

ctx is never in the critical path. A process that finishes a job side (source retire, target commit — also
``hermes serve`` and an offline laptop) calls ``enqueue``: the redacted record goes into the ledger table
``ctx_outbox``. A process that holds a plugin ``ctx`` (gateway, CLI) calls ``flush`` on first use, before each
job and on ``/handover status``; rows that fail stay in the outbox with the ledger's backoff. Neither function
raises: a failure is logged (once per row at warning level) and shown by ``/handover status``.

ctx is reached only through Hermes' MCP dispatch (``ctx.call_mcp``), which needs the operator's
``plugins.entries.ctx_handover.mcp_allowlist`` entry; ``/handover setup`` asks for it.
"""

from __future__ import annotations

import json
import logging
import re
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
HEAD_TITLE = "Compaction checkpoint head {root}"
"""ctx_checkpoint's stable head title per root session (its README "Recall of a checkpoint")."""

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


def _checkpoint_head(call_mcp: Callable[..., dict], server: str, root: str) -> str | None:
    if not root:
        return None
    title = HEAD_TITLE.format(root=root)
    try:
        envelope = call_mcp(server, "search", {"query": title, "limit": 5}, SEARCH_TIMEOUT_S)
    except Exception:
        return None
    if not isinstance(envelope, Mapping) or envelope.get("ok") is not True:
        return None
    for row in _rows_of(envelope.get("result")):
        if row.get("title") == title and isinstance(row.get("id"), str):
            return row["id"]
    return None


def flush(ledger, call_mcp: Callable[[str, str, dict, float], dict], server: str, category: str, now: float) -> int:
    """Store every due outbox row in ctx; returns how many were stored. Never raises.

    A failed row is reported back with ``ok=False`` (the ledger backs it off); the first failure of a row is
    logged at warning level, repeats at debug level.
    """
    try:
        rows = ledger.outbox_due(now, 20)
    except Exception:
        logger.debug("ctx_handover: outbox not readable", exc_info=True)
        return 0
    stored = 0
    for row in rows:
        row_id = row.get("id") if isinstance(row, Mapping) else None
        try:
            payload = dict(row["payload"])
            metadata = dict(payload.get("metadata") or {})
            content = str(payload.get("content") or "")
            head = _checkpoint_head(call_mcp, server, str(payload.get("source_root") or ""))
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
                CALL_TIMEOUT_S,
            )
            ok = isinstance(envelope, Mapping) and envelope.get("ok") is True and not envelope.get("error")
            if not ok:
                raise RuntimeError(str(envelope.get("error") if isinstance(envelope, Mapping) else envelope)[:200])
        except Exception as exc:
            attempts = row.get("attempts", 0) if isinstance(row, Mapping) else 0
            log = logger.warning if not attempts else logger.debug
            log("ctx_handover: ctx record %s not stored (%s); it stays queued", row_id, type(exc).__name__)
            ok = False
        if row_id is None:
            continue
        try:
            ledger.outbox_done(row_id, ok, now)
        except Exception:
            logger.debug("ctx_handover: outbox row %s not updated", row_id, exc_info=True)
            continue
        stored += 1 if ok else 0
    return stored


def pending(ledger) -> int:
    """Rows still waiting for ctx (for ``/handover status``); 0 when the outbox is unreadable."""
    try:
        return len(ledger.outbox_due(float("inf"), 1000))
    except Exception:
        return 0
