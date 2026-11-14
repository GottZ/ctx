"""Round G′, cluster C — V44 (lab finding F05): pairing is two-sided or nothing (DESIGN §12.3).

A dialer whose chosen name for the listener is already taken must not leave the listener with a paired inbound peer
the dialer does not know: the name is checked before the code is redeemed, and a clash that appears while the
redeem is under way is taken back on the listener (route #3 ``POST /pair/revoke``, signed as the new peer). A bundle
signed by a key that several peers share names the established peer, never a later duplicate.

The listener runs in process (real A ``Ledger`` and ``ParticipantRegistry``, real C ``Server``); the dialer reaches
it through the real ``wire.WireClient`` over ``InProcessTransport``.
"""

from __future__ import annotations

import hashlib
import json
import os
import time

import httpx
import pytest

from ctx_handover.engine import pairing
from ctx_handover.engine.bundle import BundleSource
from ctx_handover.engine.crypto import H_RESP_SIG, b64e, response_string
from ctx_handover.engine.errors import ConflictError
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import GateAuth, GateMode, Peer
from ctx_handover.engine.server import PeerSignatureAuth, Server
from ctx_handover.engine.wire import API_PREFIX, WireClient
from ctx_handover.tests.unit.C._l2r_helpers import ALL_RIGHTS, InProcessTransport
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.ledger_stub import make_ledger
from ctx_handover.tests.unit.C.test_bundle import Exporter, make_bundle

URL = "http://127.0.0.1:9"
GATE = GateAuth(mode=GateMode.SESSION_TOKEN)


class _Listener:
    """An unpaired listener with an open pairing code, served in process (no dashboard gate in front)."""

    def __init__(self, tmp_path) -> None:
        from ctx_handover.engine.participant import ParticipantRegistry

        self.ledger = make_ledger(tmp_path / "listener" / "ledger.sqlite3")
        self.identity = new_identity(self.ledger)
        registry = ParticipantRegistry(self.ledger, lambda role, peer: None)
        server = Server(
            self.ledger, self.identity, registry, {"name": "server"}, [PeerSignatureAuth(self.ledger, self.identity)]
        )
        self.transport = InProcessTransport(server)

    def paste(self) -> tuple[str, pairing.PairCode]:
        code = pairing.create_code(self.ledger, rights=ALL_RIGHTS, name_hint="laptop", ttl_s=600)
        text = pairing.pair_string(code, url=URL, fingerprint=self.identity.fingerprint(), name="server", profile=None)
        return text, code

    def entries_of(self, pubkey: str) -> list[Peer]:
        return [p for p in self.ledger.peers(include_revoked=True) if p.pubkey == pubkey]

    def routes(self) -> list[str]:
        return [route for _method, route, _n in self.transport.seen]


class _Dialer:
    def __init__(self, tmp_path, listener: _Listener) -> None:
        self.ledger = make_ledger(tmp_path / "dialer" / "ledger.sqlite3")
        self.identity = new_identity(self.ledger)
        self.listener = listener

    def client(self, peer: Peer, transport: httpx.BaseTransport | None = None) -> WireClient:
        return WireClient(
            peer,
            self.identity,
            read_secret=lambda name: "gate-token",
            transport=transport or self.listener.transport,
            max_attempts=1,
            sleep=lambda s: None,
        )

    def pair(self, text: str, *, name: str, factory=None) -> Peer:
        return pairing.redeem_dialer(
            factory or self.client,
            self.identity,
            pairing.parse_pair_string(text),
            name=name,
            gate=GATE,
            allow_insecure=False,
            ledger=self.ledger,
        )


def _other_peer(name: str, direction: str, *, revoked: bool = False, pubkey: str | None = None, at: float = 0.0):
    """A peer row of another device (a random key unless ``pubkey`` is given)."""
    return Peer(
        name=name,
        peer_id=ulid(),
        direction=direction,
        url=URL if direction == "dial" else None,
        pubkey=pubkey or b64e(os.urandom(32)),
        rights=ALL_RIGHTS,
        created_at=at or time.time(),
        revoked_at=time.time() if revoked else None,
    )


@pytest.fixture
def two(tmp_path):
    listener = _Listener(tmp_path)
    return listener, _Dialer(tmp_path, listener)


@pytest.mark.parametrize(("direction", "revoked"), [("inbound", False), ("dial", False), ("inbound", True)])
def test_a_taken_name_is_refused_before_the_code_is_redeemed(two, direction, revoked):
    """★ F05: ``pair <code> --name b`` on a device that already has a peer ``b`` (here: B paired with it the other
    way, or an old revoked entry) is refused before anything reaches the listener; the code stays valid, so pairing
    again under another name works with the same paste string, and the listener then has exactly one entry.

    Red proof: on ``94cf9fc1``/``c31da4c3`` the redeem ran first — the listener kept an inbound peer the dialer does
    not know (``entries_of`` = 1 active entry, ``routes`` = hello + pair/redeem). Mutant 01 (name check removed).
    """
    listener, dialer = two
    dialer.ledger.upsert_peer(_other_peer("b", direction, revoked=revoked))
    text, code = listener.paste()
    with pytest.raises(ConflictError) as info:
        dialer.pair(text, name="b")
    mine = dialer.identity.public_b64()
    assert listener.entries_of(mine) == [], listener.entries_of(mine)
    assert listener.routes() == [], listener.routes()
    assert info.value.details.get("name") == "b" and info.value.details.get("code_used") is False
    assert "not used" in info.value.message and "--name" in info.value.message
    assert listener.ledger.pair_code(code.code_id, time.time()) is not None  # the code is still open

    peer = dialer.pair(text, name="b-server")
    assert peer.name == "b-server" and peer.pubkey == listener.identity.public_b64()
    [entry] = listener.entries_of(mine)
    assert entry.revoked_at is None and entry.peer_id == peer.peer_id


def _racing(dialer: _Dialer, clash: Peer):
    """A ``WireClient`` factory whose redeem lets another process of the dialer's profile take the name first."""

    def factory(peer: Peer) -> WireClient:
        client = dialer.client(peer)
        real = client.redeem

        def redeem(body: dict) -> dict:
            dialer.ledger.upsert_peer(clash)
            return real(body)

        client.redeem = redeem
        return client

    return factory


def test_a_name_taken_while_redeeming_is_withdrawn_on_the_listener(two):
    """★ F05, race: the name was free when checked but another process stored a peer ``b`` before this one could —
    the dialer takes the redeem back on the listener (signed ``POST /pair/revoke`` as the new peer), so neither side
    keeps a half pairing; the error says so.

    Red proof: on ``c31da4c3`` the ``ConflictError`` of ``upsert_peer`` left the listener's entry active. Mutant 02
    (no withdrawal after a failed store).
    """
    listener, dialer = two
    clash = _other_peer("b", "dial")
    text, _code = listener.paste()
    with pytest.raises(ConflictError) as info:
        dialer.pair(text, name="b", factory=_racing(dialer, clash))
    [entry] = listener.entries_of(dialer.identity.public_b64())
    assert entry.revoked_at is not None, "the listener still holds a paired entry the dialer does not know"
    assert "pair/revoke" in listener.routes()
    assert info.value.details.get("listener_entry") == "withdrawn"
    assert "withdrawn" in info.value.message and "new code" in info.value.message
    assert [p.peer_id for p in dialer.ledger.peers(include_revoked=True)] == [clash.peer_id]
    # inbound ``auto``: no paired peer and no open code left, so the listener's routes are closed again (F05)
    closed = listener.transport.handle_request(httpx.Request("GET", f"{URL}{API_PREFIX}/v1/hello"))
    assert closed.status_code == 404


class _RevokeFails(httpx.BaseTransport):
    """The listener for every route, except that ``pair/revoke`` is unreachable or answered without ``revoked``."""

    def __init__(self, listener: _Listener, how: str) -> None:
        self.listener = listener
        self.how = how

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        if not request.url.path.endswith("/pair/revoke"):
            return self.listener.transport.handle_request(request)
        if self.how == "unreachable":
            raise httpx.ConnectError("listener gone", request=request)
        body = json.dumps({"ok": True}).encode()  # signed and 200, but no ``revoked: true``
        nonce = request.headers["x-ctxh-nonce"]
        sig = self.listener.identity.sign(response_string(200, nonce, hashlib.sha256(body).hexdigest()))
        return httpx.Response(200, headers={H_RESP_SIG: sig, "content-type": "application/json"}, content=body)


@pytest.mark.parametrize("how", ["unreachable", "unconfirmed"])
def test_a_withdrawal_the_listener_does_not_confirm_is_reported(two, how):
    """★ Erfolgs-Sweep: the withdrawal counts only when the listener answers ``{"revoked": true}``. An
    unreachable listener or an answer without it ⇒ the error names the entry left on the listener and how to revoke
    it there (``listener_entry: kept``) — never a silent "nothing is paired".

    Red proof: on ``c31da4c3`` no withdrawal was attempted and ``listener_entry`` was missing. Mutant 03 (any answer
    counts as withdrawn) → the ``unconfirmed`` case reports ``withdrawn``.
    """
    listener, dialer = two
    text, _code = listener.paste()
    transport = _RevokeFails(listener, how)
    clash = _other_peer("b", "inbound")

    def factory(peer: Peer) -> WireClient:
        client = dialer.client(peer, transport)
        real = client.redeem

        def redeem(body: dict) -> dict:
            dialer.ledger.upsert_peer(clash)
            return real(body)

        client.redeem = redeem
        return client

    with pytest.raises(ConflictError) as info:
        dialer.pair(text, name="b", factory=factory)
    assert info.value.details.get("listener_entry") == "kept"
    assert "/handover peers revoke" in info.value.message and "server" in info.value.message
    [entry] = listener.entries_of(dialer.identity.public_b64())
    assert entry.revoked_at is None  # the report matches the listener's real state


def test_without_a_name_the_dialer_picks_a_free_one_like_the_listener(two):
    """★ Without ``--name`` the dialer names the listener after its paste-string name and, when that is taken (the
    usual two-sided pairing: B already paired with this device as ``server``), picks the next free name the way the
    listener does (``_free_name``) instead of failing.

    Red proof: on ``c31da4c3`` the default name ran into ``ConflictError`` after the redeem. Mutant 04 (the default
    name refused like an explicit one).
    """
    listener, dialer = two
    dialer.ledger.upsert_peer(_other_peer("server", "inbound", pubkey=listener.identity.public_b64()))
    text, _code = listener.paste()
    peer = dialer.pair(text, name="")
    assert peer.name == "server-2" and peer.direction == "dial"
    [entry] = listener.entries_of(dialer.identity.public_b64())
    assert entry.revoked_at is None and entry.peer_id == peer.peer_id


def test_bundle_origin_is_the_established_peer_not_a_later_duplicate(tmp_path):
    """★ F05 (Bündel-Herkunft): B holds A's key three times — ``a`` (inbound, B's listener named A), ``gottz``
    (dial, B dials A) and ``probe-x`` (a later inbound duplicate, as the failed pairing left it). A bundle A signed
    is attributed to the dial peer (the one with a way back, V13), else to the oldest entry — never to the newest
    duplicate because its name sorts last.

    Red proof: on ``c31da4c3`` ``_trusted_keys`` kept the last name in name order ⇒ ``probe-x`` in both cases.
    Mutant 05 (name order restored).
    """
    exp = Exporter(tmp_path)
    out, *_ = make_bundle(tmp_path, exp)
    importer = make_ledger(tmp_path / "importer" / "ledger.sqlite3")
    key = exp.identity.public_b64()
    t0 = time.time() - 3600
    importer.upsert_peer(_other_peer("a", "inbound", pubkey=key, at=t0))
    importer.upsert_peer(_other_peer("gottz", "dial", pubkey=key, at=t0 + 60))
    importer.upsert_peer(_other_peer("probe-x", "inbound", pubkey=key, at=t0 + 120))
    src = BundleSource(out, trusted_pubkeys=set(), trust_unsigned=False, ledger=importer)
    assert (src.verified, src.signer) == (True, "gottz")
    src.close()
    assert importer.remove_peer("gottz")
    src = BundleSource(out, trusted_pubkeys=set(), trust_unsigned=False, ledger=importer, again=True)
    assert (src.verified, src.signer) == (True, "a")
    src.close()
