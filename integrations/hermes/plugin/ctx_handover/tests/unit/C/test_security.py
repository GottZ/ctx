"""Security tests of cluster C per threat (TESTPLAN §5: T1, T2, T5, T10, T12; hardening #4, #5)."""

from __future__ import annotations

import json
import time

from ctx_handover.engine import pairing
from ctx_handover.engine.crypto import IDENTITY_ENV, random_b64
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Peer, Right
from ctx_handover.engine.server import Request
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.ledger_stub import make_ledger
from ctx_handover.tests.unit.C.test_server import ALL, NEEDS_REAL_LEDGER, Dialer, Listener, body_of


def test_sec_revoked_peer_rejected_immediately(tmp_path):
    """T1: after ``peers revoke`` the very next request of that peer is refused (no cache)."""
    lst = Listener(tmp_path)
    keep = lst.pair(tmp_path, "other")  # keeps inbound active
    d = lst.pair(tmp_path, "laptop")
    assert lst.handle(d.req("GET", "hello")).status == 200
    lst.ledger.revoke_peer("laptop")
    assert lst.handle(d.req("GET", "hello")).status == 401
    assert lst.handle(keep.req("GET", "hello")).status == 200


def test_sec_pair_revoke_route_aborts_jobs(tmp_path):
    """Hardening #4 (SEC 1.6): a peer revoking itself ends its participant jobs; its key stops working at once."""
    lst = Listener(tmp_path)
    lst.pair(tmp_path, "other")
    d = lst.pair(tmp_path, "laptop")
    job = ulid()
    body = {"job_id": job, "epoch": 1, "role": "target", "mode": "push", "conductor": {}, "options": {}}
    assert lst.handle(d.req("POST", "jobs", body)).status == 200
    resp = lst.handle(d.req("POST", "pair/revoke", {}))
    assert body_of(resp) == {"revoked": True}
    assert lst.registry.aborted == [((job,), "peer_revoked")]
    assert lst.handle(d.req("GET", "hello")).status == 401


def test_sec_listener_store_has_no_secrets(tmp_path):
    """T2: after pairing, the listener's ledger holds public keys only — no code, no secret, no private key."""
    env: dict[str, str] = {}
    ledger = make_ledger(tmp_path / "listener" / "ledger.sqlite3")
    identity = new_identity(ledger, env)
    dialer = new_identity(make_ledger(tmp_path / "dialer" / "ledger.sqlite3"))
    code = pairing.create_code(ledger, rights=ALL, name_hint="laptop", ttl_s=600)
    text = pairing.pair_string(
        code, url="https://s.example", fingerprint=identity.fingerprint(), name="s", profile=None
    )
    body = pairing.redeem_body(
        dialer,
        pairing.parse_pair_string(text),
        listener_instance_id=identity.instance_id,
        self_name="laptop",
        plugin_version="0.1.0",
    )
    pairing.redeem_listener(ledger, identity, body, "127.0.0.1")
    blob = b""
    for path in (tmp_path / "listener").iterdir():
        blob += path.read_bytes()
    assert code.secret.encode() not in blob
    assert env[IDENTITY_ENV].encode() not in blob
    assert body["proof"].encode() not in blob
    assert identity.public_b64().encode() in blob and dialer.public_b64().encode() in blob
    assert not ledger.pairing_pending(time.time())


def test_sec_cookie_only_request_rejected(tmp_path):
    """T5: a request that only rides the dashboard cookie (a cross-site page) has no Ed25519 headers ⇒ 401."""
    lst = Listener(tmp_path)
    lst.pair(tmp_path)
    for method, path, ctype, body in (
        ("POST", "sessions/list", "application/json", b'{"limit": 5}'),
        ("POST", f"jobs/{ulid()}/target/commit", "application/json", b'{"epoch": 1}'),
        ("GET", "hello", "", b""),
    ):
        headers = {"cookie": "hermes_session_at=valid-dashboard-session", "content-type": ctype}
        resp = lst.handle(Request(method, path, headers, body, "10.0.0.5"))
        if method == "GET":
            assert resp.status == 200 and "peer" not in body_of(resp)  # unsigned hello: public facts only
        else:
            assert resp.status == 401, path


def test_sec_simple_form_post_rejected(tmp_path):
    """T5: a CORS-simple form POST cannot carry the signature headers; even a signed one with a form body type is
    refused before its body is used."""
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    form = Request("POST", "sessions/list", {"content-type": "application/x-www-form-urlencoded"}, b"limit=5", None)
    assert lst.handle(form).status == 401
    signed_form = d.req("POST", "sessions/list", raw=b"limit=5", ctype="text/plain")
    resp = lst.handle(signed_form)
    assert resp.status == 400
    assert body_of(resp)["error"]["details"]["field"] == "content-type"
    redeem = Request(
        "POST",
        "pair/redeem",
        {"content-type": "text/plain", "x-ctxh-nonce": random_b64(16)},
        json.dumps({"code_id": "ABCDEF"}).encode(),
        None,
    )
    assert lst.handle(redeem).status == 400


def test_sec_signature_bound_to_listener_id(tmp_path):
    """T10: a request signed for listener A is refused by listener B even when the same dialer key is paired with
    both (the listener instance id is part of the signed string)."""
    a = Listener(tmp_path / "a")
    b = Listener(tmp_path / "b")
    d = a.pair(tmp_path / "a")
    b.ledger.upsert_peer(
        Peer(
            name="laptop",
            peer_id=d.peer_id,
            direction="inbound",
            url=None,
            pubkey=d.identity.public_b64(),
            rights=ALL,
            created_at=time.time(),
        )
    )
    req = d.req("GET", "hello")
    assert b.handle(req).status == 401
    assert a.handle(req).status == 200
    for_b = Dialer(d.identity, d.peer_id, b.identity.instance_id)
    assert b.handle(for_b.req("GET", "hello")).status == 200


@NEEDS_REAL_LEDGER
def test_sec_concurrent_job_cap(tmp_path):
    """T12: at most 2 open participant jobs per peer and 4 per listener; re-opening a known job is not new. The
    caps are the participant registry's (contract X18), so the listener runs with cluster A's registry here."""
    lst = Listener(tmp_path, real_registry=True)
    peers = [lst.pair(tmp_path, f"p{i}", rights={Right.PUSH, Right.PULL}) for i in range(3)]

    def open_job(d, job=None):
        job = job or ulid()
        body = {"job_id": job, "epoch": 1, "role": "target", "mode": "push", "conductor": {}, "options": {}}
        return job, lst.handle(d.req("POST", "jobs", body))

    first, r1 = open_job(peers[0])
    _, r2 = open_job(peers[0])
    _, r3 = open_job(peers[0])
    assert (r1.status, r2.status, r3.status) == (200, 200, 429)
    assert body_of(r3)["error"]["code"] == "rate_limited"
    assert open_job(peers[0], first)[1].status == 200  # idempotent re-open
    assert open_job(peers[1])[1].status == 200
    assert open_job(peers[1])[1].status == 200
    assert open_job(peers[2])[1].status == 429  # listener total of 4 reached


def test_sec_hello_never_reveals_peer_data_unsigned(tmp_path):
    """SEC 1.10: the unsigned hello names only protocol facts and the listener key."""
    lst = Listener(tmp_path)
    lst.pair(tmp_path)
    resp = lst.handle(Request("GET", "hello", {}, b"", None))
    assert set(body_of(resp)) == {"protocol", "majors", "minor", "instance_id", "listener_pubkey", "pairing_open"}


def test_sec_dial_and_inbound_same_key_distinct(tmp_path):
    """Hardening #5: the same public key pinned as a dial peer and paired inbound authenticates only through the
    inbound row (its own peer id)."""
    lst = Listener(tmp_path)
    inbound = lst.pair(tmp_path, "in")
    dial_id = ulid()
    lst.ledger.upsert_peer(
        Peer(
            name="out",
            peer_id=dial_id,
            direction="dial",
            url="https://x.example",
            pubkey=inbound.identity.public_b64(),
            rights=ALL,
            created_at=time.time(),
        )
    )
    as_dial = Dialer(inbound.identity, dial_id, lst.identity.instance_id)
    assert lst.handle(as_dial.req("GET", "hello")).status == 401
    assert lst.handle(inbound.req("GET", "hello")).status == 200
