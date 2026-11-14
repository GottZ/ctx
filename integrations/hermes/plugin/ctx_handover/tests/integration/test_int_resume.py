"""A conductor process killed in the middle of the block transfer, then resumed (TESTPLAN §3
``test_int_resume_after_kill_mid_transfer``; DESIGN §3.6 janitor, §3.7 idempotency, §4.6 "Resume / cache", I9).

A separate process of the laptop's profile pushes a session with a 24 MiB repository to the server's listener and
dies by SIGKILL after its third block upload (the capsule, then two batches of the git pack). The test process then
does what a restarted gateway and the user do: once the dead owner is stale the janitor suspends the job, and
``resume`` drives it to the end. Expected: the target already holds part of the pack, the resumed run sends exactly
the blocks it does not hold (none twice), the pack is not built again, and the workspace arrives complete.

``STALE_OWNER_S`` (60 s) is shortened in this process to 2 s; the test then waits past it — the same as waiting a
minute before ``/handover resume``."""

from __future__ import annotations

import json
import os
import signal
import time

from ctx_handover.engine import jobs, local_source, local_target, ops, remote
from ctx_handover.engine.model import Role
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.integration._int2_helpers import ack_ids, conductor_ctx, outcome, run_child
from ctx_handover.tests.integration.conftest import builder

PARTS, PART_BYTES = 6, 4 * 1024 * 1024
STALE_S = 2.0


def _pack_blocks(home, pack_id: str) -> set[str]:
    with home.active():
        store = jobs._store(jobs._profile())  # the profile's block store, as the plugin uses it
        ids = ops.object_closure(store, pack_id, max_doc_bytes=64 * 1024 * 1024, cache={})
        return {b for oid in ids for b in store.manifest(oid).blocks}


def _held(home, block_ids: set[str]) -> set[str]:
    with home.active():
        store = jobs._store(jobs._profile())
        return {b for b in block_ids if store.has(b)}


def test_int_resume_after_kill_mid_transfer(online, monkeypatch):
    server, laptop, _listener, peer = online
    repo = RepoBuilder.init(laptop.home / "proj")
    repo.write("README.md", "# data\n")
    for i in range(PARTS):
        repo.write(f"data/part{i}.bin", os.urandom(PART_BYTES))
    repo.add_all()
    repo.commit("data")
    digest = tree_digest(repo.root)
    b = builder(laptop)
    sid = b.create(title="Killed mid transfer", cwd=str(repo.root))
    b.turns(sid, 3)

    cfg = laptop.root / "push-and-die.json"
    cfg.write_text(json.dumps({"peer_id": peer.peer_id, "session_id": sid, "die_after_batches": 3}), encoding="utf-8")
    child = run_child(laptop, ["push-and-die", str(cfg)])
    assert child.returncode == -signal.SIGKILL, (child.returncode, child.stdout, child.stderr[-3000:])
    job_id = next(line.split()[1] for line in child.stdout.splitlines() if line.startswith("JOB "))

    before = conductor_ctx(laptop, job_id)
    assert before.get("pack"), f"died before the pack was journaled: {laptop.ledger().job(job_id, Role.CONDUCTOR)}"
    pack_id = before["pack"]["pack_id"]
    pack_blocks = _pack_blocks(laptop, pack_id)
    held = _held(server, pack_blocks)
    for _ in range(20):  # the listener finishes the request the dead child left half-sent
        time.sleep(0.25)
        again = _held(server, pack_blocks)
        if again == held:
            break
        held = again
    assert 0 < len(held) < len(pack_blocks), (len(held), len(pack_blocks))
    packs_before = [o for o in laptop.ledger().ops_for_job(job_id) if o["op"] == "pack"]
    assert [o["state"] for o in packs_before] == ["done"], packs_before

    sent: list[str] = []
    original = remote.RemoteBlocks.write_blocks

    def record(self, frames):
        frames = list(frames)
        sent.extend(block_id for block_id, _raw in frames)
        return original(self, frames)

    monkeypatch.setattr(remote.RemoteBlocks, "write_blocks", record)
    for module in (jobs, local_source, local_target):
        monkeypatch.setattr(module, "STALE_OWNER_S", STALE_S)
    time.sleep(STALE_S + 0.5)

    with laptop.active():
        prof = jobs._profile()
        runner = jobs.runner_for(None)
        swept = prof.janitor(background=False)
        assert (job_id, laptop.ledger().job(job_id, Role.CONDUCTOR)["state"]) in [
            (j, "suspended") for j, _s in swept["conductor"]
        ], (swept, outcome(laptop, job_id))
        runner.resume(job_id)
        runner.join(240)
    row = laptop.ledger().job(job_id, Role.CONDUCTOR)
    assert jobs.exit_code(row) == 0, outcome(laptop, job_id)

    resent = set(sent) & pack_blocks
    assert not resent & held, f"{len(resent & held)} blocks the target held were sent again"
    assert resent == pack_blocks - held, (len(resent), len(pack_blocks - held))
    assert len(sent) == len(set(sent)), "a block was sent twice in the resumed run"
    after = conductor_ctx(laptop, job_id)
    assert after["pack"]["pack_id"] == pack_id  # the pack built before the kill is the one that arrived
    packs_after = [o for o in laptop.ledger().ops_for_job(job_id) if o["op"] == "pack"]
    assert [(o["op_id"], o["updated_at"]) for o in packs_after] == [
        (o["op_id"], o["updated_at"]) for o in packs_before
    ], "the source built the pack again"

    _root, new_tip = ack_ids(laptop, job_id)
    assert new_tip
    assert tree_digest(server.home / "hermes-handover" / "proj") == digest
