"""Suggestion routes of the settings page (SETTINGS-PAGE §4): server directory, members, user names, and
Hermes' profiles, skills and models. Each answer names its source; none of them is authority — what is saved
is an id or name the store and ``store.checks`` validate."""

from __future__ import annotations

from typing import Any

from ..store.schema import is_snowflake
from . import directory, members
from .responses import WebResponse, error, ok


def _routes():
    from . import routes  # late: routes imports this module's handlers

    return routes


def get_directory(req: Any, bot: str, guild: str) -> WebResponse:
    routes = _routes()
    routes._ids_or_404(bot, guild)
    snap = routes.store_for(req.home).read()
    routes._bot_or_404(req, bot, snap)
    found = routes.guild_dir(req, bot, guild)
    return ok({**found.as_dict(), "label": found.label()})


def get_members(req: Any, bot: str, guild: str) -> WebResponse:
    routes = _routes()
    routes._ids_or_404(bot, guild)
    snap = routes.store_for(req.home).read()
    routes._bot_or_404(req, bot, snap)
    mode = routes.settings_io.member_search_mode(req.backend)
    try:
        result = members.search(req.home, bot, guild, req.query.get("q", ""), process=req.process, mode=mode,
                                hermes=req.backend.hermes)
    except ValueError as exc:
        return error(422, "invalid", str(exc), field_name="q")
    except members.Unavailable as exc:
        return error(503, "unavailable", str(exc))
    return ok({**result, "note": None})


def get_users(req: Any, bot: str) -> WebResponse:
    routes = _routes()
    snap = routes.store_for(req.home).read()
    routes._bot_or_404(req, bot, snap)
    ids = [i for i in str(req.query.get("ids", "")).split(",") if is_snowflake(i.strip())]
    return ok({"users": members.users(req.home, bot, [i.strip() for i in ids], process=req.process,
                                      hermes=req.backend.hermes,
                                      mode=routes.settings_io.member_search_mode(req.backend))})


def get_profiles(req: Any) -> WebResponse:
    rows = req.backend.profiles()
    if rows is None:
        return ok({"profiles": [], "source": "none"})
    current = req.backend.profile_of(req.home)
    return ok({"profiles": [{"name": r["name"], "display_name": r.get("display_name") or "",
                             "model": r.get("model"), "provider": r.get("provider"),
                             "current": r["name"] == current} for r in rows], "source": "hermes"})


def _profile_home(req: Any) -> tuple[Any, str]:
    name = str(req.query.get("profile") or "").strip()
    if not name or name == req.backend.profile_of(req.home):
        return req.home, req.backend.profile_of(req.home)
    return req.backend.profile_home(name), name


def get_skills(req: Any) -> WebResponse:
    home, name = _profile_home(req)
    if home is None:
        return error(404, "not_found", f"There is no Hermes profile named {name}.")
    rows = req.backend.skill_rows(home)
    return ok({"profile": name, "skills": rows or [], "source": "hermes" if rows is not None else "none"})


def get_models(req: Any) -> WebResponse:
    home, name = _profile_home(req)
    if home is None:
        return error(404, "not_found", f"There is no Hermes profile named {name}.")
    payload = req.backend.models(None if home == req.home else name)
    providers = []
    for row in (payload or {}).get("providers") or []:
        if not isinstance(row, dict):
            continue
        ident = str(row.get("slug") or row.get("id") or "")
        if not ident:
            continue
        models = []
        for model in row.get("models") or []:
            model_id = model.get("id") if isinstance(model, dict) else model
            if model_id:
                label = model.get("label") or model.get("name") if isinstance(model, dict) else None
                models.append({"id": str(model_id), "label": str(label or model_id)})
        providers.append({"id": ident, "label": str(row.get("name") or row.get("label") or ident),
                          "models": models[:200]})
    return ok({"profile": name, "providers": providers, "source": "hermes" if payload is not None else "none"})


ROUTES = [
    ("GET", r"/directory/([^/]+)/([^/]+)", get_directory),
    ("GET", r"/members/([^/]+)/([^/]+)", get_members),
    ("GET", r"/users/([^/]+)", get_users),
    ("GET", r"/hermes/profiles", get_profiles),
    ("GET", r"/hermes/skills", get_skills),
    ("GET", r"/hermes/models", get_models),
]
