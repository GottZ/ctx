"""Security interventions A3–A7: bind the Discord adapter class and the gateway runner.

Order of events in a gateway process:
1. ``register()`` (every profile instance): hooks (A8), CLI, and the platform-handler factory below. If
   ``gateway.run`` is already imported, the runner wrappers (A7) are installed right away; ``register()``
   never imports it (plugin discovery can run inside that module's import).
2. The Discord adapter connects; after ``ready`` Hermes calls the factory in the scope of the profile that
   runs the bot: A3 binds ``type(adapter)`` (A5 admission, A6 handler), A7 is installed if still missing,
   the bot is bound to its owner home, and the status file is written there.
Until step 2 has completed for an adapter, every message of that adapter keeps stock behaviour.

Every wrapper wraps the *current* attribute (MRO included), passes ``*args, **kwargs`` through, and is
marked so a reconnect or a second profile instance never wraps twice.
"""

from __future__ import annotations

import logging
import sys
import time
from typing import Any

from .. import RUNTIME_API, hermes
from ..ingress import admission, classify
from ..policy import authority, hooks
from ..policy.routes import snowflake
from ..runtime import RUNTIME
from .probes import probe_callable
from .status import SECURITY, Outcome, Result
from .strategies import wrap_attr

logger = logging.getLogger("ctx_discord_policy")


# ── A5 / A6: Discord adapter class ───────────────────────────────────────────────────────────────


def _reply_type(adapter: Any) -> Any:
    module = sys.modules.get(type(adapter).__module__)
    discord_module = getattr(module, "discord", None)
    if discord_module is None:
        try:
            import discord as discord_module  # noqa: PLC0415 - in-process Hermes ships discord.py
        except ImportError:
            return "reply"
    return discord_module.MessageType.reply


def _make_admission(original: Any) -> Any:
    def policy_admission(self: Any, message: Any, *args: Any, **kwargs: Any) -> Any:
        decision = admission.decide(self, message)
        if decision is None:
            return original(self, message, *args, **kwargs)
        if decision.excluded:
            return (False, False)  # "never respond in": nobody, not even through stock
        if not decision.addressed:
            return original(self, message, *args, **kwargs)  # stock alone decides; A6 still routes
        allowed = getattr(self, "_allowed_user_ids", None)
        if not isinstance(allowed, set):
            RUNTIME.status.record(Result("A5", SECURITY, Outcome.OFF,
                                         "adapter._allowed_user_ids is no longer a set", "runtime check"))
            return original(self, message, *args, **kwargs)
        widened = decision.author_id not in allowed
        # Synchronous call: the widened allowlist is visible to nothing else while it is in place.
        self._allowed_user_ids = set(allowed) | {decision.author_id}
        try:
            result = original(self, admission.admission_view(message), *args, **kwargs)
        finally:
            self._allowed_user_ids = allowed
        if not (isinstance(result, tuple) and len(result) == 2 and all(isinstance(v, bool) for v in result)):
            RUNTIME.status.record(Result("A5", SECURITY, Outcome.OFF,
                                         f"admission returned {type(result).__name__}, not (bool, bool)",
                                         "runtime type check"))
            return (False, False)
        if widened:
            # Stock reports role authorization for every admitted human once roles are configured; for an
            # author we let in, it must never travel on (it would authorize an unrouted turn by roles).
            RUNTIME.widened.put(str(getattr(message, "id", "") or ""), time.time())
            return (result[0], False)
        return result

    return policy_admission


def _make_handle(original: Any) -> Any:
    async def policy_handle(self: Any, message: Any, *args: Any, **kwargs: Any) -> Any:
        message_id = str(getattr(message, "id", "") or "")
        decision = admission.decide(self, message)
        if decision is not None and decision.excluded:
            return False
        if decision is not None and decision.reply_unverified \
                and not await classify.verify_reply_to_self(self, message):
            # Not a reply to the bot after all: unaddressed. Routed all the same (stock decides whether
            # the bot reacts, the lane still applies), unless only the widened allowlist let it in.
            decision.addressed = False
            if message_id in RUNTIME.widened:
                decision = None
        if decision is None or not message_id:
            if message_id and message_id in RUNTIME.widened:
                # Admitted only because the policy widened the allowlist: never hand it on unrouted.
                RUNTIME.widened.pop(message_id)
                return False
            return await original(self, message, *args, **kwargs)
        RUNTIME.widened.pop(message_id)
        route = decision.route(message_id)
        RUNTIME.message_routes.put(route.key, route)
        self_user = getattr(getattr(self, "_client", None), "user", None)
        routed = admission.routed_message(message, route, self_user=self_user, message_type=_reply_type(self),
                                          addressed=decision.addressed,
                                          isolate_thread=decision.in_thread and not route.is_operator)
        return await original(self, routed, *args, **kwargs)

    return policy_handle


def bind_adapter_class(cls: Any) -> bool:
    """A3 + A5 + A6 for one adapter class. Returns True when both wrappers are in place."""
    a5 = probe_callable(cls, "_discord_message_admission", is_async=False, positional=["message"],
                        keyword_only=["claim"])
    a6 = probe_callable(cls, "_handle_message", is_async=True, positional=["message", "role_authorized"],
                        keyword_only=["recovered"])
    where = f"{cls.__module__}.{cls.__qualname__}"
    RUNTIME.status.record(Result("A3", SECURITY, Outcome.OK, f"adapter class {where}", "platform handler factory"))
    if not a5:
        RUNTIME.status.record(Result("A5", SECURITY, Outcome.OFF, a5.reason, a5.evidence))
    if not a6:
        RUNTIME.status.record(Result("A6", SECURITY, Outcome.OFF, a6.reason, a6.evidence))
    if not (a5 and a6):
        return False
    wrap_attr(cls, "_discord_message_admission", _make_admission, "A5", RUNTIME_API)
    wrap_attr(cls, "_handle_message", _make_handle, "A6", RUNTIME_API)
    RUNTIME.status.record(Result("A5", SECURITY, Outcome.WRAPPED, a5.reason, a5.evidence))
    RUNTIME.status.record(Result("A6", SECURITY, Outcome.WRAPPED, a6.reason, a6.evidence))
    return True


# ── A7 / A4: gateway runner ──────────────────────────────────────────────────────────────────────


def _make_runner_handle(original: Any) -> Any:
    async def policy_runner_handle(self: Any, event: Any, *args: Any, **kwargs: Any) -> Any:
        candidate = authority.candidate_for_event(event)
        route = None
        if candidate is not None:
            route = authority.route_for_event(event)
            target = authority.event_target_key(self, event) if route is not None else None
            refusal = authority.refusal(self, event, route, target)
            if refusal is not None:
                # A message the policy admitted must never run without its lane: revoke the route (no later
                # authorization check can find it) and drop the event instead of handing it on unrouted.
                authority.revoke(candidate)
                logger.warning("ctx_discord_policy: dropped message %s in channel %s: %s",
                               candidate.message_id, candidate.channel_id, refusal)
                return None
        # Always set (also to None): a drain task created inside another message's context must never
        # run this event under that message's route.
        token = RUNTIME.route.set(route)
        try:
            return await original(self, event, *args, **kwargs)
        finally:
            RUNTIME.route.reset(token)

    return policy_runner_handle


def _make_runner_authorized(original: Any) -> Any:
    def policy_runner_authorized(self: Any, source: Any, *args: Any, **kwargs: Any) -> Any:
        if authority.authorizes(source):
            return True
        return original(self, source, *args, **kwargs)

    return policy_runner_authorized


def install_runner() -> bool:
    """A7 (and the A4 resolver probe) on ``gateway.run.GatewayRunner``, only if that module is loaded."""
    module = sys.modules.get("gateway.run")
    runner = getattr(module, "GatewayRunner", None)
    if runner is None:
        return False
    handle = probe_callable(runner, "_handle_message", is_async=True, positional=["event"])
    authorized = probe_callable(runner, "_is_user_authorized", is_async=False, positional=["source"],
                                keyword_only=["allow_adapter_delegation"])
    resolver = probe_callable(runner, "_resolve_profile_home_for_source", is_async=False, positional=["source"])
    RUNTIME.status.record(Result("A4", SECURITY, Outcome.OK if resolver else Outcome.OFF, resolver.reason,
                                 resolver.evidence))
    if not (handle and authorized and resolver):
        reason = "; ".join(r.reason for r in (handle, authorized, resolver) if not r)
        RUNTIME.status.record(Result("A7", SECURITY, Outcome.OFF, reason, "GatewayRunner probes"))
        return False
    wrap_attr(runner, "_handle_message", _make_runner_handle, "A7", RUNTIME_API)
    wrap_attr(runner, "_is_user_authorized", _make_runner_authorized, "A7", RUNTIME_API)
    RUNTIME.status.record(Result("A7", SECURITY, Outcome.WRAPPED,
                                 f"{handle.reason}; {authorized.reason}", "GatewayRunner"))
    return True


# ── the official entry: platform-handler factory ─────────────────────────────────────────────────


def bot_id_of(native: Any, adapter: Any) -> str:
    for holder in (native, getattr(adapter, "_client", None)):
        if holder is None:
            continue
        for value in (getattr(holder, "application_id", None), getattr(getattr(holder, "user", None), "id", None)):
            ident = snowflake(value)
            if ident:
                return ident
    return ""


def discord_platform_handler(native: Any, adapter: Any) -> None:
    """Called by Hermes for a connected Discord adapter (after ``ready``), in its owner's profile scope."""
    from .. import bootstrap  # late: bootstrap imports this module

    bind_adapter_class(type(adapter))
    install_runner()  # idempotent per class; also covers a runner class that differs from register() time
    bot_id = bot_id_of(native, adapter)
    if not bot_id:
        RUNTIME.status.record(Result("A3", SECURITY, Outcome.OFF, "connected adapter has no application id",
                                     "platform handler factory"))
        return
    binding = RUNTIME.bots.bind(adapter, bot_id, hermes.current_home())
    logger.info("ctx_discord_policy: bot %s bound; state in %s", bot_id, binding.store.path)
    bootstrap.safe_write_status(binding.owner_home)
