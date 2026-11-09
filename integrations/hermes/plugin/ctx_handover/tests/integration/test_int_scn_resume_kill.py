"""Mandatory scenario 5 (layer 2, DESIGN §3.1, §3.6, §4.6 I9, TESTPLAN E5b): the laptop's conductor process is
killed (SIGKILL) in the middle of the block upload of a push; the user resumes the job later from another process.

The conductor runs in its own process (the CLI process of ``hermes handover to``) and parks once the listener
acknowledged a first part of the blocks; the test kills it there. After the owner went stale (60 s without
heartbeat, DESIGN §3.1) the next use of the plugin runs the janitor and ``resume`` continues the job in this process.
Visible results: only blocks the server did not have are uploaded after the resume (none of the acknowledged ones
again, every announced but missing one), and the handover completes — model history and work tree at the server
equal the laptop's, the source carries the primary lock."""

from __future__ import annotations

import os

from ctx_handover.engine import jobs, remote
from ctx_handover.engine.jsonutil import to_wire
from ctx_handover.engine.ledger import STALE_OWNER_S
from ctx_handover.engine.model import Options, Role
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.integration._int3_helpers import STALL_S, ParkedPush, arrival, repo_view, session_in
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks, wait_for


def _big_repo(root):
    r = RepoBuilder.init(root)
    for i in range(3):
        r.write(f"data/blob{i}.bin", os.urandom(3 * 1024 * 1024))  # incompressible: ≈ 18 blocks in the git pack
    r.write("src/main.py", "print('v1')\n")
    r.add_all()
    r.commit("data")
    r.write("src/main.py", "print('edited, not committed')\n")
    return r


def _stale(ledger, job_id: str) -> bool:
    row = ledger.job(job_id, Role.CONDUCTOR) or {}
    hb = row.get("owner_heartbeat_at")
    return row.get("owner") is None or (hb is not None and hb < ledger.now() - STALE_OWNER_S)


def test_int_scn_kill_mid_upload_then_resume_sends_only_missing_blocks(online, monkeypatch):
    """Scenario 5 (≈ 90 s: the kill must age past the 60 s owner staleness)."""
    server, laptop, _listener, _peer = online
    repo = _big_repo(laptop.home / "big")
    view = repo_view(repo.root)
    sid = session_in(laptop, repo.root, "Interrupted upload")
    opts = to_wire(Options(refine="off", yes=True, stall_timeout_s=STALL_S))

    parked = ParkedPush(laptop, sid, "server", opts, min_blocks=4)
    try:
        job_id = parked.wait_parked()
    finally:
        sent_before, announced_before = parked.sent(), parked.announced()
        parked.kill()
    assert sent_before, "nothing was uploaded before the kill"
    assert announced_before - sent_before, "the kill did not land in the middle of the upload"

    ledger = laptop.ledger()
    assert wait_for(lambda: _stale(ledger, job_id), timeout=STALE_OWNER_S + 60, interval=1.0)

    sent_after: list[str] = []
    announced_after: set[str] = set()
    write, put = remote.RemoteBlocks.write_blocks, remote.RemoteBlocks.put_manifests

    def record_write(self, frames):
        frames = list(frames)
        result = write(self, iter(frames))
        rejected = {b for b, _ in result[1]}
        sent_after.extend(b for b, _ in frames if b not in rejected)
        return result

    def record_put(self, manifests):
        manifests = list(manifests)
        for m in manifests:
            announced_after.update(m.blocks)
        return put(self, manifests)

    monkeypatch.setattr(remote.RemoteBlocks, "write_blocks", record_write)
    monkeypatch.setattr(remote.RemoteBlocks, "put_manifests", record_put)
    with laptop.active():
        rt = jobs.ensure_started()  # first use in this process: the janitor adopts the dead conductor (§3.6)
        rt.runner.join(60)
        rt.runner.resume(job_id)
        rt.runner.join(300)
        row = rt.ledger.job(job_id, Role.CONDUCTOR)
        errors = [e["data"] for e in rt.ledger.events(job_id, Role.CONDUCTOR) if e["kind"] == "error"]
    assert jobs.exit_code(row) == 0, (row["state"], row["last_error"], errors)

    assert not (set(sent_after) & sent_before), "blocks the server already had were uploaded again"
    assert len(sent_after) == len(set(sent_after)), "a block was uploaded twice after the resume"
    assert set(sent_after) == (announced_before | announced_after) - sent_before, "not exactly the missing blocks"

    tip = (arrival(server, job_id) or {}).get("session_id")
    assert tip, "no arrival row on the server"
    assert lineage_hashes(server.db(), tip) == lineage_hashes(laptop.db(), sid)
    assert repo_view(server.home / "hermes-handover" / "big") == view
    marks = session_marks(laptop.db(), sid)
    assert marks["ended"] and marks["archived"] and marks["hidden"], marks
