"""Strategy helpers: wrap (never replace) Hermes attachment points, idempotently and visibly."""

from __future__ import annotations

import functools
import logging
from typing import Any, Callable

from .status import COMFORT, SECURITY, Outcome, Result, StatusRegister

logger = logging.getLogger("ctx_discord_policy")

WRAPPER_MARK = "__dsp_wrapper__"


def wrapper_mark(fn: Any) -> Any:
    return getattr(fn, WRAPPER_MARK, None)


def is_own_wrapper(fn: Any, intervention_id: str) -> bool:
    mark = wrapper_mark(fn)
    return isinstance(mark, tuple) and len(mark) == 2 and mark[0] == intervention_id


def wrap_attr(owner: Any, name: str, make_wrapper: Callable[[Any], Any], intervention_id: str,
              runtime_api: int) -> str:
    """Wrap ``owner.name`` around whatever it resolves to now (MRO included).

    Returns ``"wrapped"`` when installed, ``"already"`` when the current attribute is already our wrapper
    for this intervention (reconnect, second profile instance). The wrapper keeps ``__wrapped__`` so
    signature probes keep seeing the original's parameters.
    """
    current = getattr(owner, name)
    if is_own_wrapper(current, intervention_id):
        return "already"
    wrapper = make_wrapper(current)
    functools.update_wrapper(wrapper, current)
    setattr(wrapper, WRAPPER_MARK, (intervention_id, runtime_api))
    setattr(owner, name, wrapper)
    return "wrapped"


def run_intervention(register: StatusRegister, intervention_id: str, klass: str,
                     body: Callable[[], Result]) -> Result:
    """Run one intervention body; an unexpected exception becomes a degraded, recorded outcome.

    Security interventions degrade to ``off`` (the policy then stays closed for non-operators), comfort
    interventions to ``fallback`` (upstream behaviour).
    """
    try:
        result = body()
    except Exception as exc:  # the layer must never break plugin loading or connect
        logger.exception("ctx_discord_policy: %s raised during adaptation", intervention_id)
        result = Result(intervention_id, klass, Outcome.OFF if klass == SECURITY else Outcome.FALLBACK,
                        f"{type(exc).__name__}: {exc}")
    return register.record(result)


__all__ = ["COMFORT", "SECURITY", "WRAPPER_MARK", "is_own_wrapper", "run_intervention", "wrap_attr",
           "wrapper_mark"]
