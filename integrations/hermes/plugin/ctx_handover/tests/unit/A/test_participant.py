"""ParticipantRegistry: job binding, rights, epochs, offers, idle/TTL sweeps, revocation (cluster A;
TESTPLAN §2.2, BUILD-ORDER §6 items 4, 5)."""

from __future__ import annotations

import pytest

from ctx_handover.engine.errors import Forbidden, InvalidState, JobConflict, NotFound, RateLimited
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import (
    AuthContext,
    AuthScheme,
    Mode,
    Options,
    Peer,
    Right,
    Role,
    SessionInfo,
)
from ctx_handover.engine.participant import ParticipantRegistry, participant_options


def _job_id(tag: str) -> str:
    """A ULID-shaped job id for a short test tag (job ids are ULIDs everywhere, contract K-2)."""
    return ("0" + tag).ljust(26, "0")


J_J1, J_JS, J_JT, J_J9 = _job_id("J1"), _job_id("JS"), _job_id("JT"), _job_id("J9")
J_A1, J_A2, J_A3, J_B1, J_B2, J_C1 = (_job_id(t) for t in ("A1", "A2", "A3", "B1", "B2", "C1"))


class RecordingAPI:
    def __init__(self, role, peer, log):
        self.role, self.peer, self.log = role, peer, log

    def open_job(self, job):
        self.log.append(("open_job", self.role, job.job_id, job.epoch))

    def suspend(self, job):
        self.log.append(("suspend", job.job_id))

    def abort(self, job, reason):
        self.log.append(("abort", job.job_id, reason))

    def start_release(self, job, reason):
        self.log.append(("release", job.job_id, reason))

    def heartbeat(self, job):
        self.log.append(("heartbeat", job.job_id))

    def list_sessions(self, limit, query, include_archived):
        self.log.append(("list_sessions", limit, query, include_archived))
        return [{"id": "s1"}]

    def status(self, job):
        from ctx_handover.engine.model import ParticipantStatus

        return ParticipantStatus(state="open", step="", epoch=job.epoch, progress=None, ops={}, last_error=None)


def _peer(pid, rights=(Right.PUSH, Right.PULL)):
    return Peer(
        name=pid.lower(), peer_id=pid, direction="inbound", url=None, pubkey="pk" + pid, rights=frozenset(rights)
    )


def _auth(peer, epoch=1, rights=None):
    return AuthContext(
        scheme=AuthScheme.PEER_SIGNATURE,
        peer=peer,
        rights=frozenset(rights if rights is not None else peer.rights),
        epoch=epoch,
    )


@pytest.fixture
def env(tmp_path, clock):
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    log: list = []
    reg = ParticipantRegistry(led, lambda role, peer: RecordingAPI(role, peer, log), clock=clock)
    yield led, reg, log
    led.close()


def _open(reg, auth, job_id=J_J1, role="target", mode="push", epoch=1, **extra):
    body = {
        "job_id": job_id,
        "epoch": epoch,
        "role": role,
        "mode": mode,
        "conductor": {"instance_id": "C", "name": "laptop"},
        "options": {},
        **extra,
    }
    return reg.open(auth, body)


def test_job_role_requires_matching_right(env):
    """★ ``role=source`` needs ``pull``, ``role=target`` needs ``push``; later calls check the job's role (SEC 1.7).

    Mutant: ``open`` does not check the right for the role."""
    led, reg, _log = env
    push_only = _peer("P1", (Right.PUSH,))
    pull_only = _peer("P2", (Right.PULL,))
    with pytest.raises(Forbidden):
        _open(reg, _auth(push_only), job_id=J_JS, role="source", mode="pull")
    with pytest.raises(Forbidden):
        _open(reg, _auth(pull_only), job_id=J_JT, role="target", mode="push")
    assert led.job(J_JS, Role.SOURCE) is None and led.job(J_JT, Role.TARGET) is None
    assert _open(reg, _auth(push_only), job_id=J_JT)["state"] == "open"
    assert _open(reg, _auth(pull_only), job_id=J_JS, role="source", mode="pull")["job_id"] == J_JS
    with pytest.raises(NotFound):
        reg.get(_auth(push_only), J_JT, Role.SOURCE)  # a target job is no source job
    with pytest.raises(Forbidden):
        reg.get(_auth(push_only, rights=()), J_JT, Role.TARGET)  # right removed since


def test_participant_job_bound_to_peer(env):
    """★ Jobs are keyed by (peer_id, job_id): another peer gets ``not_found`` and cannot take the id (SEC 1.7).

    Mutant: ``get`` does not compare the job's peer with the caller."""
    led, reg, _log = env
    p1, p2 = _peer("P1"), _peer("P2")
    _open(reg, _auth(p1))
    assert reg.get(_auth(p1), J_J1, Role.TARGET).peer.peer_id == "P1"
    with pytest.raises(NotFound):
        reg.get(_auth(p2), J_J1, Role.TARGET)
    with pytest.raises(NotFound):
        reg.context(_auth(p2), J_J1, Role.TARGET)
    with pytest.raises(JobConflict):
        _open(reg, _auth(p2))
    assert led.job(J_J1, Role.TARGET)["peer_id"] == "P1"


def test_participant_rejects_stale_epoch(env):
    """★ A request with an epoch below the highest seen ⇒ ``invalid_state(stale_epoch)`` [SK M5].

    Mutant: ``get`` skips the epoch check."""
    _led, reg, _log = env
    p = _peer("P1")
    _open(reg, _auth(p, epoch=2), epoch=2)
    reg.get(_auth(p, epoch=3), J_J1, Role.TARGET)
    with pytest.raises(InvalidState) as exc:
        reg.get(_auth(p, epoch=2), J_J1, Role.TARGET)
    assert exc.value.details["reason"] == "stale_epoch"
    assert reg.context(_auth(p, epoch=3), J_J1, Role.TARGET).epoch == 3


def test_participant_open_idempotent_and_conductor_epoch(env):
    _led, reg, log = env
    p = _peer("P1")
    a = _open(reg, _auth(p))
    b = _open(reg, _auth(p))
    assert a == b
    assert [x for x in log if x[0] == "open_job"] == [("open_job", Role.TARGET, J_J1, 1)] * 2
    with pytest.raises(JobConflict):
        _open(reg, _auth(p), options={"dest": "/elsewhere"})


def test_participant_options_remote_cannot_widen():
    """Security settings stay local; a remote conductor can only narrow (R18, R19)."""
    local = Options(export_roots=(), allow_roots=("~/hermes-handover",), wait_timeout_s=600, refine="auto")
    remote = {
        "dest": "~/x",
        "export_roots": ["/"],
        "allow_roots": ["/"],
        "hints": {"include": ["/etc"], "include_secret": ["~/.ssh/id_ed25519"], "exclude": ["*.log"]},
        "wait": True,
        "wait_timeout_s": 99999,
        "max_total_bytes": 10**15,
        "continue_prompt": "x" * 5000,
        "refine": "llm",
    }
    o = participant_options(local, remote)
    assert o.export_roots == () and o.allow_roots == ("~/hermes-handover",)
    assert o.hints.include_secret == () and o.hints.include == () and o.hints.exclude == ("*.log",)
    assert o.wait is True and o.wait_timeout_s == 600 and o.max_total_bytes == local.max_total_bytes
    assert o.dest == "~/x" and len(o.continue_prompt) == 2000 and o.refine == "auto"
    assert participant_options(local, {"refine": "off"}).refine == "off"


def test_participant_concurrent_job_caps(env):
    _led, reg, _log = env
    p1, p2, p3 = _peer("P1"), _peer("P2"), _peer("P3")
    _open(reg, _auth(p1), job_id=J_A1)
    _open(reg, _auth(p1), job_id=J_A2)
    with pytest.raises(RateLimited):
        _open(reg, _auth(p1), job_id=J_A3)
    _open(reg, _auth(p2), job_id=J_B1)
    _open(reg, _auth(p2), job_id=J_B2)
    with pytest.raises(RateLimited):
        _open(reg, _auth(p3), job_id=J_C1)


def test_offer_claim_only_by_target_peer(env, clock):
    """★ Offers are listed and claimed only by their peer (SEC 1.7); claiming needs ``pull``.

    Mutant: ``Ledger.offers_for`` without the peer filter (every unexpired offer is listed)."""
    led, reg, _log = env
    p1, p2 = _peer("P1"), _peer("P2")
    led.offer_put("O1", "P1", "sess-1", "note", {"notes": "r"}, clock() + 7200)
    assert [o["offer_id"] for o in reg.offers(_auth(p1))["offers"]] == ["O1"]
    assert reg.offers(_auth(p2))["offers"] == []
    with pytest.raises(NotFound):
        reg.claim(_auth(p2), "O1", J_J9)
    with pytest.raises(Forbidden):
        reg.claim(_auth(_peer("P1", (Right.PUSH,))), "O1", J_J1)
    got = reg.claim(_auth(p1), "O1", J_J1)
    assert got["session_ref"]["id"] == "sess-1" and got["refinement"] == {"notes": "r"}
    assert reg.offers(_auth(p1))["offers"] == []


def test_participant_list_sessions_requires_pull_and_caps_limit(env):
    _led, reg, log = env
    with pytest.raises(Forbidden):
        reg.list_sessions(_auth(_peer("P1", (Right.PUSH,))), {"limit": 5})
    out = reg.list_sessions(_auth(_peer("P2", (Right.PULL,))), {"limit": 5000, "query": "x"})
    assert out == {"sessions": [{"id": "s1"}]}
    assert log[-1] == ("list_sessions", 200, "x", False)


def _source_job(led, job_id, peer_id, state, root):
    led.create_job(job_id, Role.SOURCE, Mode.PULL, peer_id, {"role": "source"})
    info = SessionInfo("t-" + root, root, (root, "t-" + root), None, None, None, None, None, None, 1, (1,))
    led.source_lock(job_id, info, "h", peer_id)
    led.source_set(job_id, state)
    led.transition(job_id, Role.SOURCE, "*", state, {}, epoch=0)


def test_participant_idle_timeout_releases_lease(env, clock):
    """No request for ``participant_idle_timeout`` ⇒ the source gives up its leases (``suspended``, §7.3)."""
    led, reg, log = env
    _source_job(led, "01JZS100000000000000000000", "P1", "locked", "r1")
    _source_job(led, "01JZS200000000000000000000", "P1", "locked", "r2")
    _source_job(led, "01JZS300000000000000000000", "P1", "prepared", "r3")
    clock.advance(10 * 60)
    reg.touch("01JZS200000000000000000000")
    clock.advance(5 * 60 + 1)
    reg.sweep(clock())
    assert ("suspend", "01JZS100000000000000000000") in log
    assert ("suspend", "01JZS200000000000000000000") not in log
    assert (
        "suspend",
        "01JZS300000000000000000000",
    ) not in log  # a prepared source never lets go on its own (fail-closed)


def test_participant_staging_ttl_expiry(env, clock):
    """A target job that did not commit within ``staging_ttl`` aborts itself and cleans up (§3.5, T12)."""
    led, reg, log = env
    for jid, state in (
        ("01JZT100000000000000000000", "prepared"),
        ("01JZT200000000000000000000", "committing"),
        ("01JZT300000000000000000000", "committed"),
        ("01JZT400000000000000000000", "data_staged"),
    ):
        led.create_job(jid, Role.TARGET, Mode.PUSH, "P1", {"role": "target"})
        led.transition(jid, Role.TARGET, "*", state, {}, epoch=0)
    clock.advance(86_400 + 1)
    reg.sweep(clock())
    aborted = sorted(x[1] for x in log if x[0] == "abort")
    assert aborted == ["01JZT100000000000000000000", "01JZT400000000000000000000"]
    assert all(x[2] == "expired" for x in log if x[0] == "abort")


def test_registry_abort_jobs_after_revoke(env):
    """After ``revoke_peer``: target jobs discard staging, locked sources are released, prepared sources stay
    (in-doubt rule, DESIGN §12.5)."""
    led, reg, log = env
    led.upsert_peer(_peer("P1"))
    led.create_job("01JZT100000000000000000000", Role.TARGET, Mode.PUSH, "P1", {"role": "target"})
    led.transition("01JZT100000000000000000000", Role.TARGET, "*", "prepared", {}, epoch=0)
    _source_job(led, "01JZS100000000000000000000", "P1", "locked", "r1")
    _source_job(led, "01JZS200000000000000000000", "P1", "prepared", "r2")
    ids = led.revoke_peer("P1")
    reg.abort_jobs(ids, "peer_revoked")
    assert ("abort", "01JZT100000000000000000000", "peer_revoked") in log
    assert ("release", "01JZS100000000000000000000", "peer_revoked") in log
    assert not any(x[1] == "01JZS200000000000000000000" for x in log if x[0] in ("abort", "release"))


def test_registry_heartbeat_cancel_status(env):
    led, reg, log = env
    p = _peer("P1")
    _open(reg, _auth(p), job_id=J_JS, role="source", mode="pull")
    out = reg.heartbeat(_auth(p), J_JS, {"epoch": 1, "conductor_state": "packing"})
    assert ("heartbeat", J_JS) in log and out["state"] == "open"
    _open(reg, _auth(p), job_id=J_JT, role="target", mode="push")
    reg.cancel(_auth(p), J_JT, {"epoch": 1, "reason": "user"})
    assert ("abort", J_JT, "cancelled: user") in log
    led.arrival_put(
        session_ids=["n1"],
        root_id="n1",
        job_id=J_JT,
        peer="p1",
        note="N",
        report_path="",
        path_map=[],
        continue_prompt=None,
    )
    st = reg.status(_auth(p), J_JT, {"epoch": 1, "note": "source_reclaimed"})
    assert st.state == "open"
    assert led.arrival_for("n1")["source_reclaimed_at"] is not None
