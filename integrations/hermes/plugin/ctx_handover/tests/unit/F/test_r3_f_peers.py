"""Review round 3, R2-L1 (the second review §3, a reviewer probe): after ``/handover peers remove <server>`` the
settings form's token slot (``CTX_HANDOVER_PEER_TOKEN``) still held the server's token. No entry bound that value to the
server's address any more (``pairing.credential_owner`` compares only with stored entries), so the next pairing with
another address carried it there behind that address's gate.

Now ``peers remove`` also blanks the form's token/password slot when it holds the value the pairing copied for that
peer and no other entry uses that value — through the same writer and read-back, before the entry goes; a slot that
cannot be blanked keeps the entry. A slot holding another value (the next gateway's credential) and header variables
named in ``peer_extra_headers`` (the user's own) stay.

The profile ``.env`` is the settings form's store; the tests load it into the process environment before each command
the way a starting Hermes process does (``_fresh``)."""

from __future__ import annotations

import os

import pytest

from ctx_handover.engine import hostcompat, jobs, pairing
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Peer, Right
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration._int3_helpers import surface
from ctx_handover.tests.integration.harness import GATE_ENV, Home, Listener
from ctx_handover.tests.unit.F._docs import docs_text
from ctx_handover.tests.unit.F.test_rv_final_peers import _env_lines

PASSWORD_SLOT = "CTX_HANDOVER_PEER_PASSWORD"


def _fresh(home: Home) -> None:
    """A new Hermes process: the profile ``.env`` is in its environment (``Home`` keeps the form slot out of
    ``os.environ`` between commands)."""
    value = _env_lines(home).get(GATE_ENV)
    if value:
        home.extra_env[GATE_ENV] = value
    else:
        home.extra_env.pop(GATE_ENV, None)


def _run(home: Home, line: str):
    _fresh(home)
    call, _ = surface(home, line)
    return call, "\n".join(call.out)


@pytest.fixture
def paired(tmp_path):
    """The laptop's form slot (in its ``.env``) holds the server's token; ``hermes handover pair`` copied it into
    ``CTX_HANDOVER_PEER_TOKEN_<peer id>``."""
    require_upstream()
    jobs._reset_for_tests()
    server, laptop = Home(tmp_path, "server"), Home(tmp_path, "laptop")
    ident = server.identity()
    listener = Listener(server)
    names = [GATE_ENV]
    try:
        with server.active():
            code = pairing.create_code(
                jobs._profile().ledger, rights=frozenset({Right.PULL}), name_hint="laptop", ttl_s=600
            )
            text = pairing.pair_string(
                code, url=listener.url, fingerprint=ident.fingerprint(), name="server", profile=None
            )
        with laptop.active():
            hostcompat.save_env(GATE_ENV, listener.token)  # what the settings form does
        call, out = _run(laptop, f"pair {text} --allow-insecure --json")
        assert call.exit_code == 0, out
        (peer,) = [p for p in laptop.ledger().peers() if p.direction == "dial"]
        own = pairing.PEER_TOKEN_PREFIX + peer.peer_id
        names.append(own)
        assert _env_lines(laptop).get(own) == listener.token == _env_lines(laptop).get(GATE_ENV)
        yield laptop, listener, peer, own
    finally:
        listener.stop()
        jobs._reset_for_tests()
        for name in names:
            os.environ.pop(name, None)


def test_r3_f_l1_remove_blanks_the_form_slot_that_held_only_this_peer(paired):
    """★ The reviewer's scenario: after ``peers remove server`` neither the peer's own variable nor the form slot
    holds the server's token, so no later pairing can carry it anywhere.

    Red before the fix: ``.env`` kept ``CTX_HANDOVER_PEER_TOKEN=<token>`` after the removal."""
    laptop, listener, peer, own = paired
    call, out = _run(laptop, "peers remove server")
    assert call.exit_code == 0, out
    assert [p for p in laptop.ledger().peers(include_revoked=True) if p.peer_id == peer.peer_id] == []
    env = _env_lines(laptop)
    assert env.get(own) in ("", None) and env.get(GATE_ENV) in ("", None), env
    assert listener.token not in (laptop.hermes_home / ".env").read_text(encoding="utf-8")
    _fresh(laptop)  # the next process
    with laptop.active():
        assert hostcompat.read_env_fresh(GATE_ENV) is None and not hostcompat.read_secret(GATE_ENV)


def test_r3_f_l1_a_slot_with_another_value_stays(paired):
    """The form already holds the next gateway's credential: it is not this peer's, so it stays."""
    laptop, _listener, _peer, own = paired
    with laptop.active():
        hostcompat.save_env(GATE_ENV, "next-gateway-token")
    call, out = _run(laptop, "peers remove server")
    assert call.exit_code == 0, out
    env = _env_lines(laptop)
    assert env.get(GATE_ENV) == "next-gateway-token" and env.get(own) in ("", None), env


def test_r3_f_l1_a_value_another_entry_uses_stays(paired):
    """Another entry (another profile of the same dashboard) holds the same value in its own variable: the value
    stays bound to that entry's address, so the slot stays too."""
    laptop, listener, peer, _own = paired
    twin_id = ulid()
    twin = Peer(
        name="server-ops",
        peer_id=twin_id,
        direction="dial",
        url=peer.url,
        pubkey=peer.pubkey,
        rights=peer.rights,
        gate=pairing.peer_gate(peer.gate, twin_id),
    )
    with laptop.active():
        jobs._profile().ledger.upsert_peer(twin)
        hostcompat.save_env(pairing.PEER_TOKEN_PREFIX + twin_id, listener.token)
    try:
        call, out = _run(laptop, "peers remove server")
        assert call.exit_code == 0, out
        assert _env_lines(laptop).get(GATE_ENV) == listener.token
    finally:
        os.environ.pop(pairing.PEER_TOKEN_PREFIX + twin_id, None)


def test_r3_f_l1_a_slot_that_cannot_be_blanked_keeps_the_entry(paired, monkeypatch):
    """The ``.env`` refuses the slot's write: nothing is removed (the peer's own copy stays, so a second attempt still
    recognises the slot), the reply names the slot; once the write goes through the same command removes both."""
    laptop, listener, peer, own = paired
    real = hostcompat.save_env

    def refuse_slot(name, value):
        if name != GATE_ENV:
            real(name, value)

    monkeypatch.setattr(hostcompat, "save_env", refuse_slot)
    call, out = _run(laptop, "peers remove server")
    assert call.exit_code != 0 and GATE_ENV in out and "not removed" in out, out
    assert [p.peer_id for p in laptop.ledger().peers() if p.direction == "dial"] == [peer.peer_id]
    assert _env_lines(laptop).get(own) == listener.token
    monkeypatch.undo()
    call, out = _run(laptop, "peers remove server")
    assert call.exit_code == 0, out
    env = _env_lines(laptop)
    assert env.get(GATE_ENV) in ("", None) and env.get(own) in ("", None), env


def test_r3_f_l1_readme_says_what_remove_blanks():
    """★ Disclosure (Writes): the README names the slot rule exactly; "the settings form's own fields stay as they
    are" is gone.

    Red before the change: the README said the form's fields stay."""
    text = " ".join(docs_text().split())
    assert "the settings form's own fields stay as they are" not in text
    assert (
        "and the form's token or password field when it still holds that peer's value and no other paired gateway "
        "uses it" in text
    )
    assert "variables you named in `peer_extra_headers` stay as they are" in text
