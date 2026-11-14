"""Worklog: the channel where the operator's tool work is written down (operators only; setting
``worklog`` — worklogs carry the output of the operator's tools, OPEN L3).
"""

from __future__ import annotations

from typing import Any, Optional

from ...store.schema import ARCHIVE_MINUTES
from ..panels import Control, Ctx, Page, Panel, action, register_panel
from .common import foreign, guild_channel_ids, onoff, selected_ids

KEY = "worklog"
SETTING = "worklog"
ARCHIVE_TEXT = {60: "1 hour", 1440: "24 hours", 4320: "3 days", 10080: "7 days"}
PRIVACY = ("Keep this channel visible to operators only: the kickoff message of every worklog shows the "
           "operator's request text, and the log shows what the tools returned.")


def _worklog(guild: dict) -> dict:
    return guild.get("worklog") or {}


def summary(guild: dict, ctx: Ctx) -> str:
    wl = _worklog(guild)
    if not wl.get("channel"):
        return "off"
    return (f"<#{wl['channel']}> · archive after {ARCHIVE_TEXT.get(wl.get('archive_minutes'), '?')}"
            + (" · final copy to origin" if wl.get("final_to_origin") else ""))


def _everyone_sees(ctx: Ctx, channel_id: Any) -> Optional[bool]:
    guild = getattr(ctx.interaction, "guild", None)
    channel = guild.get_channel(int(channel_id)) if guild is not None and channel_id else None
    everyone = getattr(guild, "default_role", None)
    if channel is None or everyone is None:
        return None
    try:
        return bool(channel.permissions_for(everyone).view_channel)
    except Exception:
        return None


def page(ctx: Ctx) -> Page:
    wl = _worklog(ctx.guild or {})
    channel = wl.get("channel")
    fields = [("Channel", f"<#{channel}>" if channel else "off — tool work stays in the conversation"),
              ("Archive threads after", ARCHIVE_TEXT.get(wl.get("archive_minutes"), "?")),
              ("Final copy to the original channel", onoff(wl.get("final_to_origin"))),
              ("Privacy", PRIVACY)]
    if channel and _everyone_sees(ctx, channel):
        fields.insert(1, ("⚠️ Everyone can read this channel",
                          "Members would see the operator's requests and tool output. Restrict the channel to "
                          "operators in its Discord permissions."))
    final = bool(wl.get("final_to_origin"))
    controls = [
        Control(SETTING, "wl_channel", kind="channel_select", mutates=True, min_values=0, max_values=1,
                defaults=(channel,) if channel else (), channel_types=("text",),
                placeholder="Worklog channel (none: off)"),
        Control(SETTING, "wl_archive", kind="select", mutates=True, placeholder="Archive threads after",
                options=tuple((ARCHIVE_TEXT[m], str(m), None, m == wl.get("archive_minutes")) for m in ARCHIVE_MINUTES)),
        Control(SETTING, "wl_final", f"Final copy to origin: {onoff(final)}", arg=onoff(not final), mutates=True),
        Control("panel.open", "nav", "Back", arg="home"),
    ]
    return Page("Worklog", "Where the bot writes down the operator's tool work in this server.", fields, controls)


@action("wl_channel", SETTING, mutates=True, page=KEY)
async def _channel(ctx: Ctx) -> None:
    chosen = selected_ids(ctx.item)[:1]
    if foreign(chosen, guild_channel_ids(ctx)):
        await ctx.say("Nothing was saved: that channel is not part of this server. Next step: choose a channel "
                      "of this server.")
        return
    value = chosen[0] if chosen else None
    if await ctx.write(SETTING, {"worklog.channel": value}, expected_rev=ctx.ref.rev, page=KEY):
        note = f"✅ Worklog in <#{value}>." if value else "✅ Worklog off."
        if value and _everyone_sees(ctx, value):
            note += " ⚠️ Everyone can read that channel; restrict it to operators."
        await ctx.show_page(KEY, notice=note)


@action("wl_archive", SETTING, mutates=True, page=KEY)
async def _archive(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    try:
        minutes = int(values[0])
    except (IndexError, ValueError):
        minutes = -1
    if await ctx.write(SETTING, {"worklog.archive_minutes": minutes}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Threads are archived after {ARCHIVE_TEXT[minutes]}.")


@action("wl_final", SETTING, mutates=True, page=KEY)
async def _final(ctx: Ctx) -> None:
    value = ctx.ref.arg == "on"
    if await ctx.write(SETTING, {"worklog.final_to_origin": value}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Final copy to origin: {onoff(value)}.")


register_panel(Panel(KEY, "Worklog", SETTING, 60, page, summary=summary))
