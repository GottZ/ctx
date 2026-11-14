"""A server manager who pauses or resumes the policy tells the operators by direct message (H1-G).

Pausing closes the server for members; the operators hear about it. Messages are throttled like the other
operator messages: one per server and window, a change inside the window is sent when it ends.
"""

from __future__ import annotations

from replay.test_ui_panels import (ADMIN_USER_ID, MANAGE, OP_ID, PLAIN, activate_directly, click, command,
                                   custom_id, notify_wait, panel_harness, panel_message, ui)  # noqa: F401


async def _toggle(ui, h, action, user, perms):
    home = (await command(h, user=user, perms=perms))[0]
    await click(h, panel_message(home), custom_id(ui, home, action), user=user, perms=perms)


async def test_a_manager_pausing_and_resuming_tells_the_operators(ui, monkeypatch):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        notify = ui.anchor.mod("ui.notify")
        clock = [1000.0]
        monkeypatch.setattr(notify, "time", type("Clock", (), {"time": staticmethod(lambda: clock[0])}))
        await _toggle(ui, h, "pause", ADMIN_USER_ID, MANAGE)
        assert ui.state() == "paused"
        await notify_wait(ui, expected=1)
        assert len(ui.dms) == 1 and ui.dms[0][0] == str(OP_ID), ui.dms
        assert "paused" in ui.dms[0][1] and f"<@{ADMIN_USER_ID}>" in ui.dms[0][1], ui.dms
        await _toggle(ui, h, "resume", ADMIN_USER_ID, MANAGE)
        assert ui.state() == "active" and len(ui.dms) == 1          # inside the window: held back
        (timer,) = ui.binding.notify._pending.values()
        clock[0] += notify.GUIDANCE_WINDOW
        fire, args = timer._callback, timer._args
        timer.cancel()
        fire(*args)
        await notify_wait(ui, expected=2)
        assert len(ui.dms) == 2 and "resumed" in ui.dms[1][1], ui.dms
    finally:
        await h.close()


async def test_an_operator_pausing_sends_no_message(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        await _toggle(ui, h, "pause", OP_ID, PLAIN)
        assert ui.state() == "paused"
        await notify_wait(ui, expected=0)
        assert ui.dms == []
    finally:
        await h.close()


async def test_a_damaged_policy_file_is_told_once_a_day(ui):
    activate_directly(ui)
    h = await panel_harness(ui)
    try:
        notify = ui.binding.notify
        assert await notify.store_unreadable("the policy file is not JSON") == 1
        assert await notify.store_unreadable("the policy file is not JSON") == 0      # same day: once
        assert len(ui.dms) == 1 and "get no answer in any of its servers" in ui.dms[0][1], ui.dms
    finally:
        await h.close()
