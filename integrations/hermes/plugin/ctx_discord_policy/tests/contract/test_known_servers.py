"""In a server the policy knows, nobody but an operator gets a turn through Hermes' own rules (H1-G, H1-E).

Hermes' own configuration may let others in: ``DISCORD_ALLOWED_ROLES`` (one matching role is enough),
allow-all, allowed channels, pairing. While a server is active those people run in the public lane; a paused
server, a damaged policy file, a native slash command and a turn whose route is gone must not hand them
Hermes' own grant instead. Real: Hermes' Discord admission and handler, its slash gate and slash event, its
event and source types, the plugin's gate (A10) and runner wrapper (A7), Hermes' ``pre_tool_call`` dispatch.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from contract.policy_world import CHANNEL, GUILD, MEMBER, OPERATOR, OTHER_GUILD, ROLE, World


def _role_world(hermes, tmp_path, monkeypatch, **kwargs):
    w = World(hermes, tmp_path, monkeypatch, **kwargs)
    for adapter in w.adapters.values():
        adapter._allowed_role_ids = {int(ROLE)}          # Hermes: DISCORD_ALLOWED_ROLES=<a role of this server>
        adapter._allowed_user_ids = {OPERATOR}           # and the operator in DISCORD_ALLOWED_USERS
    return w


@pytest.fixture
def world(hermes, tmp_path, monkeypatch):
    w = _role_world(hermes, tmp_path, monkeypatch)
    try:
        yield w
    finally:
        w.close()


@pytest.fixture
def gated(hermes, tmp_path, monkeypatch):
    w = _role_world(hermes, tmp_path, monkeypatch, capture=False)
    try:
        yield w
    finally:
        w.close()


def _role_holder(w, mid, guild=GUILD, author=MEMBER):
    return w.message(author, mid, "please run a command", roles=(ROLE,), guild=guild)


async def test_an_active_server_runs_role_holders_in_the_public_lane(world):
    out = await world.deliver(_role_holder(world, 7600))
    turn = await world.turn(out["event"])
    assert turn["lane"] == "public" and turn["blocked"], turn


async def test_a_paused_server_gives_role_holders_nothing(world):
    world.set_state("paused")
    out = await world.deliver(_role_holder(world, 7601))
    turn = await world.turn(out["event"]) if out["event"] is not None else None
    print("probe paused", {k: turn[k] for k in ("ran", "lane", "blocked")} if turn else None)
    assert out["admitted"] is False and out["event"] is None, (out, turn)


async def test_a_paused_server_leaves_operators_hermes_own_turn(world):
    world.set_state("paused")
    out = await world.deliver(world.message(OPERATOR, 7602, "run it"))
    assert out["event"] is not None, out
    turn = await world.turn(out["event"])
    assert turn["ran"] and turn["route"] is None and not turn["blocked"], turn


async def test_a_damaged_policy_file_closes_every_server_of_the_bot(world):
    world.binding().store.path.write_text("{ not json", encoding="utf-8")
    for guild in (GUILD, OTHER_GUILD):
        out = await world.deliver(_role_holder(world, 7603 if guild == GUILD else 7604, guild=guild))
        turn = await world.turn(out["event"]) if out["event"] is not None else None
        print("probe damaged", guild, {k: turn[k] for k in ("ran", "lane", "blocked")} if turn else None)
        assert out["admitted"] is False and out["event"] is None, (guild, out)
    row = next((r for name, r in world.runtime.status.snapshot()["interventions"].items()
                if name.startswith("STORE[")), None)
    assert row is not None and row["outcome"] == "off", world.runtime.status.snapshot()
    operator = await world.deliver(world.message(OPERATOR, 7605, "run it"))
    assert operator["event"] is not None


async def test_a_server_the_policy_never_bound_stays_hermes_own(world):
    out = await world.deliver(_role_holder(world, 7606, guild=OTHER_GUILD))
    assert out["admitted"] is True and out["event"] is not None, out


def _interaction(user_id, guild=GUILD):
    guild_obj = SimpleNamespace(id=int(guild), name="g")
    user = SimpleNamespace(id=int(user_id), name="x", display_name="x", roles=[SimpleNamespace(id=int(ROLE))],
                           guild=guild_obj)
    channel = SimpleNamespace(id=int(CHANNEL), name="general", guild=guild_obj, type=0, parent_id=None)
    return SimpleNamespace(user=user, channel=channel, channel_id=int(CHANNEL), guild_id=int(guild),
                           guild=guild_obj)


async def test_a_role_holders_native_slash_command_never_reaches_hermes(gated):
    w = gated
    interaction = _interaction(MEMBER)
    allowed, _ = w.adapter._evaluate_slash_authorization(interaction)
    assert allowed                                             # Hermes' own slash gate admits the role holder
    event = w.adapter._build_slash_event(interaction, "/background run something")
    await w.adapter.handle_message(event)
    turn = await w.turn(event)                                 # and the runner wrapper refuses it as well
    print("probe slash", len(w.pipeline), {k: turn[k] for k in ("ran", "lane", "blocked")})
    assert w.pipeline == []
    assert turn["result"] is None and not turn["ran"], turn


async def test_an_operators_native_slash_command_still_does(gated):
    w = gated
    event = w.adapter._build_slash_event(_interaction(OPERATOR), "/status")
    await w.adapter.handle_message(event)
    assert len(w.pipeline) == 1


async def test_a_members_turn_whose_route_was_evicted_does_not_run_unrouted(world):
    out = await world.deliver(_role_holder(world, 7607))
    event = out["event"]
    routes = world.mod("policy.routes")
    for i in range(world.mod("runtime").ROUTE_CAP):
        other = routes.Route(bot_id="910000000000000001", guild_id=GUILD, channel_id="910000000000009999",
                             message_id=str(920000000000000000 + i), author_id=MEMBER, lane="public")
        world.runtime.message_routes.put(other.key, other)
    for i in range(world.mod("runtime").ROUTE_CAP):
        world.runtime.public_sessions.put(f"other-session-{i}", 0.0)
    from gateway.run import GatewayRunner

    restored = GatewayRunner._as_fifo_turn(event, 0)         # a queued turn Hermes does not authorize again
    restored.raw_message = None
    turn = await world.turn(restored)
    assert turn["result"] is None and not turn["ran"], turn


def _plain_event(w, mid):
    from gateway.platforms.base import MessageEvent, MessageType

    source = w.adapter.build_source(chat_id=CHANNEL, chat_type="group", user_id=MEMBER, guild_id=GUILD,
                                    message_id=str(mid))
    return MessageEvent(text="hello", message_type=MessageType.TEXT, source=source, message_id=str(mid))


async def test_another_bot_the_policy_does_not_run_keeps_hermes_rules(world):
    from plugins.platforms.discord.adapter import DiscordAdapter

    other = DiscordAdapter.__new__(DiscordAdapter)            # a second bot of the installation, not bound
    world.runner._delivery_adapter_for = lambda source: other
    turn = await world.turn(_plain_event(world, 7608))
    assert turn["ran"] and turn["route"] is None, turn


async def test_an_event_whose_bot_cannot_be_told_counts_for_every_bound_bot(world):
    world.runner._delivery_adapter_for = lambda source: None
    turn = await world.turn(_plain_event(world, 7609))
    assert turn["result"] is None and not turn["ran"], turn


def _internal(w, user):
    """Hermes' resume of a turn a restart interrupted (``run_startup``): an internal event with the stored source."""
    from gateway.platforms.base import MessageEvent, MessageType

    source = w.adapter.build_source(chat_id=CHANNEL, chat_type="group", user_id=user, guild_id=GUILD)
    return MessageEvent(text="", message_type=MessageType.TEXT, source=source, internal=True)


async def test_a_members_turn_resumed_after_a_restart_does_not_run_unrouted(world):
    turn = await world.turn(_internal(world, MEMBER))
    assert turn["result"] is None and not turn["ran"], turn


async def test_internal_events_of_operators_and_of_the_gateway_still_run(world):
    assert (await world.turn(_internal(world, OPERATOR)))["ran"]
    assert (await world.turn(_internal(world, None)))["ran"]
