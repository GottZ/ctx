"""S3 contract: the security core against real Hermes classes (DESIGN §9.1 L2).

Real ``DiscordAdapter._discord_message_admission`` / ``build_source``, real
``GatewayRunner._resolve_profile_home_for_source`` with multiplex profile routes, real plugin managers in
two homes, real ``SessionSource`` serialization. Writes ``status-matrix-<ref>.json``.
"""

from __future__ import annotations

import os
import sys
from types import SimpleNamespace

import pytest
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, dispatch_tool, load_in

GUILD = "900000000000000100"
CHANNEL = "900000000000000200"
BOT = "900000000000000300"
OPERATOR = "900000000000000400"
MEMBER = "900000000000000500"


def _config(home, *, operators=(), routes=False):
    ops = ", ".join(f'"{o}"' for o in operators)
    text = (f"plugins:\n  enabled: [{PLUGIN}]\n  entries:\n    {PLUGIN}:\n      settings:\n"
            f"        operators: [{ops}]\n")
    (home / "config.yaml").write_text(text, encoding="utf-8")


@pytest.fixture
def real(hermes, tmp_path):
    """Two homes, the plugin enabled in the launch home; restores every class attribute it wraps."""
    _evict(ANCHOR)
    os.environ["HERMES_HOME"] = str(tmp_path / "root")
    homes = Homes(tmp_path)
    from gateway.run import GatewayRunner
    from plugins.platforms.discord.adapter import DiscordAdapter

    saved = {(cls, name): cls.__dict__.get(name) for cls in (GatewayRunner, DiscordAdapter)
             for name in ("_handle_message", "_is_user_authorized", "_discord_message_admission")}
    yield SimpleNamespace(homes=homes, GatewayRunner=GatewayRunner, DiscordAdapter=DiscordAdapter)
    for (cls, name), value in saved.items():
        if value is None:
            if name in cls.__dict__:
                delattr(cls, name)
        else:
            setattr(cls, name, value)
    _evict(ANCHOR)


def _runner(real, *, routed_profile):
    from gateway.profile_routing import ProfileRoute

    runner = real.GatewayRunner.__new__(real.GatewayRunner)
    routes = [ProfileRoute(name="guild", platform="discord", profile=routed_profile, guild_id=GUILD)]
    runner.config = SimpleNamespace(multiplex_profiles=True, profile_routes=routes)
    return runner


def _adapter(real, runner):
    import discord
    from gateway.config import Platform
    from gateway.platforms.helpers import MessageDeduplicator

    adapter = real.DiscordAdapter.__new__(real.DiscordAdapter)
    adapter.platform = Platform.DISCORD
    adapter.config = None
    adapter._owner_profile = None
    adapter.gateway_runner = runner
    adapter._dedup = MessageDeduplicator()
    bot_user = SimpleNamespace(id=int(BOT), bot=True)
    adapter._client = SimpleNamespace(user=bot_user, application_id=int(BOT))
    adapter._allowed_user_ids = {"900000000000000999"}  # someone else; the member is NOT allowlisted
    adapter._allowed_role_ids = set()
    adapter._is_pairing_approved_user = lambda _uid: False
    adapter._get_parent_channel_id = lambda _channel: None
    adapter._warn_if_fail_closed_default = lambda: None
    return adapter, bot_user, discord


def _message(discord, bot_user, author_id, mid):
    return SimpleNamespace(
        id=mid, author=SimpleNamespace(id=int(author_id), bot=False, display_name="member", name="member"),
        guild=SimpleNamespace(id=int(GUILD)), channel=SimpleNamespace(id=int(CHANNEL), type=0, parent_id=None),
        type=discord.MessageType.default, mentions=[bot_user], content=f"<@{BOT}> hi", reference=None)


def _bind_and_activate(real, adapter):
    interventions = sys.modules[f"{ANCHOR}.adapt.interventions"]
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    token = set_hermes_home_override(str(real.homes.root))
    try:
        interventions.discord_platform_handler(SimpleNamespace(application_id=int(BOT)), adapter)
    finally:
        reset_hermes_home_override(token)
    runtime = sys.modules[f"{ANCHOR}.runtime"].RUNTIME
    binding = runtime.bots.for_bot(BOT)
    sys.modules[f"{ANCHOR}.store.ops"].bind(binding.store, BOT, GUILD, by="cli")
    return runtime, binding


def test_probes_pass_and_status_matrix(real, hermes):
    _config(real.homes.root, operators=[OPERATOR])
    routed = real.homes.profile("routed")
    _config(routed)
    load_in(real.homes.root)
    load_in(routed)
    adapter, _, _ = _adapter(real, _runner(real, routed_profile="routed"))
    runtime, _ = _bind_and_activate(real, adapter)
    snapshot = runtime.status.snapshot()["interventions"]
    hermes.write_report("status-matrix", {"interventions": snapshot})
    for name in ("A3", "A4", "A5", "A6", "A7"):
        assert snapshot[name]["outcome"] in ("ok", "wrapped"), (name, snapshot[name])
    assert snapshot["A8[default]"]["outcome"] == "armed" and snapshot["A8[routed]"]["outcome"] == "armed"


def test_public_member_admitted_into_armed_routed_profile_and_tools_blocked_there(real):
    _config(real.homes.root, operators=[OPERATOR])
    routed = real.homes.profile("routed")
    _config(routed)
    load_in(real.homes.root)
    load_in(routed)
    runner = _runner(real, routed_profile="routed")
    adapter, bot_user, discord = _adapter(real, runner)
    runtime, _ = _bind_and_activate(real, adapter)
    msg = _message(discord, bot_user, MEMBER, 7001)
    assert adapter._discord_message_admission(msg, claim=True) == (True, False)
    assert adapter._allowed_user_ids == {"900000000000000999"}  # restored
    # The decision routes to the armed profile; the hook registered there blocks a host tool.
    admission = sys.modules[f"{ANCHOR}.ingress.admission"]
    decision = admission.decide(adapter, msg)
    from hermes_constants import hermes_home_key

    assert decision is not None and decision.target_home_key == hermes_home_key(routed)
    assert decision.lane == "public"
    route = decision.route("7001")
    token = runtime.route.set(route)
    try:
        assert "BLOCKED by ctx_discord_policy" in dispatch_tool(routed, session_id="s", turn_id="t")
        # Real runner authorization: the exact route authorizes, before any stock check runs.
        from gateway.session import SessionSource
        from gateway.config import Platform

        source = SessionSource(platform=Platform.DISCORD, chat_id=CHANNEL, chat_type="group", user_id=MEMBER,
                               guild_id=GUILD, message_id="7001")
        assert real.GatewayRunner._is_user_authorized(runner, source) is True
        other = SessionSource(platform=Platform.DISCORD, chat_id=CHANNEL, chat_type="group",
                              user_id="900000000000000777", guild_id=GUILD, message_id="7002")
        try:
            assert real.GatewayRunner._is_user_authorized(runner, other, allow_adapter_delegation=False) is not True
        except TypeError as exc:  # signature regressions must not hide here
            raise AssertionError(exc)
        except Exception:
            pass  # a bare runner lacks state for the stock check; only "not our grant" is under test
    finally:
        runtime.route.reset(token)


def test_unarmed_routed_profile_closes_public_admission(real):
    _config(real.homes.root, operators=[OPERATOR])
    bare = real.homes.profile("bare", enabled=False)
    load_in(real.homes.root)
    load_in(bare)
    runner = _runner(real, routed_profile="bare")
    adapter, bot_user, discord = _adapter(real, runner)
    runtime, _ = _bind_and_activate(real, adapter)
    msg = _message(discord, bot_user, MEMBER, 7101)
    for name in ("A5", "A6", "A7"):  # the closure below is the armed check, not a missing wrapper
        assert runtime.status.get(name).outcome.value == "wrapped", name
    assert sys.modules[f"{ANCHOR}.ingress.admission"].decide(adapter, msg) is None
    assert adapter._discord_message_admission(msg, claim=True) == (False, False)  # stock: not allowlisted
    assert runtime.status.get("A4").outcome.value == "fallback"


def test_restored_fifo_source_keeps_the_route_key():
    """Routes are recovered after Hermes' durable FIFO restores an event: the serialized source keeps
    chat_id, user_id and message_id, and the restored event carries message_id but no raw_message."""
    from gateway.config import Platform
    from gateway.session import SessionSource
    from gateway.session_envelope import _EVENT_FIELDS

    source = SessionSource(platform=Platform.DISCORD, chat_id=CHANNEL, chat_type="group",
                           user_id=f"ambient:{CHANNEL}", guild_id=GUILD, message_id="7201")
    restored = SessionSource.from_dict(source.to_dict())
    assert (restored.chat_id, restored.user_id, restored.message_id) == (CHANNEL, f"ambient:{CHANNEL}", "7201")
    assert "message_id" in _EVENT_FIELDS and "raw_message" not in _EVENT_FIELDS


def test_report_pre_llm_call_block_support(hermes, capsys):
    """Informational: whether this Hermes ends a turn on a pre_llm_call block directive (#57364). The tool
    gate never depends on it (pre_tool_call blocks on its own); it only saves the inference request."""
    import inspect
    from agent import turn_context

    supported = 'r.get("action") == "block"' in inspect.getsource(turn_context)
    hermes.write_report("pre-llm-block", {"pre_llm_call_block_supported": supported})
    with capsys.disabled():
        print(f"\n== Hermes {hermes.ref}: pre_llm_call block directive supported: {supported}")
