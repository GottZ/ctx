"""Review round 3 — R2-M1 (REVIEW-2 §3): a pull source that a conductor opened at FACTS and
never locked must not hold one of the peer's two slots for ever.

Scenario of the reviewer: ``take <peer>``; the source answers HELLO/FACTS, goes offline during PLANNING; the conductor's
abort tries ``source.release`` once (``_aborting``, review M2: never locked ⇒ not parked) and ends ``aborted``. On the
listener the source row stays ``open``: ``recover`` keeps it, ``sweep`` only looks at locked or aborting sources, ``gc``
refuses active jobs — and ``_check_caps`` counts it. After two such aborts every pull of that device is
``rate_limited``.

Now (listener side, ``participant.py``): a remote conductor's source job that never locked its session and had no
request for ``participant_idle_timeout`` no longer counts against the caps (it holds no session and no lease; a slow
confirmation of a living conductor still finds its job), and ``sweep`` ends it after ``staging_ttl`` like the target
staging of the same job (DESIGN §3.5).

Red before the fix: ``RateLimited: too many concurrent handovers for this device`` and no ``release`` in the sweep."""

from __future__ import annotations

import time

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.errors import RateLimited
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import AuthContext, AuthScheme, Mode, Options, Peer, Right, Role, SessionInfo
from ctx_handover.engine.participant import ParticipantRegistry

IDLE_S = Options().participant_idle_timeout_s
TTL_S = Options().staging_ttl_s


class _Api:
    def __init__(self, log):
        self.log = log

    def open_job(self, job):
        self.log.append(("open_job", job.job_id))

    def suspend(self, job):
        self.log.append(("suspend", job.job_id))

    def abort(self, job, reason):
        self.log.append(("abort", job.job_id, reason))

    def start_release(self, job, reason):
        self.log.append(("release", job.job_id, reason))


def _peer(pid: str) -> Peer:
    return Peer(
        name=pid.lower(),
        peer_id=pid,
        direction="inbound",
        url=None,
        pubkey="pk" + pid,
        rights=frozenset({Right.PUSH, Right.PULL}),
    )


def _auth(peer: Peer) -> AuthContext:
    return AuthContext(scheme=AuthScheme.PEER_SIGNATURE, peer=peer, rights=peer.rights, epoch=1)


def _pull(reg: ParticipantRegistry, peer: Peer, job_id: str | None = None) -> str:
    """Route #7 as a pulling conductor calls it at FACTS: a source job here, nothing locked."""
    job_id = job_id or ulid()
    body = {"job_id": job_id, "epoch": 1, "role": "source", "mode": "pull", "conductor": {"name": "c"}, "options": {}}
    reg.open(_auth(peer), body)
    return job_id


@pytest.fixture
def env(tmp_path, clock):
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    log: list = []
    reg = ParticipantRegistry(led, lambda role, peer: _Api(log), clock=clock)
    led.upsert_peer(_peer("P1"))
    yield led, reg, log
    led.close()


def test_r3_a_m1_an_unlocked_source_left_by_its_conductor_frees_the_slot(env, clock):
    """★ Two pulls opened at FACTS whose conductor aborted while the source was offline (nothing locked, no request
    since): after ``participant_idle_timeout`` the device can pull again.

    Mutant (``ParticipantRegistry._check_caps``): count every non-local source row ⇒ ``RateLimited``."""
    led, reg, _log = env
    p1 = _peer("P1")
    _pull(reg, p1)
    _pull(reg, p1)
    with pytest.raises(RateLimited):
        _pull(reg, p1)
    clock.advance(IDLE_S + 1)
    third = _pull(reg, p1)
    assert led.job(third, Role.SOURCE)["state"] == "open"


def test_r3_a_m1_a_slot_in_use_still_counts(env, clock):
    """Only idle rows without a lock are left out: a source the conductor still talks to counts, and so does a
    locked one (a remote conductor that went quiet is suspended by the sweep, not uncounted)."""
    led, reg, _log = env
    p1 = _peer("P1")
    talking = _pull(reg, p1)
    locked = _pull(reg, p1)
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
    led.source_lock(locked, info, "pid=1:x", "c")
    clock.advance(IDLE_S - 60)
    reg.touch(talking)
    clock.advance(120)
    with pytest.raises(RateLimited):
        _pull(reg, p1)


def test_r3_a_m1_sweep_ends_an_unlocked_source_after_the_staging_ttl(env, clock):
    """★ The row does not stay active for ever: after ``staging_ttl`` without a request the sweep releases it.

    Mutant (``ParticipantRegistry.sweep``): no branch for unlocked sources ⇒ no ``release``."""
    _led, reg, log = env
    p1 = _peer("P1")
    gone = _pull(reg, p1)
    clock.advance(TTL_S + 1)
    reg.sweep(clock())
    assert ("release", gone, "expired") in log


def test_r3_a_m1_a_slow_confirmation_keeps_its_source(env, clock):
    """A living conductor whose user reads the plan for longer than the idle timeout (the CLI has no limit) still
    finds its source job: nothing is released before ``staging_ttl``."""
    led, reg, log = env
    p1 = _peer("P1")
    slow = _pull(reg, p1)
    clock.advance(IDLE_S + 60)
    reg.sweep(clock())
    assert not [x for x in log if x[0] in ("release", "suspend")]
    assert led.job(slow, Role.SOURCE)["state"] == "open"


def test_r3_a_m1_this_hosts_own_source_rows_are_not_touched(env, clock):
    """The planning/source row of this host's own push or offer (``params.local``) is its conductor's business."""
    led, reg, log = env
    own = ulid()
    led.create_job(own, Role.SOURCE, Mode.PUSH, "P1", {"role": "source", "local": True})
    clock.advance(TTL_S + 1)
    reg.sweep(clock())
    assert not [x for x in log if x[1] == own]


def test_r3_a_m1_the_real_release_ends_the_row_and_frees_the_slot(fake_host, fake_ctx):
    """Production path: the profile's registry (``jobs._registry``) over the real ``LocalSource``. The sweep's release
    of an unlocked pull source ends its job row ``released``; the device's slots are free again."""
    jobs._reset_for_tests()
    try:
        rt = jobs.ensure_started(fake_ctx)
        reg = jobs._registry(rt)
        peer = _peer(ulid())
        rt.ledger.upsert_peer(peer)
        first, second = _pull(reg, peer), _pull(reg, peer)
        with pytest.raises(RateLimited):
            _pull(reg, peer)
        with rt.ledger.tx() as c:
            c.execute("UPDATE jobs SET updated_at=updated_at-? WHERE role='source'", (TTL_S + 60,))
        reg.sweep(rt.ledger.now())
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline and any(
            rt.ledger.job(j, Role.SOURCE)["state"] != "released" for j in (first, second)
        ):
            time.sleep(0.05)
        assert [rt.ledger.job(j, Role.SOURCE)["state"] for j in (first, second)] == ["released", "released"]
        assert _pull(reg, peer) and _pull(reg, peer)
    finally:
        jobs._reset_for_tests()
