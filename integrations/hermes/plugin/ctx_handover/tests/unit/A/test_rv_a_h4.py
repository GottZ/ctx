"""Review round rv-a, finding H4 (``review-code/REVIEW.md`` §2; g1 H3, g2 M7): retention (BUILD-ORDER §6 Nr. 21,
"reports/events 30 days") covered only pairing codes and the journal.

* The source's pack work directory ``staging/<job>/git`` (temp repository with the changed and untracked work tree as
  loose objects) and the re-check scratch ``staging/<job>/recheck`` stayed after every handover. The source discards
  them when its hold ends (retire, release); the janitor removes what a reclaim or a crash left behind — the
  target's staging (quarantine, workspace steps) is never touched by it.
* Note reports (``reports/<job>.md``: paths, LLM text) never expired.
* ``suggestions``, expired ``offers`` (LLM refinement), finished ``ctx_outbox`` rows and the ``backups`` of given-back
  sources (goal texts, titles) were never deleted."""

from __future__ import annotations

import os
from pathlib import Path

from ctx_handover.engine import jobs, local_source, local_target
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import JOURNAL_KEEP_S, SUGGESTION_TTL_S, Ledger
from ctx_handover.engine.model import CommitAck, DestMode, Mode, OpState, PathMapEntry, PreparedInfo, Role, SessionInfo
from ctx_handover.tests.unit.A.test_l2_source_real import mk  # noqa: F401  (fixture)

DAY = 86400.0


def _prepared(capsule: dict) -> PreparedInfo:
    return PreparedInfo("n", "n", "T", capsule["h_all"], capsule["h_rows"], capsule["h_model"], "A", (), (), ())


def _packed(env, *, big: bool = False):
    if big:  # untracked work in progress: lands as loose objects in the pack's temp repository
        (env.repo.root / "big.bin").write_bytes(os.urandom(1024 * 1024))
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    capsule = env.source.start_export(job).result
    st = env.source.start_pack(job, env.final(plan))
    assert st.state is OpState.DONE, st
    staging = env.data / "staging" / job.job_id
    assert (staging / "git").is_dir()  # the pack really used its work directory
    return job, capsule, staging


def _source_dirs(staging: Path) -> list[str]:
    """The source's own directories under ``staging/<job>`` (``_pack`` work directory, ``_git_states`` scratch)."""
    return sorted(n for n in ("git", "recheck") if (staging / n).exists())


def test_rv_a_h4_retire_discards_the_source_staging(mk):  # noqa: F811
    env = mk(remote_driven=False)
    job, capsule, staging = _packed(env, big=True)
    assert env.source.start_prepare(job, _prepared(capsule)).result["outcome"] == "ok"
    ack = CommitAck(
        "n", "n", "T", (("w0", "/x", DestMode.FRESH),), (PathMapEntry("/a", "/b", "workspace"),), "m", True, (), 1.0
    )

    st = env.source.start_retire(job, ack)

    assert st.state is OpState.DONE and st.result["state"] == "retired", st
    assert _source_dirs(staging) == []
    assert not staging.exists()


def test_rv_a_h4_release_discards_the_source_staging(mk):  # noqa: F811
    env = mk(remote_driven=False)
    job, _capsule, staging = _packed(env)

    st = env.source.start_release(job, "cancelled")

    assert st.state is OpState.DONE and st.result["state"] == "released", st
    assert _source_dirs(staging) == []


def test_rv_a_h4_janitor_purge_takes_only_the_source_staging_of_finished_jobs(mk):  # noqa: F811
    env = mk(remote_driven=False)
    left, _c1, left_staging = _packed(env)
    assert env.source.start_release(left, "cancelled").result["state"] == "released"
    # a process that died between the release and its clean-up (or a reclaim, ``lock.reclaim``) left it behind
    (left_staging / "git" / "w0").mkdir(parents=True)
    # a target staging under the same job id (both sides in one data folder) is not the source's to remove
    (left_staging / "quarantine").mkdir()
    (left_staging / "quarantine" / "kept.pack").write_bytes(b"target data")
    _running, _c2, running_staging = _packed(env)  # the next handover of the same session, packed and locked
    # the target side of another job keeps its staging (quarantine of a failed inverse, DESIGN §3.4)
    target_job = ulid()
    env.ledger.create_job(target_job, Role.TARGET, Mode.PUSH, None, {"role": "target"})
    quarantine = env.data / "staging" / target_job / "quarantine" / "w0"
    quarantine.mkdir(parents=True)
    (quarantine / "kept.pack").write_bytes(b"user data")
    orphan = env.data / "staging" / ulid() / "git"
    orphan.mkdir(parents=True)

    n = local_source.purge_source_staging(env.ledger, env.data, lock_dir=env.data / "locks")

    assert n == 2
    assert _source_dirs(left_staging) == []
    assert (left_staging / "quarantine" / "kept.pack").read_bytes() == b"target data"
    assert not orphan.parent.exists()
    assert (quarantine / "kept.pack").read_bytes() == b"user data"
    assert _source_dirs(running_staging) == ["git", "recheck"]  # still locked: its pack work directory stays


def test_rv_a_h4_janitor_purge_waits_for_a_held_job_lock(mk):  # noqa: F811
    from ctx_handover.engine.ops import JobLock

    env = mk(remote_driven=False)
    job, _capsule, staging = _packed(env)
    env.ledger.source_set(job.job_id, "released")
    held = JobLock(env.data / "locks" / f"{job.job_id}.lock")
    assert held.acquire()  # an operation of the job (pack after a resume) runs right now
    try:
        assert local_source.purge_source_staging(env.ledger, env.data, lock_dir=env.data / "locks") == 0
        assert _source_dirs(staging) == ["git", "recheck"]
    finally:
        held.release()
    assert local_source.purge_source_staging(env.ledger, env.data, lock_dir=env.data / "locks") == 1


def _target_row(ledger: Ledger, job_id: str, state: str) -> None:
    ledger.create_job(job_id, Role.TARGET, Mode.PUSH, None, {"role": "target"})
    epoch = ledger.claim_owner(job_id, Role.TARGET, "me", 60)
    ledger.transition(job_id, Role.TARGET, "*", state, {}, epoch=epoch)


def test_rv_a_h4_reports_expire_after_the_retention(tmp_path, clock):
    ledger = Ledger(tmp_path / "data" / "ledger.sqlite3", clock=clock)
    reports = tmp_path / "data" / "reports"
    reports.mkdir()
    old_done, old_active, fresh_done = ulid(), ulid(), ulid()
    _target_row(ledger, old_done, "committed")
    _target_row(ledger, old_active, "prepared")
    _target_row(ledger, fresh_done, "aborted")
    for job_id in (old_done, old_active, fresh_done):
        (reports / f"{job_id}.md").write_text("# report\n/home/me/proj\n", encoding="utf-8")
    old = clock() - JOURNAL_KEEP_S - DAY
    for job_id in (old_done, old_active):
        os.utime(reports / f"{job_id}.md", (old, old))
    os.utime(reports / f"{fresh_done}.md", (clock() - DAY, clock() - DAY))
    (reports / "notes.txt").write_text("not a report", encoding="utf-8")
    os.utime(reports / "notes.txt", (old, old))

    n = local_target.purge_reports(ledger, tmp_path / "data", keep_s=JOURNAL_KEEP_S)

    assert n == 1
    assert sorted(p.name for p in reports.iterdir()) == sorted([f"{old_active}.md", f"{fresh_done}.md", "notes.txt"])
    ledger.close()


def _info(root: str) -> SessionInfo:
    return SessionInfo(root, root, (root,), "t", None, None, None, None, None, 1, (1,))


def test_rv_a_h4_ledger_retention_purges_the_unbounded_tables(tmp_path, clock):
    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    # suggestions: one from long ago, one from now (the first is past its TTL long before the purge)
    ledger.suggestion_put("s-old", {"path": "/old"})
    # offers: expired and unclaimed; claimed by a job that is still running; fresh
    ledger.offer_put("off-old", "peer", "s1", "note", {"add": [["/x", "LLM text"]]}, clock() + 600)
    live_job = ulid()
    ledger.offer_put("off-live", "peer", "s2", None, {"add": []}, clock() + 600)
    ledger.offer_claim("off-live", "peer", live_job)
    ledger.create_job(live_job, Role.CONDUCTOR, Mode.PULL, "peer", {"mode": "pull"})
    # outbox: delivered long ago; still pending
    ledger.outbox_put("departure", {"title": "old"})
    ledger.outbox_put("departure", {"title": "pending"})
    first = ledger.outbox_due(clock() + 1, 10)[0]
    ledger.outbox_done(first["id"], True, clock())
    # backups: a released source (restored long ago), a retired source (reclaim needs them), a released source whose
    # conductor is suspended (resume re-locks it)
    released, retired, resume = ulid(), ulid(), ulid()
    for job_id, root, final in ((released, "r1", "released"), (retired, "r2", "retired"), (resume, "r3", "released")):
        ledger.source_lock(job_id, _info(root), "holder", None)
        ledger.backup_put(job_id, "title:" + root, "Original title")
        ledger.backup_put(job_id, "step:p1", "done")
        ledger.source_set(job_id, final)
    ledger.create_job(resume, Role.CONDUCTOR, Mode.PUSH, None, {"mode": "push"})
    epoch = ledger.claim_owner(resume, Role.CONDUCTOR, "me", 60)
    ledger.transition(resume, Role.CONDUCTOR, "*", "suspended", {}, epoch=epoch)

    clock.advance(JOURNAL_KEEP_S + DAY)
    ledger.suggestion_put("s-new", {"path": "/new"})
    ledger.offer_put("off-new", "peer", "s3", None, None, clock() + 600)

    did = ledger.purge_retention(JOURNAL_KEEP_S)

    assert did == {"suggestions": 1, "offers": 1, "outbox": 1, "backups": 2}
    assert ledger.suggestions("s-new", clock()) != []
    assert ledger.offer_get("off-old") is None
    assert ledger.offer_get("off-live") is not None and ledger.offer_get("off-new") is not None
    assert [r["payload"]["title"] for r in ledger.outbox_due(clock() + 10**9, 10)] == ["pending"]
    assert ledger.backups(released) == []
    assert ledger.backups(retired) != []
    assert ledger.backups(resume) != []
    assert SUGGESTION_TTL_S < JOURNAL_KEEP_S
    ledger.close()


def test_rv_a_h4_janitor_runs_the_retention(mk, clock):  # noqa: F811
    """The janitor (every process start, before every job) applies all of it (BUILD-ORDER §6 Nr. 21)."""
    env = mk(remote_driven=False)
    job, _capsule, staging = _packed(env)
    env.ledger.source_set(job.job_id, "reclaimed")
    reports = env.data / "reports"
    reports.mkdir(exist_ok=True)
    report = reports / f"{ulid()}.md"
    report.write_text("# report\n", encoding="utf-8")
    old = clock() - JOURNAL_KEEP_S - DAY
    os.utime(report, (old, old))
    env.ledger.suggestion_put("s-old", {"path": "/old"})
    clock.advance(SUGGESTION_TTL_S + 1)

    did = jobs.janitor(env.ledger, None, None, flush_outbox=None, background=False, data_dir=env.data)

    assert _source_dirs(staging) == []
    assert not report.exists()
    assert did["retention"]["suggestions"] == 1
