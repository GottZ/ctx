"""Test bootstrap: load the plugin as package ``ctx_plugin`` without network.

Needs the Hermes repo on sys.path (``agent.memory_provider``,
``tools.registry``). Override with HERMES_REPO; default ~/.hermes/hermes-agent.
No ``__init__.py`` in tests/ on purpose — otherwise pytest would put the
plugin's parent dir on sys.path and ``tools`` could resolve to our tools.py.
"""

from __future__ import annotations

import importlib.util
import os
import sys
from pathlib import Path
from typing import ClassVar

import pytest

PLUGIN_DIR = Path(__file__).resolve().parent.parent / "plugin"
HERMES_REPO = Path(os.environ.get("HERMES_REPO", str(Path.home() / ".hermes" / "hermes-agent")))

if str(HERMES_REPO) not in sys.path:
    sys.path.insert(0, str(HERMES_REPO))


def _load_plugin():
    name = "ctx_plugin"
    if name in sys.modules:
        return sys.modules[name]
    spec = importlib.util.spec_from_file_location(
        name, PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)]
    )
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


_load_plugin()


class FakeClient:
    """Stands in for CtxClient. ``fail_on`` = set of 0-based store-call indices that raise."""

    instances: ClassVar[list] = []

    def __init__(self, base_url="", api_key="", timeout=10.0, synthesis_timeout=90.0, **_kw):
        self.calls: list = []
        self.fail_on: set = set()
        self.reject_on: set = set()
        self.raise_on_store = None  # exception instance raised by every store()
        self.reads: list = []  # ("query"|"search", text, kwargs)
        self.query_result = {"success": True, "sources": []}
        self.search_result = {"success": True, "results": []}
        self.query_raises = None
        # P3: transcript-chain traffic (category compaction-checkpoints) is kept apart so
        # the fact/mirror assertions of P0–P2 count only their own writes.
        self.chain_calls: list = []
        self.chain_reads: list = []
        FakeClient.instances.append(self)

    def whoami(self):
        return {"success": True, "home_scope": "shared", "label": "test-key"}

    def store(self, category, title, content, sensitivity=None, tags=None, **extra):
        idx = len(self.calls)
        record = {
            "category": category,
            "title": title,
            "content": content,
            "sensitivity": sensitivity,
            "tags": tags,
            **extra,
        }
        if category == "compaction-checkpoints":
            n = len(self.chain_calls)
            self.chain_calls.append(record)
            if self.raise_on_store is not None:
                raise self.raise_on_store
            if idx in self.fail_on:
                from ctx_plugin.client import CtxApiError

                raise CtxApiError("connection error: simulated")
            if idx in self.reject_on:
                return {"success": False, "error": "simulated rejection"}
            ts = "2026-10-20T00:00:00Z"
            return {
                "success": True,
                "block": {"id": f"019f6000-0000-7000-8000-{900000 + n:012d}", "created_at": ts, "updated_at": ts},
            }
        self.calls.append(record)
        if self.raise_on_store is not None:
            raise self.raise_on_store
        if idx in self.fail_on:
            from ctx_plugin.client import CtxApiError

            raise CtxApiError("connection error: simulated")
        if idx in self.reject_on:
            return {"success": False, "error": "simulated rejection"}
        ts = "2026-10-20T00:00:00Z"
        return {"success": True, "block": {"id": f"blk-{idx}", "created_at": ts, "updated_at": ts}}

    def query(self, text, **k):
        self.reads.append(("query", text, k))
        if self.query_raises is not None:
            raise self.query_raises
        return self.query_result

    def search(self, text, **k):
        if k.get("category") == "compaction-checkpoints":
            self.chain_reads.append(("search", text, k))
            return {"success": True, "results": []}
        self.reads.append(("search", text, k))
        return self.search_result

    def get_block(self, block_id, **k):
        self.chain_reads.append(("get", block_id, k))
        return {"success": True, "block": {"id": block_id, "metadata": {}}}


@pytest.fixture
def fake_client_cls(monkeypatch):
    import ctx_plugin.provider as provider_mod

    FakeClient.instances = []
    monkeypatch.setattr(provider_mod, "CtxClient", FakeClient)
    return FakeClient


@pytest.fixture
def make_provider(fake_client_cls, monkeypatch, tmp_path):
    """Initialized provider with FakeClient; ``profile`` controls HERMES_HOME layout."""
    from ctx_plugin.provider import CtxMemoryProvider

    monkeypatch.setenv("CTX_KEY", "test-key-not-real")
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx.invalid")

    def _make(profile: str | None = "testprof", config: dict | None = None):
        home = tmp_path / "profiles" / profile if profile else tmp_path / ".hermes"
        home.mkdir(parents=True, exist_ok=True)
        if config is not None:
            import json as _json

            (home / "ctx.json").write_text(_json.dumps(config), encoding="utf-8")
        p = CtxMemoryProvider()
        p.initialize("sess-1234", hermes_home=str(home), agent_context="primary")
        assert p._active, "provider must be active with FakeClient"
        return p, p._client

    return _make


def join_writes(provider) -> None:
    t = provider._write_thread
    if t is not None:
        t.join(timeout=5.0)
