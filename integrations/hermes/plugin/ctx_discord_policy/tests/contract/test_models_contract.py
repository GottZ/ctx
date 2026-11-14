"""S13 contract: the model per server (K8) against real Hermes (PROFILE-PER-GUILD §B8).

Real ``GatewayRunner._resolve_session_agent_runtime`` / ``_fallback_baseline_model`` /
``_run_agent_evict_on_fallback`` on a bare runner (as the evict-baseline probe), real profile runtime scope
with fake keys in each profile's ``.env``, real provider runtime resolution, real context-window lookup.
Sockets are blocked: nothing leaves the machine. Writes ``models-<ref>.json``.
"""

from __future__ import annotations

import os
import socket
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, load_in

GUILD = "900000000000000100"
CHANNEL = "900000000000000200"
BOT = "900000000000000300"
OPERATOR = "900000000000000400"
MEMBER = "900000000000000500"
DS = {"provider": "deepseek", "model": "deepseek-reasoner"}


def mod(name):
    return sys.modules[f"{ANCHOR}.{name}"]


@pytest.fixture
def world(hermes, tmp_path, monkeypatch):
    def blocked(*_a, **_k):
        raise OSError("network blocked by the contract test")

    monkeypatch.setattr(socket.socket, "connect", blocked)
    monkeypatch.setattr(socket, "create_connection", blocked)
    _evict(ANCHOR)
    os.environ["HERMES_HOME"] = str(tmp_path / "root")
    homes = Homes(tmp_path)
    root = homes.root
    (root / "config.yaml").write_text(
        "multiplex_profiles: true\nmodel:\n  default: some/model\n  provider: openrouter\n"
        f"plugins:\n  enabled: [{PLUGIN}]\n  entries:\n    {PLUGIN}:\n      settings:\n"
        f"        operators: ['{OPERATOR}']\n", encoding="utf-8")
    (root / ".env").write_text("OPENROUTER_API_KEY=FAKE-LAUNCH-OR\n", encoding="utf-8")
    routed = homes.profile("routed")
    (routed / "config.yaml").write_text(
        f"plugins:\n  enabled: [{PLUGIN}]\nmodel:\n  default: deepseek-chat\n  provider: deepseek\n"
        "custom_providers:\n  - name: tiny\n    base_url: http://127.0.0.1:9/v1\n    context_length: 32000\n"
        "    models: [tiny-model]\n", encoding="utf-8")
    (routed / ".env").write_text("DEEPSEEK_API_KEY=FAKE-ROUTED-DS\n", encoding="utf-8")
    monkeypatch.setenv("OPENROUTER_API_KEY", "FAKE-LAUNCH-OR")
    import hermes_state

    monkeypatch.setattr(hermes_state, "SessionDB", lambda *a, **k: (_ for _ in ()).throw(RuntimeError("no db")))
    from gateway.run import GatewayRunner

    saved = {name: GatewayRunner.__dict__.get(name) for name in
             ("_resolve_session_agent_runtime", "_fallback_baseline_model", "_handle_message", "_is_user_authorized")}
    load_in(root)
    load_in(routed)
    from agent.secret_scope import set_multiplex_active
    set_multiplex_active(True)

    class Adapter:  # the bot's adapter only identifies the binding here (weak-referenceable)
        gateway_runner = None

    adapter = Adapter()
    runtime = mod("runtime").RUNTIME
    binding = runtime.bots.bind(adapter, BOT, root)
    mod("store.ops").bind(binding.store, BOT, GUILD, by="cli")
    yield SimpleNamespace(root=root, routed=routed, runtime=runtime, binding=binding, GatewayRunner=GatewayRunner)
    for name, value in saved.items():
        if value is None:
            if name in GatewayRunner.__dict__:
                delattr(GatewayRunner, name)
        else:
            setattr(GatewayRunner, name, value)
    try:
        set_multiplex_active(False)
    except Exception:
        pass
    _evict(ANCHOR)


def runner(world, **extra):
    r = object.__new__(world.GatewayRunner)
    r._session_model_overrides = {}
    r.session_store = None
    r.evicted = []
    r._evict_cached_agent = lambda key, **kw: r.evicted.append(key)
    for name, value in extra.items():
        setattr(r, name, value)
    return r


def source(user=MEMBER, chat=CHANNEL, profile="routed"):
    from gateway.config import Platform
    from gateway.session import SessionSource

    return SessionSource(platform=Platform.DISCORD, chat_id=chat, chat_type="group", user_id=user, guild_id=GUILD,
                         scope_id=GUILD, profile=profile)


def key_of(src):
    return f"agent:routed:discord:group:{src.chat_id}:{src.user_id}"


def route(lane="public", user=MEMBER):
    return mod("policy.routes").Route(bot_id=BOT, guild_id=GUILD, channel_id=CHANNEL, message_id="1", author_id=user,
                                      lane=lane, target_home_key="")


def set_models(world, **changes):
    mod("store.ops").set_models(world.binding.store, BOT, GUILD, {f"models.{k}": v for k, v in changes.items()},
                                by=OPERATOR)


def resolve(world, r, src, lane_route):
    from gateway.run import _profile_runtime_scope

    token = world.runtime.route.set(lane_route)
    try:
        with _profile_runtime_scope(world.routed):
            return r._resolve_session_agent_runtime(source=src, session_key=key_of(src))
    finally:
        world.runtime.route.reset(token)


def agent_for(model, runtime):
    """The agent Hermes builds for (model, runtime): AIAgent canonicalises the id for a native provider."""
    from hermes_cli.model_normalize import _AGGREGATOR_PROVIDERS, normalize_model_for_provider

    provider = runtime.get("provider") or ""
    if provider and provider not in _AGGREGATOR_PROVIDERS:
        model = normalize_model_for_provider(model, provider)
    return SimpleNamespace(model=model, provider=runtime.get("provider"), base_url=runtime.get("base_url"),
                           _primary_runtime={"provider": runtime.get("provider"), "base_url": runtime.get("base_url")},
                           _provider_fallback_active=False)


def turn_ctx(src, agent):
    return SimpleNamespace(session_key=key_of(src), agent_holder=[agent], result_holder=[{"failed": False}], source=src)


def test_guild_model_is_chosen_and_the_agent_is_kept(world, hermes):
    """Abnahme 1 / M-M1: K8 wraps both; agent.model = guild model = baseline; no eviction after the turn."""
    result = mod("models.runtime").install(world.GatewayRunner)
    assert result.outcome.value == "wrapped", result
    strategies = mod("adapt.strategies")
    for name in ("_resolve_session_agent_runtime", "_fallback_baseline_model"):
        assert strategies.is_own_wrapper(getattr(world.GatewayRunner, name), "K8"), name
    set_models(world, public=DS)
    r = runner(world)
    src = source()
    model, rt = resolve(world, r, src, route())
    assert model == "deepseek-reasoner" and rt.get("provider") == "deepseek" and bool(rt.get("api_key"))
    agent = agent_for(model, rt)
    assert r._fallback_baseline_model(key_of(src), src, agent) == agent.model   # canonical form of the guild model
    r._run_agent_evict_on_fallback(turn_ctx(src, agent))
    assert r.evicted == []
    hermes.write_report("models", {"k8": result.as_dict(), "model": model, "provider": rt.get("provider")})


def test_without_the_baseline_wrapper_hermes_evicts_every_turn(world):
    """Why both are needed (probe modell_evict_baseline): the guild model alone is evicted after the turn."""
    model_runtime = mod("models.runtime")
    original = world.GatewayRunner._fallback_baseline_model
    model_runtime.install(world.GatewayRunner)
    set_models(world, public=DS)
    r = runner(world)
    src = source()
    model, rt = resolve(world, r, src, route())
    r._fallback_baseline_model = original.__get__(r, world.GatewayRunner)
    r._run_agent_evict_on_fallback(turn_ctx(src, agent_for(model, rt)))
    assert r.evicted == [key_of(src)]


def test_sessions_without_a_policy_route_keep_hermes_model(world):
    mod("models.runtime").install(world.GatewayRunner)
    set_models(world, public=DS)
    model, _ = resolve(world, runner(world), source(user="900000000000000999"), None)
    assert model == "deepseek-chat"


def test_session_model_and_channel_override_win(world):
    """M-M3: a session's own /model (and channel_overrides) come first."""
    mod("models.runtime").install(world.GatewayRunner)
    set_models(world, public=DS)
    src = source()
    r = runner(world)
    r._session_model_overrides[key_of(src)] = {"model": "deepseek-chat", "provider": "deepseek",
                                               "api_key": "FAKE-ROUTED-DS", "base_url": "https://api.deepseek.com/v1"}
    model, _ = resolve(world, r, src, route())
    assert model == "deepseek-chat"
    from gateway.config import load_gateway_config

    # Older refs read channel_overrides from the launch config, main from the serving profile's own
    # config (#136198): the override is in both, so it applies on every ref.
    for home in (world.root, world.routed):
        (home / "config.yaml").write_text(
            (home / "config.yaml").read_text() + "discord:\n  channel_overrides:\n"
            f"    '{CHANNEL}':\n      model: deepseek-chat\n", encoding="utf-8")
    r2 = runner(world, config=load_gateway_config())
    assert r2._resolve_model_for_channel(src.platform, CHANNEL) == "deepseek-chat" or \
        r2._channel_override(src.platform, CHANNEL, None, None, source=src) is not None
    model, _ = resolve(world, r2, src, route())
    assert model == "deepseek-chat"


def test_lane_separation(world):
    """M-M4: the operator's session gets the operator model, a member's session the members' model."""
    mod("models.runtime").install(world.GatewayRunner)
    set_models(world, public=DS, operator={"provider": "deepseek", "model": "deepseek-chat-op"})
    r = runner(world)
    member = source()
    operator = source(user=OPERATOR)
    assert resolve(world, r, member, route())[0] == "deepseek-reasoner"
    assert resolve(world, r, operator, route("operator", OPERATOR))[0] == "deepseek-chat-op"
    # Background calls without the turn's context use the lane recorded for the session.
    from gateway.run import _profile_runtime_scope

    with _profile_runtime_scope(world.routed):
        assert r._resolve_session_agent_runtime(source=member, session_key=key_of(member))[0] == "deepseek-reasoner"


def test_provider_without_credential_in_the_profile_falls_back(world):
    """Abnahme 2 / M-M2: openrouter is keyed only in the launch home; Hermes resolves it without a key
    in the routed profile instead of failing, so 'no error' is not availability."""
    from gateway.run import _profile_runtime_scope, _resolve_runtime_agent_kwargs_for_provider

    with _profile_runtime_scope(world.routed):
        assert not _resolve_runtime_agent_kwargs_for_provider("openrouter", target_model="x/y").get("api_key")
    mod("models.runtime").install(world.GatewayRunner)
    set_models(world, public={"provider": "openrouter", "model": "x/y"})
    model, rt = resolve(world, runner(world), source(), route())
    assert model == "deepseek-chat" and rt.get("provider") == "deepseek"
    check = mod("models.check")
    ok, why = check.availability(world.routed, "openrouter", "x/y", cached=False)
    assert not ok and "no credential" in why
    assert check.availability(world.routed, "deepseek", "deepseek-chat", cached=False)[0]


def test_context_window_below_the_minimum_is_refused(world):
    """Abnahme 7: a 32 000-token endpoint (custom_providers context_length) cannot be saved."""
    check = mod("models.check")
    verdict = check.validate_choice(world.routed, "custom:tiny", "tiny-model", profile="routed")
    assert not verdict.ok and "32 000" in verdict.reason and "64 000" in verdict.reason, verdict
    assert check.validate_choice(world.routed, "openrouter", "x/y", profile="routed").ok is False


def test_missing_method_wraps_neither(world):
    """Abnahme 8: without one of the two methods nothing is wrapped; K8 falls back."""
    model_runtime = mod("models.runtime")

    class Changed(world.GatewayRunner):
        _fallback_baseline_model = None

    result = model_runtime.install(Changed)
    assert result.outcome.value == "fallback" and "_fallback_baseline_model" in result.reason
    assert "_resolve_session_agent_runtime" not in Changed.__dict__


def test_secret_values_never_reach_the_log(world, caplog):
    """No credential value in any log line of the plugin (checks, fallbacks, K8)."""
    import logging

    caplog.set_level(logging.DEBUG, logger="ctx_discord_policy")
    mod("models.runtime").install(world.GatewayRunner)
    set_models(world, public={"provider": "openrouter", "model": "x/y"})
    resolve(world, runner(world), source(), route())
    mod("models.check").validate_choice(world.routed, "deepseek", "deepseek-chat", profile="routed")
    text = "\n".join(r.getMessage() for r in caplog.records)
    assert "FAKE-" not in text
