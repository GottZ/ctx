"""Review round 3 — M8 rest (REVIEW-2 §5): the process dies inside the hidden import, the
prepare operation starts again (the janitor's default for ``preparing``) and mints new ids. Before the fix the restart
replaced ``planned_ids``: the session the first attempt wrote fell out of every list and stayed in ``state.db`` —
after the successful restart and after an abort alike.

Now ``LocalTarget._prepare`` discards what an earlier, unfinished attempt wrote before it plans the new import, and
whatever could not be removed stays journaled next to the new ids.

Red before the fix: ``first attempt's session still there after the restart``. The reviewer's probe
is the scenario."""

from __future__ import annotations

import dataclasses

import pytest

from ctx_handover.engine import session_import as real_si
from ctx_handover.engine.model import OpState, PackInfo, PrepareRequest, SessionRef
from ctx_handover.tests.fakes.apis_fake import SimulatedCrash, make_facts
from ctx_handover.tests.unit.A import standins
from ctx_handover.tests.unit.A.standins import tree_pack
from ctx_handover.tests.unit.A.test_local_target import WorkspaceTxn, env  # noqa: F401  (fixture)


def _ready(env):  # noqa: F811
    t, job = env.target, env.job
    t.open_job(job)
    t.facts(job, make_facts("server"))
    final = t.negotiate(job, env.plan())
    info = standins.describe(env.db, SessionRef(id=env.src_sid))
    capsule = standins.export_capsule(
        env.db, info, job_id=job.job_id, store=env.store, source_meta={}, hermes=env.hermes
    )
    assert env.wait(t.start_stage_context(job, capsule)).state is OpState.DONE
    pack_id = tree_pack(env.store, "w0", {"a.txt": b"hello"}, final.plan_id)
    req = PrepareRequest(
        capsule=capsule,
        pack=PackInfo(pack_id=pack_id, size=1),
        final_plan_id=final.plan_id,
        source_facts=make_facts("server"),
        note_parts={"mode": "pull"},
    )
    stand_in_plan = t.ports.session_import.plan_import

    def fresh_plan(db, *a, **kw):
        """Like the real ``plan_import``: every call mints fresh ids (``mint_free_id``)."""
        res = stand_in_plan(db, *a, **kw)
        taken: set = set()
        res.segments = tuple(dataclasses.replace(s, new_id=real_si.mint_free_id(db, taken)) for s in res.segments)
        return res

    t.ports.session_import.plan_import = fresh_plan
    return t, job, req


def _crash_inside_the_import(env, t, job, req) -> str:  # noqa: F811
    real = t.ports.session_import.import_hidden
    written: list[str] = []

    def dies_midway(db, store, capsule_id, plan, **kw):
        sid = plan.segments[0].new_id
        db.create_session(sid, "cli", model="test-model", cwd=plan.cwd)
        written.append(sid)
        raise SimulatedCrash("process died inside import_hidden")

    t.ports.session_import.import_hidden = dies_midway
    with pytest.raises(SimulatedCrash):
        t._prepare(job, req, lambda *_: None)
    t.ports.session_import.import_hidden = real
    assert env.db.get_session(written[0]) is not None
    return written[0]


def test_r3_a_m8_a_restarted_prepare_removes_the_first_attempts_session(env):  # noqa: F811
    """★ Crash inside the import, the prepare op runs again and succeeds: the first attempt's session is gone, the
    restarted import is the one prepared.

    Mutant (``LocalTarget._prepare``): no discard of the earlier ``planned_ids`` ⇒ the first session stays."""
    t, job, req = _ready(env)
    first = _crash_inside_the_import(env, t, job, req)

    prepared = t._prepare(job, req, lambda *_: None)

    assert prepared.tip_id != first
    assert env.db.get_session(first) is None, "first attempt's session still there after the restart"
    assert env.db.get_session(prepared.tip_id) is not None


def test_r3_a_m8_an_abort_after_the_restart_removes_both_attempts(env):  # noqa: F811
    t, job, req = _ready(env)
    first = _crash_inside_the_import(env, t, job, req)
    prepared = t._prepare(job, req, lambda *_: None)

    t.abort(job, "user")

    assert env.db.get_session(first) is None
    assert env.db.get_session(prepared.tip_id) is None


def test_r3_a_m8_what_could_not_be_removed_stays_journaled(env, monkeypatch):  # noqa: F811
    """A session the restart could not discard (the import's ``discard`` failed) is not forgotten: it stays in
    ``planned_ids`` next to the new ids, so the abort tries it again."""
    t, job, req = _ready(env)
    first = _crash_inside_the_import(env, t, job, req)
    real_discard = t.ports.session_import.discard
    calls = {"n": 0}

    def flaky(db, prepared):
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("database is locked")
        return real_discard(db, prepared)

    t.ports.session_import.discard = flaky
    prepared = t._prepare(job, req, lambda *_: None)
    planned = t._ctx(job.job_id).get("planned_ids")
    assert first in planned and prepared.tip_id in planned

    t.abort(job, "user")
    assert env.db.get_session(first) is None
