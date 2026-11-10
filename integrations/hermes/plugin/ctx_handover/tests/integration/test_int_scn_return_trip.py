"""Mandatory scenario 4 (layer 2, R4, R14, DESIGN §6.6, §9.5, §9.8; TESTPLAN E1/E3/E4): server → laptop → server.

The server's session works in ``~/proj`` with unsaved work (stash, index patch, modified and untracked files) and
ignored build output. The laptop pulls it, works on (one commit, one further uncommitted edit, one more turn) and
pushes it back. The server's repository is unchanged since the outgoing handover, so the return updates it in
place — although it still holds the work-in-progress that was handed away — instead of materialising a fresh
``-handover-`` directory; the ignored build output survives; the session comes back under its original title (no
``#2``) with the laptop's history and cwd ``~/proj``."""

from __future__ import annotations

from ctx_handover.engine.model import Mode, Options, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.integration._int3_helpers import arrival, repo_view, run_job, title_of, wip_repo
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks

TITLE = "Round trip"


def _handover_dirs(*roots) -> list[str]:
    out = []
    for root in roots:
        if root.is_dir():
            out += [str(p) for p in root.rglob("*-handover-*") if p.is_dir()]
    return out


def test_int_scn_return_trip_updates_origin_in_place(online):
    """★ Scenario 4.

    Mutant (red proof): ``LocalTarget.negotiate`` ignores the ``out`` row (``out_row = None``) — the return is not
    recognised and the repository lands in a fresh directory."""
    server, laptop, _listener, peer = online
    origin = server.home / "proj"
    wip_repo(origin)
    b = builder(server)
    sid = b.create(title=TITLE, cwd=str(origin))
    b.user(sid, "start the feature")
    b.assistant(sid, "started in src/main.py")

    # server -> laptop (pull)
    there = run_job(laptop, Mode.PULL, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    assert there.rc == 0, there.why()
    lap_tip = arrival(laptop, there.job_id)["session_id"]
    lap_dest = laptop.home / "hermes-handover" / "proj"
    assert title_of(laptop, lap_tip) == TITLE

    # offline work on the laptop: one commit, one further edit, one more turn
    lap_repo = RepoBuilder(lap_dest)
    lap_repo.write("src/feature.py", "def feature():\n    return 42\n")
    lap_repo.add("src/feature.py")
    lap_repo.commit("feature on the laptop")
    lap_repo.write("README.md", "# app\n\nedited offline on the laptop\n")
    lb = builder(laptop)
    lb.user(lap_tip, "add the feature")
    lb.assistant(lap_tip, "added src/feature.py and updated the README")
    expected = repo_view(lap_dest)

    # laptop -> server (push back)
    back = run_job(laptop, Mode.PUSH, peer, SessionRef(id=lap_tip), options=Options(refine="off", yes=True))
    assert back.rc == 0, back.why()
    new_tip = arrival(server, back.job_id)["session_id"]

    assert _handover_dirs(server.home) == [], "the return materialised a fresh -handover- directory"
    assert not (server.home / "hermes-handover" / "proj").exists(), "the return did not go to the origin path"
    got = repo_view(origin)
    assert got["head"] == expected["head"], "origin repository was not updated to the laptop's commit"
    assert got["worktree"] == expected["worktree"] and got["index"] == expected["index"]
    assert (origin / "build" / "out.bin").read_bytes() == b"\x00ignored build output\n", "ignored output lost"
    assert lineage_hashes(server.db(), new_tip) == lineage_hashes(laptop.db(), lap_tip)
    row = server.db().get_session(new_tip) or {}
    assert row.get("cwd") == str(origin)
    assert title_of(server, new_tip) == TITLE, "the title counted up over the round trip (R14)"
    assert not session_marks(server.db(), new_tip)["hidden"]
    assert session_marks(laptop.db(), lap_tip)["hidden"], "the laptop copy is not locked after the return"
