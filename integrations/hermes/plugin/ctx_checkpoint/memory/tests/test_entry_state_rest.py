"""GottZ/ctx#299 point 2 on ``transport: rest``: the server entry is the base URL and the key.

ctx counts writes per key, so every binding of one process to the same base URL and key shares one
rate window; another base URL (or another key) is another entry and paces on its own.
"""

from __future__ import annotations

import pytest

from ctx_plugin import checkpoint as _cp


@pytest.fixture
def clock():
    return {"now": 1_000.0, "slept": []}


def _on(clock: dict, provider) -> None:
    """The provider and its chain writer on the fake clock; one part per window (three writes)."""
    provider._now = lambda: clock["now"]
    writer = provider._chain()
    writer.monotonic = lambda: clock["now"]

    def sleep(seconds: float) -> None:
        clock["slept"].append(seconds)
        clock["now"] += seconds

    writer.sleep = sleep
    writer.max_parts = 1


def _turns(tag: str) -> list[dict]:
    return [{"role": "user", "content": f"{tag} " + "x" * 400}]  # one part + manifest + head


def test_rest_bindings_to_one_key_share_its_window_and_another_base_url_does_not(make_provider, monkeypatch, clock):
    first, _ = make_provider("prof-a")
    same_key, _ = make_provider("prof-b")  # another profile, the same base URL and key
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx-other.invalid")
    other, _ = make_provider("prof-c")
    for provider in (first, same_key, other):
        _on(clock, provider)

    assert first._entry_key() == same_key._entry_key() != other._entry_key()
    assert "test-key-not-real" not in repr(first._entry_key())  # the key only as its digest

    first.on_pre_compress(_turns("a"), require_checkpoint=True)  # the key's three writes of this window
    other.on_pre_compress(_turns("c"), require_checkpoint=True)
    assert clock["slept"] == []

    same_key.on_pre_compress(_turns("b"), require_checkpoint=True)
    assert len(clock["slept"]) == 1 and 60.0 < clock["slept"][0] <= 62.0, clock["slept"]
    assert same_key._chain().entry is first._chain().entry is _cp.entry_state(first._entry_key())


def test_another_key_on_the_same_base_url_is_another_entry(make_provider, monkeypatch, clock):
    first, _ = make_provider("prof-a")
    monkeypatch.setenv("CTX_KEY", "another-test-key-not-real")
    second, _ = make_provider("prof-b")
    for provider in (first, second):
        _on(clock, provider)
    assert first._entry_key() != second._entry_key()

    first.on_pre_compress(_turns("a"), require_checkpoint=True)
    second.on_pre_compress(_turns("b"), require_checkpoint=True)
    assert clock["slept"] == []
