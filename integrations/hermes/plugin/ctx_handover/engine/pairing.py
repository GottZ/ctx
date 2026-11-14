"""Pairing: one-time code on the listener, mutual HMAC proof, key pinning on both sides (layer L6, stdlib).

DESIGN §12.3 (R7, R17, SEC 1.3). The listener (operator verb on a local surface or the CLI) creates a code
``CTXH-<6>-<16>`` (80-bit secret, ≤ 60 min) and prints one paste string ``ctxh-pair:<base64url canonical JSON>``
with its URL, the code, the fingerprint of its public key, its name and profile — **never a gate credential**
(R17). The dialer checks the fingerprint against the unsigned ``hello``, proves knowledge of the secret with
``HMAC(secret, length-prefixed fields incl. sha256 of the canonical redeem body)``, and verifies the listener's
``server_proof``. The listener redeems in one ledger transaction (``DELETE … failures < 5 AND expires_at > now``
must hit exactly one row), so two processes of one profile can never redeem one code twice.

Codes, secrets and proofs are never logged (SEC 6.2).

**Gate credentials (H1, D6a).** A credential is only ever sent to the address it was stored for. Before anything
leaves, ``credential_owner`` refuses a gate whose secret variable — or, with a ``SecretStore``, whose value — a
stored peer at another origin uses. The first ``hello`` goes out without any gate header; only when something in
front of the plugin refuses it does the credential follow (``wire.WireClient._first_hello``), and the answer must
be signed by the key of the paste string's fingerprint. A successful pairing copies the credential into variables
of this peer alone (``CTX_HANDOVER_PEER_TOKEN_<peer id>``, ``…_PASSWORD_<peer id>``, ``…_HEADER<n>_<peer id>``;
``store_credentials``), so the settings form's one credential slot can take the next peer's credential without
changing what earlier peers send.
"""

from __future__ import annotations

import contextlib
import json
import re
import secrets
import time
from collections.abc import Callable
from dataclasses import dataclass, replace
from typing import TYPE_CHECKING
from urllib.parse import urlsplit

from .crypto import Identity, b64d, b64e, consteq, fingerprint_of, length_prefixed, pair_proof, random_b64
from .errors import (
    BadRequest,
    BadResponseSignature,
    ConflictError,
    EnvNotWritable,
    GateRejected,
    HandoverError,
    PairInvalid,
    PeerUnreachable,
    RateLimited,
)
from .ids import CROCKFORD_ALPHABET, ULID_RE, ulid
from .jsonutil import canonical_bytes, loads_bounded, sha256_hex
from .model import GateAuth, GateMode, Hello, Peer, Right

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


@dataclass(frozen=True)
class SecretStore:
    """The profile ``.env`` as the dialer's pairing uses it for gate credentials (H1, D6a; catalog rule 11):
    ``read`` = ``hostcompat.read_secret`` (``get_secret``), ``save`` = ``hostcompat.save_env`` (the public Hermes
    writer), ``read_fresh`` = ``hostcompat.read_env_fresh`` (read-back right after the write, R16),
    ``writable`` = ``hostcompat.env_writable``."""

    read: Callable[[str], str | None]
    save: Callable[[str, str], None]
    read_fresh: Callable[[str], str | None]
    writable: Callable[[str], None]


PEER_TOKEN_PREFIX = "CTX_HANDOVER_PEER_TOKEN_"  # noqa: S105 - env variable name, not a secret
PEER_PASSWORD_PREFIX = "CTX_HANDOVER_PEER_PASSWORD_"  # noqa: S105 - env variable name, not a secret
PEER_HEADER_PREFIX = "CTX_HANDOVER_PEER_HEADER"
"""Per-peer ``.env`` variables a pairing writes: ``<prefix><peer id>`` (headers: ``<prefix><n>_<peer id>``)."""


def gate_secrets(gate: GateAuth | None) -> list[tuple[str, str]]:
    """``(what, env name)`` of every secret variable ``gate`` sends: the dashboard token (session token, bearer) or
    the Basic password, then one entry per extra proxy header — in this order, always."""
    if gate is None:
        return []
    out: list[tuple[str, str]] = []
    if gate.mode in (GateMode.SESSION_TOKEN, GateMode.BEARER):
        out.append(("dashboard token", gate.token_env))
    elif gate.mode == GateMode.BASIC_LOGIN:
        out.append(("dashboard password", gate.password_env))
    out.extend((f"value of header {name}", env) for name, env in gate.extra_headers)
    return out


def peer_gate(gate: GateAuth, peer_id: str) -> GateAuth:
    """``gate`` with each secret variable it sends renamed to this peer's own (``PEER_*_PREFIX`` + ``peer_id``)."""
    if not ULID_RE.match(str(peer_id)):
        raise BadRequest("invalid peer id", field="peer_id")
    return replace(
        gate,
        token_env=PEER_TOKEN_PREFIX + peer_id
        if gate.mode in (GateMode.SESSION_TOKEN, GateMode.BEARER)
        else gate.token_env,
        password_env=PEER_PASSWORD_PREFIX + peer_id if gate.mode == GateMode.BASIC_LOGIN else gate.password_env,
        extra_headers=tuple(
            (name, f"{PEER_HEADER_PREFIX}{i}_{peer_id}") for i, (name, _env) in enumerate(gate.extra_headers, 1)
        ),
    )


def store_credentials(store: SecretStore, gate: GateAuth, peer_id: str) -> GateAuth:
    """Copy every secret ``gate`` sends into this peer's own variables (``peer_gate``) and return that gate.

    Each value is read with ``store.read``, written with ``store.save`` and read back with ``store.read_fresh``
    (R16); a refused or differing write raises ``EnvNotWritable`` and blanks the variables already written."""
    fresh = peer_gate(gate, peer_id)
    written: list[str] = []
    try:
        for (what, old), (_what, new) in zip(gate_secrets(gate), gate_secrets(fresh), strict=True):
            value = store.read(old)
            if not value:
                raise GateRejected(
                    f"the {what} ({old}) is not set in this Hermes profile; enter it in Settings → Plugins → "
                    "ctx_handover",
                    variable=old,
                )
            store.writable(new)
            store.save(new, value)
            written.append(new)
            if store.read_fresh(new) != value:
                raise EnvNotWritable(
                    f"{new} could not be stored in this profile's .env (the write was refused or overridden); unlock "
                    f"the .env or set {new} through your secret manager",
                    variable=new,
                )
    except Exception:
        for name in written:
            with contextlib.suppress(Exception):
                store.save(name, "")
        raise
    return fresh


def own_secret_names(gate: GateAuth | None, peer_id: str) -> list[str]:
    """The variables ``gate`` sends that are this peer's own copies (``store_credentials``: ``PEER_*_PREFIX`` + its
    id), in ``gate_secrets`` order. A shared slot — the settings form's ``CTX_HANDOVER_PEER_TOKEN``, named by entries
    paired before per-peer copies — is never one; a gate-less entry or one without a ULID id has none."""
    if gate is None or not ULID_RE.match(str(peer_id)):
        return []
    own = {env for _what, env in gate_secrets(peer_gate(gate, peer_id))}
    return [env for _what, env in gate_secrets(gate) if env in own]


def forget_credentials(store: SecretStore, names: list[str]) -> list[str]:
    """Blank each of ``names`` the way the pairing wrote it (``peers remove``): ``store.writable``,
    ``store.save(name, "")``, then the read-back ``store.read_fresh`` (R16). Returns the names that are not proven
    blank — the write lock refused, the write raised, or the read-back still shows a value."""
    left: list[str] = []
    for name in names:
        try:
            store.writable(name)
            store.save(name, "")
            if store.read_fresh(name):
                left.append(name)
        except Exception:
            left.append(name)
    return left


def _origin(url: object) -> tuple[str, str, int] | None:
    """``(scheme, host, port)`` of a peer URL, or None when it has none."""
    try:
        parts = urlsplit(str(url or ""))
        if parts.scheme not in {"http", "https"} or not parts.hostname:
            return None
        return parts.scheme, parts.hostname.lower(), parts.port or (443 if parts.scheme == "https" else 80)
    except ValueError:
        return None


def credential_owner(
    ledger: Ledger, gate: GateAuth | None, url: str, read_secret: Callable[[str], str | None] | None = None
) -> tuple[Peer, str, str] | None:
    """``(peer, what, env name)`` when a stored dial peer at another origin sends one of ``gate``'s secrets: the
    same variable, or — with ``read_secret`` — the same value under another name (H1). A credential only ever goes
    to the address it was stored for; peers at one origin (scheme, host, port — other profiles or path prefixes of
    one dashboard) may share it. Revoked entries count: the credential still belongs to that address."""
    mine = gate_secrets(gate)
    if not mine:
        return None
    here = _origin(url)
    values = {}
    if read_secret is not None:
        values = {env: v for _what, env in mine if (v := read_secret(env))}
    for other in ledger.peers(include_revoked=True):
        if other.direction != "dial" or other.gate is None:
            continue
        there = _origin(other.url)
        if here is not None and there == here:
            continue
        theirs = [env for _what, env in gate_secrets(other.gate)]
        for what, env in mine:
            if env in theirs:
                return other, what, env
        if values:
            for env in theirs:
                value = read_secret(env) if read_secret is not None else None
                if not value:
                    continue
                for what, mine_env in mine:
                    if mine_env in values and consteq(values[mine_env], value):
                        return other, what, mine_env
    return None


def paired_gateway(ledger: Ledger, fp: str, profile: str | None = None) -> Peer | None:
    """The stored dial entry (revoked ones included) for the same gateway profile — listener key with fingerprint
    ``fp`` and the same ``profile`` (None = default): this device is paired with it already (L3-F25). Another profile
    keeps a ledger of its own, so a pairing there ends nothing here."""
    want = profile or "default"
    for other in ledger.peers(include_revoked=True):
        if other.direction != "dial" or not other.pubkey or (other.profile or "default") != want:
            continue
        try:
            theirs = fingerprint_of(other.pubkey)
        except ValueError:
            continue
        if consteq(theirs, str(fp or "")):
            return other
    return None


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
        raise BadRequest("a pairing code needs at least one right for the other machine", field="rights")
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
        raise BadRequest(
            "the address for the pairing line is missing or too long; check listen_url in the ctx_handover settings "
            "of the machine that made the pairing line",
            field="url",
        )
    parts = urlsplit(url.strip())
    if parts.scheme not in {"http", "https"} or not parts.hostname:
        raise BadRequest(
            "the address for the pairing line must look like https://host[:port][/path]; check listen_url in the "
            "ctx_handover settings of the machine that made the pairing line",
            field="url",
        )
    if parts.username is not None or parts.password is not None or "@" in parts.netloc:
        raise BadRequest(
            "the address for the pairing line must not contain a user name or password; check listen_url in the "
            "ctx_handover settings of the machine that made the pairing line",
            field="url",
        )
    if parts.query or parts.fragment or url.strip().endswith(("?", "#")):
        raise BadRequest(
            "the address for the pairing line must not contain ? or #; check listen_url in the ctx_handover settings "
            "of the machine that made the pairing line",
            field="url",
        )
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
        raise BadRequest("this is not a pairing line; copy the whole line that /handover pair printed", field="pair")
    text = text.strip()
    if not text.startswith(PREFIX) or len(text) > MAX_PASTE_CHARS:
        raise BadRequest(
            "this is not a pairing line (it starts with ctxh-pair:); copy the whole line that /handover pair printed",
            field="pair",
        )
    try:
        raw = b64d(text[len(PREFIX) :])
    except ValueError:
        raise BadRequest("the pairing line is damaged; copy the whole line again", field="pair") from None
    doc = loads_bounded(raw, max_bytes=MAX_PASTE_CHARS, max_depth=4)
    if not isinstance(doc, dict) or set(doc) != {"v", "url", "code", "fp", "name", "profile"} or doc["v"] != 1:
        raise BadRequest(
            "the pairing line has an unknown format (damaged, or made by another plugin version)", field="pair"
        )
    if not isinstance(doc["code"], str) or not _CODE_RE.fullmatch(doc["code"]):
        raise BadRequest("the pairing line is damaged (its pairing code is invalid)", field="pair.code")
    if not isinstance(doc["fp"], str) or not _FP_RE.fullmatch(doc["fp"]):
        raise BadRequest("the pairing line is damaged (its key fingerprint is invalid)", field="pair.fp")
    if not isinstance(doc["name"], str) or not doc["name"] or len(doc["name"]) > MAX_NAME:
        raise BadRequest("the pairing line is damaged (its machine name is invalid)", field="pair.name")
    profile = doc["profile"]
    if profile is not None and (not isinstance(profile, str) or not _PROFILE_RE.fullmatch(profile)):
        raise BadRequest("the pairing line is damaged (its profile name is invalid)", field="pair.profile")
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
        raise BadRequest("the pairing line is damaged (its pairing code is invalid)", field="pair.code")
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
        raise PairInvalid("the pairing code is unknown, expired or already used")
    secret, rights, name_hint = found
    expected = pair_proof(secret, _proof_message(body, identity.instance_id))
    if not consteq(expected, body["proof"]):
        ledger.fail_pair_code(body["code_id"])
        raise PairInvalid("the pairing code is wrong")
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
        raise PairInvalid("the pairing code is unknown, expired or already used")
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
    secret_store: SecretStore | None = None,
) -> Peer:
    """Dialer side (DESIGN §12.3 steps 3–6): unsigned ``hello`` + fingerprint check, redeem with proof, verify
    ``server_proof``, pin the listener key, store the ``dial`` peer, then one signed ``hello``.

    Gate credentials (H1): ``credential_owner`` refuses — before anything is sent — a gate whose secret a stored
    peer at another address uses (``ConflictError``, ``details.code_used=False``); with ``secret_store`` the values
    are compared too and a successful pairing stores the credential in this peer's own variables
    (``store_credentials``). The unsigned ``hello`` of the not yet pinned listener carries a gate header only when
    something in front of the plugin refuses it without one (``wire.WireClient._first_hello``). A listener that
    answers with a peer id this device already uses is refused (its entry would replace that peer's).

    Pairing is two-sided or nothing (L3-F05): the name for the listener is settled before the code is redeemed —
    an explicit ``name`` that is taken here ⇒ ``ConflictError`` with the code unused; without one the listener's
    paste-string name or, when taken, the next free one (``_free_name``, as the listener does). When the peer still
    cannot be stored after the redeem (another process of this profile took the name meanwhile), the redeem is
    taken back on the listener (``_withdraw``) and the error says whether that worked (``listener_entry``).

    ``client_factory(peer)`` returns an object with ``hello(signed=…) -> Hello``, ``redeem(body) -> dict`` and
    ``call(method, route, body) -> dict`` (``wire.WireClient``). Gate failures surface as ``GateRejected`` from
    the client.
    """
    match = _CODE_RE.fullmatch(pair["code"])
    if not match:
        raise BadRequest("the pairing line is damaged (its pairing code is invalid)", field="pair.code")
    secret = match.group(2)
    wanted = clean_name(name or pair["name"], "listener")
    if not name:
        wanted = _free_name(ledger, wanted)
    elif _name_taken(ledger, wanted):
        raise ConflictError(
            f"a paired device named {wanted!r} already exists on this machine; the pairing code was not used. Pair "
            f"again with --name <another name>, or rename the existing entry first (/handover peers rename {wanted} "
            "<new>).",
            field="name",
            name=wanted,
            code_used=False,
        )
    same = paired_gateway(ledger, pair["fp"], pair.get("profile"))
    if same is not None:
        # L3-F25: the listener keeps one live row per device key (review M13), so a second entry for the same
        # gateway — another address or another name — would end this one on its side without a word here
        raise ConflictError(
            f"this machine is already paired with that machine as {same.name} ({same.url}); it keeps one pairing per "
            f"device, so a second one would end {same.name}. Nothing was sent to {pair['url']} and the pairing code "
            f"was not used. To pair with it at {pair['url']} — or again after it revoked this machine — run "
            f"/handover peers remove {same.name} first, then paste the pairing line again.",
            field="pair",
            peer=same.name,
            code_used=False,
        )
    owner = credential_owner(ledger, gate, pair["url"], secret_store.read if secret_store is not None else None)
    if owner is not None:
        other, what, env = owner
        raise ConflictError(
            f"the {what} in {env} is the one stored for {other.name} ({other.url}); a sign-in is only ever sent to the "
            f"address it was stored for, so nothing was sent to {pair['url']} and the pairing code was not used. Enter "
            f"the {what} of {pair['url']} in Settings → Plugins → ctx_handover, then paste the pairing line again. "
            f"If that address takes the same {what} (one dashboard under two addresses), pair through the address of "
            f"{other.name} instead, where entries may share it, or remove {other.name} first "
            f"(/handover peers remove {other.name}).",
            field="gate",
            peer=other.name,
            variable=env,
            code_used=False,
        )
    provisional = Peer(
        name=wanted,
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
            "the key of the other machine does not match the key fingerprint in the pairing line, so nothing was "
            "paired: the address is wrong, or something between the two machines changed the answer. Check the "
            "address and pair again with a new code."
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
    try:
        resp = client_factory(replace(provisional, pubkey=hello.listener_pubkey)).redeem(body)
    except (PeerUnreachable, RateLimited) as exc:
        exc.message = (
            f"{exc.message}; the answer to the pairing was lost, so {pair['name']} may have paired this device "
            f"anyway. If /handover peers on {pair['name']} lists this device, revoke it there; then create a new code "
            "there and pair again."
        )
        exc.details["listener_entry"] = "unknown"
        raise
    try:
        peer_id = resp["peer_id"]
        listener = resp["listener"]
        rights = frozenset(Right(r) for r in resp["rights"])
        server_proof = resp["server_proof"]
        if not isinstance(peer_id, str) or not ULID_RE.match(peer_id) or not rights:
            raise ValueError
    except (KeyError, TypeError, ValueError):
        raise BadResponseSignature(
            "the other machine sent an unreadable answer to the pairing; nothing was paired"
        ) from None
    expected = pair_proof(
        secret, _server_message(peer_id, hello.listener_pubkey, body["client_nonce"], canonical_rights(rights))
    )
    if (
        not consteq(expected, str(server_proof))
        or listener.get("pubkey") != hello.listener_pubkey
        or listener.get("instance_id") != hello.instance_id
    ):
        raise BadResponseSignature(
            "the other machine could not prove that it knows the pairing code; nothing was paired"
        )
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
    try:
        if any(p.peer_id == peer_id for p in ledger.peers(include_revoked=True)):
            # ``upsert_peer`` keys on the peer id: the entry of another peer would be replaced by this listener
            raise BadResponseSignature(
                f"{pair['name']} answered with an id this machine already uses for another paired device; nothing was "
                "paired"
            )
        if secret_store is not None:
            peer = replace(peer, gate=store_credentials(secret_store, gate, peer_id))
        ledger.upsert_peer(peer)
    except Exception as exc:
        withdrawn = _withdraw(client_factory, peer)
        if secret_store is not None and peer.gate is not gate:  # copied, but the entry was not stored: blank them
            for _what, env in gate_secrets(peer.gate):
                with contextlib.suppress(Exception):
                    secret_store.save(env, "")
        text = _not_stored_text(str(pair["name"]), withdrawn)
        if not isinstance(exc, HandoverError):
            raise HandoverError(text, listener_entry="withdrawn" if withdrawn else "kept") from exc
        exc.message = f"{exc.message}; {text}"
        exc.details["listener_entry"] = "withdrawn" if withdrawn else "kept"
        raise
    client_factory(peer).hello(signed=True)
    return peer


def _name_taken(ledger: Ledger, name: str) -> bool:
    """Whether a peer row — revoked ones included, they keep their name — already uses ``name`` here."""
    return any(p.name == name for p in ledger.peers(include_revoked=True))


def _withdraw(client_factory: Callable[[Peer], object], peer: Peer) -> bool:
    """Take back a redeem this dialer could not store: a signed ``POST /v1/pair/revoke`` as the new inbound peer
    (route #3, DESIGN §12.5) — the listener marks its entry revoked, so nothing stays paired on one side only. True
    only when the listener confirms ``{"revoked": true}``; any failure means its entry is still there."""
    try:
        answer = client_factory(peer).call("POST", "pair/revoke", {})
    except Exception:
        return False
    return isinstance(answer, dict) and answer.get("revoked") is True


def _not_stored_text(listener: str, withdrawn: bool) -> str:
    if withdrawn:
        return (
            f"this device did not store the pairing with {listener}, and {listener} has withdrawn it, so nothing is "
            f"paired. Run /handover pair on {listener} for a new code and pair again (with another --name if the name "
            "is taken)."
        )
    return (
        f"this device did not store the pairing with {listener}, and {listener} could not be told: it still lists "
        f"this device as paired. Revoke this device there (/handover peers revoke <its name for this device> on "
        f"{listener}), then pair again with a new code."
    )


def _hostname() -> str:
    import socket

    return clean_name(socket.gethostname(), "dialer")


def describe(pair: dict) -> str:
    """One-line description of a parsed paste string without the code (for confirmations and logs)."""
    return json.dumps({k: pair[k] for k in ("url", "fp", "name", "profile")}, sort_keys=True)
