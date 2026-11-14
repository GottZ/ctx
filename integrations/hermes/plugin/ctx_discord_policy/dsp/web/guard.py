"""Request guard of the settings page backend (SETTINGS-PAGE §2.3 F1–F4, F7).

Order for every request, before any handler runs:

1. **F2 admission** — Hermes' own ``admitted_for_sensitive_route`` (the same decision as the dashboard gate;
   in a plugin-host child, where the router runs without middleware, the only one). Missing or failing
   check ⇒ 503, never open.
2. **F3 methods** — reads are GET/HEAD and have no side effects; writes are POST/PUT/PATCH/DELETE and must
   carry ``Content-Type: application/json`` (no HTML form can send that) and pass the origin check: Hermes'
   ``cookie_origin_is_allowed`` where it exists (v0.23.0+), :func:`fallback_origin_ok` on older Hermes.
3. **F4 size** — bodies are read with a cap (``BODY_LIMIT``, imports ``IMPORT_LIMIT``) by the HTTP layer.

:func:`host_actor` is the only place a host actor is built (F7): Discord code never imports this module.
"""

from __future__ import annotations

from typing import Any, Callable, Mapping, Optional
from urllib.parse import urlsplit

from ..ui import perms
from .responses import WebResponse, error

READ_METHODS = frozenset({"GET", "HEAD"})
WRITE_METHODS = frozenset({"POST", "PUT", "PATCH", "DELETE"})
BODY_LIMIT = 256 * 1024
IMPORT_LIMIT = 1024 * 1024


def host_actor() -> perms.Actor:
    """The settings page's actor: role host, recorded as ``dashboard`` (never a person's login, F10)."""
    return perms.Actor("dashboard", perms.HOST)


def precheck(method: str, headers: Mapping[str, str], *, admitted: Callable[[], Optional[bool]],
             origin_ok: Callable[[], Optional[bool]]) -> Optional[WebResponse]:
    """``None`` when the request may reach a handler, else the refusal. ``headers`` keys are lower case."""
    try:
        verdict = admitted()
    except Exception:
        verdict = None
    if verdict is None:
        return error(503, "guard_unavailable", "This Hermes cannot confirm that the request is signed in, so the "
                     "settings page stays closed. Next step: update Hermes.")
    if verdict is not True:
        return error(401, "unauthorized", "Sign in to Hermes first.")
    method = method.upper()
    if method in READ_METHODS:
        return None
    if method not in WRITE_METHODS:
        return error(405, "method", f"{method} is not supported here.")
    content_type = str(headers.get("content-type", "")).split(";", 1)[0].strip().lower()
    if content_type != "application/json":
        return error(415, "json_only", "Changes must be sent as JSON.")
    try:
        allowed = origin_ok()
    except Exception:
        allowed = False
    if allowed is not True:
        return error(403, "origin", "This change did not come from the Hermes app itself and was refused.")
    return None


def _origin(value: str) -> Optional[tuple]:
    try:
        parts = urlsplit(value.strip())
    except ValueError:
        return None
    if parts.scheme not in ("http", "https") or not parts.hostname or parts.username is not None \
            or parts.password is not None or parts.path not in ("", "/") or parts.query or parts.fragment:
        return None
    try:
        port = parts.port or (443 if parts.scheme == "https" else 80)
    except ValueError:
        return None
    return parts.scheme, parts.hostname.lower(), port


def fallback_origin_ok(headers: Mapping[str, str], *, host: str, scheme: str, public_url: str) -> bool:
    """Origin check for Hermes versions without ``cookie_origin_is_allowed`` (v0.22.4), same rules: no
    ``Origin`` passes only without Fetch Metadata or with ``Sec-Fetch-Site: none`` (native clients); with an
    ``Origin`` the browser's ``Sec-Fetch-Site`` decides when present, else the origin must equal the public
    URL's origin or this request's own (``scheme://host``)."""
    origin = str(headers.get("origin", "")).strip()
    fetch_site = str(headers.get("sec-fetch-site", "")).strip().lower()
    if not origin:
        return fetch_site in ("", "none")
    parsed = _origin(origin)
    if parsed is None:
        return False
    if fetch_site:
        return fetch_site == "same-origin"
    target = _site_of(public_url) if public_url else _origin(f"{scheme}://{host}")
    return target is not None and parsed == target


def _site_of(public_url: str) -> Optional[tuple]:
    """The origin of Hermes' public URL; like Hermes' own check, only scheme and host count, not its path."""
    try:
        parts = urlsplit(public_url.strip())
    except ValueError:
        return None
    return _origin(f"{parts.scheme}://{parts.netloc}") if parts.scheme and parts.netloc else None


def request_admission(request: Any, hermes: Any) -> Callable[[], Optional[bool]]:
    return lambda: hermes.admitted(request)


def request_origin(request: Any, hermes: Any) -> Callable[[], Optional[bool]]:
    """Hermes' origin check, or the fallback where this Hermes has none (or it cannot run, e.g. without the
    dashboard's app state in a plugin-host child)."""
    def check() -> Optional[bool]:
        try:
            verdict = hermes.origin_allowed(request)
        except Exception:
            verdict = None
        if verdict is not None:
            return verdict
        headers = {k.lower(): v for k, v in request.headers.items()}
        state = getattr(getattr(request, "app", None), "state", None)
        return fallback_origin_ok(headers, host=headers.get("host", ""), scheme=request.url.scheme,
                                  public_url=hermes.public_url(state))
    return check
