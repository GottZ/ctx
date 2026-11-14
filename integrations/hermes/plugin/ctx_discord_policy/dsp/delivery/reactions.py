"""Intervention K4: ambient turns run without processing reactions.

Hermes marks a message it is working on with reactions (in progress, then done/failed) in
``on_processing_start`` / ``on_processing_complete``. Those hooks also record durable handling state for
missed-message recovery, so they must still run for ambient messages; only the reactions are unwanted
there (ambient conversation is something the bot may silently read along).

Strategy: call the original with a copy of the event whose message hides the reaction methods; the
original guards its reaction calls with ``hasattr(message, "add_reaction")``. A behaviour probe runs the
real original against a recording double (once with a plain message as control, once with the hidden
one) before the wrapper is installed. Fallback: reactions stay visible.
"""

from __future__ import annotations

import asyncio
import copy
import inspect
import threading
from types import SimpleNamespace
from typing import Any, Optional

from .. import RUNTIME_API
from ..adapt.probes import ProbeResult, probe_callable
from ..adapt.status import COMFORT, Outcome, Result
from ..adapt.strategies import wrap_attr
from ..policy.routes import AMBIENT, Route
from ..runtime import RUNTIME
from .worklog import beneath

INTERVENTION = "K4"
FALLBACK_HINT = "processing reactions stay visible on ambient turns"
HIDDEN = frozenset({"add_reaction", "remove_reaction", "clear_reaction", "clear_reactions"})
PROBE_TIMEOUT = 5.0


class _Reactionless:
    """The event's message without its reaction methods; everything else is the message itself."""

    def __init__(self, message: Any) -> None:
        object.__setattr__(self, "_message", message)

    def __getattr__(self, name: str) -> Any:
        if name in HIDDEN:
            raise AttributeError(name)
        return getattr(self._message, name)


def hide_reactions(event: Any) -> Any:
    """A shallow copy of ``event`` whose ``raw_message`` cannot take reactions."""
    raw = getattr(event, "raw_message", None)
    if raw is None:
        return event
    clone = copy.copy(event)
    clone.raw_message = _Reactionless(raw)
    return clone


def event_keys(event: Any) -> list[tuple[str, str]]:
    keys = []
    source = getattr(event, "source", None)
    raw = getattr(event, "raw_message", None)
    message_id = str(getattr(event, "message_id", None) or getattr(source, "message_id", None)
                     or getattr(raw, "id", None) or "")
    for channel in (getattr(source, "chat_id", None), getattr(getattr(raw, "channel", None), "id", None)):
        if channel is not None and message_id:
            keys.append((str(channel), message_id))
    return keys


def event_route(event: Any) -> Optional[Route]:
    """The policy route of the message this processing event belongs to (route registry, then context)."""
    for key in event_keys(event):
        route = RUNTIME.message_routes.get(key)
        if isinstance(route, Route):
            return route
    current = RUNTIME.route.get()
    if isinstance(current, Route) and current.key in event_keys(event):
        return current
    return None


def is_ambient(event: Any) -> bool:
    route = event_route(event)
    return route is not None and route.mode == AMBIENT


def _make(original: Any) -> Any:
    async def policy_processing(self: Any, event: Any, *args: Any, **kwargs: Any) -> Any:
        try:
            if is_ambient(event):
                event = hide_reactions(event)
        except Exception:
            pass  # comfort only: on any doubt the stock hook runs unchanged
        return await original(self, event, *args, **kwargs)

    return policy_processing


# ── behaviour probe ───────────────────────────────────────────────────────────────────────────────


RECORDED = ("_reactions_enabled", "_add_reaction", "_remove_reaction", "_record_discord_processing_start",
            "_record_discord_processing_complete")


class _Recorder:
    """The reaction and durable-state helpers Hermes' processing hooks call, recording every call."""

    name = "ctx_discord_policy-probe"

    def __init__(self) -> None:
        self.reactions: list[tuple[str, str]] = []
        self.records: list[str] = []

    def _reactions_enabled(self) -> bool:
        return True

    async def _add_reaction(self, message: Any, emoji: str) -> bool:
        self.reactions.append(("add", emoji))
        return True

    async def _remove_reaction(self, message: Any, emoji: str) -> bool:
        self.reactions.append(("remove", emoji))
        return True

    def _record_discord_processing_start(self, event: Any, **_: Any) -> None:
        self.records.append(f"start:{getattr(event.raw_message, 'id', None)}")

    def _record_discord_processing_complete(self, event: Any, *_: Any, **__: Any) -> None:
        self.records.append(f"complete:{getattr(event.raw_message, 'id', None)}")


class _ProbeMessage:
    id = 4242

    async def add_reaction(self, emoji: Any) -> None:
        raise AssertionError("the probe must go through the adapter's reaction helpers")

    async def remove_reaction(self, emoji: Any, member: Any = None) -> None:
        raise AssertionError("the probe must go through the adapter's reaction helpers")


def _success_outcome(fn: Any) -> Any:
    outcome = inspect.unwrap(fn).__globals__.get("ProcessingOutcome")
    return getattr(outcome, "SUCCESS", "success")


def _probe_adapter(adapter_cls: Any, recorder: _Recorder) -> Any:
    """An uninitialised instance of the real class whose recorded helpers are the recorder's; every other
    helper the hooks call (e.g. a reaction-target resolver) is the class's own. Plain recorder if that fails."""
    try:
        instance = adapter_cls.__new__(adapter_cls)
        for name in RECORDED:
            setattr(instance, name, getattr(recorder, name))
        instance._client = None
    except Exception:
        return recorder
    try:
        instance.name = recorder.name
    except Exception:
        pass
    return instance


async def _run_pair(adapter_cls: Any, start: Any, complete: Any, event: Any) -> _Recorder:
    recorder = _Recorder()
    adapter = _probe_adapter(adapter_cls, recorder)
    await start(adapter, event)
    await complete(adapter, event, _success_outcome(complete))
    return recorder


async def _behaviour(adapter_cls: Any, start: Any, complete: Any) -> ProbeResult:
    event = SimpleNamespace(raw_message=_ProbeMessage(), message_id="4242",
                            source=SimpleNamespace(chat_id="4243", parent_chat_id=None, auto_thread_created=False,
                                                   message_id="4242"))
    control = await _run_pair(adapter_cls, start, complete, event)
    if not control.reactions:
        return ProbeResult(False, "the processing hooks set no reaction on a plain message (shape changed)")
    hidden = await _run_pair(adapter_cls, start, complete, hide_reactions(event))
    if hidden.reactions:
        return ProbeResult(False, f"hidden reaction methods still produced reactions {hidden.reactions}")
    if hidden.records != control.records:
        return ProbeResult(False, f"durable state differs: {hidden.records} vs {control.records}")
    return ProbeResult(True, f"control reactions {control.reactions}, hidden none, durable state kept "
                             f"({len(control.records)} records)")


def probe_behaviour(adapter_cls: Any, start: Any, complete: Any) -> ProbeResult:
    """Run the real hooks once in a private event loop on a worker thread (callable from any context)."""
    box: dict[str, Any] = {}

    def worker() -> None:
        try:
            box["result"] = asyncio.run(_behaviour(adapter_cls, start, complete))
        except BaseException as exc:  # report, never raise into the caller
            box["error"] = exc

    thread = threading.Thread(target=worker, name="dsp-k4-probe", daemon=True)
    thread.start()
    thread.join(PROBE_TIMEOUT)
    if thread.is_alive():
        return ProbeResult(False, "behaviour probe timed out")
    if "error" in box:
        exc = box["error"]
        return ProbeResult(False, f"behaviour probe raised {type(exc).__name__}: {exc}")
    return box["result"]


def _underlying(owner: Any, name: str) -> Any:
    return beneath(getattr(owner, name), INTERVENTION)


def install(adapter_cls: Any) -> Result:
    start = probe_callable(adapter_cls, "on_processing_start", is_async=True, positional=["event"])
    complete = probe_callable(adapter_cls, "on_processing_complete", is_async=True,
                              positional=["event", "outcome"])
    for probe in (start, complete):
        if not probe:
            return Result(INTERVENTION, COMFORT, Outcome.FALLBACK, f"{probe.reason}; {FALLBACK_HINT}",
                          probe.evidence)
    behaviour = probe_behaviour(adapter_cls, _underlying(adapter_cls, "on_processing_start"),
                                _underlying(adapter_cls, "on_processing_complete"))
    if not behaviour:
        return Result(INTERVENTION, COMFORT, Outcome.FALLBACK, f"{behaviour.reason}; {FALLBACK_HINT}",
                      start.evidence)
    states = [wrap_attr(adapter_cls, "on_processing_start", _make,
                        INTERVENTION, RUNTIME_API),
              wrap_attr(adapter_cls, "on_processing_complete", _make,
                        INTERVENTION, RUNTIME_API)]
    note = "already wrapped" if all(s == "already" for s in states) else "wrapped"
    return Result(INTERVENTION, COMFORT, Outcome.WRAPPED, f"{note}: ambient turns hide reactions; {behaviour.reason}",
                  start.evidence)


__all__ = ["FALLBACK_HINT", "event_keys", "event_route", "hide_reactions", "install", "is_ambient", "probe_behaviour"]
