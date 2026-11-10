"""The second pull of the same repository (TESTPLAN §3 ``test_int_second_pull_uses_basis``; DESIGN §8.6, §9.3,
§9.5, §4.5 route #15; [BX H3], R9).

The laptop pulls a session with a clean repository from the server, the server's repository gets one more small
commit, and the laptop pulls a second session of that repository. The laptop's copy is clean and shares the root
commit, so its negotiation answers ``update`` with ``git_have`` = its ref tips; the FinalPlan travels inline with
the pack request to the remote source, which packs only what lies beyond that basis. Expected: the second git pack
names the basis and is a small fraction of the first, and the laptop's copy is updated in place to the new HEAD."""

from __future__ import annotations

import json
import os

from ctx_handover.engine import jobs
from ctx_handover.engine.model import Mode, Options, Role, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.integration._int2_helpers import conductor_ctx, outcome
from ctx_handover.tests.integration.conftest import builder

BIG = 3 * 1024 * 1024  # incompressible history the second pull must not resend


def _session(home, cwd, title: str) -> str:
    b = builder(home)
    sid = b.create(title=title, cwd=str(cwd))
    b.turns(sid, 3)
    return sid


def _git_item(home, job_id: str) -> tuple[dict, dict, int]:
    """``(pack-document item, target decision, size of the git pack object)`` of the job's one git item."""
    ctx = conductor_ctx(home, job_id)
    with home.active():
        store = jobs._store(jobs._profile())  # the profile's block store, as the plugin uses it
        pack_doc = json.loads(store.read_object_bytes(ctx["pack"]["pack_id"]))
        items = [i for i in pack_doc["items"] if i["kind"] == "git_repo"]
        assert len(items) == 1, pack_doc["items"]
        size = store.manifest(items[0]["pack"]).size
    decision = next(d for d in ctx["final_plan"]["decisions"] if d["item_id"] == items[0]["item_id"])
    return items[0], decision, size


def test_int_second_pull_uses_basis(online):
    """★ Second pull of the same repo sends an incremental pack (FinalPlan inline, ``git_have``) [BX H3].

    Mutant: the source ignores the target's ``git_have`` (``basis_candidates=()`` in ``local_source``) — the second
    pack carries the whole history again."""
    server, laptop, _listener, peer = online
    repo = RepoBuilder.init(server.home / "proj")
    repo.write("README.md", "# proj\n")
    repo.write("assets/blob.bin", os.urandom(BIG))
    repo.add_all()
    first = repo.commit("first")

    job1, rc1 = laptop.run(
        Mode.PULL,
        peer,
        SessionRef(id=_session(server, repo.root, "Pull one")),
        options=Options(refine="off", yes=True, stall_timeout_s=60),
    )
    assert rc1 == 0, outcome(laptop, job1)
    dest = laptop.home / "hermes-handover" / "proj"
    assert RepoBuilder(dest).head() == first
    item1, decision1, size1 = _git_item(laptop, job1)
    assert decision1["mode"] == "fresh" and not item1["basis"], (decision1, item1["basis"])
    assert size1 > BIG

    repo.write("src/feature.py", "print('second')\n")
    repo.add_all()
    second = repo.commit("second")

    job2, rc2 = laptop.run(
        Mode.PULL,
        peer,
        SessionRef(id=_session(server, repo.root, "Pull two")),
        options=Options(refine="off", yes=True, stall_timeout_s=60),
    )
    assert rc2 == 0, outcome(laptop, job2)
    item2, decision2, size2 = _git_item(laptop, job2)
    assert decision2["dest"] == str(dest), decision2
    assert decision2["mode"] == "update", decision2
    assert first in decision2["git_have"], decision2["git_have"]
    # The FinalPlan reached the remote source inline with the pack request (route #15) ...
    source_ctx: dict = {}
    for ev in server.ledger().events(job2, Role.SOURCE):
        if isinstance((ev["data"] or {}).get("ctx"), dict):
            source_ctx.update(ev["data"]["ctx"])
    remote_decision = next(d for d in source_ctx["final_plan"]["decisions"] if d["item_id"] == item2["item_id"])
    assert first in remote_decision["git_have"], remote_decision
    # ... and the source packed against that basis: only the new commit travels.
    assert item2["basis"] == [first], item2["basis"]
    assert size2 * 10 < size1, (size1, size2)
    assert RepoBuilder(dest).head() == second
    assert tree_digest(dest) == tree_digest(repo.root)
