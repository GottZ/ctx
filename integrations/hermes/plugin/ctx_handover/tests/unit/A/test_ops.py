"""OpRunner: asynchronous, idempotent, restart-safe participant operations (DESIGN §3.7, I7)."""

from __future__ import annotations

import contextvars
import threading

import pytest

from ctx_handover.engine.errors import NotFound, VerifyFailed
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import OpState, Progress
from ctx_handover.engine.ops import OpRunner


@pytest.fixture
def led(tmp_path, clock):
    lg = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    yield lg
    lg.close()


def test_ops_runner_idempotent_start(led):
    calls = []
    gate = threading.Event()

    def fn(report):
        calls.append(1)
        gate.wait(10)
        return {"ok": 1}

    runner = OpRunner(led, None)
    a = runner.start("01JZJ000000000000000000000", "export", "d1", fn)
    b = runner.start("01JZJ000000000000000000000", "export", "d1", fn)
    assert a.op_id == b.op_id and a.state is OpState.RUNNING
    other = runner.start("01JZJ000000000000000000000", "export", "d2", lambda r: {"ok": 2})
    assert other.op_id != a.op_id
    gate.set()
    done = runner.wait("01JZJ000000000000000000000", a.op_id)
    assert done.state is OpState.DONE and done.result == {"ok": 1}
    assert runner.start("01JZJ000000000000000000000", "export", "d1", fn).result == {"ok": 1}
    assert len(calls) == 1


def test_ops_runner_persists_result_across_restart(led):
    first = OpRunner(led, None)
    op = first.start("01JZJ000000000000000000000", "pack", "d", lambda r: {"pack_id": "p1", "size": 3})
    assert first.wait("01JZJ000000000000000000000", op.op_id).state is OpState.DONE
    second = OpRunner(led, None)  # another process: same ledger, empty memory

    def boom(report):
        raise AssertionError("must not run again")

    again = second.start("01JZJ000000000000000000000", "pack", "d", boom)
    assert again.op_id == op.op_id and again.state is OpState.DONE and again.result["pack_id"] == "p1"
    assert second.status("01JZJ000000000000000000000", op.op_id).result == {"pack_id": "p1", "size": 3}


def test_ops_runner_failure_recorded(led):
    runner = OpRunner(led, None)

    def bad(report):
        raise VerifyFailed("hash mismatch", which="h_all")

    op = runner.start("01JZJ000000000000000000000", "prepare", "d", bad)
    st = runner.wait("01JZJ000000000000000000000", op.op_id)
    assert st.state is OpState.FAILED
    assert st.error["code"] == "verify_failed" and st.error["retryable"] is False
    assert st.error["details"]["which"] == "h_all"
    # non-retryable failures are final for this digest
    assert runner.start("01JZJ000000000000000000000", "prepare", "d", lambda r: {"x": 1}).state is OpState.FAILED

    attempts = []

    def flaky(report):
        attempts.append(1)
        if len(attempts) == 1:
            raise RuntimeError("transient")
        return {"ok": True}

    op2 = runner.start("01JZJ000000000000000000000", "export", "d", flaky)
    st2 = runner.wait("01JZJ000000000000000000000", op2.op_id)
    assert st2.state is OpState.FAILED and st2.error["code"] == "internal" and st2.error["retryable"] is True
    again = runner.start(
        "01JZJ000000000000000000000", "export", "d", flaky
    )  # retryable ⇒ restarted under the same op id
    assert again.op_id == op2.op_id
    assert runner.wait("01JZJ000000000000000000000", op2.op_id).result == {"ok": True}
    assert len(attempts) == 2


class _Crash(BaseException):
    pass


def test_ops_orphaned_running_op_is_interrupted_then_restarted(led, clock):
    """A process that died mid-op leaves ``running``; after the stale window another process reports it
    ``interrupted`` (retryable) and a repeated start runs it again (DESIGN §3.6 participant rows)."""
    dying = OpRunner(led, None, stale_s=60)

    def crash(report):
        raise _Crash()

    op = dying.start("01JZJ000000000000000000000", "commit", "d", crash)
    dying.join_all(5)
    assert led.op_by_id(op.op_id)["state"] == "running"

    fresh = OpRunner(led, None, stale_s=60)
    assert fresh.status("01JZJ000000000000000000000", op.op_id).state is OpState.RUNNING  # not yet stale
    clock.advance(61)
    st = fresh.status("01JZJ000000000000000000000", op.op_id)
    assert st.state is OpState.FAILED and st.error["details"]["reason"] == "interrupted" and st.error["retryable"]
    again = fresh.start("01JZJ000000000000000000000", "commit", "d", lambda r: {"done": True})
    assert again.op_id == op.op_id
    assert fresh.wait("01JZJ000000000000000000000", op.op_id).result == {"done": True}


def test_ops_progress_recorded_and_status_scoped_to_job(led):
    gate = threading.Event()

    def fn(report):
        report(Progress(step="packing", bytes_done=5, bytes_total=10))
        gate.wait(10)
        return {}

    runner = OpRunner(led, None, progress_interval_s=0)
    op = runner.start("01JZJ000000000000000000000", "pack", "d", fn)
    for _ in range(200):
        st = runner.status("01JZJ000000000000000000000", op.op_id)
        if st.progress is not None:
            break
        threading.Event().wait(0.01)
    assert st.progress == Progress(step="packing", bytes_done=5, bytes_total=10)
    with pytest.raises(NotFound):
        runner.status("OTHER", op.op_id)
    gate.set()
    runner.wait("01JZJ000000000000000000000", op.op_id)


def test_ops_thread_inherits_caller_context(led):
    """Workers run in a copy of the caller's context (profile-scoped helpers resolve there, DESIGN §2.1 c6)."""
    var: contextvars.ContextVar[str] = contextvars.ContextVar("profile", default="none")
    token = var.set("work")
    try:
        runner = OpRunner(led, None)
        op = runner.start("01JZJ000000000000000000000", "facts", "d", lambda r: {"profile": var.get()})
    finally:
        var.reset(token)
    assert runner.wait("01JZJ000000000000000000000", op.op_id).result == {"profile": "work"}


def test_ops_inline_mode_runs_synchronously(led):
    runner = OpRunner(led, None, inline=True)
    st = runner.start("01JZJ000000000000000000000", "plan", "d", lambda r: {"v": 1})
    assert st.state is OpState.DONE and st.result == {"v": 1}
