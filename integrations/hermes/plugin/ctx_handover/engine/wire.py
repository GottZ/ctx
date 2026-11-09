"""HTTP client of the v1 wire protocol (layer L5; ``httpx`` from the Hermes runtime, R13).

DESIGN §4.1–§4.6. One ``WireClient`` talks to one paired peer's dashboard origin
``<peer.url>/api/plugins/ctx_handover/v1/<route>`` (``?profile=`` for multiplexed listeners):

* **Gate (D6a, R7)**: every request carries the peer's dashboard-gate credential — session token, bearer, or the
  access token of a Basic-provider login (kept in memory only; one re-login after an unsigned 401) — plus
  optional proxy headers. Values come from ``read_secret`` (``agent.secret_scope.get_secret``, catalog rule 11)
  and are never logged.
* **Signatures (D6b)**: requests are signed with the identity (``crypto.sign_request``) over the route-relative
  path; every response must carry ``X-Ctxh-Resp-Sig`` by the pinned listener key. Unsigned responses are only
  status signals — 413, 429 (``Retry-After`` ≤ 300 s), 502–504; their bodies are never parsed (SEC 1.5).
  An unsigned 401 is the dashboard gate (``gate_rejected``), an unsigned 404 a missing or closed listener
  (``listener_missing``), anything else ``bad_response_signature``.
* **Transport**: HTTP/1.1, ``trust_env``, timeouts connect 10 s / read and write 60 s, no redirects; https
  required unless loopback or ``allow_insecure``. Retries with full-jitter backoff (0.5 s … 60 s) on connect
  errors, 429 and 502–504 — every call is idempotent (DESIGN §3.7), and every attempt is signed afresh.
* **Request size** adapts (DESIGN §4.6): start ``min(4 MiB, peer max)``, 413 halves (floor one block + framing),
  a request over 20 s halves, three under 5 s double. Under ``limits.hosted`` one request is in flight.
"""

from __future__ import annotations

import hashlib
import ipaddress
import logging
import random
import re
import threading
import time
from collections import deque
from collections.abc import Callable, Iterable, Iterator, Sequence
from urllib.parse import urlsplit

import httpx

from .blocks import BLOCK_SIZE, FRAME_HEADER, FRAME_SLACK, MEDIA_BLOCKS, ZSTD_AVAILABLE, decode_frames
from .crypto import H_RESP_SIG, Identity, random_b64, response_string, sign_request, verify
from .errors import (
    BadResponseSignature,
    BlockCorrupt,
    GateRejected,
    HandoverError,
    HttpsRequired,
    ListenerMissing,
    NotFound,
    PayloadTooLarge,
    PeerUnreachable,
    RateLimited,
)
from .jsonutil import from_wire, loads_bounded
from .model import GateAuth, GateMode, Hello, Peer

log = logging.getLogger("ctx_handover.wire")

API_PREFIX = "/api/plugins/ctx_handover"
MIB = 1024**2
START_REQUEST = 4 * MIB
RETRY_AFTER_CAP = 300
BACKOFF_MIN, BACKOFF_MAX = 0.5, 60.0
SLOW_S, FAST_S = 20.0, 5.0
MAX_JSON_RESPONSE = 16 * MIB
MAX_IDS = 4096

_HEADER_NAME = re.compile(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}")
_RESERVED_HEADERS = {"host", "content-length", "content-type", "transfer-encoding", "connection"}


def is_loopback(host: str | None) -> bool:
    """``localhost`` or a loopback IP literal."""
    if not host:
        return False
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host.strip("[]")).is_loopback
    except ValueError:
        return False


# ---------------------------------------------------------------------------------------------------------
# Gate
# ---------------------------------------------------------------------------------------------------------


def _secret(read_secret: Callable[[str], str | None], env_name: str, what: str) -> str:
    value = read_secret(env_name)
    if not value:
        raise GateRejected(f"{what}: {env_name} is not set in this profile (plugin settings)", variable=env_name)
    if any(c in value for c in "\r\n\0"):
        raise GateRejected(f"{what}: {env_name} contains a line break", variable=env_name)
    return value


def gate_headers(
    gate: GateAuth | None, read_secret: Callable[[str], str | None], *, login: Callable[[], str] | None = None
) -> dict[str, str]:
    """Headers that pass the peer's dashboard gate (R7). Secrets only through ``read_secret`` (SEC 7.3)."""
    if gate is None:
        raise GateRejected("no dashboard gate is configured for this peer")
    headers: dict[str, str] = {}
    if gate.mode == GateMode.SESSION_TOKEN:
        headers["X-Hermes-Session-Token"] = _secret(read_secret, gate.token_env, "session token")
    elif gate.mode == GateMode.BEARER:
        headers["Authorization"] = "Bearer " + _secret(read_secret, gate.token_env, "bearer token")
    elif gate.mode == GateMode.BASIC_LOGIN:
        if login is None:
            raise GateRejected("basic_login needs a login callable")
        token = login()
        if not token or any(c in token for c in "\r\n\0"):
            raise GateRejected("basic login returned no usable access token")
        headers["Authorization"] = "Bearer " + token
    else:  # pragma: no cover - enum is closed
        raise GateRejected(f"unknown gate mode {gate.mode}")
    for name, env_name in gate.extra_headers:
        if (
            not _HEADER_NAME.fullmatch(str(name))
            or name.lower() in _RESERVED_HEADERS
            or name.lower().startswith("x-ctxh-")
        ):
            raise GateRejected(f"invalid extra header name {name!r}")
        headers[name] = _secret(read_secret, env_name, f"header {name}")
    return headers


def basic_login(
    base_url: str,
    username: str,
    password: str,
    *,
    verify_tls: bool,
    timeout: float = 20,
    transport: httpx.BaseTransport | None = None,
) -> str:
    """Sign in at the Basic dashboard provider (``POST <base>/auth/password-login``) and return the access token
    from the ``hermes_session_at`` cookie (DESIGN §4.1, R7; ``spike/r7/basic_login.py``)."""
    url = base_url.rstrip("/") + "/auth/password-login"
    try:
        with httpx.Client(
            timeout=timeout, verify=verify_tls, trust_env=True, follow_redirects=False, transport=transport
        ) as client:
            resp = client.post(url, json={"provider": "basic", "username": username, "password": password})
    except httpx.HTTPError as exc:
        raise PeerUnreachable(f"basic login failed: {type(exc).__name__}") from None
    if resp.status_code == 429:
        raise RateLimited("basic login rate limited", retry_after=60)
    if resp.status_code != 200:
        raise GateRejected(f"basic login refused (HTTP {resp.status_code})")
    for name, value in resp.cookies.items():
        if name.endswith("hermes_session_at") and value:
            return value
    raise GateRejected("basic login answered without an access token cookie")


def gate_warnings(gate: GateAuth | None, *, env_mode: int | None) -> list[str]:
    """``doctor`` lines about the stored gate credential and the profile ``.env`` (T1, SEC 1.2, R17)."""
    out: list[str] = []
    if gate is not None:
        if gate.mode in (GateMode.SESSION_TOKEN, GateMode.BEARER):
            out.append(
                "the stored dashboard token opens the whole dashboard of this peer (configuration, secrets, "
                "terminal), not only ctx_handover; prefer a reverse proxy that passes only "
                "/api/plugins/ctx_handover/ and checks its own header (peer_extra_headers)"
            )
        elif gate.mode == GateMode.BASIC_LOGIN:
            out.append(
                "a dashboard password is stored for this peer; it opens the whole dashboard, not only "
                "ctx_handover; prefer a reverse proxy with its own header (peer_extra_headers)"
            )
    if env_mode is not None and env_mode & 0o077:
        out.append(f"the profile .env is group- or world-readable (mode {env_mode & 0o777:o}); run chmod 600 on it")
    return out


# ---------------------------------------------------------------------------------------------------------
# Adaptive request size
# ---------------------------------------------------------------------------------------------------------


class Adaptive:
    """Request-size controller of DESIGN §4.6, plus one refinement: the size of a body a proxy refused (413)
    becomes a ceiling, so doubling never walks back into a known limit."""

    def __init__(self, peer_max: int) -> None:
        self.floor = BLOCK_SIZE + FRAME_HEADER + FRAME_SLACK
        self.peer_max = max(self.floor, int(peer_max))
        self.ceiling = self.peer_max
        self.size = min(START_REQUEST, self.peer_max)
        self._fast = 0

    def too_large(self, failed_bytes: int | None = None) -> None:
        if self.size <= self.floor:
            raise PayloadTooLarge(
                "a proxy in front of the peer limits request bodies below one block (≈ 513 KiB); raise its limit",
                limit=self.size,
            )
        if failed_bytes is not None:
            self.ceiling = max(self.floor, min(self.ceiling, int(failed_bytes) - 1))
        self.size = max(self.floor, min(self.size // 2, self.ceiling))
        self._fast = 0

    def observe(self, seconds: float) -> None:
        if seconds > SLOW_S:
            self.size = max(self.floor, self.size // 2)
            self._fast = 0
        elif seconds < FAST_S:
            self._fast += 1
            if self._fast >= 3:
                self.size = min(self.peer_max, self.ceiling, self.size * 2)
                self._fast = 0
        else:
            self._fast = 0


# ---------------------------------------------------------------------------------------------------------
# Client
# ---------------------------------------------------------------------------------------------------------


class WireClient:
    """Client of one peer (see module docstring). ``peer.pubkey`` empty ⇒ only unsigned pairing calls work."""

    def __init__(
        self,
        peer: Peer,
        identity: Identity,
        *,
        read_secret: Callable[[str], str | None],
        listener_id: str | None = None,
        timeout: float = 60,
        transport: httpx.BaseTransport | None = None,
        clock: Callable[[], float] = time.time,
        sleep: Callable[[float], None] = time.sleep,
        max_attempts: int = 6,
        verify_tls: bool = True,
        self_info: dict | None = None,
    ) -> None:
        parts = urlsplit(peer.url or "")
        if parts.scheme not in {"http", "https"} or not parts.hostname:
            raise HttpsRequired(f"peer {peer.name} has no usable http(s) URL")
        if parts.scheme == "http" and not is_loopback(parts.hostname) and not peer.allow_insecure:
            raise HttpsRequired(f"peer {peer.name} uses plain HTTP; pair with --allow-insecure for VPN-only addresses")
        self.peer = peer
        self.identity = identity
        self._read_secret = read_secret
        self.listener_id = listener_id
        self._clock = clock
        self._sleep = sleep
        self._attempts = max(1, int(max_attempts))
        self._base = peer.url.rstrip("/")
        self._verify_tls = verify_tls
        self._transport = transport
        self._http = httpx.Client(
            timeout=httpx.Timeout(connect=10, read=timeout, write=timeout, pool=timeout),
            trust_env=True,
            follow_redirects=False,
            verify=verify_tls,
            transport=transport,
        )
        self._access: str | None = None
        self._hello: Hello | None = None
        self._adaptive = Adaptive(8 * MIB)
        self._lock = threading.Lock()
        self._hosted = False
        self.clock_skew = 0.0
        self.self_info = dict(self_info or {})

    # -- lifecycle -------------------------------------------------------------------------------------------
    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> WireClient:
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    # -- properties ------------------------------------------------------------------------------------------
    @property
    def request_bytes(self) -> int:
        return self._adaptive.size

    @property
    def peer_caps(self) -> frozenset[str]:
        caps = self._hello.capabilities if self._hello is not None else frozenset()
        if not ZSTD_AVAILABLE:
            caps = caps - {"enc.zstd"}
        return frozenset(caps)

    @property
    def limits(self):
        return self._hello.limits if self._hello is not None else None

    def conductor_info(self) -> dict:
        """``conductor`` object of ``POST /jobs``."""
        return {"instance_id": self.identity.instance_id, **self.self_info}

    # -- gate --------------------------------------------------------------------------------------------------
    def _login(self) -> str:
        if self._access is None:
            gate = self.peer.gate
            password = _secret(self._read_secret, gate.password_env, "basic login password")
            self._access = basic_login(
                self._base, gate.username, password, verify_tls=self._verify_tls, transport=self._transport
            )
        return self._access

    def _gate(self) -> dict[str, str]:
        return gate_headers(self.peer.gate, self._read_secret, login=self._login)

    # -- core request ----------------------------------------------------------------------------------------
    def _url(self, route: str) -> str:
        return f"{self._base}{API_PREFIX}/v1/{route}"

    def _send(
        self,
        method: str,
        route: str,
        body: bytes,
        *,
        content_type: str | None,
        signed: bool,
        epoch: int | None,
        accept_unsigned_ok: bool,
    ) -> httpx.Response:
        """One logical request with retries; returns a verified 2xx response or raises. Every ``GateRejected``
        (gate secret missing, Basic login refused, unsigned 401) carries ``details.mode`` — this peer's gate mode,
        ``none`` without a gate — and ``details.peer`` — the peer's name — for the remedy text (W122)."""
        try:
            return self._exchange(
                method,
                route,
                body,
                content_type=content_type,
                signed=signed,
                epoch=epoch,
                accept_unsigned_ok=accept_unsigned_ok,
            )
        except GateRejected as exc:
            gate = self.peer.gate
            exc.details.setdefault("mode", str(gate.mode) if gate is not None else "none")
            exc.details.setdefault("peer", self.peer.name)
            raise

    def _exchange(
        self,
        method: str,
        route: str,
        body: bytes,
        *,
        content_type: str | None,
        signed: bool,
        epoch: int | None,
        accept_unsigned_ok: bool,
    ) -> httpx.Response:
        last: HandoverError | None = None
        relogged = False
        attempt = 0
        while attempt < self._attempts:
            attempt += 1
            nonce = random_b64(16)
            headers = {"X-Ctxh-Nonce": nonce, "Accept": "application/json, " + MEDIA_BLOCKS}
            headers.update(self._gate())
            if content_type:
                headers["Content-Type"] = content_type
            if signed:
                if not self.listener_id:
                    self._learn_listener()
                headers.update(
                    sign_request(
                        self.identity,
                        method=method,
                        path="/v1/" + route,
                        body=body,
                        peer_id=self.peer.peer_id,
                        listener_id=self.listener_id,
                        epoch=epoch,
                        ts=int(self._clock()),
                        nonce=nonce,
                    )
                )
            params = {"profile": self.peer.profile} if self.peer.profile else None
            started = time.monotonic()
            try:
                if self._hosted:
                    with self._lock:
                        resp = self._http.request(
                            method, self._url(route), content=body, headers=headers, params=params
                        )
                else:
                    resp = self._http.request(method, self._url(route), content=body, headers=headers, params=params)
            except httpx.HTTPError as exc:
                last = PeerUnreachable(f"{self.peer.name} is not reachable ({type(exc).__name__})")
                self._backoff(attempt, None)
                continue
            elapsed = time.monotonic() - started
            try:
                self._interpret(resp, nonce, accept_unsigned_ok=accept_unsigned_ok)
            except GateRejected:
                if self.peer.gate is not None and self.peer.gate.mode == GateMode.BASIC_LOGIN and not relogged:
                    relogged = True
                    self._access = None
                    attempt -= 1  # the re-login is not a retry of the request
                    continue
                raise
            except (PeerUnreachable, RateLimited) as exc:
                last = exc
                self._backoff(attempt, exc.details.get("retry_after"))
                continue
            if route.endswith("/blocks") or route.endswith("/blocks/get"):
                self._adaptive.observe(elapsed)
            return resp
        raise last if last is not None else PeerUnreachable(f"{self.peer.name} is not reachable")

    def _backoff(self, attempt: int, retry_after: object) -> None:
        if attempt >= self._attempts:
            return
        if isinstance(retry_after, int) and retry_after > 0:
            delay = float(min(retry_after, RETRY_AFTER_CAP))
        else:
            delay = random.uniform(BACKOFF_MIN, min(BACKOFF_MAX, BACKOFF_MIN * 2**attempt))  # noqa: S311 - jitter
        self._sleep(delay)

    def _interpret(self, resp: httpx.Response, nonce: str, *, accept_unsigned_ok: bool) -> None:
        status = resp.status_code
        sig = resp.headers.get(H_RESP_SIG)
        pubkey = self.peer.pubkey
        if sig and pubkey:
            msg = response_string(status, nonce, hashlib.sha256(resp.content).hexdigest())
            if not verify(pubkey, msg, sig):
                raise BadResponseSignature(f"response of {self.peer.name} has an invalid signature")
            if 200 <= status < 300:
                return
            try:
                body = loads_bounded(resp.content, max_bytes=MAX_JSON_RESPONSE)
            except HandoverError:
                body = {}
            err = HandoverError.from_wire(status, body if isinstance(body, dict) else {})
            if status == 429:
                err = RateLimited(err.message, retry_after=_retry_after(resp))
            raise err
        # unsigned: a status signal at most (SEC 1.5); bodies are not parsed
        if status == 401:
            raise GateRejected(f"the dashboard of {self.peer.name} rejected the access credential")
        if status == 404:
            raise ListenerMissing(f"{self.peer.name} has no ctx_handover routes (not installed, not enabled or closed)")
        if status == 413:
            raise PayloadTooLarge("request body refused by a proxy in front of the peer", proxy=True)
        if status == 429:
            raise RateLimited("rate limited by a proxy in front of the peer", retry_after=_retry_after(resp))
        if status in (502, 503, 504):
            raise PeerUnreachable(f"{self.peer.name} answered HTTP {status} through a proxy", status=status)
        if accept_unsigned_ok and not pubkey and 200 <= status < 300:
            return
        raise BadResponseSignature(f"unsigned response (HTTP {status}) from {self.peer.name}")

    def _json(self, resp: httpx.Response) -> dict:
        data = loads_bounded(resp.content, max_bytes=MAX_JSON_RESPONSE)
        if not isinstance(data, dict):
            raise BadResponseSignature("response is not a JSON object")
        return data

    # -- hello and pairing ------------------------------------------------------------------------------------
    def _learn_listener(self) -> None:
        resp = self._send("GET", "hello", b"", content_type=None, signed=False, epoch=None, accept_unsigned_ok=True)
        data = self._json(resp)
        if self.peer.pubkey and data.get("listener_pubkey") != self.peer.pubkey:
            raise BadResponseSignature(f"the key of {self.peer.name} changed; pair again")
        instance = data.get("instance_id")
        if not isinstance(instance, str) or not instance:
            raise BadResponseSignature("hello without instance id")
        self.listener_id = instance

    def hello(self, *, signed: bool = True) -> Hello:
        """``GET /v1/hello``; the signed form also sets limits, capabilities, hosted mode and clock skew."""
        if not signed:
            resp = self._send("GET", "hello", b"", content_type=None, signed=False, epoch=None, accept_unsigned_ok=True)
            hello = from_wire(Hello, self._json(resp))
            if not self.listener_id:
                self.listener_id = hello.instance_id
            return hello
        resp = self._send("GET", "hello", b"", content_type=None, signed=True, epoch=None, accept_unsigned_ok=False)
        hello = from_wire(Hello, self._json(resp))
        if hello.listener_pubkey != self.peer.pubkey or hello.instance_id != self.listener_id:
            raise BadResponseSignature(f"hello of {self.peer.name} does not match the paired listener")
        self._hello = hello
        self._hosted = bool(hello.limits.hosted)
        self._adaptive = Adaptive(hello.limits.max_request_bytes)
        if hello.server_time:
            self.clock_skew = float(hello.server_time) - self._clock()
        return hello

    def redeem(self, body: dict) -> dict:
        """Unsigned ``POST /v1/pair/redeem`` (the pairing proofs bind it, DESIGN §12.3)."""
        import json

        data = json.dumps(body, separators=(",", ":")).encode("utf-8")
        resp = self._send(
            "POST",
            "pair/redeem",
            data,
            content_type="application/json",
            signed=False,
            epoch=None,
            accept_unsigned_ok=True,
        )
        return self._json(resp)

    # -- signed calls -------------------------------------------------------------------------------------------
    def call(self, method: str, route: str, body: dict | None = None, *, epoch: int | None = None) -> dict:
        """Signed JSON request; ``epoch`` goes into the body and the ``X-Ctxh-Epoch`` header."""
        import json

        if epoch is not None:
            body = {**(body or {}), "epoch": int(epoch)}
        data = b"" if method == "GET" else json.dumps(body or {}, separators=(",", ":"), ensure_ascii=False).encode()
        resp = self._send(
            method,
            route,
            data,
            content_type=None if method == "GET" else "application/json",
            signed=True,
            epoch=epoch,
            accept_unsigned_ok=False,
        )
        return self._json(resp)

    def put_blocks(self, job_id: str, epoch: int, frames: Iterable[bytes]) -> tuple[int, list]:
        """``PUT /v1/jobs/{id}/blocks`` in batches of at most ``request_bytes`` (frames are never split)."""
        import json

        stored = 0
        rejected: list[tuple[str, str]] = []
        pending: deque[bytes] = deque()
        source = iter(frames)
        exhausted = False
        while True:
            batch: list[bytes] = []
            size = 0
            while True:
                if not pending and not exhausted:
                    try:
                        pending.append(next(source))
                    except StopIteration:
                        exhausted = True
                if not pending:
                    break
                nxt = pending[0]
                if batch and size + len(nxt) > self.request_bytes:
                    break
                batch.append(pending.popleft())
                size += len(nxt)
            if not batch:
                return stored, rejected
            try:
                resp = self._send(
                    "PUT",
                    f"jobs/{job_id}/blocks",
                    b"".join(batch),
                    content_type=MEDIA_BLOCKS,
                    signed=True,
                    epoch=epoch,
                    accept_unsigned_ok=False,
                )
            except PayloadTooLarge:
                self._adaptive.too_large(size)
                pending.extendleft(reversed(batch))
                continue
            data = json.loads(resp.content)
            stored += int(data.get("stored", 0))
            rejected.extend((str(r.get("id")), str(r.get("reason"))) for r in data.get("rejected", []))

    def get_blocks(self, job_id: str, epoch: int, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        """``POST /v1/jobs/{id}/blocks/get`` until every id arrived; each frame is hash-verified."""
        accept = [e for e, cap in (("zstd", "enc.zstd"), ("zlib", "enc.zlib")) if cap in self.peer_caps]
        remaining = list(dict.fromkeys(ids))
        while remaining:
            chunk = remaining[:MAX_IDS]
            try:
                resp = self._send(
                    "POST",
                    f"jobs/{job_id}/blocks/get",
                    _json_bytes({"epoch": int(epoch), "ids": chunk, "accept": accept, "max_bytes": self.request_bytes}),
                    content_type="application/json",
                    signed=True,
                    epoch=epoch,
                    accept_unsigned_ok=False,
                )
            except PayloadTooLarge:
                self._adaptive.too_large()
                continue
            wanted = set(chunk)
            got: list[str] = []
            for block_id, raw, reason in decode_frames(resp.content, block_size=BLOCK_SIZE):
                if raw is None or block_id not in wanted:
                    raise BlockCorrupt(
                        f"block {block_id[:12]} from {self.peer.name} rejected ({reason})", block_id=block_id
                    )
                got.append(block_id)
                yield block_id, raw
            if not got:
                raise NotFound("the listener returned no blocks")
            done = set(got)
            remaining = [i for i in remaining if i not in done]

    # -- diagnostics -------------------------------------------------------------------------------------------
    def probe_gate(self, *, measure_limit: bool = False) -> dict:
        """``doctor``: which layer answers (gate, plugin, proxy) and, with ``measure_limit``, the largest request
        body (1, 2, 4, 8 MiB) that reaches the plugin — measured with signed requests to an unknown job, which the
        plugin reads completely and answers ``not_found``.

        Keys: ``gate`` (``ok``/``rejected``/``unreachable``), ``plugin``, ``signed``, ``error`` (an error code),
        ``body_limit`` (bytes, with ``measure_limit``) and the summary ``ok`` (True, None = works with a warning,
        False), ``detail`` and ``remedy`` (None when nothing is to do) — W121."""
        report: dict = {"gate": "unknown", "plugin": False, "signed": False}
        try:
            self.hello(signed=bool(self.peer.pubkey and self.peer.peer_id))
        except GateRejected as exc:
            report.update(gate="rejected", error=exc.code)
            return self._summary(report)
        except ListenerMissing as exc:
            report.update(gate="ok", error=exc.code)
            return self._summary(report)
        except PeerUnreachable as exc:
            report.update(gate="unreachable", error=exc.code)
            return self._summary(report)
        except HandoverError as exc:
            report.update(gate="ok", plugin=True, error=exc.code)
            return self._summary(report)
        report.update(gate="ok", plugin=True, signed=self._hello is not None)
        if measure_limit and self._hello is not None:
            passed = 0
            for size in (1 * MIB, 2 * MIB, 4 * MIB, 8 * MIB):
                try:
                    self._send(
                        "PUT",
                        "jobs/00000000000000000000000000/blocks",
                        b"\0" * size,
                        content_type=MEDIA_BLOCKS,
                        signed=True,
                        epoch=0,
                        accept_unsigned_ok=False,
                    )
                except PayloadTooLarge:
                    break
                except (GateRejected, PeerUnreachable, BadResponseSignature):
                    break
                except HandoverError:
                    pass  # the plugin read the body and refused the job: the size passed every proxy
                passed = size
            report["body_limit"] = passed
        return self._summary(report)

    def _summary(self, report: dict) -> dict:
        """``ok``/``detail``/``remedy`` of a ``probe_gate`` report (the layer that answered decides)."""
        name, mode = self.peer.name, (str(self.peer.gate.mode) if self.peer.gate is not None else "none")
        error = report.get("error") or ""
        if report["gate"] == "unreachable":
            ok, detail = False, f"{self.peer.url or name} did not answer ({error})"
            remedy = f"check the address, TLS and the network or VPN, and that the dashboard of {name} runs"
        elif report["gate"] == "rejected":
            ok, detail = False, f"the dashboard gate of {name} refused the stored credential ({mode})"
            remedy = f"store the current dashboard credential of {name} for this peer (gate mode {mode})"
        elif not report["plugin"]:
            ok, detail = False, f"the dashboard of {name} answered, ctx_handover did not ({error})"
            remedy = f"install and enable ctx_handover on {name}; its inbound must be open (pair again)"
        elif error or not report["signed"]:
            ok, detail = False, f"ctx_handover on {name} answered without a valid signed hello ({error or 'unsigned'})"
            remedy = f"pair again with {name} if its key changed (/handover pair)"
        else:
            ok, detail, remedy = True, "dashboard gate passed, ctx_handover answered with a valid signature", None
            limit = report.get("body_limit")
            if isinstance(limit, int) and limit < 8 * MIB:
                ok = None
                detail += (
                    f"; a proxy limits request bodies to {limit // MIB} MiB"
                    if limit
                    else "; a proxy limits request bodies below 1 MiB"
                )
                remedy = "raise the body limit of the proxy in front of it to 8 MiB (transfers stay smaller)"
        report.update(ok=ok, detail=detail, remedy=remedy)
        return report


def _retry_after(resp: httpx.Response) -> int:
    try:
        value = int(resp.headers.get("Retry-After", "1"))
    except ValueError:
        value = 1
    return max(1, min(RETRY_AFTER_CAP, value))


def _json_bytes(obj: dict) -> bytes:
    import json

    return json.dumps(obj, separators=(",", ":")).encode("utf-8")
