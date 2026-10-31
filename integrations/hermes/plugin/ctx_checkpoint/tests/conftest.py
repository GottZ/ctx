"""Test isolation for the ctx_checkpoint umbrella suite.

The provider resolves the Hermes home through ``hermes_constants.get_hermes_home()`` when a
test does not pass ``hermes_home=``; point it at a temporary directory so no test reads or
writes the real profile (``ctx.json``, ``state.db``, ``ctx_state/``).
"""

from __future__ import annotations

import pytest


@pytest.fixture(autouse=True)
def _isolated_hermes_home(tmp_path, monkeypatch):
    home = tmp_path / "isolated-hermes-home"  # test_setup's own fixture uses "hermes-home"
    home.mkdir(exist_ok=True)
    # hermes_constants.get_hermes_home() reads the env var at call time (override → env →
    # default); test_setup's own ``hermes_home`` fixture re-points it the same way.
    monkeypatch.setenv("HERMES_HOME", str(home))
    yield home
