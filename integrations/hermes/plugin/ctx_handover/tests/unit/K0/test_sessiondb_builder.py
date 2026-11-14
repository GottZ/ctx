"""K0: ``SessionBuilder`` produces state the Hermes core itself observes (TESTPLAN §1 fixtures, §2.3 shapes).

Assertions read back through public ``SessionDB`` reads or a read-only sqlite connection — never through
the builder's own bookkeeping — so a builder that only *believes* it wrote a lineage fails here.
"""

from __future__ import annotations

import ast
import json
import re
import sqlite3
import time
from itertools import pairwise
from pathlib import Path

import pytest

from ctx_handover.tests.fakes import sessiondb as sessiondb_module
from ctx_handover.tests.fakes.sessiondb import BRANCH_COPY_KEYS, SessionBuilder, large_segment

ID_RE = re.compile(r"^\d{8}_\d{6}_[0-9a-f]{6}$")
SUMMARY_HEAD = "[CONTEXT COMPACTION"


def rows_of(db, sid):
    return db.get_messages(sid, include_inactive=True)


def flags_of(db, sid):
    return {m["id"]: (m["active"], m["compacted"]) for m in rows_of(db, sid)}


def ro_connect(hermes_home: Path) -> sqlite3.Connection:
    return sqlite3.connect(f"file:{hermes_home / 'state.db'}?mode=ro", uri=True)


def contents(history):
    return [m.get("content") for m in history]


def lock_is_free(db, sid) -> bool:
    probe = "pid=1:tid=1:agent=k0-probe:nonce=00000000"
    acquired = db.try_acquire_compression_lock(sid, probe)
    if acquired:
        db.release_compression_lock(sid, probe)
    return acquired


# ── sessions and messages ─────────────────────────────────────────────────────────────────────────


def test_create_writes_the_row_through_the_core(make_session, session_db, clock):
    sid = make_session.create(
        title="Work",
        model="m1",
        provider="openrouter",
        base_url="https://peer.example/v1",
        api_mode="chat_completions",
        cwd="/srv/work",
        git_repo_root="/srv/work",
        git_branch="main",
        system_prompt="SYS",
    )
    assert ID_RE.match(sid)
    assert sid.startswith(clock.datetime().strftime("%Y%m%d_%H%M%S"))
    row = session_db.get_session(sid)
    assert (row["source"], row["model"], row["title"], row["system_prompt"]) == ("cli", "m1", "Work", "SYS")
    assert (row["cwd"], row["git_repo_root"], row["git_branch"]) == ("/srv/work", "/srv/work", "main")
    route = json.loads(row["model_config"])["gateway_runtime"]
    assert route == {"provider": "openrouter", "base_url": "https://peer.example/v1", "api_mode": "chat_completions"}


def test_create_title_collision_is_the_cores_error(make_session):
    make_session.create(title="Same")
    with pytest.raises(ValueError, match="already in use"):
        make_session.create(title="Same")


def test_single_messages_land_as_the_model_history(make_session, session_db):
    sid = make_session.create()
    call = SessionBuilder.tool_call("read_file", {"path": "/srv/a.txt"}, call_id="call_1")
    ids = [
        make_session.user(sid, "hello"),
        make_session.assistant(sid, "", tool_calls=[call], reasoning="need the file"),
        make_session.tool(sid, "call_1", '{"content": "A"}', name="read_file"),
        make_session.assistant(sid, "done"),
    ]
    assert ids == [m["id"] for m in session_db.get_messages(sid)]
    model, _display = session_db.get_resume_conversations(sid)
    assert [(m["role"], m.get("tool_call_id")) for m in model] == [
        ("user", None),
        ("assistant", None),
        ("tool", "call_1"),
        ("assistant", None),
    ]
    assert contents(model)[0::2] == ["hello", '{"content": "A"}']
    assert model[1]["tool_calls"][0]["function"]["name"] == "read_file"
    assert model[1]["reasoning"] == "need the file"
    assert model[3]["content"] == "done"


def test_timestamps_follow_the_clock_without_moving_it(make_session, session_db, clock):
    start = clock.now()
    sid = make_session.create()
    make_session.turns(sid, 3)
    stamps = [m["timestamp"] for m in session_db.get_messages(sid)]
    assert stamps[0] >= start
    assert all(b > a for a, b in pairwise(stamps))
    assert clock.now() == start
    clock.advance(3600)
    later = make_session.user(sid, "an hour later")
    assert session_db.get_messages(sid)[-1]["id"] == later
    assert session_db.get_messages(sid)[-1]["timestamp"] >= start + 3600


def test_turns_batch_builds_paired_tool_rounds(make_session, session_db):
    sid = make_session.create()
    ids = make_session.turns(sid, 3, size=100, with_tools=True)
    stored = session_db.get_messages(sid)
    assert ids == [m["id"] for m in stored]
    assert [m["role"] for m in stored] == ["user", "assistant", "tool", "assistant"] * 3
    for call_row, result_row in zip(stored[1::4], stored[2::4], strict=True):
        assert call_row["tool_calls"][0]["id"] == result_row["tool_call_id"]
    assert all(len(m["content"]) == 100 for m in stored if m["role"] == "user")
    more = make_session.turns(sid, 2)
    assert len(more) == 4
    assert session_db.get_session(sid)["message_count"] == 16
    model, _ = session_db.get_resume_conversations(sid)
    assert [m.get("tool_call_id") for m in model] == [m.get("tool_call_id") for m in session_db.get_messages(sid)]


# ── rotation ──────────────────────────────────────────────────────────────────────────────────────


def test_rotate_yields_two_segments_with_the_right_parent(make_session, session_db):
    root = make_session.create(title="Work")
    make_session.turns(root, 3)
    root_live = session_db.get_messages(root)
    tip = make_session.rotate(root, keep_last=2)
    assert session_db.get_compression_lineage(tip) == [root, tip]
    assert session_db.get_session(tip)["parent_session_id"] == root
    assert session_db.get_session(root)["end_reason"] == "compression"
    assert session_db.resolve_resume_session_id(root) == tip
    assert (session_db.get_session_title(root), session_db.get_session_title(tip)) == (None, "Work")
    assert session_db.export_session_lineage(tip, include_inactive=True)["lineage_session_ids"] == [root, tip]
    lineage = make_session.lineage(tip)
    assert (lineage.root_id, lineage.tip_id, lineage.segment_ids) == (root, tip, (root, tip))
    assert make_session.lineage(root) == lineage
    assert lineage.revision == session_db.conversation_revision(tip, include_ancestors=True)
    model, _ = session_db.get_resume_conversations(tip)
    assert model[0]["content"].startswith(SUMMARY_HEAD)
    assert contents(model[1:]) == contents(root_live[-2:])
    tip_rows = session_db.get_messages(tip)
    assert [m["message_uid"] for m in tip_rows[1:]] == [m["message_uid"] for m in root_live[-2:]]
    assert lock_is_free(session_db, root)


def test_rotate_twice_chains_three_segments(make_session, session_db):
    root = make_session.create(title="Chain")
    make_session.turns(root, 2)
    mid = make_session.rotate(root)
    make_session.turns(mid, 2)
    tip = make_session.rotate(mid)
    make_session.turns(tip, 1)
    assert session_db.get_compression_lineage(tip) == [root, mid, tip]
    assert make_session.lineage(mid).segment_ids == (root, mid, tip)
    assert session_db.get_session_title(tip) == "Chain"


def test_rotate_clones_the_concurrent_tail_into_the_child(make_session, session_db):
    root = make_session.create()
    make_session.turns(root, 2)
    tip = make_session.rotate(root, keep_last=0, concurrent=1)
    parent_last = session_db.get_messages(root)[-1]
    child_last = session_db.get_messages(tip)[-1]
    assert parent_last["content"].startswith("concurrent")
    assert (parent_last["active"], child_last["active"]) == (1, 1)
    assert child_last["message_uid"] == parent_last["message_uid"]
    assert child_last["content"] == parent_last["content"]
    assert contents(session_db.get_resume_conversations(tip)[0])[-1] == parent_last["content"]


def test_rotate_refuses_under_a_foreign_compression_lock(make_session, session_db):
    root = make_session.create()
    make_session.turns(root, 1)
    assert session_db.try_acquire_compression_lock(root, "pid=1:tid=1:agent=other:nonce=11111111")
    with pytest.raises(RuntimeError, match="compression lock"):
        make_session.rotate(root)
    assert session_db.get_compression_lineage(root) == [root]
    assert session_db.get_session(root)["end_reason"] is None


# ── in-place compaction ───────────────────────────────────────────────────────────────────────────


def test_compact_in_place_archives_rows_and_export_carries_them(make_session, session_db, hermes_home):
    sid = make_session.create()
    held = make_session.turns(sid, 4)
    result = make_session.compact_in_place(sid, keep_last=2)
    assert result["archived_ids"] == held[:6]
    assert result["rewound_ids"] == held[6:]
    assert result["covered_ids"] == held
    assert make_session.counts(sid) == sessiondb_module.RowCounts(rows=11, active=3, archived=6, rewind=2)
    with ro_connect(hermes_home) as conn:
        stored = [
            r[0]
            for r in conn.execute(
                "SELECT id FROM messages WHERE session_id = ? AND active = 0 AND compacted = 1 ORDER BY id", (sid,)
            )
        ]
    assert stored == held[:6]
    exported = session_db.export_session_lineage(sid, include_inactive=True)["segments"][-1]["messages"]
    assert {m["id"]: (m["active"], m["compacted"]) for m in exported} == flags_of(session_db, sid)
    plain = session_db.export_session_lineage(sid)["segments"][-1]["messages"]
    assert not set(held) & {m["id"] for m in plain}
    model, display = session_db.get_resume_conversations(sid)
    assert model[0]["content"].startswith(SUMMARY_HEAD)
    originals = session_db.get_messages(sid, include_inactive=True)[:8]
    assert contents(model[1:]) == contents(originals[6:])
    assert [c for c in contents(display) if not c.startswith(SUMMARY_HEAD)] == contents(originals)
    assert lock_is_free(session_db, sid)


@pytest.mark.parametrize("coverage", [True, False], ids=["covered_ids", "watermark_only"])
def test_compact_in_place_concurrent_rows_become_rewind_rows_and_clones(make_session, session_db, coverage):
    sid = make_session.create()
    held = make_session.turns(sid, 3)
    result = make_session.compact_in_place(sid, keep_last=2, concurrent=1, coverage=coverage)
    assert result["covered_ids"] == (held if coverage else None)
    assert result["rewound_ids"] == [*held[4:], *result["concurrent_ids"]]
    rows = rows_of(session_db, sid)
    rewind = [m for m in rows if (m["active"], m["compacted"]) == (0, 0)]
    live = [m for m in rows if m["active"] == 1]
    assert len(rewind) == 3
    for original in rewind:
        twins = [m for m in live if m["message_uid"] == original["message_uid"]]
        assert len(twins) == 1
        assert twins[0]["content"] == original["content"]
    model, _ = session_db.get_resume_conversations(sid)
    assert model[-1]["content"].startswith("concurrent")


def test_rewind_makes_inactive_uncompacted_rows(make_session, session_db):
    sid = make_session.create()
    ids = make_session.turns(sid, 3)
    before = session_db.conversation_revision(sid)
    result = make_session.rewind(sid, ids[2])
    assert result["rewound_count"] == 4
    flags = flags_of(session_db, sid)
    assert [flags[i] for i in ids] == [(1, 0)] * 2 + [(0, 0)] * 4
    model, display = session_db.get_resume_conversations(sid)
    first_two = contents(session_db.get_messages(sid))
    assert contents(model) == first_two
    assert contents(display) == first_two
    assert session_db.conversation_revision(sid) != before


def test_one_segment_can_hold_every_row_kind(make_session, session_db):
    sid = make_session.create()
    make_session.turns(sid, 3)
    make_session.compact_in_place(sid, keep_last=2)
    late = make_session.turns(sid, 2)
    make_session.rewind(sid, late[2])
    counts = make_session.counts(sid)
    assert (counts.archived, counts.rewind, counts.active) == (4, 4, 5)
    exported = session_db.export_session_lineage(sid, include_inactive=True)["messages"]
    assert {(m["active"], m["compacted"]) for m in exported} == {(1, 0), (0, 1), (0, 0)}


# ── branch copies ─────────────────────────────────────────────────────────────────────────────────


def test_clone_copies_rows_with_the_same_message_uids(make_session, session_db):
    root = make_session.create(title="T")
    make_session.turns(root, 2, with_tools=True)
    copy = make_session.clone(root)
    src, dst = session_db.get_messages(root), session_db.get_messages(copy)
    assert [m["message_uid"] for m in dst] == [m["message_uid"] for m in src]
    assert [(m["role"], m["content"], m["tool_call_id"]) for m in dst] == [
        (m["role"], m["content"], m["tool_call_id"]) for m in src
    ]
    assert [m["tool_call_uids"] for m in dst] == [m["tool_call_uids"] for m in src]
    row = session_db.get_session(copy)
    assert row["parent_session_id"] == root
    assert json.loads(row["model_config"])["_branched_from"] == root
    assert row["title"] == "T #2"
    assert session_db.get_session(root)["end_reason"] == "branched"
    assert session_db.get_compression_lineage(copy) == [copy]
    assert make_session.lineage(copy).segment_ids == (copy,)


def test_clone_of_a_compacted_tip_keeps_the_model_history(make_session, session_db):
    root = make_session.create()
    make_session.turns(root, 3)
    tip = make_session.rotate(root)
    make_session.turns(tip, 1)
    make_session.compact_in_place(tip, keep_last=2)
    copy = make_session.clone(tip, end_parent=False)
    assert session_db.get_session(tip)["end_reason"] is None
    src_model = session_db.get_resume_conversations(tip)[0]
    dst_model = session_db.get_resume_conversations(copy)[0]
    assert [(m["role"], m["content"]) for m in dst_model] == [(m["role"], m["content"]) for m in src_model]
    assert [m["message_uid"] for m in dst_model] == [m["message_uid"] for m in src_model]


# ── model history over a mixed lineage ────────────────────────────────────────────────────────────


def test_resume_conversations_return_the_expected_model_history(make_session, session_db):
    root = make_session.create()
    for i in range(3):
        make_session.user(root, f"q{i}")
        make_session.assistant(root, f"a{i}")
    tip = make_session.rotate(root, keep_last=2, summary="first summary")
    make_session.user(tip, "q3")
    make_session.assistant(tip, "a3")
    make_session.compact_in_place(tip, keep_last=2, summary="second summary")
    make_session.user(tip, "q4")
    make_session.assistant(tip, "a4")
    model, display = session_db.get_resume_conversations(tip)
    assert [m["role"] for m in model] == ["user", "user", "assistant", "user", "assistant"]
    assert model[0]["content"].startswith(SUMMARY_HEAD)
    assert model[0]["content"].endswith("second summary")
    assert contents(model[1:]) == ["q3", "a3", "q4", "a4"]
    visible = [c for c in contents(display) if not c.startswith(SUMMARY_HEAD)]
    assert visible == ["q0", "a0", "q1", "a1", "q2", "a2", "q3", "a3", "q4", "a4"]


# ── large segments ────────────────────────────────────────────────────────────────────────────────


def test_large_segment_15613_rows_5205_live(make_session, session_db, hermes_home, record_property):
    started = time.perf_counter()
    sid = large_segment(make_session, live=5205, archived=10408)
    seconds = time.perf_counter() - started
    record_property("large_segment_seconds", round(seconds, 2))
    print(f"large_segment(live=5205, archived=10408): {seconds:.2f} s")
    with ro_connect(hermes_home) as conn:
        total, active, compacted, rewind = conn.execute(
            "SELECT COUNT(*), SUM(active), SUM(compacted), SUM(active = 0 AND compacted = 0) "
            "FROM messages WHERE session_id = ?",
            (sid,),
        ).fetchone()
        last_archived, first_live = conn.execute(
            "SELECT (SELECT MAX(id) FROM messages WHERE session_id = ?1 AND compacted = 1),"
            " (SELECT MIN(id) FROM messages WHERE session_id = ?1 AND active = 1)",
            (sid,),
        ).fetchone()
    assert (total, active, compacted, rewind) == (15613, 5205, 10408, 0)
    assert last_archived < first_live
    segment = session_db.export_session_lineage(sid, include_inactive=True)["segments"][-1]
    assert len(segment["messages"]) == 15613
    assert sum(1 for m in segment["messages"] if m["active"]) == 5205
    model, _ = session_db.get_resume_conversations(sid)
    assert len(model) == 5205
    assert model[0]["content"].startswith(SUMMARY_HEAD)


def test_large_segment_generations_archive_their_summaries(make_session, session_db):
    sid = large_segment(make_session, live=10, archived=50, generations=3)
    assert make_session.counts(sid) == sessiondb_module.RowCounts(rows=60, active=10, archived=50, rewind=0)
    rows = rows_of(session_db, sid)
    summaries = [(m["active"], m["compacted"]) for m in rows if m["content"].startswith(SUMMARY_HEAD)]
    assert summaries == [(0, 1), (0, 1), (1, 0)]


def test_large_segment_12000_small_rows_without_archive(make_session, session_db):
    sid = large_segment(make_session, live=12000, archived=0, size=16)
    assert make_session.counts(sid) == sessiondb_module.RowCounts(rows=12000, active=12000, archived=0, rewind=0)
    assert not any(m["content"].startswith(SUMMARY_HEAD) for m in session_db.get_messages(sid, limit=5))


def test_large_segment_rejects_shapes_it_cannot_build(make_session):
    with pytest.raises(ValueError):
        large_segment(make_session, live=0, archived=10)
    with pytest.raises(ValueError, match="too small"):
        large_segment(make_session, live=1, archived=2, generations=3)


# ── guards on the builder itself ──────────────────────────────────────────────────────────────────


def test_builder_writes_only_through_public_sessiondb_methods():
    source = Path(sessiondb_module.__file__).read_text(encoding="utf-8")
    tree = ast.parse(source)
    imported = {
        alias.name.split(".")[0] for node in ast.walk(tree) if isinstance(node, ast.Import) for alias in node.names
    }
    imported |= {
        node.module.split(".")[0] for node in ast.walk(tree) if isinstance(node, ast.ImportFrom) and node.module
    }
    assert "sqlite3" not in imported
    private_from_upstream = [
        alias.name
        for node in ast.walk(tree)
        if isinstance(node, ast.ImportFrom) and node.module and not node.module.startswith("ctx_handover")
        for alias in node.names
        if alias.name.startswith("_")
    ]
    assert private_from_upstream == []
    private_db_calls = [
        node.attr
        for node in ast.walk(tree)
        if isinstance(node, ast.Attribute)
        and node.attr.startswith("_")
        and (
            (isinstance(node.value, ast.Name) and node.value.id == "db")
            or (isinstance(node.value, ast.Attribute) and node.value.attr == "db")
        )
    ]
    assert private_db_calls == []
    sql = re.compile(r"\b(INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM|SELECT\s+.+\s+FROM)\b", re.IGNORECASE)
    strings = [n.value for n in ast.walk(tree) if isinstance(n, ast.Constant) and isinstance(n.value, str)]
    assert [s for s in strings if sql.search(s)] == []


def test_branch_copy_keys_match_upstream():
    from hermes_cli.cli_commands_mixin import _BRANCH_COPY_KEYS

    assert tuple(_BRANCH_COPY_KEYS) == BRANCH_COPY_KEYS
