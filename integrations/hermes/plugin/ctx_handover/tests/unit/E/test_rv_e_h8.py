"""First review round, finding H8 (and the condition M26 asks for): with git-lfs present, a fresh destination gets
the LFS content of the shipped objects, not the pointer text; without git-lfs every mode says the pointers stay
(DESIGN §9.4, SEC 3.5). ``git-lfs`` here is a stand-in script that does what ``git lfs checkout`` does locally:
it replaces each pointer file whose object lies in ``.git/lfs/objects`` by that object (no network), and like
git-lfs it fills nothing (exit code 0) when no LFS filter is configured for the call."""

from __future__ import annotations

import hashlib
import os
from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import LINUX, apply_fresh, pack

DATA = b"REAL LFS CONTENT\n" * 64

STAND_IN = """#!/bin/sh
echo "$*" >> "{log}"
[ "$1" = checkout ] || exit 0
if [ -z "$(git config --get filter.lfs.process)$(git config --get filter.lfs.clean)" ]; then
  echo "Cannot checkout LFS objects, Git LFS is not installed."; exit 0
fi
git ls-files | while IFS= read -r f; do
  oid=$(sed -n 's/^oid sha256://p' "$f" 2>/dev/null)
  [ -n "$oid" ] || continue
  a=$(printf %s "$oid" | cut -c1-2)
  b=$(printf %s "$oid" | cut -c3-4)
  obj=".git/lfs/objects/$a/$b/$oid"
  [ -f "$obj" ] && cp "$obj" "$f"
done
exit 0
"""


def _pointer(data: bytes) -> tuple[str, bytes]:
    oid = hashlib.sha256(data).hexdigest()
    return oid, f"version https://git-lfs.github.com/spec/v1\noid sha256:{oid}\nsize {len(data)}\n".encode()


def _lfs_source(root: Path) -> tuple[RepoBuilder, bytes]:
    src = RepoBuilder.init(root)
    oid, pointer = _pointer(DATA)
    src.write(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
    src.write("asset.bin", pointer)
    src.write("plain.txt", "plain\n")
    src.commit_all("lfs")
    obj = src.root / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
    obj.parent.mkdir(parents=True)
    obj.write_bytes(DATA)
    return src, pointer


@pytest.fixture
def lfs_stand_in(tmp_path, monkeypatch) -> Path:
    bindir = tmp_path / "lfs-bin"
    bindir.mkdir()
    log = tmp_path / "lfs.log"
    script = bindir / "git-lfs"
    script.write_text(STAND_IN.format(log=log), encoding="utf-8")
    os.chmod(script, 0o755)  # noqa: S103 - executable test stand-in
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ.get('PATH', '')}")
    return log


def test_rv_e_h8_fresh_checks_out_lfs_content(git, user_home, staging, cfg, store, handover_root, lfs_stand_in):
    """★ H8: fresh mode runs ``git lfs checkout`` after its own checkout, so the shipped content stays in the work
    tree (mutant: LFS checkout before ``checkout-index -a -f``, which rewrites the pointer)."""
    src, _pointer = _lfs_source(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    assert item["lfs_objects"]
    dest = apply_fresh(git, store, item, handover_root / "app", cfg)
    assert (dest / "asset.bin").read_bytes() == DATA
    assert (dest / "plain.txt").read_bytes() == b"plain\n"
    assert "checkout" in lfs_stand_in.read_text()
    index = RepoBuilder(dest).run("ls-files", "-s", "asset.bin").stdout
    assert index.split()[1].decode() == RepoBuilder(src.root).rev("HEAD:asset.bin")


def test_rv_e_h8_fresh_partial_still_0700_during_lfs_checkout(
    git, user_home, staging, cfg, store, handover_root, lfs_stand_in
):
    """The LFS checkout runs inside the 0700 partial (before commit renames it)."""
    src, _ = _lfs_source(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    dest = handover_root / "app"
    partial = handover_root / ".ctxh-app"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=dest)
    assert (partial / "asset.bin").read_bytes() == DATA
    assert partial.stat().st_mode & 0o777 == 0o700
    assert not dest.exists()
    gitapply.discard(git, state)


def test_rv_e_h8_fresh_without_git_lfs_warns(git, user_home, staging, cfg, store, handover_root, no_lfs):
    """Without git-lfs the pointers stay and PREPARE says so."""
    src, pointer = _lfs_source(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    partial = handover_root / ".ctxh-app"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert (partial / "asset.bin").read_bytes() == pointer
    assert any("git-lfs is not installed" in w for w in state.warnings)
    gitapply.discard(git, state)


def test_rv_e_h8_update_without_git_lfs_warns(git, user_home, staging, cfg, store, handover_root, no_lfs):
    """★ update mode without git-lfs: PREPARE names the pointers that stay (mutant: the warning of the c1 tail
    is dropped and PREPARE says nothing)."""
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    _, seed = pack(git, src.root, staging, cfg, store, item_id="seed")
    tgt = RepoBuilder(apply_fresh(git, store, seed, handover_root / "app", cfg))
    oid, pointer = _pointer(DATA)
    src.write(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
    src.write("asset.bin", pointer)
    src.commit_all("lfs")
    obj = src.root / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
    obj.parent.mkdir(parents=True)
    obj.write_bytes(DATA)
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert any("git-lfs is not installed" in w for w in state.warnings)
    gitapply.commit(git, state, item)
    assert (tgt.root / "asset.bin").read_bytes() == pointer
