"""First review round, finding H6 (the first external review §2): the janitor of any process released the local source
of a running push/export as soon as its participant row had seen no ``LocalSource._epoch`` call for 60 s — between
pack and prepare (a large work tree on the target) that is the normal case, and the handover broke off.

Two sides of the fix, each pinned on its own: the conductor's lease refresh (``LocalSource.heartbeat``) keeps the
source row's owner heartbeat fresh, and the janitor leaves a local source row alone while the conductor row of the
same job is alive. A source whose conductor is gone is still released (the janitor's job, DESIGN §3.6)."""

from __future__ import annotations

from ctx_handover.engine import jobs
from ctx_handover.engine.ledger import process_owner
from ctx_handover.engine.model import OpState, PreparedInfo, Role
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.unit.A.test_l2_source_real import mk  # noqa: F401  (fixture)


def _prepared(capsule: dict) -> PreparedInfo:
    return PreparedInfo("n", "n", "T", capsule["h_all"], capsule["h_rows"], capsule["h_model"], "A", (), (), ())


def _packed(env):
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    capsule = env.source.start_export(job).result
    env.source.start_pack(job, env.final(plan))
    return job, capsule


def _janitor(env, clock) -> dict:
    reg = ParticipantRegistry(env.ledger, lambda role, peer: env.source, clock=clock)
    return jobs.janitor(env.ledger, None, reg, flush_outbox=None, background=False)


def test_rv_a_h6_lease_refresh_keeps_the_source_row_alive(mk, clock):  # noqa: F811
    """The conductor refreshes the leases every 15–30 s (``Conductor._lease_beat``); 61 s without another
    ``_epoch`` call must not make the row stale."""
    env = mk(remote_driven=False)
    job, capsule = _packed(env)
    for _ in range(4):
        clock.advance(15.25)
        env.source.heartbeat(job)

    did = _janitor(env, clock)

    assert did["participant"] == [], did
    assert env.ledger.source_state(job.job_id)["state"] == "locked"
    env.source.heartbeat(job)  # no LeaseLost
    st = env.source.start_prepare(job, _prepared(capsule))
    assert st.state is OpState.DONE and st.result["outcome"] == "ok", st
    assert env.ledger.source_state(job.job_id)["state"] == "prepared"


def test_rv_a_h6_janitor_keeps_the_local_source_of_a_live_conductor(mk, clock):  # noqa: F811
    """Even a source row that missed its heartbeat (no lease refresh for a while) stays held while the conductor
    row of the same job is alive: the conductor decides about its source, not a janitor of another process."""
    env = mk(remote_driven=False)
    job, _capsule = _packed(env)
    owner = "4242:feedc0de"  # the conductor runs in another process of this profile (unstamped: no liveness probe)
    assert env.ledger.claim_owner(job.job_id, Role.CONDUCTOR, owner, 60) is not None
    clock.advance(61)
    env.ledger.heartbeat(job.job_id, Role.CONDUCTOR, owner, env.ledger.job(job.job_id, Role.CONDUCTOR)["owner_epoch"])
    assert [r["role"] for r in env.ledger.stale_jobs(60)] == ["source"]

    did = _janitor(env, clock)

    assert did["participant"] == [(job.job_id, "source", "kept")], did
    assert env.ledger.source_state(job.job_id)["state"] == "locked"


def test_rv_a_h6_janitor_still_releases_the_source_of_a_gone_conductor(mk, clock):  # noqa: F811
    """Counter-check: conductor row stale (its process ended) and source row stale ⇒ released, as before."""
    env = mk(remote_driven=False)
    job, _capsule = _packed(env)
    epoch = env.ledger.claim_owner(job.job_id, Role.CONDUCTOR, "4242:feedc0de", 60)
    assert epoch is not None
    clock.advance(61)

    did = _janitor(env, clock)

    assert did["participant"] == [(job.job_id, "source", "released")], did
    assert env.ledger.source_state(job.job_id)["state"] == "released"


def test_rv_a_h6_heartbeat_never_takes_a_row_another_process_owns(mk, clock):  # noqa: F811
    """The lease refresh only renews this process's own claim: a row a live other process owns keeps its owner
    and epoch (no adoption through a heartbeat)."""
    env = mk(remote_driven=False)
    job, _capsule = _packed(env)
    row = env.ledger.job(job.job_id, Role.SOURCE)
    assert row["owner"] == process_owner()
    clock.advance(61)
    other = env.ledger.claim_owner(job.job_id, Role.SOURCE, "4242:feedc0de", 60)
    assert other is not None
    clock.advance(5)  # the other owner is alive (well inside its stale window)
    env.source.heartbeat(job)
    after = env.ledger.job(job.job_id, Role.SOURCE)
    assert (after["owner"], after["owner_epoch"]) == ("4242:feedc0de", other)
