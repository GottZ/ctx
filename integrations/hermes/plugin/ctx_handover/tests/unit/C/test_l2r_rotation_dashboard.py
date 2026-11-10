"""Layer R — V26 of the layer-2 contract: an identity rotation in another process of the profile counts for
the next request of a running listener, also through the dashboard path ``dashboard/plugin_api.py`` whose
``_server()`` caches the server object per profile and process.

Process 1 rotates (``Identity.create(…, rotate=True)`` through the real ``hostcompat`` of the home, as the CLI verb
``identity rotate`` does — twice). Process 2 is the listener: ``listener_app.py`` mounts the plugin router loaded
by path, exactly as Hermes does, and the server is built and cached by the first request before any rotation.
The dialer (this process) pins the listener key; a signed job-level route (``offers/list``) and the signed
``hello`` must verify with the newest key only.
"""

from __future__ import annotations

import json
import subprocess
import sys
from dataclasses import replace

import pytest

from ctx_handover.engine import hostcompat, jobs, wire
from ctx_handover.engine.errors import BadResponseSignature
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration.harness import Home, Listener, pair
from ctx_handover.tests.unit.C.helpers import child_env


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    yield Home(tmp_path, "server"), Home(tmp_path, "laptop")
    jobs._reset_for_tests()


def _rotate_in_another_process(home: Home) -> str:
    proc = subprocess.run(  # noqa: S603 - this interpreter and a module of the test tree
        [
            sys.executable,
            "-m",
            "ctx_handover.tests.unit.C.proc_helpers",
            "rotate",
            str(home.data_dir() / "ledger.sqlite3"),
            "-",
        ],
        env=child_env(**home.env()),
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    assert proc.returncode == 0, proc.stderr[-3000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])["pubkey"]


def test_l2r_rotation_reaches_the_cached_dashboard_server(homes):
    """V26: after each rotation in process 1 the cached dashboard server of process 2 answers the next request
    with the new key — a dialer pinned to the new key is served on a signed job route, one pinned to any older key
    gets ``bad_response_signature`` — without a restart and through ``/api/plugins/ctx_handover/v1/…``.

    Red proof: mutant = ``Server.current_identity`` keeps the identity the server was built with.
    """
    server, laptop = homes
    first = server.identity()
    keys, instance = [first.public_b64()], first.instance_id
    listener = Listener(server)
    try:
        peer = pair(server, laptop, listener)
        ident = laptop.identity()

        def call(pubkey: str) -> dict:
            # the listener id is known, so no unsigned hello runs first: the response signature alone decides
            client = wire.WireClient(
                replace(peer, pubkey=pubkey), ident, read_secret=hostcompat.read_secret, listener_id=instance
            )
            try:
                return client.call("POST", "offers/list", {})
            finally:
                client.close()

        with laptop.active():
            assert call(keys[0]) == {"offers": []}  # the dashboard path built and cached the server
        for _round in range(2):
            keys.append(_rotate_in_another_process(server))
            assert len(set(keys)) == len(keys)
            with laptop.active():
                assert call(keys[-1]) == {"offers": []}
                for old in keys[:-1]:
                    with pytest.raises(BadResponseSignature):
                        call(old)
                hello = wire.WireClient(replace(peer, pubkey=keys[-1]), ident, read_secret=hostcompat.read_secret)
                try:
                    assert hello.hello(signed=True).listener_pubkey == keys[-1]
                finally:
                    hello.close()
        assert any("/api/plugins/ctx_handover/v1/offers/list" in line for line in listener.requests())
    finally:
        listener.stop()
