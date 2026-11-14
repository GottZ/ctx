"""K1: ``delete_message`` for the Discord adapter (comfort injection, DESIGN §6.2).

The fakes here are minimal stand-ins written for these tests: a base adapter whose ``delete_message`` is a
no-op returning False, and adapter classes that inherit it, override it, or lack a precondition.
"""

from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest


def make_classes(*, resolve="async", client_in_init=True, base_delete=None, own_delete=False, mixin_delete=False):
    """A fresh base/adapter pair per test (installing mutates the adapter class)."""

    async def base_noop(self, chat_id, message_id):
        return False

    base = type("BasePlatformAdapter", (), {"delete_message": base_delete or base_noop})
    namespace = {}
    if client_in_init:
        def __init__(self):
            self._client = None
            self.name = "fake-discord"
        namespace["__init__"] = __init__
    if resolve == "async":
        async def _resolve_channel(self, channel_id):
            return self._client.get_channel(int(channel_id))
        namespace["_resolve_channel"] = _resolve_channel
    elif resolve == "sync":
        def _resolve_channel(self, channel_id):
            return None
        namespace["_resolve_channel"] = _resolve_channel
    elif resolve == "two-args":
        async def _resolve_channel(self, channel_id, guild_id):
            return None
        namespace["_resolve_channel"] = _resolve_channel
    if own_delete:
        async def delete_message(self, chat_id, message_id):
            return "own"
        namespace["delete_message"] = delete_message
    bases = (base,)
    if mixin_delete:
        async def mixin_delete_message(self, chat_id, message_id):
            return "mixin"
        bases = (type("DeleteMixin", (), {"delete_message": mixin_delete_message}), base)
    adapter = type("Adapter", bases, namespace)
    return base, adapter


def fake_client(*, fail=None):
    deleted = []

    class Partial:
        def __init__(self, ident):
            self.ident = ident

        async def delete(self):
            if fail is not None:
                raise fail
            deleted.append(self.ident)

    channel = SimpleNamespace(id=555, get_partial_message=lambda ident: Partial(ident))
    return SimpleNamespace(get_channel=lambda cid: channel if cid == 555 else None), deleted


def test_injects_into_an_adapter_that_inherits_the_base_noop(anchor):
    delete = anchor.mod("delivery.delete")
    base, adapter = make_classes()
    result = delete.install(adapter, anchor.RUNTIME.status)
    assert result.outcome.value == "injected", result
    assert result.klass == "K" and result.intervention == "K1"
    assert "delete_message" in adapter.__dict__
    assert adapter.delete_message is not base.delete_message
    # The core decides "this adapter can delete" exactly like this (identity against the base no-op).
    assert getattr(adapter, "delete_message", None) not in (None, base.delete_message)
    assert anchor.RUNTIME.status.get("K1").outcome.value == "injected"


def test_injected_method_deletes_exactly_the_given_message(anchor):
    delete = anchor.mod("delivery.delete")
    _, adapter_cls = make_classes()
    delete.install(adapter_cls, anchor.RUNTIME.status)
    adapter = adapter_cls()
    adapter._client, deleted = fake_client()
    assert asyncio.run(adapter.delete_message("555", "42")) is True
    assert deleted == [42]
    # Keyword call form used by the core's ephemeral-notice cleanup.
    assert asyncio.run(adapter.delete_message(chat_id="555", message_id="43")) is True
    assert deleted == [42, 43]


def test_injected_method_reports_failure_instead_of_raising(anchor):
    delete = anchor.mod("delivery.delete")
    _, adapter_cls = make_classes()
    delete.install(adapter_cls, anchor.RUNTIME.status)
    adapter = adapter_cls()
    assert asyncio.run(adapter.delete_message("555", "42")) is False  # not connected (_client is None)
    adapter._client, _ = fake_client(fail=RuntimeError("403 Missing Permissions"))
    assert asyncio.run(adapter.delete_message("555", "42")) is False
    adapter._client, _ = fake_client()
    assert asyncio.run(adapter.delete_message("555", "not-a-snowflake")) is False
    assert asyncio.run(adapter.delete_message("999", "42")) is False  # channel unknown -> None.get_partial…


@pytest.mark.parametrize("where", ["own", "mixin"])
def test_native_delete_is_left_alone(anchor, where):
    delete = anchor.mod("delivery.delete")
    _, adapter = make_classes(own_delete=where == "own", mixin_delete=where == "mixin")
    before = adapter.__dict__.get("delete_message")
    resolved = adapter.delete_message
    result = delete.install(adapter, anchor.RUNTIME.status)
    assert result.outcome.value == "native", result
    assert adapter.__dict__.get("delete_message") is before
    assert adapter.delete_message is resolved
    assert asyncio.run(adapter.delete_message(adapter(), "1", "2")) == where


def test_second_install_injects_nothing_twice(anchor):
    delete = anchor.mod("delivery.delete")
    _, adapter = make_classes()
    first = delete.install(adapter, anchor.RUNTIME.status)
    injected = adapter.__dict__["delete_message"]
    second = delete.install(adapter, anchor.RUNTIME.status)
    assert first.outcome.value == second.outcome.value == "injected"
    assert "already" in second.reason
    assert adapter.__dict__["delete_message"] is injected
    assert not hasattr(injected, "__wrapped__")


@pytest.mark.parametrize("case", ["no-resolve", "sync-resolve", "resolve-signature", "base-extra-param",
                                  "base-optional-param", "base-sync", "no-client", "no-base"])
def test_changed_preconditions_turn_k1_off_and_leave_the_class_untouched(anchor, case):
    delete = anchor.mod("delivery.delete")

    async def base_extra(self, chat_id, message_id, reason):
        return False

    async def base_optional(self, chat_id, message_id, *, reason=None):
        return False

    def base_sync(self, chat_id, message_id):
        return False

    kwargs = {
        "no-resolve": {"resolve": None},
        "sync-resolve": {"resolve": "sync"},
        "resolve-signature": {"resolve": "two-args"},
        "base-extra-param": {"base_delete": base_extra},
        "base-optional-param": {"base_delete": base_optional},
        "base-sync": {"base_delete": base_sync},
        "no-client": {"client_in_init": False},
        "no-base": {},
    }[case]
    base, adapter = make_classes(**kwargs)
    if case == "no-base":
        base.__name__ = base.__qualname__ = "SomethingElse"
    result = delete.install(adapter, anchor.RUNTIME.status)
    assert result.outcome.value == "off", result
    assert result.reason
    assert "delete_message" not in adapter.__dict__
    assert adapter.delete_message is base.delete_message


def test_a_class_holding_the_base_noop_itself_is_not_overwritten(anchor):
    delete = anchor.mod("delivery.delete")
    base, adapter = make_classes()
    adapter.delete_message = base.__dict__["delete_message"]  # explicitly opted out of deleting
    result = delete.install(adapter, anchor.RUNTIME.status)
    assert result.outcome.value == "off", result
    assert adapter.__dict__["delete_message"] is base.__dict__["delete_message"]


def test_client_evidence_from_a_connected_instance(anchor):
    """An adapter that creates ``_client`` outside ``__init__`` passes when the bound instance has it."""
    delete = anchor.mod("delivery.delete")
    _, adapter_cls = make_classes(client_in_init=False)
    instance = adapter_cls()
    instance._client = None
    assert delete.install(adapter_cls, anchor.RUNTIME.status, adapter=instance).outcome.value == "injected"


def test_explicit_base_class_wins_over_the_mro_lookup(anchor):
    delete = anchor.mod("delivery.delete")
    base, adapter = make_classes()
    base.__name__ = base.__qualname__ = "RenamedBase"
    result = delete.install(adapter, anchor.RUNTIME.status, base_cls=base)
    assert result.outcome.value == "injected", result


def test_unexpected_error_is_a_recorded_fallback(anchor, monkeypatch):
    delete = anchor.mod("delivery.delete")
    _, adapter = make_classes()

    def boom(*args, **kwargs):
        raise RuntimeError("probe exploded")

    monkeypatch.setattr(delete, "base_adapter_class", boom)
    result = delete.install(adapter, anchor.RUNTIME.status)
    assert result.outcome.value == "fallback"
    assert "probe exploded" in result.reason
    assert "delete_message" not in adapter.__dict__
