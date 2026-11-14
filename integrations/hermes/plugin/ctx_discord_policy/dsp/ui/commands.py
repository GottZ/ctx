"""``/policy`` registration, the K6 capability probe and the per-bot UI binding.

Seam for the integrator: in the Discord platform-handler factory (``ctx.register_platform_handler``;
Hermes calls it after ``ready`` in the profile scope of the bot owner, ``gateway/platforms/base.py``
``_wire_plugin_handlers``) call::

    from .ui import commands as ui_commands
    ui_commands.install(native, adapter, bot_id=binding.bot_id, owner_home=binding.owner_home,
                        store=binding.store)

``install`` never raises; its outcome is recorded as intervention ``K6`` in the status register.

Registration (DESIGN §8.1, OPEN L1): ``/policy`` is a **guild** command in every server the bot is in,
``default_member_permissions = Manage Server``; never a global command (Hermes' own ``safe`` sync deletes
global commands it did not create). It is synced when the binding is installed (the bot is ready then),
on every later ``ready`` and when the bot joins a server, and only when the command's signature for that
server changed since the last successful sync (fingerprints in ``command-sync.json`` next to the policy
store). A join always syncs.
"""

from __future__ import annotations

import asyncio
import hashlib
import importlib
import inspect
import io
import json
import logging
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Optional

from .. import config, hermes
from ..adapt.status import COMFORT, Outcome, Result, write_json_atomic
from ..adapt.strategies import run_intervention
from ..store.file import PolicyStore, _file_lock  # the store's thread+process lock, reused
from . import items, pages, panels, perms  # noqa: F401  (pages: importing registers the settings panels)

logger = logging.getLogger("ctx_discord_policy")

COMMAND_NAME = "policy"
COMMAND_DESCRIPTION = "Set up how the bot behaves in this server"
REQUEST_WINDOW = 24 * 3600
MEMBER_PROBE_ID = "1"          # a user id no Discord account has (snowflakes are far larger)
MODERN_PARTS = ("Label", "FileUpload", "RadioGroup", "Checkbox")


# ── K6: capability probe and strategy ────────────────────────────────────────────────────────────────


@dataclass(frozen=True)
class Caps:
    mode: str                  # "full" | "classic" | "views" | "off"
    modern: bool               # Label/FileUpload/RadioGroup/Checkbox in modals
    persistent: bool           # DynamicItems: controls survive a restart
    reason: str

    @property
    def outcome(self) -> Outcome:
        return {"full": Outcome.OK, "off": Outcome.OFF}.get(self.mode, Outcome.FALLBACK)


def probe(discord: Any, native: Any) -> Caps:
    """Shape checks (DESIGN §6.2 K6): command tree with guild sync, DynamicItems, modern modal parts."""
    if discord is None:
        return Caps("off", False, False, "off: discord.py is not importable; configure with the CLI")
    app_commands = getattr(discord, "app_commands", None)
    tree = getattr(native, "tree", None)
    missing = []
    if app_commands is None or not inspect.isclass(getattr(app_commands, "Command", None)):
        missing.append("app_commands.Command")
    for name in ("add_command", "remove_command", "get_commands", "sync"):
        if not callable(getattr(tree, name, None)):
            missing.append(f"tree.{name}")
    if callable(getattr(tree, "sync", None)):
        try:
            if "guild" not in inspect.signature(tree.sync).parameters:
                missing.append("tree.sync(guild=…)")
        except (TypeError, ValueError):
            missing.append("tree.sync signature")
    if missing:
        return Caps("off", False, False, f"off: {', '.join(missing)} missing; configure with the CLI "
                                         "(hermes ctx-discord-policy)")
    ui = getattr(discord, "ui", None)
    persistent = (inspect.isclass(getattr(ui, "DynamicItem", None))
                  and callable(getattr(native, "add_dynamic_items", None)))
    lacking = [n for n in MODERN_PARTS if not inspect.isclass(getattr(ui, n, None))]
    modern = not lacking
    if modern and persistent:
        return Caps("full", True, True, "full: Label, FileUpload, RadioGroup, Checkbox, DynamicItems")
    if persistent:
        return Caps("classic", False, True, f"classic components: {', '.join(lacking)} missing "
                                            "(text-only forms, choices as selects)")
    return Caps("views", modern, False, "panels expire after 14 minutes: DynamicItems missing "
                                        + (f"; {', '.join(lacking)} missing" if lacking else ""))


def _downgrade(caps: Caps, mode: Optional[str]) -> Caps:
    """Force a lesser strategy (tests, troubleshooting); never an upgrade beyond what the probe found."""
    if mode in (None, caps.mode) or caps.mode == "off":
        return caps
    if mode == "classic" and caps.persistent:
        return Caps("classic", False, True, "classic components (forced)")
    if mode == "views":
        return Caps("views", caps.modern, False, "panels expire after 14 minutes (forced)")
    if mode == "off":
        return Caps("off", False, False, "off (forced)")
    return caps


# ── Hermes data through dsp.hermes (optional functions; absent ⇒ unknown, which fails closed) ──────────


def _default_skills_provider(ctx: Any) -> Optional[list]:
    """Skills installed in every profile members' turns of this server run in (a skill must exist wherever
    it is released), or None when Hermes can't tell us."""
    lister = getattr(hermes, "installed_skills", None)
    homes = ctx.binding.target_homes(ctx)
    if not callable(lister) or not homes:
        return None
    common: Optional[set] = None
    for home in sorted(homes):
        try:
            names = set(lister(home))
        except Exception:
            logger.warning("ctx_discord_policy: listing the skills of %s failed", home, exc_info=True)
            return None
        common = names if common is None else common & names
    return sorted(common or ())


def _default_spill_provider() -> Any:
    reader = getattr(hermes, "spill_config", None)
    if not callable(reader):
        return None
    try:
        return reader()
    except Exception:
        return None


async def _read_attachment(attachment: Any) -> bytes:
    return await attachment.read()


# ── activation requests (throttled, persisted next to the store) ─────────────────────────────────────


class ActivationRequests:
    def __init__(self, path: Path, window: float = REQUEST_WINDOW) -> None:
        self.path = Path(path)
        self.lock_path = self.path.with_name(self.path.name + ".lock")
        self.window = window

    def _read(self) -> dict:
        try:
            data = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return {}
        return data if isinstance(data, dict) else {}

    def claim(self, bot_id: str, guild_id: str, now: Optional[float] = None) -> tuple[bool, Optional[float]]:
        """Take the request slot of one server; ``(False, last)`` while the previous request is recent."""
        now = time.time() if now is None else now
        with _file_lock(self.lock_path):          # threads and processes: one claim per window
            data = self._read()
            key = f"{bot_id}:{guild_id}"
            last = data.get(key)
            if isinstance(last, (int, float)) and now - last < self.window:
                return False, float(last)
            data = {k: v for k, v in data.items() if isinstance(v, (int, float)) and now - v < self.window}
            data[key] = now
            write_json_atomic(self.path, data)
        return True, None

    def release(self, bot_id: str, guild_id: str) -> None:
        with _file_lock(self.lock_path):
            data = self._read()
            if data.pop(f"{bot_id}:{guild_id}", None) is not None:
                write_json_atomic(self.path, data)


# ── the binding of one bot ───────────────────────────────────────────────────────────────────────────


class UiBinding:
    """Everything the interface needs for one connected bot (one discord.py client)."""

    def __init__(self, *, client: Any, discord: Any, bot_id: str, owner_home: Path, store: PolicyStore,
                 caps: Caps, runtime: Any, adapter: Any = None) -> None:
        self.client = client
        self.discord = discord
        self.tree = client.tree
        self.bot_id = str(bot_id)
        self.owner_home = Path(owner_home)
        self.store = store
        self.caps = caps
        self.runtime = runtime
        self.adapter = adapter
        self.command: Any = None
        data_dir = hermes.plugin_data_dir(self.owner_home)
        self.sync_path = data_dir / "command-sync.json"
        self.requests = ActivationRequests(data_dir / "activation-requests.json")
        self.notifier: Any = None        # async (user_id, text, file=None) -> None; default: a direct message
        self.channel_sink: Any = None    # async (channel_id, text) -> None; default: post in the channel
        # Hermes data the interface needs but may not import itself (DESIGN: Hermes only via dsp.hermes):
        # skills_provider(ctx) -> iterable of skill names installed in the profile this server's turns run in,
        # or None when unknown; spill_provider() -> Hermes' spill config (prompt budget), or None.
        self.skills_provider: Any = _default_skills_provider
        self.spill_provider: Any = _default_spill_provider
        self.read_attachment: Any = _read_attachment
        self.sync_results: dict = {}
        self._sync_lock = asyncio.Lock()
        self._tasks: set = set()
        from .notify import Notifier

        self.notify = Notifier(self)

    @property
    def modern(self) -> bool:
        return self.caps.modern

    @property
    def persistent(self) -> bool:
        return self.caps.persistent

    def prompt_cap(self) -> int:
        return int(config.setting(self.owner_home, "guild_prompt_max_chars"))

    def target_homes(self, ctx: Any) -> Optional[set]:
        """Homes of the profiles a member's message in this server runs in (A4), for every text channel the
        client knows; only profiles where the policy is armed (elsewhere public admission stays closed).
        ``None`` when none is known — callers then fail closed."""
        from ..ingress import admission
        from ..policy import hooks
        from ..runtime import RUNTIME

        guild = getattr(ctx.interaction, "guild", None)
        channels = [str(c.id) for c in (getattr(guild, "text_channels", None) or [])]
        if not channels:
            channels = [str(getattr(ctx.interaction, "channel_id", None) or ctx.guild_id)]
        homes: set = set()
        for channel in channels:
            try:
                # A neutral author: what a member gets, never a route that matches the caller's own user id.
                key = admission.target_home_key(self.adapter, None, guild_id=ctx.guild_id, channel_id=channel,
                                                author_id=MEMBER_PROBE_ID, thread_id=None, parent_id=None)
            except Exception:
                logger.warning("ctx_discord_policy: resolving the target profile failed", exc_info=True)
                return None
            if key is None or not hooks.armed_live(key):
                continue
            instance = RUNTIME.armed.instance(key)
            if instance is not None and getattr(instance, "home", ""):
                homes.add(Path(instance.home))
        return homes or None

    # ── interface entry points ──

    async def on_command(self, interaction: Any, page: str) -> None:
        await panels.open_from_command(self, interaction, page)

    async def on_component(self, interaction: Any, item: Any) -> None:
        await panels.handle_component(self, interaction, item)

    async def on_modal(self, interaction: Any, modal: Any) -> None:
        await panels.handle_modal(self, interaction, modal)

    async def on_upload(self, interaction: Any, attachment: Any) -> None:
        from .pages import guidance

        await guidance.upload_from_command(self, interaction, attachment)

    # ── the command ──

    def build_command(self) -> Any:
        discord = self.discord
        app_commands = discord.app_commands
        binding = self

        async def policy(interaction, page=None, guidance_file=None):  # noqa: ANN001 - annotations set below
            if guidance_file is not None:
                await binding.on_upload(interaction, guidance_file)
            else:
                await binding.on_command(interaction, page or "home")

        policy.__annotations__ = {"interaction": discord.Interaction, "page": Optional[str],
                                  "guidance_file": Optional[discord.Attachment], "return": None}
        choices = [app_commands.Choice(name="Overview", value="home"),
                   app_commands.Choice(name="Status", value="status")]
        choices += [app_commands.Choice(name=p.title, value=p.key) for p in panels.registered_panels()]
        fn: Any = app_commands.describe(page="Open one page directly",
                                        guidance_file="Replace this server's guidance with a .md or .txt file")(policy)
        fn = app_commands.choices(page=choices[:25])(fn)
        command = app_commands.Command(name=COMMAND_NAME, description=COMMAND_DESCRIPTION, callback=fn)
        command = app_commands.default_permissions(manage_guild=True)(command)
        return app_commands.guild_only()(command)

    def ensure_command(self, guild_id: Any) -> None:
        self.tree.add_command(self.command, guild=self.discord.Object(id=int(guild_id)), override=True)

    def fingerprint(self, guild_id: Any) -> str:
        guild = self.discord.Object(id=int(guild_id))
        desired = [c.to_dict(self.tree) for c in self.tree.get_commands(guild=guild)]
        desired.sort(key=lambda d: (d.get("type", 1), d.get("name", "")))
        return hashlib.sha256(json.dumps(desired, sort_keys=True, default=str).encode("utf-8")).hexdigest()

    def _read_sync(self) -> dict:
        try:
            data = json.loads(self.sync_path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return {}
        return data if isinstance(data, dict) else {}

    async def sync_guild(self, guild_id: Any, *, force: bool = False) -> str:
        gid = str(guild_id)
        self.ensure_command(gid)
        fp = self.fingerprint(gid)
        async with self._sync_lock:
            state = self._read_sync()
            entry = (state.get(self.bot_id) or {}).get(gid)
            if not force and isinstance(entry, dict) and entry.get("fingerprint") == fp:
                result = "unchanged"
            else:
                try:
                    await self.tree.sync(guild=self.discord.Object(id=int(gid)))
                except Exception as exc:  # Forbidden without applications.commands, rate limits, outages
                    logger.warning("ctx_discord_policy: /policy sync for server %s failed: %s", gid, exc)
                    result = f"failed: {type(exc).__name__}"
                else:
                    state.setdefault(self.bot_id, {})[gid] = {"fingerprint": fp, "at": time.time()}
                    write_json_atomic(self.sync_path, state)
                    result = "synced"
        self.sync_results[gid] = result
        return result

    async def sync_all(self) -> dict:
        for guild in list(getattr(self.client, "guilds", None) or []):
            await self.sync_guild(guild.id)
        counts: dict = {}
        for result in self.sync_results.values():
            counts[result.split(":")[0]] = counts.get(result.split(":")[0], 0) + 1
        logger.info("ctx_discord_policy: /policy command per server: %s", counts or "no servers")
        return dict(self.sync_results)

    def forget_guild(self, guild_id: Any) -> None:
        gid = str(guild_id)
        self.tree.remove_command(COMMAND_NAME, guild=self.discord.Object(id=int(gid)))
        self.sync_results.pop(gid, None)
        state = self._read_sync()
        if (state.get(self.bot_id) or {}).pop(gid, None) is not None:
            write_json_atomic(self.sync_path, state)

    async def _on_ready(self) -> None:
        await self.startup()

    async def startup(self) -> None:
        """After connecting: sync the command, report a worse protection status, send coalesced messages."""
        await self.sync_all()
        try:
            await self.notify.check_status()
            await self.notify.flush_all_pending()
        except Exception:
            logger.warning("ctx_discord_policy: operator notifications at startup failed", exc_info=True)

    async def _on_guild_join(self, guild: Any) -> None:
        await self.sync_guild(guild.id, force=True)

    async def _on_guild_remove(self, guild: Any) -> None:
        self.forget_guild(guild.id)

    def attach(self, *, schedule: bool = True) -> None:
        items.bind_client(self.client, self)
        self.command = self.build_command()
        for guild in list(getattr(self.client, "guilds", None) or []):
            self.ensure_command(guild.id)
        if self.persistent:
            self.client.add_dynamic_items(items.component_class(self.discord))
        self.client.add_listener(self._on_ready, "on_ready")
        self.client.add_listener(self._on_guild_join, "on_guild_join")
        self.client.add_listener(self._on_guild_remove, "on_guild_remove")
        if schedule:
            try:
                loop = asyncio.get_running_loop()
            except RuntimeError:
                return  # no loop (CLI, tests): the next on_ready syncs
            task = loop.create_task(self.startup(), name="ctx_discord_policy-command-sync")
            self._tasks.add(task)
            task.add_done_callback(self._tasks.discard)

    def detach(self) -> None:
        """Undo a partial :meth:`attach` (best effort): no binding, no command, no listeners, no timers."""
        items.unbind_client(self.client)
        self.notify.cancel_timers()
        for name, listener in (("on_ready", self._on_ready), ("on_guild_join", self._on_guild_join),
                               ("on_guild_remove", self._on_guild_remove)):
            try:
                self.client.remove_listener(listener, name)
            except Exception:
                pass
        if self.command is not None:
            for guild in list(getattr(self.client, "guilds", None) or []):
                try:
                    self.tree.remove_command(COMMAND_NAME, guild=self.discord.Object(id=int(guild.id)))
                except Exception:
                    pass

    # ── activation requests ──

    def request_text(self, ctx: Any) -> str:
        user = getattr(ctx.interaction, "user", None)
        name = panels.clean(getattr(user, "name", "") or "", 40)
        guild = panels.clean(ctx.guild_name, 80)
        return (f"<@{ctx.actor.user_id}> ({name}) asked to activate the server policy in **{guild}** "
                f"(id {ctx.guild_id}).\n"
                "Activate it with /policy in that server. If /policy is not offered to you there (it needs Manage "
                "Server unless the server's admins allow it for you under Server Settings → Integrations), "
                f"activate it on the host instead: `hermes ctx-discord-policy bind {ctx.guild_id} --bot {self.bot_id}`.\n"
                "(You get at most one request per server per day.)")

    async def _send_dm(self, user_id: str, text: str, file: Optional[tuple] = None) -> None:
        user = self.client.get_user(int(user_id)) or await self.client.fetch_user(int(user_id))
        kwargs: dict = {"allowed_mentions": self.discord.AllowedMentions.none()}
        if file is not None:
            kwargs["file"] = self.discord.File(io.BytesIO(file[1]), filename=file[0])
        await user.send(text[:2000], **kwargs)

    async def send_dm(self, user_id: str, text: str, *, file: Optional[tuple] = None) -> None:
        if self.notifier is not None:
            await self.notifier(user_id, text, file=file)
        else:
            await self._send_dm(user_id, text, file)

    async def send_channel(self, channel_id: str, text: str) -> None:
        if self.channel_sink is not None:
            await self.channel_sink(channel_id, text)
            return
        channel = self.client.get_channel(int(channel_id)) or await self.client.fetch_channel(int(channel_id))
        await channel.send(text[:2000], allowed_mentions=self.discord.AllowedMentions.none())

    def changed(self, ctx: Any, setting: str, paths: list, before: Optional[dict], after: Optional[dict]) -> None:
        """After every saved change: one line in the audit channel, a diff to the operators for guidance."""
        from .panels import describe_change

        guild = after or before
        self.notify.audit(guild, describe_change(ctx, setting, paths, before, after))
        if after is None:
            self.notify.forget_guild(ctx.guild_id)            # settings deleted: drop the held guidance copy
        if "prompts.guild" in paths:
            old = ((before or {}).get("prompts") or {}).get("guild") or ""
            new = ((after or {}).get("prompts") or {}).get("guild") or ""
            if old != new:
                self.notify.guidance_changed(ctx, old, new)

    async def request_activation(self, ctx: Any) -> None:
        await ctx.interaction.response.defer(ephemeral=True, thinking=True)
        found, readable = perms.operator_list(self.owner_home)
        operators = sorted(found)
        if not readable:
            await ctx.say("The bot's operator list can't be read right now (its Hermes configuration on the host "
                          "is damaged), so no request was sent. Next step: tell the person who runs the bot.")
            return
        if not operators:
            await ctx.say("This bot has no operator yet, so nobody can activate the policy here. Next step: tell "
                          "the person who runs the bot; operators are set in its Hermes configuration.")
            return
        if not config.setting(self.owner_home, "notify_operators"):
            await ctx.say("The bot's operator does not take activation requests through Discord. Next step: "
                          "contact them directly.")
            return
        claimed, last = await asyncio.to_thread(self.requests.claim, self.bot_id, ctx.guild_id)
        if not claimed:
            await ctx.say(f"The operator was already asked <t:{int(last)}:R>. Requests are limited to one per "
                          f"server per day; you can ask again <t:{int(last + self.requests.window)}:R>.")
            return
        text = self.request_text(ctx)
        delivered = 0
        for operator in operators:
            try:
                await self.send_dm(operator, text)
                delivered += 1
            except Exception as exc:  # closed DMs, unknown user, outage
                logger.warning("ctx_discord_policy: activation request to an operator failed: %s", exc)
        if not delivered:
            await asyncio.to_thread(self.requests.release, self.bot_id, ctx.guild_id)
            await ctx.say("The request could not be delivered: the operator does not accept direct messages from "
                          "this bot. Nothing was sent. Next step: contact the operator directly.")
            return
        await ctx.say("✅ Request sent to the bot's operator; you'll see the change here once they activate it. "
                      "Tip: if the operator does not manage this server, they need /policy allowed for them under "
                      "Server Settings → Integrations, or they activate it on the host.")


def install(native: Any, adapter: Any = None, *, bot_id: Any, owner_home: Any, store: Optional[PolicyStore] = None,
            runtime: Any = None, mode: Optional[str] = None, discord_module: Any = None,
            schedule: bool = True) -> Optional[UiBinding]:
    """Install ``/policy`` on one connected bot. Returns the binding, or None when the UI is off (K6)."""
    if runtime is None:
        from ..runtime import RUNTIME as runtime  # noqa: N811
    if discord_module is None:
        try:
            discord_module = importlib.import_module("discord")
        except ImportError:
            discord_module = None
    holder: dict = {}

    def body() -> Result:
        caps = _downgrade(probe(discord_module, native), mode)
        holder["caps"] = caps
        if caps.mode != "off":
            binding = UiBinding(client=native, discord=discord_module, bot_id=str(bot_id),
                                owner_home=Path(owner_home),
                                store=store or PolicyStore(hermes.plugin_data_dir(owner_home) / "policy.json"),
                                caps=caps, runtime=runtime, adapter=adapter)
            try:
                binding.attach(schedule=schedule)
            except Exception as exc:
                logger.exception("ctx_discord_policy: installing /policy failed")
                binding.detach()
                return Result("K6", COMFORT, Outcome.OFF, f"off: installing the interface failed "
                              f"({type(exc).__name__}: {exc}); configure with the CLI", "UiBinding.attach")
            holder["binding"] = binding
        return Result("K6", COMFORT, caps.outcome, caps.reason, "discord.ui, CommandTree.sync(guild=), "
                                                                "Client.add_dynamic_items")

    run_intervention(runtime.status, "K6", COMFORT, body)
    return holder.get("binding")
