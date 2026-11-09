"""Review round rv-e, finding B2: update and return mode never overwrite or delete what lies at the destination
outside the repository's tracked content (ignored files, ignored directories), and their inverse never deletes a
file someone changed after the forward step (BRIEF D10, I5, DESIGN §9.5)."""

from __future__ import annotations

from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops, materialize
from ctx_handover.engine.errors import ConflictError
from ctx_handover.engine.model import DestMode, Include, ItemKind, ItemRole, Origin, PlanItem, TargetDecision
from ctx_handover.tests.fakes.git_fake import RecordingJournal
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.unit.E._support import AT, apply_fresh, pack

USER_DATA = "TARGET USER DATA\n"


def _clean_copy(git, store, cfg, src: RepoBuilder, staging: Path, dest: Path) -> RepoBuilder:
    _, item = pack(git, src.root, staging, cfg, store, item_id="seed")
    t = RepoBuilder(apply_fresh(git, store, item, dest, cfg))
    t.run("stash", "clear")
    t.run("reset", "-q", "--hard")
    t.run("clean", "-qfdx")
    return t


def _exclude(repo: RepoBuilder, *patterns: str) -> None:
    info = repo.root / ".git" / "info"
    info.mkdir(exist_ok=True)
    (info / "exclude").write_text("".join(p + "\n" for p in patterns), encoding="utf-8")


def _update_item(git, store, cfg, src: RepoBuilder, tgt: RepoBuilder, staging: Path) -> dict:
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    return pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")[1]


def _seeded(git, user_home, staging, cfg, store, handover_root) -> tuple[RepoBuilder, RepoBuilder]:
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.write("d/x.txt", "x\n")
    src.commit_all("c1")
    return src, _clean_copy(git, store, cfg, src, staging, handover_root / "app")


# ---------------------------------------------------------------------------------------------------------
# update mode
# ---------------------------------------------------------------------------------------------------------


def test_rv_e_b2_update_refuses_ignored_file_in_the_way(git, user_home, staging, cfg, store, handover_root):
    """★ B2: the target ignores ``local.cfg`` (``info/exclude``) and keeps its own data there; the source has the
    same path untracked. PREPARE refuses (``dirty``) and the file keeps its bytes (mutant: no obstruction check)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _exclude(tgt, "local.cfg")
    tgt.write("local.cfg", USER_DATA)
    assert gitops.GitProbe(git, profile="received").status_clean(str(tgt.root))[0]
    src.write("local.cfg", "from source\n")
    item = _update_item(git, store, cfg, src, tgt, staging)
    before = tgt.worktree_digest()
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert exc.value.details["kind"] == "dirty"
    assert "local.cfg" in exc.value.message
    assert (tgt.root / "local.cfg").read_text() == USER_DATA
    assert tgt.worktree_digest() == before


def test_rv_e_b2_update_commit_rechecks_ignored_file(git, user_home, staging, cfg, store, handover_root):
    """★ B2 at c1: an ignored file created between PREPARE and commit at a path the update adds stops c1 before
    the repository changes (mutant: no re-check in ``commit``)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _exclude(tgt, "local.cfg")
    src.write("local.cfg", "from source\n")
    item = _update_item(git, store, cfg, src, tgt, staging)
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    h0 = tgt.head()
    tgt.write("local.cfg", USER_DATA)
    marks: list[int] = []
    with pytest.raises(ConflictError) as exc:
        gitapply.commit(git, state, item, mark=lambda: marks.append(1))
    assert exc.value.details["kind"] == "dirty"
    assert marks == []
    assert (tgt.root / "local.cfg").read_text() == USER_DATA
    assert tgt.head() == h0


def test_rv_e_b2_update_refuses_ignored_dir_at_file_path(git, user_home, staging, cfg, store, handover_root):
    """★ B2 (second form): an ignored directory ``node_modules/`` with content where the source has a file
    ``node_modules`` — the update refuses instead of replacing the directory (mutant: only files checked)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _exclude(tgt, "node_modules/")
    tgt.write("node_modules/pkg/index.js", USER_DATA)
    src.write("node_modules", "a file on the source\n")
    item = _update_item(git, store, cfg, src, tgt, staging)
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert exc.value.details["kind"] == "dirty"
    assert (tgt.root / "node_modules" / "pkg" / "index.js").read_text() == USER_DATA


def test_rv_e_b2_update_refuses_ignored_file_at_dir_prefix(git, user_home, staging, cfg, store, handover_root):
    """★ B2 (third form): an ignored file ``build`` where the source has ``build/out.txt`` (mutant: only the full
    path checked, not its parent directories)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _exclude(tgt, "build")
    tgt.write("build", USER_DATA)
    src.write("build/out.txt", "out\n")
    item = _update_item(git, store, cfg, src, tgt, staging)
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert exc.value.details["kind"] == "dirty"
    assert (tgt.root / "build").read_text() == USER_DATA


def test_rv_e_b2_update_tracked_dir_replaced_by_file(git, user_home, staging, cfg, store, handover_root):
    """The check is not stricter than needed: a tracked directory ``d/`` (nothing else in it) that the source
    replaced by a file ``d`` still updates in place."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    src.rm("d/x.txt")
    src.write("d", "now a file\n")
    src.commit_all("d becomes a file")
    item = _update_item(git, store, cfg, src, tgt, staging)
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert state.mode is DestMode.UPDATE
    gitapply.commit(git, state, item)
    assert (tgt.root / "d").read_text() == "now a file\n"
    assert tgt.head() == src.head()


def test_rv_e_b2_update_ignored_file_elsewhere_is_no_conflict(git, user_home, staging, cfg, store, handover_root):
    """An ignored file the update does not touch stays and does not block the update."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _exclude(tgt, "local.cfg")
    tgt.write("local.cfg", USER_DATA)
    src.write("other.txt", "new\n")
    item = _update_item(git, store, cfg, src, tgt, staging)
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    assert (tgt.root / "other.txt").read_text() == "new\n"
    gitapply.rollback(git, state, item)
    assert not (tgt.root / "other.txt").exists()
    assert (tgt.root / "local.cfg").read_text() == USER_DATA


def test_rv_e_b2_update_via_txn_lands_fresh(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """★ B2 through the production path: the workspace transaction falls back to a fresh directory, the ignored
    file at the original destination keeps its bytes (mutant: no obstruction check)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _exclude(tgt, "local.cfg")
    tgt.write("local.cfg", USER_DATA)
    before = dir_fingerprint(tgt.root)
    src.write("local.cfg", "from source\n")
    item = _update_item(git, store, cfg, src, tgt, staging)
    plan_item = PlanItem(
        item_id="u",
        kind=ItemKind.GIT_REPO,
        source_path=str(src.root),
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=1000,
    )
    txn = materialize.WorkspaceTxn(
        "01JOB0000000000000000RVEB2X", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg, now=lambda: AT
    )
    txn.prepare_item(plan_item, TargetDecision("u", str(tgt.root), DestMode.UPDATE, "suggested", None), item)
    for _, forward, _ in txn.commit_steps():
        forward()
    ((_, final, mode),) = txn.dests()
    assert mode is DestMode.FRESH and final != str(tgt.root)
    assert (Path(final) / "local.cfg").read_text() == "from source\n"
    assert dir_fingerprint(tgt.root) == before


@pytest.mark.parametrize("partial", [False, True])
def test_rv_e_b2_update_rollback_keeps_files_changed_after_commit(
    git, user_home, staging, cfg, store, handover_root, partial
):
    """★ B2 inverse: files the update added and someone changed after c1 survive the rollback (as untracked
    files), also the resetting one of an interrupted inverse; everything else returns to H0 (mutant: the inverse
    deletes every added path)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    h0 = tgt.head()
    src.write("new_committed.txt", "c\n")
    src.write("unchanged_new.txt", "u\n")
    src.commit_all("advance")
    src.write("new_untracked.txt", "w\n")
    item = _update_item(git, store, cfg, src, tgt, staging)
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    tgt.write("new_committed.txt", "EDITED AFTER COMMIT\n")
    tgt.write("new_untracked.txt", "EDITED AFTER COMMIT TOO\n")
    gitapply.rollback(git, state, item, partial=partial)
    assert tgt.head() == h0
    assert (tgt.root / "new_committed.txt").read_text() == "EDITED AFTER COMMIT\n"
    assert (tgt.root / "new_untracked.txt").read_text() == "EDITED AFTER COMMIT TOO\n"
    assert not (tgt.root / "unchanged_new.txt").exists()
    tracked = tgt.out("ls-files")
    assert "new_committed.txt" not in tracked and "unchanged_new.txt" not in tracked


# ---------------------------------------------------------------------------------------------------------
# return mode
# ---------------------------------------------------------------------------------------------------------


def _round_trip(git, user_home, staging, cfg, store, handover_root):
    server = RepoBuilder.init(user_home / "server")
    server.write("a.txt", "a\n")
    server.commit_all("c1")
    _exclude(server, "notes.txt")
    server.write("notes.txt", "SERVER-ONLY NOTES\n")
    out, item = pack(git, server.root, staging, cfg, store, item_id="out")
    laptop = RepoBuilder(apply_fresh(git, store, item, handover_root / "laptop", cfg))
    return server, laptop, out.state


def test_rv_e_b2_return_refuses_ignored_file_in_the_way(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """★ B2 on the return trip: ``notes.txt`` is ignored on the server only (``info/exclude``), the laptop sends it
    back as untracked; return mode refuses instead of replacing the server's notes (mutant: no obstruction
    check)."""
    server, laptop, out_state = _round_trip(git, user_home, staging, cfg, store, handover_root)
    assert not (laptop.root / "notes.txt").exists()
    laptop.write("notes.txt", "laptop notes\n")
    basis = gitops.GitProbe(git).ref_tips(str(server.root))
    _, back = pack(git, laptop.root, staging, cfg, store, basis=basis, item_id="back")
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_return(
            git, store, back, server.root, out_state, tmp=tmp_path / "rt", cfg=cfg, qdir=staging / "q"
        )
    assert exc.value.details["kind"] == "dirty"
    assert (server.root / "notes.txt").read_text() == "SERVER-ONLY NOTES\n"


def test_rv_e_b2_return_commit_rechecks(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """★ B2 on the return trip at c1: an ignored file created after PREPARE at an added path stops c1 (mutant:
    no re-check in ``commit``)."""
    server, laptop, out_state = _round_trip(git, user_home, staging, cfg, store, handover_root)
    (server.root / "notes.txt").unlink()
    laptop.write("notes.txt", "laptop notes\n")
    basis = gitops.GitProbe(git).ref_tips(str(server.root))
    _, back = pack(git, laptop.root, staging, cfg, store, basis=basis, item_id="back")
    state = gitapply.prepare_return(
        git, store, back, server.root, out_state, tmp=tmp_path / "rt", cfg=cfg, qdir=staging / "q"
    )
    server.write("notes.txt", "WRITTEN AFTER PREPARE\n")
    with pytest.raises(ConflictError):
        gitapply.commit(git, state, back)
    assert (server.root / "notes.txt").read_text() == "WRITTEN AFTER PREPARE\n"


@pytest.mark.parametrize("partial", [False, True])
def test_rv_e_b2_return_rollback_keeps_file_changed_after_commit(
    git, user_home, tmp_path, staging, cfg, store, handover_root, partial
):
    """★ B2 inverse on the return trip: a file the return added and someone edited after c1 survives the
    rollback (merging and resetting inverse); the rest returns to the out-state (mutant: the inverse removes or
    refuses on every added path)."""
    server, laptop, out_state = _round_trip(git, user_home, staging, cfg, store, handover_root)
    laptop.write("extra.txt", "from the laptop\n")
    laptop.write("plain.txt", "plain\n")
    basis = gitops.GitProbe(git).ref_tips(str(server.root))
    _, back = pack(git, laptop.root, staging, cfg, store, basis=basis, item_id="back")
    state = gitapply.prepare_return(
        git, store, back, server.root, out_state, tmp=tmp_path / "rt", cfg=cfg, qdir=staging / "q"
    )
    gitapply.commit(git, state, back)
    server.write("extra.txt", "EDITED ON THE SERVER AFTER COMMIT\n")
    gitapply.rollback(git, state, back, partial=partial)
    assert (server.root / "extra.txt").read_text() == "EDITED ON THE SERVER AFTER COMMIT\n"
    assert not (server.root / "plain.txt").exists()
    assert (server.root / "notes.txt").read_text() == "SERVER-ONLY NOTES\n"
    assert server.head() == out_state.head
    assert "extra.txt" not in server.out("ls-files") and "plain.txt" not in server.out("ls-files")
