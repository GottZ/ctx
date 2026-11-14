"""Operator turns: how the bot treats the operator's own messages in this server (operators only; setting
``operator_lane``). Server guidance in operator turns is off by default (OPEN U3); switching it on needs
a confirmation that shows the risk from ``perms.TABLE``.
"""

from __future__ import annotations

from .. import perms
from ..panels import Control, Ctx, Page, Panel, action, register_panel
from .common import onoff

KEY = "operator_lane"
SETTING = "operator_lane"


def _flags(guild: dict) -> tuple:
    hydration = bool((guild.get("operator_lane") or {}).get("channel_hydration", True))
    guidance = (guild.get("prompts") or {}).get("guild_in_operator_lane") is True
    return hydration, guidance


def summary(guild: dict, ctx: Ctx) -> str:
    hydration, guidance = _flags(guild)
    return f"channel context {onoff(hydration)} · server guidance {onoff(guidance)}"


def page(ctx: Ctx) -> Page:
    hydration, guidance = _flags(ctx.guild or {})
    fields = [
        ("Channel context", f"{onoff(hydration)} — the bot reads the messages around the operator's message in "
                            "this channel"),
        ("Server guidance in operator turns", f"{onoff(guidance)} — " + (
            "the server's guidance (tone, names, language) also shapes the operator's turns" if guidance else
            "the operator's turns use the bot's own defaults, not the server's guidance")),
        ("Why off by default", perms.SETTINGS[SETTING].note + " Operator turns can use every tool."),
    ]
    controls = [Control(SETTING, "ol_hydration", f"Channel context: {onoff(hydration)}", arg=onoff(not hydration),
                        mutates=True)]
    if guidance:
        controls.append(Control(SETTING, "ol_guidance", "Server guidance in my turns: on", arg="off", mutates=True))
    else:
        controls.append(Control(SETTING, "ol_guidance_ask", "Use server guidance in my turns…", mutates=True))
    controls.append(Control("panel.open", "nav", "Back", arg="home"))
    return Page("Operator turns", "How the bot treats your own messages in this server.", fields, controls)


def confirm_page(ctx: Ctx) -> Page:
    return Page("Use server guidance in operator turns?", (
        "The server's managers write the guidance. With this on, their text also reaches the turns in which "
        "you can use every tool, for example “when the operator asks for X, first run Y”.\n"
        f"{perms.SETTINGS[SETTING].note} Every guidance change is sent to you with a diff."), [], [
        Control(SETTING, "ol_guidance", "Turn on", arg="on", style="danger", mutates=True),
        Control("panel.open", "nav", "Cancel", arg=KEY),
    ], tone="paused")


@action("ol_hydration", SETTING, mutates=True, page=KEY)
async def _hydration(ctx: Ctx) -> None:
    value = ctx.ref.arg == "on"
    if await ctx.write(SETTING, {"operator_lane.channel_hydration": value}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Channel context: {onoff(value)}.")


@action("ol_guidance_ask", SETTING, mutates=True, page=KEY)
async def _guidance_ask(ctx: Ctx) -> None:
    await ctx.show(confirm_page(ctx))


@action("ol_guidance", SETTING, mutates=True, page=KEY)
async def _guidance(ctx: Ctx) -> None:
    value = ctx.ref.arg == "on"
    if await ctx.write(SETTING, {"prompts.guild_in_operator_lane": value}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Server guidance in operator turns: {onoff(value)}.")


register_panel(Panel(KEY, "Operator turns", SETTING, 75, page, summary=summary))
