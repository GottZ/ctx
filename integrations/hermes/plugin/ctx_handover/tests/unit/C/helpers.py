"""Shared helpers of the cluster C tests: child-process plumbing and a small participant registry double."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path

from ctx_handover.tests.conftest import PLUGIN_ROOT


def child_env(**extra: str) -> dict[str, str]:
    """Environment for a child Python: this test's isolated env plus a PYTHONPATH that reaches the plugin and
    the upstream checkout the parent imports Hermes from."""
    env = dict(os.environ)
    paths = [str(PLUGIN_ROOT.parent)]
    try:
        import hermes_state

        paths.append(str(Path(hermes_state.__file__).resolve().parent))
    except ImportError:
        pass
    old = env.get("PYTHONPATH", "")
    env["PYTHONPATH"] = os.pathsep.join([*paths, *([old] if old else [])])
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env.update(extra)
    return env


def run_barrier_children(argvs: list[list[str]], go: Path, *, env: dict[str, str], timeout: float = 90) -> list[dict]:
    """Start one child per argv, release them together through the barrier file, return their JSON lines."""
    procs = [
        subprocess.Popen(  # noqa: S603 - fixed interpreter, test-controlled argv
            [sys.executable, "-m", "ctx_handover.tests.unit.C.proc_helpers", *argv],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            text=True,
        )
        for argv in argvs
    ]
    try:
        time.sleep(1.5)  # imports done in every child
        go.write_text("go", encoding="utf-8")
        out = []
        for proc in procs:
            stdout, stderr = proc.communicate(timeout=timeout)
            assert proc.returncode == 0, stderr[-3000:]
            out.append(json.loads(stdout.strip().splitlines()[-1]))
        return out
    finally:
        for proc in procs:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=10)


# ---------------------------------------------------------------------------------------------------------
# Participant registry double (cluster A's ParticipantRegistry surface as the server calls it)
# ---------------------------------------------------------------------------------------------------------


class _Eng:
    """The engine modules a double needs; a path-loaded engine copy passes its own (``eng=``)."""

    def __init__(self, eng=None) -> None:
        if eng is None:
            from ctx_handover.engine import contracts, errors, model

            self.errors, self.model, self.contracts = errors, model, contracts
        else:
            self.errors, self.model, self.contracts = eng.errors, eng.model, eng.contracts


def sample_facts(m, name: str = "host") -> object:
    """A complete ``EnvFacts`` value for tests (``m`` = model module)."""
    host = m.HostFacts(
        is_wsl=False,
        is_container=False,
        profile="default",
        multiplex=False,
        hermes_version="0.22.6",
        plugin_version="0.1.0",
        model="test-model",
        provider="test-provider",
        context_length=None,
        terminal_backend="local",
        hermes_home="/h",
        document_cache="/h/cache/documents",
    )
    return m.EnvFacts(  # noqa: S604 - "shell" is an EnvFacts field, not subprocess(shell=…)
        instance_name=name,
        os_family="linux",
        os_name="Linux",
        os_version="6",
        arch="x86_64",
        hostname=name,
        user="u",
        home="/home/u",
        shell="/bin/sh",
        host=host,
        tools={},
        tools_seen={},
        workspace_root="/w",
        allow_roots=("/w",),
        disk_free={"/w": 1 << 30},
        case_insensitive={"/w": False},
        offline_capable=True,
        locale="C",
        timezone="UTC",
        git_version="2.45.0",
        git_lfs=False,
    )


class _NullReporter:
    def progress(self, p) -> None:
        pass

    def info(self, text: str) -> None:
        pass

    def warn(self, text: str) -> None:
        pass


class FakeParticipant:
    """Records every call; returns well-formed results of the contract types."""

    def __init__(self, registry: FakeRegistry, role) -> None:
        self.r = registry
        self.role = role
        self.calls: list[tuple[str, tuple]] = []

    def _op(self, name: str, result: dict | None = None):
        m = self.r.e.model
        return m.OpStatus(op_id=f"op-{name}", state=m.OpState.DONE, result=result or {"op": name})

    def _rec(self, name: str, *args):
        self.calls.append((name, args))
        exc = self.r.raise_on.get(name)
        if exc is not None:
            raise exc

    # shared
    def hello(self):
        raise AssertionError("the server answers hello itself")

    def status(self, job):
        self._rec("status", job)
        m = self.r.e.model
        return m.ParticipantStatus(
            state="open", step="", epoch=job.epoch, progress=None, ops={"plan": self._op("plan")}, last_error=None
        )

    def op_status(self, job, op_id):
        self._rec("op_status", job, op_id)
        return self._op("plan")

    def blocks(self):
        return self.r.store

    def facts(self, job, *rest):
        self._rec("facts", job, *rest)
        return sample_facts(self.r.e.model, f"{self.role}-host")

    # source
    def list_sessions(self, limit, query, include_archived):
        return []

    def start_plan(self, job, target_facts, session_ref):
        self._rec("start_plan", job, target_facts, session_ref)
        return self._op("plan")

    def lock(self, job, session_ref, plan_revision):
        self._rec("lock", job, session_ref, plan_revision)
        m = self.r.e.model
        return m.SessionInfo(
            tip_id="t",
            root_id="r",
            lineage_ids=("r", "t"),
            title="T",
            model=None,
            provider=None,
            cwd=None,
            git_repo_root=None,
            git_branch=None,
            active_messages=3,
            revision=(1, 2),
        )

    def heartbeat(self, job):
        self._rec("heartbeat", job)

    def start_export(self, job):
        self._rec("start_export", job)
        return self._op("export")

    def start_pack(self, job, final_plan):
        self._rec("start_pack", job, final_plan)
        return self._op("pack")

    def start_prepare(self, job, arg):
        self._rec("start_prepare", job, arg)
        return self._op("prepare")

    def start_retire(self, job, ack):
        self._rec("start_retire", job, ack)
        return self._op("retire", {"state": "retired"})

    def start_release(self, job, reason):
        self._rec("start_release", job, reason)
        return self._op("release", {"state": "released"})

    # target
    def open_job(self, job):
        self._rec("open_job", job)

    def negotiate(self, job, plan):
        self._rec("negotiate", job, plan)
        m = self.r.e.model
        return m.FinalPlan(plan_id="p" * 64, plan=plan, decisions=(), need_bytes=0, free_bytes={}, model_change=None)

    def start_stage_context(self, job, capsule):
        self._rec("start_stage_context", job, capsule)
        return self._op("stage_context")

    def start_apply_delta(self, job, delta):
        self._rec("start_apply_delta", job, delta)
        return self._op("apply_delta")

    def start_reset_session(self, job):
        self._rec("start_reset_session", job)
        return self._op("reset_session", {})

    def start_commit(self, job):
        self._rec("start_commit", job)
        return self._op("commit")

    def continue_session(self, job, prompt):
        self._rec("continue_session", job, prompt)
        return self.r.e.model.ContinueResult(started=True, pid=4242, log="logs/continue-x.log")

    def abort(self, job, reason):
        self._rec("abort", job, reason)


class FakeRegistry:
    """``ParticipantRegistry`` double: jobs keyed by (peer, job, role), role↔right binding, epoch fencing, and
    routes #8–#10 the way the registry serves them (contract X18). No job caps — those are the real registry's
    (``test_server.test_registry_owns_job_caps_and_routes``)."""

    def __init__(self, ledger, *, eng=None, store=None) -> None:
        from ctx_handover.tests.fakes.blocks_fake import MemoryBlockStore

        self.e = _Eng(eng)
        self.ledger = ledger
        self.store = store if store is not None else MemoryBlockStore()
        self.source = FakeParticipant(self, "source")
        self.target = FakeParticipant(self, "target")
        self.jobs: dict[tuple, dict] = {}
        self.aborted: list[tuple[tuple, str]] = []
        self.touched: list[str] = []
        self.raise_on: dict[str, BaseException] = {}
        self.contexts: list = []
        self.calls: list[tuple[str, str]] = []

    def _peer_id(self, auth) -> str | None:
        return auth.peer.peer_id if auth.peer is not None else None

    def open(self, auth, body: dict) -> dict:
        m, err = self.e.model, self.e.errors
        role = m.Role(body["role"])
        need = m.Right.PULL if role == m.Role.SOURCE else m.Right.PUSH
        if need not in auth.rights:
            raise err.Forbidden("forbidden")
        key = (self._peer_id(auth), body["job_id"], role)
        if key not in self.jobs:
            self.jobs[key] = {"mode": m.Mode(body["mode"]), "epoch": int(body["epoch"]), "body": body}
            self.ledger.create_job(body["job_id"], role, m.Mode(body["mode"]), self._peer_id(auth), {})
        return {"job_id": body["job_id"], "state": "open", "epoch": self.jobs[key]["epoch"]}

    def _job(self, auth, job_id: str, role) -> dict:
        job = self.jobs.get((self._peer_id(auth), job_id, role))
        if job is None:
            raise self.e.errors.NotFound("job not found")
        if auth.epoch is not None:
            if auth.epoch < job["epoch"]:
                raise self.e.errors.InvalidState("stale epoch", reason="stale_epoch", state="open")
            job["epoch"] = auth.epoch
        return job

    def get(self, auth, job_id: str, role):
        self._job(auth, job_id, role)
        return self.source if role == self.e.model.Role.SOURCE else self.target

    def context(self, auth, job_id: str, role):
        job = self._job(auth, job_id, role)
        ctx = self.e.contracts.JobContext(
            job_id=job_id,
            mode=job["mode"],
            peer=auth.peer,
            options=self.e.model.Options(),
            epoch=job["epoch"],
            reporter=_NullReporter(),
            cancelled=lambda: False,
            created_at=0.0,
        )
        self.contexts.append(ctx)
        return ctx

    def _find(self, auth, job_id: str):
        for role in (self.e.model.Role.SOURCE, self.e.model.Role.TARGET):
            if (self._peer_id(auth), job_id, role) in self.jobs:
                return role
        raise self.e.errors.NotFound("job not found")

    def status(self, auth, job_id: str, body: dict):
        self.calls.append(("status", job_id))
        role = self._find(auth, job_id)
        api = self.get(auth, job_id, role)
        return api.status(self.context(auth, job_id, role))

    def heartbeat(self, auth, job_id: str, body: dict) -> dict:
        self.calls.append(("heartbeat", job_id))
        role = self._find(auth, job_id)
        api, job = self.get(auth, job_id, role), self.context(auth, job_id, role)
        if role == self.e.model.Role.SOURCE:
            api.heartbeat(job)
        return {"state": api.status(job).state}

    def cancel(self, auth, job_id: str, body: dict) -> dict:
        self.calls.append(("cancel", job_id))
        role = self._find(auth, job_id)
        api, job = self.get(auth, job_id, role), self.context(auth, job_id, role)
        reason = "cancelled: " + str(body.get("reason") or "conductor")[:200]
        if role == self.e.model.Role.SOURCE:
            api.start_release(job, reason)
        else:
            api.abort(job, reason)
        return {"state": api.status(job).state}

    def list_sessions(self, auth, body: dict) -> dict:
        return {"sessions": [{"id": "s1", "title": "x"}], "limit": body.get("limit")}

    def offers(self, auth) -> dict:
        return {"offers": []}

    def claim(self, auth, offer_id: str, job_id: str) -> dict:
        return {"session_ref": {"offer_id": offer_id}, "job_id": job_id}

    def touch(self, job_id: str) -> None:
        self.touched.append(job_id)

    def abort_jobs(self, job_ids, reason: str) -> None:
        self.aborted.append((tuple(job_ids), reason))


# ---------------------------------------------------------------------------------------------------------
# Real HTTP listener process (listener_proc.py)
# ---------------------------------------------------------------------------------------------------------


class ListenerProcess:
    """A ``listener_proc`` child on a free loopback port; stopped by PID (terminate, then kill)."""

    def __init__(
        self,
        base_dir: Path,
        *,
        gate_token: str = "gate-tok",  # noqa: S107 - test fixture token
        settings: dict | None = None,
        basic: dict | None = None,
        prefix: str = "",
        proxy_limit: int | None = None,
        extra_header: list[str] | None = None,
    ) -> None:
        from ctx_handover.tests.unit.C.conftest import new_identity
        from ctx_handover.tests.unit.C.ledger_stub import make_ledger

        self.dir = Path(base_dir)
        self.plugin_dir = self.dir / "plugin-data"
        self.plugin_dir.mkdir(parents=True, exist_ok=True)
        os.chmod(self.plugin_dir, 0o700)
        self.control = self.dir / "control"
        self.control.mkdir(parents=True, exist_ok=True)
        self.env: dict[str, str] = {}
        self.ledger = make_ledger(self.plugin_dir / "ledger.sqlite3")
        self.identity = new_identity(self.ledger, self.env)
        self.prefix = prefix
        self.cfg = {
            "plugin_dir": str(self.plugin_dir),
            "env": self.env,
            "settings": {"name": "server", **(settings or {})},
            "gate_token": gate_token,
            "basic": basic,
            "extra_header": extra_header,
            "prefix": prefix,
            "proxy_limit": proxy_limit,
            "control": str(self.control),
        }
        self.proc: subprocess.Popen | None = None
        self.port = 0

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.port}{self.prefix}"

    def start(self) -> ListenerProcess:
        import selectors

        cfg_path = self.dir / "config.json"
        fd = os.open(cfg_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(self.cfg, fh)
        self._log = (self.dir / "listener.log").open("w", encoding="utf-8")
        self.proc = subprocess.Popen(  # noqa: S603 - fixed interpreter, test-controlled argv
            [sys.executable, "-m", "ctx_handover.tests.unit.C.listener_proc", str(cfg_path)],
            stdout=subprocess.PIPE,
            stderr=self._log,
            env=child_env(),
            text=True,
        )
        sel = selectors.DefaultSelector()
        sel.register(self.proc.stdout, selectors.EVENT_READ)
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            if sel.select(timeout=0.5):
                line = self.proc.stdout.readline()
                if line.startswith("PORT "):
                    self.port = int(line.split()[1])
                    break
                if not line:
                    break
            if self.proc.poll() is not None:
                break
        sel.close()
        if not self.port:
            self.stop()
            raise AssertionError("listener did not start: " + (self.dir / "listener.log").read_text()[-3000:])
        return self

    def stop(self) -> None:
        if self.proc is None:
            return
        pid = self.proc.pid
        if self.proc.poll() is None:
            os.kill(pid, 15)
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.kill(pid, 9)
                self.proc.wait(timeout=10)
        self.proc.stdout.close()
        self._log.close()
        self.proc = None

    def inject(self, *items: dict) -> None:
        (self.control / "inject.json").write_text(json.dumps(list(items)), encoding="utf-8")

    def requests(self) -> list[str]:
        log = self.control / "requests.log"
        return log.read_text(encoding="utf-8").splitlines() if log.exists() else []

    def logins(self) -> int:
        path = self.control / "logins.json"
        return json.loads(path.read_text(encoding="utf-8")) if path.exists() else 0

    def expire_tokens(self) -> None:
        (self.control / "basic_tokens.json").write_text("[]", encoding="utf-8")
