"""Channels: where the bot reads along (ambient) and where it never answers (server managers and operators;
setting ``channels``). Everywhere else it answers only when addressed.

Each list is a channel select of this server; a choice is saved at once. Discord shows at most 25 chosen
channels in one select, so a longer list (for example from an import) is shown but not edited here.
"""

from __future__ import annotations

from ..panels import Control, Ctx, Page, Panel, action, register_panel
from .common import foreign, guild_channel_ids, mentions, selected_ids

KEY = "channels"
SETTING = "channels"
SELECT_MAX = 25
LISTS = {
    "ambient": ("conversation.ambient_channels", "Reads along in (it sees every message there)"),
    "excluded": ("conversation.excluded_channels", "Never responds in"),
}


def _conversation(guild: dict) -> dict:
    return guild.get("conversation") or {}


def summary(guild: dict, ctx: Ctx) -> str:
    conv = _conversation(guild)
    ambient = conv.get("ambient_channels") or []
    text = f"reads along in {mentions(ambient)}" if ambient else "only when addressed (mention, reply, reaction, name)"
    excluded = conv.get("excluded_channels") or []
    return text + (f" · never in {mentions(excluded)}" if excluded else "")


def page(ctx: Ctx) -> Page:
    conv = _conversation(ctx.guild or {})
    fields = []
    controls = []
    for which, (path, label) in LISTS.items():
        current = list(conv.get(path.split(".")[-1]) or [])
        fields.append((label, mentions(current)))
        if len(current) > SELECT_MAX:
            fields.append((f"{label}: too long for a menu",
                           f"This list has {len(current)} channels; a Discord menu holds {SELECT_MAX}. Change it "
                           "with `hermes ctx-discord-policy import` on the host, or ask the operator."))
            continue
        controls.append(Control(SETTING, "ch_set", kind="channel_select", arg=which, mutates=True, min_values=0,
                                max_values=SELECT_MAX, defaults=tuple(current),
                                placeholder=label if current else f"{label}: none chosen"))
    fields.append(("Everywhere else", "The bot answers only when addressed: a mention, a reply to it, a reaction "
                                      "with its emoji, or its name at the start of a message."))
    controls.append(Control("panel.open", "nav", "Back", arg="home"))
    return Page("Channels", "Choose where the bot reads along and where it stays silent.", fields, controls)


@action("ch_set", SETTING, mutates=True, page=KEY)
async def _set(ctx: Ctx) -> None:
    entry = LISTS.get(ctx.ref.arg)
    if entry is None:
        await ctx.say("This control is not known to this version of the plugin. Next step: run /policy again.")
        return
    path, label = entry
    chosen = selected_ids(ctx.item)
    stray = foreign(chosen, guild_channel_ids(ctx))
    if stray:
        await ctx.say("Nothing was saved: some chosen channels are not part of this server. Next step: choose "
                      "channels of this server.")
        return
    other_path = LISTS["excluded" if ctx.ref.arg == "ambient" else "ambient"][0]
    other = set(_conversation(ctx.guild or {}).get(other_path.split(".")[-1]) or [])
    both = [c for c in chosen if c in other]
    if both:
        await ctx.say(f"Nothing was saved: {mentions(both)} can't be in both lists. Next step: remove it from the "
                      "other list first.")
        return
    if await ctx.write(SETTING, {path: chosen}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Saved: {label.split(' (')[0].lower()} {mentions(chosen)}.")


register_panel(Panel(KEY, "Channels", SETTING, 20, page, summary=summary))
