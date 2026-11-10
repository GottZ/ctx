"""Source side of the session transfer (layer L4, stdlib only; DESIGN §6.1–§6.3, §6.9, INTERFACES §4.1).

``describe`` resolves the session a job is about; ``export_capsule`` writes the faithful, target-agnostic
capsule (row objects, keyset-paged message pages, side state, session-file references, verification hashes)
inside a revision bracket; ``check_revision``/``export_delta`` decide at SOURCE_PREPARING whether the
transcript only grew (delta) or changed otherwise (``stale_session``).

Every read goes through public ``SessionDB`` methods; ``db`` is passed in (built by ``hostcompat`` in the
composition roots). Nothing here writes to the session store.

Identity completion (``complete_identity``): rows an older Hermes build wrote carry no ``message_uid`` (the core
mints one lazily, one bounded slice per store open, ``hermes_state_schema.py:1127-1156``) and assistant rows no
``tool_call_uids`` (never backfilled; ``agent/message_metadata.py:341-348``). Every insert path of the target
mints the missing ids at random (``hermes_state_messages.py:417-418, 674-677``), so such a row could never verify.
The capsule therefore carries, for exactly the ids the target's insert would mint, a stable id derived from the
source row (``ctxh-legacy-uid-v1``: sha256 of the session id, the row id and, for a call, its pairing key) — the
rows the core already stamped stay byte-true. Source hashes, delta hashes and the target all see the same ids.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Callable, Iterator, Mapping, Sequence

from . import hermes_files
from .errors import BadRequest, Cancelled, NotFound, StaleSession
from .jsonutil import canonical_bytes
from .model import CapsuleInfo, DeltaInfo, HermesPaths, PrepareOutcome, SessionInfo, SessionRef
from .session_hash import (
    ALL_ALGO,
    MAX_OBJECT_BYTES,
    MAX_PAGES,
    MAX_SEGMENTS,
    MODEL_ALGO,
    ROWS_ALGO,
    HashStream,
    h_model,
    is_live_model_row,
    is_rewind_row,
    put_json,
    read_capsule_doc,
    read_json,
)

SIDE_STATE_KEYS: tuple[str, ...] = ("goal", "heartbeat", "loop")
"""``state_meta`` drivers carried with a session (``<key>:<tip id>``, DESIGN §6.3)."""

ROW_OBJECT_DROP: frozenset[str] = frozenset({"messages", "timings", "segments", "system_prompt", "tool_names"})
"""Session-row keys the capsule never carries. ``system_prompt``/``tool_names`` are rebuilt by the target
(DESIGN §6.4) and would only ship the source's prompt text (memory, context files) for nothing."""

LEGACY_UID_ALGO = "ctxh-legacy-uid-v1"
"""Namespace of the ids ``complete_identity`` derives for rows without core-stamped identities."""

DEFAULT_PAGE_ROWS = 2000
DEFAULT_PAGE_BYTES = 4 * 1024**2
_READ_BATCH = 500


def _provider_of(row: Mapping) -> str | None:
    """The provider name a session row records (``model_config`` route shapes, then ``billing_provider``)."""
    cfg = row.get("model_config")
    if isinstance(cfg, str) and cfg:
        try:
            cfg = json.loads(cfg)
        except ValueError:
            cfg = None
    if isinstance(cfg, Mapping):
        for candidate in (
            cfg.get("provider"),
            (cfg.get("gateway_runtime") or {}).get("provider")
            if isinstance(cfg.get("gateway_runtime"), Mapping)
            else None,
        ):
            if isinstance(candidate, str) and candidate.strip():
                return candidate.strip()
    billing = row.get("billing_provider")
    return billing.strip() if isinstance(billing, str) and billing.strip() else None


def provider_of(row: Mapping) -> str | None:
    """Public form of the provider lookup (also used by the target's model mapping)."""
    return _provider_of(row)


def _latest_session_id(db) -> str:
    rows = db.list_sessions_rich(limit=1, min_message_count=1, order_by_last_active=True, include_archived=False)
    if not rows:
        raise NotFound("this Hermes profile has no session with messages to hand over")
    return str(rows[0]["id"])


def _offered_session(ref: SessionRef, offer_session: Callable[[str], str | None] | None) -> str:
    """The session an offer names (F12): ``offer_session`` is the ledger lookup of the composition root (cluster A
    may claim the offer for the job inside it; its errors pass through)."""
    offer_id = str(ref.offer_id)
    if offer_session is None:
        raise BadRequest(f"offer {offer_id} cannot be found here", field="session_ref.offer_id")
    sid = offer_session(offer_id)
    if not isinstance(sid, str) or not sid:
        raise NotFound(f"offer {offer_id} not found", offer_id=offer_id)
    return sid


def _tip_of(db, sid: str) -> str:
    if db.get_session(sid) is None:
        raise NotFound("session not found", session_id=sid)
    return db.resolve_resume_session_id(sid) or sid


def describe(
    db,
    ref: SessionRef,
    *,
    calling_session: str | None,
    offer_session: Callable[[str], str | None] | None = None,
) -> SessionInfo:
    """Resolve ``ref`` (explicit id › offer › ``latest`` › the calling session) to its tip and compression lineage.

    ``offer_session(offer_id) -> session id | None`` resolves ``ref.offer_id`` (F12, ``take --offer``). With both an
    id and an offer (route #6 answers ``{id, offer_id}``) and a lookup, both must name the same tip, so an offer's
    stored refinement never rides on a session the offer did not name.

    ``cwd``/``git_*`` come from the session row of the tip, never from ``ctx.session_cwd()`` [LAB L2, R11].
    """
    if ref.id:
        sid = ref.id
        if (
            ref.offer_id
            and offer_session is not None
            and _tip_of(db, _offered_session(ref, offer_session)) != _tip_of(db, sid)
        ):
            raise BadRequest("the offer names another session", field="session_ref.offer_id")
    elif ref.offer_id:
        sid = _offered_session(ref, offer_session)
    elif ref.latest:
        sid = _latest_session_id(db)
    elif calling_session:
        sid = calling_session
    else:
        raise BadRequest("no session was named", field="session_ref")
    tip = _tip_of(db, sid)
    lineage = tuple(db.get_compression_lineage(tip)) or (tip,)
    row = db.get_session(tip)
    if row is None:
        raise NotFound("session not found", session_id=tip)
    return SessionInfo(
        tip_id=tip,
        root_id=lineage[0],
        lineage_ids=lineage,
        title=row.get("title"),
        model=row.get("model"),
        provider=_provider_of(row),
        cwd=row.get("cwd"),
        git_repo_root=row.get("git_repo_root"),
        git_branch=row.get("git_branch"),
        active_messages=int(db.conversation_revision(tip)[1]),
        revision=_freeze(db.conversation_revision(tip, include_ancestors=True)),
    )


def _freeze(value: object) -> object:
    if isinstance(value, (list, tuple)):
        return tuple(_freeze(v) for v in value)
    return value


def iter_segment_rows(db, session_id: str, *, after_id: int = 0, batch: int = _READ_BATCH) -> Iterator[dict]:
    """Every stored row of one segment in storage order, read in keyset pages (bounded memory)."""
    last = after_id
    while True:
        rows = db.get_messages(session_id, include_inactive=True, after_id=last, limit=batch)
        if not rows:
            return
        yield from rows
        last = int(rows[-1]["id"])
        if len(rows) < batch:
            return


def coalesce_call_id(tc: object) -> str:
    """The core's pairing key of one decoded tool call (stdlib mirror of
    ``agent.message_sanitization.coalesce_tool_call_id``, ``agent/message_sanitization.py:551-559``): ``call_id``
    before ``id``, stripped, a bridge suffix ``|…`` cut; ``""`` when the entry names none."""
    if not isinstance(tc, Mapping):
        return ""
    for key in ("call_id", "id"):
        raw = tc.get(key)
        value = raw.strip() if isinstance(raw, str) else ""
        if value:
            return value.split("|", 1)[0].strip() or value
    return ""


def uid_map(value: object) -> dict:
    """A stored ``{call id: uid | [uid, …]}`` map as the core's insert keeps it (stdlib mirror of
    ``hermes_state_identity._uid_map``, ``hermes_state_identity.py:28-37``): JSON text decoded, invalid entries
    dropped, anything else ``{}``."""
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except ValueError:
            value = {}
    if not isinstance(value, dict):
        return {}
    return {
        k: v
        for k, v in value.items()
        if isinstance(k, str)
        and k
        and ((isinstance(v, str) and v) or (isinstance(v, list) and v and all(isinstance(u, str) and u for u in v)))
    }


def _derived_uid(*parts: object) -> str:
    text = "\n".join((LEGACY_UID_ALGO, *(str(p) for p in parts)))
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:32]


_INVALID = object()


def _decoded(value: object) -> object:
    if isinstance(value, str):
        try:
            return json.loads(value)
        except ValueError:
            return _INVALID
    return value


def complete_identity(row: dict, session_id: str) -> bool:
    """Give ``row`` (in place) every identity the target's insert would otherwise mint at random; ``True`` when
    it changed something.

    * ``message_uid`` missing or not a non-empty string ⇒ ``sha256(algo, session id, row id)[:32]`` (the shape of
      the core's ids, ``lower(hex(randomblob(16)))``);
    * an assistant row's ``tool_calls`` name a pairing key without an entry in ``tool_call_uids`` ⇒ one derived
      uid per key (a key repeated inside the row shares it, as at the core's insert,
      ``hermes_state_messages.py:306-322``); the map is written in the core's column form
      (``json.dumps(…, sort_keys=True)``, ``NULL`` when empty) only when it differs from the stored one.

    Rows that already carry the core's identities are left untouched (byte-true). ``tool_call_uid`` of a result
    row stays as stored: the target derives it from the pairing and the hashes leave it out
    (``session_hash.ALL_DERIVED``).
    """
    changed = False
    sid = str(row.get("session_id") or session_id)
    rid = str(row.get("id"))
    uid = row.get("message_uid")
    if not (isinstance(uid, str) and uid):
        row["message_uid"] = _derived_uid("message", sid, rid)
        changed = True
    stored = row.get("tool_call_uids")
    uids = uid_map(stored)
    if row.get("role") == "assistant":
        calls = _decoded(row.get("tool_calls"))
        if isinstance(calls, list):
            for tc in calls:
                key = coalesce_call_id(tc)
                if key and key not in uids:
                    uids[key] = _derived_uid("tool_call", sid, rid, key)
    if _decoded(stored) != (uids or None):
        row["tool_call_uids"] = json.dumps(uids, sort_keys=True) if uids else None
        changed = True
    return changed


def export_rows(db, session_id: str, *, after_id: int = 0, stats: dict | None = None) -> Iterator[dict]:
    """Rows of one segment as the capsule, the delta and every source-side hash see them: ``iter_segment_rows``
    with ``complete_identity`` applied; ``stats["identity_completed"]`` counts the rows it changed."""
    for row in iter_segment_rows(db, session_id, after_id=after_id):
        if complete_identity(row, session_id) and stats is not None:
            stats["identity_completed"] = int(stats.get("identity_completed", 0)) + 1
        yield row


def row_object(row: Mapping) -> dict:
    """The capsule's copy of a session row: every column except ``ROW_OBJECT_DROP`` and in-memory ``_`` keys."""
    return {k: v for k, v in row.items() if k not in ROW_OBJECT_DROP and not str(k).startswith("_")}


def _side_state(db, tip: str) -> dict:
    out: dict = {}
    for key in SIDE_STATE_KEYS:
        raw = db.get_meta(f"{key}:{tip}")
        value = None
        if raw:
            try:
                parsed = json.loads(raw)
            except ValueError:
                parsed = None
            value = parsed if isinstance(parsed, dict) else None
        out[key] = value
    return out


class _Pager:
    """Groups rows into pages of ≤ ``page_rows`` rows and ≤ ``page_bytes`` canonical bytes (a larger row alone)."""

    def __init__(self, store, page_rows: int, page_bytes: int):
        self.store = store
        self.page_rows = max(1, int(page_rows))
        self.page_bytes = max(1, int(page_bytes))
        self.buf: list[dict] = []
        self.buf_bytes = 2
        self.pages: list[str] = []
        self.bytes = 0

    def add(self, row: dict) -> None:
        size = len(canonical_bytes(row)) + 1
        if self.buf and (len(self.buf) >= self.page_rows or self.buf_bytes + size > self.page_bytes):
            self.flush()
        self.buf.append(row)
        self.buf_bytes += size

    def flush(self) -> None:
        if not self.buf:
            return
        manifest = put_json(self.store, self.buf)
        self.pages.append(manifest.object_id)
        self.bytes += manifest.size
        self.buf = []
        self.buf_bytes = 2


def _segment_bytes(row_obj: Mapping, rows_bytes: int) -> int:
    """Estimate of the core's import measure (compact JSON of the segment without ``timings``)."""
    return len(json.dumps(row_obj, ensure_ascii=False, separators=(",", ":"), default=str).encode()) + rows_bytes


def export_capsule(
    db,
    info: SessionInfo,
    *,
    job_id: str,
    store,
    source_meta: dict,
    hermes: HermesPaths,
    page_rows: int = DEFAULT_PAGE_ROWS,
    page_bytes: int = DEFAULT_PAGE_BYTES,
    cancelled: Callable[[], bool] = lambda: False,
) -> CapsuleInfo:
    """Export the lineage of ``info`` into ``store`` and return the capsule's identity and hashes.

    Revision bracket [SK H2, R3]: the lineage and ``conversation_revision`` are read before the first and after
    the last row; any difference raises ``StaleSession`` (the caller re-exports and counts the restart).
    """
    tip = info.tip_id
    rev_before = _freeze(db.conversation_revision(tip, include_ancestors=True))
    lineage = tuple(db.get_compression_lineage(tip)) or (tip,)
    if lineage != tuple(info.lineage_ids):
        raise StaleSession("the session was compacted after it was locked", reason="lineage")
    h_all_stream = HashStream(ALL_ALGO)
    h_rows_stream = HashStream(ROWS_ALGO)
    segments: list[dict] = []
    refs: dict[str, dict] = {}
    total_rows = 0
    total_bytes = 0
    max_segment_bytes = 0
    approx_chars = 0
    completion: dict = {"identity_completed": 0}
    for seg_id in lineage:
        if cancelled():
            raise Cancelled("export cancelled")
        seg_row = db.get_session(seg_id)
        if seg_row is None:
            raise StaleSession(
                "a part of the session history disappeared during the export", reason="lineage", session_id=seg_id
            )
        obj = row_object(seg_row)
        pager = _Pager(store, page_rows, page_bytes)
        counts = {"rows": 0, "active": 0, "archived": 0, "rewind": 0}
        rows_bytes = 0
        batch: list[dict] = []
        for row in export_rows(db, seg_id, stats=completion):
            counts["rows"] += 1
            if is_rewind_row(row):
                counts["rewind"] += 1
            else:
                h_all_stream.add(row)
                if row.get("active", 1):
                    counts["active"] += 1
                else:
                    counts["archived"] += 1
            if seg_id == tip and is_live_model_row(row):
                h_rows_stream.add(row)
                approx_chars += _chars(row.get("content")) + _chars(row.get("api_content"))
            rows_bytes += len(json.dumps(row, ensure_ascii=False, separators=(",", ":"), default=str).encode()) + 1
            pager.add(row)
            batch.append(row)
            if len(batch) >= _READ_BATCH:
                _collect_refs(refs, batch, hermes)
                batch = []
                if cancelled():
                    raise Cancelled("export cancelled")
        _collect_refs(refs, batch, hermes)
        pager.flush()
        seg_bytes = _segment_bytes(obj, rows_bytes)
        max_segment_bytes = max(max_segment_bytes, seg_bytes)
        total_rows += counts["rows"]
        total_bytes += pager.bytes
        segments.append({"row_object": obj, "pages": pager.pages, "bytes": seg_bytes, **counts})
    for ref in hermes_files.references((), lineage, hermes):
        refs.setdefault(ref["source_path"], ref)
    conversation = db.get_resume_conversations(tip)[0]
    model_hash = h_model(conversation)
    side_state = _side_state(db, tip)
    rev_after = _freeze(db.conversation_revision(tip, include_ancestors=True))
    lineage_after = tuple(db.get_compression_lineage(tip)) or (tip,)
    if rev_after != rev_before or lineage_after != lineage:
        raise StaleSession("the conversation changed during the export", reason="revision")
    hashes = {
        "h_all": h_all_stream.hexdigest(),
        "h_rows": h_rows_stream.hexdigest(),
        "h_model": model_hash,
        "algos": [ALL_ALGO, ROWS_ALGO, MODEL_ALGO],
    }
    capsule = {
        "kind": "ctxh.capsule",
        "v": 1,
        "job_id": job_id,
        "source": {**dict(source_meta), "hermes_home": hermes.home, "document_cache": hermes.document_cache},
        "session": {
            "tip_id": tip,
            "root_id": lineage[0],
            "lineage_ids": list(lineage),
            "title": info.title,
            "revision": rev_after,
        },
        "segments": segments,
        "state_meta": side_state,
        "hermes_file_refs": list(refs.values()),
        "hashes": hashes,
        "stats": {
            "rows": total_rows,
            "bytes": total_bytes,
            "max_segment_bytes": max_segment_bytes,
            "approx_tokens": approx_chars // 4,
            "identity_completed": completion["identity_completed"],
        },
    }
    manifest = put_json(store, capsule)
    return CapsuleInfo(
        capsule_id=manifest.object_id,
        size=manifest.size + total_bytes,
        segments=len(segments),
        rows=total_rows,
        max_segment_bytes=max_segment_bytes,
        approx_tokens=approx_chars // 4,
        h_all=hashes["h_all"],
        h_rows=hashes["h_rows"],
        h_model=hashes["h_model"],
        revision=rev_after,
    )


def _chars(value: object) -> int:
    if isinstance(value, str):
        return len(value)
    if value is None:
        return 0
    return len(json.dumps(value, ensure_ascii=False, default=str))


def _collect_refs(refs: dict, rows: Sequence[dict], hermes: HermesPaths) -> None:
    if not rows:
        return
    for ref in hermes_files.references(rows, (), hermes):
        refs.setdefault(ref["source_path"], ref)


def _appended_rows(db, tip: str, base: tuple, now: tuple) -> list[dict] | None:
    """The tip rows appended since ``base`` when the change is append-only, else ``None``."""
    try:
        ids_old, count_old, max_old, trev_old = base
        ids_new, count_new, max_new, trev_new = now
    except (TypeError, ValueError):
        return None
    if tuple(ids_old) != tuple(ids_new) or trev_old != trev_new or count_new <= count_old or max_new <= max_old:
        return None
    above = [r for r in export_rows(db, tip, after_id=int(max_old)) if r.get("active", 1)]
    if len(above) != count_new - count_old:
        return None
    return above


def _lineage(db, tip: str) -> tuple[str, ...]:
    """The compression lineage containing ``tip`` now (a rotation after the lock appends a child)."""
    return tuple(db.get_compression_lineage(tip)) or (tip,)


def check_revision(db, info: SessionInfo, exported: CapsuleInfo) -> tuple[PrepareOutcome, list[dict]]:
    """Compare the live revision with the exported one: unchanged ⇒ ``ok``; only new live rows on the tip
    (same lineage, same rewrite counter, count and max id grown by exactly those rows) ⇒ ``delta`` with the rows;
    anything else (compaction, rewind of exported rows, rotation, in-place rewrite) ⇒ ``stale_session``. A late
    row that was appended and taken back again leaves the exported live set unchanged ⇒ ``ok``."""
    if _lineage(db, info.tip_id) != tuple(info.lineage_ids):
        return PrepareOutcome.STALE_SESSION, []
    now = _freeze(db.conversation_revision(info.tip_id, include_ancestors=True))
    base = _freeze(exported.revision)
    if now == base:
        return PrepareOutcome.OK, []
    rows = _appended_rows(db, info.tip_id, base, now)
    if rows is None:
        return PrepareOutcome.STALE_SESSION, []
    return PrepareOutcome.DELTA, rows


def _hashes_now(db, tip: str, lineage: Sequence[str]) -> tuple[str, str, str]:
    all_stream = HashStream(ALL_ALGO)
    rows_stream = HashStream(ROWS_ALGO)
    for seg in lineage:
        for row in export_rows(db, seg):
            if not is_rewind_row(row):
                all_stream.add(row)
            if seg == tip and is_live_model_row(row):
                rows_stream.add(row)
    return all_stream.hexdigest(), rows_stream.hexdigest(), h_model(db.get_resume_conversations(tip)[0])


def export_delta(db, info: SessionInfo, exported: CapsuleInfo, new_rows: list[dict], *, store) -> DeltaInfo:
    """Write the ``ctxh.delta`` object: the tip rows appended since the capsule and the hashes of the whole
    lineage after them. The rows are re-read inside a revision bracket; ``new_rows`` from ``check_revision`` must
    name the same row ids, else ``StaleSession``. The expected hashes come from the store as it is now (identities
    completed as in the capsule): when the core's backfill minted its own ids onto legacy rows after the export
    (no revision step), they differ from the target's and ``apply_delta`` raises ``VerifyFailed``: the conductor
    aborts before the commit point and releases the source (no restart); a new handover starts from a fresh export.
    Never a silent mismatch."""
    tip = info.tip_id
    rev_before = _freeze(db.conversation_revision(tip, include_ancestors=True))
    base = _freeze(exported.revision)
    rows = _appended_rows(db, tip, base, rev_before) if _lineage(db, tip) == tuple(info.lineage_ids) else None
    if rows is None or [int(r["id"]) for r in rows] != [int(r["id"]) for r in new_rows]:
        raise StaleSession("the conversation changed in more than new messages", reason="revision")
    lineage = tuple(db.get_compression_lineage(tip)) or (tip,)
    h_all_now, h_rows_now, h_model_now = _hashes_now(db, tip, lineage)
    rev_after = _freeze(db.conversation_revision(tip, include_ancestors=True))
    if rev_after != rev_before:
        raise StaleSession("the conversation changed while its new messages were read", reason="revision")
    doc = {
        "kind": "ctxh.delta",
        "v": 1,
        "tip_id": tip,
        "base_revision": base,
        "revision": rev_after,
        "rows": rows,
        "hashes": {"h_all": h_all_now, "h_rows": h_rows_now, "h_model": h_model_now},
    }
    manifest = put_json(store, doc)
    return DeltaInfo(
        delta_id=manifest.object_id,
        tip_id=tip,
        rows=len(rows),
        h_all=h_all_now,
        h_rows=h_rows_now,
        h_model=h_model_now,
        revision=rev_after,
    )


def read_capsule(
    store,
    capsule_id: str,
    *,
    max_json_bytes: int,
    max_segments: int = MAX_SEGMENTS,
    max_pages: int = MAX_PAGES,
    max_object_bytes: int = MAX_OBJECT_BYTES,
    max_session_bytes: int = 512 * 1024**2,
) -> dict:
    """The capsule document, validated against the receiver caps before any page is parsed [SEC 2.5]."""
    return read_capsule_doc(
        store,
        capsule_id,
        max_json_bytes=max_json_bytes,
        max_segments=max_segments,
        max_pages=max_pages,
        max_object_bytes=max_object_bytes,
        max_session_bytes=max_session_bytes,
    )


def iter_capsule_rows(store, capsule_id: str, *, max_json_bytes: int = MAX_OBJECT_BYTES) -> Iterator[tuple[int, dict]]:
    """``(segment index, row)`` for every row of the capsule in lineage and storage order."""
    capsule = read_capsule(store, capsule_id, max_json_bytes=max_json_bytes)
    for index, segment in enumerate(capsule["segments"]):
        for page_id in segment["pages"]:
            page = read_json(store, page_id, max_bytes=MAX_OBJECT_BYTES)
            if not isinstance(page, list):
                raise BadRequest("capsule page is not a list", field=f"segments[{index}].pages")
            for row in page:
                if not isinstance(row, dict):
                    raise BadRequest("capsule row is not an object", field=f"segments[{index}].pages")
                yield index, row
