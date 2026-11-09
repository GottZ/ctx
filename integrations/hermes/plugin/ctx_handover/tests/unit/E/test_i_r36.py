"""R36 / L3-F19 (package i-r36), cluster E part: ``gitops.GitProbe.is_ignored`` — is a directory inside a work tree
ignored there (``git check-ignore``)? The collector asks it before it takes an enclosing repository as the session's
workspace: a cwd git ignores in that repository travels as a file tree of its own.

Repositories come from ``tests/fakes/repo_builder.py``; the probe runs through the hardened runner (``Git.run``:
forced options of DESIGN §9.2, no inherited ``GIT_*``, the discovery ceiling) with ``--end-of-options``. Failures are
injected with ``FailingGit`` (a real git whose ``check-ignore`` dies)."""

from __future__ import annotations

from pathlib import Path

import pytest

from ctx_handover.engine import contracts, gitops
from ctx_handover.tests.fakes.git_fake import FakeGitProbe
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import FailingGit


def _repo(root: Path) -> RepoBuilder:
    r = RepoBuilder.init(root)
    r.write(".gitignore", "scratch/\nbuild/\n.*\n!.gitignore\n")
    r.write("src/app.py", "print(1)\n")
    r.write("build/keep/k.txt", "kept\n")
    r.run("add", "-f", "--", "build/keep/k.txt")
    r.commit_all("c1")
    for rel in ("scratch/session/data", "src/sub", "build/out", ".cache/w", "excluded/x"):
        (root / rel).mkdir(parents=True, exist_ok=True)
    (root / ".git" / "info").mkdir(parents=True, exist_ok=True)
    (root / ".git" / "info" / "exclude").write_text("excluded/\n", encoding="utf-8")
    (root / "scratch" / "file.txt").write_text("x\n", encoding="utf-8")
    return r


class RecordingGit(gitops.Git):
    """A real ``Git`` that records the argv and spec of every run."""

    def __init__(self) -> None:
        super().__init__()
        self.runs: list[tuple[list, gitops.GitEnvSpec, Path | None]] = []

    def run(self, args, *, cwd, spec, **kw):
        self.runs.append((list(args), spec, cwd))
        return super().run(args, cwd=cwd, spec=spec, **kw)


@pytest.mark.parametrize(
    ("rel", "ignored"),
    [
        ("scratch", True),
        ("scratch/session", True),
        ("scratch/session/data", True),
        (".cache/w", True),
        ("excluded/x", True),
        ("build/out", True),
        ("src", False),
        ("src/sub", False),
        ("build", False),
        ("build/keep", False),
        ("", False),
    ],
    ids=lambda v: str(v) if not isinstance(v, str) or v else "<root>",
)
def test_is_ignored_answers_like_git_check_ignore(tmp_path, rel, ignored):
    """★ R36: an ignored directory (by ``.gitignore``, by ``.git/info/exclude``, itself or through an ignored parent)
    is ignored; a directory git does not ignore, the work-tree root and an ignored directory that holds a tracked
    (force-added) file are not — git's own answer, the same the user's ``git status`` follows.

    Red on ``ae0749a5``: ``AttributeError: 'GitProbe' object has no attribute 'is_ignored'``.

    Mutant: ``is_ignored`` maps exit status 0 to False (the answer inverted)."""
    _repo(tmp_path / "w")
    probe = gitops.GitProbe(gitops.Git(), ceiling=tmp_path)
    path = tmp_path / "w" / rel if rel else tmp_path / "w"
    assert probe.is_ignored(str(path)) is ignored


def test_is_ignored_runs_check_ignore_through_the_hardened_runner(tmp_path):
    """★ R36 / DESIGN §9.2: one ``git check-ignore -q --end-of-options .`` in the directory, through ``Git.run`` with
    the probe's discovery spec — the forced options (``core.fsmonitor=false`` …) and the ceiling are in its
    environment, no inherited ``GIT_*`` variable is.

    Mutants: argv without ``--end-of-options``; the run outside ``Git.run`` (plain ``subprocess``)."""
    _repo(tmp_path / "w")
    git = RecordingGit()
    probe = gitops.GitProbe(git, ceiling=tmp_path)
    assert probe.is_ignored(str(tmp_path / "w" / "scratch")) is True
    assert len(git.runs) == 1, git.runs
    args, spec, cwd = git.runs[0]
    assert args == ["check-ignore", "-q", "--end-of-options", "."]
    assert cwd == tmp_path / "w" / "scratch"
    assert (spec.profile, spec.ceiling) == ("source", tmp_path)
    env = git.env(spec)
    pairs = {(env[f"GIT_CONFIG_KEY_{i}"], env[f"GIT_CONFIG_VALUE_{i}"]) for i in range(int(env["GIT_CONFIG_COUNT"]))}
    assert set(gitops.FORCED_OPTIONS) <= pairs
    assert env["GIT_CEILING_DIRECTORIES"] == str(tmp_path)


@pytest.mark.parametrize("rc", [128, 129, 2, -9])
def test_is_ignored_fails_closed_on_an_unclear_answer(tmp_path, rc):
    """★ R36, success sweep: only exit status 1 (git says "not ignored") reads as not ignored; a git that died or
    refused (128, 129, a kill) counts as ignored — the collector then keeps the workspace to the directory itself
    instead of growing it to the enclosing repository.

    Mutant: ``is_ignored`` returns ``proc.returncode == 0`` (an unclear answer reads as "not ignored")."""
    _repo(tmp_path / "w")
    git = FailingGit(("check-ignore",), rc=rc)
    probe = gitops.GitProbe(git, ceiling=tmp_path)
    assert probe.is_ignored(str(tmp_path / "w" / "src")) is True
    assert git.failed == [("check-ignore",)]


def test_is_ignored_outside_a_work_tree_and_for_a_file(tmp_path):
    """No directory or no work tree: git cannot confirm "not ignored", so the answer is True (fail closed); the
    collector asks only for a directory below a work tree's root."""
    r = _repo(tmp_path / "w")
    plain = tmp_path / "plain"
    plain.mkdir()
    probe = gitops.GitProbe(gitops.Git(), ceiling=tmp_path)
    assert probe.is_ignored(str(plain)) is True
    assert probe.is_ignored(str(r.root / "src" / "app.py")) is True
    assert probe.is_ignored(str(tmp_path / "missing")) is True


def test_probes_satisfy_the_protocol_with_is_ignored():
    """K0 + E: ``is_ignored`` is a member of ``contracts.GitProbe``; E's probe and the scripted fake satisfy it; the
    fake answers "not ignored" unless a path is scripted as ignored (or lies below one)."""
    assert isinstance(gitops.GitProbe(gitops.Git()), contracts.GitProbe)
    fake = FakeGitProbe(ignored={"/w/repo/scratch"})
    assert isinstance(fake, contracts.GitProbe)
    assert fake.is_ignored("/w/repo/scratch") is True
    assert fake.is_ignored("/w/repo/scratch/session") is True
    assert fake.is_ignored("/w/repo/scratch-other") is False
    assert fake.is_ignored("/w/repo/src") is False
    assert ("is_ignored", ("/w/repo/scratch",)) in fake.calls


# ---------------------------------------------------------------------------------------------------------
# R36a: does the directory hold tracked files? (a tracked subfolder of the own repository needs no question)
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("rel", "tracked"),
    [
        ("src", True),
        ("build", True),
        ("build/keep", True),
        ("", True),
        ("src/sub", False),
        ("scratch/session", False),
        ("scratch", False),
        ("build/out", False),
        ("excluded/x", False),
    ],
    ids=lambda v: str(v) if not isinstance(v, str) or v else "<root>",
)
def test_has_tracked_files_answers_from_the_index(tmp_path, rel, tracked):
    """★ R36a: True when the index holds a file below the directory (also one force-added into an ignored folder),
    False for a folder with untracked or ignored files only (``src/sub`` is empty, ``scratch`` holds an untracked
    file).

    Red on ``08cfe0f0``: ``AttributeError: 'GitProbe' object has no attribute 'has_tracked_files'``.

    Mutant: ``has_tracked_files`` returns True for an empty listing."""
    _repo(tmp_path / "w")
    probe = gitops.GitProbe(gitops.Git(), ceiling=tmp_path)
    path = tmp_path / "w" / rel if rel else tmp_path / "w"
    assert probe.has_tracked_files(str(path)) is tracked


class StreamingGit(gitops.Git):
    """A real ``Git`` that records the argv and spec of every streamed command and how many records were read."""

    def __init__(self) -> None:
        super().__init__()
        self.streams: list[tuple[list, gitops.GitEnvSpec, Path | None]] = []
        self.read = 0

    def iter_z(self, args, *, cwd, spec, **kw):
        self.streams.append((list(args), spec, cwd))
        for record in super().iter_z(args, cwd=cwd, spec=spec, **kw):
            self.read += 1
            yield record


def test_has_tracked_files_streams_ls_files_through_the_hardened_runner(tmp_path):
    """★ R36a / DESIGN §9.2: one ``git ls-files -z --cached --end-of-options .`` in the directory, streamed through
    ``Git.iter_z`` (forced options, discovery ceiling) and stopped at the first record — a folder with many tracked
    files is never listed in full.

    Mutants: argv without ``--end-of-options``; the listing read to the end."""
    r = _repo(tmp_path / "w")
    for i in range(50):
        r.write(f"src/many/f{i:02d}.txt", f"{i}\n")
    r.commit_all("many")
    git = StreamingGit()
    probe = gitops.GitProbe(git, ceiling=tmp_path)
    assert probe.has_tracked_files(str(tmp_path / "w" / "src")) is True
    assert len(git.streams) == 1, git.streams
    args, spec, cwd = git.streams[0]
    assert args == ["ls-files", "-z", "--cached", "--end-of-options", "."]
    assert cwd == tmp_path / "w" / "src"
    assert (spec.profile, spec.ceiling) == ("source", tmp_path)
    assert git.read == 1


class FailingStreamGit(gitops.Git):
    """A real ``Git`` whose streamed ``ls-files`` fails like a git that refused or died: ``iter_z`` raises
    ``GitCommandError`` with ``rc`` after the last (here: no) record, as it does for a failing git."""

    def __init__(self, rc: int):
        super().__init__()
        self.rc = rc
        self.failed: list[tuple[str, ...]] = []

    def iter_z(self, args, *, cwd, spec, **kw):
        if "ls-files" not in args:
            yield from super().iter_z(args, cwd=cwd, spec=spec, **kw)
            return
        self.failed.append(("ls-files",))
        raise self._error(args, self.rc, b"fatal: injected failure")
        yield b""  # pragma: no cover - makes this a generator like iter_z


@pytest.mark.parametrize("rc", [128, 129, -9])
def test_has_tracked_files_is_unknown_when_git_fails(tmp_path, rc):
    """★ R36a, success sweep: a git that refused or died is no answer — None (unknown; the plan then asks), never
    False or True.

    Mutant: a failing ``ls-files`` reads as False (an empty listing)."""
    _repo(tmp_path / "w")
    git = FailingStreamGit(rc)
    probe = gitops.GitProbe(git, ceiling=tmp_path)
    assert probe.has_tracked_files(str(tmp_path / "w" / "src")) is None
    assert git.failed == [("ls-files",)]


def test_has_tracked_files_outside_a_work_tree_and_for_a_file(tmp_path):
    """No directory or no work tree: None (git cannot answer)."""
    r = _repo(tmp_path / "w")
    plain = tmp_path / "plain"
    plain.mkdir()
    probe = gitops.GitProbe(gitops.Git(), ceiling=tmp_path)
    assert probe.has_tracked_files(str(plain)) is None
    assert probe.has_tracked_files(str(r.root / "src" / "app.py")) is None
    assert probe.has_tracked_files(str(tmp_path / "missing")) is None


def test_fake_probe_answers_has_tracked_files():
    """The scripted fake: a scripted directory (or one below it) holds tracked files, every other path is unknown
    (None) unless scripted as untracked."""
    fake = FakeGitProbe(tracked={"/w/repo/src"}, untracked={"/w/repo/drafts"})
    assert fake.has_tracked_files("/w/repo/src") is True
    assert fake.has_tracked_files("/w/repo/src/sub") is True
    assert fake.has_tracked_files("/w/repo/drafts") is False
    assert fake.has_tracked_files("/w/repo/other") is None
    assert ("has_tracked_files", ("/w/repo/src",)) in fake.calls
