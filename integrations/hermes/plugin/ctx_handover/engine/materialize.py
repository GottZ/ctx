"""Target-side workspace decisions and the journaled workspace transaction (layer L5, stdlib only).

``assess`` decides per plan item and destination how it materialises (DESIGN §8.6, §9.5–§9.7, §10.4): the
target denylist and allow-roots first (R19), then fresh / update / return / conflict ⇒ fresh at the fallback.
Local uncommitted changes are never overwritten (I5): a dirty repository, a foreign directory, a file, a changed
tree or anything an extra would write into without a local choice goes to a fresh directory instead.

``WorkspaceTxn`` runs one target job's workspace part: ``prepare_item`` (heavy, invisible: partial directories,
quarantined git objects, staged files), then ``commit_steps`` hands the conductor's commit op the c1 steps as
``(name, forward, inverse)`` in plan order, ``place_hermes_files`` the c3 pair; ``discard`` drops whatever was
prepared and not committed. Every sub-step is reported through ``journal(kind, step, data)``.
"""

from __future__ import annotations

import contextlib
import os
import shutil
import stat
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field, replace
from pathlib import Path

from . import filetree, gitapply
from .errors import ConflictError, PolicyDenied
from .gitops import (
    Git,
    GitProbe,
    dec_path,
    enc_path,
    git_dirs,
    iter_object,
    object_manifest,
    sha256_file,
    worktree_state,
)
from .model import Conflict, DestMode, EnvFacts, GitState, ItemKind, MapRow, NotTransferred, PlanItem, TargetDecision
from .paths import PlatformRules
from .policy import HERMES_FILE_KINDS, PolicyConfig, check_target_dest


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


def _git_state_from(row_state: dict | None) -> GitState | None:
    if not isinstance(row_state, dict):
        return None
    try:
        return GitState(
            head=str(row_state.get("head") or ""),
            index_tree=str(row_state["index_tree"]),
            worktree_tree=str(row_state["worktree_tree"]),
            branch=row_state.get("branch"),
            exclude_pathspecs=tuple(row_state.get("exclude_pathspecs") or ()),
        )
    except KeyError:
        return None


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
    mode); ``tree_fingerprint`` the plugin's last write there (tree update mode)."""
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
        out_state = _git_state_from(out_row.state) if out_row is not None and out_row.direction == "out" else None
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
    fp = tree_fingerprint
    mode = DestMode.UPDATE
    if out_row is not None and out_row.direction == "out" and isinstance(out_row.state, dict):
        fp, mode = out_row.state, DestMode.RETURN
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
        return _Prepared(item_id, "git", state.mode, dest, pack_item, state, created)

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
        return _Prepared(item_id, "tree", DestMode.FRESH, dest, pack_item, state, created)

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
                    out_state = _git_state_from(out_row.state if out_row else None)
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
            fp = out_row.state if out_row is not None and isinstance(out_row.state, dict) else None
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
        return _Prepared(item.item_id, "extra", DestMode.FRESH, dest, pack_item, temp, created)

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

    def _forward(self, p: _Prepared) -> Callable[[], None]:
        def run() -> None:
            if p.kind == "git":
                final = gitapply.commit(self.git, p.state, p.pack_item)
            elif p.kind == "tree":
                final = Path(filetree.commit(p.state)["dest"])
            else:
                temp: Path = p.state
                os.link(temp, p.dest)
                os.unlink(temp)
                final = p.dest
            p.final, p.committed = Path(final), True
            self.journal("done", f"c1:{p.item_id}", {"dest": str(final), "mode": p.mode.value})

        return run

    def _inverse(self, p: _Prepared) -> Callable[[], None]:
        def run() -> None:
            final = p.final or p.dest
            if p.kind == "git":
                gitapply.rollback(self.git, replace(p.state, dest=final), p.pack_item)
            elif p.kind == "tree":
                filetree.rollback(replace(p.state, dest=final))
            else:
                temp: Path = p.state
                if os.path.lexists(final) and not os.path.lexists(temp):
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
        placed, a different one is skipped and reported. The inverse deletes what was placed."""
        cfg = self._require_cfg()

        def forward() -> None:
            for pack_item, target in files:
                target = Path(target)
                kinds = [pack_item.get("hermes_kind")] if pack_item.get("hermes_kind") else list(HERMES_FILE_KINDS)
                allowed = any(
                    check_target_dest(target, cfg, recorded=False, local_choice=False, hermes_file_kind=k).allowed
                    for k in kinds
                    if k in HERMES_FILE_KINDS
                )
                if not allowed:
                    self.skipped.append(NotTransferred(str(target), "target_denied"))
                    continue
                obj = pack_item.get("object")
                want = object_manifest(self.store, obj).sha256
                if os.path.lexists(target):
                    same = stat.S_ISREG(os.lstat(target).st_mode) and sha256_file(target) == want
                    if not same:
                        self.skipped.append(NotTransferred(str(target), "exists"))
                    continue
                for d in self._ensure_parent(target):
                    self._hermes_dirs.append(d)
                temp = target.parent / f".{target.name}.ctxh-{self.job6}"
                with contextlib.suppress(FileNotFoundError):
                    os.unlink(temp)
                fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
                try:
                    for raw in iter_object(self.store, obj, cancelled=self.cancelled):
                        os.write(fd, raw)
                finally:
                    os.close(fd)
                try:
                    os.link(temp, target)
                except FileExistsError:
                    self.skipped.append(NotTransferred(str(target), "exists"))
                    os.unlink(temp)
                    continue
                os.unlink(temp)
                self._hermes_placed.append(target)
            self.journal("done", "c3", {"placed": [str(p) for p in self._hermes_placed]})

        def inverse() -> None:
            for path in reversed(self._hermes_placed):
                with contextlib.suppress(FileNotFoundError):
                    os.unlink(path)
            self._hermes_placed.clear()
            self._remove_dirs(self._hermes_dirs)
            self._hermes_dirs.clear()
            self.journal("done", "c3-inverse", {})

        return forward, inverse

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
                }
            )
        return {
            "v": 1,
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
        """A transaction rebuilt from ``snapshot()`` in another process (same staging directory)."""
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
        for sub in ("quarantine", "git-tmp", "tree-backup"):
            shutil.rmtree(self.staging / sub, ignore_errors=True)
        self.journal("done", "discard", {})

    def dests(self) -> list[tuple[str, str, DestMode]]:
        """``(item_id, destination, mode)`` per prepared item (the final path after commit)."""
        return [(p.item_id, str(p.final or p.dest), p.mode) for p in self.items]

    def map_states(self) -> dict[str, dict]:
        """Per item the state to record in the ``in`` workspace-map row after commit (git: the received
        ``GitState`` fields; tree: the fingerprint)."""
        out: dict[str, dict] = {}
        for p in self.items:
            if p.kind == "git":
                pi = p.pack_item
                out[p.item_id] = {
                    "head": (pi.get("head") or {}).get("commit") or "",
                    "index_tree": pi.get("index_tree"),
                    "worktree_tree": pi.get("worktree_tree"),
                    "branch": (pi.get("head") or {}).get("ref"),
                    "exclude_pathspecs": list(pi.get("exclude_pathspecs") or []),
                }
            elif p.kind == "tree":
                out[p.item_id] = dict(p.state.fingerprint)
        return out
