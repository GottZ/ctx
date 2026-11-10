"""First review round, point 2: ``/handover peers remove`` dropped a dial peer's entry
and left the credential the pairing had copied for that peer alone in the profile ``.env``
(``CTX_HANDOVER_PEER_TOKEN_<peer id>``, ``…_PASSWORD_<peer id>``, ``…_HEADER<n>_<peer id>``) — a secret no entry
named any more and nothing would ever clean up.

Now the removal first blanks those variables through the same ``.env`` path as the pairing (``pairing.SecretStore``:
Hermes' writer, then the read-back of R16) and only then drops the entry; a variable whose read-back still shows a
value keeps the entry, and the reply names the variable. Shared slots (the settings form's
``CTX_HANDOVER_PEER_TOKEN``, named by entries paired before per-peer copies) are never touched.

``peers revoke`` takes inbound rows only (``commands.py`` refuses a dial peer), and an inbound row never carries a gate
(``Ledger.redeem_pair_code`` stores ``gate=None``) — so it has no variable of the peer to blank; the premise is pinned
below."""

from __future__ import annotations

import os

import pytest

from ctx_handover.engine import hostcompat, jobs, pairing
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import GateAuth, GateMode, Peer, Right
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration._int3_helpers import surface
from ctx_handover.tests.integration.harness import GATE_ENV, Home, Listener, pair


def _env_lines(home: Home) -> dict[str, str]:
    path = home.hermes_home / ".env"
    out: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines() if path.exists() else []:
        if "=" in line and not line.lstrip().startswith("#"):
            key, _, value = line.partition("=")
            out[key.strip()] = value.strip().strip("'\"")
    return out


@pytest.fixture
def paired_per_peer(tmp_path):
    """The laptop paired with the server through ``hermes handover pair`` (H1: the token copied into the peer's own
    ``CTX_HANDOVER_PEER_TOKEN_<peer id>``)."""
    require_upstream()
    jobs._reset_for_tests()
    server, laptop = Home(tmp_path, "server"), Home(tmp_path, "laptop")
    ident = server.identity()
    listener = Listener(server)
    names: list[str] = []
    try:
        with server.active():
            code = pairing.create_code(
                jobs._profile().ledger, rights=frozenset({Right.PULL}), name_hint="laptop", ttl_s=600
            )
            text = pairing.pair_string(
                code, url=listener.url, fingerprint=ident.fingerprint(), name="server", profile=None
            )
        laptop.extra_env[GATE_ENV] = listener.token
        call, _ = surface(laptop, f"pair {text} --allow-insecure --json")
        assert call.exit_code == 0, call.out
        (peer,) = [p for p in laptop.ledger().peers() if p.direction == "dial"]
        name = pairing.PEER_TOKEN_PREFIX + peer.peer_id
        names.append(name)
        assert peer.gate is not None and peer.gate.token_env == name, peer.gate
        assert _env_lines(laptop).get(name) == listener.token
        yield laptop, listener, peer, name
    finally:
        listener.stop()
        jobs._reset_for_tests()
        for name in names:  # Hermes' writer also publishes into os.environ; Home.active() does not restore these
            os.environ.pop(name, None)


def test_rv_final_peers_remove_blanks_the_peer_credential(paired_per_peer):
    """★ ``hermes handover peers remove server`` ⇒ the entry is gone and ``CTX_HANDOVER_PEER_TOKEN_<peer id>`` is
    blank in the profile ``.env`` (Hermes' reader confirms it), the form's slot is left alone.

    Red before the fix: ``assert '<token>' in ('', None)`` — the token stayed in ``.env`` after the removal."""
    laptop, listener, peer, name = paired_per_peer
    call, _ = surface(laptop, "peers remove server")
    assert call.exit_code == 0, call.out
    assert [p for p in laptop.ledger().peers(include_revoked=True) if p.peer_id == peer.peer_id] == []
    assert _env_lines(laptop).get(name) in ("", None), _env_lines(laptop).get(name)
    with laptop.active():
        assert hostcompat.read_env_fresh(name) is None
    assert listener.token not in (laptop.hermes_home / ".env").read_text(encoding="utf-8")


def test_rv_final_peers_remove_keeps_the_entry_when_the_env_refuses(paired_per_peer, monkeypatch):
    """★ Hermes refuses the write silently (``save_env_value`` returns under a managed ``.env``): the read-back still
    shows the token ⇒ nothing is removed, the reply names the variable and the step; once the write goes through, the
    same command removes the entry.

    Red before the fix: exit 0 and the entry gone while the token stayed (``assert 0 != 0``)."""
    laptop, listener, peer, name = paired_per_peer
    monkeypatch.setattr(hostcompat, "save_env", lambda _name, _value: None)
    call, _ = surface(laptop, "peers remove server")
    text = "\n".join(call.out)
    assert call.exit_code != 0, text
    assert name in text and "not removed" in text, text
    assert [p.peer_id for p in laptop.ledger().peers() if p.direction == "dial"] == [peer.peer_id]
    assert _env_lines(laptop).get(name) == listener.token
    monkeypatch.undo()
    call, _ = surface(laptop, "peers remove server")
    assert call.exit_code == 0, call.out
    assert [p for p in laptop.ledger().peers() if p.direction == "dial"] == []
    assert _env_lines(laptop).get(name) in ("", None)


@pytest.fixture
def paired_on_the_slot(tmp_path):
    """An entry paired the old way: its gate names the settings form's shared slot ``CTX_HANDOVER_PEER_TOKEN``."""
    require_upstream()
    jobs._reset_for_tests()
    server, laptop = Home(tmp_path, "server"), Home(tmp_path, "laptop")
    server.identity()
    listener = Listener(server)
    try:
        peer = pair(server, laptop, listener)
        yield laptop, peer
    finally:
        listener.stop()
        jobs._reset_for_tests()


def test_rv_final_peers_remove_never_blanks_a_shared_slot(paired_on_the_slot, monkeypatch):
    """The form's slot belongs to the next pairing (and may be named by other entries): removing an entry that names
    it writes no ``.env`` variable at all."""
    laptop, peer = paired_on_the_slot
    assert peer.gate is not None and peer.gate.token_env == GATE_ENV
    saved: list[tuple[str, str]] = []
    monkeypatch.setattr(hostcompat, "save_env", lambda n, v: saved.append((n, v)))
    call, _ = surface(laptop, f"peers remove {peer.name}")
    assert call.exit_code == 0, call.out
    assert saved == []
    assert [p for p in laptop.ledger().peers() if p.direction == "dial"] == []


def test_rv_final_own_secret_names_are_the_per_peer_copies_only():
    """``pairing.own_secret_names``: exactly the variables ``store_credentials`` wrote for this peer id — token or
    password, then each header — never a shared slot, nothing for a gate-less or non-ULID entry."""
    pid = ulid()
    token = GateAuth(mode=GateMode.SESSION_TOKEN, extra_headers=(("X-A", "PROXY_A"), ("X-B", "PROXY_B")))
    copied = pairing.peer_gate(token, pid)
    assert pairing.own_secret_names(copied, pid) == [
        pairing.PEER_TOKEN_PREFIX + pid,
        f"{pairing.PEER_HEADER_PREFIX}1_{pid}",
        f"{pairing.PEER_HEADER_PREFIX}2_{pid}",
    ]
    basic = pairing.peer_gate(GateAuth(mode=GateMode.BASIC_LOGIN, username="u"), pid)
    assert pairing.own_secret_names(basic, pid) == [pairing.PEER_PASSWORD_PREFIX + pid]
    assert pairing.own_secret_names(token, pid) == []  # the shared slots of the form
    assert pairing.own_secret_names(copied, ulid()) == []  # another peer's copies are not this peer's
    assert pairing.own_secret_names(None, pid) == []
    assert pairing.own_secret_names(copied, "not-a-ulid") == []


def test_rv_final_revoke_has_no_peer_variable_to_blank(tmp_path):
    """Premise of ``peers revoke`` (inbound rows only): an inbound row is stored without a gate, even when one is
    handed in — so it names no variable of its own."""
    from ctx_handover.engine.ledger import Ledger

    ledger = Ledger(tmp_path / "ledger.sqlite3")
    try:
        code = pairing.create_code(ledger, rights=frozenset({Right.PULL}), name_hint="laptop", ttl_s=600)
        pid = ulid()
        gate = pairing.peer_gate(GateAuth(mode=GateMode.SESSION_TOKEN), pid)
        peer = Peer(
            name="laptop",
            peer_id=pid,
            direction="inbound",
            url=None,
            pubkey="k" * 43,
            rights=frozenset({Right.PULL}),
            gate=gate,
        )
        assert ledger.redeem_pair_code(code.code_id, ledger.now(), peer) is True
        (stored,) = [p for p in ledger.peers() if p.peer_id == pid]
        assert stored.gate is None
        assert pairing.own_secret_names(stored.gate, pid) == []
    finally:
        ledger.close()


def test_rv_final_readme_says_remove_blanks_the_peer_variables():
    """★ Disclosure (Writes): ``peers remove`` blanks the peer's own variables through the same writer and read-back,
    and a value that stays keeps the entry.

    Red before the README change (README at ``b112d228``): the sentence is missing."""
    from pathlib import Path

    text = " ".join((Path(pairing.__file__).resolve().parents[1] / "README.md").read_text(encoding="utf-8").split())
    assert "`/handover peers remove` blanks that peer's variables the same way before it drops the entry" in text
    assert "a value still there afterwards keeps the entry and the reply names the variable" in text
