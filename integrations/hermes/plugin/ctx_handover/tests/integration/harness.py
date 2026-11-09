"""Two-home integration harness (layer 2, TESTPLAN §3): real modules, real ``SessionDB``, real git, real block
store, real HTTP listener process on loopback.

* ``Home`` — one Hermes installation: its own ``$HOME``, ``HERMES_HOME`` (and so its own ``state.db``, plugin data
  directory, ledger, block store and identity). ``home.active()`` switches this process's environment to it; the
  plugin's composition roots (``jobs``) resolve everything through ``hostcompat`` and pin it per profile.
* ``Listener`` — home A served by ``listener_app.py`` in its own process (the gateway / ``hermes serve`` stand-in
  without a gateway): the dashboard gate with a fixed session token in front of the plugin router.
* ``pair(server, laptop, listener)`` — identity on both sides, pairing code on A, redeem from B over HTTP.

Only one home is active in this process at a time; the other side of an online handover always runs in the
listener process. Settings (``config.yaml`` of each home) switch refinement off so no model is contacted.

Fault-matrix helpers (wp-int1; additive — the helpers above keep their behaviour, every integration test may use
these):

* ``Listener(home, faults=True, supervise=True)`` — ``arm(rules)``/``disarm()`` write the listener-side fault rules
  (``<control>/faults.json``, read by ``listener_app.py`` on every request), ``fired()`` lists what fired
  (``<control>/fired.jsonl``, kept across restarts). ``supervise`` restarts the process on the same port whenever it
  dies (``restarts`` counts that); ``restart()`` kills and restarts it on purpose; ``kill()`` stays a hard stop
  without restart, ``revive()`` starts it again on the same port; ``wait_ready()`` waits for the port.
* ``run_conductor(home, control, action=…)`` — the conductor in its own process (``python -m
  ctx_handover.tests.integration.harness conduct <spec.json>``): ``start`` (``JobRunner.plan_sync`` +
  ``run_foreground``), ``resume`` (``JobRunner.resume``) or ``janitor`` (``Runtime.janitor``: adopts stale jobs),
  optionally with conductor-side fault rules; returns a ``ChildRun`` (``killed`` when a crash rule SIGKILLed it).
  The process composes its participants exactly like the product (``JobRunner._default_apis``); only the poll and
  backoff intervals are shorter (``FAST_POLL``, ``WIRE_SLEEP_SCALE``) — staleness (60 s) and heartbeats stay real.
* ``rules_for(mode, side, method, kind)`` — where one fault of the matrix is injected (``SEAMS`` in the docstring of
  that function): conductor-side rules (crash = SIGKILL of the conductor process, cancel = the cancel verb run in a
  separate process, link faults in an httpx transport under the real ``WireClient``, errors of a local
  participant) and listener-side rules (error envelope of the participant, participant process killed after a
  request took effect or inside a running operation).
* ``InvariantMonitor`` — samples both ``state.db`` read-only every 20 ms and records every moment in which the
  target shows a new visible session while the source session lacks one of the four primary-lock marks.
* ``FaultWorld`` (fixture ``fault_world`` in ``conftest.py``) — ``run_case(mode, side, method, kind)`` runs one
  matrix case end to end (git workspace with uncommitted state + session at the source, faults armed, conductor
  started, then driven like the product — resume a suspended job, janitor adoption after the stale window, retries
  — until DONE or ABORTED) and returns a ``CaseResult``; ``CaseResult.problems()`` lists every broken promise.
* ``cancel_job(home, job_id)`` — the cancel verb (``JobRunner.cancel``, what ``/handover cancel`` runs) in a
  separate process; ``dirty_repo(root)``, ``make_session(home, cwd, title)`` — fixtures through the real paths.
"""

from __future__ import annotations

import contextlib
import contextvars
import functools
import json
import os
import re
import secrets
import signal
import socket
import sqlite3
import subprocess
import sys
import threading
import time
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from types import SimpleNamespace

import httpx

from ctx_handover.engine import crypto, hostcompat, jobs, pairing, wire
from ctx_handover.engine.model import GateAuth, GateMode, Mode, Options, Peer, Right, Role, SessionRef

PLUGIN_PARENT = Path(__file__).resolve().parents[3]
UPSTREAM = os.environ.get("CTXH_UPSTREAM") or next((p for p in sys.path if Path(p, "hermes_state.py").exists()), "")
GATE_ENV = "CTX_HANDOVER_PEER_TOKEN"


def child_env(home: Home) -> dict[str, str]:
    """Environment of a child process of ``home`` (listener, conductor, cancel verb)."""
    env = {k: v for k, v in os.environ.items() if not k.startswith(("HERMES_", "CTX_HANDOVER_", "GIT_"))}
    env.update(home.env())
    env["PYTHONPATH"] = os.pathsep.join(p for p in (UPSTREAM, str(PLUGIN_PARENT)) if p)
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return env


class Home:
    """One Hermes home (see module docstring)."""

    def __init__(self, base: Path, name: str, *, settings: dict | None = None) -> None:
        self.name = name
        self.root = Path(base) / name
        self.home = self.root / "home"
        self.hermes_home = self.root / "hermes-home"
        self.home.mkdir(parents=True, exist_ok=True)
        self.hermes_home.mkdir(parents=True, exist_ok=True)
        self.extra_env: dict[str, str] = {}
        self.write_settings({"refine": "off", **(settings or {})})

    @property
    def state_db(self) -> Path:
        return self.hermes_home / "state.db"

    def env(self) -> dict[str, str]:
        out = {"HOME": str(self.home), "USERPROFILE": str(self.home), "HERMES_HOME": str(self.hermes_home)}
        for name, rel in (
            ("XDG_CONFIG_HOME", ".config"),
            ("XDG_DATA_HOME", ".local/share"),
            ("XDG_CACHE_HOME", ".cache"),
            ("XDG_STATE_HOME", ".local/state"),
        ):
            out[name] = str(self.home / rel)
        out.update(self.extra_env)
        return out

    def write_settings(self, settings: dict, **config: object) -> None:
        """``config.yaml`` (JSON is valid YAML) with ``plugins.entries.ctx_handover.settings``."""
        doc = {"plugins": {"entries": {"ctx_handover": {"enabled": True, "settings": settings}}}, **config}
        (self.hermes_home / "config.yaml").write_text(json.dumps(doc), encoding="utf-8")

    @contextlib.contextmanager
    def active(self) -> Iterator[Home]:
        saved = {k: os.environ.get(k) for k in (*self.env(), GATE_ENV)}
        os.environ.pop(GATE_ENV, None)
        os.environ.update(self.env())
        try:
            yield self
        finally:
            for k, v in saved.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v

    def db(self):
        return hostcompat.session_db(self.state_db)

    def ledger(self):
        with self.active():
            return jobs._profile().ledger

    def data_dir(self) -> Path:
        with self.active():
            return jobs._profile().data_dir

    def identity(self):
        """This home's identity (created like an operator verb does, R16)."""
        with self.active():
            ledger = jobs._profile().ledger
            ident = crypto.Identity.load(
                ledger, read_secret=hostcompat.read_secret, read_env_fresh=hostcompat.read_env_fresh
            )
            if ident is None:
                ident = crypto.Identity.create(
                    ledger,
                    env_writable=hostcompat.env_writable,
                    save_env=hostcompat.save_env,
                    read_env_fresh=hostcompat.read_env_fresh,
                )
            return ident

    def runner(self) -> jobs.JobRunner:
        with self.active():
            return jobs.runner_for(None)

    def run(
        self,
        mode: Mode,
        peer: Peer | None,
        session_ref: SessionRef,
        *,
        options: Options | None = None,
        bundle_path: Path | None = None,
        confirm=None,
    ) -> tuple[str, int]:
        """plan_sync + run_foreground in this home (the CLI path); returns ``(job_id, exit code)``."""
        options = options or Options(refine="off", yes=True)
        with self.active():
            runner = jobs.runner_for(None)
            job_id, final = runner.plan_sync(
                mode,
                peer,
                options,
                session_ref=session_ref,
                calling_session=None,
                session_key=None,
                confirm=confirm,
                bundle_path=bundle_path,
            )
            if final is None:
                return job_id, jobs.exit_code(runner.ledger.job(job_id, Role.CONDUCTOR))
            return job_id, runner.run_foreground(job_id)


class Listener:
    """``listener_app.py`` with home A's environment (see module docstring)."""

    def __init__(
        self, home: Home, *, proxy_limit: int | None = None, faults: bool = False, supervise: bool = False
    ) -> None:
        self.home = home
        self.token = secrets.token_urlsafe(16)
        self.control = home.root / "listener"
        self.control.mkdir(parents=True, exist_ok=True)
        self._cfg: dict = {"gate_token": self.token, "control": str(self.control)}
        if proxy_limit:
            self._cfg["proxy_limit"] = proxy_limit
        if faults:
            self._cfg["faults"] = True
        self._cfg_path = self.control / "config.json"
        self.port = 0
        self.restarts = 0
        self._log = None
        self._lock = threading.RLock()
        self._stopping = threading.Event()
        self._hold = threading.Event()
        self._spawn()
        self.url = f"http://127.0.0.1:{self.port}"
        self._watch: threading.Thread | None = None
        if supervise:
            self._watch = threading.Thread(target=self._supervise, name="listener-supervisor", daemon=True)
            self._watch.start()

    def _spawn(self) -> None:
        if self.port:
            self._cfg["port"] = self.port  # a restart binds the port the peer row points at
        self._cfg_path.write_text(json.dumps(self._cfg), encoding="utf-8")
        if self._log is not None:
            self._log.close()
        self._log = (self.control / "listener.log").open("ab")
        self.proc = subprocess.Popen(  # noqa: S603 - fixed argv: this interpreter and a module of the test tree
            [sys.executable, "-m", "ctx_handover.tests.integration.listener_app", str(self._cfg_path)],
            stdout=subprocess.PIPE,
            stderr=self._log,
            env=child_env(self.home),
            cwd=str(self.home.root),
        )
        line = self.proc.stdout.readline().decode().strip()
        if not line.startswith("PORT "):
            if self.proc.poll() is None:
                self.proc.kill()
                self.proc.wait(10)
            raise RuntimeError(f"listener did not start: {line!r}; see {self.control / 'listener.log'}")
        self.port = int(line.split()[1])

    def _supervise(self) -> None:
        while not self._stopping.is_set():
            proc = self.proc
            proc.wait()
            while self._hold.is_set() and not self._stopping.is_set():
                self._stopping.wait(0.05)
            with self._lock:
                if self._stopping.is_set() or self.proc is not proc:
                    continue
                for _ in range(10):
                    try:
                        self._spawn()
                        self.restarts += 1
                        break
                    except RuntimeError:
                        time.sleep(0.5)

    def stop(self) -> None:
        self._stopping.set()
        with self._lock:
            if self.proc.poll() is None:
                self.proc.terminate()
                try:
                    self.proc.wait(10)
                except subprocess.TimeoutExpired:
                    self.proc.kill()
                    self.proc.wait(10)
            if self._log is not None:
                self._log.close()
                self._log = None
        if self._watch is not None:
            self._watch.join(10)

    def kill(self) -> None:
        """Hard stop (process death without cleanup); a supervised listener stays down until ``revive()``."""
        self._hold.set()
        with self._lock:
            if self.proc.poll() is None:
                self.proc.kill()
                self.proc.wait(10)

    def revive(self) -> None:
        """Start the listener again on the same port after ``kill()``."""
        with self._lock:
            if self.proc.poll() is not None:
                self._spawn()
                self.restarts += 1
            self._hold.clear()

    def restart(self) -> None:
        """Kill the process (SIGKILL) and start it again on the same port."""
        self.kill()
        self.revive()

    def wait_ready(self, timeout: float = 30.0) -> bool:
        """True once the process is alive and its port accepts connections."""

        def up() -> bool:
            if self.proc.poll() is not None:
                return False
            try:
                with socket.create_connection(("127.0.0.1", self.port), timeout=0.5):
                    return True
            except OSError:
                return False

        return wait_for(up, timeout)

    def arm(self, rules: list[dict]) -> None:
        """Listener-side fault rules (format: ``rules_for``); read by the listener on every request."""
        tmp = self.control / "faults.json.tmp"
        tmp.write_text(json.dumps(list(rules)), encoding="utf-8")
        os.replace(tmp, self.control / "faults.json")

    def disarm(self) -> None:
        self.arm([])

    def fired(self) -> list[dict]:
        return read_fired(self.control / "fired.jsonl")

    def requests(self) -> list[str]:
        path = self.control / "requests.log"
        return path.read_text(encoding="utf-8").splitlines() if path.exists() else []


def pair(server: Home, laptop: Home, listener: Listener, *, rights=(Right.PUSH, Right.PULL)) -> Peer:
    """Pair ``laptop`` (dialer) with ``server`` (listener) the way ``/handover pair`` does on both sides."""
    ident_a = server.identity()
    with server.active():
        code = pairing.create_code(jobs._profile().ledger, rights=frozenset(rights), name_hint="laptop", ttl_s=600)
        text = pairing.pair_string(
            code, url=listener.url, fingerprint=ident_a.fingerprint(), name="server", profile=None
        )
    ident_b = laptop.identity()
    laptop.extra_env[GATE_ENV] = listener.token
    with laptop.active():
        ledger = jobs._profile().ledger
        return pairing.redeem_dialer(
            lambda peer: wire.WireClient(peer, ident_b, read_secret=hostcompat.read_secret),
            ident_b,
            pairing.parse_pair_string(text),
            name="server",
            gate=GateAuth(mode=GateMode.SESSION_TOKEN, token_env=GATE_ENV),
            allow_insecure=True,
            ledger=ledger,
        )


def wait_for(predicate, timeout: float = 30.0, interval: float = 0.05) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return bool(predicate())


def lineage_hashes(db, tip: str) -> tuple[str, str, str]:
    """``(H_all, H_rows, H_model)`` of a lineage with cluster B's own hash functions (DESIGN §6.9)."""
    from ctx_handover.engine import session_export

    lineage = tuple(db.get_compression_lineage(tip)) or (tip,)
    return session_export._hashes_now(db, tip, lineage)


def session_marks(db, sid: str) -> dict:
    """The four primary-lock marks of a source session (DESIGN §1.4) plus its title."""
    row = db.get_session(sid) or {}
    return {
        "ended": row.get("ended_at") is not None,
        "end_reason": row.get("end_reason"),
        "archived": bool(row.get("archived")),
        "hidden": bool(row.get("hidden")),
        "title": row.get("title"),
    }


def arrived_session(home: Home, job_id: str):
    """The target session id of a committed job as the target's ledger recorded it."""
    for row in home.ledger().arrival_rows():
        if row["job_id"] == job_id:
            return row["root_id"], row["session_id"]
    return None, None


# ---------------------------------------------------------------------------------------------------------
# fixtures through the real paths
# ---------------------------------------------------------------------------------------------------------


def dirty_repo(root: Path):
    """A git repository with a commit, a staged change, a further work-tree change and an untracked file."""
    from ctx_handover.tests.fakes.repo_builder import RepoBuilder

    r = RepoBuilder.init(root)
    r.write("README.md", "# app\n")
    r.write("src/main.py", "print('v1')\n")
    r.add_all()
    r.commit("first")
    r.write("src/main.py", "print('v2 staged')\n")
    r.add("src/main.py")
    r.write("src/main.py", "print('v3 in the work tree')\n")
    r.write("notes/todo.txt", "untracked work\n")
    return r


def make_session(home: Home, cwd: Path, title: str) -> str:
    """A two-row session in ``home`` written through the core's own paths (``SessionBuilder``)."""
    from ctx_handover.tests.fakes import FakeClock
    from ctx_handover.tests.fakes.sessiondb import SessionBuilder

    b = SessionBuilder(home.db(), clock=FakeClock())
    sid = b.create(title=title, cwd=str(cwd))
    b.user(sid, "please change src/main.py")
    b.assistant(sid, "done, see src/main.py")
    return sid


# ---------------------------------------------------------------------------------------------------------
# the fault matrix: calls, kinds, seams
# ---------------------------------------------------------------------------------------------------------

SOURCE_CALLS = (
    "hello",
    "facts",
    "start_plan",
    "lock",
    "start_export",
    "start_pack",
    "start_prepare",
    "start_retire",
    "start_release",
    "op_status",
    "complete:plan",
    "complete:export",
    "complete:pack",
    "complete:prepare",
    "complete:retire",
)
TARGET_CALLS = (
    "hello",
    "open_job",
    "facts",
    "negotiate",
    "start_stage_context",
    "start_prepare",
    "start_commit",
    "op_status",
    "status",
    "complete:stage_context",
    "complete:prepare",
    "complete:commit",
)
BLOCK_CALLS = (
    ("source_blocks", "manifests"),
    ("source_blocks", "read_blocks"),
    ("target_blocks", "missing"),
    ("target_blocks", "write_blocks"),
    ("target_blocks", "put_manifests"),
)
CASES = (*[("source", m) for m in SOURCE_CALLS], *[("target", m) for m in TARGET_CALLS], *BLOCK_CALLS)
"""The call points of the fake matrix (``tests/unit/A/test_fault_matrix.py``), identical names."""

FAULT_KINDS = ("disconnect", "disconnect_after", "error", "crash", "crash_after", "interrupt", "down", "cancel")
LINK_KINDS = frozenset({"disconnect", "disconnect_after", "down", "interrupt"})
"""Faults of a connection or of the participant's own process: only a remote participant has them. A local
participant runs inside the conductor's process — its restart is the conductor crash (``crash``/``crash_after``)."""

STALE_S = 60.0
"""``ledger.STALE_OWNER_S``: the janitor adopts a job only after its owner missed heartbeats this long."""
JANITOR_PAUSE_S = 5.0
"""Pause before the driver repeats a janitor run that changed nothing."""

FAST_POLL = {
    "op_first_s": 0.05,
    "op_max_s": 0.5,
    "in_doubt_first_s": 0.2,
    "in_doubt_max_s": 2.0,
    "retry_first_s": 0.1,
    "retry_max_s": 2.0,
}
"""``PollPolicy`` intervals of the conductor processes (only waits shrink; attempts, heartbeats, staleness stay)."""
WIRE_SLEEP_SCALE = 0.2
"""The ``WireClient``'s backoff sleeps are scaled by this (its six attempts and their order stay)."""

_START_OPS = {
    "start_plan": "plan",
    "start_export": "export",
    "start_pack": "pack",
    "start_prepare": "prepare",
    "start_retire": "retire",
    "start_release": "release",
    "start_stage_context": "stage_context",
    "start_commit": "commit",
    "start_apply_delta": "apply_delta",
    "start_reset_session": "reset_session",
}
_BLOCK_ROUTES = {
    "manifests": ("objects/list", "objects_list"),
    "read_blocks": ("blocks/get", "blocks_get"),
    "missing": ("blocks/need", "blocks_need"),
    "write_blocks": ("blocks", "put_blocks"),
    "put_manifests": ("objects/put", "objects_put"),
}
_API_METHODS = (
    "hello",
    "facts",
    "start_plan",
    "lock",
    "heartbeat",
    "start_export",
    "start_pack",
    "start_prepare",
    "start_retire",
    "start_release",
    "op_status",
    "status",
    "open_job",
    "negotiate",
    "start_stage_context",
    "start_apply_delta",
    "start_reset_session",
    "start_commit",
    "continue_session",
    "abort",
)
_BLOCK_METHODS = ("manifests", "read_blocks", "missing", "write_blocks", "put_manifests")
TERMINAL_OPS = frozenset({"done", "failed"})


def remote_role(mode: Mode) -> str:
    """The participant behind HTTP: the target of a push, the source of a pull."""
    return "target" if Mode(mode) is Mode.PUSH else "source"


def is_remote(mode: Mode, side: str) -> bool:
    return side.split("_", 1)[0] == remote_role(mode)


def applicable(mode: Mode, side: str, kind: str) -> bool:
    return kind not in LINK_KINDS or is_remote(mode, side)


def route_for(side: str, method: str) -> tuple[str, str]:
    """``(route, server handler)`` of the request that carries ``side.method`` when the participant is remote
    (route relative to ``/v1/`` with ``jobs/<id>/`` removed)."""
    if side.endswith("_blocks"):
        return _BLOCK_ROUTES[method]
    if method == "hello":
        return "hello", "hello"
    if method == "open_job":
        return "jobs", "jobs_open"
    if method in ("op_status", "status") or method.startswith("complete:"):
        return "status", "job_status"
    name = _START_OPS.get(method) or method
    role = side.split("_", 1)[0]
    return f"{role}/{name}", f"{role}_{name}"


def rules_for(mode: Mode, side: str, method: str, kind: str) -> tuple[list[dict], list[dict]]:
    """``(conductor rules, listener rules)`` for one matrix case. SEAMS:

    ==================  ==========================================================================================
    kind                seam (``complete:X`` = the completion of operation ``X``)
    ==================  ==========================================================================================
    crash / crash_after the conductor process SIGKILLs itself before / after the call (``complete:X``: before the
                        first poll of ``X`` / after the poll that reports ``X`` finished)
    cancel              the cancel verb (``JobRunner.cancel``) in a separate process, before the call
                        (``complete:X``: when the poll reports ``X`` finished)
    disconnect          remote only: the request never leaves (``httpx.ConnectError`` in the conductor's transport)
    disconnect_after    remote only: the request took effect, its response is lost (``httpx.ReadError``)
    down                remote only: from that request on every request of the conductor process fails
    interrupt           remote only: the listener process SIGKILLs itself after the request took effect (response
                        lost), or inside running operation ``X``; the supervisor restarts it on the same port
    error               remote: the participant answers with an ``import_failed`` error envelope (or operation ``X``
                        fails with it); local: the call (or operation ``X``) raises ``ImportFailed``
    ==================  ==========================================================================================
    """
    if not applicable(mode, side, kind):
        raise ValueError(f"{kind} needs a remote participant; {side} is local in {mode}")
    label = f"{side}.{method}"
    op = method.split(":", 1)[1] if method.startswith("complete:") else None
    route, handler = route_for(side, method)
    rid = f"{kind}@{label}"
    if kind in ("crash", "crash_after", "cancel"):
        return [{"id": rid, "kind": kind, "label": label}], []
    if kind in ("disconnect", "disconnect_after", "down"):
        return [{"id": rid, "kind": kind, "label": label, "route": route}], []
    if kind == "interrupt":
        rule = {"id": rid, "kind": kind, "op": op} if op else {"id": rid, "kind": kind, "label": label}
        rule.setdefault("handler", None if op else handler)
        return [], [rule]
    if kind != "error":
        raise ValueError(f"unknown fault kind {kind}")
    if is_remote(mode, side):
        rule = {"id": rid, "kind": "error", "op": op} if op else {"id": rid, "kind": "error", "label": label}
        rule.setdefault("handler", None if op else handler)
        return [], [rule]
    if op:
        return [{"id": rid, "kind": "op_error", "op": op}], []
    return [{"id": rid, "kind": "error", "label": label}], []


def read_fired(path: Path) -> list[dict]:
    out = []
    try:
        text = path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return out
    for line in text.splitlines():
        with contextlib.suppress(ValueError):
            if line.strip():
                out.append(json.loads(line))
    return out


class _Fired:
    """Fired-rule bookkeeping (``fired.jsonl``; same format as ``listener_app.py``)."""

    def __init__(self, path: Path, where: str) -> None:
        self.path = path
        self.where = where
        self._lock = threading.Lock()

    def take(self, rule: dict) -> bool:
        with self._lock:
            n = sum(1 for r in read_fired(self.path) if r.get("id") == rule["id"])
            if n >= int(rule.get("times", 1)):
                return False
            with self.path.open("a", encoding="utf-8") as fh:
                fh.write(json.dumps({"id": rule["id"], "where": self.where, "pid": os.getpid(), "at": time.time()}))
                fh.write("\n")
            return True


# ---------------------------------------------------------------------------------------------------------
# conductor-side faults (inside the conductor process)
# ---------------------------------------------------------------------------------------------------------

CALL: contextvars.ContextVar[tuple[str, ...] | None] = contextvars.ContextVar("ctxh_test_call", default=None)
"""Labels of the participant call in progress in this thread (``("target.op_status", "target.complete:commit")``)."""

_JOB_ROUTE = re.compile(r"^jobs/[^/]+/(.+)$")


def _route_of(path: str) -> str:
    rest = path.split("/v1/", 1)[1] if "/v1/" in path else path
    m = _JOB_ROUTE.match(rest)
    return m.group(1) if m else rest


def _die() -> None:
    sys.stdout.flush()
    os.kill(os.getpid(), signal.SIGKILL)
    time.sleep(60)  # never reached


def _op_name_of(label: str) -> str | None:
    return label.split(".complete:", 1)[1] if ".complete:" in label else None


class ChildFaults:
    """The conductor-side rules of one conductor process (see ``rules_for``)."""

    def __init__(self, rules, control: Path) -> None:
        self.rules = [dict(r) for r in rules]
        self.fired = _Fired(control / "fired.jsonl", "conductor")
        self.ops: dict[str, str] = {}
        self.down = False
        self.job_id: str | None = None

    def _matching(self, labels: tuple[str, ...], kinds: tuple[str, ...]) -> list[dict]:
        return [r for r in self.rules if r["kind"] in kinds and r.get("label") in labels]

    def link_rules(self, labels: tuple[str, ...], route: str, kinds: tuple[str, ...]) -> list[dict]:
        return [r for r in self._matching(labels, kinds) if r.get("route") == route]

    def before(self, labels: tuple[str, ...]) -> None:
        for rule in self._matching(labels, ("crash", "error", "cancel")):
            if rule["kind"] == "cancel" and _op_name_of(rule["label"]):
                continue  # a completion cancel fires when the poll reports the operation finished
            if self.fired.take(rule):
                self._act(rule)

    def after(self, labels: tuple[str, ...], result: object) -> None:
        for rule in self._matching(labels, ("crash_after", "cancel")):
            complete = _op_name_of(rule["label"]) is not None
            if rule["kind"] == "cancel" and not complete:
                continue
            if complete and str(getattr(result, "state", "")) not in TERMINAL_OPS:
                continue
            if self.fired.take(rule):
                self._act(rule)

    def _act(self, rule: dict) -> None:
        from ctx_handover.engine.errors import ImportFailed

        print(json.dumps({"event": "fault", "rule": rule["id"]}), flush=True)
        if rule["kind"] in ("crash", "crash_after"):
            _die()
        if rule["kind"] == "error":
            raise ImportFailed("injected failure", hermes_error="Injected")
        if rule["kind"] == "cancel" and self.job_id:
            out = _cancel_in_process_of_env(dict(os.environ), self.job_id)
            print(json.dumps({"event": "cancel", "answer": out}), flush=True)

    # -- participants ------------------------------------------------------------------------------------------
    def _labels(self, side: str, name: str, args: tuple, kw: dict) -> tuple[str, ...]:
        labels = (f"{side}.{name}",)
        if name == "op_status":
            op_id = args[1] if len(args) > 1 else kw.get("op_id")
            labels += (f"{side}.complete:{self.ops.get(str(op_id), '?')}",)
        return labels

    def _guarded(self, labels: tuple[str, ...], fn, name: str | None = None):
        if CALL.get() is not None:
            return fn()  # a participant calling its own wrapped method: one call, one label set
        token = CALL.set(labels)
        try:
            self.before(labels)
            result = fn()
            op = _START_OPS.get(name or "")
            if op and getattr(result, "op_id", None):
                self.ops[str(result.op_id)] = op
            self.after(labels, result)
            return result
        finally:
            CALL.reset(token)

    def instrument(self, api, side: str):
        """Wrap the participant methods of ``api`` on the instance (``isinstance`` checks still hold)."""
        for name in _API_METHODS:
            inner = getattr(api, name, None)
            if callable(inner):
                setattr(api, name, self._wrap(side, name, inner))
        for name in ("blocks", "blocks_for"):
            inner = getattr(api, name, None)
            if callable(inner):
                setattr(api, name, self._wrap_blocks(side, inner))
        return api

    def _wrap(self, side: str, name: str, inner):
        @functools.wraps(inner)
        def call(*args, **kw):
            return self._guarded(self._labels(side, name, args, kw), lambda: inner(*args, **kw), name)

        return call

    def _wrap_blocks(self, side: str, inner):
        @functools.wraps(inner)
        def get(*args, **kw):
            return _FaultBlocks(inner(*args, **kw), f"{side}_blocks", self)

        return get

    def install_ops(self, op_runner_cls) -> None:
        """Operation ``X`` of a local participant fails (``op_error`` rules)."""
        rules = [r for r in self.rules if r["kind"] == "op_error"]
        if not rules:
            return
        original = op_runner_cls.start
        faults = self

        def start(runner, job_id, op, digest, fn, **kw):
            for rule in rules:
                if rule["op"] == op:
                    fn = _failing_op(fn, rule, faults)
            return original(runner, job_id, op, digest, fn, **kw)

        op_runner_cls.start = start


def _failing_op(fn, rule: dict, faults: ChildFaults):
    def run(report):
        from ctx_handover.engine.errors import ImportFailed

        if faults.fired.take(rule):
            print(json.dumps({"event": "fault", "rule": rule["id"]}), flush=True)
            raise ImportFailed("injected failure", hermes_error="Injected")
        return fn(report)

    return run


class _FaultBlocks:
    """A ``BlockSource``/``BlockSink`` whose calls carry labels ``<side>_blocks.<method>``."""

    def __init__(self, inner, side: str, faults: ChildFaults) -> None:
        self._inner = inner
        self._side = side
        self._faults = faults

    def __getattr__(self, name: str):
        return getattr(self._inner, name)

    def _call(self, name: str, fn):
        return self._faults._guarded((f"{self._side}.{name}",), fn)

    def manifests(self, object_ids):
        return self._call("manifests", lambda: self._inner.manifests(object_ids))

    def read_blocks(self, ids):
        return iter(self._call("read_blocks", lambda: list(self._inner.read_blocks(ids))))

    def missing(self, ids):
        return self._call("missing", lambda: self._inner.missing(ids))

    def write_blocks(self, frames):
        return self._call("write_blocks", lambda: self._inner.write_blocks(frames))

    def put_manifests(self, manifests):
        return self._call("put_manifests", lambda: self._inner.put_manifests(manifests))


def _reports_terminal(content: bytes, op: str, faults: ChildFaults) -> bool:
    try:
        data = json.loads(content)
    except ValueError:
        return False
    for key, value in ((data or {}).get("ops") or {}).items():
        if isinstance(value, dict) and (key == op or faults.ops.get(str(value.get("op_id"))) == op):
            return str(value.get("state")) in TERMINAL_OPS
    return False


class FaultTransport(httpx.BaseTransport):
    """The real HTTP transport under the conductor's ``WireClient`` with the link faults of ``rules_for``; every
    request carries ``X-Test-Call`` (the call labels) for the listener-side rules."""

    def __init__(self, faults: ChildFaults) -> None:
        self._inner = httpx.HTTPTransport()
        self._faults = faults

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        labels = CALL.get() or ()
        if labels:
            request.headers["X-Test-Call"] = ",".join(labels)
        route = _route_of(request.url.path)
        f = self._faults
        if f.down:
            raise httpx.ConnectError("injected: the link is down", request=request)
        for rule in f.link_rules(labels, route, ("disconnect", "down")):
            if f.fired.take(rule):
                print(json.dumps({"event": "fault", "rule": rule["id"]}), flush=True)
                if rule["kind"] == "down":
                    f.down = True
                raise httpx.ConnectError(f"injected {rule['kind']} at {rule['label']}", request=request)
        response = self._inner.handle_request(request)
        response.read()
        for rule in f.link_rules(labels, route, ("disconnect_after",)):
            op = _op_name_of(rule["label"])
            if op and not _reports_terminal(response.content, op, f):
                continue
            if f.fired.take(rule):
                print(json.dumps({"event": "fault", "rule": rule["id"]}), flush=True)
                response.close()
                raise httpx.ReadError(f"injected lost response at {rule['label']}", request=request)
        return response

    def close(self) -> None:
        self._inner.close()


def _wire_sleep(seconds: float) -> None:
    time.sleep(float(seconds) * WIRE_SLEEP_SCALE)


def _cancel_in_process_of_env(env: dict[str, str], job_id: str) -> str:
    proc = subprocess.run(  # noqa: S603 - fixed argv: this interpreter and a module of the test tree
        [sys.executable, "-m", "ctx_handover.tests.integration.harness", "cancel", job_id],
        env=env,
        capture_output=True,
        timeout=120,
        check=False,
    )
    return (proc.stdout or b"").decode(errors="replace").strip() or f"rc={proc.returncode}"


def cancel_job(home: Home, job_id: str) -> str:
    """The cancel verb of ``home`` for ``job_id`` in a separate process; returns its answer."""
    return _cancel_in_process_of_env(child_env(home), job_id)


def _conduct(spec: dict) -> int:
    """Child process: compose the job's participants like the product, wrap them with the rules, run ``action``."""
    from ctx_handover.engine import bundle, ops, remote
    from ctx_handover.engine.conductor import PollPolicy
    from ctx_handover.engine.errors import HandoverError
    from ctx_handover.engine.jsonutil import from_wire

    control = Path(spec["control"])
    faults = ChildFaults(spec.get("rules") or (), control)
    faults.install_ops(ops.OpRunner)
    client = functools.partial(wire.WireClient, transport=FaultTransport(faults), sleep=_wire_sleep)
    transport = SimpleNamespace(bundle=bundle, remote=remote, wire=SimpleNamespace(WireClient=client))
    jobs._default_transport = lambda: transport
    prof = jobs._profile()
    holder: dict = {}

    def apis(row: dict):
        source, target = holder["runner"]._default_apis(row)
        faults.job_id = row["job_id"]
        return faults.instrument(source, "source"), faults.instrument(target, "target")

    runner = jobs.JobRunner(prof.ledger, apis_factory=apis, poll=PollPolicy(**FAST_POLL), before_job=prof.janitor)
    holder["runner"] = runner
    prof.runner = runner  # the janitor of this process adopts with this runner
    action = spec["action"]
    print(json.dumps({"event": "action", "action": action, "pid": os.getpid()}), flush=True)
    if action == "start":
        peer = prof.ledger.peer(spec["peer_id"]) if spec.get("peer_id") else None
        options = from_wire(Options, spec["options"])
        try:
            job_id, final = runner.plan_sync(
                Mode(spec["mode"]),
                peer,
                options,
                session_ref=SessionRef(id=spec["session_id"]),
                calling_session=None,
                session_key=None,
            )
        except HandoverError as exc:
            print(json.dumps({"event": "plan_sync", "error": exc.code, "message": exc.message}), flush=True)
            return 1
        print(json.dumps({"event": "planned", "job_id": job_id}), flush=True)
        if final is None:
            return jobs.exit_code(prof.ledger.job(job_id, Role.CONDUCTOR))
        return runner.run_foreground(job_id)
    if action == "resume":
        runner.resume(spec["job_id"])
        runner.join(timeout=900)
        return jobs.exit_code(prof.ledger.job(spec["job_id"], Role.CONDUCTOR))
    if action == "janitor":
        done = prof.janitor(background=False)
        print(json.dumps({"event": "janitor", "done": done}, default=str), flush=True)
        return 0
    raise SystemExit(f"unknown action {action}")


def _child_main(argv: list[str]) -> int:
    if argv[:1] == ["conduct"]:
        return _conduct(json.loads(Path(argv[1]).read_text(encoding="utf-8")))
    if argv[:1] == ["cancel"]:
        print(jobs.runner_for(None).cancel(argv[1]), flush=True)
        return 0
    raise SystemExit("usage: harness conduct <spec.json> | cancel <job_id>")


@dataclass
class ChildRun:
    """One conductor process: ``returncode`` (``None`` after a timeout kill), its log."""

    action: str
    returncode: int | None
    seconds: float
    log: Path
    started_at: float = 0.0

    @property
    def killed(self) -> bool:
        return self.returncode == -signal.SIGKILL

    def tail(self, n: int = 12) -> str:
        try:
            return "\n".join(self.log.read_text(encoding="utf-8", errors="replace").splitlines()[-n:])
        except OSError:
            return ""


def run_conductor(
    home: Home,
    control: Path,
    *,
    action: str,
    mode: Mode | None = None,
    peer: Peer | None = None,
    session_id: str | None = None,
    options: Options | None = None,
    job_id: str | None = None,
    rules=(),
    timeout: float = 900.0,
) -> ChildRun:
    """Run one conductor process of ``home`` (see module docstring) and wait for it."""
    from ctx_handover.engine.jsonutil import to_wire

    control.mkdir(parents=True, exist_ok=True)
    n = sum(1 for _ in control.glob("spec-*.json")) + 1
    spec = {
        "action": action,
        "control": str(control),
        "mode": str(mode) if mode is not None else None,
        "peer_id": peer.peer_id if peer is not None else None,
        "session_id": session_id,
        "options": to_wire(options) if options is not None else None,
        "job_id": job_id,
        "rules": list(rules),
    }
    spec_path = control / f"spec-{n}.json"
    spec_path.write_text(json.dumps(spec), encoding="utf-8")
    log_path = control / f"conductor-{n}-{action}.log"
    started, wall = time.monotonic(), time.time()
    with log_path.open("ab") as log:
        proc = subprocess.Popen(  # noqa: S603 - fixed argv: this interpreter and a module of the test tree
            [sys.executable, "-m", "ctx_handover.tests.integration.harness", "conduct", str(spec_path)],
            stdout=log,
            stderr=subprocess.STDOUT,
            env=child_env(home),
            cwd=str(home.root),
        )
        try:
            rc: int | None = proc.wait(timeout)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(10)
            rc = None
    return ChildRun(action, rc, time.monotonic() - started, log_path, wall)


# ---------------------------------------------------------------------------------------------------------
# invariant monitor and promise checks
# ---------------------------------------------------------------------------------------------------------


def _ro(path: Path) -> sqlite3.Connection:
    return sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=2.0)


def _source_locked(row) -> bool:
    return bool(row) and row[0] is not None and row[1] == "handed_over" and bool(row[2]) and bool(row[3])


def visible_sessions(state_db: Path) -> set[str]:
    """Ids of the sessions of ``state_db`` that are visible (not hidden)."""
    if not Path(state_db).exists():
        return set()
    conn = _ro(state_db)
    try:
        return {r[0] for r in conn.execute("SELECT id FROM sessions WHERE hidden = 0")}
    finally:
        conn.close()


def all_sessions(state_db: Path) -> set[str]:
    if not Path(state_db).exists():
        return set()
    conn = _ro(state_db)
    try:
        return {r[0] for r in conn.execute("SELECT id FROM sessions")}
    finally:
        conn.close()


class InvariantMonitor:
    """Never two active copies (DESIGN §1.3 I2/I3, §3.4): a thread samples both ``state.db`` files read-only and
    records every sample in which the target shows a session that did not exist before the job and is visible while
    the source session is not under the primary lock (ended ``handed_over``, archived, hidden)."""

    def __init__(self, source_db: Path, source_sid: str, target_db: Path, interval: float = 0.02) -> None:
        self.source_db = Path(source_db)
        self.source_sid = source_sid
        self.target_db = Path(target_db)
        self.interval = interval
        self.baseline: set[str] = set()
        self.violations: list[str] = []
        self.samples = 0
        self.errors = 0
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self) -> InvariantMonitor:
        self.baseline = all_sessions(self.target_db)
        self._thread = threading.Thread(target=self._run, name="invariant-monitor", daemon=True)
        self._thread.start()
        return self

    def _run(self) -> None:
        while not self._stop.is_set():
            self.sample()
            self._stop.wait(self.interval)

    def sample(self) -> None:
        try:
            if not self.target_db.exists():
                return
            conn = _ro(self.target_db)
            try:
                new = sorted(
                    r[0] for r in conn.execute("SELECT id FROM sessions WHERE hidden = 0") if r[0] not in self.baseline
                )
            finally:
                conn.close()
            if not new:
                self.samples += 1
                return
            conn = _ro(self.source_db)
            try:
                row = conn.execute(
                    "SELECT ended_at, end_reason, archived, hidden, title FROM sessions WHERE id = ?",
                    (self.source_sid,),
                ).fetchone()
            finally:
                conn.close()
            self.samples += 1
            if not _source_locked(row):
                marks = dict(zip(("ended_at", "end_reason", "archived", "hidden", "title"), row or (), strict=False))
                self.violations.append(f"two active copies at {time.time():.3f}: target {new}, source {marks}")
        except sqlite3.Error:
            self.errors += 1

    def stop(self) -> list[str]:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(10)
        self.sample()
        return list(self.violations)


def lease_problems(state_db: Path, sid: str) -> list[str]:
    """Probe the source session like a new turn would: take and give back the turn lease and the compression lock
    (a dead holder's lease is reclaimed by the core itself; a live one refuses)."""
    db = hostcompat.session_db(state_db)
    holder = f"pid={os.getpid()}:ctxh-int-probe"
    out = []
    if db.try_acquire_session_turn_lease(sid, holder, ttl_seconds=5, patience_s=0.5):
        db.release_session_turn_lease(sid, holder)
    else:
        out.append("turn lease of the source session is held")
    if db.try_acquire_compression_lock(sid, holder, ttl_seconds=5):
        db.release_compression_lock(sid, holder)
    else:
        out.append("compression lock of the source session is held")
    return out


def effective_veto(home: Home, sid: str) -> dict | None:
    """What the plugin's ``pre_llm_call`` veto answers for ``sid`` (a fresh ``VetoIndex`` over the home's ledger)."""
    from ctx_handover.engine import lock

    ledger = home.ledger()
    index = lock.VetoIndex(ledger.path, ledger_factory=lambda _path: ledger)
    index.refresh_now(force=True)
    return index.lookup(sid)


@dataclass
class CaseResult:
    """Outcome of one ``FaultWorld.run_case``."""

    mode: Mode
    side: str
    method: str
    kind: str
    job_id: str | None = None
    final: str | None = None
    runs: list[ChildRun] = field(default_factory=list)
    fired: list[dict] = field(default_factory=list)
    violations: list[str] = field(default_factory=list)
    checks: list[str] = field(default_factory=list)
    states: list[str] = field(default_factory=list)
    last_error: dict | None = None
    restarts: int = 0
    seconds: float = 0.0
    samples: int = 0

    @property
    def case(self) -> str:
        return f"{self.mode}:{self.side}.{self.method}:{self.kind}"

    @property
    def fault_fired(self) -> bool:
        return bool(self.fired)

    def problems(self) -> list[str]:
        out = []
        if self.final not in ("done", "aborted"):
            out.append(f"job ended {self.final!r}, not done/aborted")
        return [*out, *self.violations, *self.checks]

    def summary(self) -> dict:
        """One JSON line per case (``CTXH_MATRIX_LOG``) for the report."""
        return {
            "case": self.case,
            "final": self.final,
            "fired": [f.get("id") for f in self.fired],
            "problems": self.problems(),
            "runs": [[r.action, r.returncode, round(r.seconds, 1), round(r.started_at, 1)] for r in self.runs],
            "restarts": self.restarts,
            "seconds": round(self.seconds, 1),
            "states": self.states,
            "last_error": (self.last_error or {}).get("code"),
        }

    def describe(self) -> str:
        runs = "; ".join(f"{r.action} rc={r.returncode} {r.seconds:.1f}s" for r in self.runs)
        tails = "\n".join(f"--- {r.log.name}\n{r.tail()}" for r in self.runs[-2:])
        return (
            f"case {self.case} job {self.job_id} final={self.final} fired={[f.get('id') for f in self.fired]}\n"
            f"runs: {runs}; listener restarts {self.restarts}; {self.seconds:.1f}s; samples {self.samples}\n"
            f"states: {' > '.join(self.states)}\nlast_error: {self.last_error}\n"
            f"problems: {self.problems()}\n{tails}"
        )


class FaultWorld:
    """Home A served by a supervised, fault-armed listener; home B paired with it (fixture ``fault_world``)."""

    def __init__(self, server: Home, laptop: Home, listener: Listener, peer: Peer, base: Path) -> None:
        self.server = server
        self.laptop = laptop
        self.listener = listener
        self.peer = peer
        self.base = Path(base)
        self.cases = 0

    def options(self, **kw) -> Options:
        return Options(refine="off", yes=True, stall_timeout_s=20, **kw)

    def _job_id(self) -> str | None:
        rows = self.laptop.ledger().jobs(role=Role.CONDUCTOR, limit=1)
        return rows[0]["job_id"] if rows else None

    @staticmethod
    def _adoptable_at(row: dict) -> float:
        """When the product's janitor takes this job over (DESIGN §3.6, ``janitor``/``stale_jobs``)."""
        if row.get("owner") is not None:
            return float(row.get("owner_heartbeat_at") or 0.0) + STALE_S + 1.0
        if row["state"] in ("committed", "retiring", "in_doubt", "aborting"):
            return 0.0
        return float(row.get("updated_at") or 0.0) + STALE_S + 1.0

    def run_case(
        self, mode: Mode, side: str, method: str, kind: str, *, options: Options | None = None, timeout: float = 720.0
    ) -> CaseResult:
        mode = Mode(mode)
        self.cases += 1
        started = time.monotonic()
        res = CaseResult(mode=mode, side=side, method=method, kind=kind)
        src, dst = (self.laptop, self.server) if mode is Mode.PUSH else (self.server, self.laptop)
        repo = dirty_repo(src.home / "proj")
        from ctx_handover.tests.fakes.repo_builder import tree_digest

        digest = tree_digest(repo.root)
        sid = make_session(src, repo.root, f"Matrix {self.cases}")
        title = session_marks(src.db(), sid)["title"]
        control = self.base / f"case-{self.cases}"
        crules, lrules = rules_for(mode, side, method, kind)
        self.listener.arm(lrules)
        monitor = InvariantMonitor(src.state_db, sid, dst.state_db).start()
        try:
            res.runs.append(
                run_conductor(
                    self.laptop,
                    control,
                    action="start",
                    mode=mode,
                    peer=self.peer,
                    session_id=sid,
                    options=options or self.options(),
                    rules=crules,
                )
            )
            res.job_id = self._job_id()
            self._drive(res, control, started + timeout)
        finally:
            res.violations = monitor.stop()
            res.samples = monitor.samples
            self.listener.disarm()
        row = self.laptop.ledger().job(res.job_id, Role.CONDUCTOR) if res.job_id else None
        res.final = row["state"] if row else None
        res.last_error = row.get("last_error") if row else None
        if res.job_id:
            evs = self.laptop.ledger().events(res.job_id, Role.CONDUCTOR)
            res.states = [e["state"] for e in evs if e["kind"] in ("created", "transition")]
        res.fired = read_fired(control / "fired.jsonl") + self.listener.fired()
        self.listener.wait_ready()
        res.restarts = self.listener.restarts
        res.checks = check_promises(
            src, dst, sid=sid, title=title, job_id=res.job_id, final=res.final, digest=digest,
            dest=dst.home / "hermes-handover" / "proj", baseline=monitor.baseline,
        )  # fmt: skip
        res.seconds = time.monotonic() - started
        log = os.environ.get("CTXH_MATRIX_LOG")
        if log:
            with open(log, "a", encoding="utf-8") as fh:
                fh.write(json.dumps(res.summary()) + "\n")
        return res

    def _drive(self, res: CaseResult, control: Path, deadline: float) -> None:
        """Keep the job going like the product until DONE or ABORTED: the link comes back (faults disarmed, the
        listener up again), a SUSPENDED job is resumed (``/handover resume``), everything else is left to the
        janitor of a later process, which adopts it once its owner is stale (DESIGN §3.6); a janitor run that
        changed nothing is repeated ``JANITOR_PAUSE_S`` later (the product runs it on every plugin use)."""
        last: tuple | None = None
        while res.job_id is not None and time.monotonic() < deadline:
            row = self.laptop.ledger().job(res.job_id, Role.CONDUCTOR)
            if row is None or row["state"] in ("done", "aborted"):
                return
            self.listener.disarm()
            self.listener.wait_ready()
            mark = (row["state"], row.get("owner_epoch"), row.get("updated_at"))
            if mark == last:
                time.sleep(min(JANITOR_PAUSE_S, max(0.0, deadline - time.monotonic())))
            last = mark
            if row["state"] == "suspended":
                res.runs.append(run_conductor(self.laptop, control, action="resume", job_id=res.job_id))
                continue
            wait = self._adoptable_at(row) - time.time()
            if wait > 0:
                time.sleep(min(wait, max(0.0, deadline - time.monotonic())))
            res.runs.append(run_conductor(self.laptop, control, action="janitor"))


def assert_case(res: CaseResult) -> None:
    """The matrix assertions (fake: ``test_fault_matrix``): DONE or ABORTED, no broken promise, and one lost packet
    or one participant restart stays invisible (DONE) — except a fault inside the commit operation."""
    assert res.final in ("done", "aborted"), res.describe()
    assert res.problems() == [], res.describe()
    if res.kind in ("disconnect", "disconnect_after", "interrupt") and res.method != "complete:commit":
        assert res.final == "done", res.describe()


def check_promises(
    src: Home,
    dst: Home,
    *,
    sid: str,
    title: str | None,
    job_id: str | None,
    final: str | None,
    digest: str,
    dest: Path,
    baseline: set[str],
) -> list[str]:
    """The promises after the job ended (fake: ``World.assert_outcome``). DONE ⇒ complete at the target (model
    history hashes equal, workspace bytes equal, visible) and the source under the primary lock with veto, leases
    given back, source job ``retired``. ABORTED ⇒ source usable (no lock mark, title unchanged, leases free, no
    veto, source job released or never locked) and no active copy at the target (no visible new session, no arrival
    row, no workspace at the destination)."""
    from ctx_handover.tests.fakes.repo_builder import tree_digest

    problems: list[str] = []
    if job_id is None or final not in ("done", "aborted"):
        return problems
    src_db, dst_db = src.db(), dst.db()
    marks = session_marks(src_db, sid)
    src_job = src.ledger().source_state(job_id)
    src_state = src_job.get("state") if src_job else None
    leases = lease_problems(src.state_db, sid)
    veto = effective_veto(src, sid)
    arrivals = [r for r in dst.ledger().arrival_rows() if r["job_id"] == job_id]
    new_visible = sorted(visible_sessions(dst.state_db) - set(baseline))
    if final == "done":
        if not arrivals:
            problems.append("done: no arrival row on the target")
        else:
            tip = arrivals[0]["session_id"]
            if lineage_hashes(src_db, sid) != lineage_hashes(dst_db, tip):
                problems.append("done: model history hashes differ between source and target")
            if session_marks(dst_db, tip)["hidden"]:
                problems.append("done: target session still hidden")
        if not dest.exists() or tree_digest(dest) != digest:
            problems.append(f"done: workspace at {dest} is not byte-equal to the source")
        if not (marks["ended"] and marks["end_reason"] == "handed_over" and marks["archived"] and marks["hidden"]):
            problems.append(f"done: source not under the primary lock: {marks}")
        problems.extend(f"done: {p}" for p in leases)
        if veto is None:
            problems.append("done: no veto on the source session")
        if src_state != "retired":
            problems.append(f"done: source job is {src_state!r}, not retired")
    else:
        if marks["ended"] or marks["archived"] or marks["hidden"] or marks["title"] != title:
            problems.append(f"aborted: source not usable: {marks} (title before {title!r})")
        problems.extend(f"aborted: {p}" for p in leases)
        if veto is not None:
            problems.append(f"aborted: veto still active on the source: {veto.get('state')}")
        if src_state not in (None, "released", "reclaimed"):
            problems.append(f"aborted: source job is {src_state!r}")
        if new_visible:
            problems.append(f"aborted: visible new session(s) on the target: {new_visible}")
        if arrivals:
            problems.append("aborted: arrival row on the target")
        if dest.exists():
            problems.append(f"aborted: workspace materialised at {dest}")
    return problems


if __name__ == "__main__":
    rc = _child_main(sys.argv[1:])
    sys.stdout.flush()
    os._exit(int(rc or 0))
