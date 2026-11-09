"""Test repositories built through the plugin's own git helper (cluster E).

House rule (BUILD-ORDER §0, R5): no test types a git identity into a shell command or writes one into any git
config. Every git call here goes through ``gitops.Git`` with the ``received`` profile — the user's global and
system configuration (on the build host: commit signing, ``submodule.recurse``, …) are nulled per process — and
every commit, tag or stash gets the plugin's technical identity and a fixed time from
``GitEnvSpec.technical_identity_at``, only in the environment of that one subprocess.

``RepoBuilder`` writes files with plain ``os`` calls and drives git for staging, commits, branches, tags,
stashes and renames, so a fixture repository is built exactly the way a user's repository would be.
"""

from __future__ import annotations

import hashlib
import os
from collections.abc import Iterable, Sequence
from pathlib import Path

from ctx_handover.engine.gitops import RECEIVED, Git, GitEnvSpec

BASE_TIME = 1_762_646_400.0  # 2025-11-09T00:00:00Z


class RepoBuilder:
    """One repository under construction (work tree ``root``)."""

    def __init__(self, root: Path, *, git: Git | None = None, at: float = BASE_TIME):
        self.root = Path(root)
        self.git = git or Git()
        self.at = float(at)

    # -- git plumbing -----------------------------------------------------------------------------------------

    def _spec(self, identity: bool = False) -> GitEnvSpec:
        if identity:
            self.at += 60
            return GitEnvSpec(profile="received", ceiling=self.root.parent, technical_identity_at=self.at)
        return GitEnvSpec(profile="received", ceiling=self.root.parent)

    def run(self, *args: str | bytes, identity: bool = False, check: bool = True, env_extra: dict | None = None):
        """Run git in the work tree (received profile; technical identity on request)."""
        return self.git.run(list(args), cwd=self.root, spec=self._spec(identity), check=check, env_extra=env_extra)

    def out(self, *args: str | bytes) -> str:
        return self.run(*args).stdout.decode("utf-8", errors="surrogateescape").strip()

    # -- construction -----------------------------------------------------------------------------------------

    @classmethod
    def init(
        cls, root: Path, *, branch: str = "main", object_format: str = "sha1", git: Git | None = None
    ) -> RepoBuilder:
        root = Path(root)
        root.mkdir(parents=True, exist_ok=True)
        builder = cls(root, git=git)
        builder.git.run(
            ["init", "-q", "--template=", f"--object-format={object_format}", "-b", branch, str(root)],
            cwd=root.parent,
            spec=RECEIVED,
        )
        return builder

    def write(self, rel: str | bytes, content: bytes | str, *, mode: int | None = None) -> Path:
        path = self.path(rel)
        path.parent.mkdir(parents=True, exist_ok=True)
        data = content.encode("utf-8") if isinstance(content, str) else content
        with open(path, "wb") as fh:
            fh.write(data)
        if mode is not None:
            os.chmod(path, mode)
        return path

    def symlink(self, rel: str | bytes, target: str) -> Path:
        path = self.path(rel)
        path.parent.mkdir(parents=True, exist_ok=True)
        os.symlink(target, path)
        return path

    def path(self, rel: str | bytes) -> Path:
        if isinstance(rel, bytes):
            return Path(os.fsdecode(os.path.join(os.fsencode(self.root), rel)))
        return self.root / rel

    def remove(self, rel: str | bytes) -> None:
        os.unlink(self.path(rel))

    def add(self, *paths: str | bytes) -> None:
        self.run("add", "--", *(paths or (".",)))

    def add_all(self) -> None:
        self.run("add", "-A")

    def rm(self, *paths: str) -> None:
        self.run("rm", "-q", "--", *paths)

    def mv(self, src: str, dst: str) -> None:
        self.run("mv", "--", src, dst)

    def commit(self, message: str = "commit", *, allow_empty: bool = False) -> str:
        args = ["commit", "-q", "--no-verify", "-m", message]
        if allow_empty:
            args.insert(2, "--allow-empty")
        self.run(*args, identity=True)
        return self.head()

    def commit_all(self, message: str = "commit") -> str:
        self.add_all()
        return self.commit(message)

    def tag(self, name: str, *, annotated: bool = False, ref: str = "HEAD") -> None:
        if annotated:
            self.run("tag", "-a", "-m", f"tag {name}", name, ref, identity=True)
        else:
            self.run("tag", name, ref)

    def branch(self, name: str, ref: str = "HEAD") -> None:
        self.run("branch", name, ref)

    def checkout(self, ref: str, *, new: bool = False) -> None:
        self.run("checkout", "-q", *(["-b"] if new else []), ref)

    def stash(self, message: str = "wip", *, untracked: bool = False, paths: Sequence[str] = ()) -> None:
        args = ["stash", "push", "-q", "-m", message]
        if untracked:
            args.append("-u")
        if paths:
            args += ["--", *paths]
        self.run(*args, identity=True)

    def head(self) -> str:
        return self.out("rev-parse", "HEAD")

    def rev(self, ref: str) -> str:
        return self.out("rev-parse", ref)

    # -- observation (what the acceptance tests compare) ------------------------------------------------------

    def status(self) -> bytes:
        """``git status --porcelain=v2 -z --untracked-files=all`` with the per-line object ids kept."""
        return self.run("status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=none").stdout

    def index_entries(self) -> bytes:
        """``git ls-files -s -z`` (mode, object, stage, path for every index entry)."""
        return self.run("ls-files", "-s", "-z").stdout

    def refs(self) -> dict[str, str]:
        text = self.out("for-each-ref", "--format=%(refname) %(objectname)")
        return dict(line.split(" ", 1) for line in text.split("\n") if line)

    def stash_list(self) -> list[tuple[str, str]]:
        text = self.run("stash", "list", "-z", "--format=%H %gs", check=False).stdout.decode()
        return [tuple(e.split(" ", 1)) for e in text.split("\x00") if e]  # type: ignore[misc]

    def worktree_digest(self, *, exclude: Iterable[bytes] = (b".git",)) -> dict[bytes, tuple[str, str]]:
        """``{raw rel path: (kind+mode, sha256 or link target)}`` of the work tree (no ``.git``)."""
        return tree_digest(self.root, exclude=exclude)


def tree_digest(root: Path, *, exclude: Iterable[bytes] = (b".git",)) -> dict[bytes, tuple[str, str]]:
    """Byte-level description of a directory tree (files: mode + sha256, links: target, dirs: mode)."""
    skip = set(exclude)
    out: dict[bytes, tuple[str, str]] = {}
    base = os.fsencode(root)
    stack = [b""]
    while stack:
        rel = stack.pop()
        full = os.path.join(base, rel) if rel else base
        with os.scandir(full) as it:
            for entry in it:
                name = entry.name
                child = os.path.join(rel, name) if rel else name
                if child in skip:
                    continue
                st = entry.stat(follow_symlinks=False)
                if entry.is_symlink():
                    out[child] = ("link", os.fsdecode(os.readlink(entry.path)))
                elif entry.is_dir(follow_symlinks=False):
                    out[child] = (f"dir {st.st_mode & 0o777:o}", "")
                    stack.append(child)
                elif not entry.is_file(follow_symlinks=False):
                    out[child] = ("special", "")
                else:
                    h = hashlib.sha256()
                    with open(entry.path, "rb") as fh:
                        for chunk in iter(lambda: fh.read(1 << 20), b""):
                            h.update(chunk)
                    out[child] = (f"file {st.st_mode & 0o777:o}", h.hexdigest())
    return out


def dir_fingerprint(root: Path) -> dict[str, tuple[int, int, str]]:
    """Every path below ``root`` (``.git`` included) with ``(st_mode, size, sha256 of content or link)`` — proves
    a repository was not modified (refs, index, objects, config, work tree)."""
    out: dict[str, tuple[int, int, str]] = {}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for name in sorted(dirnames + filenames):
            full = os.path.join(dirpath, name)
            st = os.lstat(full)
            rel = os.path.relpath(full, root)
            if os.path.islink(full):
                out[rel] = (st.st_mode, st.st_size, os.readlink(full))
            elif os.path.isfile(full) and not os.path.islink(full):
                h = hashlib.sha256()
                with open(full, "rb") as fh:
                    for chunk in iter(lambda: fh.read(1 << 20), b""):
                        h.update(chunk)
                out[rel] = (st.st_mode, st.st_size, h.hexdigest())
            else:
                out[rel] = (st.st_mode, 0, "")
    return out
