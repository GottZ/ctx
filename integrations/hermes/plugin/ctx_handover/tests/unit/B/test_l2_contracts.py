# ruff: noqa: F811  (pytest fixtures imported from test_lock_support are redefined as parameters)
"""Layer-2 contract points of cluster B against the real modules of its neighbours.

* X08/W006 — veto texts and the placeholder rewind of reclaim come from the same ledger rows; release/reclaim
  delete the job's veto rows (``ledger.veto_remove``). Run on the real ``ledger.Ledger`` (cluster A) and on the
  same ledger answering ``veto_rows`` with only the columns INTERFACES §4.5 promises.
* W004 — ``lock.suspend`` (DESIGN §7.3): an idle source gives up its leases before prepare and resumes.
* X04/W086 — ``session_import.map_model`` returns ``model_change`` in its one form; ``offline_capable`` is the one
  offline rule.
* X01/K-1, W061 — ``session_hash`` objects carry the same ids as cluster C's ``BlockStore`` (also the empty
  object); every read checks the manifest against the requested id and reads through ``read_object_bytes``.
* X09/W105 — retire step r3 stores the flat outgoing git state that cluster E's reader understands.
* W085 — ``hermes_files.discover`` reads a sequence of rows without changing it.

Every session comes from a real ``SessionDB`` built through the core's write paths; vetoed turns run through the
real agent loop (``test_lock_support.vetoed_turn``).
"""

from __future__ import annotations

import copy

import pytest

from ctx_handover.engine import hermes_files, jsonutil, lock, session_hash, session_import
from ctx_handover.engine.errors import BadRequest, BlockCorrupt, InvalidState, PayloadTooLarge
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import HermesPaths, MapRow, ObjectManifest, Options
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.unit.B.support import MemoryBlockStore
from ctx_handover.tests.unit.B.test_lock_support import (  # noqa: F401 (fixtures)
    JOB,
    PEER,
    ack,
    build,
    chat_requests,
    describe,
    locked,
    now,
    prepared,
    provider_url,
    vetoed_turn,
)

OTHER = "pid=1:turn=other:platform=cli"
MiB = 1024 * 1024
IF45_VETO_COLUMNS = ("session_id", "root_id", "state", "peer", "title", "at", "job")
"""``veto_rows`` columns INTERFACES §4.5 (Z. 786) promises — the minimum any ledger answers with."""


class If45Ledger(Ledger):
    """The real ledger answering ``veto_rows`` with only the IF §4.5 columns (no ``ack``, no ``prepared_at``)."""

    def veto_rows(self) -> list[dict]:
        rows = []
        for r in super().veto_rows():
            row = {k: r.get(k) for k in IF45_VETO_COLUMNS}
            row["job"] = r.get("job") or r.get("job_id")
            rows.append(row)
        return rows


def _ledger(tmp_path, cls=Ledger) -> Ledger:
    return cls(tmp_path / "ledger.sqlite3")


@pytest.fixture
def real_ledger(tmp_path):
    led = _ledger(tmp_path)
    try:
        yield led
    finally:
        led.close()


def _marks(db, tip, lineage) -> dict:
    """What the primary lock changes (DESIGN §7.4 p2–p6) plus the drivers."""
    return {
        "rows": {
            s: {k: db.get_session(s)[k] for k in ("archived", "hidden", "ended_at", "end_reason", "title")}
            for s in lineage
        },
        "meta": {k: db.get_meta(f"{k}:{tip}") for k in lock.DRIVER_KEYS},
    }


def _turn_lease_free(db, tip) -> bool:
    got = db.try_acquire_session_turn_lease(tip, OTHER, ttl_seconds=30, patience_s=0.5)
    if got:
        db.release_session_turn_lease(tip, OTHER)
    return bool(got)


# ---------------------------------------------------------------------------------------------------------
# X08 / W006: one text source for the veto and the rewind; veto rows removed on release/reclaim
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("columns", ["ledger", "if45"])
def test_reclaim_rewinds_the_texts_the_hook_showed(session_db, make_session, provider_url, tmp_path, columns):
    """★ Two real vetoed turns after retire, answered by a ``VetoIndex`` on the real ledger; reclaim rewinds both
    placeholder pairs, returns the typed texts and deletes the job's veto rows (X08, W006). With the full
    ``veto_rows`` of cluster A the hook names the commit time and the target title (W034); with only the IF §4.5
    columns it cannot, and reclaim still finds exactly the text that was shown.

    Mutant: reclaim builds its texts from the source row only (``_job_texts`` returns ``_all_texts(entry)``, the
    layer-1 rule) ⇒ ``if45``: the shown text matches none of them, ``rewound == 0``.
    Mutant: ``_undo`` without ``ledger.veto_remove`` ⇒ the veto rows outlive the reclaim.
    """
    led = _ledger(tmp_path, Ledger if columns == "ledger" else If45Ledger)
    index = lock.VetoIndex(led.path, poll_s=0.05, ledger_factory=type(led))
    try:
        tip = build(make_session)
        info, holder = prepared(session_db, led, tip)
        lock.retire(session_db, led, info, holder, JOB, ack("Work", committed_at=1_762_650_000.0), [])
        index.refresh_now(force=True)
        before = len(session_db.get_messages(tip))
        shown = [vetoed_turn(session_db, tip, provider_url, index, f"typed {i}")["final_response"] for i in range(2)]
        assert chat_requests() == []
        assert shown[0] == shown[1] == index.lookup(tip)["message"]
        if columns == "ledger":
            assert "on 2025-11-09 01:00 UTC" in shown[0] and 'continues there as "Work"' in shown[0]
        else:
            assert "an unknown time" in shown[0]
        assert len(session_db.get_messages(tip)) == before + 4

        out = lock.reclaim(session_db, led, tip, force=False)
        assert out["state"] == "reclaimed"
        assert (out["rewound"], out["kept"]) == (4, 0)
        assert out["withheld"] == ["typed 0", "typed 1"]
        live = session_db.get_messages(tip)
        assert len(live) == before
        assert not any(m.get("content") == shown[0] for m in live)
        assert [r for r in led.veto_rows() if (r.get("job_id") or r.get("job")) == JOB] == []
        index.refresh_now()
        assert all(index.lookup(s) is None for s in info.lineage_ids)
    finally:
        index.stop()
        led.close()


def test_release_removes_veto_rows_and_a_released_job_never_prepares_again(session_db, make_session, real_ledger):
    """W006: release after prepare deletes the job's veto rows (inverse of p1). The real ledger lets the same job
    lock again from ``released``; its prepare then refuses (``InvalidState``, reason ``undone``) instead of
    replaying the journaled steps of the restored attempt, which would set ``prepared`` without a single mark.

    Mutant: ``prepare`` without the ``undone`` check ⇒ ``prepared`` while the session is neither archived nor
    hidden nor vetoed.
    """
    tip = build(make_session)
    unlocked = _marks(session_db, tip, describe(session_db, tip).lineage_ids)
    info, holder = prepared(session_db, real_ledger, tip)
    assert {r["session_id"] for r in real_ledger.veto_rows()} == set(info.lineage_ids)
    lock.release(session_db, real_ledger, info, holder, JOB)
    assert real_ledger.source_state(JOB)["state"] == "released"
    assert real_ledger.veto_rows() == []
    assert _marks(session_db, tip, info.lineage_ids) == unlocked
    info, holder = locked(session_db, real_ledger, tip)
    assert real_ledger.source_state(JOB)["state"] == "locked"
    with pytest.raises(InvalidState) as err:
        lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    assert err.value.details["reason"] == "undone"
    assert real_ledger.source_state(JOB)["state"] == "locked"
    assert _marks(session_db, tip, info.lineage_ids) == unlocked
    lock.release(session_db, real_ledger, info, holder, JOB)
    assert real_ledger.source_state(JOB)["state"] == "released" and _turn_lease_free(session_db, tip)


def test_reclaim_force_on_a_locked_pull_source(session_db, make_session, real_ledger):
    """DESIGN §7.7 ``locked`` + ``--force``: cancel, then release. On the source of a pull job the conductor is the
    peer — the real ledger has no conductor row to cancel (``set_cancel`` ⇒ ``NotFound``); the release still runs
    and the peer learns ``released`` on its next contact.

    Mutant: ``reclaim`` lets ``NotFound`` from ``set_cancel`` through ⇒ the session stays locked.
    """
    tip = build(make_session)
    locked(session_db, real_ledger, tip)
    out = lock.reclaim(session_db, real_ledger, tip, force=True)
    assert out["state"] == "released"
    assert real_ledger.source_state(JOB)["state"] == "released"
    assert _turn_lease_free(session_db, tip)


# ---------------------------------------------------------------------------------------------------------
# W004: suspend
# ---------------------------------------------------------------------------------------------------------


def test_suspend_gives_up_the_leases_before_prepare_and_resumes(session_db, make_session, real_ledger):
    """★ DESIGN §7.3 (W004): an idle source in ``locked`` gives up the turn lease and the compression lock and is
    ``suspended`` — the session is usable at once and carries no mark and no veto row. ``suspend`` again is a
    no-op; ``acquire`` of the same job locks again and prepares. From ``prepared`` on, suspend refuses and leaves
    leases and state alone (only release/reclaim undo the primary lock).

    Mutant: ``suspend`` without ``_release_leases`` ⇒ a turn cannot get the lease.
    Mutant: ``suspend`` also accepts ``preparing``/``prepared`` ⇒ no ``InvalidState``, the prepared source loses
    its leases.
    """
    tip = build(make_session)
    unlocked = _marks(session_db, tip, describe(session_db, tip).lineage_ids)
    info, holder = locked(session_db, real_ledger, tip)
    assert not _turn_lease_free(session_db, tip)
    lock.suspend(session_db, real_ledger, info, holder, JOB)
    assert real_ledger.source_state(JOB)["state"] == "suspended"
    assert _turn_lease_free(session_db, tip)
    assert session_db.get_compression_lock_holder(tip) is None
    assert _marks(session_db, tip, info.lineage_ids) == unlocked
    assert real_ledger.veto_rows() == []
    lock.suspend(session_db, real_ledger, info, None, JOB)
    assert real_ledger.source_state(JOB)["state"] == "suspended"

    info, holder = locked(session_db, real_ledger, tip)
    assert real_ledger.source_state(JOB)["state"] == "locked"
    lock.prepare(session_db, real_ledger, info, JOB, PEER, now())
    assert real_ledger.source_state(JOB)["state"] == "prepared"
    with pytest.raises(InvalidState):
        lock.suspend(session_db, real_ledger, info, holder, JOB)
    assert real_ledger.source_state(JOB)["state"] == "prepared"
    assert session_db.get_compression_lock_holder(tip) == holder
    assert not _turn_lease_free(session_db, tip)


def test_release_of_a_suspended_job(session_db, make_session, real_ledger):
    """A cancelled idle job: release from ``suspended`` ends in ``released`` (A's abort path calls release for
    every state but ``released``/``reclaimed``); nothing was marked, nothing is restored.

    Mutant: ``suspended`` not releasable ⇒ ``InvalidState`` and the job stays ``suspended``.
    """
    tip = build(make_session)
    info, holder = locked(session_db, real_ledger, tip)
    lock.suspend(session_db, real_ledger, info, holder, JOB)
    lock.release(session_db, real_ledger, info, None, JOB)
    assert real_ledger.source_state(JOB)["state"] == "released"
    assert _turn_lease_free(session_db, tip)


# ---------------------------------------------------------------------------------------------------------
# X04 / W086: model_change form and the one offline rule
# ---------------------------------------------------------------------------------------------------------

_TARGET = {
    "model": {"default": "qwen3:32b", "provider": "ollama", "context_length": 65_536},
    "providers": {
        "lan": {
            "base_url": "http://192.168.1.9:8080/v1",
            "context_length": 32_768,
            "models": {"qwen-big": {"context_length": 131_072}},
        },
        "cloud": {"base_url": "https://api.example.com/v1"},
    },
}


def test_map_model_change_has_the_contract_form():
    """★ X04/W086: ``model_change`` is ``None`` when the model stays, else exactly ``{"from": {model, provider},
    "to": {model, provider, context_length}, "reason", "offline_capable"}``; ``offline_capable`` equals
    ``session_import.offline_capable`` on the resulting route and ``context_length`` is this host's configured
    value (``None`` when it configures none).

    Mutant: the layer-1 form ``{"old", "new", "reason"}``.
    Mutant: ``offline_capable`` missing from the change record.
    """
    row = {"model": "claude-x", "model_config": {"provider": "anthropic"}}
    fields, change = session_import.map_model(row, "auto", None, _TARGET, lambda n, c: False)
    assert change == {
        "from": {"model": "claude-x", "provider": "anthropic"},
        "to": {"model": "qwen3:32b", "provider": "ollama", "context_length": 65_536},
        "reason": "provider_unusable",
        "offline_capable": True,
    }
    assert change["offline_capable"] is session_import.offline_capable(fields["model_config"], _TARGET)

    cloud = {"model": {"default": "gpt-x", "provider": "openrouter"}, "providers": {}}
    fields, change = session_import.map_model(row, "target_default", None, cloud, lambda n, c: True)
    assert change == {
        "from": {"model": "claude-x", "provider": "anthropic"},
        "to": {"model": "gpt-x", "provider": "openrouter", "context_length": None},
        "reason": "target_default",
        "offline_capable": False,
    }

    _fields, change = session_import.map_model(row, "auto", "my-model", _TARGET, lambda n, c: True)
    assert change["to"] == {"model": "my-model", "provider": "ollama", "context_length": None}
    assert change["reason"] == "override" and change["offline_capable"] is True

    kept = {"model": "qwen-big", "model_config": {"gateway_runtime": {"provider": "lan"}}}
    fields, change = session_import.map_model(kept, "auto", None, _TARGET, lambda n, c: n == "lan")
    assert change is None and fields["model_config"] == {"gateway_runtime": {"provider": "lan"}}


@pytest.mark.parametrize(
    ("model_config", "model", "expected"),
    [
        ({"gateway_runtime": {"provider": "lan"}}, "qwen-big", 131_072),
        ({"gateway_runtime": {"provider": "lan"}}, "qwen-small", 32_768),
        ({"gateway_runtime": {"provider": "cloud"}}, "x", None),
        (None, "qwen3:32b", 65_536),
        (None, "another", None),
        ({"gateway_runtime": {"provider": "unknown"}}, "qwen3:32b", 65_536),
    ],
)
def test_context_length_from_this_host_only(model_config, model, expected):
    assert session_import.context_length(model_config, model, _TARGET) == expected


def test_context_length_ignores_non_positive_values():
    cfg = {
        "model": {"default": "m", "provider": "p", "context_length": True},
        "providers": {"x": {"context_length": 0}},
    }
    assert session_import.context_length(None, "m", cfg) is None
    assert session_import.context_length({"gateway_runtime": {"provider": "x"}}, "m", cfg) is None


# ---------------------------------------------------------------------------------------------------------
# X01 / K-1, W061: object ids, manifest check, reads through the block store
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("size", [0, 1, 524_288, 524_289], ids=["empty", "one", "one-block", "two-blocks"])
def test_object_ids_equal_in_every_store(tmp_path, size):
    """★ X01/K-1: the same bytes get the same object id through the bare ``BlockSink`` path of ``session_hash`` and
    through cluster C's ``BlockStore.put_bytes`` — K0's ``jsonutil.object_id(object_manifest(...))`` — also the
    empty object (no blocks); both stores read the object back through ``session_hash.read_object`` (the real
    store via ``read_object_bytes``, W061).

    Mutant: ``put_object`` with the layer-1 rule ``range(0, max(len(data), 1), …)`` ⇒ the empty object gets one
    block and another id than cluster C's.
    """
    from ctx_handover.engine.blocks import BlockStore

    data = bytes(i % 251 for i in range(size))
    led = _ledger(tmp_path)
    try:
        real = BlockStore(tmp_path / "data", led)
        mem = MemoryBlockStore()
        doc, _blocks = jsonutil.object_manifest(data, session_hash.MEDIA_JSON)
        expected = jsonutil.object_id(doc)
        m_mem = session_hash.put_object(mem, data)
        m_real = session_hash.put_object(real, data)
        assert m_mem.object_id == m_real.object_id == expected
        assert m_mem.blocks == m_real.blocks and len(m_mem.blocks) == -(-size // jsonutil.OBJECT_BLOCK_SIZE)
        assert session_hash.read_object(mem, expected, max_bytes=2 * MiB) == data
        assert session_hash.read_object(real, expected, max_bytes=2 * MiB) == data
    finally:
        led.close()


def test_read_uses_read_object_bytes_of_the_store(tmp_path):
    """W061: a store with ``read_object_bytes`` serves the bytes; the size cap still holds before it is asked."""
    from ctx_handover.engine.blocks import BlockStore

    led = _ledger(tmp_path)
    try:
        real = BlockStore(tmp_path / "data", led)
        calls: list[tuple[str, int]] = []
        inner = real.read_object_bytes

        def spy(object_id, *, max_bytes):
            calls.append((object_id, max_bytes))
            return inner(object_id, max_bytes=max_bytes)

        real.read_object_bytes = spy
        oid = session_hash.put_json(real, {"k": "v" * 100}).object_id
        assert session_hash.read_json(real, oid, max_bytes=MiB) == {"k": "v" * 100}
        assert calls == [(oid, MiB)]
        with pytest.raises(PayloadTooLarge):
            session_hash.read_object(real, oid, max_bytes=10)
        assert len(calls) == 1
    finally:
        led.close()


def test_reader_refuses_foreign_block_size_and_swapped_manifest():
    """K-1: a manifest written with another block size (the layer-1 stand-in rule, 4096) is refused as
    ``BadRequest``; a manifest swapped under an existing id as ``BlockCorrupt`` — both before any block is read.

    Mutant: ``read_object`` without ``jsonutil.check_object_manifest`` ⇒ both objects are read.
    """
    store = MemoryBlockStore()
    data = b"x" * 10
    bid = jsonutil.sha256_hex(data)
    store.write_blocks([(bid, data)])
    doc = jsonutil.object_manifest_doc(jsonutil.sha256_hex(data), len(data), 4096, [bid], session_hash.MEDIA_JSON)
    small = jsonutil.object_id(doc)
    store.put_manifests([ObjectManifest(doc["sha256"], len(data), 4096, (bid,), session_hash.MEDIA_JSON, small)])
    with pytest.raises(BadRequest):
        session_hash.read_object(store, small, max_bytes=MiB)
    good = session_hash.put_object(store, b"y" * 10).object_id
    store.tamper(good, lambda raw: raw.replace(b"y", b"z", 1))
    store.reads.clear()
    with pytest.raises(BlockCorrupt):
        session_hash.read_object(store, good, max_bytes=MiB)
    assert store.reads == []


# ---------------------------------------------------------------------------------------------------------
# X09 / W105: the outgoing git state in the workspace map
# ---------------------------------------------------------------------------------------------------------


def test_retire_r3_stores_the_flat_git_state(session_db, make_session, real_ledger):
    """X09/W105 round trip: the ``out`` row r3 writes carries the flat state of cluster E's ``map_state_of``
    unchanged through the real ledger, and E's reader rebuilds the same ``GitState`` from it (the return mode of
    R4 depends on it)."""
    from ctx_handover.engine import materialize
    from ctx_handover.engine.model import GitState

    git = GitState(
        head="a" * 40,
        index_tree="b" * 40,
        worktree_tree="c" * 40,
        branch="refs/heads/main",
        exclude_pathspecs=(":(exclude,literal).env",),
    )
    flat = materialize.map_state_of(git, git.branch)
    assert set(flat) == {"head", "index_tree", "worktree_tree", "branch", "exclude_pathspecs"}
    row = MapRow("/srv/work", "PEER1", "/home/u/work", "out", "r" * 40, flat, JOB)
    tip = build(make_session)
    info, holder = prepared(session_db, real_ledger, tip)
    lock.retire(session_db, real_ledger, info, holder, JOB, ack(), [row])
    stored = real_ledger.map_out("/srv/work", "PEER1")
    assert stored is not None and stored.state == flat
    assert materialize.git_state_of(stored.state) == git


# ---------------------------------------------------------------------------------------------------------
# W085: discover reads a sequence and leaves it alone
# ---------------------------------------------------------------------------------------------------------


def test_discover_reads_a_sequence_without_changing_it(hermes_home, user_home):
    """W085: cluster D hands the transcript rows to ``discover`` and reads them again afterwards; the rows stay
    unchanged and every result has ``kind``, ``source_path``, ``rel``."""
    spill = hermes_home / "cache" / "spillover"
    spill.mkdir(parents=True)
    (spill / "out.txt").write_text("full output")
    rows = (
        {"role": "tool", "content": f"Full output saved to: {spill / 'out.txt'}"},
        {"role": "user", "content": [{"type": "text", "text": "hi"}]},
    )
    snapshot = copy.deepcopy(rows)
    cfg = PolicyConfig.from_options(
        Options(),
        home=user_home,
        hermes_home=hermes_home,
        hermes_root=hermes_home,
        plugin_dir=hermes_home / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
        document_cache=hermes_home / "cache" / "documents",
    )
    paths = HermesPaths(home=str(hermes_home), document_cache=str(hermes_home / "cache" / "documents"))
    found = hermes_files.discover(rows, [], paths, cfg)
    assert rows == snapshot
    assert [(f.kind, f.source_path, f.rel) for f in found] == [("spillover", str(spill / "out.txt"), "out.txt")]
    assert hermes_files.discover(rows, [], paths, cfg) == found
