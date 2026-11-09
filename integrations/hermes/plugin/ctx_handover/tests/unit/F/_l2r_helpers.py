"""Helpers of the layer-R tests of cluster F (package r-opb): an ``EngineBackend`` over a real cluster-A ``Ledger``
in a runtime object of the ``jobs.Runtime`` shape, and README text access."""

from __future__ import annotations

import re
import threading
import time
import types
from pathlib import Path

from ctx_handover.engine.commands import EngineBackend, Plugin
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import GateAuth, GateMode, Peer, Right
from ctx_handover.tests.conftest import PLUGIN_ROOT
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx

README = (PLUGIN_ROOT / "README.md").read_text(encoding="utf-8")
FLAT = re.sub(r"\s+", " ", README)

IDENTITY_SENTINEL = object()


def backend_with_runtime(fake_host, tmp_path: Path) -> tuple[EngineBackend, types.SimpleNamespace]:
    """``EngineBackend`` whose runtime holds a real ``Ledger`` (identity cache preset to a sentinel)."""
    from ctx_handover.engine.ledger import Ledger

    ctx = FakeCtx(host=fake_host)
    backend = EngineBackend(ctx, Plugin(ctx))
    ledger = Ledger(tmp_path / "f-runtime" / "ledger.sqlite3")
    runtime = types.SimpleNamespace(
        ledger=ledger,
        store=None,
        veto=None,
        identity=IDENTITY_SENTINEL,
        identity_error=None,
        lock=threading.RLock(),
    )
    backend._parts["runtime"] = runtime
    return backend, runtime


def dial_peer(name: str = "server") -> Peer:
    return Peer(
        name=name,
        peer_id=ulid(),
        direction="dial",
        url=f"https://{name}.example",
        pubkey="A" * 43,
        rights=frozenset({Right.PUSH, Right.PULL}),
        gate=GateAuth(mode=GateMode.SESSION_TOKEN),
        created_at=time.time(),
    )


def near(text: str, first: str, second: str, window: int = 400) -> bool:
    """True when ``second`` occurs within ``window`` characters of an occurrence of ``first``."""
    start = 0
    while True:
        i = text.find(first, start)
        if i < 0:
            return False
        if second in text[max(0, i - window) : i + len(first) + window]:
            return True
        start = i + 1
