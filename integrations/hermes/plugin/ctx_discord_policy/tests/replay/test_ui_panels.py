"""S9 L3: the settings panels of /policy, replayed through discord.py's real dispatch, per role.

Identity, channels, worklog, member tools, operator turns, managers and the server guidance (form, upload,
preview, history, reset), plus the audit channel and the operators' direct messages.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

discord = pytest.importorskip("discord")

from tests.replay.harness import (  # noqa: E402
    ADMIN_USER_ID,
    APP_ID,
    CHANNEL_ID,
    GUILD_ID,
    MEMBER_USER_ID,
    ReplayHarness,
    interaction_payload,
    message_payload,
    role_payload,
)
from tests.replay.test_ui_replay import (  # noqa: E402,F401  (ui is a fixture)
    BOT,
    EPHEMERAL,
    GUILD,
    MANAGE,
    OP_ID,
    PLAIN,
    activate_directly,
    assert_all_ephemeral,
    click,
    command,
    components,
    controls,
    custom_id,
    embed,
    field_names,
    forged_message,
    labels,
    panel_message,
    ui,
)

pytestmark = [pytest.mark.asyncio, pytest.mark.replay]

PUBLIC_CHANNEL = CHANNEL_ID                     # where /policy is used ("general")
WORK_CHANNEL = 444444444444444445               # visible to operators only
OTHER_CHANNEL = 444444444444444446
FOREIGN_CHANNEL = 999999999999999990            # not in this server
AUDIT_CHANNEL = 444444444444444447
ROLE_A, ROLE_B = 777777777777777771, 777777777777777772
FOREIGN_ROLE = 777777777777777779
EMOJIS = [(880000000000000000 + i, f"emoji_{i:02d}") for i in range(30)]
VIEW_CHANNEL = discord.Permissions(view_channel=True).value


def _channel(cid: int, name: str, *, private: bool = False, position: int = 0) -> dict:
    overwrites = [{"id": str(GUILD_ID), "type": 0, "allow": "0", "deny": str(VIEW_CHANNEL)}] if private else []
    return {"id": str(cid), "type": 0, "name": name, "position": position, "parent_id": None, "topic": None,
            "nsfw": False, "rate_limit_per_user": 0, "last_message_id": None, "permission_overwrites": overwrites,
            "flags": 0}


def seed(h: ReplayHarness, *, emojis=EMOJIS) -> discord.Guild:
    data = {
        "id": str(GUILD_ID), "name": "Example Guild", "owner_id": str(APP_ID),
        "roles": [role_payload(GUILD_ID, "@everyone", VIEW_CHANNEL, position=0),
                  role_payload(ROLE_A, "moderators", MANAGE, position=1),
                  role_payload(ROLE_B, "helpers", PLAIN, position=2)],
        "emojis": [{"id": str(eid), "name": name, "animated": False, "available": True, "require_colons": True,
                    "managed": False, "roles": []} for eid, name in emojis],
        "stickers": [], "features": [], "member_count": 3, "preferred_locale": "en-US",
        "channels": [_channel(PUBLIC_CHANNEL, "general"), _channel(WORK_CHANNEL, "work-log", private=True, position=1),
                     _channel(OTHER_CHANNEL, "memes", position=2), _channel(AUDIT_CHANNEL, "policy-audit", position=3)],
    }
    guild = discord.Guild(data=data, state=h.state)  # type: ignore[arg-type]
    h.state._add_guild(guild)
    return guild


async def panel_harness(ui, **install) -> ReplayHarness:
    h = ReplayHarness()
    await h.start()
    seed(h)
    ui.install(h.client, **install)
    ui.audit_posts = []

    async def record(user_id, text, file=None):
        ui.dms.append((user_id, text))
        ui.files.append(file)

    async def post(channel_id, text):
        ui.audit_posts.append((channel_id, text))

    ui.binding.notifier = record
    ui.binding.channel_sink = post
    return h


async def open_panel(ui, h, key, *, user=OP_ID, perms=PLAIN):
    home = (await command(h, user=user, perms=perms))[0]
    page = await click(h, panel_message(home), custom_id(ui, home, "nav", key), user=user, perms=perms)
    assert [r.callback_type for r in page] == [7], page
    return page[0]


def select_payload(request, action, values, *, component_type, user, perms, resolved=None, arg=None):
    cid = custom_id_for(request, action, arg)
    data: dict[str, Any] = {"custom_id": cid, "component_type": component_type, "values": [str(v) for v in values]}
    if resolved is not None:
        data["resolved"] = resolved
    return interaction_payload(3, data, message=panel_message(request), user_id=user, permissions=perms)


def custom_id_for(request, action, arg=None):
    for c in controls(request):
        cid = c.get("custom_id", "")
        parts = cid.split(":")
        if len(parts) == 6 and parts[3] == action and (arg is None or parts[4] == arg):
            return cid
    raise AssertionError(f"no control {action} in {[c.get('custom_id') for c in controls(request)]}")


def channels_resolved(*ids):
    return {"channels": {str(i): {"id": str(i), "type": 0, "name": f"c{i}", "permissions": "0"} for i in ids}}


def roles_resolved(*ids):
    return {"roles": {str(i): role_payload(i, f"r{i}", 0) for i in ids}}


def modal_submit(opened, values: dict, *, message, user, perms, resolved=None):
    """Submit the modal recorded in ``opened`` with ``values`` keyed by input custom_id."""
    comps = []
    for n, row in enumerate(opened.data["components"], start=1):
        inner = row.get("component") or (row.get("components") or [{}])[0]
        key = inner["custom_id"]
        if key not in values:
            continue
        value = values[key]
        item: dict[str, Any] = {"type": inner["type"], "id": n * 10, "custom_id": key}
        if inner["type"] == 19:
            item["values"] = [str(v) for v in value]
        else:
            item["value"] = value
        comps.append({"type": 18, "id": n, "component": item} if "component" in row else
                     {"type": 1, "id": n, "components": [item]})
    data: dict[str, Any] = {"custom_id": opened.data["custom_id"], "components": comps}
    if resolved is not None:
        data["resolved"] = resolved
    return interaction_payload(5, data, message=message, user_id=user, permissions=perms)


def store_guild(ui):
    return ui.guild()


# ── visibility per role ──────────────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize("key", ["worklog", "lanes", "operator_lane", "managers"])
async def test_operator_only_panels_are_hidden_from_and_refused_to_managers(ui, key):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        home = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        assert not [c for c in controls(home) if c.get("custom_id", "").endswith(f":nav:{key}:1")]
        cid = ui.items.encode(GUILD, "nav", key, 1)
        sent = await click(h, forged_message(cid), cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert "Only the bot's operator" in sent[0].data["content"]
        page = await open_panel(ui, h, key)                                  # the operator gets it
        assert embed(page)["title"]
        assert_all_ephemeral(sent)
    finally:
        await h.close()


@pytest.mark.parametrize(("action", "arg"), [
    ("wl_final", "off"), ("wl_archive", ""), ("wl_channel", ""), ("ln_history", "off"), ("ln_links", "off"),
    ("ln_limit", ""), ("ln_skills", ""), ("ln_knowledge", ""), ("ol_hydration", "off"), ("ol_guidance", "on"),
    ("ol_guidance_ask", ""), ("mg_mode", ""), ("mg_roles", ""), ("mg_audit", ""),
])
async def test_managers_cannot_change_operator_settings(ui, action, arg):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        before = ui.raw()
        cid = ui.items.encode(GUILD, action, arg, 1)
        sent = await click(h, forged_message(cid), cid, user=ADMIN_USER_ID, perms=MANAGE)
        assert [r.callback_type for r in sent] == [4]
        assert "Only the bot's operator" in sent[0].data["content"] and "Next step" in sent[0].data["content"]
        assert ui.raw() == before
    finally:
        await h.close()


@pytest.mark.parametrize(("action", "arg"), [("id_edit", ""), ("ch_set", "ambient"), ("gd_edit", ""),
                                             ("gd_reset_ok", ""), ("gd_restore", "a" * 32)])
async def test_members_cannot_change_manager_settings(ui, action, arg):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        before = ui.raw()
        cid = ui.items.encode(GUILD, action, arg, 1)
        sent = await click(h, forged_message(cid), cid, user=MEMBER_USER_ID, perms=PLAIN)
        assert "Manage Server" in sent[0].data["content"] and ui.raw() == before
    finally:
        await h.close()


# ── identity ─────────────────────────────────────────────────────────────────────────────────────────


async def test_identity_form_per_role(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "identity", user=ADMIN_USER_ID, perms=MANAGE)
        opened = await click(h, panel_message(page), custom_id_for(page, "id_edit"), user=ADMIN_USER_ID, perms=MANAGE)
        assert [r.callback_type for r in opened] == [9] and len(opened[0].data["title"]) <= 45
        assert [row["component"]["custom_id"] for row in opened[0].data["components"]] == [
            "bot_name", "wake_names", "greetings", "language"]
        values = {"bot_name": "Nova", "wake_names": "nova, nov, nova", "greetings": "hey, hallo",
                  "language": "German"}
        saved = await h.replay(modal_submit(opened[0], values, message=panel_message(page), user=ADMIN_USER_ID,
                                            perms=MANAGE))
        assert [r.callback_type for r in saved] == [7]
        guild = store_guild(ui)
        assert guild["identity"]["bot_name"] == "Nova" and guild["identity"]["wake_names"] == ["nova", "nov"]
        assert guild["identity"]["greetings"] == ["hey", "hallo"] and guild["conversation"]["language"] == "German"

        opened = await click(h, panel_message(saved[0]), custom_id_for(saved[0], "id_edit"), user=ADMIN_USER_ID,
                             perms=MANAGE)
        before = ui.raw()
        too_many = await h.replay(modal_submit(opened[0], dict(values, wake_names="a,b,c,d,e,f"),
                                               message=panel_message(saved[0]), user=ADMIN_USER_ID, perms=MANAGE))
        assert "at most 5" in too_many[0].data["content"] and ui.raw() == before
        assert_all_ephemeral(saved + too_many)
    finally:
        await h.close()


async def test_reaction_emoji_from_the_server_with_pages(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "identity", user=ADMIN_USER_ID, perms=MANAGE)
        select = [c for c in controls(page) if c["type"] == 3][0]
        assert len(select["options"]) == 25 and select["options"][0]["label"] == "emoji_00"
        nxt = await click(h, panel_message(page), custom_id_for(page, "id_page", "1"), user=ADMIN_USER_ID,
                          perms=MANAGE)
        second = [c for c in controls(nxt[0]) if c["type"] == 3][0]
        assert [o["label"] for o in second["options"]] == [f"emoji_{i:02d}" for i in range(25, 30)]
        eid, name = EMOJIS[27]
        saved = await h.replay(select_payload(nxt[0], "id_emoji", [eid], component_type=3, user=ADMIN_USER_ID,
                                              perms=MANAGE))
        assert store_guild(ui)["identity"]["reaction_emoji"] == {"id": str(eid), "name": name}
        before = ui.raw()
        forged = await h.replay(select_payload(saved[0], "id_emoji", [FOREIGN_CHANNEL], component_type=3,
                                               user=ADMIN_USER_ID, perms=MANAGE))
        assert "not one of this server's emojis" in forged[0].data["content"] and ui.raw() == before

        opened = await click(h, panel_message(saved[0]), custom_id_for(saved[0], "id_emoji_std"), user=ADMIN_USER_ID,
                             perms=MANAGE)
        std = await h.replay(modal_submit(opened[0], {"emoji": "👀"}, message=panel_message(saved[0]),
                                          user=ADMIN_USER_ID, perms=MANAGE))
        assert store_guild(ui)["identity"]["reaction_emoji"] == "👀"
        opened = await click(h, panel_message(std[0]), custom_id_for(std[0], "id_emoji_std"), user=ADMIN_USER_ID,
                             perms=MANAGE)
        ascii_ = await h.replay(modal_submit(opened[0], {"emoji": ":)"}, message=panel_message(std[0]),
                                             user=ADMIN_USER_ID, perms=MANAGE))
        assert "unicode emoji" in ascii_[0].data["content"]
        off = await click(h, panel_message(std[0]), custom_id_for(std[0], "id_emoji_off"), user=ADMIN_USER_ID,
                          perms=MANAGE)
        assert store_guild(ui)["identity"]["reaction_emoji"] is None
        await click(h, panel_message(off[0]), custom_id_for(off[0], "id_reset"), user=ADMIN_USER_ID, perms=MANAGE)
        assert store_guild(ui)["identity"]["greetings"] == ["hey", "hi", "yo", "ok", "okay", "please"]
    finally:
        await h.close()


# ── channels ─────────────────────────────────────────────────────────────────────────────────────────


async def test_channels_per_role_with_checks(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "channels", user=ADMIN_USER_ID, perms=MANAGE)
        selects = [c for c in controls(page) if c["type"] == 8]
        assert len(selects) == 2 and all(s["max_values"] == 25 and s["min_values"] == 0 for s in selects)
        saved = await h.replay(select_payload(page, "ch_set", [OTHER_CHANNEL], component_type=8, arg="ambient",
                                              user=ADMIN_USER_ID, perms=MANAGE, resolved=channels_resolved(OTHER_CHANNEL)))
        assert [r.callback_type for r in saved] == [7]
        assert store_guild(ui)["conversation"]["ambient_channels"] == [str(OTHER_CHANNEL)]
        before = ui.raw()
        both = await h.replay(select_payload(saved[0], "ch_set", [OTHER_CHANNEL], component_type=8, arg="excluded",
                                             user=ADMIN_USER_ID, perms=MANAGE,
                                             resolved=channels_resolved(OTHER_CHANNEL)))
        assert "can't be in both lists" in both[0].data["content"] and ui.raw() == before
        foreign = await h.replay(select_payload(saved[0], "ch_set", [FOREIGN_CHANNEL], component_type=8,
                                                arg="excluded", user=ADMIN_USER_ID, perms=MANAGE,
                                                resolved=channels_resolved(FOREIGN_CHANNEL)))
        assert "not part of this server" in foreign[0].data["content"] and ui.raw() == before
        cleared = await h.replay(select_payload(saved[0], "ch_set", [], component_type=8, arg="ambient",
                                                user=ADMIN_USER_ID, perms=MANAGE, resolved={}))
        assert store_guild(ui)["conversation"]["ambient_channels"] == []
        assert_all_ephemeral(both + foreign + cleared)
    finally:
        await h.close()


# ── worklog ──────────────────────────────────────────────────────────────────────────────────────────


async def test_worklog_operator(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "worklog")
        assert "visible to operators only" in json.dumps(embed(page))
        public = await h.replay(select_payload(page, "wl_channel", [OTHER_CHANNEL], component_type=8, user=OP_ID,
                                               perms=PLAIN, resolved=channels_resolved(OTHER_CHANNEL)))
        assert "Everyone can read that channel" in embed(public[0])["description"]
        private = await h.replay(select_payload(public[0], "wl_channel", [WORK_CHANNEL], component_type=8, user=OP_ID,
                                                perms=PLAIN, resolved=channels_resolved(WORK_CHANNEL)))
        assert "Everyone can read" not in embed(private[0])["description"]
        archive = await h.replay(select_payload(private[0], "wl_archive", [4320], component_type=3, user=OP_ID,
                                                perms=PLAIN))
        final = await click(h, panel_message(archive[0]), custom_id_for(archive[0], "wl_final"), user=OP_ID,
                            perms=PLAIN)
        wl = store_guild(ui)["worklog"]
        assert wl == {"channel": str(WORK_CHANNEL), "archive_minutes": 4320, "final_to_origin": False}
        assert "Final copy to origin: off" in labels(final[0])
    finally:
        await h.close()


# ── member tools ─────────────────────────────────────────────────────────────────────────────────────


async def _skills(ui, h, page, text):
    opened = await click(h, panel_message(page), custom_id_for(page, "ln_skills"), user=OP_ID, perms=PLAIN)
    return await h.replay(modal_submit(opened[0], {"skills": text}, message=panel_message(page), user=OP_ID,
                                       perms=PLAIN))


async def test_member_tools_skills_are_checked_against_the_target_profile(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        asked = []

        def installed(ctx):
            asked.append(ctx.guild_id)
            return ["server-index", "faq"]

        ui.binding.skills_provider = installed
        page = await open_panel(ui, h, "lanes")
        saved = await _skills(ui, h, page, "server-index\nfaq")
        assert store_guild(ui)["lanes"]["skills"] == ["server-index", "faq"] and asked == [GUILD]
        before = ui.raw()
        missing = await _skills(ui, h, saved[0], "server-index\nterminal-helper")
        assert "Not installed in the bot's profile" in missing[0].data["content"] and ui.raw() == before
        ui.binding.skills_provider = lambda ctx: None                         # unknown: nothing new released
        unknown = await _skills(ui, h, saved[0], "server-index\nfaq\nnotes")
        assert "can't read which skills" in unknown[0].data["content"] and ui.raw() == before
        removed = await _skills(ui, h, saved[0], "faq")                       # removing needs no list
        assert [r.callback_type for r in removed] == [7] and store_guild(ui)["lanes"]["skills"] == ["faq"]
    finally:
        await h.close()


async def test_member_tools_toggles_limit_and_knowledge(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "lanes")
        off = await click(h, panel_message(page), custom_id_for(page, "ln_history"), user=OP_ID, perms=PLAIN)
        limit = await h.replay(select_payload(off[0], "ln_limit", [25], component_type=3, user=OP_ID, perms=PLAIN))
        links = await click(h, panel_message(limit[0]), custom_id_for(limit[0], "ln_links"), user=OP_ID, perms=PLAIN)
        opened = await click(h, panel_message(links[0]), custom_id_for(links[0], "ln_knowledge"), user=OP_ID,
                             perms=PLAIN)
        saved = await h.replay(modal_submit(opened[0], {"knowledge": "mcp__notes__get_note 42\n\n"},
                                            message=panel_message(links[0]), user=OP_ID, perms=PLAIN))
        lanes = store_guild(ui)["lanes"]
        assert lanes["channel_history"] == {"enabled": False, "max_limit": 25} and lanes["permalink_hydration"] is False
        assert lanes["knowledge"] == [{"tool": "mcp__notes__get_note", "id": "42"}]
        before = ui.raw()
        opened = await click(h, panel_message(saved[0]), custom_id_for(saved[0], "ln_knowledge"), user=OP_ID,
                             perms=PLAIN)
        bad = await h.replay(modal_submit(opened[0], {"knowledge": "terminal rm"}, message=panel_message(saved[0]),
                                          user=OP_ID, perms=PLAIN))
        assert "not an MCP read tool" in bad[0].data["content"] and ui.raw() == before
    finally:
        await h.close()


# ── operator turns ───────────────────────────────────────────────────────────────────────────────────


async def test_guidance_in_operator_turns_needs_confirmation_with_the_risk(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "operator_lane")
        assert "off" in embed(page)["fields"][1]["value"]
        confirm = await click(h, panel_message(page), custom_id_for(page, "ol_guidance_ask"), user=OP_ID, perms=PLAIN)
        risk = ui.anchor.mod("ui.perms").SETTINGS["operator_lane"].note
        assert risk in embed(confirm[0])["description"] and store_guild(ui)["prompts"]["guild_in_operator_lane"] is False
        on = await click(h, panel_message(confirm[0]), custom_id_for(confirm[0], "ol_guidance", "on"), user=OP_ID,
                         perms=PLAIN)
        assert store_guild(ui)["prompts"]["guild_in_operator_lane"] is True
        await click(h, panel_message(on[0]), custom_id_for(on[0], "ol_guidance", "off"), user=OP_ID, perms=PLAIN)
        assert store_guild(ui)["prompts"]["guild_in_operator_lane"] is False
    finally:
        await h.close()


# ── managers and the audit channel ───────────────────────────────────────────────────────────────────


async def test_managers_mode_roles_and_audit_channel(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "managers")
        mode = await h.replay(select_payload(page, "mg_mode", ["role"], component_type=3, user=OP_ID, perms=PLAIN))
        assert "No role chosen" in json.dumps(embed(mode[0]))
        roles = await h.replay(select_payload(mode[0], "mg_roles", [ROLE_A], component_type=6, user=OP_ID,
                                              perms=PLAIN, resolved=roles_resolved(ROLE_A)))
        before = ui.raw()
        foreign = await h.replay(select_payload(roles[0], "mg_roles", [FOREIGN_ROLE], component_type=6, user=OP_ID,
                                                perms=PLAIN, resolved=roles_resolved(FOREIGN_ROLE)))
        assert "not part of this server" in foreign[0].data["content"] and ui.raw() == before
        await h.replay(select_payload(roles[0], "mg_audit", [AUDIT_CHANNEL], component_type=8, user=OP_ID,
                                      perms=PLAIN, resolved=channels_resolved(AUDIT_CHANNEL)))
        guild = store_guild(ui)
        assert guild["managers"] == {"mode": "role", "role_ids": [str(ROLE_A)]}
        assert guild["audit_channel"] == str(AUDIT_CHANNEL)

        # From now on every change made in /policy is announced in the audit channel (names, no values).
        manager = await command(h, user=ADMIN_USER_ID, perms=MANAGE, )
        assert "Manage Server" in manager[0].data["content"]                  # lacks the role now
        home = (await command(h, user=OP_ID, perms=PLAIN))[0]
        await click(h, panel_message(home), custom_id(ui, home, "pause"), user=OP_ID, perms=PLAIN)
        assert ui.audit_posts[-1] == (str(AUDIT_CHANNEL), f"<@{OP_ID}> set the server policy to **paused** (/policy).")
    finally:
        await h.close()


# ── server guidance ──────────────────────────────────────────────────────────────────────────────────


async def open_guidance(ui, h, user=ADMIN_USER_ID, perms=MANAGE):
    return await open_panel(ui, h, "guidance", user=user, perms=perms)


async def edit_guidance(ui, h, page, text, *, user=ADMIN_USER_ID, perms=MANAGE):
    opened = await click(h, panel_message(page), custom_id_for(page, "gd_edit"), user=user, perms=perms)
    assert [r.callback_type for r in opened] == [9], opened
    return await h.replay(modal_submit(opened[0], {"part1": text[:4000], "part2": text[4000:]},
                                       message=panel_message(page), user=user, perms=perms))


async def test_guidance_page_explains_itself(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_guidance(ui, h)
        names = field_names(page)
        assert {"Length", "What the bot does with it", "In the operator's turns", "Placeholders"} <= set(names)
        placeholders = embed(page)["fields"][names.index("Placeholders")]["value"]
        for name in ("bot_name", "server_name", "channel_name", "channel", "language", "worklog_channel",
                     "operator_label"):
            assert "{" + name + "}" in placeholders
        assert labels(page) == ["Edit…", "Upload file…", "Download", "History (0)", "Reset…", "Back"]
        opened = await click(h, panel_message(page), custom_id_for(page, "gd_edit"), user=ADMIN_USER_ID, perms=MANAGE)
        parts = [row["component"] for row in opened[0].data["components"]]
        assert [(p["custom_id"], p["max_length"]) for p in parts] == [("part1", 4000), ("part2", 2000)]
    finally:
        await h.close()


async def test_guidance_saved_announced_and_sent_to_operators_with_a_diff(ui):
    activate_directly(ui)
    ui.ops.update_fields(ui.store, BOT, GUILD, {"audit_channel": str(AUDIT_CHANNEL)}, by="cli")
    h = await panel_harness(ui)
    try:
        page = await open_guidance(ui, h)
        text = "We talk about {server_name} retro games.\nBe kind. Answer in {language}."
        saved = await edit_guidance(ui, h, page, text)
        assert [r.callback_type for r in saved] == [7]
        assert embed(saved[0])["description"].startswith("✅ Saved:")
        prompts = store_guild(ui)["prompts"]
        assert prompts["guild"] == text and len(prompts["history"]) == 1
        assert prompts["history"][0]["by"] == str(ADMIN_USER_ID)
        assert ui.dms and ui.dms[-1][0] == str(OP_ID) and "changed the server guidance" in ui.dms[-1][1]
        name, data = ui.files[-1]
        assert name.endswith(".diff") and b"+We talk about {server_name} retro games." in data
        channel, line = ui.audit_posts[-1]
        assert channel == str(AUDIT_CHANNEL) and f"<@{ADMIN_USER_ID}> changed server guidance" in line
        assert "retro" not in line                                            # names and size, never the text
    finally:
        await h.close()


async def test_guidance_over_the_limit_is_not_saved(ui):
    activate_directly(ui)
    ui.settings["guild_prompt_max_chars"] = 50
    h = await panel_harness(ui)
    try:
        page = await open_guidance(ui, h)
        before = ui.raw()
        sent = await edit_guidance(ui, h, page, "x" * 60)
        assert "The guidance has 60 characters; the limit is 50." in sent[0].data["content"]
        assert ui.raw() == before and ui.dms == []
    finally:
        await h.close()


async def test_guidance_over_the_context_budget_is_not_saved(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        ui.binding.spill_provider = lambda: {"enabled": True, "max_chars": 2500}
        page = await open_guidance(ui, h)
        before = ui.raw()
        sent = await edit_guidance(ui, h, page, "y" * 1900)
        assert "context budget leaves room for" in sent[0].data["content"] and ui.raw() == before
    finally:
        await h.close()


async def test_unknown_placeholders_are_saved_with_a_warning(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_guidance(ui, h)
        saved = await edit_guidance(ui, h, page, "Hi {nickname}, see {x.__class__}.")
        assert "Not placeholders, kept as written" in embed(saved[0])["description"]
        assert "{nickname}" in embed(saved[0])["description"]
        assert store_guild(ui)["prompts"]["guild"] == "Hi {nickname}, see {x.__class__}."
    finally:
        await h.close()


def _capture_files(h):
    captured = []
    original = h.adapter.request

    async def request(route, session, *, payload=None, multipart=None, files=None, **kwargs):
        for f in files or ():
            f.fp.seek(0)
            captured.append((f.filename, f.fp.read()))
            f.fp.seek(0)
        return await original(route, session, payload=payload, multipart=multipart, files=files, **kwargs)

    h.adapter.request = request
    return captured


@pytest.mark.parametrize(("key", "lane", "mode"), [("public.ambient", "public", "ambient"),
                                                   ("public.direct", "public", "direct"),
                                                   ("operator.direct", "operator", "direct")])
async def test_preview_is_exactly_what_assemble_returns(ui, key, lane, mode):
    activate_directly(ui)
    ui.ops.update_fields(ui.store, BOT, GUILD, {"lanes.skills": ["faq"], "identity.bot_name": "Nova"}, by="cli")
    ui.ops.set_guidance(ui.store, BOT, GUILD, "Talk about {server_name} in #{channel_name}.", by="cli")
    ui.settings.update(operator_label="the host", frame_prompt="You serve {server_name}.")
    h = await panel_harness(ui)
    try:
        captured = _capture_files(h)
        page = await open_guidance(ui, h)
        sent = await h.replay(select_payload(page, "gd_preview", [key], component_type=3, user=ADMIN_USER_ID,
                                             perms=MANAGE))
        assert [r.callback_type for r in sent] == [4] and sent[0].data["flags"] & EPHEMERAL
        assert sent[0].file_count == 1
        name, data = captured[-1]
        assert name == f"preview-{key.replace('.', '-')}.md"

        # Independent of the panel: the runtime's own pieces with the same turn facts.
        prompts = ui.anchor.mod("prompts")
        lanes = ui.anchor.mod("policy.lanes")
        routes = ui.anchor.mod("policy.routes")
        guild = store_guild(ui)
        route = routes.Route(BOT, GUILD, str(PUBLIC_CHANNEL), "0", str(ADMIN_USER_ID), lane,
                             lanes=lanes.PublicLanes.from_guild(guild))
        settings = {"frame_prompt": "You serve {server_name}.", "operator_label": "the host",
                    "guild_prompt_max_chars": 6000, "notify_operators": True}
        expected = prompts.assemble(settings, guild, lane=lane, mode=mode, capability=lanes.capability_lane_text(route),
                                    budget=prompts.budget_from_spill(None), server_name="Example Guild",
                                    channel_name="general", channel_id=str(PUBLIC_CHANNEL),
                                    bot_display_name="replay-bot")
        assert data.decode("utf-8") == expected.text
        assert ("Talk about Example Guild in #general." in expected.text) is (lane == "public")

        # And exactly what the runtime injects in pre_llm_call for such a turn (wiring.turn_context).
        class Adapter:  # weak-referenceable, like the real adapter
            pass

        adapter = Adapter()
        ui.anchor.RUNTIME.bots.bind(adapter, BOT, ui.anchor.home)
        turn = routes.Route(BOT, GUILD, str(PUBLIC_CHANNEL), "1", str(ADMIN_USER_ID), lane, mode=mode,
                            lanes=lanes.PublicLanes.from_guild(guild), server_name="Example Guild",
                            channel_name="general", bot_display_name="replay-bot")
        assert ui.anchor.mod("wiring").turn_context(turn) == data.decode("utf-8")
    finally:
        await h.close()


class _Attachment:
    def __init__(self, filename, data, *, size=None):
        self.id = 123450000000000001
        self.filename = filename
        self.size = len(data) if size is None else size
        self.data = data


def _attachment_resolved(att):
    return {"attachments": {str(att.id): {"id": str(att.id), "filename": att.filename, "size": att.size,
                                          "url": "https://cdn.invalid/x", "proxy_url": "https://cdn.invalid/x"}}}


@pytest.mark.parametrize(("filename", "data", "replace", "outcome"), [
    ("rules.md", "New rules.\n".encode(), True, "New rules.\n"),
    ("rules.txt", "﻿More.".encode("utf-8"), False, "Old text.\n\nMore."),
    ("rules.exe", b"x", True, "only .md and .txt"),
    ("rules.md", b"\xff\xfe\x00", True, "not UTF-8"),
    ("big.md", b"x" * (64 * 1024 + 1), True, "at most 65 536"),
], ids=["replace", "append-bom", "suffix", "encoding", "size"])
async def test_guidance_upload_modal(ui, filename, data, replace, outcome):
    activate_directly(ui)
    ui.ops.set_guidance(ui.store, BOT, GUILD, "Old text.", by="cli")
    h = await panel_harness(ui)
    try:
        att = _Attachment(filename, data)

        async def read(attachment):
            assert str(attachment.id) == str(att.id)
            return att.data

        ui.binding.read_attachment = read
        page = await open_guidance(ui, h)
        opened = await click(h, panel_message(page), custom_id_for(page, "gd_upload"), user=ADMIN_USER_ID,
                             perms=MANAGE)
        kinds = [row["component"]["type"] for row in opened[0].data["components"]]
        assert kinds == [19, 23]                                              # FileUpload, Checkbox
        before = ui.raw()
        sent = await h.replay(modal_submit(opened[0], {"file": [att.id], "replace": replace},
                                           message=panel_message(page), user=ADMIN_USER_ID, perms=MANAGE,
                                           resolved=_attachment_resolved(att)))
        assert sent[0].callback_type == 6                                     # acknowledged before downloading
        if outcome.endswith("\n") or outcome.endswith("."):
            assert store_guild(ui)["prompts"]["guild"] == outcome
        else:
            assert outcome in sent[-1].payload["content"] and ui.raw() == before
        assert_all_ephemeral(sent)
    finally:
        await h.close()


async def test_guidance_upload_through_the_command_option(ui):
    activate_directly(ui)
    h = await panel_harness(ui, mode="classic")
    try:
        att = _Attachment("guide.md", b"From a file.")

        async def read(attachment):
            return att.data

        ui.binding.read_attachment = read
        data = {"id": "999999999999999999", "name": "policy", "type": 1, "guild_id": str(GUILD_ID),
                "options": [{"name": "guidance_file", "type": 11, "value": str(att.id)}],
                "resolved": _attachment_resolved(att)}
        sent = await h.replay(interaction_payload(2, data, user_id=ADMIN_USER_ID, permissions=MANAGE))
        assert sent[0].callback_type == 5 and store_guild(ui)["prompts"]["guild"] == "From a file."
        member = await h.replay(interaction_payload(2, data, user_id=MEMBER_USER_ID, permissions=PLAIN))
        assert [r.callback_type for r in member] == [4] and "Manage Server" in member[0].data["content"]
        assert_all_ephemeral(sent + member)
    finally:
        await h.close()


async def test_guidance_history_restore_with_rev_and_reset(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_guidance(ui, h)
        first = await edit_guidance(ui, h, page, "Version one.")
        second = await edit_guidance(ui, h, first[0], "Version two.")
        history = await click(h, panel_message(second[0]), custom_id_for(second[0], "gd_history"), user=ADMIN_USER_ID,
                              perms=MANAGE)
        restore = [c for c in controls(history[0]) if ":gd_restore:" in c.get("custom_id", "")]
        assert len(restore) == 1                                              # the current version has none
        stale_message = panel_message(history[0])
        done = await click(h, stale_message, restore[0]["custom_id"], user=ADMIN_USER_ID, perms=MANAGE)
        assert store_guild(ui)["prompts"]["guild"] == "Version one."
        assert len(store_guild(ui)["prompts"]["history"]) == 3
        before = ui.raw()
        again = await click(h, stale_message, restore[0]["custom_id"], user=ADMIN_USER_ID, perms=MANAGE)
        assert embed(again[0])["description"].startswith("⚠️ Not saved:") and ui.raw() == before
        confirm = await click(h, panel_message(done[0]), custom_id_for(done[0], "gd_reset"), user=ADMIN_USER_ID,
                              perms=MANAGE)
        await click(h, panel_message(confirm[0]), custom_id_for(confirm[0], "gd_reset_ok"), user=ADMIN_USER_ID,
                    perms=MANAGE)
        prompts = store_guild(ui)["prompts"]
        assert prompts["guild"] == "" and prompts["history"][-2]["text"] == "Version one."
    finally:
        await h.close()


async def test_guidance_download(ui):
    activate_directly(ui)
    ui.ops.set_guidance(ui.store, BOT, GUILD, "Be nice.", by="cli")
    h = await panel_harness(ui)
    try:
        captured = _capture_files(h)
        page = await open_guidance(ui, h)
        sent = await click(h, panel_message(page), custom_id_for(page, "gd_download"), user=ADMIN_USER_ID,
                           perms=MANAGE)
        assert sent[0].data["flags"] & EPHEMERAL and captured[-1] == ("guidance.md", b"Be nice.")
    finally:
        await h.close()


# ── operator messages: throttled, coalesced, switchable ──────────────────────────────────────────────


async def test_guidance_message_inside_the_window_is_delayed_not_dropped(ui, monkeypatch):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        notify = ui.anchor.mod("ui.notify")
        # The window is judged by the module's clock, held still here, so a slow host between the two edits
        # can't push the second one out of the window (no wall-clock race).
        clock = [1000.0]
        monkeypatch.setattr(notify, "time", type("Clock", (), {"time": staticmethod(lambda: clock[0])}))
        page = await open_guidance(ui, h)
        first = await edit_guidance(ui, h, page, "One.")
        assert len(ui.dms) == 1
        await edit_guidance(ui, h, first[0], "Two.")
        assert len(ui.dms) == 1                                # inside the window: held back, not dropped
        (timer,) = ui.binding.notify._pending.values()         # armed for the end of the window
        clock[0] += notify.GUIDANCE_WINDOW
        fire, args = timer._callback, timer._args              # what the event loop runs when the timer fires
        timer.cancel()
        fire(*args)
        await notify_wait(ui, expected=2)
        assert len(ui.dms) == 2
        name, data = ui.files[-1]
        assert b"-One." in data and b"+Two." in data
    finally:
        await h.close()


async def notify_wait(ui, *, expected, timeout=3.0):
    import asyncio

    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while loop.time() < deadline:
        if len(ui.dms) >= expected and not ui.binding.notify._pending and not ui.binding._tasks:
            return
        await asyncio.sleep(0.02)


async def test_operator_messages_can_be_switched_off(ui):
    activate_directly(ui)
    ui.settings["notify_operators"] = False
    h = await panel_harness(ui)
    try:
        page = await open_guidance(ui, h)
        await edit_guidance(ui, h, page, "Quiet.")
        assert ui.dms == [] and store_guild(ui)["prompts"]["guild"] == "Quiet."
    finally:
        await h.close()


async def test_worse_protection_is_reported_once_per_day(ui):
    status = ui.anchor.mod("adapt.status")
    h = await panel_harness(ui)
    try:
        ui.anchor.RUNTIME.status.record(status.Result("A5", status.SECURITY, status.Outcome.OFF, "signature changed"))
        assert await ui.binding.notify.check_status() == 1
        assert "A5: off — signature changed" in ui.dms[-1][1]
        assert await ui.binding.notify.check_status() == 0
        ui.settings["notify_operators"] = False
        ui.anchor.RUNTIME.status.record(status.Result("A7", status.SECURITY, status.Outcome.OFF, "missing"))
        assert await ui.binding.notify.check_status() == 0
    finally:
        await h.close()


# ── review follow-ups ────────────────────────────────────────────────────────────────────────────────


async def test_history_offers_each_version_once(ui):
    activate_directly(ui)
    for text in ("A.", "B.", "A.", "B."):
        ui.ops.set_guidance(ui.store, BOT, GUILD, text, by=str(ADMIN_USER_ID))
    h = await panel_harness(ui)
    try:
        page = await open_guidance(ui, h)
        history = await click(h, panel_message(page), custom_id_for(page, "gd_history"), user=ADMIN_USER_ID,
                              perms=MANAGE)
        ids = [c["custom_id"] for c in controls(history[0])]
        assert len(ids) == len(set(ids))                                      # Discord refuses duplicate ids
        assert len([i for i in ids if ":gd_restore:" in i]) == 1              # "A." once; "B." is current
    finally:
        await h.close()


class _Runner:
    def __init__(self, routes, op_home):
        self.routes = routes
        self.op_home = op_home
        self.sources = []

    def _resolve_profile_home_for_source(self, source):
        self.sources.append(source)
        if source.user_id == str(OP_ID):
            return self.op_home                                               # a user route for the operator
        return self.routes[source.chat_id]


class _Adapter:
    def __init__(self, runner):
        self.gateway_runner = runner

    def build_source(self, **kwargs):
        from types import SimpleNamespace

        return SimpleNamespace(profile_route_rejected=False, **kwargs)


async def test_skills_are_checked_in_every_profile_members_reach(ui, monkeypatch, tmp_path):
    activate_directly(ui)
    hermes = ui.anchor.mod("hermes")
    runtime = ui.anchor.mod("runtime")
    homes = {name: tmp_path / name for name in ("members-a", "members-b", "operator", "unarmed")}
    for home in homes.values():
        home.mkdir()
    for name in ("members-a", "members-b", "operator"):
        ui.anchor.RUNTIME.armed.arm(runtime.Instance(home_key=hermes.home_key(homes[name]), profile=name,
                                                     plugin_id="ctx_discord_policy", plugin_dir="",
                                                     home=str(homes[name])))
    installed = {homes["members-a"]: {"faq", "rules"}, homes["members-b"]: {"faq"}, homes["operator"]: {"admin"}}
    monkeypatch.setattr(hermes, "installed_skills", lambda home: installed[Path(home)], raising=False)
    routes = {str(PUBLIC_CHANNEL): homes["members-a"], str(WORK_CHANNEL): homes["members-b"],
              str(OTHER_CHANNEL): homes["unarmed"], str(AUDIT_CHANNEL): homes["members-a"]}
    h = await panel_harness(ui)
    try:
        runner = _Runner(routes, homes["operator"])
        ui.binding.adapter = _Adapter(runner)
        page = await open_panel(ui, h, "lanes")
        ok = await _skills(ui, h, page, "faq")
        assert store_guild(ui)["lanes"]["skills"] == ["faq"]
        before = ui.raw()
        only_a = await _skills(ui, h, ok[0], "faq\nrules")                    # missing in members-b
        assert "Not installed" in only_a[0].data["content"] and ui.raw() == before
        op_only = await _skills(ui, h, ok[0], "faq\nadmin")                   # the operator's own profile
        assert "Not installed" in op_only[0].data["content"] and ui.raw() == before
        assert {s.user_id for s in runner.sources} == {ui.anchor.mod("ui.commands").MEMBER_PROBE_ID}
        assert {s.chat_id for s in runner.sources} == set(routes)            # every text channel
        runner.routes = {k: homes["unarmed"] for k in routes}                 # nowhere armed: unknown
        unknown = await _skills(ui, h, ok[0], "faq\nnew")
        assert "can't read which skills" in unknown[0].data["content"]
    finally:
        await h.close()


async def test_a_lowered_cap_does_not_block_other_settings(ui):
    activate_directly(ui)
    ui.ops.set_guidance(ui.store, BOT, GUILD, "z" * 80, by="cli")
    ui.settings["guild_prompt_max_chars"] = 50
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "channels", user=ADMIN_USER_ID, perms=MANAGE)
        await h.replay(select_payload(page, "ch_set", [OTHER_CHANNEL], component_type=8, arg="ambient",
                                      user=ADMIN_USER_ID, perms=MANAGE, resolved=channels_resolved(OTHER_CHANNEL)))
        assert store_guild(ui)["conversation"]["ambient_channels"] == [str(OTHER_CHANNEL)]
        guidance = await open_guidance(ui, h)
        assert "the limit is 50" in json.dumps(embed(guidance))               # shown where it can be fixed
    finally:
        await h.close()


async def test_long_knowledge_list_does_not_open_a_broken_form(ui):
    activate_directly(ui)
    entries = [{"tool": "mcp__notes__get_note", "id": f"{i:03d}-" + "k" * 190} for i in range(25)]
    ui.ops.update_fields(ui.store, BOT, GUILD, {"lanes.knowledge": entries}, by="cli")
    h = await panel_harness(ui)
    try:
        page = await open_panel(ui, h, "lanes")
        sent = await click(h, panel_message(page), custom_id_for(page, "ln_knowledge"), user=OP_ID, perms=PLAIN)
        assert [r.callback_type for r in sent] == [4] and "longer than a Discord form" in sent[0].data["content"]
    finally:
        await h.close()


async def test_deleting_the_settings_drops_the_held_guidance_copy(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        page = await open_guidance(ui, h)
        await edit_guidance(ui, h, page, "Remember me.")
        state = ui.binding.notify.state
        assert any(k.startswith("guidance:") for k in state._read())
        home = (await command(h, user=OP_ID, perms=PLAIN))[0]
        confirm = await click(h, panel_message(home), custom_id(ui, home, "unbind"), user=OP_ID, perms=PLAIN)
        await click(h, panel_message(confirm[0]), custom_id(ui, confirm[0], "purge_ok"), user=OP_ID, perms=PLAIN)
        assert store_guild(ui) is None and not any(k.startswith("guidance:") for k in state._read())
    finally:
        await h.close()


async def test_every_discord_write_records_its_origin(ui):
    """Lifecycle, settings and guidance writes made in /policy all carry ``via = "discord"`` in the audit."""
    h = await panel_harness(ui)
    try:
        panel = (await command(h, user=OP_ID, perms=PLAIN))[0]
        confirm = await click(h, panel_message(panel), custom_id(ui, panel, "activate"), user=OP_ID, perms=PLAIN)
        await click(h, panel_message(confirm[0]), custom_id(ui, confirm[0], "activate_ok"), user=OP_ID, perms=PLAIN)
        page = await open_panel(ui, h, "channels", user=ADMIN_USER_ID, perms=MANAGE)
        await h.replay(select_payload(page, "ch_set", [OTHER_CHANNEL], component_type=8, arg="ambient",
                                      user=ADMIN_USER_ID, perms=MANAGE, resolved=channels_resolved(OTHER_CHANNEL)))
        await edit_guidance(ui, h, await open_guidance(ui, h), "Be kind.")
        home = (await command(h, user=ADMIN_USER_ID, perms=MANAGE))[0]
        await click(h, panel_message(home), custom_id(ui, home, "pause"), user=ADMIN_USER_ID, perms=MANAGE)
        assert ui.state() == "paused"
        audit = ui.store.read().data["audit"]
        assert {"created", "state", "conversation.ambient_channels", "prompts.guild"} <= {a["field"] for a in audit}
        assert [a for a in audit if a.get("via") != "discord"] == []
    finally:
        await h.close()
