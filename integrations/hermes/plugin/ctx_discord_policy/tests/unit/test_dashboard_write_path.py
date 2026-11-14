"""Settings page P0: the shared write path both interfaces use (SETTINGS-PAGE §2.2, §3.3, F7, F10).

* role ``host`` — the Hermes settings page acts as the host: every setting of the rights table, the
  operator list included; Discord can never produce it (``resolve``/``from_interaction``);
* audit origin ``via`` (``cli`` | ``discord`` | ``dashboard``) on every audit entry a write produces;
* ``import_guild_checked`` — an import with the optimistic ``rev`` check;
* ``checks.environment`` — what ``validate_guild`` cannot know: installed skills, known channels/roles/emojis.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

BOT = "900000000000000001"
GUILD = "900000000000000002"
OP = "900000000000000011"
ADMIN = "900000000000000021"
ROLE = "900000000000000041"
CHANNEL = "900000000000000051"
OTHER_CHANNEL = "900000000000000052"


@pytest.fixture
def perms(anchor):
    config = anchor.mod("config")
    config.set_reader(lambda home, key, default: {"operators": [OP]}.get(key, default))
    yield anchor.mod("ui.perms")
    config.set_reader(None)


@pytest.fixture
def store_mod(anchor):
    ns = SimpleNamespace()
    ns.file = anchor.mod("store.file")
    ns.ops = anchor.mod("store.ops")
    ns.schema = anchor.mod("store.schema")
    ns.store = ns.file.PolicyStore(anchor.tmp / "owner" / "plugin-data" / "ctx_discord_policy" / "policy.json")
    return ns


# ── role host ───────────────────────────────────────────────────────────────────────────────────


def test_host_may_every_setting_of_the_table_including_the_operator_list(perms):
    host = perms.Actor("dashboard", perms.HOST)
    for setting in perms.TABLE:
        assert perms.may(host, setting.key), setting.key
        perms.require(host, setting.key)
    assert perms.may(host, "operators")
    assert not perms.may(host, "no.such.setting")
    assert set(perms.visible(host)) == set(perms.SETTINGS)


def test_host_writes_still_stay_inside_the_setting_they_name(perms):
    host = perms.Actor("dashboard", perms.HOST)
    perms.check_write(host, "progress", ["progress.grouping"])
    with pytest.raises(perms.Refused):
        perms.check_write(host, "progress", ["progress.grouping", "worklog.channel"])


def test_the_operator_list_stays_closed_to_every_discord_role(perms):
    for role in perms.ROLES:
        assert not perms.may(perms.Actor("x", role), "operators"), role


@pytest.mark.parametrize("mode", ["discord_admins", "role", "operators_only"])
@pytest.mark.parametrize("user", [OP, ADMIN, ""])
@pytest.mark.parametrize("manage", [True, False])
def test_resolve_never_produces_the_host_role(perms, anchor, mode, user, manage):
    guild = {"managers": {"mode": mode, "role_ids": [ROLE]}}
    actor = perms.resolve(user_id=user, manage_guild=manage, role_ids=[ROLE], owner_home=anchor.home,
                          guild_config=guild)
    assert actor.role in perms.ROLES and actor.role != perms.HOST
    assert perms.HOST not in perms.ROLES


def test_a_discord_administrator_and_the_server_owner_cannot_reach_the_operator_list(perms, anchor):
    """M5: nothing an interaction carries (Administrator, Manage Server, the owner's own id) maps to host."""
    for user in (ADMIN, OP):
        interaction = SimpleNamespace(
            permissions=SimpleNamespace(manage_guild=True, administrator=True),
            user=SimpleNamespace(id=int(user), roles=[SimpleNamespace(id=int(ROLE))]),
            guild=SimpleNamespace(owner_id=int(user)))
        actor = perms.from_interaction(interaction, owner_home=anchor.home, guild_config=None)
        assert actor.role != perms.HOST
        with pytest.raises(perms.Refused):
            perms.check_write(actor, "operators", [])


# ── audit origin ────────────────────────────────────────────────────────────────────────────────


def _audit(snapshot):
    return snapshot.data["audit"]


def test_update_records_the_origin_of_a_write_on_every_audit_entry(store_mod):
    snap = store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")
    assert all("via" not in entry for entry in _audit(snap))  # older entries keep their shape
    with store_mod.ops.origin("dashboard"):
        snap = store_mod.ops.update_fields(store_mod.store, BOT, GUILD, {"progress.grouping": "rotate"},
                                           by="dashboard", expected_rev=1)
    last = _audit(snap)[-1]
    assert (last["field"], last["by"], last["via"]) == ("progress.grouping", "dashboard", "dashboard")
    assert snap.guild(BOT, GUILD)["updated_by"] == "dashboard"


def test_explicit_origin_wins_and_unknown_origins_are_refused(store_mod):
    store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")

    def mutate(document):
        document["bots"][BOT]["guilds"][GUILD]["progress"]["grouping"] = "separate"
        return [{"bot": BOT, "guild": GUILD, "field": "progress.grouping", "old": "default", "new": "separate"}]

    with store_mod.ops.origin("cli"):
        snap = store_mod.store.update(mutate, by=OP, via="discord")
    assert _audit(snap)[-1]["via"] == "discord"
    with pytest.raises(ValueError):
        store_mod.store.update(mutate, by=OP, via="browser")
    with pytest.raises(ValueError):
        with store_mod.ops.origin("somewhere"):
            pass


def test_origin_is_context_local_and_restored(store_mod):
    file = store_mod.file
    assert file.AUDIT_VIA.get() is None
    with store_mod.ops.origin("dashboard"):
        assert file.AUDIT_VIA.get() == "dashboard"
        with store_mod.ops.origin("discord"):
            assert file.AUDIT_VIA.get() == "discord"
        assert file.AUDIT_VIA.get() == "dashboard"
    assert file.AUDIT_VIA.get() is None


def test_audit_entries_with_origin_stay_readable(store_mod):
    store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli", )
    with store_mod.ops.origin("dashboard"):
        store_mod.ops.set_state(store_mod.store, BOT, GUILD, "paused", by="dashboard", expected_rev=1)
    snap = store_mod.file.PolicyStore(store_mod.store.path).read()  # a fresh reader, strict validation
    assert snap.state == "ok" and _audit(snap)[-1]["via"] == "dashboard"


# ── import with rev ─────────────────────────────────────────────────────────────────────────────


def test_import_checked_refuses_a_stale_revision_and_keeps_the_guild(store_mod):
    ops, store = store_mod.ops, store_mod.store
    ops.bind(store, BOT, GUILD, by="cli")
    ops.update_fields(store, BOT, GUILD, {"progress.grouping": "rotate"}, by=OP, expected_rev=1)
    payload = store_mod.schema.guild_defaults()
    payload["identity"]["bot_name"] = "Imported"
    with pytest.raises(store_mod.file.ConflictError):
        ops.import_guild_checked(store, BOT, GUILD, payload, by="dashboard", prompt_cap=6000, expected_rev=1)
    assert store.read().guild(BOT, GUILD)["identity"]["bot_name"] == ""
    with ops.origin("dashboard"):
        snap = ops.import_guild_checked(store, BOT, GUILD, payload, by="dashboard", prompt_cap=6000,
                                        expected_rev=2)
    guild = snap.guild(BOT, GUILD)
    assert guild["identity"]["bot_name"] == "Imported" and guild["rev"] == 3
    assert guild["state"] == "unbound"   # the payload's state applies, bookkeeping stays the store's
    assert {a.get("via") for a in _audit(snap)[-2:]} == {"dashboard"}


def test_import_checked_validates_strictly(store_mod):
    store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")
    with pytest.raises(store_mod.schema.SchemaError):
        store_mod.ops.import_guild_checked(store_mod.store, BOT, GUILD, {"operators": [OP]}, by="dashboard",
                                           prompt_cap=6000, expected_rev=1)


# ── checks.environment ──────────────────────────────────────────────────────────────────────────


@pytest.fixture
def checks(anchor):
    return anchor.mod("store.checks")


def _codes(findings):
    return {(f.level, f.field, f.code) for f in findings}


def test_a_skill_the_target_profile_lacks_is_an_error(checks):
    env = checks.Environment(skills=frozenset({"faq-lookup"}), skills_profile="community")
    findings = checks.environment({"lanes.skills": ["faq-lookup", "missing-skill"]}, env)
    assert _codes(findings) == {("error", "lanes.skills", "skill_missing")}
    assert "missing-skill" in findings[0].message and "community" in findings[0].message
    assert checks.has_errors(findings)
    assert checks.environment({"lanes.skills": ["faq-lookup"]}, env) == []


def test_unknown_skill_inventory_is_a_warning_not_a_refusal(checks):
    findings = checks.environment({"lanes.skills": ["anything"]}, checks.Environment())
    assert _codes(findings) == {("warning", "lanes.skills", "skills_unknown")}
    assert not checks.has_errors(findings)


def test_ids_missing_from_the_directory_are_warnings(checks):
    env = checks.Environment(channels={CHANNEL: "text"}, roles=frozenset({ROLE}), emojis=frozenset(),
                             directory_source="directory of 2026-11-14T01:12:00Z")
    findings = checks.environment({
        "conversation.ambient_channels": [CHANNEL, OTHER_CHANNEL],
        "worklog.channel": OTHER_CHANNEL,
        "managers.role_ids": [ROLE, "900000000000000049"],
        "identity.reaction_emoji": {"id": "900000000000000061", "name": "wave"},
        "audit_channel": None,
    }, env)
    assert _codes(findings) == {
        ("warning", "conversation.ambient_channels", "channel_unknown"),
        ("warning", "worklog.channel", "channel_unknown"),
        ("warning", "managers.role_ids", "role_unknown"),
        ("warning", "identity.reaction_emoji", "emoji_unknown"),
    }
    assert not checks.has_errors(findings)
    assert all("directory of 2026-11-14T01:12:00Z" in f.message for f in findings)


def test_channel_kinds_that_cannot_carry_the_setting_are_warned(checks):
    env = checks.Environment(channels={CHANNEL: "voice", OTHER_CHANNEL: "category"})
    findings = checks.environment({"worklog.channel": CHANNEL, "conversation.ambient_channels": [OTHER_CHANNEL]},
                                  env)
    assert _codes(findings) == {("warning", "worklog.channel", "channel_kind"),
                                ("warning", "conversation.ambient_channels", "channel_kind")}


def test_without_a_directory_nothing_is_claimed_about_ids(checks):
    findings = checks.environment({"conversation.ambient_channels": [CHANNEL], "managers.role_ids": [ROLE],
                                   "identity.reaction_emoji": "\U0001F44D"}, checks.Environment())
    assert findings == []


def test_findings_serialise_for_both_interfaces(checks):
    env = checks.Environment(skills=frozenset())
    (finding,) = checks.environment({"lanes.skills": ["x"]}, env)
    assert finding.as_dict() == {"level": "error", "field": "lanes.skills", "code": "skill_missing",
                                 "message": finding.message}


# ── review round: origin across threads, every entry, no-op import, profile, kinds, flatten ─────


def test_origin_does_not_leak_into_a_write_on_another_thread(store_mod):
    import threading

    store_mod.ops.bind(store_mod.store, BOT, GUILD, by="cli")
    inside, release = threading.Event(), threading.Event()

    def holder():
        with store_mod.ops.origin("dashboard"):
            inside.set()
            release.wait(5)

    thread = threading.Thread(target=holder)
    thread.start()
    try:
        assert inside.wait(5)
        snap = store_mod.ops.set_state(store_mod.store, BOT, GUILD, "paused", by=OP, expected_rev=1)
    finally:
        release.set()
        thread.join(5)
    assert "via" not in _audit(snap)[-1]


def test_origin_applies_to_every_entry_of_a_write(store_mod):
    with store_mod.ops.origin("dashboard"):
        snap = store_mod.ops.bind(store_mod.store, BOT, GUILD, by="dashboard")
    entries = _audit(snap)
    assert len(entries) == 2 and {e["via"] for e in entries} == {"dashboard"}


def test_a_no_op_import_with_a_stale_revision_is_still_a_conflict(store_mod):
    ops, store = store_mod.ops, store_mod.store
    snap = ops.bind(store, BOT, GUILD, by="cli")
    same = {k: v for k, v in snap.guild(BOT, GUILD).items()}
    ops.update_fields(store, BOT, GUILD, {"progress.grouping": "rotate"}, by=OP, expected_rev=1)
    same = store.read().guild(BOT, GUILD)
    with pytest.raises(store_mod.file.ConflictError):
        ops.import_guild_checked(store, BOT, GUILD, same, by="dashboard", prompt_cap=6000, expected_rev=1)
    snap = ops.import_guild_checked(store, BOT, GUILD, same, by="dashboard", prompt_cap=6000, expected_rev=2,
                                    via="dashboard")
    assert _audit(snap)[-1]["field"] == "import" and _audit(snap)[-1]["via"] == "dashboard"


def test_a_profile_that_does_not_exist_is_an_error(checks):
    env = checks.Environment(profiles=frozenset({"default", "community"}))
    assert _codes(checks.environment({"routing.profile": "missing"}, env)) == {
        ("error", "routing.profile", "profile_missing")}
    assert checks.environment({"routing.profile": "community"}, env) == []
    assert checks.environment({"routing.profile": None}, env) == []
    assert checks.environment({"routing.profile": "missing"}, checks.Environment()) == []


def test_an_excluded_category_is_warned_because_admission_never_matches_it(checks):
    env = checks.Environment(channels={CHANNEL: "category"})
    assert _codes(checks.environment({"conversation.excluded_channels": [CHANNEL]}, env)) == {
        ("warning", "conversation.excluded_channels", "channel_kind")}


def test_integer_ids_from_discord_compare_as_text_and_non_lists_are_left_to_the_schema(checks):
    env = checks.Environment(roles=frozenset({int(ROLE)}), channels={int(CHANNEL): "text"},
                             skills=frozenset({"faq"}))
    assert checks.environment({"managers.role_ids": [ROLE], "worklog.channel": CHANNEL}, env) == []
    assert checks.environment({"lanes.skills": "faq"}, env) == []


def test_flatten_turns_a_guild_into_checkable_changes(checks, store_mod):
    guild = store_mod.schema.guild_defaults()
    guild["identity"]["reaction_emoji"] = {"id": "900000000000000061", "name": "wave"}
    flat = checks.flatten(guild)
    assert flat["lanes.skills"] == [] and flat["identity.reaction_emoji"] == {"id": "900000000000000061",
                                                                              "name": "wave"}
    assert flat["prompts.template_overrides"] == {} and "lanes.channel_history.enabled" in flat
