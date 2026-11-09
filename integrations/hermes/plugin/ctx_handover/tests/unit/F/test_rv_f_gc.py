"""Review round rv-f — hand-over of rv-a (H4, DESIGN §14 "staging GC now"): ``/handover gc`` without ``--job`` also
removes the source staging a finished hold left behind (``local_source.purge_source_staging``, the janitor's step),
not only the block cache; a hold that is not over keeps its directories."""

from __future__ import annotations

from ctx_handover.engine import jobs
from ctx_handover.engine.commands import Plugin
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Mode, Role
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx


def test_rv_f_gc_without_job_purges_finished_source_staging(fake_host):
    """★ rv-a hand-over: an orphan ``staging/<job>/git`` (no row left, e.g. after a reclaim and a crash) goes with a
    plain ``gc``; the staging of a source job that still holds is kept; the target's quarantine is never touched.

    Red before the fix: the orphan's ``git`` folder was still there after ``gc``."""
    jobs._reset_for_tests()
    prof = None
    try:
        backend = Plugin(FakeCtx(host=fake_host)).backend
        prof = backend._started()
        data = fake_host.plugin_data
        orphan, holding = ulid(), ulid()
        prof.ledger.create_job(holding, Role.SOURCE, Mode.PULL, None, {"name": "holding"})
        for jid in (orphan, holding):
            (data / "staging" / jid / "git" / "pack").mkdir(parents=True, exist_ok=True)
            (data / "staging" / jid / "git" / "pack" / "x").write_text("x", encoding="utf-8")
        quarantine = data / "staging" / orphan / "quarantine" / "w0"
        quarantine.mkdir(parents=True)
        result = backend.gc(job_id=None, all_jobs=False)
        assert not (data / "staging" / orphan / "git").exists(), "the finished hold's staging is still there"
        assert quarantine.exists(), "the target's quarantine of the same id must stay"
        assert (data / "staging" / holding / "git" / "pack" / "x").exists(), "a holding source lost its staging"
        assert result.get("staging") == 1, result
    finally:
        if prof is not None and getattr(prof, "veto", None) is not None:
            prof.veto.stop()
        jobs._reset_for_tests()
