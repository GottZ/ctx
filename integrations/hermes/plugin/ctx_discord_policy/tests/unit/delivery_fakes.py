"""Discord doubles for the delivery tests: a small world of guilds, channels, threads and messages, and an
adapter class with the delivery methods in Hermes' shapes (``send``, ``edit_message``, media senders,
processing hooks). Every call to ``make_adapter_class()`` returns a fresh class, because the delivery
layer wraps class attributes.
"""

from __future__ import annotations

import asyncio
import itertools
from dataclasses import dataclass
from types import SimpleNamespace
from typing import Any, Optional

# Obviously synthetic snowflakes (no real Discord object has these ids).
BOT = "910000000000000001"
GUILD_A = "910000000000000100"
GUILD_B = "910000000000000200"
OPERATOR = "910000000000000007"
MEMBER = "910000000000000008"


@dataclass
class SendResult:
    success: bool
    message_id: Optional[str] = None
    error: Optional[str] = None
    raw_response: Any = None
    retryable: bool = False
    continuation_message_ids: tuple = ()


class ProcessingOutcome:
    SUCCESS = "success"
    FAILURE = "failure"


class FakeMessage:
    def __init__(self, world: "World", channel: "FakeChannel", message_id: int, content: Any = None,
                 author: Any = None, **kwargs: Any) -> None:
        self.world = world
        self.channel = channel
        self.id = message_id
        self.content = content
        self.author = author
        self.kwargs = kwargs
        self.reactions: list[tuple[str, str]] = []
        self.deleted = False

    async def create_thread(self, *, name: str, auto_archive_duration: Optional[int] = None) -> "FakeChannel":
        if self.world.fail_threads:
            raise RuntimeError("Missing Permissions")
        thread = self.world.channel(self.world.new_id(), self.channel.guild.id, parent_id=self.channel.id)
        thread.name = name
        thread.auto_archive_duration = auto_archive_duration
        return thread

    async def delete(self) -> None:
        self.deleted = True

    async def add_reaction(self, emoji: str) -> None:
        self.reactions.append(("add", emoji))

    async def remove_reaction(self, emoji: str, member: Any = None) -> None:
        self.reactions.append(("remove", emoji))


class FakeChannel:
    def __init__(self, world: "World", channel_id: int, guild_id: str, parent_id: Optional[int] = None) -> None:
        self.world = world
        self.id = channel_id
        self.guild = SimpleNamespace(id=int(guild_id))
        self.parent_id = parent_id
        self.name = f"channel-{channel_id}"
        self.auto_archive_duration: Optional[int] = None
        self.messages: dict[int, FakeMessage] = {}
        self.edits: list[tuple[int, str]] = []

    async def send(self, content: Any = None, **kwargs: Any) -> FakeMessage:
        message = FakeMessage(self.world, self, self.world.new_id(), content, **kwargs)
        message.attachments = [kwargs["file"]] if kwargs.get("file") is not None else list(kwargs.get("files") or [])
        self.messages[message.id] = message
        self.world.log.append((str(self.id), content, kwargs))
        return message

    async def fetch_message(self, message_id: int) -> FakeMessage:
        return self.messages[int(message_id)]

    def get_partial_message(self, message_id: int) -> Any:
        channel = self

        class Partial:
            async def edit(self, *, content: str) -> None:
                if int(message_id) not in channel.messages:
                    raise LookupError("Unknown Message")
                channel.messages[int(message_id)].content = content
                channel.edits.append((int(message_id), content))

            async def delete(self) -> None:
                if int(message_id) not in channel.messages:
                    raise LookupError("Unknown Message")
                channel.messages[int(message_id)].deleted = True

        return Partial()


class World:
    """The Discord client double: ``get_channel``/``fetch_channel`` over every guild of the test."""

    def __init__(self) -> None:
        self.channels: dict[int, FakeChannel] = {}
        self.log: list[tuple[str, Any, dict]] = []
        self.ids = itertools.count(920000000000000001)
        self.fail_threads = False
        self.fetches: list[int] = []
        self.user = SimpleNamespace(id=int(BOT), bot=True)
        # Optional fault injection: hook(adapter, chat_id, content, reply_to, metadata) -> SendResult | None
        self.send_hook: Any = None
        # hook(adapter, chat_id, message_id, content, finalize) -> awaitable SendResult | None
        self.edit_hook: Any = None

    def new_id(self) -> int:
        return next(self.ids)

    def channel(self, channel_id: int, guild_id: str, parent_id: Optional[int] = None) -> FakeChannel:
        channel = FakeChannel(self, int(channel_id), guild_id, parent_id)
        self.channels[int(channel_id)] = channel
        return channel

    def get_channel(self, channel_id: int) -> Optional[FakeChannel]:
        return self.channels.get(int(channel_id))

    async def fetch_channel(self, channel_id: int) -> FakeChannel:
        self.fetches.append(int(channel_id))
        if int(channel_id) not in self.channels:
            raise LookupError("Unknown Channel")
        return self.channels[int(channel_id)]

    def sent_to(self, channel_id: Any) -> list[Any]:
        return [content for chat, content, _ in self.log if chat == str(channel_id)]

    def guild_of(self, channel_id: Any) -> str:
        return str(self.channels[int(channel_id)].guild.id)

    def post(self, channel: FakeChannel, content: str, author_id: str, name: str) -> FakeMessage:
        """A human message that already exists in Discord (the trigger of a route)."""
        message = FakeMessage(self, channel, self.new_id(), content,
                              author=SimpleNamespace(id=int(author_id), display_name=name, name=name))
        channel.messages[message.id] = message
        return message


def make_adapter_class() -> type:
    class FakeDiscordAdapter:
        name = "discord"

        def __init__(self, world: World) -> None:
            self._client = world
            self.calls: list[tuple] = []

        def _target(self, chat_id: Any, metadata: Any) -> FakeChannel:
            target = (metadata or {}).get("thread_id") or chat_id
            return self._client.get_channel(int(target))

        async def send(self, chat_id: str, content: str, reply_to: Optional[str] = None,
                       metadata: Optional[dict[str, Any]] = None) -> SendResult:
            if self._client.send_hook is not None:
                injected = self._client.send_hook(self, chat_id, content, reply_to, metadata)
                if injected is not None:
                    return injected
            channel = self._target(chat_id, metadata)
            if channel is None:
                return SendResult(success=False, error=f"Channel {(metadata or {}).get('thread_id') or chat_id} not found")
            self.calls.append(("send", str(channel.id), content, reply_to, metadata))
            message = await channel.send(content=content, reference=reply_to)
            return SendResult(success=True, message_id=str(message.id), raw_response={"message_ids": [str(message.id)]})

        async def edit_message(self, chat_id: str, message_id: str, content: str, *, finalize: bool = False,
                               metadata: Optional[dict[str, Any]] = None) -> SendResult:
            if self._client.edit_hook is not None:
                injected = self._client.edit_hook(self, chat_id, message_id, content, finalize)
                if injected is not None:
                    return await injected
            channel = self._client.get_channel(int(chat_id))
            self.calls.append(("edit", str(chat_id), str(message_id), content, finalize))
            try:
                await channel.get_partial_message(int(message_id)).edit(content=content)
            except LookupError as exc:
                return SendResult(success=False, error=str(exc))
            return SendResult(success=True, message_id=str(message_id))

        async def send_multiple_images(self, chat_id: str, images: list, metadata: Optional[dict] = None,
                                       human_delay: float = 0.0) -> SendResult:
            channel = self._client.get_channel(int((metadata or {}).get("thread_id") or chat_id))
            self.calls.append(("images", str(channel.id), images))
            message = await channel.send(content=None, files=list(images))
            return SendResult(success=True, message_id=str(message.id))

        async def send_voice(self, chat_id: str, audio_path: str, caption: Optional[str] = None,
                             reply_to: Optional[str] = None, metadata: Optional[dict] = None,
                             is_voice: Optional[bool] = None, **kwargs: Any) -> SendResult:
            channel = self._client.get_channel(int((metadata or {}).get("thread_id") or chat_id))
            self.calls.append(("voice", str(channel.id), audio_path, reply_to, kwargs))
            message = await channel.send(file=audio_path, reference=reply_to)
            return SendResult(success=True, message_id=str(message.id))

        async def send_video(self, chat_id: str, video_path: str, caption: Optional[str] = None,
                             reply_to: Optional[str] = None, metadata: Optional[dict] = None) -> SendResult:
            channel = self._client.get_channel(int((metadata or {}).get("thread_id") or chat_id))
            self.calls.append(("video", str(channel.id), video_path))
            message = await channel.send(file=video_path)
            return SendResult(success=True, message_id=str(message.id))

        async def send_document(self, chat_id: str, file_path: str, caption: Optional[str] = None,
                                file_name: Optional[str] = None, reply_to: Optional[str] = None,
                                metadata: Optional[dict] = None) -> SendResult:
            channel = self._client.get_channel(int((metadata or {}).get("thread_id") or chat_id))
            self.calls.append(("document", str(channel.id), file_path))
            message = await channel.send(file=file_path)
            return SendResult(success=True, message_id=str(message.id))

        @staticmethod
        def _message_reference_from_ids(message_id: Any, channel: Any) -> tuple:
            return ("reference", str(message_id), str(channel.id))

        # Processing hooks in Hermes' shape: reactions guarded by hasattr, durable state always recorded.
        def _reactions_enabled(self) -> bool:
            return True

        async def _add_reaction(self, message: Any, emoji: str) -> bool:
            await message.add_reaction(emoji)
            return True

        async def _remove_reaction(self, message: Any, emoji: str) -> bool:
            await message.remove_reaction(emoji)
            return True

        def _record_discord_processing_start(self, event: Any, *, emoji_ack: bool) -> None:
            self.calls.append(("record-start", str(getattr(event.raw_message, "id", "")), emoji_ack))

        def _record_discord_processing_complete(self, event: Any, outcome: Any) -> None:
            self.calls.append(("record-complete", str(getattr(event.raw_message, "id", "")), outcome))

        async def on_processing_start(self, event: Any) -> None:
            message = event.raw_message
            acked = False
            if self._reactions_enabled() and hasattr(message, "add_reaction"):
                acked = await self._add_reaction(message, "eyes")
            await asyncio.to_thread(self._record_discord_processing_start, event, emoji_ack=acked)

        async def on_processing_complete(self, event: Any, outcome: Any) -> None:
            await asyncio.to_thread(self._record_discord_processing_complete, event, outcome)
            if not self._reactions_enabled():
                return
            message = event.raw_message
            if hasattr(message, "add_reaction"):
                await self._remove_reaction(message, "eyes")
                if outcome == ProcessingOutcome.SUCCESS:
                    await self._add_reaction(message, "check")

    return FakeDiscordAdapter


def guild_config(worklog_channel: Any = None, *, archive_minutes: Any = 1440, final_to_origin: Any = True,
                 state: str = "active", bot_name: str = "") -> dict[str, Any]:
    """A guild configuration in the stored shape (DESIGN §5.3), only the groups delivery reads."""
    return {
        "state": state,
        "identity": {"bot_name": bot_name},
        "worklog": {"channel": None if worklog_channel is None else str(worklog_channel),
                    "archive_minutes": archive_minutes, "final_to_origin": final_to_origin},
    }


class Configs:
    """``get_guild_config(bot_id, guild_id)`` over a dict; counts calls."""

    def __init__(self) -> None:
        self.by_guild: dict[tuple[str, str], dict[str, Any]] = {}
        self.calls = 0

    def set(self, guild_id: str, config: dict[str, Any], bot_id: str = BOT) -> None:
        self.by_guild[(bot_id, guild_id)] = config

    def __call__(self, bot_id: str, guild_id: str) -> Optional[dict[str, Any]]:
        self.calls += 1
        return self.by_guild.get((str(bot_id), str(guild_id)))


def make_route(anchor: Any, message: FakeMessage, *, lane: str = "operator", mode: str = "direct",
               register: bool = True, bot_id: str = BOT) -> Any:
    route = anchor.routes.Route(bot_id=bot_id, guild_id=str(message.channel.guild.id),
                                channel_id=str(message.channel.id), message_id=str(message.id),
                                author_id=str(message.author.id), lane=lane, mode=mode)
    if register:
        anchor.RUNTIME.message_routes.put(route.key, route)
    return route


def runner_metadata(route: Any, **extra: Any) -> dict[str, Any]:
    """What Hermes' runner puts into reply metadata for a guild source (scope + parent anchors)."""
    return {"scope_id": route.guild_id, **extra}


class Scene:
    """One test world: Discord double, guild configurations, a fresh adapter class with K3/K4 installed."""

    def __init__(self, anchor: Any, *, install: bool = True) -> None:
        self.anchor = anchor
        self.world = World()
        self.configs = Configs()
        self.cls = make_adapter_class()
        self.split = anchor.mod("delivery.split")
        self.worklog = anchor.mod("delivery.worklog")
        self.media = anchor.mod("delivery.media")
        self.reactions = anchor.mod("delivery.reactions")
        self.results = self.split.install(self.cls, self.configs, anchor.RUNTIME.status) if install else []
        self.adapter = self.cls(self.world)

    def guild(self, guild_id: str, *, worklog: bool = True, **config: Any) -> SimpleNamespace:
        """A guild with a general channel and (optionally) a worklog channel, configured."""
        general = self.world.channel(self.world.new_id(), guild_id)
        work = self.world.channel(self.world.new_id(), guild_id) if worklog else None
        self.configs.set(guild_id, guild_config(work.id if work is not None else None, **config))
        return SimpleNamespace(id=guild_id, general=general, work=work)

    def route(self, message: FakeMessage, **kwargs: Any) -> Any:
        return make_route(self.anchor, message, **kwargs)

    def in_turn(self, route: Any) -> Any:
        """Context manager: the runner's route context for one turn."""
        runtime = self.anchor.RUNTIME

        class _Turn:
            def __enter__(self_inner) -> Any:
                self_inner.token = runtime.route.set(route)
                return route

            def __exit__(self_inner, *exc: Any) -> None:
                runtime.route.reset(self_inner.token)

        return _Turn()

    async def complete(self, message: FakeMessage, outcome: str = ProcessingOutcome.SUCCESS,
                       adapter: Any = None) -> None:
        """Hermes ends the turn of ``message`` (base adapter ``on_processing_complete``)."""
        event = SimpleNamespace(raw_message=message, message_id=str(message.id),
                                source=SimpleNamespace(chat_id=str(message.channel.id), message_id=str(message.id),
                                                       user_id=str(message.author.id)))
        await (adapter or self.adapter).on_processing_complete(event, outcome)

    def threads_of(self, channel: FakeChannel) -> list[FakeChannel]:
        return [c for c in self.world.channels.values() if c.parent_id == channel.id]
