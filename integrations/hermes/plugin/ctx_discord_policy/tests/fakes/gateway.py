"""Stand-ins for Hermes' Discord adapter and gateway runner with the stock semantics the policy relies on.

Each call to :func:`make_classes` returns fresh classes, so class-level wrappers installed by one test
never leak into another. The stock behaviour modelled here mirrors Hermes (read in
``plugins/platforms/discord/adapter.py`` and ``gateway/run_inbound.py``): admission requires the author in
``_allowed_user_ids`` and drops messages that mention only others; the handler builds a source whose
``user_id``/``chat_id``/``message_id`` come from the (routed) message; the runner refuses unauthorized
sources and otherwise runs the turn, dispatching the policy hooks the way Hermes does.
"""

from __future__ import annotations

import sys
import types
from types import SimpleNamespace
from typing import Any, Callable, Optional

GUILD = "900000000000000100"
CHANNEL = "900000000000000200"
OTHER_CHANNEL = "900000000000000201"
BOT_USER = "900000000000000300"
APP_ID = "900000000000000300"
OPERATOR = "900000000000000400"
MEMBER = "900000000000000500"


def user(uid: str, *, bot: bool = False, name: str = "member", **extra: Any) -> SimpleNamespace:
    return SimpleNamespace(id=int(uid), bot=bot, display_name=name, name=name, **extra)


class FakeThread(SimpleNamespace):
    """Stands in for ``discord.Thread`` (Hermes tests ``isinstance(channel, discord.Thread)``)."""


def message(author: Any, *, mid: int = 1001, channel: str = CHANNEL, content: str = "hello",
            mentions: Optional[list] = None, reference: Any = None, guild: Optional[str] = GUILD,
            channel_type: int = 0, parent_id: Optional[str] = None) -> SimpleNamespace:
    kind = FakeThread if channel_type in (10, 11, 12) else SimpleNamespace
    chan = kind(id=int(channel), type=channel_type, parent_id=int(parent_id) if parent_id else None)
    return SimpleNamespace(id=mid, author=author, channel=chan, content=content, mentions=list(mentions or []),
                           reference=reference, guild=SimpleNamespace(id=int(guild)) if guild else None,
                           attachments=[], message_snapshots=[], created_at=None, type="default")


class HookBus:
    """Dispatch like Hermes: every registered callback runs; the first block wins for pre_tool_call."""

    def __init__(self, hooks: list[tuple[str, Callable]]) -> None:
        self.hooks = hooks

    def call(self, name: str, **kwargs: Any) -> list[Any]:
        return [r for n, cb in self.hooks if n == name for r in [cb(**kwargs)] if r is not None]

    def tool(self, tool: str, args: Optional[dict] = None, **ids: Any) -> Optional[str]:
        for result in self.call("pre_tool_call", tool_name=tool, args=args or {}, **ids):
            if isinstance(result, dict) and result.get("action") == "block":
                return result.get("message")
        return None


def make_classes(*, home_resolver: Callable[[Any], Any], bus: HookBus,
                 stock_allowed: Optional[set] = None, allowed_roles: Optional[set] = None,
                 group_sessions_per_user: bool = True, thread_sessions_per_user: bool = False,
                 free_channels: Optional[set] = None) -> SimpleNamespace:
    """Fresh DiscordAdapter/GatewayRunner doubles plus a fake ``gateway.run`` module."""

    class DiscordAdapter:
        def __init__(self, runner: Any) -> None:
            self._client = SimpleNamespace(user=user(BOT_USER, bot=True, name="bot"), application_id=int(APP_ID))
            self._allowed_user_ids = set(stock_allowed or ())
            self._allowed_role_ids = set(allowed_roles or ())
            self.gateway_runner = runner
            self.handled: list[Any] = []
            self.last_result: Any = None

        def _self_is_explicitly_mentioned(self, msg: Any) -> bool:
            return any(str(getattr(m, "id", "")) == BOT_USER for m in getattr(msg, "mentions", []))

        def build_source(self, chat_id, chat_type="dm", user_id=None, thread_id=None, guild_id=None,
                         parent_chat_id=None, message_id=None, **_: Any):
            return SimpleNamespace(platform="discord", chat_id=str(chat_id), user_id=user_id, thread_id=thread_id,
                                   guild_id=guild_id, parent_chat_id=parent_chat_id, message_id=message_id,
                                   profile_route_rejected=False)

        def _discord_message_admission(self, message: Any, *, claim: bool) -> tuple[bool, bool]:
            # Hermes: allowlisted users (or role allowlist configured); role flag = roles configured at all.
            if str(message.author.id) not in self._allowed_user_ids and not self._allowed_role_ids:
                return False, False
            if message.mentions and not self._self_is_explicitly_mentioned(message):
                return False, False
            return True, bool(self._allowed_role_ids)

        async def _handle_message(self, message: Any, role_authorized: bool = False, *,
                                  recovered: bool = False) -> bool:
            msg = message
            is_thread = isinstance(msg.channel, FakeThread)
            addressed = self._self_is_explicitly_mentioned(msg) or str(getattr(msg, "type", "")) == "reply"
            if not addressed and not is_thread and str(msg.channel.id) not in (free_channels or set()):
                return False  # require_mention outside bot threads and free-response channels
            self.handled.append(msg)
            source = self.build_source(chat_id=str(msg.channel.id), chat_type="thread" if is_thread else "group",
                                       user_id=str(msg.author.id), thread_id=str(msg.channel.id) if is_thread else None,
                                       guild_id=str(msg.guild.id), message_id=str(msg.id))
            text = (msg.content or "").replace(f"<@{BOT_USER}>", "").strip()
            first = text[1:].split()[0] if text.startswith("/") and len(text) > 1 else ""
            # Hermes dispatches "/name args"; "/home/x.log …" is typed COMMAND but get_command() is None.
            command = first if first and first.replace("-", "").replace("_", "").isalnum() else None
            event = SimpleNamespace(source=source, raw_message=msg, message_id=str(msg.id), text=msg.content,
                                    get_command=lambda: command, role_authorized=role_authorized,
                                    message_type="command" if text.startswith("/") else "text")
            self.last_result = await self.gateway_runner._handle_message(event)
            return True

    class GatewayRunner:
        def __init__(self) -> None:
            self.turns: list[dict] = []
            self.counter = 0
            self.plan: list[tuple[str, dict]] = []   # tool calls every turn attempts, inside the turn

        def _resolve_profile_home_for_source(self, source: Any):
            return home_resolver(source)

        def _is_user_authorized(self, source: Any, *, allow_adapter_delegation: bool = True) -> bool:
            return str(source.user_id) in (stock_allowed or set()) or bool(getattr(source, "role_authorized", False))

        def _session_key_for_source(self, source: Any) -> str:
            # Hermes defaults: threads shared by everyone in them, group channels per user.
            if source.thread_id:
                return f"thread:{source.chat_id}" + (f":{source.user_id}" if thread_sessions_per_user else "")
            return f"group:{source.chat_id}" + (f":{source.user_id}" if group_sessions_per_user else "")

        async def _handle_message(self, event: Any):
            # Internal events (background completions, notifications) skip user authorization in Hermes.
            if getattr(event, "role_authorized", False):
                event.source.role_authorized = True
            if not getattr(event, "internal", False) and not self._is_user_authorized(event.source):
                return "unauthorized"
            self.counter += 1
            session, turn = self._session_key_for_source(event.source), f"turn-{self.counter}"
            context = bus.call("pre_llm_call", session_id=session, turn_id=turn, platform="discord")
            tools = [bus.tool(name, args, session_id=session, turn_id=turn) for name, args in self.plan]
            record = {"session": session, "turn": turn, "context": context, "event": event, "tools": tools}
            self.turns.append(record)
            return record

    module = types.ModuleType("gateway.run")
    module.GatewayRunner = GatewayRunner
    return SimpleNamespace(DiscordAdapter=DiscordAdapter, GatewayRunner=GatewayRunner, module=module)


def install_gateway_module(module: types.ModuleType) -> None:
    sys.modules["gateway.run"] = module


def remove_gateway_module(module: types.ModuleType) -> None:
    if sys.modules.get("gateway.run") is module:
        del sys.modules["gateway.run"]
