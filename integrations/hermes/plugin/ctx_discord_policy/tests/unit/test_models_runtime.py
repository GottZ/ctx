"""S13: the model per server — store operations, effective model, save-time checks and the K8 wrappers."""

from __future__ import annotations

import logging
from types import SimpleNamespace

import pytest
from fakes.ctx import FakeCtx
from unit.routing_fakes import FakeHermes

BOT, GUILD = "900000000000000300", "900000000000000100"
CHANNEL, MEMBER, OP = "900000000000000200", "900000000000000500", "900000000000000400"
DS = {"provider": "deepseek", "model": "deepseek-chat"}
OR = {"provider": "openrouter", "model": "x/y"}


class Runner:
    """Hermes' surface K8 relies on (signatures as on every supported ref)."""

    def __init__(self, home):
        self.home = home
        self.overrides: dict = {}
        self.channel = None

    def _resolve_session_agent_runtime(self, *, source=None, session_key=None, user_config=None, fallback=True):
        self._pre_agent_fallback_notice = getattr(self, "notice", None)  # Hermes: notice of ITS runtime choice
        return "profile-model", {"provider": "profile-provider", "api_key": "FAKE-PROFILE-KEY"}

    def _fallback_baseline_model(self, session_key, source, agent, *, policy_model=None):
        return "profile-model"

    def _session_model_override(self, session_key):
        return self.overrides.get(session_key)

    def _resolve_session_key_or_none(self, source, session_key):
        return session_key or f"agent:main:discord:group:{source.chat_id}:{source.user_id}"

    def _resolve_profile_home_for_source(self, source):
        return self.home

    def _channel_override(self, platform, chat_id, thread_id, parent_id):
        return self.channel


@pytest.fixture
def world(anchor, monkeypatch):
    anchor.entry.register(FakeCtx("default"))
    fake = FakeHermes(anchor.home).install(monkeypatch, anchor.mod("hermes"))
    fake.credentials = {"default": {"deepseek": "available"}}
    anchor.mod("config").set_reader(lambda home, key, default: {"operators": [OP]}.get(key, default))
    store = anchor.mod("cli").store_for(anchor.home)

    class Adapter:
        gateway_runner = None

    adapter = Adapter()
    anchor.RUNTIME.bots.bind(adapter, BOT, anchor.home)
    store = anchor.RUNTIME.bots.for_bot(BOT).store
    anchor.mod("store.ops").bind(store, BOT, GUILD, by="cli")
    cls = type("GatewayRunner", (Runner,), {})
    result = anchor.mod("models.runtime").install(cls)
    assert result.outcome.value == "wrapped", result
    w = SimpleNamespace(anchor=anchor, fake=fake, store=store, cls=cls, runner=cls(anchor.home),
                        ops=anchor.mod("store.ops"), runtime=anchor.mod("models.runtime"),
                        check=anchor.mod("models.check"), adapter=adapter)
    yield w
    anchor.mod("config").set_reader(None)


def source(user=MEMBER):
    return SimpleNamespace(platform="discord", chat_id=CHANNEL, user_id=user, thread_id=None, parent_chat_id=None,
                           guild_id=GUILD, profile=None, message_id="1")


def route(anchor, lane="public", user=MEMBER):
    return anchor.routes.Route(bot_id=BOT, guild_id=GUILD, channel_id=CHANNEL, message_id="1", author_id=user,
                               lane=lane)


def in_turn(w, src, lane_route):
    """A turn as Hermes runs it: the A7 route set, inside the routed profile's scope."""
    token = w.anchor.RUNTIME.route.set(lane_route)
    try:
        with w.fake.profile_scope(w.anchor.home):
            return w.runner._resolve_session_agent_runtime(source=src, session_key=None)
    finally:
        w.anchor.RUNTIME.route.reset(token)


def set_models(w, **changes):
    w.ops.set_models(w.store, BOT, GUILD, {f"models.{k}": v for k, v in changes.items()}, by=OP)


def test_guild_model_applies_and_baseline_follows(world):
    set_models(world, public=DS)
    model, rt = in_turn(world, source(), route(world.anchor))
    assert model == "deepseek-chat" and rt["provider"] == "deepseek" and "model" not in rt
    agent = SimpleNamespace(model="deepseek-chat", provider="deepseek", _primary_runtime={"provider": "deepseek"})
    assert world.runner._fallback_baseline_model("agent:main:discord:group:%s:%s" % (CHANNEL, MEMBER), source(),
                                                 agent) == "deepseek-chat"


def test_hermes_fallback_notice_is_dropped_when_the_server_model_replaces_its_choice(world):
    """Review finding 7: Hermes' notice about ITS provider fallback must not reach members when K8 replaced
    that runtime with the server's model; it stays when Hermes' answer stands."""
    world.runner.notice = "switched to the fallback provider"
    assert in_turn(world, source(), route(world.anchor))[0] == "profile-model"
    assert world.runner._pre_agent_fallback_notice == "switched to the fallback provider"
    set_models(world, public=DS)
    assert in_turn(world, source(), route(world.anchor))[0] == "deepseek-chat"
    assert world.runner._pre_agent_fallback_notice is None


def test_install_wraps_both_or_neither(world):
    """M-M1: both methods carry the K8 mark; a class without one of them gets neither."""
    strategies = world.anchor.mod("adapt.strategies")
    assert strategies.is_own_wrapper(world.cls._resolve_session_agent_runtime, "K8")
    assert strategies.is_own_wrapper(world.cls._fallback_baseline_model, "K8")
    assert world.runtime.install(world.cls).outcome.value == "wrapped"    # idempotent
    partial = type("Older", (), {k: v for k, v in vars(Runner).items() if k != "_fallback_baseline_model"})
    result = world.runtime.install(partial)
    assert result.outcome.value == "fallback" and "_fallback_baseline_model" in result.reason
    assert not strategies.is_own_wrapper(partial._resolve_session_agent_runtime, "K8")


def test_session_model_wins(world):
    """M-M3: the session's own /model comes before the server's model (also in the baseline)."""
    set_models(world, public=DS)
    key = f"agent:main:discord:group:{CHANNEL}:{MEMBER}"
    world.runner.overrides[key] = {"model": "own-model"}
    assert in_turn(world, source(), route(world.anchor))[0] == "profile-model"
    assert world.runner._fallback_baseline_model(key, source(), SimpleNamespace(model="x")) == "profile-model"


def test_channel_override_wins(world):
    set_models(world, public=DS)
    world.runner.channel = SimpleNamespace(model="channel-model", provider=None)
    assert in_turn(world, source(), route(world.anchor))[0] == "profile-model"


def test_lane_comes_from_the_route_not_the_server(world):
    """M-M4: an operator session gets the operator model, a member's the members' model."""
    set_models(world, public=DS, operator={"provider": "deepseek", "model": "deepseek-reasoner"})
    assert in_turn(world, source(), route(world.anchor))[0] == "deepseek-chat"
    assert in_turn(world, source(OP), route(world.anchor, "operator", OP))[0] == "deepseek-reasoner"


def test_no_policy_route_no_change(world):
    set_models(world, public=DS)
    assert in_turn(world, source("900000000000000999"), None)[0] == "profile-model"


def test_missing_credential_falls_back(world):
    """M-M2: Hermes resolving a runtime is not enough; the profile must hold the credential."""
    set_models(world, public=OR)
    assert in_turn(world, source(), route(world.anchor))[0] == "profile-model"
    assert ("900000000000000300", GUILD, "public") in world.runtime.UNAVAILABLE
    k8 = world.anchor.RUNTIME.status.get("K8")
    assert k8.outcome.value == "wrapped" and k8.reason.endswith("1 server model unavailable")
    world.fake.credentials["default"]["openrouter"] = "available"
    assert in_turn(world, source(), route(world.anchor))[0] == "x/y"
    assert world.anchor.RUNTIME.status.get("K8").reason.endswith("0 server models unavailable")


def test_outside_the_routed_profile_scope_hermes_decides(world, monkeypatch):
    """Credentials of another profile are never used: out of the source's scope the answer stands, also
    when that other profile holds the provider's key (review finding 4)."""
    set_models(world, public=DS)
    world.fake.credentials["elsewhere"] = {"deepseek": "available"}
    token = world.anchor.RUNTIME.route.set(route(world.anchor))
    try:
        with world.fake.profile_scope(world.anchor.home / "profiles" / "elsewhere"):
            assert world.runner._resolve_session_agent_runtime(source=source())[0] == "profile-model"
    finally:
        world.anchor.RUNTIME.route.reset(token)


def test_manager_pick_counts_only_while_released(world):
    """M-M5: a pick outside public_choices is ignored (and refused when written)."""
    schema = world.anchor.mod("store.schema")
    with pytest.raises(schema.SchemaError, match="not released"):
        world.ops.set_manager_pick(world.store, BOT, GUILD, DS, by=MEMBER)
    set_models(world, public=OR, public_choices=[DS])
    world.ops.set_manager_pick(world.store, BOT, GUILD, DS, by=MEMBER)
    guild = world.store.read().guild(BOT, GUILD)
    assert world.check.effective(guild, "public") == DS
    set_models(world, public_choices=[])                                  # withdrawn: the pick is cleared
    guild = world.store.read().guild(BOT, GUILD)
    assert guild["models"]["manager_pick"] is None and world.check.effective(guild, "public") == OR
    guild["models"]["public_choices"], guild["models"]["manager_pick"] = [], DS   # hand-edited: still ignored
    assert world.check.effective(guild, "public") == OR
    assert world.check.effective(guild, "operator") is None


def test_wrapper_never_touches_route_or_lane(world):
    """M-M8: the route context variable and the session's lane are left as they were."""
    set_models(world, public=DS)
    current = route(world.anchor)
    token = world.anchor.RUNTIME.route.set(current)
    try:
        with world.fake.profile_scope(world.anchor.home):
            world.runner._resolve_session_agent_runtime(source=source())
        assert world.anchor.RUNTIME.route.get() is current and current.lane == "public"
    finally:
        world.anchor.RUNTIME.route.reset(token)
    assert world.anchor.RUNTIME.route.get() is None


def test_errors_inside_the_wrapper_keep_hermes_answer(world, monkeypatch):
    set_models(world, public=DS)
    monkeypatch.setattr(world.runtime.check, "resolve_in_scope", lambda *a: (_ for _ in ()).throw(RuntimeError()))
    assert in_turn(world, source(), route(world.anchor))[0] == "profile-model"


def test_no_secret_value_in_any_log_line(world, caplog):
    caplog.set_level(logging.DEBUG, logger="ctx_discord_policy")
    set_models(world, public=DS)
    in_turn(world, source(), route(world.anchor))
    set_models(world, public=OR)
    in_turn(world, source(), route(world.anchor))
    world.check.validate_choice(world.anchor.home, "deepseek", "deepseek-chat", profile="default")
    world.check.validate_choice(world.anchor.home, "openrouter", "x/y", profile="default")
    assert "FAKE-" not in "\n".join(r.getMessage() for r in caplog.records)


# ── save-time checks ─────────────────────────────────────────────────────────────────────────────────


def test_save_time_check_refuses_small_windows_and_missing_credentials(world):
    world.fake.contexts[("deepseek", "tiny")] = 32_000
    verdict = world.check.validate_choice(world.anchor.home, "deepseek", "tiny", profile="default")
    assert not verdict.ok and "32 000" in verdict.reason and "64 000" in verdict.reason
    assert world.check.validate_choice(world.anchor.home, "deepseek", "deepseek-chat", profile="default").ok
    missing = world.check.validate_choice(world.anchor.home, "openrouter", "x/y", profile="default")
    assert not missing.ok and "auth add openrouter" in missing.next_step


def test_unknown_window_saves_with_a_warning(world, monkeypatch):
    monkeypatch.setattr(world.anchor.mod("hermes"), "context_length", lambda *a: None)
    verdict = world.check.validate_choice(world.anchor.home, "deepseek", "x", profile="default")
    assert verdict.ok and "could not be determined" in verdict.reason


def test_model_fields_are_validated_strictly(world):
    schema = world.anchor.mod("store.schema")
    for value in ({"provider": "Deep Seek", "model": "x"}, {"provider": "deepseek", "model": "a\nb"},
                  {"provider": "deepseek", "model": " x"}, {"provider": "deepseek"}, "deepseek-chat"):
        with pytest.raises(schema.SchemaError):
            set_models(world, public=value)
    with pytest.raises(schema.SchemaError):
        set_models(world, public_choices=[{"provider": "p", "model": f"m{i}"} for i in range(26)])
    with pytest.raises(schema.SchemaError):
        set_models(world, on_unavailable="ignore")
    with pytest.raises(schema.SchemaError, match="not a model setting"):
        world.ops.set_models(world.store, BOT, GUILD, {"models.manager_pick": DS}, by=OP)
