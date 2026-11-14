"""Server guidance: the per-server prompt written by the server's managers (DESIGN §7; setting ``guidance``).

What a server admin can do here without any prior knowledge:

* read what the bot does with the text, what it can't do, and which placeholders exist;
* write it in a form (two parts, 4000 + 2000 characters) or upload a ``.md``/``.txt`` file;
* preview the exact context the bot would get (``dsp.prompts.assemble`` with the real capability lanes);
* see every problem before it is saved (``dsp.prompts.check_guidance``: limit, context budget, unknown
  placeholders), go back to one of the last ten versions, or reset it.

Writes go through ``ops.set_guidance`` / ``restore_guidance`` / ``reset_fields`` (the one write path the
Hermes settings page uses too); every change is announced in the audit channel and sent to the operators
with a diff (:mod:`..notify`).
"""

from __future__ import annotations

from typing import Any, Optional

from ...policy.lanes import PublicLanes, capability_lane_text
from ...policy.routes import OPERATOR, PUBLIC, Route
from ...prompts import PLACEHOLDERS, assemble, budget_from_spill, check_guidance
from ...store.ops import guidance_digest
from .. import modals, panels
from ..panels import Control, Ctx, Page, Panel, action, clean, modal_action, register_panel, when, who

KEY = "guidance"
SETTING = "guidance"
PART1, PART2 = 4000, 2000
UPLOAD_MAX_BYTES = 64 * 1024
UPLOAD_SUFFIXES = (".md", ".txt")

PREVIEWS = {
    "public.ambient": ("A member, while the bot reads along", PUBLIC, "ambient"),
    "public.direct": ("A member who addresses the bot", PUBLIC, "direct"),
    "operator.direct": ("The operator", OPERATOR, "direct"),
}
PLACEHOLDER_HELP = {
    "bot_name": "the name set under Identity (else the bot's display name)",
    "server_name": "this server's name",
    "channel_name": "the name of the channel the message is in",
    "channel": "a link to that channel",
    "language": "the reply language set under Identity",
    "worklog_channel": "a link to the worklog channel",
    "operator_label": "how the bot refers to its operator",
}
WHAT_IT_DOES = ("The bot reads this text in every member's turn as guidance about tone, topics and the norms "
                "of this server. It can't give anyone tools, make anyone the operator, or override the "
                "operator's rules; the bot is told to treat it as information, not as orders. In the operator's "
                "own turns it is used only if the operator switched that on.")


def _text(guild: Optional[dict]) -> str:
    return ((guild or {}).get("prompts") or {}).get("guild") or ""


def capabilities(ctx: Ctx, guild: Optional[dict] = None) -> dict:
    """The capability-lane sections the runtime generates for this server (``dsp.policy.lanes``)."""
    guild = ctx.guild if guild is None else guild
    lanes = PublicLanes.from_guild(guild)
    channel = str(getattr(ctx.interaction, "channel_id", None) or ctx.guild_id)
    out = {}
    for lane in (PUBLIC, OPERATOR):
        route = Route(ctx.bot_id, ctx.guild_id, channel, "0", ctx.actor.user_id, lane, lanes=lanes)
        out[lane] = capability_lane_text(route)
    return out


def budget(ctx: Ctx) -> Optional[int]:
    return budget_from_spill(ctx.binding.spill_provider())


def check(ctx: Ctx, text: str) -> Any:
    return check_guidance(text, ctx.settings(), ctx.guild, budget=budget(ctx), capabilities=capabilities(ctx))


def _chars(n: int) -> str:
    return f"{n:,}".replace(",", " ")


def summary(guild: dict, ctx: Ctx) -> str:
    text = _text(guild)
    cap = ctx.binding.prompt_cap()
    if not text:
        return f"none yet (up to {_chars(cap)} characters)"
    out = f"{_chars(len(text))} / {_chars(cap)} characters"
    history = (guild.get("prompts") or {}).get("history") or []
    if history:
        out += f" · changed by {who(history[-1].get('by'))} {when(history[-1].get('at'))}"
    return out


def page(ctx: Ctx) -> Page:
    guild = ctx.guild or {}
    text = _text(guild)
    result = check(ctx, text)
    room = "no limit" if result.room is None else f"{_chars(result.room)}"
    fields = [("Length", f"{_chars(result.chars)} / {_chars(result.max_chars)} characters · it takes "
                         f"{_chars(result.needed)} of the {room} characters left in the bot's context")]
    history = (guild.get("prompts") or {}).get("history") or []
    if history:
        last = history[-1]
        fields.append(("Last change", f"{who(last.get('by'))} {when(last.get('at'))} · version "
                                      f"`{str(last.get('sha256', ''))[:12]}`"))
    attention = list(result.problems)
    if result.unknown_placeholders:
        attention.append("These stay as written because they are not placeholders: "
                         + ", ".join(clean(u, 40) for u in result.unknown_placeholders))
    if attention:
        fields.append(("⚠️ Needs attention", "\n".join(attention)[:1000]))
    fields.append(("What the bot does with it", WHAT_IT_DOES))
    operator_turns = (guild.get("prompts") or {}).get("guild_in_operator_lane") is True
    fields.append(("In the operator's turns", "used (the operator switched it on)" if operator_turns else
                   "not used (the operator's own turns keep the bot's defaults)"))
    fields.append(("Placeholders", "\n".join(f"`{{{name}}}` — {PLACEHOLDER_HELP[name]}" for name in PLACEHOLDERS)
                   + "\n`{{` and `}}` give a literal brace; anything else in braces stays as written."))
    if not ctx.binding.modern:
        fields.append(("Longer texts", "Upload a .md or .txt file with `/policy guidance_file:`."))
    too_long = len(text) > PART1 + PART2
    controls = [
        Control(SETTING, "gd_edit", "Edit…" if not too_long else "Edit (too long for the form; upload a file)",
                style="primary", mutates=True, disabled=too_long),
    ]
    if ctx.binding.modern:
        controls.append(Control(SETTING, "gd_upload", "Upload file…", mutates=True))
    controls += [
        Control(SETTING, "gd_preview", kind="select", placeholder="Preview the context the bot gets for…",
                options=tuple((label, value, None, False) for value, (label, _, _) in PREVIEWS.items())),
        Control(SETTING, "gd_download", "Download", disabled=not text),
        Control(SETTING, "gd_history", f"History ({len(history)})", disabled=not history),
        Control(SETTING, "gd_reset", "Reset…", style="danger", mutates=True, disabled=not text),
        Control("panel.open", "nav", "Back", arg="home"),
    ]
    return Page("Server guidance", "Tell the bot what is special about this server: its topics, tone, in-jokes "
                "and rules. Written once here, it applies to every conversation in this server.", fields, controls)


def history_page(ctx: Ctx) -> Page:
    guild = ctx.guild or {}
    current = _text(guild)
    history = list((guild.get("prompts") or {}).get("history") or [])
    lines, controls, offered = [], [], set()
    current_sha = guidance_digest(current)
    for entry in reversed(history):
        sha = str(entry.get("sha256", ""))
        is_current = sha == current_sha
        lines.append(f"`{sha[:12]}` · {_chars(len(entry.get('text') or ''))} characters · {who(entry.get('by'))} "
                     f"{when(entry.get('at'))}" + (" · **current**" if is_current else ""))
        if not is_current and sha[:32] not in offered:       # a version that occurs twice gets one button
            offered.add(sha[:32])
            controls.append(Control(SETTING, "gd_restore", f"Restore {sha[:12]}", arg=sha[:32], mutates=True))
    controls = controls[:10] + [Control(SETTING, "nav", "Back", arg=KEY)]
    return Page("Guidance history", "The last ten versions, newest first. Restoring makes a version current "
                "again (as a new version, so nothing is lost).", [("Versions", "\n".join(lines) or "none")],
                controls)


def reset_page(ctx: Ctx) -> Page:
    return Page("Reset the server guidance?", "The guidance becomes empty. The current text stays in the history "
                "and can be restored.", [], [
                    Control(SETTING, "gd_reset_ok", "Reset", style="danger", mutates=True),
                    Control(SETTING, "nav", "Cancel", arg=KEY)], tone="paused")


# ── saving (one place for form, upload and restore) ──────────────────────────────────────────────────


def _problems(result: Any) -> str:
    return " ".join(result.problems)


async def save(ctx: Ctx, text: str, rev: int) -> None:
    text = text.replace("\r\n", "\n").replace("\r", "\n")
    result = check(ctx, text)
    if result.problems:
        await ctx.say(f"Nothing was saved: {_problems(result)} Next step: shorten the text, or ask the operator "
                      "to raise the limit.")
        return
    if await ctx.write_guidance(text, expected_rev=rev, page=KEY):
        notice = f"✅ Saved: {_chars(len(text))} characters." if text else "✅ The guidance is now empty."
        if result.unknown_placeholders:
            notice += (" ⚠️ Not placeholders, kept as written: "
                       + ", ".join(clean(u, 40) for u in result.unknown_placeholders))
        await ctx.show_page(KEY, notice=notice)


def edit_form(ctx: Ctx) -> modals.ModalSpec:
    text = _text(ctx.guild)
    return modals.ModalSpec("m_gd_edit", "Server guidance", [
        modals.Field("part1", "paragraph", "Part 1", value=text[:PART1], required=False, max_length=PART1,
                     description="Topics, tone, rules of this server. Placeholders like {bot_name} work."),
        modals.Field("part2", "paragraph", "Part 2 (optional)", value=text[PART1:PART1 + PART2], required=False,
                     max_length=PART2, description="Continues part 1 directly."),
    ])


@action("gd_edit", SETTING, mutates=True, page=KEY)
async def _edit(ctx: Ctx) -> None:
    if len(_text(ctx.guild)) > PART1 + PART2:
        await ctx.say("This guidance is longer than the form can hold. Next step: download it, edit the file and "
                      "upload it again.")
        return
    await ctx.open_modal(edit_form(ctx))


@modal_action("m_gd_edit", SETTING, page=KEY)
async def _edit_submit(ctx: Ctx, values: dict) -> None:
    await save(ctx, str(values.get("part1") or "") + str(values.get("part2") or ""), ctx.ref.rev)


# ── upload ───────────────────────────────────────────────────────────────────────────────────────────


@action("gd_upload", SETTING, mutates=True, page=KEY)
async def _upload(ctx: Ctx) -> None:
    if not ctx.binding.modern:
        await ctx.say("Upload a .md or .txt file with `/policy guidance_file:` in this server.")
        return
    await ctx.open_modal(modals.ModalSpec("m_gd_upload", "Upload guidance file", [
        modals.Field("file", "file", "File (.md or .txt, UTF-8, up to 64 KB)",
                     description="Its text becomes the server guidance."),
        modals.Field("replace", "checkbox", "Replace the current guidance", value=True,
                     description="Off: the file's text is added at the end."),
    ]))


async def read_upload(ctx: Ctx, attachment: Any) -> tuple:
    """``(text, problem)``: the file's text, or a user-facing reason why it can't be used."""
    name = str(getattr(attachment, "filename", "") or "")
    if not name.lower().endswith(UPLOAD_SUFFIXES):
        return None, "only .md and .txt files can be used."
    size = getattr(attachment, "size", None)
    if isinstance(size, int) and size > UPLOAD_MAX_BYTES:
        return None, f"the file has {_chars(size)} bytes; at most {_chars(UPLOAD_MAX_BYTES)} are accepted."
    try:
        data = await ctx.binding.read_attachment(attachment)
    except Exception:
        return None, "the file could not be downloaded from Discord."
    if len(data) > UPLOAD_MAX_BYTES:
        return None, f"the file has more than {_chars(UPLOAD_MAX_BYTES)} bytes."
    try:
        return data.decode("utf-8-sig"), None
    except UnicodeDecodeError:
        return None, "the file is not UTF-8 text."


async def apply_upload(ctx: Ctx, attachment: Any, *, replace: bool, rev: int) -> None:
    await ctx.acknowledge()                     # downloading may take longer than Discord's 3-second window
    text, problem = await read_upload(ctx, attachment)
    if problem:
        await ctx.say(f"Nothing was saved: {problem} Next step: upload a UTF-8 .md or .txt file of at most 64 KB.")
        return
    if not replace:
        current = _text(ctx.guild)
        text = current + ("\n\n" if current and text else "") + text
    await save(ctx, text, rev)


@modal_action("m_gd_upload", SETTING, page=KEY)
async def _upload_submit(ctx: Ctx, values: dict) -> None:
    files = list(values.get("file") or [])
    if len(files) != 1:
        await ctx.say("Nothing was saved: attach exactly one file. Next step: try again.")
        return
    await apply_upload(ctx, files[0], replace=values.get("replace") is not False, rev=ctx.ref.rev)


async def upload_from_command(binding: Any, interaction: Any, attachment: Any) -> None:
    """``/policy guidance_file:`` (the upload path without modal file fields, K6 classic)."""
    ctx = panels.make_ctx(binding, interaction)
    if ctx is None:
        await interaction.response.send_message("/policy works inside a server, not in direct messages. Next "
                                                "step: run it in the server you want to set up.", ephemeral=True)
        return
    for key in ("panel.open", SETTING):
        if not ctx.may(key):
            await ctx.say(panels.perms.refusal(ctx.actor, key))
            return
    if not ctx.snapshot.writable:
        await ctx.say(f"Nothing was saved: the policy file on the host is {ctx.snapshot.state}. Next step: tell "
                      "the bot's operator.")
        return
    await apply_upload(ctx, attachment, replace=True, rev=ctx.rev)


# ── preview, download, history, reset ────────────────────────────────────────────────────────────────


def preview(ctx: Ctx, key: str) -> Any:
    _, lane, mode = PREVIEWS[key]
    guild_obj = getattr(ctx.interaction, "guild", None)
    channel = getattr(ctx.interaction, "channel", None)
    me = getattr(guild_obj, "me", None) or getattr(getattr(ctx.interaction, "client", None), "user", None)
    return assemble(ctx.settings(), ctx.guild, lane=lane, mode=mode, capability=capabilities(ctx)[lane],
                    budget=budget(ctx), server_name=getattr(guild_obj, "name", "") or "",
                    channel_name=getattr(channel, "name", "") or "",
                    channel_id=getattr(ctx.interaction, "channel_id", "") or "",
                    bot_display_name=getattr(me, "display_name", "") or "")


@action("gd_preview", SETTING, page=KEY)
async def _preview(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    key = values[0] if values else ""
    if key not in PREVIEWS:
        await ctx.say("That preview is not available. Next step: pick one from the list.")
        return
    result = preview(ctx, key)
    states = {"included": "included", "shortened": "shortened to fit", "empty": "empty",
              "excluded_operator_lane": "not used in operator turns", "no_room": "left out: no room"}
    limit = "no limit" if result.budget is None else _chars(result.budget)
    text = (f"**Preview — {PREVIEWS[key][0]}**\n{_chars(result.length)} of {limit} characters · server guidance: "
            f"{states.get(result.guidance, result.guidance)}")
    if result.unknown_placeholders:
        text += " · kept as written: " + ", ".join(clean(u, 40)
                                                   for u in result.unknown_placeholders)
    if result.over_budget:
        text += "\n⚠️ The operator's fixed parts alone exceed the context budget; tell the operator."
    text += ("\nThe attached file is the context assembled for such a turn: operator frame, what the turn may "
             "use, conversation rules and this server's guidance, in that order.")
    await ctx.send_file(text, f"preview-{key.replace('.', '-')}.md", result.text.encode("utf-8"))


@action("gd_download", SETTING, page=KEY)
async def _download(ctx: Ctx) -> None:
    text = _text(ctx.guild)
    if not text:
        await ctx.say("The guidance is empty; there is nothing to download.")
        return
    await ctx.send_file(f"Server guidance, version `{guidance_digest(text)[:12]}`.", "guidance.md",
                        text.encode("utf-8"))


@action("gd_history", SETTING, page=KEY)
async def _history(ctx: Ctx) -> None:
    await ctx.show(history_page(ctx))


@action("gd_restore", SETTING, mutates=True, page=KEY)
async def _restore(ctx: Ctx) -> None:
    from ...store.ops import find_guidance_version
    from ...store.schema import SchemaError

    try:
        version = find_guidance_version(ctx.guild or {}, ctx.ref.arg)
    except SchemaError as exc:
        await ctx.say(f"Nothing was restored: {exc}. Next step: open the history again.")
        return
    result = check(ctx, version.get("text") or "")
    if result.problems:
        await ctx.say(f"Nothing was restored: {_problems(result)} Next step: ask the operator to raise the limit.")
        return
    if await ctx.write_guidance(None, restore=ctx.ref.arg, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Restored version `{ctx.ref.arg[:12]}`.")


@action("gd_reset", SETTING, mutates=True, page=KEY)
async def _reset(ctx: Ctx) -> None:
    await ctx.show(reset_page(ctx))


@action("gd_reset_ok", SETTING, mutates=True, page=KEY)
async def _reset_ok(ctx: Ctx) -> None:
    if await ctx.reset(SETTING, ["prompts.guild"], expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice="✅ The guidance is now empty. The old text is in the history.")


register_panel(Panel(KEY, "Guidance", SETTING, 30, page, summary=summary))
