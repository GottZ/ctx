"""Review round rv-e2, finding M22 (gitapply part, wish to the integrator): a resumed c1 (the process died after the
step, before the journal) recomputes the destination while it holds the received files; the filter driver a
received ``.gitattributes`` names — and the destination's own trees did not — never runs (DESIGN §9.2 received,
SEC 3.1). Source and target are separate homes: the peer packs with ``Git(home=<peer home>)``."""

from __future__ import annotations

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.engine.model import DestMode
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import (
    apply_fresh,
    marker_script,
    pack,
    write_global_gitconfig,
)


@pytest.mark.parametrize("mode", ["update", "return"])
def test_rv_e2_m22_resume_runs_no_peer_chosen_user_filter(
    git, user_home, tmp_path, staging, cfg, store, handover_root, monkeypatch, mode
):
    """★ M22: ``commit`` and ``commit(resume=True)`` after it run no driver of the target user that only the
    received tree selects; the resume recognises the finished step (mutant: ``_peer_filters`` returns nothing ⇒
    the user's ``clean`` runs over the peer's file; the resume without the received tree's names fails the
    marker check)."""
    markers = tmp_path / "markers"
    markers.mkdir()
    clean = marker_script(tmp_path / "clean.sh", markers, "global-clean")
    (tmp_path / "peer-home").mkdir()
    peer_git = gitops.Git(home=tmp_path / "peer-home")
    if mode == "update":
        peer = RepoBuilder.init(user_home / "peer")
        peer.write("a.txt", "a\n")
        peer.commit_all("c1")
        _, seed = pack(peer_git, peer.root, staging, cfg, store, item_id="seed")
        dest = apply_fresh(git, store, seed, handover_root / "app", cfg)
        tgt = RepoBuilder(dest)
        tgt.run("reset", "-q", "--hard")
        tgt.run("clean", "-qfdx")
        peer.write(".gitattributes", "*.txt filter=peerchosen\n")
        peer.commit_all("attributes")
        peer.write("b.txt", "peer file\n")
        write_global_gitconfig(user_home, f'[filter "peerchosen"]\n\tclean = {clean}\n\trequired = true\n')
        basis = gitops.GitProbe(git).ref_tips(str(dest))
        _, item = pack(peer_git, peer.root, staging, cfg, store, basis=basis, item_id="u")
        state = gitapply.prepare_update(git, store, item, dest, cfg=cfg, qdir=staging / "q")
        assert state.mode is DestMode.UPDATE
    else:
        server = RepoBuilder.init(user_home / "server")
        server.write("a.txt", "a\n")
        server.commit_all("c1")
        server.write("wip.txt", "handed away\n")
        out, item0 = pack(git, server.root, staging, cfg, store, item_id="out")
        laptop = RepoBuilder(apply_fresh(peer_git, store, item0, handover_root / "laptop", cfg))
        laptop.write(".gitattributes", "*.txt filter=peerchosen\n")
        laptop.write("b.txt", "peer file\n")
        write_global_gitconfig(user_home, f'[filter "peerchosen"]\n\tclean = {clean}\n\trequired = true\n')
        basis = gitops.GitProbe(git).ref_tips(str(server.root))
        _, item = pack(peer_git, laptop.root, staging, cfg, store, basis=basis, item_id="back")
        state = gitapply.prepare_return(
            git,
            store,
            item,
            server.root,
            out.state,
            tmp=tmp_path / "rt",
            cfg=cfg,
            qdir=staging / "q",
        )
        assert state.mode is DestMode.RETURN
        dest = server.root
    gitapply.commit(git, state, item)
    assert sorted(p.name for p in markers.iterdir()) == []
    undone: list[bool] = []
    real_rollback = gitapply.rollback
    monkeypatch.setattr(
        gitapply,
        "rollback",
        lambda *a, **kw: undone.append(True) or real_rollback(*a, **kw),
    )
    gitapply.commit(git, state, item, resume=True)
    assert sorted(p.name for p in markers.iterdir()) == []
    assert undone == []  # the finished step was recognised, not undone and redone
    assert (dest / "b.txt").read_text() == "peer file\n"
    assert (dest / ".gitattributes").read_text() == "*.txt filter=peerchosen\n"
