"""Fixtures of the integration suite: two homes, a listener process for home A, pairing (see harness.py)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import jobs
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.sessiondb import SessionBuilder
from ctx_handover.tests.integration.harness import Home, Listener, pair


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


def builder(home: Home) -> SessionBuilder:
    return SessionBuilder(home.db(), clock=FakeClock())
