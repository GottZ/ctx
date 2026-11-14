"""Pages of ``/policy``: the page model, the panel registry, the start/status/confirmation pages, the
lifecycle actions (activate, request, pause/resume, unbind) and the dispatcher.

Settings panels live in :mod:`.pages`, one module each; :mod:`.pages.progress` is the complete example.
How a panel is added (the pattern S9 follows):

1. a page builder ``def my_page(ctx: Ctx) -> Page`` that lists fields and :class:`Control` s; every control
   names the :mod:`.perms` setting that governs it, so a caller only sees what :data:`perms.TABLE` allows;
2. ``register_panel(Panel(key, title, setting, order, my_page, summary=…))`` — adds the navigation button
   and the summary row on the start page (only for callers who may use ``setting``) and the
   ``/policy page:`` choice;
3. one ``@action(name, setting, mutates=True, page=key)`` handler per control and, for forms,
   :meth:`Ctx.open_modal` plus one ``@modal_action(name, setting, page=key)`` handler; handlers write
   through :meth:`Ctx.write`, which re-checks the caller's rights for exactly the written schema paths, the
   server's state and the panel's ``rev``;
4. import the module in :mod:`.pages` (``pages/__init__.py``).

The dispatcher (:func:`handle_component`, :func:`handle_modal`) re-resolves the caller's role on every
interaction; the server always comes from the interaction, never from the ``custom_id``.
"""

from __future__ import annotations

import asyncio
import io
import logging
import re
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Awaitable, Callable, Optional

from .. import config
from ..policy.routes import snowflake
from ..store import ops
from ..store.file import CORRUPT, DOWNGRADE, ConflictError, Snapshot, StoreError
from ..store.schema import HARD_PROMPT_LIMIT, SchemaError
from . import items, modals, perms

logger = logging.getLogger("ctx_discord_policy")

VIEW_TIMEOUT = 840.0           # non-persistent fallback: just under Discord's 15-minute interaction token
DEFER_AFTER = 1.5              # seconds a write may take before the answer is deferred (Discord allows 3)
_CONTROL = re.compile(r"[\x00-\x1f\x7f\x85\u2028\u2029]+")
_TONES = {"active": 0x2ECC71, "paused": 0xF1C40F, "inactive": 0x95A5A6, "error": 0xE74C3C, "neutral": 0x5865F2}
_STYLES = ("primary", "secondary", "success", "danger")


# ── page model ───────────────────────────────────────────────────────────────────────────────────────


@dataclass
class Control:
    setting: str                   # perms key: shown only to callers who may use it, and checked on click
    action: str
    label: str = ""
    arg: str = ""
    kind: str = "button"           # "button" | "select" | "channel_select" | "role_select"
    style: str = "secondary"
    emoji: Optional[Any] = None
    options: tuple = ()            # select: (label, value, description, default[, emoji]) tuples
    placeholder: Optional[str] = None
    mutates: bool = False          # hidden while the store is read-only (damaged or newer schema)
    min_values: int = 1
    max_values: int = 1
    defaults: tuple = ()           # channel/role select: preselected ids
    channel_types: tuple = ("text", "news", "forum")
    disabled: bool = False
    row: Optional[int] = None      # 0-4; None = Discord's automatic layout


@dataclass
class Page:
    title: str
    description: str = ""
    fields: list = field(default_factory=list)     # (name, value) pairs
    controls: list = field(default_factory=list)
    tone: str = "neutral"
    footer: str = ""


@dataclass
class Panel:
    key: str                       # navigation argument and /policy page value
    title: str                     # button label and page choice name
    setting: str                   # perms key that governs seeing and changing this panel
    order: int
    build: Callable[["Ctx"], Page]
    emoji: Optional[str] = None
    summary: Optional[Callable[[dict, "Ctx"], str]] = None   # one line on the start page


@dataclass(frozen=True)
class Action:
    name: str
    setting: Any                   # perms key, or callable(ref) -> perms key
    handler: Callable[["Ctx"], Awaitable[None]]
    mutates: bool = False
    page: str = "home"             # page shown again after a conflict


@dataclass(frozen=True)
class ModalAction:
    name: str
    setting: str
    handler: Callable[["Ctx", dict], Awaitable[None]]
    page: str = "home"


PANELS: dict = {}
ACTIONS: dict = {}
MODAL_ACTIONS: dict = {}


def register_panel(panel: Panel) -> Panel:
    if panel.setting not in perms.SETTINGS:
        raise ValueError(f"panel {panel.key}: unknown setting {panel.setting}")
    PANELS[panel.key] = panel
    return panel


def registered_panels() -> list:
    return sorted(PANELS.values(), key=lambda p: (p.order, p.key))


def action(name: str, setting: Any, *, mutates: bool = False, page: str = "home"):
    def decorate(fn):
        ACTIONS[name] = Action(name, setting, fn, mutates, page)
        return fn
    return decorate


def modal_action(name: str, setting: str, *, page: str = "home"):
    def decorate(fn):
        MODAL_ACTIONS[name] = ModalAction(name, setting, fn, page)
        return fn
    return decorate


# ── interaction context ──────────────────────────────────────────────────────────────────────────────


def clean(value: Any, limit: int = 100, *, escape: bool = True) -> str:
    """Text from Discord or from server managers, made safe for one line: no control characters or line
    breaks, bounded length and (``escape``) no markdown, mentions or emoji syntax."""
    text = _CONTROL.sub(" ", str(value or "")).strip()
    text = re.sub(r"\s{2,}", " ", text)
    if len(text) > limit:
        text = text[: limit - 1] + "…"
    if escape:
        for ch in "\\*_~`|>[]()#@:<":
            text = text.replace(ch, "\\" + ch)
    return text


def when(iso: Any) -> str:
    try:
        stamp = datetime.fromisoformat(str(iso).replace("Z", "+00:00"))
    except ValueError:
        return ""
    return f"<t:{int(stamp.timestamp())}:R>"


def who(by: Any) -> str:
    text = str(by or "")
    return f"<@{text}>" if snowflake(text) else ("the host command line" if text == "cli" else "someone")


@dataclass
class Ctx:
    binding: Any
    interaction: Any
    guild_id: str
    actor: perms.Actor
    snapshot: Snapshot
    ref: Optional[items.Ref] = None
    item: Any = None
    origin_view: Any = None        # non-persistent strategy: the view this interaction came from
    deferred_update: bool = False  # a slow write deferred the in-place update
    page_arg: str = ""             # page-local state for the next render (e.g. a list page number)

    @property
    def discord(self) -> Any:
        return self.binding.discord

    @property
    def bot_id(self) -> str:
        return self.binding.bot_id

    @property
    def guild(self) -> Optional[dict]:
        return self.snapshot.guild(self.bot_id, self.guild_id)

    @property
    def state(self) -> str:
        return self.snapshot.guild_state(self.bot_id, self.guild_id)

    @property
    def rev(self) -> int:
        return self.snapshot.guild_rev(self.bot_id, self.guild_id)  # never repeats across a purge

    @property
    def guild_name(self) -> str:
        """Server name for embed titles (titles render no mentions; cleaned, not escaped)."""
        guild = getattr(self.interaction, "guild", None)
        name = getattr(guild, "name", None)
        return clean(name, 80, escape=False) if name else "this server"

    def may(self, key: str) -> bool:
        return perms.may(self.actor, key)

    def settings(self) -> dict:
        """The operator's global settings (DESIGN §5.1) as the prompt engine reads them."""
        home = self.binding.owner_home
        return {key: config.setting(home, key) for key in ("frame_prompt", "operator_label",
                                                           "guild_prompt_max_chars", "notify_operators")}

    def refresh(self) -> None:
        """Re-read the store and the caller's role (after a write or a conflict)."""
        self.snapshot = self.binding.store.read()
        self.actor = perms.from_interaction(self.interaction, owner_home=self.binding.owner_home,
                                            guild_config=self.guild)

    # ── responding ──

    async def say(self, text: str) -> None:
        """A short ephemeral answer in a new message (refusals, results); the panel stays as it is."""
        response = self.interaction.response
        if response.is_done():
            await self.interaction.followup.send(text, ephemeral=True)
        else:
            await response.send_message(text, ephemeral=True)

    refuse = say

    def _in_place(self) -> bool:
        return (getattr(self.interaction, "message", None) is not None
                and _interaction_kind(self.interaction) in ("component", "modal_submit"))

    def _retire_origin_view(self) -> None:
        # Non-persistent strategy only: the replaced view must stop now, or its later timeout removes the
        # (type, custom_id) keys it shares with the new panel on the same message (discord.py ViewStore).
        old = self.origin_view or getattr(self.item, "view", None)
        if not self.binding.persistent and old is not None and hasattr(old, "stop"):
            old.stop()

    async def show(self, page: Page, *, notice: str = "", new_message: bool = False) -> None:
        payload = render(self, page, notice=notice)
        response = self.interaction.response
        in_place = not new_message and self._in_place()
        if in_place:
            self._retire_origin_view()
        if in_place and self.deferred_update:
            await self.interaction.edit_original_response(**payload)
        elif response.is_done():
            await self.interaction.followup.send(ephemeral=True, **payload)
        elif in_place:
            await response.edit_message(**payload)
        else:
            await response.send_message(ephemeral=True, **payload)

    async def open_modal(self, spec: Any) -> None:
        """Open a form as the answer to this click (a modal must be the first response)."""
        modal = modals.build(self.discord, spec, guild_id=self.guild_id, rev=self.rev, opener_id=self.actor.user_id,
                             modern=self.binding.modern)
        modal.origin_view = getattr(self.item, "view", None)
        await self.interaction.response.send_modal(modal)

    async def acknowledge(self) -> None:
        """Answer now, update later (slow work: store lock, downloads): edits in place where possible."""
        if self.interaction.response.is_done():
            return
        if self._in_place():
            await self.interaction.response.defer()
            self.deferred_update = True
        else:
            await self.interaction.response.defer(ephemeral=True, thinking=True)

    async def send_file(self, text: str, name: str, data: bytes) -> None:
        """A new ephemeral message with one attached file (previews, downloads)."""
        file = self.discord.File(io.BytesIO(data), filename=name)
        if self.interaction.response.is_done():
            await self.interaction.followup.send(text, file=file, ephemeral=True)
        else:
            await self.interaction.response.send_message(text, file=file, ephemeral=True)

    async def show_page(self, key: str, *, notice: str = "") -> None:
        await self.show(build_page(self, key), notice=notice)

    # ── writing ──

    async def _guard(self, setting: str, paths: Any) -> bool:
        try:
            perms.check_write(self.actor, setting, paths)
        except perms.Refused as exc:
            await self.refuse(str(exc))
            return False
        if self.state == "unbound":
            await self.refuse("Nothing was saved: the policy is not active in this server, so its settings can't "
                              "be changed. Next step: ask the bot's operator to activate it.")
            return False
        return True

    async def write(self, setting: str, changes: dict, *, expected_rev: int, page: str) -> bool:
        """Write settings through the one guarded path; answers refusals and conflicts itself."""
        if not await self._guard(setting, changes):
            return False
        # The operator's guidance cap binds guidance writes; a guidance that predates a lowered cap must not
        # block every other setting of the server (the runtime shortens it to the cap).
        cap = self.binding.prompt_cap() if "prompts.guild" in changes else HARD_PROMPT_LIMIT
        return await self._commit(lambda: ops.update_fields(self.binding.store, self.bot_id, self.guild_id, changes,
                                                            by=self.actor.user_id, expected_rev=expected_rev,
                                                            prompt_cap=cap),
                                  page, setting=setting, paths=list(changes))

    async def reset(self, setting: str, paths: list, *, expected_rev: int, page: str) -> bool:
        """Put ``paths`` back to their defaults (``ops.reset_fields``), with the same guards as :meth:`write`."""
        written = list(paths) + (["prompts.history"] if "prompts.guild" in paths else [])
        if not await self._guard(setting, written):
            return False
        cap = self.binding.prompt_cap() if "prompts.guild" in paths else HARD_PROMPT_LIMIT
        return await self._commit(lambda: ops.reset_fields(self.binding.store, self.bot_id, self.guild_id, paths,
                                                           by=self.actor.user_id, via="discord",
                                                           expected_rev=expected_rev, prompt_cap=cap),
                                  page, setting=setting, paths=list(paths))

    async def write_guidance(self, text: Optional[str], *, expected_rev: int, page: str,
                             restore: Optional[str] = None) -> bool:
        """Set (or, with ``restore`` = version id, restore) the server guidance through ``ops``."""
        if not await self._guard("guidance", ["prompts.guild", "prompts.history"]):
            return False
        cap = self.binding.prompt_cap()
        store, bot, guild, by = self.binding.store, self.bot_id, self.guild_id, self.actor.user_id
        if restore is not None:
            run = lambda: ops.restore_guidance(store, bot, guild, restore, by=by, via="discord",  # noqa: E731
                                               expected_rev=expected_rev, prompt_cap=cap)
        else:
            run = lambda: ops.set_guidance(store, bot, guild, text, by=by, via="discord",  # noqa: E731
                                           expected_rev=expected_rev, prompt_cap=cap)
        return await self._commit(run, page, setting="guidance", paths=["prompts.guild"])

    async def transition(self, new_state: str, *, expected_rev: int, purge: bool = False) -> bool:
        """Change the server's state; the (old, new) pair decides which setting the caller needs."""
        old = self.state
        key = perms.PURGE if purge else perms.transition_setting(old, new_state)
        if key is None:
            await self.show_page("home", notice=f"Nothing changed: the policy is already {_STATE_WORD[old]}.")
            return False
        try:
            perms.require(self.actor, key)
        except perms.Refused as exc:
            await self.refuse(str(exc))
            return False
        store, bot, guild, by = self.binding.store, self.bot_id, self.guild_id, self.actor.user_id
        if purge:
            run = lambda: ops.purge_checked(store, bot, guild, by=by, expected_rev=expected_rev)  # noqa: E731
        elif new_state == "active" and old == "unbound":
            run = lambda: ops.activate(store, bot, guild, by=by, expected_rev=expected_rev)  # noqa: E731
        else:
            run = lambda: ops.set_state(store, bot, guild, new_state, by=by, expected_rev=expected_rev)  # noqa: E731
        return await self._commit(run, "home", setting=key, paths=["state"])

    async def _commit(self, run: Callable[[], Snapshot], page: str, *, setting: str = "", paths: Any = ()) -> bool:
        before = self.guild
        task = asyncio.ensure_future(asyncio.to_thread(_from_discord, run))
        try:
            done, _ = await asyncio.wait({task}, timeout=DEFER_AFTER)
            if not done and not self.interaction.response.is_done():
                # The store lock can wait longer than Discord's 3-second answer window: acknowledge first.
                await self.acknowledge()
            await task
        except ConflictError:
            await self.conflict(page)
            return False
        except SchemaError as exc:  # a value the schema refuses (message names the setting, never a path on disk)
            await self.refuse(f"Nothing was saved: {exc}. Next step: run /policy again and choose another value.")
            return False
        except (StoreError, OSError) as exc:
            # Lock timeouts, a damaged or newer file, a full disk: details stay in the host log.
            logger.warning("ctx_discord_policy: write refused for guild %s: %s", self.guild_id, exc)
            await self.refuse("Nothing was saved: the policy file on the host can't be written right now. "
                              "Next step: try again in a moment; if this keeps happening, tell the bot's operator.")
            return False
        self.refresh()
        try:
            self.binding.changed(self, setting, list(paths), before, self.guild)
        except Exception:  # audit and operator messages must never undo or hide a saved change
            logger.warning("ctx_discord_policy: change notification failed", exc_info=True)
        return True

    async def conflict(self, page: str) -> None:
        self.refresh()
        guild = self.guild or {}
        by = who(guild.get("updated_by"))
        stamp = when(guild.get("updated_at"))
        await self.show(build_page(self, page), notice=(
            f"⚠️ Not saved: {by} changed this server's settings {stamp} after this panel was opened. "
            "The panel now shows the current settings; review them and try again."))


def _from_discord(run: Callable[[], Snapshot]) -> Snapshot:
    """Run one store write with the audit origin ``discord`` (lifecycle and settings writes alike)."""
    with ops.origin("discord"):
        return run()


def describe_change(ctx: "Ctx", setting: str, paths: list, before: Optional[dict], after: Optional[dict]) -> str:
    """One audit line: who changed which settings — names only, never values (guidance: size and version)."""
    label = perms.SETTINGS[setting].label if setting in perms.SETTINGS else "settings"
    names = sorted({p.split(".")[-1].replace("_", " ") for p in paths if p != "state"})
    if "state" in paths:
        state = (after or {}).get("state", "deleted") if after else "deleted"
        return f"<@{ctx.actor.user_id}> set the server policy to **{_STATE_WORD.get(state, state)}** (/policy)."
    text = f"<@{ctx.actor.user_id}> changed {label} in /policy"
    if "prompts.guild" in paths:
        guidance = ((after or {}).get("prompts") or {}).get("guild") or ""
        text += f": {len(guidance)} characters" + (f", version {ops.guidance_digest(guidance)[:12]}" if guidance
                                                     else " (now empty)")
    elif names:
        text += f": {', '.join(names)}"
    return text + "."


def _interaction_kind(interaction: Any) -> str:
    kind = getattr(interaction, "type", None)
    return str(getattr(kind, "name", kind) or "")


# ── rendering (one derivation of visibility: perms.may on each control's setting) ────────────────────


def visible_controls(ctx: Ctx, page: Page) -> list:
    out = []
    for control in page.controls:
        if not ctx.may(control.setting):
            continue
        if control.mutates and not ctx.snapshot.writable:
            continue
        out.append(control)
    return out


def _item(ctx: Ctx, control: Control) -> Any:
    discord = ctx.discord
    custom_id = items.encode(ctx.guild_id, control.action, control.arg, ctx.rev)
    if control.kind == "select":
        options = [discord.SelectOption(label=str(opt[0])[:100], value=str(opt[1]), description=(opt[2] or None),
                                        default=bool(opt[3]), emoji=(opt[4] if len(opt) > 4 else None))
                   for opt in control.options]
        item = discord.ui.Select(custom_id=custom_id, options=options, placeholder=control.placeholder,
                                 min_values=min(control.min_values, len(options)),
                                 max_values=min(control.max_values, len(options)), disabled=control.disabled,
                                 row=control.row)
    elif control.kind == "channel_select":
        types = [getattr(discord.ChannelType, t) for t in control.channel_types]
        item = discord.ui.ChannelSelect(custom_id=custom_id, channel_types=types, placeholder=control.placeholder,
                                        min_values=control.min_values, max_values=control.max_values,
                                        default_values=[discord.Object(id=int(i), type=discord.abc.GuildChannel)
                                                        for i in control.defaults],
                                        disabled=control.disabled, row=control.row)
    elif control.kind == "role_select":
        item = discord.ui.RoleSelect(custom_id=custom_id, placeholder=control.placeholder,
                                     min_values=control.min_values, max_values=control.max_values,
                                     default_values=[discord.Object(id=int(i), type=discord.Role)
                                                     for i in control.defaults],
                                     disabled=control.disabled, row=control.row)
    else:
        style = getattr(discord.ButtonStyle, control.style if control.style in _STYLES else "secondary")
        item = discord.ui.Button(custom_id=custom_id, label=control.label[:80], style=style, emoji=control.emoji,
                                 disabled=control.disabled, row=control.row)
    if ctx.binding.persistent:
        return items.component_class(discord)(item)

    async def callback(interaction: Any, _item: Any = item) -> None:
        await items.dispatch(interaction, _item)

    item.callback = callback
    return item


def render(ctx: Ctx, page: Page, *, notice: str = "") -> dict:
    discord = ctx.discord
    description = "\n\n".join(part for part in (notice, page.description) if part)
    embed = discord.Embed(title=page.title[:256], description=description[:4000] or None,
                          colour=_TONES.get(page.tone, _TONES["neutral"]))
    for name, value in page.fields[:25]:
        embed.add_field(name=str(name)[:256], value=(str(value) or "—")[:1024], inline=False)
    if page.footer:
        embed.set_footer(text=page.footer[:2048])
    # Persistent panels never time out: discord.py drops the dynamic-item registration of a view that
    # stops, so a stopping panel would disarm every persistent control in the process.
    view = discord.ui.View(timeout=None if ctx.binding.persistent else VIEW_TIMEOUT)
    for control in visible_controls(ctx, page)[:25]:
        view.add_item(_item(ctx, control))
    return {"embed": embed, "view": view}


# ── protection status (adaptation layer) ─────────────────────────────────────────────────────────────


OUTCOME_WORDS = {"ok": "ok", "armed": "armed", "native": "built into Hermes", "wrapped": "provided by this plugin",
                  "injected": "provided by this plugin", "fallback": "Hermes' own behaviour", "off": "unavailable"}


def _interventions(ctx: Ctx) -> dict:
    return ctx.binding.runtime.status.snapshot().get("interventions") or {}


def protection_summary(ctx: Ctx) -> str:
    rows = _interventions(ctx)
    if not rows:
        return "Not reported yet."
    # Per-context notices (A9[bot/guild/lane/mode]: guidance shortened) close nothing; the operator sees them in full.
    security_down = sorted(n for n, r in rows.items() if r.get("klass") == "S" and r.get("outcome") in ("fallback", "off")
                           and not n.startswith("A9["))
    comfort_down = sorted(n for n, r in rows.items() if r.get("klass") == "K" and r.get("outcome") in ("fallback", "off"))
    armed = ctx.binding.runtime.armed.snapshot()
    if security_down or not armed:
        head = ("⚠️ Reduced: members' messages are not admitted where a safety check failed "
                "(the bot stays closed, not open). The operator can see the details.")
    else:
        head = f"✓ Active in {len(armed)} profile{'s' if len(armed) != 1 else ''}."
    if comfort_down:
        head += f" Comfort: {len(comfort_down)} feature{'s' if len(comfort_down) != 1 else ''} use Hermes' own behaviour."
    return head


def protection_lines(ctx: Ctx, *, reasons: bool) -> str:
    rows = _interventions(ctx)
    if not rows:
        return "Not reported yet."
    lines = []
    for name, result in sorted(rows.items()):
        line = f"{result.get('klass', '?')} {name:<14} {result.get('outcome', '?')}"
        if reasons and result.get("reason"):
            line += f"  {str(result['reason'])[:60]}"
        lines.append(line)
    text = "\n".join(lines)
    if len(text) > 990:
        text = text[:980] + "\n…"
    return f"```\n{text}\n```"


# ── summaries on the start page (each row governed by one perms setting) ─────────────────────────────


# Start-page rows for settings without a panel of their own: (order, setting, label, summary(guild, ctx)).
# Every setting group has its panel now (``pages``), and each panel brings its row (``Panel.summary``).
SUMMARIES: tuple = ()


def summary_rows(ctx: "Ctx") -> list:
    guild = ctx.guild
    if not guild:
        return []
    rows = [(order, key, label, fn) for order, key, label, fn in SUMMARIES]
    rows += [(p.order, p.setting, p.title, p.summary) for p in registered_panels() if p.summary is not None]
    out = []
    for _, key, label, fn in sorted(rows, key=lambda r: r[0]):
        if ctx.may(key):
            out.append((("🔒 " if _locked(key) else "") + label, fn(guild, ctx)))
    return out


def operators_readable(ctx: Ctx) -> bool:
    return bool(perms.operator_list(ctx.binding.owner_home)[1])


def _locked(key: str) -> bool:
    return perms.SETTINGS[key].roles == frozenset({perms.OPERATOR})


_STATE_WORD = {"unbound": "not active", "active": "active", "paused": "paused"}


# ── pages ────────────────────────────────────────────────────────────────────────────────────────────


def _nav(panel: Panel) -> Control:
    label = ("🔒 " if _locked(panel.setting) else "") + panel.title
    return Control(panel.setting, "nav", label, arg=panel.key, emoji=panel.emoji)


def home_page(ctx: Ctx) -> Page:
    title = f"Server policy — {ctx.guild_name}"
    snap = ctx.snapshot
    protection = ("Protection", protection_lines(ctx, reasons=False) if ctx.may("status.full")
                  else protection_summary(ctx))
    status_button = Control("status.summary", "nav", "Status", arg="status")
    if snap.state == CORRUPT:
        return Page(title, ("**The policy file on the host is damaged.** Every server stays paused and nothing "
                            "can be changed here until the operator repairs it (`hermes ctx-discord-policy status` "
                            "on the host shows the details)."), [protection], [status_button], tone="error")
    if snap.state == DOWNGRADE:
        return Page(title, ("**Paused in every server.** The policy file was written by a newer version of this "
                            "plugin, so this version only reads it and changes nothing. The operator can restore "
                            "the newer version or repair the file on the host."), [protection], [status_button],
                    tone="error")
    state, guild = ctx.state, ctx.guild
    readonly = ""
    if not operators_readable(ctx):
        readonly = ("\n\n⚠️ The bot's operator list can't be read right now (its Hermes configuration on the "
                    "host is damaged), so nobody is treated as operator until it is repaired.")
    if state == "unbound":
        if ctx.actor.is_operator:
            text = ("**Not active.** The bot behaves here as if this plugin were not installed.\n"
                    "Activating applies safe defaults that you and the server's managers can change afterwards.")
        else:
            text = "**Not active.** Only the bot's operator can activate it here."
        return Page(title, text + readonly, [protection], [
            Control("guild.activate", "activate", "Activate in this server", style="primary", mutates=True),
            Control("guild.request", "request", "Ask the operator to activate", style="primary"),
            status_button,
        ], tone="inactive")
    head = {"active": "**Active**", "paused": "**Paused.** The bot behaves here as if this plugin were not "
                                              "installed until someone resumes it. Settings are kept."}[state]
    changed = ""
    if guild and guild.get("updated_at"):
        changed = f"\nLast change by {who(guild.get('updated_by'))} {when(guild.get('updated_at'))}."
    description = f"{head}{changed}{readonly}"
    fields = summary_rows(ctx)
    fields.append(protection)
    controls = [_nav(p) for p in registered_panels()]
    controls.append(status_button)
    if state == "active":
        controls.append(Control("guild.pause", "pause", "Pause", style="danger", mutates=True))
    else:
        controls.append(Control("guild.pause", "resume", "Resume", style="success", mutates=True))
    controls.append(Control("guild.unbind", "unbind", "Unbind…", mutates=True))
    return Page(title, description, fields, controls, tone=state, footer=f"Settings revision {ctx.rev}")


def _operators_line(ctx: Ctx) -> str:
    found, readable = perms.operator_list(ctx.binding.owner_home)
    if not readable:
        return "can't be read: the bot's Hermes configuration on the host is damaged; nobody is operator until it is repaired"
    if not found:
        return "none configured: nobody can activate the policy (set in the Hermes configuration on the host)"
    return f"{len(found)} configured (in the Hermes configuration on the host)"


def status_page(ctx: Ctx) -> Page:
    fields = [("Operators", _operators_line(ctx))]
    if ctx.may("status.full"):
        fields.append(("Interventions", protection_lines(ctx, reasons=True)))
        armed = ctx.binding.runtime.armed.snapshot()
        fields.append(("Armed profiles", ", ".join(sorted(clean(v.get("profile"), 40) for v in armed.values()))
                       or "none — public messages are not admitted anywhere"))
        fields.append(("This interface", ctx.binding.caps.reason))
        fields.append(("Policy file", ctx.snapshot.state + (f": {clean(ctx.snapshot.error, 200)}"
                                                            if ctx.snapshot.error else "")))
        description = ("S = safety (fails closed: members get no access), K = comfort (falls back to Hermes' own "
                       "behaviour).")
    else:
        fields.append(("Protection", protection_summary(ctx)))
        description = ("This is how well the plugin's safeguards fit the Hermes version the bot runs. "
                       "The operator sees the details.")
    return Page("Policy status", description, fields,
                [Control("panel.open", "nav", "Back", arg="home")], footer=f"plugin {ctx.binding.runtime.version}")


def activate_page(ctx: Ctx) -> Page:
    return Page(f"Activate the policy in {ctx.guild_name}?", (
        "These defaults apply until you or the server's managers change them:\n"
        "• the bot answers only when addressed (mention, reply, reaction, its name)\n"
        "• it reads along in no channel\n"
        "• members may let it read the current channel's history, nothing else\n"
        "• no worklog channel\n"
        "Tools stay with the operator; members never get them."), [], [
        Control("guild.activate", "activate_ok", "Activate", style="primary", mutates=True),
        Control("panel.open", "nav", "Cancel", arg="home"),
    ], tone="inactive")


def unbind_page(ctx: Ctx) -> Page:
    return Page(f"Unbind {ctx.guild_name}?", (
        "The bot goes back to behaving as if this plugin were not installed here.\n"
        "**Unbind** keeps the settings, so activating again restores them. "
        "**Unbind and delete settings** removes them for good."), [], [
        Control("guild.unbind", "unbind_ok", "Unbind", style="danger", mutates=True),
        Control("guild.unbind", "purge_ok", "Unbind and delete settings", style="danger", mutates=True),
        Control("panel.open", "nav", "Cancel", arg="home"),
    ], tone="paused")


SPECIAL_PAGES = {"home": ("panel.open", home_page), "status": ("status.summary", status_page)}


def page_setting(key: str) -> Optional[str]:
    if key in SPECIAL_PAGES:
        return SPECIAL_PAGES[key][0]
    panel = PANELS.get(key)
    return panel.setting if panel else None


def build_page(ctx: Ctx, key: str) -> Page:
    if key in SPECIAL_PAGES:
        return SPECIAL_PAGES[key][1](ctx)
    panel = PANELS.get(key)
    if panel is None or not ctx.guild:
        return home_page(ctx)
    return panel.build(ctx)


# ── lifecycle actions ────────────────────────────────────────────────────────────────────────────────


@action("nav", lambda ref: page_setting(ref.arg))
async def _nav_action(ctx: Ctx) -> None:
    await ctx.show_page(ctx.ref.arg)


@action("activate", "guild.activate")
async def _activate(ctx: Ctx) -> None:
    if ctx.state != "unbound":
        await ctx.show_page("home", notice="The policy is already active in this server.")
        return
    await ctx.show(activate_page(ctx))


@action("activate_ok", "guild.activate", mutates=True)
async def _activate_ok(ctx: Ctx) -> None:
    if await ctx.transition("active", expected_rev=ctx.ref.rev):
        await ctx.show_page("home", notice="✅ Activated. Open a panel below to adjust the defaults.")


@action("pause", "guild.pause", mutates=True)
async def _pause(ctx: Ctx) -> None:
    if await ctx.transition("paused", expected_rev=ctx.ref.rev):
        await ctx.show_page("home", notice="⏸️ Paused. The bot behaves here as if this plugin were not installed "
                                           "until someone resumes it.")


@action("resume", "guild.pause", mutates=True)
async def _resume(ctx: Ctx) -> None:
    if ctx.state != "paused":
        await ctx.show_page("home", notice=f"Nothing changed: the policy is {_STATE_WORD[ctx.state]}.")
        return
    if await ctx.transition("active", expected_rev=ctx.ref.rev):
        await ctx.show_page("home", notice="▶️ Resumed.")


@action("unbind", "guild.unbind")
async def _unbind(ctx: Ctx) -> None:
    if ctx.state == "unbound" and ctx.guild is None:
        await ctx.show_page("home", notice="This server has no policy settings.")
        return
    await ctx.show(unbind_page(ctx))


@action("unbind_ok", "guild.unbind", mutates=True)
async def _unbind_ok(ctx: Ctx) -> None:
    if await ctx.transition("unbound", expected_rev=ctx.ref.rev):
        await ctx.show_page("home", notice="Unbound. The settings are kept; activating again restores them.")


@action("purge_ok", "guild.unbind", mutates=True)
async def _purge_ok(ctx: Ctx) -> None:
    if await ctx.transition("unbound", expected_rev=ctx.ref.rev, purge=True):
        await ctx.show_page("home", notice="Unbound and settings deleted.")


@action("request", "guild.request")
async def _request(ctx: Ctx) -> None:
    if ctx.state != "unbound":
        await ctx.show_page("home", notice=f"Nothing to ask: the policy is {_STATE_WORD[ctx.state]} here.")
        return
    await ctx.binding.request_activation(ctx)


# ── dispatch ─────────────────────────────────────────────────────────────────────────────────────────


def make_ctx(binding: Any, interaction: Any, *, ref: Optional[items.Ref] = None, item: Any = None) -> Optional[Ctx]:
    guild_id = snowflake(getattr(interaction, "guild_id", None))
    if guild_id is None:
        return None
    snapshot = binding.store.read()
    actor = perms.from_interaction(interaction, owner_home=binding.owner_home,
                                   guild_config=snapshot.guild(binding.bot_id, guild_id))
    return Ctx(binding, interaction, guild_id, actor, snapshot, ref, item)


async def _no_guild(interaction: Any) -> None:
    await interaction.response.send_message("/policy works inside a server, not in direct messages. "
                                            "Next step: run /policy in the server you want to set up.",
                                            ephemeral=True)


async def open_from_command(binding: Any, interaction: Any, page: str) -> None:
    ctx = make_ctx(binding, interaction)
    if ctx is None:
        await _no_guild(interaction)
        return
    setting = page_setting(page)
    if setting is None:
        await ctx.refuse("That page is not available in this version. Next step: run /policy without options.")
        return
    if not ctx.may("panel.open"):
        await ctx.refuse(perms.refusal(ctx.actor, "panel.open"))
        return
    if not ctx.may(setting):
        await ctx.refuse(perms.refusal(ctx.actor, setting))
        return
    await ctx.show(build_page(ctx, page), new_message=True)


async def handle_component(binding: Any, interaction: Any, item: Any) -> None:
    try:
        ref = items.parse(getattr(item, "custom_id", None))
    except items.BadId as exc:
        await interaction.response.send_message(str(exc), ephemeral=True)
        return
    ctx = make_ctx(binding, interaction, ref=ref, item=item)
    if ctx is None:
        await _no_guild(interaction)
        return
    if ref.guild != ctx.guild_id:
        await ctx.refuse("This control belongs to a panel of a different server; nothing was changed. "
                         "Next step: run /policy in this server.")
        return
    act = ACTIONS.get(ref.action)
    setting = None if act is None else (act.setting(ref) if callable(act.setting) else act.setting)
    if act is None or setting is None:
        await ctx.refuse("This control is not known to this version of the plugin. Next step: run /policy again.")
        return
    if not ctx.may(setting):
        await ctx.refuse(perms.refusal(ctx.actor, setting))
        return
    if act.mutates:
        if not ctx.snapshot.writable:
            await ctx.refuse("Settings can't be changed right now: the policy file on the host is "
                             f"{ctx.snapshot.state}. Next step: tell the bot's operator.")
            return
        if ref.rev != ctx.rev:
            await ctx.conflict(act.page)
            return
    await act.handler(ctx)


async def handle_modal(binding: Any, interaction: Any, modal: Any) -> None:
    try:
        ref = items.parse(getattr(modal, "custom_id", None))
    except items.BadId as exc:
        await interaction.response.send_message(str(exc), ephemeral=True)
        return
    ctx = make_ctx(binding, interaction, ref=ref)
    if ctx is None:
        await _no_guild(interaction)
        return
    user_id = str(getattr(getattr(interaction, "user", None), "id", ""))
    if ref.guild != ctx.guild_id or modal.guild_id != ctx.guild_id or modal.opener_id != user_id:
        await ctx.refuse("This form belongs to someone else or to another server; nothing was saved.")
        return
    act = MODAL_ACTIONS.get(ref.action)
    if act is None:
        await ctx.refuse("This form is not known to this version of the plugin. Nothing was saved.")
        return
    if not ctx.may(act.setting):
        await ctx.refuse(perms.refusal(ctx.actor, act.setting))
        return
    if not ctx.snapshot.writable:
        await ctx.refuse("Nothing was saved: the policy file on the host is "
                         f"{ctx.snapshot.state}. Next step: tell the bot's operator.")
        return
    if ref.rev != ctx.rev:
        await ctx.conflict(act.page)
        return
    ctx.origin_view = getattr(modal, "origin_view", None)
    await act.handler(ctx, modal.values())
