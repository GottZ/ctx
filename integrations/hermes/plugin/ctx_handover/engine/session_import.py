"""Target side of the session transfer (layer L4, stdlib only; DESIGN §6.3–§6.10, INTERFACES §4.1).

``plan_import`` decides ids, titles, model route and import path per segment; ``import_hidden`` writes the
lineage hidden and verifies it (H_all, H_rows, H_model, lineage shape); ``apply_delta`` appends rows that
arrived during the handover; ``unhide``/``hide``/``discard`` are the commit and rollback steps; side state
(goal/heartbeat/loop) arrives paused.

Two import paths, both lossless (R2):

* **A** — ``import_sessions`` (one write transaction) for segments within the core's import limits
  (≤ 10 000 rows incl. archived, ≤ 5 MiB compact JSON; whole-payload ≤ 50 000 rows, ≤ 25 MiB, ≤ 500 sessions;
  ``hermes_state.py:510-511``, ``hermes_state_portability.py:690-711``);
* **B′** — ``create_session`` + ``append_messages_batch`` (archived rows, as live) + ``archive_and_compact``
  (the live rows) for every larger segment: the compaction archives exactly the appended rows
  (``active=0, compacted=1``) and inserts the live set after them (``hermes_state_messages.py:908``).

Rewind rows (``active=0, compacted=0``) are not transferred on either path (they are outside every history and
every hash). The peer's route — ``base_url``, ``api_mode``, ``gateway_runtime``, ``billing_*`` — never reaches a
row (R20): ``map_model`` rebuilds model fields from this host's configuration only.
"""

from __future__ import annotations

import ipaddress
import json
import logging
import os
import sqlite3
from collections.abc import Callable, Iterable, Iterator, Mapping, Sequence
from dataclasses import dataclass, field, replace
from datetime import datetime
from urllib.parse import urlsplit

from .errors import BadRequest, Cancelled, ImportFailed, PayloadTooLarge, VerifyFailed
from .ids import mint_session_id, short
from .model import DeltaInfo, PreparedInfo
from .session_hash import (
    ALL_ALGO,
    MAX_OBJECT_BYTES,
    ROWS_ALGO,
    HashStream,
    h_model,
    is_live_model_row,
    is_rewind_row,
    read_capsule_doc,
    read_json,
)

_log = logging.getLogger("ctx_handover.session_import")

MODEL_CONFIG_ALLOW: frozenset[str] = frozenset({"reasoning_config", "service_tier", "prompt_refresh_epoch"})
"""``model_config`` keys copied from the peer's row (DESIGN §6.4, R20); everything else is dropped."""

IMPORT_MAX_ROWS = 10_000
IMPORT_MAX_SEGMENT_BYTES = 5 * 1024 * 1024
IMPORT_MAX_TOTAL_ROWS = 50_000
IMPORT_MAX_TOTAL_BYTES = 25 * 1024 * 1024
IMPORT_MAX_SESSIONS = 500
"""The core's import limits (``hermes_state.py:510-511``); path A only within them."""

TITLE_RETRIES = 8
"""Attempts of the ``IntegrityError`` retitle loop before the ``(handover <job>)`` fallback (DESIGN §6.6)."""

BATCH_ROWS = 500
"""Chunk size of every batch append (the core's own value for large copies, DESIGN §6.8)."""

LOCAL_PROVIDERS: frozenset[str] = frozenset({"llamacpp", "llama.cpp", "llama-cpp", "ollama", "lmstudio"})
"""Providers whose route is local by construction (DESIGN §6.7 step 4)."""

RETIRED_PREFIX = "gateway.retired_session.v1."
"""Tombstone of a session deleted on this host (``hermes_state_mutation_retirement.py:9``)."""

PATH_A, PATH_B = "A", "B'"

# Sessions columns ``import_sessions`` writes (``_IMPORT_SESSION_INSERT_SQL``, hermes_state_portability.py:43-77)
# that the target takes from the peer's row object unchanged.
_KEEP_SESSION_COLS = (
    "started_at",
    "git_branch",
    "input_tokens",
    "output_tokens",
    "cache_read_tokens",
    "cache_write_tokens",
    "reasoning_tokens",
    "estimated_cost_usd",
    "actual_cost_usd",
    "cost_status",
    "cost_source",
    "pricing_version",
    "api_call_count",
    "pinned",
)
_MESSAGE_DROP = frozenset({"id", "session_id", "display_identity", "display_order"})


@dataclass(frozen=True)
class SegmentPlan:
    """One source segment and how it lands here. ``model``/``model_config`` are this host's route (R20)."""

    old_id: str
    new_id: str
    parent_new_id: str | None
    is_tip: bool
    title: str | None
    path: str  # "A" | "B'"
    model: str | None = None
    model_config: dict | None = None
    rows: int = 0
    bytes: int = 0


@dataclass(frozen=True)
class ImportPlan:
    """The import plan; rows stay in the block store and are streamed segment by segment (memory, SK L7)."""

    segments: tuple[SegmentPlan, ...]
    model_change: dict | None
    cwd: str
    git_repo_root: str | None
    job_short: str = ""
    warnings: tuple[str, ...] = field(default=())


# ---------------------------------------------------------------------------------------------------------
# Model and provider mapping (DESIGN §6.7)
# ---------------------------------------------------------------------------------------------------------


def _target_main(target_config: Mapping) -> tuple[str | None, str | None]:
    model = target_config.get("model") if isinstance(target_config, Mapping) else None
    if isinstance(model, Mapping):
        name = model.get("default") or model.get("model") or model.get("name")
        provider = model.get("provider")
    else:
        name, provider = model, None
    name = name.strip() if isinstance(name, str) and name.strip() else None
    provider = provider.strip() if isinstance(provider, str) and provider.strip() else None
    return name, provider


def _json_object(value: object) -> dict:
    if isinstance(value, str) and value:
        try:
            value = json.loads(value)
        except ValueError:
            return {}
    return dict(value) if isinstance(value, Mapping) else {}


def source_route(row: Mapping) -> tuple[str | None, str | None]:
    """The two names the target reads from a peer's row: ``(model, provider)`` (DESIGN §6.7 step 1)."""
    cfg = _json_object(row.get("model_config"))
    provider = cfg.get("provider")
    runtime = cfg.get("gateway_runtime")
    if not (isinstance(provider, str) and provider.strip()) and isinstance(runtime, Mapping):
        provider = runtime.get("provider")
    if not (isinstance(provider, str) and provider.strip()):
        provider = row.get("billing_provider")
    model = row.get("model")
    return (
        model.strip() if isinstance(model, str) and model.strip() else None,
        provider.strip() if isinstance(provider, str) and provider.strip() else None,
    )


def map_model(
    row: Mapping,
    policy: str,
    override: str | None,
    target_config: dict,
    provider_usable: Callable[[str, dict], bool],
) -> tuple[dict, dict | None]:
    """Row fields ``model``/``model_config``/``billing_*`` rebuilt from THIS host's configuration only (R20).

    ``override`` (a local ``--model``) wins; ``policy == "target_default"`` takes the main model; ``auto`` keeps
    the peer's model name when its provider is this host's main provider or a configured ``providers.<name>``
    entry and ``provider_usable`` says this host can serve it — the route is then this host's own (nothing for
    the main provider, ``gateway_runtime = {"provider": name}`` for a named entry).

    Returns the fields and ``model_change`` in its one form (contract X04): ``None`` when model and provider
    stay the same, else ``{"from": {"model", "provider"}, "to": {"model", "provider", "context_length"},
    "reason": str, "offline_capable": bool}`` — ``offline_capable`` from :func:`offline_capable` on the
    resulting route, ``context_length`` from :func:`context_length`. ``reason``: ``override`` |
    ``target_default`` | ``provider_unusable``.
    """
    src_model, src_provider = source_route(row)
    main_model, main_provider = _target_main(target_config)
    allowed = {k: v for k, v in _json_object(row.get("model_config")).items() if k in MODEL_CONFIG_ALLOW}
    named = target_config.get("providers") if isinstance(target_config, Mapping) else None
    named = named if isinstance(named, Mapping) else {}
    reason = "target_default"
    new_model, new_provider = main_model, main_provider
    route: dict | None = None
    if override:
        new_model, reason = override.strip(), "override"
    elif policy == "auto" and src_provider and src_model:
        is_main = main_provider is not None and src_provider == main_provider
        is_named = isinstance(named.get(src_provider), Mapping)
        usable = False
        if is_main or is_named:
            try:
                usable = bool(provider_usable(src_provider, target_config))
            except Exception:  # a probe failure is "not usable", never a peer route
                usable = False
        if usable:
            new_model, new_provider, reason = src_model, src_provider, "kept"
            if not is_main:
                route = {"provider": src_provider}
        else:
            reason = "provider_unusable"
    elif policy not in ("auto", "target_default"):
        raise BadRequest(f"unknown model_mapping {policy!r}", field="model_mapping")
    model_config = dict(allowed)
    if route is not None:
        model_config["gateway_runtime"] = route
    fields = {
        "model": new_model,
        "model_config": model_config or None,
        "billing_provider": None,
        "billing_base_url": None,
        "billing_mode": None,
    }
    change = None
    if (new_model, new_provider) != (src_model, src_provider):
        change = {
            "from": {"model": src_model, "provider": src_provider},
            "to": {
                "model": new_model,
                "provider": new_provider,
                "context_length": context_length(fields["model_config"], new_model, target_config),
            },
            "reason": reason,
            "offline_capable": offline_capable(fields["model_config"], target_config),
        }
    return fields, change


def _positive_int(value: object) -> int | None:
    return value if isinstance(value, int) and not isinstance(value, bool) and value > 0 else None


def context_length(model_config: dict | None, model: str | None, target_config: dict) -> int | None:
    """The context length THIS host configures for ``model`` on the route ``model_config`` names (DESIGN §6.7
    step 4): for a named provider entry (``gateway_runtime.provider``) its ``models.<model>.context_length``,
    else the entry's ``context_length``; on the main route ``model.context_length`` when ``model`` is the main
    model. ``None`` when nothing is configured — the core then resolves it from its model catalog.
    """
    cfg = model_config if isinstance(model_config, Mapping) else {}
    runtime = cfg.get("gateway_runtime") if isinstance(cfg.get("gateway_runtime"), Mapping) else {}
    named = target_config.get("providers") if isinstance(target_config, Mapping) else None
    entry = named.get(runtime.get("provider")) if isinstance(named, Mapping) and runtime.get("provider") else None
    if isinstance(entry, Mapping):
        models = entry.get("models")
        per_model = models.get(model) if isinstance(models, Mapping) and model else None
        if isinstance(per_model, Mapping) and _positive_int(per_model.get("context_length")):
            return _positive_int(per_model.get("context_length"))
        return _positive_int(entry.get("context_length"))
    main_model, _main_provider = _target_main(target_config)
    section = target_config.get("model") if isinstance(target_config, Mapping) else None
    if model and model == main_model and isinstance(section, Mapping):
        return _positive_int(section.get("context_length"))
    return None


def _is_local_url(url: object) -> bool:
    if not isinstance(url, str) or not url:
        return False
    try:
        host = urlsplit(url).hostname
    except ValueError:
        return False
    if not host:
        return False
    if host == "localhost" or host.endswith(".localhost"):
        return True
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return False
    return ip.is_loopback or ip.is_private or ip.is_link_local


def offline_capable(model_config: dict | None, target_config: dict) -> bool:
    """Does the resulting route stay on this host or its LAN? (DESIGN §6.7 step 4; BX P7)

    The provider is the row's own route (``gateway_runtime.provider``) or this host's main provider; local
    means a local provider kind or a loopback/RFC 1918 ``base_url`` in THIS host's configuration. The one
    offline rule (contract X04): ``map_model`` fills ``model_change.offline_capable`` and the target's
    ``CommitAck.offline_capable`` from it, with the tip segment's ``model_config`` of the import plan.
    """
    cfg = model_config if isinstance(model_config, Mapping) else {}
    runtime = cfg.get("gateway_runtime") if isinstance(cfg.get("gateway_runtime"), Mapping) else {}
    _main_model, main_provider = _target_main(target_config)
    provider = runtime.get("provider") or main_provider
    if not isinstance(provider, str) or not provider:
        return False
    if provider.lower() in LOCAL_PROVIDERS:
        return True
    named = target_config.get("providers") if isinstance(target_config, Mapping) else None
    entry = named.get(provider) if isinstance(named, Mapping) else None
    if isinstance(entry, Mapping):
        return _is_local_url(entry.get("base_url") or entry.get("api_base"))
    model = target_config.get("model") if isinstance(target_config, Mapping) else None
    if provider == main_provider and isinstance(model, Mapping):
        return _is_local_url(model.get("base_url"))
    return False


# ---------------------------------------------------------------------------------------------------------
# Plan
# ---------------------------------------------------------------------------------------------------------


def _id_taken(db, session_id: str) -> bool:
    return db.get_session(session_id) is not None or bool(db.get_meta(RETIRED_PREFIX + session_id))


def mint_free_id(db, taken: set[str], now: datetime | None = None) -> str:
    """A fresh id in the core's format that is neither a row, a tombstone nor already planned (DESIGN §6.4)."""
    for _ in range(64):
        candidate = mint_session_id(now)
        if candidate not in taken and not _id_taken(db, candidate):
            taken.add(candidate)
            return candidate
    raise ImportFailed("could not mint a free session id")


def _title_candidates(db, title: str, job_short: str) -> Iterator[str]:
    """Titles to try for ``title`` on this host: itself, ``<T> #n`` (``get_next_title_in_lineage``), fallback."""
    canonical = getattr(db, "CANONICAL_BOT_CHAT_TITLE", "Bot Chat")
    limit = int(getattr(db, "MAX_TITLE_LENGTH", 100) or 100)
    fallback = _fit(title, f" (handover {job_short})", limit)
    if title == canonical:
        yield fallback
        return
    if db.get_session_by_title(title) is None:
        yield title
    seen = {title}
    for _ in range(TITLE_RETRIES):
        nxt = db.get_next_title_in_lineage(title)
        if nxt in seen:
            break
        seen.add(nxt)
        yield nxt
    yield fallback


def _fit(base: str, suffix: str, limit: int) -> str:
    room = max(1, limit - len(suffix))
    return (base[:room].rstrip() or base[:room]) + suffix


def plan_import(
    db,
    capsule: dict,
    *,
    cwd_for: Callable[[str | None], str],
    model_policy: str,
    model_override: str | None,
    target_config: dict,
    provider_usable: Callable[[str, dict], bool],
    max_session_bytes: int,
) -> ImportPlan:
    """Plan ids, titles, model route and path for every segment of ``capsule`` (validated by ``read_capsule``).

    ``cwd_for(source_path)`` maps a source path to the target path (DESIGN §9.7; it never returns a peer path
    unchecked — cluster D/A own the mapping); the cwd comes from the tip's row [R11].
    """
    segments = capsule.get("segments") or []
    stats = capsule.get("stats") or {}
    total_bytes = int(stats.get("bytes") or 0) or sum(int(s.get("bytes") or 0) for s in segments)
    if total_bytes > max_session_bytes:
        raise PayloadTooLarge("session exceeds max_session_bytes", size=total_bytes, limit=max_session_bytes)
    job_short = short(str(capsule.get("job_id") or "")) or "import"
    taken: set[str] = set()
    tip_row = segments[-1]["row_object"]
    cwd = cwd_for(tip_row.get("cwd"))
    git_root = cwd_for(tip_row.get("git_repo_root")) if tip_row.get("git_repo_root") else None
    planned: list[SegmentPlan] = []
    tip_change: dict | None = None
    titles_planned: set[str] = set()
    parent_new: str | None = None
    for index, seg in enumerate(segments):
        row = seg["row_object"]
        is_tip = index == len(segments) - 1
        fields, change = map_model(row, model_policy, model_override, target_config, provider_usable)
        if is_tip:
            tip_change = change
        title = row.get("title")
        planned_title = None
        if isinstance(title, str) and title:
            for candidate in _title_candidates(db, title, job_short):
                if candidate not in titles_planned:
                    planned_title = candidate
                    break
            if planned_title:
                titles_planned.add(planned_title)
        rows = int(seg.get("rows") or 0) - int(seg.get("rewind") or 0)
        seg_bytes = int(seg.get("bytes") or 0)
        path = PATH_A if rows <= IMPORT_MAX_ROWS and seg_bytes <= IMPORT_MAX_SEGMENT_BYTES else PATH_B
        new_id = mint_free_id(db, taken)
        planned.append(
            SegmentPlan(
                old_id=str(row.get("id")),
                new_id=new_id,
                parent_new_id=parent_new,
                is_tip=is_tip,
                title=planned_title,
                path=path,
                model=fields["model"],
                model_config=fields["model_config"],
                rows=rows,
                bytes=seg_bytes,
            )
        )
        parent_new = new_id
    return ImportPlan(tuple(planned), tip_change, cwd, git_root, job_short)


# ---------------------------------------------------------------------------------------------------------
# Rewrite (DESIGN §6.4, §6.5)
# ---------------------------------------------------------------------------------------------------------


def rewrite_session(row: Mapping, seg: SegmentPlan, plan: ImportPlan) -> dict:
    """The session dict ``import_sessions`` receives for ``seg`` (every column rule of DESIGN §6.4)."""
    out = {col: row.get(col) for col in _KEEP_SESSION_COLS}
    out.update(
        id=seg.new_id,
        source="cli",
        user_id=None,
        session_key=None,
        chat_id=None,
        chat_type=None,
        thread_id=None,
        display_name=None,
        origin_json=None,
        model=seg.model,
        model_config=seg.model_config,
        billing_provider=None,
        billing_base_url=None,
        billing_mode=None,
        system_prompt=None,
        system_prompt_hash=None,
        parent_session_id=seg.parent_new_id,
        ended_at=None if seg.is_tip else row.get("ended_at"),
        end_reason=None if seg.is_tip else row.get("end_reason"),
        cwd=plan.cwd,
        git_repo_root=plan.git_repo_root,
        title=seg.title,
        archived=0,
        auto_archived=0,
        hidden=1,
    )
    return out


def rewrite_message(row: Mapping, new_id: str) -> dict:
    """A message row for this host: byte-true payload, new ``session_id``, no row id, no platform id (§6.5)."""
    out = {k: v for k, v in row.items() if k not in _MESSAGE_DROP}
    out["session_id"] = new_id
    out["platform_message_id"] = None
    return out


# ---------------------------------------------------------------------------------------------------------
# Import
# ---------------------------------------------------------------------------------------------------------


def _segment_rows(store, segment: Mapping) -> Iterator[dict]:
    for page_id in segment["pages"]:
        page = read_json(store, page_id, max_bytes=MAX_OBJECT_BYTES)
        if not isinstance(page, list):
            raise BadRequest("capsule page is not a list", field="segments.pages")
        for row in page:
            if not isinstance(row, dict) or not isinstance(row.get("role"), str):
                raise BadRequest("capsule row is malformed", field="segments.pages")
            yield row


def _compact_size(obj: object) -> int:
    return len(json.dumps(obj, ensure_ascii=False, separators=(",", ":"), default=str).encode("utf-8"))


class _Txn:
    """Remembers what the import created, so any failure removes all of it (I3)."""

    def __init__(self, db):
        self.db = db
        self.created: list[str] = []

    def discard(self) -> None:
        for sid in reversed(self.created):
            try:
                if self.db.get_session(sid) is not None:
                    self.db.delete_session(sid)
            except Exception as exc:  # cleanup must try every created row
                _log.warning("discarding imported session %s failed: %s", sid, exc)


def _import_a(db, payload: list[dict], plan: ImportPlan) -> tuple[list[dict], set[str]]:
    """``import_sessions`` with the title loop: an ``IntegrityError`` on a title retitles that segment (≤ 8).

    Returns the payload written and the ids the core actually imported. Success is only ``ok is True`` with every
    payload id in ``imported_ids``: the core answers ``ok`` for a payload whose ids already exist and skips them
    (``hermes_state_portability.py:822-838``) — such an id is somebody else's session, never ours."""
    for _attempt in range(TITLE_RETRIES + 2):
        try:
            result = db.import_sessions(payload)
        except sqlite3.IntegrityError as exc:
            if "title" not in str(exc):
                raise ImportFailed("import_sessions failed", error=str(exc)[:300]) from exc
            payload = _retitle(db, payload, plan)
            continue
        if not isinstance(result, Mapping):
            raise ImportFailed("import_sessions gave no result")
        if result.get("ok") is True:
            listed = result.get("imported_ids")
            if isinstance(listed, list):
                imported = {i for i in listed if isinstance(i, str)}
            else:  # a core without the id list: not skipped and present now (all were free just before)
                skipped = {i for i in (result.get("skipped_ids") or ()) if isinstance(i, str)}
                imported = {
                    str(seg.get("id"))
                    for seg in payload
                    if str(seg.get("id")) not in skipped and db.get_session(str(seg.get("id"))) is not None
                }
            return payload, imported
        errors = [str(e) for e in (result.get("errors") or [])]
        if any("was deleted here" in e for e in errors):
            raise _TombstoneHit(errors)
        raise ImportFailed("import_sessions refused the lineage", errors=errors[:8])
    raise ImportFailed("could not find a free title")


class _TombstoneHit(Exception):
    """A planned id is not free any more (a tombstone, or a session created here after planning): re-mint."""


def _retitle(db, payload: list[dict], plan: ImportPlan) -> list[dict]:
    out = []
    for seg in payload:
        title = seg.get("title")
        if title and db.get_session_by_title(title) is not None:
            base = title
            nxt = db.get_next_title_in_lineage(base)
            if nxt == title or db.get_session_by_title(nxt) is not None:
                nxt = _fit(base, f" (handover {plan.job_short})", int(getattr(db, "MAX_TITLE_LENGTH", 100) or 100))
            seg = {**seg, "title": nxt}
        out.append(seg)
    return out


def _set_title(db, session_id: str, title: str | None, plan: ImportPlan) -> str | None:
    if not title:
        return None
    for candidate in [title, *list(_title_candidates(db, title, plan.job_short))]:
        try:
            db.set_session_title(session_id, candidate)
            return candidate
        except (ValueError, sqlite3.IntegrityError):
            continue
    return None


def _import_b_prime(
    db, store, segment: Mapping, seg: SegmentPlan, plan: ImportPlan, cancelled
) -> tuple[str | None, int]:
    """Path B′ for one segment (DESIGN §6.8 steps 1–5); returns the title written and the rewind rows skipped."""
    row = segment["row_object"]
    db.create_session(
        seg.new_id,
        "cli",
        model=seg.model,
        model_config=seg.model_config,
        parent_session_id=seg.parent_new_id,
        cwd=plan.cwd,
        git_repo_root=plan.git_repo_root,
    )
    db.set_session_hidden(seg.new_id, True)
    if row.get("git_branch") and plan.cwd:
        db.update_session_cwd(seg.new_id, plan.cwd, git_branch=row.get("git_branch"), git_repo_root=plan.git_repo_root)
    archived_chunk: list[dict] = []
    live: list[dict] = []
    rewind = 0
    seen_live = False
    for src in _segment_rows(store, segment):
        if is_rewind_row(src):
            rewind += 1
            continue
        msg = rewrite_message(src, seg.new_id)
        if msg.get("active", 1):
            seen_live = True
            msg["active"] = 1
            msg.pop("compacted", None)
            live.append(msg)
            continue
        if seen_live:
            raise ImportFailed("an archived row follows a live row; path B' cannot reproduce it", segment=seg.old_id)
        msg["active"] = 1
        msg.pop("compacted", None)
        archived_chunk.append(msg)
        if len(archived_chunk) >= BATCH_ROWS:
            db.append_messages_batch(seg.new_id, archived_chunk, chunk_rows=BATCH_ROWS)
            archived_chunk = []
            if cancelled():
                raise Cancelled("import cancelled")
    if archived_chunk:
        db.append_messages_batch(seg.new_id, archived_chunk, chunk_rows=BATCH_ROWS)
    if live:
        db.archive_and_compact(seg.new_id, live)
    elif db.get_messages(seg.new_id, limit=1):
        raise ImportFailed(
            "segment has archived rows but no live rows; path B' cannot reproduce it", segment=seg.old_id
        )
    title = _set_title(db, seg.new_id, seg.title, plan)
    if not seg.is_tip:
        db.end_session(seg.new_id, row.get("end_reason") or "compression")
    return title, rewind


def _verify(db, new_ids: Sequence[str]) -> tuple[str, str, str]:
    tip = new_ids[-1]
    all_stream = HashStream(ALL_ALGO)
    rows_stream = HashStream(ROWS_ALGO)
    for sid in new_ids:
        last = 0
        while True:
            rows = db.get_messages(sid, include_inactive=True, after_id=last, limit=BATCH_ROWS)
            for r in rows:
                if not is_rewind_row(r):
                    all_stream.add(r)
                if sid == tip and is_live_model_row(r):
                    rows_stream.add(r)
            if len(rows) < BATCH_ROWS:
                break
            last = int(rows[-1]["id"])
    return all_stream.hexdigest(), rows_stream.hexdigest(), h_model(db.get_resume_conversations(tip)[0])


def _freeze(value: object) -> object:
    if isinstance(value, (list, tuple)):
        return tuple(_freeze(v) for v in value)
    return value


def import_hidden(
    db,
    store,
    capsule_id: str,
    plan: ImportPlan,
    *,
    cancelled: Callable[[], bool] = lambda: False,
    local_hermes_version: str | None = None,
    max_json_bytes: int = MAX_OBJECT_BYTES,
    max_session_bytes: int = 512 * 1024**2,
) -> PreparedInfo:
    """Write the planned lineage hidden and verify it; any failure leaves nothing behind (I3).

    Gates: lineage shape (``get_compression_lineage`` of the new tip = the planned ids), H_all and H_rows equal
    to the capsule's; H_model equal when both Hermes versions are known and equal, else a mismatch is a warning.
    ``assert_resume_safe`` failing is a warning [SK L4]. Raises ``VerifyFailed``/``ImportFailed``.
    """
    capsule = read_capsule_doc(store, capsule_id, max_json_bytes=max_json_bytes, max_session_bytes=max_session_bytes)
    segments = capsule["segments"]
    if len(segments) != len(plan.segments):
        raise BadRequest("import plan does not match the capsule", field="plan.segments")
    for attempt in range(3):
        txn = _Txn(db)
        try:
            return _import_once(db, store, capsule, plan, txn, cancelled, local_hermes_version)
        except _TombstoneHit:
            txn.discard()
            taken: set[str] = set()
            renewed: list[SegmentPlan] = []
            parent = None
            for seg in plan.segments:
                new_id = mint_free_id(db, taken)
                renewed.append(replace(seg, new_id=new_id, parent_new_id=parent))
                parent = new_id
            plan = replace(plan, segments=tuple(renewed))
            if attempt == 2:
                raise ImportFailed("planned ids collide with deleted sessions on this host") from None
        except BaseException:
            txn.discard()
            raise
    raise ImportFailed("import did not complete")


def _require_free(db, ids: Sequence[str]) -> None:
    """Every planned id is still free right before it is written (a session may have taken it since planning)."""
    taken = [i for i in ids if _id_taken(db, i)]
    if taken:
        raise _TombstoneHit([f"planned session {i} already exists here" for i in taken])


def _import_once(
    db, store, capsule: dict, plan: ImportPlan, txn: _Txn, cancelled, local_hermes_version
) -> PreparedInfo:
    segments = capsule["segments"]
    warnings: list[str] = list(plan.warnings)
    paths_used: list[str] = []
    titles: dict[str, str | None] = {}
    rewind_total = 0
    batch: list[dict] = []
    batch_rows = 0
    batch_bytes = 0

    def flush() -> None:
        nonlocal batch, batch_rows, batch_bytes
        if not batch:
            return
        ids = [s["id"] for s in batch]
        _require_free(db, ids)
        written, imported = _import_a(db, batch, plan)
        txn.created.extend(i for i in ids if i in imported)  # only what this import wrote is ever discarded
        if imported != set(ids):
            raise _TombstoneHit([f"planned session {i} already exists here" for i in ids if i not in imported])
        for s in written:
            titles[s["id"]] = s.get("title")
        batch, batch_rows, batch_bytes = [], 0, 0

    for seg, segment in zip(plan.segments, segments, strict=True):
        if cancelled():
            raise Cancelled("import cancelled")
        row = segment["row_object"]
        if str(row.get("id")) != seg.old_id:
            raise BadRequest("import plan does not match the capsule", field="plan.segments")
        path = seg.path
        session = rewrite_session(row, seg, plan) if path == PATH_A else None
        if session is not None:
            messages = []
            for src in _segment_rows(store, segment):
                if is_rewind_row(src):
                    rewind_total += 1
                    continue
                messages.append(rewrite_message(src, seg.new_id))
            session["messages"] = messages
            size = _compact_size(session)
            if len(messages) > IMPORT_MAX_ROWS or size > IMPORT_MAX_SEGMENT_BYTES:
                path, session = PATH_B, None
            elif (
                batch_rows + len(messages) > IMPORT_MAX_TOTAL_ROWS
                or batch_bytes + size > IMPORT_MAX_TOTAL_BYTES
                or len(batch) >= IMPORT_MAX_SESSIONS
            ):
                flush()
            if session is not None:
                batch.append(session)
                batch_rows += len(messages)
                batch_bytes += size
        if path == PATH_B:
            flush()
            _require_free(db, [seg.new_id])  # ``create_session`` is an upsert: never write into a foreign row
            txn.created.append(seg.new_id)
            title, rewind = _import_b_prime(db, store, segment, seg, plan, cancelled)
            titles[seg.new_id] = title
            rewind_total += rewind
            warnings.append(
                f"segment {seg.new_id} used path B' (over the import limit): token/cost counters start at 0"
            )
        paths_used.append(path)
    flush()
    new_ids = [s.new_id for s in plan.segments]
    tip = new_ids[-1]
    lineage = list(db.get_compression_lineage(tip))
    if lineage != new_ids:
        raise VerifyFailed("imported lineage shape differs", expected=new_ids, got=lineage)
    hashes = capsule["hashes"]
    got_all, got_rows, got_model = _verify(db, new_ids)
    if got_all != hashes.get("h_all") or got_rows != hashes.get("h_rows"):
        raise VerifyFailed(
            "imported session differs from the capsule",
            h_all_ok=got_all == hashes.get("h_all"),
            h_rows_ok=got_rows == hashes.get("h_rows"),
        )
    source_version = str((capsule.get("source") or {}).get("hermes_version") or "")
    if got_model != hashes.get("h_model"):
        if local_hermes_version and source_version and local_hermes_version == source_version:
            raise VerifyFailed("imported model history differs from the capsule", h_model_ok=False)
        warnings.append("model history hash differs (different Hermes versions repair alternation differently)")
    try:
        db.assert_resume_safe(tip)
    except Exception as exc:
        warnings.append(f"resume guard: {str(exc)[:200]}")
    if rewind_total:
        warnings.append(f"{rewind_total} rewound row(s) were not transferred (outside every history)")
    path_label = "+".join(dict.fromkeys(paths_used))
    return PreparedInfo(
        tip_id=tip,
        root_id=new_ids[0],
        title=titles.get(tip),
        h_all=got_all,
        h_rows=got_rows,
        h_model=got_model,
        import_path=path_label,
        dests=(),
        warnings=tuple(warnings),
        source_revision=_freeze((capsule.get("session") or {}).get("revision") or ()),
    )


def apply_delta(db, store, delta: DeltaInfo, prepared: PreparedInfo) -> PreparedInfo:
    """Append the delta rows to the hidden tip and re-verify H_all/H_rows against the delta's expectation.

    A mismatch raises ``VerifyFailed`` and leaves the hidden import for the caller's reset/discard decision.
    """
    doc = read_json(store, delta.delta_id, max_bytes=MAX_OBJECT_BYTES)
    if not isinstance(doc, dict) or doc.get("kind") != "ctxh.delta" or not isinstance(doc.get("rows"), list):
        raise BadRequest("not a ctxh.delta document", field="delta")
    rows = []
    for src in doc["rows"]:
        if not isinstance(src, dict) or not isinstance(src.get("role"), str) or not src.get("active", 1):
            raise BadRequest("delta row is malformed or not live", field="delta.rows")
        msg = rewrite_message(src, prepared.tip_id)
        msg.pop("compacted", None)
        rows.append(msg)
    if rows:
        db.append_messages_batch(prepared.tip_id, rows, chunk_rows=BATCH_ROWS)
    lineage = list(db.get_compression_lineage(prepared.tip_id))
    got_all, got_rows, got_model = _verify(db, lineage)
    if got_all != delta.h_all or got_rows != delta.h_rows:
        raise VerifyFailed(
            "session differs from the source after the delta",
            h_all_ok=got_all == delta.h_all,
            h_rows_ok=got_rows == delta.h_rows,
        )
    warnings = list(prepared.warnings)
    if got_model != delta.h_model:
        warnings.append("model history hash differs after the delta")
    return replace(
        prepared,
        h_all=got_all,
        h_rows=got_rows,
        h_model=got_model,
        warnings=tuple(warnings),
        source_revision=_freeze(delta.revision),
    )


def unhide(db, prepared: PreparedInfo) -> None:
    """Commit step c6: make the lineage visible (``set_session_hidden`` is lineage-wide).

    Refuses (``ImportFailed``) while the tip's recorded cwd does not exist: a resume would answer "must be an
    absolute path to an existing directory" [LAB L3, R11]; commit step c2 creates it before this step.
    """
    row = db.get_session(prepared.tip_id) or {}
    cwd = row.get("cwd")
    if not cwd or not os.path.isdir(cwd):
        raise ImportFailed("the session's working directory does not exist on this host", cwd=cwd)
    db.set_session_hidden(prepared.tip_id, False)


def hide(db, prepared: PreparedInfo) -> None:
    """Rollback of c6."""
    db.set_session_hidden(prepared.tip_id, True)


def discard(db, prepared: PreparedInfo) -> None:
    """Remove the imported lineage completely (abort cleanup, DESIGN §6.8)."""
    if db.get_session(prepared.tip_id) is not None:
        db.delete_session(prepared.tip_id, include_compression_chain=True)
    if db.get_session(prepared.root_id) is not None:
        db.delete_session(prepared.root_id, include_compression_chain=True)


# ---------------------------------------------------------------------------------------------------------
# Side state (DESIGN §6.3)
# ---------------------------------------------------------------------------------------------------------

_GOAL_WAIT_CLEAR = {
    "waiting_on_pid": None,
    "waiting_on_session": None,
    "waiting_until": 0.0,
    "waiting_on_delegations": 0,
    "waiting_reason": None,
    "waiting_since": 0.0,
}


def side_state_values(capsule: Mapping, *, goal_max_turns: int = 20) -> dict[str, dict]:
    """The target's goal/heartbeat/loop JSON: always ``paused`` [R22, SEC 4.5]; goal ``max_turns`` capped and
    wait barriers cleared; loop ``route`` (the source's chat route) removed."""
    state = capsule.get("state_meta") if isinstance(capsule.get("state_meta"), Mapping) else {}
    out: dict[str, dict] = {}
    goal = state.get("goal")
    if isinstance(goal, Mapping):
        g = dict(goal)
        g["status"] = "paused"
        g["paused_reason"] = "handed over"
        try:
            g["max_turns"] = min(int(g.get("max_turns") or goal_max_turns), int(goal_max_turns))
        except (TypeError, ValueError):
            g["max_turns"] = int(goal_max_turns)
        g.update(_GOAL_WAIT_CLEAR)
        out["goal"] = g
    heartbeat = state.get("heartbeat")
    if isinstance(heartbeat, Mapping):
        out["heartbeat"] = {**heartbeat, "status": "paused"}
    loop = state.get("loop")
    if isinstance(loop, Mapping):
        lp = {k: v for k, v in loop.items() if k != "route"}
        lp["status"] = "paused"
        lp["paused_reason"] = "handed over"
        out["loop"] = lp
    return out


def write_side_state(db, capsule: dict, new_tip: str, *, goal_max_turns: int = 20) -> None:
    """Commit step c4: write the paused drivers under the new tip id."""
    for key, value in side_state_values(capsule, goal_max_turns=goal_max_turns).items():
        db.set_meta(f"{key}:{new_tip}", json.dumps(value, ensure_ascii=False, sort_keys=True))


def clear_side_state(db, new_tip: str) -> None:
    """Rollback of c4: the drivers written for ``new_tip`` become ``cleared`` (state_meta has no public delete)."""
    for key in ("goal", "heartbeat", "loop"):
        raw = db.get_meta(f"{key}:{new_tip}")
        if not raw:
            continue
        try:
            value = json.loads(raw)
        except ValueError:
            value = {}
        if isinstance(value, dict):
            value["status"] = "cleared"
            db.set_meta(f"{key}:{new_tip}", json.dumps(value, ensure_ascii=False, sort_keys=True))


def iter_rows_of(store, capsule: Mapping) -> Iterable[tuple[int, dict]]:
    """``(segment index, row)`` of a validated capsule document (helper for callers holding the dict)."""
    for index, segment in enumerate(capsule["segments"]):
        for row in _segment_rows(store, segment):
            yield index, row
