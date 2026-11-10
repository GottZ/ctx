"""Source side: describe, capsule export, revision bracket, delta detection, receiver caps (DESIGN §6.1–§6.3)."""

from __future__ import annotations

import json

import pytest

from ctx_handover.engine import session_export
from ctx_handover.engine.errors import NotFound, PayloadTooLarge, StaleSession
from ctx_handover.engine.jsonutil import canonical_bytes, doc_id, sha256_hex
from ctx_handover.engine.model import ObjectManifest, PrepareOutcome, SessionRef
from ctx_handover.engine.session_hash import put_json
from ctx_handover.tests.unit.B.conftest import export, rich_lineage
from ctx_handover.tests.unit.B.support import MemoryBlockStore

MiB = 1024**2


def test_describe_resolves_tip_lineage_and_row_cwd(make_session, session_db):
    tip = rich_lineage(make_session, cwd="/srv/row-cwd")
    root = session_db.get_compression_lineage(tip)[0]
    info = session_export.describe(session_db, SessionRef(id=root), calling_session=None)
    assert info.tip_id == tip and info.root_id == root
    assert info.lineage_ids == tuple(session_db.get_compression_lineage(tip))
    assert info.cwd == "/srv/row-cwd" and info.git_branch == "main"
    assert info.provider == "src-provider" and info.model == "src-model"
    assert info.revision == session_db.conversation_revision(tip, include_ancestors=True)
    assert info.active_messages == len(session_db.get_messages(tip))
    by_call = session_export.describe(session_db, SessionRef(), calling_session=tip)
    assert by_call.tip_id == tip
    with pytest.raises(NotFound):
        session_export.describe(session_db, SessionRef(id="20250101_000000_000000"), calling_session=None)


def test_describe_latest_picks_newest_session_with_messages(make_session, session_db, clock):
    b = make_session
    older = b.create()
    b.turns(older, 1)
    clock.advance(60)
    newer = b.create()
    b.turns(newer, 1)
    clock.advance(60)
    b.create()  # empty: never "latest"
    info = session_export.describe(session_db, SessionRef(latest=True), calling_session=None)
    assert info.tip_id == newer


def _capsule(store, cap):
    return session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * MiB)


def test_export_includes_archived_rows(make_session, session_db, store, source_paths):
    tip = rich_lineage(make_session)
    info, cap = export(session_db, store, tip, source_paths)
    capsule = _capsule(store, cap)
    for seg_id, seg in zip(info.lineage_ids, capsule["segments"], strict=True):
        counts = make_session.counts(seg_id)
        assert (seg["rows"], seg["active"], seg["archived"], seg["rewind"]) == (
            counts.rows,
            counts.active,
            counts.archived,
            counts.rewind,
        )
    rows = [r for _, r in session_export.iter_capsule_rows(store, cap.capsule_id)]
    assert any(not r["active"] and r["compacted"] for r in rows)
    assert len(rows) == cap.rows == sum(make_session.counts(s).rows for s in info.lineage_ids)
    assert "system_prompt" not in capsule["segments"][0]["row_object"]
    assert capsule["session"]["lineage_ids"] == list(info.lineage_ids)


def test_export_pages_bounded(make_session, session_db, store, source_paths):
    b = make_session
    sid = b.create()
    b.turns(sid, 20)
    b.append(sid, [b.message("user", "x" * 9000)])
    b.turns(sid, 2)
    _info, cap = export(session_db, store, sid, source_paths, page_rows=7, page_bytes=4000)
    capsule = _capsule(store, cap)
    pages = capsule["segments"][0]["pages"]
    assert len(pages) > 6
    for page_id in pages:
        page = json.loads(b"".join(store.blocks[x] for x in store.objects[page_id].blocks))
        size = len(canonical_bytes(page))
        assert len(page) <= 7
        assert size <= 4000 or len(page) == 1


def test_export_keyset_paging(make_session, session_db, store, source_paths):
    b = make_session
    sid = b.create()
    b.turns(sid, 30)
    paged = list(session_export.iter_segment_rows(session_db, sid, batch=4))
    assert [r["id"] for r in paged] == [r["id"] for r in session_db.get_messages(sid, include_inactive=True)]
    _info, cap = export(session_db, store, sid, source_paths, page_rows=5)
    ids = [r["id"] for _, r in session_export.iter_capsule_rows(store, cap.capsule_id)]
    assert ids == sorted(ids) and len(ids) == 60


def test_export_side_state_goal_heartbeat_loop(make_session, session_db, store, source_paths):
    b = make_session
    sid = b.create()
    b.turns(sid, 1)
    goal = {"goal": "ship it", "status": "active", "max_turns": 50, "waiting_on_pid": 4242}
    heartbeat = {"prompt": "ping", "interval_seconds": 60, "status": "active"}
    loop = {"prompt": "tick", "status": "active", "route": {"platform": "telegram", "chat_id": "1"}}
    for key, value in (("goal", goal), ("heartbeat", heartbeat), ("loop", loop)):
        session_db.set_meta(f"{key}:{sid}", json.dumps(value))
    _info, cap = export(session_db, store, sid, source_paths)
    assert _capsule(store, cap)["state_meta"] == {"goal": goal, "heartbeat": heartbeat, "loop": loop}


class _AppendDuringExport(MemoryBlockStore):
    """A store that lets the user type a message (``append_message``, the TUI submit path) after the first page."""

    def __init__(self, builder, sid):
        super().__init__()
        self.builder, self.sid, self.done = builder, sid, False

    def put_manifests(self, manifests):
        if not self.done:
            self.done = True
            self.builder.user(self.sid, "typed in Desktop during the handover")
        return super().put_manifests(manifests)


def test_export_revision_bracket(make_session, session_db, source_paths):
    """★ A message appended during the export makes the capsule stale ⇒ ``StaleSession`` [SK H2, R3].

    Mutant: no bracket (the ``rev_after != rev_before`` check removed).
    """
    b = make_session
    sid = b.create()
    b.turns(sid, 10)
    store = _AppendDuringExport(b, sid)
    with pytest.raises(StaleSession):
        export(session_db, store, sid, source_paths, page_rows=4)
    assert store.done


def test_check_revision_append_only_vs_other(make_session, session_db, store, source_paths):
    """★ Append on the tip ⇒ ``delta`` with exactly the new rows; compaction, rewind ⇒ ``stale_session``.

    Mutant: every revision change is reported as ``delta`` (shape check in ``_appended_rows`` removed).
    """
    b = make_session
    tip = rich_lineage(b)
    info, cap = export(session_db, store, tip, source_paths)
    assert session_export.check_revision(session_db, info, cap) == (PrepareOutcome.OK, [])
    new_id = b.user(tip, "late message")
    outcome, rows = session_export.check_revision(session_db, info, cap)
    assert outcome == PrepareOutcome.DELTA and [r["id"] for r in rows] == [new_id]
    b.rewind(tip, new_id)  # taking back only the late row restores the exported live set exactly
    assert session_export.check_revision(session_db, info, cap) == (PrepareOutcome.OK, [])
    exported_user = [r["id"] for r in session_db.get_messages(tip) if r["role"] == "user"][-1]
    b.rewind(tip, exported_user)
    assert session_export.check_revision(session_db, info, cap)[0] == PrepareOutcome.STALE_SESSION
    info2, cap2 = export(session_db, store, tip, source_paths)
    b.compact_in_place(tip, keep_last=2)
    assert session_export.check_revision(session_db, info2, cap2)[0] == PrepareOutcome.STALE_SESSION
    info3, cap3 = export(session_db, store, tip, source_paths)
    child = b.rotate(tip, keep_last=2)
    assert child != tip
    assert session_export.check_revision(session_db, info3, cap3)[0] == PrepareOutcome.STALE_SESSION


def test_export_delta_carries_rows_and_new_hashes(make_session, session_db, store, source_paths):
    b = make_session
    tip = rich_lineage(b)
    info, cap = export(session_db, store, tip, source_paths)
    b.user(tip, "late 1")
    b.assistant(tip, "late answer")
    _outcome, rows = session_export.check_revision(session_db, info, cap)
    delta = session_export.export_delta(session_db, info, cap, rows, store=store)
    assert delta.rows == 2 and delta.tip_id == tip
    _info_now, cap_now = export(session_db, MemoryBlockStore(), tip, source_paths)
    assert (delta.h_all, delta.h_rows, delta.h_model) == (cap_now.h_all, cap_now.h_rows, cap_now.h_model)
    b.user(tip, "even later")
    with pytest.raises(StaleSession):
        session_export.export_delta(session_db, info, cap, rows, store=store)


# ---------------------------------------------------------------------------------------------------------
# Receiver caps (SEC 2.5)
# ---------------------------------------------------------------------------------------------------------


def _fake_page(store, size):
    """Register a page manifest that claims ``size`` bytes but has no blocks (reading it would fail loudly)."""
    sha = sha256_hex(str(size).encode())
    doc = {"kind": "ctxh.object", "v": 1, "sha256": sha, "size": size, "block_size": 524288, "blocks": [], "media": "x"}
    oid = doc_id(doc)
    store.put_manifests([ObjectManifest(sha, size, 524288, (), "application/json", oid)])
    return oid


def _capsule_doc(segments):
    return {
        "kind": "ctxh.capsule",
        "v": 1,
        "session": {"tip_id": "t"},
        "hashes": {"h_all": "a", "h_rows": "b", "h_model": "c"},
        "segments": segments,
    }


@pytest.mark.parametrize("case", ["segments", "pages", "page_object", "session_bytes", "capsule_object"])
def test_capsule_count_caps_before_parse(case):
    """★ Caps hold before any page is fetched or parsed: > 500 segments, > 10 000 pages, a page object > 64 MiB,
    pages together > ``max_session_bytes``, a capsule object > ``max_json_bytes`` [SEC 2.5].

    Mutant: ``read_capsule_doc`` without the segment/page/size checks.
    """
    store = MemoryBlockStore()
    if case == "segments":
        doc = _capsule_doc([{"row_object": {"id": str(i)}, "pages": []} for i in range(501)])
    elif case == "pages":
        doc = _capsule_doc([{"row_object": {"id": "s"}, "pages": [f"{i:064x}" for i in range(10_001)]}])
    elif case == "page_object":
        doc = _capsule_doc([{"row_object": {"id": "s"}, "pages": [_fake_page(store, 64 * MiB + 1)]}])
    elif case == "session_bytes":
        pages = [_fake_page(store, 40 * MiB + i) for i in range(3)]
        doc = _capsule_doc([{"row_object": {"id": "s"}, "pages": pages}])
    else:
        doc = _capsule_doc([{"row_object": {"id": "s", "pad": "x" * 70_000}, "pages": []}])
    capsule_id = put_json(store, doc).object_id
    store.reads.clear()
    limit = 64 * 1024 if case == "capsule_object" else 64 * MiB
    with pytest.raises(PayloadTooLarge):
        session_export.read_capsule(store, capsule_id, max_json_bytes=limit, max_session_bytes=100 * MiB)
    page_reads = [b for b in store.reads if b not in store.objects[capsule_id].blocks]
    assert page_reads == []
    if case == "capsule_object":
        assert store.reads == []
