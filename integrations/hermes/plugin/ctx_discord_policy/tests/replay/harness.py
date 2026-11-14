"""Offline interaction replay harness for discord.py 2.7.1 (spike L3).

The harness feeds raw ``INTERACTION_CREATE`` gateway payloads into the real
dispatch path of a ``discord.Client`` (``ConnectionState.parsers``, the same
table the gateway uses) and records every outgoing request instead of sending
it. No network, no bot token, no gateway connection.

Two transport seams are faked:

* Interaction responses and followups do NOT go through ``client.http``; they
  go through ``discord.webhook.async_.async_context`` (a ``ContextVar`` holding
  an ``AsyncWebhookAdapter``). The harness binds a recording adapter to that
  context variable inside a private ``contextvars.Context`` for each replay,
  so every task spawned by the dispatch inherits it and nothing leaks.
* ``client.http.request`` (the bot REST client) is replaced on the instance by
  a recorder that refuses to send. The aiohttp session of the client is never
  created.

Errors: discord.py catches exceptions of command, view and modal callbacks and
only logs them (``on_error``). The harness attaches a log handler to the
``discord`` logger for the duration of each replay and records every ERROR
record (with its exception, if any) in ``task_errors``, next to exceptions of
tasks the dispatch spawned. A test asserting ``task_errors == []`` therefore
also proves that no callback failed.

Limits: a task created *before* ``replay()`` (a worker started earlier) does
not see the recording adapter; its interaction response fails inside
discord.py with an ``AttributeError`` (no session) and is not recorded. Work
scheduled with ``loop.call_later`` is not awaited by ``_settle``.

All ids used by the payload builders are obviously fake snowflakes.
"""

from __future__ import annotations

import asyncio
import contextvars
import copy
import itertools
import json
import logging
from dataclasses import dataclass, field
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple, Union

import discord
from discord import app_commands
from discord.ext import commands
from discord.http import Route
from discord.webhook.async_ import AsyncWebhookAdapter, async_context

__all__ = [
    'APP_ID',
    'GUILD_ID',
    'OTHER_GUILD_ID',
    'CHANNEL_ID',
    'ADMIN_USER_ID',
    'MEMBER_USER_ID',
    'ADMIN_ROLE_ID',
    'MESSAGE_ID',
    'COMMAND_ID',
    'OfflineRequestError',
    'RecordedRequest',
    'RecordingWebhookAdapter',
    'ReplayHarness',
    'ReplayTimeout',
    'app_command_payload',
    'button_row',
    'component_payload',
    'interaction_payload',
    'member_payload',
    'message_payload',
    'modal_submit_payload',
    'role_payload',
    'user_payload',
]

# Obviously fake snowflakes. None of them belongs to a real Discord object.
APP_ID = 111111111111111111
GUILD_ID = 222222222222222222
OTHER_GUILD_ID = 333333333333333333
CHANNEL_ID = 444444444444444444
ADMIN_USER_ID = 555555555555555555
MEMBER_USER_ID = 666666666666666666
ADMIN_ROLE_ID = 777777777777777777
MESSAGE_ID = 888888888888888888
COMMAND_ID = 999999999999999999

_INTERACTION_IDS = itertools.count(1000000000000000001)
_RESPONSE_MESSAGE_IDS = itertools.count(1100000000000000001)

PermissionsLike = Union[discord.Permissions, int]

# Permission bit fields as ints (immutable defaults).
NO_PERMISSIONS = 0
DEFAULT_APP_PERMISSIONS = discord.Permissions(send_messages=True, embed_links=True).value


def _perm_value(permissions: PermissionsLike) -> int:
    return permissions.value if isinstance(permissions, discord.Permissions) else int(permissions)


# --------------------------------------------------------------------------- #
# Recording transport
# --------------------------------------------------------------------------- #


class OfflineRequestError(RuntimeError):
    """Raised when code under test tries a bot REST call the harness does not serve."""


class ReplayTimeout(AssertionError):
    """Raised when tasks spawned by a replay do not settle in time."""


@dataclass
class RecordedRequest:
    """One outgoing request captured by the harness."""

    transport: str  # 'webhook' (interaction/webhook adapter) or 'http' (bot REST client)
    method: str
    path: str  # route template, e.g. '/interactions/{webhook_id}/{webhook_token}/callback'
    url: str
    route_params: Dict[str, Any] = field(default_factory=dict)
    payload: Optional[Dict[str, Any]] = None
    query: Optional[Dict[str, Any]] = None
    file_count: int = 0

    @property
    def is_interaction_callback(self) -> bool:
        return self.path == '/interactions/{webhook_id}/{webhook_token}/callback'

    @property
    def callback_type(self) -> Optional[int]:
        """Interaction callback type (4 message, 6 deferred update, 9 modal, ...)."""
        if not self.is_interaction_callback or self.payload is None:
            return None
        return self.payload.get('type')

    @property
    def data(self) -> Dict[str, Any]:
        """The ``data`` object of an interaction callback (empty dict otherwise)."""
        if self.payload is None:
            return {}
        return self.payload.get('data') or {}


def _payload_from_multipart(multipart: Optional[Sequence[Dict[str, Any]]]) -> Optional[Dict[str, Any]]:
    for part in multipart or ():
        if part.get('name') == 'payload_json':
            return json.loads(part['value'])
    return None


def _fake_message(*, channel_id: int, content: Optional[str], webhook_id: Any, flags: int = 0) -> Dict[str, Any]:
    """Minimal message object returned for webhook message routes."""
    return {
        'id': str(next(_RESPONSE_MESSAGE_IDS)),
        'channel_id': str(channel_id),
        'webhook_id': str(webhook_id) if webhook_id is not None else None,
        'author': user_payload(APP_ID, username='replay-bot', bot=True),
        'content': content or '',
        'timestamp': '2026-01-01T00:00:00.000000+00:00',
        'edited_timestamp': None,
        'tts': False,
        'mention_everyone': False,
        'mentions': [],
        'mention_roles': [],
        'attachments': [],
        'embeds': [],
        'pinned': False,
        'type': 20,
        'flags': flags,
        'components': [],
    }


class RecordingWebhookAdapter(AsyncWebhookAdapter):
    """``AsyncWebhookAdapter`` whose single network method records instead of sending.

    Every adapter method in ``discord/webhook/async_.py`` (``create_interaction_response``,
    ``execute_webhook``, ``edit_original_interaction_response`` ...) funnels into
    ``AsyncWebhookAdapter.request``; overriding it captures them all.
    """

    def __init__(self, sink: List[RecordedRequest], *, channel_id: int = CHANNEL_ID) -> None:
        super().__init__()
        self._sink = sink
        self._channel_id = channel_id

    async def request(  # type: ignore[override]
        self,
        route: Route,
        session: Any,
        *,
        payload: Optional[Dict[str, Any]] = None,
        multipart: Optional[List[Dict[str, Any]]] = None,
        proxy: Optional[str] = None,
        proxy_auth: Any = None,
        files: Optional[Sequence[discord.File]] = None,
        reason: Optional[str] = None,
        auth_token: Optional[str] = None,
        params: Optional[Dict[str, Any]] = None,
    ) -> Any:
        body = payload if payload is not None else _payload_from_multipart(multipart)
        self._sink.append(
            RecordedRequest(
                transport='webhook',
                method=route.method,
                path=route.path,
                url=route.url,
                route_params={'webhook_id': route.webhook_id, 'webhook_token': route.webhook_token},
                payload=body,
                query=dict(params) if params else None,
                file_count=len(files or ()),
            )
        )
        return self._respond(route, body)

    def _respond(self, route: Route, body: Optional[Dict[str, Any]]) -> Any:
        if route.method == 'DELETE':
            return None
        if route.path == '/interactions/{webhook_id}/{webhook_token}/callback':
            body = body or {}
            response_type = body.get('type')
            flags = (body.get('data') or {}).get('flags', 0) or 0
            interaction: Dict[str, Any] = {
                'id': str(route.webhook_id),
                'type': 0,  # interaction type is not known here and not read by discord.py
                'response_message_loading': response_type == 5,
                'response_message_ephemeral': bool(flags & 64),
            }
            if response_type in (4, 5):
                interaction['response_message_id'] = str(next(_RESPONSE_MESSAGE_IDS))
            return {'interaction': interaction}
        content = (body or {}).get('content')
        flags = (body or {}).get('flags', 0) or 0
        return _fake_message(channel_id=self._channel_id, content=content, webhook_id=route.webhook_id, flags=flags)


# --------------------------------------------------------------------------- #
# Harness
# --------------------------------------------------------------------------- #


def _is_background_task(task: asyncio.Task[Any]) -> bool:
    """Long-lived library tasks that a replay must not wait for (view timeouts)."""
    coro = task.get_coro()
    qualname = getattr(coro, '__qualname__', '') or ''
    return 'timeout_task_impl' in qualname


class CallbackError(RuntimeError):
    """An ERROR logged by discord.py during a replay (callback exceptions are caught and logged there)."""


class _ErrorCollector(logging.Handler):
    def __init__(self, sink: List[BaseException]) -> None:
        super().__init__(level=logging.ERROR)
        self._sink = sink

    def emit(self, record: logging.LogRecord) -> None:
        error = record.exc_info[1] if record.exc_info else None
        self._sink.append(error if error is not None else CallbackError(record.getMessage()))


class ReplayHarness:
    """Replays raw interaction payloads through a real discord.py client, offline.

    Usage::

        async with ReplayHarness() as h:
            h.tree.add_command(cmd, guild=discord.Object(GUILD_ID))
            sent = await h.replay(app_command_payload('policy', ...))
            assert sent[0].callback_type == 9
    """

    def __init__(
        self,
        client: Optional[discord.Client] = None,
        *,
        application_id: int = APP_ID,
        settle_timeout: float = 2.0,
    ) -> None:
        if client is None:
            client = commands.Bot(
                command_prefix=commands.when_mentioned,
                intents=discord.Intents.default(),
                application_id=application_id,
            )
        self.client: discord.Client = client
        self.settle_timeout = settle_timeout
        self.requests: List[RecordedRequest] = []
        self.task_errors: List[BaseException] = []
        self.adapter = RecordingWebhookAdapter(self.requests)
        self._tree: Optional[app_commands.CommandTree[Any]] = getattr(client, 'tree', None)
        if self._tree is None:
            self._tree = app_commands.CommandTree(client)
        self._install_http_recorder()

    # -- lifecycle --------------------------------------------------------- #

    async def __aenter__(self) -> 'ReplayHarness':
        await self.start()
        return self

    async def __aexit__(self, *exc: Any) -> None:
        await self.close()

    async def start(self) -> None:
        # Public entry that binds the running loop to client/http/state
        # (Client.__aenter__ -> Client._async_setup_hook). No login, no gateway.
        await self.client.__aenter__()
        self._set_client_user()

    def _set_client_user(self) -> None:
        # Mirrors the two lines of ConnectionState.parse_ready that set the bot
        # user. Feeding a real READY payload instead would start _delay_ready,
        # which waits guild_ready_timeout for GUILD_CREATE events. Interaction
        # parsing needs the bot user (Guild.me reads state.user.id).
        state = self.state
        user = discord.ClientUser(state=state, data=user_payload(APP_ID, username='replay-bot', bot=True))
        state.user = user
        state._users[user.id] = user

    async def close(self) -> None:
        await self.client.__aexit__(None, None, None)

    # -- registration helpers --------------------------------------------- #

    @property
    def tree(self) -> app_commands.CommandTree[Any]:
        assert self._tree is not None
        return self._tree

    @property
    def state(self) -> Any:
        """The client's ``ConnectionState`` (private attribute ``Client._connection``)."""
        return self.client._connection

    def add_dynamic_items(self, *items: Any) -> None:
        self.client.add_dynamic_items(*items)

    def seed_guild(
        self,
        guild_id: int = GUILD_ID,
        *,
        name: str = 'replay-guild',
        roles: Iterable[Tuple[int, str, PermissionsLike]] = (),
        everyone_permissions: PermissionsLike = NO_PERMISSIONS,
    ) -> discord.Guild:
        """Put a guild with roles into the client cache, as a GUILD_CREATE would.

        Without this, ``Interaction`` builds an "unavailable" guild from the
        partial ``guild`` object of the payload, which carries no roles, so
        ``Member.get_role`` / ``Member.roles`` resolve to nothing.
        """
        # The @everyone role shares the guild's id.
        role_list = [role_payload(guild_id, '@everyone', everyone_permissions, position=0)]
        for position, (role_id, role_name, perms) in enumerate(roles, start=1):
            role_list.append(role_payload(role_id, role_name, perms, position=position))
        data = {
            'id': str(guild_id),
            'name': name,
            'owner_id': str(APP_ID),
            'roles': role_list,
            'emojis': [],
            'stickers': [],
            'features': [],
            'channels': [],
            'member_count': 2,
            'preferred_locale': 'en-US',
        }
        guild = discord.Guild(data=data, state=self.state)  # type: ignore[arg-type]
        self.state._add_guild(guild)
        return guild

    # -- replay ------------------------------------------------------------ #

    async def replay(self, payload: Mapping[str, Any]) -> List[RecordedRequest]:
        """Dispatch one INTERACTION_CREATE payload and return the requests it caused."""
        start = len(self.requests)
        before = asyncio.all_tasks()
        parser = self.state.parsers['INTERACTION_CREATE']
        ctx = contextvars.copy_context()
        ctx.run(async_context.set, self.adapter)
        # Tasks created inside ctx.run copy ctx, so they all see the recording adapter.
        handler = _ErrorCollector(self.task_errors)
        discord_logger = logging.getLogger('discord')
        discord_logger.addHandler(handler)
        try:
            ctx.run(parser, copy.deepcopy(dict(payload)))
            await self._settle(before)
        finally:
            discord_logger.removeHandler(handler)
        return self.requests[start:]

    async def _settle(self, before: set) -> None:
        loop = asyncio.get_running_loop()
        deadline = loop.time() + self.settle_timeout
        seen: set = set()
        while True:
            await asyncio.sleep(0)
            spawned = [t for t in asyncio.all_tasks() if t not in before and not _is_background_task(t)]
            for task in spawned:
                if task.done() and task not in seen:
                    seen.add(task)
                    self._collect(task)
            pending = [t for t in spawned if not t.done()]
            if not pending:
                return
            remaining = deadline - loop.time()
            if remaining <= 0:
                names = ', '.join(t.get_name() for t in pending)
                raise ReplayTimeout(f'replay did not settle within {self.settle_timeout}s: {names}')
            await asyncio.wait(pending, timeout=remaining)

    def _collect(self, task: asyncio.Task[Any]) -> None:
        if task.cancelled():
            return
        error = task.exception()
        if error is not None:
            self.task_errors.append(error)

    # -- bot REST client --------------------------------------------------- #

    def _install_http_recorder(self) -> None:
        sink = self.requests

        async def request(route: Route, *, files: Any = None, form: Any = None, **kwargs: Any) -> Any:
            sink.append(
                RecordedRequest(
                    transport='http',
                    method=route.method,
                    path=route.path,
                    url=route.url,
                    route_params={
                        'channel_id': route.channel_id,
                        'guild_id': route.guild_id,
                        'webhook_id': route.webhook_id,
                    },
                    payload=kwargs.get('json'),
                    query=kwargs.get('params'),
                    file_count=len(files or ()),
                )
            )
            raise OfflineRequestError(f'offline replay: {route.method} {route.path} is not served')

        # Instance attribute shadows HTTPClient.request (discord/http.py).
        self.client.http.request = request  # type: ignore[method-assign]


# --------------------------------------------------------------------------- #
# Payload builders (fields discord.py 2.7.1 reads)
# --------------------------------------------------------------------------- #


def user_payload(user_id: int, *, username: Optional[str] = None, bot: bool = False) -> Dict[str, Any]:
    data: Dict[str, Any] = {
        'id': str(user_id),
        'username': username or f'replay-user-{str(user_id)[-4:]}',
        'global_name': None,
        'discriminator': '0',
        'avatar': None,
        'public_flags': 0,
    }
    if bot:
        data['bot'] = True
    return data


def role_payload(role_id: int, name: str, permissions: PermissionsLike, *, position: int = 1) -> Dict[str, Any]:
    return {
        'id': str(role_id),
        'name': name,
        'color': 0,
        'colors': {'primary_color': 0, 'secondary_color': None, 'tertiary_color': None},
        'hoist': False,
        'icon': None,
        'unicode_emoji': None,
        'position': position,
        'permissions': str(_perm_value(permissions)),
        'managed': False,
        'mentionable': False,
        'flags': 0,
    }


def member_payload(
    user_id: int,
    *,
    permissions: PermissionsLike,
    roles: Sequence[int] = (),
    username: Optional[str] = None,
) -> Dict[str, Any]:
    """Guild member object as sent in interactions (``permissions`` is pre-resolved by Discord)."""
    return {
        'user': user_payload(user_id, username=username),
        'roles': [str(r) for r in roles],
        'joined_at': '2026-01-01T00:00:00.000000+00:00',
        'premium_since': None,
        'nick': None,
        'avatar': None,
        'banner': None,
        'pending': False,
        'flags': 0,
        'deaf': False,
        'mute': False,
        'communication_disabled_until': None,
        'permissions': str(_perm_value(permissions)),
    }


def _channel_payload(channel_id: int, guild_id: int, permissions: PermissionsLike) -> Dict[str, Any]:
    return {
        'id': str(channel_id),
        'type': 0,
        'guild_id': str(guild_id),
        'name': 'replay-channel',
        'position': 0,
        'parent_id': None,
        'topic': None,
        'nsfw': False,
        'rate_limit_per_user': 0,
        'last_message_id': None,
        'permission_overwrites': [],
        'flags': 0,
        'permissions': str(_perm_value(permissions)),
    }


def interaction_payload(
    interaction_type: int,
    data: Optional[Dict[str, Any]],
    *,
    guild_id: int = GUILD_ID,
    user_id: int = MEMBER_USER_ID,
    permissions: PermissionsLike = NO_PERMISSIONS,
    roles: Sequence[int] = (),
    channel_id: int = CHANNEL_ID,
    message: Optional[Dict[str, Any]] = None,
    app_permissions: PermissionsLike = DEFAULT_APP_PERMISSIONS,
    locale: str = 'en-US',
) -> Dict[str, Any]:
    """Guild ``INTERACTION_CREATE`` payload; ``type``: 2 command, 3 component, 5 modal submit."""
    interaction_id = next(_INTERACTION_IDS)
    payload: Dict[str, Any] = {
        'id': str(interaction_id),
        'application_id': str(APP_ID),
        'type': interaction_type,
        'token': f'replay-token-{interaction_id}',
        'version': 1,
        'guild_id': str(guild_id),
        'guild': {'id': str(guild_id), 'locale': 'en-US', 'features': []},
        'channel_id': str(channel_id),
        'channel': _channel_payload(channel_id, guild_id, permissions),
        'member': member_payload(user_id, permissions=permissions, roles=roles),
        'app_permissions': str(_perm_value(app_permissions)),
        'locale': locale,
        'guild_locale': 'en-US',
        'entitlements': [],
        'authorizing_integration_owners': {'0': str(guild_id)},
        'context': 0,
        'attachment_size_limit': 10485760,
    }
    if data is not None:
        payload['data'] = data
    if message is not None:
        payload['message'] = message
    return payload


def app_command_payload(
    name: str,
    *,
    command_guild_id: Optional[int] = GUILD_ID,
    options: Optional[List[Dict[str, Any]]] = None,
    command_id: int = COMMAND_ID,
    **kwargs: Any,
) -> Dict[str, Any]:
    """APPLICATION_COMMAND (type 2) for a chat-input command.

    ``command_guild_id`` is ``data.guild_id``: the guild the command is registered
    in (absent for global commands). discord.py routes on this field, not on
    the interaction's ``guild_id``.
    """
    data: Dict[str, Any] = {'id': str(command_id), 'name': name, 'type': 1}
    if command_guild_id is not None:
        data['guild_id'] = str(command_guild_id)
    if options:
        data['options'] = options
    return interaction_payload(2, data, **kwargs)


def button_row(*buttons: Tuple[str, str]) -> Dict[str, Any]:
    """Action row of primary buttons, each given as ``(custom_id, label)``."""
    return {
        'type': 1,
        'components': [{'type': 2, 'style': 1, 'label': label, 'custom_id': cid} for cid, label in buttons],
    }


def message_payload(
    components: List[Dict[str, Any]],
    *,
    message_id: int = MESSAGE_ID,
    channel_id: int = CHANNEL_ID,
    content: str = 'replay message',
    flags: int = 0,
) -> Dict[str, Any]:
    """Bot message carrying components; needed for component interactions."""
    return {
        'id': str(message_id),
        'channel_id': str(channel_id),
        'author': user_payload(APP_ID, username='replay-bot', bot=True),
        'content': content,
        'timestamp': '2026-01-01T00:00:00.000000+00:00',
        'edited_timestamp': None,
        'tts': False,
        'mention_everyone': False,
        'mentions': [],
        'mention_roles': [],
        'attachments': [],
        'embeds': [],
        'pinned': False,
        'type': 0,
        'flags': flags,
        'components': components,
    }


def component_payload(
    custom_id: str,
    *,
    component_type: int = 2,
    message: Optional[Dict[str, Any]] = None,
    **kwargs: Any,
) -> Dict[str, Any]:
    """MESSAGE_COMPONENT (type 3). By default the message carries a button with ``custom_id``."""
    if message is None:
        message = message_payload([button_row((custom_id, 'replay'))])
    data = {'custom_id': custom_id, 'component_type': component_type}
    return interaction_payload(3, data, message=message, **kwargs)


def modal_submit_payload(
    custom_id: str,
    values: Mapping[str, str],
    *,
    labelled: bool = True,
    **kwargs: Any,
) -> Dict[str, Any]:
    """MODAL_SUBMIT (type 5); ``values`` maps text input custom_id -> submitted text.

    ``labelled=True`` emits Label (type 18) wrappers, as Discord does for modals
    built from ``discord.ui.Label``; otherwise legacy action rows (type 1).
    """
    components: List[Dict[str, Any]] = []
    component_ids = itertools.count(1)
    for input_id, value in values.items():
        text_input = {'type': 4, 'id': next(component_ids), 'custom_id': input_id, 'value': value}
        if labelled:
            components.append({'type': 18, 'id': next(component_ids), 'component': text_input})
        else:
            components.append({'type': 1, 'id': next(component_ids), 'components': [text_input]})
    data = {'custom_id': custom_id, 'components': components}
    return interaction_payload(5, data, **kwargs)
