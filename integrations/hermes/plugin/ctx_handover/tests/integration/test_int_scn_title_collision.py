"""Mandatory scenario 7 (layer 2, DESIGN §6.6, TESTPLAN E7, spike S1): the target already holds an unrelated
session with the title of the incoming one. The import deduplicates the title (``T #2``) instead of failing with
``IntegrityError UNIQUE constraint failed: sessions.title``; the unrelated session keeps its title."""

from __future__ import annotations

from ctx_handover.engine.model import Mode, Options, PlanHints, SessionRef
from ctx_handover.tests.integration._int3_helpers import arrival, run_job, title_of
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes

TITLE = "Quarterly report"


def test_int_scn_title_collision_dedups(online):
    """Scenario 7: push of ``T`` onto a server that has an unrelated ``T`` ⇒ ``T #2``, no error."""
    server, laptop, _listener, peer = online
    sb = builder(server)
    other = sb.create(title=TITLE, cwd=str(server.home))
    sb.user(other, "unrelated work on the server")
    lb = builder(laptop)
    sid = lb.create(title=TITLE, cwd=str(laptop.home))
    lb.user(sid, "draft the report")
    lb.assistant(sid, "drafted")
    opts = Options(refine="off", yes=True, hints=PlanHints(no_workspace=True))
    res = run_job(laptop, Mode.PUSH, peer, SessionRef(id=sid), options=opts)
    assert res.rc == 0, res.why()
    assert not any("IntegrityError" in str(e) for e in res.errors), res.errors
    tip = arrival(server, res.job_id)["session_id"]
    assert title_of(server, tip) == f"{TITLE} #2"
    assert title_of(server, other) == TITLE, "the unrelated session lost its title"
    assert lineage_hashes(server.db(), tip) == lineage_hashes(laptop.db(), sid)
