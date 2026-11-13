"""Capability probes: check the SHAPE of a Hermes attachment point instead of its version.

A probe answers "does the target exist in the form this intervention needs?" — name, sync/async,
required parameters by name and kind. It never imports anything itself; callers hand in the object.
Wrappers pass ``*args, **kwargs`` through, so only parameters the wrapper itself reads are required.
"""

from __future__ import annotations

import inspect
from dataclasses import dataclass, field
from typing import Any, Iterable, Optional


@dataclass
class ProbeResult:
    ok: bool
    reason: str
    evidence: str = ""
    details: dict[str, Any] = field(default_factory=dict)

    def __bool__(self) -> bool:
        return self.ok


def _where(fn: Any) -> str:
    target = inspect.unwrap(fn)
    try:
        _, line = inspect.getsourcelines(target)
        return f"{getattr(target, '__module__', '?')}:{getattr(target, '__qualname__', '?')}:{line}"
    except (OSError, TypeError):
        return f"{getattr(target, '__module__', '?')}:{getattr(target, '__qualname__', '?')}"


def probe_callable(
    owner: Any,
    name: str,
    *,
    is_async: Optional[bool] = None,
    positional: Iterable[str] = (),
    keyword_only: Iterable[str] = (),
) -> ProbeResult:
    """Attribute ``owner.name`` exists, is callable, has the expected sync/async kind and parameters.

    ``positional`` names must appear (in order, after ``self`` for methods) as positional-or-keyword
    parameters; ``keyword_only`` names must be accepted as keywords (explicitly or through ``**kwargs``).
    """
    fn = getattr(owner, name, None)
    if fn is None or not callable(fn):
        return ProbeResult(False, f"{name} missing")
    target = inspect.unwrap(fn)
    actual_async = inspect.iscoroutinefunction(target)
    if is_async is not None and actual_async != is_async:
        return ProbeResult(False, f"{name} is {'async' if actual_async else 'sync'}, expected "
                                  f"{'async' if is_async else 'sync'}", _where(fn))
    try:
        params = list(inspect.signature(target).parameters.values())
    except (TypeError, ValueError):
        return ProbeResult(False, f"{name} has no inspectable signature", _where(fn))
    if params and params[0].name in {"self", "cls"}:
        params = params[1:]
    names = [p.name for p in params]
    has_var_kw = any(p.kind is inspect.Parameter.VAR_KEYWORD for p in params)
    wanted = list(positional)
    plain = [p.name for p in params if p.kind in (inspect.Parameter.POSITIONAL_ONLY,
                                                  inspect.Parameter.POSITIONAL_OR_KEYWORD)]
    if plain[: len(wanted)] != wanted:
        return ProbeResult(False, f"{name} positional parameters {plain} do not start with {wanted}", _where(fn))
    for kw in keyword_only:
        if kw not in names and not has_var_kw:
            return ProbeResult(False, f"{name} does not accept keyword {kw!r}", _where(fn))
    return ProbeResult(True, f"{name}{inspect.signature(target)}", _where(fn),
                       {"async": actual_async, "params": names})


def probe_attribute(owner: Any, name: str, expected_type: Optional[type] = None) -> ProbeResult:
    if not hasattr(owner, name):
        return ProbeResult(False, f"{name} missing")
    value = getattr(owner, name)
    if expected_type is not None and not isinstance(value, expected_type):
        return ProbeResult(False, f"{name} is {type(value).__name__}, expected {expected_type.__name__}")
    return ProbeResult(True, f"{name}: {type(value).__name__}")
