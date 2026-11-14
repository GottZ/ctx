"""Identity rotation across processes (layer-2 finding, DESIGN §12.2): ``identity rotate`` runs in a CLI process
while the listener process of the same profile keeps running.

The listener is the integration harness's ``listener_app.py`` — the plugin router loaded by path and the server
built by the composition root ``jobs.listener_server()`` with the real ledger, identity and ``hostcompat`` of the
home — and the rotation is a second process (``proc_helpers rotate``) calling ``Identity.create(…, rotate=True)``
through the real ``.env`` functions, as the CLI verb does. Requests to the listener are signed with the *dialer's*
key; the listener's own key signs every response, names itself in ``hello`` and is what pairing pins. So "the old
key is refused, the new one counts" means here: after the rotation nothing the listener sends verifies with the old
key, everything verifies with the new one, and pairing with the new fingerprint works — without a restart.
"""

from __future__ import annotations

import json
import subprocess
import sys
from dataclasses import replace

import pytest

from ctx_handover.engine import hostcompat, jobs, pairing, wire
from ctx_handover.engine.errors import BadResponseSignature
from ctx_handover.engine.model import GateAuth, GateMode, Right
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration.harness import GATE_ENV, Home, Listener, pair
from ctx_handover.tests.unit.C.helpers import child_env


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    yield Home(tmp_path, "server"), Home(tmp_path, "laptop"), Home(tmp_path, "tablet")
    jobs._reset_for_tests()


def _rotate_in_cli_process(home: Home) -> str:
    ledger_path = home.data_dir() / "ledger.sqlite3"
    proc = subprocess.run(  # noqa: S603 - this interpreter and a module of the test tree
        [sys.executable, "-m", "ctx_handover.tests.unit.C.proc_helpers", "rotate", str(ledger_path), "-"],
        env=child_env(**home.env()),
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    assert proc.returncode == 0, proc.stderr[-3000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])["pubkey"]


def test_listener_process_follows_identity_rotate_of_the_cli(homes):
    """★ Layer-2 finding: the running listener process loads the identity another process rotated, on the next
    request: the unsigned ``hello`` names the new key, a dialer that pinned the old key rejects the responses
    (``bad_response_signature``), one that pins the new key is served, and a new pairing with the new fingerprint
    succeeds — no restart, and nothing is signed with the retired key any more.

    Red proof: mutant = ``Server.current_identity`` returns the identity it was built with (no re-read) → the
    unsigned ``hello`` still names the old key.
    """
    server, laptop, tablet = homes
    old = server.identity().public_b64()
    listener = Listener(server)
    try:
        peer = pair(server, laptop, listener)
        ident = laptop.identity()

        def client(p):
            return wire.WireClient(p, ident, read_secret=hostcompat.read_secret)

        with laptop.active():
            assert client(peer).hello(signed=True).listener_pubkey == old
        new = _rotate_in_cli_process(server)
        assert new != old
        with laptop.active():
            assert client(replace(peer, pubkey="", peer_id="")).hello(signed=False).listener_pubkey == new
            with pytest.raises(BadResponseSignature):
                client(peer).hello(signed=True)
            assert client(replace(peer, pubkey=new)).hello(signed=True).listener_pubkey == new
        with server.active():
            code = pairing.create_code(jobs._profile().ledger, rights=frozenset({Right.PULL}), name_hint="", ttl_s=600)
        text = pairing.pair_string(
            code, url=listener.url, fingerprint=server.identity().fingerprint(), name="server", profile=None
        )
        tablet_ident = tablet.identity()
        tablet.extra_env[GATE_ENV] = listener.token
        with tablet.active():
            paired = pairing.redeem_dialer(
                lambda p: wire.WireClient(p, tablet_ident, read_secret=hostcompat.read_secret),
                tablet_ident,
                pairing.parse_pair_string(text),
                name="server",
                gate=GateAuth(mode=GateMode.SESSION_TOKEN, token_env=GATE_ENV),
                allow_insecure=True,
                ledger=jobs._profile().ledger,
            )
        assert paired.pubkey == new
    finally:
        listener.stop()
