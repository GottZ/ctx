"""S-INT through the interface: a server purged and activated again in /policy, and a panel from before."""

from __future__ import annotations

import pytest

from tests.replay.test_ui_replay import (  # noqa: F401  (ui and the helpers are shared with the S8 replay tests)
    OP_ID, PLAIN, activate_directly, click, command, custom_id, harness, panel_message, ui,
)

pytestmark = [pytest.mark.asyncio, pytest.mark.replay]


async def test_purged_server_activates_again_and_a_panel_from_before_stays_stale(ui):
    activate_directly(ui)
    h = await harness(ui)
    try:
        stale = (await command(h, user=OP_ID, perms=PLAIN))[0]           # shown before the purge
        confirm = await click(h, panel_message(stale), custom_id(ui, stale, "unbind"), user=OP_ID, perms=PLAIN)
        await click(h, panel_message(confirm[0]), custom_id(ui, confirm[0], "purge_ok"), user=OP_ID, perms=PLAIN)
        assert ui.guild() is None
        fresh = (await command(h, user=OP_ID, perms=PLAIN))[0]           # a panel on the deleted server
        confirm = await click(h, panel_message(fresh), custom_id(ui, fresh, "activate"), user=OP_ID, perms=PLAIN)
        await click(h, panel_message(confirm[0]), custom_id(ui, confirm[0], "activate_ok"), user=OP_ID, perms=PLAIN)
        assert ui.state() == "active"
        before = ui.guild()["rev"]
        await click(h, panel_message(stale), custom_id(ui, stale, "unbind"), user=OP_ID, perms=PLAIN)
        assert ui.state() == "active" and ui.guild()["rev"] == before     # the stale panel changes nothing
    finally:
        await h.close()
