"""First review round, gitops parts of M24 (the size listing stops at the limit; streamed git output has the
runner's deadline), M25 (``core.longpaths=true`` on Windows hosts) and L15 (packing never writes a shared index
into the source's ``.git``) — DESIGN §9.2, §9.3, §9.4, SEC 2.5."""

from __future__ import annotations

import os
import time

import pytest

from ctx_handover.engine import gitops
from ctx_handover.engine.errors import PayloadTooLarge
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.unit.E._support import append_repo_config, build_mixed, pack, write_global_gitconfig


def _fanout_tree(git: gitops.Git, repo: RepoBuilder, width: int) -> str:
    """A tree of ``width × width`` one-byte blobs (written into the repository's objects, nothing checked out)."""
    spec = gitops.GitEnvSpec(profile="received", ceiling=repo.root.parent)
    blob = git.out(["hash-object", "-w", "--stdin"], cwd=repo.root, spec=spec, input=b"x")
    sub_body = b"".join(b"100644 f%04d\x00" % i + bytes.fromhex(blob) for i in range(width))
    sub = git.out(["hash-object", "-w", "-t", "tree", "--stdin"], cwd=repo.root, spec=spec, input=sub_body)
    root_body = b"".join(b"40000 d%04d\x00" % i + bytes.fromhex(sub) for i in range(width))
    return git.out(["hash-object", "-w", "-t", "tree", "--stdin"], cwd=repo.root, spec=spec, input=root_body)


def test_rv_e2_m24_ls_tree_sizes_stops_at_the_limit(git, user_home, monkeypatch):
    """★ M24 (gitops): ``ls_tree_sizes(max_entries=, max_bytes=)`` refuses as soon as the listing passes a limit
    and stops git there — it never reads the rest of what a few KiB of pack can describe (mutant: limits checked
    after the full listing ⇒ every record read)."""
    r = RepoBuilder.init(user_home / "repo")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    big = _fanout_tree(git, r, 40)
    probe = gitops.GitProbe(git, profile="received")
    assert probe.ls_tree_sizes(str(r.root), big) == (1600, 1600)
    read = []
    real = git.iter_z

    def counting(*a, **kw):
        for rec in real(*a, **kw):
            read.append(rec)
            yield rec

    monkeypatch.setattr(git, "iter_z", counting)
    with pytest.raises(PayloadTooLarge) as exc:
        probe.ls_tree_sizes(str(r.root), big, max_entries=100)
    assert exc.value.details["field"] == "max_files" and len(read) == 101
    read.clear()
    with pytest.raises(PayloadTooLarge) as exc:
        probe.ls_tree_sizes(str(r.root), big, max_bytes=50)
    assert exc.value.details["max_bytes"] == 50 and len(read) == 51
    assert probe.ls_tree_sizes(str(r.root), big, max_entries=1600, max_bytes=1600) == (1600, 1600)


def _slow_git(tmp_path, body: str) -> str:
    exe = tmp_path / "slow-git"
    exe.write_text("#!/bin/sh\n" + body, encoding="utf-8")
    os.chmod(exe, 0o755)  # noqa: S103 - executable test stand-in
    return str(exe)


@pytest.mark.parametrize("how", ["runner", "call"])
def test_rv_e2_m24_iter_z_has_the_runner_deadline(tmp_path, how):
    """★ M24 (gitops): a git whose streamed output stalls is killed at the deadline (``Git.timeout_s``, or the
    call's ``timeout``) and the read ends in ``GitCommandError`` — the record cut off by the kill is never yielded
    (mutant: no deadline ⇒ the reader waits for git; it then exits 0 and the call does not raise)."""
    exe = _slow_git(tmp_path, "printf 'a\\0b'\nexec sleep 6\n")
    if how == "runner":
        git, kw = gitops.Git(exe, timeout_s=0.5), {}
    else:
        git, kw = gitops.Git(exe), {"timeout": 0.5}
    got = []
    t0 = time.monotonic()
    with pytest.raises(gitops.GitCommandError) as exc:
        for rec in git.iter_z(["ls-files", "-z"], cwd=tmp_path, spec=gitops.SOURCE, **kw):
            got.append(rec)
    assert time.monotonic() - t0 < 4
    assert "timed out" in str(exc.value) and exc.value.details["rc"] == -1
    assert b"b" not in got and got in ([], [b"a"])  # reads are buffered: a stalled chunk may hold "a" too


def test_rv_e2_m24_iter_z_within_deadline_unchanged(tmp_path):
    exe = _slow_git(tmp_path, "printf 'a\\0b\\0c'\n")
    assert list(gitops.Git(exe, timeout_s=5).iter_z(["x"], cwd=tmp_path, spec=gitops.SOURCE)) == [b"a", b"b", b"c"]


def _effective(env: dict, key: str) -> str | None:
    found = None
    for i in range(int(env.get("GIT_CONFIG_COUNT", "0"))):
        if env[f"GIT_CONFIG_KEY_{i}"].lower() == key.lower():
            found = env[f"GIT_CONFIG_VALUE_{i}"]
    return found


@pytest.mark.parametrize("profile", ["source", "received"])
def test_rv_e2_m25_windows_host_forces_long_paths(git, user_home, monkeypatch, profile):
    """★ M25 (gitops): on a Windows host every git call gets ``core.longpaths=true`` (DESIGN §9.2, §9.4), after
    the safelist, so a user's ``core.longpaths=false`` cannot undo it; elsewhere nothing is added (mutant: the
    host check dropped ⇒ no ``core.longpaths``)."""
    write_global_gitconfig(user_home, "[core]\n\tlongpaths = false\n")
    spec = gitops.GitEnvSpec(profile=profile)
    monkeypatch.setattr(gitops, "_WINDOWS", True, raising=False)
    assert _effective(git.env(spec), "core.longpaths") == "true"
    monkeypatch.setattr(gitops, "_WINDOWS", False, raising=False)
    assert _effective(git.env(spec), "core.longpaths") in (None, "false")
    assert _effective(git.env(gitops.GitEnvSpec(profile=profile, windows_long_paths=True)), "core.longpaths") == "true"


def test_rv_e2_l15_split_index_source_untouched(git, user_home, staging, cfg, store, tmp_path):
    """★ L15: a source with ``core.splitIndex=true`` and a split index keeps its ``.git`` byte-identical through
    pack and state re-check — no new ``sharedindex.*`` (mutant: temp-index calls without ``core.splitIndex=false``)."""
    r = build_mixed(user_home / "src")
    append_repo_config(r.root, "[core]\n\tsplitIndex = true\n[splitIndex]\n\tmaxPercentChange = 0\n")
    r.run("update-index", "--split-index")
    shared = sorted(p.name for p in (r.root / ".git").iterdir() if p.name.startswith("sharedindex."))
    assert shared
    before = dir_fingerprint(r.root)
    pack(git, r.root, staging, cfg, store)
    gitops.worktree_state(git, str(r.root), (), tmp_path / "state")
    assert sorted(p.name for p in (r.root / ".git").iterdir() if p.name.startswith("sharedindex.")) == shared
    assert dir_fingerprint(r.root) == before
