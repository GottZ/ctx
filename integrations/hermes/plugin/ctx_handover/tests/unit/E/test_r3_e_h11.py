"""Review round 3, H11 rest (R2-M7): the target's byte cap ``max_total_bytes`` holds per job, not per item — the
declared bytes of all workspace items of one job together, checked before an item writes its first byte — and what
the job's items write × 1.1 must fit the free space each file system had when the job first measured it
(DESIGN §10.2 ``max_total_gb`` per job, R28; REVIEW-2 §2 H11, §3)."""

from __future__ import annotations

import io
import os
import shutil
from collections import namedtuple
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitops, materialize
from ctx_handover.engine.errors import InsufficientStorage, PayloadTooLarge
from ctx_handover.engine.jsonutil import canonical_bytes
from ctx_handover.engine.model import DestMode, Include, ItemKind, ItemRole, Origin, PlanItem, TargetDecision
from ctx_handover.tests.fakes.git_fake import RecordingJournal
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.unit.E._support import AT, apply_fresh, make_cfg, pack

SIZE = 64 * 1024


def _plan_item(kind: ItemKind, item_id: str, source: Path) -> PlanItem:
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=str(source),
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=1000,
    )


def _tree(store, copies: int, fill: bytes = b"\x00") -> dict:
    """A tree pack item of ``copies`` files of ``SIZE`` bytes that all name one stored block."""
    m = gitops.put_stream(store, io.BytesIO(fill * SIZE), media="application/octet-stream")
    entries = [
        {"path": f"f{i:03d}", "type": "file", "mode": "100644", "size": SIZE, "mtime_ns": 0, "object": m.object_id}
        for i in range(copies)
    ]
    doc = {"kind": "ctxh.tree", "v": 1, "entries": entries}
    return {
        "kind": "file_tree",
        "manifest": gitops.put_bytes(store, canonical_bytes(doc), "application/json").object_id,
    }


def _txn(git, store, tmp_path, cfg, job="01JOB0000000000000000R3EH11"):
    return materialize.WorkspaceTxn(job, tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg, now=lambda: AT)


def _prepare_tree(txn, store, handover_root, user_home, item_id: str, copies: int) -> None:
    txn.prepare_item(
        _plan_item(ItemKind.FILE_TREE, item_id, user_home / item_id),
        TargetDecision(item_id, str(handover_root / item_id), DestMode.FRESH, "workspace-root", None),
        {"item_id": item_id, **_tree(store, copies)},
    )


def _written(root: Path) -> int:
    return sum(os.lstat(os.path.join(d, f)).st_size for d, _, fs in os.walk(root) for f in fs)


def test_r3_e_h11_job_cap_over_tree_items(git, user_home, hermes_home, tmp_path, store, handover_root):
    """★ H11 rest (probe 9 "job" of the reviewer): cap 1 MiB, three tree items of 384 KiB each — every one under the
    cap. The third is refused before its first byte (``PayloadTooLarge`` names the job's bytes), the two before it
    stay as prepared; the job never writes more than its cap (mutant: the cap per item only)."""
    cap = 1024 * 1024
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=cap)
    txn = _txn(git, store, tmp_path, cfg)
    _prepare_tree(txn, store, handover_root, user_home, "t0", 6)
    _prepare_tree(txn, store, handover_root, user_home, "t1", 6)
    written = _written(handover_root)
    with pytest.raises(PayloadTooLarge) as exc:
        _prepare_tree(txn, store, handover_root, user_home, "t2", 6)
    assert exc.value.details["max_bytes"] == cap
    assert exc.value.details["bytes"] == 3 * 6 * SIZE
    assert _written(handover_root) == written <= cap
    assert [p.mode for p in txn.items] == [DestMode.FRESH, DestMode.FRESH]
    txn.discard()
    assert list(handover_root.iterdir()) == []


@pytest.mark.parametrize("first", ["git", "extra"])
def test_r3_e_h11_job_cap_counts_git_and_extra_items(
    git, user_home, hermes_home, tmp_path, staging, store, handover_root, first
):
    """★ H11 rest across item kinds: a repository whose work tree holds 600 KiB and a 600 KiB extra file, cap
    1 MiB — whichever comes second is refused before it writes (mutants: the git precheck or the extra file does
    not count towards the job)."""
    cap = 1024 * 1024
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=cap)
    src = RepoBuilder.init(user_home / "src")
    src.write("big.bin", os.urandom(600 * 1024))
    src.commit_all("c1")
    _, git_item = pack(git, src.root, staging, cfg, store, item_id="g")
    extra_src = user_home / "extra.bin"
    extra_src.write_bytes(os.urandom(600 * 1024))
    extra_item = {"item_id": "e", **filetree.pack_file(extra_src, store)}
    steps = {
        "git": (
            _plan_item(ItemKind.GIT_REPO, "g", src.root),
            TargetDecision("g", str(handover_root / "src"), DestMode.FRESH, "workspace-root", None),
            git_item,
        ),
        "extra": (
            _plan_item(ItemKind.EXTRA_FILE, "e", extra_src),
            TargetDecision("e", str(handover_root / "extra.bin"), DestMode.FRESH, "user", None),
            extra_item,
        ),
    }
    second = "extra" if first == "git" else "git"
    txn = _txn(git, store, tmp_path, cfg)
    txn.prepare_item(*steps[first])
    before = dir_fingerprint(handover_root)
    with pytest.raises(PayloadTooLarge) as exc:
        txn.prepare_item(*steps[second])
    assert exc.value.details["max_bytes"] == cap
    assert dir_fingerprint(handover_root) == before
    txn.discard()


def test_r3_e_h11_job_free_space_from_the_jobs_start(
    git, user_home, hermes_home, tmp_path, store, handover_root, monkeypatch
):
    """★ H11 rest, free space: the file system holds 36 blocks free. The first item (30 blocks × 1.1 = 33) fits;
    after it, 6 are free and a second item of 5 blocks (5.5) fits that rest too — together they would fill the disk
    to 35 of 36. The job's items × 1.1 are measured against the free space at the job's start: the second is
    refused before it writes (mutant: the free-space check per item only)."""
    cfg = make_cfg(user_home, hermes_home)
    usage = namedtuple("usage", "total used free")
    real = shutil.disk_usage
    free = 36 * SIZE

    def disk_usage(p):
        if str(p).startswith(str(handover_root)):
            return usage(10**12, 0, free - _written(handover_root))
        return real(p)

    monkeypatch.setattr(shutil, "disk_usage", disk_usage)
    txn = _txn(git, store, tmp_path, cfg)
    _prepare_tree(txn, store, handover_root, user_home, "t0", 30)
    with pytest.raises(InsufficientStorage) as exc:
        _prepare_tree(txn, store, handover_root, user_home, "t1", 5)
    assert exc.value.details["need_bytes"] == int(35 * SIZE * 1.1)
    assert exc.value.details["free_bytes"] == free
    assert _written(handover_root) == 30 * SIZE
    txn.discard()


def test_r3_e_h11_job_within_the_cap_lands(git, user_home, hermes_home, tmp_path, store, handover_root):
    """Not stricter than the cap: items that together reach it exactly all land."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=12 * SIZE)
    txn = _txn(git, store, tmp_path, cfg)
    for i in range(3):
        _prepare_tree(txn, store, handover_root, user_home, f"t{i}", 4)
    for _, forward, _ in txn.commit_steps():
        forward()
    assert sorted(p.name for p in handover_root.iterdir()) == ["t0", "t1", "t2"]


def _exclude(repo: RepoBuilder, *patterns: str) -> None:
    info = repo.root / ".git" / "info"
    info.mkdir(exist_ok=True)
    (info / "exclude").write_text("".join(p + "\n" for p in patterns), encoding="utf-8")


def test_r3_e_h11_fallback_counts_the_item_once(git, user_home, hermes_home, tmp_path, staging, store, handover_root):
    """An update that falls back to a fresh directory checks the item twice (update attempt, then fresh): it is
    counted once. Work tree ≈ 300 KiB, cap 400 KiB — the fallback lands (mutant: every check is counted)."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=400 * 1024)
    src = RepoBuilder.init(user_home / "src")
    src.write("big.bin", os.urandom(300 * 1024))
    src.commit_all("c1")
    _, seed = pack(git, src.root, staging, cfg, store, item_id="seed")
    tgt = RepoBuilder(apply_fresh(git, store, seed, handover_root / "app", cfg))
    tgt.run("stash", "clear")
    tgt.run("reset", "-q", "--hard")
    tgt.run("clean", "-qfdx")
    _exclude(tgt, "local.cfg")
    tgt.write("local.cfg", "the target's own\n")
    src.write("local.cfg", "from the source\n")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    txn = _txn(git, store, tmp_path, cfg, job="01JOB0000000000000000R3EH1F")
    txn.prepare_item(
        _plan_item(ItemKind.GIT_REPO, "u", src.root),
        TargetDecision("u", str(tgt.root), DestMode.UPDATE, "return-mapping", None),
        item,
    )
    ((_, final, mode),) = txn.dests()
    assert mode is DestMode.FRESH and final != str(tgt.root)
    txn.discard()
