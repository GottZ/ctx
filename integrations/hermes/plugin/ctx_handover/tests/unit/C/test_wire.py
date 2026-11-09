"""HTTP client against a real listener process on loopback (DESIGN §4.1–§4.6; R7, SEC 1.4/1.5; TESTPLAN §2.4).

Every test here talks to ``listener_proc.py`` — a separate Python process serving the real
``dashboard/plugin_api.py`` behind an imitation of the dashboard gate and a reverse proxy — over TCP. The
process is stopped by PID at teardown (fixture ``spawn_listener``).
"""

from __future__ import annotations

import hashlib
import logging

import pytest

from ctx_handover.engine import hostcompat, pairing, wire
from ctx_handover.engine.blocks import BLOCK_SIZE, BlockStore, encode_frame
from ctx_handover.engine.errors import (
    BadResponseSignature,
    GateRejected,
    HttpsRequired,
    ListenerMissing,
    NotFound,
    PayloadTooLarge,
    PeerUnreachable,
    RateLimited,
    Unauthorized,
)
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import GateAuth, GateMode, Peer, Right
from ctx_handover.engine.wire import WireClient, gate_headers, gate_warnings
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

ALL = frozenset({Right.PUSH, Right.PULL})
TOKEN = "gate-tok-7f3a"


class Dialer:
    def __init__(self, tmp_path, name: str = "dialer") -> None:
        self.ledger = make_ledger(tmp_path / name / "ledger.sqlite3")
        self.identity = new_identity(self.ledger)
        self.sleeps: list[float] = []

    def client(self, peer: Peer, **kw) -> WireClient:
        kw.setdefault("sleep", self.sleeps.append)
        return WireClient(peer, self.identity, read_secret=hostcompat.read_secret, **kw)

    def pair(self, lst, *, gate=None, rights=ALL, name="server", **kw) -> Peer:
        code = pairing.create_code(lst.ledger, rights=rights, name_hint="laptop", ttl_s=600)
        text = pairing.pair_string(
            code, url=lst.base_url, fingerprint=lst.identity.fingerprint(), name="server", profile=None
        )
        return pairing.redeem_dialer(
            lambda peer: self.client(peer, **kw),
            self.identity,
            pairing.parse_pair_string(text),
            name=name,
            gate=gate or GateAuth(mode=GateMode.SESSION_TOKEN),
            allow_insecure=False,
            ledger=self.ledger,
        )


@pytest.fixture
def token(host):
    host.secrets["CTX_HANDOVER_PEER_TOKEN"] = TOKEN
    return TOKEN


def test_wire_pair_and_signed_calls_over_http(tmp_path, spawn_listener, token):
    lst = spawn_listener(gate_token=token)
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    assert lst.ledger.inbound_peer(peer.peer_id).pubkey == d.identity.public_b64()
    client = d.client(peer)
    hello = client.hello(signed=True)
    assert hello.peer.peer_id == peer.peer_id and hello.peer.rights == ALL
    assert hello.limits.max_request_bytes == 8 * 1024**2
    assert client.listener_id == lst.identity.instance_id
    assert client.call("POST", "sessions/list", {"limit": 2})["sessions"][0]["id"] == "s1"
    assert abs(client.clock_skew) < 5


def test_signature_behind_prefix_proxy(tmp_path, spawn_listener, token):
    """★ Base URL ``…/hermes`` behind a proxy that strips the prefix: signatures still verify (SEC 1.4).

    Red proof: mutant = ``plugin_api.dispatch`` passes ``request.url.path`` instead of the router's ``path``
    parameter → the listener sees ``/api/plugins/ctx_handover/v1/hello`` as the route and refuses it.
    """
    lst = spawn_listener(gate_token=token, prefix="/hermes")
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    assert peer.url.endswith("/hermes")
    client = d.client(peer)
    assert client.hello(signed=True).peer.peer_id == peer.peer_id
    assert client.call("POST", "sessions/list", {"limit": 1})["sessions"]
    assert all(line.split()[1].startswith("/hermes/") for line in lst.requests())


def test_response_signature_required(tmp_path, spawn_listener, token):
    """★ An unsigned 200 (or a 200 signed by another key) is refused: ``bad_response_signature`` (T11).

    Red proof: mutant = ``_interpret`` accepts unsigned 2xx when the peer key is pinned → the forged body is
    returned and ``pytest.raises(BadResponseSignature)`` fails.
    """
    lst = spawn_listener(gate_token=token)
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    client = d.client(peer)
    client.hello(signed=True)
    lst.inject(
        {"status": 200, "body": '{"sessions": [{"id": "forged"}]}', "headers": {"content-type": "application/json"}}
    )
    with pytest.raises(BadResponseSignature):
        client.call("POST", "sessions/list", {"limit": 1})
    lst.inject({"status": 200, "body": "{}", "headers": {"X-Ctxh-Resp-Sig": "A" * 86}})
    with pytest.raises(BadResponseSignature):
        client.call("POST", "sessions/list", {"limit": 1})


@pytest.mark.parametrize(
    ("status", "error", "retry_after"),
    [
        (413, PayloadTooLarge, None),
        (429, RateLimited, 300),
        (502, PeerUnreachable, None),
        (503, PeerUnreachable, None),
        (504, PeerUnreachable, None),
    ],
)
def test_wire_unsigned_proxy_status_is_signal_only(tmp_path, spawn_listener, token, status, error, retry_after):
    """★ Unsigned 413/429/502–504 are status signals only: the body is never parsed, ``Retry-After`` ≤ 300 s
    (SEC 1.5, hardening #3).

    The injected body is a forged plugin envelope (``verify_failed``); parsing it would raise ``VerifyFailed``.
    Red proof: mutant = ``_interpret`` without the cap (``min(…, 300)`` removed) → the 429 case records a
    999-second sleep.
    """
    lst = spawn_listener(gate_token=token)
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    client = d.client(peer, max_attempts=1)
    client.hello(signed=True)
    forged = '{"error": {"code": "verify_failed", "message": "forged", "retryable": false, "details": {}}}'
    lst.inject(
        {"status": status, "body": forged, "headers": {"Retry-After": "999", "content-type": "application/json"}}
    )
    with pytest.raises(error) as info:
        client.call("POST", "sessions/list", {"limit": 1})
    assert type(info.value) is error
    if retry_after is not None:
        assert info.value.details["retry_after"] == retry_after
    # with retries the same signal is waited out (capped) and the call then succeeds
    retrying = d.client(peer, max_attempts=3)
    retrying.hello(signed=True)
    lst.inject({"status": status, "body": forged, "headers": {"Retry-After": "999"}})
    if status == 413:
        with pytest.raises(PayloadTooLarge):
            retrying.call("POST", "sessions/list", {"limit": 1})
        return
    assert retrying.call("POST", "sessions/list", {"limit": 1})["sessions"]
    assert all(s <= 300 for s in d.sleeps)
    if status == 429:
        assert 300 in d.sleeps


def test_wire_unsigned_401_is_gate_rejected(tmp_path, spawn_listener, token, host):
    lst = spawn_listener(gate_token=token)
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "wrong"
    with pytest.raises(GateRejected):
        d.client(peer).hello(signed=True)
    host.secrets["CTX_HANDOVER_PEER_TOKEN"] = token
    # a signed 401 is the plugin, not the gate
    lst.ledger.revoke_peer(peer.peer_id)
    lst.ledger.upsert_peer(
        Peer(
            name="keepalive", peer_id=ulid(), direction="inbound", url=None, pubkey=d.identity.public_b64(), rights=ALL
        )
    )
    with pytest.raises(Unauthorized):
        d.client(peer).hello(signed=True)


def test_wire_404_is_listener_missing(tmp_path, spawn_listener, token):
    lst = spawn_listener(gate_token=token)
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    lst.inject({"status": 404, "body": '{"detail":"Plugin not found"}'})
    with pytest.raises(ListenerMissing):
        d.client(peer).hello(signed=True)


def test_wire_gate_modes(tmp_path, spawn_listener, host, caplog):
    """Session token, bearer, Basic login and extra proxy headers reach the plugin; no secret is logged (R7)."""
    caplog.set_level(logging.DEBUG)
    host.secrets.update(
        {"CTX_HANDOVER_PEER_TOKEN": TOKEN, "CTX_HANDOVER_PEER_PASSWORD": "pw-9d1c", "PROXY_KEY": "px-55aa"}
    )
    lst = spawn_listener(
        gate_token=TOKEN, basic={"user": "lab", "password": "pw-9d1c"}, extra_header=["X-Proxy-Key", "px-55aa"]
    )
    d = Dialer(tmp_path)
    extra = (("X-Proxy-Key", "PROXY_KEY"),)
    peer = d.pair(lst, gate=GateAuth(mode=GateMode.SESSION_TOKEN, extra_headers=extra))
    modes = [
        GateAuth(mode=GateMode.SESSION_TOKEN, extra_headers=extra),
        GateAuth(mode=GateMode.BEARER, extra_headers=extra),
        GateAuth(mode=GateMode.BASIC_LOGIN, username="lab", extra_headers=extra),
    ]
    for gate in modes:
        client = d.client(Peer(**{**peer.__dict__, "gate": gate}))
        assert client.hello(signed=True).peer.peer_id == peer.peer_id, gate.mode
    assert lst.logins() == 1
    without_extra = d.client(Peer(**{**peer.__dict__, "gate": GateAuth(mode=GateMode.BEARER)}))
    with pytest.raises(GateRejected):
        without_extra.hello(signed=True)
    for secret in ("pw-9d1c", "px-55aa", TOKEN):
        assert secret not in caplog.text


def test_gate_secrets_read_via_get_secret(host, monkeypatch):
    """Gate secrets come from ``read_secret`` (= ``get_secret``) only — never from ``.env`` or ``os.environ``
    (SEC 7.3, hardening #22)."""
    host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "from-get-secret"
    host.env_file["CTX_HANDOVER_PEER_TOKEN"] = "from-dotenv"
    monkeypatch.setenv("CTX_HANDOVER_PEER_TOKEN", "from-environ")
    headers = gate_headers(GateAuth(mode=GateMode.SESSION_TOKEN), hostcompat.read_secret)
    assert headers == {"X-Hermes-Session-Token": "from-get-secret"}
    assert ("read_secret", ("CTX_HANDOVER_PEER_TOKEN",)) in host.calls
    assert not [c for c in host.calls if c[0] == "read_env_fresh"]
    del host.secrets["CTX_HANDOVER_PEER_TOKEN"]
    host.env_file.clear()
    with pytest.raises(GateRejected) as info:
        gate_headers(GateAuth(mode=GateMode.BEARER), hostcompat.read_secret)
    assert "CTX_HANDOVER_PEER_TOKEN" in info.value.message
    host.secrets["BAD"] = "x\r\ny"
    with pytest.raises(GateRejected):
        gate_headers(GateAuth(mode=GateMode.SESSION_TOKEN, token_env="BAD"), hostcompat.read_secret)
    with pytest.raises(GateRejected):
        gate_headers(GateAuth(mode=GateMode.BASIC_LOGIN), hostcompat.read_secret, login=None)


def test_basic_login_relogin_once_on_expiry(tmp_path, spawn_listener, host):
    host.secrets.update({"CTX_HANDOVER_PEER_TOKEN": TOKEN, "CTX_HANDOVER_PEER_PASSWORD": "pw"})
    lst = spawn_listener(gate_token=TOKEN, basic={"user": "u", "password": "pw"})
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    client = d.client(Peer(**{**peer.__dict__, "gate": GateAuth(mode=GateMode.BASIC_LOGIN, username="u")}))
    client.hello(signed=True)
    assert lst.logins() == 1
    lst.expire_tokens()
    assert client.call("POST", "sessions/list", {"limit": 1})["sessions"]
    assert lst.logins() == 2
    lst.expire_tokens()
    (lst.control / "login_disabled").write_text("1")
    with pytest.raises(GateRejected):
        client.call("POST", "sessions/list", {"limit": 1})
    assert lst.logins() == 3  # exactly one re-login attempt


def test_basic_login_function(tmp_path, spawn_listener):
    lst = spawn_listener(gate_token=TOKEN, basic={"user": "u", "password": "pw"})
    token = wire.basic_login(lst.base_url, "u", "pw", verify_tls=True)
    assert len(token) > 20
    with pytest.raises(GateRejected):
        wire.basic_login(lst.base_url, "u", "nope", verify_tls=True)


def test_wire_https_required(tmp_path):
    d = Dialer(tmp_path)

    def peer(url, insecure=False):
        return Peer(
            name="s",
            peer_id=ulid(),
            direction="dial",
            url=url,
            pubkey="",
            rights=ALL,
            gate=GateAuth(mode=GateMode.SESSION_TOKEN),
            allow_insecure=insecure,
        )

    with pytest.raises(HttpsRequired):
        d.client(peer("http://server.example:9119"))
    with pytest.raises(HttpsRequired):
        d.client(peer("http://100.64.0.7:9119"))
    d.client(peer("http://100.64.0.7:9119", insecure=True)).close()
    for ok in ("https://server.example", "http://127.0.0.1:9119", "http://localhost:1", "http://[::1]:5"):
        d.client(peer(ok)).close()
    with pytest.raises(HttpsRequired):
        d.client(peer("ftp://server.example"))


def test_wire_adaptive_request_size():
    a = wire.Adaptive(peer_max=8 * 1024**2)
    assert a.size == 4 * 1024**2
    a.too_large()
    assert a.size == 2 * 1024**2
    for _ in range(3):
        a.observe(1.0)
    assert a.size == 4 * 1024**2
    a.observe(25.0)
    assert a.size == 2 * 1024**2
    for _ in range(20):
        a.too_large() if a.size > a.floor else None
    assert a.size == a.floor == BLOCK_SIZE + 41 + 1024
    with pytest.raises(PayloadTooLarge):
        a.too_large()
    small = wire.Adaptive(peer_max=1024**2)
    assert small.size == 1024**2
    learned = wire.Adaptive(peer_max=8 * 1024**2)
    learned.too_large(3_000_000)
    for _ in range(9):
        learned.observe(1.0)
    assert learned.size == 2_999_999


def _noise(n: int, seed: int) -> bytes:
    out = bytearray()
    i = 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}:{i}".encode()).digest()
        i += 1
    return bytes(out[:n])


def test_wire_put_get_blocks_adaptive_over_http(tmp_path, spawn_listener, token):
    """Blocks travel both ways; a proxy body limit below the starting size is found by halving (DESIGN §4.6)."""
    lst = spawn_listener(gate_token=token, proxy_limit=1_200_000)
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    client = d.client(peer)
    client.hello(signed=True)
    blocks = [(hashlib.sha256(r).hexdigest(), r) for r in (_noise(BLOCK_SIZE, s) for s in range(5))]
    job = ulid()
    client.call(
        "POST", "jobs", {"job_id": job, "epoch": 1, "role": "target", "mode": "push", "conductor": {}, "options": {}}
    )
    frames = [encode_frame(i, raw, peer_caps=client.peer_caps) for i, raw in blocks]
    stored, rejected = client.put_blocks(job, 1, frames)
    assert (stored, rejected) == (5, [])
    assert client._adaptive.ceiling < 2 * 1024**2  # the refused batch size is remembered
    assert client.request_bytes < 4 * 1024**2
    assert any(" /api/plugins/ctx_handover/v1/jobs/" in line for line in lst.requests())
    # pull the same blocks from a source job of this peer
    src = ulid()
    client.call(
        "POST", "jobs", {"job_id": src, "epoch": 1, "role": "source", "mode": "pull", "conductor": {}, "options": {}}
    )
    store = BlockStore(lst.plugin_dir, lst.ledger)
    m = store.put_bytes(b"".join(raw for _i, raw in blocks), "application/octet-stream")
    lst.ledger.job_object(src, m.object_id)
    got = dict(client.get_blocks(src, 1, list(m.blocks)))
    assert list(got) == list(m.blocks)
    assert b"".join(got[i] for i in m.blocks) == b"".join(raw for _i, raw in blocks)
    with pytest.raises(NotFound):
        list(client.get_blocks(src, 1, [hashlib.sha256(b"nope").hexdigest()]))


def test_wire_retries_only_retryable(tmp_path, spawn_listener, token):
    lst = spawn_listener(gate_token=token)
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    client = d.client(peer, max_attempts=4)
    client.hello(signed=True)
    before = len(lst.requests())
    lst.inject({"status": 503}, {"status": 502})
    assert client.call("POST", "sessions/list", {"limit": 1})["sessions"]
    assert len(lst.requests()) - before == 3
    before = len(lst.requests())
    with pytest.raises(NotFound):
        client.call("POST", f"jobs/{ulid()}/status", {"epoch": 1}, epoch=1)
    assert len(lst.requests()) - before == 1
    stopped = d.client(Peer(**{**peer.__dict__, "url": "http://127.0.0.1:9"}), max_attempts=2)
    with pytest.raises(PeerUnreachable):
        stopped.call("POST", "sessions/list", {"limit": 1})


def test_probe_gate_reports_layers(tmp_path, spawn_listener, token, host):
    lst = spawn_listener(gate_token=token, proxy_limit=3 * 1024**2)
    d = Dialer(tmp_path)
    peer = d.pair(lst)
    report = d.client(peer).probe_gate(measure_limit=True)
    assert report["gate"] == "ok" and report["plugin"] is True and report["signed"] is True
    assert 2 * 1024**2 <= report["body_limit"] < 4 * 1024**2
    host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "wrong"
    assert d.client(peer).probe_gate()["gate"] == "rejected"


def test_doctor_warns_deployment_token_and_readable_env():
    """T1: doctor names stored full-dashboard credentials and a group/world-readable ``.env``."""
    warnings = gate_warnings(GateAuth(mode=GateMode.SESSION_TOKEN), env_mode=0o644)
    assert any("whole dashboard" in w for w in warnings)
    assert any("readable" in w for w in warnings)
    assert any("password" in w for w in gate_warnings(GateAuth(mode=GateMode.BASIC_LOGIN), env_mode=0o600))
    assert not any("readable" in w for w in gate_warnings(GateAuth(mode=GateMode.BEARER), env_mode=0o600))
