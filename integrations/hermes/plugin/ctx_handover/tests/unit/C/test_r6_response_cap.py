"""R6-B: the dialer reads a peer's answer only up to a bound — the size check runs while the body streams in, not
after it was held in memory as a whole (review of PR #190, §3 "the dialer reads a whole response into memory before
the size check").

A paired listener is to be thought of as possibly compromised: its answer to a signed call, its block pages and the
dashboard login in front of it must not make the dialer allocate more than the protocol needs (JSON
``MAX_JSON_RESPONSE``; a block page the request size it asked for plus one frame). The request size itself grows only
up to a local ceiling, whatever ``limits.max_request_bytes`` the peer announces."""

from __future__ import annotations

import gzip
import hashlib
import json
import tracemalloc
import zlib

import httpx
import pytest

from ctx_handover.engine import wire
from ctx_handover.engine.blocks import BLOCK_SIZE
from ctx_handover.engine.crypto import H_RESP_SIG, response_string
from ctx_handover.engine.errors import BadResponseSignature, HandoverError
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import GateAuth, GateMode, Peer, Right
from ctx_handover.engine.wire import MAX_JSON_RESPONSE, MIB, Adaptive, WireClient, basic_login
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

CHUNK = MIB


class Flood:
    """A response body of ``total`` bytes in 1 MiB chunks that counts how many chunks the client pulled."""

    def __init__(self, total: int) -> None:
        self.total, self.pulled = total, 0

    def __iter__(self):
        sent = 0
        while sent < self.total:
            self.pulled += 1
            sent += CHUNK
            yield b"\0" * CHUNK


def _client(tmp_path, handler, listener=None) -> WireClient:
    ident = new_identity(make_ledger(tmp_path / "d" / "ledger.sqlite3"))
    listener = listener or new_identity(make_ledger(tmp_path / "l" / "ledger.sqlite3"))
    peer = Peer(
        name="server",
        peer_id=ulid(),
        direction="dial",
        url="http://127.0.0.1:9",
        pubkey=listener.public_b64(),
        rights=frozenset({Right.PUSH, Right.PULL}),
        gate=GateAuth(mode=GateMode.SESSION_TOKEN, token_env="CTXH_TEST_TOKEN"),
    )
    return WireClient(
        peer,
        ident,
        read_secret=lambda name: "tok",
        listener_id=listener.instance_id,
        transport=httpx.MockTransport(handler),
        sleep=lambda s: None,
        max_attempts=1,
    )


def test_json_answer_over_the_bound_is_cut_while_streaming(tmp_path):
    """★ A 64 MiB answer to a signed JSON call: refused after ``MAX_JSON_RESPONSE`` (16 MiB) arrived.

    Red proof (base ``cab3a492``): ``httpx.Client.request`` reads all 64 chunks, the signature check fails after."""
    flood = Flood(64 * MIB)
    client = _client(tmp_path, lambda req: httpx.Response(200, content=iter(flood)))
    with pytest.raises(HandoverError):
        client.call("POST", "sessions/list", {"limit": 1})
    assert flood.pulled <= MAX_JSON_RESPONSE // CHUNK + 1, flood.pulled


def test_declared_length_over_the_bound_is_refused_before_reading(tmp_path):
    """★ ``Content-Length`` over the bound: refused before a byte of the body is pulled."""
    flood = Flood(64 * MIB)
    client = _client(
        tmp_path,
        lambda req: httpx.Response(200, headers={"Content-Length": str(64 * MIB)}, content=iter(flood)),
    )
    with pytest.raises(BadResponseSignature):
        client.call("POST", "sessions/list", {"limit": 1})
    assert flood.pulled == 0


def test_block_page_over_the_requested_size_is_cut(tmp_path):
    """★ ``blocks/get`` answers at most the size it asked for plus one frame; a 64 MiB page is cut on the way."""
    flood = Flood(64 * MIB)
    client = _client(tmp_path, lambda req: httpx.Response(200, content=iter(flood)))
    with pytest.raises(HandoverError):
        list(client.get_blocks(ulid(), 1, ["a" * 64]))
    bound = max(MAX_JSON_RESPONSE, client.request_bytes + 2 * (BLOCK_SIZE + wire.FRAME_HEADER + wire.FRAME_SLACK))
    assert flood.pulled <= bound // CHUNK + 1, flood.pulled


def test_request_size_has_a_local_ceiling():
    """★ A peer announcing a huge ``max_request_bytes`` and answering fast cannot grow the dialer's request size
    (and with it the block page it accepts and the upload batch it builds) beyond 64 MiB.

    Red proof (base ``cab3a492``): ``Adaptive.peer_max`` is the announced value; the size doubles towards it."""
    a = Adaptive(1 << 40)
    for _ in range(300):
        a.observe(0.01)
    assert a.size <= 64 * MIB
    assert a.peer_max <= 64 * MIB


def test_basic_login_does_not_read_the_page(tmp_path):
    """★ The dashboard login needs only its cookie: a huge login page is not read."""
    flood = Flood(64 * MIB)

    def handler(req):
        return httpx.Response(200, headers={"Set-Cookie": "hermes_session_at=abc; Path=/"}, content=iter(flood))

    token = basic_login("http://127.0.0.1:9", "u", "p", verify_tls=True, transport=httpx.MockTransport(handler))
    assert token == "abc"
    assert flood.pulled <= 1


def _gzip_bomb(decoded: int) -> bytes:
    """``decoded`` zero bytes, gzip-compressed (≈ 1/1000 of the size)."""
    comp = zlib.compressobj(9, zlib.DEFLATED, 16 + zlib.MAX_WBITS)
    out = bytearray()
    for _ in range(decoded // CHUNK):
        out += comp.compress(b"\0" * CHUNK)
    return bytes(out + comp.flush())


def test_compressed_answer_is_bounded_after_inflating(tmp_path):
    """★ A gzip answer that inflates to 256 MiB (a few hundred KiB on the wire): refused once the bound is reached,
    without holding the inflated body (peak memory well below it) — Fremd-Review R6 finding 2.

    Red proof (base ``cab3a492``, and the first R6 version that bounded decoded chunks): ``iter_bytes`` inflates a
    whole raw chunk at once; the peak is above 256 MiB."""
    bomb = _gzip_bomb(256 * MIB)
    client = _client(
        tmp_path, lambda req: httpx.Response(200, headers={"Content-Encoding": "gzip"}, content=iter([bomb]))
    )
    tracemalloc.start()
    try:
        with pytest.raises(HandoverError):
            client.call("POST", "sessions/list", {"limit": 1})
        _now, peak = tracemalloc.get_traced_memory()
    finally:
        tracemalloc.stop()
    assert peak < 3 * MAX_JSON_RESPONSE, peak


def test_identity_is_asked_for_and_a_gzip_answer_still_reads(tmp_path):
    """The dialer asks for ``Accept-Encoding: identity``; a proxy that compresses anyway is inflated (bounded), the
    signature is checked over the inflated body as before."""
    listener = new_identity(make_ledger(tmp_path / "l2" / "ledger.sqlite3"))
    seen = []

    def handler(req):
        seen.append(req.headers.get("accept-encoding"))
        body = json.dumps({"sessions": [{"id": "s1"}]}).encode()
        sig = listener.sign(response_string(200, req.headers["x-ctxh-nonce"], hashlib.sha256(body).hexdigest()))
        return httpx.Response(
            200, headers={"Content-Encoding": "gzip", H_RESP_SIG: sig}, content=iter([gzip.compress(body)])
        )

    client = _client(tmp_path, handler, listener=listener)
    assert client.call("POST", "sessions/list", {"limit": 1})["sessions"] == [{"id": "s1"}]
    assert seen == ["identity"]


def test_unknown_encoding_is_refused(tmp_path):
    """An encoding this dialer cannot inflate under a bound (``br``, ``zstd``) is refused, not decoded."""
    client = _client(
        tmp_path, lambda req: httpx.Response(200, headers={"Content-Encoding": "br"}, content=iter([b"x"]))
    )
    with pytest.raises(BadResponseSignature):
        client.call("POST", "sessions/list", {"limit": 1})


def test_an_answer_the_transport_already_read_is_checked_in_memory(tmp_path):
    """A transport that read the answer itself (in-process test transports, the integration harness's fault
    transport) hands it over consumed: it is used as it is when within the bound, refused above it — never read again
    (Fremd-Review R6 round 2: ``iter_raw`` on such an answer raised ``StreamConsumed``)."""
    listener = new_identity(make_ledger(tmp_path / "l3" / "ledger.sqlite3"))

    def handler(req):
        body = json.dumps({"sessions": []}).encode()
        sig = listener.sign(response_string(200, req.headers["x-ctxh-nonce"], hashlib.sha256(body).hexdigest()))
        return httpx.Response(200, headers={H_RESP_SIG: sig}, content=body)  # bytes: read on construction

    client = _client(tmp_path, handler, listener=listener)
    assert client.call("POST", "sessions/list", {"limit": 1}) == {"sessions": []}
    big = _client(tmp_path / "big", lambda req: httpx.Response(200, content=b"\0" * (MAX_JSON_RESPONSE + 1)))
    with pytest.raises(BadResponseSignature) as exc:
        big.call("POST", "sessions/list", {"limit": 1})
    assert exc.value.details.get("max_bytes") == MAX_JSON_RESPONSE
