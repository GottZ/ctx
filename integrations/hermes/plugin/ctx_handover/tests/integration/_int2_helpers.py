"""Helpers of the TESTPLAN §3 integration tests (package r-int2; ``harness.py`` stays the two-home base).

* ``conductor_ctx`` / ``target_ctx`` / ``job_errors`` — the journal of a job read back the way the conductor and
  the target rebuild their working data (merged ``ctx`` entries of the events).
* ``ServeListener`` — home A served by a real ``hermes serve`` process: the plugin installed into the home's
  ``HERMES_HOME/plugins/ctx_handover`` (a copy of this tree without tests), enabled in its ``config.yaml``, the
  dashboard gate with a fixed ``HERMES_DASHBOARD_SESSION_TOKEN``. Same surface as ``harness.Listener`` (``url``,
  ``token``, ``stop``, ``kill``), so ``harness.pair`` pairs through it.
* ``hermes_cli`` — the real ``hermes`` CLI with a home's environment (never the user's ``~/.hermes``).
* ``HookHost`` — a plugin ``ctx`` that only records what ``register()`` registers (the hooks the core calls).
* ``typed_submit`` / ``withhold_like_core`` — the TUI/Desktop submit-time user row and the core's withheld
  placeholder of a vetoed turn, both through the core's own functions.
* ``python -m ctx_handover.tests.integration._int2_helpers push-and-die <cfg.json>`` — a conductor process that
  runs a push and kills itself (SIGKILL) after a number of block uploads.
"""

from __future__ import annotations

import contextlib
import json
import os
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from collections.abc import Iterable, Mapping
from pathlib import Path

from ctx_handover.engine.model import Role
from ctx_handover.tests.integration.harness import PLUGIN_PARENT, UPSTREAM, Home

PLUGIN_ROOT = Path(__file__).resolve().parents[2]
VENV_BIN = Path(sys.executable).parent
HERMES_BIN = VENV_BIN / "hermes"
PLUGIN_ROUTE = "/api/plugins/ctx_handover/v1"

_DROP_PREFIXES = ("HERMES_", "CTX_HANDOVER_", "GIT_", "XDG_", "PYTHON", "OPENAI", "ANTHROPIC", "OPENROUTER", "NOUS_")
_DROP_SUFFIXES = ("_API_KEY", "_TOKEN", "_SECRET")
_DROP_KEYS = {"VIRTUAL_ENV", "PYTEST_CURRENT_TEST", "PYTEST_ADDOPTS", "NOTIFY_SOCKET", "INVOCATION_ID"}


# ---------------------------------------------------------------------------------------------------------
# journal read-back
# ---------------------------------------------------------------------------------------------------------


def _merged_ctx(home: Home, job_id: str, role: Role) -> dict:
    out: dict = {}
    for ev in home.ledger().events(job_id, role):
        data = ev["data"] or {}
        if isinstance(data.get("ctx"), dict):
            out.update(data["ctx"])
    return out


def conductor_ctx(home: Home, job_id: str) -> dict:
    """The conductor's working data of ``job_id`` as ``Conductor._load`` rebuilds it."""
    return _merged_ctx(home, job_id, Role.CONDUCTOR)


def target_ctx(home: Home, job_id: str) -> dict:
    """The target participant's working data of ``job_id`` (``LocalTarget._ctx``)."""
    return _merged_ctx(home, job_id, Role.TARGET)


def ack_ids(home: Home, job_id: str) -> tuple[str | None, str | None]:
    """``(root_id, tip_id)`` of the target lineage from the conductor's journaled ``CommitAck``."""
    ack = conductor_ctx(home, job_id).get("ack") or {}
    return ack.get("root_id"), ack.get("session_id")


def job_errors(home: Home, job_id: str, role: Role = Role.CONDUCTOR) -> list:
    return [e["data"] for e in home.ledger().events(job_id, role) if e["kind"] == "error"]


def events_of(home: Home, job_id: str, kind: str, role: Role = Role.CONDUCTOR) -> list[dict]:
    return [e["data"] or {} for e in home.ledger().events(job_id, role) if e["kind"] == kind]


def outcome(home: Home, job_id: str) -> tuple:
    """``(state, result, errors)`` of a conductor job, for assertion messages."""
    row = home.ledger().job(job_id, Role.CONDUCTOR) or {}
    return row.get("state"), row.get("result"), job_errors(home, job_id)


# ---------------------------------------------------------------------------------------------------------
# session views
# ---------------------------------------------------------------------------------------------------------

_ROW_DROP = frozenset({"id", "session_id"})


def stored_rows(db, sid: str) -> list[dict]:
    """Every stored row of one segment without storage coordinates, rewind rows left out (DESIGN §6.8 step 4)."""
    out = []
    for r in db.get_messages(sid, include_inactive=True):
        if not r.get("active") and not r.get("compacted"):
            continue
        out.append({k: v for k, v in r.items() if k not in _ROW_DROP})
    return out


def norm_conv(conv: Iterable[Mapping]) -> list[dict]:
    """A model or display history without in-memory row markers and storage ids."""
    out = []
    for m in conv:
        d = {k: v for k, v in m.items() if not k.startswith("_") and k not in ("message_id", "id", "session_id")}
        if m.get("_compressed_summary"):
            d["_compressed_summary"] = True
        out.append(d)
    return out


def histories(db, tip: str) -> tuple[list[dict], list[dict]]:
    """``(model history, display history)`` of ``tip`` as a resume reads them (``get_resume_conversations``)."""
    model, display = db.get_resume_conversations(tip)
    return norm_conv(model), norm_conv(display)


def typed_submit(db, sid: str, text: str) -> int:
    """The user row TUI/Desktop write at submit time, before any lease (``tui_gateway/session_workdir.py``
    ``_write_submit_user_row``): stamped timestamp and message uid, plain ``append_message``."""
    from agent.message_metadata import stamp_message_timestamp, stamp_message_uid

    staged = stamp_message_timestamp({"role": "user", "content": text})
    return db.append_message(
        sid, "user", content=text, timestamp=staged["timestamp"], message_uid=stamp_message_uid(staged)
    )


def withhold_like_core(db, sid: str, row_id: int, typed: str, block_message: str) -> None:
    """What the core does with a turn a ``pre_llm_call`` hook vetoed (``agent/turn_context.py``
    ``_withhold_blocked_prompt``, ``agent/conversation_loop.py``): the submit-time row becomes the withheld
    placeholder (typed text only in ``display_metadata``) and the block text is the assistant reply."""
    from types import SimpleNamespace

    from agent import turn_context

    agent = SimpleNamespace(_session_db=db, session_id=sid)
    turn_user_msg = {"role": "user", "content": typed, "_row_id": row_id, "_submit_row_session_id": sid}
    turn_context._withhold_blocked_prompt(agent, turn_user_msg, typed)
    db.append_message(sid, "assistant", content=block_message)


# ---------------------------------------------------------------------------------------------------------
# plugin ctx that records registrations
# ---------------------------------------------------------------------------------------------------------


class HookHost:
    """A plugin ``ctx`` offering only ``register_hook``: ``register()`` hands it the callbacks the core calls."""

    def __init__(self) -> None:
        self.hooks: dict[str, object] = {}

    def register_hook(self, name: str, callback) -> None:
        self.hooks[name] = callback

    @classmethod
    def registered(cls) -> HookHost:
        import ctx_handover

        host = cls()
        ctx_handover.register(host)
        return host


# ---------------------------------------------------------------------------------------------------------
# real Hermes processes (disposable homes only)
# ---------------------------------------------------------------------------------------------------------


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def hermes_env(home: Home, **extra: str) -> dict[str, str]:
    """Environment of a Hermes process of ``home``: the host's credentials and Hermes variables dropped, the
    home's ``HOME``/``HERMES_HOME``/XDG dirs, no bytecode written anywhere near the plugin tree."""
    env = {
        k: v
        for k, v in os.environ.items()
        if not (k.startswith(_DROP_PREFIXES) or k.endswith(_DROP_SUFFIXES) or k in _DROP_KEYS)
    }
    env.update(home.env())
    tmp = home.root / "tmp"
    tmp.mkdir(parents=True, exist_ok=True)
    env.update(
        {
            "TMPDIR": str(tmp),
            "PYTHONDONTWRITEBYTECODE": "1",
            "PYTHONPYCACHEPREFIX": str(home.root / "pycache"),
            "PYTHONUNBUFFERED": "1",
            "NO_COLOR": "1",
            "TZ": "UTC",
            "HERMES_GATEWAY_LOCK_DIR": str(home.root / "locks"),
            "PATH": f"{VENV_BIN}{os.pathsep}{os.environ.get('PATH', '/usr/bin:/bin')}",
        }
    )
    env.update(extra)
    return env


def install_plugin(home: Home, *, settings: Mapping | None = None) -> Path:
    """Install this plugin tree into ``home`` like a user does (directory copy under ``HERMES_HOME/plugins``,
    ``plugins.enabled``), keeping the plugin settings ``Home.write_settings`` wrote."""
    dest = home.hermes_home / "plugins" / "ctx_handover"
    if dest.exists():
        shutil.rmtree(dest)
    shutil.copytree(PLUGIN_ROOT, dest, ignore=shutil.ignore_patterns("__pycache__", "tests", "*.pyc"))
    cfg_path = home.hermes_home / "config.yaml"
    cfg = json.loads(cfg_path.read_text(encoding="utf-8")) if cfg_path.exists() else {}
    plugins = cfg.setdefault("plugins", {})
    plugins["enabled"] = ["ctx_handover"]
    entry = plugins.setdefault("entries", {}).setdefault("ctx_handover", {})
    entry["enabled"] = True
    entry.setdefault("settings", {}).update(dict(settings or {}))
    cfg.setdefault("updates", {"check": False})
    cfg.setdefault("telemetry", {"shared_metrics": {"enabled": False, "send": False}})
    cfg_path.write_text(json.dumps(cfg), encoding="utf-8")
    return dest


def hermes_cli(home: Home, *args: str, timeout: float = 180.0) -> subprocess.CompletedProcess:
    """``hermes <args>`` as a CLI process of ``home``."""
    return subprocess.run(  # noqa: S603 - fixed argv: the venv's hermes entry point and test arguments
        [str(HERMES_BIN), *args],
        env=hermes_env(home),
        cwd=str(home.root),
        capture_output=True,
        text=True,
        timeout=timeout,
        stdin=subprocess.DEVNULL,
        check=False,
    )


def http_get(url: str, token: str | None = None, timeout: float = 5.0) -> tuple[int | None, bytes]:
    headers = {"X-Hermes-Session-Token": token} if token else {}
    req = urllib.request.Request(url, headers=headers)  # noqa: S310 - loopback URL built by the test
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
            return resp.status, resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read()
    except OSError:
        return None, b""


class ServeListener:
    """Home A behind a real ``hermes serve`` (see module docstring)."""

    def __init__(self, home: Home, *, settings: Mapping | None = None, start_timeout: float = 180.0) -> None:
        self.home = home
        self.token = secrets.token_urlsafe(16)
        self.port = free_port()
        self.url = f"http://127.0.0.1:{self.port}"
        install_plugin(home, settings={"listen_url": self.url, **dict(settings or {})})
        self.log_path = home.root / "serve.log"
        self._log = self.log_path.open("ab")
        env = hermes_env(home, HERMES_DASHBOARD_SESSION_TOKEN=self.token)
        self.proc = subprocess.Popen(  # noqa: S603 - fixed argv: the venv's hermes entry point
            [str(HERMES_BIN), "serve", "--host", "127.0.0.1", "--port", str(self.port)],
            env=env,
            cwd=str(home.root),
            stdin=subprocess.DEVNULL,
            stdout=self._log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        deadline = time.monotonic() + start_timeout
        while time.monotonic() < deadline and self.proc.poll() is None:
            code, _ = http_get(f"{self.url}/api/status", timeout=2.0)
            if code:
                break
            time.sleep(0.2)
        if self.proc.poll() is not None or not http_get(f"{self.url}/api/status")[0]:
            self.stop()
            raise RuntimeError(f"hermes serve did not start; see {self.log_path}: {self.log_tail()}")

    def get(self, route: str, *, token: str | None = None) -> tuple[int | None, bytes]:
        return http_get(f"{self.url}{PLUGIN_ROUTE}/{route}", token if token is not None else self.token)

    def mounted(self) -> bool:
        """The serve process logged the mount of this plugin's router (``web_server_dashboard`` plugin mount)."""
        line = "Mounted plugin API routes: /api/plugins/ctx_handover/"
        logs = self.home.hermes_home / "logs"
        if not logs.is_dir():
            return False
        return any(line in p.read_text(errors="replace") for p in sorted(logs.glob("*.log")))

    def ran_job(self, job_id: str, role: Role) -> bool:
        """A participant of ``job_id`` was owned by the serve process (ledger ``owner`` events name its pid)."""
        prefix = f"{self.proc.pid}:"
        return any(
            str((e["data"] or {}).get("owner") or "").startswith(prefix)
            for e in self.home.ledger().events(job_id, role)
            if e["kind"] == "owner"
        )

    def log_tail(self, n: int = 4000) -> str:
        try:
            return self.log_path.read_bytes()[-n:].decode(errors="replace")
        except OSError:
            return ""

    def _signal(self, sig: int) -> None:
        with contextlib.suppress(ProcessLookupError, PermissionError):
            os.killpg(self.proc.pid, sig)

    def stop(self) -> None:
        if self.proc.poll() is None:
            self._signal(signal.SIGTERM)
            try:
                self.proc.wait(20)
            except subprocess.TimeoutExpired:
                self._signal(signal.SIGKILL)
                self.proc.wait(10)
        self._signal(signal.SIGKILL)  # leftovers of the process group
        if not self._log.closed:
            self._log.close()

    def kill(self) -> None:
        self._signal(signal.SIGKILL)
        with contextlib.suppress(subprocess.TimeoutExpired):
            self.proc.wait(10)


# ---------------------------------------------------------------------------------------------------------
# a conductor process that dies mid-transfer
# ---------------------------------------------------------------------------------------------------------


def run_child(home: Home, args: list[str], *, timeout: float = 300.0) -> subprocess.CompletedProcess:
    """``python -m ctx_handover.tests.integration._int2_helpers <args>`` with ``home``'s environment (incl. its
    gate secret) — a second process of the same profile, like a CLI run beside the test."""
    env = {k: v for k, v in os.environ.items() if not k.startswith(("HERMES_", "CTX_HANDOVER_", "GIT_"))}
    env.update(home.env())
    env["PYTHONPATH"] = os.pathsep.join(p for p in (UPSTREAM, str(PLUGIN_PARENT)) if p)
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return subprocess.run(  # noqa: S603 - fixed argv: this interpreter and a module of the test tree
        [sys.executable, "-m", "ctx_handover.tests.integration._int2_helpers", *args],
        env=env,
        cwd=str(home.root),
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def _push_and_die(cfg: dict) -> int:
    from ctx_handover.engine import jobs, remote
    from ctx_handover.engine.model import Mode, Options, SessionRef

    budget = {"left": int(cfg["die_after_batches"])}
    original = remote.RemoteBlocks.write_blocks

    def write_blocks(self, frames):
        result = original(self, frames)
        budget["left"] -= 1
        if budget["left"] <= 0:
            os.kill(os.getpid(), signal.SIGKILL)
        return result

    remote.RemoteBlocks.write_blocks = write_blocks
    runner = jobs.runner_for(None)
    peer = runner.ledger.peer(cfg["peer_id"])
    job_id, _final = runner.plan_sync(
        Mode.PUSH,
        peer,
        Options(refine="off", yes=True),
        session_ref=SessionRef(id=cfg["session_id"]),
        calling_session=None,
        session_key=None,
    )
    print(f"JOB {job_id}", flush=True)
    rc = runner.run_foreground(job_id)
    print(f"RC {rc}", flush=True)
    return 0


def main(argv: list[str]) -> int:
    if len(argv) == 2 and argv[0] == "push-and-die":
        return _push_and_die(json.loads(Path(argv[1]).read_text(encoding="utf-8")))
    print(f"usage: push-and-die <cfg.json> (got {argv!r})", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
