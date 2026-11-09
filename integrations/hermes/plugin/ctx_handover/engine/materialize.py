"""Workspace decisions, the journaled workspace transaction and the pack-item entry (layer L5, stdlib only).

``assess`` decides per plan item and destination how it materialises (DESIGN §8.6, §9.5–§9.7, §10.4): the
target denylist and allow-roots first (R19), then fresh / update / return / conflict ⇒ fresh at the fallback.
Local uncommitted changes are never overwritten (I5): a dirty repository, a foreign directory, a file, a changed
tree or anything an extra would write into without a local choice goes to a fresh directory instead.

``WorkspaceTxn`` runs one target job's workspace part: ``prepare_item`` (heavy, invisible: partial directories,
quarantined git objects, staged files), then ``commit_steps`` hands the conductor's commit op the c1 steps as
``(name, forward, inverse)`` in plan order, ``place_hermes_files`` the c3 pair; ``discard`` drops whatever was
prepared and not committed. Every sub-step is reported through ``journal(kind, step, data)``.

Crash resumption (contract X11): the caller journals ``snapshot()`` after PREPARE and after every c1/c3 step;
``WorkspaceTxn.restore`` rebuilds the transaction from the newest snapshot in another process. Every forward and
inverse step can run again after the process that ran it died at any point: a fresh directory or a staged extra
file is recognised by the ``(st_dev, st_ino)`` PREPARE recorded, a git update/return step leaves a marker in
``staging/ws-steps/`` before its first change, and a placed session file is recorded there before it is linked
in. A forward step that ran completely is only finished, a partial one is undone and run again, and an inverse
touches only what this job put there.

``map_state_of`` is the one producer of ``MapRow.state`` (contract X09); ``pack_plan_item`` the one entry the
source's orchestration packs a plan item through (contract X12).
"""

from __future__ import annotations

import contextlib
import hashlib
import json
import os
import shutil
import stat
import time
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field, replace
from pathlib import Path

from . import filetree, gitapply, gitpack
from .contracts import BlockSink
from .errors import BadRequest, ConflictError, PolicyDenied
from .gitops import (
    Git,
    GitProbe,
    dec_path,
    enc_path,
    git_dirs,
    is_sha,
    iter_object,
    name_report,
    object_manifest,
    sha256_file,
    worktree_state,
)
from .ignore import IgnoreMatcher
from .model import (
    Conflict,
    DestMode,
    EnvFacts,
    GitState,
    ItemKind,
    MapRow,
    NotTransferred,
    PlanItem,
    TargetDecision,
)
from .paths import PlatformRules
from .policy import HERMES_FILE_KINDS, PolicyConfig, check_source_file, check_target_dest, is_hermes_protected

SNAPSHOT_VERSION = 2
"""``snapshot()`` format; ``restore`` reads 1 (no identities) and 2."""
STEP_DIR = "ws-steps"
"""Staging subdirectory of the transaction's durable step records (with ``quarantine/``, ``git-tmp/``,
``tree-backup/``)."""
DOC_LIST_CAP = 1000
"""Most ``excluded``/``skipped`` entries a file-tree pack item lists (the rest is counted in a warning)."""


def _never() -> bool:
    return False


@dataclass(frozen=True)
class Assessment:
    """How an item lands at a destination (INTERFACES §4.3)."""

    mode: DestMode
    conflict: Conflict | None
    git_have: tuple[str, ...]
    need_bytes: int
    warnings: tuple[str, ...]


def fallback_dest(dest: str, now: float | None = None) -> str:
    """``<dest>-handover-<YYYYmmdd-HHMMSS>`` (DESIGN §9.7)."""
    stamp = time.strftime("%Y%m%d-%H%M%S", time.localtime(now if now is not None else time.time()))
    return f"{str(dest).rstrip('/')}-handover-{stamp}"


def _need(item: PlanItem) -> int:
    est = max(0, int(item.size_estimate))
    return est * 3 if item.kind is ItemKind.GIT_REPO else est * 2


# ---------------------------------------------------------------------------------------------------------
# Workspace-map row state (contract X09, DESIGN §9.8)
# ---------------------------------------------------------------------------------------------------------

_GIT_STATE_KEYS = ("head", "index_tree", "worktree_tree", "branch", "exclude_pathspecs")


def _is_fingerprint(value: object) -> bool:
    return isinstance(value, Mapping) and isinstance(value.get("files"), Mapping)


def tree_fingerprint_of(row_state: Mapping | None) -> dict | None:
    """The file-tree fingerprint of a map row's ``state``: ``{"tree": fp}`` (contract X09) or a bare fingerprint
    (rows written before X09); None for anything else."""
    if not isinstance(row_state, Mapping):
        return None
    inner = row_state.get("tree")
    if _is_fingerprint(inner):
        return dict(inner)
    if _is_fingerprint(row_state):
        return dict(row_state)
    return None


def _git_fields(state: object) -> dict | None:
    if isinstance(state, GitState):
        return {k: getattr(state, k) for k in _GIT_STATE_KEYS}
    if not isinstance(state, Mapping):
        return None
    nested = state.get("git")
    if isinstance(nested, Mapping):  # the nested form A wrote in layer 1: {"git": GitState, "branch": …}
        fields = dict(nested)
        if fields.get("branch") is None and state.get("branch") is not None:
            fields["branch"] = state.get("branch")
        return fields
    if "index_tree" in state or "worktree_tree" in state:
        return dict(state)
    return None


def _flat_git(fields: Mapping, branch: str | None) -> dict:
    head = fields.get("head") or ""
    index_tree, worktree_tree = fields.get("index_tree"), fields.get("worktree_tree")
    if not (is_sha(index_tree) and is_sha(worktree_tree)) or (head and not is_sha(head)):
        raise BadRequest("git state without valid tree ids", field="state")
    ref = branch if branch is not None else fields.get("branch")
    if ref is not None and not isinstance(ref, str):
        raise BadRequest("git state branch must be a string", field="state.branch")
    excl = fields.get("exclude_pathspecs") or ()
    if isinstance(excl, str) or not all(isinstance(x, str) for x in excl):
        raise BadRequest("git state exclusions must be strings", field="state.exclude_pathspecs")
    return {
        "head": head,
        "index_tree": index_tree,
        "worktree_tree": worktree_tree,
        "branch": ref,
        "exclude_pathspecs": list(excl),
    }


def map_state_of(state: GitState | Mapping | None, branch: str | None = None) -> dict | None:
    """``MapRow.state`` of a workspace item — the one producer for ``out`` rows (source, r3) and ``in`` rows
    (target, c7) (contract X09, DESIGN §9.8).

    Git: a ``GitState``, its wire form or the nested ``{"git": …, "branch": …}`` form ⇒ flat
    ``{"head", "index_tree", "worktree_tree", "branch", "exclude_pathspecs"}`` (``branch`` overrides when given).
    File tree: a ``filetree.fingerprint`` or ``{"tree": fp}`` ⇒ ``{"tree": fp}``. None ⇒ None. Idempotent.
    ``BadRequest`` (``field=state…``) for anything else."""
    if state is None:
        return None
    fp = tree_fingerprint_of(state) if isinstance(state, Mapping) else None
    if fp is not None:
        return {"tree": fp}
    fields = _git_fields(state)
    if fields is None:
        raise BadRequest("not a workspace state", field="state")
    return _flat_git(fields, branch)


def git_state_of(row_state: Mapping | None) -> GitState | None:
    """The ``GitState`` of a map row's ``state`` (every form ``map_state_of`` accepts), or None when the row holds
    no usable git state (a reader never raises: no state means no return mode)."""
    fields = _git_fields(row_state)
    if fields is None:
        return None
    try:
        flat = _flat_git(fields, None)
    except BadRequest:
        return None
    return GitState(
        head=flat["head"],
        index_tree=flat["index_tree"],
        worktree_tree=flat["worktree_tree"],
        branch=flat["branch"],
        exclude_pathspecs=tuple(flat["exclude_pathspecs"]),
    )


def assess(
    item: PlanItem,
    dest: str,
    *,
    git: Git,
    probe: GitProbe,
    out_row: MapRow | None,
    tree_fingerprint: dict | None,
    target_facts: EnvFacts,
    fallback: Callable[[str], str],
    tmp: Path,
    cfg: PolicyConfig,
    recorded: bool,
    local_choice: bool,
) -> Assessment:
    """Decide fresh / update / return / conflict for ``item`` at ``dest`` (see module docstring).

    ``policy.check_target_dest`` runs first: a denied destination ⇒ conflict ``not_allowed`` with a fallback under
    ``workspace_root``. ``recorded`` = the destination comes from this host's own workspace map (return trip);
    ``local_choice`` = typed on this host. ``out_row`` is this host's ``out`` row for the destination (return
    mode); ``tree_fingerprint`` the plugin's last write there (tree update mode; the ``in`` row's state in either
    form ``tree_fingerprint_of`` reads). ``need_bytes`` counts bytes on the destination's filesystem (W023)."""
    d = Path(dest)
    need = _need(item)
    warnings: list[str] = []

    def conflict(kind: str, detail: str, alt: str | None = None) -> Assessment:
        return Assessment(DestMode.FRESH, Conflict(kind, detail, alt or fallback(dest)), (), need, tuple(warnings))

    verdict = check_target_dest(d, cfg, recorded=recorded, local_choice=local_choice)
    if not verdict.allowed:
        root = Path(target_facts.workspace_root or (cfg.allow_roots[0] if cfg.allow_roots else cfg.home))
        alt = str(root / (d.name or "workspace"))
        if os.path.lexists(alt):
            alt = fallback(alt)
        return conflict("not_allowed", f"destination not allowed ({verdict.reason})", alt)
    if item.kind in (ItemKind.EXTRA_FILE, ItemKind.EXTRA_DIR) and not (recorded or local_choice):
        return conflict("remote_extra", "extras land at their path only through a local map or the return trip")
    if not os.path.lexists(d):
        return Assessment(DestMode.FRESH, None, (), need, tuple(warnings))
    st = os.lstat(d)
    if stat.S_ISLNK(st.st_mode):
        return conflict("not_ours", "the destination is a symbolic link")
    if not stat.S_ISDIR(st.st_mode):
        if item.kind is ItemKind.EXTRA_FILE and stat.S_ISREG(st.st_mode):
            return conflict("exists_file", "a different file may exist at the destination")
        return conflict("exists_file", "a file exists at the destination")
    if item.kind is ItemKind.EXTRA_FILE:
        return conflict("exists_file", "a directory exists at the destination")
    try:
        empty = not any(os.scandir(d))
    except OSError:
        return conflict("not_ours", "the destination cannot be read")
    if empty:
        return Assessment(DestMode.FRESH, None, (), need, tuple(warnings))
    if item.kind is ItemKind.GIT_REPO:
        top = probe.toplevel(str(d))
        if top is None or os.path.realpath(top) != os.path.realpath(d):
            return conflict("not_ours", "the destination holds other content")
        roots = set(probe.root_commits(str(d)))
        theirs = set(item.git.root_commits) if item.git else set()
        out_state = git_state_of(out_row.state) if out_row is not None and out_row.direction == "out" else None
        if not (roots & theirs) and out_state is None:
            return conflict("foreign", "a different repository is at the destination")
        if out_state is not None:
            now = worktree_state(git, str(d), out_state.exclude_pathspecs, tmp)
            if (now.head, now.index_tree, now.worktree_tree) == (
                out_state.head,
                out_state.index_tree,
                out_state.worktree_tree,
            ):
                return Assessment(DestMode.RETURN, None, probe.ref_tips(str(d)), need, tuple(warnings))
            return conflict("dirty", "the repository changed since it was handed over")
        clean, why = probe.status_clean(str(d))
        if not clean:
            return conflict("dirty", why or "uncommitted changes at the destination")
        return Assessment(DestMode.UPDATE, None, probe.ref_tips(str(d)), need, tuple(warnings))
    fp = tree_fingerprint_of(tree_fingerprint)
    mode = DestMode.UPDATE
    out_fp = tree_fingerprint_of(out_row.state) if out_row is not None and out_row.direction == "out" else None
    if out_fp is not None:
        fp, mode = out_fp, DestMode.RETURN
    if not fp:
        return conflict("not_ours", "the destination holds content the plugin did not write")
    same, why = filetree.matches_fingerprint(d, dict(fp.get("files") or {}))
    if not same:
        return conflict("foreign", f"the destination changed since the plugin wrote it ({why})")
    return Assessment(mode, None, (), need, tuple(warnings))


# ---------------------------------------------------------------------------------------------------------
# Workspace transaction
# ---------------------------------------------------------------------------------------------------------


@dataclass
class _Prepared:
    item_id: str
    kind: str
    mode: DestMode
    dest: Path
    pack_item: dict
    state: object
    created_parents: list[Path] = field(default_factory=list)
    final: Path | None = None
    committed: bool = False
    ident: tuple[int, int] | None = None
    """``(st_dev, st_ino)`` of the partial directory (fresh) or the staged temp file (extra) at PREPARE: a rename or
    link keeps it, so a resumed step recognises its own result at the destination or the fallback."""


def _ident(path: Path | None) -> tuple[int, int] | None:
    if path is None:
        return None
    try:
        st = os.lstat(path)
    except OSError:
        return None
    return st.st_dev, st.st_ino


def _same(path: Path | None, ident: tuple[int, int] | None) -> bool:
    return ident is not None and path is not None and _ident(path) == tuple(ident)


def _fsync_dir(path: Path) -> None:
    with contextlib.suppress(OSError):
        fd = os.open(path, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


class _StepLog:
    """Durable records of the transaction under ``staging/ws-steps/`` (0700 directory, 0600 files, fsynced): a
    marker per git update/return step written before its first change, one record per session file before it is
    linked in and per directory created for one."""

    def __init__(self, staging: Path):
        self.root = Path(staging) / STEP_DIR

    def _path(self, name: str) -> Path:
        return self.root / (hashlib.sha256(name.encode("utf-8", "surrogateescape")).hexdigest()[:40] + ".json")

    def put(self, name: str, data: dict) -> None:
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        path = self._path(name)
        tmp = path.with_suffix(".tmp")
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0), 0o600)
        try:
            os.write(fd, json.dumps({"name": name, **data}, sort_keys=True).encode("utf-8"))
            os.fsync(fd)
        finally:
            os.close(fd)
        os.replace(tmp, path)
        _fsync_dir(self.root)

    def get(self, name: str) -> dict | None:
        try:
            with open(self._path(name), "rb") as fh:
                data = json.loads(fh.read(1024 * 1024))
        except (OSError, ValueError):
            return None
        return data if isinstance(data, dict) and data.get("name") == name else None

    def drop(self, name: str) -> None:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(self._path(name))

    def items(self, prefix: str) -> list[dict]:
        """Every record whose name starts with ``prefix`` (sorted by name)."""
        out = []
        try:
            entries = sorted(os.listdir(self.root))
        except OSError:
            return out
        for entry in entries:
            if not entry.endswith(".json"):
                continue
            try:
                with open(self.root / entry, "rb") as fh:
                    data = json.loads(fh.read(1024 * 1024))
            except (OSError, ValueError):
                continue
            if isinstance(data, dict) and str(data.get("name", "")).startswith(prefix):
                out.append(data)
        return sorted(out, key=lambda d: d["name"])


class WorkspaceTxn:
    """One target job's workspace work (see module docstring). ``cfg``/``rules`` (additions with defaults) are
    the target's policy and path rules; ``now`` the clock for fallback names."""

    def __init__(
        self,
        job_id: str,
        staging: Path,
        git: Git,
        store,
        journal: Callable[[str, str, dict], None],
        *,
        cfg: PolicyConfig | None = None,
        rules: PlatformRules | None = None,
        now: Callable[[], float] = time.time,
        cancelled: Callable[[], bool] = _never,
    ):
        self.job_id = job_id
        self.job6 = job_id[-6:].lower()
        self.staging = Path(staging)
        self.git = git
        self.store = store
        self.journal = journal
        self.cfg = cfg
        self.rules = rules or (cfg.platform_rules() if cfg is not None else None)
        self.now = now
        self.cancelled = cancelled
        self.items: list[_Prepared] = []
        self.skipped: list[NotTransferred] = []
        self.warnings: list[str] = []
        self._hermes_placed: list[Path] = []
        self._hermes_dirs: list[Path] = []
        self._steps = _StepLog(self.staging)

    # -- helpers ------------------------------------------------------------------------------------------

    def _require_cfg(self) -> PolicyConfig:
        if self.cfg is None:
            raise ValueError("WorkspaceTxn needs the target's PolicyConfig")
        return self.cfg

    def _partial_for(self, dest: Path, item_id: str) -> Path:
        name = f".ctxh-{self.job6}"
        if any(p.dest.parent == dest.parent for p in self.items):
            name += f"-{item_id[:8]}"
        return dest.parent / name

    def _ensure_parent(self, dest: Path) -> list[Path]:
        created: list[Path] = []
        missing = []
        cur = dest.parent
        while not cur.exists():
            missing.append(cur)
            cur = cur.parent
        for d in reversed(missing):
            os.mkdir(d, 0o777)
            created.append(d)
        return created

    def _fresh_target(self, decision: TargetDecision) -> tuple[Path, Path]:
        cfg = self._require_cfg()
        dest = Path(decision.conflict.fallback_dest if decision.conflict else decision.dest)
        recorded = decision.reason in {"return-mapping", "user"} and decision.conflict is None
        verdict = check_target_dest(dest, cfg, recorded=recorded, local_choice=recorded)
        if not verdict.allowed:
            raise PolicyDenied(f"destination not allowed ({verdict.reason})", reason="target_denied", path=str(dest))
        return dest, Path(fallback_dest(str(dest), self.now()))

    def _check_fresh(self, dest: Path) -> None:
        verdict = check_target_dest(dest, self._require_cfg(), recorded=False, local_choice=False)
        if not verdict.allowed:
            raise PolicyDenied(
                f"fallback destination not allowed ({verdict.reason})", reason="target_denied", path=str(dest)
            )

    def _prepare_git_fresh(
        self, item_id: str, dest: Path, fb: Path, pack_item: dict, *, alternates: Path | None = None
    ) -> _Prepared:
        cfg = self._require_cfg()
        created = self._ensure_parent(dest)
        partial = self._partial_for(dest, item_id)
        try:
            state = gitapply.prepare_fresh(
                self.git,
                self.store,
                pack_item,
                partial,
                rules=self.rules,
                cfg=cfg,
                cancelled=self.cancelled,
                dest=dest,
                fallback=fb,
                qdir=self.staging / "quarantine" / item_id,
                alternates=alternates,
            )
        except BaseException:
            self._remove_dirs(created)
            raise
        return _Prepared(item_id, "git", state.mode, dest, pack_item, state, created, ident=_ident(state.partial))

    def _prepare_tree_fresh(self, item_id: str, dest: Path, fb: Path, pack_item: dict) -> _Prepared:
        cfg = self._require_cfg()
        created = self._ensure_parent(dest)
        partial = self._partial_for(dest, item_id)
        try:
            state = filetree.prepare_fresh(
                self.store,
                pack_item["manifest"],
                partial,
                self.rules,
                self.cancelled,
                dest=dest,
                max_files=cfg.max_files,
                fallback=fb,
            )
        except BaseException:
            self._remove_dirs(created)
            raise
        self.skipped.extend(state.skipped)
        return _Prepared(item_id, "tree", DestMode.FRESH, dest, pack_item, state, created, ident=_ident(state.partial))

    @staticmethod
    def _remove_dirs(dirs: Sequence[Path]) -> None:
        for d in sorted(dirs, key=lambda p: -len(p.parts)):
            with contextlib.suppress(OSError):
                d.rmdir()

    # -- prepare ------------------------------------------------------------------------------------------

    def prepare_item(
        self, item: PlanItem, decision: TargetDecision, pack_item: dict, *, out_row: MapRow | None = None
    ) -> None:
        """PREPARE one item according to ``decision`` (DESIGN §3.4 step 1). Update/return attempts that turn out
        to conflict (dirty, diverged, changed since the handover, case collision) fall back to a fresh directory at
        the conflict's fallback. ``out_row`` is the map row whose state applies: the ``out`` row (return mode) or
        the ``in`` row with the tree fingerprint (tree update mode)."""
        if decision.mode is DestMode.SKIP:
            self.journal("info", f"prepare:{item.item_id}", {"mode": "skip"})
            return
        kind = pack_item.get("kind")
        prepared: _Prepared
        if kind == "git_repo":
            prepared = self._prepare_git(item, decision, pack_item, out_row)
        elif kind == "file_tree":
            prepared = self._prepare_tree(item, decision, pack_item, out_row)
        elif kind == "extra_file":
            prepared = self._prepare_extra_file(item, decision, pack_item)
        else:
            raise PolicyDenied(f"unknown pack item kind {kind!r}", reason="special")
        self.items.append(prepared)
        warnings = list(getattr(prepared.state, "warnings", ()) or ())
        self.warnings.extend(warnings)
        self.journal(
            "done",
            f"prepare:{item.item_id}",
            {"mode": prepared.mode.value, "dest": str(prepared.dest), "warnings": warnings},
        )

    def _prepare_git(self, item, decision, pack_item, out_row) -> _Prepared:
        cfg = self._require_cfg()
        if decision.mode in (DestMode.UPDATE, DestMode.RETURN) and decision.conflict is None:
            dest = Path(decision.dest)
            try:
                if decision.mode is DestMode.RETURN:
                    out_state = git_state_of(out_row.state if out_row else None)
                    if out_state is None:
                        raise ConflictError("no recorded out-state for the return trip", kind="changed_since_handover")
                    state = gitapply.prepare_return(
                        self.git,
                        self.store,
                        pack_item,
                        dest,
                        out_state,
                        tmp=self.staging / "git-tmp" / item.item_id,
                        cfg=cfg,
                        cancelled=self.cancelled,
                        qdir=self.staging / "quarantine" / item.item_id,
                        rules=self.rules,
                    )
                else:
                    state = gitapply.prepare_update(
                        self.git,
                        self.store,
                        pack_item,
                        dest,
                        cfg=cfg,
                        cancelled=self.cancelled,
                        qdir=self.staging / "quarantine" / item.item_id,
                        rules=self.rules,
                    )
                return _Prepared(item.item_id, "git", state.mode, dest, pack_item, state)
            except ConflictError as exc:
                fb = Path(fallback_dest(str(dest), self.now()))
                self.warnings.append(f"{dest}: {exc.message}; materialised fresh at {fb}")
                self.journal("info", f"prepare:{item.item_id}", {"fallback": str(fb), "why": exc.details.get("kind")})
                self._check_fresh(fb)
                alt = None
                with contextlib.suppress(Exception):
                    alt = git_dirs(self.git, dest)[1] / "objects"
                return self._prepare_git_fresh(
                    item.item_id, fb, Path(fallback_dest(str(fb), self.now() + 1)), pack_item, alternates=alt
                )
        dest, fb = self._fresh_target(decision)
        return self._prepare_git_fresh(item.item_id, dest, fb, pack_item)

    def _prepare_tree(self, item, decision, pack_item, out_row) -> _Prepared:
        cfg = self._require_cfg()
        if decision.mode in (DestMode.UPDATE, DestMode.RETURN) and decision.conflict is None:
            dest = Path(decision.dest)
            fp = tree_fingerprint_of(out_row.state) if out_row is not None else None
            try:
                if fp is None:
                    raise ConflictError("no recorded fingerprint for the destination", kind="foreign")
                state = filetree.prepare_update(
                    self.store,
                    pack_item["manifest"],
                    dest,
                    fp,
                    self.rules,
                    self.staging / "tree-backup" / item.item_id,
                    self.cancelled,
                    tag=self.job6,
                    max_files=cfg.max_files,
                )
                self.skipped.extend(state.skipped)
                return _Prepared(item.item_id, "tree", decision.mode, dest, pack_item, state)
            except ConflictError as exc:
                fb = Path(fallback_dest(str(dest), self.now()))
                self.warnings.append(f"{dest}: {exc.message}; materialised fresh at {fb}")
                self._check_fresh(fb)
                return self._prepare_tree_fresh(
                    item.item_id, fb, Path(fallback_dest(str(fb), self.now() + 1)), pack_item
                )
        dest, fb = self._fresh_target(decision)
        return self._prepare_tree_fresh(item.item_id, dest, fb, pack_item)

    def _prepare_extra_file(self, item, decision, pack_item) -> _Prepared:
        dest, _ = self._fresh_target(decision)
        obj = pack_item.get("object")
        content_sha = object_manifest(self.store, obj).sha256
        if os.path.lexists(dest):
            st = os.lstat(dest)
            if stat.S_ISREG(st.st_mode) and sha256_file(dest) == content_sha:
                return _Prepared(item.item_id, "extra", DestMode.SKIP, dest, pack_item, None)
            raise ConflictError("a different file exists at the destination", kind="exists_file", path=str(dest))
        created = self._ensure_parent(dest)
        temp = dest.parent / f".{dest.name}.ctxh-{self.job6}"
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temp)
        entry = {
            "path": dest.name,
            "type": "file",
            "mode": pack_item.get("mode", "100644"),
            "size": int(pack_item.get("size", 0)),
            "mtime_ns": int(pack_item.get("mtime_ns", 0)),
            "object": obj,
        }
        try:
            filetree._write_file(self.store, entry, temp, self.cancelled)
        except BaseException:
            self._remove_dirs(created)
            raise
        return _Prepared(item.item_id, "extra", DestMode.FRESH, dest, pack_item, temp, created, ident=_ident(temp))

    # -- commit -------------------------------------------------------------------------------------------

    def commit_steps(self) -> list[tuple[str, Callable[[], None], Callable[[], None]]]:
        """The c1 steps in plan order: ``(name, forward, inverse)``. A forward step records the final
        destination (a fresh destination that appeared meanwhile ⇒ fallback) in the journal."""
        steps = []
        for p in self.items:
            if p.mode is DestMode.SKIP:
                continue
            steps.append((f"c1:{p.item_id}", self._forward(p), self._inverse(p)))
        return steps

    def _fresh_landed(self, p: _Prepared) -> Path | None:
        """Where an earlier run of this fresh/extra step put the prepared directory or file (identity check at the
        destination and the fallback), or None."""
        fallback = getattr(p.state, "fallback", None)
        for cand in (p.final, p.dest, fallback):
            if cand is not None and _same(Path(cand), p.ident):
                return Path(cand)
        return None

    def _forward(self, p: _Prepared) -> Callable[[], None]:
        def run() -> None:
            final: Path | None = None
            if p.kind in ("git", "tree") and p.mode in (DestMode.FRESH, DestMode.NO_CHECKOUT):
                partial = getattr(p.state, "partial", None)
                if p.ident is not None and partial is not None and not os.path.lexists(partial):
                    final = self._fresh_landed(p)  # resumed: the rename happened in a process that died
                    if final is None:
                        raise ConflictError("the prepared directory is gone", kind="partial_missing", path=str(partial))
            if final is None and p.kind == "git":
                marker = f"c1:{p.item_id}:forward"
                final = Path(
                    gitapply.commit(
                        self.git,
                        p.state,
                        p.pack_item,
                        mark=lambda: self._steps.put(marker, {}),
                        resume=self._steps.get(marker) is not None,
                        tmp=self.staging / "git-tmp" / p.item_id,
                    )
                )
            elif final is None and p.kind == "tree":
                final = Path(filetree.commit(p.state)["dest"])
            elif final is None:
                final = self._extra_forward(p)
            p.final, p.committed = Path(final), True
            self.journal("done", f"c1:{p.item_id}", {"dest": str(final), "mode": p.mode.value})

        return run

    def _extra_forward(self, p: _Prepared) -> Path:
        temp: Path = p.state
        if not os.path.lexists(temp):
            if _same(p.dest, p.ident) or (p.ident is None and os.path.lexists(p.dest)):
                return p.dest  # linked and the temp name removed by a process that died
            raise ConflictError("the prepared file is gone", kind="partial_missing", path=str(temp))
        try:
            os.link(temp, p.dest)
        except FileExistsError:
            if not os.path.samefile(temp, p.dest):
                raise ConflictError(
                    "a file appeared at the destination", kind="dest_appeared", path=str(p.dest)
                ) from None
        os.unlink(temp)
        return p.dest

    def _inverse(self, p: _Prepared) -> Callable[[], None]:
        def run() -> None:
            final = p.final or p.dest
            if p.kind in ("git", "tree") and p.mode in (DestMode.FRESH, DestMode.NO_CHECKOUT):
                landed = self._fresh_landed(p) if p.ident is not None else final
                if landed is not None:
                    final = landed
                    if p.kind == "git":
                        gitapply.rollback(self.git, replace(p.state, dest=final), p.pack_item)
                    else:
                        filetree.rollback(replace(p.state, dest=final))
            elif p.kind == "git":
                # a rollback that was itself interrupted resets (partial); a first one merges and so refuses to
                # overwrite what someone changed in the repository since the forward step
                marker = f"c1:{p.item_id}:inverse"
                interrupted = self._steps.get(marker) is not None
                self._steps.put(marker, {})
                gitapply.rollback(self.git, replace(p.state, dest=final), p.pack_item, partial=interrupted)
            elif p.kind == "tree":
                filetree.rollback(replace(p.state, dest=final))
            else:
                temp: Path = p.state
                ours = _same(final, p.ident) if p.ident is not None else True
                if ours and os.path.lexists(final):
                    if not os.path.lexists(temp):
                        os.link(final, temp)
                    os.unlink(final)
            p.committed = False
            self.journal("done", f"c1-inverse:{p.item_id}", {"dest": str(final)})

        return run

    # -- HERMES files (c3) --------------------------------------------------------------------------------

    def place_hermes_files(self, files: Sequence[tuple[dict, Path]]) -> tuple[Callable[[], None], Callable[[], None]]:
        """c3 forward/inverse (DESIGN §6.11, §10.6): each ``(pack item, target path)`` — the path is built by the
        caller from kind and checked basename — is written with ``O_EXCL|O_NOFOLLOW`` only inside a session-file
        area of this host (``policy.check_target_dest(hermes_file_kind=…)``); an identical existing file counts as
        placed, a different one is skipped and reported. The inverse deletes what was placed.

        Both are repeatable (contract X11): before a file is linked in, its target and the identity of the
        written file are recorded under ``staging/ws-steps/``; a resumed forward step counts a target holding that
        identity as placed by this job, and the inverse removes exactly those targets (and the directories it
        created), also when the snapshot that names them was never journaled. A failing forward step undoes itself
        before the error propagates."""
        cfg = self._require_cfg()

        def forward() -> None:
            try:
                for pack_item, target in files:
                    self._place_one(cfg, pack_item, Path(target))
            except Exception:
                with contextlib.suppress(Exception):
                    inverse()
                raise
            self.journal("done", "c3", {"placed": [str(p) for p in self._hermes_placed]})

        def inverse() -> None:
            recorded = set()
            for rec in self._steps.items("c3:"):
                target = Path(dec_path(rec["target"]))
                recorded.add(target)
                if _same(target, (rec.get("dev"), rec.get("ino"))):
                    os.unlink(target)
                self._steps.drop(rec["name"])
            for path in reversed(self._hermes_placed):
                if path not in recorded:  # snapshot of format 1: no records
                    with contextlib.suppress(FileNotFoundError):
                        os.unlink(path)
            self._hermes_placed.clear()
            dirs = list(self._hermes_dirs)
            for rec in self._steps.items("c3dir:"):
                dirs.append(Path(dec_path(rec["dir"])))
                self._steps.drop(rec["name"])
            self._remove_dirs(sorted(set(dirs)))
            self._hermes_dirs.clear()
            self.journal("done", "c3-inverse", {})

        return forward, inverse

    def _place_one(self, cfg: PolicyConfig, pack_item: dict, target: Path) -> None:
        kinds = [pack_item.get("hermes_kind")] if pack_item.get("hermes_kind") else list(HERMES_FILE_KINDS)
        allowed = any(
            check_target_dest(target, cfg, recorded=False, local_choice=False, hermes_file_kind=k).allowed
            for k in kinds
            if k in HERMES_FILE_KINDS
        )
        if not allowed:
            self.skipped.append(NotTransferred(str(target), "target_denied"))
            return
        obj = pack_item.get("object")
        want = object_manifest(self.store, obj).sha256
        name = "c3:" + enc_path(str(target))
        temp = target.parent / f".{target.name}.ctxh-{self.job6}"
        if os.path.lexists(target):
            rec = self._steps.get(name)
            if rec is not None and _same(target, (rec.get("dev"), rec.get("ino"))):
                if target not in self._hermes_placed:  # linked by a run of this step that died before its result
                    self._hermes_placed.append(target)
                with contextlib.suppress(FileNotFoundError):
                    os.unlink(temp)
                return
            same = stat.S_ISREG(os.lstat(target).st_mode) and sha256_file(target) == want
            if not same:
                self.skipped.append(NotTransferred(str(target), "exists"))
            return
        for d in self._ensure_parent(target):
            self._hermes_dirs.append(d)
            self._steps.put("c3dir:" + enc_path(str(d)), {"dir": enc_path(str(d))})
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temp)
        fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        try:
            for raw in iter_object(self.store, obj, cancelled=self.cancelled):
                os.write(fd, raw)
            os.fsync(fd)
            st = os.fstat(fd)
        finally:
            os.close(fd)
        self._steps.put(name, {"target": enc_path(str(target)), "dev": st.st_dev, "ino": st.st_ino})
        try:
            os.link(temp, target)
        except FileExistsError:
            self.skipped.append(NotTransferred(str(target), "exists"))
            os.unlink(temp)
            self._steps.drop(name)
            return
        os.unlink(temp)
        self._hermes_placed.append(target)

    # -- snapshot / restore (crash recovery of the journaled commit op) ------------------------------------

    def snapshot(self) -> dict:
        """JSON-safe state of every prepared item and of the placed HERMES files; journal it after PREPARE and after
        each c1/c3 step so a new process can ``restore`` the transaction and continue in the journaled direction."""
        items = []
        for p in self.items:
            if p.kind == "git":
                state = gitapply.state_to_dict(p.state)
            elif p.kind == "tree":
                state = filetree.state_to_dict(p.state)
            else:
                state = enc_path(str(p.state)) if p.state is not None else None
            items.append(
                {
                    "item_id": p.item_id,
                    "kind": p.kind,
                    "mode": p.mode.value,
                    "dest": enc_path(str(p.dest)),
                    "pack_item": p.pack_item,
                    "state": state,
                    "created_parents": [enc_path(str(d)) for d in p.created_parents],
                    "final": enc_path(str(p.final)) if p.final is not None else None,
                    "committed": p.committed,
                    "ident": list(p.ident) if p.ident is not None else None,
                }
            )
        return {
            "v": SNAPSHOT_VERSION,
            "job_id": self.job_id,
            "items": items,
            "hermes_placed": [enc_path(str(x)) for x in self._hermes_placed],
            "hermes_dirs": [enc_path(str(x)) for x in self._hermes_dirs],
        }

    @classmethod
    def restore(
        cls,
        snapshot: dict,
        staging: Path,
        git: Git,
        store,
        journal: Callable[[str, str, dict], None],
        *,
        cfg: PolicyConfig | None = None,
        rules: PlatformRules | None = None,
        now: Callable[[], float] = time.time,
        cancelled: Callable[[], bool] = _never,
    ) -> WorkspaceTxn:
        """A transaction rebuilt from ``snapshot()`` in another process (same staging directory): its steps continue
        in the journaled direction and run only what is missing (module docstring). Reads formats 1 and 2."""
        if not isinstance(snapshot, dict) or snapshot.get("v") not in (1, SNAPSHOT_VERSION):
            raise BadRequest("unknown workspace snapshot format", field="snapshot.v")
        txn = cls(snapshot["job_id"], staging, git, store, journal, cfg=cfg, rules=rules, now=now, cancelled=cancelled)
        for d in snapshot.get("items") or ():
            kind = d["kind"]
            if kind == "git":
                state = gitapply.state_from_dict(d["state"])
            elif kind == "tree":
                state = filetree.state_from_dict(d["state"])
            else:
                state = Path(dec_path(d["state"])) if d.get("state") is not None else None
            txn.items.append(
                _Prepared(
                    item_id=d["item_id"],
                    kind=kind,
                    mode=DestMode(d["mode"]),
                    dest=Path(dec_path(d["dest"])),
                    pack_item=d["pack_item"],
                    state=state,
                    created_parents=[Path(dec_path(x)) for x in d.get("created_parents") or ()],
                    final=Path(dec_path(d["final"])) if d.get("final") else None,
                    committed=bool(d.get("committed")),
                    ident=(int(d["ident"][0]), int(d["ident"][1])) if d.get("ident") else None,
                )
            )
        txn._hermes_placed = [Path(dec_path(x)) for x in snapshot.get("hermes_placed") or ()]
        txn._hermes_dirs = [Path(dec_path(x)) for x in snapshot.get("hermes_dirs") or ()]
        return txn

    # -- discard and results ------------------------------------------------------------------------------

    def discard(self) -> None:
        """Drop everything prepared and not committed (partials, staged files, quarantines, created parents)."""
        for p in reversed(self.items):
            if p.committed:
                continue
            if p.kind == "git":
                gitapply.discard(self.git, p.state)
            elif p.kind == "tree":
                filetree.discard(p.state)
            elif p.state is not None:
                with contextlib.suppress(OSError):
                    os.unlink(p.state)
            self._remove_dirs(p.created_parents)
        for sub in ("quarantine", "git-tmp", "tree-backup", STEP_DIR):
            shutil.rmtree(self.staging / sub, ignore_errors=True)
        self.journal("done", "discard", {})

    def dests(self) -> list[tuple[str, str, DestMode]]:
        """``(item_id, destination, mode)`` per prepared item (the final path after commit)."""
        return [(p.item_id, str(p.final or p.dest), p.mode) for p in self.items]

    def map_states(self) -> dict[str, dict]:
        """Per item the ``MapRow.state`` of its ``in`` row after commit (``map_state_of``: git — the received state,
        flat; tree — ``{"tree": fingerprint}``). Single extra files have none."""
        out: dict[str, dict] = {}
        for p in self.items:
            if p.kind == "git":
                pi = p.pack_item
                head = pi.get("head") or {}
                state = map_state_of(
                    {
                        "head": head.get("commit") or "",
                        "index_tree": pi.get("index_tree"),
                        "worktree_tree": pi.get("worktree_tree"),
                        "branch": head.get("ref"),
                        "exclude_pathspecs": list(pi.get("exclude_pathspecs") or []),
                    }
                )
            elif p.kind == "tree":
                state = map_state_of(dict(p.state.fingerprint))
            else:
                continue
            if state is not None:
                out[p.item_id] = state
        return out


# ---------------------------------------------------------------------------------------------------------
# Source side: one entry that packs a plan item (contract X12, DESIGN §9.1)
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class PackedItem:
    """One packed plan item: ``doc`` is its pack-document item (``item_id`` included), ``state`` the ``MapRow.state``
    of the source's ``out`` row (``map_state_of`` form; None for single files)."""

    doc: dict
    state: dict | None
    excluded: tuple[NotTransferred, ...] = ()
    skipped: tuple[NotTransferred, ...] = ()
    warnings: tuple[str, ...] = ()


def _listed(entries: Sequence[NotTransferred], what: str, warnings: list[str]) -> list[dict]:
    if len(entries) > DOC_LIST_CAP:
        warnings.append(f"{len(entries) - DOC_LIST_CAP} more {what} paths not listed in the pack document")
    return [{"path": n.path, "why": n.why} for n in entries[:DOC_LIST_CAP]]


def _refused(item: PlanItem, kind: str, why: str) -> PackedItem:
    doc = {"item_id": item.item_id, "kind": kind, "object": None, "skipped": why}
    return PackedItem(doc, None, skipped=(NotTransferred(enc_path(item.source_path), why),))


def _pack_single(item: PlanItem, path: Path, store: BlockSink, cancelled: Callable[[], bool]) -> dict | str:
    try:
        return filetree.pack_file(path, store, cancelled)
    except PolicyDenied as exc:
        return str(exc.details.get("reason") or "special")
    except FileNotFoundError:
        return "missing"
    except OSError:
        return "symlink" if os.path.islink(path) else "unreadable"


def pack_plan_item(
    item: PlanItem,
    *,
    git: Git,
    store: BlockSink,
    cfg: PolicyConfig,
    workdir: Path,
    created_at: float,
    basis_candidates: Sequence[str] = (),
    force: bool = False,
    extra_ignore: Sequence[str] = (),
    hermes_home: str | os.PathLike | None = None,
    cancelled: Callable[[], bool] = _never,
) -> PackedItem:
    """Pack one included plan item into ``store`` and build its pack-document item (DESIGN §9.1, §9.3, §9.6).

    * ``git_repo`` — ``gitpack.pack_repo`` in ``workdir`` (``staging/<job>/git/<item_id>``) against
      ``basis_candidates`` (the target's ``git_have``), item from ``gitpack.pack_item``: ``lfs_objects``,
      ``case_collisions``, ``ntfs_invalid``; ``state`` = the outgoing git state (R4).
    * ``file_tree`` and ``extra_dir`` — ``filetree.scan`` (ignore rules incl. ``extra_ignore``, the policy) and
      ``pack_tree``; one ``file_tree`` item with ``manifest``, ``entries``, ``bytes``, ``excluded``, ``skipped``,
      ``case_collisions``, ``ntfs_invalid``; ``state`` = ``{"tree": fingerprint}``.
    * ``extra_file`` — the policy again (``check_source_file``, explicit, no link following), then
      ``filetree.pack_file`` (modes ``100644``/``100755``).
    * ``hermes_file`` — kind from ``PlanItem.hermes_kind``, the session-file checks of planning again (known kind,
      not protected, regular, ``max_hermes_file_bytes``); ``rel`` relative to ``hermes_home`` (else the basename).

    A single file the policy refuses at pack time becomes ``{"item_id", "kind", "object": None, "skipped": why}``
    (the target skips it). Raises like ``gitpack.pack_repo`` (``CapabilityMissing`` for shallow/partial clones,
    ``ConflictError(in_progress)``, ``PolicyDenied(untracked_count)``), ``filetree.scan`` (``max_files``) and
    ``Cancelled``; ``PolicyDenied(reason=special)`` for an unknown item kind."""
    if item.kind is ItemKind.GIT_REPO:
        res = gitpack.pack_repo(
            git,
            item.source_path,
            Path(workdir),
            basis_candidates=tuple(basis_candidates),
            cfg=cfg,
            store=store,
            created_at=created_at,
            force=force,
            cancelled=cancelled,
        )
        return PackedItem(
            gitpack.pack_item(item.item_id, res), map_state_of(res.state), res.excluded, res.skipped, res.warnings
        )
    if item.kind in (ItemKind.FILE_TREE, ItemKind.EXTRA_DIR):
        root = Path(item.source_path)
        entries, left = filetree.scan(root, IgnoreMatcher.for_tree(root, tuple(extra_ignore)), cfg, cancelled)
        manifest = filetree.pack_tree(entries, root, store, cancelled)
        pairs, invalid, warnings = name_report(os.fsdecode(filetree.entry_rel_bytes(e)) for e in entries)
        excluded = tuple(n for n in left if n.why == "secret")
        skipped = tuple(n for n in left if n.why != "secret")
        doc = {
            "item_id": item.item_id,
            "kind": "file_tree",
            "manifest": manifest,
            "entries": len(entries),
            "bytes": sum(int(e.size) for e in entries if e.type == "file"),
            "excluded": _listed(excluded, "excluded", warnings),
            "skipped": _listed(skipped, "skipped", warnings),
            "case_collisions": pairs,
            "ntfs_invalid": invalid,
        }
        state = map_state_of(filetree.fingerprint(root, entries))
        return PackedItem(doc, state, excluded, skipped, tuple(warnings))
    if item.kind is ItemKind.EXTRA_FILE:
        path = Path(item.source_path)
        verdict = check_source_file(path, cfg, explicit=True, follow_link=False)
        if not verdict.allowed:
            return _refused(item, "extra_file", verdict.reason or "special")
        fields = _pack_single(item, path, store, cancelled)
        if isinstance(fields, str):
            return _refused(item, "extra_file", fields)
        return PackedItem({"item_id": item.item_id, **fields, "kind": "extra_file"}, None)
    if item.kind is ItemKind.HERMES_FILE:
        path = Path(item.source_path)
        kind = item.hermes_kind
        if kind not in HERMES_FILE_KINDS:
            return _refused(item, "hermes_file", "hermes")
        try:
            st = os.lstat(path)
        except OSError:
            return _refused(item, "hermes_file", "missing")
        if is_hermes_protected(path, cfg, session_file_kind=kind) or not stat.S_ISREG(st.st_mode):
            return _refused(item, "hermes_file", "hermes")
        if st.st_size > cfg.max_hermes_file_bytes:
            return _refused(item, "hermes_file", "size")
        fields = _pack_single(item, path, store, cancelled)
        if isinstance(fields, str):
            return _refused(item, "hermes_file", fields)
        rel = path.name
        if hermes_home is not None:
            with contextlib.suppress(ValueError):
                rel = path.relative_to(Path(hermes_home)).as_posix()
        doc = {"item_id": item.item_id, **fields, "kind": "hermes_file", "hermes_kind": kind, "rel": enc_path(rel)}
        return PackedItem(doc, None)
    raise PolicyDenied(f"unknown plan item kind {item.kind!r}", reason="special")
