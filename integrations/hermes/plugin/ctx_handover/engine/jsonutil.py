"""Canonical JSON, bounded parsing and the dataclass wire codec (layer L1, stdlib only; INTERFACES §3.4).

Canonical JSON (DESIGN §5.3): UTF-8, sorted keys, ``,``/``:`` separators, no NaN/Infinity. Document and
object ids are the sha256 of these bytes (``doc_id``).

Object manifests (DESIGN §4.6, §5.2; contract K-1): ``object_manifest`` splits bytes into blocks and builds the
``ctxh.object`` document, ``object_id`` is the sha256 of that document's canonical bytes, and
``check_object_manifest`` is the one reader-side check (block size exactly ``OBJECT_BLOCK_SIZE``, block count from
the size, id recomputed). Every module that writes or reads objects delegates here.

``loads_bounded`` rejects oversized input and nesting deeper than ``max_depth`` *before* ``json.loads`` runs,
and refuses duplicate keys and non-finite numbers.

Codec: ``to_wire`` turns a dataclass into plain JSON data (enums → value, tuples → lists, frozensets → sorted
lists, nested dataclasses → dicts); ``from_wire(cls, data)`` rebuilds it from the type hints: unknown keys are
ignored, missing keys take the field default, wrong types raise ``BadRequest`` with ``details["field"]`` set to
the field path (``plan.items[2].kind``). A field declared with ``model.added_field`` (added after the v1 wire form,
contract K-3) is left out of ``to_wire``/``canonical_bytes`` while it equals its default, so documents and ids that
never set it keep their bytes.
"""

from __future__ import annotations

import dataclasses
import enum
import functools
import hashlib
import json
import math
import re
import types
import typing
from collections.abc import Iterable, Mapping
from pathlib import PurePath

from .errors import BadRequest, BlockCorrupt
from .model import WIRE_OMIT_DEFAULT

DEFAULT_MAX_DEPTH = 64

# ---------------------------------------------------------------------------------------------------------
# Canonical JSON
# ---------------------------------------------------------------------------------------------------------


def _encode(obj: object) -> object:
    if obj is None or isinstance(obj, (bool, str)):
        return obj
    if isinstance(obj, enum.Enum):
        return obj.value
    if isinstance(obj, int):
        return obj
    if isinstance(obj, float):
        if not math.isfinite(obj):
            raise ValueError("non-finite float is not JSON")
        return obj
    if dataclasses.is_dataclass(obj) and not isinstance(obj, type):
        out = {}
        for f in dataclasses.fields(obj):
            value = getattr(obj, f.name)
            if f.metadata.get(WIRE_OMIT_DEFAULT) and value == f.default:
                continue  # added field at its default: the v1 bytes stay (contract K-3)
            out[f.name] = _encode(value)
        return out
    if isinstance(obj, Mapping):
        out = {}
        for key, value in obj.items():
            if isinstance(key, enum.Enum):
                key = key.value
            if not isinstance(key, str):
                raise TypeError(f"JSON object keys must be str, got {type(key).__name__}")
            out[key] = _encode(value)
        return out
    if isinstance(obj, (list, tuple)):
        return [_encode(v) for v in obj]
    if isinstance(obj, (set, frozenset)):
        items = [_encode(v) for v in obj]
        return sorted(items, key=lambda v: json.dumps(v, sort_keys=True, ensure_ascii=False))
    if isinstance(obj, PurePath):
        return str(obj)
    raise TypeError(f"not JSON-encodable: {type(obj).__name__}")


def canonical_bytes(obj: object) -> bytes:
    """Canonical JSON bytes of ``obj`` (dataclasses are encoded with ``to_wire`` rules)."""
    return json.dumps(_encode(obj), sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False).encode(
        "utf-8"
    )


def sha256_hex(data: bytes) -> str:
    """Lower-case hex sha256 of ``data``."""
    return hashlib.sha256(data).hexdigest()


def doc_id(obj: object) -> str:
    """Id of a document or manifest: sha256 of its canonical bytes (DESIGN §4.6, §5.3)."""
    return sha256_hex(canonical_bytes(obj))


# ---------------------------------------------------------------------------------------------------------
# Object manifests (DESIGN §4.6, §5.2) — the one rule for object ids (contract K-1)
# ---------------------------------------------------------------------------------------------------------

OBJECT_BLOCK_SIZE = 524_288
"""Block size of every object (DESIGN §4.6): writers always use it and readers accept no other value."""
OBJECT_MAX_BYTES = 30 * 1024**3
"""Objects above ≈ 30 GiB are refused (DESIGN §4.6); ``max_total_gb`` caps far below."""
OBJECT_MEDIA_MAX = 200
"""Longest accepted ``media`` string of a manifest."""
OBJECT_KIND = "ctxh.object"

_MANIFEST_KEYS = frozenset({"kind", "v", "sha256", "size", "block_size", "blocks", "media"})
_HEX64 = re.compile(r"[0-9a-f]{64}")


def object_manifest_doc(sha256: str, size: int, block_size: int, blocks: Iterable[str], media: str) -> dict:
    """The canonical ``ctxh.object`` manifest document (DESIGN §5.2); ``object_id`` is not part of it.

    Streaming writers that hash their blocks themselves build their document here, so the document shape exists
    once. ``object_id(object_manifest_doc(...))`` is the object id.
    """
    return {
        "kind": OBJECT_KIND,
        "v": 1,
        "sha256": sha256,
        "size": int(size),
        "block_size": int(block_size),
        "blocks": list(blocks),
        "media": media,
    }


def object_manifest(
    data: bytes, media: str, block_size: int = OBJECT_BLOCK_SIZE
) -> tuple[dict, list[tuple[str, bytes]]]:
    """Split ``data`` into blocks and return ``(manifest document, [(block_id, raw), …])`` (DESIGN §4.6, §5.2).

    Blocks are consecutive slices of ``block_size`` bytes, the last one shorter; ``block_id = sha256(raw)``. An
    empty object has no blocks (``size`` 0, ``blocks`` ``[]``). ``block_size`` is part of the signature only:
    readers accept nothing but ``OBJECT_BLOCK_SIZE`` (``check_object_manifest``), so any other value raises
    ``ValueError`` instead of producing a manifest nobody can read.
    """
    if isinstance(block_size, bool) or block_size != OBJECT_BLOCK_SIZE:
        raise ValueError(f"objects are written with block size {OBJECT_BLOCK_SIZE}, not {block_size!r}")
    if not isinstance(media, str):
        raise TypeError("media must be a str")
    view = memoryview(data).cast("B")
    blocks: list[tuple[str, bytes]] = []
    for start in range(0, len(view), block_size):
        raw = bytes(view[start : start + block_size])
        blocks.append((sha256_hex(raw), raw))
    doc = object_manifest_doc(
        hashlib.sha256(view).hexdigest(), len(view), block_size, [bid for bid, _ in blocks], media
    )
    return doc, blocks


def object_id(manifest_doc: dict) -> str:
    """Object id of a manifest document: ``sha256_hex(canonical_bytes(manifest_doc))`` (DESIGN §4.6)."""
    return sha256_hex(canonical_bytes(manifest_doc))


def check_object_manifest(doc: object, object_id: str) -> None:
    """Reader-side check of a ``ctxh.object`` manifest document against the id it was asked for by.

    Raises ``BadRequest`` (``details.field``) when ``object_id`` is not 64 lower-case hex or the document is not a
    v1 manifest: exactly the keys of ``object_manifest_doc``, ``sha256`` 64 hex, ``block_size`` exactly
    ``OBJECT_BLOCK_SIZE``, ``0 <= size <= OBJECT_MAX_BYTES``, ``ceil(size / block_size)`` block ids of 64 hex,
    ``media`` a string of at most ``OBJECT_MEDIA_MAX`` characters. Raises ``BlockCorrupt`` (a hash mismatch,
    DESIGN §4.4) when the recomputed ``object_id`` differs from ``object_id``. Returns None when the manifest
    holds; block contents and the content hash are checked by whoever reads the blocks.
    """
    if not isinstance(object_id, str) or not _HEX64.fullmatch(object_id):
        raise BadRequest("malformed object id", field="object_id")
    if not isinstance(doc, dict) or set(doc) != _MANIFEST_KEYS:
        raise BadRequest("not a ctxh.object manifest", field="manifest")
    if doc["kind"] != OBJECT_KIND:
        raise BadRequest("not a ctxh.object manifest", field="manifest.kind")
    version = doc["v"]
    if not isinstance(version, int) or isinstance(version, bool) or version != 1:
        raise BadRequest("unsupported manifest version", field="manifest.v")
    sha = doc["sha256"]
    if not isinstance(sha, str) or not _HEX64.fullmatch(sha):
        raise BadRequest("malformed content hash", field="manifest.sha256")
    block_size = doc["block_size"]
    if not isinstance(block_size, int) or isinstance(block_size, bool) or block_size != OBJECT_BLOCK_SIZE:
        raise BadRequest("unsupported block size", field="manifest.block_size", block_size=str(block_size)[:32])
    size = doc["size"]
    if not isinstance(size, int) or isinstance(size, bool) or not 0 <= size <= OBJECT_MAX_BYTES:
        raise BadRequest("object size out of range", field="manifest.size")
    blocks = doc["blocks"]
    expected = -(-size // OBJECT_BLOCK_SIZE)
    if not isinstance(blocks, (list, tuple)) or len(blocks) != expected:
        raise BadRequest("block count does not match the size", field="manifest.blocks", expected=expected)
    for i, block_id in enumerate(blocks):
        if not isinstance(block_id, str) or not _HEX64.fullmatch(block_id):
            raise BadRequest("malformed block id", field=f"manifest.blocks[{i}]")
    media = doc["media"]
    if not isinstance(media, str) or len(media) > OBJECT_MEDIA_MAX:
        raise BadRequest("invalid media type", field="manifest.media")
    if sha256_hex(canonical_bytes(doc)) != object_id:
        raise BlockCorrupt("object manifest does not match its id", object=object_id)


# ---------------------------------------------------------------------------------------------------------
# Bounded parsing
# ---------------------------------------------------------------------------------------------------------

_STRUCTURAL = re.compile(rb'["\\\[\]{}]')


def _check_depth(data: bytes, max_depth: int) -> None:
    """Reject nesting deeper than ``max_depth`` by scanning structural bytes outside strings (no parse)."""
    depth = 0
    in_string = False
    skip = -1
    for match in _STRUCTURAL.finditer(data):
        pos = match.start()
        if pos == skip:
            continue
        char = data[pos]
        if in_string:
            if char == 0x5C:  # backslash: the next byte is escaped
                skip = pos + 1
            elif char == 0x22:
                in_string = False
            continue
        if char == 0x22:
            in_string = True
        elif char in (0x5B, 0x7B):
            depth += 1
            if depth > max_depth:
                raise BadRequest("JSON nesting too deep", field="$", reason="depth", max_depth=max_depth)
        elif char in (0x5D, 0x7D):
            depth -= 1


def _no_duplicates(pairs: list[tuple[str, object]]) -> dict:
    out: dict = {}
    for key, value in pairs:
        if key in out:
            raise ValueError(f"duplicate key {key!r}")
        out[key] = value
    return out


def _reject_constant(name: str) -> object:
    raise ValueError(f"non-finite number {name}")


def loads_bounded(data: bytes, *, max_bytes: int, max_depth: int = DEFAULT_MAX_DEPTH) -> object:
    """Parse JSON from ``data`` with size and depth caps checked before parsing.

    Raises ``BadRequest`` (``details.reason`` ``too_large`` | ``depth`` | ``invalid``) for oversized input,
    nesting deeper than ``max_depth``, invalid UTF-8 or JSON, duplicate keys and NaN/Infinity.
    """
    if isinstance(data, str):
        data = data.encode("utf-8")
    elif isinstance(data, (bytearray, memoryview)):
        data = bytes(data)
    if not isinstance(data, bytes):
        raise BadRequest("JSON input must be bytes", field="$", reason="invalid")
    if len(data) > max_bytes:
        raise BadRequest("JSON document too large", field="$", reason="too_large", max_bytes=max_bytes)
    _check_depth(data, max_depth)
    try:
        text = data.decode("utf-8")
        return json.loads(text, object_pairs_hook=_no_duplicates, parse_constant=_reject_constant)
    except (UnicodeDecodeError, ValueError, RecursionError) as exc:
        raise BadRequest(f"invalid JSON: {exc}"[:200], field="$", reason="invalid") from None


# ---------------------------------------------------------------------------------------------------------
# Dataclass codec
# ---------------------------------------------------------------------------------------------------------


def to_wire(obj) -> dict:
    """Plain JSON data of a dataclass instance (see module docstring)."""
    if not (dataclasses.is_dataclass(obj) and not isinstance(obj, type)):
        raise TypeError(f"to_wire expects a dataclass instance, got {type(obj).__name__}")
    return typing.cast(dict, _encode(obj))


@functools.cache
def _hints(cls: type) -> dict[str, object]:
    return typing.get_type_hints(cls)


def _bad(path: str, expected: str, value: object) -> BadRequest:
    return BadRequest(f"{path}: expected {expected}, got {type(value).__name__}", field=path)


def _freeze(value: object, path: str) -> object:
    """Bare ``tuple`` fields (revisions): lists become tuples recursively, scalars stay."""
    if isinstance(value, (list, tuple)):
        return tuple(_freeze(v, f"{path}[{i}]") for i, v in enumerate(value))
    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    if isinstance(value, dict):
        return _plain_dict(value, path)
    raise _bad(path, "JSON value", value)


def _plain_dict(value: object, path: str) -> dict:
    if not isinstance(value, dict):
        raise _bad(path, "object", value)
    for key in value:
        if not isinstance(key, str):
            raise _bad(path, "object with string keys", key)
    return dict(value)


def _decode(tp: object, value: object, path: str) -> object:
    origin = typing.get_origin(tp)
    args = typing.get_args(tp)

    if origin is typing.Union or origin is types.UnionType:
        if value is None and type(None) in args:
            return None
        options = [a for a in args if a is not type(None)]
        last: BadRequest | None = None
        for option in options:
            try:
                return _decode(option, value, path)
            except BadRequest as exc:
                last = exc
        raise last or _bad(path, "value", value)

    if tp is type(None) or tp is None:
        if value is not None:
            raise _bad(path, "null", value)
        return None
    if tp is typing.Any or tp is object:
        return value
    if tp is bool:
        if not isinstance(value, bool):
            raise _bad(path, "boolean", value)
        return value
    if tp is int:
        if isinstance(value, bool) or not isinstance(value, int):
            raise _bad(path, "integer", value)
        return value
    if tp is float:
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise _bad(path, "number", value)
        return float(value)
    if tp is str:
        if not isinstance(value, str):
            raise _bad(path, "string", value)
        return value
    if isinstance(tp, type) and issubclass(tp, enum.Enum):
        if isinstance(value, tp):
            return value
        try:
            return tp(value)
        except (ValueError, TypeError):
            raise BadRequest(f"{path}: invalid {tp.__name__} value {value!r}"[:200], field=path) from None
    if isinstance(tp, type) and dataclasses.is_dataclass(tp):
        return _decode_dataclass(tp, value, path)

    if tp is tuple or origin is tuple:
        if not isinstance(value, (list, tuple)):
            raise _bad(path, "array", value)
        if not args:
            return _freeze(value, path)
        if len(args) == 2 and args[1] is Ellipsis:
            return tuple(_decode(args[0], v, f"{path}[{i}]") for i, v in enumerate(value))
        if len(value) != len(args):
            raise BadRequest(f"{path}: expected {len(args)} elements, got {len(value)}", field=path)
        return tuple(_decode(a, v, f"{path}[{i}]") for i, (a, v) in enumerate(zip(args, value, strict=True)))
    if tp is list or origin is list:
        if not isinstance(value, (list, tuple)):
            raise _bad(path, "array", value)
        if not args:
            return [_freeze(v, f"{path}[{i}]") for i, v in enumerate(value)]
        return [_decode(args[0], v, f"{path}[{i}]") for i, v in enumerate(value)]
    if tp is frozenset or origin is frozenset or origin is set:
        if not isinstance(value, (list, tuple, set, frozenset)):
            raise _bad(path, "array", value)
        item_tp = args[0] if args else typing.Any
        return frozenset(_decode(item_tp, v, f"{path}[{i}]") for i, v in enumerate(value))
    if tp is dict or origin is dict:
        if not args:
            return _plain_dict(value, path)
        if not isinstance(value, dict):
            raise _bad(path, "object", value)
        key_tp, value_tp = args
        out = {}
        for key, item in value.items():
            if not isinstance(key, str):
                raise _bad(path, "object with string keys", key)
            out[_decode(key_tp, key, f"{path}.<key>")] = _decode(value_tp, item, f"{path}.{key}")
        return out
    raise BadRequest(f"{path}: unsupported field type {tp!r}"[:200], field=path)


def _decode_dataclass(cls: type, value: object, path: str) -> object:
    if isinstance(value, cls):
        return value
    if not isinstance(value, dict):
        raise _bad(path, f"object ({cls.__name__})", value)
    hints = _hints(cls)
    kwargs = {}
    for f in dataclasses.fields(cls):
        if not f.init:
            continue
        sub = f"{path}.{f.name}" if path else f.name
        if f.name in value:
            kwargs[f.name] = _decode(hints[f.name], value[f.name], sub)
        elif f.default is dataclasses.MISSING and f.default_factory is dataclasses.MISSING:
            raise BadRequest(f"{sub}: missing required field", field=sub)
    return cls(**kwargs)


def from_wire(cls, data: dict):
    """Rebuild a ``cls`` instance from wire data (see module docstring); ``BadRequest`` on schema violations."""
    if not (isinstance(cls, type) and dataclasses.is_dataclass(cls)):
        raise TypeError(f"from_wire expects a dataclass type, got {cls!r}")
    return _decode_dataclass(cls, data, "")
