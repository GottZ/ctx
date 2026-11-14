"""Comfort injections K1 (``delete_message``), K2 (``rotate``) and K2b (``separate``) against real Hermes.

Per ref the outcome must match an oracle computed here, independently of the plugin's own probes:
- K1: the core's own predicate (``run_turn``/``base``: the adapter type can delete when its
  ``delete_message`` is not the base no-op) ⇒ ``native``, else ``injected``;
- K2: the real ``_progress_absorb`` run with ``progress_grouping="rotate"`` on ``['a'] + 'b'`` ⇒ ``['b']``
  means the core rotates by itself (``native``), ``['a', 'b']`` means it collects (``wrapped``);
- K2b: no released core selects ``separate`` per server ⇒ ``wrapped``.
For the refs this layer was built against the outcomes are also pinned (``PINNED``; more refs through
``DSP_EXPECT_COMFORT="K1=native,K2=native,K2b=wrapped"``). Afterwards the real turn path (real
``TurnContext``/``SessionSource``, real ``DiscordAdapter`` instance) must show the per-server behaviour, and
a second install must not wrap anything twice.
"""

from __future__ import annotations

import asyncio
import os
from types import SimpleNamespace

import pytest

PINNED = {
    "v0.22.4": {"K1": "injected", "K2": "wrapped", "K2b": "wrapped"},
    "v0.23.0": {"K1": "injected", "K2": "wrapped", "K2b": "wrapped"},
}
BOT = "900000000000000101"
ROTATE_GUILD = "900000000000000111"
SEPARATE_GUILD = "900000000000000112"
DEFAULT_GUILD = "900000000000000113"
CHANNEL = "900000000000000121"
MISSING = object()


def grouping_table(bot, guild):
    return {(BOT, ROTATE_GUILD): "rotate", (BOT, SEPARATE_GUILD): "separate"}.get((bot, guild), "default")


def pinned_for(ref: str) -> dict[str, str] | None:
    raw = os.environ.get("DSP_EXPECT_COMFORT", "").strip()
    if raw:
        return dict(part.split("=", 1) for part in raw.split(","))
    return PINNED.get(ref)


@pytest.fixture
def core(hermes):
    import gateway.run  # noqa: F401  - production order: the gateway is loaded before the first turn
    from gateway.config import Platform, PlatformConfig
    from gateway.platforms.base import BasePlatformAdapter
    from gateway.run_turn_runner import TurnRunner
    from gateway.session import SessionSource
    from gateway.turn_context import TurnContext
    from plugins.platforms.discord.adapter import DiscordAdapter

    return SimpleNamespace(TurnRunner=TurnRunner, Base=BasePlatformAdapter, DiscordAdapter=DiscordAdapter,
                           TurnContext=TurnContext, SessionSource=SessionSource, Platform=Platform,
                           PlatformConfig=PlatformConfig)


@pytest.fixture
def pristine(core):
    """Restore every class attribute the comfort layer may touch on the real Hermes classes."""
    touched = {core.DiscordAdapter: ("delete_message",),
               core.TurnRunner: ("_progress_absorb", "_progress_edit_state")}
    saved = {(cls, name): cls.__dict__.get(name, MISSING) for cls, names in touched.items() for name in names}
    yield saved
    for (cls, name), value in saved.items():
        if value is MISSING:
            if name in cls.__dict__:
                delattr(cls, name)
        else:
            setattr(cls, name, value)


@pytest.fixture
def layer(anchor):
    return SimpleNamespace(delete=anchor.mod("delivery.delete"), progress=anchor.mod("delivery.progress"),
                           status=anchor.RUNTIME.status)


def discord_adapter(core, cls=None):
    adapter = (cls or core.DiscordAdapter)(core.PlatformConfig(enabled=True, token="contract-test"))
    deleted = []

    class Partial:
        def __init__(self, ident):
            self.ident = ident

        async def delete(self):
            deleted.append(self.ident)

    channel = SimpleNamespace(id=int(CHANNEL), get_partial_message=lambda ident: Partial(ident))
    adapter._client = SimpleNamespace(application_id=int(BOT), user=SimpleNamespace(id=int(BOT)),
                                      get_channel=lambda cid: channel if cid == int(CHANNEL) else None)
    return adapter, deleted


def turn(core, guild, grouping="accumulate", *, platform="discord", cls=None):
    runner = object.__new__(cls or core.TurnRunner)
    runner._ctx = core.TurnContext(
        source=core.SessionSource(platform=core.Platform(platform), chat_id=CHANNEL, chat_type="group",
                                  guild_id=guild),
        progress_grouping=grouping)
    return runner


def run_turn_lines(runner, adapter, items=("a", "b", ("__dedup__", "b", 1))):
    st = runner._progress_edit_state(adapter)
    for item in items:
        runner._progress_absorb(st, item)
    return st


def k1_oracle(core) -> str:
    resolved = getattr(core.DiscordAdapter, "delete_message", None)
    return "injected" if resolved in (None, core.Base.delete_message) else "native"


def k2_oracle(core) -> tuple[str, list]:
    runner = object.__new__(core.TurnRunner)
    runner._ctx = SimpleNamespace(progress_grouping="rotate")
    st = SimpleNamespace(progress_lines=["a"], can_edit=True)
    core.TurnRunner._progress_absorb(runner, st, "b")
    return ("native" if st.progress_lines == ["b"] else "wrapped"), st.progress_lines


def install_all(layer, core, adapter_cls=None, runner_cls=None):
    k1 = layer.delete.install(adapter_cls or core.DiscordAdapter, layer.status)
    k2, k2b = layer.progress.install(runner_cls or core.TurnRunner, grouping_table, layer.status)
    return {"K1": k1, "K2": k2, "K2b": k2b}


def test_outcome_matrix_and_per_server_behaviour(hermes, core, layer, pristine, capsys):
    oracle_k1 = k1_oracle(core)
    oracle_k2, oracle_lines = k2_oracle(core)
    results = install_all(layer, core)
    got = {name: result.outcome.value for name, result in results.items()}
    expected = {"K1": oracle_k1, "K2": oracle_k2, "K2b": "wrapped"}
    report = hermes.write_report("comfort", {
        "outcomes": {name: result.as_dict() for name, result in results.items()},
        "oracle": {"K1": oracle_k1, "K2": oracle_k2, "K2_rotate_lines": oracle_lines},
    })
    with capsys.disabled():
        print(f"\n== Hermes {hermes.ref}: comfort {got} (oracle rotate lines {oracle_lines}) ({report})")
        for result in results.values():
            print(f"   {result.intervention:3} {result.outcome.value:8} {result.reason}")
    assert got == expected
    pinned = pinned_for(hermes.ref)
    if pinned is not None:
        assert got == pinned

    # K1: the core now treats the Discord adapter as able to delete, and deleting works on a real instance.
    assert getattr(core.DiscordAdapter, "delete_message", None) not in (None, core.Base.delete_message)
    adapter, deleted = discord_adapter(core)
    assert asyncio.run(adapter.delete_message(CHANNEL, "900000000000000131")) is True
    assert asyncio.run(adapter.delete_message(chat_id=CHANNEL, message_id="900000000000000132")) is True
    assert deleted == [900000000000000131, 900000000000000132]
    adapter._client = None
    assert asyncio.run(adapter.delete_message(CHANNEL, "900000000000000133")) is False
    if got["K1"] == "native":
        # Nothing set on the class (the native method may live on the class or on any mixin in its MRO).
        own = core.DiscordAdapter.__dict__.get("delete_message", MISSING)
        assert own is pristine[(core.DiscordAdapter, "delete_message")]
        assert not hasattr(core.DiscordAdapter.delete_message, "__dsp_wrapper__")

    # K2/K2b on the real turn path: the core's line for 'b' with its repeat counter is what a rotate server keeps.
    adapter, _ = discord_adapter(core)
    collected = run_turn_lines(turn(core, DEFAULT_GUILD), adapter)
    assert len(collected.progress_lines) == 2 and collected.can_edit is True
    newest = collected.progress_lines[-1]
    assert newest != "b"  # the repeat counter is on it
    rotated = run_turn_lines(turn(core, ROTATE_GUILD), adapter)
    assert rotated.progress_lines == [newest]
    assert rotated.can_edit is True
    separate = run_turn_lines(turn(core, SEPARATE_GUILD), adapter)
    assert separate.can_edit is False
    # A server's explicit choice wins over the profile's grouping in both directions.
    over_separate = run_turn_lines(turn(core, ROTATE_GUILD, "separate"), adapter)
    assert over_separate.can_edit is True and over_separate.progress_lines == [newest]
    assert run_turn_lines(turn(core, DEFAULT_GUILD, "separate"), adapter).can_edit is False
    for other in (turn(core, None), turn(core, ROTATE_GUILD, platform="telegram")):
        assert run_turn_lines(other, adapter).progress_lines == collected.progress_lines
    if got["K2"] == "native":
        # Self-withdrawal: the core's own rotation is not wrapped; the server's choice selects the core's mode.
        assert "_progress_absorb" not in core.TurnRunner.__dict__
        assert run_turn_lines(turn(core, DEFAULT_GUILD, "rotate"), adapter).progress_lines == [newest]
    else:
        assert core.TurnRunner.__dict__["_progress_absorb"].__dsp_wrapper__[0] == "K2"


def test_second_install_wraps_nothing_twice(core, layer, pristine):
    first = install_all(layer, core)
    after_first = {name: cls.__dict__.get(name) for (cls, name) in pristine}
    second = install_all(layer, core)
    assert {k: r.outcome for k, r in first.items()} == {k: r.outcome for k, r in second.items()}
    assert {name: cls.__dict__.get(name) for (cls, name) in pristine} == after_first
    for (cls, name), before in pristine.items():
        current = cls.__dict__.get(name)
        if current is not None and current is not before and hasattr(current, "__wrapped__"):
            assert not hasattr(current.__wrapped__, "__dsp_wrapper__")  # one layer, around the core


def test_future_upstream_that_does_both_natively(core, layer, pristine):
    class FutureAdapter(core.DiscordAdapter):
        async def delete_message(self, chat_id, message_id):
            return "future"

    class FutureRunner(core.TurnRunner):
        def _progress_absorb(self, st, raw):
            out = super()._progress_absorb(st, raw)
            if getattr(self._ctx, "progress_grouping", None) == "rotate":
                st.progress_lines = st.progress_lines[-1:]
            return out

    own_delete = FutureAdapter.__dict__["delete_message"]
    own_absorb = FutureRunner.__dict__["_progress_absorb"]
    results = install_all(layer, core, FutureAdapter, FutureRunner)
    assert {k: r.outcome.value for k, r in results.items()} == {"K1": "native", "K2": "native", "K2b": "wrapped"}
    assert FutureAdapter.__dict__["delete_message"] is own_delete
    assert FutureRunner.__dict__["_progress_absorb"] is own_absorb
    # The real classes stay as they were.
    assert all(cls.__dict__.get(name, MISSING) is value for (cls, name), value in pristine.items())
    adapter, _ = discord_adapter(core, FutureAdapter)
    st = run_turn_lines(turn(core, ROTATE_GUILD, cls=FutureRunner), adapter, ("a", "b"))
    assert st.progress_lines == ["b"]


def test_changed_attachment_points_fall_back_without_error(core, layer, pristine):
    class ChangedAdapter(core.DiscordAdapter):
        def _resolve_channel(self, channel_id):  # became synchronous
            return None

    async def absorb_changed(self, st):  # different kind and parameters
        return None

    class ChangedRunner(core.TurnRunner):
        _progress_absorb = absorb_changed

        def _progress_edit_state(self):  # adapter parameter gone
            return None

    before = {(cls, name): cls.__dict__.get(name, MISSING) for cls in (ChangedAdapter, ChangedRunner)
              for name in ("delete_message", "_resolve_channel", "_progress_absorb", "_progress_edit_state")}
    results = install_all(layer, core, ChangedAdapter, ChangedRunner)
    got = {k: r.outcome.value for k, r in results.items()}
    assert got["K2"] == "fallback" and got["K2b"] == "fallback"
    # K1 has nothing to change on a core whose adapter deletes natively; otherwise it stays off.
    assert got["K1"] == ("native" if k1_oracle(core) == "native" else "off")
    assert all(r.reason for r in results.values())
    assert {(cls, name): cls.__dict__.get(name, MISSING) for (cls, name) in before} == before
    assert all(cls.__dict__.get(name, MISSING) is value for (cls, name), value in pristine.items())
