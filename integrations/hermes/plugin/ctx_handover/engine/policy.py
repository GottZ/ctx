"""Secrets and file policy (layer L2, stdlib only; DESIGN §10.1–§10.4, R18, R19, R23, INTERFACES §3.5).

Source side (I6): a file leaves the source only when it is not on the secrets denylist (name, directory,
content sniff), not inside the Hermes root, a Hermes credential store or a protected system path, not a
special file, and within the limits. ``include_secret`` — local flags of this job only, never from a peer,
never from settings (R18) — lifts the name/directory/content denylist for exactly the named paths; it never
lifts the Hermes root, Hermes credential stores or system paths (R19).

Target side (I5, R19): a write lands only under ``allow_roots`` (or at a path this host recorded itself or the
local user typed) and never on the target denylist: the Hermes root and every profile, the plugin data dir,
credential directories, shell/login startup files, autostart locations, ``~/.local/bin``, ``~/bin``, git/ssh/
package configuration, every dot entry directly under ``$HOME`` that is not opened by an allow-root — and never
at ``$HOME`` itself or any ancestor of a denied location (a tree written there would land its entries inside
one).

Every check looks at the path as given **and** at its ``realpath``; components are compared NFC-normalised and,
on case-insensitive filesystems, case-folded. No check reads a file before the never-releasable rules passed.

``PolicyConfig`` describes the host where the checks run: ``case_insensitive`` is its filesystem's behaviour,
``target_os`` the OS whose naming rules apply to paths written here.
"""

from __future__ import annotations

import fnmatch
import os
import re
import stat
import unicodedata
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path, PurePath
from typing import NamedTuple

from .errors import PolicyDenied
from .model import Options
from .paths import PlatformRules, expand_root, reject_windows_device_prefix, rules_for, under_any

SNIFF_BYTES = 8 * 1024

# Reasons a Verdict can carry. "missing" and "unreadable" are K0 additions to the INTERFACES §3.5 list.
REASONS = frozenset(
    {
        "secret",
        "hermes",
        "size",
        "outside_home",
        "special",
        "symlink",
        "target_denied",
        "not_allowed_root",
        "suspected_secret",
        "missing",
        "unreadable",
    }
)

SOURCE_DENY_NAMES: tuple[str, ...] = (
    ".env",
    ".env.*",
    ".envrc",
    ".netrc",
    "_netrc",
    ".pgpass",
    ".npmrc",
    ".pypirc",
    ".yarnrc",
    ".yarnrc.yml",
    "pip.conf",
    "pip.ini",
    ".git-credentials",
    ".dockercfg",
    ".vault-token",
    ".htpasswd",
    ".boto",
    ".s3cfg",
    "rclone.conf",
    ".terraformrc",
    "credentials.tfrc.json",
    "credentials",
    "credentials.json",
    ".credentials.json",
    "credentials.toml",
    "auth.json",
    "client_secret*.json",
    "service-account*.json",
    "*.tfstate",
    "*.tfstate.backup",
    "*.tfvars",
    "kubeconfig",
    "id_rsa",
    "id_dsa",
    "id_ecdsa",
    "id_ed25519",
    "id_ecdsa_sk",
    "id_ed25519_sk",
    "*.pem",
    "*.key",
    "*.p12",
    "*.pfx",
    "*.jks",
    "*.keystore",
    "*.kdbx",
    "*.ovpn",
    "*.ppk",
    ".bash_history",
    ".zsh_history",
    ".python_history",
    ".psql_history",
    ".mysql_history",
    ".node_repl_history",
)
"""Basename patterns, compared case-insensitively (DESIGN §10.1, SEC 5.1)."""

SOURCE_DENY_NAME_EXCEPTIONS: tuple[str, ...] = (".env.example", ".env.sample", ".env.template", ".env.dist")
"""Template files that match ``.env.*`` but carry no secrets."""

SOURCE_DENY_DIRS: tuple[str, ...] = (
    ".ssh",
    ".aws",
    ".gnupg",
    ".kube",
    ".docker",
    ".azure",
    ".config/gh",
    ".config/gcloud",
    ".password-store",
    ".config/op",
    ".config/github-copilot",
    ".local/share/keyrings",
    "Library/Keychains",
    "Library/Cookies",
    ".mozilla",
    ".config/google-chrome",
    ".config/chromium",
    "Library/Application Support/Google/Chrome",
    "AppData/Roaming/Microsoft/Credentials",
    "AppData/Roaming/Microsoft/Protect",
    "AppData/Local/Microsoft/Credentials",
)
"""Credential directories (component sequences, matched anywhere in a path; DESIGN §10.1)."""

TARGET_DENY: tuple[str, ...] = (
    *SOURCE_DENY_DIRS,
    ".bashrc",
    ".bash_profile",
    ".bash_login",
    ".bash_logout",
    ".profile",
    ".zshrc",
    ".zprofile",
    ".zshenv",
    ".zlogin",
    ".zlogout",
    ".config/fish",
    ".config/autostart",
    ".config/systemd/user",
    "Library/LaunchAgents",
    "AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup",
    ".local/bin",
    "bin",
    ".gitconfig",
    ".config/git",
    ".npmrc",
    ".pypirc",
    ".pip",
    ".config/pip",
)
"""Home-relative target denylist (DESIGN §10.4, R19); applies even inside ``allow_roots``."""

HERMES_CREDENTIAL_PATHS: tuple[str, ...] = (
    ".anthropic_oauth.json",
    ".aws",
    ".azure",
    ".config/gcloud",
    ".config/gh",
    ".copilot_jwt.json",
    ".docker",
    ".env",
    ".envrc",
    ".gnupg",
    ".kube",
    ".op.env",
    ".ssh",
    "auth.json",
    "auth/google_oauth.json",
    "backups",
    "bot-desktop",
    "browser-profile",
    "browser-profiles",
    "browser_auth",
    "browser_profiles",
    "bws_cache.enc.json",
    "bws_cache.json",
    "cache/bws_cache.enc.json",
    "cache/bws_cache.json",
    "cache/op_cache.json",
    "chrome-debug",
    "feishu_comment_pairing.json",
    "gateway-connection-tokens.json",
    "gateway-connections.json",
    "google_chat_user_client_secret.json",
    "google_chat_user_oauth_pending",
    "google_chat_user_oauth_pending.json",
    "google_chat_user_token.json",
    "google_chat_user_tokens",
    "google_client_secret.json",
    "google_oauth_pending.json",
    "google_token.json",
    "home",
    "honcho.json",
    "matrix/store",
    "mcp-tokens",
    "mem0.json",
    "npmrc",
    "op_cache.json",
    "pairing",
    "photon-sidecar.json",
    "platforms/matrix/store",
    "platforms/pairing",
    "platforms/whatsapp/session",
    "proxy",
    "runtime/photon-sidecar.json",
    "slack_tokens.json",
    "state-snapshots",
    "teams_pipeline_store.json",
    "vault",
    "webapp",
    "webhook_subscriptions.json",
    "weixin/accounts",
    "whatsapp/session",
    "workspace/meetings/node_token.json",
)
"""Copy of the core's private ``hermes_cli.profiles.PROFILE_CREDENTIAL_PATHS`` (main ``0f0e96fc8e``), relative
to a profile home. A K0 test fails when the core list grows beyond this copy."""

SYSTEM_DENY: tuple[tuple[str, ...], ...] = (
    ("etc", "shadow"),
    ("etc", "gshadow"),
    ("etc", "sudoers*"),
    ("etc", "ssl", "private"),
    ("private", "etc", "shadow"),
    ("private", "etc", "sudoers*"),
    ("private", "etc", "ssl", "private"),
)
"""Absolute system paths that are never releasable (component globs below ``/``; DESIGN §10.1, SEC 5.4).
``/private/etc`` is where ``/etc`` resolves on macOS."""

HERMES_FILE_KINDS: tuple[str, ...] = ("spillover", "hook_outputs", "native_inputs")
"""Session-file kinds (DESIGN §6.11) and the only Hermes-home areas the plugin may read from or place into:
``<home>/cache/spillover``, ``<home>/hook_outputs``, ``<document cache>/native-inputs``."""

_SNIFF_MARKERS: tuple[tuple[bytes, bytes], ...] = (
    (b"-----BEGIN ", rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----"),
    (b"PuTTY-User-Key-File-", rb"PuTTY-User-Key-File-"),
    (b"AGE-SECRET-KEY-1", rb"AGE-SECRET-KEY-1"),
    (b"PrivateKey", rb"^[ \t]*PrivateKey[ \t]*="),
)
"""Private-key markers of the content sniff: (a literal every match of the alternative contains, the regex
alternative)."""
_SNIFF_RE = re.compile(b"|".join(pattern for _, pattern in _SNIFF_MARKERS), re.MULTILINE)
_SNIFF_LITERALS = tuple(literal for literal, _ in _SNIFF_MARKERS)


@dataclass(frozen=True)
class Verdict:
    """Outcome of a policy check. ``reason`` is None when allowed; ``"suspected_secret"`` comes with
    ``allowed=True`` and means "only as a proposal, never automatic" (DESIGN §8.7)."""

    allowed: bool
    reason: str | None


_OK = Verdict(True, None)


@dataclass(frozen=True)
class PolicyConfig:
    """Resolved policy for one job on one host (see module docstring)."""

    max_file_bytes: int
    max_extra_dir_bytes: int
    max_hermes_file_bytes: int
    max_files: int
    max_untracked_files: int
    max_total_bytes: int
    allow_roots: tuple[Path, ...]
    export_roots: tuple[Path, ...]
    home: Path
    hermes_home: Path
    hermes_root: Path
    plugin_dir: Path
    include_secret: frozenset[str]
    case_insensitive: bool
    target_os: str
    document_cache: Path | None = None

    @classmethod
    def from_options(
        cls,
        o: Options,
        *,
        home: Path,
        hermes_home: Path,
        hermes_root: Path,
        plugin_dir: Path,
        case_insensitive: bool,
        target_os: str,
        include_secret: Iterable[str] = (),
        document_cache: Path | None = None,
    ) -> PolicyConfig:
        """Build the policy from resolved options. ``include_secret`` must hold only flags typed locally for this
        job; ``o.hints.include_secret`` is deliberately NOT read (a remote peer's hints may carry it, R18).
        ``plugin_dir`` is the plugin's data directory (``hostcompat.plugin_dir``)."""
        home = Path(home)
        lifted = frozenset(os.path.normpath(str(expand_root(p, home))) for p in include_secret if str(p).strip())
        return cls(
            max_file_bytes=o.max_file_bytes,
            max_extra_dir_bytes=o.max_extra_dir_bytes,
            max_hermes_file_bytes=o.max_hermes_file_bytes,
            max_files=o.max_files,
            max_untracked_files=o.max_untracked_files,
            max_total_bytes=o.max_total_bytes,
            allow_roots=tuple(expand_root(r, home) for r in o.allow_roots),
            export_roots=tuple(expand_root(r, home) for r in o.export_roots),
            home=home,
            hermes_home=Path(hermes_home),
            hermes_root=Path(hermes_root),
            plugin_dir=Path(plugin_dir),
            include_secret=lifted,
            case_insensitive=bool(case_insensitive),
            target_os=target_os,
            document_cache=Path(document_cache) if document_cache is not None else None,
        )

    def platform_rules(self) -> PlatformRules:
        """``paths.PlatformRules`` for paths written on this host."""
        return rules_for(self.target_os, self.case_insensitive)


# ---------------------------------------------------------------------------------------------------------
# Component helpers
# ---------------------------------------------------------------------------------------------------------


def _fold(text: str, case_insensitive: bool) -> str:
    text = unicodedata.normalize("NFC", text)
    return unicodedata.normalize("NFC", text.casefold()) if case_insensitive else text


_DRIVE_RE = re.compile(r"^[A-Za-z]:/")


def _components(rel_or_path: str) -> list[str]:
    """Path components of a POSIX or Windows spelling (both separators), without root, drive and ``.``."""
    text = str(rel_or_path).replace("\\", "/")
    if _DRIVE_RE.match(text):
        text = text[3:]
    return [c for c in text.split("/") if c not in {"", "."}]


def _seq(spec: str, case_insensitive: bool) -> tuple[str, ...]:
    return tuple(_fold(c, case_insensitive) for c in spec.split("/"))


def _seq_index(specs: Iterable[str], case_insensitive: bool) -> dict[str, tuple[tuple[str, ...], ...]]:
    """Folded component sequences of ``specs``, grouped by their first component."""
    index: dict[str, list[tuple[str, ...]]] = {}
    for spec in specs:
        seq = _seq(spec, case_insensitive)
        index.setdefault(seq[0], []).append(seq)
    return {first: tuple(seqs) for first, seqs in index.items()}


# Precompiled forms of the constant lists above (pure functions of those constants, no filesystem state).
_DENY_DIR_INDEX = {ci: _seq_index(SOURCE_DENY_DIRS, ci) for ci in (False, True)}
_DENY_NAME_RE = re.compile("|".join(f"(?:{fnmatch.translate(pattern)})" for pattern in SOURCE_DENY_NAMES))
_CREDENTIAL_SEQS = tuple(_seq(c, True) for c in HERMES_CREDENTIAL_PATHS)
_SYSTEM_DEPTH = max(len(pattern) for pattern in SYSTEM_DENY)
_SYSTEM_FIRST_RE = re.compile(
    "|".join(f"(?:{fnmatch.translate(first)})" for first in dict.fromkeys(p[0] for p in SYSTEM_DENY))
)


def _name_denied(basename: str) -> bool:
    name = _fold(basename, True)
    if name in SOURCE_DENY_NAME_EXCEPTIONS:
        return False
    return _DENY_NAME_RE.match(name) is not None


def _deny_dir_in(folded: tuple[str, ...], case_insensitive: bool) -> bool:
    """A ``SOURCE_DENY_DIRS`` sequence occurs anywhere in the folded components."""
    index = _DENY_DIR_INDEX[bool(case_insensitive)]
    for i, comp in enumerate(folded):
        for seq in index.get(comp, ()):
            if folded[i : i + len(seq)] == seq:
                return True
    return False


def is_denied_name(rel_or_path: str, *, case_insensitive: bool) -> bool:
    """True when the basename is on the secrets name list (always case-insensitive) or the path contains a
    credential directory of ``SOURCE_DENY_DIRS`` anywhere (casefold + NFC per component when
    ``case_insensitive``; DESIGN §10.1, SEC 5.2)."""
    comps = _components(rel_or_path)
    if not comps:
        return False
    if _name_denied(comps[-1]):
        return True
    return _deny_dir_in(tuple(_fold(c, case_insensitive) for c in comps), case_insensitive)


def _parts_abs(path: Path, case_insensitive: bool) -> tuple[str, ...]:
    """Folded components of an absolute path without its anchor."""
    parts = PurePath(path).parts[1:]
    return tuple(_fold(p, case_insensitive) for p in parts)


def _inside(path: Path, base: Path, case_insensitive: bool) -> bool:
    """``path`` equals or lies under ``base`` (lexical, folded)."""
    return under_any(Path(path), [Path(base)], case_insensitive=case_insensitive)


def _key(parts: tuple[str, ...], case_insensitive: bool) -> tuple[str, ...] | None:
    """The comparison key ``paths.under_any`` uses for the components of an absolute path: NFC per component
    (plus casefold and NFC again when ``case_insensitive``); None for a path with a ``..`` component, which is
    never under anything."""
    if ".." in parts:
        return None
    nfc = tuple(unicodedata.normalize("NFC", p) for p in parts)
    if case_insensitive:
        return tuple(unicodedata.normalize("NFC", p.casefold()) for p in nfc)
    return nfc


def _root_key(path: Path | str, case_insensitive: bool) -> tuple[str, ...] | None:
    """``_key`` of a root; None for a relative root (``under_any`` skips those)."""
    pure = PurePath(path)
    return _key(pure.parts, case_insensitive) if pure.is_absolute() else None


def _under(key: tuple[str, ...] | None, root: tuple[str, ...] | None) -> bool:
    """``paths.under_any`` for one root, on precomputed keys: ``key`` equals or lies under ``root``."""
    return key is not None and root is not None and key[: len(root)] == root


class _Cand(NamedTuple):
    """One spelling of a checked path (as given, normalised, or resolved): text, components, ``_key``."""

    text: str
    parts: tuple[str, ...]
    key: tuple[str, ...] | None


def _cand(path: Path | str, case_insensitive: bool) -> _Cand:
    pure = PurePath(path)
    return _Cand(str(pure), pure.parts, _key(pure.parts, case_insensitive) if pure.is_absolute() else None)


def _system_denied(path: Path) -> bool:
    return _system_denied_parts(PurePath(path).parts)


def _system_denied_parts(parts: tuple[str, ...]) -> bool:
    """``SYSTEM_DENY`` on the components after the anchor (only the first ``_SYSTEM_DEPTH`` can match; a first
    component no pattern starts with ends the check)."""
    if len(parts) < 2:
        return False
    first = _fold(parts[1], True)
    if _SYSTEM_FIRST_RE.match(first) is None:
        return False
    head = (first, *(_fold(p, True) for p in parts[2 : 1 + _SYSTEM_DEPTH]))
    for pattern in SYSTEM_DENY:
        if len(head) >= len(pattern) and all(
            fnmatch.fnmatchcase(p, q) for p, q in zip(head[: len(pattern)], pattern, strict=False)
        ):
            return True
    return False


def _candidates(path: Path) -> tuple[Path, Path]:
    """(lexically normalised absolute path, realpath)."""
    lexical = Path(os.path.normpath(path))
    return lexical, Path(os.path.realpath(path))


# ---------------------------------------------------------------------------------------------------------
# Per-scan cache
# ---------------------------------------------------------------------------------------------------------

_REAL_MEMO_MAX = 65536
_RESOLVE_BY_PARENT = os.name == "posix"


class SourceCheckCache:
    """Memo for the source checks of **one** scan (one tree walk, one ``git ls-files`` listing).

    Create it right before the scan, pass it as ``cache=`` to every ``check_source_file``/``check_extra``/
    ``is_hermes_protected`` call of that scan and drop it afterwards — never keep one across scans or jobs.
    It remembers filesystem answers from the moment they were first needed: the realpaths of the Hermes
    bases, ``$HOME``, the export roots, the item roots and (POSIX) of parent directories; a directory swapped
    for a symlink later in the same scan is therefore judged by its earlier realpath. Every checked path itself
    is still ``lstat``-ed, every link still resolved in full and every regular file still sniffed per call.
    The cache serves one ``PolicyConfig`` at a time; used with a different one it starts over. Without a cache
    every call uses a fresh one, i.e. resolves everything itself.
    """

    __slots__ = ("_cfg", "_credential", "_item_roots", "_locations", "_protected", "_real")

    def __init__(self) -> None:
        self._cfg: PolicyConfig | None = None
        self._clear()

    def _clear(self) -> None:
        self._protected: tuple[tuple[str, ...], ...] | None = None
        self._credential: tuple[tuple[str, ...], ...] | None = None
        self._locations: tuple[tuple[str, ...] | None, ...] | None = None
        self._item_roots: dict[str, tuple[str, ...] | None] = {}
        self._real: dict[str, str] = {}

    def _bind(self, cfg: PolicyConfig) -> SourceCheckCache:
        """Serve ``cfg``; a different configuration starts over. Touches no file (everything is resolved on
        first use, i.e. after the caller's device-prefix check)."""
        if self._cfg is not cfg and self._cfg != cfg:
            self._cfg = cfg
            self._clear()
        return self

    def _resolve(self, path: Path | str) -> str:
        """``os.path.realpath(path)``, remembered for this scan (directories: bases, roots, parents)."""
        text = os.fspath(path)
        real = self._real.get(text)
        if real is None:
            if len(self._real) >= _REAL_MEMO_MAX:
                self._real.clear()
            real = self._real[text] = os.path.realpath(text)
        return real

    def _protected_keys(self) -> tuple[tuple[str, ...], ...]:
        """Keys of ``_hermes_bases`` (Hermes root, active home, plugin data dir; as given and resolved)."""
        if self._protected is None:
            ci = self._cfg.case_insensitive
            keys = (_root_key(b, ci) for b in _hermes_bases(self._cfg, self._resolve))
            self._protected = tuple(k for k in keys if k is not None)
        return self._protected

    def _credential_keys(self) -> tuple[tuple[str, ...], ...]:
        """Keys of the bases whose credential stores are protected (active home and Hermes root, as given and
        resolved)."""
        if self._credential is None:
            cfg = self._cfg
            keys = []
            for base in (cfg.hermes_home, cfg.hermes_root):
                for b in {Path(base), Path(self._resolve(base))}:
                    key = _root_key(b, cfg.case_insensitive)
                    if key is not None:
                        keys.append(key)
            self._credential = tuple(keys)
        return self._credential

    def _location_keys(self) -> tuple[tuple[str, ...] | None, ...]:
        """Keys of the resolved ``$HOME`` and export roots (where a file without item root must lie)."""
        if self._locations is None:
            cfg = self._cfg
            roots = (cfg.home, *cfg.export_roots)
            self._locations = tuple(_root_key(self._resolve(r), cfg.case_insensitive) for r in roots)
        return self._locations

    def _item_root_key(self, item_root: Path) -> tuple[str, ...] | None:
        text = os.fspath(item_root)
        if text not in self._item_roots:
            self._item_roots[text] = _root_key(self._resolve(text), self._cfg.case_insensitive)
        return self._item_roots[text]

    def _realpath(self, path: Path, lst: os.stat_result) -> str:
        """``os.path.realpath(path)`` for a path whose ``lstat`` is ``lst``. On POSIX the realpath of a
        non-link is its parent's realpath plus its name, so only the parent is resolved (and remembered);
        a link, ``..`` or the root is resolved in full, every time."""
        text = os.fspath(path)
        if _RESOLVE_BY_PARENT and not stat.S_ISLNK(lst.st_mode):
            head, tail = os.path.split(text)
            if head and tail and tail not in {".", ".."}:
                return os.path.join(self._resolve(head), tail)
        return os.path.realpath(text)


def _memo(cache: SourceCheckCache | None, cfg: PolicyConfig) -> SourceCheckCache:
    return (cache if cache is not None else SourceCheckCache())._bind(cfg)


# ---------------------------------------------------------------------------------------------------------
# Hermes protection
# ---------------------------------------------------------------------------------------------------------


def _credential_in(cand: _Cand, memo: SourceCheckCache) -> bool:
    """``cand`` lies in a Hermes credential store of the active home or the Hermes root (also of any profile
    below the root)."""
    for base in memo._credential_keys():
        if not _under(cand.key, base):
            continue
        rel = tuple(_fold(p, True) for p in cand.parts[len(base) :])
        if len(rel) >= 2 and rel[0] == "profiles" and any(rel[2 : 2 + len(c)] == c for c in _CREDENTIAL_SEQS):
            return True
        if any(rel[: len(c)] == c for c in _CREDENTIAL_SEQS):
            return True
    return False


def _hermes_credential(path: Path, cfg: PolicyConfig) -> bool:
    return _credential_in(_cand(path, cfg.case_insensitive), _memo(None, cfg))


def _session_area(kind: str, cfg: PolicyConfig) -> Path | None:
    if kind == "spillover":
        return cfg.hermes_home / "cache" / "spillover"
    if kind == "hook_outputs":
        return cfg.hermes_home / "hook_outputs"
    if kind == "native_inputs":
        cache = cfg.document_cache if cfg.document_cache is not None else cfg.hermes_home / "cache" / "documents"
        return cache / "native-inputs"
    return None


def _in_session_area(path: Path, kind: str, cfg: PolicyConfig) -> bool:
    area = _session_area(kind, cfg)
    if area is None:
        return False
    ci = cfg.case_insensitive
    return any(_inside(path, a, ci) and Path(path) != a for a in {area, Path(os.path.realpath(area))})


def _hermes_bases(cfg: PolicyConfig, resolve: Callable[[Path], str] = os.path.realpath) -> list[Path]:
    bases = []
    for b in (cfg.hermes_root, cfg.hermes_home, cfg.plugin_dir):
        bases.append(Path(b))
        bases.append(Path(resolve(b)))
    return bases


def _protected(
    cands: tuple[_Cand, ...], cfg: PolicyConfig, memo: SourceCheckCache, session_file_kind: str | None
) -> bool:
    for cand in cands:
        if _credential_in(cand, memo):
            return True
        if session_file_kind is not None and _in_session_area(Path(cand.text), session_file_kind, cfg):
            continue
        if any(_under(cand.key, base) for base in memo._protected_keys()):
            return True
    return False


def is_hermes_protected(
    path: Path, cfg: PolicyConfig, *, session_file_kind: str | None = None, cache: SourceCheckCache | None = None
) -> bool:
    """True when ``path`` (as given or resolved) lies in the Hermes root, the active Hermes home, the plugin data
    dir or a Hermes credential store — never liftable (R19, SEC 2.4). With ``session_file_kind`` (one of
    ``HERMES_FILE_KINDS``), a path inside that kind's area is not protected unless it is a credential store.
    ``cache``: the ``SourceCheckCache`` of the current scan (optional)."""
    if session_file_kind is not None and session_file_kind not in HERMES_FILE_KINDS:
        raise PolicyDenied(f"unknown session file kind {session_file_kind!r}", reason="hermes")
    ci = cfg.case_insensitive
    cands = tuple(_cand(c, ci) for c in _candidates(Path(path)))
    return _protected(cands, cfg, _memo(cache, cfg), session_file_kind)


# ---------------------------------------------------------------------------------------------------------
# Content sniffing
# ---------------------------------------------------------------------------------------------------------


def _head(path: Path, *, follow: bool) -> bytes | None:
    """First ``SNIFF_BYTES`` of a regular file, or None when it cannot be read as one."""
    flags = os.O_RDONLY | getattr(os, "O_NONBLOCK", 0)
    if not follow:
        flags |= getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        return None
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            return None
        chunks = []
        remaining = SNIFF_BYTES
        while remaining > 0:
            chunk = os.read(fd, remaining)
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        return b"".join(chunks)
    except OSError:
        return None
    finally:
        os.close(fd)


def _has_key_marker(head: bytes) -> bool:
    """``_SNIFF_RE`` finds a marker in ``head``. The regex runs only when one of the literals every match
    contains is present: a substring search costs a fraction of the multiline regex on an 8 KiB head."""
    return any(literal in head for literal in _SNIFF_LITERALS) and _SNIFF_RE.search(head) is not None


def sniff_secret(path: Path) -> bool:
    """True when the first 8 KiB carry a private-key marker (PEM/OpenSSH ``PRIVATE KEY`` block, PuTTY, age,
    WireGuard ``PrivateKey =``). Fail-closed: a file that cannot be read as a regular file counts as secret."""
    head = _head(Path(path), follow=True)
    return True if head is None else _has_key_marker(head)


def suspected_secret(path: Path, redact: Callable[[str], str]) -> bool:
    """True when the first 8 KiB, read as text, change under ``redact`` (``redact_sensitive_text(force=True)``):
    a token-looking value is present (DESIGN §8.7). Binary content (NUL byte) is not text and returns False;
    an unreadable file returns True."""
    head = _head(Path(path), follow=True)
    if head is None:
        return True
    if b"\x00" in head:
        return False
    text = head.decode("utf-8", errors="replace")
    return redact(text) != text


# ---------------------------------------------------------------------------------------------------------
# Source checks
# ---------------------------------------------------------------------------------------------------------


def _lifted(cfg: PolicyConfig, *paths: Path) -> bool:
    return any(os.path.normpath(str(p)) in cfg.include_secret for p in paths)


def check_source_file(
    path: Path,
    cfg: PolicyConfig,
    *,
    explicit: bool,
    item_root: Path | None = None,
    cache: SourceCheckCache | None = None,
) -> Verdict:
    """May this source file or directory be transferred? (DESIGN §10.1–§10.3)

    Order: system paths and the Hermes root/credential stores (never liftable, decided before any read), symlinks
    (only an ``explicit`` path — an item root or an ``--include`` — is resolved, everything else is recorded as a
    link by the walker), special files, the name/directory denylist on the given path and its realpath, the
    location (``item_root`` given: the realpath must stay inside it; otherwise under ``$HOME`` or an
    ``export_roots`` entry), the size limit (``explicit`` lifts it), and the content sniff of every regular file
    (hard-linked files included, SEC 5.3). ``include_secret`` lifts the denylist and the sniff for those paths.

    Callers: a file inside a workspace item passes that item's root as ``item_root``; the primary workspace root
    itself is checked with ``item_root=<itself>`` (it may lie outside ``$HOME``, DESIGN §8.2); extras pass no
    ``item_root`` and use ``check_extra``. Directories get every check except size and sniff. A scan over many
    paths passes one ``SourceCheckCache`` as ``cache`` (created per scan, see there); the verdicts are the same.
    """
    raw = os.fspath(path)
    try:
        reject_windows_device_prefix(raw)
    except PolicyDenied:
        return Verdict(False, "special")
    p = Path(raw)
    if not p.is_absolute():
        return Verdict(False, "special")
    try:
        lst = os.lstat(p)
    except FileNotFoundError:
        return Verdict(False, "missing")
    except OSError:
        return Verdict(False, "unreadable")
    memo = _memo(cache, cfg)
    ci = cfg.case_insensitive
    lexical = _cand(os.path.normpath(p), ci)
    real = _cand(memo._realpath(p, lst), ci)
    if _system_denied_parts(lexical.parts) or _system_denied_parts(real.parts):
        return Verdict(False, "secret")
    if _protected((lexical, real), cfg, memo, None):
        return Verdict(False, "hermes")
    if stat.S_ISLNK(lst.st_mode):
        if not explicit:
            return Verdict(False, "symlink")
        try:
            st = os.stat(p)
        except FileNotFoundError:
            return Verdict(False, "missing")
        except OSError:
            return Verdict(False, "unreadable")
    else:
        st = lst
    if not (stat.S_ISREG(st.st_mode) or stat.S_ISDIR(st.st_mode)):
        return Verdict(False, "special")
    lifted = _lifted(cfg, lexical.text, real.text)
    denied = is_denied_name(lexical.text, case_insensitive=ci) or is_denied_name(real.text, case_insensitive=ci)
    if denied and not lifted:
        return Verdict(False, "secret")
    if item_root is not None:
        if not _under(real.key, memo._item_root_key(item_root)):
            return Verdict(False, "symlink")
    elif not any(_under(real.key, root) for root in memo._location_keys()):
        return Verdict(False, "outside_home")
    if stat.S_ISDIR(st.st_mode):
        return _OK
    if st.st_size > cfg.max_file_bytes and not explicit:
        return Verdict(False, "size")
    head = _head(p, follow=explicit)
    if head is None:
        return Verdict(False, "unreadable")
    if not lifted and _has_key_marker(head):
        return Verdict(False, "secret")
    return _OK


def check_extra(
    path: Path,
    cfg: PolicyConfig,
    *,
    explicit: bool,
    redact: Callable[[str], str],
    cache: SourceCheckCache | None = None,
) -> Verdict:
    """``check_source_file`` for an extra path outside the workspace, plus the suspected-secret rule: a regular
    file whose first 8 KiB change under ``redact`` is allowed only as a proposal (``reason="suspected_secret"``,
    DESIGN §8.7, SEC 5.1). ``include_secret`` on the path skips that rule. ``cache`` as in ``check_source_file``."""
    verdict = check_source_file(path, cfg, explicit=explicit, cache=cache)
    if not verdict.allowed:
        return verdict
    lexical, real = _candidates(Path(path))
    if os.path.isdir(real) or _lifted(cfg, lexical, real):
        return verdict
    if suspected_secret(real, redact):
        return Verdict(True, "suspected_secret")
    return verdict


# ---------------------------------------------------------------------------------------------------------
# Target checks
# ---------------------------------------------------------------------------------------------------------


def _target_denied(path: Path, cfg: PolicyConfig) -> bool:
    ci = cfg.case_insensitive
    for base in _hermes_bases(cfg):
        if _inside(path, base, ci) or _inside(base, path, ci):
            return True
    homes = {Path(cfg.home), Path(os.path.realpath(cfg.home))}
    for home in homes:
        if _inside(home, path, ci):
            return True
        if not _inside(path, home, ci):
            continue
        rel = _parts_abs(path, ci)[len(PurePath(home).parts) - 1 :]
        for spec in TARGET_DENY:
            deny = _seq(spec, ci)
            n = min(len(rel), len(deny))
            if rel[:n] == deny[:n]:
                return True
        if rel and rel[0].startswith("."):
            opened = False
            for root in cfg.allow_roots:
                for r in {Path(root), Path(os.path.realpath(root))}:
                    if not _inside(r, home, ci):
                        continue
                    root_rel = _parts_abs(r, ci)[len(PurePath(home).parts) - 1 :]
                    if root_rel and root_rel[0] == rel[0] and _inside(path, r, ci):
                        opened = True
            if not opened:
                return True
    return False


def check_target_dest(
    path: Path, cfg: PolicyConfig, *, recorded: bool, local_choice: bool, hermes_file_kind: str | None = None
) -> Verdict:
    """May the plugin write at ``path`` on this host? (DESIGN §10.4, R19)

    The target denylist is checked on the path as given and on its realpath (the existing prefix resolved) and
    applies always. Then the destination must lie under ``allow_roots`` (realpath against the roots' realpaths),
    unless it is a path this host recorded itself (``recorded``, return trip) or the local user typed
    (``local_choice``). ``hermes_file_kind`` (one of ``HERMES_FILE_KINDS``) is the plugin's own placement of a
    session file: allowed only inside that kind's area of this host's Hermes home, never into a credential store.
    """
    raw = os.fspath(path)
    try:
        reject_windows_device_prefix(raw)
    except PolicyDenied:
        return Verdict(False, "target_denied")
    p = Path(raw)
    if not p.is_absolute():
        return Verdict(False, "not_allowed_root")
    lexical, real = _candidates(p)
    for candidate in (lexical, real):
        if _system_denied(candidate):
            return Verdict(False, "target_denied")
        if hermes_file_kind is not None:
            if hermes_file_kind not in HERMES_FILE_KINDS:
                return Verdict(False, "target_denied")
            if not _in_session_area(candidate, hermes_file_kind, cfg) or _hermes_credential(candidate, cfg):
                return Verdict(False, "target_denied")
            continue
        if _target_denied(candidate, cfg):
            return Verdict(False, "target_denied")
    if hermes_file_kind is not None:
        return _OK
    if not (recorded or local_choice):
        roots = [Path(os.path.realpath(r)) for r in cfg.allow_roots]
        if not under_any(real, roots, case_insensitive=cfg.case_insensitive):
            return Verdict(False, "not_allowed_root")
    return _OK


# ---------------------------------------------------------------------------------------------------------
# Tree manifests and git pathspecs
# ---------------------------------------------------------------------------------------------------------

_TYPE_BITS = {"file": 0o100000, "dir": 0o040000, "symlink": 0o120000}
_ALLOWED_PERMS = {"file": {0o644, 0o755}, "dir": {0o644, 0o755}, "symlink": {0, 0o644, 0o755, 0o777}}


def _parse_mode(mode: object) -> int | None:
    if isinstance(mode, bool):
        return None
    if isinstance(mode, int):
        return mode if mode >= 0 else None
    if isinstance(mode, str) and re.fullmatch(r"[0-7]{1,7}", mode):
        return int(mode, 8)
    return None


def _link_target_inside(link_path: str, target: str) -> bool:
    if not target or "\x00" in target or any(unicodedata.category(c) == "Cc" for c in target):
        return False
    if target.startswith(("/", "\\")) or re.match(r"^[A-Za-z]:", target):
        return False
    stack = [c for c in link_path.replace("\\", "/").split("/")[:-1] if c not in {"", "."}]
    for comp in target.replace("\\", "/").split("/"):
        if comp in {"", "."}:
            continue
        if comp == "..":
            if not stack:
                return False
            stack.pop()
            continue
        folded = unicodedata.normalize("NFC", comp).casefold().rstrip(". ")
        if folded in {".git", "git~1"}:
            return False
        stack.append(comp)
    return True


def check_tree_entry(entry: dict) -> Verdict:
    """Validate one tree-manifest entry from a peer (DESIGN §10.3, SEC 2.3): ``type`` ∈ file|dir|symlink, mode
    permission bits 0644 or 0755 (symlinks: 0644/0755/0777 or none), no setuid/setgid/sticky, type bits (when
    present, e.g. ``100644``) matching the type, and a symlink target that is relative, stays inside the tree
    and does not point into a ``.git`` directory. The entry's own ``path`` is checked separately with
    ``paths.check_rel``."""
    if not isinstance(entry, dict):
        return Verdict(False, "special")
    kind = entry.get("type")
    if kind not in _TYPE_BITS:
        return Verdict(False, "special")
    mode = _parse_mode(entry.get("mode", 0 if kind == "symlink" else None))
    if mode is None:
        return Verdict(False, "special")
    type_bits = mode & 0o170000
    if type_bits and type_bits != _TYPE_BITS[kind]:
        return Verdict(False, "special")
    if mode & ~0o177777:
        return Verdict(False, "special")
    perm = mode & 0o7777
    if perm & 0o7000 or perm not in _ALLOWED_PERMS[kind]:
        return Verdict(False, "special")
    size = entry.get("size", 0)
    if isinstance(size, bool) or not isinstance(size, int) or size < 0:
        return Verdict(False, "special")
    if kind == "symlink":
        target, link_path = entry.get("target"), entry.get("path")
        if not isinstance(target, str) or not isinstance(link_path, str):
            return Verdict(False, "symlink")
        if not _link_target_inside(link_path, target):
            return Verdict(False, "symlink")
    return _OK


def exclude_pathspecs(paths: Iterable[str]) -> tuple[str, ...]:
    """git pathspecs ``:(exclude,literal)<p>`` for repo-relative paths (sorted, unique). Paths with NUL, CR or LF
    raise ``PolicyDenied``."""
    out = set()
    for p in paths:
        if not isinstance(p, str) or not p or any(ch in p for ch in "\x00\r\n"):
            raise PolicyDenied("invalid path for a git pathspec", reason="special", path=repr(p)[:200])
        out.add(":(exclude,literal)" + p)
    return tuple(sorted(out))
