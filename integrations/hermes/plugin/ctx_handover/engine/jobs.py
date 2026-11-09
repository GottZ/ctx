"""Composition roots, the job runner and the janitor (layer L9; INTERFACES §4.5, DESIGN §2.1, §3.6, §14.1).

This is the only place where the engine is wired to Hermes (through ``hostcompat``) and to the transport
(cluster C). Three roots:

* ``make_local_apis`` — ``LocalSource``/``LocalTarget`` for this profile (slash handler, CLI, routes);
* ``apis_for`` — the ``SourceAPI``/``TargetAPI`` pair of a mode: local, remote (``remote.Remote*`` over a
  ``wire.WireClient``) or bundle (``bundle.Bundle*``);
* ``listener_server`` — the dashboard routes' ``server.Server`` with its ``ParticipantRegistry``.

Everything is lazy and per profile: nothing runs at import or in ``register()`` (R12, BX L2). The first real use
in a process (``ensure_started``) returns the profile's one ``Runtime`` (X19): one ``Ledger``, one ``VetoIndex``
(sharing that ledger), one ``BlockStore``, one ``JobRunner``; it runs the janitor once. The janitor runs again
before every new job and on ``status``; its block-store GC (DESIGN §5.1, R28) at most every ``GC_INTERVAL_S``
across all processes of the profile.

``JobRunner`` owns conductor jobs of this process: ``plan_sync`` runs CREATED … NEGOTIATED in the caller (the
slash handler can ask the user), ``continue_async`` hands the job to a worker thread that runs in a copy of the
caller's context (profile-scoped helpers resolve correctly there, DESIGN §2.1 consequence 6).
"""

from __future__ import annotations

import contextlib
import contextvars
import functools
import json
import logging
import os
import platform
import secrets
import shutil
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from types import SimpleNamespace

from . import hostcompat
from . import settings as settings_mod
from .conductor import Conductor, PollPolicy, error_from, unexpected
from .contracts import JobContext, Reporter, SourceAPI, TargetAPI
from .errors import (
    BadRequest,
    Cancelled,
    Forbidden,
    GateRejected,
    HandoverError,
    InDoubt,
    InvalidState,
    NotFound,
    SessionBusy,
    Unauthorized,
    WaitTimeout,
)
from .ids import short, ulid
from .jsonutil import from_wire, to_wire
from .ledger import JOURNAL_KEEP_S, STALE_OWNER_S, TERMINAL, Ledger, private_dir, process_owner
from .local_source import LocalSource, purge_source_staging
from .local_target import LocalTarget, purge_reports
from .model import (
    ConductorState,
    EnvFacts,
    FinalPlan,
    Mode,
    Options,
    Peer,
    Progress,
    Refinement,
    Role,
    SessionRef,
)
from .ops import OpRunner
from .participant import MAX_JOBS, MAX_JOBS_PER_PEER, ParticipantRegistry
from .policy import PolicyConfig

PLUGIN_ID = "ctx_handover"
log = logging.getLogger("ctx_handover.jobs")
OFFER_TTL_S = 2 * 3600
MAINTENANCE_S = 10.0
GC_INTERVAL_S = 3600.0
"""The janitor's block-store GC runs at most this often per profile (claimed in the ledger across processes)."""
GC_META_KEY = "janitor_gc_at"

EXIT_OK, EXIT_FAILED, EXIT_USAGE, EXIT_BUSY, EXIT_AUTH, EXIT_PENDING = 0, 1, 2, 3, 4, 5
_STOP = frozenset({"done", "aborted", "suspended"})
_PARTICIPANT_ROLES = (Role.TARGET, Role.SOURCE)
_UNLOCKED = tuple(
    str(s)
    for s in (
        ConductorState.CREATED,
        ConductorState.HELLO,
        ConductorState.FACTS,
        ConductorState.PLANNING,
        ConductorState.NEGOTIATED,
    )
)
"""Conductor states before LOCKING: nothing is locked anywhere yet."""
_ACKED = frozenset({"committed", "retiring", "retired", "continuing", "done"})
"""Conductor states after the commit point: the target's ack is journaled (``ctx.ack``) from COMMITTED on."""
_PARTICIPANT_OK = frozenset({"committed", "retired", "exported"})
"""Participant end states of a handover that went through (target committed, source retired or exported)."""
RECLAIM_SETTLE_S = 15.0
"""How long ``reclaim`` waits for this process's runner of an export to settle it (V39)."""
CANCEL_SETTLE_S = 15.0
"""How long ``cancel`` of a SUSPENDED job waits for its abort before it answers (L3-F07)."""


# ---------------------------------------------------------------------------------------------------------
# settings → options (DESIGN §14.4)
# ---------------------------------------------------------------------------------------------------------


def plugin_settings(config: dict) -> dict:
    """``plugins.entries.ctx_handover.settings`` of a loaded config through the one normaliser (contract V22,
    ``settings.normalize``): every schema key, YAML ``on``/``off`` words per type, ``None`` and any rejected value
    (wrong type, outside the choices, negative, not finite) are the schema default."""
    return settings_mod.normalize(settings_mod.raw_settings(config))


_MIB, _GIB = 1024**2, 1024**3


def local_options(settings: dict) -> Options:
    """The local defaults of a job from the plugin settings (the participant side's own limits, §14.4). The input
    goes through ``settings.normalize`` again (idempotent), so a raw mapping cannot bring an unchecked value into
    ``Options``; an empty ``allow_roots`` stays empty (fail-closed, the surfaces show the same)."""
    s = settings_mod.normalize(settings)
    return Options(
        workspace_root=str(s["workspace_root"]),
        allow_roots=tuple(s["allow_roots"]),
        export_roots=tuple(s["export_roots"]),
        max_session_bytes=int(s["max_session_mb"] * _MIB),
        max_file_bytes=int(s["max_file_mb"] * _MIB),
        max_extra_dir_bytes=int(s["max_extra_dir_mb"] * _MIB),
        max_hermes_file_bytes=int(s["max_hermes_file_mb"] * _MIB),
        max_files=int(s["max_files"]),
        max_untracked_files=int(s["max_untracked_files"]),
        max_total_bytes=int(s["max_total_gb"] * _GIB),
        confirm_above_bytes=int(s["confirm_above_mb"] * _MIB),
        staging_ttl_s=int(s["staging_ttl_hours"] * 3600),
        lease_ttl_s=int(s["lease_ttl_s"]),
        participant_idle_timeout_s=int(s["participant_idle_timeout_min"] * 60),
        stall_timeout_s=int(s["stall_timeout_min"] * 60),
        wait_timeout_s=int(s["wait_timeout_min"] * 60),
        refine=str(s["refine"]),
        refine_timeout_s=int(s["refine_timeout_s"]),
        refine_history_chars=int(s["refine_history_chars"]),
        refine_side_question_max_tokens=int(s["refine_side_question_max_tokens"]),
        refine_auto_include=s["refine_auto_include"] is True,
        extra_paths=str(s["extra_paths"]),
        include_hermes_files=s["include_hermes_files"] is True,
        model_mapping=str(s["model_mapping"]),
        extra_ignore=tuple(s["extra_ignore"]),
        tool_probe=tuple(s["tool_probe"]),
    )


# ---------------------------------------------------------------------------------------------------------
# per-profile process state (lazy; never touched by register())
# ---------------------------------------------------------------------------------------------------------


@dataclass(eq=False)
class Runtime:
    """The engine runtime of one profile in one process (X19, W109): exactly one ``ledger``, ``veto``
    (``lock.VetoIndex`` over that same ledger), ``store`` (``blocks.BlockStore``) and ``runner`` (``JobRunner``) —
    the surfaces, hooks, routes and the CLI use these and build none of their own. ``janitor()`` runs DESIGN §3.6 for
    this profile. Created lazily by ``ensure_started``/``runner_for``/``listener_server``, never by ``register()``.

    ``identity`` is cached; it is reloaded when the ledger's identity fingerprint changes (``/handover pair`` or
    ``identity rotate`` in another process of the profile). ``ctx`` is the latest plugin context that reached
    the runtime (for the ctx-records outbox)."""

    data_dir: Path
    ledger: Ledger
    ops: OpRunner
    store: object | None = None
    git: object | None = None
    identity: object | None = None
    identity_error: str | None = None
    identity_fp: str | None = None
    veto: object | None = None
    registry: ParticipantRegistry | None = None
    runner: JobRunner | None = None
    server: object | None = None
    server_identity: object | None = None
    ctx: object | None = None
    janitor_ran: bool = False
    maintenance: threading.Thread | None = None
    outbox_thread: threading.Thread | None = None
    stopping: threading.Event = field(default_factory=threading.Event)
    db_factories: dict = field(default_factory=dict)
    lock: threading.RLock = field(default_factory=threading.RLock)

    def janitor(self, *, background: bool = True) -> dict:
        """DESIGN §3.6 for this profile (expired codes, journal compaction, stale jobs, outbox) plus the block-store
        GC of DESIGN §5.1/R28 (blocks and manifests of finished and aborted jobs), the latter at most every
        ``GC_INTERVAL_S``. Never raises for what it cleans up."""
        return janitor(
            self.ledger,
            self.runner,
            _registry(self),
            flush_outbox=_flush(self.ctx),
            background=background,
            store=_store(self),
            data_dir=self.data_dir,
        )

    def before_job(self) -> dict:
        """``before_job`` of every job (review M29): the janitor without the ctx outbox flush, which runs in a daemon
        thread ``ctxh-outbox`` (one at a time) — ``call_mcp`` waits up to 10 s per row, and ctx is never on the job's
        critical path (D12, DESIGN §13)."""
        done = janitor(
            self.ledger,
            self.runner,
            _registry(self),
            flush_outbox=None,
            background=True,
            store=_store(self),
            data_dir=self.data_dir,
        )
        flush = _flush(self.ctx)
        if flush is not None:
            self.flush_in_background(flush)
        return done

    def flush_in_background(self, flush: Callable[[Ledger], object]) -> None:
        """Start the outbox flush in ``ctxh-outbox`` unless one is still running (it continues the same rows)."""
        with self.lock:
            if self.outbox_thread is not None and self.outbox_thread.is_alive():
                return
            ledger = self.ledger

            def run() -> None:
                try:
                    flush(ledger)
                except Exception:  # best effort: the rows stay due for the next flush
                    log.warning("ctx_handover outbox flush failed", exc_info=True)

            ctx = contextvars.copy_context()
            self.outbox_thread = threading.Thread(target=ctx.run, args=(run,), name="ctxh-outbox", daemon=True)
            self.outbox_thread.start()


_Profile = Runtime  # older name inside this module

_PROFILES: dict[str, Runtime] = {}
_PROFILES_LOCK = threading.Lock()


def _profile() -> Runtime:
    data_dir = private_dir(hostcompat.plugin_dir())
    key = str(data_dir)
    with _PROFILES_LOCK:
        prof = _PROFILES.get(key)
        if prof is None:
            ledger = Ledger(data_dir / "ledger.sqlite3")
            ops = OpRunner(ledger, hostcompat.run_in_context, lock_dir=data_dir / "locks")
            prof = Runtime(data_dir=data_dir, ledger=ledger, ops=ops)
            _PROFILES[key] = prof
        return prof


def _reset_for_tests() -> None:
    """Drop the per-process caches and stop their threads (tests only)."""
    with _PROFILES_LOCK:
        for prof in _PROFILES.values():
            prof.stopping.set()
            if prof.veto is not None:
                with contextlib.suppress(Exception):
                    prof.veto.stop(timeout=5.0)
            if prof.runner is not None:
                prof.runner.join(5.0)
            prof.ops.stop()
            prof.ledger.close()
        _PROFILES.clear()


def _store(prof: Runtime):
    with prof.lock:
        if prof.store is None:
            from . import blocks

            settings = plugin_settings(hostcompat.load_config())
            prof.store = blocks.BlockStore(  # root = plugin data dir (DESIGN §5.1); cache limits of §14.4
                prof.data_dir,
                prof.ledger,
                cache_days=float(settings["block_cache_days"]),
                cache_bytes=int(settings["block_cache_gb"] * _GIB),
            )
        return prof.store


def _git(prof: _Profile):
    with prof.lock:
        if prof.git is None:
            from . import gitops

            prof.git = gitops.Git(home=hostcompat.effective_home())  # W5: same ~/.gitconfig as the terminal
        return prof.git


def _db_factory(prof: _Profile, state_db: Path) -> Callable:
    """One ``SessionDB`` per thread and ``state.db`` path, pinned at composition time (worker threads never resolve
    the profile from the environment of the moment)."""
    key = str(state_db)
    with prof.lock:
        factory = prof.db_factories.get(key)
        if factory is None:
            local = threading.local()

            def factory():
                db = getattr(local, "db", None)
                if db is None:
                    db = hostcompat.session_db(state_db)
                    local.db = db
                return db

            prof.db_factories[key] = factory
        return factory


def _identity(prof: Runtime):
    """Load (never create) this profile's identity (R16); ``None`` when there is none yet or it is broken
    (``IdentityLost`` is logged once and remembered in ``identity_error``, W056). A changed fingerprint in the
    ledger (paired or rotated by another process of the profile) drops the cached identity and its error."""
    from . import crypto

    fp = prof.ledger.meta_get(crypto.META_FP)
    with prof.lock:
        if fp != prof.identity_fp:
            prof.identity, prof.identity_error, prof.identity_fp = None, None, fp
        if prof.identity is None and prof.identity_error is None:
            try:
                prof.identity = crypto.Identity.load(
                    prof.ledger, read_secret=hostcompat.read_secret, read_env_fresh=hostcompat.read_env_fresh
                )
            except HandoverError as exc:
                prof.identity_error = exc.code
                log.warning("ctx_handover identity not usable: %s (run /handover doctor)", exc.code)
        return prof.identity


def _peer_verified(peer: Peer | None) -> bool:
    """X21: a peer whose messages are signature-checked — paired, not revoked, with a stored public key (the
    listener verifies its requests, ``wire`` its responses). ``None`` (no peer: a bundle, a local job) is not."""
    return peer is not None and peer.revoked_at is None and bool(peer.pubkey)


_TARGET_OS = {"windows": "windows", "darwin": "macos"}


def _os_family() -> str:
    """This host's OS family by the rule its facts report (``envfacts._family`` over ``platform.system()``): MSYS,
    Cygwin and MinGW are Windows — ``os.name`` is ``posix`` there, so the Windows naming rules and denylist of this
    host's own policy did not apply (rv-k0)."""
    from .envfacts import _family

    return _family(platform.system())


def _policy(opts: Options, prof: _Profile, hermes, *, include_secret=()) -> PolicyConfig:
    return PolicyConfig.from_options(
        opts,
        home=hostcompat.effective_home(),
        hermes_home=Path(hermes.home),
        hermes_root=hostcompat.hermes_root(),
        plugin_dir=prof.data_dir,
        case_insensitive=_os_family() in ("windows", "darwin"),
        target_os=_TARGET_OS.get(_os_family(), "linux"),
        include_secret=include_secret,
        document_cache=Path(hermes.document_cache),
    )


def _side_question(ctx, features, timeout_s: float = 45.0) -> Callable[[str], str | None] | None:
    if ctx is None or not features.side_question:
        return None
    token = getattr(ctx, "side_question_token", None)

    def ask(prompt: str) -> str | None:
        try:
            return ctx.side_question(token, prompt, timeout=float(timeout_s)) if token else None
        except Exception:
            return None

    return ask


def _llm(ctx) -> Callable | None:
    if ctx is not None and getattr(ctx, "llm", None) is not None:
        complete = getattr(ctx.llm, "complete_structured", None)
        if callable(complete):

            def call(instructions: str, input_blocks: list, schema: dict, timeout_s: int) -> dict | None:
                result = complete(
                    instructions=instructions,
                    input=input_blocks,
                    json_schema=schema,
                    timeout=float(timeout_s),
                    purpose="ctx_handover.refine",
                )
                return structured_answer(result)

            return call
    return hostcompat.plugin_llm()


def structured_answer(result: object) -> dict | None:
    """The JSON object of a ``complete_structured`` answer (R34, L3-F09): ``ctx.llm`` returns
    ``PluginLlmStructuredResult`` whose ``parsed`` holds it (set only when the reply was JSON) — the same unpacking
    as ``hostcompat.plugin_llm``. Without a JSON object there is no answer; the result object itself is never passed
    on as if it were the refinement."""
    parsed = getattr(result, "parsed", None)
    if isinstance(parsed, dict):
        return parsed
    return _first_json_object(getattr(result, "text", None))


_JSON_SCAN_CHARS = 1024 * 1024
_JSON_SCAN_STARTS = 64


def _first_json_object(text: object) -> dict | None:
    """The first JSON object inside a model's prose or code fence (review L13: ``parsed`` stays empty when the reply
    is not pure JSON, and the refinement was dropped). Bounded: the first ``_JSON_SCAN_CHARS`` characters, at most
    ``_JSON_SCAN_STARTS`` opening braces tried. The schema is checked by the caller (``refine``) as for ``parsed``."""
    if not isinstance(text, str):
        return None
    text = text[:_JSON_SCAN_CHARS]
    decoder = json.JSONDecoder()
    start = text.find("{")
    for _ in range(_JSON_SCAN_STARTS):
        if start < 0:
            return None
        try:
            value, _end = decoder.raw_decode(text, start)
        except ValueError:
            start = text.find("{", start + 1)
            continue
        if isinstance(value, dict):
            return value
        start = text.find("{", start + 1)
    return None


def make_local_apis(
    ctx_or_none,
    *,
    calling_session: str | None,
    session_key: str | None,
    ports: SimpleNamespace | None = None,
    remote_driven: bool = False,
    peer: Peer | None = None,
    include_secret=(),
    side_question: Callable[[str], str | None] | None = None,
) -> tuple[LocalSource, LocalTarget]:
    """``LocalSource`` and ``LocalTarget`` of this profile (composition root; Hermes only via ``hostcompat``)."""
    prof = _profile()
    config = hostcompat.load_config()
    settings = plugin_settings(config)
    opts = local_options(settings)
    hermes = hostcompat.hermes_paths()
    host = hostcompat.host_facts()
    features = hostcompat.features(ctx_or_none)
    policy = _policy(opts, prof, hermes, include_secret=include_secret)
    store = _store(prof)
    state_db = hostcompat.state_db_path()
    db_factory = _db_factory(prof, state_db)
    git = _git(prof)
    source_ports = getattr(ports, "source", None) if ports is not None else None
    target_ports = getattr(ports, "target", None) if ports is not None else None
    source = LocalSource(
        ledger=prof.ledger,
        store=store,
        db_factory=db_factory,
        hermes=hermes,
        host=host,
        config=config,
        ops=prof.ops,
        git=git,
        calling_session=calling_session,
        session_key=session_key,
        side_question=side_question or _side_question(ctx_or_none, features, opts.refine_timeout_s),
        llm=_llm(ctx_or_none),
        read_secret=hostcompat.read_secret,
        redact=hostcompat.redact,
        state_db=state_db,
        remote_driven=remote_driven,
        policy=policy,
        ports=source_ports,
        data_dir=prof.data_dir,
        peer_name=peer.name if peer is not None else None,
    )
    from . import continuation

    spawner = continuation.Spawner(
        log_dir=private_dir(prof.data_dir / "logs"),
        profile=host.profile or "default",  # by name, also ``default``: no sticky ``active_profile`` (review M28)
        approvals_off=lambda: hostcompat.approvals_off(hostcompat.load_config()),
        allow_remote=lambda: plugin_settings(hostcompat.load_config()).get("allow_remote_continue") is True,
    )
    target = LocalTarget(
        ledger=prof.ledger,
        store=store,
        db_factory=db_factory,
        hermes=hermes,
        host=host,
        config=config,
        ops=prof.ops,
        git=git,
        provider_usable=hostcompat.provider_usable,
        continuation=spawner,
        redact=hostcompat.redact,
        source_peer_id=peer.peer_id if peer is not None else None,
        peer_verified=_peer_verified(peer),  # X21; an import sets it from the bundle signature (JobRunner)
        policy=policy,
        ports=target_ports,
        data_dir=prof.data_dir,
        settings=settings,
        peer_name=peer.name if peer is not None else None,
    )
    return source, target


def _default_transport() -> SimpleNamespace:
    from . import bundle, remote, wire

    return SimpleNamespace(bundle=bundle, remote=remote, wire=wire)


def apis_for(
    mode: Mode,
    peer: Peer | None,
    *,
    local_source: LocalSource | None,
    local_target: LocalTarget | None,
    identity,
    bundle_path: Path | None,
    options: Options,
    ledger: Ledger,
    transport: SimpleNamespace | None = None,
) -> tuple[SourceAPI, TargetAPI]:
    """The participant pair of a conductor job (DESIGN §2.2): push = local → remote, pull = remote → local,
    export = local → bundle, import = bundle → local. Bundle trust comes only from stored, non-revoked peer
    keys (SEC 1.6)."""
    t = transport or _default_transport()
    if mode in (Mode.PUSH, Mode.PULL):
        if peer is None or peer.direction != "dial" or not peer.url:
            raise BadRequest("this mode needs a paired peer this device dials", field="peer")
        if peer.revoked_at is not None:
            raise Forbidden(f"{peer.name} was revoked")
        if identity is None:
            raise Unauthorized("this profile has no identity yet; pair first")
        client = t.wire.WireClient(peer, identity, read_secret=hostcompat.read_secret)
        if mode is Mode.PUSH:
            if local_source is None:
                raise BadRequest("push needs the local source", field="source")
            return local_source, t.remote.RemoteTarget(client)
        if local_target is None:
            raise BadRequest("pull needs the local target", field="target")
        return t.remote.RemoteSource(client), local_target
    if mode is Mode.EXPORT:
        if local_source is None or bundle_path is None:
            raise BadRequest("export needs the local source and a bundle path", field="bundle")
        host = getattr(local_source, "host", None)
        return local_source, t.bundle.BundleTarget(
            Path(bundle_path),
            identity,
            as_dir=options.as_dir,
            declared_facts=known_facts(ledger, peer),  # export --for <peer> (V28); None: generic importer facts
            ledger=ledger,
            retire=not options.keep_source,  # --keep exports a copy and leaves the source active (O3)
            exporter_name=getattr(local_source, "instance_name", "") or "",
            negotiate=_bundle_negotiation(Path(bundle_path), options),
            plugin_version=getattr(host, "plugin_version", "") or "",
            hermes_version=getattr(host, "hermes_version", "") or "",
        )
    if mode is Mode.IMPORT:
        if local_target is None or bundle_path is None:
            raise BadRequest("import needs the local target and a bundle path", field="bundle")
        trusted = {p.pubkey for p in ledger.peers() if p.revoked_at is None and p.pubkey}
        own = ledger.meta_get("identity_pubkey")
        if own:
            trusted.add(own)  # a bundle this profile exported itself
        return (
            t.bundle.BundleSource(
                Path(bundle_path),
                trusted_pubkeys=trusted,
                trust_unsigned=options.trust_unsigned,
                ledger=ledger,
                again=options.again,
            ),
            local_target,
        )
    raise BadRequest(f"mode {mode} has no conductor", field="mode")


def known_facts(ledger: Ledger, peer: Peer | None) -> EnvFacts | None:
    """The target facts last received from ``peer`` (``Ledger.peer_facts_get``, contract V28): ``export --for
    <peer>`` negotiates with them (DESIGN §5.4). ``None`` without a peer, without stored facts or when they no
    longer parse (written by another plugin version) — the bundle then assumes an unknown importer."""
    if peer is None:
        return None
    raw = ledger.peer_facts_get(peer.peer_id)
    if not isinstance(raw, dict):
        return None
    try:
        return from_wire(EnvFacts, raw)
    except (HandoverError, TypeError, ValueError):
        return None


def _bundle_negotiation(bundle_path: Path, options: Options) -> Callable | None:
    """The planning cluster's export negotiation (W062, X23): ``plan.bundle_negotiate`` bound to the bundle's
    directory (free-space check before anything is written); ``None`` without it (``BundleTarget`` then decides
    fresh directories itself)."""
    from . import plan as planning

    negotiate = getattr(planning, "bundle_negotiate", None)
    if not callable(negotiate):
        return None
    out_dir = bundle_path if options.as_dir else bundle_path.parent
    return functools.partial(negotiate, out_dir=out_dir)


def listener_server():
    """The ``server.Server`` behind ``dashboard/plugin_api.py`` (cached per profile; starts the maintenance
    sweep of remote-driven participants). Without an identity nothing is paired: ``None`` — every route then
    answers like a missing plugin (DESIGN §12.2, §12.4) and the next request tries again, so a ``/handover pair``
    in another process of this profile takes effect without a restart."""
    prof = _profile()
    with prof.lock:
        identity = _identity(prof)
        if identity is None:
            prof.server, prof.server_identity = None, None
            return None
        if prof.server is not None and prof.server_identity is identity:
            return prof.server
        from . import server

        registry = _registry(prof)
        settings = server_settings(hostcompat.load_config())
        auths = [server.OperatorSessionAuth(enabled=False), server.PeerSignatureAuth(prof.ledger, identity)]
        srv = server.Server(
            prof.ledger,
            identity,
            registry,
            settings,
            auths,
            redact=hostcompat.redact,
            # read on every request: ``inbound`` changed in the settings takes effect without a restart (M15)
            inbound_setting=lambda: plugin_settings(hostcompat.load_config()).get("inbound"),
        )
        prof.server, prof.server_identity = srv, identity  # rebuilt when another process rotates the identity
    _start_maintenance(prof)
    return srv


def server_settings(config: dict) -> dict:
    """The listener's settings: the plugin settings plus what the server reports in ``hello`` (W055)."""
    host = hostcompat.host_facts()
    raw = settings_mod.raw_settings(config)
    # the listener's own knobs outside the schema (``name``, ``operator_api``, rate limits …) pass as given — the
    # server normalises them itself; every schema key comes from the one normaliser
    settings = {k: v for k, v in raw.items() if k not in settings_mod.SCHEMA}
    settings.update(plugin_settings(config))
    if not isinstance(settings.get("name"), str) or not settings["name"].strip():
        settings["name"] = host.profile if host.profile not in ("", "default") else _hostname()
    settings["max_jobs_per_peer"] = settings_mod.positive_int(raw.get("max_jobs_per_peer"), MAX_JOBS_PER_PEER)
    settings["max_jobs_total"] = settings_mod.positive_int(raw.get("max_jobs_total"), MAX_JOBS)
    settings["hosted"] = bool(hostcompat.features().host_isolated)
    settings["plugin_version"] = host.plugin_version
    settings["hermes_version"] = host.hermes_version
    return settings


def _hostname() -> str:
    import socket

    return socket.gethostname().split(".")[0][:64] or "hermes"


def _registry(prof: _Profile) -> ParticipantRegistry:
    with prof.lock:
        if prof.registry is None:

            def factory(role: Role, peer: Peer | None):
                source, target = make_local_apis(
                    None, calling_session=None, session_key=None, remote_driven=True, peer=peer
                )
                return source if role is Role.SOURCE else target

            raw = settings_mod.raw_settings(hostcompat.load_config())
            prof.registry = ParticipantRegistry(  # the one owner of the job caps (X18)
                prof.ledger,
                factory,
                local_options=lambda: local_options(plugin_settings(hostcompat.load_config())),
                max_jobs_per_peer=settings_mod.positive_int(raw.get("max_jobs_per_peer"), MAX_JOBS_PER_PEER),
                max_jobs=settings_mod.positive_int(raw.get("max_jobs_total"), MAX_JOBS),
            )
        return prof.registry


def _start_maintenance(prof: _Profile) -> None:
    with prof.lock:
        if prof.maintenance is not None and prof.maintenance.is_alive():
            return
        registry = _registry(prof)
        ctx = contextvars.copy_context()

        def loop() -> None:
            while not prof.stopping.wait(MAINTENANCE_S):
                with contextlib.suppress(Exception):
                    registry.sweep(prof.ledger.now())

        prof.maintenance = threading.Thread(target=ctx.run, args=(loop,), name="ctxh-maintenance", daemon=True)
        prof.maintenance.start()


# ---------------------------------------------------------------------------------------------------------
# the job runner
# ---------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class CancelAnswer:
    """What ``/handover cancel`` did (L3-F22). ``requested`` is True only when a cancel was recorded or carried out;
    otherwise ``reason`` says why not — ``finished`` (the job is done or aborted) or ``committing`` (the conductor
    journaled ``intent: commit``, DESIGN §3.8) — and the surfaces answer with a refusal. ``state`` is the conductor's
    state when it answered; ``text`` names it."""

    requested: bool
    state: str
    text: str
    reason: str | None = None


class _NullReporter:
    def progress(self, p: Progress) -> None:
        pass

    def info(self, text: str) -> None:
        pass

    def warn(self, text: str) -> None:
        pass


def exit_code(row: dict | None) -> int:
    """CLI exit codes (DESIGN §14.2): 0 ok · 1 failed · 3 busy/conflict/wait · 4 auth/gate · 5 in-doubt/suspended."""
    if row is None:
        return EXIT_USAGE
    state = row["state"]
    if state == "done":
        return EXIT_OK
    if state in ("suspended", "in_doubt", "retiring", "committed", "committing"):
        return EXIT_PENDING
    code = (row.get("last_error") or {}).get("code")
    if code in (SessionBusy.code, WaitTimeout.code, "already_handing_over", "conflict", "job_conflict"):
        return EXIT_BUSY
    if code in (GateRejected.code, Unauthorized.code, Forbidden.code, "pair_invalid", "https_required"):
        return EXIT_AUTH
    if code == InDoubt.code:
        return EXIT_PENDING
    return EXIT_FAILED


def _size_of(path: Path) -> int | None:
    try:
        if path.is_dir():
            return sum(p.stat().st_size for p in path.rglob("*") if p.is_file())
        return path.stat().st_size
    except OSError:
        return None


def _plan_lists(plan_doc: dict) -> dict:
    """Lists the final message and ``status`` show from the confirmed plan (SEC 4.6): excluded secrets, withheld
    stash entries, conflicts, instruction files."""
    plan = plan_doc.get("plan") or {}
    items = plan.get("items") or []
    secrets = [
        n.get("path") for n in plan.get("not_transferred") or () if n.get("why") in ("secret", "suspected_secret")
    ]
    stash = [n.get("path") for n in plan.get("not_transferred") or () if "stash" in str(n.get("why"))]
    conflicts = [
        {"item_id": d.get("item_id"), "kind": (d.get("conflict") or {}).get("kind"), "dest": d.get("dest")}
        for d in plan_doc.get("decisions") or ()
        if d.get("conflict")
    ]
    instructions = sorted(
        {
            str(r).split(":", 1)[1].strip()
            for i in items
            for r in i.get("reasons") or ()
            if str(r).startswith("instruction file:")
        }
    )
    exclusions = [
        {"path": n.get("path"), "why": n.get("why")} for n in plan.get("not_transferred") or () if isinstance(n, dict)
    ]
    for i in items:  # tracked files on the secrets list: their working-tree changes stay behind (R23)
        git = i.get("git") if isinstance(i, dict) else None
        denied = git.get("denied_tracked") if isinstance(git, dict) else None
        for path in denied if isinstance(denied, list) else ():
            entry = {"path": _joined(i.get("source_path"), path), "why": "denied_tracked"}
            if entry not in exclusions:
                exclusions.append(entry)
    return {
        "exclusions": exclusions,
        "excluded_secrets": secrets,
        "withheld_stash": stash,
        "conflicts": conflicts,
        "instruction_files": instructions,
    }


def _joined(root: object, path: object) -> str:
    """A path of a work tree made absolute under its item's source path (absolute paths stay)."""
    text = str(path)
    if isinstance(root, str) and root and not text.startswith(("/", "\\")):
        return f"{root.rstrip('/')}/{text}"
    return text


def _left_behind(lists: dict, result: dict) -> None:
    """Merge the conductor's ``result["left_behind"]`` (pack exclusions and dropped suspected secrets, contract V34:
    ``{"path", "why", "item_id"}``) into the status lists: every entry into ``exclusions``, secrets into
    ``excluded_secrets``, whole stash entries (``why == "stash_secret"``) into ``withheld_stash`` — each path once."""
    for entry in result.get("left_behind") or ():
        if not isinstance(entry, dict) or not isinstance(entry.get("path"), str):
            continue
        path, why = entry["path"], str(entry.get("why") or "excluded")
        item = {"path": path, "why": why}
        if item not in lists["exclusions"]:
            lists["exclusions"].append(item)
        if why in ("secret", "suspected_secret") and path not in lists["excluded_secrets"]:
            lists["excluded_secrets"].append(path)
        if why == "stash_secret" and path not in lists["withheld_stash"]:
            lists["withheld_stash"].append(path)


def _next_action(row: dict, outcome: str) -> str | None:
    s = short(row["job_id"])
    state = row["state"]
    if state == "suspended":
        return f"/handover resume {s}"
    if state in ("in_doubt", "committing"):
        return "/handover status shows progress; /handover reclaim --force takes the session back here"
    if outcome == "active_source_pending" or state == "retiring":
        return "the other side is released on the next contact (the janitor retries)"
    return None


class JobRunner:
    """Conductor jobs of this process (see module docstring)."""

    def __init__(
        self,
        ledger: Ledger,
        reporter_factory: Callable[[str], Reporter] | None = None,
        *,
        apis_factory: Callable[[dict], tuple[SourceAPI, TargetAPI]] | None = None,
        planning=None,
        clock: Callable[[], float] | None = None,
        sleep: Callable[[float], None] | None = None,
        poll: PollPolicy | None = None,
        keepalive: bool = True,
        before_job: Callable[[], None] | None = None,
    ) -> None:
        self.ledger = ledger
        self._reporter_factory = reporter_factory or (lambda job_id: _NullReporter())
        self._has_reporter_factory = reporter_factory is not None
        self._apis_factory = apis_factory or self._default_apis
        self._planning = planning
        self._clock = clock or time.time
        self._sleep = sleep
        self._poll = poll
        self._keepalive = keepalive
        self._before_job = before_job
        self._lock = threading.Lock()
        self._threads: dict[str, threading.Thread] = {}
        self._owners: dict[str, tuple[str, int]] = {}
        self._ctxs: dict[str, object] = {}
        self._sides: dict[str, Callable[[str], str | None]] = {}
        # V24: the participant pair of a job, built once and kept from ``plan_sync`` until its run in this process
        # ends — ``plan_sync`` and ``run_foreground``/``continue_async`` drive the same instances.
        self._pairs: dict[str, tuple[SourceAPI, TargetAPI]] = {}

    def bind_reporter_factory(self, reporter_factory: Callable[[str], Reporter]) -> bool:
        """Give a runner that was created without one (by the janitor, a route, the CLI) the surfaces' reporter
        factory; the first real factory stays (one broker per process). True when this call bound it."""
        with self._lock:
            if self._has_reporter_factory:
                return False
            self._reporter_factory = reporter_factory
            self._has_reporter_factory = True
            return True

    # -- wiring -------------------------------------------------------------------------------------------------
    def _default_apis(self, row: dict) -> tuple[SourceAPI, TargetAPI]:
        params = row["params"] or {}
        mode = Mode(row["mode"])
        peer = self.ledger.peer(row["peer_id"]) if row["peer_id"] else None
        options = from_wire(Options, params.get("options") or {})
        ctx = self._ctxs.get(row["job_id"])
        source, target = make_local_apis(
            ctx,
            calling_session=params.get("calling_session"),
            session_key=params.get("session_key"),
            peer=peer,
            include_secret=tuple(params.get("include_secret") or ()),
            side_question=self._sides.get(row["job_id"]),
        )
        prof = _profile()
        bundle = params.get("bundle_path")
        src_api, tgt_api = apis_for(
            mode,
            peer,
            local_source=source,
            local_target=target,
            identity=_identity(prof),
            bundle_path=Path(bundle) if bundle else None,
            options=options,
            ledger=self.ledger,
        )
        if mode is Mode.IMPORT and tgt_api is target:
            # X21: an import is verified only by the bundle signature of a stored, non-revoked key (BundleSource)
            target.peer_verified = getattr(src_api, "verified", False) is True
            exporter = self._exporter(src_api) if target.peer_verified else None
            if exporter is not None:  # V13: the verified exporter is the job's peer (return trip, workspace map)
                target.source_peer_id, target.peer_name = exporter.peer_id, exporter.name
            self._record_origin(row["job_id"], src_api)
        return src_api, tgt_api

    def _exporter(self, bundle_source) -> Peer | None:
        """The paired peer whose stored, non-revoked key signed an imported bundle (V13), or None — an unsigned
        bundle, one signed by this profile's own key (``"trusted key"``) or a signer that is no peer here."""
        signer = getattr(bundle_source, "signer", None)
        if not isinstance(signer, str) or not signer or signer == "trusted key":
            return None
        peer = self.ledger.peer(signer)
        if peer is None or peer.name != signer or peer.revoked_at is not None or not peer.pubkey:
            return None
        return peer

    def _record_origin(self, job_id: str, bundle_source) -> None:
        """Journal where an imported bundle comes from (SEC 4.6: status shows the origin)."""
        with self._lock:
            mine = self._owners.get(job_id)
        if mine is None:
            return
        signer = getattr(bundle_source, "signer", None)
        verified = getattr(bundle_source, "verified", False) is True
        origin = f"verified bundle ({signer})" if verified else "unverified bundle"
        with contextlib.suppress(HandoverError):
            self.ledger.event(job_id, Role.CONDUCTOR, "origin", {"origin": origin}, epoch=mine[1])

    def _claim(self, job_id: str) -> tuple[str, int]:
        with self._lock:
            mine = self._owners.get(job_id)
        if mine is not None:
            epoch = self.ledger.claim_owner(job_id, Role.CONDUCTOR, mine[0], STALE_OWNER_S)
            if epoch == mine[1]:
                return mine
        owner = f"{process_owner()}/{secrets.token_hex(3)}"
        epoch = self.ledger.claim_owner(job_id, Role.CONDUCTOR, owner, STALE_OWNER_S)
        if epoch is None:
            raise InvalidState("another runner is working on this job", reason="busy_elsewhere", job=short(job_id))
        with self._lock:
            self._owners[job_id] = (owner, epoch)
        return owner, epoch

    def _session_title(self, session_ref: SessionRef, calling_session: str | None) -> str | None:
        sid = session_ref.id or calling_session
        if not sid:
            return None
        try:
            db = hostcompat.session_db(hostcompat.state_db_path())
            row = db.get_session(sid) or {}
            return row.get("title")
        except Exception:
            return None

    def _release(self, job_id: str) -> None:
        with self._lock:
            mine = self._owners.pop(job_id, None)
        if mine is not None:
            with contextlib.suppress(HandoverError):
                self.ledger.release_owner(job_id, Role.CONDUCTOR, mine[0], mine[1])

    def _abort_unbuilt(self, job_id: str, exc: HandoverError) -> None:
        """The participants of a job could not be composed (bundle refused, peer removed, no identity): a job that
        locked nothing yet ends ``aborted`` with the error instead of staying active forever (the janitor would
        retry it on every run); a later state is left for ``resume``/``reclaim``."""
        with self._lock:
            mine = self._owners.get(job_id)
        if mine is None:
            return
        with contextlib.suppress(HandoverError):
            self.ledger.transition(
                job_id,
                Role.CONDUCTOR,
                _UNLOCKED,
                str(ConductorState.ABORTED),
                {"step": "compose", "reason": exc.code},
                epoch=mine[1],
            )
            self.ledger.job_update(job_id, Role.CONDUCTOR, epoch=mine[1], last_error=exc.to_wire()["error"])

    def _conductor(self, job_id: str, *, confirm=None) -> Conductor:
        row = self.ledger.job(job_id, Role.CONDUCTOR)
        if row is None:
            raise NotFound(f"job {short(job_id)} not found")
        owner, epoch = self._claim(job_id)
        params = row["params"] or {}
        options = from_wire(Options, params.get("options") or {})
        peer = self.ledger.peer(row["peer_id"]) if row["peer_id"] else None
        job = JobContext(
            job_id=job_id,
            mode=Mode(row["mode"]),
            peer=peer,
            options=options,
            epoch=epoch,
            reporter=self._reporter_factory(job_id),
            cancelled=lambda: self.ledger.cancel_requested(job_id),
            created_at=float(row["created_at"]),
        )
        source, target = self._participants(row)
        return Conductor(
            self.ledger,
            job,
            source,
            target,
            confirm=confirm,
            continue_prompt=params.get("continue_prompt"),
            planning=self._planning,
            owner=owner,
            clock=self._clock,
            sleep=self._sleep,
            poll=self._poll,
            keepalive=self._keepalive,
        )

    def _participants(self, row: dict) -> tuple[SourceAPI, TargetAPI]:
        """One participant pair per job and process run (V24): the pair ``plan_sync`` composed is the pair the run
        continues with; it is dropped when that run ends (a later ``resume`` composes afresh and re-checks the
        peer)."""
        job_id = row["job_id"]
        with self._lock:
            pair = self._pairs.get(job_id)
        if pair is None:
            pair = self._apis_factory(row)
            with self._lock:
                pair = self._pairs.setdefault(job_id, pair)
        return pair

    # -- verbs ----------------------------------------------------------------------------------------------------
    def plan_sync(
        self,
        mode: Mode,
        peer: Peer | None,
        options: Options,
        *,
        session_ref: SessionRef,
        calling_session: str | None,
        session_key: str | None,
        ctx=None,
        confirm: Callable[[dict], str | None] | None = None,
        bundle_path: Path | None = None,
        include_secret: tuple[str, ...] = (),
        side_question: Callable[[str], str | None] | None = None,
    ) -> tuple[str, FinalPlan | None]:
        """Create the job and run it to NEGOTIATED with the user's confirmation (synchronous; the slash handler
        can ask). Returns ``(job_id, FinalPlan)``; ``None`` when the job ended there (``show_plan``/``cancel``).
        Other failures are raised after the job was aborted (nothing locked)."""
        if self._before_job is not None:
            self._before_job()
        if mode is Mode.EXPORT and bundle_path is None:
            hermes = hostcompat.hermes_paths()
            bundle_path = default_bundle_path(
                self._session_title(session_ref, calling_session),
                options,
                home=hostcompat.effective_home(),
                hermes_home=Path(hermes.home),
                hermes_root=hostcompat.hermes_root(),
            )
        job_id = ulid()
        params = {
            "mode": str(mode),
            "session_ref": to_wire(session_ref),
            "options": to_wire(options),
            "continue_prompt": options.continue_prompt,
            "calling_session": calling_session,
            "session_key": session_key,
            "bundle_path": str(bundle_path) if bundle_path else None,
            "include_secret": list(include_secret),
        }
        self.ledger.create_job(job_id, Role.CONDUCTOR, mode, peer.peer_id if peer else None, params)
        if ctx is not None:
            self._ctxs[job_id] = ctx
        if side_question is not None:
            self._sides[job_id] = side_question
        try:
            conductor = self._conductor(job_id, confirm=confirm)
        except HandoverError as exc:
            self._abort_unbuilt(job_id, exc)
            self._forget(job_id)
            raise
        except Exception as exc:  # composition failed outside the error model: the job ends, nothing is locked
            err = unexpected(exc)
            self._abort_unbuilt(job_id, err)
            self._forget(job_id)
            raise err from exc
        try:
            final = conductor.run_until_negotiated()
        except Cancelled:
            self._forget(job_id)
            return job_id, None
        except HandoverError:
            self._forget(job_id)
            raise
        return job_id, final

    def _forget(self, job_id: str) -> None:
        """Release the job and drop what this process kept for it (context, side question)."""
        self._release(job_id)
        with self._lock:
            self._ctxs.pop(job_id, None)
            self._sides.pop(job_id, None)
            self._pairs.pop(job_id, None)

    def _run(self, job_id: str, how: str) -> None:
        """One run of a job in this thread. The conductor turns every failure of a step into the job's state;
        what escapes it (the composition of the participants, the ledger itself) is logged, and a job that locked
        nothing yet ends ``aborted`` — a failure never leaves the thread with the job held by this runner."""
        try:
            try:
                conductor = self._conductor(job_id)
            except HandoverError as exc:
                self._abort_unbuilt(job_id, exc)
                return
            except Exception as exc:
                log.warning("ctx_handover job %s: participants not composed", short(job_id), exc_info=True)
                self._abort_unbuilt(job_id, unexpected(exc))
                return
            getattr(conductor, how)()
        except HandoverError:
            pass
        except Exception:
            log.warning("ctx_handover job %s: unexpected failure", short(job_id), exc_info=True)
        finally:
            self._release(job_id)
            with self._lock:
                self._threads.pop(job_id, None)
                self._ctxs.pop(job_id, None)
                self._sides.pop(job_id, None)
                self._pairs.pop(job_id, None)

    def _spawn(self, job_id: str, how: str) -> threading.Thread:
        with self._lock:
            thread = self._threads.get(job_id)
            if thread is not None and thread.is_alive():
                return thread
            ctx = contextvars.copy_context()
            thread = threading.Thread(
                target=ctx.run, args=(self._run, job_id, how), name=f"ctxh-job-{short(job_id)}", daemon=True
            )
            self._threads[job_id] = thread
        thread.start()
        return thread

    def continue_async(self, job_id: str) -> None:
        """LOCKING onwards in a worker thread (the handler returns at once)."""
        self._spawn(job_id, "run")

    def run_foreground(self, job_id: str) -> int:
        """CLI: run to a stop state in this thread; returns the exit code of DESIGN §14.2."""
        self._run(job_id, "run")
        return exit_code(self.ledger.job(job_id, Role.CONDUCTOR))

    def resume(self, job_id: str) -> None:
        self._spawn(job_id, "resume")

    def recover(self, job_id: str, *, background: bool = True) -> None:
        """Janitor: adopt a job whose runner died and apply DESIGN §3.6."""
        if background:
            self._spawn(job_id, "recover")
        else:
            self._run(job_id, "recover")

    def join(self, timeout: float = 30.0) -> None:
        for thread in list(self._threads.values()):
            thread.join(timeout)

    def cancel(self, job_id: str) -> str:
        """The text of ``cancel_answer`` (for callers that only show it)."""
        return self.cancel_answer(job_id).text

    def cancel_answer(self, job_id: str) -> CancelAnswer:
        """Request cancellation. The job's state is read first (L3-F22): a finished job (done, aborted) is named as
        such — ``ledger.set_cancel`` would call every job with a journaled ``intent: commit`` "committing", and every
        finished job journaled it; a job past its commit point is refused with its state; only a request the ledger
        holds is ``requested``. A SUSPENDED job has no runner that would see the request (L3-F07): the cancel drives
        its abort itself (``_abort_suspended``). An unknown job raises ``NotFound``."""
        sid = short(job_id)
        row = self.ledger.job(job_id, Role.CONDUCTOR)
        refused = False
        if row is None or row["state"] not in TERMINAL[Role.CONDUCTOR]:
            try:
                self.ledger.set_cancel(job_id)  # NotFound for an unknown job
            except InvalidState:
                refused = True
            row = self.ledger.job(job_id, Role.CONDUCTOR)
        state = str(row["state"]) if row is not None else "gone"
        if state in TERMINAL[Role.CONDUCTOR] or row is None:
            text = f"Handover {sid} is already {state}; nothing to cancel."
            if state == str(ConductorState.DONE):
                text = (
                    f"Too late to cancel: handover {sid} is already done; nothing was cancelled. /handover reclaim on "
                    "the source takes the session back."
                )
            return CancelAnswer(False, state, text, "finished")
        if refused:
            return CancelAnswer(
                False,
                state,
                f"Too late to cancel handover {sid}: it is past its commit point ({state}). Use /handover reclaim on "
                "the source to take the session back.",
                "committing",
            )
        if self.ledger.cancel_requested(job_id) is not True:  # set_cancel wrote nothing: no request is claimed
            return CancelAnswer(False, state, f"Handover {sid} is {state}; nothing was cancelled.", "not_recorded")
        if state == str(ConductorState.SUSPENDED):
            text = self._abort_suspended(job_id)
            after = self.ledger.job(job_id, Role.CONDUCTOR) or {}
            return CancelAnswer(True, str(after.get("state") or state), text)
        return CancelAnswer(True, state, f"Cancel requested for job {sid}.")

    def _abort_suspended(self, job_id: str) -> str:
        """``/handover cancel`` of a SUSPENDED job (L3-F07): the abort runs now — ``target.abort``, then
        ``source.release`` (V37 order) ⇒ ABORTED, which frees the source and the target's participant slot. The answer
        waits at most ``CANCEL_SETTLE_S``; a target that cannot be reached keeps the job ``aborting`` (the session's
        turn lease runs out on its own, DESIGN §7.3) and the janitor finishes it on the next start."""
        thread = self._spawn(job_id, "recover")
        thread.join(CANCEL_SETTLE_S)
        row = self.ledger.job(job_id, Role.CONDUCTOR) or {}
        state, sid = row.get("state"), short(job_id)
        if state == str(ConductorState.ABORTED):
            return f"Handover {sid} cancelled; the session is yours again."
        if state == str(ConductorState.ABORTING):
            err = row.get("last_error") or {}
            why = f" ({err.get('message')})" if isinstance(err, dict) and err.get("message") else ""
            return (
                f"Cancelling handover {sid}: the other side has not confirmed yet{why}. It finishes on the next "
                f"contact; /handover status {sid} shows it."
            )
        return f"Cancel requested for job {sid}."

    def status(self, job_id: str | None) -> dict:
        """Machine-readable status for the surfaces (cluster F renders it, X20): ``{"jobs": [...], …}`` without a
        job, ``{"job": {...}}`` with one (journal tail in ``job.events``).

        Jobs another gateway conducts are known here too (contract V43, L3-F04): the arrival of a push (role
        ``target``) and the source of a pull (role ``source``) — listed after this host's conductor jobs and found by
        ``status <job>``. A participant row of a job this host conducts itself (export, import, the local side of a
        push or pull) is part of that conductor job and not listed twice."""
        if job_id is None:
            recent = self.ledger.jobs(role=Role.CONDUCTOR, limit=5)
            active = self.ledger.jobs(role=Role.CONDUCTOR, active=True, limit=20)
            seen, jobs = set(), []
            for r in [*active, *recent]:
                if r["job_id"] not in seen:
                    seen.add(r["job_id"])
                    jobs.append(self._summary(r))
            jobs += [self._participant_summary(r) for r in self._participant_rows()]
            return {
                "jobs": jobs,
                "pull_log": self.ledger.pull_log(5),
                "outbox_pending": len(self.ledger.outbox_due(self.ledger.now() + 10**9, limit=1000)),
            }
        row = self.ledger.job(job_id, Role.CONDUCTOR)
        if row is not None:
            role, out = Role.CONDUCTOR, self._summary(row)
        else:
            row = next((r for r in (self.ledger.job(job_id, ro) for ro in _PARTICIPANT_ROLES) if r is not None), None)
            if row is None:
                raise NotFound(f"job {short(job_id)} not found")
            role, out = Role(row["role"]), self._participant_summary(row)
        out["events"] = [
            {"kind": e["kind"], "state": e["state"], "step": e["step"], "at": e["ts"]}
            for e in self.ledger.events(job_id, role)[-20:]
        ]
        return {"job": out}

    def _participant_rows(self) -> list[dict]:
        """Active and recent participant rows of jobs another gateway conducts (one per job id and role)."""
        out, seen = [], set()
        for role in _PARTICIPANT_ROLES:
            for r in [*self.ledger.jobs(role=role, active=True, limit=20), *self.ledger.jobs(role=role, limit=5)]:
                key = (r["job_id"], r["role"])
                if key in seen:
                    continue
                seen.add(key)
                if (r.get("params") or {}).get("local") is True:
                    continue  # this host's own participant rows (its conductor jobs, an offer's planning row)
                if self.ledger.job(r["job_id"], Role.CONDUCTOR) is None:
                    out.append(r)
        return out

    def _participant_summary(self, row: dict) -> dict:
        """The X20 summary of a participant job (V43): role, state, peer; on a target the ack and the note the agent
        gets with the first message there (DESIGN §11.4); on a source the state of its handover. ``calling_session``
        is this host's session of the job (the arrived one, or the source session)."""
        role, job_id = Role(row["role"]), row["job_id"]
        params = row.get("params") or {}
        peer = self.ledger.peer(row["peer_id"]) if row["peer_id"] else None
        state, ack, note, origin, session, title = row["state"], None, None, None, None, None
        if role is Role.TARGET:
            ack = _journal_ctx(self.ledger, job_id, role).get("ack")
            ack = ack if isinstance(ack, dict) else None
            arrival = self.ledger.arrival_for(str(ack.get("session_id") or "")) if ack else None
            if arrival is None:
                arrival = next((a for a in self.ledger.arrival_rows() if a.get("job_id") == job_id), None)
            if arrival is not None:
                note, session = arrival.get("note"), arrival.get("session_id")
                origin = f"verified peer ({peer.name})" if _peer_verified(peer) else "peer"
            session = session or (ack or {}).get("session_id")
            title = (ack or {}).get("title")
        else:
            src = self.ledger.source_state(job_id)
            if src is not None:
                state, session, title = str(src["state"]), src.get("tip_id"), src.get("title")
                ack = src.get("ack") if isinstance(src.get("ack"), dict) else None
        terminal = state in TERMINAL[role]
        return {
            "job_id": job_id,
            "short": short(job_id),
            "mode": row["mode"],
            "role": str(role),
            "state": state,
            "active": not terminal,
            "outcome": state if terminal else "running",
            "peer_id": row["peer_id"],
            "peer": peer.name if peer else None,
            "calling_session": session,
            "session_ref": params.get("session_ref"),
            "progress": {"step": state},
            "restarts": row["restarts"],
            "last_error": row["last_error"],
            "error": row["last_error"],
            "next_action": None,
            "ack": ack,
            "title": title,
            "target_session": _target_session(ack, session if role is Role.TARGET else None),
            "note": note,
            "origin": origin,
            "exclusions": [],
            "excluded_secrets": [],
            "withheld_stash": [],
            "conflicts": [],
            "instruction_files": [],
            "continuation": None,
            "withheld": [],
            "source_kept": False,
            "warnings": [],
            "exit_code": EXIT_OK if state in _PARTICIPANT_OK else (EXIT_FAILED if terminal else EXIT_PENDING),
        }

    def _summary(self, row: dict) -> dict:
        result = row.get("result") or {}
        params = row.get("params") or {}
        state = row["state"]
        outcome = result.get("outcome") or ("running" if state not in _STOP else state)
        ack = result.get("ack") if isinstance(result.get("ack"), dict) else None
        if ack is None and state in _ACKED:
            # COMMITTED … DONE before the result is written: the ack is journaled since COMMITTED (V42, L3-F03)
            journaled = _journal_ctx(self.ledger, row["job_id"], Role.CONDUCTOR).get("ack")
            ack = journaled if isinstance(journaled, dict) else None
        plan = result.get("plan") if isinstance(result.get("plan"), dict) else None
        peer = self.ledger.peer(row["peer_id"]) if row["peer_id"] else None
        out = {
            "job_id": row["job_id"],
            "short": short(row["job_id"]),
            "mode": row["mode"],
            "role": str(Role.CONDUCTOR),
            "state": state,
            "active": state not in ("done", "aborted"),
            "outcome": outcome,
            "peer_id": row["peer_id"],
            "peer": peer.name if peer else None,
            "calling_session": params.get("calling_session"),
            "session_ref": params.get("session_ref"),
            "progress": {"step": state},
            "restarts": row["restarts"],
            "last_error": row["last_error"],
            "error": row["last_error"],
            "next_action": _next_action(row, outcome),
            "ack": ack,
            "title": (ack or {}).get("title"),
            "target_session": _target_session(ack, None),
            "note": None,
            "origin": None,
            "exclusions": [],
            "excluded_secrets": [],
            "withheld_stash": [],
            "conflicts": [],
            "instruction_files": [],
            "continuation": result.get("continue"),
            "withheld": [p for p in result.get("withheld") or () if isinstance(p, str)],
            "source_kept": result.get("source_kept") is True,
            "warnings": result.get("warnings") or [],
            "exit_code": exit_code(row),
        }
        if params.get("bundle_path"):
            path = Path(params["bundle_path"])
            out["bundle"] = {"path": str(path), "size": _size_of(path)}
        if plan is not None:
            out.update(_plan_lists(plan))
        _left_behind(out, result)
        if ack is not None:
            arrival = self.ledger.arrival_for(str(ack.get("session_id") or ""))
            if arrival is not None:
                out["note"] = arrival.get("note")
                out["origin"] = self._origin(row)
        return out

    def _origin(self, row: dict) -> str | None:
        """Where a session that arrived here comes from (SEC 4.6): a pull from a paired peer (its responses are
        signed), or the bundle an import read (journaled by ``_record_origin``)."""
        if row["mode"] == str(Mode.PULL):
            return "verified peer"
        if row["mode"] == str(Mode.IMPORT):
            found = [e for e in self.ledger.events(row["job_id"], Role.CONDUCTOR) if e["kind"] == "origin"]
            return str((found[-1]["data"] or {}).get("origin")) if found else "bundle"
        return None

    def create_offer(
        self,
        peer: Peer,
        session_ref: SessionRef,
        *,
        ctx,
        local_source: LocalSource | None = None,
        note: str | None = None,
        options: Options | None = None,
        side_question: Callable[[str], str | None] | None = None,
        include_secret: tuple[str, ...] = (),
    ) -> dict:
        """``/handover to <dial-only peer>`` on the source: store an offer with a refinement computed by the
        session's own model now (DESIGN §2.2 ``offer``, §8.3); the peer claims it with ``/handover take``. Offers
        are never auto-claimed (O6). ``include_secret`` is the local ``--include-secret`` (X07, R18)."""
        if peer.direction != "inbound" or peer.revoked_at is not None:
            raise BadRequest("offers are for paired devices that connect to this gateway", field="peer")
        source = (
            local_source
            or make_local_apis(
                ctx,
                calling_session=session_ref.id,
                session_key=None,
                side_question=side_question,
                include_secret=tuple(include_secret),
            )[0]
        )
        offer_id = ulid()
        job = JobContext(
            job_id=offer_id,
            mode=Mode.OFFER,
            peer=peer,
            options=options or Options(),
            epoch=0,
            reporter=_NullReporter(),
            cancelled=lambda: False,
            created_at=self._clock(),
        )
        facts = source.facts(job)
        st = source.start_plan(job, facts, session_ref)
        while st.state.value == "running":
            time.sleep(0.2)
            st = source.op_status(job, st.op_id)
        refinement = None
        if st.state.value == "done" and st.result:
            from .model import ResourcePlan

            plan = from_wire(ResourcePlan, st.result)
            if plan.refine.used not in ("off", "none", ""):
                refinement = to_wire(
                    Refinement(
                        add=tuple(
                            (i.source_path, ", ".join(i.reasons)) for i in plan.items if i.origin.value == "model"
                        ),
                        drop=(),
                        tools_needed=plan.tools_needed,
                        env_vars_needed=plan.env_vars_needed,
                        notes=plan.notes_for_target,
                    )
                )
        planned = _journal_ctx(self.ledger, offer_id, Role.SOURCE).get("planned_info")
        # the session the plan resolved (its tip): an offer from the calling session has no id in ``session_ref``
        sid = str(planned.get("tip_id") or "") if isinstance(planned, dict) else ""
        sid = sid or str(session_ref.id or "")
        self._close_offer_row(offer_id)
        if not sid:
            raise error_from(st.error) if st.error else BadRequest("no session given", field="session_ref")
        expires = self._clock() + OFFER_TTL_S
        self.ledger.offer_put(offer_id, peer.peer_id, sid, note, refinement, expires)
        return {
            "offer_id": offer_id,
            "peer": peer.name,
            "session": sid,
            "expires_at": expires,
            "has_refinement": refinement is not None,
        }

    def _close_offer_row(self, offer_id: str) -> None:
        """The source row the offer's planning opened (``LocalSource._ensure_job``) is no job: it ends ``released``
        at once, so it neither counts against the peer's job caps nor shows up in ``status`` as a running source
        (L3-F07: an open offer held one of the two slots of its peer). The offer itself lives in ``offers``."""
        with contextlib.suppress(HandoverError):
            row = self.ledger.job(offer_id, Role.SOURCE)
            if row is None or row["state"] in TERMINAL[Role.SOURCE]:
                return
            owner = process_owner()
            epoch = self.ledger.claim_owner(offer_id, Role.SOURCE, owner, STALE_OWNER_S)
            if epoch is None:
                return
            self.ledger.transition(offer_id, Role.SOURCE, "*", "released", {"reason": "offer"}, epoch=epoch)
            self.ledger.release_owner(offer_id, Role.SOURCE, owner, epoch)


# ---------------------------------------------------------------------------------------------------------
# janitor, gc, lazy start
# ---------------------------------------------------------------------------------------------------------


def janitor(
    ledger: Ledger,
    runner: JobRunner | None,
    registry: ParticipantRegistry | None,
    *,
    flush_outbox: Callable[[Ledger], object] | None,
    background: bool = True,
    store=None,
    gc_interval_s: float = GC_INTERVAL_S,
    data_dir: Path | None = None,
) -> dict:
    """DESIGN §3.6: expired pairing codes, journal compaction, then every job whose owner went stale; COMMITTED/
    RETIRING conductor jobs are retried on every run ("the source is released on the next contact"). With a
    ``store`` the block-store GC of DESIGN §5.1 runs too (blocks beyond ``block_cache_days``/``block_cache_gb`` and
    the manifests of objects no non-terminal job references, R28) — at most every ``gc_interval_s`` for all
    processes of the profile (``Ledger.claim_interval``). Retention (BUILD-ORDER §6 Nr. 21, review H4): the ledger
    rows that otherwise grow without bound (``Ledger.purge_retention``) and, with ``data_dir``, the source staging a
    finished hold left behind (``local_source.purge_source_staging``) and note reports older than 30 days
    (``local_target.purge_reports``). Returns what it did (for ``status``/``doctor``)."""
    done: dict = {"codes": 0, "compacted": 0, "conductor": [], "participant": [], "gc": None}
    now = ledger.now()
    with contextlib.suppress(HandoverError):
        done["codes"] = ledger.purge_expired_codes(now)
    with contextlib.suppress(HandoverError):
        done["compacted"] = ledger.compact_terminal(JOURNAL_KEEP_S)
    done.update(_retention(ledger, data_dir))
    stale = ledger.stale_jobs(STALE_OWNER_S)
    seen = set()
    for row in stale:
        role = Role(row["role"])
        if role is Role.CONDUCTOR:
            if runner is None or (row["state"] == "suspended" and not ledger.cancel_requested(row["job_id"])):
                continue
            seen.add(row["job_id"])
            done["conductor"].append((row["job_id"], row["state"]))
            runner.recover(row["job_id"], background=background)
        elif registry is not None:
            try:
                done["participant"].append((row["job_id"], str(role), registry.recover(row)))
            except HandoverError as exc:
                done["participant"].append((row["job_id"], str(role), f"error:{exc.code}"))
    if runner is not None:
        for row in ledger.jobs(role=Role.CONDUCTOR, active=True, limit=1000):
            if row["job_id"] in seen or row["owner"] is not None:
                continue
            cancelled_suspended = row["state"] == str(ConductorState.SUSPENDED) and ledger.cancel_requested(
                row["job_id"]
            )  # a cancel whose abort did not finish (its process ended): abort it now (L3-F07)
            if cancelled_suspended or row["state"] in (
                str(ConductorState.COMMITTED),
                str(ConductorState.RETIRING),
                str(ConductorState.IN_DOUBT),
                str(ConductorState.ABORTING),
            ):
                done["conductor"].append((row["job_id"], row["state"]))
                runner.recover(row["job_id"], background=background)
    if store is not None:
        done["gc"] = _gc_store(ledger, store, gc_interval_s)
    if flush_outbox is not None:
        with contextlib.suppress(Exception):
            flush_outbox(ledger)
    return done


def _retention(ledger: Ledger, data_dir: Path | None) -> dict:
    """The janitor's retention step (review H4); a failure is logged and named, never raised (like ``_gc_store``)."""
    out: dict = {"retention": None, "notes": None, "staging": None, "reports": None}
    steps = [
        ("retention", lambda: ledger.purge_retention(JOURNAL_KEEP_S)),
        ("notes", lambda: ledger.purge_delivered(JOURNAL_KEEP_S)),  # delivered notes, pull log (H4 rest)
    ]
    if data_dir is not None:
        root = Path(data_dir)
        steps += [
            ("staging", lambda: purge_source_staging(ledger, root, lock_dir=root / "locks")),
            ("reports", lambda: purge_reports(ledger, root, keep_s=JOURNAL_KEEP_S)),
        ]
    for key, step in steps:
        try:
            out[key] = step()
        except Exception as exc:  # cleanup must not break the job that triggered the janitor
            log.warning("ctx_handover retention step %s failed: %s", key, type(exc).__name__, exc_info=True)
            out[key] = f"error:{type(exc).__name__}"
    return out


def _gc_store(ledger: Ledger, store, interval_s: float) -> int | str | None:
    """The janitor's GC step: freed block count, ``None`` when another run within ``interval_s`` did it, or
    ``"error:<type>"`` (logged; the janitor never fails a job start over its cleanup)."""
    try:
        if not ledger.claim_interval(GC_META_KEY, ledger.now(), interval_s):
            return None
        return int(store.gc(keep_days=float(store.cache_days), max_bytes=int(store.cache_bytes)))
    except Exception as exc:  # cleanup must not break the job that triggered the janitor
        log.warning("ctx_handover block-store GC failed: %s", type(exc).__name__, exc_info=True)
        return f"error:{type(exc).__name__}"


def gc_job(ledger: Ledger, job_id: str, *, data_dir: Path, store=None) -> None:
    """``/handover gc --job``: blocks, staging (incl. quarantine), report, continuation files and journal of a
    finished job (SEC 6.3). The block store releases first — it finds the job's objects through ``job_objects``,
    which ``purge_job`` removes."""
    for role in (Role.CONDUCTOR, Role.SOURCE, Role.TARGET):
        row = ledger.job(job_id, role)
        if row is not None and row["state"] not in (
            "done",
            "aborted",
            "released",
            "reclaimed",
            "retired",
            "exported",
            "committed",
            "expired",
        ):
            raise InvalidState(f"job {short(job_id)} is still {row['state']}", reason="active", state=row["state"])
    src = ledger.source_state(job_id)
    if src is not None and src["state"] not in ("released", "reclaimed", "retired", "exported"):
        # the job's rows say "over" only while its hold agrees (R3-H1 sweep): a source handover that still holds the
        # session (a lock whose job row an earlier release had ended) is no finished job
        raise InvalidState(f"job {short(job_id)} is still {src['state']}", reason="active", state=src["state"])
    if store is not None and callable(getattr(store, "release_job", None)):
        store.release_job(job_id, keep=False)
    shutil.rmtree(Path(data_dir) / "staging" / job_id, ignore_errors=True)
    for path in (Path(data_dir) / "reports" / f"{job_id}.md", *_continuation_files(Path(data_dir) / "logs", job_id)):
        with contextlib.suppress(FileNotFoundError):
            path.unlink()
    ledger.purge_job(job_id)


def _target_session(ack: dict | None, arrived: str | None) -> dict | None:
    """L17 (A part): the session the handover created on the target — ``{"id", "title"}`` from the target's ack
    (``arrived``: the session id this host's arrival row names, a target without its journaled ack yet); None before
    the commit. Surfaces name it in the final message and say how to open it (F)."""
    sid = (ack or {}).get("session_id") or arrived
    if not isinstance(sid, str) or not sid:
        return None
    title = (ack or {}).get("title")
    return {"id": sid, "title": title if isinstance(title, str) else None}


def _journal_ctx(ledger: Ledger, job_id: str, role: Role) -> dict:
    """The journaled context of one side of a job (every ``ctx`` dict of its events, in order)."""
    out: dict = {}
    for e in ledger.events(job_id, role):
        data = e["data"] or {}
        if isinstance(data.get("ctx"), dict):
            out.update(data["ctx"])
    return out


def _continuation_files(log_dir: Path, job_id: str) -> tuple[Path, ...]:
    """The files a job's continuation leaves (log and sidecar) as ``continuation.job_files`` names them — the one
    place that knows the names; the log alone with an older ``continuation`` module."""
    from . import continuation

    names = getattr(continuation, "job_files", None)
    if callable(names):
        return tuple(names(log_dir, job_id))
    return (log_dir / f"continue-{job_id}.log",)


def _flush(ctx) -> Callable[[Ledger], object] | None:
    if ctx is None or not callable(getattr(ctx, "call_mcp", None)):
        return None
    config = hostcompat.load_config()
    settings = plugin_settings(config)
    from . import ctxrecords

    server = ctxrecords.server_for(config, settings)  # one rule for ``ctx_persist`` and detection (X22)
    if server is None:
        return None
    category = str(settings["ctx_category"])

    def flush(ledger: Ledger) -> object:
        # the operator's grant (``mcp_allowlist``) is read at flush time, like ``/handover setup`` writes it: without
        # it ``call_mcp`` refuses, and every refusal would count as a failed attempt with backoff (review L21)
        if not ctxrecords.allowed(hostcompat.load_config(), server):
            return 0
        return ctxrecords.flush(ledger, ctx.call_mcp, server, category, ledger.now())

    return flush


def runner_for(ctx=None, reporter_factory: Callable[[str], Reporter] | None = None) -> JobRunner:
    """The profile's one ``JobRunner`` (created on first use; X19). A ``reporter_factory`` is bound when the runner
    has none yet — a runner created first by the janitor, a route or the CLI does not keep the surfaces' progress
    from arriving. The janitor runs before every job of this runner (``Runtime.janitor``)."""
    prof = _profile()
    with prof.lock:
        if ctx is not None:
            prof.ctx = ctx
        if prof.runner is None:
            prof.runner = JobRunner(prof.ledger, reporter_factory, before_job=prof.before_job)
        elif reporter_factory is not None:
            prof.runner.bind_reporter_factory(reporter_factory)
        return prof.runner


def ensure_started(
    ctx=None, reporter_factory: Callable[[str], Reporter] | None = None, *, janitor_background: bool = False
) -> Runtime:
    """First real use in a process (slash, CLI, route, tool, hook): returns the profile's one ``Runtime`` (X19,
    W109) with ``ledger``, ``veto`` (started, over the same ledger), ``store``, ``runner`` and ``janitor()``; runs the
    janitor once per process. Loads but never creates the identity (R16). Never called from ``register()``
    (BX L2). ``janitor_background`` (additive, review M6): that one janitor run — recovery, retention, GC, outbox
    flush — goes to a daemon thread ``ctxh-janitor`` once veto, identity and store stand, so the first hook of a
    process does not wait for it. The veto index has read the ledger once before any caller gets the runtime
    (review R2-H1)."""
    prof = _profile()
    runner_for(ctx, reporter_factory)
    with prof.lock:
        first = not prof.janitor_ran
        prof.janitor_ran = True
        if prof.veto is None:
            from . import lock

            # the index reads this runtime's ledger: no second Ledger object in the process (X19)
            prof.veto = lock.VetoIndex(prof.ledger.path, ledger_factory=lambda _path: prof.ledger)
        veto = prof.veto
    # every caller — the hooks' build first of all — returns only once the index has read the ledger (review R2-H1:
    # the first turn of a fresh process must meet the veto); ``start`` is idempotent and serialises that first read
    # itself, so it runs outside the profile lock and before the janitor, which stays in the background (M6)
    veto.start()
    _identity(prof)
    _store(prof)
    if first and janitor_background:
        ctx_copy = contextvars.copy_context()
        threading.Thread(target=ctx_copy.run, args=(_janitor_quietly, prof), name="ctxh-janitor", daemon=True).start()
    elif first:
        prof.janitor()
    return prof


def _janitor_quietly(prof: Runtime) -> None:
    """The background janitor of ``ensure_started``: logged, never raised (nothing waits for it)."""
    try:
        prof.janitor()
    except Exception:
        log.warning("ctx_handover janitor failed", exc_info=True)


def revoke_peer(name_or_id: str) -> list[str]:
    """``/handover peers revoke``: the ledger marks the peer revoked and its jobs ``aborting`` in one transaction,
    then the registry discards target staging and releases sources not yet prepared (DESIGN §12.5, W067)."""
    prof = _profile()
    ids = prof.ledger.revoke_peer(name_or_id)
    _registry(prof).abort_jobs(ids, "peer_revoked")
    return ids


def reclaim(session_id: str, *, force: bool) -> dict:
    """``/handover reclaim`` (DESIGN §7.7) through cluster B's ``lock.reclaim``; when this side was the source of a
    job this process dialed, the target is told ``source_reclaimed`` best effort (route #8, W047)."""
    from . import lock

    prof = _profile()
    db = hostcompat.session_db(hostcompat.state_db_path())
    try:
        out = dict(lock.reclaim(db, prof.ledger, session_id, force=force) or {})
    finally:
        with contextlib.suppress(Exception):
            db.close()
    job_id = out.get("job_id")
    _reclaim_outcome(prof, out)
    row = prof.ledger.job(str(job_id), Role.CONDUCTOR) if job_id else None
    if row is not None and row["mode"] == str(Mode.EXPORT):
        if out.get("state") in ("reclaimed", "released"):
            _settle_export(prof, row, out)
        return out
    if job_id and out.get("state") == "reclaimed":
        out["target_notified"] = _notify_reclaimed(prof, str(job_id))
    return out


def _settle_export(prof: _Profile, row: dict, out: dict) -> None:
    """``reclaim`` of an exported session (V39, R1/D8): an export that has not ended is settled now — the conductor
    sees the reclaimed source and aborts a bundle that was never committed (its ``.partial`` is removed) — by this
    process's runner thread of the job if one runs, else by a recovery here (a live owner in another process does
    it on its next poll). The warnings speak of the bundle: an export has no target "on another machine"."""
    job_id = row["job_id"]
    if row["state"] not in _STOP:
        runner = runner_for(None)
        with runner._lock:
            thread = runner._threads.get(job_id)
        if thread is not None and thread.is_alive():
            thread.join(RECLAIM_SETTLE_S)
        else:
            with contextlib.suppress(HandoverError):
                runner.recover(job_id, background=False)
        row = prof.ledger.job(job_id, Role.CONDUCTOR) or row
    path = (row.get("params") or {}).get("bundle_path")
    shown = path or "the bundle"
    warnings = [w for w in out.get("warnings") or () if not str(w).startswith(("the copy on ", "the target on "))]
    if row["state"] == "aborted":
        warnings.append(f"the unfinished bundle {shown} was removed; nothing was exported")
    elif row["state"] == "done":
        if path and Path(path).exists():
            warnings.append(
                f"the bundle {path} stays a valid copy; importing it elsewhere makes a second, independent copy"
            )
    else:
        warnings.append(
            f"the export (job {short(job_id)}) has not ended yet ({row['state']}); /handover status "
            f"{short(job_id)} shows what becomes of {shown}"
        )
    out["warnings"] = warnings


_RECLAIM_REFUSALS = {
    "prepared": "committing",
    "retiring": "committing",
    "locking": "in_progress",
    "locked": "in_progress",
    "preparing": "in_progress",
}


def _reclaim_outcome(prof: _Profile, out: dict) -> None:
    """L3-F14 (A part): ``ok`` is True only when the session is active here again (``reclaimed``/``released``); a
    refusal or "nothing to reclaim" carries ``ok: False`` and ``error`` (``code``, ``message``, ``details.reason``:
    ``committing`` — prepared/retiring without ``--force`` —, ``in_progress`` — use cancel —, or
    ``nothing_to_reclaim``) so the CLI's ``--json`` and exit code can show it."""
    out["ok"] = out.get("state") in ("reclaimed", "released")
    if out["ok"]:
        return
    job_id = out.get("job_id")
    src = None
    with contextlib.suppress(HandoverError):
        src = prof.ledger.source_state(str(job_id)) if job_id else None
    state = str(src["state"]) if src else None
    reason = _RECLAIM_REFUSALS.get(state or "", "nothing_to_reclaim")
    code = "invalid_state" if reason != "nothing_to_reclaim" else "not_found"
    out["error"] = {
        "code": code,
        "message": str(out.get("message") or "Nothing to reclaim."),
        "details": {"reason": reason, "state": state, "job": short(str(job_id)) if job_id else None},
    }


NOTIFY_TIMEOUT_S = 5.0
"""Read timeout of the ``source_reclaimed`` notice (one attempt, L3-F16)."""
NOTIFY_WAIT_S = 5.0
"""How long ``reclaim`` waits for that notice before it answers; the notice is best effort (DESIGN §7.7)."""


def _notify_reclaimed(prof: _Profile, job_id: str) -> bool:
    """Tell the target of a job this host dialed that the source took the session back (route #8, best effort,
    DESIGN §7.7). Bounded (L3-F16): one attempt with a short read timeout in a background thread, ``reclaim`` waits
    at most ``NOTIFY_WAIT_S`` — a target that does not answer (stopped, in doubt) never holds the reclaim; True only
    when the target accepted the notice in time."""
    row = prof.ledger.job(job_id, Role.CONDUCTOR)
    peer = prof.ledger.peer(row["peer_id"]) if row and row["peer_id"] else None
    identity = _identity(prof)
    if row is None or peer is None or peer.direction != "dial" or identity is None:
        return False
    from . import wire

    epoch = int(row["owner_epoch"])
    delivered = threading.Event()

    def send() -> None:
        try:
            client = wire.WireClient(
                peer, identity, read_secret=hostcompat.read_secret, timeout=NOTIFY_TIMEOUT_S, max_attempts=1
            )
            try:
                body = {"epoch": epoch, "note": "source_reclaimed"}
                client.call("POST", f"jobs/{job_id}/status", body, epoch=epoch)
            finally:
                with contextlib.suppress(Exception):
                    client.close()
        except HandoverError:
            return
        except Exception:  # a notice must never break the reclaim that already happened
            log.warning("ctx_handover source_reclaimed notice failed", exc_info=True)
            return
        delivered.set()

    ctx = contextvars.copy_context()  # the profile scope (secrets, HERMES_HOME) travels with the thread (V33)
    thread = threading.Thread(target=ctx.run, args=(send,), name=f"ctxh-reclaimed-{short(job_id)}", daemon=True)
    thread.start()
    thread.join(NOTIFY_WAIT_S)
    return delivered.is_set()


def default_bundle_path(
    title: str | None,
    options: Options,
    now: float | None = None,
    *,
    home: Path | None = None,
    hermes_home: Path | None = None,
    hermes_root: Path | None = None,
) -> Path:
    """``<workspace_root>/<title-slug>-<YYYYmmdd-HHMMSS>.ctxh.tar`` (DESIGN §5.4, W127); a directory with ``--dir``.
    With ``home`` and ``hermes_home`` the root resolves like the target's (``paths.expand_handover_root``, R29): the
    default ``~/hermes-handover`` inside the Hermes home is ``<HERMES_HOME>/handover-workspaces``."""
    import re
    from datetime import datetime

    from .paths import expand_handover_root

    slug = re.sub(r"[^A-Za-z0-9]+", "-", str(title or "session")).strip("-").lower()[:48] or "session"
    stamp = datetime.fromtimestamp(now if now is not None else time.time()).strftime("%Y%m%d-%H%M%S")
    root = Path(os.path.expanduser(options.workspace_root))
    if home is not None and hermes_home is not None:
        with contextlib.suppress(HandoverError):
            root = expand_handover_root(options.workspace_root, home, hermes_home, hermes_root=hermes_root)
    return root / (f"{slug}-{stamp}" + ("" if options.as_dir else ".ctxh.tar"))


__all__ = [
    "JobRunner",
    "Runtime",
    "apis_for",
    "ensure_started",
    "exit_code",
    "gc_job",
    "janitor",
    "listener_server",
    "local_options",
    "make_local_apis",
    "plugin_settings",
    "runner_for",
]
