"""Review round 4 (``REVIEW-3.md`` R3-H2): ``hostcompat.approvals_off`` — the continuation spawner's gate — refuses
where this process cannot identify the gateway that runs the turn, instead of answering "not off".

The gate's contract: True (approvals off), False (may start), or ``ContinueRefused`` with reason
``approvals_unchecked`` and ``why`` from ``UNIDENTIFIED_WHY``. The gateway files are real (Hermes' record format via
``_record`` of the round-3 tests); what "cannot identify" means is decided by Hermes' own ``gateway.status``."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

import pytest

from ctx_handover.engine import hostcompat
from ctx_handover.engine.errors import ContinueRefused
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.F.test_r3_f_h2 import _record, gateway

pytestmark = pytest.mark.skipif(not Path("/proc/self/cmdline").exists(), reason="the gateway stand-in needs /proc")


@pytest.fixture(autouse=True)
def _upstream():
    require_upstream()


def _dead_pid() -> int:
    gone = subprocess.Popen([sys.executable, "-c", "pass"])
    gone.wait(10)
    return gone.pid


def test_r4_f_gate_contract(tmp_path, monkeypatch):
    """★ The three answers of the gate. Red @ 88068475: ``approvals_off`` returned False for the unverified record
    and for an unreadable gateway status (``off is True`` only)."""
    import gateway.status as gs

    home = Home(tmp_path, "server")
    with home.active():
        assert hostcompat.approvals_off({}) is False  # no gateway: approvals apply
        assert hostcompat.approvals_off({"approvals": {"mode": "off"}}) is True
        _record(home, _dead_pid(), ["hermes", "gateway", "run"])
        with pytest.raises(ContinueRefused) as info:
            hostcompat.approvals_off({})
        assert info.value.details == {"reason": "approvals_unchecked", "why": "gateway_unverified"}
        assert "cannot identify" in info.value.message
        (home.hermes_home / "gateway_state.json").unlink()

        def boom(_home):
            raise OSError("unreadable")

        monkeypatch.setattr(gs, "live_gateway_pid_for_home", boom)
        with pytest.raises(ContinueRefused) as info:
            hostcompat.approvals_off({})
        assert info.value.details == {"reason": "approvals_unchecked", "why": "gateway_unknown"}
        assert hostcompat.approvals_off({"approvals": {"mode": "off"}}) is True  # the config still decides first


def test_r4_f_unidentified_why_matches_the_table():
    """``UNIDENTIFIED_WHY`` names exactly the "not checkable" answers that are not an identified other process."""
    not_checkable = {w for w, off in hostcompat.APPROVAL_WHY.items() if off is None}
    assert not_checkable - {"gateway_elsewhere"} == hostcompat.UNIDENTIFIED_WHY
    for why in hostcompat.UNIDENTIFIED_WHY:
        assert hostcompat.unchecked_text(why)


def _named_profile(tmp_path: Path):
    root = tmp_path / "root"
    profile_home = root / "profiles" / "work"
    profile_home.mkdir(parents=True)
    user_home = tmp_path / "uhome"
    user_home.mkdir()
    return root, profile_home, {"HERMES_HOME": str(profile_home), "HOME": str(user_home)}


def test_r4_f_unidentified_profile_gateway_is_not_hidden_behind_the_root_gateway(tmp_path, monkeypatch):
    """★ A named profile whose own gateway runs where this process cannot identify it (a fresh record in the profile
    home) while the Hermes root's gateway is identified here without ``--yolo``: the profile's gateway decides —
    unverified, refused — not the root's ("elsewhere", which would start).

    Red @ 88068475: ``('elsewhere', <root pid>, False)`` — the loop went on to the root after the profile's fresh
    record."""
    root, profile_home, env = _named_profile(tmp_path)
    for key, value in env.items():
        monkeypatch.setenv(key, value)
    assert hostcompat.hermes_root() == root
    rooted = type("R", (), {"hermes_home": root})()
    profiled = type("P", (), {"hermes_home": profile_home})()
    with gateway(rooted, tmp_path):
        _record(profiled, _dead_pid(), ["hermes", "-p", "work", "gateway", "run"])
        check = hostcompat.approval_check({})
        assert (check.off, check.why, check.pid) == (None, "gateway_unverified", None), check
        with pytest.raises(ContinueRefused):
            hostcompat.approvals_off({})


def test_r4_f_root_gateway_still_found_for_a_named_profile(tmp_path, monkeypatch):
    """Without a record in the profile home the root's gateway is the one (``gateway.pid``/``gateway_state.json`` of
    the Hermes root are read — README, Reads)."""
    _root, _profile_home, env = _named_profile(tmp_path)
    for key, value in env.items():
        monkeypatch.setenv(key, value)
    rooted = type("R", (), {"hermes_home": _root})()
    with gateway(rooted, tmp_path) as pid:
        check = hostcompat.approval_check({})
        assert (check.off, check.why, check.pid) == (None, "gateway_elsewhere", pid)
        assert hostcompat.approvals_off({}) is False
    assert os.environ["HERMES_HOME"] == env["HERMES_HOME"]
