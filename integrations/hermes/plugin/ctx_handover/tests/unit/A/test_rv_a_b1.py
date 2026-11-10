"""First review round, finding B1 (the first external review §2): a release that ran for an earlier hold of the source
is no answer for a later hold of the same job.

The janitor releases the local source of a job whose owner went quiet (``ParticipantRegistry.recover`` →
``LocalSource.start_release``); ``/handover resume`` locks the same job again (``Ledger.source_lock`` re-locks a
``released`` row). Every later abort — the target refuses the commit, ``/handover cancel``, ``export --keep`` after
the commit — asks for the release again. That release has to run for the current hold: the source ends ``released``
and the session is usable again (promise b), never a finished operation of the earlier hold that reports
``released`` while the source stays ``prepared``/``locked``."""

from __future__ import annotations

from ctx_handover.engine import jobs
from ctx_handover.engine.errors import AlreadyHandingOver
from ctx_handover.engine.model import OpState, PreparedInfo
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.unit.A.test_l2_source_real import mk  # noqa: F401  (fixture)


def _prepared(capsule: dict) -> PreparedInfo:
    return PreparedInfo("n", "n", "T", capsule["h_all"], capsule["h_rows"], capsule["h_model"], "A", (), (), ())


def _janitor_releases(env, clock) -> None:
    """What a janitor run of another process does once the job's owner went quiet (conductor process gone)."""
    clock.advance(61)
    reg = ParticipantRegistry(env.ledger, lambda role, peer: env.source, clock=clock)
    did = jobs.janitor(env.ledger, None, reg, flush_outbox=None, background=False)
    assert [d[2] for d in did["participant"]] == ["released"], did
    assert env.ledger.source_state(did["participant"][0][0])["state"] == "released"


def test_rv_a_b1_release_after_resume_and_prepare_releases_the_source(mk, clock, session_db):  # noqa: F811
    env = mk(remote_driven=False)
    title = (session_db.get_session(env.sid) or {}).get("title")
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    _janitor_releases(env, clock)
    env.source.lock(job, env.ref, plan.revision)  # /handover resume -> LOCKING again
    capsule = env.source.start_export(job).result
    env.source.start_pack(job, env.final(plan))
    st = env.source.start_prepare(job, _prepared(capsule))
    assert st.state is OpState.DONE and st.result["outcome"] == "ok", st
    assert env.ledger.source_state(job.job_id)["state"] == "prepared"

    st = env.source.start_release(job, "commit_failed")  # Conductor._aborting after target.abort

    assert st.state is OpState.DONE, st
    assert st.result["state"] == "released"
    assert env.ledger.source_state(job.job_id)["state"] == "released"
    row = session_db.get_session(env.sid) or {}
    assert not row.get("archived") and not row.get("hidden"), row
    assert row.get("title") == title
    assert [r for r in env.ledger.veto_rows() if r["job_id"] == job.job_id] == []


def test_rv_a_b1_release_after_resume_frees_a_locked_source_for_the_next_handover(mk, clock):  # noqa: F811
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    _janitor_releases(env, clock)
    env.source.lock(job, env.ref, plan.revision)
    assert env.ledger.source_state(job.job_id)["state"] == "locked"

    st = env.source.start_release(job, "kept")  # also ``export --keep`` after the commit (conductor _retiring)

    assert st.state is OpState.DONE and st.result["state"] == "released", st
    assert env.ledger.source_state(job.job_id)["state"] == "released"
    nxt = env.job()
    try:
        env.source.lock(nxt, env.ref, plan.revision)
    except AlreadyHandingOver as exc:  # pragma: no cover - the failure this test pins
        raise AssertionError(f"the session is still held by the released job: {exc}") from exc
    assert env.ledger.source_state(nxt.job_id)["state"] == "locked"


def test_rv_a_b1_release_is_still_one_operation_per_hold(mk, clock):  # noqa: F811
    """Binding the release to the hold must not turn a repeated request into a second operation: within one hold
    the same release is returned (route #10 and the conductor may ask twice)."""
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    first = env.source.start_release(job, "cancelled")
    again = env.source.start_release(job, "cancelled")
    assert first.state is OpState.DONE and again.op_id == first.op_id
    env.source.lock(job, env.ref, plan.revision)  # resumed: a new hold
    third = env.source.start_release(job, "cancelled")
    assert third.op_id != first.op_id
    assert env.ledger.source_state(job.job_id)["state"] == "released"


def test_rv_a_release_warnings_travel_with_the_release(mk, monkeypatch):  # noqa: F811
    """The hand-over: what ``lock.release`` could not undo (placeholder rows not rewound, a mark not restored) is
    named in the release operation's result once ``lock.release`` returns its ``_undo`` report (today ``None``: then
    there is nothing to name)."""
    from ctx_handover.engine import lock

    real = lock.release

    def reporting(*args, **kw):
        real(*args, **kw)
        return {"state": "released", "warnings": ["placeholder rows not rewound: database is locked"]}

    monkeypatch.setattr(lock, "release", reporting)
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    st = env.source.start_release(job, "cancelled")
    assert st.state is OpState.DONE
    assert st.result == {"state": "released", "warnings": ["placeholder rows not rewound: database is locked"]}


def test_rv_a_release_warnings_reach_the_job_result(tmp_path, clock):
    """The conductor names the release's warnings in the aborted job's result (the final message shows them)."""
    from ctx_handover.engine.errors import HandoverError
    from ctx_handover.engine.model import Role
    from ctx_handover.tests.unit.A.harness import Harness

    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_prepare", "error", error=HandoverError("no", retryable=False))
    src = h.world.source
    real = src.start_release

    def release(job, reason):
        st = real(job, reason)
        op = src.ops[st.op_id]
        done = op.complete
        op.complete = lambda: {**done(), "warnings": ["a title mark was not restored"]}
        return st

    src.start_release = release
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    row = h.ledger.job(h.job_id, Role.CONDUCTOR)
    assert row["state"] == "aborted"
    assert any("a title mark was not restored" in w for w in row["result"].get("warnings") or [])
