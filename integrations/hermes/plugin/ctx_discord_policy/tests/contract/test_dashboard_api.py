"""Settings page against Hermes' real dashboard app (SETTINGS-PAGE §6.1 V, P0/P2 acceptance, mutants M1–M12).

The plugin is installed into a temporary Hermes home with ``dashboard/``; ``hermes_cli.web_server.app`` mounts
``dashboard/plugin_api.py`` exactly as in production (startup sweep or the hot-mount fallback), so every
request passes Hermes' own middleware chain: the session-token gate, the plugin runtime gate, the profile
scope dependency. Writes are checked on disk: the policy store (rev, audit) and ``config.yaml`` (Hermes'
settings writer).
"""

from __future__ import annotations

import json
import shutil
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

REPO = Path(__file__).resolve().parents[2]
PLUGIN = "ctx_discord_policy"
ANCHOR = "ctx_discord_policy"
BOT = "900000000000000001"
GUILD = "900000000000000002"
OP = "900000000000000011"
BASE = f"/api/plugins/{PLUGIN}/v1"
JSON_CT = {"Content-Type": "application/json"}


def _install(home: Path) -> Path:
    target = home / "plugins" / PLUGIN
    if target.exists():
        shutil.rmtree(target)
    target.mkdir(parents=True)
    for name in ("__init__.py", "plugin.yaml"):
        shutil.copy2(REPO / name, target / name)
    for name in ("dsp", "dashboard"):
        shutil.copytree(REPO / name, target / name, ignore=shutil.ignore_patterns("__pycache__"))
    return target


def _set_enabled(enabled: bool) -> None:
    from hermes_cli.config import load_config, save_config

    cfg = load_config()
    plugins = cfg.setdefault("plugins", {})
    plugins["enabled"] = [PLUGIN] if enabled else []
    plugins["disabled"] = [] if enabled else [PLUGIN]
    save_config(cfg)


def _evict_anchor() -> None:
    for key in [k for k in sys.modules if k == ANCHOR or k.startswith(ANCHOR + ".")]:
        sys.modules.pop(key, None)


@pytest.fixture(scope="module")
def dash(hermes):
    from hermes_constants import get_hermes_home

    home = Path(get_hermes_home())
    plugin_dir = _install(home)
    _set_enabled(True)
    from starlette.testclient import TestClient

    from hermes_cli import web_server
    from hermes_cli.web_server import _SESSION_HEADER_NAME, _SESSION_TOKEN, app

    web_server._dashboard_plugins_cache = None
    original_routes = list(app.router.routes)
    _evict_anchor()
    anonymous = TestClient(app)
    client = TestClient(app)
    client.headers[_SESSION_HEADER_NAME] = _SESSION_TOKEN
    ns = SimpleNamespace(home=home, plugin_dir=plugin_dir, app=app, client=client, anonymous=anonymous,
                         hermes=hermes)
    try:
        first = client.get(f"{BASE}/meta")
        assert first.status_code == 200, first.text
        yield ns
    finally:
        app.router.routes[:] = original_routes
        web_server._dashboard_plugins_cache = None
        _evict_anchor()


def _anchor_mod(name: str):
    import importlib

    return importlib.import_module(f"{ANCHOR}.{name}")


def _data(dash) -> Path:
    return dash.home / "plugin-data" / PLUGIN


def _store(dash):
    return _anchor_mod("store.file").PolicyStore(_data(dash) / "policy.json")


@pytest.fixture
def bound(dash):
    """A clean store with one active guild of a bot this home's gateway reported."""
    data = _data(dash)
    shutil.rmtree(data, ignore_errors=True)
    data.mkdir(parents=True)
    (data / "status.json").write_text(json.dumps({"status_schema": 1, "bots": {BOT: {"owner_home": str(dash.home)}},
                                                  "interventions": {}}), encoding="utf-8")
    _anchor_mod("store.ops").bind(_store(dash), BOT, GUILD, by="cli")
    yield _store(dash)
    shutil.rmtree(data, ignore_errors=True)


def _report(dash, name: str, payload: dict) -> None:
    dash.hermes.write_report(name, payload)


# ── gate and mount (P0 acceptance, M1, M11) ─────────────────────────────────────────────────────


ROUTES = [("GET", "/meta"), ("GET", "/overview"), ("GET", "/schema"), ("GET", f"/guilds/{BOT}/{GUILD}"),
          ("PATCH", f"/guilds/{BOT}/{GUILD}"), ("POST", f"/guilds/{BOT}/{GUILD}/state"), ("POST", "/preview"),
          ("GET", "/settings"), ("PUT", "/settings"), ("GET", "/status"), ("GET", "/audit"),
          ("POST", f"/guilds/{BOT}/{GUILD}/import")]


@pytest.mark.parametrize("method,path", ROUTES)
def test_every_route_needs_a_hermes_sign_in(dash, method, path):
    """M1: no route of the page is reachable without the dashboard credential."""
    response = dash.anonymous.request(method, BASE + path, headers=JSON_CT, content=b"{}")
    assert response.status_code == 401, (method, path, response.status_code, response.text[:200])


def test_meta_answers_behind_the_gate_and_names_the_process(dash, bound):
    body = dash.client.get(f"{BASE}/meta").json()
    assert body["plugin_version"] == "3.0.0" and body["process"] == "dashboard"
    dash.app.state.gateway_runner = object()
    try:
        assert dash.client.get(f"{BASE}/meta").json()["process"] == "gateway"
    finally:
        dash.app.state.gateway_runner = None
    _report(dash, "dashboard-meta", {"meta": body})


def test_a_disabled_plugin_answers_not_found(dash):
    _set_enabled(False)
    try:
        assert dash.client.get(f"{BASE}/meta").status_code == 404
    finally:
        _set_enabled(True)
    assert dash.client.get(f"{BASE}/meta").status_code == 200


def test_the_plugin_host_admission_is_checked_by_the_router_itself(dash, tmp_path):
    """M11: in a plugin-host child the router runs on a bare FastAPI app without Hermes' middleware."""
    from hermes_cli.plugin_host import PluginHost

    host_home = tmp_path / "host-home"
    host_home.mkdir()
    host = PluginHost(SimpleNamespace(home_path=host_home, _plugins={}))
    args = (PLUGIN, str(dash.plugin_dir / "dashboard"), "plugin_api.py", "GET", "/v1/meta", "",
            [("Accept", "application/json")], b"")
    try:
        refused = host.asgi_request(*args, authenticated=False)
        admitted = host.asgi_request(*args, authenticated=True)
    finally:
        try:
            host.stop()
        except Exception:
            pass
    assert refused["status"] == 401, refused
    assert admitted["status"] == 200, admitted
    _report(dash, "dashboard-plugin-host", {"refused": refused["status"], "admitted": admitted["status"]})


def test_another_plugin_version_in_the_process_answers_closed(dash, monkeypatch):
    """Files replaced without a restart: nothing of the loaded runtime is used; without admission only 401,
    with admission GET /meta says read-only and every other route asks for a restart."""
    from fastapi import FastAPI
    from starlette.testclient import TestClient

    import hermes_cli.plugin_host_http_auth as auth

    api = next(m for n, m in sys.modules.items() if n.startswith("hermes_dashboard_plugin_") and PLUGIN in n)
    monkeypatch.setattr(api, "_runtime", lambda: (None, "plugin 9.9.9 is loaded in this process"))
    bare = FastAPI()                                   # like a plugin-host child: no Hermes middleware
    bare.include_router(api.router, prefix=f"/api/plugins/{PLUGIN}")
    client = TestClient(bare)
    assert client.get(f"{BASE}/meta").status_code == 401
    assert client.post(f"{BASE}/guilds/{BOT}/{GUILD}/state", headers=JSON_CT, json={}).status_code == 401
    monkeypatch.setattr(auth, "admitted_for_sensitive_route", lambda request: True)
    meta = client.get(f"{BASE}/meta")
    assert meta.status_code == 200 and meta.json()["mode"] == "read-only" and "9.9.9" in meta.json()["conflict"]
    write = client.post(f"{BASE}/guilds/{BOT}/{GUILD}/state", headers=JSON_CT, json={"expected_rev": 1})
    assert write.status_code == 503 and write.json()["error"]["code"] == "restart_required"


# ── request guard through the real app (M6, M7, F3, F4) ─────────────────────────────────────────


@pytest.mark.parametrize("ctype", ["text/plain", "application/x-www-form-urlencoded"])
def test_form_posts_are_refused(dash, bound, ctype):
    response = dash.client.post(f"{BASE}/guilds/{BOT}/{GUILD}/state", headers={"Content-Type": ctype},
                                content=b'{"expected_rev": 1, "state": "paused"}')
    assert response.status_code == 415
    assert bound.read().guild(BOT, GUILD)["state"] == "active"


def test_cross_site_writes_are_refused(dash, bound):
    response = dash.client.post(f"{BASE}/guilds/{BOT}/{GUILD}/state",
                                headers={**JSON_CT, "Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
                                json={"expected_rev": 1, "state": "paused"})
    assert response.status_code == 403
    assert bound.read().guild(BOT, GUILD)["state"] == "active"


def test_native_client_writes_without_origin_pass(dash, bound):
    response = dash.client.post(f"{BASE}/guilds/{BOT}/{GUILD}/state", headers=JSON_CT,
                                json={"expected_rev": 1, "state": "paused"})
    assert response.status_code == 200, response.text
    assert bound.read().guild(BOT, GUILD)["state"] == "paused"


def test_webapp_same_origin_writes_pass(dash, bound):
    response = dash.client.post(f"{BASE}/guilds/{BOT}/{GUILD}/state",
                                headers={**JSON_CT, "Origin": "http://testserver", "Sec-Fetch-Site": "same-origin"},
                                json={"expected_rev": 1, "state": "paused"})
    assert response.status_code == 200, response.text


def test_get_never_changes_state(dash, bound):
    response = dash.client.get(f"{BASE}/guilds/{BOT}/{GUILD}/state", params={"state": "paused"})
    assert response.status_code == 405
    assert bound.read().guild(BOT, GUILD)["state"] == "active"


def test_deeply_nested_json_is_a_bad_request(dash, bound):
    nested = "[" * 100_000 + "]" * 100_000          # 200 KB, under the cap; json.loads recurses past the limit
    response = dash.client.post(f"{BASE}/guilds/{BOT}/{GUILD}/state", headers=JSON_CT, content=nested)
    assert response.status_code == 400 and response.json()["error"]["code"] == "bad_request"


def test_bodies_are_capped(dash, bound):
    big = json.dumps({"expected_rev": 1, "text": "x" * (300 * 1024)})
    response = dash.client.post(f"{BASE}/guilds/{BOT}/{GUILD}/guidance", headers=JSON_CT, content=big)
    assert response.status_code == 413
    url = f"{BASE}/guilds/{BOT}/{GUILD}/import"
    sized = json.dumps({"expected_rev": 1, "document": {"identity": {"bot_name": "x"}}, "pad": "x" * (300 * 1024)})
    assert dash.client.post(url, headers=JSON_CT, content=sized).status_code == 422    # read, then refused by key
    huge = json.dumps({"expected_rev": 1, "document": {}, "pad": "x" * (1100 * 1024)})
    assert dash.client.post(url, headers=JSON_CT, content=huge).status_code == 413


# ── writes land in the store and in Hermes' configuration (M3, M4, M8, M9) ──────────────────────


def test_patch_goes_through_the_store(dash, bound):
    url = f"{BASE}/guilds/{BOT}/{GUILD}"
    body = {"expected_rev": 1, "section": "progress", "changes": {"progress.grouping": "rotate"}}
    first = dash.client.patch(url, headers=JSON_CT, json=body)
    assert first.status_code == 200, first.text
    second = dash.client.patch(url, headers=JSON_CT, json=dict(body, changes={"progress.grouping": "separate"}))
    assert second.status_code == 409 and second.json()["current_rev"] == 2
    snap = bound.read()
    assert snap.guild(BOT, GUILD)["progress"]["grouping"] == "rotate" and snap.guild(BOT, GUILD)["rev"] == 2
    assert snap.data["audit"][-1]["via"] == "dashboard"
    invalid = dash.client.patch(url, headers=JSON_CT, json={"expected_rev": 2, "section": "worklog",
                                                            "changes": {"worklog.archive_minutes": 7}})
    assert invalid.status_code == 422 and invalid.json()["error"]["field"] == "worklog.archive_minutes"


def test_global_settings_go_through_hermes_settings_writer(dash, bound):
    from hermes_cli.config import load_config

    before = load_config()
    got = dash.client.get(f"{BASE}/settings").json()
    assert got["readable"] is True
    response = dash.client.put(f"{BASE}/settings", headers=JSON_CT, json={
        "expected_digest": got["digest"], "values": {"operators": [OP], "operator_label": "the host"}})
    assert response.status_code == 200, response.text
    after = load_config()
    settings = after["plugins"]["entries"][PLUGIN]["settings"]
    assert settings["operators"] == [OP] and settings["operator_label"] == "the host"
    assert after["plugins"]["enabled"] == before["plugins"]["enabled"]   # other keys unchanged
    assert OP not in json.dumps(bound.read().data)                       # never the policy store
    stale = dash.client.put(f"{BASE}/settings", headers=JSON_CT, json={
        "expected_digest": got["digest"], "values": {"operator_label": "again"}})
    assert stale.status_code == 409
    _report(dash, "dashboard-settings-write", {"written": response.json()["written"]})


def test_settings_are_not_written_over_a_broken_configuration(dash, bound):
    """M9: Hermes serves a fallback copy for an unreadable config.yaml; the page must refuse to write."""
    from hermes_cli import config as hermes_config

    path = Path(hermes_config.get_config_path())
    good = path.read_bytes()
    digest = dash.client.get(f"{BASE}/settings").json()["digest"]
    try:
        path.write_bytes(good + b"\n  broken: [unclosed\n")
        response = dash.client.put(f"{BASE}/settings", headers=JSON_CT, json={
            "expected_digest": digest, "values": {"operator_label": "x"}})
        assert response.status_code == 423, response.text
        assert path.read_bytes().endswith(b"broken: [unclosed\n")
        _report(dash, "dashboard-broken-config", {"status": response.status_code})
    finally:
        path.write_bytes(good)
