"""R36 / L3-F19 (package i-r36), the conductor's way: a workspace that holds more than the session's folder (D's
reason (j) ``workspace_differs``, in ``plan.YES_EXEMPT``) is asked about even with ``--yes``; without an answer the job
ends before the lock. A needs no change for it — ``conductor._confirm`` asks for every ``plan.yes_exempt`` reason —
and these tests pin that over the real ``Conductor`` and ``Ledger`` (unit ``Harness``, World fakes).

``FakePlanning`` raises only (a), (b), (g), (i) (the World's items are extras outside the workspace, so D's full table
would raise (f) in every test); the subclass here adds D's own (j) from ``plan.confirmation_reasons``."""

from __future__ import annotations

from dataclasses import replace

import pytest

from ctx_handover.engine import plan as planning
from ctx_handover.engine.errors import Cancelled
from ctx_handover.engine.model import Options, Role
from ctx_handover.tests.fakes.apis_fake import FakePlanning, make_item
from ctx_handover.tests.unit.A.harness import Harness


class _WithWorkspaceReason(FakePlanning):
    def confirmation_reasons(self, final, options, source_offline, **kw):
        reasons = super().confirmation_reasons(final, options, source_offline, **kw)
        if planning.CONFIRM_WORKSPACE_DIFFERS in planning.confirmation_reasons(final, options, source_offline, **kw):
            reasons.append(planning.CONFIRM_WORKSPACE_DIFFERS)
        return reasons


def _harness(tmp_path, clock, *, rel_cwd: str | None) -> Harness:
    h = Harness(tmp_path, clock, options=Options(yes=True))
    h.planning = _WithWorkspaceReason()
    h.world.source.items[0] = replace(make_item("w0", "/srv/app"), rel_cwd=rel_cwd, cwd_bytes=40 if rel_cwd else None)
    return h


def _conductor_ctx(h: Harness) -> dict:
    out: dict = {}
    for e in h.ledger.events(h.job_id, Role.CONDUCTOR):
        data = e["data"] or {}
        if isinstance(data.get("ctx"), dict):
            out.update(data["ctx"])
    return out


WORKSPACE = {"path": "/srv/app", "bytes": 100, "cwd": "/srv/app/services/api", "cwd_bytes": 40, "differs": True}


@pytest.mark.parametrize("answer", [None, "yes"])
def test_yes_does_not_confirm_a_workspace_that_differs_from_the_cwd(tmp_path, clock, answer):
    """★ R36 over the conductor: the session's folder is ``services/api`` of the workspace ``/srv/app``. With ``--yes``
    the user is asked anyway (``yes_exempt`` = ``workspace_differs``) and the question's summary names both folders;
    without an answer the job ends ABORTED before the lock; an explicit ``yes`` goes ahead and the journaled pre-send
    summary (ctx ``presend``) names the workspace.

    Red on ``ae0749a5``: ``AttributeError: module 'ctx_handover.engine.plan' has no attribute
    'CONFIRM_WORKSPACE_DIFFERS'`` (no reason (j); ``--yes`` went through without a question).

    Mutant (A): ``_confirm`` asks only when ``not options.yes`` (drops the ``yes_exempt`` clause)."""
    h = _harness(tmp_path, clock, rel_cwd="services/api")
    asked: list[dict] = []

    def confirm(info):
        asked.append(info)
        return answer

    c = h.conductor(confirm=confirm)
    if answer is None:
        with pytest.raises(Cancelled):
            c.run_until_negotiated()
        assert h.state() == "aborted" and "lock" not in h.calls("source"), h.states()
        h.world.assert_outcome("aborted")
    else:
        c.run_until_negotiated()
        c.run()
        assert h.state() == "done", h.states()
        assert _conductor_ctx(h)["presend"]["workspace"] == WORKSPACE
    assert len(asked) == 1, asked
    assert asked[0]["yes_exempt"] == [planning.CONFIRM_WORKSPACE_DIFFERS], asked[0]
    assert asked[0]["summary"]["workspace"] == WORKSPACE


def test_a_workspace_that_is_the_cwd_goes_with_yes(tmp_path, clock):
    """R36 (control): the session's folder is the workspace itself — ``--yes`` goes through without a question and the
    pre-send summary names the workspace with ``differs`` False."""
    h = _harness(tmp_path, clock, rel_cwd=None)
    asked: list[dict] = []
    c = h.conductor(confirm=lambda info: asked.append(info) or "yes")
    c.run_until_negotiated()
    c.run()
    assert h.state() == "done", h.states()
    assert asked == []
    assert _conductor_ctx(h)["presend"]["workspace"] == {
        "path": "/srv/app",
        "bytes": 100,
        "cwd": "/srv/app",
        "cwd_bytes": 100,
        "differs": False,
    }
