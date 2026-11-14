"""The members' channel-history lane reads recent messages only, and only where everyone may (H1-H)."""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from unit.test_ingress_hydrate import G1, MEMBER, World, config, route_for


@pytest.fixture
def lanes(anchor):
    return anchor.mod("policy.lanes")


def _route(lanes, **history):
    return SimpleNamespace(channel_id="940000000000000110", is_operator=False,
                           lanes=lanes.PublicLanes.from_guild({"lanes": {"channel_history": history}}))


@pytest.mark.parametrize("extra", [{"before": "1000000000000000000"}, {"after": "1"}, {"around": "5"},
                                   {"guild_id": "1"}, {"oldest_first": True}])
def test_paging_and_any_other_argument_is_refused(lanes, extra):
    route = _route(lanes, enabled=True, max_limit=100)
    call = {"action": "fetch_messages", "channel_id": route.channel_id, "limit": 20, **extra}
    assert not lanes.public_tool_allowed(route, "discord", call)


def test_recent_messages_of_this_channel_stay_allowed(lanes):
    route = _route(lanes, enabled=True, max_limit=100)
    assert lanes.public_tool_allowed(route, "discord", {"action": "fetch_messages", "channel_id": route.channel_id})
    assert lanes.public_tool_allowed(route, "discord", {"action": "fetch_messages", "channel_id": route.channel_id,
                                                        "limit": 100})
    assert not lanes.public_tool_allowed(route, "discord", {"action": "fetch_messages",
                                                            "channel_id": route.channel_id, "limit": 101})


def _adapter(w):
    return SimpleNamespace(_client=w.client())


def test_history_opens_where_author_and_audience_read_it(anchor):
    hydrate = anchor.mod("ingress.hydrate")
    w = World()
    msg = w.general.post(w.member, "what did we decide?")
    route = route_for(anchor, msg, lane="public")
    assert hydrate.history_readable(_adapter(w), msg, route, config(anchor)) is True


def test_history_stays_closed_where_the_author_may_not_read_it(anchor):
    hydrate = anchor.mod("ingress.hydrate")
    w = World()
    w.general.no_history = {MEMBER}
    msg = w.general.post(w.member, "what did we decide?")
    assert hydrate.history_readable(_adapter(w), msg, route_for(anchor, msg, lane="public"), config(anchor)) is False


def test_history_stays_closed_where_part_of_the_audience_may_not_read_it(anchor):
    hydrate = anchor.mod("ingress.hydrate")
    w = World()
    w.general.no_history = {G1}             # @everyone views the channel but may not read its history
    msg = w.general.post(w.mod, "what did we decide?")
    assert hydrate.history_readable(_adapter(w), msg, route_for(anchor, msg, lane="public"), config(anchor)) is False


def test_history_stays_closed_without_a_permission_check(anchor):
    hydrate = anchor.mod("ingress.hydrate")
    w = World()
    msg = w.general.post(w.member, "what did we decide?")
    w.general.permissions_for = None
    assert hydrate.history_readable(_adapter(w), msg, route_for(anchor, msg, lane="public"), config(anchor)) is False
