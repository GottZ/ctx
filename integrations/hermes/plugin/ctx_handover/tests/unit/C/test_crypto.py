"""Identity key handling and the signature primitives (DESIGN §4.2, §12.2, §12.3; R16; TESTPLAN §2.4)."""

from __future__ import annotations

import hashlib
import itertools
import os
import re

import pytest

from ctx_handover.engine import crypto
from ctx_handover.engine.crypto import (
    IDENTITY_ENV,
    Identity,
    b64d,
    b64e,
    consteq,
    fingerprint_of,
    length_prefixed,
    pair_proof,
    request_string,
    response_string,
    verify,
)
from ctx_handover.engine.errors import EnvNotWritable, IdentityLost
from ctx_handover.tests.unit.C.helpers import child_env, run_barrier_children
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

# ---------------------------------------------------------------------------------------------------------
# Identity (R16, SEC 1.1)
# ---------------------------------------------------------------------------------------------------------


def test_identity_create_and_load_roundtrip(ledger, host, make_identity):
    assert Identity.load(ledger, read_secret=host.read_secret, read_env_fresh=host.read_env_fresh) is None
    ident = make_identity()
    assert re.fullmatch(r"[A-Za-z0-9_-]{43}", ident.public_b64())
    assert ledger.meta_get("identity_pubkey") == ident.public_b64()
    assert ledger.meta_get("identity_fp") == ident.fingerprint() == fingerprint_of(ident.public_b64())
    assert len(ident.fingerprint()) == 16
    assert ident.instance_id == ledger.instance_id()
    seed = host.env_file[IDENTITY_ENV]
    assert len(b64d(seed)) == 32
    again = make_identity()  # idempotent: loads, never regenerates
    assert again.public_b64() == ident.public_b64()
    assert [c for c in host.calls if c[0] == "save_env"] == [("save_env", (IDENTITY_ENV,))]
    loaded = Identity.load(ledger, read_secret=host.read_secret, read_env_fresh=host.read_env_fresh)
    msg = b"hello"
    assert verify(loaded.public_b64(), msg, loaded.sign(msg))
    assert verify(ident.public_b64(), msg, loaded.sign(msg))


def test_identity_load_never_creates(ledger, host):
    assert Identity.load(ledger, read_secret=host.read_secret, read_env_fresh=host.read_env_fresh) is None
    assert ledger.meta_get("identity_pubkey") is None
    assert not [c for c in host.calls if c[0] in {"save_env", "env_writable"}]


def test_identity_env_write_refused_fails_closed(ledger, host, make_identity):
    """★ A managed ``.env`` lock ⇒ ``EnvNotWritable`` with a clear message and no ledger pubkey (R16).

    Red proof: mutant = ``env_writable`` call removed from ``Identity.create`` → the key is written into the
    locked ``.env`` and ``identity_pubkey`` is stored; ``pytest.raises(EnvNotWritable)`` fails.
    """
    host.env_locked.add(IDENTITY_ENV)
    with pytest.raises(EnvNotWritable) as info:
        make_identity()
    assert "not writable" in info.value.message
    assert ledger.meta_get("identity_pubkey") is None
    assert IDENTITY_ENV not in host.env_file


def test_identity_readback_mismatch_aborts(ledger, host, make_identity):
    """★ ``save_env`` that silently does nothing ⇒ abort, no ledger write (R16).

    Red proof: mutant = read-back comparison removed → a pubkey is stored for a key that exists nowhere;
    ``pytest.raises(EnvNotWritable)`` fails.
    """
    host.silently_refused.add(IDENTITY_ENV)
    with pytest.raises(EnvNotWritable) as info:
        make_identity()
    assert "read back" in info.value.message
    assert ledger.meta_get("identity_pubkey") is None


def test_identity_mismatch_refuses_to_sign(ledger, host, make_identity):
    """★ ``.env`` key ≠ ledger pubkey ⇒ no identity object, so nothing is signed; the text says the key does not match
    and that this process signs nothing.

    Red proof: mutant = pubkey comparison in ``Identity.load`` removed → a foreign key is loaded and signs;
    ``pytest.raises(IdentityLost)`` fails.
    """
    make_identity()
    host.env_file[IDENTITY_ENV] = b64e(os.urandom(32))
    with pytest.raises(IdentityLost) as info:
        Identity.load(ledger, read_secret=host.read_secret, read_env_fresh=host.read_env_fresh)
    assert "handover key does not match" in info.value.message and "signs nothing" in info.value.message
    assert info.value.details["reason"] == "mismatch"


def test_identity_lost_when_env_key_missing(ledger, host, make_identity):
    make_identity()
    del host.env_file[IDENTITY_ENV]
    with pytest.raises(IdentityLost) as info:
        Identity.load(ledger, read_secret=host.read_secret, read_env_fresh=host.read_env_fresh)
    assert info.value.details["reason"] == "missing"
    host.env_file[IDENTITY_ENV] = "not-a-key"
    with pytest.raises(IdentityLost) as info:
        Identity.load(ledger, read_secret=host.read_secret, read_env_fresh=host.read_env_fresh)
    assert info.value.details["reason"] == "malformed"


def test_identity_stale_process_env_falls_back_to_fresh_dotenv(ledger, host, make_identity):
    """A process started before a rotation sees the old value in its environment; the fresh ``.env`` wins."""
    ident = make_identity()
    host.secrets[IDENTITY_ENV] = b64e(os.urandom(32))
    loaded = Identity.load(ledger, read_secret=host.read_secret, read_env_fresh=host.read_env_fresh)
    assert loaded.public_b64() == ident.public_b64()


def test_identity_rotate_replaces_key(ledger, host, make_identity):
    from ctx_handover.engine import hostcompat

    old = make_identity()
    new = Identity.create(
        ledger,
        env_writable=hostcompat.env_writable,
        save_env=hostcompat.save_env,
        read_env_fresh=hostcompat.read_env_fresh,
        rotate=True,
    )
    assert new.public_b64() != old.public_b64()
    assert ledger.meta_get("identity_pubkey") == new.public_b64()


def test_identity_single_key_across_processes(tmp_path, hermes_home):
    """★ Four processes running the operator path at once end with one key in ``.env`` and ledger (R16).

    Real processes, real SQLite ``BEGIN IMMEDIATE``, real Hermes ``save_env_value``/``get_env_value_prefer_dotenv``
    on ``$HERMES_HOME/.env``. Red proof: mutant = ``Identity.create`` without ``ledger.tx()`` (check and write
    unserialised) → the children report different public keys.
    """
    ledger_path = tmp_path / "shared" / "ledger.sqlite3"
    make_ledger(ledger_path)
    go = tmp_path / "go"
    results = run_barrier_children([["identity", str(ledger_path), str(go)]] * 4, go, env=child_env())
    keys = {r["pubkey"] for r in results}
    assert len(keys) == 1, keys
    env_text = (hermes_home / ".env").read_text(encoding="utf-8")
    assert len(re.findall(rf"^{IDENTITY_ENV}=", env_text, flags=re.MULTILINE)) == 1
    assert make_ledger(ledger_path).meta_get("identity_pubkey") == keys.pop()


# ---------------------------------------------------------------------------------------------------------
# Signature primitives
# ---------------------------------------------------------------------------------------------------------

_REQ = {
    "method": "POST",
    "path": "/v1/jobs/01ARZ3NDEKTSV4RRFFQ69G5FAV/status",
    "ts": 1_762_646_400,
    "nonce": "A" * 22,
    "body_sha256": hashlib.sha256(b"{}").hexdigest(),
    "peer_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV",
    "listener_id": "01BX5ZZKBKACTAV9WEVGEMMVRZ",
    "epoch": "3",
}

_ALTERED = {
    "method": "PUT",
    "path": "/v1/jobs/01ARZ3NDEKTSV4RRFFQ69G5FAV/cancel",
    "ts": 1_762_646_401,
    "nonce": "B" * 22,
    "body_sha256": hashlib.sha256(b"{ }").hexdigest(),
    "peer_id": "01ARZ3NDEKTSV4RRFFQ69G5FAW",
    "listener_id": "01BX5ZZKBKACTAV9WEVGEMMVRY",
    "epoch": "4",
}


def test_request_signature_roundtrip(ledger, make_identity):
    """★ Any change of method, route path, time, nonce, body hash, peer, listener id or epoch fails (T10).

    Red proof: mutant = ``request_string`` drops the listener id field → the altered-``listener_id`` case still
    verifies.
    """
    ident = make_identity()
    sig = ident.sign(request_string(**_REQ))
    assert re.fullmatch(r"[A-Za-z0-9_-]{86}", sig)
    assert verify(ident.public_b64(), request_string(**_REQ), sig)
    for field, value in _ALTERED.items():
        changed = dict(_REQ, **{field: value})
        assert not verify(ident.public_b64(), request_string(**changed), sig), field
    assert not verify(ident.public_b64(), request_string(**dict(_REQ, epoch="")), sig)


def test_request_string_refuses_field_separators():
    for field in ("method", "path", "nonce", "peer_id", "listener_id", "epoch"):
        with pytest.raises(ValueError):
            request_string(**dict(_REQ, **{field: "x\ny"}))


def test_response_signature_binds_status_nonce_body(ledger, make_identity):
    ident = make_identity()
    body_sha = hashlib.sha256(b'{"ok":true}').hexdigest()
    sig = ident.sign(response_string(200, "N" * 22, body_sha))
    assert verify(ident.public_b64(), response_string(200, "N" * 22, body_sha), sig)
    for status, nonce, sha in ((201, "N" * 22, body_sha), (200, "M" * 22, body_sha), (200, "N" * 22, "0" * 64)):
        assert not verify(ident.public_b64(), response_string(status, nonce, sha), sig)


def test_verify_never_raises_on_garbage(ledger, make_identity):
    ident = make_identity()
    for pub, sig in itertools.product(["", "!!", ident.public_b64(), "A" * 43], ["", "x", "A" * 86, "@" * 86]):
        assert verify(pub, b"m", sig) is False


def test_length_prefixed_fields_are_unambiguous():
    assert length_prefixed(b"ab", b"c") != length_prefixed(b"a", b"bc")
    assert length_prefixed(b"ab", b"c") == b"\x00\x00\x00\x02ab\x00\x00\x00\x01c"


def test_pair_proof_and_consteq():
    proof = pair_proof("SECRET0000000000", b"message")
    assert re.fullmatch(r"[A-Za-z0-9_-]{43}", proof)
    assert consteq(proof, pair_proof("SECRET0000000000", b"message"))
    assert not consteq(proof, pair_proof("SECRET0000000001", b"message"))
    assert not consteq(proof, "")


def test_b64_strict():
    assert b64d(b64e(b"\x00\xff" * 16)) == b"\x00\xff" * 16
    for bad in ("A=", "A+B/", "AAAAA", " AAAA"):
        with pytest.raises(ValueError):
            b64d(bad)
    assert crypto.b64e(b"") == ""
