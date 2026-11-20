"""S8 contract: where the /policy interface meets Hermes, executed against real Hermes source per ref.

1. The platform-handler factory is called as ``factory(native, adapter)`` (``_wire_plugin_handlers``).
2. The discord.py Hermes ships offers the full interface strategy (K6 probe on a real ``commands.Bot``).
3. Hermes' own slash-command sync works on global commands only: the guild command ``/policy`` changes
   neither its fingerprint nor what its ``safe`` sync creates or deletes (DESIGN §12, OPEN L1). The sync runs
   on a logged-in client, as in the gateway, against a local stand-in for Discord's REST API.
4. S9: the control kinds of the settings panels (selects with emojis, channel and role selects with
   preselected ids, disabled buttons on fixed rows) and the upload form (file field and checkbox in labels)
   build with the discord.py Hermes ships.
"""

from __future__ import annotations

import asyncio
import datetime
import importlib
import io
import json
import re
import socket
import ssl
import types

import pytest

pytestmark = pytest.mark.contract

BOT = "900000000000000001"
GUILD = 900000000000000002
TOKEN = "contract-test-token"


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


def _bot(**options):
    discord = importlib.import_module("discord")
    from discord.ext import commands

    return discord, commands.Bot(command_prefix="!", intents=discord.Intents.none(), application_id=int(BOT),
                                 **options)


def test_shipped_discord_py_supports_the_full_interface(hermes, anchor):
    discord, bot = _bot()
    caps = anchor.mod("ui.commands").probe(discord, bot)
    hermes.write_report("ui-k6", {"discord_py": discord.__version__, "mode": caps.mode, "reason": caps.reason})
    assert caps.mode == "full", caps.reason


class _DiscordRest:
    """Local HTTPS stand-in for the Discord REST routes a bot's login and command sync use.

    The bot reaches it through discord.py's public ``connector`` option: every host name resolves to this
    server, so nothing leaves the machine, and discord.py's own request path (login, rate limits, JSON) runs
    unchanged. Every request is recorded as ``(method, path)`` without the ``/api/vN`` prefix. Command
    lists answer empty, a command write is answered as stored (with id, application id and version, which
    discord.py parses after a bulk sync), anything else is 404.
    """

    def __init__(self, tmp_path):
        self.tmp_path = tmp_path
        self.requests = []
        self.port = 0
        self._runner = None
        user = {"id": BOT, "username": "policy-contract", "discriminator": "0", "avatar": None, "bot": True}
        self._fixed = {
            "/users/@me": user,
            "/oauth2/applications/@me": {"id": BOT, "name": "policy-contract", "description": "", "icon": None,
                                         "bot_public": False, "bot_require_code_grant": False, "owner": user,
                                         "verify_key": "0" * 64, "flags": 0},
        }

    async def __aenter__(self):
        from aiohttp import web

        tls = self._tls()
        app = web.Application()
        app.router.add_route("*", "/{tail:.*}", self._handle)
        self._runner = web.AppRunner(app)
        await self._runner.setup()
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.bind(("127.0.0.1", 0))
        self.port = sock.getsockname()[1]
        await web.SockSite(self._runner, sock, ssl_context=tls).start()
        return self

    async def __aexit__(self, *exc):
        await self._runner.cleanup()

    def connector(self):
        """A connector for ``discord.Client(connector=...)``; it verifies no certificate (``ssl=False``), which
        only ever meets the throwaway one here because every host name resolves to the loopback server."""
        import aiohttp
        from aiohttp.abc import AbstractResolver

        server_port = self.port

        class Loopback(AbstractResolver):
            async def resolve(self, host, port=0, family=socket.AF_INET):
                return [{"hostname": host, "host": "127.0.0.1", "port": server_port, "family": socket.AF_INET,
                         "proto": 0, "flags": socket.AI_NUMERICHOST}]

            async def close(self):
                return None

        return aiohttp.TCPConnector(resolver=Loopback(), ssl=False, limit=0)

    def _tls(self):
        from cryptography import x509
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import ec
        from cryptography.x509.oid import NameOID

        key = ec.generate_private_key(ec.SECP256R1())
        name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "discord.com")])
        now = datetime.datetime.now(datetime.timezone.utc)
        cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name).public_key(key.public_key())
                .serial_number(x509.random_serial_number()).not_valid_before(now - datetime.timedelta(minutes=5))
                .not_valid_after(now + datetime.timedelta(hours=1)).sign(key, hashes.SHA256()))
        pem = self.tmp_path / "discord-rest.pem"
        pem.write_bytes(cert.public_bytes(serialization.Encoding.PEM) + key.private_bytes(
            serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(pem)
        return context

    async def _handle(self, request):
        from aiohttp import web

        path = re.sub(r"^/api/v\d+", "", request.path)
        body = await request.read()
        self.requests.append((request.method, path))
        if request.method == "GET" and path in self._fixed:
            status, payload = 200, self._fixed[path]
        elif "/commands" not in path:
            status, payload = 404, {"message": f"no stand-in for {request.method} {path}", "code": 0}
        elif request.method == "DELETE":
            return web.Response(status=204)
        elif request.method == "GET":
            status, payload = 200, []
        else:
            written = json.loads(body or b"null")
            stored = [{"id": str(index), "application_id": BOT, "version": "1", **command}
                      for index, command in enumerate(written if isinstance(written, list) else [written], 1)]
            status, payload = 200, stored if isinstance(written, list) else stored[0]
        # discord.py parses a body as JSON only for exactly this content type (discord/http.py json_or_text).
        return web.Response(status=status, body=json.dumps(payload).encode(),
                            headers={"Content-Type": "application/json"})


def test_hermes_command_sync_ignores_the_guild_command(hermes, anchor, monkeypatch, tmp_path):
    adapter_mod = importlib.import_module("plugins.platforms.discord.adapter")
    config = anchor.mod("config")

    async def sync():
        async with _DiscordRest(tmp_path) as rest:
            discord, bot = _bot(connector=rest.connector())
            async with bot:
                # Hermes syncs from on_ready, after client.start() logged the client in (DiscordAdapter.connect).
                await bot.login(TOKEN)
                adapter = object.__new__(adapter_mod.DiscordAdapter)
                adapter._client = bot
                monkeypatch.setattr(adapter, "_sleep_between_command_sync_mutations", lambda: asyncio.sleep(0),
                                    raising=False)
                before = adapter._desired_command_sync_fingerprint()

                binding = anchor.mod("ui.commands").install(bot, adapter, bot_id=BOT, owner_home=anchor.home,
                                                            schedule=False)
                assert binding is not None
                binding.ensure_command(GUILD)
                assert bot.tree.get_command("policy", guild=discord.Object(GUILD)) is not None
                assert adapter._desired_command_sync_fingerprint() == before

                logged_in = len(rest.requests)
                summary = await adapter._safe_sync_slash_commands()
            return summary, rest.requests[logged_in:]

    config.set_reader(lambda home, key, default: default)
    try:
        summary, requests = asyncio.run(sync())
    finally:
        config.set_reader(None)
    mutations = [f"{method} {path}" for method, path in requests if method != "GET"]
    reads = [path for method, path in requests if method == "GET" and path.endswith("/commands")]
    hermes.write_report("ui-core-sync", {"summary": summary, "mutations": mutations, "registry_reads": reads})
    assert all(path == f"/applications/{BOT}/commands" for path in reads), reads     # the global registry only
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
