"""S5 K5: a person's reaction with the server's wake emoji wakes the bot for exactly that message.

The woken message is written by the reactor (never by the author of the message reacted to) and goes
through the same admission gate as every other message; this module grants nothing itself.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from unit.test_security_core import world  # noqa: F401 - the S3 world fixture (security core end to end)

BOT = "930000000000000001"
GUILD_A = "930000000000000100"
GUILD_B = "930000000000000200"
CHAN_A = "930000000000000110"
CHAN_B = "930000000000000210"
THREAD_A = "930000000000000111"
EMOJI_A = "930000000000000900"
OTHER_EMOJI = "930000000000000901"
MEMBER = "930000000000000008"
SECOND = "930000000000000009"
AUTHOR = "930000000000000010"
OTHER_BOT = "930000000000000011"
ORIGINAL = 930000000000005000


def person(uid, *, bot=False, name="member"):
    return SimpleNamespace(id=int(uid), bot=bot, display_name=name, name=name)


class Channel:
    def __init__(self, cid, guild, *, kind=0, parent_id=None, silent=()):
        self.id, self.guild, self.type, self.name = int(cid), guild, kind, f"chan-{str(cid)[-3:]}"
        self.parent_id = int(parent_id) if parent_id else None
        self.silent = {str(s) for s in silent}       # members who may read but not write here
        self.messages = {}

    def permissions_for(self, member):
        may_write = str(member.id) not in self.silent
        return SimpleNamespace(view_channel=True, read_message_history=True, send_messages=may_write,
                               send_messages_in_threads=may_write)

    async def fetch_message(self, mid):
        return self.messages[int(mid)]


class Guild:
    def __init__(self, gid, members):
        self.id, self.name = int(gid), f"server-{str(gid)[-3:]}"
        self.members = {int(m.id): m for m in members}
        self.me = SimpleNamespace(id=int(BOT), display_name="Nova")
        self.fetched = []

    def get_member(self, uid):
        return None

    async def fetch_member(self, uid):
        self.fetched.append(uid)
        return self.members.get(int(uid))


class Client:
    def __init__(self, guilds, channels):
        self.user = person(BOT, bot=True, name="Nova")
        self.application_id = int(BOT)
        self._guilds = {g.id: g for g in guilds}
        self._channels = {c.id: c for c in channels}

    def get_guild(self, gid):
        return self._guilds.get(int(gid))

    def get_channel(self, cid):
        return self._channels.get(int(cid))

    async def fetch_channel(self, cid):
        return self._channels[int(cid)]


class Native:
    """discord.ext.commands.Bot's listener registry (add_listener / dispatch)."""

    def __init__(self):
        self.listeners = {}

    def add_listener(self, func, name):
        self.listeners.setdefault(name, []).append(func)

    async def fire(self, name, payload):
        for func in self.listeners.get(name, []):
            await func(payload)


class Adapter:
    def __init__(self, client):
        self._client = client


def payload(*, guild=GUILD_A, channel=CHAN_A, message=ORIGINAL, user=MEMBER, emoji=None, member=None):
    return SimpleNamespace(guild_id=int(guild) if guild else None, channel_id=int(channel), message_id=int(message),
                           user_id=int(user), emoji=emoji if emoji is not None else custom(EMOJI_A), member=member)


def custom(eid, name="nova"):
    return SimpleNamespace(id=int(eid), name=name)


def unicode_emoji(text):
    return SimpleNamespace(id=None, name=text)


class Scene:
    def __init__(self, anchor, *, emoji_a=None, emoji_b="🦊", state="active"):
        self.anchor = anchor
        self.reactions = anchor.mod("ingress.reactions")
        self.reactions.SEEN.clear()
        schema = anchor.mod("store.schema")
        self.members = [person(MEMBER), person(SECOND, name="second"), person(AUTHOR, name="author"),
                        person(OTHER_BOT, bot=True, name="otherbot")]
        self.guild_a = Guild(GUILD_A, self.members)
        self.guild_b = Guild(GUILD_B, self.members)
        self.chan_a = Channel(CHAN_A, self.guild_a)
        self.thread_a = Channel(THREAD_A, self.guild_a, kind=11, parent_id=CHAN_A, silent={SECOND})
        self.chan_b = Channel(CHAN_B, self.guild_b)
        self.original = SimpleNamespace(id=ORIGINAL, content="the message people react to", author=person(AUTHOR),
                                        channel=self.chan_a, attachments=[SimpleNamespace(filename="a.png")],
                                        jump_url="https://discord.com/channels/x/y/z", added=[])

        async def add_reaction(emoji):
            self.original.added.append(emoji)

        self.original.add_reaction = add_reaction
        self.chan_a.messages[ORIGINAL] = self.original
        self.thread_a.messages[ORIGINAL] = self.original
        self.chan_b.messages[ORIGINAL] = self.original
        self.client = Client([self.guild_a, self.guild_b], [self.chan_a, self.thread_a, self.chan_b])
        self.adapter = Adapter(self.client)
        anchor.RUNTIME.bots.bind(self.adapter, BOT, anchor.home)
        cfg_a, cfg_b = schema.guild_defaults(), schema.guild_defaults()
        cfg_a.update(state=state, rev=1)
        cfg_b.update(state="active", rev=1)
        cfg_a["identity"]["reaction_emoji"] = emoji_a if emoji_a is not None else {"id": EMOJI_A, "name": "nova"}
        cfg_b["identity"]["reaction_emoji"] = emoji_b
        self.configs = {(BOT, GUILD_A): cfg_a, (BOT, GUILD_B): cfg_b}
        self.dispatched = []
        self.answer = True

    def get(self, bot_id, guild_id):
        return self.configs.get((str(bot_id), str(guild_id)))

    async def dispatch(self, adapter, message):
        self.dispatched.append(message)
        if isinstance(self.answer, Exception):
            raise self.answer
        return self.answer

    async def react(self, **kw):
        return await self.reactions.on_reaction_add(self.adapter, payload(**kw), self.get, self.dispatch)


@pytest.fixture
def scene(anchor):
    return Scene(anchor)


# ── installation (strategies, idempotence, status) ───────────────────────────────────────────────


def test_listener_through_the_platform_handler_factory(anchor, scene):
    native = Native()
    result = scene.reactions.install_reaction_wakeup(native, scene.adapter, scene.get, scene.dispatch,
                                                     anchor.RUNTIME.status)
    assert result.intervention == "K5" and result.klass == "K" and result.outcome.value == "injected"
    assert set(native.listeners) == {"on_raw_reaction_add", "on_raw_reaction_remove"}
    again = scene.reactions.install_reaction_wakeup(native, scene.adapter, scene.get, scene.dispatch,
                                                    anchor.RUNTIME.status)
    assert again.outcome.value == "injected" and "already" in again.reason
    assert all(len(v) == 1 for v in native.listeners.values())
    assert anchor.RUNTIME.status.get("K5").outcome.value == "injected"


async def test_listeners_fire_and_wake(anchor, scene):
    native = Native()
    scene.reactions.install_reaction_wakeup(native, scene.adapter, scene.get, scene.dispatch)
    await native.fire("on_raw_reaction_add", payload())
    assert len(scene.dispatched) == 1
    await native.fire("on_raw_reaction_add", payload())
    assert len(scene.dispatched) == 1
    await native.fire("on_raw_reaction_remove", payload())
    await native.fire("on_raw_reaction_add", payload())
    assert len(scene.dispatched) == 2


async def test_client_without_add_listener_gets_its_event_handlers_wrapped(anchor, scene):
    calls = []

    class PlainClient:
        def dispatch(self, event, *args):
            pass

        def event(self, coro):
            setattr(self, coro.__name__, coro)
            return coro

        async def on_raw_reaction_add(self, payload_):
            calls.append("existing")

    native = PlainClient()
    result = scene.reactions.install_reaction_wakeup(native, scene.adapter, scene.get, scene.dispatch)
    assert result.outcome.value == "wrapped"
    await native.on_raw_reaction_add(payload())
    assert calls == ["existing"] and len(scene.dispatched) == 1
    assert scene.reactions.install_reaction_wakeup(native, scene.adapter, scene.get, scene.dispatch).outcome.value \
        == "wrapped"
    await native.on_raw_reaction_remove(payload())
    await native.on_raw_reaction_add(payload())
    assert calls == ["existing", "existing"] and len(scene.dispatched) == 2


@pytest.mark.parametrize("native", [None, object()])
def test_without_a_usable_client_the_wakeup_is_off(anchor, scene, native):
    result = scene.reactions.install_reaction_wakeup(native, scene.adapter, scene.get, scene.dispatch,
                                                     anchor.RUNTIME.status)
    assert result.outcome.value == "off" and result.klass == "K"
    assert anchor.RUNTIME.status.get("K5").outcome.value == "off"


# ── which reaction wakes, per server ─────────────────────────────────────────────────────────────


async def test_each_server_has_its_own_wake_emoji(scene):
    assert await scene.react() == "dispatched"                                       # A: custom emoji by id
    assert await scene.react(emoji=custom(EMOJI_A, name="renamed"), user=SECOND) == "dispatched"
    assert (await scene.react(emoji=custom(OTHER_EMOJI), user=AUTHOR)).startswith("ignored")
    assert (await scene.react(emoji=unicode_emoji("🦊"), user=AUTHOR)).startswith("ignored")  # B's emoji in A
    assert await scene.react(guild=GUILD_B, channel=CHAN_B, emoji=unicode_emoji("🦊")) == "dispatched"
    assert (await scene.react(guild=GUILD_B, channel=CHAN_B, emoji=custom(EMOJI_A), user=SECOND)) \
        .startswith("ignored")
    assert len(scene.dispatched) == 3


async def test_unicode_emoji_matches_with_and_without_variation_selector(anchor):
    s = Scene(anchor, emoji_a="❤️")
    assert await s.react(emoji=unicode_emoji("❤")) == "dispatched"
    s2 = Scene(anchor, emoji_a="❤")
    assert await s2.react(emoji=unicode_emoji("❤️"), user=SECOND) == "dispatched"
    assert (await s2.react(emoji=unicode_emoji("💙"), user=AUTHOR)).startswith("ignored")


async def test_skin_tone_variants_are_the_same_wake_emoji(anchor):
    """Discord applies a member's chosen skin tone to reactions: the tone never decides the wake emoji."""
    s = Scene(anchor, emoji_a="👋")
    assert await s.react(emoji=unicode_emoji("👋🏿")) == "dispatched"
    toned = Scene(anchor, emoji_a="👍🏽")                     # taken over from a manager's own reaction
    assert await toned.react(emoji=unicode_emoji("👍"), user=SECOND) == "dispatched"
    assert await toned.react(emoji=unicode_emoji("👍🏻"), user=SECOND) == "ignored: duplicate"
    assert toned.reactions.on_reaction_remove(toned.adapter, payload(user=SECOND, emoji=unicode_emoji("👍🏾")))


async def test_no_configured_emoji_means_off(anchor):
    s = Scene(anchor)
    s.configs[(BOT, GUILD_A)]["identity"]["reaction_emoji"] = None
    assert (await s.react()).startswith("ignored") and s.dispatched == []


@pytest.mark.parametrize("state", ["paused", "unbound"])
async def test_inactive_server_never_wakes(anchor, state):
    s = Scene(anchor, state=state)
    assert (await s.react()).startswith("ignored") and s.dispatched == []


async def test_unbound_bot_and_unconfigured_server_never_wake(anchor, scene):
    assert (await scene.react(guild="930000000000000300", channel=CHAN_A)).startswith("ignored")
    other = Adapter(scene.client)
    out = await scene.reactions.on_reaction_add(other, payload(), scene.get, scene.dispatch)
    assert out.startswith("ignored") and scene.dispatched == []


async def test_direct_messages_and_the_bots_own_reactions_never_wake(scene):
    assert (await scene.react(guild=None)).startswith("ignored")
    assert (await scene.react(user=BOT)).startswith("ignored")
    assert scene.dispatched == []


# ── who wakes: people only, and only where they could have written ───────────────────────────────


async def test_bots_never_wake(scene):
    assert (await scene.react(user=OTHER_BOT)).startswith("ignored")
    assert (await scene.react(user=OTHER_BOT, member=person(OTHER_BOT, bot=True))).startswith("ignored")
    assert scene.dispatched == []


async def test_reactor_who_cannot_write_in_the_channel_does_not_wake(scene):
    assert (await scene.react(channel=THREAD_A, user=SECOND)).startswith("ignored")
    assert await scene.react(channel=THREAD_A, user=MEMBER) == "dispatched"
    assert [m.author.id for m in scene.dispatched] == [int(MEMBER)]


async def test_channel_of_another_server_never_wakes(scene):
    assert (await scene.react(guild=GUILD_A, channel=CHAN_B)).startswith("ignored")
    assert scene.dispatched == []


# ── dedup per (message, user, emoji) ─────────────────────────────────────────────────────────────


async def test_dedup_per_message_user_and_emoji(scene):
    assert await scene.react() == "dispatched"
    assert await scene.react() == "ignored: duplicate"
    assert await scene.react(user=SECOND) == "dispatched"
    scene.reactions.on_reaction_remove(scene.adapter, payload())
    assert await scene.react() == "dispatched"
    assert len(scene.dispatched) == 3


async def test_a_wakeup_that_did_not_reach_dispatch_can_be_retried(scene):
    scene.answer = False
    assert await scene.react() == "not dispatched"
    scene.answer = RuntimeError("boom")
    assert await scene.react() == "failed"
    scene.answer = True
    assert await scene.react() == "dispatched"
    assert len(scene.dispatched) == 3


async def test_dedup_is_bounded(anchor, scene):
    runtime = anchor.mod("runtime")
    assert isinstance(scene.reactions.SEEN, runtime.BoundedMap)


# ── the woken message ────────────────────────────────────────────────────────────────────────────


async def test_the_woken_message_is_the_reactors_reply_to_the_original(anchor, scene):
    await scene.react()
    woken = scene.dispatched[0]
    c = anchor.mod("ingress.classify")
    assert woken.author.id == int(MEMBER) and woken.author is not scene.original.author
    assert woken.id == ORIGINAL and woken.channel is scene.chan_a and woken.guild is scene.guild_a
    assert woken.reference.resolved is scene.original and woken.reference.message_id == ORIGINAL
    assert woken.mentions == [scene.client.user] and f"<@{BOT}>" in woken.content
    assert "[REACTION WAKEUP] Reply to the referenced message" in woken.content
    assert woken.attachments == [] and woken.message_snapshots == [] and woken.webhook_id is None
    assert woken._dsp_reaction_wakeup.reactor_id == MEMBER
    signal = c.classify(SimpleNamespace(_client=scene.client), woken, scene.get(BOT, GUILD_A))
    assert signal.trigger == "reaction" and signal.direct == "direct" and signal.human
    await woken.add_reaction("eyes")            # processing feedback lands on the reacted-to message
    assert scene.original.added == ["eyes"]


async def test_operator_template_override_of_the_wakeup_text(anchor, scene):
    scene.configs[(BOT, GUILD_A)]["prompts"]["template_overrides"] = {
        "reaction_wakeup": "Antworte auf die Nachricht in {server_name}."}
    await scene.react()
    assert "Antworte auf die Nachricht in server-100." in scene.dispatched[0].content


async def test_managers_text_reaches_an_operator_wakeup_only_with_the_guidance_switched_on(anchor, scene):
    """The bot name a server's managers chose is their text: in an operator's turn it counts only where the
    operator switched the server guidance on for operator turns (the prompt engine's rule)."""
    anchor.mod("config").set_reader(lambda home, key, default: [MEMBER] if key == "operators" else default)
    cfg = scene.configs[(BOT, GUILD_A)]
    cfg["identity"]["bot_name"] = "Zeta"
    cfg["prompts"]["template_overrides"] = {"reaction_wakeup": "Answer as {bot_name}, please."}
    await scene.react()                                                     # MEMBER is an operator
    assert "Answer as Nova, please." in scene.dispatched[-1].content       # the Discord display name
    await scene.react(user=SECOND)                                          # a member: the server's text
    assert "Answer as Zeta, please." in scene.dispatched[-1].content
    cfg["prompts"]["guild_in_operator_lane"] = True
    scene.reactions.on_reaction_remove(scene.adapter, payload())
    await scene.react()
    assert "Answer as Zeta, please." in scene.dispatched[-1].content


@pytest.mark.parametrize("template,name", [
    ("{bot_name} please answer the referenced message.", "/yolo"),          # a manager's bot name
    ("{bot_name} answer this.", ""),                                       # the Discord nickname
    ("@image:x\n/yolo now", "Zeta"),                                       # Hermes drops leading media refs
    (f"<@{BOT}>/yolo", "Zeta"),                                            # ... and the bot's own mention
])
async def test_the_wakeup_text_never_reads_as_a_gateway_command(anchor, scene, template, name):
    cfg = scene.configs[(BOT, GUILD_A)]
    cfg["identity"]["bot_name"] = name
    cfg["prompts"]["template_overrides"] = {"reaction_wakeup": template}
    scene.guild_a.me.display_name = "/yolo"
    assert await scene.react() == "dispatched"
    seen = scene.dispatched[-1].content.replace(f"<@{BOT}>", "").strip()
    assert seen.startswith("[REACTION WAKEUP] Reply to the referenced message"), seen   # the shipped template


async def test_member_found_through_the_server_when_the_event_has_none(scene):
    assert await scene.react(member=None) == "dispatched"
    assert scene.guild_a.fetched == [int(MEMBER)]


# ── end to end: the wakeup runs through the security core's gate ─────────────────────────────────


def _wire_world(world, anchor, *, emoji=None):
    from fakes.gateway import APP_ID, CHANNEL, GUILD, OPERATOR, user

    reactions = anchor.mod("ingress.reactions")
    reactions.SEEN.clear()
    world.configure(identity__reaction_emoji=emoji or {"id": EMOJI_A, "name": "nova"})
    guild = SimpleNamespace(id=int(GUILD), name="server", me=SimpleNamespace(display_name="Nova"))
    members = {int(OPERATOR): user(OPERATOR, name="op"), int(MEMBER): user(MEMBER, name="member")}

    async def fetch_member(uid):
        return members.get(int(uid))

    guild.get_member = lambda uid: None
    guild.fetch_member = fetch_member
    channel = Channel(CHANNEL, guild)
    original = SimpleNamespace(id=ORIGINAL, content="deploy the thing", author=user(OPERATOR, name="op"),
                               channel=channel, attachments=[], jump_url="")
    channel.messages[ORIGINAL] = original
    client = world.adapter._client
    client.get_guild = lambda gid: guild if int(gid) == int(GUILD) else None
    client.get_channel = lambda cid: channel if int(cid) == int(CHANNEL) else None
    native = Native()
    result = reactions.install_reaction_wakeup(native, world.adapter)
    assert result.outcome.value == "injected", result.reason
    assert APP_ID
    return SimpleNamespace(native=native, guild=guild, channel=channel, members=members, GUILD=GUILD,
                           CHANNEL=CHANNEL, OPERATOR=OPERATOR)


async def test_member_waking_on_an_operator_message_gets_a_public_turn_of_their_own(anchor, world):
    w = _wire_world(world, anchor)
    await w.native.fire("on_raw_reaction_add", payload(guild=w.GUILD, channel=w.CHANNEL, user=MEMBER))
    turn = world.runner.turns[-1]
    route = turn["event"].raw_message._dsp_route
    assert route.lane == "public" and route.author_id == MEMBER and route.mode == "direct"
    assert turn["event"].source.user_id == MEMBER
    assert turn["tools"][0] and "BLOCKED" in turn["tools"][0]          # no operator tools for the member
    assert "[Capability lane: public]" in turn["context"][0]["context"]


async def test_operator_waking_gets_an_operator_turn(anchor, world):
    w = _wire_world(world, anchor)
    await w.native.fire("on_raw_reaction_add", payload(guild=w.GUILD, channel=w.CHANNEL, user=w.OPERATOR))
    turn = world.runner.turns[-1]
    assert turn["event"].raw_message._dsp_route.lane == "operator"
    assert turn["tools"][0] is None


async def test_the_gate_decides_excluded_channels_and_paused_servers(anchor, world):
    w = _wire_world(world, anchor)
    world.configure(conversation__excluded_channels=[w.CHANNEL])
    await w.native.fire("on_raw_reaction_add", payload(guild=w.GUILD, channel=w.CHANNEL, user=MEMBER))
    assert world.runner.turns == []
    world.configure(conversation__excluded_channels=[])
    anchor.mod("store.ops").set_state(world.store, "900000000000000300", w.GUILD, "paused", by="cli")
    await w.native.fire("on_raw_reaction_add", payload(guild=w.GUILD, channel=w.CHANNEL, user=SECOND))
    assert world.runner.turns == []


async def test_the_gate_keeps_the_worklog_area_closed_to_members(anchor, world):
    w = _wire_world(world, anchor)
    world.configure(worklog__channel=w.CHANNEL)
    await w.native.fire("on_raw_reaction_add", payload(guild=w.GUILD, channel=w.CHANNEL, user=MEMBER))
    assert world.runner.turns == []
