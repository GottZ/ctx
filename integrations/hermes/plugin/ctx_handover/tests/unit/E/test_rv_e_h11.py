"""Review round rv-e, finding H11: a file tree (and a single extra file) at the target is checked against the byte
cap and the free space before the first byte is written — block de-duplication must not let a peer write more than
the target allows (R28, BUILD-ORDER §6 Nr. 8)."""

from __future__ import annotations

import io
import shutil
from collections import namedtuple
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitops, materialize
from ctx_handover.engine.errors import InsufficientStorage, PayloadTooLarge
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.jsonutil import canonical_bytes
from ctx_handover.engine.model import (
    DestMode,
    Include,
    ItemKind,
    ItemRole,
    MapRow,
    Origin,
    PlanItem,
    TargetDecision,
)
from ctx_handover.tests.fakes.git_fake import RecordingJournal
from ctx_handover.tests.unit.E._support import LINUX, make_cfg

SIZE = 64 * 1024
COPIES = 40


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


def _amplifying_manifest(store) -> str:
    """``COPIES`` entries that all name one stored object (one block) of ``SIZE`` bytes."""
    m = gitops.put_stream(store, io.BytesIO(b"\x00" * SIZE), media="application/octet-stream")
    entries = [
        {"path": f"f{i:03d}", "type": "file", "mode": "100644", "size": SIZE, "mtime_ns": 0, "object": m.object_id}
        for i in range(COPIES)
    ]
    doc = {"kind": "ctxh.tree", "v": 1, "entries": entries}
    return gitops.put_bytes(store, canonical_bytes(doc), "application/json").object_id


def _txn(git, store, tmp_path, cfg, job="01JOB0000000000000000RVEH11"):
    return materialize.WorkspaceTxn(job, tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg)


def test_rv_e_h11_tree_fresh_byte_cap(git, user_home, hermes_home, tmp_path, store, handover_root):
    """★ H11: 40 × 64 KiB from one block against a 1 MiB cap is refused before anything is written (mutant: only
    ``max_files`` checked)."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=1024 * 1024)
    pitem = {"item_id": "t1", "kind": "file_tree", "manifest": _amplifying_manifest(store)}
    txn = _txn(git, store, tmp_path, cfg)
    with pytest.raises(PayloadTooLarge) as exc:
        txn.prepare_item(
            _plan_item(ItemKind.FILE_TREE, "t1", user_home / "t"),
            TargetDecision("t1", str(handover_root / "t"), DestMode.FRESH, "workspace-root", None),
            pitem,
        )
    assert exc.value.details["bytes"] == SIZE * COPIES
    assert list(handover_root.iterdir()) == []


def test_rv_e_h11_tree_fresh_free_space(git, user_home, hermes_home, tmp_path, store, handover_root, monkeypatch):
    """★ H11: the tree must fit into the free space (×1.1) of the destination's file system (mutant: no
    free-space check)."""
    cfg = make_cfg(user_home, hermes_home)
    pitem = {"item_id": "t1", "kind": "file_tree", "manifest": _amplifying_manifest(store)}
    usage = namedtuple("usage", "total used free")
    real = shutil.disk_usage
    monkeypatch.setattr(
        shutil,
        "disk_usage",
        lambda p: usage(10**9, 0, SIZE * COPIES) if str(p).startswith(str(handover_root)) else real(p),
    )
    txn = _txn(git, store, tmp_path, cfg)
    with pytest.raises(InsufficientStorage):
        txn.prepare_item(
            _plan_item(ItemKind.FILE_TREE, "t1", user_home / "t"),
            TargetDecision("t1", str(handover_root / "t"), DestMode.FRESH, "workspace-root", None),
            pitem,
        )
    assert list(handover_root.iterdir()) == []


def test_rv_e_h11_tree_update_byte_cap(user_home, hermes_home, cfg, store, handover_root, tmp_path):
    """★ H11 in update mode: the staged files count against the cap before the first is written (mutant: no cap in
    ``prepare_update``)."""
    root = user_home / "tree"
    root.mkdir()
    (root / "a.txt").write_text("a\n")
    entries, _ = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    dest = handover_root / "tree"
    st = filetree.prepare_fresh(
        store, filetree.pack_tree(entries, root, store), handover_root / ".ctxh-f", LINUX, dest=dest
    )
    fp = filetree.commit(st)["fingerprint"]
    with pytest.raises(PayloadTooLarge):
        filetree.prepare_update(
            store, _amplifying_manifest(store), dest, fp, LINUX, tmp_path / "bk", max_bytes=1024 * 1024
        )
    assert sorted(p.name for p in dest.iterdir()) == ["a.txt"]


def test_rv_e_h11_tree_update_cap_via_txn(git, user_home, hermes_home, tmp_path, store, handover_root):
    """★ H11 in update mode through the workspace transaction (production path; mutant: the transaction does
    not pass the target's cap)."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=1024 * 1024)
    root = user_home / "tree"
    root.mkdir()
    (root / "a.txt").write_text("a\n")
    entries, _ = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    dest = handover_root / "tree"
    st = filetree.prepare_fresh(
        store, filetree.pack_tree(entries, root, store), handover_root / ".ctxh-f", LINUX, dest=dest
    )
    fp = filetree.commit(st)["fingerprint"]
    row = MapRow(local_path=str(dest), peer_id="p", remote_path=str(root), direction="in", state={"tree": fp})
    pitem = {"item_id": "t1", "kind": "file_tree", "manifest": _amplifying_manifest(store)}
    txn = _txn(git, store, tmp_path, cfg)
    with pytest.raises(PayloadTooLarge):
        txn.prepare_item(
            _plan_item(ItemKind.FILE_TREE, "t1", root),
            TargetDecision("t1", str(dest), DestMode.UPDATE, "workspace-map", None),
            pitem,
            out_row=row,
        )
    assert sorted(p.name for p in dest.iterdir()) == ["a.txt"]


def test_rv_e_h11_extra_file_byte_cap(git, user_home, hermes_home, tmp_path, store, handover_root):
    """★ H11 for a single extra file: larger than the target's cap ⇒ refused before writing (mutant: no cap for
    ``extra_file``)."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=1024)
    src = user_home / "big.bin"
    src.write_bytes(b"\x01" * 4096)
    pitem = {"item_id": "e1", **filetree.pack_file(src, store)}
    txn = _txn(git, store, tmp_path, cfg)
    with pytest.raises(PayloadTooLarge):
        txn.prepare_item(
            _plan_item(ItemKind.EXTRA_FILE, "e1", src),
            TargetDecision("e1", str(handover_root / "big.bin"), DestMode.FRESH, "user", None),
            pitem,
        )
    assert list(handover_root.iterdir()) == []


def test_rv_e_h11_within_cap_still_lands(git, user_home, hermes_home, tmp_path, store, handover_root):
    """The cap counts declared sizes: a tree within it lands unchanged."""
    cfg = make_cfg(user_home, hermes_home, max_total_bytes=SIZE * COPIES)
    pitem = {"item_id": "t1", "kind": "file_tree", "manifest": _amplifying_manifest(store)}
    txn = _txn(git, store, tmp_path, cfg)
    txn.prepare_item(
        _plan_item(ItemKind.FILE_TREE, "t1", user_home / "t"),
        TargetDecision("t1", str(handover_root / "t"), DestMode.FRESH, "workspace-root", None),
        pitem,
    )
    for _, forward, _ in txn.commit_steps():
        forward()
    assert len(list((handover_root / "t").iterdir())) == COPIES
