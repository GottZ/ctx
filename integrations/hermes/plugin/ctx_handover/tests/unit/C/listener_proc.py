"""A real HTTP listener process for the cluster C transport tests (never imported by runtime code).

``python -m ctx_handover.tests.unit.C.listener_proc <config.json>`` binds ``127.0.0.1:0``, prints
``PORT <n>`` and serves until terminated. The app imitates the parts of a Hermes dashboard the plugin meets:

* ``dashboard/plugin_api.py`` loaded **by path** the way Hermes loads it (``spec_from_file_location`` under the
  module name ``hermes_dashboard_plugin_ctx_handover``) and mounted at ``/api/plugins/ctx_handover``; its
  server factory builds ``server.Server`` from the path-loaded engine copy with the C ledger stub, a disk
  ``BlockStore`` and the participant double of ``helpers.FakeRegistry``;
* a dashboard gate in front of ``/api/plugins/``: ``X-Hermes-Session-Token``/``Authorization: Bearer`` with the
  configured token or a token issued by ``POST /auth/password-login`` (Basic provider); refusals are unsigned
  ``401 {"error": "session_expired", …}`` like ``hermes_cli/dashboard_auth/middleware.py``;
* a reverse proxy in front of everything: strips ``prefix``; refuses bodies above ``proxy_limit`` with an
  unsigned HTML 413; replays responses queued in ``<control>/inject.json`` (unsigned, for proxy-status tests);
  appends one line per request to ``<control>/requests.log``.

Config keys: ``plugin_dir``, ``env`` (identity seed), ``settings``, ``gate_token``, ``basic`` ({user, password}),
``extra_header`` ([name, value]), ``prefix``, ``proxy_limit``, ``control``.
"""

from __future__ import annotations

import importlib.util
import json
import secrets
import socket
import sys
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parents[3]


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

    from ctx_handover.tests.unit.C.helpers import FakeRegistry
    from ctx_handover.tests.unit.C.ledger_stub import LedgerStub

    control = Path(cfg["control"])
    control.mkdir(parents=True, exist_ok=True)
    tokens_file = control / "basic_tokens.json"
    logins_file = control / "logins.json"
    plugin_api = _load_plugin_api()

    def factory(eng):
        plugin_dir = Path(cfg["plugin_dir"])
        ledger = LedgerStub(plugin_dir / "ledger.sqlite3", model=eng.model)
        env = cfg.get("env") or {}
        identity = eng.crypto.Identity.load(ledger, read_secret=env.get, read_env_fresh=env.get)
        store = eng.blocks.BlockStore(plugin_dir, ledger)
        registry = FakeRegistry(ledger, eng=eng, store=store)
        auths = [eng.server.OperatorSessionAuth(enabled=False)]
        if identity is not None:
            auths.append(eng.server.PeerSignatureAuth(ledger, identity))
        return eng.server.Server(ledger, identity, registry, cfg.get("settings") or {}, auths)

    plugin_api.set_server_factory(factory)
    app = FastAPI()
    app.include_router(plugin_api.router, prefix="/api/plugins/ctx_handover")

    def _tokens() -> list[str]:
        try:
            return json.loads(tokens_file.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return []

    @app.post("/auth/password-login")
    async def password_login(payload: dict):
        count = json.loads(logins_file.read_text(encoding="utf-8")) if logins_file.exists() else 0
        logins_file.write_text(json.dumps(count + 1), encoding="utf-8")
        basic = cfg.get("basic") or {}
        if (control / "login_disabled").exists() or payload != {
            "provider": "basic",
            "username": basic.get("user"),
            "password": basic.get("password"),
        }:
            return JSONResponse({"error": "invalid_credentials"}, status_code=401)
        token = secrets.token_urlsafe(24)
        tokens_file.write_text(json.dumps([*_tokens(), token]), encoding="utf-8")
        resp = JSONResponse({"ok": True, "next": "/"})
        resp.set_cookie("hermes_session_at", token, httponly=True)
        resp.set_cookie("hermes_session_rt", secrets.token_urlsafe(8), httponly=True)
        return resp

    @app.get("/api/auth/providers")
    async def providers():
        names = [{"name": "basic", "supports_password": True}] if cfg.get("basic") else []
        return {"providers": names}

    async def gate(scope, receive, send, inner):
        headers = {k.decode().lower(): v.decode() for k, v in scope.get("headers", [])}
        token = headers.get("x-hermes-session-token") or ""
        auth = headers.get("authorization", "")
        if auth.lower().startswith("bearer "):
            token = token or auth[7:]
        ok = bool(token) and (token == cfg.get("gate_token") or token in _tokens())
        extra = cfg.get("extra_header")
        if extra and headers.get(extra[0].lower()) != extra[1]:
            ok = False
        if not ok:
            resp = JSONResponse(
                {"error": "session_expired", "detail": "Unauthorized", "reason": "invalid_or_expired_session"},
                status_code=401,
            )
            await resp(scope, receive, send)
            return
        await inner(scope, receive, send)

    async def proxy(scope, receive, send):
        if scope["type"] != "http":
            await app(scope, receive, send)
            return
        path = scope["path"]
        with (control / "requests.log").open("a", encoding="utf-8") as fh:
            fh.write(f"{scope['method']} {path}\n")
        prefix = cfg.get("prefix") or ""
        if prefix:
            if not path.startswith(prefix + "/"):
                await Response("not proxied", status_code=404)(scope, receive, send)
                return
            path = path[len(prefix) :]
            scope = dict(scope, path=path, raw_path=path.encode())
        inject = control / "inject.json"
        if inject.exists() and path.startswith("/api/plugins/"):
            queue = json.loads(inject.read_text(encoding="utf-8"))
            if queue:
                item = queue.pop(0)
                inject.write_text(json.dumps(queue), encoding="utf-8")
                body = item.get("body", "<html>proxy</html>").encode()
                await Response(body, status_code=item["status"], headers=item.get("headers", {}))(scope, receive, send)
                return
        limit = cfg.get("proxy_limit")
        if limit:
            headers = {k.decode().lower(): v.decode() for k, v in scope.get("headers", [])}
            if int(headers.get("content-length", "0") or 0) > int(limit):
                await Response(b"<html>413 Request Entity Too Large</html>", status_code=413)(scope, receive, send)
                return
        if path.startswith("/api/plugins/"):
            await gate(scope, receive, send, app)
            return
        await app(scope, receive, send)

    return proxy


def main(argv: list[str]) -> int:
    import uvicorn

    cfg = json.loads(Path(argv[0]).read_text(encoding="utf-8"))
    app = build_app(cfg)
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("127.0.0.1", 0))
    sock.listen(128)  # connections queue in the backlog until uvicorn accepts them
    print(f"PORT {sock.getsockname()[1]}", flush=True)
    server = uvicorn.Server(uvicorn.Config(app, log_level="warning", lifespan="off", access_log=False))
    server.run(sockets=[sock])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
