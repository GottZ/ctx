"""Layer R (red first) — V24 of ``reports/L2-contracts.md``, cluster C's side: ``BundleTarget`` keeps its state
(FinalPlan, progress) in the job's staging, not only in memory, so a new instance — or a new process — continues the
export. The CLI path ``plan_sync`` (open, facts, negotiate) + ``run_foreground`` (objects, stage, prepare, commit)
builds a new ``BundleTarget`` per call through ``jobs.apis_for``; the conductor never repeats the journaled
``open_job`` there.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

from ctx_handover.engine.bundle import BundleTarget, read_manifest
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, OpState
from ctx_handover.tests.unit.C._l2r_helpers import M, bundle_plan, job_context, stage_and_prepare
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.helpers import child_env, sample_facts


def _exporter(tmp_path):
    env: dict[str, str] = {}
    ledger = Ledger(tmp_path / "exporter" / "ledger.sqlite3")
    identity = new_identity(ledger, env)
    return ledger, identity, env


def _target(out, identity, ledger) -> BundleTarget:
    """``BundleTarget`` as ``jobs.apis_for`` builds it for every runner call of one export job."""
    return BundleTarget(out, identity, as_dir=False, declared_facts=None, ledger=ledger, exporter_name="server")


def _check_bundle(out, identity, job, plan_id: str) -> None:
    assert out.is_file(), f"no bundle at {out}"
    manifest, verified = read_manifest(out, trusted_pubkeys={identity.public_b64()})
    assert verified
    assert manifest["job_id"] == job.job_id
    assert manifest["plan_id"] == plan_id
    assert manifest["title"] == "My session"
    assert manifest["created_at"] == job.created_at


def test_l2r_bundle_target_new_instance_continues_after_negotiation(tmp_path):
    """V24: instance 1 does what ``plan_sync`` does (``open_job``, ``facts``, ``negotiate``); a new instance for
    the same bundle path — what ``run_foreground`` gets — receives the objects, stages, prepares and commits: the
    commit succeeds and the bundle carries the FinalPlan instance 1 negotiated (today: ``bundle commit before
    prepare`` ⇒ the CLI export ends ``in_doubt``, wp-a2)."""
    ledger, identity, _env = _exporter(tmp_path)
    out = tmp_path / "exports" / "work.ctxh.tar"
    job = job_context(mode=Mode.EXPORT)

    first = _target(out, identity, ledger)
    first.open_job(job)
    first.facts(job, sample_facts(M, "server"))
    final = first.negotiate(job, bundle_plan(job.job_id))
    del first

    second = _target(out, identity, ledger)
    stage, prepare = stage_and_prepare(second, job, final)
    assert stage.state == OpState.DONE, stage
    assert prepare.state == OpState.DONE, prepare
    commit = second.start_commit(job)
    assert commit.state == OpState.DONE, commit.error
    _check_bundle(out, identity, job, final.plan_id)


def test_l2r_bundle_target_new_process_commits_a_prepared_export(tmp_path):
    """V24: a process stops after ``prepare`` (the conductor process ended between the steps); in a new process a
    new ``BundleTarget`` reports the job's progress from the staging (state ``prepared``, the prepare operation
    known to ``op_status`` — what a resumed conductor polls) and commits the bundle with the FinalPlan of the
    first process."""
    ledger, identity, env = _exporter(tmp_path)
    out = tmp_path / "exports" / "work.ctxh.tar"
    job = job_context(mode=Mode.EXPORT)
    cfg = tmp_path / "child.json"
    fd = os.open(cfg, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        json.dump(
            {
                "ledger": str(ledger.path),
                "env": env,
                "out": str(out),
                "as_dir": False,
                "job_id": job.job_id,
                "created_at": job.created_at,
            },
            fh,
        )
    proc = subprocess.run(  # noqa: S603 - this interpreter and a helper module of the test tree
        [sys.executable, "-m", "ctx_handover.tests.unit.C._l2r_helpers", "bundle-child", str(cfg)],
        env=child_env(),
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    assert proc.returncode == 0, proc.stderr[-3000:]
    child = json.loads(proc.stdout.strip().splitlines()[-1])
    assert child["stage"] == str(OpState.DONE) and child["prepare"] == str(OpState.DONE), child

    resumed = _target(out, identity, ledger)
    assert resumed.status(job).state == "prepared"
    assert resumed.op_status(job, child["op"]).state == OpState.DONE
    commit = resumed.start_commit(job)
    assert commit.state == OpState.DONE, commit.error
    _check_bundle(out, identity, job, child["plan_id"])
