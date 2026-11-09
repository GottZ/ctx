"""Path normalisation and safety rules per target OS (layer L1, stdlib only; DESIGN §10.3–§10.5, INTERFACES §3.5).

Relative paths arrive from peers (tree manifests, plans, refs) and are hostile until ``check_rel`` accepted
them. The rules hold on every target; Windows-only rules apply when ``PlatformRules.windows`` is set.

Deliberate tightening over DESIGN §10.5 (recorded in the K0 report): the ``.git`` check strips HFS-ignorable
code points and NTFS trailing dots/spaces and stream suffixes on **every** target, not only on macOS/Windows
targets — a tree written on Linux is often synced to a Mac later, and the rule costs nothing on Linux.
"""

from __future__ import annotations

import os
import re
import stat
import unicodedata
from collections.abc import Iterable, Sequence
from dataclasses import dataclass
from pathlib import Path, PurePath, PurePosixPath, PureWindowsPath

from .errors import BadRequest, PolicyDenied

MAX_COMPONENT_BYTES = 255
MAX_REL_BYTES = 4096
WINDOWS_MAX_PATH = 260
MAX_ROUTE_CHARS = 1024

DEFAULT_WORKSPACE_SPEC = "~/hermes-handover"
"""Schema default of ``workspace_root`` and ``allow_roots`` (DESIGN §14.4)."""
HERMES_WORKSPACES_DIR = "handover-workspaces"
"""The directory the plugin reserves in a Hermes home (R29): the default write root when ``~/hermes-handover`` lies
in the Hermes home (``HOME`` = ``HERMES_HOME`` in the official Docker image), and the one exception from the target
denylist for the Hermes home (``policy.check_target_dest``)."""

# git utf8.c next_hfs_char(): code points HFS+ ignores when comparing names (git's core.protectHFS).
HFS_IGNORABLE = frozenset(
    "\u200c\u200d\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u206a\u206b\u206c\u206d\u206e\u206f\ufeff"
)
_SUPERSCRIPT_DIGITS = ("\u00b9", "\u00b2", "\u00b3")
WINDOWS_RESERVED_NAMES = frozenset(
    {"con", "prn", "aux", "nul", "conin$", "conout$"}
    | {f"{dev}{n}" for dev in ("com", "lpt") for n in (*"123456789", *_SUPERSCRIPT_DIGITS)}
)
WINDOWS_FORBIDDEN_CHARS = frozenset('<>:"|?*\\')

_ROUTE_RE = re.compile(r"[A-Za-z0-9/_.\-]+")
_MSYS_RE = re.compile(r"^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])(/.*)?$")
_WINDOWS_ABS_RE = re.compile(r"^[A-Za-z]:[\\/]")
_URL_RE = re.compile(r"^(?P<scheme>[A-Za-z][A-Za-z0-9+.\-]*)://(?P<authority>[^/?#]*)(?P<rest>.*)$", re.DOTALL)


@dataclass(frozen=True)
class PlatformRules:
    """Path rules of one target: Windows naming rules, case-insensitive comparison, long-path support."""

    windows: bool
    case_insensitive: bool
    long_paths: bool = False


def rules_for(os_family: str, case_insensitive: bool, *, long_paths: bool = False) -> PlatformRules:
    """Rules for a target described by ``EnvFacts.os_family`` (``linux`` | ``darwin`` | ``windows`` …)."""
    family = (os_family or "").strip().lower()
    windows = family in {"windows", "win32", "nt", "cygwin", "msys"}
    return PlatformRules(windows=windows, case_insensitive=bool(case_insensitive or windows), long_paths=long_paths)


def _deny(rel: object, why: str) -> PolicyDenied:
    shown = rel if isinstance(rel, str) else repr(rel)
    return PolicyDenied(f"unsafe path ({why}): {shown!r}"[:300], reason=why, path=str(shown)[:300])


def strip_hfs_ignorable(rel: str) -> str:
    """``rel`` without the code points HFS+ ignores (U+200C … U+FEFF), as git's ``protectHFS`` compares names."""
    return "".join(ch for ch in rel if ch not in HFS_IGNORABLE)


def _is_dotgit(component: str) -> bool:
    """True when a component would be ``.git`` on some filesystem (case, HFS, NTFS 8.3/stream/trailing forms)."""
    name = strip_hfs_ignorable(component)
    name = name.split(":", 1)[0].rstrip(". ")
    folded = unicodedata.normalize("NFC", name).casefold()
    return folded in {".git", "git~1"}


def _has_control(text: str) -> bool:
    return any(unicodedata.category(ch) in {"Cc", "Cs"} for ch in text)


def _posix_name_char(ch: str) -> bool:
    """A character ``allow_control`` admits on a POSIX target: a C0 control except NUL, DEL, or a
    ``surrogateescape`` byte (U+DC80–U+DCFF, a non-UTF-8 byte of a file name)."""
    code = ord(ch)
    return 0 < code < 0x20 or code == 0x7F or 0xDC80 <= code <= 0xDCFF


def _has_forbidden_control(text: str, allow_control: bool) -> bool:
    if not allow_control:
        return _has_control(text)
    return any(unicodedata.category(ch) in {"Cc", "Cs"} and not _posix_name_char(ch) for ch in text)


def check_rel(rel: str, rules: PlatformRules, *, allow_control: bool = False) -> str:
    """Validate a relative path from a manifest, plan or ref (DESIGN §10.5); return it or raise ``PolicyDenied``.

    Every target: non-empty, ``/``-separated, not absolute, no ``.``/``..``/empty components, no NUL or control
    characters, components ≤ 255 bytes and the whole ≤ 4096 bytes (UTF-8), no component that is ``.git`` in any
    spelling (case, HFS-ignorable code points, ``git~1``, trailing dots/spaces, NTFS stream suffix).
    Windows targets additionally: none of ``<>:"|?*\\``, no reserved device names (any extension), no trailing
    dot or space, ≤ 260 characters unless ``long_paths``. The accepted path is returned unchanged — every
    non-canonical spelling is rejected rather than rewritten.

    ``allow_control`` (W095) is for names git or a file tree reproduces byte for byte on a POSIX target: C0
    control characters except NUL, DEL and ``surrogateescape`` bytes (``os.fsdecode`` of non-UTF-8 names) are then
    legal, and lengths count those bytes as one byte each. NUL, C1 controls (U+0080–U+009F) and every other
    surrogate stay refused, and the flag has no effect on a Windows target (NTFS forbids control characters).
    """
    if not isinstance(rel, str) or rel == "":
        raise _deny(rel, "empty")
    allow_control = bool(allow_control) and not rules.windows
    if _has_forbidden_control(rel, allow_control):
        raise _deny(rel, "control_character")
    errors = "surrogateescape" if allow_control else "strict"
    try:
        encoded = rel.encode("utf-8", errors)
    except UnicodeEncodeError:
        raise _deny(rel, "encoding") from None
    if rel.startswith("/") or _WINDOWS_ABS_RE.match(rel) or rel.startswith("\\"):
        raise _deny(rel, "absolute")
    if len(encoded) > MAX_REL_BYTES:
        raise _deny(rel, "too_long")
    for part in rel.split("/"):
        if part in {"", ".", ".."}:
            raise _deny(rel, "dot_or_empty_component")
        if len(part.encode("utf-8", errors)) > MAX_COMPONENT_BYTES:
            raise _deny(rel, "component_too_long")
        if _is_dotgit(part):
            raise _deny(rel, "dotgit")
        if rules.windows:
            if any(ch in WINDOWS_FORBIDDEN_CHARS for ch in part):
                raise _deny(rel, "windows_forbidden_character")
            if part[-1] in {".", " "}:
                raise _deny(rel, "windows_trailing_dot_or_space")
            base = part.split(".", 1)[0].rstrip(" ").casefold()
            if base in WINDOWS_RESERVED_NAMES:
                raise _deny(rel, "windows_reserved_name")
    if rules.windows and not rules.long_paths and len(rel) > WINDOWS_MAX_PATH:
        raise _deny(rel, "windows_path_too_long")
    return rel


def safe_join(root: Path, rel: str, rules: PlatformRules) -> Path:
    """``root / rel`` after ``check_rel``; every existing parent component inside ``root`` must be a real
    directory (``os.lstat``; a symlink or non-directory raises ``PolicyDenied``). The final component is not
    inspected — writers open it with ``O_NOFOLLOW`` (DESIGN §10.3)."""
    rel = check_rel(rel, rules)
    base = Path(root)
    if not base.is_absolute():
        raise _deny(str(root), "root_not_absolute")
    parts = rel.split("/")
    current = base
    for part in parts[:-1]:
        current = current / part
        try:
            st = os.lstat(current)
        except FileNotFoundError:
            break
        if stat.S_ISLNK(st.st_mode):
            raise _deny(rel, "symlink_component")
        if not stat.S_ISDIR(st.st_mode):
            raise _deny(rel, "not_a_directory")
    target = base.joinpath(*parts)
    if rules.windows and not rules.long_paths and len(str(target)) > WINDOWS_MAX_PATH:
        raise _deny(rel, "windows_path_too_long")
    return target


def _norm_parts(path: PurePath, case_insensitive: bool) -> tuple[str, ...] | None:
    parts = path.parts
    if ".." in parts:
        return None
    folded = tuple(unicodedata.normalize("NFC", p) for p in parts)
    if case_insensitive:
        folded = tuple(unicodedata.normalize("NFC", p.casefold()) for p in folded)
    return folded


def under_any(path: Path, roots: Sequence[Path], *, case_insensitive: bool = False) -> bool:
    """True when ``path`` equals or lies under one of ``roots`` — a lexical comparison of components (NFC, plus
    casefold when ``case_insensitive``). A path containing ``..`` is never under anything; pass real paths."""
    candidate = _norm_parts(PurePath(path), case_insensitive)
    if candidate is None or not PurePath(path).is_absolute():
        return False
    for root in roots:
        root_parts = _norm_parts(PurePath(root), case_insensitive)
        if root_parts is None or not PurePath(root).is_absolute():
            continue
        if candidate[: len(root_parts)] == root_parts:
            return True
    return False


def expand_root(spec: str, home: Path) -> Path:
    """Expand a configured root (``~``, ``~/x``, absolute, or relative = under ``home``) to an absolute, lexically
    normalised path. ``~user`` forms and device prefixes raise ``PolicyDenied``."""
    text = (spec or "").strip()
    if not text:
        raise _deny(spec, "empty_root")
    reject_windows_device_prefix(text)
    if text == "~":
        expanded = Path(home)
    elif text.startswith(("~/", "~\\")):
        expanded = Path(home) / text[2:]
    elif text.startswith("~"):
        raise _deny(spec, "tilde_user_form")
    else:
        expanded = Path(text)
        if not expanded.is_absolute():
            expanded = Path(home) / expanded
    return Path(os.path.normpath(expanded))


def _absolute(path: Path | str) -> Path:
    text = os.fspath(path)
    if not isinstance(text, str) or not Path(text).is_absolute():
        raise _deny(text, "root_not_absolute")
    return Path(os.path.normpath(text))


def default_workspace_root(
    home: Path | str,
    hermes_home: Path | str,
    *,
    hermes_root: Path | str | None = None,
    case_insensitive: bool = False,
) -> Path:
    """The default write root on this host (R29): ``<home>/hermes-handover``, unless that lies in the Hermes home
    (``hermes_home``, or ``hermes_root`` — the root of all profiles, which the target denylist refuses as well) as
    given or resolved (``os.path.realpath``, as the denylist compares) — then ``<hermes_home>/handover-workspaces``.

    Both paths must be absolute (``PolicyDenied`` with reason ``root_not_absolute`` otherwise); the result is
    lexically normalised in the spelling of ``hermes_home`` (never a realpath). Pass ``hermes_root`` whenever it is
    known: with a profile in the Docker image ``HOME`` is the Hermes root, not the active home."""
    base = _absolute(home)
    hh = _absolute(hermes_home)
    bases = [hh]
    if hermes_root is not None:
        bases.append(_absolute(hermes_root))
    candidate = base / DEFAULT_WORKSPACE_SPEC[2:]
    spellings = {candidate, Path(os.path.realpath(candidate))}
    roots = {spelling for b in bases for spelling in (b, Path(os.path.realpath(b)))}
    if any(under_any(c, list(roots), case_insensitive=case_insensitive) for c in spellings):
        return hh / HERMES_WORKSPACES_DIR
    return candidate


def expand_handover_root(
    spec: str,
    home: Path | str,
    hermes_home: Path | str,
    *,
    hermes_root: Path | str | None = None,
    case_insensitive: bool = False,
) -> Path:
    """``expand_root(spec, home)`` for ``workspace_root`` and each ``allow_roots`` entry; a spelling that expands to
    ``<home>/hermes-handover`` (the schema default however written) is ``default_workspace_root(...)``. Any other
    root stays as configured — an explicit root inside the Hermes home is still refused by the target denylist.

    R29 needs ``home``, ``hermes_home`` and a given ``hermes_root`` absolute on this host; when one is not (a home of
    another OS, e.g. ``C:\\Users\\x`` on a POSIX host), the result is ``expand_root(spec, home)`` — no error."""
    expanded = expand_root(spec, Path(home))
    default = Path(os.path.normpath(Path(home) / DEFAULT_WORKSPACE_SPEC[2:]))
    if _norm_parts(expanded, case_insensitive) != _norm_parts(default, case_insensitive):
        return expanded
    given = (home, hermes_home) if hermes_root is None else (home, hermes_home, hermes_root)
    if not all(Path(os.fspath(p)).is_absolute() for p in given):
        return expanded
    return default_workspace_root(home, hermes_home, hermes_root=hermes_root, case_insensitive=case_insensitive)


def _flavour(*paths: str) -> type[PurePath]:
    if any(_WINDOWS_ABS_RE.match(p) or p.startswith("\\\\") for p in paths):
        return PureWindowsPath
    return PurePosixPath


def home_relative(path: str, home: str) -> str | None:
    """``path`` relative to ``home`` as a ``/``-separated string (``""`` for home itself), or None when ``path`` is
    not under ``home``. Works on POSIX and Windows spellings regardless of the local OS (Windows: case-insensitive);
    paths containing ``..`` are never home-relative."""
    if not path or not home:
        return None
    flavour = _flavour(path, home)
    p, h = flavour(path), flavour(home)
    if not p.is_absolute() or not h.is_absolute():
        return None
    ci = flavour is PureWindowsPath
    p_parts, h_parts = _norm_parts(p, ci), _norm_parts(h, ci)
    if p_parts is None or h_parts is None or p_parts[: len(h_parts)] != h_parts:
        return None
    return "/".join(p.parts[len(h_parts) :])


def fold_key(rel: str, rules: PlatformRules) -> str:
    """Comparison key of a relative path on the target. Case-insensitive targets (macOS, Windows) compare
    NFC-normalised and case-folded names; case-sensitive targets compare bytes, so NFC and NFD spellings stay
    distinct there (ext4 keeps both as separate files)."""
    if not rules.case_insensitive:
        return rel
    return unicodedata.normalize("NFC", unicodedata.normalize("NFC", rel).casefold())


def collisions(rels: Iterable[str], rules: PlatformRules) -> list[tuple[str, str]]:
    """``(kept, colliding)`` pairs of paths with the same ``fold_key`` (exact duplicates included); the first
    occurrence wins (DESIGN §10.5)."""
    first: dict[str, str] = {}
    found: list[tuple[str, str]] = []
    for rel in rels:
        key = fold_key(rel, rules)
        if key in first:
            found.append((first[key], rel))
        else:
            first[key] = rel
    return found


def reject_windows_device_prefix(path: str) -> None:
    """Raise ``PolicyDenied`` for UNC and device paths (two leading separators of either kind, which covers the
    ``\\\\?\\`` and ``\\\\.\\`` forms, and ``\\??\\``) before any filesystem call — resolving them would
    trigger SMB authentication (DESIGN §10.4)."""
    if not isinstance(path, str):
        raise _deny(path, "not_a_string")
    head = path[:4]
    if len(head) >= 2 and head[0] in "\\/" and head[1] in "\\/":
        raise _deny(path, "windows_device_or_unc")
    if head == "\\??\\":
        raise _deny(path, "windows_device_or_unc")


def route_path(route: str) -> str:
    """Canonical signed path ``/v1/<route>`` from the router's path parameter (DESIGN §4.2, SEC 1.4).

    Only ``[A-Za-z0-9/_.-]`` is accepted (so no percent-escapes, spaces or ``;``) and no empty, ``.`` or ``..``
    segment; anything else raises ``BadRequest``.
    """
    if not isinstance(route, str) or not route or len(route) > MAX_ROUTE_CHARS or not _ROUTE_RE.fullmatch(route):
        raise BadRequest("invalid route path", field="path")
    if any(seg in {"", ".", ".."} for seg in route.split("/")):
        raise BadRequest("invalid route path segment", field="path")
    return "/v1/" + route


def msys_to_windows(path: str) -> str:
    """``/c/x``, ``/cygdrive/c/x``, ``/mnt/c/x`` → ``C:\\x`` (re-implementation of the private
    ``tools/environments/local.py:_msys_to_windows_path``); anything else is returned unchanged."""
    match = _MSYS_RE.match(path or "")
    if not match:
        return path
    tail = (match.group(2) or "").replace("/", "\\")
    return f"{match.group(1).upper()}:{tail or chr(92)}"


def strip_url_userinfo(url: str) -> str:
    """Remove credentials from a URL (DESIGN §10.7): the whole userinfo for every scheme except ``ssh`` /
    ``git+ssh`` / ``ssh+git``, where the user name (not a secret) is kept and only a password is dropped.
    scp-like ``user@host:path`` and non-URL strings are returned unchanged."""
    match = _URL_RE.match(url or "")
    if not match:
        return url
    scheme, authority, rest = match.group("scheme"), match.group("authority"), match.group("rest")
    if "@" not in authority:
        return url
    userinfo, host = authority.rsplit("@", 1)
    if scheme.lower() in {"ssh", "git+ssh", "ssh+git"}:
        user = userinfo.split(":", 1)[0]
        return f"{scheme}://{user}@{host}{rest}" if user else f"{scheme}://{host}{rest}"
    return f"{scheme}://{host}{rest}"
