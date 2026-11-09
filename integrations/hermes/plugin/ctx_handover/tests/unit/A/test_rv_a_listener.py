"""Review round rv-a, A parts of rv-c's findings: M15 (``inbound`` changed in the settings took effect only after a
restart — the composition root now hands the listener the live setting) and M13 (a revocation hit one ledger row,
not the key: after a second pairing the same key stayed valid through another inbound row)."""

from __future__ import annotations

import time

import pytest

from ctx_handover.engine import crypto, jobs
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Peer, Right, Role


@pytest.fixture
def runtime_env(fake_host):
    jobs._reset_for_tests()
    yield fake_host
    jobs._reset_for_tests()


def test_rv_a_m15_listener_reads_inbound_on_every_request(runtime_env):
    """The listener is built once per process; ``inbound: off`` set afterwards (settings form, ``config.yaml``) must
    switch the routes off without a restart."""
    host = runtime_env
    host.config.setdefault("plugins", {}).setdefault("entries", {})["ctx_handover"] = {"settings": {"inbound": "on"}}
    rt = jobs.ensure_started(None)
    crypto.Identity.create(
        rt.ledger, env_writable=host.env_writable, save_env=host.save_env, read_env_fresh=host.read_env_fresh
    )
    srv = jobs.listener_server()
    assert srv is not None and srv._inbound_active()
    host.config["plugins"]["entries"]["ctx_handover"]["settings"]["inbound"] = "off"
    assert jobs.listener_server() is srv
    assert not srv._inbound_active()


def _peer(name: str, pubkey: str) -> Peer:
    return Peer(
        name=name,
        peer_id=ulid(),
        direction="inbound",
        url=None,
        pubkey=pubkey,
        rights=frozenset({Right.PUSH, Right.PULL}),
        created_at=time.time(),
    )


def test_rv_a_m13_revoke_takes_every_row_of_the_key(tmp_path, clock):
    """``/handover peers revoke laptop-2`` after a second pairing of the same key: the first row (``laptop``) stops
    working too, and the jobs of both rows are aborted."""
    led = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    first, second = _peer("laptop", "KEY-A"), _peer("laptop-2", "KEY-A")
    other = _peer("phone", "KEY-B")
    for p in (first, second, other):
        led.upsert_peer(p)
    job = ulid()
    led.create_job(job, Role.TARGET, Mode.PUSH, first.peer_id, {"role": "target"})

    ids = led.revoke_peer("laptop-2")

    assert led.inbound_peer(first.peer_id) is None and led.inbound_peer(second.peer_id) is None
    assert led.inbound_peer(other.peer_id) is not None
    assert ids == [job] and led.job(job, Role.TARGET)["state"] == "aborting"
    led.close()


def test_rv_a_m13_a_new_pairing_replaces_the_old_rows_of_its_key(tmp_path, clock):
    """Redeeming a pairing code with a key that already has an inbound row revokes that row in the same
    transaction (its open jobs go ``aborting``; the listener's sweep finishes them): one key, one live row."""
    led = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    old = _peer("laptop", "KEY-A")
    led.upsert_peer(old)
    job = ulid()
    led.create_job(job, Role.TARGET, Mode.PUSH, old.peer_id, {"role": "target"})
    led.put_pair_code("code1", "secret", frozenset({Right.PUSH}), "laptop", clock() + 600)
    new = _peer("laptop", "KEY-A")

    assert led.redeem_pair_code("code1", clock(), new) is True

    assert led.inbound_peer(old.peer_id) is None
    assert led.inbound_peer(new.peer_id) is not None
    assert led.job(job, Role.TARGET)["state"] == "aborting"
    led.close()
