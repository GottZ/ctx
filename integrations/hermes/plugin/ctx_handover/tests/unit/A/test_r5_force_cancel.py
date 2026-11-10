"""``/handover cancel --force`` ends a handover stuck in ABORTING here (review round 4, finding R4-M1).

The reviewer's chain: a pull locks its remote source, then aborts (the local target's prepare fails); the source
answers ``not_found`` to the release (a new pairing of the device ended the old rows there). "Not found" after the lock
intent is no confirmation, so the job stays ``aborting``; every janitor run asks the peer again, ``cancel`` answered
"Cancel requested" and changed nothing, ``gc --job`` refused, ``status`` listed the job as active for ever.

``cancel --force`` runs one more abort round (``Conductor.force_abort``): the side on this host takes its normal path
(the local source's release — leases, marks, veto — or the local target's abort; a failure there stays an error), the
side on the peer may stay unconfirmed and the answer names what may remain there. A job from the commit intent on is
refused (the target may hold the active session; never two active copies), and so is leaving a target that may have
committed.

Red at ``ad5a5cfa``: ``cancel_answer`` has no ``force`` (``TypeError``) and a plain ``cancel`` of the stuck job answers
``requested=True`` "Cancel requested for job …"."""

from __future__ import annotations

import threading

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.errors import ImportFailed, InvalidState, NotFound, PeerUnreachable
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Mode, OpState, PreparedInfo, Role
from ctx_handover.tests.fakes.apis_fake import FakePlanning, World
from ctx_handover.tests.unit.A.harness import FAST
from ctx_handover.tests.unit.A.test_jobs import RunnerEnv
from ctx_handover.tests.unit.A.test_l2_source_real import mk  # noqa: F401  (fixture)


def _janitor(e: RunnerEnv) -> dict:
    """A later start of the profile; the injected faults stay (the peer still does not know the job)."""
    e.clock.advance(61)
    return jobs.janitor(e.ledger, e.runner, None, flush_outbox=None, background=False)


def _stuck_pull(tmp_path, clock) -> tuple[RunnerEnv, str]:
    e = RunnerEnv(tmp_path, clock, mode=Mode.PULL)
    e.world.inject("target", "start_prepare", "error", error=ImportFailed("target prepare failed"))
    e.world.inject("source", "start_release", "error", error=NotFound("job not found"), times=10_000)
    job_id = e.start()
    e.runner.run_foreground(job_id)
    assert e.state(job_id) == "aborting"
    assert (e.ledger.job(job_id, Role.CONDUCTOR)["last_error"] or {}).get("code") == "not_found"
    return e, job_id


def _active(e: RunnerEnv, job_id: str) -> bool:
    return any(j["job_id"] == job_id and j.get("active") for j in e.runner.status(None)["jobs"])


def test_r5_reviewer_scenario_cancel_force_ends_the_stuck_pull(tmp_path, clock):
    """★ R4-M1, the reviewer's probe: after two janitor runs the job is still ``aborting``; a plain ``cancel`` no longer
    claims a request it does not make and names ``--force`` (so does ``status``); ``cancel --force`` ends the job
    ``aborted`` with the note that the source on the peer did not confirm and how to take the session back there. The
    local target's abort was confirmed; afterwards ``gc --job`` removes the job and the janitor leaves it alone.

    Red at ``ad5a5cfa``: ``assert (True, 'aborting', None) == (False, 'aborting', 'aborting')`` (plain cancel), then
    ``TypeError: cancel_answer() got an unexpected keyword argument 'force'``.

    Mutant ``R5-no-leave`` (``_leave_on_peer`` always raises): the forced answer is ``requested=False``, the job stays
    ``aborting``."""
    e, job_id = _stuck_pull(tmp_path, clock)
    for _ in range(2):
        _janitor(e)
        assert e.state(job_id) == "aborting"

    plain = e.runner.cancel_answer(job_id)
    assert (plain.requested, plain.state, plain.reason) == (False, "aborting", "aborting"), plain
    assert "--force" in plain.text and jobs.short(job_id) in plain.text, plain.text
    summary = next(j for j in e.runner.status(None)["jobs"] if j["job_id"] == job_id)
    assert "--force" in (summary.get("next_action") or ""), summary

    forced = e.runner.cancel_answer(job_id, force=True)

    assert (forced.requested, forced.state) == (True, "aborted"), forced
    assert "nothing of it is kept here" in forced.text, forced.text
    assert "did not confirm that it gave the session back (not_found)" in forced.text, forced.text
    assert "/handover reclaim" in forced.text, forced.text
    row = e.ledger.job(job_id, Role.CONDUCTOR)
    assert row["state"] == "aborted"
    assert [(f["side"], f["code"]) for f in row["result"]["forced"]] == [("source", "not_found")], row["result"]
    assert any("did not confirm" in w for w in row["result"]["warnings"]), row["result"]
    assert e.world.target.state == "aborted"  # the side on this host confirmed its abort
    assert not _active(e, job_id)
    assert _janitor(e)["conductor"] == []
    jobs.gc_job(e.ledger, job_id, data_dir=tmp_path / "pd")
    assert e.ledger.job(job_id, Role.CONDUCTOR) is None


def test_r5_force_on_a_job_the_peer_now_answers_ends_without_a_note(tmp_path, clock):
    """The peer answers again by the time ``--force`` runs: nothing is left unconfirmed, the job ends ``aborted`` the
    normal way and the answer carries no note about the peer."""
    e, job_id = _stuck_pull(tmp_path, clock)
    e.world.faults.clear()
    forced = e.runner.cancel_answer(job_id, force=True)
    assert (forced.requested, forced.state) == (True, "aborted"), forced
    assert "did not confirm" not in forced.text, forced.text
    assert "forced" not in (e.ledger.job(job_id, Role.CONDUCTOR)["result"] or {})


def _prepared(capsule: dict) -> PreparedInfo:
    return PreparedInfo("n", "n", "T", capsule["h_all"], capsule["h_rows"], capsule["h_model"], "A", (), (), ())


def _visible(session_db, sid: str) -> bool:
    row = session_db.get_session(sid) or {}
    return not row.get("archived") and not row.get("hidden") and row.get("end_reason") != "handed_over"


def _abort_push_with_unreachable_target(env, clock, *, commit: bool):
    """A push whose source on this host is ``prepared`` and whose target on the peer stops answering: the conductor
    row is ABORTING after ``target.open_job`` and ``source.lock`` (and ``intent: commit`` when ``commit``)."""
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    capsule = env.source.start_export(job).result
    env.source.start_pack(job, env.final(plan))
    st = env.source.start_prepare(job, _prepared(capsule))
    assert st.state is OpState.DONE and st.result["outcome"] == "ok", st
    led = env.ledger
    for step in ("target.open_job", "source.lock"):
        led.event(job.job_id, Role.CONDUCTOR, "intent", {"step": step}, epoch=0)
    if commit:
        assert led.intent_commit(job.job_id, 0)
    led.transition(job.job_id, Role.CONDUCTOR, "*", "aborting", {"ctx": {"abort_reason": "commit_failed"}}, epoch=0)
    world = World(clock)
    world.inject("target", "abort", "error", error=PeerUnreachable("laptop is offline"), times=10_000)
    runner = jobs.JobRunner(
        led,
        None,
        apis_factory=lambda row: (env.source, world.target),
        planning=FakePlanning(),
        clock=clock,
        sleep=world.sleep,
        poll=FAST,
        keepalive=False,
    )
    return job, runner


def test_r5_force_of_a_push_releases_the_local_source_the_normal_way(mk, clock, session_db):  # noqa: F811
    """★ R4-M1, the side on this host: a push stuck in ABORTING because its target on the peer is unreachable keeps the
    local source ``prepared`` (a janitor run changes nothing — the source is never released behind an unconfirmed
    target). ``cancel --force`` releases it through ``LocalSource.start_release`` → ``lock.release``: the ledger says
    ``released``, the session is visible again (not archived, hidden or ended ``handed_over``), no veto row is left and
    the turn lease is free. The answer says the session is usable and that the peer may keep a hidden remainder.

    Mutant ``R5-shortcut`` (``force_abort`` goes straight to ABORTED instead of running the abort round): ``prepared``
    stays, the session stays hidden — red at the ``released`` assertion."""
    env = mk(remote_driven=False)
    job, runner = _abort_push_with_unreachable_target(env, clock, commit=False)
    runner.recover(job.job_id, background=False)
    assert env.ledger.job(job.job_id, Role.CONDUCTOR)["state"] == "aborting"
    assert env.ledger.source_state(job.job_id)["state"] == "prepared"
    assert not _visible(session_db, env.sid)

    forced = runner.cancel_answer(job.job_id, force=True)

    assert (forced.requested, forced.state) == (True, "aborted"), forced
    assert "the session is yours again" in forced.text, forced.text
    assert "did not confirm the cancel (peer_unreachable)" in forced.text, forced.text
    assert env.ledger.source_state(job.job_id)["state"] == "released"
    assert _visible(session_db, env.sid)
    assert [r for r in env.ledger.veto_rows() if r["job_id"] == job.job_id] == []
    assert session_db.try_acquire_session_turn_lease(env.sid, "pid=9:turn", ttl_seconds=60, patience_s=0)
    session_db.release_session_turn_lease(env.sid, "pid=9:turn")


def test_r5_force_never_leaves_a_target_that_may_have_committed(mk, clock, session_db):  # noqa: F811
    """★ Never two active copies: with ``intent: commit`` journaled, an unreachable target may have committed.
    ``--force`` does not leave it: the job stays ``aborting``, the local source stays ``prepared`` (the session stays
    handed over), and the answer says the job was not ended.

    Mutant ``R5-target-after-commit`` (the commit-intent check in ``_leave_on_peer`` removed): the source is released
    next to a target that may be active — red at ``requested``."""
    env = mk(remote_driven=False)
    job, runner = _abort_push_with_unreachable_target(env, clock, commit=True)

    forced = runner.cancel_answer(job.job_id, force=True)

    assert (forced.requested, forced.state, forced.reason) == (False, "aborting", "not_forced"), forced
    assert "could not be ended here" in forced.text, forced.text
    assert env.ledger.job(job.job_id, Role.CONDUCTOR)["state"] == "aborting"
    assert env.ledger.source_state(job.job_id)["state"] == "prepared"
    assert not _visible(session_db, env.sid)


def test_r5_force_keeps_a_failure_on_this_host_an_error(tmp_path, clock):
    """★ No shortcut on this host: in a pull the target is local; when its abort fails (here: another process holds its
    row), ``--force`` does not end the job, and the source's release is not asked for either.

    Mutant ``R5-any-side`` (``_leave_on_peer`` ignores which side is the peer's): the job ends ``aborted`` over a target
    that never confirmed its abort."""
    e, job_id = _stuck_pull(tmp_path, clock)
    e.world.faults.clear()
    busy = InvalidState("another process is working on this job", reason="busy_elsewhere")
    e.world.inject("target", "abort", "error", error=busy, times=10_000)
    released_before = e.world.started.count(("source", "release"))

    forced = e.runner.cancel_answer(job_id, force=True)

    assert (forced.requested, forced.reason) == (False, "not_forced"), forced
    assert e.state(job_id) == "aborting"
    assert e.world.started.count(("source", "release")) == released_before


@pytest.mark.parametrize("state", ["committing", "in_doubt", "committed", "retiring", "retired", "continuing"])
def test_r5_force_is_refused_from_the_commit_intent_on(tmp_path, clock, state):
    """★ ``--force`` never ends a job that is committing or committed (the target may hold the active session): a clear
    refusal (``committing``) that names ``--force`` and ``/handover reclaim``; the participants are not even composed
    and the job keeps its state.

    Mutant ``R5-no-commit-gate`` (the ``_PAST_COMMIT`` check removed): the answer is the plain "past its commit point"
    without the ``--force`` sentence — red at the text."""
    e = RunnerEnv(tmp_path, clock, mode=Mode.PUSH)
    composed: list[str] = []
    e.runner._apis_factory = lambda row: composed.append(row["job_id"]) or (e.world.source, e.world.target)
    job_id = ulid()
    e.ledger.create_job(job_id, Role.CONDUCTOR, Mode.PUSH, None, {"session_ref": {"id": "tip"}})
    assert e.ledger.intent_commit(job_id, 0)
    e.ledger.transition(job_id, Role.CONDUCTOR, "*", state, {}, epoch=0)

    forced = e.runner.cancel_answer(job_id, force=True)

    assert (forced.requested, forced.state, forced.reason) == (False, state, "committing"), forced
    assert "also with --force" in forced.text and "/handover reclaim" in forced.text, forced.text
    assert composed == []
    assert e.state(job_id) == state


def test_r5_force_on_a_finished_or_running_job_is_the_plain_answer(tmp_path, clock):
    """``--force`` adds nothing outside ABORTING: a ``done`` job is "too late", an ``aborted`` one "already aborted",
    and a job before the commit point gets the normal cancel request."""
    e = RunnerEnv(tmp_path, clock, mode=Mode.PUSH)
    ids = {}
    for state in ("done", "aborted", "packing"):
        ids[state] = ulid()
        e.ledger.create_job(ids[state], Role.CONDUCTOR, Mode.PUSH, None, {"session_ref": {"id": "tip"}})
        if state == "done":
            assert e.ledger.intent_commit(ids[state], 0)
        e.ledger.transition(ids[state], Role.CONDUCTOR, "*", state, {}, epoch=0)
    done = e.runner.cancel_answer(ids["done"], force=True)
    assert (done.requested, done.reason) == (False, "finished") and "already done" in done.text, done
    aborted = e.runner.cancel_answer(ids["aborted"], force=True)
    assert (aborted.requested, aborted.reason) == (False, "finished") and "already aborted" in aborted.text, aborted
    running = e.runner.cancel_answer(ids["packing"], force=True)
    assert running.requested is True and running.text.startswith("Cancel requested"), running
    assert e.ledger.cancel_requested(ids["packing"]) is True


def test_r5_force_that_outlasts_the_answer_says_so_and_finishes(tmp_path, clock, monkeypatch):
    """A peer that is slow to refuse the release keeps the forced round running past ``CANCEL_SETTLE_S``: the answer
    does not claim the job ended (nor that it could not be ended) — it says the end is under way; the round then
    finishes ``aborted`` on its own."""
    e, job_id = _stuck_pull(tmp_path, clock)
    gate = threading.Event()

    def slow_release(job, reason):
        gate.wait(30)
        raise NotFound("job not found")

    monkeypatch.setattr(jobs, "CANCEL_SETTLE_S", 0.2)
    monkeypatch.setattr(e.world.source, "start_release", slow_release)

    forced = e.runner.cancel_answer(job_id, force=True)

    assert (forced.requested, forced.state, forced.reason) == (False, "aborting", "not_forced"), forced
    assert "slow to answer" in forced.text and "could not be ended" not in forced.text, forced.text
    gate.set()
    e.runner.join(30)
    assert e.state(job_id) == "aborted"
