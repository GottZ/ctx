"""Block store, object manifests and the block frame codec (DESIGN §4.6, §5.1–§5.2; TESTPLAN §2.4)."""

from __future__ import annotations

import hashlib
import io
import math
import os
import stat
import struct
import subprocess
import sys
import time
import zlib
from dataclasses import replace

import pytest

from ctx_handover.engine import blocks, jsonutil
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
from ctx_handover.engine.errors import BadRequest, BlockCorrupt, Cancelled, NotFound, VerifyFailed
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Mode, ObjectManifest, Role
from ctx_handover.tests.fakes.blocks_fake import MemoryBlockStore
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


def _age(store: BlockStore, *ms: ObjectManifest, seconds: float = 3600) -> None:
    """Make objects look stored ``seconds`` ago (block files and manifest file), like a cache from earlier jobs."""
    then = time.time() - seconds
    for m in ms:
        for path in (store.manifest_path(m.object_id), *(store.block_path(b) for b in m.blocks)):
            os.utime(path, (then, then))


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
    with pytest.raises(BlockCorrupt):  # K-1: an id that does not match its manifest is a hash mismatch
        store.put_manifests([forged])
    too_few = replace(m, size=BLOCK_SIZE + 1, object_id="")
    with pytest.raises(BadRequest):
        store.put_manifests([replace(too_few, object_id=blocks.object_id_of(too_few))])
    odd_block = replace(m, block_size=1024, object_id="")
    with pytest.raises(BadRequest):
        store.put_manifests([replace(odd_block, object_id=blocks.object_id_of(odd_block))])
    assert store.put_manifests([m]) == 0  # already known


@pytest.mark.parametrize("size", [0, 1, BLOCK_SIZE, BLOCK_SIZE + 1, 2 * BLOCK_SIZE + 3])
def test_object_ids_follow_the_k1_rule(store, size):
    """X01/K-1: ``put_bytes``, ``put_stream`` and the in-memory fake give the object id of
    ``jsonutil.object_manifest`` — one rule, an empty object has no blocks."""
    data = _noise(size, seed=size)
    doc, parts = jsonutil.object_manifest(data, "application/octet-stream")
    want = jsonutil.object_id(doc)
    streamed = store.put_stream(io.BytesIO(data), media="application/octet-stream", cancelled=lambda: False)
    for m in (store.put_bytes(data, "application/octet-stream"), streamed):
        assert m.object_id == want and list(m.blocks) == [b for b, _ in parts]
        assert blocks.manifest_doc(m) == doc
    assert MemoryBlockStore().put_bytes(data, "application/octet-stream").object_id == want


def test_stored_manifest_with_foreign_block_size_is_refused(store):
    """K-1: readers accept only ``block_size == 524288``; a manifest file with another value (and a matching id)
    is refused on read."""
    doc = jsonutil.object_manifest_doc(_sha(b"x"), 1, 1024, [_sha(b"x")], "application/octet-stream")
    oid = jsonutil.object_id(doc)
    path = store.manifest_path(oid)
    blocks.write_private(path, jsonutil.canonical_bytes(doc), store.root / "blocks" / "tmp")
    with pytest.raises(BadRequest):
        store.manifest(oid)


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
    _age(store, dead)  # cached before the live job began (newer blocks are kept while it runs)
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


def _manifest_gone(store, ledger, m) -> bool:
    return not store.manifest_path(m.object_id).exists() and ledger.object_get(m.object_id) is None


def _manifest_kept(store, ledger, m) -> bool:
    return store.manifest_path(m.object_id).is_file() and ledger.object_get(m.object_id) is not None


def test_manifest_gc_collects_finished_and_aborted_jobs(plugin_data, ledger, monkeypatch):
    """★ R28: ``release_job(keep=False)`` and ``gc`` remove the manifest files and ``objects`` rows of objects that
    no non-terminal job references (committed and aborted jobs, unreferenced objects); objects of a live job — also
    one shared with a finished job — keep both, and so does a manifest (re-)stored within the grace window. The
    finished jobs' objects were stored before the live job began (``_age``): what is touched while a job runs
    stays until it ends (``test_gc_keeps_objects_in_flight_of_a_running_job``).

    Red proof: mutant = ``release_job`` and ``gc`` without ``self.collect_manifests(…)`` → the aborted job's
    manifest file is still there after ``release_job(keep=False)``.
    """
    monkeypatch.setattr(blocks, "GC_BATCH", 2)  # several write transactions per walk
    later = time.time() + 2 * blocks.MANIFEST_GRACE_S
    store = BlockStore(plugin_data, ledger, clock=lambda: later)
    done, aborted, live = "01JOBDONE00000000000000000", "01JOBABRT00000000000000000", "01JOBLIVE00000000000000000"
    for job in (done, aborted, live):
        ledger.create_job(job, Role.TARGET, Mode.PUSH, None, {})
    m_done = store.put_bytes(_noise(BLOCK_SIZE + 9, seed=11), "application/octet-stream")
    m_aborted = store.put_bytes(_noise(700, seed=12), "application/json")
    m_live = store.put_bytes(_noise(800, seed=13), "application/json")
    m_shared = store.put_bytes(_noise(900, seed=14), "application/json")
    m_orphan = store.put_bytes(_noise(1000, seed=15), "application/json")
    _age(store, m_done, m_aborted, m_shared, m_orphan)
    for job, m in ((done, m_done), (done, m_shared), (aborted, m_aborted), (live, m_live), (live, m_shared)):
        ledger.job_object(job, m.object_id)
    end_job(ledger, done, Role.TARGET, "committed")
    end_job(ledger, aborted, Role.TARGET, "aborted")

    store.release_job(aborted, keep=False)
    assert _manifest_gone(store, ledger, m_aborted)
    assert not any(store.has(b) for b in m_aborted.blocks)
    for m in (m_done, m_live, m_shared, m_orphan):  # not objects of the released job
        assert _manifest_kept(store, ledger, m)

    store.gc(keep_days=3, max_bytes=1 << 40)
    assert _manifest_gone(store, ledger, m_done)
    assert _manifest_gone(store, ledger, m_orphan)
    assert _manifest_kept(store, ledger, m_live)
    assert _manifest_kept(store, ledger, m_shared)
    assert store.has_object(m_live.object_id) and store.has_object(m_shared.object_id)
    assert all(store.has(b) for b in m_done.blocks)  # blocks stay cached within the cap; only manifests go
    # a collected object stored again is a new manifest; a live job's object is never collected
    assert store.put_manifests([m_done]) == 1
    end_job(ledger, live, Role.TARGET, "committed")
    assert store.collect_manifests() == 3  # m_done (re-stored, unreferenced), m_live, m_shared
    assert not [p for p in (plugin_data / "manifests").rglob("*.json")]


def test_manifest_gc_grace_covers_store_before_job_object(plugin_data, ledger):
    """A manifest (re-)stored within ``MANIFEST_GRACE_S`` survives GC although no job references it yet: the
    window between ``put_manifests`` and ``ledger.job_object``. Re-storing an old manifest refreshes it.

    Red proof: mutant = ``_store_manifest`` without ``os.utime`` (an existing file is left as it is) → the re-stored
    old manifest is collected.
    """
    store = BlockStore(plugin_data, ledger)
    fresh = store.put_bytes(_noise(600, seed=21), "application/json")
    old = store.put_bytes(_noise(650, seed=22), "application/json")
    stale = time.time() - 2 * blocks.MANIFEST_GRACE_S
    os.utime(store.manifest_path(old.object_id), (stale, stale))
    assert store.put_manifests([old]) == 0  # known already: only refreshed
    assert store.collect_manifests() == 0
    assert _manifest_kept(store, ledger, fresh) and _manifest_kept(store, ledger, old)
    os.utime(store.manifest_path(old.object_id), (stale, stale))
    assert store.collect_manifests() == 1
    assert _manifest_gone(store, ledger, old)
    assert _manifest_kept(store, ledger, fresh)


def test_gc_keeps_objects_in_flight_of_a_running_job(plugin_data, ledger):
    """★ Befund d: an object a running job stored but has not registered with itself yet keeps its blocks and
    manifest over the cache cap — through ``gc`` and through ``release_job(keep=False)`` of a finished job whose
    cached blocks it re-uses — whether the job wrote new blocks (``put_bytes``), stored an identical object again,
    or found old blocks present through ``missing`` and stored only the rest and the manifest (the sink protocol
    ``gitops.put_stream`` uses). Untouched cache still goes, and once no job runs the same GC frees everything.

    Red proofs: mutant = ``_protect_since`` returns ``math.inf`` (no protection) → the in-flight objects lose
    their blocks (reading ``new`` raises ``NotFound``); mutant = ``missing`` without ``_touch`` (ledger LRU only)
    → the manifest-only object loses its re-used block.
    """
    store = BlockStore(plugin_data, ledger, cache_bytes=0, cache_days=3)
    data_a = _noise(2 * BLOCK_SIZE + 11, seed=41)
    data_b = _noise(BLOCK_SIZE + 12, seed=42)
    finished = ulid()
    ledger.create_job(finished, Role.TARGET, Mode.PUSH, None, {})
    old_a = store.put_bytes(data_a, "application/octet-stream")
    old_b = store.put_bytes(data_b, "application/octet-stream")
    for m in (old_a, old_b):
        ledger.job_object(finished, m.object_id)
    end_job(ledger, finished, Role.TARGET, "committed")
    _age(store, old_a, old_b)  # the cache of an earlier handover

    running = ulid()
    ledger.create_job(running, Role.TARGET, Mode.PUSH, None, {})
    data_new = _noise(BLOCK_SIZE + 13, seed=43)
    new = store.put_bytes(data_new, "application/octet-stream")
    again = store.put_bytes(data_a, "application/octet-stream")
    assert again == old_a
    tail = _noise(100, seed=44)
    parts = [(old_b.blocks[0], data_b[:BLOCK_SIZE]), (_sha(tail), tail)]
    need = store.missing([b for b, _ in parts])
    assert need == [_sha(tail)]
    store.write_blocks([(b, raw) for b, raw in parts if b in need])
    data_mixed = data_b[:BLOCK_SIZE] + tail
    mixed = blocks.verify_manifest(
        ObjectManifest(
            sha256=_sha(data_mixed),
            size=len(data_mixed),
            block_size=BLOCK_SIZE,
            blocks=tuple(b for b, _ in parts),
            media="application/octet-stream",
        )
    )
    store.put_manifests([mixed])

    store.gc(keep_days=3, max_bytes=0)  # every block not live is beyond the cap
    store.release_job(finished, keep=False)
    for m, data in ((new, data_new), (again, data_a), (mixed, data_mixed)):
        with store.open_object(m.object_id) as fh:
            assert fh.read() == data
    assert not store.has(old_b.blocks[1])  # the cache nobody touched went
    assert not store.manifest_path(old_b.object_id).exists()

    for m in (new, again, mixed):
        ledger.job_object(running, m.object_id)
    end_job(ledger, running, Role.TARGET, "aborted")
    store.gc(keep_days=3, max_bytes=0)
    assert not [b for m in (new, again, mixed) for b in m.blocks if store.has(b)]


def test_gc_spares_a_block_stored_again_while_aside(store, ledger, monkeypatch):
    """A writer that comes while GC has a block renamed aside finds it missing and stores it again; GC then deletes
    only its own renamed copy, and the block keeps its ledger row (it is cached again)."""
    raw = _noise(300, seed=51)
    block_id = store.put(raw)
    os.utime(store.block_path(block_id), (time.time() - 3600,) * 2)
    real = store._protect_since

    def writer_comes() -> float:
        assert not store.has(block_id)  # aside
        assert store.missing([block_id]) == [block_id]
        assert store.write_blocks([(block_id, raw)]) == (1, [])
        return real()

    monkeypatch.setattr(store, "_protect_since", writer_comes)
    assert store.gc(keep_days=0, max_bytes=0) == 0
    assert store.get(block_id) == raw
    assert block_id in ledger.gc_candidates(math.inf, 0)
    assert not [p for p in (store.root / "blocks" / "tmp").iterdir() if p.name.startswith("gc.")]


def test_gc_puts_back_blocks_a_dead_gc_left_aside(store, ledger):
    """A GC that died between renaming a block aside and deciding on it does not lose the block: the next GC puts
    it back (restoring an unneeded block only keeps it cached)."""
    raw = _noise(400, seed=52)
    block_id = store.put(raw)
    done = subprocess.run(
        [sys.executable, "-c", "import os; print(os.getpid())"], capture_output=True, text=True, check=True
    )
    tomb = store.root / "blocks" / "tmp" / f"gc.{block_id}.{int(done.stdout)}.1.1"
    os.rename(store.block_path(block_id), tomb)
    store.gc(keep_days=3, max_bytes=1 << 40)
    assert store.get(block_id) == raw and not tomb.exists()


def test_gc_protection_reaches_back_at_most_a_day(store, monkeypatch):
    """A job stuck for days (suspended, crashed) does not switch off the cache cap for good: the protection of
    objects in flight starts at the oldest running job, but no earlier than ``MAX_IN_FLIGHT_S`` ago."""
    now = time.time()
    monkeypatch.setattr(store._ledger, "jobs", lambda **kw: [{"created_at": now - 600}])
    assert store._protect_since() == pytest.approx(now - 600 - blocks.GC_SLACK_S, abs=1)
    monkeypatch.setattr(store._ledger, "jobs", lambda **kw: [{"created_at": now - 3 * 86_400}, {"created_at": now}])
    assert store._protect_since() == pytest.approx(now - blocks.MAX_IN_FLIGHT_S, abs=5)
    monkeypatch.setattr(store._ledger, "jobs", lambda **kw: [])
    assert store._protect_since() == math.inf
    monkeypatch.setattr(store._ledger, "jobs", lambda **kw: [{"created_at": None}])  # unknown start: keep a day
    assert store._protect_since() == pytest.approx(now - blocks.MAX_IN_FLIGHT_S, abs=5)
