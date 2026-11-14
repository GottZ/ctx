"""A session of about one million tokens compacted in place (TESTPLAN §3
``test_int_one_million_token_in_place_session``; DESIGN §6.8 path B′, §6.9; [SK H1], R2).

Hermes' default configuration compacts in place, so a long session keeps thousands of archived rows in ONE
segment; the 10 000-row limit of ``import_sessions`` counts them ([SK H1] executed: 15 613 rows, 12.6 MB). The
segment here has that shape — 10 408 archived + 5 205 live rows of ~800 bytes, built through the core's own
compaction path — and is pushed over the real HTTP listener. The target must take path B′ (lossless), so every
stored row, the model history and the display history (which shows the archived rows) are equal on both sides, and
the whole handover stays under 60 s."""

from __future__ import annotations

import time

from ctx_handover.engine.model import Mode, Options, PlanHints, SessionRef
from ctx_handover.tests.fakes.sessiondb import large_segment
from ctx_handover.tests.integration._int2_helpers import ack_ids, conductor_ctx, histories, outcome, stored_rows
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks

LIVE, ARCHIVED, ROW_BYTES = 5205, 10_408, 800
BUDGET_S = 60.0


def test_int_one_million_token_in_place_session(online):
    """★ 15 613 rows (10 408 archived) through B′; display history equal; < 60 s [SK H1].

    Mutant: B′ replaced by the v1 path B (only the live rows appended, no archived rows, no ``archive_and_compact``)
    — or by path A, which ``import_sessions`` refuses above 10 000 rows."""
    server, laptop, _listener, peer = online
    b = builder(laptop)
    sid = large_segment(b, live=LIVE, archived=ARCHIVED, size=ROW_BYTES, cwd=str(laptop.home), title="Long haul")
    counts = b.counts(sid)
    assert (counts.active, counts.archived) == (LIVE, ARCHIVED)
    src_db = laptop.db()
    src_hashes = lineage_hashes(src_db, sid)
    src_histories = histories(src_db, sid)
    assert len(src_histories[1]) > ARCHIVED  # the display history shows the archived rows

    opts = Options(refine="off", yes=True, hints=PlanHints(no_workspace=True), stall_timeout_s=120)
    started = time.monotonic()
    job_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=opts)
    elapsed = time.monotonic() - started
    assert rc == 0, outcome(laptop, job_id)

    prepared = conductor_ctx(laptop, job_id)["prepared"]
    assert prepared["import_path"] == "B'", prepared["import_path"]
    _root, new_tip = ack_ids(laptop, job_id)
    dst_db = server.db()
    assert lineage_hashes(dst_db, new_tip) == src_hashes
    assert histories(dst_db, new_tip) == src_histories  # model AND display history
    src_rows, dst_rows = stored_rows(src_db, sid), stored_rows(dst_db, new_tip)
    assert len(dst_rows) == LIVE + ARCHIVED
    assert sum(1 for r in dst_rows if not r["active"] and r["compacted"]) == ARCHIVED
    assert dst_rows == src_rows
    assert not session_marks(dst_db, new_tip)["hidden"]
    assert elapsed < BUDGET_S, f"handover of {LIVE + ARCHIVED} rows took {elapsed:.1f} s"
