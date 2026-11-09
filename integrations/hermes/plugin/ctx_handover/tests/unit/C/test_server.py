"""Listener request handling: inbound switch, authentication order, limits, routes (DESIGN §4.1–§4.7, §12.4)."""

from __future__ import annotations

import asyncio
import hashlib
import json
import time
from dataclasses import replace

from ctx_handover.engine import jsonutil
from ctx_handover.engine.blocks import BLOCK_SIZE, MEDIA_BLOCKS, decode_frames, encode_frame
from ctx_handover.engine.crypto import H_RESP_SIG, random_b64, response_string, sign_request, verify
from ctx_handover.engine.errors import HandoverError, InvalidState
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import (
    AuthScheme,
    CapsuleInfo,
    Peer,
    Right,
)
from ctx_handover.engine.server import (
    ROUTES,
    OperatorSessionAuth,
    PeerSignatureAuth,
    Request,
    Server,
)
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.helpers import FakeRegistry, sample_facts
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

ALL = frozenset({Right.PUSH, Right.PULL})


def run(coro):
    return asyncio.run(coro)


class Listener:
    def __init__(self, tmp_path, *, settings=None, operator=False, identity=True, redact=None) -> None:
        self.ledger = make_ledger(tmp_path / "listener" / "ledger.sqlite3")
        self.identity = new_identity(self.ledger) if identity else None
        self.registry = FakeRegistry(self.ledger)
        self.settings = {"name": "server", **(settings or {})}
        auths = [OperatorSessionAuth(enabled=operator)]
        if self.identity is not None:
            auths.append(PeerSignatureAuth(self.ledger, self.identity))
        self.server = Server(self.ledger, self.identity, self.registry, self.settings, auths, redact=redact)

    def pair(self, tmp_path, name="laptop", rights=ALL, direction="inbound") -> Dialer:
        led = make_ledger(tmp_path / f"d-{name}" / "ledger.sqlite3")
        ident = new_identity(led)
        peer = Peer(
            name=name,
            peer_id=ulid(),
            direction=direction,
            url=None,
            pubkey=ident.public_b64(),
            rights=frozenset(rights),
            created_at=time.time(),
        )
        self.ledger.upsert_peer(peer)
        return Dialer(ident, peer.peer_id, self.identity.instance_id if self.identity else "")

    def handle(self, req: Request):
        return run(self.server.handle(req))


class Dialer:
    def __init__(self, identity, peer_id: str, listener_id: str) -> None:
        self.identity, self.peer_id, self.listener_id = identity, peer_id, listener_id

    def req(
        self,
        method,
        route,
        body=None,
        *,
        epoch=None,
        ctype=None,
        raw=None,
        ts=None,
        nonce=None,
        listener_id=None,
        extra=None,
        drop=(),
    ):
        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else b"")
        headers = sign_request(
            self.identity,
            method=method,
            path="/v1/" + route,
            body=data,
            peer_id=self.peer_id,
            listener_id=listener_id or self.listener_id,
            epoch=epoch,
            ts=int(time.time()) if ts is None else ts,
            nonce=nonce or random_b64(16),
        )
        if ctype or method != "GET":
            headers["Content-Type"] = ctype or "application/json"
        headers.update(extra or {})
        out = {k.lower(): v for k, v in headers.items() if k.lower() not in {d.lower() for d in drop}}
        return Request(method=method, path=route, headers=out, body=data, client_ip="10.0.0.9")


def body_of(resp) -> dict:
    return json.loads(resp.body)


def signed_by(listener: Listener, req: Request, resp) -> bool:
    sig = resp.headers.get(H_RESP_SIG)
    if not sig:
        return False
    msg = response_string(resp.status, req.headers.get("x-ctxh-nonce", ""), hashlib.sha256(resp.body).hexdigest())
    return verify(listener.identity.public_b64(), msg, sig)


def unsigned_hello() -> Request:
    return Request(method="GET", path="hello", headers={"x-ctxh-nonce": random_b64(16)}, body=b"", client_ip=None)


# ---------------------------------------------------------------------------------------------------------
# Inbound switch and identity
# ---------------------------------------------------------------------------------------------------------


def test_server_inbound_auto_off_hides_hello(tmp_path):
    """★ No inbound peer and no pending code ⇒ even unsigned ``hello`` is a plain 404 (SEC 1.10, DESIGN §12.4).

    Red proof: mutant = ``hello`` answered before the inbound check in ``handle_stream`` → 200 instead of 404.
    """
    lst = Listener(tmp_path)
    resp = lst.handle(unsigned_hello())
    assert resp.status == 404
    assert body_of(resp) == {"detail": "Plugin not found"}
    assert H_RESP_SIG not in resp.headers  # indistinguishable from "plugin not installed"
    from ctx_handover.engine import pairing

    pairing.create_code(lst.ledger, rights=ALL, name_hint="", ttl_s=600)
    resp = lst.handle(unsigned_hello())
    assert resp.status == 200
    data = body_of(resp)
    assert set(data) == {"protocol", "majors", "minor", "instance_id", "listener_pubkey", "pairing_open"}
    assert data["pairing_open"] is True
    off = Listener(tmp_path / "off", settings={"inbound": "off"})
    off.pair(tmp_path / "off")
    assert off.handle(unsigned_hello()).status == 404
    on = Listener(tmp_path / "on", settings={"inbound": "on"})
    assert on.handle(unsigned_hello()).status == 200


def test_identity_create_only_in_operator_verbs(tmp_path, host):
    """★ Route paths never create a key: without identity every route is a plain 404 and nothing is written
    (R16, SEC 1.1).

    Red proof: mutant = ``Server.__init__`` creates the identity when ``identity is None``
    (``Identity.create(ledger, env_writable=…, …)``) → ``hello`` answers 200 and ``identity_pubkey`` exists.
    """
    lst = Listener(tmp_path, identity=False, settings={"inbound": "on"})
    for req in (unsigned_hello(), Request("POST", "pair/redeem", {"content-type": "application/json"}, b"{}", None)):
        resp = lst.handle(req)
        assert resp.status == 404
        assert body_of(resp) == {"detail": "Plugin not found"}
    assert lst.ledger.meta_get("identity_pubkey") is None
    assert not [c for c in host.calls if c[0] in {"save_env", "env_writable"}]


# ---------------------------------------------------------------------------------------------------------
# Authentication
# ---------------------------------------------------------------------------------------------------------


def test_signed_hello_full_and_response_signed(tmp_path):
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    req = d.req("GET", "hello")
    resp = lst.handle(req)
    assert resp.status == 200
    assert signed_by(lst, req, resp)
    data = body_of(resp)
    assert data["peer"] == {"peer_id": d.peer_id, "rights": ["pull", "push"]}
    assert data["limits"]["max_request_bytes"] == 8 * 1024**2
    assert data["limits"]["block_size"] == BLOCK_SIZE
    assert "operator.v1" not in data["capabilities"]
    assert {"session.v1", "blocks.v1", "enc.zlib"} <= set(data["capabilities"])
    assert data["instance_id"] == lst.identity.instance_id
    assert abs(data["server_time"] - time.time()) < 5


def test_server_auth_order_and_generic_errors(tmp_path):
    """★ Unknown peer, wrong key, missing signature and a body that differs from the signed hash all end in the
    same signed 401 ``unauthorized`` without details (DESIGN §4.2).

    Red proof: mutant = signature verification in ``PeerSignatureAuth.finish`` skipped → the request with the right
    peer id but a foreign key is answered 200.
    """
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    stranger = Dialer(new_identity(make_ledger(tmp_path / "x" / "l.sqlite3")), ulid(), lst.identity.instance_id)
    forged = Dialer(stranger.identity, d.peer_id, lst.identity.instance_id)  # right id, wrong key
    cases = [
        (stranger.req("GET", "hello"), 401),
        (forged.req("GET", "hello"), 401),
        (d.req("GET", "hello", drop=["x-ctxh-sig"]), 401),
        (replace(d.req("POST", "sessions/list", {"limit": 5}), body=b'{"limit": 6}'), 401),  # body ≠ signed hash
    ]
    for req, status in cases:
        resp = lst.handle(req)
        assert resp.status == status
        err = body_of(resp)["error"]
        assert err["code"] == "unauthorized"
        assert err["details"] == {}
        assert err["message"] == "unauthorized"
        assert signed_by(lst, req, resp)  # a plugin 401 is signed; only the gate's 401 is not


def test_header_formats_strict_before_nonce(tmp_path, monkeypatch):
    """Malformed nonce/time/signature/peer/epoch are refused before the nonce cache is touched (SEC 1.4)."""
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    seen = []
    real = lst.ledger.nonce_seen
    monkeypatch.setattr(lst.ledger, "nonce_seen", lambda *a, **k: seen.append(a) or real(*a, **k))
    bad_headers = [
        {"X-Ctxh-Nonce": "short"},
        {"X-Ctxh-Nonce": "A" * 21 + "="},
        {"X-Ctxh-Time": "12.5"},
        {"X-Ctxh-Time": "-5"},
        {"X-Ctxh-Sig": "A" * 85},
        {"X-Ctxh-Peer": "not-a-ulid"},
        {"X-Ctxh-Body-Sha256": "A" * 64},
        {"X-Ctxh-Epoch": "x"},
    ]
    for extra in bad_headers:
        resp = lst.handle(d.req("GET", "hello", extra=extra))
        assert resp.status == 401, extra
    assert seen == []
    assert lst.handle(d.req("GET", "hello")).status == 200
    assert len(seen) == 1


def test_dial_peer_cannot_authenticate_inbound(tmp_path):
    """★ A key pinned for a listener we dial never authenticates requests to us (SEC 1.7, hardening #5).

    Red proof: mutant = ``PeerSignatureAuth.precheck`` looks the peer up with ``ledger.peer(peer_id)`` instead of
    ``ledger.inbound_peer(peer_id)`` and drops the ``direction`` check → the dial peer's request is answered 200.
    """
    lst = Listener(tmp_path)
    lst.pair(tmp_path, name="real-inbound")  # inbound is active, so the refusal is the authenticator's
    d = lst.pair(tmp_path, name="dialed", direction="dial")
    assert lst.handle(d.req("GET", "hello")).status == 401
    assert lst.handle(d.req("POST", "sessions/list", {"limit": 1})).status == 401


def test_server_clock_skew(tmp_path):
    """★ ±300 s accepted, beyond refused (T10).

    Red proof: mutant = skew bound multiplied by 1000 → the request 310 s old is answered 200.
    """
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    now = int(time.time())
    assert lst.handle(d.req("GET", "hello", ts=now - 290)).status == 200
    assert lst.handle(d.req("GET", "hello", ts=now + 290)).status == 200
    assert lst.handle(d.req("GET", "hello", ts=now - 310)).status == 401
    assert lst.handle(d.req("GET", "hello", ts=now + 310)).status == 401


def test_server_replay_rejected(tmp_path):
    """★ The same signed request twice: the second is refused (T10).

    Red proof: mutant = nonce check removed from ``PeerSignatureAuth.finish`` → the replay is answered 200.
    """
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    req = d.req("POST", "sessions/list", {"limit": 3})
    assert lst.handle(req).status == 200
    replay = lst.handle(req)
    assert replay.status == 401
    assert body_of(replay)["error"]["code"] == "unauthorized"


def test_server_rights_enforced(tmp_path):
    lst = Listener(tmp_path)
    push_only = lst.pair(tmp_path, "pusher", rights={Right.PUSH})
    pull_only = lst.pair(tmp_path, "puller", rights={Right.PULL})
    job = ulid()
    open_src = {"job_id": job, "epoch": 1, "role": "source", "mode": "pull", "conductor": {}, "options": {}}
    open_tgt = dict(open_src, role="target", mode="push")
    assert lst.handle(push_only.req("POST", "sessions/list", {"limit": 1})).status == 403
    assert lst.handle(push_only.req("POST", "jobs", open_src)).status == 403
    assert lst.handle(pull_only.req("POST", "jobs", open_tgt)).status == 403
    assert lst.handle(pull_only.req("POST", "jobs", open_src)).status == 200
    resp = lst.handle(pull_only.req("POST", f"jobs/{job}/target/facts", {"epoch": 1, "source_facts": {}}, epoch=1))
    assert resp.status == 403
    # a push peer cannot reach the pull peer's job at all
    resp = lst.handle(push_only.req("POST", f"jobs/{job}/status", {"epoch": 1}, epoch=1))
    assert resp.status == 404


def test_server_rate_limit(tmp_path):
    lst = Listener(tmp_path, settings={"rate_per_s": 1, "rate_burst": 3})
    d = lst.pair(tmp_path)
    statuses = [lst.handle(d.req("GET", "hello")).status for _ in range(5)]
    assert statuses[:3] == [200, 200, 200]
    assert 429 in statuses[3:]
    resp = lst.handle(d.req("GET", "hello"))
    assert resp.status == 429
    assert 1 <= int(resp.headers["Retry-After"]) <= 300
    assert body_of(resp)["error"]["code"] == "rate_limited"


def test_server_body_limit_streaming(tmp_path):
    """★ An over-limit body is cut while streaming, before it is read to the end (I7, DESIGN §4.7).

    Red proof: mutant = ``_read_capped`` collects the whole stream before comparing with the cap → all 64 chunks
    are consumed (``consumed == 64``).
    """
    lst = Listener(tmp_path, settings={"max_request_mb": 1})
    d = lst.pair(tmp_path)
    consumed = 0

    async def chunks():
        nonlocal consumed
        for _ in range(64):
            consumed += 1
            yield b"\0" * (64 * 1024)

    req = d.req("PUT", f"jobs/{ulid()}/blocks", raw=b"", ctype=MEDIA_BLOCKS, epoch=1)
    resp = run(lst.server.handle_stream(req.method, req.path, req.headers, chunks(), req.client_ip))
    assert resp.status == 413
    assert body_of(resp)["error"]["code"] == "payload_too_large"
    assert consumed <= 17  # 1 MiB cap / 64 KiB chunks + the chunk that crossed it
    # JSON routes are capped at max_json_bytes even when the request cap is larger
    big = d.req("POST", "sessions/list", raw=b'{"limit": 1, "q": "' + b"x" * (5 * 1024**2) + b'"}')
    assert lst.handle(big).status == 413


def test_server_error_details_redacted_and_capped(tmp_path):
    lst = Listener(tmp_path, redact=lambda text: text.replace("sk-SECRET", "***"))
    d = lst.pair(tmp_path)
    job = ulid()
    body = {"job_id": job, "epoch": 1, "role": "target", "mode": "push", "conductor": {}, "options": {}}
    assert lst.handle(d.req("POST", "jobs", body)).status == 200
    lst.registry.raise_on["start_commit"] = HandoverError("boom sk-SECRET", path="x" * 5000, key="sk-SECRET")
    resp = lst.handle(d.req("POST", f"jobs/{job}/target/commit", {"epoch": 1}, epoch=1))
    assert resp.status == 500
    err = body_of(resp)["error"]
    assert "sk-SECRET" not in resp.body.decode()
    assert len(json.dumps(err["details"])) <= 1024
    lst.registry.raise_on["start_commit"] = RuntimeError("internal sk-SECRET traceback")
    resp = lst.handle(d.req("POST", f"jobs/{job}/target/commit", {"epoch": 1}, epoch=1))
    assert resp.status == 500
    assert body_of(resp)["error"] == {"code": "internal", "message": "internal error", "retryable": True, "details": {}}


def test_operator_session_disabled_by_default(tmp_path):
    """``X-Ctxh-Operator`` is ignored unless ``operator_api`` enables the scheme (R8, §17.5)."""
    lst = Listener(tmp_path)
    lst.pair(tmp_path)
    op = Request(
        "GET", "hello", {"x-ctxh-operator": "1", "authorization": "Bearer t", "x-ctxh-nonce": random_b64(16)}, b"", None
    )
    resp = lst.handle(op)
    assert resp.status == 401
    enabled = Listener(tmp_path / "op", operator=True, settings={"inbound": "on", "operator_api": True})
    resp = enabled.handle(op)
    assert resp.status == 200
    assert "operator.v1" in body_of(resp)["capabilities"]
    # cookie-only (no non-cookie credential) is refused even when enabled (SEC 1.12)
    cookie_only = Request("GET", "hello", {"x-ctxh-operator": "1", "cookie": "hermes_session_at=x"}, b"", None)
    assert enabled.handle(cookie_only).status == 401
    auth = OperatorSessionAuth(enabled=True).authenticate(op)
    assert auth.scheme == AuthScheme.OPERATOR_SESSION and auth.rights == ALL and auth.peer is None


# ---------------------------------------------------------------------------------------------------------
# Routes
# ---------------------------------------------------------------------------------------------------------


def test_routes_table_matches_design():
    assert len(ROUTES) == 32
    assert ROUTES[("PUT", "/v1/jobs/{id}/blocks")] == ("put_blocks", Right.PUSH)
    assert ROUTES[("POST", "/v1/jobs/{id}/source/pack")][1] == Right.PULL
    assert ROUTES[("POST", "/v1/jobs/{id}/target/commit")][1] == Right.PUSH
    assert ROUTES[("GET", "/v1/hello")][1] is None


def test_server_unknown_route_and_bad_path(tmp_path):
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    assert lst.handle(d.req("POST", "nope", {})).status == 404
    assert lst.handle(d.req("POST", "jobs/../x", {})).status == 400
    assert lst.handle(d.req("POST", "jobs/not-an-id/status", {"epoch": 1}, epoch=1)).status == 404
    assert lst.handle(d.req("GET", "jobs", {})).status == 404


def _open(lst, d, role="target", mode="push", epoch=1) -> str:
    job = ulid()
    body = {"job_id": job, "epoch": epoch, "role": role, "mode": mode, "conductor": {"name": "c"}, "options": {}}
    resp = lst.handle(d.req("POST", "jobs", body))
    assert resp.status == 200, resp.body
    return job


def test_server_job_routes_dispatch(tmp_path):
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    m = __import__("ctx_handover.engine.model", fromlist=["x"])
    tgt = _open(lst, d)
    facts = jsonutil.to_wire(sample_facts(m, "src"))
    resp = lst.handle(d.req("POST", f"jobs/{tgt}/target/facts", {"epoch": 1, "source_facts": facts}, epoch=1))
    assert resp.status == 200 and body_of(resp)["instance_name"] == "target-host"
    capsule = CapsuleInfo(
        capsule_id="c" * 64,
        size=1,
        segments=1,
        rows=1,
        max_segment_bytes=1,
        approx_tokens=1,
        h_all="a",
        h_rows="b",
        h_model="c",
        revision=(1, 2),
    )
    resp = lst.handle(
        d.req("POST", f"jobs/{tgt}/target/stage_context", {"epoch": 1, "capsule": jsonutil.to_wire(capsule)}, epoch=1)
    )
    assert body_of(resp)["op_id"] == "op-stage_context"
    call = next(c for c in lst.registry.target.calls if c[0] == "start_stage_context")
    assert call[1][1] == capsule
    assert lst.handle(d.req("POST", f"jobs/{tgt}/target/continue", {"epoch": 1, "prompt": "go"}, epoch=1)).status == 200
    resp = lst.handle(d.req("POST", f"jobs/{tgt}/target/continue", {"epoch": 1, "prompt": "x" * 2001}, epoch=1))
    assert resp.status == 400
    resp = lst.handle(d.req("POST", f"jobs/{tgt}/target/abort", {"epoch": 1, "reason": "user"}, epoch=1))
    assert body_of(resp) == {"state": "open"}
    resp = lst.handle(d.req("POST", f"jobs/{tgt}/status", {"epoch": 1}, epoch=1))
    assert body_of(resp)["state"] == "open"
    resp = lst.handle(d.req("POST", f"jobs/{tgt}/heartbeat", {"epoch": 1, "conductor_state": "x"}, epoch=1))
    assert body_of(resp)["state"] == "open"
    src = _open(lst, d, role="source", mode="pull")
    resp = lst.handle(
        d.req(
            "POST",
            f"jobs/{src}/source/plan",
            {"epoch": 1, "target_facts": facts, "hints": {}, "session_ref": {"latest": True}},
            epoch=1,
        )
    )
    assert body_of(resp)["op_id"] == "op-plan"
    call = next(c for c in lst.registry.source.calls if c[0] == "start_plan")
    assert call[1][2] == m.SessionRef(latest=True)
    resp = lst.handle(
        d.req(
            "POST",
            f"jobs/{src}/source/lock",
            {"epoch": 1, "session_ref": {"id": "s"}, "wait_s": 0, "plan_revision": [1, 2]},
            epoch=1,
        )
    )
    assert body_of(resp)["tip_id"] == "t"
    resp = lst.handle(d.req("POST", f"jobs/{src}/cancel", {"epoch": 1, "reason": "user"}, epoch=1))
    assert resp.status == 200
    assert tuple(c[0] for c in lst.registry.source.calls if c[0] == "start_release") == ("start_release",)


def test_server_epoch_header_must_match_body(tmp_path):
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    job = _open(lst, d, epoch=5)
    assert lst.handle(d.req("POST", f"jobs/{job}/status", {"epoch": 5}, epoch=4)).status == 400
    resp = lst.handle(d.req("POST", f"jobs/{job}/status", {"epoch": 4}, epoch=4))
    assert resp.status == 409
    assert body_of(resp)["error"]["details"]["reason"] == "stale_epoch"
    assert isinstance(InvalidState("x"), HandoverError)


def test_server_push_blocks_and_pull_scope(tmp_path):
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    store = lst.registry.store
    # push: manifests, need, frames
    raws = [hashlib.sha256(bytes([i])).digest() * 100 for i in range(3)]
    tmp = type(store)()
    m = tmp.put_bytes(b"".join(raws), "application/octet-stream")
    job = _open(lst, d)
    resp = lst.handle(
        d.req("POST", f"jobs/{job}/objects/put", {"epoch": 1, "manifests": [jsonutil.to_wire(m)]}, epoch=1)
    )
    assert body_of(resp) == {"stored": 1}
    resp = lst.handle(d.req("POST", f"jobs/{job}/blocks/need", {"epoch": 1, "ids": list(m.blocks)}, epoch=1))
    assert body_of(resp) == {"missing": list(m.blocks)}
    frames = b"".join(encode_frame(b, tmp.get(b), peer_caps=frozenset({"enc.zlib"})) for b in m.blocks)
    frames += encode_frame("0" * 64, b"x", peer_caps=frozenset())  # wrong id ⇒ rejected alone
    resp = lst.handle(d.req("PUT", f"jobs/{job}/blocks", raw=frames, ctype=MEDIA_BLOCKS, epoch=1))
    data = body_of(resp)
    assert data["stored"] == 1 and data["rejected"] == [{"id": "0" * 64, "reason": "block_corrupt"}]
    assert store.has_object(m.object_id)
    assert m.object_id in lst.ledger.job_objects(job)
    # pull: only the job's own objects and blocks leave the listener
    src = _open(lst, d, role="source", mode="pull")
    other = store.put_bytes(b"not part of the job", "application/octet-stream")
    lst.ledger.job_object(src, m.object_id)
    resp = lst.handle(d.req("POST", f"jobs/{src}/objects/list", {"epoch": 1, "object_ids": [m.object_id]}, epoch=1))
    assert [x["object_id"] for x in body_of(resp)["manifests"]] == [m.object_id]
    resp = lst.handle(d.req("POST", f"jobs/{src}/objects/list", {"epoch": 1, "object_ids": [other.object_id]}, epoch=1))
    assert resp.status == 404
    req = d.req("POST", f"jobs/{src}/blocks/get", {"epoch": 1, "ids": list(m.blocks), "accept": ["zlib"]}, epoch=1)
    resp = lst.handle(req)
    assert resp.headers["Content-Type"] == MEDIA_BLOCKS
    assert signed_by(lst, req, resp)
    got = list(decode_frames(resp.body, block_size=BLOCK_SIZE))
    assert [g[0] for g in got] == list(m.blocks) and all(g[2] is None for g in got)
    resp = lst.handle(d.req("POST", f"jobs/{src}/blocks/get", {"epoch": 1, "ids": list(other.blocks)}, epoch=1))
    assert resp.status == 404


def test_server_blocks_get_pages_by_size(tmp_path):
    lst = Listener(tmp_path, settings={"max_request_mb": 1})
    d = lst.pair(tmp_path)
    store = lst.registry.store
    data = b"".join(hashlib.sha256(str(i).encode()).digest() for i in range(4 * BLOCK_SIZE // 32))
    m = store.put_bytes(data, "application/octet-stream")
    src = _open(lst, d, role="source", mode="pull")
    lst.ledger.job_object(src, m.object_id)
    resp = lst.handle(d.req("POST", f"jobs/{src}/blocks/get", {"epoch": 1, "ids": list(m.blocks)}, epoch=1))
    got = list(decode_frames(resp.body, block_size=BLOCK_SIZE))
    assert len(resp.body) <= 1024**2
    assert 1 <= len(got) < len(m.blocks)
    assert [g[0] for g in got] == list(m.blocks[: len(got)])


def test_pair_redeem_route_and_failure_cap(tmp_path):
    from ctx_handover.engine import pairing

    lst = Listener(tmp_path, settings={"pair_failures_per_hour": 3})
    pairing.create_code(lst.ledger, rights=ALL, name_hint="", ttl_s=600)
    bad = {
        "code_id": "ABCDEF",
        "dialer": {"name": "x", "instance_id": ulid(), "pubkey": "A" * 43, "plugin_version": "1"},
        "client_nonce": "A" * 22,
        "proof": "A" * 43,
    }
    req = Request(
        "POST",
        "pair/redeem",
        {"content-type": "application/json", "x-ctxh-nonce": random_b64(16)},
        json.dumps(bad).encode(),
        "1.2.3.4",
    )
    codes = [body_of(lst.handle(req))["error"]["code"] for _ in range(4)]
    assert codes == ["pair_invalid", "pair_invalid", "pair_invalid", "rate_limited"]
    form = Request("POST", "pair/redeem", {"content-type": "application/x-www-form-urlencoded"}, b"a=b", None)
    assert lst.handle(form).status == 400
