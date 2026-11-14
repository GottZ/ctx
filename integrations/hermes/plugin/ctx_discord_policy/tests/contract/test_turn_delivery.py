"""What a turn leaves behind in Discord: prompt cards follow the worklog, members' answers ping nobody, a dropped
member turn leaves no "done" reaction (H1-F).

Real: Hermes' Discord adapter (``send_clarify`` through ``_send_prompt`` and ``_prompt_target_id``,
``on_processing_start``/``on_processing_complete`` with its reaction helpers), the plugin's delivery wrappers
(K3, K4) and the A7 runner wrapper.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from contract.policy_world import CHANNEL, MEMBER, OPERATOR, World

THREAD = "900000000000000250"
WORKLOG = "900000000000000240"
OTHER = "900000000000000501"


@pytest.fixture
def world(hermes, tmp_path, monkeypatch):
    w = World(hermes, tmp_path, monkeypatch)
    try:
        yield w
    finally:
        w.close()


def _resolving_client(w):
    resolved = []

    class Channel:
        def __init__(self, cid):
            self.id = int(cid)
            self.type = 0
            self.guild = SimpleNamespace(id=900000000000000100)

        async def send(self, **kwargs):
            return SimpleNamespace(id=4500)

    def get_channel(cid):
        resolved.append(str(cid))
        return Channel(cid)

    w.adapter._client = SimpleNamespace(user=w.adapter._client.user, application_id=w.adapter._client.application_id,
                                        get_channel=get_channel)
    w.adapter._component_live_auth = None
    return resolved


async def test_an_operators_clarify_card_goes_to_the_turns_worklog_thread(world):
    out = await world.deliver(world.message(OPERATOR, 7801, "fix the deploy"))
    route = out["event"].raw_message._dsp_route
    route.tool_started = True
    worklog = world.mod("delivery.worklog")
    state = worklog.WorklogState(route=route, settings=worklog.WorklogSettings(channel_id=WORKLOG),
                                 key=worklog.state_key(world.adapter, *route.key[-3:]), thread_id=THREAD)
    worklog.DELIVERY.states.put(state.key, state)
    resolved = _resolving_client(world)
    token = world.runtime.route.set(route)               # the card is sent from inside the turn
    try:
        await world.adapter.send_clarify(CHANNEL, "Which environment?", ["staging", "prod"], "c1", "s1")
    finally:
        world.runtime.route.reset(token)
    assert resolved == [THREAD], resolved


async def test_a_card_without_a_worklog_stays_in_the_channel(world):
    out = await world.deliver(world.message(OPERATOR, 7802, "fix the deploy"))
    resolved = _resolving_client(world)
    token = world.runtime.route.set(out["event"].raw_message._dsp_route)
    try:
        await world.adapter.send_clarify(CHANNEL, "Which environment?", ["staging", "prod"], "c2", "s1")
    finally:
        world.runtime.route.reset(token)
    assert resolved == [CHANNEL], resolved


async def test_a_members_answer_pings_nobody_but_the_asker(world):
    out = await world.deliver(world.message(MEMBER, 7803, "who should I ask?"))
    text = f"Ask <@{OTHER}> or <@&900000000000000777>, @everyone. You asked, <@{MEMBER}>."
    turn = await world.turn(out["event"], reply=text)
    assert f"<@{OTHER}>" not in turn["result"] and "<@&900000000000000777>" not in turn["result"], turn["result"]
    assert "@everyone" not in turn["result"] and f"<@{MEMBER}>" in turn["result"], turn["result"]


async def test_a_dropped_members_turn_leaves_no_done_reaction(world):
    from gateway.platforms.base import ProcessingOutcome

    msg = world.message(MEMBER, 7804, "hello")
    reactions = []

    async def add_reaction(emoji):
        reactions.append(("add", emoji))

    async def remove_reaction(emoji, member=None):
        reactions.append(("remove", emoji))

    msg.add_reaction, msg.remove_reaction = add_reaction, remove_reaction
    out = await world.deliver(msg)
    event = out["event"]
    adapter = world.adapter
    adapter._reactions_enabled = lambda: True
    adapter._record_discord_processing_start = lambda *a, **k: None
    adapter._record_discord_processing_complete = lambda *a, **k: None
    await adapter.on_processing_start(event)
    world.set_state("paused")                              # the route is no longer valid: A7 drops the turn
    turn = await world.turn(event)
    assert turn["result"] is None and not turn["ran"]
    await adapter.on_processing_complete(event, ProcessingOutcome.SUCCESS)   # Hermes: None counts as success
    assert ("add", "✅") not in reactions, reactions
    assert reactions[0] == ("add", "👀") and ("remove", "👀") in reactions, reactions
