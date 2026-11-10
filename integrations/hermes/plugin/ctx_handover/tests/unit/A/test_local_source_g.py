"""Layer G for ``LocalSource`` (cluster A core): V17 — a lost source names its state; the explicit-success sweep —
the prepare re-check counts nothing it did not see as "unchanged"."""

from __future__ import annotations

from dataclasses import replace

import pytest

from ctx_handover.engine.errors import LeaseLost
from ctx_handover.engine.model import DestMode, Include, ItemKind, ItemRole, OpState, PlanItem, PrepareOutcome
from ctx_handover.tests.unit.A.test_local_source import _prepared, mk

assert mk


@pytest.mark.parametrize("state", ["reclaimed", "released", "suspended"])
def test_heartbeat_of_a_source_that_gave_up_names_its_state(mk, state):
    """V17: a source that is reclaimed (or released, suspended) answers the keepalive with ``LeaseLost`` whose
    details carry the state — route #9 turns it into ``{state}``/``source_reclaimed`` for the dialer."""
    e = mk(remote_driven=True)
    plan = e.plan()
    e.source.lock(e.job, e.ref, plan.revision)
    e.ledger.source_set(e.job.job_id, state)
    with pytest.raises(LeaseLost) as exc:
        e.source.heartbeat(e.job)
    assert exc.value.details.get("state") == state


def test_prepare_without_a_recorded_pack_is_not_unchanged(mk):
    """No pack recorded on this side (another process lost it) ⇒ ``stale_worktree`` (the conductor packs again) —
    before, the re-check was skipped and the primary lock applied."""
    e = mk(remote_driven=False)
    plan = e.plan()
    e.source.lock(e.job, e.ref, plan.revision)
    capsule = e.source.start_export(e.job).result
    st = e.source.start_prepare(e.job, _prepared(capsule))
    assert st.state is OpState.DONE and st.result["outcome"] == PrepareOutcome.STALE_WORKTREE
    assert e.ledger.source_state(e.job.job_id)["state"] == "locked"


def test_packed_git_item_without_a_recorded_state_counts_as_changed(mk, tmp_path):
    """A packed git item whose state is missing is not "unchanged" (re-pack); an item the target skips needs none."""
    e = mk(remote_driven=False)
    plan = e.plan()
    repo = PlanItem(
        item_id="w0",
        kind=ItemKind.GIT_REPO,
        source_path=str(tmp_path / "repo"),
        role=ItemRole.PRIMARY,
        origin=plan.items[0].origin,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=0,
    )
    final = e.final(plan, items=(*plan.items, repo))
    assert e.source._worktrees_unchanged(e.job, final, {}) is False
    skipped = replace(
        final, decisions=tuple(replace(d, mode=DestMode.SKIP) if d.item_id == "w0" else d for d in final.decisions)
    )
    assert e.source._worktrees_unchanged(e.job, skipped, {}) is True
