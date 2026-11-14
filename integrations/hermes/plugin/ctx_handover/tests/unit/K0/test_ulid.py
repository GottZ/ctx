"""K0: ``ids.ULID_RE``/``ids.is_ulid`` — the one job/peer/offer id check (contract K-2, X25, W048)."""

from __future__ import annotations

import re

import pytest

from ctx_handover.engine import ids

C_COPY = re.compile(r"[0-9A-HJKMNP-TV-Z]{26}")
"""The pattern cluster C copied three times (``server.py``, ``pairing.py``, ``bundle.py``) before K-2."""


def test_every_generated_ulid_matches():
    for _ in range(2000):
        value = ids.ulid()
        assert ids.ULID_RE.fullmatch(value) and ids.ULID_RE.match(value) and ids.is_ulid(value)


def test_ulid_bounds_and_alphabet():
    assert ids.is_ulid("0" * 26)
    assert ids.is_ulid("7" + "Z" * 25)
    assert ids.is_ulid("01JC5ABCDEFGHJKMNPQRSTVWXY")
    assert set(ids.CROCKFORD_ALPHABET) == {ch for ch in map(chr, range(128)) if ids.ULID_RE.match("0" * 25 + ch)}


@pytest.mark.parametrize(
    "value",
    [
        "8" + "0" * 25,  # above 2**128 - 1
        "Z" * 26,
        "01jc5abcdefghjkmnpqrstvwxy",  # lower case is not canonical
        "01JC5ABCDEFGHJKMNPQRSTVWXI",  # I, L, O, U are decoder aliases only
        "01JC5ABCDEFGHJKMNPQRSTVWXL",
        "01JOBLIVE00000000000000000",
        "01JC5ABCDEFGHJKMNPQRSTVWXU",
        "0" * 25,
        "0" * 27,
        "0" * 26 + "\n",
        "\n" + "0" * 26,
        "",
        None,
        26,
        b"0" * 26,
    ],
)
def test_non_canonical_values_do_not_match(value):
    assert not ids.is_ulid(value)
    if isinstance(value, str):
        assert ids.ULID_RE.match(value) is None
        assert ids.ULID_RE.search(value) is None


def test_k0_rule_is_the_c_copy_narrowed_to_128_bits():
    """Everything ``ULID_RE`` accepts the old C copies accepted; the only values they accepted beyond it start
    with ``8``–``Z`` (more than 128 bits, never produced by ``ulid()``)."""
    for value in (ids.ulid() for _ in range(200)):
        assert C_COPY.fullmatch(value)
    for first in ids.CROCKFORD_ALPHABET:
        value = first + "0" * 25
        assert bool(C_COPY.fullmatch(value))
        assert ids.is_ulid(value) == (first in "01234567")
