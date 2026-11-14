"""First review round (the first external review): what the collector found but the plan left behind
without a word.

* H14 — a session cwd that is itself a symlink (Desktop/TUI store the cwd lexically) was "no cwd": no workspace, no
  question, R30 never fired.
* H15 — initialised submodules (and repositories cloned inside the workspace) never travel; their files and the
  agent's changes in them stayed behind while the plan claimed them as covered (``write: lib/src/app.py``).
* M35 — the home rule knew one ``$HOME`` only: another account's home or ``/home`` became a whole workspace,
  another account's Hermes home with it.
* M36 — a merely mentioned folder around a secondary repository became an ``extra_dir`` of its own (the repository
  twice, its unrelated neighbours with it).
* M20 (D part) — evidence in the Hermes home that the session-file discovery does not deliver was dropped silently.

Repositories come from ``tests/fakes/repo_builder.py`` (technical identity, no git config written); the probe is the
production ``gitops.GitProbe`` with a ceiling at the test's tmp directory.
"""

from __future__ import annotations

import os
from dataclasses import replace
from pathlib import Path

from ctx_handover.engine import collector, note, plan, policy
from ctx_handover.engine.gitops import Git, GitProbe
from ctx_handover.engine.model import HermesPaths, Include, ItemKind, ItemRole, Options, Origin, PlanHints, RefineReport
from ctx_handover.tests.fakes.planning_fake import Busy, HermesFileStub, fake_redact, final_plan, static_discover
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.D.conftest import write


def _hermes(lay) -> HermesPaths:
    return HermesPaths(home=str(lay.hermes_home), document_cache=str(lay.hermes_home / "cache" / "documents"))


def _probe(lay) -> GitProbe:
    return GitProbe(Git(), ceiling=lay.home.parent)


def _collect(t, lay, *, cfg=None, discover=None):
    return collector.collect(
        t.rows(),
        t.session_row(),
        _hermes(lay),
        cfg or lay.cfg(),
        _probe(lay),
        source_os="linux",
        discover=discover or static_discover([]),
    )


def _plan(cands, lay, *, cfg=None, hints: PlanHints | None = None):
    options = Options()
    cfg = cfg or lay.cfg(options=options)
    return plan.build_plan(
        "01JCJOB",
        (1,),
        cands,
        None,
        RefineReport(used="off"),
        hints or PlanHints(),
        cfg,
        lambda root: _probe(lay).info(root, cfg),
        Busy(),
        (),
        options,
        1000,
        redact=fake_redact,
    )


def _primary_cand(cands):
    return next((c for c in cands if c.origin is Origin.CWD), None)


def _primary_item(rp):
    return next((i for i in rp.items if i.role is ItemRole.PRIMARY), None)


def _nt(rp) -> dict[str, str]:
    return {n.path: n.why for n in rp.not_transferred}


def _repo(root: Path, files: dict[str, str]) -> RepoBuilder:
    repo = RepoBuilder.init(root)
    for rel, text in files.items():
        repo.write(rel, text)
    repo.commit_all("initial")
    return repo


# ---------------------------------------------------------------------------------------------------------
# H14 — the cwd is a symlink
# ---------------------------------------------------------------------------------------------------------


def test_h14_symlinked_cwd_is_the_primary_workspace(transcript, lay, tmp_path):
    """★ H14 (g8 S1a/S1b): the session's cwd is a link to a repository — outside ``$HOME`` (``~/proj-link ->
    /disk/proj``) and below it (``~/proj2-link -> ~/real/proj2``). The repository behind the link is the primary
    workspace (role ``primary``, ``yes``), named in the summary before sending; it is no secondary proposal.

    Red on ``2d35ba93``: no primary candidate at all — ``collector.collect`` checked ``is_dir`` with ``lstat`` on the
    link (``assert None is not None``); for S1b the repository came back as a secondary proposal.

    Mutant: ``collect`` checks the lexical cwd with ``is_dir`` again (``cls.is_dir(cwd)``)."""
    outside = tmp_path / "disk" / "proj"
    _repo(outside, {"README.md": "outside the home\n", "src/app.py": "print(1)\n"})
    inside = lay.home / "real" / "proj2"
    _repo(inside, {"README.md": "inside the home\n"})
    for real, link in ((outside, lay.home / "proj-link"), (inside, lay.home / "proj2-link")):
        link.symlink_to(real, target_is_directory=True)
        t = transcript(link)
        t.tool("write_file", {"path": "README.md", "content": "x"}, {"resolved_path": str(link / "README.md")})
        cands = _collect(t, lay)
        primary = _primary_cand(cands)
        assert primary is not None, [(c.kind, c.path, c.origin) for c in cands]
        assert (primary.kind, primary.path) == (ItemKind.GIT_REPO, os.path.realpath(real))
        assert "write: README.md" in primary.reasons
        rp = _plan(cands, lay)
        item = _primary_item(rp)
        assert item is not None and item.include is Include.YES and item.source_path == os.path.realpath(real)
        assert [i for i in rp.items if i.role is ItemRole.SECONDARY] == []
        summary = plan.presend_summary(final_plan(rp))
        assert summary["workspace"]["path"] == os.path.realpath(real)
        assert summary["workspace_excluded"] is None


def test_h14_existing_cwd_without_a_workspace_is_named_and_asked(transcript, lay, tmp_path):
    """★ H14 / R30: a cwd that exists but yields no workspace — a link whose target is gone, a regular file — is the
    primary placeholder the plan leaves out and names (``missing`` / ``not a directory``); the summary says so
    (``workspace_excluded``) and reason (h) is raised, which ``--yes`` does not confirm.

    Red on ``2d35ba93``: no primary candidate, so ``workspace_excluded`` was None and no reason was raised
    (``assert None is not None``).

    Mutant: ``collect`` returns no placeholder for an existing cwd that is no directory."""
    dangling = lay.home / "gone-link"
    dangling.symlink_to(tmp_path / "removed-project", target_is_directory=True)
    a_file = write(lay.home / "notes.txt", "a file, not a folder\n")
    for cwd, why, code in ((dangling, "missing", "missing"), (a_file, plan.WHY_NOT_DIRECTORY, "other")):
        rp = _plan(_collect(transcript(cwd), lay), lay)
        final = final_plan(rp)
        excluded = plan.presend_summary(final)["workspace_excluded"]
        assert excluded is not None, rp.items
        assert excluded == {"path": str(cwd), "reason": code, "why": why}
        assert _nt(rp)[str(cwd)] == why
        reasons = plan.confirmation_reasons(final, Options(yes=True), False)
        assert plan.CONFIRM_WORKSPACE_EXCLUDED in plan.yes_exempt(reasons), reasons
        assert all(i.include is Include.NO for i in rp.items if i.role is ItemRole.PRIMARY)


# ---------------------------------------------------------------------------------------------------------
# H15 — submodules and repositories inside the workspace
# ---------------------------------------------------------------------------------------------------------


def _superproject(lay, tmp_path) -> tuple[Path, Path]:
    """``~/work/super`` with the initialised submodules ``lib`` (changed by the session) and ``docs`` (untouched),
    and an untracked clone ``vendor/x`` inside the work tree. Returns ``(super, a file the session wrote in lib)``."""
    lib_src = tmp_path / "upstream" / "lib"
    _repo(lib_src, {"src/app.py": "print('lib')\n"})
    docs_src = tmp_path / "upstream" / "docs"
    _repo(docs_src, {"index.md": "# docs\n"})
    root = lay.home / "work" / "super"
    sup = _repo(root, {"README.md": "superproject\n"})
    for src, rel in ((lib_src, "lib"), (docs_src, "docs")):
        sup.run("-c", "protocol.file.allow=always", "submodule", "add", "-q", "--", str(src), rel)
    sup.commit_all("add submodules")
    changed = write(root / "lib" / "src" / "app.py", "print('changed by the agent')\n")
    _repo(root / "vendor" / "x", {"lib.py": "x = 1\n"})
    assert (root / "lib" / ".git").is_file() and (root / "docs" / ".git").is_file()
    return root, changed


def test_h15_submodules_and_nested_clones_are_named_not_covered(transcript, lay, tmp_path):
    """★ H15 (g8 S3a/S6): a superproject with two initialised submodules — ``lib`` (the session changed
    ``lib/src/app.py``) and ``docs`` (untouched) — and an untracked clone ``vendor/x`` the session wrote in. Only the
    superproject travels (gitlinks); the plan no longer claims the session's writes there as covered by it, names each
    submodule and the clone under "not transferred", the summary before sending lists them (``repos_left``) and reason
    (k) ``repos_left`` is raised, which ``--yes`` does not confirm (R30 form).

    Red on ``2d35ba93``: the primary carried ``write: lib/src/app.py`` as if the superproject covered it
    (``assert 'write: lib/src/app.py' not in (…)``), nothing was named.

    Mutants: the collector's ``inner_repo`` answers None; ``report_inner_repos`` skips the gitlinks of ``GitInfo``;
    ``repos_left`` missing from ``YES_EXEMPT``."""
    root, _changed = _superproject(lay, tmp_path)
    t = transcript(root)
    t.tool("write_file", {"path": "lib/src/app.py", "content": "x"}, {"resolved_path": str(root / "lib/src/app.py")})
    t.tool("write_file", {"path": "vendor/x/lib.py", "content": "x"}, {"resolved_path": str(root / "vendor/x/lib.py")})
    t.tool("write_file", {"path": "README.md", "content": "x"}, {"resolved_path": str(root / "README.md")})
    cands = _collect(t, lay)
    primary = _primary_cand(cands)
    assert primary.path == os.path.realpath(root)
    assert "write: lib/src/app.py" not in primary.reasons, primary.reasons
    assert "write: vendor/x/lib.py" not in primary.reasons, primary.reasons
    assert "write: README.md" in primary.reasons

    rp = _plan(cands, lay)
    real = os.path.realpath(root)
    lib, docs, nested = (os.path.join(real, rel) for rel in ("lib", "docs", os.path.join("vendor", "x")))
    nt = _nt(rp)
    assert nt.get(lib) == plan.WHY_SUBMODULE and nt.get(docs) == plan.WHY_SUBMODULE, nt
    assert nt.get(nested) == plan.WHY_NESTED_REPO, nt
    assert {i.source_path for i in rp.items} == {real}, [(i.kind, i.source_path) for i in rp.items]
    final = final_plan(rp)
    summary = plan.presend_summary(final)
    assert summary["repos_left"] == [
        {"path": docs, "kind": "submodule"},
        {"path": lib, "kind": "submodule"},
        {"path": nested, "kind": "nested"},
    ]
    assert summary["repos_left_more"] == 0
    reasons = plan.confirmation_reasons(final, Options(yes=True), False)
    assert plan.CONFIRM_REPOS_LEFT in reasons and plan.CONFIRM_REPOS_LEFT in plan.yes_exempt(reasons), reasons
    assert plan.CONFIRM_LETTERS[plan.CONFIRM_REPOS_LEFT] == "k"
    assert plan.REASON_CODES[-1] == plan.CONFIRM_REPOS_LEFT
    # the note on the target shows both reasons verbatim (its vocabulary), not "not transferred"
    assert {plan.WHY_SUBMODULE, plan.WHY_NESTED_REPO, plan.WHY_NOT_DIRECTORY} <= note.KNOWN_WHY


def test_h15_nothing_left_behind_without_submodules_or_when_the_workspace_stays(transcript, lay, tmp_path):
    """H15 negative space: a plain repository raises no (k); a superproject whose workspace stays here
    (``--no-workspace``) leaves nothing behind *relative to the transfer* — no (k), and the submodules are not listed
    on their own; an uninitialised submodule (gitlink, empty folder) loses nothing.

    Mutant: ``repos_left`` ignores whether the repository around a submodule travels."""
    plain = lay.home / "work" / "plain"
    _repo(plain, {"a.txt": "a\n"})
    rp = _plan(_collect(transcript(plain), lay), lay)
    assert plan.CONFIRM_REPOS_LEFT not in plan.confirmation_reasons(final_plan(rp), Options(yes=True), False)
    assert plan.presend_summary(final_plan(rp))["repos_left"] == []

    root, _changed = _superproject(lay, tmp_path)
    travels = _plan(_collect(transcript(root), lay), lay)
    assert plan.repos_left(travels)
    # the superproject narrowed to ``no`` after planning (a pull's restriction, a declined proposal): nothing of it
    # travels, so nothing inside it is "left behind" on its own
    narrowed = replace(travels, items=tuple(replace(i, include=Include.NO) for i in travels.items))
    assert plan.repos_left(narrowed) == []
    stays = _plan(_collect(transcript(root), lay), lay, hints=PlanHints(no_workspace=True))
    assert plan.presend_summary(final_plan(stays))["repos_left"] == []
    assert plan.CONFIRM_REPOS_LEFT not in plan.confirmation_reasons(final_plan(stays), Options(yes=True), False)

    clone = tmp_path / "clone"
    RepoBuilder(tmp_path).run("-c", "protocol.file.allow=always", "clone", "-q", "--", str(root), str(clone))
    assert not (clone / "lib" / ".git").exists()
    fresh = _plan(_collect(transcript(clone), lay), lay)
    assert plan.presend_summary(final_plan(fresh))["repos_left"] == []


# ---------------------------------------------------------------------------------------------------------
# M35 — every account's home
# ---------------------------------------------------------------------------------------------------------


def _with_account(monkeypatch, home: Path) -> None:
    """Another account whose home is ``home``: the password database of the policy's home list (``policy.homes_of``,
    read by ``collector.HomeRule``) lists it — the test home cannot lie below the real ``/home``."""
    listed = policy.homes_of

    def homes_of(path, cfg, *, windows):
        return (*listed(path, cfg, windows=windows), home)

    monkeypatch.setattr(policy, "homes_of", homes_of)


def _other_account(tmp_path) -> Path:
    alice = tmp_path / "accounts" / "alice"
    write(alice / "proj" / "main.py", "print('alice')\n")
    write(alice / ".hermes" / "state.db", "SQLite format 3\x00 alice's sessions\n")
    write(alice / ".hermes" / "config.yaml", "model: alice\n")
    return alice


def test_m35_another_accounts_home_or_the_homes_folder_is_no_workspace(transcript, lay, tmp_path, monkeypatch):
    """★ M35: Hermes runs as one account (``cfg.home``), the session's cwd is another account's home or the
    folder holding the homes (``/home``). Neither becomes a workspace: the primary stays here as ``home`` and reason
    (h) asks even with ``--yes``; nothing of that home — its Hermes ``state.db`` least of all — is an item.

    Red on ``2d35ba93``: the other home was the whole workspace (``include yes``), ``state.db`` inside it.

    Mutant: ``collector.HomeRule`` ignores ``policy.homes_of``."""
    alice = _other_account(tmp_path)
    _with_account(monkeypatch, alice)
    for cwd in (alice, alice.parent):
        rp = _plan(_collect(transcript(cwd), lay), lay)
        final = final_plan(rp)
        excluded = plan.presend_summary(final)["workspace_excluded"]
        assert excluded is not None and excluded["reason"] == "home", (cwd, rp.items)
        assert [i for i in rp.items if i.include is not Include.NO] == []
        assert plan.CONFIRM_WORKSPACE_EXCLUDED in plan.confirmation_reasons(final, Options(yes=True), False)


def test_m35_another_accounts_hermes_home_is_never_an_item(transcript, lay, tmp_path, monkeypatch):
    """★ M35: the session works in a project of another account (``~alice/proj``, released through
    ``export_roots``) and reads that account's Hermes ``state.db`` and ``config.yaml``. Neither becomes an item, even
    under ``export_roots``; both are named as Hermes paths left behind.

    Red on ``2d35ba93``: both files were extra items under ``export_roots``
    (``assert '…/.hermes/state.db' not in {…}``).

    Mutant: ``HomeRule.foreign_hermes`` answers False."""
    alice = _other_account(tmp_path)
    _with_account(monkeypatch, alice)
    cfg = lay.cfg(options=Options(export_roots=(str(alice.parent),)))
    t = transcript(alice / "proj")
    t.tool("read_file", {"path": str(alice / ".hermes" / "state.db")})
    t.tool("write_file", {"path": str(alice / ".hermes" / "config.yaml"), "content": "x"})
    rp = _plan(_collect(t, lay, cfg=cfg), lay, cfg=cfg)
    paths = {i.source_path for i in rp.items}
    for name in ("state.db", "config.yaml"):
        target = os.path.realpath(alice / ".hermes" / name)
        assert target not in paths, paths
        assert _nt(rp).get(target) == "hermes", rp.not_transferred
    assert _primary_item(rp).source_path == os.path.realpath(alice / "proj")


# ---------------------------------------------------------------------------------------------------------
# M36 — a mentioned folder around a secondary repository
# ---------------------------------------------------------------------------------------------------------


def test_m36_mentioned_folder_around_a_secondary_repository_is_no_extra(transcript, lay):
    """★ M36: ``ls ~/projects`` and ``read_file ~/projects/b/f`` — ``b`` is a repository, ``c`` an unrelated
    neighbour. The plan proposes the repository ``b``; ``~/projects`` is no extra directory (it contains a workspace of
    the plan) and is named as such; nothing of ``c`` travels.

    Red on ``2d35ba93``: ``~/projects`` was an ``extra_dir`` item (``assert '…/projects' not in {…}``).

    Mutant: ``_Builder.evaluate`` skips ``contains_tree`` for transcript directories."""
    projects = lay.home / "projects"
    _repo(projects / "b", {"f": "b's file\n"})
    write(projects / "c" / "unrelated.txt", "somebody else's notes\n")
    t = transcript(lay.project)
    t.terminal(f"ls {projects}")
    t.tool("read_file", {"path": str(projects / "b" / "f")})
    rp = _plan(_collect(t, lay), lay)
    paths = {i.source_path for i in rp.items}
    real = os.path.realpath(projects)
    assert real not in paths, [(i.kind, i.source_path) for i in rp.items]
    assert os.path.join(real, "b") in paths
    assert not [p for p in paths if p.startswith(os.path.join(real, "c"))]
    assert _nt(rp).get(real) == plan.WHY_CONTAINS_WORKSPACE


# ---------------------------------------------------------------------------------------------------------
# M20 (D part) — Hermes-home evidence is named, not dropped
# ---------------------------------------------------------------------------------------------------------


def test_m20_hermes_home_evidence_without_a_session_file_is_named(transcript, lay):
    """★ M20 (D part): the session read a page ``web_extract`` saved in the Hermes home
    (``cache/web/x.md``) and Hermes' ``config.yaml``; the session-file discovery delivers neither. Both are named
    under "not transferred" (``hermes``) instead of disappearing; a session file the discovery delivers is an item
    and is not named again; a folder of the Hermes home is no entry.

    Red on ``2d35ba93``: the collector dropped Hermes-home evidence (``assert None == 'hermes'``).

    Mutant: ``collect`` skips the ``hermes_left`` candidates."""
    page = write(lay.hermes_home / "cache" / "web" / "example.com-1a2b.md", "the full page\n")
    config = write(lay.hermes_home / "config.yaml", "model: x\n")
    spill = write(lay.hermes_home / "cache" / "spillover" / "call_1.txt", "spilled output\n")
    t = transcript(lay.project)
    t.tool("read_file", {"path": str(page)})
    t.terminal(f"cat {config}")
    t.tool("read_file", {"path": str(spill)})
    t.terminal(f"ls {lay.hermes_home / 'cache'}")
    stub = HermesFileStub("spillover", str(spill), "cache/spillover/call_1.txt", spill.stat().st_size)
    rp = _plan(_collect(t, lay, discover=static_discover([stub])), lay)
    nt = _nt(rp)
    assert nt.get(str(page)) == "hermes", rp.not_transferred
    assert nt.get(str(config)) == "hermes", rp.not_transferred
    assert str(spill) not in nt
    assert str(lay.hermes_home / "cache") not in nt
    assert str(spill) in {i.source_path for i in rp.items}
