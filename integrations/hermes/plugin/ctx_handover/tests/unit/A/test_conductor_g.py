"""Layer G for the conductor points of the layer-2 contract (cluster A core) against the fake participants:

* V37 — a job ends DONE only on an explicit confirmation of the source's retirement; a row another process holds
  (``busy_elsewhere``), an error or an answer without a confirming state parks it RETIRING (never DONE, never
  "the copies are independent"); only an explicit ``released``/``reclaimed`` is that (DESIGN §3.7).
* V37 (abort side) — ABORTED only after the target's abort and a release that reports ``released``; a target
  refusal other than ``committing``/``committed`` is never a reason to release the source behind its back.
* V38 — a participant whose row a restarted process's dead predecessor holds is waited out for the stale window;
  the job ends DONE, not ABORTED.
* V24 (A part) — a commit the target refused for good while it never entered its commit ends ABORTED at once.
* V33 — the block writer threads run in a copy of the caller's context.
* V34/V09 — the pack's exclusions reach the note parts and the job result (``left_behind``).
* V36 — ``export --keep``: no primary lock, the source released after the commit, DONE.
* V28 — the target facts of a paired peer are kept per peer.
* V29 — a pull's local ``--include`` asks for conductor items; what the source refuses is withheld, not the job.
"""

from __future__ import annotations

import contextvars
import dataclasses
import threading

from ctx_handover.engine import plan as planning
from ctx_handover.engine.conductor import move_object
from ctx_handover.engine.errors import ImportFailed, InvalidState, PolicyDenied
from ctx_handover.engine.jsonutil import to_wire
from ctx_handover.engine.model import Mode, Options, PlanHints, Role
from ctx_handover.tests.fakes.apis_fake import FakePlanning, RecordingReporter
from ctx_handover.tests.unit.A.harness import Harness


def _busy(times: int, *, what: str = "another process is working on this job") -> dict:
    return {"error": InvalidState(what, reason="busy_elsewhere", retryable=True), "times": times}


def _run(h: Harness):
    c = h.conductor()
    c.run_until_negotiated()
    return c.run()


def _row(h: Harness) -> dict:
    return h.ledger.job(h.job_id, Role.CONDUCTOR)


# ---------------------------------------------------------------------------------------------------------
# V37 — retire: DONE only on an explicit confirmation
# ---------------------------------------------------------------------------------------------------------


def test_retire_waits_out_a_row_held_by_a_dead_process_and_ends_done(tmp_path, clock):
    """V37/V38: ``busy_elsewhere`` from ``source.retire`` (the source row of the parked runner, DESIGN §3.1) is not
    "released": the conductor waits within the stale window, retires and only then ends DONE. Mutant: the old
    ``except InvalidState`` branch ⇒ DONE with "the copies are independent" and the source still ``prepared``."""
    h = Harness(tmp_path, clock)
    h.world.inject("source", "start_retire", "error", **_busy(4))
    _run(h)
    row = _row(h)
    assert row["state"] == "done", (row["state"], row["last_error"])
    h.world.assert_outcome("done")
    assert not any("independent" in w for w in (row["result"] or {}).get("warnings") or ()), row["result"]


def test_retire_busy_beyond_the_window_parks_retiring_never_done(tmp_path, clock):
    """V37: a row that stays held past the stale window and the retries parks the job RETIRING
    (``active_source_pending``) — the source keeps its primary lock; DONE never appears."""
    h = Harness(tmp_path, clock)
    h.world.inject("source", "start_retire", "error", **_busy(10_000))
    _run(h)
    row = _row(h)
    assert row["state"] == "retiring", row["state"]
    assert row["result"]["outcome"] == "active_source_pending"
    assert row["last_error"]["details"]["reason"] == "busy_elsewhere"
    assert h.world.source.state == "prepared" and h.world.source.primary_lock
    assert "done" not in h.states() and h.world.violations == []


def test_retire_answer_without_a_confirming_state_parks(tmp_path, clock):
    """V37: a retire operation that finishes ``done`` but names a state other than ``retired`` (here ``retiring``)
    or none at all is not a confirmation: RETIRING, parked, reason ``not_confirmed``."""
    for answer in ({"state": "retiring"}, {}):
        h = Harness(tmp_path, clock)
        src = h.world.source

        def odd_retire(job, ack, src=src, answer=answer):
            return src._call("start_retire", job, lambda: src._start("retire", "odd", lambda: dict(answer)))

        src.start_retire = odd_retire
        _run(h)
        row = _row(h)
        assert row["state"] == "retiring", (answer, row["state"])
        assert row["last_error"]["details"]["reason"] == "not_confirmed", row["last_error"]
        assert row["result"]["outcome"] == "active_source_pending"


def test_retire_refused_as_reclaimed_ends_done_with_the_independence_warning(tmp_path, clock):
    """DESIGN §3.7: the user took the session back on the source (``reclaimed``) — an explicit answer, so the job
    ends DONE with the warning that the copies are independent (the only case that warning is given)."""
    h = Harness(tmp_path, clock)

    def reclaim(world):
        world.source.reclaim(force=True)

    h.world.inject("source", "start_retire", "action", action=reclaim)
    _run(h)
    row = _row(h)
    assert row["state"] == "done", row["state"]
    assert any("reclaimed" in w and "independent" in w for w in row["result"]["warnings"]), row["result"]


def test_retire_refused_with_another_state_parks(tmp_path, clock):
    """V37: an ``invalid_state`` refusal of the retire naming no released/reclaimed state is not a confirmation."""
    h = Harness(tmp_path, clock)
    h.world.inject("source", "start_retire", "error", error=InvalidState("odd", reason="state", state="locked"))
    _run(h)
    row = _row(h)
    assert row["state"] == "retiring" and row["result"]["outcome"] == "active_source_pending"
    assert h.world.source.primary_lock and h.world.violations == []


# ---------------------------------------------------------------------------------------------------------
# V37 — abort: ABORTED only on explicit confirmations, target first
# ---------------------------------------------------------------------------------------------------------


def test_abort_waits_for_the_target_before_releasing_the_source(tmp_path, clock):
    """A target refusing ``abort`` with ``busy_elsewhere`` is waited out; the source is released only after the
    target confirmed (before: the refusal was ignored and the source released at once)."""
    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_prepare", "error", error=ImportFailed("injected", hermes_error="X"))
    h.world.inject("target", "abort", "error", **_busy(3))
    _run(h)
    assert h.state() == "aborted"
    h.world.assert_outcome("aborted")
    log = h.world.log
    last_abort = max(i for i, e in enumerate(log) if e == ("target", "abort"))
    release = log.index(("source", "start_release"))
    assert last_abort < release and log.count(("target", "abort")) == 4, log


def test_abort_refused_for_good_parks_without_releasing_behind_the_target(tmp_path, clock):
    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_prepare", "error", error=ImportFailed("injected", hermes_error="X"))
    h.world.inject("target", "abort", "error", **_busy(10_000))
    _run(h)
    assert h.state() == "aborting"
    assert ("source", "start_release") not in h.world.log
    assert _row(h)["last_error"]["details"]["reason"] == "busy_elsewhere"


def test_abort_with_an_unconfirmed_release_parks(tmp_path, clock):
    """A release that reports no ``released``/``reclaimed`` state is not a confirmation: ABORTING stays."""
    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_prepare", "error", error=ImportFailed("injected", hermes_error="X"))
    src = h.world.source

    def odd_release(job, reason):
        return src._call("start_release", job, lambda: src._start("release", "odd", lambda: {"state": "locked"}))

    src.start_release = odd_release
    _run(h)
    assert h.state() == "aborting"
    assert _row(h)["last_error"]["details"]["reason"] == "not_confirmed"


# ---------------------------------------------------------------------------------------------------------
# V38 — participant restart: busy_elsewhere is waited out
# ---------------------------------------------------------------------------------------------------------


def test_participant_restart_in_planning_ends_done(tmp_path, clock):
    """V38: ``invalid_state busy_elsewhere`` (as it arrives over the wire: ``retryable`` False, the reason in the
    details) at ``target.negotiate`` for a while ⇒ the job continues and ends DONE, not ABORTED."""
    h = Harness(tmp_path, clock)
    h.world.inject("target", "negotiate", "error", error=InvalidState("busy", reason="busy_elsewhere"), times=6)
    _run(h)
    assert h.state() == "done"
    h.world.assert_outcome("done")


def test_participant_busy_past_the_window_suspends_after_the_lock(tmp_path, clock):
    """A row held past the stale window after the lock is a transient failure: SUSPENDED (resume later), never an
    abort of a job that could continue."""
    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_stage_context", "error", **_busy(10_000))
    _run(h)
    assert h.state() == "suspended", (h.state(), _row(h)["last_error"])


# ---------------------------------------------------------------------------------------------------------
# V24 (A part) — a commit refused for good
# ---------------------------------------------------------------------------------------------------------


def test_commit_refused_for_good_before_the_target_committed_aborts_at_once(tmp_path, clock):
    """V24: the commit operation fails with a non-retryable error while the target still reports a state before
    ``committing`` — repeating cannot help: ABORTED at the first status, the target aborted first, the source
    released (before: up to ``stall_timeout_s`` IN_DOUBT, then parked ``in_doubt``)."""
    h = Harness(tmp_path, clock)
    tgt = h.world.target

    def refuse_commit(job):
        def fail() -> dict:
            raise ImportFailed("bundle commit before prepare", hermes_error="X")

        return tgt._call("start_commit", job, lambda: tgt._start("commit", "refused", fail))

    tgt.start_commit = refuse_commit
    t0 = clock()
    _run(h)
    assert h.state() == "aborted", h.states()
    h.world.assert_outcome("aborted")
    assert "in_doubt" in h.states() and clock() - t0 < 120
    assert h.states().count("committing") == 1


# ---------------------------------------------------------------------------------------------------------
# V33 — writer threads in the caller's context
# ---------------------------------------------------------------------------------------------------------


_PROFILE: contextvars.ContextVar[str] = contextvars.ContextVar("profile", default="none")


def test_block_writers_run_in_the_callers_context(tmp_path, clock):
    """V33: ``move_object`` writes with two batches in flight in worker threads; each sees the caller's context
    variables (profile scope, secrets) — mutant: ``pool.submit(dst.write_blocks, frames)``."""
    h = Harness(tmp_path, clock)
    src = h.world.source.blocks_store
    m = src.put_object(b"x" * (3 * 524_288 + 17), "application/octet-stream")
    dst = h.world.target.blocks_store
    seen: list[tuple[str, str]] = []
    real = dst.write_blocks

    def write(frames):
        seen.append((threading.current_thread().name, _PROFILE.get()))
        return real(frames)

    dst.write_blocks = write
    token = _PROFILE.set("work")
    try:
        move_object(
            src,
            dst,
            [m.object_id],
            reporter=RecordingReporter(),
            cancelled=lambda: False,
            hosted=False,
            request_bytes=524_288,
        )
    finally:
        _PROFILE.reset(token)
    assert len(seen) >= 2 and {p for _t, p in seen} == {"work"}, seen
    assert all(t.startswith("ctxh-move") for t, _p in seen)


# ---------------------------------------------------------------------------------------------------------
# V34/V09 — what the pack left behind reaches the note and the result
# ---------------------------------------------------------------------------------------------------------


def test_pack_exclusions_reach_the_note_parts_and_the_result(tmp_path, clock):
    h = Harness(tmp_path, clock)
    store = h.world.source.blocks_store
    real_put = store.put_doc

    def put_doc(doc):
        if isinstance(doc, dict) and doc.get("kind") == "ctxh.pack" and doc.get("items"):
            items = [dict(i) for i in doc["items"]]
            items[0]["excluded"] = [
                {"path": ".env", "why": "secret"},
                {"path": "stash@{1} (wip): .env", "why": "stash_secret"},
                {"path": "vendor/sub", "why": "nested_repo"},
            ]
            doc = {**doc, "items": items}
        return real_put(doc)

    store.put_doc = put_doc
    sent: list[dict] = []
    real_prepare = h.world.target.start_prepare

    def prepare(job, req):
        sent.append(dict(req.note_parts))
        return real_prepare(job, req)

    h.world.target.start_prepare = prepare
    _run(h)
    assert h.state() == "done"
    parts = sent[-1]
    assert any(p.endswith("/.env") for p in parts["secrets_excluded"]), parts
    assert parts["withheld_stash"] == ["stash@{1} (wip): .env"]
    assert any(e["path"].endswith("/vendor/sub") and e["why"] == "nested_repo" for e in parts["not_transferred"])
    left = _row(h)["result"]["left_behind"]
    assert {e["why"] for e in left} == {"secret", "stash_secret", "nested_repo"}, left


# ---------------------------------------------------------------------------------------------------------
# V36 — export --keep
# ---------------------------------------------------------------------------------------------------------


def test_export_keep_leaves_the_source_active_and_ends_done(tmp_path, clock):
    """V36/O3: no ``source.prepare`` (no primary lock, no veto), no retire; the source is released after the
    commit and the job ends DONE with ``source_kept``; the note parts say the session stays active there."""
    h = Harness(tmp_path, clock, mode=Mode.EXPORT, options=Options(keep_source=True))
    sent: list[dict] = []
    real_prepare = h.world.target.start_prepare

    def prepare(job, req):
        sent.append(dict(req.note_parts))
        return real_prepare(job, req)

    h.world.target.start_prepare = prepare
    _run(h)
    row = _row(h)
    assert row["state"] == "done", (row["state"], row["last_error"])
    src = h.world.source
    assert "start_prepare" not in h.calls("source") and "start_retire" not in h.calls("source")
    assert src.state == "released" and not src.primary_lock and not src.veto and not src.leases
    assert row["result"]["source_kept"] is True and sent[-1]["source_kept"] is True


# ---------------------------------------------------------------------------------------------------------
# V28 — target facts per peer
# ---------------------------------------------------------------------------------------------------------


def test_push_keeps_the_target_facts_of_the_peer(tmp_path, clock):
    from ctx_handover.engine.model import Peer, Right

    h = Harness(tmp_path, clock)
    peer = Peer(name="server", peer_id="01HZX3P0RR0000000000000000", direction="dial", url=None, pubkey="",
                rights=frozenset({Right.PUSH}))  # fmt: skip
    c = h.conductor()
    c.job = dataclasses.replace(c.job, peer=peer)
    c.run_until_negotiated()
    tf = c._get("target_facts")
    assert h.ledger.peer_facts_get(peer.peer_id) == tf


# ---------------------------------------------------------------------------------------------------------
# V29 — pull --include within what the source releases
# ---------------------------------------------------------------------------------------------------------


class _Planning(FakePlanning):
    def user_item(self, *a, **kw):
        return planning.user_item(*a, **kw)


def test_pull_include_refused_by_the_source_is_withheld_not_the_job(tmp_path, clock):
    """V29: two local ``--include`` paths become conductor items ``u0``/``u1``; the source refuses ``u1``
    (``policy_denied`` naming only it) — ``u1`` leaves the plan, the target negotiates the rest, the job ends DONE
    with ``withheld`` = that path; ``u0`` is packed. A refusal naming a source item still ends the job."""
    hints = PlanHints(include=("/srv/shared/data.txt", "/srv/private/keep.txt"))
    h = Harness(tmp_path, clock, mode=Mode.PULL, options=Options(hints=hints))
    h.planning = _Planning()
    src = h.world.source
    real_pack = src.start_pack

    def pack(job, final):
        ids = {i.item_id: i for i in final.plan.items}
        if "u1" in ids:
            raise PolicyDenied("'/srv/private/keep.txt' is not released", reason="not_allowed_root", items=["u1"])
        return real_pack(job, final)

    src.start_pack = pack
    _run(h)
    row = _row(h)
    assert row["state"] == "done", (row["state"], row["last_error"])
    assert row["result"]["withheld"] == ["/srv/private/keep.txt"]
    packed = {i.item_id: i for i in src.packed_plan.plan.items}
    assert "u0" in packed and "u1" not in packed
    assert to_wire(packed["u0"])["source_path"] == "/srv/shared/data.txt"


def test_pull_refusal_of_a_source_item_still_ends_the_job(tmp_path, clock):
    h = Harness(tmp_path, clock, mode=Mode.PULL)
    src = h.world.source

    def pack(job, final):
        raise PolicyDenied("item differs", reason="not_allowed_root", items=[final.plan.items[0].item_id])

    src.start_pack = pack
    _run(h)
    assert h.state() == "aborted" and _row(h)["last_error"]["code"] == "policy_denied"


def test_a_lease_lost_while_waiting_out_a_busy_row_ends_the_job_cleanly(tmp_path, clock):
    """The pause of an error path can fail itself (the keepalive finds the lease lost while the conductor waits out
    a busy participant row): the runner handles that failure like any other — the job ends ABORTED with
    ``lease_lost`` and the source released — instead of the exception escaping ``run()``."""
    h = Harness(tmp_path, clock)
    calls = []

    def busy_stage(job, capsule):
        calls.append(1)
        if len(calls) == 2:
            h.world.source.lease_lost = True
        raise InvalidState("another process is working on this job", reason="busy_elsewhere")

    h.world.target.start_stage_context = busy_stage
    _run(h)
    row = _row(h)
    assert row["state"] == "aborted", (row["state"], row["last_error"])
    assert row["last_error"]["code"] == "lease_lost"
    h.world.assert_outcome("aborted")
