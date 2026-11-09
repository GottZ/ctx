"""Review round 4, package r4-a — the success sweep after R3-H1 (orchestrator): every place that sets or reports
``released``, ``done``, ``aborted``, ``retired`` or ``reclaimed`` is held against one rule — success only from a
fresh, explicitly positive answer about the hold that exists now (B1, V37). The deviations found, one test each:

* ``LocalSource.start_retire`` / ``op_status``: a stored "retired" answered after the user reclaimed the session
  (the conductor went RETIRED → CONTINUING: a turn on the target next to the active source, M27).
* ``LocalSource.op_status``: a stored "released" answered while the source holds the session again.
* ``LocalSource.start_prepare``: an "ok" of an earlier hold answered a later one (same capsule/pack ids after a
  re-export of an unchanged session) — the conductor would commit with the source only ``locked``.
* ``LocalSource.heartbeat``: silent (= lease held) while a release or reclaim had given the leases up.
* ``lock.reclaim --force`` of a handover in progress: released a source that had become ``prepared`` meanwhile,
  without the warning that the target may already be active.
* ``Conductor._aborting``: ``not_found`` from the release of a job that asked the source to lock taken as "released".
* ``Conductor.resolve_in_doubt``: returned ``"aborted"`` for a job that was only ``aborting``.
* ``jobs.gc_job``: a finished-looking job whose source handover still holds the session was purged.

Red before r4-a: see each test's docstring."""

from __future__ import annotations

import pytest

from ctx_handover.engine import jobs, lock
from ctx_handover.engine.errors import ImportFailed, InvalidState, LeaseLost, NotFound
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import (
    CommitAck,
    DestMode,
    Mode,
    OpState,
    PathMapEntry,
    PreparedInfo,
    Role,
    SessionInfo,
)
from ctx_handover.tests.unit.A.harness import Harness
from ctx_handover.tests.unit.A.test_l2_source_real import mk  # noqa: F401  (fixture)


def _prepared(capsule: dict) -> PreparedInfo:
    return PreparedInfo("n", "n", "T", capsule["h_all"], capsule["h_rows"], capsule["h_model"], "A", (), (), ())


def _ack() -> CommitAck:
    return CommitAck(
        "n", "n", "T", (("w0", "/x", DestMode.FRESH),), (PathMapEntry("/a", "/b", "workspace"),), "m", True, (), 1.0
    )


def _to_prepared(env):
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    capsule = env.source.start_export(job).result
    env.source.start_pack(job, env.final(plan))
    prepared = _prepared(capsule)
    st = env.source.start_prepare(job, prepared)
    assert st.state is OpState.DONE and st.result["outcome"] == "ok", st
    return job, plan, prepared, st


# -- retire after a reclaim --------------------------------------------------------------------------------------


def test_r4_a_a_stored_retire_is_no_answer_after_a_reclaim(mk, session_db):  # noqa: F811
    """★ The retire finished, the conductor did not hear it (died before it journaled RETIRED, or polls late), the
    user reclaimed the session here. The repeated retire — and the poll of the finished one — answer the refusal the
    retire gives now (``reclaimed``): the conductor ends with "the copies are independent" and starts no turn on the
    target (R10, M27), never "retired".

    Red before r4-a: ``DONE {'state': 'retired'}`` from all three (start, ``op_status``, the status route's
    ``ops``). Mutant ``SWEEP-retire-cached`` (``_fresh`` passes a retire through) ⇒ the same; mutant
    ``SWEEP-status-ops-raw`` (route #8 without the check) ⇒ the route's answer."""
    env = mk(remote_driven=False)
    job, _plan, _prepared_info, _st = _to_prepared(env)
    first = env.source.start_retire(job, _ack())
    assert first.state is OpState.DONE and first.result["state"] == "retired"
    out = lock.reclaim(env.db, env.ledger, env.sid, force=False)
    assert out["state"] == "reclaimed", out
    again = env.source.start_retire(job, _ack())
    polled = env.source.op_status(job, first.op_id)
    remote = env.source.status(job).ops["retire"]  # route #8: how ``RemoteSource.op_status`` polls it
    assert remote.op_id == first.op_id
    for st in (again, polled, remote):
        assert st.state is OpState.FAILED, st
        assert st.error["code"] == "invalid_state" and st.error["details"]["state"] == "reclaimed", st.error


def test_r4_a_a_retire_that_stands_is_still_answered(mk):  # noqa: F811
    """Counter-check: while the ledger says ``retired`` the finished retire is returned as it is (one operation)."""
    env = mk(remote_driven=False)
    job, *_ = _to_prepared(env)
    first = env.source.start_retire(job, _ack())
    again = env.source.start_retire(job, _ack())
    assert again.op_id == first.op_id and again.state is OpState.DONE
    assert env.source.op_status(job, first.op_id).state is OpState.DONE


# -- a stored release next to an active hold -----------------------------------------------------------------------


def test_r4_a_a_polled_release_is_no_answer_for_an_active_hold(mk):  # noqa: F811
    """★ ``op_status`` of a finished release while the source holds the session again (any entrance — here the row
    set back to ``locked``) answers a retryable refusal naming the state, so the caller starts the release again
    (which releases the current hold), never "released".

    Red before r4-a: ``DONE {'state': 'released'}``. Mutant ``SWEEP-release-poll-cached`` ⇒ the same."""
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    first = env.source.start_release(job, "cancelled")
    assert first.state is OpState.DONE
    env.ledger.source_set(job.job_id, "locked")
    st = env.source.op_status(job, first.op_id)
    assert st.state is OpState.FAILED and st.error["details"]["state"] == "locked", st
    assert st.error["retryable"] is True


# -- prepare per hold ----------------------------------------------------------------------------------------------


def test_r4_a_a_prepare_of_an_earlier_hold_is_no_answer(mk):  # noqa: F811
    """★ Prepared, then released (the conductor's abort), then locked again (a re-lock of a ``released`` row starts a
    new hold): the same prepare request (an unchanged session re-exports to the same ids) is no "ok" — the primary
    lock of the first hold was undone, the source is only ``locked``.

    Red before r4-a: ``DONE {'outcome': 'ok'}`` with the ledger ``locked``. Mutant ``SWEEP-prepare-no-hold`` (digest
    without the hold) ⇒ the same."""
    env = mk(remote_driven=False)
    job, plan, prepared, first = _to_prepared(env)
    assert env.source.start_release(job, "commit_failed").result["state"] == "released"
    env.source.lock(job, env.ref, plan.revision)
    assert env.ledger.source_state(job.job_id)["state"] == "locked"
    st = env.source.start_prepare(job, prepared)
    assert st.op_id != first.op_id
    assert not (st.state is OpState.DONE and (st.result or {}).get("outcome") == "ok"), st
    assert env.ledger.source_state(job.job_id)["state"] != "prepared"


# -- heartbeat -----------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("state", ["releasing", "reclaiming"])
def test_r4_a_a_heartbeat_names_a_release_under_way(mk, state):  # noqa: F811
    """★ A release or reclaim under way has given the leases up: ``heartbeat`` says so (``LeaseLost(state=…)``, the
    states route #9 names too) instead of returning as if the lease were held.

    Red before r4-a: no exception. Mutant ``SWEEP-heartbeat-silent`` ⇒ ``DID NOT RAISE``."""
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    env.ledger.source_set(job.job_id, state)
    with pytest.raises(LeaseLost) as caught:
        env.source.heartbeat(job)
    assert caught.value.details.get("state") == state


# -- reclaim --force of a handover in progress -----------------------------------------------------------------


def test_r4_a_a_forced_cancel_does_not_release_a_source_that_became_prepared(mk, session_db, monkeypatch):  # noqa: F811
    """★ ``/handover reclaim --force`` read ``locked`` (a handover in progress ⇒ "Handover cancelled; the session is
    yours again"), the prepare finished before the release: the source is ``prepared`` and its target may commit. The
    reclaim refuses (the next reclaim takes the ``prepared`` branch with its warning) instead of releasing it.

    Red before r4-a: ``state released`` (no error). Mutant ``SWEEP-reclaim-any-state`` ⇒ the same."""
    env = mk(remote_driven=False)
    job, *_ = _to_prepared(env)
    real = env.ledger.source_by_session

    def stale(session_id):
        entry = real(session_id)
        return {**entry, "state": "locked"} if entry else entry

    monkeypatch.setattr(env.ledger, "source_by_session", stale)
    with pytest.raises(InvalidState):
        lock.reclaim(env.db, env.ledger, env.sid, force=True)
    assert env.ledger.source_state(job.job_id)["state"] == "prepared"
    monkeypatch.setattr(env.ledger, "source_by_session", real)
    out = lock.reclaim(env.db, env.ledger, env.sid, force=True)
    assert out["state"] == "reclaimed" and any("may already be active" in w for w in out["warnings"]), out


def test_r4_a_a_forced_cancel_still_releases_a_locked_source(mk):  # noqa: F811
    """Counter-check: the cancel branch still gives a locked source back."""
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    out = lock.reclaim(env.db, env.ledger, env.sid, force=True)
    assert out["state"] == "released" and env.ledger.source_state(job.job_id)["state"] == "released"


# -- conductor -----------------------------------------------------------------------------------------------------


def test_r4_a_a_not_found_after_the_lock_is_no_release(tmp_path, clock):
    """★ The job asked the source to lock (``source.lock`` journaled); the abort's release request answers
    ``not_found`` (the source's rows of that peer are gone: a new pairing of the device, a lost ledger). That is no
    confirmation that the session there is usable: the job stays ``aborting`` (V37), unlike L3-F11's source that
    never opened the job (``test_g3_a.py::test_abort_ends_when_the_source_never_opened_the_job``).

    Red before r4-a: ``'aborted'``. Mutant ``SWEEP-notfound-released`` ⇒ the same."""
    h = Harness(tmp_path, clock, mode=Mode.PULL)
    h.world.inject("target", "start_prepare", "error", error=ImportFailed("no", retryable=False))
    h.world.inject("source", "start_release", "error", error=NotFound(f"job {h.job_id} not found"), times=10_000)
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert "source.lock" in [e["data"].get("step") for e in h.events("intent")]
    assert h.state() == "aborting", h.states()


def test_r4_a_a_resolve_in_doubt_names_the_state_it_reached(tmp_path, clock):
    """★ ``resolve_in_doubt`` of a job whose target aborted: the job is ``aborting`` (target abort and source release
    still to come), and that is what it returns — not ``"aborted"``.

    Red before r4-a: ``'aborted'`` while the ledger says ``aborting``."""
    h = Harness(tmp_path, clock)
    h.world.target.commit_fail = ImportFailed("disk full", hermes_error="OSError")

    def cut(world):
        world.inject("target", "op_status", "disconnect", times=10_000)
        world.inject("target", "status", "disconnect", times=10_000)

    h.world.inject("target", "start_commit", "action_after", action=cut)
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == "in_doubt", h.states()  # parked after the stall timeout
    h.world.faults.clear()
    h.stale()
    out = h.conductor().resolve_in_doubt()
    assert h.state() == "aborting"
    assert out == "aborting"
    assert "start_release" not in h.calls("source")


# -- gc ------------------------------------------------------------------------------------------------------------


def test_r4_a_a_gc_keeps_a_job_whose_source_still_holds_the_session(tmp_path, clock):
    """★ A source job row ended ``released`` (the sweep, before any lock) while a lock inserted its handover row
    (the race R3-H1 names; the process died before the row followed): ``gc --job`` sees the hold and refuses.

    Red before r4-a: no error (journal and ops of a held source purged)."""
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    job_id = ulid()
    led.create_job(job_id, Role.SOURCE, Mode.PULL, "P1", {"role": "source"})
    epoch = led.claim_owner(job_id, Role.SOURCE, "me", 60)
    led.transition(job_id, Role.SOURCE, "*", "released", {"reason": "expired"}, epoch=epoch)
    info = SessionInfo(
        tip_id="s1",
        root_id="s1",
        lineage_ids=("s1",),
        title="W",
        model=None,
        provider=None,
        cwd=None,
        git_repo_root=None,
        git_branch=None,
        active_messages=1,
        revision=(("s1",), 1, 1, 0),
    )
    led.source_lock(job_id, info, "pid=1:x", None)
    led.source_set(job_id, "locked")
    with pytest.raises(InvalidState) as caught:
        jobs.gc_job(led, job_id, data_dir=tmp_path / "data")
    assert caught.value.details.get("state") == "locked"
    assert led.job(job_id, Role.SOURCE) is not None
    led.close()


def test_r4_a_a_gc_still_removes_a_finished_job(tmp_path, clock):
    """Counter-check: a released source handover with ended rows is gc'd as before."""
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    job_id = ulid()
    led.create_job(job_id, Role.SOURCE, Mode.PULL, "P1", {"role": "source"})
    epoch = led.claim_owner(job_id, Role.SOURCE, "me", 60)
    led.transition(job_id, Role.SOURCE, "*", "released", {}, epoch=epoch)
    jobs.gc_job(led, job_id, data_dir=tmp_path / "data")
    assert led.job(job_id, Role.SOURCE) is None
    led.close()
