"""Ignore rules for file trees (layer L1, stdlib only; DESIGN §8.2 step 5, §9.6, INTERFACES §3.5).

Used for non-git trees and extra directories (git repositories use git's own ignore handling, cluster E).

Precedence, lowest to highest: ``.gitignore`` of a directory, then that directory's ``.handoverignore``,
deeper directories over shallower ones, then ``extra`` (the ``extra_ignore`` setting); within one list the last
matching pattern wins and ``!`` re-includes. ``BUILTIN_IGNORES`` always apply and cannot be negated (VCS
directories, dependency and build output, caches). A path below an ignored directory is ignored (git's rule:
a file cannot be re-included when a parent directory is excluded).

Supported gitignore subset: blank lines and ``#`` comments, ``\\#``/``\\!`` escapes, trailing-space trimming
(``\\ `` keeps one), ``!`` negation, trailing ``/`` (directories only), a leading or inner ``/`` anchors the
pattern to its file's directory, ``*``, ``?``, ``[...]`` (``[!...]``/``[^...]``), ``**`` as ``**/x``, ``x/**``
and ``a/**/b``. Ignore files are read lazily, only when they are regular files (never through a symlink), and
only up to ``MAX_IGNORE_FILE_BYTES``.
"""

from __future__ import annotations

import os
import re
import stat
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path

BUILTIN_IGNORES: tuple[str, ...] = (
    ".git",
    ".hg/",
    ".svn/",
    ".bzr/",
    "node_modules/",
    "__pycache__/",
    ".venv/",
    "venv/",
    ".tox/",
    ".mypy_cache/",
    ".pytest_cache/",
    ".ruff_cache/",
    "dist/",
    "build/",
    "target/",
    ".gradle/",
    ".next/",
    ".cache/",
    "*.pyc",
    ".DS_Store",
    "Thumbs.db",
)
"""DESIGN §8.2 step 5 (``.git`` without a slash also covers a worktree's ``.git`` file)."""

IGNORE_FILE_NAMES: tuple[str, ...] = (".gitignore", ".handoverignore")
MAX_IGNORE_FILE_BYTES = 1024 * 1024
MAX_PATTERNS_PER_FILE = 10_000


@dataclass(frozen=True)
class _Rule:
    regex: re.Pattern[str]
    negate: bool
    dir_only: bool


def _translate_glob(glob: str) -> str:
    """Regex for one gitignore glob segment sequence (``/`` already handled by the caller)."""
    out: list[str] = []
    i, n = 0, len(glob)
    while i < n:
        ch = glob[i]
        if ch == "*":
            if glob.startswith("**", i):
                before = i == 0 or glob[i - 1] == "/"
                after = i + 2 == n or glob[i + 2] == "/"
                if before and after:
                    if i + 2 == n:
                        out.append(".*")
                        i += 2
                        continue
                    out.append("(?:.*/)?")
                    i += 3
                    continue
            out.append("[^/]*")
        elif ch == "?":
            out.append("[^/]")
        elif ch == "[":
            j = i + 1
            if j < n and glob[j] in "!^":
                j += 1
            if j < n and glob[j] == "]":
                j += 1
            while j < n and glob[j] != "]":
                j += 1
            if j >= n:
                out.append(re.escape(ch))
            else:
                body = glob[i + 1 : j]
                negated = body[:1] in {"!", "^"}
                if negated:
                    body = body[1:]
                escaped = "".join("\\" + c if c in "\\[]&~|^" else c for c in body)
                out.append(f"(?!/)[{'^' if negated else ''}{escaped}]")
                i = j
        elif ch == "\\" and i + 1 < n:
            i += 1
            out.append(re.escape(glob[i]))
        else:
            out.append(re.escape(ch))
        i += 1
    return "".join(out)


def _compile(line: str) -> _Rule | None:
    """One ignore-file line → rule (None for blank lines and comments)."""
    if line.endswith("\r"):
        line = line[:-1]
    if not line or line.startswith("#"):
        return None
    stripped = line.rstrip(" ")
    if stripped.endswith("\\") and len(stripped) < len(line):
        stripped += " "
    line = stripped
    negate = False
    if line.startswith("!"):
        negate, line = True, line[1:]
    elif line.startswith(("\\!", "\\#")):
        line = line[1:]
    if not line:
        return None
    dir_only = line.endswith("/")
    line = line.rstrip("/")
    if not line:
        return None
    anchored = "/" in line
    line = line.lstrip("/")
    body = _translate_glob(line)
    prefix = "" if anchored else "(?:.*/)?"
    return _Rule(regex=re.compile(f"^{prefix}{body}$", re.DOTALL), negate=negate, dir_only=dir_only)


def _compile_all(lines: Sequence[str]) -> tuple[_Rule, ...]:
    rules = []
    for line in lines[:MAX_PATTERNS_PER_FILE]:
        rule = _compile(line)
        if rule is not None:
            rules.append(rule)
    return tuple(rules)


def _match(rules: Sequence[_Rule], rel: str, is_dir: bool) -> bool | None:
    """True = ignored, False = re-included, None = no rule matched (last match wins)."""
    verdict: bool | None = None
    for rule in rules:
        if rule.dir_only and not is_dir:
            continue
        if rule.regex.match(rel):
            verdict = not rule.negate
    return verdict


class IgnoreMatcher:
    """Matcher over ``/``-separated paths relative to a tree root (see module docstring)."""

    def __init__(self, patterns: Sequence[str]):
        """A matcher with the built-in ignores plus ``patterns`` (gitignore syntax, relative to the root)."""
        self._builtin = _compile_all(BUILTIN_IGNORES)
        self._extra = _compile_all(list(patterns))
        self._root: Path | None = None
        self._per_dir: dict[str, tuple[_Rule, ...]] = {}
        self._dir_cache: dict[str, bool] = {}

    @classmethod
    def for_tree(cls, root: Path, extra: Sequence[str]) -> IgnoreMatcher:
        """Matcher for the tree at ``root``: built-ins, every directory's ``.gitignore`` and ``.handoverignore``
        (read lazily), then ``extra``."""
        matcher = cls(extra)
        matcher._root = Path(root)
        return matcher

    def _read_rules(self, directory: str) -> tuple[_Rule, ...]:
        cached = self._per_dir.get(directory)
        if cached is not None:
            return cached
        rules: list[_Rule] = []
        if self._root is not None:
            base = self._root / directory if directory else self._root
            for name in IGNORE_FILE_NAMES:
                rules.extend(_compile_all(_read_ignore_file(base / name)))
        result = tuple(rules)
        self._per_dir[directory] = result
        return result

    def _ignored_here(self, rel: str, is_dir: bool) -> bool:
        if _match(self._builtin, rel, is_dir):
            return True
        verdict: bool | None = None
        if self._root is not None:
            parts = rel.split("/")
            for depth in range(len(parts)):
                directory = "/".join(parts[:depth])
                sub_rel = "/".join(parts[depth:])
                found = _match(self._read_rules(directory), sub_rel, is_dir)
                if found is not None:
                    verdict = found
        found = _match(self._extra, rel, is_dir)
        if found is not None:
            verdict = found
        return bool(verdict)

    def ignored(self, rel: str, is_dir: bool) -> bool:
        """True when ``rel`` (``/``-separated, relative to the root) is ignored, itself or through a parent."""
        rel = rel.strip("/")
        if not rel:
            return False
        parts = rel.split("/")
        for depth in range(1, len(parts)):
            parent = "/".join(parts[:depth])
            cached = self._dir_cache.get(parent)
            if cached is None:
                cached = self._ignored_here(parent, True)
                self._dir_cache[parent] = cached
            if cached:
                return True
        return self._ignored_here(rel, is_dir)


def _read_ignore_file(path: Path) -> list[str]:
    """Lines of a regular ignore file (never followed through a symlink, at most ``MAX_IGNORE_FILE_BYTES``)."""
    try:
        st = os.lstat(path)
    except OSError:
        return []
    if not stat.S_ISREG(st.st_mode) or st.st_size > MAX_IGNORE_FILE_BYTES:
        return []
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        return []
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            return []
        data = os.read(fd, MAX_IGNORE_FILE_BYTES + 1)[:MAX_IGNORE_FILE_BYTES]
    finally:
        os.close(fd)
    return data.decode("utf-8", errors="replace").split("\n")
