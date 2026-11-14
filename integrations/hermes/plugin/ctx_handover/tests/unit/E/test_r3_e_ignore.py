"""Review round 3, R2-M4: a user's global git ignore file (``.DS_Store``, ``.idea/``, ``*.swp``) does not turn every
in-place update into a fresh directory. The update precondition (``status_clean``) counts untracked files as the
user's own ``git status`` does — the repository's ignore rules and the user's global ignore file, wherever it lives;
what lies where the update writes is still refused by ``gitapply.obstructions``, whatever ignores it (B2; BRIEF D10,
DESIGN §9.5, REVIEW-2 §3 R2-M4)."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops, materialize
from ctx_handover.engine.errors import ConflictError
from ctx_handover.engine.model import DestMode, ItemKind, TargetDecision
from ctx_handover.tests.fakes.git_fake import RecordingJournal
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.unit.E._support import AT, apply_fresh, pack, write_global_gitconfig
from ctx_handover.tests.unit.E.test_materialize import _assess, _item

GLOBAL = ".DS_Store\n.idea/\n*.swp\n"
USER_DATA = b"\x00\x01 the target user's own file\n"


def _global_ignore(user_home: Path, where: str, monkeypatch) -> Path:
    """The user's global ignore file: the XDG default (``$XDG_CONFIG_HOME/git/ignore``, or ``~/.config/git/ignore``
    without ``XDG_CONFIG_HOME``) or a file ``core.excludesFile`` in ``~/.gitconfig`` names."""
    if where == "xdg-unset":
        monkeypatch.delenv("XDG_CONFIG_HOME", raising=False)
        path = user_home / ".config" / "git" / "ignore"
    elif where == "xdg":
        path = Path(os.environ["XDG_CONFIG_HOME"]) / "git" / "ignore"
    else:
        path = user_home / "global-ignore"
        write_global_gitconfig(user_home, "[core]\n\texcludesFile = ~/global-ignore\n")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(GLOBAL, encoding="utf-8")
    gitops.reset_process_caches()
    return path


def _user_files(root: Path) -> None:
    (root / ".DS_Store").write_bytes(USER_DATA)
    (root / ".idea").mkdir()
    (root / ".idea" / "workspace.xml").write_bytes(USER_DATA)
    (root / "notes.swp").write_bytes(USER_DATA)


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


@pytest.mark.parametrize("where", ["xdg", "xdg-unset", "gitconfig"])
def test_r3_e_m4_status_clean_follows_the_users_global_ignore(git, user_home, monkeypatch, where):
    """★ R2-M4 (probe of the reviewer): a clean clone plus ``.DS_Store``, ``.idea/…`` and ``*.swp`` files the user's
    global ignore file hides is clean for the update precondition — the same answer for the XDG default and for a
    ``core.excludesFile`` in ``~/.gitconfig`` (W5: never dependent on where the file lives). An untracked file no
    rule ignores still makes it dirty (mutant: the received profile reads no global ignore file)."""
    _global_ignore(user_home, where, monkeypatch)
    r = RepoBuilder.init(user_home / "r")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    _user_files(r.root)
    probe = gitops.GitProbe(git, profile="received")
    assert probe.status_clean(str(r.root)) == (True, "")
    r.write("notes.txt", "not ignored by anything\n")
    clean, why = probe.status_clean(str(r.root))
    assert clean is False and "notes.txt" in why


def test_r3_e_m4_status_clean_reads_the_pinned_home(git, user_home, tmp_path, monkeypatch):
    """The global ignore file is the one of the runner's home (``Git(home=…)``, W5) — the home whose ``git status``
    the user sees in the agent's terminal — never the process home's."""
    monkeypatch.delenv("XDG_CONFIG_HOME", raising=False)
    pinned = tmp_path / "pinned-home"
    (pinned / ".config" / "git").mkdir(parents=True)
    (pinned / ".config" / "git" / "ignore").write_text(".DS_Store\n", encoding="utf-8")
    (user_home / ".config" / "git").mkdir(parents=True)
    (user_home / ".config" / "git" / "ignore").write_text("*.swp\n", encoding="utf-8")
    r = RepoBuilder.init(user_home / "r")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    (r.root / ".DS_Store").write_bytes(USER_DATA)
    assert gitops.GitProbe(gitops.Git(home=pinned), profile="received").status_clean(str(r.root))[0] is True
    assert gitops.GitProbe(gitops.Git(), profile="received").status_clean(str(r.root))[0] is False
    (r.root / ".DS_Store").unlink()
    (r.root / "notes.swp").write_bytes(USER_DATA)
    assert gitops.GitProbe(gitops.Git(home=pinned), profile="received").status_clean(str(r.root))[0] is False


def test_r3_e_m4_global_ignore_is_no_received_config(git, user_home, monkeypatch):
    """Only the clean-status question reads the user's global ignore file: every other received-profile call keeps
    ``core.excludesFile=/dev/null`` (W5) — e.g. ``ls-files --others --exclude-standard`` lists ``.DS_Store``."""
    _global_ignore(user_home, "gitconfig", monkeypatch)
    r = RepoBuilder.init(user_home / "r")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    _user_files(r.root)
    listed = git.out(
        ["ls-files", "--others", "--exclude-standard"], cwd=r.root, spec=gitops.GitEnvSpec(profile="received")
    )
    assert ".DS_Store" in listed.split("\n")


@pytest.mark.parametrize("where", ["xdg", "gitconfig"])
def test_r3_e_m4_update_with_globally_ignored_files_stays_in_place(
    git, user_home, tmp_path, staging, cfg, store, handover_root, monkeypatch, where
):
    """★ R2-M4 through the production path: the target holds ``.DS_Store``, ``.idea/workspace.xml`` and
    ``notes.swp`` its user's global ignore file hides; the source advanced without touching those paths.
    ``assess`` decides UPDATE, the workspace transaction updates in place (not a fresh ``…-handover-…`` folder), and
    the user's files keep their bytes (mutant: as above)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    _global_ignore(user_home, where, monkeypatch)
    _user_files(tgt.root)
    src.write("b.txt", "b\n")
    src.commit_all("c2")
    info = gitops.GitProbe(git).info(str(src.root), cfg)
    plan_item = _item(ItemKind.GIT_REPO, src.root, git_info=info, item_id="u")
    a = _assess(git, cfg, plan_item, tgt.root, user_home, tmp_path, recorded=True)
    assert a.mode is DestMode.UPDATE and a.conflict is None
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    txn = materialize.WorkspaceTxn(
        "01JOB0000000000000000R3EM4X", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg, now=lambda: AT
    )
    txn.prepare_item(plan_item, TargetDecision("u", str(tgt.root), DestMode.UPDATE, "return-mapping", None), item)
    for _, forward, _ in txn.commit_steps():
        forward()
    ((_, final, mode),) = txn.dests()
    assert (mode, final) == (DestMode.UPDATE, str(tgt.root))
    assert tgt.head() == src.head()
    assert (tgt.root / "b.txt").read_text() == "b\n"
    for rel in (".DS_Store", ".idea/workspace.xml", "notes.swp"):
        assert (tgt.root / rel).read_bytes() == USER_DATA


def test_r3_e_m4_added_path_on_a_globally_ignored_file_still_conflicts(
    git, user_home, tmp_path, staging, cfg, store, handover_root, monkeypatch
):
    """★ B2 stays whole: the source committed ``.idea/workspace.xml``; at the target the same path holds the user's
    file, hidden only by the global ignore file. PREPARE refuses (``dirty``), the transaction lands fresh beside it,
    and the target's file keeps its bytes (mutant: no obstruction check)."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    src.write(".idea/workspace.xml", "from the source\n")
    src.commit_all("tracks the IDE file")
    _global_ignore(user_home, "gitconfig", monkeypatch)
    _user_files(tgt.root)
    before = dir_fingerprint(tgt.root)
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    with pytest.raises(ConflictError) as exc:
        gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    assert exc.value.details["kind"] == "dirty"
    assert ".idea/workspace.xml" in exc.value.details["paths"]
    info = gitops.GitProbe(git).info(str(src.root), cfg)
    plan_item = _item(ItemKind.GIT_REPO, src.root, git_info=info, item_id="u")
    txn = materialize.WorkspaceTxn(
        "01JOB0000000000000000R3EM4Y", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg, now=lambda: AT
    )
    txn.prepare_item(plan_item, TargetDecision("u", str(tgt.root), DestMode.UPDATE, "return-mapping", None), item)
    for _, forward, _ in txn.commit_steps():
        forward()
    ((_, final, mode),) = txn.dests()
    assert mode is DestMode.FRESH and final != str(tgt.root)
    assert (Path(final) / ".idea" / "workspace.xml").read_text() == "from the source\n"
    assert dir_fingerprint(tgt.root) == before


def test_r3_e_m4_globally_ignored_file_appearing_before_c1_stops_it(
    git, user_home, staging, cfg, store, handover_root, monkeypatch
):
    """★ B2 at c1 with a global ignore: ``x.swp`` (hidden only by the global file) appears between PREPARE and
    commit at a path the update adds — c1 stops before the repository changes, the file keeps its bytes."""
    src, tgt = _seeded(git, user_home, staging, cfg, store, handover_root)
    src.write("x.swp", "tracked on the source\n")
    src.commit_all("tracks a swap file")
    _global_ignore(user_home, "xdg", monkeypatch)
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    h0 = tgt.head()
    (tgt.root / "x.swp").write_bytes(USER_DATA)
    marks: list[int] = []
    with pytest.raises(ConflictError) as exc:
        gitapply.commit(git, state, item, mark=lambda: marks.append(1))
    assert exc.value.details["kind"] == "dirty"
    assert marks == [] and tgt.head() == h0
    assert (tgt.root / "x.swp").read_bytes() == USER_DATA
