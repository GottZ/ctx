"""Round G′ of cluster A — the lab findings reproduced without Hermes:

* V39 (L3-F06) — a bundle export completes from every surface: the gateway slash path (``plan_sync`` +
  ``continue_async`` in one process) and a run continued by another runner (``plan_sync`` here, the run in a new
  process); ``stall_timeout_s`` bounds the in-doubt wait from its first entry (commit retries do not restart it); a
  commit that cannot succeed by repeating it ends the job at once (``.partial`` removed, source free); ``reclaim``
  of an export that stays in doubt takes the session back and settles the job (``.partial`` removed, no bundle
  written behind the user's back, no warning about another machine).
* V42 (L3-F03) — when the surfaces hear DONE (and the RETIRING step before it) the job's status already names the
  target session and its model: the result is written together with the terminal transition.
* V43 (L3-F04) — ``JobRunner.status`` knows participant jobs: ``status <job>`` on the receiving side of a push
  (role ``target``, with the note) and on the source of a pull (role ``source``); a push arrival is listed in the
  overview.
* V40 (R29, A part) — the target's facts say its workspace root is unusable ⇒ the job ends before anything is
  locked or transferred, the message names the setting ``workspace_root``; an export (no receiver yet) is not
  stopped by it. The default root resolves like the policy's (``paths.expand_handover_root``) for the session cwd,
  the fallback destination and the default bundle path; the facts are probed with this host's policy.
* R30/V41 (A part) — ``--yes`` does not confirm a reason ``plan.yes_exempt`` names: the user is asked anyway.
* N05 — goal/heartbeat/loop arrive paused and the note says so; N55 — an export titles its source "exported to
  bundle <time>".
* Hand-overs of layer G — ``OFFER_TTL_S`` is two hours; ``local_source`` settings through ``settings.from_config``;
  the owner of a dead process on this host and PID namespace is taken over at once (V38 speed-up).
"""

from __future__ import annotations

import dataclasses
import json
import os
import subprocess
import sys
from datetime import datetime
from pathlib import Path

import pytest

from ctx_handover.engine import bundle as bundle_mod
from ctx_handover.engine import jobs
from ctx_handover.engine.conductor import PollPolicy
from ctx_handover.engine.errors import HandoverError, InvalidState, PolicyDenied
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.jsonutil import to_wire
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import (
    CommitAck,
    DestMode,
    EnvFacts,
    Mode,
    Options,
    Peer,
    Right,
    Role,
    SessionInfo,
    SessionRef,
)
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes.apis_fake import RecordingReporter, make_facts
from ctx_handover.tests.integration.harness import Home, session_marks
from ctx_handover.tests.unit.A._l2r_helpers import OFF, dirty_repo, errors, note_of, paired, session_in, state
from ctx_handover.tests.unit.A.harness import Harness
from ctx_handover.tests.unit.A.test_local_source import mk
from ctx_handover.tests.unit.A.test_local_target import env

assert env and mk


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    yield Home(tmp_path, "server"), Home(tmp_path, "laptop")
    jobs._reset_for_tests()


def _row(h: Harness) -> dict:
    return h.ledger.job(h.job_id, Role.CONDUCTOR)


def _run(h: Harness):
    c = h.conductor()
    c.run_until_negotiated()
    return c.run()


def _export_plan(home: Home, runner: jobs.JobRunner, sid: str, out, options: Options = OFF) -> str:
    job_id, final = runner.plan_sync(
        Mode.EXPORT,
        None,
        options,
        session_ref=SessionRef(id=sid),
        calling_session=None,
        session_key=None,
        bundle_path=out,
    )
    assert final is not None, errors(home, job_id)
    return job_id


def _partial(out) -> object:
    return out.parent / f"{out.name}.partial"


# ---------------------------------------------------------------------------------------------------------
# V39 — the bundle export from every surface
# ---------------------------------------------------------------------------------------------------------


def test_slash_export_plan_sync_then_continue_async_ends_done(homes):
    """L3-F06 (gateway slash): ``/handover export <path> --yes`` plans in the handler (``plan_sync``) and runs in a
    worker thread of the same process (``continue_async``); the job ends DONE with the bundle file and no
    ``.partial`` (lab: ``in_doubt`` "bundle commit before prepare", source locked, only ``.partial``)."""
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Slash export")
    out = laptop.root / "out" / "slash.ctxh.tar"
    out.parent.mkdir(parents=True)
    with laptop.active():
        runner = jobs.runner_for(None)
        job_id = _export_plan(laptop, runner, sid, out)
        runner.continue_async(job_id)
        runner.join(120)
    assert state(laptop, job_id) == "done", errors(laptop, job_id)
    assert out.is_file() and not _partial(out).exists()
    assert laptop.ledger().source_state(job_id)["state"] in ("retired", "exported")


def test_export_continued_by_another_runner_ends_done(homes):
    """V39/V24 across processes: the run of an export planned by one runner is continued by another one (a new
    gateway process after a restart, the janitor of another process): the bundle's progress lives in its staging,
    the job ends DONE."""
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Handed on")
    out = laptop.root / "out" / "other.ctxh.tar"
    out.parent.mkdir(parents=True)
    with laptop.active():
        first = jobs.runner_for(None)
        job_id = _export_plan(laptop, first, sid, out)
        first._forget(job_id)  # the planning process ends: its owner and participant pair are gone
        rc = jobs.JobRunner(first.ledger).run_foreground(job_id)
    assert (state(laptop, job_id), rc) == ("done", 0), errors(laptop, job_id)
    assert out.is_file() and not _partial(out).exists()


def test_export_whose_commit_cannot_succeed_ends_at_once_and_frees_the_source(homes):
    """V39: the export's recorded progress is lost (its state file in the staging is gone) — the commit fails with a
    non-retryable ``invalid_state`` ("bundle commit before prepare"). The job ends ABORTED at once — no in-doubt wait,
    no commit retries —, ``BundleTarget.abort`` removes the ``.partial`` and the session is usable again."""
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Lost progress")
    out = laptop.root / "out" / "lost.ctxh.tar"
    out.parent.mkdir(parents=True)
    with laptop.active():
        first = jobs.runner_for(None)
        job_id = _export_plan(laptop, first, sid, out)
        first._forget(job_id)
        (_partial(out) / bundle_mod.STATE_FILE).unlink()
        rc = jobs.JobRunner(first.ledger).run_foreground(job_id)
    events = laptop.ledger().events(job_id, Role.CONDUCTOR)
    states = [e["state"] for e in events if e["kind"] == "transition"]
    assert state(laptop, job_id) == "aborted" and rc != 0, (states, errors(laptop, job_id))
    assert states.count("committing") == 1, states
    assert not _partial(out).exists() and not out.exists()
    assert laptop.ledger().source_state(job_id)["state"] == "released"
    marks = session_marks(laptop.db(), sid)
    assert not marks["archived"] and not marks["hidden"] and marks["end_reason"] is None, marks


def test_stall_timeout_counts_from_the_first_in_doubt_entry(tmp_path, clock):
    """V39 (L3-F06 "Wartezeit-Grenze"): the commit request never reaches the target (it stays ``prepared``); every
    commit retry re-enters IN_DOUBT. ``stall_timeout_s`` bounds the whole wait from the first entry — the lab saw
    182 s with a one-minute limit because each entry restarted the clock. Bound: twice the limit."""
    stall = 10
    h = Harness(tmp_path, clock, options=Options(stall_timeout_s=stall))
    h.world.inject("target", "start_commit", "disconnect", times=10_000)
    _run(h)
    row = _row(h)
    assert row["state"] == "in_doubt" and row["result"]["outcome"] == "in_doubt", row["state"]
    first = min(e["ts"] for e in h.events("transition") if e["state"] == "in_doubt")
    assert clock() - first <= 2 * stall, (clock() - first, h.states())


def _poll_fast() -> PollPolicy:
    return PollPolicy(
        op_first_s=0.02,
        op_max_s=0.1,
        in_doubt_first_s=0.02,
        in_doubt_max_s=0.1,
        retry_attempts=1,
        retry_first_s=0.01,
        retry_max_s=0.02,
        retire_attempts=2,
    )


def test_reclaim_of_an_export_in_doubt_settles_the_job_and_removes_the_partial(homes, monkeypatch):
    """V39 (L3-F06, R1/D8): an export whose commit keeps failing parks ``in_doubt`` (source prepared, only a
    ``.partial``). ``reclaim --force`` gives the session back AND settles the job: ABORTED, ``.partial`` removed,
    no bundle written afterwards (the commit works again by then), and no warning about a target "on the other
    machine" — an export has none (lab: ledger stayed ``in_doubt``/``prepared``, both ``.partial`` stayed)."""
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Stuck export")
    out = laptop.root / "out" / "stuck.ctxh.tar"
    out.parent.mkdir(parents=True)

    def hiccup(self):
        raise HandoverError("disk hiccup")  # retryable: the conductor repeats the commit, then waits in doubt

    monkeypatch.setattr(bundle_mod.BundleTarget, "_commit", hiccup)
    options = dataclasses.replace(OFF, stall_timeout_s=1)
    with laptop.active():
        runner = jobs.JobRunner(jobs.runner_for(None).ledger, poll=_poll_fast())
        job_id = _export_plan(laptop, runner, sid, out, options)
        rc = runner.run_foreground(job_id)
    assert (state(laptop, job_id), rc) == ("in_doubt", jobs.EXIT_PENDING), errors(laptop, job_id)
    assert _partial(out).is_dir() and session_marks(laptop.db(), sid)["archived"]
    monkeypatch.undo()
    with laptop.active():
        result = jobs.reclaim(sid, force=True)
    assert result["state"] == "reclaimed", result
    assert state(laptop, job_id) == "aborted", errors(laptop, job_id)
    assert not _partial(out).exists() and not out.exists()
    marks = session_marks(laptop.db(), sid)
    assert not marks["archived"] and not marks["hidden"], marks
    text = " ".join(result.get("warnings") or ())
    assert "other machine" not in text and "may already be active" not in text, result
    assert "handover file" in text, result


def test_reclaim_after_a_finished_export_names_the_bundle_not_a_machine(homes):
    """V39 (R1): ``reclaim`` of an exported session (source ``exported``/``retired``) is allowed without ``--force``;
    the warning names the bundle that stays a valid copy instead of "the copy on the other machine"."""
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Exported")
    out = laptop.root / "out" / "done.ctxh.tar"
    out.parent.mkdir(parents=True)
    job_id, rc = laptop.run(Mode.EXPORT, None, SessionRef(id=sid), options=OFF, bundle_path=out)
    assert rc == 0, errors(laptop, job_id)
    with laptop.active():
        result = jobs.reclaim(sid, force=False)
    assert result["state"] == "reclaimed", result
    text = " ".join(result.get("warnings") or ())
    assert "other machine" not in text and str(out) in text, result
    assert state(laptop, job_id) == "done" and out.is_file()


# ---------------------------------------------------------------------------------------------------------
# V42 — the final message knows the target session
# ---------------------------------------------------------------------------------------------------------


class _StatusAtSteps(RecordingReporter):
    """Reads ``JobRunner.status`` the moment a step is reported — what ``PostMessageReporter.on_done`` does."""

    def __init__(self, ledger: Ledger, job_id: str, steps: tuple[str, ...]) -> None:
        super().__init__()
        self._ledger = ledger
        self._runner = jobs.JobRunner(ledger)
        self._job_id = job_id
        self._steps = steps
        self.seen: dict[str, dict] = {}
        self.results: dict[str, dict | None] = {}

    def progress(self, p) -> None:
        super().progress(p)
        if p.step in self._steps:
            self.seen[p.step] = self._runner.status(self._job_id)["job"]
            self.results[p.step] = self._ledger.job(self._job_id, Role.CONDUCTOR)["result"]


def test_done_is_reported_after_the_result_names_the_target_session(tmp_path, clock):
    """L3-F03: the gateway's final text is built when the reporter hears ``done`` (and ``retiring`` before it);
    the job's status must then already carry the target's ack — session and model (lab: "session ?, model
    default; works offline: unknown")."""
    h = Harness(tmp_path, clock)
    rep = _StatusAtSteps(h.ledger, h.job_id, ("retiring", "done"))
    h.reporter = rep
    _run(h)
    assert h.state() == "done"
    for step in ("retiring", "done"):
        job = rep.seen[step]
        assert job["ack"] is not None, (step, job["state"], job["outcome"])
        assert (job["ack"]["session_id"], job["ack"]["model"]) == ("new-tip", "m"), step
    assert rep.seen["done"]["outcome"] == "done" and rep.seen["done"]["exit_code"] == 0
    assert (rep.results["done"] or {}).get("ack", {}).get("session_id") == "new-tip", rep.results["done"]


def test_aborted_is_reported_with_its_result(tmp_path, clock):
    """V42, abort side: when ``aborted`` is reported the job's result is recorded already (same transaction as the
    state), not only derived from the state by the status."""
    h = Harness(tmp_path, clock)
    h.world.inject("target", "negotiate", "error", error=InvalidState("no", reason="state"))
    rep = _StatusAtSteps(h.ledger, h.job_id, ("aborted",))
    h.reporter = rep
    c = h.conductor()
    with pytest.raises(HandoverError):
        c.run_until_negotiated()
    assert h.state() == "aborted"
    assert rep.seen["aborted"]["outcome"] == "aborted"
    assert (rep.results["aborted"] or {}).get("outcome") == "aborted", rep.results


# ---------------------------------------------------------------------------------------------------------
# V43 — participant jobs in the status
# ---------------------------------------------------------------------------------------------------------


def _inbound(led: Ledger, name: str = "laptop") -> Peer:
    peer = Peer(name=name, peer_id=ulid(), direction="inbound", url=None, pubkey="pk-" + name, rights=frozenset(Right))
    led.upsert_peer(peer)
    return peer


def _ack(session_id: str) -> CommitAck:
    return CommitAck(
        session_id=session_id,
        root_id="root-" + session_id,
        title="Arrived",
        dests=(("w0", "/srv/recv/proj", DestMode.FRESH),),
        path_map=(),
        model="fake-model",
        offline_capable=False,
        warnings=(),
        committed_at=1.0,
    )


@pytest.fixture
def led(tmp_path, clock):
    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    yield ledger
    ledger.close()


def test_status_of_a_push_arrival_on_the_target(led):
    """L3-F04: on the receiving side of a push (target role, the conductor is the other gateway) ``status <job>``
    shows the job with the target session, model and the note the agent gets; the overview lists the arrival."""
    peer = _inbound(led)
    job_id = ulid()
    led.create_job(job_id, Role.TARGET, Mode.PUSH, peer.peer_id, {"role": "target", "mode": "push"})
    ack = _ack("20261109_095017_7c4fc4")
    led.transition(job_id, Role.TARGET, "open", "committing", {}, epoch=0)
    led.transition(job_id, Role.TARGET, "committing", "committed", {"ctx": {"ack": to_wire(ack)}}, epoch=0)
    led.arrival_put(
        session_ids=[ack.session_id],
        root_id=ack.root_id,
        job_id=job_id,
        peer=peer.name,
        note="<ctx-handover-note>moved</ctx-handover-note>",
        report_path="",
        path_map=[],
        continue_prompt=None,
    )
    runner = jobs.JobRunner(led)
    job = runner.status(job_id)["job"]
    assert (job["job_id"], job["role"], job["mode"], job["state"]) == (job_id, "target", "push", "committed")
    assert job["peer"] == "laptop" and job["active"] is False
    assert job["ack"]["session_id"] == ack.session_id and job["ack"]["model"] == "fake-model"
    assert job["note"] == "<ctx-handover-note>moved</ctx-handover-note>"
    assert job["calling_session"] == ack.session_id and job["origin"]
    listed = [(j["job_id"], j["role"]) for j in runner.status(None)["jobs"]]
    assert (job_id, "target") in listed, listed


def test_status_of_a_pull_on_its_source(led):
    """L3-F04 (pull): the source of a pull (the conductor dialed in) answers ``status <job>`` with role ``source`` and
    the state of its handover; the overview lists it."""
    peer = _inbound(led)
    job_id = ulid()
    led.create_job(job_id, Role.SOURCE, Mode.PULL, peer.peer_id, {"role": "source", "mode": "pull"})
    info = SessionInfo("tip-1", "root-1", ("root-1", "tip-1"), "T", None, None, None, None, None, 2, (1,))
    led.source_lock(job_id, info, "holder", peer.name)
    led.source_set(job_id, "retired")
    runner = jobs.JobRunner(led)
    job = runner.status(job_id)["job"]
    assert (job["role"], job["mode"], job["state"], job["peer"]) == ("source", "pull", "retired", "laptop")
    assert job["calling_session"] == "tip-1" and job["title"] == "T"
    assert (job_id, "source") in [(j["job_id"], j["role"]) for j in runner.status(None)["jobs"]]


def test_unknown_job_is_still_not_found(led):
    from ctx_handover.engine.errors import NotFound

    with pytest.raises(NotFound):
        jobs.JobRunner(led).status(ulid())


def test_a_local_job_is_listed_once_as_the_conductor(homes):
    """A job this host conducts with a local participant (export: local source) is one entry of the overview — the
    conductor's — not a second ``source`` entry."""
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Once")
    out = laptop.root / "out" / "once.ctxh.tar"
    out.parent.mkdir(parents=True)
    job_id, rc = laptop.run(Mode.EXPORT, None, SessionRef(id=sid), options=OFF, bundle_path=out)
    assert rc == 0, errors(laptop, job_id)
    with laptop.active():
        listed = [(j["job_id"], j["role"]) for j in jobs.runner_for(None).status(None)["jobs"]]
    assert listed.count((job_id, "conductor")) == 1 and (job_id, "source") not in listed, listed


def test_push_arrival_is_visible_on_the_receiving_side(homes):
    """L3-F04 end to end: after a push laptop → server, the server's ``status <job>`` names the arrived session
    and the note; its overview lists the arrival."""
    server, laptop = homes
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Arriving")
    with paired(server, laptop) as (_listener, peer):
        push_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=OFF)
        assert rc == 0, (state(laptop, push_id), errors(laptop, push_id))
    arrived = next(r for r in server.ledger().arrival_rows() if r["job_id"] == push_id)
    with server.active():
        runner = jobs.runner_for(None)
        job = runner.status(push_id)["job"]
        listed = [(j["job_id"], j["role"]) for j in runner.status(None)["jobs"]]
    assert (job["role"], job["mode"], job["state"]) == ("target", "push", "committed"), job
    assert job["ack"]["session_id"] == arrived["session_id"] and job["note"] == arrived["note"] and job["note"]
    assert (push_id, "target") in listed, listed


# ---------------------------------------------------------------------------------------------------------
# V40 — an unusable workspace root on the target ends the job before anything moves
# ---------------------------------------------------------------------------------------------------------


def _unusable(name: str = "laptop") -> EnvFacts:
    facts = make_facts(name)
    if not hasattr(facts, "workspace_root_ok"):  # pragma: no cover - K0's EnvFacts field not there yet
        pytest.fail("EnvFacts.workspace_root_ok missing (K0 contract)")
    return dataclasses.replace(facts, workspace_root_ok=False, workspace_root_problem="denied")


def test_unusable_target_workspace_root_ends_the_job_before_the_lock(tmp_path, clock):
    """R29 (L3-F01): the target reports in its facts that its workspace root cannot be written (inside the Hermes
    home). The job ends ABORTED in planning — no lock, no export, no block moved —, the error names the setting
    ``workspace_root`` and the source is untouched (lab: the failure came at commit, after lock and prepare)."""
    h = Harness(tmp_path, clock)
    tgt = h.world.target
    tgt.facts = lambda job, sf: tgt._call("facts", job, _unusable)
    c = h.conductor()
    with pytest.raises(HandoverError) as raised:
        c.run_until_negotiated()
    assert h.state() == "aborted", h.states()
    assert "lock" not in h.calls("source") and "start_export" not in h.calls("source")
    assert "negotiate" not in h.calls("target")
    err = _row(h)["last_error"]
    assert err["code"] == "policy_denied" and err["details"]["setting"] == "workspace_root", err
    assert "workspace_root" in raised.value.message
    h.world.assert_outcome("aborted")


def test_export_is_not_stopped_by_the_workspace_root_flag_of_its_facts(tmp_path, clock):
    """R29 applies to a receiving host: an export writes a bundle (the importer checks its own root), so facts
    carrying ``workspace_root_ok=False`` (copied from the source by the generic importer facts) do not stop it."""
    h = Harness(tmp_path, clock, mode=Mode.EXPORT)
    tgt = h.world.target
    tgt.facts = lambda job, sf: tgt._call("facts", job, _unusable)
    _run(h)
    assert h.state() == "done", (h.states(), _row(h)["last_error"])


def test_push_into_a_docker_layout_lands_under_handover_workspaces(homes):
    """L3-F01 end to end (R29): the receiving home has HOME = HERMES_HOME (the official Docker image) and default
    settings. The push ends DONE and the arrived workspace and session cwd lie under
    ``<HERMES_HOME>/handover-workspaces`` (lab: ``c2:cwd`` ``target_denied`` for ``~/hermes-handover``, job aborted
    after lock and prepare)."""
    server, laptop = homes
    server.extra_env.update({"HOME": str(server.hermes_home), "USERPROFILE": str(server.hermes_home)})
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Into docker")
    with paired(server, laptop) as (_listener, peer):
        push_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=OFF)
        assert (state(laptop, push_id), rc) == ("done", 0), errors(laptop, push_id)
    arrived = next(r for r in server.ledger().arrival_rows() if r["job_id"] == push_id)
    cwd = (server.db().get_session(arrived["session_id"]) or {}).get("cwd")
    root = server.hermes_home / "handover-workspaces"
    assert cwd and Path(cwd).is_relative_to(root) and Path(cwd, "src", "main.py").is_file(), cwd


def test_push_to_an_unusable_workspace_root_ends_before_the_lock(homes):
    """R29/V40 end to end: the receiving home names a workspace root inside its Hermes home explicitly (only the
    reserved ``handover-workspaces`` is writable there). The push ends ABORTED at the facts — the laptop's session
    was never locked, nothing was staged — and the error names ``workspace_root``."""
    server, laptop = homes
    inside = str(server.hermes_home / "elsewhere")
    server.write_settings({"refine": "off", "workspace_root": inside, "allow_roots": [inside]})
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Refused root")
    # plan_sync raises after the job ended (nothing locked)
    with paired(server, laptop) as (_listener, peer), pytest.raises(PolicyDenied) as raised:
        laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=OFF)
    assert "workspace_root" in raised.value.message
    push_id = laptop.ledger().jobs(role=Role.CONDUCTOR, limit=1)[0]["job_id"]
    assert state(laptop, push_id) == "aborted", errors(laptop, push_id)
    err = laptop.ledger().job(push_id, Role.CONDUCTOR)["last_error"]
    assert err["code"] == "policy_denied" and "workspace_root" in err["message"], err
    states = [e["state"] for e in laptop.ledger().events(push_id, Role.CONDUCTOR) if e["kind"] == "transition"]
    assert "locking" not in states and laptop.ledger().source_state(push_id) is None, states
    marks = session_marks(laptop.db(), sid)
    assert not marks["archived"] and not marks["hidden"] and marks["end_reason"] is None, marks


def _docker_policy(tmp_path) -> PolicyConfig:
    """A target policy of the official Docker image: HOME = HERMES_HOME = Hermes root."""
    hh = tmp_path / "opt-data"
    hh.mkdir()
    return PolicyConfig.from_options(
        Options(),
        home=hh,
        hermes_home=hh,
        hermes_root=hh,
        plugin_dir=hh / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
    )


def test_fallback_session_cwd_resolves_the_default_root_like_the_policy(tmp_path):
    """R29 (A part): a session without a primary workspace gets the handover root as its cwd on the target; with
    HOME = HERMES_HOME that is ``<HERMES_HOME>/handover-workspaces`` — the root the policy lets the commit write — not
    ``~/hermes-handover`` inside the Hermes home (lab: ``c2:cwd`` ``target_denied``)."""
    from ctx_handover.engine.local_target import LocalTarget
    from ctx_handover.engine.policy import check_target_dest

    cfg = _docker_policy(tmp_path)
    stub = type("T", (), {"policy": cfg})()
    root = LocalTarget._workspace_root(stub, Options().workspace_root)
    assert root == str(cfg.hermes_home / "handover-workspaces")
    assert check_target_dest(Path(root), cfg, recorded=False, local_choice=True).allowed
    stub.policy = None  # stand-ins without a policy: plain expansion
    assert LocalTarget._workspace_root(stub, "~/x") == str(Path.home() / "x")


def test_default_bundle_path_uses_the_same_root_rule(tmp_path):
    """R29: ``export`` without a path writes under the resolved default root (HOME = HERMES_HOME ⇒
    ``<HERMES_HOME>/handover-workspaces``); without the homes the plain expansion (unchanged callers)."""
    hh = tmp_path / "opt-data"
    at = 1_762_000_000.0
    path = jobs.default_bundle_path("T", Options(), at, home=hh, hermes_home=hh, hermes_root=hh)
    assert path.parent == hh / "handover-workspaces" and path.name.endswith(".ctxh.tar"), path
    other = tmp_path / "home"
    path = jobs.default_bundle_path("T", Options(), at, home=other, hermes_home=hh, hermes_root=hh)
    assert path.parent == other / "hermes-handover", path


def test_source_settings_go_through_the_one_normalizer():
    """Hand-over V22: ``local_source`` reads its settings through ``settings.from_config`` — every schema key,
    YAML words by the one rule (``None`` ⇒ default), unknown keys dropped — not the raw mapping."""
    from ctx_handover.engine import settings
    from ctx_handover.engine.local_source import _settings_of

    config = {"plugins": {"entries": {"ctx_handover": {"settings": {"ctx_persist": "no", "refine": None, "x": 1}}}}}
    got = _settings_of(config)
    assert got == settings.from_config(config)
    assert got["ctx_persist"] == "off" and got["refine"] == settings.DEFAULTS["refine"] and "x" not in got
    assert _settings_of({}) == dict(settings.DEFAULTS)


def test_fallback_destination_resolves_with_this_hosts_policy(tmp_path):
    """Hand-over (V35/R29): ``LocalTarget``'s generic fallback goes through ``pathmap.fallback_dest`` with this
    host's policy (``cfg``) — the result lies under the root the policy allows (Docker:
    ``<HERMES_HOME>/handover-workspaces``), not under the home of the process that runs the code."""
    from types import SimpleNamespace

    from ctx_handover.engine import pathmap
    from ctx_handover.engine.contracts import JobContext
    from ctx_handover.engine.local_target import LocalTarget
    from ctx_handover.engine.policy import check_target_dest
    from ctx_handover.tests.fakes.apis_fake import make_item

    cfg = _docker_policy(tmp_path)
    stub = SimpleNamespace(policy=cfg, ports=SimpleNamespace(pathmap=pathmap))
    job = JobContext(
        job_id=ulid(),
        mode=Mode.PUSH,
        peer=None,
        options=Options(),
        epoch=1,
        reporter=None,
        cancelled=lambda: False,
        created_at=0.0,
    )
    fallback = LocalTarget._fallback_for(stub, job, make_item("w0", "/srv/app"), None, None, datetime(2026, 11, 9))
    dest = fallback("/srv/elsewhere/app")
    assert Path(dest).is_relative_to(cfg.hermes_home / "handover-workspaces"), dest
    assert check_target_dest(Path(dest), cfg, recorded=False, local_choice=True).allowed


def test_facts_are_probed_with_this_hosts_policy(env):
    """R29: ``LocalTarget.facts`` hands its own policy to ``envfacts.probe`` (``cfg``), so the write-root
    check sees the Hermes root and denylist this host enforces at commit time."""
    seen: dict = {}
    real = env.target.ports.envfacts.probe

    def probe(**kw):
        seen.update(kw)
        return real(**kw)

    env.target.ports.envfacts.probe = probe
    env.target.open_job(env.job)
    env.target.facts(env.job, make_facts("server"))
    assert seen.get("cfg") is env.target.policy is not None


def test_source_facts_are_probed_with_this_hosts_policy(mk):
    """R29, source side: ``LocalSource.facts`` passes its policy to ``envfacts.probe`` as well."""
    e = mk()
    seen: dict = {}
    real = e.source.ports.envfacts.probe

    def probe(**kw):
        seen.update(kw)
        return real(**kw)

    e.source.ports.envfacts.probe = probe
    e.source.facts(e.job)
    assert seen.get("cfg") is e.source.policy is not None


def test_yes_does_not_confirm_a_left_out_working_directory(tmp_path, clock):
    """R30/V41 (hand-over): with ``--yes`` the user is still asked for a reason ``plan.yes_exempt`` names
    (``workspace_excluded``); without an answer the job ends ABORTED before the lock, the source untouched; an
    explicit ``yes`` goes ahead."""
    from ctx_handover.engine import plan as planning
    from ctx_handover.engine.errors import Cancelled

    for answer in (None, "yes"):
        h = Harness(tmp_path, clock, options=Options(yes=True))
        h.planning.confirmation_reasons = lambda *a, **kw: [planning.CONFIRM_WORKSPACE_EXCLUDED]
        asked: list[dict] = []

        def confirm(info, asked=asked, answer=answer):
            asked.append(info)
            return answer

        c = h.conductor(confirm=confirm)
        if answer is None:
            with pytest.raises(Cancelled):
                c.run_until_negotiated()
            assert h.state() == "aborted" and "lock" not in h.calls("source"), h.states()
            h.world.assert_outcome("aborted")
        else:
            c.run_until_negotiated()
            c.run()
            assert h.state() == "done", h.states()
        assert len(asked) == 1 and asked[0]["yes_exempt"] == [planning.CONFIRM_WORKSPACE_EXCLUDED], asked


def test_a_forward_commit_step_that_needs_attention_keeps_the_staging(env):
    """Hand-over (DESIGN §3.4): a forward commit step fails with ``details.needs_attention`` (E: it could not
    undo its own partial change, e.g. an orphaned ``index.lock``). It is booked like a failing inverse — a
    ``needs_attention`` event, listed in ``last_error.needs_attention`` — and the rollback keeps the staging and the
    hidden session for a person instead of cleaning up as if everything were rolled back."""
    from ctx_handover.engine.errors import ConflictError
    from ctx_handover.engine.model import OpState
    from ctx_handover.tests.unit.A.standins import WorkspaceTxn

    prepared = env.prepared()
    WorkspaceTxn.fail["c3:hermes_files"] = ConflictError(
        "index.lock left behind", needs_attention=True, kind="index_lock"
    )
    st = env.wait(env.target.start_commit(env.job))
    assert st.state is OpState.FAILED and st.error["code"] == "import_failed", st.error
    row = env.ledger.job(env.job.job_id, Role.TARGET)
    assert row["state"] == "aborted"
    assert [a["step"] for a in row["last_error"]["needs_attention"]] == ["c3:hermes_files"], row["last_error"]
    events = [e["data"] for e in env.ledger.events(env.job.job_id, Role.TARGET) if e["kind"] == "needs_attention"]
    assert [e["step"] for e in events] == ["c3:hermes_files"], events
    assert (env.data / "staging" / env.job.job_id).is_dir(), "staging removed although the step needs a person"
    assert env.db.get_session(prepared.tip_id) is not None and env.hidden(prepared.tip_id)


# ---------------------------------------------------------------------------------------------------------
# N55 — the source title of an export; N05 — paused drivers in the note
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("for_peer", [False, True], ids=["plain", "for-peer"])
def test_export_titles_the_source_as_exported_to_a_bundle(homes, for_peer):
    """N55: an export marks the source title "exported to bundle <time>" — not "handed over to  <time>" with an
    empty peer name, and not "handed over to <peer>" for ``export --for <peer>`` (that only picks the facts)."""
    _server, laptop = homes
    laptop.identity()
    peer = None
    if for_peer:
        peer = Peer(
            name="far",
            peer_id=ulid(),
            direction="dial",
            url="https://far.example",
            pubkey="pk-far",
            rights=frozenset(Right),
        )
        laptop.ledger().upsert_peer(peer)
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Exported work")
    out = laptop.root / "out" / "title.ctxh.tar"
    out.parent.mkdir(parents=True)
    job_id, rc = laptop.run(Mode.EXPORT, peer, SessionRef(id=sid), options=OFF, bundle_path=out)
    assert rc == 0, errors(laptop, job_id)
    title = laptop.db().get_session_title(sid)
    assert title.startswith("Exported work (exported to bundle ") and title.endswith(")"), title
    assert "handed over to" not in title, title


def test_an_active_goal_arrives_paused_and_the_note_says_so(homes):
    """N05 (R22): the source session has an active goal; the import writes it paused under the arrived session and
    the note the agent gets says so ("goal paused (/goal resume)"). The arrived title is the original one (R14: the
    source's export mark does not travel)."""
    server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Goal work")
    laptop.db().set_meta(f"goal:{sid}", json.dumps({"goal": "ship it", "status": "active", "max_turns": 10}))
    out = laptop.root / "out" / "goal.ctxh.tar"
    out.parent.mkdir(parents=True)
    job_id, rc = laptop.run(Mode.EXPORT, None, SessionRef(id=sid), options=OFF, bundle_path=out)
    assert rc == 0, errors(laptop, job_id)
    import_options = dataclasses.replace(OFF, trust_unsigned=True)
    imp_id, rc = server.run(Mode.IMPORT, None, SessionRef(id=""), options=import_options, bundle_path=out)
    assert rc == 0, errors(server, imp_id)
    arrived = next(r for r in server.ledger().arrival_rows() if r["job_id"] == imp_id)
    goal = json.loads(server.db().get_meta(f"goal:{arrived['session_id']}"))
    assert goal["status"] == "paused", goal
    assert "goal paused (/goal resume)" in note_of(server, imp_id), note_of(server, imp_id)
    assert server.db().get_session_title(arrived["session_id"]) == "Goal work"


# ---------------------------------------------------------------------------------------------------------
# V38 speed-up (wish) — the row of a dead process on this host is taken over at once
# ---------------------------------------------------------------------------------------------------------


def _dead_pid() -> int:
    proc = subprocess.Popen([sys.executable, "-c", "pass"])
    proc.wait()
    return proc.pid


def test_a_row_of_a_dead_process_on_this_host_is_taken_over_at_once(led):
    """V38 (wish, DESIGN §7.2 step 2): owners carry this host's boot id and PID namespace; a restarted
    participant (or janitor) takes over the row of a predecessor whose pid is gone at once instead of after the
    60-s stale window. A live owner, an unstamped owner and one of another namespace still hold it."""
    from ctx_handover.engine import ledger as ledger_mod

    stamp = ledger_mod._ns_stamp()
    assert stamp, "this suite runs on Linux: /proc/sys/kernel/random/boot_id and /proc/self/ns/pid"
    assert ledger_mod.process_owner().endswith("@" + stamp)
    job_id = ulid()
    led.create_job(job_id, Role.TARGET, Mode.PUSH, None, {})
    dead = f"{_dead_pid()}:0badc0de@{stamp}"
    assert led.claim_owner(job_id, Role.TARGET, dead, 60) == 1
    assert led.claim_owner(job_id, Role.TARGET, ledger_mod.process_owner(), 60) == 2  # no wait for the window
    for holder in (
        f"{os.getppid()}:0badc0de@{stamp}",  # alive
        f"{_dead_pid()}:0badc0de",  # unstamped
        f"{_dead_pid()}:0badc0de@{stamp.rsplit('.', 1)[0]}.1",  # another PID namespace
    ):
        other = ulid()
        led.create_job(other, Role.TARGET, Mode.PUSH, None, {})
        assert led.claim_owner(other, Role.TARGET, holder, 60) == 1
        assert led.claim_owner(other, Role.TARGET, "next-owner", 60) is None, holder
    assert led.claim_owner(job_id, Role.TARGET, ledger_mod.process_owner() + "/c0ffee", 60) is None  # own pid: alive


# ---------------------------------------------------------------------------------------------------------
# hand-overs of layer G
# ---------------------------------------------------------------------------------------------------------


def test_offers_are_valid_for_two_hours():
    """DESIGN §12.6 / README: an offer stays valid for two hours (wish)."""
    assert jobs.OFFER_TTL_S == 2 * 3600
