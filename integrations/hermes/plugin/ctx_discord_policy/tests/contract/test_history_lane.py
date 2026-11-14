"""A member's turn reads the channel's history only where the author and everyone in the channel may (H1-H).

Real: Hermes' Discord admission and handler (A5/A6) including its own history backfill
(``_discord_history_backfill`` / ``_fetch_channel_context``), the route the plugin attaches, the A7 wrapper and
Hermes' ``pre_tool_call`` dispatch of the ``discord`` tool's ``fetch_messages`` call. The channel's permission
check stands for discord.py's ``permissions_for``.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from contract.policy_world import CHANNEL, GUILD, MEMBER, OPERATOR, World


@pytest.fixture
def world(hermes, tmp_path, monkeypatch):
    w = World(hermes, tmp_path, monkeypatch)
    w.backfills = []

    async def fetch_context(channel, before=None, reply_target=None):
        w.backfills.append(str(getattr(channel, "id", "")))
        return ""

    w.adapter._fetch_channel_context = fetch_context
    w.adapter._extra_or_env_flag = lambda name, *a, **k: name != "auto_thread"   # Hermes' default: backfill on
    try:
        yield w
    finally:
        w.close()


def _message(w, author, mid, *, history: bool):
    msg = w.message(author, mid, "what did we say earlier?")
    everyone = SimpleNamespace(id=int(GUILD), name="@everyone")
    msg.guild.roles = [everyone]
    msg.guild.default_role = everyone
    msg.channel.overwrites = {}
    msg.channel.permissions_for = lambda who: SimpleNamespace(view_channel=True, read_message_history=history)
    return msg


async def _fetch(w, event, args):
    from hermes_cli import plugins
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    hooks = w.mod("policy.hooks")
    route = event.raw_message._dsp_route
    token = w.runtime.route.set(route)
    home = set_hermes_home_override(str(w.homes.root))
    try:
        hooks.on_pre_llm_call(session_id="s-history", turn_id="t1")
        return plugins._dispatch_pre_tool_call_hooks("discord", args, session_id="s-history", turn_id="t1")[0]
    finally:
        reset_hermes_home_override(home)
        w.runtime.route.reset(token)


async def test_recent_history_where_everyone_may_read_it(world):
    out = await world.deliver(_message(world, MEMBER, 7701, history=True))
    route = out["event"].raw_message._dsp_route
    assert route.lanes.channel_history
    assert await _fetch(world, out["event"], {"action": "fetch_messages", "channel_id": CHANNEL}) is None
    assert world.backfills == [CHANNEL]                    # Hermes' own backfill follows the same rule


async def test_no_paging_back_through_the_channel(world):
    out = await world.deliver(_message(world, MEMBER, 7702, history=True))
    for extra in ({"before": "1000000000000000000"}, {"after": "1"}):
        blocked = await _fetch(world, out["event"], {"action": "fetch_messages", "channel_id": CHANNEL,
                                                    "limit": 100, **extra})
        assert blocked, extra


async def test_no_history_where_the_channel_hides_it(world):
    out = await world.deliver(_message(world, MEMBER, 7703, history=False))
    route = out["event"].raw_message._dsp_route
    blocked = await _fetch(world, out["event"], {"action": "fetch_messages", "channel_id": CHANNEL})
    print("probe hidden", route.lanes.channel_history, blocked, world.backfills)
    assert not route.lanes.channel_history and blocked, (route.lanes, blocked)
    assert world.backfills == [], world.backfills          # and no backfill of that history into the turn


async def test_an_operators_turn_keeps_hermes_backfill(world):
    out = await world.deliver(_message(world, OPERATOR, 7704, history=False))
    assert out["event"] is not None and world.backfills == [CHANNEL]
