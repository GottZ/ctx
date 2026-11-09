"""Identifiers (layer L1, stdlib only; INTERFACES §3.4, DESIGN Appendix A).

Job, peer, offer and instance ids are ULIDs (26 Crockford base32 characters: 48-bit millisecond time + 80 random
bits). ``ULID_RE``/``is_ulid`` are the one check for an id received from anywhere (contract K-2). Session ids
follow the core's own shape ``%Y%m%d_%H%M%S_<6 hex>`` (``hermes_state_ids.new_session_id``, local time like the
core).
"""

from __future__ import annotations

import re
import secrets
import time
from datetime import datetime

CROCKFORD_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

ULID_RE = re.compile(r"\A[0-7][0-9A-HJKMNP-TV-Z]{25}\Z")
"""A canonical ULID as ``ulid()`` writes it: 26 upper-case Crockford characters, the first one ``0``–``7`` (the
value has 128 bits, the 26 characters carry 130). Anchored, so ``match``, ``fullmatch`` and ``search`` agree;
lower case and the decoder aliases ``I L O U`` are not canonical and do not match."""


def _b32(value: int, length: int) -> str:
    chars = []
    for _ in range(length):
        chars.append(CROCKFORD_ALPHABET[value & 0x1F])
        value >>= 5
    return "".join(reversed(chars))


def ulid() -> str:
    """A new ULID (time-sortable to the millisecond, 80 random bits)."""
    millis = time.time_ns() // 1_000_000
    value = ((millis & ((1 << 48) - 1)) << 80) | secrets.randbits(80)
    return _b32(value, 26)


def is_ulid(value: object) -> bool:
    """True when ``value`` is a ``str`` in the canonical ULID form of ``ULID_RE``."""
    return isinstance(value, str) and ULID_RE.match(value) is not None


def crockford(nbytes: int) -> str:
    """``nbytes`` random bytes as Crockford base32 (``ceil(8 * nbytes / 5)`` characters)."""
    if nbytes <= 0:
        raise ValueError("nbytes must be positive")
    bits = 8 * nbytes
    length = -(-bits // 5)
    value = int.from_bytes(secrets.token_bytes(nbytes), "big") << (5 * length - bits)
    return _b32(value, length)


def short(job_id: str) -> str:
    """Short display form of a job id: its last six characters."""
    return job_id[-6:]


def mint_session_id(now: datetime | None = None) -> str:
    """A session id in the core's format ``%Y%m%d_%H%M%S_<6 hex>`` (local time, like ``new_session_id``)."""
    return (now or datetime.now()).strftime("%Y%m%d_%H%M%S_") + secrets.token_hex(3)
