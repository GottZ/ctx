"""The listener process of the integration tests: a real HTTP server on loopback with the plugin router.

``python -m ctx_handover.tests.integration.listener_app <config.json>`` runs with the environment of home A
(``HOME``, ``HERMES_HOME``) and serves ``dashboard/plugin_api.py`` exactly the way Hermes loads it — by path,
mounted at ``/api/plugins/ctx_handover`` — behind a dashboard gate that accepts one fixed session token
(``X-Hermes-Session-Token``, like ``hermes serve`` with ``HERMES_DASHBOARD_SESSION_TOKEN``). The plugin builds
its server through its own composition root ``jobs.listener_server()``: real ledger, block store, identity,
participants and Hermes ``SessionDB`` of home A. Prints ``PORT <n>`` once bound.

Config keys: ``gate_token``, ``control`` (directory for ``requests.log``), optional ``proxy_limit``, optional
``port`` (bind exactly this port — a restart keeps the address the peer row points at), optional ``faults``.

With ``faults: true`` the server is still built by ``jobs.listener_server()`` (through ``plugin_api``'s documented
``set_server_factory`` seam), and two places of the engine package the router loaded are instrumented with the
rules of ``<control>/faults.json`` (written by ``harness.Listener.arm``; format ``harness.rules_for``):

* ``Server._dispatch`` — a rule ``{"kind": "error"|"interrupt", "handler": <server handler>, "label": <call>}``
  matches a request of that handler whose ``X-Test-Call`` header (set by the conductor's test transport) names the
  call: ``error`` raises ``ImportFailed`` before the handler runs (the participant's error envelope), ``interrupt``
  lets the handler run and then SIGKILLs this process (effect done, response lost).
* ``OpRunner.start`` — a rule ``{"kind": …, "op": <operation>}`` wraps the operation's function: ``error`` makes the
  operation fail with ``ImportFailed``, ``interrupt`` SIGKILLs this process inside the running operation.

Every rule fires ``times`` times (default once) — counted in ``<control>/fired.jsonl``, so a restarted process
does not fire it again.
"""

from __future__ import annotations

import errno
import importlib.util
import json
import os
import signal
import socket
import sys
import threading
import time
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parents[2]


def _load_plugin_api():
    path = PLUGIN_ROOT / "dashboard" / "plugin_api.py"
    spec = importlib.util.spec_from_file_location("hermes_dashboard_plugin_ctx_handover", path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class _Faults:
    """The listener-side fault rules (see module docstring)."""

    def __init__(self, control: Path) -> None:
        self.rules_path = control / "faults.json"
        self.fired_path = control / "fired.jsonl"
        self._key: tuple[int, int] | None = None
        self._rules: list[dict] = []
        self._lock = threading.Lock()

    def rules(self) -> list[dict]:
        with self._lock:
            try:
                st = self.rules_path.stat()
            except FileNotFoundError:
                return []
            key = (st.st_mtime_ns, st.st_size)
            if key != self._key:
                try:
                    self._rules = [dict(r) for r in json.loads(self.rules_path.read_text(encoding="utf-8")) or ()]
                except (OSError, ValueError):
                    self._rules = []
                self._key = key
            return list(self._rules)

    def take(self, rule: dict) -> bool:
        with self._lock:
            fired = 0
            try:
                for line in self.fired_path.read_text(encoding="utf-8").splitlines():
                    if line.strip() and json.loads(line).get("id") == rule["id"]:
                        fired += 1
            except FileNotFoundError:
                pass
            if fired >= int(rule.get("times", 1)):
                return False
            with self.fired_path.open("a", encoding="utf-8") as fh:
                entry = {"id": rule["id"], "where": "listener", "pid": os.getpid(), "at": time.time()}
                fh.write(json.dumps(entry) + "\n")
            return True

    @staticmethod
    def die() -> None:
        os.kill(os.getpid(), signal.SIGKILL)
        time.sleep(60)  # never reached


def _install_faults(plugin_api, control: Path) -> None:
    faults = _Faults(control)
    patched: dict[str, bool] = {}

    def factory(eng):
        jobs = plugin_api._submodule(eng, "jobs")
        errors = eng.errors
        if not patched.get("ops"):
            _patch_ops(sys.modules[f"{eng.__name__}.ops"].OpRunner, errors, faults)
            patched["ops"] = True
        server = jobs.listener_server()
        if server is not None and not getattr(server, "_ctxh_test_faults", False):
            _patch_dispatch(server, errors, faults)
        return server

    plugin_api.set_server_factory(factory)


def _patch_ops(op_runner_cls, errors, faults: _Faults) -> None:
    original = op_runner_cls.start

    def start(runner, job_id, op, digest, fn, **kw):
        rules = [r for r in faults.rules() if r.get("op") == op and r.get("kind") in ("error", "interrupt")]
        if rules:
            fn = _faulty(fn, rules, errors, faults)
        return original(runner, job_id, op, digest, fn, **kw)

    op_runner_cls.start = start


def _faulty(fn, rules: list[dict], errors, faults: _Faults):
    def run(report):
        for rule in rules:
            if faults.take(rule):
                if rule["kind"] == "interrupt":
                    faults.die()
                raise errors.ImportFailed("injected failure", hermes_error="Injected")
        return fn(report)

    return run


def _patch_dispatch(server, errors, faults: _Faults) -> None:
    original = server._dispatch

    def dispatch(handler, auth, params, req):
        labels = {x for x in str(req.headers.get("x-test-call") or "").split(",") if x}
        rules = [
            r
            for r in faults.rules()
            if r.get("handler") == handler and r.get("label") in labels and r.get("kind") in ("error", "interrupt")
        ]
        for rule in rules:
            if rule["kind"] == "error" and faults.take(rule):
                raise errors.ImportFailed("injected failure", hermes_error="Injected")
        result = original(handler, auth, params, req)
        for rule in rules:
            if rule["kind"] == "interrupt" and faults.take(rule):
                faults.die()
        return result

    server._dispatch = dispatch
    server._ctxh_test_faults = True


def build_app(cfg: dict):
    from fastapi import FastAPI
    from starlette.responses import JSONResponse, Response

    control = Path(cfg["control"])
    control.mkdir(parents=True, exist_ok=True)
    plugin_api = _load_plugin_api()
    if cfg.get("faults"):
        _install_faults(plugin_api, control)
    app = FastAPI()
    app.include_router(plugin_api.router, prefix="/api/plugins/ctx_handover")

    async def gate(scope, receive, send):
        headers = {k.decode().lower(): v.decode() for k, v in scope.get("headers", [])}
        if headers.get("x-hermes-session-token") != cfg["gate_token"]:
            resp = JSONResponse({"error": "session_expired", "detail": "Unauthorized"}, status_code=401)
            await resp(scope, receive, send)
            return
        await app(scope, receive, send)

    async def front(scope, receive, send):
        if scope["type"] != "http":
            await app(scope, receive, send)
            return
        with (control / "requests.log").open("a", encoding="utf-8") as fh:
            fh.write(f"{scope['method']} {scope['path']}\n")
        limit = cfg.get("proxy_limit")
        if limit:
            headers = {k.decode().lower(): v.decode() for k, v in scope.get("headers", [])}
            if int(headers.get("content-length", "0") or 0) > int(limit):
                await Response(b"<html>413</html>", status_code=413)(scope, receive, send)
                return
        if scope["path"].startswith("/api/plugins/"):
            await gate(scope, receive, send)
            return
        await app(scope, receive, send)

    return front


def _bind(port: int) -> socket.socket:
    deadline = time.monotonic() + 15.0
    while True:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            sock.bind(("127.0.0.1", port))
            return sock
        except OSError as exc:
            sock.close()
            if exc.errno != errno.EADDRINUSE or not port or time.monotonic() > deadline:
                raise
            time.sleep(0.1)


def main(argv: list[str]) -> int:
    import uvicorn

    cfg = json.loads(Path(argv[0]).read_text(encoding="utf-8"))
    app = build_app(cfg)
    sock = _bind(int(cfg.get("port") or 0))
    sock.listen(128)
    print(f"PORT {sock.getsockname()[1]}", flush=True)
    server = uvicorn.Server(uvicorn.Config(app, log_level="warning", lifespan="off", access_log=False))
    server.run(sockets=[sock])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
