"""R6-B test gap (review of PR #190, §3 "the re-check between PREPARE and COMMIT in ``gitapply.py``"): commit step c1
checks the destination repository again before it changes anything — work, a commit or a branch switch made at the
destination between PREPARE and COMMIT is never overwritten, and the repository stays byte-identical.

``test_gitapply.test_apply_update_recheck_at_commit`` covers one branch of it (a new untracked file in update mode).
These cover the others: HEAD moved by a commit (status clean), the checked-out branch switched (same commit), another
ref of the destination moved, and the return mode's out-state comparison at commit.

Red proof (mutants, run against ``engine/gitapply.py`` with the named line removed; log in the R6 report):
``M1`` the HEAD/branch comparison of update mode, ``M2`` the ``old_refs`` loop, ``M3`` the out-state comparison of
return mode — each kills the test that names it."""

from __future__ import annotations

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.engine.errors import ConflictError
from ctx_handover.tests.fakes.repo_builder import dir_fingerprint
from ctx_handover.tests.unit.E._support import build_mixed, pack
from ctx_handover.tests.unit.E.test_gitapply import _clean_copy, _server_and_laptop


def _prepared_update(git, user_home, staging, cfg, store, handover_root, *, advance_feature=False):
    src = build_mixed(user_home / "src")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    if advance_feature:  # the update then also writes ``feature``: PREPARE records the destination's value of it
        tree = src.out("rev-parse", "feature^{tree}")
        res = src.run("commit-tree", tree, "-p", "feature", "-m", "advance feature", identity=True)
        src.run("update-ref", "refs/heads/feature", res.stdout.decode().strip())
    src.write("fresh.txt", "x\n")
    src.add("fresh.txt")
    src.commit("advance")
    _, item = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(tgt.root)), item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    return tgt, state, item


def test_update_commit_refuses_a_commit_made_after_prepare(git, user_home, staging, cfg, store, handover_root):
    """★ M1 + M2: a commit at the destination after PREPARE (work tree clean again) ⇒ ``dirty``, nothing changed. Two
    checks guard it — the commit moves HEAD and the branch the update writes — so only removing both lets it through."""
    tgt, state, item = _prepared_update(git, user_home, staging, cfg, store, handover_root)
    tgt.write("late.txt", "committed after prepare\n")
    tgt.add("late.txt")
    tgt.commit("late work")
    before = (tgt.head(), tgt.refs(), dir_fingerprint(tgt.root))
    with pytest.raises(ConflictError) as exc:
        gitapply.commit(git, state, item)
    assert exc.value.details["kind"] == "dirty"
    assert (tgt.head(), tgt.refs(), dir_fingerprint(tgt.root)) == before


def test_update_commit_refuses_a_branch_switch_after_prepare(git, user_home, staging, cfg, store, handover_root):
    """★ M1: the destination switched to another branch at the same commit ⇒ refused, HEAD stays where the user put
    it."""
    tgt, state, item = _prepared_update(git, user_home, staging, cfg, store, handover_root)
    tgt.checkout("elsewhere", new=True)
    before = (tgt.out("symbolic-ref", "HEAD"), tgt.refs(), dir_fingerprint(tgt.root))
    with pytest.raises(ConflictError):
        gitapply.commit(git, state, item)
    assert (tgt.out("symbolic-ref", "HEAD"), tgt.refs(), dir_fingerprint(tgt.root)) == before


def test_update_commit_refuses_a_moved_ref(git, user_home, staging, cfg, store, handover_root):
    """★ M2: a branch the update writes (``feature``, not checked out) was moved at the destination after PREPARE
    ⇒ "a ref moved", nothing changed."""
    tgt, state, item = _prepared_update(git, user_home, staging, cfg, store, handover_root, advance_feature=True)
    assert "refs/heads/feature" in state.old_refs
    tgt.run("branch", "-q", "-f", "feature", "main")
    before = (tgt.head(), tgt.refs(), dir_fingerprint(tgt.root))
    with pytest.raises(ConflictError) as exc:
        gitapply.commit(git, state, item)
    assert exc.value.details.get("ref") == "refs/heads/feature"
    assert (tgt.head(), tgt.refs(), dir_fingerprint(tgt.root)) == before


def test_return_commit_refuses_an_edit_after_prepare(git, user_home, staging, cfg, store, handover_root, tmp_path):
    """★ M3: return mode, the server's copy edited after PREPARE ⇒ ``changed_since_handover`` at commit."""
    server, laptop, out_state = _server_and_laptop(git, user_home, staging, cfg, store, handover_root)
    laptop.write("mod.txt", "edited on the laptop\n")
    _, item = pack(
        git, laptop.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(server.root)), item_id="back"
    )
    state = gitapply.prepare_return(
        git, store, item, server.root, out_state, tmp=tmp_path / "rt", cfg=cfg, qdir=staging / "q"
    )
    server.write("untracked.txt", "edited on the server after prepare\n")
    before = (server.head(), server.refs(), dir_fingerprint(server.root))
    with pytest.raises(ConflictError) as exc:
        gitapply.commit(git, state, item)
    assert exc.value.details["kind"] == "changed_since_handover"
    assert (server.head(), server.refs(), dir_fingerprint(server.root)) == before
