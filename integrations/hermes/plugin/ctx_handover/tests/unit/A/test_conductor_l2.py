"""Conductor, layer 2 (contract X02, X06, X16, K-1; W089, W111, W112 and the pre-send summary finding of L2-F).

Fault-injection world of ``apis_fake`` with ``FakePlanning`` delegating to the real ``plan`` module (``apply_choice``,
``plan_id``, ``presend_summary``, ``note_parts_from``, ``instruction_files``); export → import through the real
``BundleTarget``/``BundleSource``/``LocalSource``/``LocalTarget`` composed by ``jobs``.
"""

from __future__ import annotations

import dataclasses
from pathlib import Path

import pytest

from ctx_handover.engine import jobs, note
from ctx_handover.engine import plan as planning
from ctx_handover.engine.conductor import VerifiedBlocks
from ctx_handover.engine.errors import BadRequest, BlockCorrupt, Cancelled, HandoverError, InvalidState, VerifyFailed
from ctx_handover.engine.model import EnvFacts, Include, Mode, Options, Role, SessionRef
from ctx_handover.engine.progress import TERMINAL_STEPS
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.apis_fake import FakeBlocks, World, make_item
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.fakes.sessiondb import SessionBuilder
from ctx_handover.tests.unit.A.harness import Harness


def _ctx(h: Harness) -> dict:
    out: dict = {}
    for e in h.events():
        if isinstance((e["data"] or {}).get("ctx"), dict):
            out.update(e["data"]["ctx"])
    return out


def _proposals(h: Harness) -> None:
    h.world.source.items.append(make_item("x2", "/srv/maybe.txt", include=Include.PROPOSED))
    secret = make_item("x3", "/srv/token.txt", include=Include.PROPOSED)
    h.world.source.items.append(dataclasses.replace(secret, reasons=(planning.SUSPECTED_SECRET,)))


@pytest.mark.parametrize("how", ["option", "answer"])
def test_l2_conductor_yes_takes_proposals_but_never_suspected_secrets(tmp_path, clock, how):
    """★ X06/R25: ``--yes`` and the answer ``yes`` both run ``plan.apply_choice("yes")``: an unsuspicious proposal
    is packed and arrives, a suspected secret is not; the target holds exactly the confirmed plan the source
    packed (re-negotiated when the choice changed the plan).

    Mutant: ``Conductor._confirm`` keeps the negotiated plan (no ``_settle``/``apply_choice``)."""
    h = Harness(tmp_path, clock, options=Options(yes=how == "option"))
    _proposals(h)
    c = h.conductor(confirm=(lambda info: "yes") if how == "answer" else None)
    final = c.run_until_negotiated()
    c.run()
    h.world.assert_outcome("done")
    includes = {i.item_id: i.include for i in final.plan.items}
    assert includes["x2"] is Include.YES and includes["x3"] is Include.NO
    assert "x2" in h.world.target.pack_items and "x3" not in h.world.target.pack_items
    assert h.world.target.final.plan_id == final.plan_id == h.world.source.packed_plan.plan_id
    assert "apply_choice:yes" in h.planning.calls


def test_l2_conductor_answer_outside_the_four_choices_cancels(tmp_path, clock):
    """W112: ``confirm`` answers ``yes``/``no_proposed``/``show_plan``/``cancel``; anything else ends the job with
    nothing locked (never treated as consent)."""
    h = Harness(tmp_path, clock)
    _proposals(h)
    c = h.conductor(confirm=lambda info: "sure, whatever")
    with pytest.raises(Cancelled) as exc:
        c.run_until_negotiated()
    assert exc.value.details["ended_by"] == "cancel"
    assert h.state() == "aborted" and "lock" not in h.calls("source")


def test_l2_conductor_timeout_with_only_proposals_excludes_them(tmp_path, clock):
    """W089: no answer (timeout) while proposals are the only reason ⇒ ``no_proposed`` through the real
    ``apply_choice``: proposals become ``no``, their decisions ``skip``; nothing of them is packed."""
    h = Harness(tmp_path, clock)
    _proposals(h)
    c = h.conductor(confirm=lambda info: None)
    final = c.run_until_negotiated()
    c.run()
    h.world.assert_outcome("done")
    assert {i.item_id for i in final.plan.items if i.include is Include.PROPOSED} == set()
    assert not {"x2", "x3"} & set(h.world.target.pack_items)
    assert "apply_choice:no_proposed" in h.planning.calls


def test_l2_conductor_presend_summary_is_structured_not_a_text_line(tmp_path, clock):
    """Finding L2-F: the pre-send summary is journaled as ``plan.presend_summary``'s document (ctx ``presend``);
    the conductor sends no ``k=v`` text through the reporter (F's notice shows D's summary only)."""
    h = Harness(tmp_path, clock, options=Options(yes=True))
    _proposals(h)
    c = h.conductor()
    final = c.run_until_negotiated()
    c.run()
    h.world.assert_outcome("done")
    facts = {"target_facts": c._get("target_facts", EnvFacts), "source_facts": c._get("source_facts", EnvFacts)}
    assert _ctx(h)["presend"] == to_wire_dict(planning.presend_summary(final, **facts))
    assert not [t for t in h.reporter.infos if "summary" in t.lower() or "=" in t]


def to_wire_dict(doc: dict) -> dict:
    import json

    return json.loads(json.dumps(doc))


def test_l2_conductor_note_parts_only_what_the_note_renders(tmp_path, clock):
    """X16: ``PrepareRequest.note_parts`` is exactly ``plan.note_parts_from(final, …)`` (with ``pack_time`` and the
    session title) — every key one ``note.compose`` renders; no conductor keys the note does not render (mode,
    deltas, restarts, free-text warnings, the plan's notes)."""
    h = Harness(tmp_path, clock)
    seen = []
    real = h.world.target.start_prepare

    def start_prepare(job, req):
        seen.append(req)
        return real(job, req)

    h.world.target.start_prepare = start_prepare
    c = h.conductor()
    final = c.run_until_negotiated()
    c.run()
    h.world.assert_outcome("done")
    parts = seen[-1].note_parts
    assert note.unknown_note_parts(parts) == ()
    assert parts["source_kept"] is False and parts["pack_time"] > 0 and parts["title"] == "T"
    expected = planning.note_parts_from(final, pack_time=parts["pack_time"], title="T")
    assert parts == to_wire_dict(expected)


@pytest.mark.parametrize("case", ["done", "in_doubt", "retiring", "aborting"])
def test_l2_conductor_last_progress_names_the_stop_state(tmp_path, clock, case):
    """W111: the last ``Progress.step`` a runner reports is the state it stopped in — a stop state, or the state it
    parked in for the janitor; the stop states and in_doubt/retiring are F's terminal steps."""
    h = Harness(tmp_path, clock, options=Options(stall_timeout_s=30))
    if case == "in_doubt":
        h.world.op_polls["commit"] = 10_000

        def cut(world):
            world.inject("target", "op_status", "disconnect", times=100_000)
            world.inject("target", "status", "disconnect", times=100_000)

        h.world.inject("target", "start_commit", "action_after", action=cut)
    elif case == "retiring":
        h.world.inject("source", "start_retire", "error", error=HandoverError("source broken"), times=100)
    elif case == "aborting":
        h.world.inject("source", "lock", "error", error=InvalidState("no", reason="state"))
        h.world.inject("target", "abort", "error", error=HandoverError("target broken"), times=100)
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == case
    last = h.reporter.progress_events[-1].step
    assert last == case
    if case != "aborting":  # parked ABORTING: wish to wp-f (PARKING_STEPS)
        assert last in TERMINAL_STEPS


def test_l2_conductor_replan_keeps_the_confirmed_choice(tmp_path, clock):
    """X06 on the LOCKED re-plan: a proposal the user confirmed with ``yes`` stays confirmed, a proposal that is new
    after the re-plan stays out (no confirmation in the non-interactive part)."""
    h = Harness(tmp_path, clock)
    h.world.source.items.append(make_item("x2", "/srv/maybe.txt", include=Include.PROPOSED))

    def confirm(info):
        h.world.source.append_rows("typed while confirming")  # the revision moves before the lock
        h.world.source.items.append(make_item("x4", "/srv/new.txt", include=Include.PROPOSED))
        return "yes"

    c = h.conductor(confirm=confirm)
    c.run_until_negotiated()
    c.run()
    h.world.assert_outcome("done")
    assert "x2" in h.world.target.pack_items and "x4" not in h.world.target.pack_items
    replanned = [e for e in h.events("info") if e["data"].get("event") == "replanned"]
    assert replanned and replanned[0]["data"]["excluded_proposed"] == ["x4"]


def test_l2_verified_blocks_check_every_manifest(clock):
    """K-1 on the conductor's read path: a manifest whose recomputed id differs from the requested one
    (``BlockCorrupt``), one with another block size (``BadRequest``) or a missing one never passes."""
    w = World(clock)
    good = w.source.blocks_store.put_doc({"kind": "ctxh.capsule", "v": 1, "segments": []})
    other = w.source.blocks_store.put_doc({"kind": "ctxh.capsule", "v": 1, "segments": [], "x": 1})
    view = VerifiedBlocks(w.source.blocks_store)
    assert view.manifests([good.object_id])[0].object_id == good.object_id
    w.source.blocks_store.objects[good.object_id] = other  # the source answers with another object's manifest
    with pytest.raises(BlockCorrupt):
        view.manifests([good.object_id])
    small = FakeBlocks(w, "small", 64)
    m = small.put_object(b"x" * 200)
    with pytest.raises(BadRequest):
        VerifiedBlocks(small).manifests([m.object_id])


def test_l2_conductor_aborts_on_a_forged_manifest(tmp_path, clock):
    """K-1 with V23: a source that answers a document id with a manifest that does not hash to it makes the move fail
    before the commit point (``block_corrupt``): one refetch, then the job ends ABORTED — not suspended, a forged
    object does not get better by waiting. Nothing is active at the target, the source never got the primary lock
    and is released."""
    h = Harness(tmp_path, clock)
    real = h.world.source.blocks_store.manifests

    def forged(ids):
        out = real(ids)
        return [dataclasses.replace(m, size=m.size + 1) if m.media == "application/json" else m for m in out]

    h.world.source.blocks_store.manifests = forged
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == "aborted"
    assert h.ledger.job(h.job_id, Role.CONDUCTOR)["last_error"]["code"] == "block_corrupt"
    assert not h.world.target.active and not h.world.source.primary_lock and h.world.violations == []
    assert h.world.source.state == "released" and not h.world.source.leases
    assert "start_prepare" not in h.calls("target")


# ---------------------------------------------------------------------------------------------------------
# export → import with the real modules (contract X02)
# ---------------------------------------------------------------------------------------------------------


@pytest.fixture
def two_homes(tmp_path):
    require_upstream()
    from ctx_handover.tests.integration.harness import Home

    jobs._reset_for_tests()
    yield Home(tmp_path, "laptop"), Home(tmp_path, "server")
    jobs._reset_for_tests()


def _run_pinned(home, mode: Mode, sid: str, options: Options, bundle: Path) -> tuple[str, int]:
    """``plan_sync`` + ``run_foreground`` with ONE participant pair per job (``BundleTarget`` keeps its plan in
    memory; reported by wp-a2 to wp-c/wp-a3)."""
    with home.active():
        runner = jobs.runner_for(None)
        make, pairs = runner._apis_factory, {}
        runner._apis_factory = lambda row: pairs.setdefault(row["job_id"], make(row))
        try:
            job_id, final = runner.plan_sync(
                mode,
                None,
                options,
                session_ref=SessionRef(id=sid),
                calling_session=None,
                session_key=None,
                bundle_path=bundle,
            )
            if final is None:
                return job_id, jobs.exit_code(runner.ledger.job(job_id, Role.CONDUCTOR))
            return job_id, runner.run_foreground(job_id)
        finally:
            runner._apis_factory = make


def test_l2_export_with_a_confirmed_proposal_imports(two_homes):
    """X02 + X06: an export whose user confirmed a proposed extra file (``--yes``) stores the confirmed FinalPlan in
    the bundle (the target was re-negotiated on it), so the import's pack check holds and the file arrives; the
    import takes the bundle's plan over (its id, not a newly negotiated one, is the one the pack belongs to).

    Mutant: ``Conductor._settle`` returns the confirmed plan without negotiating it again with the target."""
    laptop, server = two_homes
    laptop.identity()
    server.identity()
    repo = RepoBuilder.init(laptop.home / "proj")
    repo.write("README.md", "# app\n")
    repo.add_all()
    repo.commit("first")
    extra = laptop.home / "notes" / "plan.txt"
    extra.parent.mkdir()
    extra.write_text("the plan\n")
    b = SessionBuilder(laptop.db(), clock=FakeClock())
    sid = b.create(title="With extra", cwd=str(repo.root))
    b.user(sid, "read my plan")
    b.assistant(sid, "", tool_calls=[SessionBuilder.tool_call("read_file", {"path": str(extra)}, call_id="c1")])
    b.tool(sid, "c1", "the plan", name="read_file")
    b.assistant(sid, "read it")
    bundle = laptop.root / "out" / "job.ctxh"
    bundle.parent.mkdir(parents=True)
    job_x, rc = _run_pinned(laptop, Mode.EXPORT, sid, Options(refine="off", yes=True), bundle)
    events = [e["data"] for e in laptop.ledger().events(job_x, Role.CONDUCTOR) if e["kind"] == "error"]
    assert rc == 0, events
    exported = laptop.ledger().job(job_x, Role.CONDUCTOR)["result"]["plan"]
    assert any(i["source_path"] == str(extra) and i["include"] == "yes" for i in exported["plan"]["items"])

    job_i, rc = server.run(
        Mode.IMPORT,
        None,
        SessionRef(id=""),
        options=Options(refine="off", yes=True, trust_unsigned=True),
        bundle_path=bundle,
    )
    events = [e["data"] for e in server.ledger().events(job_i, Role.CONDUCTOR) if e["kind"] == "error"]
    assert rc == 0, events
    ctx: dict = {}
    for e in server.ledger().events(job_i, Role.CONDUCTOR):
        if isinstance((e["data"] or {}).get("ctx"), dict):
            ctx.update(e["data"]["ctx"])
    assert ctx["bundle_plan_id"] == exported["plan_id"]
    arrived = [p for p in server.home.rglob("plan.txt") if p.read_text() == "the plan\n"]
    assert arrived, "the confirmed extra file did not arrive"


def test_l2_import_refuses_a_bundle_plan_that_does_not_match_its_manifest(two_homes, monkeypatch):
    """X02: the import recomputes the bundle FinalPlan's id and compares it with the bundle manifest; a mismatch
    ends the job before anything is locked or staged (``verify_failed``)."""
    from ctx_handover.engine import bundle as bundle_module

    laptop, server = two_homes
    laptop.identity()
    server.identity()
    b = SessionBuilder(laptop.db(), clock=FakeClock())
    work = laptop.home / "work"
    work.mkdir()
    (work / "a.txt").write_text("a\n")
    sid = b.create(title="Plain", cwd=str(work))
    b.user(sid, "hello")
    b.assistant(sid, "hi")
    bundle = laptop.root / "out" / "plain.ctxh"
    bundle.parent.mkdir(parents=True)
    _job, rc = _run_pinned(laptop, Mode.EXPORT, sid, Options(refine="off", yes=True), bundle)
    assert rc == 0
    real_init = bundle_module.BundleSource.__init__

    def tampered(self, *a, **kw):
        real_init(self, *a, **kw)
        self.manifest = {**self.manifest, "plan_id": "0" * 64}

    monkeypatch.setattr(bundle_module.BundleSource, "__init__", tampered)
    with pytest.raises(VerifyFailed):
        server.run(
            Mode.IMPORT,
            None,
            SessionRef(id=""),
            options=Options(refine="off", yes=True, trust_unsigned=True),
            bundle_path=bundle,
        )
    (row,) = server.ledger().jobs(role=Role.CONDUCTOR, limit=5)
    assert row["state"] == "aborted" and row["last_error"]["code"] == "verify_failed"
    assert server.ledger().job(row["job_id"], Role.TARGET)["state"] in ("aborted", "open")
    assert not server.ledger().arrival_rows()
