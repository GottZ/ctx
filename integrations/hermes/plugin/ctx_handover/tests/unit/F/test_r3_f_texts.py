"""Review round 3, R2-L4 (the second review §3): the M27 fix refuses
the continuation with ``continue_refused`` / ``reason: source_given_back`` when the source took the session back during
the retire; the surfaces knew only ``approvals_off`` and ``unverified`` and explained every other refusal with
``allow_remote_continue`` — whoever followed that line switched a setting that changes nothing. After a push the first
line also still said "This session is now read-only here", which a reclaim had just made false.

The texts below are the ones ``render_final`` prints for such a job (the reviewer's probe input)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import surfaces
from ctx_handover.engine.errors import ContinueRefused
from ctx_handover.engine.model import Mode

MSG = "the source took the session back (reclaimed), so no turn starts on the target"
WARN = ["the source was not retired (reclaimed); the copies are independent now", f"continuation not started: {MSG}"]
ARRIVED = {"id": "20261109_141003_a1b2c3", "title": "T"}


def _final(mode: Mode, reason: str) -> str:
    err = ContinueRefused(MSG, details={"reason": reason})
    return surfaces.render_final(
        mode, None, "done", "server", None, continue_error=err, warnings=WARN, target_session=ARRIVED
    )


def test_r3_f_l4_push_given_back_names_the_reclaim():
    """★ Push (this gateway is the source and took the session back): no ``allow_remote_continue`` advice, no
    "read-only here"; the text says no turn started on the target and the copies are independent.

    Red before the fix: "Handed over, but server does not allow remote continuation (allow_remote_continue) …" and
    "This session is now read-only here"."""
    out = _final(Mode.PUSH, "source_given_back")
    assert "allow_remote_continue" not in out, out
    assert "read-only here" not in out, out
    assert "The agent was not started on server: this session was taken back here during the handover" in out, out
    assert "the two copies are independent" in out, out


def test_r3_f_l4_pull_given_back_names_the_source():
    """★ Pull/import (the session is here, the source took it back): no ``allow_remote_continue`` advice.

    Red before the fix: "Handed over, but continuation is not allowed here (allow_remote_continue) …"."""
    out = _final(Mode.PULL, "source_given_back")
    assert "allow_remote_continue" not in out, out
    assert (
        "The agent was not started here: the machine the session came from took it back during the handover" in out
    ), out


@pytest.mark.parametrize("mode", [Mode.PUSH, Mode.PULL])
def test_r3_f_l4_not_allowed_still_names_the_setting(mode):
    """Control: the refusal by the setting keeps its advice (the fallback is ``not_allowed`` only)."""
    out = _final(mode, "not_allowed")
    assert "allow_remote_continue" in out, out


@pytest.mark.parametrize("here", [False, True])
def test_r3_f_l4_unknown_reason_is_not_explained_as_the_setting(here):
    """A reason the surfaces do not know is named as it is, never explained as ``allow_remote_continue``."""
    out = surfaces.render_error(ContinueRefused("x", details={"reason": "something_new"}), "server", here=here)
    assert "allow_remote_continue" not in out and "something_new" in out, out
