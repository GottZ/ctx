"""r4-ke, review finding R3-M1 (``review-code/REVIEW-3.md`` §3; ``round3/g5-git-r3.md`` N3): the check "has the forward
step c1 touched the repository yet?" (``gitapply._pre_state``) decides whether a failed or resumed step is rolled back.
Since R2-M4 it asked ``status_clean`` in the received profile, which reads the user's global ignore file: a received
untracked file that file hides (``debug.log`` under ``*.log``) was invisible, the repository read as untouched, and a
c1 that failed at its end (``stash store``) left the file at the target — the contract says a failed step leaves the
destination as PREPARE found it.

Now ``_pre_state`` counts untracked files without the user's global ignore file (it stays with the update
precondition, R2-M4), looks on disk at every path the step adds (PREPARE proved them free, ``obstructions``), and
compares the stash depth with the one PREPARE recorded. Real git, the production ``commit``; the failure is injected at
the named git subcommand (``FailingGit``)."""

from __future__ import annotations

from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.engine.gitops import GitCommandError
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import FailingGit, pack
from ctx_handover.tests.unit.E.test_r3_e_ignore import _seeded

RECEIVED = b"RECEIVED FROM PEER\n"


class _NthFails(gitops.Git):
    """A real ``Git`` whose ``n``-th run of the named subcommand fails like a git process that died."""

    def __init__(self, words: tuple[str, ...], n: int):
        super().__init__()
        self.words = words
        self.n = n
        self.hits = 0

    def run(self, args, *, cwd, spec, check: bool = True, **kw):
        if tuple(args[: len(self.words)]) == self.words:
            self.hits += 1
            if self.hits == self.n:
                raise self._error(args, 128, b"fatal: injected failure")
        return super().run(args, cwd=cwd, spec=spec, check=check, **kw)


def _ignore(user_home: Path, text: str) -> None:
    path = user_home / ".config" / "git" / "ignore"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    gitops.reset_process_caches()


def _stash_depth(repo: RepoBuilder) -> int:
    return len(repo.stash_list())


def test_r4_ke_m1_globally_ignored_received_file_is_rolled_back(git, user_home, staging, cfg, store, handover_root):
    """★ The reviewer's case: the target's user ignores ``*.log`` globally; the source has the same HEAD, a stash
    entry and an untracked ``debug.log`` (its host does not ignore it). PREPARE is fine, the tail of c1 fails at
    ``stash store``. The exception propagates and the received ``debug.log`` is gone again. Before the fix:
    ``debug.log`` stayed at the target, hidden from ``git status`` (``_pre_state`` read the repository as
    untouched and skipped the rollback)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    src.write("a.txt", "wip\n")
    src.stash("wip")
    src.write("debug.log", RECEIVED)
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    _ignore(user_home, "*.log\n")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert state.added_paths == ("debug.log",)
    h0 = tgt.head()
    with pytest.raises(GitCommandError):
        gitapply.commit(FailingGit(("stash", "store")), state, item)
    assert not (tgt.root / "debug.log").exists()
    assert tgt.head() == h0 and _stash_depth(tgt) == 0
    assert (tgt.root / "a.txt").read_text() == "a\n"


def test_r4_ke_m1_received_file_hidden_by_the_targets_info_exclude_is_rolled_back(
    git, user_home, staging, cfg, store, handover_root
):
    """★ The same class without a global ignore file: the target repository's own ``.git/info/exclude`` hides the
    received ``debug.log``. The on-disk look at the added paths finds it, the rollback removes it. Before the fix: it
    stayed."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    src.write("a.txt", "wip\n")
    src.stash("wip")
    src.write("debug.log", RECEIVED)
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    exclude = tgt.root / ".git" / "info" / "exclude"
    exclude.parent.mkdir(parents=True, exist_ok=True)
    exclude.write_text("*.log\n", encoding="utf-8")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    with pytest.raises(GitCommandError):
        gitapply.commit(FailingGit(("stash", "store")), state, item)
    assert not (tgt.root / "debug.log").exists()


def test_r4_ke_m1_partly_stored_stash_is_rolled_back(git, user_home, staging, cfg, store, handover_root):
    """★ The stash depth counts: the source has the same HEAD, a clean work tree and two stash entries; c1 stores the
    first, the second ``stash store`` fails. Nothing else changed, but the repository is not untouched — the rollback
    drops the stored entry. Before the fix: one stash entry stayed at the target."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    for text in ("one\n", "two\n"):
        src.write("a.txt", text)
        src.stash(text.strip())
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert state.stash_before == 0 and len(item["stash"]) == 2
    bad = _NthFails(("stash", "store"), 2)
    with pytest.raises(GitCommandError):
        gitapply.commit(bad, state, item)
    assert bad.hits == 2
    assert _stash_depth(tgt) == 0


def test_r4_ke_m1_r2_m4_holds_and_the_users_ignored_files_survive_a_failed_step(
    git, user_home, staging, cfg, store, handover_root
):
    """R2-M4 is not broken: with the user's own globally ignored ``notes.log`` at the target, the update precondition
    still passes (``status_clean`` → ``(True, '')``). When c1 then fails, the rollback leaves that file as it was and
    removes only what the step added."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _ignore(user_home, "*.log\n")
    (tgt.root / "notes.log").write_bytes(b"the target user's own log\n")
    assert gitops.GitProbe(git, profile="received").status_clean(str(tgt.root)) == (True, "")
    src.write("b.txt", "b\n")
    src.commit_all("c2")
    src.write("a.txt", "wip\n")
    src.stash("wip")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    h0 = tgt.head()
    with pytest.raises(GitCommandError):
        gitapply.commit(FailingGit(("stash", "store")), state, item)
    assert tgt.head() == h0
    assert not (tgt.root / "b.txt").exists()
    assert (tgt.root / "notes.log").read_bytes() == b"the target user's own log\n"
    assert gitops.GitProbe(git, profile="received").status_clean(str(tgt.root)) == (True, "")


def test_r4_ke_m1_pre_state_sees_an_added_file_the_global_ignore_hides(
    git, user_home, staging, cfg, store, handover_root
):
    """★ ``_pre_state`` itself: right after PREPARE (nothing of c1 ran) it is True; once a path the step adds holds
    something — here the received ``debug.log``, hidden by the user's global ``*.log`` — it is False. Before the fix:
    True both times."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    src.write("b.txt", "b\n")
    src.commit_all("c2")
    src.write("debug.log", RECEIVED)
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    _ignore(user_home, "*.log\n")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert gitapply._pre_state(git, state, tgt.root, staging / "pre")
    (tgt.root / "debug.log").write_bytes(RECEIVED)  # what the forward step would have written
    assert not gitapply._pre_state(git, state, tgt.root, staging / "pre")
