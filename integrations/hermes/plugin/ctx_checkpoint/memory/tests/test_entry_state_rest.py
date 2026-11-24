"""GottZ/ctx#299 point 2 on ``transport: rest``: the server entry is the base URL and the key.

ctx counts writes per key, so every binding of one process to the same base URL and key shares one
rate window; another base URL (or another key) is another entry and paces on its own.
"""

from __future__ import annotations

import json

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


# -- M1 on REST: a store that raises leaves the entry as it found it -----------------------------


class _Answer:
    def __init__(self, body: str) -> None:
        self._body = body.encode("utf-8")

    def __enter__(self):
        return self

    def __exit__(self, *exc) -> None:
        return None

    def read(self) -> bytes:
        return self._body


class _RestCtx:
    """ctx's REST API behind the real ``CtxClient`` (its ``_urlopen`` seam): whoami, an empty head
    search, and stores that book a new block. ``raise_next`` is raised by the next store, inside
    the request, as a socket or the interpreter would; ``unreadable_next`` answers it with a body
    that is no JSON (the client returns its failure envelope instead of raising)."""

    def __init__(self) -> None:
        self.stores = 0
        self.raise_next: BaseException | None = None
        self.unreadable_next = False

    def __call__(self, request, *, timeout: float):
        path = request.full_url.split("ctx.invalid", 1)[1]
        if path == "/api/whoami":
            return _Answer(json.dumps({"success": True, "home_scope": "shared", "label": "test-key"}))
        if path == "/api/search":
            return _Answer(json.dumps({"success": True, "results": []}))
        assert path == "/api/store", path
        if self.raise_next is not None:
            exc, self.raise_next = self.raise_next, None
            raise exc
        if self.unreadable_next:
            self.unreadable_next = False
            return _Answer("<html>bad gateway</html>")
        self.stores += 1
        ts = "2026-11-24T00:00:00Z"
        block = {"id": f"019f7000-0000-7000-8000-{self.stores:012d}", "created_at": ts, "updated_at": ts}
        return _Answer(json.dumps({"success": True, "block": block}))


@pytest.fixture
def rest_ctx(monkeypatch):
    from ctx_plugin import client as client_mod

    server = _RestCtx()
    monkeypatch.setattr(client_mod, "_urlopen", server)
    monkeypatch.setenv("CTX_KEY", "test-key-not-real")
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx.invalid")
    return server


def _rest_provider(tmp_path, clock, name: str):
    from ctx_plugin.provider import CtxMemoryProvider

    home = tmp_path / "profiles" / name
    home.mkdir(parents=True, exist_ok=True)
    provider = CtxMemoryProvider()
    provider.initialize(f"sess-{name}", hermes_home=str(home), agent_context="primary")
    assert provider._active
    _on(clock, provider)
    return provider


@pytest.mark.parametrize("exc_type", [RuntimeError, MemoryError, KeyboardInterrupt, SystemExit])
def test_rest_a_store_that_raises_leaves_the_entry_free_for_the_next_checkpoint(rest_ctx, tmp_path, clock, exc_type):
    """The request of the chain's first write raises (an error the client does not turn into its
    own: RuntimeError and MemoryError reach the writer as themselves, KeyboardInterrupt and
    SystemExit too). The entry ends with no call in flight, no planned writes of the chain, one
    error answer and the write's slot kept; the next checkpoint of the process runs through."""
    one = _rest_provider(tmp_path, clock, "one")
    rest_ctx.raise_next = exc_type("raised inside the request")
    with pytest.raises(_cp.CtxCheckpointError if issubclass(exc_type, Exception) else exc_type):
        one.on_pre_compress(_turns("a"), require_checkpoint=True)
    state = one._chain().entry
    assert (state.in_flight, state.writes_in_flight, state.planned_runs) == (0, 0, 0)
    assert state.errors_in_row == 1
    assert state.writes_in_window(clock["now"]) == 1

    two = _rest_provider(tmp_path, clock, "two")
    assert "ctx checkpoint head:" in two.on_pre_compress(_turns("b"), require_checkpoint=True)
    assert two._chain().entry is state
    assert (state.in_flight, state.writes_in_flight, state.planned_runs, state.errors_in_row) == (0, 0, 0, 0)
    # The kept slot: three writes per window here, so the chain's head waits for the next one.
    assert len(clock["slept"]) == 1 and 60.0 < clock["slept"][0] <= 62.0, clock["slept"]


def test_rest_an_answer_the_client_cannot_read_is_an_error_answer(rest_ctx, tmp_path, clock):
    """A 200 whose body is no JSON: the client returns ``{"success": false}`` instead of raising. It
    counted as an answer (the run of error answers ended); it is an error answer now, and the write
    keeps its slot (ctx may have stored it)."""
    one = _rest_provider(tmp_path, clock, "one")
    rest_ctx.unreadable_next = True
    with pytest.raises(_cp.CtxCheckpointError, match="rejected"):
        one.on_pre_compress(_turns("a"), require_checkpoint=True)
    state = one._chain().entry
    assert state.errors_in_row == 1
    assert state.writes_in_window(clock["now"]) == 1
    assert (state.in_flight, state.writes_in_flight, state.planned_runs) == (0, 0, 0)
