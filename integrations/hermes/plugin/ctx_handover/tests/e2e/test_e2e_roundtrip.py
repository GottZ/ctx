"""Layer 3 — the round trip server → laptop → offline → server (TESTPLAN §4 E1–E4).

One chain on the paired stand (``conftest.lab``): E1 pulls a session with a dirty repository, a spilled tool
output, a todo call and an active goal from A to B; E2 works on B while A is down; E3 pushes the continued
session back; E4 checks the return of the repository into its original folder on A. Later steps skip with the
name of the missing earlier step when it did not run.

Runtime estimate: 4–7 min (E1 2–3 min incl. two gateway turns with tool calls, E2 1–2 min incl. stopping and
restarting gw-a/serve-a, E3/E4 1–2 min).
"""

from __future__ import annotations

import json
import re
import time
from pathlib import Path

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)

CHAIN: dict = {}


def _need(step: str) -> dict:
    if step not in CHAIN:
        pytest.skip(f"needs {step} of this chain, which did not complete")
    return CHAIN[step]


def _assert_retired_marks(lab, inst: str, sid: str, title: str, peer_hint: str | None = None) -> dict:
    marks = {s: lab.marks(inst, s) for s in lab.lineage(inst, sid)}
    for seg, m in marks.items():
        assert m["ended"] and m["end_reason"] == "handed_over", (seg, m)
        assert m["archived"] and m["hidden"], (seg, m)
        assert str(m["title"] or "").startswith(f"{title} (handed over to"), (seg, m)
        if peer_hint:
            assert peer_hint in str(m["title"]), (seg, m)
    return marks


def _assert_veto(lab, ev, inst: str, sid: str, tag: str) -> str:
    """A turn on the handed-over session returns the veto text and the fake model sees no request."""
    probe = f"{tag}-VETO-{_lab.uid()}"
    mark = lab.llm_mark()
    res = lab.chat_q(inst, sid, probe)
    out = res.stdout + "\n" + res.stderr
    ev.write(f"veto-{inst}.txt", f"rc={res.returncode}\n{out}")
    seen = [r for r in lab.llm_since(mark) if probe in json.dumps((r.get("body") or {}).get("messages") or [])]
    assert not seen, f"the fake model received the vetoed turn: {[r.get('seq') for r in seen]}"
    assert "handed over to" in out and "/handover reclaim" in out, out[-2000:]
    return out


@pytest.mark.e2e_timeout(900)
def test_e01_server_to_laptop_pull(lab, ev):
    """E1: pull of a session with dirty repo, spillover, todo and an active goal from A (server) to B (laptop).

    Pass criteria (TESTPLAN §4): B has a new session with H_all/H_rows/H_model equal to A's lineage and the same
    display history; the repository is identical (HEAD, branch, status with object ids, index, branches/tags,
    stash, work-tree bytes without the ignored ``build/``); the spillover file is copied byte-equal; the goal
    arrives **paused**. A's lineage is ended (``handed_over``), archived, hidden and renamed; a turn on it returns
    the veto text and the fake model gets no request. B's first turn carries the note (facts + one
    ``<untrusted-data>`` block with the refinement note) and a system prompt naming B's cwd.

    Deviations: the active goal is written as ``goal:<sid>`` through the public ``SessionDB.set_meta`` in the JSON
    form ``hermes_cli.goals.GoalState`` stores — ``/goal`` through the gateway would start goal-continuation turns
    against the fake model (the judge's answer is not scripted), which makes the source busy at random. No todo:
    the lab gateway's toolset has no todo tool (``todo_list`` → "Tool 'todo_list' does not exist", run 1). Files the
    terminal tool keeps under ``cache/terminal-output`` (``full_output_path`` in the transcript) are recorded in the
    evidence, not asserted (not one of the session-file kinds of DESIGN §6.11, L3-F10).
    """
    note = f"E01-NOTE-{_lab.uid()}"
    # The terminal tool keeps ≈ 59 500 characters of a long output (its capture window) and writes the rest to
    # cache/terminal-output; four such results in one turn exceed the 200 000-character turn budget, so Hermes
    # persists the largest ones to cache/spillover ("Full output saved to: …", tools/tool_result_storage.py:488).
    # ``seq`` needs no approval (``python3 -c`` is flagged as script execution and waits for one).
    tools = [{"name": "terminal", "arguments": {"command": f"seq {i} {40000 + i}"}} for i in range(1, 5)]
    src = lab.make_source("a", "e01proj", tool_turn=tools)
    goal = {"goal": f"ship the e01 feature {lab.run_id}", "status": "active", "turns_used": 0, "max_turns": 20}
    goal["created_at"] = time.time()
    with lab.db("a", write=True) as db:
        db.set_meta(f"goal:{src.sid}", json.dumps(goal))
    spills = lab.spill_files("a", src.sid)
    ev.write("source.json", {"sid": src.sid, "title": src.title, "repo": str(src.repo), "spills": spills})
    assert spills and all(p.is_file() for p in spills), "scaffold: the terminal tool call left no spillover file"
    before_a = lab.hashes("a", src.sid)
    repo_a = _lab.repo_state(src.repo)
    build_sha = _lab.file_sha(src.repo / "build" / "out.bin")
    ev.write("source-hashes.json", before_a)
    ev.write("source-repo.json", repo_a)

    with lab.llm_rules([_lab.refine_rule(note)]):
        res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
    ev.cli("take", res)
    assert res.rc == 0 and res.job_id, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write("job-b.json", status)
    job = status["job"]
    assert job.get("state") == "done", job
    new_sid = str((job.get("ack") or {}).get("session_id") or "")
    assert new_sid, job
    row_b = lab.row("b", new_sid)
    ev.write("row-b.json", row_b)
    dest = Path(str(row_b.get("cwd") or ""))
    CHAIN["e01"] = {
        "src": src,
        "b_sid": new_sid,
        "dest": dest,
        "job": res.job_id,
        "note": note,
        "repo_a": repo_a,
        "build_sha": build_sha,
    }

    # B: transcript
    after_b = lab.hashes("b", new_sid)
    ev.write("target-hashes.json", after_b)
    for key in ("h_all", "h_rows", "h_model", "model_norm", "display_norm", "rows"):
        assert after_b[key] == before_a[key], (key, before_a[key], after_b[key])
    assert not row_b.get("hidden") and row_b.get("ended_at") is None, row_b
    # B: workspace
    assert dest.is_dir() and dest != src.repo, row_b
    assert dest.is_relative_to(lab.handover_root("b")), (dest, lab.handover_root("b"))
    repo_b = _lab.repo_state(dest)
    ev.write("target-repo.json", repo_b)
    for key in ("head", "branch", "status", "index", "refs", "stash", "tree"):
        assert repo_b[key] == repo_a[key], key
    # B: session files and side state
    for spill in spills:
        copied = lab.home("b") / "cache" / "spillover" / spill.name
        assert copied.is_file() and _lab.file_sha(copied) == _lab.file_sha(spill), copied
    goal_b = json.loads(lab.meta("b", f"goal:{new_sid}") or "{}")
    ev.write("goal-b.json", goal_b)
    assert goal_b.get("status") == "paused" and goal_b.get("goal") == goal["goal"], goal_b
    terminal = {}
    for m in lab.messages("a", src.sid):
        for path in re.findall(r'"full_output_path": "([^"]+)"', _lab.message_text(m)):
            here = (
                lab.home("b") / Path(path).relative_to(lab.home("a")) if path.startswith(str(lab.home("a"))) else None
            )
            terminal[path] = {"on_b": str(here) if here else None, "exists_on_b": bool(here and here.exists())}
    ev.write("terminal-output-files.json", terminal)

    # A: the source is locked
    ev.write("source-marks.json", _assert_retired_marks(lab, "a", src.sid, src.title))
    _assert_veto(lab, ev, "a", src.sid, "E01")

    # B: the first turn gets the note and B's cwd in a freshly built system prompt
    probe = f"E01-FIRST-{_lab.uid()}"
    mark = lab.llm_mark()
    turn = lab.turn("b", new_sid, probe)
    ev.write("first-turn-b.json", turn.brief())
    assert turn.ok, turn.brief()
    recs = [r for r in lab.llm_since(mark) if r.get("kind") == "main" and probe in _lab.last_user_message(r)]
    assert recs, "the first turn on B reached no model"
    first = recs[0]
    user = _lab.last_user_message(first)
    assert _lab.NOTE_MARK in user and user.count(_lab.DATA_MARK) == 1 and note in user, user[-3000:]
    data_block = user[user.index(_lab.DATA_MARK) :]
    assert note in data_block and note not in user[: user.index(_lab.DATA_MARK)], "model text outside the data block"
    assert str(dest) in user, "the note names no path mapping to B's folder"
    assert str(dest) in _lab.system_prompt(first) and str(src.repo) not in _lab.system_prompt(first)


@pytest.mark.e2e_timeout(600)
def test_e02_offline_work_on_the_laptop(lab, ev):
    """E2: A stopped (gw-a and serve-a); two turns on B, an edit and one commit in B's copy; no request to A; the
    note was delivered once (it sits in exactly one stored user message and is not appended again).

    "No request to A" is observed by a tripwire listener bound to A's paired address (127.0.0.1:47111) while A is
    down: every connection attempt from B would be recorded. The commit is made with ``RepoBuilder`` (house rule:
    no git identity in shell commands or config); the two turns run through B's gateway."""
    c = _need("e01")
    dest, sid = c["dest"], c["b_sid"]
    lab.stop("gw-a", "serve-a")
    wire = _lab.Tripwire(47111)
    mark = lab.llm_mark()
    try:
        t1 = lab.turn("b", sid, "E02 offline turn one")
        rb = lab.repo(dest)
        rb.write("offline.txt", "written on the laptop while the server was off\n")
        rb.add("offline.txt")
        head = rb.commit("offline work on the laptop")
        t2 = lab.turn("b", sid, "E02 offline turn two")
        status = lab.handover("b", "status", timeout=180)
        ev.cli("status-b-offline", status)
    finally:
        wire.close()
        lab.start("serve-a", "gw-a")
    ev.write("turns.json", {"t1": t1.brief(), "t2": t2.brief(), "tripwire": wire.hits})
    assert t1.ok and t2.ok
    assert wire.hits == [], f"B contacted A's address {len(wire.hits)} times while A was down"
    recs = [r for r in lab.llm_since(mark) if r.get("kind") == "main" and "E02 offline turn" in r.get("last_user", "")]
    assert len(recs) >= 2, [r.get("last_user") for r in recs]
    for rec in recs:
        assert _lab.NOTE_MARK not in _lab.last_user_message(rec), "the note was appended again"
        # user messages only: the fake model's echo reply of E1's first turn quotes the note's opening tag
        stored = [
            m for m in rec["body"]["messages"] if m.get("role") == "user" and _lab.NOTE_MARK in _lab.message_text(m)
        ]
        assert len(stored) == 1, len(stored)
    arrival = lab.arrival("b", sid) or {}
    ev.write("arrival-b.json", arrival)
    assert arrival.get("note_state") == "delivered", arrival
    CHAIN["e02"] = {"head": head}


@pytest.mark.e2e_timeout(900)
def test_e03_laptop_to_server_push(lab, ev):
    """E3: ``/handover to server`` from B (CLI form, ``--session``): A gets the continued session with the full
    history (H_all/H_rows/H_model and display equal to B's lineage), titled exactly "T" (no "#2", the source copy on A
    carries the "handed over" title), and B's session is vetoed (marks + veto text, no model request)."""
    c = _need("e01")
    src, sid_b = c["src"], lab.tip("b", c["b_sid"])
    before_b = lab.hashes("b", sid_b)
    repo_b = _lab.repo_state(c["dest"])
    repo_a_before = _lab.repo_state(src.repo)
    ev.write("before.json", {"hashes_b": before_b, "repo_b": repo_b, "repo_a": repo_a_before})
    res = lab.handover("b", "to", _lab.SERVER, "--session", sid_b, "--yes", timeout=600)
    ev.cli("to-server", res)
    assert res.rc == 0 and res.job_id, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write("job-b.json", status)
    job = status["job"]
    assert job.get("state") == "done", job
    sid_a = str((job.get("ack") or {}).get("session_id") or "")
    assert sid_a and sid_a != src.sid, job
    CHAIN["e03"] = {"job": job, "sid_a": sid_a, "repo_b": repo_b, "repo_a_before": repo_a_before}
    after_a = lab.hashes("a", sid_a)
    ev.write("hashes-a.json", after_a)
    for key in ("h_all", "h_rows", "h_model", "model_norm", "display_norm", "rows"):
        assert after_a[key] == before_b[key], key
    row_a = lab.row("a", sid_a)
    ev.write("row-a.json", row_a)
    assert row_a.get("title") == src.title, (row_a.get("title"), src.title)
    assert not row_a.get("hidden") and not row_a.get("archived") and row_a.get("ended_at") is None
    title_b = lab.row("b", sid_b).get("title") or ""
    base_b = title_b.split(" (handed over to", 1)[0]
    ev.write("marks-b.json", _assert_retired_marks(lab, "b", sid_b, base_b))
    _assert_veto(lab, ev, "b", sid_b, "E03")


@pytest.mark.e2e_timeout(300)
def test_e04_return_to_the_same_path_with_handed_away_wip(lab, ev):
    """E4 (R4, SK H3): the push of E3 lands the repository **in place** in A's original folder although that folder
    still holds the uncommitted work handed away in E1 (return mode: unchanged since the outgoing handover); the
    ignored ``build/`` survives byte-equal; no ``<dest>-handover-<ts>`` folder exists."""
    c1, c3 = _need("e01"), _need("e03")
    src = c1["src"]
    assert c3["repo_a_before"] == c1["repo_a"], "precondition: A's folder changed after E1"
    path, mode = _lab.dest_of(c3["job"], src.repo.name)
    ev.write("dest.json", {"path": path, "mode": mode, "ack": c3["job"].get("ack")})
    assert path and Path(path).resolve() == src.repo.resolve(), (path, str(src.repo))
    assert mode and mode.lower().endswith("return"), mode
    repo_a = _lab.repo_state(src.repo)
    ev.write("repo-a-after.json", repo_a)
    for key in ("head", "branch", "status", "index", "refs", "stash", "tree"):
        assert repo_a[key] == c3["repo_b"][key], key
    assert _lab.file_sha(src.repo / "build" / "out.bin") == c1["build_sha"]
    strays = [p.name for p in src.repo.parent.iterdir() if p.name.startswith(f"{src.repo.name}-handover-")]
    root = lab.handover_root("a")
    if root.is_dir():
        strays += [p.name for p in root.iterdir() if p.name.startswith(f"{src.repo.name}")]
    assert not strays, strays
    row = lab.row("a", c3["sid_a"])
    assert Path(str(row.get("cwd") or "")).resolve() == src.repo.resolve(), row.get("cwd")
