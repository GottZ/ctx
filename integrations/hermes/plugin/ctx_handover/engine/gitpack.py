"""Source side of a git item: pack a repository without touching it (layer L4, stdlib only; DESIGN §9.3, §9.4).

``pack_repo`` records refs, HEAD and stash, captures the index and the work tree as two synthetic commits
(I on HEAD, W on I — technical identity and the job's creation time, so a repeat yields the same ids, R5/SK M2),
and streams ``git pack-objects --revs --thin`` straight into blocks. Every new object goes into a temporary bare
repository under the job's staging directory whose alternates point at the source's object store; the user's
``.git`` (refs, index, objects, config) and work tree stay byte-identical.

Secrets policy (I6, R23): untracked files go through ``policy.check_source_file`` (name, directory, content sniff,
size); tracked paths with a denied name and modified tracked files whose content sniffs as a key are excluded
(a staged copy is reset to HEAD in the temporary index); every stash entry is checked over its work-tree, index
and untracked commits and withheld as a whole when it carries a denied path or key that differs from its base.
Everything left behind is reported (``excluded``/``skipped``).
"""

from __future__ import annotations

import contextlib
import hashlib
import json
import os
import shutil
import stat
from collections import Counter
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path

from .contracts import BlockSink
from .errors import CapabilityMissing, ConflictError, PolicyDenied
from .gitops import (
    LFS_MAX_POINTER,
    LFS_MIN_POINTER,
    RECEIVED,
    Git,
    GitCommandError,
    GitEnvSpec,
    GitProbe,
    compute_state,
    display_path,
    encode_pathspec,
    git_dirs,
    head_info,
    in_progress_op,
    is_sha,
    lfs_pointer_blobs,
    name_report,
    object_format,
    put_bytes,
    put_stream,
    worktree_state,
)
from .jsonutil import canonical_bytes
from .model import GitState, NotTransferred
from .paths import strip_url_userinfo
from .policy import PolicyConfig, SourceCheckCache, check_source_file, is_denied_name, sniff_bytes, sniff_secret

PACK_MEDIA = "application/x-git-pack"
TREE_MEDIA = "application/json"
_SNIFF_BYTES = 8 * 1024
_CACHE_FILE = "pack-cache.json"


def _never() -> bool:
    return False


@dataclass(frozen=True)
class GitPackResult:
    """Everything the pack document needs for one git item (DESIGN §9.1). ``lfs_manifest`` (tree manifest object
    of the shipped LFS objects), ``warnings``, ``case_collisions`` and ``ntfs_invalid`` (``gitops.name_report`` over
    the work-tree tree W, JSON-safe) are additions over INTERFACES §4.3."""

    state: GitState
    index_commit: str
    worktree_commit: str
    refs: dict[str, str]
    head: dict
    stash: tuple[dict, ...]
    remotes: tuple[dict, ...]
    upstreams: dict
    excluded: tuple[NotTransferred, ...]
    skipped: tuple[NotTransferred, ...]
    lfs_files: tuple[tuple[str, Path], ...]
    object_format: str
    basis: tuple[str, ...]
    pack_object: str | None
    lfs_manifest: str | None = None
    warnings: tuple[str, ...] = ()
    reused: bool = False
    case_collisions: tuple[tuple[str, str], ...] = ()
    ntfs_invalid: tuple[str, ...] = ()


@dataclass
class _Scan:
    excluded: list[NotTransferred] = field(default_factory=list)
    skipped: list[NotTransferred] = field(default_factory=list)
    pathspecs: set[str] = field(default_factory=set)
    warnings: list[str] = field(default_factory=list)


# ---------------------------------------------------------------------------------------------------------
# Exclusions (I6, SK M9)
# ---------------------------------------------------------------------------------------------------------


def _abs(root: Path, rel: bytes) -> Path:
    return Path(os.fsdecode(os.path.join(os.fsencode(root), rel)))


def _lifted(cfg: PolicyConfig, path: Path) -> bool:
    return os.path.normpath(str(path)) in cfg.include_secret


def _scan_untracked(
    git: Git, root: Path, cfg: PolicyConfig, include: frozenset[str], out: _Scan, cancelled: Callable[[], bool]
) -> None:
    spec = GitEnvSpec(profile="source", ceiling=root.parent)
    count = 0
    per_dir: Counter[str] = Counter()
    policy_cache = SourceCheckCache()  # one scan, one cache (never across scans)
    for rec in git.iter_z(["ls-files", "-o", "--exclude-standard", "-z"], cwd=root, spec=spec, cancelled=cancelled):
        if not rec:
            continue
        if rec.endswith(b"/"):
            out.excluded.append(NotTransferred(display_path(rec.rstrip(b"/")), "nested_repo"))
            continue
        count += 1
        top = rec.split(b"/", 1)[0] if b"/" in rec else b"."
        per_dir[display_path(top)] += 1
        if count > cfg.max_untracked_files:
            continue
        path = _abs(root, rec)
        try:
            st = os.lstat(path)
        except OSError:
            out.skipped.append(NotTransferred(display_path(rec), "missing"))
            out.pathspecs.add(encode_pathspec(rec))
            continue
        if stat.S_ISLNK(st.st_mode):
            if is_denied_name(os.fsdecode(rec), case_insensitive=cfg.case_insensitive) and not _lifted(cfg, path):
                out.excluded.append(NotTransferred(display_path(rec), "secret"))
                out.pathspecs.add(encode_pathspec(rec))
            continue
        if not stat.S_ISREG(st.st_mode):
            out.skipped.append(NotTransferred(display_path(rec), "special"))
            out.pathspecs.add(encode_pathspec(rec))
            continue
        explicit = os.path.normpath(str(path)) in include
        verdict = check_source_file(path, cfg, explicit=explicit, item_root=root, cache=policy_cache)
        if verdict.allowed:
            continue
        why = verdict.reason or "special"
        target = out.skipped if why in {"size", "missing", "unreadable", "special"} else out.excluded
        target.append(NotTransferred(display_path(rec), why))
        out.pathspecs.add(encode_pathspec(rec))
    if count > cfg.max_untracked_files:
        top = [name for name, _ in per_dir.most_common(5)]
        raise PolicyDenied(
            f"{count} untracked files exceed max_untracked_files ({cfg.max_untracked_files})",
            reason="untracked_count",
            count=count,
            top_dirs=top,
        )


def _scan_tracked(git: Git, root: Path, cfg: PolicyConfig, out: _Scan, cancelled: Callable[[], bool]) -> None:
    spec = GitEnvSpec(profile="source", ceiling=root.parent)
    denied: list[bytes] = []
    for rec in git.iter_z(["ls-files", "-z", "--cached"], cwd=root, spec=spec, cancelled=cancelled):
        if (
            rec
            and is_denied_name(os.fsdecode(rec), case_insensitive=cfg.case_insensitive)
            and not _lifted(cfg, _abs(root, rec))
        ):
            denied.append(rec)
    changed: set[bytes] = set()
    for args in (["diff-files", "--name-only", "-z", "--no-ext-diff"], ["diff-index", "--cached", "--name-only", "-z"]):
        if args[0] == "diff-index":
            head, _ = head_info(git, root)
            if head is None:
                continue
            args = [*args, head]
        # must succeed: a failed (killed) listing would read as "nothing changed" and skip the content sniff
        proc = git.run(args, cwd=root, spec=spec, cancelled=cancelled)
        changed.update(r for r in proc.stdout.split(b"\x00") if r)
    for rec in sorted(changed):
        if rec in denied:
            continue
        path = _abs(root, rec)
        try:
            st = os.lstat(path)
        except OSError:
            continue
        if not stat.S_ISREG(st.st_mode) or st.st_size > cfg.max_file_bytes or _lifted(cfg, path):
            continue
        if sniff_secret(path):
            denied.append(rec)
    has_head = head_info(git, root)[0] is not None
    for rec in denied:
        in_head = has_head and (
            git.run(["cat-file", "-e", b"HEAD:" + rec], cwd=root, spec=spec, check=False).returncode == 0
        )
        out.excluded.append(NotTransferred(display_path(rec), "secret"))
        out.pathspecs.add(encode_pathspec(rec))
        if in_head:
            out.warnings.append(f"denied_tracked: {display_path(rec)} keeps its committed content")


# ---------------------------------------------------------------------------------------------------------
# Stash (R23, SEC 5.5)
# ---------------------------------------------------------------------------------------------------------


def _empty_tree(git: Git, root: Path) -> str:
    return git.out(
        ["hash-object", "-t", "tree", "--stdin"],
        cwd=root,
        spec=GitEnvSpec(profile="source", ceiling=root.parent),
        input=b"",
    )


def _blob_head(git: Git, root: Path, oid: str, n: int = _SNIFF_BYTES) -> bytes:
    proc = git.popen_stdout(
        ["cat-file", "blob", "--end-of-options", oid],
        cwd=root,
        spec=GitEnvSpec(profile="source", ceiling=root.parent),
        input_lines=[],
    )
    assert proc.stdout is not None  # noqa: S101 - PIPE requested
    data, whole = b"", False
    try:
        data = proc.stdout.read(n)
        whole = len(data) < n  # end of output before n bytes: the whole blob, if cat-file succeeded
    finally:
        if not whole:
            proc.kill()  # the head is all the sniff needs (or the read failed)
        proc.stdout.close()
        rc = proc.wait()
    if whole and rc != 0:  # a failed cat-file is no empty, harmless blob
        raise GitCommandError(f"git cat-file failed (rc {rc}) for blob {oid}", subcommand="cat-file", rc=rc)
    return data


def _blob_is_secret(git: Git, root: Path, oid: str) -> bool:
    """The policy's content sniff on the head of a blob, in memory (``policy.sniff_bytes``, W093)."""
    return sniff_bytes(_blob_head(git, root, oid))


def _changed_entries(git: Git, root: Path, base: str, tree: str) -> list[tuple[str, bytes]]:
    proc = git.run(
        ["diff-tree", "-r", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--end-of-options", base, tree],
        cwd=root,
        spec=GitEnvSpec(profile="source", ceiling=root.parent),
    )
    recs = proc.stdout.split(b"\x00")
    out: list[tuple[str, bytes]] = []
    i = 0
    while i + 1 < len(recs):
        meta, path = recs[i], recs[i + 1]
        i += 2
        parts = meta.lstrip(b":").split()
        if len(parts) >= 5 and parts[4][:1] in (b"A", b"M", b"T") and parts[1] in (b"100644", b"100755"):
            out.append((parts[3].decode("ascii"), path))
    return out


def filter_stash(
    git: Git, root: str, entries: Sequence[dict], cfg: PolicyConfig, *, scratch: Path | None = None
) -> tuple[list[dict], list[NotTransferred]]:
    """R23: keep a stash entry only when none of its work-tree, index and untracked (``stash -u``) commits adds or
    changes a path with a denied name or a key-sniffing blob relative to its base; such an entry is withheld as a
    whole and listed, unless the path was released locally with ``--include-secret``. Blobs are sniffed in memory
    (``policy.sniff_bytes``); ``scratch`` is accepted for compatibility and no longer written to."""
    r = Path(root)
    spec = GitEnvSpec(profile="source", ceiling=r.parent)
    kept: list[dict] = []
    withheld: list[NotTransferred] = []
    empty = _empty_tree(git, r)
    for n, entry in enumerate(entries):
        commit = entry.get("commit")
        if not is_sha(commit):
            continue
        parents = git.out(["rev-list", "--parents", "-n", "1", "--end-of-options", commit], cwd=r, spec=spec).split()[
            1:
        ]
        if not parents:
            continue
        base = parents[0]
        pairs = [(base + "^{tree}", commit + "^{tree}")]
        if len(parents) > 1:
            pairs.append((base + "^{tree}", parents[1] + "^{tree}"))
        if len(parents) > 2:
            pairs.append((empty, parents[2] + "^{tree}"))
        bad: str | None = None
        for left, right in pairs:
            for oid, path in _changed_entries(git, r, left, right):
                lifted = _lifted(cfg, _abs(r, path))
                if lifted:
                    continue
                if is_denied_name(os.fsdecode(path), case_insensitive=cfg.case_insensitive):
                    bad = display_path(path)
                    break
                if _blob_is_secret(git, r, oid):
                    bad = display_path(path)
                    break
            if bad:
                break
        if bad:
            withheld.append(NotTransferred(f"stash@{{{n}}} ({entry.get('message', '')}): {bad}", "stash_secret"))
        else:
            kept.append({"commit": commit, "message": str(entry.get("message", ""))})
    return kept, withheld


def _stash_entries(git: Git, root: Path) -> list[dict]:
    proc = git.run(  # rc 0 also without any stash; a failure must not read as "no stash"
        ["stash", "list", "-z", "--format=%H %gs"],
        cwd=root,
        spec=GitEnvSpec(profile="source", ceiling=root.parent),
    )
    out = []
    for rec in proc.stdout.decode("utf-8", errors="replace").split("\x00"):
        sha, _, msg = rec.partition(" ")
        if is_sha(sha):
            out.append({"commit": sha, "message": msg})
    return out


# ---------------------------------------------------------------------------------------------------------
# Refs, remotes, upstreams
# ---------------------------------------------------------------------------------------------------------


def _refs(git: Git, root: Path) -> dict[str, str]:
    proc = git.run(
        ["for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags", "refs/remotes", "refs/notes"],
        cwd=root,
        spec=GitEnvSpec(profile="source", ceiling=root.parent),
    )
    refs: dict[str, str] = {}
    for line in proc.stdout.decode("utf-8", errors="surrogateescape").split("\n"):
        sha, _, name = line.partition(" ")
        if is_sha(sha) and name and not name.endswith("/HEAD"):
            refs[name] = sha
    return refs


def _config_sections(git: Git, root: Path, pattern: str) -> list[tuple[str, str]]:
    proc = git.run(
        ["config", "--local", "-z", "--get-regexp", pattern],
        cwd=root,
        spec=GitEnvSpec(profile="source", ceiling=root.parent),
        check=False,
    )
    if proc.returncode not in (0, 1):  # 1 = no match; anything else is a failed read, not "none"
        raise GitCommandError(
            f"git config failed (rc {proc.returncode}): {proc.stderr[-300:].decode('utf-8', 'replace')}",
            subcommand="config",
            rc=proc.returncode,
        )
    out = []
    for rec in proc.stdout.split(b"\x00"):
        if rec:
            key, _, value = rec.partition(b"\n")
            out.append((key.decode("utf-8", errors="replace"), value.decode("utf-8", errors="replace")))
    return out


def _remotes(git: Git, root: Path) -> tuple[tuple[dict, ...], dict]:
    remotes: dict[str, dict] = {}
    for key, value in _config_sections(git, root, r"^remote\..*\.(url|pushurl)$"):
        name, _, prop = key[len("remote.") :].rpartition(".")
        entry = remotes.setdefault(name, {"name": name, "fetch_url": None, "push_url": None})
        if prop == "url":
            entry["fetch_url"] = strip_url_userinfo(value)
        else:
            entry["push_url"] = strip_url_userinfo(value)
    upstreams: dict[str, dict] = {}
    for key, value in _config_sections(git, root, r"^branch\..*\.(remote|merge)$"):
        branch, _, prop = key[len("branch.") :].rpartition(".")
        upstreams.setdefault(branch, {})[prop] = value
    ups = {b: v for b, v in upstreams.items() if "remote" in v and "merge" in v}
    return tuple(remotes[n] for n in sorted(remotes)), dict(sorted(ups.items()))


# ---------------------------------------------------------------------------------------------------------
# LFS (DESIGN §9.4)
# ---------------------------------------------------------------------------------------------------------


def lfs_pointers(git: Git, gitdir_args: list[str], cwd: Path | None, spec: GitEnvSpec, trees: Sequence[str]) -> dict:
    """``{oid: size}`` of every LFS pointer blob in ``trees`` (small blobs only are read)."""
    small: dict[str, None] = {}
    for tree in trees:
        for rec in git.iter_z([*gitdir_args, "ls-tree", "-r", "-l", "-z", "--full-tree", tree], cwd=cwd, spec=spec):
            meta = rec.partition(b"\t")[0].split()
            if (
                len(meta) == 4
                and meta[1] == b"blob"
                and meta[3].isdigit()
                and LFS_MIN_POINTER <= int(meta[3]) <= LFS_MAX_POINTER
            ):
                small[meta[2].decode("ascii")] = None
    found: dict[str, int] = {}
    for oid, size in lfs_pointer_blobs(git, gitdir_args, cwd, spec, small).values():
        found[oid] = size
    return found


def _ship_lfs(
    git: Git,
    repo: Path,
    common: Path,
    trees: Sequence[str],
    store: BlockSink,
    cancelled: Callable[[], bool],
) -> tuple[str | None, tuple[tuple[str, Path], ...], list[str]]:
    pointers = lfs_pointers(git, ["--git-dir", str(repo)], None, RECEIVED, trees)
    if not pointers:
        return None, (), []
    entries, files, warnings = [], [], []
    for oid, size in sorted(pointers.items()):
        path = common / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
        try:
            st = os.lstat(path)
        except OSError:
            warnings.append(
                f"lfs object {oid[:12]} not present locally; the pointer stays (git lfs pull on the target)"
            )
            continue
        if not stat.S_ISREG(st.st_mode) or st.st_size != size:
            warnings.append(f"lfs object {oid[:12]} has the wrong size; not shipped")
            continue
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd, "rb") as fh:
            m = put_stream(store, fh, media="application/octet-stream", cancelled=cancelled)
        if m.sha256 != oid:
            warnings.append(f"lfs object {oid[:12]} does not match its oid; not shipped")
            continue
        entries.append(
            {"path": oid, "type": "file", "mode": "100644", "size": size, "mtime_ns": 0, "object": m.object_id}
        )
        files.append((oid, path))
    if not entries:
        return None, (), warnings
    doc = {"kind": "ctxh.tree", "v": 1, "entries": entries}
    return put_bytes(store, canonical_bytes(doc), TREE_MEDIA).object_id, tuple(files), warnings


# ---------------------------------------------------------------------------------------------------------
# pack_repo
# ---------------------------------------------------------------------------------------------------------


def _init_temp_repo(git: Git, repo: Path, common: Path, fmt: str) -> None:
    if not (repo / "objects").is_dir():
        repo.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        git.run(["init", "-q", "--bare", "--template=", f"--object-format={fmt}", str(repo)], cwd=None, spec=RECEIVED)
    alternates = repo / "objects" / "info" / "alternates"
    alternates.parent.mkdir(parents=True, exist_ok=True)
    alternates.write_text(str(common / "objects") + "\n", encoding="utf-8")


def _commit_tree(git: Git, repo: Path, tree: str, parent: str | None, message: bytes, at: float) -> str:
    args = ["--git-dir", str(repo), "commit-tree"]
    if parent:
        args += ["-p", parent]
    args += ["--end-of-options", tree]
    spec = GitEnvSpec(profile="received", technical_identity_at=at)
    return git.out(args, cwd=None, spec=spec, input=message)


def _stream_pack(
    git: Git, repo: Path, tips: Sequence[str], basis: Sequence[str], store: BlockSink, cancelled: Callable[[], bool]
) -> str | None:
    lines = sorted(set(tips)) + [f"^{b}" for b in sorted(set(basis))]
    proc = git.popen_stdout(
        ["--git-dir", str(repo), "pack-objects", "--revs", "--thin", "--stdout", "-q", "--delta-base-offset"],
        cwd=None,
        spec=RECEIVED,
        input_lines=lines,
    )
    assert proc.stdout is not None  # noqa: S101 - PIPE requested
    try:
        header = b""
        while len(header) < 12:
            chunk = proc.stdout.read(12 - len(header))
            if not chunk:
                break
            header += chunk
        if len(header) < 12 or header[:4] != b"PACK":
            raise GitCommandError("pack-objects produced no pack", subcommand="pack-objects", rc=proc.poll())
        count = int.from_bytes(header[8:12], "big")
        if count == 0:
            proc.stdout.read()
            manifest = None
        else:
            manifest = put_stream(store, proc.stdout, media=PACK_MEDIA, cancelled=cancelled, first=header)
    except BaseException:
        proc.kill()
        raise
    finally:
        proc.stdout.close()
        rc = proc.wait()
    if rc != 0:
        err = b""
        if proc.stderr is not None:
            with contextlib.suppress(OSError, ValueError):
                proc.stderr.seek(0)
                err = proc.stderr.read()
        raise GitCommandError(f"pack-objects failed: {err[-300:]!r}", subcommand="pack-objects", rc=rc)
    return manifest.object_id if manifest else None


def _cache_key(state: GitState, refs: dict, stash: list[dict], basis: Sequence[str], lfs: bool) -> str:
    doc = {
        "head": state.head,
        "index_tree": state.index_tree,
        "worktree_tree": state.worktree_tree,
        "excl": sorted(state.exclude_pathspecs),
        "refs": refs,
        "stash": [s["commit"] for s in stash],
        "basis": sorted(basis),
        "lfs": lfs,
    }
    return hashlib.sha256(canonical_bytes(doc)).hexdigest()


def _load_cache(workdir: Path, key: str, store: BlockSink) -> dict | None:
    try:
        data = json.loads((workdir / _CACHE_FILE).read_text("utf-8"))
    except (OSError, ValueError):
        return None
    if not isinstance(data, dict) or data.get("key") != key:
        return None
    if not all(k in data for k in _CACHE_KEYS):
        return None
    # a hit needs the blocks of every object it names, known and present (an unknown list is not "nothing")
    needed: list[str] = []
    for obj, listed in (("pack_object", "blocks"), ("lfs_manifest", "lfs_blocks")):
        if data.get(obj) is None:
            continue
        blocks = data.get(listed)
        if not isinstance(blocks, list) or not blocks:
            return None
        needed += blocks
    if needed and store.missing(needed):
        return None
    return data


_CACHE_KEYS = ("index_commit", "worktree_commit", "pack_object", "lfs_manifest", "lfs_files", "lfs_warnings")


def _save_cache(workdir: Path, key: str, data: dict) -> None:
    tmp = workdir / (_CACHE_FILE + ".tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        json.dump({"key": key, **data}, fh, sort_keys=True)
    os.replace(tmp, workdir / _CACHE_FILE)


def _pack_blocks(store: BlockSink, object_id: str | None) -> list[str]:
    if object_id is None:
        return []
    manifests = getattr(store, "manifests", None)
    if manifests is None:
        return []
    found = manifests([object_id])
    return list(found[0].blocks) if found else []


def pack_repo(
    git: Git,
    root: str,
    workdir: Path,
    *,
    basis_candidates: Sequence[str],
    cfg: PolicyConfig,
    store: BlockSink,
    created_at: float,
    force: bool,
    cancelled: Callable[[], bool] = _never,
    include: frozenset[str] = frozenset(),
) -> GitPackResult:
    """Pack the repository whose work-tree root is ``root`` (DESIGN §9.3). ``workdir`` is
    ``staging/<job>/git/<item>`` (created 0700); ``basis_candidates`` are the target's ``git_have`` commits;
    ``include`` holds absolute paths released from the size limit (``--include``). Never writes into ``root``.

    Raises ``CapabilityMissing`` (git below 2.38, shallow or partial clone — such repositories travel as file
    trees), ``ConflictError(kind=in_progress)`` for an in-progress operation or unmerged paths without ``force``,
    ``PolicyDenied(reason=untracked_count)`` above ``max_untracked_files``."""
    git.ensure_supported()
    r = Path(root)
    probe = GitProbe(git)
    top = probe.toplevel(str(r))
    if top is None or os.path.realpath(top) != os.path.realpath(r):
        raise PolicyDenied("not the root of a git work tree", reason="special", path=str(r))
    if probe.is_shallow(str(r)) or probe.is_partial(str(r)):
        raise CapabilityMissing("shallow or partial clones travel as file trees", capability="git.full_history")
    gitdir, common = git_dirs(git, r)
    op = in_progress_op(gitdir)
    if op and not force:
        raise ConflictError(f"a {op} is in progress", kind="in_progress", operation=op)
    fmt = object_format(git, r)
    workdir = Path(workdir)
    workdir.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(workdir, 0o700)
    repo = workdir / "repo"
    _init_temp_repo(git, repo, common, fmt)

    scan = _Scan()
    _scan_untracked(git, r, cfg, include, scan, cancelled)
    _scan_tracked(git, r, cfg, scan, cancelled)
    excl = tuple(sorted(scan.pathspecs))
    sw = compute_state(git, r, excl, repo / "objects", workdir / "state", force=force, cancelled=cancelled)
    state = GitState(
        head=sw.head or "",
        index_tree=sw.index_tree,
        worktree_tree=sw.worktree_tree,
        branch=sw.branch,
        exclude_pathspecs=excl,
    )
    refs = _refs(git, r)
    stash_all = _stash_entries(git, r)
    stash, withheld = filter_stash(git, str(r), stash_all, cfg, scratch=workdir / "sniff")
    remotes, upstreams = _remotes(git, r)
    basis = tuple(sorted(b for b in set(basis_candidates) if probe.has_commit(str(r), b)))
    lfs_wanted = (common / "lfs").is_dir()
    key = _cache_key(state, refs, stash, basis, lfs_wanted)
    excluded = tuple(scan.excluded) + tuple(withheld)
    skipped = tuple(scan.skipped)
    warnings = list(scan.warnings)
    if sw.unmerged:
        warnings.append("unmerged paths were packed with their work-tree content; the merge state stays behind")
    if op:
        warnings.append(f"a {op} was in progress; its state stays behind")

    cached = _load_cache(workdir, key, store)
    if cached is not None:
        index_commit, worktree_commit = cached["index_commit"], cached["worktree_commit"]
        pack_object, lfs_manifest = cached.get("pack_object"), cached.get("lfs_manifest")
        lfs_files = tuple((o, Path(p)) for o, p in cached.get("lfs_files", []))
        warnings += cached.get("lfs_warnings", [])
        reused = True
    else:
        index_commit = _commit_tree(git, repo, state.index_tree, sw.head, b"ctx_handover index state\n", created_at)
        worktree_commit = _commit_tree(
            git, repo, state.worktree_tree, index_commit, b"ctx_handover work-tree state\n", created_at
        )
        lfs_manifest, lfs_files, lfs_warn = (None, (), [])
        if lfs_wanted:
            lfs_manifest, lfs_files, lfs_warn = _ship_lfs(
                git, repo, common, [x for x in (sw.head, worktree_commit) if x], store, cancelled
            )
        warnings += lfs_warn
        tips = [*refs.values(), *(s["commit"] for s in stash), index_commit, worktree_commit]
        if sw.head:
            tips.append(sw.head)
        pack_object = _stream_pack(git, repo, tips, basis, store, cancelled)
        _save_cache(
            workdir,
            key,
            {
                "index_commit": index_commit,
                "worktree_commit": worktree_commit,
                "pack_object": pack_object,
                "lfs_manifest": lfs_manifest,
                "lfs_files": [[o, str(p)] for o, p in lfs_files],
                "lfs_warnings": lfs_warn,
                "blocks": _pack_blocks(store, pack_object),
                "lfs_blocks": _pack_blocks(store, lfs_manifest),
            },
        )
        reused = False
    head_doc = {"ref": sw.branch, "commit": sw.head}
    case_pairs, ntfs_invalid, name_warnings = name_report(
        GitProbe(git, profile="received").ls_tree_paths(str(repo), state.worktree_tree)
    )
    warnings += name_warnings
    return GitPackResult(
        state=state,
        index_commit=index_commit,
        worktree_commit=worktree_commit,
        refs=refs,
        head=head_doc,
        stash=tuple(stash),
        remotes=remotes,
        upstreams=upstreams,
        excluded=excluded,
        skipped=skipped,
        lfs_files=lfs_files,
        object_format=fmt,
        basis=basis,
        pack_object=pack_object,
        lfs_manifest=lfs_manifest,
        warnings=tuple(warnings),
        reused=reused,
        case_collisions=tuple((a, b) for a, b in case_pairs),
        ntfs_invalid=tuple(ntfs_invalid),
    )


def pack_item(item_id: str, result: GitPackResult) -> dict:
    """The ``git_repo`` item of the pack document (DESIGN §9.1), incl. ``lfs_objects``, ``case_collisions`` and
    ``ntfs_invalid``. Orchestration packs through ``materialize.pack_plan_item``, which calls this."""
    return {
        "item_id": item_id,
        "kind": "git_repo",
        "pack": result.pack_object,
        "object_format": result.object_format,
        "basis": list(result.basis),
        "head": dict(result.head),
        "index_commit": result.index_commit,
        "worktree_commit": result.worktree_commit,
        "index_tree": result.state.index_tree,
        "worktree_tree": result.state.worktree_tree,
        "refs": dict(sorted(result.refs.items())),
        "stash": [dict(s) for s in result.stash],
        "remotes": [dict(x) for x in result.remotes],
        "upstreams": {k: dict(v) for k, v in result.upstreams.items()},
        "excluded": [{"path": n.path, "why": n.why} for n in result.excluded],
        "skipped": [{"path": n.path, "why": n.why} for n in result.skipped],
        "exclude_pathspecs": list(result.state.exclude_pathspecs),
        "lfs_objects": result.lfs_manifest,
        "case_collisions": [list(pair) for pair in result.case_collisions],
        "ntfs_invalid": list(result.ntfs_invalid),
    }


def recheck(git: Git, root: str, recorded: GitState, tmp: Path) -> tuple[bool, GitState]:
    """Prepare re-check (DESIGN §9.3 step 8): recompute ``(HEAD, index tree, work-tree tree)`` with the recorded
    exclusions; ``(equal, current)``. A difference means ``stale_worktree``."""
    current = worktree_state(git, root, recorded.exclude_pathspecs, tmp)
    same = (current.head, current.index_tree, current.worktree_tree) == (
        recorded.head,
        recorded.index_tree,
        recorded.worktree_tree,
    )
    return same, current


def discard_workdir(workdir: Path) -> None:
    """Remove a pack work directory (temp repository, cache)."""
    shutil.rmtree(workdir, ignore_errors=True)


__all__ = [
    "GitPackResult",
    "discard_workdir",
    "filter_stash",
    "lfs_pointers",
    "pack_item",
    "pack_repo",
    "recheck",
]
