"""Round G‴ of cluster A — the E2E findings of ``reports/L3-findings.md`` (phase 2) reproduced without Hermes:

* L3-F09 (R34) — the refinement of ``ctx.llm.complete_structured`` is taken: the host returns
  ``PluginLlmStructuredResult`` (upstream ``agent/plugin_llm.py``), its ``parsed`` holds the answer; an offer created
  with it carries the refinement.
* L3-F07 — ``/handover cancel`` of a SUSPENDED job aborts it: job terminal, source released, the target's
  participant slot free (no ``rate_limited`` for the next handovers); a cancel whose target cannot be reached is
  finished by the janitor; an open offer and this host's own jobs do not use up a peer's slots.
* L3-F12 (A part) — ``take --offer <id>`` redeems the offer: the source resolves the offer's session for the peer it
  was made for (claimed for that job) and the pull ends DONE with the offer's refinement in the note.
* L3-F11 — the source never opened the job (it refused at FACTS): the abort ends ABORTED with the real cause.
* L3-F16 — ``reclaim`` does not wait for a target that does not answer; L3-F14 (A part) — a refused reclaim says so
  (``ok: False`` and an error); L17 (A part) — status names the target session (id and title).
* Hand-overs of the integrator — the bundle-import sentence travels as ``note_parts["bundle_import"]``.
"""

from __future__ import annotations

import dataclasses
import errno
import json
import os
import signal
import threading
import time

import pytest

from ctx_handover.engine import bundle as bundle_mod
from ctx_handover.engine import jobs, refine
from ctx_handover.engine.errors import HandoverError, NotFound, RateLimited
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, OpState, Options, Peer, Right, Role, SessionRef
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes.apis_fake import SimulatedCrash
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.integration.harness import Home, session_marks
from ctx_handover.tests.unit.A._l2r_helpers import (
    OFF,
    ctx_of,
    dirty_repo,
    errors,
    note_of,
    paired,
    session_in,
    state,
)
from ctx_handover.tests.unit.A.harness import FAST, Harness
from ctx_handover.tests.unit.A.test_gp_a import _poll_fast


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    yield Home(tmp_path, "server"), Home(tmp_path, "laptop")
    jobs._reset_for_tests()


NOTE = "G3A-NOTE: run the migrations before the tests"
REPLY = {"notes_for_target": NOTE, "tools_needed": ["rg"]}


def _structured_result(parsed):
    """The object ``ctx.llm.complete_structured`` returns on the host (upstream ``PluginLlmStructuredResult``)."""
    require_upstream()
    from agent.plugin_llm import PluginLlmStructuredResult

    return PluginLlmStructuredResult(
        text=json.dumps(parsed),
        provider="custom",
        model="fake-model",
        agent_id="default",
        parsed=parsed,
        content_type="json",
    )


def _llm_ctx(parsed) -> FakeCtx:
    ctx = FakeCtx()
    ctx.llm_result = _structured_result(parsed)
    return ctx


# ---------------------------------------------------------------------------------------------------------
# L3-F09 (R34) — the structured answer of ctx.llm is taken
# ---------------------------------------------------------------------------------------------------------


def test_llm_answer_in_the_upstream_result_form_is_taken():
    """R34/L3-F09: ``ctx.llm.complete_structured`` returns ``PluginLlmStructuredResult``; the plugin hands its
    ``parsed`` object to ``refine.validate`` (lab: the result object itself ⇒ "refinement: not an object")."""
    ctx = _llm_ctx(REPLY)
    llm = jobs._llm(ctx)
    assert llm is not None
    assert llm("instructions", [{"type": "text", "text": "x"}], refine.REFINE_SCHEMA, 5) == REPLY
    assert ctx.llm_calls and ctx.llm_calls[0]["purpose"] == "ctx_handover.refine"
    assert refine.validate(llm("i", [], refine.REFINE_SCHEMA, 5)).notes == NOTE


def test_llm_reply_that_was_no_json_is_no_answer():
    """``parsed`` is None when the reply was not JSON (upstream sets ``content_type="text"``): no answer — the result
    object is never passed on as the refinement."""
    ctx = FakeCtx()
    ctx.llm_result = dataclasses.replace(_structured_result(REPLY), parsed=None, content_type="text")
    assert jobs._llm(ctx)("i", [], refine.REFINE_SCHEMA, 5) is None


def test_offer_created_with_ctx_llm_carries_the_refinement(homes):
    """E17 (L3-F09): ``/handover to <dial-only peer>`` stores an offer; its refinement comes from the session's own
    model through ``ctx.llm`` (lab: ``refinement: null``, "refinement: not an object")."""
    server, _laptop = homes
    repo = dirty_repo(server.home / "proj")
    sid = session_in(server, repo.root, "Offered")
    ctx = _llm_ctx(REPLY)
    with server.active():
        ledger = jobs._profile().ledger
        laptop = _inbound(ledger)
        runner = jobs.runner_for(None)
        offer = runner.create_offer(laptop, SessionRef(id=sid), ctx=ctx, options=Options(refine="llm", yes=True))
        stored = ledger.offer_get(offer["offer_id"])
    assert ctx.llm_calls, "the session's model was not asked"
    assert offer["has_refinement"] is True and offer["session"] == sid, offer
    assert NOTE in json.dumps(stored["refinement"]), stored


def _inbound(led: Ledger, name: str = "laptop") -> Peer:
    from ctx_handover.engine.ids import ulid

    peer = Peer(name=name, peer_id=ulid(), direction="inbound", url=None, pubkey="pk-" + name, rights=frozenset(Right))
    led.upsert_peer(peer)
    return peer


# ---------------------------------------------------------------------------------------------------------
# L3-F07 — cancel of a SUSPENDED job
# ---------------------------------------------------------------------------------------------------------


def _suspended(tmp_path, clock) -> Harness:
    """A push parked SUSPENDED: the target was unreachable at TARGET_PREPARING (the lab stopped ``serve-b``)."""
    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_prepare", "disconnect", times=FAST.retry_attempts + 1)
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == "suspended", h.states()
    assert h.world.source.state == "locked" and h.world.target.state not in (None, "aborted")
    return h


def _runner_ended(h: Harness) -> None:
    """The runner that parked the job gave it up (``JobRunner._run`` releases its owner when the run stops)."""
    row = h.ledger.job(h.job_id, Role.CONDUCTOR)
    h.ledger.release_owner(h.job_id, Role.CONDUCTOR, row["owner"], row["owner_epoch"])


def _runner(h: Harness) -> jobs.JobRunner:
    """The runner of a new process (``hermes handover cancel``) over the same ledger and participants."""
    return jobs.JobRunner(
        h.ledger,
        apis_factory=lambda row: (h.world.source, h.world.target),
        planning=h.planning,
        clock=h.clock,
        sleep=h.world.sleep,
        poll=FAST,
        keepalive=False,
    )


def test_cancel_of_a_suspended_job_aborts_it(tmp_path, clock):
    """L3-F07: ``cancel`` on a SUSPENDED job ends it ABORTED — target aborted (its participant slot is free),
    source released — instead of only setting the flag nobody reads (lab: job stays ``suspended``, source
    ``locked``, target ``negotiated``/``preparing``)."""
    h = _suspended(tmp_path, clock)
    _runner_ended(h)
    answer = _runner(h).cancel(h.job_id)
    assert h.state() == "aborted", (h.states(), answer)
    h.world.assert_outcome("aborted")
    assert h.world.target.state == "aborted" and h.world.source.state == "released"
    assert "cancelled" in answer, answer
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]["code"] == "cancelled"


def test_cancel_while_the_target_is_unreachable_is_finished_by_the_janitor(tmp_path, clock):
    """L3-F07 with the target still down: the cancel cannot get the target's confirmation, so the source stays
    with the target first (V37) and the job ``aborting``; the answer says so. The janitor finishes the abort once
    the target answers again."""
    h = _suspended(tmp_path, clock)
    _runner_ended(h)
    down = h.world.inject("target", "abort", "disconnect", times=10_000)
    runner = _runner(h)
    answer = runner.cancel(h.job_id)
    assert h.state() == "aborting", h.states()
    assert "not confirmed" in answer, answer
    assert ("source", "start_release") not in h.world.log
    down.times = down.fired  # the target is back
    jobs.janitor(h.ledger, runner, None, flush_outbox=None, background=False)
    assert h.state() == "aborted", h.states()
    h.world.assert_outcome("aborted")


def test_janitor_aborts_a_cancelled_suspended_job(tmp_path, clock):
    """A cancel whose own run did not get to the abort (its process ended) is not left SUSPENDED for ever: the
    janitor aborts a SUSPENDED job that carries a cancel request (before: suspended jobs were skipped)."""
    h = _suspended(tmp_path, clock)
    h.ledger.set_cancel(h.job_id)
    h.stale()
    jobs.janitor(h.ledger, _runner(h), None, flush_outbox=None, background=False)
    assert h.state() == "aborted", h.states()
    h.world.assert_outcome("aborted")


def test_janitor_still_leaves_an_uncancelled_suspended_job_alone(tmp_path, clock):
    h = _suspended(tmp_path, clock)
    h.stale()
    jobs.janitor(h.ledger, _runner(h), None, flush_outbox=None, background=False)
    assert h.state() == "suspended", h.states()


def test_a_dead_runner_of_a_cancelled_job_is_aborted_not_suspended(tmp_path, clock):
    """DESIGN §3.8/§3.6: the runner died after the user cancelled (before the commit point) — recovery aborts the
    job instead of parking it SUSPENDED (where no runner would read the request again)."""
    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_prepare", "crash")
    c = h.conductor()
    c.run_until_negotiated()
    with pytest.raises(SimulatedCrash):
        c.run()
    h.ledger.set_cancel(h.job_id)
    h.stale()
    h.conductor().recover()
    assert h.state() == "aborted", h.states()
    h.world.assert_outcome("aborted")


def _registry(led: Ledger, clock) -> ParticipantRegistry:
    return ParticipantRegistry(led, lambda role, peer: None, clock=clock, max_jobs_per_peer=2, max_jobs=4)


def test_an_open_offer_and_own_jobs_do_not_use_up_a_peers_slots(tmp_path, clock):
    """L3-F07 (run 1, E19): an open offer (its planning row, role source, ``open``) and the local participant rows
    of this host's own jobs (a pending export) counted against the caps, so B → A was ``rate_limited``. Only jobs a
    remote conductor opened here count."""
    from ctx_handover.engine.ids import ulid

    led = Ledger(tmp_path / "caps.sqlite3", clock=clock)
    peer = _inbound(led)
    for mode, role in ((Mode.OFFER, Role.SOURCE), (Mode.PUSH, Role.SOURCE), (Mode.PULL, Role.TARGET)):
        led.create_job(ulid(), role, mode, peer.peer_id, {"role": str(role), "local": True})
    for _ in range(3):
        led.create_job(ulid(), Role.SOURCE, Mode.EXPORT, None, {"role": "source", "local": True})
    reg = _registry(led, clock)
    reg._check_caps(peer.peer_id)  # nothing remote yet
    for _ in range(2):
        led.create_job(ulid(), Role.TARGET, Mode.PUSH, peer.peer_id, {"role": "target", "mode": "push"})
    with pytest.raises(RateLimited):
        reg._check_caps(peer.peer_id)
    led.close()


def test_an_offer_leaves_no_open_source_job(homes):
    """The planning row an offer opens ends at once: it is no job (no slot, not listed as a running source)."""
    server, _laptop = homes
    repo = dirty_repo(server.home / "proj")
    sid = session_in(server, repo.root, "Offered once")
    with server.active():
        ledger = jobs._profile().ledger
        offer = jobs.runner_for(None).create_offer(_inbound(ledger), SessionRef(id=sid), ctx=None, options=OFF)
        row = ledger.job(offer["offer_id"], Role.SOURCE)
        listed = [j["job_id"] for j in jobs.runner_for(None).status(None)["jobs"]]
    assert row is not None and row["state"] == "released", row
    assert offer["offer_id"] not in listed, listed


def test_an_offer_from_the_calling_session_names_that_session(homes):
    """g3-b (F12): an offer made in the session itself (no ``--session``) stores the session the plan resolved,
    not an empty id (else the offer could never be redeemed)."""
    server, _laptop = homes
    repo = dirty_repo(server.home / "proj")
    sid = session_in(server, repo.root, "Offered from inside")
    with server.active():
        ledger = jobs._profile().ledger
        peer = _inbound(ledger)
        runner = jobs.runner_for(None)
        source = jobs.make_local_apis(None, calling_session=sid, session_key=None)[0]
        offer = runner.create_offer(peer, SessionRef(), ctx=None, local_source=source, options=OFF)
        stored = ledger.offer_get(offer["offer_id"])
    assert stored["session_id"] == sid and offer["session"] == sid, stored


def test_cancelled_suspended_pushes_free_the_targets_slots(homes, monkeypatch):
    """L3-F07 end to end: two pushes park SUSPENDED (the server stopped answering while they prepared) and are
    cancelled from a new process. Each ends ABORTED, the laptop's session is released and untouched, the server's
    participant jobs are aborted — and a third push gets through (lab: the server answered ``rate_limited``, "too
    many concurrent handovers for this device", to every further push)."""
    from ctx_handover.engine import remote
    from ctx_handover.engine.errors import PeerUnreachable

    server, laptop = homes
    repo = dirty_repo(laptop.home / "proj")
    with paired(server, laptop) as (_listener, peer):
        parked = []
        for n in range(2):
            sid = session_in(laptop, repo.root, f"Parked {n}")

            def gone(self, job, req):
                raise PeerUnreachable("server is not reachable (stopped)")

            monkeypatch.setattr(remote.RemoteTarget, "start_prepare", gone)
            with laptop.active():
                runner = jobs.JobRunner(jobs.runner_for(None).ledger, poll=_poll_fast())
                job_id, final = runner.plan_sync(
                    Mode.PUSH, peer, OFF, session_ref=SessionRef(id=sid), calling_session=None, session_key=None
                )
                assert final is not None, errors(laptop, job_id)
                runner.run_foreground(job_id)
            monkeypatch.undo()
            assert state(laptop, job_id) == "suspended", errors(laptop, job_id)
            with laptop.active():
                answer = jobs.JobRunner(jobs.runner_for(None).ledger, poll=_poll_fast()).cancel(job_id)
            parked.append((job_id, sid, answer))
        for job_id, sid, answer in parked:
            assert state(laptop, job_id) == "aborted", (answer, errors(laptop, job_id))
            assert laptop.ledger().source_state(job_id)["state"] == "released"
            marks = session_marks(laptop.db(), sid)
            assert not marks["archived"] and not marks["hidden"] and marks["end_reason"] is None, marks
            assert server.ledger().job(job_id, Role.TARGET)["state"] == "aborted"
        third = session_in(laptop, repo.root, "Third")
        push_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=third), options=OFF)
        assert (state(laptop, push_id), rc) == ("done", 0), errors(laptop, push_id)


# ---------------------------------------------------------------------------------------------------------
# L3-F12 — take --offer
# ---------------------------------------------------------------------------------------------------------


def test_take_offer_ends_done_and_claims_the_offer(homes):
    """L3-F12 (O6): ``take --offer <id>`` — a pull naming only the offer — ends DONE: the source resolves the offer's
    session for this peer and claims the offer for the job; the session arrives (lab: "no session given", offer
    unclaimed and holding a slot for two hours)."""
    server, laptop = homes
    repo = dirty_repo(server.home / "proj")
    sid = session_in(server, repo.root, "Offered plainly")
    with paired(server, laptop) as (_listener, peer):
        with server.active():
            offer = jobs.runner_for(None).create_offer(
                jobs._profile().ledger.peer("laptop"), SessionRef(id=sid), ctx=None, options=OFF
            )
        job_id, rc = laptop.run(Mode.PULL, peer, SessionRef(offer_id=offer["offer_id"]), options=OFF)
        assert (state(laptop, job_id), rc) == ("done", 0), errors(laptop, job_id)
    assert server.ledger().offer_get(offer["offer_id"])["claimed_job"] == job_id
    assert server.ledger().source_state(job_id)["state"] == "retired"
    assert session_marks(server.db(), sid)["archived"]
    with laptop.active():
        ack = jobs.runner_for(None).status(job_id)["job"]["ack"]
    assert laptop.db().get_session(ack["session_id"]) is not None


def test_take_offer_redeems_the_offer_and_carries_its_refinement(homes):
    """E17 / L3-F12 (O6): the server leaves an offer for the laptop (refinement from the session's model through
    ``ctx.llm``); ``take --offer <id>`` on the laptop ends DONE: the offer is claimed by that job, the session arrives
    and its note carries the offer's refinement (lab: "no session given", the offer stayed unclaimed)."""
    server, laptop = homes
    server.write_settings({"refine": "auto"})
    repo = dirty_repo(server.home / "proj")
    sid = session_in(server, repo.root, "Offered for the laptop")
    with paired(server, laptop) as (_listener, peer):
        with server.active():
            ledger = jobs._profile().ledger
            to_laptop = ledger.peer("laptop")
            offer = jobs.runner_for(None).create_offer(
                to_laptop, SessionRef(id=sid), ctx=_llm_ctx(REPLY), options=Options(refine="llm", yes=True)
            )
        assert offer["has_refinement"], offer
        assert server.ledger().offer_get(offer["offer_id"])["claimed_job"] is None  # never claimed on its own
        # the laptop's default ``refine: auto`` (a remote ``off`` switches the source's refinement off, R18)
        take = dataclasses.replace(OFF, refine="auto")
        job_id, rc = laptop.run(Mode.PULL, peer, SessionRef(offer_id=offer["offer_id"]), options=take)
        assert (state(laptop, job_id), rc) == ("done", 0), errors(laptop, job_id)
    assert server.ledger().offer_get(offer["offer_id"])["claimed_job"] == job_id
    assert server.ledger().source_state(job_id)["state"] == "retired"
    plan = ctx_of(server, job_id, Role.SOURCE).get("plan") or {}
    assert plan.get("refine", {}).get("used") == "offer", plan.get("refine")
    assert NOTE in note_of(laptop, job_id)


def test_take_offer_of_another_peer_is_refused(homes):
    """An offer is redeemed only by the peer it was made for: another peer's offer id ⇒ ``not_found``, nothing
    claimed, nothing locked."""
    server, laptop = homes
    repo = dirty_repo(server.home / "proj")
    sid = session_in(server, repo.root, "Not for you")
    with paired(server, laptop) as (_listener, peer):
        with server.active():
            ledger = jobs._profile().ledger
            offer = jobs.runner_for(None).create_offer(
                _inbound(ledger, "other"), SessionRef(id=sid), ctx=None, options=OFF
            )
        with pytest.raises(HandoverError):
            laptop.run(Mode.PULL, peer, SessionRef(offer_id=offer["offer_id"]), options=OFF)
        job_id = laptop.ledger().jobs(role=Role.CONDUCTOR, limit=1)[0]["job_id"]
    assert state(laptop, job_id) == "aborted"
    assert server.ledger().offer_get(offer["offer_id"])["claimed_job"] is None
    assert not session_marks(server.db(), sid)["archived"]


# ---------------------------------------------------------------------------------------------------------
# L3-F11 — the source never opened the job
# ---------------------------------------------------------------------------------------------------------


def test_abort_ends_when_the_source_never_opened_the_job(tmp_path, clock):
    """L3-F11 (E6, L15): the pull's source refused at FACTS (``rate_limited``) and so never created the job; its
    release answers ``not_found``. The job ends ABORTED and keeps the real cause (lab: stuck ``aborting``, the CLI
    said "job … not found")."""
    h = Harness(tmp_path, clock, mode=Mode.PULL)
    h.world.inject("source", "facts", "error", error=RateLimited("too many concurrent handovers"), times=10_000)
    h.world.inject("source", "start_release", "error", error=NotFound(f"job {h.job_id} not found"), times=10_000)
    with pytest.raises(HandoverError):
        h.conductor().run_until_negotiated()
    assert h.state() == "aborted", h.states()
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]["code"] == "rate_limited"


def test_a_lost_release_operation_is_no_release(tmp_path, clock):
    """Only the refusal of the release request means "never opened": a release operation the source forgets while
    it is polled (``not_found`` from its status) is no confirmation — the job stays ``aborting`` (V37)."""
    h = Harness(tmp_path, clock, mode=Mode.PULL)
    h.world.inject("target", "negotiate", "error", error=HandoverError("no", retryable=False))
    src = h.world.source
    real_release, real_status, ids = src.start_release, src.op_status, []

    def release(job, reason):
        st = real_release(job, reason)
        ids.append(st.op_id)
        return dataclasses.replace(st, state=OpState.RUNNING, result=None)

    def op_status(job, op_id):
        if op_id in ids:
            raise NotFound("operation not known to the peer", op_id=op_id)
        return real_status(job, op_id)

    src.start_release, src.op_status = release, op_status
    with pytest.raises(HandoverError):
        h.conductor().run_until_negotiated()
    assert h.state() == "aborting", h.states()


# ---------------------------------------------------------------------------------------------------------
# L3-F16, L3-F14, L17 — reclaim and status
# ---------------------------------------------------------------------------------------------------------


def test_status_names_the_target_session(homes):
    """L17 (A part): after a push the status on both sides names the session the handover created on the target —
    id and title as the target's session table holds them — for F's final message."""
    server, laptop = homes
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Where did it go")
    with paired(server, laptop) as (_listener, peer):
        push_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=OFF)
        assert rc == 0, errors(laptop, push_id)
    arrived = next(r for r in server.ledger().arrival_rows() if r["job_id"] == push_id)
    title = (server.db().get_session(arrived["session_id"]) or {}).get("title")
    with laptop.active():
        here = jobs.runner_for(None).status(push_id)["job"]
    with server.active():
        there = jobs.runner_for(None).status(push_id)["job"]
    assert title and here["target_session"] == {"id": arrived["session_id"], "title": title}, here
    assert there["target_session"] == {"id": arrived["session_id"], "title": title}, there


def test_reclaim_does_not_wait_for_a_target_that_does_not_answer(homes):
    """L3-F16: after a push the server process hangs (SIGSTOP, as in the lab's E10). ``reclaim`` takes the session
    back at once and returns within the notice's bound — ``target_notified`` False — instead of blocking on the
    ``source_reclaimed`` notice (lab: no return after 180 s)."""
    server, laptop = homes
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Reclaimed while the server sleeps")
    out: dict = {}

    def reclaim() -> None:
        with laptop.active():
            out["result"] = jobs.reclaim(sid, force=False)

    with paired(server, laptop) as (listener, peer):
        push_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=OFF)
        assert rc == 0, errors(laptop, push_id)
        os.kill(listener.proc.pid, signal.SIGSTOP)
        worker = threading.Thread(target=reclaim, daemon=True)
        try:
            started = time.monotonic()
            worker.start()
            worker.join(BOUND_S)
            took = time.monotonic() - started
        finally:
            os.kill(listener.proc.pid, signal.SIGCONT)
        worker.join(600)  # the old, unbounded notice returns once the server runs again
    assert took < BOUND_S, f"reclaim still waiting for the stopped server after {took:.1f}s"
    result = out["result"]
    assert result["state"] == "reclaimed" and result["target_notified"] is False, result
    marks = session_marks(laptop.db(), sid)
    assert not marks["archived"] and not marks["hidden"], marks


BOUND_S = 20.0
"""What ``reclaim`` may take against a stopped target: the notice's wait plus the reclaim itself."""


def test_reclaim_tells_a_target_that_answers(homes):
    """The bounded notice still reaches a live target (route #8 ``source_reclaimed``, DESIGN §7.7)."""
    server, laptop = homes
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Reclaimed, server awake")
    with paired(server, laptop) as (_listener, peer):
        push_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=OFF)
        assert rc == 0, errors(laptop, push_id)
        with laptop.active():
            result = jobs.reclaim(sid, force=False)
    assert result["target_notified"] is True, result
    arrival = next(r for r in server.ledger().arrival_rows() if r["job_id"] == push_id)
    assert arrival.get("source_reclaimed_at"), arrival


def test_refused_reclaim_says_it_failed(homes, monkeypatch):
    """L3-F14 (A part): ``reclaim`` without ``--force`` while a handover is being committed is refused — the result
    carries ``ok: False`` and an error naming the reason (lab: exit 0, ``ok: true``); "nothing to reclaim" is no
    success either."""
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Being committed")
    out = laptop.root / "out" / "busy.ctxh.tar"
    out.parent.mkdir(parents=True)

    def hiccup(self):
        raise HandoverError("disk hiccup")

    monkeypatch.setattr(bundle_mod.BundleTarget, "_commit", hiccup)
    with laptop.active():
        runner = jobs.JobRunner(jobs.runner_for(None).ledger, poll=_poll_fast())
        job_id, _final = runner.plan_sync(
            Mode.EXPORT,
            None,
            dataclasses.replace(OFF, stall_timeout_s=1),
            session_ref=SessionRef(id=sid),
            calling_session=None,
            session_key=None,
            bundle_path=out,
        )
        runner.run_foreground(job_id)
        refused = jobs.reclaim(sid, force=False)
        other = session_in(laptop, repo.root, "Never handed over")
        nothing = jobs.reclaim(other, force=False)
    assert refused["state"] is None and refused["ok"] is False, refused
    assert refused["error"]["code"] == "invalid_state" and refused["error"]["details"]["reason"] == "committing"
    assert nothing["ok"] is False and nothing["error"]["details"]["reason"] == "nothing_to_reclaim", nothing


# ---------------------------------------------------------------------------------------------------------
# Hand-over of the integrator (gp-d): the bundle-import sentence is a note part
# ---------------------------------------------------------------------------------------------------------


def test_a_bundle_import_sets_the_note_part_and_the_note_says_it(homes):
    """``note_parts["bundle_import"]`` on every bundle import (``note.BUNDLE_IMPORT_SENTENCE``); the origin line
    names only the origin."""
    from ctx_handover.engine import note as note_mod
    from ctx_handover.tests.unit.A._l2r_helpers import export_pinned

    server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Bundled")
    bundle = laptop.root / "out" / "b.ctxh.tar"
    job_id, rc = export_pinned(laptop, sid, bundle)
    assert rc == 0, errors(laptop, job_id)
    imp_id, rc = server.run(
        Mode.IMPORT, None, SessionRef(), options=dataclasses.replace(OFF, trust_unsigned=True), bundle_path=bundle
    )
    assert rc == 0, errors(server, imp_id)
    text = note_of(server, imp_id)
    assert note_mod.BUNDLE_IMPORT_SENTENCE in text
    assert text.count("may have been imported elsewhere too") == 1, text


# ---------------------------------------------------------------------------------------------------------
# Hand-over gpp-cf (integrator): failures outside the error model never leave the job hanging
# ---------------------------------------------------------------------------------------------------------


def _enospc() -> OSError:
    return OSError(errno.ENOSPC, "No space left on device")


def test_a_full_disk_while_pulling_blocks_aborts_the_job(tmp_path, clock):
    """A pull writes the blocks into the local store; the disk is full (``OSError``, outside the error model).
    The job ends ABORTED with a non-retryable ``internal`` error naming it, the source released (before: the
    ``OSError`` left the runner's thread and the job stayed in its transfer state)."""
    h = Harness(tmp_path, clock, mode=Mode.PULL)
    h.world.inject("target_blocks", "write_blocks", "error", error=_enospc())
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == "aborted", h.states()
    h.world.assert_outcome("aborted")
    err = h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]
    assert err["code"] == "internal" and err["retryable"] is False, err
    assert err["details"]["exception"] == "OSError" and "No space left" in err["message"], err


def test_a_failure_outside_the_error_model_while_committing_goes_in_doubt(tmp_path, clock):
    """V37 stays: the same kind of failure when the commit is started does not release anything — the job goes
    IN_DOUBT, learns there that the target never entered its commit, and only then aborts (target first)."""
    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_commit", "error", error=_enospc())
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    states = h.states()
    assert states.index("in_doubt") < states.index("aborting") and h.state() == "aborted", states
    h.world.assert_outcome("aborted")
    log = h.world.log
    assert log.index(("target", "abort")) < log.index(("source", "start_release")), log


def test_a_runner_whose_participants_cannot_be_built_ends_the_job(tmp_path, clock):
    """``JobRunner._run``: composing the participants fails outside the error model (``OSError`` opening the
    store) — a job that locked nothing ends ABORTED instead of the exception leaving the thread."""
    h = Harness(tmp_path, clock)
    c = h.conductor()
    c.run_until_negotiated()
    _runner_ended(h)

    def broken(row):
        raise _enospc()

    runner = jobs.JobRunner(h.ledger, apis_factory=broken, planning=h.planning, clock=clock, poll=FAST)
    rc = runner.run_foreground(h.job_id)
    assert h.state() == "aborted" and rc != 0, h.states()
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]["details"]["exception"] == "OSError"
