"""Managers: who may manage this server's policy, and the audit channel (operators only; setting
``managers``). Server managers are people with Manage Server; the operator can narrow that to a role
(Manage Server **and** the role) or to operators only.
"""

from __future__ import annotations

from ...store.schema import MANAGER_MODES
from ..panels import Control, Ctx, Page, Panel, action, register_panel
from .common import foreign, guild_channel_ids, guild_role_ids, mentions, selected_ids

KEY = "managers"
SETTING = "managers"
MODE_TEXT = {
    "discord_admins": ("Everyone with Manage Server", "Discord's own rule; the default."),
    "role": ("Manage Server and one of these roles", "Choose the roles below."),
    "operators_only": ("Operators only", "Nobody in the server can change the policy."),
}


def _managers(guild: dict) -> dict:
    return guild.get("managers") or {}


def summary(guild: dict, ctx: Ctx) -> str:
    managers = _managers(guild)
    mode = managers.get("mode", "discord_admins")
    text = MODE_TEXT.get(mode, (mode, ""))[0]
    if mode == "role":
        text += ": " + mentions(managers.get("role_ids") or (), "@&")
    audit = guild.get("audit_channel")
    return text + (f" · audit in <#{audit}>" if audit else "")


def page(ctx: Ctx) -> Page:
    guild = ctx.guild or {}
    managers = _managers(guild)
    mode = managers.get("mode", "discord_admins")
    roles = list(managers.get("role_ids") or [])
    audit = guild.get("audit_channel")
    fields = [("Who may manage", MODE_TEXT.get(mode, (mode, ""))[0]),
              ("Roles", mentions(roles, "@&") + ("" if mode == "role" else " (used when the mode is a role)")),
              ("Audit channel", (f"<#{audit}> — every change made here is announced there" if audit else
                                 "none — changes are recorded only in the policy file"))]
    if mode == "role" and not roles:
        fields.insert(1, ("⚠️ No role chosen", "Until you choose one, nobody in the server can manage the policy."))
    controls = [
        Control(SETTING, "mg_mode", kind="select", mutates=True, placeholder="Who may manage",
                options=tuple((MODE_TEXT[m][0], m, MODE_TEXT[m][1], m == mode) for m in MANAGER_MODES)),
        Control(SETTING, "mg_roles", kind="role_select", mutates=True, min_values=0, max_values=25,
                defaults=tuple(roles), placeholder="Roles that may manage (with Manage Server)"),
        Control(SETTING, "mg_audit", kind="channel_select", mutates=True, min_values=0, max_values=1,
                defaults=(audit,) if audit else (), channel_types=("text",), placeholder="Audit channel (none: off)"),
        Control("panel.open", "nav", "Back", arg="home"),
    ]
    return Page("Managers", "Who in this server may change the bot's policy, and where changes are announced.",
                fields, controls)


@action("mg_mode", SETTING, mutates=True, page=KEY)
async def _mode(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    mode = values[0] if values else ""
    if await ctx.write(SETTING, {"managers.mode": mode}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ {MODE_TEXT[mode][0]}.")


@action("mg_roles", SETTING, mutates=True, page=KEY)
async def _roles(ctx: Ctx) -> None:
    chosen = selected_ids(ctx.item)
    if foreign(chosen, guild_role_ids(ctx)):
        await ctx.say("Nothing was saved: some chosen roles are not part of this server. Next step: choose roles "
                      "of this server.")
        return
    if await ctx.write(SETTING, {"managers.role_ids": chosen}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Roles: {mentions(chosen, '@&')}.")


@action("mg_audit", SETTING, mutates=True, page=KEY)
async def _audit(ctx: Ctx) -> None:
    chosen = selected_ids(ctx.item)[:1]
    if foreign(chosen, guild_channel_ids(ctx)):
        await ctx.say("Nothing was saved: that channel is not part of this server. Next step: choose a channel "
                      "of this server.")
        return
    value = chosen[0] if chosen else None
    if await ctx.write(SETTING, {"audit_channel": value}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Audit channel: <#{value}>." if value else "✅ Audit channel off.")


register_panel(Panel(KEY, "Managers", SETTING, 80, page, summary=summary))
