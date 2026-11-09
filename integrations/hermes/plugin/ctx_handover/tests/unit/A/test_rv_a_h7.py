"""Review round rv-a, finding H7 (``review-code/REVIEW.md`` §2): the listener's maintenance sweep suspended the local
source of a running push/export after ``participant_idle_timeout`` (15 min) — ``updated_at`` of that row moves only
with transitions, never between lock and prepare, so a transfer or target prepare of more than 15 min broke off.

The idle rule of DESIGN §7.3 is about a *remote* conductor that went quiet; this host's own conductor keeps its
source alive itself (lease refresh, owner heartbeat) and the janitor handles its death. A remote-driven source that
nobody contacts is still suspended."""

from __future__ import annotations

from ctx_handover.engine.errors import HandoverError
from ctx_handover.engine.model import Role
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.unit.A.test_l2_source_real import mk  # noqa: F401  (fixture)


def _packed(env):
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    env.source.start_export(job)
    env.source.start_pack(job, env.final(plan))
    return job


def test_rv_a_h7_sweep_leaves_the_local_source_of_a_running_push_alone(mk, clock):  # noqa: F811
    env = mk(remote_driven=False)
    job = _packed(env)
    assert env.ledger.job(job.job_id, Role.SOURCE)["params"].get("local") is True
    reg = ParticipantRegistry(env.ledger, lambda role, peer: env.source, clock=clock)
    for i in range(32):  # 16 minutes of transfer / target prepare; the conductor refreshes the leases every 30 s
        clock.advance(30)
        try:
            env.source.heartbeat(job)
        except HandoverError as exc:  # pragma: no cover - the failure this test pins
            raise AssertionError(f"after {(i + 1) * 30} s the lease refresh fails: {exc.message}") from exc
        reg.sweep(env.ledger.now())

    assert env.ledger.source_state(job.job_id)["state"] == "locked"
    env.source.heartbeat(job)


def test_rv_a_h7_sweep_still_suspends_an_idle_remote_driven_source(mk, clock):  # noqa: F811
    """Counter-check: a pull whose remote conductor went quiet for longer than the idle timeout gives up the leases
    (DESIGN §7.3) — the restriction to remote rows must not switch that off."""
    env = mk(remote_driven=True)
    job = _packed(env)
    assert env.ledger.job(job.job_id, Role.SOURCE)["params"].get("local") is not True
    reg = ParticipantRegistry(env.ledger, lambda role, peer: env.source, clock=clock)
    clock.advance(16 * 60)

    reg.sweep(env.ledger.now())

    assert env.ledger.source_state(job.job_id)["state"] == "suspended"
