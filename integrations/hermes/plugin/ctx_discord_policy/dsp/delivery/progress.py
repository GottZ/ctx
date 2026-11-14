"""K2 (``rotate``) and K2b (``separate``): progress grouping chosen per Discord server.

Hermes groups tool-progress lines per profile and platform (``display.tool_progress_grouping``). Each
server can choose here instead: ``default`` (the profile decides), ``rotate`` (one bubble that shows only
the newest line) or ``separate`` (one message per line). A server's explicit choice wins over the
profile's grouping. The choice is looked up once per turn through ``get_progress_grouping(bot_id,
guild_id)``; a server without an active policy must answer ``default``.

Attachment points (methods of the class that runs a turn's progress loop, ``TurnRunner``):
- ``_progress_edit_state(self, adapter)`` builds the loop state once per turn; its ``can_edit`` is the
  core's own switch between one edited bubble and one message per line, derived from the turn's
  ``progress_grouping``.
- ``_progress_absorb(self, st, raw)`` folds one queued item into ``st.progress_lines`` (a repeat of the
  last tool rewrites the last line with a counter).

Adaptation (DESIGN §6.1, §6.6), decided by running the core's own methods on a synthetic turn:
- K2b ``separate``: wrap ``_progress_edit_state`` and set ``can_edit = False`` for servers that choose it.
- K2 ``rotate``, strategies in order:
  1. ``native`` — the core already keeps only the newest line when the turn's ``progress_grouping`` is
     ``"rotate"``, also when that mode is selected right before the core builds the turn's state: no
     wrapper around ``_progress_absorb``; for servers that choose rotate, the K2b wrapper selects that mode
     for the turn (needs K2b).
  2. ``wrapped`` — wrap ``_progress_absorb``: run the core, then, while the bubble is editable and holds
     more than one line, keep the core's newest line (its repeat counter included); a bubble the core
     made non-editable only because the profile says ``separate`` is made editable again.
  3. ``fallback`` — the profile's grouping applies.
Comfort class: every failure keeps the core's behaviour, and a wrapper never lets its own error escape.
"""

from __future__ import annotations

import logging
import weakref
from dataclasses import dataclass
from types import SimpleNamespace
from typing import Any, Callable, Optional

from .. import RUNTIME_API
from ..adapt.probes import ProbeResult, probe_callable
from ..adapt.status import COMFORT, Outcome, Result, StatusRegister
from ..adapt.strategies import run_intervention, wrap_attr
from ..policy.routes import platform_is_discord, snowflake

logger = logging.getLogger("ctx_discord_policy")

K2 = "K2"
K2B = "K2b"
DEFAULT = "default"
ROTATE = "rotate"
SEPARATE = "separate"
ABSORB = "_progress_absorb"
EDIT_STATE = "_progress_edit_state"
# The value of ``progress_grouping`` under which a natively rotating core keeps only the newest line.
NATIVE_ROTATE_VALUE = "rotate"
# The turn's decision, kept on the core's progress state so the lookup runs once per turn.
CHOICE_ATTR = "_dsp_progress_grouping"

Lookup = Callable[[str, str], str]


def adapter_bot_id(adapter: Any) -> str:
    """The Discord application id behind an adapter (falls back to the bot user id), or ``""``."""
    client = getattr(adapter, "_client", None)
    if client is None:
        return ""
    return (snowflake(getattr(client, "application_id", None))
            or snowflake(getattr(getattr(client, "user", None), "id", None)) or "")


@dataclass
class _Seam:
    """What the wrappers of one progress class need at call time (updated by every ``install``)."""

    lookup: Optional[Lookup] = None
    bot_id_of: Callable[[Any], str] = adapter_bot_id
    rotate: Outcome = Outcome.FALLBACK
    separate: bool = False


_SEAMS: "weakref.WeakKeyDictionary[type, _Seam]" = weakref.WeakKeyDictionary()


def _seam_for(runner: Any) -> Optional[_Seam]:
    for klass in type(runner).__mro__:
        seam = _SEAMS.get(klass)
        if seam is not None:
            return seam
    return None


def _grouping(seam: Optional[_Seam], runner: Any, adapter: Any) -> str:
    if seam is None or seam.lookup is None:
        return DEFAULT
    source = getattr(getattr(runner, "_ctx", None), "source", None)
    if source is None or not platform_is_discord(getattr(source, "platform", None)):
        return DEFAULT
    guild = snowflake(getattr(source, "scope_id", None)) or snowflake(getattr(source, "guild_id", None))
    if not guild:
        return DEFAULT
    bot = seam.bot_id_of(adapter)
    if not bot:
        return DEFAULT
    value = seam.lookup(str(bot), guild)
    return value if value in (ROTATE, SEPARATE) else DEFAULT


def turn_grouping(runner: Any, adapter: Any) -> str:
    """The server's choice for this turn: ``rotate``, ``separate`` or ``default`` (also for DMs, other
    platforms, unknown bots and unbound servers)."""
    return _grouping(_seam_for(runner), runner, adapter)


def _remember(st: Any, grouping: str) -> None:
    try:
        setattr(st, CHOICE_ATTR, grouping)
    except Exception:  # a state that takes no extra attribute: the absorb wrapper then asks per line
        pass


def _turn_choice(seam: _Seam, runner: Any, st: Any) -> str:
    cached = getattr(st, CHOICE_ATTR, None)
    if cached is not None:
        return cached
    grouping = _grouping(seam, runner, getattr(st, "adapter", None))
    _remember(st, grouping)
    return grouping


def _trimmable(st: Any) -> bool:
    lines = getattr(st, "progress_lines", None)
    return bool(getattr(st, "can_edit", False)) and isinstance(lines, list) and len(lines) > 1


def _trim(st: Any) -> None:
    st.progress_lines = st.progress_lines[-1:]


# ── wrappers ─────────────────────────────────────────────────────────────────────────────────────


def _make_absorb(original: Any) -> Any:
    def dsp_progress_absorb(self: Any, st: Any, raw: Any, *args: Any, **kwargs: Any) -> Any:
        result = original(self, st, raw, *args, **kwargs)
        try:
            seam = _seam_for(self)
            if (seam is not None and seam.rotate is Outcome.WRAPPED and _trimmable(st)
                    and _turn_choice(seam, self, st) == ROTATE):
                _trim(st)
        except Exception:  # comfort: the core's line stays as the core left it
            logger.debug("ctx_discord_policy: K2 rotate skipped for one progress line", exc_info=True)
        return result

    return dsp_progress_absorb


def _make_edit_state(original: Any) -> Any:
    def dsp_progress_edit_state(self: Any, adapter: Any, *args: Any, **kwargs: Any) -> Any:
        seam, grouping = None, DEFAULT
        try:
            seam = _seam_for(self)
            grouping = _grouping(seam, self, adapter)
            if grouping == ROTATE and seam.rotate is Outcome.NATIVE:
                # Before the core builds the state: it derives can_edit from this value.
                self._ctx.progress_grouping = NATIVE_ROTATE_VALUE
        except Exception:  # comfort: the turn keeps the profile's grouping
            logger.debug("ctx_discord_policy: K2/K2b grouping lookup skipped for one turn", exc_info=True)
            grouping = DEFAULT
        st = original(self, adapter, *args, **kwargs)
        try:
            if grouping == SEPARATE and seam.separate:
                st.can_edit = False
            elif (grouping == ROTATE and seam.rotate is Outcome.WRAPPED and not st.can_edit
                  and getattr(self._ctx, "progress_grouping", None) == SEPARATE):
                st.can_edit = True  # only the profile's own "separate" turned editing off
            _remember(st, grouping)
        except Exception:  # comfort: the core's state stays as the core built it
            logger.debug("ctx_discord_policy: K2/K2b grouping skipped for one turn", exc_info=True)
        return st

    return dsp_progress_edit_state


# ── probes (run the core's own methods on a synthetic turn) ──────────────────────────────────────


def _synthetic_turn(owner: type, grouping: str) -> Any:
    """A turn object of ``owner`` without a server: installed wrappers of this layer pass it through."""
    runner = object.__new__(owner)
    runner._ctx = SimpleNamespace(progress_grouping=grouping, _progress_metadata=None, _progress_reply_to=None,
                                  source=SimpleNamespace(platform=None, chat_id="0", scope_id=None, guild_id=None))
    return runner


def _state(lines: list) -> Any:
    return SimpleNamespace(progress_lines=list(lines), can_edit=True, adapter=None, progress_msg_id=None)


def _probe_adapter() -> Any:
    return SimpleNamespace(MAX_MESSAGE_LENGTH=2000, name="probe")


@dataclass
class RotationProbe:
    shape: ProbeResult
    native: bool = False
    steered: bool = False
    wrappable: bool = False
    evidence: str = ""


def probe_rotation(owner: type, *, steerable: bool = False) -> RotationProbe:
    """Does the core rotate by itself (and when its mode is selected the way the K2b wrapper does it), and
    does "run the core, keep its newest line" behave as intended?"""
    shape = probe_callable(owner, ABSORB, is_async=False, positional=["st", "raw"])
    if not shape:
        return RotationProbe(shape, evidence=shape.reason)
    core = getattr(owner, ABSORB)

    def absorb(grouping: str, items: tuple, *, trim: bool) -> tuple[list, list]:
        runner, st, returned = _synthetic_turn(owner, grouping), _state([]), []
        for item in items:
            returned.append(core(runner, st, item))
            if trim and _trimmable(st):
                _trim(st)
        return st.progress_lines, returned

    rotated, _ = absorb(NATIVE_ROTATE_VALUE, ("a", "b"), trim=False)
    native = rotated == ["b"]
    evidence = f"progress_grouping={NATIVE_ROTATE_VALUE!r}: ['a'] + 'b' -> {rotated}"
    steered = False
    if native and steerable:
        # The runtime path: a collecting turn, mode selected before the core builds the state, then absorb.
        runner = _synthetic_turn(owner, "accumulate")
        runner._ctx.progress_grouping = NATIVE_ROTATE_VALUE
        st = getattr(owner, EDIT_STATE)(runner, _probe_adapter())
        for item in ("a", "b"):
            core(runner, st, item)
        steered = st.can_edit is True and st.progress_lines == ["b"]
        evidence += f"; selected before the state: can_edit={st.can_edit!r}, lines {st.progress_lines}"
    # The wrapper strategy: on a collecting turn, trimming after every line keeps exactly the core's newest
    # line, also when a repeat rewrites that line with a counter, and never changes what the core returns.
    plain, plain_returned = absorb("accumulate", ("a", "b"), trim=False)
    kept, kept_returned = absorb("accumulate", ("a", "b"), trim=True)
    repeat = ("a", "b", ("__dedup__", "b", 1))
    plain_repeat, plain_repeat_returned = absorb("accumulate", repeat, trim=False)
    kept_repeat, kept_repeat_returned = absorb("accumulate", repeat, trim=True)
    wrappable = (plain == ["a", "b"] and kept == ["b"] and kept_returned == plain_returned
                 and bool(plain_repeat) and kept_repeat == plain_repeat[-1:]
                 and kept_repeat_returned == plain_repeat_returned)
    evidence += f"; collecting: {plain}, kept {kept}; with repeat: {plain_repeat}, kept {kept_repeat}"
    return RotationProbe(shape, native=native, steered=steered, wrappable=wrappable, evidence=evidence)


def probe_edit_state(owner: type) -> ProbeResult:
    """``_progress_edit_state(adapter)`` returns a state whose ``can_edit`` is the core's separate switch."""
    shape = probe_callable(owner, EDIT_STATE, is_async=False, positional=["adapter"])
    if not shape:
        return shape
    core = getattr(owner, EDIT_STATE)
    collecting = core(_synthetic_turn(owner, "accumulate"), _probe_adapter())
    separate = core(_synthetic_turn(owner, SEPARATE), _probe_adapter())
    if getattr(collecting, "can_edit", None) is not True or getattr(separate, "can_edit", None) is not False:
        return ProbeResult(False, f"{EDIT_STATE}().can_edit no longer follows the core's separate grouping "
                                  f"(collect: {getattr(collecting, 'can_edit', None)!r}, "
                                  f"separate: {getattr(separate, 'can_edit', None)!r})", shape.evidence)
    collecting.can_edit = False
    separate.can_edit = True
    if collecting.can_edit is not False or separate.can_edit is not True:
        return ProbeResult(False, f"{EDIT_STATE}().can_edit cannot be set", shape.evidence)
    return ProbeResult(True, shape.reason, shape.evidence)


# ── installation ─────────────────────────────────────────────────────────────────────────────────


def _install_separate(owner: type, seam: _Seam) -> Result:
    seam.separate = False  # until this probe has passed again (also on a reinstall)
    probe = probe_edit_state(owner)
    if not probe:
        return Result(K2B, COMFORT, Outcome.FALLBACK, f"{probe.reason}; servers get the profile's grouping",
                      probe.evidence)
    state = wrap_attr(owner, EDIT_STATE, _make_edit_state, K2B, RUNTIME_API)
    seam.separate = True
    return Result(K2B, COMFORT, Outcome.WRAPPED,
                  f"{owner.__qualname__}.{EDIT_STATE} {state}: one message per line for servers that choose "
                  f"separate", probe.evidence)


def _install_rotate(owner: type, seam: _Seam, *, steerable: bool) -> Result:
    seam.rotate = Outcome.FALLBACK  # until a strategy has been chosen again (also on a reinstall)
    probe = probe_rotation(owner, steerable=steerable)
    if probe.steered:
        seam.rotate = Outcome.NATIVE
        return Result(K2, COMFORT, Outcome.NATIVE,
                      f"core rotates by itself; servers that choose rotate get the core's mode for their turns "
                      f"({EDIT_STATE}), {ABSORB} not wrapped", probe.evidence)
    if probe.wrappable:
        state = wrap_attr(owner, ABSORB, _make_absorb, K2, RUNTIME_API)
        seam.rotate = Outcome.WRAPPED
        why = ("core rotates by itself but its mode cannot be selected per turn"
               if probe.native else "core collects every line")
        return Result(K2, COMFORT, Outcome.WRAPPED,
                      f"{owner.__qualname__}.{ABSORB} {state}: {why}; the newest line is kept for servers "
                      f"that choose rotate", probe.evidence)
    reason = probe.shape.reason if not probe.shape else "core's line folding no longer behaves as expected"
    return Result(K2, COMFORT, Outcome.FALLBACK, f"{reason}; servers that choose rotate get the profile's "
                                                 f"grouping", probe.evidence)


def install(progress_cls: Optional[type], get_progress_grouping: Optional[Lookup], register: StatusRegister,
            *, bot_id_of: Optional[Callable[[Any], str]] = None) -> list[Result]:
    """K2 and K2b on the class that runs a turn's progress loop (Hermes: ``gateway.run_turn_runner.TurnRunner``).

    ``get_progress_grouping(bot_id, guild_id)`` answers ``"default" | "rotate" | "separate"`` per server
    (anything else counts as ``default``). It is process-wide: ``TurnRunner`` exists once per process and
    runs the turns of every bot and profile, so the lookup must answer for every bot id (resolve the bot's
    own store through the runtime anchor) and answer ``default`` for servers without an active policy.
    ``bot_id_of(adapter)`` maps the delivering adapter to that bot id (default: ``adapter_bot_id``).

    Installing again (reconnect, another profile instance) re-probes, wraps nothing twice and replaces the
    lookup. Returns ``[K2, K2b]``; never raises.
    """
    if progress_cls is None or not callable(get_progress_grouping):
        missing = "progress class not available" if progress_cls is None else "no per-server grouping lookup"
        seam = _SEAMS.get(progress_cls) if progress_cls is not None else None
        if seam is not None:
            seam.rotate, seam.separate = Outcome.FALLBACK, False
        return [register.record(Result(name, COMFORT, Outcome.FALLBACK,
                                       f"{missing}; servers get the profile's grouping", "install"))
                for name in (K2, K2B)]
    seam = _SEAMS.get(progress_cls)
    if seam is None:
        seam = _SEAMS[progress_cls] = _Seam()
    seam.lookup = get_progress_grouping
    seam.bot_id_of = bot_id_of or adapter_bot_id
    k2b = run_intervention(register, K2B, COMFORT, lambda: _install_separate(progress_cls, seam))
    k2 = run_intervention(register, K2, COMFORT,
                          lambda: _install_rotate(progress_cls, seam, steerable=k2b.outcome is Outcome.WRAPPED))
    return [k2, k2b]


__all__ = ["K2", "K2B", "adapter_bot_id", "install", "probe_edit_state", "probe_rotation", "turn_grouping"]
