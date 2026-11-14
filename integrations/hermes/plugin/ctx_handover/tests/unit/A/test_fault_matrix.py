"""Every participant call × every fault, on both sides, in push and pull (cluster A; brief: "jeder Zustand gegen
jeden Fehler").

Faults: lost connection before the call took effect and after it (lost response), a non-retryable error, the
conductor's process dying before/after a call (adopted by a new owner after the stale window), the participant's
process restarting (running operations lost), a link that stays down until the driver "brings it back", and the
user cancelling at that point.

For every scenario the driver keeps going the way the product does — resume a suspended job, let the janitor
recover an orphaned one, retry a parked abort/retire once the link is back — until the job is DONE or ABORTED.
Then ``World.assert_outcome`` checks the two promises: success ⇒ the target holds exactly the rows the source
had at its prepare point and every confirmed item; failure ⇒ the source is usable, unlocked and has no veto;
and the invariant monitor saw no moment with two active copies.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine.errors import HandoverError, ImportFailed
from ctx_handover.engine.model import Mode, Role
from ctx_handover.tests.fakes.apis_fake import SimulatedCrash
from ctx_handover.tests.unit.A.harness import Harness

SOURCE_CALLS = [
    "hello",
    "facts",
    "start_plan",
    "lock",
    "start_export",
    "start_pack",
    "start_prepare",
    "start_retire",
    "start_release",
    "op_status",
    "complete:plan",
    "complete:export",
    "complete:pack",
    "complete:prepare",
    "complete:retire",
]
TARGET_CALLS = [
    "hello",
    "open_job",
    "facts",
    "negotiate",
    "start_stage_context",
    "start_prepare",
    "start_commit",
    "op_status",
    "status",
    "complete:stage_context",
    "complete:prepare",
    "complete:commit",
]
BLOCK_CALLS = [
    ("source_blocks", "manifests"),
    ("source_blocks", "read_blocks"),
    ("target_blocks", "missing"),
    ("target_blocks", "write_blocks"),
    ("target_blocks", "put_manifests"),
]
KINDS = ["disconnect", "disconnect_after", "error", "crash", "crash_after", "interrupt", "down", "cancel"]

CASES = [("source", m) for m in SOURCE_CALLS] + [("target", m) for m in TARGET_CALLS] + BLOCK_CALLS


def _inject(h: Harness, side: str, method: str, kind: str, skip: int) -> None:
    w = h.world
    if kind == "error":
        w.inject(side, method, "error", error=ImportFailed("injected failure", hermes_error="Injected"), skip=skip)
    elif kind == "down":
        w.inject(side, method, "disconnect", times=10_000, skip=skip)
    elif kind == "interrupt":
        w.inject(side if side in ("source", "target") else side.split("_")[0], "op_status", "interrupt", skip=skip)
        if not method.startswith("complete:"):
            w.inject(side, method, "disconnect_after", skip=skip)
    elif kind == "cancel":

        def cancel(world):
            try:
                h.ledger.set_cancel(h.job_id)
            except HandoverError:
                pass

        w.inject(side, method, "action", action=cancel, skip=skip)
    else:
        w.inject(side, method, kind, skip=skip)


def drive(h: Harness, *, max_rounds: int = 12) -> str:
    """Run the job like the product would until DONE or ABORTED; returns the final state."""
    c = h.conductor()
    first = True
    for _ in range(max_rounds):
        try:
            if first:
                first = False
                try:
                    c.run_until_negotiated()
                except HandoverError:
                    pass
                if h.state() not in ("done", "aborted"):
                    c.run()
            else:
                c.recover()
        except SimulatedCrash:
            pass
        state = h.state()
        if state in ("done", "aborted"):
            return state
        # the link comes back, the process restarts, a later janitor run adopts the job
        h.world.faults.clear()
        h.stale()
        c = h.conductor()
        if state == "suspended":
            try:
                c.resume()
            except SimulatedCrash:
                pass
            state = h.state()
            if state in ("done", "aborted"):
                return state
            h.stale()
            c = h.conductor()
    return h.state()


@pytest.mark.parametrize("mode", [Mode.PUSH, Mode.PULL])
@pytest.mark.parametrize("kind", KINDS)
@pytest.mark.parametrize(("side", "method"), CASES, ids=[f"{s}.{m}" for s, m in CASES])
def test_fault_matrix(tmp_path, clock, mode, kind, side, method):
    h = Harness(tmp_path, clock, mode=mode)
    h.world.op_polls.update({"export": 2, "pack": 2, "prepare": 2, "commit": 3})
    _inject(h, side, method, kind, skip=0)
    final = drive(h)
    assert final in ("done", "aborted"), (final, h.states())
    h.world.assert_outcome(final)
    if kind in ("disconnect", "disconnect_after", "interrupt") and method != "complete:commit":
        # one lost packet or one participant restart is invisible to the user (a commit operation that fails
        # inside the target rolls back by design: that job ends ABORTED with the source released)
        assert final == "done", (h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"], h.states())


@pytest.mark.parametrize("kind", ["crash", "crash_after", "down", "disconnect_after"])
@pytest.mark.parametrize("nth", [1, 2, 3])
def test_fault_matrix_repeated_polls(tmp_path, clock, kind, nth):
    """Faults on later polls of the commit and retire operations (deep inside the async phases)."""
    for side in ("source", "target"):
        h = Harness(tmp_path, clock)
        h.world.op_polls.update({"commit": 5, "retire": 4, "prepare": 4})
        _inject(h, side, "op_status", kind, skip=nth + 8)
        final = drive(h)
        assert final in ("done", "aborted")
        h.world.assert_outcome(final)


def test_two_conductors_never_both_drive(tmp_path, clock):
    """Doppelte Ausführung: a second conductor adopts a stalled job; the first one is fenced on its next write and
    the participants refuse its epoch, so the job completes exactly once."""
    from ctx_handover.engine.ledger import Fenced

    h = Harness(tmp_path, clock)
    first = h.conductor()
    first.run_until_negotiated()

    def takeover(world):
        h.stale()
        second = h.conductor()
        second.run()

    h.world.inject("source", "start_pack", "action_after", action=takeover)
    with pytest.raises(Fenced):
        first.run()
    assert h.state() == "done"
    assert h.world.target.ops and sum(1 for _, m in h.world.started if m == "commit") == 1
    h.world.assert_outcome("done")
