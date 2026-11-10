"""Review round 4 — R3-L1 and R3-L3 (REVIEW-3 §3).

* **R3-L1** (g2): the first synchronous read of the veto index fails (ledger in DELETE mode, a foreign lock for longer
  than 5 s): turns pass without a veto until the thread's retry succeeds (≈ 8.5 s), and nothing said so. Decision:
  fail-open with a log line at both ends of that window, not a blocked turn. The veto is the fallback layer of the lock
  (DECISIONS R1): the handover's session marks (ended ``handed_over``, archived, hidden) hold without it — the same
  guarantee as with the plugin not loaded (README). Without a read ledger the index cannot tell which session is
  handed over, so "fail-closed for the affected session" would block every session of the profile.
* **R3-L3**: reason (k) ``repos_left`` came for a clone inside a merely *proposed* secondary repository too;
  without an answer (agent start, timeout) the job ended, although "without proposals" takes nothing of it along.
  Reason (k) now asks only for repositories that travel (an included item, or a proposed one under ``--yes``).

Red before the fix: no log record; reasons ``['proposed', 'repos_left']`` ⇒ ``cancel``."""

from __future__ import annotations

import logging
import sqlite3

from ctx_handover.engine import plan, surfaces
from ctx_handover.engine.lock import VetoIndex
from ctx_handover.engine.model import (
    Include,
    ItemKind,
    ItemRole,
    NotTransferred,
    Options,
    Origin,
    PlanItem,
    RefineReport,
    ResourcePlan,
)
from ctx_handover.tests.fakes.planning_fake import final_plan


class _LockedLedger:
    """A ledger whose reads fail while ``locked`` (``database is locked`` after the 5 s busy timeout)."""

    def __init__(self) -> None:
        self.locked = True

    def _check(self) -> None:
        if self.locked:
            raise sqlite3.OperationalError("database is locked")

    def data_version(self) -> int:
        self._check()
        return 1

    def veto_rows(self) -> list[dict]:
        self._check()
        row = {"session_id": "s1", "root_id": "s1", "job_id": "01JCJ0B0000000000000000000", "state": "retired"}
        return [{**row, "peer": "laptop"}]

    def arrival_rows(self) -> list[dict]:
        return []

    def veto_add(self, *a) -> None:
        self._check()


def test_r4_a_l1_a_failed_first_read_is_logged_and_so_is_its_end(caplog):
    """★ The first read fails: a warning names the window (messages in handed-over sessions are not withheld until
    the ledger is read); the read that ends it is logged too. ``start`` still never raises.

    Mutant ``L1-no-log`` (``VetoIndex.start`` without the warning) ⇒ no record."""
    led = _LockedLedger()
    idx = VetoIndex("unused.sqlite3", poll_s=3600, ledger_factory=lambda path: led)
    with caplog.at_level(logging.WARNING, logger="ctx_handover.lock"):
        idx.start()
        try:
            assert idx.refreshed_at is None and idx.lookup("s1") is None
            first = [r.getMessage() for r in caplog.records if r.name == "ctx_handover.lock"]
            assert any("could not be read yet" in m and "not withheld" in m for m in first), first
            led.locked = False
            assert idx.refresh_now(force=True)
            assert idx.lookup("s1") is not None
            after = [r.getMessage() for r in caplog.records if r.name == "ctx_handover.lock"]
            assert any("read now" in m for m in after), after
        finally:
            idx.stop()


def test_r4_a_l1_a_good_first_read_logs_nothing(caplog):
    """Counter-check: the normal start is quiet."""
    led = _LockedLedger()
    led.locked = False
    idx = VetoIndex("unused.sqlite3", poll_s=3600, ledger_factory=lambda path: led)
    with caplog.at_level(logging.WARNING, logger="ctx_handover.lock"):
        idx.start()
        idx.stop()
    assert not [r for r in caplog.records if r.name == "ctx_handover.lock"]


def _item(item_id: str, path: str, role: ItemRole, include: Include) -> PlanItem:
    origin = Origin.CWD if role is ItemRole.PRIMARY else Origin.TRANSCRIPT
    return PlanItem(item_id, ItemKind.GIT_REPO, path, role, origin, include, ("read: src/app.py",), 330)


def _final(secondary: Include):
    """The reviewer's ``probe-r3sec``: primary ``proj2`` (yes), secondary ``projects/b`` with the untracked clone
    ``vendor/y`` inside it (named ``WHY_NESTED_REPO``)."""
    rp = ResourcePlan(
        job_id="01JCJ0B0000000000000000000",
        revision=(1,),
        items=(
            _item("w0", "/home/u/real/proj2", ItemRole.PRIMARY, Include.YES),
            _item("w1", "/home/u/projects/b", ItemRole.SECONDARY, secondary),
        ),
        not_transferred=(NotTransferred("/home/u/projects/b/vendor/y", plan.WHY_NESTED_REPO),),
        processes=(),
        delegations=(),
        cron=(),
        refine=RefineReport(used="off"),
        tools_needed=(),
        env_vars_needed=(),
        notes_for_target="",
        approx_tokens=100,
    )
    return final_plan(rp)


def test_r4_a_l3_a_clone_in_a_proposed_repository_does_not_stop_an_unanswered_handover():
    """★ Without ``--yes`` the clone inside the proposed repository raises no reason (k): the only reason is (a), an
    unanswered confirmation (agent start, timeout) continues without proposals — and then nothing of ``b`` travels.
    The summary still names the clone (``repos_left``) for a person who answers the question about proposals.

    Red before the fix: ``['proposed', 'repos_left']`` and ``cancel``. Mutant ``L3-proposed-roots`` (reason (k) over
    every root that is not ``no``) ⇒ the same."""
    final = _final(Include.PROPOSED)
    reasons = plan.confirmation_reasons(final, Options(), False)
    assert reasons == [plan.CONFIRM_PROPOSED], reasons
    assert surfaces.choice_on_timeout(reasons) == "no_proposed"
    assert plan.repos_left(final) == [{"path": "/home/u/projects/b/vendor/y", "kind": "nested"}]
    settled = plan.apply_choice(final, "no_proposed")
    assert plan.repos_left(settled) == [] and plan.confirmation_reasons(settled, Options(), False) == []


def test_r4_a_l3_yes_still_asks_when_the_proposed_repository_goes_along():
    """``--yes`` takes the proposal along, so its clone stays behind with it: (k) asks, and ``--yes`` does not answer
    it (``YES_EXEMPT``, H15)."""
    reasons = plan.confirmation_reasons(_final(Include.PROPOSED), Options(yes=True), False)
    assert plan.CONFIRM_REPOS_LEFT in reasons and plan.CONFIRM_REPOS_LEFT in plan.yes_exempt(reasons)


def test_r4_a_l3_an_included_repository_still_asks():
    """A clone inside an included repository stays (k), with or without ``--yes``."""
    for options in (Options(), Options(yes=True)):
        assert plan.CONFIRM_REPOS_LEFT in plan.confirmation_reasons(_final(Include.YES), options, False)
