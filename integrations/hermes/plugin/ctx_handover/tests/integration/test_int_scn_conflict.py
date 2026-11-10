"""Mandatory scenario 9 (layer 2, D10, DESIGN §9.5, §9.7, TESTPLAN E9): the target's copy of the repository has
local changes (a modified tracked file and an untracked file) when the next handover of that repository arrives.
Nothing at the target is overwritten: the repository there stays byte-identical (work tree and ``.git``); the
incoming state lands in ``<dest>-handover-<YYYYmmdd-HHMMSS>``, the session's cwd points there and the job reports
the conflict."""

from __future__ import annotations

import re

from ctx_handover.engine import jobs
from ctx_handover.engine.model import Mode, Options, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.integration._int3_helpers import arrival, repo_view, run_job, session_in, wip_repo
from ctx_handover.tests.integration.harness import lineage_hashes

FALLBACK = re.compile(r"^proj-handover-\d{8}-\d{6}$")


def test_int_scn_conflict_goes_to_fresh_directory(online):
    """★ Scenario 9: first push puts the repository at ``~/hermes-handover/proj`` on the server; the server's user
    edits it there; a second session on the same laptop repository is pushed ⇒ fresh directory, nothing overwritten.

    Red at ``a8fec516``: the fallback destination is built from the unexpanded
    ``~/hermes-handover`` and refused as ``not_allowed_root``."""
    server, laptop, _listener, peer = online
    repo = wip_repo(laptop.home / "proj", stash=False)
    first_sid = session_in(laptop, repo.root, "First visit")
    first = run_job(laptop, Mode.PUSH, peer, SessionRef(id=first_sid), options=Options(refine="off", yes=True))
    assert first.rc == 0, first.why()
    dest = server.home / "hermes-handover" / "proj"
    assert dest.is_dir()

    # the server's user works in the copy: a tracked edit and a new untracked file
    srv_repo = RepoBuilder(dest)
    srv_repo.write("README.md", "# app\n\nlocal edit on the server\n")
    srv_repo.write("server-notes.txt", "do not lose me\n")
    before = dir_fingerprint(dest)

    # meanwhile on the laptop: a commit and a second session on the same repository
    repo.write("src/extra.py", "EXTRA = True\n")
    repo.add("src/extra.py")
    repo.commit("more work on the laptop")
    expected = repo_view(repo.root)
    second_sid = session_in(laptop, repo.root, "Second visit")
    second = run_job(laptop, Mode.PUSH, peer, SessionRef(id=second_sid), options=Options(refine="off", yes=True))
    assert second.rc == 0, second.why()

    assert dir_fingerprint(dest) == before, "the server's copy with local changes was modified"
    fresh = [p for p in dest.parent.iterdir() if FALLBACK.match(p.name)]
    assert len(fresh) == 1, sorted(p.name for p in dest.parent.iterdir())
    got = repo_view(fresh[0])
    assert got["head"] == expected["head"] and got["worktree"] == expected["worktree"]
    tip = arrival(server, second.job_id)["session_id"]
    assert (server.db().get_session(tip) or {}).get("cwd") == str(fresh[0])
    assert lineage_hashes(server.db(), tip) == lineage_hashes(laptop.db(), second_sid)
    with laptop.active():
        status = jobs.runner_for(None).status(second.job_id)["job"]
    assert status["conflicts"], ("the conflict is not reported", status)
