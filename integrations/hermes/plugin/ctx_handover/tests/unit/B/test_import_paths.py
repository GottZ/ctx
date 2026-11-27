"""Import paths A and B′ against real ``SessionDB`` stores (DESIGN §6.8, §6.9; TESTPLAN §2.3)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import session_export, session_import
from ctx_handover.engine.errors import BlockCorrupt, VerifyFailed
from ctx_handover.tests.fakes.sessiondb import large_segment
from ctx_handover.tests.unit.B.conftest import (
    assert_lineage_equal,
    export,
    plan,
    rich_lineage,
    roundtrip,
    stored_rows,
)


def test_import_path_a_whole_lineage(make_session, session_db, target_db, store, source_paths, target_cwd):
    """Two segments with compaction, rotation, rewind, multimodal, reasoning, api_content and tool-call uids:
    path A carries every non-rewind row byte-true; model and display history are equal."""
    tip = rich_lineage(make_session)
    info, cap, _capsule, p, prepared = roundtrip(session_db, target_db, store, tip, source_paths, target_cwd)
    assert [s.path for s in p.segments] == ["A", "A"]
    assert prepared.import_path == "A"
    assert (prepared.h_all, prepared.h_rows, prepared.h_model) == (cap.h_all, cap.h_rows, cap.h_model)
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [s.new_id for s in p.segments])
    rows = stored_rows(target_db, prepared.tip_id)
    assert any(r.get("tool_call_uids") for r in rows)
    assert any(isinstance(r.get("content"), list) for r in stored_rows(target_db, prepared.root_id))
    assert target_db.get_session(prepared.tip_id)["hidden"] == 1


@pytest.mark.parametrize("label", ["15613"])
def test_import_path_b_prime_lossless(make_session, session_db, target_db, store, source_paths, target_cwd, label):
    """★ A 15 613-row segment (5 205 live, 10 408 archived) goes through B′: every column of every row equal,
    model and display history equal, H_all/H_rows gate passed [SK H1, R2].

    Mutant: v1 path B (append only the live rows, no archived rows, no ``archive_and_compact``).
    """
    sid = large_segment(make_session, live=5205, archived=10408, cwd="/srv/src-work")
    make_session.db.set_session_title(sid, "Big")
    counts = make_session.counts(sid)
    assert (counts.active, counts.archived) == (5205, 10408)
    _info, cap, _capsule, p, prepared = roundtrip(session_db, target_db, store, sid, source_paths, target_cwd)
    assert p.segments[0].path == "B'"
    assert prepared.import_path == "B'"
    assert prepared.h_all == cap.h_all
    assert_lineage_equal(session_db, target_db, [sid], [prepared.tip_id])


def test_import_path_b_prime_more_than_10000_rows(make_session, session_db, target_db, store, source_paths, target_cwd):
    """12 000 small live rows (≤ 5 MiB, > 10 000 rows) take B′ and verify; the plan holds no rows (memory)."""
    sid = large_segment(make_session, live=12_000, archived=0, size=16)
    _info, cap, _capsule, p, prepared = roundtrip(session_db, target_db, store, sid, source_paths, target_cwd)
    assert p.segments[0].path == "B'"
    assert not hasattr(p, "rows") and all(isinstance(s.rows, int) for s in p.segments)
    assert prepared.h_all == cap.h_all and prepared.h_rows == cap.h_rows
    assert len(target_db.get_messages(prepared.tip_id)) == 12_000


def test_import_mixed_paths_keep_lineage(make_session, session_db, target_db, store, source_paths, target_cwd):
    """Root over the limit (B′) and a small tip (A): the lineage stays one conversation and verifies."""
    root = large_segment(make_session, live=50, archived=10_100, size=16, cwd="/srv/src-work")
    tip = make_session.rotate(root, keep_last=2)
    make_session.turns(tip, 4)
    info, _cap, _capsule, p, prepared = roundtrip(session_db, target_db, store, tip, source_paths, target_cwd)
    assert [s.path for s in p.segments] == ["B'", "A"]
    assert prepared.import_path == "B'+A"
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [s.new_id for s in p.segments])


def test_import_hidden_until_unhide(make_session, session_db, target_db, store, source_paths, target_cwd):
    tip = rich_lineage(make_session)
    *_, prepared = roundtrip(session_db, target_db, store, tip, source_paths, target_cwd)
    listed = {r["id"] for r in target_db.list_sessions_rich(limit=50)}
    assert prepared.tip_id not in listed
    session_import.unhide(target_db, prepared)
    listed = {r["id"] for r in target_db.list_sessions_rich(limit=50)}
    assert prepared.tip_id in listed
    assert all(target_db.get_session(s)["hidden"] == 0 for s in (prepared.root_id, prepared.tip_id))
    session_import.hide(target_db, prepared)
    assert prepared.tip_id not in {r["id"] for r in target_db.list_sessions_rich(limit=50)}


def test_import_verify_failed_discards(make_session, session_db, target_db, store, source_paths, target_cwd):
    """★ A page manipulated in transit with consistent objects (new page object, capsule re-stored with the
    source's hashes, so every block, manifest and object id holds and only the session hashes can see it)
    ⇒ ``VerifyFailed`` and nothing of the import remains [I8].

    Mutant: skip the H_all/H_rows comparison after the import.
    """
    tip = rich_lineage(make_session)
    _info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    page = capsule["segments"][-1]["pages"][0]
    forged = store.forge_capsule(cap.capsule_id, page, lambda data: data.replace(b"user ", b"usr! ", 1))
    assert forged != cap.capsule_id
    p = plan(target_db, capsule, target_cwd)
    before = {r["id"] for r in target_db.list_sessions_rich(limit=100, include_hidden=True, include_children=True)}
    with pytest.raises(VerifyFailed):
        session_import.import_hidden(target_db, store, forged, p)
    after = {r["id"] for r in target_db.list_sessions_rich(limit=100, include_hidden=True, include_children=True)}
    assert after == before
    assert all(target_db.get_session(s.new_id) is None for s in p.segments)


def test_import_rollback_deletes_without_a_document_cache(
    make_session, session_db, target_db, store, source_paths, target_cwd, hermes_home, target_home
):
    """A failed import removes what it wrote even where the profile's document cache cannot be created (a regular
    file at ``cache/documents``, in the active profile and in the target's): the media root a delete of the
    storage interface names is determined without creating a folder, so the delete runs and no imported session stays
    hidden in the store [I3].

    Mutant: the media root taken from ``get_document_cache_dir()``, which creates the folder → the delete raises
    before it runs and the imported segments stay."""
    import shutil

    tip = rich_lineage(make_session)
    _info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    page = capsule["segments"][-1]["pages"][0]
    forged = store.forge_capsule(cap.capsule_id, page, lambda data: data.replace(b"user ", b"usr! ", 1))
    for home in (hermes_home, target_home):
        documents = home / "cache" / "documents"
        if documents.is_dir():
            shutil.rmtree(documents)
        documents.parent.mkdir(parents=True, exist_ok=True)
        documents.write_text("not a folder")
    p = plan(target_db, capsule, target_cwd)
    with pytest.raises(VerifyFailed):
        session_import.import_hidden(target_db, store, forged, p)
    assert all(target_db.get_session(s.new_id) is None for s in p.segments)
    listed = {r["id"] for r in target_db.list_sessions_rich(limit=100, include_hidden=True, include_children=True)}
    assert not listed & {s.new_id for s in p.segments}
    assert all((home / "cache" / "documents").is_file() for home in (hermes_home, target_home))


def test_import_refuses_object_readdressed_under_old_id(
    make_session, session_db, target_db, store, source_paths, target_cwd
):
    """A page whose manifest was swapped under its old object id is refused by the object reader before a block
    is read (``BlockCorrupt``, contract K-1: the id is checked against the manifest on every read path), and
    nothing of the import remains."""
    tip = rich_lineage(make_session)
    _info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    page = capsule["segments"][-1]["pages"][0]
    store.tamper(page, lambda data: data.replace(b"user ", b"usr! ", 1))
    p = plan(target_db, capsule, target_cwd)
    store.reads.clear()
    with pytest.raises(BlockCorrupt):
        session_import.import_hidden(target_db, store, cap.capsule_id, p)
    assert not set(store.reads) & set(store.objects[page].blocks)
    assert all(target_db.get_session(s.new_id) is None for s in p.segments)


def test_discard_removes_whole_lineage(make_session, session_db, target_db, store, source_paths, target_cwd):
    tip = rich_lineage(make_session)
    *_, p, prepared = roundtrip(session_db, target_db, store, tip, source_paths, target_cwd)
    session_import.discard(target_db, prepared)
    assert all(target_db.get_session(s.new_id) is None for s in p.segments)
    assert all(not target_db.get_messages(s.new_id, include_inactive=True) for s in p.segments)


def test_path_a_and_b_prime_agree(make_session, session_db, target_db, store, source_paths, target_cwd, tmp_path):
    """The same lineage through path A and, forced, through path B′ lands with equal rows on both targets — the
    two paths are interchangeable for every message column (B′ only loses the session counters, DESIGN §6.8)."""
    from dataclasses import replace

    from ctx_handover.tests.fakes.hoststore import open_session_db

    tip = rich_lineage(make_session)
    info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    plan_a = plan(target_db, capsule, target_cwd)
    prepared_a = session_import.import_hidden(target_db, store, cap.capsule_id, plan_a)
    (tmp_path / "other-target").mkdir()
    other = open_session_db(tmp_path / "other-target" / "state.db")
    try:
        plan_b = plan(other, capsule, target_cwd)
        plan_b = replace(plan_b, segments=tuple(replace(s, path="B'") for s in plan_b.segments))
        prepared_b = session_import.import_hidden(other, store, cap.capsule_id, plan_b)
        assert prepared_a.import_path == "A" and prepared_b.import_path == "B'"
        assert (prepared_a.h_all, prepared_a.h_rows, prepared_a.h_model) == (
            prepared_b.h_all,
            prepared_b.h_rows,
            prepared_b.h_model,
        )
        assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [s.new_id for s in plan_a.segments])
        assert_lineage_equal(session_db, other, list(info.lineage_ids), [s.new_id for s in plan_b.segments])
    finally:
        other.close()


@pytest.mark.parametrize("path", ["A", "B'"])
def test_import_takes_display_metadata_as_json_text(
    make_session, session_db, target_db, store, source_paths, target_cwd, path
):
    """A capsule row may carry ``display_metadata`` as JSON text instead of the object (``session_hash.JSON_FIELDS``:
    the core decodes it on some read paths and not on others, and the session hashes read both alike). Both import
    paths store the object, an archived row (``append_messages_batch`` on B′) and a live one (``archive_and_compact``)
    alike, as the old ``SessionDB`` writers decoded the text — on a storage-interface host too, whose repository
    takes only a mapping from ``NewMessage.from_live``.

    Mutant: ``hostcompat._new_messages`` passes the JSON text through → B′ raises ``ValueError`` and the import
    fails."""
    import json
    from dataclasses import replace

    tip = rich_lineage(make_session)
    make_session.append(
        tip,
        [
            make_session.message("user", "one more", display_metadata={"note": "kept", "n": [1, 2]}),
            make_session.message("assistant", "noted"),
        ],
    )
    info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    as_text = {"archived": 0, "live": 0}

    def to_text(data: bytes) -> bytes:
        rows = json.loads(data)
        for row in rows:
            if isinstance(row.get("display_metadata"), dict):
                row["display_metadata"] = json.dumps(row["display_metadata"])
                as_text["live" if row.get("active") else "archived"] += 1
        return json.dumps(rows).encode()

    forged = cap.capsule_id
    for segment in capsule["segments"]:
        for page in segment["pages"]:
            forged = store.forge_capsule(forged, page, to_text)
    assert as_text["archived"] >= 1 and as_text["live"] >= 1, as_text
    forged_doc = session_export.read_capsule(store, forged, max_json_bytes=64 * 1024**2)
    p = plan(target_db, forged_doc, target_cwd)
    p = replace(p, segments=tuple(replace(s, path=path) for s in p.segments))
    prepared = session_import.import_hidden(target_db, store, forged, p)
    assert prepared.import_path == ("A" if path == "A" else "B'")
    assert (prepared.h_all, prepared.h_rows, prepared.h_model) == (cap.h_all, cap.h_rows, cap.h_model)
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [s.new_id for s in p.segments])
    stored = [r.get("display_metadata") for s in p.segments for r in stored_rows(target_db, s.new_id)]
    assert {"note": "kept", "n": [1, 2]} in stored and {"k": 1} in stored


def test_unhide_requires_existing_cwd(make_session, session_db, target_db, store, source_paths, tmp_path):
    """The lineage only becomes visible when its recorded cwd exists on this host [LAB L3, R11]."""
    from ctx_handover.engine.errors import ImportFailed

    tip = rich_lineage(make_session)
    later = tmp_path / "materialised-at-commit"
    *_, prepared = roundtrip(session_db, target_db, store, tip, source_paths, later)
    assert target_db.get_session(prepared.tip_id)["cwd"] == str(later)
    with pytest.raises(ImportFailed):
        session_import.unhide(target_db, prepared)
    assert target_db.get_session(prepared.tip_id)["hidden"] == 1
    later.mkdir()
    session_import.unhide(target_db, prepared)
    assert target_db.get_session(prepared.tip_id)["hidden"] == 0
