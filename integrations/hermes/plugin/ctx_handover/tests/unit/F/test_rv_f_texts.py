"""First review round — surface texts:

* L18: after a pull or an import the continuation runs on this gateway, but its refusal named "the other
  gateway" (``render_error(continue_error, None)`` fell back to that peer text).
* Reason (j) ``workspace_differs`` after R36a (hand-over): only a working folder that holds none of the
  repository's tracked files asks; the text says so.
* Reason (k) ``repos_left`` (cluster D, H15 in R30's form): every reason code of D has a text and a letter here (X03).
"""

from __future__ import annotations

import pytest

from ctx_handover.engine import plan as planning
from ctx_handover.engine import surfaces
from ctx_handover.engine.errors import ContinueRefused
from ctx_handover.engine.model import CommitAck, Mode

SID = "20261109_120000_abcdef"


def _ack() -> CommitAck:
    return CommitAck(
        session_id=SID,
        root_id=SID,
        title="T",
        dests=(),
        path_map=(),
        model="m",
        offline_capable=True,
        warnings=(),
        committed_at=1.0,
    )


@pytest.mark.parametrize("mode", [Mode.PULL, Mode.IMPORT])
@pytest.mark.parametrize("reason", ["approvals_off", "not_allowed"])
def test_rv_f_l18_a_refused_continuation_here_says_here(mode, reason):
    """★ L18: pull/import, the continuation was refused on this gateway ⇒ the text says "here" and names no other
    gateway.

    Red before the fix: "approvals are switched off on the other gateway" / "the other gateway does not allow …"."""
    err = ContinueRefused("refused", reason=reason)
    text = surfaces.render_final(mode, _ack(), "done", "server", None, continue_error=err)
    line = next(x for x in text.splitlines() if x.startswith("Handed over, but"))
    assert "here" in line, line
    assert "the other gateway" not in line and "server" not in line, line


def test_rv_f_l18_a_push_still_names_the_peer():
    """L18: after a push the continuation is the target's — the refusal keeps naming that gateway."""
    err = ContinueRefused("refused", reason="approvals_off")
    text = surfaces.render_final(Mode.PUSH, _ack(), "done", "server", None, continue_error=err)
    assert "approvals are switched off on server" in text, text


def test_rv_f_l18_unverified_names_no_bundle_for_a_pull():
    """L18: a pull's source counts as unverified for a continuation (``plan.UNVERIFIED``); the text does not call it
    a bundle."""
    err = ContinueRefused("refused", reason=planning.UNVERIFIED)
    text = surfaces.render_final(Mode.PULL, _ack(), "done", "server", None, continue_error=err)
    line = next(x for x in text.splitlines() if x.startswith("Handed over, but"))
    assert "not verified" in line and "the bundle is" not in line, line


def test_rv_f_reason_texts_cover_every_code_of_d():
    """★ X03: every reason code of D (now (a)–(k)) has a text, its letter maps back, and the ``--yes``-exempt ones
    say so; (j) names the R36a condition, (k) the repository that stays.

    Red before the fix: ``AssertionError: repos_left`` (no text for D's new code (k))."""
    for code, letter in planning.CONFIRM_LETTERS.items():
        assert surfaces.REASON_TEXT.get(code), code
        assert surfaces.reason_names([letter]) == [code]
        if code in planning.YES_EXEMPT:
            assert "--yes does not confirm this" in surfaces.REASON_TEXT[code], code
    assert "none of its tracked files" in surfaces.REASON_TEXT[planning.CONFIRM_WORKSPACE_DIFFERS]
    assert "stays here" in surfaces.REASON_TEXT[planning.CONFIRM_REPOS_LEFT]
