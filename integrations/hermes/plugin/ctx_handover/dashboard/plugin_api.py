"""ctx_handover listener routes ``/api/plugins/ctx_handover/v1/*`` (D4, R12; DESIGN §2.1, §4).

Hermes imports this file on its own (``hermes_cli.plugin_isolation.import_user_code`` in the gateway and in
``hermes serve``; ``plugin_host_child.op_asgi`` under ``plugins.isolation: host``) and mounts ``router`` behind
the dashboard gate and the profile scope. It therefore

* imports no Hermes module and nothing relative: the engine is loaded **by path** as its own package
  ``ctx_handover_engine_<sha8 of the engine dir>`` (every engine module imports its siblings relatively);
* assumes no shared memory with the plugin's ``ctx`` — the listener is built by the engine's composition root
  ``jobs.listener_server()`` (cluster A) from disk state only, once per profile home and process;
* does nothing at import time; the first request builds the server in a worker thread (context copied, so the
  profile-scoped lookups of the dashboard request resolve the right profile);
* passes the router's path parameter — never ``request.url.path`` — and the body as a stream, so the server
  signs the route-relative path and cuts oversized bodies while reading (SEC 1.4, I7).

When no server can be built (engine incomplete, no identity, unreadable ledger) every route answers exactly like
a missing plugin: ``404 {"detail": "Plugin not found"}`` (fail closed, SEC 1.10).
"""

from __future__ import annotations

import asyncio
import hashlib
import importlib.util
import logging
import sys
import threading
from pathlib import Path

from fastapi import APIRouter, Request
from starlette.responses import Response

log = logging.getLogger("ctx_handover.dashboard")

router = APIRouter()

_ENGINE_DIR = Path(__file__).resolve().parent.parent / "engine"
_PRELOAD = ("errors", "model", "contracts", "jsonutil", "ids", "paths", "hostcompat", "blocks", "crypto", "server")
_NOT_FOUND = b'{"detail":"Plugin not found"}'

_lock = threading.RLock()
_engine_module = None
_servers: dict[str, object] = {}


def _load_file(name: str, path: Path, *, package_dir: Path | None = None):
    existing = sys.modules.get(name)
    if existing is not None:
        return existing
    if package_dir is not None:
        spec = importlib.util.spec_from_file_location(name, path, submodule_search_locations=[str(package_dir)])
    else:
        spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise ImportError(f"cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(name, None)
        raise
    return module


def _submodule(eng, name: str):
    """Load ``engine/<name>.py`` as ``<engine package>.<name>`` and expose it as an attribute."""
    module = _load_file(f"{eng.__name__}.{name}", _ENGINE_DIR / f"{name}.py")
    setattr(eng, name, module)
    return module


def _engine():
    """The engine package loaded by path (cached per process)."""
    global _engine_module
    with _lock:
        if _engine_module is None:
            digest = hashlib.sha256(str(_ENGINE_DIR).encode("utf-8")).hexdigest()[:8]
            eng = _load_file(f"ctx_handover_engine_{digest}", _ENGINE_DIR / "__init__.py", package_dir=_ENGINE_DIR)
            for name in _PRELOAD:
                _submodule(eng, name)
            _engine_module = eng
        return _engine_module


def _default_factory(eng):
    """``jobs.listener_server()`` — the composition root of cluster A."""
    return _submodule(eng, "jobs").listener_server()


_factory = _default_factory


def set_server_factory(factory) -> None:
    """Replace how the server is built (``factory(engine_package) -> server.Server | None``); ``None`` disables
    the listener. Used by tests and lab harnesses; clears cached servers."""
    global _factory
    with _lock:
        _factory = factory if factory is not None else (lambda eng: None)
        _servers.clear()


def _scope_key(eng) -> str:
    """Profile home of the current request scope (one server per profile in a multiplexed process)."""
    try:
        return str(eng.hostcompat.state_db_path().parent)
    except Exception:
        return ""


def _server():
    eng = _engine()
    key = _scope_key(eng)
    with _lock:
        server = _servers.get(key)
        if server is not None:
            return server
        try:
            server = _factory(eng)
        except Exception as exc:
            log.warning("ctx_handover listener unavailable: %s", type(exc).__name__)
            return None
        if server is not None:
            _servers[key] = server
        return server


def _headers(request: Request) -> dict[str, str]:
    out: dict[str, str] = {}
    for key, value in request.headers.items():
        key = key.lower()
        if key in out and key.startswith("x-ctxh-"):
            out[key] = ""  # a repeated protocol header never passes the strict format checks
        else:
            out[key] = value
    return out


@router.api_route("/v1/{path:path}", methods=["GET", "POST", "PUT"], include_in_schema=False, response_model=None)
async def dispatch(path: str, request: Request) -> Response:
    try:
        server = await asyncio.to_thread(_server)
    except Exception as exc:
        log.warning("ctx_handover engine failed to load: %s", type(exc).__name__)
        server = None
    if server is None:
        return Response(_NOT_FOUND, status_code=404, media_type="application/json")
    client_ip = request.client.host if request.client else None
    resp = await server.handle_stream(request.method, path, _headers(request), request.stream(), client_ip)
    return Response(content=resp.body, status_code=resp.status, headers=resp.headers)
