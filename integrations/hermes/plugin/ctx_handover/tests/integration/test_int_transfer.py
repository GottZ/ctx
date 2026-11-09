"""Push and pull over the real listener process with a git working directory including uncommitted state
(TESTPLAN §3 ``test_int_http_push_loopback`` / ``test_int_http_pull_loopback``; layer 2 mandatory scenario)."""

from __future__ import annotations

from pathlib import Path

from ctx_handover.engine.model import Mode, Options, Role, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks


def dirty_repo(root: Path) -> RepoBuilder:
    r = RepoBuilder.init(root)
    r.write("README.md", "# app\n")
    r.write("src/main.py", "print('v1')\n")
    r.add_all()
    r.commit("first")
    r.write("src/main.py", "print('v2 staged')\n")
    r.add("src/main.py")
    r.write("src/main.py", "print('v3 in the work tree')\n")
    r.write("notes/todo.txt", "untracked work\n")
    return r


def session_in(home, cwd: Path, title: str) -> str:
    b = builder(home)
    sid = b.create(title=title, cwd=str(cwd))
    b.user(sid, "please change src/main.py")
    b.assistant(sid, "done, see src/main.py")
    return sid


def _assert_handed_over(src_home, src_sid, dst_home, job_id, dst_dest: Path, repo_digest):
    dst_led = dst_home.ledger()
    commit_rows = [r for r in dst_led.arrival_rows() if r["job_id"] == job_id]
    assert commit_rows, "no arrival row on the target"
    new_tip = commit_rows[0]["session_id"]
    src_db, dst_db = src_home.db(), dst_home.db()
    assert lineage_hashes(src_db, src_sid) == lineage_hashes(dst_db, new_tip)
    assert not session_marks(dst_db, new_tip)["hidden"]
    marks = session_marks(src_db, src_sid)
    assert marks["ended"] and marks["archived"] and marks["hidden"], marks
    assert tree_digest(dst_dest) == repo_digest
    return new_tip


def test_int_http_push_loopback(online):
    server, laptop, listener, peer = online
    repo = dirty_repo(laptop.home / "proj")
    digest = tree_digest(repo.root)
    head = repo.head()
    sid = session_in(laptop, repo.root, "Push me")
    job_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    row = laptop.ledger().job(job_id, Role.CONDUCTOR)
    errors = [e["data"] for e in laptop.ledger().events(job_id, Role.CONDUCTOR) if e["kind"] == "error"]
    assert rc == 0, (row["state"], errors)
    dest = server.home / "hermes-handover" / "proj"
    _assert_handed_over(laptop, sid, server, job_id, dest, digest)
    assert RepoBuilder(dest).head() == head
    assert b"src/main.py" in RepoBuilder(dest).status()
    assert laptop.ledger().source_rows(states=("retired",))


def test_int_http_pull_loopback(online):
    server, laptop, listener, peer = online
    repo = dirty_repo(server.home / "proj")
    digest = tree_digest(repo.root)
    sid = session_in(server, repo.root, "Pull me")
    job_id, rc = laptop.run(Mode.PULL, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    row = laptop.ledger().job(job_id, Role.CONDUCTOR)
    errors = [e["data"] for e in laptop.ledger().events(job_id, Role.CONDUCTOR) if e["kind"] == "error"]
    assert rc == 0, (row["state"], errors)
    dest = laptop.home / "hermes-handover" / "proj"
    _assert_handed_over(server, sid, laptop, job_id, dest, digest)
    assert row["result"]["outcome"] == "done"
    pulled = server.ledger().pull_log(5)
    assert pulled and pulled[0]["job_id"] == job_id
