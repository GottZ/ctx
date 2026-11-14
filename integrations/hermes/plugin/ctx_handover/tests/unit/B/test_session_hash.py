"""Verification hashes H_all / H_rows / H_model (DESIGN §6.9; TESTPLAN §2.3)."""

from __future__ import annotations

import json

import pytest

from ctx_handover.engine import session_hash as sh


def _segments(db, lineage_ids):
    return [db.get_messages(sid, include_inactive=True) for sid in lineage_ids]


def _lineage_with_archived_root(b):
    root = b.create(cwd="/srv/w")
    b.turns(root, 4, with_tools=True)
    b.compact_in_place(root, keep_last=2)
    b.turns(root, 2)
    tip = b.rotate(root, keep_last=2)
    b.turns(tip, 3)
    return root, tip


def test_h_all_covers_all_segments_and_archived_rows(make_session, session_db):
    """★ Dropping one archived row of the ROOT segment changes H_all [SK M7].

    Mutant: H_all over the tip only (``h_all`` hashes ``list(segments_rows)[-1:]``).
    """
    root, tip = _lineage_with_archived_root(make_session)
    segs = _segments(session_db, [root, tip])
    archived = [i for i, r in enumerate(segs[0]) if not r["active"] and r["compacted"]]
    assert archived, "fixture must hold archived rows in the root"
    base = sh.h_all(segs)
    dropped = [[r for i, r in enumerate(segs[0]) if i != archived[0]], segs[1]]
    assert sh.h_all(dropped) != base
    flipped = [[dict(r, compacted=0) if i == archived[-1] else r for i, r in enumerate(segs[0])], segs[1]]
    assert sh.h_all(flipped) != base


def test_h_all_ignores_rewind_rows(make_session, session_db):
    b = make_session
    sid = b.create()
    b.turns(sid, 2)
    first = b.user(sid, "taken back")
    b.assistant(sid, "gone")
    b.rewind(sid, first)
    b.turns(sid, 1)
    rows = session_db.get_messages(sid, include_inactive=True)
    rewind = [r for r in rows if sh.is_rewind_row(r)]
    assert len(rewind) == 2
    assert sh.h_all([rows]) == sh.h_all([[r for r in rows if not sh.is_rewind_row(r)]])


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("content", "changed"),
        ("tool_calls", [{"id": "x", "type": "function", "function": {"name": "f", "arguments": "{}"}}]),
        ("api_content", "sidecar changed"),
        ("reasoning", "other thought"),
        ("reasoning_details", [{"type": "reasoning.encrypted", "data": "zz"}]),
        ("codex_reasoning_items", [{"type": "reasoning", "id": "rs_x"}]),
        ("_compressed_summary", True),
        ("display_kind", "note"),
    ],
)
def test_h_rows_detects_any_field_change(make_session, session_db, field, value):
    """★ Every model field of a live tip row reaches H_rows (DESIGN §6.9).

    Mutant: ``ROWS_FIELDS`` without ``api_content`` and ``display_kind``.
    """
    b = make_session
    sid = b.create()
    b.turns(sid, 3, with_tools=True)
    rows = session_db.get_messages(sid, include_inactive=True)
    base = sh.h_rows(rows)
    victim = next(i for i, r in enumerate(rows) if r["role"] == "assistant")
    changed = [dict(r, **{field: value}) if i == victim else r for i, r in enumerate(rows)]
    assert sh.h_rows(changed) != base


def test_h_rows_decodes_json_content_prefix():
    parts = [{"type": "text", "text": "a"}, {"type": "image_url", "image_url": {"url": "data:x"}}]
    decoded = {"role": "user", "content": parts, "active": 1}
    raw = {"role": "user", "content": sh.CONTENT_JSON_PREFIX + json.dumps(parts), "active": 1}
    assert sh.h_rows([decoded]) == sh.h_rows([raw])
    assert sh.h_all([[decoded]]) == sh.h_all([[raw]])


def test_h_rows_json_text_and_parsed_fields_compare_equal():
    calls = [{"id": "c1", "type": "function", "function": {"name": "f", "arguments": "{}"}}]
    a = {"role": "assistant", "content": "", "tool_calls": calls, "_compressed_summary": True, "active": 1}
    b = {"role": "assistant", "content": "", "tool_calls": json.dumps(calls), "_compressed_summary": 1, "active": True}
    assert sh.h_rows([a]) == sh.h_rows([b])


def test_h_model_detects_display_kind_tamper(make_session, session_db):
    """A field the v1 list missed (``display_kind``) changes H_model [SK §4.2]."""
    b = make_session
    sid = b.create()
    b.turns(sid, 2)
    conversation = session_db.get_resume_conversations(sid)[0]
    base = sh.h_model(conversation)
    tampered = [dict(m, display_kind="injected") if i == 1 else m for i, m in enumerate(conversation)]
    assert sh.h_model(tampered) != base


def test_hash_formula_matches_design():
    """``H = sha256(algo + "\\n" + count + "\\n" + Σ (sha256(row_json) + "\\n"))`` (DESIGN §6.9)."""
    import hashlib

    row = {"role": "user", "content": "x", "active": 1}
    digest = hashlib.sha256(
        json.dumps({"content": "x", "role": "user"}, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    expected = hashlib.sha256(f"ctxh-rows-v1\n1\n{digest}\n".encode()).hexdigest()
    assert sh.h_rows([row]) == expected


def test_tool_call_uid_is_derived_not_hashed():
    """A NULL result uid filled by a batch insert compares equal; every other field still counts."""
    a = {"role": "tool", "content": "r", "tool_call_id": "c1", "tool_call_uid": None, "active": 1}
    b = dict(a, tool_call_uid="u1")
    assert sh.h_all([[a]]) == sh.h_all([[b]])
    assert sh.h_all([[a]]) != sh.h_all([[dict(a, tool_call_id="c2")]])
