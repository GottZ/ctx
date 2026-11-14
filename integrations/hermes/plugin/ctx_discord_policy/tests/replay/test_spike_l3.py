"""Spike L3: replay Discord interactions offline through discord.py's real dispatch path.

Each test builds a real ``commands.Bot``, registers commands/items, feeds raw
INTERACTION_CREATE payloads into ``ConnectionState.parsers['INTERACTION_CREATE']``
and asserts on the requests captured by the recording transport.
"""

from __future__ import annotations

import logging
from typing import Any, List, Tuple

import pytest

discord = pytest.importorskip('discord')

from discord import app_commands  # noqa: E402

from tests.replay.harness import (  # noqa: E402
    ADMIN_ROLE_ID,
    APP_ID,
    ADMIN_USER_ID,
    GUILD_ID,
    MEMBER_USER_ID,
    OTHER_GUILD_ID,
    ReplayHarness,
    app_command_payload,
    component_payload,
    modal_submit_payload,
)

pytestmark = [pytest.mark.asyncio, pytest.mark.replay]

MANAGE_GUILD = discord.Permissions(view_channel=True, send_messages=True, manage_guild=True)
PLAIN_MEMBER = discord.Permissions(view_channel=True, send_messages=True)
POLICY_INPUT_ID = 'policy_text'
MODAL_TITLE = 'Server policy'

# Side effects observed by callbacks; cleared before every test.
EVENTS: List[Tuple[Any, ...]] = []


@pytest.fixture(autouse=True)
def _clear_events() -> None:
    EVENTS.clear()


# --------------------------------------------------------------------------- #
# Code under test (stand-in for the plugin's UI layer)
# --------------------------------------------------------------------------- #


def policy_modal_custom_id(guild_id: int, user_id: int) -> str:
    return f'dsp:1:modal:{guild_id}:{user_id}'


class PolicyModal(discord.ui.Modal):
    def __init__(self, *, guild_id: int, user_id: int) -> None:
        super().__init__(title=MODAL_TITLE, custom_id=policy_modal_custom_id(guild_id, user_id), timeout=None)
        self.text = discord.ui.TextInput(
            custom_id=POLICY_INPUT_ID,
            style=discord.TextStyle.paragraph,
            max_length=1000,
        )
        self.add_item(discord.ui.Label(text='Policy text', component=self.text))

    async def on_submit(self, interaction: discord.Interaction) -> None:
        EVENTS.append(('modal_submit', interaction.user.id, interaction.guild_id, self.text.value))
        await interaction.response.send_message(f'Saved {len(self.text.value)} characters.', ephemeral=True)


class PolicyButton(
    discord.ui.DynamicItem[discord.ui.Button],
    template=r'dsp:1:(?P<guild_id>[0-9]{15,20}):(?P<action>[a-z_]{1,32})',
):
    def __init__(self, guild_id: int, action: str) -> None:
        super().__init__(
            discord.ui.Button(
                label=action,
                style=discord.ButtonStyle.primary,
                custom_id=f'dsp:1:{guild_id}:{action}',
            )
        )
        self.guild_id = guild_id
        self.action = action

    @classmethod
    async def from_custom_id(
        cls, interaction: discord.Interaction, item: discord.ui.Button, match: Any, /
    ) -> 'PolicyButton':
        EVENTS.append(('button_from_custom_id', int(match['guild_id']), match['action']))
        return cls(int(match['guild_id']), match['action'])

    async def interaction_check(self, interaction: discord.Interaction, /) -> bool:
        allowed = interaction.guild_id == self.guild_id and interaction.permissions.manage_guild
        EVENTS.append(('button_check', interaction.user.id, interaction.guild_id, allowed))
        return allowed

    async def callback(self, interaction: discord.Interaction) -> None:
        EVENTS.append(('button_callback', interaction.user.id, interaction.guild_id, self.action))
        await interaction.response.send_message(f'Action {self.action} accepted.', ephemeral=True)


def make_policy_command() -> app_commands.Command:
    @app_commands.command(name='policy', description='Edit the server policy')
    @app_commands.default_permissions(manage_guild=True)
    @app_commands.guild_only()
    async def policy(interaction: discord.Interaction) -> None:
        perms = interaction.permissions
        EVENTS.append(('policy', interaction.user.id, interaction.guild_id, perms.manage_guild))
        if not perms.manage_guild:
            await interaction.response.send_message('Manage Server is required.', ephemeral=True)
            return
        await interaction.response.send_modal(PolicyModal(guild_id=interaction.guild_id, user_id=interaction.user.id))

    return policy


def policy_harness() -> ReplayHarness:
    harness = ReplayHarness()
    harness.tree.add_command(make_policy_command(), guild=discord.Object(id=GUILD_ID))
    return harness


def assert_ephemeral_message(request: Any) -> None:
    assert request.transport == 'webhook'
    assert request.method == 'POST'
    assert request.callback_type == 4
    assert request.data['flags'] == 64


# --------------------------------------------------------------------------- #
# Case 1: guild command, permissions from the payload
# --------------------------------------------------------------------------- #


async def test_case1_registration_is_guild_scoped_with_default_permissions() -> None:
    async with policy_harness() as h:
        cmd = h.tree.get_command('policy', guild=discord.Object(id=GUILD_ID))
        assert cmd is not None
        assert h.tree.get_command('policy') is None  # not global
        assert cmd.default_permissions == discord.Permissions(manage_guild=True)
        # This is the sync payload field; discord.py only sends it, Discord enforces it.
        assert cmd.to_dict(h.tree)['default_member_permissions'] == discord.Permissions(manage_guild=True).value


async def test_case1_command_with_manage_guild_reaches_callback() -> None:
    async with policy_harness() as h:
        sent = await h.replay(app_command_payload('policy', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))

        assert EVENTS == [('policy', ADMIN_USER_ID, GUILD_ID, True)]
        assert [r.callback_type for r in sent] == [9]
        assert h.task_errors == []
        assert not h.client.http._HTTPClient__session  # aiohttp session never created


async def test_case1_command_without_manage_guild_still_reaches_callback() -> None:
    """default_permissions is not enforced by discord.py; the callback must guard itself."""
    async with policy_harness() as h:
        sent = await h.replay(app_command_payload('policy', user_id=MEMBER_USER_ID, permissions=PLAIN_MEMBER))

        assert EVENTS == [('policy', MEMBER_USER_ID, GUILD_ID, False)]
        assert len(sent) == 1
        assert_ephemeral_message(sent[0])
        assert sent[0].data['content'] == 'Manage Server is required.'


# --------------------------------------------------------------------------- #
# Case 2: send_modal -> callback type 9
# --------------------------------------------------------------------------- #


async def test_case2_send_modal_records_type_9_with_label_and_text_input() -> None:
    async with policy_harness() as h:
        payload = app_command_payload('policy', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD)
        sent = await h.replay(payload)

        assert len(sent) == 1
        request = sent[0]
        assert request.method == 'POST'
        assert request.path == '/interactions/{webhook_id}/{webhook_token}/callback'
        assert request.route_params == {'webhook_id': int(payload['id']), 'webhook_token': payload['token']}
        assert request.query == {'with_response': '1'}
        assert request.callback_type == 9

        expected_custom_id = policy_modal_custom_id(GUILD_ID, ADMIN_USER_ID)
        assert request.data == {
            'custom_id': expected_custom_id,
            'title': MODAL_TITLE,
            'components': [
                {
                    'type': 18,
                    'label': 'Policy text',
                    'component': {
                        'type': 4,
                        'style': 2,
                        'label': None,
                        'custom_id': POLICY_INPUT_ID,
                        'required': True,
                        'max_length': 1000,
                    },
                }
            ],
        }
        assert len(request.data['title']) <= 45
        assert len(request.data['custom_id']) <= 100


# --------------------------------------------------------------------------- #
# Case 3: MODAL_SUBMIT -> on_submit -> ephemeral message
# --------------------------------------------------------------------------- #


async def test_case3_modal_submit_runs_on_submit_and_answers_ephemeral() -> None:
    async with policy_harness() as h:
        await h.replay(app_command_payload('policy', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))
        custom_id = policy_modal_custom_id(GUILD_ID, ADMIN_USER_ID)

        submit = modal_submit_payload(
            custom_id,
            {POLICY_INPUT_ID: 'No spam in #general.'},
            user_id=ADMIN_USER_ID,
            permissions=MANAGE_GUILD,
        )
        sent = await h.replay(submit)

        assert EVENTS[-1] == ('modal_submit', ADMIN_USER_ID, GUILD_ID, 'No spam in #general.')
        assert len(sent) == 1
        assert_ephemeral_message(sent[0])
        assert sent[0].data['content'] == 'Saved 20 characters.'
        assert h.task_errors == []

        # A successful on_submit stops the modal and removes it from the store:
        # a replayed second submit with the same custom_id is discarded silently.
        assert await h.replay(submit) == []


async def test_case3_legacy_action_row_submit_is_parsed_too() -> None:
    async with policy_harness() as h:
        await h.replay(app_command_payload('policy', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))
        submit = modal_submit_payload(
            policy_modal_custom_id(GUILD_ID, ADMIN_USER_ID),
            {POLICY_INPUT_ID: 'legacy'},
            labelled=False,
            user_id=ADMIN_USER_ID,
            permissions=MANAGE_GUILD,
        )
        sent = await h.replay(submit)

        assert EVENTS[-1] == ('modal_submit', ADMIN_USER_ID, GUILD_ID, 'legacy')
        assert [r.callback_type for r in sent] == [4]


# --------------------------------------------------------------------------- #
# Case 4: persistent DynamicItem button
# --------------------------------------------------------------------------- #


async def test_case4_dynamic_button_dispatches_from_custom_id_and_callback() -> None:
    async with ReplayHarness() as h:
        h.add_dynamic_items(PolicyButton)
        custom_id = f'dsp:1:{GUILD_ID}:approve'
        assert len(custom_id) <= 100

        sent = await h.replay(component_payload(custom_id, user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))

        assert EVENTS == [
            ('button_from_custom_id', GUILD_ID, 'approve'),
            ('button_check', ADMIN_USER_ID, GUILD_ID, True),
            ('button_callback', ADMIN_USER_ID, GUILD_ID, 'approve'),
        ]
        assert len(sent) == 1
        assert_ephemeral_message(sent[0])
        assert sent[0].data['content'] == 'Action approve accepted.'
        assert h.task_errors == []


# --------------------------------------------------------------------------- #
# Case 5: negative paths
# --------------------------------------------------------------------------- #


async def test_case5_command_from_other_guild_is_not_dispatched(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG, logger='discord')
    async with policy_harness() as h:
        payload = app_command_payload(
            'policy',
            command_guild_id=OTHER_GUILD_ID,
            guild_id=OTHER_GUILD_ID,
            user_id=ADMIN_USER_ID,
            permissions=MANAGE_GUILD,
        )
        sent = await h.replay(payload)

    assert EVENTS == []
    assert sent == []
    errors = [r for r in caplog.records if r.name == 'discord.app_commands.tree' and r.levelno == logging.ERROR]
    assert len(errors) == 1
    assert errors[0].getMessage() == 'Ignoring exception in command tree'
    assert isinstance(errors[0].exc_info[1], app_commands.CommandNotFound)


async def test_case5_unknown_command_is_not_dispatched(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG, logger='discord')
    async with policy_harness() as h:
        sent = await h.replay(app_command_payload('unknown', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))

    assert EVENTS == []
    assert sent == []
    errors = [r for r in caplog.records if r.levelno == logging.ERROR]
    assert len(errors) == 1
    error = errors[0].exc_info[1]
    assert isinstance(error, app_commands.CommandNotFound)
    assert error.name == 'unknown'
    assert [type(e) for e in h.task_errors] == [app_commands.CommandNotFound]


async def test_callback_exception_is_recorded_not_swallowed() -> None:
    """discord.py logs callback exceptions instead of raising; the harness must still see them."""
    async with ReplayHarness() as h:
        @app_commands.command(name='boom', description='raises')
        async def boom(interaction: discord.Interaction) -> None:
            raise LookupError('callback failed')

        h.tree.add_command(boom, guild=discord.Object(GUILD_ID))
        sent = await h.replay(app_command_payload('boom', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))
    assert sent == []
    assert any(isinstance(e, LookupError) or isinstance(getattr(e, 'original', None), LookupError)
               for e in h.task_errors), h.task_errors


async def test_case5_routing_key_is_data_guild_id_not_interaction_guild_id() -> None:
    """Measured: the tree looks the command up by data.guild_id only."""
    async with policy_harness() as h:
        payload = app_command_payload(
            'policy',
            command_guild_id=GUILD_ID,
            guild_id=OTHER_GUILD_ID,
            user_id=ADMIN_USER_ID,
            permissions=MANAGE_GUILD,
        )
        sent = await h.replay(payload)

    assert EVENTS == [('policy', ADMIN_USER_ID, OTHER_GUILD_ID, True)]
    assert [r.callback_type for r in sent] == [9]


async def test_case5_unknown_modal_custom_id_is_discarded(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG, logger='discord')
    async with policy_harness() as h:
        sent = await h.replay(
            modal_submit_payload('dsp:1:modal:never-opened', {POLICY_INPUT_ID: 'x'}, user_id=ADMIN_USER_ID)
        )

    assert EVENTS == []
    assert sent == []
    assert any('Modal interaction referencing unknown custom_id' in r.getMessage() for r in caplog.records)


async def test_case5_button_outside_template_is_ignored(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG, logger='discord')
    async with ReplayHarness() as h:
        h.add_dynamic_items(PolicyButton)
        sent = await h.replay(component_payload('other:1:approve', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))

    assert EVENTS == []
    assert sent == []
    assert [r for r in caplog.records if r.levelno >= logging.WARNING] == []  # silently dropped


@pytest.mark.parametrize(
    ('guild_id', 'permissions'),
    [(OTHER_GUILD_ID, MANAGE_GUILD), (GUILD_ID, PLAIN_MEMBER)],
    ids=['cross-guild', 'missing-manage-guild'],
)
async def test_case5_button_interaction_check_rejects(guild_id: int, permissions: discord.Permissions) -> None:
    async with ReplayHarness() as h:
        h.add_dynamic_items(PolicyButton)
        sent = await h.replay(
            component_payload(f'dsp:1:{GUILD_ID}:approve', guild_id=guild_id, user_id=MEMBER_USER_ID, permissions=permissions)
        )

    assert EVENTS == [
        ('button_from_custom_id', GUILD_ID, 'approve'),
        ('button_check', MEMBER_USER_ID, guild_id, False),
    ]
    assert sent == []  # no response at all: Discord would show "interaction failed"


# --------------------------------------------------------------------------- #
# Findings beyond the five cases (role checks, modal store)
# --------------------------------------------------------------------------- #


def make_role_command() -> app_commands.Command:
    @app_commands.command(name='policy-role', description='Role gated')
    @app_commands.checks.has_role(ADMIN_ROLE_ID)
    async def policy_role(interaction: discord.Interaction) -> None:
        EVENTS.append(('policy_role', interaction.user.id))
        await interaction.response.send_message('ok', ephemeral=True)

    return policy_role


@pytest.mark.parametrize(
    ('seed', 'roles', 'expected_calls'),
    [(False, [ADMIN_ROLE_ID], 0), (True, [ADMIN_ROLE_ID], 1), (True, [], 0)],
    ids=['role-unseeded-guild', 'role-seeded-guild', 'no-role-seeded-guild'],
)
async def test_role_check_needs_seeded_guild_cache(
    seed: bool, roles: List[int], expected_calls: int, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG, logger='discord')
    async with ReplayHarness() as h:
        h.tree.add_command(make_role_command(), guild=discord.Object(id=GUILD_ID))
        if seed:
            h.seed_guild(GUILD_ID, roles=[(ADMIN_ROLE_ID, 'policy-admin', MANAGE_GUILD)])
        sent = await h.replay(
            app_command_payload('policy-role', user_id=MEMBER_USER_ID, permissions=PLAIN_MEMBER, roles=roles)
        )

    assert len(EVENTS) == expected_calls
    assert len(sent) == expected_calls
    check_errors = [r.exc_info[1] for r in caplog.records if r.levelno == logging.ERROR and r.exc_info]
    if expected_calls:
        assert check_errors == []
    else:
        # The member payload carries the role id, but without the guild in the
        # client cache Member.get_role() resolves to None -> MissingRole.
        assert len(check_errors) == 1
        assert isinstance(check_errors[0], app_commands.MissingRole)


class SharedIdModal(discord.ui.Modal, title='Shared id'):
    text = discord.ui.TextInput(label='Text', custom_id='shared_text')

    def __init__(self, opened_by: int) -> None:
        super().__init__(custom_id='dsp:1:shared')
        self.opened_by = opened_by

    async def on_submit(self, interaction: discord.Interaction) -> None:
        EVENTS.append(('shared_submit', 'opened_by', self.opened_by, 'submitted_by', interaction.user.id))
        await interaction.response.send_message('ok', ephemeral=True)


async def test_modal_store_is_keyed_by_custom_id_only() -> None:
    """Two open modals with one fixed custom_id: the later one wins for both users."""

    @app_commands.command(name='shared', description='Opens a modal with a fixed custom_id')
    async def shared(interaction: discord.Interaction) -> None:
        await interaction.response.send_modal(SharedIdModal(interaction.user.id))

    async with ReplayHarness() as h:
        h.tree.add_command(shared, guild=discord.Object(id=GUILD_ID))
        await h.replay(app_command_payload('shared', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))
        await h.replay(app_command_payload('shared', user_id=MEMBER_USER_ID, permissions=PLAIN_MEMBER))
        await h.replay(
            modal_submit_payload('dsp:1:shared', {'shared_text': 'x'}, labelled=False, user_id=ADMIN_USER_ID)
        )

    assert EVENTS == [('shared_submit', 'opened_by', MEMBER_USER_ID, 'submitted_by', ADMIN_USER_ID)]


async def test_defer_then_followup_is_recorded_on_the_webhook_routes() -> None:
    @app_commands.command(name='slow', description='Defers, then follows up')
    async def slow(interaction: discord.Interaction) -> None:
        await interaction.response.defer(ephemeral=True, thinking=True)
        message = await interaction.followup.send('done', ephemeral=True, wait=True)
        EVENTS.append(('followup_message', message.content))

    async with ReplayHarness() as h:
        h.tree.add_command(slow, guild=discord.Object(id=GUILD_ID))
        sent = await h.replay(app_command_payload('slow', user_id=ADMIN_USER_ID, permissions=MANAGE_GUILD))

    assert [(r.method, r.path) for r in sent] == [
        ('POST', '/interactions/{webhook_id}/{webhook_token}/callback'),
        ('POST', '/webhooks/{webhook_id}/{webhook_token}'),
    ]
    assert sent[0].callback_type == 5
    assert sent[0].data == {'flags': 64}
    assert sent[1].payload['content'] == 'done'
    assert sent[1].payload['flags'] == 64
    assert sent[1].route_params['webhook_id'] == APP_ID  # followups go to the application webhook
    assert sent[1].query is not None and sent[1].query['wait'] == 1
    assert EVENTS == [('followup_message', 'done')]
