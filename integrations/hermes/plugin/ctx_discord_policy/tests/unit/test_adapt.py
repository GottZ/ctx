"""S1: adaptation-layer building blocks (probes, idempotent wrappers, outcome register)."""

from __future__ import annotations

import inspect


def test_probe_callable_checks_kind_and_parameters(anchor):
    probes = anchor.mod("adapt.probes")

    class Target:
        def sync(self, message, *, claim):
            return message

        async def handler(self, message, role_authorized=False, *, recovered=False):
            return True

        def flexible(self, source, **kwargs):
            return True

    assert probes.probe_callable(Target, "sync", is_async=False, positional=["message"], keyword_only=["claim"])
    assert not probes.probe_callable(Target, "sync", is_async=True)
    assert not probes.probe_callable(Target, "sync", positional=["other"])
    assert probes.probe_callable(Target, "handler", is_async=True, positional=["message", "role_authorized"],
                                 keyword_only=["recovered"])
    assert probes.probe_callable(Target, "flexible", positional=["source"],
                                 keyword_only=["allow_adapter_delegation"])  # through **kwargs
    assert not probes.probe_callable(Target, "missing")


def test_wrap_attr_is_idempotent_and_keeps_the_original_signature(anchor):
    strategies = anchor.mod("adapt.strategies")

    class Target:
        def method(self, message, *, claim):
            return ("original", message, claim)

    def make(original):
        def wrapper(self, *args, **kwargs):
            return ("wrapped",) + original(self, *args, **kwargs)
        return wrapper

    assert strategies.wrap_attr(Target, "method", make, "A5", 1) == "wrapped"
    first = Target.method
    assert strategies.wrap_attr(Target, "method", make, "A5", 1) == "already"
    assert Target.method is first
    assert Target().method("m", claim=True) == ("wrapped", "original", "m", True)
    assert list(inspect.signature(inspect.unwrap(Target.method)).parameters) == ["self", "message", "claim"]
    assert strategies.is_own_wrapper(Target.method, "A5")
    # A different intervention may stack its own wrapper on top.
    assert strategies.wrap_attr(Target, "method", make, "X9", 1) == "wrapped"


def test_run_intervention_degrades_security_to_off_and_comfort_to_fallback(anchor):
    strategies = anchor.mod("adapt.strategies")
    status = anchor.mod("adapt.status")
    register = status.StatusRegister()

    def boom():
        raise LookupError("target moved")

    assert strategies.run_intervention(register, "A5", "S", boom).outcome is status.Outcome.OFF
    assert strategies.run_intervention(register, "K2", "K", boom).outcome is status.Outcome.FALLBACK
    assert not register.security_ok("A5")
    register.record(status.Result("A5", "S", status.Outcome.WRAPPED, "ok"))
    assert register.security_ok("A5")


def test_probe_rejects_positional_only_keyword(anchor):
    probes = anchor.mod("adapt.probes")

    class Target:
        def admission(self, message, claim, /):
            return message

    assert not probes.probe_callable(Target, "admission", positional=["message"], keyword_only=["claim"])


def test_stacked_and_foreign_wrappers_never_double_wrap(anchor):
    import functools

    strategies = anchor.mod("adapt.strategies")
    layers = []

    class Target:
        def method(self):
            return 0

    def make(tag):
        def factory(original):
            def wrapper(self, *args, **kwargs):
                layers.append(tag)
                return original(self, *args, **kwargs)
            return wrapper
        return factory

    for _ in range(3):  # reconnects
        strategies.wrap_attr(Target, "method", make("A5"), "A5", 1)
        strategies.wrap_attr(Target, "method", make("X9"), "X9", 1)
    layers.clear()
    Target().method()
    assert layers == ["X9", "A5"]

    inner = Target.method

    @functools.wraps(inner)
    def foreign(self, *args, **kwargs):  # another plugin's well-behaved wrapper on top
        return inner(self, *args, **kwargs)

    Target.method = foreign
    assert strategies.wrap_attr(Target, "method", make("A5"), "A5", 1) == "already"


def test_static_and_class_methods_are_refused(anchor):
    strategies = anchor.mod("adapt.strategies")

    class Target:
        @staticmethod
        def helper(x):
            return x

    import pytest

    with pytest.raises(TypeError):
        strategies.wrap_attr(Target, "helper", lambda original: original, "A5", 1)
    assert Target.helper(3) == 3


def test_status_comparison_survives_odd_files(anchor, tmp_path):
    status = anchor.mod("adapt.status")
    path = tmp_path / "status.json"
    for text in ('{"interventions": []}', '{"interventions": {"A5": "armed"}}', "[]", "not json"):
        path.write_text(text)
        status.compare_with_previous(path, {"interventions": {"A5": {"outcome": "wrapped"}}})
    path.write_text('{"interventions": {"K9": {"outcome": "wrapped"}}}')
    assert any("K9" in c for c in status.compare_with_previous(path, {"interventions": {}}))
