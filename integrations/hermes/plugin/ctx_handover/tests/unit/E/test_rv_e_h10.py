"""Review round rv-e, finding H10: a file-tree update never overwrites a path that appeared between PREPARE and
c1, and its inverse removes only the files the forward step put in (BRIEF D10, I5, BUILD-ORDER §6 Nr. 9)."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import filetree
from ctx_handover.engine.errors import ConflictError
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.tests.unit.E._support import LINUX


def _scan_pack(root: Path, cfg, store) -> str:
    entries, _ = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    return filetree.pack_tree(entries, root, store)


def _landed(user_home, cfg, store, handover_root) -> tuple[Path, Path, dict]:
    root = user_home / "tree"
    root.mkdir()
    (root / "a.txt").write_text("a\n")
    dest = handover_root / "tree"
    st = filetree.prepare_fresh(store, _scan_pack(root, cfg, store), handover_root / ".ctxh-f", LINUX, dest=dest)
    fp = filetree.commit(st)["fingerprint"]
    return root, dest, fp


def test_rv_e_h10_new_path_appeared_after_prepare(user_home, cfg, store, handover_root, tmp_path):
    """★ H10: the update brings ``notes.md`` (new); the user creates ``notes.md`` after PREPARE. c1 stops with
    ``foreign``, the user's file keeps its bytes, also after the rollback (mutant: ``os.replace`` onto a new
    path without an existence check)."""
    root, dest, fp = _landed(user_home, cfg, store, handover_root)
    (root / "notes.md").write_text("from source\n")
    (root / "a.txt").write_text("a2\n")
    state = filetree.prepare_update(store, _scan_pack(root, cfg, store), dest, fp, LINUX, tmp_path / "bk")
    (dest / "notes.md").write_text("TYPED BY THE TARGET USER\n")
    with pytest.raises(ConflictError) as exc:
        filetree.commit(state)
    assert exc.value.details["kind"] == "foreign"
    assert (dest / "notes.md").read_text() == "TYPED BY THE TARGET USER\n"
    assert (dest / "a.txt").read_text() == "a\n"
    filetree.rollback(state)
    assert (dest / "notes.md").read_text() == "TYPED BY THE TARGET USER\n"
    assert (dest / "a.txt").read_text() == "a\n"
    assert sorted(p.name for p in dest.iterdir()) == ["a.txt", "notes.md"]


def test_rv_e_h10_rollback_keeps_new_file_replaced_after_commit(user_home, cfg, store, handover_root, tmp_path):
    """★ H10 inverse: a new file the update put in and someone replaced afterwards (another file at the path) is
    not deleted by the rollback (mutant: the inverse unlinks every swapped-in new path)."""
    root, dest, fp = _landed(user_home, cfg, store, handover_root)
    (root / "notes.md").write_text("from source\n")
    state = filetree.prepare_update(store, _scan_pack(root, cfg, store), dest, fp, LINUX, tmp_path / "bk")
    filetree.commit(state)
    assert (dest / "notes.md").read_text() == "from source\n"
    tmp = dest / "notes.md.new"
    tmp.write_text("SAVED BY AN EDITOR AFTER COMMIT\n")
    os.replace(tmp, dest / "notes.md")
    filetree.rollback(state)
    assert (dest / "notes.md").read_text() == "SAVED BY AN EDITOR AFTER COMMIT\n"


def test_rv_e_h10_rollback_keeps_new_file_edited_in_place(user_home, cfg, store, handover_root, tmp_path):
    """★ H10 inverse: an in-place edit (same inode, new content) of a swapped-in new file survives the rollback
    (mutant: the inverse compares only the inode)."""
    root, dest, fp = _landed(user_home, cfg, store, handover_root)
    (root / "notes.md").write_text("from source\n")
    state = filetree.prepare_update(store, _scan_pack(root, cfg, store), dest, fp, LINUX, tmp_path / "bk")
    filetree.commit(state)
    with open(dest / "notes.md", "a", encoding="utf-8") as fh:
        fh.write("appended after commit\n")
    os.utime(dest / "notes.md", ns=(1, 1))
    filetree.rollback(state)
    assert (dest / "notes.md").read_text() == "from source\nappended after commit\n"


def test_rv_e_h10_rollback_removes_untouched_new_file(user_home, cfg, store, handover_root, tmp_path):
    """The inverse still removes what the forward step put in and nobody touched; replaced files come back."""
    root, dest, fp = _landed(user_home, cfg, store, handover_root)
    (root / "notes.md").write_text("from source\n")
    os.symlink("a.txt", root / "link")
    (root / "a.txt").write_text("a2\n")
    state = filetree.prepare_update(store, _scan_pack(root, cfg, store), dest, fp, LINUX, tmp_path / "bk")
    filetree.commit(state)
    assert os.readlink(dest / "link") == "a.txt" and (dest / "a.txt").read_text() == "a2\n"
    filetree.rollback(state)
    assert sorted(p.name for p in dest.iterdir()) == ["a.txt"]
    assert (dest / "a.txt").read_text() == "a\n"


def test_rv_e_h10_resume_after_link_before_unlink(user_home, cfg, store, handover_root, tmp_path):
    """Contract X11: a forward step that linked a new file in and died before removing the staged name resumes
    without a conflict; the inverse of that half-done state removes the linked file."""
    root, dest, fp = _landed(user_home, cfg, store, handover_root)
    (root / "notes.md").write_text("from source\n")
    state = filetree.prepare_update(store, _scan_pack(root, cfg, store), dest, fp, LINUX, tmp_path / "bk")
    ((temp, final),) = state.staged
    os.link(temp, final)
    filetree.rollback(filetree.state_from_dict(filetree.state_to_dict(state)))
    assert not final.exists() and not temp.exists()
    state = filetree.prepare_update(store, _scan_pack(root, cfg, store), dest, fp, LINUX, tmp_path / "bk2")
    ((temp, final),) = state.staged
    os.link(temp, final)
    filetree.commit(filetree.state_from_dict(filetree.state_to_dict(state)))
    assert final.read_text() == "from source\n" and not temp.exists()
