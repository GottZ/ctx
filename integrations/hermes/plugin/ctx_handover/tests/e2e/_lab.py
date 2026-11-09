"""Helpers of the layer-3 lab suite (BUILD-ORDER §5, TESTPLAN §4/§6): drive the two-gateway lab.

Everything here talks to the lab of ``lab/README.md`` (``CTXH_LAB``, default ``/compose/n8n/.gotmp/handover/lab``):

* components start and stop **only** through ``lab/bin/labctl.py`` (as a subprocess; ``spawn`` of the same module
  is used once for a serve with the Basic login provider, exactly as ``spike/r7/basic_login.py`` does);
* ``hermes …`` CLI calls run with ``labctl.env_for(<home>)`` (the lab environment: own ``HOME``, ``HERMES_HOME``,
  ``LAB_TAG``) minus every ``PYTEST_*`` variable, so a child never believes it runs under pytest;
* the gateways are driven with ``lab/bin/gw.py`` (control-socket ticket → ``/api/ws``);
* ``state.db`` is read through the public ``hermes_state.SessionDB`` (read-only unless a fixture builds a session);
* the plugin ledger is read with a read-only SQLite connection (observation only, never written);
* the fake model's rules (``lab/llm/rules.json``) are swapped per test and restored.

Nothing in this module runs at import time; the lab modules are imported lazily, so collection without a lab works.
Process hygiene: helpers that start a process return it, and every caller ends it by PID in ``finally``.
"""

from __future__ import annotations

import contextlib
import hashlib
import importlib
import json
import os
import re
import shutil
import signal
import socket
import sqlite3
import subprocess
import sys
import threading
import time
import uuid
from collections.abc import Callable, Iterable, Iterator, Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

LAB = Path(os.environ.get("CTXH_LAB", "/compose/n8n/.gotmp/handover/lab")).resolve()
BIN = LAB / "bin"
LLM_DIR = LAB / "llm"
PLUGIN_SRC = Path(__file__).resolve().parents[2]
"""The plugin tree of this worktree (``…/ctx_handover``) — the copy source of the lab install."""

TOKENS = {"a": "lab-tok-a", "b": "lab-tok-b"}
"""Fixed ``HERMES_DASHBOARD_SESSION_TOKEN`` per home, used by its gateway and its ``hermes serve``."""
SERVE_URL = {"a": "http://127.0.0.1:47111", "b": "http://127.0.0.1:47112"}
OTHER = {"a": "b", "b": "a"}

# Peer names of the paired stand (peer names are unique per ledger, so each direction has its own name):
SERVER = "server"  # on B: the dial peer for A (B dials A's serve on 47111)
LAPTOP = "laptop"  # on A: the inbound peer that is B
LAPTOP_GW = "laptopgw"  # on A: the dial peer for B (A dials B's serve on 47112)
SERVER_IN = "serverin"  # on B: the inbound peer that is A

COMPONENTS = ("llm", "gw-a", "gw-b", "serve-a", "serve-b")
PORTS = {"llm": 47100, "serve-a": 47111, "serve-b": 47112}
NOTE_MARK = "<ctx-handover-note"
DATA_MARK = "<untrusted-data"
PAIR_RE = re.compile(r"(ctxh-pair:[A-Za-z0-9_=-]+)")
STOP_STATES = frozenset({"done", "aborted"})
SKIP_REASON = (
    "layer-3 lab test (BUILD-ORDER §5): set CTXH_E2E=1 once the lab is handed over (CTXH_LAB, default "
    "/compose/n8n/.gotmp/handover/lab)"
)

OUTSIDE_ROOT = Path(os.environ.get("CTXH_E2E_OUTSIDE") or "/root/.cache/ctx-handover-e2e")
"""Parent of the per-test folders of ``Lab.outside`` (override: ``CTXH_E2E_OUTSIDE``) for sources that must lie
outside every git work tree. Every folder under ``lab/`` is part of the work tree ``/compose/n8n`` (ignored there), so a
file a transcript mentions next to the session's folder tests the enclosing-repository rule (L3-F26, R36b) instead of
what the scenario checks. ``/root/.cache`` lies in no git work tree on the lab host — the project's rule for test
TMPDIRs outside every repository uses the same place — and ``Lab.outside`` checks that again at every use."""

_NO_TEMP = (Path("/tmp"), Path("/var/tmp"), Path("/dev/shm"))  # noqa: S108 - refused as OUTSIDE_ROOT, never written
"""World-writable temp locations ``Lab.outside`` refuses (house rule: no experimental data under ``/tmp``)."""

_KEEP = ("role", "content", "tool_calls", "tool_call_id", "tool_name", "reasoning", "_compressed_summary")


def enabled() -> bool:
    return os.environ.get("CTXH_E2E") == "1"


class LabError(RuntimeError):
    """The lab did not do what a step needs (a scaffold problem, not a product finding)."""


def uid(n: int = 6) -> str:
    return uuid.uuid4().hex[:n]


def _bin_module(name: str) -> Any:
    if str(BIN) not in sys.path:
        sys.path.insert(0, str(BIN))
    return importlib.import_module(name)


def _child_env(env: Mapping[str, str]) -> dict[str, str]:
    """Environment for a lab child: no ``PYTEST_*`` marker (Hermes' state-db guard and test switches key on it)."""
    return {k: v for k, v in env.items() if not k.startswith("PYTEST_")}


def norm_hash(conversation: Iterable[Mapping]) -> tuple[str, int]:
    """sha256 over the model-facing fields of a message list (the lab's ``import_session.norm_hash``)."""
    rows = [{k: m.get(k) for k in _KEEP if m.get(k) is not None} for m in conversation]
    blob = json.dumps(rows, sort_keys=True, ensure_ascii=False, separators=(",", ":"), default=str).encode()
    return hashlib.sha256(blob).hexdigest(), len(rows)


def file_sha(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def parse_json_tail(stdout: str) -> dict:
    """The ``--json`` document of ``hermes handover …`` (the last stdout line that is a JSON object)."""
    for line in reversed((stdout or "").strip().splitlines()):
        line = line.strip()
        if line.startswith("{") and line.endswith("}"):
            with contextlib.suppress(ValueError):
                doc = json.loads(line)
                if isinstance(doc, dict):
                    return doc
    return {}


def text_of(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(str(p.get("text", "")) for p in content if isinstance(p, dict))
    return "" if content is None else json.dumps(content, ensure_ascii=False, default=str)


def message_text(msg: Mapping) -> str:
    return text_of(msg.get("content"))


# ---------------------------------------------------------------------------------------------------------
# Evidence
# ---------------------------------------------------------------------------------------------------------


def evidence_root() -> Path:
    stamp = os.environ.get("CTXH_E2E_EVIDENCE") or time.strftime("%Y%m%d", time.gmtime())
    return LAB / "evidence" / f"e2e-{stamp}"


class Evidence:
    """Raw data of one test under ``lab/evidence/e2e-<date>/<test>/`` (files numbered in write order)."""

    def __init__(self, root: Path) -> None:
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self._n = 0
        self._lock = threading.Lock()

    def write(self, name: str, data: Any) -> Path:
        with self._lock:
            self._n += 1
            path = self.root / f"{self._n:02d}-{name}"
        if isinstance(data, bytes):
            path.write_bytes(data)
        elif isinstance(data, str):
            path.write_text(data, encoding="utf-8")
        else:
            path.write_text(json.dumps(data, indent=2, ensure_ascii=False, default=str), encoding="utf-8")
        return path

    def cli(self, name: str, res: CliResult) -> None:
        self.write(
            f"{name}.json",
            {
                "args": res.args,
                "rc": res.rc,
                "elapsed_s": round(res.elapsed, 2),
                "data": res.data,
                "stdout_tail": res.stdout[-6000:],
                "stderr_tail": res.stderr[-6000:],
            },
        )


# ---------------------------------------------------------------------------------------------------------
# Results
# ---------------------------------------------------------------------------------------------------------


@dataclass
class CliResult:
    """One ``hermes handover … --json`` run."""

    args: list[str]
    rc: int
    stdout: str
    stderr: str
    data: dict
    elapsed: float

    @property
    def text(self) -> str:
        return str(self.data.get("text") or self.stdout)

    @property
    def job_id(self) -> str | None:
        job = self.data.get("job_id")
        return str(job) if job else None

    @property
    def error_code(self) -> str | None:
        err = self.data.get("error")
        return str(err.get("code")) if isinstance(err, Mapping) and err.get("code") else None

    def brief(self) -> str:
        return f"rc={self.rc} data={json.dumps(self.data, default=str)[:1500]} stderr={self.stderr[-1500:]}"


@dataclass
class Turn:
    """One ``prompt.submit`` through a gateway WebSocket."""

    resume: dict | None
    submit: dict
    done: dict | None
    events: list[dict] = field(default_factory=list)

    @property
    def text(self) -> str:
        payload = (self.done or {}).get("payload") or {}
        return str(payload.get("text") or payload.get("final_response") or payload.get("message") or "")

    @property
    def ok(self) -> bool:
        return bool(self.done) and self.done.get("type") == "message.complete"

    def brief(self) -> dict:
        return {
            "resume_error": (self.resume or {}).get("error"),
            "submit": self.submit,
            "done": self.done,
            "event_types": [e.get("type") for e in self.events][-40:],
        }


@dataclass
class Source:
    """A session on one home prepared as the source of a scenario."""

    inst: str
    sid: str
    title: str
    repo: Path | None = None
    rb: Any = None
    spill: Path | None = None
    extra: dict = field(default_factory=dict)


# ---------------------------------------------------------------------------------------------------------
# Tripwire (E2): a listener on a stopped peer's port that records every connection attempt
# ---------------------------------------------------------------------------------------------------------


class Tripwire:
    def __init__(self, port: int) -> None:
        self.port = port
        self.hits: list[float] = []
        self._sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._sock.bind(("127.0.0.1", port))
        self._sock.listen(16)
        self._sock.settimeout(0.2)
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name=f"e2e-tripwire-{port}", daemon=True)
        self._thread.start()

    def _run(self) -> None:
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except (TimeoutError, OSError):
                continue
            self.hits.append(time.time())
            with contextlib.suppress(OSError):
                conn.close()

    def close(self) -> None:
        self._stop.set()
        self._thread.join(5)
        with contextlib.suppress(OSError):
            self._sock.close()


# ---------------------------------------------------------------------------------------------------------
# The lab
# ---------------------------------------------------------------------------------------------------------


class Lab:
    """The paired two-gateway stand (homes ``a`` = server, ``b`` = laptop)."""

    def __init__(self, evidence: Evidence | None = None) -> None:
        self.ctl = _bin_module("labctl")
        self.GW = _bin_module("gw").GW
        self.ev = evidence
        self.tokens = dict(TOKENS)
        self.py = str(self.ctl.PY)
        self.run_id = uid(4)

    # -- paths ---------------------------------------------------------------------------------------------
    def home(self, inst: str) -> Path:
        return Path(self.ctl.home(inst)).resolve()

    def work(self, inst: str) -> Path:
        return self.home(inst) / "work"

    def user_home(self, inst: str) -> Path:
        return self.home(inst) / "home"

    def ws(self, inst: str) -> Path:
        """Session workspaces of the suite, outside the homes: a cwd inside ``lab/<x>`` is Hermes' own tree there
        (HOME = ``lab/<x>/home`` lies inside HERMES_HOME in this lab) and is never sent (lab finding L3-F02)."""
        return LAB / "ws-e2e" / inst

    def handover_root(self, inst: str) -> Path:
        """``workspace_root`` = the only ``allow_roots`` entry of the suite (the default ``~/hermes-handover`` lies
        inside HERMES_HOME in this lab and is refused as a target, lab finding L3-F01)."""
        return LAB / "recv-e2e" / inst

    def data_dir(self, inst: str) -> Path:
        return self.home(inst) / "plugin-data" / "ctx_handover"

    def log_path(self, component: str) -> Path:
        return LAB / "run" / f"{component}.log"

    # -- labctl --------------------------------------------------------------------------------------------
    def labctl(self, *args: str, extra_env: Mapping[str, str] | None = None, timeout: float = 600.0):
        env = _child_env(os.environ)
        for key in ("GW_TOKEN", "SERVE_TOKEN", "SERVE_ISOLATED"):
            env.pop(key, None)
        env.update(extra_env or {})
        res = subprocess.run(  # noqa: S603 - fixed interpreter and the lab's own labctl.py, no shell
            [self.py, str(BIN / "labctl.py"), *args],
            env=env,
            cwd=str(LAB),
            capture_output=True,
            text=True,
            timeout=timeout,
            stdin=subprocess.DEVNULL,
            check=False,
        )
        if self.ev is not None:
            self.ev.write(
                "labctl-" + re.sub(r"[^A-Za-z0-9._-]+", "_", "-".join(args[:2]))[:60] + ".txt",
                f"$ labctl.py {' '.join(args)}\nrc={res.returncode}\n{res.stdout}\n{res.stderr}",
            )
        return res

    def labctl_bg(self, *args: str, log: Path, extra_env: Mapping[str, str] | None = None) -> subprocess.Popen:
        """``labctl.py <args>`` in the background with the environment of :meth:`labctl`, output appended to ``log``,
        in a process group of its own; the caller waits for it and ends it with :func:`kill_tree` in ``finally``. A
        component it starts runs in a session of its own (``labctl.spawn``) and is no member of that group."""
        env = _child_env(os.environ)
        for key in ("GW_TOKEN", "SERVE_TOKEN", "SERVE_ISOLATED"):
            env.pop(key, None)
        env.update(extra_env or {})
        with open(log, "ab") as fh:  # the child holds its own descriptor; the parent's closes here
            return subprocess.Popen(  # noqa: S603 - fixed interpreter and the lab's own labctl.py, no shell
                [self.py, str(BIN / "labctl.py"), *args],
                env=env,
                cwd=str(LAB),
                stdin=subprocess.DEVNULL,
                stdout=fh,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )

    def pid(self, component: str) -> int | None:
        return self.ctl.read_pid(component)

    def running(self, component: str) -> bool:
        pid = self.pid(component)
        if not pid:
            return False
        if component.startswith("gw-"):
            desc = self.ctl.identify(component[3:]) or {}
            return desc.get("state") == "ready"
        if component.startswith("serve-"):
            return self.ctl.http_ok(f"http://127.0.0.1:{self.ctl.SERVE_PORT[component[6:]]}/api/status") is not None
        return True

    def start(self, *components: str) -> None:
        """Start through ``labctl.py start`` with the fixed token of the home; a gateway or serve that came up
        without the plugin (version-probe flake of source installs under host load, PATH.md "Stolpersteine") is
        restarted up to twice. Fixed lab ports must be free first (port collision 47112 seen by the lab runner,
        ``reports/L3-findings.md`` "Labor-Beobachtungen"): waits up to 90 s."""
        for comp in components or COMPONENTS:
            port = PORTS.get(comp)
            if port and not wait_for(lambda port=port: port_free(port), 90, poll=0.5):
                raise LabError(f"port {port} of {comp} is still in use after 90 s")
            for attempt in range(3):
                extra: dict[str, str] = {}
                if comp.startswith("gw-"):
                    extra["GW_TOKEN"] = self.tokens[comp[3:]]
                elif comp.startswith("serve-"):
                    extra["SERVE_TOKEN"] = self.tokens[comp[6:]]
                log = self.log_path(comp)
                offset = log.stat().st_size if log.exists() else 0
                res = self.labctl("start", comp, extra_env=extra, timeout=300)
                if not self.running(comp):
                    raise LabError(f"{comp} did not come up: {res.stdout[-1200:]} {res.stderr[-600:]}")
                if self._plugin_loaded(comp, log, offset) or attempt == 2:
                    break
                self.labctl("stop", comp, timeout=300)

    def _plugin_loaded(self, comp: str, log: Path, offset: int) -> bool:
        if comp == "llm":
            return True
        if log.exists():
            with open(log, "rb") as fh:
                fh.seek(offset)
                if b"Plugin 'ctx_handover' skipped" in fh.read():
                    return False
        if comp.startswith("gw-"):
            inst = comp[3:]
            try:
                sid = self.new_session(inst, self.work(inst))
                out, _ = self.slash(inst, sid, "/handover help", timeout=60, linger=0.2)
            except Exception:
                return False
            return "ctx_handover moves" in out
        return True

    def stop(self, *components: str) -> None:
        self.labctl("stop", *components, timeout=300)
        for comp in components:
            if self.pid(comp):
                raise LabError(f"{comp} still runs after labctl stop")

    def restart(self, *components: str) -> None:
        self.stop(*components)
        self.start(*components)

    def status_text(self) -> str:
        res = self.labctl("status", timeout=60)
        return res.stdout + res.stderr

    def lab_tagged(self) -> list[tuple[int, str]]:
        return list(self.ctl.lab_tagged())

    # -- configuration -------------------------------------------------------------------------------------
    def config_path(self, inst: str) -> Path:
        return self.home(inst) / "config.yaml"

    def env_path(self, inst: str) -> Path:
        return self.home(inst) / ".env"

    def config_set(self, inst: str, key: str, value: str) -> None:
        res = self.hermes(inst, "config", "set", key, value, timeout=180)
        if res.returncode != 0:
            raise LabError(f"[{inst}] hermes config set {key} failed: {res.stdout[-600:]} {res.stderr[-600:]}")

    def setting(self, inst: str, key: str, value: str) -> None:
        self.config_set(inst, f"plugins.entries.ctx_handover.settings.{key}", value)

    def profile_home(self, inst: str) -> None:
        """``terminal.home_mode: profile`` for ``inst``, so ``~`` of its Hermes processes is ``<HERMES_HOME>/home``.

        The plugin expands ``~`` as Hermes' own tools do (``engine/hostcompat.py`` ``effective_home`` →
        upstream ``hermes_constants.get_subprocess_home``, since the L3-F20 fix): on a host (no container), a launch
        ``HOME`` equal to the profile home ``<HERMES_HOME>/home`` — the lab's ``HOME`` — is repaired to the account's
        home from the password database (``/root`` on this host); containers and ``home_mode: profile`` keep
        ``<HERMES_HOME>/home``. A test that needs ``~`` inside HERMES_HOME (the official image, R29) sets this inside
        ``preserved(inst)``; without it the schema default ``~/hermes-handover`` is the host's real
        ``/root/hermes-handover`` (Nachlauf ``edd53876``: written there by ``test_l3_f01_*``)."""
        self.config_set(inst, "terminal.home_mode", "profile")

    def env_set(self, inst: str, key: str, value: str | None) -> None:
        """Set (or with ``None`` remove) ``key`` in the home's ``.env`` — what the Desktop settings form writes."""
        path = self.env_path(inst)
        lines = path.read_text(encoding="utf-8").splitlines() if path.exists() else []
        lines = [ln for ln in lines if not ln.startswith(f"{key}=")]
        if value is not None:
            lines.append(f"{key}={value}")
        path.write_text("\n".join(lines) + "\n", encoding="utf-8")
        path.chmod(0o600)

    @contextlib.contextmanager
    def preserved(self, inst: str, *, restart: Iterable[str] = ()) -> Iterator[None]:
        """Restore ``config.yaml`` and ``.env`` of ``inst`` byte-exact afterwards, then restart ``restart``."""
        saved = {p: p.read_bytes() for p in (self.config_path(inst), self.env_path(inst)) if p.exists()}
        try:
            yield
        finally:
            for path, data in saved.items():
                path.write_bytes(data)
            comps = list(restart)
            if comps:
                self.restart(*comps)

    # -- hermes CLI ----------------------------------------------------------------------------------------
    def hermes(self, inst: str, *args: str, timeout: float = 300.0, env_extra: Mapping[str, str] | None = None):
        """``hermes …`` in the lab environment of ``inst``. Retried (≤ 3 more times) when the plugin was skipped in
        that process by the version-probe flake ("'handover' is not a `hermes` command", PATH.md)."""
        env = _child_env(self.ctl.env_for(inst, **dict(env_extra or {})))
        for _attempt in range(4):
            res = subprocess.run(  # noqa: S603 - the lab's hermes executable, argv built by the test, no shell
                [str(self.ctl.HERMES), *args],
                env=env,
                cwd=str(self.work(inst)),
                capture_output=True,
                text=True,
                timeout=timeout,
                stdin=subprocess.DEVNULL,
                check=False,
            )
            if "is not a `hermes` command" not in res.stdout + res.stderr:
                break
        return res

    def hermes_bg(self, inst: str, *args: str, log: Path) -> subprocess.Popen:
        """Start ``hermes …`` in the background (own process group); the caller ends it with :func:`kill_tree`."""
        env = _child_env(self.ctl.env_for(inst))
        fh = open(log, "ab")  # noqa: SIM115 - handed to the child, closed by the caller through the Popen
        return subprocess.Popen(  # noqa: S603 - the lab's hermes executable, argv built by the test, no shell
            [str(self.ctl.HERMES), *args],
            env=env,
            cwd=str(self.work(inst)),
            stdin=subprocess.DEVNULL,
            stdout=fh,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )

    def handover(self, inst: str, *args: str, timeout: float = 600.0) -> CliResult:
        argv = ["handover", *args, "--json"]
        t0 = time.monotonic()
        res = self.hermes(inst, *argv, timeout=timeout)
        out = CliResult(argv, res.returncode, res.stdout, res.stderr, parse_json_tail(res.stdout), 0.0)
        out.elapsed = time.monotonic() - t0
        return out

    def job_status(self, inst: str, job_id: str) -> dict:
        res = self.handover(inst, "status", job_id, timeout=180)
        status = res.data.get("status") if isinstance(res.data.get("status"), Mapping) else {}
        job = status.get("job") if isinstance(status.get("job"), Mapping) else {}
        return {"rc": res.rc, "text": res.text, "job": dict(job), "status": dict(status)}

    def chat_q(self, inst: str, sid: str, text: str, *, timeout: float = 240.0):
        """Classic CLI path ``hermes chat --resume <sid> -q <text>`` (attaches to the home's gateway, LAB L3)."""
        return self.hermes(inst, "chat", "--resume", sid, "-q", text, timeout=timeout)

    # -- gateway -------------------------------------------------------------------------------------------
    def gw(self, inst: str) -> Any:
        return self.GW(inst)

    def gw_rpc(self, inst: str, method: str, params: dict, *, resume: str | None = None, timeout: float = 120.0):
        gw = self.gw(inst)
        try:
            if resume:
                gw.rpc("session.resume", {"session_id": resume})
            return gw.rpc(method, params, timeout=timeout)
        finally:
            gw.close()

    def new_session(self, inst: str, cwd: Path) -> str:
        cwd.mkdir(parents=True, exist_ok=True)
        reply = self.gw_rpc(inst, "session.create", {"request_id": f"e2e-{uid(12)}", "source": "cli", "cwd": str(cwd)})
        result = reply.get("result") or {}
        sid = result.get("session_id") or (result.get("ref") or {}).get("session_id") or result.get("id")
        if not sid:
            raise LabError(f"session.create on {inst} returned no id: {reply}")
        return str(sid)

    def turn(self, inst: str, sid: str, text: str, *, timeout: float = 180.0, resume: bool = True) -> Turn:
        gw = self.gw(inst)
        try:
            resumed = gw.rpc("session.resume", {"session_id": sid}) if resume else None
            submit = gw.rpc("prompt.submit", {"session_id": sid, "text": text, "input_id": f"e2e-{uid(12)}"})
            done = gw.wait(lambda ev: ev.get("type") in {"message.complete", "error"}, timeout)
            gw.linger(0.3)
            return Turn(resumed, submit, done, list(gw.events))
        finally:
            gw.close()

    def turn_bg(self, inst: str, sid: str, text: str, *, timeout: float = 300.0) -> tuple[threading.Thread, dict]:
        """A turn on a daemon thread (the WebSocket closes when it ends); ``box['turn']`` holds the result."""
        box: dict[str, Any] = {}

        def run() -> None:
            try:
                box["turn"] = self.turn(inst, sid, text, timeout=timeout)
            except Exception as exc:  # recorded for the test, never raised on the thread
                box["error"] = repr(exc)

        thread = threading.Thread(target=run, name=f"e2e-turn-{inst}", daemon=True)
        thread.start()
        return thread, box

    def slash(self, inst: str, sid: str, command: str, *, timeout: float = 300.0, linger: float = 1.0):
        gw = self.gw(inst)
        try:
            gw.rpc("session.resume", {"session_id": sid})
            reply = gw.rpc("slash.exec", {"session_id": sid, "command": command}, timeout=timeout)
            gw.linger(linger)
            events = list(gw.events)
        finally:
            gw.close()
        result = reply.get("result") or {}
        return str(result.get("output") or reply.get("error") or ""), events

    def compress(self, inst: str, sid: str, *, timeout: float = 300.0) -> dict:
        """Manual compaction the way Desktop/Ink request it: ``session.mutate`` with ``operation: compress``, the
        session's ``runtime_revision`` as ``expected_revision`` and its ``runtime_generation`` as
        ``expected_generation`` (``slash.exec "/compress"`` is refused for local sessions,
        ``gateway/session_commands.py:133-145``; without ``expected_generation`` the gateway answers
        ``invalid_params``)."""
        row = self.row(inst, sid)
        gw = self.gw(inst)
        try:
            gw.rpc("session.resume", {"session_id": sid})
            params = {
                "session_id": sid,
                "request_id": f"e2e-{uid(12)}",
                "expected_revision": int(row.get("runtime_revision") or 0),
                "expected_generation": int(row.get("runtime_generation") or 0),
                "operation": "compress",
                "payload": {},
            }
            reply = gw.rpc("session.mutate", params, timeout=timeout)
            gw.linger(1.0)
            return {"params": params, "reply": reply, "events": [e.get("type") for e in gw.events][-30:]}
        finally:
            gw.close()

    # -- state.db ------------------------------------------------------------------------------------------
    @contextlib.contextmanager
    def db(self, inst: str, *, write: bool = False) -> Iterator[Any]:
        from hermes_state import SessionDB

        db = SessionDB(self.home(inst) / "state.db", read_only=not write)
        try:
            yield db
        finally:
            db.close()

    def row(self, inst: str, sid: str) -> dict:
        with self.db(inst) as db:
            return dict(db.get_session(sid) or {})

    def tip(self, inst: str, sid: str) -> str:
        with self.db(inst) as db:
            return str(db.resolve_resume_session_id(sid))

    def lineage(self, inst: str, sid: str) -> list[str]:
        with self.db(inst) as db:
            return list(db.get_compression_lineage(sid) or [sid])

    def marks(self, inst: str, sid: str) -> dict:
        """The primary lock marks of DESIGN §7.4 on one row."""
        r = self.row(inst, sid)
        return {
            "ended": r.get("ended_at") is not None,
            "end_reason": r.get("end_reason"),
            "archived": bool(r.get("archived")),
            "hidden": bool(r.get("hidden")),
            "title": r.get("title"),
        }

    def meta(self, inst: str, key: str) -> str | None:
        with self.db(inst) as db:
            return db.get_meta(key)

    def messages(self, inst: str, sid: str, *, inactive: bool = True) -> list[dict]:
        with self.db(inst) as db:
            return [dict(m) for m in db.get_messages(sid, include_inactive=inactive, include_compacted=inactive)]

    def hashes(self, inst: str, sid: str) -> dict:
        """H_all / H_rows / H_model (the plugin's ``session_hash`` definitions, DESIGN §6.9) of the lineage of ``sid``,
        computed from Hermes' own export on that home, plus an independent model/display fingerprint."""
        from ctx_handover.engine import session_hash as sh

        with self.db(inst) as db:
            tip = db.resolve_resume_session_id(sid)
            exp = db.export_session_lineage(tip, include_compacted=True, include_inactive=True) or {}
            segments = list(exp.get("segments") or [])
            model, display = db.get_resume_conversations(tip)
        seg_rows = [list(s.get("messages") or []) for s in segments]
        tip_rows = next((list(s.get("messages") or []) for s in segments if s.get("id") == tip), [])
        return {
            "tip": tip,
            "segments": len(segments),
            "rows": sum(len(r) for r in seg_rows),
            "h_all": sh.h_all(seg_rows),
            "h_rows": sh.h_rows(tip_rows),
            "h_model": sh.h_model(model),
            "model_norm": norm_hash(model),
            "display_norm": norm_hash(display),
        }

    # -- ledger (read-only) --------------------------------------------------------------------------------
    def ledger_rows(self, inst: str, sql: str, params: tuple = ()) -> list[dict]:
        path = self.data_dir(inst) / "ledger.sqlite3"
        if not path.exists():
            return []
        conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=10)
        conn.row_factory = sqlite3.Row
        try:
            return [dict(r) for r in conn.execute(sql, params).fetchall()]
        except sqlite3.OperationalError:
            return []
        finally:
            conn.close()

    def job_row(self, inst: str, job_id: str, role: str = "conductor") -> dict | None:
        rows = self.ledger_rows(inst, "SELECT * FROM jobs WHERE job_id = ? AND role = ?", (job_id, role))
        return rows[0] if rows else None

    def latest_job(self, inst: str, role: str = "conductor", *, after: float = 0.0) -> dict | None:
        rows = self.ledger_rows(
            inst,
            "SELECT * FROM jobs WHERE role = ? AND created_at >= ? ORDER BY created_at DESC LIMIT 1",
            (role, after),
        )
        return rows[0] if rows else None

    def source_row(self, inst: str, job_id: str) -> dict | None:
        rows = self.ledger_rows(inst, "SELECT * FROM source_handovers WHERE job_id = ?", (job_id,))
        return rows[0] if rows else None

    def arrival(self, inst: str, sid: str) -> dict | None:
        rows = self.ledger_rows(inst, "SELECT * FROM arrivals WHERE session_id = ?", (sid,))
        return rows[0] if rows else None

    def events(self, inst: str, job_id: str, role: str = "conductor") -> list[dict]:
        return self.ledger_rows(
            inst, "SELECT * FROM job_events WHERE job_id = ? AND role = ? ORDER BY seq", (job_id, role)
        )

    def wait_job(
        self,
        inst: str,
        job_id: str,
        states: Iterable[str],
        *,
        role: str = "conductor",
        timeout: float = 300.0,
        poll: float = 0.05,
    ) -> str | None:
        """Poll the ledger until the job of ``role`` is in one of ``states``; returns that state or None."""
        wanted = set(states)
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            row = self.job_row(inst, job_id, role)
            if row and row["state"] in wanted:
                return str(row["state"])
            time.sleep(poll)
        return None

    def wait_new_job(self, inst: str, after: float, *, role: str = "conductor", timeout: float = 120.0) -> dict:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            row = self.latest_job(inst, role, after=after)
            if row:
                return row
            time.sleep(0.05)
        raise LabError(f"no {role} job appeared on {inst} within {timeout}s")

    # -- fake LLM ------------------------------------------------------------------------------------------
    def llm_mark(self) -> int:
        path = LLM_DIR / "requests.jsonl"
        if not path.exists():
            return 0
        with open(path, "rb") as fh:
            return sum(1 for _ in fh)

    def llm_since(self, mark: int) -> list[dict]:
        path = LLM_DIR / "requests.jsonl"
        if not path.exists():
            return []
        out = []
        with open(path, encoding="utf-8") as fh:
            for i, line in enumerate(fh):
                if i >= mark and line.strip():
                    with contextlib.suppress(ValueError):
                        out.append(json.loads(line))
        return out

    @contextlib.contextmanager
    def llm_rules(self, rules: list[dict]) -> Iterator[None]:
        path = LLM_DIR / "rules.json"
        old = path.read_text(encoding="utf-8") if path.exists() else "[]\n"
        path.write_text(json.dumps(rules, indent=1), encoding="utf-8")
        try:
            yield
        finally:
            path.write_text(old, encoding="utf-8")

    # -- workspaces ----------------------------------------------------------------------------------------
    @contextlib.contextmanager
    def outside(self, ev: Evidence | None = None) -> Iterator[Path]:
        """A fresh folder ``<OUTSIDE_ROOT>/<run id>-<uid>`` (mode 0700) outside every git work tree; afterwards it is
        removed with everything in it, and ``OUTSIDE_ROOT`` too when that is empty then.

        Refused with ``LabError`` before anything is created: a relative ``CTXH_E2E_OUTSIDE``, one at or below
        ``/tmp``, ``/var/tmp`` or ``/dev/shm``, and one at or below a folder holding ``.git`` (``git_work_tree_of``)."""
        base = OUTSIDE_ROOT.expanduser()
        if not base.is_absolute():
            raise LabError(f"scaffold: CTXH_E2E_OUTSIDE must be an absolute path, not {base}")
        base = Path(os.path.realpath(base))
        temp = set(_NO_TEMP) | {Path(os.path.realpath(t)) for t in _NO_TEMP}
        if any(base == t or base.is_relative_to(t) for t in temp):
            raise LabError(f"scaffold: {base} is a temp location (no experimental data there); set CTXH_E2E_OUTSIDE")
        tree = git_work_tree_of(base)
        if tree is not None:
            raise LabError(f"scaffold: {base} lies in the git work tree {tree}; set CTXH_E2E_OUTSIDE outside of it")
        base.mkdir(mode=0o700, parents=True, exist_ok=True)
        root = base / f"{self.run_id}-{uid(6)}"
        root.mkdir(mode=0o700)
        root.chmod(0o700)
        try:
            yield root
        finally:
            shutil.rmtree(root, ignore_errors=True)
            with contextlib.suppress(OSError):
                base.rmdir()
            if ev is not None:
                ev.write("outside-root.json", {"root": str(root), "removed": not root.exists(), "base": str(base)})

    @staticmethod
    def repo(path: Path) -> Any:
        from ctx_handover.tests.fakes.repo_builder import RepoBuilder

        return RepoBuilder(Path(path))

    @staticmethod
    def make_repo(path: Path, *, dirty: bool = True, ignored: bool = True, files: Mapping[str, str] | None = None):
        """A repository built only through ``RepoBuilder`` (technical identity per git call, never a config write).

        Committed ``README.md``, ``src/app.py``, ``.gitignore`` (``build/``); with ``dirty`` an unstaged edit, a
        staged new file and an untracked file; with ``ignored`` an ignored ``build/out.bin``."""
        from ctx_handover.tests.fakes.repo_builder import RepoBuilder

        rb = RepoBuilder.init(Path(path))
        rb.write("README.md", "# e2e demo\n")
        rb.write("src/app.py", "print('hello from the lab')\n")
        rb.write(".gitignore", "build/\n")
        for rel, content in (files or {}).items():
            rb.write(rel, content)
        rb.commit_all("initial")
        if dirty:
            rb.write("README.md", "# e2e demo\nwork in progress\n")
            rb.write("src/new.py", "VALUE = 1\n")
            rb.add("src/new.py")
            rb.write("notes/scratch.txt", "untracked note\n")
        if ignored:
            rb.write("build/out.bin", b"\x00\x01" * 2048)
        return rb

    def make_source(
        self,
        inst: str,
        name: str,
        *,
        title: str | None = None,
        repo: bool = True,
        dirty: bool = True,
        turns: Iterable[str] = ("first question", "second question"),
        tool_turn: list[dict] | None = None,
        files: Mapping[str, str] | None = None,
    ) -> Source:
        """A session on ``inst`` created through its gateway, with a workspace under ``<home>/work/<name>``.

        ``tool_turn``: tool calls the fake model answers for one extra turn (e.g. a terminal command whose output
        spills, a ``todo_list`` call); the gateway executes them, so the transcript has real tool rows."""
        cwd = self.ws(inst) / name
        rb = self.make_repo(cwd, dirty=dirty, files=files) if repo else None
        if not repo:
            cwd.mkdir(parents=True, exist_ok=True)
            for rel, content in (files or {}).items():
                (cwd / rel).parent.mkdir(parents=True, exist_ok=True)
                (cwd / rel).write_text(content, encoding="utf-8")
        sid = self.new_session(inst, cwd)
        for text in turns:
            t = self.turn(inst, sid, f"{text} [{name}]")
            if not t.ok:
                raise LabError(f"turn on {inst} failed: {t.brief()}")
        if tool_turn:
            token = f"TOOLS-{name}-{uid()}"
            rule = {
                "id": token,
                "when": {"kind": "main", "last_user_contains": token},
                "reply": {"tool_calls": tool_turn},
            }
            with self.llm_rules([rule]):
                t = self.turn(inst, sid, f"please run the tools {token}", timeout=300)
            if not t.ok:
                raise LabError(f"tool turn on {inst} failed: {t.brief()}")
        sid = self.tip(inst, sid)
        title = title or f"{name} {self.run_id}"
        with self.db(inst, write=True) as db:
            db.set_session_title(sid, title)
        return Source(inst, sid, title, cwd, rb)

    def spill_files(self, inst: str, sid: str) -> list[Path]:
        """Spillover files named by the session's transcript (``Full output saved to: …``)."""
        out = []
        for m in self.messages(inst, sid):
            for match in re.finditer(
                r"Full output saved to: (\S+)", message_text(m) + "\n" + str(m.get("api_content") or "")
            ):
                path = Path(match.group(1).strip())
                if path not in out:
                    out.append(path)
        return out

    # -- pairing -------------------------------------------------------------------------------------------
    def pair(
        self, listener: str, dialer: str, *, hint: str, name: str, gate: str | None = None
    ) -> tuple[CliResult, CliResult]:
        """``/handover pair --name <hint>`` on ``listener`` (CLI), then the paste string on ``dialer``."""
        made = self.handover(listener, "pair", "--name", hint, timeout=180)
        match = PAIR_RE.search(made.text)
        if made.rc != 0 or not match:
            raise LabError(f"pair code on {listener} failed: {made.brief()}")
        args = ["pair", match.group(1), "--name", name] + (["--gate", gate] if gate else [])
        redeemed = self.handover(dialer, *args, timeout=180)
        if redeemed.rc != 0:
            raise LabError(f"pairing on {dialer} failed: {redeemed.brief()}")
        return made, redeemed

    def _server_inbound_on_a(self) -> list[str]:
        rows = self.ledger_rows("b", "SELECT peer_id FROM peers WHERE name = ?", (SERVER,))
        if not rows:
            return []
        live = self.ledger_rows(
            "a",
            "SELECT name FROM peers WHERE peer_id = ? AND direction = 'inbound' AND revoked_at IS NULL",
            (str(rows[0]["peer_id"]),),
        )
        return [str(r["name"]) for r in live]

    def _dial_entry(self, inst: str, name: str) -> bool:
        return bool(self.ledger_rows(inst, "SELECT 1 FROM peers WHERE name = ? AND direction = 'dial'", (name,)))

    def remove_server_entry(self, ev: Evidence | None = None) -> None:
        """B ``peers remove server`` before a test pairs B with A a second time.

        Since integrator round 3 (``feat/ctx-handover`` ``ee82897c``, L3-F25) the dialer keeps one pairing per gateway
        profile and refuses a second one — under another name or through another address of the same gateway — before
        any request ("this device is already paired with that gateway as server … run /handover peers remove server
        first"). The removal also blanks the settings form's slot ``CTX_HANDOVER_PEER_TOKEN`` when it held the value
        copied for ``server`` alone (review R2-L1); a test that pairs through that slot sets it again. A's inbound row
        ``laptop`` needs no ``peers revoke``: the next pairing of B's key revokes it on A (review M13). Afterwards
        ``restore_pairing`` gives the stand its pairing back. Scaffold check: B keeps no dial entry ``server``."""
        if not self._dial_entry("b", SERVER):
            return
        res = self.handover("b", "peers", "remove", SERVER, timeout=120)
        if ev is not None:
            ev.cli("remove-server", res)
        if res.rc != 0 or self._dial_entry("b", SERVER):
            raise LabError(f"scaffold: B's dial entry {SERVER} was not removed: {res.brief()}")

    def restore_pairing(self, ev: Evidence | None = None) -> None:
        """Give the stand its pairing A←B back (B's dial entry ``server`` = A's inbound ``laptop``) after a test paired
        B with A a second time.

        Such a test removes ``server`` first (``remove_server_entry``, L3-F25), and its pairing revokes A's ``laptop``
        (``engine/ledger.py`` ``redeem_pair_code``, review M13: one key, one live row). Re-pairs as ``conftest`` does:
        B's ``server`` removed if it is still there (``peers remove``), B's slot ``CTX_HANDOVER_PEER_TOKEN`` set to
        A's fixed token again (a removal blanks it, review R2-L1), A's ``laptop`` renamed out of the way
        (``peers rename``, when it exists), ``laptop``/``server`` by paste string. Checked: A has exactly one live
        inbound row with the peer id of B's ``server``, named ``laptop``. Nothing happens when that already holds."""
        if self._server_inbound_on_a() == [LAPTOP]:
            return
        steps = []
        if self._dial_entry("b", SERVER):
            steps.append(("restore-remove-server", self.handover("b", "peers", "remove", SERVER, timeout=120)))
        self.env_set("b", "CTX_HANDOVER_PEER_TOKEN", TOKENS["a"])
        old = None
        if self.ledger_rows("a", "SELECT 1 FROM peers WHERE name = ?", (LAPTOP,)):
            old = f"{LAPTOP}-old{uid(4)}"
            steps.append(("restore-rename-laptop", self.handover("a", "peers", "rename", LAPTOP, old, timeout=120)))
        made, redeemed = self.pair("a", "b", hint=LAPTOP, name=SERVER)
        steps += [("restore-pair-code", made), ("restore-pair-redeem", redeemed)]
        live = self._server_inbound_on_a()
        if ev is not None:
            for name, res in steps:
                ev.cli(name, res)
            ev.write("restore-pairing.json", {"live_inbound_of_server": live, "renamed": old})
        if live != [LAPTOP]:
            raise LabError(f"scaffold: the stand's pairing A<-B was not restored: {live}")


# ---------------------------------------------------------------------------------------------------------
# Process helpers
# ---------------------------------------------------------------------------------------------------------


def port_free(port: int) -> bool:
    """True when nothing accepts connections on 127.0.0.1:<port> and it can be bound."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.settimeout(0.5)
        if probe.connect_ex(("127.0.0.1", port)) == 0:
            return False
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as bind:
        bind.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            bind.bind(("127.0.0.1", port))
        except OSError:
            return False
    return True


def git_work_tree_of(path: Path) -> Path | None:
    """The nearest folder at or above ``path`` holding a ``.git`` entry (a work tree, a repository or the gitfile of a
    linked worktree), else None."""
    path = Path(path)
    for folder in (path, *path.parents):
        if (folder / ".git").exists():
            return folder
    return None


def tree_digests(root: Path) -> dict[str, str]:
    """sha256 per file below ``root`` without caches (what ``labctl.py install-handover`` copies)."""
    out = {}
    skip = {"__pycache__", ".ruff_cache", ".pytest_cache"}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if d not in skip)
        for name in sorted(filenames):
            if name.endswith((".pyc", ".pyo")):
                continue
            path = Path(dirpath) / name
            out[str(path.relative_to(root))] = file_sha(path)
    return out


def kill_tree(proc: subprocess.Popen | None, *, grace: float = 10.0) -> None:
    """End a process started with ``start_new_session=True`` and its group (SIGTERM, then SIGKILL)."""
    if proc is None or proc.poll() is not None:
        return
    with contextlib.suppress(OSError):
        os.killpg(proc.pid, signal.SIGTERM)
    try:
        proc.wait(grace)
    except subprocess.TimeoutExpired:
        with contextlib.suppress(OSError):
            os.killpg(proc.pid, signal.SIGKILL)
        with contextlib.suppress(subprocess.TimeoutExpired):
            proc.wait(grace)


def pid_alive(pid: int | None) -> bool:
    if not pid:
        return False
    try:
        os.kill(pid, 0)
    except OSError:
        return False
    try:
        state = Path(f"/proc/{pid}/stat").read_text().split(")")[-1].split()[0]
    except OSError:
        return False
    return state != "Z"


def kill_pid(pid: int | None, *, grace: float = 10.0) -> None:
    if not pid_alive(pid):
        return
    with contextlib.suppress(OSError):
        os.kill(int(pid), signal.SIGTERM)
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline and pid_alive(pid):
        time.sleep(0.1)
    if pid_alive(pid):
        with contextlib.suppress(OSError):
            os.kill(int(pid), signal.SIGKILL)


@contextlib.contextmanager
def stopped(pid: int | None) -> Iterator[None]:
    """SIGSTOP ``pid`` for the block, SIGCONT afterwards (always)."""
    if not pid:
        raise LabError("no pid to stop")
    os.kill(pid, signal.SIGSTOP)
    try:
        yield
    finally:
        with contextlib.suppress(OSError):
            os.kill(pid, signal.SIGCONT)


def wait_for(pred: Callable[[], Any], timeout: float, poll: float = 0.25) -> Any:
    deadline = time.monotonic() + timeout
    while True:
        value = pred()
        if value:
            return value
        if time.monotonic() >= deadline:
            return value
        time.sleep(poll)


def requests_with(records: Iterable[Mapping], needle: str) -> list[Mapping]:
    """Fake-LLM records whose message list contains ``needle`` anywhere."""
    out = []
    for rec in records:
        body = rec.get("body") or {}
        if needle in json.dumps(body.get("messages") or [], ensure_ascii=False):
            out.append(rec)
    return out


def compact_records(records: Iterable[Mapping]) -> list[dict]:
    """Evidence form of fake-LLM records: no full bodies (sessions can be megabytes), the parts the tests read."""
    out = []
    for rec in records:
        messages = (rec.get("body") or {}).get("messages") or []
        system = next((message_text(m) for m in messages if m.get("role") == "system"), "")
        last = messages[-1] if messages else {}
        out.append(
            {
                "seq": rec.get("seq"),
                "t": rec.get("t"),
                "kind": rec.get("kind"),
                "caller": rec.get("caller"),
                "rule": rec.get("rule"),
                "model": rec.get("model"),
                "n_messages": rec.get("n_messages"),
                "last_user": rec.get("last_user"),
                "last_message": {"role": last.get("role"), "text": message_text(last)[:6000]} if last else None,
                "system_head": system[:3000],
                "note_messages": sum(1 for m in messages if NOTE_MARK in message_text(m)),
                "reply": rec.get("reply"),
            }
        )
    return out


def system_prompt(rec: Mapping) -> str:
    messages = (rec.get("body") or {}).get("messages") or []
    return next((message_text(m) for m in messages if m.get("role") == "system"), "")


def last_user_message(rec: Mapping) -> str:
    messages = (rec.get("body") or {}).get("messages") or []
    for m in reversed(messages):
        if m.get("role") == "user":
            return message_text(m)
    return ""


def refine_rule(note: str, *, rid: str | None = None, tools: Iterable[str] = ("rg",)) -> dict:
    """Fake-model rule: a valid refinement answer (DESIGN §8.3) carrying ``note`` as ``notes_for_target``."""
    reply = json.dumps({"add": [], "drop": [], "tools_needed": list(tools), "notes_for_target": note})
    return {
        "id": rid or f"refine-{uid()}",
        "when": {"kind": "aux", "has_response_format": True, "any_contains": "notes_for_target"},
        "reply": {"text": reply},
    }


def dest_of(job: Mapping, item_hint: str | None = None) -> tuple[str | None, str | None]:
    """``(path, mode)`` of the first workspace destination in the job's ``ack.dests`` (or the one ending in
    ``item_hint``)."""
    ack = job.get("ack") if isinstance(job.get("ack"), Mapping) else {}
    dests = list(ack.get("dests") or [])
    for entry in dests:
        if isinstance(entry, Mapping):
            path, mode = entry.get("path") or entry.get("dest"), entry.get("mode")
        elif isinstance(entry, (list, tuple)) and len(entry) >= 3:
            path, mode = entry[1], entry[2]
        else:
            continue
        if item_hint is None or str(path).rstrip("/").endswith(item_hint) or item_hint in str(path):
            return (str(path) if path else None), (str(mode) if mode else None)
    return None, None


def without(digest: Mapping[bytes, Any], *prefixes: bytes) -> dict[bytes, Any]:
    return {k: v for k, v in digest.items() if not any(k == p or k.startswith(p + b"/") for p in prefixes)}


def repo_state(path: Path, *, exclude: Iterable[bytes] = (b"build",)) -> dict:
    """Git-observable state of a work tree in JSON form: HEAD, branch, ``status --porcelain=v2`` (object ids
    included), index entries, ``refs/heads`` + ``refs/tags``, stash list and the byte digest of the work tree
    without ``.git`` and the ``exclude`` prefixes (ignored build output does not travel)."""
    from ctx_handover.tests.fakes.repo_builder import RepoBuilder

    rb = RepoBuilder(Path(path))
    branch = rb.run("symbolic-ref", "-q", "HEAD", check=False).stdout.decode(errors="replace").strip()
    refs = {k: v for k, v in rb.refs().items() if k.startswith(("refs/heads/", "refs/tags/"))}
    tree = without(rb.worktree_digest(), *exclude)
    return {
        "head": rb.head(),
        "branch": branch,
        "status": rb.status().decode("utf-8", errors="surrogateescape").split("\x00"),
        "index": rb.index_entries().decode("utf-8", errors="surrogateescape").split("\x00"),
        "refs": refs,
        "stash": [list(e) for e in rb.stash_list()],
        "tree": {k.decode("utf-8", errors="surrogateescape"): list(v) for k, v in sorted(tree.items())},
    }
