"""Target rewrite, model route, titles, tombstones, delta and side state (DESIGN §6.3–§6.8; TESTPLAN §2.3)."""

from __future__ import annotations

import json
import re

import pytest

from ctx_handover.engine import session_export, session_import
from ctx_handover.engine.errors import VerifyFailed
from ctx_handover.engine.session_hash import put_json
from ctx_handover.tests.unit.B.conftest import (
    assert_lineage_equal,
    export,
    plan,
    rich_lineage,
    roundtrip,
)

ID_RE = re.compile(r"^\d{8}_\d{6}_[0-9a-f]{6}$")

# Columns the target never writes: runtime state of the source; the imported row holds the store default.
NOT_WRITTEN = (
    "runtime_revision",
    "runtime_generation",
    "expiry_finalized",
    "git_metadata_generation",
    "transcript_revision",
    "rewind_count",
    "last_read_at",
    "transport_profile",
    "last_activity_at",
    "last_activity_description",
    "last_activity_provenance",
    "compression_failure_cooldown_until",
    "compression_failure_error",
    "compression_fallback_streak",
    "compression_ineffective_count",
    "compression_recovery_deadline",
    "compression_overload_streak",
    "handoff_state",
    "handoff_platform",
    "handoff_error",
    "created_source",
    "title_source",
    "tool_names",
    "profile_name",
)
NULLED = (
    "user_id",
    "session_key",
    "chat_id",
    "chat_type",
    "thread_id",
    "display_name",
    "origin_json",
    "system_prompt",
    "system_prompt_hash",
    "billing_provider",
    "billing_base_url",
    "billing_mode",
)
KEPT = (
    "started_at",
    "git_branch",
    "input_tokens",
    "output_tokens",
    "cache_read_tokens",
    "cache_write_tokens",
    "reasoning_tokens",
    "estimated_cost_usd",
    "actual_cost_usd",
    "cost_status",
    "cost_source",
    "pricing_version",
    "api_call_count",
    "pinned",
    "message_count",
    "tool_call_count",
)
SPECIAL = (
    "id",
    "source",
    "model",
    "model_config",
    "parent_session_id",
    "ended_at",
    "end_reason",
    "cwd",
    "git_repo_root",
    "title",
    "archived",
    "auto_archived",
    "hidden",
)
RULES = {**dict.fromkeys(NOT_WRITTEN, "default"), **dict.fromkeys(NULLED, "null"), **dict.fromkeys(KEPT, "keep")}
RULES.update(dict.fromkeys(SPECIAL, "special"))


def _gateway_lineage(b, db):
    """A gateway-adopted source lineage: routing columns, billing route, tokens, pin, a prompt; built by core calls."""
    root = b.mint_id()
    db.create_session(
        root,
        "tui",
        model="src-model",
        system_prompt="SYSTEM PROMPT OF THE SOURCE",
        user_id="uid:0",
        session_key=f"agent:main:local:dm:{root}",
        chat_id=root,
        chat_type="dm",
        thread_id="t1",
        display_name="Alice",
        origin_json=json.dumps({"platform": "local"}),
        cwd="/srv/src-work",
        git_repo_root="/srv/src-work",
        model_config={"reasoning_config": {"effort": "low"}, "_branched_from": "x", "follow_profile_config": True},
    )
    db.update_session_model(root, "src-model", "openrouter", base_url="https://evil.example/v1", api_mode="responses")
    db.update_session_cwd(root, "/srv/src-work", git_branch="feature", git_repo_root="/srv/src-work")
    db.update_token_counts(
        root,
        input_tokens=100,
        output_tokens=40,
        cache_read_tokens=7,
        reasoning_tokens=3,
        estimated_cost_usd=0.25,
        cost_status="estimated",
        cost_source="table",
        pricing_version="v9",
        billing_provider="openrouter",
        billing_base_url="https://evil.example/v1",
        billing_mode="metered",
        api_call_count=4,
    )
    db.set_session_pinned(root, True)
    b.turns(root, 3, with_tools=True)
    tip = b.rotate(root, keep_last=2)
    b.turns(tip, 2)
    db.set_session_title(tip, "Gateway Work")
    return root, tip


@pytest.fixture
def imported(make_session, session_db, target_db, store, source_paths, target_cwd):
    _root, tip = _gateway_lineage(make_session, session_db)
    info, _cap, _capsule, p, prepared = roundtrip(
        session_db,
        target_db,
        store,
        tip,
        source_paths,
        target_cwd,
        target_config={"model": {"default": "target-model", "provider": "openrouter"}},
    )
    control = "20200101_000000_c0ffee"
    assert target_db.import_sessions([{"id": control, "source": "cli", "messages": []}])["ok"]
    return {"info": info, "plan": p, "prepared": prepared, "control": target_db.get_session(control)}


def test_rule_table_covers_all_65_columns(target_db, target_home):
    import sqlite3

    conn = sqlite3.connect(target_home / "state.db")
    try:
        cols = [r[1] for r in conn.execute("PRAGMA table_info(sessions)")]
    finally:
        conn.close()
    assert len(cols) == 65
    assert set(cols) == set(RULES)


@pytest.mark.parametrize("column", sorted(RULES))
def test_rewrite_session_columns_table(imported, session_db, target_db, target_cwd, column):
    """★ Every ``sessions`` column follows DESIGN §6.4 on the imported lineage (parametrised over all 65).

    Mutant: ``rewrite_session`` keeps the peer's ``user_id``/``session_key``/``chat_id`` (the ``None`` lines removed).
    """
    p, prepared, control = imported["plan"], imported["prepared"], imported["control"]
    src_ids = imported["info"].lineage_ids
    rule = RULES[column]
    for index, (old, seg) in enumerate(zip(src_ids, p.segments, strict=True)):
        src, dst = session_db.get_session(old), target_db.get_session(seg.new_id)
        is_tip = index == len(src_ids) - 1
        value = dst[column]
        if rule == "default":
            assert value == control[column], (column, value, control[column])
        elif rule == "null":
            assert value is None, (column, value)
        elif rule == "keep":
            assert value == src[column], (column, value, src[column])
        elif column == "id":
            assert ID_RE.match(value) and value != old
        elif column == "source":
            assert value == "cli"
        elif column == "model":
            assert value == "src-model"
        elif column == "model_config":
            cfg = json.loads(value) if value else {}
            assert cfg == {"reasoning_config": {"effort": "low"}}, cfg
        elif column == "parent_session_id":
            assert value == (None if index == 0 else p.segments[index - 1].new_id)
        elif column in ("ended_at", "end_reason"):
            assert value == (None if is_tip else src[column])
        elif column in ("cwd", "git_repo_root"):
            assert value == str(target_cwd)
        elif column == "title":
            assert value == src["title"]
        elif column in ("archived", "auto_archived"):
            assert value == 0
        elif column == "hidden":
            assert value == 1
        else:  # pragma: no cover - the table above is exhaustive
            raise AssertionError(column)
    assert "evil.example" not in json.dumps(target_db.get_session(prepared.tip_id), default=str)


def test_rewrite_message_fields_table(make_session, session_db, target_db, store, source_paths, target_cwd):
    """DESIGN §6.5: new ids and session ids, ``platform_message_id`` NULL, every payload field byte-true."""
    b = make_session
    sid = b.create()
    b.user(sid, "from telegram", platform_message_id="tg-123")
    b.assistant(sid, "ok")
    _info, _cap, _capsule, _p, prepared = roundtrip(session_db, target_db, store, sid, source_paths, target_cwd)
    src = session_db.get_messages(sid, include_inactive=True)
    dst = target_db.get_messages(prepared.tip_id, include_inactive=True)
    assert src[0]["platform_message_id"] == "tg-123" and dst[0]["platform_message_id"] is None
    assert all(r["session_id"] == prepared.tip_id for r in dst)
    for a, c in zip(src, dst, strict=True):
        assert {k: v for k, v in a.items() if k not in ("id", "session_id", "platform_message_id")} == {
            k: v for k, v in c.items() if k not in ("id", "session_id", "platform_message_id")
        }
    assert_lineage_equal(session_db, target_db, [sid], [prepared.tip_id])


def test_model_config_allowlist():
    """★ ``gateway_runtime``, ``base_url``, ``api_mode``, ``_branched_from`` dropped; ``reasoning_config`` kept [R20].

    Mutant: ``map_model`` copies the whole source ``model_config``.
    """
    row = {
        "model": "m",
        "model_config": json.dumps(
            {
                "provider": "openrouter",
                "base_url": "https://evil.example/v1",
                "api_mode": "responses",
                "gateway_runtime": {"provider": "openrouter", "base_url": "https://evil.example/v1"},
                "_branched_from": "x",
                "_delegate_from": "y",
                "_reset_from": "z",
                "follow_profile_config": True,
                "reasoning_config": {"effort": "high"},
                "service_tier": "flex",
                "prompt_refresh_epoch": 3,
            }
        ),
    }
    cfg = {"model": {"default": "d", "provider": "openrouter"}}
    fields, change = session_import.map_model(row, "auto", None, cfg, lambda n, c: True)
    assert fields["model_config"] == {
        "reasoning_config": {"effort": "high"},
        "service_tier": "flex",
        "prompt_refresh_epoch": 3,
    }
    assert fields["model"] == "m" and change is None


def test_sec_peer_route_base_url_not_kept(make_session, session_db, target_db, store, source_paths, target_cwd):
    """★ Provider configured and usable on the target, peer row with a foreign ``base_url``: the target writes its
    own route only, the peer URL appears nowhere in the imported rows [R20, SEC 2.8].

    Mutant: ``map_model`` keeps the peer's route (``gateway_runtime``/``base_url``) when the provider is usable.
    """
    b = make_session
    sid = b.create(model="claude-x", provider="anthropic", base_url="https://evil.example/v1", api_mode="messages")
    b.turns(sid, 2)
    cfg = {"model": {"default": "local", "provider": "ollama"}, "providers": {"anthropic": {"api_key_env": "K"}}}
    _info, _cap, _capsule, _p, prepared = roundtrip(
        session_db, target_db, store, sid, source_paths, target_cwd, target_config=cfg
    )
    row = target_db.get_session(prepared.tip_id)
    assert row["model"] == "claude-x"
    assert json.loads(row["model_config"]) == {"gateway_runtime": {"provider": "anthropic"}}
    assert "evil.example" not in json.dumps(row, default=str)
    assert row["billing_base_url"] is None


def test_map_model_named_provider_writes_target_route_only():
    cfg = {
        "model": {"default": "d", "provider": "openai"},
        "providers": {"lan": {"base_url": "http://192.168.1.9:8080/v1"}},
    }
    row = {"model": "qwen", "model_config": {"gateway_runtime": {"provider": "lan", "base_url": "https://x.example"}}}
    fields, change = session_import.map_model(row, "auto", None, cfg, lambda n, c: n == "lan")
    assert fields["model"] == "qwen"
    assert fields["model_config"] == {"gateway_runtime": {"provider": "lan"}}
    assert change is None
    assert session_import.offline_capable(fields["model_config"], cfg) is True


def test_map_model_unusable_falls_back_to_default():
    cfg = {"model": {"default": "target-model", "provider": "target-provider"}, "providers": {"anthropic": {}}}
    row = {"model": "claude-x", "model_config": {"provider": "anthropic"}}
    fields, change = session_import.map_model(row, "auto", None, cfg, lambda n, c: False)
    assert fields["model"] == "target-model" and fields["model_config"] is None
    assert change == {
        "from": {"model": "claude-x", "provider": "anthropic"},
        "to": {"model": "target-model", "provider": "target-provider", "context_length": None},
        "reason": "provider_unusable",
        "offline_capable": False,
    }
    unknown, change2 = session_import.map_model(
        {"model": "m", "model_config": {"provider": "nowhere"}}, "auto", None, cfg, lambda n, c: True
    )
    assert unknown["model"] == "target-model" and change2["reason"] == "provider_unusable"
    forced, change3 = session_import.map_model(row, "auto", "my-model", cfg, lambda n, c: True)
    assert forced["model"] == "my-model" and change3["reason"] == "override"
    default, change4 = session_import.map_model(row, "target_default", None, cfg, lambda n, c: True)
    assert default["model"] == "target-model" and change4["reason"] == "target_default"


@pytest.mark.parametrize(
    ("cfg", "model_config", "expected"),
    [
        ({"model": {"provider": "ollama"}}, None, True),
        ({"model": {"provider": "openai"}}, None, False),
        ({"model": {"provider": "custom", "base_url": "http://127.0.0.1:1234/v1"}}, None, True),
        (
            {"model": {"provider": "x"}, "providers": {"lan": {"base_url": "http://10.0.0.5/v1"}}},
            {"gateway_runtime": {"provider": "lan"}},
            True,
        ),
        (
            {"model": {"provider": "x"}, "providers": {"far": {"base_url": "https://api.example.com"}}},
            {"gateway_runtime": {"provider": "far"}},
            False,
        ),
    ],
)
def test_offline_capable(cfg, model_config, expected):
    assert session_import.offline_capable(model_config or {}, cfg) is expected


def test_import_path_a_tombstone_error_leaves_nothing(
    make_session, session_db, target_db, store, source_paths, target_cwd
):
    """An id deleted on the target after planning (tombstone): ``import_sessions`` answers ``ok: False``, nothing is
    written, the ids are re-minted and the import completes."""
    tip = rich_lineage(make_session)
    info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    p = plan(target_db, capsule, target_cwd)
    doomed = p.segments[-1].new_id
    target_db.create_session(doomed, "cli")
    assert target_db.delete_session(doomed)
    assert target_db.get_meta(session_import.RETIRED_PREFIX + doomed)
    prepared = session_import.import_hidden(target_db, store, cap.capsule_id, p)
    assert prepared.tip_id != doomed
    assert all(target_db.get_session(s.new_id) is None for s in p.segments)
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [prepared.root_id, prepared.tip_id])


def test_import_title_collision_retitles(make_session, session_db, target_db, store, source_paths, target_cwd):
    """★ A title taken on the target between plan and import (``IntegrityError``) ⇒ ``T #2`` [S1].

    Mutant: no ``IntegrityError`` retry in ``_import_a``.
    """
    tip = rich_lineage(make_session, title="Shared Title")
    _info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    p = plan(target_db, capsule, target_cwd)
    assert p.segments[-1].title == "Shared Title"
    other = "20200202_000000_abcdef"
    target_db.create_session(other, "cli")
    target_db.set_session_title(other, "Shared Title")
    prepared = session_import.import_hidden(target_db, store, cap.capsule_id, p)
    assert prepared.title == "Shared Title #2"
    assert target_db.get_session(prepared.tip_id)["title"] == "Shared Title #2"


def test_import_title_taken_at_plan_and_bot_chat(make_session, session_db, target_db, store, source_paths, target_cwd):
    other = "20200202_000000_abcde0"
    target_db.create_session(other, "cli")
    target_db.set_session_title(other, "Taken")
    tip = rich_lineage(make_session, title="Taken")
    *_, _p, prepared = roundtrip(session_db, target_db, store, tip, source_paths, target_cwd)
    assert prepared.title == "Taken #2"
    b = make_session
    bot = b.create()
    b.turns(bot, 1)
    session_db.set_session_title(bot, "Bot Chat")
    *_, _p2, prepared2 = roundtrip(session_db, target_db, store, bot, source_paths, target_cwd)
    assert prepared2.title.startswith("Bot Chat (handover ")


def test_assert_resume_safe_reported(make_session, session_db, target_db, store, source_paths, target_cwd, hermes_home):
    """The core's resume guard failing on the target is a warning in the prepared info, not an abort [SK L4]."""
    (hermes_home / "config.yaml").write_text("sessions:\n  max_resume_messages: 5\n")
    tip = rich_lineage(make_session)
    *_, prepared = roundtrip(session_db, target_db, store, tip, source_paths, target_cwd)
    assert any(w.startswith("Hermes' resume check reports a problem") for w in prepared.warnings)


def test_apply_delta_appends_and_reverifies(make_session, session_db, target_db, store, source_paths, target_cwd):
    """★ Delta rows land at the hidden tip and H_all/H_rows are recomputed; a tampered delta ⇒ ``VerifyFailed``.

    Mutant: ``apply_delta`` without the re-verification.
    """
    b = make_session
    tip = rich_lineage(b)
    info, cap, _capsule, p, prepared = roundtrip(session_db, target_db, store, tip, source_paths, target_cwd)
    b.user(tip, "typed during the handover")
    _outcome, rows = session_export.check_revision(session_db, info, cap)
    delta = session_export.export_delta(session_db, info, cap, rows, store=store)
    after = session_import.apply_delta(target_db, store, delta, prepared)
    assert (after.h_all, after.h_rows) == (delta.h_all, delta.h_rows)
    assert after.source_revision == delta.revision
    assert target_db.get_messages(prepared.tip_id)[-1]["content"] == "typed during the handover"
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [s.new_id for s in p.segments])
    doc = {"kind": "ctxh.delta", "v": 1, "rows": [dict(rows[0], content="forged")], "hashes": {}}
    forged = type(delta)(
        put_json(store, doc).object_id, delta.tip_id, 1, delta.h_all, delta.h_rows, delta.h_model, delta.revision
    )
    with pytest.raises(VerifyFailed):
        session_import.apply_delta(target_db, store, forged, after)


def test_side_state_written_paused(target_db):
    """★ goal **paused** with ``max_turns`` capped and wait barriers cleared, heartbeat/loop paused, loop route removed
    [R22, SEC 4.5].

    Mutant: the goal keeps its status (``g["status"] = "paused"`` removed).
    """
    capsule = {
        "state_meta": {
            "goal": {
                "goal": "ship",
                "status": "active",
                "max_turns": 50,
                "waiting_on_pid": 4242,
                "waiting_on_session": "proc_1",
                "waiting_until": 99.0,
                "waiting_on_delegations": 2,
            },
            "heartbeat": {"prompt": "ping", "interval_seconds": 60, "status": "active"},
            "loop": {"prompt": "tick", "status": "active", "route": {"platform": "telegram", "chat_id": "1"}},
        }
    }
    tip = "20200303_000000_aaaaaa"
    target_db.create_session(tip, "cli")
    session_import.write_side_state(target_db, capsule, tip)
    goal = json.loads(target_db.get_meta(f"goal:{tip}"))
    assert goal["status"] == "paused" and goal["max_turns"] == 20
    assert goal["waiting_on_pid"] is None and goal["waiting_on_session"] is None
    assert goal["waiting_until"] == 0.0 and goal["waiting_on_delegations"] == 0
    assert json.loads(target_db.get_meta(f"heartbeat:{tip}"))["status"] == "paused"
    loop = json.loads(target_db.get_meta(f"loop:{tip}"))
    assert loop["status"] == "paused" and "route" not in loop
    session_import.clear_side_state(target_db, tip)
    assert all(
        json.loads(target_db.get_meta(f"{k}:{tip}"))["status"] == "cleared" for k in ("goal", "heartbeat", "loop")
    )


def test_side_state_survives_roundtrip(make_session, session_db, target_db, store, source_paths, target_cwd):
    b = make_session
    sid = b.create()
    b.turns(sid, 1)
    session_db.set_meta(f"goal:{sid}", json.dumps({"goal": "g", "status": "active", "max_turns": 10}))
    _info, _cap, capsule, _p, prepared = roundtrip(session_db, target_db, store, sid, source_paths, target_cwd)
    session_import.write_side_state(target_db, capsule, prepared.tip_id)
    goal = json.loads(target_db.get_meta(f"goal:{prepared.tip_id}"))
    assert goal == {
        "goal": "g",
        "status": "paused",
        "max_turns": 10,
        "paused_reason": "handed over",
        **session_import._GOAL_WAIT_CLEAR,
    }


def test_plan_rejects_capsule_over_max_session_bytes(
    make_session, session_db, target_db, store, source_paths, target_cwd
):
    from ctx_handover.engine.errors import PayloadTooLarge

    tip = rich_lineage(make_session)
    _info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    with pytest.raises(PayloadTooLarge):
        plan(target_db, capsule, target_cwd, max_session_bytes=1000)


def test_retitle_on_prepare_prevents_drift(
    make_session, session_db, target_db, store, source_paths, target_cwd, ledger, tmp_path
):
    """★ Round trip A→B→A: the title stays "T" on the active copy on both hosts — the source renames its retired
    lineage at prepare, so "T" is free when the session comes back [BX M5, R14].

    Mutant: ``lock.prepare`` skips the title step p3 (no rename) ⇒ the return import lands as "T #2".
    """
    from datetime import datetime, timezone

    from ctx_handover.engine import lock
    from ctx_handover.engine.model import HermesPaths, SessionRef
    from ctx_handover.tests.unit.B.support import LedgerStub

    tip_a = rich_lineage(make_session, title="T")
    info_a = session_export.describe(session_db, SessionRef(id=tip_a), calling_session=None)
    *_, prepared_b = roundtrip(session_db, target_db, store, tip_a, source_paths, target_cwd)
    session_import.unhide(target_db, prepared_b)
    holder = lock.acquire(
        session_db, ledger, info_a, "01JOBAAAAAAAAAAAAAAAAAAAAA", ttl_s=60, wait_s=0, cancelled=lambda: False
    )
    lock.prepare(
        session_db,
        ledger,
        info_a,
        "01JOBAAAAAAAAAAAAAAAAAAAAA",
        "laptop",
        datetime(2026, 11, 9, 18, 0, tzinfo=timezone.utc),
    )
    del holder
    # B → A: export the copy on B, import it on A
    paths_b = HermesPaths(home=str(tmp_path / "target-home"), document_cache=str(tmp_path / "target-home" / "c"))
    back_cwd = tmp_path / "back"
    back_cwd.mkdir()
    _, _, _, _, prepared_a = roundtrip(target_db, session_db, store, prepared_b.tip_id, paths_b, back_cwd)
    assert prepared_a.title == "T"
    assert session_db.get_session(tip_a)["title"].startswith("T (handed over to laptop ")
    assert target_db.get_session(prepared_b.tip_id)["title"] == "T"
    ledger_b = LedgerStub(tmp_path / "ledger-b.sqlite3")
    try:
        info_b = session_export.describe(target_db, SessionRef(id=prepared_b.tip_id), calling_session=None)
        lock.acquire(
            target_db, ledger_b, info_b, "01JOBBBBBBBBBBBBBBBBBBBBBB", ttl_s=60, wait_s=0, cancelled=lambda: False
        )
        lock.prepare(
            target_db,
            ledger_b,
            info_b,
            "01JOBBBBBBBBBBBBBBBBBBBBBB",
            "server",
            datetime(2026, 11, 9, 20, 0, tzinfo=timezone.utc),
        )
    finally:
        ledger_b.close()
    session_import.unhide(session_db, prepared_a)
    titled = {r["title"] for r in session_db.list_sessions_rich(limit=50, include_archived=True, include_hidden=True)}
    assert "T" in titled and "T #2" not in titled
    assert target_db.get_session(prepared_b.tip_id)["title"].startswith("T (handed over to server ")
