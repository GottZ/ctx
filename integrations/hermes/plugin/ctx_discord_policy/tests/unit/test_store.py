"""S2: policy store — atomic, locked, audited; corrupt files fail closed; migrations; concurrency."""

from __future__ import annotations

import json
import multiprocessing
import os
import stat
import sys
import threading
from pathlib import Path

import pytest
from conftest import ROOT
from fakes import storeworker

BOT = "900000000000000001"
GUILD = "900000000000000002"


@pytest.fixture
def store_mod(anchor):
    ns = type("S", (), {})()
    ns.file = anchor.mod("store.file")
    ns.ops = anchor.mod("store.ops")
    ns.schema = anchor.mod("store.schema")
    ns.migrate = anchor.mod("store.migrate")
    ns.path = anchor.tmp / "owner" / "plugin-data" / "ctx_discord_policy" / "policy.json"
    ns.store = ns.file.PolicyStore(ns.path)
    return ns


def test_bind_creates_defaults_audits_hashes_and_is_private(store_mod):
    snap = store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")
    guild = snap.guild(BOT, GUILD)
    assert snap.state == "ok" and guild["state"] == "active" and guild["rev"] == 1
    # Defaults after activation (OPEN L8)
    assert guild["conversation"]["ambient_channels"] == []
    assert guild["lanes"]["channel_history"] == {"enabled": True, "max_limit": 100}
    assert guild["lanes"]["skills"] == [] and guild["lanes"]["knowledge"] == []
    assert guild["worklog"]["channel"] is None
    assert guild["prompts"]["guild_in_operator_lane"] is False  # OPEN U3 default off
    audit = snap.data["audit"]
    assert {a["field"] for a in audit} == {"created", "state"}
    assert all(len(a["old_sha256"]) == 64 and "old" not in a for a in audit)
    if os.name == "posix":
        assert stat.S_IMODE(store_mod.path.stat().st_mode) == 0o600
        assert stat.S_IMODE(store_mod.path.parent.stat().st_mode) == 0o700


def test_snapshot_is_cached_until_the_file_changes(store_mod):
    store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")
    first = store_mod.store.read()
    assert store_mod.store.read() is first
    store_mod.ops.set_state(store_mod.store, BOT, GUILD, "paused", by="cli")
    second = store_mod.store.read()
    assert second is not first and second.guild_state(BOT, GUILD) == "paused"


def test_optimistic_rev_conflict(store_mod):
    snap = store_mod.ops.bind(store_mod.store, BOT, GUILD, by="panel-a")
    rev = snap.guild(BOT, GUILD)["rev"]
    store_mod.ops.set_state(store_mod.store, BOT, GUILD, "paused", by="panel-b", expected_rev=rev)
    with pytest.raises(store_mod.file.ConflictError):
        store_mod.ops.set_state(store_mod.store, BOT, GUILD, "active", by="panel-a", expected_rev=rev)


@pytest.mark.parametrize("garbage", [b"{not json", b'{"schema": 1, "bots": {"', b"[]", b'{"bots": {}}',
                                     b'{"schema": 1, "bots": {"x": 1}}', b"\xff\xfe"])
def test_corrupt_file_is_kept_never_overwritten_and_pauses_everything(store_mod, garbage):
    store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")
    assert store_mod.store.read().guild_state(BOT, GUILD) == "active"
    store_mod.path.write_bytes(garbage)  # disk damage / a botched hand edit
    os.utime(store_mod.path, ns=(1, 1))
    snap = store_mod.store.read()
    assert snap.state == "corrupt" and snap.guild_state(BOT, GUILD) == "paused" and snap.error
    copies = sorted(store_mod.path.parent.glob("policy.json.corrupt-*"))
    assert copies and copies[0].read_bytes() == garbage
    with pytest.raises(store_mod.file.StoreError):
        store_mod.ops.bind(store_mod.store, BOT, "900000000000000009", by="cli")
    assert store_mod.path.read_bytes() == garbage


def test_newer_schema_is_read_only(store_mod):
    store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")
    doc = json.loads(store_mod.path.read_text())
    doc["schema"] = 2
    doc["bots"][BOT]["guilds"][GUILD]["future_field"] = {"x": 1}
    store_mod.path.write_text(json.dumps(doc))
    snap = store_mod.store.read()
    assert snap.state == "downgrade" and snap.guild_state(BOT, GUILD) == "active"
    assert any("future_field" in p for p in snap.problems)
    with pytest.raises(store_mod.file.StoreError):
        store_mod.ops.set_state(store_mod.store, BOT, GUILD, "paused", by="cli")
    assert json.loads(store_mod.path.read_text())["schema"] == 2


def test_migration_chain_backs_up_then_upgrades(store_mod, monkeypatch):
    def v0_to_v1(doc):
        return {"bots": {BOT: {"guilds": {GUILD: dict(state=doc["active"] and "active" or "paused")}}},
                "audit": []}

    monkeypatch.setattr(store_mod.migrate, "MIGRATIONS", {0: v0_to_v1})
    store_mod.path.parent.mkdir(parents=True)
    store_mod.path.write_text(json.dumps({"schema": 0, "active": True}))
    snap = store_mod.store.read()
    assert snap.state == "ok" and snap.guild_state(BOT, GUILD) == "active"
    assert json.loads(store_mod.path.read_text())["schema"] == 1
    backups = list(store_mod.path.parent.glob("policy.json.bak-0-*"))
    assert backups and json.loads(backups[0].read_text())["schema"] == 0


def test_missing_migration_step_fails_closed(store_mod, monkeypatch):
    monkeypatch.setattr(store_mod.migrate, "MIGRATIONS", {})
    store_mod.path.parent.mkdir(parents=True)
    store_mod.path.write_text(json.dumps({"schema": 0}))
    assert store_mod.store.read().state == "corrupt"


def test_import_is_strict_and_rejects_authority_shaped_keys(store_mod):
    store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")
    exported = store_mod.ops.export(store_mod.store.read(), BOT, GUILD)["bots"][BOT]["guilds"][GUILD]
    for key, value in (("operators", ["900000000000000077"]), ("admins", []), ("lane", "operator")):
        with pytest.raises(store_mod.schema.SchemaError):
            store_mod.ops.import_guild(store_mod.store, BOT, GUILD, dict(exported, **{key: value}), by="cli",
                                       prompt_cap=6000)
    exported["conversation"]["ambient_channels"] = ["900000000000000033"]
    snap = store_mod.ops.import_guild(store_mod.store, BOT, GUILD, exported, by="cli", prompt_cap=6000)
    assert snap.guild(BOT, GUILD)["conversation"]["ambient_channels"] == ["900000000000000033"]
    assert snap.guild(BOT, GUILD)["rev"] == 2
    with pytest.raises(store_mod.schema.SchemaError):
        bad = dict(exported, prompts=dict(exported["prompts"], guild="x" * 6001))
        store_mod.ops.import_guild(store_mod.store, BOT, GUILD, bad, by="cli", prompt_cap=6000)


def test_schema_has_no_authority_field(store_mod):
    """Pinned list of every settable field: none of them names who may use tools (DESIGN §4.2)."""
    paths = store_mod.schema.field_paths()
    assert paths == [
        "state", "rev", "bound_by", "bound_at",
        "identity.bot_name", "identity.wake_names", "identity.greetings", "identity.reaction_emoji",
        "conversation.ambient_channels", "conversation.excluded_channels", "conversation.language",
        "prompts.guild", "prompts.guild_in_operator_lane", "prompts.history", "prompts.template_overrides",
        "progress.grouping",
        "worklog.channel", "worklog.archive_minutes", "worklog.final_to_origin",
        "lanes.channel_history.enabled", "lanes.channel_history.max_limit", "lanes.permalink_hydration",
        "lanes.skills", "lanes.knowledge",
        "operator_lane.channel_hydration",
        "managers.mode", "managers.role_ids",
        "audit_channel", "updated_by", "updated_at",
    ]


def test_threads_never_lose_updates(store_mod):
    errors = []

    def worker(n):
        try:
            for i in range(10):
                store_mod.ops.bind(store_mod.store, BOT, f"9{n:02d}{i:02d}00000000000000", by=f"t{n}")
        except Exception as exc:  # pragma: no cover - reported below
            errors.append(exc)

    threads = [threading.Thread(target=worker, args=(n,)) for n in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not errors
    fresh = type(store_mod.store)(store_mod.path).read()
    assert len(fresh.data["bots"][BOT]["guilds"]) == 80


def test_processes_never_lose_updates(store_mod):
    ctx = multiprocessing.get_context("spawn")
    procs = [ctx.Process(target=storeworker.bind_many, args=(str(ROOT), str(store_mod.path), n)) for n in range(6)]
    for p in procs:
        p.start()
    for p in procs:
        p.join(60)
    assert all(p.exitcode == 0 for p in procs), [p.exitcode for p in procs]
    snap = type(store_mod.store)(store_mod.path).read()
    guilds = snap.data["bots"][BOT]["guilds"]
    assert len(guilds) == 90
    assert sum(1 for g in guilds.values() if g["state"] == "paused") == 30
    assert all(g["rev"] == (2 if g["state"] == "paused" else 1) for g in guilds.values())
    assert not list(store_mod.path.parent.glob(".policy.json.*.tmp"))
