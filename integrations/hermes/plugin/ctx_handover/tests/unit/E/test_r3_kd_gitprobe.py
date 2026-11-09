"""r3-kd wish for cluster E: two read-only questions of ``gitops.GitProbe`` the collector asks (optional members — a
probe without them keeps the old rules).

* ``carried_files(path, limit)`` — what git carries from a folder (tracked, or untracked and not ignored): the
  collector's placeholder rule (R36, review R2-M3: ``tmp/*`` with ``!tmp/.gitkeep`` made the whole repository the
  workspace and left the session's files behind).
* ``nested_repos(root)`` — the untracked clones inside a work tree, the list the pack step leaves out
  (``gitops.nested_repositories``): every one is named and asked about (H15 reason (k), review R2-M2), also one the
  transcript never touched.

Repositories from ``tests/fakes/repo_builder.py`` (technical identity, no git config written); the probe has its ceiling
at the test's tmp directory.
"""

from __future__ import annotations

import os
from pathlib import Path

from ctx_handover.engine import collector, plan
from ctx_handover.engine.gitops import Git, GitProbe
from ctx_handover.engine.model import HermesPaths, Options, PlanHints, RefineReport
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.fakes.planning_fake import Busy, fake_redact, final_plan, static_discover
from ctx_handover.tests.fakes.repo_builder import RepoBuilder


def _probe(tmp_path: Path) -> GitProbe:
    return GitProbe(Git(), ceiling=tmp_path)


def _write(path: Path, text: str = "x\n") -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    return path


def test_carried_files_answers_like_git_add(tmp_path):
    repo = RepoBuilder.init(tmp_path / "r")
    repo.write(".gitignore", "tmp/*\n!tmp/.gitkeep\nbuild/\n")
    repo.write("tmp/.gitkeep", "")
    repo.write("src/a.py", "a\n")
    repo.commit_all("seed")
    _write(repo.root / "tmp" / "notes.md")
    _write(repo.root / "src" / "new.py")
    _write(repo.root / "build" / "out.o")
    probe = _probe(tmp_path)
    assert probe.carried_files(str(repo.root / "tmp"), 64) == (".gitkeep",)
    assert set(probe.carried_files(str(repo.root / "src"), 64)) == {"a.py", "new.py"}
    assert probe.carried_files(str(repo.root / "build"), 64) == ()
    assert len(probe.carried_files(str(repo.root), 2)) == 2


def test_carried_files_is_unknown_outside_a_work_tree_and_for_a_file(tmp_path):
    plain = tmp_path / "plain"
    _write(plain / "f.txt")
    probe = _probe(tmp_path)
    assert probe.carried_files(str(plain), 64) is None
    assert probe.carried_files(str(plain / "f.txt"), 64) is None


def test_nested_repos_lists_untracked_clones_only(tmp_path):
    outer = RepoBuilder.init(tmp_path / "outer")
    outer.write(".gitignore", "deps/\n")
    outer.write("README.md", "o\n")
    outer.commit_all("outer")
    for rel in ("vendor/x", "deps/y"):
        inner = RepoBuilder.init(outer.root / rel)
        inner.write("f", "1\n")
        inner.commit_all("inner")
    probe = _probe(tmp_path)
    assert probe.nested_repos(str(outer.root)) == (os.path.join(str(outer.root), "vendor/x"),)
    assert probe.nested_repos(str(tmp_path / "missing")) is None


def test_collector_with_the_production_probe(tmp_path, monkeypatch):
    """R2-M3 and R2-M2 end to end through ``gitops.GitProbe``: the placeholder folder travels as its own tree; an
    untouched clone is named and asked about (k)."""
    home = tmp_path / "home"
    hermes = home / ".hermes"
    hermes.mkdir(parents=True)
    cfg = PolicyConfig.from_options(
        Options(),
        home=home,
        hermes_home=hermes,
        hermes_root=hermes,
        plugin_dir=hermes / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
    )
    hp = HermesPaths(home=str(hermes), document_cache=str(hermes / "cache" / "documents"))
    repo = RepoBuilder.init(home / "r36b")
    repo.write(".gitignore", "tmp/*\n!tmp/.gitkeep\n")
    repo.write("tmp/.gitkeep", "")
    repo.write("src/a.py", "a\n")
    repo.commit_all("seed")
    _write(repo.root / "tmp" / "notes.md")
    clone = RepoBuilder.init(repo.root / "vendor" / "x")
    clone.write("lib.py", "1\n")
    clone.commit_all("clone")
    probe = _probe(tmp_path)

    def run(cwd: Path):
        return collector.collect(
            [], {"id": "s1", "cwd": str(cwd)}, hp, cfg, probe, source_os="linux", discover=static_discover([])
        )

    scratch = run(repo.root / "tmp")
    assert scratch[0].path == os.path.realpath(repo.root / "tmp")
    assert collector.CONTENT_IGNORED_REASON in scratch[0].reasons
    whole = run(repo.root)
    inner = [c.path for c in whole if collector.INNER_REPO_REASON in c.reasons]
    assert inner == [os.path.realpath(repo.root / "vendor" / "x")]
    rp = plan.build_plan(
        "01JCJOB", (1,), whole, None, RefineReport(used="off"), PlanHints(), cfg,
        lambda root: probe.info(root, cfg), Busy(), (), Options(), 10, redact=fake_redact,
    )
    final = final_plan(rp)
    assert plan.repos_left(final) == [{"path": inner[0], "kind": "nested"}]
    assert plan.CONFIRM_REPOS_LEFT in plan.confirmation_reasons(final, Options(yes=True), False)
