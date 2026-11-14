"""Panel "Models": the model of members' turns and of operator turns in this server (PROFILE-PER-GUILD §B7).

Operators choose both models, release up to 25 models to the server's managers and decide what happens
when a chosen model is unavailable. Managers see the members' model and, when the operator released
models, choose among exactly those. Choosing runs in two steps (provider, then model) because a Discord
select holds at most 25 options; both lists page instead of cutting off. Every model is checked when it is
saved: the server's profile must hold a credential for the provider (by name) and the context window must
reach Hermes' minimum of 64 000 tokens.
"""

from __future__ import annotations

import asyncio
import logging
import os
from typing import Any, Optional

from ... import hermes
from ...models import catalog, check, runtime
from ...routing import apply
from ...runtime import BoundedMap
from ...store import ops
from ...store.schema import MODEL_CHOICES_MAX, ON_UNAVAILABLE
from .. import modals
from ..panels import OUTCOME_WORDS, Control, Ctx, Page, Panel, action, clean, modal_action, register_panel
from .profile import defer_in_place, guard, run_change, view as routing_view

logger = logging.getLogger("ctx_discord_policy")

KEY = "models"
SETTING = "model"           # operator: both models, the release list, the unavailable rule
PICK = "model.pick"         # managers (and operators): the members' model from the released list
PER_PAGE = 25
LANES = {"public": "Members' turns", "operator": "Operator turns", "choices": "Released to server managers"}
UNAVAILABLE_CHOICES = {
    "profile": ("Use the profile's model", "Members keep getting answers, from the profile's own model."),
    "close": ("Close for members", "Members' messages are not admitted until the model is available again."),
}

# token -> (lane, provider slug): select values and custom_id arguments are too short for both.
TOKENS: BoundedMap = BoundedMap(1024)


def _token(lane: str, provider: str) -> str:
    token = f"{lane[0]}{os.urandom(6).hex()}"
    TOKENS.put(token, (lane, provider))
    return token


def _ref(model: Optional[dict]) -> str:
    if not model:
        return "the profile's model"
    return f"`{clean(model.get('model'), 80, escape=False)}` ({clean(model.get('provider'), 40, escape=False)})"


def _models(ctx: Ctx) -> dict:
    return (ctx.guild or {}).get("models") or {}


def _profile(ctx: Ctx) -> tuple[str, Any]:
    name = apply.effective_profile(routing_view(ctx))
    return name, hermes.profile_dir(name)


def _unavailable_line(ctx: Ctx, lane: str) -> str:
    entry = runtime.UNAVAILABLE.get((ctx.bot_id, ctx.guild_id, lane))
    return f" · ⚠️ unavailable: {clean(entry[1], 120, escape=False)}" if entry else ""


def _k8(ctx: Ctx) -> str:
    result = ctx.binding.runtime.status.get(runtime.INTERVENTION)
    if result is None:
        return "Not checked yet (the check runs when the gateway starts)."
    word = OUTCOME_WORDS.get(result.outcome.value, result.outcome.value)
    return f"{word} ({clean(result.reason, 120, escape=False)})" if ctx.may("status.full") else word


def page(ctx: Ctx) -> Page:
    models = _models(ctx)
    if ctx.may(SETTING):
        profile, _ = _profile(ctx)
        choices = models.get("public_choices") or []
        fields = [("Profile", f"`{clean(profile, 64, escape=False)}` (its own model applies where none is chosen)"),
                  (LANES["public"], _ref(check.effective(ctx.guild, check.PUBLIC)) + _unavailable_line(ctx, "public")
                   + (" · chosen by a server manager" if models.get("manager_pick") and
                      check.effective(ctx.guild, check.PUBLIC) == models.get("manager_pick") else "")),
                  (LANES["operator"], _ref(models.get("operator")) + _unavailable_line(ctx, "operator")),
                  (LANES["choices"], (f"{len(choices)} model{'s' if len(choices) != 1 else ''}: "
                                      + ", ".join(clean(c.get("model"), 40, escape=False) for c in choices[:8])
                                      + (" …" if len(choices) > 8 else "")) if choices else "none"),
                  ("If a model is unavailable", UNAVAILABLE_CHOICES[models.get("on_unavailable", "profile")][0]),
                  ("Status in this Hermes", _k8(ctx)),
                  ("Note", "Members' turns read untrusted text: tool access stays locked by the policy, but a weaker "
                           "model is easier to talk into revealing context.")]
        controls = [Control(SETTING, "mod_pick", "Members' model…", arg="public", style="primary", mutates=True),
                    Control(SETTING, "mod_pick", "Operator model…", arg="operator", mutates=True),
                    Control(SETTING, "mod_pick", "Release to managers…", arg="choices", mutates=True),
                    Control(SETTING, "mod_unav", kind="select", mutates=True, placeholder="If a model is unavailable",
                            options=tuple((label, value, desc[:100], value == models.get("on_unavailable", "profile"))
                                          for value, (label, desc) in UNAVAILABLE_CHOICES.items())),
                    Control("panel.open", "nav", "Back", arg="home")]
        return Page("Models for this server", "Only you can see this. Models run on the operator's credentials.",
                    fields, controls)
    choices = models.get("public_choices") or []
    fields = [("Members' conversations", _ref(check.effective(ctx.guild, check.PUBLIC)))]
    controls = []
    if choices:
        fields.append(("Your choice", f"{len(choices)} model{'s' if len(choices) != 1 else ''} released by the "
                                      "bot's operator"))
        controls.append(Control(PICK, "mod_mgr", "Choose…", style="primary", mutates=True))
    else:
        fields.append(("Your choice", "The bot's operator has not released models for this server."))
    controls.append(Control("panel.open", "nav", "Back", arg="home"))
    return Page("Models for this server", "Which model answers members here.", fields, controls)


def _summary(guild: dict, ctx: Ctx) -> str:
    if ctx.may(SETTING):
        models = guild.get("models") or {}
        return f"members: {_ref(check.effective(guild, check.PUBLIC))} · operator: {_ref(models.get('operator'))}"
    return f"members' conversations: {_ref(check.effective(guild, check.PUBLIC))}"


# ── step 1: provider ────────────────────────────────────────────────────────────────────────────────


async def _catalog(ctx: Ctx) -> tuple[str, Any, catalog.Catalog]:
    await defer_in_place(ctx)
    name, home = _profile(ctx)
    return name, home, await asyncio.to_thread(catalog.load, home)


def _provider_page(lane: str, profile: str, cat: catalog.Catalog, index: int) -> Page:
    pages = max(1, (len(cat.providers) + PER_PAGE - 1) // PER_PAGE)
    index = max(0, min(index, pages - 1))
    chunk = cat.providers[index * PER_PAGE:(index + 1) * PER_PAGE]
    controls = []
    if chunk:
        controls.append(Control(SETTING, "mod_prov", kind="select", mutates=True, arg=lane, placeholder="Choose a provider",
                                options=tuple((p.slug[:100], _token(lane, p.slug),
                                               (f"{p.total} models · {p.state}" if p.state != "no credential"
                                                else f"no credential in this profile")[:100], False)
                                              for p in chunk)))
    if index > 0:
        controls.append(Control(SETTING, "mod_pick", "Previous", arg=f"{lane}.{index - 1}"))
    if index < pages - 1:
        controls.append(Control(SETTING, "mod_pick", "Next", arg=f"{lane}.{index + 1}"))
    if lane != "choices":
        controls.append(Control(SETTING, "mod_clear", "Use the profile's model", arg=lane, mutates=True))
    controls.append(Control(SETTING, "nav", "Back", arg=KEY))
    head = f"{LANES[lane]} — step 1 of 2: provider (profile `{clean(profile, 64, escape=False)}`"
    if cat.profile_model:
        head += f", its model: `{clean(cat.profile_model, 80, escape=False)}`"
    head += ")."
    if cat.error:
        head += f"\n⚠️ Hermes could not list this profile's providers: {clean(cat.error, 160, escape=False)}"
    elif not chunk:
        head += "\nThis profile has no provider configured."
    lines = [f"• `{p.slug}` — no credential in profile `{clean(profile, 64, escape=False)}`. Next step: "
             f"`hermes -p {clean(profile, 64, escape=False)} auth add {p.slug}`, then try again."
             for p in chunk if p.state == "no credential"][:8]
    return Page("Choose a model", head, [("Not usable here", "\n".join(lines))] if lines else [], controls)


@action("mod_pick", SETTING, page=KEY)
async def _pick(ctx: Ctx) -> None:
    lane, _, index = (ctx.ref.arg or "").partition(".")
    if lane not in LANES:
        await ctx.show_page(KEY)
        return
    profile, _, cat = await _catalog(ctx)
    await ctx.show(_provider_page(lane, profile, cat, int(index) if index.isdigit() else 0))


# ── step 2: model ───────────────────────────────────────────────────────────────────────────────────


def _model_page(lane: str, provider: catalog.Provider, profile: str, index: int, models: dict) -> Page:
    ids = [m for m in provider.models if len(provider.slug) + 1 + len(m) <= 100]
    pages = max(1, (len(ids) + PER_PAGE - 1) // PER_PAGE)
    index = max(0, min(index, pages - 1))
    chunk = ids[index * PER_PAGE:(index + 1) * PER_PAGE]
    released = models.get("public_choices") or []
    token = _token(lane, provider.slug)
    controls = []
    if chunk:
        controls.append(Control(SETTING, "mod_set", kind="select", mutates=True, arg=token,
                                placeholder="Choose a model" if lane != "choices" else "Release or withdraw a model",
                                options=tuple((m[:100], f"{provider.slug}|{m}",
                                               ("released · choose to withdraw" if {"provider": provider.slug, "model": m}
                                                in released else "")[:100] if lane == "choices" else "", False)
                                              for m in chunk)))
    if index > 0:
        controls.append(Control(SETTING, "mod_prov_pg", "Previous", arg=f"{token}.{index - 1}"))
    if index < pages - 1:
        controls.append(Control(SETTING, "mod_prov_pg", "Next", arg=f"{token}.{index + 1}"))
    controls += [Control(SETTING, "mod_other", "Other model id…", arg=token, mutates=True),
                 Control(SETTING, "mod_pick", "Back", arg=lane)]
    text = (f"{LANES[lane]} — step 2 of 2: model of `{clean(provider.slug, 64, escape=False)}`"
            + (f" (page {index + 1} of {pages})" if pages > 1 else "") + ".")
    if provider.state == "no credential":
        text += (f"\n⚠️ Profile `{clean(profile, 64, escape=False)}` has no credential for this provider; saving will be "
                 f"refused. Next step: `hermes -p {clean(profile, 64, escape=False)} auth add {provider.slug}`.")
    return Page("Choose a model", text, [], controls)


async def _provider_step(ctx: Ctx, token: str, index: int) -> None:
    entry = TOKENS.get(token)
    if entry is None:
        await ctx.show_page(KEY, notice="That list expired. Next step: choose again.")
        return
    lane, slug = entry
    profile, _, cat = await _catalog(ctx)
    provider = cat.provider(slug)
    if provider is None:
        await ctx.show(_provider_page(lane, profile, cat, 0))
        return
    await ctx.show(_model_page(lane, provider, profile, index, _models(ctx)))


@action("mod_prov", SETTING, mutates=True, page=KEY)
async def _provider(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    await _provider_step(ctx, values[0] if values else "", 0)


@action("mod_prov_pg", SETTING, page=KEY)
async def _provider_page_nav(ctx: Ctx) -> None:
    token, _, index = (ctx.ref.arg or "").partition(".")
    await _provider_step(ctx, token, int(index) if index.isdigit() else 0)


# ── saving ──────────────────────────────────────────────────────────────────────────────────────────


async def _save(ctx: Ctx, lane: str, provider: str, model: str, *, warning: str = "") -> None:
    refused = guard(ctx, SETTING, ops.MODEL_PATHS)
    if refused:
        await ctx.refuse(refused)
        return
    choice = {"provider": provider, "model": model}
    models = _models(ctx)
    if lane == "choices":
        released = list(models.get("public_choices") or [])
        if choice in released:
            released.remove(choice)
            notice = f"✅ Withdrawn: {_ref(choice)}."
        else:
            if len(released) >= MODEL_CHOICES_MAX:
                await ctx.refuse(f"Nothing was changed: at most {MODEL_CHOICES_MAX} models can be released. "
                                 "Next step: withdraw one first.")
                return
            released.append(choice)
            notice = f"✅ Released to server managers: {_ref(choice)}."
        changes = {"models.public_choices": released}
    else:
        changes = {f"models.{lane}": choice}
        notice = f"✅ {LANES[lane]}: {_ref(choice)}."
    adding = lane != "choices" or choice not in (models.get("public_choices") or [])
    verdict = None
    if adding:
        profile, home = _profile(ctx)
        await defer_in_place(ctx)
        verdict = await asyncio.to_thread(check.validate_choice, home, provider, model, profile=profile)
        if not verdict.ok:
            await ctx.refuse(f"Nothing was saved: {verdict.reason}. Next step: {verdict.next_step}.")
            return
    done, _ = await run_change(ctx, lambda: ops.set_models(ctx.binding.store, ctx.bot_id, ctx.guild_id, changes,
                                                           by=ctx.actor.user_id, via="discord",
                                                           expected_rev=ctx.ref.rev), page=KEY)
    if done:
        for text in (warning, verdict.reason if verdict is not None else ""):
            if text:
                notice += f"\n⚠️ {clean(text, 240, escape=False)}"
        await ctx.show_page(KEY, notice=notice)


@action("mod_set", SETTING, mutates=True, page=KEY)
async def _set(ctx: Ctx) -> None:
    entry = TOKENS.get(ctx.ref.arg or "")
    values = list(getattr(ctx.item, "values", None) or [])
    if entry is None or not values or "|" not in values[0]:
        await ctx.show_page(KEY, notice="That list expired. Nothing was saved. Next step: choose again.")
        return
    lane, slug = entry
    provider, _, model = values[0].partition("|")
    if provider != slug:
        await ctx.refuse("Nothing was saved: the choice does not belong to this list. Next step: choose again.")
        return
    await _save(ctx, lane, provider, model)


def _other_modal(lane: str, provider: str) -> modals.ModalSpec:
    return modals.ModalSpec(f"m_mod_{lane[:2]}", "Other model id", [
        modals.Field("provider", "text", "Provider", description="A provider id of this profile.", value=provider,
                     max_length=64),
        modals.Field("model", "text", "Model id", description="Exactly as the provider names it.", max_length=200)])


@action("mod_other", SETTING, mutates=True, page=KEY)
async def _other(ctx: Ctx) -> None:
    entry = TOKENS.get(ctx.ref.arg or "")
    if entry is None:
        await ctx.show_page(KEY, notice="That list expired. Next step: choose again.")
        return
    await ctx.open_modal(_other_modal(*entry))


async def _catalog_warning(ctx: Ctx, provider: str, model: str) -> str:
    """A typed model id is checked against the provider's list Hermes knows in this profile (B4): known ids
    pass silently, anything else is saved as typed with a warning (catalogs lag behind providers)."""
    profile, _, cat = await _catalog(ctx)
    known = cat.provider(provider)
    if known is not None and model in known.models:
        return ""
    if known is None or not known.models:
        return (f"Hermes knows no model list for {provider} in profile {profile}; the id was saved as typed. If the "
                "provider does not serve it, the profile's fallback applies.")
    return (f"`{model}` is not in the model list Hermes knows for {provider}; the id was saved as typed. If the "
            "provider does not serve it, the profile's fallback applies.")


def _other_handler(lane: str):
    async def handler(ctx: Ctx, values: dict) -> None:
        provider = str(values.get("provider") or "").strip().lower()
        model = str(values.get("model") or "").strip()
        if not provider or not model:
            await ctx.refuse("Nothing was saved: provider and model id are both needed.")
            return
        await _save(ctx, lane, provider, model, warning=await _catalog_warning(ctx, provider, model))
    return handler


for _lane in LANES:
    modal_action(f"m_mod_{_lane[:2]}", SETTING, page=KEY)(_other_handler(_lane))


@action("mod_clear", SETTING, mutates=True, page=KEY)
async def _clear(ctx: Ctx) -> None:
    lane = ctx.ref.arg
    if lane not in ("public", "operator"):
        await ctx.show_page(KEY)
        return
    refused = guard(ctx, SETTING, ops.MODEL_PATHS)
    if refused:
        await ctx.refuse(refused)
        return
    done, _ = await run_change(ctx, lambda: ops.set_models(ctx.binding.store, ctx.bot_id, ctx.guild_id,
                                                           {f"models.{lane}": None}, by=ctx.actor.user_id,
                                                           via="discord", expected_rev=ctx.ref.rev), page=KEY)
    if done:
        await ctx.show_page(KEY, notice=f"✅ {LANES[lane]}: the profile's model.")


@action("mod_unav", SETTING, mutates=True, page=KEY)
async def _unavailable(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    value = values[0] if values else None
    if value not in ON_UNAVAILABLE:
        await ctx.refuse("That choice is not available. Nothing was saved.")
        return
    refused = guard(ctx, SETTING, ops.MODEL_PATHS)
    if refused:
        await ctx.refuse(refused)
        return
    done, _ = await run_change(ctx, lambda: ops.set_models(ctx.binding.store, ctx.bot_id, ctx.guild_id,
                                                           {"models.on_unavailable": value}, by=ctx.actor.user_id,
                                                           via="discord", expected_rev=ctx.ref.rev), page=KEY)
    if done:
        await ctx.show_page(KEY, notice=f"✅ If a model is unavailable: {UNAVAILABLE_CHOICES[value][0]}.")


# ── managers: choose from the released list ─────────────────────────────────────────────────────────


@action("mod_mgr", PICK, mutates=True, page=KEY)
async def _manager(ctx: Ctx) -> None:
    models = _models(ctx)
    released = models.get("public_choices") or []
    if not released:
        await ctx.show_page(KEY, notice="The bot's operator has not released models for this server.")
        return
    pick = models.get("manager_pick")
    pick = pick if pick in released else None          # exactly one option is preselected
    options = [(clean(c["model"], 100, escape=False), f"{c['provider']}|{c['model']}", c["provider"][:100], c == pick)
               for c in released[:PER_PAGE] if len(c["provider"]) + 1 + len(c["model"]) <= 100]
    options.append(("The operator's choice", "-", "Use the model the operator set for members", pick is None))
    await ctx.show(Page("Members' model", "Choose among the models the bot's operator released for this server.", [], [
        Control(PICK, "mod_mgr_set", kind="select", mutates=True, placeholder="Members' model", options=tuple(options)),
        Control("panel.open", "nav", "Back", arg=KEY)]))


@action("mod_mgr_set", PICK, mutates=True, page=KEY)
async def _manager_set(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    raw = values[0] if values else ""
    pick = None
    if raw != "-":
        provider, _, model = raw.partition("|")
        pick = {"provider": provider, "model": model}
    refused = guard(ctx, PICK, (ops.MANAGER_PICK_PATH,))
    if refused:
        await ctx.refuse(refused)
        return
    done, _ = await run_change(ctx, lambda: ops.set_manager_pick(ctx.binding.store, ctx.bot_id, ctx.guild_id, pick,
                                                                 by=ctx.actor.user_id, via="discord",
                                                                 expected_rev=ctx.ref.rev), page=KEY)
    if done:
        await ctx.show_page(KEY, notice=f"✅ Members' model: {_ref(check.effective(ctx.guild, check.PUBLIC))}.")


register_panel(Panel(KEY, "Models", PICK, 55, page, summary=_summary))  # after Progress
