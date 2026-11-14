"""S8 L3: /policy, its persistent controls and the example panel, replayed through discord.py's real dispatch.

Every test registers the real interface (``ui.commands.install``) on a real ``commands.Bot`` and feeds raw
interaction payloads; it checks what each role sees, that every answer is ephemeral, the refusal texts,
and that the policy store changes only when the action is allowed.
"""

from __future__ import annotations

import json
from typing import Any, Optional

import pytest

discord = pytest.importorskip("discord")

from tests.replay.harness import (  # noqa: E402
    ADMIN_ROLE_ID,
    ADMIN_USER_ID,
    APP_ID,
    GUILD_ID,
    MEMBER_USER_ID,
    OTHER_GUILD_ID,
    ReplayHarness,
    app_command_payload,
    interaction_payload,
    message_payload,
)

pytestmark = [pytest.mark.asyncio, pytest.mark.replay]

OP_ID = 123456789012345678          # operator (only through the configuration)
MANAGE = discord.Permissions(view_channel=True, send_messages=True, manage_guild=True)
ADMINISTRATOR = discord.Permissions(administrator=True)
PLAIN = discord.Permissions(view_channel=True, send_messages=True)
BOT, GUILD, OTHER = str(APP_ID), str(GUILD_ID), str(OTHER_GUILD_ID)
EPHEMERAL = 64


# ── fixtures ─────────────────────────────────────────────────────────────────────────────────────────


class UI:
    def __init__(self, anchor, settings):
        self.anchor = anchor
        self.settings = settings
        self.commands = anchor.mod("ui.commands")
        self.items = anchor.mod("ui.items")
        self.panels = anchor.mod("ui.panels")
        self.ops = anchor.mod("store.ops")
        self.store = anchor.mod("cli").store_for(anchor.home)
        self.binding = None
        self.dms: list = []

    def install(self, client, **kwargs):
        kwargs.setdefault("schedule", False)
        self.binding = self.commands.install(client, None, bot_id=BOT, owner_home=self.anchor.home,
                                             store=self.store, **kwargs)
        return self.binding

    def guild(self, gid=GUILD):
        return self.store.read().guild(BOT, gid)

    def state(self, gid=GUILD):
        return self.store.read().guild_state(BOT, gid)

    def raw(self) -> str:
        return self.store.path.read_text() if self.store.path.exists() else ""


@pytest.fixture
def ui(anchor):
    config = anchor.mod("config")
    settings = {"operators": [str(OP_ID)]}
    config.set_reader(lambda home, key, default: settings.get(key, default))
    yield UI(anchor, settings)
    config.set_reader(None)


async def harness(ui: UI, **install) -> ReplayHarness:
    h = ReplayHarness()
    await h.start()
    h.seed_guild(GUILD_ID, name="Example Guild",
                 roles=[(ADMIN_ROLE_ID, "Operator", ADMINISTRATOR)])
    ui.install(h.client, **install)

    async def record(user_id, text):
        ui.dms.append((user_id, text))

    if ui.binding is not None:
        ui.binding.notifier = record
    return h


# ── helpers ──────────────────────────────────────────────────────────────────────────────────────────


def assert_all_ephemeral(sent) -> None:
    """Every new message (type 4/5, followups) carries the ephemeral flag; updates (7) edit an ephemeral panel."""
    for request in sent:
        if request.callback_type in (4, 5):
            assert request.data.get("flags", 0) & EPHEMERAL, request.payload
        elif request.path == "/webhooks/{webhook_id}/{webhook_token}":
            assert request.payload.get("flags", 0) & EPHEMERAL, request.payload


def components(request) -> list:
    return request.data.get("components") or []


def controls(request) -> list:
    out = []
    for row in components(request):
        for c in row.get("components", []):
            out.append(c)
    return out


def labels(request) -> list:
    return [c.get("label") for c in controls(request) if c.get("type") == 2]


def custom_id(ui: UI, request, action: str, arg: Optional[str] = None) -> str:
    for c in controls(request):
        cid = c.get("custom_id", "")
        ref = ui.items.parse(cid)
        if ref.action == action and (arg is None or ref.arg == arg):
            return cid
    raise AssertionError(f"no control {action} in {labels(request)}")


def embed(request) -> dict:
    embeds = request.data.get("embeds") or []
    return embeds[0] if embeds else {}


def field_names(request) -> list:
    return [f["name"] for f in embed(request).get("fields", [])]


def panel_message(request) -> dict:
    return message_payload(components(request), flags=EPHEMERAL)


async def command(h, *, user, perms, page: Optional[str] = None, guild_id=GUILD_ID):
    options = [{"name": "page", "type": 3, "value": page}] if page else None
    return await h.replay(app_command_payload("policy", options=options, user_id=user, permissions=perms,
                                              guild_id=guild_id, command_guild_id=GUILD_ID))


async def click(h, message: dict, cid: str, *, user, perms, guild_id=GUILD_ID, values=None, roles=()):
    data: dict[str, Any] = {"custom_id": cid, "component_type": 3 if values is not None else 2}
    if values is not None:
        data["values"] = values
    return await h.replay(interaction_payload(3, data, message=message, user_id=user, permissions=perms,
                                              guild_id=guild_id, roles=roles))


def forged_message(cid: str, *, select: bool = False) -> dict:
    """A message carrying one control with ``cid`` (what a modified client could send)."""
    if select:
        row = {"type": 1, "components": [{"type": 3, "custom_id": cid, "options": [
            {"label": "x", "value": "rotate"}], "min_values": 1, "max_values": 1}]}
    else:
        row = {"type": 1, "components": [{"type": 2, "style": 1, "label": "x", "custom_id": cid}]}
    return message_payload([row], flags=EPHEMERAL)


def activate_directly(ui: UI, gid=GUILD) -> None:
    ui.ops.bind(ui.store, BOT, gid, by="cli")


# ── registration ─────────────────────────────────────────────────────────────────────────────────────


async def test_policy_is_a_guild_command_with_manage_server_default(ui):
    h = await harness(ui)
    try:
        cmd = h.tree.get_command("policy", guild=discord.Object(GUILD_ID))
        assert cmd is not None and h.tree.get_commands() == []          # no global command
        payload = cmd.to_dict(h.tree)
        assert payload["default_member_permissions"] == discord.Permissions(manage_guild=True).value
        assert payload["contexts"] == [0]                                  # guild only
        page = payload["options"][0]
        assert page["name"] == "page" and not page["required"]
        assert {c["value"] for c in page["choices"]} >= {"home", "status", "progress"}
        assert ui.anchor.RUNTIME.status.get("K6").outcome.value == "ok"
    finally:
        await h.close()


async def test_sync_is_guild_scoped_and_only_on_signature_change(ui, monkeypatch):
    h = await harness(ui)
    try:
        # Offline the real sync reaches the HTTP layer: a guild-scoped bulk overwrite, never a global one.
        result = await ui.binding.sync_guild(GUILD_ID)
        assert result.startswith("failed")
        puts = [r for r in h.requests if r.transport == "http"]
        assert [(r.method, r.path) for r in puts] == [("PUT", "/applications/{application_id}/guilds/{guild_id}/commands")]
        assert puts[0].route_params["guild_id"] == GUILD_ID
        assert [c["name"] for c in puts[0].payload] == ["policy"]
        assert not ui.binding.sync_path.exists()                            # a failed sync is retried later

        calls = []

        async def fake_sync(*, guild=None):
            calls.append(guild.id if guild else None)
            return []

        monkeypatch.setattr(h.tree, "sync", fake_sync)
        assert await ui.binding.sync_guild(GUILD_ID) == "synced"
        assert await ui.binding.sync_guild(GUILD_ID) == "unchanged"
        assert await ui.binding.sync_all() == {GUILD: "unchanged"}
        assert calls == [GUILD_ID]
        stored = json.loads(ui.binding.sync_path.read_text())
        assert stored[BOT][GUILD]["fingerprint"] == ui.binding.fingerprint(GUILD_ID)

        # A changed signature (e.g. a new panel choice) syncs again; joining a server always syncs.
        ui.binding.command = ui.binding.command.__class__(
            name="policy", description="changed", callback=ui.binding.command.callback)
        assert await ui.binding.sync_guild(GUILD_ID) == "synced"
        await ui.binding._on_guild_join(discord.Object(OTHER_GUILD_ID))
        assert calls == [GUILD_ID, GUILD_ID, OTHER_GUILD_ID]
        assert h.tree.get_command("policy", guild=discord.Object(OTHER_GUILD_ID)) is not None
        await ui.binding._on_guild_remove(discord.Object(OTHER_GUILD_ID))
        assert h.tree.get_command("policy", guild=discord.Object(OTHER_GUILD_ID)) is None
        assert OTHER not in json.loads(ui.binding.sync_path.read_text())[BOT]
        assert h.tree.get_commands() == []
    finally:
        await h.close()


# ── /policy per role ─────────────────────────────────────────────────────────────────────────────────


async def test_policy_unbound_per_role(ui):
    h = await harness(ui)
    try:
        op = await command(h, user=OP_ID, perms=PLAIN)
        assert [r.callback_type for r in op] == [4]
        assert embed(op[0])["title"] == "Server policy — Example Guild"
        assert labels(op[0]) == ["Activate in this server", "Status"]

        manager = await command(h, user=ADMIN_USER_ID, perms=MANAGE)
        assert labels(manager[0]) == ["Ask the operator to activate", "Status"]
        assert "Only the bot's operator can activate" in embed(manager[0])["description"]

        # Administrator plus a role called "Operator": still a manager, never an operator.
        admin = await command(h, user=ADMIN_USER_ID, perms=ADMINISTRATOR)
        assert labels(admin[0]) == ["Ask the operator to activate", "Status"]

        member = await command(h, user=MEMBER_USER_ID, perms=PLAIN)
        assert [r.callback_type for r in member] == [4]
        assert "Manage Server" in member[0].data["content"] and "Next step" in member[0].data["content"]
        assert not member[0].data.get("components") and not member[0].data.get("embeds")
        assert_all_ephemeral(op + manager + admin + member)
        assert ui.raw() == ""                                               # opening never writes
        assert h.task_errors == []
    finally:
        await h.close()


async def test_policy_active_shows_locked_areas_only_to_the_operator(ui):
    activate_directly(ui)
    h = await harness(ui)
    try:
        op = (await command(h, user=OP_ID, perms=PLAIN))[0]
        manager = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        assert labels(op) == ["Progress", "Status", "Pause", "Unbind…"]
        assert labels(manager) == ["Progress", "Status", "Pause"]
        assert {"🔒 Worklog", "🔒 Members may", "🔒 Managed by"} <= set(field_names(op))
        assert not [n for n in field_names(manager) if n.startswith("🔒")]
        assert "Identity" in field_names(manager) and "Progress" in field_names(manager)
        assert embed(op)["description"].startswith("**Active**")
        assert_all_ephemeral([op, manager])
    finally:
        await h.close()


async def test_status_page_full_for_operator_summary_for_manager(ui):
    activate_directly(ui)
    h = await harness(ui)
    try:
        op = (await command(h, user=OP_ID, perms=PLAIN, page="status"))[0]
        manager = (await command(h, user=ADMIN_USER_ID, perms=MANAGE, page="status"))[0]
        assert "Interventions" in field_names(op) and "K6" in embed(op)["fields"][1]["value"]
        assert "Interventions" not in field_names(manager) and "Protection" in field_names(manager)
        assert labels(op) == labels(manager) == ["Back"]
        assert_all_ephemeral([op, manager])
    finally:
        await h.close()


async def test_operator_list_unreadable_is_shown_not_treated_as_empty(ui):
    h = await harness(ui)
    try:
        def broken(home, key, default):
            raise RuntimeError("config.yaml could not be read")

        ui.anchor.mod("config").set_reader(broken)
        sent = await command(h, user=OP_ID, perms=MANAGE)
        assert labels(sent[0]) == ["Ask the operator to activate", "Status"]   # nobody is operator now
        assert "operator list can't be read" in embed(sent[0])["description"]
        request = await click(h, panel_message(sent[0]), custom_id(ui, sent[0], "request"), user=ADMIN_USER_ID,
                              perms=MANAGE)
        assert "can't be read" in request[-1].payload["content"] and ui.dms == []
    finally:
        await h.close()


# ── activation ───────────────────────────────────────────────────────────────────────────────────────


async def test_operator_activates_with_confirmation(ui):
    h = await harness(ui)
    try:
        panel = (await command(h, user=OP_ID, perms=PLAIN))[0]
        confirm = await click(h, panel_message(panel), custom_id(ui, panel, "activate"), user=OP_ID, perms=PLAIN)
        assert [r.callback_type for r in confirm] == [7]
        assert labels(confirm[0]) == ["Activate", "Cancel"]
        assert ui.state() == "unbound"
        done = await click(h, panel_message(confirm[0]), custom_id(ui, confirm[0], "activate_ok"), user=OP_ID,
                           perms=PLAIN)
        assert [r.callback_type for r in done] == [7]
        assert embed(done[0])["description"].startswith("✅ Activated.")
        guild = ui.guild()
        assert guild["state"] == "active" and guild["bound_by"] == str(OP_ID) and guild["rev"] == 1
        assert h.task_errors == []
    finally:
        await h.close()


@pytest.mark.parametrize(("user", "perms", "text"), [
    (ADMIN_USER_ID, MANAGE, "Only the bot's operator can activate"),
    (ADMIN_USER_ID, ADMINISTRATOR, "Only the bot's operator can activate"),
    (MEMBER_USER_ID, PLAIN, "Manage Server"),
], ids=["manager", "administrator", "member"])
async def test_only_the_operator_can_activate(ui, user, perms, text):
    h = await harness(ui)
    try:
        cid = ui.items.encode(GUILD, "activate_ok", "", 0)
        sent = await click(h, forged_message(cid), cid, user=user, perms=perms, roles=[ADMIN_ROLE_ID])
        assert [r.callback_type for r in sent] == [4]
        assert text in sent[0].data["content"] and "Next step" in sent[0].data["content"]
        assert_all_ephemeral(sent)
        assert ui.raw() == "" and ui.guild() is None
    finally:
        await h.close()


async def test_operator_removed_from_config_is_refused_on_the_next_click(ui):
    """Rights are checked on every interaction, not when the panel was opened."""
    h = await harness(ui)
    try:
        panel = (await command(h, user=OP_ID, perms=PLAIN))[0]
        confirm = await click(h, panel_message(panel), custom_id(ui, panel, "activate"), user=OP_ID, perms=PLAIN)
        ui.settings["operators"] = []
        sent = await click(h, panel_message(confirm[0]), custom_id(ui, confirm[0], "activate_ok"), user=OP_ID,
                           perms=PLAIN)
        assert [r.callback_type for r in sent] == [4]
        assert "Manage Server" in sent[0].data["content"]
        assert ui.guild() is None
    finally:
        await h.close()


# ── activation request ───────────────────────────────────────────────────────────────────────────────


async def test_manager_asks_the_operator_throttled(ui):
    h = await harness(ui)
    try:
        panel = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        cid = custom_id(ui, panel, "request")
        first = await click(h, panel_message(panel), cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert first[0].callback_type == 5 and first[0].data["flags"] & EPHEMERAL      # deferred, thinking
        assert first[-1].payload["content"].startswith("✅ Request sent")
        assert ui.dms == [(str(OP_ID), ui.dms[0][1])]
        assert f"<@{ADMIN_USER_ID}>" in ui.dms[0][1] and "Example Guild" in ui.dms[0][1]
        assert f"(id {GUILD})" in ui.dms[0][1]
        assert f"hermes ctx-discord-policy bind {GUILD} --bot {BOT}" in ui.dms[0][1]   # reachable without /policy
        second = await click(h, panel_message(panel), cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert "already asked" in second[-1].payload["content"]
        assert len(ui.dms) == 1
        assert_all_ephemeral(first + second)
        assert ui.raw() == ""                                               # no policy change
    finally:
        await h.close()


async def test_request_through_discord_dm_failure_releases_the_slot(ui):
    h = await harness(ui)
    try:
        ui.binding.notifier = None                                          # the real direct-message path
        panel = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        sent = await click(h, panel_message(panel), custom_id(ui, panel, "request"), user=ADMIN_USER_ID,
                           perms=MANAGE)
        attempts = [r for r in sent if r.transport == "http"]
        assert attempts and attempts[0].path == "/users/{user_id}" and str(OP_ID) in attempts[0].url
        assert "could not be delivered" in sent[-1].payload["content"]
        assert ui.binding.requests.claim(BOT, GUILD)[0]                     # slot was released
    finally:
        await h.close()


async def test_request_refused_for_operator_and_member_and_without_operators(ui):
    h = await harness(ui)
    try:
        cid = ui.items.encode(GUILD, "request", "", 0)
        op = await click(h, forged_message(cid), cid, user=OP_ID, perms=PLAIN)
        assert "activate the policy directly" in op[0].data["content"]
        member = await click(h, forged_message(cid), cid, user=MEMBER_USER_ID, perms=PLAIN)
        assert "Manage Server" in member[0].data["content"]
        ui.settings["operators"] = []
        none = await click(h, forged_message(cid), cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert "no operator yet" in none[-1].payload["content"]
        ui.settings.update(operators=[str(OP_ID)], notify_operators=False)
        off = await click(h, forged_message(cid), cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert "does not take activation requests" in off[-1].payload["content"]
        assert ui.dms == []
        assert_all_ephemeral(op + member + none + off)
    finally:
        await h.close()


# ── pause / resume / unbind ──────────────────────────────────────────────────────────────────────────


async def test_manager_pauses_and_resumes(ui):
    activate_directly(ui)
    h = await harness(ui)
    try:
        panel = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        paused = await click(h, panel_message(panel), custom_id(ui, panel, "pause"), user=ADMIN_USER_ID,
                             perms=MANAGE)
        assert [r.callback_type for r in paused] == [7]
        assert ui.state() == "paused" and ui.guild()["updated_by"] == str(ADMIN_USER_ID)
        assert "Resume" in labels(paused[0]) and "Pause" not in labels(paused[0])
        resumed = await click(h, panel_message(paused[0]), custom_id(ui, paused[0], "resume"),
                              user=ADMIN_USER_ID, perms=MANAGE)
        assert ui.state() == "active"
        assert embed(resumed[0])["description"].startswith("▶️ Resumed.")
        assert h.task_errors == []
    finally:
        await h.close()


async def test_member_cannot_pause_and_manager_cannot_resume_an_unbound_server(ui):
    activate_directly(ui)
    h = await harness(ui)
    try:
        before = ui.raw()
        cid = ui.items.encode(GUILD, "pause", "", 1)
        sent = await click(h, forged_message(cid), cid, user=MEMBER_USER_ID, perms=PLAIN)
        assert "Manage Server" in sent[0].data["content"] and ui.raw() == before
        ui.ops.set_state(ui.store, BOT, GUILD, "unbound", by="cli")
        before = ui.raw()
        cid = ui.items.encode(GUILD, "resume", "", 2)
        sent = await click(h, forged_message(cid), cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert ui.state() == "unbound" and ui.raw() == before
        assert_all_ephemeral(sent)
    finally:
        await h.close()


async def test_operator_unbinds_or_deletes_with_confirmation(ui):
    activate_directly(ui)
    h = await harness(ui)
    try:
        panel = (await command(h, user=OP_ID, perms=PLAIN))[0]
        confirm = await click(h, panel_message(panel), custom_id(ui, panel, "unbind"), user=OP_ID, perms=PLAIN)
        assert labels(confirm[0]) == ["Unbind", "Unbind and delete settings", "Cancel"]
        assert ui.state() == "active"
        await click(h, panel_message(confirm[0]), custom_id(ui, confirm[0], "unbind_ok"), user=OP_ID, perms=PLAIN)
        assert ui.state() == "unbound" and ui.guild() is not None

        panel = (await command(h, user=OP_ID, perms=PLAIN))[0]
        assert labels(panel) == ["Activate in this server", "Status"]
        ui.ops.bind(ui.store, BOT, GUILD, by="cli")
        panel = (await command(h, user=OP_ID, perms=PLAIN))[0]
        confirm = await click(h, panel_message(panel), custom_id(ui, panel, "unbind"), user=OP_ID, perms=PLAIN)
        done = await click(h, panel_message(confirm[0]), custom_id(ui, confirm[0], "purge_ok"), user=OP_ID,
                           perms=PLAIN)
        assert ui.guild() is None
        assert "settings deleted" in embed(done[0])["description"]
    finally:
        await h.close()


@pytest.mark.parametrize("action", ["unbind_ok", "purge_ok", "unbind"])
async def test_manager_cannot_unbind(ui, action):
    activate_directly(ui)
    h = await harness(ui)
    try:
        before = ui.raw()
        cid = ui.items.encode(GUILD, action, "", 1)
        sent = await click(h, forged_message(cid), cid, user=ADMIN_USER_ID, perms=ADMINISTRATOR,
                           roles=[ADMIN_ROLE_ID])
        assert [r.callback_type for r in sent] == [4]
        assert "Only the bot's operator" in sent[0].data["content"]
        assert ui.raw() == before and ui.state() == "active"
    finally:
        await h.close()


# ── manipulated custom ids ───────────────────────────────────────────────────────────────────────────


async def test_custom_id_of_another_server_changes_nothing(ui):
    activate_directly(ui)
    activate_directly(ui, OTHER)
    h = await harness(ui)
    try:
        before = ui.raw()
        for action, rev in (("pause", 1), ("unbind_ok", 1), ("purge_ok", 1)):
            cid = ui.items.encode(OTHER, action, "", rev)
            sent = await click(h, forged_message(cid), cid, user=OP_ID, perms=PLAIN)   # clicked in GUILD
            assert [r.callback_type for r in sent] == [4]
            assert "different server" in sent[0].data["content"]
        assert ui.raw() == before
        assert ui.state() == ui.state(OTHER) == "active"
    finally:
        await h.close()


@pytest.mark.parametrize(("cid", "text"), [
    (f"dsp:1:{GUILD}:zz_unknown::0", "not known to this version"),
    (f"dsp:0:{GUILD}:pause::1", "outdated panel"),
    (f"dsp:1:{GUILD}:pause:x:1:extra", "outdated panel"),
    (f"dsp:1:{GUILD}:Pause::1", "outdated panel"),
    ("dsp:1:../../etc:pause::1", "outdated panel"),
    (f"dsp:1:{GUILD}:nav:worklog:0", "not known"),
], ids=["unknown-action", "old-version", "extra-field", "bad-action", "bad-guild", "unknown-page"])
async def test_malformed_or_unknown_custom_ids_are_answered_and_change_nothing(ui, cid, text):
    activate_directly(ui)
    h = await harness(ui)
    try:
        before = ui.raw()
        sent = await click(h, forged_message(cid), cid, user=OP_ID, perms=PLAIN)
        assert [r.callback_type for r in sent] == [4]
        assert text in sent[0].data["content"]
        assert_all_ephemeral(sent)
        assert ui.raw() == before
    finally:
        await h.close()


async def test_custom_ids_stay_within_discord_limits(ui):
    longest = ui.items.encode("9" * 20, "a" * 16, "b" * 32, 10 ** 12 - 1)
    assert len(longest) <= 100
    h = await harness(ui)
    try:
        activate_directly(ui)
        for user, perms in ((OP_ID, PLAIN), (ADMIN_USER_ID, MANAGE)):
            panel = (await command(h, user=user, perms=perms))[0]
            for c in controls(panel):
                assert len(c["custom_id"]) <= 100
                assert ui.items.parse(c["custom_id"]).guild == GUILD
    finally:
        await h.close()


# ── the example panel: progress display ──────────────────────────────────────────────────────────────


async def open_progress(ui, h, user, perms):
    panel = (await command(h, user=user, perms=perms))[0]
    page = await click(h, panel_message(panel), custom_id(ui, panel, "nav", "progress"), user=user, perms=perms)
    assert [r.callback_type for r in page] == [7]
    return page[0]


def radio_submit(modal_request, value, *, message, user, perms, guild_id=GUILD_ID):
    label = modal_request.data["components"][0]
    radio = label["component"]
    data = {"custom_id": modal_request.data["custom_id"], "components": [
        {"type": 18, "id": 1, "component": {"type": radio["type"], "id": 2, "custom_id": radio["custom_id"],
                                            "value": value}}]}
    return interaction_payload(5, data, message=message, user_id=user, permissions=perms, guild_id=guild_id)


@pytest.mark.parametrize(("user", "perms"), [(ADMIN_USER_ID, MANAGE), (OP_ID, PLAIN)], ids=["manager", "operator"])
async def test_progress_panel_modal_full_strategy(ui, user, perms):
    activate_directly(ui)
    h = await harness(ui)
    try:
        page = await open_progress(ui, h, user, perms)
        assert embed(page)["title"] == "Progress display for tool work"
        assert "● **Hermes default**" in embed(page)["fields"][0]["value"]
        assert labels(page) == ["Change…", "Back"]
        opened = await click(h, panel_message(page), custom_id(ui, page, "prog_edit"), user=user, perms=perms)
        assert [r.callback_type for r in opened] == [9]
        modal = opened[0].data
        assert len(modal["title"]) <= 45 and len(modal["custom_id"]) <= 100
        label = modal["components"][0]
        assert label["type"] == 18 and label["component"]["type"] == 21           # Label + RadioGroup
        assert [o["value"] for o in label["component"]["options"]] == ["default", "rotate", "separate"]
        assert [o.get("default", False) for o in label["component"]["options"]] == [True, False, False]

        saved = await h.replay(radio_submit(opened[0], "rotate", message=panel_message(page), user=user,
                                            perms=perms))
        assert [r.callback_type for r in saved] == [7]
        assert embed(saved[0])["description"].startswith("✅ Saved: Newest line only.")
        guild = ui.guild()
        assert guild["progress"]["grouping"] == "rotate" and guild["rev"] == 2
        assert guild["updated_by"] == str(user)
        audit = json.loads(ui.raw())["audit"][-1]
        assert audit["field"] == "progress.grouping" and audit["by"] == str(user)
        assert h.task_errors == []
    finally:
        await h.close()


async def test_progress_modal_rechecks_rights_submitter_and_value(ui):
    activate_directly(ui)
    h = await harness(ui)
    try:
        page = await open_progress(ui, h, ADMIN_USER_ID, MANAGE)
        cid = custom_id(ui, page, "prog_edit")
        msg = panel_message(page)

        opened = await click(h, msg, cid, user=ADMIN_USER_ID, perms=MANAGE)
        before = ui.raw()
        lost = await h.replay(radio_submit(opened[0], "rotate", message=msg, user=ADMIN_USER_ID, perms=PLAIN))
        assert "Manage Server" in lost[0].data["content"] and ui.raw() == before   # rights gone since opening

        opened = await click(h, msg, cid, user=ADMIN_USER_ID, perms=MANAGE)
        other = await h.replay(radio_submit(opened[0], "rotate", message=msg, user=OP_ID, perms=PLAIN))
        assert "belongs to someone else" in other[0].data["content"] and ui.raw() == before

        opened = await click(h, msg, cid, user=ADMIN_USER_ID, perms=MANAGE)
        bogus = await h.replay(radio_submit(opened[0], "accumulate", message=msg, user=ADMIN_USER_ID,
                                            perms=MANAGE))
        assert "not available" in bogus[0].data["content"] and ui.raw() == before
        assert_all_ephemeral(lost + other + bogus)
    finally:
        await h.close()


async def test_progress_panel_classic_strategy_uses_a_select(ui):
    activate_directly(ui)
    h = await harness(ui, mode="classic")
    try:
        assert ui.anchor.RUNTIME.status.get("K6").outcome.value == "fallback"
        page = await open_progress(ui, h, ADMIN_USER_ID, MANAGE)
        select = [c for c in controls(page) if c["type"] == 3]
        assert len(select) == 1 and [o["value"] for o in select[0]["options"]] == ["default", "rotate", "separate"]
        saved = await click(h, panel_message(page), select[0]["custom_id"], user=ADMIN_USER_ID, perms=MANAGE,
                            values=["separate"])
        assert [r.callback_type for r in saved] == [7]
        assert ui.guild()["progress"]["grouping"] == "separate"
    finally:
        await h.close()


async def test_progress_select_refused_for_members_and_stale_panels(ui):
    activate_directly(ui)
    h = await harness(ui, mode="classic")
    try:
        page = await open_progress(ui, h, ADMIN_USER_ID, MANAGE)
        select = [c for c in controls(page) if c["type"] == 3][0]["custom_id"]
        before = ui.raw()
        member = await click(h, panel_message(page), select, user=MEMBER_USER_ID, perms=PLAIN, values=["rotate"])
        assert "Manage Server" in member[0].data["content"] and ui.raw() == before

        # Someone else saves first: the stale panel is reloaded with the reason, nothing is overwritten.
        ui.ops.update_fields(ui.store, BOT, GUILD, {"progress.grouping": "separate"}, by=str(OP_ID))
        stale = await click(h, panel_message(page), select, user=ADMIN_USER_ID, perms=MANAGE, values=["rotate"])
        assert [r.callback_type for r in stale] == [7]
        description = embed(stale[0])["description"]
        assert description.startswith("⚠️ Not saved:") and f"<@{OP_ID}>" in description
        assert ui.guild()["progress"]["grouping"] == "separate"
        fresh = [c for c in controls(stale[0]) if c["type"] == 3][0]
        assert ui.items.parse(fresh["custom_id"]).rev == ui.guild()["rev"]
        assert [o.get("default", False) for o in fresh["options"]] == [False, False, True]
    finally:
        await h.close()


# ── persistence across a restart, and the non-persistent fallback ────────────────────────────────────


async def test_controls_survive_a_restart(ui):
    activate_directly(ui)
    h = await harness(ui)
    try:
        panel = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
    finally:
        await h.close()
    message = panel_message(panel)
    cid = custom_id(ui, panel, "pause")

    restarted = ReplayHarness()                       # a new process: fresh client, nothing in memory
    await restarted.start()
    try:
        restarted.seed_guild(GUILD_ID, name="Example Guild")
        assert await click(restarted, message, cid, user=ADMIN_USER_ID, perms=MANAGE) == []   # not installed yet
        ui.install(restarted.client)
        sent = await click(restarted, message, cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert [r.callback_type for r in sent] == [7]
        assert ui.state() == "paused"
        assert restarted.task_errors == []
    finally:
        await restarted.close()


async def test_views_strategy_without_dynamic_items(ui):
    activate_directly(ui)
    h = await harness(ui, mode="views")
    try:
        assert ui.binding.persistent is False
        assert not h.state._view_store._dynamic_items                    # nothing persistent registered
        panel = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        stored = [mid for mid, info in h.state._view_store._views.items() if mid is not None and info]
        assert len(stored) == 1                                           # a live view with a timeout
        view = h.state._view_store._synced_message_views[stored[0]]
        assert view.timeout == ui.panels.VIEW_TIMEOUT
        message = message_payload(components(panel), message_id=stored[0], flags=EPHEMERAL)
        sent = await click(h, message, custom_id(ui, panel, "pause"), user=ADMIN_USER_ID, perms=MANAGE)
        assert [r.callback_type for r in sent] == [7] and ui.state() == "paused"
    finally:
        await h.close()


# ── the dispatcher refuses before any handler runs (independent of the write-path checks) ───────────


async def test_dispatcher_checks_rights_before_any_handler(ui):
    """Panels S9 adds may have handlers that do not write; the per-click check must not rely on the
    write path. A probe action and a probe form on an operator-only setting record every call."""
    activate_directly(ui)
    panels, modals = ui.panels, ui.anchor.mod("ui.modals")
    calls = []

    @panels.action("zz_probe", "worklog")
    async def probe(ctx):
        calls.append(("click", ctx.actor.role))
        await ctx.say("probe ran")

    @panels.modal_action("m_zz_probe", "worklog")
    async def probe_form(ctx, values):
        calls.append(("form", ctx.actor.role, values))
        await ctx.say("probe form ran")

    h = await harness(ui)
    try:
        cid = ui.items.encode(GUILD, "zz_probe", "", 1)
        refused = await click(h, forged_message(cid), cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert "Only the bot's operator" in refused[0].data["content"] and calls == []
        await click(h, forged_message(cid), cid, user=OP_ID, perms=PLAIN)
        assert calls == [("click", "operator")]

        spec = modals.ModalSpec("m_zz_probe", "Probe", [modals.Field("text", "text", "Text")])
        for opener, perms_, expected in ((str(ADMIN_USER_ID), MANAGE, None), (str(OP_ID), PLAIN, "form")):
            form = modals.build(discord, spec, guild_id=GUILD, rev=1, opener_id=opener, modern=True)
            h.state.store_view(form)
            data = {"custom_id": form.custom_id, "components": [
                {"type": 18, "id": 1, "component": {"type": 4, "id": 2, "custom_id": "text", "value": "x"}}]}
            sent = await h.replay(interaction_payload(5, data, user_id=int(opener), permissions=perms_))
            assert_all_ephemeral(sent)
            if expected is None:
                assert "Only the bot's operator" in sent[0].data["content"]
        assert calls == [("click", "operator"), ("form", "operator", {"text": "x"})]
    finally:
        await h.close()


@pytest.mark.parametrize(("damage", "text"), [
    ("corrupt", "policy file on the host is damaged"),
    ("newer", "written by a newer version"),
], ids=["corrupt", "newer-schema"])
async def test_damaged_or_newer_store_is_shown_and_never_written(ui, damage, text):
    activate_directly(ui)
    h = await harness(ui)
    try:
        panel = (await command(h, user=OP_ID, perms=PLAIN))[0]
        raw = json.loads(ui.raw())
        if damage == "corrupt":
            raw["bots"][BOT]["guilds"][GUILD]["progress"]["grouping"] = "accumulate"
        else:
            raw["schema"] = 99
        ui.store.path.write_text(json.dumps(raw))
        before = ui.raw()
        home = (await command(h, user=OP_ID, perms=PLAIN))[0]
        assert text in embed(home)["description"] and labels(home) == ["Status"]
        sent = await click(h, panel_message(panel), custom_id(ui, panel, "pause"), user=OP_ID, perms=PLAIN)
        assert [r.callback_type for r in sent] == [4]
        assert "can't be changed right now" in sent[0].data["content"] and ui.raw() == before
        assert_all_ephemeral(sent + [home])
    finally:
        await h.close()


# ── review follow-ups: slow writes, the non-persistent fallback, unbound servers, failed installs ────


async def test_slow_write_is_deferred_and_then_updates_the_panel(ui, monkeypatch):
    import time as _time

    activate_directly(ui)
    h = await harness(ui, mode="classic")
    try:
        page = await open_progress(ui, h, ADMIN_USER_ID, MANAGE)
        select = [c for c in controls(page) if c["type"] == 3][0]["custom_id"]
        real = ui.ops.update_fields

        def slow(*args, **kwargs):
            _time.sleep(0.3)                       # e.g. the CLI holds the store lock
            return real(*args, **kwargs)

        monkeypatch.setattr(ui.ops, "update_fields", slow)
        monkeypatch.setattr(ui.panels, "DEFER_AFTER", 0.05)
        sent = await click(h, panel_message(page), select, user=ADMIN_USER_ID, perms=MANAGE, values=["rotate"])
        assert sent[0].callback_type == 6                                   # acknowledged in time
        assert (sent[1].method, sent[1].path) == ("PATCH", "/webhooks/{webhook_id}/{webhook_token}/messages/@original")
        assert sent[1].payload["embeds"][0]["description"].startswith("✅ Saved: Newest line only.")
        assert ui.guild()["progress"]["grouping"] == "rotate" and h.task_errors == []
    finally:
        await h.close()


async def test_views_fallback_keeps_the_new_panel_working_after_the_old_view_ends(ui):
    activate_directly(ui)
    h = await harness(ui, mode="views")
    try:
        panel = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        store = h.state._view_store
        message_id = [mid for mid, info in store._views.items() if mid is not None and info][0]
        first = store._synced_message_views[message_id]
        message = message_payload(components(panel), message_id=message_id, flags=EPHEMERAL)
        page = await click(h, message, custom_id(ui, panel, "nav", "progress"), user=ADMIN_USER_ID, perms=MANAGE)
        back = await click(h, message_payload(components(page[0]), message_id=message_id, flags=EPHEMERAL),
                           custom_id(ui, page[0], "nav", "home"), user=ADMIN_USER_ID, perms=MANAGE)
        assert first.is_finished()                                          # retired when it was replaced
        first.stop()                                                        # its timeout firing late is harmless
        current = message_payload(components(back[0]), message_id=message_id, flags=EPHEMERAL)
        sent = await click(h, current, custom_id(ui, back[0], "pause"), user=ADMIN_USER_ID, perms=MANAGE)
        assert [r.callback_type for r in sent] == [7] and ui.state() == "paused"
    finally:
        await h.close()


async def test_settings_of_an_unbound_server_are_not_written(ui):
    activate_directly(ui)
    ui.ops.set_state(ui.store, BOT, GUILD, "unbound", by="cli")             # settings kept
    h = await harness(ui, mode="classic")
    try:
        page = (await command(h, user=ADMIN_USER_ID, perms=MANAGE, page="progress"))[0]
        select = [c for c in controls(page) if c["type"] == 3][0]["custom_id"]
        before = ui.raw()
        sent = await click(h, panel_message(page), select, user=ADMIN_USER_ID, perms=MANAGE, values=["rotate"])
        assert "not active in this server" in sent[0].data["content"] and ui.raw() == before
    finally:
        await h.close()


async def test_failed_install_leaves_nothing_behind(ui, monkeypatch):
    h = ReplayHarness()
    await h.start()
    try:
        h.seed_guild(GUILD_ID, name="Example Guild")

        def broken(*items):
            raise RuntimeError("synthetic failure")

        monkeypatch.setattr(h.client, "add_dynamic_items", broken)
        assert ui.install(h.client) is None
        result = ui.anchor.RUNTIME.status.get("K6")
        assert result.outcome.value == "off" and "CLI" in result.reason
        assert ui.items.binding_for(h.client) is None
        assert h.tree.get_command("policy", guild=discord.Object(GUILD_ID)) is None
        assert await command(h, user=OP_ID, perms=PLAIN) == []
    finally:
        await h.close()
