"""Member tools: what members (not the operator) may let the bot do in this server (operators only;
setting ``public_lanes`` — these entries release the operator's data and tools to members).

Only entries of the code catalog (``dsp.policy.lanes``) exist; this panel switches them on or off and fills
their lists. Skills are checked against the skills installed in the profile this server's turns run in;
when that list is unknown, no new skill can be released (removing is always possible).
"""

from __future__ import annotations

from ...store.schema import SchemaError
from .. import modals
from ..panels import Control, Ctx, Page, Panel, action, clean, modal_action, register_panel
from .common import onoff, parse_list

KEY = "lanes"
SETTING = "public_lanes"
LIMITS = (10, 25, 50, 100)
FORM_MAX = 4000                     # Discord's longest text input


def _lanes(guild: dict) -> dict:
    return guild.get("lanes") or {}


def summary(guild: dict, ctx: Ctx) -> str:
    lanes = _lanes(guild)
    history = lanes.get("channel_history") or {}
    return " · ".join([
        f"channel history ≤{history.get('max_limit', 100)}" if history.get("enabled") else "no channel history",
        "permalinks" if lanes.get("permalink_hydration") else "no permalinks",
        f"{len(lanes.get('skills') or [])} skills", f"{len(lanes.get('knowledge') or [])} knowledge entries"])


def page(ctx: Ctx) -> Page:
    lanes = _lanes(ctx.guild or {})
    history = lanes.get("channel_history") or {}
    enabled = bool(history.get("enabled"))
    limit = int(history.get("max_limit", 100))
    skills = lanes.get("skills") or []
    knowledge = lanes.get("knowledge") or []
    fields = [
        ("Read the current channel", f"{onoff(enabled)} — up to {limit} recent messages of the channel the "
                                     "member writes in, nothing else"),
        ("Open message links", onoff(lanes.get("permalink_hydration")) + " — the bot reads a linked message of "
                                                                         "this server when the member can see it"),
        ("Skills members may use", ", ".join(clean(s, 64) for s in skills) or "none"),
        ("Knowledge entries members may look up",
         "\n".join(f"{clean(e.get('tool'), 80)} {clean(e.get('id'), 80)}" for e in knowledge) or "none"),
        ("Never", "Members never get the operator's other tools; the runtime blocks every call outside these "
                  "entries."),
    ]
    controls = [
        Control(SETTING, "ln_history", f"Channel history: {onoff(enabled)}", arg=onoff(not enabled), mutates=True),
        Control(SETTING, "ln_limit", kind="select", mutates=True, placeholder="How many recent messages",
                options=tuple((f"up to {n} messages", str(n), None, n == limit) for n in LIMITS)),
        Control(SETTING, "ln_links", f"Message links: {onoff(lanes.get('permalink_hydration'))}",
                arg=onoff(not lanes.get("permalink_hydration")), mutates=True),
        Control(SETTING, "ln_skills", "Skills…", mutates=True),
        Control(SETTING, "ln_knowledge", "Knowledge entries…", mutates=True),
        Control("panel.open", "nav", "Back", arg="home"),
    ]
    return Page("Member tools", "What members may let the bot do for them in this server. Every entry here "
                "shares something of the operator's with the server.", fields, controls)


@action("ln_history", SETTING, mutates=True, page=KEY)
async def _history(ctx: Ctx) -> None:
    value = ctx.ref.arg == "on"
    if await ctx.write(SETTING, {"lanes.channel_history.enabled": value}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Channel history: {onoff(value)}.")


@action("ln_limit", SETTING, mutates=True, page=KEY)
async def _limit(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    try:
        limit = int(values[0])
    except (IndexError, ValueError):
        limit = 0
    if limit not in LIMITS:
        await ctx.say("That choice is not available. Nothing was saved. Next step: pick one from the list.")
        return
    if await ctx.write(SETTING, {"lanes.channel_history.max_limit": limit}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Up to {limit} messages.")


@action("ln_links", SETTING, mutates=True, page=KEY)
async def _links(ctx: Ctx) -> None:
    value = ctx.ref.arg == "on"
    if await ctx.write(SETTING, {"lanes.permalink_hydration": value}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Message links: {onoff(value)}.")


@action("ln_skills", SETTING, mutates=True, page=KEY)
async def _skills(ctx: Ctx) -> None:
    current = _lanes(ctx.guild or {}).get("skills") or []
    await ctx.open_modal(modals.ModalSpec("m_ln_skills", "Skills members may use", [modals.Field(
        "skills", "paragraph", "Skill names, one per line", value="\n".join(current), required=False,
        max_length=1800, description="Only skills installed in the bot's profile for this server.")]))


def check_skills(ctx: Ctx, names: list) -> str:
    """Empty when every name may be released; otherwise the user-facing reason."""
    current = set(_lanes(ctx.guild or {}).get("skills") or [])
    new = [n for n in names if n not in current]
    if not new:
        return ""                                      # only removals or reordering: nothing to verify
    installed = ctx.binding.skills_provider(ctx)
    if installed is None:
        return ("The bot can't read which skills its profile for this server has, so no new skill can be "
                "released here right now. Next step: try again after the bot reconnects, or ask the person who "
                "runs the bot.")
    missing = [n for n in new if n not in set(installed)]
    if missing:
        return (f"Not installed in the bot's profile for this server: {', '.join(clean(m, 64) for m in missing)}. "
                "Next step: install the skill there first, or check the spelling.")
    return ""


@modal_action("m_ln_skills", SETTING, page=KEY)
async def _skills_submit(ctx: Ctx, values: dict) -> None:
    names = parse_list(values.get("skills"))
    problem = check_skills(ctx, names)
    if problem:
        await ctx.say(f"Nothing was saved. {problem}")
        return
    if await ctx.write(SETTING, {"lanes.skills": names}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Skills: {', '.join(names) or 'none'}.")


@action("ln_knowledge", SETTING, mutates=True, page=KEY)
async def _knowledge(ctx: Ctx) -> None:
    current = _lanes(ctx.guild or {}).get("knowledge") or []
    text = "\n".join(f"{e['tool']} {e['id']}" for e in current)
    if len(text) > FORM_MAX:
        await ctx.say("These entries are longer than a Discord form can hold. Next step: change them on the host "
                      "with `hermes ctx-discord-policy export` and `import`.")
        return
    await ctx.open_modal(modals.ModalSpec("m_ln_knowledge", "Knowledge entries", [modals.Field(
        "knowledge", "paragraph", "One entry per line: tool and id", required=False, max_length=FORM_MAX,
        value=text,
        description="Tool = an MCP read tool (mcp__<server>__get…); id = the entry members may look up.")]))


def parse_knowledge(text: str) -> list:
    out = []
    for number, line in enumerate(str(text or "").splitlines(), start=1):
        line = line.strip()
        if not line:
            continue
        parts = line.split(None, 1)
        if len(parts) != 2:
            raise SchemaError(f"line {number}: expected a tool name and an id separated by a space")
        entry = {"tool": parts[0], "id": parts[1].strip()}
        if entry not in out:
            out.append(entry)
    return out


@modal_action("m_ln_knowledge", SETTING, page=KEY)
async def _knowledge_submit(ctx: Ctx, values: dict) -> None:
    try:
        entries = parse_knowledge(values.get("knowledge"))
    except SchemaError as exc:
        await ctx.say(f"Nothing was saved: {exc}. Next step: one entry per line, e.g. `mcp__notes__get_note 42`.")
        return
    if await ctx.write(SETTING, {"lanes.knowledge": entries}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ {len(entries)} knowledge entries.")


register_panel(Panel(KEY, "Member tools", SETTING, 70, page, summary=summary))
