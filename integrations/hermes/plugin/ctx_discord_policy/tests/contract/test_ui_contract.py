"""S8 contract: where the /policy interface meets Hermes, executed against real Hermes source per ref.

1. The platform-handler factory is called as ``factory(native, adapter)`` (``_wire_plugin_handlers``).
2. The discord.py Hermes ships offers the full interface strategy (K6 probe on a real ``commands.Bot``).
3. Hermes' own slash-command sync works on global commands only: the guild command ``/policy`` changes
   neither its fingerprint nor what its ``safe`` sync creates or deletes (DESIGN §12, OPEN L1).
4. S9: the control kinds of the settings panels (selects with emojis, channel and role selects with
   preselected ids, disabled buttons on fixed rows) and the upload form (file field and checkbox in labels)
   build with the discord.py Hermes ships.
"""

from __future__ import annotations

import asyncio
import importlib
import io
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


def test_shipped_discord_py_builds_the_settings_controls(hermes, anchor):
    discord, _ = _bot()
    panels, modals = anchor.mod("ui.panels"), anchor.mod("ui.modals")
    ctx = types.SimpleNamespace(discord=discord, guild_id=str(GUILD), rev=3,
                                binding=types.SimpleNamespace(persistent=False))
    emoji = discord.PartialEmoji(name="example", id=880000000000000001)
    view = discord.ui.View(timeout=None)
    for control in (
        panels.Control("identity", "id_emoji", kind="select", row=0,
                       options=(("example", "880000000000000001", None, True, emoji),)),
        panels.Control("channels", "ch_set", kind="channel_select", arg="ambient", min_values=0, max_values=25,
                       defaults=("444444444444444445",)),
        panels.Control("managers", "mg_roles", kind="role_select", min_values=0, max_values=25,
                       defaults=("777777777777777771",)),
        panels.Control("guidance", "gd_download", "Download", disabled=True, row=3),
    ):
        view.add_item(panels._item(ctx, control))
    rows = [row["components"][0] for row in view.to_components()]
    select, channels, roles, button = rows
    assert str(select["options"][0]["emoji"]["id"]) == "880000000000000001" and select["options"][0]["default"]
    assert channels["type"] == 8
    assert [(str(v["id"]), v["type"]) for v in channels["default_values"]] == [("444444444444444445", "channel")]
    assert {discord.ChannelType(t).name for t in channels["channel_types"]} == {"text", "news", "forum"}
    assert roles["type"] == 6
    assert [(str(v["id"]), v["type"]) for v in roles["default_values"]] == [("777777777777777771", "role")]
    assert button["disabled"] is True

    spec = modals.ModalSpec("m_gd_upload", "Upload guidance file", [
        modals.Field("file", "file", "File (.md or .txt, UTF-8, up to 64 KB)", description="Its text becomes it."),
        modals.Field("replace", "checkbox", "Replace the current guidance", value=True)])
    modal = modals.build(discord, spec, guild_id=str(GUILD), rev=3, opener_id="1", modern=True)
    labels = modal.to_dict()["components"]
    assert [c["component"]["type"] for c in labels] == [19, 23]           # FileUpload, Checkbox inside Labels
    assert modal.values() == {"file": [], "replace": True}
    attachment = discord.File(io.BytesIO(b"preview"), filename="preview-public-direct.md")
    hermes.write_report("ui-s9-controls", {"discord_py": discord.__version__, "rows": len(rows),
                                           "modal_component_types": [c["component"]["type"] for c in labels],
                                           "file": attachment.filename})
