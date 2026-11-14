"""Fixtures of the cluster-B suites: a second real ``SessionDB`` (the target), a block store, lineages built
through the core's own write paths (``SessionBuilder``: append, batch append, in-place compaction, rotation,
rewind), and the export → import round trip."""

from __future__ import annotations

import base64
import json
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path

import pytest

from ctx_handover.engine import session_export, session_import
from ctx_handover.engine.model import HermesPaths, SessionRef
from ctx_handover.tests.unit.B.support import LedgerStub, MemoryBlockStore

TARGET_CONFIG = {"model": {"default": "target-model", "provider": "target-provider"}, "providers": {}}
SOURCE_META = {"instance_id": "SRCINSTANCE", "profile": "default", "hermes_version": "0.22.6-test"}
IMAGE_URI = "data:image/png;base64," + base64.b64encode(bytes(range(256)) * 40).decode()
DROP = frozenset({"id", "session_id", "platform_message_id"})


@pytest.fixture
def target_home(tmp_path) -> Path:
    home = tmp_path / "target-home"
    home.mkdir()
    return home


@pytest.fixture
def target_db(target_home):
    from ctx_handover.tests.conftest import require_upstream

    require_upstream()
    from hermes_state import SessionDB

    db = SessionDB(target_home / "state.db")
    try:
        yield db
    finally:
        db.close()


@pytest.fixture
def store() -> MemoryBlockStore:
    return MemoryBlockStore()


@pytest.fixture
def ledger(tmp_path) -> LedgerStub:
    stub = LedgerStub(tmp_path / "ledger.sqlite3")
    try:
        yield stub
    finally:
        stub.close()


@pytest.fixture
def source_paths(hermes_home) -> HermesPaths:
    return HermesPaths(home=str(hermes_home), document_cache=str(hermes_home / "cache" / "documents"))


@pytest.fixture
def target_cwd(tmp_path) -> Path:
    d = tmp_path / "target-work"
    d.mkdir()
    return d


def rich_lineage(b, *, cwd: str = "/srv/src-work", title: str = "My Work") -> str:
    """Root + tip built through core paths; returns the tip id.

    Root: turn-loop appends (``append_message``) with tool calls (core-stamped tool-call uids), reasoning
    fields, ``api_content``, a multimodal user turn with a data-URI image, codex items, display metadata; an
    in-place compaction with a carried tail (archived + rewind rows, cloned uids); more turns. Rotation with a
    concurrent row (clone into the child). Tip: turns, a rewind (rewind rows), more turns, the title.
    """
    sid = b.create(
        model="src-model",
        provider="src-provider",
        base_url="https://peer.example/v1",
        api_mode="chat_completions",
        cwd=cwd,
        git_repo_root=cwd,
        git_branch="main",
        model_config={"reasoning_config": {"effort": "high"}, "service_tier": "flex"},
    )
    b.user(sid, "please read the file", api_content="please read the file\n\n<memory-context>m</memory-context>")
    call = b.tool_call("read_file", {"path": f"{cwd}/a.txt"}, call_id="call_r1")
    b.assistant(
        sid,
        "",
        tool_calls=[call],
        reasoning="need the file",
        reasoning_content="rc 1",
        reasoning_details=[{"type": "reasoning.encrypted", "data": "enc1"}],
        finish_reason="tool_calls",
        token_count=17,
    )
    b.tool(
        sid, "call_r1", json.dumps({"content": "file body"}), name="read_file", effect_disposition="read", observed=True
    )
    b.assistant(
        sid,
        "done reading",
        finish_reason="stop",
        codex_reasoning_items=[{"type": "reasoning", "id": "rs_1", "encrypted_content": "e"}],
    )
    b.append(
        sid,
        [
            b.message(
                "user",
                [{"type": "text", "text": "look"}, {"type": "image_url", "image_url": {"url": IMAGE_URI}}],
                display_metadata={"k": 1},
            ),
            b.message("assistant", "seen", display_kind="note"),
        ],
    )
    b.turns(sid, 3, with_tools=True)
    b.compact_in_place(sid, keep_last=2)
    b.turns(sid, 2)
    tip = b.rotate(sid, keep_last=2, concurrent=1)
    b.turns(tip, 3, with_tools=True)
    first = b.user(tip, "this will be rewound")
    b.assistant(tip, "rewound answer")
    b.rewind(tip, first)
    b.turns(tip, 2)
    b.db.set_session_title(tip, title)
    return tip


def export(db, store, tip: str, paths: HermesPaths, *, job_id: str = "01JOBTESTAAAAAAAAAAAAAAAAA", **kw):
    info = session_export.describe(db, SessionRef(id=tip), calling_session=None)
    cap = session_export.export_capsule(
        db, info, job_id=job_id, store=store, source_meta=SOURCE_META, hermes=paths, **kw
    )
    return info, cap


def plan(db, capsule: dict, cwd: Path, **kw):
    args = {
        "cwd_for": lambda p: str(cwd),
        "model_policy": "auto",
        "model_override": None,
        "target_config": TARGET_CONFIG,
        "provider_usable": lambda name, cfg: True,
        "max_session_bytes": 512 * 1024**2,
    }
    args.update(kw)
    return session_import.plan_import(db, capsule, **args)


def roundtrip(src_db, dst_db, store, tip, paths, cwd, **plan_kw):
    info, cap = export(src_db, store, tip, paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    p = plan(dst_db, capsule, cwd, **plan_kw)
    prepared = session_import.import_hidden(
        dst_db, store, cap.capsule_id, p, local_hermes_version=SOURCE_META["hermes_version"]
    )
    return info, cap, capsule, p, prepared


def stored_rows(db, sid: str, *, skip_rewind: bool = True) -> list[dict]:
    """Every row of a segment without storage coordinates (and without rewind rows)."""
    out = []
    for r in db.get_messages(sid, include_inactive=True):
        if skip_rewind and not r.get("active") and not r.get("compacted"):
            continue
        out.append({k: v for k, v in r.items() if k not in DROP})
    return out


def norm_conv(conv: Iterable[Mapping]) -> list[dict]:
    """A model/display history without in-memory row markers (``_row_id`` etc.)."""
    out = []
    for m in conv:
        d = {k: v for k, v in m.items() if not k.startswith("_") and k not in ("message_id", "id", "session_id")}
        if m.get("_compressed_summary"):
            d["_compressed_summary"] = True
        out.append(d)
    return out


@dataclass
class Pair:
    src_ids: list[str]
    dst_ids: list[str]


def assert_lineage_equal(src_db, dst_db, src_ids, dst_ids) -> None:
    assert len(src_ids) == len(dst_ids)
    for a, b in zip(src_ids, dst_ids, strict=True):
        ra, rb = stored_rows(src_db, a), stored_rows(dst_db, b)
        assert len(ra) == len(rb), (a, b)
        pairing: dict[str, str] = {}
        for x, y in zip(ra, rb, strict=True):
            if x.get("role") == "assistant" and isinstance(x.get("tool_call_uids"), str):
                pairing.update(json.loads(x["tool_call_uids"]))
            if x.get("role") == "tool" and x.get("tool_call_uid") is None and y.get("tool_call_uid") is not None:
                # the core fills a NULL result uid from its assistant row on batch inserts (session_hash.ALL_DERIVED)
                assert y["tool_call_uid"] == pairing.get(x.get("tool_call_id")), x.get("tool_call_id")
                y = {**y, "tool_call_uid": None}
            assert x == y
    ma, da = src_db.get_resume_conversations(src_ids[-1])
    mb, dbb = dst_db.get_resume_conversations(dst_ids[-1])
    assert norm_conv(ma) == norm_conv(mb)
    assert norm_conv(da) == norm_conv(dbb)
