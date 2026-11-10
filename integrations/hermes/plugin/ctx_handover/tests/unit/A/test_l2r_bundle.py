"""Layer R (red) for the bundle points of the layer-2 contract (cluster A side), with the real modules:

* V24 — a CLI export (``plan_sync`` + ``run_foreground``, the real ``bundle.BundleTarget`` factory, no memo) ends
  DONE, not ``in_doubt`` ("bundle commit before prepare").
* V13 — a bundle import binds the verified exporter as the job's peer: the ``in`` rows of the workspace map name that
  peer, and the return trip by bundle (push there, work, export back, import here) lands at the origin
  (DESIGN §9.7 ``return-mapping``); an unverified bundle has no return path and the note says so.
* V28 — ``export --for <peer>``: the ledger keeps the target facts last received from a peer (``hello``/``facts``);
  the export negotiates with them instead of the generic facts of an unknown importer.
* V09 — the note's ``case_collisions`` are strings built from E's pairs: an import on a case-insensitive target
  names both paths of a colliding pair.
"""

from __future__ import annotations

import dataclasses
from pathlib import Path

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.model import DestMode, Mode, Role, SessionRef
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.A._l2r_helpers import (
    OFF,
    ctx_of,
    dirty_repo,
    errors,
    export_pinned,
    note_of,
    own_final,
    paired,
    session_in,
    state,
)


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    yield Home(tmp_path, "server"), Home(tmp_path, "laptop")
    jobs._reset_for_tests()


def test_cli_export_with_the_real_bundle_target_ends_done(homes):
    """V24 (A side): the CLI path builds a new participant pair in ``plan_sync`` and in ``run_foreground``; the
    export still completes — the bundle file exists and the job is DONE (exit code 0)."""
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Export me")
    bundle = laptop.root / "out" / "job.ctxh"
    bundle.parent.mkdir(parents=True)
    job_id, rc = laptop.run(Mode.EXPORT, None, SessionRef(id=sid), options=OFF, bundle_path=bundle)
    assert (state(laptop, job_id), rc) == ("done", 0), errors(laptop, job_id)
    assert bundle.exists()


def test_import_binds_the_verified_exporter_and_the_return_trip_lands_at_the_origin(homes):
    """V13 with the airport scenario: the laptop pushes its repository to the server (``out`` row, peer =
    server); the server exports the arrived session; the laptop imports that bundle, signed by the paired server.
    The import binds the server as the job's peer — its ``in`` row names it — and the lookup finds the ``out`` row:
    the repository returns to its original place (``DestMode.RETURN``), not into a fresh directory."""
    server, laptop = homes
    repo = dirty_repo(laptop.home / "proj")
    origin = str(repo.root)
    sid = session_in(laptop, repo.root, "Round trip")
    with paired(server, laptop) as (_listener, peer):
        push_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=OFF)
        assert rc == 0, (state(laptop, push_id), errors(laptop, push_id))
    arrived = next(r for r in server.ledger().arrival_rows() if r["job_id"] == push_id)
    bundle = server.root / "out" / "back.ctxh"
    exp_id, rc = export_pinned(server, arrived["session_id"], bundle)
    assert rc == 0, (state(server, exp_id), errors(server, exp_id))

    imp_id, rc = laptop.run(Mode.IMPORT, None, SessionRef(id=""), options=OFF, bundle_path=bundle)
    assert rc == 0, (state(laptop, imp_id), errors(laptop, imp_id))
    server_peer = laptop.ledger().peer("server")
    own = own_final(laptop, imp_id)
    dests = [d.dest for d in own.decisions]
    rows = [r for d in dests for r in laptop.ledger().map_by_local(d) if r.last_job_id == imp_id]
    assert rows and {r.peer_id for r in rows if r.direction == "in"} == {server_peer.peer_id}, rows
    decision = next(d for d in own.decisions if d.mode is not DestMode.SKIP)
    assert (decision.dest, decision.mode) == (origin, DestMode.RETURN), decision


def test_unverified_bundle_note_says_there_is_no_return_path(homes):
    """V13: an unsigned bundle (``--trust-unsigned``) binds no peer; the note names that no return path exists for
    what arrived (it lands in a fresh directory and a later handover back cannot find its origin)."""
    server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Unverified")
    bundle = laptop.root / "out" / "job.ctxh"
    exp_id, rc = export_pinned(laptop, sid, bundle)
    assert rc == 0, errors(laptop, exp_id)
    imp_id, rc = server.run(
        Mode.IMPORT, None, SessionRef(id=""), options=dataclasses.replace(OFF, trust_unsigned=True), bundle_path=bundle
    )
    assert rc == 0, (state(server, imp_id), errors(server, imp_id))
    note = note_of(server, imp_id)
    assert "unverified" in note.lower()
    assert "return" in note.lower(), note


def test_export_for_a_peer_negotiates_with_its_last_known_facts(homes):
    """V28: a dry-run push hands the laptop the server's target facts; after a process restart (fresh composition
    root, same ledger) ``export --for server`` negotiates with them — the export's target facts are the server's
    (instance name, home, workspace root), not the generic facts of an unknown importer (``instance_name ==
    "bundle"``)."""
    server, laptop = homes
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "For server")
    with paired(server, laptop) as (_listener, peer):
        dry_id, _rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=dataclasses.replace(OFF, dry_run=True))
    known = ctx_of(laptop, dry_id, Role.CONDUCTOR)["target_facts"]
    assert known["instance_name"] != "bundle" and known["home"] == str(server.home)
    jobs._reset_for_tests()
    bundle = laptop.root / "out" / "for-server.ctxh"
    bundle.parent.mkdir(parents=True, exist_ok=True)
    exp_id, _rc = laptop.run(
        Mode.EXPORT, peer, SessionRef(id=sid), options=dataclasses.replace(OFF, dry_run=True), bundle_path=bundle
    )
    used = ctx_of(laptop, exp_id, Role.CONDUCTOR)["target_facts"]
    got = {k: used[k] for k in ("instance_name", "home", "workspace_root")}
    assert got == {k: known[k] for k in got}


def test_import_on_a_case_insensitive_target_names_both_colliding_paths(homes, monkeypatch):
    """V09: E reports a case collision as a pair (``README.md``/``readme.md``); ``note_parts["case_collisions"]``
    holds strings built from it, so the note's line "case collisions here" names both paths. The target's file
    system is made case-insensitive through the composition root's ``PolicyConfig`` (as on macOS/Windows)."""
    server, laptop = homes
    laptop.identity()
    r = RepoBuilder.init(laptop.home / "proj")
    r.write("README.md", "# upper\n")
    r.write("readme.md", "# lower\n")
    r.add_all()
    r.commit("two names")
    digest = tree_digest(r.root)
    sid = session_in(laptop, r.root, "Case pair")
    bundle = laptop.root / "out" / "case.ctxh"
    exp_id, rc = export_pinned(laptop, sid, bundle)
    assert rc == 0, errors(laptop, exp_id)

    real_policy = jobs._policy

    def case_insensitive(*a, **kw):
        return dataclasses.replace(real_policy(*a, **kw), case_insensitive=True)

    monkeypatch.setattr(jobs, "_policy", case_insensitive)
    imp_id, rc = server.run(
        Mode.IMPORT, None, SessionRef(id=""), options=dataclasses.replace(OFF, trust_unsigned=True), bundle_path=bundle
    )
    assert rc == 0, (state(server, imp_id), errors(server, imp_id))
    dest = Path(next(d.dest for d in own_final(server, imp_id).decisions))
    assert tree_digest(dest) == digest  # both files arrived (this host's real file system is case-sensitive)
    note = note_of(server, imp_id)
    line = next((ln for ln in note.splitlines() if "case collision" in ln), "")
    assert "README.md" in line and "readme.md" in line, note
