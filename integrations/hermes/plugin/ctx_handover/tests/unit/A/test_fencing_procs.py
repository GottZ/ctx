"""File-system fencing across processes (cluster A, layer 2 open point): a process that hangs while it changes
the file system (heartbeats stop, process alive) must never be overtaken by a second run of the same steps.

The ledger's owner epoch fences journal writes only; the ``JobLock`` (``flock``) is what the kernel releases
only when the holder's process dies. Two real processes, no mocks.
"""

from __future__ import annotations

import multiprocessing as mp
import os
import signal
import time

from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import OpState
from ctx_handover.engine.ops import OpRunner
from ctx_handover.tests.unit.A import procs_helpers

CTX = mp.get_context("spawn")


def _second_run(marker):
    def fn(report):
        with open(marker, "a", encoding="utf-8") as fh:
            fh.write("M-start\n")
        return {"by": "M"}

    return fn


def test_hung_holder_is_not_overtaken_then_dead_holder_is(tmp_path):
    """★ Hung (alive) holder: its op stays ``running`` although the heartbeat is stale, and a second start does
    not run the steps again; after the holder dies, the op is ``interrupted`` and the restart runs.

    Mutant: ``OpRunner._held`` always answers False (no file-system fencing, only the stale heartbeat)."""
    path = tmp_path / "ledger.sqlite3"
    locks = tmp_path / "locks"
    marker = tmp_path / "marker.txt"
    Ledger(path).close()
    started, release = CTX.Event(), CTX.Event()
    out = CTX.Queue()
    proc = CTX.Process(
        target=procs_helpers.hung_op_owner,
        args=(str(path), str(locks), "J1", str(marker), started, release, out),
    )
    proc.start()
    op_id = out.get(timeout=60)
    assert started.wait(60)
    time.sleep(0.6)  # far beyond stale_s = 0.2: the heartbeat is "missing"
    lg = Ledger(path)
    other = OpRunner(lg, None, stale_s=0.2, lock_dir=locks)
    assert other.status("J1", op_id).state is OpState.RUNNING
    st = other.start("J1", "commit", "d", _second_run(marker), exclusive=True, restart_failed=True)
    assert st.state is OpState.RUNNING
    time.sleep(0.3)
    assert "M-start" not in marker.read_text(), "a second run overtook a live holder"
    release.set()
    proc.join(60)
    assert proc.exitcode == 0
    assert other.status("J1", op_id).state is OpState.DONE

    started2, release2 = CTX.Event(), CTX.Event()
    out2 = CTX.Queue()
    proc2 = CTX.Process(
        target=procs_helpers.hung_op_owner,
        args=(str(path), str(locks), "J2", str(marker), started2, release2, out2),
    )
    proc2.start()
    op2 = out2.get(timeout=60)
    assert started2.wait(60)
    os.kill(proc2.pid, signal.SIGKILL)
    proc2.join(60)
    time.sleep(0.4)
    assert other.status("J2", op2).state is OpState.FAILED  # interrupted: the holder is gone
    st2 = other.start("J2", "commit", "d", _second_run(marker), exclusive=True, restart_failed=True)
    assert other.wait("J2", st2.op_id).result == {"by": "M"}
    assert marker.read_text().count("M-start") == 1
    lg.close()
