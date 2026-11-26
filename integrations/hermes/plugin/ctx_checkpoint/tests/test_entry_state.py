"""GottZ/ctx#299, points 2 and 5: what the chains of one process share for one ctx server entry.

Each chain used to count its own refusals in a row and its own writes in the rate window. Two
chains of one process at the write limit (two sessions of a gateway, a compaction and its retry)
could therefore show the server entry more error answers in a row than Hermes' circuit breaker
allows, and together book twice the writes one chain may book in a window; the required facts
after a chain were not paced at all.

The double below is one ctx server entry behind Hermes' MCP dispatch (``tools/mcp_tool_handlers.py``):
calls of the entry run one at a time under its RPC lock, every answer is recorded in the order the
entry gave it, and ctx's write limit counts the booked writes of the key over a sliding 60 s
window (an identical write is confirmed before that check and books nothing). Time is virtual and
shared by every thread of a test: a call costs 50 ms, and the clock jumps to the earliest wake-up
only while every chain sleeps, so two chains interleave the way they would in one process.
"""

from __future__ import annotations

import importlib.util
import itertools
import json
import sys
import threading
import time
from pathlib import Path

import pytest

_PROVIDER_PATH = Path(__file__).resolve().parents[1] / "__init__.py"
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_entry_state", _PROVIDER_PATH, submodule_search_locations=[str(_PROVIDER_PATH.parent)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
CtxCheckpointMemoryProvider = _MODULE.CtxCheckpointMemoryProvider
CtxCheckpointError = _MODULE.CtxCheckpointError
_cp = _MODULE._cp

CALL_LATENCY_S = 0.05
WINDOW_S = 60.0
_STORE_ANSWERS = json.loads(
    (Path(__file__).resolve().parents[1] / "memory" / "tests" / "mcp_answers.json").read_text(encoding="utf-8")
)["store"]


def _host_breaker_threshold() -> int:
    from tools import mcp_tool

    return int(mcp_tool._CIRCUIT_BREAKER_THRESHOLD)


class VirtualClock:
    """Monotonic time shared by the threads of one test (see the module docstring)."""

    def __init__(self, start: float = 1_000.0) -> None:
        self.now = start
        self.slept: list[tuple[str, float]] = []
        self._cond = threading.Condition()
        self._active = 0
        self._sleeping: dict[int, float] = {}

    def monotonic(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        with self._cond:
            self.now += seconds
            self._cond.notify_all()

    def sleep(self, seconds: float) -> None:
        with self._cond:
            self.slept.append((threading.current_thread().name, seconds))
            wake = self.now + max(0.0, seconds)
            if self._active == 0:  # no chains running side by side: plain fake time
                self.now = max(self.now, wake)
                return
            me = threading.get_ident()
            self._sleeping[me] = wake
            while self.now < wake:
                earliest = min(self._sleeping.values())
                if len(self._sleeping) >= self._active and earliest > self.now:
                    self.now = earliest  # every chain sleeps: on to the first wake-up
                    self._cond.notify_all()
                    continue
                self._cond.wait(0.2)  # a chain still runs, or the first one is waking up
            del self._sleeping[me]
            self._cond.notify_all()

    def run_together(self, *jobs, timeout: float = 120.0) -> list:
        """Run the jobs in threads of their own; each one's result or the exception it raised."""
        results: list = [None] * len(jobs)
        with self._cond:
            self._active = len(jobs)

        def runner(index: int, job) -> None:
            try:
                results[index] = job()
            except BaseException as exc:  # handed back to the test
                results[index] = exc
            finally:
                with self._cond:
                    self._active -= 1
                    self._cond.notify_all()

        threads = [
            threading.Thread(target=runner, args=(i, job), name=f"chain-{i}", daemon=True) for i, job in enumerate(jobs)
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout)
            assert not thread.is_alive(), "a chain did not finish"
        return results


class EntryDouble:
    """One ctx server entry (MCP name ``name``) with ctx's write limit for its key."""

    def __init__(self, clock: VirtualClock, name: str = "ctx", *, write_limit: int = 100) -> None:
        self.clock = clock
        self.name = name
        self.write_limit = write_limit
        self.rpc_lock = threading.Lock()  # Hermes: server._rpc_lock
        self.blocks: dict[tuple[str, str], dict] = {}
        self.booked: list[float] = []
        self.book_log: list[float] = []  # every booked write, never pruned
        self.booked_writes: list[tuple[dict, float]] = []  # (store arguments, time) of each booked write
        self.answers: list[str] = []  # "ok" / "error", in the order the entry answered
        self.log: list[tuple[str, str, str, float]] = []  # (thread, tool, answer, time) of each answer
        self.counter = 0
        self.refused = 0
        # Foreign writers of the same key keep its window full: every store that is not a
        # confirmation is refused.
        self.keep_full = False
        # A store this answers True for is refused once more (the count stays with the test).
        self.refuse = None

    @property
    def max_errors_in_row(self) -> int:
        best = run = 0
        for answer in self.answers:
            run = run + 1 if answer == "error" else 0
            best = max(best, run)
        return best

    def max_booked_in_window(self, window_s: float = WINDOW_S) -> int:
        times = sorted(self.book_log)
        return max((sum(1 for t in times if start <= t < start + window_s) for start in times), default=0)

    def dispatch(self, name: str, args: dict, **_kwargs) -> str:
        server, tool = name.split("__")[1], name.rsplit("__", 1)[-1]
        assert server == self.name, (server, self.name)
        with self.rpc_lock:
            self.clock.advance(CALL_LATENCY_S)
            answer = getattr(self, f"_{tool}")(args)
            self.answers.append("error" if "error" in answer else "ok")
            self.log.append((threading.current_thread().name, tool, self.answers[-1], round(self.clock.now, 2)))
        return json.dumps(answer)

    def _store(self, args: dict) -> dict:
        key = (args["category"], args["title"])
        incoming = {
            "content": args["content"],
            "tags": list(args.get("tags") or []),
            "sensitivity": args.get("sensitivity"),
            "type": args.get("type"),
        }
        metadata = dict(args.get("metadata") or {})
        stored = self.blocks.get(key)
        if (
            stored is not None
            and all(stored[k] == v for k, v in incoming.items())
            and all(stored["metadata"].get(k) == v for k, v in metadata.items())
        ):
            return {"result": _STORE_ANSWERS["no_change"].format(title=args["title"], id=stored["id"], category=key[0])}
        now = self.clock.now
        self.booked = [t for t in self.booked if t > now - WINDOW_S]
        if self.keep_full or len(self.booked) >= self.write_limit or (self.refuse is not None and self.refuse(args)):
            self.refused += 1
            return {"error": f"Rate limit exceeded: max {self.write_limit} writes per 60 seconds"}
        self.booked.append(now)
        self.book_log.append(now)
        self.booked_writes.append((dict(args), now))
        if stored is None:
            self.counter += 1
            stored = {"id": f"019f7000-0000-7000-8000-{self.counter:012d}"}
            self.blocks[key] = stored
        stored.update(incoming, metadata=metadata, title=args["title"], category=args["category"])
        return {
            "result": _STORE_ANSWERS["stored"].format(title=args["title"], id=stored["id"], category=args["category"])
        }

    def _search(self, args: dict) -> dict:
        tags = set(args.get("tags") or [])
        rows = [
            {"id": b["id"], "title": b["title"], "category": b["category"], "type": "checkpoint", "untrusted": True}
            for b in self.blocks.values()
            if args["query"] in b["title"] and tags <= set(b["tags"])
        ]
        return {"result": json.dumps(rows[: int(args.get("limit") or 10)])}

    def _get(self, args: dict) -> dict:
        for b in self.blocks.values():
            if b["id"] == args["id"]:
                return {"result": json.dumps({k: b[k] for k in ("id", "title", "category", "content", "metadata")})}
        return {"error": f"block not found: {args['id']}"}

    def heads(self) -> dict[str, dict]:
        return {
            b["metadata"]["root_session_id"]: b
            for b in self.blocks.values()
            if b["title"].startswith("Compaction checkpoint head")
        }

    def facts(self) -> list[dict]:
        return [b for b in self.blocks.values() if b["category"] == "learnings"]


@pytest.fixture
def clock(monkeypatch):
    clock = VirtualClock()
    monkeypatch.setattr(_MODULE, "_monotonic", clock.monotonic)
    monkeypatch.setattr(_MODULE, "_sleep", clock.sleep)
    # No wait of the host bounds these chains (its own tests are in test_paced_idempotent_chain).
    monkeypatch.setattr(_MODULE, "_host_compaction_wait_s", lambda: None, raising=False)
    return clock


def _provider(dispatch, session: str, notices: list | None = None, **config):
    provider = CtxCheckpointMemoryProvider(config=dict(config), dispatch=dispatch)
    sink = notices.append if notices is not None else (lambda message: None)
    provider.initialize(session, platform="cli", warning_callback=sink)
    return provider


def _transcript(n_messages: int, tag: str, size: int = 900) -> list[dict]:
    return [
        {"role": "user" if i % 2 == 0 else "assistant", "content": f"{tag}{i:04d} " + tag[:1] * size}
        for i in range(n_messages)
    ]


def _compaction(provider, messages):
    return lambda: provider.on_pre_compress(messages, require_checkpoint=True)


# -- (i) the error answers in a row ----------------------------------------------------------


def test_the_plugin_stops_one_error_short_of_the_host_breaker():
    """Hermes opens the circuit breaker of a server entry at its third error answer in a row and
    then refuses every call to it for a minute (tools/mcp_tool.py _CIRCUIT_BREAKER_THRESHOLD)."""
    assert _host_breaker_threshold() - 1 == _cp.MAX_RATE_REFUSALS


def test_two_chains_at_the_write_limit_show_the_entry_no_more_errors_in_a_row_than_one_chain(clock):
    """Foreign writes keep the key's window full. Each chain stopped at its own second refusal, so
    two chains interleaved showed the entry three or four refusals in a row, and the third opened
    the breaker for recall, the agent's ctx tools and every other session of the profile."""
    entry = EntryDouble(clock, write_limit=3)
    entry.keep_full = True
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, budget_s=600)
    two = _provider(entry.dispatch, "session-two", chunk_chars=1_000, budget_s=600)

    outcomes = clock.run_together(
        _compaction(one, _transcript(4, "a")),
        _compaction(two, _transcript(4, "b")),
    )

    assert all(isinstance(o, CtxCheckpointError) and "rate-limited" in str(o) for o in outcomes), outcomes
    assert entry.max_errors_in_row <= _cp.MAX_RATE_REFUSALS, (entry.log, outcomes)
    assert entry.max_errors_in_row < _host_breaker_threshold()


def test_one_chain_alone_still_gets_its_second_try(clock):
    """The shared count changes nothing for a chain alone: refused, it waits and tries once more."""
    entry = EntryDouble(clock, write_limit=3)
    entry.keep_full = True
    provider = _provider(entry.dispatch, "session-alone", chunk_chars=1_000, budget_s=600)
    with pytest.raises(CtxCheckpointError, match="refused twice in a row"):
        provider.on_pre_compress(_transcript(4, "a"), require_checkpoint=True)
    assert entry.refused == 2 and entry.max_errors_in_row == 2


# -- (ii) the booked writes of the window ----------------------------------------------------


def test_two_chains_side_by_side_book_max_parts_plus_two_writes_per_window_together(clock):
    """max_parts 3: five writes per window. Each chain paced against its own five, so two chains
    side by side booked ten in one window."""
    entry = EntryDouble(clock)
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, max_parts=3, budget_s=600)
    two = _provider(entry.dispatch, "session-two", chunk_chars=1_000, max_parts=3, budget_s=600)

    outcomes = clock.run_together(
        _compaction(one, _transcript(3, "a")),  # 3 parts + manifest + head
        _compaction(two, _transcript(3, "b")),
    )

    assert all(isinstance(o, str) and "ctx checkpoint head:" in o for o in outcomes), outcomes
    assert set(entry.heads()) == {"session-one", "session-two"}
    assert len(entry.book_log) == 10
    assert entry.max_booked_in_window() <= 5, sorted(entry.book_log)


def test_a_chain_after_another_paces_against_the_writes_the_first_booked(clock):
    """ctx's limit is the chain's share here (five writes): the second chain of the process used
    to start with an empty window of its own and was refused at its second write. It waits for
    the next window now, inside its default budget (60 s): its plan holds the window the writes
    already booked take, and the user learns why the compaction waits."""
    entry = EntryDouble(clock, write_limit=5)
    notices: list[str] = []
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, max_parts=3)
    two = _provider(entry.dispatch, "session-two", notices, chunk_chars=1_000, max_parts=3)

    one.on_pre_compress(_transcript(3, "a"), require_checkpoint=True)
    two.on_pre_compress(_transcript(3, "b"), require_checkpoint=True)

    assert entry.refused == 0
    assert set(entry.heads()) == {"session-one", "session-two"}
    assert two._last_chain_receipt.rate_limited == 0
    assert len(clock.slept) == 1 and 60.0 < clock.slept[0][1] <= 62.0, clock.slept
    assert len(notices) == 1 and "other checkpoints of this Hermes process" in notices[0], notices


# -- (iii) a success of any chain ends the run of refusals ------------------------------------


def test_a_success_of_another_chain_ends_the_refusals_in_a_row(clock):
    """Chain one is refused, chain two then writes, chain one is refused again: the entry saw one
    refusal in a row each time (Hermes' breaker resets on every success), so chain one goes on
    and completes instead of stopping at its own second refusal."""
    entry = EntryDouble(clock)
    refused_for_one = {"left": 2}

    def refuse(args):
        mine = args["title"].startswith("Compaction source part ") and "a0000" in args["content"]
        if mine and refused_for_one["left"] > 0:
            refused_for_one["left"] -= 1
            return True
        return False

    entry.refuse = refuse
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, budget_s=600)
    two = _provider(entry.dispatch, "session-two", chunk_chars=1_000, budget_s=600)

    def later_chain():
        clock.sleep(30.0)  # between chain one's first refusal and its retry
        return two.on_pre_compress(_transcript(2, "b"), require_checkpoint=True)

    outcomes = clock.run_together(_compaction(one, _transcript(2, "a")), later_chain)

    assert all(isinstance(o, str) and "ctx checkpoint head:" in o for o in outcomes), outcomes
    assert entry.refused == 2 and entry.max_errors_in_row == 1, entry.answers
    assert set(entry.heads()) == {"session-one", "session-two"}


# -- (iv) two server entries ----------------------------------------------------------------


def _router(*entries: EntryDouble):
    by_name = {entry.name: entry for entry in entries}

    def dispatch(name: str, args: dict, **kwargs) -> str:
        return by_name[name.split("__")[1]].dispatch(name, args, **kwargs)

    return dispatch


def test_another_mcp_entry_is_not_paced_by_the_writes_of_the_first(clock):
    first, second = EntryDouble(clock, "ctx"), EntryDouble(clock, "ctx_b")
    dispatch = _router(first, second)
    one = _provider(dispatch, "session-one", chunk_chars=1_000, max_parts=3, budget_s=600)
    two = _provider(dispatch, "session-two", mcp_server="ctx_b", chunk_chars=1_000, max_parts=3, budget_s=600)

    one.on_pre_compress(_transcript(3, "a"), require_checkpoint=True)  # five writes: ctx's window is used up
    slept = len(clock.slept)
    two.on_pre_compress(_transcript(3, "b"), require_checkpoint=True)

    assert len(clock.slept) == slept, clock.slept
    assert len(first.book_log) == len(second.book_log) == 5
    assert set(second.heads()) == {"session-two"}


def test_refusals_on_one_entry_never_stop_a_chain_on_another(clock):
    first, second = EntryDouble(clock, "ctx", write_limit=3), EntryDouble(clock, "ctx_b", write_limit=3)
    first.keep_full = True
    dispatch = _router(first, second)
    one = _provider(dispatch, "session-one", chunk_chars=1_000, budget_s=600)
    two = _provider(dispatch, "session-two", mcp_server="ctx_b", chunk_chars=1_000, max_parts=1, budget_s=600)

    outcomes = clock.run_together(
        _compaction(one, _transcript(2, "a")),
        _compaction(two, _transcript(1, "b")),
    )

    assert isinstance(outcomes[0], CtxCheckpointError)
    assert isinstance(outcomes[1], str) and "ctx checkpoint head:" in outcomes[1], outcomes[1]
    assert second.refused == 0


# -- (v) the key and the threads --------------------------------------------------------------


def _write_entry(home: Path, url: str, **fields) -> None:
    entry = {"url": url, **fields}
    config = {
        "mcp_servers": {"ctx": entry},
        "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {"mcp_server": "ctx"}},
    }
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")  # JSON is YAML


def test_a_change_outside_the_connection_identity_keeps_the_state(clock, _isolated_hermes_home):
    """Hermes keeps the entry's connection, breaker and lock when a timeout changes (only route and
    credentials make another connection): the next chain still paces against the writes booked."""
    entry = EntryDouble(clock)
    _write_entry(_isolated_hermes_home, "https://ctx-a.example/mcp")
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, max_parts=3, budget_s=600)
    one.on_pre_compress(_transcript(3, "a"), require_checkpoint=True)

    _write_entry(_isolated_hermes_home, "https://ctx-a.example/mcp", timeout=120, connect_timeout=45)
    slept = len(clock.slept)
    two = _provider(entry.dispatch, "session-two", chunk_chars=1_000, max_parts=3, budget_s=600)
    two.on_pre_compress(_transcript(3, "b"), require_checkpoint=True)
    assert len(clock.slept) == slept + 1, clock.slept


def _in_profile(dispatch, session: str, home: Path, **config):
    home.mkdir(parents=True, exist_ok=True)
    provider = CtxCheckpointMemoryProvider(config=dict(config), dispatch=dispatch)
    provider.initialize(session, platform="cli", hermes_home=str(home), warning_callback=lambda message: None)
    return provider


@pytest.mark.parametrize("auth, shared", [(None, True), ("oauth", False)])
def test_two_profiles_with_one_connection_identity_share_one_state(clock, _isolated_hermes_home, auth, shared):
    """Under a profile multiplexer a profile adopts another profile's live connection when route and
    credentials match (tools/mcp_tool_registration.py _same_server_route): one breaker, one RPC lock,
    one ctx key's window. Keyed by profile, the two counted apart (four errors in a row again). An
    OAuth connection is never adopted: there each profile keeps its own state."""
    entry = EntryDouble(clock)
    _write_entry(_isolated_hermes_home, "https://ctx.example/mcp", **({"auth": auth} if auth else {}))
    one = _in_profile(entry.dispatch, "s-one", _isolated_hermes_home / "profiles" / "a", chunk_chars=1_000, max_parts=3)
    two = _in_profile(entry.dispatch, "s-two", _isolated_hermes_home / "profiles" / "b", chunk_chars=1_000, max_parts=3)
    assert (one._entry_key() == two._entry_key()) is shared

    one.on_pre_compress(_transcript(3, "a"), require_checkpoint=True)
    slept = len(clock.slept)
    two.on_pre_compress(_transcript(3, "b"), require_checkpoint=True)
    assert (len(clock.slept) > slept) is shared, clock.slept


def test_an_answer_ends_only_the_errors_released_before_its_call_was_sent():
    """A read answered before a refusal but released after it (answers reach their threads in
    another order than the entry gave them) must not end that refusal: else the next writes go out
    and the entry sees three refusals in a row."""
    state = _cp.EntryState()
    read = state.admit(write=False, window_writes=5, now=0.0).ticket
    write = state.admit(write=True, window_writes=5, now=0.0).ticket
    assert read is not None and write is not None
    state.release(write, error=True, now=0.1)  # refused (answered after the read)
    state.release(read, error=False, now=0.2)  # the read's answer, released late
    assert state.errors_in_row == 1
    later = state.admit(write=True, window_writes=5, now=0.3).ticket
    state.release(later, error=True, now=0.4)
    assert state.errors_in_row == 2
    probe = state.admit(write=True, window_writes=5, now=0.5)
    # The read that comes with the admission (#289) is sent after both: its answer ends them.
    assert probe.probe and probe.ticket is not None and not probe.ticket.write
    state.release(probe.ticket, error=False, now=0.7)
    assert state.errors_in_row == 0


def test_a_write_that_failed_other_than_by_a_refusal_keeps_its_slot():
    """An abandoned or cut-off write may still be booked by ctx: it counts in the window, a
    refused one does not."""
    counted = []
    for error, booked in ((True, True), (True, False), (False, True), (None, True)):
        state = _cp.EntryState()
        ticket = state.admit(write=True, window_writes=5, now=1.0).ticket
        state.release(ticket, error=error, booked=booked, now=1.0)
        counted.append(state.writes_in_window(2.0))
    assert counted == [1, 0, 1, 0]  # a call that was never sent books nothing


def test_a_changed_server_entry_starts_with_a_window_of_its_own(clock, _isolated_hermes_home):
    """The entry now names another server (Hermes connects anew and its breaker starts at zero):
    the writes booked against the old server do not pace the new one."""
    entry = EntryDouble(clock)
    _write_entry(_isolated_hermes_home, "https://ctx-a.example/mcp")
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, max_parts=3, budget_s=600)
    one.on_pre_compress(_transcript(3, "a"), require_checkpoint=True)

    _write_entry(_isolated_hermes_home, "https://ctx-other.example/mcp")
    slept = len(clock.slept)
    two = _provider(entry.dispatch, "session-two", chunk_chars=1_000, max_parts=3, budget_s=600)
    two.on_pre_compress(_transcript(3, "b"), require_checkpoint=True)
    assert len(clock.slept) == slept, clock.slept

    # The same entry again: the state is the one of that entry, the new chain paces against it.
    three = _provider(entry.dispatch, "session-three", chunk_chars=1_000, max_parts=3, budget_s=600)
    three.on_pre_compress(_transcript(3, "c"), require_checkpoint=True)
    assert len(clock.slept) > slept


def test_the_state_is_one_per_entry_whoever_asks_for_it():
    keys = [("mcp", "/home/a", "ctx", "f1"), ("mcp", "/home/a", "ctx_b", "f1"), ("rest", "https://x", "k", "")]
    seen: dict[tuple, set[int]] = {key: set() for key in keys}
    lock = threading.Lock()
    barrier = threading.Barrier(24)

    def ask(i: int) -> None:
        barrier.wait()
        for _ in range(200):
            key = keys[i % len(keys)]
            state = _cp.entry_state(key)
            with lock:
                seen[key].add(id(state))

    threads = [threading.Thread(target=ask, args=(i,)) for i in range(24)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(30)
    assert all(len(ids) == 1 for ids in seen.values()), seen
    assert len({next(iter(ids)) for ids in seen.values()}) == len(keys)


def test_many_threads_on_one_entry_never_show_it_more_errors_in_a_row_than_allowed():
    """Stress: 16 threads send reads and writes through one entry state; every write the server
    answers is refused at random, reads always succeed, and the answers reach their threads in
    another order than the server gave them. The answers, in the server's order, never hold more
    than MAX_RATE_REFUSALS errors in a row (a success released after an error it preceded must not
    end that error), and the writes admitted at one time never exceed the window's share."""
    import random
    import time

    state = _cp.EntryState()
    server_lock = threading.Lock()
    answers: list[str] = []
    window_writes = 5
    over_share: list[int] = []
    errors: list[BaseException] = []
    clock_lock = threading.Lock()
    now = [0.0]

    def monotonic() -> float:
        with clock_lock:
            return now[0]

    def worker(seed: int) -> None:
        rng = random.Random(seed)  # noqa: S311 - a reproducible stress schedule, not a secret
        try:
            for _ in range(500):
                write = rng.random() < 0.7
                admission = state.admit(write=write, window_writes=window_writes, now=monotonic())
                if admission.ticket is None:
                    time.sleep(0.0002)
                    continue  # stop, pace or wait: the caller would not send now
                if admission.probe:
                    write = False  # the read before a write at the limit (#289: its ticket comes with it)
                time.sleep(rng.random() * 0.001)  # the way to the server: calls of other threads overlap
                with server_lock:
                    with clock_lock:
                        now[0] += 2.0  # thirty answers per window: the window turns over
                    if write and state.writes_in_window(now[0]) > window_writes:
                        over_share.append(state.writes_in_window(now[0]))
                    refused = write and rng.random() < 0.5
                    answers.append("error" if refused else "ok")
                time.sleep(rng.random() * 0.001)  # the answer's way back: releases come in another order
                state.release(admission.ticket, error=refused, booked=write and not refused, now=monotonic())
                if rng.random() < 0.02:
                    with clock_lock:
                        now[0] += WINDOW_S + 2  # the window ages out now and then
        except BaseException as exc:
            errors.append(exc)

    threads = [threading.Thread(target=worker, args=(seed,)) for seed in range(16)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(60)
    assert not errors, errors
    assert answers.count("error") >= 20, answers.count("error")  # the stress did refuse
    run = best = 0
    for answer in answers:
        run = run + 1 if answer == "error" else 0
        best = max(best, run)
    assert best <= _cp.MAX_RATE_REFUSALS, best
    assert not over_share, over_share[:5]


# -- point 5: the required facts after the chain ------------------------------------------------


_FACTS = [
    "The proxy on INFRA-WWW terminates TLS for every service with Traefik 3.2 since 2026-10-01.",
    "Backups of the gitea database run nightly at 03:15 to the NAS share /volume1/backup/gitea.",
    "The staging cluster uses PostgreSQL 18.6 with pgvector 0.8.7 on host db-stage-02.",
    "Release tags are annotated and signed with the SSH key of the build host since v5.0.0.",
]


def _fact_turns() -> list[dict]:
    # An assistant turn counts as a fact when it closes a turn (config.extract_salient_facts).
    return [
        m for fact in _FACTS for m in ({"role": "user", "content": "And then?"}, {"role": "assistant", "content": fact})
    ]


def test_required_facts_wait_for_the_window_the_chain_left_full(clock):
    """ctx's limit is the checkpoint's share (max_parts + 2 writes), and the chain used all of it
    in one window. The required facts after it were sent at once and refused — each refusal one
    more error answer for the entry; now they wait for the next window, within the chain's
    deadline (its plan holds the facts' window)."""
    # The chain's text: user turns only (an assistant turn that closes a turn counts as a fact).
    messages = [{"role": "user", "content": f"u{i} " + "u" * 900} for i in range(3)] + _fact_turns()
    budget = _cp.CTX_CONTENT_MAX_BYTES - len(_cp.part_header().encode("utf-8"))
    parts = len(_cp.split_chunks(_cp.render_direct_evidence(messages), 1_000, budget))
    share = parts + 2  # max_parts = parts: the chain's writes are exactly one window's share
    entry = EntryDouble(clock, write_limit=share)
    provider = _provider(entry.dispatch, "session-facts", chunk_chars=1_000, max_parts=parts, memory=True)
    started = clock.now

    receipt = provider.on_pre_compress(messages, require_checkpoint=True)

    assert "ctx checkpoint head:" in receipt and "not stored" not in receipt, receipt
    assert entry.refused == 0, entry.answers
    assert sorted(b["content"] for b in entry.facts()) == sorted(_FACTS)
    assert len(entry.book_log) == share + len(_FACTS)
    assert entry.max_booked_in_window() <= share
    # One wait for the next window, no more: the facts follow the chain 61 s later.
    assert WINDOW_S < clock.now - started <= WINDOW_S + 1.0 + 5.0, clock.now - started


def test_a_refused_fact_takes_no_slot_of_the_window(clock):
    """A refusal is not booked by ctx: after a refused required fact the next checkpoint of the
    process finds every slot the chain before left (max_parts 3: five writes per window)."""
    entry = EntryDouble(clock)
    entry.refuse = lambda args: args["category"] == "learnings"
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, max_parts=3, budget_s=600, memory=True)
    two = _provider(entry.dispatch, "session-two", chunk_chars=1_000, max_parts=3, budget_s=600)
    first = [{"role": "user", "content": "u0 " + "u" * 300}, *_fact_turns()[:2]]  # one part, one fact

    receipt = one.on_pre_compress(first, require_checkpoint=True)
    assert "ctx checkpoint head:" in receipt and "not stored" in receipt, receipt
    assert entry.refused == 1 and len(entry.book_log) == 3
    two.on_pre_compress([{"role": "user", "content": "v0 " + "v" * 300}], require_checkpoint=True)

    window = entry.book_log[0] + WINDOW_S
    assert sum(1 for t in entry.book_log if t < window) == 5, entry.book_log


# -- round 1 of #299: chains side by side are served in the order they started ------------------


def _with_host_wait(monkeypatch, seconds: float = 300.0) -> float:
    """Hermes' wait for a compaction to make progress (its default is 300 s); the chain may use it
    less HOST_WAIT_MARGIN_S."""
    monkeypatch.setattr(_MODULE, "_host_compaction_wait_s", lambda: seconds, raising=False)
    return seconds - _cp.HOST_WAIT_MARGIN_S


def _side_by_side(clock, entry, chains: int, n_messages: int, *, stagger: float = 1.0, **config):
    """``chains`` checkpoints of one process, the k-th starting ``k * stagger`` seconds after the
    first (so the order they start in is fixed). Returns (outcomes, start times, end times, providers)."""
    config = {"chunk_chars": 1_000, "budget_s": 60, **config}
    providers = [_provider(entry.dispatch, f"session-{i}", **config) for i in range(chains)]
    origin = clock.now
    started = [0.0] * chains
    ended = [0.0] * chains

    def job(i: int):
        def run():
            clock.sleep(i * stagger)
            started[i] = clock.now
            try:
                return providers[i].on_pre_compress(_transcript(n_messages, f"c{i}x"), require_checkpoint=True)
            finally:
                ended[i] = clock.now

        return run

    outcomes = clock.run_together(*[job(i) for i in range(chains)], timeout=300.0)
    return outcomes, [s - origin for s in started], [e - origin for e in ended], providers


def _booked_for(entry, tag: str, session: str) -> list[float]:
    """When ctx booked the writes of one checkpoint: its parts carry its transcript (``tag``), its
    manifest and head its session."""
    return [
        t
        for args, t in entry.booked_writes
        if tag in args["content"] or (args.get("metadata") or {}).get("root_session_id") == session
    ]


def _writes_of(entry, chain: int) -> list[float]:
    """The booked writes of the ``chain``-th checkpoint of ``_side_by_side``."""
    return _booked_for(entry, f"c{chain}x", f"session-{chain}")


def _completed(outcomes) -> list[int]:
    return [i for i, o in enumerate(outcomes) if isinstance(o, str) and "ctx checkpoint head:" in o]


@pytest.mark.parametrize(
    "chains, n_messages, max_parts",
    [
        (2, 8, 3),  # 2 x 10 writes, five per window: four windows
        (12, 6, None),  # 12 x 8 writes, 82 per window: two windows
        (4, 20, None),  # 4 x 22
        (20, 6, None),  # 20 x 8
    ],
)
def test_chains_whose_writes_fit_the_host_wait_together_all_complete(clock, monkeypatch, chains, n_messages, max_parts):
    """Each chain planned its windows from the writes booked when it started; the writes the
    chains beside it would make were not in the plan, and every chain whose next slot came after
    its deadline stopped: with max_parts 3 two chains of ten writes both failed, with the defaults
    four of twelve chains of eight writes. The plan holds the planned writes of the chains that
    started before now, so all of them end within Hermes' wait."""
    limit = _with_host_wait(monkeypatch)
    entry = EntryDouble(clock)
    config = {"max_parts": max_parts} if max_parts else {}
    window_writes = (max_parts or _cp.DEFAULT_MAX_PARTS) + 2

    outcomes, _started, ended, providers = _side_by_side(clock, entry, chains, n_messages, **config)

    assert _completed(outcomes) == list(range(chains)), [str(o)[:120] for o in outcomes]
    assert max(ended) <= limit, ended
    assert entry.max_booked_in_window() <= window_writes
    assert entry.max_errors_in_row == 0
    assert all(p._chain().entry.planned_runs == 0 for p in providers)


def test_without_a_host_wait_the_deadline_holds_the_windows_of_the_chains_before(clock):
    """No wait of the host bounds them: three chains of ten writes at five per window need six
    windows together, and the last one's deadline holds them (its own budget is one window)."""
    entry = EntryDouble(clock)
    outcomes, _started, ended, providers = _side_by_side(clock, entry, 3, 8, max_parts=3)

    assert _completed(outcomes) == [0, 1, 2], [str(o)[:120] for o in outcomes]
    assert entry.max_booked_in_window() <= 5
    assert ended[2] > 5 * WINDOW_S  # the sixth window
    assert providers[2]._last_chain_receipt.deadline >= providers[2]._last_chain_receipt.duration_s


def test_the_chains_are_served_in_the_order_they_started(clock):
    """Five chains of ten writes at five per window: every chain's writes come after all writes of
    the chain that started before it, and they end in the order they started. Before, the slots
    went to whichever thread came first."""
    entry = EntryDouble(clock)
    outcomes, started, ended, _providers = _side_by_side(clock, entry, 5, 8, max_parts=3)

    assert _completed(outcomes) == [0, 1, 2, 3, 4], [str(o)[:120] for o in outcomes]
    writes = [_writes_of(entry, i) for i in range(5)]
    assert all(len(w) == 10 for w in writes), [len(w) for w in writes]
    for before, after in itertools.pairwise(writes):
        assert max(before) < min(after), (max(before), min(after))
    assert ended == sorted(ended) and started == sorted(started)


@pytest.mark.parametrize("chains, n_messages, complete", [(3, 8, 2), (10, 20, 1)])
def test_chains_that_do_not_fit_the_host_wait_together_stop_early_and_the_first_ones_complete(
    clock, monkeypatch, chains, n_messages, complete
):
    """Three chains of ten writes at five per window need six windows, and Hermes waits for five:
    the two that started first complete, the third stops at once — before its first write, with
    the reason — instead of every chain failing at its deadline. Its next attempt goes through."""
    limit = _with_host_wait(monkeypatch)
    entry = EntryDouble(clock)

    outcomes, started, ended, providers = _side_by_side(clock, entry, chains, n_messages, max_parts=3)

    assert _completed(outcomes) == list(range(complete)), [str(o)[:120] for o in outcomes]
    for i in range(complete, chains):
        stopped = outcomes[i]
        assert isinstance(stopped, CtxCheckpointError), stopped
        assert "other checkpoints of this Hermes process started first" in str(stopped)
        assert "Hermes' wait of 300s" in str(stopped) and "next attempt continues" in str(stopped)
        assert ended[i] - started[i] < 5.0, (i, started[i], ended[i])  # early, not at its deadline
        assert _writes_of(entry, i) == []
    assert max(ended) <= limit
    assert entry.max_booked_in_window() <= 5 and entry.max_errors_in_row == 0
    assert providers[0]._chain().entry.planned_runs == 0

    # The next attempt of a stopped one, once the others are done, completes.
    again = providers[complete].on_pre_compress(_transcript(n_messages, f"c{complete}x"), require_checkpoint=True)
    assert "ctx checkpoint head:" in again


def test_a_chain_too_long_for_the_host_wait_behind_another_keeps_what_it_saved(clock, monkeypatch):
    """A chain that cannot end within Hermes' wait even alone (30 writes, five per window) waits for
    the short chain that started before it, saves the parts that fit and stops; its next attempt
    has ctx confirm those parts and writes the rest."""
    _with_host_wait(monkeypatch)
    entry = EntryDouble(clock)
    short = _provider(entry.dispatch, "session-short", chunk_chars=1_000, max_parts=3, budget_s=60)
    long_ = _provider(entry.dispatch, "session-long", chunk_chars=1_000, max_parts=3, budget_s=60)
    long_messages = _transcript(28, "l")

    def later():
        clock.sleep(1.0)
        return long_.on_pre_compress(long_messages, require_checkpoint=True)

    outcomes = clock.run_together(lambda: short.on_pre_compress(_transcript(3, "s"), require_checkpoint=True), later)

    assert "ctx checkpoint head:" in outcomes[0]
    assert isinstance(outcomes[1], CtxCheckpointError) and "Hermes waits 300s" in str(outcomes[1]), outcomes[1]
    saved = len(_booked_for(entry, "llllllllll", "session-long"))
    assert saved == 20, saved  # windows two to five

    receipt_text = long_.on_pre_compress(long_messages, require_checkpoint=True)
    assert "ctx checkpoint head:" in receipt_text
    receipt = long_._last_chain_receipt
    assert receipt.reused == saved and receipt.writes == 30 - saved


def test_a_refused_write_keeps_its_run_s_place_in_the_order(clock):
    """ctx refuses the first chain's first write once (other writers of the key): that write is
    still to come, so the chain that started after it writes nothing before the first one is done."""
    entry = EntryDouble(clock)
    left = {"refusals": 1}

    def refuse(args: dict) -> bool:
        if left["refusals"] and "c0x" in args["content"]:
            left["refusals"] -= 1
            return True
        return False

    entry.refuse = refuse
    outcomes, _started, _ended, providers = _side_by_side(clock, entry, 2, 3, max_parts=3, budget_s=600)

    assert _completed(outcomes) == [0, 1], [str(o)[:120] for o in outcomes]
    assert entry.refused == 1
    assert max(_writes_of(entry, 0)) < min(_writes_of(entry, 1))
    assert providers[0]._chain().entry.planned_runs == 0


@pytest.mark.parametrize("chains", [3, 5])
def test_more_chains_at_the_write_limit_still_show_the_entry_no_more_than_two_errors_in_a_row(clock, chains):
    entry = EntryDouble(clock, write_limit=3)
    entry.keep_full = True

    outcomes, _started, _ended, providers = _side_by_side(clock, entry, chains, 4, stagger=0.0, budget_s=600)

    assert all(isinstance(o, CtxCheckpointError) and "rate-limited" in str(o) for o in outcomes), outcomes
    assert entry.max_errors_in_row <= _cp.MAX_RATE_REFUSALS < _host_breaker_threshold(), entry.log
    assert providers[0]._chain().entry.planned_runs == 0


def test_a_call_of_a_later_chain_gives_way_to_a_waiting_call_of_an_earlier_one():
    """At the limit of calls in flight the next free place goes to the run that started first."""
    state = _cp.EntryState()
    early, _ = state.register(5, lambda: 100.0, now=0.0, window_writes=5)
    late, _ = state.register(5, lambda: 100.0, now=0.0, window_writes=5)
    first = state.admit(write=False, window_writes=5, now=0.0, reservation=late).ticket
    second = state.admit(write=False, window_writes=5, now=0.0, reservation=late).ticket
    assert first is not None and second is not None
    assert state.admit(write=False, window_writes=5, now=0.1, reservation=early).ticket is None  # waits
    state.release(first, error=False, now=0.2)

    assert state.admit(write=False, window_writes=5, now=0.3, reservation=late).ticket is None  # gives way
    assert state.admit(write=False, window_writes=5, now=0.3, reservation=early).ticket is not None


def _with_foreign_writes(entry, count: int) -> None:
    """Other writers of the key (another Hermes process, the memory mirror) booked ``count`` writes
    in ctx's window just now; they age out of it after 60 s."""
    entry.booked.extend([entry.clock.now] * count)


def test_refusals_of_two_chains_in_a_row_stop_neither_of_them(clock, monkeypatch):
    """Other writers hold 90 of ctx's 100 writes when two chains of twenty writes start. Each chain
    is refused once, so the entry has given two error answers in a row; the shared count stopped
    both — one at once ("refused twice in a row" after its own first refusal), the other when it
    woke for the next window. Before a write at that limit a chain now sends one read alone; its
    answer ends the run, the writes go on in the next window, and both complete."""
    _with_host_wait(monkeypatch)
    entry = EntryDouble(clock)
    _with_foreign_writes(entry, 90)

    outcomes, _started, _ended, providers = _side_by_side(clock, entry, 2, 18, stagger=0.0)

    assert _completed(outcomes) == [0, 1], [str(o)[:160] for o in outcomes]
    assert entry.refused >= 2
    assert entry.max_errors_in_row <= _cp.MAX_RATE_REFUSALS, entry.log
    assert providers[0]._chain().entry.planned_runs == 0


def test_a_chain_waiting_behind_a_refused_one_follows_its_delay_within_the_host_wait(clock, monkeypatch):
    """Other writers hold 97 of ctx's 100: the first chain (five writes, five per window) is refused
    after three and gets one more window; the second planned its deadline behind the first one's
    plan without that window and stopped at it, although Hermes' wait would have held it."""
    limit = _with_host_wait(monkeypatch)
    entry = EntryDouble(clock)
    _with_foreign_writes(entry, 97)

    outcomes, _started, ended, _providers = _side_by_side(clock, entry, 2, 3, max_parts=3)

    assert _completed(outcomes) == [0, 1], [str(o)[:160] for o in outcomes]
    assert entry.refused >= 1
    assert max(ended) <= limit
    assert entry.max_errors_in_row <= _cp.MAX_RATE_REFUSALS


def test_a_chain_after_one_that_cannot_finish_counts_only_what_that_one_can_still_send(clock, monkeypatch):
    """A chain of 40 writes at five per window cannot end within Hermes' wait and stops at its
    deadline; a chain of ten writes that starts 100 s later counted all of its outstanding writes,
    found its own turn behind its own limit and stopped at once, although the windows after the
    long chain's deadline were free for it."""
    _with_host_wait(monkeypatch)
    entry = EntryDouble(clock)
    long_ = _provider(entry.dispatch, "session-long", chunk_chars=1_000, max_parts=3, budget_s=60)
    short = _provider(entry.dispatch, "session-short", chunk_chars=1_000, max_parts=3, budget_s=60)

    def later():
        clock.sleep(100.0)
        return short.on_pre_compress(_transcript(8, "s"), require_checkpoint=True)

    outcomes = clock.run_together(lambda: long_.on_pre_compress(_transcript(38, "l"), require_checkpoint=True), later)

    assert isinstance(outcomes[0], CtxCheckpointError) and "Hermes waits 300s" in str(outcomes[0]), outcomes[0]
    assert isinstance(outcomes[1], str) and "ctx checkpoint head:" in outcomes[1], outcomes[1]
    assert entry.max_booked_in_window() <= 5


def test_a_run_plans_its_deadline_behind_the_planned_writes_of_the_runs_before_it():
    """The plan of a run that starts while another one has fifteen writes planned (five per window)
    holds the three windows those take before its own: its deadline is four windows out."""
    writer = _cp.ChainWriter(client=None, max_parts=3)
    writer.entry = _cp.EntryState()
    first = writer.new_run(started=0.0, deadline=60.0, budget_s=60.0, planned_writes=15)
    second = writer.new_run(started=0.0, deadline=60.0, budget_s=60.0, planned_writes=5)

    assert first.deadline == 60.0 + 2 * (WINDOW_S + 1.0)
    assert second.planned_windows == 4
    assert second.deadline == 60.0 + 3 * (WINDOW_S + 1.0)
    first.close()
    third = writer.new_run(started=0.0, deadline=60.0, budget_s=60.0, planned_writes=5)
    assert third.planned_windows == 2  # behind the second one only


# -- M1: a call that raises leaves nothing at the entry ------------------------------------------


@pytest.mark.parametrize("exc_type", [RuntimeError, MemoryError, KeyboardInterrupt, SystemExit])
def test_a_call_that_raises_in_the_dispatch_leaves_the_entry_free_for_the_next_checkpoint(clock, exc_type):
    """The dispatch raises on the chain's first write (inside the bounded call). The entry must end
    with no call in flight, no planned writes of the failed chain, the failure counted as one
    error answer and the write's slot kept (ctx may have booked it); the next checkpoint of the
    process runs through. RuntimeError and MemoryError reach the writer as dispatch failures,
    KeyboardInterrupt and SystemExit as themselves."""
    entry = EntryDouble(clock)
    left = {"raise": 1}

    def dispatch(name: str, args: dict, **kwargs) -> str:
        if name.endswith("__store") and left["raise"]:
            left["raise"] = 0
            raise exc_type("raised inside the call")
        return entry.dispatch(name, args, **kwargs)

    one = _provider(dispatch, "session-one", chunk_chars=1_000, max_parts=3, budget_s=600)
    with pytest.raises(CtxCheckpointError if issubclass(exc_type, Exception) else exc_type):
        one.on_pre_compress(_transcript(3, "a"), require_checkpoint=True)
    state = one._chain().entry
    assert (state.in_flight, state.writes_in_flight, state.planned_runs) == (0, 0, 0)
    assert state.errors_in_row == 1
    assert state.writes_in_window(clock.now) == 1

    two = _provider(dispatch, "session-two", chunk_chars=1_000, max_parts=3, budget_s=600)
    assert "ctx checkpoint head:" in two.on_pre_compress(_transcript(3, "b"), require_checkpoint=True)
    assert (state.in_flight, state.writes_in_flight, state.planned_runs, state.errors_in_row) == (0, 0, 0, 0)


def test_a_failed_chain_leaves_no_planned_writes_for_the_next_one(clock):
    """A chain stopped by ctx's refusals ends its run: the next checkpoint plans one window, as on
    an entry nobody used, and does not wait for writes that will never be sent."""
    entry = EntryDouble(clock, write_limit=3)
    entry.keep_full = True
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, max_parts=3, budget_s=60)
    with pytest.raises(CtxCheckpointError, match="refused twice in a row"):
        one.on_pre_compress(_transcript(20, "a"), require_checkpoint=True)
    entry.keep_full, entry.write_limit = False, 100
    slept = len(clock.slept)

    two = _provider(entry.dispatch, "session-two", chunk_chars=1_000, max_parts=3, budget_s=60)
    started = clock.now
    two.on_pre_compress(_transcript(3, "b"), require_checkpoint=True)

    assert len(clock.slept) == slept
    assert two._last_chain_receipt.deadline == started + 60.0
    assert two._chain().entry.planned_runs == 0


# -- L2: a read at the limit ------------------------------------------------------------------------


def test_a_read_sent_at_the_limit_that_fails_ends_its_chain(clock):
    """After two error answers in a row no write goes out, but a read still does, alone: it is how
    a later checkpoint learns that the entry answers again. When that read fails, its chain sends
    nothing more (the head search is the first call of every chain and its failure ends it)."""
    entry = EntryDouble(clock, write_limit=3)
    entry.keep_full = True
    one = _provider(entry.dispatch, "session-one", chunk_chars=1_000, budget_s=600)
    with pytest.raises(CtxCheckpointError, match="refused twice in a row"):
        one.on_pre_compress(_transcript(4, "a"), require_checkpoint=True)
    assert one._chain().entry.errors_in_row == _cp.MAX_RATE_REFUSALS
    calls = len(entry.log)
    entry._search = lambda args: {"error": "search backend unavailable"}

    two = _provider(entry.dispatch, "session-two", chunk_chars=1_000, budget_s=600)
    with pytest.raises(CtxCheckpointError, match="head search failed"):
        two.on_pre_compress(_transcript(4, "b"), require_checkpoint=True)

    assert [(tool, answer) for _thread, tool, answer, _t in entry.log[calls:]] == [("search", "error")]
    assert one._chain().entry.errors_in_row == _cp.MAX_RATE_REFUSALS + 1


# -- L1: the key follows Hermes' own connection identity ----------------------------------------


_SAME_CONNECTION = {
    "include order": (
        {"url": "https://c.example/mcp", "tools": {"include": ["search", "store"]}},
        {"url": "https://c.example/mcp", "tools": {"include": ["store", "search"]}},
    ),
    "ssl_verify true named": ({"url": "https://c.example/mcp"}, {"url": "https://c.example/mcp", "ssl_verify": True}),
    "empty env": ({"url": "https://c.example/mcp"}, {"url": "https://c.example/mcp", "env": {}}),
    "empty args": ({"command": "ctx-mcp"}, {"command": "ctx-mcp", "args": []}),
    "strict_redirect_headers false": (
        {"url": "https://c.example/mcp"},
        {"url": "https://c.example/mcp", "strict_redirect_headers": False},
    ),
    "another tools flag": (
        {"url": "https://c.example/mcp", "tools": {"include": ["store"]}},
        {"url": "https://c.example/mcp", "tools": {"include": ["store"], "resources": False}},
    ),
    "a timeout": ({"url": "https://c.example/mcp", "timeout": 30}, {"url": "https://c.example/mcp", "timeout": 300}),
}
_OTHER_CONNECTION = {
    "another url": ({"url": "https://c.example/mcp"}, {"url": "https://d.example/mcp"}),
    "another header": (
        {"url": "https://c.example/mcp", "headers": {"Authorization": "Bearer one-0123456789"}},
        {"url": "https://c.example/mcp", "headers": {"Authorization": "Bearer two-0123456789"}},
    ),
    "ssl_verify off": ({"url": "https://c.example/mcp"}, {"url": "https://c.example/mcp", "ssl_verify": False}),
    "another include": (
        {"url": "https://c.example/mcp", "tools": {"include": ["store"]}},
        {"url": "https://c.example/mcp", "tools": {"include": ["search"]}},
    ),
    "strict_redirect_headers on": (
        {"url": "https://c.example/mcp"},
        {"url": "https://c.example/mcp", "strict_redirect_headers": True},
    ),
}


def _key_and_entry(home: Path, entry: dict) -> tuple[tuple, dict]:
    from hermes_cli.config import load_config

    _write_entry(home, **entry) if "url" in entry else _write_command_entry(home, entry)
    provider = CtxCheckpointMemoryProvider(config={})
    provider.initialize("s", platform="cli", hermes_home=str(home), warning_callback=lambda message: None)
    return provider._entry_key(), load_config()["mcp_servers"]["ctx"]


def _write_command_entry(home: Path, entry: dict) -> None:
    config = {"mcp_servers": {"ctx": entry}, "memory": {"provider": "ctx_checkpoint"}}
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")


@pytest.mark.parametrize("pair", [*_SAME_CONNECTION, *_OTHER_CONNECTION])
def test_two_entries_share_a_state_exactly_when_hermes_sees_one_connection(_isolated_hermes_home, pair):
    """The key used the entry's raw fields: entries Hermes treats as one connection (one breaker, one
    RPC lock) got two states, so their chains together could show it four error answers in a row."""
    from tools.mcp_tool_registration import _connection_identity

    a, b = {**_SAME_CONNECTION, **_OTHER_CONNECTION}[pair]
    key_a, effective_a = _key_and_entry(_isolated_hermes_home, a)
    key_b, effective_b = _key_and_entry(_isolated_hermes_home, b)

    hermes_same = _connection_identity(effective_a) == _connection_identity(effective_b)
    assert hermes_same is (pair in _SAME_CONNECTION)
    assert (key_a == key_b) is hermes_same, (key_a, key_b)


def test_without_hermes_connection_identity_the_key_is_the_raw_entry_as_before(_isolated_hermes_home, monkeypatch):
    """A Hermes without ``_connection_identity``: the raw connection fields, the key 0.17.0 used
    before it followed Hermes' function."""
    import hashlib

    import tools.mcp_tool_registration as registration

    monkeypatch.delattr(registration, "_connection_identity")
    a, b = _SAME_CONNECTION["include order"]
    key_a, _ = _key_and_entry(_isolated_hermes_home, a)
    key_b, _ = _key_and_entry(_isolated_hermes_home, b)

    assert key_a != key_b
    raw = json.dumps({"url": a["url"], "tools": a["tools"]}, sort_keys=True, ensure_ascii=False, default=str)
    assert key_a == ("mcp", "ctx", hashlib.sha256(raw.encode("utf-8", "surrogatepass")).hexdigest()[:32], "")


# -- GottZ/ctx#289: the follow-ups of #299 ------------------------------------------------------


class _WriterCtx:
    """ctx as the chain writer's client sees it (the REST client's answer shapes): reads answer
    with no row, a store books a new block — unless ``refuse`` is above zero (ctx's write limit)
    or ``searches_fail_after`` reads have answered (a search backend that fails from then on)."""

    def __init__(self, *, refuse: int = 0, searches_fail_after: int | None = None, on_search=None) -> None:
        self.refuse = refuse
        self.searches_fail_after = searches_fail_after
        self.on_search = on_search
        self.searches = 0
        self.stores: list[str] = []

    def search(self, *, timeout=None, **kwargs) -> dict:
        self.searches += 1
        if self.on_search is not None:
            self.on_search(self.searches)
        if self.searches_fail_after is not None and self.searches > self.searches_fail_after:
            raise _cp.CtxApiError("search backend unavailable")
        return {"success": True, "results": []}

    def get_block(self, *, block_id, timeout=None) -> dict:
        raise AssertionError("no block to read")

    def store(self, *, timeout=None, **kwargs) -> dict:
        if self.refuse > 0:
            self.refuse -= 1
            raise _cp.CtxApiError("Rate limit exceeded: max 100 writes per 60 seconds", status=429, code="rate_limit")
        self.stores.append(kwargs["title"])
        ts = "2026-11-26T00:00:00Z"
        return {"block": {"id": f"019f7000-0000-7000-8000-{len(self.stores):012d}", "created_at": ts, "updated_at": ts}}


def _writer(ctx, state, *, clock=None, **fields):
    seams = {"sleep": clock.sleep, "monotonic": clock.monotonic} if clock is not None else {}
    return _cp.ChainWriter(client=ctx, chunk_chars=1_000, max_parts=3, entry=state, **seams, **fields)


def _at_the_limit(state: _cp.EntryState, now: float = 0.0) -> None:
    """Two refused writes: the entry's last MAX_RATE_REFUSALS answers were errors."""
    for _ in range(_cp.MAX_RATE_REFUSALS):
        ticket = state.admit(write=True, window_writes=5, now=now).ticket
        assert ticket is not None
        state.release(ticket, error=True, now=now)
    assert state.errors_in_row == _cp.MAX_RATE_REFUSALS


class _AnswerAfterTheFirstRefusal(_cp.EntryState):
    """The entry answers a call of another chain right after the first refusal is released — after
    its release, before the refused chain looks at the entry's answers (in production another
    thread releases it there)."""

    def __init__(self) -> None:
        super().__init__()
        self.between = 0

    def release(self, ticket, *, error, booked=False, now=0.0):
        seen = super().release(ticket, error=error, booked=booked, now=now)
        if error and ticket.write and not self.between:
            self.between += 1
            other = self.admit(write=False, window_writes=5, now=now)
            assert other.ticket is not None
            super().release(other.ticket, error=False, now=now)
        return seen


def test_an_answer_released_after_a_refusal_ends_the_refusals_in_a_row_of_the_refused_write(clock):
    """A1: the refused write read the entry's count of answers after ``release``, outside its lock;
    an answer of another chain released in between moved that baseline past the answer, so the
    write's next refusal counted as its second in a row and the chain stopped although an answer
    of the entry lay between its two refusals (the run Hermes counts had ended). The count is the
    one ``release`` saw under the entry's lock now."""
    state = _AnswerAfterTheFirstRefusal()
    ctx = _WriterCtx(refuse=2)

    receipt = _writer(ctx, state, clock=clock).write(
        _transcript(1, "a"), root_session_id="root-a1", session_id="s-a1", budget_s=600, host_api=2, negotiated_api=2
    )

    assert receipt.head_id and receipt.rate_limited == 2, receipt
    assert state.between == 1
    assert state.errors_in_row == 0 and state.in_flight == 0


def test_two_writers_at_the_limit_are_not_both_told_to_probe():
    """A2: after two error answers in a row both writers were told to probe; the second one's read
    then went out after the first probe had ended the run — an ordinary read, and its failure ended
    a chain the entry no longer held back. The check of the limit and the probe ticket are one
    operation now: the first writer gets the read, the second waits for its answer."""
    state = _cp.EntryState()
    _at_the_limit(state)

    first = state.admit(write=True, window_writes=5, now=1.0)
    second = state.admit(write=True, window_writes=5, now=1.0)

    assert first.probe and first.ticket is not None and not first.ticket.write
    assert not second.probe and second.ticket is None  # waits for the probe's answer
    state.release(first.ticket, error=False, now=1.1)
    after = state.admit(write=True, window_writes=5, now=1.2)
    assert not after.probe and after.ticket is not None and after.ticket.write


class _SeesWaiting(_cp.EntryState):
    def __init__(self) -> None:
        super().__init__()
        self.waiting = threading.Event()

    def wait_for_answer(self, timeout: float) -> None:
        self.waiting.set()
        super().wait_for_answer(timeout)


def test_a_writer_that_waits_for_another_writer_s_probe_sends_no_read_of_its_own():
    """A2 over two chain writers in two threads: the first one's probe is in flight when the second
    wants to write; the probe answers, and the reads fail from then on. The second writer's own
    read went out after the run had ended, failed, and stopped it; now it waits for the probe's
    answer and writes."""
    state = _SeesWaiting()
    _at_the_limit(state, now=time.monotonic())
    later: dict = {}

    def second_writer() -> None:
        writer = _writer(ctx, state)
        run = writer.new_run(started=time.monotonic(), deadline=time.monotonic() + 30, budget_s=30, planned_writes=1)
        try:
            later["data"] = writer.store_paced("fact b", run, category="learnings", title="fact b", content="b")
        except BaseException as exc:  # handed back to the test
            later["error"] = exc
        finally:
            run.close()

    def on_search(n: int) -> None:
        if n == 1:  # the first writer's probe is in flight: the second one wants to write now
            thread = threading.Thread(target=second_writer, name="second-writer", daemon=True)
            later["thread"] = thread
            thread.start()
            assert state.waiting.wait(10), "the second writer never waited at the entry"

    ctx = _WriterCtx(searches_fail_after=1, on_search=on_search)
    first = _writer(ctx, state)
    run = first.new_run(started=time.monotonic(), deadline=time.monotonic() + 30, budget_s=30, planned_writes=1)
    try:
        first.store_paced("fact a", run, category="learnings", title="fact a", content="a")
    finally:
        run.close()
    later["thread"].join(30)

    assert "error" not in later, later.get("error")
    assert ctx.searches == 1 and sorted(ctx.stores) == ["fact a", "fact b"]
    assert state.errors_in_row == 0 and state.in_flight == 0


def test_a_later_chain_whose_transcript_fits_is_not_stopped_for_its_required_facts(clock, monkeypatch):
    """A3: a chain of 21 writes started first (five per window: its last window opens at 244 s, within
    Hermes' wait less the margin, 290 s). A chain started a second later needs three writes and has
    two required facts: its transcript fits the window at 244 s, its second fact would not. Its early
    stop counted the facts, and the whole checkpoint failed before its first write; the transcript
    writes decide now — the chain is saved, and the fact that does not fit is left for a later
    compaction, as a fact that does not fit after a stored chain always was."""
    _with_host_wait(monkeypatch)
    entry = EntryDouble(clock)
    first = _provider(entry.dispatch, "session-first", chunk_chars=1_000, max_parts=3, budget_s=60)
    later = _provider(entry.dispatch, "session-later", chunk_chars=1_000, max_parts=3, budget_s=60, memory=True)
    messages = [{"role": "user", "content": "w0 " + "w" * 300}, *_fact_turns()[:4]]
    budget = _cp.CTX_CONTENT_MAX_BYTES - len(_cp.part_header().encode("utf-8"))
    assert len(_cp.split_chunks(_cp.render_direct_evidence(messages), 1_000, budget)) == 1  # three chain writes

    def second():
        clock.sleep(1.0)
        return later.on_pre_compress(messages, require_checkpoint=True)

    outcomes = clock.run_together(
        lambda: first.on_pre_compress(_transcript(19, "f"), require_checkpoint=True), second, timeout=300.0
    )

    assert _completed(outcomes) == [0, 1], [str(o)[:200] for o in outcomes]
    assert "1 of 2 stored" in outcomes[1], outcomes[1]
    assert len(entry.facts()) == 1
    assert entry.max_booked_in_window() <= 5


def test_a_slow_notice_does_not_move_the_paced_write_past_its_slot(clock):
    """A4: the notice before a paced write (another checkpoint of the process filled the window)
    took 150 s in the user's callback. The write then slept the whole wait it had computed before
    the notice and woke 211 s after the start, 50 s past its deadline (161 s); it goes when its slot
    is free, judged after the notice, now."""
    state = _cp.EntryState()
    started = clock.now
    for _ in range(5):  # another checkpoint of the process booked the window's five writes
        ticket = state.admit(write=True, window_writes=5, now=started).ticket
        state.release(ticket, error=False, booked=True, now=started)
    notices: list[float] = []

    def slow_notice(message: str) -> None:
        notices.append(clock.now)
        clock.advance(150.0)

    writer = _writer(_WriterCtx(), state, clock=clock, notify=slow_notice)
    run = writer.new_run(started=started, deadline=started + 100.0, budget_s=100.0, planned_writes=1)
    assert run.deadline == started + 100.0 + WINDOW_S + 1.0
    try:
        writer.store_paced("fact a", run, category="learnings", title="fact a", content="a")
    finally:
        run.close()

    assert notices == [started]
    assert clock.now - started < 151.0, clock.now - started


def test_a_slow_notice_that_ends_past_the_deadline_stops_the_write_without_sleeping(clock):
    """A4, fail-closed: when the notice itself ends after the deadline the write stops at once with
    the deadline's error, instead of sleeping on past it."""
    state = _cp.EntryState()
    started = clock.now
    for _ in range(5):
        ticket = state.admit(write=True, window_writes=5, now=started).ticket
        state.release(ticket, error=False, booked=True, now=started)

    writer = _writer(_WriterCtx(), state, clock=clock, notify=lambda message: clock.advance(200.0))
    run = writer.new_run(started=started, deadline=started + 100.0, budget_s=100.0, planned_writes=1)
    slept = len(clock.slept)
    with pytest.raises(CtxCheckpointError, match="budget"):
        try:
            writer.store_paced("fact a", run, category="learnings", title="fact a", content="a")
        finally:
            run.close()
    assert len(clock.slept) == slept
    assert clock.now == started + 200.0


def test_a_run_past_its_deadline_holds_no_place_for_the_runs_after_it():
    """A8 (the second review of #299: no test pinned it): a registration whose run's deadline has
    passed holds nothing — neither planned writes in the window nor the next free place at the limit
    of calls in flight, although its call is still marked as waiting there (its thread has not woken
    to see its deadline yet). A run that starts after it is served at once."""
    state = _cp.EntryState()
    ghost, _ = state.register(400, lambda: 10.0, now=0.0, window_writes=5)  # never ended
    later, ahead = state.register(5, lambda: 500.0, now=20.0, window_writes=5)
    assert ahead == 0
    assert state.admit(write=True, window_writes=5, now=20.0, reservation=later).ticket is not None

    early, _ = state.register(5, lambda: 30.0, now=21.0, window_writes=5)
    late, _ = state.register(5, lambda: 500.0, now=21.0, window_writes=5)
    read = state.admit(write=False, window_writes=5, now=21.0, reservation=late).ticket
    assert read is not None  # with the later run's write: two calls in flight
    assert state.admit(write=False, window_writes=5, now=21.1, reservation=early).ticket is None  # waits
    assert early.waiting
    state.release(read, error=False, now=21.2)
    # 40 s: the waiting call's run is past its deadline; the later run does not give way to it.
    assert state.admit(write=False, window_writes=5, now=40.0, reservation=late).ticket is not None
    assert ghost.pending == 400


def test_a_read_before_a_write_that_fails_is_the_entry_s_third_error_answer_in_a_row(clock):
    """A10 (the second review of #299), reproduced: two chains are each refused once (other writers
    hold ctx's window), so the entry's last two answers are errors; the first chain that wants to
    write again sends one read alone, and the reads fail from then on. That read is the entry's
    third error answer in a row — where Hermes opens its breaker — while before 0.17.0 each chain
    stopped at its own second refusal and sent nothing more until the next checkpoint's first call.
    Kept (see the README): the read is the only call that can end the run the plugin sees, so a
    later checkpoint of the process would send it anyway; it fails closed, and no write follows it."""
    state = _cp.EntryState()
    answers: list[tuple[str, str]] = []

    class Recorded(_WriterCtx):
        def search(self, **kwargs):
            try:
                data = super().search(**kwargs)
            except _cp.CtxApiError:
                answers.append(("search", "error"))
                raise
            answers.append(("search", "ok"))
            return data

        def store(self, **kwargs):
            try:
                data = super().store(**kwargs)
            except _cp.CtxApiError:
                answers.append(("store", "error"))
                raise
            answers.append(("store", "ok"))
            return data

    ctx = Recorded(refuse=2, searches_fail_after=0)
    first, second = _writer(ctx, state, clock=clock), _writer(ctx, state, clock=clock)
    runs = [
        writer.new_run(started=clock.now, deadline=clock.now + 60, budget_s=60, planned_writes=2)
        for writer in (first, second)
    ]

    for writer, run, title in ((first, runs[0], "fact a"), (second, runs[1], "fact b")):
        with pytest.raises(CtxCheckpointError, match="rate-limited"):
            writer.store_paced(title, run, category="learnings", title=title, content=title)
    assert state.errors_in_row == _cp.MAX_RATE_REFUSALS
    with pytest.raises(CtxCheckpointError, match="read before fact a failed"):
        first.store_paced("fact a", runs[0], category="learnings", title="fact a", content="a")
    for run in runs:
        run.close()

    assert answers == [("store", "error"), ("store", "error"), ("search", "error")]
    assert len(answers) == _host_breaker_threshold()
    assert state.errors_in_row == _host_breaker_threshold() and state.in_flight == 0


def test_the_read_before_a_write_at_the_limit_is_a_listing_of_the_category():
    """A11 (#289, measured on a test ctx): the probe searched its category for "Compaction checkpoint
    head", which every head matches in full text — ctx read and ranked all of them (0.7–1.1 s median
    at 100 000 heads, 15–18 s at 1 000 000, past the 10 s bound of a call). Any answer ends the run,
    so the probe lists the category instead (no query text, one row): ctx answers that from an index
    of the category in 1.0–1.6 ms, the same at 1 000 000 heads as at none."""
    state = _cp.EntryState()
    _at_the_limit(state)
    searched: list[dict] = []

    class Ctx(_WriterCtx):
        def search(self, **kwargs):
            searched.append(kwargs)
            return super().search(**kwargs)

    writer = _cp.ChainWriter(client=Ctx(), category="compaction-checkpoints", entry=state)
    run = writer.new_run(started=time.monotonic(), deadline=time.monotonic() + 30, budget_s=30, planned_writes=1)
    try:
        writer.store_paced("fact a", run, category="learnings", title="fact a", content="a")
    finally:
        run.close()

    assert [{k: v for k, v in s.items() if k != "timeout"} for s in searched] == [
        {"text": "", "category": "compaction-checkpoints", "limit": 1}
    ]


def _held_behind_an_earlier_run(clock, notice_s: float):
    """A run registered first plans five writes (one window at max_parts 3) and ends at 70 s without
    sending them; a write of a run started after it is held behind them, and its notice takes
    ``notice_s`` seconds in the user's callback."""
    state = _cp.EntryState()
    started = clock.now
    earlier, _ = state.register(5, lambda: started + 70.0, now=started, window_writes=5)
    writer = _writer(_WriterCtx(), state, clock=clock, notify=lambda message: clock.advance(notice_s))
    run = writer.new_run(started=started, deadline=started + 100.0, budget_s=100.0, planned_writes=1)
    return state, earlier, writer, run, started


def test_a_slow_notice_of_a_held_write_does_not_lengthen_its_next_look(clock):
    """A4, held behind an earlier run: the notice took 59 s of the 61 s until the write's turn; the
    write slept the five seconds it computed before the notice, now the two that are left."""
    state, earlier, writer, run, started = _held_behind_an_earlier_run(clock, 59.0)
    slept = len(clock.slept)
    try:
        writer.store_paced("fact a", run, category="learnings", title="fact a", content="a")
    finally:
        run.close()
        state.unregister(earlier)
    assert clock.slept[slept][1] == pytest.approx(started + 61.0 - (started + 59.0))


def test_a_slow_notice_of_a_held_write_that_ends_past_the_deadline_stops_it_at_once(clock):
    """A4, fail-closed: the notice of a held write ended after the write's deadline; the write stops
    there with the deadline's error instead of sleeping on and looking again."""
    state, earlier, writer, run, started = _held_behind_an_earlier_run(clock, 200.0)
    slept = len(clock.slept)
    with pytest.raises(CtxCheckpointError, match="budget"):
        try:
            writer.store_paced("fact a", run, category="learnings", title="fact a", content="a")
        finally:
            run.close()
            state.unregister(earlier)
    assert len(clock.slept) == slept and clock.now == started + 200.0
