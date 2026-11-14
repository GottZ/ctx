"""Settings page P2: where suggestions come from (SETTINGS-PAGE §4, S5–S7, mutant M10).

The gateway keeps a directory of the bot's servers (files in plugin-data, live from the discord.py cache in its
own process), answers read-only plugin RPC methods for a backend in another process, and searches members by
prefix with a throttle. Every answer names its source; with nothing available the page falls back to IDs.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import threading
import time
from pathlib import Path
from types import SimpleNamespace

import pytest

from fakes.dashboard import FakeBackend

BOT = "900000000000000001"
GUILD = "900000000000000002"

GUILD_B = "900000000000000003"
CHAN = "900000000000000051"
CAT = "900000000000000050"
ROLE = "900000000000000041"
EMOJI = "900000000000000061"
USER = "900000000000000071"


# ── a discord.py-shaped client ──────────────────────────────────────────────────────────────────


class Kind:
    def __init__(self, name):
        self.name = name


class Perms:
    view_channel = True
    send_messages = False


class Channel:
    def __init__(self, ident, name, kind, category=None, position=0):
        self.id, self.name, self.type, self.category_id, self.position = int(ident), name, Kind(kind), category, position

    def permissions_for(self, member):
        return Perms()


class Role:
    def __init__(self, ident, name, position, default=False):
        self.id, self.name, self.position, self.managed = int(ident), name, position, False
        self.color = SimpleNamespace(value=0x112233)
        self._default = default

    def is_default(self):
        return self._default


class Member:
    def __init__(self, ident, name, nick=None):
        self.id, self.name, self.display_name = int(ident), name, nick or name
        self.display_avatar = SimpleNamespace(url=f"https://cdn.example/{ident}.png")


class Guild:
    def __init__(self, ident, name, members=()):
        self.id, self.name = int(ident), name
        self.icon = SimpleNamespace(key="abc123")
        self.me = Member("900000000000000001", "bot")
        self.channels = [Channel(CAT, "Text channels", "category"), Channel(CHAN, "general", "text", int(CAT), 1)]
        self.threads = []
        self.roles = [Role(ident, "@everyone", 0, default=True), Role(ROLE, "Mods", 5)]
        self.emojis = [SimpleNamespace(id=int(EMOJI), name="wave", animated=False, available=True)]
        self._members = list(members)
        self.queries = []

    async def query_members(self, query, limit, cache):
        self.queries.append((query, limit, cache))
        return [m for m in self._members if m.name.lower().startswith(query.lower())][:limit]


class Client:
    def __init__(self, guilds, loop=None):
        self.guilds = guilds
        self.user = SimpleNamespace(name="Policy Bot", display_name="Policy Bot", id=int(BOT))
        self.loop = loop
        self.listeners = {}
        self.users = {}

    def add_listener(self, fn, name):
        self.listeners.setdefault(name, []).append(fn)

    def remove_listener(self, fn, name):
        self.listeners.get(name, []).remove(fn)

    def get_guild(self, ident):
        return next((g for g in self.guilds if g.id == ident), None)

    def is_ready(self):
        return True

    def get_user(self, ident):
        return self.users.get(ident)

    async def fetch_user(self, ident):
        raise LookupError(ident)


def _bind(ns, guild=GUILD):
    return ns.ops.bind(ns.store, BOT, guild, by="cli").guild(BOT, guild)["rev"]


def _status_file(ns, bots):
    """The gateway's status file; every bot entry names its owner home (this one unless the test says so)."""
    bots = {b: {"owner_home": str(ns.home), **info} for b, info in bots.items()}
    ns.data.mkdir(parents=True, exist_ok=True)
    (ns.data / "status.json").write_text(json.dumps({"status_schema": 1, "bots": bots, "interventions": {}}),
                                         encoding="utf-8")


@pytest.fixture
def loop_thread():
    loop = asyncio.new_event_loop()
    thread = threading.Thread(target=loop.run_forever, daemon=True)
    thread.start()
    yield loop
    loop.call_soon_threadsafe(loop.stop)
    thread.join(5)
    loop.close()


@pytest.fixture
def src(anchor, monkeypatch):
    ns = SimpleNamespace(anchor=anchor)
    ns.snapshot = anchor.mod("snapshot")
    ns.directory = anchor.mod("web.directory")
    ns.remote = anchor.mod("web.remote")
    ns.members = anchor.mod("web.members")
    ns.rpc = anchor.mod("rpc")
    ns.routes = anchor.mod("web.routes")
    ns.ops = anchor.mod("store.ops")
    ns.file = anchor.mod("store.file")
    ns.hermes = anchor.mod("hermes")
    ns.home = anchor.home
    ns.data = ns.hermes.plugin_data_dir(ns.home)
    ns.store = ns.file.PolicyStore(ns.data / "policy.json")
    ns.backend = FakeBackend()
    ns.backend.hermes = ns.hermes
    ns.remote.reset()
    ns.members.reset()
    ns.snapshot.THROTTLE = ns.snapshot.Throttle()
    ns.members.REST_THROTTLE = ns.snapshot.Throttle()

    def call(method, path, body=None, query=None, process="dashboard"):
        req = ns.routes.WebRequest(method=method, path=path, query=query or {}, body=body, home=ns.home,
                                   plugin_dir=Path(anchor.entry.__file__).parent, process=process, surface=process,
                                   backend=ns.backend)
        resp = ns.routes.handle(req)
        json.dumps(resp.body)
        return resp

    ns.call = call
    yield ns
    for bot in list(ns.snapshot.LIVE):
        ns.snapshot.detach(bot)


def _refresh(src, client, store=None, reason="bound"):
    entry = src.snapshot.LiveBot(BOT, src.home, src.hermes.home_key(src.home), lambda: client, store)
    asyncio.run(src.snapshot.refresh(entry, reason))
    return src.directory.directory_root(src.home) / BOT


# ── the directory files the gateway writes ──────────────────────────────────────────────────────


def test_refresh_writes_index_and_server_files_without_members(src):
    client = Client([Guild(GUILD, "Example Server", [Member(USER, "alice")])])
    root = _refresh(src, client)
    index = json.loads((root / "index.json").read_text())
    assert index["schema"] == 1 and index["bot_name"] == "Policy Bot"
    assert index["guilds"] == [{"id": GUILD, "name": "Example Server", "icon": "abc123"}]
    detail = json.loads((root / f"{GUILD}.json").read_text())
    assert [c["kind"] for c in detail["channels"]] == ["category", "text"]
    assert detail["channels"][1] == {"id": CHAN, "name": "general", "kind": "text", "parent_id": CAT, "position": 1,
                                     "can_view": True, "can_send": False}
    assert [r["name"] for r in detail["roles"]] == ["Mods"]          # @everyone left out
    assert detail["emojis"] == [{"id": EMOJI, "name": "wave", "animated": False, "available": True}]
    assert "alice" not in (root / f"{GUILD}.json").read_text()       # never members


def test_detail_files_for_configured_servers_first_and_left_servers_removed(src, monkeypatch):
    monkeypatch.setattr(src.snapshot, "DETAIL_EXTRA", 1)
    src.ops.bind(src.store, BOT, GUILD_B, by="cli")
    guilds = [Guild(f"9000000000000001{i:02d}", f"s{i}") for i in range(3)] + [Guild(GUILD_B, "configured")]
    root = _refresh(src, Client(guilds), store=src.store)
    files = sorted(p.stem for p in root.glob("*.json"))
    assert files == sorted(["index", GUILD_B, "900000000000000100"])
    _refresh(src, Client([Guild(GUILD_B, "configured")]), store=src.store)
    assert sorted(p.stem for p in root.glob("*.json")) == sorted(["index", GUILD_B])


def test_route_reads_the_snapshot_and_marks_an_old_one_as_stale(src):
    _status_file(src, {BOT: {}})
    root = _refresh(src, Client([Guild(GUILD, "Example Server")]))
    body = src.call("GET", f"/directory/{BOT}/{GUILD}").body
    assert body["source"] == "snapshot" and body["stale"] is False and body["name"] == "Example Server"
    assert {c["id"] for c in body["channels"]} == {CAT, CHAN}
    old = time.time() - 3600
    for path in root.glob("*.json"):
        data = json.loads(path.read_text())
        data["written_at"] = old
        path.write_text(json.dumps(data))
    body = src.call("GET", f"/directory/{BOT}/{GUILD}").body
    assert body["stale"] is True and body["label"].startswith("directory of ")
    overview = src.call("GET", "/overview").body
    assert overview["bots"][0]["online"] is False and overview["bots"][0]["guilds"][0]["name"] == "Example Server"


def test_without_any_source_the_page_gets_ids_only(src):
    _status_file(src, {BOT: {}})
    body = src.call("GET", f"/directory/{BOT}/{GUILD}").body
    assert body["source"] == "none" and body["channels"] == [] and body["label"] == "IDs only"


def test_environment_warnings_use_the_directory(src):
    _status_file(src, {BOT: {}})
    _refresh(src, Client([Guild(GUILD, "Example Server")]))
    rev = _bind(src)
    response = src.call("PATCH", f"/guilds/{BOT}/{GUILD}", {"expected_rev": rev, "section": "channels",
                                                           "changes": {"conversation.ambient_channels": [
                                                               CHAN, "900000000000000099"]}})
    assert response.status == 200
    assert [w["code"] for w in response.body["warnings"]] == ["channel_unknown"]


# ── attaching to the live client ────────────────────────────────────────────────────────────────


def test_attach_registers_listeners_once_and_reports_its_outcome(src, anchor, loop_thread):
    client = Client([Guild(GUILD, "Example Server")], loop=loop_thread)
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    assert set(client.listeners) == set(src.snapshot.EVENTS)
    assert all(len(v) == 1 for v in client.listeners.values())
    assert anchor.RUNTIME.status.get("P2").outcome.value == "ok"
    bare = SimpleNamespace(guilds=[])
    assert src.snapshot.attach(bare, bot_id=BOT, owner_home=src.home) is None
    assert anchor.RUNTIME.status.get("P2").outcome.value == "off"


def test_changes_are_debounced_into_one_write(src, monkeypatch, loop_thread):
    monkeypatch.setattr(src.snapshot, "DEBOUNCE", 0.05)
    calls = []

    async def fake_refresh(entry, reason, guild_id=None):
        calls.append((reason, guild_id))

    monkeypatch.setattr(src.snapshot, "refresh", fake_refresh)
    guild = Guild(GUILD, "Example Server")
    client = Client([guild], loop=loop_thread)
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    handler = client.listeners["on_guild_channel_update"][0]
    for _ in range(3):
        asyncio.run_coroutine_threadsafe(handler(SimpleNamespace(guild=guild), SimpleNamespace(guild=guild)),
                                         loop_thread).result(2)
    time.sleep(0.3)
    assert calls == [("guild_channel_update", GUILD)]


def test_live_data_in_the_gateway_process_comes_from_the_client(src, loop_thread):
    _status_file(src, {BOT: {}})
    client = Client([Guild(GUILD, "Live Server")], loop=loop_thread)
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    body = src.call("GET", f"/directory/{BOT}/{GUILD}", process="gateway").body
    assert body["source"] == "live" and body["name"] == "Live Server"
    other = src.anchor.tmp / "other-home"
    assert src.directory.guild_directory(other, BOT, GUILD).source == "none"   # another home's bot: not ours


# ── members ─────────────────────────────────────────────────────────────────────────────────────


def _live_members(src, loop_thread):
    _status_file(src, {BOT: {}})
    guild = Guild(GUILD, "Example Server", [Member(USER, "alice", "Alice A"), Member("900000000000000072", "albert")])
    client = Client([guild], loop=loop_thread)
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    return guild, client


def test_member_search_by_prefix_in_the_gateway_with_cache_and_throttle(src, loop_thread):
    guild, _ = _live_members(src, loop_thread)
    first = src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"}, process="gateway")
    assert first.status == 200 and first.body["source"] == "live"
    assert [r["id"] for r in first.body["results"]] == [USER, "900000000000000072"]
    assert guild.queries == [("al", 25, False)]
    again = src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"}, process="gateway")
    assert again.status == 200 and len(guild.queries) == 1                     # cached
    busy = src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "ali"}, process="gateway")
    assert busy.status == 503 and len(guild.queries) == 1                      # 1 per second per server
    assert src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "a"}, process="gateway").status == 422


def test_member_search_switched_off_answers_unavailable(src, loop_thread):
    guild, client = _live_members(src, loop_thread)   # keep the client alive: the registry holds it weakly
    src.backend.settings["member_search"] = "off"
    response = src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"}, process="gateway")
    assert response.status == 503 and "switched off" in response.body["error"]["message"]
    assert guild.queries == [] and src.call("GET", "/meta").body["features"]["member_search"] is False
    src.backend.settings["member_search"] = "auto"
    assert src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"}, process="gateway").status == 200


def test_member_search_falls_back_to_rest_only_with_this_bots_token(src, monkeypatch):
    _status_file(src, {BOT: {}})
    monkeypatch.setattr(src.hermes, "discord_bot_token", lambda: "token-value")
    seen = []

    def fake_rest(path, token):
        seen.append(path)
        if path == "/oauth2/applications/@me":
            return {"id": BOT}
        return [{"user": {"id": USER, "username": "alice", "global_name": "Alice"}, "nick": None}]

    monkeypatch.setattr(src.members, "_rest", fake_rest)
    response = src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"})
    assert response.status == 200 and response.body["source"] == "rest"
    assert response.body["results"] == [{"id": USER, "name": "alice", "display_name": "Alice", "avatar": None}]
    assert seen[1].startswith(f"/guilds/{GUILD}/members/search?") and "token-value" not in json.dumps(response.body)
    src.members.reset()

    def other_bots_token(path, token):
        if path == "/oauth2/applications/@me":
            return {"id": "900000000000000999"}
        return [{"user": {"id": USER, "username": "bob"}, "nick": None}]

    monkeypatch.setattr(src.members, "_rest", other_bots_token)
    monkeypatch.setattr(src.members, "REST_THROTTLE", src.snapshot.Throttle())
    refused = src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "bo"})   # the token is another bot's
    assert refused.status == 503 and "not available" in refused.body["error"]["message"]


def test_user_names_over_rest_stop_at_the_first_network_failure(src, monkeypatch):
    """Up to ten ids, one request each: an unreachable Discord ends the loop instead of waiting ten times."""
    _status_file(src, {BOT: {}})
    calls = []

    def rest(path, token):
        calls.append(path)
        if path == "/oauth2/applications/@me":
            return {"id": BOT}
        raise src.members.urllib.error.URLError("timed out")

    monkeypatch.setattr(src.members, "_rest", rest)
    src.backend.hermes = SimpleNamespace(discord_bot_token=lambda: "token-of-this-bot")
    ids = ",".join(f"9000000000000001{i:02d}" for i in range(10))
    assert src.call("GET", f"/users/{BOT}", query={"ids": ids}).body == {"users": {}}
    assert len([c for c in calls if c.startswith("/users/")]) == 1


def test_user_names_come_from_the_live_cache(src, loop_thread):
    _, client = _live_members(src, loop_thread)
    client.users[int(USER)] = SimpleNamespace(name="alice", display_name="Alice A")
    body = src.call("GET", f"/users/{BOT}", query={"ids": f"{USER},900000000000000079,nope"}, process="gateway").body
    assert body == {"users": {USER: {"name": "alice", "display_name": "Alice A"}}}


# ── plugin RPC ──────────────────────────────────────────────────────────────────────────────────


def test_rpc_registers_only_read_methods(src, anchor):
    """M10: no RPC method writes; the registered names are exactly the read-only set."""
    registered = {}
    ctx = SimpleNamespace(register_rpc=lambda name, handler: registered.setdefault(name, handler))
    src.rpc.register(ctx)
    assert set(registered) == set(src.rpc.READ_METHODS)
    assert set(registered) <= {"directory.guilds", "directory.guild", "members.search", "users.resolve",
                               "routing.reapply"}
    assert anchor.RUNTIME.status.get("P3").outcome.value == "ok"
    src.rpc.register(SimpleNamespace())
    assert anchor.RUNTIME.status.get("P3").outcome.value == "off"


def test_rpc_handlers_answer_only_for_bots_of_their_own_profile(src, loop_thread, monkeypatch):
    client = Client([Guild(GUILD, "Live Server")], loop=loop_thread)
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    data = src.rpc.directory_guild({"bot": BOT, "guild": GUILD}, session_id=None)
    assert data["name"] == "Live Server" and data["channels"]
    monkeypatch.setattr(src.hermes, "current_home", lambda: src.anchor.tmp / "another-profile")
    assert src.rpc.directory_guild({"bot": BOT, "guild": GUILD}, session_id=None) is None
    with pytest.raises(ValueError):
        src.rpc.directory_guild({"bot": "x", "guild": GUILD}, session_id=None)


def test_a_separate_dashboard_asks_the_gateway_over_rpc_and_backs_off(src, monkeypatch):
    _status_file(src, {BOT: {}})
    calls = []

    def fake_rpc(home, method, params, timeout):
        calls.append(method)
        if method.endswith("directory.guild"):
            return {"name": "Remote Server", "icon": None, "at": time.time(), "channels": [
                {"id": CHAN, "name": "general", "kind": "text"}], "roles": [], "emojis": []}
        raise ConnectionRefusedError("no gateway")

    monkeypatch.setattr(src.hermes, "rpc_available", lambda: True)
    monkeypatch.setattr(src.hermes, "gateway_rpc", fake_rpc)
    body = src.call("GET", f"/directory/{BOT}/{GUILD}").body
    assert body["source"] == "rpc" and body["name"] == "Remote Server"
    assert calls == ["plugin.ctx_discord_policy.directory.guild"]
    assert src.call("GET", f"/directory/{BOT}/{GUILD}", process="gateway").body["source"] == "none"  # never from inside
    src.remote.reset()
    monkeypatch.setattr(src.hermes, "gateway_rpc", lambda *a: (_ for _ in ()).throw(OSError("down")))
    calls.clear()
    assert src.call("GET", f"/directory/{BOT}/{GUILD}").body["source"] == "none"
    monkeypatch.setattr(src.hermes, "gateway_rpc", fake_rpc)
    assert src.call("GET", f"/directory/{BOT}/{GUILD}").body["source"] == "none" and calls == []   # backing off


# ── Hermes lists ────────────────────────────────────────────────────────────────────────────────


def test_profiles_skills_and_models_come_from_hermes(src, tmp_path):
    other = tmp_path / "community"
    src.backend.profile_rows = [{"name": "default", "path": str(src.home), "display_name": "", "model": "m1",
                                 "provider": "p1"},
                                {"name": "community", "path": str(other), "display_name": "Public helper",
                                 "model": "m2", "provider": "p2"}]
    src.backend.profile_homes["community"] = other
    src.backend.skills_by_profile[str(other)] = frozenset({"faq-lookup"})
    src.backend.model_payload = {"providers": [{"slug": "p2", "name": "Provider Two",
                                                "models": ["m2", {"id": "m3", "label": "Model 3"}]}, {"x": 1}]}
    profiles = src.call("GET", "/hermes/profiles").body["profiles"]
    assert [(p["name"], p["current"]) for p in profiles] == [("default", True), ("community", False)]
    skills = src.call("GET", "/hermes/skills", query={"profile": "community"}).body
    assert skills["profile"] == "community" and [s["name"] for s in skills["skills"]] == ["faq-lookup"]
    assert src.call("GET", "/hermes/skills", query={"profile": "missing"}).status == 404
    models = src.call("GET", "/hermes/models", query={"profile": "community"}).body
    assert models["providers"] == [{"id": "p2", "label": "Provider Two",
                                    "models": [{"id": "m2", "label": "m2"}, {"id": "m3", "label": "Model 3"}]}]


def test_bots_of_other_profiles_are_named_but_not_offered(src, tmp_path):
    """One gateway writes its process-wide bot list (owner per entry) into every profile's status file."""
    other, third = tmp_path / "helper", tmp_path / "gone"
    data = src.hermes.plugin_data_dir(other)
    data.mkdir(parents=True)
    everyone = {BOT: {"owner_home": str(src.home)}, "900000000000000009": {"owner_home": str(other)},
                "900000000000000008": {"owner_home": str(third)}}
    (data / "status.json").write_text(json.dumps({"bots": everyone}), encoding="utf-8")
    _status_file(src, everyone)
    src.backend.profile_rows = [{"name": "default", "path": str(src.home)}, {"name": "helper", "path": str(other)}]
    body = src.call("GET", "/overview").body
    assert body["other_profiles"] == [{"profile": "helper", "bots": ["900000000000000009"]}]
    assert [b["id"] for b in body["bots"]] == [BOT]
    assert str(third) not in json.dumps(body)          # an owner that is no profile is not named by its path


def test_the_discord_attributes_the_directory_reads_exist_in_discord_py():
    """The fakes above mirror discord.py 2.7.1 (Hermes' pin); this keeps them honest."""
    discord = pytest.importorskip("discord")
    from discord.ext import commands

    params = inspect.signature(discord.Guild.query_members).parameters
    assert {"query", "limit", "cache"} <= set(params)
    for owner, names in ((discord.Guild, ("channels", "threads", "roles", "emojis", "me", "icon", "get_channel")),
                         (discord.TextChannel, ("category_id", "position", "permissions_for", "type")),
                         (discord.Thread, ("parent_id", "permissions_for", "type")),
                         (discord.Role, ("is_default", "color", "position", "managed")),
                         (discord.Emoji, ("animated", "available")), (discord.Asset, ("key",)),
                         (discord.Member, ("display_name", "display_avatar")),
                         (discord.Permissions, ("view_channel", "send_messages")),
                         (commands.Bot, ("add_listener", "remove_listener", "get_guild", "get_user", "fetch_user",
                                         "is_ready", "guilds"))):
        for name in names:
            assert hasattr(owner, name), f"{owner.__name__}.{name}"


def test_the_wiring_entry_points_take_the_integrator_shapes(src, anchor, loop_thread):
    client = Client([Guild(GUILD, "Example Server")], loop=loop_thread)
    binding = SimpleNamespace(bot_id=BOT, owner_home=src.home, store=src.store)
    src.snapshot.bind_step(SimpleNamespace(native=client, adapter=None, binding=binding))
    assert src.snapshot.live_bot(src.home, BOT) is not None
    index = src.directory.directory_root(src.home) / BOT / "index.json"
    deadline = time.monotonic() + 3
    while not index.exists() and time.monotonic() < deadline:
        time.sleep(0.02)
    assert index.exists(), "binding writes the directory once (on the bot's loop)"
    registered = []
    src.rpc.register_step(SimpleNamespace(register_rpc=lambda n, h: registered.append(n)), object())
    assert registered == list(src.rpc.READ_METHODS)


# ── review round (P2) ───────────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize("stored", [False, None, "no", "Off"])
def test_member_search_is_off_for_anything_but_auto(src, loop_thread, stored):
    """YAML reads an unquoted ``off`` as False; anything that is not exactly ``auto`` keeps member search off."""
    guild, client = _live_members(src, loop_thread)
    src.backend.settings["member_search"] = stored
    assert src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"}, process="gateway").status == 503
    assert guild.queries == [] and src.call("GET", "/meta").body["features"]["member_search"] is False


def test_member_search_is_off_while_the_configuration_is_unreadable(src, loop_thread):
    guild, client = _live_members(src, loop_thread)
    src.backend.unreadable = True
    assert src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"}, process="gateway").status == 503
    assert guild.queries == []


def test_member_search_off_also_holds_for_the_gateway_methods_and_user_names(src, loop_thread, monkeypatch):
    guild, client = _live_members(src, loop_thread)
    client.users[int(USER)] = SimpleNamespace(name="alice", display_name="Alice A")
    src.backend.settings["member_search"] = "off"
    monkeypatch.setattr(src.hermes, "plugin_settings", lambda: {"member_search": "off"})
    assert src.rpc.members_search({"bot": BOT, "guild": GUILD, "q": "al"}, session_id=None) == \
        {"off": True, "results": []}
    assert src.rpc.users_resolve({"bot": BOT, "ids": [USER]}, session_id=None) == {"users": {}}
    assert guild.queries == []
    body = src.call("GET", f"/users/{BOT}", query={"ids": USER}, process="gateway").body
    assert body == {"users": {}}

    def unreadable():
        raise src.hermes.ConfigUnreadable("broken")

    monkeypatch.setattr(src.hermes, "plugin_settings", unreadable)
    assert src.rpc.members_search({"bot": BOT, "guild": GUILD, "q": "al"}, session_id=None)["off"] is True
    monkeypatch.setattr(src.hermes, "plugin_settings", lambda: {"member_search": "auto"})
    assert src.rpc.members_search({"bot": BOT, "guild": GUILD, "q": "al"}, session_id=None)["results"]


def test_a_dashboard_hears_that_the_gateway_has_member_search_off(src, monkeypatch):
    _status_file(src, {BOT: {}})
    monkeypatch.setattr(src.hermes, "rpc_available", lambda: True)
    monkeypatch.setattr(src.hermes, "gateway_rpc", lambda home, method, params, timeout: {"off": True, "results": []})
    response = src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"})
    assert response.status == 503 and "switched off" in response.body["error"]["message"]


def test_a_closed_or_unready_client_writes_no_fresh_directory(src):
    client = Client([Guild(GUILD, "Example Server")])
    client.is_closed = lambda: True
    root = _refresh(src, client)
    assert not (root / "index.json").exists()
    client.is_closed = lambda: False
    client.is_ready = lambda: False
    assert not (_refresh(src, client) / "index.json").exists()
    client.is_ready = lambda: True
    assert (_refresh(src, client) / "index.json").exists()


def test_a_closed_client_gives_no_live_data(src, loop_thread):
    client = Client([Guild(GUILD, "Live Server")], loop=loop_thread)
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    assert src.snapshot.live_guild(src.home, BOT, GUILD) is not None
    client.is_closed = lambda: True
    assert src.snapshot.live_guild(src.home, BOT, GUILD) is None
    assert src.snapshot.live_members(src.home, BOT, GUILD, "al") is None


def test_a_client_that_is_not_ready_gives_no_live_data(src, loop_thread):
    client = Client([Guild(GUILD, "Live Server")], loop=loop_thread)
    client.is_ready = lambda: False
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    assert src.snapshot.live_guild(src.home, BOT, GUILD) is None and src.snapshot.live_index(src.home, BOT) is None


def test_the_rpc_wait_covers_the_work_the_gateway_may_do(src):
    """A dashboard waits longer than the gateway may work on a search or a name lookup; otherwise every slow
    answer would be thrown away and block the gateway for the whole back-off."""
    assert src.remote.timeout_for("members.search") > src.snapshot.SEARCH_TIMEOUT
    assert src.remote.timeout_for("users.resolve") > src.snapshot.USERS_TIMEOUT
    assert src.remote.timeout_for("directory.guild") >= src.snapshot.CALL_TIMEOUT


def test_name_lookups_in_the_gateway_fetch_at_most_ten_unknown_users(src, loop_thread):
    client = Client([Guild(GUILD, "Live Server")], loop=loop_thread)
    fetched = []

    async def fetch_user(ident):
        fetched.append(ident)
        return SimpleNamespace(name=f"u{ident}", display_name="")

    client.fetch_user = fetch_user
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    ids = [f"9000000000000002{i:02d}" for i in range(25)]
    found = src.snapshot.live_users(src.home, BOT, ids)
    assert len(fetched) == src.snapshot.FETCH_LIMIT == 10 and len(found) == 10


def test_a_call_on_the_bots_own_loop_is_refused_not_deadlocked(src, loop_thread):
    client = Client([], loop=loop_thread)

    async def inner():
        return src.snapshot._on_loop(client, lambda: asyncio.sleep(0), 1.0)

    with pytest.raises(RuntimeError, match="own event loop"):
        asyncio.run_coroutine_threadsafe(inner(), loop_thread).result(3)


def test_a_slow_read_times_out_and_is_cancelled_on_the_bots_loop(src, loop_thread):
    client = Client([], loop=loop_thread)
    state = {"cancelled": False}

    async def slow():
        try:
            await asyncio.sleep(5)
        except asyncio.CancelledError:
            state["cancelled"] = True
            raise

    started = time.monotonic()
    with pytest.raises(Exception):
        src.snapshot._on_loop(client, slow, 0.2)
    assert time.monotonic() - started < 2
    time.sleep(0.2)
    assert state["cancelled"] is True


def test_the_throttle_limits_per_server_and_per_bot(src):
    throttle = src.snapshot.Throttle(per_guild=1.0, per_bot_minute=2)
    assert throttle.allow(BOT, "g1", now=100.0) and not throttle.allow(BOT, "g1", now=100.5)
    assert throttle.allow(BOT, "g2", now=100.6) and not throttle.allow(BOT, "g3", now=100.7)   # 2 per minute
    assert throttle.allow(BOT, "g3", now=161.0)


def test_rest_member_search_is_throttled_too(src, monkeypatch):
    _status_file(src, {BOT: {}})
    monkeypatch.setattr(src.hermes, "discord_bot_token", lambda: "token-value")
    monkeypatch.setattr(src.members, "_rest", lambda path, token: {"id": BOT} if path.startswith("/oauth2") else [])
    assert src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "al"}).status == 200
    second = src.call("GET", f"/members/{BOT}/{GUILD}", query={"q": "bo"})
    assert second.status == 503 and "too fast" in second.body["error"]["message"]


def test_the_heartbeat_rewrites_the_directory_until_the_client_is_gone(src, monkeypatch):
    monkeypatch.setattr(src.snapshot, "HEARTBEAT", 0.02)
    client = Client([Guild(GUILD, "Example Server")])
    holder = {"client": client}
    entry = src.snapshot.LiveBot(BOT, src.home, src.hermes.home_key(src.home), lambda: holder["client"], None)
    reasons = []

    async def refresh(entry_, reason, guild_id=None):
        reasons.append(reason)
        if len(reasons) == 3:
            holder["client"] = None

    monkeypatch.setattr(src.snapshot, "refresh", refresh)
    asyncio.run(asyncio.wait_for(src.snapshot._heartbeat(entry), 2))
    assert reasons == ["heartbeat"] * 3


def test_detach_cancels_pending_writes(src, loop_thread, monkeypatch):
    monkeypatch.setattr(src.snapshot, "DEBOUNCE", 30)
    guild = Guild(GUILD, "Example Server")
    client = Client([guild], loop=loop_thread)
    entry = src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    handler = client.listeners["on_guild_role_update"][0]
    asyncio.run_coroutine_threadsafe(handler(SimpleNamespace(guild=guild), SimpleNamespace(guild=guild)),
                                     loop_thread).result(2)
    handle = next(iter(entry.pending.values()))
    src.snapshot.detach(BOT)
    time.sleep(0.1)
    assert handle.cancelled() and BOT not in src.snapshot.LIVE


def test_rpc_answers_are_cached_briefly(src, monkeypatch):
    calls = []
    monkeypatch.setattr(src.hermes, "rpc_available", lambda: True)
    monkeypatch.setattr(src.hermes, "gateway_rpc", lambda home, method, params, timeout: calls.append(method) or {})
    src.remote.call(src.home, "directory.guilds", {"bot": BOT})
    src.remote.call(src.home, "directory.guilds", {"bot": BOT})
    assert len(calls) == 1


def test_the_token_owner_is_checked_again_after_ten_minutes_and_a_network_failure_soon(src, monkeypatch):
    clock = {"now": 1000.0}
    monkeypatch.setattr(src.members.time, "monotonic", lambda: clock["now"])
    answers = {"app": BOT}
    asked = []

    def rest(path, token):
        asked.append(path)
        if answers["app"] is None:
            raise src.members.urllib.error.URLError("down")
        return {"id": answers["app"]}

    monkeypatch.setattr(src.members, "_rest", rest)
    hermes = SimpleNamespace(discord_bot_token=lambda: "tok")
    assert src.members._token_for(BOT, hermes) == "tok"
    answers["app"] = "900000000000000999"
    clock["now"] += 500
    assert src.members._token_for(BOT, hermes) == "tok" and len(asked) == 1        # cached
    clock["now"] += 200
    assert src.members._token_for(BOT, hermes) is None and len(asked) == 2         # re-checked after 10 min
    src.members.reset()
    answers["app"] = None
    assert src.members._token_for(BOT, hermes) is None                              # network down
    answers["app"] = BOT
    clock["now"] += 90
    assert src.members._token_for(BOT, hermes) == "tok"                             # retried after a minute


def test_the_rpc_search_checks_its_query(src):
    with pytest.raises(ValueError):
        src.rpc.members_search({"bot": BOT, "guild": GUILD, "q": "a"}, session_id=None)


def test_name_lookups_over_rest_stop_at_a_rate_limit(src, monkeypatch):
    _status_file(src, {BOT: {}})
    calls = []

    def rest(path, token):
        calls.append(path)
        if path == "/oauth2/applications/@me":
            return {"id": BOT}
        raise src.members.urllib.error.HTTPError(path, 429, "Too Many Requests", {}, None)

    monkeypatch.setattr(src.members, "_rest", rest)
    src.backend.hermes = SimpleNamespace(discord_bot_token=lambda: "token-of-this-bot")
    ids = ",".join(f"9000000000000001{i:02d}" for i in range(10))
    assert src.call("GET", f"/users/{BOT}", query={"ids": ids}).body == {"users": {}}
    assert len([c for c in calls if c.startswith("/users/")]) == 1


def test_gateway_rpc_never_starts_a_gateway(src, monkeypatch):
    """``connect_gateway()`` without an endpoint may start one; the plugin only connects to a running one."""
    import sys
    import types

    connected = []
    runtime = types.ModuleType("hermes_cli.gateway_runtime")
    client = types.ModuleType("hermes_cli.gateway_client")
    runtime.discover_gateway_endpoint = lambda home, timeout: SimpleNamespace(endpoint=None, state="absent")

    def connect_gateway(**kwargs):
        connected.append(kwargs)
        raise AssertionError("must not connect")

    client.connect_gateway = connect_gateway
    monkeypatch.setitem(sys.modules, "hermes_cli", types.ModuleType("hermes_cli"))
    monkeypatch.setitem(sys.modules, "hermes_cli.gateway_runtime", runtime)
    monkeypatch.setitem(sys.modules, "hermes_cli.gateway_client", client)
    with pytest.raises(RuntimeError, match="not reachable"):
        src.hermes.gateway_rpc(src.home, "plugin.x.y", {}, 1.0)
    assert connected == []


def test_a_failed_directory_write_is_reported_and_cleared_by_the_next_good_one(src, anchor, loop_thread, monkeypatch):
    runtime = anchor.mod("runtime").RUNTIME
    client = Client([Guild(GUILD, "Example Server")], loop=loop_thread)
    entry = src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)

    def broken(*args, **kwargs):
        raise OSError("disk full")

    monkeypatch.setattr(src.snapshot, "write_files", broken)
    task = asyncio.run_coroutine_threadsafe(src.snapshot.refresh(entry, "change"), loop_thread)
    with pytest.raises(OSError):
        task.result(3)
    assert runtime.status.get("P2").outcome.value == "fallback"
    monkeypatch.undo()
    asyncio.run_coroutine_threadsafe(src.snapshot.refresh(entry, "change"), loop_thread).result(3)
    assert runtime.status.get("P2").outcome.value == "ok"


def test_listeners_that_could_not_be_registered_stay_reported(src, anchor):
    runtime = anchor.mod("runtime").RUNTIME

    class Stubborn(Client):
        def add_listener(self, fn, name):
            raise TypeError("no listeners here")

    client = Stubborn([Guild(GUILD, "Example Server")])
    src.snapshot.attach(client, bot_id=BOT, owner_home=src.home, schedule=False)
    assert runtime.status.get("P2").outcome.value == "fallback"


def test_an_old_directory_does_not_claim_a_bot_another_profile_owns(src, tmp_path):
    other = tmp_path / "work"
    _status_file(src, {BOT: {"owner_home": str(other)}})
    root = src.directory.directory_root(src.home) / BOT
    root.mkdir(parents=True)
    (root / "index.json").write_text(json.dumps({"schema": 1, "bot": BOT, "written_at": time.time(),
                                                 "guilds": []}), encoding="utf-8")
    src.backend.profile_rows = [{"name": "default", "path": str(src.home)}, {"name": "work", "path": str(other)}]
    body = src.call("GET", "/overview").body
    assert body["bots"] == [] and body["other_profiles"] == [{"profile": "work", "bots": [BOT]}]


def test_installed_skills_is_one_list_for_the_page_and_the_discord_panels(src, monkeypatch):
    """``hermes.installed_skills(home)``: names of the enabled skills of a profile; raises when Hermes cannot
    tell, which every caller treats as unknown (the Discord panels and the environment check fail closed)."""
    monkeypatch.setattr(src.hermes, "skills_of", lambda home: [{"name": "faq-lookup"}, {"name": "server-index"}])
    assert src.hermes.installed_skills(src.home) == frozenset({"faq-lookup", "server-index"})
    assert src.routes.HermesBackend().skills(src.home) == frozenset({"faq-lookup", "server-index"})
    monkeypatch.setattr(src.hermes, "skills_of", lambda home: None)
    with pytest.raises(RuntimeError):
        src.hermes.installed_skills(src.home)
    assert src.routes.HermesBackend().skills(src.home) is None


def test_a_profile_home_exists_and_has_a_valid_name(src, tmp_path, monkeypatch):
    """The page asks the routing layer's ``hermes.profile_dir`` (one definition for both), and only for a
    valid name whose home exists."""
    backend = src.routes.HermesBackend()
    work = tmp_path / "profiles" / "work"
    work.mkdir(parents=True)
    monkeypatch.setattr(src.hermes, "profile_dir", lambda name: tmp_path / "profiles" / name)
    assert backend.profile_home("work") == work
    assert backend.profile_home("missing") is None
    assert backend.profile_home("../work") is None
