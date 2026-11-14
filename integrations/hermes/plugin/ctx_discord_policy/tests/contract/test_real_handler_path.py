"""S3 contract: a public member's message through the REAL Discord handler into the wrapped runner.

Real ``_discord_message_admission`` and ``_handle_message`` (Hermes builds the MessageEvent, strips the
mention and types ``/…`` text as a gateway command); the runner wrapper must drop a public command before
Hermes' own command dispatch, while an ordinary public message carries its route into the runner.
"""

from __future__ import annotations

import asyncio
import sys
from types import SimpleNamespace

import pytest
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, load_in

GUILD, CHANNEL, BOT, OPERATOR, MEMBER = ("900000000000000100", "900000000000000200", "900000000000000300",
                                         "900000000000000400", "900000000000000500")


@pytest.fixture
def setup(hermes, tmp_path, monkeypatch):
    _evict(ANCHOR)
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "root"))
    homes = Homes(tmp_path)
    (homes.root / "config.yaml").write_text(
        f"plugins:\n  enabled: [{PLUGIN}]\n  entries:\n    {PLUGIN}:\n      settings:\n"
        f"        operators: [\"{OPERATOR}\"]\n", encoding="utf-8")
    import discord
    from gateway.config import Platform
    from gateway.platforms.helpers import MessageDeduplicator
    from gateway.run import GatewayRunner
    from plugins.platforms.discord.adapter import DiscordAdapter

    saved = {(cls, name): cls.__dict__.get(name) for cls in (GatewayRunner, DiscordAdapter)
             for name in ("_handle_message", "_is_user_authorized", "_discord_message_admission")}
    load_in(homes.root)
    runner = GatewayRunner.__new__(GatewayRunner)
    # Real session-key derivation (GatewayRunner._session_key_for_source, config fallback without a store).
    runner.config = SimpleNamespace(multiplex_profiles=False, profile_routes=[], group_sessions_per_user=True,
                                    thread_sessions_per_user=False)
    runner._resolve_profile_home_for_source = lambda source: homes.root
    runner._profile_name_for_source = lambda source, adapter_profile=None: None
    adapter = DiscordAdapter.__new__(DiscordAdapter)
    adapter.platform = Platform.DISCORD
    adapter.config = None
    adapter._owner_profile = None
    adapter.gateway_runner = runner
    adapter._dedup = MessageDeduplicator()
    bot_user = SimpleNamespace(id=int(BOT), bot=True)
    adapter._client = SimpleNamespace(user=bot_user, application_id=int(BOT))
    adapter._allowed_user_ids = {"900000000000000999"}
    adapter._allowed_role_ids = set()
    stubs = dict(
        _is_pairing_approved_user=lambda _uid: False, _get_parent_channel_id=lambda _c: None,
        _warn_if_fail_closed_default=lambda: None, _get_allowed_channels=lambda: set(),
        _get_ignored_channels=lambda: set(), _discord_free_response_channels=lambda: set(),
        _discord_require_mention=lambda: True, _voice_text_channels={}, _in_bot_thread=lambda m: False,
        _get_no_thread_channels=lambda: set(), _extra_or_env_flag=lambda *a, **k: True,
        _discord_free_response_auto_thread=lambda: False, _get_effective_topic=lambda ch, is_thread=False: None,
        _discord_history_backfill=lambda: False, _resolve_channel_skills=lambda *a: None,
        _resolve_channel_prompt=lambda *a: None, _text_batch_delay_seconds=0,
        _threads=SimpleNamespace(mark_async=lambda _t: asyncio.sleep(0)))
    for name, value in stubs.items():
        setattr(adapter, name, value)

    async def media(_attachments):
        return [], [], [], None

    adapter._collect_attachment_media = media
    captured = []

    async def capture(event):
        captured.append(event)

    adapter.handle_message = capture
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    token = set_hermes_home_override(str(homes.root))
    try:
        sys.modules[f"{ANCHOR}.adapt.interventions"].discord_platform_handler(
            SimpleNamespace(application_id=int(BOT)), adapter)
    finally:
        reset_hermes_home_override(token)
    runtime = sys.modules[f"{ANCHOR}.runtime"].RUNTIME
    sys.modules[f"{ANCHOR}.store.ops"].bind(runtime.bots.for_bot(BOT).store, BOT, GUILD, by="cli")

    def message(author_id, mid, text):
        return SimpleNamespace(
            id=mid, author=SimpleNamespace(id=int(author_id), bot=False, display_name="member", name="member"),
            guild=SimpleNamespace(id=int(GUILD), name="g"),
            channel=SimpleNamespace(id=int(CHANNEL), type=0, parent_id=None, name="general",
                                    guild=SimpleNamespace(id=int(GUILD), name="g")),
            type=discord.MessageType.default, mentions=[bot_user], content=f"<@{BOT}> {text}", reference=None,
            attachments=[], message_snapshots=[], created_at=None)

    yield SimpleNamespace(adapter=adapter, runner=runner, captured=captured, message=message, runtime=runtime,
                          GatewayRunner=GatewayRunner)
    for (cls, name), value in saved.items():
        if value is None:
            if name in cls.__dict__:
                delattr(cls, name)
        else:
            setattr(cls, name, value)
    _evict(ANCHOR)


async def _through_handler(s, author_id, mid, text):
    msg = s.message(author_id, mid, text)
    admitted, role = s.adapter._discord_message_admission(msg, claim=True)
    assert admitted
    await s.adapter._handle_message(msg, role_authorized=role)
    return s.captured[-1]


async def test_public_text_command_is_dropped_before_hermes_dispatch(setup):
    event = await _through_handler(setup, MEMBER, 7301, "/restart")
    assert event.get_command() == "restart"  # what Hermes would dispatch
    reached = []

    async def original(self, ev):  # stands in for Hermes' dispatch behind the wrapper
        reached.append(ev)
        return "dispatched"

    wrapper = sys.modules[f"{ANCHOR}.adapt.interventions"]._make_runner_handle(original)
    assert await wrapper(setup.runner, event) is None and reached == []
    authority = sys.modules[f"{ANCHOR}.policy.authority"]
    assert not authority.authorizes(event.source)  # revoked: no later check finds the route


async def test_public_conversation_reaches_the_runner_with_its_route(setup):
    event = await _through_handler(setup, MEMBER, 7302, "what is up?")
    assert not event.get_command()
    seen = []

    async def original(self, ev):
        seen.append(setup.runtime.route.get())
        return "ran"

    wrapper = sys.modules[f"{ANCHOR}.adapt.interventions"]._make_runner_handle(original)
    assert await wrapper(setup.runner, event) == "ran"
    assert seen and seen[0].lane == "public" and seen[0].message_id == "7302"
    assert setup.GatewayRunner._is_user_authorized(setup.runner, event.source) is True


def test_shared_session_configuration_closes_public_admission_with_real_session_keys(setup):
    """With group_sessions_per_user: false Hermes derives one key for every member of a channel (real
    derivation); public admission then stays closed while the operator keeps working."""
    admission = sys.modules[f"{ANCHOR}.ingress.admission"]
    member, operator = setup.message(MEMBER, 7401, "hello"), setup.message(OPERATOR, 7402, "hello")
    assert admission.decide(setup.adapter, member).lane == "public"
    setup.runner.config.group_sessions_per_user = False
    assert admission.decide(setup.adapter, member) is None
    assert admission.decide(setup.adapter, operator).lane == "operator"
    assert "group_sessions_per_user" in setup.runtime.status.get("A4").reason
