"""Intervention K8 (comfort): the per-server model inside Hermes' own model resolution.

``GatewayRunner._resolve_session_agent_runtime`` picks ``(model, runtime)`` for a session;
``_fallback_baseline_model`` tells the post-turn check which model the cached agent was built for. Both
are wrapped together or not at all: with only the first, Hermes evicts the agent after every turn (the
agent's model differs from what the baseline expects), losing the prompt cache each message.

The wrapper calls the original first; its answer is the fallback. It then applies the server's model
only when all of this holds, in this order:

1. the session belongs to a policy route: the turn's route (A7 context variable, matching this source),
   else the route recorded for this session key earlier — the lane comes from that route, never from
   the server's configuration;
2. the session has no ``/model`` of its own, no frozen launch policy owns it, and no ``channel_overrides``
   entry applies (more specific wins, like routes);
3. the server is active and has a model for the lane;
4. the call runs in the scope of the profile the source routes to (otherwise another profile's
   credentials would be used: leave it to Hermes);
5. that profile has a credential for the provider (by name) and Hermes resolves a runtime for it.

Anything else, or any error, returns the original answer unchanged. The wrapper never touches the route
context variable or the lane.
"""

from __future__ import annotations

import inspect
import logging
import sys
import time
from typing import Any, Optional

from .. import RUNTIME_API, hermes
from ..adapt.probes import probe_callable
from ..adapt.status import COMFORT, Outcome, Result
from ..adapt.strategies import run_intervention, wrap_attr
from ..policy.routes import Route, platform_is_discord
from ..runtime import RUNTIME, BoundedMap
from . import check

logger = logging.getLogger("ctx_discord_policy")

INTERVENTION = "K8"
RESOLVE = "_resolve_session_agent_runtime"
BASELINE = "_fallback_baseline_model"

# session key -> (bot, guild, lane) of the policy route that used it (lanes never mix in one session).
SESSION_ROUTES: BoundedMap = BoundedMap()
# session key -> (provider, model) the wrapper applied in its last resolution of that session.
APPLIED: BoundedMap = BoundedMap()
# (bot, guild, lane) -> (since, reason) for chosen models that could not be applied.
UNAVAILABLE: dict = {}
# listener(bot_id, guild_id, lane, model, reason), called whenever a chosen model is found unavailable (the
# wiring throttles and sends the operators a direct message). A failing listener never affects the turn.
UNAVAILABLE_LISTENERS: list = []


def _link(source: Any, session_key: str) -> Optional[tuple[str, str, str]]:
    route = RUNTIME.route.get()
    if isinstance(route, Route) and route.matches_source(
            source, platform_is_discord=platform_is_discord(getattr(source, "platform", None))):
        link = (str(route.bot_id), str(route.guild_id), str(route.lane))
        SESSION_ROUTES.put(session_key, link)
        return link
    return SESSION_ROUTES.get(session_key)


def _channel_override_applies(runner: Any, source: Any) -> bool:
    lookup = getattr(runner, "_channel_override", None)
    if not callable(lookup):
        return False
    args = (getattr(source, "platform", None), str(getattr(source, "chat_id", "") or ""),
            str(source.thread_id) if getattr(source, "thread_id", None) else None,
            str(source.parent_chat_id) if getattr(source, "parent_chat_id", None) else None)
    try:
        takes_source = "source" in inspect.signature(lookup).parameters
    except (TypeError, ValueError):
        takes_source = False
    entry = lookup(*args, source=source) if takes_source else lookup(*args)
    return bool(entry is not None and (getattr(entry, "model", None) or getattr(entry, "provider", None)))


def _in_profile_scope(runner: Any, source: Any) -> Optional[Any]:
    """The routed home when this call runs in its scope (credentials of that profile), else None."""
    home = runner._resolve_profile_home_for_source(source)
    scoped = hermes.scope_home()
    current = scoped if scoped is not None else hermes.current_home()
    return home if hermes.home_key(current) == hermes.home_key(home) else None


def _status_with_count() -> None:
    """Keep the K8 row's reason current: how many chosen server models can't be applied right now."""
    current = RUNTIME.status.get(INTERVENTION)
    if current is None or current.outcome is not Outcome.WRAPPED:
        return
    count = len(UNAVAILABLE)
    base = current.reason.split(" · ", 1)[0]
    RUNTIME.status.record(Result(INTERVENTION, COMFORT, Outcome.WRAPPED,
                                 f"{base} · {count} server model{'s' if count != 1 else ''} unavailable",
                                 current.probe))


def note_unavailable(key: tuple, reason: str, model: str = "") -> None:
    """A server's chosen model for one lane can't be applied (K8 fell back, or ``on_unavailable: close``)."""
    new = key not in UNAVAILABLE
    if new:
        logger.warning("ctx_discord_policy: K8: server %s %s model unavailable: %s", key[1], key[2], reason)
    UNAVAILABLE[key] = (UNAVAILABLE.get(key, (time.time(), ""))[0], reason)
    if new:
        _status_with_count()
    for listener in list(UNAVAILABLE_LISTENERS):  # every time: a message that could not go out yet is retried
        try:
            listener(*key, model, reason)
        except Exception:
            logger.warning("ctx_discord_policy: K8 unavailable listener failed", exc_info=True)


def note_available(key: tuple) -> None:
    if UNAVAILABLE.pop(key, None) is not None:
        _status_with_count()


def choose(runner: Any, source: Any, session_key: Optional[str]) -> Optional[tuple[str, dict]]:
    """``(model, runtime)`` of the server's model for this session, or None (Hermes' answer stands)."""
    if source is None:
        return None
    skey = runner._resolve_session_key_or_none(source, session_key)
    if not skey:
        return None
    link = _link(source, str(skey))
    if link is None:
        APPLIED.pop(str(skey))
        return None
    bot_id, guild_id, lane = link
    if runner._session_model_override(skey) or hermes.launch_policy_applies(runner, source) \
            or _channel_override_applies(runner, source):
        APPLIED.pop(str(skey))
        return None
    binding = RUNTIME.bots.for_bot(bot_id)
    snapshot = binding.store.read() if binding is not None else None
    if snapshot is None or snapshot.guild_state(bot_id, guild_id) != "active":
        APPLIED.pop(str(skey))
        return None
    choice = check.effective(snapshot.guild(bot_id, guild_id), lane)
    if choice is None:
        APPLIED.pop(str(skey))
        return None
    if _in_profile_scope(runner, source) is None:
        return None
    ok, why, runtime = check.resolve_in_scope(choice["provider"], choice["model"])
    if not ok:
        note_unavailable((bot_id, guild_id, lane), why, choice["model"])
        APPLIED.pop(str(skey))
        return None
    note_available((bot_id, guild_id, lane))
    APPLIED.put(str(skey), (choice["provider"], choice["model"]))
    return choice["model"], runtime


def _make_resolve(original: Any) -> Any:
    def k8_resolve(self: Any, *args: Any, **kwargs: Any) -> Any:
        result = original(self, *args, **kwargs)
        try:
            chosen = choose(self, kwargs.get("source"), kwargs.get("session_key"))
        except Exception:
            logger.exception("ctx_discord_policy: K8 failed; Hermes' model stands for this session")
            chosen = None
        if chosen is None:
            return result
        if getattr(self, "_pre_agent_fallback_notice", None) is not None:
            # Hermes' notice describes ITS runtime choice (a provider fallback), which K8 just replaced.
            self._pre_agent_fallback_notice = None
        return chosen

    return k8_resolve


def baseline_for(runner: Any, session_key: Any, agent: Any, original_value: Any, policy_model: Any) -> Any:
    applied = APPLIED.get(str(session_key or ""))
    if applied is None or policy_model or runner._session_model_override(session_key):
        return original_value
    primary = getattr(agent, "_primary_runtime", None) or {}
    provider = primary.get("provider", getattr(agent, "provider", "")) or applied[0]
    return hermes.normalize_model(applied[1], provider)


def _make_baseline(original: Any) -> Any:
    def k8_baseline(self: Any, session_key: Any, source: Any, agent: Any, *args: Any, **kwargs: Any) -> Any:
        value = original(self, session_key, source, agent, *args, **kwargs)
        try:
            return baseline_for(self, session_key, agent, value, kwargs.get("policy_model"))
        except Exception:
            logger.exception("ctx_discord_policy: K8 baseline failed; Hermes' baseline stands")
            return value

    return k8_baseline


def probe(cls: Any) -> tuple[bool, str]:
    resolve = probe_callable(cls, RESOLVE, is_async=False, keyword_only=["source", "session_key"])
    baseline = probe_callable(cls, BASELINE, is_async=False, positional=["session_key", "source", "agent"])
    missing = [r.reason for r in (resolve, baseline) if not r]
    for name in ("_session_model_override", "_resolve_session_key_or_none", "_resolve_profile_home_for_source"):
        if not callable(getattr(cls, name, None)):
            missing.append(f"{name} missing")
    if not hermes.provider_runtime_present():
        missing.append("gateway.run._resolve_runtime_agent_kwargs_for_provider missing")
    if missing:
        return False, "; ".join(missing)
    return True, f"{resolve.reason}; {baseline.reason}"


def install(cls: Any = None) -> Result:
    """Wrap both methods on ``GatewayRunner`` (default: the loaded ``gateway.run`` class), or neither."""

    def body() -> Result:
        runner_cls = cls if cls is not None else getattr(sys.modules.get("gateway.run"), "GatewayRunner", None)
        if runner_cls is None:
            return Result(INTERVENTION, COMFORT, Outcome.FALLBACK, "gateway.run is not loaded yet; profile models apply",
                          "GatewayRunner")
        ok, reason = probe(runner_cls)
        if not ok:
            return Result(INTERVENTION, COMFORT, Outcome.FALLBACK,
                          f"fallback: profile model (Hermes changed: {reason})", "GatewayRunner probes")
        before = runner_cls.__dict__.get(RESOLVE)
        wrap_attr(runner_cls, RESOLVE, _make_resolve, INTERVENTION, RUNTIME_API)
        try:
            wrap_attr(runner_cls, BASELINE, _make_baseline, INTERVENTION, RUNTIME_API)
        except Exception as exc:
            # Never only one of the two: without the baseline Hermes would evict the agent every turn.
            if before is None:
                delattr(runner_cls, RESOLVE)
            else:
                setattr(runner_cls, RESOLVE, before)
            return Result(INTERVENTION, COMFORT, Outcome.FALLBACK,
                          f"fallback: profile model ({BASELINE} not wrappable: {exc})", "GatewayRunner")
        return Result(INTERVENTION, COMFORT, Outcome.WRAPPED, f"server models through {RESOLVE} and {BASELINE}",
                      reason)

    return run_intervention(RUNTIME.status, INTERVENTION, COMFORT, body)
