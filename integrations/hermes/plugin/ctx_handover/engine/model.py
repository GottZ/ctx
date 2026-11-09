"""Shared data model of ctx_handover (layer L0, stdlib only).

Every value that crosses a module or process boundary is one of these frozen dataclasses or enums
(INTERFACES §3.1). They are pure data: no I/O, no behaviour beyond construction. The wire codec lives in
``jsonutil`` (``to_wire``/``from_wire``: nested dataclasses, enums, tuples, frozensets; unknown keys are
ignored, missing keys take the defaults, wrong types raise ``BadRequest`` with the field path).

Additive rule (INTERFACES §6): fields are only ever added, and only with a default, so an older peer's
document still decodes. A field added after the v1 wire form is declared with ``added_field(default)``: the codec
leaves it out of the wire form while it holds its default, so every document that does not use it — and every id
hashed over such a document (plan ids, object ids) — keeps the bytes it had before the field existed.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum, auto
from types import MappingProxyType
from typing import Any

WIRE_OMIT_DEFAULT = "ctxh.wire_omit_default"
"""Field-metadata key read by ``jsonutil``: the field is omitted from the wire form while it equals its default."""


def added_field(default: Any) -> Any:
    """A dataclass field added after the v1 wire form (contract K-3): ``default`` is its value when absent, and
    the codec omits it from ``to_wire``/``canonical_bytes`` while it equals ``default``. ``default`` must be
    immutable (``None``, ``""``, ``()``, a number, …)."""
    return field(default=default, metadata=MappingProxyType({WIRE_OMIT_DEFAULT: True}))


# ---------------------------------------------------------------------------------------------------------
# Enums (values are the lower-case member names; they are wire values and never change)
# ---------------------------------------------------------------------------------------------------------


class Mode(StrEnum):
    """How a job moves a session (DESIGN §2.2)."""

    PUSH = "push"
    PULL = "pull"
    OFFER = "offer"
    EXPORT = "export"
    IMPORT = "import"


class Role(StrEnum):
    """The part a ledger job row plays on this host (DESIGN §3.1)."""

    CONDUCTOR = "conductor"
    SOURCE = "source"
    TARGET = "target"


class ConductorState(StrEnum):
    """Conductor state machine (DESIGN §3.2)."""

    CREATED = auto()
    HELLO = auto()
    FACTS = auto()
    PLANNING = auto()
    NEGOTIATED = auto()
    LOCKING = auto()
    LOCKED = auto()
    EXPORTING = auto()
    CONTEXT_STAGED = auto()
    PACKING = auto()
    DATA_STAGED = auto()
    TARGET_PREPARING = auto()
    TARGET_PREPARED = auto()
    SOURCE_PREPARING = auto()
    DELTA_APPLYING = auto()
    TARGET_RESETTING = auto()
    SOURCE_PREPARED = auto()
    COMMITTING = auto()
    IN_DOUBT = auto()
    COMMITTED = auto()
    RETIRING = auto()
    RETIRED = auto()
    CONTINUING = auto()
    DONE = auto()
    ABORTING = auto()
    ABORTED = auto()
    SUSPENDED = auto()


class SourceState(StrEnum):
    """Source participant states (DESIGN §3.3); also the ``source_handovers.state`` column."""

    LOCKING = auto()
    LOCKED = auto()
    PREPARING = auto()
    PREPARED = auto()
    RETIRING = auto()
    RETIRED = auto()
    RELEASING = auto()
    RELEASED = auto()
    RECLAIMING = auto()
    RECLAIMED = auto()
    EXPORTED = auto()
    SUSPENDED = auto()


class TargetState(StrEnum):
    """Target participant states (DESIGN §3.3)."""

    OPEN = auto()
    CONTEXT_STAGED = auto()
    NEGOTIATED = auto()
    DATA_STAGED = auto()
    PREPARING = auto()
    PREPARED = auto()
    DELTA_APPLYING = auto()
    RESETTING = auto()
    COMMITTING = auto()
    COMMITTED = auto()
    ABORTING = auto()
    ABORTED = auto()
    EXPIRED = auto()


class ItemKind(StrEnum):
    """What a plan item is (DESIGN §8.2, §9)."""

    GIT_REPO = "git_repo"
    FILE_TREE = "file_tree"
    EXTRA_FILE = "extra_file"
    EXTRA_DIR = "extra_dir"
    HERMES_FILE = "hermes_file"


class ItemRole(StrEnum):
    """Why an item is in the plan (DESIGN §8.2)."""

    PRIMARY = auto()
    SECONDARY = auto()
    EXTRA = auto()
    HERMES = auto()


class Origin(StrEnum):
    """Where the evidence for a candidate came from (DESIGN §8.2–§8.4)."""

    CWD = auto()
    TRANSCRIPT = auto()
    MODEL = auto()
    AGENT = auto()
    USER = auto()


class Weight(StrEnum):
    """Strength of transcript evidence (DESIGN §8.2)."""

    WRITE = "write"
    READ = "read"
    MENTION = "mention"


class Include(StrEnum):
    """Inclusion decision of a plan item (DESIGN §8.4)."""

    YES = "yes"
    PROPOSED = "proposed"
    NO = "no"


class DestMode(StrEnum):
    """How the target materialises an item (DESIGN §9.5, §9.6)."""

    FRESH = "fresh"
    UPDATE = "update"
    RETURN = "return"
    SKIP = "skip"
    NO_CHECKOUT = "no_checkout"


class OpState(StrEnum):
    """State of an asynchronous participant operation (DESIGN §3.7)."""

    RUNNING = "running"
    DONE = "done"
    FAILED = "failed"


class Right(StrEnum):
    """Per-peer right granted at pairing (DESIGN §12.3)."""

    PUSH = "push"
    PULL = "pull"


class GateMode(StrEnum):
    """How a dialer passes the listener's dashboard gate (DESIGN §4.1, R7)."""

    SESSION_TOKEN = "session_token"  # noqa: S105 - mode name, not a secret
    BEARER = "bearer"
    BASIC_LOGIN = "basic_login"


class AuthScheme(StrEnum):
    """Plugin-layer request authentication scheme (DESIGN §4.2, §17.2)."""

    PEER_SIGNATURE = "peer_signature"
    OPERATOR_SESSION = "operator_session"


class PrepareOutcome(StrEnum):
    """Result of the source's prepare re-check (DESIGN §3.4, §6.2, §9.3)."""

    OK = "ok"
    DELTA = "delta"
    STALE_SESSION = "stale_session"
    STALE_WORKTREE = "stale_worktree"


# ---------------------------------------------------------------------------------------------------------
# Hermes and transport facts
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class HermesPaths:
    """The active profile's Hermes home and its document cache (supplied by ``hostcompat.hermes_paths``)."""

    home: str
    document_cache: str


@dataclass(frozen=True)
class Limits:
    """Listener request limits announced in ``hello`` (DESIGN §4.7, Appendix A)."""

    max_request_bytes: int = 8 * 1024**2
    block_size: int = 512 * 1024
    max_ids_per_request: int = 4096
    max_json_bytes: int = 4 * 1024**2
    hosted: bool = False


@dataclass(frozen=True)
class PeerInfo:
    """What the listener tells an authenticated dialer about itself (``hello``)."""

    peer_id: str
    rights: frozenset[Right]


@dataclass(frozen=True)
class Hello:
    """Listener self-description, versions and capabilities (DESIGN §4.3)."""

    protocol: str
    majors: tuple[int, ...]
    minor: int
    instance_id: str
    listener_pubkey: str
    plugin_version: str = ""
    hermes_version: str = ""
    name: str = ""
    capabilities: frozenset[str] = frozenset()
    limits: Limits = Limits()
    pairing_open: bool = False
    peer: PeerInfo | None = None
    server_time: int = 0


@dataclass(frozen=True)
class GateAuth:
    """Dashboard-gate credentials of a dial peer (R7). Secrets are environment-variable NAMES, never values.

    ``extra_headers`` holds ``(header, ENV_NAME)`` pairs for proxies in front of the listener.
    """

    mode: GateMode
    token_env: str = "CTX_HANDOVER_PEER_TOKEN"  # noqa: S105 - env variable name, not a secret
    username: str = ""
    password_env: str = "CTX_HANDOVER_PEER_PASSWORD"  # noqa: S105 - env variable name, not a secret
    extra_headers: tuple[tuple[str, str], ...] = ()


@dataclass(frozen=True)
class Peer:
    """A paired peer as stored in the ledger (DESIGN §12.3). ``direction`` is ``"dial"`` or ``"inbound"``."""

    name: str
    peer_id: str
    direction: str
    url: str | None
    pubkey: str
    rights: frozenset[Right]
    gate: GateAuth | None = None
    profile: str | None = None
    allow_insecure: bool = False
    created_at: float = 0.0
    last_seen_at: float = 0.0
    last_ip: str | None = None
    revoked_at: float | None = None


@dataclass(frozen=True)
class AuthContext:
    """Outcome of request authentication on the listener (DESIGN §4.2)."""

    scheme: AuthScheme
    peer: Peer | None
    rights: frozenset[Right]
    epoch: int | None


# ---------------------------------------------------------------------------------------------------------
# Session
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class SessionRef:
    """Which session a job is about: an id, the latest session, or a claimed offer."""

    id: str | None = None
    latest: bool = False
    offer_id: str | None = None


@dataclass(frozen=True)
class SessionInfo:
    """Resolved source session and lineage (DESIGN §6.1). ``revision`` is the core's opaque revision tuple."""

    tip_id: str
    root_id: str
    lineage_ids: tuple[str, ...]
    title: str | None
    model: str | None
    provider: str | None
    cwd: str | None
    git_repo_root: str | None
    git_branch: str | None
    active_messages: int
    revision: tuple
    revision_changed: bool = False


@dataclass(frozen=True)
class ObjectManifest:
    """A content-addressed object split into blocks (DESIGN §5.2)."""

    sha256: str
    size: int
    block_size: int
    blocks: tuple[str, ...]
    media: str
    object_id: str = ""


@dataclass(frozen=True)
class CapsuleInfo:
    """The exported session capsule and its verification hashes (DESIGN §6.2, §6.9)."""

    capsule_id: str
    size: int
    segments: int
    rows: int
    max_segment_bytes: int
    approx_tokens: int
    h_all: str
    h_rows: str
    h_model: str
    revision: tuple


@dataclass(frozen=True)
class DeltaInfo:
    """Rows appended to the source tip after the capsule was exported (DESIGN §6.2)."""

    delta_id: str
    tip_id: str
    rows: int
    h_all: str
    h_rows: str
    h_model: str
    revision: tuple


# ---------------------------------------------------------------------------------------------------------
# Environment facts
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class ToolInfo:
    """A probed command-line tool (DESIGN §11.1)."""

    path: str | None
    version: str | None


@dataclass(frozen=True)
class HostFacts:
    """Hermes-side facts of this host, supplied by ``hostcompat.host_facts`` (DESIGN §11.1)."""

    is_wsl: bool
    is_container: bool
    profile: str
    multiplex: bool
    hermes_version: str
    plugin_version: str
    model: str | None
    provider: str | None
    context_length: int | None
    terminal_backend: str
    hermes_home: str
    document_cache: str


@dataclass(frozen=True)
class EnvFacts:
    """Deterministic environment facts of one side (DESIGN §11.1). Values of env variables are never collected."""

    instance_name: str
    os_family: str
    os_name: str
    os_version: str
    arch: str
    hostname: str
    user: str
    home: str
    shell: str
    host: HostFacts
    tools: dict[str, ToolInfo]
    tools_seen: dict[str, str | None]
    workspace_root: str
    allow_roots: tuple[str, ...]
    disk_free: dict[str, int]
    case_insensitive: dict[str, bool]
    offline_capable: bool
    locale: str
    timezone: str
    git_version: str | None
    git_lfs: bool
    capabilities: frozenset[str] = frozenset()
    python_version: str = added_field("")
    """``python3 --version`` of this side (DESIGN §11.1 ``python_toolchain``; diagnostics only, W075)."""
    workspace_root_ok: bool | None = added_field(None)
    """R29: this side checked its ``workspace_root`` when it probed its facts. True = usable, False = unusable
    (``workspace_root_problem`` says why), None = not checked (facts not built by ``envfacts.probe``)."""
    workspace_root_problem: str = added_field("")
    """Empty when ``workspace_root_ok`` is True; else one of ``envfacts.ROOT_PROBLEMS`` (D)."""


# ---------------------------------------------------------------------------------------------------------
# Workspace
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class GitInfo:
    """Facts about a source repository collected at planning time (DESIGN §9.3, §9.4)."""

    head: str | None
    branch: str | None
    object_format: str
    shallow: bool
    dirty: bool
    in_progress: str | None
    lfs: bool
    submodules: tuple[str, ...]
    remotes: tuple[tuple[str, str], ...]
    root_commits: tuple[str, ...]
    untracked_bytes: int
    untracked_count: int
    denied_tracked: tuple[str, ...]


@dataclass(frozen=True)
class GitState:
    """A repository's HEAD, index and work-tree state as tree ids (DESIGN §9.3, §9.5 return mode)."""

    head: str
    index_tree: str
    worktree_tree: str
    branch: str | None
    exclude_pathspecs: tuple[str, ...]


# ---------------------------------------------------------------------------------------------------------
# Planning
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class Candidate:
    """Collector output: one resource the session may need (DESIGN §8.2)."""

    kind: ItemKind
    path: str
    weight: Weight
    reasons: tuple[str, ...]
    origin: Origin


@dataclass(frozen=True)
class Refinement:
    """Validated model or agent advice; ``add``/``drop`` hold ``(path_or_id, reason)`` pairs (DESIGN §8.3)."""

    add: tuple[tuple[str, str], ...]
    drop: tuple[tuple[str, str], ...]
    tools_needed: tuple[str, ...]
    env_vars_needed: tuple[str, ...]
    notes: str


@dataclass(frozen=True)
class PlanItem:
    """One item of the resource plan (DESIGN §8.5)."""

    item_id: str
    kind: ItemKind
    source_path: str
    role: ItemRole
    origin: Origin
    include: Include
    reasons: tuple[str, ...]
    size_estimate: int
    git: GitInfo | None = None
    suggested_dest: str | None = None
    rel_cwd: str | None = None
    hermes_kind: str | None = added_field(None)
    """Kind of a ``hermes_file`` item, one of ``policy.HERMES_FILE_KINDS`` (DESIGN §6.11, W003); the target places
    the file by kind and basename only (hardening #7). None for every other item."""
    instruction_files: tuple[tuple[str, str], ...] = added_field(())
    """``(rel, sha256)`` of the agent instruction files inside the item (DESIGN §8.7, SEC 4.3, W073)."""


@dataclass(frozen=True)
class NotTransferred:
    """Something deliberately left behind, with the reason shown in note and report."""

    path: str
    why: str


@dataclass(frozen=True)
class RefineReport:
    """Which refinement step ran and what it changed (DESIGN §8.3)."""

    used: str
    error: str | None = None
    added: int = 0
    dropped: int = 0


@dataclass(frozen=True)
class ResourcePlan:
    """The source's plan document (``ctxh.plan``, DESIGN §8.5)."""

    job_id: str
    revision: tuple
    items: tuple[PlanItem, ...]
    not_transferred: tuple[NotTransferred, ...]
    processes: tuple[dict, ...]
    delegations: tuple[dict, ...]
    cron: tuple[dict, ...]
    refine: RefineReport
    tools_needed: tuple[str, ...]
    env_vars_needed: tuple[str, ...]
    notes_for_target: str
    approx_tokens: int
    warnings: tuple[str, ...] = added_field(())
    """Warning codes of the source side while planning (busy probe, resume guard; W072)."""
    context_tokens: int = added_field(0)
    """Estimated tokens of the session's active context — the tip segment's live rows, what the target's model loads
    on the first turn (``plan.active_context_tokens``; R32, L3-F17). 0 = not estimated (a source before this field):
    the window check then measures the session by ``approx_tokens``, the lineage total."""


@dataclass(frozen=True)
class PlanHints:
    """User hints for planning. ``include_secret`` is honoured only when given locally on the source (R18)."""

    include: tuple[str, ...] = ()
    include_secret: tuple[str, ...] = ()
    exclude: tuple[str, ...] = ()
    dest_overrides: tuple[tuple[str, str], ...] = ()
    no_workspace: bool = False


@dataclass(frozen=True)
class Conflict:
    """Why an item cannot go to its first-choice destination, and where it goes instead (DESIGN §8.6)."""

    kind: str
    detail: str
    fallback_dest: str


@dataclass(frozen=True)
class TargetDecision:
    """The target's decision for one plan item (DESIGN §8.6). ``git_seed`` is reserved and always None in v1."""

    item_id: str
    dest: str
    mode: DestMode
    reason: str
    conflict: Conflict | None = None
    git_have: tuple[str, ...] = ()
    git_seed: None = None
    need_bytes: int = 0
    warnings: tuple[str, ...] = ()


@dataclass(frozen=True)
class FinalPlan:
    """Negotiated plan (DESIGN §8.6); ``plan_id`` is the sha256 of the canonical plan document."""

    plan_id: str
    plan: ResourcePlan
    decisions: tuple[TargetDecision, ...]
    need_bytes: int
    free_bytes: dict[str, int]
    model_change: dict | None


@dataclass(frozen=True)
class PackInfo:
    """The packed working material (``ctxh.pack`` object, DESIGN §9.1)."""

    pack_id: str
    size: int


@dataclass(frozen=True)
class PathMapEntry:
    """Source path → target path; ``kind`` is ``workspace`` | ``extra`` | ``hermes_home`` | ``hermes_file``."""

    source: str
    target: str
    kind: str


@dataclass(frozen=True)
class MapRow:
    """A workspace-map row of this host's ledger (DESIGN §9.7, §9.8). ``direction`` is ``"in"`` or ``"out"``."""

    local_path: str
    peer_id: str
    remote_path: str
    direction: str
    repo_identity: str | None = None
    state: dict | None = None
    last_job_id: str = ""


# ---------------------------------------------------------------------------------------------------------
# Two-phase commit
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class PrepareRequest:
    """What the conductor hands the target's ``prepare`` (DESIGN §3.4)."""

    capsule: CapsuleInfo
    pack: PackInfo
    final_plan_id: str
    source_facts: EnvFacts
    note_parts: dict
    continue_requested: bool = False


@dataclass(frozen=True)
class PreparedInfo:
    """The target's prepared (hidden, verified) session and staged destinations (DESIGN §3.4)."""

    tip_id: str
    root_id: str
    title: str | None
    h_all: str
    h_rows: str
    h_model: str
    import_path: str
    dests: tuple[tuple[str, str, DestMode], ...]
    warnings: tuple[str, ...]
    source_revision: tuple


@dataclass(frozen=True)
class PrepareResult:
    """Outcome of the source's prepare re-check; ``delta`` is set for ``PrepareOutcome.DELTA``."""

    outcome: PrepareOutcome
    delta: DeltaInfo | None = None
    detail: str = ""


@dataclass(frozen=True)
class CommitAck:
    """The target's commit acknowledgement (DESIGN §3.4, §11.6)."""

    session_id: str
    root_id: str
    title: str | None
    dests: tuple[tuple[str, str, DestMode], ...]
    path_map: tuple[PathMapEntry, ...]
    model: str | None
    offline_capable: bool
    warnings: tuple[str, ...]
    committed_at: float


@dataclass(frozen=True)
class ContinueResult:
    """Whether the target started the continuation (DESIGN §11.5)."""

    started: bool
    pid: int | None = None
    log: str | None = None
    via: str = "subprocess"


# ---------------------------------------------------------------------------------------------------------
# Progress and status
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class Progress:
    """A progress report of a long step."""

    step: str
    bytes_done: int = 0
    bytes_total: int = 0
    items_done: int = 0
    items_total: int = 0
    message: str = ""


@dataclass(frozen=True)
class OpStatus:
    """Status of an asynchronous operation; ``result``/``error`` are wire dicts (DESIGN §3.7)."""

    op_id: str
    state: OpState
    result: dict | None = None
    error: dict | None = None
    progress: Progress | None = None


@dataclass(frozen=True)
class ParticipantStatus:
    """A participant's view of a job (``jobs/{id}/status``)."""

    state: str
    step: str
    epoch: int
    progress: Progress | None
    ops: dict[str, OpStatus]
    last_error: dict | None
    notes: tuple[str, ...] = ()


# ---------------------------------------------------------------------------------------------------------
# Options
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class Options:
    """Settings and flags, resolved once at job creation (DESIGN §14.4). Sizes in bytes, times in seconds."""

    dest: str | None = None
    maps: tuple[tuple[str, str], ...] = ()
    hints: PlanHints = PlanHints()
    model: str | None = None
    force: bool = False
    dry_run: bool = False
    yes: bool = False
    wait: bool = False
    wait_timeout_s: int = 1800
    continue_prompt: str | None = None
    keep_source: bool = False
    trust_unsigned: bool = False
    again: bool = False
    as_dir: bool = False
    max_total_bytes: int = 10 * 1024**3
    max_file_bytes: int = 100 * 1024**2
    max_extra_dir_bytes: int = 256 * 1024**2
    max_hermes_file_bytes: int = 64 * 1024**2
    max_files: int = 200_000
    max_untracked_files: int = 200_000
    workspace_root: str = "~/hermes-handover"
    allow_roots: tuple[str, ...] = ("~/hermes-handover",)
    export_roots: tuple[str, ...] = ()
    max_session_bytes: int = 512 * 1024**2
    refine: str = "auto"
    refine_timeout_s: int = 45
    refine_history_chars: int = 60_000
    refine_side_question_max_tokens: int = 200_000
    refine_auto_include: bool = False
    extra_paths: str = "ask"
    include_hermes_files: bool = True
    model_mapping: str = "auto"
    lease_ttl_s: int = 120
    participant_idle_timeout_s: int = 900
    stall_timeout_s: int = 1800
    staging_ttl_s: int = 86_400
    confirm_above_bytes: int = 500 * 1024**2
    extra_ignore: tuple[str, ...] = ()
    tool_probe: tuple[str, ...] = ()
