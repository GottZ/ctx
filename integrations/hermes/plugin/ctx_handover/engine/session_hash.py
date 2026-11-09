"""Verification hashes of a session transfer (layer L2, stdlib only; DESIGN §6.9, INTERFACES §4.1).

Three digests decide whether a lineage arrived intact:

* **H_all** (``ctxh-all-v1``): every row of every segment in lineage order and storage order, except rewind
  rows (``active=0, compacted=0``), over every exported field except ``id``, ``session_id``,
  ``platform_message_id`` and the core-derived ``tool_call_uid`` (``ALL_DERIVED``) — the gate for both import
  paths and after every delta.
* **H_rows** (``ctxh-rows-v1``): the tip's live rows (``active=1``, ``role != session_meta``) over the model fields.
* **H_model** (``ctxh-model-v1``): the core's model history (``get_resume_conversations(tip)[0]``) over the
  same fields.

Normalisation ``ctxh-norm-v1`` of one row: the selected fields, JSON-typed fields parsed when they arrive as
text, structured content decoded from the core's ``"\\x00json:"`` prefix, ``None`` dropped, flag fields
reduced to ``1`` (and dropped when false), integral floats written as integers. A row digest is the sha256 of
the row's canonical JSON; ``H = sha256(algo + "\\n" + count + "\\n" + Σ (row_digest + "\\n"))``.

The accumulator keeps 32 raw bytes per row (the count prefix needs the total before the row digests), so
memory stays at 32 bytes per row — 3.2 MB for 100 000 rows — instead of holding rows.
"""

from __future__ import annotations

import hashlib
import json
import math
from collections.abc import Iterable, Mapping

from . import jsonutil
from .errors import BadRequest, BlockCorrupt, NotFound, PayloadTooLarge
from .jsonutil import canonical_bytes, loads_bounded, sha256_hex
from .model import ObjectManifest

ALL_ALGO = "ctxh-all-v1"
ROWS_ALGO = "ctxh-rows-v1"
MODEL_ALGO = "ctxh-model-v1"
NORM_ALGO = "ctxh-norm-v1"

ROWS_FIELDS: tuple[str, ...] = (
    "role",
    "content",
    "tool_call_id",
    "tool_calls",
    "tool_name",
    "reasoning",
    "reasoning_content",
    "reasoning_details",
    "codex_reasoning_items",
    "codex_message_items",
    "api_content",
    "_compressed_summary",
    "display_kind",
)
"""Model-facing fields of H_rows and H_model (DESIGN §6.9)."""

ALL_EXCLUDED: frozenset[str] = frozenset({"id", "session_id", "platform_message_id"})
"""Storage coordinates H_all leaves out; every other exported field is covered."""

ALL_DERIVED: frozenset[str] = frozenset({"tool_call_uid"})
"""Fields the core derives on insert and H_all therefore leaves out as well.

``tool_call_uid`` of a tool-result row is the uid its assistant row minted for the call; every batch insert
(``import_sessions``, ``append_messages_batch``, ``archive_and_compact``) fills a NULL one from the pairing within
the same batch (``_stamp_tool_call_uids``, ``hermes_state_messages.py:306-327``), while a single ``append_message``
leaves it NULL. A present value is kept byte-true (``_tool_call_uid_or_none``); a NULL one may come back filled,
and the filled value depends on how the import path batches. Both inputs of the pairing — the assistant row's
``tool_call_uids`` and the result's ``tool_call_id`` — stay inside H_all.
"""

JSON_FIELDS: frozenset[str] = frozenset(
    {
        "tool_calls",
        "reasoning_details",
        "codex_reasoning_items",
        "codex_message_items",
        "absorbed_message_uids",
        "tool_call_uids",
        "display_metadata",
    }
)
"""Fields the core stores as JSON text and decodes on some read paths but not on others."""

FLAG_FIELDS: frozenset[str] = frozenset({"active", "compacted", "observed", "_compressed_summary"})
"""Boolean columns; ``True``/``1`` and ``False``/``0``/absent compare equal across read paths."""

CONTENT_JSON_PREFIX = "\x00json:"
"""Prefix of structured message content in the ``messages.content`` column (``hermes_state.py:1633``)."""

_STRUCTURAL_EXCLUDED: frozenset[str] = frozenset({"display_identity", "display_order"})
"""Columns the core never exports (``hermes_state_messages.py:1312-1313``); dropped if a raw row carries them."""


def _decode_json_text(value: object) -> object:
    if isinstance(value, str):
        try:
            return json.loads(value)
        except ValueError:
            return value
    return value


def _decode_content(value: object) -> object:
    if isinstance(value, str) and value.startswith(CONTENT_JSON_PREFIX):
        try:
            return json.loads(value[len(CONTENT_JSON_PREFIX) :])
        except ValueError:
            return value
    return value


def _canon_value(value: object) -> object:
    """Recursively normalise numbers (integral finite floats → int) and drop ``None`` inside mappings."""
    if isinstance(value, bool) or value is None or isinstance(value, str):
        return value
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        if math.isfinite(value) and value.is_integer():
            return int(value)
        return value
    if isinstance(value, Mapping):
        return {str(k): _canon_value(v) for k, v in value.items() if v is not None}
    if isinstance(value, (list, tuple)):
        return [_canon_value(v) for v in value]
    return str(value)


def normalize_row(row: Mapping, fields: Iterable[str] | None = None) -> dict:
    """``ctxh-norm-v1`` of one message row.

    ``fields=None`` selects every field except ``ALL_EXCLUDED`` and ``ALL_DERIVED`` (the H_all view); otherwise
    exactly ``fields``.
    Keys starting with ``_row`` and the core's in-memory markers never take part.
    """
    keys = list(fields) if fields is not None else [k for k in row if k not in ALL_EXCLUDED and k not in ALL_DERIVED]
    out: dict = {}
    for key in keys:
        if key in _STRUCTURAL_EXCLUDED or key.startswith("_row") or key == "_db_persisted":
            continue
        if key not in row:
            continue
        value = row[key]
        if key == "content":
            value = _decode_content(value)
        elif key in JSON_FIELDS:
            value = _decode_json_text(value)
        if key in FLAG_FIELDS:
            if value in (None, "", "0"):
                continue
            if isinstance(value, str):
                try:
                    value = int(value)
                except ValueError:
                    value = 1
            if not value:
                continue
            out[key] = 1
            continue
        if value is None:
            continue
        out[key] = _canon_value(value)
    return out


def row_digest(row: Mapping, fields: Iterable[str] | None = None) -> bytes:
    """Raw sha256 of the canonical JSON of ``normalize_row(row, fields)``."""
    data = json.dumps(
        normalize_row(row, fields), sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False
    )
    return hashlib.sha256(data.encode("utf-8")).digest()


class HashStream:
    """Streaming accumulator of one verification hash (32 bytes of state per row)."""

    def __init__(self, algo: str, fields: Iterable[str] | None = None):
        if algo not in (ALL_ALGO, ROWS_ALGO, MODEL_ALGO):
            raise ValueError(f"unknown hash algorithm {algo!r}")
        self.algo = algo
        if fields is not None:
            self._fields: tuple[str, ...] | None = tuple(fields)
        else:
            self._fields = None if algo == ALL_ALGO else ROWS_FIELDS
        self._digests = bytearray()
        self.count = 0

    def add(self, row: Mapping) -> None:
        """Add one row (already filtered by the caller)."""
        self._digests += row_digest(row, self._fields)
        self.count += 1

    def hexdigest(self) -> str:
        h = hashlib.sha256(f"{self.algo}\n{self.count}\n".encode())
        view = memoryview(self._digests)
        for i in range(0, len(view), 32):
            h.update(view[i : i + 32].hex().encode("ascii") + b"\n")
        return h.hexdigest()


def is_rewind_row(row: Mapping) -> bool:
    """A row the user took back or a compaction superseded (``active=0, compacted=0``)."""
    return not _flag(row.get("active", 1)) and not _flag(row.get("compacted"))


def is_live_model_row(row: Mapping) -> bool:
    """A live tip row that reaches the model (``active=1``, not the gateway's ``session_meta`` marker)."""
    return _flag(row.get("active", 1)) and row.get("role") != "session_meta"


def _flag(value: object) -> bool:
    if value is None:
        return False
    if isinstance(value, str):
        return value not in ("", "0")
    return bool(value)


def h_all(segments_rows: Iterable[Iterable[Mapping]]) -> str:
    """H_all over all segments (lineage order) and rows (storage order); rewind rows are skipped."""
    stream = HashStream(ALL_ALGO)
    for rows in segments_rows:
        for row in rows:
            if not is_rewind_row(row):
                stream.add(row)
    return stream.hexdigest()


def h_rows(tip_rows: Iterable[Mapping]) -> str:
    """H_rows over the tip's live model rows."""
    stream = HashStream(ROWS_ALGO)
    for row in tip_rows:
        if is_live_model_row(row):
            stream.add(row)
    return stream.hexdigest()


def h_model(conversation: Iterable[Mapping]) -> str:
    """H_model over the core's model history (every message, model fields)."""
    stream = HashStream(MODEL_ALGO)
    for message in conversation:
        stream.add(message)
    return stream.hexdigest()


# ---------------------------------------------------------------------------------------------------------
# Object codec of session documents (capsule, pages, delta)
#
# ``session_export`` and ``session_import`` are both layer L4 and may not import each other (INTERFACES §2);
# the few helpers both need to write and read content-addressed JSON objects through the ``BlockSink`` /
# ``BlockSource`` protocols live here, one layer below. ``session_export.read_capsule``/``iter_capsule_rows``
# are the public names (INTERFACES §4.1) and delegate to these. Object ids, block size and the manifest check
# are K0's one rule (``jsonutil.object_manifest``/``object_id``/``check_object_manifest``, contract K-1).
# ---------------------------------------------------------------------------------------------------------

MEDIA_JSON = "application/json"
MAX_SEGMENTS = 500
"""Segments per capsule (the core's ``_IMPORT_MAX_SESSIONS``, ``hermes_state.py:510``; SEC 2.5)."""
MAX_PAGES = 10_000
"""Pages per capsule (SEC 2.5)."""
MAX_OBJECT_BYTES = 64 * 1024**2
"""Largest JSON object (page, capsule, delta) a receiver parses (SEC 2.5)."""


def _manifest_doc(m: ObjectManifest) -> dict:
    return jsonutil.object_manifest_doc(m.sha256, m.size, m.block_size, m.blocks, m.media)


def put_object(store, data: bytes, media: str = MEDIA_JSON):
    """Store ``data`` as one object in ``store`` and return its ``ObjectManifest``.

    A store with ``put_bytes`` (cluster C's ``BlockStore``) builds the manifest itself; any other ``BlockSink``
    gets the blocks through ``write_blocks`` and the manifest through ``put_manifests``. Both follow K0's rule
    (``jsonutil.object_manifest``: 524 288-byte blocks, an empty object has none; ``jsonutil.object_id``), so
    the same bytes have the same id in every store.
    """
    put_bytes = getattr(store, "put_bytes", None)
    if callable(put_bytes):
        return put_bytes(data, media)
    doc, blocks = jsonutil.object_manifest(data, media)
    accepted, rejected = store.write_blocks(blocks)
    if rejected:
        raise BlockCorrupt("block store refused session blocks", rejected=[list(r) for r in rejected][:8])
    del accepted
    manifest = ObjectManifest(
        doc["sha256"], doc["size"], doc["block_size"], tuple(doc["blocks"]), media, jsonutil.object_id(doc)
    )
    store.put_manifests([manifest])
    return manifest


def put_json(store, doc: object):
    """Store ``doc`` as canonical JSON (``jsonutil.canonical_bytes``); returns its ``ObjectManifest``."""
    return put_object(store, canonical_bytes(doc), MEDIA_JSON)


def object_manifest(store, object_id: str):
    """The manifest of ``object_id`` from a ``BlockSource`` (``NotFound`` when unknown)."""
    found = store.manifests([object_id])
    if not found:
        raise NotFound("session object not found", object_id=object_id)
    return found[0]


def read_object(store, object_id: str, *, max_bytes: int):
    """Bytes of ``object_id``. Before any block is read the manifest must be the one ``object_id`` names
    (``jsonutil.check_object_manifest``: v1 shape, block size 524 288, recomputed id = ``object_id``, else
    ``BadRequest``/``BlockCorrupt``) and its size within ``max_bytes`` (``PayloadTooLarge``).

    A store with ``read_object_bytes`` (cluster C's ``BlockStore``, W061) reads and verifies the blocks itself;
    otherwise every block is checked against its id here. Either way the content must match the manifest's
    size and sha256 (I8).
    """
    manifest = object_manifest(store, object_id)
    jsonutil.check_object_manifest(_manifest_doc(manifest), object_id)
    if manifest.size > max_bytes:
        raise PayloadTooLarge(
            "session object exceeds the receiver limit", object_id=object_id, size=manifest.size, limit=max_bytes
        )
    read_object_bytes = getattr(store, "read_object_bytes", None)
    if callable(read_object_bytes):
        data = read_object_bytes(object_id, max_bytes=max_bytes)
    else:
        data = _read_blocks(store, manifest)
    if len(data) != manifest.size or sha256_hex(data) != manifest.sha256:
        raise BlockCorrupt("session object does not match its manifest", object_id=object_id)
    return data


def _read_blocks(store, manifest: ObjectManifest) -> bytes:
    wanted = list(dict.fromkeys(manifest.blocks))
    got: dict[str, bytes] = {}
    for block_id, raw in store.read_blocks(wanted):
        if block_id in got or block_id not in wanted:
            continue
        if raw is None or sha256_hex(raw) != block_id:
            raise BlockCorrupt("session block does not match its id", block_id=block_id)
        got[block_id] = raw
    missing = [b for b in wanted if b not in got]
    if missing:
        raise BlockCorrupt("session blocks missing", missing=missing[:8], count=len(missing))
    return b"".join(got[b] for b in manifest.blocks)


def read_json(store, object_id: str, *, max_bytes: int) -> object:
    """Parse ``object_id`` with ``jsonutil.loads_bounded`` after the size check of :func:`read_object`."""
    return loads_bounded(read_object(store, object_id, max_bytes=max_bytes), max_bytes=max_bytes)


def read_capsule_doc(
    store,
    capsule_id: str,
    *,
    max_json_bytes: int,
    max_segments: int = MAX_SEGMENTS,
    max_pages: int = MAX_PAGES,
    max_object_bytes: int = MAX_OBJECT_BYTES,
    max_session_bytes: int = 512 * 1024**2,
) -> dict:
    """Read and validate a ``ctxh.capsule`` document; caps are enforced before any page is read [SEC 2.5].

    Order: capsule object size (manifest) ≤ ``max_json_bytes`` before its bytes are read; shape; segment count
    ≤ ``max_segments``; page count ≤ ``max_pages``; then every page manifest — each ≤ ``max_object_bytes`` and
    together ≤ ``max_session_bytes`` — before a single page block is fetched or parsed.
    """
    doc = read_json(store, capsule_id, max_bytes=max_json_bytes)
    if not isinstance(doc, dict) or doc.get("kind") != "ctxh.capsule" or doc.get("v") != 1:
        raise BadRequest("not a ctxh.capsule v1 document", field="capsule")
    segments = doc.get("segments")
    if not isinstance(segments, list) or not segments:
        raise BadRequest("capsule has no segments", field="capsule.segments")
    if len(segments) > max_segments:
        raise PayloadTooLarge("capsule has too many segments", count=len(segments), limit=max_segments)
    page_ids: list[str] = []
    for index, segment in enumerate(segments):
        if not isinstance(segment, dict) or not isinstance(segment.get("row_object"), dict):
            raise BadRequest("capsule segment is malformed", field=f"capsule.segments[{index}]")
        pages = segment.get("pages")
        if not isinstance(pages, list) or any(not isinstance(p, str) for p in pages):
            raise BadRequest("capsule segment pages are malformed", field=f"capsule.segments[{index}].pages")
        page_ids.extend(pages)
        if len(page_ids) > max_pages:
            raise PayloadTooLarge("capsule has too many pages", limit=max_pages)
    for key in ("session", "hashes"):
        if not isinstance(doc.get(key), dict):
            raise BadRequest(f"capsule {key} is malformed", field=f"capsule.{key}")
    total = 0
    for start in range(0, len(page_ids), 4096):
        for manifest in store.manifests(page_ids[start : start + 4096]):
            if manifest.size > max_object_bytes:
                raise PayloadTooLarge(
                    "capsule page exceeds the object limit", size=manifest.size, limit=max_object_bytes
                )
            total += manifest.size
            if total > max_session_bytes:
                raise PayloadTooLarge("capsule exceeds max_session_bytes", limit=max_session_bytes)
    return doc
