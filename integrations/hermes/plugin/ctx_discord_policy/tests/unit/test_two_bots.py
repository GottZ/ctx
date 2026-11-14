"""Two bots of one gateway process admit the same Discord message (route keys carry the bot).

Bot A (owner home A, no operators) and bot B (owner home B, operator X) both admit message M by X. A turn of
bot A that Hermes runs again from its durable FIFO (restored event: no ``raw_message``) finds its route in the
registry. It must never run under bot B's operator route; when the bot cannot be told, it does not run.
"""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace

import pytest
from fakes.ctx import FakeCtx
from fakes.gateway import CHANNEL, GUILD, HookBus, install_gateway_module, make_classes, message, \
    remove_gateway_module, user

BOT_A = "900000000000000300"     # the fakes' default bot
BOT_B = "900000000000000301"
X = "900000000000000777"
TERMINAL = ("terminal", {"command": "id"})


@pytest.fixture
def two_bots(anchor, monkeypatch, tmp_path):
    home_a, home_b = tmp_path / "home-a", tmp_path / "home-b"
    home_a.mkdir()
    home_b.mkdir()
    (home_a / "settings.json").write_text(json.dumps({"operators": []}))
    (home_b / "settings.json").write_text(json.dumps({"operators": [X]}))
    monkeypatch.setenv("HERMES_HOME", str(home_a))
    ctx = FakeCtx("default")
    anchor.entry.register(ctx)
    config = anchor.mod("config")

    def reader(home, key, default):
        path = Path(home) / "settings.json"
        return json.loads(path.read_text()).get(key, default) if path.exists() else default

    config.set_reader(reader)
    bus = HookBus(ctx.hooks)
    classes = make_classes(home_resolver=lambda source: home_a, bus=bus)     # both bots' turns run in home A
    install_gateway_module(classes.module)
    interventions = anchor.mod("adapt.interventions")
    ops = anchor.mod("store.ops")
    runner_a, runner_b = classes.GatewayRunner(), classes.GatewayRunner()
    adapter_a = classes.DiscordAdapter(runner_a)
    adapter_b = classes.DiscordAdapter(runner_b)
    adapter_b._client = SimpleNamespace(user=user(BOT_B, bot=True, name="botb"), application_id=int(BOT_B))
    adapter_b._self_is_explicitly_mentioned = lambda msg: any(
        str(getattr(m, "id", "")) in (BOT_A, BOT_B) for m in getattr(msg, "mentions", []))
    interventions.discord_platform_handler(SimpleNamespace(application_id=int(BOT_A)), adapter_a)
    monkeypatch.setenv("HERMES_HOME", str(home_b))
    interventions.discord_platform_handler(SimpleNamespace(application_id=int(BOT_B)), adapter_b)
    monkeypatch.setenv("HERMES_HOME", str(home_a))
    for bot in (BOT_A, BOT_B):
        ops.bind(anchor.RUNTIME.bots.for_bot(bot).store, bot, GUILD, by="cli")
    msg = message(user(X), mid=4242, mentions=[user(BOT_A, bot=True), user(BOT_B, bot=True)])
    try:
        yield SimpleNamespace(anchor=anchor, bus=bus, interventions=interventions, adapter_a=adapter_a,
                              adapter_b=adapter_b, runner_a=runner_a, runner_b=runner_b, msg=msg,
                              admission=anchor.mod("ingress.admission"))
    finally:
        remove_gateway_module(classes.module)
        config.set_reader(None)


async def _admit_both(w):
    lanes = {"A": w.admission.decide(w.adapter_a, w.msg).lane, "B": w.admission.decide(w.adapter_b, w.msg).lane}
    w.runner_a.plan = [TERMINAL]
    w.runner_b.plan = [TERMINAL]
    for adapter in (w.adapter_a, w.adapter_b):          # A first, then B (gateway event order)
        admitted, role = adapter._discord_message_admission(w.msg, claim=True)
        assert admitted
        await adapter._handle_message(w.msg, role)
    return lanes


async def _restored_turn_of_a(w):
    """Bot A's turn of M as Hermes restores it from the FIFO, run through A7."""
    source = w.adapter_a.build_source(chat_id=CHANNEL, chat_type="group", user_id=X, guild_id=GUILD,
                                      message_id="4242")
    restored = SimpleNamespace(source=source, raw_message=None, message_id="4242", text="hello",
                               get_command=lambda: None, message_type="text")
    seen = {}

    async def original(self, event):
        route = w.anchor.RUNTIME.route.get()
        seen.update(route_bot=getattr(route, "bot_id", None), route_lane=getattr(route, "lane", None),
                    terminal=w.bus.tool(*TERMINAL, session_id="s-restored", turn_id="t-restored"))
        return "ran"

    result = await w.interventions._make_runner_handle(original)(w.runner_a, restored)
    return result, seen


async def test_both_bots_keep_their_own_route_for_one_message(two_bots):
    w = two_bots
    assert await _admit_both(w) == {"A": "public", "B": "operator"}
    keys = sorted(key for key in w.anchor.RUNTIME.message_routes._data)
    assert [k[0] for k in keys] == [BOT_A, BOT_B]                # one route per bot, the first never overwritten
    assert w.adapter_a.last_result["tools"][0] is not None       # live turn of A: public lane, terminal refused
    assert w.adapter_b.last_result["tools"][0] is None           # live turn of B: operator lane


async def test_restored_turn_of_a_runs_under_its_own_route_when_the_bot_is_known(two_bots):
    w = two_bots
    await _admit_both(w)
    w.runner_a._delivery_adapter_for = lambda source: w.adapter_a      # Hermes: the adapter that answers
    result, seen = await _restored_turn_of_a(w)
    assert result == "ran"
    assert seen["route_bot"] == BOT_A and seen["route_lane"] == "public", seen
    assert seen["terminal"] is not None, seen                     # refused: never B's operator lane


async def test_restored_turn_whose_bot_cannot_be_told_does_not_run(two_bots):
    w = two_bots
    await _admit_both(w)                                          # the fake runner names no delivering adapter
    result, seen = await _restored_turn_of_a(w)
    assert result is None and seen == {}, seen                    # ambiguous: dropped, not run unrouted
