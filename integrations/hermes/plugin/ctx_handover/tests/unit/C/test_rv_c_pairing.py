"""First review round — pairing and the dashboard-gate credential (the first review §2 H1; M14, M32).

H1: pairing sent the stored dashboard credential with the first request to the URL of any paste string, before the
fingerprint was compared, and every peer shared one credential slot. Now a credential only ever goes to the address
it was stored for (``pairing.credential_owner``), the first ``hello`` carries no gate header and must be signed over
its nonce by the paste string's key (``wire.WireClient._first_hello``), and a pairing copies the credential into
variables of that peer alone (``pairing.store_credentials``, ``CTX_HANDOVER_PEER_TOKEN_<peer id>`` …).

Listeners run in process (real Ledger, ParticipantRegistry and Server) behind an imitation of the dashboard gate;
the dialer uses the real ``wire.WireClient``. ``Net`` records every request the dialer sends, with headers and body.
"""

from __future__ import annotations

import hashlib
import json
import os
import time

import httpx
import pytest

from ctx_handover.engine import pairing
from ctx_handover.engine.crypto import H_RESP_SIG, b64e, response_string
from ctx_handover.engine.errors import BadResponseSignature, ConflictError, EnvNotWritable, PeerUnreachable
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import GateAuth, GateMode, Peer
from ctx_handover.engine.server import PeerSignatureAuth, Server
from ctx_handover.engine.wire import MIB, PROBE_SIZES_MIB, WireClient
from ctx_handover.tests.unit.C._l2r_helpers import ALL_RIGHTS, InProcessTransport
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

SLOT = "CTX_HANDOVER_PEER_TOKEN"  # the settings form's token slot (``peer_token``)
PASSWORD_SLOT = "CTX_HANDOVER_PEER_PASSWORD"  # env variable name
TOKEN = GateAuth(mode=GateMode.SESSION_TOKEN)
BEARER = GateAuth(mode=GateMode.BEARER)
BASIC = GateAuth(mode=GateMode.BASIC_LOGIN, username="me")
HEADER = GateAuth(mode=GateMode.SESSION_TOKEN, extra_headers=(("X-Proxy-Key", "PROXY_KEY"),))
HELLO = "/api/plugins/ctx_handover/v1/hello"


class Listener:
    """An unpaired listener with its own key, served in process (no gate; wrap ``app`` in ``Gate`` for one)."""

    def __init__(self, tmp_path, name: str) -> None:
        from ctx_handover.engine.participant import ParticipantRegistry

        self.name = name
        self.ledger = make_ledger(tmp_path / name / "ledger.sqlite3")
        self.identity = new_identity(self.ledger)
        registry = ParticipantRegistry(self.ledger, lambda role, peer: None)
        server = Server(
            self.ledger, self.identity, registry, {"name": name}, [PeerSignatureAuth(self.ledger, self.identity)]
        )
        self.app = InProcessTransport(server)

    def paste(self, url: str, *, fp: str | None = None, profile: str | None = None) -> str:
        code = pairing.create_code(self.ledger, rights=ALL_RIGHTS, name_hint="laptop", ttl_s=600)
        return pairing.pair_string(
            code, url=url, fingerprint=fp or self.identity.fingerprint(), name=self.name, profile=profile
        )

    def inbound(self) -> list[Peer]:
        """Active inbound entries (devices this listener considers paired)."""
        return [p for p in self.ledger.peers(include_revoked=True) if p.direction == "inbound" and p.revoked_at is None]


class Gate(httpx.BaseTransport):
    """A dashboard gate in front of ``inner`` (``hermes_cli/dashboard_auth/middleware.py``): an unsigned 401 unless
    the request carries ``token`` as session token or bearer — ``token=None`` takes any credential (a harvester) —
    and, with ``header``, that proxy header. ``/auth/password-login`` answers the access-token cookie for ``basic``
    (user, password); a harvester accepts any login."""

    def __init__(self, inner, token: str | None = None, *, basic=None, header=None) -> None:
        self.inner, self.token, self.basic, self.header = inner, token, basic, header

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/auth/password-login"):
            doc = json.loads(request.read() or b"{}")
            if self.token is None or (doc.get("username"), doc.get("password")) == self.basic:
                cookie = f"hermes_session_at={self.token or 'harvested'}; Path=/"
                return httpx.Response(200, headers={"set-cookie": cookie}, json={"ok": True})
            return httpx.Response(401, json={"error": "invalid_credentials"})
        sent = request.headers.get("x-hermes-session-token") or ""
        auth = request.headers.get("authorization", "")
        if auth.lower().startswith("bearer "):
            sent = sent or auth[7:]
        ok = bool(sent) and (self.token is None or sent == self.token)
        if self.header is not None and request.headers.get(self.header[0]) != self.header[1]:
            ok = False
        if not ok:
            return httpx.Response(401, json={"error": "session_expired", "detail": "Unauthorized"})
        return self.inner.handle_request(request)


class Net(httpx.BaseTransport):
    """What the dialer reaches: one transport per host name; every request is logged (host, path, headers, body)."""

    def __init__(self) -> None:
        self.hosts: dict[str, httpx.BaseTransport] = {}
        self.log: list[tuple[str, str, dict[str, str], bytes]] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        body = request.read()
        headers = {k.lower(): v for k, v in request.headers.items()}
        self.log.append((request.url.host, request.url.path, headers, body))
        return self.hosts[request.url.host].handle_request(request)

    def to(self, host: str) -> list[str]:
        return [path for h, path, _headers, _body in self.log if h == host]

    def carrying(self, host: str, secret: str) -> list[str]:
        """Paths of the requests to ``host`` that carried ``secret`` in a header or in the body."""
        return [
            path
            for h, path, headers, body in self.log
            if h == host and (any(secret in v for v in headers.values()) or secret.encode() in body)
        ]


class Env(dict):
    """The dialer's profile ``.env``: ``read`` like ``hostcompat.read_secret`` (empty = unset)."""

    def read(self, name: str) -> str | None:
        value = self.get(name)
        return value or None

    def store(self, *, refuse: str = "") -> pairing.SecretStore:
        """``hostcompat`` stand-in; names starting with ``refuse`` are silently not written (a managed ``.env``)."""

        def save(name: str, value: str) -> None:
            if not (refuse and name.startswith(refuse)):
                self[name] = value

        return pairing.SecretStore(read=self.read, save=save, read_fresh=self.read, writable=lambda name: None)


class Dialer:
    def __init__(self, tmp_path, net: Net) -> None:
        self.ledger = make_ledger(tmp_path / "dialer" / "ledger.sqlite3")
        self.identity = new_identity(self.ledger)
        self.env = Env()
        self.net = net

    def client(self, peer: Peer, **kw) -> WireClient:
        kw.setdefault("max_attempts", 1)
        kw.setdefault("transport", self.net)
        return WireClient(peer, self.identity, read_secret=self.env.read, sleep=lambda s: None, **kw)

    def pair(self, text: str, *, gate: GateAuth, name: str = "", per_peer: bool = True, factory=None, store=None):
        extra = {"secret_store": store or self.env.store()} if per_peer else {}
        return pairing.redeem_dialer(
            factory or self.client,
            self.identity,
            pairing.parse_pair_string(text),
            name=name,
            gate=gate,
            allow_insecure=False,
            ledger=self.ledger,
            **extra,
        )


def _setup(tmp_path, *hosts: str) -> tuple[Net, Dialer, dict[str, Listener]]:
    net = Net()
    listeners = {h: Listener(tmp_path, h.split(".")[0]) for h in hosts}
    for host, lst in listeners.items():
        net.hosts[host] = lst.app
    return net, Dialer(tmp_path, net), listeners


# ---------------------------------------------------------------------------------------------------------
# H1 (a): nothing secret before the paste string's key is proven
# ---------------------------------------------------------------------------------------------------------


def test_paste_url_gets_no_credential_before_its_key_is_proven(tmp_path):
    """★ H1, the reviewer's scenario (probe ``test_probe_gate_token_reaches_paste_url_before_fingerprint_check``):
    a paste string names the attacker's URL and a fingerprint the attacker cannot match. The dialer must refuse on
    the fingerprint without the stored dashboard credential ever reaching that URL.

    Red on ``08cfe0f0``: the first request ``GET …/v1/hello`` to ``attacker.example`` carried
    ``x-hermes-session-token: SECRET-OF-MY-SERVER`` (``wire.py:366`` adds the gate to every request). Mutant 08
    (the first hello with the gate header): red the same way.
    """
    net, d, lst = _setup(tmp_path, "attacker.example")
    d.env[SLOT] = "SECRET-OF-MY-SERVER"
    text = lst["attacker.example"].paste("https://attacker.example", fp="0" * 16)
    with pytest.raises(BadResponseSignature) as info:
        d.pair(text, gate=TOKEN, per_peer=False)
    assert "fingerprint" in info.value.message
    assert net.carrying("attacker.example", "SECRET-OF-MY-SERVER") == [], net.log
    assert net.to("attacker.example") == [HELLO]
    assert d.ledger.peers(include_revoked=True) == []


class Relay(httpx.BaseTransport):
    """Answers with a real listener's responses, signed by another key: a relay that copies a listener's hello
    (and so its key and fingerprint) without holding the key."""

    def __init__(self, inner, impostor) -> None:
        self.inner, self.impostor = inner, impostor

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        resp = self.inner.handle_request(request)
        nonce = request.headers.get("x-ctxh-nonce", "")
        msg = response_string(resp.status_code, nonce, hashlib.sha256(resp.content).hexdigest())
        headers = {**dict(resp.headers), H_RESP_SIG: self.impostor.sign(msg)}
        return httpx.Response(resp.status_code, headers=headers, content=resp.content)


def test_a_relay_of_the_listener_hello_gets_no_credential(tmp_path):
    """★ H1: the paste string's fingerprint is checked against a key the responder has to prove — the hello must be
    signed over this request's nonce by that key. A relay serving a copy of the real listener's hello (right key,
    right fingerprint, its own signature) is refused before any credential leaves.

    Red on ``08cfe0f0``: the unsigned hello of a not yet pinned peer was taken as it came; the fingerprint matched
    and ``hello`` and ``pair/redeem`` went to the relay with ``x-hermes-session-token``. Mutant 09 (``_prove``
    accepts any signature): ``pair/redeem`` carried the credential.
    """
    net, d, lst = _setup(tmp_path, "server.example")
    real = lst["server.example"]
    impostor = new_identity(make_ledger(tmp_path / "impostor" / "ledger.sqlite3"))
    net.hosts["server.example"] = Relay(real.app, impostor)
    d.env[SLOT] = "SECRET-OF-MY-SERVER"
    with pytest.raises(BadResponseSignature) as info:
        d.pair(real.paste("https://server.example"), gate=TOKEN, per_peer=False)
    assert net.carrying("server.example", "SECRET-OF-MY-SERVER") == []
    assert net.to("server.example") == [HELLO]
    assert "did not prove" in info.value.message
    assert real.inbound() == []


def test_a_gated_listener_gets_the_credential_after_the_gate_refused_the_bare_hello(tmp_path):
    """A listener behind the dashboard gate: the bare hello meets the gate (unsigned 401), the hello is repeated
    with the credential, and pairing completes; the signed hello of the stored peer passes the gate."""
    net, d, lst = _setup(tmp_path, "server.example")
    net.hosts["server.example"] = Gate(lst["server.example"].app, "TOKEN-S")
    d.env[SLOT] = "TOKEN-S"
    peer = d.pair(lst["server.example"].paste("https://server.example"), gate=TOKEN, per_peer=False)
    hellos = [headers.get("x-hermes-session-token") for _h, path, headers, _b in net.log if path == HELLO]
    assert hellos[:2] == [None, "TOKEN-S"], hellos
    assert d.client(d.ledger.peer(peer.name)).hello(signed=True).peer.peer_id == peer.peer_id


# ---------------------------------------------------------------------------------------------------------
# H1 (a): a credential stored for one address never reaches another
# ---------------------------------------------------------------------------------------------------------

_MODES = {
    # mode: (gate, the server's gate transport kwargs, .env for the server, .env changes before the second pairing)
    "token": (TOKEN, {"token": "TOKEN-S"}, {SLOT: "TOKEN-S"}, {}),
    "bearer": (BEARER, {"token": "TOKEN-S"}, {SLOT: "TOKEN-S"}, {}),
    "basic": (BASIC, {"token": "ACCESS-S", "basic": ("me", "PASSWORD-S")}, {PASSWORD_SLOT: "PASSWORD-S"}, {}),
    "header": (
        HEADER,
        {"token": "TOKEN-S", "header": ("X-Proxy-Key", "PROXY-S")},
        {SLOT: "TOKEN-S", "PROXY_KEY": "PROXY-S"},
        {SLOT: "TOKEN-NEW"},  # a fresh dashboard token, but the proxy header value is still the server's
    ),
}


@pytest.mark.parametrize("per_peer", [False, True], ids=["slot", "per-peer"])
@pytest.mark.parametrize("mode", sorted(_MODES))
def test_a_credential_stored_for_one_address_never_reaches_another(tmp_path, mode, per_peer):
    """★ H1, second half of the scenario: the laptop is paired with server S (behind its dashboard gate). A paste
    string of another machine (a second server, a colleague, a planted string) whose gate takes any credential must
    not receive S's credential — not the token, not the bearer, not the Basic password (no login POST either), not
    a proxy header value. ``slot``: S's entry names the settings slot itself (no ``secret_store``: the variable is
    bound); ``per-peer``: S's credential was copied to S's own variables and the slot still holds the same value
    (the value is bound). Nothing reaches the other machine and its code stays unused.

    Red on ``08cfe0f0``: ``slot`` — ``hello`` and ``pair/redeem`` to ``other.example`` carried S's credential
    (``basic``: ``POST /auth/password-login`` with S's password); ``per-peer`` — no ``secret_store`` parameter.
    Mutant 02 (``credential_owner`` returns None): ``slot`` and ``per-peer`` red. Mutant 03 (value comparison
    removed): ``per-peer`` red.
    """
    gate, gate_kw, env, change = _MODES[mode]
    net, d, lst = _setup(tmp_path, "server.example", "other.example")
    net.hosts["server.example"] = Gate(lst["server.example"].app, **gate_kw)
    net.hosts["other.example"] = Gate(lst["other.example"].app, None)  # takes any credential
    d.env.update(env)
    server = d.pair(lst["server.example"].paste("https://server.example"), gate=gate, per_peer=per_peer)
    d.env.update(change)
    with pytest.raises(ConflictError) as info:
        d.pair(lst["other.example"].paste("https://other.example"), gate=gate, per_peer=per_peer)
    assert net.to("other.example") == [], net.to("other.example")
    assert lst["other.example"].inbound() == []
    details = info.value.details
    assert details["peer"] == server.name and details["code_used"] is False
    if mode == "header" and per_peer:
        assert details["variable"] == "PROXY_KEY"
    assert "nothing was sent" in info.value.message and "https://server.example" in info.value.message
    # the server's own pairing is untouched and still works (an entry naming the slot itself reads what the slot
    # holds now — the one-slot problem that per-peer variables remove)
    if per_peer or not change:
        stored = d.ledger.peer(server.name)
        assert d.client(stored).hello(signed=True).peer.peer_id == server.peer_id


def test_one_dashboard_may_share_a_credential(tmp_path):
    """Two profiles behind one dashboard (one origin, other ``profile``) share its credential: the address the
    credential was stored for is the origin (scheme, host, port), so the second pairing is not refused."""
    net, d, lst = _setup(tmp_path, "server.example")
    net.hosts["server.example"] = Gate(lst["server.example"].app, "TOKEN-S")
    d.env[SLOT] = "TOKEN-S"
    first = d.pair(lst["server.example"].paste("https://server.example"), gate=TOKEN)
    second = d.pair(lst["server.example"].paste("https://server.example:443", profile="work"), gate=TOKEN)
    assert first.name != second.name and second.profile == "work"
    assert d.env[second.gate.token_env] == "TOKEN-S"


# ---------------------------------------------------------------------------------------------------------
# H1 (b): one credential per peer
# ---------------------------------------------------------------------------------------------------------

_PER_PEER = {
    "token": (TOKEN, lambda s: {"token": f"TOKEN-{s}"}, lambda s: {SLOT: f"TOKEN-{s}"}),
    "basic": (
        BASIC,
        lambda s: {"token": f"ACCESS-{s}", "basic": ("me", f"PASSWORD-{s}")},
        lambda s: {PASSWORD_SLOT: f"PASSWORD-{s}"},
    ),
    "header": (
        HEADER,
        lambda s: {"token": f"TOKEN-{s}", "header": ("X-Proxy-Key", f"PROXY-{s}")},
        lambda s: {SLOT: f"TOKEN-{s}", "PROXY_KEY": f"PROXY-{s}"},
    ),
}


@pytest.mark.parametrize("mode", sorted(_PER_PEER))
def test_each_peer_sends_only_its_own_credential(tmp_path, mode):
    """★ H1 (b): every paired peer keeps its own credential in variables of its own
    (``CTX_HANDOVER_PEER_TOKEN_<peer id>``, ``…_PASSWORD_<peer id>``, ``…_HEADER1_<peer id>``); the settings slot can
    take the next peer's credential without changing what earlier peers send. Two servers with different tokens
    (passwords, proxy keys) both keep working after the slot holds a third value, and neither ever sees the
    other's credential.

    Red on ``08cfe0f0``: no ``secret_store`` parameter — one slot for all peers, S's next request carried whatever
    the slot held. Mutant 04 (``store_credentials`` returns the gate unchanged, entries keep naming the slot): the
    second pairing is refused, ``ConflictError`` "the dashboard token in CTX_HANDOVER_PEER_TOKEN is the one stored
    for s".
    """
    gate, gate_kw, env_of = _PER_PEER[mode]
    net, d, lst = _setup(tmp_path, "s.example", "t.example")
    net.hosts["s.example"] = Gate(lst["s.example"].app, **gate_kw("S"))
    net.hosts["t.example"] = Gate(lst["t.example"].app, **gate_kw("T"))
    d.env.update(env_of("S"))
    s_peer = d.pair(lst["s.example"].paste("https://s.example"), gate=gate)
    d.env.update(env_of("T"))
    t_peer = d.pair(lst["t.example"].paste("https://t.example"), gate=gate)
    d.env.update(env_of("NEXT"))  # the form now holds the credential for a third pairing

    for peer, own in ((s_peer, "S"), (t_peer, "T")):
        stored = d.ledger.peer(peer.name)
        assert stored.gate == peer.gate
        names = [env for _what, env in pairing.gate_secrets(stored.gate)]
        assert names and all(env.endswith("_" + peer.peer_id) and env.startswith("CTX_HANDOVER_PEER_") for env in names)
        assert sorted(d.env[env] for env in names) == sorted(env_of(own).values())
        assert stored.gate.username == gate.username
        assert d.client(stored).hello(signed=True).peer.peer_id == peer.peer_id
    for host, foreign in (("s.example", ("T", "NEXT")), ("t.example", ("S", "NEXT"))):
        for who in foreign:
            for value in env_of(who).values():
                assert net.carrying(host, value) == [], (host, value)


def test_a_refused_credential_copy_leaves_nothing_paired(tmp_path):
    """The copy into the peer's own variable is proven by reading it back (R16). A ``.env`` that silently refuses
    the write ⇒ ``EnvNotWritable``; the listener's entry is withdrawn and this device stores nothing."""
    _net, d, lst = _setup(tmp_path, "server.example")
    d.env[SLOT] = "TOKEN-S"
    with pytest.raises(EnvNotWritable) as info:
        d.pair(
            lst["server.example"].paste("https://server.example"),
            gate=TOKEN,
            store=d.env.store(refuse=pairing.PEER_TOKEN_PREFIX),
        )
    assert info.value.details.get("listener_entry") == "withdrawn"
    assert d.ledger.peers(include_revoked=True) == []
    assert lst["server.example"].inbound() == []
    assert not [k for k in d.env if k.startswith(pairing.PEER_TOKEN_PREFIX)]


# ---------------------------------------------------------------------------------------------------------
# Found while fixing H1: the listener's peer id must not replace another entry
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("direction", ["dial", "inbound"])
def test_a_listener_cannot_take_over_another_peers_entry(tmp_path, monkeypatch, direction):
    """★ The dialer stores the peer under the id the listener assigns, and ``upsert_peer`` keys on that id. A
    listener answering with the id of an entry this device already has (a peer it dials, or a device that paired
    with it) would replace that entry — its URL, key and gate (with per-peer credentials: also its variables).
    Refused; the existing entry is unchanged and the listener's entry is withdrawn.

    Red on ``08cfe0f0``: no error — the existing entry was overwritten (URL, key, direction, name of the new
    listener). Mutant 10 (the id check removed): red the same way.
    """
    _net, d, lst = _setup(tmp_path, "server.example")
    existing = Peer(
        name="old",
        peer_id=ulid(),
        direction=direction,
        url="https://old.example" if direction == "dial" else None,
        pubkey=b64e(os.urandom(32)),
        rights=ALL_RIGHTS,
        gate=GateAuth(mode=GateMode.SESSION_TOKEN, token_env="OLD_TOKEN") if direction == "dial" else None,
        created_at=time.time(),
    )
    d.ledger.upsert_peer(existing)
    monkeypatch.setattr(pairing, "ulid", lambda: existing.peer_id)  # the listener's redeem assigns this id
    d.env[SLOT] = "TOKEN-S"
    with pytest.raises(BadResponseSignature) as info:
        d.pair(lst["server.example"].paste("https://server.example"), gate=TOKEN, per_peer=False)
    after = d.ledger.peer(existing.peer_id)
    assert (after.name, after.direction, after.url, after.pubkey, after.gate) == (
        existing.name,
        existing.direction,
        existing.url,
        existing.pubkey,
        existing.gate,
    )
    assert info.value.details.get("listener_entry") == "withdrawn"
    assert lst["server.example"].inbound() == []


# ---------------------------------------------------------------------------------------------------------
# M14: a lost redeem answer is not repeated
# ---------------------------------------------------------------------------------------------------------


class DropFirstRedeemAnswer(httpx.BaseTransport):
    """The listener processes the first ``pair/redeem``, then the connection drops before the answer arrives."""

    def __init__(self, inner) -> None:
        self.inner, self.dropped, self.routes = inner, False, []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        resp = self.inner.handle_request(request)
        self.routes.append(request.url.path.rsplit("/v1/", 1)[-1])
        if request.url.path.endswith("/pair/redeem") and not self.dropped:
            self.dropped = True
            raise httpx.ReadError("connection reset after the listener answered")
        return resp


def test_a_lost_redeem_answer_is_not_repeated(tmp_path):
    """★ M14: the listener burns the code with the first redeem it processes; a repeat could only fail with
    ``pair_invalid`` and hide that the listener has paired this device. One attempt; the error says the listener
    may list this device (``details.listener_entry = "unknown"``) and how to clean up.

    Red on ``08cfe0f0``: ``SEEN [hello, pair/redeem, pair/redeem]`` and ``PairInvalid`` (retry with up to six
    attempts, ``wire.py:395-412``).
    """
    net, d, lst = _setup(tmp_path, "server.example")
    drop = DropFirstRedeemAnswer(lst["server.example"].app)
    net.hosts["server.example"] = drop
    d.env[SLOT] = "TOKEN-S"
    with pytest.raises(PeerUnreachable) as info:
        d.pair(
            lst["server.example"].paste("https://server.example"),
            gate=TOKEN,
            per_peer=False,
            factory=lambda peer: d.client(peer, max_attempts=6),
        )
    assert drop.routes.count("pair/redeem") == 1, drop.routes
    assert info.value.details.get("listener_entry") == "unknown"
    assert "may have paired this device" in info.value.message and "/handover peers" in info.value.message
    assert d.ledger.peers(include_revoked=True) == []


# ---------------------------------------------------------------------------------------------------------
# M32: ``doctor --measure`` sends each probe size once
# ---------------------------------------------------------------------------------------------------------


class BodyLimit(httpx.BaseTransport):
    """A proxy that answers an unsigned 504 for request bodies over ``limit`` and counts the bytes it was sent."""

    def __init__(self, inner, limit: int) -> None:
        self.inner, self.limit, self.sent = inner, limit, 0

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        body = request.read()
        self.sent += len(body)
        if len(body) > self.limit:
            return httpx.Response(504, text="<html>gateway timeout</html>")
        return self.inner.handle_request(request)


def test_measure_sends_each_probe_size_once(tmp_path):
    """★ M32: ``probe_gate(measure_limit=True)`` sends every size of ``PROBE_SIZES_MIB`` at most once — at most their
    sum (15 MiB) in total, as the README says — even when a proxy answers 5xx instead of 413.

    Red on ``08cfe0f0``: the 4 MiB probe was repeated up to ``max_attempts`` (6) times ⇒ 27 MiB with a 2 MiB proxy.
    """
    net, d, lst = _setup(tmp_path, "server.example")
    d.env[SLOT] = "TOKEN-S"
    peer = d.pair(lst["server.example"].paste("https://server.example"), gate=TOKEN, per_peer=False)
    limit = BodyLimit(lst["server.example"].app, 2 * MIB)
    net.hosts["server.example"] = limit
    report = d.client(d.ledger.peer(peer.name), max_attempts=6).probe_gate(measure_limit=True)
    assert report["body_limit"] == 2 * MIB and report["ok"] is None
    assert limit.sent <= sum(PROBE_SIZES_MIB) * MIB, limit.sent / MIB
