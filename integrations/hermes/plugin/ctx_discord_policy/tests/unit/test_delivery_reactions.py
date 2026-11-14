"""S6 K4: ambient turns run without processing reactions; the durable processing record still happens."""

from __future__ import annotations

from types import SimpleNamespace

from unit.delivery_fakes import GUILD_A, MEMBER, OPERATOR, ProcessingOutcome, Scene, make_adapter_class


def _event(message, route=None):
    source = SimpleNamespace(chat_id=str(message.channel.id), message_id=str(message.id))
    return SimpleNamespace(raw_message=message, message_id=str(message.id), source=source)


async def _process(adapter, event):
    await adapter.on_processing_start(event)
    await adapter.on_processing_complete(event, ProcessingOutcome.SUCCESS)


async def test_ambient_turn_has_no_reactions_but_keeps_its_durable_record(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "nice weather", MEMBER, "Mallory")
    scene.route(msg, lane="public", mode="ambient")
    await _process(scene.adapter, _event(msg))
    assert msg.reactions == []
    records = [c for c in scene.adapter.calls if c[0].startswith("record")]
    assert records == [("record-start", str(msg.id), False), ("record-complete", str(msg.id), "success")]


async def test_direct_turns_and_unrouted_messages_keep_stock_reactions(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    direct = scene.world.post(a.general, "hey bot", MEMBER, "Mallory")
    scene.route(direct, lane="public", mode="direct")
    operator_ambient = scene.world.post(a.general, "chatting", OPERATOR, "Ada")
    scene.route(operator_ambient, lane="operator", mode="ambient")
    unrouted = scene.world.post(a.general, "stock", OPERATOR, "Ada")
    for message in (direct, unrouted):
        await _process(scene.adapter, _event(message))
        assert message.reactions == [("add", "eyes"), ("remove", "eyes"), ("add", "check")]
    await _process(scene.adapter, _event(operator_ambient))
    assert operator_ambient.reactions == []


async def test_route_found_through_the_turn_context(anchor):
    scene = Scene(anchor)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "ambient", MEMBER, "Mallory")
    route = scene.route(msg, lane="public", mode="ambient", register=False)
    with scene.in_turn(route):
        await _process(scene.adapter, _event(msg))
    assert msg.reactions == []


async def test_hooks_that_resolve_their_target_through_another_helper_still_qualify(anchor):
    """Hermes moved the hooks into a mixin that asks ``_reaction_target(event)`` first (upstream main,
    2026-11): the probe runs on an instance of the real class, so such helpers are the class's own."""
    scene = Scene(anchor, install=False)
    cls = make_adapter_class()

    async def _reaction_target(self, event):
        message = event.raw_message
        if message is not None or not self._client:
            return message
        return None

    async def on_processing_start(self, event):
        acked = False
        if self._reactions_enabled():
            message = await self._reaction_target(event)
            if hasattr(message, "add_reaction"):
                acked = await self._add_reaction(message, "eyes")
        self._record_discord_processing_start(event, emoji_ack=acked)

    async def on_processing_complete(self, event, outcome):
        self._record_discord_processing_complete(event, outcome)
        message = await self._reaction_target(event)
        if hasattr(message, "add_reaction"):
            await self._remove_reaction(message, "eyes")

    cls._reaction_target = _reaction_target
    cls.on_processing_start = on_processing_start
    cls.on_processing_complete = on_processing_complete
    _, k4 = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert k4.outcome.value == "wrapped", k4.reason
    scene.cls, scene.adapter = cls, cls(scene.world)
    a = scene.guild(GUILD_A)
    msg = scene.world.post(a.general, "nice weather", MEMBER, "Mallory")
    scene.route(msg, lane="public", mode="ambient")
    await _process(scene.adapter, _event(msg))
    assert msg.reactions == []


async def test_behaviour_probe_refuses_hooks_that_bypass_the_guard(anchor):
    scene = Scene(anchor, install=False)
    cls = make_adapter_class()

    async def on_processing_start(self, event):          # reacts without asking whether it can
        await event.raw_message.add_reaction("eyes")

    cls.on_processing_start = on_processing_start
    original = cls.__dict__["on_processing_start"]
    _, k4 = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert k4.outcome.value == "fallback" and "reactions stay visible" in k4.reason
    assert cls.__dict__["on_processing_start"] is original


async def test_behaviour_probe_refuses_hooks_that_react_without_asking_the_message(anchor):
    scene = Scene(anchor, install=False)
    cls = make_adapter_class()

    async def on_processing_start(self, event):          # no hasattr guard: hiding cannot stop it
        acked = await self._add_reaction(event.raw_message, "eyes")
        self._record_discord_processing_start(event, emoji_ack=acked)

    cls.on_processing_start = on_processing_start
    _, k4 = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert k4.outcome.value == "fallback" and "hidden reaction methods still produced reactions" in k4.reason


async def test_behaviour_probe_refuses_hooks_that_drop_the_durable_record(anchor):
    scene = Scene(anchor, install=False)
    cls = make_adapter_class()

    async def on_processing_start(self, event):
        message = event.raw_message
        if hasattr(message, "add_reaction"):
            await self._add_reaction(message, "eyes")
            self._record_discord_processing_start(event, emoji_ack=True)   # recorded only with reactions

    cls.on_processing_start = on_processing_start
    _, k4 = scene.split.install(cls, scene.configs, anchor.RUNTIME.status)
    assert k4.outcome.value == "fallback" and "durable state differs" in k4.reason


async def test_k4_install_is_idempotent(anchor):
    scene = Scene(anchor)
    start = scene.cls.on_processing_start
    _, k4 = scene.split.install(scene.cls, scene.configs, anchor.RUNTIME.status)
    assert k4.outcome.value == "wrapped" and k4.reason.startswith("already wrapped")
    assert scene.cls.on_processing_start is start
