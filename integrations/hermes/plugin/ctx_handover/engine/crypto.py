"""Identity key and signature primitives (layer L2; ``cryptography`` from the Hermes runtime, R13/O9).

DESIGN §4.2, §12.2, §12.3. Each profile has one Ed25519 identity: the 32-byte seed lives base64url-encoded in the
profile ``.env`` as ``CTX_HANDOVER_IDENTITY_KEY`` (O2); the ledger's ``meta`` holds only the public key and its
fingerprint. The key is created **only** by operator verbs (``pair``, ``setup``, ``identity rotate``, ``export``)
under the ledger's ``BEGIN IMMEDIATE`` lock, and the write is proven by reading the ``.env`` back (R16). Routes
and hooks only ever ``load``; a mismatch between ``.env`` and ledger refuses to produce an identity, so nothing
is signed with a key the peers do not know.

All encodings are unpadded base64url. Signatures: 64 bytes (86 chars); public keys: 32 bytes (43 chars);
nonces: 16 bytes (22 chars); pairing proofs: HMAC-SHA256 (43 chars).
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import os
import re
import struct
from collections.abc import Callable
from typing import TYPE_CHECKING

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat

from .errors import EnvNotWritable, IdentityLost

if TYPE_CHECKING:  # same layer (L2): type only
    from .ledger import Ledger

IDENTITY_ENV = "CTX_HANDOVER_IDENTITY_KEY"
"""Profile ``.env`` variable holding the identity seed (named in the disclosure, O2)."""

META_PUBKEY = "identity_pubkey"
META_FP = "identity_fp"

REQ_TAG = "ctxh-req-v1"
RESP_TAG = "ctxh-resp-v1"

_B64URL = re.compile(r"[A-Za-z0-9_-]*")


# ---------------------------------------------------------------------------------------------------------
# Encoding helpers
# ---------------------------------------------------------------------------------------------------------


def b64e(data: bytes) -> str:
    """Unpadded base64url."""
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def b64d(text: str) -> bytes:
    """Strict unpadded base64url decode: alphabet, length and canonical trailing bits are checked."""
    if not isinstance(text, str) or not _B64URL.fullmatch(text) or len(text) % 4 == 1:
        raise ValueError("not unpadded base64url")
    try:
        data = base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))
    except (binascii.Error, ValueError) as exc:
        raise ValueError("not unpadded base64url") from exc
    if b64e(data) != text:
        raise ValueError("non-canonical base64url")
    return data


def fingerprint_of(pubkey_b64: str) -> str:
    """16 hex characters of ``sha256(raw public key)`` (pairing string ``fp``, ``peers show``)."""
    return hashlib.sha256(b64d(pubkey_b64)).hexdigest()[:16]


def _public_raw(key: Ed25519PrivateKey) -> bytes:
    return key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)


# ---------------------------------------------------------------------------------------------------------
# Identity
# ---------------------------------------------------------------------------------------------------------


class Identity:
    """The profile's Ed25519 identity; construct only via ``load`` (routes, hooks) or ``create`` (operator verbs)."""

    __slots__ = ("_key", "_pub", "instance_id")

    def __init__(self, instance_id: str, key: Ed25519PrivateKey) -> None:
        self.instance_id = instance_id
        self._key = key
        self._pub = b64e(_public_raw(key))

    def __repr__(self) -> str:  # never print key material
        return f"Identity(instance_id={self.instance_id!r}, fp={self.fingerprint()!r})"

    @staticmethod
    def _from_seed(text: str | None) -> Ed25519PrivateKey | None:
        if not text:
            return None
        try:
            seed = b64d(text.strip())
        except ValueError:
            return None
        if len(seed) != 32:
            return None
        return Ed25519PrivateKey.from_private_bytes(seed)

    @classmethod
    def load(
        cls,
        ledger: Ledger,
        *,
        read_secret: Callable[[str], str | None],
        read_env_fresh: Callable[[str], str | None],
    ) -> Identity | None:
        """The stored identity, or None when this profile has none (nothing paired yet). Never creates one.

        The seed comes from ``get_secret`` and, when that is missing or stale, from a fresh ``.env`` read (a
        process started before the key existed). Raises ``IdentityLost`` (``details.reason`` ``missing`` |
        ``malformed`` | ``mismatch``) when the ledger has a public key the ``.env`` does not reproduce.
        """
        expected = ledger.meta_get(META_PUBKEY)
        if not expected:
            return None
        values = [read_secret(IDENTITY_ENV)]
        fresh = read_env_fresh(IDENTITY_ENV)
        if fresh not in values:
            values.append(fresh)
        values = [v for v in values if v]
        if not values:
            raise IdentityLost(
                "identity lost: the ledger has a public key but CTX_HANDOVER_IDENTITY_KEY is missing from this "
                "profile's .env; run /handover identity rotate and pair again",
                reason="missing",
            )
        keys = [cls._from_seed(v) for v in values]
        if all(k is None for k in keys):
            raise IdentityLost("identity lost: CTX_HANDOVER_IDENTITY_KEY is malformed", reason="malformed")
        for key in keys:
            if key is not None and hmac.compare_digest(b64e(_public_raw(key)), expected):
                return cls(ledger.instance_id(), key)
        raise IdentityLost(
            "identity mismatch: CTX_HANDOVER_IDENTITY_KEY does not match the public key in the ledger; this "
            "process will not sign. Run /handover identity rotate and pair again",
            reason="mismatch",
        )

    @classmethod
    def create(
        cls,
        ledger: Ledger,
        *,
        env_writable: Callable[[str], None],
        save_env: Callable[[str, str], None],
        read_env_fresh: Callable[[str], str | None],
        rotate: bool = False,
    ) -> Identity:
        """Load or create the identity — operator verbs only (R16).

        Under ``ledger.tx()``: an existing public key is loaded (``rotate=False``); otherwise ``env_writable``
        (raises ``EnvNotWritable`` under a managed ``.env`` lock), generate, ``save_env``, read it back with
        ``read_env_fresh`` and compare — a missing or different value aborts with ``EnvNotWritable`` and no ledger
        write. Only then are ``identity_pubkey``/``identity_fp`` stored in the same transaction.
        """
        instance_id = ledger.instance_id()  # outside the transaction: it may write the meta row itself
        key: Ed25519PrivateKey | None = None
        with ledger.tx() as conn:
            if rotate or not ledger.meta_get(META_PUBKEY):
                try:
                    env_writable(IDENTITY_ENV)
                except EnvNotWritable as exc:
                    raise EnvNotWritable(
                        f"the .env of this profile is managed and not writable; {exc.message} — set {IDENTITY_ENV} "
                        "through your secret manager or unlock the .env, then retry",
                        variable=IDENTITY_ENV,
                    ) from exc
                key = Ed25519PrivateKey.generate()
                seed = b64e(key.private_bytes_raw())
                save_env(IDENTITY_ENV, seed)
                back = read_env_fresh(IDENTITY_ENV)
                if not back or not hmac.compare_digest(back.strip(), seed):
                    raise EnvNotWritable(
                        "the identity key could not be read back from this profile's .env (the write was refused "
                        "or overridden); the .env of this profile is managed or not writable — set "
                        f"{IDENTITY_ENV} through your secret manager or unlock the .env, then retry",
                        variable=IDENTITY_ENV,
                    )
                ident_pub = b64e(_public_raw(key))
                ledger.meta_set(META_PUBKEY, ident_pub, conn=conn)
                ledger.meta_set(META_FP, fingerprint_of(ident_pub), conn=conn)
        if key is None:
            loaded = cls.load(ledger, read_secret=read_env_fresh, read_env_fresh=read_env_fresh)
            if loaded is None:  # pragma: no cover - the meta row was seen under the lock above
                raise IdentityLost("identity vanished while loading", reason="missing")
            return loaded
        return cls(instance_id, key)

    def public_b64(self) -> str:
        return self._pub

    def fingerprint(self) -> str:
        return fingerprint_of(self._pub)

    def sign(self, msg: bytes) -> str:
        """Ed25519 signature of ``msg`` (base64url, 86 characters)."""
        return b64e(self._key.sign(msg))


# ---------------------------------------------------------------------------------------------------------
# Signatures and canonical strings
# ---------------------------------------------------------------------------------------------------------


def verify(pubkey_b64: str, msg: bytes, sig_b64: str) -> bool:
    """True when ``sig_b64`` is a valid Ed25519 signature of ``msg`` by ``pubkey_b64``; never raises."""
    try:
        pub = b64d(pubkey_b64)
        sig = b64d(sig_b64)
        if len(pub) != 32 or len(sig) != 64:
            return False
        Ed25519PublicKey.from_public_bytes(pub).verify(sig, msg)
    except (ValueError, TypeError, InvalidSignature):
        return False
    return True


def _field(value: object) -> str:
    text = str(value)
    if "\n" in text or "\r" in text:
        raise ValueError("signature field contains a line break")
    return text


def request_string(method, path, ts, nonce, body_sha256, peer_id, listener_id, epoch: str) -> bytes:
    """Canonical request string (DESIGN §4.2): ``ctxh-req-v1``, method, route-relative path ``/v1/<route>``,
    time, nonce, body sha256, peer id, listener instance id, ``X-Ctxh-Epoch`` value (``""`` when absent)."""
    parts = [REQ_TAG, method, path, ts, nonce, body_sha256, peer_id, listener_id, "" if epoch is None else epoch]
    return "\n".join(_field(p) for p in parts).encode("utf-8")


def response_string(status: int, nonce: str, body_sha256: str) -> bytes:
    """Canonical response string ``ctxh-resp-v1\\n<status>\\n<request nonce>\\n<sha256 of body>``."""
    return "\n".join(_field(p) for p in (RESP_TAG, int(status), nonce, body_sha256)).encode("utf-8")


def length_prefixed(*parts: bytes) -> bytes:
    """Concatenation of ``u32 BE length + bytes`` per field (pairing proofs, SEC 1.3)."""
    out = bytearray()
    for part in parts:
        data = part.encode("utf-8") if isinstance(part, str) else bytes(part)
        out += struct.pack(">I", len(data)) + data
    return bytes(out)


def pair_proof(secret: str, message: bytes) -> str:
    """``HMAC-SHA256(secret, message)`` as base64url."""
    return b64e(hmac.new(secret.encode("utf-8"), message, hashlib.sha256).digest())


def consteq(a: str, b: str) -> bool:
    """Constant-time string comparison."""
    return hmac.compare_digest(str(a).encode("utf-8"), str(b).encode("utf-8"))


def random_b64(nbytes: int) -> str:
    """``nbytes`` from ``os.urandom`` as base64url (nonces)."""
    return b64e(os.urandom(nbytes))


# ---------------------------------------------------------------------------------------------------------
# Request headers (dialer side; the listener verifies with its own code in ``server``)
# ---------------------------------------------------------------------------------------------------------

H_PEER = "X-Ctxh-Peer"
H_TIME = "X-Ctxh-Time"
H_NONCE = "X-Ctxh-Nonce"
H_BODY = "X-Ctxh-Body-Sha256"
H_SIG = "X-Ctxh-Sig"
H_EPOCH = "X-Ctxh-Epoch"
H_RESP_SIG = "X-Ctxh-Resp-Sig"


def sign_request(
    identity: Identity,
    *,
    method: str,
    path: str,
    body: bytes,
    peer_id: str,
    listener_id: str,
    epoch: int | None,
    ts: int,
    nonce: str,
) -> dict[str, str]:
    """The ``X-Ctxh-*`` headers of a signed request (DESIGN §4.2). ``path`` is ``/v1/<route>``."""
    body_sha = hashlib.sha256(body).hexdigest()
    epoch_text = "" if epoch is None else str(int(epoch))
    msg = request_string(method, path, int(ts), nonce, body_sha, peer_id, listener_id, epoch_text)
    headers = {
        H_PEER: peer_id,
        H_TIME: str(int(ts)),
        H_NONCE: nonce,
        H_BODY: body_sha,
        H_SIG: identity.sign(msg),
    }
    if epoch is not None:
        headers[H_EPOCH] = epoch_text
    return headers
