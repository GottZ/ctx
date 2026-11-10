"""R6-B: what the receiving side stages is bounded by this host's own size limits, not by the sizes the other side
declares in its plan (review of PR #190, §3 "the staging quota is set from the peer's declared ``size_estimate``
without a cap against ``max_total_gb``").

``negotiate`` set the job's quota to the plan's need + 10 % (R28); the need comes from the items' ``size_estimate``
and the plan's ``approx_tokens`` — both written by the counterpart. A paired device declaring a terabyte on a host
with that much free space could stage a terabyte of blocks before PREPARE's own checks ran. Now:

* ``max_total_gb`` per handover is checked at negotiation (DESIGN §9.4, §10.2): the confirmed items' declared sizes
  plus the capsule estimate against ``max_total_bytes + max_session_bytes`` — a larger handover is refused before a
  byte moves (also an honest one, e.g. a full git history over the limit: a clear refusal instead of a quota error in
  the middle of the transfer, Fremd-Review R6 finding 1);
* a git item this host already has commits of travels as an incremental pack of unknown size and is not counted
  there; the staging quota stays capped at ``(max_total_bytes + max_session_bytes) × 1.1`` for it."""

from __future__ import annotations

import dataclasses

import pytest

from ctx_handover.engine.errors import PayloadTooLarge
from ctx_handover.engine.local_target import QUOTA_MARGIN
from ctx_handover.engine.materialize import Assessment
from ctx_handover.engine.model import DestMode, ItemKind, Role
from ctx_handover.tests.fakes.apis_fake import make_facts
from ctx_handover.tests.unit.A.test_local_target import Env

TIB = 1024**4
GIB = 1024**3


def _env(tmp_path, clock, session_db, make_session, user_home, hermes_home, **kw) -> Env:
    e = Env(tmp_path, clock, session_db, make_session, user_home, hermes_home, free=10 * 1024 * TIB, **kw)
    e.target.open_job(e.job)
    e.target.facts(e.job, make_facts("server"))
    return e


def test_declared_terabyte_is_refused_at_negotiation(
    tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host
):
    """★ Plan item declared at 1 TiB, free space 10 PiB: refused at negotiation (``payload_too_large`` naming the
    setting), no staging quota at all.

    Red proof (base ``cab3a492``): ``negotiate`` accepts and sets a quota of ``need × 1.1`` ≈ 73 TiB."""
    e = _env(tmp_path, clock, session_db, make_session, user_home, hermes_home)
    with pytest.raises(PayloadTooLarge) as exc:
        e.target.negotiate(e.job, e.plan(size=TIB))
    assert exc.value.details["setting"] == "max_total_gb"
    assert exc.value.details["max_bytes"] == e.options.max_total_bytes + e.options.max_session_bytes
    assert e.ledger.quota_get(e.job.job_id, Role.TARGET) is None
    assert e.ledger.job(e.job.job_id, Role.TARGET)["state"] == "open"
    e.ledger.close()


def test_declared_tokens_count_against_the_limit(
    tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host
):
    """★ The session's declared size (``approx_tokens``) counts too: 10^12 tokens ⇒ refused."""
    e = _env(tmp_path, clock, session_db, make_session, user_home, hermes_home)
    with pytest.raises(PayloadTooLarge):
        e.target.negotiate(e.job, dataclasses.replace(e.plan(size=1000), approx_tokens=10**12))
    assert e.ledger.quota_get(e.job.job_id, Role.TARGET) is None
    e.ledger.close()


def test_incremental_git_item_is_not_counted_but_the_quota_is_capped(
    tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host
):
    """★ A git item this host has commits of (``git_have``) declared at 1 TiB: negotiation goes through (the pack is
    incremental, its size unknown here), the staging quota stays within this host's limits.

    Red proof (base ``cab3a492``): the quota is ``need × 1.1`` ≈ 3.3 TiB."""
    e = _env(tmp_path, clock, session_db, make_session, user_home, hermes_home)

    def assess(item, dest, **kw):
        return Assessment(DestMode.UPDATE, None, ("refs/heads/main " + "a" * 40,), 3 * item.size_estimate, ())

    e.target.ports.materialize.assess = assess
    item = dataclasses.replace(e.plan().items[0], kind=ItemKind.GIT_REPO, size_estimate=TIB)
    plan = dataclasses.replace(e.plan(), items=(item,))
    e.target.negotiate(e.job, plan)
    quota, used = e.ledger.quota_get(e.job.job_id, Role.TARGET)
    limit = int((e.options.max_total_bytes + e.options.max_session_bytes) * QUOTA_MARGIN)
    assert used == 0
    assert quota <= limit, (quota, limit)
    e.ledger.close()


@pytest.mark.parametrize("size", [1000, 9 * GIB])
def test_plan_within_the_limits_is_accepted(
    tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host, size
):
    """A plan below the limits is accepted; its quota is the need of R28 + 10 %, the need capped at the limits (the
    need counts a tree twice at the destination, so 9 GiB of files need 18 GiB there, of which at most the limit is
    staged)."""
    e = _env(tmp_path, clock, session_db, make_session, user_home, hermes_home)
    final = e.target.negotiate(e.job, e.plan(size=size))
    quota, _used = e.ledger.quota_get(e.job.job_id, Role.TARGET)
    limit = e.options.max_total_bytes + e.options.max_session_bytes
    assert quota == int(min(final.need_bytes, limit) * QUOTA_MARGIN)
    e.ledger.close()
