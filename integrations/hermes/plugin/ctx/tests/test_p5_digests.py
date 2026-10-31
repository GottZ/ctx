"""P5 (ctx-plugin-v2): segment / rolling / idle / end digests, stable root session, state pruning.

FakeClient (conftest) records every non-chain store in ``calls``; chain traffic lands in
``chain_calls``. Host lineage contract tests use the real ``hermes_state.SessionDB`` on a
tmp ``state.db`` (never the production DB).
"""

from __future__ import annotations

import json
import os
import time
from pathlib import Path

import pytest
from conftest import FakeClient, join_writes

import ctx_plugin.provider as provider_mod
from ctx_plugin import digest as dg
from ctx_plugin.config import load_ctx_config
from ctx_plugin.provider import CtxMemoryProvider


def _turns(n: int, tag: str = "a"):
    out = []
    for i in range(n):
        out.append({"role": "user", "content": f"Frage {tag}{i}: wie ist der Stand von Dienst {tag}{i}?"})
        out.append({"role": "assistant", "content": f"Dienst {tag}{i} laeuft auf Port {8000 + i}, Healthcheck gruen."})
    return out


CARRIER = {
    "role": "user",
    "content": "[CONTEXT COMPACTION] Zusammenfassung frueherer Turns ...",
    "_compressed_summary": True,
}


def _digests(client):
    return [c for c in client.calls if str(c["title"]).startswith("Session digest")]


def _provider(
    tmp_path,
    monkeypatch,
    *,
    session_id="20261020_100000_abc123",
    profile="default",
    config=None,
    agent_context="primary",
    home=None,
):
    monkeypatch.setenv("CTX_KEY", "test-key-not-real")
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx.invalid")
    monkeypatch.setattr(provider_mod, "CtxClient", FakeClient)
    home = home or (tmp_path / "profiles" / profile)
    home.mkdir(parents=True, exist_ok=True)
    if config is not None:
        (home / "ctx.json").write_text(json.dumps(config), encoding="utf-8")
    p = CtxMemoryProvider()
    p.initialize(session_id, hermes_home=str(home), agent_context=agent_context)
    return p, p._client, home


@pytest.fixture(autouse=True)
def _reset_prune_registry():
    provider_mod._PRUNED_HOMES.clear()
    yield
    provider_mod._PRUNED_HOMES.clear()


# -- Config -------------------------------------------------------------------------------


def test_config_defaults_and_coercion(tmp_path):
    cfg = load_ctx_config(str(tmp_path))
    assert (
        cfg["digest_min_exchanges"],
        cfg["digest_every_exchanges"],
        cfg["digest_every_hours"],
        cfg["idle_digest_minutes"],
        cfg["state_retention_days"],
    ) == (4, 40, 6.0, 360.0, 30)
    (tmp_path / "ctx.json").write_text(
        json.dumps(
            {
                "digest_every_exchanges": "0",
                "digest_every_hours": -3,
                "idle_digest_minutes": "x",
                "state_retention_days": 99999,
                "digest_min_exchanges": 0,
            }
        ),
        encoding="utf-8",
    )
    cfg = load_ctx_config(str(tmp_path))
    assert cfg["digest_every_exchanges"] == 0
    assert cfg["digest_every_hours"] == 0.0
    assert cfg["idle_digest_minutes"] == 360.0
    assert cfg["state_retention_days"] == 3650
    assert cfg["digest_min_exchanges"] == 1


# -- Segment digests: compaction + end ----------------------------------------------------


def test_compaction_and_end_give_two_distinct_digests(tmp_path, monkeypatch):
    p, c, _ = _provider(tmp_path, monkeypatch)
    seg1 = _turns(3, "a")
    # compaction: pre_compress (chain) then commit_memory_session → on_session_end
    p.on_pre_compress(seg1, evidence_messages=seg1)
    join_writes(p)
    p.on_session_end(seg1)
    join_writes(p)
    # conversation continues on the compacted transcript: head + carrier + tail + new turns
    seg2_msgs = [*seg1[:2], CARRIER, *seg1[-2:], *_turns(3, "b")]
    p.on_session_end(seg2_msgs)
    join_writes(p)
    d = _digests(c)
    assert len(d) == 2
    t1, t2 = d[0]["title"], d[1]["title"]
    assert t1 != t2
    assert t1.startswith("Session digest ") and " seg 001 " in t1 and " seg 002 " in t2
    assert "abc123" in t1  # root8
    for rec in d:
        assert rec["category"] == "agent-briefing"
        assert rec["tags"] == ["source:hermes", "profile:default", "hook:session_end"]
        assert not rec["content"].startswith("[CONTEXT COMPACTION")
        assert "[CONTEXT COMPACTION" not in rec["content"]
    assert d[0]["metadata"]["trigger"] == "compaction"
    assert d[0]["metadata"]["manifest_id"] == p._last_manifest_by_root[p._root_session_id]
    assert d[1]["metadata"]["trigger"] == "end"
    assert "manifest_id" not in d[1]["metadata"]
    # segment 2 covers only the new exchanges; intent = first NEW user turn (not the head)
    assert d[1]["metadata"]["exchanges"] == 6
    assert "Intent: Frage b0" in d[1]["content"]
    assert d[0]["metadata"]["segment"] == 1 and d[1]["metadata"]["segment"] == 2


def test_at_most_one_digest_per_segment(tmp_path, monkeypatch):
    """Compaction digest followed by ws_orphan_reap/soft-evict with the same history → 1 digest."""
    p, c, _ = _provider(tmp_path, monkeypatch)
    msgs = _turns(4)
    p.on_session_end(msgs)
    join_writes(p)
    p.on_session_end(msgs)
    join_writes(p)
    p.on_session_end(msgs + _turns(1, "z"))  # 2 new < 4
    join_writes(p)
    assert len(_digests(c)) == 1


def test_threshold_counts_since_last_digest(tmp_path, monkeypatch):
    p, c, _ = _provider(tmp_path, monkeypatch)
    p.on_session_end(_turns(1))  # 2 exchanges < 4
    join_writes(p)
    assert _digests(c) == []
    p.on_session_end(_turns(2))  # 4 → digest
    join_writes(p)
    assert len(_digests(c)) == 1


def test_failed_store_releases_exchanges(tmp_path, monkeypatch):
    p, c, _ = _provider(tmp_path, monkeypatch)
    c.reject_on = {0}
    msgs = _turns(2)
    p.on_session_end(msgs)
    join_writes(p)
    p.on_session_end(msgs)
    join_writes(p)
    d = _digests(c)
    assert len(d) == 2  # retry of the same exchanges, new (unique) segment title
    assert d[0]["title"] != d[1]["title"]


# -- Gating ---------------------------------------------------------------------------------


@pytest.mark.parametrize("ctx", ["cron", "subagent", "flush"])
def test_no_digest_in_cron_subagent_flush(tmp_path, monkeypatch, ctx):
    p, c, _ = _provider(
        tmp_path, monkeypatch, agent_context=ctx, config={"digest_every_exchanges": 2, "idle_digest_minutes": 0.001}
    )
    msgs = _turns(4)
    p.on_pre_compress(msgs)
    p.sync_turn("u", "a", messages=msgs)
    p.on_session_end(msgs)
    time.sleep(0.2)
    join_writes(p)
    assert _digests(c) == []
    assert p._idle_timer is None


@pytest.mark.parametrize("cfg", [{"auto_session_digest": False}, {"auto_capture": False}])
def test_switches_block_all_digest_paths(tmp_path, monkeypatch, cfg):
    p, c, _ = _provider(
        tmp_path, monkeypatch, config={**cfg, "digest_every_exchanges": 2, "idle_digest_minutes": 0.001}
    )
    msgs = _turns(4)
    p.sync_turn("u", "a", messages=msgs)
    p.on_session_end(msgs)
    time.sleep(0.2)
    join_writes(p)
    assert _digests(c) == []
    assert not (Path(p._hermes_home) / "ctx_state").exists() or not list(
        (Path(p._hermes_home) / "ctx_state").glob("*.digest.json")
    )


# -- Rolling + idle ---------------------------------------------------------------------------


def test_rolling_digest_by_exchanges_fires_once_per_threshold(tmp_path, monkeypatch):
    p, c, _ = _provider(tmp_path, monkeypatch, config={"digest_every_exchanges": 6, "idle_digest_minutes": 0})
    msgs = _turns(2)
    p.sync_turn("u", "a", messages=msgs)  # 4 < 6
    join_writes(p)
    assert _digests(c) == []
    msgs = msgs + _turns(1, "b")
    p.sync_turn("u", "a", messages=msgs)  # 6 → digest
    join_writes(p)
    msgs = msgs + _turns(1, "c")
    p.sync_turn("u", "a", messages=msgs)  # 2 new
    join_writes(p)
    d = _digests(c)
    assert len(d) == 1
    assert d[0]["metadata"]["trigger"] == "rolling"
    assert d[0]["tags"][-1] == "hook:digest_rolling"


def test_rolling_digest_by_hours(tmp_path, monkeypatch):
    p, c, _ = _provider(
        tmp_path, monkeypatch, config={"digest_every_exchanges": 0, "digest_every_hours": 6, "idle_digest_minutes": 0}
    )
    msgs = _turns(2)
    p.sync_turn("u", "a", messages=msgs)
    join_writes(p)
    assert _digests(c) == []
    p._digest_state(p._root_session_id).since_ts -= 6 * 3600 + 1
    p.sync_turn("u", "a", messages=msgs)
    join_writes(p)
    assert len(_digests(c)) == 1


def test_idle_trigger_fires_exactly_once(tmp_path, monkeypatch):
    p, c, _ = _provider(
        tmp_path,
        monkeypatch,
        config={"idle_digest_minutes": 0.002, "digest_every_exchanges": 0, "digest_every_hours": 0},
    )
    msgs = _turns(3)
    p.sync_turn("u", "a", messages=msgs)
    assert p._idle_timer is not None
    time.sleep(0.5)
    join_writes(p)
    d = _digests(c)
    assert len(d) == 1
    assert d[0]["metadata"]["trigger"] == "idle"
    assert d[0]["tags"][-1] == "hook:digest_idle"
    # idle again without new exchanges → nothing
    p.sync_turn("u", "a", messages=msgs)
    time.sleep(0.5)
    join_writes(p)
    assert len(_digests(c)) == 1
    # later end of session without new exchanges → nothing either
    p.on_session_end(msgs)
    join_writes(p)
    assert len(_digests(c)) == 1


def test_new_turn_rearms_idle_timer_and_end_cancels(tmp_path, monkeypatch):
    p, _c, _ = _provider(tmp_path, monkeypatch, config={"idle_digest_minutes": 10})
    p.sync_turn("u", "a", messages=_turns(2))
    first = p._idle_timer
    p.sync_turn("u", "a", messages=_turns(3))
    assert p._idle_timer is not first and first.finished.is_set(), "re-arm must cancel the old timer"
    p.on_session_end(_turns(3))
    join_writes(p)
    assert p._idle_timer is None
    p.shutdown()


# -- Segment counter + root stability over restart -----------------------------------------------


def _make_state_db(home: Path):
    from hermes_state import SessionDB

    db = SessionDB(home / "state.db")
    db.create_session("R-root", "desktop")
    db.end_session("R-root", "compression")
    db.create_session("C-child", "desktop", parent_session_id="R-root")
    db.end_session("C-child", "compression")
    db.create_session("G-grandchild", "desktop", parent_session_id="C-child")
    db.create_session("B-branch", "desktop", parent_session_id="C-child", model_config={"_branched_from": "C-child"})
    db.create_session("N-new", "desktop")
    return db


def test_lineage_root_matches_host_contract(tmp_path):
    home = tmp_path / "profiles" / "default"
    home.mkdir(parents=True)
    db = _make_state_db(home)
    try:
        for sid in ("R-root", "C-child", "G-grandchild", "B-branch", "N-new"):
            assert dg.resolve_lineage_root(str(home), sid) == db.get_compression_lineage(sid)[0], sid
    finally:
        db.close()
    assert dg.resolve_lineage_root(str(home), "G-grandchild") == "R-root"
    assert dg.resolve_lineage_root(str(home), "B-branch") == "B-branch"
    assert dg.resolve_lineage_root(str(home), "missing") == ""
    assert dg.resolve_lineage_root(str(tmp_path / "nohome"), "G-grandchild") == ""


def test_root_stable_after_restart_chain_dedupe_and_segments(tmp_path, monkeypatch):
    home = tmp_path / "profiles" / "default"
    home.mkdir(parents=True)
    _make_state_db(home).close()
    evidence = [
        {"role": "user", "content": "Wie ist der Stand beim Backup?"},
        {
            "role": "assistant",
            "content": "Das Backup-Ziel ist jetzt das NAS unter 10.0.0.20, Aufbewahrung 30 Tage, "
            "taeglicher Lauf um 02:00 mit Verifikation per Restore-Test.",
        },
        *_turns(2),
    ]
    # process 1: original root session
    p1, c1, _ = _provider(tmp_path, monkeypatch, session_id="R-root", home=home)
    assert p1._root_session_id == "R-root"
    p1.on_pre_compress(evidence, evidence_messages=evidence)
    join_writes(p1)
    p1.on_session_end(evidence)
    join_writes(p1)
    facts_1 = [x for x in c1.calls if x["category"] == "learnings"]
    assert facts_1
    # process 2 (restart): resumes the compression grandchild
    p2, c2, _ = _provider(tmp_path, monkeypatch, session_id="G-grandchild", home=home)
    assert p2._root_session_id == "R-root"
    assert p2._root_source == "state.db"
    p2.on_pre_compress(evidence, evidence_messages=evidence)
    join_writes(p2)
    assert [x for x in c2.calls if x["category"] == "learnings"] == []  # P4 dedupe continues
    heads = [x["title"] for x in c2.chain_calls if "head" in x["title"]]
    assert heads and all(t.endswith("R-root") for t in heads)  # P3 chain continues on the root
    p2.on_session_end(evidence + _turns(2, "n"))
    join_writes(p2)
    d = _digests(c2)
    assert len(d) == 1 and " seg 002 " in d[0]["title"]  # segment counter survived the restart
    assert d[0]["metadata"]["root_session_id"] == "R-root"


def test_switch_rules(tmp_path, monkeypatch):
    home = tmp_path / "profiles" / "default"
    home.mkdir(parents=True)
    _make_state_db(home).close()
    p, _, _ = _provider(tmp_path, monkeypatch, session_id="N-new", home=home)
    assert p._root_session_id == "N-new"
    p.on_session_switch("G-grandchild", parent_session_id="", reset=False, reason="resume")
    assert p._root_session_id == "R-root"
    p.on_session_switch("X-unknown-child", parent_session_id="G-grandchild", reset=False, reason="compression")
    assert p._root_session_id == "R-root"  # compaction keeps the root (row may not exist yet)
    p.on_session_switch("X-unknown-child", reset=False, rewound=True)
    assert p._root_session_id == "R-root"
    p.on_session_switch("B-branch", parent_session_id="C-child", reset=False, reason="branch")
    assert p._root_session_id == "B-branch"
    p.on_session_switch("Z-fresh", parent_session_id="B-branch", reset=True, reason="new_session")
    assert p._root_session_id == "Z-fresh"
    p.on_session_switch("Q-not-in-db", reset=False, reason="resume")
    assert p._root_session_id == "Z-fresh"  # no DB row → keep (pre-P5 behaviour)


def test_no_state_db_falls_back_to_session_id(tmp_path, monkeypatch):
    p, _, _ = _provider(tmp_path, monkeypatch, session_id="20261020_120000_ffffff")
    assert p._root_session_id == "20261020_120000_ffffff"
    assert p._root_source == "session_id"


# -- Pruning ---------------------------------------------------------------------------------------


def test_prune_state_dir_removes_only_old_files(tmp_path):
    state = tmp_path / "ctx_state"
    state.mkdir()
    old = time.time() - 40 * 86400
    for name in ("old-root.json", "old-root.digest.json", ".ledger-abc", "fresh.json", "notes.txt"):
        (state / name).write_text("{}", encoding="utf-8")
    for name in ("old-root.json", "old-root.digest.json", ".ledger-abc", "notes.txt"):
        os.utime(state / name, (old, old))
    assert dg.prune_state_dir(str(tmp_path), 30) == 3
    assert sorted(x.name for x in state.iterdir()) == ["fresh.json", "notes.txt"]
    assert dg.prune_state_dir(str(tmp_path), 0) == 0
    assert dg.prune_state_dir(str(tmp_path / "missing"), 30) == 0


def test_provider_prunes_once_per_home(tmp_path, monkeypatch):
    home = tmp_path / "profiles" / "default"
    state = home / "ctx_state"
    state.mkdir(parents=True)
    old = time.time() - 31 * 86400
    (state / "a.json").write_text("{}", encoding="utf-8")
    os.utime(state / "a.json", (old, old))
    _provider(tmp_path, monkeypatch, home=home)
    assert not (state / "a.json").exists()
    (state / "b.json").write_text("{}", encoding="utf-8")
    os.utime(state / "b.json", (old, old))
    _provider(tmp_path, monkeypatch, home=home)  # same process → no second sweep
    assert (state / "b.json").exists()


def test_retention_zero_disables_pruning(tmp_path, monkeypatch):
    home = tmp_path / "profiles" / "default"
    state = home / "ctx_state"
    state.mkdir(parents=True)
    old = time.time() - 400 * 86400
    (state / "a.json").write_text("{}", encoding="utf-8")
    os.utime(state / "a.json", (old, old))
    _provider(tmp_path, monkeypatch, home=home, config={"state_retention_days": 0})
    assert (state / "a.json").exists()


# -- Host contract: real MemoryManager forwards messages to sync_turn ------------------------------


def test_real_memory_manager_sync_all_reaches_rolling_digest(tmp_path, monkeypatch):
    from agent.memory_manager import MemoryManager

    p, c, _ = _provider(tmp_path, monkeypatch, config={"digest_every_exchanges": 4, "idle_digest_minutes": 0})
    assert MemoryManager._provider_sync_accepts(p, "messages")
    mm = MemoryManager()
    mm.add_provider(p)
    msgs = _turns(2)
    mm.sync_all(
        "Frage a1: wie ist der Stand von Dienst a1?", "Dienst a1 laeuft.", session_id=p._session_id, messages=msgs
    )
    assert mm.flush_pending(timeout=5.0)
    join_writes(p)
    d = _digests(c)
    assert len(d) == 1 and d[0]["metadata"]["trigger"] == "rolling"
    mm.on_session_end(msgs)  # same history → nothing new
    join_writes(p)
    assert len(_digests(c)) == 1
