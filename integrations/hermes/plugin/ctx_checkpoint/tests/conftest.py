"""Test isolation for the ctx_checkpoint umbrella suite.

The provider resolves the Hermes home through ``hermes_constants.get_hermes_home()`` when a
test does not pass ``hermes_home=``; point it at a temporary directory so no test reads or
writes the real profile (``ctx.json``, ``state.db``, ``ctx_state/``).
"""

from __future__ import annotations

import sys

import pytest


@pytest.fixture(autouse=True)
def _isolated_hermes_home(tmp_path, monkeypatch):
    home = tmp_path / "isolated-hermes-home"  # test_setup's own fixture uses "hermes-home"
    home.mkdir(exist_ok=True)
    # hermes_constants.get_hermes_home() reads the env var at call time (override → env →
    # default); test_setup's own ``hermes_home`` fixture re-points it the same way.
    monkeypatch.setenv("HERMES_HOME", str(home))
    yield home


@pytest.fixture(autouse=True)
def _fresh_entry_states():
    """Each test starts with the server-entry states of a fresh process (GottZ/ctx#299): the test
    modules load the plugin under names of their own, each copy with its own registry."""

    def reset() -> None:
        for name, module in list(sys.modules.items()):
            if name.endswith(".memory.checkpoint") and callable(getattr(module, "reset_entry_states", None)):
                module.reset_entry_states()

    reset()
    yield
    reset()
