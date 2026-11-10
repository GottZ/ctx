"""Review finding R3-M3 (the third review §3; its round-3 report „Nicht geprüft“): the byte cap
(``max_total_bytes``, per job) and the free-space check at the target counted a git item's work tree by its blobs —
for Git LFS files that is the pointer text. PREPARE then places the shipped LFS objects and ``git lfs checkout`` writes
every pointer file's object into the work tree: many pointers to one object (as in H11) write a multiple of what was
checked.

Now the item's LFS bytes count too, before anything of the item is written: the shipped objects (the sizes in the pack
item's LFS manifest) and — when git-lfs exists here, i.e. when the plugin's checkout writes them — the object size once
per pointer file of the received work tree whose object is shipped. ``git-lfs`` is the stand-in of the H8 tests: it
replaces each pointer file whose object lies in ``.git/lfs/objects`` by that object (no network)."""

from __future__ import annotations

import hashlib
import os
import shutil
from collections import namedtuple
from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops, materialize
from ctx_handover.engine.errors import InsufficientStorage, PayloadTooLarge
from ctx_handover.engine.model import DestMode, ItemKind, TargetDecision
from ctx_handover.tests.fakes.git_fake import RecordingJournal
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import AT, LINUX, make_cfg, pack
from ctx_handover.tests.unit.E.test_r3_e_h11 import _plan_item, _prepare_tree, _written
from ctx_handover.tests.unit.E.test_r3_e_ignore import _clean_copy
from ctx_handover.tests.unit.E.test_rv_e_h8 import lfs_stand_in  # noqa: F401 - fixture

KiB = 1024
DATA = bytes(range(256)) * 128  # 32 KiB, one LFS object
COPIES = 8  # pointer files naming that one object


def _pointer(data: bytes) -> tuple[str, bytes]:
    oid = hashlib.sha256(data).hexdigest()
    return oid, f"version https://git-lfs.github.com/spec/v1\noid sha256:{oid}\nsize {len(data)}\n".encode()


def _add_lfs(src: RepoBuilder, copies: int, data: bytes = DATA, *, ship: bool = True) -> None:
    """``copies`` pointer files to one object; the object lies in the source's LFS store (``ship``)."""
    oid, pointer = _pointer(data)
    src.write(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
    for i in range(copies):
        src.write(f"assets/a{i:02d}.bin", pointer)
    src.commit_all(f"lfs {copies}")
    if ship:
        obj = src.root / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
        obj.parent.mkdir(parents=True, exist_ok=True)
        obj.write_bytes(data)
    else:
        (src.root / ".git" / "lfs" / "objects").mkdir(parents=True, exist_ok=True)


def _lfs_repo(root: Path, copies: int = COPIES, **kw) -> RepoBuilder:
    src = RepoBuilder.init(root)
    src.write("plain.txt", "plain\n")
    _add_lfs(src, copies, **kw)
    return src


def _usage_with_free(monkeypatch, under: Path, free: int) -> None:
    usage = namedtuple("usage", "total used free")
    real = shutil.disk_usage
    monkeypatch.setattr(
        shutil, "disk_usage", lambda p: usage(10**12, 0, free) if str(p).startswith(str(under)) else real(p)
    )


def test_r4_ke_m3_fresh_cap_counts_the_lfs_checkout(
    git,
    user_home,
    hermes_home,
    staging,
    store,
    handover_root,
    lfs_stand_in,  # noqa: F811
):
    """★ The reviewer's case: cap 128 KiB, eight pointer files (≈ 1 KiB of blobs) naming one 32 KiB object. The
    checkout would write 8 × 32 KiB plus the placed object — PREPARE refuses before the partial exists. Before the
    fix: PREPARE succeeded and the partial held 256 KiB of LFS content, twice the cap."""
    cap = 128 * KiB
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=cap)
    src = _lfs_repo(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    assert item["lfs_objects"]
    partial = handover_root / ".ctxh-app"
    with pytest.raises(PayloadTooLarge) as exc:
        gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert exc.value.details["max_bytes"] == cap
    assert exc.value.details["bytes"] >= (COPIES + 1) * len(DATA)
    assert not partial.exists()
    assert "checkout" not in (lfs_stand_in.read_text() if lfs_stand_in.exists() else "")


def test_r4_ke_m3_fresh_free_space_counts_the_lfs_checkout(
    git,
    user_home,
    hermes_home,
    staging,
    store,
    handover_root,
    monkeypatch,
    lfs_stand_in,  # noqa: F811
):
    """★ Free space: 128 KiB free, the same repository — the LFS bytes × 1.1 do not fit, PREPARE refuses
    (``InsufficientStorage``) before it writes. Before the fix: the pointer bytes fit and the checkout filled
    256 KiB."""
    cfg = make_cfg(user_home, hermes_home)
    src = _lfs_repo(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    _usage_with_free(monkeypatch, handover_root, 128 * KiB)
    partial = handover_root / ".ctxh-app"
    with pytest.raises(InsufficientStorage) as exc:
        gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert exc.value.details["need_bytes"] >= int((COPIES + 1) * len(DATA) * 1.1)
    assert not partial.exists()


def test_r4_ke_m3_update_cap_counts_the_lfs_checkout(
    git,
    user_home,
    hermes_home,
    staging,
    store,
    handover_root,
    lfs_stand_in,  # noqa: F811
):
    """★ Update mode: the source added the eight pointer files since the target's copy. ``prepare_update`` refuses
    before any object is placed, and the target keeps its state. Before the fix: PREPARE passed and c1's checkout
    wrote 256 KiB under a 128 KiB cap."""
    cap = 128 * KiB
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=cap)
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    tgt = _clean_copy(git, store, cfg, src, staging, handover_root / "app")
    _add_lfs(src, COPIES)
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    h0 = tgt.head()
    with pytest.raises(PayloadTooLarge):
        gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert tgt.head() == h0
    assert not (tgt.root / ".git" / "lfs").exists()


def test_r4_ke_m3_job_cap_counts_the_lfs_bytes_of_a_later_item(
    git,
    user_home,
    hermes_home,
    tmp_path,
    staging,
    store,
    handover_root,
    lfs_stand_in,  # noqa: F811
):
    """★ Per job: cap 150 KiB; a tree item of 64 KiB lands first, then a repository whose two pointer files name one
    32 KiB object (≈ 96 KiB with its checkout — under the cap on its own). Together they pass the cap: the git item
    is refused before it writes. Before the fix: its ≈ 300 bytes of pointers were counted and it landed."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=150 * KiB)
    src = _lfs_repo(user_home / "src", copies=2)
    _, git_item = pack(git, src.root, staging, cfg, store, item_id="g")
    txn = materialize.WorkspaceTxn(
        "01JOB0000000000000000R4KEM3", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg, now=lambda: AT
    )
    _prepare_tree(txn, store, handover_root, user_home, "t0", 1)
    written = _written(handover_root)
    with pytest.raises(PayloadTooLarge) as exc:
        txn.prepare_item(
            _plan_item(ItemKind.GIT_REPO, "g", src.root),
            TargetDecision("g", str(handover_root / "src"), DestMode.FRESH, "workspace-root", None),
            git_item,
        )
    assert exc.value.details["item_bytes"] >= 3 * len(DATA)
    assert _written(handover_root) == written
    txn.discard()


def test_r4_ke_m3_without_git_lfs_only_the_placed_objects_count(
    git, user_home, hermes_home, staging, store, handover_root, no_lfs
):
    """Without git-lfs nothing checks the pointers out (they stay, PREPARE says so): only the placed object counts.
    Cap 64 KiB against 32 KiB object + pointers — PREPARE passes, the pointers stay."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=64 * KiB)
    src = _lfs_repo(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    partial = handover_root / ".ctxh-app"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert (partial / "assets" / "a00.bin").read_bytes() == _pointer(DATA)[1]
    assert any("git-lfs is not installed" in w for w in state.warnings)
    gitapply.discard(git, state)


def test_r4_ke_m3_unshipped_objects_do_not_count(
    git,
    user_home,
    hermes_home,
    staging,
    store,
    handover_root,
    lfs_stand_in,  # noqa: F811
):
    """Pointers whose object the source does not hold are not shipped and stay pointers at the target: they do not
    count. Cap 16 KiB, eight pointers to an object nobody has — PREPARE passes."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=16 * KiB)
    src = _lfs_repo(user_home / "src", ship=False)
    _, item = pack(git, src.root, staging, cfg, store)
    partial = handover_root / ".ctxh-app"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert (partial / "assets" / "a00.bin").read_bytes() == _pointer(DATA)[1]
    gitapply.discard(git, state)


def test_r4_ke_m3_within_the_cap_the_content_lands(
    git,
    user_home,
    hermes_home,
    staging,
    store,
    handover_root,
    lfs_stand_in,  # noqa: F811
):
    """Not stricter than needed: cap 512 KiB holds the 8 × 32 KiB checkout plus the object — the content lands."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=512 * KiB)
    src = _lfs_repo(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    partial = handover_root / ".ctxh-app"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert all((partial / "assets" / f"a{i:02d}.bin").read_bytes() == DATA for i in range(COPIES))
    gitapply.discard(git, state)
    assert not os.path.exists(partial)
