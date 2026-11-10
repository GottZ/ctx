"""Listener: request authentication, limits and the v1 route table (layer L8; DESIGN §4.1–§4.7, §12.4, §17.5).

``dashboard/plugin_api.py`` hands every ``/api/plugins/ctx_handover/v1/<path>`` request to ``Server.handle_stream``
with the router's path parameter (never ``request.url.path``, so a reverse proxy with a path prefix cannot shift
the signed path, SEC 1.4) and the body as an async chunk stream. Processing order (DESIGN §4.2, I7):

1. inbound switch (``inbound`` setting, §12.4) and identity present — else a plain, unsigned
   ``404 {"detail": "Plugin not found"}`` that cannot be told apart from a missing plugin (SEC 1.10);
2. canonical route path (``paths.route_path``) and route match;
3. unsigned routes: ``GET hello`` without signature headers, ``POST pair/redeem``;
4. authenticator chosen by the request (``OperatorSession`` only when enabled, R8) → header formats → skew →
   peer known, ``direction = 'inbound'``, not revoked → right of the route → body streamed with the route's cap
   (cut as soon as it is exceeded) → body hash → signature → nonce;
5. per-peer rate limit, handler, signed response (``X-Ctxh-Resp-Sig``).

Handlers delegate to the participant registry (cluster A, ``participant.ParticipantRegistry``), which owns every
job rule — binding to the peer, role ↔ right, epoch fencing, the concurrent-job caps and routes #8–#10 (contract
X18); the server itself owns no job state and no job logic. It calls: ``open``, ``bind`` (once per job route,
contract V17), ``status``, ``heartbeat``, ``cancel``, ``list_sessions``, ``offers``, ``claim``, ``abort_jobs``.
Ledger methods used: ``meta_get`` (identity rotation), ``peers``, ``pairing_pending``, ``inbound_peer``,
``nonce_seen``, ``touch_peer``, ``revoke_peer``, ``tx``, ``job_object``, ``job_objects`` and the pairing methods
through ``pairing``. Pulled objects are exactly ``ledger.job_objects(job_id)`` and their blocks (R18, contract X13).

The identity is re-read when another process of the profile rotated it (``identity rotate`` writes a new
``identity_pubkey``): the next request signs with the new key, or — when it cannot be loaded — the listener
answers like a missing plugin; it never signs with a retired key.

``settings`` keys (all optional): ``inbound`` (auto|on|off), ``name``, ``max_request_mb`` (8),
``max_json_bytes`` (4 MiB), ``operator_api`` (False), ``hosted`` (False), ``capabilities`` (restricting
iterable), ``plugin_version``, ``hermes_version``, ``rate_per_s`` (20), ``rate_burst`` (60),
``pair_failures_per_hour`` (30). Schema keys (``inbound``, ``max_request_mb``) and the flags ``operator_api``/
``hosted`` are read through the one settings reader ``settings`` (contract V22): a bare YAML ``off``/``on`` arrives
as a boolean and means ``off``/``on``, a YAML boolean word for ``operator_api``/``hosted`` counts like the boolean,
a rejected ``max_request_mb`` is its schema default; an ``inbound`` value the reader does not recognise is ``off``
(the reader's fail-closed rule — its default ``auto`` would answer paired peers). The listener's own knobs outside
the schema fall back to their default when missing, non-numeric or not positive.
"""

from __future__ import annotations

import asyncio
import collections
import hashlib
import json
import logging
import math
import re
import threading
import time
from collections.abc import AsyncIterator, Callable, Mapping, Sequence
from dataclasses import dataclass, replace
from typing import TYPE_CHECKING, Protocol

from . import CAPABILITIES_V1, PROTOCOL, WIRE_MAJOR, WIRE_MINOR
from . import settings as settings_mod
from .blocks import BLOCK_SIZE, MEDIA_BLOCKS, ZSTD_AVAILABLE, decode_frames, encode_frame, verify_manifest
from .crypto import H_RESP_SIG, META_PUBKEY, Identity, request_string, response_string, verify
from .errors import (
    BadRequest,
    CapabilityMissing,
    Forbidden,
    HandoverError,
    JobConflict,
    NotFound,
    PayloadTooLarge,
    RateLimited,
    Unauthorized,
)
from .ids import ULID_RE
from .jsonutil import from_wire, loads_bounded, to_wire
from .model import (
    AuthContext,
    AuthScheme,
    CapsuleInfo,
    CommitAck,
    DeltaInfo,
    EnvFacts,
    FinalPlan,
    ObjectManifest,
    PreparedInfo,
    PrepareRequest,
    ResourcePlan,
    Right,
    Role,
    SessionRef,
)
from .paths import route_path

if TYPE_CHECKING:
    from .contracts import JobContext
    from .ledger import Ledger
    from .participant import ParticipantRegistry

log = logging.getLogger("ctx_handover.server")

MIB = 1024**2
DEFAULT_MAX_JSON = 4 * MIB
MAX_IDS = 4096
MAX_OBJECT_IDS = 64
MAX_PROMPT = 2000
PAIR_BODY_CAP = 16 * 1024
SKEW_S = 300
NONCE_TTL_S = 900
RETRY_AFTER_CAP = 300

NOT_FOUND_BODY = b'{"detail":"Plugin not found"}'
SERVER_MARK = "X-Ctxh-Server"

_TIME_RE = re.compile(r"[0-9]{1,12}")
_NONCE_RE = re.compile(r"[A-Za-z0-9_-]{22}")
_SIG_RE = re.compile(r"[A-Za-z0-9_-]{86}")
_SHA_RE = re.compile(r"[0-9a-f]{64}")
_EPOCH_RE = re.compile(r"[0-9]{1,18}")
_PARAM_RE = {"id": ULID_RE, "offer_id": ULID_RE}

PUSH, PULL = Right.PUSH, Right.PULL
ROUTES: dict[tuple[str, str], tuple[str, Right | None]] = {
    ("GET", "/v1/hello"): ("hello", None),
    ("POST", "/v1/pair/redeem"): ("pair_redeem", None),
    ("POST", "/v1/pair/revoke"): ("pair_revoke", None),
    ("POST", "/v1/sessions/list"): ("sessions_list", PULL),
    ("POST", "/v1/offers/list"): ("offers_list", PULL),
    ("POST", "/v1/offers/{offer_id}/claim"): ("offer_claim", PULL),
    ("POST", "/v1/jobs"): ("jobs_open", None),
    ("POST", "/v1/jobs/{id}/status"): ("job_status", None),
    ("POST", "/v1/jobs/{id}/heartbeat"): ("job_heartbeat", None),
    ("POST", "/v1/jobs/{id}/cancel"): ("job_cancel", None),
    ("POST", "/v1/jobs/{id}/source/facts"): ("source_facts", PULL),
    ("POST", "/v1/jobs/{id}/source/plan"): ("source_plan", PULL),
    ("POST", "/v1/jobs/{id}/source/lock"): ("source_lock", PULL),
    ("POST", "/v1/jobs/{id}/source/export"): ("source_export", PULL),
    ("POST", "/v1/jobs/{id}/source/pack"): ("source_pack", PULL),
    ("POST", "/v1/jobs/{id}/source/prepare"): ("source_prepare", PULL),
    ("POST", "/v1/jobs/{id}/source/retire"): ("source_retire", PULL),
    ("POST", "/v1/jobs/{id}/source/release"): ("source_release", PULL),
    ("POST", "/v1/jobs/{id}/objects/list"): ("objects_list", PULL),
    ("POST", "/v1/jobs/{id}/blocks/get"): ("blocks_get", PULL),
    ("POST", "/v1/jobs/{id}/target/facts"): ("target_facts", PUSH),
    ("POST", "/v1/jobs/{id}/target/negotiate"): ("target_negotiate", PUSH),
    ("POST", "/v1/jobs/{id}/objects/put"): ("objects_put", PUSH),
    ("POST", "/v1/jobs/{id}/blocks/need"): ("blocks_need", PUSH),
    ("PUT", "/v1/jobs/{id}/blocks"): ("put_blocks", PUSH),
    ("POST", "/v1/jobs/{id}/target/stage_context"): ("target_stage_context", PUSH),
    ("POST", "/v1/jobs/{id}/target/prepare"): ("target_prepare", PUSH),
    ("POST", "/v1/jobs/{id}/target/apply_delta"): ("target_apply_delta", PUSH),
    ("POST", "/v1/jobs/{id}/target/reset_session"): ("target_reset_session", PUSH),
    ("POST", "/v1/jobs/{id}/target/commit"): ("target_commit", PUSH),
    ("POST", "/v1/jobs/{id}/target/abort"): ("target_abort", PUSH),
    ("POST", "/v1/jobs/{id}/target/continue"): ("target_continue", PUSH),
}
"""DESIGN §4.5: ``(method, pattern) -> (handler, right)``; right None = any authenticated peer (or unsigned)."""

_UNSIGNED = {"pair_redeem"}
_BINARY_IN = {"put_blocks"}


@dataclass(frozen=True)
class Request:
    """``path`` is the router's path parameter (route-relative, without ``v1/``); header names are lower-case."""

    method: str
    path: str
    headers: Mapping[str, str]
    body: bytes
    client_ip: str | None


@dataclass(frozen=True)
class Response:
    status: int
    headers: dict[str, str]
    body: bytes


# ---------------------------------------------------------------------------------------------------------
# Authenticators (DESIGN §4.2 seam: PeerSignature active, OperatorSession for Stage 2, disabled)
# ---------------------------------------------------------------------------------------------------------


class Authenticator(Protocol):
    scheme: AuthScheme

    def claims(self, headers: Mapping[str, str]) -> bool:
        """True when the request uses this scheme."""
        ...

    def precheck(self, req: Request) -> AuthContext:
        """Everything that does not need the body — for signed requests the signature over the declared body hash;
        raises ``Unauthorized``."""
        ...

    def finish(self, ctx: AuthContext, req: Request) -> AuthContext:
        """Body-dependent checks (the body against its declared hash, then the nonce); raises ``Unauthorized``."""
        ...

    def authenticate(self, req: Request) -> AuthContext:
        """``precheck`` + ``finish`` on a request with its body."""
        ...


def _unauthorized() -> Unauthorized:
    return Unauthorized("unauthorized")


def _epoch_header(headers: Mapping[str, str]) -> int | None:
    value = headers.get("x-ctxh-epoch")
    if value is None:
        return None
    if not _EPOCH_RE.fullmatch(value):
        raise _unauthorized()
    return int(value)


class PeerSignatureAuth:
    """Ed25519 request signatures of paired inbound peers (DESIGN §4.2)."""

    scheme = AuthScheme.PEER_SIGNATURE

    def __init__(
        self,
        ledger: Ledger,
        identity: Identity,
        *,
        skew_s: int = SKEW_S,
        nonce_ttl_s: int = NONCE_TTL_S,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self._ledger = ledger
        self._identity = identity
        self._skew = skew_s
        self._ttl = nonce_ttl_s
        self._clock = clock

    def use_identity(self, identity: Identity) -> None:
        """The listener's identity after a rotation (``Server`` calls it)."""
        self._identity = identity

    def claims(self, headers: Mapping[str, str]) -> bool:
        return any(h in headers for h in ("x-ctxh-sig", "x-ctxh-peer"))

    def precheck(self, req: Request) -> AuthContext:
        h = req.headers
        fields = {
            "x-ctxh-peer": ULID_RE,
            "x-ctxh-time": _TIME_RE,
            "x-ctxh-nonce": _NONCE_RE,
            "x-ctxh-body-sha256": _SHA_RE,
            "x-ctxh-sig": _SIG_RE,
        }
        for name, pattern in fields.items():
            value = h.get(name)
            if not isinstance(value, str) or not pattern.fullmatch(value):
                raise _unauthorized()
        epoch = _epoch_header(h)
        if abs(int(h["x-ctxh-time"]) - self._clock()) > self._skew:
            raise _unauthorized()
        peer = self._ledger.inbound_peer(h["x-ctxh-peer"])
        if peer is None or peer.direction != "inbound" or peer.revoked_at is not None:
            raise _unauthorized()
        # the signature covers the body through its declared hash, so it is checked here, before the route's right
        # is compared and before a byte of the body is read (review R6-B: a request naming a peer's id learned that
        # peer's rights from 403 vs 401, and a forged upload was read in full)
        msg = request_string(
            req.method,
            route_path(req.path),
            int(h["x-ctxh-time"]),
            h["x-ctxh-nonce"],
            h["x-ctxh-body-sha256"],
            peer.peer_id,
            self._identity.instance_id,
            h.get("x-ctxh-epoch", ""),
        )
        ctx = AuthContext(scheme=self.scheme, peer=peer, rights=frozenset(peer.rights), epoch=epoch)
        if not verify(ctx.peer.pubkey, msg, h["x-ctxh-sig"]):
            raise _unauthorized()
        return ctx

    def finish(self, ctx: AuthContext, req: Request) -> AuthContext:
        """The body is the one the signature declared (``precheck`` verified the signature over its hash); the
        nonce is spent last, so a request with another body never uses up the nonce of the real one."""
        h = req.headers
        if hashlib.sha256(req.body).hexdigest() != h["x-ctxh-body-sha256"]:
            raise _unauthorized()
        if self._ledger.nonce_seen(ctx.peer.peer_id, h["x-ctxh-nonce"], int(h["x-ctxh-time"]), self._ttl):
            raise _unauthorized()
        return ctx

    def authenticate(self, req: Request) -> AuthContext:
        return self.finish(self.precheck(req), req)


class OperatorSessionAuth:
    """Stage-2 operator scheme (DESIGN §4.2, §17.2): the request passed this listener's dashboard gate, carries
    ``X-Ctxh-Operator: 1`` and a non-cookie credential (bearer or session-token header, SEC 1.12). Disabled in
    v0.1: a disabled instance claims operator requests and refuses them, so the header never falls through to
    another scheme."""

    scheme = AuthScheme.OPERATOR_SESSION

    def __init__(self, enabled: bool) -> None:
        self.enabled = bool(enabled)

    def claims(self, headers: Mapping[str, str]) -> bool:
        return "x-ctxh-operator" in headers

    def precheck(self, req: Request) -> AuthContext:
        h = req.headers
        if not self.enabled or h.get("x-ctxh-operator") != "1":
            raise _unauthorized()
        bearer = h.get("authorization", "")
        if not (bearer.lower().startswith("bearer ") and len(bearer) > 7) and not h.get("x-hermes-session-token"):
            raise _unauthorized()
        return AuthContext(scheme=self.scheme, peer=None, rights=frozenset({PUSH, PULL}), epoch=_epoch_header(h))

    def finish(self, ctx: AuthContext, req: Request) -> AuthContext:
        return ctx

    def authenticate(self, req: Request) -> AuthContext:
        return self.finish(self.precheck(req), req)


# ---------------------------------------------------------------------------------------------------------
# Rate limiting (in-process; per peer token bucket, pairing failures per listener)
# ---------------------------------------------------------------------------------------------------------


class _Buckets:
    def __init__(self, rate: float, burst: float, clock: Callable[[], float]) -> None:
        self.rate, self.burst, self.clock = float(rate), float(burst), clock
        self._state: dict[str, tuple[float, float]] = {}
        self._lock = threading.Lock()

    def take(self, key: str) -> float:
        """0 when allowed, else seconds until a token is available."""
        with self._lock:
            now = self.clock()
            tokens, last = self._state.get(key, (self.burst, now))
            tokens = min(self.burst, tokens + (now - last) * self.rate)
            if tokens >= 1:
                self._state[key] = (tokens - 1, now)
                return 0.0
            self._state[key] = (tokens, now)
            return (1 - tokens) / self.rate if self.rate > 0 else RETRY_AFTER_CAP


class _Window:
    def __init__(self, limit: int, window_s: float, clock: Callable[[], float]) -> None:
        self.limit, self.window, self.clock = int(limit), float(window_s), clock
        self._events: collections.deque[float] = collections.deque()
        self._lock = threading.Lock()

    def _prune(self, now: float) -> None:
        while self._events and self._events[0] <= now - self.window:
            self._events.popleft()

    def full(self) -> bool:
        with self._lock:
            self._prune(self.clock())
            return len(self._events) >= self.limit

    def add(self) -> None:
        with self._lock:
            now = self.clock()
            self._prune(now)
            self._events.append(now)


# ---------------------------------------------------------------------------------------------------------
# Server
# ---------------------------------------------------------------------------------------------------------


def _match(method: str, route: str) -> tuple[str, Right | None, str, dict[str, str]] | None:
    parts = route.split("/")
    for (m, pattern), (handler, right) in ROUTES.items():
        if m != method:
            continue
        pat = pattern.split("/")
        if len(pat) != len(parts):
            continue
        params: dict[str, str] = {}
        for p, v in zip(pat, parts, strict=True):
            if p.startswith("{"):
                name = p[1:-1]
                if not _PARAM_RE[name].fullmatch(v):
                    break
                params[name] = v
            elif p != v:
                break
        else:
            return handler, right, pattern, params
    return None


def _ctype(headers: Mapping[str, str]) -> str:
    return headers.get("content-type", "").split(";", 1)[0].strip().lower()


async def _read_capped(chunks: AsyncIterator[bytes], cap: int) -> bytes:
    """Collect the body, raising ``PayloadTooLarge`` as soon as more than ``cap`` bytes arrived (I7)."""
    buf = bytearray()
    async for chunk in chunks:
        buf += chunk
        if len(buf) > cap:
            raise PayloadTooLarge("request body over the limit", max_bytes=cap)
    return bytes(buf)


def _as_tuple(value):
    """Revision values arrive as JSON arrays; the contracts use (nested) tuples."""
    if isinstance(value, (list, tuple)):
        return tuple(_as_tuple(v) for v in value)
    return value


async def _one(body: bytes) -> AsyncIterator[bytes]:
    if body:
        yield body


def _positive(value: object, default: float) -> float:
    """A positive number from a setting (int, float or numeric string), else ``default``."""
    if isinstance(value, bool) or value is None:
        return default
    try:
        number = float(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return default
    return number if math.isfinite(number) and number > 0 else default


def _text(value: object) -> str:
    return "" if value is None else str(value)


def _flag(value: object) -> bool:
    """A boolean outside the schema (``operator_api``, ``hosted``) by the one settings reader (contract V22): YAML
    booleans, the words of ``settings.BOOL_WORDS`` and 0/1; anything else — ``None`` included — is False."""
    return settings_mod.flag(value, False)


def inbound_mode(value: object) -> str:
    """``inbound`` as ``auto``|``on``|``off`` (DESIGN §12.4) — exactly the one settings reader (contract V22):
    YAML booleans and the boolean words read as ``on``/``off``, None is the default ``auto``, a value it does not
    recognise is ``off`` (fail-closed, ``settings.FAIL_CLOSED_KEYS``). No second rule here."""
    return settings_mod.coerce("inbound", value)


class Server:
    """See module docstring."""

    def __init__(
        self,
        ledger: Ledger,
        identity: Identity | None,
        registry: ParticipantRegistry,
        settings: dict,
        authenticators: Sequence[Authenticator],
        *,
        redact: Callable[[str], str] | None = None,
        clock: Callable[[], float] = time.time,
        load_identity: Callable[[], Identity | None] | None = None,
        inbound_setting: Callable[[], object] | None = None,
    ) -> None:
        """``inbound_setting``, when given, returns the current raw ``inbound`` setting and is asked on every
        request, so ``inbound: off`` closes the routes without a restart (M15); a failing read counts as ``off``.
        Without it ``settings["inbound"]`` holds for the life of this server."""
        self._ledger = ledger
        self._inbound_setting = inbound_setting
        self._identity = identity
        self._load_identity = load_identity or self._load_profile_identity
        self._identity_lock = threading.Lock()
        self._registry = registry
        self._settings = dict(settings or {})
        self._auths = list(authenticators)
        self._redact = redact or (lambda text: text)
        self._clock = clock
        s = self._settings
        self.name = _text(s.get("name"))
        raw_inbound = s.get("inbound")
        self.inbound = inbound_mode(raw_inbound)
        for text in settings_mod.problems({"inbound": raw_inbound}):
            log.warning("ctx_handover: %s", text)
        self.max_request_bytes = int(settings_mod.coerce("max_request_mb", s.get("max_request_mb")) * MIB)
        self.max_json_bytes = min(int(_positive(s.get("max_json_bytes"), DEFAULT_MAX_JSON)), self.max_request_bytes)
        self._rate = _Buckets(_positive(s.get("rate_per_s"), 20), _positive(s.get("rate_burst"), 60), clock)
        self._pair_failures = _Window(int(_positive(s.get("pair_failures_per_hour"), 30)), 3600, clock)
        self._allowed_cache: collections.OrderedDict[str, tuple[frozenset[str], frozenset[str]]] = (
            collections.OrderedDict()
        )
        self._cache_lock = threading.Lock()

    # -- public ----------------------------------------------------------------------------------------------
    @property
    def stream_cap(self) -> int:
        """Largest body any route accepts (the dashboard adapter needs nothing else)."""
        return max(self.max_request_bytes, self.max_json_bytes)

    def operator_enabled(self) -> bool:
        return _flag(self._settings.get("operator_api")) and any(
            a.scheme == AuthScheme.OPERATOR_SESSION and getattr(a, "enabled", False) for a in self._auths
        )

    def capabilities(self) -> frozenset[str]:
        caps = set(CAPABILITIES_V1) - {"operator.v1"}
        if not ZSTD_AVAILABLE:
            caps.discard("enc.zstd")
        restrict = self._settings.get("capabilities")
        if isinstance(restrict, (list, tuple, set, frozenset)):
            caps &= {str(c) for c in restrict}
        if self.operator_enabled():
            caps.add("operator.v1")
        return frozenset(caps)

    async def handle(self, req: Request) -> Response:
        """``handle_stream`` for a request whose body is already in memory."""
        return await self.handle_stream(req.method, req.path, req.headers, _one(req.body), req.client_ip)

    async def handle_stream(
        self,
        method: str,
        path: str,
        headers: Mapping[str, str],
        chunks: AsyncIterator[bytes],
        client_ip: str | None,
    ) -> Response:
        headers = {str(k).lower(): str(v) for k, v in headers.items()}
        nonce = headers.get("x-ctxh-nonce", "")
        nonce = nonce if _NONCE_RE.fullmatch(nonce) else ""
        try:
            if not await asyncio.to_thread(self._inbound_active):
                return self._plain_not_found()
            route = route_path(path)
            found = _match(method.upper(), route)
            if found is None:
                raise NotFound("no such route")
            handler, right, _pattern, params = found
            req = Request(method=method.upper(), path=path, headers=headers, body=b"", client_ip=client_ip)
            if handler in _UNSIGNED or (handler == "hello" and not self._pick(headers)):
                body = await _read_capped(chunks, PAIR_BODY_CAP)
                result = await asyncio.to_thread(self._unsigned, handler, replace(req, body=body))
                return self._respond(200, result, nonce)
            auth = self._pick(headers)
            if auth is None:
                raise _unauthorized()
            ctx = await asyncio.to_thread(auth.precheck, req)
            if right is not None and right not in ctx.rights:
                raise Forbidden("forbidden")
            cap = self.max_request_bytes if handler in _BINARY_IN else self.max_json_bytes
            want = MEDIA_BLOCKS if handler in _BINARY_IN else "application/json"
            if method.upper() != "GET" and _ctype(headers) != want:
                raise BadRequest("unsupported content type", field="content-type", expected=want)
            body = await _read_capped(chunks, cap)
            req = replace(req, body=body)
            ctx = await asyncio.to_thread(auth.finish, ctx, req)
            if ctx.peer is not None:
                wait = self._rate.take(ctx.peer.peer_id)
                if wait:
                    raise RateLimited("too many requests", retry_after=min(RETRY_AFTER_CAP, math.ceil(wait)))
                await asyncio.to_thread(self._ledger.touch_peer, ctx.peer.peer_id, client_ip)
            result = await asyncio.to_thread(self._dispatch, handler, ctx, params, req)
            if isinstance(result, Response):
                return self._sign(result, nonce)
            return self._respond(200, result, nonce)
        except HandoverError as exc:
            return self._error(exc, nonce)
        except Exception as exc:
            log.error("ctx_handover route %s failed: %s", path[:200], self._redact(f"{type(exc).__name__}: {exc}"))
            return self._respond(
                500,
                {"error": {"code": "internal", "message": "internal error", "retryable": True, "details": {}}},
                nonce,
            )

    # -- identity ---------------------------------------------------------------------------------------------
    def _load_profile_identity(self) -> Identity | None:
        """Default loader: the profile's identity through ``hostcompat`` (``get_secret`` + a fresh ``.env`` read,
        DESIGN §12.2) — the composition root may pass its own (``load_identity``)."""
        from . import hostcompat

        return Identity.load(self._ledger, read_secret=hostcompat.read_secret, read_env_fresh=hostcompat.read_env_fresh)

    def current_identity(self) -> Identity | None:
        """The identity this listener signs with, re-read when the ledger names another public key (``identity
        rotate`` in another process of the profile). A key that cannot be loaded, or does not match the ledger,
        gives None: the listener then answers like a missing plugin instead of signing with a retired key."""
        expected = self._ledger.meta_get(META_PUBKEY)
        current = self._identity
        if (current.public_b64() if current is not None else None) == (expected or None):
            return current
        with self._identity_lock:
            current = self._identity
            if (current.public_b64() if current is not None else None) == (expected or None):
                return current
            fresh: Identity | None = None
            if expected:
                try:
                    fresh = self._load_identity()
                except Exception as exc:  # any failure to load keeps the listener closed, never on the old key
                    code = exc.code if isinstance(exc, HandoverError) else type(exc).__name__
                    log.warning("ctx_handover listener: identity not usable after a change (%s)", code)
                if fresh is not None and fresh.public_b64() != expected:
                    fresh = None
            if fresh is not None:
                for auth in self._auths:
                    use = getattr(auth, "use_identity", None)
                    if callable(use):
                        use(fresh)
                log.info("ctx_handover listener: identity changed to %s", fresh.fingerprint())
            self._identity = fresh
            return fresh

    # -- plumbing ---------------------------------------------------------------------------------------------
    def _inbound_active(self) -> bool:
        if self.current_identity() is None:
            return False
        mode = self.inbound
        if self._inbound_setting is not None:
            try:
                mode = inbound_mode(self._inbound_setting())
            except Exception:
                mode = "off"  # fail closed: an unreadable setting never answers peers
        if mode == "off":
            return False
        if mode == "on":
            return True
        if self._ledger.pairing_pending(self._clock()):
            return True
        return any(p.direction == "inbound" and p.revoked_at is None for p in self._ledger.peers())

    def _pick(self, headers: Mapping[str, str]) -> Authenticator | None:
        for auth in self._auths:
            if auth.claims(headers):
                return auth
        return None

    @staticmethod
    def _plain_not_found() -> Response:
        return Response(404, {"Content-Type": "application/json"}, NOT_FOUND_BODY)

    def _sign(self, resp: Response, nonce: str) -> Response:
        headers = dict(resp.headers)
        headers[SERVER_MARK] = "1"
        headers["Cache-Control"] = "no-store"
        if self._identity is not None:
            msg = response_string(resp.status, nonce, hashlib.sha256(resp.body).hexdigest())
            headers[H_RESP_SIG] = self._identity.sign(msg)
        return Response(resp.status, headers, resp.body)

    def _respond(self, status: int, payload: dict, nonce: str, extra: dict[str, str] | None = None) -> Response:
        body = json.dumps(payload, separators=(",", ":"), ensure_ascii=False, sort_keys=True).encode("utf-8")
        headers = {"Content-Type": "application/json", **(extra or {})}
        return self._sign(Response(status, headers, body), nonce)

    def _error(self, exc: HandoverError, nonce: str) -> Response:
        envelope = exc.to_peer(self._redact)
        extra = {}
        if exc.http_status == 429:
            retry = exc.details.get("retry_after", 1)
            extra["Retry-After"] = str(max(1, min(RETRY_AFTER_CAP, int(retry) if isinstance(retry, int) else 1)))
        status = exc.http_status if exc.wire else 500
        return self._respond(status, envelope, nonce, extra)

    def _json(self, req: Request) -> dict:
        data = loads_bounded(req.body or b"{}", max_bytes=self.max_json_bytes)
        if not isinstance(data, dict):
            raise BadRequest("JSON object expected", field="$")
        return data

    def _limits(self) -> dict:
        return {
            "max_request_bytes": self.max_request_bytes,
            "block_size": BLOCK_SIZE,
            "max_ids_per_request": MAX_IDS,
            "max_json_bytes": self.max_json_bytes,
            "hosted": _flag(self._settings.get("hosted", False)),
        }

    # -- unsigned routes --------------------------------------------------------------------------------------
    def _unsigned(self, handler: str, req: Request) -> dict:
        identity = self._identity
        if handler == "hello":
            return {
                "protocol": PROTOCOL,
                "majors": [WIRE_MAJOR],
                "minor": WIRE_MINOR,
                "instance_id": identity.instance_id,
                "listener_pubkey": identity.public_b64(),
                "pairing_open": bool(self._ledger.pairing_pending(self._clock())),
            }
        # pair/redeem
        from . import pairing

        if _ctype(req.headers) != "application/json":
            raise BadRequest("unsupported content type", field="content-type", expected="application/json")
        if self._pair_failures.full():
            raise RateLimited("too many failed pairing attempts; try again later", retry_after=RETRY_AFTER_CAP)
        body = loads_bounded(req.body, max_bytes=PAIR_BODY_CAP, max_depth=4)
        try:
            return pairing.redeem_listener(
                self._ledger,
                identity,
                body,
                req.client_ip,
                name=self.name,
                clock=self._clock,
            )
        except HandoverError:
            self._pair_failures.add()
            raise

    # -- authenticated routes ---------------------------------------------------------------------------------
    def _dispatch(self, handler: str, auth: AuthContext, params: dict[str, str], req: Request):
        body = {} if handler in _BINARY_IN or req.method == "GET" else self._json(req)
        if "epoch" in body:
            epoch = body["epoch"]
            if isinstance(epoch, bool) or not isinstance(epoch, int) or epoch < 0:
                raise BadRequest("epoch must be a non-negative integer", field="epoch")
            if auth.epoch is None:
                auth = replace(auth, epoch=epoch)
            elif auth.epoch != epoch:
                raise BadRequest("epoch header and body disagree", field="epoch")
        return getattr(self, f"_h_{handler}")(auth, params, body, req)

    def _h_hello(self, auth, params, body, req) -> dict:
        identity = self._identity
        out = self._unsigned("hello", req)
        out.update(
            plugin_version=_text(self._settings.get("plugin_version")),
            hermes_version=_text(self._settings.get("hermes_version")),
            name=self.name,
            capabilities=sorted(self.capabilities()),
            limits=self._limits(),
            server_time=int(self._clock()),
            listener_pubkey=identity.public_b64(),
        )
        if auth.peer is not None:
            out["peer"] = {"peer_id": auth.peer.peer_id, "rights": sorted(str(r) for r in auth.rights)}
        return out

    def _h_pair_revoke(self, auth, params, body, req) -> dict:
        if auth.peer is None:
            raise Forbidden("forbidden")
        job_ids = self._ledger.revoke_peer(auth.peer.peer_id)
        if job_ids:
            self._registry.abort_jobs(job_ids, "peer_revoked")
        return {"revoked": True}

    def _h_sessions_list(self, auth, params, body, req) -> dict:
        limit = body.get("limit", 20)
        if isinstance(limit, bool) or not isinstance(limit, int) or not 1 <= limit <= 200:
            raise BadRequest("limit must be 1..200", field="limit")
        return self._registry.list_sessions(auth, body)

    def _h_offers_list(self, auth, params, body, req) -> dict:
        return self._registry.offers(auth)

    def _h_offer_claim(self, auth, params, body, req) -> dict:
        job_id = body.get("job_id")
        if not isinstance(job_id, str) or not ULID_RE.match(job_id):
            raise BadRequest("job_id must be a ULID", field="job_id")
        return self._registry.claim(auth, params["offer_id"], job_id)

    def _h_jobs_open(self, auth, params, body, req) -> dict:
        """Route #7: the wire format is checked here; role ↔ right, the job caps (``max_jobs_per_peer``,
        ``max_jobs_total``) and idempotent re-opens are the registry's (contract X18).

        A job id that a job of another peer — or a job of this host without a peer (export, import) — uses in any
        role is not available (M17): the objects a job may serve (``job_objects``) are keyed by the job id alone,
        so a pull peer opening a source job under the id of someone else's push would read that push's objects."""
        job_id = body.get("job_id")
        if not isinstance(job_id, str) or not ULID_RE.match(job_id):
            raise BadRequest("job_id must be a ULID", field="job_id")
        mine = auth.peer.peer_id if auth.peer is not None else None
        for role in Role:
            row = self._ledger.job(job_id, role)
            if row is not None and (mine is None or row["peer_id"] != mine):
                raise JobConflict("job id not available")
        return self._registry.open(auth, body)

    # job plumbing
    def _participant(self, auth: AuthContext, job_id: str, role: Role) -> tuple[object, JobContext]:
        """The job's participant API and ``JobContext`` from the registry (binding, right, epoch: contract X18) —
        one ``bind`` per request, so one epoch fence and one ``touch_job`` (contract V17)."""
        return self._registry.bind(auth, job_id, role)

    @staticmethod
    def _decode(cls, body: dict, field: str):
        if field not in body:
            raise BadRequest("missing field", field=field)
        value = body[field]
        try:
            return from_wire(cls, value)
        except BadRequest as exc:
            inner = exc.details.get("field", "")
            raise BadRequest(exc.message, field=f"{field}.{inner}" if inner else field) from None

    @staticmethod
    def _op(status) -> dict:
        return to_wire(status)

    def _h_job_status(self, auth, params, body, req) -> dict:
        """Route #8 — the registry's (contract X18)."""
        return to_wire(self._registry.status(auth, params["id"], body))

    def _h_job_heartbeat(self, auth, params, body, req) -> dict:
        """Route #9 — the registry's (contract X18), including ``{state}`` of a source that gave up its leases
        (contract V17, ``source_reclaimed``)."""
        return self._registry.heartbeat(auth, params["id"], body)

    def _h_job_cancel(self, auth, params, body, req) -> dict:
        """Route #10 — the registry's: a prepared source or a committing target refuse (contract X18)."""
        return self._registry.cancel(auth, params["id"], body)

    # source routes
    def _src(self, auth, params):
        return self._participant(auth, params["id"], Role.SOURCE)

    def _h_source_facts(self, auth, params, body, req) -> dict:
        api, job = self._src(auth, params)
        return to_wire(api.facts(job))

    def _h_source_plan(self, auth, params, body, req) -> dict:
        api, job = self._src(auth, params)
        facts = self._decode(EnvFacts, body, "target_facts")
        ref = from_wire(SessionRef, body.get("session_ref") or {})
        return self._op(api.start_plan(job, facts, ref))

    def _h_source_lock(self, auth, params, body, req) -> dict:
        api, job = self._src(auth, params)
        ref = self._decode(SessionRef, body, "session_ref")
        revision = body.get("plan_revision", [])
        if not isinstance(revision, list):
            raise BadRequest("plan_revision must be an array", field="plan_revision")
        return to_wire(api.lock(job, ref, _as_tuple(revision)))

    def _h_source_export(self, auth, params, body, req) -> dict:
        api, job = self._src(auth, params)
        return self._op(api.start_export(job))

    def _h_source_pack(self, auth, params, body, req) -> dict:
        api, job = self._src(auth, params)
        return self._op(api.start_pack(job, self._decode(FinalPlan, body, "final_plan")))

    def _h_source_prepare(self, auth, params, body, req) -> dict:
        api, job = self._src(auth, params)
        return self._op(api.start_prepare(job, self._decode(PreparedInfo, body, "prepared")))

    def _h_source_retire(self, auth, params, body, req) -> dict:
        api, job = self._src(auth, params)
        return self._op(api.start_retire(job, self._decode(CommitAck, body, "ack")))

    def _h_source_release(self, auth, params, body, req) -> dict:
        api, job = self._src(auth, params)
        return self._op(api.start_release(job, str(body.get("reason", "released"))[:200]))

    def _job_allowed(self, job_id: str, source) -> tuple[frozenset[str], frozenset[str]]:
        """Objects of this job and the blocks they consist of — all a pull peer may read (R18, T17)."""
        objects = frozenset(self._ledger.job_objects(job_id))
        with self._cache_lock:
            cached = self._allowed_cache.get(job_id)
            if cached is not None and cached[0] == objects:
                self._allowed_cache.move_to_end(job_id)
                return cached
        blocks: set[str] = set()
        for m in source.manifests(sorted(objects)):
            blocks.update(m.blocks)
        entry = (objects, frozenset(blocks))
        with self._cache_lock:
            self._allowed_cache[job_id] = entry
            while len(self._allowed_cache) > 8:
                self._allowed_cache.popitem(last=False)
        return entry

    def _h_objects_list(self, auth, params, body, req) -> dict:
        api, _job = self._src(auth, params)
        ids = body.get("object_ids")
        if not isinstance(ids, list) or not 1 <= len(ids) <= MAX_OBJECT_IDS or not all(isinstance(i, str) for i in ids):
            raise BadRequest(f"object_ids must hold 1..{MAX_OBJECT_IDS} ids", field="object_ids")
        cursor = body.get("cursor") or 0
        if isinstance(cursor, str) and cursor.isdigit():
            cursor = int(cursor)
        if isinstance(cursor, bool) or not isinstance(cursor, int) or not 0 <= cursor < len(ids):
            raise BadRequest("invalid cursor", field="cursor")
        allowed = frozenset(self._ledger.job_objects(params["id"]))
        if any(i not in allowed for i in ids):
            raise NotFound("object not part of this job")
        out, size = [], 0
        source = api.blocks()
        for index in range(cursor, len(ids)):
            wire = to_wire(source.manifests([ids[index]])[0])
            n = len(json.dumps(wire))
            if out and size + n > self.max_json_bytes - 1024:
                return {"manifests": out, "cursor": str(index)}
            if n > self.max_json_bytes - 1024:
                raise PayloadTooLarge("object manifest too large for one response", object_id=ids[index])
            out.append(wire)
            size += n
        return {"manifests": out}

    def _ids(self, body: dict) -> list[str]:
        ids = body.get("ids")
        if (
            not isinstance(ids, list)
            or len(ids) > MAX_IDS
            or not all(isinstance(i, str) and _SHA_RE.fullmatch(i) for i in ids)
        ):
            raise BadRequest(f"ids must hold at most {MAX_IDS} block ids", field="ids")
        return ids

    def _h_blocks_get(self, auth, params, body, req) -> Response:
        api, _job = self._src(auth, params)
        ids = self._ids(body)
        source = api.blocks()
        _objects, allowed = self._job_allowed(params["id"], source)
        if any(i not in allowed for i in ids):
            raise NotFound("block not part of this job")
        accept = body.get("accept") or []
        caps = frozenset(f"enc.{a}" for a in accept if a in {"zlib", "zstd"})
        max_bytes = body.get("max_bytes")
        cap = self.max_request_bytes
        if isinstance(max_bytes, int) and not isinstance(max_bytes, bool) and max_bytes > 0:
            cap = min(cap, max(max_bytes, BLOCK_SIZE + 1024 + 41))
        frames, size = [], 0
        for block_id in ids:
            (_bid, raw) = next(iter(source.read_blocks([block_id])))
            frame = encode_frame(block_id, raw, peer_caps=caps)
            if frames and size + len(frame) > cap:
                break
            frames.append(frame)
            size += len(frame)
        return Response(200, {"Content-Type": MEDIA_BLOCKS}, b"".join(frames))

    # target routes
    def _tgt(self, auth, params):
        return self._participant(auth, params["id"], Role.TARGET)

    def _h_target_facts(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        return to_wire(api.facts(job, self._decode(EnvFacts, body, "source_facts")))

    def _h_target_negotiate(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        return to_wire(api.negotiate(job, self._decode(ResourcePlan, body, "plan")))

    def _h_objects_put(self, auth, params, body, req) -> dict:
        """Route #23: the manifests are stored and registered with the job in one ledger transaction, so block GC
        never sees a received object that belongs to no job (Befund d)."""
        api, job = self._tgt(auth, params)
        raw = body.get("manifests")
        if not isinstance(raw, list) or len(raw) > MAX_OBJECT_IDS:
            raise BadRequest(f"manifests must hold at most {MAX_OBJECT_IDS} entries", field="manifests")
        ms = [verify_manifest(from_wire(ObjectManifest, m)) for m in raw]
        with self._ledger.tx():
            stored = self._sink(api, job).put_manifests(ms)
            for m in ms:
                self._ledger.job_object(params["id"], m.object_id)
        return {"stored": stored}

    @staticmethod
    def _sink(api, job):
        """The job's quota-enforcing sink (R28, ``blocks_for``). A target without one gets no unbounded fallback to its
        plain store (review M5): the request is refused (``capability_missing``) and nothing is stored."""
        blocks_for = getattr(api, "blocks_for", None)
        if not callable(blocks_for):
            raise CapabilityMissing("the target offers no quota-bound block store", capability="blocks_for")
        return blocks_for(job)

    def _h_blocks_need(self, auth, params, body, req) -> dict:
        api, _job = self._tgt(auth, params)
        return {"missing": api.blocks().missing(self._ids(body))}

    def _h_put_blocks(self, auth, params, body, req) -> dict:
        api, _job = self._tgt(auth, params)
        if auth.epoch is None:
            raise BadRequest("X-Ctxh-Epoch header required", field="epoch")
        good, rejected = [], []
        for block_id, raw, reason in decode_frames(req.body, block_size=BLOCK_SIZE):
            if raw is None:
                rejected.append({"id": block_id, "reason": reason})
            else:
                good.append((block_id, raw))
        stored, sink_rejects = self._sink(api, _job).write_blocks(good)
        rejected.extend({"id": i, "reason": code} for i, code in sink_rejects)
        return {"stored": stored, "rejected": rejected}

    def _h_target_stage_context(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        return self._op(api.start_stage_context(job, self._decode(CapsuleInfo, body, "capsule")))

    def _h_target_prepare(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        return self._op(api.start_prepare(job, self._decode(PrepareRequest, body, "request")))

    def _h_target_apply_delta(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        return self._op(api.start_apply_delta(job, self._decode(DeltaInfo, body, "delta")))

    def _h_target_reset_session(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        return self._op(api.start_reset_session(job))

    def _h_target_commit(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        return self._op(api.start_commit(job))

    def _h_target_abort(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        api.abort(job, str(body.get("reason", "aborted"))[:200])
        return {"state": api.status(job).state}

    def _h_target_continue(self, auth, params, body, req) -> dict:
        api, job = self._tgt(auth, params)
        prompt = body.get("prompt", "")
        if not isinstance(prompt, str) or len(prompt) > MAX_PROMPT:
            raise BadRequest(f"prompt must be a string of at most {MAX_PROMPT} characters", field="prompt")
        result = api.continue_session(job, prompt)
        out = {"started": bool(result.started)}
        if result.pid is not None:
            out["pid"] = result.pid
        if result.log is not None:
            out["log"] = result.log
        return out
