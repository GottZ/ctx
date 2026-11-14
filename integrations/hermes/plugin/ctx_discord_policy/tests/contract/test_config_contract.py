"""S2 contract: operators are read through Hermes' own config loader, from the bot owner's home, and an
edit of config.yaml applies to the next read without a restart."""

from __future__ import annotations

import os
import sys
import time

import pytest
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, load_in

OP_A = "900000000000000011"
OP_B = "900000000000000012"
OP_ROUTED = "900000000000000013"


def _config(home, operators):
    ops = ", ".join(f'"{o}"' for o in operators)
    text = (f"plugins:\n  enabled: [{PLUGIN}]\n  entries:\n    {PLUGIN}:\n      settings:\n"
            f"        operators: [{ops}]\n")
    path = home / "config.yaml"
    before = path.stat().st_mtime_ns if path.exists() else 0
    path.write_text(text, encoding="utf-8")
    # Make the edit visible to a signature cache even on coarse-mtime filesystems.
    os.utime(path, ns=(max(time.time_ns(), before + 1_000_000), max(time.time_ns(), before + 1_000_000)))


@pytest.fixture
def homes(hermes, tmp_path, monkeypatch):
    _evict(ANCHOR)
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "root"))
    built = Homes(tmp_path)
    yield built
    _evict(ANCHOR)


def test_operator_edit_applies_without_restart_and_follows_the_owner_home(homes):
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    routed = homes.profile("routed")
    _config(homes.root, [OP_A])
    _config(routed, [OP_ROUTED])
    load_in(homes.root)
    load_in(routed)
    config = sys.modules[f"{ANCHOR}.config"]
    assert config.operators(homes.root) == {OP_A}

    _config(homes.root, [OP_B])
    assert config.operators(homes.root) == {OP_B}  # same process, no reload

    # A hook runs under the routed profile's scope; the owner's list still decides.
    token = set_hermes_home_override(str(routed))
    try:
        assert config.operators(homes.root) == {OP_B}
        assert config.operators(routed) == {OP_ROUTED}
    finally:
        reset_hermes_home_override(token)


def test_unreadable_config_yields_no_operator(homes):
    """Hermes serves its last-known-good copy for a broken config.yaml; an operator removed in that very
    edit must not keep authority from it: a broken config means no operator at all."""
    _config(homes.root, [OP_A, OP_B])
    load_in(homes.root)
    config = sys.modules[f"{ANCHOR}.config"]
    assert config.operators(homes.root) == {OP_A, OP_B}
    path = homes.root / "config.yaml"
    path.write_text(f"plugins:\n  enabled: [{PLUGIN}]\n  entries: [unclosed\n", encoding="utf-8")
    os.utime(path, ns=(time.time_ns() + 2_000_000, time.time_ns() + 2_000_000))
    assert config.operators(homes.root) == frozenset()
