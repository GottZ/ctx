"""Hardened git runner, read-only probes and object streaming helpers (layer L3, stdlib only).

DESIGN §9.2 (runner, profiles, forced options, version), §9.3 step 4–5 (temp-index state), §9.5 step 3/5
(ref and remote validation), §4.6 (objects as blocks); INTERFACES §4.3.

Every git call of the plugin goes through ``Git.run`` (or its streaming siblings): no shell, stdin closed unless
the plugin feeds it, a timeout, an environment built by ``Git.env``. That environment

* drops every inherited ``GIT_*`` variable and sets only the ones the plugin needs,
* forces the options of ``FORCED_OPTIONS`` through ``GIT_CONFIG_COUNT``/``KEY_n``/``VALUE_n`` — per process, never
  written to any config file,
* in the ``received`` profile additionally nulls the user's global and system configuration (only the
  ``SAFELIST`` keys come back, read once per process) and empties every filter driver it knows about,
* carries the technical identity and the fixed time of a synthetic commit only when the caller asks for it
  (``GitEnvSpec.technical_identity_at``, R5) — in the environment of that one subprocess.

Objects (pack streams, file contents, tree manifests) travel as content-addressed blocks through the
``contracts.BlockSink``/``BlockSource`` protocols; ``put_stream``/``iter_object`` never hold more than one batch of
blocks in memory.
"""

from __future__ import annotations

import base64
import contextlib
import hashlib
import os
import re
import shutil
import stat
import subprocess
import tempfile
import threading
import time
from collections.abc import Callable, Iterable, Iterator, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import IO

from .contracts import BlockSink, BlockSource
from .errors import BlockCorrupt, Cancelled, CapabilityMissing, ConflictError, HandoverError, NotFound, PolicyDenied
from .jsonutil import doc_id, loads_bounded
from .model import GitInfo, GitState, ObjectManifest
from .paths import strip_url_userinfo
from .policy import PolicyConfig, is_denied_name

MIN_GIT = (2, 38)
"""Hard minimum (``safe.bareRepository``, ``--pathspec-from-file``, bundle v3, SHA-256 objects; DESIGN §9.2)."""

PATCHED_LINES: tuple[tuple[int, int, int], ...] = (
    (2, 39, 4),
    (2, 40, 2),
    (2, 41, 1),
    (2, 42, 2),
    (2, 43, 7),
    (2, 44, 4),
    (2, 45, 4),
    (2, 46, 4),
    (2, 47, 3),
    (2, 48, 2),
    (2, 49, 1),
    (2, 50, 1),
)
"""Per release line the first release carrying the 2024 checkout/clone fixes (2.39.4 … 2.45.1) and the
CVE-2025-48384 fixes (2.43.7 … 2.50.1), whichever is later. Lines from 2.51 on carry both. Doctor warning only:
distributions backport, the number alone is not proof (DESIGN §9.2)."""

TECH_IDENTITY = ("ctx_handover", "ctx_handover@handover.invalid")
"""Author and committer of the synthetic index/work-tree commits (R5); never lands on a user ref."""

FSCK_ERROR_IDS: tuple[str, ...] = (
    "hasDotgit",
    "hasDot",
    "hasDotdot",
    "fullPathname",
    "emptyName",
    "nullSha1",
    "zeroPaddedFilemode",
    "badFilemode",
    "gitattributesSymlink",
    "gitignoreSymlink",
    "gitmodulesSymlink",
    "mailmapSymlink",
)
"""fsck message ids raised to ``error`` in the quarantine (git-fsck(1) "FSCK MESSAGES": WARN/INFO by default)."""

FORCED_OPTIONS: tuple[tuple[str, str], ...] = (
    ("credential.helper", ""),
    ("core.askPass", ""),
    ("core.fsmonitor", "false"),
    ("core.untrackedCache", "false"),
    ("core.hooksPath", os.devnull),
    ("core.pager", "cat"),
    ("core.editor", "true"),
    ("sequence.editor", "true"),
    ("diff.external", ""),
    ("core.sshCommand", "ssh -o BatchMode=yes"),
    ("submodule.recurse", "false"),
    ("fetch.recurseSubmodules", "false"),
    ("gc.auto", "0"),
    ("maintenance.auto", "false"),
    ("commit.gpgSign", "false"),
    ("tag.gpgSign", "false"),
    ("safe.bareRepository", "explicit"),
    ("protocol.allow", "never"),
    ("core.protectNTFS", "true"),
    ("core.protectHFS", "true"),
    ("transfer.fsckObjects", "true"),
    ("fetch.fsckObjects", "true"),
    ("core.bigFileThreshold", "32m"),
)
"""DESIGN §9.2 — forced on every invocation, both profiles. ``core.bigFileThreshold=32m`` (addition) makes git
stream large blobs instead of holding them for delta search: index-pack of a 256 MiB blob 262 → 13 MiB RSS,
pack-objects 533 → 277 MiB (measured with git 2.56, cluster E report)."""

RECEIVED_EXTRA_OPTIONS: tuple[tuple[str, str], ...] = (("core.attributesFile", os.devnull),)
"""Received profile only: the user's global attributes file (read by default from ``$XDG_CONFIG_HOME/git``
even when the global config is nulled) must not select conversions for a received tree."""

SAFELIST: tuple[str, ...] = (
    "core.autocrlf",
    "core.eol",
    "core.safecrlf",
    "core.symlinks",
    "core.ignorecase",
    "core.precomposeUnicode",
    "core.longpaths",
    "safe.directory",
)
"""User values passed back into the received profile (DESIGN §9.2)."""

SET_ENV: dict[str, str] = {
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

STATE_MESSAGES = {"index": b"ctx_handover index state\n", "worktree": b"ctx_handover work-tree state\n"}

BLOCK_SIZE = 524_288
"""Block size of every object the workspace cluster writes (DESIGN §4.6)."""
OBJECT_JSON_CAP = 64 * 1024**2
"""Receiver cap for object JSON (tree manifests; DESIGN §10.2)."""

REF_NAMESPACES = ("refs/heads/", "refs/tags/", "refs/remotes/", "refs/notes/")
_CTXH_REF_RE = re.compile(r"refs/ctxh/(?:head|index|worktree|stash/[0-9]{1,6})")
_SHA_RE = re.compile(r"[0-9a-f]{40}|[0-9a-f]{64}")
_REMOTE_NAME_RE = re.compile(r"[A-Za-z0-9._-]{1,64}")
_URL_SCHEMES = frozenset({"https", "http", "ssh", "git"})
_SCP_RE = re.compile(r"^(?:(?P<user>[A-Za-z0-9._~+-]+)@)?(?P<host>[A-Za-z0-9._-]+|\[[0-9A-Fa-f:.]+\]):(?P<path>.+)$")
_VERSION_RE = re.compile(rb"(\d+)\.(\d+)(?:\.(\d+))?")
_FILTER_ATTR_RE = re.compile(rb"(?:^|\s)filter=([^\s]+)")
_FILTER_NAME_RE = re.compile(r"[A-Za-z0-9._+-]{1,128}")
_STDERR_CAP = 2048


def _never() -> bool:
    return False


class GitCommandError(HandoverError):
    """A git invocation failed (code ``internal``; ``details``: subcommand, rc, stderr tail)."""

    retryable = False


def is_sha(value: object) -> bool:
    """True for a lower-case 40 (SHA-1) or 64 (SHA-256) hex object id."""
    return isinstance(value, str) and _SHA_RE.fullmatch(value) is not None


def require_sha(value: object, what: str) -> str:
    """``value`` if it is an object id, else ``PolicyDenied`` (data from a peer is never passed to git unchecked)."""
    if not is_sha(value):
        raise PolicyDenied(f"invalid object id for {what}", reason="special", field=what)
    return value  # type: ignore[return-value]


def display_path(raw: bytes) -> str:
    """Printable form of a raw path (non-UTF-8 bytes as ``\\xNN``)."""
    return raw.decode("utf-8", errors="backslashreplace")


def encode_pathspec(raw_path: bytes) -> str:
    """A transportable ``:(exclude,literal)`` pathspec for a repo-relative raw path: plain text when the path is
    UTF-8, else ``b64:`` + base64 of the pathspec bytes (JSON cannot carry raw bytes)."""
    spec = b":(exclude,literal)" + raw_path
    try:
        return spec.decode("utf-8")
    except UnicodeDecodeError:
        return "b64:" + base64.b64encode(spec).decode("ascii")


def decode_pathspec(spec: str) -> bytes:
    """Inverse of ``encode_pathspec``; only ``:(exclude,literal)`` pathspecs are accepted."""
    if not isinstance(spec, str):
        raise PolicyDenied("invalid pathspec", reason="special")
    raw = base64.b64decode(spec[4:], validate=True) if spec.startswith("b64:") else spec.encode("utf-8")
    if not raw.startswith(b":(exclude,literal)") or b"\x00" in raw or len(raw) <= len(b":(exclude,literal)"):
        raise PolicyDenied("invalid pathspec", reason="special")
    return raw


def pathspec_path(spec: str) -> bytes:
    """The raw repo-relative path of an encoded exclusion pathspec."""
    return decode_pathspec(spec)[len(b":(exclude,literal)") :]


# ---------------------------------------------------------------------------------------------------------
# Environment spec and runner
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class GitEnvSpec:
    """How one git process is configured (DESIGN §9.2).

    ``profile``: ``"source"`` (the user's own repository on the source; user config stays active so ``git add``
    produces the objects the user's own ``git add`` would) or ``"received"`` (everything touching received
    objects or plugin-created repositories: global/system config nulled, safelist passed back, filters emptied).
    ``filters_from``: a repository (work tree or git dir) whose local-config filter drivers are emptied;
    ``filter_names``: further driver names (e.g. from a received tree's ``.gitattributes``).
    ``file_protocol`` allows ``protocol.file`` (explicit local transfer only). ``technical_identity_at`` sets the
    technical identity and that fixed time for a synthetic commit (R5).
    """

    profile: str = "received"
    ceiling: Path | None = None
    filters_from: Path | None = None
    file_protocol: bool = False
    windows_long_paths: bool = False
    technical_identity_at: float | None = None
    filter_names: tuple[str, ...] = ()


SOURCE = GitEnvSpec(profile="source")
RECEIVED = GitEnvSpec(profile="received")

_VERSION_CACHE: dict[str, tuple[int, int, int] | None] = {}
_SAFELIST_CACHE: dict[str, dict[str, list[str]]] = {}
_CACHE_LOCK = threading.Lock()


def _strip_git_vars(env: dict[str, str]) -> dict[str, str]:
    return {k: v for k, v in env.items() if not k.upper().startswith("GIT_")}


class Git:
    """The plugin's only way to run git (see module docstring)."""

    def __init__(self, git_exe: str | None = None, timeout_s: float = 600):
        exe = git_exe or shutil.which("git") or "git"
        self.exe = str(exe)
        self.timeout_s = float(timeout_s)

    # -- version ------------------------------------------------------------------------------------------

    def version(self) -> tuple[int, int, int] | None:
        """``(major, minor, patch)`` of the git executable, or None when it cannot run (cached per process)."""
        with _CACHE_LOCK:
            if self.exe in _VERSION_CACHE:
                return _VERSION_CACHE[self.exe]
        found: tuple[int, int, int] | None = None
        try:
            proc = self.run(["version"], cwd=None, spec=SOURCE, check=False, timeout=30)
            match = _VERSION_RE.search(proc.stdout or b"") if proc.returncode == 0 else None
            if match:
                found = (int(match.group(1)), int(match.group(2)), int(match.group(3) or 0))
        except (OSError, subprocess.SubprocessError, GitCommandError):
            found = None
        with _CACHE_LOCK:
            _VERSION_CACHE[self.exe] = found
        return found

    def ensure_supported(self) -> tuple[int, int, int]:
        """The version, or ``CapabilityMissing(git.bundle.v1)`` below ``MIN_GIT`` / without git (DESIGN §9.2)."""
        ver = self.version()
        if ver is None or ver[:2] < MIN_GIT:
            shown = ".".join(map(str, ver)) if ver else "none"
            raise CapabilityMissing(
                f"git {shown} is below the minimum {MIN_GIT[0]}.{MIN_GIT[1]}", capability="git.bundle.v1", git=shown
            )
        return ver

    # -- environment --------------------------------------------------------------------------------------

    @staticmethod
    def _base_env() -> dict[str, str]:
        env = _strip_git_vars(dict(os.environ))
        env.update(SET_ENV)
        return env

    def env(self, spec: GitEnvSpec) -> dict[str, str]:
        """The full environment of one git process for ``spec`` (DESIGN §9.2). Never writes any config."""
        if spec.profile not in {"source", "received"}:
            raise ValueError(f"unknown git profile {spec.profile!r}")
        env = self._base_env()
        pairs: list[tuple[str, str]] = []
        if spec.profile == "received":
            env["GIT_CONFIG_GLOBAL"] = os.devnull
            env["GIT_CONFIG_SYSTEM"] = os.devnull
            env["GIT_CONFIG_NOSYSTEM"] = "1"
            for key, values in user_safelist(self).items():
                pairs.extend((key, v) for v in values)
            pairs.extend(RECEIVED_EXTRA_OPTIONS)
        pairs.extend(FORCED_OPTIONS)
        if spec.file_protocol:
            pairs.append(("protocol.file.allow", "always"))
        if spec.windows_long_paths:
            pairs.append(("core.longpaths", "true"))
        if spec.profile == "received":
            names = set(spec.filter_names)
            if spec.filters_from is not None:
                names |= set(local_filter_names(self, Path(spec.filters_from)))
            for name in sorted(n for n in names if _FILTER_NAME_RE.fullmatch(n)):
                pairs.extend(
                    [
                        (f"filter.{name}.clean", ""),
                        (f"filter.{name}.smudge", ""),
                        (f"filter.{name}.process", ""),
                        (f"filter.{name}.required", "false"),
                    ]
                )
        env["GIT_CONFIG_COUNT"] = str(len(pairs))
        for i, (key, value) in enumerate(pairs):
            env[f"GIT_CONFIG_KEY_{i}"] = key
            env[f"GIT_CONFIG_VALUE_{i}"] = value
        if spec.ceiling is not None:
            env["GIT_CEILING_DIRECTORIES"] = str(spec.ceiling)
        if spec.technical_identity_at is not None:
            when = f"@{int(spec.technical_identity_at)} +0000"
            name, email = TECH_IDENTITY
            env.update(
                GIT_AUTHOR_NAME=name,
                GIT_AUTHOR_EMAIL=email,
                GIT_AUTHOR_DATE=when,
                GIT_COMMITTER_NAME=name,
                GIT_COMMITTER_EMAIL=email,
                GIT_COMMITTER_DATE=when,
            )
        return env

    # -- running ------------------------------------------------------------------------------------------

    def _argv(self, args: Sequence[str | bytes]) -> list[str | bytes]:
        if not args or not all(isinstance(a, (str, bytes)) for a in args):
            raise ValueError("git arguments must be str or bytes")
        return [self.exe, *args]

    def _error(self, args: Sequence[str | bytes], rc: int, stderr: bytes) -> GitCommandError:
        sub, skip = "?", False
        for a in args:
            if skip:
                skip = False
                continue
            if a in ("--git-dir", "-c", "-C"):
                skip = True
                continue
            if isinstance(a, str) and not a.startswith("-"):
                sub = a
                break
        tail = (stderr or b"")[-_STDERR_CAP:].decode("utf-8", errors="replace").strip()
        return GitCommandError(f"git {sub} failed (rc {rc}): {tail[:300]}", subcommand=sub, rc=rc, stderr=tail)

    def run(
        self,
        args: Sequence[str | bytes],
        *,
        cwd: Path | None,
        spec: GitEnvSpec,
        env_extra: dict | None = None,
        input: bytes | None = None,
        check: bool = True,
        timeout: float | None = None,
        cancelled: Callable[[], bool] = _never,
    ) -> subprocess.CompletedProcess:
        """Run git once (bytes in, bytes out). ``check`` raises ``GitCommandError`` on a non-zero exit;
        ``cancelled`` is polled twice a second and kills the process (``Cancelled``)."""
        if cancelled():
            raise Cancelled("cancelled before git ran")
        env = self.env(spec)
        if env_extra:
            env.update({k: str(v) for k, v in env_extra.items()})
        limit = self.timeout_s if timeout is None else float(timeout)
        proc = subprocess.Popen(  # noqa: S603 - argv list, shell=False
            self._argv(args),
            cwd=str(cwd) if cwd is not None else None,
            env=env,
            stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        deadline = time.monotonic() + limit
        pending = input
        out = err = b""
        while True:
            try:
                out, err = proc.communicate(pending, timeout=min(0.5, max(0.01, deadline - time.monotonic())))
                break
            except subprocess.TimeoutExpired:
                pending = None
                if cancelled():
                    proc.kill()
                    proc.communicate()
                    raise Cancelled("cancelled while git ran") from None
                if time.monotonic() >= deadline:
                    proc.kill()
                    proc.communicate()
                    raise GitCommandError(f"git timed out after {limit:.0f}s", rc=-1, subcommand=str(args[0])) from None
        result = subprocess.CompletedProcess(proc.args, proc.returncode, out, err)
        if check and proc.returncode != 0:
            raise self._error(args, proc.returncode, err)
        return result

    def popen_stdin(self, args: Sequence[str | bytes], *, cwd: Path | None, spec: GitEnvSpec) -> subprocess.Popen:
        """A git process the caller feeds through ``stdin`` (``index-pack --stdin``); stdout/stderr go to
        anonymous temp files so the process never blocks on them."""
        return subprocess.Popen(  # noqa: S603 - argv list, shell=False
            self._argv(args),
            cwd=str(cwd) if cwd is not None else None,
            env=self.env(spec),
            stdin=subprocess.PIPE,
            stdout=tempfile.TemporaryFile(),
            stderr=tempfile.TemporaryFile(),
        )

    def popen_stdout(
        self, args: Sequence[str | bytes], *, cwd: Path | None, spec: GitEnvSpec, input_lines: Iterable[str]
    ) -> subprocess.Popen:
        """A git process whose stdout the caller streams (``pack-objects --stdout``). ``input_lines`` are written
        to stdin (one per line) and stdin is closed before the caller reads."""
        proc = subprocess.Popen(  # noqa: S603 - argv list, shell=False
            self._argv(args),
            cwd=str(cwd) if cwd is not None else None,
            env=self.env(spec),
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=tempfile.TemporaryFile(),  # noqa: SIM115
        )
        assert proc.stdin is not None  # noqa: S101 - PIPE requested
        try:
            for line in input_lines:
                if "\n" in line or "\x00" in line:
                    raise ValueError("input line contains a newline")
                proc.stdin.write(line.encode("utf-8") + b"\n")
        finally:
            with contextlib.suppress(BrokenPipeError):
                proc.stdin.close()
        return proc

    def iter_z(
        self,
        args: Sequence[str | bytes],
        *,
        cwd: Path | None,
        spec: GitEnvSpec,
        cancelled: Callable[[], bool] = _never,
        sep: bytes = b"\x00",
    ) -> Iterator[bytes]:
        """Stream the ``sep``-separated records of a git command's stdout (never the whole output in memory);
        raises ``GitCommandError`` after the last record when git failed."""
        with tempfile.TemporaryFile() as errf:
            proc = subprocess.Popen(  # noqa: S603 - argv list, shell=False
                self._argv(args),
                cwd=str(cwd) if cwd is not None else None,
                env=self.env(spec),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=errf,
            )
            assert proc.stdout is not None  # noqa: S101 - PIPE requested
            buf = b""
            try:
                while True:
                    if cancelled():
                        raise Cancelled("cancelled while reading git output")
                    chunk = proc.stdout.read(65536)
                    if not chunk:
                        break
                    buf += chunk
                    *records, buf = buf.split(sep)
                    yield from records
                if buf:
                    yield buf
            finally:
                if proc.poll() is None:
                    proc.kill()
                proc.stdout.close()
                rc = proc.wait()
            if rc != 0:
                errf.seek(0)
                raise self._error(args, rc, errf.read()[-_STDERR_CAP:])

    def out(self, args: Sequence[str | bytes], *, cwd: Path | None, spec: GitEnvSpec, **kw) -> str:
        """stdout of a successful run, decoded and stripped (short outputs only)."""
        return self.run(args, cwd=cwd, spec=spec, **kw).stdout.decode("utf-8", errors="surrogateescape").strip()


# ---------------------------------------------------------------------------------------------------------
# Safelist, filters, version
# ---------------------------------------------------------------------------------------------------------


def user_safelist(git: Git) -> dict[str, list[str]]:
    """The user's global/system values of ``SAFELIST`` keys (system first, global wins), read once per process
    with ``git config --system/--global --get-all`` (DESIGN §9.2). Multi-valued keys keep every value."""
    with _CACHE_LOCK:
        cached = _SAFELIST_CACHE.get(git.exe)
    if cached is not None:
        return {k: list(v) for k, v in cached.items()}
    found: dict[str, list[str]] = {}
    canonical = {k.lower(): k for k in SAFELIST}
    pattern = "^(" + "|".join(re.escape(k.lower()) for k in SAFELIST) + ")$"
    for scope in ("--system", "--global"):
        try:
            proc = git.run(
                ["config", scope, "-z", "--get-regexp", pattern], cwd=None, spec=SOURCE, check=False, timeout=30
            )
        except (OSError, subprocess.SubprocessError, GitCommandError):
            continue
        if proc.returncode != 0:
            continue
        scoped: dict[str, list[str]] = {}
        for rec in proc.stdout.split(b"\x00"):
            if not rec:
                continue
            raw_key, _, value = rec.partition(b"\n")
            key = canonical.get(raw_key.decode("utf-8", errors="replace").lower())
            if key is not None:
                scoped.setdefault(key, []).append(value.decode("utf-8", errors="replace"))
        for key, values in scoped.items():
            if key == "safe.directory":
                found.setdefault(key, []).extend(values)
            else:
                found[key] = [values[-1]]
    with _CACHE_LOCK:
        _SAFELIST_CACHE[git.exe] = found
    return {k: list(v) for k, v in found.items()}


def reset_process_caches() -> None:
    """Forget the per-process version and safelist caches (tests change ``$HOME`` per test)."""
    with _CACHE_LOCK:
        _VERSION_CACHE.clear()
        _SAFELIST_CACHE.clear()


def _config_file_of(path: Path) -> Path | None:
    path = Path(path)
    for candidate in (path / ".git" / "config", path / "config"):
        if candidate.is_file():
            return candidate
    git_file = path / ".git"
    if git_file.is_file():
        try:
            text = git_file.read_text("utf-8", errors="replace").strip()
        except OSError:
            return None
        if text.startswith("gitdir:"):
            gitdir = Path(text[7:].strip())
            if not gitdir.is_absolute():
                gitdir = path / gitdir
            common = gitdir / "commondir"
            if common.is_file():
                rel = common.read_text("utf-8", errors="replace").strip()
                gitdir = (gitdir / rel) if not Path(rel).is_absolute() else Path(rel)
            if (gitdir / "config").is_file():
                return gitdir / "config"
    return None


def local_filter_names(git: Git, repo: Path) -> list[str]:
    """Filter driver names defined in a repository's local config (``filter.<name>.*``), read with
    ``git config --file`` (no hooks, no includes of the user's config)."""
    cfg = _config_file_of(Path(repo))
    if cfg is None:
        return []
    proc = git.run(
        ["config", "--file", str(cfg), "--name-only", "--get-regexp", r"^filter\."],
        cwd=None,
        spec=GitEnvSpec(profile="received"),
        check=False,
    )
    names = set()
    for line in proc.stdout.decode("utf-8", errors="replace").split("\n"):
        if line.startswith("filter.") and line.count(".") >= 2:
            names.add(line[len("filter.") : line.rindex(".")])
    return sorted(names)


def attribute_filter_names(git: Git, gitdir: Path, tree: str) -> list[str]:
    """Filter names named by any ``.gitattributes`` blob of ``tree`` (a received tree, read from objects only)."""
    tree = require_sha(tree, "tree")
    names: set[str] = set()
    blobs: list[str] = []
    for rec in git.iter_z(
        ["--git-dir", str(gitdir), "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", tree],
        cwd=None,
        spec=RECEIVED,
    ):
        meta, _, path = rec.partition(b"\t")
        parts = meta.split()
        if len(parts) == 3 and parts[1] == b"blob" and (path == b".gitattributes" or path.endswith(b"/.gitattributes")):
            blobs.append(parts[2].decode("ascii"))
    for blob in blobs:
        data = git.run(
            ["--git-dir", str(gitdir), "cat-file", "blob", "--end-of-options", blob], cwd=None, spec=RECEIVED
        ).stdout[: 1024 * 1024]
        names.update(m.decode("utf-8", errors="replace") for m in _FILTER_ATTR_RE.findall(data))
    return sorted(n for n in names if _FILTER_NAME_RE.fullmatch(n))


def check_version(git: Git) -> tuple[bool, str | None]:
    """``(meets MIN_GIT, warning)``. The warning names the patched release of the version's line when the
    version is below it (DESIGN §9.2, SEC 3.6); ``doctor`` fails on False and shows the warning."""
    ver = git.version()
    if ver is None:
        return False, "git is not available"
    shown = ".".join(map(str, ver))
    if ver[:2] < MIN_GIT:
        return False, f"git {shown} is below the minimum {MIN_GIT[0]}.{MIN_GIT[1]}"
    latest = max(PATCHED_LINES)
    if ver[:2] > latest[:2]:
        return True, None
    line = next((p for p in PATCHED_LINES if p[:2] == ver[:2]), None)
    if line is not None and ver >= line:
        return True, None
    want = ".".join(map(str, line or latest))
    return True, (
        f"git {shown} predates the security fixes of {want} (2024 checkout/clone CVEs, CVE-2025-48384); "
        "distributions may have backported them"
    )


# ---------------------------------------------------------------------------------------------------------
# Validation of peer data
# ---------------------------------------------------------------------------------------------------------


def validate_ref(git: Git, name: str, *, allow_ctxh: bool = False) -> str:
    """A ref name from a peer: namespace allowlist (``refs/heads|tags|remotes|notes/*``; the plugin's own
    ``refs/ctxh/{head,index,worktree,stash/<n>}`` only with ``allow_ctxh``), no control characters, and
    ``git check-ref-format`` (DESIGN §9.5 step 3). Raises ``PolicyDenied``."""
    if not isinstance(name, str) or not name or len(name) > 1024:
        raise PolicyDenied("invalid ref name", reason="special", ref=str(name)[:200])
    if any(ord(c) < 0x20 or ord(c) == 0x7F for c in name):
        raise PolicyDenied("ref name with control characters", reason="special", ref=repr(name)[:200])
    allowed = name.startswith(REF_NAMESPACES) or (allow_ctxh and _CTXH_REF_RE.fullmatch(name) is not None)
    if not allowed or name.rstrip("/") in {ns.rstrip("/") for ns in REF_NAMESPACES}:
        raise PolicyDenied("ref outside the allowed namespaces", reason="special", ref=name[:200])
    proc = git.run(["check-ref-format", name], cwd=None, spec=RECEIVED, check=False)
    if proc.returncode != 0:
        raise PolicyDenied("malformed ref name", reason="special", ref=name[:200])
    return name


def _has_control(text: str) -> bool:
    return any(ord(c) < 0x20 or ord(c) == 0x7F for c in text)


def validate_remote(name: str, url: str) -> tuple[str, str]:
    """A remote from peer data (DESIGN §9.5 step 5, SEC 3.4, CVE-2025-48384 class): name
    ``^[A-Za-z0-9._-]{1,64}$`` without a leading ``-``/``.``; URL ``https``/``http``/``ssh``/``git`` or scp form,
    no ``<helper>::``, ``ext::``, ``fd::``, ``file:``, local paths, no leading ``-``, no CR/LF/NUL/control
    characters; userinfo stripped (an ssh user name is kept). Returns ``(name, url)`` or raises ``PolicyDenied``."""
    if not isinstance(name, str) or not _REMOTE_NAME_RE.fullmatch(name) or name[0] in "-." or ".." in name:
        raise PolicyDenied("invalid remote name", reason="special", remote=str(name)[:80])
    if not isinstance(url, str) or not url or len(url) > 2048 or _has_control(url):
        raise PolicyDenied("invalid remote url", reason="special", remote=name)
    if url.startswith("-") or "::" in url or url.lower().startswith("file:"):
        raise PolicyDenied("remote url transport not allowed", reason="special", remote=name)
    scheme_match = re.match(r"^([A-Za-z][A-Za-z0-9+.-]*)://", url)
    if scheme_match:
        scheme = scheme_match.group(1).lower()
        if scheme not in _URL_SCHEMES:
            raise PolicyDenied("remote url scheme not allowed", reason="special", remote=name, scheme=scheme)
        rest = url[scheme_match.end() :]
        host = rest.split("/", 1)[0].rsplit("@", 1)[-1]
        if not host or host.startswith("-"):
            raise PolicyDenied("remote url without host", reason="special", remote=name)
        return name, strip_url_userinfo(url)
    scp = _SCP_RE.match(url)
    if scp is None or "/" in url.split(":", 1)[0]:
        raise PolicyDenied("remote url is a local path", reason="special", remote=name)
    host = scp.group("host")
    if len(host) == 1 or host.startswith("-") or scp.group("path").startswith("-"):
        raise PolicyDenied("remote url host not allowed", reason="special", remote=name)
    return name, url


# ---------------------------------------------------------------------------------------------------------
# Repository discovery helpers
# ---------------------------------------------------------------------------------------------------------


def _ceiling(root: Path) -> GitEnvSpec:
    return GitEnvSpec(profile="source", ceiling=Path(root).parent)


def git_dirs(git: Git, root: Path) -> tuple[Path, Path]:
    """``(git_dir, common_dir)`` absolute, of the work tree at ``root`` (linked worktrees included)."""
    spec = _ceiling(root)
    out = git.run(["rev-parse", "--absolute-git-dir", "--git-common-dir"], cwd=Path(root), spec=spec).stdout.decode(
        "utf-8", errors="surrogateescape"
    )
    lines = [line for line in out.split("\n") if line]
    gitdir = Path(lines[0])
    common = Path(lines[1]) if len(lines) > 1 else gitdir
    if not common.is_absolute():
        common = (Path(root) / common).resolve()
    return gitdir, common


def object_format(git: Git, root_or_gitdir: Path, *, bare: bool = False) -> str:
    """``sha1`` or ``sha256``."""
    args = (["--git-dir", str(root_or_gitdir)] if bare else []) + ["rev-parse", "--show-object-format"]
    spec = RECEIVED if bare else _ceiling(root_or_gitdir)
    fmt = git.out(args, cwd=None if bare else Path(root_or_gitdir), spec=spec)
    return fmt or "sha1"


def head_info(git: Git, root: Path) -> tuple[str | None, str | None]:
    """``(HEAD commit or None for an unborn branch, symbolic ref or None when detached)``."""
    spec = _ceiling(root)
    ref = git.run(["symbolic-ref", "-q", "HEAD"], cwd=Path(root), spec=spec, check=False)
    head_ref = ref.stdout.decode("utf-8", errors="surrogateescape").strip() if ref.returncode == 0 else None
    commit = git.run(["rev-parse", "--verify", "-q", "HEAD^{commit}"], cwd=Path(root), spec=spec, check=False)
    sha = commit.stdout.decode("ascii", errors="replace").strip() if commit.returncode == 0 else None
    return (sha or None), (head_ref or None)


def index_path(git: Git, root: Path) -> Path:
    """The index file of the work tree at ``root``."""
    rel = git.out(["rev-parse", "--git-path", "index"], cwd=Path(root), spec=_ceiling(root))
    path = Path(rel)
    return path if path.is_absolute() else Path(root) / path


IN_PROGRESS_MARKERS: tuple[tuple[str, str], ...] = (
    ("MERGE_HEAD", "merge"),
    ("CHERRY_PICK_HEAD", "cherry-pick"),
    ("REVERT_HEAD", "revert"),
    ("rebase-merge", "rebase"),
    ("rebase-apply", "rebase"),
    ("BISECT_LOG", "bisect"),
    ("sequencer", "sequencer"),
)


def in_progress_op(gitdir: Path) -> str | None:
    """Name of an in-progress operation recorded in ``gitdir`` (merge, rebase, cherry-pick, …) or None."""
    for marker, name in IN_PROGRESS_MARKERS:
        if (Path(gitdir) / marker).exists():
            return name
    return None


# ---------------------------------------------------------------------------------------------------------
# Read-only state computation (§9.3 steps 4–5; shared by pack, prepare re-check and return mode)
# ---------------------------------------------------------------------------------------------------------


@dataclass
class StateWork:
    """Intermediate result of ``compute_state``: the trees plus what was excluded on the way."""

    head: str | None
    branch: str | None
    index_tree: str
    worktree_tree: str
    exclude_pathspecs: tuple[str, ...]
    unmerged: tuple[str, ...] = ()
    nested: tuple[str, ...] = ()
    notes: list[str] = field(default_factory=list)


def _write_pathspec_file(path: Path, records: Iterable[bytes]) -> Path:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(fd, "wb") as fh:
        for rec in records:
            fh.write(rec + b"\x00")
    return path


def compute_state(
    git: Git,
    root: Path,
    exclude_pathspecs: Sequence[str],
    objdir: Path,
    scratch: Path,
    *,
    force: bool = False,
    cancelled: Callable[[], bool] = _never,
) -> StateWork:
    """Index tree and work-tree tree of the repository at ``root`` without touching it (DESIGN §9.3 4–5).

    The work tree's index is copied into ``scratch``; every new object goes to ``objdir`` (``GIT_OBJECT_DIRECTORY``;
    the repository's objects are an alternate). Paths named by ``exclude_pathspecs`` are reset to HEAD in the index
    copy (a denied file staged in the index never leaves) and skipped by ``git add -A`` (they keep the index
    copy's content in the work-tree tree). Unmerged entries are refused unless ``force`` (then their work-tree
    content is taken)."""
    root = Path(root)
    _, common = git_dirs(git, root)
    head, branch = head_info(git, root)
    scratch.mkdir(parents=True, exist_ok=True)
    objdir.mkdir(parents=True, exist_ok=True)
    (objdir / "info").mkdir(exist_ok=True)
    (objdir / "pack").mkdir(exist_ok=True)
    idx1, idx2 = scratch / "index.i", scratch / "index.w"
    for p in (idx1, idx2):
        with contextlib.suppress(FileNotFoundError):
            p.unlink()
    src_index = index_path(git, root)
    if src_index.is_file():
        shutil.copyfile(src_index, idx1)
    extra = {
        "GIT_OBJECT_DIRECTORY": str(objdir),
        "GIT_ALTERNATE_OBJECT_DIRECTORIES": str(common / "objects"),
    }
    spec = _ceiling(root)
    excluded_paths = [pathspec_path(s) for s in exclude_pathspecs]
    if excluded_paths:
        ps = _write_pathspec_file(scratch / "reset.ps", (b":(literal)" + p for p in excluded_paths))
        if head is not None:
            git.run(
                ["reset", "-q", "--pathspec-from-file=" + str(ps), "--pathspec-file-nul", head],
                cwd=root,
                spec=spec,
                env_extra={**extra, "GIT_INDEX_FILE": str(idx1)},
                check=False,
                cancelled=cancelled,
            )
        else:
            git.run(
                ["rm", "-q", "--cached", "--ignore-unmatch", "--pathspec-from-file=" + str(ps), "--pathspec-file-nul"],
                cwd=root,
                spec=spec,
                env_extra={**extra, "GIT_INDEX_FILE": str(idx1)},
                check=False,
                cancelled=cancelled,
            )
    unmerged = _unmerged_paths(git, root, idx1, extra)
    if unmerged:
        if not force:
            raise ConflictError(
                "the repository has unmerged paths", kind="in_progress", paths=[display_path(p) for p in unmerged[:20]]
            )
        ps_u = _write_pathspec_file(scratch / "unmerged.ps", (b":(literal)" + p for p in unmerged))
        git.run(
            ["add", "-A", "--pathspec-from-file=" + str(ps_u), "--pathspec-file-nul"],
            cwd=root,
            spec=spec,
            env_extra={**extra, "GIT_INDEX_FILE": str(idx1)},
            cancelled=cancelled,
        )
    index_tree = git.out(
        ["write-tree"], cwd=root, spec=spec, env_extra={**extra, "GIT_INDEX_FILE": str(idx1)}, cancelled=cancelled
    )
    if idx1.is_file():
        shutil.copyfile(idx1, idx2)
    nested = nested_repositories(git, root, cancelled=cancelled)
    ps_w = _write_pathspec_file(
        scratch / "add.ps",
        [
            b":(top)",
            *(decode_pathspec(s) for s in exclude_pathspecs),
            *(b":(exclude,literal)" + n for n in nested),
        ],
    )
    git.run(
        ["add", "-A", "--pathspec-from-file=" + str(ps_w), "--pathspec-file-nul"],
        cwd=root,
        spec=spec,
        env_extra={**extra, "GIT_INDEX_FILE": str(idx2)},
        cancelled=cancelled,
    )
    worktree_tree = git.out(
        ["write-tree"], cwd=root, spec=spec, env_extra={**extra, "GIT_INDEX_FILE": str(idx2)}, cancelled=cancelled
    )
    return StateWork(
        head=head,
        branch=branch,
        index_tree=index_tree,
        worktree_tree=worktree_tree,
        exclude_pathspecs=tuple(exclude_pathspecs),
        unmerged=tuple(display_path(p) for p in unmerged),
        nested=tuple(display_path(n) for n in nested),
    )


def nested_repositories(git: Git, root: Path, *, cancelled: Callable[[], bool] = _never) -> list[bytes]:
    """Untracked directories that are repositories of their own (``ls-files -o`` lists them with a trailing
    slash). They are never added to the work-tree tree (``git add`` would record a bare gitlink or fail); the
    collector plans them as separate items."""
    found = []
    for rec in git.iter_z(
        ["ls-files", "-o", "--exclude-standard", "-z"], cwd=Path(root), spec=_ceiling(Path(root)), cancelled=cancelled
    ):
        if rec.endswith(b"/"):
            found.append(rec.rstrip(b"/"))
    return found


def _unmerged_paths(git: Git, root: Path, index: Path, extra: dict) -> list[bytes]:
    if not index.is_file():
        return []
    found: list[bytes] = []
    proc = git.run(
        ["ls-files", "-u", "-z"], cwd=root, spec=_ceiling(root), env_extra={**extra, "GIT_INDEX_FILE": str(index)}
    )
    for rec in proc.stdout.split(b"\x00"):
        if rec:
            path = rec.partition(b"\t")[2]
            if path and path not in found:
                found.append(path)
    return found


def worktree_state(git: Git, root: str, exclude_pathspecs: Sequence[str], tmp: Path) -> GitState:
    """``(HEAD, index tree, work-tree tree)`` of the repository at ``root`` with the recorded exclusions, computed
    in a temporary index and object directory under ``tmp`` (deleted afterwards). Read-only for the repository;
    used by the source's prepare re-check (§9.3 step 8) and the return-mode check (§9.5)."""
    tmp = Path(tmp)
    tmp.mkdir(parents=True, exist_ok=True, mode=0o700)
    work = Path(tempfile.mkdtemp(prefix="state-", dir=tmp))
    try:
        st = compute_state(git, Path(root), exclude_pathspecs, work / "objects", work, force=True)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    return GitState(
        head=st.head or "",
        index_tree=st.index_tree,
        worktree_tree=st.worktree_tree,
        branch=st.branch,
        exclude_pathspecs=tuple(exclude_pathspecs),
    )


# ---------------------------------------------------------------------------------------------------------
# Probe (read-only questions; satisfies contracts.GitProbe)
# ---------------------------------------------------------------------------------------------------------


class GitProbe:
    """Read-only git questions for collector, assessment and update-mode checks (INTERFACES §4.3)."""

    def __init__(self, git: Git, *, profile: str = "source"):
        """``profile``: ``source`` for questions about the user's own repository on the source (collector);
        ``received`` for every question about a destination on the target (its local filter drivers emptied)."""
        if profile not in {"source", "received"}:
            raise ValueError(f"unknown git profile {profile!r}")
        self.git = git
        self.profile = profile

    def _spec(self, path: Path) -> GitEnvSpec:
        p = Path(path)
        if self.profile == "received":
            return GitEnvSpec(profile="received", ceiling=p.parent, filters_from=p)
        return _ceiling(p)

    def _run(self, path: str, args: list[str], **kw) -> subprocess.CompletedProcess:
        return self.git.run(args, cwd=Path(path), spec=self._spec(Path(path)), check=False, **kw)

    def toplevel(self, path: str) -> str | None:
        p = Path(path)
        if not p.is_dir():
            return None
        proc = self._run(path, ["rev-parse", "--show-toplevel"])
        out = proc.stdout.decode("utf-8", errors="surrogateescape").strip()
        return out if proc.returncode == 0 and out else None

    def common_dir(self, path: str) -> str | None:
        if not Path(path).is_dir():
            return None
        proc = self._run(path, ["rev-parse", "--path-format=absolute", "--git-common-dir"])
        out = proc.stdout.decode("utf-8", errors="surrogateescape").strip()
        return out if proc.returncode == 0 and out else None

    def is_shallow(self, path: str) -> bool:
        proc = self._run(path, ["rev-parse", "--is-shallow-repository"])
        return proc.returncode == 0 and proc.stdout.strip() == b"true"

    def is_partial(self, path: str) -> bool:
        """True for a partial clone (a promisor remote)."""
        proc = self._run(path, ["config", "--get-regexp", r"^remote\..*\.promisor$"])
        return proc.returncode == 0 and b"true" in proc.stdout

    def in_progress(self, root: str) -> str | None:
        try:
            gitdir, _ = git_dirs(self.git, Path(root))
        except GitCommandError:
            return None
        return in_progress_op(gitdir)

    def status_clean(self, root: str) -> tuple[bool, str]:
        """``(clean, why)``: no change in index or work tree including untracked files, no in-progress
        operation, no ``index.lock`` (DESIGN §9.5 update preconditions)."""
        gitdir, _ = git_dirs(self.git, Path(root))
        op = in_progress_op(gitdir)
        if op:
            return False, f"{op} in progress"
        if (gitdir / "index.lock").exists():
            return False, "index.lock present"
        proc = self.git.run(
            ["status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=none"],
            cwd=Path(root),
            spec=self._spec(Path(root)),
        )
        if proc.stdout:
            first = proc.stdout.split(b"\x00", 1)[0]
            return False, "uncommitted changes: " + display_path(first)[:200]
        return True, ""

    def root_commits(self, root: str) -> tuple[str, ...]:
        """Up to 16 root commits reachable from any ref (repository identity, DESIGN §9.5)."""
        proc = self._run(root, ["rev-list", "--max-parents=0", "--all", "--max-count=16"])
        if proc.returncode != 0:
            return ()
        return tuple(sorted(x for x in proc.stdout.decode("ascii", errors="replace").split() if is_sha(x)))

    def ref_tips(self, root: str) -> tuple[str, ...]:
        """Commits at the tips of local branches, remote-tracking refs and tags (``git_have``, DESIGN §8.6)."""
        proc = self._run(
            root,
            [
                "for-each-ref",
                "--format=%(objecttype) %(objectname) %(*objecttype) %(*objectname)",
                "refs/heads",
                "refs/remotes",
                "refs/tags",
            ],
        )
        tips: set[str] = set()
        for line in proc.stdout.decode("ascii", errors="replace").split("\n"):
            parts = line.split()
            if len(parts) >= 2 and parts[0] == "commit" and is_sha(parts[1]):
                tips.add(parts[1])
            elif len(parts) == 4 and parts[2] == "commit" and is_sha(parts[3]):
                tips.add(parts[3])
        return tuple(sorted(tips))

    def is_ancestor(self, root: str, a: str, b: str) -> bool:
        require_sha(a, "a")
        require_sha(b, "b")
        proc = self._run(root, ["merge-base", "--is-ancestor", "--end-of-options", a, b])
        return proc.returncode == 0

    def has_commit(self, root: str, sha: str) -> bool:
        if not is_sha(sha):
            return False
        proc = self._run(root, ["cat-file", "-e", "--end-of-options", sha + "^{commit}"])
        return proc.returncode == 0

    def _tree_args(self, root_or_gitdir: str) -> tuple[list[str], Path | None, GitEnvSpec]:
        p = Path(root_or_gitdir)
        if (p / "objects").is_dir() and (p / "HEAD").exists() and not (p / ".git").exists():
            return ["--git-dir", str(p)], None, RECEIVED
        return [], p, self._spec(p)

    def ls_tree_paths(self, root_or_gitdir: str, tree: str) -> list[str]:
        """Every path of ``tree`` (recursive, raw bytes decoded with surrogateescape)."""
        require_sha(tree, "tree")
        pre, cwd, spec = self._tree_args(root_or_gitdir)
        return [
            os.fsdecode(rec)
            for rec in self.git.iter_z(
                [*pre, "ls-tree", "-r", "-z", "--name-only", "--full-tree", "--end-of-options", tree],
                cwd=cwd,
                spec=spec,
            )
            if rec
        ]

    def ls_tree_sizes(self, gitdir: str, tree: str) -> tuple[int, int]:
        """``(blob bytes, entries)`` of ``tree`` without reading blob contents (pre-checkout check, SEC 2.5)."""
        require_sha(tree, "tree")
        pre, cwd, spec = self._tree_args(gitdir)
        total = entries = 0
        for rec in self.git.iter_z(
            [*pre, "ls-tree", "-r", "-l", "-z", "--full-tree", "--end-of-options", tree], cwd=cwd, spec=spec
        ):
            if not rec:
                continue
            entries += 1
            meta = rec.partition(b"\t")[0].split()
            if len(meta) == 4 and meta[1] == b"blob" and meta[3].isdigit():
                total += int(meta[3])
        return total, entries

    def info(self, root: str, cfg: PolicyConfig) -> GitInfo:
        """Planning facts of the repository at ``root`` (DESIGN §8.2 step 6, §9.3, §9.4). Streams the file lists;
        sizes come from ``lstat`` of untracked files only."""
        r = Path(root)
        head, branch = head_info(self.git, r)
        spec = self._spec(r)
        gitdir, common = git_dirs(self.git, r)
        fmt = object_format(self.git, r)
        status = self.git.run(
            ["status", "--porcelain=v2", "-z", "--untracked-files=no", "--ignore-submodules=all"], cwd=r, spec=spec
        )
        clean = not status.stdout
        untracked_count = untracked_bytes = 0
        for rec in self.git.iter_z(["ls-files", "-o", "--exclude-standard", "-z"], cwd=r, spec=spec):
            if not rec:
                continue
            untracked_count += 1
            with contextlib.suppress(OSError):
                st = os.lstat(os.path.join(os.fsencode(r), rec))
                if stat.S_ISREG(st.st_mode):
                    untracked_bytes += st.st_size
        submodules: list[str] = []
        denied: list[str] = []
        for rec in self.git.iter_z(["ls-files", "-s", "-z"], cwd=r, spec=spec):
            if not rec:
                continue
            meta, _, path = rec.partition(b"\t")
            if meta.startswith(b"160000"):
                submodules.append(display_path(path))
            if is_denied_name(os.fsdecode(path), case_insensitive=cfg.case_insensitive):
                denied.append(display_path(path))
        remotes = []
        rem = self.git.run(["config", "-z", "--get-regexp", r"^remote\..*\.url$"], cwd=r, spec=spec, check=False)
        for rec in rem.stdout.split(b"\x00"):
            if not rec:
                continue
            key, _, value = rec.partition(b"\n")
            name = key.decode("utf-8", errors="replace")[len("remote.") : -len(".url")]
            remotes.append((name, strip_url_userinfo(value.decode("utf-8", errors="replace"))))
        lfs = (common / "lfs").is_dir() or _attributes_mention_lfs(r)
        return GitInfo(
            head=head,
            branch=branch,
            object_format=fmt,
            shallow=self.is_shallow(root),
            dirty=(not clean) or untracked_count > 0,
            in_progress=in_progress_op(gitdir),
            lfs=lfs,
            submodules=tuple(sorted(submodules)),
            remotes=tuple(sorted(remotes)),
            root_commits=self.root_commits(root),
            untracked_bytes=untracked_bytes,
            untracked_count=untracked_count,
            denied_tracked=tuple(sorted(denied)),
        )


def _attributes_mention_lfs(root: Path) -> bool:
    try:
        with open(Path(root) / ".gitattributes", "rb") as fh:
            return b"filter=lfs" in fh.read(1024 * 1024)
    except OSError:
        return False


# ---------------------------------------------------------------------------------------------------------
# Objects as blocks (DESIGN §4.6, §5.2) — through the BlockSink/BlockSource protocols only
# ---------------------------------------------------------------------------------------------------------


def manifest_doc(m: ObjectManifest) -> dict:
    """The ``ctxh.object`` document of DESIGN §5.2 (without ``object_id``)."""
    return {
        "kind": "ctxh.object",
        "v": 1,
        "sha256": m.sha256,
        "size": m.size,
        "block_size": m.block_size,
        "blocks": list(m.blocks),
        "media": m.media,
    }


def object_id_of(m: ObjectManifest) -> str:
    """``object_id = sha256(canonical manifest bytes)`` (DESIGN §4.6)."""
    return doc_id(manifest_doc(m))


def _flush(store: BlockSink, frames: list[tuple[str, bytes]]) -> None:
    if not frames:
        return
    unique = dict(frames)
    need = set(store.missing(list(unique)))
    if need:
        _, errors = store.write_blocks([(bid, unique[bid]) for bid in unique if bid in need])
        if errors:
            raise BlockCorrupt("block store refused blocks", errors=[list(e) for e in errors[:5]])


def put_stream(
    store: BlockSink,
    source: IO[bytes] | Callable[[int], bytes],
    *,
    media: str,
    cancelled: Callable[[], bool] = _never,
    batch: int = 16,
    first: bytes = b"",
) -> ObjectManifest:
    """Chunk a byte stream into ``BLOCK_SIZE`` blocks and write them and the manifest into ``store``; at most
    ``batch`` blocks are held in memory. ``first`` is prepended (bytes already read by the caller)."""
    read = source if callable(source) else source.read
    content = hashlib.sha256()
    ids: list[str] = []
    frames: list[tuple[str, bytes]] = []
    size = 0
    pending = bytearray(first)
    while True:
        if cancelled():
            raise Cancelled("cancelled while storing an object")
        while len(pending) < BLOCK_SIZE:
            chunk = read(BLOCK_SIZE - len(pending))
            if not chunk:
                break
            pending.extend(chunk)
        if not pending:
            break
        raw = bytes(pending[:BLOCK_SIZE])
        del pending[:BLOCK_SIZE]
        bid = hashlib.sha256(raw).hexdigest()
        content.update(raw)
        size += len(raw)
        ids.append(bid)
        frames.append((bid, raw))
        if len(frames) >= batch:
            _flush(store, frames)
            frames = []
        if len(raw) < BLOCK_SIZE and not pending:
            break
    _flush(store, frames)
    manifest = ObjectManifest(
        sha256=content.hexdigest(), size=size, block_size=BLOCK_SIZE, blocks=tuple(ids), media=media
    )
    manifest = ObjectManifest(**{**manifest.__dict__, "object_id": object_id_of(manifest)})
    store.put_manifests([manifest])
    return manifest


def put_bytes(store: BlockSink, data: bytes, media: str) -> ObjectManifest:
    """``put_stream`` for an in-memory document (manifests, small files)."""
    view = memoryview(data)
    pos = 0

    def read(n: int) -> bytes:
        nonlocal pos
        chunk = bytes(view[pos : pos + n])
        pos += len(chunk)
        return chunk

    return put_stream(store, read, media=media)


def object_manifest(source: BlockSource, object_id: str) -> ObjectManifest:
    """The verified manifest of ``object_id`` (its recomputed id must match)."""
    require_sha(object_id, "object")
    found = source.manifests([object_id])
    if len(found) != 1:
        raise NotFound("object manifest missing", object=object_id)
    m = found[0]
    if object_id_of(m) != object_id:
        raise BlockCorrupt("object manifest does not match its id", object=object_id)
    if m.block_size <= 0 or m.block_size > 4 * 1024 * 1024 or m.size < 0:
        raise BlockCorrupt("object manifest out of range", object=object_id)
    if len(m.blocks) != (m.size + m.block_size - 1) // m.block_size:
        raise BlockCorrupt("object manifest block count does not match its size", object=object_id)
    return m


def iter_object(
    source: BlockSource,
    object_id: str,
    *,
    cancelled: Callable[[], bool] = _never,
    max_size: int | None = None,
    batch: int = 8,
) -> Iterator[bytes]:
    """Yield the blocks of an object in order, verifying every block id and, at the end, size and content hash
    (``BlockCorrupt`` otherwise). At most ``batch`` blocks are held in memory."""
    m = object_manifest(source, object_id)
    if max_size is not None and m.size > max_size:
        raise PolicyDenied("object larger than allowed", reason="size", object=object_id, size=m.size)
    content = hashlib.sha256()
    total = 0
    ids = list(m.blocks)
    for start in range(0, len(ids), batch):
        if cancelled():
            raise Cancelled("cancelled while reading an object")
        want = ids[start : start + batch]
        got: dict[str, bytes] = {}
        for bid, raw in source.read_blocks(list(dict.fromkeys(want))):
            if bid in want and bid not in got:
                if hashlib.sha256(raw).hexdigest() != bid or len(raw) > m.block_size:
                    raise BlockCorrupt("block content does not match its id", block=bid)
                got[bid] = raw
        for bid in want:
            raw = got.get(bid)
            if raw is None:
                raise NotFound("block missing", block=bid, object=object_id)
            content.update(raw)
            total += len(raw)
            yield raw
    if total != m.size or content.hexdigest() != m.sha256:
        raise BlockCorrupt("object content does not match its manifest", object=object_id)


def read_object(source: BlockSource, object_id: str, *, max_size: int = OBJECT_JSON_CAP) -> bytes:
    """A whole (small) object, bounded by ``max_size`` before any block is read."""
    return b"".join(iter_object(source, object_id, max_size=max_size))


def read_json_object(source: BlockSource, object_id: str, *, max_size: int = OBJECT_JSON_CAP) -> object:
    """A JSON object, size-capped before reading and parsed with ``loads_bounded``."""
    return loads_bounded(read_object(source, object_id, max_size=max_size), max_bytes=max_size)


def write_object_to(
    source: BlockSource, object_id: str, fd: int, *, cancelled: Callable[[], bool] = _never, max_size: int | None = None
) -> tuple[int, str]:
    """Stream an object into an open file descriptor; returns ``(size, sha256)``."""
    h = hashlib.sha256()
    n = 0
    for raw in iter_object(source, object_id, cancelled=cancelled, max_size=max_size):
        view = memoryview(raw)
        while view:
            written = os.write(fd, view)
            view = view[written:]
        h.update(raw)
        n += len(raw)
    return n, h.hexdigest()


def sha256_file(path: Path, *, follow: bool = False) -> str:
    """Streaming sha256 of a regular file (``O_NOFOLLOW`` unless ``follow``)."""
    flags = os.O_RDONLY | (0 if follow else getattr(os, "O_NOFOLLOW", 0))
    fd = os.open(path, flags)
    try:
        h = hashlib.sha256()
        while True:
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            h.update(chunk)
        return h.hexdigest()
    finally:
        os.close(fd)


def enc_path(text: str) -> str:
    """JSON-safe form of a path string that may carry surrogate-escaped bytes (``b64:`` + base64 of the bytes)."""
    try:
        text.encode("utf-8")
        return text if not text.startswith("b64:") else "b64:" + base64.b64encode(text.encode()).decode("ascii")
    except UnicodeEncodeError:
        return "b64:" + base64.b64encode(os.fsencode(text)).decode("ascii")


def dec_path(text: str) -> str:
    """Inverse of ``enc_path``."""
    if text.startswith("b64:"):
        return os.fsdecode(base64.b64decode(text[4:], validate=True))
    return text
