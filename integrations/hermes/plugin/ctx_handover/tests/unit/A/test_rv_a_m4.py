"""Review round rv-a, M4 (``review-code/REVIEW.md`` §2): the janitor tests and the fault matrix ran the janitor
without its participant half, and the fake source keyed its release like the production code did before B1 — so no
A test could reach B1. This drives the janitor with the participant half (what ``ParticipantRegistry.recover`` does
for this host's stale source row) and runs B1 as a matrix row: crash in the transfer phase ⇒ janitor ⇒ resume ⇒ the
target refuses the commit ⇒ the job aborts and the source is usable again."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from ctx_handover.engine.errors import ImportFailed
from ctx_handover.engine.model import OpState
from ctx_handover.tests.unit.A.test_jobs import RunnerEnv


def _participant_half(e: RunnerEnv) -> None:
    """``ParticipantRegistry.recover`` for this host's source row of a job whose conductor process died: a source in
    ``locking``/``locked``/``preparing`` is released (DESIGN §3.6)."""
    src = e.world.source
    if src.state in ("locking", "locked", "preparing"):
        job = SimpleNamespace(epoch=src.max_epoch, job_id=src.job_id)
        st = src.start_release(job, "owner_died")
        while st.state is OpState.RUNNING:
            st = src.op_status(job, st.op_id)
        assert st.state is OpState.DONE, st


@pytest.mark.parametrize("method", ["start_export", "start_pack"])
def test_rv_a_m4_b1_row_resume_then_refused_commit_frees_the_source(tmp_path, clock, method):
    e = RunnerEnv(tmp_path, clock)
    e.world.inject("source", method, "crash")
    job_id = e.start()
    e.run_crashing(job_id)
    did = e.janitor()
    assert e.state(job_id) == "suspended", did
    _participant_half(e)
    assert e.world.source.state == "released"

    e.world.inject("target", "start_commit", "error", error=ImportFailed("the target refused the commit"))
    e.runner.resume(job_id)
    e.runner.join(30)

    assert e.state(job_id) == "aborted"
    e.world.assert_outcome("aborted")  # source usable, no lease, no veto (promise b)
    e.ledger.close()


def test_rv_a_m4_janitor_table_with_the_participant_half(tmp_path, clock):
    """``test_janitor_actions_table`` expects ``locked`` after the janitor for the transfer phase — true for the
    conductor half alone; with the participant half the source is released and a resume locks it again."""
    e = RunnerEnv(tmp_path, clock)
    e.world.inject("source", "start_export", "crash")
    job_id = e.start()
    e.run_crashing(job_id)
    e.janitor()
    _participant_half(e)
    assert e.world.source.state == "released" and not e.world.source.leases
    e.runner.resume(job_id)
    e.runner.join(30)
    assert e.state(job_id) == "done"
    e.world.assert_outcome("done")
    e.ledger.close()
