"""Source side: packing a repository without touching it (DESIGN §9.3, §9.4, R5, R23)."""

from __future__ import annotations

import os
import subprocess
import time

import pytest

from ctx_handover.engine import gitops, gitpack
from ctx_handover.engine.errors import PolicyDenied
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.unit.E._support import AT, apply_fresh, build_mixed, make_cfg, pack


def _tree(git, repo, rev):
    """``{path: (mode, type, oid)}`` of a commit's tree in the pack's temp repository."""
    out = {}
    for rec in git.iter_z(
        ["--git-dir", str(repo), "ls-tree", "-r", "-z", "--full-tree", rev], cwd=None, spec=gitops.RECEIVED
    ):
        meta, _, path = rec.partition(b"\t")
        mode, kind, oid = meta.split()
        out[path] = (mode.decode(), kind.decode(), oid.decode())
    return out


def _blob(git, repo, oid):
    return git.run(["--git-dir", str(repo), "cat-file", "blob", oid], cwd=None, spec=gitops.RECEIVED).stdout


def test_pack_without_git_identity(git, user_home, staging, cfg, store, monkeypatch):
    """★ no identity anywhere (no user config, system config off): packing succeeds and I/W carry the technical
    identity and the job time (mutant: no technical identity on commit-tree) [R5, SK H4]."""
    monkeypatch.setenv("GIT_CONFIG_GLOBAL", os.devnull)
    monkeypatch.setenv("GIT_CONFIG_NOSYSTEM", "1")
    monkeypatch.delenv("EMAIL", raising=False)
    r = build_mixed(user_home / "src")
    res, _ = pack(git, r.root, staging, cfg, store, at=AT)
    repo = staging / "git" / "w0" / "repo"
    for sha in (res.index_commit, res.worktree_commit):
        fmt = git.out(
            ["--git-dir", str(repo), "show", "-s", "--format=%an|%ae|%at|%cn|%ce|%ct", sha],
            cwd=None,
            spec=gitops.RECEIVED,
        )
        name, email = gitops.TECH_IDENTITY
        assert fmt == f"{name}|{email}|{int(AT)}|{name}|{email}|{int(AT)}"
    assert "user." not in (r.root / ".git" / "config").read_text()


def test_pack_does_not_modify_source_repo(git, user_home, staging, cfg, store):
    """★ refs, index, objects, config and work tree of the source are byte-identical before and after packing
    (mutant: the work-tree add runs on the repository's own index)."""
    r = build_mixed(user_home / "src")
    before = dir_fingerprint(r.root)
    refs, status = r.refs(), r.status()
    res, _ = pack(git, r.root, staging, cfg, store)
    assert dir_fingerprint(r.root) == before
    assert r.refs() == refs and r.status() == status
    objects = r.root / ".git" / "objects"
    for sha in (res.index_commit, res.worktree_commit, res.state.worktree_tree):
        assert not (objects / sha[:2] / sha[2:]).exists()


def test_pack_index_and_worktree_commits(git, user_home, staging, cfg, store):
    """I = the index (staged new, deleted, renamed, mode change as staged), W = the work tree (unstaged edits,
    untracked files, worktree deletions, exec bit, symlink, non-UTF-8 and newline names) on top of I."""
    r = build_mixed(user_home / "src")
    res, _ = pack(git, r.root, staging, cfg, store)
    repo = staging / "git" / "w0" / "repo"
    head = r.head()
    assert res.head == {"ref": "refs/heads/main", "commit": head}
    assert (
        git.out(["--git-dir", str(repo), "rev-parse", res.index_commit + "^"], cwd=None, spec=gitops.RECEIVED) == head
    )
    assert (
        git.out(["--git-dir", str(repo), "rev-parse", res.worktree_commit + "^"], cwd=None, spec=gitops.RECEIVED)
        == res.index_commit
    )
    i_tree, w_tree = _tree(git, repo, res.index_commit), _tree(git, repo, res.worktree_commit)
    index = {}
    for rec in r.index_entries().split(b"\x00"):
        if rec:
            meta, _, path = rec.partition(b"\t")
            mode, oid, _stage = meta.split()
            index[path] = (mode.decode(), oid.decode())
    assert {p: (m, o) for p, (m, _k, o) in i_tree.items()} == index
    assert b"del.txt" not in i_tree and b"renamed.txt" in i_tree and b"ren.txt" not in i_tree
    assert _blob(git, repo, i_tree[b"staged_new.txt"][2]) == b"s1\n"
    assert _blob(git, repo, w_tree[b"staged_new.txt"][2]) == b"s2\n"
    assert _blob(git, repo, w_tree[b"mod.txt"][2]) == b"b3\n"
    assert w_tree[b"tool.sh"][0] == "100755" and i_tree[b"tool.sh"][0] == "100644"
    assert w_tree[b"link"][0] == "120000"
    for name in (b"untracked.txt", b"dir/untracked_deep.txt", b"n\xe4me.txt", b"new\nline.txt"):
        assert name in w_tree and name not in i_tree
    assert b"gone.txt" in i_tree and b"gone.txt" not in w_tree
    for name in (b".env", b"build.log", b"ignored/cache.bin"):
        assert name not in w_tree
    assert [s["message"] for s in res.stash] == ["On main: wip on mod"]
    assert set(res.refs) == {"refs/heads/main", "refs/heads/feature", "refs/tags/v1", "refs/tags/v1-annotated"}


def test_pack_denied_untracked_staged_and_stash_secret(git, user_home, staging, cfg, store):
    """★ untracked ``.env``, staged ``.env.local`` and a ``stash -u`` holding ``.env`` are withheld and listed; a
    clean stash entry travels (mutant: stash entries not filtered) [R23, SEC 5.5]."""
    r = RepoBuilder.init(user_home / "src")
    r.write("app.py", "print(1)\n")
    r.commit_all("c1")
    r.write("notes.txt", "clean wip\n")
    r.stash("clean", untracked=True)
    r.write(".env", "TOKEN=abc\n")
    r.write("other.txt", "x\n")
    r.stash("secret wip", untracked=True)
    r.write(".env", "TOKEN=def\n")
    r.write(".env.local", "PASSWORD=1\n")
    r.add(".env.local")
    r.write("key.txt", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n")
    res, item = pack(git, r.root, staging, cfg, store)
    excluded = {(n.path, n.why) for n in res.excluded}
    assert (".env", "secret") in excluded and (".env.local", "secret") in excluded
    assert ("key.txt", "secret") in excluded
    withheld = [n for n in res.excluded if n.why == "stash_secret"]
    assert len(withheld) == 1 and "secret wip" in withheld[0].path and ".env" in withheld[0].path
    assert [s["message"] for s in item["stash"]] == ["On main: clean"]
    repo = staging / "git" / "w0" / "repo"
    for rev in (res.index_commit, res.worktree_commit):
        tree = _tree(git, repo, rev)
        assert b".env" not in tree and b".env.local" not in tree and b"key.txt" not in tree
    secret_stash = r.stash_list()[0][0]
    dest = apply_fresh(git, store, item, user_home / "hermes-handover" / "app", cfg)
    t = RepoBuilder(dest)
    assert [m for _, m in t.stash_list()] == ["On main: clean"]
    probe = git.run(["cat-file", "-e", secret_stash], cwd=dest, spec=gitops.RECEIVED, check=False)
    assert probe.returncode != 0
    assert not (dest / ".env").exists() and not (dest / ".env.local").exists()


def test_pack_include_secret_releases_named_path(git, user_home, staging, hermes_home, store):
    """A path released locally with --include-secret travels; nothing else does."""
    r = RepoBuilder.init(user_home / "src")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    r.write(".env", "X=1\n")
    r.write(".env.prod", "X=2\n")
    cfg = make_cfg(user_home, hermes_home, include_secret=(str(r.root / ".env"),))
    res, _ = pack(git, r.root, staging, cfg, store)
    assert [n.path for n in res.excluded] == [".env.prod"]


def test_pack_large_untracked_excluded(git, user_home, staging, hermes_home, store):
    """Untracked files above max_file_mb are skipped and listed; ``include`` lifts it per path [SK M9]."""
    cfg = make_cfg(user_home, hermes_home, max_file_bytes=1000)
    r = RepoBuilder.init(user_home / "src")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    r.write("big.bin", b"\x01" * 4000)
    res, _ = pack(git, r.root, staging, cfg, store)
    assert [(n.path, n.why) for n in res.skipped] == [("big.bin", "size")]
    assert b"big.bin" not in _tree(git, staging / "git" / "w0" / "repo", res.worktree_commit)
    res2, _ = pack(git, r.root, staging, cfg, store, item_id="w1", include=frozenset({str(r.root / "big.bin")}))
    assert res2.skipped == ()
    assert b"big.bin" in _tree(git, staging / "git" / "w1" / "repo", res2.worktree_commit)


def test_pack_untracked_count_gate(git, user_home, staging, hermes_home, store):
    """More untracked files than max_untracked_files ⇒ refused with the fullest directories named [SK M9]."""
    cfg = make_cfg(user_home, hermes_home, max_untracked_files=5)
    r = RepoBuilder.init(user_home / "src")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    for i in range(6):
        r.write(f"gen/f{i}.txt", "x")
    r.write("other/x.txt", "x")
    r.write("other/y.txt", "y")
    with pytest.raises(PolicyDenied) as exc:
        pack(git, r.root, staging, cfg, store)
    assert exc.value.details["reason"] == "untracked_count"
    assert exc.value.details["top_dirs"][0] == "gen"


def test_repack_reuses_cached_pack(git, user_home, staging, cfg, store, monkeypatch):
    """★ a repeat (resume after suspension) reuses the stored pack object, and a re-pack from scratch later yields
    the same I/W ids (mutant: commit time from the wall clock instead of the job's creation time) [SK M2]."""
    r = build_mixed(user_home / "src")
    first, _ = pack(git, r.root, staging, cfg, store)
    calls: list[list] = []
    real = gitops.Git.popen_stdout

    def spy(self, args, **kw):
        calls.append(list(args))
        return real(self, args, **kw)

    monkeypatch.setattr(gitops.Git, "popen_stdout", spy)
    again, _ = pack(git, r.root, staging, cfg, store)
    assert again.reused is True
    assert not [c for c in calls if "pack-objects" in c]
    assert (again.index_commit, again.worktree_commit, again.pack_object) == (
        first.index_commit,
        first.worktree_commit,
        first.pack_object,
    )
    time.sleep(1.1)
    later, _ = pack(git, r.root, staging, cfg, store, workdir=staging / "git" / "fresh")
    assert later.reused is False
    assert (later.index_commit, later.worktree_commit) == (first.index_commit, first.worktree_commit)
    r.write("untracked.txt", "changed\n")
    changed, _ = pack(git, r.root, staging, cfg, store)
    assert changed.reused is False and changed.worktree_commit != first.worktree_commit


def test_pack_incremental_basis_and_empty_pack(git, user_home, staging, cfg, store):
    """The basis excludes the target's objects (thin pack); a stream whose objects are all covered is ``None``
    (``pack: null``) [SK M1]."""
    r = RepoBuilder.init(user_home / "src")
    for i in range(30):
        r.write(f"f{i}.txt", os.urandom(2048).hex())
    base = r.commit_all("base")
    full, _ = pack(git, r.root, staging, cfg, store, item_id="full")
    r.write("f0.txt", "changed\n")
    r.commit_all("next")
    inc, _ = pack(git, r.root, staging, cfg, store, basis=[base, "0" * 40], item_id="inc")
    assert inc.basis == (base,)
    full_size = store.manifests([full.pack_object])[0].size
    inc_size = store.manifests([inc.pack_object])[0].size
    assert inc_size < full_size / 5
    repo = staging / "git" / "inc" / "repo"
    assert gitpack._stream_pack(git, repo, [base], [base], store, lambda: False) is None


def test_pack_refuses_in_progress_and_shallow(git, user_home, staging, cfg, store):
    from ctx_handover.engine.errors import CapabilityMissing, ConflictError

    r = RepoBuilder.init(user_home / "src")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    (r.root / ".git" / "MERGE_HEAD").write_text(r.head() + "\n")
    with pytest.raises(ConflictError):
        pack(git, r.root, staging, cfg, store)
    res = gitpack.pack_repo(
        git,
        str(r.root),
        staging / "git" / "forced",
        basis_candidates=(),
        cfg=cfg,
        store=store,
        created_at=AT,
        force=True,
    )
    assert any("merge" in w for w in res.warnings)
    (r.root / ".git" / "MERGE_HEAD").unlink()
    (r.root / ".git" / "shallow").write_text(r.head() + "\n")
    with pytest.raises(CapabilityMissing):
        pack(git, r.root, staging, cfg, store, item_id="s")


def test_pack_nested_repository_left_out(git, user_home, staging, cfg, store):
    r = RepoBuilder.init(user_home / "src")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    RepoBuilder.init(r.root / "vendor" / "lib").write("x", "1")
    res, _ = pack(git, r.root, staging, cfg, store)
    assert ("vendor/lib", "nested_repo") in {(n.path, n.why) for n in res.excluded}


def test_recheck_detects_changes(git, user_home, staging, cfg, store, tmp_path):
    """Prepare re-check (§9.3 step 8): equal while untouched, different after any change [SK M10]."""
    r = build_mixed(user_home / "src")
    res, _ = pack(git, r.root, staging, cfg, store)
    assert gitpack.recheck(git, str(r.root), res.state, tmp_path)[0] is True
    r.write("dir/sub/deep.txt", "edited\n")
    same, now = gitpack.recheck(git, str(r.root), res.state, tmp_path)
    assert same is False and now.worktree_tree != res.state.worktree_tree


def test_pack_runs_without_shell(git, user_home, staging, cfg, store, monkeypatch):
    """Every git process starts from an argv list without a shell (DESIGN §9.2)."""
    seen = []
    real = subprocess.Popen

    def spy(argv, *a, **kw):
        seen.append((argv, kw.get("shell", False), kw.get("stdin")))
        return real(argv, *a, **kw)

    monkeypatch.setattr(gitops.subprocess, "Popen", spy)
    r = build_mixed(user_home / "src")
    seen.clear()
    pack(git, r.root, staging, cfg, store)
    assert seen and all(isinstance(a, list) and shell is False for a, shell, _ in seen)
