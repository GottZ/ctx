"""S8 contract: where the /policy interface meets Hermes, executed against real Hermes source per ref.

1. The platform-handler factory is called as ``factory(native, adapter)`` (``_wire_plugin_handlers``).
2. The discord.py Hermes ships offers the full interface strategy (K6 probe on a real ``commands.Bot``).
3. Hermes' own slash-command sync works on global commands only: the guild command ``/policy`` changes
   neither its fingerprint nor what its ``safe`` sync creates or deletes (DESIGN §12, OPEN L1).
"""

from __future__ import annotations

import asyncio
import importlib
import types

import pytest

pytestmark = pytest.mark.contract

BOT = "900000000000000001"
GUILD = 900000000000000002


def test_platform_handler_factory_receives_native_and_adapter(hermes, monkeypatch):
    base = importlib.import_module("gateway.platforms.base")
    plugins = importlib.import_module("hermes_cli.plugins")
    calls = []

    def factory(native, adapter):
        calls.append((native, adapter))

    manager = types.SimpleNamespace(get_platform_handler_factories=lambda platform: [(factory, "ctx_discord_policy")])
    monkeypatch.setattr(plugins, "get_plugin_manager", lambda: manager)
    adapter = types.SimpleNamespace(platform=types.SimpleNamespace(value="discord"), name="discord",
                                    _plugin_handler_native=None, _plugin_handlers_wired=None)
    native = object()
    base.BasePlatformAdapter._wire_plugin_handlers(adapter, native)
    base.BasePlatformAdapter._wire_plugin_handlers(adapter, native)        # idempotent per native client
    assert calls == [(native, adapter)]
    hermes.write_report("ui-factory", {"factory_args": ["native", "adapter"], "idempotent": True})


def _bot():
    discord = importlib.import_module("discord")
    from discord.ext import commands

    return discord, commands.Bot(command_prefix="!", intents=discord.Intents.none(), application_id=int(BOT))


def test_shipped_discord_py_supports_the_full_interface(hermes, anchor):
    discord, bot = _bot()
    caps = anchor.mod("ui.commands").probe(discord, bot)
    hermes.write_report("ui-k6", {"discord_py": discord.__version__, "mode": caps.mode, "reason": caps.reason})
    assert caps.mode == "full", caps.reason


def test_hermes_command_sync_ignores_the_guild_command(hermes, anchor, monkeypatch):
    adapter_mod = importlib.import_module("plugins.platforms.discord.adapter")
    discord, bot = _bot()
    adapter = object.__new__(adapter_mod.DiscordAdapter)
    adapter._client = bot
    monkeypatch.setattr(adapter, "_sleep_between_command_sync_mutations", lambda: asyncio.sleep(0),
                        raising=False)
    before = adapter._desired_command_sync_fingerprint()

    config = anchor.mod("config")
    config.set_reader(lambda home, key, default: default)
    try:
        binding = anchor.mod("ui.commands").install(bot, adapter, bot_id=BOT, owner_home=anchor.home,
                                                    schedule=False)
        assert binding is not None
        binding.ensure_command(GUILD)
        assert bot.tree.get_command("policy", guild=discord.Object(GUILD)) is not None
        assert adapter._desired_command_sync_fingerprint() == before

        mutations = []

        async def fetch_commands(*, guild=None):
            assert guild is None
            return []

        def record(name):
            async def call(*args, **kwargs):
                mutations.append((name, args))
                return {}
            return call

        monkeypatch.setattr(bot.tree, "fetch_commands", fetch_commands)
        for name in ("upsert_global_command", "delete_global_command", "edit_global_command",
                     "bulk_upsert_global_commands", "upsert_guild_command", "delete_guild_command",
                     "bulk_upsert_guild_commands"):
            monkeypatch.setattr(bot.http, name, record(name), raising=False)
        summary = asyncio.run(adapter._safe_sync_slash_commands())
    finally:
        config.set_reader(None)
    hermes.write_report("ui-core-sync", {"summary": summary, "mutations": [m[0] for m in mutations]})
    assert mutations == [] and summary.get("created", 0) == 0 and summary.get("deleted", 0) == 0
