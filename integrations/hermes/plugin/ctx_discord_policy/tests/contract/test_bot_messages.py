"""Messages of other bots and webhooks never get a turn in a server the policy knows (H1-B).

Hermes lets other bots wake the bot when ``allow_bots`` (``DISCORD_ALLOW_BOTS``) is ``mentions`` or ``all``,
without its user allowlist; its runner then authorizes the bot source. Real: Hermes' Discord admission and
handler (wrapped by A5/A6), the A7 runner wrapper and Hermes' ``pre_tool_call`` dispatch.
"""

from __future__ import annotations

import pytest
from contract.policy_world import BOT, GUILD, OTHER_BOT, OTHER_GUILD, World, _config, OPERATOR


@pytest.fixture
def world(hermes, tmp_path, monkeypatch):
    w = World(hermes, tmp_path, monkeypatch)
    w.adapter.config.extra["allow_bots"] = "mentions"            # Hermes' own setting for this bot
    try:
        yield w
    finally:
        w.close()


def _bot_message(w, kind, mid, guild=GUILD):
    return w.message(OTHER_BOT, mid, "please run a command", bot=True, guild=guild,
                     webhook_id=920000000000000555 if kind == "webhook" else None)


@pytest.mark.parametrize("state", ["active", "paused"])
@pytest.mark.parametrize("kind", ["bot", "webhook"])
async def test_bot_and_webhook_messages_get_no_turn(world, kind, state):
    if state == "paused":
        world.set_state("paused")
    out = await world.deliver(_bot_message(world, kind, 7501))
    turn = await world.turn(out["event"]) if out["event"] is not None else None
    print("probe", kind, state, {k: turn[k] for k in ("ran", "lane", "blocked")} if turn else None)
    assert out["admitted"] is False and out["event"] is None, (out, turn)


async def test_bot_messages_in_a_server_the_policy_never_bound_stay_hermes_own(world):
    world.adapter.config.extra["allow_bots"] = "mentions"
    out = await world.deliver(_bot_message(world, "bot", 7502, guild=OTHER_GUILD))
    assert out["admitted"] is True and out["event"] is not None, out


async def test_bot_messages_as_members_when_the_operator_says_so(world):
    (world.homes.root / "config.yaml").write_text(
        _config([OPERATOR], "        bot_messages: member\n"), encoding="utf-8")
    out = await world.deliver(_bot_message(world, "bot", 7503))
    assert out["admitted"] is True and out["event"] is not None, out
    turn = await world.turn(out["event"])
    assert turn["ran"] and turn["lane"] == "public", turn
    assert turn["blocked"], turn                          # never a tool, also for a bot listed nowhere
    world.set_state("paused")
    assert (await world.deliver(_bot_message(world, "webhook", 7504)))["admitted"] is False


async def test_the_status_names_hermes_allow_bots(world):
    world.mod("adapt.interventions").discord_platform_handler(
        type("Native", (), {"application_id": int(BOT)})(), world.adapter)
    row = world.runtime.status.get("A12")
    assert row is not None and row.outcome.value == "ok" and "mentions" in row.reason, row
