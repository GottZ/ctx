"""Error hierarchy of ctx_handover (layer L0, stdlib only).

Every failure the plugin reports is a ``HandoverError`` with a stable ``code`` (DESIGN §4.4). Wire codes
travel in the error envelope ``{"error": {"code", "message", "retryable", "details"}}``; client-only codes
(``wire = False``) never leave the process — ``to_peer`` sends them as ``internal``.

``from_wire`` trusts only the *code* of a peer's envelope and only for wire codes: an unknown or client-only
code becomes a plain ``internal`` error carrying ``details["peer_code"]``, so a peer can never make this
process believe in a local condition such as ``in_doubt`` or ``lease_lost``. The envelope's boolean
``retryable`` is kept on the rebuilt error (contract V38: a participant that restarted answers with a known code
such as ``invalid_state`` and ``retryable: true``, and the conductor repeats the step instead of aborting); without
it the class's own flag applies. Message and details from a peer are capped.
"""

from __future__ import annotations

import json
from collections.abc import Callable
from typing import ClassVar

PEER_TEXT_CAP = 1024
"""Maximum characters of a message and bytes of encoded details sent to or accepted from a peer (§10.7)."""

_CODE_MAX = 64


class HandoverError(Exception):
    """Base class; also the ``internal`` error (HTTP 500, retryable, DESIGN §4.4).

    ``details`` is a JSON-compatible dict. Keyword arguments other than ``details``/``retryable`` are merged
    into ``details`` (``BadRequest("…", field="plan.items[2].kind")``).
    """

    code: ClassVar[str] = "internal"
    http_status: ClassVar[int] = 500
    retryable: bool = True
    wire: ClassVar[bool] = True

    _registry: ClassVar[dict[str, type[HandoverError]]] = {}

    def __init_subclass__(cls, **kwargs: object) -> None:
        super().__init_subclass__(**kwargs)
        if "code" in cls.__dict__:
            HandoverError._registry[cls.code] = cls

    def __init__(
        self, message: str = "", *, details: dict | None = None, retryable: bool | None = None, **fields: object
    ) -> None:
        super().__init__(message)
        self.message = str(message)
        merged = dict(details or {})
        merged.update(fields)
        self.details = merged
        if retryable is not None:
            self.retryable = bool(retryable)

    def __str__(self) -> str:
        return f"{self.code}: {self.message}" if self.message else self.code

    def to_wire(self) -> dict:
        """The error envelope of DESIGN §4.4, unredacted (use ``to_peer`` for anything leaving the process)."""
        return {
            "error": {
                "code": self.code,
                "message": self.message,
                "retryable": bool(self.retryable),
                "details": dict(self.details),
            }
        }

    def to_peer(self, redact: Callable[[str], str]) -> dict:
        """Envelope for a peer: client-only codes become ``internal``; message and details are redacted and
        capped at ``PEER_TEXT_CAP`` (DESIGN §10.7 "Error details sent to a peer")."""
        code = self.code if self.wire else "internal"
        message = redact(self.message)[:PEER_TEXT_CAP]
        try:
            encoded = redact(json.dumps(self.details, ensure_ascii=False, sort_keys=True, default=str))
            details = json.loads(encoded) if len(encoded.encode("utf-8")) <= PEER_TEXT_CAP else {"truncated": True}
            if not isinstance(details, dict):
                details = {"truncated": True}
        except (TypeError, ValueError):
            details = {"truncated": True}
        retryable = bool(self.retryable) if self.wire else True
        return {"error": {"code": code, "message": message, "retryable": retryable, "details": details}}

    @classmethod
    def from_wire(cls, status: int, body: dict) -> HandoverError:
        """Rebuild an error from a peer's envelope (or from a bare HTTP status when the body is not one).

        Known wire codes map to their class; anything else becomes an ``internal`` error with
        ``details["peer_code"]``. Either way the envelope's ``retryable`` is kept when it is a bool (contract V38),
        otherwise the class's own flag applies.
        """
        env = body.get("error") if isinstance(body, dict) else None
        if not isinstance(env, dict) or not isinstance(env.get("code"), str):
            err = HandoverError(f"HTTP {status} without an error envelope", status=int(status))
            err.retryable = int(status) >= 500
            return err
        code = env["code"][:_CODE_MAX]
        raw_message = env.get("message")
        message = raw_message[:PEER_TEXT_CAP] if isinstance(raw_message, str) else ""
        raw_details = env.get("details")
        details: dict = {}
        if isinstance(raw_details, dict):
            try:
                size = len(json.dumps(raw_details, ensure_ascii=False, default=str).encode("utf-8"))
            except (TypeError, ValueError):
                size = PEER_TEXT_CAP + 1
            details = dict(raw_details) if size <= PEER_TEXT_CAP else {"truncated": True}
        klass = HandoverError._registry.get(code)
        if klass is not None and klass.wire:
            err = klass(message, details=details)
        else:
            details["peer_code"] = code
            err = HandoverError(message, details=details)
        flag = env.get("retryable")
        if isinstance(flag, bool):
            err.retryable = flag
        return err


HandoverError._registry["internal"] = HandoverError


def error_class(code: str) -> type[HandoverError] | None:
    """The class registered for ``code`` (wire or client-only), or None."""
    return HandoverError._registry.get(code)


# ---------------------------------------------------------------------------------------------------------
# Wire codes (DESIGN §4.4)
# ---------------------------------------------------------------------------------------------------------


class InboundDisabled(HandoverError):
    """Listener inbound is off (§12.4); indistinguishable from a missing route."""

    code = "inbound_disabled"
    http_status = 404
    retryable = False


class Unauthorized(HandoverError):
    """Plugin signature invalid, unknown or revoked peer, clock skew or replay."""

    code = "unauthorized"
    http_status = 401
    retryable = False


class Forbidden(HandoverError):
    """The peer lacks the right for this route or role."""

    code = "forbidden"
    http_status = 403
    retryable = False


class VersionUnsupported(HandoverError):
    """No common wire major; ``details.capability`` names what is missing."""

    code = "version_unsupported"
    http_status = 400
    retryable = False


class CapabilityMissing(HandoverError):
    """A required capability (peer or host) is absent; ``details.capability``."""

    code = "capability_missing"
    http_status = 400
    retryable = False


class BadRequest(HandoverError):
    """Schema violation; ``details.field`` names the offending field path."""

    code = "bad_request"
    http_status = 400
    retryable = False


class PayloadTooLarge(HandoverError):
    """Body over the negotiated limit; the client shrinks and retries itself."""

    code = "payload_too_large"
    http_status = 413
    retryable = False


class NotFound(HandoverError):
    """Job, offer or session unknown."""

    code = "not_found"
    http_status = 404
    retryable = False


class JobConflict(HandoverError):
    """A job id reused with different parameters."""

    code = "job_conflict"
    http_status = 409
    retryable = False


class InvalidState(HandoverError):
    """Operation not allowed in the current state; ``details.state``, ``details.reason`` (``stale_epoch`` …)."""

    code = "invalid_state"
    http_status = 409
    retryable = False


class SessionBusy(HandoverError):
    """A turn runs or a compression lock is held; the user may retry or use ``--wait``."""

    code = "session_busy"
    http_status = 409
    retryable = True


class WaitTimeout(HandoverError):
    """``--wait`` gave up after ``wait_timeout_min``."""

    code = "wait_timeout"
    http_status = 409
    retryable = True


class AlreadyHandingOver(HandoverError):
    """Another job holds this lineage."""

    code = "already_handing_over"
    http_status = 409
    retryable = False


class StaleSession(HandoverError):
    """The source revision changed in a way that is not append-only (handled by the conductor)."""

    code = "stale_session"
    http_status = 409
    retryable = False


class StaleWorktree(HandoverError):
    """The source work tree changed after packing (handled by the conductor)."""

    code = "stale_worktree"
    http_status = 409
    retryable = False


class RestartLimit(HandoverError):
    """More than three restarts in one job."""

    code = "restart_limit"
    http_status = 409
    retryable = False


class PolicyDenied(HandoverError):
    """A path or item violates the file policy; ``details.items`` or ``details.reason``."""

    code = "policy_denied"
    http_status = 422
    retryable = False


class ConflictError(HandoverError):
    """Destination conflict that needs a decision; ``details.items``."""

    code = "conflict"
    http_status = 409
    retryable = False


class InsufficientStorage(HandoverError):
    """Not enough space; ``details.need_bytes``, ``details.free_bytes``, ``details.path``."""

    code = "insufficient_storage"
    http_status = 507
    retryable = False


class BlockCorrupt(HandoverError):
    """A block's content does not match its id; resend."""

    code = "block_corrupt"
    http_status = 422
    retryable = True


class VerifyFailed(HandoverError):
    """H_all/H_rows (or git integrity) mismatch after import or delta."""

    code = "verify_failed"
    http_status = 422
    retryable = False


class ImportFailed(HandoverError):
    """The core refused the import; ``details.hermes_error``."""

    code = "import_failed"
    http_status = 500
    retryable = False


class RateLimited(HandoverError):
    """Listener rate limit; honour ``Retry-After``."""

    code = "rate_limited"
    http_status = 429
    retryable = True


class PairInvalid(HandoverError):
    """Wrong, expired or burned pairing code."""

    code = "pair_invalid"
    http_status = 401
    retryable = False


class ContinueRefused(HandoverError):
    """The target does not allow remote continuation (``allow_remote_continue: false`` or approvals off)."""

    code = "continue_refused"
    http_status = 403
    retryable = False


# ---------------------------------------------------------------------------------------------------------
# Client-only codes (never on the wire, DESIGN §4.4 last paragraph, INTERFACES §3.2)
# ---------------------------------------------------------------------------------------------------------


class GateRejected(HandoverError):
    """401 without a response signature: the dashboard gate, not the plugin, refused the request."""

    code = "gate_rejected"
    http_status = 401
    retryable = False
    wire = False


class ListenerMissing(HandoverError):
    """404 "Plugin not found": ctx_handover is not installed or not enabled on the listener."""

    code = "listener_missing"
    http_status = 404
    retryable = False
    wire = False


class PeerUnreachable(HandoverError):
    """Connection failed or timed out."""

    code = "peer_unreachable"
    http_status = 503
    retryable = True
    wire = False


class BadResponseSignature(HandoverError):
    """A response's Ed25519 signature is missing or wrong."""

    code = "bad_response_signature"
    http_status = 502
    retryable = False
    wire = False


class HttpsRequired(HandoverError):
    """Plain http to a non-loopback peer without ``allow_insecure``."""

    code = "https_required"
    http_status = 400
    retryable = False
    wire = False


class LeaseLost(HandoverError):
    """The source turn lease or compression lock was lost while the job held it."""

    code = "lease_lost"
    http_status = 409
    retryable = False
    wire = False


class Cancelled(HandoverError):
    """The job was cancelled by the user."""

    code = "cancelled"
    http_status = 409
    retryable = False
    wire = False


class InDoubt(HandoverError):
    """The target commit outcome is unknown (DESIGN §3.2 IN_DOUBT)."""

    code = "in_doubt"
    http_status = 409
    retryable = True
    wire = False


class IdentityLost(HandoverError):
    """The identity key in the profile .env does not match the ledger's public key (§12.2)."""

    code = "identity_lost"
    http_status = 500
    retryable = False
    wire = False


class EnvNotWritable(HandoverError):
    """The profile .env cannot take the identity key (``require_env_writable`` refused)."""

    code = "env_not_writable"
    http_status = 500
    retryable = False
    wire = False


WIRE_CODES: frozenset[str] = frozenset(code for code, klass in HandoverError._registry.items() if klass.wire)
"""Every code that may appear in an envelope on the wire (``internal`` included)."""

CLIENT_CODES: frozenset[str] = frozenset(code for code, klass in HandoverError._registry.items() if not klass.wire)
"""Codes that exist only inside a process."""
