"""Hardened git runner: profiles, forced options, inherited variables, version gate (DESIGN §9.2, R21)."""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from ctx_handover.engine import gitops
from ctx_handover.engine.errors import CapabilityMissing
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import (
    append_repo_config,
    apply_fresh,
    build_mixed,
    marker_script,
    pack,
    write_global_gitconfig,
)

# DESIGN §9.2, typed out independently of gitops.FORCED_OPTIONS so an omission there turns this test red.
DESIGN_FORCED = {
    "credential.helper": "",
    "core.askPass": "",
    "core.fsmonitor": "false",
    "core.untrackedCache": "false",
    "core.hooksPath": os.devnull,
    "core.pager": "cat",
    "core.editor": "true",
    "sequence.editor": "true",
    "diff.external": "",
    "core.sshCommand": "ssh -o BatchMode=yes",
    "submodule.recurse": "false",
    "fetch.recurseSubmodules": "false",
    "gc.auto": "0",
    "maintenance.auto": "false",
    "commit.gpgSign": "false",
    "tag.gpgSign": "false",
    "safe.bareRepository": "explicit",
    "protocol.allow": "never",
    "core.protectNTFS": "true",
    "core.protectHFS": "true",
    "transfer.fsckObjects": "true",
    "fetch.fsckObjects": "true",
}
DESIGN_ENV = {
    "GIT_TERMINAL_PROMPT": "0",
    "GCM_INTERACTIVE": "Never",
    "GIT_PAGER": "cat",
    "PAGER": "cat",
    "GIT_EDITOR": "true",
    "GIT_SEQUENCE_EDITOR": "true",
    "GIT_NO_LAZY_FETCH": "1",
    "GIT_NO_REPLACE_OBJECTS": "1",
    "GIT_OPTIONAL_LOCKS": "0",
    "GIT_LFS_SKIP_SMUDGE": "1",
    "LC_ALL": "C",
}


def _pairs(env: dict) -> list[tuple[str, str]]:
    n = int(env.get("GIT_CONFIG_COUNT", "0"))
    return [(env[f"GIT_CONFIG_KEY_{i}"], env[f"GIT_CONFIG_VALUE_{i}"]) for i in range(n)]


def _effective(pairs: list[tuple[str, str]], key: str) -> str | None:
    found = None
    for k, v in pairs:
        if k.lower() == key.lower():
            found = v
    return found


@pytest.mark.real_system_config
def test_git_env_source_and_received_profiles(git, user_home, tmp_path):
    """★ received nulls the user's global/system config and passes only the safelist back; source keeps the user's
    config (mutant: received profile without GIT_CONFIG_GLOBAL/SYSTEM nulling — the v1 behaviour) [R21, SEC 3.1]."""
    write_global_gitconfig(
        user_home,
        "[core]\n\tautocrlf = input\n[safe]\n\tdirectory = /srv/a\n\tdirectory = /srv/b\n"
        '[filter "probe"]\n\tsmudge = cat\n\tclean = cat\n[alias]\n\tzz = status\n',
    )
    received = git.env(gitops.RECEIVED)
    source = git.env(gitops.SOURCE)
    assert received["GIT_CONFIG_GLOBAL"] == os.devnull
    assert received["GIT_CONFIG_SYSTEM"] == os.devnull
    assert received["GIT_CONFIG_NOSYSTEM"] == "1"
    assert "GIT_CONFIG_GLOBAL" not in source and "GIT_CONFIG_NOSYSTEM" not in source
    pairs = _pairs(received)
    assert ("core.autocrlf", "input") in pairs
    assert ("safe.directory", "/srv/a") in pairs and ("safe.directory", "/srv/b") in pairs
    assert not any(k.startswith("alias.") for k, _ in pairs)

    def value(spec, key):
        proc = git.run(["config", "--get", key], cwd=tmp_path, spec=spec, check=False)
        return proc.stdout.decode().strip() if proc.returncode == 0 else None

    assert value(gitops.SOURCE, "filter.probe.smudge") == "cat"
    assert value(gitops.SOURCE, "alias.zz") == "status"
    assert value(gitops.RECEIVED, "filter.probe.smudge") is None
    assert value(gitops.RECEIVED, "alias.zz") is None
    assert value(gitops.RECEIVED, "core.autocrlf") == "input"


@pytest.mark.parametrize("profile", ["source", "received"])
def test_git_env_forced_options_complete(git, profile):
    """Every forced option and variable of DESIGN §9.2 is in the environment of each profile [SEC 3.3]."""
    env = git.env(gitops.GitEnvSpec(profile=profile))
    pairs = _pairs(env)
    for key, val in DESIGN_FORCED.items():
        assert _effective(pairs, key) == val, key
    for key, val in DESIGN_ENV.items():
        assert env[key] == val, key


def test_git_env_forced_options_on_every_call(git, user_home, tmp_path, staging, cfg, store, monkeypatch):
    """A whole pack + fresh apply: every git process carried every forced option [SEC 3.3]."""
    seen: list[dict] = []
    real = subprocess.Popen

    def spy(argv, *a, **kw):
        if argv and str(argv[0]) == git.exe:
            seen.append(dict(kw.get("env") or {}))
        return real(argv, *a, **kw)

    monkeypatch.setattr(gitops.subprocess, "Popen", spy)
    r = build_mixed(user_home / "src")
    seen.clear()
    _, item = pack(git, r.root, staging, cfg, store)
    apply_fresh(git, store, item, user_home / "hermes-handover" / "app", cfg)
    assert len(seen) > 20
    for env in seen:
        pairs = _pairs(env)
        missing = [k for k, v in DESIGN_FORCED.items() if _effective(pairs, k) != v]
        assert not missing
        assert not [k for k in env if k.startswith("GIT_") and k in {"GIT_DIR", "GIT_WORK_TREE", "GIT_EXEC_PATH"}]


def test_git_inherited_git_vars_removed(git, user_home, tmp_path, staging, cfg, store, monkeypatch):
    """GIT_DIR, GIT_SSH_COMMAND, GIT_EXEC_PATH, GIT_CONFIG_PARAMETERS … from the caller have no effect [SEC 3.3]."""
    markers = tmp_path / "markers"
    markers.mkdir()
    evil = marker_script(tmp_path / "evil.sh", markers, "ssh")
    r = build_mixed(user_home / "src")
    monkeypatch.setenv("GIT_DIR", str(tmp_path / "nowhere"))
    monkeypatch.setenv("GIT_WORK_TREE", str(tmp_path))
    monkeypatch.setenv("GIT_INDEX_FILE", str(tmp_path / "evil-index"))
    monkeypatch.setenv("GIT_SSH_COMMAND", str(evil))
    monkeypatch.setenv("GIT_EXEC_PATH", str(tmp_path / "no-exec-path"))
    monkeypatch.setenv("GIT_CONFIG_PARAMETERS", f"'core.hookspath'='{tmp_path}'")
    monkeypatch.setenv("GIT_CONFIG_GLOBAL", str(tmp_path / "evil.cfg"))
    for spec in (gitops.SOURCE, gitops.RECEIVED):
        env = git.env(spec)
        for name in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_SSH_COMMAND", "GIT_EXEC_PATH"):
            assert name not in env
        assert "GIT_CONFIG_PARAMETERS" not in env
    assert git.env(gitops.SOURCE).get("GIT_CONFIG_GLOBAL") is None
    _, item = pack(git, r.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, user_home / "hermes-handover" / "app", cfg)
    assert (dest / "untracked.txt").is_file()
    assert not (tmp_path / "evil-index").exists()
    assert list(markers.iterdir()) == []


def test_git_hooks_never_run(git, user_home, tmp_path, staging, cfg, store):
    """★ hooks and fsmonitor of the source and of an update-mode destination never run during pack and apply
    (mutant: core.hooksPath and core.fsmonitor not forced)."""
    markers = tmp_path / "markers"
    markers.mkdir()
    hooks = (
        "post-index-change",
        "reference-transaction",
        "post-checkout",
        "pre-commit",
        "post-commit",
        "pre-auto-gc",
        "post-rewrite",
    )

    def arm(repo: Path, tag: str) -> None:
        hook_dir = repo / ".git" / "hooks"
        hook_dir.mkdir(exist_ok=True)
        for name in hooks:
            marker_script(hook_dir / name, markers, f"{tag}-{name}")
        fsmon = marker_script(tmp_path / f"fsmonitor-{tag}.sh", markers, f"{tag}-fsmonitor")
        append_repo_config(repo, f"[core]\n\tfsmonitor = {fsmon}\n")

    src = build_mixed(user_home / "src")
    root = user_home / "hermes-handover"
    _, item0 = pack(git, src.root, staging, cfg, store, item_id="a")
    target = apply_fresh(git, store, item0, root / "app", cfg)
    arm(src.root, "src")
    tgt = RepoBuilder(target)
    tgt.run("stash", "clear")
    tgt.run("reset", "-q", "--hard")
    tgt.run("clean", "-qfdx")
    arm(target, "dst")
    src.write("later.txt", "x\n")
    _, item = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(target)), item_id="b")
    from ctx_handover.engine import gitapply

    state = gitapply.prepare_update(git, store, item, target, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item)
    assert (target / "later.txt").is_file()
    assert sorted(p.name for p in markers.iterdir()) == []


def test_git_no_background_maintenance(git, user_home, tmp_path, staging, cfg, store):
    """gc.auto=0 and maintenance.auto=false win over the user's and the repository's settings [SEC 3.3, SK L3]."""
    write_global_gitconfig(user_home, "[gc]\n\tauto = 1\n[maintenance]\n\tauto = true\n")
    r = build_mixed(user_home / "src")
    append_repo_config(r.root, "[gc]\n\tauto = 1\n\tautoDetach = false\n[maintenance]\n\tauto = true\n")
    for spec in (gitops.SOURCE, gitops.RECEIVED):
        assert git.out(["config", "--get", "gc.auto"], cwd=r.root, spec=spec) == "0"
        assert git.out(["config", "--get", "maintenance.auto"], cwd=r.root, spec=spec) == "false"
    _, item = pack(git, r.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, user_home / "hermes-handover" / "app", cfg)
    for repo in (r.root, dest):
        assert not (repo / ".git" / "gc.pid").exists()
        assert not (repo / ".git" / "gc.log").exists()


def test_git_synthetic_commit_ignores_gpgsign(git, user_home, tmp_path, staging, cfg, store):
    """commit.gpgSign=true in the user's config: no signing program runs, I/W carry no signature [SEC 3.1]."""
    markers = tmp_path / "markers"
    markers.mkdir()
    signer = marker_script(tmp_path / "gpg.sh", markers, "gpg")
    write_global_gitconfig(
        user_home, f"[commit]\n\tgpgSign = true\n[tag]\n\tgpgSign = true\n[gpg]\n\tprogram = {signer}\n"
    )
    r = build_mixed(user_home / "src")
    append_repo_config(r.root, f"[commit]\n\tgpgSign = true\n[gpg]\n\tprogram = {signer}\n")
    res, _ = pack(git, r.root, staging, cfg, store)
    repo = staging / "git" / "w0" / "repo"
    for sha in (res.index_commit, res.worktree_commit):
        raw = git.run(["--git-dir", str(repo), "cat-file", "commit", sha], cwd=None, spec=gitops.RECEIVED).stdout
        assert b"gpgsig" not in raw
    assert list(markers.iterdir()) == []


def _fake_git(tmp_path: Path, version: str) -> str:
    exe = tmp_path / f"git-{version}"
    exe.write_text(f'#!/bin/sh\necho "git version {version}"\n', encoding="utf-8")
    os.chmod(exe, 0o755)  # noqa: S103 - executable test fixture
    return str(exe)


def test_git_version_gate(tmp_path):
    """< 2.38 refused (``capability_missing``); below a patched release line ⇒ warning [R21, SEC 3.6]."""
    old = gitops.Git(_fake_git(tmp_path, "2.37.1"))
    assert gitops.check_version(old)[0] is False
    with pytest.raises(CapabilityMissing) as exc:
        old.ensure_supported()
    assert exc.value.details["capability"] == "git.bundle.v1"
    ok, warn = gitops.check_version(gitops.Git(_fake_git(tmp_path, "2.43.5")))
    assert ok is True and "2.43.7" in warn
    ok, warn = gitops.check_version(gitops.Git(_fake_git(tmp_path, "2.38.0")))
    assert ok is True and warn
    assert gitops.check_version(gitops.Git(_fake_git(tmp_path, "2.45.4"))) == (True, None)
    assert gitops.check_version(gitops.Git(_fake_git(tmp_path, "2.51.0"))) == (True, None)
    assert gitops.check_version(gitops.Git(_fake_git(tmp_path, "2.39.3 (Apple Git-146)")))[0] is True
    assert gitops.check_version(gitops.Git(str(tmp_path / "missing-git")))[0] is False


def test_worktree_state_is_read_only(git, user_home, tmp_path):
    """The temp-index procedure leaves refs, index, objects and work tree byte-identical (DESIGN §9.3)."""
    from ctx_handover.tests.fakes.repo_builder import dir_fingerprint

    r = build_mixed(user_home / "src")
    before = dir_fingerprint(r.root)
    state = gitops.worktree_state(git, str(r.root), [gitops.encode_pathspec(b".env")], tmp_path)
    assert dir_fingerprint(r.root) == before
    again = gitops.worktree_state(git, str(r.root), [gitops.encode_pathspec(b".env")], tmp_path)
    assert again == state
    r.write("untracked.txt", "changed\n")
    changed = gitops.worktree_state(git, str(r.root), [gitops.encode_pathspec(b".env")], tmp_path)
    assert changed.worktree_tree != state.worktree_tree and changed.index_tree == state.index_tree


def test_pathspec_encoding_roundtrip():
    for raw in (b".env", b"n\xe4me.txt", b"new\nline.txt"):
        spec = gitops.encode_pathspec(raw)
        assert gitops.pathspec_path(spec) == raw
    with pytest.raises(Exception):  # noqa: B017 - any PolicyDenied subclass
        gitops.decode_pathspec(":(top)")
