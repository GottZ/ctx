"""R36 / L3-F19 (package i-r36): a session whose working folder lies in a git-ignored folder of an enclosing
repository.

The lab case (``reports/L3-findings.md`` L3-F19): the session's cwd sat in a folder the enclosing checkout ignores; the
collector took that checkout as the workspace (102 MiB of somebody else's work tree, untracked files included) and the
cwd's own files never arrived (git does not pack ignored files); ``--yes`` went through without a question. R36: the
enclosing repository is the workspace only when git does not ignore the cwd there; otherwise the cwd itself travels as a
file tree. Whatever the collector chose, the summary before sending names the workspace and its size against the cwd,
and a workspace that differs from the cwd needs a confirmation ``--yes`` does not give (reason (j)
``workspace_differs``).

Repositories come from ``tests/fakes/repo_builder.py`` (technical identity, no git config written); the probe is the
production one (``gitops.GitProbe``) with a ceiling at the test's tmp directory. HOME lies outside the repository
(the repository is a folder of HOME, as for a real user): the ``home_like`` brake that hid the case in the lab's
``in_process`` runs does not apply.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

from ctx_handover.engine import collector, plan
from ctx_handover.engine.gitops import Git, GitProbe
from ctx_handover.engine.jsonutil import from_wire, to_wire
from ctx_handover.engine.model import (
    FinalPlan,
    HermesPaths,
    Include,
    ItemKind,
    ItemRole,
    Options,
    Origin,
    PlanHints,
    RefineReport,
    Weight,
)
from ctx_handover.tests.fakes.planning_fake import Busy, StaticGitProbe, fake_redact, final_plan, static_discover
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.D.conftest import write


@dataclass
class Foreign:
    root: str
    """Realpath of the enclosing repository (somebody else's checkout under HOME)."""
    cwd: str
    """Realpath of the session's working folder: ``<root>/scratch/session``, ignored by ``scratch/``."""
    session_bytes: int
    """Bytes of the files in ``cwd``."""
    src_bytes: int
    """Bytes of the files in ``<root>/src`` (a folder git does not ignore)."""
    builder: RepoBuilder


def _foreign(lay) -> Foreign:
    root = lay.home / "work" / "foreign"
    repo = RepoBuilder.init(root)
    repo.write(".gitignore", "scratch/\n")
    repo.write("README.md", "somebody else's project\n")
    repo.write("src/app.py", "print('foreign')\n" * 40)
    repo.commit_all("foreign project")
    repo.write("untracked-notes.md", "versioned nowhere, ignored nowhere\n")
    cwd = root / "scratch" / "session"
    files = [write(cwd / "work.txt", "the session's own file\n"), write(cwd / "data" / "result.csv", "a,b\n1,2\n")]
    assert not str(lay.home.resolve()).startswith(str(root.resolve()))  # HOME outside the repository (lab: inside)
    return Foreign(
        root=os.path.realpath(root),
        cwd=os.path.realpath(cwd),
        session_bytes=sum(f.stat().st_size for f in files),
        src_bytes=(root / "src" / "app.py").stat().st_size,
        builder=repo,
    )


def _hermes(lay) -> HermesPaths:
    return HermesPaths(home=str(lay.hermes_home), document_cache=str(lay.hermes_home / "cache" / "documents"))


def _probe(lay) -> GitProbe:
    return GitProbe(Git(), ceiling=lay.home.parent)


def _collect(t, lay, git=None):
    return collector.collect(
        t.rows(),
        t.session_row(),
        _hermes(lay),
        lay.cfg(),
        git or _probe(lay),
        source_os="linux",
        discover=static_discover([]),
    )


def _plan(cands, lay, *, hints: PlanHints | None = None):
    options = Options()
    return plan.build_plan(
        "01JCJOB",
        (1,),
        cands,
        None,
        RefineReport(used="off"),
        hints or PlanHints(),
        lay.cfg(options=options),
        lambda root: _probe(lay).info(root, lay.cfg(options=options)),
        Busy(),
        (),
        options,
        1000,
        redact=fake_redact,
    )


def _primary(items):
    return next(i for i in items if i.origin is Origin.CWD)


# ---------------------------------------------------------------------------------------------------------
# (a) the collector: the ignored cwd is the workspace, not the enclosing repository
# ---------------------------------------------------------------------------------------------------------


def test_r36_collector_takes_the_ignored_cwd_not_the_enclosing_repository(transcript, lay):
    """★ L3-F19 / R36: the session's cwd is ignored in the enclosing repository (``scratch/``) — the primary candidate
    is the cwd itself as a file tree (with a reason saying why), the enclosing repository is no candidate at all.

    Red on ``ae0749a5``: the primary was the enclosing repository (``GIT_REPO`` ``<root>``, reason
    ``cwd: scratch/session``) — ``assert '<root>' == '<root>/scratch/session'``.

    Mutants: the collector skips the ``ignored`` check; ``_Classifier.ignored`` answers ``False`` for a failing probe.
    """
    f = _foreign(lay)
    t = transcript(f.cwd)
    t.tool("write_file", {"path": "work.txt", "content": "x"}, {"resolved_path": os.path.join(f.cwd, "work.txt")})
    cands = _collect(t, lay)
    primary = cands[0]
    assert primary.origin is Origin.CWD
    assert primary.path == f.cwd
    assert primary.kind is ItemKind.FILE_TREE
    assert collector.IGNORED_REASON in primary.reasons
    assert collector.rel_cwd(primary.reasons) is None
    assert f.root not in {c.path for c in cands}, [(c.kind, c.path) for c in cands]


def test_r36_evidence_in_the_enclosing_repository_does_not_bring_it_back(transcript, lay):
    """★ R36: the transcript reads files of the enclosing repository outside the cwd (tracked ``README.md`` and
    ``src/app.py``, the untracked ``untracked-notes.md``) and mentions a sibling in the ignored folder — that repository
    is no secondary ``GIT_REPO`` either (otherwise ``--yes``, which confirms proposals, would send the whole checkout
    again); the files themselves are single extra candidates.

    Red on ``ae0749a5``: the enclosing repository was the primary candidate (``<root> in {…}``).

    Mutant: drop the evidence guard (``enclosing``) in ``collect`` — the repository comes back as a secondary
    ``GIT_REPO``."""
    f = _foreign(lay)
    write(Path(f.root) / "scratch" / "other.txt", "a sibling in the ignored folder\n")
    t = transcript(f.cwd)
    t.tool("read_file", {"path": os.path.join(f.root, "README.md")})
    t.tool("read_file", {"path": os.path.join(f.root, "src", "app.py")})
    t.tool("read_file", {"path": os.path.join(f.root, "untracked-notes.md")})
    t.terminal(f"cat {os.path.join(f.root, 'scratch', 'other.txt')}")
    cands = _collect(t, lay)
    by_path = {c.path: c for c in cands}
    assert f.root not in by_path, [(c.kind, c.path) for c in cands]
    assert not [c for c in cands if c.kind is ItemKind.GIT_REPO], [(c.kind, c.path) for c in cands]
    assert by_path[os.path.join(f.root, "README.md")].kind is ItemKind.EXTRA_FILE
    assert by_path[os.path.join(f.root, "README.md")].weight is Weight.READ
    assert cands[0].path == f.cwd


def test_r36_plan_carries_the_cwd_files_and_no_foreign_checkout(transcript, lay):
    """★ L3-F19 / R36: the plan's primary item is the cwd as a file tree with the size of the session's own files; no
    item is the enclosing checkout; the summary before sending names the workspace (= the cwd, ``differs`` False) and
    no reason (j) is raised — the session goes with ``--yes`` as before, but with its own files.

    Red on ``ae0749a5``: the primary item was the enclosing repository (``GIT_REPO``) — the lab's 102 MiB checkout
    without the session's files."""
    f = _foreign(lay)
    cands = _collect(transcript(f.cwd), lay)
    rp = _plan(cands, lay)
    primary = _primary(rp.items)
    assert primary.role is ItemRole.PRIMARY and primary.include is Include.YES
    assert (primary.kind, primary.source_path, primary.rel_cwd) == (ItemKind.FILE_TREE, f.cwd, None)
    assert primary.size_estimate == f.session_bytes
    assert f.root not in {i.source_path for i in rp.items}
    final = final_plan(rp)
    assert plan.presend_summary(final)["workspace"] == {
        "path": f.cwd,
        "bytes": f.session_bytes,
        "cwd": f.cwd,
        "cwd_bytes": f.session_bytes,
        "differs": False,
    }
    reasons = plan.confirmation_reasons(final, Options(yes=True), False)
    assert plan.CONFIRM_WORKSPACE_DIFFERS not in reasons, reasons


# ---------------------------------------------------------------------------------------------------------
# what stays: a cwd git does not ignore keeps its repository
# ---------------------------------------------------------------------------------------------------------


def test_r36_a_cwd_git_does_not_ignore_keeps_its_repository(transcript, lay):
    """R36 (control): a cwd in a folder git does not ignore (``src``) and a cwd in an ignored folder that holds a
    tracked file (force-added: versioned, git does not report it as ignored) keep the enclosing repository as the
    workspace, with the cwd relative to it — the behaviour before R36."""
    f = _foreign(lay)
    f.builder.write("scratch/kept/keep.txt", "force-added\n")
    f.builder.run("add", "-f", "--", "scratch/kept/keep.txt")
    f.builder.commit("keep one scratch file")
    for rel in ("src", "scratch/kept"):
        cands = _collect(transcript(os.path.join(f.root, rel)), lay)
        primary = cands[0]
        assert (primary.origin, primary.kind, primary.path) == (Origin.CWD, ItemKind.GIT_REPO, f.root), rel
        assert collector.rel_cwd(primary.reasons) == rel
        assert collector.IGNORED_REASON not in primary.reasons


class _Unclear(StaticGitProbe):
    """A probe whose ``is_ignored`` cannot answer (raises, or answers with something that is not ``False``)."""

    def __init__(self, roots, answer):
        super().__init__(roots)
        self.answer = answer

    def is_ignored(self, path: str) -> bool:
        self.calls.append(("is_ignored", path))
        if isinstance(self.answer, BaseException):
            raise self.answer
        return self.answer


def test_r36_an_unclear_ignore_answer_keeps_the_workspace_to_the_cwd(transcript, lay):
    """★ R36, fail closed: when the probe cannot say that the cwd is not ignored (an exception, ``None``, a truthy
    non-bool), the workspace stays the cwd — it never grows to a repository the session's folder may not be part of.
    Only an explicit ``False`` keeps the repository. The probe is asked only for a cwd below the repository's root.

    Red on ``ae0749a5``: the primary was the repository whatever the probe said (no ``is_ignored`` question).

    Mutant: ``_Classifier.ignored`` returns ``bool(answer)`` / ``False`` on an exception."""
    repo = lay.home / "repo"
    cwd = repo / "sub"
    write(cwd / "f.txt")
    roots = {os.path.realpath(repo): False}
    for answer in (OSError("git failed"), None, "yes", 1):
        cands = _collect(transcript(cwd), lay, git=_Unclear(roots, answer))
        assert (cands[0].kind, cands[0].path) == (ItemKind.FILE_TREE, os.path.realpath(cwd)), answer
    probe = _Unclear(roots, False)
    cands = _collect(transcript(cwd), lay, git=probe)
    assert (cands[0].kind, cands[0].path) == (ItemKind.GIT_REPO, os.path.realpath(repo))
    assert ("is_ignored", os.path.realpath(cwd)) in probe.calls
    top = _Unclear(roots, OSError("never asked"))
    cands = _collect(transcript(repo), lay, git=top)
    assert (cands[0].kind, cands[0].path) == (ItemKind.GIT_REPO, os.path.realpath(repo))
    assert not [c for c in top.calls if c[0] == "is_ignored"], top.calls


# ---------------------------------------------------------------------------------------------------------
# (b) the summary before sending names the workspace against the cwd; (j) is not confirmed by --yes
# ---------------------------------------------------------------------------------------------------------


def test_r36_summary_names_a_workspace_that_differs_from_the_cwd_and_yes_does_not_confirm_it(transcript, lay):
    """★ R36: the cwd is a folder git does not ignore (``src``), so the workspace is the whole repository. The summary
    before sending names the repository and its size against the cwd and the cwd's size (measured on the source and
    carried in the plan item, ``PlanItem.cwd_bytes`` — the conductor of a pull has only the wire plan); the
    confirmation reasons hold ``workspace_differs`` (letter j), which ``--yes`` does not confirm (``YES_EXEMPT``).
    ``--no-workspace`` (the folder does not travel) names no workspace and raises no (j).

    Red on ``ae0749a5``: ``KeyError: 'workspace'`` — the summary named neither the workspace nor the cwd, and
    ``--yes`` confirmed every reason of such a plan.

    Mutants: ``workspace`` reports ``differs`` False; ``confirmation_reasons`` omits (j); ``YES_EXEMPT`` without
    ``workspace_differs``; ``build_plan`` does not measure ``cwd_bytes``."""
    f = _foreign(lay)
    rp = _plan(_collect(transcript(os.path.join(f.root, "src")), lay), lay)
    primary = _primary(rp.items)
    assert (primary.kind, primary.source_path, primary.rel_cwd) == (ItemKind.GIT_REPO, f.root, "src")
    assert primary.cwd_bytes == f.src_bytes
    final = final_plan(rp)
    expected = {
        "path": f.root,
        "bytes": primary.size_estimate,
        "cwd": os.path.join(f.root, "src"),
        "cwd_bytes": f.src_bytes,
        "differs": True,
    }
    assert plan.presend_summary(final)["workspace"] == expected
    assert "workspace" in plan.PRESEND_KEYS
    reasons = plan.confirmation_reasons(final, Options(yes=True), False)
    assert plan.CONFIRM_WORKSPACE_DIFFERS in reasons, reasons
    assert plan.CONFIRM_WORKSPACE_DIFFERS in plan.yes_exempt(reasons)
    assert plan.CONFIRM_LETTERS[plan.CONFIRM_WORKSPACE_DIFFERS] == "j"
    assert plan.REASON_CODES[-1] == plan.CONFIRM_WORKSPACE_DIFFERS
    assert plan.CONFIRM_WORKSPACE_DIFFERS in plan.YES_EXEMPT

    # what the conductor of a pull sees: the plan after the wire
    received = from_wire(FinalPlan, to_wire(final))
    assert plan.presend_summary(received)["workspace"] == expected

    cands = _collect(transcript(os.path.join(f.root, "src")), lay)
    bare = final_plan(_plan(cands, lay, hints=PlanHints(no_workspace=True)))
    assert plan.presend_summary(bare)["workspace"] is None
    assert plan.CONFIRM_WORKSPACE_DIFFERS not in plan.confirmation_reasons(bare, Options(yes=True), False)
