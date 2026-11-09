"""Pairing: one-time code on the listener, mutual HMAC proof, key pinning on both sides (layer L6, stdlib).

DESIGN §12.3 (R7, R17, SEC 1.3). The listener (operator verb on a local surface or the CLI) creates a code
``CTXH-<6>-<16>`` (80-bit secret, ≤ 60 min) and prints one paste string ``ctxh-pair:<base64url canonical JSON>``
with its URL, the code, the fingerprint of its public key, its name and profile — **never a gate credential**
(R17). The dialer checks the fingerprint against the unsigned ``hello``, proves knowledge of the secret with
``HMAC(secret, length-prefixed fields incl. sha256 of the canonical redeem body)``, and verifies the listener's
``server_proof``. The listener redeems in one ledger transaction (``DELETE … failures < 5 AND expires_at > now``
must hit exactly one row), so two processes of one profile can never redeem one code twice.

Codes, secrets and proofs are never logged (SEC 6.2).
"""

from __future__ import annotations

import json
import re
import secrets
import time
from collections.abc import Callable
from dataclasses import dataclass, replace
from typing import TYPE_CHECKING
from urllib.parse import urlsplit

from .crypto import Identity, b64d, b64e, consteq, fingerprint_of, length_prefixed, pair_proof, random_b64
from .errors import BadRequest, BadResponseSignature, PairInvalid
from .ids import CROCKFORD_ALPHABET, ULID_RE, ulid
from .jsonutil import canonical_bytes, loads_bounded, sha256_hex
from .model import GateAuth, Hello, Peer, Right

if TYPE_CHECKING:
    from .ledger import Ledger

PREFIX = "ctxh-pair:"
MAX_TTL_S = 3600
MIN_TTL_S = 60
DEFAULT_TTL_S = 600
MAX_PASTE_CHARS = 4096

REQ_TAG = "ctxh-pair-req-v1"
RESP_TAG = "ctxh-pair-resp-v1"

_CODE_RE = re.compile(r"CTXH-([0-9A-HJKMNP-TV-Z]{6})-([0-9A-HJKMNP-TV-Z]{16})")
_CODE_ID_RE = re.compile(r"[0-9A-HJKMNP-TV-Z]{6}")
_FP_RE = re.compile(r"[0-9a-f]{16}")
_B64_43 = re.compile(r"[A-Za-z0-9_-]{43}")
_B64_22 = re.compile(r"[A-Za-z0-9_-]{22}")
_PROFILE_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}")
_NAME_BAD = re.compile(r"[^A-Za-z0-9._-]+")
MAX_NAME = 64


@dataclass(frozen=True)
class PairCode:
    """A pending pairing code (listener side)."""

    code_id: str
    secret: str
    expires_at: float
    rights: frozenset[Right]
    name_hint: str

    @property
    def code(self) -> str:
        return f"CTXH-{self.code_id}-{self.secret}"


def _random_crockford(n: int) -> str:
    return "".join(secrets.choice(CROCKFORD_ALPHABET) for _ in range(n))


def canonical_rights(rights) -> str:
    """``push,pull`` order-independent text form used in proofs."""
    return ",".join(sorted(str(Right(r)) for r in rights))


def clean_name(text: object, fallback: str = "peer") -> str:
    """A peer name usable in commands: ``[A-Za-z0-9._-]``, ≤ 64 chars, starting alphanumeric."""
    name = _NAME_BAD.sub("-", str(text or "")).strip("-._")[:MAX_NAME]
    return name or fallback


# ---------------------------------------------------------------------------------------------------------
# Listener: code and paste string
# ---------------------------------------------------------------------------------------------------------


def create_code(
    ledger: Ledger,
    *,
    rights: frozenset[Right],
    name_hint: str,
    ttl_s: int,
    clock: Callable[[], float] = time.time,
) -> PairCode:
    """Store a new code (TTL clamped to 60 s … 60 min) and return it. The caller has created the identity."""
    rights = frozenset(Right(r) for r in rights)
    if not rights:
        raise BadRequest("a pairing code needs at least one right", field="rights")
    ttl = max(MIN_TTL_S, min(int(ttl_s), MAX_TTL_S))
    code = PairCode(
        code_id=_random_crockford(6),
        secret=_random_crockford(16),
        expires_at=clock() + ttl,
        rights=rights,
        name_hint=clean_name(name_hint, "") if name_hint else "",
    )
    ledger.put_pair_code(code.code_id, code.secret, code.rights, code.name_hint, code.expires_at)
    return code


def _clean_url(url: object) -> str:
    """A listener base URL fit for a paste string: http(s), a host, no userinfo, query or fragment (R17)."""
    if not isinstance(url, str) or len(url) > 1024:
        raise BadRequest("listener URL missing or too long", field="url")
    parts = urlsplit(url.strip())
    if parts.scheme not in {"http", "https"} or not parts.hostname:
        raise BadRequest("listener URL must be http(s)://host[:port][/prefix]", field="url")
    if parts.username is not None or parts.password is not None or "@" in parts.netloc:
        raise BadRequest("listener URL must not contain credentials", field="url")
    if parts.query or parts.fragment or url.strip().endswith(("?", "#")):
        raise BadRequest("listener URL must not contain a query or fragment", field="url")
    return url.strip().rstrip("/")


def pair_string(code: PairCode, *, url: str, fingerprint: str, name: str, profile: str | None) -> str:
    """``ctxh-pair:`` + base64url(canonical JSON ``{v, url, code, fp, name, profile}``) — never a gate secret."""
    if not _FP_RE.fullmatch(str(fingerprint)):
        raise BadRequest("invalid fingerprint", field="fp")
    if profile is not None and not _PROFILE_RE.fullmatch(str(profile)):
        raise BadRequest("invalid profile name", field="profile")
    doc = {
        "v": 1,
        "url": _clean_url(url),
        "code": code.code,
        "fp": fingerprint,
        "name": clean_name(name, "listener"),
        "profile": profile,
    }
    return PREFIX + b64e(canonical_bytes(doc))


def parse_pair_string(text: str) -> dict:
    """Strictly parse a paste string; ``BadRequest`` for anything unexpected."""
    if not isinstance(text, str):
        raise BadRequest("not a pairing string", field="pair")
    text = text.strip()
    if not text.startswith(PREFIX) or len(text) > MAX_PASTE_CHARS:
        raise BadRequest("not a ctxh-pair string", field="pair")
    try:
        raw = b64d(text[len(PREFIX) :])
    except ValueError:
        raise BadRequest("pairing string is not base64url", field="pair") from None
    doc = loads_bounded(raw, max_bytes=MAX_PASTE_CHARS, max_depth=4)
    if not isinstance(doc, dict) or set(doc) != {"v", "url", "code", "fp", "name", "profile"} or doc["v"] != 1:
        raise BadRequest("unsupported pairing string", field="pair")
    if not isinstance(doc["code"], str) or not _CODE_RE.fullmatch(doc["code"]):
        raise BadRequest("invalid pairing code", field="pair.code")
    if not isinstance(doc["fp"], str) or not _FP_RE.fullmatch(doc["fp"]):
        raise BadRequest("invalid fingerprint", field="pair.fp")
    if not isinstance(doc["name"], str) or not doc["name"] or len(doc["name"]) > MAX_NAME:
        raise BadRequest("invalid listener name", field="pair.name")
    profile = doc["profile"]
    if profile is not None and (not isinstance(profile, str) or not _PROFILE_RE.fullmatch(profile)):
        raise BadRequest("invalid profile", field="pair.profile")
    return {**doc, "url": _clean_url(doc["url"]), "name": clean_name(doc["name"], "listener")}


# ---------------------------------------------------------------------------------------------------------
# Proofs
# ---------------------------------------------------------------------------------------------------------


def _body_hash(body: dict) -> str:
    return sha256_hex(canonical_bytes({k: v for k, v in body.items() if k != "proof"}))


def _proof_message(body: dict, listener_instance_id: str) -> bytes:
    dialer = body["dialer"]
    return length_prefixed(
        REQ_TAG,
        body["code_id"],
        listener_instance_id,
        dialer["pubkey"],
        dialer["instance_id"],
        body["client_nonce"],
        _body_hash(body),
    )


def _server_message(peer_id: str, listener_pubkey: str, client_nonce: str, rights: str) -> bytes:
    return length_prefixed(RESP_TAG, peer_id, listener_pubkey, client_nonce, rights)


def redeem_body(
    identity: Identity,
    pair: dict,
    *,
    listener_instance_id: str,
    self_name: str,
    plugin_version: str,
    client_nonce: str | None = None,
) -> dict:
    """The dialer's ``POST /v1/pair/redeem`` body including its proof (DESIGN §12.3 step 4)."""
    match = _CODE_RE.fullmatch(pair["code"])
    if not match:
        raise BadRequest("invalid pairing code", field="pair.code")
    code_id, secret = match.groups()
    body = {
        "code_id": code_id,
        "dialer": {
            "name": clean_name(self_name, "dialer"),
            "instance_id": identity.instance_id,
            "pubkey": identity.public_b64(),
            "plugin_version": str(plugin_version)[:32],
        },
        "client_nonce": client_nonce or random_b64(16),
    }
    body["proof"] = pair_proof(secret, _proof_message(body, listener_instance_id))
    return body


def _check_redeem_body(body: object) -> dict:
    if not isinstance(body, dict) or set(body) != {"code_id", "dialer", "client_nonce", "proof"}:
        raise BadRequest("invalid redeem body", field="body")
    dialer = body["dialer"]
    if not isinstance(dialer, dict) or set(dialer) != {"name", "instance_id", "pubkey", "plugin_version"}:
        raise BadRequest("invalid dialer", field="dialer")
    checks = (
        ("code_id", body["code_id"], _CODE_ID_RE),
        ("client_nonce", body["client_nonce"], _B64_22),
        ("proof", body["proof"], _B64_43),
        ("dialer.instance_id", dialer["instance_id"], ULID_RE),
        ("dialer.pubkey", dialer["pubkey"], _B64_43),
    )
    for field, value, pattern in checks:
        if not isinstance(value, str) or not pattern.fullmatch(value):
            raise BadRequest("malformed field", field=field)
    for field in ("name", "plugin_version"):
        if not isinstance(dialer[field], str) or len(dialer[field]) > MAX_NAME:
            raise BadRequest("malformed field", field=f"dialer.{field}")
    try:
        if len(b64d(dialer["pubkey"])) != 32:
            raise ValueError
    except ValueError:
        raise BadRequest("malformed public key", field="dialer.pubkey") from None
    return body


def _free_name(ledger: Ledger, wanted: str) -> str:
    taken = {p.name for p in ledger.peers(include_revoked=True)}
    if wanted not in taken:
        return wanted
    for i in range(2, 1000):
        candidate = f"{wanted[: MAX_NAME - 5]}-{i}"
        if candidate not in taken:
            return candidate
    return f"{wanted[:40]}-{ulid()[-8:]}"


def redeem_listener(
    ledger: Ledger,
    identity: Identity,
    body: dict,
    client_ip: str | None,
    *,
    name: str = "",
    clock: Callable[[], float] = time.time,
) -> dict:
    """Listener side of ``POST /v1/pair/redeem`` (DESIGN §12.3 step 5).

    Unknown, expired or burned code, or a wrong proof ⇒ ``PairInvalid`` (a wrong proof also counts a failure;
    five burn the code). On success the dialer becomes an ``inbound`` peer in the same transaction that deletes
    the code; the response carries ``server_proof``. ``client_ip`` is stored for display only (SEC 1.9).
    """
    body = _check_redeem_body(body)
    now = clock()
    found = ledger.pair_code(body["code_id"], now)
    if found is None:
        raise PairInvalid("pairing code unknown, expired or used")
    secret, rights, name_hint = found
    expected = pair_proof(secret, _proof_message(body, identity.instance_id))
    if not consteq(expected, body["proof"]):
        ledger.fail_pair_code(body["code_id"])
        raise PairInvalid("pairing proof does not match")
    dialer = body["dialer"]
    peer = Peer(
        name=_free_name(ledger, clean_name(name_hint or dialer["name"], "dialer")),
        peer_id=ulid(),
        direction="inbound",
        url=None,
        pubkey=dialer["pubkey"],
        rights=frozenset(rights),
        created_at=now,
        last_seen_at=now,
        last_ip=client_ip,
    )
    if not ledger.redeem_pair_code(body["code_id"], now, peer):
        raise PairInvalid("pairing code unknown, expired or used")
    rights_text = canonical_rights(peer.rights)
    return {
        "peer_id": peer.peer_id,
        "listener": {
            "name": clean_name(name, "listener"),
            "instance_id": identity.instance_id,
            "pubkey": identity.public_b64(),
        },
        "rights": sorted(str(r) for r in peer.rights),
        "server_proof": pair_proof(
            secret, _server_message(peer.peer_id, identity.public_b64(), body["client_nonce"], rights_text)
        ),
    }


# ---------------------------------------------------------------------------------------------------------
# Dialer
# ---------------------------------------------------------------------------------------------------------


def redeem_dialer(
    client_factory: Callable[[Peer], object],
    identity: Identity,
    pair: dict,
    *,
    name: str,
    gate: GateAuth,
    allow_insecure: bool,
    ledger: Ledger,
    self_name: str | None = None,
    clock: Callable[[], float] = time.time,
) -> Peer:
    """Dialer side (DESIGN §12.3 steps 3–6): unsigned ``hello`` + fingerprint check, redeem with proof, verify
    ``server_proof``, pin the listener key, store the ``dial`` peer, then one signed ``hello``.

    ``client_factory(peer)`` returns an object with ``hello(signed=…) -> Hello`` and ``redeem(body) -> dict``
    (``wire.WireClient``). Gate failures surface as ``GateRejected`` from the client.
    """
    match = _CODE_RE.fullmatch(pair["code"])
    if not match:
        raise BadRequest("invalid pairing code", field="pair.code")
    secret = match.group(2)
    provisional = Peer(
        name=clean_name(name or pair["name"], "listener"),
        peer_id="",
        direction="dial",
        url=pair["url"],
        pubkey="",
        rights=frozenset(),
        gate=gate,
        profile=pair.get("profile"),
        allow_insecure=bool(allow_insecure),
    )
    client = client_factory(provisional)
    hello: Hello = client.hello(signed=False)
    try:
        fp = fingerprint_of(hello.listener_pubkey)
    except ValueError:
        fp = ""
    if not consteq(fp, pair["fp"]):
        raise BadResponseSignature(
            "the listener's key fingerprint does not match the pairing string; not paired (wrong URL or a "
            "machine in the middle)"
        )
    from . import PLUGIN_VERSION  # lazy: reading ../VERSION is I/O

    body = redeem_body(
        identity,
        pair,
        listener_instance_id=hello.instance_id,
        self_name=self_name if self_name is not None else _hostname(),
        plugin_version=PLUGIN_VERSION,
    )
    # The listener key is known now (fingerprint-checked): its signed answer to the redeem — success or a refusal
    # such as ``pair_invalid`` — is verified and read, never taken for an unsigned gate or proxy signal.
    resp = client_factory(replace(provisional, pubkey=hello.listener_pubkey)).redeem(body)
    try:
        peer_id = resp["peer_id"]
        listener = resp["listener"]
        rights = frozenset(Right(r) for r in resp["rights"])
        server_proof = resp["server_proof"]
        if not isinstance(peer_id, str) or not ULID_RE.match(peer_id) or not rights:
            raise ValueError
    except (KeyError, TypeError, ValueError):
        raise BadResponseSignature("malformed pairing response") from None
    expected = pair_proof(
        secret, _server_message(peer_id, hello.listener_pubkey, body["client_nonce"], canonical_rights(rights))
    )
    if (
        not consteq(expected, str(server_proof))
        or listener.get("pubkey") != hello.listener_pubkey
        or listener.get("instance_id") != hello.instance_id
    ):
        raise BadResponseSignature("the listener's pairing proof is invalid; not paired")
    now = clock()
    peer = Peer(
        name=provisional.name,
        peer_id=peer_id,
        direction="dial",
        url=pair["url"],
        pubkey=hello.listener_pubkey,
        rights=rights,
        gate=gate,
        profile=pair.get("profile"),
        allow_insecure=bool(allow_insecure),
        created_at=now,
        last_seen_at=now,
    )
    ledger.upsert_peer(peer)
    client_factory(peer).hello(signed=True)
    return peer


def _hostname() -> str:
    import socket

    return clean_name(socket.gethostname(), "dialer")


def describe(pair: dict) -> str:
    """One-line description of a parsed paste string without the code (for confirmations and logs)."""
    return json.dumps({k: pair[k] for k in ("url", "fp", "name", "profile")}, sort_keys=True)
