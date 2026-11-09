"""File-system fencing of operations across two real processes (layer 2 open point, contract "Offene Punkte"):
a hung but alive process must never lead to two runs of the same file-system steps, also when it hangs between
recording the operation and taking the job lock. ``ops`` module docstring, rules 1-4."""

from __future__ import annotations

import multiprocessing as mp
import threading
import time

from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import OpState
from ctx_handover.engine.ops import OpRunner
from ctx_handover.tests.unit.A import procs_helpers

CTX = mp.get_context("spawn")


def _late_worker(tmp_path, job_id):
    path, locks, marker = tmp_path / "ledger.sqlite3", tmp_path / "locks", tmp_path / "marker.txt"
    Ledger(path).close()
    marker.write_text("")
    entered, release, out = CTX.Event(), CTX.Event(), CTX.Queue()
    proc = CTX.Process(
        target=procs_helpers.late_worker_owner,
        args=(str(path), str(locks), job_id, str(marker), entered, release, out),
    )
    proc.start()
    kind, op_id = out.get(timeout=60)
    assert kind == "op"
    assert entered.wait(60), "the child's worker never started"
    return path, locks, marker, release, out, proc, op_id


def _finish(proc, out, release):
    release.set()
    kind, state = out.get(timeout=120)
    assert kind == "done"
    proc.join(60)
    assert proc.exitcode == 0
    return state


def test_late_worker_of_hung_process_never_reruns_finished_steps(tmp_path):
    """★ H recorded the op and hung before its worker took the job lock; M saw the stale heartbeat, marked it
    interrupted (nobody held the lock), restarted and finished it. When H wakes up, its worker takes the free lock
    — and must find the work done instead of running the steps a second time.

    Mutant: ``OpRunner._body`` without the ``Ledger.op_claim`` check (rule 4) → H's late worker runs the function
    again: ``H-start`` appears in the marker after ``M-start``."""
    path, locks, marker, release, out, proc, op_id = _late_worker(tmp_path, "J1")
    time.sleep(0.6)  # far beyond stale_s = 0.2: H's heartbeat is missing
    lg = Ledger(path)
    other = OpRunner(lg, None, stale_s=0.2, lock_dir=locks)
    st = other.status("J1", op_id)
    assert st.state is OpState.FAILED and st.error["details"]["reason"] == "interrupted"

    def by_m(report):
        with open(marker, "a", encoding="utf-8") as fh:
            fh.write("M-start\n")
        return {"by": "M"}

    again = other.start("J1", "commit", "d", by_m, exclusive=True)
    assert again.op_id == op_id
    assert other.wait("J1", op_id).result == {"by": "M"}
    state = _finish(proc, out, release)
    assert marker.read_text().splitlines() == ["M-start"], "a late worker ran finished file-system steps again"
    assert state == "done"
    final = other.status("J1", op_id)
    assert final.state is OpState.DONE and final.result == {"by": "M"}
    lg.close()


def test_worker_that_cannot_take_the_lock_leaves_the_running_row_alone(tmp_path):
    """★ Same start, but M's restarted run is still working (it holds the job lock) when H wakes up. H's worker
    cannot take the lock and must leave M's ``running`` row untouched — no ``failed``/``busy_elsewhere`` that would
    make the conductor restart the op once more (and run it twice after M released the lock).

    Mutant: ``OpRunner._body`` records ``failed`` (``busy_elsewhere``, retryable) when the lock is taken (the
    pre-layer-2 behaviour) → M's status reads ``failed`` while M runs."""
    path, locks, marker, release, out, proc, op_id = _late_worker(tmp_path, "J2")
    time.sleep(0.6)
    lg = Ledger(path)
    other = OpRunner(lg, None, stale_s=0.2, lock_dir=locks)
    assert other.status("J2", op_id).state is OpState.FAILED
    running, finish = threading.Event(), threading.Event()

    def by_m(report):
        with open(marker, "a", encoding="utf-8") as fh:
            fh.write("M-start\n")
        running.set()
        finish.wait(60)
        return {"by": "M"}

    other.start("J2", "commit", "d", by_m, exclusive=True)
    assert running.wait(30)
    state = _finish(proc, out, release)  # H's worker tried the lock (with retries) and gave up
    assert state == "running"
    assert other.status("J2", op_id).state is OpState.RUNNING, "a worker without the lock rewrote the row"
    assert other.start("J2", "commit", "d", by_m, exclusive=True).state is OpState.RUNNING  # no second run
    finish.set()
    assert other.wait("J2", op_id).result == {"by": "M"}
    assert marker.read_text().splitlines() == ["M-start"]
    lg.close()


def test_restart_never_overwrites_a_result_written_meanwhile(tmp_path):
    """★ Two runners (two ledger connections, as two processes) restart the same failed op: B read ``failed``,
    then A restarted it and A's worker finished, before B wrote. B's restart is a compare-and-set (rule 2): it
    fails, B returns A's result and runs nothing.

    Mutant: ``OpRunner.start`` restarts with an unconditional ``op_put(..., RUNNING)`` instead of
    ``Ledger.op_cas`` → B overwrites A's ``done`` row and its worker runs the steps a second time (``calls`` gets
    ``"b"``)."""
    path = tmp_path / "ledger.sqlite3"
    a_lg, b_lg = Ledger(path), Ledger(path)
    a = OpRunner(a_lg, None, lock_dir=tmp_path / "locks")
    b = OpRunner(b_lg, None, lock_dir=tmp_path / "locks")

    def boom(report):
        raise RuntimeError("transient")

    op = a.start("J3", "retire", "d", boom, exclusive=True)
    assert a.wait("J3", op.op_id).state is OpState.FAILED
    calls: list[str] = []

    def run(tag):
        def fn(report):
            calls.append(tag)
            return {"by": tag}

        return fn

    real_cas = b_lg.op_cas

    def interleaved(*args, **kwargs):  # A restarts and finishes between B's read and B's write
        b_lg.op_cas = real_cas
        a.start("J3", "retire", "d", run("a"), exclusive=True, restart_failed=True)
        assert a.wait("J3", op.op_id).state is OpState.DONE
        return real_cas(*args, **kwargs)

    b_lg.op_cas = interleaved
    got = b.start("J3", "retire", "d", run("b"), exclusive=True, restart_failed=True)
    b.join_all(10)
    assert got.op_id == op.op_id and got.state is OpState.DONE and got.result == {"by": "a"}
    assert calls == ["a"], "a restart overwrote a finished op and ran it again"
    a_lg.close()
    b_lg.close()


def test_op_blocked_by_another_holder_is_restartable_once_the_lock_is_free(tmp_path, clock):
    """Rule 3, liveness: an exclusive op whose worker found the job lock held (another op of the job, here an
    outside holder) stays ``running`` while the lock is held, and is reported interrupted (restartable) as soon as
    the lock is free — not only after ``stale_s`` without heartbeat.

    Mutant: ``OpRunner._body`` returns without ``Ledger.op_expire`` → the op reads ``running`` until ``stale_s``."""
    from ctx_handover.engine.ops import JobLock

    lg = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    runner = OpRunner(lg, None, stale_s=60, lock_dir=tmp_path / "locks")
    outside = JobLock(tmp_path / "locks" / "J4.lock")
    assert outside.acquire()
    calls: list[int] = []
    st = runner.start("J4", "commit", "d", lambda report: calls.append(1) or {"ok": 1}, exclusive=True)
    runner.join_all(10)
    assert calls == [] and runner.status("J4", st.op_id).state is OpState.RUNNING  # the holder still works
    outside.release()
    again = runner.status("J4", st.op_id)
    assert again.state is OpState.FAILED and again.error["details"]["reason"] == "interrupted"
    assert runner.wait(
        "J4", runner.start("J4", "commit", "d", lambda report: {"ok": 2}, exclusive=True).op_id
    ).result == {"ok": 2}
    assert calls == []
    lg.close()
