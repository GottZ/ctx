"""Block store, object manifests and the block frame codec (DESIGN §4.6, §5.1–§5.2; TESTPLAN §2.4)."""

from __future__ import annotations

import hashlib
import io
import os
import stat
import struct
import zlib
from dataclasses import replace

import pytest

from ctx_handover.engine import blocks
from ctx_handover.engine.blocks import (
    BLOCK_SIZE,
    ENC_IDENTITY,
    ENC_ZLIB,
    ENC_ZSTD,
    FRAME_HEADER,
    FRAME_MAGIC,
    BlockStore,
    decode_frames,
    encode_frame,
    encode_frames,
)
from ctx_handover.engine.errors import BadRequest, Cancelled, NotFound, VerifyFailed
from ctx_handover.engine.model import Mode, Role
from ctx_handover.tests.unit.C.conftest import end_job

ALL_CAPS = frozenset({"enc.zlib", "enc.zstd"})


def _sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _noise(n: int, seed: int = 1) -> bytes:
    out = bytearray()
    counter = 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}:{counter}".encode()).digest()
        counter += 1
    return bytes(out[:n])


def _frame(block_id: str, enc: int, payload: bytes, *, magic: bytes = FRAME_MAGIC, length: int | None = None) -> bytes:
    n = len(payload) if length is None else length
    return magic + bytes.fromhex(block_id) + bytes([enc]) + struct.pack(">I", n) + payload


# ---------------------------------------------------------------------------------------------------------
# Block store
# ---------------------------------------------------------------------------------------------------------


def test_block_store_put_get_verify(store, ledger):
    raw = b"hello block"
    block_id = store.put(raw)
    assert block_id == _sha(raw)
    assert store.has(block_id)
    assert store.get(block_id) == raw
    assert store.put(raw) == block_id  # idempotent
    assert not store.has(_sha(b"other"))
    with pytest.raises(NotFound):
        store.get(_sha(b"other"))
    # a block whose file was altered on disk is never served
    path = store.block_path(block_id)
    os.chmod(path, 0o600)
    path.write_bytes(b"tampered")
    with pytest.raises(blocks.BlockCorrupt):
        store.get(block_id)
    # malformed ids are rejected before touching the filesystem
    for bad in ("", "../../etc/passwd", "A" * 64, "0" * 63):
        with pytest.raises(BadRequest):
            store.has(bad)
    with pytest.raises(BadRequest):
        store.put(b"x" * (BLOCK_SIZE + 1))


def test_block_store_atomic_write(store, monkeypatch):
    raw = _noise(1000)
    block_id = _sha(raw)
    real_replace = os.replace

    def failing_replace(src, dst):
        raise OSError("disk gone")

    monkeypatch.setattr(blocks.os, "replace", failing_replace)
    with pytest.raises(OSError):
        store.put(raw)
    monkeypatch.setattr(blocks.os, "replace", real_replace)
    assert not store.has(block_id)
    assert not store.block_path(block_id).exists()
    # no temp file is left behind and the final write lands with 0600
    tmp_dir = store.root / "blocks" / "tmp"
    assert not tmp_dir.exists() or list(tmp_dir.iterdir()) == []
    store.put(raw)
    assert stat.S_IMODE(store.block_path(block_id).stat().st_mode) == 0o600


def test_block_store_permissions(store):
    """Hardening #20 (SEC 6.1): directories 0700, files 0600 from the first byte."""
    m = store.put_bytes(_noise(BLOCK_SIZE + 10), "application/octet-stream")
    for path in (store.root / "blocks", store.block_path(m.blocks[0]).parent, store.manifest_path(m.object_id).parent):
        assert stat.S_IMODE(path.stat().st_mode) == 0o700, path
    assert stat.S_IMODE(store.manifest_path(m.object_id).stat().st_mode) == 0o600


def test_put_stream_manifest_and_open_object(store):
    data = _noise(2 * BLOCK_SIZE + 123)
    m = store.put_stream(io.BytesIO(data), media="application/octet-stream", cancelled=lambda: False)
    assert m.size == len(data)
    assert m.sha256 == _sha(data)
    assert m.block_size == BLOCK_SIZE
    assert len(m.blocks) == 3
    assert m.object_id == blocks.object_id_of(m)
    assert store.manifests([m.object_id]) == [m]
    with store.open_object(m.object_id) as fh:
        assert fh.read() == data
    # empty object
    empty = store.put_bytes(b"", "application/json")
    assert empty.blocks == () and empty.size == 0
    assert store.open_object(empty.object_id).read() == b""
    with pytest.raises(NotFound):
        store.manifests(["0" * 64])


def test_put_stream_honours_cancel(store):
    calls = []

    def cancelled():
        calls.append(1)
        return len(calls) > 1

    with pytest.raises(Cancelled):
        store.put_stream(io.BytesIO(_noise(3 * BLOCK_SIZE)), media="application/octet-stream", cancelled=cancelled)


def test_object_manifest_assembly_sha256(store):
    """★ Reordered blocks fail at assembly even when the manifest's own id is consistent (DESIGN §4.6 chain).

    Red proof: mutant = the content sha256 check at EOF in ``_ObjectReader`` removed → reading the reordered
    object returns the wrong bytes without ``VerifyFailed``.
    """
    data = _noise(2 * BLOCK_SIZE)
    m = store.put_bytes(data, "application/octet-stream")
    swapped = replace(m, blocks=(m.blocks[1], m.blocks[0]), object_id="")
    swapped = replace(swapped, object_id=blocks.object_id_of(swapped))
    assert store.put_manifests([swapped]) == 1
    with pytest.raises(VerifyFailed):
        store.open_object(swapped.object_id).read()


def test_put_manifests_recomputes_object_id(store):
    m = store.put_bytes(_noise(10), "application/octet-stream")
    forged = replace(m, object_id="f" * 64)
    with pytest.raises(BadRequest):
        store.put_manifests([forged])
    too_few = replace(m, size=BLOCK_SIZE + 1, object_id="")
    with pytest.raises(BadRequest):
        store.put_manifests([replace(too_few, object_id=blocks.object_id_of(too_few))])
    odd_block = replace(m, block_size=1024, object_id="")
    with pytest.raises(BadRequest):
        store.put_manifests([replace(odd_block, object_id=blocks.object_id_of(odd_block))])
    assert store.put_manifests([m]) == 0  # already known


def test_write_blocks_rejects_per_frame(store):
    good = _noise(100)
    bad = _noise(100, seed=2)
    accepted, rejected = store.write_blocks([(_sha(good), good), (_sha(good[:-1]), bad), ("zz", b"x")])
    assert accepted == 1
    assert {code for _id, code in rejected} == {"block_corrupt", "bad_request"}
    assert store.missing([_sha(good), _sha(bad)]) == [_sha(bad)]
    assert list(store.read_blocks([_sha(good)])) == [(_sha(good), good)]


# ---------------------------------------------------------------------------------------------------------
# Frames
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("caps", "expected_enc"),
    [(frozenset(), ENC_IDENTITY), (frozenset({"enc.zlib"}), ENC_ZLIB), (ALL_CAPS, ENC_ZSTD)],
)
def test_frames_roundtrip_all_encodings(caps, expected_enc):
    text = b"compressible line\n" * 4000
    noise = _noise(50_000)
    pairs = [(_sha(text), text), (_sha(noise), noise)]
    frames = [encode_frame(i, raw, peer_caps=caps) for i, raw in pairs]
    assert frames[0][36] == expected_enc
    assert frames[1][36] == ENC_IDENTITY  # gain < 5 % ⇒ identity
    body = b"".join(frames)
    assert list(decode_frames(body, block_size=BLOCK_SIZE)) == [(i, raw, None) for i, raw in pairs]


def test_encode_frames_batches_under_max_body():
    pairs = [(_sha(_noise(200_000, s)), _noise(200_000, s)) for s in range(5)]
    bodies = list(encode_frames(pairs, peer_caps=frozenset(), max_body=450_000))
    assert all(len(b) <= 450_000 for b in bodies)
    assert len(bodies) == 3  # two 200 kB frames (+ 41-byte headers) fit, a third would not
    decoded = [x for b in bodies for x in decode_frames(b, block_size=BLOCK_SIZE)]
    assert decoded == [(i, raw, None) for i, raw in pairs]
    # a single frame larger than max_body still travels alone
    big = [(_sha(_noise(BLOCK_SIZE)), _noise(BLOCK_SIZE))]
    assert len(list(encode_frames(big, peer_caps=frozenset(), max_body=1000))) == 1


@pytest.mark.parametrize("enc", [ENC_ZLIB, ENC_ZSTD])
def test_frames_decompression_bomb_capped(enc):
    """★ A frame that expands beyond the block size is rejected as ``too_large`` (DESIGN §4.6, T7).

    The raw content is 600 KiB of zeros and the frame's block id is its true sha256, so only the output cap
    stops it. Red proof: mutant = ``_decompress`` without ``max_length`` and without the size check → the
    oversized block is accepted (``(id, raw, None)``).
    """
    raw = bytes(600 * 1024)
    payload = zlib.compress(raw) if enc == ENC_ZLIB else blocks._zstd_compress(raw)
    assert len(payload) < BLOCK_SIZE
    out = list(decode_frames(_frame(_sha(raw), enc, payload), block_size=BLOCK_SIZE))
    assert out == [(_sha(raw), None, "too_large")]


def test_frames_bad_magic_and_length():
    raw = b"abc"
    ok = _frame(_sha(raw), ENC_IDENTITY, raw)
    # bad magic: parsing stops, the frames before stay valid
    out = list(decode_frames(ok + _frame(_sha(raw), ENC_IDENTITY, raw, magic=b"XXXX") + ok, block_size=BLOCK_SIZE))
    assert out[0] == (_sha(raw), raw, None)
    assert out[1][1:] == (None, "bad_frame")
    assert len(out) == 2
    # declared length above block size + 1024: rejected without reading the payload
    out = list(decode_frames(_frame(_sha(raw), ENC_IDENTITY, b"", length=BLOCK_SIZE + 1025), block_size=BLOCK_SIZE))
    assert out[0][1:] == (None, "bad_frame")
    # truncated payload
    out = list(decode_frames(ok[:-1], block_size=BLOCK_SIZE))
    assert out[0][1:] == (None, "bad_frame")
    # truncated header
    assert next(decode_frames(ok[: FRAME_HEADER - 1], block_size=BLOCK_SIZE))[1:] == (None, "bad_frame")
    # hash mismatch and unknown encoding reject only their own frame
    wrong = _frame(_sha(b"other"), ENC_IDENTITY, raw)
    unknown = _frame(_sha(raw), 9, raw)
    out = list(decode_frames(wrong + unknown + ok, block_size=BLOCK_SIZE))
    assert [o[2] for o in out] == ["block_corrupt", "bad_encoding", None]
    # garbage zlib stream
    out = list(decode_frames(_frame(_sha(raw), ENC_ZLIB, b"not zlib"), block_size=BLOCK_SIZE))
    assert out[0][1:] == (None, "bad_encoding")


# ---------------------------------------------------------------------------------------------------------
# GC
# ---------------------------------------------------------------------------------------------------------


def test_block_gc_respects_live_jobs(store, ledger):
    live = store.put_bytes(_noise(BLOCK_SIZE + 5, seed=3), "application/octet-stream")
    dead = store.put_bytes(_noise(100, seed=4), "application/octet-stream")
    ledger.create_job("01JOBLIVE00000000000000000", Role.TARGET, Mode.PUSH, None, {})
    ledger.job_object("01JOBLIVE00000000000000000", live.object_id)
    freed = store.gc(keep_days=0, max_bytes=0)
    assert freed == len(dead.blocks)
    assert all(store.has(b) for b in live.blocks)
    assert not any(store.has(b) for b in dead.blocks)
    assert store.missing(list(dead.blocks)) == list(dead.blocks)


def test_release_job_frees_over_cap(plugin_data, ledger):
    store = BlockStore(plugin_data, ledger, cache_bytes=2 * BLOCK_SIZE, cache_days=3)
    job = "01JOBDONE00000000000000000"
    ledger.create_job(job, Role.TARGET, Mode.PUSH, None, {})
    m = store.put_bytes(_noise(4 * BLOCK_SIZE, seed=5), "application/octet-stream")
    ledger.job_object(job, m.object_id)
    end_job(ledger, job, Role.TARGET, "committed")
    store.release_job(job, keep=True)
    remaining = [b for b in m.blocks if store.has(b)]
    assert 0 < len(remaining) <= 2
    # keep=False frees everything of the job that no live job needs
    store.release_job(job, keep=False)
    assert not any(store.has(b) for b in m.blocks)
