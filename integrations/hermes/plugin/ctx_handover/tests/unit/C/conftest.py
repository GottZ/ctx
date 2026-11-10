"""Fixtures of the cluster C suites (transport and security).

``ledger`` is the real ``engine.ledger.Ledger`` once cluster A's module exists, else the stub of
``ledger_stub.py`` (same methods, DDL of INTERFACES §7). ``plugin_data`` is a 0700 directory like
``<HERMES_HOME>/plugin-data/ctx_handover``. ``host`` is the K0 ``FakeHost`` installed over ``hostcompat``.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.tests.unit.C.ledger_stub import make_ledger


@pytest.fixture
def plugin_data(tmp_path) -> Path:
    path = tmp_path / "plugin-data" / "ctx_handover"
    path.mkdir(parents=True)
    os.chmod(path, 0o700)
    return path


@pytest.fixture
def ledger(plugin_data):
    return make_ledger(plugin_data / "ledger.sqlite3")


@pytest.fixture
def store(plugin_data, ledger):
    from ctx_handover.engine.blocks import BlockStore

    return BlockStore(plugin_data, ledger)


@pytest.fixture
def host(fake_host):
    return fake_host


def end_job(ledger, job_id: str, role, state: str = "done") -> None:
    """Move a job row to a terminal state (stub helper, or A's CAS API once the real ledger is used)."""
    if hasattr(ledger, "set_state"):
        ledger.set_state(job_id, role, state)
        return
    epoch = ledger.claim_owner(job_id, role, "test:0", 0.0)
    row = ledger.job(job_id, role)
    ledger.transition(job_id, role, row["state"], state, {"kind": "test"}, epoch=epoch)


@pytest.fixture
def make_identity(ledger, host):
    """Create (or load) this ledger's identity through the operator path with the fake host's .env."""

    def make(led=None):
        from ctx_handover.engine import hostcompat
        from ctx_handover.engine.crypto import Identity

        return Identity.create(
            led or ledger,
            env_writable=hostcompat.env_writable,
            save_env=hostcompat.save_env,
            read_env_fresh=hostcompat.read_env_fresh,
        )

    return make


def new_identity(ledger, env: dict | None = None):
    """An identity for a second ledger in the same test, with its own in-memory ``.env`` dict."""
    from ctx_handover.engine.crypto import Identity

    env = {} if env is None else env
    return Identity.create(ledger, env_writable=lambda name: None, save_env=env.__setitem__, read_env_fresh=env.get)


@pytest.fixture
def spawn_listener(tmp_path):
    """Factory for real HTTP listener processes; every started process is stopped by PID at teardown."""
    from ctx_handover.tests.unit.C.helpers import ListenerProcess

    started = []

    def spawn(name: str = "listener", **kw) -> ListenerProcess:
        proc = ListenerProcess(tmp_path / name, **kw).start()
        started.append(proc)
        return proc

    yield spawn
    for proc in started:
        proc.stop()
