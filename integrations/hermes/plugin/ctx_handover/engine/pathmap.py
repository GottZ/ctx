"""Target path mapping (layer L4, stdlib only; DESIGN §9.7, §10.4, R4, R19).

Deterministic, on the target. First matching rule wins:

1. an explicit ``--dest`` (the primary workspace) or ``--map SRC=DST`` / ``dest_overrides`` — typed on THIS host
   (``local_choice``); typed on the counterpart, the same values are only proposals (rule 3 semantics);
2. the **return mapping**: a row of this host's workspace map for the job's source peer whose ``remote_path`` is the
   item's ``source_path`` (``out`` rows first — the return trip, R4 — then ``in`` rows — a repeat in the same
   direction). It may leave ``allow_roots``: this host recorded it itself;
3. the source's ``suggested_dest`` — only under ``allow_roots``;
4. the same path (same OS family and home) — only under ``allow_roots``;
5. the home-relative path — only under ``allow_roots``;
6. ``<workspace_root>/<basename>``.

Extras (single files and directories outside the workspace) skip rules 3–5: they land at their mapped path only
through the return mapping or a local ``--map``, otherwise under ``<workspace_root>/_handover/<job6>/<home-relative
path>`` (DESIGN §10.4). Session files keep their place in the target's Hermes home (cluster B places them).

Every proposal from the counterpart is normalised, must be an absolute path of the target's OS (no UNC/device
prefix) and lie under ``allow_roots``; with ``cfg`` (this host's policy) it must also pass ``check_target_dest`` —
the target denylist applies inside ``allow_roots`` too. ``materialize.assess`` checks the final destination of
every rule again (``recorded`` for ``return-mapping``, ``local_choice`` for ``user``).
"""

from __future__ import annotations

import ntpath
import posixpath
import re
import unicodedata
from collections.abc import Callable, Sequence
from datetime import datetime
from pathlib import Path

from .errors import PolicyDenied
from .ids import short
from .model import EnvFacts, ItemKind, ItemRole, MapRow, Options, PlanItem
from .paths import check_rel, home_relative, reject_windows_device_prefix, rules_for
from .policy import PolicyConfig, check_target_dest

USER = "user"
RETURN = "return-mapping"
SUGGESTED = "suggested"
SAME_PATH = "same-path"
HOME_RELATIVE = "home-relative"
WORKSPACE_ROOT = "workspace-root"
HERMES = "hermes"
GENERATED = frozenset({SUGGESTED, SAME_PATH, HOME_RELATIVE, WORKSPACE_ROOT})

_DRIVE_RE = re.compile(r"^[A-Za-z]:[\\/]")
_EXTRA_KINDS = frozenset({ItemKind.EXTRA_FILE, ItemKind.EXTRA_DIR})


def _windows_family(facts: EnvFacts) -> bool:
    return rules_for(facts.os_family, False).windows


def _spelled_windows(path: str) -> bool:
    return bool(_DRIVE_RE.match(path or ""))


class _Target:
    def __init__(self, facts: EnvFacts, cfg: PolicyConfig | None) -> None:
        self.facts = facts
        self.cfg = cfg
        self.windows = _windows_family(facts)
        self.mod = ntpath if self.windows else posixpath
        self.ci = self.windows or any(bool(v) for v in (facts.case_insensitive or {}).values())
        self.rules = rules_for(facts.os_family, self.ci)
        self.roots = [self.mod.normpath(r) for r in facts.allow_roots if isinstance(r, str) and r]

    def parts(self, path: str) -> tuple[str, ...]:
        if self.windows:
            drive, rest = ntpath.splitdrive(path)
            comps = (drive.upper(), *[c for c in rest.replace("/", "\\").split("\\") if c])
        else:
            comps = tuple(c for c in path.split("/") if c)
        if self.ci:
            return tuple(unicodedata.normalize("NFC", unicodedata.normalize("NFC", c).casefold()) for c in comps)
        return tuple(unicodedata.normalize("NFC", c) for c in comps)

    def under(self, path: str, root: str) -> bool:
        p, r = self.parts(path), self.parts(root)
        return len(p) >= len(r) and p[: len(r)] == r

    def valid(self, path: object) -> str | None:
        """Normalised absolute path of this target's OS, or None."""
        if not isinstance(path, str) or not path or "\x00" in path or len(path) > 4096:
            return None
        try:
            reject_windows_device_prefix(path)
        except PolicyDenied:
            return None
        if self.windows:
            if not _spelled_windows(path):
                return None
        elif not path.startswith("/"):
            return None
        return self.mod.normpath(path)

    def allowed(self, path: object) -> str | None:
        """A counterpart's proposal: valid, under ``allow_roots`` and (with ``cfg``) not on the target denylist."""
        dest = self.valid(path)
        if dest is None or not any(self.under(dest, r) for r in self.roots):
            return None
        if (
            self.cfg is not None
            and not check_target_dest(Path(dest), self.cfg, recorded=False, local_choice=False).allowed
        ):
            return None
        return dest

    def join(self, base: str, rel_parts: Sequence[str]) -> str:
        return self.mod.join(base, *rel_parts) if rel_parts else self.mod.normpath(base)

    def safe_component(self, name: str, fallback: str) -> str:
        try:
            return check_rel(name, self.rules) if "/" not in name else fallback
        except PolicyDenied:
            cleaned = re.sub(r'[<>:"|?*\\/\x00-\x1f]', "_", name).rstrip(". ")
            try:
                return check_rel(cleaned, self.rules)
            except PolicyDenied:
                return fallback


def _source_mod(path: str):
    return ntpath if _spelled_windows(path) else posixpath


def _source_parts(path: str) -> list[str]:
    if _spelled_windows(path):
        drive, rest = ntpath.splitdrive(path)
        return [drive.rstrip(":").upper(), *[c for c in rest.replace("/", "\\").split("\\") if c]]
    return [c for c in path.split("/") if c]


def _rel_under(path: str, root: str) -> list[str] | None:
    """Components of ``path`` below ``root`` (source spelling; Windows case-insensitive), or None."""
    p, r = _source_parts(path), _source_parts(root)
    fold = _spelled_windows(path)
    pk = [c.casefold() for c in p] if fold else p
    rk = [c.casefold() for c in r] if fold else r
    if len(pk) < len(rk) or pk[: len(rk)] != rk or ".." in p:
        return None
    return p[len(r) :]


def _explicit(item: PlanItem, options: Options) -> str | None:
    if options.dest and item.role is ItemRole.PRIMARY and item.kind not in _EXTRA_KINDS:
        return options.dest
    best: tuple[int, str] | None = None
    for src, dst in (*options.maps, *options.hints.dest_overrides):
        if not isinstance(src, str) or not isinstance(dst, str) or not src or not dst:
            continue
        rel = _rel_under(item.source_path, src)
        if rel is None:
            continue
        mod = ntpath if _spelled_windows(dst) else posixpath
        candidate = mod.join(dst, *rel) if rel else dst
        depth = len(_source_parts(src))
        if best is None or depth > best[0]:
            best = (depth, candidate)
    return best[1] if best else None


def _extra_dest(item: PlanItem, t: _Target, source_facts: EnvFacts, job_id: str) -> str:
    rel = home_relative(item.source_path, source_facts.home)
    parts = [c for c in rel.split("/") if c] if rel else ["_abs", *_source_parts(item.source_path)]
    safe = [t.safe_component(c, f"_{i}") for i, c in enumerate(parts)]
    job6 = short(job_id) if job_id else "job"
    return t.join(t.mod.normpath(t.facts.workspace_root), ["_handover", t.safe_component(job6, "job"), *safe])


def choose_dest(
    item: PlanItem,
    *,
    source_facts: EnvFacts,
    target_facts: EnvFacts,
    lookup: Callable[[str, str], MapRow | None],
    options: Options,
    local_choice: bool = True,
    job_id: str = "",
    cfg: PolicyConfig | None = None,
) -> tuple[str, str]:
    """``(dest, reason)`` for one item (module docstring). ``lookup(remote_path, direction)`` returns this host's
    workspace-map row for the job's source peer (the caller binds the peer). Additive keywords over INTERFACES
    §4.2: ``local_choice`` (False when the job's options were set by the counterpart), ``job_id`` (extras'
    ``_handover/<job6>`` directory), ``cfg`` (this host's policy for the denylist check of proposals)."""
    t = _Target(target_facts, cfg)
    if item.kind is ItemKind.HERMES_FILE:
        return target_facts.host.hermes_home, HERMES
    extra = item.kind in _EXTRA_KINDS
    explicit = _explicit(item, options)
    if explicit is not None:
        if local_choice:
            dest = t.valid(explicit)
            if dest is not None:
                return dest, USER
        elif not extra:
            dest = t.allowed(explicit)
            if dest is not None:
                return dest, SUGGESTED
    for direction in ("out", "in"):
        try:
            row = lookup(item.source_path, direction)
        except Exception:  # a broken lookup is a missing mapping, never a reason to stop
            row = None
        dest = t.valid(row.local_path) if row is not None else None
        if dest is not None:
            return dest, RETURN
    if extra:
        return _extra_dest(item, t, source_facts, job_id), WORKSPACE_ROOT
    dest = t.allowed(item.suggested_dest) if item.suggested_dest else None
    if dest is not None:
        return dest, SUGGESTED
    same_family = _windows_family(source_facts) == t.windows and source_facts.os_family == target_facts.os_family
    same_home = (
        same_family
        and t.valid(source_facts.home) is not None
        and t.valid(target_facts.home) is not None
        and t.parts(t.mod.normpath(source_facts.home)) == t.parts(t.mod.normpath(target_facts.home))
    )
    dest = t.allowed(item.source_path) if same_home else None
    if dest is not None:
        return dest, SAME_PATH
    rel = home_relative(item.source_path, source_facts.home)
    if rel:
        base = t.valid(target_facts.home)
        if base is not None:
            parts = [t.safe_component(c, f"_{i}") for i, c in enumerate(rel.split("/"))]
            dest = t.allowed(t.join(base, parts))
            if dest is not None:
                return dest, HOME_RELATIVE
    parts = _source_parts(item.source_path)
    name = t.safe_component(parts[-1] if parts else "", f"item-{item.item_id}") if parts else f"item-{item.item_id}"
    return t.join(t.mod.normpath(target_facts.workspace_root), [name]), WORKSPACE_ROOT


def choose_all(
    items: Sequence[PlanItem],
    *,
    source_facts: EnvFacts,
    target_facts: EnvFacts,
    lookup: Callable[[str, str], MapRow | None],
    options: Options,
    local_choice: bool = True,
    job_id: str = "",
    cfg: PolicyConfig | None = None,
) -> list[tuple[str, str]]:
    """``choose_dest`` for every item; two workspace items that a generated rule (3–6) maps to the same place get
    ``-2``, ``-3`` … suffixes in item order (explicit and recorded destinations are left to the conflict check)."""
    t = _Target(target_facts, cfg)
    out = [
        choose_dest(
            i,
            source_facts=source_facts,
            target_facts=target_facts,
            lookup=lookup,
            options=options,
            local_choice=local_choice,
            job_id=job_id,
            cfg=cfg,
        )
        for i in items
    ]
    taken = {t.parts(d) for (d, r), i in zip(out, items, strict=True) if r not in GENERATED or i.kind in _EXTRA_KINDS}
    result = []
    for (dest, reason), item in zip(out, items, strict=True):
        if reason in GENERATED and item.kind in {ItemKind.GIT_REPO, ItemKind.FILE_TREE}:
            candidate, n = dest, 2
            while t.parts(candidate) in taken:
                candidate = f"{dest}-{n}"
                n += 1
            dest = candidate
            taken.add(t.parts(dest))
        result.append((dest, reason))
    return result


def fallback_dest(dest: str, *, now: datetime, workspace_root: str, allow_roots: Sequence[str]) -> str:
    """Where an item goes when its first choice conflicts (DESIGN §9.7): ``<dest>-handover-<YYYYmmdd-HHMMSS>`` when
    the parent lies under ``allow_roots``, else the same name under ``workspace_root``."""
    mod = ntpath if _spelled_windows(dest) else posixpath
    ci = mod is ntpath
    stamp = now.strftime("%Y%m%d-%H%M%S")
    norm = mod.normpath(dest)
    parent = mod.dirname(norm)

    def parts(p: str) -> list[str]:
        out = [c for c in mod.normpath(p).replace("\\", "/").split("/") if c]
        return [c.casefold() for c in out] if ci else out

    if any(parts(parent)[: len(parts(r))] == parts(r) for r in allow_roots if r):
        return f"{norm}-handover-{stamp}"
    return mod.join(mod.normpath(workspace_root), f"{mod.basename(norm) or 'workspace'}-handover-{stamp}")


def map_cwd(source_cwd: str, item: PlanItem, dest: str) -> str:
    """The session ``cwd`` on the target: the same position relative to the item root (``rel_cwd`` from the plan,
    else ``source_cwd`` below ``source_path``); anything unsafe or outside falls back to ``dest``."""
    windows = _spelled_windows(dest)
    mod = ntpath if windows else posixpath
    rules = rules_for("windows" if windows else "linux", windows)
    rel: list[str] | None = None
    if item.rel_cwd:
        try:
            rel = check_rel(item.rel_cwd, rules).split("/")
        except PolicyDenied:
            return dest
    elif isinstance(source_cwd, str) and source_cwd:
        rel = _rel_under(
            _source_mod(source_cwd).normpath(source_cwd), _source_mod(item.source_path).normpath(item.source_path)
        )
        if rel:
            try:
                rel = check_rel("/".join(rel), rules).split("/")
            except PolicyDenied:
                rel = None
    return mod.join(dest, *rel) if rel else dest
