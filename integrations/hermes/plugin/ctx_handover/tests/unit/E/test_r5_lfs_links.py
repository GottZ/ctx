"""Catalog review of 2026-11-10, point 2: ``git lfs checkout`` runs in a received work tree, and git-lfs before 3.7.1
follows a symbolic link when it writes a file there (CVE-2025-26625, GHSA-6pvw-g552-53c5): a link whose path is or
leads to a tracked LFS file sends the write outside the repository. The plugin therefore skips its LFS checkout
when a path git-lfs may write — every path of HEAD, of the index and of the received trees — is a symbolic link or
passes through one, and says so; the pointers stay.

``git-lfs`` here is a stand-in that behaves like the vulnerable releases: for each index entry whose blob is a
pointer with a local object it copies the object to the path, through any link on the way, also when the file
does not exist yet."""

from __future__ import annotations

import hashlib
import os
import shutil
from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import LINUX, apply_fresh, pack

DATA = b"LFS CONTENT FROM THE PEER\n" * 32

VULNERABLE = """#!/bin/sh
echo "$*" >> "{log}"
[ "$1" = version ] && {{ [ -n "{version}" ] && echo "git-lfs/{version} (stand-in)"; exit 0; }}
[ "$1" = checkout ] || exit 0
if [ -z "$(git config --get filter.lfs.process)$(git config --get filter.lfs.clean)" ]; then
  echo "Cannot checkout LFS objects, Git LFS is not installed."; exit 0
fi
git ls-files | while IFS= read -r f; do
  oid=$(git cat-file blob ":$f" 2>/dev/null | sed -n 's/^oid sha256://p')
  [ -n "$oid" ] || continue
  a=$(printf %s "$oid" | cut -c1-2)
  b=$(printf %s "$oid" | cut -c3-4)
  obj="$(git rev-parse --git-common-dir)/lfs/objects/$a/$b/$oid"
  [ -f "$obj" ] && cp "$obj" "$f"
done
exit 0
"""


def _stand_in(tmp_path: Path, monkeypatch, version: str) -> Path:
    bindir = tmp_path / "lfs-bin"
    bindir.mkdir()
    log = tmp_path / "lfs.log"
    log.write_text("", encoding="utf-8")
    script = bindir / "git-lfs"
    script.write_text(VULNERABLE.format(log=log, version=version), encoding="utf-8")
    os.chmod(script, 0o755)  # noqa: S103 - executable test stand-in
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ.get('PATH', '')}")
    return log


@pytest.fixture
def vulnerable_lfs(tmp_path, monkeypatch) -> Path:
    """A stand-in that reports 3.7.0 (a release that writes through links)."""
    return _stand_in(tmp_path, monkeypatch, "3.7.0")


def _pointer(data: bytes) -> tuple[str, bytes]:
    oid = hashlib.sha256(data).hexdigest()
    return oid, f"version https://git-lfs.github.com/spec/v1\noid sha256:{oid}\nsize {len(data)}\n".encode()


def _add_lfs(src: RepoBuilder, rel: str) -> bytes:
    oid, pointer = _pointer(DATA)
    src.write(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
    src.write(rel, pointer)
    obj = src.root / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
    obj.parent.mkdir(parents=True, exist_ok=True)
    obj.write_bytes(DATA)
    return pointer


def _ran_checkout(log: Path) -> bool:
    return "checkout" in log.read_text(encoding="utf-8")


def test_r5_fresh_link_in_the_work_tree_skips_the_lfs_checkout(
    git, user_home, tmp_path, staging, cfg, store, handover_root, vulnerable_lfs
):
    """★ The index names ``d/x.bin`` (an LFS pointer), the work tree holds ``d`` as a link to a directory outside the
    repository. Before the fix the plugin ran ``git lfs checkout`` there and the peer's object landed in that outside
    directory; now the checkout is skipped with a warning and nothing is written outside."""
    outside = tmp_path / "outside"
    outside.mkdir()
    src = RepoBuilder.init(user_home / "src")
    _add_lfs(src, "d/x.bin")
    src.commit_all("lfs")
    shutil.rmtree(src.root / "d")
    src.symlink("d", str(outside))
    _, item = pack(git, src.root, staging, cfg, store)
    assert item["lfs_objects"]
    partial = handover_root / ".ctxh-app"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert not (outside / "x.bin").exists(), "the LFS checkout wrote through the link"
    assert not _ran_checkout(vulnerable_lfs)
    assert any("symbolic link" in w and "'d'" in w for w in state.warnings), state.warnings
    gitapply.discard(git, state)


def test_r5_fresh_any_link_in_a_received_tree_skips_the_lfs_checkout(
    git, user_home, staging, cfg, store, handover_root, vulnerable_lfs
):
    """A link anywhere in the received trees is enough (on a case-insensitive filesystem a link can alias a
    pointer path under another spelling); the pointers stay and the warning names the link."""
    src = RepoBuilder.init(user_home / "src")
    pointer = _add_lfs(src, "asset.bin")
    src.symlink("docs", "asset.bin")
    src.commit_all("lfs")
    _, item = pack(git, src.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, handover_root / "app", cfg)
    assert (dest / "asset.bin").read_bytes() == pointer
    assert not _ran_checkout(vulnerable_lfs)


def test_r5_fresh_without_links_still_checks_out(git, user_home, staging, cfg, store, handover_root, vulnerable_lfs):
    """The rule costs nothing where there is no link: the LFS content arrives as before (H8)."""
    src = RepoBuilder.init(user_home / "src")
    _add_lfs(src, "asset.bin")
    src.commit_all("lfs")
    _, item = pack(git, src.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, handover_root / "app", cfg)
    assert (dest / "asset.bin").read_bytes() == DATA
    assert _ran_checkout(vulnerable_lfs)


def test_r5_update_ignored_link_on_the_way_skips_the_lfs_checkout(
    git, user_home, tmp_path, staging, cfg, store, handover_root, vulnerable_lfs
):
    """★ Update mode: the destination has an ignored link ``data`` to a directory outside; the source's HEAD and
    index name ``data/x.bin`` (an LFS pointer) while its work tree deleted it, so git's own checkout never touches
    ``data``. Before the fix the c1 tail's ``git lfs checkout`` wrote the object through the link; now PREPARE warns
    and the tail skips the checkout."""
    outside = tmp_path / "outside"
    outside.mkdir()
    src = RepoBuilder.init(user_home / "src")
    src.write(".gitignore", "/data\n")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    _, seed = pack(git, src.root, staging, cfg, store, item_id="seed")
    tgt = RepoBuilder(apply_fresh(git, store, seed, handover_root / "app", cfg))
    os.symlink(outside, tgt.root / "data")
    _add_lfs(src, "data/x.bin")
    src.run("add", "-f", "data/x.bin", ".gitattributes")
    src.commit("lfs under an ignored directory")
    (src.root / "data" / "x.bin").unlink()
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    assert not (outside / "x.bin").exists(), "the LFS checkout wrote through the link"
    assert not _ran_checkout(vulnerable_lfs)
    assert any("symbolic link" in w and "'data'" in w for w in state.warnings), state.warnings


def test_r5_link_check_reads_head_and_index_of_the_repository(git, tmp_path, vulnerable_lfs):
    """``lfs_checkout`` on its own (``place_lfs`` with checkout): the paths of HEAD and of the index are checked on
    disk, so a link the caller's trees do not mention still stops it."""
    outside = tmp_path / "outside"
    outside.mkdir()
    repo = RepoBuilder.init(tmp_path / "repo")
    _add_lfs(repo, "d/x.bin")
    repo.commit_all("lfs")
    shutil.rmtree(repo.root / "d")
    os.symlink(outside, repo.root / "d")
    warnings = gitapply.lfs_checkout(git, repo.root)
    assert any("symbolic link" in w for w in warnings), warnings
    assert not (outside / "x.bin").exists()
    assert not _ran_checkout(vulnerable_lfs)


REAL_ENV = "CTXH_REAL_GIT_LFS"
"""Path of a real ``git-lfs`` executable for the opt-in tests below (none ships with the test tree)."""


@pytest.fixture
def real_lfs(tmp_path, monkeypatch) -> Path:
    exe = os.environ.get(REAL_ENV, "")
    if not exe or not os.access(exe, os.X_OK):
        pytest.skip(f"set {REAL_ENV} to a git-lfs executable to run the real-binary checks")
    bindir = tmp_path / "real-lfs-bin"
    bindir.mkdir()
    os.symlink(exe, bindir / "git-lfs")
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ.get('PATH', '')}")
    return bindir


def test_r5_real_git_lfs_fills_the_files(git, user_home, staging, cfg, store, handover_root, real_lfs):
    """Opt-in, real git-lfs: the plugin's checkout fills the file. Before the declared filter git-lfs answered "Git
    LFS is not installed" (exit code 0) in the received profile and the pointer stayed without a warning."""
    src = RepoBuilder.init(user_home / "src")
    _add_lfs(src, "asset.bin")
    src.commit_all("lfs")
    _, item = pack(git, src.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, handover_root / "app", cfg)
    assert (dest / "asset.bin").read_bytes() == DATA


def test_r5_real_git_lfs_link_stops_the_checkout(
    git, user_home, tmp_path, staging, cfg, store, handover_root, real_lfs
):
    """Opt-in, real git-lfs (also a release before 3.7.1): a link in the received work tree stops the checkout and
    nothing lands outside."""
    outside = tmp_path / "outside"
    outside.mkdir()
    src = RepoBuilder.init(user_home / "src")
    _add_lfs(src, "d/x.bin")
    src.commit_all("lfs")
    shutil.rmtree(src.root / "d")
    src.symlink("d", str(outside))
    _, item = pack(git, src.root, staging, cfg, store)
    partial = handover_root / ".ctxh-app"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert not (outside / "x.bin").exists()
    assert any("symbolic link" in w for w in state.warnings), state.warnings
    gitapply.discard(git, state)


def test_r5_update_new_link_in_the_received_work_tree(
    git, user_home, staging, cfg, store, handover_root, vulnerable_lfs
):
    """★ Update mode, a link that exists only in the received work tree (an untracked link on the source, so neither
    HEAD nor the index names it): PREPARE sees it only in the tree listing (it is not on disk there yet) and warns,
    and the c1 tail, which checks the same trees, keeps the pointers — the warning and what happens agree."""
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    _, seed = pack(git, src.root, staging, cfg, store, item_id="seed")
    tgt = RepoBuilder(apply_fresh(git, store, seed, handover_root / "app", cfg))
    pointer = _add_lfs(src, "asset.bin")
    src.commit_all("lfs")
    src.symlink("docs", "asset.bin")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert any("symbolic link" in w and "'docs'" in w and "3.7.0" in w for w in state.warnings), state.warnings
    gitapply.commit(git, state, item)
    assert (tgt.root / "docs").is_symlink()
    assert (tgt.root / "asset.bin").read_bytes() == pointer
    assert not _ran_checkout(vulnerable_lfs)


def _repo_with_link_dir(tmp_path: Path, outside: Path) -> RepoBuilder:
    repo = RepoBuilder.init(tmp_path / "repo")
    repo.write("a.txt", "a\n")
    repo.commit_all("c1")
    _add_lfs(repo, "d/x.bin")
    return repo


def test_r5_index_only_path_is_checked(git, tmp_path, vulnerable_lfs):
    """A pointer only staged (the index names ``d/x.bin``, HEAD does not) behind a link: the index listing alone
    stops the checkout, and nothing lands outside."""
    outside = tmp_path / "outside"
    outside.mkdir()
    repo = _repo_with_link_dir(tmp_path, outside)
    repo.add("d/x.bin", ".gitattributes")
    shutil.rmtree(repo.root / "d")
    os.symlink(outside, repo.root / "d")
    warnings = gitapply.lfs_checkout(git, repo.root)
    assert any("'d'" in w for w in warnings), warnings
    assert not (outside / "x.bin").exists()
    assert not _ran_checkout(vulnerable_lfs)


def test_r5_head_only_path_is_checked(git, tmp_path, vulnerable_lfs):
    """HEAD names ``d/x.bin``, the index no longer does (git-lfs releases differ in which of the two they scan): the
    HEAD listing alone stops the checkout."""
    outside = tmp_path / "outside"
    outside.mkdir()
    repo = _repo_with_link_dir(tmp_path, outside)
    repo.commit_all("lfs")
    repo.run("rm", "-q", "--cached", "-r", "d")
    shutil.rmtree(repo.root / "d")
    os.symlink(outside, repo.root / "d")
    warnings = gitapply.lfs_checkout(git, repo.root)
    assert any("'d'" in w for w in warnings), warnings
    assert not _ran_checkout(vulnerable_lfs)


def test_r5_unlistable_trees_stop_the_checkout(git, tmp_path, vulnerable_lfs):
    """Fail-closed: a tree that cannot be listed counts as a link in the way."""
    repo = RepoBuilder.init(tmp_path / "repo")
    _add_lfs(repo, "asset.bin")
    repo.commit_all("lfs")
    warnings = gitapply.lfs_checkout(git, repo.root, ("0" * 40,))
    assert any("could not be checked" in w for w in warnings), warnings
    assert not _ran_checkout(vulnerable_lfs)


def test_r5_uninspectable_path_stops_the_checkout(git, tmp_path, vulnerable_lfs):
    """Fail-closed: an index path the filesystem cannot inspect (a name longer than any filesystem allows) counts as
    a link in the way."""
    repo = RepoBuilder.init(tmp_path / "repo")
    _add_lfs(repo, "asset.bin")
    repo.commit_all("lfs")
    blob = repo.git.out(["hash-object", "-w", "--stdin"], cwd=repo.root, spec=gitops.RECEIVED, input=b"x\n")
    long_name = "n" * 300
    repo.run("update-index", "--add", "--cacheinfo", f"100644,{blob},{long_name}")
    warnings = gitapply.lfs_checkout(git, repo.root)
    assert any(long_name in w for w in warnings), warnings
    assert not _ran_checkout(vulnerable_lfs)


def test_r5_windows_junction_counts_as_a_link(git, tmp_path, vulnerable_lfs, monkeypatch):
    """A directory that is a Windows junction (reparse point) on the way counts like a symbolic link (simulated:
    ``lstat`` reports the reparse tag for ``d``)."""
    repo = RepoBuilder.init(tmp_path / "repo")
    _add_lfs(repo, "d/x.bin")
    repo.commit_all("lfs")
    junction = 0xA0000003
    monkeypatch.setattr(gitapply, "_REPARSE_LINKS", frozenset({junction}))
    real_lstat = os.lstat

    class _Junction:
        def __init__(self, st):
            self.st_mode = st.st_mode
            self.st_reparse_tag = junction

    def fake_lstat(path, *a, **kw):
        st = real_lstat(path, *a, **kw)
        return _Junction(st) if os.path.basename(os.fspath(path)) == "d" else st

    monkeypatch.setattr(gitapply.os, "lstat", fake_lstat)
    warnings = gitapply.lfs_checkout(git, repo.root)
    monkeypatch.setattr(gitapply.os, "lstat", real_lstat)
    assert any("'d'" in w for w in warnings), warnings
    assert not _ran_checkout(vulnerable_lfs)


@pytest.mark.parametrize(("version", "runs"), [("3.7.1", True), ("4.0.0", True), ("3.7.0", False), ("", False)])
def test_r5_git_lfs_from_3_7_1_runs_despite_a_link(git, user_home, tmp_path, monkeypatch, version, runs):
    """git-lfs 3.7.1 and newer check for links themselves: the checkout runs with a link in the tree. Older or
    unreadable versions keep the pointers and the warning names the version (or that it could not be read)."""
    log = _stand_in(tmp_path, monkeypatch, version)
    repo = RepoBuilder.init(tmp_path / "repo")
    pointer = _add_lfs(repo, "asset.bin")
    repo.symlink("docs", "asset.bin")
    repo.commit_all("lfs")
    warnings = gitapply.lfs_checkout(git, repo.root)
    assert _ran_checkout(log) is runs
    if runs:
        assert warnings == [] and (repo.root / "asset.bin").read_bytes() == DATA
    else:
        assert (repo.root / "asset.bin").read_bytes() == pointer
        assert any(("3.7.0 here is older" in w) if version else ("could not be read" in w) for w in warnings)
