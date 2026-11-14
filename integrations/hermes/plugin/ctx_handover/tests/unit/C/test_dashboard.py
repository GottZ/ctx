"""``dashboard/manifest.json`` and ``dashboard/plugin_api.py`` (R12, D4, DESIGN §2.1, §14.5; TESTPLAN §2.4)."""

from __future__ import annotations

import ast
import importlib.util
import json
import sys

import pytest

from ctx_handover.tests.conftest import PLUGIN_ROOT
from ctx_handover.tests.unit.C.helpers import FakeRegistry
from ctx_handover.tests.unit.C.ledger_stub import LedgerStub

API = PLUGIN_ROOT / "dashboard" / "plugin_api.py"


def load_plugin_api(name: str = "hermes_dashboard_plugin_ctx_handover"):
    """Import the file exactly like ``hermes_cli.plugin_isolation.import_user_code`` does."""
    spec = importlib.util.spec_from_file_location(name, API)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    except Exception:
        sys.modules.pop(name, None)
        raise
    return module


@pytest.fixture
def api_module():
    module = load_plugin_api()
    yield module
    sys.modules.pop("hermes_dashboard_plugin_ctx_handover", None)


def _app(module):
    from fastapi import FastAPI

    app = FastAPI()
    app.include_router(module.router, prefix="/api/plugins/ctx_handover")
    return app


def _factory(tmp_path, settings=None):
    def build(eng):
        ledger = LedgerStub(tmp_path / "ledger.sqlite3", model=eng.model)
        env: dict[str, str] = {}
        identity = eng.crypto.Identity.create(
            ledger, env_writable=lambda n: None, save_env=env.__setitem__, read_env_fresh=env.get
        )
        registry = FakeRegistry(ledger, eng=eng, store=eng.blocks.BlockStore(tmp_path, ledger))
        auths = [eng.server.OperatorSessionAuth(enabled=False), eng.server.PeerSignatureAuth(ledger, identity)]
        build.identity = identity
        return eng.server.Server(ledger, identity, registry, {"inbound": "on", **(settings or {})}, auths)

    return build


def test_dashboard_manifest_tab_hidden():
    manifest = json.loads((PLUGIN_ROOT / "dashboard" / "manifest.json").read_text(encoding="utf-8"))
    assert manifest == {"name": "ctx_handover", "api": "plugin_api.py", "tab": {"hidden": True}}


def test_dashboard_router_mounts_and_dispatches(api_module, tmp_path):
    from fastapi.testclient import TestClient

    build = _factory(tmp_path)
    api_module.set_server_factory(build)
    with TestClient(_app(api_module)) as client:
        resp = client.get("/api/plugins/ctx_handover/v1/hello")
        assert resp.status_code == 200
        assert resp.json()["listener_pubkey"] == build.identity.public_b64()
        assert resp.headers["X-Ctxh-Server"] == "1"
        assert client.get("/api/plugins/ctx_handover/v1/nope").status_code == 404
        assert client.post("/api/plugins/ctx_handover/v1/sessions/list", json={}).status_code == 401
        # the streaming cap of the adapter: a body over the route cap is refused with 413, signed
        big = client.post(
            "/api/plugins/ctx_handover/v1/pair/redeem",
            content=b"x" * 70_000,
            headers={"content-type": "application/json"},
        )
        assert big.status_code == 413
        assert "X-Ctxh-Resp-Sig" in big.headers


def test_dashboard_engine_loaded_by_path_as_own_package(api_module, tmp_path):
    api_module.set_server_factory(_factory(tmp_path))
    eng = api_module._engine()
    assert eng.__name__.startswith("ctx_handover_engine_")
    assert len(eng.__name__) == len("ctx_handover_engine_") + 8
    assert eng.server.__name__ == eng.__name__ + ".server"
    assert api_module._engine() is eng  # cached
    assert "ctx_handover.engine" not in eng.__name__


def test_dashboard_fails_closed_without_server(api_module, tmp_path):
    """No ``jobs.listener_server`` yet (or it raises): every route answers like a missing plugin."""
    from fastapi.testclient import TestClient

    api_module.set_server_factory(None)
    with TestClient(_app(api_module)) as client:
        resp = client.get("/api/plugins/ctx_handover/v1/hello")
    assert resp.status_code == 404
    assert resp.json() == {"detail": "Plugin not found"}

    def broken(eng):
        raise RuntimeError("ledger unreadable")

    api_module.set_server_factory(broken)
    with TestClient(_app(api_module)) as client:
        assert client.get("/api/plugins/ctx_handover/v1/hello").status_code == 404


def test_dashboard_source_has_no_shared_memory_assumptions():
    """R12/§2.1 consequence 7: no ``ctx``, no ``hermes_plugins`` lookup, no Hermes import, no relative import."""
    tree = ast.parse(API.read_text(encoding="utf-8"))
    names = {n.id for n in ast.walk(tree) if isinstance(n, ast.Name)}
    attrs = {n.attr for n in ast.walk(tree) if isinstance(n, ast.Attribute)}
    text = API.read_text(encoding="utf-8")
    assert "hermes_plugins" not in text
    assert "ctx" not in names
    assert "modules" not in attrs or "sys.modules[" not in text.replace("sys.modules[name] = module", "")
    imports = [n for n in ast.walk(tree) if isinstance(n, (ast.Import, ast.ImportFrom))]
    tops = {(a.name if isinstance(n, ast.Import) else n.module or "").split(".")[0] for n in imports for a in n.names}
    assert tops <= {
        "__future__",
        "asyncio",
        "hashlib",
        "importlib",
        "logging",
        "sys",
        "threading",
        "pathlib",
        "fastapi",
        "starlette",
        "typing",
        "collections",
    }
    assert all(not isinstance(n, ast.ImportFrom) or n.level == 0 for n in imports)


def test_dashboard_per_profile_server_cache(api_module, tmp_path, monkeypatch):
    """One server per profile home (multiplexed ``hermes serve``, DESIGN §15.4)."""
    built = []

    def factory(eng):
        built.append(eng)
        return object()

    api_module.set_server_factory(factory)
    keys = iter(["/homes/a", "/homes/b", "/homes/a"])
    monkeypatch.setattr(api_module, "_scope_key", lambda eng: next(keys))
    a1 = api_module._server()
    b = api_module._server()
    a2 = api_module._server()
    assert a1 is a2 and a1 is not b
    assert len(built) == 2
