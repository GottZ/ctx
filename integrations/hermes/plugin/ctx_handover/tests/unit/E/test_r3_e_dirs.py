"""Review round 3, R2-L3: the inverse of an in-place update or return trip removes only the directories its
forward step created — an empty directory the user had at the destination before stays (BRIEF D10, DESIGN §9.5,
REVIEW-2 §3 R2-L3)."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import apply_fresh, pack


def _clean_copy(git, store, cfg, src: RepoBuilder, staging: Path, dest: Path) -> RepoBuilder:
    _, item = pack(git, src.root, staging, cfg, store, item_id="seed")
    t = RepoBuilder(apply_fresh(git, store, item, dest, cfg))
    t.run("stash", "clear")
    t.run("reset", "-q", "--hard")
    t.run("clean", "-qfdx")
    return t


def _seeded(git, user_home, staging, cfg, store, handover_root) -> tuple[RepoBuilder, RepoBuilder]:
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    return src, _clean_copy(git, store, cfg, src, staging, handover_root / "app")


def _user_dirs(root: Path) -> None:
    (root / "logs").mkdir()  # empty, the user's
    os.chmod(root / "logs", 0o700)
    (root / "var").mkdir()  # empty, the user's; the update creates var/cache/ below it


@pytest.mark.parametrize("partial", [False, True])
def test_r3_e_l3_update_rollback_keeps_users_empty_dirs(git, user_home, staging, cfg, store, handover_root, partial):
    """★ R2-L3 (probe 14 of the reviewer): the target has empty ``logs/`` and ``var/``; the update adds
    ``logs/x.log`` and ``var/cache/c.bin``. After c1 and the rollback (merging and resetting inverse) both of the
    user's directories exist again — same mode, empty — and only ``var/cache/`` (created by the forward step) is
    gone (mutant: the inverse removes every empty parent of an added path)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _user_dirs(tgt.root)
    ino = os.lstat(tgt.root / "logs").st_ino
    src.write("logs/x.log", "log\n")
    src.write("var/cache/c.bin", "c\n")
    src.commit_all("adds below the user's dirs")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    assert (tgt.root / "logs" / "x.log").read_text() == "log\n"
    gitapply.rollback(git, state, item, partial=partial)
    assert (tgt.root / "logs").is_dir() and os.listdir(tgt.root / "logs") == []
    assert (os.lstat(tgt.root / "logs").st_mode & 0o777) == 0o700
    assert os.lstat(tgt.root / "logs").st_ino == ino  # never removed and re-created
    assert (tgt.root / "var").is_dir() and os.listdir(tgt.root / "var") == []
    assert not (tgt.root / "var" / "cache").exists()


def test_r3_e_l3_update_rollback_without_pack_item(git, user_home, staging, cfg, store, handover_root):
    """The inverse without the pack item (it cannot tell edited files apart) keeps the user's directories too."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _user_dirs(tgt.root)
    src.write("logs/x.log", "log\n")
    src.commit_all("adds below logs")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    gitapply.rollback(git, state)
    assert (tgt.root / "logs").is_dir() and not (tgt.root / "logs" / "x.log").exists()


def test_r3_e_l3_update_rollback_removes_dirs_it_created(git, user_home, staging, cfg, store, handover_root):
    """Not too lenient: directories the forward step created (``new/deep/``) are gone after the rollback."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    src.write("new/deep/f.txt", "f\n")
    src.commit_all("adds new dirs")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    gitapply.rollback(git, state, item)
    assert not (tgt.root / "new").exists()


@pytest.mark.parametrize("partial", [False, True])
def test_r3_e_l3_return_rollback_keeps_users_empty_dir(
    git, user_home, tmp_path, staging, cfg, store, handover_root, partial
):
    """★ R2-L3 on the return trip: the server keeps an empty ``logs/`` (git does not carry empty directories);
    the laptop sends ``logs/x.log`` back; the rollback after c1 leaves the server's ``logs/`` in place (mutant: as
    above)."""
    server = RepoBuilder.init(user_home / "server")
    server.write("a.txt", "a\n")
    server.commit_all("c1")
    (server.root / "logs").mkdir()
    ino = os.lstat(server.root / "logs").st_ino
    out, item = pack(git, server.root, staging, cfg, store, item_id="out")
    laptop = RepoBuilder(apply_fresh(git, store, item, handover_root / "laptop", cfg))
    laptop.write("logs/x.log", "from the laptop\n")
    basis = gitops.GitProbe(git).ref_tips(str(server.root))
    _, back = pack(git, laptop.root, staging, cfg, store, basis=basis, item_id="back")
    state = gitapply.prepare_return(
        git, store, back, server.root, out.state, tmp=tmp_path / "rt", cfg=cfg, qdir=staging / "q"
    )
    gitapply.commit(git, state, back)
    assert (server.root / "logs" / "x.log").exists()
    gitapply.rollback(git, state, back, partial=partial)
    assert (server.root / "logs").is_dir() and os.listdir(server.root / "logs") == []
    assert os.lstat(server.root / "logs").st_ino == ino


def test_r3_e_l3_state_round_trips(git, user_home, staging, cfg, store, handover_root):
    """The recorded directories survive the journal snapshot (a rollback in a new process keeps them too)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _user_dirs(tgt.root)
    src.write("logs/x.log", "log\n")
    src.commit_all("adds below logs")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    again = gitapply.state_from_dict(gitapply.state_to_dict(state))
    assert again == state
    gitapply.commit(git, again, item)
    gitapply.rollback(git, again, item)
    assert (tgt.root / "logs").is_dir()
