"""Fixtures of the workspace cluster (E): real git, an on-disk block store, the target policy of the isolated
test home. Repositories are built only through ``tests.fakes.repo_builder`` (plugin helper, technical identity)."""

from __future__ import annotations

import os
import shutil

import pytest

pytest.register_assert_rewrite("ctx_handover.tests.unit.E._support")

from ctx_handover.engine import gitops  # noqa: E402
from ctx_handover.tests.fakes.git_fake import DirBlockStore  # noqa: E402
from ctx_handover.tests.unit.E._support import make_cfg  # noqa: E402


@pytest.fixture(autouse=True)
def _fresh_git_caches():
    """The safelist is read once per process from ``$HOME``; every test has its own home."""
    gitops.reset_process_caches()
    yield
    gitops.reset_process_caches()


@pytest.fixture
def git() -> gitops.Git:
    g = gitops.Git()
    g.ensure_supported()
    return g


@pytest.fixture
def cfg(user_home, hermes_home):
    return make_cfg(user_home, hermes_home)


@pytest.fixture
def store(tmp_path) -> DirBlockStore:
    return DirBlockStore(tmp_path / "store")


@pytest.fixture
def staging(tmp_path):
    path = tmp_path / "staging"
    path.mkdir(mode=0o700)
    return path


@pytest.fixture
def handover_root(user_home):
    root = user_home / "hermes-handover"
    root.mkdir()
    return root


@pytest.fixture
def no_lfs(tmp_path, monkeypatch, git) -> None:
    """A ``PATH`` holding only git: no git-lfs, whatever this host has installed (the fresh/update LFS checkout of
    DESIGN §9.4 runs only with git-lfs — tests of the pointer-stays path must not depend on the host)."""
    bindir = tmp_path / "no-lfs-bin"
    bindir.mkdir()
    os.symlink(shutil.which("git") or "/usr/bin/git", bindir / "git")
    monkeypatch.setenv("PATH", str(bindir))
    assert shutil.which("git-lfs") is None
