"""First review round, Medium findings of the conductor (the first external review §2 and its
triage): M1, M2, M3, M18, M27 — each reproduced with the A harness (real ledger,
World of fake participants) before the fix."""

from __future__ import annotations

import threading
import time
from dataclasses import replace

import pytest

from ctx_handover.engine import plan as planning
from ctx_handover.engine.conductor import Conductor, PollPolicy
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.errors import ListenerMissing, PeerUnreachable
from ctx_handover.engine.model import FinalPlan, Include, Mode, Role
from ctx_handover.tests.fakes.apis_fake import make_item
from ctx_handover.tests.unit.A.harness import FAST, Harness

# -- M27: no continuation when the source took the session back during the retire -------------------------------


def test_rv_a_m27_reclaimed_source_never_continues_on_the_target(tmp_path, clock):
    """``/handover to server --continue …`` and ``reclaim --force`` on the source while the target commits: the
    retire reports ``reclaimed`` — two active copies must never get an unattended turn (R10)."""
    h = Harness(tmp_path, clock, continue_prompt="run the tests, commit and push")
    h.world.inject("source", "start_retire", "action", action=lambda world: world.source.reclaim(force=True))
    c = h.conductor()
    c.run_until_negotiated()
    c.run()

    row = h.ledger.job(h.job_id, Role.CONDUCTOR)
    assert row["state"] == "done"
    assert h.world.target.continued == []
    cont = row["result"].get("continue") or {}
    assert cont.get("started") is False
    assert cont["error"]["details"]["reason"] == "source_given_back"
    assert any("no turn was started" in w for w in row["result"].get("warnings") or [])


def test_rv_a_m27_confirmed_retire_still_continues(tmp_path, clock):
    """Counter-check: an explicitly retired source lets the continuation start as before."""
    h = Harness(tmp_path, clock, continue_prompt="go on")
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == "done"
    assert h.world.target.continued == ["go on"]


# -- M18: re-plan matches confirmed proposals by what they are, not by their position ----------------------------


def test_rv_a_m18_replan_keeps_the_confirmed_proposal_not_the_one_that_took_its_id(tmp_path, clock):
    """``plan._assign_ids`` numbers extras by position (``x0, x1 …``): a file mentioned after the confirmation can take
    the id of the proposal the user said yes to. The re-plan must keep that proposal and leave the new file out."""
    h = Harness(tmp_path, clock)
    notes, diary = "/home/u/notes/notes.txt", "/home/u/notes/a-private-diary.txt"
    h.world.source.items = [make_item("w0", "/srv/app"), make_item("x0", notes, include=Include.PROPOSED)]

    def confirm(info):
        # while the user reads the summary the session moves on and mentions another file; the deterministic
        # re-plan assigns the positional ids anew (what the real ``build_plan`` does, see below)
        h.world.source.append_rows("cat ~/notes/a-private-diary.txt")
        h.world.source.items = [
            make_item("w0", "/srv/app"),
            make_item("x0", diary, include=Include.PROPOSED),
            make_item("x1", notes, include=Include.PROPOSED),
        ]
        return "yes"

    c = h.conductor(confirm=confirm)
    c.run_until_negotiated()
    c.run()

    assert h.state() == "done"
    final = c._get("final_plan", FinalPlan)
    taken = {i.source_path for i in final.plan.items if i.include is Include.YES}
    assert notes in taken, "the proposal the user confirmed fell out"
    assert diary not in taken, "a file the user never saw went with 'yes'"


def test_rv_a_m18_build_plan_ids_are_positional(tmp_path):
    """The premise of the test above, on the real planner: an extra mentioned first takes ``x0``."""
    from ctx_handover.engine.model import Candidate, ItemKind, Options, Origin, PlanHints, RefineReport, Weight
    from ctx_handover.engine.policy import PolicyConfig
    from ctx_handover.tests.fakes.planning_fake import Busy

    home = tmp_path / "u"
    (home / ".hermes").mkdir(parents=True)
    (home / "proj").mkdir()
    (home / "notes").mkdir()
    for name in ("notes.txt", "a-private-diary.txt"):
        (home / "notes" / name).write_text("x\n")
    cfg = PolicyConfig.from_options(
        Options(),
        home=home,
        hermes_home=home / ".hermes",
        hermes_root=home / ".hermes",
        plugin_dir=home / ".hermes" / "pd",
        case_insensitive=False,
        target_os="linux",
    )

    def build(paths):
        cands = [Candidate(ItemKind.FILE_TREE, str(home / "proj"), Weight.MENTION, ("cwd",), Origin.CWD)]
        cands += [Candidate(ItemKind.EXTRA_FILE, str(p), Weight.MENTION, ("m",), Origin.TRANSCRIPT) for p in paths]
        args = (None, RefineReport(used="off"), PlanHints(), cfg, lambda r: None, Busy(), (), Options(), 10)
        return planning.build_plan("01JCJOB", (1,), cands, *args, redact=lambda s: s)

    old = build([home / "notes" / "notes.txt"])
    new = build([home / "notes" / "a-private-diary.txt", home / "notes" / "notes.txt"])
    ids = {i.item_id: i.source_path for i in new.items}
    assert {i.item_id for i in old.items if i.source_path.endswith("notes.txt")} & set(ids)
    assert ids["x0"].endswith("a-private-diary.txt")


# -- M1: resume after a re-plan packs the new plan ----------------------------------------------------------------


def test_rv_a_m1_resume_after_a_replan_packs_the_new_plan(tmp_path, clock):
    h = Harness(tmp_path, clock)
    h.world.inject("target", "start_prepare", "disconnect", times=10_000)
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == "suspended"
    h.world.faults.clear()
    h.world.source.items.append(make_item("x3", "/srv/app/new.txt"))
    h.world.source.append_rows("the user typed on while the laptop was away")
    h.stale()

    c2 = h.conductor()
    c2.resume()

    assert h.state() == "done", h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]
    packs = [m for m in h.calls("source") if m == "start_pack"]
    assert len(packs) >= 2, "the re-planned job never packed its new plan"


# -- M2: an abort never waits for a side that never saw the job ---------------------------------------------------


@pytest.mark.parametrize("err", [PeerUnreachable("server is offline"), ListenerMissing("no ctx_handover routes")])
def test_rv_a_m2_abort_of_a_job_the_source_never_saw_ends(tmp_path, clock, err):
    h = Harness(tmp_path, clock, mode=Mode.PULL)
    for method in ("hello", "facts", "start_release"):
        h.world.inject("source", method, "error", error=err, times=10_000)
    c = h.conductor()
    with pytest.raises(Exception):  # noqa: B017 - the hello error itself
        c.run_until_negotiated()
    if h.state() != "aborted":
        h.stale()
        h.conductor().recover()
    assert h.state() == "aborted"
    intents = [e["data"].get("step") for e in h.events("intent")]
    assert "source.lock" not in intents and "target.open_job" not in intents


def test_rv_a_m2_a_locked_source_is_still_waited_for(tmp_path, clock):
    """Counter-check (V37): once ``source.lock`` is journaled the release must be confirmed — an unreachable source
    keeps the job ``aborting`` for the janitor instead of ending it as given back."""
    h = Harness(tmp_path, clock, mode=Mode.PULL)
    h.world.inject("target", "start_prepare", "error", error=ListenerMissing("gone"), times=10_000)
    h.world.inject("source", "start_release", "error", error=PeerUnreachable("server is offline"), times=10_000)
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert "source.lock" in [e["data"].get("step") for e in h.events("intent")]
    assert h.state() != "aborted"


# -- M3: the confirmation keeps the job's owner heartbeat ---------------------------------------------------------


def _live_conductor(h: Harness, confirm) -> Conductor:
    """``Harness.conductor`` with the keepalive thread on (the harness switches it off for the fake clock)."""
    owner = "owner-live"
    epoch = h.ledger.claim_owner(h.job_id, Role.CONDUCTOR, owner, 60)
    job = JobContext(
        job_id=h.job_id,
        mode=h.mode,
        peer=None,
        options=h.options,
        epoch=epoch,
        reporter=h.reporter,
        cancelled=lambda: h.ledger.cancel_requested(h.job_id),
        created_at=h.clock(),
    )
    poll: PollPolicy = replace(FAST, keepalive_s=0.01, owner_heartbeat_s=5.0)
    return Conductor(
        h.ledger,
        job,
        h.world.source,
        h.world.target,
        confirm=confirm,
        continue_prompt=None,
        planning=h.planning,
        owner=owner,
        clock=h.clock,
        sleep=h.world.sleep,
        poll=poll,
        keepalive=True,
    )


def test_rv_a_m3_a_slow_confirmation_keeps_the_job(tmp_path, clock):
    """The user reads the summary for 70 s (gateway clarify timeout 90 s, the CLI has none) while another process
    starts: its janitor must not find the job stale and adopt it."""
    h = Harness(tmp_path, clock)
    h.world.source.items.append(make_item("x2", "/srv/maybe", include=Include.PROPOSED))
    seen: dict = {}
    lock = threading.Lock()

    def beat_seen() -> bool:
        with lock:
            hb = h.ledger.job(h.job_id, Role.CONDUCTOR)["owner_heartbeat_at"]
        return hb is not None and hb >= clock() - 5.0

    def confirm(info):
        for _ in range(7):
            clock.advance(10)
            deadline = time.monotonic() + 1.0
            while not beat_seen() and time.monotonic() < deadline:
                time.sleep(0.01)
        seen["stale"] = [r["role"] for r in h.ledger.stale_jobs(60)]
        seen["adopted"] = h.ledger.claim_owner(h.job_id, Role.CONDUCTOR, "other-process", 60)
        return "yes"

    c = _live_conductor(h, confirm)
    final = c.run_until_negotiated()

    assert seen["stale"] == [] and seen["adopted"] is None, seen
    assert final.plan.items and h.state() != "aborted"


# -- H15: reason (k) ``repos_left`` is settled by the export -------------------------------------------------------


def test_rv_a_import_does_not_ask_repos_left_again(tmp_path, clock, monkeypatch):
    """The first review round added reason (k) ``repos_left`` (repositories inside a transferred git workspace that do
    not travel, H15); ``--yes`` does not answer it (``plan.YES_EXEMPT``), so the export asked a person. The bundle holds
    that answer: the import does not ask (k) again (like ``proposed`` and ``workspace_differs``, R36a). The bundle's
    plan of record is the World's plan here (``_bundle_final`` stands in for reading it from a real bundle)."""
    from ctx_handover.engine import conductor as conductor_mod
    from ctx_handover.engine.jsonutil import from_wire
    from ctx_handover.engine.model import EnvFacts, Options, ResourcePlan

    def bundle_final(self):
        st = self.source.start_plan(self.job, self._get("target_facts", EnvFacts), self._session_ref)
        plan = from_wire(ResourcePlan, self._await(self.source, st))
        return FinalPlan(plan_id="b", plan=plan, decisions=(), need_bytes=0, free_bytes={}, model_change=None)

    monkeypatch.setattr(conductor_mod.Conductor, "_bundle_final", bundle_final)
    h = Harness(tmp_path, clock, mode=Mode.IMPORT, options=Options(yes=True))
    h.world.source.verified = True
    real = h.planning.confirmation_reasons
    h.planning.confirmation_reasons = lambda *a, **kw: [*real(*a, **kw), planning.CONFIRM_REPOS_LEFT]
    asked: list = []

    def confirm(info):
        asked.append(info)
        return "cancel"

    c = h.conductor(confirm=confirm)
    final = c.run_until_negotiated()
    assert final is not None and asked == []
