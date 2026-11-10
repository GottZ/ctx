"""Catalog review of 2026-11-10, point 1: every git process (and everything git starts — git-lfs, a helper) gets an
environment built from an allowlist, never the inherited one. Provider keys and tokens of the Hermes process must
not reach it. The probes here are real child processes: a ``!env`` alias that git runs through its shell, and a
``git-lfs`` stand-in that git starts for ``git lfs`` — not a look at the dict ``Git.env`` returns."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import envfacts, gitapply, gitops
from ctx_handover.tests.fakes.repo_builder import RepoBuilder

SECRETS = {
    "OPENROUTER_API_KEY": "sk-or-fake-0001",
    "ANTHROPIC_API_KEY": "sk-ant-fake-0002",
    "X_TOKEN": "fake-token-0003",
    "SSH_AUTH_SOCK": "/nonexistent/agent.sock",
    "HTTPS_PROXY": "http://proxy.invalid:3128",
    "SSL_CERT_FILE": "/nonexistent/ca.pem",
    "LD_LIBRARY_PATH": "/nonexistent/lib",
}
"""Set in the parent; none may arrive (git talks to no remote, so neither the agent socket nor proxy or CA
settings have a use there)."""

ADDED_BY_THE_CHILD = frozenset({"PWD", "OLDPWD", "SHLVL", "_"})
"""Names the shell running the alias sets itself."""

PROFILES = [gitops.SOURCE, gitops.RECEIVED]


def _allowed() -> frozenset[str]:
    return frozenset({*gitops.CHILD_ENV_POSIX, *gitops.GIT_ENV_POSIX, *gitops.SET_ENV})


def _parse(raw: bytes) -> dict[str, str]:
    out: dict[str, str] = {}
    for rec in raw.split(b"\x00"):
        key, sep, value = rec.partition(b"=")
        if sep:
            out[key.decode()] = value.decode(errors="replace")
    return out


@pytest.fixture
def secrets_set(monkeypatch, tmp_path) -> None:
    for key, value in SECRETS.items():
        monkeypatch.setenv(key, value)
    tmp = tmp_path / "proc-tmp"
    tmp.mkdir()
    monkeypatch.setenv("TMPDIR", str(tmp))
    monkeypatch.setenv("TZ", "Europe/Berlin")


def _assert_clean(env: dict[str, str]) -> None:
    for key in SECRETS:
        assert key not in env, f"{key} reached the git child"
    assert not any(v in env.values() for v in SECRETS.values())
    stray = {k for k in env if k not in _allowed() and not k.startswith("GIT_") and k not in ADDED_BY_THE_CHILD}
    assert not stray, f"names outside the allowlist reached the git child: {sorted(stray)}"


@pytest.mark.parametrize("spec", PROFILES, ids=lambda s: s.profile)
def test_r5_secrets_never_reach_a_git_child(git, user_home, tmp_path, secrets_set, spec):
    """★ Both profiles, ``Git.run``: the inherited secrets stay in the parent; what git needs arrives (``HOME`` as
    pinned, ``XDG_CONFIG_HOME``, ``TMPDIR``, ``TZ``, ``PATH``, ``LC_ALL=C``). Before the fix ``_base_env`` copied
    ``os.environ`` minus ``GIT_*`` and every key above reached git."""
    repo = RepoBuilder.init(tmp_path / "repo")
    proc = git.run(["-c", "alias.e=!env -0", "e"], cwd=repo.root, spec=spec)
    env = _parse(proc.stdout)
    _assert_clean(env)
    assert env["HOME"] == os.environ["HOME"]
    assert env["XDG_CONFIG_HOME"] == os.environ["XDG_CONFIG_HOME"]
    assert env["TMPDIR"] == os.environ["TMPDIR"]
    assert env["TZ"] == "Europe/Berlin"
    assert env["PATH"].endswith(os.environ["PATH"])  # git puts its exec path in front for an alias
    assert env["LC_ALL"] == "C"


@pytest.mark.parametrize("spec", PROFILES, ids=lambda s: s.profile)
def test_r5_streaming_entry_points_get_the_same_environment(git, tmp_path, secrets_set, spec):
    """★ ``iter_z``, ``popen_stdout`` and ``popen_stdin`` build their environment the same way as ``run``."""
    repo = RepoBuilder.init(tmp_path / "repo")
    alias = ["-c", "alias.e=!cat >/dev/null; env -0", "e"]
    streamed = b"\x00".join(git.iter_z(["-c", "alias.e=!env -0", "e"], cwd=repo.root, spec=spec))
    _assert_clean(_parse(streamed))
    proc = git.popen_stdout(alias, cwd=repo.root, spec=spec, input_lines=["x"])
    out = proc.stdout.read()
    assert proc.wait() == 0
    _assert_clean(_parse(out))
    dump = tmp_path / "stdin-env"
    proc = git.popen_stdin(["-c", f"alias.e=!cat >/dev/null; env -0 > '{dump}'", "e"], cwd=repo.root, spec=spec)
    proc.stdin.close()
    assert proc.wait() == 0
    _assert_clean(_parse(dump.read_bytes()))


def test_r5_git_lfs_started_by_git_gets_no_secret(git, user_home, tmp_path, secrets_set, monkeypatch):
    """★ git-lfs is a child of git (``git lfs checkout`` runs ``git-lfs`` from ``PATH``): the plugin's LFS checkout
    hands it the same minimal environment."""
    bindir = tmp_path / "lfs-bin"
    bindir.mkdir()
    dump = tmp_path / "lfs-env"
    script = bindir / "git-lfs"
    script.write_text(f'#!/bin/sh\nenv -0 > "{dump}"\n', encoding="utf-8")
    os.chmod(script, 0o755)  # noqa: S103 - executable test stand-in
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ['PATH']}")
    repo = RepoBuilder.init(tmp_path / "repo")
    repo.write("a.txt", "a\n")
    repo.commit_all("c1")
    assert gitapply.lfs_checkout(git, repo.root) == []
    _assert_clean(_parse(dump.read_bytes()))


def test_r5_unpinned_runner_uses_the_process_home(user_home, tmp_path, secrets_set):
    """``Git()`` without a pinned home (the doctor's version check) passes the process ``HOME`` on."""
    unpinned = gitops.Git()
    proc = unpinned.run(["-c", "alias.e=!env -0", "e"], cwd=tmp_path, spec=gitops.SOURCE)
    env = _parse(proc.stdout)
    _assert_clean(env)
    assert env["HOME"] == str(user_home)


def test_r5_pinned_home_wins_over_the_process_home(user_home, tmp_path, secrets_set):
    other = tmp_path / "other-home"
    other.mkdir()
    proc = gitops.Git(home=other).run(["-c", "alias.e=!env -0", "e"], cwd=tmp_path, spec=gitops.SOURCE)
    assert _parse(proc.stdout)["HOME"] == str(other)


def test_r5_windows_allowlist(monkeypatch):
    """The Windows branch (dict level only — no Windows host here): the system variables git needs to start and
    to find the home and temp directory pass, the secrets do not."""
    fake = {
        "PATH": r"C:\Program Files\Git\cmd",
        "SYSTEMROOT": r"C:\Windows",
        "WINDIR": r"C:\Windows",
        "PATHEXT": ".COM;.EXE;.BAT;.CMD",
        "USERPROFILE": r"C:\Users\u",
        "HOMEDRIVE": "C:",
        "HOMEPATH": r"\Users\u",
        "PROGRAMDATA": r"C:\ProgramData",
        "TEMP": r"C:\Users\u\AppData\Local\Temp",
        "TMP": r"C:\Users\u\AppData\Local\Temp",
        "APPDATA": r"C:\Users\u\AppData\Roaming",
        **SECRETS,
    }
    monkeypatch.setattr(gitops, "_WINDOWS", True)
    monkeypatch.setattr(gitops.os, "environ", fake)
    env = gitops.Git(git_exe="git")._base_env()
    for key in ("PATH", "SYSTEMROOT", "WINDIR", "PATHEXT", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "PROGRAMDATA"):
        assert env[key] == fake[key]
    assert env["TEMP"] == fake["TEMP"] and env["TMP"] == fake["TMP"]
    for key in (*SECRETS, "APPDATA"):
        assert key not in env


def test_r5_tool_probes_and_git_share_the_system_variables():
    """One source for what every child needs to start (the tool probes of ``envfacts`` and git)."""
    assert envfacts._CHILD_ENV_POSIX is gitops.CHILD_ENV_POSIX
    assert envfacts._CHILD_ENV_WINDOWS is gitops.CHILD_ENV_WINDOWS


def test_r5_git_operations_work_with_the_minimal_environment(git, user_home, tmp_path, secrets_set, monkeypatch):
    """A parent with nothing but the secrets and the allowlisted names: init, add, commit-tree through the
    technical identity, ls-files — the runner needs nothing else of the parent."""
    keep = {k: os.environ[k] for k in ("PATH", "HOME", "XDG_CONFIG_HOME", "TMPDIR") if k in os.environ}
    for key in list(os.environ):
        if key not in keep and key not in SECRETS:
            monkeypatch.delenv(key)
    repo = tmp_path / "repo"
    git.run(["init", "-q", "--template=", str(repo)], cwd=None, spec=gitops.RECEIVED)
    Path(repo, "f.txt").write_text("f\n", encoding="utf-8")
    git.run(["add", "f.txt"], cwd=repo, spec=gitops.SOURCE)
    tree = git.out(["write-tree"], cwd=repo, spec=gitops.SOURCE)
    commit = git.out(
        ["commit-tree", "-m", "m", tree],
        cwd=repo,
        spec=gitops.GitEnvSpec(profile="received", technical_identity_at=1.7e9),
    )
    assert gitops.is_sha(commit)
    assert git.out(["ls-files"], cwd=repo, spec=gitops.RECEIVED) == "f.txt"
