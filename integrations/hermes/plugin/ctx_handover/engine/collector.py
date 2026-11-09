"""Deterministic resource collector — the floor of resource planning (layer L4, stdlib only; DESIGN §8.2, I1).

No model takes part: the same transcript, session row, filesystem and policy always yield the same sorted
candidate list.

1. **Primary workspace**: the ``cwd`` of the tip row. Inside a git work tree its toplevel is the item root
   (``GIT_REPO``; a shallow or partial clone becomes a ``FILE_TREE`` with a reason, W107 — such repositories cannot
   be bundled with full history), otherwise the directory itself
   (``FILE_TREE``). A primary root that is ``$HOME``, an ancestor of it or a filesystem root is still reported
   (the plan refuses it as a workspace) but never "covers" evidence — otherwise every file under the home would
   silently ride along. A ``cwd`` that git ignores in the work tree around it (``GitProbe.is_ignored``: a scratch or
   build folder of somebody's checkout) is no part of that repository: the ``cwd`` itself is the item root
   (``FILE_TREE``, reason ``IGNORED_REASON``), and evidence in that repository never brings it back as a secondary
   item (R36, L3-F19).
2. **Evidence** from the transcript, walked in storage order with a running directory (start: ``cwd``; a terminal
   result's ``cwd`` field moves it, ``tools/terminal_tool_result.py:247-263``): ``write_file``/``patch`` arguments
   and results (``resolved_path``, ``files_modified``) weigh *write*; ``read_file``/``search_files`` arguments weigh
   *read*; path-like tokens of terminal commands weigh *mention*. Windows sources: MSYS forms (``/c/…``,
   ``/cygdrive/c/…``, ``/mnt/c/…``) become ``C:\\…`` before any filesystem call (BX M13).
3. **Classification after realpath** (SEC 5.3): a missing path is dropped; the realpath decides — inside the
   primary root ⇒ covered (recorded as a reason on the primary); inside another work tree ⇒ a secondary
   ``GIT_REPO``; otherwise an ``EXTRA_FILE``/``EXTRA_DIR`` candidate. Paths in the Hermes home, the plugin data
   directory or the document cache are never extras (session files come from ``hermes_files.discover``); mentions
   outside ``$HOME``/``export_roots`` and kernel pseudo-filesystems are dropped. Whether a candidate may leave the
   machine is decided by the plan through the policy (``plan.build_plan``), which also names refused ones.
4. **Session files** (spillover, hook outputs, native inputs) come from ``hermes_files.discover`` (cluster B) as
   ``HERMES_FILE`` candidates with the reason ``"<kind>: <rel>"``. A referenced file the discovery refuses
   (missing, a symlink, special, too large, a Hermes credential store) is a ``HERMES_FILE`` candidate with the single
   reason ``"rejected: <why>"`` (W040): the plan names it under "not transferred" and never makes it an item.

Size estimates and inclusion defaults are the plan's job (``plan.build_plan``).
"""

from __future__ import annotations

import ntpath
import os
import posixpath
import shlex
import stat
import unicodedata
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass, field

from .contracts import GitProbe
from .errors import BadRequest
from .jsonutil import loads_bounded
from .model import Candidate, HermesPaths, ItemKind, Origin, Weight
from .paths import msys_to_windows
from .policy import HERMES_FILE_KINDS, PolicyConfig, is_denied_name, reserved_workspace_roots

RANK: dict[Weight, int] = {Weight.MENTION: 0, Weight.READ: 1, Weight.WRITE: 2}
"""Order of evidence weights (DESIGN §8.2)."""

WRITE_TOOLS = frozenset({"write_file", "patch"})
READ_TOOLS = frozenset({"read_file", "search_files"})
TERMINAL_TOOLS = frozenset({"terminal"})

CWD_REASON = "cwd"
"""Reason carried by the primary candidate."""

CWD_REL_PREFIX = "cwd: "
"""Reason prefix with the session cwd relative to the primary root (``plan`` reads it into ``rel_cwd``)."""

SHALLOW_REASON = "shallow clone: packed as a file tree"
IGNORED_REASON = "ignored in the git repository around it: packed as a file tree"
"""Reason of a primary whose ``cwd`` git ignores in the enclosing work tree (R36, L3-F19): the ``cwd`` alone travels."""
PARTIAL_REASON = "partial clone: packed as a file tree"
REJECTED_PREFIX = "rejected: "
"""Reason prefix of a session file the discovery refused (``"rejected: <why>"``, ``why`` one of
``hermes_files.discover``'s reasons); read back with ``rejected_reason``."""

MAX_EVIDENCE_PATHS = 20_000
"""Distinct evidence paths kept per session; later mentions beyond it are ignored (reads/writes still count)."""

MAX_REASONS = 20
MAX_GIT_PROBES = 2048
"""Directories probed with git per session; beyond it, further paths are classified without a probe (extras)."""
MAX_ARGS_BYTES = 1024 * 1024
MAX_RESULT_BYTES = 4 * 1024 * 1024
MAX_TOKENS_PER_COMMAND = 64
MAX_PATH_CHARS = 4096
MAX_COMMAND_CHARS = 64 * 1024

_PSEUDO_FS = ("/proc", "/sys", "/dev", "/run")
_KIND_ORDER = {
    ItemKind.GIT_REPO: 0,
    ItemKind.FILE_TREE: 1,
    ItemKind.EXTRA_DIR: 2,
    ItemKind.EXTRA_FILE: 3,
    ItemKind.HERMES_FILE: 4,
}
_REDIRECT_PREFIXES = ("2>>", "&>>", "2>", "&>", ">>", ">", "<")


@dataclass(frozen=True)
class FsOps:
    """The filesystem calls of the collector (injectable so a Windows source can be exercised on any host)."""

    lstat: Callable[[str], os.stat_result] = os.lstat
    realpath: Callable[[str], str] = os.path.realpath


def sort_key(candidate: Candidate) -> tuple:
    """Deterministic order: the primary first, then by kind (repos, trees, dirs, files, session files), then path."""
    return (0 if candidate.origin is Origin.CWD else 1, _KIND_ORDER[candidate.kind], candidate.path)


# ---------------------------------------------------------------------------------------------------------
# Path flavour of the source
# ---------------------------------------------------------------------------------------------------------


class _Flavour:
    """POSIX or Windows path arithmetic for the source OS (independent of the host the tests run on)."""

    def __init__(self, source_os: str, case_insensitive: bool) -> None:
        family = (source_os or "").strip().lower()
        self.windows = family in {"windows", "win32", "nt", "cygwin", "msys"}
        self.mod = ntpath if self.windows else posixpath
        self.ci = bool(case_insensitive or self.windows)

    def norm(self, path: str) -> str:
        return self.mod.normpath(path)

    def isabs(self, path: str) -> bool:
        if self.windows:
            drive, rest = ntpath.splitdrive(path)
            return bool(drive) and len(drive) == 2 and rest.startswith(("\\", "/"))
        return path.startswith("/")

    def parts(self, path: str) -> tuple[str, ...]:
        if self.windows:
            drive, rest = ntpath.splitdrive(path)
            comps = [c for c in rest.replace("/", "\\").split("\\") if c]
            out = (drive.upper(), *comps)
        else:
            out = tuple(c for c in path.split("/") if c)
        if self.ci:
            return tuple(unicodedata.normalize("NFC", unicodedata.normalize("NFC", c).casefold()) for c in out)
        return tuple(unicodedata.normalize("NFC", c) for c in out)

    def under(self, path: str, root: str) -> bool:
        p, r = self.parts(path), self.parts(root)
        return len(p) >= len(r) and p[: len(r)] == r

    def rel(self, path: str, root: str) -> str:
        """``path`` relative to ``root`` as a ``/``-separated string (``""`` for the root itself)."""
        raw = (
            [c for c in path.replace("\\", "/").split("/") if c] if self.windows else [c for c in path.split("/") if c]
        )
        depth = len(self.parts(root))
        if self.windows:
            raw = raw[1:] if raw and raw[0].endswith(":") else raw
            depth -= 1
        return "/".join(raw[depth:])

    def join(self, base: str, rel: str) -> str:
        return self.mod.join(base, rel)

    def parent(self, path: str) -> str:
        return self.mod.dirname(path)

    def is_root(self, path: str) -> bool:
        return len(self.parts(path)) <= (1 if self.windows else 0)


# ---------------------------------------------------------------------------------------------------------
# Transcript parsing
# ---------------------------------------------------------------------------------------------------------


def _json_dict(raw: object, max_bytes: int) -> dict | None:
    if isinstance(raw, dict):
        return raw
    if not isinstance(raw, str):
        return None
    text = raw.strip()
    if not text.startswith("{"):
        return None
    data = text.encode("utf-8", errors="replace")
    if len(data) > max_bytes:
        return None
    try:
        value = loads_bounded(data, max_bytes=max_bytes)
    except BadRequest:
        return None
    return value if isinstance(value, dict) else None


def tool_calls(raw: object) -> list[tuple[str, str, dict]]:
    """``(call_id, tool name, arguments)`` of a stored ``messages.tool_calls`` value (list or JSON string)."""
    if isinstance(raw, str):
        try:
            raw = loads_bounded(raw.encode("utf-8", errors="replace"), max_bytes=MAX_RESULT_BYTES)
        except BadRequest:
            return []
    if not isinstance(raw, list):
        return []
    out: list[tuple[str, str, dict]] = []
    for call in raw:
        if not isinstance(call, dict):
            continue
        fn = call.get("function") if isinstance(call.get("function"), dict) else {}
        name = fn.get("name") or call.get("name")
        if not isinstance(name, str):
            continue
        args = _json_dict(fn.get("arguments", call.get("arguments")), MAX_ARGS_BYTES) or {}
        call_id = call.get("id") if isinstance(call.get("id"), str) else ""
        out.append((call_id, name, args))
    return out


def _command_tokens(command: str) -> list[str]:
    command = command[:MAX_COMMAND_CHARS]
    try:
        tokens = shlex.split(command, comments=False, posix=True)
    except ValueError:
        tokens = command.split()
    out: list[str] = []
    for token in tokens:
        for prefix in _REDIRECT_PREFIXES:
            if token.startswith(prefix):
                token = token[len(prefix) :]
                break
        if "=" in token and (token.startswith("-") or not token.startswith(("/", "~", "."))):
            token = token.split("=", 1)[1]
        token = token.rstrip(";,)|&").lstrip("(")
        if not token or token.startswith("-") or "://" in token or "$" in token or "`" in token:
            continue
        if token.startswith(("~", "/", "./", "../")) or "/" in token or "\\" in token or "." in token:
            out.append(token)
        if len(out) >= MAX_TOKENS_PER_COMMAND:
            break
    return out


# ---------------------------------------------------------------------------------------------------------
# Evidence walk
# ---------------------------------------------------------------------------------------------------------


@dataclass
class _Evidence:
    weight: Weight
    tools: set[str] = field(default_factory=set)


class _Walk:
    def __init__(self, flav: _Flavour, home: str, cwd: str | None) -> None:
        self.flav = flav
        self.home = home
        self.running = cwd
        self.found: dict[str, _Evidence] = {}

    def resolve(self, token: object, base: str | None) -> str | None:
        if not isinstance(token, str) or not token or "\x00" in token or len(token) > MAX_PATH_CHARS:
            return None
        text = token.strip()
        if self.flav.windows and text.startswith("/"):
            text = msys_to_windows(text)
        if text == "~":
            text = self.home
        elif text.startswith(("~/", "~\\")):
            text = self.flav.join(self.home, text[2:])
        elif text.startswith("~"):
            return None
        if not self.flav.isabs(text):
            if not base:
                return None
            text = self.flav.join(base, text)
        return self.flav.norm(text)

    def add(self, token: object, weight: Weight, tool: str, base: str | None) -> None:
        path = self.resolve(token, base)
        if path is None:
            return
        ev = self.found.get(path)
        if ev is None:
            if len(self.found) >= MAX_EVIDENCE_PATHS and weight is Weight.MENTION:
                return
            ev = self.found[path] = _Evidence(weight)
        elif RANK[weight] > RANK[ev.weight]:
            ev.weight = weight
        ev.tools.add(tool)

    def call(self, name: str, args: dict) -> None:
        if name in WRITE_TOOLS:
            self.add(args.get("path"), Weight.WRITE, name, self.running)
        elif name in READ_TOOLS:
            self.add(args.get("path", "." if name == "search_files" else None), Weight.READ, name, self.running)
        elif name in TERMINAL_TOOLS:
            command = args.get("command")
            if not isinstance(command, str):
                return
            workdir = self.resolve(args.get("workdir"), self.running) if args.get("workdir") else None
            base = workdir or self.running
            for token in _command_tokens(command):
                self.add(token, Weight.MENTION, name, base)

    def result(self, name: str, content: object) -> None:
        if name not in WRITE_TOOLS and name not in TERMINAL_TOOLS:
            return
        result = _json_dict(content, MAX_RESULT_BYTES)
        if result is None:
            return
        if name in WRITE_TOOLS:
            self.add(result.get("resolved_path"), Weight.WRITE, name, self.running)
            modified = result.get("files_modified")
            if isinstance(modified, list):
                for path in modified[:MAX_TOKENS_PER_COMMAND]:
                    self.add(path, Weight.WRITE, name, self.running)
        else:
            moved = result.get("cwd")
            if isinstance(moved, str) and moved:
                resolved = self.resolve(moved, self.running)
                if resolved is not None:
                    self.running = resolved


def _walk(rows: Sequence[dict], walk: _Walk) -> list[str]:
    """Feed ``rows`` (storage order) into ``walk``; returns the lineage session ids in order of appearance."""
    pending: dict[str, str] = {}
    lineage: list[str] = []
    for row in rows:
        if not isinstance(row, dict):
            continue
        sid = row.get("session_id")
        if isinstance(sid, str) and sid and sid not in lineage:
            lineage.append(sid)
        role = row.get("role")
        if role == "assistant":
            for call_id, name, args in tool_calls(row.get("tool_calls")):
                if call_id:
                    pending[call_id] = name
                walk.call(name, args)
        elif role == "tool":
            name = row.get("tool_name")
            if not isinstance(name, str) or not name:
                name = pending.get(str(row.get("tool_call_id") or ""), "")
            walk.result(name, row.get("content"))
    return lineage


# ---------------------------------------------------------------------------------------------------------
# Classification
# ---------------------------------------------------------------------------------------------------------


@dataclass
class _Item:
    kind: ItemKind
    path: str
    origin: Origin
    weight: Weight
    head: list[str] = field(default_factory=list)
    evidence: list[tuple[str, Weight]] = field(default_factory=list)
    tools: set[str] = field(default_factory=set)

    def note(self, rel: str, weight: Weight, tools: Iterable[str]) -> None:
        self.evidence.append((rel, weight))
        self.tools.update(tools)
        if RANK[weight] > RANK[self.weight]:
            self.weight = weight

    def reasons(self) -> tuple[str, ...]:
        out = list(self.head)
        evidence = sorted(self.evidence)
        tools = ", ".join(sorted(self.tools))
        for rel, weight in evidence[:MAX_REASONS]:
            out.append(f"{weight.value}: {rel}" if rel else f"{weight.value} ({tools})")
        if len(evidence) > MAX_REASONS:
            out.append(f"… {len(evidence) - MAX_REASONS} more")
        return tuple(dict.fromkeys(out)) or (self.weight.value,)


class _Classifier:
    def __init__(self, flav: _Flavour, fs: FsOps, cfg: PolicyConfig, hermes: HermesPaths, git: GitProbe) -> None:
        self.flav, self.fs, self.git = flav, fs, git
        self.home = self._real(str(cfg.home))
        self.export_roots = [self._real(str(r)) for r in cfg.export_roots]
        protected = [cfg.hermes_home, cfg.hermes_root, cfg.plugin_dir, hermes.home, hermes.document_cache]
        if cfg.document_cache is not None:
            protected.append(cfg.document_cache)
        self.protected: list[str] = []
        for p in protected:
            if p is None or str(p) == "":
                continue
            for q in (flav.norm(str(p)), self._real(str(p))):
                if q not in self.protected:
                    self.protected.append(q)
        # R29/V46: the directory the plugin reserves in the Hermes home is a source like any workspace; it lifts only
        # the Hermes home and root (never the plugin data dir or the document cache), and only in the lexical form
        # and below the resolved Hermes home — never through the realpath of the reserved directory itself
        self.reserved = [flav.norm(str(r)) for r in reserved_workspace_roots(cfg)]
        self.liftable = {
            q
            for p in (cfg.hermes_home, cfg.hermes_root, hermes.home)
            if p is not None and str(p) != ""
            for q in (flav.norm(str(p)), self._real(str(p)))
        }
        self._toplevel: dict[str, str | None] = {}

    def _real(self, path: str) -> str:
        try:
            return self.flav.norm(self.fs.realpath(path))
        except (OSError, ValueError):
            return self.flav.norm(path)

    def lstat(self, path: str) -> os.stat_result | None:
        try:
            return self.fs.lstat(path)
        except (OSError, ValueError):
            return None

    def is_dir(self, path: str) -> bool:
        st = self.lstat(path)
        return st is not None and stat.S_ISDIR(st.st_mode)

    def home_like(self, path: str) -> bool:
        """``$HOME``, an ancestor of it, or a filesystem root — never a workspace, never an extra."""
        return self.flav.is_root(path) or self.flav.under(self.home, path)

    def hermes_owned(self, path: str) -> bool:
        """Inside the Hermes root, home, plugin data or document cache — except the reserved workspace directory
        (``<HERMES_HOME>/handover-workspaces``, R29/V46), which only lifts the Hermes home and root."""
        reserved = any(self.flav.under(path, r) for r in self.reserved)
        return any(self.flav.under(path, p) for p in self.protected if not (reserved and p in self.liftable))

    def pseudo_fs(self, path: str) -> bool:
        return not self.flav.windows and any(path == p or path.startswith(p + "/") for p in _PSEUDO_FS)

    def exportable(self, path: str) -> bool:
        return any(self.flav.under(path, r) for r in (self.home, *self.export_roots))

    def toplevel(self, directory: str) -> str | None:
        if directory in self._toplevel:
            return self._toplevel[directory]
        if len(self._toplevel) >= MAX_GIT_PROBES:
            return None
        try:
            top = self.git.toplevel(directory)
        except Exception:  # a broken probe never stops planning; the path is then an extra
            top = None
        result = self._real(top) if isinstance(top, str) and top else None
        self._toplevel[directory] = result
        return result

    def ignored(self, directory: str) -> bool:
        """``directory`` is ignored in the work tree around it (R36). Only an explicit ``False`` of the probe reads as
        not ignored; an exception or any other answer counts as ignored — the workspace then stays the directory and
        never grows to a repository it may not be part of."""
        try:
            return self.git.is_ignored(directory) is not False
        except Exception:  # an unclear answer: fail closed (the directory alone travels)
            return True

    def shallow(self, root: str) -> bool:
        try:
            return bool(self.git.is_shallow(root))
        except Exception:  # unknown ⇒ treat as a full clone; the pack step re-checks with git itself
            return False

    def partial(self, root: str) -> bool:
        """A partial clone (promisor remote, W107; ``contracts.GitProbe.is_partial``, V30). A failing probe counts
        as complete — the pack step re-checks and refuses a partial clone itself."""
        try:
            return bool(self.git.is_partial(root))
        except Exception:  # unknown ⇒ complete, as above
            return False

    def flat_reason(self, root: str) -> str | None:
        """Why the repository at ``root`` travels as a file tree (shallow or partial clone), or None."""
        if self.shallow(root):
            return SHALLOW_REASON
        if self.partial(root):
            return PARTIAL_REASON
        return None


def collect(
    rows: Iterable[dict],
    session_row: dict,
    hermes: HermesPaths,
    cfg: PolicyConfig,
    git: GitProbe,
    *,
    source_os: str,
    discover: Callable | None = None,
    fs: FsOps | None = None,
) -> list[Candidate]:
    """Deterministic, sorted candidate list of one session (DESIGN §8.2).

    ``rows`` are the capsule-equivalent message rows of all segments in storage order (as
    ``export_session_lineage(include_inactive=True)`` returns them; an iterator is materialised once because the
    session-file discovery reads the rows too). ``session_row`` is the tip's ``sessions`` row. ``discover`` is
    ``hermes_files.discover`` (cluster B; resolved lazily when None) and is called with ``rejected=<list>``: refused
    session files become ``"rejected: <why>"`` candidates (W040). ``fs`` replaces ``os.lstat``/``realpath``
    (tests of other source platforms). Additive keywords over INTERFACES §4.2: ``discover``, ``fs``.
    """
    rows = rows if isinstance(rows, Sequence) else list(rows)
    fs = fs or FsOps()
    flav = _Flavour(source_os, cfg.case_insensitive)
    cls = _Classifier(flav, fs, cfg, hermes, git)
    items: dict[str, _Item] = {}

    primary: _Item | None = None
    covering: str | None = None
    enclosing: str | None = None  # R36: the repository around a cwd it ignores (never an item)
    cwd_raw = session_row.get("cwd") if isinstance(session_row, dict) else None
    cwd: str | None = None
    if isinstance(cwd_raw, str) and cwd_raw.strip():
        cwd_text = msys_to_windows(cwd_raw.strip()) if flav.windows and cwd_raw.startswith("/") else cwd_raw.strip()
        if flav.isabs(cwd_text):
            cwd = flav.norm(cwd_text)
    if cwd is not None and cls.is_dir(cwd):
        real_cwd = cls._real(cwd)
        top = cls.toplevel(real_cwd) if not cls.home_like(real_cwd) else None
        if top is not None and cls.home_like(top):
            top = None
        head = [CWD_REASON]
        if top is not None and flav.parts(top) != flav.parts(real_cwd) and cls.ignored(real_cwd):
            enclosing, top = top, None
            head.append(IGNORED_REASON)
        if top is not None:
            root = top
            flat = cls.flat_reason(root)
            kind = ItemKind.FILE_TREE if flat else ItemKind.GIT_REPO
            if flat:
                head.append(flat)
        else:
            root, kind = real_cwd, ItemKind.FILE_TREE
        rel = flav.rel(real_cwd, root) if flav.under(real_cwd, root) else ""
        if rel:
            head.append(CWD_REL_PREFIX + rel)
        primary = _Item(kind, root, Origin.CWD, Weight.MENTION, head=head)
        items[root] = primary
        if not cls.home_like(root):
            covering = root

    walk = _Walk(flav, cls.home, cwd)
    lineage = _walk(rows, walk)

    repo_roots: list[str] = []
    for path in sorted(walk.found):
        ev = walk.found[path]
        if cls.lstat(path) is None:
            continue
        real = cls._real(path)
        if cls.pseudo_fs(real) or cls.home_like(real) or cls.hermes_owned(real):
            continue
        if (
            real != path
            and is_denied_name(path, case_insensitive=flav.ci)
            and not is_denied_name(real, case_insensitive=flav.ci)
        ):
            # a link spelled like a secret (``~/proj/.env -> config.txt``): keep the spelling, the plan refuses it
            item = items.get(path) or items.setdefault(
                path,
                _Item(
                    ItemKind.EXTRA_DIR if cls.is_dir(real) else ItemKind.EXTRA_FILE, path, Origin.TRANSCRIPT, ev.weight
                ),
            )
            item.note("", ev.weight, ev.tools)
            continue
        if covering is not None and real != covering and flav.under(covering, real):
            continue  # an ancestor of the workspace: copying it would duplicate the workspace
        if covering is not None and flav.under(real, covering):
            rel = flav.rel(real, covering)
            if rel:
                primary.note(rel, ev.weight, ev.tools)
            continue
        owner = next((r for r in repo_roots if flav.under(real, r)), None)
        exportable = cls.exportable(real)
        if owner is None and exportable:
            probe_dir = real if cls.is_dir(real) else flav.parent(real)
            top = cls.toplevel(probe_dir)
            if top is not None and enclosing is not None and covering is not None and flav.under(covering, top):
                top = None  # R36: a repository around a cwd it ignores never comes back; its files are single extras
            if top is not None and not cls.home_like(top) and not (covering and flav.under(top, covering)):
                owner = top
                repo_roots.append(top)
                if top not in items:
                    flat = cls.flat_reason(top)
                    kind = ItemKind.FILE_TREE if flat else ItemKind.GIT_REPO
                    items[top] = _Item(kind, top, Origin.TRANSCRIPT, ev.weight, head=[flat] if flat else [])
        if owner is not None:
            items[owner].note(flav.rel(real, owner), ev.weight, ev.tools)
            continue
        if not exportable and ev.weight is Weight.MENTION:
            continue
        kind = ItemKind.EXTRA_DIR if cls.is_dir(real) else ItemKind.EXTRA_FILE
        item = items.get(real)
        if item is None:
            item = items[real] = _Item(kind, real, Origin.TRANSCRIPT, ev.weight)
        item.note("", ev.weight, ev.tools)

    out = [
        Candidate(kind=i.kind, path=i.path, weight=i.weight, reasons=i.reasons(), origin=i.origin)
        for i in items.values()
    ]
    tip_id = session_row.get("id") if isinstance(session_row, dict) else None
    if isinstance(tip_id, str) and tip_id and tip_id not in lineage:
        lineage.append(tip_id)
    if discover is None:
        from .hermes_files import discover as discover_files

        discover = discover_files
    seen = {c.path for c in out}
    rejected: list[tuple[str, str]] = []
    for f in discover(rows, lineage, hermes, cfg, rejected=rejected):
        source_path = getattr(f, "source_path", None)
        kind = getattr(f, "kind", None)
        if not isinstance(source_path, str) or not isinstance(kind, str) or source_path in seen:
            continue
        seen.add(source_path)
        rel = getattr(f, "rel", "") or ""
        out.append(Candidate(ItemKind.HERMES_FILE, source_path, Weight.READ, (f"{kind}: {rel}",), Origin.TRANSCRIPT))
    for entry in rejected[:MAX_EVIDENCE_PATHS]:
        if not (isinstance(entry, tuple) and len(entry) == 2):
            continue
        source_path, why = entry
        if not isinstance(source_path, str) or not source_path or source_path in seen:
            continue
        seen.add(source_path)
        why = why if isinstance(why, str) and why.isidentifier() else "hermes"
        out.append(
            Candidate(ItemKind.HERMES_FILE, source_path, Weight.READ, (f"{REJECTED_PREFIX}{why}",), Origin.TRANSCRIPT)
        )
    return sorted(out, key=sort_key)


def rejected_reason(reasons: Sequence[str]) -> str | None:
    """Why the session-file discovery refused a candidate (``"rejected: <why>"``), or None for an accepted one."""
    for reason in reasons:
        if isinstance(reason, str) and reason.startswith(REJECTED_PREFIX):
            return reason[len(REJECTED_PREFIX) :] or "hermes"
    return None


def hermes_kind(candidate_or_reasons: Candidate | Sequence[str]) -> str | None:
    """The session-file kind recorded in a ``HERMES_FILE`` candidate's or item's reasons (``"<kind>: <rel>"``)."""
    reasons = candidate_or_reasons.reasons if isinstance(candidate_or_reasons, Candidate) else candidate_or_reasons
    for reason in reasons:
        kind, sep, _rel = str(reason).partition(": ")
        if sep and kind in HERMES_FILE_KINDS:
            return kind
    return None


def rel_cwd(reasons: Sequence[str]) -> str | None:
    """The session cwd relative to the primary root, from the primary's reasons (``"cwd: <rel>"``)."""
    for reason in reasons:
        if isinstance(reason, str) and reason.startswith(CWD_REL_PREFIX):
            rel = reason[len(CWD_REL_PREFIX) :]
            return rel or None
    return None
