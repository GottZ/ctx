"""In-memory ``BlockStore`` (cluster C fake; INTERFACES §6: "in-memory BlockStore").

Same public surface as ``engine.blocks.BlockStore`` and the same ids (manifests are built and verified with the
real ``blocks`` helpers), but blocks, manifests and job references live in dicts — no ledger, no disk. Other
clusters use it to test code that stores or reads objects (capsules, packs, plan documents) without cluster C's
filesystem layout.

Extra test hooks: ``corrupt(block_id, raw)`` replaces a stored block's bytes; ``job_objects`` maps job id → object
ids (``track(job_id, object_id)``); ``live_jobs`` names jobs GC must keep; ``reads``/``writes`` count block I/O.
"""

from __future__ import annotations

import hashlib
import io
from collections.abc import Callable, Iterable, Iterator, Sequence
from dataclasses import replace

from ctx_handover.engine.blocks import (
    BLOCK_SIZE,
    MAX_MANIFEST_JSON,
    MAX_OBJECT_BYTES,
    object_id_of,
    verify_manifest,
)
from ctx_handover.engine.errors import BadRequest, BlockCorrupt, Cancelled, NotFound, VerifyFailed
from ctx_handover.engine.model import ObjectManifest


class MemoryBlockStore:
    """Dict-backed stand-in for ``engine.blocks.BlockStore`` (``BlockSource`` + ``BlockSink``)."""

    def __init__(self) -> None:
        self.blocks: dict[str, bytes] = {}
        self.objects: dict[str, ObjectManifest] = {}
        self.job_objects: dict[str, set[str]] = {}
        self.live_jobs: set[str] = set()
        self.reads = 0
        self.writes = 0

    # -- blocks --------------------------------------------------------------------------------------------
    def has(self, block_id: str) -> bool:
        return block_id in self.blocks

    def get(self, block_id: str) -> bytes:
        try:
            raw = self.blocks[block_id]
        except KeyError:
            raise NotFound("block not found", block_id=block_id) from None
        if hashlib.sha256(raw).hexdigest() != block_id:
            raise BlockCorrupt("stored block does not match its id", block_id=block_id)
        self.reads += 1
        return raw

    def put(self, raw: bytes) -> str:
        if len(raw) > BLOCK_SIZE:
            raise BadRequest("block larger than the block size", field="block")
        block_id = hashlib.sha256(raw).hexdigest()
        self.blocks[block_id] = bytes(raw)
        self.writes += 1
        return block_id

    def corrupt(self, block_id: str, raw: bytes) -> None:
        self.blocks[block_id] = raw

    def missing(self, ids: Sequence[str]) -> list[str]:
        return [i for i in ids if i not in self.blocks]

    def write_blocks(self, frames: Iterable[tuple[str, bytes]]) -> tuple[int, list[tuple[str, str]]]:
        accepted, rejected = 0, []
        for block_id, raw in frames:
            if len(raw) > BLOCK_SIZE:
                rejected.append((block_id, "payload_too_large"))
            elif hashlib.sha256(raw).hexdigest() != block_id:
                rejected.append((block_id, "block_corrupt"))
            else:
                self.put(raw)
                accepted += 1
        return accepted, rejected

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        for block_id in ids:
            yield block_id, self.get(block_id)

    # -- objects -------------------------------------------------------------------------------------------
    def put_stream(self, fileobj, *, media: str, cancelled: Callable[[], bool]) -> ObjectManifest:
        content = hashlib.sha256()
        ids, size = [], 0
        while True:
            if cancelled():
                raise Cancelled("cancelled while storing an object")
            chunk = fileobj.read(BLOCK_SIZE)
            if not chunk:
                break
            content.update(chunk)
            size += len(chunk)
            if size > MAX_OBJECT_BYTES:
                raise BadRequest("object too large", field="object")
            ids.append(self.put(chunk))
        m = ObjectManifest(sha256=content.hexdigest(), size=size, block_size=BLOCK_SIZE, blocks=tuple(ids), media=media)
        m = replace(m, object_id=object_id_of(m))
        self.objects[m.object_id] = m
        return m

    def put_bytes(self, data: bytes, media: str) -> ObjectManifest:
        return self.put_stream(io.BytesIO(data), media=media, cancelled=lambda: False)

    def manifest(self, object_id: str) -> ObjectManifest:
        try:
            return self.objects[object_id]
        except KeyError:
            raise NotFound("object not found", object_id=object_id) from None

    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        return [self.manifest(oid) for oid in object_ids]

    def put_manifests(self, ms: Sequence[ObjectManifest]) -> int:
        new = 0
        for m in ms:
            m = verify_manifest(m)
            if m.object_id not in self.objects:
                self.objects[m.object_id] = m
                new += 1
        return new

    def has_object(self, object_id: str) -> bool:
        m = self.objects.get(object_id)
        return m is not None and not self.missing(list(m.blocks))

    def open_object(self, object_id: str) -> io.BytesIO:
        m = self.manifest(object_id)
        data = b"".join(self.get(b) for b in m.blocks)
        if len(data) != m.size or hashlib.sha256(data).hexdigest() != m.sha256:
            raise VerifyFailed("object content does not match its manifest", object_id=object_id)
        return io.BytesIO(data)

    def read_object_bytes(self, object_id: str, *, max_bytes: int = MAX_MANIFEST_JSON) -> bytes:
        m = self.manifest(object_id)
        if m.size > max_bytes:
            raise BadRequest("object too large to load", field="object")
        return self.open_object(object_id).read()

    # -- job references and GC -----------------------------------------------------------------------------
    def track(self, job_id: str, object_id: str) -> None:
        self.job_objects.setdefault(job_id, set()).add(object_id)

    def _live_blocks(self) -> set[str]:
        out: set[str] = set()
        for job in self.live_jobs:
            for oid in self.job_objects.get(job, ()):
                if oid in self.objects:
                    out.update(self.objects[oid].blocks)
        return out

    def gc(self, *, keep_days: float, max_bytes: int) -> int:
        live = self._live_blocks()
        total = sum(len(v) for v in self.blocks.values())
        freed = 0
        for block_id in list(self.blocks):
            if block_id in live:
                continue
            if keep_days <= 0 or total > max_bytes:
                total -= len(self.blocks.pop(block_id))
                freed += 1
        return freed

    def release_job(self, job_id: str, *, keep: bool) -> None:
        self.live_jobs.discard(job_id)
        if keep:
            return
        live = self._live_blocks()
        for oid in self.job_objects.get(job_id, ()):
            m = self.objects.get(oid)
            for block_id in m.blocks if m else ():
                if block_id not in live:
                    self.blocks.pop(block_id, None)
