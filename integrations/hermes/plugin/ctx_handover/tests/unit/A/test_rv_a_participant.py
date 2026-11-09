"""Review round rv-a, participant findings (``review-code/REVIEW.md`` §2): M17 (a pull peer opened a source job under
another peer's job id and read that job's objects) and L4 (the janitor keeps an interrupted ``reclaiming`` row; a
repeated ``/handover reclaim`` finishes it — the docstring said otherwise)."""

from __future__ import annotations

from ctx_handover.engine import jsonutil
from ctx_handover.engine.blocks import BLOCK_SIZE, decode_frames
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Right, Role, SessionInfo
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.unit.C.test_server import Listener, body_of


def _open(lst, dialer, job_id, role, mode, name):
    body = {"job_id": job_id, "epoch": 1, "role": role, "mode": mode, "conductor": {"name": name}, "options": {}}
    return lst.handle(dialer.req("POST", "jobs", body))


def test_rv_a_m17_a_job_id_of_another_peer_is_not_reopened_in_another_role(tmp_path):
    """Peer P pushes (target job X, an object registered with it); peer Q opens a *source* job with the same id X —
    refused, and Q reads neither the manifest nor the blocks of P's object."""
    lst = Listener(tmp_path, real_registry=True)
    p = lst.pair(tmp_path, name="pusher", rights=frozenset({Right.PUSH}))
    q = lst.pair(tmp_path, name="puller", rights=frozenset({Right.PULL}))
    secret = b"transcript of the pusher's session " * 100
    m = lst.registry.store.put_bytes(secret, "application/json")
    x = ulid()
    assert _open(lst, p, x, "target", "push", "p").status == 200
    put = {"epoch": 1, "manifests": [jsonutil.to_wire(m)]}
    assert lst.handle(p.req("POST", f"jobs/{x}/objects/put", put, epoch=1)).status == 200

    resp = _open(lst, q, x, "source", "pull", "q")

    assert resp.status == 409, (resp.status, body_of(resp))
    assert body_of(resp)["error"]["code"] == "job_conflict"
    listed = lst.handle(q.req("POST", f"jobs/{x}/objects/list", {"epoch": 1, "object_ids": [m.object_id]}, epoch=1))
    assert listed.status != 200 or not body_of(listed).get("manifests")
    got = lst.handle(q.req("POST", f"jobs/{x}/blocks/get", {"epoch": 1, "ids": list(m.blocks)}, epoch=1))
    raw = b"".join(r for _i, r, _x in decode_frames(got.body, block_size=BLOCK_SIZE) if r) if got.status == 200 else b""
    assert secret not in raw


def test_rv_a_m17_the_id_of_this_hosts_own_conductor_job_is_refused(tmp_path):
    """A job id this host conducts (its own push to P) cannot be opened by a peer as a participant job."""
    lst = Listener(tmp_path, real_registry=True)
    q = lst.pair(tmp_path, name="puller", rights=frozenset({Right.PULL}))
    x = ulid()
    lst.ledger.create_job(x, Role.CONDUCTOR, Mode.PUSH, None, {"mode": "push"})
    resp = _open(lst, q, x, "source", "pull", "q")
    assert resp.status == 409, (resp.status, body_of(resp))


def _auth(peer, rights):
    from ctx_handover.engine.model import AuthContext, AuthScheme

    return AuthContext(scheme=AuthScheme.PEER_SIGNATURE, peer=peer, rights=frozenset(rights), epoch=1)


def test_rv_a_m17_registry_binds_a_job_id_to_one_peer_across_roles(tmp_path, clock):
    """The registry owns the binding rules (DESIGN §4.5, contract X18), not only the route: a job id another peer
    uses in any role, or one of this host's own conductor jobs, is refused for every caller of ``open``."""
    import pytest

    from ctx_handover.engine.errors import JobConflict
    from ctx_handover.engine.model import Peer

    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    p = Peer(name="pusher", peer_id=ulid(), direction="inbound", url=None, pubkey="pk", rights=frozenset({Right.PUSH}))
    q = Peer(name="puller", peer_id=ulid(), direction="inbound", url=None, pubkey="pk", rights=frozenset({Right.PULL}))
    for peer in (p, q):
        ledger.upsert_peer(peer)
    reg = ParticipantRegistry(ledger, lambda role, peer: None, clock=clock)
    x = ulid()
    ledger.create_job(x, Role.TARGET, Mode.PUSH, p.peer_id, {"role": "target"})
    body = {"job_id": x, "role": "source", "mode": "pull", "epoch": 1}
    with pytest.raises(JobConflict):
        reg.open(_auth(q, {Right.PULL}), body)
    own = ulid()
    ledger.create_job(own, Role.CONDUCTOR, Mode.PUSH, q.peer_id, {"mode": "push"})
    with pytest.raises(JobConflict):
        reg.open(_auth(q, {Right.PULL}), {**body, "job_id": own})
    assert ledger.job(x, Role.SOURCE) is None and ledger.job(own, Role.SOURCE) is None
    ledger.close()


def test_rv_a_m17_the_same_peer_reopens_its_own_job(tmp_path):
    """Counter-check: route #7 stays idempotent for the peer that owns the job."""
    lst = Listener(tmp_path, real_registry=True)
    q = lst.pair(tmp_path, name="puller", rights=frozenset({Right.PULL}))
    x = ulid()
    assert _open(lst, q, x, "source", "pull", "q").status == 200
    assert _open(lst, q, x, "source", "pull", "q").status == 200


def test_rv_a_l4_janitor_keeps_reclaiming_and_says_so(tmp_path, clock):
    """``recover`` keeps an interrupted ``reclaiming`` row (a repeated ``/handover reclaim`` finishes it); the docstring
    names exactly that."""
    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    job = ulid()
    ledger.create_job(job, Role.SOURCE, Mode.PUSH, None, {"role": "source", "local": True})
    info = SessionInfo("t", "r", ("r", "t"), "T", None, None, None, None, None, 1, (1,))
    ledger.source_lock(job, info, "holder", None)
    ledger.source_set(job, "reclaiming")
    called = []
    reg = ParticipantRegistry(ledger, lambda role, peer: called.append(role))
    assert reg.recover(ledger.job(job, Role.SOURCE)) == "kept"
    assert ledger.source_state(job)["state"] == "reclaiming"
    doc = ParticipantRegistry.recover.__doc__ or ""
    assert "reclaiming" in doc and "repeated" in doc and "continue in their direction" not in doc
    ledger.close()
