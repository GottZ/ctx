"""Session files under HERMES_HOME (layer L3, stdlib only; DESIGN §6.11, §10.6, INTERFACES §4.1).

Three kinds of files belong to a session without living in its workspace; their kind strings are K0's
``policy.HERMES_FILE_KINDS``:

* ``spillover`` — large tool results, ``<home>/cache/spillover/<name>``, named in the transcript by the marker
  ``Full output saved to: <path>`` (``tools/tool_result_storage.py:411``);
* ``hook_outputs`` — truncated hook output, ``<home>/hook_outputs/<session id>/<name>``, marker ``… full content
  saved to <path>]`` (``tools/hook_output_spill.py:105-113``) plus every file in the lineage's per-session
  directories;
* ``native_inputs`` — retained images, ``<document cache>/native-inputs/<sha256>/<name>``, marker
  ``[Image attached at: <path>]`` (``agent/image_replay.py:34``); replay only accepts a file whose sha256 equals
  its directory name (``agent/image_replay.py:75-90``).

The source side discovers references (``references``) and files (``discover``, with the plan's policy). The
target side places a received file from its **kind and checked basename only** (``target_path``): a peer's
``rel`` or ``source_path`` never chooses a directory (SEC 2.3), and the native-input directory is the sha256 of
the bytes actually received.
"""

from __future__ import annotations

import os
import re
import stat
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path, PurePosixPath, PureWindowsPath

from .errors import PolicyDenied
from .model import HermesPaths
from .paths import check_rel, rules_for
from .policy import HERMES_FILE_KINDS, PolicyConfig, is_hermes_protected

SPILLOVER, HOOK_OUTPUTS, NATIVE_INPUTS = HERMES_FILE_KINDS

SPILL_RE: re.Pattern = re.compile(r"^Full output saved to: (.+)$", re.MULTILINE)
HOOK_RE: re.Pattern = re.compile(r"full content saved to (.+)\]$", re.MULTILINE)
IMAGE_RE: re.Pattern = re.compile(r"^\[Image attached at: (.+)\]$", re.MULTILINE)

_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
_TEXT_FIELDS = ("content", "api_content")
_MARKERS = ((SPILLOVER, SPILL_RE), (HOOK_OUTPUTS, HOOK_RE), (NATIVE_INPUTS, IMAGE_RE))
_POSIX_RULES = rules_for("linux", False)


@dataclass(frozen=True)
class HermesFile:
    """One session file on the source: ``rel`` is relative to its kind's area root (DESIGN §6.2)."""

    kind: str
    source_path: str
    rel: str
    size: int
    session_id: str | None


def area_root(kind: str, hermes: HermesPaths) -> Path:
    """The directory of ``kind`` under ``hermes`` (the anchor of ``HermesFile.rel``)."""
    if kind == SPILLOVER:
        return Path(hermes.home) / "cache" / "spillover"
    if kind == HOOK_OUTPUTS:
        return Path(hermes.home) / "hook_outputs"
    if kind == NATIVE_INPUTS:
        return Path(hermes.document_cache) / "native-inputs"
    raise PolicyDenied(f"unknown session file kind {kind!r}", reason="hermes")


def _texts(value: object) -> Iterable[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, list):
        for part in value:
            if isinstance(part, Mapping):
                text = part.get("text")
                if isinstance(text, str):
                    yield text
            elif isinstance(part, str):
                yield part


def _rel_in_area(path: str, kind: str, hermes: HermesPaths) -> str | None:
    """``path`` relative to the kind's area when it lies there with the kind's shape, else ``None``."""
    try:
        p = Path(path)
    except (TypeError, ValueError):
        return None
    if not p.is_absolute() or "\x00" in path:
        return None
    root = area_root(kind, hermes)
    try:
        rel = p.relative_to(root)
    except ValueError:
        return None
    parts = rel.parts
    if kind == SPILLOVER and len(parts) != 1:
        return None
    if kind in (HOOK_OUTPUTS, NATIVE_INPUTS) and len(parts) != 2:
        return None
    if kind == NATIVE_INPUTS and not _SHA256_RE.match(parts[0]):
        return None
    rel_text = "/".join(parts)
    try:
        check_rel(rel_text, _POSIX_RULES)
    except PolicyDenied:
        return None
    return rel_text


def references(rows: Iterable[Mapping], lineage_ids: Sequence[str], hermes: HermesPaths) -> list[dict]:
    """Session-file references of a transcript plus the lineage's hook-output directories (no policy, no reads).

    Returns ``{"kind", "source_path", "rel", "session_id"}`` dicts (the capsule's ``hermes_file_refs``),
    unique by ``source_path``, in discovery order. Only paths inside the kind's own area count.
    """
    out: dict[str, dict] = {}

    def add(kind: str, path: str) -> None:
        path = path.strip()
        rel = _rel_in_area(path, kind, hermes)
        if rel is None or path in out:
            return
        session_id = rel.split("/", 1)[0] if kind == HOOK_OUTPUTS else None
        out[path] = {"kind": kind, "source_path": path, "rel": rel, "session_id": session_id}

    for row in rows:
        for field in _TEXT_FIELDS:
            for text in _texts(row.get(field)):
                for kind, pattern in _MARKERS:
                    if pattern is IMAGE_RE and "Image attached at" not in text:
                        continue
                    for match in pattern.finditer(text):
                        add(kind, match.group(1))
    hook_root = area_root(HOOK_OUTPUTS, hermes)
    for sid in lineage_ids:
        directory = hook_root / str(sid)
        try:
            entries = sorted(os.scandir(directory), key=lambda e: e.name)
        except OSError:
            continue
        for entry in entries:
            try:
                if entry.is_file(follow_symlinks=False):
                    add(HOOK_OUTPUTS, os.path.join(directory, entry.name))
            except OSError:
                continue
    return list(out.values())


def discover(
    rows: Iterable[Mapping],
    lineage_ids: Sequence[str],
    hermes: HermesPaths,
    cfg: PolicyConfig,
    *,
    rejected: list[tuple[str, str]] | None = None,
) -> list[HermesFile]:
    """Session files the plan may carry: references that exist as regular files (no symlinks) within
    ``cfg.max_hermes_file_bytes`` and outside every Hermes credential store. ``rejected`` (optional) collects
    ``(source_path, reason)`` for the rest (``missing`` | ``symlink`` | ``special`` | ``size`` | ``hermes``)."""
    found: list[HermesFile] = []
    for ref in references(rows, lineage_ids, hermes):
        path = ref["source_path"]
        reason = None
        try:
            st = os.lstat(path)
        except FileNotFoundError:
            reason = "missing"
        except OSError:
            reason = "unreadable"
        else:
            if stat.S_ISLNK(st.st_mode):
                reason = "symlink"
            elif not stat.S_ISREG(st.st_mode):
                reason = "special"
            elif st.st_size > cfg.max_hermes_file_bytes:
                reason = "size"
            elif _resolves_outside_area(path, ref["kind"], hermes) or is_hermes_protected(
                Path(path), cfg, session_file_kind=ref["kind"]
            ):
                reason = "hermes"
        if reason is not None:
            if rejected is not None:
                rejected.append((path, reason))
            continue
        found.append(HermesFile(ref["kind"], path, ref["rel"], int(st.st_size), ref["session_id"]))
    return found


def _resolves_outside_area(path: str, kind: str, hermes: HermesPaths) -> bool:
    root = os.path.realpath(area_root(kind, hermes))
    real = os.path.realpath(path)
    return os.path.commonpath([root, real]) != root or real == root


def checked_basename(name: str) -> str:
    """The last component of a peer-supplied name, validated as one safe path component (``PolicyDenied``).

    Both separators count, so ``../../.bashrc`` and ``..\\x`` reduce to their final component; the result must
    pass ``paths.check_rel`` for POSIX and Windows targets and contain no separator.
    """
    if not isinstance(name, str) or not name:
        raise PolicyDenied("empty session file name", reason="empty")
    base = PureWindowsPath(PurePosixPath(name).name).name
    if not base or base in {".", ".."} or "/" in base or "\\" in base:
        raise PolicyDenied(f"unsafe session file name {name[:80]!r}", reason="dot_or_empty_component")
    check_rel(base, _POSIX_RULES)
    check_rel(base, rules_for("windows", True))
    return base


def _session_segment(session_id: str) -> str:
    """The per-session directory name, sanitised like ``tools/hook_output_spill.py:_resolve_spill_dir``."""
    segment = (session_id or "no-session").replace("/", "_").replace("\\", "_").replace("..", "_")
    check_rel(segment, _POSIX_RULES)
    return segment


def target_path(kind: str, basename: str, content_sha256: str, target: HermesPaths, new_session_id: str) -> Path:
    """Where a received session file goes on this host — from ``kind`` and the checked ``basename`` only.

    ``spillover`` → ``<home>/cache/spillover/<name>``; ``hook_outputs`` → ``<home>/hook_outputs/<new session id>/
    <name>``; ``native_inputs`` → ``<document cache>/native-inputs/<sha256 of the received bytes>/<name>``.
    """
    if kind not in HERMES_FILE_KINDS:
        raise PolicyDenied(f"unknown session file kind {kind!r}", reason="hermes")
    name = checked_basename(basename)
    root = area_root(kind, target)
    if kind == SPILLOVER:
        return root / name
    if kind == HOOK_OUTPUTS:
        return root / _session_segment(new_session_id) / name
    if not isinstance(content_sha256, str) or not _SHA256_RE.match(content_sha256):
        raise PolicyDenied("native input needs the sha256 of the received bytes", reason="hermes")
    return root / content_sha256 / name


def placement(ref: Mapping, content_sha256: str, target: HermesPaths, new_session_id: str) -> Path:
    """``target_path`` for a peer-supplied reference: only ``kind`` and the basename of ``rel`` are used."""
    return target_path(str(ref.get("kind")), str(ref.get("rel") or ""), content_sha256, target, new_session_id)
