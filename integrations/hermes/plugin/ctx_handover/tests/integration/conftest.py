"""Fixtures of the integration suite: two homes, a listener process for home A, pairing (see harness.py).

``fault_world``: home A behind a supervised listener with listener-side fault rules, home B paired with
it — ``harness.FaultWorld``. Selection of the fault matrix (``test_int_fault_matrix_*.py``):

* marker ``slow`` — the full cross matrix; deselected unless ``CTXH_RUN_SLOW=1`` or a ``-m`` expression is given;
* ``CTXH_SHARD=<i>/<n>`` — keep only every n-th fault-matrix test starting at i (parallel runs on separate
  ``--basetemp`` directories); other tests are not affected.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import jobs
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.sessiondb import SessionBuilder
from ctx_handover.tests.integration.harness import FaultWorld, Home, Listener, pair

_HERE = Path(__file__).resolve().parent
_MATRIX_PREFIX = "test_int_fault_matrix"


def pytest_configure(config):
    config.addinivalue_line("markers", "slow: full fault cross matrix; deselected unless CTXH_RUN_SLOW=1")


def pytest_collection_modifyitems(config, items):
    run_slow = os.environ.get("CTXH_RUN_SLOW") == "1" or bool(config.getoption("markexpr", default=""))
    shard = os.environ.get("CTXH_SHARD", "")
    index, count = (int(x) for x in shard.split("/", 1)) if shard else (0, 1)
    keep, drop, seen = [], [], 0
    for item in items:
        path = Path(str(item.path))
        if path.parent != _HERE or not path.name.startswith(_MATRIX_PREFIX):
            keep.append(item)
            continue
        if item.get_closest_marker("slow") is not None and not run_slow:
            drop.append(item)
            continue
        if count > 1:
            mine = seen % count == index
            seen += 1
            if not mine:
                drop.append(item)
                continue
        keep.append(item)
    if drop:
        config.hook.pytest_deselected(items=drop)
        items[:] = keep


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    server, laptop = Home(tmp_path, "server"), Home(tmp_path, "laptop")
    yield server, laptop
    jobs._reset_for_tests()


@pytest.fixture
def online(homes):
    """``(server, laptop, listener, peer)``: home A served by a real listener process, home B paired with it."""
    server, laptop = homes
    server.identity()
    listener = Listener(server)
    try:
        peer = pair(server, laptop, listener)
        yield server, laptop, listener, peer
    finally:
        listener.stop()


@pytest.fixture
def fault_world(homes, tmp_path):
    """``harness.FaultWorld``: home A behind a supervised, fault-armed listener, home B paired with it."""
    server, laptop = homes
    server.identity()
    listener = Listener(server, faults=True, supervise=True)
    try:
        peer = pair(server, laptop, listener)
        yield FaultWorld(server, laptop, listener, peer, tmp_path / "matrix")
    finally:
        listener.stop()


def builder(home: Home) -> SessionBuilder:
    return SessionBuilder(home.db(), clock=FakeClock())
