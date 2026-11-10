"""Review round 4 — R3-H1 (REVIEW-3 §3): a release that finished for one hold of the
source — or for no hold at all — is no answer for a later hold of the same job (the B1 class, contract V37).

Scenario of the reviewer: a pull opens the source at FACTS (route #7) and never locks it; the conductor stays quiet for
longer than ``staging_ttl`` (a CLI confirmation that waits on ``input()`` while the M3 keepalive keeps the conductor
alive). The sweep ends the row with ``start_release(job, "expired")`` (R2-M1) — a finished ``release`` operation for
hold 0. The conductor comes back and locks: ``source_lock`` inserts a new ``source_handovers`` row (no re-lock, no
``relock`` event, generation 0). Export, pack and prepare run. The abort then gets the sweep's cached operation:
``done {'state': 'released'}`` while the ledger stays ``prepared`` and the session is archived, hidden and ended
``handed_over``.

The rule (B1, V37): "released" only from a fresh, explicitly positive release of the hold that exists now.
Fixes (``engine/local_source.py``): the release belongs to "no hold yet" or to hold N (``_release_hold``); a stored
release never answers a hold that is active now (``start_release`` checks the ledger after the start); a source job
that ended before it ever locked refuses a late ``lock`` (the conductor then aborts with a source that holds nothing).

Red before the fix: ``assert 'prepared' == 'released'`` (reviewer scenario and the race), no ``InvalidState`` on the
late lock, a cached ``done`` while the ledger says ``locked``."""

from __future__ import annotations

import pytest

from ctx_handover.engine.errors import InvalidState
from ctx_handover.engine.model import AuthContext, AuthScheme, OpState, Options, PreparedInfo, Right, Role
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.unit.A.test_l2_source_real import mk  # noqa: F401  (fixture)

TTL_S = Options().staging_ttl_s


def _auth(peer) -> AuthContext:
    return AuthContext(scheme=AuthScheme.PEER_SIGNATURE, peer=peer, rights=frozenset({Right.PULL}), epoch=1)


def _prepared(capsule: dict) -> PreparedInfo:
    return PreparedInfo("n", "n", "T", capsule["h_all"], capsule["h_rows"], capsule["h_model"], "A", (), (), ())


def _visible(session_db, sid: str) -> bool:
    row = session_db.get_session(sid) or {}
    return not row.get("archived") and not row.get("hidden") and row.get("end_reason") != "handed_over"


def _expired_by_the_sweep(env, clock):
    """Route #7 row of a pull (FACTS), plan done, then no request for ``staging_ttl``: the registry's sweep ends it."""
    env.ledger.upsert_peer(env.peer)
    reg = ParticipantRegistry(env.ledger, lambda role, peer: env.source, clock=clock)
    job = env.job()
    plan = env.plan(job)
    clock.advance(TTL_S + 1)
    reg.sweep(env.ledger.now())
    assert env.ledger.job(job.job_id, Role.SOURCE)["state"] == "released"
    assert env.ledger.source_state(job.job_id) is None
    return reg, job, plan


def test_r4_a_h1_reviewer_scenario_ends_with_the_source_released(mk, clock, session_db):  # noqa: F811
    """★ The reviewer's chain: sweep release → late lock → export, pack, prepare (when the lock is taken) → abort.
    Whatever the late lock does, the abort's answer agrees with the ledger: the source holds nothing (no hold, or the
    hold ``released``) and the session is usable — never ``released`` over a ``prepared`` source.

    Mutant ``R3H1-all`` (the three layers removed: the release keyed by ``source_generation`` alone as before the fix,
    no check of a stored release against the ledger, no refusal of the late lock) ⇒ the source stays ``prepared``.
    Each layer alone keeps this test green."""
    env = mk(remote_driven=True)
    reg, job, plan = _expired_by_the_sweep(env, clock)
    api, ctx = reg.bind(_auth(env.peer), job.job_id, Role.SOURCE)  # route #12 source/lock of the returning conductor
    try:
        api.lock(ctx, env.ref, plan.revision)
    except InvalidState:
        pass
    else:
        capsule = env.source.start_export(job).result
        env.source.start_pack(job, env.final(plan))
        assert env.source.start_prepare(job, _prepared(capsule)).result["outcome"] == "ok"

    st = env.source.start_release(job, "commit_failed")  # the conductor's abort

    assert st.state is OpState.DONE and st.result["state"] == "released", st
    src = env.ledger.source_state(job.job_id)
    assert src is None or src["state"] == "released", src
    assert _visible(session_db, env.sid), session_db.get_session(env.sid)


def test_r4_a_h1_a_source_ended_before_it_locked_refuses_a_late_lock(mk, clock, session_db):  # noqa: F811
    """★ The row the sweep ended stays ended: a conductor that comes back after ``staging_ttl`` gets ``InvalidState``
    on ``lock`` (it aborts; the source never held the session) instead of a session archived for a job whose target
    staging expired as well.

    Mutant ``R3H1-no-ended-check`` (``LocalSource.lock`` without the check) ⇒ ``DID NOT RAISE``."""
    env = mk(remote_driven=True)
    reg, job, plan = _expired_by_the_sweep(env, clock)
    api, ctx = reg.bind(_auth(env.peer), job.job_id, Role.SOURCE)
    with pytest.raises(InvalidState) as caught:
        api.lock(ctx, env.ref, plan.revision)
    assert caught.value.details.get("state") == "released"
    assert env.ledger.source_state(job.job_id) is None
    assert env.ledger.job(job.job_id, Role.SOURCE)["state"] == "released"
    assert _visible(session_db, env.sid)
    st = env.source.start_release(job, "aborted")  # the abort that follows: nothing held, nothing to undo
    assert st.state is OpState.DONE and st.result["state"] == "released"


def test_r4_a_h1_release_of_no_hold_does_not_answer_a_hold_that_began_after_it(mk, clock, session_db):  # noqa: F811
    """★ The race behind the reviewer's chain: the late lock passed its checks, then the sweep's release ended the row
    (no hold yet), then the lock inserted its ``source_handovers`` row. The abort after the prepare releases that hold.

    Mutant ``R3H1-hold-count+no-fresh`` (the release keyed by ``source_generation`` alone and no check of a stored
    release against the ledger; either layer alone covers the race) ⇒ ``'prepared' == 'released'``."""
    env = mk(remote_driven=True)
    env.ledger.upsert_peer(env.peer)
    job = env.job()
    plan = env.plan(job)
    real_lock = env.ledger.source_lock
    raced = []

    def sweep_first(job_id, info, holder, peer):
        if not raced:
            raced.append(env.source.start_release(job, "expired"))  # the sweep, between the checks and the insert
        return real_lock(job_id, info, holder, peer)

    env.ledger.source_lock = sweep_first
    env.source.lock(job, env.ref, plan.revision)
    env.ledger.source_lock = real_lock
    assert raced and raced[0].state is OpState.DONE and raced[0].result["state"] == "released"
    capsule = env.source.start_export(job).result
    env.source.start_pack(job, env.final(plan))
    assert env.source.start_prepare(job, _prepared(capsule)).result["outcome"] == "ok"
    assert env.ledger.source_state(job.job_id)["state"] == "prepared"

    st = env.source.start_release(job, "commit_failed")

    assert st.state is OpState.DONE and st.result["state"] == "released", st
    assert env.ledger.source_state(job.job_id)["state"] == "released"
    assert _visible(session_db, env.sid), session_db.get_session(env.sid)
    assert [r for r in env.ledger.veto_rows() if r["job_id"] == job.job_id] == []


def test_r4_a_h1_a_stored_release_never_answers_an_active_hold(mk, clock, session_db):  # noqa: F811
    """★ The class guard: whatever made a hold active again after a release finished (an entrance not yet known —
    here the ledger row is set back to ``locked`` without a re-lock), ``start_release`` does not return the stored
    ``released``; it releases the hold that is active now.

    Mutant ``R3H1-no-fresh-check`` (``start_release`` returns the stored operation as is) ⇒ the same op id."""
    env = mk(remote_driven=False)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    first = env.source.start_release(job, "cancelled")
    assert first.state is OpState.DONE and env.ledger.source_state(job.job_id)["state"] == "released"
    env.ledger.source_set(job.job_id, "locked")  # an unknown entrance: active again, no relock event

    st = env.source.start_release(job, "cancelled")

    assert st.op_id != first.op_id
    assert env.ledger.source_state(job.job_id)["state"] == "released"
    assert st.state is OpState.DONE and st.result["state"] == "released", st
    assert _visible(session_db, env.sid)


def test_r4_a_h1_a_repeated_release_of_one_hold_stays_one_operation(mk, clock):  # noqa: F811
    """Counter-check: a repeated request within one hold — and within "no hold" — gets the same operation (route #10
    and the conductor may both ask)."""
    env = mk(remote_driven=True)
    _reg, job, _plan = _expired_by_the_sweep(env, clock)
    swept = [o for o in env.ledger.ops_for_job(job.job_id) if o["op"] == "release"]
    again = env.source.start_release(job, "aborted")
    assert len(swept) == 1 and again.op_id == swept[0]["op_id"]
    local = mk(remote_driven=False)
    ljob = local.job()
    lplan = local.plan(ljob)
    local.source.lock(ljob, local.ref, lplan.revision)
    one = local.source.start_release(ljob, "cancelled")
    two = local.source.start_release(ljob, "cancelled")
    assert one.op_id == two.op_id and two.state is OpState.DONE


def test_r4_a_h1_a_lock_inside_the_release_of_no_hold_is_released_too(mk, clock, session_db):  # noqa: F811
    """★ The other order of the race: the sweep's release read "no hold", the late lock then ran to the end, and only
    then the release ended the job row ``released`` — the row said "ended" next to a source that holds the session
    (``gc --job`` would purge it, the sweep no longer sees it). The release of nothing re-reads the ledger after its
    transition: the job row follows the hold again, its answer names the hold's state, and ``start_release`` releases
    that hold (the sweep's job is over) — the session is usable and nothing says "released" before it is.

    Red before the fix: the source stays ``locked`` under a ``released`` job row. Mutant ``R3H1-no-recheck`` (the
    no-hold branch returns "released" without the re-read) ⇒ the job row stays ``released`` next to ``locked``."""
    env = mk(remote_driven=True)
    env.ledger.upsert_peer(env.peer)
    job = env.job()
    plan = env.plan(job)
    real_transition = env.ledger.transition
    raced = []

    def lock_first(job_id, role, expect, to, event, **kw):
        if to == "released" and not raced:
            raced.append(True)
            env.source.lock(job, env.ref, plan.revision)  # the late conductor's whole lock, before this write
        return real_transition(job_id, role, expect, to, event, **kw)

    env.ledger.transition = lock_first
    st = env.source.start_release(job, "expired")  # the sweep
    env.ledger.transition = real_transition
    assert raced
    first = next(o for o in env.ledger.ops_for_job(job.job_id) if o["op"] == "release")
    assert first["result"] == {"state": "locked"}, first
    assert st.state is OpState.DONE and st.result["state"] == "released", st
    assert env.ledger.source_state(job.job_id)["state"] == "released"
    assert env.ledger.job(job.job_id, Role.SOURCE)["state"] == "released"
    assert _visible(session_db, env.sid)
    assert session_db.try_acquire_session_turn_lease(env.sid, "pid=9:turn", ttl_seconds=60, patience_s=0)
    session_db.release_session_turn_lease(env.sid, "pid=9:turn")
