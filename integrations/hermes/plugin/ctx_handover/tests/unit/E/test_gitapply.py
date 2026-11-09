"""Target side: quarantine, fresh / update / return, rollback (DESIGN §9.5, R4, R21, I5)."""

from __future__ import annotations

import os
import shutil
from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops, gitpack
from ctx_handover.engine.errors import ConflictError, PayloadTooLarge, VerifyFailed
from ctx_handover.engine.model import DestMode
from ctx_handover.engine.paths import rules_for
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.unit.E._support import (
    LINUX,
    append_repo_config,
    apply_fresh,
    assert_same_state,
    build_mixed,
    make_cfg,
    marker_script,
    pack,
    write_global_gitconfig,
)

EXCLUDED = [b".env"]
IGNORED = [b"ignored", b"build.log"]


def _clean_copy(git, store, cfg, src: RepoBuilder, staging: Path, dest: Path) -> RepoBuilder:
    """A target repository with the source's committed history only (clean, same identity)."""
    _, item = pack(git, src.root, staging, cfg, store, item_id="seed")
    final = apply_fresh(git, store, item, dest, cfg)
    t = RepoBuilder(final)
    t.run("stash", "clear")
    t.run("reset", "-q", "--hard")
    t.run("clean", "-qfdx")
    return t


# ---------------------------------------------------------------------------------------------------------
# fresh mode
# ---------------------------------------------------------------------------------------------------------


def test_apply_fresh_reproduces_state_bytes(git, user_home, staging, cfg, store, handover_root):
    """★ HEAD, refs (branches, tags), index, status, stash and work-tree bytes of the source reappear at the target,
    incl. staged/unstaged/untracked/deleted/renamed, exec bit, symlink, non-UTF-8 and newline names (mutant: the
    final ``read-tree I`` is skipped, so the index equals W) [SK §4.2]."""
    src = build_mixed(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, handover_root / "app", cfg)
    assert dest == handover_root / "app"
    assert_same_state(src, RepoBuilder(dest), excluded=EXCLUDED, ignored=IGNORED)
    assert not (dest / ".env").exists() and not (dest / "ignored").exists()
    assert (dest / "link").is_symlink() and os.readlink(dest / "link") == "keep.txt"
    assert os.stat(dest / "tool.sh").st_mode & 0o111
    assert (dest / ".git" / "config").read_text().count("[remote") == 0


def test_apply_fresh_partial_0700_then_rename(git, user_home, staging, cfg, store, handover_root, monkeypatch):
    """Partial 0700 while prepared, permissions from the umask at commit, POSIX rename onto an empty destination
    without an rmdir window [SEC 2.7, 6.1]."""
    src = build_mixed(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    dest = handover_root / "app"
    dest.mkdir()
    partial = handover_root / ".ctxh-abc123"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=dest)
    assert (os.stat(partial).st_mode & 0o777) == 0o700
    assert list(dest.iterdir()) == []
    rmdirs = []
    real_rmdir = os.rmdir
    monkeypatch.setattr(os, "rmdir", lambda p, *a, **k: (rmdirs.append(p), real_rmdir(p, *a, **k)))
    old = os.umask(0o027)
    try:
        final = gitapply.commit(git, state, item)
    finally:
        os.umask(old)
    assert final == dest and not partial.exists() and rmdirs == []
    assert (os.stat(dest).st_mode & 0o777) == 0o750
    assert (os.stat(dest / "tool.sh").st_mode & 0o777) in (0o755, 0o750)


def test_fresh_commit_dest_appeared_falls_back(git, user_home, staging, cfg, store, handover_root):
    """A destination that appears between prepare and commit is left alone; the item lands at the fallback
    [SEC 2.7]."""
    src = build_mixed(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    dest, fallback = handover_root / "app", handover_root / "app-handover-1"
    state = gitapply.prepare_fresh(
        git, store, item, handover_root / ".ctxh-x", rules=LINUX, cfg=cfg, dest=dest, fallback=fallback
    )
    dest.mkdir()
    (dest / "mine.txt").write_text("user data\n")
    final = gitapply.commit(git, state, item)
    assert final == fallback
    assert sorted(p.name for p in dest.iterdir()) == ["mine.txt"]
    assert (fallback / ".git").is_dir()
    gitapply.rollback(git, gitapply.GitApplyState(**{**state.__dict__, "dest": final}), item)
    assert not fallback.exists() and (handover_root / ".ctxh-x").is_dir()


def test_sec_git_checkout_size_precheck(git, user_home, staging, hermes_home, store, handover_root):
    """W larger than allowed (bytes or entries) is refused before anything is checked out [SEC 2.5]."""
    big = make_cfg(user_home, hermes_home)
    src = build_mixed(user_home / "src")
    src.write("blob.bin", b"\x02" * 50_000)
    _, item = pack(git, src.root, staging, big, store)
    for small in (
        make_cfg(user_home, hermes_home, max_total_bytes=10_000),
        make_cfg(user_home, hermes_home, max_files=5),
    ):
        partial = handover_root / ".ctxh-p"
        with pytest.raises(PayloadTooLarge):
            gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=small, dest=handover_root / "a")
        assert not partial.exists()
    assert list(handover_root.iterdir()) == []


# ---------------------------------------------------------------------------------------------------------
# quarantine
# ---------------------------------------------------------------------------------------------------------


def _forge(git, tmp: Path, store, tree_body: bytes) -> dict:
    """A pack whose root tree is ``tree_body`` (raw tree object bytes), with consistent I/W commits."""
    repo = tmp / "forge.git"
    git.run(["init", "-q", "--bare", "--template=", str(repo)], cwd=None, spec=gitops.RECEIVED)
    gd = ["--git-dir", str(repo)]
    tree = git.out(
        [*gd, "hash-object", "-w", "-t", "tree", "--literally", "--stdin"],
        cwd=None,
        spec=gitops.RECEIVED,
        input=tree_body,
    )
    ident = gitops.GitEnvSpec(technical_identity_at=1_700_000_000)
    c = git.out([*gd, "commit-tree", "-m", "c", tree], cwd=None, spec=ident)
    i = git.out([*gd, "commit-tree", "-p", c, "-m", "i", tree], cwd=None, spec=ident)
    w = git.out([*gd, "commit-tree", "-p", i, "-m", "w", tree], cwd=None, spec=ident)
    proc = git.popen_stdout(
        [*gd, "pack-objects", "--revs", "--stdout", "-q"], cwd=None, spec=gitops.RECEIVED, input_lines=[w]
    )
    data = proc.stdout.read()
    proc.wait()
    m = gitops.put_bytes(store, data, "application/x-git-pack")
    return {
        "item_id": "w0",
        "kind": "git_repo",
        "pack": m.object_id,
        "object_format": "sha1",
        "basis": [],
        "head": {"ref": "refs/heads/main", "commit": c},
        "index_commit": i,
        "worktree_commit": w,
        "index_tree": tree,
        "worktree_tree": tree,
        "refs": {"refs/heads/main": c},
        "stash": [],
        "remotes": [],
        "upstreams": {},
        "exclude_pathspecs": [],
        "lfs_objects": None,
    }


def _blob(git, tmp: Path, data: bytes) -> bytes:
    repo = tmp / "forge.git"
    if not repo.exists():
        git.run(["init", "-q", "--bare", "--template=", str(repo)], cwd=None, spec=gitops.RECEIVED)
    oid = git.out(["--git-dir", str(repo), "hash-object", "-w", "--stdin"], cwd=None, spec=gitops.RECEIVED, input=data)
    return bytes.fromhex(oid)


@pytest.mark.parametrize("kind", ["dotgit", "badmode", "dotdot"])
def test_git_fsck_quarantine_rejects_bad_tree(git, user_home, tmp_path, cfg, store, handover_root, kind):
    """★ a runtime-built pack with a ``.GIT`` entry, a bad file mode or a ``..`` entry fails in the quarantine;
    the destination stays untouched and the quarantine is deleted (mutant: no strict checks in the quarantine —
    index-pack without ``--strict`` and no fsck) [R21, SEC 3.2]."""
    blob = _blob(git, tmp_path, b"payload\n")
    if kind == "dotgit":
        hooks = git.out(
            ["--git-dir", str(tmp_path / "forge.git"), "hash-object", "-w", "-t", "tree", "--stdin"],
            cwd=None,
            spec=gitops.RECEIVED,
            input=b"100755 post-checkout\x00" + blob,
        )
        body = b"40000 .GIT\x00" + bytes.fromhex(hooks)
    elif kind == "badmode":
        body = b"100777 run.sh\x00" + blob
    else:
        body = b"100644 ..\x00" + blob
    item = _forge(git, tmp_path, store, body)
    qdir = tmp_path / "staging" / "quarantine" / "w0"
    partial = handover_root / ".ctxh-q"
    with pytest.raises(VerifyFailed):
        gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app", qdir=qdir)
    assert not qdir.exists() and not partial.exists()
    assert list(handover_root.iterdir()) == []


def test_git_fsck_alone_rejects_bad_mode(git, tmp_path, store):
    """The fsck stage alone (objects admitted by a lenient index-pack) rejects a bad mode — the two layers are
    independent of each other and of the git version's bundle checks."""
    item = _forge(git, tmp_path, store, b"100777 run.sh\x00" + _blob(git, tmp_path, b"x"))
    q = tmp_path / "q"
    git.run(["init", "-q", "--bare", "--template=", str(q)], cwd=None, spec=gitops.RECEIVED)
    data = gitops.read_object(store, item["pack"], max_size=1 << 20)
    git.run(["--git-dir", str(q), "index-pack", "--stdin"], cwd=None, spec=gitops.RECEIVED, input=data)
    with pytest.raises(VerifyFailed):
        gitapply._fsck(git, q)


def test_git_failed_quarantine_never_reused(git, user_home, tmp_path, staging, cfg, store):
    """★ a quarantine is recreated for every attempt: leftovers of a failed or interrupted attempt are gone
    (mutant: an existing quarantine directory is resumed into) [SEC 3.2]."""
    src = build_mixed(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    qdir = tmp_path / "q" / "w0"
    first_block = store.manifests([item["pack"]])[0].blocks[0]
    saved = store._path(first_block).read_bytes()
    store.corrupt(first_block)
    with pytest.raises(Exception):  # noqa: B017 - BlockCorrupt from the stream
        gitapply.quarantine_and_verify(git, store, item, qdir, alternates=None)
    assert not qdir.exists()
    store._path(first_block).write_bytes(saved)
    qdir.mkdir(parents=True)
    (qdir / "leftover-from-crash").write_text("stale")
    gitapply.quarantine_and_verify(git, store, item, qdir, alternates=None)
    assert not (qdir / "leftover-from-crash").exists()
    assert (qdir / "objects" / "pack").is_dir()


def test_quarantine_rejects_inconsistent_state_commits(git, user_home, staging, cfg, store, tmp_path):
    """I/W must carry the announced trees on the announced parents; HEAD must match its branch."""
    src = build_mixed(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    for field, value in (("worktree_tree", item["index_tree"]), ("index_commit", item["worktree_commit"])):
        bad = {**item, field: value}
        with pytest.raises(VerifyFailed):
            gitapply.quarantine_and_verify(git, store, bad, tmp_path / "q", alternates=None)
    bad = {**item, "refs": {**item["refs"], "refs/heads/main": item["refs"]["refs/heads/feature"]}}
    with pytest.raises(VerifyFailed):
        gitapply.quarantine_and_verify(git, store, bad, tmp_path / "q", alternates=None)


# ---------------------------------------------------------------------------------------------------------
# refs from the pack document
# ---------------------------------------------------------------------------------------------------------


def test_apply_incremental_head_unchanged(git, user_home, staging, cfg, store, handover_root):
    """★ incremental pack with HEAD unchanged: refs come from the pack document — a new tag on a commit the target
    already has arrives, HEAD stays (mutant: refs not taken from the pack document) [SK M1]."""
    src = build_mixed(user_home / "src")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    src.tag("v2", ref="HEAD")
    src.write("fresh.txt", "new\n")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    res, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="inc")
    assert res.basis
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    assert tgt.head() == src.head()
    assert tgt.refs()["refs/tags/v2"] == src.refs()["refs/tags/v2"]
    assert_same_state(src, tgt, excluded=EXCLUDED, ignored=IGNORED)


def test_apply_new_branch_on_known_commit(git, user_home, staging, cfg, store, handover_root):
    """★ a new branch pointing at a commit the target has arrives (mutant: refs not taken from the pack
    document) [SK M1]."""
    src = build_mixed(user_home / "src")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    src.branch("topic", "feature")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="inc")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    assert tgt.refs()["refs/heads/topic"] == src.rev("feature")
    assert_same_state(src, tgt, excluded=EXCLUDED, ignored=IGNORED)


# ---------------------------------------------------------------------------------------------------------
# update mode
# ---------------------------------------------------------------------------------------------------------


def test_apply_update_fast_forward(git, user_home, staging, cfg, store, handover_root):
    src = build_mixed(user_home / "src")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    src.run("stash", "push", "-q", "-m", "second", identity=True)
    src.write("committed.txt", "c\n")
    src.add("committed.txt")
    src.commit("advance")
    src.write("wip.txt", "w\n")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert state.mode is DestMode.UPDATE and state.h0 != src.head()
    gitapply.commit(git, state, item)
    assert_same_state(src, tgt, excluded=EXCLUDED, ignored=IGNORED)


def test_apply_update_refuses_dirty_target(git, user_home, staging, cfg, store, handover_root):
    """★ an update never runs over uncommitted work at the target — an untracked file the update would not even
    touch is enough (mutant: no clean-status precondition) [I5]."""
    src = build_mixed(user_home / "src")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    tgt.write("notes-of-the-target-user.txt", "mine\n")
    before = dir_fingerprint(tgt.root)
    src.write("fresh.txt", "x\n")
    _, item = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(tgt.root)), item_id="u")
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert exc.value.details["kind"] == "dirty"
    assert dir_fingerprint(tgt.root) == before


def test_apply_update_refuses_diverged_head(git, user_home, staging, cfg, store, handover_root):
    src = build_mixed(user_home / "src")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    tgt.write("own.txt", "target commit\n")
    tgt.add("own.txt")
    tgt.commit("diverge")
    _, item = pack(git, src.root, staging, cfg, store, item_id="u")
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert exc.value.details["kind"] == "diverged"
    assert (tgt.root / "own.txt").read_text() == "target commit\n"


def test_apply_update_recheck_at_commit(git, user_home, staging, cfg, store, handover_root):
    """★ the clean precondition is checked again in commit step c1: work created between prepare and commit is
    never overwritten (mutant: commit without the re-check) [I5]."""
    src = build_mixed(user_home / "src")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    src.write("fresh.txt", "x\n")
    _, item = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(tgt.root)), item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    tgt.write("late-user-file.txt", "typed after prepare\n")
    before = dir_fingerprint(tgt.root)
    with pytest.raises(ConflictError):
        gitapply.commit(git, state, item)
    assert dir_fingerprint(tgt.root) == before


def test_apply_rollback_restores_h0(git, user_home, staging, cfg, store, handover_root):
    """The inverse of an update restores HEAD, refs, index, work tree and stash depth."""
    src = build_mixed(user_home / "src")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    before = (tgt.head(), tgt.refs(), tgt.index_entries(), tgt.status(), tgt.worktree_digest(), tgt.stash_list())
    src.write("committed.txt", "c\n")
    src.add("committed.txt")
    src.commit("advance")
    src.branch("topic")
    _, item = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(tgt.root)), item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    assert tgt.head() == src.head()
    gitapply.rollback(git, state, item)
    after = (tgt.head(), tgt.refs(), tgt.index_entries(), tgt.status(), tgt.worktree_digest(), tgt.stash_list())
    assert after == before


# ---------------------------------------------------------------------------------------------------------
# return mode (R4)
# ---------------------------------------------------------------------------------------------------------


def _server_and_laptop(git, user_home, staging, cfg, store, handover_root):
    server = build_mixed(user_home / "server")
    server.write("build/out.o", b"\x7fELF")
    with open(server.root / ".gitignore", "a") as fh:
        fh.write("build/\n")
    out, item = pack(git, server.root, staging, cfg, store, item_id="out")
    laptop = RepoBuilder(apply_fresh(git, store, item, handover_root / "laptop", cfg))
    return server, laptop, out.state


def test_apply_return_mode_with_handed_away_wip(git, user_home, staging, cfg, store, handover_root, tmp_path):
    """★ server → laptop → server: the server still holds the dirty state it handed away; return mode updates it
    in place (incl. untracked files and the stash), ignored build output and the excluded ``.env`` survive
    (mutant: the v1 "clean" precondition) [R4, SK H3]."""
    server, laptop, out_state = _server_and_laptop(git, user_home, staging, cfg, store, handover_root)
    laptop.write("mod.txt", "edited on the laptop\n")
    laptop.write("laptop_new.txt", "new\n")
    laptop.add("laptop_new.txt")
    laptop.write("committed_on_laptop.txt", "c\n")
    laptop.run("add", "committed_on_laptop.txt")
    laptop.run("commit", "-q", "--no-verify", "-m", "laptop commit", "--", "committed_on_laptop.txt", identity=True)
    laptop.remove("untracked.txt")
    basis = gitops.GitProbe(git).ref_tips(str(server.root))
    _, item = pack(git, laptop.root, staging, cfg, store, basis=basis, item_id="back")
    state = gitapply.prepare_return(
        git, store, item, server.root, out_state, tmp=tmp_path / "rt", cfg=cfg, qdir=staging / "q"
    )
    assert state.mode is DestMode.RETURN
    gitapply.commit(git, state, item)
    assert_same_state(laptop, server, excluded=EXCLUDED, ignored=[*IGNORED, b"build"])
    assert (server.root / "build" / "out.o").read_bytes() == b"\x7fELF"
    assert (server.root / ".env").read_text() == "API_KEY=1\n"
    assert (server.root / "ignored" / "cache.bin").exists()


def test_apply_return_mode_refuses_changed_state(git, user_home, staging, cfg, store, handover_root, tmp_path):
    """★ one edit on the server after the handover ⇒ no return mode (mutant: the out-state comparison is
    skipped) [R4]."""
    server, laptop, out_state = _server_and_laptop(git, user_home, staging, cfg, store, handover_root)
    server.write("untracked.txt", "edited on the server after the handover\n")
    before = dir_fingerprint(server.root)
    laptop.write("mod.txt", "laptop\n")
    _, item = pack(
        git, laptop.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(server.root)), item_id="back"
    )
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_return(
            git, store, item, server.root, out_state, tmp=tmp_path / "rt", cfg=cfg, qdir=staging / "q"
        )
    assert exc.value.details["kind"] == "changed_since_handover"
    assert "work tree changed" in exc.value.message
    assert dir_fingerprint(server.root) == before


def test_apply_return_rollback(git, user_home, staging, cfg, store, handover_root, tmp_path):
    server, laptop, out_state = _server_and_laptop(git, user_home, staging, cfg, store, handover_root)
    before = (
        server.head(),
        server.refs(),
        server.index_entries(),
        server.status(),
        server.worktree_digest(),
        server.stash_list(),
    )
    laptop.write("mod.txt", "laptop\n")
    laptop.write("only_laptop.txt", "x\n")
    _, item = pack(
        git, laptop.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(server.root)), item_id="back"
    )
    state = gitapply.prepare_return(
        git, store, item, server.root, out_state, tmp=tmp_path / "rt", cfg=cfg, qdir=staging / "q"
    )
    gitapply.commit(git, state, item)
    assert (server.root / "only_laptop.txt").exists()
    gitapply.rollback(git, state, item)
    after = (
        server.head(),
        server.refs(),
        server.index_entries(),
        server.status(),
        server.worktree_digest(),
        server.stash_list(),
    )
    assert after == before


# ---------------------------------------------------------------------------------------------------------
# filters, LFS, case, NTFS
# ---------------------------------------------------------------------------------------------------------


def test_git_user_filter_not_run_on_received_tree(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """★ a filter named by a received ``.gitattributes`` runs neither from the user's global config (fresh) nor
    from the destination's local config (update) (mutant: received profile keeps the user's config) [SEC 3.1]."""
    markers = tmp_path / "markers"
    markers.mkdir()
    smudge = marker_script(tmp_path / "smudge.sh", markers, "global-smudge")
    src = RepoBuilder.init(user_home / "src")
    src.write(".gitattributes", "*.txt filter=probe\n")
    src.write("a.txt", "content\n")
    src.commit_all("c1")
    src.write("b.txt", "more\n")
    write_global_gitconfig(user_home, f'[filter "probe"]\n\tsmudge = {smudge}\n\tclean = cat\n\trequired = true\n')
    _, item = pack(git, src.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, handover_root / "app", cfg)
    assert (dest / "a.txt").read_bytes() == b"content\n"
    assert list(markers.iterdir()) == []
    tgt = RepoBuilder(dest)
    tgt.run("reset", "-q", "--hard")
    tgt.run("clean", "-qfdx")
    local = marker_script(tmp_path / "local.sh", markers, "local-smudge")
    append_repo_config(dest, f'[filter "probe"]\n\tsmudge = {local}\n\tprocess = {local}\n\trequired = true\n')
    src.write("c.txt", "third\n")
    _, item2 = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(dest)), item_id="u")
    state = gitapply.prepare_update(git, store, item2, dest, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item2)
    assert (dest / "c.txt").read_bytes() == b"third\n"
    assert list(markers.iterdir()) == []


def _lfs_pointer(data: bytes) -> tuple[str, bytes]:
    import hashlib

    oid = hashlib.sha256(data).hexdigest()
    return oid, f"version https://git-lfs.github.com/spec/v1\noid sha256:{oid}\nsize {len(data)}\n".encode()


def test_apply_lfs_objects_oid_verified_no_network(
    git, user_home, tmp_path, staging, cfg, store, handover_root, no_lfs
):
    """LFS objects travel and are placed after ``sha256 == oid`` and size checks; a forged object is refused; the
    ``.lfsconfig`` URL is never contacted [SEC 3.5]. Without git-lfs (``no_lfs``: a ``PATH`` holding only git) the
    pointer stays — with git-lfs the fresh mode checks the content out (``test_rv_e_h8.py``)."""
    import socket

    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen(1)
    listener.settimeout(0.2)
    port = listener.getsockname()[1]
    src = RepoBuilder.init(user_home / "src")
    data = os.urandom(5000)
    oid, pointer = _lfs_pointer(data)
    src.write(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
    src.write(".lfsconfig", f"[lfs]\n\turl = http://127.0.0.1:{port}/lfs\n")
    src.write("asset.bin", pointer)
    src.commit_all("lfs")
    obj = src.root / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
    obj.parent.mkdir(parents=True)
    obj.write_bytes(data)
    res, item = pack(git, src.root, staging, cfg, store)
    assert item["lfs_objects"] and [o for o, _ in res.lfs_files] == [oid]
    dest = apply_fresh(git, store, item, handover_root / "app", cfg)
    placed = dest / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
    assert placed.read_bytes() == data
    assert (dest / "asset.bin").read_bytes() == pointer
    forged_body = os.urandom(5000)
    forged_obj = gitops.put_bytes(store, forged_body, "application/octet-stream").object_id
    other_oid, _ = _lfs_pointer(os.urandom(10))
    doc = {
        "kind": "ctxh.tree",
        "v": 1,
        "entries": [
            {"path": other_oid, "type": "file", "mode": "100644", "size": 5000, "mtime_ns": 0, "object": forged_obj}
        ],
    }
    from ctx_handover.engine.jsonutil import canonical_bytes

    manifest = gitops.put_bytes(store, canonical_bytes(doc), "application/json").object_id
    warnings = gitapply.place_lfs(git, dest, store, manifest)
    assert any("refused" in w for w in warnings)
    assert not (dest / ".git" / "lfs" / "objects" / other_oid[:2] / other_oid[2:4] / other_oid).exists()
    with pytest.raises(socket.timeout):
        listener.accept()
    listener.close()


def test_apply_case_collision_reported(git, user_home, staging, cfg, store, handover_root):
    """Simulated case-insensitive target rules: README/Readme are reported on fresh, update is refused [SK M8]."""
    src = RepoBuilder.init(user_home / "src")
    src.write("README", "A\n")
    src.write("Readme", "a\n")
    src.commit_all("c1")
    _, item = pack(git, src.root, staging, cfg, store)
    ci = rules_for("darwin", True)
    dest = handover_root / "app"
    state = gitapply.prepare_fresh(git, store, item, handover_root / ".ctxh-c", rules=ci, cfg=cfg, dest=dest)
    assert state.case_collisions == (("README", "Readme"),)
    assert any("case-colliding" in w for w in state.warnings)
    gitapply.commit(git, state, item)
    tgt = RepoBuilder(dest)
    src.write("other.txt", "x\n")
    _, item2 = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(dest)), item_id="u")
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_update(git, store, item2, tgt.root, cfg=cfg, qdir=staging / "q", rules=ci)
    assert exc.value.details["kind"] == "case_collision"


def test_apply_ntfs_invalid_names_no_checkout(git, user_home, staging, cfg, store, handover_root):
    """Names invalid on NTFS ⇒ objects and refs only, no checkout, and the reason in the warnings [BX M13]."""
    src = RepoBuilder.init(user_home / "src")
    src.write("aux.txt", "reserved on windows\n")
    src.write("ok.txt", "fine\n")
    src.commit_all("c1")
    _, item = pack(git, src.root, staging, cfg, store)
    win = rules_for("windows", True)
    state = gitapply.prepare_fresh(
        git, store, item, handover_root / ".ctxh-w", rules=win, cfg=cfg, dest=handover_root / "app"
    )
    assert state.mode is DestMode.NO_CHECKOUT
    assert any("aux.txt" in w for w in state.warnings)
    final = gitapply.commit(git, state, item)
    assert sorted(p.name for p in final.iterdir()) == [".git"]
    assert RepoBuilder(final).head() == src.head()


def test_git_no_submodule_recursion_on_update(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """``submodule.recurse=true`` and an active submodule in the destination's own config change nothing: the
    gitlink stays an empty directory, nothing is cloned [SEC 3.3]."""
    sub = RepoBuilder.init(user_home / "subproject")
    sub.write("s.txt", "s\n")
    sub_head = sub.commit_all("sub")
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.write(".gitmodules", '[submodule "lib"]\n\tpath = lib\n\turl = https://example.invalid/lib.git\n')
    src.run("update-index", "--add", "--cacheinfo", f"160000,{sub_head},lib")
    src.commit_all("with gitlink")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    append_repo_config(
        tgt.root, f'[submodule]\n\trecurse = true\n[submodule "lib"]\n\turl = {sub.root}\n\tactive = true\n'
    )
    src.write("b.txt", "b\n")
    _, item = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(tgt.root)), item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    assert (tgt.root / "b.txt").exists()
    assert not (tgt.root / "lib" / "s.txt").exists() and not (tgt.root / ".git" / "modules").exists()


def test_sec_gitmodules_ignored(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """A received ``.gitmodules`` with an ``ext::`` URL and an ``update = !command`` never runs anything: the strict
    quarantine rejects it, nothing reaches the destination [T8, R21]."""
    markers = tmp_path / "markers"
    markers.mkdir()
    sub = RepoBuilder.init(user_home / "subproject")
    sub.write("s.txt", "s\n")
    sub_head = sub.commit_all("sub")
    src = RepoBuilder.init(user_home / "src")
    script = marker_script(tmp_path / "upd.sh", markers, "submodule-update")
    src.write(
        ".gitmodules",
        f'[submodule "lib"]\n\tpath = lib\n\turl = ext::sh -c touch% {markers}/ext\n\tupdate = !{script}\n',
    )
    src.run("update-index", "--add", "--cacheinfo", f"160000,{sub_head},lib")
    src.commit_all("hostile gitlink")
    _, item = pack(git, src.root, staging, cfg, store)
    with pytest.raises(VerifyFailed):
        apply_fresh(git, store, item, handover_root / "app", cfg)
    assert list(handover_root.iterdir()) == []
    assert list(markers.iterdir()) == []


def test_remote_config_written_with_five_keys(git, user_home, staging, cfg, store, handover_root):
    """Remotes and upstreams arrive as exactly the five allowed keys, credentials stripped [SEC 3.4]."""
    src = build_mixed(user_home / "src")
    append_repo_config(
        src.root,
        '[remote "origin"]\n\turl = https://alice:s3cret@example.org/o/r.git\n\tpushurl = git@example.org:o/r.git\n'
        "\tfetch = +refs/heads/*:refs/remotes/origin/*\n\tuploadpack = /bin/evil\n"
        '[remote "local"]\n\turl = /srv/repo.git\n[branch "main"]\n\tremote = origin\n\tmerge = refs/heads/main\n',
    )
    _, item = pack(git, src.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, handover_root / "app", cfg)
    keys = git.out(["config", "--file", str(dest / ".git" / "config"), "--list"], cwd=None, spec=gitops.RECEIVED)
    added = sorted(line for line in keys.split("\n") if line.startswith(("remote.", "branch.")))
    assert added == [
        "branch.main.merge=refs/heads/main",
        "branch.main.remote=origin",
        "remote.origin.fetch=+refs/heads/*:refs/remotes/origin/*",
        "remote.origin.pushurl=git@example.org:o/r.git",
        "remote.origin.url=https://example.org/o/r.git",
    ]
    assert "s3cret" not in (dest / ".git" / "config").read_text()


def test_discard_removes_partial_and_quarantine(git, user_home, staging, cfg, store, handover_root):
    src = build_mixed(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    partial = handover_root / ".ctxh-d"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    gitapply.discard(git, state)
    assert not partial.exists() and list(handover_root.iterdir()) == []
    shutil.rmtree(staging, ignore_errors=True)
    assert (
        gitpack.pack_item(
            "x",
            gitpack.pack_repo(
                git,
                str(src.root),
                staging / "g",
                basis_candidates=(),
                cfg=cfg,
                store=store,
                created_at=1.0,
                force=False,
            ),
        )["kind"]
        == "git_repo"
    )


def test_apply_lfs_update_mode_places_objects(git, user_home, staging, cfg, store, handover_root):
    """Update mode places new LFS objects (verified) into the destination's LFS store during PREPARE."""
    src = RepoBuilder.init(user_home / "src")
    src.write(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    data = os.urandom(3000)
    oid, pointer = _lfs_pointer(data)
    src.write("asset.bin", pointer)
    obj = src.root / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
    obj.parent.mkdir(parents=True)
    obj.write_bytes(data)
    _, item = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(tgt.root)), item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    placed = tgt.root / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
    assert placed.read_bytes() == data and not (tgt.root / "asset.bin").exists()
    gitapply.commit(git, state, item)
    assert (tgt.root / "asset.bin").read_bytes() == pointer
