"""A real Discord adapter and gateway runner of a Hermes checkout, the plugin loaded by Hermes' own plugin manager.

The shared scene of the contract tests for the policy's closing rules (bots and webhooks, paused servers,
commands, two bots, delivery): messages go through Hermes' real Discord admission and handler, wrapped by the
plugin (A5/A6); the event Hermes builds is captured where it would enter the pipeline, then run through the
plugin's runner wrapper (A7) around a turn body that dispatches Hermes' real ``pre_tool_call`` hooks.

Stubbed: the adapter's network-facing helpers (no Discord client), the runner's profile resolution (one home),
and the turn body itself (no model). Everything that decides who gets a turn is Hermes' or the plugin's code.
"""

from __future__ import annotations

import asyncio
import sys
from types import SimpleNamespace
from typing import Any, Optional

from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, dispatch_tool, load_in

GUILD, CHANNEL, BOT, OPERATOR, MEMBER, OTHER_BOT, ROLE = (
    "900000000000000100", "900000000000000200", "900000000000000300", "900000000000000400",
    "900000000000000500", "900000000000000600", "900000000000000777")
OTHER_GUILD = "900000000000000101"

_SAVED = (("gateway.run", "GatewayRunner", ("_handle_message", "_is_user_authorized")),
          ("plugins.platforms.discord.adapter", "DiscordAdapter",
           ("_discord_message_admission", "_handle_message", "handle_message", "send_document", "send_video",
            "send_voice", "send_image_file", "send_multiple_images", "send", "edit_message", "_send_prompt",
            "on_processing_start", "on_processing_complete", "_discord_history_backfill",
            "_deliver_media_attachments", "_send_image_batch")))


def _config(operators: list[str], extra: str = "") -> str:
    ops = ", ".join(f'"{o}"' for o in operators)
    return (f"plugins:\n  enabled: [{PLUGIN}]\n  entries:\n    {PLUGIN}:\n      settings:\n"
            f"        operators: [{ops}]\n{extra}")


class World:
    """One gateway process: the runner, the bound bots, the captured pipeline entries."""

    def __init__(self, hermes: Any, tmp_path: Any, monkeypatch: Any, *, settings: str = "",
                 capture: bool = True) -> None:
        _evict(ANCHOR)
        monkeypatch.setenv("HERMES_HOME", str(tmp_path / "root"))
        self.monkeypatch = monkeypatch
        self.homes = Homes(tmp_path)
        (self.homes.root / "config.yaml").write_text(_config([OPERATOR], settings), encoding="utf-8")
        import discord
        from gateway.config import Platform
        from gateway.run import GatewayRunner
        from plugins.platforms.discord.adapter import DiscordAdapter

        self.discord, self.Platform = discord, Platform
        self.GatewayRunner, self.DiscordAdapter = GatewayRunner, DiscordAdapter
        self._saved = {}
        for module_name, class_name, names in _SAVED:
            cls = getattr(sys.modules[module_name], class_name)
            for name in names:
                self._saved[(cls, name)] = cls.__dict__.get(name)
        self.pipeline: list = []                 # events that reached Hermes' message pipeline (handle_message)
        if not capture:
            recorded = self.pipeline

            async def pipeline_entry(self, event):
                recorded.append(event)

            DiscordAdapter.handle_message = pipeline_entry   # beneath the plugin's wrapper (installed at bind)
        load_in(self.homes.root)
        runner = GatewayRunner.__new__(GatewayRunner)
        runner.config = SimpleNamespace(multiplex_profiles=False, profile_routes=[], group_sessions_per_user=True,
                                        thread_sessions_per_user=False)
        runner._resolve_profile_home_for_source = lambda source: self.homes.root
        runner._profile_name_for_source = lambda source, adapter_profile=None: None
        self.runner = runner
        self.adapters: dict[str, Any] = {}
        self.capture = capture
        self.add_bot(BOT, self.homes.root)
        runner._delivery_adapter_for = lambda source: self.adapter
        self.runtime = self.mod("runtime").RUNTIME
        self.bind(GUILD)

    # ── plumbing ──

    @staticmethod
    def mod(name: str) -> Any:
        return sys.modules[f"{ANCHOR}.{name}"]

    @property
    def adapter(self) -> Any:
        return self.adapters[BOT]

    def add_bot(self, bot_id: str, home: Any, *, allow_bots: str = "none") -> Any:
        DiscordAdapter, Platform = self.DiscordAdapter, self.Platform
        from gateway.platforms.helpers import MessageDeduplicator

        adapter = DiscordAdapter.__new__(DiscordAdapter)
        adapter.platform = Platform.DISCORD
        adapter.config = SimpleNamespace(extra={"allow_bots": allow_bots})
        adapter._owner_profile = None
        adapter.gateway_runner = self.runner
        adapter._dedup = MessageDeduplicator()
        bot_user = SimpleNamespace(id=int(bot_id), bot=True, name="bot", display_name="bot")
        adapter._client = SimpleNamespace(user=bot_user, application_id=int(bot_id))
        adapter._allowed_user_ids = {"900000000000000999"}
        adapter._allowed_role_ids = set()
        stubs = dict(
            _is_pairing_approved_user=lambda _uid: False, _get_parent_channel_id=lambda _c: None,
            _warn_if_fail_closed_default=lambda: None, _get_allowed_channels=lambda: set(),
            _get_ignored_channels=lambda: set(), _discord_free_response_channels=lambda: set(),
            _discord_require_mention=lambda: True, _voice_text_channels={}, _in_bot_thread=lambda m: False,
            _get_no_thread_channels=lambda: set(),
            _extra_or_env_flag=lambda name, *a, **k: name not in ("auto_thread", "history_backfill"),
            _discord_free_response_auto_thread=lambda: False,
            _get_effective_topic=lambda ch, is_thread=False: None,
            _resolve_channel_skills=lambda *a: None, _resolve_channel_prompt=lambda *a: None,
            _text_batch_delay_seconds=0, _threads=SimpleNamespace(mark_async=lambda _t: asyncio.sleep(0)),
            _is_bot_tag_debounce_continuation=lambda m: False)
        for name, value in stubs.items():
            setattr(adapter, name, value)

        async def media(_attachments):
            return [], [], [], None

        adapter._collect_attachment_media = media
        if self.capture:
            recorded = self.pipeline

            async def capture(event):
                recorded.append(event)

            adapter.handle_message = capture
        from hermes_constants import reset_hermes_home_override, set_hermes_home_override

        token = set_hermes_home_override(str(home))
        try:
            self.mod("adapt.interventions").discord_platform_handler(SimpleNamespace(application_id=int(bot_id)),
                                                                     adapter)
        finally:
            reset_hermes_home_override(token)
        self.adapters[bot_id] = adapter
        return adapter

    def binding(self, bot_id: str = BOT) -> Any:
        return self.runtime.bots.for_bot(bot_id)

    def bind(self, guild_id: str, bot_id: str = BOT) -> None:
        self.mod("store.ops").bind(self.binding(bot_id).store, bot_id, guild_id, by="cli")

    def set_state(self, state: str, guild_id: str = GUILD, bot_id: str = BOT) -> None:
        self.mod("store.ops").set_state(self.binding(bot_id).store, bot_id, guild_id, state, by="test")

    def close(self) -> None:
        for (cls, name), value in self._saved.items():
            if value is None:
                if name in cls.__dict__:
                    delattr(cls, name)
            else:
                setattr(cls, name, value)
        _evict(ANCHOR)

    # ── messages and turns ──

    def message(self, author_id: str, mid: int, text: str, *, bot: bool = False, webhook_id: Any = None,
                roles: tuple = (), guild: str = GUILD, mention: Optional[str] = BOT) -> Any:
        guild_obj = SimpleNamespace(id=int(guild), name="g")
        author = SimpleNamespace(id=int(author_id), bot=bot, display_name="x", name="x", guild=guild_obj,
                                 roles=[SimpleNamespace(id=int(r)) for r in roles])
        target = self.adapters.get(mention) if mention else None
        mentions = [target._client.user] if target is not None else []
        content = f"<@{mention}> {text}" if mention else text
        return SimpleNamespace(
            id=mid, author=author, guild=guild_obj,
            channel=SimpleNamespace(id=int(CHANNEL), type=0, parent_id=None, name="general", guild=guild_obj),
            type=self.discord.MessageType.default, mentions=mentions, content=content, reference=None,
            attachments=[], message_snapshots=[], created_at=None, webhook_id=webhook_id)

    async def deliver(self, msg: Any, bot_id: str = BOT) -> dict:
        """Hermes' Discord admission and handler (both wrapped by the plugin): what reached the pipeline."""
        adapter = self.adapters[bot_id]
        before = len(self.pipeline)
        admitted, role = adapter._discord_message_admission(msg, claim=True)
        out = {"admitted": admitted, "role": role, "handled": None, "event": None}
        if admitted:
            out["handled"] = await adapter._handle_message(msg, role_authorized=role)
            if len(self.pipeline) > before:
                out["event"] = self.pipeline[-1]
        return out

    async def turn(self, event: Any, *, reply: str = "ran", tool: str = "terminal") -> dict:
        """The plugin's runner wrapper (A7) around a turn body that asks Hermes' real hook dispatch whether
        ``tool`` may run, as the agent loop does before every tool call."""
        seen: dict = {}
        hooks = self.mod("policy.hooks")
        runtime = self.runtime

        async def body(runner, ev):
            session = self.GatewayRunner._session_key_for_source(runner, ev.source)
            seen["route"] = runtime.route.get()
            seen["context"] = hooks.on_pre_llm_call(session_id=session, turn_id="t1")
            seen["blocked"] = dispatch_tool(self.homes.root, tool, session_id=session, turn_id="t1")
            return reply

        result = await self.mod("adapt.interventions")._make_runner_handle(body)(self.runner, event)
        return {"result": result, "ran": "route" in seen, "route": seen.get("route"),
                "lane": getattr(seen.get("route"), "lane", None), "blocked": seen.get("blocked")}

    def authorized(self, source: Any) -> Any:
        try:
            return self.GatewayRunner._is_user_authorized(self.runner, source)
        except Exception as exc:
            return f"error: {type(exc).__name__}: {exc}"
