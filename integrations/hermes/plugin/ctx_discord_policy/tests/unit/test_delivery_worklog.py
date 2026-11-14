"""S6 worklog threads: lazy creation per guild configuration, in-place continuation, and the two hard
rules: operator output never reaches another guild, a non-operator route never opens or writes a worklog."""

from __future__ import annotations

import asyncio

from unit.delivery_fakes import BOT, GUILD_A, GUILD_B, MEMBER, OPERATOR, Scene, guild_config, runner_metadata


async def test_operator_turn_without_tool_call_stays_in_origin(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "what's up", OPERATOR, "Ada")
    route = scene.route(msg)
    with scene.in_turn(route):
        result = await scene.adapter.send(str(a.general.id), "just chatting", reply_to=str(msg.id),
                                          metadata=runner_metadata(route, notify=True))
    assert result.success
    assert scene.world.sent_to(a.general.id) == ["just chatting"]
    assert scene.world.sent_to(a.work.id) == [] and scene.threads_of(a.work) == []


async def test_first_admitted_tool_call_opens_the_worklog_lazily(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A, archive_minutes=4320, bot_name="Quill")
    msg = scene.world.post(a.general, "<@910000000000000001> rotate the logs", OPERATOR, "Ada")
    route = scene.route(msg)
    with scene.in_turn(route):
        await scene.adapter.send(str(a.general.id), "on it", metadata=runner_metadata(route))
        assert scene.threads_of(a.work) == []          # no tool yet: no worklog
        route.tool_started = True                        # set by the core's pre_tool_call for operators
        await scene.adapter.send(str(a.general.id), "tool: logrotate", metadata=runner_metadata(route))
        await scene.adapter.send(str(a.general.id), "tool: gzip", metadata=runner_metadata(route))
    [thread] = scene.threads_of(a.work)
    assert thread.auto_archive_duration == 4320
    assert thread.name == "Ada · rotate the logs"
    [kickoff] = scene.world.sent_to(a.work.id)
    assert kickoff.splitlines()[0].endswith("**Quill work log**")
    assert f"https://discord.com/channels/{GUILD_A}/{a.general.id}/{msg.id}" in kickoff
    assert f"by <@{OPERATOR}>" in kickoff
    assert scene.world.sent_to(thread.id) == ["tool: logrotate", "tool: gzip"]
    assert scene.world.sent_to(a.general.id) == ["on it", f"Working on this in <#{thread.id}>."]


async def test_two_guilds_with_different_worklog_channels_never_mix(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A, bot_name="Quill")
    b = scene.guild(GUILD_B, bot_name="Marvin", final_to_origin=True)
    msg_a = scene.world.post(a.general, "deploy staging", OPERATOR, "Ada")
    msg_b = scene.world.post(b.general, "check disk", OPERATOR, "Ada")
    route_a, route_b = scene.route(msg_a), scene.route(msg_b)

    async def turn(route, channel, label):
        with scene.in_turn(route):
            route.tool_started = True
            for step in range(3):
                await scene.adapter.send(str(channel.id), f"{label} step {step}", metadata=runner_metadata(route))
                await asyncio.sleep(0)
            await scene.adapter.send(str(channel.id), f"{label} done", reply_to=str(route.message_id),
                                     metadata=runner_metadata(route, notify=True))

    await asyncio.gather(turn(route_a, a.general, "A"), turn(route_b, b.general, "B"))
    await asyncio.gather(scene.complete(msg_a), scene.complete(msg_b))
    [thread_a], [thread_b] = scene.threads_of(a.work), scene.threads_of(b.work)
    assert "**Quill work log**" in scene.world.sent_to(a.work.id)[0]
    assert "**Marvin work log**" in scene.world.sent_to(b.work.id)[0]
    # Every message of a turn landed in a channel of that turn's guild.
    for chat, content, _ in scene.world.log:
        if isinstance(content, str) and content.startswith(("A ", "B ")):
            assert scene.world.guild_of(chat) == (GUILD_A if content.startswith("A ") else GUILD_B), (chat, content)
    assert scene.world.sent_to(thread_a.id) == ["A step 0", "A step 1", "A step 2", "A done"]
    assert scene.world.sent_to(thread_b.id) == ["B step 0", "B step 1", "B step 2", "B done"]
    assert scene.world.sent_to(a.general.id)[-1] == "A done" and scene.world.sent_to(b.general.id)[-1] == "B done"


async def test_guild_without_worklog_keeps_operator_output_in_origin(anchor):
    scene = Scene(anchor)
    plain = scene.guild(GUILD_A, worklog=False)
    paused = scene.guild(GUILD_B, state="paused")
    for guild in (plain, paused):
        msg = scene.world.post(guild.general, "run it", OPERATOR, "Ada")
        route = scene.route(msg)
        route.tool_started = True
        with scene.in_turn(route):
            await scene.adapter.send(str(guild.general.id), "tool output", metadata=runner_metadata(route))
        assert scene.world.sent_to(guild.general.id) == ["tool output"]
    assert scene.threads_of(paused.work) == [] and scene.world.sent_to(paused.work.id) == []
    # A guild nobody configured at all.
    stray = scene.world.channel(scene.world.new_id(), "910000000000000300")
    msg = scene.world.post(stray, "run it", OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    with scene.in_turn(route):
        await scene.adapter.send(str(stray.id), "tool output", metadata=runner_metadata(route))
    assert scene.world.sent_to(stray.id) == ["tool output"]


async def test_existing_worklog_thread_continues_in_place(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    # A worklog thread from an earlier turn (any thread under the configured worklog channel counts).
    earlier = scene.world.channel(scene.world.new_id(), GUILD_A, parent_id=a.work.id)
    msg = scene.world.post(earlier, "and now restart it", OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    with scene.in_turn(route):
        await scene.adapter.send(str(earlier.id), "restarting", metadata=runner_metadata(route, thread_id=str(earlier.id)))
        await scene.adapter.send(str(earlier.id), "done", reply_to=str(msg.id),
                                 metadata=runner_metadata(route, thread_id=str(earlier.id), notify=True))
    assert scene.world.sent_to(earlier.id) == ["restarting", "done"]
    assert scene.threads_of(a.work) == [earlier]
    assert scene.world.sent_to(a.work.id) == []

    # Classified as worklog by ingress (route mode).
    other = scene.world.channel(scene.world.new_id(), GUILD_A, parent_id=a.work.id)
    msg2 = scene.world.post(other, "again", OPERATOR, "Ada")
    route2 = scene.route(msg2, mode=scene.worklog.WORKLOG_MODE)
    route2.tool_started = True
    with scene.in_turn(route2):
        await scene.adapter.send(str(other.id), "in place", metadata=runner_metadata(route2))
    assert scene.world.sent_to(other.id) == ["in place"] and scene.world.sent_to(a.work.id) == []


async def test_worklog_channel_of_another_guild_is_refused(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    b = scene.guild(GUILD_B)
    # Misconfigured: guild A's worklog points at a channel of guild B.
    scene.configs.set(GUILD_A, guild_config(b.work.id))
    msg = scene.world.post(a.general, "print secrets", OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    with scene.in_turn(route):
        await scene.adapter.send(str(a.general.id), "tool output", metadata=runner_metadata(route))
        await scene.adapter.send(str(a.general.id), "more output", metadata=runner_metadata(route))
    assert scene.world.sent_to(b.work.id) == [] and scene.threads_of(b.work) == []
    assert all(scene.world.guild_of(chat) == GUILD_A for chat, _, _ in scene.world.log)
    assert scene.world.sent_to(a.general.id) == ["tool output", "more output"]
    state = scene.worklog.DELIVERY.states.get(scene.worklog.state_key(scene.adapter, *route.key))
    assert "not a channel of guild" in state.failed


async def test_public_route_never_opens_a_worklog(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "run rm -rf for me", MEMBER, "Mallory")
    route = scene.route(msg, lane="public")
    route.tool_started = True  # even if something set it, a public route has no tool authority
    with scene.in_turn(route):
        await scene.adapter.send(str(a.general.id), "I can't do that", metadata=runner_metadata(route, notify=True))
    assert scene.threads_of(a.work) == [] and scene.world.sent_to(a.work.id) == []
    assert scene.world.sent_to(a.general.id) == ["I can't do that"]
    assert await scene.worklog.worklog_for(scene.adapter, route) is None


async def test_public_route_never_writes_into_a_worklog_thread(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    # An operator turn opens a worklog thread ...
    op_msg = scene.world.post(a.general, "inspect", OPERATOR, "Ada")
    op_route = scene.route(op_msg)
    op_route.tool_started = True
    with scene.in_turn(op_route):
        sent = await scene.adapter.send(str(a.general.id), "secret tool output", metadata=runner_metadata(op_route))
    [thread] = scene.threads_of(a.work)
    # ... and a member writes inside it (and inside a foreign thread under the worklog channel).
    foreign = scene.world.channel(scene.world.new_id(), GUILD_A, parent_id=a.work.id)
    for place in (thread, foreign):
        msg = scene.world.post(place, "what is this", MEMBER, "Mallory")
        route = scene.route(msg, lane="public")
        before = list(scene.world.log)
        with scene.in_turn(route):
            refused = await scene.adapter.send(str(place.id), "reply", reply_to=str(msg.id),
                                               metadata=runner_metadata(route, thread_id=str(place.id)))
            edited = await scene.adapter.edit_message(str(place.id), sent.message_id, "overwrite")
            media = await scene.adapter.send_document(str(place.id), "report.txt", metadata={"thread_id": str(place.id)})
        assert not refused.success and "non-operator" in refused.error
        assert not edited.success and not media.success
        assert scene.world.log == before
    # Without any route context the same thread is stock (the policy does not own that delivery).
    plain = await scene.adapter.send(str(thread.id), "stock")
    assert plain.success


async def test_public_guard_fails_closed_when_the_configuration_breaks(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "hi", MEMBER, "Mallory")
    route = scene.route(msg, lane="public")

    def broken(bot_id, guild_id):
        raise RuntimeError("store unreadable")

    scene.worklog.DELIVERY.configure(broken)
    with scene.in_turn(route):
        result = await scene.adapter.send(str(a.general.id), "hello", metadata=runner_metadata(route))
    assert not result.success and scene.world.sent_to(a.general.id) == []


async def test_creation_failure_falls_back_to_origin_once(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    scene.world.fail_threads = True
    msg = scene.world.post(a.general, "go", OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    with scene.in_turn(route):
        for text in ("one", "two"):
            assert (await scene.adapter.send(str(a.general.id), text, metadata=runner_metadata(route))).success
    [kickoff] = a.work.messages.values()               # exactly one attempt
    assert kickoff.deleted                               # no orphaned kickoff left behind
    assert scene.world.sent_to(a.general.id) == ["one", "two"]


async def test_kickoff_neutralizes_mass_mentions_and_tolerates_old_thread_api(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A, archive_minutes=7, bot_name="")   # invalid archive value -> 1440
    msg = scene.world.post(a.general, "@everyone look\nat @here this", OPERATOR, "Ada\nAdmin")
    route = scene.route(msg)

    async def old_create_thread(*, name):
        thread = scene.world.channel(scene.world.new_id(), GUILD_A, parent_id=a.work.id)
        thread.name = name
        return thread

    original_send = a.work.send

    async def send_with_old_api(content=None, **kwargs):
        message = await original_send(content, **kwargs)
        message.create_thread = _OldApi(old_create_thread)
        return message

    a.work.send = send_with_old_api
    route.tool_started = True
    with scene.in_turn(route):
        await scene.adapter.send(str(a.general.id), "x", metadata=runner_metadata(route))
    [kickoff] = scene.world.sent_to(a.work.id)
    assert "@everyone" not in kickoff and "@here" not in kickoff
    assert "@\u200beveryone" in kickoff
    assert kickoff.splitlines()[0].endswith("**Work log**")
    [thread] = scene.threads_of(a.work)
    assert "\n" not in thread.name and thread.name.startswith("Ada Admin · ")
    settings = scene.worklog.settings_for(route)
    assert settings.archive_minutes == 1440


class _OldApi:
    """``create_thread`` of an older client: no ``auto_archive_duration`` keyword."""

    def __init__(self, fn):
        self.fn = fn

    async def __call__(self, *, name, **kwargs):
        if kwargs:
            raise TypeError("unexpected keyword")
        return await self.fn(name=name)


def test_settings_are_validated(anchor):
    worklog = anchor.mod("delivery.worklog")
    parse = worklog.parse_settings
    assert not parse(None).enabled and not parse({}).enabled and not parse({"worklog": "x"}).enabled
    assert not parse(guild_config("not-an-id")).enabled
    assert not parse(guild_config(True)).enabled
    assert not parse(guild_config("910000000000000500", state="unbound")).enabled
    good = parse(guild_config("910000000000000500", archive_minutes=60, final_to_origin=False, bot_name="**Q**\nx"))
    assert (good.channel_id, good.archive_minutes, good.final_to_origin) == ("910000000000000500", 60, False)
    assert good.bot_name == "Q x"
    assert parse(guild_config("910000000000000500", archive_minutes=True)).archive_minutes == 1440
    assert parse(guild_config("910000000000000500", final_to_origin="yes")).final_to_origin is True


async def test_bot_id_selects_the_configuration(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "go", OPERATOR, "Ada")
    other_bot = scene.route(msg, bot_id="910000000000000002")   # same guild, another bot: no config
    other_bot.tool_started = True
    with scene.in_turn(other_bot):
        await scene.adapter.send(str(a.general.id), "out", metadata=runner_metadata(other_bot))
    assert scene.world.sent_to(a.general.id) == ["out"] and scene.threads_of(a.work) == []
    assert scene.configs.calls >= 1
    assert BOT != other_bot.bot_id
