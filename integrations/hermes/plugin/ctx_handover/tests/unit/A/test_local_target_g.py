"""Layer G for ``LocalTarget`` (cluster A core): V13 — an import without a bound peer says in the note that there is
no return path; V09 — on a case-insensitive file system the note names both paths of a case collision pair."""

from __future__ import annotations

import dataclasses
import json

from ctx_handover.engine import note
from ctx_handover.engine.jsonutil import canonical_bytes
from ctx_handover.engine.model import Mode, OpState, PackInfo, PrepareRequest, SessionRef
from ctx_handover.tests.fakes.apis_fake import make_facts
from ctx_handover.tests.unit.A import standins
from ctx_handover.tests.unit.A.standins import tree_pack
from ctx_handover.tests.unit.A.test_local_target import env

assert env


def _prepare(e, *, case_collisions=None) -> dict:
    """``Env.prepared`` with a recording ``note.compose``; returns the keyword arguments of the note."""
    seen: dict = {}
    real = e.target.ports.note.compose

    def compose(**kw):
        seen.update(kw)
        return real(**kw)

    e.target.ports.note.compose = compose
    e.target.ports.note.case_collision_texts = note.case_collision_texts
    t, job = e.target, e.job
    t.open_job(job)
    t.facts(job, make_facts("server"))
    final = t.negotiate(job, e.plan())
    info = standins.describe(e.db, SessionRef(id=e.src_sid))
    capsule = standins.export_capsule(e.db, info, job_id=job.job_id, store=e.store, source_meta={}, hermes=e.hermes)
    assert e.wait(t.start_stage_context(job, capsule)).state is OpState.DONE
    pack_id = tree_pack(e.store, "w0", {"README.md": b"upper", "readme.md": b"lower"}, final.plan_id)
    if case_collisions is not None:
        with e.store.open_object(pack_id) as fh:
            doc = json.loads(fh.read())
        doc["items"][0]["case_collisions"] = case_collisions
        pack_id = e.store.put_bytes(canonical_bytes(doc), "application/json").object_id
    req = PrepareRequest(
        capsule=capsule,
        pack=PackInfo(pack_id=pack_id, size=1),
        final_plan_id=final.plan_id,
        source_facts=make_facts("server"),
        note_parts={},
    )
    st = e.wait(t.start_prepare(job, req))
    assert st.state is OpState.DONE, st.error
    return seen


def test_import_without_a_bound_peer_says_there_is_no_return_path(env):
    """V13: an import that binds no paired peer (unverified bundle, unknown signer) — its note's origin line says
    that nothing that arrived can be handed back to its origin."""
    env.job = dataclasses.replace(env.job, mode=Mode.IMPORT)
    env.target.source_peer_id = None
    env.target.peer_verified = False
    seen = _prepare(env)
    assert seen["origin"].startswith("unverified bundle") and "no return path" in seen["origin"], seen["origin"]


def test_import_from_a_bound_peer_has_a_return_path(env):
    env.job = dataclasses.replace(env.job, mode=Mode.IMPORT)
    seen = _prepare(env)
    assert seen["origin"].startswith("verified bundle") and "return path" not in seen["origin"], seen["origin"]


def test_every_bundle_import_note_says_it_may_have_been_imported_elsewhere(env):
    """DESIGN §5.4: the re-import guard is per profile, so every bundle import note says "this bundle may have been
    imported elsewhere too" (verified or not); a pull's note does not. The sentence is the note part
    ``bundle_import`` (``note.BUNDLE_IMPORT_SENTENCE``), no longer part of the origin line."""
    env.job = dataclasses.replace(env.job, mode=Mode.IMPORT)
    seen = _prepare(env)
    assert seen["note_parts"]["bundle_import"] is True
    assert "imported elsewhere" not in seen["origin"], seen["origin"]


def test_a_pull_note_says_nothing_about_bundles(env):
    seen = _prepare(env)
    assert seen["origin"] == "verified peer"
    assert "bundle_import" not in seen["note_parts"]


def test_case_collisions_are_named_on_a_case_insensitive_target(env):
    """V09: E's ``[kept, colliding]`` pairs of the pack item become the note's ``case_collisions`` strings when this
    host's file system is case-insensitive (``PolicyConfig.case_insensitive``); not on a case-sensitive one."""
    pairs = [["README.md", "readme.md"]]
    env.target.policy = dataclasses.replace(env.target.policy, case_insensitive=True)
    seen = _prepare(env, case_collisions=pairs)
    assert seen["note_parts"]["case_collisions"] == ["README.md ↔ readme.md"]


def test_case_collisions_are_not_named_on_a_case_sensitive_target(env):
    seen = _prepare(env, case_collisions=[["README.md", "readme.md"]])
    assert "case_collisions" not in seen["note_parts"]
