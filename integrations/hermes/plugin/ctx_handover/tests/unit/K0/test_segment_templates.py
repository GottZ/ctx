"""K0: session-wide ``large_segment`` templates (contract K-6, W001).

A copied segment must be indistinguishable, for every test, from one built directly through the core paths:
same rows (every column), same session row up to the core's own wall-clock stamps, same builder counters, and a
``SessionDB`` that keeps writing and reopening afterwards.
"""

from __future__ import annotations

import sqlite3

import pytest

from ctx_handover.tests.conftest import SegmentTemplates, require_upstream
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.sessiondb import SessionBuilder, large_segment

SHAPE = {"live": 30, "archived": 41, "size": 16, "generations": 2, "cwd": "/srv/src-work"}
RANDOM_SESSION_COLUMNS = {"id", "started_at"}
"""What two direct builds of the same shape already differ in (minted id, the core's wall-clock start); measured
with ``lab/L2-redproof/wp-k0/diffcols.py``. Rows differ only in ``session_id`` and the random uids below."""
RANDOM_UID_COLUMNS = ("message_uid", "tool_call_uid", "tool_call_uids")


def _db(path):
    require_upstream()
    from hermes_state import SessionDB

    path.mkdir(parents=True, exist_ok=True)
    return SessionDB(path / "state.db")


def _rows(db, sid, *, uids: bool = False):
    """Every column of every row; the core's random uids (``RANDOM_UID_COLUMNS``) become first-appearance labels
    unless ``uids`` keeps them, so clones and references still share one label."""
    labels: dict[str, int] = {}

    def label(value):
        if isinstance(value, str):
            return labels.setdefault(value, len(labels))
        if isinstance(value, (list, tuple)):
            return [label(v) for v in value]
        if isinstance(value, dict):
            return {k: label(v) for k, v in value.items()}
        return value

    out = []
    for m in db.get_messages(sid, include_inactive=True):
        row = {k: v for k, v in m.items() if k != "session_id"}
        if not uids:
            row.update({k: label(row[k]) for k in RANDOM_UID_COLUMNS if row.get(k) is not None})
        out.append(row)
    return out


def _session(db, sid):
    row = dict(db.get_session(sid))
    return {k: v for k, v in row.items() if k not in RANDOM_SESSION_COLUMNS}


@pytest.fixture
def store(tmp_path):
    return SegmentTemplates(tmp_path / "templates")


def test_copy_equals_a_direct_build(tmp_path, store):
    direct_db, a_db, b_db = (_db(tmp_path / n) for n in ("direct", "a", "b"))
    try:
        direct = SessionBuilder(direct_db, clock=FakeClock())
        a = SessionBuilder(a_db, clock=FakeClock(), templates=store)
        b = SessionBuilder(b_db, clock=FakeClock(), templates=store)
        sid_direct = large_segment(direct, **SHAPE)
        sid_a = large_segment(a, **SHAPE)
        sid_b = large_segment(b, **SHAPE)
        assert (store.builds, store.copies) == (1, 2)
        assert sid_a == sid_b
        assert _rows(a_db, sid_a) == _rows(direct_db, sid_direct)
        assert _rows(a_db, sid_a, uids=True) == _rows(b_db, sid_b, uids=True)
        assert _session(a_db, sid_a) == _session(direct_db, sid_direct)
        assert a.counters() == direct.counters() == b.counters()
        assert a.counts(sid_a) == direct.counts(sid_direct)
        tips = []
        for builder, db, sid in ((direct, direct_db, sid_direct), (a, a_db, sid_a)):
            tip = builder.rotate(sid, keep_last=2)
            builder.turns(tip, 3, with_tools=True)
            tips.append(tip)
            assert db.get_resume_conversations(tip)[0]
        assert _rows(a_db, tips[1]) == _rows(direct_db, tips[0])
    finally:
        for db in (direct_db, a_db, b_db):
            db.close()
    reopened = _db(tmp_path / "a")
    try:
        assert len(reopened.get_messages(sid_a, include_inactive=True)) == 30 + 41
    finally:
        reopened.close()


def test_copy_keeps_the_target_file_identity(tmp_path, store):
    """★ The copy keeps the target's ``application_id`` and ``state_meta`` identity rows, so the open
    ``SessionDB`` — which compares the header id on every write (``hermes_state.py:1240-1250``) — keeps writing,
    also after a checkpoint moved the copied pages into the main file.

    Mutant: skip ``PRAGMA application_id = <target>`` in ``conftest._restore``.
    """
    db = _db(tmp_path / "t")
    path = tmp_path / "t" / "state.db"
    try:
        probe = sqlite3.connect(path)
        app_before = probe.execute("PRAGMA application_id").fetchone()[0]
        meta_before = sorted(probe.execute("SELECT key, value FROM state_meta").fetchall())
        probe.close()
        builder = SessionBuilder(db, clock=FakeClock(), templates=store)
        sid = large_segment(builder, **SHAPE)
        assert store.copies == 1
        probe = sqlite3.connect(path)
        assert probe.execute("PRAGMA application_id").fetchone()[0] == app_before != 0
        assert sorted(probe.execute("SELECT key, value FROM state_meta").fetchall()) == meta_before
        probe.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        probe.close()
        builder.turns(sid, 2)
        db.set_session_title(sid, "after copy")
        assert db.get_session(sid)["title"] == "after copy"
    finally:
        db.close()


def test_target_with_rows_builds_directly(tmp_path, store):
    db = _db(tmp_path / "t")
    try:
        builder = SessionBuilder(db, clock=FakeClock(), templates=store)
        first = builder.create(title="already here")
        sid = large_segment(builder, **SHAPE)
        assert (store.builds, store.copies) == (0, 0)
        assert db.get_session(first) is not None and builder.counts(sid).rows == 71
    finally:
        db.close()


def test_ineligible_builders_build_directly(tmp_path, store):
    db = _db(tmp_path / "wall")
    try:
        wall = SessionBuilder(db, clock=None, templates=store)
        large_segment(wall, live=3, archived=0)
        assert (store.builds, store.copies) == (0, 0)
    finally:
        db.close()
    db = _db(tmp_path / "kw")
    try:
        odd = SessionBuilder(db, clock=FakeClock(), templates=store)
        assert store.copy_into(odd, ("large_segment", 3, 0, 64, 1), {"x": object()}, lambda twin: "never") is None
        assert (store.builds, store.copies) == (0, 0)
    finally:
        db.close()


def test_key_includes_clock_and_counters(tmp_path, store):
    dbs = [_db(tmp_path / f"d{i}") for i in range(3)]
    try:
        large_segment(SessionBuilder(dbs[0], clock=FakeClock(), templates=store), live=3, archived=0)
        late = SessionBuilder(dbs[1], clock=FakeClock(start=1_800_000_000.0), templates=store)
        large_segment(late, live=3, archived=0)
        moved = SessionBuilder(dbs[2], clock=FakeClock(), templates=store)
        moved.next_seq()
        large_segment(moved, live=3, archived=0)
        assert (store.builds, store.copies) == (3, 3)
    finally:
        for db in dbs:
            db.close()


def test_make_session_uses_the_session_templates(make_session, segment_templates):
    assert make_session.templates is segment_templates
    if segment_templates is None:
        pytest.skip("--no-segment-templates")
    before = segment_templates.copies
    sid = large_segment(make_session, live=4, archived=7, size=8)
    assert segment_templates.copies == before + 1
    assert make_session.counts(sid).rows == 11
