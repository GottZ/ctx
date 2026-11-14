"""Split delivery (intervention K3) and the delivery layer's single install seam.

``send`` and ``edit_message`` of the Discord adapter are wrapped (never replaced). For a delivery that is
the turn's own output in the origin channel of an operator route whose tool call was admitted, and whose
guild has a worklog channel, the message goes into the worklog thread instead. Edits follow the message to
wherever it was delivered.

The final answer is copied back to the origin as a reply when ``worklog.final_to_origin`` is on, once, when
the turn ends (K3 also wraps ``on_processing_complete``): Hermes marks segment ends between tool calls and
sealed overflow heads ``finalize=True`` as well, so only the last final-marked content of a successful turn
is the answer. Without that hook the copy is made right away for each final-marked delivery.

Everything else is stock delivery: deliveries to any other chat (explicit sends of tools), public routes,
operator turns without tools, guilds without a worklog, messages written inside a worklog thread, and the
rest of a turn whose worklog delivery failed. A non-operator route is never allowed to deliver into the
worklog area (refused, fail closed).

Fallback when the probes do not match: nothing is wrapped (stock delivery in the origin channel), and the
status register says that operator tool output then stays visible in the origin channel.
"""

from __future__ import annotations

import logging
from typing import Any, Callable, Optional

from .. import RUNTIME_API
from ..adapt.probes import probe_callable
from ..adapt.status import COMFORT, Outcome, Result, StatusRegister
from ..adapt.strategies import is_own_wrapper, run_intervention, wrap_attr
from ..runtime import RUNTIME
from . import media, reactions, worklog

logger = logging.getLogger("ctx_discord_policy")

INTERVENTION = "K3"
FALLBACK_HINT = "stock delivery: operator tool output stays visible in the origin channel"
REFUSED = media.REFUSED

# (method, positional parameters the wrapper reads, keyword parameters it reads)
CORE_TARGETS: tuple[tuple[str, tuple[str, ...], tuple[str, ...]], ...] = (
    ("send", ("chat_id", "content"), ("reply_to", "metadata")),
    ("edit_message", ("chat_id", "message_id", "content"), ("finalize", "metadata")),
)
COMPLETION_TARGET = ("on_processing_complete", ("event", "outcome"), ())


async def _refuse_public(adapter: Any, routes: list, target: str, what: str) -> bool:
    if not any(not r.is_operator for r in routes):
        return False
    try:
        hit = await worklog.public_route_hits_worklog(adapter, routes, target)
    except Exception:
        logger.exception("ctx_discord_policy: worklog check for a non-operator %s failed; refusing", what)
        hit = True
    if hit:
        logger.warning("ctx_discord_policy: refused %s of a non-operator turn into the worklog area (%s)", what,
                       target)
    return hit


def copy_at_turn_end(adapter: Any) -> bool:
    """K3 owns ``on_processing_complete`` of this adapter's class (the final copy waits for the turn end)."""
    return is_own_wrapper(getattr(type(adapter), "on_processing_complete", None), INTERVENTION)


async def _final(adapter: Any, original_send: Any, state: Any, content: Any) -> None:
    """A final-marked delivery reached the worklog: remember it, or copy it now without the turn-end hook."""
    if not state.settings.final_to_origin:
        return
    if copy_at_turn_end(adapter):
        worklog.note_final(state, content)
        return
    origin = await worklog.copy_final_to_origin(original_send, adapter, state, content)
    if origin is not None and not worklog.succeeded(origin):
        logger.warning("ctx_discord_policy: final answer copy to the origin failed: %s",
                       getattr(origin, "error", None))


def make_send(original: Any) -> Any:
    signature = worklog.signature_of(original)

    async def policy_send(self: Any, *args: Any, **kwargs: Any) -> Any:
        call = worklog.CallArgs.bind(signature, (self, *args), kwargs)
        if call is None:
            return await original(self, *args, **kwargs)
        chat_id = str(call.get("chat_id") or "")
        reply_to = call.get("reply_to")
        metadata = call.get("metadata")
        target = worklog.delivery_target(chat_id, metadata)
        try:
            routes = worklog.candidate_routes(target, reply_to, metadata)
        except Exception:
            logger.exception("ctx_discord_policy: route lookup failed; stock delivery")
            routes = []
        if await _refuse_public(self, routes, target, "send"):
            return worklog.send_result(original, success=False, error=REFUSED)
        route = worklog.route_for_target(routes, target)
        try:
            state = (await worklog.worklog_for(self, route)
                     if route is not None and worklog.is_turn_output(route, reply_to, metadata) else None)
        except Exception:
            logger.exception("ctx_discord_policy: worklog lookup failed; stock delivery")
            state = None
        if state is None:
            return await original(self, *args, **kwargs)

        redirected = dict(metadata or {})
        redirected["thread_id"] = state.thread_id
        redirected["parent_chat_id"] = state.settings.channel_id
        if reply_to not in (None, ""):
            redirected.setdefault("reply_to_message_id", str(reply_to))   # ledger anchor of the origin message
        call.set("chat_id", state.thread_id)
        call.set("reply_to", None)
        call.set("metadata", redirected)
        result = await call.invoke(original)
        if not worklog.succeeded(result):
            if getattr(result, "retryable", False):
                return result      # transport trouble: Hermes retries, the retry is routed again
            state.failed = f"delivery into worklog thread {state.thread_id} failed: {getattr(result, 'error', None)}"
            logger.warning("ctx_discord_policy: %s; the rest of the turn is delivered in the origin channel",
                           state.failed)
            return await original(self, *args, **kwargs)
        worklog.remember_delivery((chat_id, target), result, state)
        if isinstance(metadata, dict) and metadata.get("notify"):
            await _final(self, original, state, call.get("content"))
        return result

    return policy_send


def make_edit(original: Any, original_send: Callable[[], Any]) -> Any:
    """``original_send`` returns the send underneath K3 at call time (for the origin copy)."""
    signature = worklog.signature_of(original)

    async def policy_edit(self: Any, *args: Any, **kwargs: Any) -> Any:
        call = worklog.CallArgs.bind(signature, (self, *args), kwargs)
        if call is None:
            return await original(self, *args, **kwargs)
        chat_id = str(call.get("chat_id") or "")
        message_id = call.get("message_id")
        metadata = call.get("metadata")
        found = worklog.located(chat_id, message_id)
        target = found[0] if found is not None else chat_id
        try:
            routes = worklog.candidate_routes(target, None, metadata)
        except Exception:
            logger.exception("ctx_discord_policy: route lookup failed; stock edit")
            routes = []
        if await _refuse_public(self, routes, target, "edit"):
            return worklog.send_result(original, success=False, error=REFUSED)
        if found is None:
            return await original(self, *args, **kwargs)

        thread_id, state = found
        call.set("chat_id", thread_id)
        result = await call.invoke(original)
        if state is not None and worklog.succeeded(result):
            # A final edit can split into continuation messages; later edits address them by the caller's chat.
            worklog.remember_delivery((chat_id,), result, state)
            if call.get("finalize"):
                await _final(self, original_send(), state, call.get("content"))
        return result

    return policy_edit


def make_complete(original_send: Callable[[], Any]) -> Callable[[Any], Any]:
    """K3 on ``on_processing_complete``: copy the turn's final answer to the origin once, then run stock."""

    def factory(original: Any) -> Any:
        signature = worklog.signature_of(original)

        async def policy_complete(self: Any, *args: Any, **kwargs: Any) -> Any:
            try:
                call = worklog.CallArgs.bind(signature, (self, *args), kwargs)
                event = call.get("event") if call is not None else None
                outcome = call.get("outcome") if call is not None else None
                state = worklog.state_for_event(self, reactions.event_keys(event)) if event is not None else None
                if state is not None and state.final_text:
                    text, state.final_text = state.final_text, ""
                    if str(getattr(outcome, "value", outcome)).lower() == "success":
                        copied = await worklog.copy_final_to_origin(original_send(), self, state, text)
                        if copied is not None and not worklog.succeeded(copied):
                            logger.warning("ctx_discord_policy: final answer copy to the origin failed: %s",
                                           getattr(copied, "error", None))
            except Exception:
                logger.exception("ctx_discord_policy: final answer copy failed")
            return await original(self, *args, **kwargs)

        return policy_complete

    return factory


def _restore(owner: Any, name: str, had_own: bool, previous: Any) -> None:
    if had_own:
        setattr(owner, name, previous)
    else:
        try:
            delattr(owner, name)
        except AttributeError:
            pass


def _send_beneath(adapter_cls: Any) -> Any:
    """The adapter's ``send`` underneath this intervention (the origin copy must not be routed again)."""
    return worklog.beneath(getattr(adapter_cls, "send"), INTERVENTION)


def install_k3(adapter_cls: Any) -> Result:
    targets = CORE_TARGETS + media.MEDIA_TARGETS + (COMPLETION_TARGET,)
    probes = {name: probe_callable(adapter_cls, name, is_async=True, positional=list(pos), keyword_only=list(kw))
              for name, pos, kw in targets}
    core_failed = [probes[name] for name, _, _ in CORE_TARGETS if not probes[name]]
    if core_failed:
        reason = "; ".join(p.reason for p in core_failed)
        return Result(INTERVENTION, COMFORT, Outcome.FALLBACK, f"{reason}; {FALLBACK_HINT}",
                      core_failed[0].evidence)

    send_beneath = lambda: _send_beneath(adapter_cls)  # noqa: E731 - resolved at call time
    saved = {name: (name in vars(adapter_cls), vars(adapter_cls).get(name)) for name, _, _ in CORE_TARGETS}
    states: list[str] = []
    try:
        states.append(wrap_attr(adapter_cls, "send", make_send, INTERVENTION, RUNTIME_API))
        states.append(wrap_attr(adapter_cls, "edit_message", lambda original: make_edit(original, send_beneath),
                                INTERVENTION, RUNTIME_API))
    except Exception:
        for name, (had_own, previous) in saved.items():
            _restore(adapter_cls, name, had_own, previous)
        raise

    notes = ["send, edit_message"]
    for name, _, _ in media.MEDIA_TARGETS:
        if not probes[name]:
            notes.append(f"{name} stock ({probes[name].reason})")
            continue
        try:
            states.append(wrap_attr(adapter_cls, name, media.make_media(name), INTERVENTION, RUNTIME_API))
        except Exception as exc:  # one media sender that cannot be wrapped keeps stock delivery
            notes.append(f"{name} stock ({type(exc).__name__}: {exc})")
            continue
        notes.append(name)
    completion = probes[COMPLETION_TARGET[0]]
    if completion:
        try:
            states.append(wrap_attr(adapter_cls, COMPLETION_TARGET[0], make_complete(send_beneath), INTERVENTION,
                                    RUNTIME_API))
            notes.append("final copy at turn end")
        except Exception as exc:
            notes.append(f"final copy per final-marked delivery ({type(exc).__name__}: {exc})")
    else:
        notes.append(f"final copy per final-marked delivery ({completion.reason})")
    reference = probe_callable(adapter_cls, "_message_reference_from_ids", positional=["message_id", "channel"])
    if not reference:
        notes.append(f"media without origin reply reference ({reference.reason})")
    note = "already wrapped" if all(s == "already" for s in states) else "wrapped"
    return Result(INTERVENTION, COMFORT, Outcome.WRAPPED, f"{note}: " + ", ".join(notes), probes["send"].evidence)


def install(adapter_cls: Any, get_guild_config: Callable[[str, str], Any],
            register: Optional[StatusRegister] = None) -> list[Result]:
    """Install K3 (worklog/split/media delivery) and K4 (ambient without processing reactions).

    ``adapter_cls``: the Discord adapter class the gateway instantiates (resolved by A3).
    ``get_guild_config(bot_id, guild_id)``: the guild's configuration dict (DESIGN §5.3) or ``None``.
    ``register``: the status register (default: the runtime anchor's).
    Idempotent (reconnect, second profile instance); never raises for an adapter that does not match.
    """
    worklog.DELIVERY.configure(get_guild_config)
    register = register if register is not None else RUNTIME.status
    k3 = run_intervention(register, INTERVENTION, COMFORT, lambda: install_k3(adapter_cls))
    k4 = run_intervention(register, reactions.INTERVENTION, COMFORT, lambda: reactions.install(adapter_cls))
    return [k3, k4]


__all__ = ["CORE_TARGETS", "FALLBACK_HINT", "INTERVENTION", "REFUSED", "copy_at_turn_end", "install", "install_k3",
           "make_complete", "make_edit", "make_send"]
