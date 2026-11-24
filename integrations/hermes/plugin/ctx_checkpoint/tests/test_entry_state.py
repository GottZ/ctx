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
import json
import sys
import threading
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
    assert state.admit(write=True, window_writes=5, now=0.5).stop
    # A read sent now was sent after both: its answer ends them.
    fresh = state.admit(write=False, window_writes=5, now=0.6).ticket
    state.release(fresh, error=False, now=0.7)
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
