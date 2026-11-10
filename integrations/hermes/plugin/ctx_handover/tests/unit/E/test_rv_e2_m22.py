"""First review round, finding M22 (gitops part): the temp-index state computation takes the profile and the filter
names of its caller. A target that recomputes a destination holding received files (resume, the failure path of
c1) must not let a received ``.gitattributes`` pick the user's filter drivers (DESIGN §9.2 received, SEC 3.1)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import gitops
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import append_repo_config, marker_script, write_global_gitconfig


def _dest_with_received_attributes(user_home, tmp_path):
    markers = tmp_path / "markers"
    markers.mkdir()
    glob = marker_script(tmp_path / "global-clean.sh", markers, "global-clean")
    local = marker_script(tmp_path / "local-clean.sh", markers, "local-clean")
    write_global_gitconfig(user_home, f'[filter "peerchosen"]\n\tclean = {glob}\n\trequired = true\n')
    r = RepoBuilder.init(user_home / "dest")
    r.write("a.txt", "a\n")
    r.write("a.md", "a\n")
    r.commit_all("c1")
    append_repo_config(r.root, f'[filter "localchosen"]\n\tclean = {local}\n\trequired = true\n')
    # what a received tree brings along: attributes naming the user's drivers, files they would clean
    r.write(".gitattributes", "*.txt filter=peerchosen\n*.md filter=localchosen\n")
    r.write("b.txt", "peer file\n")
    r.write("b.md", "peer note\n")
    return r, markers


def test_rv_e2_m22_received_state_runs_no_user_filter(git, user_home, tmp_path):
    """★ M22: ``worktree_state(..., profile="received")`` runs neither the global nor the repository's own driver
    that a received ``.gitattributes`` names; the source profile (the default, DESIGN §9.2 source) runs both — the
    profile is the caller's choice (mutant: the profile argument ignored ⇒ markers)."""
    r, markers = _dest_with_received_attributes(user_home, tmp_path)
    recv = gitops.worktree_state(git, str(r.root), (), tmp_path / "s-recv", profile="received")
    assert sorted(p.name for p in markers.iterdir()) == []
    src = gitops.worktree_state(git, str(r.root), (), tmp_path / "s-src")
    assert sorted(p.name for p in markers.iterdir()) == ["global-clean", "local-clean"]
    assert (recv.index_tree, recv.worktree_tree) == (src.index_tree, src.worktree_tree)  # pass-through drivers


def test_rv_e2_m22_source_state_with_named_filters_emptied(git, user_home, tmp_path):
    """★ M22: the source profile keeps the user's config (ignore rules, eol — the state stays comparable to one
    recorded in the source profile) but empties the drivers named in ``filter_names`` — the names a received tree
    introduced (mutant: ``filter_names`` honoured only in the received profile ⇒ marker)."""
    r, markers = _dest_with_received_attributes(user_home, tmp_path)
    work = tmp_path / "work"
    st = gitops.compute_state(
        git, r.root, (), work / "objects", work, force=True, filter_names=("peerchosen", "localchosen")
    )
    assert sorted(p.name for p in markers.iterdir()) == []
    plain = gitops.worktree_state(git, str(r.root), (), tmp_path / "s-plain", filter_names=("peerchosen",))
    assert sorted(p.name for p in markers.iterdir()) == ["local-clean"]
    assert plain.worktree_tree == st.worktree_tree


def test_rv_e2_m22_unknown_profile_refused(git, user_home, tmp_path):
    r = RepoBuilder.init(user_home / "dest")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    with pytest.raises(ValueError):
        gitops.worktree_state(git, str(r.root), (), tmp_path / "s", profile="peer")


def test_rv_e2_m22_attribute_names_through_alternates(git, user_home):
    """The filter names of a tree whose objects lie outside the repository (a return-mode out-state tree, kept in
    the alternate ``out-objects``) are readable with that alternate (``env_extra``) — the gitapply side names the
    drivers its own out-state used (M22 wish)."""
    a = RepoBuilder.init(user_home / "a")
    a.write(".gitattributes", "*.x filter=foo\n")
    a.commit_all("attrs")
    tree = a.out("rev-parse", "HEAD^{tree}")
    b = RepoBuilder.init(user_home / "b")
    with pytest.raises(gitops.GitCommandError):
        gitops.attribute_filter_names(git, b.root / ".git", tree)
    alt = {"GIT_ALTERNATE_OBJECT_DIRECTORIES": str(a.root / ".git" / "objects")}
    assert gitops.attribute_filter_names(git, b.root / ".git", tree, env_extra=alt) == ["foo"]
    records = list(
        git.iter_z(
            ["--git-dir", str(b.root / ".git"), "ls-tree", "-z", tree], cwd=None, spec=gitops.RECEIVED, env_extra=alt
        )
    )
    assert records and records[0].endswith(b"\t.gitattributes")
