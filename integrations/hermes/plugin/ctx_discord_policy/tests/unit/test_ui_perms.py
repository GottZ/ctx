"""S8: the rights table of /policy (DESIGN §4.2), complete per role x setting, and role resolution."""

from __future__ import annotations

import pytest

OP = "900000000000000011"
ADMIN = "900000000000000021"
OTHER = "900000000000000031"
ROLE_A = "900000000000000041"
ROLE_B = "900000000000000042"

O, M = "operator", "manager"

# DESIGN §4.2 written out independently of the code: who may use what.
EXPECTED = {
    "panel.open": {O, M},
    "guild.activate": {O},
    "guild.unbind": {O},
    "guild.request": {M},
    "guild.pause": {O, M},
    "channels": {O, M},
    "identity": {O, M},
    "guidance": {O, M},
    "progress": {O, M},
    "worklog": {O},
    "public_lanes": {O},
    "operator_lane": {O},
    "managers": {O},
    "templates": {O},
    "operators": set(),
    "status.full": {O},
    "status.summary": {O, M},
}


@pytest.fixture
def perms(anchor):
    config = anchor.mod("config")
    settings = {"operators": [OP]}
    config.set_reader(lambda home, key, default: settings.get(key, default))
    module = anchor.mod("ui.perms")
    module.settings = settings
    yield module
    config.set_reader(None)


def actor(perms, role):
    return perms.Actor("900000000000000099", role)


def test_table_covers_exactly_the_designed_settings(perms):
    assert set(perms.SETTINGS) == set(EXPECTED)
    assert len(perms.TABLE) == len(perms.SETTINGS)


@pytest.mark.parametrize("key", sorted(EXPECTED))
@pytest.mark.parametrize("role", ["operator", "manager", "member"])
def test_matrix(perms, key, role):
    allowed = role in EXPECTED[key]
    assert perms.may(actor(perms, role), key) is allowed
    if allowed:
        perms.require(actor(perms, role), key)
    else:
        with pytest.raises(perms.Refused) as exc:
            perms.require(actor(perms, role), key)
        assert str(exc.value)  # every refusal says why


def test_every_schema_field_belongs_to_exactly_one_setting(perms, anchor):
    schema = anchor.mod("store.schema")
    for path in schema.field_paths():
        owners = perms.owners_of(path)
        if path in perms.BOOKKEEPING:
            assert owners == [], path
        else:
            assert len(owners) == 1, (path, owners)


def test_operator_only_settings_are_exactly_the_locked_rows_of_the_design(perms):
    locked = {k for k, roles in EXPECTED.items() if roles == {O}}
    assert locked == {"guild.activate", "guild.unbind", "worklog", "public_lanes", "operator_lane", "managers",
                      "templates", "status.full"}
    for key in locked:
        assert not perms.may(actor(perms, "manager"), key)


def test_no_setting_writes_the_operator_list_or_authority(perms):
    written = {f for s in perms.TABLE for f in s.fields}
    assert not {f for f in written if "operator" in f and f not in ("operator_lane.channel_hydration",
                                                                       "prompts.guild_in_operator_lane")}
    assert perms.SETTINGS["operators"].roles == frozenset()


def test_state_transitions_need_the_matching_setting(perms):
    t = perms.TRANSITIONS
    assert t[("unbound", "active")] == "guild.activate"
    assert t[("active", "paused")] == t[("paused", "active")] == "guild.pause"
    assert t[("active", "unbound")] == t[("paused", "unbound")] == "guild.unbind"
    manager = actor(perms, "manager")
    assert perms.may(manager, perms.transition_setting("active", "paused"))
    assert perms.may(manager, perms.transition_setting("paused", "active"))
    assert not perms.may(manager, perms.transition_setting("unbound", "active"))
    assert not perms.may(manager, perms.transition_setting("active", "unbound"))
    assert perms.transition_setting("unbound", "paused") is None


def resolve(perms, home, user, *, manage=False, roles=(), guild=None):
    return perms.resolve(user_id=user, manage_guild=manage, role_ids=roles, owner_home=home, guild_config=guild)


def test_operator_comes_only_from_the_configuration(perms, anchor):
    home = anchor.home
    assert resolve(perms, home, OP).role == "operator"                     # even without Manage Server
    admin = resolve(perms, home, ADMIN, manage=True, roles=[ROLE_A])      # Administrator, "Operator" role …
    assert admin.role == "manager" and not admin.is_operator
    perms.settings["operators"] = [ADMIN]
    assert resolve(perms, home, ADMIN, manage=True).role == "operator"    # … until the config says so
    assert resolve(perms, home, OP).role == "member"                       # and removal applies at once


def test_reading_operators_failing_means_no_operator(perms, anchor):
    def broken(home, key, default):
        raise OSError("config unreadable")

    anchor.mod("config").set_reader(broken)
    assert resolve(perms, anchor.home, OP).role == "member"
    assert resolve(perms, anchor.home, OP, manage=True).role == "manager"


@pytest.mark.parametrize(("managers", "manage", "roles", "expected"), [
    (None, True, [], "manager"),
    ({"mode": "discord_admins", "role_ids": []}, True, [], "manager"),
    ({"mode": "discord_admins", "role_ids": []}, False, [], "member"),
    ({"mode": "role", "role_ids": [ROLE_A]}, True, [ROLE_A], "manager"),
    ({"mode": "role", "role_ids": [ROLE_A]}, True, [ROLE_B], "member"),
    ({"mode": "role", "role_ids": [ROLE_A]}, False, [ROLE_A], "member"),     # role without Manage Server
    ({"mode": "operators_only", "role_ids": []}, True, [], "member"),
    ({"mode": "something-new", "role_ids": []}, True, [], "member"),        # unknown mode fails closed
])
def test_manager_modes(perms, anchor, managers, manage, roles, expected):
    guild = None if managers is None else {"managers": managers}
    assert resolve(perms, anchor.home, ADMIN, manage=manage, roles=roles, guild=guild).role == expected


def test_member_refusals_name_the_reason_and_the_next_step(perms, anchor):
    plain = resolve(perms, anchor.home, OTHER)
    assert "Manage Server" in perms.refusal(plain, "panel.open") and "Next step" in perms.refusal(plain, "panel.open")
    role_mode = resolve(perms, anchor.home, ADMIN, manage=True, roles=[ROLE_B],
                        guild={"managers": {"mode": "role", "role_ids": [ROLE_A]}})
    assert f"<@&{ROLE_A}>" in perms.refusal(role_mode, "progress")
    closed = resolve(perms, anchor.home, ADMIN, manage=True, guild={"managers": {"mode": "operators_only"}})
    assert "only the bot's operator" in perms.refusal(closed, "progress")


def test_manager_refusals_point_to_the_operator(perms):
    manager = actor(perms, "manager")
    assert "Ask the operator to activate" in perms.refusal(manager, "guild.activate")
    for key in ("worklog", "public_lanes", "managers", "templates", "guild.unbind"):
        text = perms.refusal(manager, key)
        assert "Only the bot's operator" in text and "Next step" in text
    assert "Hermes configuration" in perms.refusal(manager, "operators")
    assert "activate the policy directly" in perms.refusal(actor(perms, "operator"), "guild.request")


def test_check_write_refuses_paths_outside_the_setting(perms):
    manager = actor(perms, "manager")
    perms.check_write(manager, "progress", ["progress.grouping"])
    with pytest.raises(perms.Refused):
        perms.check_write(manager, "progress", ["progress.grouping", "worklog.channel"])
    with pytest.raises(perms.Refused):
        perms.check_write(manager, "worklog", ["worklog.channel"])
    perms.check_write(actor(perms, "operator"), "worklog", ["worklog.channel"])


def test_visible_settings_are_derived_from_the_table(perms):
    for role in ("operator", "manager", "member"):
        assert set(perms.visible(actor(perms, role))) == {k for k, r in EXPECTED.items() if role in r}


class _Perms:
    def __init__(self, manage_guild=False, administrator=False):
        self.manage_guild = manage_guild
        self.administrator = administrator


class _Role:
    def __init__(self, rid):
        self.id = int(rid)


class _User:
    def __init__(self, uid, roles=(), raw=()):
        self.id = int(uid)
        self.roles = [_Role(r) for r in roles]
        self._roles = [int(r) for r in raw]


class _Interaction:
    def __init__(self, user, perms):
        self.user = user
        self.permissions = perms


def test_from_interaction_reads_discord_permissions_and_roles(perms, anchor):
    home = anchor.home
    admin_flag = _Interaction(_User(ADMIN), _Perms(administrator=True))
    assert perms.from_interaction(admin_flag, owner_home=home, guild_config=None).role == "manager"
    none = _Interaction(_User(ADMIN), _Perms())
    assert perms.from_interaction(none, owner_home=home, guild_config=None).role == "member"
    guild = {"managers": {"mode": "role", "role_ids": [ROLE_A]}}
    cached = _Interaction(_User(ADMIN, roles=[ROLE_A]), _Perms(manage_guild=True))
    assert perms.from_interaction(cached, owner_home=home, guild_config=guild).role == "manager"
    uncached = _Interaction(_User(ADMIN, raw=[ROLE_A]), _Perms(manage_guild=True))
    assert perms.from_interaction(uncached, owner_home=home, guild_config=guild).role == "manager"
    operator = _Interaction(_User(OP), _Perms())
    assert perms.from_interaction(operator, owner_home=home, guild_config=None).role == "operator"
