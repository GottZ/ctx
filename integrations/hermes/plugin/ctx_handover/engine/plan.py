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

Workspace against the working folder (R36, L3-F19): the session's workspace is the primary item — the repository
around its working folder, or the folder itself when git ignores it there (the collector decides). ``presend_summary``
always names it (``workspace``: path and size against the folder and the folder's size, which the source measures into
``PlanItem.cwd_bytes``); when the workspace holds more than the folder (``rel_cwd`` set) and the folder holds no file
the repository tracks (``PlanItem.cwd_tracked`` not True: typically somebody else's checkout, or unknown),
``confirmation_reasons`` raises ``workspace_differs`` (letter j, in ``YES_EXEMPT``). A tracked subfolder of the user's
own repository (``repo/src``) is the normal case and goes without a question (R36a).

Session larger than the target's context window (R32, L3-F17): ``context_over`` compares the estimated size of the
session's active context (``ResourcePlan.context_tokens`` when the source estimated it with
``active_context_tokens``, else the lineage total ``approx_tokens`` as an upper bound) with the target model's window
(``target_window``: ``model_change.to.context_length`` on a model change, the target's own window when the model
stays). Larger ⇒ ``presend_summary`` says so (``context_over``), ``confirmation_reasons`` raises ``context_over``
(letter i, in ``YES_EXEMPT``) and the note on the target says that the first turn compacts (note part
``context_tokens``). Nothing is refused.

Suspected secrets (R25): an extra file the policy suspects (or could not check, no redactor) carries the exact reason
element ``SUSPECTED_SECRET``; ``apply_choice(final, "yes")`` never confirms it and ``restrict_to_releasable`` never
lets a counterpart confirm it. Only a local ``include_secret`` (``yes`` already in the plan) releases it.

Left behind, named: a file tree beyond ``max_files`` or its byte cap is left out at planning and named
with the limit (H12; the pack would refuse it after the lock); a git item's size counts the object store a fresh pack
reads — behind a gitfile and through alternates (``git_object_bytes``, H13) — and an unmeasurable one carries
``SIZE_UNKNOWN`` (reason (c)); repositories inside a transferred git workspace (initialised submodules, clones in the
work tree) are named and raise reason (k) ``repos_left`` (H15, R30 form); a transcript folder around a repository of
the plan is no extra (M36); another account's home and Hermes home are no source (M35, ``collector.HomeRule``).
``presend_summary`` names the paths outside the workspace (``outside_items``, M37), the repositories left behind
(``repos_left``) and an unknown context window on the target (``context_unknown``, M12).

Plan items carry their facts in fields, not in reason texts (contract K-3): ``PlanItem.hermes_kind`` (session file
kind), ``PlanItem.instruction_files`` (``(rel, sha256)`` of the instruction files inside the item) and
``ResourcePlan.warnings`` (the busy probe's warnings). Reason texts are for people.
"""

from __future__ import annotations

import fnmatch
import hashlib
import json
import os
import re
import shutil
import stat
from collections.abc import Callable, Iterable, Mapping, Sequence
from dataclasses import dataclass, field, fields, replace
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
CONFIRM_CONTEXT = "context_over"
CONFIRM_WORKSPACE_DIFFERS = "workspace_differs"
CONFIRM_REPOS_LEFT = "repos_left"
CONFIRM_LETTERS: dict[str, str] = {
    CONFIRM_PROPOSED: "a",
    CONFIRM_CONFLICT: "b",
    CONFIRM_LARGE: "c",
    CONFIRM_SECRET: "d",
    CONFIRM_MODEL: "e",
    CONFIRM_OUTSIDE: "f",
    CONFIRM_INSTRUCTIONS: "g",
    CONFIRM_WORKSPACE_EXCLUDED: "h",
    CONFIRM_CONTEXT: "i",
    CONFIRM_WORKSPACE_DIFFERS: "j",
    CONFIRM_REPOS_LEFT: "k",
}
"""Confirmation reasons of DESIGN §14.1 (a)–(g) plus (h) ``workspace_excluded`` (R30), (i) ``context_over``
(R32: the session is larger than the target model's context window; before G‴ part of (e)) and (j)
``workspace_differs`` (R36/R36a: the workspace holds more than the session's working folder, which holds no tracked
file of it) and (k) ``repos_left`` (H15, in R30's form: a repository inside a transferred git workspace — an initialised
submodule or a clone in the work tree — stays here with its files and changes) and their letters.
``None`` (no answer) cancels the job for every reason except ``proposed`` alone, which only excludes the proposals
(BX L5)."""
YES_EXEMPT: frozenset[str] = frozenset(
    {CONFIRM_WORKSPACE_EXCLUDED, CONFIRM_CONTEXT, CONFIRM_WORKSPACE_DIFFERS, CONFIRM_REPOS_LEFT}
)
"""Reasons a blanket yes (``--yes``, ``Options.yes``) never confirms (R30, R32, R36, H15 in R30's form, like suspected
secrets under R25):
the conductor asks for them even with ``--yes``; without an answer the job ends with nothing locked."""

CHOICES = ("yes", "no_proposed")

SUSPECTED_SECRET = "suspected_secret"  # noqa: S105 - reason marker, not a secret
"""Exact ``PlanItem.reasons`` element of an item the policy suspects to hold a secret, or could not check (R25,
X05). ``apply_choice(final, "yes")`` turns such a proposal into ``no``."""
UNVERIFIED = "unverified"
"""Origin marker of a handover from a source this side cannot verify — an unsigned bundle, one signed by an unknown
or revoked key, or the remote source of a pull (DESIGN §8.7 reason (g), SEC 4.3; a continuation refuses such an
origin with this reason, R22). Surfaces import it with the reason codes (X03)."""
REASON_CODES: tuple[str, ...] = tuple(CONFIRM_LETTERS)
"""Every confirmation reason code, in letter order (a)–(k) (X03)."""
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
WHY_NOT_ABSOLUTE = "not an absolute path"
"""Why a path the user named (``--include``) was not taken: it is relative (L3-F24: named, never skipped silently)."""
WHY_NOT_DIRECTORY = "not a directory"
"""Why the session's cwd stays here when it exists but is no directory (a file; H14). A link whose target is gone is
``missing``."""
WHY_SUBMODULE = "submodule: its files and changes stay here (the repository records only its commit)"
WHY_NESTED_REPO = "repository inside the workspace: its files stay here"
"""Why a repository inside a transferred git workspace stays here (H15): an initialised submodule (a gitlink of the
workspace's index) or a clone in its work tree. ``repos_left`` lists both."""
SIZE_UNKNOWN = "size unknown: the git history could not be measured"
"""Exact reason element of a git item whose object store could not be found (H13): its size counts its untracked
files only; reason (c) ``large`` asks."""
PRESEND_PATHS_MAX = 10
"""Paths ``presend_summary`` lists per list (``outside_items``, ``repos_left``); the rest is counted (``*_more``)."""
MAX_INNER_REPOS = 256
"""Submodules named per git item (``WHY_SUBMODULE``)."""
MAX_OBJECT_ENTRIES = 2_000_000
"""Entries of a git object store counted for its size (loose objects, packs, alternates); beyond it the size is a lower
bound and says so."""
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
    cwd_bytes: int | None = None
    cwd_tracked: bool | None = None
    inner: bool = False


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


_SUSPECTED_LABELS = frozenset({"suspected secret: proposed only", "not checked for tokens: proposed only"})


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
        own = [str(p) for p in (cfg.hermes_home, cfg.hermes_root) if p is not None and str(p) != ""]
        self.homes = collector.HomeRule.for_host(str(cfg.home), cfg, own_hermes=own, real=_real)

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
        """A filesystem root, a home (own or any account's, M35) or an ancestor of one (``collector.HomeRule``)."""
        return self.homes(_real(path))

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

    def released(self, *spellings: str) -> bool:
        """One of ``spellings`` was named with a local ``--include-secret`` (``cfg.include_secret``, X07)."""
        return bool({_norm(p) for p in spellings if p} & set(self.cfg.include_secret))

    def deny(self, d: _Draft, why: str) -> None:
        """Policy refusal: the draft leaves the plan (only the primary stays, as ``no``) and is named."""
        d.include = Include.NO
        d.denied = True
        self.refuse(d.path, why)

    def over_limit(self, d: _Draft, limit: str | None = None) -> None:
        """Size refusal: the item stays visible as ``no`` and is named (``limit`` says which limit; ``--include`` lifts
        the file limit, ``--max-total-gb`` the byte cap of a tree)."""
        d.include = Include.NO
        d.reasons.append(
            f"over the size limit ({WHY_TOO_LARGE}): {limit}" if limit else f"over the size limit ({WHY_TOO_LARGE})"
        )
        self.refuse(d.path, WHY_TOO_LARGE)

    def report_inner_repos(self) -> None:
        """H15 (R30 form): every repository inside a transferred git item stays here — each initialised submodule of
        the item (a gitlink of ``GitInfo.submodules`` whose folder holds a ``.git`` entry) and each repository the
        collector found the session working in (``inner`` drafts) — named as ``WHY_SUBMODULE`` or
        ``WHY_NESTED_REPO``. A repository inside an item that does not travel is not named on its own."""
        repos = [d for d in self.drafts if d.kind is ItemKind.GIT_REPO and not d.denied and d.include is not Include.NO]
        subs: dict[str, set[str]] = {}
        for r in repos:
            found: set[str] = set()
            for rel in (r.git.submodules if r.git is not None else ())[:MAX_INNER_REPOS]:
                parts = [p for p in str(rel).split("/") if p and p not in {".", ".."}]
                if not parts:
                    continue
                path = os.path.join(r.path, *parts)
                if os.path.lexists(os.path.join(path, ".git")):
                    found.add(path)
            subs[r.path] = found
            for path in sorted(found):
                self.refuse(path, WHY_SUBMODULE)
        for d in self.drafts:
            if not d.inner:
                continue
            owner = next((r for r in repos if r.path != d.path and _under(d.path, r.path, self.ci)), None)
            if owner is not None:
                self.refuse(d.path, WHY_SUBMODULE if d.path in subs.get(owner.path, ()) else WHY_NESTED_REPO)

    def evaluate(self, d: _Draft) -> None:
        """Apply the policy to one draft; a refusal sets ``no`` and names it."""
        if d.kind is ItemKind.HERMES_FILE:
            self._evaluate_hermes(d)
            return
        if d.role is ItemRole.PRIMARY and collector.CWD_NOT_DIR_REASON in d.reasons:
            # H14: the cwd exists but is no directory — named, so R30 (h) asks
            self.deny(d, "missing" if not os.path.exists(d.path) else WHY_NOT_DIRECTORY)
            return
        if d.role is ItemRole.PRIMARY and self.home_like(d.path):
            self.deny(d, WHY_HOME_WORKSPACE)
            return
        if d.role is not ItemRole.PRIMARY and collector.INNER_REPO_REASON in d.reasons:
            # H15: never an item; named by ``report_inner_repos`` once the workspace's submodules are known
            d.include = Include.NO
            d.denied = d.inner = True
            return
        if self.homes.in_foreign_hermes(_real(d.path)) or self.homes.in_foreign_hermes(_norm(d.path)):
            self.deny(d, "hermes")  # M35: another account's Hermes home is never a source
            return
        if d.kind is ItemKind.EXTRA_DIR and d.origin is Origin.TRANSCRIPT and self.contains_tree(d.path):
            self.deny(d, WHY_CONTAINS_WORKSPACE)  # M36: a folder around a repository of the plan is no extra
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
        if not isinstance(raw, str) or not raw or "\x00" in raw:
            return None
        if not os.path.isabs(raw):
            if origin is Origin.USER:
                self.refuse(raw, WHY_NOT_ABSOLUTE)  # L3-F24: a path the user named is never dropped without a word
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
                if existing.suspected and not self.released(given, real):
                    return None  # L10/R25: --include names it, only --include-secret releases a suspected secret
                if existing.suspected:
                    existing.suspected = False
                    existing.reasons = [r for r in existing.reasons if r not in _SUSPECTED_LABELS]
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
        # L10/R25: a user's --include confirms the path, but a suspected secret stays a proposal ("yes" never takes
        # it); only --include-secret (``cfg.include_secret``, which skips the suspicion) releases it
        include = Include.YES if (origin is Origin.USER and not suspected) or auto else Include.PROPOSED
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
            objects, complete = git_object_bytes(Path(d.path))
            d.size = (objects or 0) + (d.git.untracked_bytes if d.git else 0)
            if objects is None and (d.git is None or d.git.head is not None):
                d.reasons.append(SIZE_UNKNOWN)  # H13: never a silent 0 for a repository with history
            elif not complete:
                d.reasons.append(f"git history at least {_size_text(objects or 0)} (not counted to the end)")
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
            size, over = _tree_count(Path(d.path), matcher, cap, self.cfg.max_files)
            d.size = size
            if over is not None:
                # H12: a tree beyond max_files or the byte cap is left out and named here — never packed after the lock
                # (``filetree.scan`` refuses more than max_files) and never sized with a capped count
                setting = "max_extra_dir_mb" if cap != self.cfg.max_total_bytes else "max_total_gb"
                limit = (
                    f"more than {self.cfg.max_files} entries (max_files)"
                    if over == "entries"
                    else f"more than {_size_text(cap)} ({setting})"
                )
                self.over_limit(d, limit)
        elif d.kind is ItemKind.EXTRA_FILE:
            try:
                d.size = os.lstat(d.path).st_size
            except OSError:
                d.size = 0
        if d.kind in {ItemKind.GIT_REPO, ItemKind.FILE_TREE} and d.include is not Include.NO:
            d.instructions = _scan_instructions(d.path, d.rel_cwd)
        if d.role is ItemRole.PRIMARY and d.rel_cwd:
            d.cwd_bytes = self.folder_size(Path(d.path) / d.rel_cwd)

    def folder_size(self, folder: Path) -> int:
        """Bytes of the regular files below ``folder`` as a file tree would count them (ignore rules found below it,
        ``extra_ignore``, built-ins; capped like a workspace) — the session's working folder inside a larger
        workspace (R36: ``PlanItem.cwd_bytes``)."""
        try:
            matcher = IgnoreMatcher.for_tree(folder, self.options.extra_ignore)
        except Exception:  # unreadable ignore rules: count everything
            matcher = None
        size, _over = _tree_count(folder, matcher, self.cfg.max_total_bytes, self.cfg.max_files)
        return size


def _size_text(n: int) -> str:
    if n >= 1024**3:
        return f"{n / 1024**3:.1f} GiB"
    if n >= 1024**2:
        return f"{n / 1024**2:.1f} MiB"
    return f"{n} bytes"


def _tree_count(root: Path, matcher: IgnoreMatcher | None, cap_bytes: int, max_entries: int) -> tuple[int, str | None]:
    """``(bytes, over)``: bytes of the regular files below ``root`` (sorted walk, links not followed, ignored entries
    skipped) and which limit stopped the count — ``"entries"`` (more than ``max_entries``), ``"bytes"`` (more than
    ``cap_bytes``) or None (complete: the bytes are the size)."""
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
                return total, "entries"
            if is_dir:
                stack.append((child_rel, Path(entry.path)))
            elif is_file:
                try:
                    total += entry.stat(follow_symlinks=False).st_size
                except OSError:
                    continue
                if total > cap_bytes:
                    return total, "bytes"
    return total, None


def _git_text(path: Path, cap: int = 4096) -> str | None:
    try:
        with open(path, "rb") as fh:
            return fh.read(cap).decode("utf-8", errors="surrogateescape").strip()
    except OSError:
        return None


def _alternate_line(line: str, base: Path) -> Path | None:
    """One ``objects/info/alternates`` line: a path absolute or relative to the objects folder; C-quoted when it starts
    with ``"`` (``git`` writes it so for unusual names); comments and blanks are no path."""
    line = line.strip()
    if not line or line.startswith("#"):
        return None
    if line.startswith('"') and line.endswith('"') and len(line) >= 2:
        try:
            line = line[1:-1].encode("latin-1", errors="surrogateescape").decode("unicode_escape")
        except (UnicodeError, ValueError):
            return None
    path = Path(line)
    return Path(os.path.normpath(path if path.is_absolute() else base / path))


def git_object_dirs(root: Path) -> list[Path] | None:
    """The object folders a fresh pack of the work tree at ``root`` reads (H13): the repository's common git dir's
    ``objects`` — ``.git`` a folder, or a gitfile (``gitdir: …``: linked worktree, submodule, ``--separate-git-dir``)
    whose git dir names its common dir in ``commondir`` — plus its alternates (``objects/info/alternates``, followed
    five levels deep as git does, each folder once). None when no object folder can be found."""
    dot = Path(root) / ".git"
    try:
        st = os.stat(dot)
    except OSError:
        return None
    if stat.S_ISDIR(st.st_mode):
        gitdir = dot
    elif stat.S_ISREG(st.st_mode):
        text = _git_text(dot) or ""
        first = text.splitlines()[0] if text else ""
        if not first.startswith("gitdir:"):
            return None
        target = Path(first[len("gitdir:") :].strip())
        gitdir = target if target.is_absolute() else Path(root) / target
    else:
        return None
    common = gitdir
    pointer = _git_text(gitdir / "commondir")
    if pointer:
        target = Path(pointer.splitlines()[0].strip())
        common = target if target.is_absolute() else gitdir / target
    first_objects = Path(os.path.normpath(common / "objects"))
    if not first_objects.is_dir():
        return None
    out: list[Path] = []
    seen: set[str] = set()
    level = [first_objects]
    for _depth in range(6):
        following: list[Path] = []
        for objects in level:
            key = _real(str(objects))
            if key in seen or not objects.is_dir():
                continue
            seen.add(key)
            out.append(objects)
            for line in (_git_text(objects / "info" / "alternates", 64 * 1024) or "").splitlines()[:64]:
                alt = _alternate_line(line, objects)
                if alt is not None:
                    following.append(alt)
        level = following
        if not level:
            break
    return out


def git_object_bytes(root: Path) -> tuple[int | None, bool]:
    """``(bytes, complete)`` of the git objects a fresh pack of the work tree at ``root`` draws on (``git_object_dirs``:
    loose objects and packs of the common dir and its alternates; DESIGN §8.2 step 6 names ``git count-objects -v``,
    which counts the same files). ``(None, False)`` when the object store cannot be found (H13: unknown, never 0);
    ``complete`` is False when more than ``MAX_OBJECT_ENTRIES`` entries were seen (the bytes are a lower bound)."""
    dirs = git_object_dirs(root)
    if dirs is None:
        return None, False
    total, complete = 0, True
    budget = MAX_OBJECT_ENTRIES
    for objects in dirs:
        size, over = _tree_count(objects, None, 1 << 62, budget)
        total += size
        if over is not None:
            complete = False
            break
    return total, complete


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
        cwd_bytes=d.cwd_bytes if d.role is ItemRole.PRIMARY and d.rel_cwd else None,
        cwd_tracked=d.cwd_tracked if d.role is ItemRole.PRIMARY and d.rel_cwd else None,
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
        cwd_tracked=collector.cwd_tracked(c.reasons) if role is ItemRole.PRIMARY else None,
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
    context_tokens: int | None = None,
) -> ResourcePlan:
    """The source's plan document (DESIGN §8.5) from the collector's candidates, the validated refinement, the
    agent tool's suggestions (merged here in every mode, BX H4) and the user's hints.

    ``context_tokens`` (additive, R32) is the source's estimate of the session's active context
    (``active_context_tokens``); it becomes ``ResourcePlan.context_tokens`` once that field exists (K0, added after the
    v1 wire form) — until then the plan carries ``approx_tokens`` only and ``context_tokens_of`` falls back to it.

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
    b.report_inner_repos()
    kept = [d for d in b.drafts if not d.denied or d.role is ItemRole.PRIMARY]
    _assign_ids(kept)
    items = tuple(_to_item(d, suggest_dest) for d in kept)
    out = ResourcePlan(
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
    return _with_context_tokens(out, context_tokens)


def _with_context_tokens(rp: ResourcePlan, tokens: object) -> ResourcePlan:
    """``rp`` with ``context_tokens`` set when the model has that field and ``tokens`` is a positive integer."""
    if not _positive_int(tokens) or "context_tokens" not in {f.name for f in fields(ResourcePlan)}:
        return rp
    return replace(rp, context_tokens=int(tokens))  # type: ignore[call-arg]


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


def _join_rel(root: str, rel: str) -> str:
    """``root`` joined with the ``/``-separated ``rel`` in the separator of ``root`` (a Windows source keeps ``\\``)."""
    sep = "\\" if re.match(r"^(?:[A-Za-z]:[\\/]|\\\\)", root) else "/"
    return root.rstrip("/\\") + sep + rel.replace("/", sep)


def _primary_item(final_or_plan: FinalPlan | ResourcePlan) -> PlanItem | None:
    rp = final_or_plan.plan if isinstance(final_or_plan, FinalPlan) else final_or_plan
    return next((i for i in rp.items if i.role is ItemRole.PRIMARY and i.origin is Origin.CWD), None)


def workspace_differs(final_or_plan: FinalPlan | ResourcePlan) -> bool:
    """Reason (j) (R36/R36a): the workspace travels and holds more than the session's working folder, and that folder
    holds no file the repository tracks — ``PlanItem.cwd_tracked`` False or unknown (None: a source before R36a, or git
    could not tell). Only an explicit True (a tracked folder of the user's own repository) spares the question."""
    ws = workspace(final_or_plan)
    if ws is None or not ws["differs"]:
        return False
    primary = _primary_item(final_or_plan)
    return primary is None or primary.cwd_tracked is not True


def workspace(final_or_plan: FinalPlan | ResourcePlan) -> dict | None:
    """The session's workspace against its working folder (R36, L3-F19), or None when there is no working-folder item
    or it does not travel (include ``no``: R30's ``workspace_excluded``, ``--no-workspace``).

    ``{"path", "bytes", "cwd", "cwd_bytes", "differs"}`` of the primary item (role ``primary``, origin ``cwd``):
    ``path``/``bytes`` are its root and size estimate; ``cwd`` is the session's folder (the root joined with
    ``rel_cwd``); ``cwd_bytes`` the folder's size — ``bytes`` when the folder is the root, else what the source
    measured (``PlanItem.cwd_bytes``; None from a source before R36); ``differs`` is True when the workspace holds more
    than the folder (the repository around it, ``rel_cwd`` set)."""
    primary = _primary_item(final_or_plan)
    if primary is None or primary.include is Include.NO:
        return None
    rel = primary.rel_cwd if isinstance(primary.rel_cwd, str) else ""
    measured = primary.cwd_bytes
    if not (isinstance(measured, int) and not isinstance(measured, bool) and measured >= 0):
        measured = None
    return {
        "path": primary.source_path,
        "bytes": int(primary.size_estimate),
        "cwd": _join_rel(primary.source_path, rel) if rel else primary.source_path,
        "cwd_bytes": measured if rel else int(primary.size_estimate),
        "differs": bool(rel),
    }


def yes_exempt(reasons: Iterable[str]) -> list[str]:
    """The reasons among ``reasons`` that a blanket yes does not confirm (``YES_EXEMPT``, R30, R32, R36), in their
    order."""
    return [r for r in reasons if r in YES_EXEMPT]


def _outside_workspace(final: FinalPlan) -> list[str]:
    return [i.source_path for i in _included(final) if i.role in {ItemRole.SECONDARY, ItemRole.EXTRA}]


def outside_items(final: FinalPlan, limit: int = PRESEND_PATHS_MAX) -> tuple[list[dict], int]:
    """M37: the included items outside the primary workspace (secondary repositories, extra files and folders) the
    summary before sending names with path and size — ``({"item_id", "kind", "role", "path", "size"}, …)`` largest
    first (then by path), at most ``limit`` — and how many more there are."""
    rows = [
        {
            "item_id": i.item_id,
            "kind": i.kind.value,
            "role": i.role.value,
            "path": i.source_path,
            "size": i.size_estimate,
        }
        for i in _included(final)
        if i.role in {ItemRole.SECONDARY, ItemRole.EXTRA}
    ]
    rows.sort(key=lambda r: (-int(r["size"]), r["path"]))
    return rows[: max(0, limit)], max(0, len(rows) - max(0, limit))


def _path_parts(path: str) -> list[str]:
    return [p for p in path.replace("\\", "/").split("/") if p]


def _inside_text(path: str, root: str) -> bool:
    """``path`` lies strictly below ``root`` (plain text, either separator: the plan may come from another OS)."""
    p, r = _path_parts(path), _path_parts(root)
    return len(p) > len(r) and p[: len(r)] == r


def repos_left(final_or_plan: FinalPlan | ResourcePlan) -> list[dict]:
    """H15: the repositories inside a git item that travels (not ``no``) which stay here — ``{"path", "kind"}`` with
    ``kind`` ``submodule`` (``WHY_SUBMODULE``) or ``nested`` (``WHY_NESTED_REPO``), in path order."""
    rp = final_or_plan.plan if isinstance(final_or_plan, FinalPlan) else final_or_plan
    roots = [i.source_path for i in rp.items if i.kind is ItemKind.GIT_REPO and i.include is not Include.NO]
    kinds = {WHY_SUBMODULE: "submodule", WHY_NESTED_REPO: "nested"}
    out = [
        {"path": n.path, "kind": kinds[n.why]}
        for n in rp.not_transferred
        if n.why in kinds and any(_inside_text(n.path, r) for r in roots)
    ]
    return sorted(out, key=lambda e: e["path"])


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
    "context_over",
    "workspace",
    "outside_items",
    "outside_more",
    "repos_left",
    "repos_left_more",
    "context_unknown",
    "include_refused",
    "include_refused_more",
)
"""The keys of ``presend_summary`` the surfaces read (W125, R30 ``workspace_excluded``, R32 ``context_over``, R36
``workspace``, M37 ``outside_items``/``outside_more``, H15 ``repos_left``/``repos_left_more``, M12
``context_unknown``, L3-F24 ``include_refused``/``include_refused_more``); ``item_list``, ``items`` and
``extras_outside_workspace`` come on top."""


def include_refused(final: FinalPlan, include: Iterable[str], limit: int = PRESEND_PATHS_MAX) -> tuple[list[dict], int]:
    """L3-F24: the paths the user named (``--include``/``--include-secret``, ``include``) that the plan refused —
    ``({"path", "why"}, …)`` from ``not_transferred`` (matched as given, normalised and resolved; a relative path is
    refused as ``WHY_NOT_ABSOLUTE``), in plan order, at most ``limit`` — and how many more. An include the plan took
    (an item, or covered by a workspace) is no refusal."""
    wanted: set[str] = set()
    for raw in include:
        if not isinstance(raw, str) or not raw or "\x00" in raw:
            continue
        wanted.add(raw)
        if os.path.isabs(raw):
            wanted.update({_norm(raw), _real(raw)})
    rows: list[dict] = []
    for n in final.plan.not_transferred:
        if n.path in wanted and not any(r["path"] == n.path for r in rows):
            rows.append({"path": n.path, "why": n.why})
    cap = max(0, limit)
    return rows[:cap], max(0, len(rows) - cap)


def presend_summary(
    final: FinalPlan,
    *,
    withheld_stash: Sequence[str] = (),
    instructions: Sequence[dict] | None = None,
    target_facts: EnvFacts | None = None,
    source_facts: EnvFacts | None = None,
    include: Iterable[str] = (),
) -> dict:
    """The summary every first progress message carries (DESIGN §8.7, SEC 5.8, W125), keyed as the surfaces read
    it (``PRESEND_KEYS``; ``include`` additive — the job's local ``--include`` and ``--include-secret`` paths):

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
      R30) as ``{"path", "reason", "why"}``, else None;
    * ``context_over``: ``{"tokens", "context_length", "model"}`` when the session is larger than the target model's
      window (``context_over``, R32; ``target_facts``/``source_facts`` additive as in ``confirmation_reasons``), else
      None;
    * ``workspace``: the session's workspace against its working folder (``workspace``, R36) as ``{"path", "bytes",
      "cwd", "cwd_bytes", "differs"}``, None when the folder does not travel;
    * ``outside_items``/``outside_more`` (M37): the included items outside the workspace with path and size
      (``outside_items``: at most ``PRESEND_PATHS_MAX``, largest first) and how many more;
    * ``repos_left``/``repos_left_more`` (H15): repositories inside a transferred git workspace that stay here
      (``repos_left``: ``{"path", "kind"}``, at most ``PRESEND_PATHS_MAX``) and how many more;
    * ``context_unknown`` (M12): ``{"tokens", "model"}`` when the facts at hand name a target model whose context
      window is unknown (no ``context_length`` there), so R32 cannot be checked; None when the window is known or
      no target is known yet;
    * ``include_refused``/``include_refused_more`` (L3-F24): the paths of ``include`` the plan refused, ``{"path",
      "why"}`` (``include_refused``: at most ``PRESEND_PATHS_MAX``), and how many more — empty without ``include``.

    ``items`` and ``extras_outside_workspace`` are the layer-1 names of ``items_count`` and ``outside_workspace``,
    kept with the same values for readers that have not moved yet."""
    included = _included(final)
    outside = _outside_workspace(final)
    shown, more = outside_items(final)
    left = repos_left(final)
    refused, refused_more = include_refused(final, include)
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
        "context_over": context_over(final, target_facts=target_facts, source_facts=source_facts),
        "workspace": workspace(final),
        "outside_items": shown,
        "outside_more": more,
        "repos_left": left[:PRESEND_PATHS_MAX],
        "repos_left_more": max(0, len(left) - PRESEND_PATHS_MAX),
        "context_unknown": context_unknown(final, target_facts=target_facts, source_facts=source_facts),
        "include_refused": refused,
        "include_refused_more": refused_more,
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


def _positive_int(value: object) -> int | None:
    return value if isinstance(value, int) and not isinstance(value, bool) and value > 0 else None


def active_context_tokens(rows: Iterable[object], tip_id: str | None = None) -> int:
    """Estimated tokens of the session's active context (R32): what the target's model loads on the first turn —
    the rows of the tip segment (``row["session_id"] == tip_id``; every row without ``tip_id`` or without a session
    id) that a compaction or a rewind did not deactivate (an explicit falsy ``active``; a row without the flag
    counts), content plus tool-call arguments, four characters a token. ``approx_tokens`` (the whole lineage with its
    inactive rows) is an upper bound of this."""
    chars = 0
    for row in rows or ():
        if not isinstance(row, Mapping):
            continue
        sid = row.get("session_id")
        if tip_id and isinstance(sid, str) and sid and sid != tip_id:
            continue
        if "active" in row and not row.get("active"):
            continue
        chars += len(str(row.get("content") or ""))
        calls = row.get("tool_calls")
        if isinstance(calls, str):
            chars += len(calls)
        elif calls:
            chars += len(json.dumps(calls, ensure_ascii=False, default=str))
    return chars // 4


def context_tokens_of(final_or_plan: FinalPlan | ResourcePlan) -> int:
    """The size the window check uses: ``ResourcePlan.context_tokens`` (the source's active-context estimate) when
    the plan carries one, else ``approx_tokens`` (lineage total, an upper bound); never negative."""
    rp = final_or_plan.plan if isinstance(final_or_plan, FinalPlan) else final_or_plan
    estimate = _positive_int(getattr(rp, "context_tokens", None))
    if estimate is not None:
        return estimate
    approx = rp.approx_tokens
    return approx if isinstance(approx, int) and not isinstance(approx, bool) and approx > 0 else 0


def target_window(
    model_change: dict | None, *, target_facts: EnvFacts | None = None, source_facts: EnvFacts | None = None
) -> int | None:
    """The target model's context window in tokens, or None when unknown (R32) — ``note.context_window``, the one
    rule for the plan and the note: a model change (X04) reads ``to.context_length``; a model that stays is the
    target's main model when the source's main model and provider are the same (or the source facts are absent), so
    its window is ``target_facts.host.context_length``."""
    return note.context_window(model_change, target=target_facts, source=source_facts)


def context_over(
    final: FinalPlan, *, target_facts: EnvFacts | None = None, source_facts: EnvFacts | None = None
) -> dict | None:
    """``{"tokens", "context_length", "model"}`` when the session's active context (``context_tokens_of``) is larger
    than the target model's window (``target_window``), else None (R32, L3-F17). ``model`` names the target model
    (``model_change.to.model``, or the target's own model when it stays; None when unknown)."""
    window = target_window(final.model_change, target_facts=target_facts, source_facts=source_facts)
    tokens = context_tokens_of(final)
    if window is None or tokens <= window:
        return None
    to = _model_to(final.model_change)
    host = getattr(target_facts, "host", None)
    name = to.get("model") if to is not None else getattr(host, "model", None)
    return {"tokens": tokens, "context_length": window, "model": _clean(name, 128) if isinstance(name, str) else None}


def context_unknown(
    final: FinalPlan, *, target_facts: EnvFacts | None = None, source_facts: EnvFacts | None = None
) -> dict | None:
    """M12 (named limit): ``{"tokens", "model"}`` when a target model is known — a model change (X04) or the target's
    facts — but its context window is not (``target_window`` None: no ``model.context_length`` in the target's
    ``config.yaml``, or a kept model under another name), so R32's check cannot run and nothing is asked; None when the
    window is known, the session is empty, or no target is known yet (no facts, no model change)."""
    if not isinstance(final.model_change, dict) and target_facts is None:
        return None
    if target_window(final.model_change, target_facts=target_facts, source_facts=source_facts) is not None:
        return None
    tokens = context_tokens_of(final)
    if tokens <= 0:
        return None
    to = _model_to(final.model_change)
    host = getattr(target_facts, "host", None)
    name = to.get("model") if to is not None else getattr(host, "model", None)
    return {"tokens": tokens, "model": _clean(name, 128) if isinstance(name, str) and name else None}


def confirmation_reasons(
    final: FinalPlan,
    options: Options,
    source_offline: bool,
    *,
    instructions: Sequence[dict] = (),
    cfg: PolicyConfig | None = None,
    target_facts: EnvFacts | None = None,
    source_facts: EnvFacts | None = None,
) -> list[str]:
    """Why the user must confirm before anything is locked (DESIGN §14.1 (a)–(g)), in that order.

    ``source_offline``: the source's model route was local. ``instructions`` (additive): the result of
    ``instruction_files`` when they arrive from an unverified bundle or a remote source (reason g).

    Reason (d) comes only from ``cfg.include_secret`` (X07): ``cfg`` (additive) is the source's own
    ``PolicyConfig`` — the conductor passes it when the source is local (push, export); its ``include_secret`` holds
    exactly the paths given by a local ``--include-secret``. ``options.hints.include_secret`` is never read: hints may
    come from the counterpart and never release a secret (R18). Reason (e) reads ``final.model_change`` only in the
    form of contract X04: a local source handed to a model that is not offline-capable. Reason (h)
    ``workspace_excluded`` (R30): the policy leaves the session's working directory behind (``workspace_excluded``)
    and the user did not say ``--no-workspace`` (``options.hints.no_workspace``). Reason (i) ``context_over`` (R32):
    the session's active context is larger than the target model's window (``context_over``; ``target_facts`` and
    ``source_facts`` are additive — the conductor's facts after FACTS; without them only a model change's
    ``to.context_length`` is known). Reason (j) ``workspace_differs`` (R36/R36a, ``workspace_differs``): the workspace
    holds more than the session's working folder (the repository around it) and that folder holds no file the
    repository tracks, or the source could not tell. Reason (k) ``repos_left`` (H15, R30 form): a repository inside a
    transferred git workspace stays here (``repos_left``) — inside an included git item, or a proposed one that
    ``--yes`` takes along (R3-L3). Reason (c) also comes for an included git item whose
    history could not be measured (``SIZE_UNKNOWN``, H13). A blanket yes never confirms (h), (i), (j) or (k)
    (``YES_EXEMPT``)."""
    reasons = []
    if any(i.include is Include.PROPOSED for i in final.plan.items):
        reasons.append(CONFIRM_PROPOSED)
    if any(d.conflict is not None for d in final.decisions):
        reasons.append(CONFIRM_CONFLICT)
    included = _included(final)
    unknown_size = any(SIZE_UNKNOWN in i.reasons for i in included)  # H13: an unmeasured history may be large
    if sum(i.size_estimate for i in included) > options.confirm_above_bytes or unknown_size:
        reasons.append(CONFIRM_LARGE)
    if cfg is not None and cfg.include_secret:
        reasons.append(CONFIRM_SECRET)
    changed = isinstance(final.model_change, dict)
    offline = _model_offline(final.model_change)
    # a changed model without a boolean offline flag counts as not offline-capable: (e) asks rather than stays silent
    if source_offline and changed and offline is not True:
        reasons.append(CONFIRM_MODEL)
    if _outside_workspace(final) or _denied_tracked(final):
        reasons.append(CONFIRM_OUTSIDE)
    if instructions:
        reasons.append(CONFIRM_INSTRUCTIONS)
    if workspace_excluded(final) is not None and options.hints.no_workspace is not True:
        reasons.append(CONFIRM_WORKSPACE_EXCLUDED)
    if context_over(final, target_facts=target_facts, source_facts=source_facts) is not None:
        reasons.append(CONFIRM_CONTEXT)
    if workspace_differs(final):
        reasons.append(CONFIRM_WORKSPACE_DIFFERS)
    if _repos_left_travelling(final, options):
        reasons.append(CONFIRM_REPOS_LEFT)
    return reasons


def _repos_left_travelling(final: FinalPlan, options: Options) -> list[dict]:
    """Reason (k) asks only for a repository whose git item goes along (review R3-L3): an included item, or a
    proposed one that ``--yes`` takes along. Inside a merely proposed item the repositories are named in the summary
    (``repos_left``) under the question about proposals (a); without an answer ("without proposals") nothing of them
    travels, so they do not stop the handover."""
    left = repos_left(final)
    if not left:
        return []
    takes_proposals = options.yes is True
    roots = [
        i.source_path
        for i in final.plan.items
        if i.kind is ItemKind.GIT_REPO
        and (i.include is Include.YES or (i.include is Include.PROPOSED and takes_proposals))
    ]
    return [e for e in left if any(_inside_text(e["path"], r) for r in roots)]


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
    (the session title, shown only inside the note's data block) appear when given; ``context_tokens`` is the size
    of the session's active context (``context_tokens_of``) — the target compares it with its own window and says in
    the note that the first turn compacts (R32).

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
        "context_tokens": context_tokens_of(resource),
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
