"""S-INT: the layers wired together — lookups, per-turn context, bind steps, closed armed check, operator list,
revision epochs across a purge."""

from __future__ import annotations

import asyncio
import json
import logging
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

BOT = "900000000000000300"
GUILD = "900000000000000100"
OTHER_GUILD = "900000000000000101"
CHANNEL = "900000000000000200"
MEMBER = "900000000000000500"
OPERATOR = "900000000000000400"


@pytest.fixture
def wired(anchor, monkeypatch):
    from fakes.ctx import FakeCtx

    owner = anchor.tmp / "owner"
    owner.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(owner))
    anchor.entry.register(FakeCtx("default"))
    config = anchor.mod("config")

    def reader(home, key, default):
        path = Path(home) / "settings.json"
        return json.loads(path.read_text()).get(key, default) if path.exists() else default

    config.set_reader(reader)
    (owner / "settings.json").write_text(json.dumps({"operators": [OPERATOR], "operator_label": "the host"}))

    class Adapter:  # weak-referenceable, like the real adapter
        _client = SimpleNamespace(application_id=int(BOT))

    adapter = Adapter()
    binding = anchor.RUNTIME.bots.bind(adapter, BOT, owner)
    ops = anchor.mod("store.ops")
    ops.bind(binding.store, BOT, GUILD, by="cli")
    ns = SimpleNamespace(anchor=anchor, owner=owner, binding=binding, ops=ops, wiring=anchor.mod("wiring"),
                         adapter=adapter, status=anchor.mod("adapt.status"))
    yield ns
    config.set_reader(None)


def _configure(ns, guild_id=GUILD, **sections):
    exported = ns.ops.export(ns.binding.store.read(), BOT, guild_id)["bots"][BOT]["guilds"][guild_id]
    for name, value in sections.items():
        head, _, leaf = name.partition("__")
        if leaf:
            exported[head][leaf] = value
        else:
            exported[head] = value
    ns.ops.import_guild(ns.binding.store, BOT, guild_id, exported, by="cli", prompt_cap=6000)


def _route(ns, lane, **kw):
    fields = dict(bot_id=BOT, guild_id=GUILD, channel_id=CHANNEL, message_id="1", author_id=MEMBER, lane=lane,
                  server_name="Example Guild", channel_name="general", bot_display_name="Helper")
    fields.update(kw)
    return ns.anchor.routes.Route(**fields)


def _outcome(ns, key):
    result = ns.anchor.RUNTIME.status.get(key)
    return result.outcome.value if result is not None else None


# ── lookups ──────────────────────────────────────────────────────────────────────────────────────────


def test_lookups_answer_only_for_active_guilds(wired):
    w = wired.wiring
    assert w.guild_config(BOT, GUILD)["state"] == "active"
    assert w.progress_grouping(BOT, GUILD) == "default"
    _configure(wired, progress__grouping="rotate")
    assert w.progress_grouping(BOT, GUILD) == "rotate"
    wired.ops.set_state(wired.binding.store, BOT, GUILD, "paused", by="cli")
    assert w.guild_config(BOT, GUILD) is None and w.progress_grouping(BOT, GUILD) == "default"
    wired.ops.set_state(wired.binding.store, BOT, GUILD, "unbound", by="cli")
    assert w.guild_config(BOT, GUILD) is None and w.progress_grouping(BOT, GUILD) == "default"
    assert w.guild_config("900000000000000999", GUILD) is None and w.progress_grouping(BOT, OTHER_GUILD) == "default"


# ── turn context (pre_llm_call) ──────────────────────────────────────────────────────────────────────


def test_turn_context_is_assembled_from_frame_lanes_and_guidance(wired):
    _configure(wired, prompts__guild="Be kind in {server_name}; keep answers short.")
    public = wired.wiring.turn_context(_route(wired, "public"))
    assert public.index("[Operator frame]") < public.index("[Capability lane: public]") \
        < public.index("[Conversation lane: direct]") < public.index("Be kind in Example Guild")
    operator = wired.wiring.turn_context(_route(wired, "operator", author_id=OPERATOR))
    assert "[Capability lane: operator]" in operator
    assert "Be kind in" not in operator  # guidance stays out of operator turns by default (OPEN U3)


def test_pre_llm_call_returns_the_assembled_context_never_the_raw_lane(wired):
    hooks, runtime = wired.anchor.hooks, wired.anchor.RUNTIME
    _configure(wired, prompts__guild="Server rule: no spoilers.")
    token = runtime.route.set(_route(wired, "public"))
    try:
        context = hooks.on_pre_llm_call(session_id="s", turn_id="t")["context"]
    finally:
        runtime.route.reset(token)
    assert context == wired.wiring.turn_context(_route(wired, "public"))
    assert "[Operator frame]" in context and "> Server rule: no spoilers." in context
    assert context != wired.anchor.mod("policy.lanes").capability_lane_text(_route(wired, "public"))


def test_the_route_mode_reaches_the_conversation_lane(wired):
    """S5 sets ``Route.mode = "worklog"`` for worklog threads: it must arrive as it is, never as ``direct``."""
    for mode in ("ambient", "direct", "worklog"):
        text = wired.wiring.turn_context(_route(wired, "public", mode=mode))
        assert f"[Conversation lane: {mode}]" in text
    with pytest.raises(ValueError):  # an unknown mode is never mapped silently; the hook then blocks
        wired.wiring.turn_context(_route(wired, "public", mode="whisper"))


def test_budget_problems_are_reported_per_bot_guild_and_lane_once(wired, monkeypatch, caplog):
    w = wired.wiring
    _configure(wired, prompts__guild=("Long guidance line for this server. " * 80).strip())
    public, operator = _route(wired, "public"), _route(wired, "operator", author_id=OPERATOR)
    fixed = len(w.turn_context(operator))  # operator turns leave the guidance out
    monkeypatch.setattr(wired.anchor.mod("hermes"), "spill_config",
                        lambda: {"enabled": True, "max_chars": fixed + 2500})
    caplog.set_level(logging.INFO, logger="ctx_discord_policy")
    for _ in range(3):  # alternating lanes never flap one shared entry
        w.turn_context(public)
        w.turn_context(operator)
    pub_key, op_key = w.context_key(public), w.context_key(operator)
    assert pub_key == f"A9[{BOT}/{GUILD}/public/direct]"
    assert _outcome(wired, pub_key) == "fallback" and "shortened" in wired.anchor.RUNTIME.status.get(pub_key).reason
    assert _outcome(wired, op_key) is None  # never had a problem: no entry
    assert _outcome(wired, "A9") == "ok"
    warnings = [r for r in caplog.records if r.levelno >= logging.WARNING and "A9[" in r.getMessage()]
    assert len(warnings) == 1, [r.getMessage() for r in warnings]
    # The frame alone over budget: degraded, reported once more; a fitting context recovers the entry.
    monkeypatch.setattr(wired.anchor.mod("hermes"), "spill_config", lambda: {"enabled": True, "max_chars": 600})
    w.turn_context(public)
    w.turn_context(public)
    assert "exceed the context budget" in wired.anchor.RUNTIME.status.get(pub_key).reason
    monkeypatch.setattr(wired.anchor.mod("hermes"), "spill_config", lambda: {"enabled": False})
    w.turn_context(public)
    assert _outcome(wired, pub_key) == "ok" and "no context budget" in wired.anchor.RUNTIME.status.get("A9").reason


def test_budget_status_never_flaps_between_conversation_modes_of_one_lane(wired, monkeypatch, caplog):
    """Ambient and direct turns alternate in one public channel; their contexts differ in length, so a budget
    that fits one and shortens the other must not toggle one shared entry on every turn."""
    w = wired.wiring
    direct, ambient = _route(wired, "public", mode="direct"), _route(wired, "public", mode="ambient")
    _configure(wired, prompts__guild="")
    gap = len(w.turn_context(ambient)) - len(w.turn_context(direct))
    assert gap > 0
    _configure(wired, prompts__guild=("Calm and friendly answers please. " * 120).strip())
    full = len(w.turn_context(direct))
    monkeypatch.setattr(wired.anchor.mod("hermes"), "spill_config",
                        lambda: {"enabled": True, "max_chars": full + 500 + gap // 2})
    caplog.set_level(logging.INFO, logger="ctx_discord_policy")
    for _ in range(5):
        w.turn_context(ambient)
        w.turn_context(direct)
    warnings = [r.getMessage() for r in caplog.records if r.levelno >= logging.WARNING and "A9[" in r.getMessage()]
    assert len(warnings) == 1, warnings
    assert w.context_key(direct) != w.context_key(ambient)
    assert _outcome(wired, w.context_key(ambient)) == "fallback" and _outcome(wired, w.context_key(direct)) is None


def test_context_notices_never_read_as_closed_admission_for_managers(wired):
    panels, status, runtime = wired.anchor.mod("ui.panels"), wired.status, wired.anchor.RUNTIME
    ctx = SimpleNamespace(binding=SimpleNamespace(runtime=runtime))
    runtime.status.record(status.Result(f"A9[{BOT}/{OTHER_GUILD}/public]", "S", status.Outcome.FALLBACK,
                                        "server guidance shortened"))
    assert "Reduced" not in panels.protection_summary(ctx)
    runtime.status.record(status.Result("A5", "S", status.Outcome.OFF, "admission wrapper missing"))
    assert "Reduced" in panels.protection_summary(ctx)


def test_operator_turn_survives_a_broken_context_path_public_turn_blocks(wired, monkeypatch):
    hooks, runtime = wired.anchor.hooks, wired.anchor.RUNTIME
    store = wired.binding.store

    def broken():
        raise PermissionError("policy.json: permission denied")

    monkeypatch.setattr(store, "read", broken)

    def run(route):
        token = runtime.route.set(route)
        try:
            return hooks.on_pre_llm_call(session_id="s", turn_id="t-" + route.lane)
        finally:
            runtime.route.reset(token)

    operator = _route(wired, "operator", author_id=OPERATOR)
    assert run(operator) == {"context": wired.anchor.mod("policy.lanes").capability_lane_text(operator)}
    assert _outcome(wired, wired.wiring.context_key(operator)) == "fallback"
    public = _route(wired, "public")
    assert run(public)["action"] == "block"
    # The block is visible in the status, not only in the log.
    assert _outcome(wired, wired.wiring.context_key(public)) == "off"
    assert "public turns are blocked" in runtime.status.get(wired.wiring.context_key(public)).reason


# ── bind steps ───────────────────────────────────────────────────────────────────────────────────────


def test_bind_steps_run_in_order_after_the_bot_is_bound(wired, monkeypatch):
    bootstrap = wired.anchor.bootstrap
    assert [name for name, _ in bootstrap.BIND_STEPS] == ["K1", "K1b", "K3/K4", "K2/K2b", "K6"]
    seen = []
    steps = tuple((name, (lambda n: lambda bind: seen.append((n, bind.binding.bot_id)))(name))
                  for name, _ in bootstrap.BIND_STEPS)
    monkeypatch.setattr(bootstrap, "BIND_STEPS", steps[:2] + (("boom", lambda bind: 1 / 0),) + steps[2:])
    interventions = wired.anchor.mod("adapt.interventions")
    monkeypatch.setattr(interventions, "bind_adapter_class", lambda cls: True)
    interventions.discord_platform_handler(SimpleNamespace(application_id=int(BOT)), wired.adapter)
    assert seen == [(n, BOT) for n in ("K1", "K1b", "K3/K4", "K2/K2b", "K6")]  # a failing step stops nothing
    assert (wired.owner / "plugin-data" / "ctx_discord_policy" / "status.json").exists()
    seen.clear()

    class Unknown:
        _client = SimpleNamespace(application_id=None, user=None)

    interventions.discord_platform_handler(SimpleNamespace(application_id=None), Unknown())
    assert seen == [] and _outcome(wired, "K6") is None  # no bot id: nothing is installed, not even the interface


def test_status_file_names_adapted_degraded_and_passing_entries_against_the_last_start(wired, caplog):
    """Compared with the last status file: an outcome that changed but still works is "adapted to this
    Hermes", a fallen-back one "degraded" (both warnings); a per-turn notice from the last process is only info."""
    bootstrap, status, runtime = wired.anchor.bootstrap, wired.status, wired.anchor.RUNTIME
    runtime.status.record(status.Result("K1", "K", status.Outcome.INJECTED, "delete_message added"))
    runtime.status.record(status.Result("K3", "K", status.Outcome.WRAPPED, "delivery wrapped"))
    path = bootstrap.write_status(wired.owner)
    last = json.loads(path.read_text(encoding="utf-8"))  # the last process also had a per-turn notice
    last["interventions"][f"A9[{BOT}/{GUILD}/public/direct]"] = {"outcome": "fallback", "klass": "S", "reason": "x"}
    path.write_text(json.dumps(last), encoding="utf-8")
    runtime.status.record(status.Result("K1", "K", status.Outcome.NATIVE, "Hermes deletes itself"))
    runtime.status.record(status.Result("K3", "K", status.Outcome.FALLBACK, "send changed shape"))
    caplog.clear()
    caplog.set_level(logging.INFO, logger="ctx_discord_policy")
    bootstrap.write_status(wired.owner)
    lines = {r.getMessage(): r.levelno for r in caplog.records if "since the last start" in r.getMessage()}
    adapted = [m for m, level in lines.items() if level == logging.WARNING and "adapted to this Hermes" in m]
    degraded = [m for m, level in lines.items() if level == logging.WARNING and "degraded" in m]
    notices = [m for m, level in lines.items() if level == logging.INFO and "A9[" in m]
    assert len(adapted) == 1 and "K1: injected -> native" in adapted[0], lines
    assert len(degraded) == 1 and "K3: wrapped -> fallback" in degraded[0], lines
    assert len(notices) == 1 and "no longer reported" in notices[0], lines


def test_turn_runner_class(anchor, monkeypatch):
    hermes = anchor.mod("hermes")
    monkeypatch.setitem(sys.modules, "gateway.run_turn_runner", None)  # import fails
    assert hermes.turn_runner_class() is None
    fake = type("TurnRunner", (), {})
    monkeypatch.setitem(sys.modules, "gateway.run_turn_runner", SimpleNamespace(TurnRunner=fake))
    assert hermes.turn_runner_class() is fake


def test_delete_goes_to_the_chat_the_message_really_lives_in(wired, monkeypatch):
    w = wired.wiring
    calls = []

    class Base:
        async def delete_message(self, chat_id, message_id):
            return False

    Base.__name__ = Base.__qualname__ = "BasePlatformAdapter"

    class Native(Base):
        async def delete_message(self, chat_id, message_id):
            calls.append((chat_id, message_id))
            return True

    bind = SimpleNamespace(adapter_cls=Native)
    assert w.install_delete_chat(bind).outcome.value == "wrapped"
    assert w.install_delete_chat(bind).reason.endswith("(already)")
    monkeypatch.setattr(w.worklog, "message_chat", lambda chat, mid: "worklog-thread" if mid == "m1" else str(chat))
    assert asyncio.run(Native().delete_message("origin", "m1")) is True
    asyncio.run(Native().delete_message("origin", "m2"))
    assert calls == [("worklog-thread", "m1"), ("origin", "m2")]

    class Stock(Base):  # only the core's no-op: nothing to translate (K1 did not inject)
        pass

    assert w.install_delete_chat(SimpleNamespace(adapter_cls=Stock)).outcome.value == "off"
    assert "delete_message" not in Stock.__dict__


def test_ui_is_installed_on_the_bot_bindings_own_store(wired):
    discord = pytest.importorskip("discord")
    from discord.ext import commands

    items = wired.anchor.mod("ui.items")
    native = commands.Bot(command_prefix="!", intents=discord.Intents.none(), application_id=int(BOT))
    bind = wired.wiring.Bind(native=native, adapter=wired.adapter, binding=wired.binding)
    first = wired.wiring.install_ui(bind)
    assert first is not None and items.binding_for(native) is first and first.store is wired.binding.store
    second = wired.wiring.install_ui(bind)  # the factory again for the same client: no doubled listeners
    assert items.binding_for(native) is second
    assert [l for l in native.extra_events.get("on_ready", []) if getattr(l, "__self__", None) is first] == []
    assert _outcome(wired, "K6") == "ok"


# ── closed states ────────────────────────────────────────────────────────────────────────────────────


def test_armed_entry_without_home_is_closed_reported_once_per_home(anchor, caplog):
    runtime = anchor.RUNTIME
    ghost = anchor.runtime.Instance(home_key="/x/home", profile="default", plugin_id="p", plugin_dir="/d")
    runtime.armed.arm(ghost)
    runtime.status.record(anchor.mod("adapt.status").Result("A8[default]", "S", anchor.mod("adapt.status").Outcome.ARMED,
                                                            "another home of the same profile name"))
    assert runtime.armed.is_armed("/x/home")
    caplog.set_level(logging.INFO, logger="ctx_discord_policy")
    assert [anchor.hooks.armed_live("/x/home") for _ in range(5)] == [False] * 5
    key = anchor.hooks.homeless_key(ghost)
    assert key != "A8[default]" and "no home path" in runtime.status.get(key).reason
    assert runtime.status.get("A8[default]").outcome.value == "armed"  # the other home's entry stays
    assert sum(1 for r in caplog.records if r.levelno >= logging.WARNING) == 1


def test_operator_list_is_one_public_read_with_readability(anchor):
    config, perms = anchor.mod("config"), anchor.mod("ui.perms")
    calls = []

    def reader(home, key, default):
        calls.append(key)
        return [OPERATOR, "not-an-id"]

    config.set_reader(reader)
    try:
        assert config.operator_list(anchor.home) == (frozenset({OPERATOR}), True)
        assert calls == ["operators"]
        assert perms.operator_list(anchor.home) == config.operator_list(anchor.home)

        def broken(home, key, default):
            raise RuntimeError("config.yaml could not be read")

        config.set_reader(broken)
        assert config.operator_list(anchor.home) == (frozenset(), False)
        assert config.operators(anchor.home) == frozenset()
        config.set_reader(lambda home, key, default: None)
        assert config.operator_list(anchor.home) == (frozenset(), True)  # readable, nobody configured
    finally:
        config.set_reader(None)
    assert "config._raw" not in Path(perms.__file__).read_text(encoding="utf-8")


# ── store: revisions never repeat across a purge ─────────────────────────────────────────────────────


def test_stale_panel_revision_never_matches_after_purge_and_reactivation(anchor):
    file, ops = anchor.mod("store.file"), anchor.mod("store.ops")
    store = file.PolicyStore(anchor.tmp / "owner2" / "plugin-data" / "ctx_discord_policy" / "policy.json")
    ops.activate(store, BOT, GUILD, by="op", expected_rev=0)
    ops.update_fields(store, BOT, GUILD, {"progress.grouping": "separate"}, by="op", expected_rev=1)
    stale = store.read().guild_rev(BOT, GUILD)                     # a panel shows rev 2
    ops.purge_checked(store, BOT, GUILD, by="op", expected_rev=stale)
    assert store.read().guild(BOT, GUILD) is None
    tomb = store.read().guild_rev(BOT, GUILD)
    assert tomb > stale                                            # a fresh panel on the deleted guild
    with pytest.raises(file.ConflictError):
        ops.activate(store, BOT, GUILD, by="op", expected_rev=0)   # an old "activate" from before the guild existed
    ops.activate(store, BOT, GUILD, by="op", expected_rev=tomb)
    ops.update_fields(store, BOT, GUILD, {"progress.grouping": "rotate"}, by="op",
                      expected_rev=store.read().guild_rev(BOT, GUILD))
    for old in range(0, store.read().guild_rev(BOT, GUILD)):        # every revision ever shown before is stale
        with pytest.raises(file.ConflictError):
            ops.update_fields(store, BOT, GUILD, {"progress.grouping": "default"}, by="mgr", expected_rev=old)
    data = json.loads(store.path.read_text(encoding="utf-8"))
    assert "purged" not in data["bots"][BOT]                       # the epoch lives in the guild again
    ops.purge(store, BOT, GUILD, by="cli")                         # the CLI purge (no check) keeps an epoch too
    assert store.read().guild_rev(BOT, GUILD) > tomb
    assert store.read().state == "ok"                              # the file validates with the epoch in it


def test_epochs_survive_purging_the_bots_last_guild(anchor):
    """Purging a bot's last guild removes its entry; the epochs of guilds purged before must stay."""
    file, ops = anchor.mod("store.file"), anchor.mod("store.ops")
    store = file.PolicyStore(anchor.tmp / "owner3" / "plugin-data" / "ctx_discord_policy" / "policy.json")
    for guild in (GUILD, OTHER_GUILD):
        ops.activate(store, BOT, guild, by="op", expected_rev=0)
    stale = store.read().guild_rev(BOT, GUILD)                     # a panel in GUILD shows rev 1
    ops.purge_checked(store, BOT, GUILD, by="op", expected_rev=stale)
    tomb = store.read().guild_rev(BOT, GUILD)
    ops.purge(store, BOT, OTHER_GUILD, by="cli")                   # the bot's last guild
    assert store.read().guild_rev(BOT, GUILD) == tomb
    assert store.read().guild_rev(BOT, OTHER_GUILD) > 1
    ops.activate(store, BOT, GUILD, by="op", expected_rev=tomb)
    for old in (0, stale):
        with pytest.raises(file.ConflictError):
            ops.update_fields(store, BOT, GUILD, {"progress.grouping": "rotate"}, by="mgr", expected_rev=old)
    assert store.read().state == "ok"
