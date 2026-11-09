"""Composition roots, the job runner and the janitor (layer L9; INTERFACES §4.5, DESIGN §2.1, §3.6, §14.1).

This is the only place where the engine is wired to Hermes (through ``hostcompat``) and to the transport
(cluster C). Three roots:

* ``make_local_apis`` — ``LocalSource``/``LocalTarget`` for this profile (slash handler, CLI, routes);
* ``apis_for`` — the ``SourceAPI``/``TargetAPI`` pair of a mode: local, remote (``remote.Remote*`` over a
  ``wire.WireClient``) or bundle (``bundle.Bundle*``);
* ``listener_server`` — the dashboard routes' ``server.Server`` with its ``ParticipantRegistry``.

Everything is lazy and per profile: nothing runs at import or in ``register()`` (R12, BX L2). The first real use
in a process (``ensure_started``) opens the ledger, starts the veto index and runs the janitor once; the janitor
runs again before every new job and on ``status``.

``JobRunner`` owns conductor jobs of this process: ``plan_sync`` runs CREATED … NEGOTIATED in the caller (the
slash handler can ask the user), ``continue_async`` hands the job to a worker thread that runs in a copy of the
caller's context (profile-scoped helpers resolve correctly there, DESIGN §2.1 consequence 6).
"""

from __future__ import annotations

import contextlib
import contextvars
import os
import secrets
import shutil
import sys
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from types import SimpleNamespace

from . import hostcompat
from .conductor import Conductor, PollPolicy
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
from .ledger import JOURNAL_KEEP_S, STALE_OWNER_S, Ledger, private_dir, process_owner
from .local_source import LocalSource
from .local_target import LocalTarget
from .model import (
    CommitAck,
    ConductorState,
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
from .participant import ParticipantRegistry
from .policy import PolicyConfig

PLUGIN_ID = "ctx_handover"
OFFER_TTL_S = 2 * 3600
MAINTENANCE_S = 10.0

EXIT_OK, EXIT_FAILED, EXIT_USAGE, EXIT_BUSY, EXIT_AUTH, EXIT_PENDING = 0, 1, 2, 3, 4, 5
_STOP = frozenset({"done", "aborted", "suspended"})


# ---------------------------------------------------------------------------------------------------------
# settings → options (DESIGN §14.4)
# ---------------------------------------------------------------------------------------------------------


def plugin_settings(config: dict) -> dict:
    """``plugins.entries.ctx_handover.settings`` of a loaded config (empty when absent)."""
    try:
        value = config["plugins"]["entries"][PLUGIN_ID]["settings"]
    except (KeyError, TypeError):
        return {}
    return dict(value) if isinstance(value, dict) else {}


def _num(settings: dict, key: str, default: float) -> float:
    value = settings.get(key, default)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return default
    return value


def local_options(settings: dict) -> Options:
    """The local defaults of a job from the plugin settings (the participant side's own limits, §14.4)."""
    base = Options()
    roots = settings.get("allow_roots")
    export_roots = settings.get("export_roots")
    mib, gib = 1024**2, 1024**3
    return Options(
        workspace_root=str(settings.get("workspace_root") or base.workspace_root),
        allow_roots=tuple(str(r) for r in roots) if isinstance(roots, list) and roots else base.allow_roots,
        export_roots=tuple(str(r) for r in export_roots) if isinstance(export_roots, list) else (),
        max_session_bytes=int(_num(settings, "max_session_mb", 512) * mib),
        max_file_bytes=int(_num(settings, "max_file_mb", 100) * mib),
        max_extra_dir_bytes=int(_num(settings, "max_extra_dir_mb", 256) * mib),
        max_hermes_file_bytes=int(_num(settings, "max_hermes_file_mb", 64) * mib),
        max_files=int(_num(settings, "max_files", base.max_files)),
        max_untracked_files=int(_num(settings, "max_untracked_files", base.max_untracked_files)),
        max_total_bytes=int(_num(settings, "max_total_gb", 10) * gib),
        confirm_above_bytes=int(_num(settings, "confirm_above_mb", 500) * mib),
        staging_ttl_s=int(_num(settings, "staging_ttl_hours", 24) * 3600),
        lease_ttl_s=int(_num(settings, "lease_ttl_s", base.lease_ttl_s)),
        participant_idle_timeout_s=int(_num(settings, "participant_idle_timeout_min", 15) * 60),
        stall_timeout_s=int(_num(settings, "stall_timeout_min", 30) * 60),
        wait_timeout_s=int(_num(settings, "wait_timeout_min", 30) * 60),
        refine=str(settings.get("refine") or base.refine),
        refine_timeout_s=int(_num(settings, "refine_timeout_s", base.refine_timeout_s)),
        refine_history_chars=int(_num(settings, "refine_history_chars", base.refine_history_chars)),
        refine_side_question_max_tokens=int(
            _num(settings, "refine_side_question_max_tokens", base.refine_side_question_max_tokens)
        ),
        refine_auto_include=bool(settings.get("refine_auto_include", False)),
        extra_paths=str(settings.get("extra_paths") or base.extra_paths),
        include_hermes_files=bool(settings.get("include_hermes_files", True)),
        model_mapping=str(settings.get("model_mapping") or base.model_mapping),
        extra_ignore=tuple(str(x) for x in settings.get("extra_ignore") or ()),
        tool_probe=tuple(str(x) for x in settings.get("tool_probe") or ()),
    )


# ---------------------------------------------------------------------------------------------------------
# per-profile process state (lazy; never touched by register())
# ---------------------------------------------------------------------------------------------------------


@dataclass
class _Profile:
    data_dir: Path
    ledger: Ledger
    ops: OpRunner
    store: object | None = None
    git: object | None = None
    identity: object | None = None
    identity_error: str | None = None
    veto: object | None = None
    registry: ParticipantRegistry | None = None
    runner: JobRunner | None = None
    janitor_ran: bool = False
    maintenance: threading.Thread | None = None
    lock: threading.RLock = field(default_factory=threading.RLock)


_PROFILES: dict[str, _Profile] = {}
_PROFILES_LOCK = threading.Lock()


def _profile() -> _Profile:
    data_dir = private_dir(hostcompat.plugin_dir())
    key = str(data_dir)
    with _PROFILES_LOCK:
        prof = _PROFILES.get(key)
        if prof is None:
            ledger = Ledger(data_dir / "ledger.sqlite3")
            prof = _Profile(data_dir=data_dir, ledger=ledger, ops=OpRunner(ledger, hostcompat.run_in_context))
            _PROFILES[key] = prof
        return prof


def _reset_for_tests() -> None:
    """Drop the per-process caches (tests only)."""
    with _PROFILES_LOCK:
        for prof in _PROFILES.values():
            prof.ops.stop()
            prof.ledger.close()
        _PROFILES.clear()


def _store(prof: _Profile):
    with prof.lock:
        if prof.store is None:
            from . import blocks

            prof.store = blocks.BlockStore(private_dir(prof.data_dir / "blocks"), prof.ledger)
        return prof.store


def _git(prof: _Profile):
    with prof.lock:
        if prof.git is None:
            from . import gitops

            prof.git = gitops.Git()
        return prof.git


def _identity(prof: _Profile):
    """Load (never create) this profile's identity (R16); ``None`` when there is none yet or it is broken."""
    with prof.lock:
        if prof.identity is None and prof.identity_error is None:
            from . import crypto

            try:
                prof.identity = crypto.Identity.load(
                    prof.ledger, read_secret=hostcompat.read_secret, read_env_fresh=hostcompat.read_env_fresh
                )
            except HandoverError as exc:
                prof.identity_error = exc.code
        return prof.identity


def _policy(opts: Options, prof: _Profile, hermes, *, include_secret=()) -> PolicyConfig:
    return PolicyConfig.from_options(
        opts,
        home=Path.home(),
        hermes_home=Path(hermes.home),
        hermes_root=hostcompat.hermes_root(),
        plugin_dir=prof.data_dir,
        case_insensitive=sys.platform in ("darwin", "win32"),
        target_os="windows" if os.name == "nt" else ("macos" if sys.platform == "darwin" else "linux"),
        include_secret=include_secret,
        document_cache=Path(hermes.document_cache),
    )


def _side_question(ctx, features) -> Callable[[str], str | None] | None:
    if ctx is None or not features.side_question:
        return None
    token = getattr(ctx, "side_question_token", None)

    def ask(prompt: str) -> str | None:
        try:
            return ctx.side_question(token, prompt, timeout=45.0) if token else None
        except Exception:
            return None

    return ask


def _llm(ctx) -> Callable | None:
    if ctx is not None and getattr(ctx, "llm", None) is not None:
        complete = getattr(ctx.llm, "complete_structured", None)
        if callable(complete):

            def call(instructions: str, input_blocks: list, schema: dict, timeout_s: int):
                return complete(
                    instructions=instructions,
                    input=input_blocks,
                    json_schema=schema,
                    timeout=timeout_s,
                    purpose="ctx_handover.refine",
                )

            return call
    return hostcompat.plugin_llm()


def make_local_apis(
    ctx_or_none,
    *,
    calling_session: str | None,
    session_key: str | None,
    ports: SimpleNamespace | None = None,
    remote_driven: bool = False,
    peer: Peer | None = None,
    include_secret=(),
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
    git = _git(prof)
    source_ports = getattr(ports, "source", None) if ports is not None else None
    target_ports = getattr(ports, "target", None) if ports is not None else None
    source = LocalSource(
        ledger=prof.ledger,
        store=store,
        db_factory=hostcompat.session_db,
        hermes=hermes,
        host=host,
        config=config,
        ops=prof.ops,
        git=git,
        calling_session=calling_session,
        session_key=session_key,
        side_question=_side_question(ctx_or_none, features),
        llm=_llm(ctx_or_none),
        read_secret=hostcompat.read_secret,
        redact=hostcompat.redact,
        state_db=hostcompat.state_db_path(),
        remote_driven=remote_driven,
        policy=policy,
        ports=source_ports,
        data_dir=prof.data_dir,
        peer_name=peer.name if peer is not None else None,
    )
    from . import continuation

    spawner = continuation.Spawner(
        log_dir=private_dir(prof.data_dir / "logs"),
        profile=host.profile if host.profile not in ("", "default") else None,
        approvals_off=lambda: hostcompat.approvals_off(hostcompat.load_config()),
    )
    target = LocalTarget(
        ledger=prof.ledger,
        store=store,
        db_factory=hostcompat.session_db,
        hermes=hermes,
        host=host,
        config=config,
        ops=prof.ops,
        git=git,
        provider_usable=hostcompat.provider_usable,
        continuation=spawner,
        redact=hostcompat.redact,
        source_peer_id=peer.peer_id if peer is not None else None,
        peer_verified=peer is not None,
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
        return local_source, t.bundle.BundleTarget(
            Path(bundle_path), identity, as_dir=options.as_dir, declared_facts=None, ledger=ledger
        )
    if mode is Mode.IMPORT:
        if local_target is None or bundle_path is None:
            raise BadRequest("import needs the local target and a bundle path", field="bundle")
        trusted = {p.pubkey for p in ledger.peers() if p.revoked_at is None}
        return (
            t.bundle.BundleSource(
                Path(bundle_path), trusted_pubkeys=trusted, trust_unsigned=options.trust_unsigned, ledger=ledger
            ),
            local_target,
        )
    raise BadRequest(f"mode {mode} has no conductor", field="mode")


def listener_server():
    """The ``server.Server`` behind ``dashboard/plugin_api.py`` (cached per profile; starts the maintenance
    sweep of remote-driven participants)."""
    prof = _profile()
    with prof.lock:
        if getattr(prof, "server", None) is not None:
            return prof.server  # type: ignore[attr-defined]
        from . import server

        identity = _identity(prof)
        registry = _registry(prof)
        settings = plugin_settings(hostcompat.load_config())
        srv = server.Server(
            prof.ledger,
            identity,
            registry,
            settings,
            [server.PeerSignatureAuth(prof.ledger, identity), server.OperatorSessionAuth(enabled=False)],
        )
        prof.server = srv  # type: ignore[attr-defined]
    _start_maintenance(prof)
    return srv


def _registry(prof: _Profile) -> ParticipantRegistry:
    with prof.lock:
        if prof.registry is None:

            def factory(role: Role, peer: Peer | None):
                source, target = make_local_apis(
                    None, calling_session=None, session_key=None, remote_driven=True, peer=peer
                )
                return source if role is Role.SOURCE else target

            prof.registry = ParticipantRegistry(
                prof.ledger,
                factory,
                local_options=lambda: local_options(plugin_settings(hostcompat.load_config())),
            )
        return prof.registry


def _start_maintenance(prof: _Profile) -> None:
    with prof.lock:
        if prof.maintenance is not None and prof.maintenance.is_alive():
            return
        registry = _registry(prof)
        ctx = contextvars.copy_context()

        def loop() -> None:
            while True:
                time.sleep(MAINTENANCE_S)
                with contextlib.suppress(Exception):
                    registry.sweep(prof.ledger.now())

        prof.maintenance = threading.Thread(target=ctx.run, args=(loop,), name="ctxh-maintenance", daemon=True)
        prof.maintenance.start()


# ---------------------------------------------------------------------------------------------------------
# the job runner
# ---------------------------------------------------------------------------------------------------------


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
        )
        prof = _profile()
        bundle = params.get("bundle_path")
        return apis_for(
            mode,
            peer,
            local_source=source,
            local_target=target,
            identity=_identity(prof),
            bundle_path=Path(bundle) if bundle else None,
            options=options,
            ledger=self.ledger,
        )

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

    def _release(self, job_id: str) -> None:
        with self._lock:
            mine = self._owners.pop(job_id, None)
        if mine is not None:
            with contextlib.suppress(HandoverError):
                self.ledger.release_owner(job_id, Role.CONDUCTOR, mine[0], mine[1])

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
        source, target = self._apis_factory(row)
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
    ) -> tuple[str, FinalPlan | None]:
        """Create the job and run it to NEGOTIATED with the user's confirmation (synchronous; the slash handler
        can ask). Returns ``(job_id, FinalPlan)``; ``None`` when the job ended there (``show_plan``/``cancel``).
        Other failures are raised after the job was aborted (nothing locked)."""
        if self._before_job is not None:
            self._before_job()
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
        conductor = self._conductor(job_id, confirm=confirm)
        try:
            final = conductor.run_until_negotiated()
        except Cancelled:
            self._release(job_id)
            return job_id, None
        except HandoverError:
            self._release(job_id)
            raise
        return job_id, final

    def _run(self, job_id: str, how: str) -> None:
        try:
            conductor = self._conductor(job_id)
            getattr(conductor, how)()
        except HandoverError:
            pass
        finally:
            self._release(job_id)
            with self._lock:
                self._threads.pop(job_id, None)
                self._ctxs.pop(job_id, None)

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
        try:
            self.ledger.set_cancel(job_id)
        except InvalidState as exc:
            return exc.message
        return f"Cancel requested for job {short(job_id)}."

    def status(self, job_id: str | None) -> dict:
        """Machine-readable status (F renders it): active and last jobs, or one job with its journal tail."""
        if job_id is None:
            jobs = self.ledger.jobs(role=Role.CONDUCTOR, limit=5)
            active = self.ledger.jobs(role=Role.CONDUCTOR, active=True, limit=20)
            return {
                "active": [self._summary(r) for r in active],
                "recent": [self._summary(r) for r in jobs],
                "pull_log": self.ledger.pull_log(5),
                "outbox_pending": len(self.ledger.outbox_due(self.ledger.now() + 10**9, limit=1000)),
            }
        row = self.ledger.job(job_id, Role.CONDUCTOR)
        if row is None:
            raise NotFound(f"job {short(job_id)} not found")
        out = self._summary(row)
        out["events"] = [
            {"kind": e["kind"], "state": e["state"], "step": e["step"], "at": e["ts"]}
            for e in self.ledger.events(job_id, Role.CONDUCTOR)[-20:]
        ]
        return out

    def _summary(self, row: dict) -> dict:
        result = row.get("result") or {}
        outcome = result.get("outcome")
        if outcome is None:
            outcome = "running" if row["state"] not in _STOP else row["state"]
        ack = result.get("ack")
        return {
            "job_id": row["job_id"],
            "short": short(row["job_id"]),
            "mode": row["mode"],
            "state": row["state"],
            "outcome": outcome,
            "peer_id": row["peer_id"],
            "restarts": row["restarts"],
            "last_error": row["last_error"],
            "ack": from_wire(CommitAck, ack) if isinstance(ack, dict) else None,
            "continue": result.get("continue"),
            "warnings": result.get("warnings") or [],
            "exit_code": exit_code(row),
        }

    def create_offer(
        self,
        peer: Peer,
        session_ref: SessionRef,
        *,
        ctx,
        local_source: LocalSource | None = None,
        note: str | None = None,
    ) -> dict:
        """``/handover to <dial-only peer>`` on the source: store an offer with a refinement computed by the
        session's own model now (DESIGN §2.2 ``offer``, §8.3); the peer claims it with ``/handover take``. Offers
        are never auto-claimed (O6)."""
        if peer.direction != "inbound" or peer.revoked_at is not None:
            raise BadRequest("offers are for paired devices that connect to this gateway", field="peer")
        source = local_source or make_local_apis(ctx, calling_session=session_ref.id, session_key=None)[0]
        offer_id = ulid()
        job = JobContext(
            job_id=offer_id,
            mode=Mode.OFFER,
            peer=peer,
            options=Options(),
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
        sid = session_ref.id or ""
        expires = self._clock() + OFFER_TTL_S
        self.ledger.offer_put(offer_id, peer.peer_id, sid, note, refinement, expires)
        return {
            "offer_id": offer_id,
            "peer": peer.name,
            "session": sid,
            "expires_at": expires,
            "has_refinement": refinement is not None,
        }


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
) -> dict:
    """DESIGN §3.6: expired pairing codes, journal compaction, then every job whose owner went stale; COMMITTED/
    RETIRING conductor jobs are retried on every run ("the source is released on the next contact"). Returns what
    it did (for ``status``/``doctor``)."""
    done: dict = {"codes": 0, "compacted": 0, "conductor": [], "participant": []}
    now = ledger.now()
    with contextlib.suppress(HandoverError):
        done["codes"] = ledger.purge_expired_codes(now)
    with contextlib.suppress(HandoverError):
        done["compacted"] = ledger.compact_terminal(JOURNAL_KEEP_S)
    stale = ledger.stale_jobs(STALE_OWNER_S)
    seen = set()
    for row in stale:
        role = Role(row["role"])
        if role is Role.CONDUCTOR:
            if runner is None or row["state"] in ("suspended",):
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
            if row["state"] in (
                str(ConductorState.COMMITTED),
                str(ConductorState.RETIRING),
                str(ConductorState.IN_DOUBT),
                str(ConductorState.ABORTING),
            ):
                done["conductor"].append((row["job_id"], row["state"]))
                runner.recover(row["job_id"], background=background)
    if flush_outbox is not None:
        with contextlib.suppress(Exception):
            flush_outbox(ledger)
    return done


def gc_job(ledger: Ledger, job_id: str, *, data_dir: Path, store=None) -> None:
    """``/handover gc --job``: blocks, staging (incl. quarantine), report, logs and journal of a finished job
    (SEC 6.3)."""
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
    if store is not None and callable(getattr(store, "release_job", None)):
        store.release_job(job_id, keep=False)
    shutil.rmtree(Path(data_dir) / "staging" / job_id, ignore_errors=True)
    for path in (Path(data_dir) / "reports" / f"{job_id}.md", Path(data_dir) / "logs" / f"continue-{job_id}.log"):
        with contextlib.suppress(FileNotFoundError):
            path.unlink()
    ledger.purge_job(job_id)


def _flush(ctx) -> Callable[[Ledger], object] | None:
    if ctx is None or not callable(getattr(ctx, "call_mcp", None)):
        return None
    config = hostcompat.load_config()
    settings = plugin_settings(config)
    if settings.get("ctx_persist") == "off":
        return None
    from . import ctxrecords

    server = ctxrecords.detect(config)
    if server is None:
        return None
    category = str(settings.get("ctx_category") or "handover")
    return lambda ledger: ctxrecords.flush(ledger, ctx.call_mcp, server, category, ledger.now())


def runner_for(ctx=None, reporter_factory: Callable[[str], Reporter] | None = None) -> JobRunner:
    """The profile's ``JobRunner`` (created on first use)."""
    prof = _profile()
    with prof.lock:
        if prof.runner is None:
            prof.runner = JobRunner(
                prof.ledger,
                reporter_factory,
                before_job=lambda: janitor(prof.ledger, prof.runner, _registry(prof), flush_outbox=_flush(ctx)),
            )
        return prof.runner


def ensure_started(ctx=None) -> None:
    """First real use in a process (slash, CLI, route, tool, hook): ledger, veto index, janitor once; loads but
    never creates the identity (R16). Never called from ``register()`` (BX L2)."""
    prof = _profile()
    with prof.lock:
        first = not prof.janitor_ran
        prof.janitor_ran = True
        if prof.veto is None:
            from . import lock

            prof.veto = lock.VetoIndex(prof.ledger.path)
            prof.veto.start()
    _identity(prof)
    if first:
        janitor(prof.ledger, runner_for(ctx), _registry(prof), flush_outbox=_flush(ctx))


__all__ = [
    "JobRunner",
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
