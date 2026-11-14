"""S6 split delivery (K3): turn output into the worklog, the final answer back to the origin, edits follow
their message; explicit sends stay where they were aimed; install, idempotence and the stock fallback."""

from __future__ import annotations

import asyncio
import inspect

from unit.delivery_fakes import (GUILD_A, GUILD_B, OPERATOR, ProcessingOutcome, Scene, SendResult, World,
                                 make_adapter_class, runner_metadata)


async def _tool_turn(scene, guild, text="please investigate"):
    msg = scene.world.post(guild.general, text, OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    return msg, route


async def test_progress_in_worklog_final_copied_to_origin_once(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    with scene.in_turn(route):
        preview = await scene.adapter.send(origin, "working…", reply_to=str(msg.id),
                                           metadata=runner_metadata(route, expect_edits=True))
        await scene.adapter.edit_message(origin, preview.message_id, "working… 50%", metadata=runner_metadata(route))
        final = await scene.adapter.edit_message(origin, preview.message_id, "final answer", finalize=True,
                                                 metadata=runner_metadata(route))
        again = await scene.adapter.edit_message(origin, preview.message_id, "final answer", finalize=True,
                                                 metadata=runner_metadata(route))
    assert scene.world.sent_to(origin) == [f"Working on this in <#{scene.threads_of(a.work)[0].id}>."]
    await scene.complete(msg)
    await scene.complete(msg)      # a second completion (e.g. drain) never copies twice
    assert final.success and again.success
    [thread] = scene.threads_of(a.work)
    assert thread.messages[int(preview.message_id)].content == "final answer"
    assert [e[1] for e in thread.edits] == ["working… 50%", "final answer", "final answer"]
    # The origin got the pointer and exactly one copy of the final answer, as a reply to the request.
    assert scene.world.sent_to(origin) == [f"Working on this in <#{thread.id}>.", "final answer"]
    copy_call = [c for c in scene.adapter.calls if c[0] == "send" and c[1] == origin][-1]
    assert copy_call[3] == str(msg.id) and copy_call[4]["notify"] is True


async def test_racing_final_copies_produce_one_origin_reply(anchor):
    """Without the turn-end hook every final-marked delivery copies at once: still exactly one copy."""
    cls = make_adapter_class()
    del cls.on_processing_complete
    scene = Scene(anchor, install=False)
    scene.cls = cls
    k3, _ = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert "final copy per final-marked delivery" in k3.reason
    scene.adapter = cls(scene.world)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    with scene.in_turn(route):
        first = await scene.adapter.send(origin, "step", metadata=runner_metadata(route))
        await asyncio.gather(*(scene.adapter.edit_message(origin, first.message_id, "same final", finalize=True)
                               for _ in range(3)))
    assert scene.world.sent_to(origin).count("same final") == 1


async def test_final_to_origin_off_keeps_the_final_answer_in_the_worklog(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A, final_to_origin=False)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    with scene.in_turn(route):
        await scene.adapter.send(origin, "the answer", reply_to=str(msg.id), metadata=runner_metadata(route, notify=True))
    await scene.complete(msg)
    [thread] = scene.threads_of(a.work)
    assert scene.world.sent_to(thread.id) == ["the answer"]
    assert scene.world.sent_to(origin) == [f"Working on this in <#{thread.id}>."]


async def test_base_adapter_final_send_without_turn_context_uses_the_reply_anchor(anchor):
    """Hermes' base adapter sends the non-streamed final answer after the runner returned (no route
    context): the reply anchor identifies the route in the registry."""
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    result = await scene.adapter.send(origin, "all done", reply_to=str(msg.id), metadata={"notify": True})
    await scene.complete(msg)
    assert result.success
    [thread] = scene.threads_of(a.work)
    assert scene.world.sent_to(thread.id) == ["all done"]
    assert scene.world.sent_to(origin) == [f"Working on this in <#{thread.id}>.", "all done"]
    # A replay of the origin copy (delivery ledger) is never routed again.
    replay = await scene.adapter.send(origin, "all done", reply_to=str(msg.id),
                                      metadata={"notify": True, scene.worklog.ORIGIN_COPY_KEY: True})
    assert replay.success and scene.world.sent_to(thread.id) == ["all done"]


async def test_edits_follow_the_message_to_where_it_was_delivered(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "look into it", OPERATOR, "Ada")
    route = scene.route(msg)
    origin = str(a.general.id)
    with scene.in_turn(route):
        early = await scene.adapter.send(origin, "let me check", metadata=runner_metadata(route))
        route.tool_started = True
        later = await scene.adapter.send(origin, "tool: grep", metadata=runner_metadata(route))
        e1 = await scene.adapter.edit_message(origin, early.message_id, "let me check (done)", finalize=True)
        e2 = await scene.adapter.edit_message(origin, later.message_id, "tool: grep (3 hits)")
    assert e1.success and e2.success
    [thread] = scene.threads_of(a.work)
    assert a.general.edits == [(int(early.message_id), "let me check (done)")]
    assert thread.edits == [(int(later.message_id), "tool: grep (3 hits)")]
    # An origin-resident message finalized after the tool start is not an origin copy.
    assert scene.world.sent_to(origin).count("let me check (done)") == 0


async def test_explicit_sends_are_never_rerouted(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    b = scene.guild(GUILD_B)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    with scene.in_turn(route):
        # e.g. a send_message tool call: no reply anchor, no guild scope in the metadata
        await scene.adapter.send(origin, "announcement")
        # a send aimed at another guild's channel during the turn
        await scene.adapter.send(str(b.general.id), "cross post", metadata={"scope_id": GUILD_A})
    assert scene.world.sent_to(origin) == ["announcement"]
    assert scene.world.sent_to(b.general.id) == ["cross post"]
    assert scene.threads_of(a.work) == [] and scene.threads_of(b.work) == []


async def test_route_stamp_marks_turn_output(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    stamped = scene.worklog.stamp_metadata(None, route)
    await scene.adapter.send(str(a.general.id), "stamped output", metadata=stamped)
    [thread] = scene.threads_of(a.work)
    assert scene.world.sent_to(thread.id) == ["stamped output"]


async def test_install_wraps_six_methods_idempotently_and_keeps_signatures(anchor):
    scene = Scene(anchor)
    k3, k4 = scene.results
    assert (k3.intervention, k3.outcome.value) == ("K3", "wrapped")
    assert (k4.intervention, k4.outcome.value) == ("K4", "wrapped")
    names = ("send", "edit_message", "send_multiple_images", "send_voice", "send_video", "send_document")
    for name in names:
        assert getattr(scene.cls, name).__dsp_wrapper__[0] == "K3"
    assert "metadata" in inspect.signature(scene.cls.edit_message).parameters
    before = {name: getattr(scene.cls, name) for name in names}
    again = scene.split.install(scene.cls, scene.configs, anchor.RUNTIME.status)
    assert [r.outcome.value for r in again] == ["wrapped", "wrapped"]
    assert again[0].reason.startswith("already wrapped")
    assert all(getattr(scene.cls, name) is before[name] for name in names)
    assert anchor.RUNTIME.status.get("K3").outcome.value == "wrapped"


async def test_wrappers_pass_unknown_arguments_through(anchor):
    cls = make_adapter_class()
    seen = {}

    async def send(self, chat_id, content, reply_to=None, metadata=None, *, silent=False, **extra):
        seen.update(chat_id=chat_id, silent=silent, extra=extra)
        return SendResult(success=True, message_id="1")

    cls.send = send
    scene = Scene(anchor, install=False)
    scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    result = await cls(scene.world).send("123", "x", None, None, silent=True, nonce="n1")
    assert result.success and seen == {"chat_id": "123", "silent": True, "extra": {"nonce": "n1"}}


async def test_probe_mismatch_falls_back_to_stock_and_reports_it(anchor):
    scene = Scene(anchor, install=False)
    cls = make_adapter_class()

    async def edit_message(self, chat_id, message_id, content):     # no finalize/metadata: shape changed
        return SendResult(success=True)

    cls.edit_message = edit_message
    originals = {name: cls.__dict__[name] for name in ("send", "edit_message", "send_video")}
    k3, _ = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert k3.outcome.value == "fallback"
    assert "operator tool output stays visible in the origin channel" in k3.reason
    assert anchor.RUNTIME.status.get("K3").outcome.value == "fallback"
    assert all(cls.__dict__[name] is fn for name, fn in originals.items())  # originals untouched
    # Stock delivery: an operator tool turn stays in the origin channel.
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    with scene.in_turn(route):
        assert (await cls(scene.world).send(str(a.general.id), "tool output", metadata=runner_metadata(route))).success
    assert scene.world.sent_to(a.general.id) == ["tool output"] and scene.threads_of(a.work) == []


async def test_sync_send_is_a_probe_mismatch(anchor):
    scene = Scene(anchor, install=False)
    cls = make_adapter_class()
    cls.send = lambda self, chat_id, content, reply_to=None, metadata=None: SendResult(success=True)
    k3, _ = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert k3.outcome.value == "fallback" and "sync" in k3.reason
    assert not hasattr(cls.send, "__dsp_wrapper__")


async def test_one_media_sender_missing_keeps_the_rest(anchor):
    scene = Scene(anchor, install=False)
    cls = make_adapter_class()
    del cls.send_video
    k3, _ = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert k3.outcome.value == "wrapped" and "send_video stock" in k3.reason
    assert cls.send_document.__dsp_wrapper__[0] == "K3"


async def test_a_media_sender_that_cannot_be_wrapped_stays_stock_without_undoing_send(anchor):
    scene = Scene(anchor, install=False)
    cls = make_adapter_class()

    async def send_video(chat_id, video_path, caption=None, reply_to=None, metadata=None):
        return SendResult(success=True)

    cls.send_video = staticmethod(send_video)   # probe-compatible shape, but not wrappable as a method
    k3, _ = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert k3.outcome.value == "wrapped" and "send_video stock (TypeError" in k3.reason
    assert cls.send.__dsp_wrapper__[0] == "K3" and cls.edit_message.__dsp_wrapper__[0] == "K3"


async def test_routing_errors_degrade_to_stock_delivery(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)

    def broken(bot_id, guild_id):
        raise RuntimeError("store unreadable")

    scene.worklog.DELIVERY.configure(broken)
    with scene.in_turn(route):
        result = await scene.adapter.send(str(a.general.id), "tool output", metadata=runner_metadata(route))
    assert result.success and scene.world.sent_to(a.general.id) == ["tool output"]


async def test_install_rejects_a_missing_configuration_accessor(anchor):
    scene = Scene(anchor, install=False)
    try:
        scene.split.install(scene.cls, None, anchor.RUNTIME.status)
    except TypeError as exc:
        assert "get_guild_config" in str(exc)
    else:
        raise AssertionError("install accepted no configuration accessor")
