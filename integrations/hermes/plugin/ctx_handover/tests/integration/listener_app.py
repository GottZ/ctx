"""The listener process of the integration tests: a real HTTP server on loopback with the plugin router.

``python -m ctx_handover.tests.integration.listener_app <config.json>`` runs with the environment of home A
(``HOME``, ``HERMES_HOME``) and serves ``dashboard/plugin_api.py`` exactly the way Hermes loads it — by path,
mounted at ``/api/plugins/ctx_handover`` — behind a dashboard gate that accepts one fixed session token
(``X-Hermes-Session-Token``, like ``hermes serve`` with ``HERMES_DASHBOARD_SESSION_TOKEN``). The plugin builds
its server through its own composition root ``jobs.listener_server()``: real ledger, block store, identity,
participants and Hermes ``SessionDB`` of home A. Prints ``PORT <n>`` once bound.

Config keys: ``gate_token``, ``control`` (directory for ``requests.log``), optional ``proxy_limit``.
"""

from __future__ import annotations

import importlib.util
import json
import socket
import sys
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parents[2]


def _load_plugin_api():
    path = PLUGIN_ROOT / "dashboard" / "plugin_api.py"
    spec = importlib.util.spec_from_file_location("hermes_dashboard_plugin_ctx_handover", path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def build_app(cfg: dict):
    from fastapi import FastAPI
    from starlette.responses import JSONResponse, Response

    control = Path(cfg["control"])
    control.mkdir(parents=True, exist_ok=True)
    plugin_api = _load_plugin_api()
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


def main(argv: list[str]) -> int:
    import uvicorn

    cfg = json.loads(Path(argv[0]).read_text(encoding="utf-8"))
    app = build_app(cfg)
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("127.0.0.1", 0))
    sock.listen(128)
    print(f"PORT {sock.getsockname()[1]}", flush=True)
    server = uvicorn.Server(uvicorn.Config(app, log_level="warning", lifespan="off", access_log=False))
    server.run(sockets=[sock])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
