"""L3-F22 on the runner: ``JobRunner.cancel_answer``/``cancel`` say what happened to a cancel.

``ledger.set_cancel`` refuses a job whose conductor journaled ``intent: commit`` ("Too late to cancel: the target is
committing") before it looks at the state; every finished job journaled that intent, so the lab's ``cancel`` of a
``done`` pull answered "the target is committing" (lab finding L3-F22). The runner reads the state
first and answers with it; ``requested`` is True only when a cancel was recorded or carried out."""

from __future__ import annotations

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Role


@pytest.fixture
def led(tmp_path, clock):
    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    yield ledger
    ledger.close()


def _job(led: Ledger, state: str, *, commit: bool) -> str:
    job_id = ulid()
    led.create_job(job_id, Role.CONDUCTOR, Mode.PULL, None, {})
    if commit:
        assert led.intent_commit(job_id, 0)
    if state != "created":
        led.transition(job_id, Role.CONDUCTOR, "*", state, {}, epoch=0)
    return job_id


def test_g5_cancel_of_a_done_job_names_it_done_not_committing(led):
    """★ L3-F22: the lab's case — a pull that ended ``done`` (with ``intent: commit`` journaled, as every finished job
    has). The answer says the job is done and that nothing was cancelled; it does not claim the target is committing.

    Red on ``08cfe0f0``: ``AssertionError`` — ``runner.cancel`` returned "Too late to cancel: the target is committing.
    Use /handover reclaim on the source to take the session back.".

    Mutant: ``cancel_answer`` asks ``ledger.set_cancel`` first and answers its refusal before it checks for a
    finished job (the old order)."""
    runner = jobs.JobRunner(led, None, keepalive=False)
    job_id = _job(led, "done", commit=True)
    text = runner.cancel(job_id)
    assert "already done" in text and "committing" not in text, text
    assert "Too late to cancel" in text and "/handover reclaim" in text, text
    answer = runner.cancel_answer(job_id)
    assert (answer.requested, answer.state, answer.reason) == (False, "done", "finished"), answer
    assert led.cancel_requested(job_id) is False


@pytest.mark.parametrize(
    ("state", "commit", "requested", "reason", "says"),
    [
        ("aborted", False, False, "finished", "is already aborted"),
        ("committing", True, False, "committing", "past its commit point (committing)"),
        ("in_doubt", True, False, "committing", "past its commit point (in_doubt)"),
        ("retiring", True, False, "committing", "past its commit point (retiring)"),
        ("packing", False, True, None, "Cancel requested"),
    ],
)
def test_g5_cancel_answer_says_whether_a_cancel_was_requested(led, state, commit, requested, reason, says):
    """★ L3-F22: ``requested`` is True only when the ledger holds the request; every other answer names the state
    (``finished`` for done/aborted, ``committing`` once the commit intent is journaled) — the surfaces turn
    ``requested is False`` into a refusal with exit ≠ 0.

    Mutant: ``requested=True`` on the "already …" branch (``aborted`` red)."""
    runner = jobs.JobRunner(led, None, keepalive=False)
    job_id = _job(led, state, commit=commit)
    answer = runner.cancel_answer(job_id)
    assert (answer.requested, answer.state, answer.reason) == (requested, state, reason), answer
    assert says in answer.text and jobs.short(job_id) in answer.text, answer.text
    assert led.cancel_requested(job_id) is requested
    if state not in ("committing", "in_doubt"):
        assert "is committing" not in answer.text, answer.text
