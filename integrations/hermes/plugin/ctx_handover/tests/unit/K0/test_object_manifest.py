"""K0: the one object-id rule (contract K-1; DESIGN §4.6, §5.2; W027/W094).

``jsonutil.object_manifest``/``object_id`` must produce byte-identical ids to the three writers that existed
before the rule moved into K0 — cluster C's ``BlockStore.put_bytes`` (``blocks.py``), cluster E's
``gitops.put_bytes`` and cluster B's bare-sink fallback ``session_hash.put_object`` — and
``check_object_manifest`` is the reader-side check every object reader delegates to.
"""

from __future__ import annotations

import contextlib
import hashlib
import os

import pytest

from ctx_handover.engine import jsonutil
from ctx_handover.engine.errors import BadRequest, BlockCorrupt
from ctx_handover.engine.jsonutil import (
    OBJECT_BLOCK_SIZE,
    check_object_manifest,
    object_id,
    object_manifest,
    object_manifest_doc,
)
from ctx_handover.engine.model import ObjectManifest

MEDIA = "application/octet-stream"
EMPTY_SHA = hashlib.sha256(b"").hexdigest()


class _MemSink:
    """A bare ``BlockSink``/``BlockSource`` (no ``put_bytes``): what E's and B's writers see."""

    def __init__(self) -> None:
        self.blocks: dict[str, bytes] = {}
        self.manifests_by_id: dict[str, ObjectManifest] = {}

    def missing(self, ids):
        return [i for i in ids if i not in self.blocks]

    def write_blocks(self, frames):
        accepted = 0
        for bid, raw in frames:
            self.blocks[bid] = bytes(raw)
            accepted += 1
        return accepted, []

    def put_manifests(self, ms):
        for m in ms:
            self.manifests_by_id[m.object_id] = m
        return len(ms)

    def manifests(self, ids):
        return [self.manifests_by_id[i] for i in ids if i in self.manifests_by_id]

    def read_blocks(self, ids):
        return [(i, self.blocks[i]) for i in ids]


class _LedgerStub:
    """The four ledger calls C's ``BlockStore`` makes while storing (``blocks.py`` ``put``/``_store_manifest``)."""

    @contextlib.contextmanager
    def tx(self):
        yield

    def block_put(self, block_id, size):
        pass

    def block_touch(self, ids):
        pass

    def object_put(self, m):
        pass


def _data(n: int) -> bytes:
    return (hashlib.sha256(str(n).encode()).digest() * (n // 32 + 1))[:n]


SIZES = [1, 31, OBJECT_BLOCK_SIZE - 1, OBJECT_BLOCK_SIZE, OBJECT_BLOCK_SIZE + 1, 2 * OBJECT_BLOCK_SIZE + 12_345]


def _fields(doc: dict, oid: str) -> ObjectManifest:
    return ObjectManifest(doc["sha256"], doc["size"], doc["block_size"], tuple(doc["blocks"]), doc["media"], oid)


def test_object_manifest_empty_object_has_no_blocks(tmp_path):
    """★ An empty object is 0 blocks: size 0, content hash of ``b""``, ``blocks == []`` — the rule of C and E
    (DESIGN §4.6, contract K-1); its id equals theirs and the reader check accepts it.

    Mutant: B's old fallback ``range(0, max(len(data), 1), block_size)`` (one empty block).
    """
    from ctx_handover.engine import blocks, gitops

    doc, raw_blocks = object_manifest(b"", MEDIA)
    assert raw_blocks == []
    assert doc == {
        "kind": "ctxh.object",
        "v": 1,
        "sha256": EMPTY_SHA,
        "size": 0,
        "block_size": OBJECT_BLOCK_SIZE,
        "blocks": [],
        "media": MEDIA,
    }
    oid = object_id(doc)
    check_object_manifest(doc, oid)
    assert oid == gitops.put_bytes(_MemSink(), b"", MEDIA).object_id
    assert oid == blocks.BlockStore(tmp_path / "data", _LedgerStub()).put_bytes(b"", MEDIA).object_id


@pytest.mark.parametrize("size", SIZES)
def test_object_id_byte_equal_to_blocks_gitops_and_session_hash(tmp_path, size):
    """For non-empty data the K0 rule gives the same blocks and the same object id as C's ``BlockStore``, E's
    ``gitops.put_bytes`` and B's bare-sink ``session_hash.put_object``; the documents are byte-identical."""
    from ctx_handover.engine import blocks, gitops, session_hash

    data = _data(size)
    doc, raw_blocks = object_manifest(data, MEDIA)
    oid = object_id(doc)
    assert b"".join(raw for _, raw in raw_blocks) == data
    assert all(hashlib.sha256(raw).hexdigest() == bid and len(raw) <= OBJECT_BLOCK_SIZE for bid, raw in raw_blocks)
    assert len(raw_blocks) == -(-size // OBJECT_BLOCK_SIZE)

    c = blocks.BlockStore(tmp_path / "data", _LedgerStub()).put_bytes(data, MEDIA)
    e = gitops.put_bytes(_MemSink(), data, MEDIA)
    b = session_hash.put_object(_MemSink(), data, MEDIA)
    for m in (c, e, b):
        assert m.object_id == oid
        assert list(m.blocks) == doc["blocks"]
    assert jsonutil.canonical_bytes(blocks.manifest_doc(c)) == jsonutil.canonical_bytes(doc)
    assert jsonutil.canonical_bytes(gitops.manifest_doc(e)) == jsonutil.canonical_bytes(doc)
    assert blocks.object_id_of(_fields(doc, "")) == oid == gitops.object_id_of(_fields(doc, ""))
    assert object_id(object_manifest_doc(doc["sha256"], size, OBJECT_BLOCK_SIZE, doc["blocks"], MEDIA)) == oid


def test_writers_and_readers_accept_each_other():
    """C's and E's readers accept a K0 manifest, and the K0 check accepts their documents."""
    from ctx_handover.engine import blocks, gitops

    data = _data(OBJECT_BLOCK_SIZE + 7)
    doc, raw_blocks = object_manifest(data, "application/json")
    oid = object_id(doc)
    assert blocks.manifest_from_doc(doc, oid).object_id == oid
    sink = _MemSink()
    sink.write_blocks(raw_blocks)
    sink.put_manifests([_fields(doc, oid)])
    assert gitops.object_manifest(sink, oid).object_id == oid
    assert b"".join(gitops.iter_object(sink, oid)) == data
    e = gitops.put_bytes(_MemSink(), data, "application/json")
    check_object_manifest(gitops.manifest_doc(e), e.object_id)


def test_object_manifest_writes_only_the_one_block_size():
    with pytest.raises(ValueError, match="block size"):
        object_manifest(b"abc", MEDIA, 4096)
    with pytest.raises(ValueError):
        object_manifest(b"abc", MEDIA, True)
    doc, _ = object_manifest(bytearray(b"abc"), MEDIA, OBJECT_BLOCK_SIZE)
    assert doc["size"] == 3


def _consistent(block_size: int, size: int = 10_000) -> tuple[dict, str]:
    """A self-consistent manifest document (blocks match size and block size, id recomputed by the caller)."""
    data = _data(size)
    ids = [hashlib.sha256(data[i : i + block_size]).hexdigest() for i in range(0, size, block_size)]
    doc = object_manifest_doc(hashlib.sha256(data).hexdigest(), size, block_size, ids, MEDIA)
    return doc, object_id(doc)


def test_check_object_manifest_rejects_wrong_block_size():
    """★ A manifest with any block size but 524 288 is refused, even when it is self-consistent and its id
    matches (an attacker computes ids too): ``BadRequest`` on ``manifest.block_size`` (contract K-1).

    Mutant: drop the ``block_size != OBJECT_BLOCK_SIZE`` comparison in ``check_object_manifest``.
    """
    for block_size in (4096, 1024 * 1024, 4 * 1024 * 1024):
        doc, oid = _consistent(block_size, size=5 * 1024 * 1024 + 3)
        with pytest.raises(BadRequest) as exc:
            check_object_manifest(doc, oid)
        assert exc.value.details["field"] == "manifest.block_size"


def test_check_object_manifest_id_mismatch_is_block_corrupt():
    doc, oid = _consistent(OBJECT_BLOCK_SIZE)
    other = "0" * 64
    with pytest.raises(BlockCorrupt):
        check_object_manifest(doc, other)
    tampered = {**doc, "media": "application/json"}
    with pytest.raises(BlockCorrupt):
        check_object_manifest(tampered, oid)


@pytest.mark.parametrize(
    ("change", "field"),
    [
        (lambda d: {**d, "extra": 1}, "manifest"),
        (lambda d: {k: v for k, v in d.items() if k != "media"}, "manifest"),
        (lambda d: {**d, "kind": "ctxh.capsule"}, "manifest.kind"),
        (lambda d: {**d, "v": 2}, "manifest.v"),
        (lambda d: {**d, "v": True}, "manifest.v"),
        (lambda d: {**d, "v": 1.0}, "manifest.v"),
        (lambda d: {**d, "sha256": "A" * 64}, "manifest.sha256"),
        (lambda d: {**d, "block_size": float(OBJECT_BLOCK_SIZE)}, "manifest.block_size"),
        (lambda d: {**d, "size": -1, "blocks": []}, "manifest.size"),
        (lambda d: {**d, "size": True, "blocks": ["0" * 64]}, "manifest.size"),
        (lambda d: {**d, "size": jsonutil.OBJECT_MAX_BYTES + 1}, "manifest.size"),
        (lambda d: {**d, "blocks": d["blocks"] + ["0" * 64]}, "manifest.blocks"),
        (lambda d: {**d, "blocks": "0" * 64}, "manifest.blocks"),
        (lambda d: {**d, "blocks": ["g" * 64]}, "manifest.blocks[0]"),
        (lambda d: {**d, "media": "m" * 201}, "manifest.media"),
        (lambda d: {**d, "media": None}, "manifest.media"),
    ],
)
def test_check_object_manifest_rejects_malformed_documents(change, field):
    doc, _ = _consistent(OBJECT_BLOCK_SIZE)
    bad = change(doc)
    with pytest.raises(BadRequest) as exc:
        check_object_manifest(bad, object_id(bad))
    assert exc.value.details["field"] == field


@pytest.mark.parametrize("oid", ["", "0" * 63, "0" * 65, "G" * 64, "A" * 64, None, 5])
def test_check_object_manifest_rejects_malformed_ids(oid):
    doc, _ = _consistent(OBJECT_BLOCK_SIZE)
    with pytest.raises(BadRequest) as exc:
        check_object_manifest(doc, oid)
    assert exc.value.details["field"] == "object_id"


def test_check_object_manifest_accepts_tuple_blocks_and_boundaries():
    doc, raw = object_manifest(os.urandom(OBJECT_BLOCK_SIZE), MEDIA)
    oid = object_id(doc)
    assert len(raw) == 1
    check_object_manifest({**doc, "blocks": tuple(doc["blocks"])}, oid)
    empty, _ = object_manifest(b"", "")
    check_object_manifest(empty, object_id(empty))
