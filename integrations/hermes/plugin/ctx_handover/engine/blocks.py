"""Content-addressed block store, object manifests and the block frame codec (layer L2, stdlib only).

DESIGN §4.6, §5.1, §5.2. Everything that moves between two hosts — online or through a bundle — is an *object*
(ordered blocks + size + content sha256) described by an ``ObjectManifest``; ``object_id`` is the sha256 of the
canonical manifest document (``manifest_doc``). A *block* is at most ``BLOCK_SIZE`` raw bytes and its id is
``sha256(raw)``. The integrity chain is: object id → manifest → block ids → block hashes → content sha256.

On disk (``root`` = the plugin data directory)::

    blocks/sha256/<2 hex>/<62 hex>     raw bytes, 0600, written temp → fsync → os.replace
    blocks/tmp/                        temp files on the same filesystem
    manifests/<2 hex>/<62 hex>.json    canonical manifest document, 0600

Directories are created 0700 before the first file (SEC 6.1). The ledger (cluster A) indexes blocks and objects
and records which jobs use which objects; GC frees blocks no non-terminal job needs (DESIGN §5.1, SK M3) and
removes the manifest files and ``objects`` rows of objects no non-terminal job references any more — those of
finished and aborted jobs and unreferenced ones (R28). Storing a manifest and collecting it run under the
ledger's write transaction (``Ledger.tx``), and a manifest stored or re-stored within ``MANIFEST_GRACE_S`` is
never collected.

Objects in flight: a job stores an object's blocks and manifest first and registers the object with itself
later (a pack registers its whole closure at the end), so between the two the object is not "live". Every write
path therefore touches what it relies on — ``put`` and ``missing`` refresh the mtime of a stored block, the
manifest store refreshes the manifest — and GC never collects a block or manifest touched since the oldest
running job on this host began (``_protect_since``; GC cannot tell which job touched which block). A
``suspended`` job is not running (it may wait days for ``/handover resume``): it keeps the objects it registered,
nothing else (contract V25). Blocks
are deleted by renaming them aside first and reading the mtime from the renamed file, so a touch either happened
before the rename (the block goes back) or finds the block gone (the writer stores it again or reports it
missing). The object id rule itself — manifest document, id, reader checks — is ``jsonutil``'s (contract K-1).

Wire frame ``application/vnd.ctxh.blocks+v1``: ``"CXB1" | block_id (32 raw) | enc (1) | payload_len (u32 BE) |
payload``. The receiver checks magic and length, decodes with an output cap of one block (a decompression bomb
stops at ``block_size + 1`` bytes), and verifies ``sha256(raw) == block_id``. A rejected frame does not reject the
batch; a structural error (bad magic, impossible length, truncation) ends parsing because the next frame cannot
be located.
"""

from __future__ import annotations

import hashlib
import io
import math
import os
import re
import struct
import threading
import time
import zlib
from collections.abc import Callable, Iterable, Iterator, Sequence
from dataclasses import replace
from pathlib import Path
from typing import TYPE_CHECKING

from . import jsonutil
from .errors import BadRequest, BlockCorrupt, Cancelled, NotFound, VerifyFailed
from .jsonutil import canonical_bytes, loads_bounded
from .model import ObjectManifest

if TYPE_CHECKING:  # same layer (L2): type only
    from .ledger import Ledger

try:  # stdlib from Python 3.14 (present in the managed runtime, R13)
    from compression import zstd as _zstd
except ImportError:  # pragma: no cover - older interpreters
    _zstd = None

__all__ = [
    "BLOCK_SIZE",
    "ENC_IDENTITY",
    "ENC_ZLIB",
    "ENC_ZSTD",
    "FRAME_HEADER",
    "FRAME_MAGIC",
    "MAX_OBJECT_BYTES",
    "MEDIA_BLOCKS",
    "BlockCorrupt",
    "BlockStore",
    "decode_frames",
    "encode_frame",
    "encode_frames",
    "manifest_doc",
    "object_id_of",
    "verify_manifest",
]

BLOCK_SIZE = jsonutil.OBJECT_BLOCK_SIZE
FRAME_MAGIC = b"CXB1"
FRAME_HEADER = 41  # magic 4 + id 32 + enc 1 + length 4
ENC_IDENTITY, ENC_ZLIB, ENC_ZSTD = 0, 1, 2
FRAME_SLACK = 1024
"""A frame payload may exceed the block size by this much (compression framing of incompressible data)."""

MEDIA_BLOCKS = "application/vnd.ctxh.blocks+v1"
MAX_OBJECT_BYTES = jsonutil.OBJECT_MAX_BYTES
"""Objects above ≈ 30 GiB are refused (DESIGN §4.6); ``max_total_gb`` caps far below."""
MAX_MANIFEST_JSON = 64 * 1024**2
"""Object JSON and manifest documents ≤ 64 MiB (DESIGN §10.2, T7)."""

MANIFEST_GRACE_S = 600.0
"""A manifest stored or re-stored this recently is never collected (``put_manifests`` → ``ledger.job_object``)."""
GC_BATCH = 4096
"""Manifests checked and removed per ledger write transaction (one ``live_objects`` query each)."""
FREE_BATCH = 1024
"""Blocks renamed aside per batch before the running jobs are read once for the batch (``_free``)."""
GC_SLACK_S = 2.0
"""File times can trail the wall clock (coarse kernel clock; 1–2 s timestamp granularity on some file systems):
a block touched this shortly before a job began still counts as touched by it."""
MAX_IN_FLIGHT_S = 86_400.0
"""Protection of objects in flight reaches back at most this far: a job stuck for longer (suspended, crashed)
does not switch off the cache cap for good; its unregistered objects are rebuilt when it resumes."""
PARKED_STATES = frozenset({"suspended"})
"""Job states that give no time protection (``_protect_since``): a parked job keeps only its registered objects."""
_JOB_SCAN = 100_000
_TOMB_PREFIX = "gc."

ZSTD_AVAILABLE = _zstd is not None
_HEX64 = re.compile(r"[0-9a-f]{64}")
_HEX2 = re.compile(r"[0-9a-f]{2}")
_MANIFEST_NAME = re.compile(r"[0-9a-f]{62}\.json")
_GAIN_MIN = 0.05
_ENC_NAMES = {ENC_IDENTITY: "identity", ENC_ZLIB: "zlib", ENC_ZSTD: "zstd"}


def _check_id(block_id: object, field: str = "block_id") -> str:
    if not isinstance(block_id, str) or not _HEX64.fullmatch(block_id):
        raise BadRequest("malformed content id", field=field)
    return block_id


# ---------------------------------------------------------------------------------------------------------
# Manifests
# ---------------------------------------------------------------------------------------------------------


def manifest_doc(m: ObjectManifest) -> dict:
    """The canonical manifest document of DESIGN §5.2 (``object_id`` is not part of it): K-1's
    ``jsonutil.object_manifest_doc``."""
    return jsonutil.object_manifest_doc(m.sha256, m.size, m.block_size, m.blocks, m.media)


def object_id_of(m: ObjectManifest) -> str:
    """``sha256(canonical manifest bytes)`` — K-1's ``jsonutil.object_id``."""
    return jsonutil.object_id(manifest_doc(m))


def verify_manifest(m: ObjectManifest) -> ObjectManifest:
    """Validate a manifest received from anywhere and return it with its recomputed ``object_id``.

    The checks are K-1's ``jsonutil.check_object_manifest``: ``BadRequest`` for a block size other than
    ``BLOCK_SIZE``, malformed ids, an impossible block count, an object above ``MAX_OBJECT_BYTES`` or an invalid
    media type; ``BlockCorrupt`` when a given ``object_id`` differs from the recomputed one.
    """
    if not isinstance(m, ObjectManifest):
        raise BadRequest("not an object manifest", field="manifest")
    for name in ("size", "block_size"):
        value = getattr(m, name)
        if not isinstance(value, int) or isinstance(value, bool):
            raise BadRequest("manifest field must be an integer", field=f"manifest.{name}")
    if not isinstance(m.blocks, (tuple, list)):
        raise BadRequest("manifest without blocks", field="manifest.blocks")
    doc = manifest_doc(m)
    oid = m.object_id or jsonutil.object_id(doc)
    jsonutil.check_object_manifest(doc, oid)
    return m if m.object_id == oid else replace(m, object_id=oid)


def manifest_from_doc(doc: object, object_id: str) -> ObjectManifest:
    """A stored or received manifest document, checked against the id it was asked for by (K-1)."""
    jsonutil.check_object_manifest(doc, object_id)
    return ObjectManifest(
        sha256=doc["sha256"],
        size=doc["size"],
        block_size=doc["block_size"],
        blocks=tuple(doc["blocks"]),
        media=doc["media"],
        object_id=object_id,
    )


# ---------------------------------------------------------------------------------------------------------
# Frame codec
# ---------------------------------------------------------------------------------------------------------


def _zstd_compress(raw: bytes) -> bytes:
    if _zstd is None:  # pragma: no cover
        raise RuntimeError("compression.zstd unavailable")
    return _zstd.compress(raw, level=3)


def _choose_encoding(raw: bytes, peer_caps: frozenset[str]) -> tuple[int, bytes]:
    if not raw:
        return ENC_IDENTITY, raw
    if "enc.zstd" in peer_caps and ZSTD_AVAILABLE:
        enc, payload = ENC_ZSTD, _zstd_compress(raw)
    elif "enc.zlib" in peer_caps:
        enc, payload = ENC_ZLIB, zlib.compress(raw, 6)
    else:
        return ENC_IDENTITY, raw
    if len(payload) > len(raw) * (1 - _GAIN_MIN):
        return ENC_IDENTITY, raw
    return enc, payload


def encode_frame(block_id: str, raw: bytes, *, peer_caps: frozenset[str]) -> bytes:
    """One frame for ``(block_id, raw)``: zstd when the peer lists ``enc.zstd`` (and it is available here), else
    zlib when it lists ``enc.zlib``, identity when the gain is below 5 %. The id is always over the raw bytes."""
    _check_id(block_id)
    if len(raw) > BLOCK_SIZE:
        raise BadRequest("block larger than the block size", field="block", size=len(raw))
    enc, payload = _choose_encoding(raw, frozenset(peer_caps))
    return FRAME_MAGIC + bytes.fromhex(block_id) + bytes([enc]) + struct.pack(">I", len(payload)) + payload


def encode_frames(blocks: Iterable[tuple[str, bytes]], *, peer_caps: frozenset[str], max_body: int) -> Iterator[bytes]:
    """Group frames into request bodies of at most ``max_body`` bytes (a single larger frame travels alone)."""
    batch: list[bytes] = []
    size = 0
    for block_id, raw in blocks:
        frame = encode_frame(block_id, raw, peer_caps=peer_caps)
        if batch and size + len(frame) > max_body:
            yield b"".join(batch)
            batch, size = [], 0
        batch.append(frame)
        size += len(frame)
    if batch:
        yield b"".join(batch)


def _decompress(enc: int, payload: bytes, cap: int) -> tuple[bytes | None, str | None]:
    """Decode with an output cap of ``cap`` bytes; ``(raw, None)`` or ``(None, reason)``."""
    if enc == ENC_IDENTITY:
        return (payload, None) if len(payload) <= cap else (None, "too_large")
    try:
        if enc == ENC_ZLIB:
            d = zlib.decompressobj()
            out = d.decompress(payload, cap + 1)
            if len(out) > cap or d.unconsumed_tail:
                return None, "too_large"
            if not d.eof or d.unused_data:
                return None, "bad_encoding"
            return out, None
        if enc == ENC_ZSTD and _zstd is not None:
            dz = _zstd.ZstdDecompressor()
            out = dz.decompress(payload, max_length=cap + 1)
            if len(out) > cap:
                return None, "too_large"
            if not dz.eof or dz.unused_data:
                return None, "too_large" if not dz.needs_input else "bad_encoding"
            return out, None
    except (zlib.error, ValueError, EOFError) as exc:
        del exc
        return None, "bad_encoding"
    except Exception as exc:  # zstd raises its own ZstdError
        if _zstd is not None and isinstance(exc, _zstd.ZstdError):
            return None, "bad_encoding"
        raise
    return None, "bad_encoding"


def decode_frames(body: bytes, *, block_size: int) -> Iterator[tuple[str, bytes | None, str | None]]:
    """Yield ``(block_id, raw, None)`` for good frames and ``(block_id, None, reason)`` for rejected ones.

    Reasons: ``bad_frame`` (structural; parsing stops, block id ``""`` when unknown), ``bad_encoding``,
    ``too_large`` (decompression output above ``block_size``), ``block_corrupt`` (hash mismatch).
    """
    view = memoryview(body)
    pos, end = 0, len(body)
    while pos < end:
        if end - pos < FRAME_HEADER or bytes(view[pos : pos + 4]) != FRAME_MAGIC:
            yield "", None, "bad_frame"
            return
        block_id = bytes(view[pos + 4 : pos + 36]).hex()
        enc = view[pos + 36]
        (length,) = struct.unpack(">I", view[pos + 37 : pos + 41])
        if length > block_size + FRAME_SLACK or pos + FRAME_HEADER + length > end:
            yield block_id, None, "bad_frame"
            return
        payload = bytes(view[pos + FRAME_HEADER : pos + FRAME_HEADER + length])
        pos += FRAME_HEADER + length
        if enc not in _ENC_NAMES:
            yield block_id, None, "bad_encoding"
            continue
        raw, reason = _decompress(enc, payload, block_size)
        if raw is None:
            yield block_id, None, reason
            continue
        if hashlib.sha256(raw).hexdigest() != block_id:
            yield block_id, None, "block_corrupt"
            continue
        yield block_id, raw, None


# ---------------------------------------------------------------------------------------------------------
# Block store
# ---------------------------------------------------------------------------------------------------------


def mkdir_private(path: Path) -> None:
    """``mkdir -p`` with mode 0700 for every directory this call creates (umask-independent)."""
    missing = []
    probe = path
    while not probe.exists():
        missing.append(probe)
        probe = probe.parent
    for directory in reversed(missing):
        try:
            os.mkdir(directory, 0o700)
        except FileExistsError:
            continue
        os.chmod(directory, 0o700)


def write_private(final: Path, data: bytes, tmp_dir: Path) -> None:
    """Atomic 0600 write: temp file in ``tmp_dir`` → fsync → ``os.replace``; the temp file never survives."""
    mkdir_private(tmp_dir)
    mkdir_private(final.parent)
    tmp = tmp_dir / f"{final.name}.{os.getpid()}.{threading.get_ident()}.{time.monotonic_ns()}"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        try:
            view = memoryview(data)
            while view:
                written = os.write(fd, view)
                view = view[written:]
            os.fsync(fd)
        finally:
            os.close(fd)
        os.replace(tmp, final)
    except BaseException:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


class _ObjectReader(io.RawIOBase):
    """Streaming reader over an object's blocks; every block is hash-checked, size and content sha256 at EOF."""

    def __init__(self, store: BlockStore, m: ObjectManifest) -> None:
        super().__init__()
        self._store = store
        self._m = m
        self._index = 0
        self._buf = b""
        self._hash = hashlib.sha256()
        self._seen = 0
        self._done = False

    def readable(self) -> bool:
        return True

    def _finish(self) -> None:
        if self._done:
            return
        self._done = True
        if self._seen != self._m.size or self._hash.hexdigest() != self._m.sha256:
            raise VerifyFailed(
                "transferred data does not match its checksum", object_id=self._m.object_id, reason="object_sha256"
            )

    def readinto(self, b) -> int:
        while not self._buf:
            if self._index >= len(self._m.blocks):
                self._finish()
                return 0
            raw = self._store.get(self._m.blocks[self._index])
            self._index += 1
            self._hash.update(raw)
            self._seen += len(raw)
            if self._seen > self._m.size:
                self._finish()
            self._buf = raw
        n = min(len(b), len(self._buf))
        b[:n] = self._buf[:n]
        self._buf = self._buf[n:]
        return n


class BlockStore:
    """Per-profile block store (implements ``BlockSource`` and ``BlockSink``; DESIGN §5.1).

    ``root`` is the plugin data directory; ``ledger`` is ``ledger.Ledger`` (cluster A). Methods used:
    ``tx`` (re-entrant within a thread), ``block_put``, ``block_touch``, ``object_put``, ``object_delete``,
    ``job_objects``, ``live_objects``, ``gc_candidates``, ``blocks_delete``, ``jobs(active=True)`` (``created_at``
    of the running jobs, ``_protect_since``). ``cache_days``/``cache_bytes`` are ``block_cache_days``/
    ``block_cache_gb`` (§14.4).
    """

    def __init__(
        self,
        root: Path,
        ledger: Ledger,
        *,
        cache_days: float = 3,
        cache_bytes: int = 5 * 1024**3,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self.root = Path(root)
        self._ledger = ledger
        self.cache_days = cache_days
        self.cache_bytes = int(cache_bytes)
        self._clock = clock
        self._blocks = self.root / "blocks" / "sha256"
        self._tmp = self.root / "blocks" / "tmp"
        self._manifests = self.root / "manifests"

    # -- paths ---------------------------------------------------------------------------------------------
    def block_path(self, block_id: str) -> Path:
        _check_id(block_id)
        return self._blocks / block_id[:2] / block_id[2:]

    def manifest_path(self, object_id: str) -> Path:
        _check_id(object_id, "object_id")
        return self._manifests / object_id[:2] / f"{object_id[2:]}.json"

    # -- blocks --------------------------------------------------------------------------------------------
    def has(self, block_id: str) -> bool:
        return self.block_path(block_id).is_file()

    def get(self, block_id: str) -> bytes:
        """Raw bytes of a stored block, re-verified against its id (``BlockCorrupt`` when the file changed)."""
        path = self.block_path(block_id)
        try:
            raw = path.read_bytes()
        except FileNotFoundError:
            raise NotFound("block not found", block_id=block_id) from None
        if hashlib.sha256(raw).hexdigest() != block_id:
            raise BlockCorrupt(
                "a stored piece of the transfer is damaged (it does not match its checksum)", block_id=block_id
            )
        return raw

    @staticmethod
    def _touch(path: Path) -> bool:
        """Refresh a stored file's mtime — someone relies on it again, so GC keeps it while that job runs
        (module docstring); False when the file is gone."""
        try:
            os.utime(path)
        except FileNotFoundError:
            return False
        return True

    def put(self, raw: bytes) -> str:
        """Store one block (≤ ``BLOCK_SIZE``) and return its id; an existing block only gets touched."""
        if len(raw) > BLOCK_SIZE:
            raise BadRequest("block larger than the block size", field="block", size=len(raw))
        block_id = hashlib.sha256(raw).hexdigest()
        path = self.block_path(block_id)
        if not self._touch(path):
            write_private(path, bytes(raw), self._tmp)
        self._ledger.block_put(block_id, len(raw))
        return block_id

    def missing(self, ids: Sequence[str]) -> list[str]:
        """The ids not stored here, in request order; stored ones are touched (file mtime and LRU), because the
        caller now relies on them."""
        out, present = [], []
        for block_id in ids:
            (present if self._touch(self.block_path(block_id)) else out).append(block_id)
        if present:
            self._ledger.block_touch(present)
        return out

    def write_blocks(self, frames: Iterable[tuple[str, bytes]]) -> tuple[int, list[tuple[str, str]]]:
        """Store verified ``(block_id, raw)`` pairs; a bad pair is rejected alone (``(id, code)``)."""
        accepted = 0
        rejected: list[tuple[str, str]] = []
        for block_id, raw in frames:
            if not isinstance(block_id, str) or not _HEX64.fullmatch(block_id):
                rejected.append((str(block_id)[:64], "bad_request"))
                continue
            if len(raw) > BLOCK_SIZE:
                rejected.append((block_id, "payload_too_large"))
                continue
            if hashlib.sha256(raw).hexdigest() != block_id:
                rejected.append((block_id, "block_corrupt"))
                continue
            self.put(raw)
            accepted += 1
        return accepted, rejected

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        """Yield ``(block_id, raw)``; ``NotFound`` for an unknown id, ``BlockCorrupt`` for a damaged one."""
        ids = list(ids)
        for block_id in ids:
            yield block_id, self.get(block_id)
        if ids:
            self._ledger.block_touch(ids)

    # -- objects -------------------------------------------------------------------------------------------
    def _store_manifest(self, m: ObjectManifest) -> bool:
        """Write (or refresh the mtime of) the manifest file and index it, under the ledger's write lock so the
        manifest GC never removes a file between this check and the caller's ``job_object``."""
        path = self.manifest_path(m.object_id)
        with self._ledger.tx():
            try:
                os.utime(path)  # re-stored: fresh again for MANIFEST_GRACE_S
                new = False
            except FileNotFoundError:
                write_private(path, canonical_bytes(manifest_doc(m)), self._tmp)
                new = True
            self._ledger.object_put(m)
        return new

    def put_stream(self, fileobj, *, media: str, cancelled: Callable[[], bool]) -> ObjectManifest:
        """Split a binary stream into blocks (full blocks except the last) and store its manifest."""
        content = hashlib.sha256()
        ids: list[str] = []
        size = 0
        while True:
            if cancelled():
                raise Cancelled("cancelled while storing an object")
            chunk = _read_full(fileobj, BLOCK_SIZE)
            if not chunk:
                break
            content.update(chunk)
            size += len(chunk)
            if size > MAX_OBJECT_BYTES:
                raise BadRequest("a file is too large to transfer", field="object", max_bytes=MAX_OBJECT_BYTES)
            ids.append(self.put(chunk))
            if len(chunk) < BLOCK_SIZE:
                break
        m = verify_manifest(
            ObjectManifest(sha256=content.hexdigest(), size=size, block_size=BLOCK_SIZE, blocks=tuple(ids), media=media)
        )
        self._store_manifest(m)
        return m

    def put_bytes(self, data: bytes, media: str) -> ObjectManifest:
        """An in-memory object, split by K-1's ``jsonutil.object_manifest`` (same ids as ``put_stream``)."""
        doc, parts = jsonutil.object_manifest(data, media)
        for _block_id, raw in parts:
            self.put(raw)
        m = manifest_from_doc(doc, jsonutil.object_id(doc))
        self._store_manifest(m)
        return m

    def manifest(self, object_id: str) -> ObjectManifest:
        path = self.manifest_path(object_id)
        try:
            data = path.read_bytes()
        except FileNotFoundError:
            raise NotFound("object not found", object_id=object_id) from None
        return manifest_from_doc(loads_bounded(data, max_bytes=MAX_MANIFEST_JSON), object_id)

    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        """Manifests in request order; ``NotFound`` for an unknown id."""
        return [self.manifest(oid) for oid in object_ids]

    def put_manifests(self, ms: Sequence[ObjectManifest]) -> int:
        """Store received manifests after ``verify_manifest``; returns how many were new."""
        checked = [verify_manifest(m) for m in ms]
        return sum(1 for m in checked if self._store_manifest(m))

    def has_object(self, object_id: str) -> bool:
        """Manifest known and every block present."""
        try:
            m = self.manifest(object_id)
        except NotFound:
            return False
        return not self.missing(list(m.blocks))

    def open_object(self, object_id: str) -> io.BufferedReader:
        """Streaming reader; verifies every block and, at EOF, size and content sha256 (``VerifyFailed``)."""
        return io.BufferedReader(_ObjectReader(self, self.manifest(object_id)), buffer_size=BLOCK_SIZE)

    def read_object_bytes(self, object_id: str, *, max_bytes: int = MAX_MANIFEST_JSON) -> bytes:
        """Whole object in memory, refused before reading when the manifest says it exceeds ``max_bytes``."""
        m = self.manifest(object_id)
        if m.size > max_bytes:
            raise BadRequest("object too large to load", field="object", size=m.size, max_bytes=max_bytes)
        with self.open_object(object_id) as fh:
            return fh.read()

    # -- GC --------------------------------------------------------------------------------------------------
    def _blocks_of(self, object_ids: Iterable[str]) -> set[str]:
        out: set[str] = set()
        for oid in object_ids:
            try:
                out.update(self.manifest(oid).blocks)
            except (NotFound, BadRequest):
                continue
        return out

    def _protect_since(self) -> float:
        """Blocks and manifests touched at or after this time are never collected: they may belong to an object a
        running job stored or re-used but has not registered yet. Every running job on this host counts (GC
        cannot tell which job touched which block) — a non-terminal one that is not parked (``PARKED_STATES``,
        contract V25): the oldest one's start minus ``GC_SLACK_S``, but no earlier than ``MAX_IN_FLIGHT_S`` ago
        (also when the job list is incomplete); ``inf`` when no job runs. File times are wall-clock times, so this
        is too."""
        floor = time.time() - MAX_IN_FLIGHT_S
        rows = self._ledger.jobs(active=True, limit=_JOB_SCAN)
        if len(rows) >= _JOB_SCAN:
            return floor
        starts = []
        for row in rows:
            if row.get("state") in PARKED_STATES:
                continue
            at = row.get("created_at")
            if isinstance(at, bool) or not isinstance(at, (int, float)):
                return floor
            starts.append(float(at))
        return max(min(starts) - GC_SLACK_S, floor) if starts else math.inf

    def _tomb(self, block_id: str) -> Path:
        return self._tmp / f"{_TOMB_PREFIX}{block_id}.{os.getpid()}.{threading.get_ident()}.{time.monotonic_ns()}"

    def _restore_tombs(self) -> None:
        """Put back blocks a GC of a dead process had renamed aside but not decided on (a crash between the
        rename and the decision must not lose a block a job relies on; restoring an unneeded one only keeps it
        cached)."""
        try:
            names = os.listdir(self._tmp)
        except FileNotFoundError:
            return
        for name in names:
            parts = name.split(".")
            if not name.startswith(_TOMB_PREFIX) or len(parts) != 5 or not _HEX64.fullmatch(parts[1]):
                continue
            if not parts[2].isdigit() or _pid_alive(int(parts[2])):
                continue
            tomb = self._tmp / name
            try:
                os.replace(tomb, self.block_path(parts[1]))
            except FileNotFoundError:
                continue

    def _free(self, ids: Iterable[str]) -> int:
        """Delete the blocks GC chose, except those a running job touched (``_protect_since``); returns how many
        went.

        Each block is renamed aside and its mtime read from the renamed file: a job that touched it before the
        rename is seen (the block goes back), a job touching it afterwards finds it gone and stores it again or
        reports it missing. The running jobs are read after the renames of each batch, so a job that began while
        GC ran counts too (it touches nothing before its job row exists). A block stored again while it was aside
        keeps its ledger row.
        """
        self._restore_tombs()
        ids = list(dict.fromkeys(ids))
        if not ids:
            return 0
        mkdir_private(self._tmp)
        freed = 0
        for start in range(0, len(ids), FREE_BATCH):
            aside: list[tuple[str, Path, Path | None]] = []
            try:
                for block_id in ids[start : start + FREE_BATCH]:
                    path, tomb = self.block_path(block_id), self._tomb(block_id)
                    try:
                        os.rename(path, tomb)
                    except FileNotFoundError:
                        aside.append((block_id, path, None))
                        continue
                    aside.append((block_id, path, tomb))
                protect = self._protect_since()
            except BaseException:
                for _block_id, path, tomb in aside:
                    if tomb is not None:
                        os.replace(tomb, path)
                raise
            gone = []
            for block_id, path, tomb in aside:
                if tomb is not None:
                    try:
                        touched = os.stat(tomb).st_mtime >= protect
                    except FileNotFoundError:
                        touched = False
                    if touched:
                        os.replace(tomb, path)
                        continue
                    try:
                        os.unlink(tomb)
                    except FileNotFoundError:
                        pass
                    if path.exists():  # stored again by a writer that found it aside
                        continue
                gone.append(block_id)
            if gone:
                self._ledger.blocks_delete(gone)
            freed += len(gone)
        return freed

    def _manifest_ids(self) -> Iterator[list[str]]:
        """Object ids of the stored manifests, one ``manifests/<2 hex>/`` directory at a time (foreign names are
        skipped)."""
        try:
            tops = sorted(e.name for e in os.scandir(self._manifests) if e.is_dir(follow_symlinks=False))
        except FileNotFoundError:
            return
        for top in tops:
            if not _HEX2.fullmatch(top):
                continue
            try:
                names = os.listdir(self._manifests / top)
            except FileNotFoundError:
                continue
            yield [top + name[:-5] for name in sorted(names) if _MANIFEST_NAME.fullmatch(name)]

    def _drop_manifests(self, object_ids: Sequence[str], cutoff: float) -> int:
        """One write transaction: remove the manifest files and ``objects`` rows of the given objects that no
        non-terminal job references and that were not (re-)stored after ``cutoff`` nor since a running job began
        (``_protect_since``; read inside the transaction, which also serialises job creation and manifest
        stores)."""
        gone: list[str] = []
        with self._ledger.tx():
            live = self._ledger.live_objects()
            cutoff = min(cutoff, self._protect_since())
            for oid in object_ids:
                if oid in live:
                    continue
                path = self.manifest_path(oid)
                try:
                    if path.stat().st_mtime > cutoff:
                        continue
                    path.unlink()
                except FileNotFoundError:
                    pass
                gone.append(oid)
            if gone:
                self._ledger.object_delete(gone)
        return len(gone)

    def collect_manifests(self, object_ids: Iterable[str] | None = None) -> int:
        """Remove manifests and index rows of objects no non-terminal job references (R28): finished and aborted
        jobs' objects and unreferenced ones, except those (re-)stored within ``MANIFEST_GRACE_S``. ``object_ids``
        None walks every stored manifest. Returns the number of removed objects."""
        cutoff = self._clock() - MANIFEST_GRACE_S
        groups = self._manifest_ids() if object_ids is None else [sorted(set(object_ids))]
        removed = 0
        for ids in groups:
            for start in range(0, len(ids), GC_BATCH):
                removed += self._drop_manifests(ids[start : start + GC_BATCH], cutoff)
        return removed

    def gc(self, *, keep_days: float, max_bytes: int) -> int:
        """Free blocks no non-terminal job needs that are older than ``keep_days`` or beyond ``max_bytes`` (LRU),
        then collect the manifests of objects no non-terminal job references (R28); returns the number of freed
        blocks. Blocks and manifests touched since a running job began stay (module docstring)."""
        live = self._blocks_of(self._ledger.live_objects())
        older_than = self._clock() - keep_days * 86_400
        candidates = [b for b in self._ledger.gc_candidates(older_than, int(max_bytes)) if b not in live]
        freed = self._free(candidates)
        self.collect_manifests()
        return freed

    def release_job(self, job_id: str, *, keep: bool) -> None:
        """At a job's end: ``keep`` ⇒ its blocks stay cached within the cap (LRU beyond it is freed); otherwise
        every block of its objects that no live job needs is freed now (DESIGN §4.6, SK M3) — except blocks a
        running job touched since it began, which go with a later GC. Either way the manifests of objects no
        live job references go (R28); blocks first, because they are found through the manifests."""
        if keep:
            self.gc(keep_days=self.cache_days, max_bytes=self.cache_bytes)
            return
        objects = self._ledger.job_objects(job_id)
        live = self._blocks_of(self._ledger.live_objects())
        mine = self._blocks_of(objects)
        self._free(sorted(mine - live))
        self.collect_manifests(objects)


def _pid_alive(pid: int) -> bool:
    """True unless ``pid`` certainly does not exist (a permission error means it does). On Windows signal 0 is
    ``CTRL_C_EVENT``, so there every pid counts as alive: a crashed GC's renamed blocks stay in ``blocks/tmp``
    (named ``gc.<block id>.…``) instead of being put back."""
    if os.name == "nt":
        return True
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except OSError:
        return True
    return True


def _read_full(fileobj, n: int) -> bytes:
    """Read until ``n`` bytes or EOF (short reads of pipes and sockets must not create short blocks)."""
    parts = []
    remaining = n
    while remaining:
        chunk = fileobj.read(remaining)
        if not chunk:
            break
        parts.append(chunk)
        remaining -= len(chunk)
    return b"".join(parts)
