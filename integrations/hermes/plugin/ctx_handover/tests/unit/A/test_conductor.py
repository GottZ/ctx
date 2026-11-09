"""Conductor state machine against fault-injection fakes (cluster A; TESTPLAN §2.2, DESIGN §3).

Every test ends with ``world.assert_outcome``: a successful handover is complete at the target, a failed one
leaves the source usable and unlocked, and at no point were two copies active unnoticed.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine.conductor import move_object
from ctx_handover.engine.errors import (
    Cancelled,
    ImportFailed,
    InsufficientStorage,
    InvalidState,
    LeaseLost,
    PayloadTooLarge,
)
from ctx_handover.engine.ledger import Fenced
from ctx_handover.engine.model import Include, Mode, Options, Role
from ctx_handover.tests.fakes.apis_fake import FakeBlocks, RecordingReporter, World, make_item
from ctx_handover.tests.unit.A.harness import Harness

HAPPY = [
    "created",
    "hello",
    "facts",
    "planning",
    "negotiated",
    "locking",
    "locked",
    "exporting",
    "context_staged",
    "packing",
    "data_staged",
    "target_preparing",
    "target_prepared",
    "source_preparing",
    "source_prepared",
    "committing",
    "committed",
    "retiring",
    "retired",
    "done",
]


def _run(h: Harness, **kw):
    c = h.conductor(**kw)
    c.run_until_negotiated()
    return c.run()


def _happy(tmp_path, clock, mode):
    h = Harness(tmp_path, clock, mode=mode)
    ack = _run(h)
    assert ack is not None and ack.session_id == "new-tip"
    assert h.states() == HAPPY
    h.world.assert_outcome("done")
    intents = [e["data"]["step"] for e in h.events("intent")]
    dones = [e["data"]["step"] for e in h.events("done")]
    for step in (
        "source.lock",
        "source.export",
        "source.pack",
        "target.prepare",
        "source.prepare",
        "commit",
        "target.commit",
        "source.retire",
    ):
        assert step in intents, step
    for step in (
        "source.lock",
        "source.export",
        "source.pack",
        "target.prepare",
        "source.prepare",
        "target.commit",
        "source.retire",
    ):
        assert step in dones, step
    seq = [(e["kind"], e["data"].get("step")) for e in h.events() if e["kind"] in ("intent", "done")]
    assert seq.index(("intent", "target.commit")) < seq.index(("done", "target.commit"))
    assert seq.index(("intent", "commit")) < seq.index(("intent", "target.commit"))
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["result"]["outcome"] == "done"


def test_conductor_happy_path_push(tmp_path, clock):
    """★ Every state of DESIGN §3.2 in order; every participant side effect between ``intent`` and ``done``.

    Mutant: ``Conductor._side_effect`` does not journal the ``intent`` event."""
    _happy(tmp_path, clock, Mode.PUSH)


def test_conductor_happy_path_pull(tmp_path, clock):
    _happy(tmp_path, clock, Mode.PULL)


def test_conductor_lock_after_confirmation(tmp_path, clock):
    """★ Nothing is locked before NEGOTIATED and the user's confirmation [BX M9].

    Mutant: ``source.lock`` is called in PLANNING (before negotiation)."""
    h = Harness(tmp_path, clock)
    h.world.source.items.append(make_item("x2", "/srv/maybe", include=Include.PROPOSED))
    seen = {}

    def confirm(info):
        seen["calls_before"] = list(h.calls("source"))
        seen["reasons"] = list(info["reasons"])
        return "cancel"

    c = h.conductor(confirm=confirm)
    with pytest.raises(Cancelled):
        c.run_until_negotiated()
    assert seen["reasons"] == ["proposed"]
    assert "lock" not in seen["calls_before"]
    assert "lock" not in h.calls("source")
    assert h.state() == "aborted"
    h.world.assert_outcome("aborted")

    h2 = Harness(tmp_path, clock)
    h2.world.source.items.append(make_item("x2", "/srv/maybe", include=Include.PROPOSED))
    order = []
    c2 = h2.conductor(confirm=lambda info: order.append(("confirm", list(h2.calls("source")))) or "yes")
    c2.run_until_negotiated()
    assert "lock" not in order[0][1]
    assert h2.state() == "negotiated" and "lock" not in h2.calls("source")
    c2.run()
    assert h2.calls("source").index("lock") > h2.calls("source").index("start_plan")
    h2.world.assert_outcome("done")


def test_conductor_confirmation_none_policy(tmp_path, clock):
    """``None`` (timeout) ⇒ only reason (a) continues without the proposed items; others cancel [BX L5]."""
    h = Harness(tmp_path, clock)
    h.world.source.items.append(make_item("x2", "/srv/maybe", include=Include.PROPOSED))
    c = h.conductor(confirm=lambda info: None)
    final = c.run_until_negotiated()
    assert all(i.include is not Include.PROPOSED for i in final.plan.items)
    c.run()
    h.world.assert_outcome("done")
    assert "x2" not in h.world.target.materialised

    from ctx_handover.engine.model import Conflict

    h2 = Harness(tmp_path, clock)
    real = h2.world.target.negotiate

    def conflicting(job, plan):
        from dataclasses import replace

        final = real(job, plan)
        d0 = replace(final.decisions[0], conflict=Conflict("dirty", "local edits", "/laptop/srv/app-handover-1"))
        return replace(final, decisions=(d0, *final.decisions[1:]))

    h2.world.target.negotiate = conflicting
    c2 = h2.conductor(confirm=lambda info: None)
    with pytest.raises(Cancelled):
        c2.run_until_negotiated()
    assert "lock" not in h2.calls("source")
    assert h2.ledger.job(h2.job_id, Role.CONDUCTOR)["result"]["plan"]["plan_id"]


def test_conductor_show_plan_ends_without_lock(tmp_path, clock):
    h = Harness(tmp_path, clock)
    h.world.source.items.append(make_item("x2", "/srv/maybe", include=Include.PROPOSED))
    c = h.conductor(confirm=lambda info: "show_plan")
    with pytest.raises(Cancelled) as exc:
        c.run_until_negotiated()
    assert exc.value.details["ended_by"] == "show_plan"
    assert h.state() == "aborted" and "lock" not in h.calls("source")


def test_conductor_replan_when_revision_moved_at_lock(tmp_path, clock):
    """Revision change between plan and lock ⇒ deterministic re-plan (refine off) and re-negotiation; a new
    proposal in the non-interactive part is excluded, not silently included (DESIGN §3.2 LOCKED)."""
    h = Harness(tmp_path, clock)
    c = h.conductor()
    c.run_until_negotiated()

    def move(world):
        world.source.append_rows("typed while confirming")
        world.source.items.append(make_item("x3", "/srv/new", include=Include.PROPOSED))

    h.world.inject("source", "lock", "action", action=move)
    c.run()
    plans = [m for s, m in h.world.started if m == "plan"]
    assert len(plans) == 2
    replan = [e for e in h.events("info") if e["data"].get("event") == "replanned"]
    assert replan and replan[0]["data"]["excluded_proposed"] == ["x3"]
    assert "x3" not in h.world.target.materialised
    h.world.assert_outcome("done")


def test_conductor_export_and_pack_parallel_join(tmp_path, clock):
    h = Harness(tmp_path, clock)
    h.world.op_polls.update({"export": 3, "pack": 3})
    _run(h)
    started = [m for s, m in h.world.started]
    log = h.world.log
    i_export = started.index("export")
    i_pack = started.index("pack")
    assert abs(i_export - i_pack) == 1  # both started back to back
    first_poll_done = next(i for i, (s, m) in enumerate(log) if m == "start_stage_context")
    assert ("source", "start_pack") in log[:first_poll_done]
    st = h.states()
    assert st.index("target_preparing") > st.index("data_staged") > st.index("context_staged")
    h.world.assert_outcome("done")


def test_conductor_stale_at_source_prepare_appends_delta(tmp_path, clock):
    """★ Append-only change at SOURCE_PREPARING ⇒ DELTA_APPLYING ⇒ SOURCE_PREPARING ⇒ prepared; no restart [SK H2].

    Mutant: a ``delta`` outcome is treated like ``stale_session`` (TARGET_RESETTING)."""
    h = Harness(tmp_path, clock)
    h.world.inject("source", "start_prepare", "action", action=lambda w: w.source.append_rows("late message"))
    _run(h)
    st = h.states()
    assert "delta_applying" in st and "target_resetting" not in st
    assert st[st.index("delta_applying") + 1] == "source_preparing"
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["restarts"] == 0
    assert h.world.target.hidden_rows[-1] == "late message"
    h.world.assert_outcome("done")


def test_conductor_stale_session_restarts_bounded(tmp_path, clock):
    """★ Non-append change ⇒ TARGET_RESETTING ⇒ EXPORTING; the 4th restart ends in ``restart_limit`` and the
    source is released [SK H2].

    Mutant: the restart counter is not checked (unbounded restarts)."""
    h = Harness(tmp_path, clock)
    h.world.inject("source", "start_prepare", "action", action=lambda w: w.source.rewrite())
    _run(h)
    st = h.states()
    i = st.index("target_resetting")
    assert st[i + 1] == "exporting"
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["restarts"] == 1
    h.world.assert_outcome("done")

    h2 = Harness(tmp_path, clock)
    h2.world.inject("source", "start_prepare", "action", action=lambda w: w.source.rewrite(), times=50)
    _run(h2)
    assert h2.state() == "aborted"
    assert h2.states().count("target_resetting") == 3
    assert h2.ledger.job(h2.job_id, Role.CONDUCTOR)["last_error"]["code"] == "restart_limit"
    h2.world.assert_outcome("aborted")


def test_conductor_stale_worktree_repacks(tmp_path, clock):
    """``stale_worktree`` ⇒ PACKING again, then a new target prepare [SK M10]."""
    h = Harness(tmp_path, clock)
    h.world.inject("source", "start_prepare", "action", action=lambda w: w.source.touch_worktree())
    _run(h)
    st = h.states()
    i = st.index("source_preparing")
    assert st[i + 1] == "packing"
    assert [m for s, m in h.world.started].count("pack") == 2
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["restarts"] == 1
    h.world.assert_outcome("done")


def test_conductor_prepare_order_target_then_source(tmp_path, clock):
    """★ ``source.prepare`` only after the target voted prepared; commit only after the source is prepared.

    Mutant: SOURCE_PREPARING is entered from DATA_STAGED (before TARGET_PREPARING)."""
    h = Harness(tmp_path, clock)
    _run(h)
    log = h.world.log
    t_prep = log.index(("target", "start_prepare"))
    s_prep = log.index(("source", "start_prepare"))
    t_commit = log.index(("target", "start_commit"))
    assert t_prep < s_prep < t_commit
    prepared_vote = max(i for i, x in enumerate(log[:s_prep]) if x == ("target", "op_status"))
    assert prepared_vote > t_prep
    h.world.assert_outcome("done")


def test_conductor_in_doubt_committing_waits(tmp_path, clock):
    """★ Commit unreachable while the target reports ``committing`` ⇒ IN_DOUBT keeps polling and never releases
    the source; the target finishes ⇒ COMMITTED ⇒ DONE [SK H5].

    Mutant: IN_DOUBT treats a non-terminal ``committing`` like ``aborted``."""
    h = Harness(tmp_path, clock)
    h.world.op_polls["commit"] = 12

    def cut(world):  # the link to the commit operation is lost right after it started
        world.inject("target", "op_status", "disconnect", times=10_000)

    h.world.inject("target", "start_commit", "action_after", action=cut)
    _run(h)
    st = h.states()
    assert "in_doubt" in st and "aborting" not in st
    assert "start_release" not in h.calls("source")
    assert st[-3:] == ["retiring", "retired", "done"]
    h.world.assert_outcome("done")


def test_conductor_in_doubt_resolves_committed(tmp_path, clock):
    """The commit finished but every answer about it was lost: IN_DOUBT reads ``committed`` and the ACK from the
    target's status (DESIGN §3.4 point 5)."""
    h = Harness(tmp_path, clock)
    h.world.op_polls["commit"] = 2

    def cut(world):
        world.inject("target", "op_status", "disconnect_after", times=10_000)
        world.inject("target", "start_commit", "disconnect", times=10_000)

    h.world.inject("target", "start_commit", "action_after", action=cut)
    _run(h)
    st = h.states()
    assert st[st.index("in_doubt") + 1] == "committed"
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["result"]["ack"]["session_id"] == "new-tip"
    h.world.assert_outcome("done")


def test_conductor_in_doubt_resolves_aborted(tmp_path, clock):
    h = Harness(tmp_path, clock)
    h.world.target.commit_fail = ImportFailed("disk full", hermes_error="OSError")
    h.world.inject(
        "target",
        "start_commit",
        "action_after",
        action=lambda w: w.inject("target", "op_status", "disconnect", times=10_000),
    )
    _run(h)
    assert "in_doubt" in h.states()
    assert h.state() == "aborted"
    h.world.assert_outcome("aborted")


def test_conductor_target_commit_fail_aborts_source_untouched(tmp_path, clock):
    """★ A failing commit op ⇒ the source is released (primary lock undone), never retired.

    Mutant: ABORTING does not release the source (it would stay locked after a failed commit)."""
    h = Harness(tmp_path, clock)
    h.world.target.commit_fail = ImportFailed("import failed", hermes_error="IntegrityError")
    _run(h)
    assert h.state() == "aborted"
    assert "start_retire" not in h.calls("source") and "start_release" in h.calls("source")
    assert h.world.source.events[-1] == "release"
    h.world.assert_outcome("aborted")


def test_conductor_retire_retried_until_success(tmp_path, clock):
    h = Harness(tmp_path, clock)
    h.world.inject("source", "start_retire", "disconnect", times=3)
    _run(h)
    assert h.state() == "done"
    assert h.calls("source").count("start_retire") == 4
    h.world.assert_outcome("done")


@pytest.mark.parametrize("at", ["start_export", "start_pack", "start_prepare"])
def test_conductor_cancel_before_commit(tmp_path, clock, at):
    """★ Cancel before ``intent: commit`` ⇒ ABORTING ⇒ ABORTED, source released (DESIGN §3.8).

    Mutant: the conductor never checks ``cancel_requested``."""
    h = Harness(tmp_path, clock)
    side = "target" if at == "start_prepare" else "source"
    h.world.inject(side, at, "action", action=lambda w: h.ledger.set_cancel(h.job_id))
    _run(h)
    assert h.state() == "aborted"
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]["code"] == "cancelled"
    assert "start_commit" not in h.calls("target")
    assert "start_prepare" not in h.calls("source")  # stopped before the primary lock, not only at the commit
    h.world.assert_outcome("aborted")


def test_conductor_cancel_after_commit_intent_refused(tmp_path, clock):
    h = Harness(tmp_path, clock)
    refused = []

    def try_cancel(world):
        try:
            h.ledger.set_cancel(h.job_id)
        except InvalidState as exc:
            refused.append(exc.details["reason"])

    h.world.inject("target", "start_commit", "action", action=try_cancel)
    _run(h)
    assert refused == ["committing"]
    h.world.assert_outcome("done")


def test_pull_success_message_after_remote_retire(tmp_path, clock):
    """Pull reports ``done`` only after the remote retire; before that the outcome is "active here, the source
    is released on the next contact" [SK L8]."""
    h = Harness(tmp_path, clock, mode=Mode.PULL)
    h.world.inject("source", "start_retire", "disconnect", times=100)
    ack = _run(h)
    assert ack is not None
    row = h.ledger.job(h.job_id, Role.CONDUCTOR)
    assert row["state"] == "retiring" and row["result"]["outcome"] == "active_source_pending"
    assert h.world.source.state == "prepared"  # fail-closed until the next contact
    assert h.world.violations == []
    h.world.faults.clear()
    h.stale()
    c = h.conductor()
    c.recover()
    assert h.state() == "done"
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["result"]["outcome"] == "done"
    h.world.assert_outcome("done")


def test_conductor_stops_when_fenced(tmp_path, clock):
    """A second owner adopts the job: the first conductor stops without any further participant call."""
    h = Harness(tmp_path, clock)
    c1 = h.conductor()
    c1.run_until_negotiated()

    def adopt(world):
        h.stale()
        h.conductor(owner="intruder")

    h.world.inject("source", "lock", "action", action=adopt)
    with pytest.raises(Fenced):
        c1.run()
    calls_after = h.world.log[h.world.log.index(("source", "lock")) + 1 :]
    assert calls_after == []


def test_conductor_lease_lost_before_prepare_aborts(tmp_path, clock):
    h = Harness(tmp_path, clock)
    h.world.op_polls["pack"] = 80  # long enough for a lease refresh

    def lose(world):
        world.source.lease_lost = True

    h.world.inject("source", "start_pack", "action_after", action=lose)
    _run(h)
    assert h.state() == "aborted"
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]["code"] == LeaseLost.code
    assert h.world.heartbeats >= 1
    h.world.assert_outcome("aborted")


def test_conductor_busy_without_and_with_wait(tmp_path, clock):
    h = Harness(tmp_path, clock)
    h.world.source.busy = True
    _run(h)
    assert h.state() == "aborted"
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]["code"] == "session_busy"
    h.world.assert_outcome("aborted")

    h2 = Harness(tmp_path, clock, options=Options(wait=True, wait_timeout_s=600))
    h2.world.source.busy = True
    h2.world.inject("source", "lock", "action_after", skip=3, action=lambda w: setattr(w.source, "busy", False))
    calls = {"n": 0}
    real = h2.world.source.lock

    def lock(job, ref, rev):
        calls["n"] += 1
        if calls["n"] == 4:
            h2.world.source.busy = False
        return real(job, ref, rev)

    h2.world.source.lock = lock
    _run(h2)
    assert h2.state() == "done" and calls["n"] == 4

    h3 = Harness(tmp_path, clock, options=Options(wait=True, wait_timeout_s=10))
    h3.world.source.busy = True
    _run(h3)
    assert h3.ledger.job(h3.job_id, Role.CONDUCTOR)["last_error"]["code"] == "wait_timeout"
    h3.world.assert_outcome("aborted")


def test_conductor_insufficient_storage_before_lock(tmp_path, clock):
    """Disk check at NEGOTIATED: nothing locked, nothing transferred [SK M3]."""
    h = Harness(tmp_path, clock)
    h.world.target.free = 10
    c = h.conductor()
    with pytest.raises(InsufficientStorage):
        c.run_until_negotiated()
    assert h.state() == "aborted"
    assert "lock" not in h.calls("source")
    assert h.world.target.blocks_store.data == {}
    h.world.assert_outcome("aborted")


def test_conductor_capability_missing(tmp_path, clock):
    from ctx_handover.engine.errors import CapabilityMissing

    h = Harness(tmp_path, clock)
    h.world.capabilities["target"] = frozenset({"session.v1"})
    with pytest.raises(CapabilityMissing) as exc:
        h.conductor().run_until_negotiated()
    assert exc.value.details["capability"] == "blocks.v1"
    assert h.state() == "aborted"


def test_conductor_continue_after_retire(tmp_path, clock):
    h = Harness(tmp_path, clock, continue_prompt="Go on.")
    _run(h)
    assert h.world.target.continued == ["Go on."]
    st = h.states()
    assert st[-3:] == ["retired", "continuing", "done"]
    last_retire = max(i for i, x in enumerate(h.world.log) if x == ("source", "start_retire"))
    assert h.world.log.index(("target", "continue_session")) > last_retire


# ---------------------------------------------------------------------------------------------------------
# block mover
# ---------------------------------------------------------------------------------------------------------


def _stores(clock, n_blocks=40):
    import hashlib

    w = World(clock, block_size=64)
    src = FakeBlocks(w, "src", 64)
    dst = FakeBlocks(w, "dst", 64)
    data = b"".join(hashlib.sha256(str(i).encode()).digest() * 2 for i in range(n_blocks))
    m = src.put_object(data, "application/octet-stream")
    assert len(set(m.blocks)) == n_blocks
    return w, src, dst, m


def test_block_mover_need_put_loop(clock):
    _w, src, dst, m = _stores(clock)
    dst.data[m.blocks[0]] = src.data[m.blocks[0]]  # already there: never resent (I9)
    rep = RecordingReporter()
    move_object(src, dst, [m.object_id], reporter=rep, cancelled=lambda: False, hosted=False, request_bytes=64 * 8)
    assert dst.has_object(m.object_id)
    assert sum(dst.write_calls) == m.size - 64  # every missing block exactly once, the present one never
    assert all(b <= 64 * 8 for b in dst.write_calls)
    assert rep.progress_events and rep.progress_events[-1].bytes_done == rep.progress_events[-1].bytes_total


def test_block_mover_list_get_loop_and_413_halves(clock):
    _w, src, dst, m = _stores(clock)
    dst.max_body = 64 * 2
    move_object(
        src,
        dst,
        [m.object_id],
        reporter=RecordingReporter(),
        cancelled=lambda: False,
        hosted=False,
        request_bytes=64 * 16,
    )
    assert dst.has_object(m.object_id)
    assert max(dst.write_calls) <= 64 * 2
    assert max(src.read_calls) <= 4096


def test_block_mover_one_block_over_limit_fails(clock):
    _w, src, dst, m = _stores(clock)
    dst.max_body = 10
    with pytest.raises(PayloadTooLarge):
        move_object(
            src,
            dst,
            [m.object_id],
            reporter=RecordingReporter(),
            cancelled=lambda: False,
            hosted=False,
            request_bytes=64 * 4,
        )


def test_block_mover_hosted_one_in_flight(clock):
    _w, src, dst, m = _stores(clock, n_blocks=64)
    dst.write_delay_s = 0.01
    move_object(
        src,
        dst,
        [m.object_id],
        reporter=RecordingReporter(),
        cancelled=lambda: False,
        hosted=True,
        request_bytes=64 * 2,
    )
    assert dst.max_inflight == 1
    _w2, src2, dst2, m2 = _stores(clock, n_blocks=64)
    dst2.write_delay_s = 0.01
    move_object(
        src2,
        dst2,
        [m2.object_id],
        reporter=RecordingReporter(),
        cancelled=lambda: False,
        hosted=False,
        request_bytes=64 * 2,
    )
    assert dst2.max_inflight == 2
    assert dst.has_object(m.object_id) and dst2.has_object(m2.object_id)


def test_block_mover_cancel(clock):
    _w, src, dst, m = _stores(clock)
    with pytest.raises(Cancelled):
        move_object(
            src,
            dst,
            [m.object_id],
            reporter=RecordingReporter(),
            cancelled=lambda: True,
            hosted=False,
            request_bytes=64,
        )


def test_conductor_keepalive_thread_heartbeats_during_blocking_calls(tmp_path, clock):
    """While a participant call blocks (real time), the keepalive thread keeps the owner and the source lease
    alive (DESIGN §3.1 heartbeat every 10 s, §7.2 refresh), so another process does not adopt a live job."""
    import time as _time
    from dataclasses import replace as _replace

    from ctx_handover.engine.contracts import JobContext
    from ctx_handover.tests.unit.A.harness import FAST

    h = Harness(tmp_path, clock)
    c = h.conductor()
    c.run_until_negotiated()
    beats = []
    real = h.ledger.heartbeat

    def heartbeat(*a, **kw):
        import threading as _threading

        beats.append(_threading.current_thread().name)
        return real(*a, **kw)

    h.ledger.heartbeat = heartbeat

    def slow(world):
        for _ in range(30):
            clock.advance(5)
            _time.sleep(0.01)

    h.world.inject("source", "start_pack", "action", action=slow)
    owner = c.owner
    job = JobContext(
        job_id=h.job_id,
        mode=Mode.PUSH,
        peer=None,
        options=Options(),
        epoch=c.job.epoch,
        reporter=h.reporter,
        cancelled=lambda: False,
        created_at=clock(),
    )
    from ctx_handover.engine.conductor import Conductor

    c2 = Conductor(
        h.ledger,
        job,
        h.world.source,
        h.world.target,
        confirm=None,
        continue_prompt=None,
        planning=h.planning,
        owner=owner,
        clock=clock,
        sleep=h.world.sleep,
        poll=_replace(FAST, keepalive_s=0.001),
        keepalive=True,
    )
    c2.run()
    assert h.state() == "done"
    assert "ctxh-keepalive" in beats, "no owner heartbeat from the keepalive thread while the call blocked"
    assert h.world.heartbeats >= 1
    h.world.assert_outcome("done")
