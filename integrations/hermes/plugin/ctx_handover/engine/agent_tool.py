"""The agent tool ``handover`` (layer L11; DESIGN §14.3, §8.4, SEC 5.6).

The model can look at what would move (``plan``), name resources a file scan cannot see (``suggest`` — paths,
tool names and the NAMES of environment variables, never values), ask for a handover after the current turn
(``request``) and read ``status``/``peers``. Everything it says is advisory: suggestions outside the project are
only *proposed*, the policy decides, and a ``request`` starts a handover only with the operator setting
``allow_agent_start`` — then non-interactively, without proposed items, stopping at any other confirmation.

``agent_tool`` and ``commands`` sit on the same layer; ``../__init__.py`` hands this module the plugin object
(``provider``), which supplies the backend and the request starter.
"""

from __future__ import annotations

import json
import logging
import re
from collections.abc import Callable
from typing import Any

from .model import Mode, SessionRef
from .progress import NullReporter
from .surfaces import LOCAL_PLATFORMS, human_bytes, plan_summary, settings_from_config

logger = logging.getLogger(__name__)

TOOL_SCHEMA: dict = {
    "name": "handover",
    "description": (
        "Hand this session and its working files to another Hermes gateway the user paired (see /handover). "
        "action=plan shows what would move; action=suggest records resources the continued work needs that a file "
        "scan cannot see (paths outside the project, other repos, tools, NAMES of environment variables — never "
        "secrets); action=request asks to hand over to a peer after this turn ends (only when the user asked for "
        "it); action=status and action=peers are read-only."
    ),
    "parameters": {
        "type": "object",
        "additionalProperties": False,
        "required": ["action"],
        "properties": {
            "action": {"type": "string", "enum": ["plan", "suggest", "request", "status", "peers"]},
            "peer": {"type": "string", "maxLength": 64},
            "resources": {
                "type": "array",
                "maxItems": 64,
                "items": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["path"],
                    "properties": {
                        "path": {"type": "string", "maxLength": 4096},
                        "reason": {"type": "string", "maxLength": 300},
                    },
                },
            },
            "exclude": {"type": "array", "maxItems": 64, "items": {"type": "string", "maxLength": 4096}},
            "tools_needed": {"type": "array", "maxItems": 32, "items": {"type": "string", "maxLength": 64}},
            "env_vars_needed": {"type": "array", "maxItems": 32, "items": {"type": "string", "maxLength": 128}},
            "note": {"type": "string", "maxLength": 2000},
        },
    },
}
"""DESIGN §14.3, verbatim."""

_TOOL_RE = re.compile(r"^[A-Za-z0-9._+-]{1,64}$")
_ENV_RE = re.compile(r"^[A-Z_][A-Z0-9_]{0,127}$")
_PEER_RE = re.compile(r"^[A-Za-z0-9_.@-]{1,64}$")
_CONTROL_RE = re.compile(r"[\x00-\x08\x0b-\x1f\x7f]")
_KEYS = set(TOOL_SCHEMA["parameters"]["properties"])


class ToolArgError(ValueError):
    """Arguments outside ``TOOL_SCHEMA`` (the plugin validates itself; no ``jsonschema`` in the runtime)."""


def _strings(value: Any, key: str, *, max_items: int, max_len: int, pattern: re.Pattern | None = None) -> list[str]:
    if value is None:
        return []
    if not isinstance(value, list) or len(value) > max_items:
        raise ToolArgError(f"{key} must be a list of at most {max_items} strings")
    out = []
    for item in value:
        if not isinstance(item, str) or not item or len(item) > max_len or _CONTROL_RE.search(item):
            raise ToolArgError(f"{key} entries must be non-empty strings of at most {max_len} characters")
        if pattern is not None and not pattern.fullmatch(item):
            raise ToolArgError(f"{key} entry {item[:64]!r} is not allowed (names only, no values)")
        out.append(item)
    return out


def validate(args: Any) -> dict:
    """Arguments checked against ``TOOL_SCHEMA`` (types, limits, enums, no unknown keys) plus the content rules:
    tool names and environment-variable NAMES only."""
    if not isinstance(args, dict):
        raise ToolArgError("arguments must be an object")
    unknown = set(args) - _KEYS
    if unknown:
        raise ToolArgError(f"unknown argument(s): {', '.join(sorted(unknown))}")
    action = args.get("action")
    if action not in TOOL_SCHEMA["parameters"]["properties"]["action"]["enum"]:
        raise ToolArgError("action must be one of plan, suggest, request, status, peers")
    peer = args.get("peer")
    if peer is not None and (not isinstance(peer, str) or not _PEER_RE.fullmatch(peer)):
        raise ToolArgError("peer must be the name of a paired device")
    resources = args.get("resources") or []
    if not isinstance(resources, list) or len(resources) > 64:
        raise ToolArgError("resources must be a list of at most 64 objects")
    clean_resources = []
    for res in resources:
        if not isinstance(res, dict) or set(res) - {"path", "reason"} or not isinstance(res.get("path"), str):
            raise ToolArgError("each resource is {path, reason?}")
        path, reason = res["path"], res.get("reason", "")
        if not path or len(path) > 4096 or _CONTROL_RE.search(path):
            raise ToolArgError("resource paths are non-empty strings of at most 4096 characters")
        if not isinstance(reason, str) or len(reason) > 300:
            raise ToolArgError("a resource reason is a string of at most 300 characters")
        clean_resources.append({"path": path, "reason": _CONTROL_RE.sub("", reason)})
    note = args.get("note", "")
    if not isinstance(note, str) or len(note) > 2000:
        raise ToolArgError("note is a string of at most 2000 characters")
    return {
        "action": action,
        "peer": peer,
        "resources": clean_resources,
        "exclude": _strings(args.get("exclude"), "exclude", max_items=64, max_len=4096),
        "tools_needed": _strings(args.get("tools_needed"), "tools_needed", max_items=32, max_len=64, pattern=_TOOL_RE),
        "env_vars_needed": _strings(
            args.get("env_vars_needed"), "env_vars_needed", max_items=32, max_len=128, pattern=_ENV_RE
        ),
        "note": _CONTROL_RE.sub("", note.replace("\r\n", "\n")),
    }


def _reply(**fields: Any) -> str:
    return json.dumps(fields, ensure_ascii=False, sort_keys=True, default=str)


def handler(ctx, provider: Callable[[], Any] | None = None) -> Callable[..., str]:
    """The tool handler ``(args, session_id=…, **kwargs) -> JSON string``. ``provider`` returns the plugin object
    of ``commands`` (``.backend``, ``.agent_request``); ``../__init__.py`` passes it. Never raises."""

    def handle(args: dict, session_id: str = "", **_kw: Any) -> str:
        if provider is None:
            return _reply(ok=False, error="The handover tool is not wired in this process.")
        try:
            a = validate(args)
        except ToolArgError as exc:
            return _reply(ok=False, error=str(exc))
        if not session_id:
            return _reply(ok=False, error="No session is bound to this tool call.")
        try:
            plugin = provider()
            backend = plugin.backend
            action = a["action"]
            if action == "peers":
                peers = [
                    {"name": p.name, "direction": p.direction, "rights": sorted(str(r) for r in p.rights)}
                    for p in backend.peers()
                ]
                return _reply(ok=True, peers=peers)
            if action == "status":
                state = backend.session_state(session_id)
                jobs = [
                    {
                        **{k: j.get(k) for k in ("job_id", "mode", "state", "peer", "next_action")},
                        "step": (j.get("progress") or {}).get("step") if isinstance(j.get("progress"), dict) else None,
                    }
                    for j in (backend.job_status(None).get("jobs") or ())
                    if j.get("calling_session") == session_id
                ]
                return _reply(ok=True, session=state, jobs=jobs)
            if action == "suggest":
                data = {k: a[k] for k in ("resources", "exclude", "tools_needed", "env_vars_needed", "note")}
                backend.suggestion_put(session_id, data)
                return _reply(
                    ok=True,
                    recorded=True,
                    message=(
                        f"Recorded for 30 minutes: {len(a['resources'])} resources, {len(a['exclude'])} exclusions, "
                        f"{len(a['tools_needed'])} tools, {len(a['env_vars_needed'])} variable names. Paths outside "
                        "the project are only proposed; the user confirms them."
                    ),
                )
            if action == "plan":
                return _plan(backend, session_id, a["peer"])
            if action == "request":
                return _request(plugin, backend, session_id, a["peer"], _kw.get("session_key"))
        except Exception as exc:  # never raise into the tool loop
            logger.debug("handover tool failed", exc_info=True)
            text = getattr(exc, "message", None) or type(exc).__name__
            return _reply(ok=False, error=str(text)[:500])
        return _reply(ok=False, error="unknown action")

    return handle


def _plan(backend: Any, session_id: str, peer_name: str | None) -> str:
    from .model import Options

    peer = backend.peer(peer_name) if peer_name else None
    if peer_name and peer is None:
        return _reply(ok=False, error=f"No paired device named {peer_name}.")
    mode = Mode.PUSH if peer is not None and peer.direction == "dial" else Mode.EXPORT
    options = Options(dry_run=True, yes=True, refine="off")
    _job, final = backend.start_job(
        mode,
        peer if mode is Mode.PUSH else None,
        options,
        session_ref=SessionRef(id=session_id),
        calling_session=session_id,
        session_key=None,
        bundle_path=None,
        confirm=None,
        side_question=None,
        reporter=NullReporter(),
    )
    if final is None:
        return _reply(ok=False, error="No plan could be made for this session.")
    summary = plan_summary(final)
    return _reply(
        ok=True,
        summary=(
            f"{summary['items_count']} items, {human_bytes(summary['total_bytes'])}, {summary['proposed']} proposed, "
            f"{summary['conflicts']} conflicts"
        ),
        plan=summary,
        locked=False,
    )


def _request(plugin: Any, backend: Any, session_id: str, peer_name: str | None, session_key: str | None) -> str:
    if not peer_name:
        return _reply(ok=False, error="request needs peer (the name of a paired device).")
    source = backend.session_source(session_id)
    if source is None or str(source).lower() not in LOCAL_PLATFORMS:
        return _reply(
            ok=False, error="A handover can only be requested from a session on this machine's CLI, TUI or Desktop."
        )
    peer = backend.peer(peer_name)
    if peer is None or peer.revoked_at:
        return _reply(ok=False, error=f"No paired device named {peer_name}.")
    settings = settings_from_config(backend.config())
    if not settings.get("allow_agent_start"):
        backend.suggestion_put(session_id, {"request": peer.name})
        return _reply(ok=True, started=False, message=f"Recorded. Ask the user to run /handover to {peer.name}.")
    job = plugin.agent_request(session_id, session_key, peer.name)
    return _reply(
        ok=True,
        started=True,
        message=(
            f"The handover to {peer.name} starts when this turn ends (proposed items left out; it stops if anything "
            "needs a confirmation). /handover status shows it."
        ),
        job=job,
    )
