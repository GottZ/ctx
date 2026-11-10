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
[ "$1" = checkout ] || exit 0
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


@pytest.fixture
def vulnerable_lfs(tmp_path, monkeypatch) -> Path:
    bindir = tmp_path / "lfs-bin"
    bindir.mkdir()
    log = tmp_path / "lfs.log"
    log.write_text("", encoding="utf-8")
    script = bindir / "git-lfs"
    script.write_text(VULNERABLE.format(log=log), encoding="utf-8")
    os.chmod(script, 0o755)  # noqa: S103 - executable test stand-in
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ.get('PATH', '')}")
    return log


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
