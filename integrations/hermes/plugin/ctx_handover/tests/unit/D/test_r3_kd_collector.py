"""The collector's workspace and repository rules after review round 2 and the lab run.

* **R2-M3** (the second review §3; second form of L3-F19): ``.gitignore`` ``tmp/*`` +
  ``!tmp/.gitkeep``, session cwd ``tmp``. git does not ignore ``tmp`` itself and ``.gitkeep`` is tracked, so R36/R36a
  took the whole repository as the workspace without a question — and the files the agent wrote into ``tmp/`` never
  travelled (git does not pack ignored files). R36 now judges the cwd by its content: when git carries nothing from it
  but placeholders (``.gitkeep``, ``.keep``, ``.gitignore`` …) while it holds other entries, the cwd is a scratch
  folder of that repository — the cwd itself travels as a file tree, exactly like an ignored cwd.
* **R2-M2** (H15 reason (k)): an untracked clone inside the workspace that the transcript never touches was
  neither named nor asked about; the pack step left it out silently. Every such clone (git's own list, the one the
  pack step uses) is now a candidate with ``INNER_REPO_REASON``: the plan names it and asks (k).
* **L3-F26** (lab): a file the transcript mentions in a git-ignored folder of another
  repository made that whole repository a candidate (F19 for mentions). R36 holds there too: an ignored path is the
  file itself, never the repository around it.
* **H5 rest on the source side**: the home rule (``collector.HomeRule``) knows the Windows homes behind WSL's drive
  mounts (``policy.WSL_HOME_PARENTS``) and remembers its home list per user folder, not per drive.

Repositories come from ``tests/fakes/repo_builder.py`` (technical identity, no git config written). The probe is
``planning_fake.CeilingGitProbe`` — real ``git`` with a ceiling at the test's tmp directory; its ``carried_files`` and
``nested_repos`` are the same git commands the wish for ``gitops.GitProbe`` names.
"""

from __future__ import annotations

import os
from pathlib import Path

from ctx_handover.engine import collector, plan
from ctx_handover.engine.model import (
    HermesPaths,
    Include,
    ItemKind,
    Options,
    Origin,
    PlanHints,
    RefineReport,
)
from ctx_handover.tests.fakes.planning_fake import (
    Busy,
    CeilingGitProbe,
    StaticGitProbe,
    fake_redact,
    final_plan,
    static_discover,
)
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.D.conftest import write


def _hermes(lay) -> HermesPaths:
    return HermesPaths(home=str(lay.hermes_home), document_cache=str(lay.hermes_home / "cache" / "documents"))


def _probe(lay) -> CeilingGitProbe:
    return CeilingGitProbe(lay.home.parent)


def _collect(t, lay, git=None, cfg=None):
    return collector.collect(
        t.rows(),
        t.session_row(),
        _hermes(lay),
        cfg or lay.cfg(),
        git or _probe(lay),
        source_os="linux",
        discover=static_discover([]),
    )


def _plan(cands, lay, *, options: Options | None = None):
    options = options or Options()
    cfg = lay.cfg(options=options)
    from ctx_handover.engine.gitops import Git, GitProbe

    return plan.build_plan(
        "01JCJOB",
        (1,),
        cands,
        None,
        RefineReport(used="off"),
        PlanHints(),
        cfg,
        lambda root: GitProbe(Git(), ceiling=lay.home.parent).info(root, cfg),
        Busy(),
        (),
        options,
        1000,
        redact=fake_redact,
    )


def _real(p: Path) -> str:
    return os.path.realpath(p)


# ---------------------------------------------------------------------------------------------------------
# R2-M3: a cwd whose content git ignores except placeholders
# ---------------------------------------------------------------------------------------------------------


def _scratch_repo(lay, ignore: str = "tmp/*\n!tmp/.gitkeep\n", keep: str = "tmp/.gitkeep") -> RepoBuilder:
    root = lay.home / "work" / "r36b"
    repo = RepoBuilder.init(root)
    repo.write(".gitignore", ignore)
    repo.write(keep, "")
    repo.write("src/a.py", "print('a')\n" * 20)
    repo.commit_all("project with a scratch folder")
    return repo


def test_r2m3_cwd_with_only_a_tracked_placeholder_travels_as_its_own_tree(transcript, lay):
    """★ The reviewer's case (``probe-r36b.txt``): ``tmp/*`` + ``!tmp/.gitkeep``, cwd ``tmp``, the agent wrote
    ``tmp/notes.md`` and ``tmp/experiment.py``. Before the fix: the primary was the repository (``GIT_REPO``, reasons
    ``cwd: tmp``, ``cwd holds tracked files``) and the two files did not travel."""
    repo = _scratch_repo(lay)
    cwd = repo.root / "tmp"
    write(cwd / "notes.md", "the session's notes\n")
    write(cwd / "experiment.py", "print(1)\n")
    t = transcript(cwd)
    t.tool("write_file", {"path": "experiment.py", "content": "x"}, {"resolved_path": str(cwd / "experiment.py")})
    cands = _collect(t, lay)
    primary = cands[0]
    assert primary.origin is Origin.CWD
    assert primary.path == _real(cwd), [(c.kind, c.path, c.reasons) for c in cands]
    assert primary.kind is ItemKind.FILE_TREE
    assert collector.CONTENT_IGNORED_REASON in primary.reasons
    assert _real(repo.root) not in {c.path for c in cands}


def test_r2m3_plan_sends_the_session_files_without_a_question(transcript, lay):
    """The plan's workspace is the folder (nothing differs, so no reason (j)), and its size counts the agent's files —
    the 25 B of the reviewer's first message are now what travels."""
    repo = _scratch_repo(lay)
    cwd = repo.root / "tmp"
    files = [write(cwd / "notes.md", "the session's notes\n"), write(cwd / "experiment.py", "print(1)\n")]
    t = transcript(cwd)
    rp = _plan(_collect(t, lay), lay)
    final = final_plan(rp)
    ws = plan.workspace(final)
    assert ws is not None and ws["path"] == _real(cwd) and ws["differs"] is False
    assert ws["bytes"] >= sum(f.stat().st_size for f in files)
    assert plan.CONFIRM_WORKSPACE_DIFFERS not in plan.confirmation_reasons(final, Options(yes=True), False)


def test_r2m3_gitignore_inside_the_folder_counts_as_a_placeholder(transcript, lay):
    """The other common idiom: ``tmp/.gitignore`` with ``*`` and ``!.gitignore``."""
    repo = _scratch_repo(lay, ignore="", keep="tmp/.gitignore")
    (repo.root / "tmp" / ".gitignore").write_text("*\n!.gitignore\n", encoding="utf-8")
    repo.commit_all("scratch rule")
    cwd = repo.root / "tmp"
    write(cwd / "out.log", "log\n")
    cands = _collect(transcript(cwd), lay)
    assert cands[0].path == _real(cwd)
    assert collector.CONTENT_IGNORED_REASON in cands[0].reasons


def test_r2m3_ordinary_cases_keep_their_workspace(transcript, lay):
    """No change where the cwd's content travels with the repository: a tracked source folder (R36a, no question), a
    placeholder-only folder without other entries, a folder with an untracked file git does not ignore."""
    repo = _scratch_repo(lay)
    src = transcript(repo.root / "src")
    cands = _collect(src, lay)
    assert cands[0].path == _real(repo.root) and cands[0].kind is ItemKind.GIT_REPO
    assert collector.cwd_tracked(cands[0].reasons) is True
    empty = _collect(transcript(repo.root / "tmp"), lay)
    assert empty[0].path == _real(repo.root), "only the placeholder: the repository stays the workspace"
    write(repo.root / "src" / "new.py", "x = 1\n")
    assert _collect(transcript(repo.root / "src"), lay)[0].path == _real(repo.root)


def test_r2m3_a_probe_without_the_question_keeps_the_old_rule(transcript, lay):
    """A probe that cannot answer (``StaticGitProbe`` without ``carried_files``, an error) changes nothing — the
    repository stays the workspace and R36a's reasons decide as before."""
    repo = _scratch_repo(lay)
    cwd = repo.root / "tmp"
    write(cwd / "notes.md", "x\n")
    probe = StaticGitProbe({_real(repo.root): False}, tracked=[_real(cwd)])
    cands = _collect(transcript(cwd), lay, git=probe)
    assert cands[0].path == _real(repo.root)

    class Broken(CeilingGitProbe):
        def carried_files(self, path, limit):
            raise OSError("git died")

    cands = _collect(transcript(cwd), lay, git=Broken(lay.home.parent))
    assert cands[0].path == _real(repo.root)


# ---------------------------------------------------------------------------------------------------------
# R2-M2: an untracked clone in the workspace the transcript never touches
# ---------------------------------------------------------------------------------------------------------


def _outer_with_clone(lay, *, ignored: bool = False) -> tuple[RepoBuilder, Path]:
    root = lay.home / "work" / "outer"
    repo = RepoBuilder.init(root)
    repo.write(".gitignore", "deps/\n" if ignored else "")
    repo.write("README.md", "outer\n")
    repo.commit_all("outer")
    clone_root = root / ("deps" if ignored else "vendor") / "x"
    clone = RepoBuilder.init(clone_root)
    clone.write("lib.py", "x = 1\n")
    clone.commit_all("vendored")
    clone.write("lib.py", "x = 2  # local change\n")
    return repo, clone_root


def test_r2m2_untouched_clone_in_the_workspace_is_named_and_asked(transcript, lay):
    """★ The reviewer's S6b: ``outer`` holds the untracked, not ignored clone ``vendor/x`` with a local change; the
    transcript never touches it. Before the fix: no candidate, ``repos_left`` empty, no reason (k) — the pack step left
    the clone out and only the note said so afterwards."""
    repo, clone = _outer_with_clone(lay)
    t = transcript(repo.root)
    t.tool("read_file", {"path": str(repo.root / "README.md")})
    cands = _collect(t, lay)
    inner = [c for c in cands if collector.INNER_REPO_REASON in c.reasons]
    assert [c.path for c in inner] == [_real(clone)], [(c.kind, c.path, c.reasons) for c in cands]
    final = final_plan(_plan(cands, lay))
    assert plan.repos_left(final) == [{"path": _real(clone), "kind": "nested"}]
    assert plan.CONFIRM_REPOS_LEFT in plan.confirmation_reasons(final, Options(yes=True), False)
    assert not [i for i in final.plan.items if i.source_path == _real(clone) and i.include is not Include.NO]


def test_r2m2_ignored_clone_is_not_named(transcript, lay):
    """A clone git ignores never travels with the workspace and is no part of it — not named (like any ignored file)."""
    repo, _clone = _outer_with_clone(lay, ignored=True)
    cands = _collect(transcript(repo.root), lay)
    assert not [c for c in cands if collector.INNER_REPO_REASON in c.reasons]


def test_r2m2_clone_with_evidence_is_named_once(transcript, lay):
    """The clone the agent wrote into (the case H15 already covered) stays one candidate with its evidence."""
    repo, clone = _outer_with_clone(lay)
    t = transcript(repo.root)
    t.tool("write_file", {"path": str(clone / "lib.py"), "content": "x"}, {"resolved_path": str(clone / "lib.py")})
    cands = _collect(t, lay)
    inner = [c for c in cands if collector.INNER_REPO_REASON in c.reasons]
    assert len(inner) == 1 and inner[0].path == _real(clone)
    assert any(r.startswith("write") for r in inner[0].reasons), inner[0].reasons


# ---------------------------------------------------------------------------------------------------------
# L3-F26: a mention in an ignored folder of another repository
# ---------------------------------------------------------------------------------------------------------


def _other_repo(lay) -> RepoBuilder:
    root = lay.home / "work" / "other"
    repo = RepoBuilder.init(root)
    repo.write(".gitignore", "build/\n.cache/\n")
    repo.write("README.md", "other project\n")
    repo.write("src/app.py", "print('x')\n" * 30)
    repo.commit_all("other")
    write(root / "build" / "out.log", "build output\n")
    write(root / ".cache" / "token.txt", "cached\n")
    return repo


def test_f26_mention_in_an_ignored_folder_is_the_file_not_the_repository(transcript, lay):
    """★ L3-F26: the session (its own project as cwd) mentions ``other/build/out.log`` and ``other/.cache/token.txt``;
    both folders are ignored in ``other``. Before the fix ``other`` became a secondary ``GIT_REPO`` candidate (with
    its untracked files) — ``--yes`` confirms unsuspicious proposals (R25)."""
    other = _other_repo(lay)
    t = transcript(lay.project)
    t.terminal(f"tail {other.root / 'build' / 'out.log'}")
    t.terminal(f"cat {other.root / '.cache' / 'token.txt'}")
    cands = _collect(t, lay)
    by_path = {c.path: c for c in cands}
    assert _real(other.root) not in by_path, [(c.kind, c.path) for c in cands]
    assert by_path[_real(other.root / "build" / "out.log")].kind is ItemKind.EXTRA_FILE
    assert by_path[_real(other.root / ".cache" / "token.txt")].kind is ItemKind.EXTRA_FILE


def test_f26_ignored_mention_does_not_join_a_repository_found_otherwise(transcript, lay):
    """When the repository is a candidate through a file it carries, a mention in its ignored folder is still not part
    of it (git would not pack it): the file is a single extra. The carried file sorts first (``README.md`` before
    ``build/``), so the repository is already a candidate when the ignored mention comes."""
    other = _other_repo(lay)
    t = transcript(lay.project)
    t.tool("read_file", {"path": str(other.root / "README.md")})
    t.terminal(f"tail {other.root / 'build' / 'out.log'}")
    cands = _collect(t, lay)
    by_path = {c.path: c for c in cands}
    repo = by_path[_real(other.root)]
    assert repo.kind is ItemKind.GIT_REPO
    assert not any("build/out.log" in r for r in repo.reasons), repo.reasons
    assert by_path[_real(other.root / "build" / "out.log")].kind is ItemKind.EXTRA_FILE


def test_f26_mention_in_a_carried_folder_still_brings_the_repository(transcript, lay):
    """Unchanged: a mention of a file the repository carries makes the repository the candidate (DESIGN §8.2)."""
    other = _other_repo(lay)
    t = transcript(lay.project)
    t.terminal(f"cat {other.root / 'src' / 'app.py'}")
    by_path = {c.path: c for c in _collect(t, lay)}
    assert by_path[_real(other.root)].kind is ItemKind.GIT_REPO


# ---------------------------------------------------------------------------------------------------------
# H5 rest, source side: Windows homes behind WSL's drive mounts
# ---------------------------------------------------------------------------------------------------------


def test_wsl_homes_are_home_like_per_user_folder(lay):
    """★ ``HomeRule`` remembers the policy's home list per prefix; the prefix must reach the user folder of a WSL home
    (``mnt/c/Users/<x>``, four components) — otherwise the first user's list answers for every other user of the
    drive. Before the fix ``/mnt/c/Users/bob`` was not home-like."""
    rule = collector.HomeRule.for_host(str(lay.home), lay.cfg())
    assert rule("/mnt/c/Users/alice") is True
    assert rule("/mnt/c/Users/alice/projects/app") is False
    assert rule("/mnt/c/Users/bob") is True
    assert rule("/mnt/c/Users") is True
    assert rule("/mnt/c/work/app") is False
    assert rule("/mnt/data/Users/x") is False
