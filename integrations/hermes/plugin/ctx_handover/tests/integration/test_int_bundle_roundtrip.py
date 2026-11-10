"""Offline bundle between two homes (TESTPLAN §3 ``test_int_export_import_roundtrip_two_homes``; DESIGN §5.4,
§6.2, §6.8, §6.9, §3.4).

Home A (server) exports a rotated session with a git working directory into a bundle file; home B (laptop),
paired with A, imports it. Checked along the way: the capsule travels inside the signed bundle and carries the
source's hashes, the import lands in B's staging and is hidden until the commit unhides it, and H_all/H_rows/H_model
of the imported lineage equal the source's.

``test_int_export_import_roundtrip_two_homes`` exports the way the CLI does (``plan_sync`` + ``run_foreground``:
two conductor instances, contract V24). ``test_int_import_of_single_instance_export_two_homes`` exports with one
participant pair for the whole job (a memoising ``apis_factory``), so the import half is checked independently
of V24."""

from __future__ import annotations

import json
from pathlib import Path

from ctx_handover.engine import bundle, jobs, local_target
from ctx_handover.engine.model import Mode, Options, Role, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.integration._int2_helpers import ack_ids, histories, outcome, target_ctx
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks

#: A working export of this session takes seconds; a job parked in ``in_doubt`` would otherwise wait the default
#: ``stall_timeout_min`` (30 min) before it reports.
EXPORT_OPTIONS = Options(refine="off", yes=True, stall_timeout_s=20)


def _source(server):
    r = RepoBuilder.init(server.home / "proj")
    r.write("README.md", "# bundle roundtrip\n")
    r.write("src/app.py", "print('v1')\n")
    r.add_all()
    r.commit("first")
    r.write("src/app.py", "print('v2, not committed')\n")
    r.write("notes.txt", "untracked note\n")
    b = builder(server)
    root = b.create(title="Bundle trip", cwd=str(r.root))
    b.turns(root, 6, with_tools=True)
    tip = b.rotate(root, keep_last=2)
    b.turns(tip, 4)
    db = server.db()
    return {
        "repo": r,
        "digest": tree_digest(r.root),
        "tip": tip,
        "hashes": lineage_hashes(db, tip),
        "histories": histories(db, tip),
    }


def _single_instance_runner(home) -> jobs.JobRunner:
    """A runner of ``home`` whose job keeps ONE participant pair from planning to the end (test seam for V24)."""
    with home.active():
        prof = jobs._profile()  # the profile's ledger, as the plugin uses it
        memo: dict = {}
        runner = jobs.JobRunner(prof.ledger, apis_factory=lambda row: memo[row["job_id"]])
        default = runner._default_apis  # the production composition, memoised per job

        def factory(row):
            if row["job_id"] not in memo:
                memo[row["job_id"]] = default(row)
            return memo[row["job_id"]]

        runner._apis_factory = factory
        return runner


def _export_single_instance(server, src, bundle_path: Path) -> str:
    runner = _single_instance_runner(server)
    with server.active():
        job_id, final = runner.plan_sync(
            Mode.EXPORT,
            None,
            EXPORT_OPTIONS,
            session_ref=SessionRef(id=src["tip"]),
            calling_session=None,
            session_key=None,
            bundle_path=bundle_path,
        )
        assert final is not None, outcome(server, job_id)
        rc = runner.run_foreground(job_id)
    assert rc == 0, outcome(server, job_id)
    return job_id


def _check_export(server, laptop, src, bundle_path: Path) -> None:
    assert bundle_path.is_file()
    marks = session_marks(server.db(), src["tip"])
    assert marks["ended"] and marks["archived"] and marks["hidden"], marks  # export retires by default (O3)
    # The capsule is inside the bundle, signed by A and verified by B through the stored peer key (SEC 1.6).
    manifest, verified = bundle.read_manifest(bundle_path, ledger=laptop.ledger())
    assert verified, manifest.get("exporter")
    assert manifest["kind"] == "ctxh.bundle" and manifest["retire"] is True
    assert manifest["capsule_id"] in manifest["objects"]
    reader = bundle.BundleSource(bundle_path, trusted_pubkeys=(), trust_unsigned=False, ledger=laptop.ledger())
    try:
        capsule = json.loads(reader.blocks().object_bytes(manifest["capsule_id"]))
    finally:
        reader.close()
    assert capsule["kind"] == "ctxh.capsule"
    assert capsule["session"]["tip_id"] == src["tip"] and len(capsule["session"]["lineage_ids"]) == 2
    got = (capsule["hashes"]["h_all"], capsule["hashes"]["h_rows"], capsule["hashes"]["h_model"])
    assert got == src["hashes"]


def _import_and_check(laptop, src, bundle_path: Path, monkeypatch) -> None:
    # Hidden import in staging: observed at the moment the conductor starts the commit (after PREPARE).
    seen: dict = {}
    original = local_target.LocalTarget.start_commit

    def observe_commit(self, job):
        prepared = target_ctx(laptop, job.job_id)["prepared"]
        db = laptop.db()
        ids = (prepared["root_id"], prepared["tip_id"])
        seen["hidden"] = [bool((db.get_session(s) or {}).get("hidden")) for s in ids]
        seen["listed"] = bool(set(ids) & {r["id"] for r in db.list_sessions_rich(limit=200)})
        staging = laptop.data_dir() / "staging" / job.job_id
        seen["staging"] = staging.is_dir() and any(staging.iterdir())
        seen["staging_path"] = staging
        return original(self, job)

    monkeypatch.setattr(local_target.LocalTarget, "start_commit", observe_commit)
    opts = Options(refine="off", yes=True, stall_timeout_s=20)
    job_i, rc_i = laptop.run(Mode.IMPORT, None, SessionRef(), options=opts, bundle_path=bundle_path)
    assert rc_i == 0, outcome(laptop, job_i)
    assert seen.get("hidden") == [True, True], seen
    assert seen["listed"] is False, seen
    assert seen["staging"] is True, seen

    new_root, new_tip = ack_ids(laptop, job_i)
    assert new_tip and new_root != new_tip, (new_root, new_tip)
    arrivals = {r["session_id"]: r["root_id"] for r in laptop.ledger().arrival_rows() if r["job_id"] == job_i}
    assert arrivals.get(new_tip) == new_root, arrivals  # c5: the arrival row of the tip names the new root
    dst = laptop.db()
    assert not session_marks(dst, new_tip)["hidden"] and not session_marks(dst, new_root)["hidden"]  # c6 unhide
    assert {new_root, new_tip} & {r["id"] for r in dst.list_sessions_rich(limit=200)}  # listed after the commit
    assert not seen["staging_path"].exists()  # staging released after the commit
    assert lineage_hashes(dst, new_tip) == src["hashes"]
    assert histories(dst, new_tip) == src["histories"]
    assert len(dst.get_compression_lineage(new_tip)) == 2

    dest = laptop.home / "hermes-handover" / "proj"
    assert tree_digest(dest) == src["digest"]
    assert RepoBuilder(dest).head() == src["repo"].head()
    origin = [e["data"] for e in laptop.ledger().events(job_i, Role.CONDUCTOR) if e["kind"] == "origin"]
    assert origin and origin[0]["origin"].startswith("verified handover file"), origin


def test_int_export_import_roundtrip_two_homes(online, monkeypatch):
    server, laptop, _listener, _peer = online
    src = _source(server)
    bundle_path = server.root / "out" / "trip.ctxh.tar"
    ref = SessionRef(id=src["tip"])
    job_x, rc_x = server.run(Mode.EXPORT, None, ref, options=EXPORT_OPTIONS, bundle_path=bundle_path)
    assert rc_x == 0, outcome(server, job_x)
    _check_export(server, laptop, src, bundle_path)
    _import_and_check(laptop, src, bundle_path, monkeypatch)


def test_int_import_of_single_instance_export_two_homes(online, monkeypatch):
    server, laptop, _listener, _peer = online
    src = _source(server)
    bundle_path = server.root / "out" / "trip.ctxh.tar"
    _export_single_instance(server, src, bundle_path)
    _check_export(server, laptop, src, bundle_path)
    _import_and_check(laptop, src, bundle_path, monkeypatch)
