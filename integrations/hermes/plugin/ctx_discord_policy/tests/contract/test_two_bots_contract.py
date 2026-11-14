"""Two bots of one gateway admit one message; a turn restored from Hermes' durable FIFO keeps its own bot's route.

Real: Hermes' Discord admission and handler for both bots (wrapped by A5/A6), Hermes' envelope restore
(``gateway.session_envelope.restore_native``: the restored event has no ``raw_message``), the A7 wrapper and
Hermes' ``pre_tool_call`` dispatch. The author is an operator of bot B only.
"""

from __future__ import annotations

import pytest
from contract.policy_world import BOT, GUILD, World, _config

BOT_B = "900000000000000301"
X = "900000000000000888"


@pytest.fixture
def world(hermes, tmp_path, monkeypatch):
    w = World(hermes, tmp_path, monkeypatch)
    home_b = w.homes.profile("botb")
    (home_b / "config.yaml").write_text(_config([X]), encoding="utf-8")
    w.add_bot(BOT_B, home_b)
    w.bind(GUILD, BOT_B)
    try:
        yield w
    finally:
        w.close()


def _restored(event):
    """The event as Hermes executes it from its FIFO: serialized, then rebuilt by ``restore_native``."""
    from gateway.session_envelope import _EVENT_FIELDS, restore_native

    source = event.source.to_dict()
    source["is_bot"] = bool(getattr(event.source, "is_bot", False))
    envelope = {"source": source, "route": "route", "event": {n: getattr(event, n) for n in _EVENT_FIELDS},
                "timestamp": event.timestamp.isoformat()}
    restored = restore_native({"text": event.text, "native_text_v1": envelope})
    assert restored.raw_message is None and str(restored.source.user_id) == X
    return restored


async def _admit_both(w):
    msg = w.message(X, 4242, "hello")
    a, b = w.adapters[BOT], w.adapters[BOT_B]
    msg.mentions = [a._client.user, b._client.user]
    msg.content = f"<@{BOT}> <@{BOT_B}> hello"
    first = await w.deliver(msg, BOT)
    second = await w.deliver(msg, BOT_B)
    assert first["event"] is not None and second["event"] is not None
    lanes = (first["event"].raw_message._dsp_route.lane, second["event"].raw_message._dsp_route.lane)
    assert lanes == ("public", "operator")
    return first["event"]


async def test_restored_turn_runs_under_its_own_bots_route(world):
    event_a = await _admit_both(world)
    world.runner._delivery_adapter_for = lambda source: world.adapters[BOT]      # bot A answers this source
    out = await world.turn(_restored(event_a))
    assert out["ran"] and out["lane"] == "public" and out["route"].bot_id == BOT, out
    assert out["blocked"], out                       # terminal refused: never bot B's operator lane


async def test_restored_turn_whose_bot_cannot_be_told_does_not_run(world):
    event_a = await _admit_both(world)
    world.runner._delivery_adapter_for = lambda source: None                       # Hermes cannot name the bot
    out = await world.turn(_restored(event_a))
    assert out["result"] is None and not out["ran"], out                           # ambiguous: dropped
    assert world.authorized(_restored(event_a).source) is not True                 # and never authorized by route


async def test_each_bot_keeps_its_route_in_the_registry(world):
    await _admit_both(world)
    bots = sorted(key[0] for key in world.runtime.message_routes._data if key[-2:] == ("4242", X))
    assert bots == [BOT, BOT_B]
