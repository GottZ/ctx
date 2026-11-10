"""Round G″ of cluster C and the
integration harness (N52):

5a. ``bundle._generic_facts`` — the declared facts of an unknown importer claim no checked workspace root
    (``workspace_root_ok`` None, no problem code): the exporter's own R29 check result is not the importer's;
5b. an ``OSError`` while the bundle is written (disk full, no permission) — in the commit and while blocks are staged
    — never leaves the conductor's thread: the operation fails with a non-repeatable error, the job ends ABORTED,
    the ``.partial`` is removed and the source session is free again;
6.  ``harness.arrived_session`` names the tip of a multi-segment lineage (and its root), as the commit ack does.
"""

from __future__ import annotations

import errno
import time
from dataclasses import replace
from pathlib import Path

import pytest

from ctx_handover.engine import bundle as bundle_mod
from ctx_handover.engine import jobs
from ctx_handover.engine.bundle import BundleTarget
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import (
    CapsuleInfo,
    Include,
    ItemKind,
    ItemRole,
    Mode,
    OpState,
    Options,
    Origin,
    PackInfo,
    PlanItem,
    PrepareRequest,
    RefineReport,
    ResourcePlan,
    Role,
    SessionRef,
)
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.sessiondb import SessionBuilder
from ctx_handover.tests.integration._int2_helpers import ack_ids
from ctx_handover.tests.integration.harness import Home, arrived_session, session_marks
from ctx_handover.tests.unit.A._l2r_helpers import OFF, dirty_repo, errors, session_in, state
from ctx_handover.tests.unit.C.helpers import _NullReporter, sample_facts
from ctx_handover.tests.unit.C.test_bundle import Exporter, _move, _noise

M = __import__("ctx_handover.engine.model", fromlist=["m"])


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    yield Home(tmp_path, "server"), Home(tmp_path, "laptop")
    jobs._reset_for_tests()


def _job() -> JobContext:
    return JobContext(
        job_id=ulid(),
        mode=Mode.EXPORT,
        peer=None,
        options=Options(),
        epoch=1,
        reporter=_NullReporter(),
        cancelled=lambda: False,
        created_at=time.time(),
    )


def _plan(job: JobContext) -> ResourcePlan:
    item = PlanItem(
        item_id="i1",
        kind=ItemKind.FILE_TREE,
        source_path="/w/proj",
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=10,
    )
    return ResourcePlan(
        job_id=job.job_id,
        revision=(1, 2),
        items=(item,),
        not_transferred=(),
        processes=(),
        delegations=(),
        cron=(),
        refine=RefineReport(used="off"),
        tools_needed=(),
        env_vars_needed=(),
        notes_for_target="",
        approx_tokens=5,
    )


def _prepared(tmp_path, *, as_dir: bool) -> tuple[BundleTarget, JobContext, Path]:
    """A ``BundleTarget`` with staged capsule and pack, prepared — the next step is ``start_commit``."""
    exp = Exporter(tmp_path)
    out = tmp_path / ("out.d" if as_dir else "out.ctxh.tar")
    target = BundleTarget(out, exp.identity, as_dir=as_dir, declared_facts=None, ledger=exp.ledger)
    job = _job()
    target.open_job(job)
    facts = sample_facts(M, "server")
    target.facts(job, facts)
    final = target.negotiate(job, _plan(job))
    capsule_obj = exp.source.put_bytes(_noise(3000, 1), "application/json")
    pack_obj = exp.source.put_bytes(_noise(2000, 2), "application/json")
    _move(exp.source, target.blocks(), [capsule_obj.object_id, pack_obj.object_id])
    capsule = CapsuleInfo(
        capsule_id=capsule_obj.object_id,
        size=capsule_obj.size,
        segments=1,
        rows=3,
        max_segment_bytes=capsule_obj.size,
        approx_tokens=5,
        h_all="ha",
        h_rows="hr",
        h_model="hm",
        revision=(1, 2),
    )
    assert target.start_stage_context(job, capsule).state is OpState.DONE
    req = PrepareRequest(
        capsule=capsule,
        pack=PackInfo(pack_id=pack_obj.object_id, size=pack_obj.size),
        final_plan_id=final.plan_id,
        source_facts=facts,
        note_parts={"title": "My session"},
    )
    assert target.start_prepare(job, req).state is OpState.DONE
    return target, job, out


def _partial(out: Path) -> Path:
    return out.parent / f"{out.name}.partial"


# ---------------------------------------------------------------------------------------------------------
# 5a. generic facts of an unknown importer
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(("ok", "problem"), [(False, "denied"), (True, ""), (False, "check_failed")])
def test_generic_bundle_facts_claim_no_checked_workspace_root(tmp_path, ok, problem):
    """★ (R29): the facts a bundle declares for its unknown importer say "not checked"
    (``workspace_root_ok is None``, ``workspace_root_problem == ""``) — the exporter's own check (usable or not) is
    not the importer's, and the bundle stores no claim about a root nobody probed.

    Red (on 16698398): ``assert False is None`` — ``_generic_facts`` copied the exporter's flag with ``replace``.
    """
    exp = Exporter(tmp_path)
    target = BundleTarget(tmp_path / "x.ctxh.tar", exp.identity, as_dir=False, declared_facts=None, ledger=exp.ledger)
    job = _job()
    target.open_job(job)
    src = replace(sample_facts(M, "server"), workspace_root_ok=ok, workspace_root_problem=problem)
    facts = target.facts(job, src)
    assert facts.workspace_root_ok is None and facts.workspace_root_problem == "", facts
    assert facts.instance_name == "bundle"


def test_generic_bundle_facts_claim_no_context_window(tmp_path):
    """★ (R32): the facts a bundle declares for its unknown importer claim no context window — the
    exporter's own ``host.context_length`` says nothing about the importer's model, and copying it made an export
    without ``--for`` ask (and end ``cancel`` under ``--yes`` without a terminal) against the exporter's window. The
    importer checks the window against its real facts when it imports.

    Red (before the fix): ``assert 3000 is None`` — ``_generic_facts`` copied ``host`` unchanged.
    """
    exp = Exporter(tmp_path)
    target = BundleTarget(tmp_path / "x.ctxh.tar", exp.identity, as_dir=False, declared_facts=None, ledger=exp.ledger)
    job = _job()
    target.open_job(job)
    base = sample_facts(M, "server")
    src = replace(base, host=replace(base.host, context_length=3000))
    facts = target.facts(job, src)
    assert facts.host.context_length is None, facts.host


# ---------------------------------------------------------------------------------------------------------
# 5b. OSError while the bundle is written
# ---------------------------------------------------------------------------------------------------------


def _enospc(*_a, **_kw):
    raise OSError(errno.ENOSPC, "No space left on device")


def test_disk_full_in_the_commit_is_a_failed_operation(tmp_path, monkeypatch):
    """★ ``ENOSPC`` while the tar is written — ``start_commit`` returns a FAILED operation
    (``insufficient_storage``, not repeatable, naming the folder) instead of raising; the target stays before its
    commit (the conductor's ``commit_failed`` path aborts it) and ``abort`` leaves neither ``.partial`` nor bundle.

    Red (on 16698398): ``OSError: [Errno 28] No space left on device`` raised out of ``start_commit``.
    """
    target, job, out = _prepared(tmp_path, as_dir=False)
    monkeypatch.setattr(BundleTarget, "_write_tar", _enospc)
    op = target.start_commit(job)
    assert op.state is OpState.FAILED, op
    assert op.error["code"] == "insufficient_storage" and op.error["retryable"] is False, op.error
    assert op.error["details"]["path"] == str(out.parent), op.error
    assert target.status(job).state == "prepared"
    target.abort(job, "commit_failed")
    assert not _partial(out).exists() and not out.exists()
    assert not (out.parent / f"{out.name}.tmp").exists()


def test_permission_error_in_the_commit_while_the_state_cannot_be_saved_either(tmp_path, monkeypatch):
    """★ ``EACCES`` on the rename of a directory bundle, and the staging's progress file cannot be
    written either (the same broken disk) — still a FAILED operation (``internal``, not repeatable, naming the
    bundle), never an exception; a second ``start_commit`` answers the same way.

    Red (on 16698398): ``PermissionError: [Errno 13]`` raised out of ``start_commit``.
    """
    target, job, out = _prepared(tmp_path, as_dir=True)
    real_rename = bundle_mod.os.rename

    def no_rename(src, dst, *a, **kw):
        if Path(dst) == out:
            raise PermissionError(errno.EACCES, "Permission denied", str(dst))
        return real_rename(src, dst, *a, **kw)

    real_write = bundle_mod.write_private

    def no_state(path, data, tmp):
        if Path(path).name == bundle_mod.STATE_FILE:
            raise OSError(errno.EIO, "Input/output error", str(path))
        return real_write(path, data, tmp)

    monkeypatch.setattr(bundle_mod.os, "rename", no_rename)
    monkeypatch.setattr(bundle_mod, "write_private", no_state)
    for _ in range(2):
        op = target.start_commit(job)
        assert op.state is OpState.FAILED, op
        assert op.error["code"] == "internal" and op.error["retryable"] is False, op.error
        assert str(out) in op.error["message"], op.error
    assert not out.exists()


def test_disk_full_while_blocks_are_staged_is_a_handover_error(tmp_path, monkeypatch):
    """★ (same disk): the export's block sink (``_DirSink``) raises ``insufficient_storage`` (a
    ``HandoverError`` the conductor handles) for ``ENOSPC``, not a bare ``OSError``.

    Red (on 16698398): ``OSError: [Errno 28]`` from ``write_blocks``.
    """
    from ctx_handover.engine.errors import InsufficientStorage

    exp = Exporter(tmp_path)
    target = BundleTarget(tmp_path / "x.ctxh.tar", exp.identity, as_dir=False, declared_facts=None, ledger=exp.ledger)
    job = _job()
    target.open_job(job)
    target.facts(job, sample_facts(M, "server"))
    obj = exp.source.put_bytes(_noise(5000, 3), "application/json")
    monkeypatch.setattr(bundle_mod, "write_private", _enospc)
    with pytest.raises(InsufficientStorage) as caught:
        target.blocks().write_blocks((b, exp.source.get(b)) for b in obj.blocks)
    assert caught.value.retryable is False
    with pytest.raises(InsufficientStorage):
        target.blocks().put_manifests(exp.source.manifests([obj.object_id]))


def _export(laptop: Home, sid: str, out: Path) -> tuple[str, int]:
    with laptop.active():
        runner = jobs.runner_for(None)
        job_id, final = runner.plan_sync(
            Mode.EXPORT,
            None,
            OFF,
            session_ref=SessionRef(id=sid),
            calling_session=None,
            session_key=None,
            bundle_path=out,
        )
        assert final is not None, errors(laptop, job_id)
        return job_id, runner.run_foreground(job_id)


@pytest.mark.parametrize("where", ["commit-tar", "commit-manifest", "staging-blocks"])
def test_export_whose_bundle_cannot_be_written_ends_aborted_and_frees_the_source(homes, monkeypatch, where):
    """★ On the real stack (``plan_sync`` + ``run_foreground``, the CLI path): the disk fills while the
    tar is written, the manifest cannot be written (``EACCES``), or the disk fills while the blocks are staged. The
    run returns (no exception leaves the conductor), the job ends ABORTED with the error recorded, the commit is not
    repeated, no ``.partial``/bundle stays, the source session is released and usable.

    Red (on 16698398): ``OSError``/``PermissionError`` raised out of ``run_foreground`` (the conductor's ``_step``
    handles only ``HandoverError``; ``JobRunner._run`` too).
    """
    _server, laptop = homes
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    sid = session_in(laptop, repo.root, "Full disk")
    out = laptop.root / "out" / "full.ctxh.tar"
    out.parent.mkdir(parents=True)
    real_write = bundle_mod.write_private
    if where == "commit-tar":
        monkeypatch.setattr(BundleTarget, "_write_tar", _enospc)
    elif where == "commit-manifest":

        def no_manifest(path, data, tmp):
            if Path(path).name == "manifest.json":
                raise PermissionError(errno.EACCES, "Permission denied", str(path))
            return real_write(path, data, tmp)

        monkeypatch.setattr(bundle_mod, "write_private", no_manifest)
    else:

        def full_blocks(path, data, tmp):
            if Path(path).parent.parent.name == "blocks":
                _enospc()
            return real_write(path, data, tmp)

        monkeypatch.setattr(bundle_mod, "write_private", full_blocks)
    job_id, rc = _export(laptop, sid, out)
    events = laptop.ledger().events(job_id, Role.CONDUCTOR)
    states = [e["state"] for e in events if e["kind"] == "transition"]
    assert state(laptop, job_id) == "aborted" and rc != 0, (states, errors(laptop, job_id))
    assert states.count("committing") <= 1, states
    row = laptop.ledger().job(job_id, Role.CONDUCTOR)
    code = (row["last_error"] or {}).get("code")
    assert code == ("internal" if where == "commit-manifest" else "insufficient_storage"), row["last_error"]
    assert not _partial(out).exists() and not out.exists()
    assert laptop.ledger().source_state(job_id)["state"] == "released"
    marks = session_marks(laptop.db(), sid)
    assert not marks["archived"] and not marks["hidden"] and marks["end_reason"] is None, marks


# ---------------------------------------------------------------------------------------------------------
# 6. harness.arrived_session (N52)
# ---------------------------------------------------------------------------------------------------------


def test_harness_arrived_session_names_the_tip_of_a_multi_segment_lineage(homes):
    """N52: a two-segment lineage (rotated session) exported on the server and imported on the laptop
    arrives as two arrival rows (one per segment, both naming the root). ``arrived_session`` gives ``(root, tip)``
    exactly as the conductor's commit ack does.

    Red (on 16698398): ``assert (root, root) == (root, tip)`` — the first arrival row (the root segment) was taken.
    """
    server, laptop = homes
    server.identity()
    laptop.identity()
    repo = dirty_repo(server.home / "proj")
    b = SessionBuilder(server.db(), clock=FakeClock())
    root = b.create(title="Two segments", cwd=str(repo.root))
    b.turns(root, 4)
    tip = b.rotate(root, keep_last=2)
    b.turns(tip, 2)
    out = server.root / "out" / "two.ctxh.tar"
    out.parent.mkdir(parents=True)
    job_e, rc_e = server.run(Mode.EXPORT, None, SessionRef(id=tip), options=OFF, bundle_path=out)
    assert rc_e == 0, errors(server, job_e)
    job_i, rc_i = laptop.run(
        Mode.IMPORT, None, SessionRef(), options=replace(OFF, trust_unsigned=True), bundle_path=out
    )
    assert rc_i == 0, errors(laptop, job_i)
    rows = [r for r in laptop.ledger().arrival_rows() if r["job_id"] == job_i]
    assert len(rows) == 2, rows
    new_root, new_tip = ack_ids(laptop, job_i)
    assert new_root and new_tip and new_root != new_tip, (new_root, new_tip)
    assert arrived_session(laptop, job_i) == (new_root, new_tip)
    assert arrived_session(laptop, "01JC0000000000000000NOJOB0") == (None, None)


def test_bundle_export_without_space_names_this_machine(tmp_path, monkeypatch):
    """★ (wishes N94): a bundle export that lacks space on this machine says so — the error carries
    ``side="This machine"``, so the message does not blame "the other gateway".

    Red (before the fix): ``assert None == 'This machine'`` — ``_negotiated`` raised without ``side``.
    """
    from ctx_handover.engine.errors import InsufficientStorage

    exp = Exporter(tmp_path)
    target = BundleTarget(tmp_path / "x.ctxh.tar", exp.identity, as_dir=False, declared_facts=None, ledger=exp.ledger)
    job = _job()
    target.open_job(job)
    monkeypatch.setattr(bundle_mod.shutil, "disk_usage", lambda _p: type("U", (), {"free": 1})())
    target.facts(job, sample_facts(M, "server"))
    with pytest.raises(InsufficientStorage) as info:
        target.negotiate(job, _plan(job))
    assert info.value.details.get("side") == "This machine", info.value.details
