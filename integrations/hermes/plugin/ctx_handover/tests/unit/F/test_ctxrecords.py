"""Cluster F — optional ctx records through the ledger outbox (DESIGN §13, BRIEF D12, BX M1; TESTPLAN §2.7)."""

from __future__ import annotations

import json

from ctx_handover.engine import ctxrecords
from ctx_handover.engine.model import PathMapEntry
from ctx_handover.tests.fakes.surfaces_fake import FakeCtxServer, FakeOutboxLedger

ROOT_A = "20261109_101500_aaaaaa"
ROOT_B = "20261109_141003_bbbbbb"
JOB = "01JC0000000000000000000ABC"


def _data(**over) -> dict:
    data = {
        "job_id": JOB,
        "source_root": ROOT_A,
        "target_root": ROOT_B,
        "source_session": ROOT_A,
        "target_session": ROOT_B,
        "peer": "laptop",
        "local_name": "server",
        "title": "My Work",
        "at": 1_762_696_983.0,
        "path_map": [PathMapEntry(source="/srv/app", target="/home/u/hermes-handover/app", kind="workspace")],
        "model_change": {"from": "gpt-x", "from_provider": "openrouter", "to": "qwen3", "to_provider": "llamacpp"},
        "plugin_version": "0.1.0",
    }
    data.update(over)
    return data


def test_ctxrecords_outbox_both_sides(tmp_path):
    """★ BX M1: a side finished in a process without ctx (``hermes serve``) and a side finished while ctx was
    unreachable (offline laptop) both reach ctx later, from whichever process can flush, exactly once each.

    Red proof: mutant = in ``flush`` a failed store marks the row done (``ledger.outbox_done(row_id, True, now)``,
    i.e. "record only when ctx answers at DONE") → ``assert 0 == 2`` on ``ctxrecords.pending(laptop)``: both records
    are gone before ctx was ever reachable.
    """
    db = tmp_path / "ledger.sqlite3"
    serve = FakeOutboxLedger(db)  # serve process: no ctx at all, only enqueues
    ctxrecords.enqueue(serve, "arrival", _data(), redact=lambda s: s)

    laptop = FakeOutboxLedger(db)  # offline laptop: ctx unreachable at DONE
    ctxrecords.enqueue(laptop, "departure", _data(), redact=lambda s: s)
    offline = FakeCtxServer(fail_with=ConnectionError("network is unreachable"))
    assert ctxrecords.flush(laptop, offline.call_mcp, "ctx", "handover", now=1000.0) == 0
    assert ctxrecords.pending(laptop) == 2

    gateway = FakeOutboxLedger(db)  # later: the gateway (holds a ctx) flushes after backoff
    online = FakeCtxServer()
    assert ctxrecords.flush(gateway, online.call_mcp, "ctx", "handover", now=1000.0 + 7200) == 2
    titles = sorted(call["arguments"]["title"] for call in online.stores)
    assert titles == sorted(
        [f"Handover {ROOT_A} → laptop {JOB[-6:]}", f"Handover arrival {ROOT_B} ← laptop {JOB[-6:]}"]
    )
    assert ctxrecords.pending(gateway) == 0
    assert ctxrecords.flush(gateway, online.call_mcp, "ctx", "handover", now=1000.0 + 9999) == 0
    assert len(online.stores) == 2


def test_ctxrecords_record_shape_and_checkpoint_head(tmp_path):
    """Category, tags, metadata and content of DESIGN §13; the checkpoint head of the source lineage is looked up
    by ctx_checkpoint's head title at flush time."""
    ledger = FakeOutboxLedger(tmp_path / "l.sqlite3")
    ctxrecords.enqueue(ledger, "departure", _data(), redact=lambda s: s)
    server = FakeCtxServer(heads={f"Compaction checkpoint head {ROOT_A}": "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"})
    assert ctxrecords.flush(ledger, server.call_mcp, "ctx", "handover", now=5.0) == 1
    (store,) = server.stores
    args = store["arguments"]
    assert store["server"] == "ctx" and store["tool"] == "store"
    assert args["category"] == "handover"
    assert args["tags"] == ["handover", "hermes", "peer:laptop", "direction:out"]
    meta = args["metadata"]
    assert meta["job_id"] == JOB and meta["source_root"] == ROOT_A and meta["target_root"] == ROOT_B
    assert meta["peers"] == {"from": "server", "to": "laptop"}
    assert meta["plugin_version"] == "0.1.0"
    assert "provenance" not in meta
    assert meta["checkpoint_head"] == "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
    content = args["content"]
    for needle in (
        "My Work",
        ROOT_A,
        ROOT_B,
        "/srv/app → /home/u/hermes-handover/app",
        "gpt-x",
        "qwen3",
        "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000",
    ):
        assert needle in content, needle
    assert len(content.encode("utf-8")) <= ctxrecords.RECORD_MAX_BYTES
    assert server.searches and server.searches[0]["arguments"]["query"] == f"Compaction checkpoint head {ROOT_A}"


def test_ctxrecords_detect_and_never_raise(tmp_path):
    """Detection only through ``memory.provider == ctx_checkpoint`` (server from ``mcp_server``, default ``ctx``);
    ``enqueue`` and ``flush`` never raise, whatever the ledger or ctx do (DESIGN §13 "never affects the handover")."""
    assert ctxrecords.detect({}) is None
    assert ctxrecords.detect({"memory": {"provider": "builtin"}}) is None
    assert ctxrecords.detect({"memory": {"provider": "ctx_checkpoint"}}) == "ctx"
    assert (
        ctxrecords.detect({"memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {"mcp_server": "ctx2"}}})
        == "ctx2"
    )
    assert (
        ctxrecords.detect({"memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {"mcp_server": "a b"}}}) is None
    )
    assert ctxrecords.detect({"memory": "ctx_checkpoint"}) is None
    assert ctxrecords.detect(None) is None  # type: ignore[arg-type]

    class Broken:
        def outbox_put(self, *a, **k):
            raise RuntimeError("database is locked")

        def outbox_due(self, *a, **k):
            raise RuntimeError("database is locked")

        def outbox_done(self, *a, **k):
            raise RuntimeError("database is locked")

    ctxrecords.enqueue(Broken(), "departure", _data(), redact=lambda s: s)
    assert ctxrecords.flush(Broken(), FakeCtxServer().call_mcp, "ctx", "handover", now=1.0) == 0

    ledger = FakeOutboxLedger(tmp_path / "l.sqlite3")
    ctxrecords.enqueue(ledger, "departure", _data(), redact=lambda s: 1 / 0)  # redactor fails ⇒ nothing stored raw
    assert ctxrecords.pending(ledger) == 0
    ctxrecords.enqueue(ledger, "bogus", _data(), redact=lambda s: s)
    assert ctxrecords.pending(ledger) == 0

    ctxrecords.enqueue(ledger, "arrival", _data(), redact=lambda s: s)
    denied = FakeCtxServer(fail_with=PermissionError("not in mcp_allowlist"))
    assert ctxrecords.flush(ledger, denied.call_mcp, "ctx", "handover", now=1.0) == 0
    bad_envelope = FakeCtxServer(store_result={"ok": False, "error": "rate limited"})
    assert ctxrecords.flush(ledger, bad_envelope.call_mcp, "ctx", "handover", now=99999.0) == 0
    assert ctxrecords.pending(ledger) == 1


def test_ctxrecords_redacted(tmp_path):
    """T13: the record is redacted before it is written to the outbox (the ledger never holds the raw text)."""
    ledger = FakeOutboxLedger(tmp_path / "l.sqlite3")
    secret = "sk-live-AAAABBBBCCCCDDDDEEEEFFFF"
    ctxrecords.enqueue(
        ledger, "departure", _data(title=f"deploy with {secret}"), redact=lambda s: s.replace(secret, "***")
    )
    raw = json.dumps(ledger.rows())
    assert secret not in raw
    server = FakeCtxServer()
    ctxrecords.flush(ledger, server.call_mcp, "ctx", "handover", now=1.0)
    assert secret not in json.dumps(server.stores)
    assert "deploy with ***" in server.stores[0]["arguments"]["content"]
