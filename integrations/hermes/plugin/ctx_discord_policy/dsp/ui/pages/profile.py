"""Panel "Profile": the Hermes profile this server runs in (operator only; PROFILE-PER-GUILD §6).

The overview is cheap (stored choice, running route table, cached configuration reads). Listing the
installed profiles takes a few hundred milliseconds, so it happens only after "Choose profile…", off the
event loop, with the answer deferred first. Every change goes through :mod:`...routing.apply` (fresh
eligibility check, Hermes configuration, running table, store) — never through ``Ctx.write``, which would
skip those checks (the store refuses the routing fields there anyway).
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import os
from typing import Any, Awaitable, Callable, Optional

from ... import hermes
from ...routing import apply, eligibility
from ...runtime import BoundedMap
from ...store.file import ConflictError, StoreError
from ...store.ops import ROUTING_PATHS
from ...store.schema import SchemaError
from .. import perms
from ..panels import Control, Ctx, Page, Panel, action, clean, register_panel, when, who

logger = logging.getLogger("ctx_discord_policy")

KEY = "profile"
SETTING = "profile"
PER_PAGE = 25
DEFER_AFTER = 1.5

# Short, process-local tokens for values that do not fit a custom_id argument (32 characters).
TOKENS: BoundedMap = BoundedMap(1024)


def token_for(value: str) -> str:
    if len(value) <= 32 and all(c.isalnum() or c in "_-" for c in value):
        return value
    token = "t." + hashlib.sha256(os.urandom(8) + value.encode()).hexdigest()[:20]
    TOKENS.put(token, value)
    return token


def from_token(arg: str) -> Optional[str]:
    return TOKENS.get(arg) if arg.startswith("t.") else (arg or None)


# ── shared helpers (also used by the Models panel) ───────────────────────────────────────────────────


async def defer_in_place(ctx: Ctx) -> None:
    """Acknowledge a click whose answer needs slow work (listing profiles or catalogs)."""
    response = ctx.interaction.response
    if not response.is_done():
        await response.defer()
        ctx.deferred_update = True


async def run_change(ctx: Ctx, fn: Callable[[], Any], *, page: str) -> tuple[bool, Any]:
    """Run a blocking change off the loop (deferring when it is slow) and answer every failure.

    Returns ``(done, result)``; on ``False`` the caller has nothing more to say."""
    task = asyncio.ensure_future(asyncio.to_thread(fn))
    try:
        done, _ = await asyncio.wait({task}, timeout=DEFER_AFTER)
        if not done:
            await defer_in_place(ctx)
        result = await task
    except ConflictError:
        await ctx.conflict(page)
        return False, None
    except (apply.Refused, SchemaError, perms.Refused) as exc:
        await ctx.refuse(f"Nothing was changed: {exc}")
        return False, None
    except (StoreError, OSError, hermes.HermesUnavailable) as exc:
        logger.warning("ctx_discord_policy: change refused for guild %s: %s", ctx.guild_id, exc)
        await ctx.refuse("Nothing was changed: the host could not write the change right now. Next step: try "
                         "again in a moment; if it keeps happening, check `hermes ctx-discord-policy status` on the host.")
        return False, None
    except LookupError as exc:
        await ctx.refuse(f"Nothing was changed: {exc}. Next step: open the panel again.")
        return False, None
    ctx.refresh()
    return True, result


def guard(ctx: Ctx, setting: str, paths: tuple) -> Optional[str]:
    """The checks ``Ctx.write`` makes, for writes that bypass it: rights for exactly these paths, and an
    active (or paused) server."""
    try:
        perms.check_write(ctx.actor, setting, paths)
    except perms.Refused as exc:
        return str(exc)
    if ctx.state == "unbound":
        return ("Nothing was changed: the policy is not active in this server. Next step: activate it first.")
    return None


def view(ctx: Ctx) -> apply.View:
    return apply.describe(ctx.binding.store, ctx.bot_id, ctx.guild_id, owner_home=ctx.binding.owner_home,
                          adapter=getattr(ctx.binding, "adapter", None))


# ── the page ─────────────────────────────────────────────────────────────────────────────────────────


def _runs_in(v: apply.View) -> str:
    if v.desired is None:
        if v.state == "core" and v.core_entry:
            return (f"`{clean(v.core_entry.get('profile'), 64, escape=False)}` — routed by Hermes' configuration "
                    f"(entry `{clean(v.core_entry.get('name'), 80, escape=False)}`). Take it over to manage it here; "
                    "nothing changes for the server.")
        if v.state == "pending":
            return ("the bot's own profile — ⚠️ but the running Hermes still routes this server to "
                    f"`{clean(v.contradicts or '?', 64, escape=False)}` through the plugin's entry; members' messages "
                    "stay closed until that is resolved. Next step: **Apply again** (use the bot's own profile) or "
                    f"**Use {clean(v.contradicts or '?', 64, escape=False)}**.")
        return f"the bot's own profile (`{clean(v.owner_profile, 64, escape=False)}`)"
    name = f"`{clean(v.desired, 64, escape=False)}` — chosen here"
    if v.state == "live":
        where = "live only (Hermes' configuration is managed; it does not keep the choice)" if v.managed \
            else "saved in Hermes' configuration · live"
        return f"{name}\n{where}"
    if v.contradicts:
        return (f"{name}\n⚠️ Hermes' configuration routes this server to `{clean(v.contradicts, 64, escape=False)}` "
                "(the plugin's entry was changed outside /policy). Members' messages stay closed. Next step: "
                f"**Use {clean(v.contradicts, 64, escape=False)}** or **Apply again**.")
    if v.state == "conflict":
        return (f"{name}\n⚠️ Hermes' configuration routes this server to "
                f"`{clean((v.core_entry or {}).get('profile'), 64, escape=False)}` (entry "
                f"`{clean((v.core_entry or {}).get('name'), 80, escape=False)}`). Members' messages stay closed. "
                "Next step: **Take over** (use Hermes' choice) or remove that entry on the host.")
    return (f"{name}\n⚠️ Saved, but the running Hermes still routes this server elsewhere. Members' messages here "
            "stay closed until Hermes applies it. Next step: **Apply again**, or restart Hermes.")


def _protection(v: apply.View) -> str:
    if v.desired:
        profile = v.desired
    elif v.state == "core" and v.core_entry:
        profile = v.core_entry.get("profile")
    else:
        profile = v.owner_profile
    if v.locked:
        return (f"⚠️ `{clean(profile, 64, escape=False)}` is locked (settings.locked_profiles): members' messages "
                "are never admitted there. Next step: choose another profile.")
    if not v.armed:
        return (f"⚠️ ctx_discord_policy is not enabled in `{clean(profile, 64, escape=False)}`: members' "
                f"messages are closed. Next step: `hermes -p {clean(profile, 64, escape=False)} plugins enable "
                "ctx_discord_policy`, then restart Hermes.")
    return f"✓ policy armed in `{clean(profile, 64, escape=False)}` · not locked"


def page(ctx: Ctx) -> Page:
    v = view(ctx)
    fields = [("Runs in", _runs_in(v)), ("Protection", _protection(v))]
    if v.exceptions:
        fields.append(("Exceptions (Hermes configuration)", "\n".join(
            f"`{clean(n, 60, escape=False)}` → `{clean(p, 64, escape=False)}` " + ("✓ armed" if armed else "⚠️ not armed")
            for n, p, armed in v.exceptions[:10])))
    if v.desired and v.set_at:
        fields.append(("Chosen", f"by {who(v.set_by)} {when(v.set_at)}"))
    fields.append(("Switching", "New messages start new conversations in the new profile: its tools, memory, "
                                "credentials and model apply. Nothing moves from the old profile; the server's "
                                "model choices are reset."))
    controls = []
    if v.unavailable:
        fields.insert(0, ("Not available", f"Profile selection is not available: {v.unavailable}."))
    else:
        controls.append(Control(SETTING, "prof_pick", "Choose profile…", arg="0", style="primary", mutates=True))
        if v.desired:
            controls.append(Control(SETTING, "prof_own", "Use the bot's own profile", mutates=True))
        if v.core_entry and (v.desired is None or v.state == "conflict"):
            controls.append(Control(SETTING, "prof_adopt", "Take over", style="primary", mutates=True))
        if v.state == "pending":
            controls.append(Control(SETTING, "prof_apply", "Apply again", style="success", mutates=True))
        if v.contradicts:
            controls.append(Control(SETTING, "prof_use", f"Use {v.contradicts}"[:80], mutates=True,
                                    arg=token_for(v.contradicts)))
        controls.append(Control(SETTING, "prof_why", "Why not others?"))
        if v.managed and v.desired:
            controls.append(Control(SETTING, "prof_yaml", "Route as YAML"))
    controls.append(Control("panel.open", "nav", "Back", arg="home"))
    tone = "error" if (v.locked or not v.armed or v.state in ("pending", "conflict")) else "neutral"
    return Page("Hermes profile for this server", "Only you can see this. The profile decides the tools, credentials "
                "and memory every turn in this server can reach.", fields, controls, tone=tone)


def _summary(guild: dict, ctx: Ctx) -> str:
    routing = guild.get("routing") or {}
    chosen = routing.get("profile")
    if not chosen:
        return "the bot's own profile"
    text = f"`{clean(chosen, 64, escape=False)}`"
    if routing.get("set_at"):
        text += f" · set by {who(routing.get('set_by'))} {when(routing.get('set_at'))}"
    return text


# ── choosing ─────────────────────────────────────────────────────────────────────────────────────────


def _runner(ctx: Ctx) -> Any:
    return apply.runner_of(getattr(ctx.binding, "adapter", None))


async def _candidates(ctx: Ctx) -> tuple[list, Optional[str]]:
    await defer_in_place(ctx)
    try:
        return await asyncio.to_thread(eligibility.candidates, _runner(ctx), ctx.binding.owner_home)
    except Exception as exc:
        logger.warning("ctx_discord_policy: listing profiles failed: %s", exc)
        return [], f"the profiles could not be listed ({type(exc).__name__})"


def pick_page(ctx: Ctx, found: list, index: int, current: Optional[str]) -> Page:
    eligible = [c for c in found if c.eligible]
    pages = max(1, (len(eligible) + PER_PAGE - 1) // PER_PAGE)
    index = max(0, min(index, pages - 1))
    chunk = eligible[index * PER_PAGE:(index + 1) * PER_PAGE]
    controls = []
    if chunk:
        controls.append(Control(SETTING, "prof_sel", kind="select", mutates=True, placeholder="Choose a profile",
                                options=tuple((c.label[:100], c.name, (f"model {c.model} · armed" if c.model
                                                                       else "armed")[:100], c.name == current)
                                              for c in chunk)))
    if index > 0:
        controls.append(Control(SETTING, "prof_pick", "Previous", arg=str(index - 1)))
    if index < pages - 1:
        controls.append(Control(SETTING, "prof_pick", "Next", arg=str(index + 1)))
    controls += [Control(SETTING, "prof_why", "Why not others?"), Control(SETTING, "nav", "Back", arg=KEY)]
    text = (f"{len(eligible)} profile{'s' if len(eligible) != 1 else ''} can be chosen"
            + (f" (page {index + 1} of {pages})" if pages > 1 else "") + ".")
    if not chunk:
        text = "No profile can be chosen for this server right now. **Why not others?** lists the reasons."
    return Page("Choose a Hermes profile", text, [], controls)


@action("prof_pick", SETTING, page=KEY)
async def _pick(ctx: Ctx) -> None:
    found, why = await _candidates(ctx)
    if why:
        await ctx.show_page(KEY, notice=f"Profile selection is not available: {why}.")
        return
    try:
        index = int(ctx.ref.arg or 0)
    except ValueError:
        index = 0
    current = ((ctx.guild or {}).get("routing") or {}).get("profile")
    await ctx.show(pick_page(ctx, found, index, current))


@action("prof_why", SETTING, page=KEY)
async def _why(ctx: Ctx) -> None:
    found, why = await _candidates(ctx)
    if why:
        await ctx.say(f"Profile selection is not available: {why}.")
        return
    lines = [f"• `{clean(c.name, 64, escape=False)}` — {c.reason}. Next step: {c.next_step}."
             for c in found if not c.eligible]
    text = "Not available for this server:\n" + "\n".join(lines) if lines else "Every installed profile can be chosen."
    if len(text) > 1900:
        text = text[:1880] + "\n…"
    await ctx.say(text)


def confirm_page(ctx: Ctx, name: Optional[str]) -> Page:
    guild = ctx.guild or {}
    current = (guild.get("routing") or {}).get("profile") or "the bot's own profile"
    target = f"`{clean(name, 64, escape=False)}`" if name else "the bot's own profile"
    models = guild.get("models") or {}
    reset = [f"{lane}: {clean(m.get('model'), 60, escape=False)}" for lane, m in
             (("members", models.get("public")), ("operator", models.get("operator"))) if m]
    released = len(models.get("public_choices") or [])
    if released:
        reset.append(f"{released} model{'s' if released != 1 else ''} released to server managers")
    if models.get("manager_pick"):
        reset.append("a server manager's pick")
    lines = [f"• New messages here run in {target}: its tools, memory, credentials and model.",
             f"• Conversations so far stay in `{clean(current, 64, escape=False)}`. Nothing moves.",
             "• Members' messages waiting in the queue are dropped; turns already running finish where they started.",
             "• Saved in Hermes' configuration (profile_routes)."]
    if reset:
        lines.append("• The server's model choices are reset (" + "; ".join(reset) + ").")
    controls = [Control(SETTING, "prof_ok" if name else "prof_own_ok", "Switch", style="danger", mutates=True,
                        arg=token_for(name) if name else ""),
                Control(SETTING, "nav", "Cancel", arg=KEY)]
    return Page(f"Switch this server to {name or 'the bot’s own profile'}?"[:256], "\n".join(lines), [], controls,
                tone="paused")


@action("prof_sel", SETTING, mutates=True, page=KEY)
async def _select(ctx: Ctx) -> None:
    values = list(getattr(ctx.item, "values", None) or [])
    if not values:
        await ctx.show_page(KEY)
        return
    await ctx.show(confirm_page(ctx, str(values[0])))


@action("prof_own", SETTING, mutates=True, page=KEY)
async def _own(ctx: Ctx) -> None:
    await ctx.show(confirm_page(ctx, None))


@action("prof_use", SETTING, mutates=True, page=KEY)
async def _use(ctx: Ctx) -> None:
    """Keep what Hermes' configuration routes this server to (the choice is checked again on confirm)."""
    name = from_token(ctx.ref.arg)
    if not name:
        await ctx.show_page(KEY, notice="That choice expired. Next step: open the panel again.")
        return
    await ctx.show(confirm_page(ctx, name))


async def _switch(ctx: Ctx, name: Optional[str]) -> None:
    refused = guard(ctx, SETTING, ROUTING_PATHS)
    if refused:
        await ctx.refuse(refused)
        return
    done, result = await run_change(ctx, lambda: apply.set_profile(
        ctx.binding.store, ctx.bot_id, ctx.guild_id, name, owner_home=ctx.binding.owner_home, by=ctx.actor.user_id,
        via="discord", expected_rev=ctx.ref.rev, adapter=getattr(ctx.binding, "adapter", None)), page=KEY)
    if done:
        await ctx.show_page(KEY, notice=_outcome_notice(result))


def _outcome_notice(result: apply.Applied) -> str:
    target = f"`{clean(result.profile, 64, escape=False)}`" if result.profile else "the bot's own profile"
    if result.live and result.saved:
        text = f"✅ This server now runs in {target}."
    elif result.live:
        text = f"✅ This server now runs in {target} (live only: {clean(result.reason, 200, escape=False)})."
    else:
        text = (f"⚠️ Saved: {target}. The running Hermes does not route it there yet "
                f"({clean(result.reason, 200, escape=False)}); members' messages here stay closed until it does. "
                "Next step: **Apply again**, or restart Hermes.")
    if result.warning:
        text += f"\n⚠️ {clean(result.warning, 300, escape=False)}"
    return text


@action("prof_ok", SETTING, mutates=True, page=KEY)
async def _ok(ctx: Ctx) -> None:
    name = from_token(ctx.ref.arg)
    if not name:
        await ctx.refuse("This confirmation expired. Nothing was changed. Next step: choose the profile again.")
        return
    await _switch(ctx, name)


@action("prof_own_ok", SETTING, mutates=True, page=KEY)
async def _own_ok(ctx: Ctx) -> None:
    await _switch(ctx, None)


@action("prof_adopt", SETTING, mutates=True, page=KEY)
async def _adopt(ctx: Ctx) -> None:
    v = view(ctx)
    if not v.core_entry:
        await ctx.show_page(KEY, notice="There is no hand-written route of this server to take over.")
        return
    name = clean(v.core_entry.get("name"), 80, escape=False)
    await ctx.show(Page("Take over Hermes' route of this server?", (
        f"• The entry `{name}` in Hermes' configuration is renamed so this panel manages it.\n"
        f"• The server keeps running in `{clean(v.core_entry.get('profile'), 64, escape=False)}`; nothing changes "
        "for its members or conversations."), [], [
        Control(SETTING, "prof_adopt_ok", "Take over", style="primary", mutates=True, arg=token_for(name)),
        Control(SETTING, "nav", "Cancel", arg=KEY)]))


@action("prof_adopt_ok", SETTING, mutates=True, page=KEY)
async def _adopt_ok(ctx: Ctx) -> None:
    core = from_token(ctx.ref.arg)
    refused = guard(ctx, SETTING, ROUTING_PATHS)
    if refused or not core:
        await ctx.refuse(refused or "This confirmation expired. Nothing was changed. Next step: open the panel again.")
        return
    done, result = await run_change(ctx, lambda: apply.adopt(
        ctx.binding.store, ctx.bot_id, ctx.guild_id, core, owner_home=ctx.binding.owner_home, by=ctx.actor.user_id,
        via="discord", expected_rev=ctx.ref.rev, adapter=getattr(ctx.binding, "adapter", None)), page=KEY)
    if done:
        await ctx.show_page(KEY, notice=_outcome_notice(result))


@action("prof_apply", SETTING, mutates=True, page=KEY)
async def _apply(ctx: Ctx) -> None:
    refused = guard(ctx, SETTING, ROUTING_PATHS)
    if refused:
        await ctx.refuse(refused)
        return
    done, result = await run_change(ctx, lambda: apply.reapply(
        ctx.binding.store, ctx.bot_id, ctx.guild_id, owner_home=ctx.binding.owner_home,
        adapter=getattr(ctx.binding, "adapter", None), by=ctx.actor.user_id, via="discord"), page=KEY)
    if done:
        await ctx.show_page(KEY, notice=_outcome_notice(result))


@action("prof_yaml", SETTING, page=KEY)
async def _yaml(ctx: Ctx) -> None:
    from ...routing import table

    chosen = ((ctx.guild or {}).get("routing") or {}).get("profile")
    if not chosen:
        await ctx.say("This server has no chosen profile, so there is no route to add.")
        return
    owner = eligibility.owner_profile_name(ctx.binding.owner_home)
    snippet = table.snippet_yaml(table.make_entry(ctx.bot_id, ctx.guild_id, chosen, owner))
    await ctx.say("Hermes' configuration is managed, so it does not keep this choice. Add this entry to your managed "
                  f"configuration:\n```yaml\n{snippet}```")


register_panel(Panel(KEY, "Profile", SETTING, 85, page, summary=_summary))  # after the locked panels
