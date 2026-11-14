"""Mandatory scenarios 1 and 2 (layer 2, BRIEF D7/D10, DESIGN §6.9, §9.3, §9.5): a session with a git working
directory holding every kind of unsaved work — stash entry, staged change (index patch) with a further work-tree
change on top, an untracked non-ignored file, ignored build output — is pushed from the laptop to the server and
pulled by the laptop from the server over the real listener process. Visible results: the session continues at the
target with the same model history (H_all/H_rows/H_model), the repository at the target has the same HEAD, branch,
index, stash and non-ignored work-tree bytes, ignored files stayed behind, the source repository was not touched,
and the source session carries the primary lock."""

from __future__ import annotations

from ctx_handover.engine.model import Mode, Options, SessionRef
from ctx_handover.tests.fakes.repo_builder import dir_fingerprint
from ctx_handover.tests.integration._int3_helpers import (
    arrival,
    present,
    repo_view,
    run_job,
    session_in,
    wip_repo,
)
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks


def _assert_moved(src_home, src_sid, dst_home, res, dest, view, fingerprint, src_root):
    assert res.rc == 0, res.why()
    row = arrival(dst_home, res.job_id)
    assert row is not None, "no arrival row on the target"
    tip = row["session_id"]
    src_db, dst_db = src_home.db(), dst_home.db()
    assert lineage_hashes(dst_db, tip) == lineage_hashes(src_db, src_sid), "model history differs at the target"
    assert not session_marks(dst_db, tip)["hidden"]
    assert (dst_db.get_session(tip) or {}).get("cwd") == str(dest), "session cwd is not the target workspace (R11)"
    got = repo_view(dest)
    assert got["head"] == view["head"] and got["branch"] == view["branch"]
    assert got["index"] == view["index"], "index (staged change) differs"
    assert got["stash"] == view["stash"], "stash differs"
    assert got["worktree"] == view["worktree"], "non-ignored work-tree bytes differ"
    assert present(dest, "build", "build/out.bin", "debug.log") == {
        "build": False,
        "build/out.bin": False,
        "debug.log": False,
    }, "ignored files travelled"
    assert dir_fingerprint(src_root) == fingerprint, "the source repository was modified by the handover"
    marks = session_marks(src_db, src_sid)
    assert marks["ended"] and marks["archived"] and marks["hidden"], marks


def test_int_scn_push_git_workdir_with_unsaved_work(online):
    """★ Scenario 1: push of a session whose git working directory holds a stash entry, an index patch, a modified
    and an untracked file (plus ignored files that must stay behind).

    Mutant (red proof): ``gitapply._store_stash`` returns without storing the stash entries at the target."""
    server, laptop, _listener, peer = online
    repo = wip_repo(laptop.home / "proj")
    view = repo_view(repo.root)
    assert len(view["stash"]) == 1
    fingerprint = dir_fingerprint(repo.root)
    sid = session_in(laptop, repo.root, "Push with unsaved work")
    res = run_job(laptop, Mode.PUSH, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    dest = server.home / "hermes-handover" / "proj"
    _assert_moved(laptop, sid, server, res, dest, view, fingerprint, repo.root)


def test_int_scn_pull_git_workdir_with_unsaved_work(online):
    """Scenario 2: the same constellation on the server, pulled by the laptop (the laptop dials the server)."""
    server, laptop, _listener, peer = online
    repo = wip_repo(server.home / "proj")
    view = repo_view(repo.root)
    fingerprint = dir_fingerprint(repo.root)
    sid = session_in(server, repo.root, "Pull with unsaved work")
    res = run_job(laptop, Mode.PULL, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    dest = laptop.home / "hermes-handover" / "proj"
    _assert_moved(server, sid, laptop, res, dest, view, fingerprint, repo.root)
