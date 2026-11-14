"""S6 media (K3 media half): with a worklog, attachments go to the origin channel as a reply to the request;
without one they keep stock delivery; a non-operator route never delivers media into a worklog thread."""

from __future__ import annotations

from unit.delivery_fakes import GUILD_A, MEMBER, OPERATOR, Scene, runner_metadata


async def test_media_of_a_worklog_turn_reply_in_the_origin_channel(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "render the chart", OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    origin = str(a.general.id)
    with scene.in_turn(route):
        await scene.adapter.send_multiple_images(origin, [("file:///chart.png", "chart")],
                                                 metadata=runner_metadata(route))
        await scene.adapter.send_document(origin, "report.pdf", metadata=runner_metadata(route))
        await scene.adapter.send_video(origin, "clip.mp4", metadata=runner_metadata(route))
        await scene.adapter.send_voice(origin, "note.ogg", metadata=runner_metadata(route), is_voice=True)
    [thread] = scene.threads_of(a.work)
    # The worklog was opened before the first attachment, so the pointer precedes the media.
    origin_log = [(content, kwargs) for chat, content, kwargs in scene.world.log if chat == origin]
    assert origin_log[0][0] == f"Working on this in <#{thread.id}>."
    media = origin_log[1:]
    assert len(media) == 4
    expected_reference = ("reference", str(msg.id), origin)
    for _, kwargs in media[:3]:
        assert kwargs["reference"] == expected_reference
    voice_call = [c for c in scene.adapter.calls if c[0] == "voice"][0]
    assert voice_call[3] == str(msg.id) and voice_call[4] == {}
    assert scene.world.sent_to(thread.id) == []
    # The shared adapter was never modified (the reply reference lives in a per-call copy).
    assert scene.adapter._client is scene.world


async def test_media_without_worklog_keep_stock_delivery(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A, worklog=False)
    msg = scene.world.post(a.general, "render", OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    with scene.in_turn(route):
        await scene.adapter.send_document(str(a.general.id), "report.pdf", metadata=runner_metadata(route))
    [(chat, content, kwargs)] = scene.world.log
    assert chat == str(a.general.id) and "reference" not in kwargs


async def test_explicit_media_send_is_not_touched(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "render", OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    with scene.in_turn(route):
        await scene.adapter.send_document(str(a.general.id), "report.pdf")   # send_message tool shape
    assert scene.threads_of(a.work) == []
    [(chat, _, kwargs)] = scene.world.log
    assert chat == str(a.general.id) and "reference" not in kwargs


async def test_public_media_into_a_worklog_thread_are_refused(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    thread = scene.world.channel(scene.world.new_id(), GUILD_A, parent_id=a.work.id)
    msg = scene.world.post(thread, "draw me something", MEMBER, "Mallory")
    route = scene.route(msg, lane="public")
    with scene.in_turn(route):
        results = [
            await scene.adapter.send_multiple_images(str(thread.id), [("file:///a.png", "")]),
            await scene.adapter.send_voice(str(thread.id), "a.ogg", reply_to=str(msg.id)),
            await scene.adapter.send_video(str(thread.id), "a.mp4"),
        ]
    assert not any(r.success for r in results)
    assert scene.world.log == []
    # In an ordinary channel the same public turn keeps stock media delivery.
    msg2 = scene.world.post(a.general, "draw", MEMBER, "Mallory")
    route2 = scene.route(msg2, lane="public")
    with scene.in_turn(route2):
        assert (await scene.adapter.send_video(str(a.general.id), "b.mp4", metadata=runner_metadata(route2))).success
    assert scene.world.sent_to(a.general.id) == [None]
