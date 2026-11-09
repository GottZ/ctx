"""Resource plan, merge and release rules, confirmation and pre-send summary (layer L5, stdlib only;
DESIGN §8.4–§8.7, §14.1, R18, SEC 1.8, 4.3, 5.6, 5.8).

``build_plan`` turns the collector's candidates into the source's plan document. Every path — collector evidence,
model ``add``, agent-tool resource, user ``--include`` — goes through the same K0 policy (``check_source_file`` /
``check_extra``); a refused path is named in ``not_transferred`` and never becomes an item.

Merge rules (DESIGN §8.4): model output and agent suggestions are advisory. A model ``add`` must exist, pass the
policy and stays *proposed* (``refine_auto_include`` promotes clean ones); ``drop`` only touches transcript
candidates below *write*, never the workspace or user items. Agent resources inside a workspace root are covered,
outside they are proposals — whatever the settings say (BX L6, SEC 5.6). Tool and env-var names are schema-checked;
free text (model notes, agent note) stays free text in ``notes_for_target`` and reaches the target only inside the
note's data block (R22).

Release rules (R18, I11): ``restrict_to_releasable`` reduces a remote conductor's FinalPlan to the source's own
plan plus paths under the operator's ``export_roots``; the counterpart can narrow, never widen, and its
``include_secret`` is ignored (with a warning). ``build_plan(remote_driven=True)`` ignores the counterpart's include
hints for the same reason.

Excluded working directory (R30): when the policy leaves the session's working directory behind (the primary item
is ``no`` and named in ``not_transferred`` — e.g. it lies in the Hermes home), ``presend_summary`` says so
(``workspace_excluded``) and ``confirmation_reasons`` raises ``workspace_excluded`` (letter h), which a blanket yes
never confirms (``YES_EXEMPT``/``yes_exempt``); the user's own ``--no-workspace`` is the explicit answer.

Suspected secrets (R25): an extra file the policy suspects (or could not check, no redactor) carries the exact reason
element ``SUSPECTED_SECRET``; ``apply_choice(final, "yes")`` never confirms it and ``restrict_to_releasable`` never
lets a counterpart confirm it. Only a local ``include_secret`` (``yes`` already in the plan) releases it.

Plan items carry their facts in fields, not in reason texts (contract K-3): ``PlanItem.hermes_kind`` (session file
kind), ``PlanItem.instruction_files`` (``(rel, sha256)`` of the instruction files inside the item) and
``ResourcePlan.warnings`` (the busy probe's warnings). Reason texts are for people.
"""

from __future__ import annotations

import fnmatch
import hashlib
import os
import re
import shutil
import stat
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass, field, replace
from pathlib import Path

from . import collector, note, refine
from .contracts import BusyReportLike
from .errors import InsufficientStorage, PolicyDenied
from .ignore import IgnoreMatcher
from .jsonutil import doc_id, to_wire
from .model import (
    Candidate,
    DestMode,
    EnvFacts,
    FinalPlan,
    GitInfo,
    Include,
    ItemKind,
    ItemRole,
    NotTransferred,
    Options,
    Origin,
    PlanHints,
    PlanItem,
    Refinement,
    RefineReport,
    ResourcePlan,
    TargetDecision,
    Weight,
)
from .paths import PlatformRules, check_rel, under_any
from .policy import (
    PolicyConfig,
    Verdict,
    check_extra,
    check_source_file,
    is_denied_name,
    is_hermes_protected,
)

# ---------------------------------------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------------------------------------

CONFIRM_PROPOSED = "proposed"
CONFIRM_CONFLICT = "conflict"
CONFIRM_LARGE = "large"
CONFIRM_SECRET = "include_secret"  # noqa: S105 - reason code, not a secret
CONFIRM_MODEL = "model"
CONFIRM_OUTSIDE = "outside_workspace"
CONFIRM_INSTRUCTIONS = "instruction_files"
CONFIRM_WORKSPACE_EXCLUDED = "workspace_excluded"
CONFIRM_LETTERS: dict[str, str] = {
    CONFIRM_PROPOSED: "a",
    CONFIRM_CONFLICT: "b",
    CONFIRM_LARGE: "c",
    CONFIRM_SECRET: "d",
    CONFIRM_MODEL: "e",
    CONFIRM_OUTSIDE: "f",
    CONFIRM_INSTRUCTIONS: "g",
    CONFIRM_WORKSPACE_EXCLUDED: "h",
}
"""Confirmation reasons of DESIGN §14.1 (a)–(g) plus (h) ``workspace_excluded`` (R30) and their letters. ``None``
(no answer) cancels the job for every reason except ``proposed`` alone, which only excludes the proposals (BX L5)."""
YES_EXEMPT: frozenset[str] = frozenset({CONFIRM_WORKSPACE_EXCLUDED})
"""Reasons a blanket yes (``--yes``, ``Options.yes``) never confirms (R30, like suspected secrets under R25): the
conductor asks for them even with ``--yes``; without an answer the job ends with nothing locked."""

CHOICES = ("yes", "no_proposed")

SUSPECTED_SECRET = "suspected_secret"  # noqa: S105 - reason marker, not a secret
"""Exact ``PlanItem.reasons`` element of an item the policy suspects to hold a secret, or could not check (R25,
X05). ``apply_choice(final, "yes")`` turns such a proposal into ``no``."""
UNVERIFIED = "unverified"
"""Origin marker of a handover from a source this side cannot verify — an unsigned bundle, one signed by an unknown
or revoked key, or the remote source of a pull (DESIGN §8.7 reason (g), SEC 4.3; a continuation refuses such an
origin with this reason, R22). Surfaces import it with the reason codes (X03)."""
REASON_CODES: tuple[str, ...] = tuple(CONFIRM_LETTERS)
"""Every confirmation reason code, in letter order (a)–(h) (X03)."""
MAX_REFUSED_IDS = 256
MAX_WARNINGS = 32

WHY_OUTSIDE_HOME = "outside home"
WHY_SECRET = "secret"  # noqa: S105 - reason text, not a secret
WHY_TOO_LARGE = "too large"
WHY_HOME_WORKSPACE = "home directory is not transferred as a workspace (use --include for the files you need)"
WHY_HOME = "home directory is not transferred as a whole"
WHY_CONTAINS_WORKSPACE = "contains the workspace"
WHY_DENIED_SUGGESTION = "suggested but denied by policy"
WHY_PEER_SECRET = "include_secret from peer ignored"  # noqa: S105 - reason text, not a secret
NON_CANONICAL_PATH = "non_canonical_path"
"""``PolicyDenied`` reason of a remote addition whose path is not its realpath (a link on the way; V29)."""
_WHY_BY_REASON = {
    "secret": WHY_SECRET,
    "hermes": "hermes",
    "size": WHY_TOO_LARGE,
    "outside_home": WHY_OUTSIDE_HOME,
    "special": "special file",
    "symlink": "symlink leaves the item",
    "missing": "missing",
    "unreadable": "unreadable",
    "suspected_secret": "suspected secret",
}


CHAIN_NAMES = (".hermes.md", "HERMES.md", "AGENTS.override.md", "AGENTS.md", "agents.md")
"""Instruction files Hermes reads in every directory from the git root down to the cwd
(``agent/prompt_builder.py:139-148,1580-1615``)."""

CWD_NAMES = ("CLAUDE.md", "claude.md", ".cursorrules")
"""Instruction files Hermes reads only in the cwd (plus ``.cursor/rules/*.mdc``, ``prompt_builder.py:1618-1640``)."""

MAX_INSTRUCTION_BYTES = 1024 * 1024
MAX_REASONS = 32
MAX_NOTES = 1500
_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
_TOOL_RE = re.compile(refine.TOOL_PATTERN)
_ENV_RE = re.compile(refine.ENV_PATTERN)
_RANK = {Weight.MENTION: 0, Weight.READ: 1, Weight.WRITE: 2}
_POSIX_RULES = PlatformRules(windows=False, case_insensitive=False)


# ---------------------------------------------------------------------------------------------------------
# Draft items
# ---------------------------------------------------------------------------------------------------------


@dataclass
class _Draft:
    kind: ItemKind
    path: str
    role: ItemRole
    origin: Origin
    include: Include
    weight: Weight = Weight.READ
    reasons: list[str] = field(default_factory=list)
    explicit: bool = False
    suspected: bool = False
    git: GitInfo | None = None
    rel_cwd: str | None = None
    size: int = 0
    item_id: str = ""
    instructions: list[tuple[str, str]] = field(default_factory=list)
    candidate: int | None = None
    denied: bool = False
    hermes_kind: str | None = None


@dataclass(frozen=True)
class _Busy:
    processes: tuple[dict, ...] = ()
    delegations: tuple[dict, ...] = ()
    cron: tuple[dict, ...] = ()
    warnings: tuple[str, ...] = ()


def _norm(path: str) -> str:
    return os.path.normpath(path)


def _real(path: str) -> str:
    try:
        return os.path.normpath(os.path.realpath(path))
    except (OSError, ValueError):
        return os.path.normpath(path)


def _under(path: str, root: str, ci: bool) -> bool:
    return under_any(Path(path), [Path(root)], case_insensitive=ci)


def _why(verdict: Verdict) -> str:
    return _WHY_BY_REASON.get(str(verdict.reason), str(verdict.reason))


def _clean(text: object, cap: int = 120) -> str:
    out = "".join(ch if ch.isprintable() else " " for ch in str(text or ""))
    return " ".join(out.split())[:cap]


class _Builder:
    def __init__(
        self,
        cfg: PolicyConfig,
        options: Options,
        git_info: Callable[[str], GitInfo | None],
        redact: Callable[[str], str] | None,
    ) -> None:
        self.cfg, self.options, self.git_info, self.redact = cfg, options, git_info, redact
        self.ci = cfg.case_insensitive
        self.drafts: list[_Draft] = []
        self.by_path: dict[str, _Draft] = {}
        self.not_transferred: list[NotTransferred] = []
        self.home = _real(str(cfg.home))

    # -- bookkeeping -------------------------------------------------------------------------------------
    def refuse(self, path: str, why: str) -> None:
        entry = NotTransferred(path=path, why=why)
        if entry not in self.not_transferred:
            self.not_transferred.append(entry)

    def add(self, draft: _Draft) -> _Draft:
        self.drafts.append(draft)
        self.by_path[draft.path] = draft
        return draft

    def primary(self) -> _Draft | None:
        return next((d for d in self.drafts if d.role is ItemRole.PRIMARY), None)

    def covering(self, real: str) -> _Draft | None:
        """The included tree item (primary, secondary, extra dir) whose root contains ``real``."""
        best = None
        for d in self.drafts:
            if d.kind not in {ItemKind.GIT_REPO, ItemKind.FILE_TREE, ItemKind.EXTRA_DIR} or d.include is Include.NO:
                continue
            if d.role is ItemRole.PRIMARY and self.home_like(d.path):
                continue
            if _under(real, d.path, self.ci) and (best is None or len(d.path) > len(best.path)):
                best = d
        return best

    def contains_tree(self, real: str) -> bool:
        """``real`` is an ancestor of a workspace item (copying it would duplicate the workspace)."""
        return any(
            d.kind in {ItemKind.GIT_REPO, ItemKind.FILE_TREE} and d.path != real and _under(d.path, real, self.ci)
            for d in self.drafts
            if not d.denied
        )

    def home_like(self, path: str) -> bool:
        real = _real(path)
        return real == os.path.dirname(real) or _under(self.home, real, self.ci)

    def default_include(self, weight: Weight) -> Include:
        mode = self.options.extra_paths
        if mode == "off":
            return Include.NO
        if mode == "auto" and _RANK[weight] >= _RANK[Weight.READ]:
            return Include.YES
        return Include.PROPOSED

    # -- policy ------------------------------------------------------------------------------------------
    def check_extra_path(self, path: str, *, explicit: bool, given: str | None = None) -> tuple[Verdict, bool]:
        """(verdict, suspected) for an extra file or directory outside every workspace. ``path`` is checked as
        given and as realpath by the policy; ``given`` (the spelling a model or agent used, possibly through a
        link) must also pass the name/directory denylist (SEC 5.3)."""
        p = Path(path)
        lifted = {os.path.normpath(path), os.path.normpath(given or path)} & self.cfg.include_secret
        spellings = {path, given} if given else {path}
        if not lifted and any(is_denied_name(s, case_insensitive=self.ci) for s in spellings):
            return Verdict(False, "secret"), False
        try:
            is_dir = stat.S_ISDIR(os.stat(p).st_mode)
        except OSError:
            is_dir = False
        if is_dir:
            return check_source_file(p, self.cfg, explicit=explicit), False
        if self.redact is None:
            verdict = check_source_file(p, self.cfg, explicit=explicit)
            return verdict, verdict.allowed and not lifted
        verdict = check_extra(p, self.cfg, explicit=explicit, redact=self.redact)
        return verdict, verdict.reason == "suspected_secret"

    def deny(self, d: _Draft, why: str) -> None:
        """Policy refusal: the draft leaves the plan (only the primary stays, as ``no``) and is named."""
        d.include = Include.NO
        d.denied = True
        self.refuse(d.path, why)

    def over_limit(self, d: _Draft) -> None:
        """Size refusal: the item stays visible as ``no`` (``--include`` lifts the limit) and is named."""
        d.include = Include.NO
        d.reasons.append(f"over the size limit ({WHY_TOO_LARGE})")
        self.refuse(d.path, WHY_TOO_LARGE)

    def evaluate(self, d: _Draft) -> None:
        """Apply the policy to one draft; a refusal sets ``no`` and names it."""
        if d.kind is ItemKind.HERMES_FILE:
            self._evaluate_hermes(d)
            return
        if d.role is ItemRole.PRIMARY and self.home_like(d.path):
            self.deny(d, WHY_HOME_WORKSPACE)
            return
        if d.kind in {ItemKind.GIT_REPO, ItemKind.FILE_TREE}:
            item_root = Path(d.path) if d.role is ItemRole.PRIMARY else None
            verdict = check_source_file(Path(d.path), self.cfg, explicit=True, item_root=item_root)
            suspected = False
        else:
            verdict, suspected = self.check_extra_path(d.path, explicit=d.explicit)
        if not verdict.allowed:
            if verdict.reason == "size":
                self.over_limit(d)
            else:
                self.deny(d, _why(verdict))
            return
        if suspected:
            d.suspected = True
            label = "suspected secret: proposed only" if self.redact else "not checked for tokens: proposed only"
            d.reasons.append(label)
            if d.include is Include.YES and not d.explicit:
                d.include = Include.PROPOSED

    def _evaluate_hermes(self, d: _Draft) -> None:
        rejected = collector.rejected_reason(d.reasons)
        if rejected is not None:  # refused by the session-file discovery (W040): named, never an item
            self.deny(d, _WHY_BY_REASON.get(rejected, "hermes"))
            return
        kind = d.hermes_kind
        p = Path(d.path)
        try:
            st = os.lstat(p)
        except OSError:
            self.deny(d, "missing")
            return
        if kind is None or is_hermes_protected(p, self.cfg, session_file_kind=kind) or not stat.S_ISREG(st.st_mode):
            self.deny(d, "hermes")
            return
        d.size = st.st_size
        if st.st_size > self.cfg.max_hermes_file_bytes:
            self.over_limit(d)

    # -- merges ------------------------------------------------------------------------------------------
    def suggested_path(self, raw: object, origin: Origin, reason: str, *, explicit: bool = False) -> _Draft | None:
        """Model, agent or user path: absolute, existing, covered or policy-checked. Returns the new draft or None."""
        label = {Origin.MODEL: "model", Origin.AGENT: "agent", Origin.USER: "user"}[origin]
        if not isinstance(raw, str) or not raw or "\x00" in raw or not os.path.isabs(raw):
            return None
        given = _norm(raw)
        try:
            os.lstat(given)
        except OSError:
            self.refuse(given, "missing" if origin is Origin.USER else f"{WHY_DENIED_SUGGESTION} (missing)")
            return None
        real = _real(given)
        existing = self.by_path.get(real) or self.by_path.get(given)
        if existing is not None:
            existing.reasons.append(f"{label}: {_clean(reason)}" if reason else label)
            if origin is Origin.USER and not existing.denied:
                existing.include = Include.YES
            return None
        owner = self.covering(real)
        if owner is not None:
            rel = os.path.relpath(real, owner.path) if real != owner.path else "."
            owner.reasons.append(f"{label}: {rel}")
            return None
        if self.home_like(real) or self.contains_tree(real):
            why = WHY_HOME if self.home_like(real) else WHY_CONTAINS_WORKSPACE
            self.refuse(given, why if origin is Origin.USER else f"{WHY_DENIED_SUGGESTION} ({why})")
            return None
        if explicit:
            verdict, suspected = self.check_extra_path(given, explicit=True)
        else:
            verdict, suspected = self.check_extra_path(real, explicit=False, given=given)
        if not verdict.allowed:
            why = _why(verdict)
            self.refuse(given, why if origin is Origin.USER else f"{WHY_DENIED_SUGGESTION} ({why})")
            return None
        is_dir = os.path.isdir(real)
        auto = origin is Origin.MODEL and self.options.refine_auto_include and not suspected
        include = Include.YES if origin is Origin.USER or auto else Include.PROPOSED
        reasons = [f"{label}: {_clean(reason)}" if reason else label]
        if suspected:
            reasons.append(
                "suspected secret: proposed only" if self.redact else "not checked for tokens: proposed only"
            )
        return self.add(
            _Draft(
                kind=ItemKind.EXTRA_DIR if is_dir else ItemKind.EXTRA_FILE,
                path=real,
                role=ItemRole.EXTRA,
                origin=origin,
                include=include,
                reasons=reasons,
                explicit=explicit,
                suspected=suspected,
            )
        )

    def drop(self, drops: Sequence[tuple[str, str]], cands: Sequence[Candidate]) -> int:
        dropped = 0
        for cid, reason in drops:
            if not (isinstance(cid, str) and cid.startswith("c") and cid[1:].isdigit()):
                continue
            idx = int(cid[1:])
            if idx >= len(cands):
                continue
            cand = cands[idx]
            if cand.origin is not Origin.TRANSCRIPT or _RANK[cand.weight] >= _RANK[Weight.WRITE]:
                continue
            draft = next((d for d in self.drafts if d.candidate == idx), None)
            if draft is None or draft.role is ItemRole.PRIMARY or draft.include is Include.NO:
                continue
            draft.include = Include.NO
            draft.reasons.append(f"dropped by model: {_clean(reason)}" if reason else "dropped by model")
            dropped += 1
        return dropped

    def exclude(self, patterns: Iterable[str], label: str) -> None:
        for pattern in patterns:
            if not isinstance(pattern, str) or not pattern:
                continue
            if pattern == "~" or pattern.startswith("~/"):
                pattern = str(self.cfg.home) + pattern[1:]
            for d in self.drafts:
                if d.role is ItemRole.PRIMARY or d.origin is Origin.USER:
                    continue
                matches = d.path == _norm(pattern) or fnmatch.fnmatchcase(d.path, pattern)
                if matches and d.include is not Include.NO:
                    d.include = Include.NO
                    d.reasons.append(label)

    # -- sizes, git, instruction files -------------------------------------------------------------------
    def finish(self, d: _Draft) -> None:
        if d.include is Include.NO:
            return
        if d.kind is ItemKind.GIT_REPO:
            try:
                d.git = self.git_info(d.path)
            except Exception as exc:  # the pack step re-reads the repository with git itself
                d.reasons.append(f"git info unavailable ({type(exc).__name__})")
                d.git = None
            objects, _complete = _tree_size(Path(d.path) / ".git" / "objects", None, self.cfg.max_total_bytes, 200_000)
            d.size = objects + (d.git.untracked_bytes if d.git else 0)
            if d.git is not None:
                if d.git.untracked_count > self.cfg.max_untracked_files:
                    d.reasons.append(
                        f"{d.git.untracked_count} untracked files (limit {self.cfg.max_untracked_files}): proposed"
                    )
                    d.include = Include.PROPOSED
                if d.git.in_progress:
                    d.reasons.append(f"git operation in progress: {_clean(d.git.in_progress, 40)}")
        elif d.kind in {ItemKind.FILE_TREE, ItemKind.EXTRA_DIR}:
            cap = self.cfg.max_total_bytes
            if d.kind is ItemKind.EXTRA_DIR and not d.explicit:
                cap = self.cfg.max_extra_dir_bytes
            try:
                matcher = IgnoreMatcher.for_tree(Path(d.path), self.options.extra_ignore)
            except Exception:
                matcher = None
            size, complete = _tree_size(Path(d.path), matcher, cap, self.cfg.max_files)
            d.size = size
            if not complete and d.kind is ItemKind.EXTRA_DIR:
                self.over_limit(d)
        elif d.kind is ItemKind.EXTRA_FILE:
            try:
                d.size = os.lstat(d.path).st_size
            except OSError:
                d.size = 0
        if d.kind in {ItemKind.GIT_REPO, ItemKind.FILE_TREE} and d.include is not Include.NO:
            d.instructions = _scan_instructions(d.path, d.rel_cwd)


def _tree_size(root: Path, matcher: IgnoreMatcher | None, cap_bytes: int, max_entries: int) -> tuple[int, bool]:
    """Bytes of regular files below ``root`` (sorted walk, links not followed, ignored entries skipped);
    ``complete`` is False when ``cap_bytes`` or ``max_entries`` was exceeded."""
    total, entries = 0, 0
    stack = [("", root)]
    while stack:
        rel, directory = stack.pop()
        try:
            with os.scandir(directory) as it:
                children = sorted(it, key=lambda e: e.name)
        except OSError:
            continue
        for entry in reversed(children):
            child_rel = f"{rel}/{entry.name}" if rel else entry.name
            try:
                is_dir = entry.is_dir(follow_symlinks=False)
                is_file = entry.is_file(follow_symlinks=False)
            except OSError:
                continue
            if matcher is not None and matcher.ignored(child_rel, is_dir):
                continue
            entries += 1
            if entries > max_entries:
                return total, False
            if is_dir:
                stack.append((child_rel, Path(entry.path)))
            elif is_file:
                try:
                    total += entry.stat(follow_symlinks=False).st_size
                except OSError:
                    continue
                if total > cap_bytes:
                    return total, False
    return total, True


def _sha256_file(path: Path) -> str | None:
    try:
        st = os.lstat(path)
    except OSError:
        return None
    if not stat.S_ISREG(st.st_mode) or st.st_size > MAX_INSTRUCTION_BYTES:
        return None
    try:
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError:
        return None


def _scan_instructions(root: str, rel_cwd: str | None) -> list[tuple[str, str]]:
    """``(rel, sha256)`` of the instruction files Hermes would load for a cwd inside ``root`` (chain root → cwd,
    cwd-only names)."""
    base = Path(root)
    chain = [""]
    if rel_cwd:
        parts = [p for p in rel_cwd.split("/") if p and p not in {".", ".."}]
        for i in range(1, len(parts) + 1):
            chain.append("/".join(parts[:i]))
    found: list[tuple[str, str]] = []

    def add(rel: str) -> None:
        digest = _sha256_file(base / rel)
        if digest is not None:
            found.append((rel, digest))

    for directory in chain:
        for name in CHAIN_NAMES:
            add(f"{directory}/{name}" if directory else name)
    cwd = chain[-1]
    for name in CWD_NAMES:
        add(f"{cwd}/{name}" if cwd else name)
    rules = base / cwd / ".cursor" / "rules" if cwd else base / ".cursor" / "rules"
    try:
        mdc = sorted(p.name for p in rules.iterdir() if p.name.endswith(".mdc"))
    except OSError:
        mdc = []
    for name in mdc[:64]:
        add(f"{cwd}/.cursor/rules/{name}" if cwd else f".cursor/rules/{name}")
    return found


def _assign_ids(drafts: Sequence[_Draft], taken: Iterable[str] = ()) -> None:
    used = set(taken)
    counters: dict[str, int] = {}
    for d in drafts:
        if d.item_id:
            used.add(d.item_id)
    for d in drafts:
        if d.item_id:
            continue
        prefix = {ItemRole.PRIMARY: "w", ItemRole.SECONDARY: "w", ItemRole.EXTRA: "x", ItemRole.HERMES: "h"}[d.role]
        n = counters.get(prefix, 0)
        while f"{prefix}{n}" in used:
            n += 1
        d.item_id = f"{prefix}{n}"
        used.add(d.item_id)
        counters[prefix] = n + 1


def _to_item(d: _Draft, suggest_dest: Callable[[str], str | None] | None) -> PlanItem:
    marker = [SUSPECTED_SECRET] if d.suspected or SUSPECTED_SECRET in d.reasons else []
    reasons = list(dict.fromkeys(r for r in d.reasons if r != SUSPECTED_SECRET))
    cap = MAX_REASONS - len(marker)
    if len(reasons) > cap:
        reasons = [*reasons[: cap - 1], f"… {len(reasons) - cap + 1} more"]
    reasons += marker
    suggested = None
    if suggest_dest is not None:
        try:
            value = suggest_dest(d.path)
        except Exception:
            value = None
        suggested = value if isinstance(value, str) and value else None
    return PlanItem(
        item_id=d.item_id,
        kind=d.kind,
        source_path=d.path,
        role=d.role,
        origin=d.origin,
        include=d.include,
        reasons=tuple(reasons),
        size_estimate=int(d.size),
        git=d.git,
        suggested_dest=suggested,
        rel_cwd=d.rel_cwd,
        hermes_kind=d.hermes_kind if d.kind is ItemKind.HERMES_FILE else None,
        instruction_files=tuple(d.instructions),
    )


def _draft_from_candidate(idx: int, c: Candidate, options: Options) -> _Draft:
    if c.origin is Origin.CWD:
        role, include = ItemRole.PRIMARY, Include.YES
    elif c.kind is ItemKind.HERMES_FILE:
        role, include = ItemRole.HERMES, Include.YES if options.include_hermes_files else Include.NO
    elif c.kind in {ItemKind.GIT_REPO, ItemKind.FILE_TREE}:
        role, include = ItemRole.SECONDARY, Include.PROPOSED
    else:
        role, include = ItemRole.EXTRA, Include.PROPOSED
    reasons = list(c.reasons)
    if c.kind is ItemKind.HERMES_FILE and not options.include_hermes_files:
        reasons.append("include_hermes_files is off")
    return _Draft(
        kind=c.kind,
        path=c.path,
        role=role,
        origin=c.origin,
        include=include,
        weight=c.weight,
        reasons=reasons,
        rel_cwd=collector.rel_cwd(c.reasons) if role is ItemRole.PRIMARY else None,
        candidate=idx,
        hermes_kind=collector.hermes_kind(c.reasons) if c.kind is ItemKind.HERMES_FILE else None,
    )


def _warning_list(values: Iterable[object], redact: Callable[[str], str] | None = None) -> tuple[str, ...]:
    """The busy probe's warnings for ``ResourcePlan.warnings`` (K-3, W072): one line each, redacted when a redactor
    is given (they travel to the counterpart), de-duplicated, at most ``MAX_WARNINGS``."""
    out: list[str] = []
    for value in values or ():
        if not isinstance(value, str):
            continue
        text = value
        if redact is not None:
            try:
                text = redact(text)
            except Exception:  # an unredactable warning is reduced to its prefix
                text = text.split("(", 1)[0]
        text = _clean(text, 300)
        if text and text not in out:
            out.append(text)
    return tuple(out[:MAX_WARNINGS])


def _names(values: Iterable[object], pattern: re.Pattern[str], cap: int = 32) -> list[str]:
    out: list[str] = []
    for v in values:
        if isinstance(v, str) and pattern.fullmatch(v) and v not in out:
            out.append(v)
    return out[:cap]


def _merge_model_adds(b: _Builder, refinement: Refinement) -> int:
    added = 0
    for path, reason in refinement.add:
        if b.suggested_path(path, Origin.MODEL, reason) is not None:
            added += 1
    return added


def _merge_agent(b: _Builder, suggestions: Sequence[dict]) -> tuple[list[str], list[str], list[str]]:
    tools: list[str] = []
    envs: list[str] = []
    notes: list[str] = []
    for s in suggestions:
        if not isinstance(s, dict):
            continue
        resources = s.get("resources") if isinstance(s.get("resources"), list) else []
        for res in resources[:64]:
            if isinstance(res, dict):
                b.suggested_path(res.get("path"), Origin.AGENT, str(res.get("reason") or ""))
        excludes = s.get("exclude") if isinstance(s.get("exclude"), list) else []
        b.exclude([e for e in excludes[:64] if isinstance(e, str)], "excluded by the agent")
        tools += [t for t in s.get("tools_needed", []) or [] if isinstance(t, str)]
        envs += [e for e in s.get("env_vars_needed", []) or [] if isinstance(e, str)]
        if isinstance(s.get("note"), str) and s["note"].strip():
            notes.append(s["note"][:2000])
    return tools, envs, notes


def build_plan(
    job_id: str,
    revision: tuple,
    candidates: Sequence[Candidate],
    refinement: Refinement | None,
    report: RefineReport,
    hints: PlanHints,
    cfg: PolicyConfig,
    git_info: Callable[[str], GitInfo | None],
    busy: BusyReportLike,
    suggestions: Sequence[dict],
    options: Options,
    approx_tokens: int,
    *,
    redact: Callable[[str], str] | None = None,
    remote_driven: bool = False,
    suggest_dest: Callable[[str], str | None] | None = None,
) -> ResourcePlan:
    """The source's plan document (DESIGN §8.5) from the collector's candidates, the validated refinement, the
    agent tool's suggestions (merged here in every mode, BX H4) and the user's hints.

    ``redact`` (additive) is ``redact_sensitive_text(force=True)`` for the suspected-secret rule; without it every
    extra file is at most a proposal. ``remote_driven`` (additive): the hints come from the counterpart, so their
    includes are ignored (they only narrow, R18). ``suggest_dest(source_path)`` (additive) returns the destination
    this item came from on the target (its ``in`` row, return trip §9.7 rule 3). ``hints.include_secret`` is never
    read: secrets are released only through ``cfg.include_secret`` (local flags)."""
    cands = list(candidates)
    b = _Builder(cfg, options, git_info, redact)
    for idx, c in enumerate(cands):
        if c.path in b.by_path:
            continue
        d = b.add(_draft_from_candidate(idx, c, options))
        if d.role in {ItemRole.SECONDARY, ItemRole.EXTRA}:
            d.include = b.default_include(c.weight)
            if options.extra_paths == "off":
                d.reasons.append("extra_paths is off")
    for d in list(b.drafts):
        b.evaluate(d)
    if hints.no_workspace:
        p = b.primary()
        if p is not None and p.include is not Include.NO:
            p.include = Include.NO
            p.reasons.append("--no-workspace")

    added = dropped = 0
    tools: list[str] = []
    envs: list[str] = []
    notes: list[str] = []
    if refinement is not None:
        dropped = b.drop(refinement.drop, cands)
        added = _merge_model_adds(b, refinement)
        tools += list(refinement.tools_needed)
        envs += list(refinement.env_vars_needed)
        if refinement.notes.strip():
            notes.append(refinement.notes)
    agent_tools, agent_envs, agent_notes = _merge_agent(b, suggestions)
    tools += agent_tools
    envs += agent_envs
    notes += [f"[agent] {n}" for n in agent_notes]

    if not remote_driven:
        for path in hints.include:
            b.suggested_path(path, Origin.USER, "", explicit=True)
    for path in sorted(cfg.include_secret):
        b.suggested_path(path, Origin.USER, "include-secret", explicit=True)
    b.exclude(hints.exclude, "excluded (--exclude)")

    for d in b.drafts:
        b.finish(d)
    kept = [d for d in b.drafts if not d.denied or d.role is ItemRole.PRIMARY]
    _assign_ids(kept)
    items = tuple(_to_item(d, suggest_dest) for d in kept)
    return ResourcePlan(
        job_id=job_id,
        revision=tuple(revision),
        items=items,
        not_transferred=tuple(b.not_transferred),
        processes=tuple(dict(p) for p in busy.processes if isinstance(p, dict)),
        delegations=tuple(dict(p) for p in busy.delegations if isinstance(p, dict)),
        cron=tuple(dict(p) for p in busy.cron if isinstance(p, dict)),
        refine=replace(report, added=added, dropped=dropped),
        tools_needed=tuple(_names(tools, _TOOL_RE)),
        env_vars_needed=tuple(_names(envs, _ENV_RE)),
        notes_for_target="\n".join(notes)[:MAX_NOTES],
        approx_tokens=int(approx_tokens),
        warnings=_warning_list(getattr(busy, "warnings", ()), redact),
    )


def replan(
    plan: ResourcePlan,
    candidates: Sequence[Candidate],
    revision: tuple,
    cfg: PolicyConfig,
    *,
    options: Options | None = None,
    git_info: Callable[[str], GitInfo | None] | None = None,
    redact: Callable[[str], str] | None = None,
) -> tuple[ResourcePlan, bool]:
    """Re-plan after the revision moved between planning and lock (DESIGN §3.2): confirmed items keep their id and
    decision; a new candidate becomes at most a *proposal*, and ``needs_confirmation`` is True exactly when such a
    new proposal exists. ``options``/``git_info``/``redact`` (additive) refine the new items."""
    options = options or Options()
    fresh = build_plan(
        plan.job_id,
        revision,
        candidates,
        None,
        plan.refine,
        PlanHints(),
        cfg,
        git_info or (lambda _root: None),
        _Busy(plan.processes, plan.delegations, plan.cron, plan.warnings),
        (),
        options,
        plan.approx_tokens,
        redact=redact,
    )
    old = {(i.kind, i.source_path): i for i in plan.items}
    taken = {i.item_id for i in plan.items}
    items = list(plan.items)
    new_proposals = False
    extra: list[_Draft] = []
    for item in fresh.items:
        if (item.kind, item.source_path) in old:
            continue
        include = Include.NO if item.include is Include.NO else Include.PROPOSED
        new_proposals |= include is Include.PROPOSED
        extra.append(
            _Draft(
                kind=item.kind,
                path=item.source_path,
                role=item.role if item.role is not ItemRole.PRIMARY else ItemRole.SECONDARY,
                origin=item.origin,
                include=include,
                reasons=[*item.reasons, "found after planning"],
                git=item.git,
                size=item.size_estimate,
                instructions=list(item.instruction_files),
                hermes_kind=item.hermes_kind,
            )
        )
    _assign_ids(extra, taken)
    items += [_to_item(d, None) for d in extra]
    nt = list(plan.not_transferred)
    nt += [n for n in fresh.not_transferred if n not in nt]
    return replace(plan, revision=tuple(revision), items=tuple(items), not_transferred=tuple(nt)), new_proposals


# ---------------------------------------------------------------------------------------------------------
# FinalPlan helpers
# ---------------------------------------------------------------------------------------------------------


def final_doc(final: FinalPlan) -> dict:
    """The canonical FinalPlan document without ``plan_id`` (what ``plan_id`` hashes)."""
    doc = to_wire(final)
    doc.pop("plan_id", None)
    return doc


def plan_id(final_plan_doc: dict) -> str:
    """sha256 of the canonical bytes of the FinalPlan document (DESIGN §8.6)."""
    return doc_id(final_plan_doc)


def with_plan_id(final: FinalPlan) -> FinalPlan:
    """``final`` with ``plan_id`` recomputed from its content."""
    return replace(final, plan_id=plan_id(final_doc(final)))


def apply_choice(final: FinalPlan, choice: str) -> FinalPlan:
    """Apply the user's answer to the confirmation (DESIGN §14.1): ``"yes"`` confirms every proposal (``yes``)
    except suspected secrets (``SUSPECTED_SECRET`` in ``reasons``), which become ``no`` (R25: a blanket yes never
    takes them along); ``"no_proposed"`` ("yes without proposed items", also the non-interactive agent start and an
    unanswered confirmation of reason (a), SEC 5.6, BX L5) turns every proposal into ``no``. Items that are not
    proposals (a local ``include_secret`` is ``yes`` already) are left alone. Decisions of ``no`` items become
    ``skip``; the result carries no proposal and a recomputed ``plan_id``."""
    if choice not in CHOICES:
        raise ValueError(f"unknown choice {choice!r}")

    def decide(item: PlanItem) -> PlanItem:
        if item.include is not Include.PROPOSED:
            return item
        confirm = choice == "yes" and SUSPECTED_SECRET not in item.reasons
        return replace(item, include=Include.YES if confirm else Include.NO)

    items = tuple(decide(i) for i in final.plan.items)
    no = {i.item_id for i in items if i.include is Include.NO}
    freed = 0
    decisions = []
    for d in final.decisions:
        if d.item_id in no and d.mode is not DestMode.SKIP:
            freed += d.need_bytes
            d = replace(d, mode=DestMode.SKIP, need_bytes=0)
        decisions.append(d)
    out = replace(
        final,
        plan=replace(final.plan, items=items),
        decisions=tuple(decisions),
        need_bytes=max(0, final.need_bytes - freed),
    )
    return with_plan_id(out)


def user_item(
    item_id: str,
    path: str,
    kind: ItemKind,
    *,
    role: ItemRole = ItemRole.EXTRA,
    include: Include = Include.YES,
    origin: Origin = Origin.USER,
    size: int = 0,
    reasons: tuple[str, ...] = ("user",),
) -> PlanItem:
    """A plan item the conductor adds for a local ``--include`` of a pull (released by the source only under its
    ``export_roots``, ``restrict_to_releasable``)."""
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=path,
        role=role,
        origin=origin,
        include=include,
        reasons=reasons,
        size_estimate=size,
    )


def restrict_to_releasable(
    remote_final: FinalPlan,
    own_plan: ResourcePlan,
    cfg: PolicyConfig,
    *,
    remote_hints: PlanHints | None = None,
    redact: Callable[[str], str] | None = None,
) -> FinalPlan:
    """R18 / I11: the FinalPlan a remote conductor sent, reduced to what this source releases.

    * An item of the own plan must keep ``item_id``, ``source_path`` and ``kind``; it may be narrowed (``yes`` →
      ``no``; a proposal not confirmed stays out), never re-included when the own plan says ``no``.
    * Any other item is a remote addition: released only under ``cfg.export_roots`` and through the policy (no
      suspected secret); with ``export_roots`` empty there are none.
    * A proposal the source flagged as a suspected secret (``SUSPECTED_SECRET``) is never confirmed by the
      counterpart (R25: only a local ``include_secret`` releases it).
    * Own items the remote plan omits are not transferred.
    * ``remote_hints.include_secret`` is ignored and recorded (``include_secret from peer ignored``).
    * Plan-level free text, tool/env names, refusals and busy lists come from the own plan.

    Anything else raises ``PolicyDenied`` (``policy_denied``) once for the whole plan: ``details["items"]`` lists
    every refused item id (plan order, each once, at most ``MAX_REFUSED_IDS``; W017), the other details are the first
    refusal's. The result carries a recomputed ``plan_id``."""
    if remote_final.plan.job_id != own_plan.job_id:
        raise PolicyDenied("the remote plan belongs to another job", reason="not_allowed_root")
    own = {i.item_id: i for i in own_plan.items}
    export = [Path(_real(str(r))) for r in cfg.export_roots]
    out: list[PlanItem] = []
    seen: set[str] = set()
    refused: list[tuple[str, PolicyDenied]] = []
    for item in remote_final.plan.items:
        if item.item_id in seen:
            refused.append(
                (
                    item.item_id,
                    PolicyDenied(f"duplicate item {item.item_id}", reason="not_allowed_root", item=item.item_id),
                )
            )
            continue
        seen.add(item.item_id)
        mine = own.get(item.item_id)
        if mine is not None:
            error = _own_item_refusal(item, mine)
            if error is not None:
                refused.append((item.item_id, error))
                continue
            include = Include.YES if item.include is Include.YES else Include.NO
            out.append(replace(mine, include=include))
            continue
        try:
            out.append(_remote_addition(item, cfg, export, redact))
        except PolicyDenied as exc:
            refused.append((item.item_id, exc))
    if refused:
        raise _refusal(refused)
    for item in own_plan.items:
        if item.item_id not in seen:
            out.append(replace(item, include=Include.NO))
    nt = list(own_plan.not_transferred)
    for p in remote_hints.include_secret if remote_hints is not None else ():
        entry = NotTransferred(path=_norm(str(p)) if os.path.isabs(str(p)) else str(p)[:512], why=WHY_PEER_SECRET)
        if entry not in nt:
            nt.append(entry)
    ids = {i.item_id for i in out}
    decisions = tuple(d for d in remote_final.decisions if d.item_id in ids)
    plan = replace(own_plan, items=tuple(out), not_transferred=tuple(nt))
    return with_plan_id(replace(remote_final, plan=plan, decisions=decisions))


def _own_item_refusal(item: PlanItem, mine: PlanItem) -> PolicyDenied | None:
    """Why the counterpart's version of an own item is refused (None: it only narrows)."""
    if item.source_path != mine.source_path or item.kind is not mine.kind:
        return PolicyDenied(
            f"item {item.item_id} differs from the source's plan", reason="not_allowed_root", item=item.item_id
        )
    if mine.include is Include.NO and item.include is not Include.NO:
        return PolicyDenied(
            f"item {item.item_id} is not released by the source", reason="not_allowed_root", item=item.item_id
        )
    if mine.include is Include.PROPOSED and item.include is Include.YES and SUSPECTED_SECRET in mine.reasons:
        return PolicyDenied(
            f"item {item.item_id} is a suspected secret; only a local --include-secret releases it",
            reason=SUSPECTED_SECRET,
            item=item.item_id,
        )
    return None


def _refusal(refused: Sequence[tuple[str, PolicyDenied]]) -> PolicyDenied:
    """One ``PolicyDenied`` for every refused item: the first refusal's message and details plus
    ``details["items"]`` (W017)."""
    ids = list(dict.fromkeys(str(item_id)[:128] for item_id, _exc in refused))
    first = refused[0][1]
    message = first.message if len(ids) == 1 else f"{first.message} (and {len(ids) - 1} more refused items)"
    details = {**first.details, "items": ids[:MAX_REFUSED_IDS]}
    if len(ids) > MAX_REFUSED_IDS:
        details["items_total"] = len(ids)
    return PolicyDenied(message, details=details)


def _remote_addition(item: PlanItem, cfg: PolicyConfig, export: list[Path], redact) -> PlanItem:
    """A remote conductor's addition (R18, contract V29). Released only when the requested path is canonical — its
    realpath equals the normalised path, so no link on the way (``NON_CANONICAL_PATH`` otherwise) —, an extra file
    or directory of that kind under ``export_roots``, and allowed by the policy (no suspected secret; a file needs the
    redactor). A released item comes back unchanged except ``include`` (``yes`` stays, anything else ``no``): the
    restricted plan keeps the conductor's ``plan_id``, against which the target verifies the pack."""
    path = item.source_path
    deny = PolicyDenied(f"{path!r} is not released by this source", reason="not_allowed_root", path=str(path)[:300])
    if not export or not isinstance(path, str) or not os.path.isabs(path) or "\x00" in path:
        raise deny
    if item.kind not in {ItemKind.EXTRA_FILE, ItemKind.EXTRA_DIR}:
        raise deny
    real = _real(path)
    if real != _norm(path):
        raise PolicyDenied(
            f"{path!r} is not released: not a canonical path (a link on the way)",
            reason=NON_CANONICAL_PATH,
            path=str(path)[:300],
        )
    if not under_any(Path(real), export, case_insensitive=cfg.case_insensitive):
        raise deny
    p = Path(real)
    if item.kind is ItemKind.EXTRA_DIR:
        verdict = check_source_file(p, cfg, explicit=False)
        if not os.path.isdir(real):
            raise deny
    else:
        if redact is None or not os.path.isfile(real):
            raise deny
        verdict = check_extra(p, cfg, explicit=False, redact=redact)
    if not verdict.allowed or verdict.reason == "suspected_secret":
        raise PolicyDenied(
            f"{path!r} is not released: {_why(verdict)}", reason=str(verdict.reason), path=str(path)[:300]
        )
    return replace(item, include=Include.YES if item.include is Include.YES else Include.NO)


# ---------------------------------------------------------------------------------------------------------
# Summaries
# ---------------------------------------------------------------------------------------------------------


def _included(final: FinalPlan) -> list[PlanItem]:
    return [i for i in final.plan.items if i.include is Include.YES]


def _is_secret_why(why: str) -> bool:
    return why in {WHY_SECRET, WHY_PEER_SECRET} or why.endswith(f"({WHY_SECRET})")


_EXCLUDED_REASON = {
    "hermes": "hermes",
    WHY_HOME_WORKSPACE: "home",
    WHY_SECRET: "secret",
    WHY_TOO_LARGE: "too_large",
    "missing": "missing",
    "unreadable": "unreadable",
}


def workspace_excluded(final_or_plan: FinalPlan | ResourcePlan) -> dict | None:
    """The session's working directory the plan leaves behind (R30), or None: ``{"path", "reason", "why"}`` when the
    primary item (role ``primary``, origin ``cwd``) is ``no`` and named in ``not_transferred`` — a planning refusal,
    not the user's ``--no-workspace``. ``reason`` is a code (``hermes``, ``home``, ``secret``, ``too_large``,
    ``missing``, ``unreadable``, ``other``), ``why`` the plan's text."""
    rp = final_or_plan.plan if isinstance(final_or_plan, FinalPlan) else final_or_plan
    primary = next((i for i in rp.items if i.role is ItemRole.PRIMARY and i.origin is Origin.CWD), None)
    if primary is None or primary.include is not Include.NO:
        return None
    entry = next((n for n in rp.not_transferred if n.path == primary.source_path), None)
    if entry is None:
        return None
    return {"path": primary.source_path, "reason": _EXCLUDED_REASON.get(entry.why, "other"), "why": entry.why}


def yes_exempt(reasons: Iterable[str]) -> list[str]:
    """The reasons among ``reasons`` that a blanket yes does not confirm (``YES_EXEMPT``, R30), in their order."""
    return [r for r in reasons if r in YES_EXEMPT]


def _outside_workspace(final: FinalPlan) -> list[str]:
    return [i.source_path for i in _included(final) if i.role in {ItemRole.SECONDARY, ItemRole.EXTRA}]


def _denied_tracked(final: FinalPlan) -> list[str]:
    out: list[str] = []
    for i in _included(final):
        for p in i.git.denied_tracked if i.git is not None else ():
            if p not in out:
                out.append(p)
    return out


def summarize(final: FinalPlan) -> dict:
    """Plain summary of a FinalPlan for ``show plan``, ``/handover status`` and reports."""
    decisions = {d.item_id: d for d in final.decisions}
    items = []
    for i in final.plan.items:
        d = decisions.get(i.item_id)
        items.append(
            {
                "item_id": i.item_id,
                "kind": i.kind.value,
                "role": i.role.value,
                "origin": i.origin.value,
                "include": i.include.value,
                "source_path": i.source_path,
                "size": i.size_estimate,
                "dest": d.dest if d else None,
                "mode": d.mode.value if d else None,
                "conflict": None if d is None or d.conflict is None else d.conflict.kind,
                "reasons": list(i.reasons[:3]),
            }
        )
    return {
        "plan_id": final.plan_id,
        "job_id": final.plan.job_id,
        "items": items,
        "items_included": len(_included(final)),
        "proposed": sum(1 for i in final.plan.items if i.include is Include.PROPOSED),
        "conflicts": sum(1 for d in final.decisions if d.conflict is not None),
        "total_bytes": sum(i.size_estimate for i in _included(final)),
        "need_bytes": final.need_bytes,
        "free_bytes": dict(final.free_bytes),
        "not_transferred": [{"path": n.path, "why": n.why} for n in final.plan.not_transferred],
        "model_change": final.model_change,
        "refine": {"used": final.plan.refine.used, "error": final.plan.refine.error},
        "tools_needed": list(final.plan.tools_needed),
        "env_vars_needed": list(final.plan.env_vars_needed),
    }


PRESEND_KEYS = (
    "items_count",
    "total_bytes",
    "excluded_secrets",
    "denied_tracked",
    "withheld_stash",
    "outside_workspace",
    "instruction_files",
    "model",
    "offline",
    "workspace_excluded",
)
"""The keys of ``presend_summary`` the surfaces read (W125, R30 ``workspace_excluded``); ``item_list``, ``items`` and
``extras_outside_workspace`` come on top."""


def presend_summary(
    final: FinalPlan, *, withheld_stash: Sequence[str] = (), instructions: Sequence[dict] | None = None
) -> dict:
    """The summary every first progress message carries (DESIGN §8.7, SEC 5.8, W125), keyed as the surfaces read
    it (``PRESEND_KEYS``):

    * ``items_count`` and ``total_bytes`` of the included items; ``item_list`` names each of them;
    * ``excluded_secrets``: how many secrets stay behind (refusals for a secret, an ignored peer ``include_secret``);
    * ``denied_tracked``: tracked files on the secrets list inside included repositories;
    * ``withheld_stash``: stash entries the pack step withheld (known after packing; additive keyword);
    * ``outside_workspace``: included items outside the primary workspace;
    * ``instruction_files``: ``instructions`` when given (the target's ``instruction_files`` result, with
      ``status``), else the instruction files of the included items as ``{"item_id", "rel", "sha256"}``;
    * ``model`` (``model_text``) and ``offline`` (``offline_capable``) of the target model — both None when the
      model stays (X04);
    * ``workspace_excluded``: the session's working directory the policy leaves behind (``workspace_excluded``,
      R30) as ``{"path", "reason", "why"}``, else None.

    ``items`` and ``extras_outside_workspace`` are the layer-1 names of ``items_count`` and ``outside_workspace``,
    kept with the same values for readers that have not moved yet."""
    included = _included(final)
    outside = _outside_workspace(final)
    if instructions is None:
        listed = [
            {"item_id": i.item_id, "rel": rel, "sha256": sha} for i in included for rel, sha in i.instruction_files
        ]
    else:
        listed = [dict(e) for e in instructions if isinstance(e, dict)]
    return {
        "items_count": len(included),
        "total_bytes": sum(i.size_estimate for i in included),
        "excluded_secrets": sum(1 for n in final.plan.not_transferred if _is_secret_why(n.why)),
        "denied_tracked": _denied_tracked(final),
        "withheld_stash": [str(s) for s in withheld_stash],
        "outside_workspace": outside,
        "instruction_files": listed,
        "model": model_text(final.model_change),
        "offline": _model_offline(final.model_change),
        "workspace_excluded": workspace_excluded(final),
        "item_list": [
            {"item_id": i.item_id, "kind": i.kind.value, "path": i.source_path, "size": i.size_estimate}
            for i in included
        ],
        "items": len(included),
        "extras_outside_workspace": list(outside),
    }


def _model_to(model_change: dict | None) -> dict | None:
    """The ``to`` side of a ``model_change`` in the one form of contract X04, or None."""
    if not isinstance(model_change, dict):
        return None
    to = model_change.get("to")
    return to if isinstance(to, dict) else None


def model_text(model_change: dict | None) -> str | None:
    """``"<model> (<provider>)"`` of the target model of a ``model_change`` (X04 form); None when the model stays.

    Reads only ``model_change["to"]["model"|"provider"]``."""
    to = _model_to(model_change)
    if to is None:
        return None
    name = _clean(to.get("model"), 128) if isinstance(to.get("model"), str) else ""
    provider = _clean(to.get("provider"), 64) if isinstance(to.get("provider"), str) else ""
    if name and provider:
        return f"{name} ({provider})"
    return name or provider or None


def _model_offline(model_change: dict | None) -> bool | None:
    """``model_change["offline_capable"]`` (X04: the one offline flag, computed by the target with
    ``session_import.offline_capable``); None when the model stays or the flag is not a boolean."""
    if not isinstance(model_change, dict):
        return None
    flag = model_change.get("offline_capable")
    return flag if isinstance(flag, bool) else None


def _model_context(model_change: dict | None) -> int | None:
    """``model_change["to"]["context_length"]`` when it is a positive integer (X04), else None."""
    to = _model_to(model_change)
    value = to.get("context_length") if to is not None else None
    if isinstance(value, int) and not isinstance(value, bool) and value > 0:
        return value
    return None


def confirmation_reasons(
    final: FinalPlan,
    options: Options,
    source_offline: bool,
    *,
    instructions: Sequence[dict] = (),
    cfg: PolicyConfig | None = None,
) -> list[str]:
    """Why the user must confirm before anything is locked (DESIGN §14.1 (a)–(g)), in that order.

    ``source_offline``: the source's model route was local. ``instructions`` (additive): the result of
    ``instruction_files`` when they arrive from an unverified bundle or a remote source (reason g).

    Reason (d) comes only from ``cfg.include_secret`` (X07): ``cfg`` (additive) is the source's own
    ``PolicyConfig`` — the conductor passes it when the source is local (push, export); its ``include_secret`` holds
    exactly the paths given by a local ``--include-secret``. ``options.hints.include_secret`` is never read: hints may
    come from the counterpart and never release a secret (R18). Reason (e) reads ``final.model_change`` only in the
    form of contract X04. Reason (h) ``workspace_excluded`` (R30): the policy leaves the session's working directory
    behind (``workspace_excluded``) and the user did not say ``--no-workspace`` (``options.hints.no_workspace``); a
    blanket yes never confirms it (``YES_EXEMPT``)."""
    reasons = []
    if any(i.include is Include.PROPOSED for i in final.plan.items):
        reasons.append(CONFIRM_PROPOSED)
    if any(d.conflict is not None for d in final.decisions):
        reasons.append(CONFIRM_CONFLICT)
    if sum(i.size_estimate for i in _included(final)) > options.confirm_above_bytes:
        reasons.append(CONFIRM_LARGE)
    if cfg is not None and cfg.include_secret:
        reasons.append(CONFIRM_SECRET)
    changed = isinstance(final.model_change, dict)
    offline = _model_offline(final.model_change)
    context = _model_context(final.model_change)
    # a changed model without a boolean offline flag counts as not offline-capable: (e) asks rather than stays silent
    if (source_offline and changed and offline is not True) or (
        context is not None and final.plan.approx_tokens > context
    ):
        reasons.append(CONFIRM_MODEL)
    if _outside_workspace(final) or _denied_tracked(final):
        reasons.append(CONFIRM_OUTSIDE)
    if instructions:
        reasons.append(CONFIRM_INSTRUCTIONS)
    if workspace_excluded(final) is not None and options.hints.no_workspace is not True:
        reasons.append(CONFIRM_WORKSPACE_EXCLUDED)
    return reasons


def instruction_files(items: Sequence[PlanItem], dest_lookup: Callable[[str], str | None]) -> list[dict]:
    """Transferred instruction files that are new or different at the destination (SEC 4.3).

    Reads ``PlanItem.instruction_files`` (``(rel, sha256)`` the source recorded, K-3) of every included item;
    ``dest_lookup(item_id)`` names the item's destination directory readable on this host (None ⇒ unknown, listed
    as ``new``). An identical file at the destination is not listed. Relative paths from a peer are checked with
    ``paths.check_rel`` before they are joined; an entry with a malformed path or digest is skipped."""
    out: list[dict] = []
    for item in items:
        if item.include is not Include.YES:
            continue
        try:
            dest = dest_lookup(item.item_id)
        except Exception:
            dest = None
        for entry in item.instruction_files:
            if not (isinstance(entry, (tuple, list)) and len(entry) == 2):
                continue
            rel, digest = entry
            if not (isinstance(rel, str) and isinstance(digest, str) and _SHA256_RE.fullmatch(digest)):
                continue
            try:
                check_rel(rel, _POSIX_RULES)
            except PolicyDenied:
                continue
            status = "new"
            if dest:
                there = _sha256_file(Path(dest) / rel)
                if there is not None:
                    if there == digest:
                        continue
                    status = "changed"
            out.append({"item_id": item.item_id, "rel": rel, "status": status, "sha256": digest})
    return out


def note_parts_from(
    plan: ResourcePlan | FinalPlan,
    *,
    withheld_stash: Sequence[str] = (),
    source_kept: bool = False,
    paused: Sequence[str] = (),
    instruction: Sequence[dict] = (),
    pack_time: float | None = None,
    warnings: Sequence[str] = (),
    title: str | None = None,
    extra: dict | None = None,
) -> dict:
    """The ``note_parts`` document the source sends with ``prepare`` (consumed by ``note.compose``; X16).

    Lists and flags only — the model and agent notes travel as the separate untrusted text. Every key is one
    ``note.compose`` renders (``note.NOTE_PART_KEYS``): ``source_kept`` says the session stays active on the source
    (an export with ``--keep`` or the import of such a copy; O3, W070); ``warnings`` are the plan's warnings
    (``ResourcePlan.warnings``) followed by ``warnings``; ``pack_time`` (seconds since the epoch) and ``title``
    (the session title, shown only inside the note's data block) appear when given.

    ``extra`` adds or extends keys of the same set: a list extends the list already there (without duplicates),
    any other value fills a key that is missing or empty. A key ``note.compose`` does not render raises
    ``ValueError`` — a fact nobody shows must not travel as if it did (no silent swallowing)."""
    resource = plan.plan if isinstance(plan, FinalPlan) else plan
    final = plan if isinstance(plan, FinalPlan) else FinalPlan("", plan, (), 0, {}, None)
    unknown = note.unknown_note_parts(extra or {})
    if unknown:
        raise ValueError("note parts that note.compose does not render: " + ", ".join(unknown))
    parts: dict = {
        "not_transferred": [{"path": n.path, "why": n.why} for n in resource.not_transferred],
        "processes": [dict(p) for p in resource.processes],
        "delegations": [dict(p) for p in resource.delegations],
        "cron": [dict(p) for p in resource.cron],
        "paused": [str(p) for p in paused],
        "secrets_excluded": [n.path for n in resource.not_transferred if _is_secret_why(n.why)],
        "withheld_stash": [str(s) for s in withheld_stash],
        "denied_tracked": _denied_tracked(final),
        "tools_needed": list(resource.tools_needed),
        "env_vars_needed": list(resource.env_vars_needed),
        "source_kept": bool(source_kept),
        "instruction_files": [dict(i) for i in instruction],
        "warnings": list(dict.fromkeys([*resource.warnings, *(str(w) for w in warnings)]))[:MAX_WARNINGS],
    }
    if isinstance(pack_time, (int, float)) and not isinstance(pack_time, bool) and pack_time > 0:
        parts["pack_time"] = float(pack_time)
    if isinstance(title, str) and title.strip():
        parts["title"] = title[:512]
    for key, value in (extra or {}).items():
        have = parts.get(key)
        if isinstance(have, list) and isinstance(value, (list, tuple)):
            parts[key] = have + [v for v in value if v not in have]
        elif not have:
            parts[key] = value
    return parts


def bundle_negotiate(
    plan: ResourcePlan,
    facts: EnvFacts | None = None,
    *,
    out_dir: str | os.PathLike | None = None,
    disk_usage: Callable[[str], object] = shutil.disk_usage,
) -> FinalPlan:
    """The negotiation of an export bundle (``bundle.BundleTarget(negotiate=…)``, W062, X23).

    A bundle decides no destination — the importing target does in prepare without changing the plan id (X02) — so
    every item that is not ``no`` gets a ``fresh`` decision with reason ``bundle``, its ``suggested_dest`` (or
    ``""``) and its size as ``need_bytes``. Proposals get one too: after a ``yes`` (``apply_choice``) a confirmed
    proposal must find its decision; a dropped one becomes ``skip`` there. ``need_bytes`` is the sum. With
    ``out_dir`` (the bundle's directory) the free space of its nearest existing ancestor is recorded and a shortfall
    raises ``InsufficientStorage`` before anything is written; nothing is created. ``facts`` (the bundle's declared
    target facts) complete the ``(plan, facts)`` signature the bundle target calls and decide nothing;
    ``model_change`` is None (the importer maps the model, R11). The result carries its ``plan_id``."""
    decisions = tuple(
        TargetDecision(
            item_id=i.item_id,
            dest=i.suggested_dest or "",
            mode=DestMode.FRESH,
            reason="bundle",
            need_bytes=max(0, int(i.size_estimate)),
        )
        for i in plan.items
        if i.include is not Include.NO
    )
    need = sum(d.need_bytes for d in decisions)
    free: dict[str, int] = {}
    if out_dir is not None:
        where = os.path.abspath(os.fspath(out_dir))
        probe = where
        while not os.path.isdir(probe) and os.path.dirname(probe) != probe:
            probe = os.path.dirname(probe)
        available = int(disk_usage(probe).free)
        free[where] = available
        if need > available:
            raise InsufficientStorage(
                f"The bundle needs {need / 1024**3:.1f} GiB free in {where}, has {available / 1024**3:.1f} GiB. "
                "Nothing was written.",
                need_bytes=need,
                free_bytes=available,
                path=where,
            )
    final = FinalPlan(plan_id="", plan=plan, decisions=decisions, need_bytes=need, free_bytes=free, model_change=None)
    return with_plan_id(final)


def decision_index(decisions: Sequence[TargetDecision]) -> dict[str, TargetDecision]:
    """``item_id`` → decision."""
    return {d.item_id: d for d in decisions}
