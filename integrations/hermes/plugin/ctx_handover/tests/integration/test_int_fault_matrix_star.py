"""★ cases of the fault matrix against the real modules: the conductor killed between prepare and
commit, the lost answer of the commit, an abort during retire. Every test names the mutant of its red proof;
setup and promises as in ``test_int_fault_matrix_std.py``.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine.model import Mode
from ctx_handover.tests.integration.harness import assert_case


@pytest.mark.parametrize("mode", [Mode.PUSH, Mode.PULL])
def test_int_fault_kill_conductor_after_source_prepared(fault_world, mode):
    """★ The conductor process is SIGKILLed right after the source reported ``prepared`` (primary lock applied,
    ``intent: commit`` not yet journaled). A later janitor adopts the job after the stale window and aborts it
    (DESIGN §3.6, conductor SOURCE_PREPARING ⇒ ABORTING): ABORTED, the release undid the primary lock, the target
    never activated. Mutant M1: ``Conductor._aborting`` leaves out ``source.release``."""
    res = fault_world.run_case(mode, "source", "complete:prepare", "crash_after")
    assert res.fault_fired, res.describe()
    assert_case(res)
    assert res.final == "aborted", res.describe()


@pytest.mark.parametrize("mode", [Mode.PUSH, Mode.PULL])
def test_int_fault_kill_conductor_before_commit_sent(fault_world, mode):
    """★ SIGKILL after ``intent: commit`` was journaled and before the commit request left. The janitor adopts the
    job: commit intended ⇒ IN_DOUBT; the target still reports ``prepared`` ⇒ the idempotent commit is started again
    ⇒ COMMITTED, RETIRED, DONE (DESIGN §3.6 row COMMITTING, §3.2 IN_DOUBT). Mutant M2: ``Conductor._committed``
    skips RETIRING (DONE without ``source.retire``)."""
    res = fault_world.run_case(mode, "target", "start_commit", "crash")
    assert res.fault_fired, res.describe()
    assert_case(res)
    assert res.final == "done", res.describe()
    assert "in_doubt" in res.states, res.describe()


@pytest.mark.parametrize(
    ("method", "kind"),
    [("start_commit", "disconnect_after"), ("complete:commit", "disconnect_after"), ("complete:commit", "down")],
)
def test_int_fault_lost_commit_answer(fault_world, method, kind):
    """★ Push: the commit request reached the target and its answer is lost — the answer of ``target/commit``
    itself, the status poll that reports the finished commit, or that poll and the whole link until the conductor
    process gives up (IN_DOUBT parks after ``stall_timeout``; the janitor of a later process resolves it from the
    target's status). Idempotent commit (DESIGN §3.7) ⇒ DONE, exactly one active copy. Mutants: M2 (retire left
    out) and M3 (``Conductor._source_preparing`` skips ``source.prepare``: the target activates while the source is
    usable — the invariant monitor)."""
    res = fault_world.run_case(Mode.PUSH, "target", method, kind)
    assert res.fault_fired, res.describe()
    assert_case(res)
    assert res.final == "done", res.describe()


@pytest.mark.parametrize(
    ("mode", "method", "kind"),
    [
        (Mode.PUSH, "start_retire", "cancel"),
        (Mode.PULL, "start_retire", "cancel"),
        (Mode.PULL, "start_retire", "down"),
        (Mode.PULL, "complete:retire", "interrupt"),
    ],
)
def test_int_fault_abort_during_retire(fault_world, mode, method, kind):
    """★ Abort during retire. After the commit point every failure drives forward (DESIGN §3.4 point 4): the user's
    cancel is refused (``Too late to cancel``), a link that stays down parks RETIRING and the janitor of a later
    process retries, a source process killed inside its retire operation continues the journaled sub-steps after
    its restart ⇒ DONE, the source retired. Mutants: M4 (``Conductor._retiring`` ends the job when its retries are
    exhausted instead of parking it) for the link case, M2 for all."""
    res = fault_world.run_case(mode, "source", method, kind)
    assert res.fault_fired, res.describe()
    assert_case(res)
    assert res.final == "done", res.describe()
