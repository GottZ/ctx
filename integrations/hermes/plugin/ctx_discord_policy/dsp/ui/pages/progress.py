"""Example panel: how the bot shows progress while it works with tools (``progress.grouping``).

The template for every settings panel:

* :func:`page` builds the page from the stored guild; every :class:`~..panels.Control` names the perms
  setting (``"progress"``), so visibility and enforcement both come from :data:`..perms.TABLE`;
* full strategy: a "Change…" button opens a modal with a ``RadioGroup`` (options with descriptions);
  classic strategy (no modern modal parts): the same choice as a select on the panel itself;
* both paths end in :func:`_save`, which writes through :meth:`~..panels.Ctx.write` (rights for exactly
  ``progress.grouping`` + optimistic ``rev``) and shows the panel again with a confirmation line;
* ``register_panel`` adds the navigation button, the start-page summary row and the ``/policy`` choice.
"""

from __future__ import annotations

from typing import Any

from ...store.schema import GROUPINGS
from .. import modals
from ..panels import OUTCOME_WORDS, Control, Ctx, Page, Panel, action, clean, modal_action, register_panel

KEY = "progress"
SETTING = "progress"

CHOICES = {
    "default": ("Hermes default", "Follows the display setting of the bot's Hermes profile."),
    "rotate": ("Newest line only", "One progress line that is replaced as the work goes on."),
    "separate": ("One message per tool", "Every tool step gets its own message."),
}


def current(ctx: Ctx) -> str:
    return ((ctx.guild or {}).get("progress") or {}).get("grouping", "default")


def _status(ctx: Ctx, grouping: str) -> str:
    intervention = {"rotate": "K2", "separate": "K2b"}.get(grouping)
    if intervention is None:
        return "Hermes decides (profile setting)."
    result = ctx.binding.runtime.status.get(intervention)
    if result is None:
        return "Not checked yet (the check runs when the bot connects)."
    word = OUTCOME_WORDS.get(result.outcome.value, result.outcome.value)
    reason = f" ({clean(result.reason, 80)})" if ctx.may("status.full") and result.reason else ""
    return f"{word}{reason}"


def page(ctx: Ctx) -> Page:
    chosen = current(ctx)
    lines = [(f"● **{label}**" if value == chosen else f"○ {label}") + f" — {desc}"
             for value, (label, desc) in CHOICES.items()]
    fields = [("Choice", "\n".join(lines)),
              ("Status in this Hermes", _status(ctx, chosen)),
              ("Cleanup", "Removing progress messages after the answer is a Hermes setting of the bot's "
                          "profile (`display.cleanup_progress`).")]
    if ctx.binding.modern:
        change = Control(SETTING, "prog_edit", "Change…", style="primary", mutates=True)
    else:
        change = Control(SETTING, "prog_set", kind="select", mutates=True, placeholder="Choose how progress appears",
                         options=tuple((label, value, desc[:100], value == chosen)
                                       for value, (label, desc) in CHOICES.items()))
    return Page("Progress display for tool work",
                "How the bot shows its progress while it works with tools in this server.",
                fields, [change, Control("panel.open", "nav", "Back", arg="home")])


def modal(ctx: Ctx) -> modals.ModalSpec:
    return modals.ModalSpec("m_progress", "Progress display", [modals.Field(
        "grouping", "radio", "How should tool progress appear?",
        description="Applies to tool work in this server.", value=current(ctx),
        options=tuple(modals.Option(label, value, desc[:100]) for value, (label, desc) in CHOICES.items()))])


async def _save(ctx: Ctx, value: Any, rev: int) -> None:
    if value not in GROUPINGS:
        await ctx.say("That choice is not available. Nothing was saved. Next step: run /policy again.")
        return
    if await ctx.write(SETTING, {"progress.grouping": value}, expected_rev=rev, page=KEY):
        await ctx.show_page(KEY, notice=f"✅ Saved: {CHOICES[value][0]}.")


@action("prog_edit", SETTING, mutates=True, page=KEY)
async def _edit(ctx: Ctx) -> None:
    if not ctx.binding.modern:
        await ctx.show_page(KEY)
        return
    await ctx.open_modal(modal(ctx))


@action("prog_set", SETTING, mutates=True, page=KEY)
async def _select(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    await _save(ctx, values[0] if values else None, ctx.ref.rev)


@modal_action("m_progress", SETTING, page=KEY)
async def _submit(ctx: Ctx, values: dict) -> None:
    await _save(ctx, values.get("grouping"), ctx.ref.rev)


register_panel(Panel(KEY, "Progress", SETTING, 50, page,
                     summary=lambda guild, ctx: CHOICES.get((guild.get("progress") or {}).get("grouping"),
                                                            ("?", ""))[0]))
