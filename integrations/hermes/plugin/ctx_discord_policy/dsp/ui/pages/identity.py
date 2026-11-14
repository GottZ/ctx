"""Identity in this server: the name the bot uses, the words that wake it, greetings, reply language and
the reaction emoji (server managers and operators; setting ``identity``).

Texts are edited in one form; the reaction emoji is chosen from the server's own emojis (a select with 25
per page, browsable), or typed as a standard emoji, or switched off.
"""

from __future__ import annotations

from typing import Any

from ...store.schema import DEFAULT_LANGUAGE
from .. import modals
from ..panels import Control, Ctx, Page, Panel, action, clean, modal_action, register_panel
from .common import parse_list

KEY = "identity"
SETTING = "identity"
PER_PAGE = 25
PATHS = ("identity.bot_name", "identity.wake_names", "identity.greetings", "identity.reaction_emoji",
         "conversation.language")


def _identity(ctx: Ctx) -> dict:
    return (ctx.guild or {}).get("identity") or {}


def _language(guild: dict) -> str:
    return (guild.get("conversation") or {}).get("language") or DEFAULT_LANGUAGE


def emoji_text(emoji: Any) -> str:
    if isinstance(emoji, dict) and emoji.get("id"):
        name = "".join(c for c in str(emoji.get("name", "")) if c.isalnum() or c == "_") or "emoji"
        return f"<:{name}:{emoji['id']}>"
    return clean(emoji, 32) if emoji else "none"


def summary(guild: dict, ctx: Ctx) -> str:
    ident = guild.get("identity") or {}
    name = clean(ident.get("bot_name"), 32)
    wakes = ", ".join(clean(w, 32) for w in ident.get("wake_names") or ()) or (name or "its name")
    return (f"{name or 'its server nickname'} · wakes on: {wakes} · reaction: {emoji_text(ident.get('reaction_emoji'))}"
            f" · replies in: {clean(_language(guild), 60)}")


def _server_emojis(ctx: Ctx) -> list:
    guild = getattr(ctx.interaction, "guild", None)
    return [e for e in (getattr(guild, "emojis", None) or []) if getattr(e, "available", True)]


def page(ctx: Ctx) -> Page:
    guild = ctx.guild or {}
    ident = guild.get("identity") or {}
    fields = [
        ("Name in this server", clean(ident.get("bot_name"), 32) or "its server nickname (nothing set)"),
        ("Wakes on", ", ".join(clean(w, 32) for w in ident.get("wake_names") or ()) or
         "its name (nothing set)"),
        ("Greetings that count as addressing it", ", ".join(clean(g, 24) for g in ident.get("greetings") or ())
         or "none"),
        ("Replies in", clean(_language(guild), 100)),
        ("Reaction emoji", emoji_text(ident.get("reaction_emoji")) + " — members react with it to a message to "
                                                                       "wake the bot"),
    ]
    emojis = _server_emojis(ctx)
    pages = max(1, -(-len(emojis) // PER_PAGE))
    try:
        number = min(max(int(ctx.page_arg or 0), 0), pages - 1)
    except ValueError:
        number = 0
    current = ident.get("reaction_emoji")
    current_id = str(current.get("id")) if isinstance(current, dict) else ""
    controls = [Control(SETTING, "id_edit", "Edit names and language…", style="primary", mutates=True, row=0)]
    shown = emojis[number * PER_PAGE:(number + 1) * PER_PAGE]
    if shown:
        controls.append(Control(SETTING, "id_emoji", kind="select", mutates=True,
                                placeholder=f"Reaction emoji from this server (page {number + 1} of {pages})", row=1,
                                options=tuple((e.name, str(e.id), None, str(e.id) == current_id, e) for e in shown)))
    else:
        fields.append(("Server emojis", "This server has no custom emojis the bot can use; pick a standard emoji "
                                        "instead."))
    if pages > 1:
        controls.append(Control(SETTING, "id_page", "◀ Emojis", arg=str((number - 1) % pages), row=2))
        controls.append(Control(SETTING, "id_page", "Emojis ▶", arg=str((number + 1) % pages), row=2))
    controls += [Control(SETTING, "id_emoji_std", "Standard emoji…", mutates=True, row=2),
                 Control(SETTING, "id_emoji_off", "No reaction emoji", mutates=True, disabled=current is None, row=2),
                 Control(SETTING, "id_reset", "Reset to defaults", style="danger", mutates=True, row=3),
                 Control("panel.open", "nav", "Back", arg="home", row=3)]
    return Page("Bot identity in this server", "How the bot calls itself here, what wakes it and how it answers.",
                fields, controls)


def edit_form(ctx: Ctx) -> modals.ModalSpec:
    ident = _identity(ctx)
    return modals.ModalSpec("m_id_edit", "Bot identity in this server", [
        modals.Field("bot_name", "text", "Name the bot uses here", value=ident.get("bot_name") or "",
                     required=False, max_length=32, description="Empty: its server nickname."),
        modals.Field("wake_names", "text", "Names that wake it (comma separated)",
                     value=", ".join(ident.get("wake_names") or ()), required=False, max_length=200,
                     description="Up to 5. Empty: its name."),
        modals.Field("greetings", "text", "Greeting words (comma separated)",
                     value=", ".join(ident.get("greetings") or ()), required=False, max_length=400,
                     description="Up to 12, e.g. hey, hi. A message that starts with one and a wake name addresses it."),
        modals.Field("language", "text", "Reply language", value=_language(ctx.guild or {}), max_length=100,
                     description="A language name, or “the language of the message”."),
    ])


@action("id_edit", SETTING, mutates=True, page=KEY)
async def _edit(ctx: Ctx) -> None:
    await ctx.open_modal(edit_form(ctx))


@modal_action("m_id_edit", SETTING, page=KEY)
async def _edit_submit(ctx: Ctx, values: dict) -> None:
    language = str(values.get("language") or "").strip() or DEFAULT_LANGUAGE
    changes = {
        "identity.bot_name": str(values.get("bot_name") or "").strip(),
        "identity.wake_names": parse_list(values.get("wake_names")),
        "identity.greetings": parse_list(values.get("greetings")),
        "conversation.language": language,
    }
    if await ctx.write(SETTING, changes, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice="✅ Saved.")


@action("id_page", SETTING, page=KEY)
async def _page(ctx: Ctx) -> None:
    ctx.page_arg = ctx.ref.arg
    await ctx.show_page(KEY)


@action("id_emoji", SETTING, mutates=True, page=KEY)
async def _emoji(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    chosen = next((e for e in _server_emojis(ctx) if values and str(e.id) == str(values[0])), None)
    if chosen is None:
        await ctx.say("That emoji is not one of this server's emojis. Nothing was saved. Next step: pick one "
                      "from the list again.")
        return
    if await ctx.write(SETTING, {"identity.reaction_emoji": {"id": str(chosen.id), "name": chosen.name}},
                       expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Reaction emoji: {emoji_text({'id': chosen.id, 'name': chosen.name})}.")


@action("id_emoji_std", SETTING, mutates=True, page=KEY)
async def _emoji_std(ctx: Ctx) -> None:
    await ctx.open_modal(modals.ModalSpec("m_id_emoji", "Standard reaction emoji", [modals.Field(
        "emoji", "text", "Emoji", max_length=32, placeholder="👀",
        description="Paste one standard emoji, e.g. from your emoji picker.")]))


@modal_action("m_id_emoji", SETTING, page=KEY)
async def _emoji_std_submit(ctx: Ctx, values: dict) -> None:
    emoji = str(values.get("emoji") or "").strip()
    if await ctx.write(SETTING, {"identity.reaction_emoji": emoji}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Reaction emoji: {clean(emoji, 32)}.")


@action("id_emoji_off", SETTING, mutates=True, page=KEY)
async def _emoji_off(ctx: Ctx) -> None:
    if await ctx.write(SETTING, {"identity.reaction_emoji": None}, expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice="✅ Reactions no longer wake the bot.")


@action("id_reset", SETTING, mutates=True, page=KEY)
async def _reset(ctx: Ctx) -> None:
    if await ctx.reset(SETTING, list(PATHS), expected_rev=ctx.ref.rev, page=KEY):
        await ctx.show_page(KEY, notice="✅ Identity reset to the defaults.")


register_panel(Panel(KEY, "Identity", SETTING, 10, page, summary=summary))
