"""S8: custom_id schema, the additive store operations of the interface, the K6 probe and request throttle."""

from __future__ import annotations

import json
import re
import sys
import types

import pytest

BOT = "900000000000000001"
GUILD = "900000000000000002"
OP = "900000000000000011"


# ── custom_id ────────────────────────────────────────────────────────────────────────────────────────


def test_custom_id_roundtrip_and_limit(anchor):
    items = anchor.mod("ui.items")
    cid = items.encode(GUILD, "prog_set", "rotate", 7)
    assert cid == f"dsp:1:{GUILD}:prog_set:rotate:7"
    assert items.parse(cid) == items.Ref(GUILD, "prog_set", "rotate", 7)
    longest = items.encode("9" * 20, "a" * 16, "b" * 32, 10 ** 12 - 1)
    assert len(longest) <= items.MAX_LEN
    assert re.fullmatch(items.TEMPLATE, longest)


@pytest.mark.parametrize("bad", [
    "dsp:2:900000000000000002:pause::1", "dsp:1:900000000000000002:pause::", "dsp:1:x:pause::1",
    "dsp:1:900000000000000002:pause:a b:1", "dsp:1:900000000000000002:pause", "other:1:2:pause::1",
    "dsp:1:0900000000000000002:pause::1", "dsp:1:900000000000000002:pause::01", None, "dsp:" + "9" * 120,
])
def test_custom_id_parse_refuses_anything_else(anchor, bad):
    items = anchor.mod("ui.items")
    with pytest.raises(items.BadId) as exc:
        items.parse(bad)
    assert "run /policy again" in str(exc.value)


@pytest.mark.parametrize(("args", "field"), [
    (("abc", "pause"), "guild"), ((GUILD, "Pause"), "action"), ((GUILD, "x" * 17), "action"),
    ((GUILD, "nav", "a:b"), "arg"), ((GUILD, "nav", "", -1), "rev"), ((GUILD, "nav", "", True), "rev"),
])
def test_custom_id_encode_validates(anchor, args, field):
    with pytest.raises(ValueError, match=field):
        anchor.mod("ui.items").encode(*args)


def test_clean_strips_line_breaks_mentions_and_markdown(anchor):
    clean = anchor.mod("ui.panels").clean
    assert clean("a\nb\r\n**c** <@1> @everyone #x", 100) == "a b \\*\\*c\\*\\* \\<\\@1\\> \\@everyone \\#x"
    assert len(clean("x" * 500, 40)) == 40
    assert clean("Name\u2028Next\x85End\u2029", 100, escape=False) == "Name Next End"


# ── additive store operations ────────────────────────────────────────────────────────────────────────


@pytest.fixture
def store(anchor):
    return anchor.mod("cli").store_for(anchor.home)


def test_update_fields_validates_and_audits(anchor, store):
    ops, schema = anchor.mod("store.ops"), anchor.mod("store.schema")
    ops.bind(store, BOT, GUILD, by="cli")
    snap = ops.update_fields(store, BOT, GUILD, {"progress.grouping": "rotate"}, by=OP, expected_rev=1)
    guild = snap.guild(BOT, GUILD)
    assert guild["progress"]["grouping"] == "rotate" and guild["rev"] == 2 and guild["updated_by"] == OP
    assert snap.data["audit"][-1]["field"] == "progress.grouping"
    with pytest.raises(schema.SchemaError):
        ops.update_fields(store, BOT, GUILD, {"progress.grouping": "accumulate"}, by=OP)
    for path in ("state", "rev", "bound_by", "updated_by", "operators", "progress", "nope.x"):
        with pytest.raises(schema.SchemaError, match="not a setting"):
            ops.update_fields(store, BOT, GUILD, {path: "x"}, by=OP)
    with pytest.raises(schema.SchemaError, match="not configured"):
        ops.update_fields(store, BOT, "900000000000000003", {"progress.grouping": "rotate"}, by=OP)
    assert store.read().guild(BOT, GUILD)["rev"] == 2


def test_update_fields_conflict_and_noop(anchor, store):
    ops, file = anchor.mod("store.ops"), anchor.mod("store.file")
    ops.bind(store, BOT, GUILD, by="cli")
    with pytest.raises(file.ConflictError):
        ops.update_fields(store, BOT, GUILD, {"progress.grouping": "rotate"}, by=OP, expected_rev=0)
    snap = ops.update_fields(store, BOT, GUILD, {"progress.grouping": "default"}, by=OP, expected_rev=0)
    assert snap.guild(BOT, GUILD)["rev"] == 1  # unchanged value: nothing to conflict with, no new revision


def test_update_fields_prompt_cap(anchor, store):
    ops, schema = anchor.mod("store.ops"), anchor.mod("store.schema")
    ops.bind(store, BOT, GUILD, by="cli")
    with pytest.raises(schema.SchemaError, match="limit"):
        ops.update_fields(store, BOT, GUILD, {"prompts.guild": "x" * 11}, by=OP, prompt_cap=10)
    ops.update_fields(store, BOT, GUILD, {"prompts.guild": "x" * 10}, by=OP, prompt_cap=10)


def test_activate_and_purge_checked_respect_rev(anchor, store):
    ops, file = anchor.mod("store.ops"), anchor.mod("store.file")
    with pytest.raises(file.ConflictError):
        ops.activate(store, BOT, GUILD, by=OP, expected_rev=3)
    assert store.read().guild(BOT, GUILD) is None
    snap = ops.activate(store, BOT, GUILD, by=OP, expected_rev=0)
    assert snap.guild(BOT, GUILD)["state"] == "active" and snap.guild(BOT, GUILD)["bound_by"] == OP
    with pytest.raises(file.ConflictError):
        ops.purge_checked(store, BOT, GUILD, by=OP, expected_rev=0)
    assert ops.purge_checked(store, BOT, GUILD, by=OP, expected_rev=1).guild(BOT, GUILD) is None


def test_ui_writes_are_refused_on_a_corrupt_store(anchor, store):
    ops, file = anchor.mod("store.ops"), anchor.mod("store.file")
    ops.bind(store, BOT, GUILD, by="cli")
    raw = json.loads(store.path.read_text())
    raw["bots"][BOT]["guilds"][GUILD]["progress"]["grouping"] = "accumulate"
    store.path.write_text(json.dumps(raw))
    with pytest.raises(file.StoreError):
        ops.update_fields(store, BOT, GUILD, {"progress.grouping": "rotate"}, by=OP)


# ── K6 probe ─────────────────────────────────────────────────────────────────────────────────────────


class _Tree:
    def add_command(self, *a, **k): ...
    def remove_command(self, *a, **k): ...
    def get_commands(self, *a, **k): return []
    async def sync(self, *, guild=None): return []


class _Native:
    def __init__(self, tree=None, dynamic=True):
        self.tree = tree or _Tree()
        if dynamic:
            self.add_dynamic_items = lambda *items: None


def _fake_discord(*, parts=("Label", "FileUpload", "RadioGroup", "Checkbox", "DynamicItem"), command=True):
    ui = types.SimpleNamespace(**{name: type(name, (), {}) for name in parts})
    app = types.SimpleNamespace(**({"Command": type("Command", (), {})} if command else {}))
    return types.SimpleNamespace(ui=ui, app_commands=app)


@pytest.mark.parametrize(("discord", "native", "mode"), [
    (_fake_discord(), _Native(), "full"),
    (_fake_discord(parts=("Label", "DynamicItem")), _Native(), "classic"),
    (_fake_discord(), _Native(dynamic=False), "views"),
    (_fake_discord(parts=("Label",)), _Native(), "views"),
    (_fake_discord(command=False), _Native(), "off"),
    (None, _Native(), "off"),
], ids=["full", "no-modern-parts", "no-add-dynamic-items", "no-dynamic-item-class", "no-command", "no-discord"])
def test_probe_strategies(anchor, discord, native, mode):
    caps = anchor.mod("ui.commands").probe(discord, native)
    assert caps.mode == mode
    assert caps.reason


def test_probe_needs_guild_sync(anchor):
    class OldTree(_Tree):
        async def sync(self):
            return []

    assert anchor.mod("ui.commands").probe(_fake_discord(), _Native(OldTree())).mode == "off"


def test_install_records_k6_and_never_raises(anchor, monkeypatch):
    commands = anchor.mod("ui.commands")
    monkeypatch.setitem(sys.modules, "discord", None)
    assert commands.install(_Native(), None, bot_id=BOT, owner_home=anchor.home, discord_module=None) is None
    result = anchor.RUNTIME.status.get("K6")
    assert result.outcome.value == "off" and "CLI" in result.reason

    class Broken(_Native):
        @property
        def guilds(self):
            raise RuntimeError("client in a strange state")

    assert commands.install(Broken(), None, bot_id=BOT, owner_home=anchor.home,
                            discord_module=_fake_discord()) is None
    result = anchor.RUNTIME.status.get("K6")
    assert result.outcome.value == "off" and "installing the interface failed" in result.reason


def test_ui_package_imports_without_discord(anchor, monkeypatch):
    monkeypatch.setitem(sys.modules, "discord", None)
    for name in ("ui.perms", "ui.items", "ui.modals", "ui.panels", "ui.pages", "ui.commands"):
        assert anchor.mod(name)
    panels = anchor.mod("ui.panels")
    assert [p.key for p in panels.registered_panels()] == ["progress"]
    assert {"prog_edit", "prog_set"} <= set(panels.ACTIONS) and "m_progress" in panels.MODAL_ACTIONS
    for act in panels.ACTIONS.values():          # every action names a known setting (or derives one)
        assert callable(act.setting) or act.setting in anchor.mod("ui.perms").SETTINGS
    for act in panels.MODAL_ACTIONS.values():
        assert act.setting in anchor.mod("ui.perms").SETTINGS


# ── activation request throttle ──────────────────────────────────────────────────────────────────────


def test_activation_requests_once_per_day_per_server(anchor, tmp_path):
    requests = anchor.mod("ui.commands").ActivationRequests(tmp_path / "requests.json")
    assert requests.claim(BOT, GUILD, now=1000.0) == (True, None)
    assert requests.claim(BOT, GUILD, now=1000.0 + 3600) == (False, 1000.0)
    assert requests.claim(BOT, "900000000000000003", now=1001.0)[0]       # another server
    assert requests.claim(BOT, GUILD, now=1000.0 + 86400)[0]               # a day later
    requests.release(BOT, GUILD)
    assert requests.claim(BOT, GUILD, now=1000.0 + 86401)[0]
    assert oct((tmp_path / "requests.json").stat().st_mode & 0o777) == "0o600"
    (tmp_path / "requests.json").write_text("not json")
    assert requests.claim(BOT, GUILD, now=5.0)[0]                          # damaged file: start over


def test_activation_request_claims_are_exclusive_across_threads(anchor, tmp_path):
    import threading

    commands = anchor.mod("ui.commands")
    for round_ in range(30):
        requests = commands.ActivationRequests(tmp_path / f"r{round_}.json")
        barrier = threading.Barrier(4)
        results = []

        def claim(guild):
            barrier.wait()
            results.append((guild, requests.claim(BOT, guild, now=1000.0)[0]))

        threads = [threading.Thread(target=claim, args=(g,)) for g in (GUILD, GUILD, "900000000000000003",
                                                                       "900000000000000003")]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert sorted(results) == sorted([(GUILD, True), (GUILD, False), ("900000000000000003", True),
                                          ("900000000000000003", False)]), results
        stored = json.loads((tmp_path / f"r{round_}.json").read_text())
        assert set(stored) == {f"{BOT}:{GUILD}", f"{BOT}:900000000000000003"}   # no lost entry
