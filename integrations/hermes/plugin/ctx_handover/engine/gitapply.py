"""Target side of a git item: quarantine, verify, materialise (layer L4, stdlib only; DESIGN §9.5, R4, R21).

Received git material is hostile until verified:

1. **Quarantine** — a fresh bare repository per attempt (never resumed into); the pack object is streamed block
   by block into ``index-pack --stdin --fix-thin --strict=<ids>=error`` (no bundle file on disk).
2. **Integrity** — ``fsck --strict --no-dangling`` with ``FSCK_ERROR_IDS`` raised to ``error``; failure deletes
   the quarantine (``VerifyFailed``).
3. **Refs only from the pack document** — names through the namespace allowlist and ``check-ref-format``, values
   must exist (heads/remotes/notes as commits), I/W must be the commits of the announced trees on the announced
   parents.
4. **Transfer** — the verified, self-contained pack and index are copied into the destination's object store
   (idx last); no fetch protocol, no hooks.

Checkouts run in the ``received`` profile: user global/system config nulled, every filter driver of the
destination's local config and of the received tree's ``.gitattributes`` emptied, hooks and fsmonitor off.

Modes: **fresh** (partial directory 0700, renamed onto the destination at commit; a destination that appeared
meanwhile ⇒ ``fallback``), **update** (clean repository, same identity, fast-forward; preconditions re-checked
at commit), **return** (the destination is provably what this host handed away — recomputed state equals the
recorded out-state, dirty work included; R4). Every forward step has an inverse (``rollback``).
"""

from __future__ import annotations

import contextlib
import errno
import hashlib
import os
import re
import secrets
import shutil
import stat
import sys
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path

from .contracts import BlockSource
from .errors import (
    BadRequest,
    ConflictError,
    InsufficientStorage,
    PayloadTooLarge,
    PolicyDenied,
    VerifyFailed,
)
from .gitops import (
    FSCK_ERROR_IDS,
    RECEIVED,
    Git,
    GitCommandError,
    GitEnvSpec,
    GitProbe,
    attribute_filter_names,
    compute_state,
    dec_path,
    enc_path,
    git_dirs,
    head_info,
    in_progress_op,
    is_sha,
    iter_object,
    open_partial,
    read_json_object,
    undo_failed,
    validate_ref,
    validate_remote,
    worktree_state,
)
from .model import DestMode, GitState
from .paths import PlatformRules, check_rel, collisions
from .policy import PolicyConfig

_MSG_MAX = 1000
_DISK_MARGIN = 1.1


def _never() -> bool:
    return False


@dataclass(frozen=True)
class GitApplyState:
    """What a prepared git item needs for commit, rollback and discard (INTERFACES §4.3 plus additions with
    defaults: quarantine dir, fallback, planned ref moves, stash depth, warnings, collisions)."""

    dest: Path
    mode: DestMode
    partial: Path | None
    h0: str | None
    old_refs: dict[str, str]
    added_paths: tuple[str, ...]
    out_state: GitState | None
    qdir: Path | None = None
    fallback: Path | None = None
    head_ref_before: str | None = None
    planned_refs: dict[str, str] = field(default_factory=dict)
    skipped_refs: tuple[str, ...] = ()
    stash_before: int = 0
    warnings: tuple[str, ...] = ()
    case_collisions: tuple[tuple[str, str], ...] = ()
    tmp: Path | None = None


# ---------------------------------------------------------------------------------------------------------
# Pack document item (untrusted peer data)
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class _Item:
    fmt: str
    pack: str | None
    head_ref: str | None
    head_commit: str | None
    index_commit: str
    worktree_commit: str
    index_tree: str
    worktree_tree: str
    refs: dict[str, str]
    stash: tuple[tuple[str, str], ...]
    remotes: tuple[dict, ...]
    upstreams: dict
    lfs: str | None


def _sha(d: dict, key: str, *, optional: bool = False) -> str | None:
    value = d.get(key)
    if value is None and optional:
        return None
    if not is_sha(value):
        raise BadRequest(f"pack item field {key} is not an object id", field=key)
    return value


def parse_item(item_pack: dict) -> _Item:
    """Validate the shape of a ``git_repo`` pack-document item (DESIGN §9.1); ``BadRequest`` on any deviation."""
    if not isinstance(item_pack, dict) or item_pack.get("kind") != "git_repo":
        raise BadRequest("not a git_repo pack item", field="kind")
    fmt = item_pack.get("object_format", "sha1")
    if fmt not in {"sha1", "sha256"}:
        raise BadRequest("unknown object format", field="object_format")
    head = item_pack.get("head") or {}
    if not isinstance(head, dict):
        raise BadRequest("head must be an object", field="head")
    head_ref = head.get("ref")
    if head_ref is not None and (not isinstance(head_ref, str) or not head_ref.startswith("refs/heads/")):
        raise PolicyDenied("HEAD must name a branch", reason="special", field="head.ref")
    refs = item_pack.get("refs") or {}
    if not isinstance(refs, dict) or not all(isinstance(k, str) and is_sha(v) for k, v in refs.items()):
        raise BadRequest("refs must map names to object ids", field="refs")
    stash = []
    for entry in item_pack.get("stash") or []:
        if not isinstance(entry, dict) or not is_sha(entry.get("commit")):
            raise BadRequest("invalid stash entry", field="stash")
        msg = str(entry.get("message", ""))[:_MSG_MAX]
        msg = "".join(ch if ch >= " " and ch != "\x7f" else " " for ch in msg)
        stash.append((entry["commit"], msg))
    remotes = item_pack.get("remotes") or []
    upstreams = item_pack.get("upstreams") or {}
    if not isinstance(remotes, list) or not isinstance(upstreams, dict):
        raise BadRequest("invalid remotes", field="remotes")
    lfs = item_pack.get("lfs_objects")
    if lfs is not None and not is_sha(lfs):
        raise BadRequest("invalid lfs_objects", field="lfs_objects")
    pack = item_pack.get("pack")
    if pack is not None and not is_sha(pack):
        raise BadRequest("invalid pack object id", field="pack")
    hexlen = 40 if fmt == "sha1" else 64
    item = _Item(
        fmt=fmt,
        pack=pack,
        head_ref=head_ref,
        head_commit=_sha(head, "commit", optional=True),
        index_commit=_sha(item_pack, "index_commit"),  # type: ignore[arg-type]
        worktree_commit=_sha(item_pack, "worktree_commit"),  # type: ignore[arg-type]
        index_tree=_sha(item_pack, "index_tree"),  # type: ignore[arg-type]
        worktree_tree=_sha(item_pack, "worktree_tree"),  # type: ignore[arg-type]
        refs=dict(refs),
        stash=tuple(stash),
        remotes=tuple(r for r in remotes if isinstance(r, dict)),
        upstreams={k: v for k, v in upstreams.items() if isinstance(k, str) and isinstance(v, dict)},
        lfs=lfs,
    )
    for sha in (item.index_commit, item.worktree_commit, item.index_tree, item.worktree_tree, *item.refs.values()):
        if len(sha) != hexlen:
            raise BadRequest("object id length does not match the object format", field="object_format")
    return item


# ---------------------------------------------------------------------------------------------------------
# Small git helpers
# ---------------------------------------------------------------------------------------------------------


def _bare(q: Path) -> list[str]:
    return ["--git-dir", str(q)]


def _wt_spec(repo: Path, *, filters: Sequence[str] = (), identity: bool = False) -> GitEnvSpec:
    return GitEnvSpec(
        profile="received",
        ceiling=Path(repo).parent,
        filters_from=Path(repo),
        filter_names=tuple(filters),
        technical_identity_at=time.time() if identity else None,
    )


def _exists(git: Git, gitdir_args: list[str], cwd: Path | None, spec: GitEnvSpec, rev: str) -> bool:
    proc = git.run([*gitdir_args, "cat-file", "-e", "--end-of-options", rev], cwd=cwd, spec=spec, check=False)
    return proc.returncode == 0


def _rev(git: Git, gitdir_args: list[str], cwd: Path | None, spec: GitEnvSpec, rev: str) -> str | None:
    proc = git.run([*gitdir_args, "rev-parse", "--verify", "-q", rev], cwd=cwd, spec=spec, check=False)
    out = proc.stdout.decode("ascii", errors="replace").strip()
    return out if proc.returncode == 0 and is_sha(out) else None


def _current_refs(git: Git, repo: Path) -> dict[str, str]:
    # must succeed: an empty answer would read as "no refs" (a rollback would keep the refs it should delete)
    proc = git.run(["for-each-ref", "--format=%(objectname) %(refname)"], cwd=repo, spec=_wt_spec(repo))
    out = {}
    for line in proc.stdout.decode("utf-8", errors="surrogateescape").split("\n"):
        sha, _, name = line.partition(" ")
        if is_sha(sha) and name:
            out[name] = sha
    return out


def _update_refs(git: Git, repo: Path, cmds: Sequence[tuple[str, ...]]) -> None:
    if not cmds:
        return
    chunks = []
    for verb, ref, *values in cmds:
        arity = {"create": 1, "update": 2, "delete": 1}[verb]
        values = (list(values) + [""] * arity)[:arity]
        chunks.append(f"{verb} {ref}".encode() + b"\x00" + b"".join(v.encode() + b"\x00" for v in values))
    payload = b"".join(chunks)
    git.run(["update-ref", "--stdin", "-z"], cwd=repo, spec=_wt_spec(repo, identity=True), input=payload)


def _stash_depth(git: Git, repo: Path) -> int:
    proc = git.run(["stash", "list", "-z", "--format=%H"], cwd=repo, spec=_wt_spec(repo))  # rc 0 without a stash
    return len([x for x in proc.stdout.split(b"\x00") if x])


def _stash_commits(git: Git, repo: Path) -> set[str]:
    proc = git.run(["stash", "list", "-z", "--format=%H"], cwd=repo, spec=_wt_spec(repo))
    return {x.decode("ascii") for x in proc.stdout.split(b"\x00") if x}


def _store_stash(git: Git, repo: Path, stash: Sequence[tuple[str, str]], *, skip: set[str] = frozenset()) -> None:
    for commit, message in reversed(stash):
        if commit in skip:
            continue
        git.run(
            ["stash", "store", "-q", "-m", message or "stash", commit],
            cwd=repo,
            spec=_wt_spec(repo, identity=True),
        )


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


def _fsync_dir(path: Path) -> None:
    with contextlib.suppress(OSError):
        fd = os.open(path, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


# ---------------------------------------------------------------------------------------------------------
# Quarantine (§9.5 steps 1–3)
# ---------------------------------------------------------------------------------------------------------


def stream_pack(
    git: Git, store: BlockSource, pack_id: str, qdir: Path, *, cancelled: Callable[[], bool] = _never
) -> None:
    """Feed the pack object block by block into ``index-pack --stdin --fix-thin --strict`` inside ``qdir``."""
    strict = "--strict=" + ",".join(f"{i}=error" for i in FSCK_ERROR_IDS)
    proc = git.popen_stdin(
        ["--git-dir", str(qdir), "index-pack", "--stdin", "--fix-thin", strict], cwd=None, spec=RECEIVED
    )
    assert proc.stdin is not None  # noqa: S101 - PIPE requested
    broken = False
    try:
        for chunk in iter_object(store, pack_id, cancelled=cancelled):
            try:
                proc.stdin.write(chunk)
            except BrokenPipeError:
                broken = True
                break
    except BaseException:
        proc.kill()
        raise
    finally:
        with contextlib.suppress(BrokenPipeError, OSError):
            proc.stdin.close()
        rc = proc.wait()
    if rc != 0 or broken:
        err = b""
        if proc.stderr is not None:
            with contextlib.suppress(OSError, ValueError):
                proc.stderr.seek(0)
                err = proc.stderr.read()[-1024:]
        raise VerifyFailed(
            "received pack rejected by index-pack", stage="index-pack", stderr=err.decode(errors="replace")
        )


def _fsck(git: Git, qdir: Path) -> None:
    alternates = qdir / "objects" / "info" / "alternates"
    saved = alternates.read_bytes() if alternates.exists() else None
    if saved is not None:
        alternates.unlink()
    try:
        extra = [arg for i in FSCK_ERROR_IDS for arg in ("-c", f"fsck.{i}=error")]
        if saved is not None:
            # A thin update pack may reference .gitmodules/.gitattributes blobs that live only in the destination;
            # index-pack --strict already checked them through the alternate, fsck here sees the pack alone.
            extra += ["-c", "fsck.gitmodulesMissing=ignore", "-c", "fsck.gitattributesMissing=ignore"]
        proc = git.run(
            ["--git-dir", str(qdir), *extra, "fsck", "--strict", "--no-dangling", "--no-reflogs", "--no-progress"],
            cwd=None,
            spec=RECEIVED,
            check=False,
        )
    finally:
        if saved is not None:
            alternates.write_bytes(saved)
    if proc.returncode != 0:
        raise VerifyFailed(
            "received objects failed the integrity check",
            stage="fsck",
            stderr=proc.stderr[-1024:].decode(errors="replace"),
        )


def _verify_refs(git: Git, qdir: Path, item: _Item) -> None:
    gd = _bare(qdir)
    for name, sha in item.refs.items():
        validate_ref(git, name)
        rev = sha if name.startswith("refs/tags/") else sha + "^{commit}"
        if not _exists(git, gd, None, RECEIVED, rev):
            raise VerifyFailed("a ref names a missing object", stage="refs", ref=name)
    if item.head_ref is not None:
        validate_ref(git, item.head_ref)
    for sha in (item.index_commit, item.worktree_commit, *(c for c, _ in item.stash)):
        if not _exists(git, gd, None, RECEIVED, sha + "^{commit}"):
            raise VerifyFailed("a state commit is missing", stage="refs", commit=sha)
    if item.head_commit is not None and not _exists(git, gd, None, RECEIVED, item.head_commit + "^{commit}"):
        raise VerifyFailed("HEAD commit is missing", stage="refs")
    if item.head_ref is not None and item.head_ref in item.refs and item.refs[item.head_ref] != item.head_commit:
        raise VerifyFailed("HEAD does not match its branch", stage="refs")
    if _rev(git, gd, None, RECEIVED, item.index_commit + "^{tree}") != item.index_tree:
        raise VerifyFailed("index commit does not carry the announced tree", stage="refs")
    if _rev(git, gd, None, RECEIVED, item.worktree_commit + "^{tree}") != item.worktree_tree:
        raise VerifyFailed("work-tree commit does not carry the announced tree", stage="refs")
    i_parent = _rev(git, gd, None, RECEIVED, item.index_commit + "^1")
    if i_parent != item.head_commit:
        raise VerifyFailed("index commit is not based on HEAD", stage="refs")
    if _rev(git, gd, None, RECEIVED, item.worktree_commit + "^1") != item.index_commit:
        raise VerifyFailed("work-tree commit is not based on the index commit", stage="refs")


def quarantine_and_verify(
    git: Git,
    store: BlockSource,
    item_pack: dict,
    qdir: Path,
    *,
    alternates: Path | None,
    cancelled: Callable[[], bool] = _never,
) -> Path:
    """Steps 1–3 of DESIGN §9.5. ``qdir`` is always recreated (a failed or resumed quarantine is never reused);
    ``alternates`` is the destination's object directory (update/return mode). Any failure deletes ``qdir`` and
    raises ``VerifyFailed`` (or ``PolicyDenied`` for a forbidden ref name)."""
    item = parse_item(item_pack)
    qdir = Path(qdir)
    shutil.rmtree(qdir, ignore_errors=True)
    qdir.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        git.run(
            ["init", "-q", "--bare", "--template=", f"--object-format={item.fmt}", str(qdir)], cwd=None, spec=RECEIVED
        )
        os.chmod(qdir, 0o700)
        if alternates is not None:
            (qdir / "objects" / "info").mkdir(parents=True, exist_ok=True)
            (qdir / "objects" / "info" / "alternates").write_text(str(alternates) + "\n", encoding="utf-8")
        if item.pack is not None:
            stream_pack(git, store, item.pack, qdir, cancelled=cancelled)
        _fsck(git, qdir)
        _verify_refs(git, qdir, item)
    except BaseException:
        shutil.rmtree(qdir, ignore_errors=True)
        raise
    return qdir


def transfer_pack(qdir: Path, dest_gitdir: Path) -> None:
    """Copy the verified pack(s) of ``qdir`` into ``dest_gitdir/objects/pack`` (temp name, fsync, rename; the
    ``.idx`` last, so git never sees an index without its pack)."""
    src = Path(qdir) / "objects" / "pack"
    dst = Path(dest_gitdir) / "objects" / "pack"
    if not src.is_dir():
        return
    dst.mkdir(parents=True, exist_ok=True)
    names = sorted(p.name for p in src.iterdir() if p.name.startswith("pack-"))
    order = [n for n in names if n.endswith(".pack")] + [n for n in names if n.endswith(".rev")]
    order += [n for n in names if n.endswith(".idx")]
    for name in order:
        final = dst / name
        if final.exists():
            continue
        # unique per attempt: a restarted process may carry the pid of the one that died (containers) and must not
        # trip over its temp file, nor take over one a live writer still fills
        tmp = dst / f".tmp-ctxh-{os.getpid()}-{secrets.token_hex(4)}-{name}"
        with open(src / name, "rb") as fin:
            fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o444)
            try:
                with os.fdopen(fd, "wb") as fout:
                    shutil.copyfileobj(fin, fout, 1024 * 1024)
                    fout.flush()
                    os.fsync(fout.fileno())
                os.rename(tmp, final)
            except BaseException:
                with contextlib.suppress(OSError):
                    os.unlink(tmp)
                raise
    _fsync_dir(dst)


# ---------------------------------------------------------------------------------------------------------
# Remote config and LFS (§9.5 steps 5–6)
# ---------------------------------------------------------------------------------------------------------


def write_remote_config(git: Git, repo: Path, remotes: Sequence[dict], upstreams: dict) -> list[str]:
    """Write the remotes and upstreams of the pack document into ``repo``'s config — only the five keys
    ``remote.<n>.url``/``pushurl``/``fetch`` (fixed default refspec) and ``branch.<b>.remote``/``merge``, each with
    ``git config --file <cfg> --end-of-options``; existing remotes are left alone. Invalid entries are skipped
    and returned as warnings (DESIGN §9.5 step 5)."""
    _, common = git_dirs(git, Path(repo))
    cfg = common / "config"
    spec = RECEIVED
    warnings: list[str] = []
    written: set[str] = set()

    def has(key: str) -> bool:
        return git.run(["config", "--file", str(cfg), "--get", key], cwd=None, spec=spec, check=False).returncode == 0

    def put(key: str, value: str) -> None:
        git.run(["config", "--file", str(cfg), "--end-of-options", key, value], cwd=None, spec=spec)

    for remote in remotes:
        try:
            name, url = validate_remote(remote.get("name"), remote.get("fetch_url"))  # type: ignore[arg-type]
        except PolicyDenied as exc:
            warnings.append(f"remote {str(remote.get('name'))[:64]!r} not configured: {exc.message}")
            continue
        if has(f"remote.{name}.url"):
            written.add(name)
            continue
        push = remote.get("push_url")
        push_url = None
        if push is not None:
            try:
                push_url = validate_remote(name, push)[1]
            except PolicyDenied as exc:
                warnings.append(f"push url of remote {name} not configured: {exc.message}")
        put(f"remote.{name}.url", url)
        if push_url:
            put(f"remote.{name}.pushurl", push_url)
        put(f"remote.{name}.fetch", f"+refs/heads/*:refs/remotes/{name}/*")
        written.add(name)
    for branch, up in upstreams.items():
        remote, merge = up.get("remote"), up.get("merge")
        try:
            validate_ref(git, "refs/heads/" + branch)
            validate_ref(git, merge)  # type: ignore[arg-type]
        except PolicyDenied:
            warnings.append(f"upstream of branch {branch[:64]!r} not configured")
            continue
        if remote not in written or not str(merge).startswith("refs/heads/"):
            warnings.append(f"upstream of branch {branch[:64]!r} not configured")
            continue
        if has(f"branch.{branch}.remote"):
            continue
        put(f"branch.{branch}.remote", remote)  # type: ignore[arg-type]
        put(f"branch.{branch}.merge", merge)  # type: ignore[arg-type]
    return warnings


_OID_RE = re.compile(r"[0-9a-f]{64}")


def place_lfs(git: Git, repo: Path, store, lfs_manifest: str | None, *, checkout: bool = True) -> list[str]:
    """Place shipped LFS objects into ``<common>/lfs/objects`` after checking ``sha256(content) == oid`` and the
    size (SEC 3.5); a mismatching object is refused and reported. Nothing contacts a network; ``git lfs checkout``
    (local) runs only when ``git-lfs`` exists, otherwise the pointers stay. Returns warnings."""
    if lfs_manifest is None:
        return []
    doc = read_json_object(store, lfs_manifest)
    if not isinstance(doc, dict) or doc.get("kind") != "ctxh.tree" or not isinstance(doc.get("entries"), list):
        raise BadRequest("invalid LFS manifest", field="lfs_objects")
    _, common = git_dirs(git, Path(repo))
    base = common / "lfs" / "objects"
    tmpdir = common / "lfs" / "tmp"
    tmpdir.mkdir(parents=True, exist_ok=True, mode=0o700)
    warnings: list[str] = []
    placed = 0
    for entry in doc["entries"]:
        oid = entry.get("path") if isinstance(entry, dict) else None
        size = entry.get("size") if isinstance(entry, dict) else None
        obj = entry.get("object") if isinstance(entry, dict) else None
        if not isinstance(oid, str) or not _OID_RE.fullmatch(oid) or not isinstance(size, int) or not is_sha(obj):
            warnings.append("invalid LFS entry refused")
            continue
        final = base / oid[:2] / oid[2:4] / oid
        if final.exists():
            continue
        tmp = tmpdir / f"ctxh-{oid}-{os.getpid()}"
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
        h = hashlib.sha256()
        n = 0
        try:
            with os.fdopen(fd, "wb") as fh:
                for raw in iter_object(store, obj, max_size=size + 1):
                    fh.write(raw)
                    h.update(raw)
                    n += len(raw)
        except BaseException:
            with contextlib.suppress(OSError):
                tmp.unlink()
            raise
        if h.hexdigest() != oid or n != size:
            tmp.unlink()
            warnings.append(f"LFS object {oid[:12]} refused: content does not match its oid or size")
            continue
        final.parent.mkdir(parents=True, exist_ok=True)
        os.chmod(tmp, 0o644 & ~_umask())
        os.rename(tmp, final)
        placed += 1
    if placed and checkout:
        warnings += lfs_checkout(git, Path(repo))
    return warnings


def lfs_checkout(git: Git, repo: Path) -> list[str]:
    """``git lfs checkout`` (local only, received profile) when ``git-lfs`` exists; else a warning that the
    pointers stay. Update/return mode run it in commit step c1, after the work tree moved."""
    if shutil.which("git-lfs"):
        proc = git.run(["lfs", "checkout"], cwd=Path(repo), spec=_wt_spec(Path(repo)), check=False)
        return [] if proc.returncode == 0 else ["git lfs checkout failed; run it in the repository"]
    return ["git-lfs is not installed here: LFS files stay pointers (install git-lfs, then git lfs checkout)"]


# ---------------------------------------------------------------------------------------------------------
# Path safety and size pre-check
# ---------------------------------------------------------------------------------------------------------


def path_issues(paths: Sequence[str], rules: PlatformRules) -> list[tuple[str, str]]:
    """``(path, reason)`` for every received path that violates ``paths.check_rel`` on this target. On POSIX
    targets control characters and non-UTF-8 bytes are legal file names that git reproduces
    (``check_rel(..., allow_control=True)``, W095); every other rule (``.git`` spellings, traversal, length,
    Windows names) applies."""
    issues = []
    for p in paths:
        try:
            check_rel(p, rules, allow_control=True)
        except PolicyDenied as exc:
            issues.append((p, str(exc.details.get("reason", "unsafe"))))
    return issues


def _precheck(git: Git, qdir: Path, item: _Item, dest_for_disk: Path, cfg: PolicyConfig) -> None:
    total, entries = GitProbe(git, profile="received").ls_tree_sizes(str(qdir), item.worktree_tree)
    if entries > cfg.max_files:
        raise PayloadTooLarge(
            "the work tree has more entries than allowed", entries=entries, max_files=cfg.max_files, field="max_files"
        )
    if total > cfg.max_total_bytes:
        raise PayloadTooLarge("the work tree is larger than allowed", bytes=total, max_bytes=cfg.max_total_bytes)
    probe = dest_for_disk
    while not probe.exists() and probe != probe.parent:
        probe = probe.parent
    free = shutil.disk_usage(probe).free
    if total * _DISK_MARGIN > free:
        raise InsufficientStorage(
            "not enough free space for the work tree",
            need_bytes=int(total * _DISK_MARGIN),
            free_bytes=free,
            path=str(probe),
        )


def _paths_of(git: Git, qdir: Path, tree: str) -> list[str]:
    return GitProbe(git, profile="received").ls_tree_paths(str(qdir), tree)


# ---------------------------------------------------------------------------------------------------------
# Prepare
# ---------------------------------------------------------------------------------------------------------


def prepare_fresh(
    git: Git,
    store: BlockSource,
    item_pack: dict,
    partial: Path,
    *,
    rules: PlatformRules,
    cfg: PolicyConfig,
    cancelled: Callable[[], bool] = _never,
    dest: Path | None = None,
    fallback: Path | None = None,
    qdir: Path | None = None,
    alternates: Path | None = None,
    claimed: bool = False,
) -> GitApplyState:
    """Fresh mode PREPARE (DESIGN §9.5): ``partial`` (must not exist; its parent must) is created 0700, gets a new
    repository, the verified objects, refs from the pack document, remote config, HEAD, the checkout
    (W → work tree, I → index) and the stash. NTFS-invalid or otherwise unsafe paths ⇒ ``NO_CHECKOUT`` (objects
    and refs only). ``dest`` is where ``commit`` renames the partial to; ``fallback`` where it goes when ``dest``
    appeared meanwhile. ``alternates`` (late fallback from update/return mode, whose pack is thin against that
    repository): its objects complete the new repository, which is repacked self-contained before PREPARE ends.
    ``claimed``: the caller created ``partial`` (empty, recorded — ``materialize``). Something already at the path
    is never used or removed: ``ConflictError(kind=partial_exists)`` (L3-F08). A failure removes the partial."""
    git.ensure_supported()
    item = parse_item(item_pack)
    partial = Path(partial)
    if dest is None:
        raise ValueError("prepare_fresh needs dest")
    open_partial(partial, claimed=claimed, dest=dest)
    warnings: list[str] = []
    try:
        git.run(["init", "-q", "--template=", f"--object-format={item.fmt}", str(partial)], cwd=None, spec=RECEIVED)
        q = Path(qdir) if qdir is not None else partial / ".git" / "ctxh-quarantine"
        quarantine_and_verify(git, store, item_pack, q, alternates=alternates, cancelled=cancelled)
        _precheck(git, q, item, partial, cfg)
        paths = _paths_of(git, q, item.worktree_tree)
        issues = path_issues(paths, rules)
        found = collisions(paths, rules) if rules.case_insensitive else []
        filters = attribute_filter_names(git, q, item.worktree_tree)
        transfer_pack(q, partial / ".git")
        shutil.rmtree(q, ignore_errors=True)
        alt_file = partial / ".git" / "objects" / "info" / "alternates"
        if alternates is not None:
            alt_file.write_text(str(alternates) + "\n", encoding="utf-8")
        cmds = [("create", name, sha) for name, sha in sorted(item.refs.items())]
        _update_refs(git, partial, cmds)
        if item.head_ref is not None:
            git.run(["symbolic-ref", "HEAD", item.head_ref], cwd=partial, spec=_wt_spec(partial))
        elif item.head_commit is not None:
            git.run(
                ["update-ref", "--no-deref", "HEAD", item.head_commit],
                cwd=partial,
                spec=_wt_spec(partial, identity=True),
            )
        warnings += write_remote_config(git, partial, item.remotes, item.upstreams)
        warnings += place_lfs(git, partial, store, item.lfs) if item.lfs else []
        mode = DestMode.FRESH
        if issues:
            mode = DestMode.NO_CHECKOUT
            shown = ", ".join(f"{p!r} ({why})" for p, why in issues[:5])
            warnings.append(f"not checked out: {len(issues)} path(s) are invalid on this system: {shown}")
        else:
            _checkout(git, partial, item, filters, cancelled)
            if found:
                shown = ", ".join(f"{a!r}/{b!r}" for a, b in found[:5])
                warnings.append(f"case-colliding paths on this filesystem (git shows one as modified): {shown}")
        _store_stash(git, partial, item.stash)
        if alternates is not None:
            git.run(["repack", "-a", "-d", "-q"], cwd=partial, spec=_wt_spec(partial), cancelled=cancelled)
            alt_file.unlink()
            git.run(
                ["rev-list", "--objects", "--all", "--indexed-objects", "--quiet"], cwd=partial, spec=_wt_spec(partial)
            )
    except BaseException:
        shutil.rmtree(partial, ignore_errors=True)
        raise
    return GitApplyState(
        dest=Path(dest),
        mode=mode,
        partial=partial,
        h0=None,
        old_refs={},
        added_paths=(),
        out_state=None,
        qdir=None,
        fallback=Path(fallback) if fallback is not None else None,
        planned_refs=dict(item.refs),
        warnings=tuple(warnings),
        case_collisions=tuple(found),
    )


def _checkout(git: Git, repo: Path, item: _Item, filters: Sequence[str], cancelled: Callable[[], bool]) -> None:
    spec = _wt_spec(repo, filters=filters)
    git.run(["read-tree", "--end-of-options", item.worktree_tree], cwd=repo, spec=spec, cancelled=cancelled)
    git.run(["checkout-index", "-a", "-f"], cwd=repo, spec=spec, cancelled=cancelled)
    git.run(["read-tree", "--end-of-options", item.index_tree], cwd=repo, spec=spec, cancelled=cancelled)
    git.run(["update-index", "-q", "--refresh"], cwd=repo, spec=spec, check=False, cancelled=cancelled)


def _plan_refs(
    git: Git, qdir: Path, item: _Item, current: dict[str, str], *, head_ref: str | None, head_old_allowed: str | None
) -> tuple[dict[str, str], tuple[str, ...], dict[str, str]]:
    """``(planned new values, skipped ref names, old values of planned refs)``: new refs are created, existing ones
    move only fast-forward (tags never); ``head_ref`` may move non-fast-forward when its old value is exactly
    ``head_old_allowed`` (return mode)."""
    gd = _bare(qdir)
    planned: dict[str, str] = {}
    skipped: list[str] = []
    old: dict[str, str] = {}
    for name, new in sorted(item.refs.items()):
        cur = current.get(name)
        if cur == new:
            continue
        if cur is None:
            planned[name] = new
            continue
        if name == head_ref and head_old_allowed is not None and cur == head_old_allowed:
            planned[name], old[name] = new, cur
            continue
        if name.startswith("refs/tags/"):
            skipped.append(name)
            continue
        ff = git.run(
            [*gd, "merge-base", "--is-ancestor", "--end-of-options", cur, new], cwd=None, spec=RECEIVED, check=False
        )
        if ff.returncode == 0:
            planned[name], old[name] = new, cur
        else:
            skipped.append(name)
    return planned, tuple(skipped), old


def refuse_orphaned_lock(git: Git, repo: Path) -> None:
    """Raise when ``index.lock`` exists in the repository at ``repo`` (README limit): a git killed in the middle of
    a command leaves it behind and git then refuses every index change. The plugin never removes it — a live git
    process may own it — so the step stops with ``ConflictError(kind=index_lock, needs_attention=True)`` naming
    the lock and what to do."""
    gitdir, _ = git_dirs(git, Path(repo))
    lock = gitdir / "index.lock"
    if os.path.lexists(lock):
        raise ConflictError(
            f"an interrupted git command left {lock} behind in {repo}; the plugin does not remove it (a running git "
            "process may own it): remove it once no git process runs in this repository, then hand over again",
            kind="index_lock",
            needs_attention=True,
            path=str(repo),
            lock=str(lock),
        )


def _open_existing(git: Git, dest: Path) -> tuple[Path, Path]:
    probe = GitProbe(git, profile="received")
    top = probe.toplevel(str(dest))
    if top is None or os.path.realpath(top) != os.path.realpath(dest):
        raise ConflictError("destination is not the root of a git work tree", kind="not_ours", path=str(dest))
    gitdir, common = git_dirs(git, dest)
    op = in_progress_op(gitdir)
    if op:
        raise ConflictError(f"a {op} is in progress at the destination", kind="dirty", path=str(dest))
    if (gitdir / "index.lock").exists():
        raise ConflictError("index.lock present at the destination", kind="dirty", path=str(dest))
    return gitdir, common


def _common_checks(
    git: Git, q: Path, item: _Item, dest: Path, cfg: PolicyConfig, rules: PlatformRules | None
) -> tuple[list[str], list[tuple[str, str]]]:
    _precheck(git, q, item, dest, cfg)
    rules = rules or cfg.platform_rules()
    paths = _paths_of(git, q, item.worktree_tree)
    issues = path_issues(paths, rules)
    if issues:
        raise ConflictError("received paths are invalid on this system", kind="unsafe_paths", count=len(issues))
    found = collisions(paths, rules) if rules.case_insensitive else []
    if found:
        raise ConflictError(
            "received paths collide on this case-insensitive filesystem",
            kind="case_collision",
            pairs=[list(p) for p in found[:20]],
        )
    return attribute_filter_names(git, q, item.worktree_tree), found


def _added(git: Git, q: Path, base: str, tree: str) -> tuple[str, ...]:
    proc = git.run(
        [
            *_bare(q),
            "diff-tree",
            "-r",
            "-z",
            "--name-only",
            "--no-renames",
            "--diff-filter=A",
            "--end-of-options",
            base,
            tree,
        ],
        cwd=None,
        spec=RECEIVED,
    )
    return tuple(os.fsdecode(p) for p in proc.stdout.split(b"\x00") if p)


def prepare_update(
    git: Git,
    store: BlockSource,
    item_pack: dict,
    dest: Path,
    *,
    cfg: PolicyConfig,
    cancelled: Callable[[], bool] = _never,
    qdir: Path | None = None,
    rules: PlatformRules | None = None,
) -> GitApplyState:
    """Update mode PREPARE (DESIGN §9.5): the destination must be a clean repository (status empty incl.
    untracked, no in-progress operation, no ``index.lock``) whose HEAD is an ancestor of the source HEAD; only
    objects are transferred now. Raises ``ConflictError`` (``dirty`` | ``diverged`` | ``case_collision`` |
    ``unsafe_paths`` | ``not_ours``) — the caller then materialises fresh at the fallback."""
    git.ensure_supported()
    item = parse_item(item_pack)
    dest = Path(dest)
    _, common = _open_existing(git, dest)
    clean, why = GitProbe(git, profile="received").status_clean(str(dest))
    if not clean:
        raise ConflictError("destination has uncommitted changes", kind="dirty", detail=why, path=str(dest))
    h0, head_ref = head_info(git, dest)
    if h0 is None or item.head_commit is None:
        raise ConflictError("destination or source has no commit", kind="diverged", path=str(dest))
    q = Path(qdir) if qdir is not None else common / "ctxh-quarantine"
    quarantine_and_verify(git, store, item_pack, q, alternates=common / "objects", cancelled=cancelled)
    try:
        anc = git.run(
            [*_bare(q), "merge-base", "--is-ancestor", "--end-of-options", h0, item.head_commit],
            cwd=None,
            spec=RECEIVED,
            check=False,
        )
        if anc.returncode != 0:
            raise ConflictError("destination HEAD is not an ancestor of the source HEAD", kind="diverged")
        current = _current_refs(git, dest)
        planned, skipped, old = _plan_refs(git, q, item, current, head_ref=None, head_old_allowed=None)
        target_branch = item.head_ref
        if target_branch is not None and target_branch in skipped:
            raise ConflictError("the HEAD branch diverged at the destination", kind="diverged", ref=target_branch)
        _, found = _common_checks(git, q, item, dest, cfg, rules)
        lfs_warnings = place_lfs(git, dest, store, item.lfs, checkout=False) if item.lfs else []
        added = _added(git, q, h0, item.worktree_tree)
        transfer_pack(q, common)
    finally:
        shutil.rmtree(q, ignore_errors=True)
    warnings = [f"ref {name} diverged at the destination and was not moved" for name in skipped] + lfs_warnings
    return GitApplyState(
        dest=dest,
        mode=DestMode.UPDATE,
        partial=None,
        h0=h0,
        old_refs=old,
        added_paths=added,
        out_state=None,
        head_ref_before=head_ref,
        planned_refs=planned,
        skipped_refs=skipped,
        stash_before=_stash_depth(git, dest),
        warnings=tuple(warnings),
        case_collisions=tuple(found),
    )


def _out_objects(tmp: Path) -> Path:
    """Object directory holding the recomputed out-state trees and untracked blobs of a return-mode destination
    (the outgoing pack's temporary objects never landed in the user's repository); used as an alternate by the
    return-mode checkout and its inverse."""
    return Path(tmp) / "out-objects"


def _alt_env(state: GitApplyState) -> dict | None:
    if state.mode is DestMode.RETURN and state.tmp is not None:
        return {"GIT_ALTERNATE_OBJECT_DIRECTORIES": str(_out_objects(state.tmp))}
    return None


def _state_diff(a: GitState, b: GitState) -> list[str]:
    parts = []
    if a.head != b.head:
        parts.append("HEAD moved")
    if a.index_tree != b.index_tree:
        parts.append("index changed")
    if a.worktree_tree != b.worktree_tree:
        parts.append("work tree changed")
    return parts


def prepare_return(
    git: Git,
    store: BlockSource,
    item_pack: dict,
    dest: Path,
    out_state: GitState,
    *,
    tmp: Path,
    cfg: PolicyConfig,
    cancelled: Callable[[], bool] = _never,
    qdir: Path | None = None,
    rules: PlatformRules | None = None,
) -> GitApplyState:
    """Return mode PREPARE (R4, DESIGN §9.5): the destination's current ``(HEAD, index tree, work-tree tree)``,
    recomputed with the **recorded** exclusions, must equal ``out_state`` — "unchanged since the handover", dirty
    work included, not "clean". Otherwise ``ConflictError(kind=changed_since_handover)`` names what changed."""
    git.ensure_supported()
    item = parse_item(item_pack)
    dest = Path(dest)
    _, common = _open_existing(git, dest)
    Path(tmp).mkdir(parents=True, exist_ok=True, mode=0o700)
    sw = compute_state(
        git,
        dest,
        out_state.exclude_pathspecs,
        _out_objects(Path(tmp)),
        Path(tmp) / "scratch",
        force=True,
        cancelled=cancelled,
    )
    now = GitState(sw.head or "", sw.index_tree, sw.worktree_tree, sw.branch, tuple(out_state.exclude_pathspecs))
    diff = _state_diff(now, out_state)
    if diff:
        raise ConflictError(
            "the destination changed since the handover: " + ", ".join(diff),
            kind="changed_since_handover",
            detail=diff,
            path=str(dest),
        )
    _, head_ref = head_info(git, dest)
    q = Path(qdir) if qdir is not None else Path(tmp) / "quarantine"
    quarantine_and_verify(git, store, item_pack, q, alternates=common / "objects", cancelled=cancelled)
    try:
        current = _current_refs(git, dest)
        planned, skipped, old = _plan_refs(
            git, q, item, current, head_ref=head_ref, head_old_allowed=out_state.head or None
        )
        _, found = _common_checks(git, q, item, dest, cfg, rules)
        lfs_warnings = place_lfs(git, dest, store, item.lfs, checkout=False) if item.lfs else []
        transfer_pack(q, common)
    finally:
        shutil.rmtree(q, ignore_errors=True)
    warnings = [f"ref {name} diverged at the destination and was not moved" for name in skipped] + lfs_warnings
    return GitApplyState(
        dest=dest,
        mode=DestMode.RETURN,
        partial=None,
        h0=out_state.head or None,
        old_refs=old,
        added_paths=(),
        out_state=out_state,
        head_ref_before=head_ref,
        planned_refs=planned,
        skipped_refs=skipped,
        stash_before=_stash_depth(git, dest),
        warnings=tuple(warnings),
        case_collisions=tuple(found),
        tmp=Path(tmp),
    )


# ---------------------------------------------------------------------------------------------------------
# Commit, rollback, discard
# ---------------------------------------------------------------------------------------------------------


def _rename_dir(partial: Path, dest: Path) -> None:
    if sys.platform == "win32":  # pragma: no cover - exercised on Windows targets only
        if dest.exists():
            if dest.is_dir() and not any(dest.iterdir()):
                dest.rmdir()
            else:
                raise FileExistsError(errno.EEXIST, "destination appeared", str(dest))
        for attempt in range(5):
            if dest.exists():
                raise FileExistsError(errno.EEXIST, "destination appeared", str(dest))
            try:
                os.rename(partial, dest)
                return
            except PermissionError:
                time.sleep(min(2.0, 0.2 * (2**attempt)))
        os.rename(partial, dest)
        return
    os.rename(partial, dest)


_APPEARED = (errno.ENOTEMPTY, errno.EEXIST, errno.ENOTDIR, errno.EISDIR)


def commit_fresh(state: GitApplyState) -> Path:
    """c1 for fresh/no-checkout: permissions from the umask, then ``rename(partial, dest)`` — on POSIX directly
    (replaces an empty directory, fails on a non-empty one: no ``rmdir`` window). A destination that appeared
    meanwhile ⇒ ``fallback``. Returns the final path."""
    assert state.partial is not None  # noqa: S101 - fresh states always carry their partial
    os.chmod(state.partial, 0o777 & ~_umask())
    try:
        _rename_dir(state.partial, state.dest)
        _fsync_dir(state.dest.parent)
        return state.dest
    except OSError as exc:
        if exc.errno not in _APPEARED or state.fallback is None:
            os.chmod(state.partial, 0o700)
            raise ConflictError(
                "the destination appeared during commit", kind="dest_appeared", path=str(state.dest)
            ) from exc
    try:
        _rename_dir(state.partial, state.fallback)
    except OSError as exc:
        os.chmod(state.partial, 0o700)
        raise ConflictError(
            "the fallback destination exists too", kind="dest_appeared", path=str(state.fallback)
        ) from exc
    _fsync_dir(state.fallback.parent)
    return state.fallback


def _move_head(git: Git, repo: Path, item: _Item, head_before: str | None) -> None:
    if item.head_ref is not None:
        if item.head_ref != head_before:
            git.run(["symbolic-ref", "HEAD", item.head_ref], cwd=repo, spec=_wt_spec(repo))
    elif item.head_commit is not None:
        git.run(["update-ref", "--no-deref", "HEAD", item.head_commit], cwd=repo, spec=_wt_spec(repo, identity=True))


def _apply_refs(git: Git, repo: Path, state: GitApplyState) -> None:
    cmds = []
    for name, new in sorted(state.planned_refs.items()):
        old = state.old_refs.get(name)
        cmds.append(("update", name, new, old) if old else ("create", name, new))
    _update_refs(git, repo, cmds)


def _post_state(git: Git, state: GitApplyState, item: _Item, repo: Path, tmp: Path) -> bool:
    """True when the repository already shows the whole effect of ``commit`` (refs, HEAD, index tree and
    work-tree tree as planned): a forward step that finished in a process which died before its result was
    journaled. Stash and LFS checkout are idempotent and are repeated by the caller."""
    current = _current_refs(git, repo)
    if any(current.get(name) != new for name, new in state.planned_refs.items()):
        return False
    h_now, ref_now = head_info(git, repo)
    want_ref = item.head_ref if item.head_ref is not None else (None if item.head_commit else state.head_ref_before)
    if ref_now != want_ref or (item.head_commit is not None and h_now != item.head_commit):
        return False
    excl = state.out_state.exclude_pathspecs if state.out_state is not None else ()
    now = worktree_state(git, str(repo), excl, tmp)
    return (now.index_tree, now.worktree_tree) == (item.index_tree, item.worktree_tree)


def _pre_state(git: Git, state: GitApplyState, repo: Path, tmp: Path) -> bool:
    """True when nothing of ``commit`` has touched the repository yet (the preconditions PREPARE recorded)."""
    current = _current_refs(git, repo)
    if any(current.get(name) != old for name, old in state.old_refs.items()):
        return False
    if any(name in current for name in state.planned_refs if name not in state.old_refs):
        return False
    h_now, ref_now = head_info(git, repo)
    if ref_now != state.head_ref_before:
        return False
    if state.mode is DestMode.UPDATE:
        clean, _ = GitProbe(git, profile="received").status_clean(str(repo))
        return clean and h_now == state.h0
    assert state.out_state is not None  # noqa: S101 - return states carry it
    now = worktree_state(git, str(repo), state.out_state.exclude_pathspecs, tmp)
    return not _state_diff(now, state.out_state)


def commit(
    git: Git,
    state: GitApplyState,
    item_pack: dict,
    *,
    mark: Callable[[], None] | None = None,
    resume: bool = False,
    tmp: Path | None = None,
) -> Path:
    """Forward c1 step of one git item (DESIGN §3.4, §9.5); preconditions are re-checked first. Returns the final
    path (``fallback`` when a fresh destination appeared). Raises ``ConflictError`` when the destination changed
    since PREPARE (the caller rolls back the done steps).

    Update/return mode is atomic towards its caller: ``mark`` (if given) runs right before the first change to
    the repository — the caller records durably that this step began — and an exception after it undoes the
    partial change (``rollback(partial=True)``, only when the repository no longer shows its prepared state — a
    refused first ``read-tree`` changed nothing and must not be "undone") before it propagates. ``resume=True``
    (the step began in a process that died; contract X11): a repository that already shows the full effect only
    gets the idempotent tail (stash, LFS checkout); one that shows part of it is rolled back to its prepared state
    first; then the step runs normally. ``tmp``: scratch directory for those state checks (default: the state's
    own ``tmp``). Fresh mode is one rename (``commit_fresh``); its resumption is the caller's (directory
    identity)."""
    if state.mode in (DestMode.FRESH, DestMode.NO_CHECKOUT):
        return commit_fresh(state)
    item = parse_item(item_pack)
    repo = state.dest
    if resume:  # the step began in a process that died: a lock left now is that process's git, killed mid-way
        refuse_orphaned_lock(git, repo)
    _open_existing(git, repo)
    scratch = Path(tmp or state.tmp or (Path(state.qdir).parent if state.qdir is not None else repo.parent))
    if resume:
        if _post_state(git, state, item, repo, scratch / "resume"):
            _tail(git, repo, item)
            return repo
        if not _pre_state(git, state, repo, scratch / "resume"):
            rollback(git, state, item_pack, partial=True)
    h_now, ref_now = head_info(git, repo)
    if state.mode is DestMode.UPDATE:
        clean, why = GitProbe(git, profile="received").status_clean(str(repo))
        if not clean or h_now != state.h0 or ref_now != state.head_ref_before:
            raise ConflictError("the destination changed since prepare", kind="dirty", detail=why, path=str(repo))
    else:
        assert state.out_state is not None and state.tmp is not None  # noqa: S101 - return states carry both
        now = worktree_state(git, str(repo), state.out_state.exclude_pathspecs, state.tmp)
        diff = _state_diff(now, state.out_state)
        if diff or ref_now != state.head_ref_before:
            raise ConflictError(
                "the destination changed since prepare: " + ", ".join(diff or ["HEAD branch"]),
                kind="changed_since_handover",
                path=str(repo),
            )
    current = _current_refs(git, repo)
    for name, old in state.old_refs.items():
        if current.get(name) != old:
            raise ConflictError("a ref moved since prepare", kind="dirty", ref=name)
    filters = attribute_filter_names(git, _bare_of(git, repo), item.worktree_tree)
    spec = _wt_spec(repo, filters=filters)
    alt = _alt_env(state)
    if mark is not None:
        mark()
    try:
        if state.mode is DestMode.UPDATE:
            git.run(
                ["read-tree", "-u", "-m", "--end-of-options", state.h0, item.worktree_tree],
                cwd=repo,
                spec=spec,
                env_extra=alt,
            )
        else:
            assert state.out_state is not None  # noqa: S101
            git.run(
                ["read-tree", "--end-of-options", state.out_state.worktree_tree], cwd=repo, spec=spec, env_extra=alt
            )
            git.run(["update-index", "-q", "--refresh"], cwd=repo, spec=spec, env_extra=alt, check=False)
            git.run(
                ["read-tree", "-u", "-m", "--end-of-options", state.out_state.worktree_tree, item.worktree_tree],
                cwd=repo,
                spec=spec,
                env_extra=alt,
            )
        git.run(["read-tree", "--end-of-options", item.index_tree], cwd=repo, spec=spec, env_extra=alt)
        git.run(["update-index", "-q", "--refresh"], cwd=repo, spec=spec, env_extra=alt, check=False)
        _apply_refs(git, repo, state)
        _move_head(git, repo, item, state.head_ref_before)
        _tail(git, repo, item)
    except Exception as exc:
        # undo the partial change; an undo that fails (an orphaned index.lock of the killed git, …) is reported as
        # such — the caller must not take the repository for untouched
        try:
            if not _pre_state(git, state, repo, scratch / "resume"):
                rollback(git, state, item_pack, partial=True)
        except Exception as undo:
            raise undo_failed(exc, undo, path=repo) from exc
        raise
    return repo


def _tail(git: Git, repo: Path, item: _Item) -> None:
    """The idempotent end of the update/return forward step: missing stash entries, LFS checkout."""
    _store_stash(git, repo, item.stash, skip=_stash_commits(git, repo))
    if item.lfs:
        lfs_checkout(git, repo)


def _bare_of(git: Git, repo: Path) -> Path:
    return git_dirs(git, repo)[1]


def rollback(git: Git, state: GitApplyState, item_pack: dict | None = None, *, partial: bool = False) -> None:
    """Inverse of ``commit`` (DESIGN §3.4 c1 inverse); idempotent. For fresh mode ``state.dest`` must be the path
    ``commit`` returned. ``partial=True``: the forward step may have stopped anywhere (an exception or a crash in
    its middle); return mode then resets the work tree to the out-state from an index that holds every path of
    the received tree (``read-tree -u --reset``) instead of the two-way merge, which refuses half-updated files.
    Update mode always resets (``read-tree -u --reset H0``) and removes the paths the forward step added."""
    if state.mode in (DestMode.FRESH, DestMode.NO_CHECKOUT):
        if state.partial is not None and not state.partial.exists() and state.dest.is_dir():
            os.rename(state.dest, state.partial)
            os.chmod(state.partial, 0o700)
        return
    repo = state.dest
    refuse_orphaned_lock(git, repo)  # never run git against a locked index; the lock is the user's to remove
    spec = _wt_spec(repo)
    alt = _alt_env(state)
    item = parse_item(item_pack) if item_pack is not None else None
    if state.mode is DestMode.RETURN and partial and item is not None:
        out = state.out_state
        assert out is not None  # noqa: S101 - return states carry it
        git.run(["read-tree", "--end-of-options", item.worktree_tree], cwd=repo, spec=spec, env_extra=alt)
        git.run(
            ["read-tree", "-u", "--reset", "--end-of-options", out.worktree_tree], cwd=repo, spec=spec, env_extra=alt
        )
        git.run(["read-tree", "--end-of-options", out.index_tree], cwd=repo, spec=spec, env_extra=alt)
        git.run(["update-index", "-q", "--refresh"], cwd=repo, spec=spec, env_extra=alt, check=False)
    elif state.mode is DestMode.UPDATE:
        git.run(["read-tree", "-u", "--reset", "--end-of-options", state.h0], cwd=repo, spec=spec, env_extra=alt)
        for rel in state.added_paths:
            path = Path(os.fsdecode(os.path.join(os.fsencode(repo), os.fsencode(rel))))
            with contextlib.suppress(FileNotFoundError, IsADirectoryError, NotADirectoryError):
                if not stat.S_ISDIR(os.lstat(path).st_mode):
                    os.unlink(path)
        _remove_empty_parents(repo, state.added_paths)
    else:
        out = state.out_state
        assert out is not None  # noqa: S101 - return states carry it
        w_in = item.worktree_tree if item is not None else None
        if w_in is not None:
            git.run(["read-tree", "--end-of-options", w_in], cwd=repo, spec=spec, env_extra=alt)
            git.run(["update-index", "-q", "--refresh"], cwd=repo, spec=spec, env_extra=alt, check=False)
            git.run(
                ["read-tree", "-u", "-m", "--end-of-options", w_in, out.worktree_tree],
                cwd=repo,
                spec=spec,
                env_extra=alt,
            )
        git.run(["read-tree", "--end-of-options", out.index_tree], cwd=repo, spec=spec, env_extra=alt)
        git.run(["update-index", "-q", "--refresh"], cwd=repo, spec=spec, env_extra=alt, check=False)
    current = _current_refs(git, repo)
    cmds = []
    for name in state.planned_refs:
        old = state.old_refs.get(name)
        if old is not None:
            if current.get(name) != old:
                cmds.append(("update", name, old))
        elif name in current:
            cmds.append(("delete", name))
    _update_refs(git, repo, cmds)
    if state.head_ref_before is not None:
        git.run(["symbolic-ref", "HEAD", state.head_ref_before], cwd=repo, spec=spec)
    elif state.h0 is not None:
        git.run(["update-ref", "--no-deref", "HEAD", state.h0], cwd=repo, spec=_wt_spec(repo, identity=True))
    extra = _stash_depth(git, repo) - state.stash_before
    for _ in range(max(0, extra)):
        git.run(["stash", "drop", "-q"], cwd=repo, spec=spec)  # entries exist (extra > 0): a failure is a failure


def _remove_empty_parents(repo: Path, rels: Sequence[str]) -> None:
    dirs = {os.path.dirname(r) for r in rels if "/" in r}
    for d in sorted(dirs, key=lambda x: -x.count("/")):
        cur = d
        while cur:
            path = Path(os.fsdecode(os.path.join(os.fsencode(repo), os.fsencode(cur))))
            try:
                path.rmdir()
            except OSError:
                break
            cur = os.path.dirname(cur)


def discard(git: Git, state: GitApplyState) -> None:
    """Drop a prepared, uncommitted item: the partial directory (fresh) and any quarantine; objects already copied
    into an existing repository are unreferenced and harmless (git's own gc removes them)."""
    if state.mode in (DestMode.FRESH, DestMode.NO_CHECKOUT) and state.partial is not None and state.partial.exists():
        shutil.rmtree(state.partial, ignore_errors=True)
    if state.qdir is not None:
        shutil.rmtree(state.qdir, ignore_errors=True)


__all__ = [
    "GitApplyState",
    "GitCommandError",
    "commit",
    "commit_fresh",
    "discard",
    "parse_item",
    "path_issues",
    "place_lfs",
    "prepare_fresh",
    "prepare_return",
    "prepare_update",
    "quarantine_and_verify",
    "refuse_orphaned_lock",
    "rollback",
    "state_from_dict",
    "state_to_dict",
    "stream_pack",
    "transfer_pack",
    "write_remote_config",
]


def _p(value: Path | None) -> str | None:
    return enc_path(str(value)) if value is not None else None


def _P(value: str | None) -> Path | None:
    return Path(dec_path(value)) if value is not None else None


def state_to_dict(s: GitApplyState) -> dict:
    """JSON-safe snapshot of a prepared git item (journal / crash recovery)."""
    out = None
    if s.out_state is not None:
        out = {
            "head": s.out_state.head,
            "index_tree": s.out_state.index_tree,
            "worktree_tree": s.out_state.worktree_tree,
            "branch": s.out_state.branch,
            "exclude_pathspecs": list(s.out_state.exclude_pathspecs),
        }
    return {
        "dest": _p(s.dest),
        "mode": s.mode.value,
        "partial": _p(s.partial),
        "h0": s.h0,
        "old_refs": dict(s.old_refs),
        "added_paths": [enc_path(p) for p in s.added_paths],
        "out_state": out,
        "qdir": _p(s.qdir),
        "fallback": _p(s.fallback),
        "head_ref_before": s.head_ref_before,
        "planned_refs": dict(s.planned_refs),
        "skipped_refs": list(s.skipped_refs),
        "stash_before": s.stash_before,
        "warnings": list(s.warnings),
        "case_collisions": [list(p) for p in s.case_collisions],
        "tmp": _p(s.tmp),
    }


def state_from_dict(d: dict) -> GitApplyState:
    """Inverse of ``state_to_dict``."""
    out = d.get("out_state")
    return GitApplyState(
        dest=_P(d["dest"]),  # type: ignore[arg-type]
        mode=DestMode(d["mode"]),
        partial=_P(d.get("partial")),
        h0=d.get("h0"),
        old_refs=dict(d.get("old_refs") or {}),
        added_paths=tuple(dec_path(p) for p in d.get("added_paths") or ()),
        out_state=GitState(
            out["head"], out["index_tree"], out["worktree_tree"], out.get("branch"), tuple(out["exclude_pathspecs"])
        )
        if out
        else None,
        qdir=_P(d.get("qdir")),
        fallback=_P(d.get("fallback")),
        head_ref_before=d.get("head_ref_before"),
        planned_refs=dict(d.get("planned_refs") or {}),
        skipped_refs=tuple(d.get("skipped_refs") or ()),
        stash_before=int(d.get("stash_before") or 0),
        warnings=tuple(d.get("warnings") or ()),
        case_collisions=tuple(tuple(p) for p in d.get("case_collisions") or ()),
        tmp=_P(d.get("tmp")),
    )
