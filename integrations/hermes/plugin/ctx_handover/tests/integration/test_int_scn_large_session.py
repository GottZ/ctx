"""Mandatory scenario 6 (layer 2, R2, DESIGN §6.8 path B′, §6.9, TESTPLAN E6): a session whose segment exceeds the
import limit of ``import_sessions`` (> 5 MiB compact JSON; thousands of rows archived by Hermes' default in-place
compaction) is pushed. ``import_sessions`` would refuse it (spike S1), so the target writes it with
``append_messages_batch`` + ``archive_and_compact``. Visible results: the session continues with the same model
history and the **display history is complete** — every archived row the user scrolls back to is there — and every
lineage hash (H_all, H_rows, H_model) is equal."""

from __future__ import annotations

import json

from ctx_handover.engine.model import Mode, Options, PlanHints, SessionRef
from ctx_handover.tests.fakes.sessiondb import large_segment
from ctx_handover.tests.integration._int3_helpers import arrival, histories, run_job
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks

LIMIT = 5 * 1024 * 1024


def _segment_bytes(db, sid: str) -> int:
    rows = db.get_messages(sid, include_inactive=True, include_compacted=True)
    return len(json.dumps(rows, separators=(",", ":"), default=str).encode())


def test_int_scn_session_over_import_limit(online):
    """Scenario 6: a 7 000-row segment of ≈ 6 MiB (6 800 archived rows + live tail) over HTTP."""
    server, laptop, _listener, peer = online
    b = builder(laptop)
    sid = large_segment(b, live=200, archived=6800, size=820, title="Long running", cwd=str(laptop.home))
    src_db = laptop.db()
    assert _segment_bytes(src_db, sid) > LIMIT, "fixture is not over the import limit"
    src_model, src_display = histories(src_db, sid)
    assert len(src_display) > 6000

    opts = Options(refine="off", yes=True, hints=PlanHints(no_workspace=True))
    res = run_job(laptop, Mode.PUSH, peer, SessionRef(id=sid), options=opts)
    assert res.rc == 0, res.why()
    tip = arrival(server, res.job_id)["session_id"]
    dst_db = server.db()
    dst_model, dst_display = histories(dst_db, tip)
    assert len(dst_display) == len(src_display), "display history incomplete at the target"
    assert dst_display == src_display
    assert dst_model == src_model
    assert lineage_hashes(dst_db, tip) == lineage_hashes(src_db, sid)
    assert not session_marks(dst_db, tip)["hidden"]
