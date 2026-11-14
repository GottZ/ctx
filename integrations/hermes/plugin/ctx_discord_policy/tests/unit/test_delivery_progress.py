"""K2 (``rotate``) and K2b (``separate``): progress grouping per guild (comfort, DESIGN §6.2/§6.6).

The progress "cores" below are minimal stand-ins written for these tests, shaped like the attachment points
the plugin wraps: ``_progress_edit_state(self, adapter)`` returns a state with ``can_edit`` (False only
for the core's own ``separate`` grouping), ``_progress_absorb(self, st, raw)`` appends a line or puts a
repeat counter on the last one. ``RotatingCore`` additionally keeps only the newest line when the turn's
``progress_grouping`` is ``rotate`` (a core that can rotate by itself).
"""

from __future__ import annotations

from dataclasses import dataclass
from types import SimpleNamespace
from typing import Any

import pytest

BOT = "700000000000000001"
ROTATE_GUILD = "700000000000000011"
SEPARATE_GUILD = "700000000000000012"
DEFAULT_GUILD = "700000000000000013"


@dataclass
class EditState:
    adapter: Any
    progress_lines: list
    progress_msg_id: Any
    can_edit: bool


def counted(message: str, occurrences: int) -> str:
    return f"{message} [{occurrences}x]"


class CollectingCore:
    def __init__(self, ctx):
        self._ctx = ctx

    def _progress_edit_state(self, adapter):
        return EditState(adapter, [], None, self._ctx.progress_grouping != "separate")

    def _progress_absorb(self, st, raw):
        if isinstance(raw, tuple) and len(raw) == 3 and raw[0] == "__dedup__":
            _, base, count = raw
            if not st.progress_lines:
                return base
            st.progress_lines[-1] = counted(base, count + 1)
            return st.progress_lines[-1]
        st.progress_lines.append(raw)
        return raw


class RotatingCore(CollectingCore):
    def _progress_absorb(self, st, raw):
        out = super()._progress_absorb(st, raw)
        if getattr(self._ctx, "progress_grouping", None) == "rotate":
            st.progress_lines = st.progress_lines[-1:]
        return out


def fresh(core=CollectingCore, **overrides):
    """A new subclass per test: installing wraps attributes of the class it is given."""
    return type("TurnRunnerUnderTest", (core,), dict(overrides))


def grouping_table(bot, guild):
    return {(BOT, ROTATE_GUILD): "rotate", (BOT, SEPARATE_GUILD): "separate"}.get((bot, guild), "default")


def discord_adapter(bot=BOT):
    return SimpleNamespace(_client=SimpleNamespace(application_id=int(bot), user=SimpleNamespace(id=1)))


def runner(cls, guild=ROTATE_GUILD, *, platform="discord", grouping="accumulate"):
    source = SimpleNamespace(platform=SimpleNamespace(value=platform), chat_id="5", scope_id=guild,
                             guild_id=guild)
    return cls(SimpleNamespace(source=source, progress_grouping=grouping))


def run_lines(turn, adapter, *items, can_edit=None):
    st = turn._progress_edit_state(adapter)
    if can_edit is not None:
        st.can_edit = can_edit
    returned = [turn._progress_absorb(st, item) for item in items]
    return st, returned


def outcomes(results):
    return {r.intervention: r.outcome.value for r in results}


def test_rotate_guild_keeps_only_the_newest_line_on_a_collecting_core(anchor):
    progress = anchor.mod("delivery.progress")
    cls = fresh()
    results = progress.install(cls, grouping_table, anchor.RUNTIME.status)
    assert outcomes(results) == {"K2": "wrapped", "K2b": "wrapped"}
    assert all(r.klass == "K" for r in results)
    st, returned = run_lines(runner(cls), discord_adapter(), "a", "b", "c")
    assert st.progress_lines == ["c"]
    assert returned == ["a", "b", "c"]  # the line rendered this tick is the core's, unchanged
    assert anchor.RUNTIME.status.get("K2").outcome.value == "wrapped"


def test_repeat_counter_stays_intact_on_the_kept_line(anchor):
    progress = anchor.mod("delivery.progress")
    cls = fresh()
    progress.install(cls, grouping_table, anchor.RUNTIME.status)
    st, returned = run_lines(runner(cls), discord_adapter(), "a", "b", ("__dedup__", "b", 1))
    assert st.progress_lines == [counted("b", 2)]
    assert returned[-1] == counted("b", 2)


@pytest.mark.parametrize("case", ["default-guild", "dm", "other-platform", "other-bot", "unknown-value"])
def test_other_turns_keep_the_core_grouping(anchor, case):
    progress = anchor.mod("delivery.progress")
    cls = fresh()
    table = grouping_table if case != "unknown-value" else (lambda bot, guild: "accumulate")
    progress.install(cls, table, anchor.RUNTIME.status)
    turn = {
        "default-guild": lambda: runner(cls, DEFAULT_GUILD),
        "dm": lambda: runner(cls, None),
        "other-platform": lambda: runner(cls, platform="telegram"),
        "other-bot": lambda: runner(cls),
        "unknown-value": lambda: runner(cls),
    }[case]()
    adapter = discord_adapter("700000000000000099") if case == "other-bot" else discord_adapter()
    assert progress.turn_grouping(turn, adapter) == "default"
    st, _ = run_lines(turn, adapter, "a", "b")
    assert st.progress_lines == ["a", "b"]
    assert st.can_edit is True


def test_no_trimming_while_the_bubble_cannot_be_edited(anchor):
    progress = anchor.mod("delivery.progress")
    cls = fresh()
    progress.install(cls, grouping_table, anchor.RUNTIME.status)
    st, _ = run_lines(runner(cls), discord_adapter(), "a", "b", can_edit=False)
    assert st.progress_lines == ["a", "b"]


@pytest.mark.parametrize("broken", ["lookup", "bot-id", "adapter-without-client"])
def test_lookup_failures_fall_back_to_the_core(anchor, broken):
    progress = anchor.mod("delivery.progress")
    cls = fresh()

    def raising_table(bot, guild):
        raise RuntimeError("store unavailable")

    def raising_bot_id(adapter):
        raise RuntimeError("no bot")

    kwargs = {"bot_id_of": raising_bot_id} if broken == "bot-id" else {}
    table = raising_table if broken == "lookup" else grouping_table
    progress.install(cls, table, anchor.RUNTIME.status, **kwargs)
    adapter = SimpleNamespace() if broken == "adapter-without-client" else discord_adapter()
    st, returned = run_lines(runner(cls), adapter, "a", "b")
    assert st.progress_lines == ["a", "b"]
    assert returned == ["a", "b"]
    assert st.can_edit is True


def test_separate_guild_turns_bubble_editing_off(anchor):
    progress = anchor.mod("delivery.progress")
    cls = fresh()
    progress.install(cls, grouping_table, anchor.RUNTIME.status)
    st, _ = run_lines(runner(cls, SEPARATE_GUILD), discord_adapter(), "a", "b")
    assert st.can_edit is False
    st, _ = run_lines(runner(cls, DEFAULT_GUILD), discord_adapter(), "a", "b")
    assert st.can_edit is True
    assert anchor.RUNTIME.status.get("K2b").outcome.value == "wrapped"


def test_core_that_rotates_by_itself_is_not_wrapped_and_the_guild_selects_its_mode(anchor):
    progress = anchor.mod("delivery.progress")
    cls = fresh(RotatingCore)
    results = progress.install(cls, grouping_table, anchor.RUNTIME.status)
    assert outcomes(results) == {"K2": "native", "K2b": "wrapped"}
    assert "_progress_absorb" not in cls.__dict__  # self-withdrawal: no wrapper around the native rotation
    turn = runner(cls, ROTATE_GUILD, grouping="accumulate")
    st, _ = run_lines(turn, discord_adapter(), "a", "b", ("__dedup__", "b", 1))
    assert turn._ctx.progress_grouping == "rotate"  # the core's own mode, selected for this guild's turn
    assert st.progress_lines == [counted("b", 2)]
    other = runner(cls, DEFAULT_GUILD, grouping="accumulate")
    st, _ = run_lines(other, discord_adapter(), "a", "b")
    assert other._ctx.progress_grouping == "accumulate"
    assert st.progress_lines == ["a", "b"]


def test_profile_rotation_on_a_rotating_core_is_left_to_the_core(anchor):
    progress = anchor.mod("delivery.progress")
    cls = fresh(RotatingCore)
    progress.install(cls, grouping_table, anchor.RUNTIME.status)
    st, _ = run_lines(runner(cls, DEFAULT_GUILD, grouping="rotate"), discord_adapter(), "a", "b")
    assert st.progress_lines == ["b"]
    st, _ = run_lines(runner(cls, SEPARATE_GUILD, grouping="rotate"), discord_adapter(), "a", "b")
    assert st.can_edit is False


class ResettingCore(RotatingCore):
    """Rotates when absorbing under "rotate", but its state builder maps "rotate" back to collecting (a core
    whose configuration does not know the mode): selecting the mode per turn cannot work there."""

    def _progress_edit_state(self, adapter):
        if self._ctx.progress_grouping == "rotate":
            self._ctx.progress_grouping = "accumulate"
        return super()._progress_edit_state(adapter)


def test_core_whose_state_builder_resets_the_mode_is_wrapped_instead(anchor):
    progress = anchor.mod("delivery.progress")
    cls = fresh(ResettingCore)
    results = progress.install(cls, grouping_table, anchor.RUNTIME.status)
    assert outcomes(results) == {"K2": "wrapped", "K2b": "wrapped"}
    assert "mode cannot be selected" in results[0].reason
    st, _ = run_lines(runner(cls, ROTATE_GUILD), discord_adapter(), "a", "b")
    assert st.progress_lines == ["b"]


@pytest.mark.parametrize("core", [CollectingCore, RotatingCore])
def test_lookup_runs_once_per_turn(anchor, core):
    progress = anchor.mod("delivery.progress")
    cls = fresh(core)
    calls = []

    def counting(bot, guild):
        calls.append(guild)
        return grouping_table(bot, guild)

    progress.install(cls, counting, anchor.RUNTIME.status)
    for guild in (DEFAULT_GUILD, ROTATE_GUILD):
        calls.clear()
        run_lines(runner(cls, guild), discord_adapter(), "a", "b", "c", "d", ("__dedup__", "d", 1))
        assert calls == [guild]


@pytest.mark.parametrize("core", [CollectingCore, RotatingCore])
def test_rotate_server_wins_over_a_separate_profile(anchor, core):
    progress = anchor.mod("delivery.progress")
    cls = fresh(core)
    progress.install(cls, grouping_table, anchor.RUNTIME.status)
    st, _ = run_lines(runner(cls, ROTATE_GUILD, grouping="separate"), discord_adapter(),
                      "a", "b", ("__dedup__", "b", 1))
    assert st.can_edit is True
    assert st.progress_lines == [counted("b", 2)]
    st, _ = run_lines(runner(cls, DEFAULT_GUILD, grouping="separate"), discord_adapter(), "a", "b")
    assert st.can_edit is False  # servers without a choice keep the profile's grouping


@pytest.mark.parametrize("core", [CollectingCore, RotatingCore])
def test_a_failing_reinstall_stops_the_previous_behaviour(anchor, core, monkeypatch):
    progress = anchor.mod("delivery.progress")
    cls = fresh(core)
    progress.install(cls, grouping_table, anchor.RUNTIME.status)

    def boom(*args, **kwargs):
        raise RuntimeError("attachment point changed")

    monkeypatch.setattr(progress, "probe_rotation", boom)
    monkeypatch.setattr(progress, "probe_edit_state", boom)
    assert outcomes(progress.install(cls, grouping_table, anchor.RUNTIME.status)) == {
        "K2": "fallback", "K2b": "fallback"}
    turn = runner(cls, ROTATE_GUILD)
    st, _ = run_lines(turn, discord_adapter(), "a", "b")
    assert st.progress_lines == ["a", "b"] and turn._ctx.progress_grouping == "accumulate"
    st, _ = run_lines(runner(cls, SEPARATE_GUILD), discord_adapter(), "a", "b")
    assert st.can_edit is True


@pytest.mark.parametrize("core", [CollectingCore, RotatingCore])
def test_installing_twice_wraps_once_and_takes_the_newest_lookup(anchor, core):
    progress = anchor.mod("delivery.progress")
    cls = fresh(core)
    first = progress.install(cls, lambda bot, guild: "default", anchor.RUNTIME.status)
    snapshot = {name: cls.__dict__.get(name) for name in ("_progress_absorb", "_progress_edit_state")}
    second = progress.install(cls, grouping_table, anchor.RUNTIME.status)
    assert outcomes(first) == outcomes(second)
    assert {name: cls.__dict__.get(name) for name in snapshot} == snapshot
    for name in snapshot:
        current = cls.__dict__.get(name)
        if current is not None:
            assert current.__wrapped__ is getattr(core, name)  # exactly one layer around the core
    st, _ = run_lines(runner(cls, SEPARATE_GUILD), discord_adapter(), "a")
    assert st.can_edit is False  # the second install's lookup is the one in effect


def test_missing_absorb_falls_back_and_leaves_the_class_untouched(anchor):
    progress = anchor.mod("delivery.progress")

    class NoAbsorb:
        def __init__(self, ctx):
            self._ctx = ctx

        def _progress_edit_state(self, adapter):
            return EditState(adapter, [], None, self._ctx.progress_grouping != "separate")

    results = progress.install(NoAbsorb, grouping_table, anchor.RUNTIME.status)
    assert outcomes(results) == {"K2": "fallback", "K2b": "wrapped"}
    assert "_progress_absorb" not in NoAbsorb.__dict__


async def _async_absorb(self, st, raw):
    return raw


def _absorb_one_arg(self, st):
    return None


def _absorb_drops(self, st, raw):
    return raw  # does not fold the line into the buffer: behaviour no longer what the wrapper assumes


@pytest.mark.parametrize("absorb", [_async_absorb, _absorb_one_arg, _absorb_drops, None],
                         ids=["async", "signature", "behaviour", "not-callable"])
def test_changed_absorb_falls_back_and_stays_unwrapped(anchor, absorb):
    progress = anchor.mod("delivery.progress")
    cls = fresh(_progress_absorb=absorb)
    results = progress.install(cls, grouping_table, anchor.RUNTIME.status)
    assert outcomes(results)["K2"] == "fallback"
    assert cls.__dict__["_progress_absorb"] is absorb
    assert results[0].reason


def _edit_state_without_adapter(self):
    return EditState(None, [], None, True)


def _edit_state_without_can_edit(self, adapter):
    return SimpleNamespace(progress_lines=[])


@pytest.mark.parametrize("edit_state", [_edit_state_without_adapter, _edit_state_without_can_edit],
                         ids=["signature", "no-can-edit"])
@pytest.mark.parametrize("core", [CollectingCore, RotatingCore])
def test_changed_edit_state_falls_back_and_rotation_uses_the_wrapper(anchor, edit_state, core):
    progress = anchor.mod("delivery.progress")
    cls = fresh(core, _progress_edit_state=edit_state)
    results = progress.install(cls, grouping_table, anchor.RUNTIME.status)
    assert outcomes(results) == {"K2": "wrapped", "K2b": "fallback"}
    assert cls.__dict__["_progress_edit_state"] is edit_state
    # Without the edit-state seam a rotating core cannot be steered, so the absorb wrapper trims instead.
    turn = runner(cls, ROTATE_GUILD)
    st = EditState(discord_adapter(), [], None, True)
    for item in ("a", "b"):
        turn._progress_absorb(st, item)
    assert st.progress_lines == ["b"]


def test_lookup_failure_without_the_edit_state_seam_stays_inside_the_wrapper(anchor):
    progress = anchor.mod("delivery.progress")
    cls = fresh(_progress_edit_state=_edit_state_without_adapter)

    def raising_table(bot, guild):
        raise RuntimeError("store unavailable")

    assert outcomes(progress.install(cls, raising_table, anchor.RUNTIME.status)) == {
        "K2": "wrapped", "K2b": "fallback"}
    turn = runner(cls, ROTATE_GUILD)
    st = EditState(discord_adapter(), [], None, True)
    assert [turn._progress_absorb(st, item) for item in ("a", "b")] == ["a", "b"]
    assert st.progress_lines == ["a", "b"]


@pytest.mark.parametrize("missing", ["class", "lookup"])
def test_nothing_to_attach_to_is_a_fallback(anchor, missing):
    progress = anchor.mod("delivery.progress")
    cls = fresh()
    results = progress.install(None if missing == "class" else cls,
                               None if missing == "lookup" else grouping_table, anchor.RUNTIME.status)
    assert outcomes(results) == {"K2": "fallback", "K2b": "fallback"}
    assert "_progress_absorb" not in cls.__dict__ and "_progress_edit_state" not in cls.__dict__


def test_unexpected_error_is_a_recorded_fallback(anchor, monkeypatch):
    progress = anchor.mod("delivery.progress")
    cls = fresh()

    def boom(*args, **kwargs):
        raise RuntimeError("probe exploded")

    monkeypatch.setattr(progress, "probe_edit_state", boom)
    monkeypatch.setattr(progress, "probe_rotation", boom)
    results = progress.install(cls, grouping_table, anchor.RUNTIME.status)
    assert outcomes(results) == {"K2": "fallback", "K2b": "fallback"}
    assert all("probe exploded" in r.reason for r in results)
    assert "_progress_absorb" not in cls.__dict__ and "_progress_edit_state" not in cls.__dict__


def test_bot_id_prefers_the_application_id(anchor):
    progress = anchor.mod("delivery.progress")
    client = SimpleNamespace(application_id=700000000000000005, user=SimpleNamespace(id=700000000000000006))
    assert progress.adapter_bot_id(SimpleNamespace(_client=client)) == "700000000000000005"
    client.application_id = None
    assert progress.adapter_bot_id(SimpleNamespace(_client=client)) == "700000000000000006"
    assert progress.adapter_bot_id(SimpleNamespace(_client=None)) == ""
    assert progress.adapter_bot_id(None) == ""
