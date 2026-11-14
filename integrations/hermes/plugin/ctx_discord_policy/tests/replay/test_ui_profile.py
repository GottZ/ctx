"""S12 L3: the Profile panel replayed through discord.py's real dispatch, per role.

The Hermes side (profiles, served set, configuration, running route table) is ``unit.routing_fakes``;
the real behaviour of those pieces is pinned by ``tests/contract/test_routing_contract.py``.
"""

from __future__ import annotations

from typing import Any, Optional

import pytest

discord = pytest.importorskip("discord")

from tests.replay.harness import (  # noqa: E402
    ADMIN_USER_ID, APP_ID, GUILD_ID, MEMBER_USER_ID, ReplayHarness, app_command_payload, interaction_payload,
    message_payload)
from unit.routing_fakes import FakeAdapter, FakeHermes, FakeRunner, arm, parse  # noqa: E402

pytestmark = [pytest.mark.asyncio, pytest.mark.replay]

OP_ID = 123456789012345678
MANAGE = discord.Permissions(view_channel=True, send_messages=True, manage_guild=True)
PLAIN = discord.Permissions(view_channel=True, send_messages=True)
BOT, GUILD = str(APP_ID), str(GUILD_ID)
EPHEMERAL = 64
NAME = f"ctx_discord_policy:{BOT}:{GUILD}"


class UI:
    def __init__(self, anchor, monkeypatch):
        self.anchor = anchor
        self.settings: dict = {"operators": [str(OP_ID)], "locked_profiles": []}
        anchor.mod("config").set_reader(lambda home, key, default: self.settings.get(key, default))
        self.fake = FakeHermes(anchor.home).install(monkeypatch, anchor.mod("hermes"))
        self.runner = FakeRunner(anchor.home)
        self.adapter = FakeAdapter(self.runner)
        self.items = anchor.mod("ui.items")
        self.ops = anchor.mod("store.ops")
        self.store = anchor.mod("cli").store_for(anchor.home)
        self.ops.bind(self.store, BOT, GUILD, by="cli")

    def profiles(self, *names, armed=True):
        for name in names:
            home = self.fake.add_profile(name, display_name=f"{name} helper")
            if armed:
                arm(self.anchor, home, name)

    def guild(self):
        return self.store.read().guild(BOT, GUILD)

    def raw(self) -> str:
        return self.store.path.read_text()


@pytest.fixture
def ui(anchor, monkeypatch):
    from fakes.ctx import FakeCtx

    anchor.entry.register(FakeCtx("default"))
    u = UI(anchor, monkeypatch)
    yield u
    anchor.mod("config").set_reader(None)


async def harness(ui: UI) -> ReplayHarness:
    h = ReplayHarness()
    await h.start()
    h.seed_guild(GUILD_ID, name="Example Guild")
    ui.binding = ui.anchor.mod("ui.commands").install(h.client, ui.adapter, bot_id=BOT, owner_home=ui.anchor.home,
                                                      store=ui.store, schedule=False)
    return h


def controls(request) -> list:
    data = request.data if request.data else (request.payload or {})
    return [c for row in data.get("components") or [] for c in row.get("components", [])]


def labels(request) -> list:
    return [c.get("label") for c in controls(request) if c.get("type") == 2]


def embed(request) -> dict:
    data = request.data if request.data else (request.payload or {})
    return (data.get("embeds") or [{}])[0]


def field(request, name) -> str:
    return next((f["value"] for f in embed(request).get("fields", []) if f["name"] == name), "")


def cid(ui, request, action, arg=None) -> str:
    for c in controls(request):
        ref = ui.items.parse(c.get("custom_id", ""))
        if ref.action == action and (arg is None or ref.arg == arg):
            return c["custom_id"]
    raise AssertionError(f"no control {action} in {labels(request)}")


def select(request) -> dict:
    found = [c for c in controls(request) if c.get("type") == 3]
    assert found, labels(request)
    return found[0]


def panel(request) -> dict:
    data = request.data if request.data else (request.payload or {})
    return message_payload(data.get("components") or [], flags=EPHEMERAL)


def last_panel(sent) -> Any:
    """The panel shown after a click: an in-place update (7) or the edit after a deferred update (6)."""
    for request in reversed(sent):
        if request.callback_type == 7 or (request.method == "PATCH" and request.path.endswith("@original")):
            return request
    raise AssertionError([(r.method, r.path, r.callback_type) for r in sent])


def text(sent) -> str:
    request = sent[-1]
    return (request.data.get("content") if request.data else None) or (request.payload or {}).get("content") or ""


async def command(h, *, user, perms, page: Optional[str] = None):
    options = [{"name": "page", "type": 3, "value": page}] if page else None
    return await h.replay(app_command_payload("policy", options=options, user_id=user, permissions=perms))


async def click(h, message, custom_id, *, user, perms, values=None):
    data: dict = {"custom_id": custom_id, "component_type": 3 if values is not None else 2}
    if values is not None:
        data["values"] = values
    return await h.replay(interaction_payload(3, data, message=message, user_id=user, permissions=perms))


def forged(custom_id) -> dict:
    return message_payload([{"type": 1, "components": [{"type": 2, "style": 1, "label": "x",
                                                         "custom_id": custom_id}]}], flags=EPHEMERAL)


async def open_profile(ui, h):
    home = (await command(h, user=OP_ID, perms=PLAIN))[0]
    assert "🔒 Profile" in labels(home)
    page = await click(h, panel(home), cid(ui, home, "nav", "profile"), user=OP_ID, perms=PLAIN)
    return last_panel(page)


async def test_operator_sees_the_profile_page(ui):
    ui.profiles("community")
    h = await harness(ui)
    try:
        home = (await command(h, user=OP_ID, perms=PLAIN))[0]
        assert "🔒 Profile" in [f["name"] for f in embed(home)["fields"]]
        page = await open_profile(ui, h)
        assert embed(page)["title"] == "Hermes profile for this server"
        assert "the bot's own profile" in field(page, "Runs in")
        assert "armed" in field(page, "Protection")
        assert labels(page) == ["Choose profile…", "Why not others?", "Back"]
        assert h.task_errors == []
    finally:
        await h.close()


async def test_managers_never_see_or_change_the_profile(ui):
    """M-P1: the setting is operator-only — panel hidden, page refused, forged clicks refused."""
    ui.profiles("community")
    h = await harness(ui)
    try:
        home = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        assert "🔒 Profile" not in labels(home) and "Profile" not in labels(home)
        assert not [f for f in embed(home)["fields"] if "Profile" in f["name"]]
        page = await command(h, user=ADMIN_USER_ID, perms=MANAGE, page="profile")
        assert "Only the bot's operator can change the Hermes profile" in text(page)
        before = ui.raw()
        for action, arg in (("prof_ok", "community"), ("prof_own_ok", ""), ("prof_pick", "0"), ("prof_adopt_ok", "x"),
                            ("prof_apply", "")):
            forged_id = ui.items.encode(GUILD, action, arg, ui.store.read().guild_rev(BOT, GUILD))
            sent = await click(h, forged(forged_id), forged_id, user=ADMIN_USER_ID, perms=MANAGE)
            assert "Only the bot's operator" in text(sent), action
        member = await click(h, forged(ui.items.encode(GUILD, "prof_ok", "community", 1)),
                             ui.items.encode(GUILD, "prof_ok", "community", 1), user=MEMBER_USER_ID, perms=PLAIN)
        assert "Manage Server" in text(member)
        assert ui.raw() == before and ui.fake.config_entries == []
    finally:
        await h.close()


async def test_switch_with_confirmation_pages_through_thirty_profiles(ui):
    ui.profiles(*(f"p{i:02d}" for i in range(30)))
    h = await harness(ui)
    try:
        page = await open_profile(ui, h)
        listing = last_panel(await click(h, panel(page), cid(ui, page, "prof_pick", "0"), user=OP_ID, perms=PLAIN))
        assert len(select(listing)["options"]) == 25 and "Next" in labels(listing)
        nxt = last_panel(await click(h, panel(listing), cid(ui, listing, "prof_pick", "1"), user=OP_ID, perms=PLAIN))
        options = select(nxt)["options"]
        assert [o["value"] for o in options] == [f"p{i:02d}" for i in range(25, 30)] and "Previous" in labels(nxt)
        assert options[0]["label"] == "p25 — p25 helper"
        confirm = last_panel(await click(h, panel(nxt), select(nxt)["custom_id"], user=OP_ID, perms=PLAIN,
                                         values=["p27"]))
        assert embed(confirm)["title"] == "Switch this server to p27?"
        assert "dropped" in embed(confirm)["description"]
        assert ui.guild()["routing"]["profile"] is None                   # nothing before the confirmation
        done = last_panel(await click(h, panel(confirm), cid(ui, confirm, "prof_ok"), user=OP_ID, perms=PLAIN))
        assert embed(done)["description"].startswith("✅ This server now runs in `p27`.")
        assert ui.guild()["routing"]["profile"] == "p27" and ui.guild()["routing"]["set_by"] == str(OP_ID)
        assert ui.fake.config_entries == [{"name": NAME, "platform": "discord", "guild_id": GUILD, "profile": "p27"}]
        assert [r.name for r in ui.runner.config.profile_routes] == [NAME]
        assert "saved in Hermes' configuration · live" in field(done, "Runs in")
        assert "Use the bot's own profile" in labels(done)
        assert h.task_errors == []
    finally:
        await h.close()


async def test_why_not_others_names_reason_and_next_step(ui):
    ui.profiles("community")
    ui.profiles("research", armed=False)
    ui.settings["locked_profiles"] = ["community"]
    h = await harness(ui)
    try:
        page = await open_profile(ui, h)
        sent = await click(h, panel(page), cid(ui, page, "prof_why"), user=OP_ID, perms=PLAIN)
        body = text(sent)
        assert "`community` — locked" in body
        assert "`research` — ctx_discord_policy is not enabled there" in body
        assert "hermes -p research plugins enable ctx_discord_policy" in body
        for request in sent:
            if request.callback_type in (4, 5):
                assert request.data.get("flags", 0) & EPHEMERAL
    finally:
        await h.close()


async def test_a_disarmed_choice_is_refused_at_the_click(ui):
    """Eligibility is checked again when confirming, not only when listing."""
    ui.profiles("community")
    h = await harness(ui)
    try:
        page = await open_profile(ui, h)
        listing = last_panel(await click(h, panel(page), cid(ui, page, "prof_pick", "0"), user=OP_ID, perms=PLAIN))
        confirm = last_panel(await click(h, panel(listing), select(listing)["custom_id"], user=OP_ID, perms=PLAIN,
                                         values=["community"]))
        ui.anchor.RUNTIME.armed.disarm(ui.anchor.mod("hermes").home_key(ui.anchor.home / "profiles" / "community"))
        sent = await click(h, panel(confirm), cid(ui, confirm, "prof_ok"), user=OP_ID, perms=PLAIN)
        assert "Nothing was changed: community: ctx_discord_policy is not enabled there" in text(sent)
        assert ui.guild()["routing"]["profile"] is None and ui.fake.config_entries == []
    finally:
        await h.close()


async def test_take_over_a_hand_written_route(ui):
    ui.profiles("lounge")
    ui.fake.config_entries = [{"name": "lounge-guild", "platform": "discord", "guild_id": GUILD, "profile": "lounge"}]
    ui.runner.config.profile_routes = parse(ui.fake.config_entries)
    h = await harness(ui)
    try:
        page = await open_profile(ui, h)
        assert "routed by Hermes' configuration" in field(page, "Runs in") and "Take over" in labels(page)
        confirm = last_panel(await click(h, panel(page), cid(ui, page, "prof_adopt"), user=OP_ID, perms=PLAIN))
        assert "nothing changes" in embed(confirm)["description"]
        done = last_panel(await click(h, panel(confirm), cid(ui, confirm, "prof_adopt_ok"), user=OP_ID, perms=PLAIN))
        assert embed(done)["description"].startswith("✅ This server now runs in `lounge`.")
        assert ui.fake.config_entries == [{"name": NAME, "platform": "discord", "guild_id": GUILD, "profile": "lounge"}]
        assert ui.guild()["routing"]["profile"] == "lounge"
    finally:
        await h.close()


async def test_pending_choice_offers_apply_again(ui):
    ui.profiles("community")
    h = await harness(ui)
    try:
        ui.anchor.mod("routing.apply").set_profile(ui.store, BOT, GUILD, "community", owner_home=ui.anchor.home,
                                                   by=str(OP_ID), adapter=ui.adapter)
        ui.runner.config.profile_routes = []                              # e.g. Hermes restarted from a backup
        page = await open_profile(ui, h)
        assert "stay closed" in field(page, "Runs in") and "Apply again" in labels(page)
        assert embed(page)["color"] == 0xE74C3C
        done = last_panel(await click(h, panel(page), cid(ui, page, "prof_apply"), user=OP_ID, perms=PLAIN))
        assert embed(done)["description"].startswith("✅")
        assert [r.name for r in ui.runner.config.profile_routes] == [NAME]
    finally:
        await h.close()


async def test_configuration_that_contradicts_the_policy_offers_both_sides(ui):
    """§3.3/§6.3: the plugin's own entry was edited by hand to another profile — use it, or apply ours again."""
    ui.profiles("community", "research")
    h = await harness(ui)
    try:
        ui.anchor.mod("routing.apply").set_profile(ui.store, BOT, GUILD, "community", owner_home=ui.anchor.home,
                                                   by=str(OP_ID), adapter=ui.adapter)
        ui.fake.config_entries = [{"name": NAME, "platform": "discord", "guild_id": GUILD, "profile": "research"}]
        ui.runner.config.profile_routes = parse(ui.fake.config_entries)  # restarted with the hand edit
        page = await open_profile(ui, h)
        assert "Apply again" in labels(page) and "Use research" in labels(page)
        assert "`research`" in field(page, "Runs in")
        confirm = last_panel(await click(h, panel(page), cid(ui, page, "prof_use"), user=OP_ID, perms=PLAIN))
        assert embed(confirm)["title"].startswith("Switch this server to research")
        done = last_panel(await click(h, panel(confirm), cid(ui, confirm, "prof_ok"), user=OP_ID, perms=PLAIN))
        assert embed(done)["description"].startswith("✅ This server now runs in `research`.")
        assert ui.guild()["routing"]["profile"] == "research"
        assert [(r.name, r.profile) for r in ui.runner.config.profile_routes] == [(NAME, "research")]
    finally:
        await h.close()


async def test_long_profile_names_survive_the_custom_id_limit(ui):
    long_name = "a-very-long-profile-name-for-the-public-helper-bot"
    ui.profiles(long_name)
    h = await harness(ui)
    try:
        page = await open_profile(ui, h)
        listing = last_panel(await click(h, panel(page), cid(ui, page, "prof_pick", "0"), user=OP_ID, perms=PLAIN))
        confirm = last_panel(await click(h, panel(listing), select(listing)["custom_id"], user=OP_ID, perms=PLAIN,
                                         values=[long_name]))
        assert all(len(c["custom_id"]) <= 100 for c in controls(confirm))
        await click(h, panel(confirm), cid(ui, confirm, "prof_ok"), user=OP_ID, perms=PLAIN)
        assert ui.guild()["routing"]["profile"] == long_name
    finally:
        await h.close()


async def test_selection_unavailable_without_multiplexing(ui):
    ui.runner.config.multiplex_profiles = False
    h = await harness(ui)
    try:
        page = await open_profile(ui, h)
        assert "multiplex_profiles true" in field(page, "Not available")
        assert labels(page) == ["Back"]
    finally:
        await h.close()
