"""HTTP backend of the plugin's settings page (Hermes Desktop: Settings → Plugins → ctx_discord_policy).

Hermes imports this file as ``hermes_dashboard_plugin_ctx_discord_policy`` in every process that mounts
plugin APIs (the gateway's listener, a standalone ``hermes dashboard`` or ``hermes serve``, a plugin-host
child) — only while the plugin is in ``plugins.enabled`` — and mounts ``router`` under
``/api/plugins/ctx_discord_policy/`` behind its dashboard auth gate and profile scope.

This file stays thin: it loads the plugin's process-wide runtime (``dsp/``, the same anchor the gateway's
plugin instances share; loaded from this package's files when the process has none) and hands every request
to ``dsp.web``: the guard first (admission, JSON-only writes, origin, body size), then the framework-free
routes. Nothing happens at import time except building the router. Nothing worth protecting lives in this
directory: Hermes serves its static files without sign-in.
"""

import contextvars
import importlib
import importlib.util
import json
import sys
from pathlib import Path

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse, Response
from starlette.concurrency import run_in_threadpool

PLUGIN_DIR = Path(__file__).resolve().parent.parent
ENTRY_NAME = "ctx_discord_policy__dashboard_entry"
HEADERS = {"Cache-Control": "no-store"}

router = APIRouter()


def _entry():
    module = sys.modules.get(ENTRY_NAME)
    if module is not None:
        return module
    spec = importlib.util.spec_from_file_location(ENTRY_NAME, PLUGIN_DIR / "__init__.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[ENTRY_NAME] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(ENTRY_NAME, None)
        raise
    return module


def _runtime():
    """``(anchor, conflict)``: the process-wide runtime and why it cannot serve (another version) or None."""
    entry = _entry()
    anchor = entry.load_anchor(PLUGIN_DIR)
    return anchor, entry.anchor_conflict(anchor)


def _error(status, code, message):
    return JSONResponse({"error": {"code": code, "message": message}}, status_code=status, headers=HEADERS)


async def _read_body(request, limit):
    """The request body, or None when it exceeds ``limit`` bytes (read in chunks, never all at once)."""
    chunks, size = [], 0
    async for chunk in request.stream():
        size += len(chunk)
        if size > limit:
            return None
        chunks.append(chunk)
    return b"".join(chunks)


def _conflict_answer(request, path, conflict):
    """Another plugin version runs in this process (files replaced without a restart): its modules may not
    even have this backend, so nothing of it is used. Signed-in callers learn why; everything else is closed."""
    try:
        from hermes_cli.plugin_host_http_auth import admitted_for_sensitive_route

        admitted = admitted_for_sensitive_route(request)
    except Exception:
        admitted = False
    if not admitted:
        return _error(401, "unauthorized", "Sign in to Hermes first.")
    if request.method.upper() == "GET" and path.strip("/") == "meta":
        return JSONResponse({"mode": "read-only", "conflict": conflict, "api": 1}, headers=HEADERS)
    return _error(503, "restart_required", "This Hermes process runs another version of the plugin. "
                                           "Restart Hermes to use the settings page.")


@router.api_route("/v1/{path:path}", methods=["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"])
async def settings_api(request: Request, path: str):
    try:
        anchor, conflict = _runtime()
    except Exception:
        return _error(503, "unavailable", "The plugin's runtime could not be loaded in this Hermes process. "
                                          "Details are in the Hermes log.")
    if conflict:
        return _conflict_answer(request, path, conflict)
    name = anchor.__name__
    hermes = importlib.import_module(f"{name}.hermes")
    guard = importlib.import_module(f"{name}.web.guard")
    routes = importlib.import_module(f"{name}.web.routes")

    method = request.method.upper()
    headers = {k.lower(): v for k, v in request.headers.items()}
    refused = guard.precheck(method, headers, admitted=guard.request_admission(request, hermes),
                             origin_ok=guard.request_origin(request, hermes))
    if refused is not None:
        return JSONResponse(refused.body, status_code=refused.status, headers=HEADERS)

    body = None
    if method in guard.WRITE_METHODS:
        limit = guard.IMPORT_LIMIT if path.rstrip("/").endswith("/import") else guard.BODY_LIMIT
        raw = await _read_body(request, limit)
        if raw is None:
            return _error(413, "too_large", f"The request is larger than {limit // 1024} KB.")
        if raw.strip():
            try:
                body = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, ValueError, RecursionError):
                return _error(400, "bad_request", "The request body is not valid JSON.")

    process, surface = hermes.serving_process(request.app.state)
    req = routes.WebRequest(method=method, path=path, query=dict(request.query_params), body=body,
                            home=hermes.current_home(), plugin_dir=PLUGIN_DIR, process=process, surface=surface)
    # The handler may wait on the store's file lock: run it off the event loop, in this request's context
    # (profile home and secret scope travel with it).
    context = contextvars.copy_context()
    result = await run_in_threadpool(context.run, routes.handle, req)
    if method == "HEAD":
        return Response(status_code=result.status, headers=HEADERS)
    return JSONResponse(result.body, status_code=result.status, headers={**HEADERS, **result.headers})
