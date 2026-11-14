"""S6 contract: K3/K4 against the real Hermes Discord adapter and runner of each ref.

Checked per ref: the probes of all six delivery methods and of the processing hooks match (the K4
behaviour probe runs the real hooks), the base class is never modified, the runner's reply metadata carries
the guild scope that marks turn output, a real ``send``/``edit_message`` of an operator tool turn lands in
the worklog thread with the final answer copied to the origin, and the real processing hooks hide reactions
on ambient turns only.
"""

from __future__ import annotations

import inspect
from types import SimpleNamespace

import pytest

from unit.delivery_fakes import GUILD_A, GUILD_B, MEMBER, OPERATOR, Configs, World, guild_config

DELIVERY_METHODS = ("send", "edit_message", "send_multiple_images", "send_voice", "send_video", "send_document")


@pytest.fixture
def real(hermes, anchor):
    from gateway.config import Platform, PlatformConfig
    from gateway.platforms.event import MessageEvent
    from gateway.run import GatewayRunner
    from gateway.session import SessionSource
    from plugins.platforms.discord.adapter import DiscordAdapter

    base = {name: inspect.getattr_static(DiscordAdapter, name)
            for name in DELIVERY_METHODS + ("on_processing_start", "on_processing_complete")}

    class PolicyAdapter(DiscordAdapter):
        """Installing on a subclass keeps the shared class of this test process untouched."""

    configs = Configs()
    split = anchor.mod("delivery.split")
    results = split.install(PolicyAdapter, configs, anchor.RUNTIME.status)
    yield SimpleNamespace(hermes=hermes, anchor=anchor, split=split, results=results, configs=configs, base=base,
                          DiscordAdapter=DiscordAdapter, PolicyAdapter=PolicyAdapter, GatewayRunner=GatewayRunner,
                          SessionSource=SessionSource, Platform=Platform, PlatformConfig=PlatformConfig,
                          MessageEvent=MessageEvent, worklog=anchor.mod("delivery.worklog"))


def _adapter(real, world):
    adapter = real.PolicyAdapter(real.PlatformConfig(enabled=True, token="contract-test"))
    adapter._client = world
    return adapter


def _source(real, channel, message, *, thread=False, user_id=OPERATOR):
    return real.SessionSource(platform=real.Platform.DISCORD, chat_id=str(channel.id),
                              chat_type="thread" if thread else "group", user_id=user_id,
                              thread_id=str(channel.id) if thread else None,
                              parent_chat_id=str(channel.parent_id) if thread else None,
                              guild_id=str(channel.guild.id), message_id=str(message.id))


def _route(real, message, *, lane="operator", mode="direct"):
    routes = real.anchor.routes
    route = routes.Route(bot_id="910000000000000001", guild_id=str(message.channel.guild.id),
                         channel_id=str(message.channel.id), message_id=str(message.id),
                         author_id=str(message.author.id), lane=lane, mode=mode)
    real.anchor.RUNTIME.message_routes.put(route.key, route)
    return route


def test_delivery_probes_match_the_real_adapter(real):
    k3, k4 = real.results
    rows = []
    for name in DELIVERY_METHODS + ("on_processing_start", "on_processing_complete"):
        fn = inspect.unwrap(getattr(real.DiscordAdapter, name))
        rows.append({"method": name, "signature": str(inspect.signature(fn)),
                     "async": inspect.iscoroutinefunction(fn)})
    report = real.hermes.write_report("delivery", {
        "K3": k3.as_dict(), "K4": k4.as_dict(), "methods": rows})
    print(f"\n== delivery {real.hermes.ref}: K3 {k3.outcome.value} ({k3.reason}); K4 {k4.outcome.value} "
          f"({k4.reason}) -> {report}")
    assert k3.outcome.value == "wrapped", k3.reason
    assert "stock" not in k3.reason and "without origin reply reference" not in k3.reason, k3.reason
    assert "final copy at turn end" in k3.reason, k3.reason
    assert k4.outcome.value == "wrapped", k4.reason
    for name in DELIVERY_METHODS:
        assert real.PolicyAdapter.__dict__[name].__dsp_wrapper__[0] == "K3"
    for name, value in real.base.items():
        assert inspect.getattr_static(real.DiscordAdapter, name) is value, name


def test_runner_reply_metadata_marks_turn_output(real):
    world = World()
    channel = world.channel(world.new_id(), GUILD_A)
    thread = world.channel(world.new_id(), GUILD_A, parent_id=channel.id)
    runner = real.GatewayRunner.__new__(real.GatewayRunner)
    for place, is_thread in ((channel, False), (thread, True)):
        message = world.post(place, "hello", OPERATOR, "Ada")
        route = _route(real, message)
        metadata = runner._thread_metadata_for_source(_source(real, place, message, thread=is_thread),
                                                      str(message.id))
        assert real.worklog.is_turn_output(route, None, metadata), metadata
        assert real.worklog.delivery_target(str(place.id), metadata) == route.channel_id
    # An explicit send (send_message tool shape) is not turn output.
    assert not real.worklog.is_turn_output(route, None, {"thread_id": str(thread.id)})


def test_real_progress_interim_and_stream_metadata_mark_turn_output(real):
    """The lanes Hermes really uses for a message in a plain channel: progress bubbles, interim commentary,
    the stream consumer (also without reply metadata). An explicit ``send_message`` shape is not output."""
    from gateway.run import _interim_metadata, _non_conversational_metadata, _resolve_progress_thread_id
    from gateway.stream_consumer import GatewayStreamConsumer

    world = World()
    channel = world.channel(world.new_id(), GUILD_A)
    message = world.post(channel, "hello", OPERATOR, "Ada")
    route = _route(real, message)
    source = _source(real, channel, message)
    runner = real.GatewayRunner.__new__(real.GatewayRunner)
    progress_thread = _resolve_progress_thread_id(source.platform, source.thread_id, str(message.id))
    progress = _non_conversational_metadata(
        runner._thread_metadata_for_progress(source, str(message.id), progress_thread, None), platform=source.platform)
    interim = _interim_metadata(runner._thread_metadata_for_source(source, str(message.id)))
    shapes = {"progress": progress, "interim": interim}
    for metadata in (None, runner._thread_metadata_for_source(source, str(message.id))):
        consumer = GatewayStreamConsumer(adapter=None, chat_id=str(channel.id), metadata=metadata,
                                         initial_reply_to_id=str(message.id))
        shapes[f"stream/{bool(metadata)}"] = consumer._metadata_for_send(final=False, expect_edits=True)
        shapes[f"stream-final/{bool(metadata)}"] = consumer._metadata_for_send(final=True)
    for name, metadata in shapes.items():
        assert real.worklog.is_turn_output(route, None, metadata), (name, metadata)
    assert not real.worklog.is_turn_output(route, None, None)


async def test_real_send_and_edit_route_an_operator_tool_turn_into_the_worklog(real):
    world = World()
    general = world.channel(world.new_id(), GUILD_A)
    work = world.channel(world.new_id(), GUILD_A)
    foreign = world.channel(world.new_id(), GUILD_B)
    real.configs.set(GUILD_A, guild_config(work.id))
    real.configs.set(GUILD_B, guild_config(foreign.id))
    adapter = _adapter(real, world)
    message = world.post(general, "check the queue", OPERATOR, "Ada")
    route = _route(real, message)
    runner = real.GatewayRunner.__new__(real.GatewayRunner)
    metadata = runner._thread_metadata_for_source(_source(real, general, message), str(message.id))
    token = real.anchor.RUNTIME.route.set(route)
    try:
        route.tool_started = True
        progress = await adapter.send(str(general.id), "queue: 3 jobs", metadata=metadata)
        assert progress.success, progress.error
        edited = await adapter.edit_message(str(general.id), progress.message_id, "queue is healthy", finalize=True,
                                            metadata=metadata)
        assert edited.success, edited.error
    finally:
        real.anchor.RUNTIME.route.reset(token)
    [thread] = [c for c in world.channels.values() if c.parent_id == work.id]
    assert thread.messages[int(progress.message_id)].content == "queue is healthy"
    from gateway.platforms.event import ProcessingOutcome

    event = real.MessageEvent(text="check the queue", source=_source(real, general, message), raw_message=message,
                              message_id=str(message.id))
    await adapter.on_processing_complete(event, ProcessingOutcome.SUCCESS)   # the turn ends: copy the final
    origin = [(content, kwargs) for chat, content, kwargs in world.log if chat == str(general.id)]
    assert [c for c, _ in origin] == [f"Working on this in <#{thread.id}>.", "queue is healthy"]
    reference = origin[1][1]["reference"]
    assert str(reference.message_id) == str(message.id) and str(reference.channel_id) == str(general.id)
    kickoff = [kwargs for chat, _, kwargs in world.log if chat == str(work.id)][0]
    assert kickoff["allowed_mentions"].everyone is False
    assert world.sent_to(foreign.id) == []


async def test_real_processing_hooks_hide_reactions_on_ambient_turns_only(real):
    world = World()
    general = world.channel(world.new_id(), GUILD_A)
    adapter = _adapter(real, world)
    from gateway.platforms.event import ProcessingOutcome

    async def run(message, route):
        source = _source(real, general, message, user_id=route.session_user_id)   # as Hermes builds it
        event = real.MessageEvent(text=str(message.content), source=source, raw_message=message,
                                  message_id=str(message.id))
        await adapter.on_processing_start(event)
        await adapter.on_processing_complete(event, ProcessingOutcome.SUCCESS)
        return event

    ambient = world.post(general, "nice weather", MEMBER, "Mallory")
    ambient_route = _route(real, ambient, lane="public", mode="ambient")
    direct = world.post(general, "hey bot", MEMBER, "Mallory")
    direct_route = _route(real, direct, lane="public", mode="direct")
    event = await run(ambient, ambient_route)
    assert event.raw_message is ambient                  # the caller's event is never modified
    await run(direct, direct_route)
    assert ambient.reactions == []
    assert direct.reactions and direct.reactions[0][0] == "add"


async def test_real_media_of_a_worklog_turn_reply_in_the_origin(real, tmp_path):
    world = World()
    general = world.channel(world.new_id(), GUILD_A)
    work = world.channel(world.new_id(), GUILD_A)
    real.configs.set(GUILD_A, guild_config(work.id))
    adapter = _adapter(real, world)
    message = world.post(general, "export the report", OPERATOR, "Ada")
    route = _route(real, message)
    route.tool_started = True
    report = tmp_path / "report.txt"
    report.write_text("numbers\n", encoding="utf-8")
    runner = real.GatewayRunner.__new__(real.GatewayRunner)
    metadata = runner._thread_metadata_for_source(_source(real, general, message), str(message.id))
    token = real.anchor.RUNTIME.route.set(route)
    try:
        result = await adapter.send_document(str(general.id), str(report), metadata=metadata)
    finally:
        real.anchor.RUNTIME.route.reset(token)
    assert result.success, result.error
    [(content, kwargs)] = [(c, k) for chat, c, k in world.log if chat == str(general.id) and ("file" in k or "files" in k)]
    reference = kwargs["reference"]
    assert str(reference.message_id) == str(message.id)
    assert adapter._client is world
