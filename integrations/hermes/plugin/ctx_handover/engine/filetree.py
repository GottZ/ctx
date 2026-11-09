"""File trees without git: scan, pack, materialise (layer L4, stdlib only; DESIGN §9.6, §10.3–§10.5).

Source: ``scan`` walks with ``os.scandir`` on raw byte names, never follows a symlink, applies the ignore rules
(``ignore.IgnoreMatcher``) and the secrets policy (``policy.check_source_file``), records links and skips special
files; ``pack_tree`` streams every file into blocks and writes a ``ctxh.tree`` manifest, also streamed (canonical
JSON, entries sorted).

Target: ``prepare_fresh`` builds the tree inside a 0700 partial directory — directories first, then files
(``O_EXCL|O_NOFOLLOW``, 0600 while written, then the recorded exec bit under the umask, mtime restored), symlinks
last and only when relative and inside the tree; ``commit`` renames the partial onto the destination.
``prepare_update`` is allowed only when the destination still equals the plugin's last fingerprint; changed
files are staged beside their destination and swapped in at commit after a ``(size, mtime_ns, inode)``
re-check, with backups for the inverse; a path that was free at PREPARE is only ever filled, never replaced
(H10). Both modes check the declared bytes against the target's cap and the free space before the first write
(H11).

Names: a manifest entry carries ``path`` (UTF-8) and, for a name that is not valid UTF-8, ``raw`` (base64 of the
bytes; ``path`` is then only a display form). POSIX targets accept control characters and non-UTF-8 bytes in
names (they are legal there); every other rule of ``paths.check_rel`` applies on every target.
"""

from __future__ import annotations

import base64
import contextlib
import errno
import hashlib
import json
import os
import shutil
import stat
from collections.abc import Callable, Iterator, Sequence
from dataclasses import dataclass, field
from pathlib import Path

from .contracts import BlockSink, BlockSource
from .errors import BadRequest, BlockCorrupt, ConflictError, InsufficientStorage, PayloadTooLarge, PolicyDenied
from .gitops import (
    dec_path,
    enc_path,
    object_manifest,
    open_partial,
    put_stream,
    read_json_object,
    sha256_file,
    undo_failed,
    write_object_to,
)
from .ignore import IgnoreMatcher
from .model import DestMode, NotTransferred, ObjectManifest
from .paths import PlatformRules, check_rel, fold_key
from .policy import PolicyConfig, SourceCheckCache, check_source_file, check_tree_entry, is_denied_name

TREE_KIND = "ctxh.tree"
TREE_MEDIA = "application/json"
FILE_MEDIA = "application/octet-stream"
FP_VERSION = 1
DEFAULT_MAX_BYTES = 10 * 1024**3  # ``Options.max_total_bytes``; callers pass the target's own cap
_DISK_MARGIN = 1.1


def _never() -> bool:
    return False


@dataclass(frozen=True)
class TreeEntry:
    """One manifest entry (DESIGN §9.1). ``raw`` (addition): base64 of the name bytes when they are not UTF-8."""

    path: str
    type: str
    mode: str
    size: int
    mtime_ns: int
    object: str | None
    target: str | None
    raw: str | None = None


@dataclass(frozen=True)
class TreeApplyState:
    """A prepared tree item (INTERFACES §4.3 plus additions with defaults)."""

    dest: Path
    mode: DestMode
    partial: Path | None
    staged: tuple[tuple[Path, Path], ...]
    backups: tuple[tuple[Path, Path], ...]
    removed: tuple[Path, ...]
    skipped: tuple[NotTransferred, ...] = ()
    fingerprint: dict = field(default_factory=dict)
    expect: tuple[tuple[Path, int, int, int], ...] = ()
    created_dirs: tuple[Path, ...] = ()
    fallback: Path | None = None
    staged_meta: tuple[tuple[int, int, int, str], ...] = ()


# ---------------------------------------------------------------------------------------------------------
# Names
# ---------------------------------------------------------------------------------------------------------


def _name_fields(rel: bytes) -> tuple[str, str | None]:
    try:
        return rel.decode("utf-8"), None
    except UnicodeDecodeError:
        return rel.decode("utf-8", errors="backslashreplace"), base64.b64encode(rel).decode("ascii")


def entry_rel_bytes(entry: dict | TreeEntry) -> bytes:
    """The raw relative name of a manifest entry (``raw`` when present, else ``path`` as UTF-8)."""
    raw = entry.raw if isinstance(entry, TreeEntry) else entry.get("raw")
    path = entry.path if isinstance(entry, TreeEntry) else entry.get("path")
    if raw is not None:
        if not isinstance(raw, str):
            raise BadRequest("raw name must be base64", field="raw")
        try:
            return base64.b64decode(raw, validate=True)
        except ValueError:
            raise BadRequest("raw name must be base64", field="raw") from None
    if not isinstance(path, str):
        raise BadRequest("entry path must be a string", field="path")
    try:
        return path.encode("utf-8")
    except UnicodeEncodeError:
        raise BadRequest("entry path is not UTF-8", field="path") from None


def fp_key(rel: bytes) -> str:
    """Fingerprint key of a relative name (UTF-8 text, or ``b64:`` + base64 for other bytes)."""
    try:
        return rel.decode("utf-8")
    except UnicodeDecodeError:
        return "b64:" + base64.b64encode(rel).decode("ascii")


def safe_rel(rel: bytes, rules: PlatformRules) -> str:
    """Validate a received relative name with ``paths.check_rel``; on POSIX targets control characters and
    non-UTF-8 bytes are legal names there (``allow_control``, W095). Returns the name as a ``str``
    (surrogateescape)."""
    if b"\x00" in rel:
        raise PolicyDenied("NUL in a received name", reason="control_character")
    text = os.fsdecode(rel)
    if rules.windows:
        try:
            rel.decode("utf-8")
        except UnicodeDecodeError:
            raise PolicyDenied("name is not representable on Windows", reason="encoding") from None
    check_rel(text, rules, allow_control=True)
    return text


def _join(root: Path, rel: bytes) -> Path:
    return Path(os.fsdecode(os.path.join(os.fsencode(root), rel)))


def _parents_real(root: Path, rel: bytes) -> None:
    """Every existing parent of ``root/rel`` inside ``root`` must be a real directory (``lstat``)."""
    parts = rel.split(b"/")[:-1]
    cur = os.fsencode(root)
    for part in parts:
        cur = os.path.join(cur, part)
        try:
            st = os.lstat(cur)
        except FileNotFoundError:
            return
        if not stat.S_ISDIR(st.st_mode):
            raise PolicyDenied("a parent of a received path is not a directory", reason="symlink_component")


# ---------------------------------------------------------------------------------------------------------
# Source: scan and pack
# ---------------------------------------------------------------------------------------------------------


def _mode_of(st: os.stat_result) -> str:
    if stat.S_ISDIR(st.st_mode):
        return "040755"
    if stat.S_ISLNK(st.st_mode):
        return "120000"
    return "100755" if st.st_mode & 0o111 else "100644"


def scan(
    root: Path, matcher: IgnoreMatcher, cfg: PolicyConfig, cancelled: Callable[[], bool] = _never
) -> tuple[list[TreeEntry], list[NotTransferred]]:
    """Walk ``root`` (DESIGN §9.6): ignored entries pruned (one report per pruned directory or file), denied and
    special files reported, symlinks recorded as links (only relative targets inside the tree), at most
    ``cfg.max_files`` entries (``PolicyDenied(reason=max_files)`` beyond). Entries are sorted by raw name."""
    root = Path(root)
    base = os.fsencode(root)
    entries: list[TreeEntry] = []
    left: list[NotTransferred] = []
    stack: list[bytes] = [b""]
    policy_cache = SourceCheckCache()  # one scan, one cache (never across scans)
    while stack:
        if cancelled():
            from .errors import Cancelled

            raise Cancelled("cancelled while scanning")
        rel_dir = stack.pop()
        full_dir = os.path.join(base, rel_dir) if rel_dir else base
        try:
            with os.scandir(full_dir) as it:
                children = sorted(it, key=lambda e: e.name)
        except OSError:
            left.append(NotTransferred(_name_fields(rel_dir)[0] or ".", "unreadable"))
            continue
        subdirs: list[bytes] = []
        for child in children:
            rel = os.path.join(rel_dir, child.name) if rel_dir else child.name
            text = os.fsdecode(rel)
            shown, raw = _name_fields(rel)
            try:
                st = child.stat(follow_symlinks=False)
            except OSError:
                left.append(NotTransferred(shown, "missing"))
                continue
            is_dir = stat.S_ISDIR(st.st_mode)
            if matcher.ignored(text, is_dir):
                left.append(NotTransferred(shown, "ignored"))
                continue
            path = _join(root, rel)
            if stat.S_ISLNK(st.st_mode):
                if is_denied_name(text, case_insensitive=cfg.case_insensitive):
                    left.append(NotTransferred(shown, "secret"))
                    continue
                try:
                    target = os.readlink(path)
                    target.encode("utf-8")
                except (OSError, UnicodeEncodeError):
                    left.append(NotTransferred(shown, "symlink"))
                    continue
                entry = TreeEntry(shown, "symlink", "120000", 0, st.st_mtime_ns, None, target, raw)
                if not check_tree_entry({**entry.__dict__, "path": text}).allowed:
                    left.append(NotTransferred(shown, "symlink"))
                    continue
                entries.append(entry)
            elif is_dir or stat.S_ISREG(st.st_mode):
                verdict = check_source_file(path, cfg, explicit=False, item_root=root, cache=policy_cache)
                if not verdict.allowed:
                    left.append(NotTransferred(shown, verdict.reason or "special"))
                    continue
                if is_dir:
                    entries.append(TreeEntry(shown, "dir", "040755", 0, st.st_mtime_ns, None, None, raw))
                    subdirs.append(rel)
                else:
                    entries.append(TreeEntry(shown, "file", _mode_of(st), st.st_size, st.st_mtime_ns, None, None, raw))
            else:
                left.append(NotTransferred(shown, "special"))
            if len(entries) > cfg.max_files:
                raise PolicyDenied(
                    f"more than {cfg.max_files} entries", reason="max_files", max_files=cfg.max_files, path=str(root)
                )
        stack.extend(reversed(subdirs))
    entries.sort(key=entry_rel_bytes)
    return entries, left


def _entry_doc(e: TreeEntry) -> dict:
    doc = {"path": e.path, "type": e.type, "mode": e.mode, "size": e.size, "mtime_ns": e.mtime_ns}
    if e.type == "file":
        doc["object"] = e.object
    if e.type == "symlink":
        doc["target"] = e.target
    if e.raw is not None:
        doc["raw"] = e.raw
    return doc


def _canonical(doc: dict) -> bytes:
    return json.dumps(doc, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False).encode("utf-8")


class _ManifestReader:
    """Streams ``{"entries":[…],"kind":"ctxh.tree","v":1}`` (canonical: keys sorted) without building it whole."""

    def __init__(self, docs: Iterator[dict]):
        self._parts = self._gen(docs)
        self._buf = b""

    @staticmethod
    def _gen(docs: Iterator[dict]) -> Iterator[bytes]:
        yield b'{"entries":['
        first = True
        for d in docs:
            yield (b"" if first else b",") + _canonical(d)
            first = False
        yield b'],"kind":"' + TREE_KIND.encode() + b'","v":1}'

    def read(self, n: int) -> bytes:
        while len(self._buf) < n:
            try:
                self._buf += next(self._parts)
            except StopIteration:
                break
        out, self._buf = self._buf[:n], self._buf[n:]
        return out


def pack_tree(
    entries: Sequence[TreeEntry],
    root: Path,
    store: BlockSink,
    cancelled: Callable[[], bool] = _never,
    *,
    dropped: list[NotTransferred] | None = None,
) -> str:
    """Stream every file of ``entries`` (read with ``O_NOFOLLOW``) into ``store`` and return the object id of the
    tree manifest. A file that is gone (``missing``), cannot be opened (``unreadable``) or turned into something
    else (``special``) since the scan is left out of the manifest and appended to ``dropped`` (when given) — the
    caller reports it and does not count it."""
    root = Path(root)

    def drop(e: TreeEntry, why: str) -> None:
        if dropped is not None:
            dropped.append(NotTransferred(e.path, why))

    def docs() -> Iterator[dict]:
        for e in entries:
            if e.type != "file":
                yield _entry_doc(e)
                continue
            path = _join(root, entry_rel_bytes(e))
            try:
                fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0))
            except FileNotFoundError:
                drop(e, "missing")
                continue
            except OSError:
                drop(e, "unreadable")
                continue
            with os.fdopen(fd, "rb") as fh:
                st = os.fstat(fh.fileno())
                if not stat.S_ISREG(st.st_mode):
                    drop(e, "special")
                    continue
                m = put_stream(store, fh, media=FILE_MEDIA, cancelled=cancelled)
            yield _entry_doc(TreeEntry(e.path, "file", e.mode, m.size, st.st_mtime_ns, m.object_id, None, e.raw))

    m = put_stream(store, _ManifestReader(docs()), media=TREE_MEDIA, cancelled=cancelled)
    return m.object_id


# ---------------------------------------------------------------------------------------------------------
# Target
# ---------------------------------------------------------------------------------------------------------


def _umask() -> int:
    try:
        with open("/proc/self/status", encoding="ascii") as fh:
            for line in fh:
                if line.startswith("Umask:"):
                    return int(line.split()[1], 8)
    except (OSError, ValueError):
        pass
    current = os.umask(0o022)
    os.umask(current)
    return current


def load_manifest(store: BlockSource, manifest_id: str, *, max_files: int) -> list[dict]:
    """A received tree manifest: size-capped (64 MiB) before parsing, shape checked, ``max_files`` enforced before
    anything is written (SEC 2.5)."""
    doc = read_json_object(store, manifest_id)
    if not isinstance(doc, dict) or doc.get("kind") != TREE_KIND or doc.get("v") != 1:
        raise BadRequest("not a ctxh.tree manifest", field="kind")
    entries = doc.get("entries")
    if not isinstance(entries, list):
        raise BadRequest("tree manifest without entries", field="entries")
    if len(entries) > max_files:
        raise PayloadTooLarge(
            "tree manifest has more entries than allowed", entries=len(entries), max_files=max_files, field="max_files"
        )
    if not all(isinstance(e, dict) for e in entries):
        raise BadRequest("tree manifest entry is not an object", field="entries")
    return entries


def check_room(nbytes: int, where: Path, *, max_bytes: int, write_bytes: int | None = None, what: str) -> None:
    """Before the first byte is written (H11, R28): ``nbytes`` (the declared size of what arrives) must not exceed
    ``max_bytes`` (the target's own cap), and ``write_bytes`` (default ``nbytes``: what is written now) × 1.1 must
    fit into the free space of the file system holding ``where`` (its nearest existing ancestor). One stored block
    can stand for any number of identical files, so the stored size bounds nothing."""
    if nbytes > max_bytes:
        raise PayloadTooLarge(f"the {what} is larger than allowed", bytes=nbytes, max_bytes=max_bytes)
    need = int((nbytes if write_bytes is None else write_bytes) * _DISK_MARGIN)
    probe = Path(where)
    while not probe.exists() and probe != probe.parent:
        probe = probe.parent
    free = shutil.disk_usage(probe).free
    if need > free:
        raise InsufficientStorage(
            f"not enough free space for the {what}", need_bytes=need, free_bytes=free, path=str(probe)
        )


@dataclass
class _Checked:
    rel: bytes
    entry: dict
    key: str


def _file_bytes(checked: Sequence[_Checked]) -> int:
    # sizes are whole and non-negative (policy.check_tree_entry)
    return sum(int(c.entry["size"]) for c in checked if c.entry["type"] == "file")


def _check_entries(entries: Sequence[dict], rules: PlatformRules) -> tuple[list[_Checked], list[NotTransferred]]:
    ok: list[_Checked] = []
    skipped: list[NotTransferred] = []
    seen: dict[str, str] = {}
    for e in entries:
        shown = str(e.get("path"))[:300]
        try:
            rel = entry_rel_bytes(e)
            text = safe_rel(rel, rules)
        except (PolicyDenied, BadRequest) as exc:
            skipped.append(NotTransferred(shown, "unsafe_path:" + str(exc.details.get("reason", "invalid"))))
            continue
        verdict = check_tree_entry({**e, "path": text})
        if not verdict.allowed:
            skipped.append(NotTransferred(shown, verdict.reason or "special"))
            continue
        if e["type"] == "file" and not isinstance(e.get("object"), str):
            skipped.append(NotTransferred(shown, "special"))
            continue
        folded = fold_key(text, rules)
        if folded in seen:
            skipped.append(NotTransferred(shown, "case_collision"))
            continue
        seen[folded] = text
        ok.append(_Checked(rel, e, fp_key(rel)))
    return ok, skipped


def _perm(entry: dict) -> int:
    mode = entry.get("mode")
    value = int(mode, 8) if isinstance(mode, str) else int(mode or 0)
    return 0o755 if value & 0o111 else 0o644


def _write_file(store: BlockSource, entry: dict, path: Path, cancelled: Callable[[], bool]) -> tuple[int, str]:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0)
    fd = os.open(path, flags, 0o600)
    try:
        size, digest = write_object_to(store, entry["object"], fd, cancelled=cancelled, max_size=int(entry["size"]) + 1)
        if size != entry.get("size"):
            raise BlockCorrupt("file size does not match the manifest", path=str(entry.get("path"))[:200])
        os.fchmod(fd, _perm(entry) & ~_umask())
    except BaseException:
        os.close(fd)
        with contextlib.suppress(OSError):
            os.unlink(path)
        raise
    os.close(fd)
    mtime = int(entry.get("mtime_ns") or 0)
    if mtime > 0:
        os.utime(path, ns=(mtime, mtime))
    return size, digest


def _fp_file(size: int, mtime_ns: int, digest: str) -> list:
    return ["file", size, mtime_ns, digest]


def prepare_fresh(
    store: BlockSource,
    manifest_id: str,
    partial: Path,
    rules: PlatformRules,
    cancelled: Callable[[], bool] = _never,
    *,
    dest: Path | None = None,
    max_files: int = 200_000,
    fallback: Path | None = None,
    claimed: bool = False,
    max_bytes: int = DEFAULT_MAX_BYTES,
) -> TreeApplyState:
    """Fresh mode PREPARE: build the tree in ``partial`` (created 0700; must not exist — ``claimed``: created empty
    and recorded by the caller). Something already at the path is never used or removed:
    ``ConflictError(kind=partial_exists)`` (L3-F08). Unsafe, colliding or unknown-type entries are skipped and
    reported in ``skipped``. The declared bytes of the files are checked against ``max_bytes`` and the free space
    first (``check_room``: ``PayloadTooLarge`` | ``InsufficientStorage``, H11)."""
    entries = load_manifest(store, manifest_id, max_files=max_files)
    checked, skipped = _check_entries(entries, rules)
    partial = Path(partial)
    open_partial(partial, claimed=claimed, dest=dest)
    fp: dict[str, list] = {}
    dirs: list[_Checked] = []
    try:
        check_room(_file_bytes(checked), partial.parent, max_bytes=max_bytes, what="file tree")
        for c in sorted((c for c in checked if c.entry["type"] == "dir"), key=lambda c: c.rel.count(b"/")):
            path = _join(partial, c.rel)
            _parents_real(partial, c.rel)
            try:
                os.mkdir(path, 0o777)
            except FileExistsError:
                if not stat.S_ISDIR(os.lstat(path).st_mode):
                    skipped.append(NotTransferred(c.entry.get("path", ""), "conflict"))
                    continue
            except FileNotFoundError:
                os.makedirs(path, 0o777, exist_ok=True)
            dirs.append(c)
        for c in (c for c in checked if c.entry["type"] == "file"):
            if cancelled():
                from .errors import Cancelled

                raise Cancelled("cancelled while writing files")
            path = _join(partial, c.rel)
            _parents_real(partial, c.rel)
            path.parent.mkdir(parents=True, exist_ok=True)
            size, digest = _write_file(store, c.entry, path, cancelled)
            fp[c.key] = _fp_file(size, os.lstat(path).st_mtime_ns, digest)
        for c in (c for c in checked if c.entry["type"] == "symlink"):
            path = _join(partial, c.rel)
            _parents_real(partial, c.rel)
            path.parent.mkdir(parents=True, exist_ok=True)
            try:
                os.symlink(c.entry["target"], path)
            except FileExistsError:
                skipped.append(NotTransferred(str(c.entry.get("path", ""))[:300], "conflict"))
                continue
            mtime = int(c.entry.get("mtime_ns") or 0)
            if mtime > 0:
                with contextlib.suppress(NotImplementedError, OSError):
                    os.utime(path, ns=(mtime, mtime), follow_symlinks=False)
            fp[c.key] = ["symlink", 0, 0, c.entry["target"]]
        for c in sorted(dirs, key=lambda c: -c.rel.count(b"/")):
            mtime = int(c.entry.get("mtime_ns") or 0)
            if mtime > 0:
                os.utime(_join(partial, c.rel), ns=(mtime, mtime))
    except BaseException:
        shutil.rmtree(partial, ignore_errors=True)
        raise
    return TreeApplyState(
        dest=Path(dest) if dest is not None else partial,
        mode=DestMode.FRESH,
        partial=partial,
        staged=(),
        backups=(),
        removed=(),
        skipped=tuple(skipped),
        fingerprint={"v": FP_VERSION, "files": fp},
        fallback=Path(fallback) if fallback is not None else None,
    )


def _walk_files(root: Path) -> Iterator[tuple[bytes, os.stat_result]]:
    base = os.fsencode(root)
    stack = [b""]
    while stack:
        rel_dir = stack.pop()
        with os.scandir(os.path.join(base, rel_dir) if rel_dir else base) as it:
            for child in it:
                rel = os.path.join(rel_dir, child.name) if rel_dir else child.name
                st = child.stat(follow_symlinks=False)
                if stat.S_ISDIR(st.st_mode):
                    stack.append(rel)
                else:
                    yield rel, st


def is_own_staged(rel: bytes, tag: str) -> bool:
    """``.<name>.ctxh-<tag>`` (``_staged_name``): a file the tree update of the job with this tag stages beside its
    destination."""
    name = os.path.basename(rel)
    suffix = b".ctxh-" + tag.encode("ascii")
    return name.startswith(b".") and name.endswith(suffix) and len(name) > len(suffix) + 1


def matches_fingerprint(
    root: Path, recorded: dict[str, list], *, own_tag: str | None = None, own: list[bytes] | None = None
) -> tuple[bool, str]:
    """Does the directory still hold exactly what the plugin wrote (hash only on stat mismatch)? With ``own_tag``,
    an unrecorded regular file or symlink this job's tree update staged (``is_own_staged``) is no change: it is
    listed in ``own``."""
    seen: set[str] = set()
    for rel, st in _walk_files(root):
        key = fp_key(rel)
        rec = recorded.get(key)
        own_kind = stat.S_ISREG(st.st_mode) or stat.S_ISLNK(st.st_mode)
        if rec is None and own_tag is not None and own_kind and is_own_staged(rel, own_tag):
            if own is not None:
                own.append(rel)
            continue
        if rec is None:
            return False, f"new file {key}"
        seen.add(key)
        if rec[0] == "symlink":
            if not stat.S_ISLNK(st.st_mode) or os.fsdecode(os.readlink(_join(root, rel))) != rec[3]:
                return False, f"changed link {key}"
            continue
        if not stat.S_ISREG(st.st_mode):
            return False, f"changed type {key}"
        if st.st_size == rec[1] and st.st_mtime_ns == rec[2]:
            continue
        if st.st_size != rec[1] or sha256_file(_join(root, rel)) != rec[3]:
            return False, f"changed file {key}"
    missing = set(recorded) - seen
    if missing:
        return False, f"missing file {sorted(missing)[0]}"
    return True, ""


def _staged_name(final: Path, tag: str) -> Path:
    return final.parent / f".{final.name}.ctxh-{tag}"


def prepare_update(
    store: BlockSource,
    manifest_id: str,
    dest: Path,
    fingerprint: dict,
    rules: PlatformRules,
    staging: Path,
    cancelled: Callable[[], bool] = _never,
    *,
    tag: str = "000000",
    max_files: int = 200_000,
    max_bytes: int = DEFAULT_MAX_BYTES,
) -> TreeApplyState:
    """Update mode PREPARE (DESIGN §9.6): allowed only when ``dest`` still equals ``fingerprint`` (the plugin's
    last write there, or the out-state recorded at retire for the return trip); any foreign change ⇒
    ``ConflictError(kind=foreign)``. Changed and new files are staged beside their destination
    (``.<name>.ctxh-<tag>``); replaced and removed files get backup slots under ``staging``.

    Repeatable (L3-F08 class): files staged by an earlier PREPARE with the same ``tag`` whose process died are no
    foreign change — they are staged again or removed (``tag`` is the job's). The tree's declared bytes are checked
    against ``max_bytes`` and the files to stage against the free space before anything is written (H11)."""
    dest = Path(dest)
    recorded = dict((fingerprint or {}).get("files") or {})
    leftover: list[bytes] = []
    same, why = matches_fingerprint(dest, recorded, own_tag=tag, own=leftover)
    if not same:
        raise ConflictError("the destination changed since the plugin wrote it", kind="foreign", detail=why)
    entries = load_manifest(store, manifest_id, max_files=max_files)
    checked, skipped = _check_entries(entries, rules)
    new_fp: dict[str, list] = {}
    todo: list[_Checked] = []
    for c in (c for c in checked if c.entry["type"] in ("file", "symlink")):  # read-only: what must be staged
        final = _join(dest, c.rel)
        _parents_real(dest, c.rel)
        rec = recorded.get(c.key)
        if c.entry["type"] == "file":
            m: ObjectManifest = object_manifest(store, c.entry["object"])
            if rec and rec[0] == "file" and rec[3] == m.sha256 and _perm_of(final) == _perm(c.entry):
                new_fp[c.key] = rec
                continue
        elif rec and rec[0] == "symlink" and rec[3] == c.entry.get("target"):
            new_fp[c.key] = rec
            continue
        todo.append(c)
    check_room(_file_bytes(checked), dest, max_bytes=max_bytes, write_bytes=_file_bytes(todo), what="file tree")
    staging = Path(staging)
    staging.mkdir(parents=True, exist_ok=True, mode=0o700)
    staged: list[tuple[Path, Path]] = []
    staged_meta: list[tuple[int, int, int, str]] = []
    backups: list[tuple[Path, Path]] = []
    removed: list[Path] = []
    expect: list[tuple[Path, int, int, int]] = []
    created: list[Path] = []
    wanted = {c.key for c in checked if c.entry["type"] != "dir"}
    try:
        for c in sorted((c for c in checked if c.entry["type"] == "dir"), key=lambda c: c.rel.count(b"/")):
            path = _join(dest, c.rel)
            _parents_real(dest, c.rel)
            if not os.path.lexists(path):
                os.mkdir(path, 0o777)
                created.append(path)
            elif not stat.S_ISDIR(os.lstat(path).st_mode):
                raise ConflictError("a directory of the tree is a file at the destination", kind="foreign")
        n = 0
        for c in todo:
            final = _join(dest, c.rel)
            _parents_real(dest, c.rel)
            if not final.parent.exists():
                final.parent.mkdir(parents=True, exist_ok=True)
                created.append(final.parent)
            temp = _staged_name(final, tag)
            with contextlib.suppress(FileNotFoundError):
                os.unlink(temp)
            if c.entry["type"] == "file":
                size, digest = _write_file(store, c.entry, temp, cancelled)
                st_t = os.lstat(temp)
                new_fp[c.key] = _fp_file(size, st_t.st_mtime_ns, digest)
                staged_meta.append((st_t.st_ino, size, st_t.st_mtime_ns, digest))
            else:
                os.symlink(c.entry["target"], temp)
                new_fp[c.key] = ["symlink", 0, 0, c.entry["target"]]
                staged_meta.append((os.lstat(temp).st_ino, 0, 0, c.entry["target"]))
            staged.append((temp, final))
            if os.path.lexists(final):
                st = os.lstat(final)
                n += 1
                backups.append((final, staging / f"b{n:07d}"))
                expect.append((final, st.st_size, st.st_mtime_ns, st.st_ino))
        for key in sorted(set(recorded) - wanted):
            final = _join(dest, _key_bytes(key))
            if os.path.lexists(final):
                st = os.lstat(final)
                n += 1
                backups.append((final, staging / f"b{n:07d}"))
                expect.append((final, st.st_size, st.st_mtime_ns, st.st_ino))
                removed.append(final)
    except BaseException:
        for temp, _ in staged:
            with contextlib.suppress(OSError):
                os.unlink(temp)
        for d in reversed(created):
            with contextlib.suppress(OSError):
                d.rmdir()
        raise
    restaged = {temp for temp, _ in staged}
    for rel in leftover:  # staged by an earlier run of this job and not staged again
        if _join(dest, rel) not in restaged:
            with contextlib.suppress(FileNotFoundError):
                os.unlink(_join(dest, rel))
    return TreeApplyState(
        dest=dest,
        mode=DestMode.UPDATE,
        partial=None,
        staged=tuple(staged),
        backups=tuple(backups),
        removed=tuple(removed),
        skipped=tuple(skipped),
        fingerprint={"v": FP_VERSION, "files": new_fp},
        expect=tuple(expect),
        created_dirs=tuple(created),
        staged_meta=tuple(staged_meta),
    )


def _perm_of(path: Path) -> int:
    try:
        st = os.lstat(path)
    except OSError:
        return -1
    return 0o755 if st.st_mode & 0o111 else 0o644


def _key_bytes(key: str) -> bytes:
    if key.startswith("b64:"):
        return base64.b64decode(key[4:])
    return key.encode("utf-8")


def _move(src: Path, dst: Path) -> None:
    """``os.replace`` across filesystems too. The copy goes to ``<dst>.part`` and is renamed into place before
    ``src`` is removed, so ``dst`` existing always means a complete copy (crash resumption, contract X11)."""
    try:
        os.replace(src, dst)
    except OSError as exc:
        if exc.errno != errno.EXDEV:
            raise
        part = Path(str(dst) + ".part")
        with contextlib.suppress(FileNotFoundError):
            os.unlink(part)
        if os.path.islink(src):
            os.symlink(os.readlink(src), part)
        else:
            shutil.copy2(src, part, follow_symlinks=False)
        os.replace(part, dst)
        os.unlink(src)


_NO_HARDLINK = frozenset(
    e for e in (errno.EPERM, errno.EMLINK, errno.ENOTSUP, getattr(errno, "EOPNOTSUPP", None), errno.ENOSYS) if e
)


def _same_file(a: Path, b: Path) -> bool:
    try:
        sa, sb = os.lstat(a), os.lstat(b)
    except OSError:
        return False
    return (sa.st_dev, sa.st_ino) == (sb.st_dev, sb.st_ino)


def _appeared(final: Path) -> ConflictError:
    return ConflictError("a file appeared at the destination since prepare", kind="foreign", path=str(final)[:300])


def _swap_in(temp: Path, final: Path) -> None:
    """Put the staged ``temp`` at ``final`` only while nothing is there (H10): a hard link (fails on an existing
    name), then the staged name goes; a file system without hard links gets an exclusive placeholder that the
    staged file replaces. Something at ``final`` ⇒ ``ConflictError(kind=foreign)`` — unless it is the staged file
    itself, linked by a run of this step that died before removing the staged name (contract X11)."""
    try:
        os.link(temp, final, follow_symlinks=False)
    except FileExistsError:
        if not _same_file(temp, final):
            raise _appeared(final) from None
    except (NotImplementedError, OSError) as exc:
        if isinstance(exc, OSError) and exc.errno not in _NO_HARDLINK:
            raise
        try:
            fd = os.open(final, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        except FileExistsError:
            raise _appeared(final) from None
        os.close(fd)
        os.replace(temp, final)
        return
    os.unlink(temp)


def _ours(final: Path, meta: tuple[int, int, int, str] | None) -> bool:
    """Does ``final`` still hold the file this step swapped in (same inode; for a file also its content)? A state
    without ``staged_meta`` (older snapshot) cannot tell and answers True."""
    if meta is None:
        return True
    ino, size, mtime_ns, digest = meta
    try:
        st = os.lstat(final)
    except OSError:
        return False
    if st.st_ino != ino:
        return False
    if stat.S_ISLNK(st.st_mode):
        return True
    if not stat.S_ISREG(st.st_mode):
        return False
    return (st.st_size, st.st_mtime_ns) == (size, mtime_ns) or (st.st_size == size and sha256_file(final) == digest)


def commit(state: TreeApplyState) -> dict:
    """Forward c1 step: fresh ⇒ permissions from the umask and ``rename(partial, dest)`` (``fallback`` when the
    destination appeared meanwhile); update ⇒ re-check ``(size, mtime_ns, inode)`` of every file about to be
    replaced or removed, move it to its backup, swap the staged file in. Returns ``{"dest", "fingerprint"}``.

    Update mode is repeatable (contract X11): a backup slot that exists already holds its file (moved by an earlier
    run of this step), a staged file that is gone was swapped in already — a process that resumes after a crash
    only does what is missing. A path that was free at PREPARE is filled only while it is still free
    (``_swap_in``): something that appeared there meanwhile ⇒ ``ConflictError(kind=foreign)`` (H10). An exception
    in the middle undoes the step (``rollback``) before it propagates; an undo that fails too raises
    ``gitops.undo_failed`` (``details.needs_attention``).
    Fresh mode is one rename (``fallback`` when the destination appeared; both taken ⇒
    ``ConflictError(kind=dest_appeared)`` with the partial 0700 again, L16); resuming it is the caller's (directory
    identity)."""
    if state.mode is DestMode.FRESH:
        assert state.partial is not None  # noqa: S101 - fresh states carry their partial
        os.chmod(state.partial, 0o777 & ~_umask())
        final = state.dest
        try:
            os.rename(state.partial, state.dest)
        except OSError as exc:
            if exc.errno not in (errno.ENOTEMPTY, errno.EEXIST, errno.ENOTDIR, errno.EISDIR) or state.fallback is None:
                os.chmod(state.partial, 0o700)
                raise ConflictError("the destination appeared during commit", kind="dest_appeared") from exc
            try:
                os.rename(state.partial, state.fallback)
            except OSError as exc2:
                os.chmod(state.partial, 0o700)
                raise ConflictError(
                    "the fallback destination exists too", kind="dest_appeared", path=str(state.fallback)[:300]
                ) from exc2
            final = state.fallback
        return {"dest": str(final), "fingerprint": state.fingerprint}
    expected = {p: (size, mtime, ino) for p, size, mtime, ino in state.expect}
    pending = [(final, backup) for final, backup in state.backups if not os.path.lexists(backup)]
    for final, _ in pending:
        try:
            st = os.lstat(final)
        except FileNotFoundError:
            raise ConflictError(
                "a destination file vanished since prepare", kind="foreign", path=str(final)[:300]
            ) from None
        if (st.st_size, st.st_mtime_ns, st.st_ino) != expected.get(final):
            raise ConflictError("a destination file changed since prepare", kind="foreign", path=str(final)[:300])
    backed = dict(state.backups)
    for temp, final in state.staged:  # free at PREPARE: still free (or the staged file itself), before any change
        if final not in backed and os.path.lexists(temp) and os.path.lexists(final) and not _same_file(temp, final):
            raise _appeared(final)
    try:
        for final, backup in pending:
            _move(final, backup)
        for final in state.removed:  # a cross-device move that stopped after the copy left the original
            backup = backed.get(final)
            if backup is not None and os.path.lexists(backup) and os.path.lexists(final):
                st = os.lstat(final)
                if (st.st_size, st.st_mtime_ns, st.st_ino) == expected.get(final):
                    os.unlink(final)
        for temp, final in state.staged:
            if not os.path.lexists(temp):
                continue
            if final in backed:  # its file was moved to the backup by this step just now
                os.replace(temp, final)
            else:  # free at PREPARE: filled only while still free (H10)
                _swap_in(temp, final)
    except Exception as exc:
        try:
            _undo_update(state)
        except Exception as undo:  # the destination is half-swapped: report it, never as untouched
            raise undo_failed(exc, undo, path=state.dest) from exc
        raise
    return {"dest": str(state.dest), "fingerprint": state.fingerprint}


def rollback(state: TreeApplyState) -> None:
    """Inverse of ``commit`` (idempotent): fresh ⇒ rename back into the partial; update ⇒ remove swapped-in new
    files, restore every backup, remove directories the prepare created."""
    if state.mode is DestMode.FRESH:
        if state.partial is not None and not state.partial.exists() and state.dest.is_dir():
            os.rename(state.dest, state.partial)
            os.chmod(state.partial, 0o700)
        return
    _undo_update(state)
    discard(state)


def _undo_update(state: TreeApplyState) -> None:
    """Put an update-mode destination back as PREPARE found it, from wherever the forward step stopped:
    swapped-in new files go — only while they are the file this step put there (inode and content, ``_ours``; a
    file replaced or edited since stays, H10) — and every backup returns. The staged files stay (``discard`` drops
    them)."""
    backed = {final: backup for final, backup in state.backups}
    metas: list[tuple[int, int, int, str] | None] = list(state.staged_meta)
    if len(metas) != len(state.staged):
        metas = [None] * len(state.staged)
    for (temp, final), meta in zip(state.staged, metas, strict=True):
        if final in backed or not os.path.lexists(final):
            continue
        if os.path.lexists(temp):
            if _same_file(temp, final):  # linked in, the staged name not yet removed (X11)
                os.unlink(final)
        elif _ours(final, meta):
            os.unlink(final)
    for final, backup in state.backups:
        if os.path.lexists(backup):
            _move(backup, final)


def drop_staged(dest: Path, tag: str) -> list[Path]:
    """Remove the files a tree update with this ``tag`` staged under ``dest`` (``is_own_staged``; regular files and
    symlinks only) — the abort of a job whose PREPARE died before a snapshot named them. Returns what it removed."""
    dest = Path(dest)
    removed: list[Path] = []
    if not dest.is_dir() or dest.is_symlink():
        return removed
    for rel, st in _walk_files(dest):
        if (stat.S_ISREG(st.st_mode) or stat.S_ISLNK(st.st_mode)) and is_own_staged(rel, tag):
            with contextlib.suppress(FileNotFoundError):
                os.unlink(_join(dest, rel))
                removed.append(_join(dest, rel))
    return removed


def discard(state: TreeApplyState) -> None:
    """Drop a prepared, uncommitted item (partial or staged files and created directories)."""
    if state.mode is DestMode.FRESH:
        if state.partial is not None and state.partial.exists():
            shutil.rmtree(state.partial, ignore_errors=True)
        return
    for temp, _ in state.staged:
        with contextlib.suppress(OSError):
            os.unlink(temp)
    for d in sorted(state.created_dirs, key=lambda p: -len(p.parts)):
        with contextlib.suppress(OSError):
            d.rmdir()


def fingerprint(root: Path, entries: Sequence[TreeEntry]) -> dict:
    """Fingerprint of the files of ``entries`` as they are now under ``root`` (``{"v", "files": {key: [type, size,
    mtime_ns, sha256 | link target]}}``) — recorded at retire (out-state) and after a commit (in-state)."""
    files: dict[str, list] = {}
    for e in entries:
        rel = entry_rel_bytes(e)
        path = _join(Path(root), rel)
        try:
            st = os.lstat(path)
        except OSError:
            continue
        if stat.S_ISLNK(st.st_mode):
            files[fp_key(rel)] = ["symlink", 0, 0, os.fsdecode(os.readlink(path))]
        elif stat.S_ISREG(st.st_mode):
            files[fp_key(rel)] = _fp_file(st.st_size, st.st_mtime_ns, sha256_file(path))
    return {"v": FP_VERSION, "files": files}


def manifest_sha256(store: BlockSource, object_id: str) -> str:
    """Content hash of a stored object (for single extra files)."""
    return object_manifest(store, object_id).sha256


def file_sha256(path: Path) -> str:
    """Streaming sha256 of a regular file (no symlink following)."""
    h = hashlib.sha256()
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(fd, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def pack_file(path: Path, store: BlockSink, cancelled: Callable[[], bool] = _never) -> dict:
    """Pack one extra file (``O_NOFOLLOW``, must be regular) into ``store``; returns the pack-document item fields
    ``{"kind": "extra_file", "object", "size", "mode", "mtime_ns"}`` (the caller adds ``item_id``)."""
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0))
    with os.fdopen(fd, "rb") as fh:
        st = os.fstat(fh.fileno())
        if not stat.S_ISREG(st.st_mode):
            raise PolicyDenied("extra path is not a regular file", reason="special", path=str(path)[:300])
        m = put_stream(store, fh, media=FILE_MEDIA, cancelled=cancelled)
    return {
        "kind": "extra_file",
        "object": m.object_id,
        "size": m.size,
        "mode": _mode_of(st),
        "mtime_ns": st.st_mtime_ns,
    }


def _ps(value: Path | None) -> str | None:
    return enc_path(str(value)) if value is not None else None


def _pl(value: str | None) -> Path | None:
    return Path(dec_path(value)) if value is not None else None


def state_to_dict(s: TreeApplyState) -> dict:
    """JSON-safe snapshot of a prepared tree item (journal / crash recovery)."""
    return {
        "dest": _ps(s.dest),
        "mode": s.mode.value,
        "partial": _ps(s.partial),
        "staged": [[_ps(a), _ps(b)] for a, b in s.staged],
        "backups": [[_ps(a), _ps(b)] for a, b in s.backups],
        "removed": [_ps(p) for p in s.removed],
        "skipped": [[n.path, n.why] for n in s.skipped],
        "fingerprint": s.fingerprint,
        "expect": [[_ps(p), size, mtime, ino] for p, size, mtime, ino in s.expect],
        "created_dirs": [_ps(p) for p in s.created_dirs],
        "fallback": _ps(s.fallback),
        "staged_meta": [list(m) for m in s.staged_meta],
    }


def state_from_dict(d: dict) -> TreeApplyState:
    """Inverse of ``state_to_dict``."""
    return TreeApplyState(
        dest=_pl(d["dest"]),  # type: ignore[arg-type]
        mode=DestMode(d["mode"]),
        partial=_pl(d.get("partial")),
        staged=tuple((_pl(a), _pl(b)) for a, b in d.get("staged") or ()),  # type: ignore[misc]
        backups=tuple((_pl(a), _pl(b)) for a, b in d.get("backups") or ()),  # type: ignore[misc]
        removed=tuple(_pl(p) for p in d.get("removed") or ()),  # type: ignore[misc]
        skipped=tuple(NotTransferred(p, w) for p, w in d.get("skipped") or ()),
        fingerprint=dict(d.get("fingerprint") or {}),
        expect=tuple((_pl(p), int(a), int(b), int(c)) for p, a, b, c in d.get("expect") or ()),  # type: ignore[misc]
        created_dirs=tuple(_pl(p) for p in d.get("created_dirs") or ()),  # type: ignore[misc]
        fallback=_pl(d.get("fallback")),
        staged_meta=tuple((int(a), int(b), int(c), str(t)) for a, b, c, t in d.get("staged_meta") or ()),
    )
