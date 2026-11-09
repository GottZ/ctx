"""Package i-f23 (lab finding L3-F23, BRIEF D10, R4, R19, R29): a conflict found while the target PREPAREs an item in
update or return mode sends the item to a fresh directory under ``workspace_root`` — never beside a destination
that lies outside ``allow_roots``, which the target policy refuses (``not_allowed_root``) and which ended the job
``aborted``.

Both tests run whole jobs through the real engine path (two homes, a real listener process; the harness of
``tests/integration``), without Hermes. The obstacle is the one of finding B2: a file the destination's repository
ignores (``.git/info/exclude``) at a path the arriving state adds; it keeps its bytes.

Red before i-f23: ``('aborted', … 'fallback destination not allowed (not_allowed_root)' …)``."""

from __future__ import annotations

import re
from pathlib import Path

import pytest

from ctx_handover.engine import jobs, surfaces
from ctx_handover.engine.jsonutil import from_wire
from ctx_handover.engine.model import CommitAck, Mode, Options, Role, SessionRef
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.integration._int3_helpers import arrival, repo_view, run_job, session_in
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import Home, Listener, pair

FALLBACK = re.compile(r"^proj-handover-\d{8}-\d{6}$")
SERVER_NOTES = "SERVER-ONLY NOTES (ignored here)\n"
USER_DATA = "LAPTOP USER DATA (ignored here)\n"


@pytest.fixture
def online(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    server, laptop = Home(tmp_path, "server"), Home(tmp_path, "laptop")
    server.identity()
    listener = Listener(server)
    try:
        peer = pair(server, laptop, listener)
        yield server, laptop, listener, peer
    finally:
        listener.stop()
        jobs._reset_for_tests()


def _repo(root: Path) -> RepoBuilder:
    r = RepoBuilder.init(root)
    r.write("README.md", "# app\n")
    r.write("src/main.py", "print('v1')\n")
    r.add_all()
    r.commit("first")
    return r


def _exclude(repo: RepoBuilder, *patterns: str) -> None:
    info = repo.root / ".git" / "info"
    info.mkdir(exist_ok=True)
    (info / "exclude").write_text("".join(p + "\n" for p in patterns), encoding="utf-8")


def _fallbacks(*dirs: Path) -> list[Path]:
    return sorted(p for d in dirs if d.is_dir() for p in d.iterdir() if FALLBACK.match(p.name))


def _conductor_status(home: Home, job_id: str) -> dict:
    with home.active():
        return jobs.runner_for(None).status(job_id)["job"]


def _final_message(mode: Mode, status: dict, peer: str) -> str:
    """The final message as ``commands._final_text`` renders it from the job's status (ack + warnings)."""
    ack = from_wire(CommitAck, dict(status["ack"]))
    return surfaces.render_final(mode, ack, status["state"], peer, None, warnings=status.get("warnings") or ())


def test_i_f23_return_conflict_at_an_origin_outside_the_roots_lands_under_workspace_root(online):
    """★ L3-F23, return trip (R4): the server's ``~/proj`` lies outside ``allow_roots`` (``~/hermes-handover``) and
    ignores ``notes.txt``; the laptop pulls, commits, creates its own ``notes.txt`` and pushes back. PREPARE finds
    the server's ignored file in the way (B2) ⇒ the job ends DONE, the origin is byte-identical, the returned state
    lies in ``<workspace_root>/proj-handover-<stamp>`` (nothing beside the origin), and the acknowledgement and
    the final message rendered from it (folder, warning) name that folder.

    Red before i-f23: ``materialize`` built ``~/proj-handover-<stamp>`` beside the origin, the target policy refused
    it (``not_allowed_root``), the job ended ``aborted``."""
    server, laptop, _listener, peer = online
    origin = server.home / "proj"
    repo = _repo(origin)
    _exclude(repo, "notes.txt")
    repo.write("notes.txt", SERVER_NOTES)
    sid = session_in(server, origin, "Round trip")

    there = run_job(laptop, Mode.PULL, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    assert there.rc == 0, there.why()
    lap_tip = arrival(laptop, there.job_id)["session_id"]
    lap = RepoBuilder(laptop.home / "hermes-handover" / "proj")
    assert not (lap.root / "notes.txt").exists(), "an ignored file travelled"
    lap.write("src/feature.py", "def feature():\n    return 42\n")
    lap.add("src/feature.py")
    lap.commit("feature on the laptop")
    lap.write("notes.txt", "laptop notes\n")
    lb = builder(laptop)
    lb.user(lap_tip, "add the feature")
    lb.assistant(lap_tip, "added src/feature.py")
    expected = repo_view(lap.root)
    before = dir_fingerprint(origin)

    back = run_job(laptop, Mode.PUSH, peer, SessionRef(id=lap_tip), options=Options(refine="off", yes=True))
    assert (back.row.get("state"), back.rc) == ("done", 0), back.why()

    assert dir_fingerprint(origin) == before, "the origin was modified"
    assert (origin / "notes.txt").read_text() == SERVER_NOTES
    assert _fallbacks(server.home) == [], "a fresh directory was made beside the origin, outside allow_roots"
    root = server.home / "hermes-handover"
    (fresh,) = _fallbacks(root)
    got = repo_view(fresh)
    assert (got["head"], got["worktree"]) == (expected["head"], expected["worktree"])
    assert (fresh / "notes.txt").read_text() == "laptop notes\n"

    status = _conductor_status(laptop, back.job_id)
    ack = status["ack"] or {}
    assert [d[1] for d in ack.get("dests") or ()] == [str(fresh)], ack
    assert any(str(fresh) in w and str(origin) in w for w in ack.get("warnings") or ()), ack.get("warnings")
    note = str((arrival(server, back.job_id) or {}).get("note") or "")
    assert str(fresh) in note and "returned to its original place" not in note, note
    assert (server.db().get_session(ack["session_id"]) or {}).get("cwd") == str(fresh)
    final = _final_message(Mode.PUSH, status, "server")
    assert f"(session {ack['session_id']}, {fresh}, " in final and "materialised fresh at" in final, final


def test_i_f23_update_conflict_at_a_recorded_dest_outside_the_roots_lands_under_workspace_root(online):
    """★ L3-F23, update mode: the laptop pulled ``~/proj`` into ``~/work/proj`` (a ``--dest`` typed there, outside
    its ``allow_roots``) and ignores ``local.cfg`` there with data of its own; the server's next session brings a
    commit and an untracked ``local.cfg``. The second pull maps to the recorded ``~/work/proj`` (update mode),
    PREPARE finds the ignored file in the way ⇒ DONE, ``~/work/proj`` byte-identical, the state in
    ``<workspace_root>/proj-handover-<stamp>``, named in the acknowledgement and the final message (folder, warning).

    Red before i-f23: ``~/work/proj-handover-<stamp>`` refused (``not_allowed_root``), the job ``aborted``."""
    server, laptop, _listener, peer = online
    origin = server.home / "proj"
    repo = _repo(origin)
    first_sid = session_in(server, origin, "First visit")
    dest = laptop.home / "work" / "proj"
    first = run_job(
        laptop, Mode.PULL, peer, SessionRef(id=first_sid), options=Options(refine="off", yes=True, dest=str(dest))
    )
    assert first.rc == 0, first.why()
    lap = RepoBuilder(dest)
    assert lap.head() == repo.head()
    _exclude(lap, "local.cfg")
    lap.write("local.cfg", USER_DATA)
    before = dir_fingerprint(dest)

    repo.write("src/extra.py", "EXTRA = True\n")
    repo.add("src/extra.py")
    repo.commit("more work on the server")
    repo.write("local.cfg", "from the server\n")
    expected = repo_view(origin)
    second_sid = session_in(server, origin, "Second visit")
    second = run_job(laptop, Mode.PULL, peer, SessionRef(id=second_sid), options=Options(refine="off", yes=True))
    assert (second.row.get("state"), second.rc) == ("done", 0), second.why()

    assert dir_fingerprint(dest) == before, "the laptop's copy was modified"
    assert _fallbacks(dest.parent) == [], "a fresh directory was made beside ~/work/proj, outside allow_roots"
    (fresh,) = _fallbacks(laptop.home / "hermes-handover")
    got = repo_view(fresh)
    assert (got["head"], got["worktree"]) == (expected["head"], expected["worktree"])
    status = _conductor_status(laptop, second.job_id)
    ack = status["ack"] or {}
    assert [d[1] for d in ack.get("dests") or ()] == [str(fresh)], ack
    assert any(str(fresh) in w and str(dest) in w for w in ack.get("warnings") or ()), ack.get("warnings")
    note = str((arrival(laptop, second.job_id) or {}).get("note") or "")
    assert str(fresh) in note and "updated in place" not in note, note
    assert (laptop.db().get_session(ack["session_id"]) or {}).get("cwd") == str(fresh)
    final = _final_message(Mode.PULL, status, "server")
    assert f"(session {ack['session_id']}, {fresh}, " in final and "materialised fresh at" in final, final
    assert status["state"] == "done" and status["exit_code"] == 0
    with laptop.active():
        rows = jobs.runner_for(None).ledger.events(second.job_id, Role.CONDUCTOR)
    assert not [e for e in rows if e["kind"] == "error"], "the job journaled an error"
