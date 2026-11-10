"""First review round (the first external review): sizes, limits and the summary before sending.

* H12 — a file tree beyond ``max_files`` or the byte cap was planned as included with the count where counting
  stopped; the pack refused it after the lock.
* H13 — the git size estimate was 0 for every repository whose ``.git`` is a gitfile (linked worktree, submodule,
  ``--separate-git-dir``) and ignored alternates.
* L10 — ``--include`` released a suspected secret as ``yes``.
* M37 — the summary before sending named no path outside the workspace, only a count.
* M12 (D part, named limit) — an unknown context window on the target is said, not hidden.

Repositories come from ``tests/fakes/repo_builder.py`` (technical identity, no git config written).
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, plan
from ctx_handover.engine.errors import PolicyDenied
from ctx_handover.engine.gitops import RECEIVED, Git, GitProbe
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.model import Candidate, Include, ItemKind, ItemRole, Options, Origin, PlanHints, Weight
from ctx_handover.tests.fakes.planning_fake import env_facts, final_plan, model_change
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.D.conftest import write
from ctx_handover.tests.unit.D.test_plan import _build, _extra, _git_info, _items, _nt, _primary

MIB = 1024**2


def _tree(root: Path, files: int, size: int = 100) -> int:
    for n in range(files):
        write(root / "d" / f"f{n:03d}.txt", "x" * size)
    return files * size


def _primary_item(rp):
    return next(i for i in rp.items if i.role is ItemRole.PRIMARY)


# ---------------------------------------------------------------------------------------------------------
# H12 — trees beyond the limits are left out at planning, not after the lock
# ---------------------------------------------------------------------------------------------------------


def test_h12_workspace_tree_over_max_files_is_left_out_and_asked(lay):
    """★ H12: the session's folder (no git) holds more entries than ``max_files``. The pack would refuse it
    after the lock (``filetree.scan`` → ``PolicyDenied(max_files)``); the plan now leaves it out, names it as ``too
    large`` with the limit in its reasons, and R30's reason (h) asks even with ``--yes``.

    Red on ``2d35ba93``: the workspace was included (``Include.YES``) with the count where counting stopped.

    Mutant: ``finish`` ignores the ``over`` answer of ``_tree_count`` for file trees."""
    _tree(lay.project, 12)
    options = Options(max_files=5)
    cfg = lay.cfg(options=options)
    with pytest.raises(PolicyDenied):  # what happened after the lock before
        filetree.scan(lay.project, IgnoreMatcher.for_tree(lay.project, ()), cfg)
    rp = _build(lay, [_primary(lay)], options=options, cfg=cfg)
    primary = _primary_item(rp)
    assert primary.include is Include.NO, primary
    assert any("more than 5 entries (max_files)" in r for r in primary.reasons), primary.reasons
    real = os.path.realpath(lay.project)
    assert (real, plan.WHY_TOO_LARGE) in _nt(rp)
    final = final_plan(rp)
    assert plan.presend_summary(final)["workspace_excluded"]["reason"] == "too_large"
    reasons = plan.confirmation_reasons(final, Options(yes=True), False)
    assert plan.CONFIRM_WORKSPACE_EXCLUDED in plan.yes_exempt(reasons)


def test_h12_trees_over_the_byte_cap_are_left_out_never_sized_capped(lay):
    """★ H12: a workspace tree over ``max_total_gb`` and an extra folder over ``max_extra_dir_mb`` are left out and
    named with the limit that stopped the count; a tree inside both limits keeps its exact size.

    Red on ``2d35ba93``: the workspace tree stayed ``yes`` with the bytes counted up to the cap.

    Mutant: ``_tree_count`` answers None (complete) when the byte cap stops it."""
    total = _tree(lay.project, 30, 200)
    big_dir = lay.home / "big"
    _tree(big_dir, 10, 300)
    options = Options(max_total_bytes=total - 1, max_extra_dir_bytes=2000)
    rp = _build(lay, [_primary(lay), _extra(big_dir, Weight.READ, ItemKind.EXTRA_DIR)], options=options)
    items = _items(rp)
    primary = items[os.path.realpath(lay.project)]
    assert primary.include is Include.NO
    assert any(r.endswith(f"more than {total - 1} bytes (max_total_gb)") for r in primary.reasons), primary.reasons
    extra = items[str(big_dir)]
    assert extra.include is Include.NO
    assert any(r.endswith("more than 2000 bytes (max_extra_dir_mb)") for r in extra.reasons), extra.reasons
    fits = _build(lay, [_primary(lay)], options=Options(max_total_bytes=total + 100))
    assert _primary_item(fits).include is Include.YES
    assert _primary_item(fits).size_estimate == total + len("print('hi')\n")


# ---------------------------------------------------------------------------------------------------------
# H13 — git sizes behind gitfiles and alternates
# ---------------------------------------------------------------------------------------------------------


def _store_bytes(objects: Path) -> int:
    return sum(p.stat().st_size for p in objects.rglob("*") if p.is_file())


def _payload_repo(root: Path) -> RepoBuilder:
    repo = RepoBuilder.init(root)
    repo.write("blob.bin", os.urandom(200_000))
    repo.write("README.md", "payload\n")
    repo.commit_all("payload")
    return repo


def _plan_git(lay, root: Path):
    cand = Candidate(ItemKind.GIT_REPO, os.path.realpath(root), Weight.MENTION, ("cwd",), Origin.CWD)
    probe = GitProbe(Git(), ceiling=lay.home.parent)
    return _primary_item(_build(lay, [cand], git_info=lambda r: probe.info(r, lay.cfg())))


def test_h13_git_size_behind_gitfiles_and_alternates(lay, tmp_path):
    """★ H13: the size estimate of a git workspace counts the objects a fresh pack reads — for a linked
    worktree (Hermes kanban's ``<repo>/.worktrees/<task>``) the main repository's store, for a submodule its store in
    ``.git/modules``, for ``--separate-git-dir`` the separate git dir, for a ``clone --shared`` the alternates too.

    Red on ``2d35ba93``: each estimate was 0 plus untracked bytes (``assert 0 >= 200000``).

    Mutant: ``git_object_dirs`` ignores ``commondir``; ``git_object_dirs`` skips the alternates."""
    main = tmp_path / "main"
    repo = _payload_repo(main)
    main_store = _store_bytes(main / ".git" / "objects")
    assert main_store >= 200_000

    worktree = main / ".worktrees" / "task-1"
    repo.run("worktree", "add", "-q", "-b", "task-1", "--", str(worktree))
    assert (worktree / ".git").is_file()
    assert _plan_git(lay, worktree).size_estimate >= main_store

    sup = RepoBuilder.init(tmp_path / "super")
    sup.write("README.md", "super\n")
    sup.run("-c", "protocol.file.allow=always", "submodule", "add", "-q", "--", str(main), "lib")
    sup.commit_all("lib")
    sub_store = _store_bytes(tmp_path / "super" / ".git" / "modules" / "lib" / "objects")
    assert (tmp_path / "super" / "lib" / ".git").is_file() and sub_store >= 200_000
    assert _plan_git(lay, tmp_path / "super" / "lib").size_estimate >= sub_store

    separate = tmp_path / "separate"
    gitdir = tmp_path / "separate.git"
    Git().run(
        ["init", "-q", "--template=", "-b", "main", f"--separate-git-dir={gitdir}", str(separate)],
        cwd=tmp_path,
        spec=RECEIVED,
    )
    sep = RepoBuilder(separate)
    sep.write("blob.bin", os.urandom(200_000))
    sep.commit_all("payload")
    assert (separate / ".git").is_file()
    assert _plan_git(lay, separate).size_estimate >= _store_bytes(gitdir / "objects")

    shared = tmp_path / "shared"
    RepoBuilder(tmp_path).run(
        "-c", "protocol.file.allow=always", "clone", "-q", "--shared", "--", str(main), str(shared)
    )
    assert (shared / ".git" / "objects" / "info" / "alternates").is_file()
    assert _plan_git(lay, shared).size_estimate >= main_store


def test_h13_unmeasurable_history_is_unknown_and_asked(lay):
    """★ H13: a repository whose object store cannot be found (a gitfile pointing nowhere) is never sized 0 silently —
    its item carries ``SIZE_UNKNOWN`` and reason (c) asks although the counted size is small; a plain directory
    without history (``head`` None) is not unknown.

    Red on ``2d35ba93``: no marker, no reason — the estimate was the untracked bytes (``AttributeError`` on the
    marker before that: no such name).

    Mutant: ``finish`` treats ``None`` from ``git_object_bytes`` as 0 without the marker."""
    write(lay.project / ".git", "gitdir: /nonexistent/elsewhere\n")
    assert plan.git_object_bytes(lay.project) == (None, False)
    rp = _build(lay, [_primary(lay, ItemKind.GIT_REPO)])
    primary = _primary_item(rp)
    assert plan.SIZE_UNKNOWN in primary.reasons
    reasons = plan.confirmation_reasons(final_plan(rp), Options(), False)
    assert plan.CONFIRM_LARGE in reasons, reasons
    unborn = _build(lay, [_primary(lay, ItemKind.GIT_REPO)], git_info=_git_info(head=None))
    assert plan.SIZE_UNKNOWN not in _primary_item(unborn).reasons


# ---------------------------------------------------------------------------------------------------------
# L10 — --include never releases a suspected secret
# ---------------------------------------------------------------------------------------------------------

TOKEN_TEXT = "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"


def test_l10_include_keeps_a_suspected_secret_a_proposal(lay):
    """★ L10 / R25: ``--include`` on a file with a token — given only by the user, and also when the
    transcript already proposed it — leaves it a proposal with ``SUSPECTED_SECRET`` that ``yes`` does not take;
    ``--include-secret`` on the same file releases it as ``yes`` without the suspicion label.

    Red on ``2d35ba93``: ``--include`` made it ``Include.YES`` (``assert <Include.YES> is <Include.PROPOSED>``).

    Mutant: ``suggested_path`` sets ``yes`` for every user path again."""
    tok = write(lay.notes / "keys.txt", TOKEN_TEXT)
    seen = write(lay.notes / "seen-keys.txt", TOKEN_TEXT)
    hints = PlanHints(include=(str(tok), str(seen)))
    rp = _build(lay, [_primary(lay), _extra(seen, Weight.READ)], hints=hints)
    items = _items(rp)
    for path in (tok, seen):
        item = items[str(path)]
        assert item.include is Include.PROPOSED, (path, item.include)
        assert plan.SUSPECTED_SECRET in item.reasons
    chosen = _items(plan.apply_choice(final_plan(rp), "yes").plan)
    assert chosen[str(tok)].include is Include.NO and chosen[str(seen)].include is Include.NO

    released = lay.cfg(include_secret=[str(tok), str(seen)])
    rp2 = _build(lay, [_primary(lay), _extra(seen, Weight.READ)], hints=hints, cfg=released)
    for path in (tok, seen):
        item = _items(rp2)[str(path)]
        assert item.include is Include.YES, (path, item.include)
        assert plan.SUSPECTED_SECRET not in item.reasons
        assert "suspected secret: proposed only" not in item.reasons


# ---------------------------------------------------------------------------------------------------------
# M37 — the summary before sending names the paths outside the workspace
# ---------------------------------------------------------------------------------------------------------


def test_m37_presend_names_paths_outside_the_workspace_bounded(lay):
    """★ M37: ``presend_summary`` lists every included item outside the workspace with path and size —
    largest first, at most ``PRESEND_PATHS_MAX`` — and counts the rest (``outside_more``); the workspace itself and
    proposals that were not confirmed are not in it.

    Red on ``2d35ba93``: no such key (``KeyError: 'outside_items'``); only the count ``outside_workspace`` existed.

    Mutant: ``outside_items`` drops the cap (``limit`` ignored)."""
    files = [write(lay.notes / f"n{n:02d}.md", "y" * (100 + n)) for n in range(plan.PRESEND_PATHS_MAX + 2)]
    cands = [_primary(lay), *(_extra(f, Weight.READ) for f in files)]
    rp = _build(lay, cands, options=Options(extra_paths="auto"))
    summary = plan.presend_summary(final_plan(rp))
    shown = summary["outside_items"]
    assert len(shown) == plan.PRESEND_PATHS_MAX and summary["outside_more"] == 2
    expected = sorted(files, key=lambda f: -f.stat().st_size)[: plan.PRESEND_PATHS_MAX]
    assert [r["path"] for r in shown] == [str(f) for f in expected]
    assert [r["size"] for r in shown] == [f.stat().st_size for f in expected]
    assert {r["kind"] for r in shown} == {"extra_file"} and {r["role"] for r in shown} == {"extra"}
    assert os.path.realpath(lay.project) not in {r["path"] for r in shown}
    assert set(plan.PRESEND_KEYS) >= {"outside_items", "outside_more"}

    asked = _build(lay, cands, options=Options(extra_paths="ask"))
    proposed = plan.presend_summary(final_plan(asked))
    assert proposed["outside_items"] == [] and proposed["outside_more"] == 0


# ---------------------------------------------------------------------------------------------------------
# M12 (D part) — an unknown window on the target is said
# ---------------------------------------------------------------------------------------------------------


def test_m12_unknown_context_window_is_said_not_asked(lay):
    """★ M12 (D part, named limit): the target's model has no ``context_length`` (Hermes gives a plugin only the value
    set in ``config.yaml``) — ``presend_summary`` carries ``context_unknown`` with the session's size and the model;
    no reason (i) is raised. A known window, or no target facts yet, gives None.

    Red on ``2d35ba93``: no such key (``KeyError: 'context_unknown'``).

    Mutant: ``context_unknown`` answers None when the window is unknown."""
    rp = _build(lay, [_primary(lay)])
    final = final_plan(rp)
    unknown = env_facts("laptop", host={"context_length": None, "model": "qwen3"})
    summary = plan.presend_summary(final, target_facts=unknown)
    assert summary["context_unknown"] == {"tokens": plan.context_tokens_of(final), "model": "qwen3"}
    assert plan.CONFIRM_CONTEXT not in plan.confirmation_reasons(final, Options(), False, target_facts=unknown)
    changed = final_plan(rp, model_change=model_change(to_model="big", to_provider="p", context_length=None))
    assert plan.presend_summary(changed)["context_unknown"]["model"] == "big"
    assert plan.presend_summary(final, target_facts=env_facts("laptop"))["context_unknown"] is None
    assert plan.presend_summary(final)["context_unknown"] is None
