"""First review round — R33a (DECISIONS.md): an unsigned 404 on the first ``hello`` right after ``hermes serve``
started (lab run).

Reproduced at the source (real ``hermes serve`` under ``plugins.isolation: host``): the
routes are mounted before the port accepts and the first answer is a signed 200 (13 of 13 cold starts); an unsigned
404 ``{"detail":"Plugin not found"}`` comes when another process of the profile holds the ledger's write lock past the
busy timeout while the listener is built — ``dashboard/plugin_api.py`` logged "ctx_handover listener unavailable:
OperationalError" and answered like a missing plugin. It now answers an unsigned 503 with ``Retry-After``; the
dialer already retries 502–504, and a real 404 (not installed, inbound closed) still ends at once.
"""

from __future__ import annotations

import asyncio
import sqlite3
import time

import httpx
import pytest

from ctx_handover.engine.errors import ListenerMissing
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import GateAuth, GateMode, Peer, Right
from ctx_handover.engine.wire import WireClient
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.helpers import FakeRegistry
from ctx_handover.tests.unit.C.ledger_stub import LedgerStub, make_ledger
from ctx_handover.tests.unit.C.test_dashboard import load_plugin_api

ALL = frozenset({Right.PUSH, Right.PULL})
URL = "http://127.0.0.1:9"


@pytest.fixture
def api_module():
    import sys

    module = load_plugin_api()
    yield module
    sys.modules.pop("hermes_dashboard_plugin_ctx_handover", None)


def _app(module):
    from fastapi import FastAPI

    app = FastAPI()
    app.include_router(module.router, prefix="/api/plugins/ctx_handover")
    return app


class AsgiSync(httpx.BaseTransport):
    """The dashboard app as the dialer reaches it (sync transport over ``httpx.ASGITransport``); ``seen`` holds every
    answer's status."""

    def __init__(self, app) -> None:
        self.app, self.seen = app, []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        body = request.read()

        async def go() -> httpx.Response:
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.app), base_url="http://h") as c:
                return await c.request(
                    request.method, request.url.raw_path.decode(), headers=request.headers, content=body
                )

        resp = asyncio.run(go())
        self.seen.append(resp.status_code)
        return httpx.Response(resp.status_code, headers=resp.headers, content=resp.content)


class Listener:
    """A listener behind ``plugin_api`` whose server build fails ``busy`` times with a locked ledger (a gateway of
    the same profile holding the write lock while ``hermes serve`` starts), then succeeds."""

    def __init__(self, tmp_path, dialer_identity) -> None:
        self.busy = 0
        self.builds = 0
        self.tmp_path = tmp_path
        self.dialer_identity = dialer_identity
        self.identity = None
        self.env: dict[str, str] = {}  # the listener profile's .env survives its restarts
        self.peer_id = ulid()

    def __call__(self, eng):
        self.builds += 1
        if self.busy > 0:
            self.busy -= 1
            raise sqlite3.OperationalError("database is locked")
        ledger = LedgerStub(self.tmp_path / "listener.sqlite3", model=eng.model)
        env = self.env
        self.identity = eng.crypto.Identity.create(
            ledger, env_writable=lambda n: None, save_env=env.__setitem__, read_env_fresh=env.get
        )
        if ledger.inbound_peer(self.peer_id) is None:
            ledger.upsert_peer(
                eng.model.Peer(
                    name="laptop",
                    peer_id=self.peer_id,
                    direction="inbound",
                    url=None,
                    pubkey=self.dialer_identity.public_b64(),
                    rights=frozenset(eng.model.Right(r) for r in ("push", "pull")),
                    created_at=time.time(),
                )
            )
        registry = FakeRegistry(ledger, eng=eng, store=eng.blocks.BlockStore(self.tmp_path, ledger))
        auths = [eng.server.PeerSignatureAuth(ledger, self.identity)]
        return eng.server.Server(ledger, self.identity, registry, {"inbound": "on", "name": "server"}, auths)


def _dial_peer(listener: Listener) -> Peer:
    return Peer(
        name="server",
        peer_id=listener.peer_id,
        direction="dial",
        url=URL,
        pubkey=listener.identity.public_b64(),
        rights=ALL,
        gate=GateAuth(mode=GateMode.SESSION_TOKEN),
    )


def test_a_busy_ledger_at_the_first_build_is_ridden_out(api_module, tmp_path):
    """★ R33a: the listener's first build after a start meets a ledger another process of the profile holds locked.
    The first ``hello`` must not end the handover with ``listener_missing``: the route answers an unsigned 503 with
    ``Retry-After`` (no ``X-Ctxh-Resp-Sig``, nothing of the plugin), the dialer retries, and the next build serves
    the signed hello.

    Red on ``08cfe0f0``: the first answer was an unsigned 404 ``{"detail":"Plugin not found"}`` ⇒ ``ListenerMissing``
    (``plugin_api._server`` turned every build failure into "missing plugin").
    """
    dialer_ledger = make_ledger(tmp_path / "dialer" / "ledger.sqlite3")
    dialer = new_identity(dialer_ledger)
    listener = Listener(tmp_path, dialer)
    api_module.set_server_factory(listener)
    app = _app(api_module)
    net = AsgiSync(app)
    warm = WireClient(
        Peer("x", "", "dial", URL, "", frozenset(), gate=GateAuth(mode=GateMode.SESSION_TOKEN)),
        dialer,
        read_secret=lambda n: "tok",
        transport=net,
        max_attempts=1,
    )
    warm.hello(signed=False)  # builds once: the listener's key is known now
    peer = _dial_peer(listener)

    api_module.set_server_factory(listener)  # a fresh start: no server cached
    listener.busy = 2
    net.seen.clear()
    client = WireClient(peer, dialer, read_secret=lambda n: "tok", transport=net, sleep=lambda s: None)
    hello = client.hello(signed=True)
    assert hello.peer is not None and hello.peer.peer_id == peer.peer_id
    assert net.seen[:3] == [503, 503, 200], net.seen

    api_module.set_server_factory(listener)
    listener.busy = 1
    from fastapi.testclient import TestClient

    with TestClient(app) as tc:
        resp = tc.get("/api/plugins/ctx_handover/v1/hello")
    assert resp.status_code == 503 and resp.headers.get("retry-after") == "2"
    assert "x-ctxh-resp-sig" not in resp.headers and resp.json() == {"detail": "temporarily unavailable"}


def test_any_other_build_failure_and_a_closed_listener_still_end_at_once(api_module, tmp_path):
    """The 503 is only for a busy ledger: any other build failure (and no server at all) answers like a missing
    plugin, and the dialer reports ``listener_missing`` after one request — no waiting on a listener that is not
    there (SEC 1.10, DESIGN §12.4)."""
    dialer = new_identity(make_ledger(tmp_path / "dialer" / "ledger.sqlite3"))
    for failure in (sqlite3.OperationalError("unable to open database file"), RuntimeError("ledger unreadable"), None):

        def build(eng, failure=failure):
            if failure is not None:
                raise failure
            return None

        api_module.set_server_factory(build)
        net = AsgiSync(_app(api_module))
        peer = Peer("server", ulid(), "dial", URL, "A" * 43, ALL, gate=GateAuth(mode=GateMode.SESSION_TOKEN))
        client = WireClient(peer, dialer, read_secret=lambda n: "tok", transport=net, sleep=lambda s: None)
        with pytest.raises(ListenerMissing):
            client.hello(signed=True)
        assert net.seen == [404], (failure, net.seen)
