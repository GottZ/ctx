"""S13 L3: the Models panel replayed through discord.py's real dispatch, per role.

Providers, credentials (by name) and context windows of the server's profile come from
``unit.routing_fakes``; the real Hermes behaviour is pinned by ``tests/contract/test_models_contract.py``.
"""

from __future__ import annotations

import pytest

discord = pytest.importorskip("discord")

from tests.replay.harness import ADMIN_USER_ID, GUILD_ID, MEMBER_USER_ID, interaction_payload  # noqa: E402
from tests.replay.test_ui_profile import (  # noqa: E402
    BOT, GUILD, MANAGE, OP_ID, PLAIN, UI, cid, click, command, embed, field, forged, harness, labels, last_panel,
    panel, select, text)

pytestmark = [pytest.mark.asyncio, pytest.mark.replay]

DS = {"provider": "deepseek", "model": "deepseek-chat"}


@pytest.fixture
def ui(anchor, monkeypatch):
    from fakes.ctx import FakeCtx

    anchor.entry.register(FakeCtx("default"))
    u = UI(anchor, monkeypatch)
    u.fake.credentials = {"default": {"deepseek": "available"}}
    u.fake.catalog = {"default": [
        {"slug": "deepseek", "name": "DeepSeek", "models": ["deepseek-chat", "deepseek-reasoner", "tiny"], "total": 3},
        {"slug": "openrouter", "name": "OpenRouter", "models": ["x/y"], "total": 1}]}
    u.fake.contexts[("deepseek", "tiny")] = 32_000
    yield u
    anchor.mod("config").set_reader(None)


def models(ui):
    return ui.guild()["models"]


async def open_models(ui, h, user=OP_ID, perms=PLAIN):
    home = (await command(h, user=user, perms=perms))[0]
    assert "Models" in labels(home)
    return last_panel(await click(h, panel(home), cid(ui, home, "nav", "models"), user=user, perms=perms))


async def choose(ui, h, page, lane_label_arg, provider, model, user=OP_ID, perms=PLAIN):
    step1 = last_panel(await click(h, panel(page), cid(ui, page, "mod_pick", lane_label_arg), user=user, perms=perms))
    options = select(step1)["options"]
    token = next(o["value"] for o in options if o["label"] == provider)
    step2 = last_panel(await click(h, panel(step1), select(step1)["custom_id"], user=user, perms=perms, values=[token]))
    return step1, step2, await click(h, panel(step2), select(step2)["custom_id"], user=user, perms=perms,
                                     values=[f"{provider}|{model}"])


async def test_operator_chooses_members_model_in_two_steps(ui):
    h = await harness(ui)
    try:
        page = await open_models(ui, h)
        assert embed(page)["title"] == "Models for this server"
        assert field(page, "Members' turns") == "the profile's model"
        assert labels(page) == ["Members' model…", "Operator model…", "Release to managers…", "Back"]
        step1, step2, sent = await choose(ui, h, page, "public", "deepseek", "deepseek-chat")
        descriptions = {o["label"]: o.get("description") for o in select(step1)["options"]}
        assert descriptions == {"deepseek": "3 models · available", "openrouter": "no credential in this profile"}
        assert "auth add openrouter" in field(step1, "Not usable here")
        assert [o["value"] for o in select(step2)["options"]] == [f"deepseek|{m}" for m in
                                                                  ("deepseek-chat", "deepseek-reasoner", "tiny")]
        done = last_panel(sent)
        assert embed(done)["description"].startswith("✅ Members' turns: `deepseek-chat` (deepseek).")
        assert models(ui)["public"] == DS and models(ui)["operator"] is None
        audit = __import__("json").loads(ui.raw())["audit"][-1]
        assert audit["field"] == "models.public" and audit["by"] == str(OP_ID)
        assert h.task_errors == []
    finally:
        await h.close()


async def test_small_context_window_and_missing_credential_are_refused(ui):
    h = await harness(ui)
    try:
        page = await open_models(ui, h)
        before = ui.raw()
        _, _, sent = await choose(ui, h, page, "public", "deepseek", "tiny")
        assert "32 000 tokens; Hermes needs at least 64 000" in text(sent) and "Next step" in text(sent)
        page = await open_models(ui, h)
        _, _, sent = await choose(ui, h, page, "operator", "openrouter", "x/y")
        assert "no credential for openrouter" in text(sent) and "auth add openrouter" in text(sent)
        assert ui.raw() == before
    finally:
        await h.close()


async def test_release_list_and_manager_pick(ui):
    h = await harness(ui)
    try:
        page = await open_models(ui, h)
        await choose(ui, h, page, "public", "deepseek", "deepseek-chat")
        page = await open_models(ui, h)
        _, _, sent = await choose(ui, h, page, "choices", "deepseek", "deepseek-reasoner")
        assert "Released to server managers" in embed(last_panel(sent))["description"]
        assert models(ui)["public_choices"] == [{"provider": "deepseek", "model": "deepseek-reasoner"}]

        manager = await open_models(ui, h, ADMIN_USER_ID, MANAGE)
        assert field(manager, "Members' conversations") == "`deepseek-chat` (deepseek)"
        assert labels(manager) == ["Choose…", "Back"]
        listing = last_panel(await click(h, panel(manager), cid(ui, manager, "mod_mgr"), user=ADMIN_USER_ID,
                                         perms=MANAGE))
        values = [o["value"] for o in select(listing)["options"]]
        assert values == ["deepseek|deepseek-reasoner", "-"]
        done = last_panel(await click(h, panel(listing), select(listing)["custom_id"], user=ADMIN_USER_ID,
                                      perms=MANAGE, values=["deepseek|deepseek-reasoner"]))
        assert "deepseek-reasoner" in embed(done)["description"]
        assert models(ui)["manager_pick"] == {"provider": "deepseek", "model": "deepseek-reasoner"}
        operator = await open_models(ui, h)
        assert "chosen by a server manager" in field(operator, "Members' turns")
    finally:
        await h.close()


async def test_managers_cannot_set_operator_settings(ui):
    """M-M6: ``model`` is operator-only; a forged control of the operator page is refused."""
    h = await harness(ui)
    try:
        manager = await open_models(ui, h, ADMIN_USER_ID, MANAGE)
        assert labels(manager) == ["Back"] and "has not released" in field(manager, "Your choice")
        before = ui.raw()
        rev = ui.store.read().guild_rev(BOT, GUILD)
        for action, arg in (("mod_pick", "operator"), ("mod_set", "p0123"), ("mod_clear", "public"),
                            ("mod_unav", ""), ("mod_other", "p0123")):
            forged_id = ui.items.encode(GUILD, action, arg, rev)
            sent = await click(h, forged(forged_id), forged_id, user=ADMIN_USER_ID, perms=MANAGE)
            assert "Only the bot's operator can change the models this server uses" in text(sent), action
        member = await command(h, user=MEMBER_USER_ID, perms=PLAIN, page="models")
        assert "Manage Server" in text(member)
        assert ui.raw() == before
    finally:
        await h.close()


async def test_manager_pick_outside_the_release_list_is_refused(ui):
    h = await harness(ui)
    try:
        ui.ops.set_models(ui.store, BOT, GUILD, {"models.public_choices": [DS]}, by=str(OP_ID))
        manager = await open_models(ui, h, ADMIN_USER_ID, MANAGE)
        listing = last_panel(await click(h, panel(manager), cid(ui, manager, "mod_mgr"), user=ADMIN_USER_ID,
                                         perms=MANAGE))
        sent = await click(h, panel(listing), select(listing)["custom_id"], user=ADMIN_USER_ID, perms=MANAGE,
                           values=["openrouter|x/y"])
        assert "not released" in text(sent) and models(ui)["manager_pick"] is None
    finally:
        await h.close()


async def test_unavailable_rule_and_profile_model(ui):
    h = await harness(ui)
    try:
        page = await open_models(ui, h)
        unav = [c for c in __import__("tests.replay.test_ui_profile", fromlist=["controls"]).controls(page)
                if c.get("type") == 3][0]
        done = last_panel(await click(h, panel(page), unav["custom_id"], user=OP_ID, perms=PLAIN, values=["close"]))
        assert models(ui)["on_unavailable"] == "close"
        assert field(done, "If a model is unavailable") == "Close for members"
        await choose(ui, h, done, "operator", "deepseek", "deepseek-reasoner")
        page = await open_models(ui, h)
        step1 = last_panel(await click(h, panel(page), cid(ui, page, "mod_pick", "operator"), user=OP_ID, perms=PLAIN))
        cleared = last_panel(await click(h, panel(step1), cid(ui, step1, "mod_clear", "operator"), user=OP_ID,
                                         perms=PLAIN))
        assert models(ui)["operator"] is None and "the profile's model" in embed(cleared)["description"]
    finally:
        await h.close()


async def test_other_model_id_through_a_form(ui):
    h = await harness(ui)
    try:
        page = await open_models(ui, h)
        step1 = last_panel(await click(h, panel(page), cid(ui, page, "mod_pick", "public"), user=OP_ID, perms=PLAIN))
        token = next(o["value"] for o in select(step1)["options"] if o["label"] == "deepseek")
        step2 = last_panel(await click(h, panel(step1), select(step1)["custom_id"], user=OP_ID, perms=PLAIN,
                                       values=[token]))
        opened = await click(h, panel(step2), cid(ui, step2, "mod_other"), user=OP_ID, perms=PLAIN)
        assert opened[0].callback_type == 9
        form = opened[0].data
        values = {"provider": "deepseek", "model": "deepseek-v9-preview"}
        components = [{"type": 18, "id": i + 1, "component": {"type": 4, "id": 10 + i,
                                                               "custom_id": c["component"]["custom_id"],
                                                               "value": values[c["component"]["custom_id"]]}}
                      for i, c in enumerate(form["components"])]
        sent = await h.replay(interaction_payload(5, {"custom_id": form["custom_id"], "components": components},
                                                  message=panel(step2), user_id=OP_ID, permissions=PLAIN,
                                                  guild_id=GUILD_ID))
        description = embed(last_panel(sent))["description"]
        assert "deepseek-v9-preview" in description
        assert "not in the model list Hermes knows for deepseek" in description      # B4: checked, warned
        assert models(ui)["public"] == {"provider": "deepseek", "model": "deepseek-v9-preview"}
    finally:
        await h.close()


async def test_manager_list_preselects_exactly_one_option(ui):
    """Review finding 10: the operator's members' model is also released and no manager picked yet."""
    released = {"provider": "deepseek", "model": "deepseek-reasoner"}
    ui.ops.set_models(ui.store, BOT, GUILD, {"models.public": released, "models.public_choices": [released]},
                      by=str(OP_ID))
    h = await harness(ui)
    try:
        manager = await open_models(ui, h, ADMIN_USER_ID, MANAGE)
        listing = last_panel(await click(h, panel(manager), cid(ui, manager, "mod_mgr"), user=ADMIN_USER_ID,
                                         perms=MANAGE))
        defaults = [o["value"] for o in select(listing)["options"] if o.get("default")]
        assert defaults == ["-"]
        ui.ops.set_manager_pick(ui.store, BOT, GUILD, released, by=str(ADMIN_USER_ID))
        manager = await open_models(ui, h, ADMIN_USER_ID, MANAGE)
        listing = last_panel(await click(h, panel(manager), cid(ui, manager, "mod_mgr"), user=ADMIN_USER_ID,
                                         perms=MANAGE))
        assert [o["value"] for o in select(listing)["options"] if o.get("default")] == ["deepseek|deepseek-reasoner"]
    finally:
        await h.close()


async def test_providers_page_instead_of_being_cut_off(ui):
    ui.fake.catalog = {"default": [{"slug": f"prov{i:02d}", "name": f"P{i}", "models": ["m"], "total": 1}
                                   for i in range(30)]}
    h = await harness(ui)
    try:
        page = await open_models(ui, h)
        step1 = last_panel(await click(h, panel(page), cid(ui, page, "mod_pick", "public"), user=OP_ID, perms=PLAIN))
        assert len(select(step1)["options"]) == 25 and "Next" in labels(step1)
        nxt = last_panel(await click(h, panel(step1), cid(ui, step1, "mod_pick", "public.1"), user=OP_ID, perms=PLAIN))
        assert [o["label"] for o in select(nxt)["options"]] == [f"prov{i:02d}" for i in range(25, 30)]
    finally:
        await h.close()


async def test_switching_profile_resets_the_models(ui):
    """Abnahme 6 (S13): a profile change drops the model choices (they belong to the old profile)."""
    ui.profiles("community")
    ui.ops.set_models(ui.store, BOT, GUILD, {"models.public": DS}, by=str(OP_ID))
    ui.anchor.mod("routing.apply").set_profile(ui.store, BOT, GUILD, "community", owner_home=ui.anchor.home,
                                               by=str(OP_ID), adapter=ui.adapter)
    assert models(ui)["public"] is None


async def test_switch_confirmation_names_every_model_choice_it_drops(ui):
    """B5: the confirmation names the dropped values, the release list and a manager's pick included."""
    ui.profiles("community", "research")
    released = {"provider": "deepseek", "model": "deepseek-reasoner"}
    ui.ops.set_models(ui.store, BOT, GUILD, {"models.public": DS, "models.public_choices": [released]},
                      by=str(OP_ID))
    ui.ops.set_manager_pick(ui.store, BOT, GUILD, released, by=str(ADMIN_USER_ID))
    h = await harness(ui)
    try:
        home = (await command(h, user=OP_ID, perms=PLAIN))[0]
        page = last_panel(await click(h, panel(home), cid(ui, home, "nav", "profile"), user=OP_ID, perms=PLAIN))
        listing = last_panel(await click(h, panel(page), cid(ui, page, "prof_pick", "0"), user=OP_ID, perms=PLAIN))
        confirm = last_panel(await click(h, panel(listing), select(listing)["custom_id"], user=OP_ID, perms=PLAIN,
                                         values=["research"]))
        text_ = embed(confirm)["description"]
        assert "members: deepseek-chat" in text_ and "1 model released to server managers" in text_
        assert "a server manager's pick" in text_
    finally:
        await h.close()


async def test_a_model_change_is_announced_in_the_audit_channel_without_the_model(ui):
    ui.ops.update_fields(ui.store, BOT, GUILD, {"audit_channel": "900000000000000555"}, by="cli")
    h = await harness(ui)
    posts = []

    async def post(channel_id, line):
        posts.append((channel_id, line))

    ui.binding.channel_sink = post
    try:
        page = await open_models(ui, h)
        await choose(ui, h, page, "public", "deepseek", "deepseek-chat")
        assert models(ui)["public"] == DS
        [(channel, line)] = posts
        assert channel == "900000000000000555" and line.startswith(f"<@{OP_ID}> changed the models")
        assert line.endswith(": public.") and "deepseek" not in line
    finally:
        await h.close()


async def test_operators_hear_once_a_day_when_a_chosen_model_cannot_be_used(ui):
    import asyncio

    ui.ops.set_models(ui.store, BOT, GUILD, {"models.public": DS}, by="cli")
    h = await harness(ui)
    dms = []

    async def record(user_id, line, file=None):
        dms.append((user_id, line))

    ui.binding.notifier = record
    wiring, runtime = ui.anchor.mod("wiring"), ui.anchor.mod("models.runtime")
    wiring._UI[BOT] = ui.binding

    async def unavailable(lane="public"):
        runtime.note_unavailable((BOT, GUILD, lane), "no credential for deepseek in this profile", "deepseek-chat")
        await asyncio.gather(*list(ui.binding._tasks))

    try:
        await unavailable()
        [(to, line)] = dms
        assert to == str(OP_ID) and "members' turns" in line and "deepseek" in line and "profile's model" in line
        runtime.note_available((BOT, GUILD, "public"))
        await unavailable()                                   # unavailable again the same day: no second message
        assert len(dms) == 1
        wiring._MODEL_NOTICES.clear()                         # a restart: the day is kept next to the store
        runtime.note_available((BOT, GUILD, "public"))
        await unavailable()
        assert len(dms) == 1
        ui.ops.set_models(ui.store, BOT, GUILD, {"models.on_unavailable": "close"}, by="cli")
        await unavailable("operator")                          # another lane has its own day
        assert len(dms) == 2 and "operator turns" in dms[-1][1] and "profile's model" in dms[-1][1]
    finally:
        await h.close()


async def test_a_closed_server_tells_the_operators_why_members_get_no_answer(ui):
    import asyncio

    ui.ops.set_models(ui.store, BOT, GUILD, {"models.public": DS, "models.on_unavailable": "close"}, by="cli")
    h = await harness(ui)
    dms = []

    async def record(user_id, line, file=None):
        dms.append((user_id, line))

    ui.binding.notifier = record
    ui.anchor.mod("wiring")._UI[BOT] = ui.binding
    try:
        ui.anchor.mod("models.runtime").note_unavailable((BOT, GUILD, "public"), "no credential", "deepseek-chat")
        await asyncio.gather(*list(ui.binding._tasks))
        [(_, line)] = dms
        assert "not answered" in line and "close" in line
    finally:
        await h.close()


async def test_the_message_reaches_the_operators_when_k8_notices_on_another_thread(ui):
    """Hermes resolves a session's model on worker threads too, some running a loop of their own: the message
    is always handed to the bot client's loop (its HTTP session lives there)."""
    import asyncio

    ui.ops.set_models(ui.store, BOT, GUILD, {"models.public": DS, "models.operator": DS}, by="cli")
    h = await harness(ui)
    dms, loops = [], []

    async def record(user_id, line, file=None):
        dms.append((user_id, line))
        loops.append(asyncio.get_running_loop())             # where the bot's HTTP session would be used

    ui.binding.notifier = record
    ui.anchor.mod("wiring")._UI[BOT] = ui.binding
    runtime = ui.anchor.mod("models.runtime")

    async def arrived(count):
        for _ in range(100):
            if len(dms) >= count:
                return True
            await asyncio.sleep(0.02)
        return False

    async def on_a_loop_of_its_own():
        runtime.note_unavailable((BOT, GUILD, "operator"), "no credential", "deepseek-chat")

    try:
        await asyncio.to_thread(runtime.note_unavailable, (BOT, GUILD, "public"), "no credential", "deepseek-chat")
        assert await arrived(1) and "members' turns" in dms[0][1]
        await asyncio.to_thread(lambda: asyncio.run(on_a_loop_of_its_own()))
        assert await arrived(2) and "operator turns" in dms[1][1]
        assert loops == [asyncio.get_running_loop()] * 2 == [h.client.loop] * 2
    finally:
        await h.close()


async def test_a_message_before_the_interface_is_bound_is_sent_by_the_next_occurrence(ui):
    """K8 can notice an unavailable model before ``/policy`` is bound (or while it is off): nothing is lost,
    the next occurrence sends it."""
    import asyncio

    ui.ops.set_models(ui.store, BOT, GUILD, {"models.public": DS}, by="cli")
    h = await harness(ui)
    dms = []

    async def record(user_id, line, file=None):
        dms.append((user_id, line))

    ui.binding.notifier = record
    wiring, runtime = ui.anchor.mod("wiring"), ui.anchor.mod("models.runtime")
    try:
        assert BOT not in wiring._UI
        runtime.note_unavailable((BOT, GUILD, "public"), "no credential", "deepseek-chat")
        await asyncio.sleep(0)
        assert dms == []
        wiring._UI[BOT] = ui.binding
        runtime.note_unavailable((BOT, GUILD, "public"), "no credential", "deepseek-chat")
        await asyncio.gather(*list(ui.binding._tasks))
        assert len(dms) == 1
    finally:
        await h.close()
