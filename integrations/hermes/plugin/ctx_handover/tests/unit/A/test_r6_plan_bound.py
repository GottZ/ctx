"""R6-B: plan work a paired device starts on this host's source is bounded (review of PR #190, §3 "a paired peer can
start unbounded plan/refine/facts work").

``source/plan`` (route #12) starts an operation per distinct input (``target_facts``, session reference, hints): a
worker thread that reads the whole lineage, scans the working directory and — with ``refine`` on — asks this host's
model. Route #7's job caps (``_check_caps``) bound the jobs, not the plans inside one job, and the per-device rate
limit (20/s) lets a device start a new plan with each request. The bound now: at most ``MAX_REMOTE_PLANS`` plan
operations per job, and at most ``MAX_REMOTE_PLANS_RUNNING`` running at once for one device across its jobs (a
running plan of a cancelled job keeps counting until it ends). A repeat of the same request is no new plan.
This host's own jobs (push, export, offer) are not bounded."""

from __future__ import annotations

import dataclasses

import pytest

from ctx_handover.engine import local_source
from ctx_handover.engine.errors import HandoverError, RateLimited
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Mode, OpState, Role
from ctx_handover.tests.fakes.apis_fake import make_facts
from ctx_handover.tests.unit.A.test_local_source import SrcEnv

MAX_PLANS = local_source.MAX_REMOTE_PLANS
MAX_RUNNING = local_source.MAX_REMOTE_PLANS_RUNNING


@pytest.fixture
def mk(tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host):
    made = []

    def make(remote_driven=True):
        e = SrcEnv(
            tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host, remote_driven=remote_driven
        )
        made.append(e)
        return e

    yield make
    for e in made:
        e.ledger.close()


def _facts(i: int):
    return dataclasses.replace(make_facts("laptop"), hostname=f"laptop-{i}")


def _plans(e) -> list[dict]:
    return [o for o in e.ledger.ops_for_job(e.job.job_id) if o["op"] == "plan"]


def test_a_device_cannot_start_plan_after_plan_in_one_job(mk):
    """★ 20 plan requests with varied facts in one job: at most ``MAX_REMOTE_PLANS`` operations, then refused.

    Red proof (base ``cab3a492``): every request with new facts starts (and runs) a new plan — 20 operations."""
    e = mk(remote_driven=True)
    refused = 0
    for i in range(20):
        try:
            st = e.source.start_plan(e.job, _facts(i), e.ref)
            assert st.state is OpState.DONE, st.error
        except HandoverError:
            refused += 1
    assert len(_plans(e)) <= MAX_PLANS
    assert refused == 20 - len(_plans(e))


def test_the_same_request_again_is_no_new_plan(mk):
    """A repeated request (the conductor's retry) answers with the existing operation, also beyond the bound."""
    e = mk(remote_driven=True)
    first = e.source.start_plan(e.job, _facts(0), e.ref)
    for i in range(1, MAX_PLANS):
        e.source.start_plan(e.job, _facts(i), e.ref)
    again = e.source.start_plan(e.job, _facts(0), e.ref)
    assert again.op_id == first.op_id
    assert len(_plans(e)) == MAX_PLANS


def test_running_plans_of_a_device_are_bounded_across_its_jobs(mk):
    """★ While ``MAX_REMOTE_PLANS_RUNNING`` plans of the device run here (in other jobs, e.g. ones it cancelled), a
    new plan of that device is refused for now (``rate_limited``, retryable: the conductor's client waits and asks
    again) — no thread per request."""
    e = mk(remote_driven=True)
    for _ in range(MAX_RUNNING):
        other = ulid()
        e.ledger.create_job(other, Role.SOURCE, Mode.PULL, "PL", {"role": "source"})
        assert e.ledger.op_insert(other, "plan", "d" * 64, ulid())  # a running plan of another job of PL
    with pytest.raises(RateLimited) as exc:
        e.source.start_plan(e.job, _facts(0), e.ref)
    assert exc.value.retryable is True
    assert _plans(e) == []


def test_own_jobs_are_not_bounded(mk):
    """This host's own source (push, export, the planning of an offer) plans as often as its conductor asks."""
    e = mk(remote_driven=False)
    for i in range(MAX_PLANS + 3):
        assert e.source.start_plan(e.job, _facts(i), e.ref).state is OpState.DONE


def test_the_running_bound_follows_the_devices_job_cap(mk):
    """With ``max_jobs_per_peer`` raised (the composition root passes it as ``remote_plan_slots``) every job the device
    may have here can plan at the same time (Fremd-Review R6 finding 5)."""
    e = mk(remote_driven=True)
    e.source.remote_plan_slots = MAX_RUNNING + 1
    for _ in range(MAX_RUNNING):
        other = ulid()
        e.ledger.create_job(other, Role.SOURCE, Mode.PULL, "PL", {"role": "source"})
        assert e.ledger.op_insert(other, "plan", "d" * 64, ulid())
    assert e.source.start_plan(e.job, _facts(0), e.ref).state is OpState.DONE
