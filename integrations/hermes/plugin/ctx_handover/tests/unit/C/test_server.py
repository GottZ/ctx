"""Listener request handling: inbound switch, authentication order, limits, routes (DESIGN §4.1–§4.7, §12.4)."""

from __future__ import annotations

import asyncio
import contextlib
import hashlib
import json
import os
import time
from dataclasses import replace

import pytest

from ctx_handover.engine import jsonutil
from ctx_handover.engine.blocks import BLOCK_SIZE, MEDIA_BLOCKS, BlockStore, decode_frames, encode_frame
from ctx_handover.engine.crypto import H_RESP_SIG, Identity, random_b64, response_string, sign_request, verify
from ctx_handover.engine.errors import HandoverError, IdentityLost, InvalidState
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import (
    AuthScheme,
    CapsuleInfo,
    Hello,
    Peer,
    Right,
    Role,
    SessionInfo,
)
from ctx_handover.engine.server import (
    ROUTES,
    OperatorSessionAuth,
    PeerSignatureAuth,
    Request,
    Server,
)
from ctx_handover.tests.fakes.blocks_fake import MemoryBlockStore
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.helpers import FakeParticipant, FakeRegistry, sample_facts
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

ALL = frozenset({Right.PUSH, Right.PULL})


def run(coro):
    return asyncio.run(coro)


NEEDS_REAL_LEDGER = pytest.mark.skipif(
    os.environ.get("CTXH_C_LEDGER") == "stub", reason="the participant registry needs cluster A's ledger"
)


class Listener:
    """``Server`` over the C ledger with the participant double (``FakeRegistry``) — or, with ``real_registry``,
    cluster A's ``ParticipantRegistry`` serving the double's participants. ``wrap_ledger`` wraps the ledger the
    server and its block store see; ``load_identity`` is the server's identity loader (``True``: this listener's
    in-memory ``.env``)."""

    def __init__(
        self,
        tmp_path,
        *,
        settings=None,
        operator=False,
        identity=True,
        redact=None,
        real_registry=False,
        wrap_ledger=None,
        load_identity=None,
    ) -> None:
        self.ledger = make_ledger(tmp_path / "listener" / "ledger.sqlite3")
        self.env: dict[str, str] = {}
        self.identity = new_identity(self.ledger, self.env) if identity else None
        seen = wrap_ledger(self.ledger) if wrap_ledger is not None else self.ledger
        self.seen_ledger = seen
        self.registry = FakeRegistry(seen)
        registry = self.registry
        if real_registry:
            from ctx_handover.engine.participant import ParticipantRegistry

            fake = self.registry
            registry = ParticipantRegistry(seen, lambda role, peer: fake.source if role is Role.SOURCE else fake.target)
        self.participants = registry
        self.settings = {"name": "server", **(settings or {})}
        auths = [OperatorSessionAuth(enabled=operator)]
        if self.identity is not None:
            auths.append(PeerSignatureAuth(seen, self.identity))
        if load_identity is True:
            env = self.env

            def load_identity():
                return Identity.load(self.ledger, read_secret=env.get, read_env_fresh=env.get)

        self.server = Server(
            seen, self.identity, registry, self.settings, auths, redact=redact, load_identity=load_identity
        )

    def rotate(self) -> Identity:
        """``identity rotate`` as another process of the profile does it (new key in the ``.env`` and the ledger)."""
        return Identity.create(
            self.ledger,
            env_writable=lambda name: None,
            save_env=self.env.__setitem__,
            read_env_fresh=self.env.get,
            rotate=True,
        )

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


def test_server_inbound_setting_as_loaded_from_yaml(tmp_path):
    """``hermes_yaml`` reads ``inbound: off`` as False and ``inbound: on`` as True (YAML 1.1); False and unknown
    values close inbound even with a paired peer, True opens it, None is the default ``auto`` (DESIGN §12.4).

    Red proofs: mutant = ``inbound_mode`` maps unknown values to ``auto`` (the layer-1 reading) → the paired listener
    with ``inbound: "closed"`` answers ``hello`` 200; mutant = without the boolean branch (False/True read as the
    strings ``"false"``/``"true"``) → ``inbound: True`` answers 404.
    """
    for value, status in ((False, 404), ("Off", 404), ("closed", 404), (True, 200), (None, 200), ("ON", 200)):
        lst = Listener(tmp_path / f"in-{value}", settings={"inbound": value})
        lst.pair(tmp_path / f"in-{value}")
        assert lst.handle(unsigned_hello()).status == status, value


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


def test_server_settings_as_the_composition_root_passes_them(tmp_path):
    """W055: ``jobs.server_settings`` hands over the plugin settings as loaded plus ``hosted``, ``plugin_version``
    and ``hermes_version``; the signed ``hello`` reports ``limits.hosted`` and ``name`` from them, the dialer's
    ``Hello`` parses both, and unrelated or empty plugin keys do not disturb the listener."""
    settings = {
        "inbound": "auto",
        "name": "gw-b",
        "max_request_mb": 2,
        "hosted": True,
        "plugin_version": "0.1.0",
        "hermes_version": "0.22.6",
        "listen_url": "https://b.example",
        "peer_gate": "session_token",
        "allow_roots": ["~/hermes-handover"],
        "max_total_gb": 10,
        "refine": "auto",
        "extra_ignore": [],
        "max_json_bytes": None,
        "capabilities": "not-a-list",
    }
    lst = Listener(tmp_path, settings=settings)
    d = lst.pair(tmp_path)
    req = d.req("GET", "hello")
    resp = lst.handle(req)
    assert resp.status == 200 and signed_by(lst, req, resp)
    data = body_of(resp)
    assert data["limits"]["hosted"] is True
    assert data["name"] == "gw-b"
    assert (data["plugin_version"], data["hermes_version"]) == ("0.1.0", "0.22.6")
    assert data["limits"]["max_request_bytes"] == 2 * 1024**2
    assert data["limits"]["max_json_bytes"] == 2 * 1024**2  # default 4 MiB, capped by the request size
    assert {"session.v1", "blocks.v1"} <= set(data["capabilities"])
    hello = jsonutil.from_wire(Hello, data)
    assert hello.limits.hosted is True and hello.name == "gw-b"
    plain = Listener(tmp_path / "plain", settings={"name": None, "max_request_mb": "x"})
    pd = plain.pair(tmp_path / "plain")
    data = body_of(plain.handle(pd.req("GET", "hello")))
    assert data["limits"]["hosted"] is False and data["name"] == ""
    assert data["limits"]["max_request_bytes"] == 8 * 1024**2


def test_server_auth_order_and_generic_errors(tmp_path):
    """★ Unknown peer, wrong key, missing signature and a body that differs from the signed hash all end in the
    same signed 401 ``unauthorized`` without details (DESIGN §4.2).

    Red proof: mutant = signature verification in ``PeerSignatureAuth.precheck`` skipped → the request with the right
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


class QuotaTarget(FakeParticipant):
    """Target double whose ``blocks_for(job)`` is cluster A's quota sink over the listener's real block store."""

    def blocks_for(self, job):
        from ctx_handover.engine.local_target import QuotaSink

        return QuotaSink(self.r.store, self.r.ledger, job.job_id)


def _noise(n: int, seed: int) -> bytes:
    out = bytearray()
    i = 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}:{i}".encode()).digest()
        i += 1
    return bytes(out[:n])


def test_server_job_quota_on_put_routes(tmp_path):
    """★ R28: ``objects/put`` (#23) and ``PUT blocks`` (#25) store through the job's sink (``blocks_for``): without
    a confirmed plan nothing is stored, and a peer that sends more than the job's quota gets 507
    ``insufficient_storage`` and no byte of that request lands in the store.

    Red proofs: mutant = ``_h_put_blocks`` writes through ``api.blocks()`` instead of ``self._sink(api, _job)`` →
    the block request without a confirmed plan is stored (200 instead of 409); mutant = ``_h_objects_put`` through
    ``api.blocks()`` → the manifest is stored without a quota row (200 instead of 409).
    """
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    store = BlockStore(tmp_path / "listener" / "data", lst.ledger)
    lst.registry.store = store
    lst.registry.target = QuotaTarget(lst.registry, "target")
    src = MemoryBlockStore()
    m = src.put_bytes(_noise(2 * BLOCK_SIZE + 1000, 1), "application/octet-stream")
    b0, b1, b2 = m.blocks
    job = _open(lst, d)

    def frames(*ids):
        return b"".join(encode_frame(b, src.get(b), peer_caps=frozenset({"enc.zlib"})) for b in ids)

    def put_blocks(*ids):
        return lst.handle(d.req("PUT", f"jobs/{job}/blocks", raw=frames(*ids), ctype=MEDIA_BLOCKS, epoch=1))

    def put_manifest(manifest):
        body = {"epoch": 1, "manifests": [jsonutil.to_wire(manifest)]}
        return lst.handle(d.req("POST", f"jobs/{job}/objects/put", body, epoch=1))

    # no confirmed plan for the job yet: nothing is stored
    resp = put_manifest(m)
    assert resp.status == 409 and body_of(resp)["error"]["details"]["reason"] == "no_quota"
    assert not store.manifest_path(m.object_id).exists()
    resp = put_blocks(b2)
    assert resp.status == 409 and store.missing([b2]) == [b2]
    # quota = this manifest + blocks 0 and 2
    manifest_cost = 256 + 70 * len(m.blocks)
    lst.ledger.quota_set(job, Role.TARGET, manifest_cost + BLOCK_SIZE + 1000)
    assert body_of(put_manifest(m)) == {"stored": 1}
    resp = put_blocks(b0, b1, b2)
    assert resp.status == 507, resp.body
    err = body_of(resp)["error"]
    assert err["code"] == "insufficient_storage" and err["details"]["reason"] == "job_quota"
    assert store.missing([b0, b1, b2]) == [b0, b1, b2]
    assert body_of(put_blocks(b0, b2)) == {"stored": 2, "rejected": []}
    resp = put_blocks(b1)
    assert resp.status == 507 and store.missing([b1]) == [b1]
    other = src.put_bytes(_noise(500, 2), "application/json")
    resp = put_manifest(other)
    assert resp.status == 507 and not store.manifest_path(other.object_id).exists()
    assert lst.ledger.quota_get(job, Role.TARGET) == (manifest_cost + BLOCK_SIZE + 1000,) * 2
    resp = lst.handle(d.req("POST", f"jobs/{job}/blocks/need", {"epoch": 1, "ids": list(m.blocks)}, epoch=1))
    assert body_of(resp) == {"missing": [b1]}


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


# ---------------------------------------------------------------------------------------------------------
# Layer 2: identity rotation, receiver registration, registry ownership (X18)
# ---------------------------------------------------------------------------------------------------------


def _signed_with(pubkey: str, req: Request, resp) -> bool:
    sig = resp.headers.get(H_RESP_SIG)
    msg = response_string(resp.status, req.headers.get("x-ctxh-nonce", ""), hashlib.sha256(resp.body).hexdigest())
    return bool(sig) and verify(pubkey, msg, sig)


def test_server_follows_identity_rotation(tmp_path):
    """Layer-2 finding: after ``identity rotate`` in another process of the profile the running listener answers the
    next request with the new key — ``hello`` names it, the response verifies with it and no longer with the old
    one. A rotated key this process cannot load closes the listener (plain 404) instead of signing with the
    retired key."""
    lst = Listener(tmp_path, load_identity=True)
    d = lst.pair(tmp_path)
    old = lst.identity.public_b64()
    req = d.req("GET", "hello")
    assert _signed_with(old, req, lst.handle(req))
    new = lst.rotate().public_b64()
    assert new != old
    req = d.req("GET", "hello")
    resp = lst.handle(req)
    assert resp.status == 200 and body_of(resp)["listener_pubkey"] == new
    assert _signed_with(new, req, resp) and not _signed_with(old, req, resp)
    assert body_of(lst.handle(unsigned_hello()))["listener_pubkey"] == new

    def lost():
        raise IdentityLost("gone", reason="missing")

    closed = Listener(tmp_path / "closed", load_identity=lost)
    cd = closed.pair(tmp_path / "closed")
    assert closed.handle(cd.req("GET", "hello")).status == 200
    closed.rotate()
    for req in (cd.req("GET", "hello"), unsigned_hello()):
        resp = closed.handle(req)
        assert (resp.status, resp.body) == (404, b'{"detail":"Plugin not found"}')
        assert H_RESP_SIG not in resp.headers


class _TxSpy:
    """Ledger wrapper recording in which outermost ``tx()`` each object write happens (None = outside)."""

    def __init__(self, inner) -> None:
        self._inner = inner
        self.depth = 0
        self.serial = 0
        self.log: list[tuple[str, int | None]] = []

    def __getattr__(self, name):
        attr = getattr(self._inner, name)
        if name not in {"object_put", "job_object", "job_objects_add"}:
            return attr

        def recorded(*args, **kwargs):
            self.log.append((name, self.serial if self.depth else None))
            return attr(*args, **kwargs)

        return recorded

    @contextlib.contextmanager
    def tx(self):
        if not self.depth:
            self.serial += 1
        self.depth += 1
        try:
            with self._inner.tx() as conn:
                yield conn
        finally:
            self.depth -= 1


def test_objects_put_stores_and_registers_in_one_transaction(tmp_path):
    """★ Befund d (receiver): route #23 stores the received manifests and registers them with the job in one
    ledger transaction, so a block GC never sees a received object that belongs to no job.

    Red proof: mutant = ``_h_objects_put`` without ``with self._ledger.tx():`` → ``job_object`` runs outside the
    transaction that stored the manifest (recorded ``None``).
    """
    lst = Listener(tmp_path, wrap_ledger=_TxSpy)
    spy = lst.seen_ledger
    lst.registry.store = BlockStore(tmp_path / "listener" / "data", spy)
    d = lst.pair(tmp_path)
    job = _open(lst, d)
    src = MemoryBlockStore()
    ms = [src.put_bytes(_noise(700 + i, i), "application/json") for i in range(3)]
    spy.log.clear()
    body = {"epoch": 1, "manifests": [jsonutil.to_wire(m) for m in ms]}
    assert body_of(lst.handle(d.req("POST", f"jobs/{job}/objects/put", body, epoch=1))) == {"stored": 3}
    puts = [s for name, s in spy.log if name == "object_put"]
    regs = [s for name, s in spy.log if name in {"job_object", "job_objects_add"}]
    assert len(puts) == 3 and regs
    assert len(set(puts + regs)) == 1 and None not in puts + regs
    assert set(lst.ledger.job_objects(job)) == {m.object_id for m in ms}


@NEEDS_REAL_LEDGER
def test_registry_owns_job_caps_and_routes(tmp_path):
    """★ X18 (W010/W011): the server keeps no job logic. The caps of route #7 (2 per peer, 4 per listener, with the
    registry's ``concurrent_jobs`` reason; a re-open is not a new job) and routes #8–#10 are the participant
    registry's — including its rule that a prepared source refuses ``cancel``.

    Red proofs: mutant = ``_h_job_cancel`` releases the source itself (``api.start_release`` via ``_participant``)
    → the prepared source answers 200 instead of 409; mutant = the layer-1 cap in ``_h_jobs_open`` (own
    ``RateLimited`` before ``registry.open``) → the refused job's error carries no ``reason``.
    """
    lst = Listener(tmp_path, real_registry=True)
    peers = [lst.pair(tmp_path, f"p{i}") for i in range(3)]

    def open_job(d, job=None, role="target", mode="push"):
        job = job or ulid()
        body = {"job_id": job, "epoch": 1, "role": role, "mode": mode, "conductor": {}, "options": {}}
        return job, lst.handle(d.req("POST", "jobs", body))

    src, r1 = open_job(peers[0], role="source", mode="pull")
    tgt, r2 = open_job(peers[0])
    _, r3 = open_job(peers[0])
    assert (r1.status, r2.status, r3.status) == (200, 200, 429)
    err = body_of(r3)["error"]
    assert err["code"] == "rate_limited" and err["details"]["reason"] == "concurrent_jobs"
    assert open_job(peers[0], tgt)[1].status == 200  # idempotent re-open
    assert open_job(peers[1])[1].status == 200 and open_job(peers[1])[1].status == 200
    assert body_of(open_job(peers[2])[1])["error"]["details"]["reason"] == "concurrent_jobs"  # 4 on the listener
    d = peers[0]
    resp = lst.handle(d.req("POST", f"jobs/{tgt}/status", {"epoch": 1}, epoch=1))
    assert resp.status == 200 and body_of(resp)["state"] == "open"
    resp = lst.handle(d.req("POST", f"jobs/{src}/heartbeat", {"epoch": 1, "conductor_state": "x"}, epoch=1))
    assert body_of(resp) == {"state": "open"}
    assert [c[0] for c in lst.registry.source.calls].count("heartbeat") == 1
    assert lst.handle(peers[1].req("POST", f"jobs/{src}/status", {"epoch": 1}, epoch=1)).status == 404  # other peer
    info = SessionInfo(
        tip_id="t",
        root_id="r",
        lineage_ids=("r", "t"),
        title="T",
        model=None,
        provider=None,
        cwd=None,
        git_repo_root=None,
        git_branch=None,
        active_messages=1,
        revision=(1, 1),
    )
    lst.ledger.source_lock(src, info, "test:1", d.peer_id)
    lst.ledger.source_set(src, "prepared")
    resp = lst.handle(d.req("POST", f"jobs/{src}/cancel", {"epoch": 1, "reason": "user"}, epoch=1))
    assert resp.status == 409 and body_of(resp)["error"]["details"]["state"] == "prepared"
    assert "start_release" not in [c[0] for c in lst.registry.source.calls]
    resp = lst.handle(d.req("POST", f"jobs/{tgt}/cancel", {"epoch": 1, "reason": "user"}, epoch=1))
    assert resp.status == 200
    assert [c[1][1] for c in lst.registry.target.calls if c[0] == "abort"] == ["cancelled: user"]


def test_server_flags_as_loaded_from_yaml(tmp_path):
    """YAML-bool sweep: ``operator_api`` and ``hosted`` are booleans whether YAML parsed them (``yes``/``off`` →
    True/False) or a loader handed over the quoted word — a quoted ``"false"``/``"no"``/``"off"`` never reads as
    True (``bool("false")`` would). Mutant: ``bool(...)`` instead of ``_flag`` → ``hosted: "no"`` reports True."""
    for value, expected in ((True, True), ("yes", True), ("On", True), (False, False), ("no", False), ("false", False)):
        lst = Listener(tmp_path / f"f-{value}", operator=True, settings={"operator_api": value, "hosted": value})
        d = lst.pair(tmp_path / f"f-{value}")
        data = body_of(lst.handle(d.req("GET", "hello")))
        assert data["limits"]["hosted"] is expected, value
        assert ("operator.v1" in data["capabilities"]) is expected, value
        assert lst.server.operator_enabled() is expected, value
