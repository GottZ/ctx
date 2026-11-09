"""Lab finding L3-F25 (``reports/L3-findings.md``; integrator decision for r3-f): a second pairing of this device with a
gateway it is already paired with — the same listener key, reached under a second address or a second name.

(a) Another address with the same dashboard token was refused by the H1 address binding with the remedy "enter the
dashboard token of <new address>", which cannot help: it is the same token. (b) Where the second pairing went through,
the listener revoked the first entry's row (review M13: one live row per device key), and the dialer's first entry was
dead without a word (``peers`` still listed it; only ``doctor`` found it).

Now the dialer refuses such a pairing before anything is sent and before the code is used — the pair line's key
fingerprint is the one of an entry stored here — names the feasible remedy (``/handover peers remove <name>``, then
pair again) and the first entry keeps working."""

from __future__ import annotations

import os

import pytest

from ctx_handover.engine import jobs, pairing
from ctx_handover.engine.model import Right
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration._int3_helpers import surface
from ctx_handover.tests.integration.harness import GATE_ENV, Home, Listener


def _code(server: Home, listener: Listener, ident, url: str) -> tuple[str, str]:
    with server.active():
        ledger = jobs._profile().ledger
        code = pairing.create_code(ledger, rights=frozenset({Right.PULL}), name_hint="laptop", ttl_s=600)
        text = pairing.pair_string(code, url=url, fingerprint=ident.fingerprint(), name="server", profile=None)
    return code.code_id, text


def _live_inbound(server: Home) -> list[str]:
    with server.active():
        return [p.name for p in jobs._profile().ledger.peers() if p.direction == "inbound"]


def _server_works(laptop: Home) -> str:
    call, _ = surface(laptop, "doctor")
    return next((x for x in "\n".join(call.out).splitlines() if "server" in x and "peer" in x), "")


@pytest.fixture
def paired(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    server, laptop = Home(tmp_path, "server"), Home(tmp_path, "laptop")
    ident = server.identity()
    listener = Listener(server)
    names: list[str] = []
    try:
        _code_id, text = _code(server, listener, ident, listener.url)
        laptop.extra_env[GATE_ENV] = listener.token
        call, _ = surface(laptop, f"pair {text} --allow-insecure --json")
        assert call.exit_code == 0, call.out
        (peer,) = [p for p in laptop.ledger().peers() if p.direction == "dial"]
        names.append(pairing.PEER_TOKEN_PREFIX + peer.peer_id)
        assert _live_inbound(server) == ["laptop"]
        yield server, laptop, listener, ident, peer
    finally:
        listener.stop()
        jobs._reset_for_tests()
        for name in names:
            os.environ.pop(name, None)


@pytest.mark.parametrize(
    "how",
    ["second-address", "same-address-second-name"],
)
def test_r3_f_f25_second_pairing_with_a_paired_gateway_is_refused_and_the_first_keeps_working(paired, how):
    """★ (a) the same gateway under ``localhost`` instead of ``127.0.0.1`` (same token); (b) the same address under a
    second name. Both are refused before anything is sent, the code stays unused, the reply names
    ``/handover peers remove server``, the listener keeps the first row and the first entry still answers.

    Red before the fix: (a) the H1 refusal "Enter the dashboard token of http://localhost:… in Settings" (no feasible
    remedy); (b) exit 0, the listener's row ``laptop`` revoked — ``server`` dead on the laptop."""
    server, laptop, listener, ident, _peer = paired
    url = listener.url.replace("127.0.0.1", "localhost") if how == "second-address" else listener.url
    _code_id, text = _code(server, listener, ident, url)
    extra = "" if how == "second-address" else " --name server-b"
    call, _ = surface(laptop, f"pair {text} --allow-insecure{extra}")
    out = "\n".join(call.out)
    assert call.exit_code == 3, out
    assert "already paired with that gateway as server" in out, out
    assert "/handover peers remove server" in out and "the pairing code was not used" in out, out
    assert [p.name for p in laptop.ledger().peers() if p.direction == "dial"] == ["server"]
    assert _live_inbound(server) == ["laptop"]
    line = _server_works(laptop)
    assert "[FAIL]" not in line, line


def test_r3_f_f25_after_remove_the_new_address_pairs(paired):
    """The named remedy works: ``peers remove server``, then the pairing line for the second address."""
    server, laptop, listener, ident, _peer = paired
    _code_id, text = _code(server, listener, ident, listener.url.replace("127.0.0.1", "localhost"))
    call, _ = surface(laptop, "peers remove server")
    assert call.exit_code == 0, call.out
    call, _ = surface(laptop, f"pair {text} --allow-insecure --json")
    assert call.exit_code == 0, call.out
    (again,) = [p for p in laptop.ledger().peers() if p.direction == "dial"]
    assert again.url.startswith("http://localhost:")
    os.environ.pop(pairing.PEER_TOKEN_PREFIX + again.peer_id, None)


def test_r3_f_f25_another_gateway_with_the_same_token_names_both_remedies(paired, tmp_path):
    """Another gateway (another key) whose dashboard takes the same token at another address: still refused by the
    address binding (H1), and the reply now also names the way that works — pair through the stored address, or remove
    the entry first — instead of only "enter the token of the new address"."""
    _server, laptop, _listener, _ident, _peer = paired
    other = Home(tmp_path, "other")
    other_ident = other.identity()
    with other.active():
        ledger = jobs._profile().ledger
        code = pairing.create_code(ledger, rights=frozenset({Right.PULL}), name_hint="laptop", ttl_s=600)
        text = pairing.pair_string(
            code, url="http://localhost:9", fingerprint=other_ident.fingerprint(), name="other", profile=None
        )
    call, _ = surface(laptop, f"pair {text} --allow-insecure")
    out = "\n".join(call.out)
    assert call.exit_code == 3, out
    assert "nothing was sent to http://localhost:9" in out, out
    assert "/handover peers remove server" in out, out
