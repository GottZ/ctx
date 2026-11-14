"""K1 (comfort): message deletion for the Discord adapter.

The core's base adapter defines ``delete_message`` as a no-op that returns False, and the core treats an
adapter as able to delete exactly when ``type(adapter).delete_message`` is something else (progress-bubble
cleanup, ephemeral system notices; streamed preview cleanup calls it unconditionally). Discord's bundled
adapter inherits the no-op on released Hermes, so ``display.cleanup_progress`` has no effect on Discord.

Adaptation (DESIGN §6.1): if the adapter class already resolves ``delete_message`` to anything other than
the base no-op (its own method, a mixin's, a newer Hermes), nothing is done (``native``). Otherwise, and
only when the preconditions hold (base signature ``(chat_id, message_id)``, an async
``_resolve_channel(channel_id)`` and the adapter's ``_client``), the method below is set on the adapter
class (``injected``). The class's own ``__dict__`` is never overwritten. Anything else turns K1 ``off``:
the adapter keeps the core's no-op.
"""

from __future__ import annotations

import inspect
import logging
from typing import Any, Optional

from .. import RUNTIME_API
from ..adapt.probes import ProbeResult, probe_callable
from ..adapt.status import COMFORT, Outcome, Result, StatusRegister
from ..adapt.strategies import WRAPPER_MARK, is_own_wrapper, run_intervention

logger = logging.getLogger("ctx_discord_policy")

INTERVENTION = "K1"
BASE_CLASS_NAME = "BasePlatformAdapter"
METHOD = "delete_message"


async def delete_message(self: Any, chat_id: str, message_id: str) -> bool:
    """Delete one message in ``chat_id`` (progress bubbles, ephemeral notices, stale previews).

    Returns True when Discord confirmed the deletion. Every failure (not connected, unknown channel,
    missing permission, message already gone) returns False and the caller leaves the message in place.
    """
    if not getattr(self, "_client", None):
        return False
    try:
        channel = await self._resolve_channel(chat_id)
        await channel.get_partial_message(int(message_id)).delete()
        return True
    except Exception as exc:  # best effort by contract: callers treat False as "left in place"
        logger.debug("[%s] Could not delete Discord message %s/%s: %s", getattr(self, "name", "discord"),
                     chat_id, message_id, exc)
        return False


setattr(delete_message, WRAPPER_MARK, (INTERVENTION, RUNTIME_API))


def base_adapter_class(adapter_cls: type) -> Optional[type]:
    """The core's base adapter in the adapter's MRO (found by name: no Hermes import needed)."""
    for klass in inspect.getmro(adapter_cls)[1:]:
        if klass.__name__ == BASE_CLASS_NAME:
            return klass
    return None


def _where(fn: Any) -> str:
    return f"{getattr(fn, '__module__', '?')}.{getattr(fn, '__qualname__', '?')}"


def _base_signature(base: type) -> ProbeResult:
    """The base method the core calls is exactly ``async delete_message(self, chat_id, message_id)``.

    Callers use both ``(chat_id, message_id)`` positionally and as keywords. A changed parameter list (even an
    optional extra parameter callers may start passing) means the contract moved: do not inject.
    """
    shape = probe_callable(base, METHOD, is_async=True, positional=["chat_id", "message_id"])
    if not shape:
        return shape
    names = shape.details.get("params", [])
    if names != ["chat_id", "message_id"]:
        return ProbeResult(False, f"base {METHOD} parameters are {names}, expected ['chat_id', 'message_id']",
                           shape.evidence)
    return shape


def _resolve_channel(adapter_cls: type) -> ProbeResult:
    fn = getattr(adapter_cls, "_resolve_channel", None)
    if fn is None or not callable(fn):
        return ProbeResult(False, "_resolve_channel missing")
    target = inspect.unwrap(fn)
    if not inspect.iscoroutinefunction(target):
        return ProbeResult(False, "_resolve_channel is not async")
    try:
        inspect.signature(target).bind(None, "0")
    except (TypeError, ValueError) as exc:
        return ProbeResult(False, f"_resolve_channel cannot be called with one channel id: {exc}")
    return ProbeResult(True, f"_resolve_channel{inspect.signature(target)}", _where(target))


def _client_evidence(adapter_cls: type, adapter: Any) -> ProbeResult:
    """The adapter keeps its discord.py client in ``_client`` (instance attribute set by ``__init__``)."""
    if adapter is not None:
        if hasattr(adapter, "_client"):
            return ProbeResult(True, "bound adapter instance has _client")
        return ProbeResult(False, "bound adapter instance has no _client")
    for klass in inspect.getmro(adapter_cls):
        code = getattr(klass.__dict__.get("__init__"), "__code__", None)
        if code is not None and "_client" in code.co_names:
            return ProbeResult(True, f"{klass.__qualname__}.__init__ sets _client")
    return ProbeResult(False, "no __init__ in the adapter's MRO sets _client")


def _install(adapter_cls: type, adapter: Any, base_cls: Optional[type]) -> Result:
    base = base_cls or base_adapter_class(adapter_cls)
    if base is None:
        return Result(INTERVENTION, COMFORT, Outcome.OFF,
                      f"no {BASE_CLASS_NAME} in the MRO of {adapter_cls.__qualname__}", "MRO")
    base_fn = base.__dict__.get(METHOD)
    if base_fn is None:
        return Result(INTERVENTION, COMFORT, Outcome.OFF,
                      f"{base.__qualname__} has no {METHOD}: the core no longer deletes through it", "MRO")
    current = getattr(adapter_cls, METHOD, None)
    if is_own_wrapper(current, INTERVENTION):
        return Result(INTERVENTION, COMFORT, Outcome.INJECTED, f"already injected into {adapter_cls.__qualname__}",
                      _where(current))
    if current is not base_fn:
        return Result(INTERVENTION, COMFORT, Outcome.NATIVE,
                      f"{_where(current)} overrides the base no-op; nothing injected", "identity vs base no-op")
    if adapter_cls is base or METHOD in adapter_cls.__dict__:
        return Result(INTERVENTION, COMFORT, Outcome.OFF,
                      f"{adapter_cls.__qualname__} holds the base no-op itself; not overwritten", "class __dict__")
    for check in (_base_signature(base), _resolve_channel(adapter_cls), _client_evidence(adapter_cls, adapter)):
        if not check:
            return Result(INTERVENTION, COMFORT, Outcome.OFF, check.reason, check.evidence)
    setattr(adapter_cls, METHOD, delete_message)
    return Result(INTERVENTION, COMFORT, Outcome.INJECTED,
                  f"{adapter_cls.__qualname__}.{METHOD} added (base is a no-op)", "identity vs base no-op")


def install(adapter_cls: type, register: StatusRegister, *, adapter: Any = None,
            base_cls: Optional[type] = None) -> Result:
    """K1 for one Discord adapter class; records and returns the outcome. Never raises.

    ``adapter``: the bound instance, if available (strengthens the ``_client`` precondition).
    ``base_cls``: the core's base adapter class, if the caller has it (default: looked up in the MRO).
    """
    return run_intervention(register, INTERVENTION, COMFORT, lambda: _install(adapter_cls, adapter, base_cls))


__all__ = ["INTERVENTION", "base_adapter_class", "delete_message", "install"]
