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
"""

from __future__ import annotations

import contextlib
import json
import os
import secrets
import subprocess
import sys
import time
from collections.abc import Iterator
from pathlib import Path

from ctx_handover.engine import crypto, hostcompat, jobs, pairing, wire
from ctx_handover.engine.model import GateAuth, GateMode, Mode, Options, Peer, Right, Role, SessionRef

PLUGIN_PARENT = Path(__file__).resolve().parents[3]
UPSTREAM = os.environ.get("CTXH_UPSTREAM") or next(
    (p for p in sys.path if Path(p, "hermes_state.py").exists()), ""
)
GATE_ENV = "CTX_HANDOVER_PEER_TOKEN"


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
            return jobs._profile().ledger  # noqa: SLF001 - the profile's ledger as the plugin uses it

    def data_dir(self) -> Path:
        with self.active():
            return jobs._profile().data_dir  # noqa: SLF001

    def identity(self):
        """This home's identity (created like an operator verb does, R16)."""
        with self.active():
            ledger = jobs._profile().ledger  # noqa: SLF001
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

    def __init__(self, home: Home, *, proxy_limit: int | None = None) -> None:
        self.home = home
        self.token = secrets.token_urlsafe(16)
        self.control = home.root / "listener"
        self.control.mkdir(parents=True, exist_ok=True)
        cfg = {"gate_token": self.token, "control": str(self.control)}
        if proxy_limit:
            cfg["proxy_limit"] = proxy_limit
        cfg_path = self.control / "config.json"
        cfg_path.write_text(json.dumps(cfg), encoding="utf-8")
        env = {k: v for k, v in os.environ.items() if not k.startswith(("HERMES_", "CTX_HANDOVER_", "GIT_"))}
        env.update(home.env())
        env["PYTHONPATH"] = os.pathsep.join(p for p in (UPSTREAM, str(PLUGIN_PARENT)) if p)
        env["PYTHONDONTWRITEBYTECODE"] = "1"
        self._log = (self.control / "listener.log").open("ab")
        self.proc = subprocess.Popen(  # noqa: S603 - fixed argv: this interpreter and a module of the test tree
            [sys.executable, "-m", "ctx_handover.tests.integration.listener_app", str(cfg_path)],
            stdout=subprocess.PIPE,
            stderr=self._log,
            env=env,
            cwd=str(home.root),
        )
        line = self.proc.stdout.readline().decode().strip()
        if not line.startswith("PORT "):
            self.stop()
            raise RuntimeError(f"listener did not start: {line!r}; see {self.control / 'listener.log'}")
        self.port = int(line.split()[1])
        self.url = f"http://127.0.0.1:{self.port}"

    def stop(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(10)
        self._log.close()

    def kill(self) -> None:
        """Hard stop (process death without cleanup)."""
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait(10)

    def requests(self) -> list[str]:
        path = self.control / "requests.log"
        return path.read_text(encoding="utf-8").splitlines() if path.exists() else []


def pair(server: Home, laptop: Home, listener: Listener, *, rights=(Right.PUSH, Right.PULL)) -> Peer:
    """Pair ``laptop`` (dialer) with ``server`` (listener) the way ``/handover pair`` does on both sides."""
    ident_a = server.identity()
    with server.active():
        code = pairing.create_code(jobs._profile().ledger, rights=frozenset(rights), name_hint="laptop", ttl_s=600)  # noqa: SLF001
        text = pairing.pair_string(code, url=listener.url, fingerprint=ident_a.fingerprint(), name="server",
                                   profile=None)
    ident_b = laptop.identity()
    laptop.extra_env[GATE_ENV] = listener.token
    with laptop.active():
        ledger = jobs._profile().ledger  # noqa: SLF001
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
    return session_export._hashes_now(db, tip, lineage)  # noqa: SLF001 - the hashes the handover verifies


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
