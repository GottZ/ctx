"""Shared helpers of the E suites (no fixtures; imported by conftest and tests)."""

from __future__ import annotations

import os
import subprocess
from collections.abc import Iterable, Sequence
from pathlib import Path

from ctx_handover.engine import gitapply, gitops, gitpack
from ctx_handover.engine.model import EnvFacts, HostFacts, Options
from ctx_handover.engine.paths import rules_for
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.fakes.repo_builder import RepoBuilder

AT = 1_762_646_400.0
LINUX = rules_for("linux", False)


def make_cfg(user_home: Path, hermes_home: Path, **opts) -> PolicyConfig:
    include_secret = opts.pop("include_secret", ())
    case_insensitive = opts.pop("case_insensitive", False)
    target_os = opts.pop("target_os", "linux")
    return PolicyConfig.from_options(
        Options(**opts),
        home=user_home,
        hermes_home=hermes_home,
        hermes_root=hermes_home,
        plugin_dir=hermes_home / "plugin-data" / "ctx_handover",
        case_insensitive=case_insensitive,
        target_os=target_os,
        include_secret=include_secret,
    )


def build_mixed(root: Path) -> RepoBuilder:
    """The acceptance repository: staged, unstaged, untracked, deleted, renamed, mode change, symlink, stash,
    tags, a second branch, an ignored directory, a denied untracked file, non-UTF-8 and newline names."""
    r = RepoBuilder.init(root)
    r.write("keep.txt", "a\n")
    r.write("mod.txt", "b\n")
    r.write("del.txt", "c\n")
    r.write("ren.txt", "d\n" * 20)
    r.write("gone.txt", "e\n")
    r.write("tool.sh", "#!/bin/sh\necho hi\n")
    r.write("dir/sub/deep.txt", "deep\n")
    r.symlink("link", "keep.txt")
    r.write(".gitignore", "ignored/\n*.log\n")
    r.commit_all("c1")
    r.tag("v1")
    r.tag("v1-annotated", annotated=True)
    r.branch("feature")
    r.checkout("feature")
    r.write("feature.txt", "f\n")
    r.commit_all("c2 on feature")
    r.checkout("main")
    r.write("mod.txt", "b2\n")
    r.rm("del.txt")
    r.mv("ren.txt", "renamed.txt")
    os.chmod(r.path("tool.sh"), 0o755)  # noqa: S103 - executable test fixture
    r.write("staged_new.txt", "s1\n")
    r.add("staged_new.txt")
    r.write("staged_new.txt", "s2\n")
    r.write("untracked.txt", "u\n")
    r.write("dir/untracked_deep.txt", "ud\n")
    r.write(".env", "API_KEY=1\n")
    r.write(b"n\xe4me.txt", b"latin-1 name\n")
    r.write("new\nline.txt", "newline name\n")
    r.write("ignored/cache.bin", b"\x00" * 64)
    r.write("build.log", "log\n")
    r.remove("gone.txt")
    r.stash("wip on mod", paths=["mod.txt"])
    r.write("mod.txt", "b3\n")
    return r


def pack(
    git: gitops.Git,
    root: Path,
    staging: Path,
    cfg: PolicyConfig,
    store,
    *,
    basis: Sequence[str] = (),
    at: float = AT,
    item_id: str = "w0",
    workdir: Path | None = None,
    **kw,
) -> tuple[gitpack.GitPackResult, dict]:
    res = gitpack.pack_repo(
        git,
        str(root),
        workdir or staging / "git" / item_id,
        basis_candidates=basis,
        cfg=cfg,
        store=store,
        created_at=at,
        force=False,
        **kw,
    )
    return res, gitpack.pack_item(item_id, res)


def apply_fresh(git, store, item: dict, dest: Path, cfg: PolicyConfig, *, rules=LINUX, qdir=None) -> Path:
    dest.parent.mkdir(parents=True, exist_ok=True)
    state = gitapply.prepare_fresh(
        git, store, item, dest.parent / f".ctxh-{dest.name}", rules=rules, cfg=cfg, dest=dest, qdir=qdir
    )
    return gitapply.commit(git, state, item)


def status_records(raw: bytes) -> list[bytes]:
    """Records of ``status --porcelain=v2 -z`` (a rename record keeps its original path joined to it)."""
    parts = raw.split(b"\x00")
    out: list[bytes] = []
    i = 0
    while i < len(parts):
        rec = parts[i]
        if rec.startswith(b"2 ") and i + 1 < len(parts):
            out.append(rec + b"\x00" + parts[i + 1])
            i += 2
            continue
        if rec:
            out.append(rec)
        i += 1
    return sorted(out)


def without(records: Iterable[bytes], paths: Iterable[bytes]) -> list[bytes]:
    drop = set(paths)
    return sorted(r for r in records if not any(r.endswith(b" " + p) for p in drop))


def digest_without(d: dict, prefixes: Iterable[bytes]) -> dict:
    pre = tuple(prefixes)
    return {k: v for k, v in d.items() if not any(k == p or k.startswith(p + b"/") for p in pre)}


def assert_same_state(src: RepoBuilder, dst: RepoBuilder, *, excluded: Sequence[bytes] = (), ignored=()) -> None:
    """HEAD, refs, index, status, stash and work-tree bytes equal (excluded/ignored paths aside)."""
    assert dst.out("symbolic-ref", "-q", "HEAD") == src.out("symbolic-ref", "-q", "HEAD")
    assert dst.head() == src.head()
    assert dst.refs() == src.refs()
    assert dst.index_entries() == src.index_entries()
    assert without(status_records(dst.status()), excluded) == without(status_records(src.status()), excluded)
    assert dst.stash_list() == src.stash_list()
    a = digest_without(src.worktree_digest(), [*excluded, *ignored])
    b = digest_without(dst.worktree_digest(), [*excluded, *ignored])
    assert b == a


def env_facts(home: Path, *, workspace_root: Path, case_insensitive: bool = False, os_family: str = "linux"):
    host = HostFacts(
        is_wsl=False,
        is_container=False,
        profile="default",
        multiplex=False,
        hermes_version="0.22.6",
        plugin_version="0.1.0",
        model=None,
        provider=None,
        context_length=None,
        terminal_backend="local",
        hermes_home=str(home / ".hermes"),
        document_cache=str(home / ".hermes" / "cache" / "documents"),
    )
    return EnvFacts(  # noqa: S604 - "shell" is an EnvFacts field, no process is started
        instance_name="t",
        os_family=os_family,
        os_name=os_family,
        os_version="1",
        arch="x86_64",
        hostname="h",
        user="u",
        home=str(home),
        shell="sh",
        host=host,
        tools={},
        tools_seen={},
        workspace_root=str(workspace_root),
        allow_roots=(str(workspace_root),),
        disk_free={},
        case_insensitive={str(workspace_root): case_insensitive},
        offline_capable=True,
        locale="C",
        timezone="UTC",
        git_version="2.56.0",
        git_lfs=False,
    )


def write_global_gitconfig(home: Path, text: str) -> Path:
    """The isolated test user's ``~/.gitconfig`` (filters, signing switches — never an identity)."""
    assert "[user]" not in text and "email" not in text.lower()
    path = home / ".gitconfig"
    path.write_text(text, encoding="utf-8")
    return path


def append_repo_config(repo: Path, text: str) -> None:
    """Append raw text to a test repository's ``.git/config`` (hostile or user settings — never an identity)."""
    assert "[user]" not in text and "email" not in text.lower()
    with open(Path(repo) / ".git" / "config", "a", encoding="utf-8") as fh:
        fh.write(text)


def marker_script(path: Path, marker_dir: Path, name: str) -> Path:
    """An executable that touches ``marker_dir/name`` and passes stdin through (filter/hook stand-in)."""
    path.write_text(f'#!/bin/sh\ntouch "{marker_dir}/{name}"\ncat\n', encoding="utf-8")
    os.chmod(path, 0o755)  # noqa: S103 - executable test fixture
    return path


class FailingGit(gitops.Git):
    """A real ``Git`` whose runs of the named subcommands fail like a git process that died (``rc``, no output,
    an error line) — for the success sweep (G′): a failed git answer must never read as a positive one.
    ``fail`` holds word sequences matched anywhere in the argv (``("stash", "drop")``); ``failed`` records hits."""

    def __init__(self, *fail: tuple[str, ...], rc: int = 128):
        super().__init__()
        self.fail = [tuple(f) for f in fail]
        self.rc = rc
        self.failed: list[tuple[str, ...]] = []

    def _hit(self, args) -> tuple[str, ...] | None:
        words = [os.fsdecode(a) if isinstance(a, bytes) else a for a in args]
        for f in self.fail:
            if any(tuple(words[i : i + len(f)]) == f for i in range(len(words) - len(f) + 1)):
                return f
        return None

    def run(self, args, *, cwd, spec, check: bool = True, **kw):
        hit = self._hit(args)
        if hit is None:
            return super().run(args, cwd=cwd, spec=spec, check=check, **kw)
        self.failed.append(hit)
        err = b"fatal: injected failure"
        if check:
            raise self._error(args, self.rc, err)
        return subprocess.CompletedProcess([self.exe, *args], self.rc, b"", err)
