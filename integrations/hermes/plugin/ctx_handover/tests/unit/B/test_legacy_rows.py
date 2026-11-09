"""Rows written before the core stamped identities: no ``message_uid``, no ``tool_call_uids`` (DESIGN §6.5, §6.9).

How such rows exist: the store mints ``message_uid`` for rows of older builds lazily, one bounded slice per open
(``hermes_state_schema.py:1127-1156``: "Until then a legacy row simply has no uid"), and nothing ever backfills
``tool_call_uids`` of an assistant row an older build wrote (``agent/message_metadata.py:341-348``: "a call
without a uid (a legacy row)"). Every write path of the current core stamps both (``hermes_state_messages.py:
417-418, 674-677``; insert trigger ``hermes_state_common.py:823-831``), so no public method of this version can
produce such a row. The fixture therefore takes the upstream suite's own recipe for "rows written by a build that
predates the column" (``tests/hermes_state/test_message_uid.py:103-107, 503``): a lineage built through the core's
paths, then the three identity columns set to NULL and the backfill marker removed by direct SQL — in the TEST
only; the code under test reads through public ``SessionDB`` methods.

Without identity completion the target mints random ids on insert, so H_all of the import never equals the
capsule's and every legacy session fails with ``verify_failed``.
"""

from __future__ import annotations

import json
import re
from dataclasses import replace

import pytest

from ctx_handover.engine import session_export, session_import
from ctx_handover.engine.errors import VerifyFailed
from ctx_handover.engine.model import PrepareOutcome
from ctx_handover.tests.unit.B.conftest import DROP, SOURCE_META, export, plan, rich_lineage

UID_RE = re.compile(r"^[0-9a-f]{32}$")


def make_legacy(db, session_id: str) -> list[int]:
    """Turn every row of ``session_id`` into a row of an older build; returns the row ids (see module docstring)."""
    db._write_sql(
        "UPDATE messages SET message_uid = NULL, tool_call_uids = NULL, tool_call_uid = NULL WHERE session_id = ?",
        (session_id,),
    )
    db._write_sql("DELETE FROM state_meta WHERE key IN ('message_uid_backfill', 'message_uid_backfill_id')")
    return [int(r["id"]) for r in db.get_messages(session_id, include_inactive=True)]


def legacy_lineage(b) -> tuple[str, list[str]]:
    """``rich_lineage`` (tool calls, in-place compaction, rotation, rewind) with its ROOT segment made legacy."""
    tip = rich_lineage(b)
    lineage = list(b.db.get_compression_lineage(tip))
    make_legacy(b.db, lineage[0])
    return tip, lineage


def capsule_segments(store, cap) -> list[list[dict]]:
    """The capsule's rows per segment without rewind rows (what the target must hold)."""
    out: list[list[dict]] = []
    for index, row in session_export.iter_capsule_rows(store, cap.capsule_id):
        while len(out) <= index:
            out.append([])
        if row.get("active", 1) or row.get("compacted"):
            out[index].append(row)
    return out


def assert_target_equals_capsule(dst_db, store, cap, new_ids) -> None:
    """Every stored target row equals its capsule row byte-true, apart from storage coordinates and the
    core-derived ``tool_call_uid`` (which must equal the uid of its pairing in the capsule)."""
    for rows, sid in zip(capsule_segments(store, cap), new_ids, strict=True):
        stored = [r for r in dst_db.get_messages(sid, include_inactive=True) if r.get("active") or r.get("compacted")]
        assert len(stored) == len(rows), sid
        pairing: dict[str, str] = {}
        for src, got in zip(rows, stored, strict=True):
            src = {k: v for k, v in src.items() if k not in DROP}
            got = {k: v for k, v in got.items() if k not in DROP}
            if src.get("role") == "assistant" and isinstance(src.get("tool_call_uids"), str):
                pairing.update(json.loads(src["tool_call_uids"]))
            if src.get("role") == "tool" and src.get("tool_call_uid") is None:
                assert got.get("tool_call_uid") in (None, pairing.get(src.get("tool_call_id")))
                got = {**got, "tool_call_uid": None}
            assert src == got


@pytest.mark.parametrize("force_b_prime", [False, True], ids=["path-A", "path-B-prime"])
def test_legacy_rows_import_with_equal_hashes(
    make_session, session_db, target_db, store, source_paths, target_cwd, force_b_prime
):
    """★ A lineage whose root was written by an older build (no ``message_uid``, no ``tool_call_uids``) arrives with
    H_all/H_rows/H_model equal to the capsule on path A and on path B′; every legacy row carries a stable id derived
    from its source row, the target keeps it byte-true, and the source store is not written.

    Mutant: ``session_export.export_rows`` yields the raw rows (no identity completion) ⇒ ``VerifyFailed``.
    """
    tip, lineage = legacy_lineage(make_session)
    before = session_db.get_messages(lineage[0], include_inactive=True)
    assert any(r["role"] == "assistant" and r.get("tool_calls") for r in before)
    assert all(r.get("message_uid") is None and r.get("tool_call_uids") is None for r in before)
    info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    p = plan(target_db, capsule, target_cwd)
    if force_b_prime:
        p = replace(p, segments=tuple(replace(s, path="B'") for s in p.segments))
    prepared = session_import.import_hidden(
        target_db, store, cap.capsule_id, p, local_hermes_version=SOURCE_META["hermes_version"]
    )
    assert (prepared.h_all, prepared.h_rows, prepared.h_model) == (cap.h_all, cap.h_rows, cap.h_model)
    assert_target_equals_capsule(target_db, store, cap, [s.new_id for s in p.segments])
    legacy_rows = capsule_segments(store, cap)[0]
    assert legacy_rows and all(UID_RE.match(r["message_uid"]) for r in legacy_rows)
    assert len({r["message_uid"] for r in legacy_rows}) == len(legacy_rows)
    assert session_db.get_messages(lineage[0], include_inactive=True) == before
    assert capsule["stats"]["identity_completed"] >= len(legacy_rows)
    assert info.lineage_ids == tuple(lineage)


def test_legacy_identity_is_stable_and_core_shaped(make_session, session_db, store, source_paths):
    """Two exports give the same ids (no randomness, no state); call uids are keyed exactly as the core keys them
    (``coalesce_tool_call_id``); rows that already carry identities stay byte-true in the capsule."""
    from agent.message_sanitization import coalesce_tool_call_id

    tip, lineage = legacy_lineage(make_session)
    _info, cap1 = export(session_db, store, tip, source_paths)
    _info, cap2 = export(session_db, store, tip, source_paths)
    assert cap1.capsule_id == cap2.capsule_id and cap1.h_all == cap2.h_all
    segments = capsule_segments(store, cap1)
    for row in segments[0]:
        if row["role"] == "assistant" and row.get("tool_calls"):
            uids = json.loads(row["tool_call_uids"])
            assert set(uids) == {coalesce_tool_call_id(tc) for tc in row["tool_calls"]}
            assert all(UID_RE.match(u) for u in uids.values())
    modern = {int(r["id"]): r for r in session_db.get_messages(lineage[-1], include_inactive=True)}
    for row in segments[-1]:
        assert row == modern[int(row["id"])]


def test_legacy_delta_reverifies(make_session, session_db, target_db, store, source_paths, target_cwd):
    """★ A late row on a legacy lineage: the delta's expected hashes cover the same derived ids the capsule gave the
    legacy rows, so ``apply_delta`` re-verifies on the target.

    Mutant: ``_hashes_now`` hashes the raw rows (no identity completion) ⇒ ``VerifyFailed`` in ``apply_delta``.
    """
    b = make_session
    sid = b.create()
    b.turns(sid, 3, with_tools=True)
    make_legacy(session_db, sid)
    info, cap = export(session_db, store, sid, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    prepared = session_import.import_hidden(target_db, store, cap.capsule_id, plan(target_db, capsule, target_cwd))
    b.user(sid, "late message")
    outcome, rows = session_export.check_revision(session_db, info, cap)
    assert outcome == PrepareOutcome.DELTA
    delta = session_export.export_delta(session_db, info, cap, rows, store=store)
    after = session_import.apply_delta(target_db, store, delta, prepared)
    assert (after.h_all, after.h_rows) == (delta.h_all, delta.h_rows)


def test_legacy_backfill_between_export_and_delta_fails_safe(
    make_session, session_db, hermes_home, target_db, store, source_paths, target_cwd
):
    """The core's own backfill (any ``SessionDB`` open of the source store) mints random ids onto the legacy rows
    after the export: the source changed under the capsule without a revision step, so the delta's expected
    hashes differ and ``apply_delta`` refuses (``VerifyFailed`` ⇒ the conductor aborts before the commit point and
    releases the source; a new handover starts from a fresh export — F ``test_i_readme.py``). Nothing is accepted
    silently."""
    from hermes_state import SessionDB

    b = make_session
    sid = b.create()
    b.turns(sid, 2)
    make_legacy(session_db, sid)
    info, cap = export(session_db, store, sid, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    prepared = session_import.import_hidden(target_db, store, cap.capsule_id, plan(target_db, capsule, target_cwd))
    SessionDB(hermes_home / "state.db").close()  # an open runs one backfill slice (hermes_state_schema.py:1093)
    assert all(r.get("message_uid") for r in session_db.get_messages(sid, include_inactive=True))
    b.user(sid, "late message")
    outcome, rows = session_export.check_revision(session_db, info, cap)
    assert outcome == PrepareOutcome.DELTA
    delta = session_export.export_delta(session_db, info, cap, rows, store=store)
    with pytest.raises(VerifyFailed):
        session_import.apply_delta(target_db, store, delta, prepared)


_CALL_SHAPES = [
    {"id": "call_1", "type": "function"},
    {"call_id": "call_2|fc_9", "id": "fc_9"},
    {"call_id": "  ", "id": " fc_3 "},
    {"call_id": "|tail", "id": "x"},
    {"call_id": 5, "id": "call_4"},
    {"call_id": None, "id": None},
    {},
    "call_5",
    None,
    7,
    ["call_6"],
]


@pytest.mark.parametrize("tc", _CALL_SHAPES, ids=[str(i) for i in range(len(_CALL_SHAPES))])
def test_call_id_mirror_matches_core(tc):
    """``session_export`` keys derived call uids with a stdlib mirror of the core's pairing key; it must agree with
    ``agent.message_sanitization.coalesce_tool_call_id`` for every decoded JSON shape (a drift would only cost a
    ``verify_failed``, never a silent change — this pins it)."""
    from agent.message_sanitization import coalesce_tool_call_id

    assert session_export.coalesce_call_id(tc) == coalesce_tool_call_id(tc)


_UID_MAPS = [
    None,
    "",
    "not json",
    "[]",
    '{"a": "u1"}',
    {"a": "u1", "": "u2", "b": "", "c": 3, "d": ["u3", "u4"], "e": [], "f": ["u5", ""]},
    {"x": ["u6"]},
    [1, 2],
]


@pytest.mark.parametrize("value", _UID_MAPS, ids=[str(i) for i in range(len(_UID_MAPS))])
def test_uid_map_mirror_matches_core(value):
    """The filter applied to stored call-uid maps equals the core's (``hermes_state_identity._uid_map``)."""
    from hermes_state_identity import _uid_map

    assert session_export.uid_map(value) == _uid_map(value)
