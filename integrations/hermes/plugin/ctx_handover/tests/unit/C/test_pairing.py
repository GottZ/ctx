"""Pairing protocol (DESIGN §12.3; R7, R17; SEC 1.3; TESTPLAN §2.4, hardening #1)."""

from __future__ import annotations

import base64
import json
import logging

import pytest

from ctx_handover.engine import pairing
from ctx_handover.engine.crypto import b64e, fingerprint_of
from ctx_handover.engine.errors import BadRequest, BadResponseSignature, PairInvalid
from ctx_handover.engine.model import GateAuth, GateMode, Hello, PeerInfo, Right
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.helpers import child_env, run_barrier_children
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

RIGHTS = frozenset({Right.PUSH, Right.PULL})
GATE = GateAuth(mode=GateMode.SESSION_TOKEN)


class Side:
    """One host: its ledger and identity."""

    def __init__(self, tmp_path, name: str) -> None:
        self.env: dict[str, str] = {}
        self.ledger = make_ledger(tmp_path / name / "ledger.sqlite3")
        self.identity = new_identity(self.ledger, self.env)
        self.name = name


class LoopbackClient:
    """In-process stand-in for ``wire.WireClient`` during pairing: calls the listener functions directly."""

    def __init__(self, listener: Side, clock, *, forge_server_proof: bool = False, pubkey: str | None = None):
        self.listener = listener
        self.clock = clock
        self.forge = forge_server_proof
        self.pubkey = pubkey
        self.redeemed: list[dict] = []
        self.signed_hellos = 0

    def __call__(self, peer):
        self.peer = peer
        return self

    def hello(self, *, signed: bool = True) -> Hello:
        ident = self.listener.identity
        if signed:
            self.signed_hellos += 1
            assert self.peer.peer_id and self.peer.pubkey == ident.public_b64()
        return Hello(
            protocol="ctxh",
            majors=(1,),
            minor=0,
            instance_id=ident.instance_id,
            listener_pubkey=self.pubkey or ident.public_b64(),
            pairing_open=True,
            peer=PeerInfo(peer_id=self.peer.peer_id, rights=RIGHTS) if signed else None,
        )

    def redeem(self, body: dict) -> dict:
        self.redeemed.append(body)
        resp = pairing.redeem_listener(
            self.listener.ledger, self.listener.identity, body, "127.0.0.1", name="server", clock=self.clock
        )
        if self.forge:
            resp = dict(resp, server_proof=b64e(b"\0" * 32))
        return resp


@pytest.fixture
def sides(tmp_path):
    return Side(tmp_path, "listener"), Side(tmp_path, "dialer")


def _paste(listener: Side, clock, *, rights=RIGHTS, ttl_s=600, name_hint="laptop") -> tuple[str, pairing.PairCode]:
    code = pairing.create_code(listener.ledger, rights=rights, name_hint=name_hint, ttl_s=ttl_s, clock=clock)
    text = pairing.pair_string(
        code,
        url="https://server.example/hermes",
        fingerprint=listener.identity.fingerprint(),
        name="server",
        profile=None,
    )
    return text, code


def test_pair_string_roundtrip(sides):
    listener, _ = sides
    clock = FakeClock()
    text, code = _paste(listener, clock)
    assert text.startswith("ctxh-pair:")
    parsed = pairing.parse_pair_string(text)
    assert parsed == {
        "v": 1,
        "url": "https://server.example/hermes",
        "code": f"CTXH-{code.code_id}-{code.secret}",
        "fp": listener.identity.fingerprint(),
        "name": "server",
        "profile": None,
    }
    assert len(code.code_id) == 6 and len(code.secret) == 16
    for bad in ("ctxh-pair:", "nope", "ctxh-pair:" + "A" * 9000, text[:-3] + "!!!"):
        with pytest.raises(BadRequest):
            pairing.parse_pair_string(bad)


def test_paste_string_contains_no_gate_secret(sides, host):
    """★ The paste string never carries a gate credential (R17, T1, T15).

    Red proofs: (a) mutant of TESTPLAN = ``pair_string`` adds the gate token (``"token": <CTX_HANDOVER_PEER_TOKEN>``)
    → the key-set assertion fails; (b) mutant = ``pair_string`` keeps the URL as given (no ``_clean_url``) →
    ``pytest.raises(BadRequest)`` fails and the password travels inside the string.
    """
    listener, _ = sides
    host.secrets.update({"CTX_HANDOVER_PEER_TOKEN": "tok-SECRET-1", "CTX_HANDOVER_PEER_PASSWORD": "pw-SECRET-2"})
    code = pairing.create_code(listener.ledger, rights=RIGHTS, name_hint="", ttl_s=600)
    text = pairing.pair_string(
        code, url="https://server.example", fingerprint=listener.identity.fingerprint(), name="s", profile="work"
    )
    decoded = base64.urlsafe_b64decode(text.split(":", 1)[1] + "==").decode()
    assert set(json.loads(decoded)) == {"v", "url", "code", "fp", "name", "profile"}
    for secret in host.secrets.values():
        assert secret not in decoded
    for url in (
        "https://user:pw-SECRET-2@server.example",
        "https://server.example/?token=tok-SECRET-1",
        "https://server.example/#tok",
        "ftp://server.example",
        "https://",
    ):
        with pytest.raises(BadRequest):
            pairing.pair_string(code, url=url, fingerprint=listener.identity.fingerprint(), name="s", profile=None)


def test_pair_redeem_success_mutual_proof(sides):
    listener, dialer = sides
    clock = FakeClock()
    text, _ = _paste(listener, clock)
    client = LoopbackClient(listener, clock)
    peer = pairing.redeem_dialer(
        client,
        dialer.identity,
        pairing.parse_pair_string(text),
        name="server",
        gate=GATE,
        allow_insecure=False,
        ledger=dialer.ledger,
        self_name="laptop-host",
        clock=clock,
    )
    assert peer.direction == "dial"
    assert peer.pubkey == listener.identity.public_b64()
    assert peer.url == "https://server.example/hermes"
    assert peer.rights == RIGHTS
    assert peer.gate == GATE
    assert dialer.ledger.peer("server").peer_id == peer.peer_id
    inbound = listener.ledger.inbound_peer(peer.peer_id)
    assert inbound is not None
    assert inbound.direction == "inbound"
    assert inbound.pubkey == dialer.identity.public_b64()
    assert inbound.name == "laptop"  # the listener's --name wins over the dialer's self name
    assert inbound.rights == RIGHTS
    assert inbound.url is None and inbound.gate is None
    assert client.signed_hellos == 1
    # the code is single-use
    with pytest.raises(PairInvalid):
        pairing.redeem_listener(listener.ledger, listener.identity, client.redeemed[0], None, clock=clock)


def test_pair_proof_length_prefixed_and_body_bound(sides):
    """★ Changing ``dialer.name`` (or any body field) invalidates the proof (SEC 1.3, hardening #1).

    Red proof: mutant = the body hash left out of the proof message (``_proof_message`` without ``body_hash``) →
    the renamed body still redeems; ``pytest.raises(PairInvalid)`` fails.
    """
    listener, dialer = sides
    clock = FakeClock()
    text, _ = _paste(listener, clock)
    pair = pairing.parse_pair_string(text)
    body = pairing.redeem_body(
        dialer.identity,
        pair,
        listener_instance_id=listener.identity.instance_id,
        self_name="laptop",
        plugin_version="0.1.0",
    )
    for path, value in (("name", "evil"), ("plugin_version", "9.9.9")):
        tampered = json.loads(json.dumps(body))
        tampered["dialer"][path] = value
        with pytest.raises(PairInvalid):
            pairing.redeem_listener(listener.ledger, listener.identity, tampered, None, clock=clock)
    tampered = dict(body, client_nonce=b64e(b"\1" * 16))
    with pytest.raises(PairInvalid):
        pairing.redeem_listener(listener.ledger, listener.identity, tampered, None, clock=clock)
    # two failures so far + one for the nonce; the original still works (failures < 5)
    resp = pairing.redeem_listener(listener.ledger, listener.identity, body, None, clock=clock)
    assert resp["listener"]["pubkey"] == listener.identity.public_b64()


def test_pair_ttl_capped(sides):
    listener, _ = sides
    clock = FakeClock()
    code = pairing.create_code(listener.ledger, rights=RIGHTS, name_hint="", ttl_s=7200, clock=clock)
    assert code.expires_at - clock() == 3600
    code = pairing.create_code(listener.ledger, rights=RIGHTS, name_hint="", ttl_s=600, clock=clock)
    assert code.expires_at - clock() == 600
    with pytest.raises(BadRequest):
        pairing.create_code(listener.ledger, rights=frozenset(), name_hint="", ttl_s=600, clock=clock)


def test_pair_redeem_wrong_code_burns_after_five(sides):
    """★ Five wrong proofs burn the code; the right one is refused afterwards (SEC 1.3).

    Red proof: mutant = ``ledger.fail_pair_code`` not called on a wrong proof → the sixth (correct) attempt
    redeems; ``pytest.raises(PairInvalid)`` fails.
    """
    listener, dialer = sides
    clock = FakeClock()
    text, _ = _paste(listener, clock)
    pair = pairing.parse_pair_string(text)
    good = pairing.redeem_body(
        dialer.identity, pair, listener_instance_id=listener.identity.instance_id, self_name="x", plugin_version="1"
    )
    wrong_pair = dict(pair, code=pair["code"][:-1] + ("A" if pair["code"][-1] != "A" else "B"))
    wrong = pairing.redeem_body(
        dialer.identity,
        wrong_pair,
        listener_instance_id=listener.identity.instance_id,
        self_name="x",
        plugin_version="1",
    )
    for _ in range(5):
        with pytest.raises(PairInvalid):
            pairing.redeem_listener(listener.ledger, listener.identity, wrong, None, clock=clock)
    with pytest.raises(PairInvalid):
        pairing.redeem_listener(listener.ledger, listener.identity, good, None, clock=clock)
    assert listener.ledger.peers() == []


def test_pair_redeem_expired(sides):
    listener, dialer = sides
    clock = FakeClock()
    text, _ = _paste(listener, clock, ttl_s=60)
    body = pairing.redeem_body(
        dialer.identity,
        pairing.parse_pair_string(text),
        listener_instance_id=listener.identity.instance_id,
        self_name="x",
        plugin_version="1",
    )
    clock.advance(61)
    with pytest.raises(PairInvalid):
        pairing.redeem_listener(listener.ledger, listener.identity, body, None, clock=clock)


def test_pair_mitm_without_code_fails(sides, tmp_path):
    """A party without the code cannot redeem, and a relay that cannot produce ``server_proof`` is refused."""
    listener, dialer = sides
    clock = FakeClock()
    text, code = _paste(listener, clock)
    pair = pairing.parse_pair_string(text)
    guessed = dict(pair, code=f"CTXH-{code.code_id}-{'0' * 16}")
    body = pairing.redeem_body(
        dialer.identity, guessed, listener_instance_id=listener.identity.instance_id, self_name="m", plugin_version="1"
    )
    with pytest.raises(PairInvalid):
        pairing.redeem_listener(listener.ledger, listener.identity, body, None, clock=clock)
    # a relay in the middle forwards the redeem but answers with its own server proof
    other = Side(tmp_path, "other")
    text2, _ = _paste(listener, clock)
    with pytest.raises(BadResponseSignature):
        pairing.redeem_dialer(
            LoopbackClient(listener, clock, forge_server_proof=True),
            other.identity,
            pairing.parse_pair_string(text2),
            name="server",
            gate=GATE,
            allow_insecure=False,
            ledger=other.ledger,
            clock=clock,
        )
    assert other.ledger.peers() == []


def test_pair_fingerprint_mismatch_aborts(sides, tmp_path):
    listener, dialer = sides
    clock = FakeClock()
    text, _ = _paste(listener, clock)
    impostor = Side(tmp_path, "impostor")
    client = LoopbackClient(listener, clock, pubkey=impostor.identity.public_b64())
    with pytest.raises(BadResponseSignature) as info:
        pairing.redeem_dialer(
            client,
            dialer.identity,
            pairing.parse_pair_string(text),
            name="server",
            gate=GATE,
            allow_insecure=False,
            ledger=dialer.ledger,
            clock=clock,
        )
    assert "fingerprint" in info.value.message
    assert client.redeemed == []  # nothing was sent to the impostor
    assert dialer.ledger.peers() == []
    assert fingerprint_of(impostor.identity.public_b64()) != listener.identity.fingerprint()


def test_pair_listener_name_unique(sides, tmp_path):
    listener, _ = sides
    clock = FakeClock()
    names = []
    for i in range(3):
        d = Side(tmp_path, f"d{i}")
        text, _ = _paste(listener, clock, name_hint="")
        peer = pairing.redeem_dialer(
            LoopbackClient(listener, clock),
            d.identity,
            pairing.parse_pair_string(text),
            name="server",
            gate=GATE,
            allow_insecure=False,
            ledger=d.ledger,
            self_name="lap top/../x",
            clock=clock,
        )
        names.append(listener.ledger.inbound_peer(peer.peer_id).name)
    assert names == ["lap-top-..-x", "lap-top-..-x-2", "lap-top-..-x-3"]


def test_pair_codes_never_logged(sides, caplog):
    """SEC 6.2: codes and proofs never reach a log."""
    caplog.set_level(logging.DEBUG)
    listener, dialer = sides
    clock = FakeClock()
    text, code = _paste(listener, clock)
    pair = pairing.parse_pair_string(text)
    body = pairing.redeem_body(
        dialer.identity, pair, listener_instance_id=listener.identity.instance_id, self_name="x", plugin_version="1"
    )
    bad = dict(body, proof=b64e(b"\2" * 32))
    with pytest.raises(PairInvalid):
        pairing.redeem_listener(listener.ledger, listener.identity, bad, None, clock=clock)
    pairing.redeem_listener(listener.ledger, listener.identity, body, None, clock=clock)
    assert code.secret not in caplog.text
    assert body["proof"] not in caplog.text


def test_pair_redeem_atomic_two_processes(tmp_path):
    """★ Two processes (gateway and ``hermes serve`` of one profile) redeem one code at the same instant: exactly
    one succeeds (SEC 1.3, hardening #1).

    Red proof: mutant = ``redeem_listener`` stores the peer with ``upsert_peer`` after its own ``pair_code``
    check instead of ``redeem_pair_code``'s single ``DELETE … rowcount == 1`` → both children report success.
    """
    listener = Side(tmp_path, "listener")
    dialer = Side(tmp_path, "dialer")
    code = pairing.create_code(listener.ledger, rights=RIGHTS, name_hint="x", ttl_s=3600)
    text = pairing.pair_string(
        code, url="https://s.example", fingerprint=listener.identity.fingerprint(), name="s", profile=None
    )
    body = pairing.redeem_body(
        dialer.identity,
        pairing.parse_pair_string(text),
        listener_instance_id=listener.identity.instance_id,
        self_name="x",
        plugin_version="1",
    )
    body_file = tmp_path / "body.json"
    body_file.write_text(json.dumps(body), encoding="utf-8")
    env_file = tmp_path / "env.json"
    env_file.write_text(json.dumps(listener.env), encoding="utf-8")
    go = tmp_path / "go"
    argv = ["redeem", str(tmp_path / "listener" / "ledger.sqlite3"), str(go), str(body_file), str(env_file)]
    results = run_barrier_children([argv, argv, argv], go, env=child_env())
    assert sorted(r["ok"] for r in results) == [False, False, True], results
    assert {r.get("code") for r in results if not r["ok"]} == {"pair_invalid"}
    assert len(listener.ledger.peers()) == 1


def test_parse_pair_string_strict_fields(sides):
    _listener, _ = sides
    good = {
        "v": 1,
        "url": "https://s.example",
        "code": "CTXH-ABCDEF-0123456789ABCDEF",
        "fp": "0" * 16,
        "name": "s",
        "profile": None,
    }

    def enc(doc):
        return "ctxh-pair:" + b64e(json.dumps(doc).encode())

    assert pairing.parse_pair_string(enc(good))["code"] == good["code"]
    for change in (
        {"v": 2},
        {"code": "CTXH-ABCDEF-0123"},
        {"code": "CTXH-abcdef-0123456789ABCDEF"},
        {"fp": "xyz"},
        {"url": "http://user:pw@s"},
        {"profile": "../x"},
        {"name": 5},
    ):
        with pytest.raises(BadRequest):
            pairing.parse_pair_string(enc(dict(good, **change)))
    with pytest.raises(BadRequest):
        pairing.parse_pair_string(enc({k: v for k, v in good.items() if k != "fp"}))
