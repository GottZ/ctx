"""R32 / L3-F17, cluster A wiring.

A session larger than the context window of the target model is not handed over silently: the conductor gives
``plan.confirmation_reasons`` and ``plan.presend_summary`` its facts of both sides (``target_facts``,
``source_facts``) so D can compare the session's active context with the target model's window (reason (i)
``context_over``, which ``--yes`` does not confirm), and the source puts its estimate of that active context into the
plan (``build_plan(context_tokens=plan.active_context_tokens(rows, tip))`` → ``ResourcePlan.context_tokens``), so a
long but compacted lineage is measured by what the target's model actually loads, not by its total.

* conductor tests: the unit ``Harness`` (real ``Conductor`` and ``Ledger``, World fakes; ``FakePlanning`` raises (i)
  through D's own ``plan.context_over``);
* source test: ``LocalSource`` with the real modules (``test_l2_source_real.RealSource``);
* acceptance (L3-F17 step L24): two real homes, the same model on both, a listener process and a pull (``take``)
  with ``--yes`` — the same path the lab walked.
"""

from __future__ import annotations

from dataclasses import replace

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine import note as note_mod
from ctx_handover.engine import plan as planning
from ctx_handover.engine.errors import Cancelled
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import EnvFacts, Mode, Options, Role, SessionRef
from ctx_handover.tests import fakes
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes import apis_fake
from ctx_handover.tests.fakes.sessiondb import SessionBuilder
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.A._l2r_helpers import OFF, ctx_of, dirty_repo, errors, note_of, paired, state
from ctx_handover.tests.unit.A.harness import Harness
from ctx_handover.tests.unit.A.test_l2_source_real import mk

assert mk

MODEL = "fake-model"
PROVIDER = "lab"
PLAN_TOKENS = 1000
"""``approx_tokens`` of the World's plan (``FakeSource.start_plan``); it carries no estimate of its own."""


# ---------------------------------------------------------------------------------------------------------
# conductor: the facts reach D's reason (i) and the pre-send summary
# ---------------------------------------------------------------------------------------------------------


@pytest.fixture
def windows(monkeypatch):
    """Both World sides run ``fake-model``/``lab`` (the model stays: ``model_change`` None); the target's window is
    set per test, the source's is large — passing the facts the wrong way round finds no excess."""
    real = apis_fake.make_facts
    sizes = {"server": 131_072, "laptop": 131_072}

    def make_facts(name, **kw) -> EnvFacts:
        facts = real(name, **kw)
        window = sizes.get(name, facts.host.context_length)
        host = replace(facts.host, model=MODEL, provider=PROVIDER, context_length=window)
        return replace(facts, host=host)

    monkeypatch.setattr(apis_fake, "make_facts", make_facts)
    return sizes


def _conductor_ctx(h: Harness) -> dict:
    out: dict = {}
    for e in h.ledger.events(h.job_id, Role.CONDUCTOR):
        data = e["data"] or {}
        if isinstance(data.get("ctx"), dict):
            out.update(data["ctx"])
    return out


def test_g4_yes_does_not_confirm_a_session_larger_than_the_target_window(tmp_path, clock, windows):
    """★ R32: the model stays, the plan's 1000 tokens exceed the target window of 500. With ``--yes`` the user is
    asked anyway (``yes_exempt`` = ``context_over``), the question's summary names tokens, window and model; without
    an answer the job ends ABORTED before the lock; an explicit ``yes`` goes ahead and the journaled pre-send
    summary (ctx ``presend``) names the excess.

    Mutants: ``_confirm`` calls ``confirmation_reasons`` without ``target_facts``; ``_confirm`` builds the question's
    summary without the facts; ``_negotiated`` journals ``presend_summary`` without the facts; ``target_facts`` and
    ``source_facts`` swapped."""
    windows["laptop"] = 500
    over = {"tokens": PLAN_TOKENS, "context_length": 500, "model": MODEL}
    for answer in (None, "yes"):
        h = Harness(tmp_path, clock, options=Options(yes=True))
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
            assert _conductor_ctx(h)["presend"]["context_over"] == over
        assert len(asked) == 1, asked
        assert planning.CONFIRM_CONTEXT in asked[0]["reasons"], asked[0]["reasons"]
        assert asked[0]["yes_exempt"] == [planning.CONFIRM_CONTEXT], asked[0]
        assert asked[0]["summary"]["context_over"] == over


def test_g4_a_session_within_the_target_window_is_not_asked_about(tmp_path, clock, windows):
    """R32: the plan's 1000 tokens fit the target window of 4000 — ``--yes`` goes through without a question, and the
    pre-send summary has no excess."""
    windows["laptop"] = 4000
    h = Harness(tmp_path, clock, options=Options(yes=True))
    asked: list[dict] = []
    c = h.conductor(confirm=lambda info: asked.append(info) or "yes")
    c.run_until_negotiated()
    c.run()
    assert h.state() == "done", h.states()
    assert asked == []
    assert _conductor_ctx(h)["presend"]["context_over"] is None


# ---------------------------------------------------------------------------------------------------------
# source: the plan carries the estimate of the active context
# ---------------------------------------------------------------------------------------------------------


def test_g4_source_plan_carries_the_active_context_of_the_tip(mk, make_session):
    """★ R32: ``LocalSource.start_plan`` puts ``plan.active_context_tokens(rows, tip)`` into the plan — after a
    compression rotation that is the tip's handoff, not the long parent the lineage total (``approx_tokens``) still
    counts.

    Mutants: ``start_plan`` calls ``build_plan`` without ``context_tokens``; with
    ``active_context_tokens(rows, None)`` (the whole lineage)."""
    env = mk(remote_driven=False)
    for n in range(6):
        make_session.user(env.sid, SessionBuilder.text(f"long question {n}", 4000))
        make_session.assistant(env.sid, SessionBuilder.text(f"long answer {n}", 4000))
    tip = make_session.rotate(env.sid, keep_last=1, summary="summary of the long work")
    env.ref = SessionRef(id=tip)
    plan = env.plan(env.job())
    rows = env.db.export_session_lineage(tip, include_inactive=True)["messages"]
    expected = planning.active_context_tokens(rows, tip)
    assert 0 < expected < planning.active_context_tokens(rows, None)
    assert plan.context_tokens == expected, (plan.context_tokens, expected, plan.approx_tokens)
    assert plan.approx_tokens > 4 * expected
    assert planning.context_tokens_of(plan) == expected


# ---------------------------------------------------------------------------------------------------------
# acceptance on two real homes (L3-F17, step L24): pull with --yes, the model stays
# ---------------------------------------------------------------------------------------------------------

WINDOW = 4000
"""``model.context_length`` of the taking home (the lab's B had 4000)."""


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    providers = {PROVIDER: {"base_url": "http://127.0.0.1:9/v1", "api_key": "unused"}}
    server = Home(tmp_path, "server")
    laptop = Home(tmp_path, "laptop")
    server.write_settings({"refine": "off"}, model={"default": MODEL, "provider": PROVIDER}, providers=providers)
    laptop.write_settings(
        {"refine": "off"},
        model={"default": MODEL, "provider": PROVIDER, "context_length": WINDOW},
        providers=providers,
    )
    yield server, laptop
    jobs._reset_for_tests()


def _session(home: Home, title: str, *, pairs: int, size: int) -> tuple[SessionBuilder, str]:
    repo = dirty_repo(home.home / f"proj-{ulid()[-6:].lower()}")
    b = SessionBuilder(home.db(), clock=fakes.FakeClock())
    sid = b.create(title=title, cwd=str(repo.root))
    for n in range(pairs):
        b.user(sid, SessionBuilder.text(f"question {n}", size))
        b.assistant(sid, SessionBuilder.text(f"answer {n}", size))
    return b, sid


def _planning_ended(home: Home, job_id: str) -> dict:
    """The conductor's ``planning_ended`` info event (how and why the job ended at the confirmation)."""
    for e in home.ledger().events(job_id, Role.CONDUCTOR):
        data = e["data"] or {}
        if e["kind"] == "info" and data.get("event") == "planning_ended":
            return data
    return {}


def _locked(home: Home, sid: str) -> bool:
    row = home.db().get_session(sid) or {}
    return bool(row.get("end_reason"))


def test_g4_pull_of_a_session_larger_than_the_target_window_asks_despite_yes(homes):
    """★ Acceptance R32 / L3-F17 (step L24): the model stays (``fake-model``/``lab`` on both homes), the taking home's
    window is 4000 tokens.

    * active context ≈ 6000 tokens, ``take --yes`` without a terminal ⇒ no confirmation, the job ends ABORTED before
      the lock, the source session untouched; the result names ``context_over``;
    * the same with an answer ``yes`` ⇒ the question listed ``context_over`` (``yes_exempt``) with tokens, window
      and model; the job is done and the note on the target says the conversation is larger than the window;
    * a short session, and a long lineage whose tip (after a compression rotation) is short ⇒ no question, done, no
      context line in the note.

    Red before the fix: the conductor did not pass its facts, so reason (i) never fired for a
    model that stays — the first job ended ``done`` (rc 0) like in the lab."""
    server, laptop = homes
    _b, big = _session(server, "Long session", pairs=6, size=2000)
    _b, small = _session(server, "Short session", pairs=1, size=200)
    rotated_builder, parent = _session(server, "Rotated session", pairs=6, size=2000)
    rotated = rotated_builder.rotate(parent, keep_last=1, summary="summary of the long work")
    with paired(server, laptop) as (_listener, peer):
        refused_id, rc = laptop.run(Mode.PULL, peer, SessionRef(id=big), options=OFF)
        assert (state(laptop, refused_id), rc) != ("done", 0), "a session larger than the window went through"
        assert state(laptop, refused_id) == "aborted", errors(laptop, refused_id)
        ended = _planning_ended(laptop, refused_id)
        assert ended.get("ended_by") == "cancel" and planning.CONFIRM_CONTEXT in ended.get("reasons", []), ended
        assert not _locked(server, big)

        asked: list[dict] = []
        job_id, rc = laptop.run(
            Mode.PULL, peer, SessionRef(id=big), options=OFF, confirm=lambda info: asked.append(info) or "yes"
        )
        assert (state(laptop, job_id), rc) == ("done", 0), errors(laptop, job_id)
        assert len(asked) == 1 and asked[0]["yes_exempt"] == [planning.CONFIRM_CONTEXT], asked
        over = asked[0]["summary"]["context_over"]
        assert over["context_length"] == WINDOW and over["model"] == MODEL and over["tokens"] > WINDOW, over
        assert ctx_of(laptop, job_id, Role.CONDUCTOR)["presend"]["context_over"] == over
        note = note_of(laptop, job_id)
        line = note_mod.CONTEXT_LINE.format(tokens=over["tokens"], model=MODEL, window=WINDOW)
        assert line in note, note

        for sid in (small, rotated):
            quiet: list[dict] = []
            job_id, rc = laptop.run(
                Mode.PULL, peer, SessionRef(id=sid), options=OFF, confirm=lambda info, q=quiet: q.append(info) or "yes"
            )
            assert (state(laptop, job_id), rc) == ("done", 0), errors(laptop, job_id)
            assert quiet == [], (sid, quiet)
            assert "Context: this conversation" not in note_of(laptop, job_id)
