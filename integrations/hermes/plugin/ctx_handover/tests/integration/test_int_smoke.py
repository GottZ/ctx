"""Smoke: the composition roots wire the real modules end to end (push of a session without workspace)."""

from __future__ import annotations

from ctx_handover.engine.model import Mode, Options, PlanHints, Role, SessionRef
from ctx_handover.tests.integration.conftest import builder


def test_int_smoke_push_session_only(online):
    server, laptop, listener, peer = online
    b = builder(laptop)
    sid = b.create(title="Smoke", cwd=str(laptop.home))
    b.user(sid, "hello")
    b.assistant(sid, "hi there")
    opts = Options(refine="off", yes=True, hints=PlanHints(no_workspace=True))
    job_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=opts)
    row = laptop.ledger().job(job_id, Role.CONDUCTOR)
    errors = [e["data"] for e in laptop.ledger().events(job_id, Role.CONDUCTOR) if e["kind"] == "error"]
    assert rc == 0, (row["state"], errors)
