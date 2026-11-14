"""Settings page P1: the HTTP backend's guard and routes, without Hermes (SETTINGS-PAGE §2.3, §3.4).

The routes are framework-free (``dsp.web.routes.handle(WebRequest) -> WebResponse``); FastAPI only carries
them in ``dashboard/plugin_api.py``. These tests drive the handlers directly with a fake Hermes backend;
``tests/contract/test_dashboard_api.py`` drives the same code through Hermes' real dashboard app.
"""

from __future__ import annotations

import json
import os
import time
from pathlib import Path
from types import SimpleNamespace

import pytest
from fakes.dashboard import FakeBackend

BOT = "900000000000000001"
GUILD = "900000000000000002"
GUILD2 = "900000000000000003"
OP = "900000000000000011"
CHANNEL = "900000000000000051"


@pytest.fixture
def web(anchor, monkeypatch):
    ns = SimpleNamespace()
    ns.anchor = anchor
    ns.routes = anchor.mod("web.routes")
    ns.guard = anchor.mod("web.guard")
    ns.ops = anchor.mod("store.ops")
    ns.file = anchor.mod("store.file")
    ns.hermes = anchor.mod("hermes")
    ns.backend = FakeBackend()
    ns.backend.hermes = ns.hermes
    ns.home = anchor.home
    ns.data = ns.hermes.plugin_data_dir(ns.home)
    ns.store = ns.file.PolicyStore(ns.data / "policy.json")

    def call(method, path, body=None, query=None, process="dashboard"):
        req = ns.routes.WebRequest(method=method, path=path, query=query or {}, body=body, home=ns.home,
                                   plugin_dir=Path(anchor.entry.__file__).parent, process=process,
                                   surface=process, backend=ns.backend)
        resp = ns.routes.handle(req)
        json.dumps(resp.body)  # every answer is JSON
        return resp

    ns.call = call
    return ns


def _bind(web, guild=GUILD):
    return web.ops.bind(web.store, BOT, guild, by="cli").guild(BOT, guild)["rev"]


def _status_file(web, bots):
    """The gateway's status file; every bot entry names its owner home (this one unless the test says so)."""
    bots = {b: {"owner_home": str(web.home), **info} for b, info in bots.items()}
    web.data.mkdir(parents=True, exist_ok=True)
    (web.data / "status.json").write_text(json.dumps({"status_schema": 1, "bots": bots, "interventions": {}}),
                                          encoding="utf-8")


# ── guard ───────────────────────────────────────────────────────────────────────────────────────


def _pre(web, method, headers=None, admitted=True, origin=True):
    return web.guard.precheck(method, {k.lower(): v for k, v in (headers or {}).items()},
                              admitted=lambda: admitted, origin_ok=lambda: origin)


def test_guard_refuses_without_admission_and_fails_closed_without_the_hermes_check(web):
    assert _pre(web, "GET", admitted=False).status == 401
    assert _pre(web, "GET", admitted=None).status == 503       # Hermes' admission check missing: closed

    def boom():
        raise RuntimeError("guard broke")

    assert web.guard.precheck("GET", {}, admitted=boom, origin_ok=lambda: True).status == 503
    assert _pre(web, "GET") is None


@pytest.mark.parametrize("ctype", ["text/plain", "application/x-www-form-urlencoded", "multipart/form-data", ""])
def test_guard_writes_need_a_json_body(web, ctype):
    """M6: an HTML form can post text/plain, urlencoded or multipart, never application/json."""
    response = _pre(web, "POST", {"Content-Type": ctype})
    assert response.status == 415 and response.body["error"]["code"] == "json_only"


def test_guard_checks_the_origin_of_writes_only(web):
    json_ct = {"Content-Type": "application/json; charset=utf-8"}
    assert _pre(web, "PATCH", json_ct, origin=False).status == 403
    assert _pre(web, "GET", origin=False) is None
    for method in ("POST", "PUT", "PATCH", "DELETE"):
        assert _pre(web, method, json_ct) is None
    assert _pre(web, "OPTIONS", json_ct).status == 405


def test_fallback_origin_check_for_hermes_without_its_own(web):
    check = web.guard.fallback_origin_ok
    assert check({}, host="127.0.0.1:9119", scheme="http", public_url="")                  # native client
    assert check({"sec-fetch-site": "none"}, host="h", scheme="http", public_url="")
    assert not check({"sec-fetch-site": "cross-site", "origin": "http://evil"}, host="h", scheme="http",
                     public_url="")
    assert not check({"sec-fetch-site": "same-site", "origin": "http://h:1"}, host="h", scheme="http",
                     public_url="")
    assert check({"sec-fetch-site": "same-origin", "origin": "https://dash.example"}, host="internal:9119",
                 scheme="http", public_url="https://dash.example")
    assert check({"origin": "http://127.0.0.1:9119"}, host="127.0.0.1:9119", scheme="http", public_url="")
    assert not check({"origin": "http://internal"}, host="internal", scheme="http", public_url="https://dash.example")
    for bad in ("http://127.0.0.1:9119/path", "http://user@127.0.0.1:9119", "null", "file:///x"):
        assert not check({"origin": bad}, host="127.0.0.1:9119", scheme="http", public_url=""), bad
    assert not check({"origin": "http://127.0.0.1:5173"}, host="127.0.0.1:9119", scheme="http", public_url="")
    assert check({"origin": "https://dash.example"}, host="internal", scheme="http",
                 public_url="https://dash.example/")


def test_host_actor_comes_from_the_guard(web, anchor):
    actor = web.guard.host_actor()
    perms = anchor.mod("ui.perms")
    assert actor.role == perms.HOST and actor.user_id == "dashboard"


# ── reading ─────────────────────────────────────────────────────────────────────────────────────


def test_meta_names_process_and_version(web):
    body = web.call("GET", "/meta", process="gateway").body
    assert body["plugin_version"] == "3.0.0" and body["api"] == 1 and body["process"] == "gateway"
    assert body["features"]["guidance_ops"] is callable(getattr(web.ops, "set_guidance", None))


def test_overview_without_any_bot_explains_the_next_step(web):
    body = web.call("GET", "/overview").body
    assert body["bots"] == [] and body["store"]["state"] == "missing"
    assert "gateway" in body["next_step"].lower()
    assert body["operators"] == {"count": 1, "readable": True}


def test_overview_lists_configured_and_known_servers(web):
    _bind(web)
    web.ops.set_state(web.store, BOT, GUILD, "paused", by=OP, expected_rev=1)
    _status_file(web, {BOT: {"owner_home": str(web.home)}})
    body = web.call("GET", "/overview").body
    (bot,) = body["bots"]
    assert bot["id"] == BOT
    (row,) = bot["guilds"]
    assert (row["id"], row["state"], row["configured"], row["rev"]) == (GUILD, "paused", True, 2)
    assert row["updated"]["by"] == OP and row["summary"]["ambient"] == 0
    assert body["next_step"] is None


def test_overview_is_read_only_when_the_store_is_damaged(web):
    web.data.mkdir(parents=True, exist_ok=True)
    (web.data / "policy.json").write_text("{not json", encoding="utf-8")
    body = web.call("GET", "/overview").body
    assert body["mode"] == "read-only" and body["store"]["state"] == "corrupt"


def test_guild_of_a_bot_this_profile_does_not_own_is_not_found(web):
    assert web.call("GET", f"/guilds/{BOT}/{GUILD}").status == 404
    _bind(web)
    body = web.call("GET", f"/guilds/{BOT}/{GUILD}").body
    assert body["rev"] == 1 and body["state"] == "active" and body["settings"]["progress"]["grouping"] == "default"
    assert web.call("GET", f"/guilds/{BOT}/{GUILD2}").status == 404


def test_schema_lists_sections_in_table_order_with_kinds(web):
    body = web.call("GET", "/schema").body
    keys = [s["key"] for s in body["sections"]]
    assert keys[:4] == ["channels", "identity", "guidance", "progress"]
    fields = {f["path"]: f for s in body["sections"] for f in s["fields"]}
    assert fields["conversation.ambient_channels"]["kind"] == "channel_list"
    assert fields["worklog.archive_minutes"]["kind"] == "choice"
    assert {c["value"] for c in fields["worklog.archive_minutes"]["choices"]} == {60, 1440, 4320, 10080}
    assert fields["identity.wake_names"]["max_items"] == 5
    assert "prompts.history" not in fields      # written only by the guidance operations


def test_unknown_routes_and_wrong_methods(web):
    assert web.call("GET", "/nope").status == 404
    _bind(web)
    for path in (f"/guilds/{BOT}/{GUILD}/state", f"/guilds/{BOT}/{GUILD}/guidance", "/preview",
                 f"/guilds/{BOT}/{GUILD}/reset", f"/guilds/{BOT}/{GUILD}/import"):
        response = web.call("GET", path, query={"state": "active"})   # M7: no change through GET
        assert response.status == 405, path
    assert web.store.read().guild(BOT, GUILD)["rev"] == 1


# ── writing settings ────────────────────────────────────────────────────────────────────────────


def test_patch_writes_through_the_store_with_rev_and_audit(web):
    rev = _bind(web)
    response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}",
                        {"expected_rev": rev, "section": "progress", "changes": {"progress.grouping": "rotate"}})
    assert response.status == 200, response.body
    snap = web.store.read()
    guild = snap.guild(BOT, GUILD)
    assert guild["progress"]["grouping"] == "rotate" and guild["rev"] == rev + 1   # M3: rev bumped by the store
    assert guild["updated_by"] == "dashboard"
    last = snap.data["audit"][-1]
    assert (last["field"], last["by"], last["via"]) == ("progress.grouping", "dashboard", "dashboard")
    assert response.body["guild"]["rev"] == rev + 1


def test_patch_with_a_stale_revision_is_a_conflict(web):
    """M4: the second write with the same expected_rev must not overwrite the first."""
    rev = _bind(web)
    body = {"expected_rev": rev, "section": "progress", "changes": {"progress.grouping": "rotate"}}
    assert web.call("PATCH", f"/guilds/{BOT}/{GUILD}", body).status == 200
    body["changes"] = {"progress.grouping": "separate"}
    response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", body)
    assert response.status == 409
    error = response.body["error"]
    assert error["code"] == "conflict" and response.body["current_rev"] == rev + 1
    assert response.body["updated_by"] == "dashboard" and response.body["updated_by_label"]
    assert web.store.read().guild(BOT, GUILD)["progress"]["grouping"] == "rotate"


@pytest.mark.parametrize("changes,section,field", [
    ({"worklog.archive_minutes": 7}, "worklog", "worklog.archive_minutes"),
    ({"conversation.ambient_channels": ["x"]}, "channels", "conversation.ambient_channels"),
    ({"identity.wake_names": ["a", "b", "c", "d", "e", "f"]}, "identity", "identity.wake_names"),
])
def test_patch_validates_in_the_server(web, changes, section, field):
    """M2: validation is the store's strict validate_guild, whatever the browser checked."""
    rev = _bind(web)
    response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": section,
                                                           "changes": changes})
    assert response.status == 422 and response.body["error"]["field"] == field
    assert web.store.read().guild(BOT, GUILD)["rev"] == rev


def test_patch_refuses_paths_outside_the_named_section_and_unknown_sections(web):
    rev = _bind(web)
    url = f"/guilds/{BOT}/{GUILD}"
    stray = web.call("PATCH", url, {"expected_rev": rev, "section": "progress",
                                    "changes": {"progress.grouping": "rotate", "worklog.channel": CHANNEL}})
    assert stray.status == 422
    assert web.call("PATCH", url, {"expected_rev": rev, "section": "operators", "changes": {}}).status == 422
    assert web.call("PATCH", url, {"expected_rev": rev, "section": "nope",
                                   "changes": {"progress.grouping": "rotate"}}).status == 422
    assert web.store.read().guild(BOT, GUILD)["rev"] == rev


def test_patch_takes_the_store_from_the_route_scope_never_from_the_body(web, tmp_path):
    """M12: a home or profile in the body is an unknown key, not a redirect to another store."""
    rev = _bind(web)
    other = tmp_path / "other-home"
    response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {
        "expected_rev": rev, "section": "progress", "changes": {"progress.grouping": "rotate"},
        "home": str(other), "profile": "other"})
    assert response.status == 422 and response.body["error"]["field"] in ("home", "profile")
    assert not (other / "plugin-data").exists()


def test_patch_needs_an_integer_revision(web):
    _bind(web)
    for rev in (None, "1", True, 1.5):
        response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": "progress",
                                                               "changes": {"progress.grouping": "rotate"}})
        assert response.status == 422 and response.body["error"]["field"] == "expected_rev", rev


def test_patch_refuses_a_skill_the_target_profile_lacks(web):
    rev = _bind(web)
    web.backend.skills_by_profile[str(web.home)] = frozenset({"faq-lookup"})
    response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": "public_lanes",
                                                           "changes": {"lanes.skills": ["missing-one"]}})
    assert response.status == 422
    assert [f["code"] for f in response.body["error"]["findings"]] == ["skill_missing"]
    ok = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": "public_lanes",
                                                     "changes": {"lanes.skills": ["faq-lookup"]}})
    assert ok.status == 200 and ok.body["warnings"] == []


def test_writes_are_refused_while_the_store_is_damaged(web):
    web.data.mkdir(parents=True, exist_ok=True)
    (web.data / "policy.json").write_text("{not json", encoding="utf-8")
    _status_file(web, {BOT: {}})
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/state", {"expected_rev": 0, "state": "active"})
    assert response.status == 423


# ── lifecycle ───────────────────────────────────────────────────────────────────────────────────


def test_activate_pause_resume_and_purge(web):
    _status_file(web, {BOT: {}})
    url = f"/guilds/{BOT}/{GUILD}/state"
    response = web.call("POST", url, {"expected_rev": 0, "state": "active"})
    assert response.status == 200 and response.body["guild"]["state"] == "active"
    assert web.store.read().guild(BOT, GUILD)["bound_by"] == "dashboard"
    assert web.call("POST", url, {"expected_rev": 1, "state": "paused"}).body["guild"]["state"] == "paused"
    assert web.call("POST", url, {"expected_rev": 1, "state": "active"}).status == 409
    assert web.call("POST", url, {"expected_rev": 2, "state": "active"}).body["guild"]["state"] == "active"
    purged = web.call("POST", url, {"expected_rev": 3, "state": "unbound", "purge": True})
    assert purged.status == 200 and purged.body["purged"] is True
    assert web.store.read().guild(BOT, GUILD) is None
    assert web.call("POST", url, {"expected_rev": 0, "state": "sideways"}).status == 422


def test_state_change_of_an_unknown_guild_needs_activation_first(web):
    _status_file(web, {BOT: {}})
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/state", {"expected_rev": 0, "state": "paused"})
    assert response.status == 404


# ── guidance, reset, routing: operations of other layers ────────────────────────────────────────


def test_guidance_without_the_guidance_operations_saves_the_text_without_history(web, monkeypatch):
    monkeypatch.delattr(web.ops, "set_guidance", raising=False)
    monkeypatch.delattr(web.ops, "restore_guidance", raising=False)
    rev = _bind(web)
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/guidance", {"expected_rev": rev, "text": "Be kind."})
    assert response.status == 200 and response.body["history"] is False
    guild = web.store.read().guild(BOT, GUILD)
    assert guild["prompts"]["guild"] == "Be kind." and guild["rev"] == rev + 1
    assert web.call("POST", f"/guilds/{BOT}/{GUILD}/guidance/restore",
                    {"expected_rev": rev + 1, "sha256": "0" * 64}).status == 501


def test_guidance_uses_the_shared_operation_when_it_exists(web, monkeypatch):
    calls = []

    def set_guidance(store, bot, guild, text, *, by, via, expected_rev, prompt_cap):
        calls.append((bot, guild, text, by, via, expected_rev, prompt_cap))
        return web.ops.update_fields(store, bot, guild, {"prompts.guild": text}, by=by, expected_rev=expected_rev)

    monkeypatch.setattr(web.ops, "set_guidance", set_guidance, raising=False)
    rev = _bind(web)
    web.backend.settings["guild_prompt_max_chars"] = 1234
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/guidance", {"expected_rev": rev, "text": "Hi"})
    assert response.status == 200 and response.body["history"] is True
    assert calls == [(BOT, GUILD, "Hi", "dashboard", "dashboard", rev, 1234)]


def test_guidance_longer_than_the_operator_cap_is_refused(web, monkeypatch):
    monkeypatch.delattr(web.ops, "set_guidance", raising=False)
    rev = _bind(web)
    web.backend.settings["guild_prompt_max_chars"] = 10
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/guidance", {"expected_rev": rev, "text": "x" * 11})
    assert response.status == 422 and response.body["error"]["field"] == "prompts.guild"


def test_reset_and_routing_answer_not_available_without_their_operations(web, monkeypatch):
    for name in ("reset_fields", "set_routing", "adopt_routing", "restore_guidance"):
        monkeypatch.delattr(web.ops, name, raising=False)
    rev = _bind(web)
    base = f"/guilds/{BOT}/{GUILD}"
    assert web.call("POST", base + "/reset", {"expected_rev": rev, "section": "progress"}).status == 501
    assert web.call("POST", base + "/routing", {"expected_rev": rev, "profile": None}).status == 501
    assert web.call("POST", base + "/routing/adopt", {"expected_rev": rev}).status == 501
    assert web.call("POST", base + "/guidance/restore", {"expected_rev": rev, "sha256": "a" * 64}).status == 501


def test_reset_passes_the_section_paths_to_the_shared_operation(web, monkeypatch):
    seen = {}

    def reset_fields(store, bot, guild, paths, *, by, via, expected_rev):
        seen.update(paths=sorted(paths), by=by, via=via, rev=expected_rev)
        return store.read()

    monkeypatch.setattr(web.ops, "reset_fields", reset_fields, raising=False)
    rev = _bind(web)
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/reset", {"expected_rev": rev, "section": "worklog"})
    assert response.status == 200
    assert seen == {"paths": ["worklog.archive_minutes", "worklog.channel", "worklog.final_to_origin"],
                    "by": "dashboard", "via": "dashboard", "rev": rev}


# ── import / export ─────────────────────────────────────────────────────────────────────────────


def test_export_and_import_round_trip_with_rev(web):
    rev = _bind(web)
    exported = web.call("GET", f"/guilds/{BOT}/{GUILD}/export").body
    exported["bots"][BOT]["guilds"][GUILD]["identity"]["bot_name"] = "Imported"
    url = f"/guilds/{BOT}/{GUILD}/import"
    assert web.call("POST", url, {"expected_rev": rev + 5, "document": exported}).status == 409
    response = web.call("POST", url, {"expected_rev": rev, "document": exported})
    assert response.status == 200
    assert web.store.read().guild(BOT, GUILD)["identity"]["bot_name"] == "Imported"
    bad = web.call("POST", url, {"expected_rev": rev + 1, "document": {"operators": [OP]}})
    assert bad.status == 422


# ── preview ─────────────────────────────────────────────────────────────────────────────────────


def test_preview_assembles_the_unsaved_draft_and_writes_nothing(web):
    rev = _bind(web)
    path = web.data / "policy.json"
    before = path.stat().st_mtime_ns
    response = web.call("POST", "/preview", {"bot": BOT, "guild": GUILD, "lane": "public", "mode": "ambient",
                                             "draft": "Talk about {server_name}. {botname}"})
    assert response.status == 200, response.body
    body = response.body
    assert "> Talk about" in body["text"] and body["guidance"] == "included"
    assert [s["kind"] for s in body["sections"]] == ["frame", "capability", "conversation", "guidance"]
    assert body["check"]["chars"] == len("Talk about {server_name}. {botname}")
    assert "{botname}" in body["unknown_placeholders"] and body["budget"] == 9500
    assert path.stat().st_mtime_ns == before and web.store.read().guild(BOT, GUILD)["rev"] == rev


def test_preview_in_the_operator_lane_leaves_guidance_out_by_default(web):
    _bind(web)
    body = web.call("POST", "/preview", {"bot": BOT, "guild": GUILD, "lane": "operator", "mode": "direct",
                                         "draft": "Some guidance"}).body
    assert body["guidance"] == "excluded_operator_lane" and "Some guidance" not in body["text"]


def test_preview_refuses_unknown_lanes(web):
    _bind(web)
    assert web.call("POST", "/preview", {"bot": BOT, "guild": GUILD, "lane": "root", "mode": "direct"}).status == 422


# ── global settings (Hermes configuration) ──────────────────────────────────────────────────────


def test_settings_round_trip_through_the_hermes_writer_with_digest(web):
    got = web.call("GET", "/settings").body
    assert got["values"]["operators"] == [OP] and got["readable"] is True
    assert set(got["values"]) >= {"operators", "frame_prompt", "operator_label", "guild_prompt_max_chars",
                                  "notify_operators"}
    response = web.call("PUT", "/settings", {"expected_digest": got["digest"],
                                             "values": {"operators": [OP, "900000000000000012"],
                                                        "operator_label": "the host"}})
    assert response.status == 200, response.body
    assert web.backend.writes == [{"operators": [OP, "900000000000000012"], "operator_label": "the host"}]
    assert web.backend.events == [["operator_label", "operators"]]
    assert response.body["digest"] != got["digest"]
    assert not (web.data / "policy.json").exists()      # M8: never the policy store


def test_settings_conflict_and_validation(web):
    digest = web.call("GET", "/settings").body["digest"]
    web.backend.settings["operator_label"] = "changed elsewhere"
    stale = web.call("PUT", "/settings", {"expected_digest": digest, "values": {"operator_label": "x"}})
    assert stale.status == 409 and web.backend.writes == []
    digest = web.call("GET", "/settings").body["digest"]
    for values, field in (({"operators": ["not-an-id"]}, "operators"),
                          ({"guild_prompt_max_chars": 20001}, "guild_prompt_max_chars"),
                          ({"operator_label": "two\nlines"}, "operator_label"),
                          ({"notify_operators": "yes"}, "notify_operators"),
                          ({"unknown_key": 1}, "unknown_key")):
        response = web.call("PUT", "/settings", {"expected_digest": digest, "values": values})
        assert response.status == 422 and response.body["error"]["field"] == field, values
    assert web.backend.writes == []


def test_settings_are_not_written_over_an_unreadable_configuration(web):
    """M9: a broken config.yaml is never overwritten from a fallback copy."""
    web.backend.unreadable = True
    got = web.call("GET", "/settings").body
    assert got["readable"] is False and got["values"]["operators"] == []
    response = web.call("PUT", "/settings", {"expected_digest": got["digest"], "values": {"operator_label": "x"}})
    assert response.status == 423 and web.backend.writes == []
    web.backend.unreadable, web.backend.managed = False, True
    digest = web.call("GET", "/settings").body["digest"]
    assert web.call("PUT", "/settings", {"expected_digest": digest,
                                         "values": {"operator_label": "x"}}).status == 423


# ── status and audit ────────────────────────────────────────────────────────────────────────────


def test_status_reads_the_gateway_status_file_and_the_page_heartbeat(web):
    assert web.call("GET", "/status").body["status"] is None
    web.data.mkdir(parents=True, exist_ok=True)
    (web.data / "status.json").write_text(json.dumps({
        "status_schema": 1, "pid": 4242, "plugin_version": "3.0.0", "hermes_version": "0.23.1",
        "written_at": 1.0, "armed": {"k": {"profile": "default"}},
        "interventions": {"A1[default]": {"klass": "S", "outcome": "ok", "reason": "in_process", "probe": "x",
                                          "at": 1.0}}}), encoding="utf-8")
    body = web.call("GET", "/status").body
    assert body["pid"] == 4242 and body["armed_profiles"] == ["default"]
    assert body["interventions"][0]["id"] == "A1[default]"
    assert body["page"]["process"] == "dashboard"            # the request itself left a heartbeat
    assert web.call("GET", "/status/history").body == {"entries": []}


def test_audit_is_filtered_and_names_the_origin(web):
    rev = _bind(web)
    web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": "progress",
                                                "changes": {"progress.grouping": "rotate"}})
    _bind(web, GUILD2)
    entries = web.call("GET", "/audit", query={"bot": BOT, "guild": GUILD}).body["entries"]
    assert {e["guild"] for e in entries} == {GUILD}
    assert entries[0]["field"] == "progress.grouping" and entries[0]["via"] == "dashboard"
    assert entries[0]["label"] == "the Hermes settings page"


def test_no_answer_carries_a_secret_from_the_environment(web, monkeypatch):
    """F9: tokens and .env values never appear in a response."""
    monkeypatch.setenv("DISCORD_BOT_TOKEN", "secret-token-value-123")
    _bind(web)
    for method, path in (("GET", "/meta"), ("GET", "/overview"), ("GET", "/settings"), ("GET", "/status"),
                         ("GET", f"/guilds/{BOT}/{GUILD}"), ("GET", "/schema")):
        assert "secret-token-value-123" not in json.dumps(web.call(method, path).body)


# ── the field catalogue restates the schema's limits ────────────────────────────────────────────


def _with(schema, path, value):
    guild = schema.guild_defaults()
    node = guild
    *parents, leaf = path.split(".")
    for part in parents:
        node = node[part]
    node[leaf] = value
    return guild


def test_catalogue_limits_agree_with_the_schema(anchor):
    """The browser's provisional checks use the catalogue; the server validates with the schema. They agree."""
    catalog, schema = anchor.mod("web.catalog"), anchor.mod("store.schema")
    sample = {"channel_list": lambda i: str(900000000000000100 + i), "role_list": lambda i: str(900000000000000200 + i),
              "text_list": lambda i: f"w{i}", "skill_list": lambda i: f"skill-{i}",
              "knowledge_list": lambda i: {"tool": "mcp__kb__get", "id": f"id{i}"}}
    checked = 0
    for path, entry in catalog.FIELDS.items():
        if entry["kind"] in sample and "max_items" in entry:
            make = sample[entry["kind"]]
            schema.validate_guild(_with(schema, path, [make(i) for i in range(entry["max_items"])]))
            with pytest.raises(schema.SchemaError):
                schema.validate_guild(_with(schema, path, [make(i) for i in range(entry["max_items"] + 1)]))
            checked += 1
        if entry["kind"] == "text" and "max_length" in entry:
            schema.validate_guild(_with(schema, path, "x" * entry["max_length"]))
            with pytest.raises(schema.SchemaError):
                schema.validate_guild(_with(schema, path, "x" * (entry["max_length"] + 1)))
            checked += 1
        if entry["kind"] == "int":
            schema.validate_guild(_with(schema, path, entry["max"]))
            with pytest.raises(schema.SchemaError):
                schema.validate_guild(_with(schema, path, entry["max"] + 1))
            checked += 1
        if entry["kind"] == "choice":
            for choice in entry["choices"]:
                schema.validate_guild(_with(schema, path, choice["value"]))
            checked += 1
    assert checked == 13


def test_every_settings_field_of_the_schema_has_a_catalogue_entry(anchor):
    catalog, perms, schema = anchor.mod("web.catalog"), anchor.mod("ui.perms"), anchor.mod("store.schema")
    designed = [p for p in schema.field_paths() if p not in perms.BOOKKEEPING and p not in catalog.HIDDEN]
    missing = [p for p in designed if p not in catalog.FIELDS and p.split(".")[0] not in ("routing", "models")]
    assert missing == []


# ── review round 1 ──────────────────────────────────────────────────────────────────────────────


def test_a_bot_another_profile_owns_is_not_this_profiles(web):
    """The gateway writes its process-wide bot list into every profile's status file (owner per entry)."""
    _status_file(web, {BOT: {"owner_home": str(web.anchor.tmp / "other-profile")}})
    assert web.call("GET", "/overview").body["bots"] == []
    assert web.call("GET", f"/guilds/{BOT}/{GUILD}").status == 404
    assert web.call("POST", f"/guilds/{BOT}/{GUILD}/state", {"expected_rev": 0, "state": "active"}).status == 404


def test_patch_never_writes_the_guidance_text(web):
    rev = _bind(web)
    response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": "guidance",
                                                           "changes": {"prompts.guild": "x"}})
    assert response.status == 422 and response.body["error"]["field"] == "prompts.guild"


def test_an_operation_without_a_revision_check_is_not_called(web, monkeypatch):
    calls = []

    def reset_fields(store, bot, guild, paths, *, by):
        calls.append(paths)

    monkeypatch.setattr(web.ops, "reset_fields", reset_fields, raising=False)
    rev = _bind(web)
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/reset", {"expected_rev": rev, "section": "progress"})
    assert response.status == 501 and calls == []
    assert web.call("POST", f"/guilds/{BOT}/{GUILD}/reset", {"expected_rev": rev, "section": "guidance",
                                                            "paths": ["prompts.history"]}).status == 422


def test_import_keeps_the_lifecycle_and_the_guidance_history(web):
    _status_file(web, {BOT: {}})
    payload = web.anchor.mod("store.schema").guild_defaults()
    payload["state"] = "active"
    payload["prompts"]["history"] = [{"sha256": "a" * 64, "by": "forged", "at": "2020-01-01T00:00:00Z",
                                      "text": "forged"}]
    url = f"/guilds/{BOT}/{GUILD}/import"
    response = web.call("POST", url, {"expected_rev": 0, "document": payload})
    assert response.status == 200, response.body
    guild = web.store.read().guild(BOT, GUILD)
    assert guild["state"] == "unbound" and guild["prompts"]["history"] == []     # activation stays explicit
    web.call("POST", f"/guilds/{BOT}/{GUILD}/state", {"expected_rev": guild["rev"], "state": "active"})
    rev = web.store.read().guild(BOT, GUILD)["rev"]
    paused = dict(payload, state="paused")
    assert web.call("POST", url, {"expected_rev": rev, "document": paused}).status == 200
    assert web.store.read().guild(BOT, GUILD)["state"] == "active"


def test_writes_on_a_damaged_store_say_so_instead_of_not_configured(web):
    _status_file(web, {BOT: {}})
    web.data.mkdir(parents=True, exist_ok=True)
    (web.data / "policy.json").write_text("{not json", encoding="utf-8")
    for path, body in ((f"/guilds/{BOT}/{GUILD}", {"expected_rev": 1, "section": "progress",
                                                  "changes": {"progress.grouping": "rotate"}}),
                       (f"/guilds/{BOT}/{GUILD}/guidance", {"expected_rev": 1, "text": "x"}),
                       (f"/guilds/{BOT}/{GUILD}/import", {"expected_rev": 1, "document": {}})):
        assert web.call("PATCH" if path.endswith(GUILD) else "POST", path, body).status == 423, path


def test_a_busy_store_answers_unavailable(web, monkeypatch):
    rev = _bind(web)

    def locked(*args, **kwargs):
        raise web.file.StoreError("could not lock policy.json.lock")

    monkeypatch.setattr(web.ops, "update_fields", locked)
    response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": "progress",
                                                           "changes": {"progress.grouping": "rotate"}})
    assert response.status == 503 and "busy" in response.body["error"]["message"]


def test_head_answers_like_get(web):
    assert web.call("HEAD", "/meta").status == 200


def test_a_partly_saved_settings_write_says_so(web):
    digest = web.call("GET", "/settings").body["digest"]
    backend = web.backend

    def partly(plugin_dir, values):
        backend.settings["operator_label"] = values["operator_label"]
        raise PermissionError("notify_operators is managed by the administrator")

    backend.write_settings = partly
    response = web.call("PUT", "/settings", {"expected_digest": digest,
                                             "values": {"operator_label": "x", "notify_operators": False}})
    assert response.status == 423 and response.body["error"]["code"] == "partly_saved"
    assert response.body["written"] == ["operator_label"] and backend.events == [["operator_label"]]


def test_answers_carry_no_host_paths(web):
    _status_file(web, {BOT: {"store": str(web.data / "policy.json")}})
    body = web.call("GET", "/status").body
    text = json.dumps(body)
    assert str(web.home) not in text and body["status"]["bots"][BOT]["owner_home"] == "(host path)"


def test_importing_the_current_settings_changes_nothing(web):
    """Re-importing an unchanged export is no change: no new rev, no audit entry, and the answer says so."""
    rev = _bind(web)
    exported = web.call("GET", f"/guilds/{BOT}/{GUILD}/export").body
    audit_before = len(web.store.read().data["audit"])
    url = f"/guilds/{BOT}/{GUILD}/import"
    response = web.call("POST", url, {"expected_rev": rev, "document": exported})
    assert response.status == 200, response.body
    assert response.body["changed"] is False
    snap = web.store.read()
    assert snap.guild(BOT, GUILD)["rev"] == rev and len(snap.data["audit"]) == audit_before
    assert web.call("POST", url, {"expected_rev": rev + 5, "document": exported}).status == 409   # stale view
    changed = web.anchor.mod("store.schema").guild_defaults()
    changed["identity"]["bot_name"] = "Someone else"
    response = web.call("POST", url, {"expected_rev": rev, "document": changed})
    assert response.status == 200 and response.body["changed"] is True
    assert web.store.read().guild(BOT, GUILD)["rev"] == rev + 1


# ── review round 2 ──────────────────────────────────────────────────────────────────────────────


def test_an_import_without_a_prompts_section_keeps_the_guidance_history(web):
    rev = _bind(web)
    snap = web.store.update(lambda doc: [_seed_history(doc)], by="cli")
    history = snap.guild(BOT, GUILD)["prompts"]["history"]
    assert history, "the test needs a history to keep"
    rev = snap.guild(BOT, GUILD)["rev"]
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/import",
                        {"expected_rev": rev, "document": {"identity": {"bot_name": "Imported"}}})
    assert response.status == 200, response.body
    assert web.store.read().guild(BOT, GUILD)["prompts"]["history"] == history


def _seed_history(doc):
    guild = doc["bots"][BOT]["guilds"][GUILD]
    guild["prompts"]["guild"] = "v2"
    guild["prompts"]["history"] = [{"sha256": "3" * 64, "by": OP, "at": "2026-11-14T00:00:00Z", "text": "v1"}]
    return {"bot": BOT, "guild": GUILD, "field": "prompts", "old": None, "new": None, "expected_rev": None}


def test_the_status_answer_names_profiles_not_home_paths_and_only_this_profiles_bots(web):
    other = str(web.anchor.tmp / "other-profile")
    web.data.mkdir(parents=True, exist_ok=True)
    status = {"status_schema": 1, "interventions": {},
              "armed": {str(web.home): {"profile": "default"}, other: {"profile": "work"}},
              "instances": {str(web.home): {"profile": "default", "plugin_dir": str(web.home / "plugins" / "x")}},
              "bots": {BOT: {"owner_home": str(web.home)}, "900000000000000099": {"owner_home": other}}}
    (web.data / "status.json").write_text(json.dumps(status), encoding="utf-8")
    body = web.call("GET", "/status").body
    text = json.dumps(body)
    assert str(web.home) not in text and other not in text
    assert sorted(body["status"]["armed"]) == ["default", "work"] and body["armed_profiles"] == ["default", "work"]
    assert list(body["status"]["bots"]) == [BOT]


@pytest.mark.parametrize("body", [{"state": "unbound", "purge": True}, {"state": "paused"}])
def test_lifecycle_writes_with_a_stale_revision_are_conflicts(web, body):
    rev = _bind(web)
    web.ops.update_fields(web.store, BOT, GUILD, {"progress.grouping": "rotate"}, by="cli", prompt_cap=6000)
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/state", dict(body, expected_rev=rev))
    assert response.status == 409, response.body
    assert web.store.read().guild(BOT, GUILD) is not None


def test_activating_with_a_stale_revision_is_a_conflict(web):
    _status_file(web, {BOT: {}})
    rev = _bind(web)
    web.ops.set_state(web.store, BOT, GUILD, "unbound", by="cli", expected_rev=rev)
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/state", {"expected_rev": rev, "state": "active"})
    assert response.status == 409, response.body
    assert web.store.read().guild(BOT, GUILD)["state"] == "unbound"


def test_the_guidance_fallback_checks_the_revision(web, monkeypatch):
    monkeypatch.delattr(web.ops, "set_guidance", raising=False)
    rev = _bind(web)
    web.ops.update_fields(web.store, BOT, GUILD, {"progress.grouping": "rotate"}, by="cli", prompt_cap=6000)
    response = web.call("POST", f"/guilds/{BOT}/{GUILD}/guidance", {"expected_rev": rev, "text": "late"})
    assert response.status == 409 and web.store.read().guild(BOT, GUILD)["prompts"]["guild"] == ""


def test_patch_never_writes_the_guidance_history(web):
    rev = _bind(web)
    forged = [{"sha256": "a" * 64, "by": "forged", "at": "2020-01-01T00:00:00Z", "text": "x"}]
    response = web.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": "guidance",
                                                           "changes": {"prompts.history": forged}})
    assert response.status == 422 and response.body["error"]["field"] == "prompts.history"


def test_live_bots_of_another_profile_are_not_this_profiles(web, monkeypatch):
    runtime = web.anchor.mod("runtime").RUNTIME
    other = str(web.anchor.tmp / "other-profile")
    monkeypatch.setattr(runtime.bots, "snapshot", lambda: {"900000000000000099": {"owner_home": other},
                                                           BOT: {"owner_home": str(web.home)}})
    assert [b["id"] for b in web.call("GET", "/overview").body["bots"]] == [BOT]
    assert web.call("GET", "/guilds/900000000000000099/" + GUILD).status == 404


def test_resetting_the_guidance_goes_through_the_guidance_write(web, monkeypatch):
    """Reset of the guidance section = set_guidance("") (SETTINGS-PAGE §5.6): the old text stays in the history."""
    resets, texts = [], []

    def reset_fields(store, bot, guild, paths, *, by, via, expected_rev, prompt_cap):
        resets.append(list(paths))
        return store.read()

    def set_guidance(store, bot, guild, text, *, by, via, expected_rev, prompt_cap):
        texts.append(text)
        return store.read()

    monkeypatch.setattr(web.ops, "reset_fields", reset_fields, raising=False)
    monkeypatch.setattr(web.ops, "set_guidance", set_guidance, raising=False)
    rev = _bind(web)
    assert web.call("POST", f"/guilds/{BOT}/{GUILD}/reset", {"expected_rev": rev, "section": "guidance"}).status == 200
    assert texts == [""] and resets == []
    mixed = web.call("POST", f"/guilds/{BOT}/{GUILD}/reset", {"expected_rev": rev, "section": "identity",
                                                             "paths": ["identity.bot_name", "prompts.guild"]})
    assert mixed.status == 422 and resets == []


def test_lifecycle_writes_on_a_damaged_store_are_read_only(web):
    _status_file(web, {BOT: {}})
    web.data.mkdir(parents=True, exist_ok=True)
    (web.data / "policy.json").write_text("{not json", encoding="utf-8")
    for body in ({"expected_rev": 1, "state": "paused"}, {"expected_rev": 1, "state": "unbound", "purge": True},
                 {"expected_rev": 0, "state": "active"}):
        assert web.call("POST", f"/guilds/{BOT}/{GUILD}/state", body).status == 423, body


def test_the_fallback_origin_check_compares_only_the_site_of_a_public_url_with_a_path(web):
    check = web.guard.fallback_origin_ok
    assert check({"origin": "https://dash.example"}, host="internal", scheme="http",
                 public_url="https://dash.example/hermes")
    assert not check({"origin": "https://other.example"}, host="internal", scheme="http",
                     public_url="https://dash.example/hermes")


def test_a_deleted_server_can_be_activated_again_with_the_revision_the_page_shows(web):
    """The store keeps a revision epoch for a purged guild (revisions never repeat): every answer names
    ``Snapshot.guild_rev``, so the page's next write passes the optimistic check."""
    _status_file(web, {BOT: {}})
    root = web.data / "directory" / BOT                 # the bot is still in the server (gateway's directory)
    root.mkdir(parents=True)
    (root / "index.json").write_text(json.dumps({"schema": 1, "bot": BOT, "written_at": time.time(),
                                                 "guilds": [{"id": GUILD, "name": "Example"}]}), encoding="utf-8")
    rev = _bind(web)
    assert web.call("POST", f"/guilds/{BOT}/{GUILD}/state", {"expected_rev": rev, "state": "unbound",
                                                             "purge": True}).status == 200
    shown = web.call("GET", f"/guilds/{BOT}/{GUILD}")
    epoch = web.store.read().guild_rev(BOT, GUILD)
    assert epoch > 0 and shown.status == 200 and shown.body["rev"] == epoch
    row = next(g for g in web.call("GET", "/overview").body["bots"][0]["guilds"] if g["id"] == GUILD)
    assert row["rev"] == epoch
    again = web.call("POST", f"/guilds/{BOT}/{GUILD}/state", {"expected_rev": shown.body["rev"], "state": "active"})
    assert again.status == 200, again.body
    assert web.store.read().guild(BOT, GUILD)["state"] == "active"


def test_restoring_a_version_that_left_the_history_is_not_found(web):
    """With the shared operations (S9): a known version comes back; one no longer in the history is a 404,
    not a validation error of the request."""
    rev = _bind(web)
    url = f"/guilds/{BOT}/{GUILD}"
    assert web.call("POST", url + "/guidance", {"expected_rev": rev, "text": "first"}).status == 200
    rev = web.store.read().guild_rev(BOT, GUILD)
    assert web.call("POST", url + "/guidance", {"expected_rev": rev, "text": "second"}).status == 200
    rev = web.store.read().guild_rev(BOT, GUILD)
    first = web.ops.guidance_digest("first")
    gone = web.call("POST", url + "/guidance/restore", {"expected_rev": rev, "sha256": "0" * 64})
    assert gone.status == 404 and gone.body["error"]["code"] == "not_found", gone.body
    assert web.store.read().guild_rev(BOT, GUILD) == rev                      # nothing written
    back = web.call("POST", url + "/guidance/restore", {"expected_rev": rev, "sha256": first})
    assert back.status == 200 and web.store.read().guild(BOT, GUILD)["prompts"]["guild"] == "first"


def test_profile_and_model_choice_stay_with_discord_until_the_page_runs_the_routing_flow(web):
    """S12/S13 put set_routing/adopt_routing on main; the store operation alone would record a profile
    without Hermes' profile_routes (the routing flow in dsp.routing.apply writes those). The page shows the
    choice but does not set it yet: 501, nothing written, and /meta says routing is not available."""
    rev = _bind(web)
    assert callable(getattr(web.ops, "set_routing", None)), "the test needs the operations on main"
    url = f"/guilds/{BOT}/{GUILD}/routing"
    for path, body in ((url, {"expected_rev": rev, "profile": "work"}), (url + "/adopt", {"expected_rev": rev})):
        response = web.call("POST", path, body)
        assert response.status == 501 and response.body["error"]["code"] == "not_available", (path, response.body)
    guild = web.store.read().guild(BOT, GUILD)
    assert guild["rev"] == rev and (guild.get("routing") or {}).get("profile") is None
    assert web.call("GET", "/meta").body["features"]["routing_ops"] is False
