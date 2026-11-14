"""S9: the shared guidance operations (set, restore, reset) and the coalescing of operator messages."""

from __future__ import annotations

import asyncio
import json
from types import SimpleNamespace

import pytest

BOT = "900000000000000001"
GUILD = "900000000000000002"
OP = "900000000000000011"
MGR = "900000000000000021"


@pytest.fixture
def env(anchor):
    store = anchor.mod("cli").store_for(anchor.home)
    ops = anchor.mod("store.ops")
    ops.bind(store, BOT, GUILD, by="cli")
    return SimpleNamespace(store=store, ops=ops, file=anchor.mod("store.file"), schema=anchor.mod("store.schema"),
                           anchor=anchor)


def prompts(env):
    return env.store.read().guild(BOT, GUILD)["prompts"]


def test_set_guidance_keeps_ten_versions_and_ignores_the_same_text(env):
    for i in range(12):
        env.ops.set_guidance(env.store, BOT, GUILD, f"text {i}", by=MGR, via="discord")
    p = prompts(env)
    assert p["guild"] == "text 11" and len(p["history"]) == 10
    assert [h["text"] for h in p["history"]] == [f"text {i}" for i in range(2, 12)]
    assert p["history"][-1]["sha256"] == env.ops.guidance_digest("text 11") and p["history"][-1]["by"] == MGR
    rev = env.store.read().guild(BOT, GUILD)["rev"]
    env.ops.set_guidance(env.store, BOT, GUILD, "text 11", by=MGR)
    assert env.store.read().guild(BOT, GUILD)["rev"] == rev and len(prompts(env)["history"]) == 10


def test_text_from_before_the_first_recorded_change_stays_restorable(env):
    env.ops.update_fields(env.store, BOT, GUILD, {"prompts.guild": "imported"}, by="cli")
    env.ops.set_guidance(env.store, BOT, GUILD, "new", by=MGR)
    assert [h["text"] for h in prompts(env)["history"]] == ["imported", "new"]


def test_set_guidance_refuses_over_the_cap_and_checks_rev(env):
    with pytest.raises(env.schema.SchemaError, match="limit"):
        env.ops.set_guidance(env.store, BOT, GUILD, "x" * 11, by=MGR, prompt_cap=10)
    with pytest.raises(env.file.ConflictError):
        env.ops.set_guidance(env.store, BOT, GUILD, "x", by=MGR, expected_rev=0)
    assert prompts(env)["guild"] == "" and prompts(env)["history"] == []
    with pytest.raises(env.schema.SchemaError, match="control"):
        env.ops.set_guidance(env.store, BOT, GUILD, "bad\x00", by=MGR)


def test_restore_by_version_id_with_rev(env):
    env.ops.set_guidance(env.store, BOT, GUILD, "one", by=MGR)
    env.ops.set_guidance(env.store, BOT, GUILD, "two", by=MGR)
    sha = env.ops.guidance_digest("one")
    rev = env.store.read().guild(BOT, GUILD)["rev"]
    with pytest.raises(env.file.ConflictError):
        env.ops.restore_guidance(env.store, BOT, GUILD, sha[:12], by=MGR, expected_rev=rev - 1)
    assert prompts(env)["guild"] == "two"
    env.ops.restore_guidance(env.store, BOT, GUILD, sha[:12], by=MGR, expected_rev=rev, via="discord")
    p = prompts(env)
    assert p["guild"] == "one" and [h["text"] for h in p["history"]] == ["one", "two", "one"]
    for bad in ("abc", "zz" * 8, "0" * 12):
        with pytest.raises(env.schema.SchemaError):
            env.ops.restore_guidance(env.store, BOT, GUILD, bad, by=MGR)


def test_restore_refuses_a_version_over_a_lowered_cap(env):
    env.ops.set_guidance(env.store, BOT, GUILD, "x" * 30, by=MGR)
    env.ops.set_guidance(env.store, BOT, GUILD, "short", by=MGR)
    with pytest.raises(env.schema.SchemaError, match="limit"):
        env.ops.restore_guidance(env.store, BOT, GUILD, env.ops.guidance_digest("x" * 30), by=MGR, prompt_cap=20)


def test_reset_fields_to_defaults_and_guidance_to_an_empty_version(env):
    env.ops.update_fields(env.store, BOT, GUILD, {"identity.bot_name": "Nova", "progress.grouping": "rotate"}, by=MGR)
    env.ops.set_guidance(env.store, BOT, GUILD, "keep me", by=MGR)
    env.ops.reset_fields(env.store, BOT, GUILD, ["identity.bot_name", "prompts.guild"], by=OP, via="discord")
    guild = env.store.read().guild(BOT, GUILD)
    assert guild["identity"]["bot_name"] == "" and guild["progress"]["grouping"] == "rotate"
    assert guild["prompts"]["guild"] == "" and guild["prompts"]["history"][-2]["text"] == "keep me"
    for path in ("state", "rev", "prompts.history", "operators", "identity"):
        with pytest.raises(env.schema.SchemaError):
            env.ops.reset_fields(env.store, BOT, GUILD, [path], by=OP)
    with pytest.raises(env.schema.SchemaError):
        env.ops.reset_fields(env.store, BOT, GUILD, [], by=OP)


def test_guidance_operations_record_their_origin_in_the_audit(env):
    env.ops.set_guidance(env.store, BOT, GUILD, "one", by=MGR, via="dashboard")
    env.ops.set_guidance(env.store, BOT, GUILD, "two", by=MGR, via="discord")
    env.ops.restore_guidance(env.store, BOT, GUILD, env.ops.guidance_digest("one"), by=MGR, via="discord")
    with env.ops.origin("cli"):                                   # the context origin applies without via=
        env.ops.reset_fields(env.store, BOT, GUILD, ["prompts.guild"], by=OP)
    audit = [a for a in env.store.read().data["audit"] if a["field"] == "prompts.guild"]
    assert [a.get("via") for a in audit] == ["dashboard", "discord", "discord", "cli"]
    with pytest.raises(ValueError):
        env.ops.set_guidance(env.store, BOT, GUILD, "three", by=MGR, via="elsewhere")
    assert prompts(env)["guild"] == ""


# ── coalescing of guidance messages (deterministic clock) ────────────────────────────────────────────


class _Binding:
    def __init__(self, anchor, store):
        self.bot_id = BOT
        self.owner_home = anchor.home
        self.store = store
        self.sync_path = anchor.home / "command-sync.json"
        self.runtime = anchor.RUNTIME
        self._tasks = set()
        self.sent = []

    async def send_dm(self, user_id, text, *, file=None):
        self.sent.append((user_id, text, file))


def test_guidance_messages_coalesce_into_one_diff(env, monkeypatch):
    anchor = env.anchor
    config = anchor.mod("config")
    config.set_reader(lambda home, key, default: {"operators": [OP]}.get(key, default))
    notify = anchor.mod("ui.notify")
    clock = [1000.0]
    monkeypatch.setattr(notify.time, "time", lambda: clock[0])
    binding = _Binding(anchor, env.store)
    notifier = notify.Notifier(binding)
    ctx = SimpleNamespace(guild_id=GUILD, guild_name="Example Guild", actor=SimpleNamespace(user_id=MGR))

    async def run():
        env.ops.set_guidance(env.store, BOT, GUILD, "one", by=MGR)
        notifier.guidance_changed(ctx, "", "one")
        await asyncio.sleep(0)
        await asyncio.gather(*binding._tasks)
        assert len(binding.sent) == 1
        for old, new in (("one", "two"), ("two", "three")):
            clock[0] += 10                                    # inside the window: held back
            env.ops.set_guidance(env.store, BOT, GUILD, new, by=MGR)
            notifier.guidance_changed(ctx, old, new)
        assert len(binding.sent) == 1 and len(notifier._pending) == 1
        notifier.cancel_timers()
        clock[0] += notify.GUIDANCE_WINDOW
        await notifier.flush_all_pending()                   # e.g. after a restart
        assert len(binding.sent) == 2
        diff = binding.sent[-1][2][1].decode()
        assert "-one" in diff and "+three" in diff and "two" not in diff.replace("(before)", "")
        assert await notifier.flush_pending(f"guidance:{BOT}:{GUILD}", GUILD) is False   # nothing left

    try:
        asyncio.run(run())
    finally:
        config.set_reader(None)
    state = json.loads((anchor.home / "notify-state.json").read_text())
    assert state[f"guidance:{BOT}:{GUILD}"]["pending"] is False


def test_a_held_back_diff_is_not_stuck_after_a_restart_or_during_a_flush(env, monkeypatch):
    anchor = env.anchor
    config = anchor.mod("config")
    config.set_reader(lambda home, key, default: {"operators": [OP]}.get(key, default))
    notify = anchor.mod("ui.notify")
    clock = [5000.0]
    monkeypatch.setattr(notify.time, "time", lambda: clock[0])
    binding = _Binding(anchor, env.store)
    ctx = SimpleNamespace(guild_id=GUILD, guild_name="Example Guild", actor=SimpleNamespace(user_id=MGR))
    other = SimpleNamespace(guild_id=GUILD, guild_name="Example Guild", actor=SimpleNamespace(user_id=OP))

    async def run():
        first = notify.Notifier(binding)
        env.ops.set_guidance(env.store, BOT, GUILD, "one", by=MGR)
        first.guidance_changed(ctx, "", "one")
        await asyncio.sleep(0)
        await asyncio.gather(*binding._tasks)
        assert len(binding.sent) == 1
        clock[0] += 10
        env.ops.set_guidance(env.store, BOT, GUILD, "two", by=OP)
        first.guidance_changed(other, "one", "two")
        first.cancel_timers()                                     # the process ends inside the window
        restarted = notify.Notifier(binding)
        await restarted.flush_all_pending()
        assert len(restarted._pending) == 1                      # not stuck: a timer for the rest of the window
        restarted.cancel_timers()
        clock[0] += notify.GUIDANCE_WINDOW
        await restarted.flush_all_pending()
        await asyncio.gather(*binding._tasks)
        assert len(binding.sent) == 2 and binding.sent[-1][1].startswith(f"<@{OP}> changed")   # its author
        assert b"-one" in binding.sent[-1][2][1] and b"+two" in binding.sent[-1][2][1]
        restarted._pending["guidance:x"] = object()               # a flush in progress no longer blocks new timers
        await restarted._flush_later(f"guidance:{BOT}:{GUILD}", GUILD, "Example Guild")
        assert f"guidance:{BOT}:{GUILD}" not in restarted._pending

    try:
        asyncio.run(run())
    finally:
        config.set_reader(None)

