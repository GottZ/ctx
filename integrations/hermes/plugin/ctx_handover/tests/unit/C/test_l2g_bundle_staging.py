"""Layer G — ``BundleTarget`` keeps its progress in the job's staging (the layer-2 contract V24): what a
resumed export relies on beyond the layer-R tests (``test_l2r_bundle_resume.py``) — a commit whose answer was lost,
a commit without a prepared staging, two exports to one path, and a progress file other users could write.
"""

from __future__ import annotations

import os

import pytest

from ctx_handover.engine.bundle import STATE_FILE, BundleTarget, read_manifest
from ctx_handover.engine.errors import ConflictError
from ctx_handover.engine.jsonutil import from_wire
from ctx_handover.engine.model import CommitAck, Mode, OpState, Role
from ctx_handover.tests.unit.C._l2r_helpers import (
    M,
    bundle_plan,
    job_context,
    move_objects,
    noise,
    stage_and_prepare,
)
from ctx_handover.tests.unit.C.conftest import end_job, new_identity
from ctx_handover.tests.unit.C.helpers import sample_facts
from ctx_handover.tests.unit.C.ledger_stub import make_ledger


def _exporter(tmp_path):
    ledger = make_ledger(tmp_path / "exporter" / "ledger.sqlite3")
    return ledger, new_identity(ledger)


def _target(out, identity, ledger, *, as_dir: bool = False) -> BundleTarget:
    """``BundleTarget`` as ``jobs.apis_for`` builds it for every runner call of one export job."""
    return BundleTarget(out, identity, as_dir=as_dir, declared_facts=None, ledger=ledger, exporter_name="server")


def _negotiated(target: BundleTarget, job):
    target.open_job(job)
    target.facts(job, sample_facts(M, "server"))
    return target.negotiate(job, bundle_plan(job.job_id))


def _stamp(out, as_dir: bool) -> int:
    return (out / "manifest.json" if as_dir else out).stat().st_mtime_ns


@pytest.mark.parametrize("as_dir", [False, True], ids=["tar", "dir"])
def test_a_new_instance_reports_a_bundle_already_committed(tmp_path, as_dir):
    """★ V24: the commit finished but its answer was lost (the conductor parks IN_DOUBT and polls ``status``): a
    new instance finds the bundle this identity signed for the job at ``out`` — ``status`` says ``committed``,
    ``start_commit`` answers DONE with the ack and writes nothing again, a late ``abort`` keeps the bundle. A
    directory bundle carries no progress file; the bundle of another job is no commit of this one.

    Red proof: mutant = ``_sync`` without the committed-bundle branch → ``status`` ``open``.
    """
    ledger, identity = _exporter(tmp_path)
    out = tmp_path / ("work.d" if as_dir else "work.ctxh.tar")
    job = job_context(mode=Mode.EXPORT)
    first = _target(out, identity, ledger, as_dir=as_dir)
    final = _negotiated(first, job)
    stage_and_prepare(first, job, final)
    assert first.start_commit(job).state == OpState.DONE
    stamp = _stamp(out, as_dir)
    del first
    if as_dir:
        assert not (out / STATE_FILE).exists()
    assert not (tmp_path / f"{out.name}.partial").exists()

    resumed = _target(out, identity, ledger, as_dir=as_dir)
    assert resumed.status(job).state == "committed"
    op = resumed.start_commit(job)
    assert op.state == OpState.DONE, op.error
    ack = from_wire(CommitAck, op.result)
    assert ack.title == "My session"
    assert str(out) in " ".join(ack.warnings)
    resumed.abort(job, "late abort")
    manifest, verified = read_manifest(out, trusted_pubkeys={identity.public_b64()})
    assert verified and manifest["job_id"] == job.job_id and manifest["plan_id"] == final.plan_id
    assert _stamp(out, as_dir) == stamp, "the resumed commit wrote the bundle again"

    other = job_context(mode=Mode.EXPORT)
    assert _target(out, identity, ledger, as_dir=as_dir).status(other).state == "open"


def test_commit_without_a_prepared_staging_is_not_retryable(tmp_path):
    """★ V24 addition (layer-2 contract §1a): a commit with no prepared staging fails ``invalid_state`` and is not
    retryable — the conductor ends the job with that message at once instead of waiting ``stall_timeout`` in
    IN_DOUBT (30 min). Nothing appears at ``out``.

    Red proof: mutant = ``_commit`` raises the plain ``HandoverError`` again → ``retryable`` True, code ``internal``.
    """
    ledger, identity = _exporter(tmp_path)
    out = tmp_path / "work.ctxh.tar"
    job = job_context(mode=Mode.EXPORT)
    target = _target(out, identity, ledger)
    _negotiated(target, job)
    op = _target(out, identity, ledger).start_commit(job)
    assert op.state == OpState.FAILED
    assert op.error["code"] == "invalid_state", op.error
    assert op.error["retryable"] is False
    assert "commit before prepare" in op.error["message"]
    assert not out.exists()


def test_open_job_refuses_a_running_export_to_the_same_path_and_clears_an_ended_one(tmp_path):
    """★ The staging belongs to one job: a repeated ``open_job`` of the same job keeps its progress and objects; a
    second export to the same path is refused while the first one runs; once that one ended, the second starts
    with a clean staging — no object of the old job ends up in its signed bundle.

    Red proofs: mutant = no running-job check → no ``ConflictError``; mutant = the staging of the ended job is kept
    → its object is in the new staging (and bundle).
    """
    ledger, identity = _exporter(tmp_path)
    out = tmp_path / "work.ctxh.tar"
    first_job = job_context(mode=Mode.EXPORT)
    ledger.create_job(first_job.job_id, Role.CONDUCTOR, Mode.EXPORT, None, {"role": "conductor"})
    _negotiated(_target(out, identity, ledger), first_job)
    (old,) = move_objects(_target(out, identity, ledger).blocks(), [noise(4000, 7)])

    again = _target(out, identity, ledger)
    again.open_job(first_job)
    assert old.object_id in again.blocks().all_manifests()

    second_job = job_context(mode=Mode.EXPORT)
    with pytest.raises(ConflictError):
        _target(out, identity, ledger).open_job(second_job)
    assert old.object_id in again.blocks().all_manifests(), "the refused export touched the running one"

    end_job(ledger, first_job.job_id, Role.CONDUCTOR, "aborted")
    second = _target(out, identity, ledger)
    final = _negotiated(second, second_job)
    assert second.blocks().all_manifests() == {}
    stage_and_prepare(second, second_job, final)
    assert second.start_commit(second_job).state == OpState.DONE
    manifest, _verified = read_manifest(out, trusted_pubkeys={identity.public_b64()})
    assert manifest["job_id"] == second_job.job_id
    assert old.object_id not in manifest["objects"]


@pytest.mark.parametrize("what", ["file", "dir"])
def test_a_progress_file_others_could_write_is_not_taken_over(tmp_path, what):
    """★ The progress decides what gets signed (FinalPlan, title): a new instance takes it over only from a staging
    private to this user. With the state file (or the staging) readable and writable by others, it is ignored —
    the commit fails as not prepared instead of signing what the file says.

    Red proof: mutant = ``_own_private`` accepts every file → the commit succeeds.
    """
    if not hasattr(os, "geteuid"):
        pytest.skip("POSIX ownership check")
    ledger, identity = _exporter(tmp_path)
    out = tmp_path / "work.ctxh.tar"
    job = job_context(mode=Mode.EXPORT)
    first = _target(out, identity, ledger)
    stage_and_prepare(first, job, _negotiated(first, job))
    partial = tmp_path / f"{out.name}.partial"
    if what == "file":
        os.chmod(partial / STATE_FILE, 0o644)
    else:
        os.chmod(partial, 0o755)  # noqa: S103 - the staging others can enter is what the test refuses

    resumed = _target(out, identity, ledger)
    assert resumed.status(job).state == "open"
    op = resumed.start_commit(job)
    assert op.state == OpState.FAILED and op.error["code"] == "invalid_state", op
    assert not out.exists()


def test_open_job_refuses_a_staging_this_user_does_not_own(tmp_path, monkeypatch):
    """★ The staging must be this user's private directory before anything is written into it (a staging another
    user left in a shared directory cannot be removed and would receive the objects and progress that get signed).
    Simulated: the process runs under another uid than the one owning the staging.

    Red proof: mutant = no ownership check after ``mkdir_private`` → ``open_job`` writes into the staging.
    """
    if not hasattr(os, "geteuid"):
        pytest.skip("POSIX ownership check")
    ledger, identity = _exporter(tmp_path)
    out = tmp_path / "work.ctxh.tar"
    real = os.geteuid()
    monkeypatch.setattr(os, "geteuid", lambda: real + 4242)
    with pytest.raises(ConflictError):
        _target(out, identity, ledger).open_job(job_context(mode=Mode.EXPORT))
    assert not (tmp_path / f"{out.name}.partial" / STATE_FILE).exists()
