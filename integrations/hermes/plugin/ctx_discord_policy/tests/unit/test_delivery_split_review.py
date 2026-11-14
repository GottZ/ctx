"""S6 split delivery, review round: the delivery shapes Hermes really uses (progress and interim sends of
channel sources), the final answer only at the turn end, failures inside the worklog, per-bot state, the
fail-closed edges of the public guard and the reply mode."""

from __future__ import annotations

from unit.delivery_fakes import (GUILD_A, MEMBER, OPERATOR, ProcessingOutcome, Scene, SendResult,
                                 make_adapter_class)


async def _tool_turn(scene, guild, text="please investigate"):
    msg = scene.world.post(guild.general, text, OPERATOR, "Ada")
    route = scene.route(msg)
    route.tool_started = True
    return msg, route


async def test_progress_and_interim_sends_of_a_channel_source_go_into_the_worklog(anchor):
    """For a message in a plain channel Hermes sends progress with ``{"non_conversational": True}`` and
    interim commentary with ``{"_interim_send": True, "reply_to_message_id": …}``, without a reply anchor."""
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    with scene.in_turn(route):
        await scene.adapter.send(chat_id=origin, content="terminal: cat build.log", reply_to=None,
                                 metadata={"non_conversational": True})
        await scene.adapter.send(chat_id=origin, content="Looking at the log now.",
                                 metadata={"reply_to_message_id": str(msg.id), "_interim_send": True})
        # explicit sends (send_message tool shapes) keep their target
        await scene.adapter.send(chat_id=origin, content="explicit", metadata=None)
        await scene.adapter.send(chat_id=origin, content="explicit thread", metadata={"thread_id": origin})
        # an anchor of another message is not this turn's output
        await scene.adapter.send(chat_id=origin, content="other anchor",
                                 metadata={"reply_to_message_id": "910000000000009999", "_interim_send": True})
    [thread] = scene.threads_of(a.work)
    assert scene.world.sent_to(thread.id) == ["terminal: cat build.log", "Looking at the log now."]
    assert scene.world.sent_to(origin) == [f"Working on this in <#{thread.id}>.", "explicit", "explicit thread",
                                           "other anchor"]


async def test_only_the_last_final_of_a_successful_turn_reaches_the_origin(anchor):
    """Streaming marks segment ends between tool calls ``finalize=True`` too: those stay in the worklog."""
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    with scene.in_turn(route):
        seg1 = await scene.adapter.send(origin, "Checking the queue…", metadata={"scope_id": GUILD_A})
        await scene.adapter.edit_message(origin, seg1.message_id, "Checking the queue: it has 3 stuck jobs", finalize=True)
        await scene.adapter.send(origin, "terminal: requeue", metadata={"non_conversational": True})
        seg2 = await scene.adapter.send(origin, "Done", metadata={"scope_id": GUILD_A})
        await scene.adapter.edit_message(origin, seg2.message_id, "Done: the queue is empty again.", finalize=True)
    await scene.complete(msg)
    [thread] = scene.threads_of(a.work)
    assert scene.world.sent_to(origin) == [f"Working on this in <#{thread.id}>.", "Done: the queue is empty again."]

    # A cancelled or failed turn copies nothing.
    for outcome in ("cancelled", ProcessingOutcome.FAILURE):
        msg2, route2 = await _tool_turn(scene, a, "second")
        with scene.in_turn(route2):
            sent = await scene.adapter.send(origin, "partial", metadata={"scope_id": GUILD_A, "notify": True})
            assert sent.success
        await scene.complete(msg2, outcome)
        assert "partial" not in scene.world.sent_to(origin)


async def test_failed_origin_copy_never_repeats_the_worklog_delivery(anchor):
    """Without the turn-end hook the copy happens inline; its failure must not turn into a failed send."""
    cls = make_adapter_class()
    del cls.on_processing_complete
    scene = Scene(anchor, install=False)
    scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    scene.adapter = cls(scene.world)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)

    def origin_rejects(adapter, chat_id, content, reply_to, metadata):
        if chat_id == origin and (metadata or {}).get(scene.worklog.ORIGIN_COPY_KEY):
            return SendResult(success=False, error="Missing Permissions")
        return None

    scene.world.send_hook = origin_rejects
    with scene.in_turn(route):
        result = await scene.adapter.send(origin, "all done", reply_to=str(msg.id), metadata={"notify": True})
    assert result.success
    [thread] = scene.threads_of(a.work)
    assert scene.world.sent_to(thread.id) == ["all done"]


async def test_a_failed_worklog_delivery_falls_back_to_the_origin(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    with scene.in_turn(route):
        await scene.adapter.send(origin, "step 1", metadata={"scope_id": GUILD_A})
        [thread] = scene.threads_of(a.work)
        del scene.world.channels[thread.id]            # thread deleted mid-turn: the stock send fails
        final = await scene.adapter.send(origin, "the answer", reply_to=str(msg.id), metadata={"notify": True})
        later = await scene.adapter.send(origin, "late note", metadata={"scope_id": GUILD_A})
    assert final.success and later.success
    assert scene.world.sent_to(origin)[-2:] == ["the answer", "late note"]


async def test_a_retryable_worklog_failure_is_returned_for_hermes_to_retry(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    failures = []

    def flaky(adapter, chat_id, content, reply_to, metadata):
        if (metadata or {}).get("thread_id") and not failures:
            failures.append(chat_id)
            return SendResult(success=False, error="send_path_degraded", retryable=True)
        return None

    with scene.in_turn(route):
        await scene.adapter.send(origin, "opens the worklog", metadata={"scope_id": GUILD_A})
        scene.world.send_hook = flaky
        first = await scene.adapter.send(origin, "step", metadata={"scope_id": GUILD_A})
        second = await scene.adapter.send(origin, "step", metadata={"scope_id": GUILD_A})
    [thread] = scene.threads_of(a.work)
    assert not first.success and first.retryable and second.success
    assert scene.world.sent_to(thread.id) == ["opens the worklog", "step"]
    assert "step" not in scene.world.sent_to(origin)


async def test_redirected_final_keeps_the_ledger_anchor_of_the_origin_message(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    await scene.adapter.send(str(a.general.id), "all done", reply_to=str(msg.id), metadata={"notify": True})
    redirected = [c for c in scene.adapter.calls if c[0] == "send" and c[2] == "all done"][0]
    assert redirected[3] is None and redirected[4]["reply_to_message_id"] == str(msg.id)


async def test_continuations_of_a_final_edit_stay_addressable(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    origin = str(a.general.id)
    with scene.in_turn(route):
        first = await scene.adapter.send(origin, "head", metadata={"scope_id": GUILD_A})
    [thread] = scene.threads_of(a.work)

    async def split_edit(chat_id, message_id, content):
        channel = scene.world.get_channel(int(chat_id))
        await channel.get_partial_message(int(message_id)).edit(content=content[:5])
        tail = await channel.send(content=content[5:])
        return SendResult(success=True, message_id=str(tail.id), continuation_message_ids=(str(tail.id),))

    scene.world.edit_hook = lambda adapter, chat_id, message_id, content, finalize: (
        split_edit(chat_id, message_id, content) if finalize else None)
    result = await scene.adapter.edit_message(origin, first.message_id, "head and a long tail", finalize=True)
    scene.world.edit_hook = None
    again = await scene.adapter.edit_message(origin, result.message_id, "tail, corrected")
    assert again.success
    assert thread.edits[-1] == (int(result.message_id), "tail, corrected")


async def test_two_bots_admitting_one_message_keep_separate_worklogs(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    second_bot = scene.cls(scene.world)
    with scene.in_turn(route):
        await scene.adapter.send(str(a.general.id), "bot one", metadata={"scope_id": GUILD_A})
        await second_bot.send(str(a.general.id), "bot two", metadata={"scope_id": GUILD_A})
    threads = scene.threads_of(a.work)
    assert len(threads) == 2
    assert sorted(scene.world.sent_to(t.id)[0] for t in threads) == ["bot one", "bot two"]


async def test_public_guard_covers_the_worklog_channel_and_unresolvable_targets(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.work, "what is this channel", MEMBER, "Mallory")
    route = scene.route(msg, lane="public")
    hidden = "920000000000099999"          # a channel Discord serves but this process cannot resolve
    delivered = []

    def serve_hidden(adapter, chat_id, content, reply_to, metadata):
        if chat_id == hidden:
            delivered.append(content)
            return SendResult(success=True, message_id="1")
        return None

    scene.world.send_hook = serve_hidden
    with scene.in_turn(route):
        in_work = await scene.adapter.send(str(a.work.id), "reply", metadata={"scope_id": GUILD_A})
        unknown = await scene.adapter.send(hidden, "reply", metadata={"scope_id": GUILD_A})
    assert not in_work.success and not unknown.success
    assert scene.world.log == [] and delivered == []


async def test_media_reply_reference_honours_reply_mode_off(anchor):
    scene = Scene(anchor)
    scene.adapter._reply_to_mode = "off"
    a = scene.guild(GUILD_A)
    msg, route = await _tool_turn(scene, a)
    with scene.in_turn(route):
        await scene.adapter.send_document(str(a.general.id), "report.pdf", metadata={"scope_id": GUILD_A})
    media = [kwargs for chat, content, kwargs in scene.world.log if chat == str(a.general.id) and "file" in kwargs]
    assert media == [{"file": "report.pdf"}]
